package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"proxylm/internal/core"
)

func usageRec(id, client, model, server string, st core.Status, created time.Time) *core.RequestRecord {
	return &core.RequestRecord{
		ID: id, ClientName: client, Model: model, Endpoint: "/v1/chat/completions",
		ServerName: server, Status: st, CreatedAt: created,
	}
}

func TestUsageStats_EmptyDB(t *testing.T) {
	h := NewHistory(openTestDB(t))
	rows, err := h.UsageStats(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if rows == nil || len(rows) != 0 {
		t.Fatalf("want empty non-nil slice, got %#v", rows)
	}
}

func TestUsageStats(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		since time.Time
		setup func(t *testing.T, h *History)
		check func(t *testing.T, rows []UsageRow)
	}{
		{
			name: "pending and running excluded, failed vs cancelled",
			setup: func(t *testing.T, h *History) {
				for i, st := range []core.Status{core.StatusCompleted, core.StatusFailed, core.StatusCancelled, core.StatusPending, core.StatusRunning} {
					r := usageRec(string(rune('a'+i)), "c1", "m1", "s1", st, now)
					mustInsert(t, h, r)
				}
			},
			check: func(t *testing.T, rows []UsageRow) {
				if len(rows) != 1 {
					t.Fatalf("rows=%d", len(rows))
				}
				if rows[0].Requests != 3 || rows[0].Failed != 1 {
					t.Errorf("requests=%d failed=%d, want 3/1", rows[0].Requests, rows[0].Failed)
				}
			},
		},
		{
			name:  "window filter handles trimmed fractions",
			since: now,
			setup: func(t *testing.T, h *History) {
				mustInsert(t, h, usageRec("old", "c", "m", "s", core.StatusCompleted, now.Add(-time.Second)))
				// 12:00:00.5 sorts before 12:00:00Z as a string but is later in time.
				mustInsert(t, h, usageRec("frac", "c", "m", "s", core.StatusCompleted, now.Add(500*time.Millisecond)))
				mustInsert(t, h, usageRec("exact", "c", "m", "s", core.StatusCompleted, now))
			},
			check: func(t *testing.T, rows []UsageRow) {
				if len(rows) != 1 || rows[0].Requests != 2 {
					t.Fatalf("rows=%#v", rows)
				}
			},
		},
		{
			name: "zero since keeps everything",
			setup: func(t *testing.T, h *History) {
				mustInsert(t, h, usageRec("1", "c", "m", "s", core.StatusCompleted, now.Add(-1000*time.Hour)))
			},
			check: func(t *testing.T, rows []UsageRow) {
				if len(rows) != 1 || rows[0].Requests != 1 {
					t.Fatalf("rows=%#v", rows)
				}
			},
		},
		{
			name: "busy uses run_ms, falls back for NULL, ignores negative",
			setup: func(t *testing.T, h *History) {
				a := usageRec("a", "c", "m", "s", core.StatusCompleted, now)
				a.RunMs = 1000
				mustInsert(t, h, a)
				b := usageRec("b", "c", "m", "s", core.StatusCompleted, now)
				b.StartedAt = now
				b.CompletedAt = now.Add(2500 * time.Millisecond)
				mustInsert(t, h, b)
				neg := usageRec("n", "c", "m", "s", core.StatusCompleted, now)
				neg.StartedAt = now.Add(time.Second)
				neg.CompletedAt = now
				mustInsert(t, h, neg)
				never := usageRec("z", "c", "m", "s", core.StatusCancelled, now) // no timestamps, NULL run_ms
				mustInsert(t, h, never)
			},
			check: func(t *testing.T, rows []UsageRow) {
				if len(rows) != 1 {
					t.Fatalf("rows=%d", len(rows))
				}
				if d := rows[0].BusyMs - 3500; d < -2 || d > 2 {
					t.Errorf("BusyMs=%d want ~3500", rows[0].BusyMs)
				}
			},
		},
		{
			name: "queue avg only over measured rows, tokens summed, last_used",
			setup: func(t *testing.T, h *History) {
				a := usageRec("a", "c", "m", "s", core.StatusCompleted, now.Add(-time.Hour))
				a.QueueMs, a.PromptTokens, a.OutputTokens = 100, 10, 1
				mustInsert(t, h, a)
				b := usageRec("b", "c", "m", "s", core.StatusCompleted, now.Add(123*time.Millisecond))
				b.QueueMs, b.PromptTokens, b.OutputTokens = 300, 20, 2
				mustInsert(t, h, b)
				mustInsert(t, h, usageRec("c", "c", "m", "s", core.StatusCompleted, now.Add(-2*time.Hour))) // NULL queue
			},
			check: func(t *testing.T, rows []UsageRow) {
				r := rows[0]
				if r.QueueMsAvg != 200 || r.PromptTokens != 30 || r.OutputTokens != 3 {
					t.Errorf("got %+v", r)
				}
				if want := now.Add(123 * time.Millisecond); !r.LastUsed.Equal(want) {
					t.Errorf("LastUsed=%v want %v", r.LastUsed, want)
				}
			},
		},
		{
			name: "grouping by triple, ordered by requests desc",
			setup: func(t *testing.T, h *History) {
				mustInsert(t, h, usageRec("1", "c1", "m1", "s1", core.StatusCompleted, now))
				mustInsert(t, h, usageRec("2", "c1", "m1", "s2", core.StatusCompleted, now))
				mustInsert(t, h, usageRec("3", "c2", "m1", "s1", core.StatusCompleted, now))
				mustInsert(t, h, usageRec("4", "c2", "m1", "s1", core.StatusFailed, now))
				mustInsert(t, h, usageRec("5", "c2", "m1", "", core.StatusFailed, now))
			},
			check: func(t *testing.T, rows []UsageRow) {
				if len(rows) != 4 {
					t.Fatalf("rows=%d", len(rows))
				}
				if rows[0].Client != "c2" || rows[0].Server != "s1" || rows[0].Requests != 2 || rows[0].Failed != 1 {
					t.Errorf("first=%+v", rows[0])
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewHistory(openTestDB(t)) //nolint:contextcheck // test helper owns its DB lifecycle
			tt.setup(t, h)
			rows, err := h.UsageStats(ctx, tt.since)
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, rows)
		})
	}
}

