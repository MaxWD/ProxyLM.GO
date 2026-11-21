package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"proxylm/internal/config"
)

// TestAuthRegistry_Update: hot-замена клиентских ключей — новый ключ принимается
// сразу, удалённый получает 401, admin_key остаётся прежним до рестарта.
func TestAuthRegistry_Update(t *testing.T) {
	reg := NewAuthRegistry(config.Auth{
		APIKeys:  []config.APIKey{{Name: "old", Key: "k-old"}},
		AdminKey: "adm",
	})
	h := reg.ClientAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Client", ClientFromContext(r.Context()))
	}))
	call := func(key string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Header().Get("X-Client")
	}

	if code, name := call("k-old"); code != 200 || name != "old" {
		t.Fatalf("before update: %d %q", code, name)
	}
	if code, _ := call("k-new"); code != http.StatusUnauthorized {
		t.Fatalf("k-new before update: %d", code)
	}

	reg.Update(config.Auth{
		APIKeys:  []config.APIKey{{Name: "new", Key: "k-new"}},
		AdminKey: "changed-admin",
	})

	tests := []struct {
		name     string
		key      string
		wantCode int
		wantName string
	}{
		{"new key accepted", "k-new", 200, "new"},
		{"removed key rejected", "k-old", http.StatusUnauthorized, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, name := call(tt.key)
			if code != tt.wantCode || name != tt.wantName {
				t.Errorf("got %d %q, want %d %q", code, name, tt.wantCode, tt.wantName)
			}
		})
	}
	if !reg.IsAdmin("adm") || reg.IsAdmin("changed-admin") {
		t.Error("admin key must not change on Update (restart-only)")
	}
}

func TestServer_SetCompat(t *testing.T) {
	s := New(config.Proxy{}, Deps{Compat: config.Compat{ResponseFormatMode: config.ResponseFormatPassthrough}})
	if got := s.compat.Load().ResponseFormatMode; got != config.ResponseFormatPassthrough {
		t.Fatalf("initial = %q", got)
	}
	s.SetCompat(config.Compat{ResponseFormatMode: config.ResponseFormatStrictReject})
	if got := s.compat.Load().ResponseFormatMode; got != config.ResponseFormatStrictReject {
		t.Errorf("after SetCompat = %q", got)
	}
}
