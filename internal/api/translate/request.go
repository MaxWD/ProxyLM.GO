package translate

import (
	"encoding/json"
	"fmt"
)

// OpenAIChatToAnthropicMessages переводит тело OpenAI /v1/chat/completions
// запроса в формат Anthropic /v1/messages.
func OpenAIChatToAnthropicMessages(body []byte) ([]byte, error) {
	var oai map[string]any
	if err := json.Unmarshal(body, &oai); err != nil {
		return nil, fmt.Errorf("translate: invalid JSON: %w", err)
	}

	ant := make(map[string]any)

	// model — passthrough
	if v, ok := oai["model"]; ok {
		ant["model"] = v
	}

	// max_tokens (required for Anthropic)
	if v, ok := oai["max_tokens"]; ok {
		ant["max_tokens"] = v
	} else if v, ok := oai["max_completion_tokens"]; ok {
		ant["max_tokens"] = v
	} else {
		ant["max_tokens"] = defaultMaxTokens
	}

	// stream — passthrough
	if v, ok := oai["stream"]; ok {
		ant["stream"] = v
	}

	// temperature, top_p — passthrough
	for _, k := range []string{"temperature", "top_p"} {
		if v, ok := oai[k]; ok {
			ant[k] = v
		}
	}

	// stop → stop_sequences
	if v, ok := oai["stop"]; ok {
		ant["stop_sequences"] = normalizeStopSequences(v)
	}

	// messages: extract system, convert content
	if msgs, ok := oai["messages"]; ok {
		system, remaining := extractSystemMessages(msgs)
		if system != "" {
			ant["system"] = system
		}
		ant["messages"] = convertOAIMessagesToAnthropic(remaining)
	}

	// tools → Anthropic tool format
	if tools, ok := oai["tools"]; ok {
		ant["tools"] = convertOAIToolsToAnthropic(tools)
	}

	// metadata.user_id from user field
	if v, ok := oai["user"]; ok {
		ant["metadata"] = map[string]any{"user_id": v}
	}

	return json.Marshal(ant)
}

// AnthropicMessagesToOpenAIChat переводит тело Anthropic /v1/messages
// запроса в формат OpenAI /v1/chat/completions.
func AnthropicMessagesToOpenAIChat(body []byte) ([]byte, error) {
	var ant map[string]any
	if err := json.Unmarshal(body, &ant); err != nil {
		return nil, fmt.Errorf("translate: invalid JSON: %w", err)
	}

	oai := make(map[string]any)

	// model — passthrough
	if v, ok := ant["model"]; ok {
		oai["model"] = v
	}

	// max_tokens — passthrough
	if v, ok := ant["max_tokens"]; ok {
		oai["max_tokens"] = v
	}

	// stream — passthrough
	if v, ok := ant["stream"]; ok {
		oai["stream"] = v
	}

	// temperature, top_p, top_k — passthrough (top_k не поддерживается OpenAI, но передадим)
	for _, k := range []string{"temperature", "top_p"} {
		if v, ok := ant[k]; ok {
			oai[k] = v
		}
	}

	// stop_sequences → stop
	if v, ok := ant["stop_sequences"]; ok {
		oai["stop"] = v
	}

	// system → первое сообщение с role=system
	messages := make([]any, 0)
	if sys, ok := ant["system"]; ok {
		sysStr := flattenSystem(sys)
		if sysStr != "" {
			messages = append(messages, map[string]any{
				"role":    "system",
				"content": sysStr,
			})
		}
	}

	// messages → convert content blocks
	if msgs, ok := ant["messages"]; ok {
		messages = append(messages, convertAnthropicMessagesToOAI(msgs)...)
	}
	oai["messages"] = messages

	// tools → OpenAI function format
	if tools, ok := ant["tools"]; ok {
		oai["tools"] = convertAnthropicToolsToOAI(tools)
	}

	// strip Anthropic-only fields
	// thinking, metadata, cache_control — не передаём в OpenAI

	return json.Marshal(oai)
}

func extractSystemMessages(msgs any) (string, []any) {
	arr, ok := msgs.([]any)
	if !ok {
		return "", nil
	}
	var system string
	remaining := make([]any, 0, len(arr))
	for _, m := range arr {
		msg, ok := m.(map[string]any)
		if !ok {
			remaining = append(remaining, m)
			continue
		}
		role, _ := msg["role"].(string)
		if role == "system" {
			content, _ := msg["content"].(string)
			if system != "" {
				system += "\n"
			}
			system += content
		} else {
			remaining = append(remaining, m)
		}
	}
	return system, remaining
}

func convertOAIMessagesToAnthropic(msgs []any) []any {
	result := make([]any, 0, len(msgs))
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			result = append(result, m)
			continue
		}
		converted := make(map[string]any)
		role, _ := msg["role"].(string)
		if role == "function" || role == "tool" {
			converted["role"] = "user"
			converted["content"] = convertToolResultToAnthropic(msg)
		} else {
			converted["role"] = role
			converted["content"] = convertOAIContentToAnthropic(msg)
		}
		result = append(result, converted)
	}
	return result
}

func convertOAIContentToAnthropic(msg map[string]any) any {
	// tool_calls → content blocks
	if tc, ok := msg["tool_calls"]; ok {
		return convertToolCallsToAnthropicBlocks(msg, tc)
	}
	content := msg["content"]
	if content == nil {
		return []any{map[string]any{"type": "text", "text": ""}}
	}
	switch c := content.(type) {
	case string:
		return c
	default:
		return c
	}
}

