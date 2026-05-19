package proxy

import (
	"encoding/json"
	"testing"
)

func TestConvertClaudeToOpenAIWithToolUse(t *testing.T) {
	userMsg := map[string]interface{}{
		"role": "user",
		"content": []interface{}{
			map[string]interface{}{"type": "text", "text": "What is 2+2?"},
		},
	}

	assistantMsg := map[string]interface{}{
		"role": "assistant",
		"content": []interface{}{
			map[string]interface{}{"type": "text", "text": "Let me calculate."},
			map[string]interface{}{
				"type":  "tool_use",
				"id":    "toolu_123",
				"name":  "calculator",
				"input": map[string]interface{}{"expression": "2+2"},
			},
		},
	}

	toolResultMsg := map[string]interface{}{
		"role": "user",
		"content": []interface{}{
			map[string]interface{}{
				"type":        "tool_result",
				"tool_use_id": "toolu_123",
				"content":     "4",
			},
		},
	}

	thanksMsg := map[string]interface{}{
		"role": "user",
		"content": []interface{}{
			map[string]interface{}{"type": "text", "text": "Thanks!"},
		},
	}

	claudeReq := map[string]interface{}{
		"model":    "gpt-4o",
		"messages": []interface{}{userMsg, assistantMsg, toolResultMsg, thanksMsg},
		"tools": []interface{}{
			map[string]interface{}{
				"name":         "calculator",
				"description":  "A calculator",
				"input_schema": map[string]interface{}{"type": "object"},
			},
		},
		"max_tokens": 100,
		"stream":     false,
	}

	result := convertClaudeToOpenAI(claudeReq)

	if result["model"] != "gpt-4o" {
		t.Errorf("model = %v, want gpt-4o", result["model"])
	}

	messages, ok := result["messages"].([]interface{})
	if !ok {
		t.Fatalf("messages is not a slice")
	}

	msgJSON, _ := json.MarshalIndent(messages, "", "  ")
	t.Logf("Converted messages:\n%s", string(msgJSON))

	// Verify tools converted
	tools, ok := result["tools"].([]interface{})
	if !ok || len(tools) != 1 {
		t.Fatalf("tools count = %d, want 1", len(tools))
	}
	toolMap, _ := tools[0].(map[string]interface{})
	if toolMap["type"] != "function" {
		t.Errorf("tool type = %v, want function", toolMap["type"])
	}

	// Find assistant message - should have tool_calls
	var assistantFound map[string]interface{}
	for _, msg := range messages {
		if m, ok := msg.(map[string]interface{}); ok && m["role"] == "assistant" {
			assistantFound = m
			break
		}
	}
	if assistantFound == nil {
		t.Fatal("no assistant message found")
	}
	toolCalls, ok := assistantFound["tool_calls"].([]interface{})
	if !ok || len(toolCalls) != 1 {
		t.Fatalf("assistant tool_calls missing or wrong count: %v", assistantFound["tool_calls"])
	}
	tcMap, _ := toolCalls[0].(map[string]interface{})
	if tcMap["id"] != "toolu_123" {
		t.Errorf("tool_call id = %v, want toolu_123", tcMap["id"])
	}

	// Find tool role message
	var toolMsgFound map[string]interface{}
	for _, msg := range messages {
		if m, ok := msg.(map[string]interface{}); ok && m["role"] == "tool" {
			toolMsgFound = m
			break
		}
	}
	if toolMsgFound == nil {
		t.Fatal("no tool role message found")
	}
	if toolMsgFound["tool_call_id"] != "toolu_123" {
		t.Errorf("tool_call_id = %v, want toolu_123", toolMsgFound["tool_call_id"])
	}
	if toolMsgFound["content"] != "4" {
		t.Errorf("tool content = %v, want 4", toolMsgFound["content"])
	}

	// Verify no unsupported content types in any message
	for i, msg := range messages {
		msgMap, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		if contentArr, ok := msgMap["content"].([]interface{}); ok {
			for _, block := range contentArr {
				if bm, ok := block.(map[string]interface{}); ok {
					bt, _ := bm["type"].(string)
					if bt != "text" && bt != "image_url" {
						t.Errorf("message %d has unsupported content type: %s", i, bt)
					}
				}
			}
		}
	}
}

