package proxy

import (
	"bytes"
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
func (p *CopilotProxy) SendOpenAIFormat(ctx context.Context, reqHdr http.Header, reqBody []byte) *ProxyResponse {
	// 1. 过滤不支持的参数
	reqBody = FilterUnsupportedParams(reqBody, "copilot")

	// 2. 处理模型映射并获取映射后的模型名
	modifiedBody, modelName := p.applyModelMapping(reqBody)

	// 2. 解析请求检查是否为流式
	var req map[string]interface{}
	_ = json.Unmarshal(modifiedBody, &req)
	isStream, _ := req["stream"].(bool)

	if isStream {
		resp := p.sendCopilotStream(ctx, modifiedBody)
		resp.ModelName = modelName
		return resp
	}
	resp := p.sendCopilotNonStream(ctx, modifiedBody)
	resp.ModelName = modelName
	return resp
}

// SendAnthropicFormat Copilot 需要转换 Anthropic 格式
func (p *CopilotProxy) SendAnthropicFormat(ctx context.Context, reqHdr http.Header, reqBody []byte) *ProxyResponse {
	// 1. 转换 Anthropic -> OpenAI
	openaiBody, modelName, err := p.convertAnthropicToOpenAI(reqBody)
	if err != nil {
		return &ProxyResponse{Error: err}
	}

	logger.Info("Converted Anthropic->OpenAI body: %s", string(openaiBody))

	// 2. 过滤不支持的参数
	openaiBody = FilterUnsupportedParams(openaiBody, "copilot")

	// 3. 处理模型映射并获取映射后的模型名
	modifiedBody, mappedName := p.applyModelMapping(openaiBody)
	if mappedName != "" {
		modelName = mappedName
	}

	// 4. 解析请求检查是否为流式
	var req map[string]interface{}
	_ = json.Unmarshal(modifiedBody, &req)
	isStream, _ := req["stream"].(bool)

	if isStream {
		resp := p.sendCopilotStream(ctx, modifiedBody)
		resp.ModelName = modelName
		resp.ConvertResponseFormat = "anthropic"
		return resp
	}
	resp := p.sendCopilotNonStream(ctx, modifiedBody)
	resp.ModelName = modelName
		resp.ConvertResponseFormat = "anthropic"
	return resp
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

// applyModelMapping 处理模型映射，返回映射后的请求体和模型名
func (p *CopilotProxy) applyModelMapping(reqBody []byte) ([]byte, string) {
	var req map[string]interface{}
	if err := json.Unmarshal(reqBody, &req); err != nil {
		return reqBody, ""
	}

	modelName := ""
	// 处理模型映射
	if model, ok := req["model"].(string); ok {
		modelName = model
		resolved := p.provider.ResolveModel(model)
		if resolved != model {
			logger.Info("Model resolution: %s → %s (provider: %s)", model, resolved, p.provider.Name)
			req["model"] = resolved
			modelName = resolved
		}

		// 处理 Copilot codex 模型回退（codex 模型不支持 /chat/completions）
		resolved = ResolveCopilotModel(modelName)
		if resolved != modelName {
			logger.Info("Copilot codex model fallback: %s → %s", modelName, resolved)
			req["model"] = resolved
			modelName = resolved
		}
	}

	result, _ := json.Marshal(req)
	return result, modelName
}

func (p *CopilotProxy) sendCopilotNonStream(ctx context.Context, reqBody []byte) *ProxyResponse {
	// 获取 Copilot token
	copilotToken := RefreshCopilotToken(p.provider)
	if copilotToken == "" {
		logger.Error("Failed to get Copilot token for provider %s", p.provider.Name)
		return &ProxyResponse{Error: fmt.Errorf("failed to get Copilot token")}
	}
	logger.Info("Using Copilot token (length: %d, prefix: %s)", len(copilotToken), copilotToken[:10])

	baseURL := p.buildURL()
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL, bytes.NewReader(reqBody))
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

	return &ProxyResponse{Body: respBytes, IsStream: false}
}

func (p *CopilotProxy) sendCopilotStream(ctx context.Context, reqBody []byte) *ProxyResponse {
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
		req, err := http.NewRequestWithContext(ctx, "POST", baseURL, bytes.NewReader(reqBody))
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

func (p *CopilotProxy) buildURL() string {
	return CopilotTargetURL(p.provider)
}

// ============================================================
// 格式转换
// ============================================================

func (p *CopilotProxy) convertAnthropicToOpenAI(anthropicReq []byte) ([]byte, string, error) {
	var req map[string]interface{}
	if err := json.Unmarshal(anthropicReq, &req); err != nil {
		return nil, "", err
	}

	modelName := ""
	if model, ok := req["model"].(string); ok {
		modelName = model
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
	return data, modelName, nil
}
