package api

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"

	"proxylm/internal/config"
	"proxylm/internal/core"
	"proxylm/internal/ipc"
	"proxylm/internal/storage"
)

// newTestAdminServer поднимает Server с реальным ipc.Hub на /admin/stream —
// так же, как cmd/serve.go (hub.Handler() через SetAdminStream) — поверх
// httptest.Server, чтобы проверить оба канала admin-аутентификации сквозь
// настоящий WebSocket-хендшейк (не через мок).
func newTestAdminServer(t *testing.T) (ts *httptest.Server, adminKey string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	dbPath := filepath.Join(t.TempDir(), "admin_ws_test.db")
	db, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
	history := storage.NewHistory(db)

	adminKey = "admin-secret"
	auth := NewAuthRegistry(config.Auth{AdminKey: adminKey})

	var servers []*core.ServerInfo
	hub := ipc.NewHub("test", servers, history, time.Hour, 1, 0, nil, nil, nil)

	srv := New(config.Proxy{Host: "127.0.0.1", Port: 0, LogLevel: "error"}, Deps{Auth: auth})
	srv.SetAdminStream(hub.Handler())

	ts = httptest.NewServer(srv.Router())
	t.Cleanup(ts.Close)
	return ts, adminKey
}

// wsURL преобразует http(s):// базовый URL httptest.Server в ws(s):// + путь.
func wsURL(base, path string) string {
	return "ws" + base[len("http"):] + path
}

// TestAdminStreamWebSocketAuth — e2e регрессия WS-аутентификации /admin/stream:
// существующий TUI-путь (Bearer-заголовок) должен продолжать работать, а
// новый браузерный путь (admin_key в Sec-WebSocket-Protocol) — заработать,
// не подставляя произвольных заголовков, недоступных browser WebSocket API.
func TestAdminStreamWebSocketAuth(t *testing.T) {
	ts, adminKey := newTestAdminServer(t)
	u := wsURL(ts.URL, "/admin/stream")

	t.Run("bearer header (TUI path) connects", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{
			HTTPHeader: http.Header{"Authorization": []string{"Bearer " + adminKey}},
		})
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
	})

	t.Run("subprotocol token (browser path) connects without auth header", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		encoded := base64.RawURLEncoding.EncodeToString([]byte(adminKey))
		conn, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{
			Subprotocols: []string{"proxylm-admin", "proxylm-token." + encoded},
		})
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close(websocket.StatusNormalClosure, "")

		if got := conn.Subprotocol(); got != "proxylm-admin" {
			t.Errorf("negotiated subprotocol = %q, want %q", got, "proxylm-admin")
		}
	})

	t.Run("wrong token in subprotocol fails to dial", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		encoded := base64.RawURLEncoding.EncodeToString([]byte("not-the-admin-key"))
		conn, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{
			Subprotocols: []string{"proxylm-admin", "proxylm-token." + encoded},
		})
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if err == nil {
			conn.Close(websocket.StatusNormalClosure, "")
			t.Fatal("dial: want error for wrong admin key in subprotocol, got nil")
		}
	})
}
