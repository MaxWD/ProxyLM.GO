package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOpenAIChatResponseToAnthropicMessage(t *testing.T) {
	input := `{
		"id": "chatcmpl-abc",
		"object": "chat.completion",
		"model": "gpt-4",
		"choices": [{
			"index": 0,
			"message": {"role": "assistant", "content": "Hello there!"},
			"finish_reason": "stop"
		}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
	}`

	result, err := OpenAIChatResponseToAnthropicMessage([]byte(input), "gpt-4")
	if err != nil {
		t.Fatalf("error: %v", err)
	}

	var r map[string]any
	if err := json.Unmarshal(result, &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if r["type"] != "message" {
		t.Errorf("type = %v", r["type"])
	}
	if r["role"] != "assistant" {
		t.Errorf("role = %v", r["role"])
	}
	id, _ := r["id"].(string)
	if !strings.HasPrefix(id, "msg_") {
		t.Errorf("id should start with msg_, got %q", id)
	}
	if r["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v, want end_turn", r["stop_reason"])
	}

	content, _ := r["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(content))
	}
	block, _ := content[0].(map[string]any)
	if block["type"] != "text" {
		t.Errorf("block type = %v", block["type"])
	}
	if block["text"] != "Hello there!" {
		t.Errorf("text = %v", block["text"])
	}

	usage, _ := r["usage"].(map[string]any)
	if jsonNum(usage["input_tokens"]) != 10 {
		t.Errorf("input_tokens = %v", usage["input_tokens"])
	}
	if jsonNum(usage["output_tokens"]) != 5 {
		t.Errorf("output_tokens = %v", usage["output_tokens"])
	}
}

func TestAnthropicMessageToOpenAIChatResponse(t *testing.T) {
	input := `{
		"id": "msg_abc",
		"type": "message",
		"role": "assistant",
		"model": "claude-sonnet-4-6",
		"content": [{"type": "text", "text": "Hi!"}],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 8, "output_tokens": 3}
	}`

	result, err := AnthropicMessageToOpenAIChatResponse([]byte(input))
	if err != nil {
		t.Fatalf("error: %v", err)
	}

	var r map[string]any
	if err := json.Unmarshal(result, &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if r["object"] != "chat.completion" {
		t.Errorf("object = %v", r["object"])
	}
	id, _ := r["id"].(string)
	if !strings.HasPrefix(id, "chatcmpl-") {
		t.Errorf("id should start with chatcmpl-, got %q", id)
	}

	choices, _ := r["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(choices))
	}
	choice, _ := choices[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v, want stop", choice["finish_reason"])
	}
	msg, _ := choice["message"].(map[string]any)
	if msg["content"] != "Hi!" {
		t.Errorf("content = %v", msg["content"])
	}

	usage, _ := r["usage"].(map[string]any)
	if jsonNum(usage["prompt_tokens"]) != 8 {
		t.Errorf("prompt_tokens = %v", usage["prompt_tokens"])
	}
}

func TestFinishReasonMapping(t *testing.T) {
	tests := []struct {
		oai       string
		anthropic string
	}{
		{"stop", "end_turn"},
		{"length", "max_tokens"},
		{"tool_calls", "tool_use"},
	}
	for _, tc := range tests {
		t.Run(tc.oai+"→"+tc.anthropic, func(t *testing.T) {
			if got := mapOAIFinishReason(tc.oai); got != tc.anthropic {
				t.Errorf("mapOAIFinishReason(%q) = %q, want %q", tc.oai, got, tc.anthropic)
			}
			if got := mapAnthropicStopReason(tc.anthropic); got != tc.oai {
				t.Errorf("mapAnthropicStopReason(%q) = %q, want %q", tc.anthropic, got, tc.oai)
			}
		})
	}
}

func TestPeekAnthropicUsage(t *testing.T) {
	body := `{"usage": {"input_tokens": 42, "output_tokens": 17}}`
	p, o := PeekAnthropicUsage([]byte(body))
	if p != 42 || o != 17 {
		t.Errorf("got (%d, %d), want (42, 17)", p, o)
	}
}

func TestRoundTrip_ResponseOAIToAnthropicAndBack(t *testing.T) {
	oaiResp := `{
		"id": "chatcmpl-test",
		"object": "chat.completion",
		"model": "gpt-4",
		"choices": [{
			"index": 0,
			"message": {"role": "assistant", "content": "Round trip test"},
			"finish_reason": "stop"
		}],
		"usage": {"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8}
	}`

	anthropic, err := OpenAIChatResponseToAnthropicMessage([]byte(oaiResp), "gpt-4")
	if err != nil {
		t.Fatalf("OAI→Anthropic: %v", err)
	}

	backToOAI, err := AnthropicMessageToOpenAIChatResponse(anthropic)
	if err != nil {
		t.Fatalf("Anthropic→OAI: %v", err)
	}

	var r map[string]any
	if err := json.Unmarshal(backToOAI, &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	choices, _ := r["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(choices))
	}
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "Round trip test" {
		t.Errorf("content lost in roundtrip: %v", msg["content"])
	}
}
