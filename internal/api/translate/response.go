package translate

import (
	"encoding/json"
	"fmt"
)

// OpenAIChatResponseToAnthropicMessage переводит non-streaming ответ
// OpenAI /v1/chat/completions в формат Anthropic /v1/messages.
func OpenAIChatResponseToAnthropicMessage(body []byte, model string) ([]byte, error) {
	var oai map[string]any
	if err := json.Unmarshal(body, &oai); err != nil {
		return nil, fmt.Errorf("translate response: invalid JSON: %w", err)
	}

	ant := map[string]any{
		"id":   randomID("msg_", 12),
		"type": "message",
		"role": "assistant",
	}

	if m, ok := oai["model"].(string); ok {
		ant["model"] = m
	} else {
		ant["model"] = model
	}

	// choices[0].message → content blocks
	content, finishReason := extractOAIChoice(oai)
	ant["content"] = content
	ant["stop_reason"] = mapOAIFinishReason(finishReason)
	ant["stop_sequence"] = nil

	// usage
	if usage, ok := oai["usage"].(map[string]any); ok {
		ant["usage"] = map[string]any{
			"input_tokens":  jsonNum(usage["prompt_tokens"]),
			"output_tokens": jsonNum(usage["completion_tokens"]),
		}
	}

	return json.Marshal(ant)
}

// AnthropicMessageToOpenAIChatResponse переводит non-streaming ответ
// Anthropic /v1/messages в формат OpenAI /v1/chat/completions.
func AnthropicMessageToOpenAIChatResponse(body []byte) ([]byte, error) {
	var ant map[string]any
	if err := json.Unmarshal(body, &ant); err != nil {
		return nil, fmt.Errorf("translate response: invalid JSON: %w", err)
	}

	oai := map[string]any{
		"id":      randomID("chatcmpl-", 12),
		"object":  "chat.completion",
		"created": 0,
	}

	if m, ok := ant["model"].(string); ok {
		oai["model"] = m
	}

	// content blocks → message
	message := map[string]any{"role": "assistant"}
	content, toolCalls := extractAnthropicContent(ant)
	message["content"] = content
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}

	stopReason, _ := ant["stop_reason"].(string)
	oai["choices"] = []any{
		map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": mapAnthropicStopReason(stopReason),
		},
	}

	// usage
	if usage, ok := ant["usage"].(map[string]any); ok {
		prompt := jsonNum(usage["input_tokens"])
		output := jsonNum(usage["output_tokens"])
		oai["usage"] = map[string]any{
			"prompt_tokens":     prompt,
			"completion_tokens": output,
			"total_tokens":      prompt + output,
		}
	}

	return json.Marshal(oai)
}

// PeekAnthropicUsage извлекает usage из Anthropic-ответа.
func PeekAnthropicUsage(body []byte) (prompt, output int) {
	var p struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(body, &p)
	return p.Usage.InputTokens, p.Usage.OutputTokens
}

func extractOAIChoice(oai map[string]any) ([]any, string) {
	choices, ok := oai["choices"].([]any)
	if !ok || len(choices) == 0 {
		return []any{map[string]any{"type": "text", "text": ""}}, ""
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return []any{map[string]any{"type": "text", "text": ""}}, ""
	}
	finishReason, _ := choice["finish_reason"].(string)
	message, ok := choice["message"].(map[string]any)
	if !ok {
		return []any{map[string]any{"type": "text", "text": ""}}, finishReason
	}

	var blocks []any

	if text, ok := message["content"].(string); ok && text != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": text})
	}

	if tcs, ok := message["tool_calls"].([]any); ok {
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
	}

	if len(blocks) == 0 {
		blocks = []any{map[string]any{"type": "text", "text": ""}}
	}
	return blocks, finishReason
}

func extractAnthropicContent(ant map[string]any) (string, []any) {
	blocks, ok := ant["content"].([]any)
	if !ok {
		return "", nil
	}
	var textParts []string
	var toolCalls []any
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
		}
	}
	combined := ""
	for i, p := range textParts {
		if i > 0 {
			combined += "\n"
		}
		combined += p
	}
	return combined, toolCalls
}

func mapOAIFinishReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "end_turn"
	default:
		return "end_turn"
	}
}

func mapAnthropicStopReason(reason string) string {
	switch reason {
	case "end_turn":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "stop_sequence":
		return "stop"
	default:
		return "stop"
	}
}

func jsonNum(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}