func TestConvertClaudeToOpenAIStripsThinking(t *testing.T) {
	claudeReq := map[string]interface{}{
		"model": "gpt-4o",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "Hello"},
			map[string]interface{}{
				"role": "assistant",
				"content": []interface{}{
					map[string]interface{}{"type": "thinking", "thinking": "Let me think..."},
					map[string]interface{}{"type": "redacted_thinking"},
					map[string]interface{}{"type": "text", "text": "Hi there!"},
				},
			},
		},
	}

	result := convertClaudeToOpenAI(claudeReq)
	messages, _ := result["messages"].([]interface{})

	for _, msg := range messages {
		if m, ok := msg.(map[string]interface{}); ok && m["role"] == "assistant" {
			content, _ := m["content"].(string)
			if content != "Hi there!" {
				t.Errorf("assistant content = %q, want 'Hi there!'", content)
			}
		}
	}
}

func TestConvertOpenAIToClaudeWithToolCalls(t *testing.T) {
	openaiResp := map[string]interface{}{
		"id":    "chatcmpl-123",
		"model": "gpt-4o",
		"choices": []interface{}{
			map[string]interface{}{
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": "Let me calculate.",
					"tool_calls": []interface{}{
						map[string]interface{}{
							"id":   "call_abc",
							"type": "function",
							"function": map[string]interface{}{
								"name":      "calculator",
								"arguments": `{"expression":"2+2"}`,
							},
						},
					},
				},
				"finish_reason": "tool_calls",
			},
		},
		"usage": map[string]interface{}{
			"prompt_tokens":     100.0,
			"completion_tokens": 20.0,
		},
	}

	result := convertOpenAIToClaude(openaiResp)

	if result["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v, want tool_use", result["stop_reason"])
	}

	content, ok := result["content"].([]interface{})
	if !ok {
		t.Fatalf("content is not a slice")
	}

	hasText := false
	hasToolUse := false
	for _, block := range content {
		if bm, ok := block.(map[string]interface{}); ok {
			switch bm["type"] {
			case "text":
				hasText = true
			case "tool_use":
				hasToolUse = true
				if bm["name"] != "calculator" {
					t.Errorf("tool_use name = %v, want calculator", bm["name"])
				}
				if bm["id"] != "call_abc" {
					t.Errorf("tool_use id = %v, want call_abc", bm["id"])
				}
			}
		}
	}
	if !hasText {
		t.Error("missing text block")
	}
	if !hasToolUse {
		t.Error("missing tool_use block")
	}
}

func TestFilterUnsupportedContentBlocks(t *testing.T) {
	// 测试 OpenAI → Anthropic 转换场景（过滤 thinking）
	openaiAllowedTypes := []string{"text", "image", "tool_use", "tool_result"}

	tests := []struct {
		name        string
		content     interface{}
		allowedTypes []string
		wantText    bool
		wantLen     int
	}{
		{
			name: "filter thinking block (OpenAI conversion)",
			content: []interface{}{
				map[string]interface{}{"type": "text", "text": "Hello"},
				map[string]interface{}{"type": "thinking", "thinking": "Let me think..."},
				map[string]interface{}{"type": "redacted_thinking", "thinking": "..."},
			},
			allowedTypes: openaiAllowedTypes,
			wantText: true,
			wantLen:  1,
		},
		{
			name: "preserve thinking block (Anthropic passthrough)",
			content: []interface{}{
				map[string]interface{}{"type": "text", "text": "Hello"},
				map[string]interface{}{"type": "thinking", "thinking": "Let me think..."},
			},
			allowedTypes: supportedAnthropicBlockTypes, // includes thinking
			wantText: true,
			wantLen:  2,
		},
		{
			name: "preserve text block",
			content: []interface{}{
				map[string]interface{}{"type": "text", "text": "Hello world"},
			},
			allowedTypes: openaiAllowedTypes,
			wantText: true,
			wantLen:  1,
		},
		{
			name: "preserve tool_use block",
			content: []interface{}{
				map[string]interface{}{"type": "text", "text": "Let me use a tool"},
				map[string]interface{}{"type": "tool_use", "id": "toolu_1", "name": "search", "input": map[string]interface{}{}},
			},
			allowedTypes: openaiAllowedTypes,
			wantText: true,
			wantLen:  2,
		},
		{
			name:     "empty array returns empty text block",
			content:  []interface{}{},
			allowedTypes: openaiAllowedTypes,
			wantText: true,
			wantLen:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := filterUnsupportedContentBlocks(tt.content, tt.allowedTypes)

			if tt.wantLen == 1 && tt.wantText {
				blocks, ok := result.([]interface{})
				if !ok {
					t.Fatalf("result is not a slice")
				}
				if len(blocks) != 1 {
					t.Errorf("len = %d, want 1", len(blocks))
				}
				if block, ok := blocks[0].(map[string]interface{}); ok {
					if block["type"] != "text" {
						t.Errorf("block type = %v, want text", block["type"])
					}
				}
			} else if blocks, ok := result.([]interface{}); ok {
				if len(blocks) != tt.wantLen {
					t.Errorf("len = %d, want %d", len(blocks), tt.wantLen)
				}
			}
		})
	}
}

