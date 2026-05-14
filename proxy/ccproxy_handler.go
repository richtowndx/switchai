package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"switchai/history"
	"switchai/logger"
	"switchai/stats"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// baseProxyHandlerWithCcProxy 使用 CcProxy 接口的代理处理逻辑
// 优化：Handler 只负责认证和传递原始请求，所有转换由 Proxy 内部处理
func baseProxyHandlerWithCcProxy(c *gin.Context, cfg *handlerConfig) {
	startTime := time.Now()
	requestID := uuid.New().String()

	// 1. 认证
	keyID, clientIP, ok := authenticate(c)
	if !ok {
		return
	}

	logger.Info("Incoming request: ID=%s, Method=%s, Path=%s, Format=%s, RemoteAddr=%s",
		requestID, c.Request.Method, c.Request.URL.Path, cfg.formatName, c.Request.RemoteAddr)

	// 2. 获取或创建连接级别的 CcProxy entry
	mgr := GetGlobalConnProxyManager()
	entry, err := mgr.GetOrCreate(c.Request.RemoteAddr, nil)
	if err != nil {
		logger.Error("Failed to get or create CcProxy: %v", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Failed to initialize proxy"})
		return
	}

	logger.Info("📡 使用 CcProxy: %s (format: isOpenAI=%v, isCopilot=%v, requests: %d)",
		entry.Provider.Name, entry.Provider.IsOpenAIFormat, entry.Provider.IsCopilot(), entry.RequestCount)

	// 3. 读取原始请求体（不做任何修改）
	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		logger.Error("Failed to read request body: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read request body"})
		return
	}

	// 4. 根据请求格式选择调用方式（传递原始请求头和请求体）
	ctx := context.Background()
	if cfg.isIncomingOpenAIFormat {
		entry.handleOpenAIRequest(ctx, c, c.Request.Header, bodyBytes, requestID, startTime, keyID, clientIP)
	} else {
		entry.handleAnthropicRequest(ctx, c, c.Request.Header, bodyBytes, requestID, startTime, keyID, clientIP)
	}
}

// handleOpenAIRequest 处理 OpenAI 格式请求
func (e *ConnProxyEntry) handleOpenAIRequest(ctx context.Context, c *gin.Context, reqHdr http.Header, reqBody []byte, requestID string, startTime time.Time, keyID, clientIP string) {
	// 调用 Proxy（内部处理模型映射、格式转换、流式判断）
	resp := e.Proxy.SendOpenAIFormat(ctx, reqHdr, reqBody)

	if resp.Error != nil {
		logger.Error("❌ Proxy request failed: %v", resp.Error)
		c.JSON(http.StatusBadGateway, gin.H{"error": "Proxy request failed: " + resp.Error.Error()})
		return
	}

	// 根据响应类型处理（使用 resp.ModelName）
	if resp.IsStream {
		e.handleStreamResponse(c, resp.StreamCh, resp.ErrCh, resp.ModelName, requestID, startTime, keyID, clientIP)
	} else {
		e.handleNonStreamResponse(c, resp.Body, resp.ModelName, requestID, startTime, keyID, clientIP)
	}
}

// handleAnthropicRequest 处理 Anthropic 格式请求
func (e *ConnProxyEntry) handleAnthropicRequest(ctx context.Context, c *gin.Context, reqHdr http.Header, reqBody []byte, requestID string, startTime time.Time, keyID, clientIP string) {
	// 调用 Proxy（内部处理模型映射、格式转换、流式判断）
	resp := e.Proxy.SendAnthropicFormat(ctx, reqHdr, reqBody)

	if resp.Error != nil {
		logger.Error("❌ Proxy request failed: %v", resp.Error)
		c.JSON(http.StatusBadGateway, gin.H{"error": "Proxy request failed: " + resp.Error.Error()})
		return
	}

	// 根据响应类型处理（使用 resp.ModelName）
	if resp.IsStream {
		e.handleStreamResponse(c, resp.StreamCh, resp.ErrCh, resp.ModelName, requestID, startTime, keyID, clientIP)
	} else {
		e.handleNonStreamResponse(c, resp.Body, resp.ModelName, requestID, startTime, keyID, clientIP)
	}
}

// handleNonStreamResponse 处理非流式响应
func (e *ConnProxyEntry) handleNonStreamResponse(c *gin.Context, respBody []byte, modelName string, requestID string, startTime time.Time, keyID, clientIP string) {
	// 解析响应获取 token 统计
	_, inputTokens, outputTokens := parseTokenStats(respBody)

	// 使用 Proxy 提供的模型名（如果为空则使用 "unknown"）
	model := modelName
	if model == "" {
		model = "unknown"
	}

	// 设置响应头并返回（直接使用 []byte，避免额外拷贝）
	c.Header("Content-Type", "application/json")
	c.Data(200, "application/json", respBody)

	// 记录统计
	duration := time.Since(startTime).Milliseconds()
	cost := calculateCost(model, inputTokens, outputTokens)

	stats.RecordUsage(e.Provider.ID, e.Provider.Name, model, "non-stream", "ccproxy",
		inputTokens, outputTokens, cost, duration, 0, keyID, clientIP)

	history.AddRecord(history.RequestRecord{
		ID:              requestID,
		Timestamp:       startTime,
		Method:          c.Request.Method,
		Path:            c.Request.URL.Path,
		ClientIP:        clientIP,
		KeyID:           keyID,
		Provider:        e.Provider.Name,
		Model:           model,
		StatusCode:      200,
		Duration:        duration,
		RequestBody:     "",
		ResponseHeaders: make(http.Header),
		InputTokens:     inputTokens,
		OutputTokens:    outputTokens,
		TotalTokens:     inputTokens + outputTokens,
		Cost:            cost,
	})

	logger.Info("✅ 非流式响应完成: Model=%s, Tokens=%d+%d, Cost=$%.4f, Duration=%dms",
		model, inputTokens, outputTokens, cost, duration)
}

