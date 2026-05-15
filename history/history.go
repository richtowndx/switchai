package history

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"switchai/appdata"
	"switchai/logger"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	_ "modernc.org/sqlite"
)

type RequestRecord struct {
	ID              string      `json:"id"`
	Timestamp       time.Time   `json:"timestamp"`
	Method          string      `json:"method"`
	Path            string      `json:"path"`
	ClientIP        string      `json:"client_ip"`
	KeyID           string      `json:"key_id"`
	Provider        string      `json:"provider"`
	Model           string      `json:"model"`
	StatusCode      int         `json:"status_code"`
	Duration        int64       `json:"duration_ms"`
	RequestBody     string      `json:"request_body"`
	ResponseBody    string      `json:"response_body"`
	RequestHeaders  interface{} `json:"request_headers"`
	ResponseHeaders interface{} `json:"response_headers"`
	RequestSize     int64       `json:"request_size"`
	ResponseSize    int64       `json:"response_size"`
	InputTokens     int         `json:"input_tokens"`
	OutputTokens    int         `json:"output_tokens"`
	TotalTokens     int         `json:"total_tokens"`
	Cost            float64     `json:"cost"`
}

var (
	db          *sql.DB
	history     *History
	broadcast   chan RequestRecord
	clients     map[*websocket.Conn]bool
	clientsMu   sync.RWMutex

	// 首页缓存
	homeCache      []RequestRecord
	homeCacheMu    sync.RWMutex
	homeCacheSize = 20 // 首页缓存大小
	homeCacheTotal int // 缓存时的总记录数
)

type History struct {
	quitChan chan struct{}
}

func Init() error {
	// Ensure data directory exists
	dataDir := appdata.GetConfigPath("")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return err
	}

	dbPath := filepath.Join(dataDir, "history.db")
	var err error
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}

	// Create table and index
	if err := initDB(); err != nil {
		db.Close()
		return err
	}

	history = &History{
		quitChan: make(chan struct{}),
	}
	broadcast = make(chan RequestRecord, 1000)
	clients = make(map[*websocket.Conn]bool)

	// 加载首页缓存
	loadHomeCache()

	// Start broadcast goroutine
	go history.handleBroadcast()

	return nil
}

// loadHomeCache 加载首页缓存
func loadHomeCache() {
	homeCacheMu.Lock()
	defer homeCacheMu.Unlock()

	rows, err := db.Query(`
		SELECT id, timestamp, method, path, client_ip, key_id, provider, model,
			status_code, duration_ms, request_body, response_body, request_headers, response_headers,
			request_size, response_size, input_tokens, output_tokens, total_tokens, cost
		FROM history ORDER BY timestamp DESC LIMIT ?`, homeCacheSize)
	if err != nil {
		logger.Error("Failed to load home cache: %v", err)
		return
	}
	defer rows.Close()

	homeCache = nil
	for rows.Next() {
		var r RequestRecord
		var timestamp int64
		var reqHeaders, respHeaders sql.NullString
		var method, path, clientIP, keyID, provider, model sql.NullString
		var statusCode, durationMs, inputTokens, outputTokens, totalTokens sql.NullInt64
		var requestBody, responseBody sql.NullString
		var requestSize, responseSize sql.NullInt64
		var cost sql.NullFloat64

		err := rows.Scan(&r.ID, &timestamp, &method, &path, &clientIP, &keyID, &provider, &model,
			&statusCode, &durationMs, &requestBody, &responseBody, &reqHeaders, &respHeaders,
			&requestSize, &responseSize, &inputTokens, &outputTokens, &totalTokens, &cost)
		if err != nil {
			logger.Error("Failed to scan cache record: %v", err)
			continue
		}

		r.Timestamp = time.Unix(0, timestamp)
		r.Method = method.String
		r.Path = path.String
		r.ClientIP = clientIP.String
		r.KeyID = keyID.String
		r.Provider = provider.String
		r.Model = model.String
		r.StatusCode = int(statusCode.Int64)
		r.Duration = durationMs.Int64
		r.RequestBody = requestBody.String
		r.ResponseBody = responseBody.String
		r.RequestSize = requestSize.Int64
		r.ResponseSize = responseSize.Int64
		r.InputTokens = int(inputTokens.Int64)
		r.OutputTokens = int(outputTokens.Int64)
		r.TotalTokens = int(totalTokens.Int64)
		r.Cost = cost.Float64

		if reqHeaders.Valid {
			json.Unmarshal([]byte(reqHeaders.String), &r.RequestHeaders)
		}
		if respHeaders.Valid {
			json.Unmarshal([]byte(respHeaders.String), &r.ResponseHeaders)
		}

		homeCache = append(homeCache, r)
	}

	// 获取缓存时的总记录数
	db.QueryRow("SELECT COUNT(*) FROM history").Scan(&homeCacheTotal)
	logger.Info("Home cache loaded: %d records, total: %d", len(homeCache), homeCacheTotal)
}

