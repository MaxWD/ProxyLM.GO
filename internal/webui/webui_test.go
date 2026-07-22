package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandler проверяет монтирование так же, как в internal/api/server.go:
// http.StripPrefix("/ui/", Handler()), поверх запросов вида "/ui/...".
func TestHandler(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/ui/", http.StripPrefix("/ui/", Handler()))
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
			name:           "index at /ui/",
			path:           "/ui/",
			wantStatus:     http.StatusOK,
			wantContains:   "<title>ProxyLM</title>",
			wantContentSub: "text/html",
		},
		{
			name:           "app.js",
			path:           "/ui/app.js",
			wantStatus:     http.StatusOK,
			wantContentSub: "javascript",
		},
		{
			name:       "missing asset",
			path:       "/ui/nope.js",
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
					t.Errorf("GET %s: body does not contain %q, got %q", tt.path, tt.wantContains, body)
				}
			}
		})
	}
}
