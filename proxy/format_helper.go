package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ============================================================
// 共享的格式转换函数
// 由于多个 Proxy 实现需要相同的转换逻辑，统一放在这里
// ============================================================

// convertOpenAIMessagesToAnthropic 将 OpenAI 消息格式转换为 Anthropic 格式
// 处理 tool_calls ↔ tool_use 以及 tool ↔ tool_result 的转换
func convertOpenAIMessagesToAnthropic(messages interface{}) []map[string]interface{} {
	msgs, ok := messages.([]interface{})
	if !ok {
		return nil
	}

	result := make([]map[string]interface{}, 0)

	for _, m := range msgs {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)

		if role == "system" {
			continue
		}

		if role == "tool" {
			// OpenAI tool → Anthropic tool_result content block
			// Anthropic 格式:
			//   {"role": "tool", "content": [{"type": "tool_result", "tool_use_id": "...", "content": "..."}]}
			toolCallID, _ := msg["tool_call_id"].(string)
			contentBlocks := []interface{}{
				map[string]interface{}{
					"type":        "tool_result",
					"tool_use_id": toolCallID,
					"content":     msg["content"],
				},
			}
			toolMsg := map[string]interface{}{
				"role":    "tool",
				"content": contentBlocks,
			}
			result = append(result, toolMsg)
			continue
		}

		converted := map[string]interface{}{
			"role":    role,
			"content": msg["content"],
		}

		// assistant 消息: 如果有 tool_calls，转换为 tool_use content blocks
		if role == "assistant" {
			if toolCalls, ok := msg["tool_calls"].([]interface{}); ok && len(toolCalls) > 0 {
				content := convertToolCallsToContent(msg["content"], toolCalls)
				converted["content"] = content
			}
		}

		result = append(result, converted)
	}

	return result
}

// convertToolCallsToContent 将 OpenAI tool_calls 转换为 Anthropic content blocks (含 tool_use)
// content 是 assistant 消息中原有的 content (可能为 null 或字符串)
func convertToolCallsToContent(content interface{}, toolCalls []interface{}) []interface{} {
	var blocks []interface{}

	// 保留原有的文本内容
	if text, ok := content.(string); ok && text != "" {
		blocks = append(blocks, map[string]interface{}{
			"type": "text",
			"text": text,
		})
	}

	// 转换每个 tool_call → tool_use block
	for _, tc := range toolCalls {
		tcMap, ok := tc.(map[string]interface{})
		if !ok {
			continue
		}
		fn, _ := tcMap["function"].(map[string]interface{})
		toolUse := map[string]interface{}{
			"type":  "tool_use",
			"id":    tcMap["id"],
			"name":  fn["name"],
			"input": parseJSONToMap(fn["arguments"]),
		}
		blocks = append(blocks, toolUse)
	}

	return blocks
}

// convertOpenAIToolsToAnthropic 将 OpenAI 工具格式转换为 Anthropic 格式
func convertOpenAIToolsToAnthropic(tools []interface{}) []map[string]interface{} {
	result := make([]map[string]interface{}, len(tools))
	for i, t := range tools {
		tm, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		fn, _ := tm["function"].(map[string]interface{})
		result[i] = map[string]interface{}{
			"name":         fn["name"],
			"description":  fn["description"],
			"input_schema": fn["parameters"],
		}
	}
	return result
}

