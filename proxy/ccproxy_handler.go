package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"switchai/logger"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// shouldSwitchProvider 判断错误是否为需要切换 provider 的错误
// 包括：401/403 认证错误、400 参数/模型错误（避免重复失败）
func shouldSwitchProvider(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "status 401") ||
		strings.Contains(msg, "status 403") ||
		strings.Contains(msg, "status 400")
}

// getStatusCodeFromError 从错误信息中提取 HTTP 状态码
func getStatusCodeFromError(err error) int {
	if err == nil {
		return 0
	}
	msg := err.Error()
	// 查找 "status N" 模式
	for i := 0; i < len(msg)-7; i++ {
		if msg[i:i+6] == "status " {
			j := i + 6
			code := 0
			for j < len(msg) && msg[j] >= '0' && msg[j] <= '9' {
				code = code*10 + int(msg[j]-'0')
				j++
			}
			if code > 0 {
				return code
			}
		}
	}
	return 0
}

// baseProxyHandlerWithCcProxy 使用 CcProxy 接口的代理处理逻辑
// 优化：Handler 只负责认证和传递原始请求，所有转换由 Proxy 内部处理
func baseProxyHandlerWithCcProxy(c *gin.Context, cfg *handlerConfig) {
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
	// HandleXxxFormat 内部处理：模型映射、格式转换、发送请求、响应转发、统计记录
	// 将认证信息和请求ID存储到 gin.Context 中，供 Proxy 方法读取
	c.Set("keyID", keyID)
	c.Set("clientIP", clientIP)
	c.Set("request_id", requestID)

	ctx := context.Background()
	if cfg.isIncomingOpenAIFormat {
		if err := entry.Proxy.HandleOpenAIFormat(ctx, c, c.Request.Header, bodyBytes); err != nil {
			logger.Error("❌ HandleOpenAIFormat failed: %v", err)
			if shouldSwitchProvider(err) {
				logger.Error("[ConnProxyManager] 切换 provider (错误 %d) 并重试: %s -> %s", getStatusCodeFromError(err), c.Request.RemoteAddr, entry.Provider.Name)
				newEntry, switchErr := mgr.SwitchProvider(c.Request.RemoteAddr, entry.providerIdx+1)
				if switchErr == nil {
					logger.Info("📡 切换到新 CcProxy: %s", newEntry.Provider.Name)
					if retryErr := newEntry.Proxy.HandleOpenAIFormat(ctx, c, c.Request.Header, bodyBytes); retryErr == nil {
						return
					} else {
						logger.Error("❌ 重试 HandleOpenAIFormat 仍失败 (provider=%s): %v", newEntry.Provider.Name, retryErr)
						if shouldSwitchProvider(retryErr) {
							mgr.Remove(c.Request.RemoteAddr)
						}
					}
				} else {
					mgr.Remove(c.Request.RemoteAddr)
				}
			}
			c.JSON(http.StatusBadGateway, gin.H{"error": "Request failed: " + err.Error()})
		}
	} else {
		if err := entry.Proxy.HandleAnthropicFormat(ctx, c, c.Request.Header, bodyBytes); err != nil {
			logger.Error("❌ HandleAnthropicFormat failed: %v", err)
			if shouldSwitchProvider(err) {
				logger.Error("[ConnProxyManager] 切换 provider (错误 %d) 并重试: %s -> %s", getStatusCodeFromError(err), c.Request.RemoteAddr, entry.Provider.Name)
				newEntry, switchErr := mgr.SwitchProvider(c.Request.RemoteAddr, entry.providerIdx+1)
				if switchErr == nil {
					logger.Info("📡 切换到新 CcProxy: %s", newEntry.Provider.Name)
					if retryErr := newEntry.Proxy.HandleAnthropicFormat(ctx, c, c.Request.Header, bodyBytes); retryErr == nil {
						return
					} else {
						logger.Error("❌ 重试 HandleAnthropicFormat 仍失败 (provider=%s): %v", newEntry.Provider.Name, retryErr)
						if shouldSwitchProvider(retryErr) {
							mgr.Remove(c.Request.RemoteAddr)
						}
					}
				} else {
					mgr.Remove(c.Request.RemoteAddr)
				}
			}
			c.JSON(http.StatusBadGateway, gin.H{"error": "Request failed: " + err.Error()})
		}
	}
}

