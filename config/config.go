package config

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"sort"
	"switchai/appdata"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// SessionTokenEntry represents a single session token entry
type SessionTokenEntry struct {
	Token     string    `json:"token"`
	CreatedAt time.Time `json:"created_at"`
}

type Provider struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	BaseURL        string `json:"base_url"`
	APIKey         string `json:"api_key"`
	Model          string `json:"model"`         // 兼容旧字段
	DefaultModel   string `json:"default_model"` // 默认模型
	HaikuModel     string `json:"haiku_model"`   // Haiku 模型
	SonnetModel    string `json:"sonnet_model"`  // Sonnet 模型
	OpusModel      string `json:"opus_model"`    // Opus 模型
	FastModel      string `json:"fast_model"`    // Fast 模型
	IsActive       bool   `json:"is_active"`
	CreatedAt      string `json:"created_at"`
	Order          int    `json:"order"`
	IsOpenAIFormat bool   `json:"is_openai_format"` // 标识是否为 OpenAI 格式的 API
}

// ResolveModel 根据传入的模型键名查找供应商对应的实际模型名
func (p *Provider) ResolveModel(incomingModel string) string {
	m := map[string]string{
		"default_model": p.DefaultModel,
		"haiku_model":   p.HaikuModel,
		"sonnet_model":  p.SonnetModel,
		"opus_model":    p.OpusModel,
		"fast_model":    p.FastModel,
	}
	if actual, ok := m[incomingModel]; ok && actual != "" {
		return actual
	}
	// 兜底: default_model
	if p.DefaultModel != "" {
		return p.DefaultModel
	}
	// 兼容旧字段 Model
	if p.Model != "" {
		return p.Model
	}
	return incomingModel
}

type ServerKey struct {
	ID             string  `json:"id"`               // 密钥ID
	Key            string  `json:"key"`              // 密钥值 sk-xxxx
	Remark         string  `json:"remark"`           // 备注
	IsEnabled      bool    `json:"is_enabled"`       // 是否启用
	CreatedAt      string  `json:"created_at"`       // 创建时间
	Order          int     `json:"order"`            // 排序序号
	DailyReqLimit  int     `json:"daily_req_limit"`  // 每日请求次数限额 (0=不限)
	TotalReqLimit  int     `json:"total_req_limit"`  // 总请求次数限额 (0=不限)
	DailyCostLimit float64 `json:"daily_cost_limit"` // 每日花费限额 (0=不限)
	TotalCostLimit float64 `json:"total_cost_limit"` // 总花费限额 (0=不限)
}

type Config struct {
	Providers      []Provider          `json:"providers"`
	ServerKeys     []ServerKey         `json:"server_keys"` // 服务器密钥列表
	ActiveProvider string              `json:"active_provider"`
	TOTPSecret     string              `json:"totp_secret"`    // TOTP 2FA 密钥
	TOTPEnabled    bool                `json:"totp_enabled"`   // 是否已启用 2FA
	SessionTokens  []SessionTokenEntry `json:"session_tokens"` // 多端登录的会话 token 列表
	SkipAuth       bool                `json:"skip_auth"`      // 跳过认证（内网部署）
	mu             sync.RWMutex
}

var skipAuthMode bool

// SetSkipAuth 设置跳过认证模式
func SetSkipAuth(skip bool) {
	skipAuthMode = skip
}

// IsSkipAuth 返回是否跳过认证模式
func IsSkipAuth() bool {
	return skipAuthMode
}

var cfg *Config
var db *sql.DB

// getDBPath 返回数据库文件路径
func getDBPath() string {
	return appdata.GetConfigPath("config.db")
}

func Init() error {
	// 初始化数据库
	var err error
	dbPath := getDBPath()
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}

	// 创建表
	if err := initDB(); err != nil {
		db.Close()
		return err
	}

	cfg = &Config{}

	// 加载配置
	if err := cfg.Load(); err != nil {
		return err
	}

	return nil
}

