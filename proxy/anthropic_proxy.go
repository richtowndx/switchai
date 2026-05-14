package proxy

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"switchai/config"
	"switchai/logger"
)

func init() {
	RegisterProxyFactory("anthropic", NewAnthropicProxy)
}

// AnthropicProxy 实现 Anthropic API 代理
type AnthropicProxy struct {
	provider *config.Provider
	client   *http.Client
}

// NewAnthropicProxy 创建 Anthropic 代理实例
func NewAnthropicProxy(provider *config.Provider) (CcProxy, error) {
	if provider.IsOpenAIFormat {
		return nil, fmt.Errorf("provider %s is not Anthropic format", provider.Name)
	}

	return &AnthropicProxy{
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

// SendAnthropicFormat 发送 Anthropic 格式请求
// 内部处理：模型映射 -> 格式判断 -> 发送请求
func (p *AnthropicProxy) SendAnthropicFormat(ctx context.Context, reqBody string) *ProxyResponse {
	// 1. 处理模型映射
	modifiedBody := p.applyModelMapping(reqBody)

	// 2. 解析请求检查是否为流式
	var req map[string]interface{}
	_ = json.Unmarshal([]byte(modifiedBody), &req)
	isStream, _ := req["stream"].(bool)

	if isStream {
		return p.sendAnthropicStream(ctx, modifiedBody)
	}
	return p.sendAnthropicNonStream(ctx, modifiedBody)
}

// SendOpenAIFormat 发送 OpenAI 格式请求
// 内部处理：格式转换 -> 模型映射 -> 发送请求
func (p *AnthropicProxy) SendOpenAIFormat(ctx context.Context, reqBody string) *ProxyResponse {
	// 1. 转换 OpenAI -> Anthropic
	anthropicBody, err := p.convertOpenAIToAnthropic(reqBody)
	if err != nil {
		return &ProxyResponse{Error: err}
	}

	// 2. 调用 Anthropic 格式方法（内部会处理模型映射）
	return p.SendAnthropicFormat(ctx, anthropicBody)
}

// Close 释放资源
func (p *AnthropicProxy) Close() error {
	p.client.CloseIdleConnections()
	return nil
}

// Provider 返回关联的 Provider
func (p *AnthropicProxy) Provider() *config.Provider {
	return p.provider
}

// ============================================================
// 内部方法
// ============================================================

// applyModelMapping 处理模型映射
func (p *AnthropicProxy) applyModelMapping(reqBody string) string {
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

func (p *AnthropicProxy) sendAnthropicNonStream(ctx context.Context, reqBody string) *ProxyResponse {
	baseURL := p.buildURL()
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL, strings.NewReader(reqBody))
	if err != nil {
		return &ProxyResponse{Error: fmt.Errorf("create request: %w", err)}
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", p.provider.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

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

func (p *AnthropicProxy) sendAnthropicStream(ctx context.Context, reqBody string) *ProxyResponse {
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
		req.Header.Set("x-api-key", p.provider.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
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

func (p *AnthropicProxy) buildURL() string {
	baseURL := p.provider.BaseURL
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	if !strings.HasSuffix(baseURL, "/v1/messages") {
		if !strings.HasSuffix(baseURL, "/") {
			baseURL += "/"
		}
		baseURL += "v1/messages"
	}
	return baseURL
}

// ============================================================
// 格式转换
// ============================================================

func (p *AnthropicProxy) convertOpenAIToAnthropic(openaiReq string) (string, error) {
	var req map[string]interface{}
	if err := json.Unmarshal([]byte(openaiReq), &req); err != nil {
		return "", err
	}

	anthropicReq := map[string]interface{}{
		"model":      req["model"],
		"max_tokens": req["max_tokens"],
		"messages":   convertOpenAIMessagesToAnthropic(req["messages"]),
	}

	if v, ok := req["temperature"].(float64); ok && v > 0 {
		anthropicReq["temperature"] = v
	}
	if v, ok := req["top_p"].(float64); ok && v > 0 {
		anthropicReq["top_p"] = v
	}
	if v, ok := req["stream"].(bool); ok {
		anthropicReq["stream"] = v
	}

	if tools, ok := req["tools"].([]interface{}); ok {
		anthropicReq["tools"] = convertOpenAIToolsToAnthropic(tools)
		anthropicReq["tool_choice"] = map[string]interface{}{"type": "auto"}
	}

	data, _ := json.Marshal(anthropicReq)
	return string(data), nil
}

// readResponseBody 读取并解压响应体
func readResponseBody(resp *http.Response) ([]byte, error) {
	contentEncoding := resp.Header.Get("Content-Encoding")
	var reader io.Reader = resp.Body

	if contentEncoding != "" && contentEncoding != "identity" {
		switch strings.ToLower(contentEncoding) {
		case "gzip":
			gzReader, err := gzip.NewReader(resp.Body)
			if err != nil {
				return nil, fmt.Errorf("gzip decompress: %w", err)
			}
			defer gzReader.Close()
			reader = gzReader
		case "br":
			reader = brotli.NewReader(resp.Body)
		}
	}

	return io.ReadAll(reader)
}
