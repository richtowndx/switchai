package proxy

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"switchai/config"
	"switchai/history"
	"switchai/logger"
	"switchai/stats"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const maxProviderRetries = 3

func RegisterRoutes(r *gin.Engine) {
	r.Any("/v1/*path", proxyHandler)
}

// ============================================================
// 认证 & 鉴权
// ============================================================

// authenticate 验证请求的 Authorization 头，返回 (keyID, clientIP, ok)
func authenticate(c *gin.Context) (string, string, bool) {
	authHeader := c.GetHeader("Authorization")
	if authHeader == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Missing authorization header"})
		return "", "", false
	}

	providedKey := authHeader
	if strings.HasPrefix(authHeader, "Bearer ") {
		providedKey = strings.TrimPrefix(authHeader, "Bearer ")
	}

	keyID, isValid := config.GetConfig().ValidateServerKey(providedKey)
	if !isValid {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or disabled server key"})
		return "", "", false
	}

	// 检查密钥限额
	serverKey := config.GetConfig().GetServerKeyByID(keyID)
	if serverKey != nil {
		allowed, reason := stats.CheckKeyLimit(keyID,
			serverKey.DailyReqLimit, serverKey.TotalReqLimit,
			serverKey.DailyCostLimit, serverKey.TotalCostLimit)
		if !allowed {
			c.JSON(http.StatusForbidden, gin.H{"error": reason})
			return "", "", false
		}
	}

	return keyID, c.ClientIP(), true
}

// ============================================================
// 目标 URL 构建
// ============================================================

// buildTargetURL 根据供应商 BaseURL 和请求路径构建上游目标 URL
func buildTargetURL(provider *config.Provider, reqPath, rawQuery string) string {
	base := provider.BaseURL
	if strings.Contains(base, "/v1/") || strings.HasSuffix(base, "/v1") {
		idx := strings.Index(base, "/v1")
		return base[:idx] + reqPath + querySuffix(rawQuery)
	}
	return strings.TrimSuffix(base, "/") + reqPath + querySuffix(rawQuery)
}

// buildTargetURLForConversion 为格式转换场景构建目标 URL
func buildTargetURLForConversion(provider *config.Provider, isOpenAIFormat bool) string {
	base := strings.TrimSuffix(provider.BaseURL, "/")
	if isOpenAIFormat {
		// OpenAI 格式 → /chat/completions
		if strings.HasSuffix(base, "/v1") {
			return base + "/chat/completions"
		}
		return base + "/v1/chat/completions"
	}
	// Anthropic 格式 → /v1/messages
	if strings.HasSuffix(base, "/v1") {
		return base + "/messages"
	}
	return base + "/v1/messages"
}

func querySuffix(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	return "?" + rawQuery
}

// ============================================================
// 请求体处理
// ============================================================

// processRequestResult 保存请求体处理结果
type processRequestResult struct {
	BodyBytes           []byte
	ModifiedRequestBody string
	RequestedModel      string
	IsStream            bool
	TargetURL           string
}

// processRequestBody 解析请求体、解析模型名、执行格式转换
func processRequestBody(bodyBytes []byte, provider *config.Provider, isIncomingOpenAIFormat bool, reqPath, rawQuery string) (*processRequestResult, error) {
	result := &processRequestResult{
		BodyBytes:           bodyBytes,
		ModifiedRequestBody: string(bodyBytes),
		RequestedModel:      "unknown",
		TargetURL:           buildTargetURL(provider, reqPath, rawQuery),
	}

	if len(bodyBytes) == 0 || !json.Valid(bodyBytes) {
		return result, nil
	}

	var requestBody map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &requestBody); err != nil {
		return result, nil
	}

	// 检测流式
	if stream, ok := requestBody["stream"].(bool); ok && stream {
		result.IsStream = true
	}

	// 模型解析：用传入的模型名去查 provider 的模型映射表
	if model, ok := requestBody["model"].(string); ok {
		result.RequestedModel = model
		resolved := provider.ResolveModel(model)
		logger.Info("Model resolution: %s → %s (provider: %s)", model, resolved, provider.Name)
		requestBody["model"] = resolved
		result.RequestedModel = resolved
	}

	// 格式转换
	needsConversion := provider.IsOpenAIFormat != isIncomingOpenAIFormat
	if needsConversion {
		if provider.IsOpenAIFormat && !isIncomingOpenAIFormat {
			logger.Info("Converting Anthropic request to OpenAI format")
			requestBody = convertClaudeToOpenAI(requestBody)
			result.TargetURL = buildTargetURLForConversion(provider, true)
		} else if !provider.IsOpenAIFormat && isIncomingOpenAIFormat {
			logger.Info("Converting OpenAI request to Anthropic format")
			requestBody = convertOpenAIToClaudeRequest(requestBody)
			result.TargetURL = buildTargetURLForConversion(provider, false)
		}
	} else if provider.IsOpenAIFormat && isIncomingOpenAIFormat {
		result.TargetURL = buildTargetURLForConversion(provider, true)
	}

	// 重新序列化
	serialized, _ := json.Marshal(requestBody)
	result.BodyBytes = serialized
	result.ModifiedRequestBody = string(serialized)
	return result, nil
}