func initDB() error {
	schema := `
	CREATE TABLE IF NOT EXISTS history (
		id TEXT PRIMARY KEY,
		timestamp INTEGER NOT NULL,
		method TEXT,
		path TEXT,
		client_ip TEXT,
		key_id TEXT,
		provider TEXT,
		model TEXT,
		status_code INTEGER,
		duration_ms INTEGER,
		request_body TEXT,
		response_body TEXT,
		request_headers TEXT,
		response_headers TEXT,
		request_size INTEGER,
		response_size INTEGER,
		input_tokens INTEGER,
		output_tokens INTEGER,
		total_tokens INTEGER,
		cost REAL
	);
	CREATE INDEX IF NOT EXISTS idx_history_timestamp ON history(timestamp DESC);
	`
	_, err := db.Exec(schema)
	return err
}

// cleanupOldRecords 删除超过1000条的旧数据
func cleanupOldRecords() {
	const maxRecords = 1000

	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM history").Scan(&count)
	if err != nil {
		logger.Error("Failed to count history records: %v", err)
		return
	}

	if count > maxRecords {
		// 计算需要删除的数量
		deleteCount := count - maxRecords
		// 删除最老的记录（按时间戳升序，删除最早的）
		_, err := db.Exec(`
			DELETE FROM history WHERE id IN (
				SELECT id FROM history ORDER BY timestamp ASC LIMIT ?
			)`, deleteCount)
		if err != nil {
			logger.Error("Failed to cleanup old history records: %v", err)
		} else {
			logger.Info("Cleaned up %d old history records", deleteCount)
		}
	}

	// 删除30天前的记录
	thirtyDaysAgo := time.Now().AddDate(0, 0, -30).UnixNano()
	result, err := db.Exec(`DELETE FROM history WHERE timestamp < ?`, thirtyDaysAgo)
	if err != nil {
		logger.Error("Failed to delete old history records (>30 days): %v", err)
	} else if rowsAffected, _ := result.RowsAffected(); rowsAffected > 0 {
		logger.Info("Deleted %d history records older than 30 days", rowsAffected)
	}
}

func Shutdown() {
	if history == nil {
		return
	}
	close(history.quitChan)
	if db != nil {
		db.Close()
	}
}

func AddClient(conn *websocket.Conn) {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	clients[conn] = true
}

func RemoveClient(conn *websocket.Conn) {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	conn.Close()
	delete(clients, conn)
}

func (h *History) handleBroadcast() {
	for record := range broadcast {
		clientsMu.RLock()
		total := len(clients)
		msg := gin.H{
			"id":           record.ID,
			"total":        total,
			"timestamp":    record.Timestamp,
			"method":       record.Method,
			"path":         record.Path,
			"client_ip":    record.ClientIP,
			"key_id":       record.KeyID,
			"provider":     record.Provider,
			"model":        record.Model,
			"status_code":  record.StatusCode,
			"duration_ms":  record.Duration,
			"input_tokens": record.InputTokens,
			"output_tokens": record.OutputTokens,
			"total_tokens": record.TotalTokens,
			"cost":         record.Cost,
		}
		for client := range clients {
			err := client.WriteJSON(msg)
			if err != nil {
				logger.Error("WebSocket write error: %v", err)
				client.Close()
				delete(clients, client)
			}
		}
		clientsMu.RUnlock()
	}
}

