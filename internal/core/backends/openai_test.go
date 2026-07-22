package backends

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestOpenAI_ListModels_Shapes покрывает parseModelsBody через публичный
// контракт OpenAI.ListModels: валидные формы (OpenAI object, legacy array),
// невалидные формы (ErrModelsShape) и non-2xx (ошибка НЕ должна оборачивать
// ErrModelsShape — это отдельная категория, обрабатываемая по-другому в discovery.go).
func TestOpenAI_ListModels_Shapes(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		want      []string
		wantErr   bool
		wantShape bool // errors.Is(err, ErrModelsShape) ожидается
	}{
		{
			name:   "openai_object_with_two_models",
			status: http.StatusOK,
			body:   `{"data":[{"id":"a"},{"id":"b"}]}`,
			want:   []string{"a", "b"},
		},
		{
			name:   "openai_object_empty_data",
			status: http.StatusOK,
			body:   `{"data":[]}`,
			want:   []string{},
		},
		{
			name:      "entry_missing_id_field",
			status:    http.StatusOK,
			body:      `{"data":[{"name":"x"}]}`,
			wantErr:   true,
			wantShape: true,
		},
		{
			name:      "entry_empty_id_string",
			status:    http.StatusOK,
			body:      `{"data":[{"id":""}]}`,
			wantErr:   true,
			wantShape: true,
		},
		{
			name:   "legacy_string_array",
			status: http.StatusOK,
			body:   `["a","b"]`,
			want:   []string{"a", "b"},
		},
		{
			name:   "legacy_empty_array",
			status: http.StatusOK,
			body:   `[]`,
			want:   []string{},
		},
		{
			name:      "non_json_html",
			status:    http.StatusOK,
			body:      `<html>oops</html>`,
			wantErr:   true,
			wantShape: true,
		},
		{
			name:      "error_object_without_data_key",
			status:    http.StatusOK,
			body:      `{"error":{"message":"x"}}`,
			wantErr:   true,
			wantShape: true,
		},
		{
			name:      "non_2xx_status",
			status:    http.StatusInternalServerError,
			body:      `{"error":"boom"}`,
			wantErr:   true,
			wantShape: false, // non-2xx errors must NOT wrap ErrModelsShape
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" {
					t.Errorf("unexpected path %q", r.URL.Path)
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			o, err := NewOpenAI("srv", srv.URL, "", 5*time.Second)
			if err != nil {
				t.Fatalf("NewOpenAI: %v", err)
			}
			got, err := o.ListModels(context.Background())
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (models=%v)", got)
				}
				if isShape := errors.Is(err, ErrModelsShape); isShape != tt.wantShape {
					t.Errorf("errors.Is(err, ErrModelsShape) = %v, want %v (err=%v)", isShape, tt.wantShape, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got == nil {
				t.Fatalf("expected non-nil slice, got nil")
			}
			assertEqualStrings(t, got, tt.want)
		})
	}
}

// TestAnthropic_ListModels_Shapes — зеркало для Anthropic.ListModels: тот же
// parseModelsBody, но другой endpoint/auth (x-api-key + anthropic-version
// вместо Authorization: Bearer).
func TestAnthropic_ListModels_Shapes(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		want      []string
		wantErr   bool
		wantShape bool
	}{
		{
			name:   "openai_object_with_models",
			status: http.StatusOK,
			body:   `{"data":[{"id":"claude-a"},{"id":"claude-b"}]}`,
			want:   []string{"claude-a", "claude-b"},
		},
		{
			name:      "entry_missing_id",
			status:    http.StatusOK,
			body:      `{"data":[{"name":"claude-a"}]}`,
			wantErr:   true,
			wantShape: true,
		},
		{
			name:      "non_2xx_status",
			status:    http.StatusUnauthorized,
			body:      `{"error":{"message":"unauthorized"}}`,
			wantErr:   true,
			wantShape: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" {
					t.Errorf("unexpected path %q", r.URL.Path)
				}
				if got := r.Header.Get("x-api-key"); got != "test-key" {
					t.Errorf("x-api-key = %q, want test-key", got)
				}
				if got := r.Header.Get("anthropic-version"); got != anthropicVersion {
					t.Errorf("anthropic-version = %q, want %q", got, anthropicVersion)
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			a, err := NewAnthropic("srv", srv.URL, "test-key", 5*time.Second)
			if err != nil {
				t.Fatalf("NewAnthropic: %v", err)
			}
			got, err := a.ListModels(context.Background())
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (models=%v)", got)
				}
				if isShape := errors.Is(err, ErrModelsShape); isShape != tt.wantShape {
					t.Errorf("errors.Is(err, ErrModelsShape) = %v, want %v (err=%v)", isShape, tt.wantShape, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			assertEqualStrings(t, got, tt.want)
		})
	}
}