func initDB() error {
	schema := `
	CREATE TABLE IF NOT EXISTS config (
		key TEXT PRIMARY KEY,
		value TEXT
	);
	CREATE TABLE IF NOT EXISTS providers (
		id TEXT PRIMARY KEY,
		name TEXT,
		base_url TEXT,
		api_key TEXT,
		model TEXT,
		is_active INTEGER,
		created_at TEXT,
		order_num INTEGER,
		is_openai_format INTEGER DEFAULT 0,
		default_model TEXT DEFAULT '',
		haiku_model TEXT DEFAULT '',
		sonnet_model TEXT DEFAULT '',
		opus_model TEXT DEFAULT '',
		fast_model TEXT DEFAULT ''
	);
	CREATE TABLE IF NOT EXISTS server_keys (
		id TEXT PRIMARY KEY,
		key TEXT,
		remark TEXT,
		is_enabled INTEGER,
		created_at TEXT,
		order_num INTEGER,
		daily_req_limit INTEGER DEFAULT 0,
		total_req_limit INTEGER DEFAULT 0,
		daily_cost_limit REAL DEFAULT 0,
		total_cost_limit REAL DEFAULT 0
	);
	`
	_, err := db.Exec(schema)
	if err != nil {
		return err
	}

	// 迁移：添加 is_openai_format 列（如果不存在）
	db.Exec("ALTER TABLE providers ADD COLUMN is_openai_format INTEGER DEFAULT 0")
	db.Exec("ALTER TABLE providers ADD COLUMN default_model TEXT DEFAULT ''")
	db.Exec("ALTER TABLE providers ADD COLUMN haiku_model TEXT DEFAULT ''")
	db.Exec("ALTER TABLE providers ADD COLUMN sonnet_model TEXT DEFAULT ''")
	db.Exec("ALTER TABLE providers ADD COLUMN opus_model TEXT DEFAULT ''")
	db.Exec("ALTER TABLE providers ADD COLUMN fast_model TEXT DEFAULT ''")

	return nil
}

func (c *Config) Load() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 加载 active_provider
	var activeProvider string
	err := db.QueryRow("SELECT value FROM config WHERE key = 'active_provider'").Scan(&activeProvider)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	c.ActiveProvider = activeProvider

	// 加载 totp_secret
	var totpSecret string
	err = db.QueryRow("SELECT value FROM config WHERE key = 'totp_secret'").Scan(&totpSecret)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	c.TOTPSecret = totpSecret

	// 加载 totp_enabled
	var totpEnabled int
	err = db.QueryRow("SELECT value FROM config WHERE key = 'totp_enabled'").Scan(&totpEnabled)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	c.TOTPEnabled = totpEnabled == 1

	// 加载 session_tokens (JSON 数组)
	var sessionTokensJSON string
	err = db.QueryRow("SELECT value FROM config WHERE key = 'session_tokens'").Scan(&sessionTokensJSON)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if sessionTokensJSON != "" {
		json.Unmarshal([]byte(sessionTokensJSON), &c.SessionTokens)
	} else {
		// 兼容旧版本：尝试加载旧的 session_token
		var oldToken string
		err = db.QueryRow("SELECT value FROM config WHERE key = 'session_token'").Scan(&oldToken)
		if err == nil && oldToken != "" {
			c.SessionTokens = []SessionTokenEntry{{Token: oldToken, CreatedAt: time.Now()}}
		}
	}

	// 加载 providers
	rows, err := db.Query("SELECT id, name, base_url, api_key, model, is_active, created_at, order_num, COALESCE(is_openai_format, 0), COALESCE(default_model, ''), COALESCE(haiku_model, ''), COALESCE(sonnet_model, ''), COALESCE(opus_model, ''), COALESCE(fast_model, '') FROM providers ORDER BY order_num")
	if err != nil {
		return err
	}
	defer rows.Close()

	c.Providers = nil
	for rows.Next() {
		var p Provider
		var isActive int
		var isOpenAIFormat int
		if err := rows.Scan(&p.ID, &p.Name, &p.BaseURL, &p.APIKey, &p.Model, &isActive, &p.CreatedAt, &p.Order, &isOpenAIFormat, &p.DefaultModel, &p.HaikuModel, &p.SonnetModel, &p.OpusModel, &p.FastModel); err != nil {
			return err
		}
		p.IsActive = isActive == 1
		p.IsOpenAIFormat = isOpenAIFormat == 1
		c.Providers = append(c.Providers, p)
	}

	// 加载 server_keys
	rows, err = db.Query("SELECT id, key, remark, is_enabled, created_at, order_num, COALESCE(daily_req_limit, 0), COALESCE(total_req_limit, 0), COALESCE(daily_cost_limit, 0), COALESCE(total_cost_limit, 0) FROM server_keys ORDER BY order_num")
	if err != nil {
		return err
	}
	defer rows.Close()

	c.ServerKeys = nil
	for rows.Next() {
		var k ServerKey
		var isEnabled int
		if err := rows.Scan(&k.ID, &k.Key, &k.Remark, &isEnabled, &k.CreatedAt, &k.Order, &k.DailyReqLimit, &k.TotalReqLimit, &k.DailyCostLimit, &k.TotalCostLimit); err != nil {
			return err
		}
		k.IsEnabled = isEnabled == 1
		c.ServerKeys = append(c.ServerKeys, k)
	}

	return nil
}

