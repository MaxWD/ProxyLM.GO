package core

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// tickClock — детерминированные часы: каждый вызов продвигает время на 1 мс.
func tickClock() func() time.Time {
	var n atomic.Int64
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return func() time.Time {
		return base.Add(time.Duration(n.Add(1)) * time.Millisecond)
	}
}

func TestScheduler_RunMsQueueMs(t *testing.T) {
	tests := []struct {
		name        string
		strategy    RouterStrategy
		failures    int // сколько первых попыток падают
		maxAttempts int
		wantRunMs   int64
		wantErr     bool
	}{
		{"push single attempt", RouterModelAffinityLeastBusy, 0, 1, 1, false},
		{"push retried sums attempts", RouterModelAffinityLeastBusy, 2, 3, 3, false},
		{"push exhausted", RouterModelAffinityLeastBusy, 5, 2, 2, true},
		{"deferred retried sums attempts", RouterDeferredModelThenCapable, 2, 3, 3, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			srv := mkSrv(t, "a", 100, true, "", "m1")
			r := NewRouter([]*ServerInfo{srv}, tt.strategy)
			s := NewScheduler([]*ServerInfo{srv}, r, RetryConfig{MaxAttempts: tt.maxAttempts}, nil)
			s.now = tickClock()
			s.Start(ctx)

			var calls atomic.Int32
			job := &Job{ID: "j", Model: "m1", Run: func(context.Context, *ServerInfo) JobResult {
				if int(calls.Add(1)) <= tt.failures {
					return JobResult{Err: errors.New("boom"), HTTPStatus: 502}
				}
				return JobResult{HTTPStatus: 200}
			}}
			res, err := s.Submit(ctx, job)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tt.wantErr)
			}
			if res.RunMs != tt.wantRunMs {
				t.Errorf("RunMs=%d want %d", res.RunMs, tt.wantRunMs)
			}
			if res.QueueMs <= 0 {
				t.Errorf("QueueMs=%d want >0", res.QueueMs)
			}
		})
	}
}

func TestScheduler_QueueMsZeroWhenNeverDispatched(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := mkSrv(t, "a", 100, true, "", "m1")
	r := NewRouter([]*ServerInfo{srv}, RouterModelAffinityLeastBusy)
	s := NewScheduler([]*ServerInfo{srv}, r, RetryConfig{MaxAttempts: 1}, nil)
	s.now = tickClock()
	s.Start(ctx)

	res, err := s.Submit(ctx, &Job{ID: "j", Model: "unknown", Run: func(context.Context, *ServerInfo) JobResult {
		return JobResult{}
	}})
	if err == nil {
		t.Fatal("want ErrNoServer")
	}
	if res.RunMs != 0 || res.QueueMs != 0 {
		t.Errorf("RunMs=%d QueueMs=%d, want 0/0", res.RunMs, res.QueueMs)
	}
}
