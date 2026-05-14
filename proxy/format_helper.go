package proxy

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
		result = append(result, map[string]interface{}{
			"role":    msg["role"],
			"content": msg["content"],
		})
	}

	return result
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
