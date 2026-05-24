package translate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// StreamTranslator обрабатывает SSE-строки от upstream и возвращает
// транслированные SSE-строки для клиента.
type StreamTranslator interface {
	// ProcessLine принимает одну строку из upstream SSE и возвращает
	// ноль или более строк для отправки клиенту.
	ProcessLine(line []byte) ([][]byte, error)
	// Finish возвращает финальные события при завершении потока.
	Finish() ([][]byte, error)
	// Usage возвращает накопленный usage.
	Usage() (prompt, output int)
	// Done возвращает true если поток завершён ([DONE] или message_stop).
	Done() bool
}

// --- OpenAI → Anthropic stream translator ---

type oaiToAnthropicTranslator struct {
	model        string
	msgID        string
	started      bool
	blockStarted bool
	blockIndex   int
	promptTok    int
	outputTok    int
	done         bool
}

func NewOpenAIToAnthropicTranslator(model string) StreamTranslator {
	return &oaiToAnthropicTranslator{
		model: model,
		msgID: randomID("msg_", 12),
	}
}

func (t *oaiToAnthropicTranslator) Done() bool { return t.done }
func (t *oaiToAnthropicTranslator) Usage() (int, int) {
	return t.promptTok, t.outputTok
}

func (t *oaiToAnthropicTranslator) ProcessLine(line []byte) ([][]byte, error) {
	data, ok := trimSSEData(line)
	if !ok {
		return nil, nil
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "[DONE]" {
		t.done = true
		return nil, nil
	}

	var chunk map[string]any
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil, nil
	}

	if usage, ok := chunk["usage"].(map[string]any); ok {
		t.promptTok = jsonNum(usage["prompt_tokens"])
		t.outputTok = jsonNum(usage["completion_tokens"])
	}

	var out [][]byte

	if !t.started {
		t.started = true
		model := t.model
		if m, ok := chunk["model"].(string); ok {
			model = m
		}
		msgStart := map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":      t.msgID,
				"type":    "message",
				"role":    "assistant",
				"model":   model,
				"content": []any{},
				"usage":   map[string]any{"input_tokens": t.promptTok, "output_tokens": 0},
			},
		}
		out = append(out, makeAnthropicSSE("message_start", msgStart))
	}

	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return out, nil
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return out, nil
	}

	delta, _ := choice["delta"].(map[string]any)
	if delta == nil {
		return out, nil
	}

	contentStr, hasContent := delta["content"].(string)

	if hasContent && !t.blockStarted {
		t.blockStarted = true
		blockStart := map[string]any{
			"type":          "content_block_start",
			"index":         t.blockIndex,
			"content_block": map[string]any{"type": "text", "text": ""},
		}
		out = append(out, makeAnthropicSSE("content_block_start", blockStart))
	}

	if hasContent && contentStr != "" {
		blockDelta := map[string]any{
			"type":  "content_block_delta",
			"index": t.blockIndex,
			"delta": map[string]any{"type": "text_delta", "text": contentStr},
		}
		out = append(out, makeAnthropicSSE("content_block_delta", blockDelta))
	}

	return out, nil
}

func (t *oaiToAnthropicTranslator) Finish() ([][]byte, error) {
	var out [][]byte
	if t.blockStarted {
		out = append(out, makeAnthropicSSE("content_block_stop",
			map[string]any{"type": "content_block_stop", "index": t.blockIndex}))
	}
	out = append(out, makeAnthropicSSE("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn"},
		"usage": map[string]any{"output_tokens": t.outputTok},
	}))
	out = append(out, makeAnthropicSSE("message_stop",
		map[string]any{"type": "message_stop"}))
	return out, nil
}

// --- Anthropic → OpenAI stream translator ---

type anthropicToOAITranslator struct {
	chatID    string
	model     string
	started   bool
	promptTok int
	outputTok int
	done      bool

	pendingEvent string
}

func NewAnthropicToOpenAITranslator() StreamTranslator {
	return &anthropicToOAITranslator{
		chatID: randomID("chatcmpl-", 12),
	}
}

