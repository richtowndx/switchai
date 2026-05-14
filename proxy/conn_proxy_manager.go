package proxy

import (
	"fmt"
	"sync"
	"time"

	"switchai/config"
	"switchai/logger"
)

// ConnProxyManager 管理连接级别的 CcProxy 实例
// 一个 TCP 连接（RemoteAddr）对应一个 CcProxy，确保多次 HTTP 请求复用同一个代理
type ConnProxyManager struct {
	// proxies 存储连接级别的 CcProxy 实例
	// key: RemoteAddr (IP:PORT), value: *ConnProxyEntry
	proxies sync.Map

	// cleanupInterval 清理间隔
	cleanupInterval time.Duration

	// connTimeout 连接超时时间（超过此时间未活动的连接将被清理）
	connTimeout time.Duration
}

// ConnProxyEntry 连接代理条目（简化版，移除 tool_use_id 跟踪）
type ConnProxyEntry struct {
	// RemoteAddr 客户端地址（IP:PORT）
	RemoteAddr string

	// Proxy CcProxy 实例
	Proxy CcProxy

	// Provider 关联的 Provider
	Provider *config.Provider

	// CreatedAt 创建时间
	CreatedAt time.Time

	// LastUsedAt 最后使用时间
	LastUsedAt time.Time

	// RequestCount 请求计数
	RequestCount int64

	// clientHash 客户端 hash（用于 provider 选择）
	clientHash uint64

	// providerIdx provider 索引（用于故障切换）
	providerIdx int
}

// NewConnProxyManager 创建连接代理管理器
func NewConnProxyManager() *ConnProxyManager {
	mgr := &ConnProxyManager{
		cleanupInterval: 30 * time.Second,
		connTimeout:     5 * time.Minute,
	}

	// 启动清理协程
	go mgr.cleanupLoop()

	return mgr
}

// GetOrCreate 获取或创建连接级别的 CcProxy
// remoteAddr: 客户端地址（IP:PORT）
// provider: 优先使用的 provider（如果为 nil，则使用 hash 选择）
func (m *ConnProxyManager) GetOrCreate(remoteAddr string, preferredProvider *config.Provider) (*ConnProxyEntry, error) {
	// 尝试加载现有代理
	if val, ok := m.proxies.Load(remoteAddr); ok {
		entry := val.(*ConnProxyEntry)
		entry.LastUsedAt = time.Now()
		entry.RequestCount++
		logger.Info("[ConnProxyManager] 复用现有 CcProxy: %s -> %s (count: %d)",
			remoteAddr, entry.Provider.Name, entry.RequestCount)
		return entry, nil
	}

	// 确定要使用的 provider
	var provider *config.Provider
	var clientHash uint64
	var providerIdx int

	if preferredProvider != nil {
		provider = preferredProvider
		clientHash = hashClientRemote(remoteAddr)
		providerIdx = 0
	} else {
		// 基于 hash 选择 provider
		clientHash = hashClientRemote(remoteAddr)
		provider = config.GetConfig().GetClientHashedProvider(clientHash, 0)
		providerIdx = 0
	}

	if provider == nil {
		return nil, fmt.Errorf("no provider available for client %s", remoteAddr)
	}

	// 创建新的 CcProxy
	proxy, err := NewCcProxy(provider)
	if err != nil {
		return nil, fmt.Errorf("failed to create CcProxy for provider %s: %w", provider.Name, err)
	}

	// 创建新条目
	entry := &ConnProxyEntry{
		RemoteAddr:   remoteAddr,
		Proxy:        proxy,
		Provider:     provider,
		CreatedAt:    time.Now(),
		LastUsedAt:   time.Now(),
		RequestCount: 1,
		clientHash:   clientHash,
		providerIdx:  providerIdx,
	}

	// 原子存储：如果已存在则使用现有值
	actual, _ := m.proxies.LoadOrStore(remoteAddr, entry)
	actualEntry := actual.(*ConnProxyEntry)

	// 如果是新建的，打印日志
	if actualEntry == entry {
		logger.Info("[ConnProxyManager] 创建新 CcProxy: %s -> %s (format: isOpenAI=%v)",
			remoteAddr, provider.Name, provider.IsOpenAIFormat)
	} else {
		// 如果已经存在，关闭我们创建的 proxy
		_ = proxy.Close()
		actualEntry.LastUsedAt = time.Now()
		actualEntry.RequestCount++
		logger.Info("[ConnProxyManager] 复用并发创建的 CcProxy: %s -> %s (count: %d)",
			remoteAddr, actualEntry.Provider.Name, actualEntry.RequestCount)
	}

	return actualEntry, nil
}

