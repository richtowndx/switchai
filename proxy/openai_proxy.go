package proxy

import (
	"bytes"
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"switchai/config"
	"switchai/logger"
)

func init() {
	RegisterProxyFactory("openai", NewOpenAIProxy)
}

// OpenAIProxy 实现 OpenAI API 代理
type OpenAIProxy struct {
	provider *config.Provider
	client   *http.Client
}

// NewOpenAIProxy 创建 OpenAI 代理实例
func NewOpenAIProxy(provider *config.Provider) (CcProxy, error) {
	if !provider.IsOpenAIFormat {
		return nil, fmt.Errorf("provider %s is not OpenAI format", provider.Name)
	}

	return &OpenAIProxy{
		provider: provider,
		client: &http.Client{
			Timeout: 600 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}, nil
}

// SendOpenAIFormat 发送 OpenAI 格式请求
// 优化：单次 JSON 解析完成模型映射 + stream 检查
func (p *OpenAIProxy) SendOpenAIFormat(ctx context.Context, reqHdr http.Header, reqBody []byte) *ProxyResponse {
	// 1. 一次性解析并处理：模型映射 + stream 检查
	modifiedBody, modelName, isStream := p.parseAndProcessRequest(reqBody)

	if isStream {
		resp := p.sendOpenAIStream(ctx, modifiedBody)
		resp.ModelName = modelName
		return resp
	}
	resp := p.sendOpenAINonStream(ctx, modifiedBody)
	resp.ModelName = modelName
	return resp
}

// SendAnthropicFormat 发送 Anthropic 格式请求
// 优化：转换 + 模型映射 + stream 检查一次性完成
func (p *OpenAIProxy) SendAnthropicFormat(ctx context.Context, reqHdr http.Header, reqBody []byte) *ProxyResponse {
	// 1. 转换并处理：Anthropic → OpenAI + 模型映射 + stream 检查
	modifiedBody, modelName, isStream := p.convertAndProcessAnthropicRequest(reqBody)

	if isStream {
		resp := p.sendOpenAIStream(ctx, modifiedBody)
		resp.ModelName = modelName
		return resp
	}
	resp := p.sendOpenAINonStream(ctx, modifiedBody)
	resp.ModelName = modelName
	return resp
}

// Close 释放资源
func (p *OpenAIProxy) Close() error {
	p.client.CloseIdleConnections()
	return nil
}

// Provider 返回关联的 Provider
func (p *OpenAIProxy) Provider() *config.Provider {
	return p.provider
}

// ============================================================
// 优化后的内部方法
// ============================================================

// parseAndProcessRequest 一次性解析并处理：模型映射 + stream 检查
func (p *OpenAIProxy) parseAndProcessRequest(reqBody []byte) ([]byte, string, bool) {
	var req map[string]interface{}
	if err := json.Unmarshal(reqBody, &req); err != nil {
		return reqBody, "", false
	}

	modelName := ""
	isStream := false

	// 处理模型映射
	if model, ok := req["model"].(string); ok {
		modelName = model
		resolved := p.provider.ResolveModel(model)
		if resolved != model {
			logger.Info("Model resolution: %s → %s (provider: %s)", model, resolved, p.provider.Name)
			req["model"] = resolved
			modelName = resolved
		}
	}

	// 检查 stream
	if v, ok := req["stream"].(bool); ok {
		isStream = v
	}

	// 一次性序列化
	result, _ := json.Marshal(req)
	return result, modelName, isStream
}

// convertAndProcessAnthropicRequest 转换 Anthropic → OpenAI + 模型映射 + stream 检查
func (p *OpenAIProxy) convertAndProcessAnthropicRequest(anthropicReq []byte) ([]byte, string, bool) {
	var req map[string]interface{}
	if err := json.Unmarshal(anthropicReq, &req); err != nil {
		return nil, "", false
	}

	modelName := ""
	if model, ok := req["model"].(string); ok {
		modelName = model
	}

	// 构建 OpenAI 格式
	openaiReq := map[string]interface{}{
		"model":      req["model"],
		"max_tokens": req["max_tokens"],
		"messages":   convertAnthropicMessagesToOpenAI(req["messages"], nil),
	}

	if v, ok := req["temperature"].(float64); ok && v > 0 {
		openaiReq["temperature"] = v
	}
	if v, ok := req["top_p"].(float64); ok && v > 0 {
		openaiReq["top_p"] = v
	}
	if v, ok := req["stream"].(bool); ok {
		openaiReq["stream"] = v
	}

	if tools, ok := req["tools"].([]interface{}); ok {
		openaiReq["tools"] = convertAnthropicToolsToOpenAI(tools)
		openaiReq["tool_choice"] = "auto"
	}

	// 处理模型映射
	if model, ok := openaiReq["model"].(string); ok {
		resolved := p.provider.ResolveModel(model)
		if resolved != model {
			logger.Info("Model resolution: %s → %s (provider: %s)", model, resolved, p.provider.Name)
			openaiReq["model"] = resolved
			modelName = resolved
		}
	}

	// 检查 stream
	isStream := false
	if v, ok := openaiReq["stream"].(bool); ok {
		isStream = v
	}

	// 一次性序列化
	result, _ := json.Marshal(openaiReq)
	return result, modelName, isStream
}

func (p *OpenAIProxy) sendOpenAINonStream(ctx context.Context, reqBody []byte) *ProxyResponse {
	baseURL := p.buildURL()
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL, bytes.NewReader(reqBody))
	if err != nil {
		return &ProxyResponse{Error: fmt.Errorf("create request: %w", err)}
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.provider.APIKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return &ProxyResponse{Error: fmt.Errorf("send request: %w", err)}
	}
	defer resp.Body.Close()

	respBytes, err := readResponseBody(resp)
	if err != nil {
		return &ProxyResponse{Error: err}
	}

	if resp.StatusCode != http.StatusOK {
		return &ProxyResponse{Error: fmt.Errorf("api error: status %d, body: %s", resp.StatusCode, string(respBytes))}
	}

	return &ProxyResponse{Body: respBytes, IsStream: false}
}

