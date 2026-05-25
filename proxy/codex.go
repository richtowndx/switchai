package proxy

import (
	"encoding/json"
	"fmt"
	"strings"

	"switchai/config"
	"switchai/logger"
)

// ============================================================
// Codex Model Mapping (from Python proxy.py)
// ============================================================

var codexModelMapping = map[string]string{
	"glm-5":         "haiku_model",
	"gpt-4":         "default_model",
	"gpt-4-turbo":   "default_model",
	"gpt-4o":        "default_model",
	"gpt-4o-mini":   "default_model",
	"gpt-3.5-turbo": "default_model",
	"gpt-5.2-codex": "sonet_model",
	"gpt-5.3-codex": "opus_model",
}

// applyCodexModelMapping maps a Responses API model → Chat model, then resolves
// through the provider's model table to get the final deployment model name.
func applyCodexModelMapping(model string, provider *config.Provider) string {
	mapped, ok := codexModelMapping[model]
	if !ok {
		mapped = "default_model"
	}

	if provider == nil {
		return "glm-5" // fallback to a reasonable default if provider is missing
	}

	resolved := provider.ResolveModel(mapped)
	if resolved != mapped {
		logger.Info("Codex model: %s → %s → %s (provider: %s)", model, mapped, resolved, provider.Name)
	}
	return resolved
}

// ============================================================
// Responses API → Chat Completions (Request)
// ============================================================

