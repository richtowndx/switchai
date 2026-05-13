package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"switchai/config"
)

func init() {
	RegisterProxyFactory("copilot", NewCopilotProxy)
}

// CopilotProxy 实现 Copilot API 代理
// Copilot 使用 OpenAI 兼容的 API 格式，但需要特殊的认证 headers
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
			Timeout: 120 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}, nil
}

// SendMessage 发送非流式消息请求
func (p *CopilotProxy) SendMessage(ctx context.Context, req *UnifiedRequest) (*UnifiedResponse, error) {
	// 获取/刷新 Copilot token
	copilotToken := RefreshCopilotToken(p.provider)
	if copilotToken == "" {
		return nil, fmt.Errorf("failed to get copilot token")
	}

	// 构建 OpenAI 格式请求体
	body := map[string]interface{}{
		"model":      p.resolveModel(req.Model),
		"messages":   p.convertMessages(req.Messages),
		"max_tokens": req.MaxTokens,
	}

	if req.Temperature > 0 {
		body["temperature"] = req.Temperature
	}
	if req.TopP > 0 {
		body["top_p"] = req.TopP
	}
	if len(req.Tools) > 0 {
		body["tools"] = p.convertTools(req.Tools)
		body["tool_choice"] = "auto"
	}

	// 发送请求
	resp, err := p.doRequest(ctx, body, false, copilotToken)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 解析响应 (OpenAI 格式)
	var result openAIResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("api error: %s", result.Error.Message)
	}

	return p.convertToUnifiedResponse(&result), nil
}

// SendMessageStream 发送流式消息请求
func (p *CopilotProxy) SendMessageStream(ctx context.Context, req *UnifiedRequest) (<-chan *StreamChunk, <-chan error) {
	chunkCh := make(chan *StreamChunk, 16)
	errCh := make(chan error, 1)

	go func() {
		defer close(chunkCh)
		defer close(errCh)

		// 获取/刷新 Copilot token
		copilotToken := RefreshCopilotToken(p.provider)
		if copilotToken == "" {
			errCh <- fmt.Errorf("failed to get copilot token")
			return
		}

		// 构建 OpenAI 格式请求体
		body := map[string]interface{}{
			"model":      p.resolveModel(req.Model),
			"messages":   p.convertMessages(req.Messages),
			"max_tokens": req.MaxTokens,
			"stream":     true,
		}

		if req.Temperature > 0 {
			body["temperature"] = req.Temperature
		}
		if req.TopP > 0 {
			body["top_p"] = req.TopP
		}
		if len(req.Tools) > 0 {
			body["tools"] = p.convertTools(req.Tools)
			body["tool_choice"] = "auto"
		}

		// 发送请求
		resp, err := p.doRequest(ctx, body, true, copilotToken)
		if err != nil {
			errCh <- err
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			var errResp openAIResponse
			json.NewDecoder(resp.Body).Decode(&errResp)
			errCh <- fmt.Errorf("api error: %s", errResp.Error.Message)
			return
		}

		// 解析 SSE 流 (OpenAI 格式)
		scanner := bufio.NewScanner(resp.Body)
		var currentToolUse *ToolUse

		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}

			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				chunkCh <- &StreamChunk{
					Type: ChunkTypeUsage,
					Usage: &Usage{},
					Done:  true,
				}
				return
			}

			var chunk openAIStreamChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}

			outChunk := p.convertStreamChunk(&chunk, &currentToolUse)
			if outChunk != nil {
				chunkCh <- outChunk
			}
		}

		if err := scanner.Err(); err != nil {
			errCh <- fmt.Errorf("scan stream: %w", err)
		}
	}()

	return chunkCh, errCh
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
// 转换函数
// ============================================================

func (p *CopilotProxy) convertMessages(messages []Message) []map[string]interface{} {
	result := make([]map[string]interface{}, len(messages))
	for i, msg := range messages {
		result[i] = map[string]interface{}{
			"role": msg.Role,
		}

		if msg.ToolResult != nil {
			result[i]["content"] = msg.ToolResult.Content
			result[i]["tool_call_id"] = msg.ToolResult.ToolUseID
		} else if len(msg.ContentBlocks) > 0 {
			content := make([]map[string]interface{}, len(msg.ContentBlocks))
			for j, block := range msg.ContentBlocks {
				if block.Type == "text" {
					content[j] = map[string]interface{}{
						"type": "text",
						"text": block.Text,
					}
				} else if block.Type == "image" {
					content[j] = map[string]interface{}{
						"type": "image_url",
						"image_url": map[string]interface{}{
							"url": block.Source.Data,
						},
					}
				}
			}
			result[i]["content"] = content
		} else {
			result[i]["content"] = msg.Content
		}
	}
	return result
}

func (p *CopilotProxy) convertTools(tools []Tool) []map[string]interface{} {
	result := make([]map[string]interface{}, len(tools))
	for i, tool := range tools {
		result[i] = map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        tool.Name,
				"description": tool.Description,
				"parameters":  tool.InputSchema,
			},
		}
	}
	return result
}

func (p *CopilotProxy) resolveModel(model string) string {
	// 首先归一化 Claude 模型 ID
	normalized := NormalizeCopilotModelID(model)
	// 然后解析为 Copilot 兼容的模型
	return ResolveCopilotModel(normalized)
}

