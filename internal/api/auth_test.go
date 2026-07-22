package api

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"proxylm/internal/config"
)

const testAdminKey = "sk-admin-secret"

func newTestAuthRegistry() *AuthRegistry {
	return NewAuthRegistry(config.Auth{AdminKey: testAdminKey})
}

// TestExtractAdminKey покрывает оба канала передачи admin_key: обычный
// Authorization: Bearer (TUI/curl) и Sec-WebSocket-Protocol (браузерный Web
// UI, у которого нет доступа к произвольным заголовкам при хендшейке WS).
func TestExtractAdminKey(t *testing.T) {
	encodedKey := base64.RawURLEncoding.EncodeToString([]byte(testAdminKey))
	wrongEncodedKey := base64.RawURLEncoding.EncodeToString([]byte("wrong-key"))

	tests := []struct {
		name  string
		setup func(r *http.Request)
		want  string
	}{
		{
			name: "bearer only",
			setup: func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer "+testAdminKey)
			},
			want: testAdminKey,
		},
		{
			name: "subprotocol only",
			setup: func(r *http.Request) {
				r.Header.Set("Sec-WebSocket-Protocol", "proxylm-admin, proxylm-token."+encodedKey)
			},
			want: testAdminKey,
		},
		{
			name: "subprotocol split across multiple headers",
			setup: func(r *http.Request) {
				r.Header.Add("Sec-WebSocket-Protocol", "proxylm-admin")
				r.Header.Add("Sec-WebSocket-Protocol", "proxylm-token."+encodedKey)
			},
			want: testAdminKey,
		},
		{
			name: "both present: bearer wins",
			setup: func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer "+testAdminKey)
				r.Header.Set("Sec-WebSocket-Protocol", "proxylm-admin, proxylm-token."+wrongEncodedKey)
			},
			want: testAdminKey,
		},
		{
			name: "invalid base64 in subprotocol",
			setup: func(r *http.Request) {
				r.Header.Set("Sec-WebSocket-Protocol", "proxylm-admin, proxylm-token.not!base64")
			},
			want: "",
		},
		{
			name:  "nothing present",
			setup: func(r *http.Request) {},
			want:  "",
		},
		{
			name: "subprotocol with wrong key decodes fine but is a different value",
			setup: func(r *http.Request) {
				r.Header.Set("Sec-WebSocket-Protocol", "proxylm-admin, proxylm-token."+wrongEncodedKey)
			},
			want: "wrong-key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/admin/stream", nil)
			tt.setup(req)
			if got := extractAdminKey(req); got != tt.want {
				t.Fatalf("extractAdminKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestAdminAuth проверяет middleware end-to-end: оба канала аутентификации
// должны пропускать запрос при корректном admin_key и отдавать 401 иначе.
func TestAdminAuth(t *testing.T) {
	encodedKey := base64.RawURLEncoding.EncodeToString([]byte(testAdminKey))
	wrongEncodedKey := base64.RawURLEncoding.EncodeToString([]byte("wrong-key"))

	tests := []struct {
		name       string
		setup      func(r *http.Request)
		wantStatus int
	}{
		{
			name: "bearer ok",
			setup: func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer "+testAdminKey)
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "subprotocol ok",
			setup: func(r *http.Request) {
				r.Header.Set("Sec-WebSocket-Protocol", "proxylm-admin, proxylm-token."+encodedKey)
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "wrong key in subprotocol -> 401",
			setup: func(r *http.Request) {
				r.Header.Set("Sec-WebSocket-Protocol", "proxylm-admin, proxylm-token."+wrongEncodedKey)
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "invalid base64 -> 401",
			setup: func(r *http.Request) {
				r.Header.Set("Sec-WebSocket-Protocol", "proxylm-admin, proxylm-token.not!base64")
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "missing -> 401",
			setup:      func(r *http.Request) {},
			wantStatus: http.StatusUnauthorized,
		},
	}

	reg := newTestAuthRegistry()
	okHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mw := reg.AdminAuth(okHandler)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/admin/stream", nil)
			tt.setup(req)
			rec := httptest.NewRecorder()
			mw.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("AdminAuth status = %d, want %d", rec.Code, tt.wantStatus)
			}
		})
	}
}