// convertCodexToChat converts a Responses API request body to Chat Completions format.
// provider is used for model resolution (codex model name → provider-specific deployment name).
func convertCodexToChat(req map[string]interface{}, provider *config.Provider) map[string]interface{} {
	chatBody := make(map[string]interface{})

	// Model mapping with full resolution chain
	if model, ok := req["model"].(string); ok {
		chatBody["model"] = applyCodexModelMapping(model, provider)
	} else {
		chatBody["model"] = applyCodexModelMapping("default_model", provider)
	}

	messages := make([]interface{}, 0)

	// instructions → system
	if instructions, ok := req["instructions"].(string); ok && instructions != "" {
		messages = append(messages, map[string]interface{}{
			"role":    "system",
			"content": instructions,
		})
	}

	// input → messages
	if input, ok := req["input"]; ok {
		switch v := input.(type) {
		case string:
			messages = append(messages, map[string]interface{}{
				"role":    "user",
				"content": v,
			})
		case []interface{}:
			for _, item := range v {
				itemMap, ok := item.(map[string]interface{})
				if !ok {
					continue
				}
				itemType, _ := itemMap["type"].(string)
				switch itemType {
				case "message":
					role, _ := itemMap["role"].(string)
					if role == "developer" {
						role = "system"
					}
					content := extractResponsesTextContent(itemMap["content"])


						// Special handling for system messages when there are pending tool_calls.
						// OpenAI API requires that tool_calls be resolved by tool messages
						// before any other message type appears. If we encounter a system
						// message while there are pending tool_calls, we skip it to avoid
						// breaking the message sequence required by OpenAI API.
						if role == "system" && len(messages) > 0 {
							if last, ok := messages[len(messages)-1].(map[string]interface{}); ok {
								if last["role"] == "assistant" {
									if _, hasTC := last["tool_calls"]; hasTC {
										// Skip this system message to maintain valid message sequence
										continue
									}
								}
							}
						}
					// Merge assistant text into the last assistant message when the
					// previous item was a function_call (which created an assistant
					// message with tool_calls). Responses API interleaves function_call
					// and message(assistant) items, but both OpenAI Chat and Anthropic
					// Messages APIs require tool_calls to be resolved by tool messages
					// before the next assistant message appears.
					if role == "assistant" && len(messages) > 0 {
						if last, ok := messages[len(messages)-1].(map[string]interface{}); ok && last["role"] == "assistant" {
							if _, hasTC := last["tool_calls"]; hasTC {
								existingContent, _ := last["content"].(string)
								if existingContent != "" && content != "" {
									last["content"] = existingContent + content
								} else if content != "" {
									last["content"] = content
								}
								continue
							}
						}
					}

					messages = append(messages, map[string]interface{}{
						"role":    role,
						"content": content,
					})
				case "function_call":
					callID, _ := itemMap["call_id"].(string)
					if callID == "" {
						callID, _ = itemMap["id"].(string)
					}
					name, _ := itemMap["name"].(string)
					arguments, _ := itemMap["arguments"].(string)

					toolCall := map[string]interface{}{
						"id":   callID,
						"type": "function",
						"function": map[string]interface{}{
							"name":      name,
							"arguments": arguments,
						},
					}

						// Merge tool_call into the last assistant message if one exists.
						// Responses API interleaves function_call and message items, but
						// both OpenAI Chat and Anthropic APIs require all tool_calls
						// to be grouped in a single assistant message per round.
						if len(messages) > 0 {
							if last, ok := messages[len(messages)-1].(map[string]interface{}); ok && last["role"] == "assistant" {
								existingCalls, _ := last["tool_calls"].([]interface{})
								last["tool_calls"] = append(existingCalls, toolCall)
								continue
							}
						}
					messages = append(messages, map[string]interface{}{
						"role":    "assistant",
						"content": nil,
						"tool_calls": []interface{}{toolCall},
					})
				case "function_call_output":
					callID, _ := itemMap["call_id"].(string)
					output, _ := itemMap["output"].(string)
					content := output
					if content == "" {
						if c, ok := itemMap["content"].(string); ok {
							content = c
						}
					}
					messages = append(messages, map[string]interface{}{
						"role":         "tool",
						"tool_call_id": callID,
						"content":      content,
					})
				}
			}
		case map[string]interface{}:
			if msgs, ok := v["messages"]; ok {
				if msgArr, ok := msgs.([]interface{}); ok {
					for _, msg := range msgArr {
						if msgMap, ok := msg.(map[string]interface{}); ok {
							role, _ := msgMap["role"].(string)
							if role == "developer" {
								role = "system"
							}
							messages = append(messages, map[string]interface{}{
								"role":    role,
								"content": msgMap["content"],
							})
						}
					}
				}
			} else if content, ok := v["content"].(string); ok {
				messages = append(messages, map[string]interface{}{
					"role":    "user",
					"content": content,
				})
			}
		}
	}

	chatBody["messages"] = messages

	// Passthrough common parameters
	for _, key := range []string{"temperature", "top_p", "max_tokens", "stream",
		"frequency_penalty", "presence_penalty", "stop"} {
		if v, ok := req[key]; ok {
			chatBody[key] = v
		}
	}

	// Tools conversion
	if tools, ok := req["tools"].([]interface{}); ok {
		chatTools := make([]interface{}, 0)
		for _, tool := range tools {
			toolMap, ok := tool.(map[string]interface{})
			if !ok {
				continue
			}
			toolType, _ := toolMap["type"].(string)

			// Skip unsupported tool types
			if toolType == "web_search" || toolType == "code_interpreter" ||
				toolType == "file_search" || toolType == "computer_use" {
				continue
			}

			if toolType == "function" {
				if _, hasFunc := toolMap["function"]; hasFunc {
					// Already Chat Completions format
					chatTools = append(chatTools, toolMap)
				} else {
					// Responses format: function def at top level
					chatTool := map[string]interface{}{
						"type":     "function",
						"function": make(map[string]interface{}),
					}
					if name, ok := toolMap["name"]; ok {
						chatTool["function"].(map[string]interface{})["name"] = name
					}
					if desc, ok := toolMap["description"]; ok {
						chatTool["function"].(map[string]interface{})["description"] = desc
					}
					if params, ok := toolMap["parameters"]; ok {
						chatTool["function"].(map[string]interface{})["parameters"] = params
					}
					chatTools = append(chatTools, chatTool)
				}
			} else {
				// Unknown format, pass through if it has function field
				if _, hasFunc := toolMap["function"]; hasFunc {
					chatTools = append(chatTools, toolMap)
				}
			}
		}
		if len(chatTools) > 0 {
			chatBody["tools"] = chatTools
		}
	}

	// Passthrough tool_choice
	if toolChoice, ok := req["tool_choice"]; ok {
		chatBody["tool_choice"] = toolChoice
	}

	return chatBody
}

