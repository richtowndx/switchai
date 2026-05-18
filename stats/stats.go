package stats

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"switchai/appdata"
	"switchai/history"
	"switchai/logger"

	"github.com/gorilla/websocket"
	_ "modernc.org/sqlite"
)

type UsageRecord struct {
	ProviderID           string    `json:"provider_id"`
	ProviderName         string    `json:"provider_name"`
	Model                string    `json:"model"`
	InputTokens          int       `json:"input_tokens"`
	OutputTokens         int       `json:"output_tokens"`
	CacheReadInputTokens int       `json:"cache_read_input_tokens"`
	TotalTokens          int       `json:"total_tokens"`
	Cost                 float64   `json:"cost"`
	Duration             int64     `json:"duration_ms"`
	TimeToFirst          int64     `json:"time_to_first_ms"`
	Timestamp            time.Time `json:"timestamp"`
	Group                string    `json:"group"`
	Type                 string    `json:"type"`
}

type ProviderStats struct {
	ProviderID           string  `json:"provider_id"`
	ProviderName         string  `json:"provider_name"`
	InputTokens          int     `json:"input_tokens"`
	OutputTokens         int     `json:"output_tokens"`
	CacheReadInputTokens int     `json:"cache_read_input_tokens"`
	TotalTokens          int     `json:"total_tokens"`
	TotalCost            float64 `json:"total_cost"`
	RequestCount         int     `json:"request_count"`
}

type KeyStats struct {
	KeyID                string   `json:"key_id"`
	InputTokens          int      `json:"input_tokens"`
	OutputTokens         int      `json:"output_tokens"`
	CacheReadInputTokens int      `json:"cache_read_input_tokens"`
	TotalTokens          int      `json:"total_tokens"`
	TotalCost            float64  `json:"total_cost"`
	IPAddresses          []string `json:"ip_addresses"`
	RequestCount         int      `json:"request_count"`
	TodayReqCount        int      `json:"today_req_count"`
	TodayCost            float64  `json:"today_cost"`
}

var (
	db    *sql.DB
	stats *Stats
)

type Stats struct {
	mu        sync.RWMutex
	clients   map[*websocket.Conn]bool
	broadcast chan UsageRecord
}

func Init() {
	dataDir := appdata.GetDataDir()
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		logger.Error("Failed to create data dir: %v", err)
		return
	}

	dbPath := filepath.Join(dataDir, "stats.db")
	var err error
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		logger.Error("Failed to open stats db: %v", err)
		return
	}

	if err := initDB(); err != nil {
		logger.Error("Failed to init stats db: %v", err)
		db.Close()
		return
	}

	stats = &Stats{
		clients:   make(map[*websocket.Conn]bool),
		broadcast: make(chan UsageRecord, 100),
	}

	go stats.handleBroadcast()

}

