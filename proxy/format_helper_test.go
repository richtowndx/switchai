package proxy

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestConvertOpenAIMessagesToAnthropic_ToolCalls(t *testing.T) {
	// 模拟 OpenAI 格式的 tool call 场景
	openaiMsgs := []interface{}{
		map[string]interface{}{"role": "system", "content": "You are a helpful assistant."},
		map[string]interface{}{"role": "user", "content": "What's the weather in NYC?"},
		map[string]interface{}{
			"role": "assistant",
			"content": nil,
			"tool_calls": []interface{}{
				map[string]interface{}{
					"id":   "call_1",
					"type": "function",
					"function": map[string]interface{}{
						"name":      "get_weather",
						"arguments": `{"city":"NYC"}`,
					},
				},
			},
		},
		map[string]interface{}{
			"role":         "tool",
			"tool_call_id": "call_1",
			"content":      "Sunny, 25°C",
		},
		map[string]interface{}{"role": "user", "content": "Thanks!"},
		map[string]interface{}{
			"role": "assistant",
			"content": "You're welcome!",
		},
	}

	result := convertOpenAIMessagesToAnthropic(openaiMsgs)

	b, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(b))

	// Verify: system message should be skipped
	if len(result) != 5 {
		t.Fatalf("expected 5 messages (skipped system), got %d", len(result))
	}

	// Verify: assistant with tool_calls → has tool_use content blocks
	assistantMsg := result[1]
	if role := assistantMsg["role"]; role != "assistant" {
		t.Fatalf("expected role 'assistant', got %v", role)
	}
	content := assistantMsg["content"].([]interface{})
	if len(content) != 1 {
		t.Fatalf("expected 1 content block (tool_use), got %d", len(content))
	}
	toolUse := content[0].(map[string]interface{})
	if toolUse["type"] != "tool_use" {
		t.Fatalf("expected content block type 'tool_use', got %v", toolUse["type"])
	}
	if toolUse["id"] != "call_1" {
		t.Fatalf("expected tool_use id 'call_1', got %v", toolUse["id"])
	}

	// Verify: tool message format
	toolMsg := result[2]
	if role := toolMsg["role"]; role != "tool" {
		t.Fatalf("expected role 'tool', got %v", role)
	}
	// Anthropic expects: content is array of tool_result blocks
	toolContent, ok := toolMsg["content"].([]interface{})
	if !ok {
		t.Fatalf("tool content should be []interface{}, got %T", toolMsg["content"])
	}
	if len(toolContent) != 1 {
		t.Fatalf("expected 1 tool_result block, got %d", len(toolContent))
	}
	toolResult := toolContent[0].(map[string]interface{})
	if toolResult["type"] != "tool_result" {
		t.Fatalf("expected block type 'tool_result', got %v", toolResult["type"])
	}
	if toolResult["tool_use_id"] != "call_1" {
		t.Fatalf("expected tool_use_id 'call_1', got %v", toolResult["tool_use_id"])
	}
}

func TestConvertOpenAIMessagesToAnthropic_Normal(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{"role": "user", "content": "Hello"},
		map[string]interface{}{"role": "assistant", "content": "Hi there!"},
	}
	result := convertOpenAIMessagesToAnthropic(msgs)
	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}
}

