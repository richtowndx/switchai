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

// AnthropicProxy 支持的内容块类型
// Anthropic 格式请求本身支持: text, image, tool_use, tool_result, thinking, redacted_thinking
// 只有从 OpenAI 格式转换时，才需要过滤不兼容的块
var supportedAnthropicBlockTypes = []string{"text", "image", "tool_use", "tool_result", "thinking", "redacted_thinking"}

// supportedAnthropicBlockTypesFromOpenAI 从 OpenAI 转换时允许的内容块类型
// OpenAI 格式不包含 thinking/redacted_thinking，转换后也不应有
var supportedAnthropicBlockTypesFromOpenAI = []string{"text", "image", "tool_use", "tool_result"}

// filterUnsupportedContentBlocks 过滤不支持的内容块
// allowedTypes 参数指定允许的内容块类型列表
// 注意：如果过滤后为空（包括原始空数组），返回空文本块以保持请求结构有效
func filterUnsupportedContentBlocks(content interface{}, allowedTypes []string) interface{} {
	if content == nil {
		return content
	}

	// 字符串内容直接返回
	if _, ok := content.(string); ok {
		return content
	}

	// 处理数组类型的内容块
	blocks, ok := content.([]interface{})
	if !ok {
		return content
	}

	// 创建允许类型集合
	typeSet := make(map[string]bool)
	for _, t := range allowedTypes {
		typeSet[t] = true
	}

	var filtered []interface{}
	var filteredTypes []string
	for _, block := range blocks {
		bm, ok := block.(map[string]interface{})
		if !ok {
			continue
		}
		blockType, _ := bm["type"].(string)

		if typeSet[blockType] {
			filtered = append(filtered, block)
		} else if blockType != "" {
			filteredTypes = append(filteredTypes, blockType)
		}
	}

	// 如果过滤后为空（包括原始空数组的情况），返回空文本块
	if len(filtered) == 0 {
		if len(blocks) > 0 && len(filteredTypes) > 0 {
			logger.Warn("All content blocks were filtered (types: %v), returning empty text block", filteredTypes)
		}
		return []interface{}{
			map[string]interface{}{"type": "text", "text": ""},
		}
	}

	// 如果有部分内容被过滤，记录日志
	if len(filtered) < len(blocks) {
		logger.Info("Filtered unsupported block types: %v", filteredTypes)
	}

	return filtered
}

