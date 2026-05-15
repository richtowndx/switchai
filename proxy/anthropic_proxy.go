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
	"switchai/history"
	"switchai/logger"
	"switchai/stats"

	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
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
	// 1. 过滤不支持的参数（OpenAI 格式可能包含 Anthropic 不支持的参数）
	reqBody = FilterUnsupportedParams(reqBody, "anthropic")

	// 2. 转换并处理：OpenAI → Anthropic + 模型映射 + stream 检查
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

// HandleAnthropicFormat 处理 Anthropic 格式请求（包括发送和响应转发）
func (p *AnthropicProxy) HandleAnthropicFormat(ctx context.Context, c *gin.Context, reqHdr http.Header, reqBody []byte) error {
	startTime := time.Now()
	var firstTokenTime time.Time

	// 生成请求ID
	requestID := c.GetHeader("X-Request-ID")
	if requestID == "" {
		requestID = c.GetString("request_id")
	}

	// 获取请求信息
	method := c.Request.Method
	path := c.Request.URL.Path

	// 1. 一次性解析并处理：模型映射 + stream 检查
	modifiedBody, modelName, isStream := p.parseAndProcessRequest(reqBody)

	logger.Info("[AnthropicProxy] Handling %s Anthropic request: model=%s", map[bool]string{true: "stream", false: "non-stream"}[isStream], modelName)

	if isStream {
		return p.handleAnthropicStreamingResponse(ctx, c, reqHdr, modifiedBody, modelName, startTime, &firstTokenTime, requestID, method, path, string(reqBody))
	}

	return p.handleAnthropicNonStreamingResponse(ctx, c, modifiedBody, modelName, startTime, requestID, method, path, string(reqBody))
}

// HandleOpenAIFormat 处理 OpenAI 格式请求（包括发送和响应转发）
func (p *AnthropicProxy) HandleOpenAIFormat(ctx context.Context, c *gin.Context, reqHdr http.Header, reqBody []byte) error {
	startTime := time.Now()
	var firstTokenTime time.Time

	// 生成请求ID
	requestID := c.GetHeader("X-Request-ID")
	if requestID == "" {
		requestID = c.GetString("request_id")
	}

	// 获取请求信息
	method := c.Request.Method
	path := c.Request.URL.Path

	// 1. 过滤不支持的参数
	reqBody = FilterUnsupportedParams(reqBody, "anthropic")

	// 2. 转换并处理：OpenAI → Anthropic + 模型映射 + stream 检查
	modifiedBody, modelName, isStream := p.convertAndProcessOpenAIRequest(reqBody)

	logger.Info("[AnthropicProxy] Handling %s OpenAI request: model=%s", map[bool]string{true: "stream", false: "non-stream"}[isStream], modelName)

	if isStream {
		return p.handleAnthropicStreamingResponse(ctx, c, reqHdr, modifiedBody, modelName, startTime, &firstTokenTime, requestID, method, path, string(reqBody))
	}

	return p.handleAnthropicNonStreamingResponse(ctx, c, modifiedBody, modelName, startTime, requestID, method, path, string(reqBody))
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

		// 处理 Content-Encoding: gzip/brotli
		var reader io.Reader = resp.Body
		contentEncoding := resp.Header.Get("Content-Encoding")
		if contentEncoding != "" && contentEncoding != "identity" {
			switch strings.ToLower(contentEncoding) {
			case "gzip":
				gzReader, err := gzip.NewReader(resp.Body)
				if err != nil {
					errCh <- fmt.Errorf("gzip decompress: %w", err)
					return
				}
				defer gzReader.Close()
				reader = gzReader
			case "br":
				reader = brotli.NewReader(resp.Body)
			}
		}

		scanner := bufio.NewScanner(reader)
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

// handleAnthropicNonStreamingResponse 处理非流式响应
func (p *AnthropicProxy) handleAnthropicNonStreamingResponse(ctx context.Context, c *gin.Context, reqBody []byte, modelName string, startTime time.Time, requestID, method, path, requestBody string) error {
	baseURL := p.buildURL()
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", p.provider.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := readResponseBody(resp)
	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("api error: status %d, body: %s", resp.StatusCode, string(respBytes))
	}

	// 解析 token 统计
	_, inputTokens, outputTokens := parseTokenStats(respBytes)

	// 设置响应头并返回
	c.Header("Content-Type", "application/json")
	c.Data(200, "application/json", respBytes)

	// 记录统计
	duration := time.Since(startTime).Milliseconds()
	cost := calculateCost(modelName, inputTokens, outputTokens)

	keyID, clientIP := GetAuthInfo(c)
	if clientIP == "" {
		clientIP = c.ClientIP()
	}

	stats.RecordUsage(p.provider.ID, p.provider.Name, modelName, "non-stream", "ccproxy",
		inputTokens, outputTokens, cost, duration, 0, keyID, clientIP)

	// 记录 history
	history.AddRecord(history.RequestRecord{
		ID:           requestID,
		Timestamp:    startTime,
		Method:       method,
		Path:         path,
		ClientIP:     clientIP,
		KeyID:        keyID,
		Provider:     p.provider.Name,
		Model:        modelName,
		StatusCode:   resp.StatusCode,
		Duration:     duration,
		RequestBody:  requestBody,
		ResponseBody: string(respBytes),
		RequestHeaders: c.Request.Header,
		ResponseHeaders: resp.Header,
		RequestSize:  int64(len(requestBody)),
		ResponseSize: int64(len(respBytes)),
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  inputTokens + outputTokens,
		Cost:         cost,
	})

	logger.Info("✅ Anthropic non-stream 完成: Model=%s, Tokens=%d+%d, Duration=%dms", modelName, inputTokens, outputTokens, duration)

	return nil
}

