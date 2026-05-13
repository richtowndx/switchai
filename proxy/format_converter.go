package proxy

import (
	"encoding/json"
	"fmt"
)

// FormatConverter 定义格式转换接口
type FormatConverter interface {
	// AnthropicToOpenAI 将 Anthropic 格式请求转换为 OpenAI 格式
	AnthropicToOpenAI(req *UnifiedRequest) *UnifiedRequest

	// OpenAIToAnthropic 将 OpenAI 格式请求转换为 Anthropic 格式
	OpenAIToAnthropic(req *UnifiedRequest) *UnifiedRequest

	// AnthropicToOpenAIResponse 将 Anthropic 响应转换为 OpenAI 格式
	AnthropicToOpenAIResponse(resp *UnifiedResponse) *UnifiedResponse

	// OpenAIToAnthropicResponse 将 OpenAI 响应转换为 Anthropic 格式
	OpenAIToAnthropicResponse(resp *UnifiedResponse) *UnifiedResponse
}

// DefaultFormatConverter 默认格式转换器实现
type DefaultFormatConverter struct{}

// NewFormatConverter 创建格式转换器
func NewFormatConverter() FormatConverter {
	return &DefaultFormatConverter{}
}

// AnthropicToOpenAI 将 Anthropic 格式请求转换为 OpenAI 格式
func (c *DefaultFormatConverter) AnthropicToOpenAI(req *UnifiedRequest) *UnifiedRequest {
	result := &UnifiedRequest{
		Model:       c.convertModelToOpenAI(req.Model),
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      req.Stream,
		Metadata:    req.Metadata,
	}

	// 转换消息
	result.Messages = make([]Message, len(req.Messages))
	for i, msg := range req.Messages {
		result.Messages[i] = c.convertMessageToOpenAI(msg)
	}

	// 转换工具
	if len(req.Tools) > 0 {
		result.Tools = make([]Tool, len(req.Tools))
		for i, tool := range req.Tools {
			result.Tools[i] = c.convertToolToOpenAI(tool)
		}
	}

	return result
}

// OpenAIToAnthropic 将 OpenAI 格式请求转换为 Anthropic 格式
func (c *DefaultFormatConverter) OpenAIToAnthropic(req *UnifiedRequest) *UnifiedRequest {
	result := &UnifiedRequest{
		Model:       c.convertModelToAnthropic(req.Model),
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      req.Stream,
		Metadata:    req.Metadata,
	}

	// 转换消息
	result.Messages = make([]Message, len(req.Messages))
	for i, msg := range req.Messages {
		result.Messages[i] = c.convertMessageToAnthropic(msg)
	}

	// 转换工具
	if len(req.Tools) > 0 {
		result.Tools = make([]Tool, len(req.Tools))
		for i, tool := range req.Tools {
			result.Tools[i] = c.convertToolToAnthropic(tool)
		}
	}

	// OpenAI 的 system 通常放在第一条消息中，需要提取
	if len(req.Messages) > 0 && req.Messages[0].Role == "system" {
		result.System = req.Messages[0].Content
		result.Messages = result.Messages[1:]
	}

	return result
}

// AnthropicToOpenAIResponse 将 Anthropic 响应转换为 OpenAI 格式
func (c *DefaultFormatConverter) AnthropicToOpenAIResponse(resp *UnifiedResponse) *UnifiedResponse {
	result := &UnifiedResponse{
		ID:            resp.ID,
		Model:         c.convertModelToOpenAI(resp.Model),
		Role:          resp.Role,
		Content:       resp.Content,
		StopReason:    c.convertStopReasonToOpenAI(resp.StopReason),
		Metadata:      resp.Metadata,
		ContentBlocks: make([]ContentBlock, len(resp.ContentBlocks)),
	}

	// 转换内容块
	for i, block := range resp.ContentBlocks {
		result.ContentBlocks[i] = c.convertContentBlockToOpenAI(block)
	}

	// 转换工具调用
	if resp.ToolUse != nil {
		result.ToolUse = &ToolUse{
			ID:       resp.ToolUse.ID,
			Name:     resp.ToolUse.Name,
			Input:    resp.ToolUse.Input,
			Metadata: resp.ToolUse.Metadata,
		}
	}

	// 转换 usage
	if resp.Usage != nil {
		result.Usage = &Usage{
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
		}
	}

	return result
}