// extractResponsesTextContent extracts text from Responses API content blocks (array of content items).
func extractResponsesTextContent(content interface{}) string {
	if content == nil {
		return ""
	}
	if s, ok := content.(string); ok {
		return s
	}
	arr, ok := content.([]interface{})
	if !ok {
		return ""
	}
	var parts []string
	for _, item := range arr {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t == "input_text" {
			if text, ok := m["text"].(string); ok {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, " ")
}

// ============================================================
// Chat Completions → Responses API (Non-streaming Response)
// ============================================================

// convertChatToCodex converts a Chat Completions response body to Responses API format.
func convertChatToCodex(respBody []byte) []byte {
	var resp map[string]interface{}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return respBody // passthrough on parse error
	}

	outputs := make([]interface{}, 0)

	if choices, ok := resp["choices"].([]interface{}); ok {
		for _, choice := range choices {
			c, ok := choice.(map[string]interface{})
			if !ok {
				continue
			}
			msg, _ := c["message"].(map[string]interface{})
			contentText, _ := msg["content"].(string)

			contentArr := make([]interface{}, 0)
			if contentText != "" {
				contentArr = append(contentArr, map[string]interface{}{
					"type": "output_text",
					"text": contentText,
				})
			}

			if toolCalls, ok := msg["tool_calls"].([]interface{}); ok {
				for _, tc := range toolCalls {
					tcm, ok := tc.(map[string]interface{})
					if !ok {
						continue
					}
					fn, _ := tcm["function"].(map[string]interface{})
					tcID, _ := tcm["id"].(string)
					contentArr = append(contentArr, map[string]interface{}{
						"type":      "tool_call",
						"id":        tcID,
						"call_id":   tcID,
						"name":      fn["name"],
						"arguments": fn["arguments"],
					})
				}
			}

			respID, _ := resp["id"].(string)
			role, _ := msg["role"].(string)
			if role == "" {
				role = "assistant"
			}

			outputs = append(outputs, map[string]interface{}{
				"type":    "message",
				"id":      "msg_" + respID,
				"status":  "completed",
				"role":    role,
				"content": contentArr,
			})
		}
	}

	result := map[string]interface{}{
		"id":      resp["id"],
		"object":  "response",
		"created": resp["created"],
		"model":   resp["model"],
		"output":  outputs,
		"usage":   resp["usage"],
		"status":  "completed",
	}

	converted, _ := json.Marshal(result)
	return converted
}

// ============================================================
// Streaming: Chat Completions SSE → Responses API SSE
// ============================================================

type codexToolCallState struct {
	id        string
	name      string
	arguments string
}

// codexStreamConverter converts Chat Completions SSE chunks to Responses API SSE events.
// It maintains state across chunks (response ID, accumulated content, tool calls, etc.).
type codexStreamConverter struct {
	responseID    string
	createdAt     int64
	model         string
	itemID        string
	contentPartID string
	fullContent   string
	toolCalls     map[int]*codexToolCallState
	sequenceNum   int
	initialized   bool
	finished      bool // true after finish_reason is processed
}

func newCodexStreamConverter() *codexStreamConverter {
	return &codexStreamConverter{
		toolCalls: make(map[int]*codexToolCallState),
	}
}

// ConvertLine converts a single SSE line from Chat Completions format to Responses API events.
// Returns zero or more complete SSE event strings (each ending with \n\n).
func (c *codexStreamConverter) ConvertLine(line string) []string {
	if !strings.HasPrefix(line, "data: ") {
		return []string{line + "\n"}
	}

	data := strings.TrimPrefix(line, "data: ")
	if data == "[DONE]" {
		return c.flushFinal()
	}

	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return []string{line + "\n"}
	}

	var results []string

	// First chunk: send initialization events
	if !c.initialized {
		c.initialize(chunk)
		results = append(results, c.buildCreatedEvent())
		results = append(results, c.buildItemAddedEvent())
		results = append(results, c.buildContentPartAddedEvent())
		c.initialized = true
	}

	if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
		choice, ok := choices[0].(map[string]interface{})
		if !ok {
			return results
		}
		delta, _ := choice["delta"].(map[string]interface{})

		// Content delta
		if content, ok := delta["content"].(string); ok && content != "" {
			c.fullContent += content
			results = append(results, c.buildTextDeltaEvent(content))
		}

		// Tool calls delta
		if toolCalls, ok := delta["tool_calls"].([]interface{}); ok {
			for _, tc := range toolCalls {
				tcm, ok := tc.(map[string]interface{})
				if !ok {
					continue
				}
				idxFloat, _ := tcm["index"].(float64)
				idx := int(idxFloat)

				tcID, _ := tcm["id"].(string)
				tcFn, _ := tcm["function"].(map[string]interface{})
				tcName, _ := tcFn["name"].(string)
				tcArgs, _ := tcFn["arguments"].(string)

				if _, exists := c.toolCalls[idx]; !exists {
					c.toolCalls[idx] = &codexToolCallState{
						id:   tcID,
						name: tcName,
					}
					results = append(results, c.buildToolCallItemAdded(idx, tcID, tcName))
				}

				if tcArgs != "" {
					c.toolCalls[idx].arguments += tcArgs
					results = append(results, c.buildFunctionCallDelta(idx, tcID, tcArgs))
				}
			}
		}

		// Finish reason
		if finishReason, ok := choice["finish_reason"].(string); ok && finishReason != "" && !c.finished {
			c.finished = true
			results = append(results, c.buildDoneEvents(finishReason)...)
		}
	}

	return results
}