// handleAnthropicStreamingResponse 处理流式响应
func (p *AnthropicProxy) handleAnthropicStreamingResponse(ctx context.Context, c *gin.Context, reqHdr http.Header, reqBody []byte, modelName string, startTime time.Time, firstTokenTime *time.Time, requestID, method, path, requestBody string) error {
	baseURL := p.buildURL()
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
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
	req.Header.Set("Accept", "text/event-stream")

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("api error: status %d, body: %s", resp.StatusCode, string(body))
	}

	// 设置流式响应头
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming not supported")
	}

	// 处理 Content-Encoding: gzip/brotli
	var reader io.Reader = resp.Body
	contentEncoding := resp.Header.Get("Content-Encoding")
	if contentEncoding != "" && contentEncoding != "identity" {
		switch strings.ToLower(contentEncoding) {
		case "gzip":
			gzReader, err := gzip.NewReader(resp.Body)
			if err != nil {
				return fmt.Errorf("gzip decompress: %w", err)
			}
			defer gzReader.Close()
			reader = gzReader
		case "br":
			reader = brotli.NewReader(resp.Body)
		}
	}

	// 流式转发
	var inputTokens, outputTokens int
	var responseBody strings.Builder
	scanner := bufio.NewScanner(reader)
	buf := scannerBufferPool.Get().([]byte)
	defer func() { scannerBufferPool.Put(buf) }()
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if firstTokenTime != nil && firstTokenTime.IsZero() {
			*firstTokenTime = time.Now()
		}

		c.Writer.WriteString(line)
		if line != "" {
			c.Writer.WriteString("\n")
		}
		flusher.Flush()

		// 收集响应体（限制大小）
		if responseBody.Len() < 100*1024 { // 限制 100KB
			responseBody.WriteString(line)
			if line != "" {
				responseBody.WriteString("\n")
			}
		}

		// 提取 token 统计
		inputTokens, outputTokens = extractTokensFromSSE(line, inputTokens, outputTokens)
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan stream: %w", err)
	}

	// 记录统计
	duration := time.Since(startTime).Milliseconds()
	var timeToFirst int64
	if firstTokenTime != nil && !firstTokenTime.IsZero() {
		timeToFirst = firstTokenTime.Sub(startTime).Milliseconds()
	}
	cost := calculateCost(modelName, inputTokens, outputTokens)

	keyID, clientIP := GetAuthInfo(c)
	if clientIP == "" {
		clientIP = c.ClientIP()
	}

	stats.RecordUsage(p.provider.ID, p.provider.Name, modelName, "stream", "ccproxy",
		inputTokens, outputTokens, cost, duration, timeToFirst, keyID, clientIP)

	// 记录 history
	responseBodyStr := responseBody.String()
	if responseBody.Len() >= 100*1024 {
		responseBodyStr += "\n... (truncated)"
	}

	history.AddRecord(history.RequestRecord{
		ID:           requestID,
		Timestamp:    startTime,
		Method:       method,
		Path:         path,
		ClientIP:     clientIP,
		KeyID:        keyID,
		Provider:     p.provider.Name,
		Model:        modelName,
		StatusCode:   resp.StatusCode,
		Duration:     duration,
		RequestBody:  requestBody,
		ResponseBody: responseBodyStr,
		RequestHeaders: c.Request.Header,
		ResponseHeaders: resp.Header,
		RequestSize:  int64(len(requestBody)),
		ResponseSize: int64(responseBody.Len()),
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  inputTokens + outputTokens,
		Cost:         cost,
	})

	logger.Info("✅ Anthropic stream 完成: Model=%s, Tokens=%d+%d, Duration=%dms, TTFB=%dms",
		modelName, inputTokens, outputTokens, duration, timeToFirst)

	return nil
}
