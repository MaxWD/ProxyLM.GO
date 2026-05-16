package api

import (
	"encoding/json"
	"strings"
	"testing"

	"proxylm/internal/config"
)

// TestApplyResponseFormatCompat покрывает все три режима compat.response_format_mode.
// Особое внимание — normalize_json_object: именно ради него существует фича
// (LM Studio не понимает старый json_object и отвечает HTTP 400).
func TestApplyResponseFormatCompat(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		mode     string
		body     string
		wantErr  string
		assert   func(t *testing.T, out []byte)
	}{
		{
			name:     "passthrough оставляет тело как есть",
			endpoint: "/v1/chat/completions",
			mode:     config.ResponseFormatPassthrough,
			body:     `{"model":"m","response_format":{"type":"json_object"}}`,
			assert: func(t *testing.T, out []byte) {
				if !strings.Contains(string(out), `"json_object"`) {
					t.Errorf("ожидали json_object нетронутым, got %s", out)
				}
			},
		},
		{
			name:     "пустой mode = passthrough",
			endpoint: "/v1/chat/completions",
			mode:     "",
			body:     `{"model":"m","response_format":{"type":"json_object"}}`,
			assert: func(t *testing.T, out []byte) {
				if !strings.Contains(string(out), `"json_object"`) {
					t.Errorf("ожидали json_object нетронутым, got %s", out)
				}
			},
		},
		{
			name:     "не chat/completions endpoint не трогается",
			endpoint: "/v1/embeddings",
			mode:     config.ResponseFormatNormalizeJSONObject,
			body:     `{"model":"m","response_format":{"type":"json_object"}}`,
			assert: func(t *testing.T, out []byte) {
				if !strings.Contains(string(out), `"json_object"`) {
					t.Errorf("для embeddings трансформации быть не должно, got %s", out)
				}
			},
		},
		{
			name:     "normalize: json_object → json_schema",
			endpoint: "/v1/chat/completions",
			mode:     config.ResponseFormatNormalizeJSONObject,
			body:     `{"model":"m","response_format":{"type":"json_object"}}`,
			assert: func(t *testing.T, out []byte) {
				var got map[string]any
				if err := json.Unmarshal(out, &got); err != nil {
					t.Fatalf("invalid JSON output: %v", err)
				}
				rf, ok := got["response_format"].(map[string]any)
				if !ok {
					t.Fatalf("response_format должен быть объектом")
				}
				if rf["type"] != "json_schema" {
					t.Errorf("type должен стать json_schema, got %v", rf["type"])
				}
				js, ok := rf["json_schema"].(map[string]any)
				if !ok {
					t.Fatalf("json_schema должен быть объектом")
				}
				if js["name"] != "response" {
					t.Errorf("json_schema.name = %v, want 'response'", js["name"])
				}
				if js["strict"] != false {
					t.Errorf("json_schema.strict должен быть false, got %v", js["strict"])
				}
			},
		},
		{
			name:     "normalize: json_schema нетронут",
			endpoint: "/v1/chat/completions",
			mode:     config.ResponseFormatNormalizeJSONObject,
			body:     `{"model":"m","response_format":{"type":"json_schema","json_schema":{"name":"x"}}}`,
			assert: func(t *testing.T, out []byte) {
				if !strings.Contains(string(out), `"x"`) {
					t.Errorf("существующий json_schema должен сохраниться, got %s", out)
				}
			},
		},
		{
			name:     "normalize: text нетронут",
			endpoint: "/v1/chat/completions",
			mode:     config.ResponseFormatNormalizeJSONObject,
			body:     `{"model":"m","response_format":{"type":"text"}}`,
			assert: func(t *testing.T, out []byte) {
				if !strings.Contains(string(out), `"text"`) {
					t.Errorf("text должен сохраниться, got %s", out)
				}
			},
		},
		{
			name:     "normalize: тело без response_format остаётся как есть",
			endpoint: "/v1/chat/completions",
			mode:     config.ResponseFormatNormalizeJSONObject,
			body:     `{"model":"m","messages":[]}`,
			assert: func(t *testing.T, out []byte) {
				if strings.Contains(string(out), "response_format") {
					t.Errorf("не должны добавлять response_format если его не было, got %s", out)
				}
			},
		},
		{
			name:     "strict_reject: json_object → ошибка",
			endpoint: "/v1/chat/completions",
			mode:     config.ResponseFormatStrictReject,
			body:     `{"model":"m","response_format":{"type":"json_object"}}`,
			wantErr:  "json_schema",
		},
		{
			name:     "strict_reject: json_schema пропускается",
			endpoint: "/v1/chat/completions",
			mode:     config.ResponseFormatStrictReject,
			body:     `{"model":"m","response_format":{"type":"json_schema"}}`,
			assert: func(t *testing.T, out []byte) {
				if !strings.Contains(string(out), "json_schema") {
					t.Errorf("ожидали json_schema, got %s", out)
				}
			},
		},
		{
			name:     "strict_reject: text пропускается",
			endpoint: "/v1/chat/completions",
			mode:     config.ResponseFormatStrictReject,
			body:     `{"model":"m","response_format":{"type":"text"}}`,
			assert: func(t *testing.T, out []byte) {
				if !strings.Contains(string(out), `"text"`) {
					t.Errorf("ожидали text, got %s", out)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := applyResponseFormatCompat([]byte(tc.body), tc.endpoint, tc.mode, nil)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("ожидали ошибку, содержащую %q, получили nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("ошибка %q не содержит %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("неожиданная ошибка: %v", err)
			}
			if tc.assert != nil {
				tc.assert(t, out)
			}
		})
	}
}
