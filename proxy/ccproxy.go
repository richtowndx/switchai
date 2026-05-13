package proxy

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"switchai/config"
)

// CcProxy 定义统一的代理接口，支持 Anthropic 和 OpenAI 格式的提供商
// 端口自 cc-switch: CcProxy trait (claude.rs, openai.rs)
type CcProxy interface {
	// SendMessage 发送非流式消息请求
	SendMessage(ctx context.Context, req *UnifiedRequest) (*UnifiedResponse, error)

	// SendMessageStream 发送流式消息请求，返回响应通道
	SendMessageStream(ctx context.Context, req *UnifiedRequest) (<-chan *StreamChunk, <-chan error)

	// Close 释放代理资源（HTTP 连接池等）
	Close() error

	// Provider 返回关联的 Provider 配置
	Provider() *config.Provider
}

// StreamChunk 表示流式响应的一个数据块
type StreamChunk struct {
	// Type 表示 chunk 类型
	Type ChunkType
	// Delta 内容增量
	Delta string
	// ToolUse 工具调用信息（流式开始时）
	ToolUse *ToolUse
	// ToolResult 工具执行结果（流式过程中）
	ToolResult *ToolResult
	// Usage token 使用统计（流式结束时）
	Usage *Usage
	// Error 错误信息
	Error error
	// Done 是否为最后一个 chunk
	Done bool
}

// ChunkType chunk 类型
type ChunkType int

const (
	ChunkTypeContent      ChunkType = iota // 文本内容
	ChunkTypeToolUseStart                  // 工具调用开始
	ChunkTypeToolUseDelta                  // 工具调用参数增量
	ChunkTypeToolResult                    // 工具执行结果
	ChunkTypeUsage                         // token 使用统计
	ChunkTypeError                         // 错误
)

// UnifiedRequest 统一的请求格式，内部使用
// 端口自 cc-switch: RequestBody (claude.rs, openai.rs)
type UnifiedRequest struct {
	// Model 模型名称
	Model string
	// MaxTokens 最大生成 tokens
	MaxTokens int
	// Messages 消息列表
	Messages []Message
	// System 系统提示词
	System string
	// Temperature 温度参数
	Temperature float64
	// TopP top-p 采样参数
	TopP float64
	// Tools 工具列表
	Tools []Tool
	// ToolChoice 工具选择策略
	ToolChoice any
	// Stream 是否流式响应
	Stream bool
	// Metadata 请求元数据
	Metadata map[string]string
}

// Message 消息
type Message struct {
	Role    string
	Content string
	// ToolUse 工具调用（仅 role=assistant 时）
	ToolUse *ToolUse
	// ToolResult 工具执行结果（仅 role=user 时）
	ToolResult *ToolResult
	// ContentBlocks 多模态内容块（图片等）
	ContentBlocks []ContentBlock
}

// ContentBlock 内容块（支持多模态）
type ContentBlock struct {
	Type     string // "text", "image"
	Text     string
	Source   *ImageSource
	MimeType string
}

// ImageSource 图片源
type ImageSource struct {
	Type      string // "base64", "url"
	MediaType string // "image/jpeg", "image/png" 等
	Data      string // base64 编码或 URL
}

// Tool 工具定义
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]interface{}
}

// ToolUse 工具调用
type ToolUse struct {
	ID       string
	Name     string
	Input    map[string]interface{}
	Usage    *Usage
	Metadata map[string]string
}

// ToolResult 工具执行结果
type ToolResult struct {
	ToolUseID string
	Content   string
	IsError   bool
}

// Usage token 使用统计
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// UnifiedResponse 统一的响应格式
type UnifiedResponse struct {
	// ID 响应 ID
	ID string
	// Model 模型名称
	Model string
	// Role 角色（固定为 "assistant"）
	Role string
	// Content 响应内容
	Content string
	// ContentBlocks 内容块列表（多模态）
	ContentBlocks []ContentBlock
	// ToolUse 工具调用
	ToolUse *ToolUse
	// StopReason 停止原因
	StopReason string
	// Usage token 使用统计
	Usage *Usage
	// Metadata 响应元数据
	Metadata map[string]string
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
	// 确定提供商类型
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

// ============================================================
// HTTP 连接池管理（并发安全）
// ============================================================

// ConnPool HTTP 连接池，用于复用 TCP 连接
// 使用 sync.Map 确保并发安全
type ConnPool struct {
	providers sync.Map
	createdAt time.Time
}

// ProviderConn 单个提供商的连接
type ProviderConn struct {
	Provider   *config.Provider
	Proxy      CcProxy
	LastUsedAt time.Time
	ReqCount   int64
	HealthCheck time.Time // 最后健康检查时间
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
		// 更新使用时间和计数
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

// Remove 移除提供商连接（用于故障切换）
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
				// 安全关闭 Proxy（可能为 nil）
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

// HealthCheck 检查所有连接的健康状态
func (p *ConnPool) HealthCheck() []string {
	var issues []string

	p.providers.Range(func(key, value interface{}) bool {
		conn := value.(*ProviderConn)

		// 检查空闲时间过长
		idleTime := time.Since(conn.LastUsedAt)
		if idleTime > 10*time.Minute {
			issues = append(issues, fmt.Sprintf("连接 %s 空闲过久: %v",
				conn.Provider.Name, idleTime))
		}

		// 检查请求计数异常
		if conn.ReqCount > 10000 {
			issues = append(issues, fmt.Sprintf("连接 %s 请求计数过高: %d",
				conn.Provider.Name, conn.ReqCount))
		}

		return true
	})

	return issues
}

// ============================================================
// 流式响应缓存
// ============================================================

// StreamCache 流式响应缓存，用于格式转换
type StreamCache struct {
	chunks []*StreamChunk
	done   bool
}

// NewStreamCache 创建流式缓存
func NewStreamCache() *StreamCache {
	return &StreamCache{
		chunks: make([]*StreamChunk, 0, 32),
	}
}

// Append 添加 chunk
func (c *StreamCache) Append(chunk *StreamChunk) {
	c.chunks = append(c.chunks, chunk)
	if chunk.Done {
		c.done = true
	}
}

// IsDone 是否完成
func (c *StreamCache) IsDone() bool {
	return c.done
}

// ToNonStream 转换为非流式响应
func (c *StreamCache) ToNonStream() *UnifiedResponse {
	if len(c.chunks) == 0 {
		return nil
	}

	resp := &UnifiedResponse{
		ContentBlocks: make([]ContentBlock, 0),
	}

	var contentBuilder string
	var toolUse *ToolUse
	var usage *Usage

	for _, chunk := range c.chunks {
		switch chunk.Type {
		case ChunkTypeContent:
			contentBuilder += chunk.Delta
		case ChunkTypeToolUseStart:
			toolUse = chunk.ToolUse
		case ChunkTypeToolUseDelta:
			if toolUse != nil && toolUse.Input == nil {
				toolUse.Input = make(map[string]interface{})
			}
		case ChunkTypeToolResult:
			// 工具结果不计入响应内容
		case ChunkTypeUsage:
			usage = chunk.Usage
		}
	}

	resp.Content = contentBuilder
	resp.ToolUse = toolUse
	resp.Usage = usage

	return resp
}

// Replay 重新播放缓存的 chunks
func (c *StreamCache) Replay() <-chan *StreamChunk {
	ch := make(chan *StreamChunk, len(c.chunks))
	go func() {
		for _, chunk := range c.chunks {
			ch <- chunk
		}
		close(ch)
	}()
	return ch
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
