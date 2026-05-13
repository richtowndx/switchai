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
	RegisterProxyFactory("openai", NewOpenAIProxy)
}

// OpenAIProxy 实现 OpenAI API 代理
type OpenAIProxy struct {
	provider *config.Provider
	client   *http.Client
}

// NewOpenAIProxy 创建 OpenAI 代理实例
func NewOpenAIProxy(provider *config.Provider) (CcProxy, error) {
	return &OpenAIProxy{
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
func (p *OpenAIProxy) SendMessage(ctx context.Context, req *UnifiedRequest) (*UnifiedResponse, error) {
	// 构建请求体
	body := map[string]interface{}{
		"model":      req.Model,
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
	resp, err := p.doRequest(ctx, body, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 解析响应
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
func (p *OpenAIProxy) SendMessageStream(ctx context.Context, req *UnifiedRequest) (<-chan *StreamChunk, <-chan error) {
	chunkCh := make(chan *StreamChunk, 16)
	errCh := make(chan error, 1)

	go func() {
		defer close(chunkCh)
		defer close(errCh)

		// 构建请求体
		body := map[string]interface{}{
			"model":      req.Model,
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
		resp, err := p.doRequest(ctx, body, true)
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
func (p *OpenAIProxy) Close() error {
	p.client.CloseIdleConnections()
	return nil
}

// Provider 返回关联的 Provider
func (p *OpenAIProxy) Provider() *config.Provider {
	return p.provider
}

// ============================================================
// 转换函数
// ============================================================

func (p *OpenAIProxy) convertMessages(messages []Message) []map[string]interface{} {
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

func (p *OpenAIProxy) convertTools(tools []Tool) []map[string]interface{} {
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

func (p *OpenAIProxy) convertToUnifiedResponse(resp *openAIResponse) *UnifiedResponse {
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

func (p *OpenAIProxy) convertStreamChunk(chunk *openAIStreamChunk, currentToolUse **ToolUse) *StreamChunk {
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

func (p *OpenAIProxy) doRequest(ctx context.Context, body map[string]interface{}, stream bool) (*http.Response, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	baseURL := p.provider.BaseURL
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	if !strings.HasSuffix(baseURL, "/chat/completions") {
		if !strings.HasSuffix(baseURL, "/") {
			baseURL += "/"
		}
		baseURL += "chat/completions"
	}

	req, err := http.NewRequestWithContext(ctx, "POST", baseURL, strings.NewReader(string(jsonBody)))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.provider.APIKey)

	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	return p.client.Do(req)
}

// ============================================================
// 响应类型定义
// ============================================================

type openAIResponse struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"`
	Created int64             `json:"created"`
	Model   string            `json:"model"`
	Choices []openAIChoice    `json:"choices"`
	Usage   openAIUsage       `json:"usage"`
	Error   openAIError       `json:"error"`
}

type openAIChoice struct {
	Index        int               `json:"index"`
	Message      openAIMessage     `json:"message"`
	FinishReason string            `json:"finish_reason"`
	Delta        openAIDelta       `json:"delta"`
}

type openAIMessage struct {
	Role      string              `json:"role"`
	Content   *string             `json:"content"`
	ToolCalls []openAIToolCall    `json:"tool_calls,omitempty"`
}

type openAIToolCall struct {
	Index    int                `json:"index"`
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIFunction     `json:"function"`
}

type openAIFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAIDelta struct {
	Role      string               `json:"role,omitempty"`
	Content   *string              `json:"content,omitempty"`
	ToolCalls []openAIStreamToolCall `json:"tool_calls,omitempty"`
}

type openAIStreamToolCall struct {
	Index    *int              `json:"index,omitempty"`
	ID       *string           `json:"id,omitempty"`
	Function *openAIStreamFunction `json:"function,omitempty"`
}

type openAIStreamFunction struct {
	Name      *string `json:"name,omitempty"`
	Arguments *string `json:"arguments,omitempty"`
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type openAIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

type openAIStreamChunk struct {
	ID      string          `json:"id"`
	Object  string          `json:"object"`
	Created int64           `json:"created"`
	Model   string          `json:"model"`
	Choices []openAIChoice  `json:"choices"`
}
