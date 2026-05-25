package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"switchai/logger"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ProxyError 代理错误，携带 HTTP 状态码和原始响应体
type ProxyError struct {
	StatusCode int // 0 表示无法获取状态码
	Err        error
	Body       []byte // 上游返回的原始错误响应体
}

func (e *ProxyError) Error() string {
	if e.StatusCode > 0 {
		return fmt.Sprintf("status %d: %v", e.StatusCode, e.Err)
	}
	return e.Err.Error()
}

func (e *ProxyError) Unwrap() error {
	return e.Err
}

// NewProxyError 创建代理错误
func NewProxyError(statusCode int, err error) error {
	if err == nil {
		return nil
	}
	return &ProxyError{StatusCode: statusCode, Err: err}
}

// NewProxyErrorWithBody 创建带有响应体的代理错误
func NewProxyErrorWithBody(statusCode int, err error, body []byte) error {
	if err == nil {
		return nil
	}
	return &ProxyError{StatusCode: statusCode, Err: err, Body: body}
}

// GetStatusCode 从错误中提取状态码
func GetStatusCode(err error) int {
	if err == nil {
		return 0
	}
	if proxyErr, ok := err.(*ProxyError); ok {
		return proxyErr.StatusCode
	}
	return 0
}

// ErrorClassification 错误分类枚举
type ErrorClassification int

const (
	ClassificationRetryable    ErrorClassification = iota // 可重试错误（换provider可能成功）
	ClassificationNonRetryable                            // 不可重试错误（客户端请求问题，换provider也会失败）
	ClassificationUnknown                                 // 未知错误
)

// NonRetryableStatuses 不可重试的 HTTP 状态码
// 这些状态码表示客户端请求自身有问题，换 provider 也会被拒绝
var NonRetryableStatuses = map[int]bool{
	400: true, // Bad Request - 参数格式错误
	// 401/403: 不加入，因为可能key失效或权限问题，换provider可能成功
	405: true, // Method Not Allowed
	406: true, // Not Acceptable
	413: true, // Payload Too Large
	414: true, // URI Too Long
	415: true, // Unsupported Media Type
	422: true, // Unprocessable Entity - 请求语义错误
	501: true, // Not Implemented
}

// RetryableStatuses 默认可重试的状态码（不在 NonRetryableStatuses 中的）
// 包括：认证错误(401/403)、配额限制(429)、服务端错误(5xx)
var RetryableStatuses = map[int]bool{
	0:   true, // 无法获取状态码，可能是网络错误等，默认认为可重试
	401: true, // Unauthorized - key可能失效
	403: true, // Forbidden - 权限问题，换key可能成功
	404: true, // Not Found - 模型可能不存在
	429: true, // Too Many Requests - 配额限制
	500: true, // Internal Server Error
	502: true, // Bad Gateway
	503: true, // Service Unavailable
	504: true, // Gateway Timeout
	529: true, // Bandwidth Limit Exceeded
}

// ClassifyError 根据 HTTP 状态码分类错误
func ClassifyError(statusCode int) ErrorClassification {
	if NonRetryableStatuses[statusCode] {
		return ClassificationNonRetryable
	}
	if RetryableStatuses[statusCode] || statusCode >= 500 {
		return ClassificationRetryable
	}
	return ClassificationUnknown
}

// truncateError 截断过长的错误消息
func truncateError(msg string) string {
	if len(msg) > 500 {
		return msg[:500] + "..."
	}
	return msg
}

// errorResponse 用于保存上游返回的错误响应
type errorResponse struct {
	StatusCode int
	Body       []byte
}

// writeErrorResponse 将上游错误响应写入客户端
// 根据请求格式（OpenAI/Anthropic）决定返回格式
func writeErrorResponse(c *gin.Context, errResp *errorResponse, isIncomingOpenAIFormat bool) {
	if errResp == nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Unknown error"})
		return
	}

	// 设置响应头
	c.Header("Content-Type", "application/json")

	// 如果响应体为空或解析失败，构造一个简单的错误响应
	if len(errResp.Body) == 0 {
		if isIncomingOpenAIFormat {
			c.JSON(errResp.StatusCode, gin.H{
				"error": gin.H{
					"message": fmt.Sprintf("Request failed with status %d", errResp.StatusCode),
					"type":    "api_error",
				},
			})
		} else {
			c.JSON(errResp.StatusCode, gin.H{
				"type":  "error",
				"error": gin.H{"type": "api_error", "message": fmt.Sprintf("Request failed with status %d", errResp.StatusCode)},
			})
		}
		return
	}

	// 尝试解析上游响应，确保格式正确
	var upstreamErr map[string]interface{}
	if json.Unmarshal(errResp.Body, &upstreamErr) == nil {
		// 上游响应是有效 JSON，直接转发
		c.Data(errResp.StatusCode, "application/json", errResp.Body)
	} else {
		// 上游响应不是有效 JSON，构造标准错误响应
		if isIncomingOpenAIFormat {
			c.JSON(errResp.StatusCode, gin.H{
				"error": gin.H{
					"message": string(errResp.Body),
					"type":    "api_error",
				},
			})
		} else {
			c.JSON(errResp.StatusCode, gin.H{
				"type":  "error",
				"error": gin.H{"type": "api_error", "message": string(errResp.Body)},
			})
		}
	}
}