func (p *OpenAIProxy) sendOpenAIStream(ctx context.Context, reqBody []byte) *ProxyResponse {
	ch := make(chan string, 16)
	errCh := make(chan error, 1)

	go func() {
		defer close(ch)
		defer close(errCh)

		baseURL := p.buildURL()
		req, err := http.NewRequestWithContext(ctx, "POST", baseURL, bytes.NewReader(reqBody))
		if err != nil {
			errCh <- fmt.Errorf("create request: %w", err)
			return
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+p.provider.APIKey)
		req.Header.Set("Accept", "text/event-stream")

		resp, err := p.client.Do(req)
		if err != nil {
			errCh <- fmt.Errorf("send request: %w", err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			errCh <- fmt.Errorf("api error: status %d, body: %s", resp.StatusCode, string(body))
			return
		}

		scanner := bufio.NewScanner(resp.Body)
		// 使用 buffer pool 减少 memory allocation
		buf := scannerBufferPool.Get().([]byte)
		defer func() { scannerBufferPool.Put(buf) }()
		
		scanner.Buffer(buf, 1024*1024)

		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				ch <- "\n"
			} else {
				ch <- line + "\n"
			}
		}

		if err := scanner.Err(); err != nil {
			errCh <- fmt.Errorf("scan stream: %w", err)
		}
	}()

	return &ProxyResponse{StreamCh: ch, ErrCh: errCh, IsStream: true}
}

func (p *OpenAIProxy) buildURL() string {
	baseURL := p.provider.BaseURL
	if !strings.HasSuffix(baseURL, "/chat/completions") {
		if !strings.HasSuffix(baseURL, "/v1") && !strings.HasSuffix(baseURL, "/v1/") {
			if !strings.HasSuffix(baseURL, "/") {
				baseURL += "/"
			}
			baseURL += "v1"
		}
		baseURL += "/chat/completions"
	}
	return baseURL
}