// convertAnthropicMessagesToOpenAI 将 Anthropic 消息格式转换为 OpenAI 格式
// 处理 tool_use ↔ tool_calls 以及 tool_result ↔ tool 的转换
func convertAnthropicMessagesToOpenAI(messages interface{}, system interface{}) []interface{} {
	msgs, ok := messages.([]interface{})
	if !ok {
		return nil
	}

	result := make([]interface{}, 0)

	// 添加 system 消息
	if systemText, ok := system.(string); ok && systemText != "" {
		result = append(result, map[string]interface{}{
			"role":    "system",
			"content": systemText,
		})
	}

	for _, m := range msgs {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}

		role, _ := msg["role"].(string)

		if role == "tool" || role == "user" {
			// Anthropic tool → OpenAI tool
			// 支持两种 Anthropic 格式:
			//   1) {role: "tool", content: [{type: "tool_result", ...}]} — tool role
			//   2) {role: "user", content: [{type: "tool_result", ...}]} — user role + tool_result block
			//
			// OpenAI:   {role: "tool", tool_call_id: "...", content: "..."}
			// 每个 tool_result block → 一个独立 OpenAI tool 消息

			// 字符串 content → 普通 user 消息
			if s, ok := msg["content"].(string); ok {
				result = append(result, map[string]interface{}{
					"role":    role,
					"content": s,
				})
				continue
			}

			contentBlocks, ok := msg["content"].([]interface{})
			if !ok || len(contentBlocks) == 0 {
				continue
			}

			// 查找是否有 tool_result block
			hasToolResult := false
			for _, block := range contentBlocks {
				if b, ok := block.(map[string]interface{}); ok && b["type"] == "tool_result" {
					hasToolResult = true
					break
				}
			}
			if !hasToolResult {
				// 普通 user 消息，走标准转换
				converted := map[string]interface{}{
					"role":    role,
					"content": convertAnthropicContentToOpenAI(msg["content"]),
				}
				result = append(result, converted)
				continue
			}
			// 有 tool_result 块: 将 content 拆分为 user(text) 和 tool(tool_result) 消息
			// 收集 text 块（连续 text 块合并）
			var textBuffer []string
			flushText := func() {
				if len(textBuffer) > 0 {
					result = append(result, map[string]interface{}{
						"role":    "user",
						"content": strings.Join(textBuffer, "\n"),
					})
					textBuffer = nil
				}
			}
			for _, block := range contentBlocks {
				b, ok := block.(map[string]interface{})
				if !ok {
					continue
				}
				switch b["type"] {
				case "text":
					if t, ok := b["text"].(string); ok && t != "" {
						textBuffer = append(textBuffer, t)
					}
				case "tool_result":
					flushText()
					toolUseID, _ := b["tool_use_id"].(string)
					toolContent := flattenContent(b["content"])
					result = append(result, map[string]interface{}{
						"role":         "tool",
						"tool_call_id": toolUseID,
						"content":      toolContent,
					})
				}
			}
			flushText()
			continue
		}

		converted := map[string]interface{}{
			"role": role,
		}

		if role == "assistant" {
			// assistant 消息: 从 content 中提取 tool_use 转为 tool_calls
			textContent, toolCalls := extractToolUseFromContent(msg["content"])
			converted["content"] = textContent
			if len(toolCalls) > 0 {
				converted["tool_calls"] = toolCalls
			}
		} else {
			converted["content"] = convertAnthropicContentToOpenAI(msg["content"])
		}

		result = append(result, converted)
	}

	return result
}

// extractToolUseFromContent 从 Anthropic assistant content 中提取:
//   - text 内容（合并为字符串）
//   - tool_use 块（转为 OpenAI tool_calls 格式）
func extractToolUseFromContent(content interface{}) (interface{}, []interface{}) {
	// 如果是纯字符串，没有 tool_use
	if _, ok := content.(string); ok {
		return content, nil
	}

	blocks, ok := content.([]interface{})
	if !ok {
		return nil, nil
	}

	var textParts []string
	var toolCalls []interface{}

	for _, item := range blocks {
		block, ok := item.(map[string]interface{})
		if !ok {
			continue
		}

		switch block["type"] {
		case "text":
			if t, ok := block["text"].(string); ok && t != "" {
				textParts = append(textParts, t)
			}
		case "tool_use":
			tc := map[string]interface{}{
				"id":   block["id"],
				"type": "function",
				"function": map[string]interface{}{
					"name":      block["name"],
					"arguments": mapToJSONString(block["input"]),
				},
			}
			toolCalls = append(toolCalls, tc)
		}
	}

	// 合并文本内容
	var textContent interface{}
	if len(textParts) > 0 {
		textContent = strings.Join(textParts, "")
	}

	return textContent, toolCalls
}