func TestUsageStats_NullClientAndServer(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	// client_name/server_name are NOT NULL in the base schema, so '' is the
	// "never dispatched" representation.
	h := NewHistory(db)
	mustInsert(t, h, usageRec("1", "", "m", "", core.StatusFailed, time.Now().UTC()))
	rows, err := h.UsageStats(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Client != "" || rows[0].Server != "" {
		t.Fatalf("rows=%#v", rows)
	}
}

func mustInsert(t *testing.T, h *History, r *core.RequestRecord) {
	t.Helper()
	if err := h.Insert(context.Background(), r); err != nil {
		t.Fatalf("Insert %s: %v", r.ID, err)
	}
}

// Migration 005 must apply on top of a DB that already holds rows written
// under schema 001-004, leaving old rows with NULL run_ms/queue_ms.
func TestMigration005_AppliesToExistingRows(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"001_init.sql", "002_model_reloaded.sql", "003_last_failed_server.sql", "004_perf_observations.sql"} {
		b, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.applyMigration(ctx, name, b); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO requests (id, client_name, model, endpoint, status, created_at)
        VALUES ('old', 'c', 'm', '/v1/x', 'completed', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM requests WHERE id='old' AND run_ms IS NULL AND queue_ms IS NULL`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("old row: n=%d err=%v", n, err)
	}
	rows, err := NewHistory(db).UsageStats(ctx, time.Time{})
	if err != nil || len(rows) != 1 || rows[0].Requests != 1 {
		t.Fatalf("rows=%#v err=%v", rows, err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate not idempotent: %v", err)
	}
}
