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
	RegisterProxyFactory("copilot", NewCopilotProxy)
}

// CopilotProxy 实现 Copilot API 代理
type CopilotProxy struct {
	provider *config.Provider
	client   *http.Client
}

// NewCopilotProxy 创建 Copilot 代理实例
func NewCopilotProxy(provider *config.Provider) (CcProxy, error) {
	if !provider.IsCopilot() {
		return nil, fmt.Errorf("provider %s is not Copilot format", provider.Name)
	}

	return &CopilotProxy{
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
func (p *CopilotProxy) SendOpenAIFormat(ctx context.Context, reqBody string) *ProxyResponse {
	// 1. 处理模型映射
	modifiedBody := p.applyModelMapping(reqBody)

	// 2. 解析请求检查是否为流式
	var req map[string]interface{}
	_ = json.Unmarshal([]byte(modifiedBody), &req)
	isStream, _ := req["stream"].(bool)

	if isStream {
		return p.sendCopilotStream(ctx, modifiedBody)
	}
	return p.sendCopilotNonStream(ctx, modifiedBody)
}

// SendAnthropicFormat Copilot 需要转换 Anthropic 格式
func (p *CopilotProxy) SendAnthropicFormat(ctx context.Context, reqBody string) *ProxyResponse {
	// 1. 转换 Anthropic -> OpenAI
	openaiBody, err := p.convertAnthropicToOpenAI(reqBody)
	if err != nil {
		return &ProxyResponse{Error: err}
	}

	// 2. 调用 OpenAI 格式方法（内部会处理模型映射）
	return p.SendOpenAIFormat(ctx, openaiBody)
}

// Close 释放资源
func (p *CopilotProxy) Close() error {
	p.client.CloseIdleConnections()
	return nil
}

// Provider 返回关联的 Provider
func (p *CopilotProxy) Provider() *config.Provider {
	return p.provider
}

// ============================================================
// 内部方法
// ============================================================

// applyModelMapping 处理模型映射
func (p *CopilotProxy) applyModelMapping(reqBody string) string {
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

func (p *CopilotProxy) sendCopilotNonStream(ctx context.Context, reqBody string) *ProxyResponse {
	// 获取 Copilot token
	copilotToken := RefreshCopilotToken(p.provider)
	if copilotToken == "" {
		return &ProxyResponse{Error: fmt.Errorf("failed to get Copilot token")}
	}

	baseURL := p.buildURL()
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL, strings.NewReader(reqBody))
	if err != nil {
		return &ProxyResponse{Error: fmt.Errorf("create request: %w", err)}
	}

	// 注入 Copilot headers
	for k, v := range InjectCopilotHeaders(nil, copilotToken) {
		for _, val := range v {
			req.Header.Add(k, val)
		}
	}
	req.Header.Set("Content-Type", "application/json")

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

func (p *CopilotProxy) sendCopilotStream(ctx context.Context, reqBody string) *ProxyResponse {
	ch := make(chan string, 16)
	errCh := make(chan error, 1)

	go func() {
		defer close(ch)
		defer close(errCh)

		// 获取 Copilot token
		copilotToken := RefreshCopilotToken(p.provider)
		if copilotToken == "" {
			errCh <- fmt.Errorf("failed to get Copilot token")
			return
		}

		baseURL := p.buildURL()
		req, err := http.NewRequestWithContext(ctx, "POST", baseURL, strings.NewReader(reqBody))
		if err != nil {
			errCh <- fmt.Errorf("create request: %w", err)
			return
		}

		// 注入 Copilot headers
		for k, v := range InjectCopilotHeaders(nil, copilotToken) {
			for _, val := range v {
				req.Header.Add(k, val)
			}
		}
		req.Header.Set("Content-Type", "application/json")
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

func (p *CopilotProxy) buildURL() string {
	return CopilotTargetURL(p.provider)
}

// ============================================================
// 格式转换
// ============================================================

func (p *CopilotProxy) convertAnthropicToOpenAI(anthropicReq string) (string, error) {
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

	data, _ := json.Marshal(openaiReq)
	return string(data), nil
}
