package translate

import (
	"encoding/json"
	"testing"
)

func TestOpenAIChatToAnthropicMessages(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		check   func(t *testing.T, result map[string]any)
		wantErr bool
	}{
		{
			name: "basic_chat",
			input: `{
				"model": "claude-sonnet-4-6",
				"messages": [
					{"role": "user", "content": "Hello"}
				],
				"max_tokens": 1024,
				"temperature": 0.7,
				"stream": false
			}`,
			check: func(t *testing.T, r map[string]any) {
				t.Helper()
				if r["model"] != "claude-sonnet-4-6" {
					t.Errorf("model = %v", r["model"])
				}
				if jsonNum(r["max_tokens"]) != 1024 {
					t.Errorf("max_tokens = %v", r["max_tokens"])
				}
				if r["system"] != nil {
					t.Errorf("system should be nil, got %v", r["system"])
				}
				msgs, _ := r["messages"].([]any)
				if len(msgs) != 1 {
					t.Fatalf("expected 1 message, got %d", len(msgs))
				}
			},
		},
		{
			name: "system_extraction",
			input: `{
				"model": "test",
				"messages": [
					{"role": "system", "content": "You are helpful"},
					{"role": "user", "content": "Hi"}
				],
				"max_tokens": 100
			}`,
			check: func(t *testing.T, r map[string]any) {
				t.Helper()
				sys, _ := r["system"].(string)
				if sys != "You are helpful" {
					t.Errorf("system = %q", sys)
				}
				msgs, _ := r["messages"].([]any)
				if len(msgs) != 1 {
					t.Fatalf("expected 1 message (system extracted), got %d", len(msgs))
				}
			},
		},
		{
			name: "default_max_tokens",
			input: `{
				"model": "test",
				"messages": [{"role": "user", "content": "Hi"}]
			}`,
			check: func(t *testing.T, r map[string]any) {
				t.Helper()
				if jsonNum(r["max_tokens"]) != defaultMaxTokens {
					t.Errorf("max_tokens = %v, want %d", r["max_tokens"], defaultMaxTokens)
				}
			},
		},
		{
			name: "stop_to_stop_sequences",
			input: `{
				"model": "test",
				"messages": [{"role": "user", "content": "Hi"}],
				"max_tokens": 100,
				"stop": ["END", "STOP"]
			}`,
			check: func(t *testing.T, r map[string]any) {
				t.Helper()
				ss, ok := r["stop_sequences"].([]any)
				if !ok || len(ss) != 2 {
					t.Errorf("stop_sequences = %v", r["stop_sequences"])
				}
			},
		},
		{
			name: "tools_conversion",
			input: `{
				"model": "test",
				"messages": [{"role": "user", "content": "weather?"}],
				"max_tokens": 100,
				"tools": [{
					"type": "function",
					"function": {
						"name": "get_weather",
						"description": "Get weather",
						"parameters": {"type": "object", "properties": {"city": {"type": "string"}}}
					}
				}]
			}`,
			check: func(t *testing.T, r map[string]any) {
				t.Helper()
				tools, _ := r["tools"].([]any)
				if len(tools) != 1 {
					t.Fatalf("expected 1 tool, got %d", len(tools))
				}
				tool, _ := tools[0].(map[string]any)
				if tool["name"] != "get_weather" {
					t.Errorf("tool name = %v", tool["name"])
				}
				if tool["input_schema"] == nil {
					t.Error("input_schema should not be nil")
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := OpenAIChatToAnthropicMessages([]byte(tc.input))
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			var r map[string]any
			if err := json.Unmarshal(result, &r); err != nil {
				t.Fatalf("unmarshal result: %v", err)
			}
			tc.check(t, r)
		})
	}
}

func TestAnthropicMessagesToOpenAIChat(t *testing.T) {
	tests := []struct {
		name  string
		input string
		check func(t *testing.T, result map[string]any)
	}{
		{
			name: "basic_with_system",
			input: `{
				"model": "claude-sonnet-4-6",
				"system": "You are helpful",
				"messages": [{"role": "user", "content": "Hello"}],
				"max_tokens": 1024
			}`,
			check: func(t *testing.T, r map[string]any) {
				t.Helper()
				msgs, _ := r["messages"].([]any)
				if len(msgs) != 2 {
					t.Fatalf("expected 2 messages (system + user), got %d", len(msgs))
				}
				sys, _ := msgs[0].(map[string]any)
				if sys["role"] != "system" {
					t.Errorf("first message role = %v", sys["role"])
				}
				if sys["content"] != "You are helpful" {
					t.Errorf("system content = %v", sys["content"])
				}
			},
		},
		{
			name: "stop_sequences_to_stop",
			input: `{
				"model": "test",
				"messages": [{"role": "user", "content": "Hi"}],
				"max_tokens": 100,
				"stop_sequences": ["END"]
			}`,
			check: func(t *testing.T, r map[string]any) {
				t.Helper()
				stop, _ := r["stop"].([]any)
				if len(stop) != 1 || stop[0] != "END" {
					t.Errorf("stop = %v", r["stop"])
				}
			},
		},
		{
			name: "content_blocks_flattened",
			input: `{
				"model": "test",
				"messages": [{"role": "user", "content": [{"type":"text","text":"Hello world"}]}],
				"max_tokens": 100
			}`,
			check: func(t *testing.T, r map[string]any) {
				t.Helper()
				msgs, _ := r["messages"].([]any)
				if len(msgs) != 1 {
					t.Fatalf("expected 1 message, got %d", len(msgs))
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := AnthropicMessagesToOpenAIChat([]byte(tc.input))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			var r map[string]any
			if err := json.Unmarshal(result, &r); err != nil {
				t.Fatalf("unmarshal result: %v", err)
			}
			tc.check(t, r)
		})
	}
}

func TestRoundTrip_OpenAIToAnthropicAndBack(t *testing.T) {
	original := `{
		"model": "claude-sonnet-4-6",
		"messages": [
			{"role": "system", "content": "Be concise"},
			{"role": "user", "content": "Hello"}
		],
		"max_tokens": 512,
		"temperature": 0.5,
		"stream": true
	}`

	anthropic, err := OpenAIChatToAnthropicMessages([]byte(original))
	if err != nil {
		t.Fatalf("OAI→Anthropic: %v", err)
	}

	var antMap map[string]any
	if err := json.Unmarshal(anthropic, &antMap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if antMap["system"] != "Be concise" {
		t.Errorf("system should be preserved: %v", antMap["system"])
	}

	backToOAI, err := AnthropicMessagesToOpenAIChat(anthropic)
	if err != nil {
		t.Fatalf("Anthropic→OAI: %v", err)
	}

	var oaiMap map[string]any
	if err := json.Unmarshal(backToOAI, &oaiMap); err != nil {
		t.Fatalf("unmarshal back: %v", err)
	}

	if oaiMap["model"] != "claude-sonnet-4-6" {
		t.Errorf("model lost: %v", oaiMap["model"])
	}
	msgs, _ := oaiMap["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages after roundtrip, got %d", len(msgs))
	}
}