// filterMessagesContentBlocks 过滤消息数组中不兼容的内容块
func filterMessagesContentBlocks(messages interface{}, allowedTypes []string) interface{} {
	if messages == nil {
		return messages
	}

	msgs, ok := messages.([]interface{})
	if !ok {
		return messages
	}

	for _, msg := range msgs {
		msgMap, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		if content, ok := msgMap["content"]; ok {
			msgMap["content"] = filterUnsupportedContentBlocks(content, allowedTypes)
		}
	}

	return msgs
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
func (p *AnthropicProxy) SendAnthropicFormat(ctx context.Context, reqHdr http.Header, reqBody []byte) *ProxyResponse {
	modifiedBody, modelName, isStream := p.parseAnthropicRequest(reqBody)

	if isStream {
		resp := p.sendAnthropicStream(ctx, reqHdr, modifiedBody)
		resp.ModelName = modelName
		return resp
	}
	resp := p.sendAnthropicNonStream(ctx, reqHdr, modifiedBody)
	resp.ModelName = modelName
	return resp
}

// SendOpenAIFormat 发送 OpenAI 格式请求
func (p *AnthropicProxy) SendOpenAIFormat(ctx context.Context, reqHdr http.Header, reqBody []byte) *ProxyResponse {
	modifiedBody, modelName, isStream := p.parseOpenAIRequest(reqBody)

	if isStream {
		resp := p.sendAnthropicStream(ctx, reqHdr, modifiedBody)
		resp.ModelName = modelName
		return resp
	}
	resp := p.sendAnthropicNonStream(ctx, reqHdr, modifiedBody)
	resp.ModelName = modelName
	return resp
}

// HandleAnthropicFormat 处理 Anthropic 格式请求（包括发送和响应转发）
// 返回: (error, statusCode)
func (p *AnthropicProxy) HandleAnthropicFormat(ctx context.Context, c *gin.Context, reqHdr http.Header, reqBody []byte) (error, int) {
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

	// 解析：模型映射 + stream 检查 + 过滤不支持参数（一次 JSON 解析完成）
	modifiedBody, modelName, isStream := p.parseAnthropicRequest(reqBody)

	if isStream {
		return p.handleAnthropicStreamingResponse(ctx, c, reqHdr, modifiedBody, modelName, startTime, &firstTokenTime, requestID, method, path, string(reqBody))
	}

	return p.handleAnthropicNonStreamingResponse(ctx, c, modifiedBody, modelName, startTime, requestID, method, path, string(reqBody))
}

// HandleOpenAIFormat 处理 OpenAI 格式请求（包括发送和响应转发）
// 返回: (error, statusCode)
func (p *AnthropicProxy) HandleOpenAIFormat(ctx context.Context, c *gin.Context, reqHdr http.Header, reqBody []byte) (error, int) {
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

	// 解析：OpenAI → Anthropic 转换 + 模型映射 + stream 检查 + 过滤不支持参数和内容块
	modifiedBody, modelName, isStream := p.parseOpenAIRequest(reqBody)

	if isStream {
		return p.handleAnthropicStreamingResponse(ctx, c, reqHdr, modifiedBody, modelName, startTime, &firstTokenTime, requestID, method, path, string(reqBody))
	}

	return p.handleAnthropicNonStreamingResponse(ctx, c, modifiedBody, modelName, startTime, requestID, method, path, string(reqBody))
}

// HandleCodexFormat 不支持 Codex 格式
func (p *AnthropicProxy) HandleCodexFormat(ctx context.Context, c *gin.Context, reqHdr http.Header, reqBody []byte) (error, int) {
	return fmt.Errorf("codex format not supported by Anthropic provider: %s", p.provider.Name), 0
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

// parseAnthropicRequest 解析 Anthropic 格式请求：模型映射 + stream 检查 + 过滤不支持参数
// 一次 JSON 解析完成所有操作，避免多次 marshal/unmarshal
// Anthropic → Anthropic：不过滤内容块，thinking 等块保持原样
//
// P1 优化：使用 map[string]*json.RawMessage 保留 null 值
func (p *AnthropicProxy) parseAnthropicRequest(reqBody []byte) ([]byte, string, bool) {
	// 使用 *json.RawMessage 保留原始 JSON 值，包括 null
	var req map[string]*json.RawMessage
	if err := json.Unmarshal(reqBody, &req); err != nil {
		return reqBody, "", false
	}

	modelName := ""
	isStream := false

	// 基于 hostname 过滤不支持的参数
	for _, key := range getFilterParams(p.provider.Hostname, "anthropic") {
		logger.Info("remove %s unsupported parameter: %s", p.provider.Hostname, key)
		delete(req, key)
	}

	// 处理模型映射
	if modelRaw, ok := req["model"]; ok {
		var model string
		if err := json.Unmarshal(*modelRaw, &model); err == nil {
			modelName = model
			resolved := p.provider.ResolveModel(model)
			if resolved != model {
				logger.Info("Model resolution: %s → %s (provider: %s)", model, resolved, p.provider.Name)
				newModelRaw, _ := json.Marshal(resolved)
				req["model"] = (*json.RawMessage)(&newModelRaw)
				modelName = resolved
			}
		}
	} else {
		// model 字段缺失，添加默认模型并记录错误日志
		defaultModel := p.provider.ResolveModel("default_model")
		defaultModelRaw, _ := json.Marshal(defaultModel)
		req["model"] = (*json.RawMessage)(&defaultModelRaw)
		modelName = defaultModel
		logger.Error("[MissingModel] Request missing 'model' field, added default: %s (provider: %s)", defaultModel, p.provider.Name)
	}

	// 检查 stream
	if streamRaw, ok := req["stream"]; ok {
		var stream bool
		if err := json.Unmarshal(*streamRaw, &stream); err == nil {
			isStream = stream
		}
	}

	result, _ := json.Marshal(req)
	return result, modelName, isStream
}

// parseOpenAIRequest 解析 OpenAI 格式请求并转换为 Anthropic 格式
// 一次 JSON 解析完成：格式转换 + 模型映射 + stream 检查 + 过滤不支持参数和内容块
func (p *AnthropicProxy) parseOpenAIRequest(openaiReq []byte) ([]byte, string, bool) {
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

	// 基于 hostname 过滤不支持的参数
	for _, key := range getFilterParams(p.provider.Hostname, "anthropic") {
		logger.Info("remove %s unsupported parameter: %s", p.provider.Hostname, key)
		delete(anthropicReq, key)
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

	// 过滤不兼容的内容块（OpenAI 格式不应包含 thinking 块，做防御性过滤）
	anthropicReq["messages"] = filterMessagesContentBlocks(anthropicReq["messages"], supportedAnthropicBlockTypesFromOpenAI)

	result, _ := json.Marshal(anthropicReq)
	return result, modelName, isStream
}

func (p *AnthropicProxy) sendAnthropicNonStream(ctx context.Context, reqHdr http.Header, reqBody []byte) *ProxyResponse {
	baseURL := p.buildURL()
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL, bytes.NewReader(reqBody))
	if err != nil {
		return &ProxyResponse{Error: fmt.Errorf("create request: %w", err)}
	}

	// 基础请求头
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.provider.APIKey)
	req.Header.Set("User-Agent", "claude-cli/2.1.139 (external, cli)")
	req.Header.Set("x-app", "cli")

	// P0 优化：继承客户端的 anthropic-beta，仅在缺失时使用默认值
	originalBeta := reqHdr.Get("anthropic-beta")
	if originalBeta != "" {
		req.Header.Set("anthropic-beta", originalBeta)
	} else {
		req.Header.Set("anthropic-beta", "claude-code-20250219,interleaved-thinking-2025-05-14,redact-thinking-2026-02-12,context-management-2025-06-27,prompt-caching-scope-2026-01-05,effort-2025-11-24")
	}

	// P0 优化：继承客户端的 anthropic-version，仅在缺失时使用默认值
	originalVersion := reqHdr.Get("anthropic-version")
	if originalVersion != "" {
		req.Header.Set("anthropic-version", originalVersion)
	} else {
		req.Header.Set("anthropic-version", "2023-06-01")
	}

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

		// P1 优化：将 header 设置移到循环外
		// 设置必要的请求头（转发客户端的 header，过滤掉敏感头部）
		for k, v := range reqHdr {
			for _, val := range v {
				if strings.ToLower(k) == "authorization" ||
					strings.ToLower(k) == "content-type" ||
					strings.ToLower(k) == "user-agent" ||
					strings.ToLower(k) == "anthropic-beta" ||
					strings.ToLower(k) == "x-app" ||
					strings.ToLower(k) == "x-api-key" {
					continue
				}
				req.Header.Add(k, val)
			}
		}

		// 基础请求头
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+p.provider.APIKey)
		req.Header.Set("User-Agent", "claude-cli/2.1.139 (external, cli)")
		req.Header.Set("x-app", "cli")

		// P0 优化：继承客户端的 anthropic-beta，仅在缺失时使用默认值
		originalBeta := reqHdr.Get("anthropic-beta")
		if originalBeta != "" {
			req.Header.Set("anthropic-beta", originalBeta)
		} else {
			req.Header.Set("anthropic-beta", "claude-code-20250219,interleaved-thinking-2025-05-14,redact-thinking-2026-02-12,context-management-2025-06-27,prompt-caching-scope-2026-01-05,effort-2025-11-24")
		}

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
				// 空行：SSE 事件边界，需要发送 \n\n
				// 空行 + \n = \n\n
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
// P0 优化：继承客户端的 anthropic-beta 和 anthropic-version 头部，仅在缺失时使用默认值
func (p *AnthropicProxy) handleAnthropicNonStreamingResponse(ctx context.Context, c *gin.Context, reqBody []byte, modelName string, startTime time.Time, requestID, method, path, requestBody string) (error, int) {
	baseURL := p.buildURL()
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("create request | model:%s provider:%s | %w", modelName, p.provider.Name, err), 0
	}

	// 基础请求头
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.provider.APIKey)
	req.Header.Set("User-Agent", "claude-cli/2.1.139 (external, cli)")
	req.Header.Set("x-app", "cli")

	// P0 优化：继承客户端的 anthropic-beta，仅在缺失时使用默认值
	originalBeta := c.GetHeader("anthropic-beta")
	if originalBeta != "" {
		req.Header.Set("anthropic-beta", originalBeta)
	} else {
		// 默认 beta 标志，包含最常用的功能
		req.Header.Set("anthropic-beta", "claude-code-20250219,interleaved-thinking-2025-05-14,redact-thinking-2026-02-12,context-management-2025-06-27,prompt-caching-scope-2026-01-05,effort-2025-11-24")
	}

	// P0 优化：继承客户端的 anthropic-version，仅在缺失时使用默认值
	originalVersion := c.GetHeader("anthropic-version")
	if originalVersion != "" {
		req.Header.Set("anthropic-version", originalVersion)
	} else {
		req.Header.Set("anthropic-version", "2023-06-01")
	}

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
		logger.Error("none stream status=%d req=%s resp=%s", resp.StatusCode, string(reqBody), string(respBytes))
		return NewProxyErrorWithBody(resp.StatusCode, fmt.Errorf("api error | model:%s provider:%s | status %d", modelName, p.provider.Name, resp.StatusCode), respBytes), resp.StatusCode
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

