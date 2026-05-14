package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"switchai/config"
	"switchai/logger"

	"github.com/andybalholm/brotli"
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
// 优化：单次 JSON 解析完成模型映射 + stream 检查
func (p *AnthropicProxy) SendAnthropicFormat(ctx context.Context, reqHdr http.Header, reqBody []byte) *ProxyResponse {
	// 1. 一次性解析并处理：模型映射 + stream 检查
	modifiedBody, modelName, isStream := p.parseAndProcessRequest(reqBody)

	if isStream {
		resp := p.sendAnthropicStream(ctx, reqHdr, modifiedBody)
		resp.ModelName = modelName
		return resp
	}
	resp := p.sendAnthropicNonStream(ctx, modifiedBody)
	resp.ModelName = modelName
	return resp
}

// SendOpenAIFormat 发送 OpenAI 格式请求
// 优化：转换 + 模型映射 + stream 检查一次性完成
func (p *AnthropicProxy) SendOpenAIFormat(ctx context.Context, reqHdr http.Header, reqBody []byte) *ProxyResponse {
	// 1. 转换并处理：OpenAI → Anthropic + 模型映射 + stream 检查
	modifiedBody, modelName, isStream := p.convertAndProcessOpenAIRequest(reqBody)

	if isStream {
		resp := p.sendAnthropicStream(ctx, reqHdr, modifiedBody)
		resp.ModelName = modelName
		return resp
	}
	resp := p.sendAnthropicNonStream(ctx, modifiedBody)
	resp.ModelName = modelName
	return resp
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
// 优化后的内部方法
// ============================================================

// parseAndProcessRequest 一次性解析并处理：模型映射 + stream 检查
// 优化：从原来的 2 次 JSON 解析 + 1 次序列化 → 1 次解析 + 1 次序列化
func (p *AnthropicProxy) parseAndProcessRequest(reqBody []byte) ([]byte, string, bool) {
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

// convertAndProcessOpenAIRequest 转换 OpenAI → Anthropic + 模型映射 + stream 检查
// 优化：从原来的 4 次 JSON 处理 → 2 次
func (p *AnthropicProxy) convertAndProcessOpenAIRequest(openaiReq []byte) ([]byte, string, bool) {
	var req map[string]interface{}
	if err := json.Unmarshal(openaiReq, &req); err != nil {
		return nil, "", false
	}

	modelName := ""
	if model, ok := req["model"].(string); ok {
		modelName = model
	}

	// 构建 Anthropic 格式
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

	// 处理模型映射
	if model, ok := anthropicReq["model"].(string); ok {
		resolved := p.provider.ResolveModel(model)
		if resolved != model {
			logger.Info("Model resolution: %s → %s (provider: %s)", model, resolved, p.provider.Name)
			anthropicReq["model"] = resolved
			modelName = resolved
		}
	}

	// 检查 stream
	isStream := false
	if v, ok := anthropicReq["stream"].(bool); ok {
		isStream = v
	}

	// 一次性序列化
	result, _ := json.Marshal(anthropicReq)
	return result, modelName, isStream
}

func (p *AnthropicProxy) sendAnthropicNonStream(ctx context.Context, reqBody []byte) *ProxyResponse {
	baseURL := p.buildURL()
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL, bytes.NewReader(reqBody))
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

	return &ProxyResponse{Body: respBytes, IsStream: false}
}

func (p *AnthropicProxy) sendAnthropicStream(ctx context.Context, reqHdr http.Header, reqBody []byte) *ProxyResponse {
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

		// 设置必要的请求头
		for k, v := range reqHdr {
			for _, val := range v {
				if strings.ToLower(k) == "authorization" || strings.ToLower(k) == "content-type" {
					continue
				}
				req.Header.Add(k, val)
			}
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+p.provider.APIKey)

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
