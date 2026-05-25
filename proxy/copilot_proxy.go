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

// HandleOpenAIFormat 处理 OpenAI 格式请求（包括发送和响应转发）
// 返回: (error, statusCode)
func (p *CopilotProxy) HandleOpenAIFormat(ctx context.Context, c *gin.Context, reqHdr http.Header, reqBody []byte) (error, int) {
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
	reqBody = FilterUnsupportedParams(reqBody, "copilot")

	// 2. 处理模型映射并获取映射后的模型名
	modifiedBody, modelName := p.applyModelMapping(reqBody)

	// 3. 解析请求检查是否为流式
	var req map[string]interface{}
	_ = json.Unmarshal(modifiedBody, &req)
	isStream, _ := req["stream"].(bool)

	if isStream {
		err := p.handleCopilotStreamingResponse(ctx, c, modifiedBody, modelName, startTime, &firstTokenTime, requestID, method, path, string(reqBody))
		return err, 0
	}

	err := p.handleCopilotNonStreamingResponse(ctx, c, modifiedBody, modelName, startTime, requestID, method, path, string(reqBody))
	return err, 0
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

func (p *CopilotProxy) buildURL() string {
	return CopilotTargetURL(p.provider)
}

// HandleAnthropicFormat 处理 Anthropic 格式请求（包括发送和响应转发）
// Copilot 需要：Anthropic → OpenAI 转换 → 发送 → OpenAI → Anthropic 转换 → 转发
// 返回: (error, statusCode)
func (p *CopilotProxy) HandleAnthropicFormat(ctx context.Context, c *gin.Context, reqHdr http.Header, reqBody []byte) (error, int) {
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

	// 1. 转换 Anthropic -> OpenAI
	openaiBody, modelName, err := p.convertAnthropicToOpenAI(reqBody)
	if err != nil {
		return err, 0
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
		err = p.handleCopilotStreamingResponse(ctx, c, modifiedBody, modelName, startTime, &firstTokenTime, requestID, method, path, string(reqBody))
		return err, 0
	}

	err = p.handleCopilotNonStreamingResponse(ctx, c, modifiedBody, modelName, startTime, requestID, method, path, string(reqBody))
	return err, 0
}

// handleCopilotNonStreamingResponse 处理 Copilot 非流式响应
// HandleCodexFormat 不支持 Codex 格式
func (p *CopilotProxy) HandleCodexFormat(ctx context.Context, c *gin.Context, reqHdr http.Header, reqBody []byte) (error, int) {
	return fmt.Errorf("codex format not supported by Copilot provider: %s", p.provider.Name), 0
}

// handleCopilotNonStreamingResponse 处理 Copilot 非流式响应
func (p *CopilotProxy) handleCopilotNonStreamingResponse(ctx context.Context, c *gin.Context, reqBody []byte, modelName string, startTime time.Time, requestID, method, path, requestBody string) error {
	// 获取 Copilot token
	copilotToken := RefreshCopilotToken(p.provider)
	if copilotToken == "" {
		logger.Error("Failed to get Copilot token for provider %s", p.provider.Name)
		return fmt.Errorf("failed to get Copilot token")
	}
	logger.Info("Using Copilot token (length: %d, prefix: %s)", len(copilotToken), copilotToken[:10])

	baseURL := p.buildURL()
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("create request | model:%s provider:%s | %w", modelName, p.provider.Name, err)
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
		return fmt.Errorf("send request | model:%s provider:%s | %w", modelName, p.provider.Name, err)
	}
	defer resp.Body.Close()

	respBytes, err := readResponseBody(resp)
	if err != nil {
		return fmt.Errorf("read body | model:%s provider:%s | %w", modelName, p.provider.Name, err)
	}

	if resp.StatusCode != http.StatusOK {
		return NewProxyError(resp.StatusCode, fmt.Errorf("api error | model:%s provider:%s | status %d body: %s", modelName, p.provider.Name, resp.StatusCode, string(respBytes)))
	}

	// 转换 OpenAI 响应 -> Anthropic 格式
	anthropicResp, err := convertOpenAIResponseToAnthropic(respBytes)
	if err != nil {
		logger.Error("Response conversion failed: %v", err)
		return err
	}

	// 解析 token 统计
	_, inputTokens, outputTokens, cacheReadTokens := parseTokenStats(anthropicResp)

	// 设置响应头并返回
	c.Header("Content-Type", "application/json")
	c.Data(200, "application/json", anthropicResp)

	// 记录统计
	duration := time.Since(startTime).Milliseconds()
	cost := calculateCost(modelName, inputTokens, outputTokens, cacheReadTokens)

	keyID, clientIP := GetAuthInfo(c)
	if clientIP == "" {
		clientIP = c.ClientIP()
	}

	stats.RecordUsage(p.provider.ID, p.provider.Name, modelName, "non-stream", "ccproxy",
		inputTokens, outputTokens, cacheReadTokens, cost, duration, 0, keyID, clientIP)

	// 记录 history（记录转换后的响应）
	history.AddRecord(history.RequestRecord{
		ID:                   requestID,
		Timestamp:            startTime,
		Method:               method,
		Path:                 path,
		ClientIP:             clientIP,
		KeyID:                keyID,
		Provider:             p.provider.Name,
		Model:                modelName,
		StatusCode:           resp.StatusCode,
		Duration:             duration,
		RequestBody:          requestBody,
		ResponseBody:         string(anthropicResp),
		RequestHeaders:       c.Request.Header,
		ResponseHeaders:      resp.Header,
		RequestSize:          int64(len(requestBody)),
		ResponseSize:         int64(len(anthropicResp)),
		InputTokens:          inputTokens,
		OutputTokens:         outputTokens,
		CacheReadInputTokens: cacheReadTokens,
		TotalTokens:          inputTokens + outputTokens + cacheReadTokens,
		Cost:                 cost,
	})

	return nil
}

