package proxy

import (
	"testing"
)

// TestFormatConverter_BasicMessageConversion 测试基本消息转换
func TestFormatConverter_BasicMessageConversion(t *testing.T) {
	converter := NewFormatConverter()

	// 测试 Anthropic 到 OpenAI 的消息转换
	anthropicReq := &UnifiedRequest{
		Model:     "claude-3-5-sonnet-20241022",
		MaxTokens: 4096,
		Messages: []Message{
			{
				Role:    "user",
				Content: "Hello, world!",
			},
		},
	}

	openAIReq := converter.AnthropicToOpenAI(anthropicReq)

	if openAIReq.Model != "gpt-4o" {
		t.Errorf("Expected model gpt-4o, got %s", openAIReq.Model)
	}

	if len(openAIReq.Messages) != 1 {
		t.Errorf("Expected 1 message, got %d", len(openAIReq.Messages))
	}

	if openAIReq.Messages[0].Role != "user" {
		t.Errorf("Expected role user, got %s", openAIReq.Messages[0].Role)
	}

	if openAIReq.Messages[0].Content != "Hello, world!" {
		t.Errorf("Expected content 'Hello, world!', got %s", openAIReq.Messages[0].Content)
	}
}

// TestFormatConverter_OpenAIToAnthropic 测试 OpenAI 到 Anthropic 的转换
func TestFormatConverter_OpenAIToAnthropic(t *testing.T) {
	converter := NewFormatConverter()

	// 测试 OpenAI 到 Anthropic 的消息转换
	openAIReq := &UnifiedRequest{
		Model:     "gpt-4o",
		MaxTokens: 4096,
		Messages: []Message{
			{
				Role:    "user",
				Content: "Hello, Claude!",
			},
		},
	}

	anthropicReq := converter.OpenAIToAnthropic(openAIReq)

	if anthropicReq.Model != "claude-3-5-sonnet-20241022" {
		t.Errorf("Expected model claude-3-5-sonnet-20241022, got %s", anthropicReq.Model)
	}

	if len(anthropicReq.Messages) != 1 {
		t.Errorf("Expected 1 message, got %d", len(anthropicReq.Messages))
	}

	if anthropicReq.Messages[0].Role != "user" {
		t.Errorf("Expected role user, got %s", anthropicReq.Messages[0].Role)
	}
}

// TestFormatConverter_ResponseConversion 测试响应转换
func TestFormatConverter_ResponseConversion(t *testing.T) {
	converter := NewFormatConverter()

	// 测试 Anthropic 响应转换为 OpenAI 格式
	anthropicResp := &UnifiedResponse{
		ID:      "msg-123",
		Model:   "claude-3-5-sonnet-20241022",
		Role:    "assistant",
		Content: "Hello! How can I help you?",
		StopReason: "end_turn",
		Usage: &Usage{
			InputTokens:  100,
			OutputTokens: 50,
		},
	}

	openAIResp := converter.AnthropicToOpenAIResponse(anthropicResp)

	if openAIResp.Model != "gpt-4o" {
		t.Errorf("Expected model gpt-4o, got %s", openAIResp.Model)
	}

	if openAIResp.Content != "Hello! How can I help you?" {
		t.Errorf("Expected content 'Hello! How can I help you?', got %s", openAIResp.Content)
	}

	if openAIResp.StopReason != "stop" {
		t.Errorf("Expected stop reason 'stop', got %s", openAIResp.StopReason)
	}

	if openAIResp.Usage.InputTokens != 100 {
		t.Errorf("Expected 100 input tokens, got %d", openAIResp.Usage.InputTokens)
	}

	if openAIResp.Usage.OutputTokens != 50 {
		t.Errorf("Expected 50 output tokens, got %d", openAIResp.Usage.OutputTokens)
	}
}

// TestFormatConverter_ToolUseConversion 测试工具调用转换
func TestFormatConverter_ToolUseConversion(t *testing.T) {
	converter := NewFormatConverter()

	// 测试带工具调用的消息转换
	toolMessage := &UnifiedRequest{
		Model:     "claude-3-5-sonnet-20241022",
		MaxTokens: 4096,
		Messages: []Message{
			{
				Role: "user",
				Content: "What's the weather?",
			},
			{
				Role: "assistant",
				ToolUse: &ToolUse{
					ID:   "toolu-123",
					Name: "get_weather",
					Input: map[string]interface{}{
						"location": "San Francisco",
					},
				},
			},
		},
		Tools: []Tool{
			{
				Name:        "get_weather",
				Description: "Get current weather",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"location": map[string]interface{}{
							"type": "string",
						},
					},
				},
			},
		},
	}

	openAIReq := converter.AnthropicToOpenAI(toolMessage)

	if len(openAIReq.Tools) != 1 {
		t.Errorf("Expected 1 tool, got %d", len(openAIReq.Tools))
	}

	if openAIReq.Tools[0].Name != "get_weather" {
		t.Errorf("Expected tool name 'get_weather', got %s", openAIReq.Tools[0].Name)
	}

	// 检查 assistant 消息中的工具调用
	if len(openAIReq.Messages) < 2 {
		t.Fatal("Expected at least 2 messages")
	}

	assistantMsg := openAIReq.Messages[1]
	if assistantMsg.ToolUse == nil {
		t.Error("Expected ToolUse in assistant message")
	}

	if assistantMsg.ToolUse.Name != "get_weather" {
		t.Errorf("Expected tool name 'get_weather', got %s", assistantMsg.ToolUse.Name)
	}
}