// OpenAIToAnthropicResponse 将 OpenAI 响应转换为 Anthropic 格式
func (c *DefaultFormatConverter) OpenAIToAnthropicResponse(resp *UnifiedResponse) *UnifiedResponse {
	result := &UnifiedResponse{
		ID:            resp.ID,
		Model:         c.convertModelToAnthropic(resp.Model),
		Role:          resp.Role,
		Content:       resp.Content,
		StopReason:    c.convertStopReasonToAnthropic(resp.StopReason),
		Metadata:      resp.Metadata,
		ContentBlocks: make([]ContentBlock, len(resp.ContentBlocks)),
	}

	// 转换内容块
	for i, block := range resp.ContentBlocks {
		result.ContentBlocks[i] = c.convertContentBlockToAnthropic(block)
	}

	// 转换工具调用
	if resp.ToolUse != nil {
		result.ToolUse = &ToolUse{
			ID:       resp.ToolUse.ID,
			Name:     resp.ToolUse.Name,
			Input:    resp.ToolUse.Input,
			Metadata: resp.ToolUse.Metadata,
		}
	}

	// 转换 usage
	if resp.Usage != nil {
		result.Usage = &Usage{
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
		}
	}

	return result
}

// ============================================================
// 消息转换
// ============================================================

func (c *DefaultFormatConverter) convertMessageToOpenAI(msg Message) Message {
	result := Message{
		Role:    msg.Role,
		Content: msg.Content,
	}

	// 处理工具结果
	if msg.ToolResult != nil {
		result.ToolResult = &ToolResult{
			ToolUseID: msg.ToolResult.ToolUseID,
			Content:   msg.ToolResult.Content,
			IsError:   msg.ToolResult.IsError,
		}
	}

	// 处理工具调用
	if msg.ToolUse != nil {
		result.ToolUse = &ToolUse{
			ID:       msg.ToolUse.ID,
			Name:     msg.ToolUse.Name,
			Input:    msg.ToolUse.Input,
			Metadata: msg.ToolUse.Metadata,
		}
	}

	// 处理内容块
	if len(msg.ContentBlocks) > 0 {
		result.ContentBlocks = make([]ContentBlock, len(msg.ContentBlocks))
		for i, block := range msg.ContentBlocks {
			result.ContentBlocks[i] = c.convertContentBlockToOpenAI(block)
		}
	}

	return result
}

func (c *DefaultFormatConverter) convertMessageToAnthropic(msg Message) Message {
	result := Message{
		Role:    msg.Role,
		Content: msg.Content,
	}

	// 处理工具结果
	if msg.ToolResult != nil {
		result.ToolResult = &ToolResult{
			ToolUseID: msg.ToolResult.ToolUseID,
			Content:   msg.ToolResult.Content,
			IsError:   msg.ToolResult.IsError,
		}
	}

	// 处理工具调用
	if msg.ToolUse != nil {
		result.ToolUse = &ToolUse{
			ID:       msg.ToolUse.ID,
			Name:     msg.ToolUse.Name,
			Input:    msg.ToolUse.Input,
			Metadata: msg.ToolUse.Metadata,
		}
	}

	// 处理内容块
	if len(msg.ContentBlocks) > 0 {
		result.ContentBlocks = make([]ContentBlock, len(msg.ContentBlocks))
		for i, block := range msg.ContentBlocks {
			result.ContentBlocks[i] = c.convertContentBlockToAnthropic(block)
		}
	}

	return result
}

// ============================================================
// 工具转换
// ============================================================

func (c *DefaultFormatConverter) convertToolToOpenAI(tool Tool) Tool {
	// OpenAI 和 Anthropic 的工具定义基本兼容
	return tool
}

func (c *DefaultFormatConverter) convertToolToAnthropic(tool Tool) Tool {
	// OpenAI 和 Anthropic 的工具定义基本兼容
	return tool
}

// ============================================================
// 内容块转换
// ============================================================

func (c *DefaultFormatConverter) convertContentBlockToOpenAI(block ContentBlock) ContentBlock {
	result := ContentBlock{
		Type:     block.Type,
		Text:     block.Text,
		MimeType: block.MimeType,
	}

	if block.Source != nil {
		result.Source = &ImageSource{
			Type:      block.Source.Type,
			MediaType: block.Source.MediaType,
			Data:      block.Source.Data,
		}
	}

	return result
}

func (c *DefaultFormatConverter) convertContentBlockToAnthropic(block ContentBlock) ContentBlock {
	result := ContentBlock{
		Type:     block.Type,
		Text:     block.Text,
		MimeType: block.MimeType,
	}

	if block.Source != nil {
		result.Source = &ImageSource{
			Type:      block.Source.Type,
			MediaType: block.Source.MediaType,
			Data:      block.Source.Data,
		}
	}

	return result
}

// ============================================================
// 模型名称转换
// ============================================================