func initDB() error {
	schema := `
	CREATE TABLE IF NOT EXISTS usage_records (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		provider_id TEXT NOT NULL,
		provider_name TEXT,
		model TEXT,
		input_tokens INTEGER,
		output_tokens INTEGER,
		cache_read_input_tokens INTEGER DEFAULT 0,
		total_tokens INTEGER,
		cost REAL,
		duration_ms INTEGER,
		time_to_first_ms INTEGER,
		timestamp INTEGER NOT NULL,
		group_name TEXT,
		type_name TEXT,
		key_id TEXT,
		client_ip TEXT
	);

	CREATE TABLE IF NOT EXISTS provider_stats (
		provider_id TEXT PRIMARY KEY,
		provider_name TEXT,
		input_tokens INTEGER DEFAULT 0,
		output_tokens INTEGER DEFAULT 0,
		cache_read_input_tokens INTEGER DEFAULT 0,
		total_tokens INTEGER DEFAULT 0,
		total_cost REAL DEFAULT 0,
		request_count INTEGER DEFAULT 0
	);

	CREATE TABLE IF NOT EXISTS key_stats (
		key_id TEXT PRIMARY KEY,
		input_tokens INTEGER DEFAULT 0,
		output_tokens INTEGER DEFAULT 0,
		cache_read_input_tokens INTEGER DEFAULT 0,
		total_tokens INTEGER DEFAULT 0,
		total_cost REAL DEFAULT 0,
		ip_addresses TEXT DEFAULT '[]',
		request_count INTEGER DEFAULT 0
	);

	CREATE TABLE IF NOT EXISTS key_daily_stats (
		key_id TEXT NOT NULL,
		date TEXT NOT NULL,
		request_count INTEGER DEFAULT 0,
		total_cost REAL DEFAULT 0,
		PRIMARY KEY (key_id, date)
	);

	CREATE INDEX IF NOT EXISTS idx_key_daily ON key_daily_stats(key_id, date);

	CREATE INDEX IF NOT EXISTS idx_usage_timestamp ON usage_records(timestamp DESC);
	CREATE INDEX IF NOT EXISTS idx_usage_provider ON usage_records(provider_id);
	CREATE INDEX IF NOT EXISTS idx_usage_key ON usage_records(key_id);
	`
	_, err := db.Exec(schema)
	if err != nil {
		return err
	}

	// Migration: add cache_read_input_tokens column if not exists
	migrations := []string{
		`ALTER TABLE usage_records ADD COLUMN cache_read_input_tokens INTEGER DEFAULT 0`,
		`ALTER TABLE provider_stats ADD COLUMN cache_read_input_tokens INTEGER DEFAULT 0`,
		`ALTER TABLE key_stats ADD COLUMN cache_read_input_tokens INTEGER DEFAULT 0`,
	}
	for _, m := range migrations {
		_, err := db.Exec(m)
		if err != nil && !strings.Contains(err.Error(), "duplicate column") {
			logger.Warn("Stats migration warning: %v", err)
		}
	}
	return nil
}

func Shutdown() {
	if db != nil {
		db.Close()
	}
}

func (s *Stats) handleBroadcast() {
	for record := range s.broadcast {
		// 获取完整统计摘要广播给所有 stats WebSocket 客户端
		summary := s.GetSummary()
		s.mu.RLock()
		for client := range s.clients {
			err := client.WriteJSON(summary)
			if err != nil {
				logger.Error("WebSocket write error: %v", err)
				client.Close()
				delete(s.clients, client)
			}
		}
		s.mu.RUnlock()

		// 同时广播单条记录给 history WebSocket 客户端
		history.BroadcastRecord(record.ProviderID, record.ProviderName, record.Model,
			record.InputTokens, record.OutputTokens, record.CacheReadInputTokens, record.Cost, record.Duration,
			record.Timestamp, "", "")
	}
}

func (s *Stats) AddClient(conn *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[conn] = true
}

func (s *Stats) RemoveClient(conn *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clients, conn)
	conn.Close()
}

func maskKeyID(keyID string) string {
	if len(keyID) <= 8 {
		if len(keyID) == 0 {
			return "(empty)"
		}
		return keyID[:len(keyID)/2] + "..."
	}
	return keyID[:8] + "..."
}