// ============================================================
// 辅助函数
// ============================================================

// GetAuthInfo 从 gin.Context 中获取认证信息
func GetAuthInfo(c *gin.Context) (keyID, clientIP string) {
	keyID = ""
	if v, exists := c.Get("keyID"); exists {
		if k, ok := v.(string); ok {
			keyID = k
		}
	}
	clientIP = ""
	if v, exists := c.Get("clientIP"); exists {
		if ip, ok := v.(string); ok {
			clientIP = ip
		}
	}
	return keyID, clientIP
}

// parseTokenStats 从响应体中解析 token 统计（不再解析 model）
func parseTokenStats(respBody []byte) (string, int, int, int) {
	var resp map[string]interface{}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return "", 0, 0, 0
	}

	inputTokens, outputTokens, cacheReadTokens := 0, 0, 0

	// 从 usage 对象中提取所有 token 字段
	// 同时支持 OpenAI 格式 (prompt_tokens/completion_tokens) 和 Anthropic 格式 (input_tokens/output_tokens)
	if usage, ok := resp["usage"].(map[string]interface{}); ok {
		// OpenAI 格式
		if pt, ok := usage["prompt_tokens"].(float64); ok && pt > 0 {
			inputTokens = int(pt)
		}
		if ct, ok := usage["completion_tokens"].(float64); ok && ct > 0 {
			outputTokens = int(ct)
		}
		// Anthropic 格式（优先级更高，会覆盖 OpenAI 格式的值）
		if it, ok := usage["input_tokens"].(float64); ok && it > 0 {
			inputTokens = int(it)
		}
		if ot, ok := usage["output_tokens"].(float64); ok && ot > 0 {
			outputTokens = int(ot)
		}
		// cache_read_input_tokens（两种格式通用）
		if crt, ok := usage["cache_read_input_tokens"].(float64); ok && crt > 0 {
			cacheReadTokens = int(crt)
		}
		// OpenAI 格式的 prompt_tokens_details.cached_tokens
		if ptd, ok := usage["prompt_tokens_details"].(map[string]interface{}); ok {
			if ct, ok := ptd["cached_tokens"].(float64); ok && ct > 0 && cacheReadTokens == 0 {
				cacheReadTokens = int(ct)
			}
		}
	}

	return "", inputTokens, outputTokens, cacheReadTokens
}

// parseUsageFromJSON 从 JSON 字符串中解析 usage（OpenAI 或 Anthropic 格式）
func parseUsageFromJSON(data string, inputTokens, outputTokens, cacheReadTokens int) (int, int, int) {
	var event map[string]interface{}
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return inputTokens, outputTokens, cacheReadTokens
	}

	// 从 usage 对象中提取所有 token 字段
	// 同时支持 OpenAI 格式 (prompt_tokens/completion_tokens) 和 Anthropic 格式 (input_tokens/output_tokens)
	if usage, ok := event["usage"].(map[string]interface{}); ok {
		// OpenAI 格式
		if pt, ok := usage["prompt_tokens"].(float64); ok && pt > 0 {
			inputTokens = int(pt)
		}
		if ct, ok := usage["completion_tokens"].(float64); ok && ct > 0 {
			outputTokens = int(ct)
		}
		// Anthropic 格式（优先级更高，会覆盖 OpenAI 格式的值）
		if it, ok := usage["input_tokens"].(float64); ok && it > 0 {
			inputTokens = int(it)
		}
		if ot, ok := usage["output_tokens"].(float64); ok && ot > 0 {
			outputTokens = int(ot)
		}
		// cache_read_input_tokens（两种格式通用）
		if crt, ok := usage["cache_read_input_tokens"].(float64); ok && crt > 0 {
			cacheReadTokens = int(crt)
		}
		// OpenAI 格式的 prompt_tokens_details.cached_tokens
		if ptd, ok := usage["prompt_tokens_details"].(map[string]interface{}); ok {
			if ct, ok := ptd["cached_tokens"].(float64); ok && ct > 0 && cacheReadTokens == 0 {
				cacheReadTokens = int(ct)
			}
		}
	}

	return inputTokens, outputTokens, cacheReadTokens
}