func (c *DefaultFormatConverter) convertModelToOpenAI(model string) string {
	// Anthropic 模型到 OpenAI 模型的映射
	modelMap := map[string]string{
		"claude-3-5-sonnet-20241022": "gpt-4o",
		"claude-3-5-sonnet-20240620": "gpt-4o",
		"claude-3-opus-20240229":      "gpt-4-turbo",
		"claude-3-sonnet-20240229":    "gpt-4-turbo",
		"claude-3-haiku-20240307":     "gpt-3.5-turbo",
	}

	if mapped, ok := modelMap[model]; ok {
		return mapped
	}
	return model
}

func (c *DefaultFormatConverter) convertModelToAnthropic(model string) string {
	// OpenAI 模型到 Anthropic 模型的映射
	modelMap := map[string]string{
		"gpt-4o":          "claude-3-5-sonnet-20241022",
		"gpt-4-turbo":     "claude-3-opus-20240229",
		"gpt-4":           "claude-3-opus-20240229",
		"gpt-3.5-turbo":   "claude-3-haiku-20240307",
		"gpt-3.5-turbo-16k": "claude-3-haiku-20240307",
	}

	if mapped, ok := modelMap[model]; ok {
		return mapped
	}
	return model
}

// ============================================================
// 停止原因转换
// ============================================================

func (c *DefaultFormatConverter) convertStopReasonToOpenAI(reason string) string {
	// Anthropic stop_reason 到 OpenAI finish_reason 的映射
	reasonMap := map[string]string{
		"end_turn":      "stop",
		"max_tokens":    "length",
		"stop_sequence": "stop",
		"tool_use":      "tool_calls",
	}

	if mapped, ok := reasonMap[reason]; ok {
		return mapped
	}
	return reason
}

func (c *DefaultFormatConverter) convertStopReasonToAnthropic(reason string) string {
	// OpenAI finish_reason 到 Anthropic stop_reason 的映射
	reasonMap := map[string]string{
		"stop":       "end_turn",
		"length":     "max_tokens",
		"tool_calls": "tool_use",
		"content_filter": "end_turn",
	}

	if mapped, ok := reasonMap[reason]; ok {
		return mapped
	}
	return reason
}

// ============================================================
// 流式事件转换
// ============================================================

// ConvertAnthropicStreamToOpenAI 将 Anthropic 流式事件转换为 OpenAI 格式
func ConvertAnthropicStreamToOpenAI(chunk *StreamChunk) *StreamChunk {
	if chunk == nil {
		return nil
	}

	result := &StreamChunk{
		Type:  chunk.Type,
		Delta: chunk.Delta,
		Done:  chunk.Done,
		Error: chunk.Error,
	}

	if chunk.ToolUse != nil {
		result.ToolUse = &ToolUse{
			ID:       chunk.ToolUse.ID,
			Name:     chunk.ToolUse.Name,
			Input:    chunk.ToolUse.Input,
			Metadata: chunk.ToolUse.Metadata,
		}
	}

	if chunk.Usage != nil {
		result.Usage = &Usage{
			InputTokens:  chunk.Usage.InputTokens,
			OutputTokens: chunk.Usage.OutputTokens,
		}
	}

	return result
}

// ConvertOpenAIStreamToAnthropic 将 OpenAI 流式事件转换为 Anthropic 格式
func ConvertOpenAIStreamToAnthropic(chunk *StreamChunk) *StreamChunk {
	if chunk == nil {
		return nil
	}

	result := &StreamChunk{
		Type:  chunk.Type,
		Delta: chunk.Delta,
		Done:  chunk.Done,
		Error: chunk.Error,
	}

	if chunk.ToolUse != nil {
		result.ToolUse = &ToolUse{
			ID:       chunk.ToolUse.ID,
			Name:     chunk.ToolUse.Name,
			Input:    chunk.ToolUse.Input,
			Metadata: chunk.ToolUse.Metadata,
		}
	}

	if chunk.Usage != nil {
		result.Usage = &Usage{
			InputTokens:  chunk.Usage.InputTokens,
			OutputTokens: chunk.Usage.OutputTokens,
		}
	}

	return result
}

// ============================================================
// JSON 转换辅助函数
// ============================================================

// ConvertJSONObject 将 map[string]interface{} 在不同 JSON Schema 间转换
func ConvertJSONObject(input map[string]interface{}, targetSchema map[string]interface{}) (map[string]interface{}, error) {
	// 将输入序列化为 JSON
	jsonBytes, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("marshal input: %w", err)
	}

	// 根据目标 schema 解析
	var result map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &result); err != nil {
		return nil, fmt.Errorf("unmarshal to target schema: %w", err)
	}

	return result, nil
}

// CloneJSONObject 深度克隆 JSON 对象
func CloneJSONObject(obj map[string]interface{}) (map[string]interface{}, error) {
	jsonBytes, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &result); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}

	return result, nil
}