func convertToolCallsToAnthropicBlocks(msg map[string]any, toolCalls any) []any {
	blocks := make([]any, 0)
	if text, ok := msg["content"].(string); ok && text != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": text})
	}
	tcs, ok := toolCalls.([]any)
	if !ok {
		return blocks
	}
	for _, tc := range tcs {
		tcMap, ok := tc.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := tcMap["function"].(map[string]any)
		if fn == nil {
			continue
		}
		id, _ := tcMap["id"].(string)
		name, _ := fn["name"].(string)
		argsStr, _ := fn["arguments"].(string)
		var input any
		if err := json.Unmarshal([]byte(argsStr), &input); err != nil {
			input = map[string]any{}
		}
		blocks = append(blocks, map[string]any{
			"type":  "tool_use",
			"id":    id,
			"name":  name,
			"input": input,
		})
	}
	return blocks
}

func convertToolResultToAnthropic(msg map[string]any) []any {
	content, _ := msg["content"].(string)
	toolCallID, _ := msg["tool_call_id"].(string)
	return []any{
		map[string]any{
			"type":        "tool_result",
			"tool_use_id": toolCallID,
			"content":     content,
		},
	}
}

func convertAnthropicMessagesToOAI(msgs any) []any {
	arr, ok := msgs.([]any)
	if !ok {
		return nil
	}
	result := make([]any, 0, len(arr))
	for _, m := range arr {
		msg, ok := m.(map[string]any)
		if !ok {
			result = append(result, m)
			continue
		}
		role, _ := msg["role"].(string)
		content := msg["content"]

		switch blocks := content.(type) {
		case string:
			result = append(result, map[string]any{"role": role, "content": blocks})
		case []any:
			converted := convertAnthropicBlocksToOAI(role, blocks)
			result = append(result, converted...)
		default:
			result = append(result, map[string]any{"role": role, "content": content})
		}
	}
	return result
}

func convertAnthropicBlocksToOAI(role string, blocks []any) []any {
	var textParts []string
	var toolCalls []any
	var toolResults []any

	for _, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok {
			continue
		}
		btype, _ := block["type"].(string)
		switch btype {
		case "text":
			text, _ := block["text"].(string)
			textParts = append(textParts, text)
		case "tool_use":
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			input := block["input"]
			argsBytes, _ := json.Marshal(input)
			toolCalls = append(toolCalls, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": string(argsBytes),
				},
			})
		case "tool_result":
			toolUseID, _ := block["tool_use_id"].(string)
			c, _ := block["content"].(string)
			toolResults = append(toolResults, map[string]any{
				"role":         "tool",
				"tool_call_id": toolUseID,
				"content":      c,
			})
		}
	}

	var result []any
	if len(toolCalls) > 0 {
		msg := map[string]any{"role": role}
		if len(textParts) > 0 {
			combined := ""
			for i, p := range textParts {
				if i > 0 {
					combined += "\n"
				}
				combined += p
			}
			msg["content"] = combined
		}
		msg["tool_calls"] = toolCalls
		result = append(result, msg)
	} else if len(textParts) > 0 {
		combined := ""
		for i, p := range textParts {
			if i > 0 {
				combined += "\n"
			}
			combined += p
		}
		result = append(result, map[string]any{"role": role, "content": combined})
	}
	result = append(result, toolResults...)
	return result
}

func convertOAIToolsToAnthropic(tools any) []any {
	arr, ok := tools.([]any)
	if !ok {
		return nil
	}
	result := make([]any, 0, len(arr))
	for _, t := range arr {
		tool, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := tool["function"].(map[string]any)
		if fn == nil {
			continue
		}
		name, _ := fn["name"].(string)
		desc, _ := fn["description"].(string)
		params := fn["parameters"]
		antTool := map[string]any{
			"name":         name,
			"input_schema": params,
		}
		if desc != "" {
			antTool["description"] = desc
		}
		result = append(result, antTool)
	}
	return result
}

func convertAnthropicToolsToOAI(tools any) []any {
	arr, ok := tools.([]any)
	if !ok {
		return nil
	}
	result := make([]any, 0, len(arr))
	for _, t := range arr {
		tool, ok := t.(map[string]any)
		if !ok {
			continue
		}
		name, _ := tool["name"].(string)
		desc, _ := tool["description"].(string)
		schema := tool["input_schema"]
		fn := map[string]any{"name": name}
		if desc != "" {
			fn["description"] = desc
		}
		if schema != nil {
			fn["parameters"] = schema
		}
		result = append(result, map[string]any{
			"type":     "function",
			"function": fn,
		})
	}
	return result
}

func normalizeStopSequences(v any) []any {
	switch s := v.(type) {
	case string:
		return []any{s}
	case []any:
		return s
	default:
		return nil
	}
}

func flattenSystem(sys any) string {
	switch s := sys.(type) {
	case string:
		return s
	case []any:
		var parts []string
		for _, block := range s {
			if bm, ok := block.(map[string]any); ok {
				if text, ok := bm["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		result := ""
		for i, p := range parts {
			if i > 0 {
				result += "\n"
			}
			result += p
		}
		return result
	default:
		return fmt.Sprintf("%v", s)
	}
}