// ============================================================
// 上游请求
// ============================================================

// sendRequest 发送单次请求到上游，返回响应（不做重试）
func sendRequest(method, targetURL string, originalHeaders http.Header, bodyBytes []byte, provider *config.Provider) (*http.Response, error) {
	req, err := http.NewRequest(method, targetURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}

	// 复制请求头（跳过原始 Authorization）
	for key, values := range originalHeaders {
		if key == "Authorization" {
			continue
		}
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	// 设置供应商的 API Key
	req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}

	// 检查是否为可重试的服务端错误
	if isRetryableError(resp) {
		resp.Body.Close()
		return nil, &retryableError{StatusCode: resp.StatusCode}
	}

	return resp, nil
}

// retryableError 表示可重试的上游错误（换 provider 重试）
type retryableError struct {
	StatusCode int
}

func (e *retryableError) Error() string {
	return "upstream returned retryable error"
}

// isRetryableError 判断响应是否为可重试的服务端错误
func isRetryableError(resp *http.Response) bool {
	if resp.StatusCode >= 500 || resp.StatusCode == 429 || resp.StatusCode == 529 {
		return true
	}
	return false
}

// ============================================================
// 主入口：proxyHandler
// ============================================================

func proxyHandler(c *gin.Context) {
	startTime := time.Now()
	requestID := uuid.New().String()

	// 1. 认证
	keyID, clientIP, ok := authenticate(c)
	if !ok {
		return
	}

	// 2. 检测请求格式
	isIncomingOpenAIFormat := strings.HasPrefix(c.Request.URL.Path, "/v1/chat")

	// 3. 读取请求体
	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read request body"})
		return
	}

	// 4. 轮询重试：最多尝试 maxProviderRetries 个不同 provider
	var lastErr error
	for attempt := 0; attempt < maxProviderRetries; attempt++ {
		provider := config.GetConfig().GetNextProvider(isIncomingOpenAIFormat)
		if provider == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "No provider configured"})
			return
		}

		logger.Info("📡 Provider attempt %d/%d: %s (format: isOpenAI=%v)",
			attempt+1, maxProviderRetries, provider.Name, provider.IsOpenAIFormat)

		// 处理请求体（模型解析 + 格式转换）
		processed, err := processRequestBody(bodyBytes, provider, isIncomingOpenAIFormat,
			c.Request.URL.Path, c.Request.URL.RawQuery)
		if err != nil {
			logger.Error("❌ Failed to process request body: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process request"})
			return
		}

		logger.Info("📡 代理转发 - Provider: %s, BaseURL: %s → Target: %s",
			provider.Name, provider.BaseURL, processed.TargetURL)

		// 发送请求到上游
		resp, err := sendRequest(c.Request.Method, processed.TargetURL,
			c.Request.Header, processed.BodyBytes, provider)
		if err != nil {
			lastErr = err
			if _, ok := err.(*retryableError); ok {
				logger.Info("⚠️ Provider %s 返回可重试错误 (attempt %d/%d)，尝试下一个 provider",
					provider.Name, attempt+1, maxProviderRetries)
				continue
			}
			logger.Error("❌ Provider %s 连接失败: %v", provider.Name, err)
			continue
		}
		defer resp.Body.Close()

		logger.Info("📥 收到响应 - Provider: %s, Status: %d, Content-Type: %s",
			provider.Name, resp.StatusCode, resp.Header.Get("Content-Type"))

		// 5. 处理响应
		if processed.IsStream {
			handleStreamResponse(c, resp, provider, requestID, startTime,
				c.Request.Method, c.Request.URL.Path, processed.ModifiedRequestBody,
				c.Request.Header, processed.RequestedModel, keyID, clientIP, isIncomingOpenAIFormat)
		} else {
			handleNonStreamResponse(c, resp, provider, requestID, startTime,
				c.Request.Method, c.Request.URL.Path, processed.ModifiedRequestBody,
				c.Request.Header, processed.RequestedModel, keyID, clientIP, isIncomingOpenAIFormat)
		}
		return
	}

	// 所有 provider 都失败
	logger.Error("❌ 所有 provider 均失败 (尝试 %d 次): %v", maxProviderRetries, lastErr)
	c.JSON(http.StatusBadGateway, gin.H{"error": "All providers failed after retries"})
}