func TestConvertAnthropicMessagesToOpenAI_UserRoleToolResult(t *testing.T) {
	// Anthropic 标准格式: tool_result 在 user role 的消息中
	anthropicMsgs := []interface{}{
		map[string]interface{}{"role": "user", "content": "What is 2+2?"},
		map[string]interface{}{
			"role": "assistant",
			"content": []interface{}{
				map[string]interface{}{"type": "text", "text": "Let me calculate."},
				map[string]interface{}{
					"type":  "tool_use",
					"id":    "toolu_1",
					"name":  "calculator",
					"input": map[string]interface{}{"expression": "2+2"},
				},
			},
		},
		map[string]interface{}{
			"role": "user",
			"content": []interface{}{
				map[string]interface{}{
					"type":        "tool_result",
					"tool_use_id": "toolu_1",
					"content":     "4",
				},
			},
		},
		map[string]interface{}{"role": "user", "content": "Thanks!"},
	}

	system := "You are a calculator."
	result := convertAnthropicMessagesToOpenAI(anthropicMsgs, system)

	b, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(b))

	// system 消息应该在开头
	if len(result) < 1 {
		t.Fatal("empty result")
	}
	sysMsg := result[0].(map[string]interface{})
	if sysMsg["role"] != "system" || sysMsg["content"] != "You are a calculator." {
		t.Fatalf("system message not preserved: %v", sysMsg)
	}

	// assistant 消息应该有 tool_calls
	var assistantFound map[string]interface{}
	for _, msg := range result {
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
	tcMap := toolCalls[0].(map[string]interface{})
	if tcMap["id"] != "toolu_1" {
		t.Errorf("tool_call id = %v, want toolu_1", tcMap["id"])
	}
	fn := tcMap["function"].(map[string]interface{})
	if fn["name"] != "calculator" {
		t.Errorf("function name = %v, want calculator", fn["name"])
	}

	// user + tool_result → tool 角色消息
	var toolMsgFound map[string]interface{}
	for _, msg := range result {
		if m, ok := msg.(map[string]interface{}); ok && m["role"] == "tool" {
			toolMsgFound = m
			break
		}
	}
	if toolMsgFound == nil {
		t.Fatal("no tool role message found from tool_result")
	}
	if toolMsgFound["tool_call_id"] != "toolu_1" {
		t.Errorf("tool_call_id = %v, want toolu_1", toolMsgFound["tool_call_id"])
	}
	if toolMsgFound["content"] != "4" {
		t.Errorf("tool content = %v, want 4", toolMsgFound["content"])
	}

	// 最后的 user 消息应该是纯文本
	lastMsg := result[len(result)-1].(map[string]interface{})
	if lastMsg["role"] != "user" {
		t.Fatalf("last message role = %v, want user", lastMsg["role"])
	}
}

func TestConvertAnthropicMessagesToOpenAI_ToolRole(t *testing.T) {
	// tool role 格式的 tool_result
	anthropicMsgs := []interface{}{
		map[string]interface{}{"role": "user", "content": "Hi"},
		map[string]interface{}{
			"role": "assistant",
			"content": []interface{}{
				map[string]interface{}{
					"type":  "tool_use",
					"id":    "call_xyz",
					"name":  "search",
					"input": map[string]interface{}{"q": "weather"},
				},
			},
		},
		map[string]interface{}{
			"role": "tool",
			"content": []interface{}{
				map[string]interface{}{
					"type":        "tool_result",
					"tool_use_id": "call_xyz",
					"content":     "Sunny",
				},
			},
		},
	}

	result := convertAnthropicMessagesToOpenAI(anthropicMsgs, nil)
	b, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(b))

	var toolMsgFound map[string]interface{}
	for _, msg := range result {
		if m, ok := msg.(map[string]interface{}); ok && m["role"] == "tool" {
			toolMsgFound = m
			break
		}
	}
	if toolMsgFound == nil {
		t.Fatal("no tool role message found")
	}
	if toolMsgFound["tool_call_id"] != "call_xyz" {
		t.Errorf("tool_call_id = %v, want call_xyz", toolMsgFound["tool_call_id"])
	}
	if toolMsgFound["content"] != "Sunny" {
		t.Errorf("content = %v, want Sunny", toolMsgFound["content"])
	}
}

func TestConvertAnthropicMessagesToOpenAI_MixedUserAndToolResult(t *testing.T) {
	// user 消息同时包含 text + tool_result
	anthropicMsgs := []interface{}{
		map[string]interface{}{
			"role": "user",
			"content": []interface{}{
				map[string]interface{}{"type": "text", "text": "Based on the result,"},
				map[string]interface{}{
					"type":        "tool_result",
					"tool_use_id": "call_1",
					"content":     "42",
				},
				map[string]interface{}{"type": "text", "text": "what now?"},
			},
		},
	}

	result := convertAnthropicMessagesToOpenAI(anthropicMsgs, nil)
	b, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(b))

	// 应该有 3 条消息: user(text before), tool(result), user(text after)
	if len(result) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(result))
	}

	msg1 := result[0].(map[string]interface{})
	if msg1["role"] != "user" || msg1["content"] != "Based on the result," {
		t.Errorf("msg1 = %v, want user/'Based on the result,'", msg1)
	}

	msg2 := result[1].(map[string]interface{})
	if msg2["role"] != "tool" || msg2["tool_call_id"] != "call_1" || msg2["content"] != "42" {
		t.Errorf("msg2 = %v, want tool/call_1/42", msg2)
	}

	msg3 := result[2].(map[string]interface{})
	if msg3["role"] != "user" || msg3["content"] != "what now?" {
		t.Errorf("msg3 = %v, want user/'what now?'", msg3)
	}
}