// flattenContent 将 tool_result 中的内容展平为字符串
// Anthropic tool_result.content 可能是 string 或 []content_block
// OpenAI tool content 要求是 string
func flattenContent(content interface{}) string {
	if s, ok := content.(string); ok {
		return s
	}

	blocks, ok := content.([]interface{})
	if !ok {
		return ""
	}

	var parts []string
	for _, item := range blocks {
		block, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if block["type"] == "text" {
			if t, ok := block["text"].(string); ok {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "")
}

// mapToJSONString 将 map 转为 JSON 字符串
func mapToJSONString(m interface{}) string {
	if m == nil {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

// convertAnthropicContentToOpenAI 转换 Anthropic content 格式到 OpenAI 格式
// Anthropic: "text" 或 [{"type": "text", "text": "..."}, {"type": "image", "source": {...}}]
// OpenAI: "text" 或 [{"type": "text", "text": "..."}, {"type": "image_url", "image_url": {...}}]
func convertAnthropicContentToOpenAI(content interface{}) interface{} {
	// 如果是字符串，直接返回
	if s, ok := content.(string); ok {
		return s
	}

	// 如果是数组，需要转换每个 content block
	arr, ok := content.([]interface{})
	if !ok {
		return content
	}

	result := make([]interface{}, 0, len(arr))
	for _, item := range arr {
		block, ok := item.(map[string]interface{})
		if !ok {
			continue
		}

		blockType, _ := block["type"].(string)

		switch blockType {
		case "text":
			// 文本类型：直接转换
			result = append(result, map[string]interface{}{
				"type": "text",
				"text": block["text"],
			})

		case "image":
			// 图片类型：Anthropic "image" → OpenAI "image_url"
			// Anthropic 格式: {"type": "image", "source": {"type": "base64", "media_type": "...", "data": "..."}}
			// OpenAI 格式: {"type": "image_url", "image_url": {"url": "data:...;base64,..."}}
			if source, ok := block["source"].(map[string]interface{}); ok {
				mediaType, _ := source["media_type"].(string)
				data, _ := source["data"].(string)

				// 构建 data URI
				dataURL := "data:" + mediaType + ";base64," + data

				result = append(result, map[string]interface{}{
					"type": "image_url",
					"image_url": map[string]interface{}{
						"url": dataURL,
					},
				})
			}

		default:
			// 未知类型 - 跳过以避免上游 API 错误
			// 例如: tool_use, tool_result 等类型
			continue
		}
	}

	// 如果只有一个元素且是 text 类型，可以简化为字符串
	if len(result) == 1 {
		if first, ok := result[0].(map[string]interface{}); ok {
			if t, _ := first["type"].(string); t == "text" {
				return first["text"]
			}
		}
	}

	return result
}

// convertOpenAIResponseToAnthropic 将 OpenAI 响应格式转换为 Anthropic 格式
// 用于：客户端发送 Anthropic 请求，但上游是 OpenAI 格式（如 Copilot）
func convertOpenAIResponseToAnthropic(openaiResp []byte) ([]byte, error) {
	var resp map[string]interface{}
	if err := json.Unmarshal(openaiResp, &resp); err != nil {
		return nil, err
	}

	// 构建 Anthropic 格式响应
	anthropicResp := map[string]interface{}{
		"id":            resp["id"],
		"type":          "message",
		"role":          "assistant",
		"content":       []interface{}{},
		"model":         resp["model"],
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage": map[string]interface{}{
			"input_tokens":  0,
			"output_tokens": 0,
		},
	}

	// 处理 choices
	if choices, ok := resp["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			// 处理 finish_reason -> stop_reason
			if finishReason, ok := choice["finish_reason"].(string); ok {
				anthropicResp["stop_reason"] = mapOpenAIFinishReasonToAnthropic(finishReason)
			}

			// 处理 message content
			if msg, ok := choice["message"].(map[string]interface{}); ok {
				content := make([]interface{}, 0)

				// 文本内容
				if text, ok := msg["content"].(string); ok && text != "" {
					content = append(content, map[string]interface{}{
						"type": "text",
						"text": text,
					})
				}

				// Tool calls (如果有的话)
				if toolCalls, ok := msg["tool_calls"].([]interface{}); ok {
					for _, tc := range toolCalls {
						if tcMap, ok := tc.(map[string]interface{}); ok {
							if fn, ok := tcMap["function"].(map[string]interface{}); ok {
								toolUse := map[string]interface{}{
									"type":  "tool_use",
									"id":    tcMap["id"],
									"name":  fn["name"],
									"input": parseJSONToMap(fn["arguments"]),
								}
								content = append(content, toolUse)
							}
						}
					}
				}

				if len(content) > 0 {
					anthropicResp["content"] = content
				} else {
					// 空内容
					anthropicResp["content"] = []map[string]interface{}{
						{"type": "text", "text": ""},
					}
				}
			}
		}
	}

	// 处理 usage
	if usage, ok := resp["usage"].(map[string]interface{}); ok {
		if inputTokens, ok := usage["prompt_tokens"].(float64); ok {
			anthropicResp["usage"].(map[string]interface{})["input_tokens"] = int(inputTokens)
		}
		if outputTokens, ok := usage["completion_tokens"].(float64); ok {
			anthropicResp["usage"].(map[string]interface{})["output_tokens"] = int(outputTokens)
		}
	}

	return json.Marshal(anthropicResp)
}

// mapOpenAIFinishReasonToAnthropic 将 OpenAI 的 finish_reason 映射到 Anthropic 的 stop_reason
func mapOpenAIFinishReasonToAnthropic(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "content_filter"
	case "tool_calls":
		return "tool_use"
	default:
		return "end_turn"
	}
}

// parseJSONToMap 安全地解析 JSON 字符串到 map
func parseJSONToMap(jsonStr interface{}) map[string]interface{} {
	if jsonStr == nil {
		return make(map[string]interface{})
	}

	str, ok := jsonStr.(string)
	if !ok {
		return make(map[string]interface{})
	}

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(str), &result); err != nil {
		return make(map[string]interface{})
	}
	return result
}