func RecordUsage(providerID, providerName, model, group, reqType string, inputTokens, outputTokens, cacheReadTokens int, cost float64, duration, timeToFirst int64, keyID, clientIP string) {
	logger.Info("RecordUsage START: provider=%s, model=%s, input=%d, output=%d, cache=%d, keyID=%s", providerID, model, inputTokens, outputTokens, cacheReadTokens, keyID)

	if db == nil {
		logger.Error("RecordUsage: db is nil! Stats not initialized.")
		return
	}

	tx, err := db.Begin()
	if err != nil {
		logger.Error("Failed to begin transaction: %v", err)
		return
	}
	defer tx.Rollback()

	totalTokens := inputTokens + outputTokens + cacheReadTokens

	// Insert usage record
	_, err = tx.Exec(`
		INSERT INTO usage_records (provider_id, provider_name, model, input_tokens, output_tokens, cache_read_input_tokens, total_tokens,
			cost, duration_ms, time_to_first_ms, timestamp, group_name, type_name, key_id, client_ip)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		providerID, providerName, model, inputTokens, outputTokens, cacheReadTokens, totalTokens,
		cost, duration, timeToFirst, time.Now().UnixNano(), group, reqType, keyID, clientIP)
	if err != nil {
		logger.Error("Failed to insert usage record: %v", err)
		return
	}

	// Delete records older than 7 days
	sevenDaysAgo := time.Now().AddDate(0, 0, -7).UnixNano()
	_, err = tx.Exec(`DELETE FROM usage_records WHERE timestamp < ?`, sevenDaysAgo)
	if err != nil {
		logger.Error("Failed to delete old usage records: %v", err)
		return
	}

	// Upsert provider_stats
	_, err = tx.Exec(`
		INSERT INTO provider_stats (provider_id, provider_name, input_tokens, output_tokens, cache_read_input_tokens, total_tokens, total_cost, request_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, 1)
		ON CONFLICT(provider_id) DO UPDATE SET
			provider_name = excluded.provider_name,
			input_tokens = input_tokens + excluded.input_tokens,
			output_tokens = output_tokens + excluded.output_tokens,
			cache_read_input_tokens = cache_read_input_tokens + excluded.cache_read_input_tokens,
			total_tokens = total_tokens + excluded.total_tokens,
			total_cost = total_cost + excluded.total_cost,
			request_count = request_count + 1`,
		providerID, providerName, inputTokens, outputTokens, cacheReadTokens, totalTokens, cost)
	if err != nil {
		logger.Error("Failed to upsert provider stats: %v", err)
		return
	}

	// Upsert key_stats
	if keyID != "" {
		// Get existing ip_addresses
		var existingIPs string
		err = tx.QueryRow(`SELECT ip_addresses FROM key_stats WHERE key_id = ?`, keyID).Scan(&existingIPs)
		if err != nil && err != sql.ErrNoRows {
			// 如果是真正的数据库错误，记录日志但继续执行（使用空列表）
			logger.Error("Failed to get existing key stats: %v", err)
		}

		var ips []string
		if err == nil && existingIPs != "" {
			json.Unmarshal([]byte(existingIPs), &ips)
		}

		// Add new IP if not exists
		if clientIP != "" {
			found := false
			for _, ip := range ips {
				if ip == clientIP {
					found = true
					break
				}
			}
			if !found {
				ips = append(ips, clientIP)
			}
		}

		ipsJSON, _ := json.Marshal(ips)
		_, err = tx.Exec(`
			INSERT INTO key_stats (key_id, input_tokens, output_tokens, cache_read_input_tokens, total_tokens, total_cost, ip_addresses, request_count)
			VALUES (?, ?, ?, ?, ?, ?, ?, 1)
			ON CONFLICT(key_id) DO UPDATE SET
				input_tokens = input_tokens + excluded.input_tokens,
				output_tokens = output_tokens + excluded.output_tokens,
				cache_read_input_tokens = cache_read_input_tokens + excluded.cache_read_input_tokens,
				total_tokens = total_tokens + excluded.total_tokens,
				total_cost = total_cost + excluded.total_cost,
				ip_addresses = excluded.ip_addresses,
				request_count = request_count + 1`,
			keyID, inputTokens, outputTokens, cacheReadTokens, totalTokens, cost, string(ipsJSON))
		if err != nil {
			logger.Error("Failed to upsert key stats: %v", err)
			return
		}

		// Upsert key_daily_stats
		today := time.Now().Format("2006-01-02")
		_, err = tx.Exec(`
			INSERT INTO key_daily_stats (key_id, date, request_count, total_cost)
			VALUES (?, ?, 1, ?)
			ON CONFLICT(key_id, date) DO UPDATE SET
				request_count = request_count + 1,
				total_cost = total_cost + excluded.total_cost`,
			keyID, today, cost)
		if err != nil {
			logger.Error("Failed to upsert key daily stats: %v", err)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		logger.Error("Failed to commit transaction: %v", err)
		return
	}
	logger.Info("RecordUsage: Transaction committed, DB write successful")

	// Log
	record := UsageRecord{
		ProviderID:           providerID,
		ProviderName:         providerName,
		Model:                model,
		InputTokens:          inputTokens,
		OutputTokens:         outputTokens,
		CacheReadInputTokens: cacheReadTokens,
		TotalTokens:          totalTokens,
		Cost:                 cost,
		Duration:             duration,
		TimeToFirst:          timeToFirst,
		Timestamp:            time.Now(),
		Group:                group,
		Type:                 reqType,
	}
	// Broadcast - block if channel is full to ensure delivery
	stats.broadcast <- record
}

func GetStats() *Stats {
	return stats
}

func (s *Stats) GetSummary() map[string]interface{} {
	if db == nil {
		return emptySummary()
	}

	// Get provider stats
	providerRows, err := db.Query(`SELECT provider_id, provider_name, input_tokens, output_tokens, cache_read_input_tokens, total_tokens, total_cost, request_count FROM provider_stats`)
	if err != nil {
		logger.Error("Failed to get provider stats: %v", err)
		return emptySummary()
	}
	defer providerRows.Close()

	var providerStatsArray []*ProviderStats
	for providerRows.Next() {
		var ps ProviderStats
		if err := providerRows.Scan(&ps.ProviderID, &ps.ProviderName, &ps.InputTokens, &ps.OutputTokens, &ps.CacheReadInputTokens, &ps.TotalTokens, &ps.TotalCost, &ps.RequestCount); err != nil {
			continue
		}
		providerStatsArray = append(providerStatsArray, &ps)
	}
	sort.Slice(providerStatsArray, func(i, j int) bool {
		return providerStatsArray[i].ProviderName < providerStatsArray[j].ProviderName
	})

	// Get key stats
	today := time.Now().Format("2006-01-02")
	keyRows, err := db.Query(`SELECT ks.key_id, ks.input_tokens, ks.output_tokens, ks.cache_read_input_tokens, ks.total_tokens, ks.total_cost, ks.ip_addresses, ks.request_count, COALESCE(kds.request_count, 0) as today_req_count, COALESCE(kds.total_cost, 0.0) as today_cost FROM key_stats ks LEFT JOIN key_daily_stats kds ON ks.key_id = kds.key_id AND kds.date = ?`, today)
	if err != nil {
		logger.Error("Failed to get key stats: %v", err)
		return emptySummary()
	}
	defer keyRows.Close()

	var keyStatsArray []*KeyStats
	for keyRows.Next() {
		var ks KeyStats
		var ipsJSON string
		if err := keyRows.Scan(&ks.KeyID, &ks.InputTokens, &ks.OutputTokens, &ks.CacheReadInputTokens, &ks.TotalTokens, &ks.TotalCost, &ipsJSON, &ks.RequestCount, &ks.TodayReqCount, &ks.TodayCost); err != nil {
			continue
		}
		json.Unmarshal([]byte(ipsJSON), &ks.IPAddresses)
		keyStatsArray = append(keyStatsArray, &ks)
	}
	sort.Slice(keyStatsArray, func(i, j int) bool {
		return keyStatsArray[i].KeyID < keyStatsArray[j].KeyID
	})

	// Get totals directly from usage_records to ensure consistency with individual key stats
	var totalInput, totalOutput, totalCacheRead, totalRequestCount int
	var totalCost float64
	err = db.QueryRow(`SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(cache_read_input_tokens), 0), COUNT(*), COALESCE(SUM(cost), 0.0) FROM usage_records`).Scan(&totalInput, &totalOutput, &totalCacheRead, &totalRequestCount, &totalCost)
	if err != nil {
		logger.Error("Failed to get totals from usage_records: %v", err)
	}

	// Get recent records (last 10)
	recordRows, err := db.Query(`SELECT provider_id, provider_name, model, input_tokens, output_tokens, cache_read_input_tokens, total_tokens, cost, duration_ms, time_to_first_ms, timestamp, group_name, type_name FROM usage_records ORDER BY timestamp DESC LIMIT 10`)
	if err != nil {
		logger.Error("Failed to get recent records: %v", err)
		return emptySummary()
	}
	defer recordRows.Close()

	var recentRecords []UsageRecord
	for recordRows.Next() {
		var r UsageRecord
		var timestamp int64
		if err := recordRows.Scan(&r.ProviderID, &r.ProviderName, &r.Model, &r.InputTokens, &r.OutputTokens, &r.CacheReadInputTokens, &r.TotalTokens, &r.Cost, &r.Duration, &r.TimeToFirst, &timestamp, &r.Group, &r.Type); err != nil {
			continue
		}
		r.Timestamp = time.Unix(0, timestamp)
		recentRecords = append(recentRecords, r)
	}

	return map[string]interface{}{
		"total_input_tokens":      totalInput,
		"total_output_tokens":     totalOutput,
		"total_cache_read_tokens": totalCacheRead,
		"total_tokens":            totalInput + totalOutput + totalCacheRead,
		"total_cost":              totalCost,
		"total_request_count":     totalRequestCount,
		"provider_stats":          providerStatsArray,
		"key_stats":               keyStatsArray,
		"recent_records":          recentRecords,
	}
}

// GetTodaySummary 获取今日统计
func (s *Stats) GetTodaySummary() map[string]interface{} {
	if db == nil {
		return emptySummary()
	}

	now := time.Now()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	startNano := startOfDay.UnixNano()

	// Get provider stats for today
	providerRows, err := db.Query(`SELECT provider_id, provider_name, input_tokens, output_tokens, cache_read_input_tokens, total_tokens, total_cost, request_count FROM provider_stats`)
	if err != nil {
		logger.Error("Failed to get provider stats: %v", err)
		return emptySummary()
	}
	defer providerRows.Close()

	var providerStatsArray []*ProviderStats
	for providerRows.Next() {
		var ps ProviderStats
		if err := providerRows.Scan(&ps.ProviderID, &ps.ProviderName, &ps.InputTokens, &ps.OutputTokens, &ps.CacheReadInputTokens, &ps.TotalTokens, &ps.TotalCost, &ps.RequestCount); err != nil {
			continue
		}
		providerStatsArray = append(providerStatsArray, &ps)
	}
	sort.Slice(providerStatsArray, func(i, j int) bool {
		return providerStatsArray[i].ProviderName < providerStatsArray[j].ProviderName
	})

	// Get key stats for today
	keyRows, err := db.Query(`SELECT key_id, input_tokens, output_tokens, cache_read_input_tokens, total_tokens, total_cost, ip_addresses, request_count FROM key_stats`)
	if err != nil {
		logger.Error("Failed to get key stats: %v", err)
		return emptySummary()
	}
	defer keyRows.Close()

	var keyStatsArray []*KeyStats
	for keyRows.Next() {
		var ks KeyStats
		var ipsJSON string
		if err := keyRows.Scan(&ks.KeyID, &ks.InputTokens, &ks.OutputTokens, &ks.CacheReadInputTokens, &ks.TotalTokens, &ks.TotalCost, &ipsJSON, &ks.RequestCount); err != nil {
			continue
		}
		json.Unmarshal([]byte(ipsJSON), &ks.IPAddresses)
		keyStatsArray = append(keyStatsArray, &ks)
	}
	sort.Slice(keyStatsArray, func(i, j int) bool {
		return keyStatsArray[i].KeyID < keyStatsArray[j].KeyID
	})

	// Get totals for today only
	var totalInput, totalOutput, totalCacheRead, totalRequestCount int
	var totalCost float64
	err = db.QueryRow(`SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(cache_read_input_tokens), 0), COUNT(*), COALESCE(SUM(cost), 0.0) FROM usage_records WHERE timestamp >= ?`, startNano).Scan(&totalInput, &totalOutput, &totalCacheRead, &totalRequestCount, &totalCost)
	if err != nil {
		logger.Error("Failed to get today totals from usage_records: %v", err)
	}

	// Get recent records for today (last 10)
	recordRows, err := db.Query(`SELECT provider_id, provider_name, model, input_tokens, output_tokens, cache_read_input_tokens, total_tokens, cost, duration_ms, time_to_first_ms, timestamp, group_name, type_name FROM usage_records WHERE timestamp >= ? ORDER BY timestamp DESC LIMIT 10`, startNano)
	if err != nil {
		logger.Error("Failed to get recent records: %v", err)
		return emptySummary()
	}
	defer recordRows.Close()

	var recentRecords []UsageRecord
	for recordRows.Next() {
		var r UsageRecord
		var timestamp int64
		if err := recordRows.Scan(&r.ProviderID, &r.ProviderName, &r.Model, &r.InputTokens, &r.OutputTokens, &r.CacheReadInputTokens, &r.TotalTokens, &r.Cost, &r.Duration, &r.TimeToFirst, &timestamp, &r.Group, &r.Type); err != nil {
			continue
		}
		r.Timestamp = time.Unix(0, timestamp)
		recentRecords = append(recentRecords, r)
	}

	return map[string]interface{}{
		"total_input_tokens":      totalInput,
		"total_output_tokens":     totalOutput,
		"total_cache_read_tokens": totalCacheRead,
		"total_tokens":            totalInput + totalOutput + totalCacheRead,
		"total_cost":              totalCost,
		"total_request_count":     totalRequestCount,
		"provider_stats":          providerStatsArray,
		"key_stats":               keyStatsArray,
		"recent_records":          recentRecords,
	}
}

// DailyStats 每日统计结构
type DailyStats struct {
	Date                 string  `json:"date"`
	InputTokens          int     `json:"input_tokens"`
	OutputTokens         int     `json:"output_tokens"`
	CacheReadInputTokens int     `json:"cache_read_input_tokens"`
	TotalTokens          int     `json:"total_tokens"`
	TotalCost            float64 `json:"total_cost"`
	RequestCount         int     `json:"request_count"`
}

// GetDailyHistory 获取最近7天每日统计
func (s *Stats) GetDailyHistory() []DailyStats {
	if db == nil {
		return []DailyStats{}
	}

	var result []DailyStats
	now := time.Now()

	for i := 0; i < 7; i++ {
		date := now.AddDate(0, 0, -i)
		startOfDay := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, date.Location())
		endOfDay := startOfDay.AddDate(0, 0, 1)
		startNano := startOfDay.UnixNano()
		endNano := endOfDay.UnixNano()

		var inputTokens, outputTokens, cacheReadTokens, requestCount int
		var totalCost float64

		err := db.QueryRow(`
			SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(cache_read_input_tokens), 0), COUNT(*), COALESCE(SUM(cost), 0.0)
			FROM usage_records WHERE timestamp >= ? AND timestamp < ?`,
			startNano, endNano).Scan(&inputTokens, &outputTokens, &cacheReadTokens, &requestCount, &totalCost)

		if err != nil {
			logger.Error("Failed to get daily stats for %s: %v", startOfDay.Format("2006-01-02"), err)
		}

		result = append(result, DailyStats{
			Date:                 startOfDay.Format("2006-01-02"),
			InputTokens:          inputTokens,
			OutputTokens:         outputTokens,
			CacheReadInputTokens: cacheReadTokens,
			TotalTokens:          inputTokens + outputTokens + cacheReadTokens,
			TotalCost:            totalCost,
			RequestCount:         requestCount,
		})
	}

	return result
}

func emptySummary() map[string]interface{} {
	return map[string]interface{}{
		"total_input_tokens":      0,
		"total_output_tokens":     0,
		"total_cache_read_tokens": 0,
		"total_tokens":            0,
		"total_cost":              0.0,
		"total_request_count":     0,
		"provider_stats":          []*ProviderStats{},
		"key_stats":               []*KeyStats{},
		"recent_records":          []UsageRecord{},
	}
}

func GetKeyStats(keyID string) *KeyStats {
	if db == nil {
		return nil
	}

	var ks KeyStats
	var ipsJSON string
	err := db.QueryRow(`SELECT key_id, input_tokens, output_tokens, cache_read_input_tokens, total_tokens, total_cost, ip_addresses, request_count FROM key_stats WHERE key_id = ?`, keyID).Scan(&ks.KeyID, &ks.InputTokens, &ks.OutputTokens, &ks.CacheReadInputTokens, &ks.TotalTokens, &ks.TotalCost, &ipsJSON, &ks.RequestCount)
	if err != nil {
		return nil
	}
	json.Unmarshal([]byte(ipsJSON), &ks.IPAddresses)
	return &ks
}

func ResetStats() {
	if db == nil {
		return
	}

	_, err := db.Exec(`DELETE FROM usage_records; DELETE FROM provider_stats; DELETE FROM key_stats;`)
	if err != nil {
		logger.Error("Failed to reset stats: %v", err)
	}
	logger.Info("✅ 所有统计数据已重置")
}

func ResetProviderStats(providerID string) {
	if db == nil {
		return
	}

	tx, err := db.Begin()
	if err != nil {
		logger.Error("Failed to begin transaction: %v", err)
		return
	}
	defer tx.Rollback()

	// First get all key_ids that have usage_records for this provider
	rows, err := tx.Query(`SELECT DISTINCT key_id FROM usage_records WHERE provider_id = ?`, providerID)
	if err != nil {
		logger.Error("Failed to get key_ids: %v", err)
		return
	}
	var keyIDs []string
	for rows.Next() {
		var keyID string
		if err := rows.Scan(&keyID); err != nil {
			continue
		}
		keyIDs = append(keyIDs, keyID)
	}
	rows.Close()

	// Delete usage_records for this provider
	_, err = tx.Exec(`DELETE FROM usage_records WHERE provider_id = ?`, providerID)
	if err != nil {
		logger.Error("Failed to reset provider usage records: %v", err)
		return
	}

	// Delete provider_stats for this provider
	_, err = tx.Exec(`DELETE FROM provider_stats WHERE provider_id = ?`, providerID)
	if err != nil {
		logger.Error("Failed to reset provider stats: %v", err)
		return
	}

	// Recalculate key_stats for affected keys based on remaining usage_records
	for _, keyID := range keyIDs {
		// Get aggregated stats from remaining usage_records for this key
		var inputTokens, outputTokens, cacheReadTokens, totalTokens, requestCount int
		var totalCost float64
		var ipsJSON string

		err := tx.QueryRow(`
			SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
				COALESCE(SUM(cache_read_input_tokens), 0), COALESCE(SUM(total_tokens), 0), COUNT(*), COALESCE(SUM(cost), 0.0),
				COALESCE((SELECT ip_addresses FROM key_stats WHERE key_id = ?), '[]')
			FROM usage_records WHERE key_id = ?`, keyID, keyID).Scan(
			&inputTokens, &outputTokens, &cacheReadTokens, &totalTokens, &requestCount, &totalCost, &ipsJSON)
		if err != nil && err != sql.ErrNoRows {
			logger.Error("Failed to recalculate key stats for %s: %v", keyID, err)
			continue
		}

		if requestCount == 0 {
			// No more usage_records for this key, delete the key_stats entry
			_, err = tx.Exec(`DELETE FROM key_stats WHERE key_id = ?`, keyID)
			if err != nil {
				logger.Error("Failed to delete key_stats for %s: %v", keyID, err)
			}
		} else {
			// Update key_stats with recalculated values
			_, err = tx.Exec(`
				UPDATE key_stats SET
					input_tokens = ?,
					output_tokens = ?,
					cache_read_input_tokens = ?,
					total_tokens = ?,
					total_cost = ?,
					request_count = ?
				WHERE key_id = ?`,
				inputTokens, outputTokens, cacheReadTokens, totalTokens, totalCost, requestCount, keyID)
			if err != nil {
				logger.Error("Failed to update key_stats for %s: %v", keyID, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		logger.Error("Failed to commit: %v", err)
		return
	}

	logger.Info("✅ 供应商 %s 的统计数据已重置", providerID)
}

func ResetKeyStats(keyID string) {
	if db == nil {
		return
	}

	tx, err := db.Begin()
	if err != nil {
		logger.Error("Failed to begin transaction: %v", err)
		return
	}
	defer tx.Rollback()

	_, err = tx.Exec(`DELETE FROM key_stats WHERE key_id = ?`, keyID)
	if err != nil {
		logger.Error("Failed to reset key stats: %v", err)
		return
	}

	_, err = tx.Exec(`DELETE FROM usage_records WHERE key_id = ?`, keyID)
	if err != nil {
		logger.Error("Failed to reset key usage records: %v", err)
		return
	}

	if err := tx.Commit(); err != nil {
		logger.Error("Failed to commit: %v", err)
		return
	}

	logger.Info("✅ 密钥 %s 的统计数据已重置", keyID)
}

// KeyUsage holds current usage for a key
type KeyUsage struct {
	DailyReqCount int
	DailyCost     float64
	TotalReqCount int
	TotalCost     float64
}

// GetKeyUsage gets the current usage for a key (daily and total)
func GetKeyUsage(keyID string) *KeyUsage {
	if db == nil {
		return nil
	}

	usage := &KeyUsage{}

	// Get daily usage
	today := time.Now().Format("2006-01-02")
	err := db.QueryRow(`SELECT COALESCE(request_count, 0), COALESCE(total_cost, 0) FROM key_daily_stats WHERE key_id = ? AND date = ?`, keyID, today).Scan(&usage.DailyReqCount, &usage.DailyCost)
	if err != nil && err != sql.ErrNoRows {
		logger.Error("Failed to get daily key usage: %v", err)
	}

	// Get total usage
	err = db.QueryRow(`SELECT COALESCE(request_count, 0), COALESCE(total_cost, 0) FROM key_stats WHERE key_id = ?`, keyID).Scan(&usage.TotalReqCount, &usage.TotalCost)
	if err != nil && err != sql.ErrNoRows {
		logger.Error("Failed to get total key usage: %v", err)
	}

	return usage
}

// CheckKeyLimit checks if a request is allowed based on key limits
// Returns (allowed bool, reason string)
func CheckKeyLimit(keyID string, keyDailyReqLimit, keyTotalReqLimit int, keyDailyCostLimit, keyTotalCostLimit float64) (bool, string) {
	if keyDailyReqLimit <= 0 && keyTotalReqLimit <= 0 && keyDailyCostLimit <= 0 && keyTotalCostLimit <= 0 {
		return true, ""
	}

	usage := GetKeyUsage(keyID)
	if usage == nil {
		return true, ""
	}

	if keyDailyReqLimit > 0 && usage.DailyReqCount >= keyDailyReqLimit {
		return false, fmt.Sprintf("每日请求次数限额已用尽 (%d/%d)", usage.DailyReqCount, keyDailyReqLimit)
	}

	if keyTotalReqLimit > 0 && usage.TotalReqCount >= keyTotalReqLimit {
		return false, fmt.Sprintf("总请求次数限额已用尽 (%d/%d)", usage.TotalReqCount, keyTotalReqLimit)
	}

	if keyDailyCostLimit > 0 && usage.DailyCost >= keyDailyCostLimit {
		return false, fmt.Sprintf("每日花费限额已用尽 ($%.4f/$%.4f)", usage.DailyCost, keyDailyCostLimit)
	}

	if keyTotalCostLimit > 0 && usage.TotalCost >= keyTotalCostLimit {
		return false, fmt.Sprintf("总花费限额已用尽 ($%.4f/$%.4f)", usage.TotalCost, keyTotalCostLimit)
	}

	return true, ""
}
