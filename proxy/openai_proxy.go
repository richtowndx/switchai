package proxy

import (
	"bufio"
	"bytes"
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

	"github.com/gin-gonic/gin"
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

// supportedOpenAIBlockTypesFromAnthropic 从 Anthropic 转换到 OpenAI 时允许的内容块类型
// OpenAI 格式不包含 thinking/redacted_thinking，需要过滤
// image 块在 convertAnthropicContentToOpenAI 中已转换为 image_url
var supportedOpenAIBlockTypesFromAnthropic = []string{"text", "image_url"}

// parseAndProcessRequest 一次性解析并处理：模型映射 + stream 检查 + 参数过滤
func (p *OpenAIProxy) parseAndProcessRequest(reqBody []byte) ([]byte, string, bool) {
	var req map[string]interface{}
	if err := json.Unmarshal(reqBody, &req); err != nil {
		return reqBody, "", false
	}

	modelName := ""
	isStream := false

	// 过滤不支持的参数（如 structured_outputs）
	for _, key := range getFilterParams(p.provider.Hostname, "openai") {
		delete(req, key)
	}

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
// 白名单方式构建：只复制 OpenAI 支持的字段，不支持的自动丢弃
func (p *OpenAIProxy) convertAndProcessAnthropicRequest(anthropicReq []byte) ([]byte, string, bool) {
	var req map[string]interface{}
	if err := json.Unmarshal(anthropicReq, &req); err != nil {
		return nil, "", false
	}

	modelName := ""
	if model, ok := req["model"].(string); ok {
		modelName = model
	}

	// 构建 OpenAI 格式（白名单方式，不复制 Anthropic 特有字段）
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

	// stop_sequences → stop 转换（Anthropic 用 stop_sequences，OpenAI 用 stop）
	if stopSeqs, ok := req["stop_sequences"].([]interface{}); ok && len(stopSeqs) > 0 {
		if len(stopSeqs) == 1 {
			if s, ok := stopSeqs[0].(string); ok {
				openaiReq["stop"] = s
			}
		} else {
			stops := make([]string, 0, len(stopSeqs))
			for _, s := range stopSeqs {
				if str, ok := s.(string); ok {
					stops = append(stops, str)
				}
			}
			if len(stops) > 0 {
				openaiReq["stop"] = stops
			}
		}
	}

	if tools, ok := req["tools"].([]interface{}); ok {
		openaiReq["tools"] = convertAnthropicToolsToOpenAI(tools)
		openaiReq["tool_choice"] = "auto"
	}

	// 过滤 OpenAI 不支持的参数（如 structured_outputs）
	for _, key := range getFilterParams(p.provider.Hostname, "openai") {
		delete(openaiReq, key)
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

	// 过滤不兼容的内容块（Anthropic thinking/redacted_thinking 等 OpenAI 不支持的块）
	if msgs, ok := openaiReq["messages"].([]interface{}); ok {
		for _, msg := range msgs {
			if msgMap, ok := msg.(map[string]interface{}); ok {
				if content, exists := msgMap["content"]; exists {
					msgMap["content"] = filterUnsupportedContentBlocks(content, supportedOpenAIBlockTypesFromAnthropic)
				}
			}
		}
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

// HandleOpenAIFormat 处理 OpenAI 格式请求（包括发送和响应转发）
// 返回: (error, statusCode)
func (p *OpenAIProxy) HandleOpenAIFormat(ctx context.Context, c *gin.Context, reqHdr http.Header, reqBody []byte) (error, int) {
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

	if isStream {
		return p.handleOpenAIStreamingResponse(ctx, c, modifiedBody, modelName, startTime, &firstTokenTime, requestID, method, path, string(reqBody))
	}

	return p.handleOpenAINonStreamingResponse(ctx, c, modifiedBody, modelName, startTime, requestID, method, path, string(reqBody))
}

// HandleAnthropicFormat 处理 Anthropic 格式请求（包括发送和响应转发）
// 返回: (error, statusCode)
func (p *OpenAIProxy) HandleAnthropicFormat(ctx context.Context, c *gin.Context, reqHdr http.Header, reqBody []byte) (error, int) {
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

	// 1. 转换并处理：Anthropic → OpenAI + 模型映射 + stream 检查
	modifiedBody, modelName, isStream := p.convertAndProcessAnthropicRequest(reqBody)

	if isStream {
		return p.handleOpenAIStreamingResponse(ctx, c, modifiedBody, modelName, startTime, &firstTokenTime, requestID, method, path, string(reqBody))
	}

	return p.handleOpenAINonStreamingResponse(ctx, c, modifiedBody, modelName, startTime, requestID, method, path, string(reqBody))
}

// handleOpenAIStreamingResponse 处理流式响应
func (p *OpenAIProxy) handleOpenAIStreamingResponse(ctx context.Context, c *gin.Context, reqBody []byte, modelName string, startTime time.Time, firstTokenTime *time.Time, requestID, method, path, requestBody string) (error, int) {
	baseURL := p.buildURL()
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("create request | model:%s provider:%s | %w", modelName, p.provider.Name, err), 0
	}

	// 设置请求头
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.provider.APIKey)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("send request | model:%s provider:%s | %w", modelName, p.provider.Name, err), 0
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return NewProxyError(resp.StatusCode, fmt.Errorf("api error | model:%s provider:%s | status %d body: %s", modelName, p.provider.Name, resp.StatusCode, string(body))), resp.StatusCode
	}

	// 设置流式响应头
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming not supported | model:%s provider:%s", modelName, p.provider.Name), 0
	}

	// 流式转发
	var inputTokens, outputTokens, cacheReadTokens int
	var responseBody strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	buf := scannerBufferPool.Get().([]byte)
	defer func() { scannerBufferPool.Put(buf) }()
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if firstTokenTime.IsZero() {
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
		inputTokens, outputTokens, cacheReadTokens = extractTokensFromSSE(line, inputTokens, outputTokens, cacheReadTokens)
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan stream | model:%s provider:%s | %w", modelName, p.provider.Name, err), 0
	}

	// 记录统计
	duration := time.Since(startTime).Milliseconds()
	var timeToFirst int64
	if firstTokenTime != nil && !firstTokenTime.IsZero() {
		timeToFirst = firstTokenTime.Sub(startTime).Milliseconds()
	}
	cost := calculateCost(modelName, inputTokens, outputTokens, cacheReadTokens)

	keyID, clientIP := GetAuthInfo(c)
	if clientIP == "" {
		clientIP = c.ClientIP()
	}

	stats.RecordUsage(p.provider.ID, p.provider.Name, modelName, "stream", "ccproxy",
		inputTokens, outputTokens, cacheReadTokens, cost, duration, timeToFirst, keyID, clientIP)

	// 记录 history
	responseBodyStr := responseBody.String()
	if responseBody.Len() >= 100*1024 {
		responseBodyStr += "\n... (truncated)"
	}

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
		ResponseBody:         responseBodyStr,
		RequestHeaders:       c.Request.Header,
		ResponseHeaders:      resp.Header,
		RequestSize:          int64(len(requestBody)),
		ResponseSize:         int64(responseBody.Len()),
		InputTokens:          inputTokens,
		OutputTokens:         outputTokens,
		CacheReadInputTokens: cacheReadTokens,
		TotalTokens:          inputTokens + outputTokens + cacheReadTokens,
		Cost:                 cost,
	})
	return nil, 0
}