func TestFilterMessagesContentBlocks(t *testing.T) {
	openaiAllowedTypes := []string{"text", "image", "tool_use", "tool_result"}

	messages := []interface{}{
		map[string]interface{}{
			"role":    "user",
			"content": "Hello",
		},
		map[string]interface{}{
			"role": "assistant",
			"content": []interface{}{
				map[string]interface{}{"type": "thinking", "thinking": "Let me think..."},
				map[string]interface{}{"type": "text", "text": "I think it's 42"},
			},
		},
		map[string]interface{}{
			"role": "assistant",
			"content": []interface{}{
				map[string]interface{}{"type": "redacted_thinking", "thinking": "..."},
				map[string]interface{}{"type": "thinking", "thinking": "..."},
				map[string]interface{}{"type": "text", "text": "Final answer"},
			},
		},
	}

	// OpenAI→Anthropic 转换场景：thinking 块应被过滤
	result := filterMessagesContentBlocks(messages, openaiAllowedTypes)

	resultMsgs, ok := result.([]interface{})
	if !ok {
		t.Fatalf("result is not a slice")
	}

	if len(resultMsgs) != 3 {
		t.Errorf("message count = %d, want 3", len(resultMsgs))
	}

	// 第二个消息应该只剩下 text block
	msg2 := resultMsgs[1].(map[string]interface{})
	blocks2 := msg2["content"].([]interface{})
	if len(blocks2) != 1 {
		t.Errorf("second message content blocks = %d, want 1", len(blocks2))
	}

	// 第三个消息应该只剩下 text block
	msg3 := resultMsgs[2].(map[string]interface{})
	blocks3 := msg3["content"].([]interface{})
	if len(blocks3) != 1 {
		t.Errorf("third message content blocks = %d, want 1", len(blocks3))
	}
}

func TestFilterUnsupportedOpenAIParams(t *testing.T) {
	req := map[string]interface{}{
		"model":              "gpt-4o",
		"messages":           []interface{}{},
		"temperature":        0.7,
		"structured_outputs": true,
		"response_format":    map[string]interface{}{"type": "json_object"},
	}

	filterUnsupportedOpenAIParams(req)

	// structured_outputs 应该被过滤
	if _, ok := req["structured_outputs"]; ok {
		t.Error("structured_outputs should be filtered out")
	}
	// response_format 不在过滤列表中，会保留（可选参数）
	if _, ok := req["response_format"]; !ok {
		t.Error("response_format should be preserved (not in filter list)")
	}
	// 标准参数应该保留
	if _, ok := req["model"]; !ok {
		t.Error("model should be preserved")
	}
	if _, ok := req["temperature"]; !ok {
		t.Error("temperature should be preserved")
	}
}