// handleAnthropicStreamingResponse 处理流式响应
func (p *AnthropicProxy) handleAnthropicStreamingResponse(ctx context.Context, c *gin.Context, reqHdr http.Header, reqBody []byte, modelName string, startTime time.Time, firstTokenTime *time.Time, requestID, method, path, requestBody string) (error, int) {
	baseURL := p.buildURL()
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("create request | model:%s provider:%s | %w", modelName, p.provider.Name, err), 0
	}

	// 设置必要的请求头
	// P1 优化：将 header 设置移到循环外，避免重复设置
	for k, v := range reqHdr {
		for _, val := range v {
			if strings.ToLower(k) == "authorization" ||
				strings.ToLower(k) == "content-type" ||
				strings.ToLower(k) == "user-agent" ||
				strings.ToLower(k) == "anthropic-beta" ||
				strings.ToLower(k) == "x-app" ||
				strings.ToLower(k) == "x-api-key" {
				continue
			}
			req.Header.Add(k, val)
		}
	}

	// 基础请求头
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.provider.APIKey)
	req.Header.Set("User-Agent", "claude-cli/2.1.139 (external, cli)")
	req.Header.Set("x-app", "cli")

	// P0 优化：继承客户端的 anthropic-beta，仅在缺失时使用默认值
	originalBeta := c.GetHeader("anthropic-beta")
	if originalBeta != "" {
		req.Header.Set("anthropic-beta", originalBeta)
	} else {
		req.Header.Set("anthropic-beta", "claude-code-20250219,interleaved-thinking-2025-05-14,redact-thinking-2026-02-12,context-management-2025-06-27,prompt-caching-scope-2026-01-05,effort-2025-11-24")
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w; model:%s provider:%s", err, modelName, p.provider.Name), 0
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		logger.Error("stream error: code %d req=%s resp=%s", resp.StatusCode, string(reqBody), string(body))
		return NewProxyErrorWithBody(resp.StatusCode, fmt.Errorf("api error | model:%s provider:%s | status %d", modelName, p.provider.Name, resp.StatusCode), body), resp.StatusCode
	}

	// 设置流式响应头
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming not supported | model:%s provider:%s", modelName, p.provider.Name), 0
	}

	// 处理 Content-Encoding: gzip/brotli
	var reader io.Reader = resp.Body
	contentEncoding := resp.Header.Get("Content-Encoding")
	if contentEncoding != "" && contentEncoding != "identity" {
		switch strings.ToLower(contentEncoding) {
		case "gzip":
			gzReader, err := gzip.NewReader(resp.Body)
			if err != nil {
				return fmt.Errorf("gzip decompress: %w", err), 0
			}
			defer gzReader.Close()
			reader = gzReader
		case "br":
			reader = brotli.NewReader(resp.Body)
		}
	}

	// 流式转发
	var inputTokens, outputTokens, cacheReadTokens int
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

		// 转发 SSE 行
		c.Writer.WriteString(line)
		if line != "" {
			// 非空行：添加换行符
			c.Writer.WriteString("\n")
		} else {
			// 空行：SSE 事件边界，需要添加 \n 形成 \n\n
			// 写一个 \n，与前面的 \n 一起形成 \n\n
			c.Writer.WriteString("\n")
		}
		flusher.Flush()

		// 收集响应体（限制大小）
		if responseBody.Len() < 100*1024 { // 限制 100KB
			responseBody.WriteString(line)
			if line != "" {
				responseBody.WriteString("\n")
			} else {
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
