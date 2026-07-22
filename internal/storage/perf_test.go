package storage

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"proxylm/internal/core"
)

// openTestDB opens a fresh migrated SQLite DB in t.TempDir() and registers
// cleanup, mirroring test/integration/api_e2e_test.go's startProxy helper.
func openTestDB(t *testing.T) *DB {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// waitFor polls cond every interval until it returns true, or fails the test
// once ctx is done. No raw time.Sleep-as-synchronization: the poll cadence
// is driven by a ticker inside a select alongside ctx.Done().
func waitFor(t *testing.T, ctx context.Context, interval time.Duration, cond func() bool) {
	t.Helper()
	if cond() {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for condition")
		case <-ticker.C:
			if cond() {
				return
			}
		}
	}
}

// TestPerfStore_EnqueueRun_RoundTrip exercises the async path: Enqueue (from
// a producer, mimicking core.PerfTracker's sink callback) feeding the single
// Run writer goroutine, and LoadAll reading back what was written.
func TestPerfStore_EnqueueRun_RoundTrip(t *testing.T) {
	db := openTestDB(t)
	store := NewPerfStore(db, discardLog())

	runCtx, cancel := context.WithCancel(context.Background())
	// LIFO cleanup: cancel (registered second) runs BEFORE the DB is closed
	// (registered first), so Run's writer goroutine stops before db.Close().
	t.Cleanup(func() { _ = db.Close() })
	t.Cleanup(cancel)
	go store.Run(runCtx)

	const key = "srv1"
	const model = "m1"
	const endpoint = "/v1/chat/completions"
	want := []core.Observation{
		{In: 10, Out: 1, TotalMs: 100, Loaded: true},
		{In: 20, Out: 2, TotalMs: 200, Loaded: false},
		{In: 30, Out: 3, TotalMs: 300, Loaded: false},
	}
	for _, o := range want {
		store.Enqueue(key, model, endpoint, o)
	}

	deadline, deadlineCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer deadlineCancel()

	var got []core.Observation
	waitFor(t, deadline, 10*time.Millisecond, func() bool {
		all, err := store.LoadAll(context.Background())
		if err != nil {
			t.Fatalf("LoadAll: %v", err)
		}
		rows := all[PerfKey{Server: key, Model: model, Endpoint: endpoint}]
		if len(rows) == len(want) {
			got = rows
			return true
		}
		return false
	})

	for i, w := range want {
		if got[i] != w {
			t.Errorf("row %d = %+v, want %+v", i, got[i], w)
		}
	}
}

// TestPerfStore_LoadAll_GroupingAndOrder inserts observations for two
// interleaved keys directly (synchronously, bypassing the async Enqueue/Run
// path for determinism) and verifies LoadAll groups by (server, model,
// endpoint) and orders each group by id ASC (insertion order).
func TestPerfStore_LoadAll_GroupingAndOrder(t *testing.T) {
	db := openTestDB(t)
	store := NewPerfStore(db, discardLog())

	keyA := PerfKey{Server: "srv1", Model: "m1", Endpoint: "/v1/chat/completions"}
	keyB := PerfKey{Server: "srv2", Model: "m2", Endpoint: "/v1/embeddings"}

	// Interleave insertion order: A, B, A, B, A.
	insertOrder := []struct {
		key PerfKey
		out int
	}{
		{keyA, 1}, {keyB, 101}, {keyA, 2}, {keyB, 102}, {keyA, 3},
	}
	for _, e := range insertOrder {
		store.insert(perfRow{
			server: e.key.Server, model: e.key.Model, endpoint: e.key.Endpoint,
			obs: core.Observation{In: 10, Out: e.out, TotalMs: int64(e.out), Loaded: false},
		})
	}

	all, err := store.LoadAll(context.Background())
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("len(all) = %d, want 2 keys", len(all))
	}
	rowsA := all[keyA]
	rowsB := all[keyB]
	if len(rowsA) != 3 {
		t.Fatalf("len(rowsA) = %d, want 3", len(rowsA))
	}
	if len(rowsB) != 2 {
		t.Fatalf("len(rowsB) = %d, want 2", len(rowsB))
	}
	wantA := []int{1, 2, 3}
	for i, w := range wantA {
		if rowsA[i].Out != w {
			t.Errorf("rowsA[%d].Out = %d, want %d (id ASC order within key A)", i, rowsA[i].Out, w)
		}
	}
	wantB := []int{101, 102}
	for i, w := range wantB {
		if rowsB[i].Out != w {
			t.Errorf("rowsB[%d].Out = %d, want %d (id ASC order within key B)", i, rowsB[i].Out, w)
		}
	}
}

// TestPerfStore_Trim verifies that Trim keeps exactly the newest perKeyCap
// rows per key, leaves keys with fewer rows untouched, and reports the
// correct deleted row count.
func TestPerfStore_Trim(t *testing.T) {
	db := openTestDB(t)
	store := NewPerfStore(db, discardLog())

	const cap_ = 5
	keyBig := PerfKey{Server: "srv1", Model: "m1", Endpoint: "/v1/chat/completions"}
	keySmall := PerfKey{Server: "srv2", Model: "m2", Endpoint: "/v1/chat/completions"}

	// keyBig: cap_+5 = 10 rows, Out = 1..10 (insertion order ⇒ id order).
	const bigTotal = cap_ + 5
	for i := 1; i <= bigTotal; i++ {
		store.insert(perfRow{
			server: keyBig.Server, model: keyBig.Model, endpoint: keyBig.Endpoint,
			obs: core.Observation{In: 1, Out: i, TotalMs: int64(i), Loaded: false},
		})
	}
	// keySmall: cap_-1 = 4 rows, must remain untouched by Trim.
	const smallTotal = cap_ - 1
	for i := 1; i <= smallTotal; i++ {
		store.insert(perfRow{
			server: keySmall.Server, model: keySmall.Model, endpoint: keySmall.Endpoint,
			obs: core.Observation{In: 1, Out: 1000 + i, TotalMs: int64(i), Loaded: false},
		})
	}

	deleted, err := store.Trim(context.Background(), cap_)
	if err != nil {
		t.Fatalf("Trim: %v", err)
	}
	if deleted != bigTotal-cap_ {
		t.Fatalf("deleted = %d, want %d", deleted, bigTotal-cap_)
	}

	all, err := store.LoadAll(context.Background())
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	rowsBig := all[keyBig]
	if len(rowsBig) != cap_ {
		t.Fatalf("len(rowsBig) = %d, want %d (newest kept)", len(rowsBig), cap_)
	}
	// The newest cap_ rows have Out = bigTotal-cap_+1 .. bigTotal.
	for i, r := range rowsBig {
		want := bigTotal - cap_ + 1 + i
		if r.Out != want {
			t.Errorf("rowsBig[%d].Out = %d, want %d (oldest must be trimmed away)", i, r.Out, want)
		}
	}

	rowsSmall := all[keySmall]
	if len(rowsSmall) != smallTotal {
		t.Fatalf("len(rowsSmall) = %d, want %d (untouched — below cap)", len(rowsSmall), smallTotal)
	}
	for i := 0; i < smallTotal; i++ {
		want := 1000 + i + 1
		if rowsSmall[i].Out != want {
			t.Errorf("rowsSmall[%d].Out = %d, want %d", i, rowsSmall[i].Out, want)
		}
	}
}
