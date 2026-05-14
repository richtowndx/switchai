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

		result = append(result, map[string]interface{}{
			"role":    role,
			"content": msg["content"],
		})
	}

	return result
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

		// 处理 content 字段格式转换
		content := convertAnthropicContentToOpenAI(msg["content"])
		result = append(result, map[string]interface{}{
			"role":    msg["role"],
			"content": content,
		})
	}

	return result
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
		"id":           resp["id"],
		"type":         "message",
		"role":         "assistant",
		"content":      []interface{}{},
		"model":        resp["model"],
		"stop_reason":  "end_turn",
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
	// 空行直接返回
	if line == "" || line == "\n" {
		return line
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