// handleOpenAINonStreamingResponse 处理非流式响应
func (p *OpenAIProxy) handleOpenAINonStreamingResponse(ctx context.Context, c *gin.Context, reqBody []byte, modelName string, startTime time.Time, requestID, method, path, requestBody string) (error, int) {
	baseURL := p.buildURL()
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("create request | model:%s provider:%s | %w", modelName, p.provider.Name, err), 0
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.provider.APIKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("send request | model:%s provider:%s | %w", modelName, p.provider.Name, err), 0
	}
	defer resp.Body.Close()

	respBytes, err := readResponseBody(resp)
	if err != nil {
		return fmt.Errorf("read body | model:%s provider:%s | %w", modelName, p.provider.Name, err), 0
	}

	if resp.StatusCode != http.StatusOK {
		return NewProxyError(resp.StatusCode, fmt.Errorf("api error | model:%s provider:%s | status %d body: %s", modelName, p.provider.Name, resp.StatusCode, string(respBytes))), resp.StatusCode
	}

	// 解析 token 统计
	_, inputTokens, outputTokens, cacheReadTokens := parseTokenStats(respBytes)

	// 设置响应头并返回
	c.Header("Content-Type", "application/json")
	c.Data(200, "application/json", respBytes)

	// 记录统计
	duration := time.Since(startTime).Milliseconds()
	cost := calculateCost(modelName, inputTokens, outputTokens, cacheReadTokens)

	keyID, clientIP := GetAuthInfo(c)
	if clientIP == "" {
		clientIP = c.ClientIP()
	}

	stats.RecordUsage(p.provider.ID, p.provider.Name, modelName, "non-stream", "ccproxy",
		inputTokens, outputTokens, cacheReadTokens, cost, duration, 0, keyID, clientIP)

	// 记录 history
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
		ResponseBody:         string(respBytes),
		RequestHeaders:       c.Request.Header,
		ResponseHeaders:      resp.Header,
		RequestSize:          int64(len(requestBody)),
		ResponseSize:         int64(len(respBytes)),
		InputTokens:          inputTokens,
		OutputTokens:         outputTokens,
		CacheReadInputTokens: cacheReadTokens,
		TotalTokens:          inputTokens + outputTokens + cacheReadTokens,
		Cost:                 cost,
	})
	return nil, 0
}
