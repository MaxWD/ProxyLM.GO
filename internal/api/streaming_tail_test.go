package api

import "testing"

func TestExtractDeltaContent(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{
			name: "chat delta content",
			data: `{"choices":[{"delta":{"content":"Hello"}}]}`,
			want: "Hello",
		},
		{
			name: "legacy completions text",
			data: `{"choices":[{"text":" world"}]}`,
			want: " world",
		},
		{
			name: "role-only delta → empty",
			data: `{"choices":[{"delta":{"role":"assistant"}}]}`,
			want: "",
		},
		{
			name: "no choices → empty",
			data: `{"usage":{"prompt_tokens":5}}`,
			want: "",
		},
		{
			name: "invalid json → empty",
			data: `not json`,
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractDeltaContent([]byte(tt.data)); got != tt.want {
				t.Fatalf("extractDeltaContent = %q, want %q", got, tt.want)
			}
		})
	}
}
