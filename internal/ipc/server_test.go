package ipc

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"proxylm/internal/core"
	"proxylm/internal/storage"
)

func newTestHub(t *testing.T, withHistory bool) (*Hub, *storage.History, string) {
	t.Helper()
	ctx := context.Background()
	var hist *storage.History
	if withHistory {
		db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		hist = storage.NewHistory(db)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := NewHub("test", nil, hist, time.Hour, 3, 0, nil, core.NewLiveTail(), log)
	hub.SetRetentionDays(30)
	ts := httptest.NewServer(hub.Handler())
	t.Cleanup(ts.Close)
	return hub, hist, "ws" + strings.TrimPrefix(ts.URL, "http")
}

func dialTest(t *testing.T, url string) *Client {
	t.Helper()
	c, err := Dial(context.Background(), url, "tok")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// drainInitial читает hello и первый snapshot.
func drainInitial(t *testing.T, ctx context.Context, c *Client) {
	t.Helper()
	for _, want := range []MessageType{TypeHello, TypeStateSnapshot} {
		env, _, err := c.Recv(ctx)
		if err != nil || env.Type != want {
			t.Fatalf("initial frame: type=%q err=%v want %q", env.Type, err, want)
		}
	}
}

func recvUsage(t *testing.T, ctx context.Context, c *Client) UsageStatsPayload {
	t.Helper()
	env, raw, err := c.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if env.Type != TypeUsageStats {
		t.Fatalf("type=%q want usage_stats", env.Type)
	}
	var p UsageStatsPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	return p
}

func TestHub_StatsRequest(t *testing.T) {
	tests := []struct {
		name       string
		window     string
		wantWindow string
		wantRows   int
	}{
		{"1h excludes older", "1h", "1h", 1},
		{"24h", "24h", "24h", 2},
		{"7d", "7d", "7d", 3},
		{"all", "all", "all", 4},
		{"invalid falls back to 24h", "bogus", "24h", 2},
		{"empty falls back to 24h", "", "24h", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, hist, url := newTestHub(t, true)
			now := time.Now().UTC()
			// distinct models => distinct rows
			for i, age := range []time.Duration{10 * time.Minute, 5 * time.Hour, 3 * 24 * time.Hour, 30 * 24 * time.Hour} {
				err := hist.Insert(ctx, &core.RequestRecord{
					ID: string(rune('a' + i)), ClientName: "svc", Model: "m" + string(rune('0'+i)),
					Endpoint: "/v1/x", Status: core.StatusCompleted, CreatedAt: now.Add(-age),
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			c1, c2 := dialTest(t, url), dialTest(t, url)
			drainInitial(t, ctx, c1)
			drainInitial(t, ctx, c2)

			if err := c1.RequestStats(ctx, tt.window); err != nil {
				t.Fatal(err)
			}
			p := recvUsage(t, ctx, c1)
			if p.Window != tt.wantWindow || len(p.Rows) != tt.wantRows || p.RetentionDays != 30 || p.Error != "" || p.GeneratedAt.IsZero() {
				t.Errorf("payload=%+v", p)
			}

			// c2 не должен получить usage_stats: следующий его фрейм — ответ на
			// собственный request_snapshot.
			if err := c2.RequestSnapshot(ctx); err != nil {
				t.Fatal(err)
			}
			env, _, err := c2.Recv(ctx)
			if err != nil || env.Type != TypeStateSnapshot {
				t.Fatalf("c2 got type=%q err=%v, want state_snapshot only", env.Type, err)
			}
		})
	}
}

func TestHub_StatsRequest_EmptyRowsNotNull(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, url := newTestHub(t, true)
	c := dialTest(t, url)
	drainInitial(t, ctx, c)
	if err := c.RequestStats(ctx, "all"); err != nil {
		t.Fatal(err)
	}
	env, raw, err := c.Recv(ctx)
	if err != nil || env.Type != TypeUsageStats {
		t.Fatalf("type=%q err=%v", env.Type, err)
	}
	if !strings.Contains(string(raw), `"rows":[]`) {
		t.Errorf("rows must be [] not null: %s", raw)
	}
}

func TestStatsWindow(t *testing.T) {
	tests := []struct {
		name    string
		payload any
		want    string
	}{
		{"nil", nil, "24h"},
		{"wrong type", "x", "24h"},
		{"non-string window", map[string]any{"window": 5}, "24h"},
		{"7d", map[string]any{"window": "7d"}, "7d"},
		{"unknown", map[string]any{"window": "1y"}, "24h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := statsWindow(tt.payload); got != tt.want {
				t.Errorf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestHub_BuildUsageStats_NilHistory(t *testing.T) {
	h := NewHub("t", nil, nil, time.Hour, 1, 0, nil, nil, nil)
	env := h.buildUsageStats(context.Background(), "24h")
	p, ok := env.Payload.(UsageStatsPayload)
	if !ok || env.Type != TypeUsageStats {
		t.Fatalf("env=%+v", env)
	}
	if p.Error == "" || p.Rows == nil || len(p.Rows) != 0 {
		t.Errorf("payload=%+v", p)
	}
}