// handleCopilotStreamingResponse 处理 Copilot 流式响应
func (p *CopilotProxy) handleCopilotStreamingResponse(ctx context.Context, c *gin.Context, reqBody []byte, modelName string, startTime time.Time, firstTokenTime *time.Time, requestID, method, path, requestBody string) error {
	// 获取 Copilot token
	copilotToken := RefreshCopilotToken(p.provider)
	if copilotToken == "" {
		return fmt.Errorf("failed to get Copilot token")
	}

	baseURL := p.buildURL()
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("create request | model:%s provider:%s | %w", modelName, p.provider.Name, err)
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
		return fmt.Errorf("send request | model:%s provider:%s | %w", modelName, p.provider.Name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("api error | model:%s provider:%s | status %d body: %s", modelName, p.provider.Name, resp.StatusCode, string(body))
	}

	// 设置流式响应头
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming not supported | model:%s provider:%s", modelName, p.provider.Name)
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

	// 流式转发 + 格式转换
	var inputTokens, outputTokens, cacheReadTokens int
	scanner := bufio.NewScanner(reader)
	buf := scannerBufferPool.Get().([]byte)
	defer func() { scannerBufferPool.Put(buf) }()
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if firstTokenTime != nil && firstTokenTime.IsZero() {
			*firstTokenTime = time.Now()
		}

		// 转换 OpenAI SSE -> Anthropic SSE
		convertedLine := convertOpenAIStreamLineToAnthropic(line)
		c.Writer.WriteString(convertedLine)
		flusher.Flush()

		// 提取 token 统计（使用原始行）
		inputTokens, outputTokens, cacheReadTokens = extractTokensFromSSE(line, inputTokens, outputTokens, cacheReadTokens)
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan stream | model:%s provider:%s | %w", modelName, p.provider.Name, err)
	}

	// 记录统计
	duration := time.Since(startTime).Milliseconds()
	var timeToFirst int64
	if !firstTokenTime.IsZero() {
		timeToFirst = firstTokenTime.Sub(startTime).Milliseconds()
	}
	cost := calculateCost(modelName, inputTokens, outputTokens, cacheReadTokens)

	keyID, clientIP := GetAuthInfo(c)
	if clientIP == "" {
		clientIP = c.ClientIP()
	}

	stats.RecordUsage(p.provider.ID, p.provider.Name, modelName, "stream", "ccproxy",
		inputTokens, outputTokens, cacheReadTokens, cost, duration, timeToFirst, keyID, clientIP)

	// 记录 history（记录转换后的响应）
	// 注意：这里没有收集完整的流式响应体，因为转换后的响应大小可能很大
	history.AddRecord(history.RequestRecord{
		ID:                   requestID,
		Timestamp:            startTime,
		Method:               method,
		Path:                 path,
		ClientIP:             clientIP,
		KeyID:                keyID,
		Provider:             p.provider.Name,
		Model:                modelName,
		StatusCode:           200, // 流式响应假设成功
		Duration:             duration,
		RequestBody:          requestBody,
		ResponseBody:         "(streaming response not logged)",
		RequestHeaders:       c.Request.Header,
		ResponseHeaders:      nil, // 流式响应头已经在上面设置过了
		RequestSize:          int64(len(requestBody)),
		ResponseSize:         0,
		InputTokens:          inputTokens,
		OutputTokens:         outputTokens,
		CacheReadInputTokens: cacheReadTokens,
		TotalTokens:          inputTokens + outputTokens + cacheReadTokens,
		Cost:                 cost,
	})

	return nil
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