// extractTokensFromSSE 从 SSE 行中提取 token 统计
// 优化 1: 区分 Anthropic SSE 事件类型（message_start 只取 input_tokens，message_delta 只取 output_tokens）
// 优化 2: 处理非标准情况（上游在流式请求中直接返回非流式 JSON）
func extractTokensFromSSE(line string, inputTokens, outputTokens, cacheReadTokens int) (int, int, int) {
	line = strings.TrimSpace(line)

	// 检查是否为空行
	if line == "" {
		return inputTokens, outputTokens, cacheReadTokens
	}

	// 1. 标准 SSE 格式
	var data string
	var isSSE bool

	// 兼容两种 SSE 格式: "data: " (带空格) 和 "data:" (不带空格)
	if strings.HasPrefix(line, "data: ") {
		data = strings.TrimPrefix(line, "data: ")
		isSSE = true
		if data == "[DONE]" {
			return inputTokens, outputTokens, cacheReadTokens
		}
	} else if strings.HasPrefix(line, "data:") {
		data = strings.TrimPrefix(line, "data:")
		isSSE = true
		if data == "[DONE]" {
			return inputTokens, outputTokens, cacheReadTokens
		}
	} else {
		// 2. 非标准情况：上游在流式请求中直接返回非流式 JSON
		// 检测是否为 JSON 对象（以 { 开头）
		if strings.HasPrefix(line, "{") && strings.HasSuffix(line, "}") {
			data = line
			isSSE = false
		} else {
			return inputTokens, outputTokens, cacheReadTokens
		}
	}

	var event map[string]interface{}
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return inputTokens, outputTokens, cacheReadTokens
	}

	// 优化: 区分 Anthropic SSE 事件类型，精确捕获
	if isSSE {
		if eventType, ok := event["type"].(string); ok {
			switch eventType {
				case "message_start":
					// 不提取 token，所有 token 数据由 message_delta 提供
					return inputTokens, outputTokens, cacheReadTokens

				case "message_delta":
					// message_delta 的 input_tokens 是非缓存输入，output_tokens 和 cache_read_input_tokens 是累计值
					if usage, ok := event["usage"].(map[string]interface{}); ok {
						if it, ok := usage["input_tokens"].(float64); ok {
							inputTokens = int(it)
						}
						if ot, ok := usage["output_tokens"].(float64); ok {
							outputTokens = int(ot)
						}
						if crt, ok := usage["cache_read_input_tokens"].(float64); ok {
							cacheReadTokens = int(crt)
						}
					}
					return inputTokens, outputTokens, cacheReadTokens
			}
		}
	}

	// 3. OpenAI 格式或其他格式（非 Anthropic SSE 事件）
	// 兼容处理：同时检查 prompt_tokens/completion_tokens 和 input_tokens/output_tokens
	if usage, ok := event["usage"].(map[string]interface{}); ok {
		foundOpenAI := false
		// OpenAI 格式
		if pt, ok := usage["prompt_tokens"].(float64); ok && pt > 0 {
			inputTokens = int(pt)
			foundOpenAI = true
		}
		if ct, ok := usage["completion_tokens"].(float64); ok && ct > 0 {
			outputTokens = int(ct)
			foundOpenAI = true
		}
		// Anthropic 格式（当 OpenAI 格式没有匹配到时）
		if !foundOpenAI {
			if it, ok := usage["input_tokens"].(float64); ok && it > 0 {
				inputTokens = int(it)
			}
			if ot, ok := usage["output_tokens"].(float64); ok && ot > 0 {
				outputTokens = int(ot)
			}
		}
		// cache tokens
		if crt, ok := usage["cache_read_input_tokens"].(float64); ok && crt > 0 {
			cacheReadTokens = int(crt)
		}
		if ptd, ok := usage["prompt_tokens_details"].(map[string]interface{}); ok {
			if ct, ok := ptd["cached_tokens"].(float64); ok && ct > 0 && cacheReadTokens == 0 {
				cacheReadTokens = int(ct)
			}
		}
	}

	return inputTokens, outputTokens, cacheReadTokens
}