// ============================================================
// 流式响应处理
// ============================================================

func handleStreamResponse(c *gin.Context, resp *http.Response, provider *config.Provider, requestID string, startTime time.Time, method, path, requestBody string, requestHeaders http.Header, requestedModel string, keyID, clientIP string, isIncomingOpenAIFormat bool) {
	var firstTokenTime time.Time

	c.Status(resp.StatusCode)
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		logger.Info("Streaming not supported")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Streaming not supported"})
		return
	}

	var reader io.Reader = resp.Body
	contentEncoding := resp.Header.Get("Content-Encoding")
	if contentEncoding != "" && contentEncoding != "identity" {
		if decompressed, err := decompressResponse(resp.Body, contentEncoding); err == nil {
			reader = bytes.NewReader(decompressed)
			logger.Info("Decompressed stream response with %s, decompressed size: %d",
				contentEncoding, len(decompressed))
		} else {
			logger.Error("❌ 解压流式响应失败: %v, Content-Encoding: %s", err, contentEncoding)
		}
	}

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var inputTokens, outputTokens int
	var model string
	firstToken := true
	var responseBody strings.Builder
	needsConversion := provider.IsOpenAIFormat != isIncomingOpenAIFormat

	for scanner.Scan() {
		line := scanner.Bytes()

		if needsConversion && provider.IsOpenAIFormat {
			// OpenAI SSE → Claude SSE
			lineStr := string(line)
			if strings.HasPrefix(lineStr, "data: ") {
				data := strings.TrimPrefix(lineStr, "data: ")
				if data == "[DONE]" {
					claudeDone := "data: {\"type\":\"message_stop\"}\n\n"
					if _, err := c.Writer.Write([]byte(claudeDone)); err != nil {
						return
					}
					flusher.Flush()
					responseBody.WriteString(claudeDone)
					continue
				}
				var openaiChunk map[string]interface{}
				if err := json.Unmarshal([]byte(data), &openaiChunk); err == nil {
					if m, ok := openaiChunk["model"].(string); ok && m != "" {
						model = m
					}
					claudeChunk := convertOpenAIStreamToClaude(openaiChunk)
					if claudeData, err := json.Marshal(claudeChunk); err == nil {
						claudeLine := "data: " + string(claudeData) + "\n\n"
						c.Writer.Write([]byte(claudeLine))
						flusher.Flush()
						responseBody.WriteString(claudeLine)
						if usage, ok := claudeChunk["usage"].(map[string]interface{}); ok {
							if input, ok := usage["input_tokens"].(int); ok {
								inputTokens = input
							}
							if output, ok := usage["output_tokens"].(int); ok {
								outputTokens = output
							}
						}
					}
				}
			} else {
				c.Writer.Write(line)
				c.Writer.Write([]byte("\n"))
				flusher.Flush()
				responseBody.Write(line)
				responseBody.WriteString("\n")
			}
		} else if needsConversion {
			// Claude SSE → OpenAI SSE
			lineStr := string(line)
			if strings.HasPrefix(lineStr, "data: ") {
				data := strings.TrimPrefix(lineStr, "data: ")
				if data == "[DONE]" {
					openaiDone := "data: [DONE]\n\n"
					c.Writer.Write([]byte(openaiDone))
					flusher.Flush()
					responseBody.WriteString(openaiDone)
					continue
				}
				var claudeChunk map[string]interface{}
				if err := json.Unmarshal([]byte(data), &claudeChunk); err == nil {
					if m, ok := claudeChunk["model"].(string); ok && m != "" {
						model = m
					}
					openaiChunk := convertClaudeStreamToOpenAI(claudeChunk)
					if openaiData, err := json.Marshal(openaiChunk); err == nil {
						openaiLine := "data: " + string(openaiData) + "\n\n"
						c.Writer.Write([]byte(openaiLine))
						flusher.Flush()
						responseBody.WriteString(openaiLine)
						if usage, ok := claudeChunk["usage"].(map[string]interface{}); ok {
							if input, ok := usage["input_tokens"].(float64); ok {
								inputTokens = int(input)
							}
							if output, ok := usage["output_tokens"].(float64); ok {
								outputTokens = int(output)
							}
						}
					}
				}
			} else {
				c.Writer.Write(line)
				c.Writer.Write([]byte("\n"))
				flusher.Flush()
				responseBody.Write(line)
				responseBody.WriteString("\n")
			}
		} else {
			// 格式匹配，直接转发
			c.Writer.Write(line)
			c.Writer.Write([]byte("\n"))
			flusher.Flush()
			responseBody.Write(line)
			responseBody.WriteString("\n")

			lineStr := string(line)
			if strings.HasPrefix(lineStr, "data: ") {
				data := strings.TrimPrefix(lineStr, "data: ")
				if data == "[DONE]" {
					continue
				}
				var streamData map[string]interface{}
				if err := json.Unmarshal([]byte(data), &streamData); err == nil {
					if m, ok := streamData["model"].(string); ok && m != "" {
						model = m
					}
					if usage, ok := streamData["usage"].(map[string]interface{}); ok {
						if input, ok := usage["input_tokens"].(float64); ok {
							inputTokens = int(input)
						}
						if output, ok := usage["output_tokens"].(float64); ok {
							outputTokens = int(output)
						}
					}
				}
			}
		}

		if firstToken {
			firstTokenTime = time.Now()
			firstToken = false
		}
	}

	duration := time.Since(startTime).Milliseconds()
	var timeToFirst int64
	if !firstTokenTime.IsZero() {
		timeToFirst = firstTokenTime.Sub(startTime).Milliseconds()
	}
	if model == "" {
		model = requestedModel
	}
	if model == "" {
		model = "unknown"
	}
	cost := calculateCost(model, inputTokens, outputTokens)

	stats.RecordUsage(provider.ID, provider.Name, model, "stream", "claude", inputTokens, outputTokens, cost, duration, timeToFirst, keyID, clientIP)
	history.AddRecord(history.RequestRecord{
		ID: requestID, Timestamp: startTime, Method: method, Path: path,
		ClientIP: clientIP, KeyID: keyID, Provider: provider.Name, Model: model,
		StatusCode: resp.StatusCode, Duration: duration,
		RequestBody: requestBody, ResponseBody: responseBody.String(),
		RequestHeaders: requestHeaders, ResponseHeaders: resp.Header,
		RequestSize: int64(len(requestBody)), ResponseSize: int64(responseBody.Len()),
		InputTokens: inputTokens, OutputTokens: outputTokens,
		TotalTokens: inputTokens + outputTokens, Cost: cost,
	})
}

