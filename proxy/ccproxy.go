package proxy

import (
	"context"
	"net/http"
	"sync"

	"switchai/config"

	"github.com/gin-gonic/gin"
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

	// ConvertResponseFormat 响应格式转换标记
	// "anthropic" 表示需要将 OpenAI 格式响应转换为 Anthropic 格式
	// "" 或 "openai" 表示不转换
	ConvertResponseFormat string
}

// CcProxy 定义统一的代理接口
// 优化设计：Proxy 内部处理完整的请求-响应周期，包括格式转换和响应转发
type CcProxy interface {
	// HandleOpenAIFormat 处理 OpenAI 格式请求（包括发送和响应转发）
	// 内部处理：模型映射、发送请求、响应转发
	// 返回: (error, statusCode) - statusCode 为 0 表示无法从错误中提取状态码
	HandleOpenAIFormat(ctx context.Context, c *gin.Context, reqHdr http.Header, reqBody []byte) (error, int)

	// HandleAnthropicFormat 处理 Anthropic 格式请求（包括发送和响应转发）
	// 内部处理：模型映射、格式转换、发送请求、响应转换、响应转发
	// 返回: (error, statusCode) - statusCode 为 0 表示无法从错误中提取状态码
	HandleAnthropicFormat(ctx context.Context, c *gin.Context, reqHdr http.Header, reqBody []byte) (error, int)

	// HandleCodexFormat 处理 Codex /responses 格式请求
	// 内部处理：Responses API → Chat Completions 转换、发送请求、响应转换、响应转发
	// 返回: (error, statusCode)
	HandleCodexFormat(ctx context.Context, c *gin.Context, reqHdr http.Header, reqBody []byte) (error, int)

	// Close 释放代理资源
	Close() error

	// Provider 返回关联的 Provider 配置
	Provider() *config.Provider
}

// ============================================================
// 工具函数
// ============================================================

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
