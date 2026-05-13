package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"switchai/config"
	"switchai/logger"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ============================================================
// Copilot 专用代理处理器
// ============================================================

// copilotProxyHandler 处理 Copilot 专用代理请求
// Copilot 使用 OpenAI 兼容格式，但需要特殊的认证 headers
func copilotProxyHandler(c *gin.Context) {
	startTime := time.Now()
	requestID := uuid.New().String()

	// 1. 认证
	keyID, clientIP, ok := authenticate(c)
	if !ok {
		return
	}

	// 2. 读取请求体
	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read request body"})
		return
	}

	// 3. 基于 client IP:PORT 计算 hash
	clientHash := hashClientRemote(c.Request.RemoteAddr)
	logger.Info("Incoming Copilot request: ID=%s, Method=%s, Path=%s, ClientHash=%d",
		requestID, c.Request.Method, c.Request.URL.Path, clientHash)

	// 4. 获取 Copilot provider（首次尝试，不重试）
	provider := config.GetConfig().GetClientHashedProvider(clientHash, 0)
	if provider == nil || !provider.IsCopilot() {
		logger.Error("❌ No Copilot provider configured for client hash %d", clientHash)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "No Copilot provider configured"})
		return
	}

	if provider == nil {
		logger.Error("❌ No Copilot provider configured")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "No Copilot provider configured"})
		return
	}

	logger.Info("📡 Copilot Provider: %s, BaseURL: %s", provider.Name, provider.BaseURL)

	// 5. 获取/刷新 Copilot token
	copilotToken := RefreshCopilotToken(provider)
	if copilotToken == "" {
		logger.Error("❌ Copilot token not available for provider %s", provider.Name)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Copilot token is not available. Please authorize GitHub Copilot in the web interface first."})
		return
	}

	// 6. 构建目标 URL
	targetURL := CopilotTargetURL(provider)
	if c.Request.URL.RawQuery != "" {
		targetURL += "?" + c.Request.URL.RawQuery
	}

	logger.Info("📡 代理转发到 Copilot: %s", targetURL)

	// 7. 注入 Copilot headers
	copilotHeaders := InjectCopilotHeaders(c.Request.Header, copilotToken)

	// 8. 检测是否为流式请求
	var requestBody map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &requestBody); err == nil {
		if stream, ok := requestBody["stream"].(bool); ok && stream {
			// 流式请求
			sendStreamRequest(c, "POST", targetURL, copilotHeaders, bodyBytes,
				provider, requestID, startTime, keyID, clientIP)
			return
		}
	}

	// 9. 非流式请求
	resp, err := sendHTTPRequest(c.Request.Method, targetURL, copilotHeaders, bodyBytes)
	if err != nil {
		logger.Error("❌ Copilot request failed: %v", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "Copilot request failed"})
		return
	}
	defer resp.Body.Close()

	// 10. 转发响应
	for key, values := range resp.Header {
		for _, value := range values {
			c.Header(key, value)
		}
	}
	c.Status(resp.StatusCode)

	// 复制响应体
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Error("❌ Failed to read Copilot response: %v", err)
		return
	}

	if _, err := c.Writer.Write(responseBody); err != nil {
		logger.Error("❌ Failed to write response: %v", err)
		return
	}

	// TODO: 记录统计 (需要 token 计数)
	logger.Info("✅ Copilot request completed: status=%d, duration=%dms", resp.StatusCode, time.Since(startTime).Milliseconds())
}

// sendStreamRequest 发送流式请求并转发 SSE 响应
func sendStreamRequest(c *gin.Context, method, targetURL string, headers http.Header, body []byte,
	provider *config.Provider, requestID string, startTime time.Time, keyID, clientIP string) {

	c.Status(http.StatusOK)
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		logger.Error("Streaming not supported")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Streaming not supported"})
		return
	}

	// 创建请求
	req, err := http.NewRequest(method, targetURL, bytes.NewReader(body))
	if err != nil {
		logger.Error("❌ Failed to create stream request: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create request"})
		return
	}

	// 设置 headers
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	// 发送请求
	client := &http.Client{Timeout: 0} // 无超时，支持长时间流
	resp, err := client.Do(req)
	if err != nil {
		logger.Error("❌ Stream request failed: %v", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "Stream request failed"})
		return
	}
	defer resp.Body.Close()

	// 转发 SSE 流
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		c.Writer.Write([]byte(line))
		c.Writer.Write([]byte("\n"))
		flusher.Flush()
	}

	if err := scanner.Err(); err != nil {
		logger.Error("❌ Stream scan error: %v", err)
	}

	logger.Info("✅ Copilot stream completed: duration=%dms", time.Since(startTime).Milliseconds())
}

// sendHTTPRequest 发送普通 HTTP 请求
func sendHTTPRequest(method, targetURL string, headers http.Header, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(method, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	client := &http.Client{Timeout: 120 * time.Second}
	return client.Do(req)
}
