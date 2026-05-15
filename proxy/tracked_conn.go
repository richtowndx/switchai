package proxy

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"switchai/config"
	"switchai/logger"

	"github.com/google/uuid"
)

// TrackedConn 包装 net.Conn，用于跟踪连接状态和流量
type TrackedConn struct {
	net.Conn
	tracker *ConnectionTracker
	info    *ConnectionInfo
}

func (c *TrackedConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.info.BytesRead.Add(int64(n))
		now := time.Now()
		c.info.LastActive.Store(&now)
	}
	return n, err
}

func (c *TrackedConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		c.info.BytesWrite.Add(int64(n))
		now := time.Now()
		c.info.LastActive.Store(&now)
	}
	return n, err
}

func (c *TrackedConn) Close() error {
	err := c.Conn.Close()
	c.tracker.OnClose(c.info, err)
	return err
}

// TrackedListener 包装 net.Listener，用于跟踪连接建立
type TrackedListener struct {
	Listener net.Listener
	Tracker  *ConnectionTracker // 导出字段以便外部设置
	Ctx      context.Context    // 导出字段以便外部设置
}

// Accept 接受新连接并包装为 TrackedConn
func (l *TrackedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}

	// 检测协议（简化版）
	protocol := "HTTP/1.1"
	if _, ok := conn.(*net.TCPConn); ok {
		// 可以添加 HTTP/2 检测逻辑
	}

	// 调用连接建立回调
	info := l.Tracker.OnAccept(conn, protocol, false)

	// 返回包装的连接
	return &TrackedConn{
		Conn:    conn,
		tracker: l.Tracker,
		info:    info,
	}, nil
}

// Close 关闭监听器
func (l *TrackedListener) Close() error {
	return l.Listener.Close()
}

// Addr 返回监听器地址
func (l *TrackedListener) Addr() net.Addr {
	return l.Listener.Addr()
}

// ExtendedConnectionInfo 扩展连接信息，包含 Provider 绑定和流量统计
type ExtendedConnectionInfo struct {
	ID         string
	RemoteAddr string
	StartTime  time.Time
	BytesRead  atomic.Int64
	BytesWrite atomic.Int64
	LastActive atomic.Pointer[time.Time]
	UserAgent  string
	Protocol   string
	IsTLS      bool

	// Provider 绑定（使用小写首字母避免字段冲突）
	provider    *config.Provider
	providerIdx int
	clientHash  uint64
}

// Provider 获取 Provider
func (e *ExtendedConnectionInfo) Provider() *config.Provider {
	return e.provider
}

// SetProvider 设置 Provider
func (e *ExtendedConnectionInfo) SetProvider(p *config.Provider) {
	e.provider = p
}

// ProviderIdx 获取 Provider 索引
func (e *ExtendedConnectionInfo) ProviderIdx() int {
	return e.providerIdx
}

// SetProviderIdx 设置 Provider 索引
func (e *ExtendedConnectionInfo) SetProviderIdx(idx int) {
	e.providerIdx = idx
}

// ClientHash 获取客户端 Hash
func (e *ExtendedConnectionInfo) ClientHash() uint64 {
	return e.clientHash
}

// SetClientHash 设置客户端 Hash
func (e *ExtendedConnectionInfo) SetClientHash(h uint64) {
	e.clientHash = h
}

// ConnectionInfo 兼容性别名
type ConnectionInfo = ExtendedConnectionInfo

// OnAccept 连接建立时的回调
func (t *ConnectionTracker) OnAccept(conn net.Conn, protocol string, isTLS bool) *ConnectionInfo {
	connID := fmt.Sprintf("conn-%d-%s", time.Now().UnixNano(), uuid.New().String()[:8])

	info := &ConnectionInfo{
		ID:         connID,
		RemoteAddr: conn.RemoteAddr().String(),
		StartTime:  time.Now(),
		Protocol:   protocol,
		IsTLS:      isTLS,
	}
	now := time.Now()
	info.LastActive.Store(&now)

	// 基于 client IP:PORT 计算 hash，选择 Provider
	clientHash := hashClientRemote(conn.RemoteAddr().String())
	info.SetClientHash(clientHash)

	provider := config.GetConfig().GetClientHashedProvider(clientHash, 0)
	if provider != nil {
		info.SetProvider(provider)
		info.SetProviderIdx(0)
	}

	t.connections.Store(connID, info)
	t.activeConns.Add(1)

	logger.Info("[ACCEPT] %s from %s (proto: %s, tls: %v, provider: %s, hash: %d)",
		connID, info.RemoteAddr, protocol, isTLS, getProviderName(provider), clientHash)

	return info
}

