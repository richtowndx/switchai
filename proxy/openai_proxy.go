package proxy

import (
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
// 内部处理：模型映射 -> 格式判断 -> 发送请求
func (p *OpenAIProxy) SendOpenAIFormat(ctx context.Context, reqBody string) *ProxyResponse {
	// 1. 处理模型映射
	modifiedBody := p.applyModelMapping(reqBody)

	// 2. 解析请求检查是否为流式
	var req map[string]interface{}
	_ = json.Unmarshal([]byte(modifiedBody), &req)
	isStream, _ := req["stream"].(bool)

	if isStream {
		return p.sendOpenAIStream(ctx, modifiedBody)
	}
	return p.sendOpenAINonStream(ctx, modifiedBody)
}

// SendAnthropicFormat 发送 Anthropic 格式请求
// 内部处理：格式转换 -> 模型映射 -> 发送请求
func (p *OpenAIProxy) SendAnthropicFormat(ctx context.Context, reqBody string) *ProxyResponse {
	// 1. 转换 Anthropic -> OpenAI
	openaiBody, err := p.convertAnthropicToOpenAI(reqBody)
	if err != nil {
		return &ProxyResponse{Error: err}
	}

	// 2. 调用 OpenAI 格式方法（内部会处理模型映射）
	return p.SendOpenAIFormat(ctx, openaiBody)
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
// 内部方法
// ============================================================

// applyModelMapping 处理模型映射
func (p *OpenAIProxy) applyModelMapping(reqBody string) string {
	var req map[string]interface{}
	if err := json.Unmarshal([]byte(reqBody), &req); err != nil {
		return reqBody
	}

	// 处理模型映射
	if model, ok := req["model"].(string); ok {
		resolved := p.provider.ResolveModel(model)
		if resolved != model {
			logger.Info("Model resolution: %s → %s (provider: %s)", model, resolved, p.provider.Name)
			req["model"] = resolved
		}
	}

	result, _ := json.Marshal(req)
	return string(result)
}

func (p *OpenAIProxy) sendOpenAINonStream(ctx context.Context, reqBody string) *ProxyResponse {
	baseURL := p.buildURL()
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL, strings.NewReader(reqBody))
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

	return &ProxyResponse{Body: string(respBytes), IsStream: false}
}

func (p *OpenAIProxy) sendOpenAIStream(ctx context.Context, reqBody string) *ProxyResponse {
	ch := make(chan string, 16)
	errCh := make(chan error, 1)

	go func() {
		defer close(ch)
		defer close(errCh)

		baseURL := p.buildURL()
		req, err := http.NewRequestWithContext(ctx, "POST", baseURL, strings.NewReader(reqBody))
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
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)

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

// ============================================================
// 格式转换
// ============================================================

func (p *OpenAIProxy) convertAnthropicToOpenAI(anthropicReq string) (string, error) {
	var req map[string]interface{}
	if err := json.Unmarshal([]byte(anthropicReq), &req); err != nil {
		return "", err
	}

	openaiReq := map[string]interface{}{
		"model":      req["model"],
		"max_tokens": req["max_tokens"],
		"messages":   convertAnthropicMessagesToOpenAI(req["messages"], req["system"]),
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

	data, _ := json.Marshal(openaiReq)
	return string(data), nil
}
