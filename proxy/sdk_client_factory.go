package proxy

import (
	"net/http"
	"net/url"
	"sync"

	"switchai/config"

	"github.com/anthropics/anthropic-sdk-go"
	anthropicoption "github.com/anthropics/anthropic-sdk-go/option"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

// SDKClientFactory 管理和缓存 SDK Client 实例
// 每个 Provider 配置对应一个 SDK Client，实现 TCP 连接复用
type SDKClientFactory struct {
	anthropicClients map[string]*anthropic.Client
	openaiClients    map[string]*openai.Client
	mu               sync.RWMutex
}

// 全局单例工厂实例
var globalSDKFactory = &SDKClientFactory{
	anthropicClients: make(map[string]*anthropic.Client),
	openaiClients:    make(map[string]*openai.Client),
}

// GetAnthropicClient 获取或创建 Anthropic SDK Client
// Client 缓存键为 Provider Name + BaseURL，确保相同配置复用同一连接
func (f *SDKClientFactory) GetAnthropicClient(provider *config.Provider) (*anthropic.Client, error) {
	key := provider.Name + "|" + provider.BaseURL

	// 先读锁检查是否存在
	f.mu.RLock()
	client, exists := f.anthropicClients[key]
	f.mu.RUnlock()

	if exists {
		return client, nil
	}

	// 不存在则创建
	f.mu.Lock()
	defer f.mu.Unlock()

	// 双重检查
	if client, exists := f.anthropicClients[key]; exists {
		return client, nil
	}

	// 构建选项
	opts := []anthropicoption.RequestOption{
		anthropicoption.WithAPIKey(provider.APIKey),
	}

	// 自定义 BaseURL（如果与默认不同）
	if provider.BaseURL != "" && provider.BaseURL != "https://api.anthropic.com" {
		opts = append(opts, anthropicoption.WithBaseURL(provider.BaseURL))
	}

	// 配置 HTTP Transport（代理支持）
	if provider.ProxyURL != "" {
		proxyURL, err := url.Parse(provider.ProxyURL)
		if err != nil {
			return nil, err
		}
		customTransport := &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		}
		opts = append(opts, anthropicoption.WithHTTPClient(&http.Client{Transport: customTransport}))
	}

	// 创建 Client
	newClient := anthropic.NewClient(opts...)
	client = &newClient

	// 缓存 Client
	f.anthropicClients[key] = client

	return client, nil
}

// GetOpenAIClient 获取或创建 OpenAI SDK Client
// Client 缓存键为 Provider Name + BaseURL，确保相同配置复用同一连接
func (f *SDKClientFactory) GetOpenAIClient(provider *config.Provider) (*openai.Client, error) {
	key := provider.Name + "|" + provider.BaseURL

	// 先读锁检查是否存在
	f.mu.RLock()
	client, exists := f.openaiClients[key]
	f.mu.RUnlock()

	if exists {
		return client, nil
	}

	// 不存在则创建
	f.mu.Lock()
	defer f.mu.Unlock()

	// 双重检查
	if client, exists := f.openaiClients[key]; exists {
		return client, nil
	}

	// 构建选项
	opts := []option.RequestOption{
		option.WithAPIKey(provider.APIKey),
	}

	// 自定义 BaseURL
	if provider.BaseURL != "" && provider.BaseURL != "https://api.openai.com/v1" {
		opts = append(opts, option.WithBaseURL(provider.BaseURL))
	}

	// 配置 HTTP Transport（代理支持）
	if provider.ProxyURL != "" {
		proxyURL, err := url.Parse(provider.ProxyURL)
		if err != nil {
			return nil, err
		}
		customTransport := &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		}
		opts = append(opts, option.WithHTTPClient(&http.Client{Transport: customTransport}))
	}

	// 创建 Client
	newClient := openai.NewClient(opts...)
	client = &newClient

	// 缓存 Client
	f.openaiClients[key] = client

	return client, nil
}

// Close 关闭所有缓存的 Client 并清空缓存
func (f *SDKClientFactory) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	// 清空缓存
	f.anthropicClients = make(map[string]*anthropic.Client)
	f.openaiClients = make(map[string]*openai.Client)

	return nil
}

// GetGlobalSDKFactory 获取全局 SDK Client 工厂单例
func GetGlobalSDKFactory() *SDKClientFactory {
	return globalSDKFactory
}
