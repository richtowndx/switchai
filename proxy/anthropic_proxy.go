package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"switchai/config"
)

func init() {
	RegisterProxyFactory("anthropic", NewAnthropicProxy)
}

// AnthropicProxy 实现 Anthropic API 代理
type AnthropicProxy struct {
	provider *config.Provider
	client   *http.Client
}

// NewAnthropicProxy 创建 Anthropic 代理实例
func NewAnthropicProxy(provider *config.Provider) (CcProxy, error) {
	if provider.IsOpenAIFormat {
		return nil, fmt.Errorf("provider %s is not Anthropic format", provider.Name)
	}

	return &AnthropicProxy{
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
func (p *AnthropicProxy) SendMessage(ctx context.Context, req *UnifiedRequest) (*UnifiedResponse, error) {
	// 构建请求体
	body := map[string]interface{}{
		"model":      req.Model,
		"max_tokens": req.MaxTokens,
		"messages":   p.convertMessages(req.Messages),
	}

	if req.Temperature > 0 {
		body["temperature"] = req.Temperature
	}
	if req.TopP > 0 {
		body["top_p"] = req.TopP
	}
	if req.System != "" {
		body["system"] = req.System
	}
	if len(req.Tools) > 0 {
		body["tools"] = p.convertTools(req.Tools)
		body["tool_choice"] = map[string]interface{}{"type": "auto"}
	}

	// 发送请求
	resp, err := p.doRequest(ctx, body, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 解析响应
	var result anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("api error: %s", result.Error.Message)
	}

	return p.convertToUnifiedResponse(&result), nil
}

// SendMessageStream 发送流式消息请求
func (p *AnthropicProxy) SendMessageStream(ctx context.Context, req *UnifiedRequest) (<-chan *StreamChunk, <-chan error) {
	chunkCh := make(chan *StreamChunk, 16)
	errCh := make(chan error, 1)

	go func() {
		defer close(chunkCh)
		defer close(errCh)

		// 构建请求体
		body := map[string]interface{}{
			"model":      req.Model,
			"max_tokens": req.MaxTokens,
			"messages":   p.convertMessages(req.Messages),
			"stream":     true,
		}

		if req.Temperature > 0 {
			body["temperature"] = req.Temperature
		}
		if req.TopP > 0 {
			body["top_p"] = req.TopP
		}
		if req.System != "" {
			body["system"] = req.System
		}
		if len(req.Tools) > 0 {
			body["tools"] = p.convertTools(req.Tools)
			body["tool_choice"] = map[string]interface{}{"type": "auto"}
		}

		// 发送请求
		resp, err := p.doRequest(ctx, body, true)
		if err != nil {
			errCh <- err
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			var errResp anthropicResponse
			json.NewDecoder(resp.Body).Decode(&errResp)
			errCh <- fmt.Errorf("api error: %s", errResp.Error.Message)
			return
		}

		// 解析 SSE 流
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

			var event anthropicStreamEvent
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				continue
			}

			chunk := p.convertStreamEvent(&event, &currentToolUse)
			if chunk != nil {
				chunkCh <- chunk
			}
		}

		if err := scanner.Err(); err != nil {
			errCh <- fmt.Errorf("scan stream: %w", err)
		}
	}()

	return chunkCh, errCh
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
// 转换函数
// ============================================================

func (p *AnthropicProxy) convertMessages(messages []Message) []map[string]interface{} {
	result := make([]map[string]interface{}, len(messages))
	for i, msg := range messages {
		result[i] = map[string]interface{}{
			"role": msg.Role,
		}

		if msg.ToolResult != nil {
			result[i]["content"] = []map[string]interface{}{
				{
					"type":       "tool_result",
					"tool_use_id": msg.ToolResult.ToolUseID,
					"content":    msg.ToolResult.Content,
					"is_error":   msg.ToolResult.IsError,
				},
			}
		} else if len(msg.ContentBlocks) > 0 {
			blocks := make([]map[string]interface{}, len(msg.ContentBlocks))
			for j, block := range msg.ContentBlocks {
				blocks[j] = map[string]interface{}{
					"type": block.Type,
				}
				if block.Type == "text" {
					blocks[j]["text"] = block.Text
				} else if block.Type == "image" {
					blocks[j]["source"] = map[string]interface{}{
						"type":       block.Source.Type,
						"media_type": block.Source.MediaType,
						"data":       block.Source.Data,
					}
				}
			}
			result[i]["content"] = blocks
		} else {
			result[i]["content"] = msg.Content
		}
	}
	return result
}

func (p *AnthropicProxy) convertTools(tools []Tool) []map[string]interface{} {
	result := make([]map[string]interface{}, len(tools))
	for i, tool := range tools {
		result[i] = map[string]interface{}{
			"name":        tool.Name,
			"description": tool.Description,
			"input_schema": tool.InputSchema,
		}
	}
	return result
}

func (p *AnthropicProxy) convertToUnifiedResponse(resp *anthropicResponse) *UnifiedResponse {
	result := &UnifiedResponse{
		ID:      resp.ID,
		Model:   resp.Model,
		Role:    "assistant",
		Content: "",
		ContentBlocks: make([]ContentBlock, 0),
	}

	for _, block := range resp.Content {
		if block.Type == "text" {
			result.Content += block.Text
			result.ContentBlocks = append(result.ContentBlocks, ContentBlock{
				Type: "text",
				Text: block.Text,
			})
		} else if block.Type == "tool_use" {
			result.ToolUse = &ToolUse{
				ID:       block.ID,
				Name:     block.Name,
				Input:    block.Input,
				Metadata: map[string]string{},
			}
		}
	}

	if resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0 {
		result.Usage = &Usage{
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
		}
	}

	result.StopReason = resp.StopReason

	return result
}

func (p *AnthropicProxy) convertStreamEvent(event *anthropicStreamEvent, currentToolUse **ToolUse) *StreamChunk {
	switch event.Type {
	case "content_block_start":
		if event.ContentBlock != nil && event.ContentBlock.Type == "tool_use" {
			*currentToolUse = &ToolUse{
				ID:        event.ContentBlock.ID,
				Name:      event.ContentBlock.Name,
				Input:     make(map[string]interface{}),
				Metadata:  make(map[string]string),
			}
			return &StreamChunk{
				Type:    ChunkTypeToolUseStart,
				ToolUse: *currentToolUse,
			}
		}
		return &StreamChunk{Type: ChunkTypeContent}

	case "content_block_delta":
		if event.Delta != nil && event.Delta.Type == "text_delta" && event.Delta.Text != "" {
			return &StreamChunk{
				Type:  ChunkTypeContent,
				Delta: event.Delta.Text,
			}
		}
		if event.Delta != nil && event.Delta.Type == "input_json_delta" {
			return &StreamChunk{
				Type: ChunkTypeToolUseDelta,
			}
		}

	case "message_stop":
		return &StreamChunk{
			Type: ChunkTypeUsage,
			Usage: &Usage{},
			Done:  true,
		}
	}

	return nil
}

func (p *AnthropicProxy) doRequest(ctx context.Context, body map[string]interface{}, stream bool) (*http.Response, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

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

	req, err := http.NewRequestWithContext(ctx, "POST", baseURL, strings.NewReader(string(jsonBody)))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", p.provider.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	return p.client.Do(req)
}

// ============================================================
// 响应类型定义
// ============================================================

type anthropicResponse struct {
	ID           string                 `json:"id"`
	Type         string                 `json:"type"`
	Role         string                 `json:"role"`
	Content      []anthropicContentBlock `json:"content"`
	Model        string                 `json:"model"`
	StopReason   string                 `json:"stop_reason"`
	Usage        anthropicUsage         `json:"usage"`
	Error        anthropicError         `json:"error"`
}

type anthropicContentBlock struct {
	Type     string                 `json:"type"`
	Text     string                 `json:"text,omitempty"`
	ID       string                 `json:"id,omitempty"`
	Name     string                 `json:"name,omitempty"`
	Input    map[string]interface{} `json:"input,omitempty"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type anthropicError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type anthropicStreamEvent struct {
	Type         string                       `json:"type"`
	Index        int                          `json:"index,omitempty"`
	Delta        *anthropicStreamDelta        `json:"delta,omitempty"`
	ContentBlock *anthropicStreamContentBlock `json:"content_block,omitempty"`
	Message      *anthropicStreamMessage     `json:"message,omitempty"`
}

type anthropicStreamDelta struct {
	Type         string `json:"type,omitempty"`
	Text         string `json:"text,omitempty"`
	PartialJSON  string `json:"partial_json,omitempty"`
}

type anthropicStreamContentBlock struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

type anthropicStreamMessage struct {
	StopReason string `json:"stop_reason,omitempty"`
}