func (c *Config) Save() error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.save()
}

func (c *Config) save() error {
	// 保存 active_provider
	_, err := db.Exec("INSERT OR REPLACE INTO config (key, value) VALUES ('active_provider', ?)", c.ActiveProvider)
	if err != nil {
		return err
	}

	// 保存 totp_secret
	_, err = db.Exec("INSERT OR REPLACE INTO config (key, value) VALUES ('totp_secret', ?)", c.TOTPSecret)
	if err != nil {
		return err
	}

	// 保存 totp_enabled
	totpEnabled := 0
	if c.TOTPEnabled {
		totpEnabled = 1
	}
	_, err = db.Exec("INSERT OR REPLACE INTO config (key, value) VALUES ('totp_enabled', ?)", totpEnabled)
	if err != nil {
		return err
	}

	// 保存 session_tokens (JSON 数组)
	sessionTokensJSON, _ := json.Marshal(c.SessionTokens)
	_, err = db.Exec("INSERT OR REPLACE INTO config (key, value) VALUES ('session_tokens', ?)", string(sessionTokensJSON))
	if err != nil {
		return err
	}

	// 删除并重新插入 providers
	_, err = db.Exec("DELETE FROM providers")
	if err != nil {
		return err
	}
	for _, p := range c.Providers {
		isActive := 0
		if p.IsActive {
			isActive = 1
		}
		isOpenAIFormat := 0
		if p.IsOpenAIFormat {
			isOpenAIFormat = 1
		}
		_, err = db.Exec("INSERT INTO providers (id, name, base_url, api_key, model, is_active, created_at, order_num, is_openai_format, default_model, haiku_model, sonnet_model, opus_model, fast_model) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			p.ID, p.Name, p.BaseURL, p.APIKey, p.Model, isActive, p.CreatedAt, p.Order, isOpenAIFormat, p.DefaultModel, p.HaikuModel, p.SonnetModel, p.OpusModel, p.FastModel)
		if err != nil {
			return err
		}
	}

	// 删除并重新插入 server_keys
	_, err = db.Exec("DELETE FROM server_keys")
	if err != nil {
		return err
	}
	for _, k := range c.ServerKeys {
		isEnabled := 0
		if k.IsEnabled {
			isEnabled = 1
		}
		_, err = db.Exec("INSERT INTO server_keys (id, key, remark, is_enabled, created_at, order_num, daily_req_limit, total_req_limit, daily_cost_limit, total_cost_limit) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			k.ID, k.Key, k.Remark, isEnabled, k.CreatedAt, k.Order, k.DailyReqLimit, k.TotalReqLimit, k.DailyCostLimit, k.TotalCostLimit)
		if err != nil {
			return err
		}
	}

	return nil
}

func GetConfig() *Config {
	return cfg
}

func (c *Config) GetActiveProvider() *Provider {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for i := range c.Providers {
		if c.Providers[i].ID == c.ActiveProvider {
			return &c.Providers[i]
		}
	}

	// 如果没有活跃的提供商，返回第一个
	if len(c.Providers) > 0 {
		return &c.Providers[0]
	}

	return nil
}

// GetProviderByID 根据ID获取提供商
func (c *Config) GetProviderByID(id string) *Provider {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for i := range c.Providers {
		if c.Providers[i].ID == id {
			return &c.Providers[i]
		}
	}
	return nil
}

// round-robin 轮询计数器
var roundRobinCounter uint64

// GetNextProvider 轮询方式获取下一个匹配格式的提供商
func (c *Config) GetNextProvider(isOpenAIFormat bool) *Provider {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var matching []int
	for i := range c.Providers {
		if c.Providers[i].IsOpenAIFormat == isOpenAIFormat {
			matching = append(matching, i)
		}
	}

	if len(matching) == 0 && isOpenAIFormat {
		for i := range c.Providers {
			if c.Providers[i].ID == c.ActiveProvider {
				return &c.Providers[i]
			}
		}
		if len(c.Providers) > 0 {
			return &c.Providers[0]
		}
		return nil
	}

	n := atomic.AddUint64(&roundRobinCounter, 1)
	idx := matching[int(n-1)%len(matching)]
	return &c.Providers[idx]
}

// GetProviderByFormat 获取匹配格式的第一个提供商（兼容旧调用）
func (c *Config) GetProviderByFormat(isOpenAIFormat bool) *Provider {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for i := range c.Providers {
		if c.Providers[i].IsOpenAIFormat == isOpenAIFormat {
			return &c.Providers[i]
		}
	}

	return c.GetActiveProvider()
}

func (c *Config) AddProvider(p Provider) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 自动分配序号（最大序号 + 1）
	maxOrder := 0
	for _, provider := range c.Providers {
		if provider.Order > maxOrder {
			maxOrder = provider.Order
		}
	}
	p.Order = maxOrder + 1

	c.Providers = append(c.Providers, p)

	// 如果是第一个提供商，设置为活跃
	if len(c.Providers) == 1 {
		c.ActiveProvider = p.ID
	}

	// 按序号排序
	c.sortProviders()

	return c.save()
}

func (c *Config) UpdateProvider(id string, p Provider) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i := range c.Providers {
		if c.Providers[i].ID == id {
			// 保留原有序号
			p.Order = c.Providers[i].Order
			c.Providers[i] = p
			c.sortProviders()
			return c.save()
		}
	}

	return nil
}

