package proxy

import (
	"context"
	"encoding/json"
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

	// Close 释放代理资源
	Close() error

	// Provider 返回关联的 Provider 配置
	Provider() *config.Provider
}

// ============================================================
// 工具函数
// ============================================================

// 不被支持的参数列表（按提供商类型分类）
// 这些参数在转换时需要被移除，因为上游 API 不支持
var unsupportedParams = map[string][]string{
	"anthropic": {
		"structured_outputs",  // OpenAI 结构化输出，Anthropic 不支持
		"parallel_tool_calls", // OpenAI 特定参数
	},
	"copilot": {
		"structured_outputs",  // Copilot 不支持结构化输出
		"parallel_tool_calls", // Copilot 不支持并行工具调用
	},
	"openai": {
		// OpenAI 通常支持所有参数，留空
	},
}

// FilterUnsupportedParams 过滤掉请求中不被上游支持的参数
// 返回过滤后的请求体
func FilterUnsupportedParams(reqBody []byte, providerType string) []byte {
	toRemove, exists := unsupportedParams[providerType]
	if !exists || len(toRemove) == 0 {
		return reqBody
	}

	var req map[string]interface{}
	if err := json.Unmarshal(reqBody, &req); err != nil {
		return reqBody
	}

	// 移除不支持的参数
	for _, key := range toRemove {
		delete(req, key)
	}

	result, err := json.Marshal(req)
	if err != nil {
		return reqBody
	}
	return result
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