// flushFinal returns the response.completed event and [DONE] marker.
// Called when the upstream sends [DONE].
func (c *codexStreamConverter) flushFinal() []string {
	var results []string

	// If finish_reason was never processed (interrupted stream), send done events now
	if !c.finished && c.initialized {
		results = append(results, c.buildDoneEvents("stop")...)
		c.finished = true
	}

	// Build response.completed
	outputs := make([]interface{}, 0)
	if c.fullContent != "" && c.itemID != "" {
		outputs = append(outputs, map[string]interface{}{
			"type":   "message",
			"id":     c.itemID,
			"status": "completed",
			"role":   "assistant",
			"content": []interface{}{
				map[string]interface{}{
					"type": "output_text",
					"text": c.fullContent,
				},
			},
		})
	}
	for _, tc := range c.toolCalls {
		outputs = append(outputs, map[string]interface{}{
			"type":      "function_call",
			"id":        "fc_" + tc.id,
			"call_id":   tc.id,
			"name":      tc.name,
			"arguments": tc.arguments,
			"status":    "completed",
		})
	}

	completedEvent := map[string]interface{}{
		"type":            "response.completed",
		"sequence_number": c.sequenceNum,
		"response": map[string]interface{}{
			"id":         c.responseID,
			"object":     "response",
			"created_at": c.createdAt,
			"model":      c.model,
			"output":     outputs,
			"status":     "completed",
		},
	}
	c.sequenceNum++
	results = append(results, fmt.Sprintf("event: response.completed\ndata: %s\n\n", toJSON(completedEvent)))
	results = append(results, "data: [DONE]\n\n")

	return results
}

// initialize sets up state from the first Chat Completions chunk.
func (c *codexStreamConverter) initialize(chunk map[string]interface{}) {
	c.responseID, _ = chunk["id"].(string)
	if !strings.HasPrefix(c.responseID, "resp_") {
		c.responseID = "resp_" + c.responseID
	}
	if created, ok := chunk["created"].(float64); ok {
		c.createdAt = int64(created)
	}
	c.model, _ = chunk["model"].(string)
	c.itemID = "msg_" + c.responseID
	c.contentPartID = "cp_" + c.responseID
}

// ---- SSE event builders ----

func (c *codexStreamConverter) buildCreatedEvent() string {
	event := map[string]interface{}{
		"type":            "response.created",
		"sequence_number": c.sequenceNum,
		"response": map[string]interface{}{
			"id":         c.responseID,
			"object":     "response",
			"created_at": c.createdAt,
			"model":      c.model,
			"output":     []interface{}{},
			"status":     "in_progress",
		},
	}
	c.sequenceNum++
	return fmt.Sprintf("event: response.created\ndata: %s\n\n", toJSON(event))
}

func (c *codexStreamConverter) buildItemAddedEvent() string {
	event := map[string]interface{}{
		"type":            "response.output_item.added",
		"sequence_number": c.sequenceNum,
		"output_index":    0,
		"item": map[string]interface{}{
			"type":    "message",
			"id":      c.itemID,
			"status":  "in_progress",
			"role":    "assistant",
			"content": []interface{}{},
		},
	}
	c.sequenceNum++
	return fmt.Sprintf("event: response.output_item.added\ndata: %s\n\n", toJSON(event))
}

func (c *codexStreamConverter) buildContentPartAddedEvent() string {
	event := map[string]interface{}{
		"type":            "response.content_part.added",
		"sequence_number": c.sequenceNum,
		"output_index":    0,
		"content_index":   0,
		"item_id":         c.itemID,
		"content_part": map[string]interface{}{
			"type": "output_text",
			"text": "",
		},
	}
	c.sequenceNum++
	return fmt.Sprintf("event: response.content_part.added\ndata: %s\n\n", toJSON(event))
}

