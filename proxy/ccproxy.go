package proxy

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	"switchai/config"
)

// scannerBufferPool 用于流式响应扫描的 buffer 池，减少内存分配
var scannerBufferPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 64*1024) // 64KB 初始缓冲
	},
}

// ProxyResponse 代理响应
type ProxyResponse struct {
	// Body 非流式响应体（使用 []byte 避免与 handler 之间的额外拷贝）
	Body []byte
	// StreamCh 流式响应通道
	StreamCh <-chan string
	// ErrCh 错误通道（流式）
	ErrCh <-chan error
	// Error 错误（非流式）
	Error error
	// IsStream 是否为流式响应
	IsStream bool

	ModelName string // 可选：模型名称（用于日志记录）
}

// CcProxy 定义统一的代理接口
// 优化设计：Handler 不需要知道 stream 逻辑，各 Proxy 内部根据请求参数处理
type CcProxy interface {
	// SendOpenAIFormat 发送 OpenAI 格式请求
	// 返回：根据 stream 参数返回非流式或流式响应
	SendOpenAIFormat(ctx context.Context, reqHdr http.Header, reqBody []byte) *ProxyResponse

	// SendAnthropicFormat 发送 Anthropic 格式请求
	// 返回：根据 stream 参数返回非流式或流式响应
	SendAnthropicFormat(ctx context.Context, reqHdr http.Header, reqBody []byte) *ProxyResponse

	// Close 释放代理资源
	Close() error

	// Provider 返回关联的 Provider 配置
	Provider() *config.Provider
}

// ============================================================
// HTTP 连接池管理（并发安全）
// ============================================================

// ConnPool HTTP 连接池，用于复用 TCP 连接
type ConnPool struct {
	providers sync.Map
	createdAt time.Time
}

// ProviderConn 单个提供商的连接
type ProviderConn struct {
	Provider    *config.Provider
	Proxy       CcProxy
	LastUsedAt  time.Time
	ReqCount    int64
	HealthCheck time.Time
}

// NewConnPool 创建连接池
func NewConnPool() *ConnPool {
	return &ConnPool{
		createdAt: time.Now(),
	}
}

// Get 获取或创建提供商连接（并发安全）
func (p *ConnPool) Get(provider *config.Provider) (CcProxy, error) {
	key := provider.Name + "|" + provider.BaseURL

	// 尝试加载现有连接
	if val, ok := p.providers.Load(key); ok {
		conn := val.(*ProviderConn)
		conn.LastUsedAt = time.Now()
		conn.ReqCount++
		return conn.Proxy, nil
	}

	// 创建新连接
	proxy, err := NewCcProxy(provider)
	if err != nil {
		return nil, err
	}

	newConn := &ProviderConn{
		Provider:    provider,
		Proxy:       proxy,
		LastUsedAt:  time.Now(),
		ReqCount:    1,
		HealthCheck: time.Now(),
	}

	// 原子存储：如果已存在则使用现有值
	actual, _ := p.providers.LoadOrStore(key, newConn)
	conn := actual.(*ProviderConn)

	return conn.Proxy, nil
}

// Remove 移除提供商连接
func (p *ConnPool) Remove(provider *config.Provider) error {
	key := provider.Name + "|" + provider.BaseURL

	if val, ok := p.providers.LoadAndDelete(key); ok {
		conn := val.(*ProviderConn)
		if conn.Proxy != nil {
			return conn.Proxy.Close()
		}
	}

	return nil
}

// Clear 清空连接池
func (p *ConnPool) Clear() error {
	var firstErr error

	p.providers.Range(func(key, value interface{}) bool {
		conn := value.(*ProviderConn)
		if conn.Proxy != nil {
			if err := conn.Proxy.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return true
	})

	return firstErr
}

// Stats 获取连接池统计信息
func (p *ConnPool) Stats() map[string]interface{} {
	stats := make(map[string]interface{})
	var totalConns, totalReqs int

	p.providers.Range(func(key, value interface{}) bool {
		conn := value.(*ProviderConn)
		totalConns++
		totalReqs += int(conn.ReqCount)
		return true
	})

	stats["total_connections"] = totalConns
	stats["total_requests"] = totalReqs
	stats["created_at"] = p.createdAt

	return stats
}

// CleanupIdle 清理空闲超过指定时间的连接
func (p *ConnPool) CleanupIdle(maxIdle time.Duration) int {
	cleaned := 0
	cutoff := time.Now().Add(-maxIdle)

	p.providers.Range(func(key, value interface{}) bool {
		conn := value.(*ProviderConn)
		if conn.LastUsedAt.Before(cutoff) {
			if p.providers.CompareAndDelete(key, value) {
				if conn.Proxy != nil {
					conn.Proxy.Close()
				}
				cleaned++
			}
		}
		return true
	})

	return cleaned
}

// ============================================================
// 工具函数
// ============================================================

// DrainReader 读取并丢弃 reader 中的所有数据
func DrainReader(r io.Reader) {
	if r == nil {
		return
	}
	buf := make([]byte, 4096)
	for {
		_, err := r.Read(buf)
		if err != nil {
			break
		}
	}
}

// ProxyFactory 创建 CcProxy 实例的工厂函数
type ProxyFactory func(provider *config.Provider) (CcProxy, error)

var proxyFactories = make(map[string]ProxyFactory)

// RegisterProxyFactory 注册提供商类型的工厂函数
func RegisterProxyFactory(providerType string, factory ProxyFactory) {
	proxyFactories[providerType] = factory
}

// NewCcProxy 根据 Provider 配置创建对应的 CcProxy 实例
func NewCcProxy(provider *config.Provider) (CcProxy, error) {
	providerType := getProviderType(provider)

	factory, ok := proxyFactories[providerType]
	if !ok {
		return nil, &UnsupportedProviderError{ProviderType: providerType}
	}

	return factory(provider)
}

// getProviderType 根据 Provider 配置确定其类型
func getProviderType(p *config.Provider) string {
	if p.IsCopilot() {
		return "copilot"
	}
	if p.IsOpenAIFormat {
		return "openai"
	}
	return "anthropic"
}

// UnsupportedProviderError 不支持的提供商类型错误
type UnsupportedProviderError struct {
	ProviderType string
}

func (e *UnsupportedProviderError) Error() string {
	return "unsupported provider type: " + e.ProviderType
}