// convertOpenAIStreamLineToAnthropic 将 OpenAI SSE 流行转换为 Anthropic 格式
// 输入：OpenAI 格式的 SSE 行（如 "data: {"id":"...","choices":[...]}"）
// 输出：Anthropic 格式的 SSE 行（如 "event: content_block_delta\ndata: {...}"）
func convertOpenAIStreamLineToAnthropic(line string) string {
	// 空行：SSE 事件边界，需要返回 \n\n
	if line == "" || line == "\n" {
		return "\n"
	}

	// 不以 "data: " 开头的行直接返回
	if !strings.HasPrefix(line, "data: ") {
		return line
	}

	// "[DONE]" 标记
	data := strings.TrimPrefix(line, "data: ")
	if data == "[DONE]" {
		return "event: message_stop\ndata: {\"type\": \"message_stop\"}\n\n"
	}

	// 解析 OpenAI SSE 数据
	var openaiChunk map[string]interface{}
	if err := json.Unmarshal([]byte(data), &openaiChunk); err != nil {
		// 解析失败，返回原始行
		return line
	}

	// 提取必要信息

	// 转换为 Anthropic 格式
	// OpenAI streaming format: {"id":"...","choices":[{"delta":{"content":"..."}}]}
	// Anthropic streaming format: event: content_block_delta\ndata: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"..."}}

	if choices, ok := openaiChunk["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if delta, ok := choice["delta"].(map[string]interface{}); ok {
				if content, ok := delta["content"].(string); ok {
					// 构造 Anthropic content_block_delta 事件
					anthropicEvent := map[string]interface{}{
						"type":  "content_block_delta",
						"index": 0,
						"delta": map[string]interface{}{
							"type": "text_delta",
							"text": content,
						},
					}

					eventData, _ := json.Marshal(anthropicEvent)
					return "event: content_block_delta\ndata: " + string(eventData) + "\n\n"
				}
			}

			// 检查 finish_reason
			if finishReason, ok := choice["finish_reason"].(string); ok && finishReason != "" {
				stopReason := mapOpenAIFinishReasonToAnthropic(finishReason)
				return fmt.Sprintf("event: message_stop\ndata: {\"type\":\"message_stop\",\"stop_reason\":\"%s\"}\n\n", stopReason)
			}
		}
	}

	// 无法转换，返回原始行
	return line
}

// convertAnthropicToolsToOpenAI 将 Anthropic 工具格式转换为 OpenAI 格式
func convertAnthropicToolsToOpenAI(tools []interface{}) []interface{} {
	result := make([]interface{}, len(tools))
	for i, t := range tools {
		tm, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		result[i] = map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        tm["name"],
				"description": tm["description"],
				"parameters":  tm["input_schema"],
			},
		}
	}
	return result
}