// OnClose 连接关闭时的回调
func (t *ConnectionTracker) OnClose(info *ConnectionInfo, err error) {
	duration := time.Since(info.StartTime)
	lastActive := info.LastActive.Load()
	var idleTime time.Duration
	if lastActive != nil {
		idleTime = time.Since(*lastActive)
	}

	status := "normal"
	if err != nil {
		status = "error: " + err.Error()
	}

	logger.Info("[CLOSE] %s from %s (duration: %v, idle: %v, read: %s, write: %s, provider: %s, status: %s)",
		info.ID, info.RemoteAddr, duration.Truncate(time.Millisecond),
		idleTime.Truncate(time.Millisecond),
		formatBytes(info.BytesRead.Load()),
		formatBytes(info.BytesWrite.Load()),
		getProviderName(info.Provider()),
		status)

	t.connections.Delete(info.ID)
	t.activeConns.Add(-1)
}

// GetConnProvider 获取连接绑定的 Provider，支持故障切换
func (t *ConnectionTracker) GetConnProvider(connID string, attempt int) *config.Provider {
	if info, ok := t.connections.Load(connID); ok {
		connInfo := info.(*ConnectionInfo)
		if attempt == 0 && connInfo.Provider() != nil {
			return connInfo.Provider()
		}
		// 尝试切换到下一个 provider
		newProvider := config.GetConfig().GetClientHashedProvider(connInfo.ClientHash(), attempt)
		if newProvider != nil {
			connInfo.SetProvider(newProvider)
			connInfo.SetProviderIdx(attempt)
			logger.Info("[SWITCH] %s: provider -> %s (attempt %d)",
				connID, newProvider.Name, attempt)
		}
		return newProvider
	}
	return config.GetConfig().GetClientHashedProvider(0, attempt)
}

// UpdateConnProvider 更新连接的 Provider 绑定（用于故障切换后）
func (t *ConnectionTracker) UpdateConnProvider(connID string, provider *config.Provider, idx int) {
	if info, ok := t.connections.Load(connID); ok {
		connInfo := info.(*ConnectionInfo)
		connInfo.SetProvider(provider)
		connInfo.SetProviderIdx(idx)
	}
}

// PrintStats 打印连接统计信息
func (t *ConnectionTracker) PrintStats() {
	var activeCount int
	var totalBytesRead, totalBytesWrite int64
	var conn_str string

	t.connections.Range(func(key, value interface{}) bool {
		info := value.(*ConnectionInfo)
		activeCount++
		totalBytesRead += info.BytesRead.Load()
		totalBytesWrite += info.BytesWrite.Load()
		providerName := "none"
		isOpenAI := false
		if p := info.Provider(); p != nil {
			providerName = p.Name
			isOpenAI = p.IsOpenAIFormat
		}
		conn_str += fmt.Sprintf("  - %s: %s -> provider: %s (isOpenAI: %v)\n",
			info.ID, info.RemoteAddr, providerName, isOpenAI)
		return true
	})

	logger.Info("[STATS] Active: %d, Total: %d, Read: %s, Write: %s\n%s",
		activeCount,
		t.totalConns.Load(),
		formatBytes(totalBytesRead),
		formatBytes(totalBytesWrite), conn_str)
}

// CleanupStaleConnections 清理超时连接
func (t *ConnectionTracker) CleanupStaleConnections(maxIdle time.Duration) {
	now := time.Now()
	t.connections.Range(func(key, value interface{}) bool {
		info := value.(*ConnectionInfo)
		if lastActive := info.LastActive.Load(); lastActive != nil {
			if now.Sub(*lastActive) > maxIdle {
				logger.Info("[CLEANUP] Removing stale connection: %s (idle: %v)",
					info.ID, now.Sub(*lastActive))
				t.connections.Delete(key)
				t.activeConns.Add(-1)
			}
		}
		return true
	})
}

// StartMonitor 启动监控协程
func (t *ConnectionTracker) StartMonitor(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				t.PrintStats()
				t.CleanupStaleConnections(5 * time.Minute)
			case <-ctx.Done():
				return
			}
		}
	}()
}

// formatBytes 格式化字节大小
func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// getProviderName 获取 provider 名称
func getProviderName(p *config.Provider) string {
	if p == nil {
		return "none"
	}
	return p.Name
}