// ============================================================
// 非流式响应处理
// ============================================================

func handleNonStreamResponse(c *gin.Context, resp *http.Response, provider *config.Provider, requestID string, startTime time.Time, method, path, requestBody string, requestHeaders http.Header, requestedModel string, keyID, clientIP string, isIncomingOpenAIFormat bool) {
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Error("❌ 读取响应体失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read response"})
		return
	}

	logger.Info("📥 响应信息 - Status: %d, Content-Type: %s, Content-Encoding: %s, Body大小: %d",
		resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Content-Encoding"), len(respBody))

	// 解压
	contentEncoding := resp.Header.Get("Content-Encoding")
	if contentEncoding != "" && contentEncoding != "identity" {
		if decompressed, err := decompressResponse(bytes.NewReader(respBody), contentEncoding); err == nil {
			respBody = decompressed
		} else {
			logger.Error("❌ 解压失败: %v, Content-Encoding: %s", err, contentEncoding)
		}
	}

	model := requestedModel
	var inputTokens, outputTokens int
	var responseBodyForHistory string

	if resp.StatusCode == 200 && len(respBody) > 0 {
		var result struct {
			Model string `json:"model"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(respBody, &result); err != nil {
			logger.Error("❌ JSON解析失败: %v", err)
		} else {
			if result.Model != "" {
				model = result.Model
			}
			inputTokens = result.Usage.InputTokens
			outputTokens = result.Usage.OutputTokens
		}
		responseBodyForHistory = string(respBody)
	}

	if model == "" {
		model = "unknown"
	}

	duration := time.Since(startTime).Milliseconds()
	cost := calculateCost(model, inputTokens, outputTokens)
	stats.RecordUsage(provider.ID, provider.Name, model, "non-stream", "claude",
		inputTokens, outputTokens, cost, duration, 0, keyID, clientIP)
	history.AddRecord(history.RequestRecord{
		ID: requestID, Timestamp: startTime, Method: method, Path: path,
		ClientIP: clientIP, KeyID: keyID, Provider: provider.Name, Model: model,
		StatusCode: resp.StatusCode, Duration: duration,
		RequestBody: requestBody, ResponseBody: responseBodyForHistory,
		RequestHeaders: requestHeaders, ResponseHeaders: resp.Header,
		RequestSize: int64(len(requestBody)), ResponseSize: int64(len(respBody)),
		InputTokens: inputTokens, OutputTokens: outputTokens,
		TotalTokens: inputTokens + outputTokens, Cost: cost,
	})

	// 格式转换（非流式）
	needsConversion := provider.IsOpenAIFormat != isIncomingOpenAIFormat
	if needsConversion && resp.StatusCode == 200 && len(respBody) > 0 {
		if !provider.IsOpenAIFormat && isIncomingOpenAIFormat {
			var claudeResp map[string]interface{}
			if json.Unmarshal(respBody, &claudeResp) == nil {
				openaiResp := convertClaudeToOpenAIResponse(claudeResp)
				if converted, err := json.Marshal(openaiResp); err == nil {
					respBody = converted
				}
			}
		} else if provider.IsOpenAIFormat && !isIncomingOpenAIFormat {
			var openaiResp map[string]interface{}
			if json.Unmarshal(respBody, &openaiResp) == nil {
				claudeResp := convertOpenAIToClaude(openaiResp)
				if converted, err := json.Marshal(claudeResp); err == nil {
					respBody = converted
				}
			}
		}
	}

	// 透传
	for key, values := range resp.Header {
		if key == "Content-Encoding" || key == "Content-Length" {
			continue
		}
		for _, value := range values {
			c.Header(key, value)
		}
	}
	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), respBody)
}

// ============================================================
// 解压
// ============================================================

func decompressResponse(body io.Reader, contentEncoding string) ([]byte, error) {
	switch strings.ToLower(contentEncoding) {
	case "gzip":
		gzReader, err := gzip.NewReader(body)
		if err != nil {
			return nil, err
		}
		defer gzReader.Close()
		return io.ReadAll(gzReader)
	case "deflate":
		flateReader := flate.NewReader(body)
		defer flateReader.Close()
		return io.ReadAll(flateReader)
	case "zlib":
		zlibReader, err := zlib.NewReader(body)
		if err != nil {
			return nil, err
		}
		defer zlibReader.Close()
		return io.ReadAll(zlibReader)
	case "br":
		return io.ReadAll(brotli.NewReader(body))
	default:
		return io.ReadAll(body)
	}
}

// ============================================================
// 费用计算
// ============================================================

func calculateCost(model string, inputTokens, outputTokens int) float64 {
	var inputCost, outputCost float64
	switch {
	case strings.Contains(model, "opus"):
		inputCost, outputCost = 0.000015, 0.000075
	case strings.Contains(model, "sonnet"):
		inputCost, outputCost = 0.000003, 0.000015
	case strings.Contains(model, "haiku"):
		inputCost, outputCost = 0.00000025, 0.00000125
	default:
		inputCost, outputCost = 0.000003, 0.000015
	}
	return float64(inputTokens)*inputCost + float64(outputTokens)*outputCost
}

// ============================================================
// 格式转换函数
// ============================================================

func convertClaudeToOpenAI(claudeReq map[string]interface{}) map[string]interface{} {
	openaiReq := make(map[string]interface{})
	if model, ok := claudeReq["model"]; ok {
		openaiReq["model"] = model
	}
	if stream, ok := claudeReq["stream"]; ok {
		openaiReq["stream"] = stream
	}
	if temp, ok := claudeReq["temperature"]; ok {
		openaiReq["temperature"] = temp
	}
	if topP, ok := claudeReq["top_p"]; ok {
		openaiReq["top_p"] = topP
	}
	if maxTokens, ok := claudeReq["max_tokens"]; ok {
		openaiReq["max_tokens"] = maxTokens
	}

	messages := []interface{}{}
	if system, ok := claudeReq["system"].(string); ok && system != "" {
		messages = append(messages, map[string]interface{}{
			"role":    "system",
			"content": system,
		})
	}
	if claudeMessages, ok := claudeReq["messages"].([]interface{}); ok {
		messages = append(messages, claudeMessages...)
	}
	openaiReq["messages"] = messages
	return openaiReq
}

func convertOpenAIToClaude(openaiResp map[string]interface{}) map[string]interface{} {
	claudeResp := make(map[string]interface{})
	if id, ok := openaiResp["id"]; ok {
		claudeResp["id"] = id
	}
	if model, ok := openaiResp["model"]; ok {
		claudeResp["model"] = model
	}
	claudeResp["type"] = "message"
	claudeResp["role"] = "assistant"

	if choices, ok := openaiResp["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if message, ok := choice["message"].(map[string]interface{}); ok {
				if content, ok := message["content"].(string); ok {
					claudeResp["content"] = []map[string]interface{}{
						{"type": "text", "text": content},
					}
				}
			}
			if finishReason, ok := choice["finish_reason"]; ok {
				claudeResp["stop_reason"] = finishReason
			}
		}
	}

	if usage, ok := openaiResp["usage"].(map[string]interface{}); ok {
		claudeUsage := make(map[string]interface{})
		if promptTokens, ok := usage["prompt_tokens"]; ok {
			claudeUsage["input_tokens"] = promptTokens
		}
		if completionTokens, ok := usage["completion_tokens"]; ok {
			claudeUsage["output_tokens"] = completionTokens
		}
		claudeResp["usage"] = claudeUsage
	}
	return claudeResp
}

func convertClaudeToOpenAIResponse(claudeResp map[string]interface{}) map[string]interface{} {
	openaiResp := make(map[string]interface{})
	if id, ok := claudeResp["id"]; ok {
		openaiResp["id"] = id
	}
	if model, ok := claudeResp["model"]; ok {
		openaiResp["model"] = model
	}
	openaiResp["object"] = "chat.completion"
	openaiResp["created"] = time.Now().Unix()

	var content string
	if contentArr, ok := claudeResp["content"].([]interface{}); ok && len(contentArr) > 0 {
		if firstContent, ok := contentArr[0].(map[string]interface{}); ok {
			if text, ok := firstContent["text"].(string); ok {
				content = text
			}
		}
	}

	var finishReason string
	if stopReason, ok := claudeResp["stop_reason"].(string); ok {
		finishReason = stopReason
	}

	openaiResp["choices"] = []map[string]interface{}{
		{
			"index": 0,
			"message": map[string]interface{}{
				"role":    "assistant",
				"content": content,
			},
			"finish_reason": finishReason,
		},
	}

	if usage, ok := claudeResp["usage"].(map[string]interface{}); ok {
		openaiUsage := make(map[string]interface{})
		if inputTokens, ok := usage["input_tokens"]; ok {
			openaiUsage["prompt_tokens"] = inputTokens
		}
		if outputTokens, ok := usage["output_tokens"]; ok {
			openaiUsage["completion_tokens"] = outputTokens
		}
		if inputTokens, ok := usage["input_tokens"].(int); ok {
			if outputTokens, ok := usage["output_tokens"].(int); ok {
				openaiUsage["total_tokens"] = inputTokens + outputTokens
			}
		}
		openaiResp["usage"] = openaiUsage
	}
	return openaiResp
}

func convertOpenAIStreamToClaude(openaiChunk map[string]interface{}) map[string]interface{} {
	claudeChunk := make(map[string]interface{})
	if id, ok := openaiChunk["id"]; ok {
		claudeChunk["id"] = id
	}
	if model, ok := openaiChunk["model"]; ok {
		claudeChunk["model"] = model
	}

	if choices, ok := openaiChunk["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if delta, ok := choice["delta"].(map[string]interface{}); ok {
				if content, ok := delta["content"].(string); ok && content != "" {
					claudeChunk["type"] = "content_block_delta"
					claudeChunk["delta"] = map[string]interface{}{
						"type": "text_delta",
						"text": content,
					}
				} else if role, ok := delta["role"].(string); ok {
					claudeChunk["type"] = "message_start"
					claudeChunk["message"] = map[string]interface{}{
						"id": claudeChunk["id"], "type": "message",
						"role": role, "model": claudeChunk["model"],
					}
				}
			}
			if finishReason, ok := choice["finish_reason"]; ok && finishReason != nil {
				claudeChunk["type"] = "message_delta"
				claudeChunk["delta"] = map[string]interface{}{
					"stop_reason": finishReason,
				}
			}
		}
	}

	if usage, ok := openaiChunk["usage"].(map[string]interface{}); ok {
		claudeUsage := make(map[string]interface{})
		if promptTokens, ok := usage["prompt_tokens"].(float64); ok {
			claudeUsage["input_tokens"] = int(promptTokens)
		}
		if completionTokens, ok := usage["completion_tokens"].(float64); ok {
			claudeUsage["output_tokens"] = int(completionTokens)
		}
		claudeChunk["usage"] = claudeUsage
	}
	return claudeChunk
}

func convertOpenAIToClaudeRequest(openaiReq map[string]interface{}) map[string]interface{} {
	claudeReq := make(map[string]interface{})
	if model, ok := openaiReq["model"]; ok {
		claudeReq["model"] = model
	}
	if stream, ok := openaiReq["stream"]; ok {
		claudeReq["stream"] = stream
	}
	if temp, ok := openaiReq["temperature"]; ok {
		claudeReq["temperature"] = temp
	}
	if topP, ok := openaiReq["top_p"]; ok {
		claudeReq["top_p"] = topP
	}
	if maxTokens, ok := openaiReq["max_tokens"]; ok {
		claudeReq["max_tokens"] = maxTokens
	}

	var systemMsg string
	var claudeMessages []interface{}
	if messages, ok := openaiReq["messages"].([]interface{}); ok {
		for _, msg := range messages {
			if msgMap, ok := msg.(map[string]interface{}); ok {
				role, _ := msgMap["role"].(string)
				content, _ := msgMap["content"].(string)
				if role == "system" {
					systemMsg = content
				} else {
					claudeMessages = append(claudeMessages, msgMap)
				}
			}
		}
	}
	if systemMsg != "" {
		claudeReq["system"] = systemMsg
	}
	claudeReq["messages"] = claudeMessages
	return claudeReq
}

func convertClaudeStreamToOpenAI(claudeChunk map[string]interface{}) map[string]interface{} {
	openaiChunk := make(map[string]interface{})
	if id, ok := claudeChunk["id"]; ok {
		openaiChunk["id"] = id
	}
	if model, ok := claudeChunk["model"]; ok {
		openaiChunk["model"] = model
	}
	openaiChunk["object"] = "chat.completion.chunk"

	chunkType, _ := claudeChunk["type"].(string)
	var choices []interface{}

	switch chunkType {
	case "message_start":
		if msg, ok := claudeChunk["message"].(map[string]interface{}); ok {
			choices = []interface{}{
				map[string]interface{}{
					"index":         0,
					"delta":         map[string]interface{}{"role": msg["role"]},
					"finish_reason": nil,
				},
			}
		}
	case "content_block_delta":
		if delta, ok := claudeChunk["delta"].(map[string]interface{}); ok {
			deltaType, _ := delta["type"].(string)
			if deltaType == "text_delta" {
				if text, ok := delta["text"].(string); ok {
					choices = []interface{}{
						map[string]interface{}{
							"index":         0,
							"delta":         map[string]interface{}{"content": text},
							"finish_reason": nil,
						},
					}
				}
			}
		}
	case "message_delta":
		if delta, ok := claudeChunk["delta"].(map[string]interface{}); ok {
			if stopReason, ok := delta["stop_reason"].(string); ok {
				choices = []interface{}{
					map[string]interface{}{
						"index":         0,
						"delta":         map[string]interface{}{},
						"finish_reason": stopReason,
					},
				}
			}
		}
	}
	openaiChunk["choices"] = choices

	if usage, ok := claudeChunk["usage"].(map[string]interface{}); ok {
		openaiUsage := make(map[string]interface{})
		if inputTokens, ok := usage["input_tokens"].(float64); ok {
			openaiUsage["prompt_tokens"] = int(inputTokens)
		}
		if outputTokens, ok := usage["output_tokens"].(float64); ok {
			openaiUsage["completion_tokens"] = int(outputTokens)
		}
		openaiUsage["total_tokens"] = 0
		openaiChunk["usage"] = openaiUsage
	}
	return openaiChunk
}