// Remove 移除连接的代理（用于连接关闭或故障切换）
func (m *ConnProxyManager) Remove(remoteAddr string) error {
	if val, ok := m.proxies.LoadAndDelete(remoteAddr); ok {
		entry := val.(*ConnProxyEntry)
		logger.Info("[ConnProxyManager] 移除 CcProxy: %s -> %s (total requests: %d, lifetime: %v)",
			remoteAddr, entry.Provider.Name, entry.RequestCount, time.Since(entry.CreatedAt))
		if entry.Proxy != nil {
			return entry.Proxy.Close()
		}
	}
	return nil
}

// Get 获取连接的代理（不创建）
func (m *ConnProxyManager) Get(remoteAddr string) (*ConnProxyEntry, bool) {
	if val, ok := m.proxies.Load(remoteAddr); ok {
		entry := val.(*ConnProxyEntry)
		return entry, true
	}
	return nil, false
}

// SwitchProvider 切换连接的 provider（用于故障切换）
func (m *ConnProxyManager) SwitchProvider(remoteAddr string, attempt int) (*ConnProxyEntry, error) {
	// 先移除旧的 proxy
	_ = m.Remove(remoteAddr)

	// 获取新的 provider
	clientHash := hashClientRemote(remoteAddr)
	newProvider := config.GetConfig().GetClientHashedProvider(clientHash, attempt)
	if newProvider == nil {
		return nil, fmt.Errorf("no provider available for attempt %d", attempt)
	}

	// 创建新的 entry
	return m.GetOrCreate(remoteAddr, newProvider)
}

// cleanupLoop 清理空闲连接的协程
func (m *ConnProxyManager) cleanupLoop() {
	ticker := time.NewTicker(m.cleanupInterval)
	defer ticker.Stop()

	for range ticker.C {
		m.CleanupIdle()
	}
}

// CleanupIdle 清理空闲超时的连接
func (m *ConnProxyManager) CleanupIdle() int {
	cleaned := 0
	cutoff := time.Now().Add(-m.connTimeout)

	m.proxies.Range(func(key, value interface{}) bool {
		entry := value.(*ConnProxyEntry)
		if entry.LastUsedAt.Before(cutoff) {
			if m.proxies.CompareAndDelete(key, value) {
				logger.Info("[ConnProxyManager] 清理空闲连接: %s (idle: %v, requests: %d)",
					entry.RemoteAddr, time.Since(entry.LastUsedAt), entry.RequestCount)
				if entry.Proxy != nil {
					_ = entry.Proxy.Close()
				}
				cleaned++
			}
		}
		return true
	})

	if cleaned > 0 {
		logger.Info("[ConnProxyManager] 清理了 %d 个空闲连接", cleaned)
	}

	return cleaned
}

// Stats 获取管理器统计信息
func (m *ConnProxyManager) Stats() map[string]interface{} {
	stats := make(map[string]interface{})
	var totalConns, totalReqs int
	var oldestConn *ConnProxyEntry

	m.proxies.Range(func(key, value interface{}) bool {
		entry := value.(*ConnProxyEntry)
		totalConns++
		totalReqs += int(entry.RequestCount)
		if oldestConn == nil || entry.CreatedAt.Before(oldestConn.CreatedAt) {
			oldestConn = entry
		}
		return true
	})

	stats["total_connections"] = totalConns
	stats["total_requests"] = totalReqs
	stats["conn_timeout"] = m.connTimeout.String()

	if oldestConn != nil {
		stats["oldest_conn_age"] = time.Since(oldestConn.CreatedAt).String()
		stats["oldest_conn_remote"] = oldestConn.RemoteAddr
	}

	return stats
}

// Clear 清空所有连接（用于关闭）
func (m *ConnProxyManager) Clear() error {
	var firstErr error

	m.proxies.Range(func(key, value interface{}) bool {
		entry := value.(*ConnProxyEntry)
		if entry.Proxy != nil {
			if err := entry.Proxy.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return true
	})

	m.proxies.Clear()
	return firstErr
}

// 全局单例
var globalConnProxyMgr = NewConnProxyManager()

// GetGlobalConnProxyManager 获取全局连接代理管理器
func GetGlobalConnProxyManager() *ConnProxyManager {
	return globalConnProxyMgr
}