func (c *Config) DeleteProvider(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i := range c.Providers {
		if c.Providers[i].ID == id {
			deletedOrder := c.Providers[i].Order
			c.Providers = append(c.Providers[:i], c.Providers[i+1:]...)

			// 重新调整序号，保持连续
			for j := range c.Providers {
				if c.Providers[j].Order > deletedOrder {
					c.Providers[j].Order--
				}
			}

			// 如果删除的是活跃提供商，切换到第一个
			if c.ActiveProvider == id && len(c.Providers) > 0 {
				c.ActiveProvider = c.Providers[0].ID
			}

			c.sortProviders()
			return c.save()
		}
	}

	return nil
}

func (c *Config) SetActiveProvider(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.ActiveProvider = id
	return c.save()
}

// sortProviders 按序号排序提供商（内部使用，调用前需要加锁）
func (c *Config) sortProviders() {
	sort.Slice(c.Providers, func(i, j int) bool {
		return c.Providers[i].Order < c.Providers[j].Order
	})
}

// GenerateServerKey 生成新的服务器密钥并添加到列表
func (c *Config) GenerateServerKey() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 生成 sk- 开头 + 16位随机字符
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	keyStr := "sk-"
	for _, b := range bytes {
		keyStr += string(chars[int(b)%len(chars)])
	}

	// 查找最大序号
	maxOrder := 0
	for _, k := range c.ServerKeys {
		if k.Order > maxOrder {
			maxOrder = k.Order
		}
	}

	serverKey := ServerKey{
		ID:        uuid.New().String(),
		Key:       keyStr,
		Remark:    "",
		IsEnabled: true,
		CreatedAt: time.Now().Format(time.RFC3339),
		Order:     maxOrder + 1,
	}

	c.ServerKeys = append(c.ServerKeys, serverKey)

	if err := c.save(); err != nil {
		return "", err
	}

	return keyStr, nil
}

// GetServerKeys 获取所有服务器密钥
func (c *Config) GetServerKeys() []ServerKey {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ServerKeys
}