// handleStreamResponse 处理流式响应
func (e *ConnProxyEntry) handleStreamResponse(c *gin.Context, ch <-chan string, errCh <-chan error, modelName string, requestID string, startTime time.Time, keyID, clientIP string) {
	// 设置流式响应头
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		logger.Error("Streaming not supported")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Streaming not supported"})
		return
	}

	var firstTokenTime time.Time
	firstToken := true
	var inputTokens, outputTokens int

	// 处理流式响应
	for {
		select {
		case line, ok := <-ch:
			if !ok {
				goto done
			}

			if firstToken {
				firstTokenTime = time.Now()
				firstToken = false
			}

			// 直接转发 SSE 行
			c.Writer.WriteString(line)
			flusher.Flush()

			// 提取 token 统计
			inputTokens, outputTokens = extractTokensFromSSE(line, inputTokens, outputTokens)

		case err := <-errCh:
			if err != nil {
				logger.Error("❌ Stream error: %v", err)
				return
			}
			goto done
		}
	}

done:
	// 使用 Proxy 提供的模型名（如果为空则使用 "unknown"）
	model := modelName
	if model == "" {
		model = "unknown"
	}

	// 记录统计
	duration := time.Since(startTime).Milliseconds()
	var timeToFirst int64
	if !firstTokenTime.IsZero() {
		timeToFirst = firstTokenTime.Sub(startTime).Milliseconds()
	}

	cost := calculateCost(model, inputTokens, outputTokens)

	stats.RecordUsage(e.Provider.ID, e.Provider.Name, model, "stream", "ccproxy",
		inputTokens, outputTokens, cost, duration, timeToFirst, keyID, clientIP)

	history.AddRecord(history.RequestRecord{
		ID:              requestID,
		Timestamp:       startTime,
		Method:          c.Request.Method,
		Path:            c.Request.URL.Path,
		ClientIP:        clientIP,
		KeyID:           keyID,
		Provider:        e.Provider.Name,
		Model:           model,
		StatusCode:      200,
		Duration:        duration,
		RequestBody:     "",
		ResponseHeaders: make(http.Header),
		InputTokens:     inputTokens,
		OutputTokens:    outputTokens,
		TotalTokens:     inputTokens + outputTokens,
		Cost:            cost,
	})

	logger.Info("✅ 流式响应完成: Model=%s, Tokens=%d+%d, Cost=$%.4f, Duration=%dms, TTFB=%dms",
		model, inputTokens, outputTokens, cost, duration, timeToFirst)
}

// ============================================================
// 辅助函数
// ============================================================

// parseTokenStats 从响应体中解析 token 统计（不再解析 model）
func parseTokenStats(respBody []byte) (string, int, int) {
	var resp map[string]interface{}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return "", 0, 0
	}

	inputTokens, outputTokens := 0, 0

	// 尝试 OpenAI 格式
	if usage, ok := resp["usage"].(map[string]interface{}); ok {
		if pt, ok := usage["prompt_tokens"].(float64); ok {
			inputTokens = int(pt)
		}
		if ct, ok := usage["completion_tokens"].(float64); ok {
			outputTokens = int(ct)
		}
	}

	// 尝试 Anthropic 格式
	if inputTokens == 0 && outputTokens == 0 {
		if usage, ok := resp["usage"].(map[string]interface{}); ok {
			if it, ok := usage["input_tokens"].(float64); ok {
				inputTokens = int(it)
			}
			if ot, ok := usage["output_tokens"].(float64); ok {
				outputTokens = int(ot)
			}
		}
	}

	return "", inputTokens, outputTokens
}

// extractTokensFromSSE 从 SSE 行中提取 token 统计（不再提取 model）
func extractTokensFromSSE(line string, inputTokens, outputTokens int) (int, int) {
	if !strings.HasPrefix(line, "data: ") {
		return inputTokens, outputTokens
	}

	data := strings.TrimPrefix(line, "data: ")
	if data == "[DONE]" {
		return inputTokens, outputTokens
	}

	var event map[string]interface{}
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return inputTokens, outputTokens
	}

	// OpenAI 格式
	if usage, ok := event["usage"].(map[string]interface{}); ok {
		if pt, ok := usage["prompt_tokens"].(float64); ok && pt > 0 {
			inputTokens = int(pt)
		}
		if ct, ok := usage["completion_tokens"].(float64); ok && ct > 0 {
			outputTokens = int(ct)
		}
	}

	// Anthropic 格式
	if inputTokens == 0 && outputTokens == 0 {
		if usage, ok := event["usage"].(map[string]interface{}); ok {
			if it, ok := usage["input_tokens"].(float64); ok && it > 0 {
				inputTokens = int(it)
			}
			if ot, ok := usage["output_tokens"].(float64); ok && ot > 0 {
				outputTokens = int(ot)
			}
		}
	}

	return inputTokens, outputTokens
}
