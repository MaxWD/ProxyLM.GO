package integration

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"proxylm/internal/core"
	"proxylm/internal/storage"
)

// TestPerfPersistence_SurvivesRestart exercises the full persistence round
// trip introduced for perf-observation restore: write through PerfStore.Run
// (async single-writer path), stop the writer and close the DB (simulating
// daemon shutdown), reopen the same SQLite file (simulating daemon restart),
// LoadAll the persisted rows, feed a brand-new core.PerfTracker via Load, and
// confirm Snapshot immediately reports OK with the expected sample count —
// with no further Record calls needed.
func TestPerfPersistence_SurvivesRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "perf-restart.db")
	const (
		server   = "srv1"
		model    = "qwen2.5-7b"
		endpoint = "/v1/chat/completions"
	)
	// perfMinSamples in internal/core is 3; use 4 to be comfortably above it.
	want := []core.Observation{
		{In: 100, Out: 50, TotalMs: 900, Loaded: true},
		{In: 200, Out: 100, TotalMs: 1500, Loaded: false},
		{In: 150, Out: 80, TotalMs: 1300, Loaded: false},
		{In: 300, Out: 120, TotalMs: 2100, Loaded: false},
	}

	// --- "First run": write observations, then shut down. ---
	func() {
		writeCtx, cancel := context.WithCancel(context.Background())
		db, err := storage.Open(writeCtx, dbPath)
		if err != nil {
			t.Fatalf("storage.Open (first run): %v", err)
		}
		if err := db.Migrate(writeCtx); err != nil {
			t.Fatalf("Migrate (first run): %v", err)
		}

		store := storage.NewPerfStore(db, nil)
		go store.Run(writeCtx)

		for _, o := range want {
			store.Enqueue(server, model, endpoint, o)
		}

		// Poll (ticker-driven, no time.Sleep-as-sync) until all rows are
		// durably persisted before tearing down the writer + DB.
		deadline, deadlineCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer deadlineCancel()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
	pollLoop:
		for {
			select {
			case <-deadline.Done():
				t.Fatalf("timed out waiting for observations to persist")
			case <-ticker.C:
				all, err := store.LoadAll(context.Background())
				if err != nil {
					t.Fatalf("LoadAll (first run): %v", err)
				}
				if len(all[storage.PerfKey{Server: server, Model: model, Endpoint: endpoint}]) == len(want) {
					break pollLoop
				}
			}
		}

		// Stop the writer before closing the DB out from under it.
		cancel()
		if err := db.Close(); err != nil {
			t.Fatalf("db.Close (first run): %v", err)
		}
	}()

	// --- "Restart": reopen the same file, restore the tracker, and verify. ---
	restartCtx := context.Background()
	db2, err := storage.Open(restartCtx, dbPath)
	if err != nil {
		t.Fatalf("storage.Open (restart): %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	if err := db2.Migrate(restartCtx); err != nil {
		t.Fatalf("Migrate (restart): %v", err)
	}

	store2 := storage.NewPerfStore(db2, nil)
	loaded, err := store2.LoadAll(restartCtx)
	if err != nil {
		t.Fatalf("LoadAll (restart): %v", err)
	}

	tracker := core.NewPerfTracker()
	for k, obs := range loaded {
		tracker.Load(k.Server, k.Model, k.Endpoint, obs)
	}

	snap := tracker.Snapshot(server, model, endpoint)
	if !snap.OK {
		t.Fatalf("Snapshot.OK = false after restore, want true: %+v", snap)
	}
	if snap.Samples != len(want) {
		t.Errorf("Snapshot.Samples = %d, want %d", snap.Samples, len(want))
	}
}