// TestFormatConverter_ModelMapping 测试模型名称映射
func TestFormatConverter_ModelMapping(t *testing.T) {
	converter := NewFormatConverter().(*DefaultFormatConverter)

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "Claude Sonnet 4.6 to GPT-4o",
			input:    "claude-3-5-sonnet-20241022",
			expected: "gpt-4o",
		},
		{
			name:     "Claude Opus to GPT-4 Turbo",
			input:    "claude-3-opus-20240229",
			expected: "gpt-4-turbo",
		},
		{
			name:     "Claude Haiku to GPT-3.5",
			input:    "claude-3-haiku-20240307",
			expected: "gpt-3.5-turbo",
		},
		{
			name:     "Unknown model stays same",
			input:    "unknown-model",
			expected: "unknown-model",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := converter.convertModelToOpenAI(tt.input)
			if result != tt.expected {
				t.Errorf("Expected %s, got %s", tt.expected, result)
			}
		})
	}
}

// TestFormatConverter_StopReasonMapping 测试停止原因映射
func TestFormatConverter_StopReasonMapping(t *testing.T) {
	converter := NewFormatConverter().(*DefaultFormatConverter)

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "end_turn to stop",
			input:    "end_turn",
			expected: "stop",
		},
		{
			name:     "max_tokens to length",
			input:    "max_tokens",
			expected: "length",
		},
		{
			name:     "tool_use to tool_calls",
			input:    "tool_use",
			expected: "tool_calls",
		},
		{
			name:     "Unknown stop reason",
			input:    "unknown",
			expected: "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := converter.convertStopReasonToOpenAI(tt.input)
			if result != tt.expected {
				t.Errorf("Expected %s, got %s", tt.expected, result)
			}
		})
	}
}

// TestConvertJSONObject 测试 JSON 对象转换
func TestConvertJSONObject(t *testing.T) {
	input := map[string]interface{}{
		"name": "test",
		"value": 123,
		"nested": map[string]interface{}{
			"key": "value",
		},
	}

	result, err := ConvertJSONObject(input, nil)
	if err != nil {
		t.Fatalf("ConvertJSONObject failed: %v", err)
	}

	if result["name"] != "test" {
		t.Errorf("Expected name 'test', got %v", result["name"])
	}

	if result["value"] != float64(123) {
		t.Errorf("Expected value 123, got %v", result["value"])
	}

	nested, ok := result["nested"].(map[string]interface{})
	if !ok {
		t.Fatal("Expected nested to be a map")
	}

	if nested["key"] != "value" {
		t.Errorf("Expected nested key 'value', got %v", nested["key"])
	}
}

// TestCloneJSONObject 测试 JSON 对象深拷贝
func TestCloneJSONObject(t *testing.T) {
	original := map[string]interface{}{
		"name": "test",
		"value": 123,
	}

	clone, err := CloneJSONObject(original)
	if err != nil {
		t.Fatalf("CloneJSONObject failed: %v", err)
	}

	// 修改克隆
	clone["name"] = "modified"

	// 原始应该不变
	if original["name"] != "test" {
		t.Errorf("Original was modified, expected 'test', got %v", original["name"])
	}

	if clone["name"] != "modified" {
		t.Errorf("Clone was not modified, expected 'modified', got %v", clone["name"])
	}
}

// TestConvertStreamChunks 测试流式 chunk 转换
func TestConvertStreamChunks(t *testing.T) {
	// 测试 Anthropic 到 OpenAI 的 chunk 转换
	anthropicChunk := &StreamChunk{
		Type:  ChunkTypeContent,
		Delta: "Hello",
		Done:  false,
	}

	openAIChunk := ConvertAnthropicStreamToOpenAI(anthropicChunk)

	if openAIChunk.Type != ChunkTypeContent {
		t.Errorf("Expected type %d, got %d", ChunkTypeContent, openAIChunk.Type)
	}

	if openAIChunk.Delta != "Hello" {
		t.Errorf("Expected delta 'Hello', got %s", openAIChunk.Delta)
	}

	if openAIChunk.Done != false {
		t.Errorf("Expected done false, got %v", openAIChunk.Done)
	}
}

// TestFormatConverter_ContentBlocks 测试多模态内容块转换
func TestFormatConverter_ContentBlocks(t *testing.T) {
	converter := NewFormatConverter()

	// 测试带图片的消息转换
	req := &UnifiedRequest{
		Model:     "claude-3-5-sonnet-20241022",
		MaxTokens: 4096,
		Messages: []Message{
			{
				Role: "user",
				ContentBlocks: []ContentBlock{
					{
						Type: "text",
						Text: "What's in this image?",
					},
					{
						Type: "image",
						Source: &ImageSource{
							Type:      "base64",
							MediaType: "image/jpeg",
							Data:      "base64encodeddata",
						},
					},
				},
			},
		},
	}

	converted := converter.AnthropicToOpenAI(req)

	if len(converted.Messages) != 1 {
		t.Fatalf("Expected 1 message, got %d", len(converted.Messages))
	}

	if len(converted.Messages[0].ContentBlocks) != 2 {
		t.Errorf("Expected 2 content blocks, got %d", len(converted.Messages[0].ContentBlocks))
	}

	if converted.Messages[0].ContentBlocks[0].Type != "text" {
		t.Errorf("Expected first block type 'text', got %s", converted.Messages[0].ContentBlocks[0].Type)
	}

	if converted.Messages[0].ContentBlocks[1].Type != "image" {
		t.Errorf("Expected second block type 'image', got %s", converted.Messages[0].ContentBlocks[1].Type)
	}

	if converted.Messages[0].ContentBlocks[1].Source == nil {
		t.Fatal("Expected source in image block")
	}

	if converted.Messages[0].ContentBlocks[1].Source.Data != "base64encodeddata" {
		t.Errorf("Expected image data 'base64encodeddata', got %s", converted.Messages[0].ContentBlocks[1].Source.Data)
	}
}