func (c *codexStreamConverter) buildTextDeltaEvent(text string) string {
	event := map[string]interface{}{
		"type":            "response.output_text.delta",
		"sequence_number": c.sequenceNum,
		"output_index":    0,
		"content_index":   0,
		"item_id":         c.itemID,
		"delta":           text,
	}
	c.sequenceNum++
	return fmt.Sprintf("event: response.output_text.delta\ndata: %s\n\n", toJSON(event))
}

func (c *codexStreamConverter) buildToolCallItemAdded(idx int, id, name string) string {
	event := map[string]interface{}{
		"type":            "response.output_item.added",
		"sequence_number": c.sequenceNum,
		"output_index":    idx + 1,
		"item": map[string]interface{}{
			"type":      "function_call",
			"id":        "fc_" + id,
			"call_id":   id,
			"name":      name,
			"arguments": "",
			"status":    "in_progress",
		},
	}
	c.sequenceNum++
	return fmt.Sprintf("event: response.output_item.added\ndata: %s\n\n", toJSON(event))
}

func (c *codexStreamConverter) buildFunctionCallDelta(idx int, id, delta string) string {
	event := map[string]interface{}{
		"type":            "response.function_call_arguments.delta",
		"sequence_number": c.sequenceNum,
		"output_index":    idx + 1,
		"item_id":         "fc_" + id,
		"delta":           delta,
		"call_id":         id,
	}
	c.sequenceNum++
	return fmt.Sprintf("event: response.function_call_arguments.delta\ndata: %s\n\n", toJSON(event))
}

// buildDoneEvents returns the per-item completion events triggered by finish_reason.
// These are separate from response.completed which is sent on [DONE].
func (c *codexStreamConverter) buildDoneEvents(finishReason string) []string {
	var results []string

	// Tool call done events
	if finishReason == "tool_calls" {
		for idx, tc := range c.toolCalls {
			// function_call_arguments.done
			results = append(results, fmt.Sprintf("event: response.function_call_arguments.done\ndata: %s\n\n",
				toJSON(map[string]interface{}{
					"type":            "response.function_call_arguments.done",
					"sequence_number": c.sequenceNum,
					"output_index":    idx + 1,
					"item_id":         "fc_" + tc.id,
					"arguments":       tc.arguments,
					"call_id":         tc.id,
				}),
			))
			c.sequenceNum++

			// output_item.done for function_call
			results = append(results, fmt.Sprintf("event: response.output_item.done\ndata: %s\n\n",
				toJSON(map[string]interface{}{
					"type":            "response.output_item.done",
					"sequence_number": c.sequenceNum,
					"output_index":    idx + 1,
					"item": map[string]interface{}{
						"type":      "function_call",
						"id":        "fc_" + tc.id,
						"call_id":   tc.id,
						"name":      tc.name,
						"arguments": tc.arguments,
						"status":    "completed",
					},
				}),
			))
			c.sequenceNum++
		}
	}

	// Content done events
	if c.fullContent != "" {
		// output_text.done
		results = append(results, fmt.Sprintf("event: response.output_text.done\ndata: %s\n\n",
			toJSON(map[string]interface{}{
				"type":            "response.output_text.done",
				"sequence_number": c.sequenceNum,
				"output_index":    0,
				"content_index":   0,
				"item_id":         c.itemID,
				"text":            c.fullContent,
			}),
		))
		c.sequenceNum++

		// content_part.done
		results = append(results, fmt.Sprintf("event: response.content_part.done\ndata: %s\n\n",
			toJSON(map[string]interface{}{
				"type":            "response.content_part.done",
				"sequence_number": c.sequenceNum,
				"output_index":    0,
				"content_index":   0,
				"item_id":         c.itemID,
				"content_part": map[string]interface{}{
					"type": "output_text",
					"text": c.fullContent,
				},
			}),
		))
		c.sequenceNum++

		// output_item.done for message
		results = append(results, fmt.Sprintf("event: response.output_item.done\ndata: %s\n\n",
			toJSON(map[string]interface{}{
				"type":            "response.output_item.done",
				"sequence_number": c.sequenceNum,
				"output_index":    0,
				"item": map[string]interface{}{
					"type":   "message",
					"id":     c.itemID,
					"status": "completed",
					"role":   "assistant",
					"content": []interface{}{
						map[string]interface{}{
							"type": "output_text",
							"text": c.fullContent,
						},
					},
				},
			}),
		))
		c.sequenceNum++
	}

	return results
}

// toJSON marshals v to a JSON string. Panics on failure (should never happen with map literals).
func toJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic("codex: json marshal failed: " + err.Error())
	}
	return string(b)
}