// extractErrorResponseBody 从错误中提取响应体和状态码
func extractErrorResponseBody(err error) (statusCode int, body []byte) {
	if err == nil {
		return 0, nil
	}
	if proxyErr, ok := err.(*ProxyError); ok {
		return proxyErr.StatusCode, proxyErr.Body
	}
	return 0, nil
}

// handlerSetup 存储 handler 初始化的公共上下文
type handlerSetup struct {
	requestID string
	entry     *ConnProxyEntry
	mgr       *ConnProxyManager
	bodyBytes []byte
	ok        bool
}

// setupHandler 执行 handler 公共初始化：认证、获取 proxy、读取 body
func setupHandler(c *gin.Context, providerType string) *handlerSetup {
	s := &handlerSetup{}
	s.requestID = uuid.New().String()

	keyID, clientIP, ok := authenticate(c)
	if !ok {
		logger.Warn("Authentication failed for client IP %s", c.ClientIP())
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication failed"})
		return s
	}

	s.mgr = GetGlobalConnProxyManager()
	var err error
	s.entry, err = s.mgr.GetOrCreate(c.Request.RemoteAddr, providerType)
	if err != nil {
		logger.Error("Failed to get or create CcProxy: %v", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Failed to initialize proxy"})
		return s
	}

	s.bodyBytes, err = io.ReadAll(c.Request.Body)
	if err != nil {
		logger.Error("Failed to read request body: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read request body"})
		return s
	}

	c.Set("keyID", keyID)
	c.Set("clientIP", clientIP)
	c.Set("request_id", s.requestID)
	s.ok = true
	return s
}

// handleError 处理代理调用错误，包含故障切换重试逻辑
func handleError(c *gin.Context, s *handlerSetup, providerType string, err error, statusCode int, modelName string, isOpenAIFormat bool, retry func(newEntry *ConnProxyEntry) (error, int)) {
	isRetryable := ClassifyError(statusCode) == ClassificationRetryable
	logger.Error("[Error] provider=%s model=%s status=%d classification=%s error=%s",
		s.entry.Proxy.Provider().Name, modelName, statusCode,
		map[bool]string{true: "retryable", false: "non-retryable"}[isRetryable],
		truncateError(err.Error()))

	errStatusCode, errBody := extractErrorResponseBody(err)
	if errStatusCode == 0 {
		errStatusCode = statusCode
	}

	if isRetryable {
		newEntry, switchErr := s.mgr.SwitchProvider(c.Request.RemoteAddr, providerType, s.entry.providerIdx+1)
		if switchErr == nil {
			retryErr, retryStatusCode := retry(newEntry)
			if retryErr == nil {
				return
			}
			retryIsRetryable := ClassifyError(retryStatusCode) == ClassificationRetryable
			logger.Error("[SwitchProvider] retry failed status=%d classification=%s error=%s",
				retryStatusCode,
				map[bool]string{true: "retryable", false: "non-retryable"}[retryIsRetryable],
				truncateError(retryErr.Error()))
			if !retryIsRetryable {
				s.mgr.Remove(c.Request.RemoteAddr)
			}
			retryStatusCode2, retryBody := extractErrorResponseBody(retryErr)
			if retryStatusCode2 > 0 {
				errStatusCode, errBody = retryStatusCode2, retryBody
			}
		} else {
			logger.Error("[SwitchProvider] failed to switch: %s", switchErr.Error())
			s.mgr.Remove(c.Request.RemoteAddr)
		}
	}
	writeErrorResponse(c, &errorResponse{StatusCode: errStatusCode, Body: errBody}, isOpenAIFormat)
}

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