// BroadcastRecord 从外部包广播记录到历史 WebSocket 客户端
func BroadcastRecord(providerID, providerName, model string, inputTokens, outputTokens int, cost float64, duration int64, timestamp time.Time, keyID, clientIP string) {
	record := RequestRecord{
		ID:           "",
		Timestamp:    timestamp,
		Method:       "",
		Path:         "",
		ClientIP:     clientIP,
		KeyID:        keyID,
		Provider:     providerName,
		Model:        model,
		StatusCode:   0,
		Duration:     duration,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  inputTokens + outputTokens,
		Cost:         cost,
	}
	broadcast <- record
}

func AddRecord(record RequestRecord) {
	// Insert into SQLite
	var reqHeaders, respHeaders string
	if record.RequestHeaders != nil {
		b, _ := json.Marshal(record.RequestHeaders)
		reqHeaders = string(b)
	}
	if record.ResponseHeaders != nil {
		b, _ := json.Marshal(record.ResponseHeaders)
		respHeaders = string(b)
	}

	_, err := db.Exec(`
		INSERT INTO history (id, timestamp, method, path, client_ip, key_id, provider, model,
			status_code, duration_ms, request_body, response_body, request_headers, response_headers,
			request_size, response_size, input_tokens, output_tokens, total_tokens, cost)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID, record.Timestamp.UnixNano(), record.Method, record.Path, record.ClientIP,
		record.KeyID, record.Provider, record.Model, record.StatusCode, record.Duration,
		record.RequestBody, record.ResponseBody, reqHeaders, respHeaders,
		record.RequestSize, record.ResponseSize, record.InputTokens, record.OutputTokens,
		record.TotalTokens, record.Cost)
	if err != nil {
		logger.Error("Failed to insert history record: %v", err)
	}

	// Cleanup old records if exceeds 1000
	cleanupOldRecords()

	// 更新首页缓存：头部插入新记录
	updateHomeCache(record)

	// Broadcast to WebSocket clients
	go func(r RequestRecord) {
		broadcast <- r
	}(record)

	logger.Info("[%s] %s %s | %s | %s | %d | %dms | in:%d out:%d",
		record.Method, record.Path, record.ClientIP, record.Provider, record.Model,
		record.StatusCode, record.Duration, record.InputTokens, record.OutputTokens)
}

// updateHomeCache 更新首页缓存，头部插入新记录
func updateHomeCache(record RequestRecord) {
	homeCacheMu.Lock()
	defer homeCacheMu.Unlock()

	// 头部插入新记录
	newCache := make([]RequestRecord, 0, homeCacheSize)
	newCache = append(newCache, record)
	// 追加原有缓存（保留前 homeCacheSize-1 条）
	for i := 0; i < len(homeCache) && len(newCache) < homeCacheSize; i++ {
		newCache = append(newCache, homeCache[i])
	}
	homeCache = newCache
	homeCacheTotal++
}

// getHomeCache 返回缓存的副本
func getHomeCache() ([]RequestRecord, int) {
	homeCacheMu.RLock()
	defer homeCacheMu.RUnlock()

	if homeCache == nil {
		return []RequestRecord{}, 0
	}
	// 返回副本避免并发问题
	cacheCopy := make([]RequestRecord, len(homeCache))
	copy(cacheCopy, homeCache)
	return cacheCopy, homeCacheTotal
}

// RecordSummary 列表展示用的精简记录
type RecordSummary struct {
	ID          string    `json:"id"`
	Timestamp   time.Time `json:"timestamp"`
	Method      string    `json:"method"`
	Path        string    `json:"path"`
	ClientIP    string    `json:"client_ip"`
	KeyID       string    `json:"key_id"`
	Provider    string    `json:"provider"`
	Model       string    `json:"model"`
	StatusCode  int       `json:"status_code"`
	Duration    int64     `json:"duration_ms"`
	RequestSize int64     `json:"request_size"`
	ResponseSize int64    `json:"response_size"`
	InputTokens int       `json:"input_tokens"`
	OutputTokens int      `json:"output_tokens"`
	TotalTokens int       `json:"total_tokens"`
	Cost        float64   `json:"cost"`
}

func GetRecords(page, pageSize int) ([]RequestRecord, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	// Get total count
	var total int
	err := db.QueryRow("SELECT COUNT(*) FROM history").Scan(&total)
	if err != nil {
		logger.Error("Failed to count records: %v", err)
		return []RequestRecord{}, 0
	}

	offset := (page - 1) * pageSize
	rows, err := db.Query(`
		SELECT id, timestamp, method, path, client_ip, key_id, provider, model,
			status_code, duration_ms, request_body, response_body, request_headers, response_headers,
			request_size, response_size, input_tokens, output_tokens, total_tokens, cost
		FROM history ORDER BY timestamp DESC LIMIT ? OFFSET ?`, pageSize, offset)
	if err != nil {
		logger.Error("Failed to query records: %v", err)
		return []RequestRecord{}, 0
	}
	defer rows.Close()

	var records []RequestRecord
	for rows.Next() {
		var r RequestRecord
		var timestamp int64
		var reqHeaders, respHeaders sql.NullString
		var method, path, clientIP, keyID, provider, model sql.NullString
		var statusCode, durationMs, inputTokens, outputTokens, totalTokens sql.NullInt64
		var requestBody, responseBody sql.NullString
		var requestSize, responseSize sql.NullInt64
		var cost sql.NullFloat64

		err := rows.Scan(&r.ID, &timestamp, &method, &path, &clientIP, &keyID, &provider, &model,
			&statusCode, &durationMs, &requestBody, &responseBody, &reqHeaders, &respHeaders,
			&requestSize, &responseSize, &inputTokens, &outputTokens, &totalTokens, &cost)
		if err != nil {
			logger.Error("Failed to scan record: %v", err)
			continue
		}

		r.Timestamp = time.Unix(0, timestamp)
		r.Method = method.String
		r.Path = path.String
		r.ClientIP = clientIP.String
		r.KeyID = keyID.String
		r.Provider = provider.String
		r.Model = model.String
		r.StatusCode = int(statusCode.Int64)
		r.Duration = durationMs.Int64
		r.RequestBody = requestBody.String
		r.ResponseBody = responseBody.String
		r.RequestSize = requestSize.Int64
		r.ResponseSize = responseSize.Int64
		r.InputTokens = int(inputTokens.Int64)
		r.OutputTokens = int(outputTokens.Int64)
		r.TotalTokens = int(totalTokens.Int64)
		r.Cost = cost.Float64

		if reqHeaders.Valid {
			json.Unmarshal([]byte(reqHeaders.String), &r.RequestHeaders)
		}
		if respHeaders.Valid {
			json.Unmarshal([]byte(respHeaders.String), &r.ResponseHeaders)
		}

		records = append(records, r)
	}

	return records, total
}

// GetRecordsSummary 获取精简的记录列表（不含 request_body、response_body 等大字段）
func GetRecordsSummary(page, pageSize int) ([]RecordSummary, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	// 第1页且请求大小不超过缓存大小时，使用缓存
	// 移除缓存逻辑以确保分页一致性：始终使用数据库查询
	// 避免因 homeCacheTotal 和实际数据库记录数不同步导致的分页错误
	/*
	if page == 1 && pageSize <= homeCacheSize {
		homeCacheMu.RLock()
		defer homeCacheMu.RUnlock()

		summaries := make([]RecordSummary, 0, len(homeCache))
		for _, r := range homeCache {
			summaries = append(summaries, RecordSummary{
				ID:           r.ID,
				Timestamp:    r.Timestamp,
				Method:       r.Method,
				Path:         r.Path,
				ClientIP:     r.ClientIP,
				KeyID:        r.KeyID,
				Provider:     r.Provider,
				Model:        r.Model,
				StatusCode:   r.StatusCode,
				Duration:     r.Duration,
				RequestSize:  r.RequestSize,
				ResponseSize: r.ResponseSize,
				InputTokens:  r.InputTokens,
				OutputTokens: r.OutputTokens,
				TotalTokens:  r.TotalTokens,
				Cost:         r.Cost,
			})
		}
		return summaries, homeCacheTotal
	}
	*/

	// Get total count
	var total int
	err := db.QueryRow("SELECT COUNT(*) FROM history").Scan(&total)
	if err != nil {
		logger.Error("Failed to count records: %v", err)
		return []RecordSummary{}, 0
	}

	offset := (page - 1) * pageSize
	rows, err := db.Query(`
		SELECT id, timestamp, method, path, client_ip, key_id, provider, model,
			status_code, duration_ms, request_size, response_size,
			input_tokens, output_tokens, total_tokens, cost
		FROM history ORDER BY timestamp DESC LIMIT ? OFFSET ?`, pageSize, offset)
	if err != nil {
		logger.Error("Failed to query records: %v", err)
		return []RecordSummary{}, 0
	}
	defer rows.Close()

	var records []RecordSummary
	for rows.Next() {
		var r RecordSummary
		var timestamp int64
		var method, path, clientIP, keyID, provider, model sql.NullString
		var statusCode, durationMs, inputTokens, outputTokens, totalTokens sql.NullInt64
		var requestSize, responseSize sql.NullInt64
		var cost sql.NullFloat64

		err := rows.Scan(&r.ID, &timestamp, &method, &path, &clientIP, &keyID, &provider, &model,
			&statusCode, &durationMs, &requestSize, &responseSize,
			&inputTokens, &outputTokens, &totalTokens, &cost)
		if err != nil {
			logger.Error("Failed to scan record: %v", err)
			continue
		}

		r.Timestamp = time.Unix(0, timestamp)
		r.Method = method.String
		r.Path = path.String
		r.ClientIP = clientIP.String
		r.KeyID = keyID.String
		r.Provider = provider.String
		r.Model = model.String
		r.StatusCode = int(statusCode.Int64)
		r.Duration = durationMs.Int64
		r.RequestSize = requestSize.Int64
		r.ResponseSize = responseSize.Int64
		r.InputTokens = int(inputTokens.Int64)
		r.OutputTokens = int(outputTokens.Int64)
		r.TotalTokens = int(totalTokens.Int64)
		r.Cost = cost.Float64

		records = append(records, r)
	}

	return records, total
}

func GetRecord(id string) *RequestRecord {
	var r RequestRecord
	var timestamp int64
	var reqHeaders, respHeaders sql.NullString
	var method, path, clientIP, keyID, provider, model sql.NullString
	var statusCode, durationMs, inputTokens, outputTokens, totalTokens sql.NullInt64
	var requestBody, responseBody sql.NullString
	var requestSize, responseSize sql.NullInt64
	var cost sql.NullFloat64

	err := db.QueryRow(`
		SELECT id, timestamp, method, path, client_ip, key_id, provider, model,
			status_code, duration_ms, request_body, response_body, request_headers, response_headers,
			request_size, response_size, input_tokens, output_tokens, total_tokens, cost
		FROM history WHERE id = ?`, id).Scan(&r.ID, &timestamp, &method, &path, &clientIP, &keyID, &provider, &model,
		&statusCode, &durationMs, &requestBody, &responseBody, &reqHeaders, &respHeaders,
		&requestSize, &responseSize, &inputTokens, &outputTokens, &totalTokens, &cost)
	if err != nil {
		return nil
	}

	r.Timestamp = time.Unix(0, timestamp)
	r.Method = method.String
	r.Path = path.String
	r.ClientIP = clientIP.String
	r.KeyID = keyID.String
	r.Provider = provider.String
	r.Model = model.String
	r.StatusCode = int(statusCode.Int64)
	r.Duration = durationMs.Int64
	r.RequestBody = requestBody.String
	r.ResponseBody = responseBody.String
	r.RequestSize = requestSize.Int64
	r.ResponseSize = responseSize.Int64
	r.InputTokens = int(inputTokens.Int64)
	r.OutputTokens = int(outputTokens.Int64)
	r.TotalTokens = int(totalTokens.Int64)
	r.Cost = cost.Float64

	if reqHeaders.Valid {
		json.Unmarshal([]byte(reqHeaders.String), &r.RequestHeaders)
	}
	if respHeaders.Valid {
		json.Unmarshal([]byte(respHeaders.String), &r.ResponseHeaders)
	}

	return &r
}
