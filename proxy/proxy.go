package proxy

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/url"
	"strings"
	"switchai/config"
	"switchai/history"
	"switchai/logger"
	"switchai/stats"

	"time"

	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
)

const maxProviderRetries = 1 // 简化：只尝试一次，不重试

// RegisterRoutes 注册所有代理路由
// 在路由注册时就区分服务类型：Anthropic/OpenAI 格式使用 /v1/ 前缀，Copilot 使用 /copilot/ 或 /chat/completions
func RegisterRoutes(r *gin.Engine) {
	// OpenAI 格式 API 路由
	r.Any("/v1/chat/completions", openAIHandler)
	r.POST("/v1/completions", openAIHandler)

	// Anthropic 格式 API 路由
	r.POST("/v1/messages", anthropicHandler)

	// Copilot 专用路由 (不含 /v1 前缀)
	r.POST("/chat/completions", copilotHandler)
	r.Any("/copilot/*path", copilotHandler)

	// Codex/Responses API 路由
	r.POST("/responses", codexHandler)
	r.POST("/v1/responses", codexHandler)
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

		// Copilot 模型处理：归一化 + 兼容性回退
		if provider.IsCopilot() {
			normalized := NormalizeCopilotModelID(resolved)
			if normalized != resolved {
				logger.Info("Copilot model normalization: %s → %s", resolved, normalized)
			}
			chatModel := ResolveCopilotModel(normalized)
			if chatModel != normalized {
				logger.Info("Copilot model fallback: %s → %s", normalized, chatModel)
			}
			requestBody["model"] = chatModel
			result.RequestedModel = chatModel
		}
	}

	// 格式转换（4 种情况）：
	//   openai → openai : 不转换，URL = /chat/completions
	//   openai → claude : 转换，URL = /v1/messages
	//   claude → openai : 转换，URL = /chat/completions
	//   claude → claude : 不转换，URL = /v1/messages
	// 目标 URL 始终由 provider 自身格式决定；仅在格式不匹配时转换请求体。
	result.TargetURL = buildTargetURLForConversion(provider, provider.IsOpenAIFormat)
	if provider.IsOpenAIFormat != isIncomingOpenAIFormat {
		if provider.IsOpenAIFormat {
			// 传入 Claude 格式 → provider 需要 OpenAI 格式
			logger.Info("Converting Claude request to OpenAI format")
			requestBody = convertClaudeToOpenAI(requestBody)
		} else {
			// 传入 OpenAI 格式 → provider 需要 Claude 格式
			logger.Info("Converting OpenAI request to Claude format")
			requestBody = convertOpenAIToClaudeRequest(requestBody)
		}
	}

	// Copilot 提供商：固定目标 URL 为 Copilot Chat Completions
	if provider.IsCopilot() {
		result.TargetURL = CopilotTargetURL(provider)
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
func sendRequest(method, targetURL string, originalHeaders http.Header, bodyBytes []byte, provider *config.Provider, copilotToken string) (*http.Response, error) {
	req, err := http.NewRequest(method, targetURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}

	if copilotToken != "" {
		// Copilot 提供商：注入 Copilot 专用 header
		for k, v := range InjectCopilotHeaders(originalHeaders, copilotToken) {
			for _, val := range v {
				req.Header.Add(k, val)
			}
		}
	} else {
		// 普通提供商：复制请求头（跳过原始 Authorization / Content-Type / Content-Length）
		// Content-Length 不转发：请求体可能经过格式转换，长度已变，由 http 客户端重新计算
		for key, values := range originalHeaders {
			if key == "Authorization" || key == "Content-Type" || key == "Content-Length" {
				continue
			}
			for _, value := range values {
				req.Header.Add(key, value)
			}
		}

		// 设置供应商的 API Key
		req.Header.Set("Authorization", "Bearer "+provider.APIKey)
		req.Header.Set("Content-Type", "application/json")
	}

	// 创建 HTTP client（如果 provider 配置了代理则使用代理）
	client := http.DefaultClient
	if provider.ProxyURL != "" {
		proxyURL, err := url.Parse(provider.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy URL %q: %w", provider.ProxyURL, err)
		}
		client = &http.Client{
			Transport: &http.Transport{
				Proxy: http.ProxyURL(proxyURL),
			},
		}
	}

	resp, err := client.Do(req)
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

// hashClientRemote 将客户端 IP:PORT 哈希为 uint64，用于 provider 选择。
// 相同 IP:PORT 始终得到相同 hash 值，保证客户端亲和性。
func hashClientRemote(remoteAddr string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(remoteAddr))
	return h.Sum64()
}

// ============================================================
// 主入口：特定格式的 Proxy Handler
// ============================================================

// openAIHandler 处理 OpenAI 格式的请求
func openAIHandler(c *gin.Context) {
	s := setupHandler(c, "openai")
	if !s.ok {
		return
	}

	err, statusCode := s.entry.Proxy.HandleOpenAIFormat(context.Background(), c, c.Request.Header, s.bodyBytes)
	if err != nil {
		handleError(c, s, "openai", err, statusCode, "OpenAI", true,
			func(newEntry *ConnProxyEntry) (error, int) {
				return newEntry.Proxy.HandleOpenAIFormat(context.Background(), c, c.Request.Header, s.bodyBytes)
			})
	}
}

// anthropicHandler 处理 Anthropic/Claude 格式的请求
func anthropicHandler(c *gin.Context) {
	s := setupHandler(c, "anthropic")
	if !s.ok {
		return
	}

	err, statusCode := s.entry.Proxy.HandleAnthropicFormat(context.Background(), c, c.Request.Header, s.bodyBytes)
	if err != nil {
		handleError(c, s, "anthropic", err, statusCode, "Anthropic", false,
			func(newEntry *ConnProxyEntry) (error, int) {
				return newEntry.Proxy.HandleAnthropicFormat(context.Background(), c, c.Request.Header, s.bodyBytes)
			})
	}
}

// copilotHandler 处理 Copilot 格式的请求
func copilotHandler(c *gin.Context) {
	s := setupHandler(c, "copilot")
	if !s.ok {
		return
	}

	err, statusCode := s.entry.Proxy.HandleAnthropicFormat(context.Background(), c, c.Request.Header, s.bodyBytes)
	if err != nil {
		handleError(c, s, "copilot", err, statusCode, "Copilot", false,
			func(newEntry *ConnProxyEntry) (error, int) {
				return newEntry.Proxy.HandleAnthropicFormat(context.Background(), c, c.Request.Header, s.bodyBytes)
			})
	}
}

// codexHandler 处理 Codex/Responses API 格式的请求
func codexHandler(c *gin.Context) {
	s := setupHandler(c, "openai")
	if !s.ok {
		return
	}
	err, statusCode := s.entry.Proxy.HandleCodexFormat(context.Background(), c, c.Request.Header, s.bodyBytes)
	if err != nil {
		handleError(c, s, "openai", err, statusCode, "Codex", true,
			func(newEntry *ConnProxyEntry) (error, int) {
				return newEntry.Proxy.HandleCodexFormat(context.Background(), c, c.Request.Header, s.bodyBytes)
			})
	}
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

	var inputTokens, outputTokens, cacheReadTokens int
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
						// token 提取在通用逻辑中处理
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
						// token 提取在通用逻辑中处理
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
		}

		// 提取 token 统计（所有格式通用）
		lineStr := string(line)
		lineStr = strings.TrimSpace(lineStr)

		if strings.HasPrefix(lineStr, "data: ") {
			data := strings.TrimPrefix(lineStr, "data: ")
			if data == "[DONE]" {
				// continue
			} else {
				var streamData map[string]interface{}
				if err := json.Unmarshal([]byte(data), &streamData); err == nil {
					if m, ok := streamData["model"].(string); ok && m != "" {
						model = m
					}
					// Distinguish Anthropic SSE event types
					if eventType, ok := streamData["type"].(string); ok {
						switch eventType {
						case "message_start":
							// 不提取 token，所有 token 数据由 message_delta 提供
						case "message_delta":
							if usage, ok := streamData["usage"].(map[string]interface{}); ok {
								if ot, ok := usage["output_tokens"].(float64); ok {
									outputTokens = int(ot)
								}
								if it, ok := usage["input_tokens"].(float64); ok {
									inputTokens = int(it)
								}
								if crt, ok := usage["cache_read_input_tokens"].(float64); ok {
									cacheReadTokens = int(crt)
								}
							}
						default:
							// OpenAI or other format: extract usage directly
							if usage, ok := streamData["usage"].(map[string]interface{}); ok {
								if input, ok := usage["input_tokens"].(float64); ok {
									inputTokens = int(input)
								}
								if output, ok := usage["output_tokens"].(float64); ok {
									outputTokens = int(output)
								}
								if cache, ok := usage["cache_read_input_tokens"].(float64); ok {
									cacheReadTokens = int(cache)
								}
							}
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
	cost := calculateCost(model, inputTokens, outputTokens, cacheReadTokens)

	logZeroTokenDebug(provider.Name, requestID, requestBody, responseBody.String(), inputTokens, outputTokens)

	stats.RecordUsage(provider.ID, provider.Name, model, "stream", "claude", inputTokens, outputTokens, cacheReadTokens, cost, duration, timeToFirst, keyID, clientIP)
	history.AddRecord(history.RequestRecord{
		ID: requestID, Timestamp: startTime, Method: method, Path: path,
		ClientIP: clientIP, KeyID: keyID, Provider: provider.Name, Model: model,
		StatusCode: resp.StatusCode, Duration: duration,
		RequestBody: requestBody, ResponseBody: responseBody.String(),
		RequestHeaders: requestHeaders, ResponseHeaders: resp.Header,
		RequestSize: int64(len(requestBody)), ResponseSize: int64(responseBody.Len()),
		InputTokens: inputTokens, OutputTokens: outputTokens,
		CacheReadInputTokens: cacheReadTokens, TotalTokens: inputTokens + outputTokens + cacheReadTokens, Cost: cost,
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
	var inputTokens, outputTokens, cacheReadTokens int
	var responseBodyForHistory string

	if resp.StatusCode == 200 && len(respBody) > 0 {
		var result struct {
			Model string `json:"model"`
			Usage struct {
				InputTokens          int `json:"input_tokens"`
				OutputTokens         int `json:"output_tokens"`
				CacheReadInputTokens int `json:"cache_read_input_tokens"`
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
			cacheReadTokens = result.Usage.CacheReadInputTokens

		}
		responseBodyForHistory = string(respBody)
	}

	if model == "" {
		model = "unknown"
	}

	duration := time.Since(startTime).Milliseconds()
	cost := calculateCost(model, inputTokens, outputTokens, cacheReadTokens)
	logZeroTokenDebug(provider.Name, requestID, requestBody, responseBodyForHistory, inputTokens, outputTokens)

	stats.RecordUsage(provider.ID, provider.Name, model, "non-stream", "claude",
		inputTokens, outputTokens, cacheReadTokens, cost, duration, 0, keyID, clientIP)
	history.AddRecord(history.RequestRecord{
		ID: requestID, Timestamp: startTime, Method: method, Path: path,
		ClientIP: clientIP, KeyID: keyID, Provider: provider.Name, Model: model,
		StatusCode: resp.StatusCode, Duration: duration,
		RequestBody: requestBody, ResponseBody: responseBodyForHistory,
		RequestHeaders: requestHeaders, ResponseHeaders: resp.Header,
		RequestSize: int64(len(requestBody)), ResponseSize: int64(len(respBody)),
		InputTokens: inputTokens, OutputTokens: outputTokens,
		CacheReadInputTokens: cacheReadTokens, TotalTokens: inputTokens + outputTokens + cacheReadTokens, Cost: cost,
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

// logZeroTokenDebug 在 input 或 output token 计数为 0 时记录请求和应答
func logZeroTokenDebug(providerName, requestID, requestBody, responseBody string, inputTokens, outputTokens int) {
	if inputTokens > 0 && outputTokens > 0 {
		return
	}
	logger.Info("⚠️ 零Token请求 | Provider=%s | RequestID=%s | InputTokens=%d | OutputTokens=%d | Request=%s | Response=%s",
		providerName, requestID, inputTokens, outputTokens, requestBody, responseBody)
}

// ============================================================
// 费用计算（统一定价，不区分模型）
// 输入: 3元/百万token, 缓存读取: 0.025元/百万token, 输出: 6元/百万token
// ============================================================

func calculateCost(model string, inputTokens, outputTokens, cacheReadTokens int) float64 {
	const (
		inputPricePerToken  = 3.0 / 1_000_000   // 输入: 3元/百万
		cachePricePerToken  = 0.025 / 1_000_000 // 缓存读取: 0.025元/百万
		outputPricePerToken = 6.0 / 1_000_000   // 输出: 6元/百万
	)
	return float64(inputTokens)*inputPricePerToken + float64(cacheReadTokens)*cachePricePerToken + float64(outputTokens)*outputPricePerToken
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
		openaiReq["max_completion_tokens"] = maxTokens
	}

	// Convert tools: Anthropic format → OpenAI function calling format
	if tools, ok := claudeReq["tools"].([]interface{}); ok {
		var openaiTools []interface{}
		for _, t := range tools {
			if tm, ok := t.(map[string]interface{}); ok {
				oaTool := map[string]interface{}{
					"type": "function",
					"function": map[string]interface{}{
						"name":        tm["name"],
						"description": tm["description"],
					},
				}
				if schema, ok := tm["input_schema"]; ok {
					oaTool["function"].(map[string]interface{})["parameters"] = schema
				}
				openaiTools = append(openaiTools, oaTool)
			}
		}
		if len(openaiTools) > 0 {
			openaiReq["tools"] = openaiTools
		}
	}

	messages := []interface{}{}

	// System prompt: handle both string and array formats
	if system, ok := claudeReq["system"]; ok {
		var systemText string
		switch s := system.(type) {
		case string:
			systemText = s
		case []interface{}:
			var parts []string
			for _, block := range s {
				if bm, ok := block.(map[string]interface{}); ok {
					if t, ok := bm["text"].(string); ok {
						parts = append(parts, t)
					}
				}
			}
			systemText = strings.Join(parts, "\n")
		}
		if systemText != "" {
			messages = append(messages, map[string]interface{}{
				"role":    "system",
				"content": systemText,
			})
		}
	}

	// Convert messages: Anthropic content blocks → OpenAI format
	if claudeMessages, ok := claudeReq["messages"].([]interface{}); ok {
		for _, msg := range claudeMessages {
			msgMap, ok := msg.(map[string]interface{})
			if !ok {
				continue
			}
			role, _ := msgMap["role"].(string)
			content := msgMap["content"]

			switch role {
			case "user":
				converted := convertClaudeUserMsg(content)
				messages = append(messages, converted...)
			case "assistant":
				converted := convertClaudeAssistantMsg(content)
				messages = append(messages, converted)
			default:
				messages = append(messages, msgMap)
			}
		}
	}
	openaiReq["messages"] = messages
	return openaiReq
}

// convertClaudeUserMsg converts a Claude user message content to OpenAI format.
// Handles: text blocks → string, tool_result → separate tool role messages, image → image_url.
func convertClaudeUserMsg(content interface{}) []interface{} {
	// Simple string content
	if s, ok := content.(string); ok {
		return []interface{}{map[string]interface{}{"role": "user", "content": s}}
	}

	blocks, ok := content.([]interface{})
	if !ok {
		return []interface{}{map[string]interface{}{"role": "user", "content": content}}
	}

	var textParts []string
	var toolResults []interface{}
	var contentParts []interface{} // for multimodal

	for _, block := range blocks {
		bm, ok := block.(map[string]interface{})
		if !ok {
			continue
		}
		blockType, _ := bm["type"].(string)

		switch blockType {
		case "text":
			if text, ok := bm["text"].(string); ok {
				textParts = append(textParts, text)
			}
		case "tool_result":
			toolResults = append(toolResults, bm)
		case "image":
			if src, ok := bm["source"].(map[string]interface{}); ok {
				url := ""
				if t, ok := src["type"].(string); ok && t == "base64" {
					if mt, ok := src["media_type"].(string); ok {
						if d, ok := src["data"].(string); ok {
							url = "data:" + mt + ";base64," + d
						}
					}
				} else if t, ok := src["type"].(string); ok && t == "url" {
					url, _ = src["url"].(string)
				}
				if url != "" {
					contentParts = append(contentParts, map[string]interface{}{
						"type":      "image_url",
						"image_url": map[string]interface{}{"url": url},
					})
				}
			}
		}
	}

	var result []interface{}

	// Build the user message
	if len(toolResults) == 0 {
		// No tool results: simple user message
		allText := strings.Join(textParts, "\n")
		if len(contentParts) > 0 {
			// Multimodal: text + images
			var parts []interface{}
			if allText != "" {
				parts = append(parts, map[string]interface{}{"type": "text", "text": allText})
			}
			parts = append(parts, contentParts...)
			result = append(result, map[string]interface{}{"role": "user", "content": parts})
		} else {
			result = append(result, map[string]interface{}{"role": "user", "content": allText})
		}
	} else {
		// Has tool results: user message with text, then tool result messages
		if len(textParts) > 0 {
			result = append(result, map[string]interface{}{
				"role":    "user",
				"content": strings.Join(textParts, "\n"),
			})
		}
		for _, tr := range toolResults {
			trMap, _ := tr.(map[string]interface{})
			toolUseID, _ := trMap["tool_use_id"].(string)
			trContent := ""
			if c, ok := trMap["content"].(string); ok {
				trContent = c
			} else if cArr, ok := trMap["content"].([]interface{}); ok {
				var cParts []string
				for _, cb := range cArr {
					if cbm, ok := cb.(map[string]interface{}); ok {
						if t, ok := cbm["text"].(string); ok {
							cParts = append(cParts, t)
						}
					}
				}
				trContent = strings.Join(cParts, "\n")
			}
			result = append(result, map[string]interface{}{
				"role":         "tool",
				"tool_call_id": toolUseID,
				"content":      trContent,
			})
		}
	}

	if len(result) == 0 {
		return []interface{}{map[string]interface{}{"role": "user", "content": ""}}
	}
	return result
}

// convertClaudeAssistantMsg converts a Claude assistant message content to OpenAI format.
// Strips thinking/redacted_thinking blocks, converts tool_use → tool_calls.
func convertClaudeAssistantMsg(content interface{}) interface{} {
	if s, ok := content.(string); ok {
		return map[string]interface{}{"role": "assistant", "content": s}
	}

	blocks, ok := content.([]interface{})
	if !ok {
		return map[string]interface{}{"role": "assistant", "content": content}
	}

	var textParts []string
	var toolCalls []interface{}

	for _, block := range blocks {
		bm, ok := block.(map[string]interface{})
		if !ok {
			continue
		}
		blockType, _ := bm["type"].(string)

		switch blockType {
		case "text":
			if text, ok := bm["text"].(string); ok {
				textParts = append(textParts, text)
			}
		case "tool_use":
			id, _ := bm["id"].(string)
			name, _ := bm["name"].(string)
			input := bm["input"]
			// OpenAI requires arguments as JSON string
			argsJSON, _ := json.Marshal(input)
			toolCalls = append(toolCalls, map[string]interface{}{
				"id":   id,
				"type": "function",
				"function": map[string]interface{}{
					"name":      name,
					"arguments": string(argsJSON),
				},
			})
			// Skip thinking/redacted_thinking
		}
	}

	msg := map[string]interface{}{
		"role":    "assistant",
		"content": strings.Join(textParts, "\n"),
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	return msg
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
				var contentBlocks []interface{}

				// Text content
				if text, ok := message["content"].(string); ok && text != "" {
					contentBlocks = append(contentBlocks, map[string]interface{}{
						"type": "text", "text": text,
					})
				}

				// Tool calls → tool_use blocks
				if toolCalls, ok := message["tool_calls"].([]interface{}); ok {
					for _, tc := range toolCalls {
						if tcm, ok := tc.(map[string]interface{}); ok {
							fn, _ := tcm["function"].(map[string]interface{})
							name, _ := fn["name"].(string)
							args := fn["arguments"]
							tcID, _ := tcm["id"].(string)
							contentBlocks = append(contentBlocks, map[string]interface{}{
								"type":  "tool_use",
								"id":    tcID,
								"name":  name,
								"input": args,
							})
						}
					}
				}

				if len(contentBlocks) == 0 {
					contentBlocks = append(contentBlocks, map[string]interface{}{
						"type": "text", "text": "",
					})
				}
				claudeResp["content"] = contentBlocks

				// Map finish reasons
				if fr, ok := choice["finish_reason"].(string); ok {
					switch fr {
					case "tool_calls":
						claudeResp["stop_reason"] = "tool_use"
					case "stop":
						claudeResp["stop_reason"] = "end_turn"
					default:
						claudeResp["stop_reason"] = fr
					}
				}
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
