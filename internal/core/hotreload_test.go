package core

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"proxylm/internal/core/backends"
)

func TestRouter_SetStrategy(t *testing.T) {
	tests := []struct {
		name     string
		strategy RouterStrategy
		want     string
	}{
		// b уже держит модель, но a занимает приоритетное место при least_busy (равная нагрузка).
		{"affinity prefers loaded server", RouterModelAffinityLeastBusy, "b"},
		{"least_busy ignores loaded model", RouterLeastBusy, "a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := mkSrv(t, "a", 100, true, "", "m")
			b := mkSrv(t, "b", 200, true, "m", "m")
			// стартуем с противоположной стратегии и переключаем на лету
			start := RouterLeastBusy
			if tt.strategy == RouterLeastBusy {
				start = RouterModelAffinityLeastBusy
			}
			r := NewRouter([]*ServerInfo{a, b}, start)
			r.SetStrategy(tt.strategy)
			if r.Strategy() != tt.strategy {
				t.Fatalf("Strategy() = %s, want %s", r.Strategy(), tt.strategy)
			}
			got, err := r.Pick("m", nil)
			if err != nil {
				t.Fatal(err)
			}
			if got.Name != tt.want {
				t.Errorf("Pick = %s, want %s", got.Name, tt.want)
			}
		})
	}
}

func TestScheduler_SetRetryChangesAttempts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := mkSrv(t, "a", 100, true, "", "m")
	r := NewRouter([]*ServerInfo{srv}, RouterModelAffinityLeastBusy)
	s := NewScheduler([]*ServerInfo{srv}, r, RetryConfig{MaxAttempts: 1}, nil)
	s.Start(ctx)

	var attempts atomic.Int32
	submit := func(id string) {
		attempts.Store(0)
		_, err := s.Submit(ctx, &Job{ID: id, Model: "m", Run: func(context.Context, *ServerInfo) JobResult {
			attempts.Add(1)
			e := errors.New("upstream 500")
			return JobResult{Err: e}
		}})
		if err == nil {
			t.Error("expected error")
		}
	}

	submit("j1")
	if got := attempts.Load(); got != 1 {
		t.Errorf("before SetRetry: attempts = %d, want 1", got)
	}
	s.SetRetry(RetryConfig{MaxAttempts: 3})
	submit("j2")
	if got := attempts.Load(); got != 3 {
		t.Errorf("after SetRetry: attempts = %d, want 3", got)
	}
}

// TestScheduler_StrategySwitchDrainsBothSources: переключение push <-> pull на
// лету не теряет Job'ы (INV-7) и оба режима работают.
func TestScheduler_StrategySwitch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := mkSrv(t, "a", 100, true, "", "m")
	r := NewRouter([]*ServerInfo{srv}, RouterDeferredModelThenCapable)
	s := NewScheduler([]*ServerInfo{srv}, r, RetryConfig{MaxAttempts: 1}, nil)
	s.Start(ctx)

	run := func(context.Context, *ServerInfo) JobResult { return JobResult{HTTPStatus: 200} }
	steps := []RouterStrategy{
		RouterDeferredModelThenCapable, RouterRoundRobin, RouterFairShareRoundRobin,
		RouterLeastBusy, RouterPreserveModelCoverage, RouterModelAffinityLeastBusy,
	}
	for _, st := range steps {
		r.SetStrategy(st)
		s.WakeAll()
		if _, err := s.Submit(ctx, &Job{ID: "j-" + string(st), Model: "m", Run: run}); err != nil {
			t.Fatalf("strategy %s: Submit: %v", st, err)
		}
	}

	// «Хвост» push-очереди, оставшийся после перехода в pull-режим, дренируется.
	r.SetStrategy(RouterDeferredModelThenCapable)
	leftover := &Job{ID: "leftover", Model: "m", Run: run, done: make(chan JobResult, 1)}
	srv.Lock()
	srv.Queue = append(srv.Queue, leftover)
	srv.Unlock()
	srv.Wake()
	select {
	case res := <-leftover.done:
		if res.Err != nil {
			t.Errorf("leftover: %v", res.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("leftover push-queue job was not drained in pull mode")
	}

	// И наоборот: Job в пуле при push-режиме.
	r.SetStrategy(RouterRoundRobin)
	pooled := &Job{ID: "pooled", Model: "m", Run: run, done: make(chan JobResult, 1)}
	s.pool.Push(pooled)
	srv.Wake()
	select {
	case res := <-pooled.done:
		if res.Err != nil {
			t.Errorf("pooled: %v", res.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("leftover pool job was not drained in push mode")
	}
}

// pollNotifier — backend, сигнализирующий о каждом вызове ListModels.
type pollNotifier struct {
	*stubBackend
	polled chan struct{}
}

func (p *pollNotifier) ListModels(ctx context.Context) ([]string, error) {
	defer func() {
		select {
		case p.polled <- struct{}{}:
		default:
		}
	}()
	return p.stubBackend.ListModels(ctx)
}

func TestDiscovery_SetIntervalResetsTicker(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := NewDiscovery(time.Hour, 3, discardLogger())
	srv := NewServerInfo("srv", "http://example.invalid", 100, "openai", time.Second, "")
	pn := &pollNotifier{stubBackend: newStubBackend("srv", stubResult{models: []string{"m"}}), polled: make(chan struct{}, 16)}
	d.AddServer(srv, pn, nil)

	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()

	waitPoll := func(what string) {
		t.Helper()
		select {
		case <-pn.polled:
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout waiting for %s", what)
		}
	}
	waitPoll("initial poll")
	d.SetInterval(5 * time.Millisecond) // ticker 1h -> 5ms без ожидания часа
	waitPoll("poll after interval change")
	waitPoll("second poll after interval change")

	d.SetInterval(0) // игнорируется
	if got := time.Duration(d.interval.Load()); got != 5*time.Millisecond {
		t.Errorf("interval = %v, want 5ms", got)
	}
	cancel()
	<-done
}

func TestDiscovery_SetUnhealthyAfter(t *testing.T) {
	d := NewDiscovery(time.Hour, 5, discardLogger())
	srv := NewServerInfo("srv", "http://example.invalid", 100, "openai", time.Second, "")
	srv.Healthy.Store(true)
	d.AddServer(srv, newStubBackend("srv", stubResult{err: backends.ErrModelsShape}), nil)
	d.SetUnhealthyAfter(2)
	d.SetUnhealthyAfter(0) // игнорируется

	e := d.entries[0]
	d.pollOne(context.Background(), e)
	if !srv.Healthy.Load() {
		t.Fatal("unhealthy after 1 failed poll, threshold is 2")
	}
	d.pollOne(context.Background(), e)
	if srv.Healthy.Load() {
		t.Fatal("still healthy after 2 failed polls with threshold 2")
	}
}