func (p *CopilotProxy) convertToUnifiedResponse(resp *openAIResponse) *UnifiedResponse {
	result := &UnifiedResponse{
		ID:      resp.ID,
		Model:   resp.Model,
		Role:    "assistant",
		Content: "",
		ContentBlocks: make([]ContentBlock, 0),
	}

	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		if choice.Message.Content != nil {
			result.Content = *choice.Message.Content
		}

		if len(choice.Message.ToolCalls) > 0 {
			for _, tc := range choice.Message.ToolCalls {
				var input map[string]interface{}
				if tc.Function.Arguments != "" {
					json.Unmarshal([]byte(tc.Function.Arguments), &input)
				}
				result.ToolUse = &ToolUse{
					ID:       tc.ID,
					Name:     tc.Function.Name,
					Input:    input,
					Metadata: map[string]string{},
				}
			}
		}

		result.StopReason = string(choice.FinishReason)
	}

	if resp.Usage.TotalTokens > 0 {
		result.Usage = &Usage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
		}
	}

	return result
}

func (p *CopilotProxy) convertStreamChunk(chunk *openAIStreamChunk, currentToolUse **ToolUse) *StreamChunk {
	if len(chunk.Choices) == 0 {
		return nil
	}

	choice := chunk.Choices[0]
	delta := choice.Delta

	// 文本内容
	if delta.Content != nil && *delta.Content != "" {
		return &StreamChunk{
			Type:  ChunkTypeContent,
			Delta: *delta.Content,
		}
	}

	// 工具调用
	if len(delta.ToolCalls) > 0 {
		for _, tc := range delta.ToolCalls {
			if tc.Index != nil && *tc.Index == 0 {
				if tc.ID != nil && *tc.ID != "" {
					*currentToolUse = &ToolUse{
						ID:        *tc.ID,
						Name:      "",
						Input:     make(map[string]interface{}),
						Metadata:  make(map[string]string),
					}
					if tc.Function.Name != nil {
						(*currentToolUse).Name = *tc.Function.Name
					}
					return &StreamChunk{
						Type:    ChunkTypeToolUseStart,
						ToolUse: *currentToolUse,
					}
				}

				// 工具调用参数增量
				if tc.Function.Arguments != nil && *tc.Function.Arguments != "" && *currentToolUse != nil {
					return &StreamChunk{
						Type: ChunkTypeToolUseDelta,
					}
				}
			}
		}
	}

	// 流结束
	if choice.FinishReason != "" {
		return &StreamChunk{
			Type: ChunkTypeUsage,
			Usage: &Usage{},
			Done:  true,
		}
	}

	return nil
}

func (p *CopilotProxy) doRequest(ctx context.Context, body map[string]interface{}, stream bool, copilotToken string) (*http.Response, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	targetURL := CopilotTargetURL(p.provider)

	req, err := http.NewRequestWithContext(ctx, "POST", targetURL, strings.NewReader(string(jsonBody)))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	// 注入 Copilot headers
	req.Header = InjectCopilotHeaders(req.Header, copilotToken)

	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	return p.client.Do(req)
}

// ============================================================
// 流式响应转发
// ============================================================

// ForwardStreamToWriter 将流式响应转发到指定的 writer
// 用于 HTTP handler 直接转发 SSE 流
func ForwardStreamToWriter(ctx context.Context, w io.Writer, chunkCh <-chan *StreamChunk, errCh <-chan error, format string) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("writer is not a http.Flusher")
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case err, ok := <-errCh:
			if !ok {
				// errCh 已关闭，正常结束
				return nil
			}
			return err

		case chunk, ok := <-chunkCh:
			if !ok {
				// chunkCh 已关闭，正常结束
				return nil
			}

			// 根据目标格式转换 chunk
			var data []byte
			var err error

			if format == "openai" || format == "copilot" {
				data, err = json.Marshal(chunkToOpenAIFormat(chunk))
			} else {
				data, err = json.Marshal(chunkToAnthropicFormat(chunk))
			}

			if err != nil {
				return fmt.Errorf("marshal chunk: %w", err)
			}

			// 写入 SSE 格式
			fmt.Fprintf(w, "data: %s\n\n", string(data))
			flusher.Flush()
		}
	}
}

// chunkToOpenAIFormat 将 StreamChunk 转换为 OpenAI 格式
func chunkToOpenAIFormat(chunk *StreamChunk) map[string]interface{} {
	result := map[string]interface{}{
		"id":      "chatcmpl-" + fmt.Sprintf("%d", time.Now().UnixNano()),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   "gpt-4o",
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"delta": map[string]interface{}{
					"content": chunk.Delta,
				},
				"finish_reason": nil,
			},
		},
	}

	if chunk.Done {
		result["choices"] = []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": "stop",
			},
		}
	}

	return result
}

// chunkToAnthropicFormat 将 StreamChunk 转换为 Anthropic 格式
func chunkToAnthropicFormat(chunk *StreamChunk) map[string]interface{} {
	result := map[string]interface{}{
		"type": "content_block_delta",
		"index": 0,
		"delta": map[string]interface{}{
			"type": "text_delta",
			"text": chunk.Delta,
		},
	}

	if chunk.Done {
		result = map[string]interface{}{
			"type": "message_stop",
		}
	}

	return result
}