func (t *anthropicToOAITranslator) Done() bool { return t.done }
func (t *anthropicToOAITranslator) Usage() (int, int) {
	return t.promptTok, t.outputTok
}

func (t *anthropicToOAITranslator) ProcessLine(line []byte) ([][]byte, error) {
	trimmed := bytes.TrimSpace(line)

	// Anthropic SSE: "event: <type>" then "data: <json>"
	if bytes.HasPrefix(trimmed, []byte("event:")) {
		t.pendingEvent = strings.TrimSpace(string(trimmed[6:]))
		return nil, nil
	}

	data, ok := trimSSEData(line)
	if !ok {
		return nil, nil
	}

	eventType := t.pendingEvent
	t.pendingEvent = ""

	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, nil
	}

	switch eventType {
	case "message_start":
		msg, _ := payload["message"].(map[string]any)
		if msg != nil {
			if m, ok := msg["model"].(string); ok {
				t.model = m
			}
			if u, ok := msg["usage"].(map[string]any); ok {
				t.promptTok = jsonNum(u["input_tokens"])
			}
		}
		if !t.started {
			t.started = true
			return [][]byte{t.makeOAIChunk(map[string]any{"role": "assistant"}, nil)}, nil
		}
		return nil, nil

	case "content_block_delta":
		delta, _ := payload["delta"].(map[string]any)
		if delta == nil {
			return nil, nil
		}
		dtype, _ := delta["type"].(string)
		switch dtype {
		case "text_delta":
			text, _ := delta["text"].(string)
			if text != "" {
				return [][]byte{t.makeOAIChunk(map[string]any{"content": text}, nil)}, nil
			}
		case "thinking_delta":
			// Extended thinking — strip при кросс-протоколе
		}
		return nil, nil

	case "message_delta":
		delta, _ := payload["delta"].(map[string]any)
		if u, ok := payload["usage"].(map[string]any); ok {
			t.outputTok = jsonNum(u["output_tokens"])
		}
		stopReason := ""
		if delta != nil {
			stopReason, _ = delta["stop_reason"].(string)
		}
		finishReason := mapAnthropicStopReason(stopReason)
		usage := map[string]any{
			"prompt_tokens":     t.promptTok,
			"completion_tokens": t.outputTok,
			"total_tokens":      t.promptTok + t.outputTok,
		}
		return [][]byte{t.makeOAIChunk(nil, &oaiFinish{reason: finishReason, usage: usage})}, nil

	case "message_stop":
		t.done = true
		return [][]byte{[]byte("data: [DONE]")}, nil

	case "content_block_start", "content_block_stop", "ping":
		return nil, nil
	}

	return nil, nil
}

func (t *anthropicToOAITranslator) Finish() ([][]byte, error) {
	if !t.done {
		return [][]byte{[]byte("data: [DONE]")}, nil
	}
	return nil, nil
}

type oaiFinish struct {
	reason string
	usage  map[string]any
}

func (t *anthropicToOAITranslator) makeOAIChunk(delta map[string]any, finish *oaiFinish) []byte {
	chunk := map[string]any{
		"id":      t.chatID,
		"object":  "chat.completion.chunk",
		"created": 0,
		"model":   t.model,
	}
	choice := map[string]any{
		"index": 0,
	}
	if delta != nil {
		choice["delta"] = delta
	} else {
		choice["delta"] = map[string]any{}
	}
	if finish != nil {
		choice["finish_reason"] = finish.reason
		if finish.usage != nil {
			chunk["usage"] = finish.usage
		}
	} else {
		choice["finish_reason"] = nil
	}
	chunk["choices"] = []any{choice}
	b, _ := json.Marshal(chunk)
	return append([]byte("data: "), b...)
}

// helpers

func trimSSEData(line []byte) ([]byte, bool) {
	const prefix = "data:"
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte(prefix)) {
		return nil, false
	}
	rest := trimmed[len(prefix):]
	if len(rest) > 0 && rest[0] == ' ' {
		rest = rest[1:]
	}
	return rest, true
}

func makeAnthropicSSE(event string, payload any) []byte {
	data, _ := json.Marshal(payload)
	return []byte(fmt.Sprintf("event: %s\ndata: %s", event, data))
}
