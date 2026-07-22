package core

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"proxylm/internal/core/backends"
)

// stubResult — одна запрограммированная реакция stubBackend.ListModels.
type stubResult struct {
	models []string
	err    error
}

// stubBackend — тестовая реализация backends.Backend с программируемой
// последовательностью ответов ListModels (последний результат повторяется,
// если вызовов больше, чем результатов в очереди). Forward не используется
// discovery-кодом — паникует, если вызван по ошибке.
type stubBackend struct {
	name string

	mu      sync.Mutex
	results []stubResult
	idx     int
	calls   int
}

func newStubBackend(name string, results ...stubResult) *stubBackend {
	return &stubBackend{name: name, results: results}
}

func (s *stubBackend) Name() string     { return s.name }
func (s *stubBackend) Protocol() string { return "openai" }

func (s *stubBackend) ListModels(_ context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if len(s.results) == 0 {
		return nil, nil
	}
	i := s.idx
	if i >= len(s.results) {
		i = len(s.results) - 1
	} else {
		s.idx++
	}
	return s.results[i].models, s.results[i].err
}

func (s *stubBackend) Forward(context.Context, string, io.Reader, http.Header) (*http.Response, error) {
	panic("stubBackend.Forward: not used by discovery")
}

func (s *stubBackend) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestDiscovery_InitialHealthcheck covers the four outcome branches of
// InitialHealthcheck for a non-explicit-models server: shape error, network
// error, normal list, and empty list ({"data":[]} equivalent — the backend
// already parsed it into an empty non-nil slice with nil error).
func TestDiscovery_InitialHealthcheck(t *testing.T) {
	tests := []struct {
		name        string
		result      stubResult
		wantHealthy bool
		wantModels  []string
	}{
		{
			name:        "shape_error_marks_unhealthy",
			result:      stubResult{err: errors.New("wrap: " + backends.ErrModelsShape.Error())},
			wantHealthy: false,
		},
		{
			name:        "shape_error_sentinel_marks_unhealthy",
			result:      stubResult{err: backends.ErrModelsShape},
			wantHealthy: false,
		},
		{
			name:        "network_error_marks_unhealthy",
			result:      stubResult{err: errors.New("dial tcp: connection refused")},
			wantHealthy: false,
		},
		{
			name:        "normal_list_marks_healthy_and_populates_models",
			result:      stubResult{models: []string{"model-a", "model-b"}},
			wantHealthy: true,
			wantModels:  []string{"model-a", "model-b"},
		},
		{
			name:        "empty_list_is_healthy_zero_models",
			result:      stubResult{models: []string{}},
			wantHealthy: true,
			wantModels:  []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := NewDiscovery(time.Hour, 3, discardLogger())
			srv := NewServerInfo("srv", "http://example.invalid", 100, "openai", 5*time.Second, "")
			backend := newStubBackend("srv", tt.result)
			d.AddServer(srv, backend, nil)

			d.InitialHealthcheck(context.Background())

			if got := srv.Healthy.Load(); got != tt.wantHealthy {
				t.Fatalf("Healthy = %v, want %v", got, tt.wantHealthy)
			}
			if tt.wantModels != nil {
				srv.Lock()
				got := make([]string, 0, len(srv.Models))
				for _, m := range srv.Models {
					got = append(got, m.Name)
				}
				srv.Unlock()
				assertEqualStringsCore(t, got, tt.wantModels)
			}
		})
	}
}

// TestDiscovery_InitialHealthcheck_ExplicitModels: server with explicitModels
// (config.backends[].models set) is marked healthy without ever calling
// ListModels — the config is the source of truth for its model list.
func TestDiscovery_InitialHealthcheck_ExplicitModels(t *testing.T) {
	d := NewDiscovery(time.Hour, 3, discardLogger())
	srv := NewServerInfo("srv", "http://example.invalid", 100, "openai", 5*time.Second, "")
	backend := newStubBackend("srv") // no results programmed; a call would return (nil,nil)
	d.AddServer(srv, backend, []string{"explicit-a", "explicit-b"})

	d.InitialHealthcheck(context.Background())

	if !srv.Healthy.Load() {
		t.Fatalf("explicit-models server should be healthy after InitialHealthcheck")
	}
	if got := backend.callCount(); got != 0 {
		t.Fatalf("ListModels should not be called for explicit-models server, got %d calls", got)
	}
}

// TestDiscovery_PollOne_ExplicitModelsShapeErrorStaysHealthy: an
// explicit-models server whose /v1/models responds but in a non-OpenAI shape
// must be treated as a successful liveness check (endpoint responded) and
// stay/become healthy across unhealthyAfterFailedPolls+1 consecutive polls —
// it must NOT accumulate failedPolls like a real failure would.
func TestDiscovery_PollOne_ExplicitModelsShapeErrorStaysHealthy(t *testing.T) {
	const unhealthyAfter = 3
	d := NewDiscovery(time.Hour, unhealthyAfter, discardLogger())
	srv := NewServerInfo("srv", "http://example.invalid", 100, "openai", 5*time.Second, "")
	srv.Healthy.Store(true)
	backend := newStubBackend("srv", stubResult{err: backends.ErrModelsShape})
	d.AddServer(srv, backend, []string{"explicit-a"})

	entry := d.entries[0]
	for i := 0; i < unhealthyAfter+1; i++ {
		d.pollOne(context.Background(), entry)
		if !srv.Healthy.Load() {
			t.Fatalf("poll %d: explicit-models server became unhealthy on shape error", i+1)
		}
		if entry.failedPolls != 0 {
			t.Fatalf("poll %d: failedPolls = %d, want 0 (shape error must reset it for explicit-models servers)", i+1, entry.failedPolls)
		}
	}
}

// TestDiscovery_PollOne_NonExplicitShapeErrorGoesUnhealthy: a non-explicit
// server whose backend keeps returning ErrModelsShape must accumulate
// failedPolls exactly like any other error and flip to unhealthy once the
// threshold is reached.
func TestDiscovery_PollOne_NonExplicitShapeErrorGoesUnhealthy(t *testing.T) {
	const unhealthyAfter = 3
	d := NewDiscovery(time.Hour, unhealthyAfter, discardLogger())
	srv := NewServerInfo("srv", "http://example.invalid", 100, "openai", 5*time.Second, "")
	srv.Healthy.Store(true)
	backend := newStubBackend("srv", stubResult{err: backends.ErrModelsShape})
	d.AddServer(srv, backend, nil)

	entry := d.entries[0]
	for i := 1; i <= unhealthyAfter; i++ {
		d.pollOne(context.Background(), entry)
		wantHealthy := i < unhealthyAfter
		if got := srv.Healthy.Load(); got != wantHealthy {
			t.Fatalf("poll %d: Healthy = %v, want %v (unhealthyAfter=%d)", i, got, wantHealthy, unhealthyAfter)
		}
	}
	if entry.failedPolls != unhealthyAfter {
		t.Fatalf("failedPolls = %d, want %d", entry.failedPolls, unhealthyAfter)
	}
}

// assertEqualStringsCore mirrors backends.assertEqualStrings (unexported,
// different package) for use within internal/core tests.
func assertEqualStringsCore(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len mismatch: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("at %d: got %q want %q (full got=%v)", i, got[i], want[i], got)
		}
	}
}
