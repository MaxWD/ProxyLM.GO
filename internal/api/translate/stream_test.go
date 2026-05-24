package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOpenAIToAnthropicStream(t *testing.T) {
	translator := NewOpenAIToAnthropicTranslator("claude-test")

	lines := []string{
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"claude-test","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"claude-test","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"claude-test","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"claude-test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`,
		`data: [DONE]`,
	}

	var allOut []string
	for _, line := range lines {
		out, err := translator.ProcessLine([]byte(line))
		if err != nil {
			t.Fatalf("ProcessLine(%q): %v", line, err)
		}
		for _, o := range out {
			allOut = append(allOut, string(o))
		}
	}

	finish, _ := translator.Finish()
	for _, o := range finish {
		allOut = append(allOut, string(o))
	}

	if len(allOut) == 0 {
		t.Fatal("no output lines produced")
	}

	foundMessageStart := false
	foundBlockDelta := false
	foundMessageStop := false
	for _, line := range allOut {
		if strings.Contains(line, "message_start") {
			foundMessageStart = true
		}
		if strings.Contains(line, "content_block_delta") && strings.Contains(line, "Hello") {
			foundBlockDelta = true
		}
		if strings.Contains(line, "message_stop") {
			foundMessageStop = true
		}
	}
	if !foundMessageStart {
		t.Error("missing message_start event")
	}
	if !foundBlockDelta {
		t.Error("missing content_block_delta with 'Hello'")
	}
	if !foundMessageStop {
		t.Error("missing message_stop event")
	}

	p, o := translator.Usage()
	if p != 5 || o != 2 {
		t.Errorf("usage = (%d, %d), want (5, 2)", p, o)
	}
	if !translator.Done() {
		t.Error("translator should be done after [DONE]")
	}
}

func TestAnthropicToOpenAIStream(t *testing.T) {
	translator := NewAnthropicToOpenAITranslator()

	lines := []string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" there"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
	}

	var allOut []string
	for _, line := range lines {
		out, err := translator.ProcessLine([]byte(line))
		if err != nil {
			t.Fatalf("ProcessLine(%q): %v", line, err)
		}
		for _, o := range out {
			allOut = append(allOut, string(o))
		}
	}

	finish, _ := translator.Finish()
	for _, o := range finish {
		allOut = append(allOut, string(o))
	}

	if len(allOut) == 0 {
		t.Fatal("no output lines produced")
	}

	foundRole := false
	foundContent := false
	foundDone := false
	for _, line := range allOut {
		if strings.Contains(line, `"role":"assistant"`) {
			foundRole = true
		}
		if strings.Contains(line, `"content":"Hi"`) {
			foundContent = true
		}
		if line == "data: [DONE]" {
			foundDone = true
		}
	}
	if !foundRole {
		t.Error("missing role: assistant chunk")
	}
	if !foundContent {
		t.Error("missing content delta chunk")
	}
	if !foundDone {
		t.Error("missing [DONE] marker")
	}

	p, o := translator.Usage()
	if p != 10 {
		t.Errorf("prompt tokens = %d, want 10", p)
	}
	if o != 3 {
		t.Errorf("output tokens = %d, want 3", o)
	}
	if !translator.Done() {
		t.Error("translator should be done after message_stop")
	}
}

func TestStreamTranslator_EmptyInput(t *testing.T) {
	t.Run("oai_to_anthropic", func(t *testing.T) {
		tr := NewOpenAIToAnthropicTranslator("test")
		out, err := tr.ProcessLine([]byte(""))
		if err != nil {
			t.Fatalf("error: %v", err)
		}
		if len(out) != 0 {
			t.Errorf("expected no output for empty line, got %d", len(out))
		}
	})
	t.Run("anthropic_to_oai", func(t *testing.T) {
		tr := NewAnthropicToOpenAITranslator()
		out, err := tr.ProcessLine([]byte(""))
		if err != nil {
			t.Fatalf("error: %v", err)
		}
		if len(out) != 0 {
			t.Errorf("expected no output for empty line, got %d", len(out))
		}
	})
}

func TestOAIChunkHasModel(t *testing.T) {
	translator := NewAnthropicToOpenAITranslator()
	_, _ = translator.ProcessLine([]byte(`event: message_start`))
	out, _ := translator.ProcessLine([]byte(`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus","content":[],"usage":{"input_tokens":1}}}`))
	if len(out) == 0 {
		t.Fatal("expected output")
	}
	var chunk map[string]any
	data := strings.TrimPrefix(string(out[0]), "data: ")
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if chunk["model"] != "claude-opus" {
		t.Errorf("model = %v, want claude-opus", chunk["model"])
	}
}
