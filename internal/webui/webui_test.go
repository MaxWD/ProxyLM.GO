package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandler проверяет монтирование так же, как в cmd/web.go:
// Handler() в корне локального HTTP-сервера команды `proxylm web`.
func TestHandler(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/", Handler())
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	tests := []struct {
		name           string
		path           string
		wantStatus     int
		wantContains   string
		wantContentSub string
	}{
		{
			name:           "index at /",
			path:           "/",
			wantStatus:     http.StatusOK,
			wantContains:   "<title>ProxyLM</title>",
			wantContentSub: "text/html",
		},
		{
			name:           "app.js",
			path:           "/app.js",
			wantStatus:     http.StatusOK,
			wantContentSub: "javascript",
		},
		{
			name:       "missing asset",
			path:       "/nope.js",
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := http.Get(ts.URL + tt.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tt.path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("GET %s: status = %d, want %d", tt.path, resp.StatusCode, tt.wantStatus)
			}

			if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
				t.Errorf("GET %s: Cache-Control = %q, want %q", tt.path, cc, "no-cache")
			}

			if tt.wantContentSub != "" {
				if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, tt.wantContentSub) {
					t.Errorf("GET %s: Content-Type = %q, want substring %q", tt.path, ct, tt.wantContentSub)
				}
			}

			if tt.wantContains != "" {
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatalf("GET %s: read body: %v", tt.path, err)
				}
				if !strings.Contains(string(body), tt.wantContains) {
					t.Errorf("GET %s: body does not contain %q", tt.path, tt.wantContains)
				}
			}
		})
	}
}

// TestConfigJS проверяет генерацию config.js: корректный JS-префикс и
// json-экранирование произвольных символов в токене (не должны ломать скрипт).
func TestConfigJS(t *testing.T) {
	tests := []struct {
		name  string
		ws    string
		token string
		want  []string
		not   []string
	}{
		{
			name:  "ws and token",
			ws:    "ws://localhost:8080/admin/stream",
			token: "sk-admin-x",
			want:  []string{`window.PROXYLM = {`, `"ws":"ws://localhost:8080/admin/stream"`, `"token":"sk-admin-x"`},
		},
		{
			name: "empty token omitted",
			ws:   "ws://h:1/admin/stream",
			want: []string{`"ws":"ws://h:1/admin/stream"`},
			not:  []string{`"token"`},
		},
		{
			name:  "token with quotes and script tag is escaped",
			ws:    "ws://h:1/admin/stream",
			token: `a"b</script><script>evil()`,
			want:  []string{`\"b`},
			not:   []string{`</script><script>`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(ConfigJS(tt.ws, tt.token))
			if !strings.HasPrefix(got, "window.PROXYLM = {") || !strings.HasSuffix(got, ";\n") {
				t.Fatalf("ConfigJS: unexpected wrapper: %q", got)
			}
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("ConfigJS: missing %q in %q", w, got)
				}
			}
			for _, n := range tt.not {
				if strings.Contains(got, n) {
					t.Errorf("ConfigJS: must not contain %q, got %q", n, got)
				}
			}
		})
	}
}