// ValidateServerKey 验证密钥是否有效，返回密钥ID和是否有效
func (c *Config) ValidateServerKey(keyStr string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, k := range c.ServerKeys {
		if k.Key == keyStr && k.IsEnabled {
			return k.ID, true
		}
	}
	return "", false
}

// GetServerKeyByID 获取服务器密钥（包含限额信息）
func (c *Config) GetServerKeyByID(id string) *ServerKey {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for i := range c.ServerKeys {
		if c.ServerKeys[i].ID == id {
			return &c.ServerKeys[i]
		}
	}
	return nil
}

// AddServerKey 添加服务器密钥
func (c *Config) AddServerKey(key ServerKey) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 查找最大序号
	maxOrder := 0
	for _, k := range c.ServerKeys {
		if k.Order > maxOrder {
			maxOrder = k.Order
		}
	}

	key.ID = uuid.New().String()
	key.CreatedAt = time.Now().Format(time.RFC3339)
	key.Order = maxOrder + 1

	c.ServerKeys = append(c.ServerKeys, key)
	return c.save()
}

// UpdateServerKey 更新服务器密钥
func (c *Config) UpdateServerKey(id string, key ServerKey) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i := range c.ServerKeys {
		if c.ServerKeys[i].ID == id {
			// 保留原有序号和创建时间
			key.ID = id
			key.CreatedAt = c.ServerKeys[i].CreatedAt
			key.Order = c.ServerKeys[i].Order
			key.Key = c.ServerKeys[i].Key // 不允许修改密钥值
			c.ServerKeys[i] = key
			return c.save()
		}
	}

	return nil
}

// DeleteServerKey 删除服务器密钥
func (c *Config) DeleteServerKey(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i := range c.ServerKeys {
		if c.ServerKeys[i].ID == id {
			deletedOrder := c.ServerKeys[i].Order
			c.ServerKeys = append(c.ServerKeys[:i], c.ServerKeys[i+1:]...)

			// 重新调整序号
			for j := range c.ServerKeys {
				if c.ServerKeys[j].Order > deletedOrder {
					c.ServerKeys[j].Order--
				}
			}

			return c.save()
		}
	}

	return nil
}

// GetTOTPSecret 获取 TOTP 密钥
func (c *Config) GetTOTPSecret() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.TOTPSecret
}

// IsTOTPEnabled 检查 TOTP 是否已启用
func (c *Config) IsTOTPEnabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.TOTPEnabled
}

// SetTOTPSecret 设置 TOTP 密钥（首次设置时调用）
func (c *Config) SetTOTPSecret(secret string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.TOTPSecret = secret
	c.TOTPEnabled = false // 首次设置时未启用，需要验证后启用
	return c.save()
}

// EnableTOTP 启用 TOTP（验证成功后调用）
func (c *Config) EnableTOTP() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.TOTPEnabled = true
	return c.save()
}

// GetSessionTokens 获取所有会话 tokens
func (c *Config) GetSessionTokens() []SessionTokenEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.SessionTokens
}

// ValidateSessionToken 验证会话 token 是否有效
func (c *Config) ValidateSessionToken(token string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, entry := range c.SessionTokens {
		if entry.Token == token {
			return true
		}
	}
	return false
}

// AddSessionToken 添加新的会话 token
func (c *Config) AddSessionToken(token string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.SessionTokens = append(c.SessionTokens, SessionTokenEntry{
		Token:     token,
		CreatedAt: time.Now(),
	})
	return c.save()
}

// RemoveSessionToken 移除指定的会话 token
func (c *Config) RemoveSessionToken(token string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i, entry := range c.SessionTokens {
		if entry.Token == token {
			c.SessionTokens = append(c.SessionTokens[:i], c.SessionTokens[i+1:]...)
			return c.save()
		}
	}
	return nil
}

// ClearAllSessionTokens 清除所有会话 token
func (c *Config) ClearAllSessionTokens() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.SessionTokens = nil
	return c.save()
}

// ResetTOTP 重置 TOTP 数据（清除密钥和启用状态，但保留其他配置）
func (c *Config) ResetTOTP() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.TOTPSecret = ""
	c.TOTPEnabled = false
	return c.save()
}

// Shutdown 关闭数据库连接
func Shutdown() {
	if db != nil {
		db.Close()
	}
}
