package tui

import (
	"strings"
	"testing"
	"time"

	"proxylm/internal/ipc"
)

// TestFmtTLoad — маркер уверенности оценки t_load (v0.12.0):
// loaded==0 → «—»; 0<loaded<min → значение со «*»; loaded≥min → чистое значение.
func TestFmtTLoad(t *testing.T) {
	tests := []struct {
		name    string
		tLoadMs float64
		loaded  int
		want    string
	}{
		{"no reload observed", 0, 0, "—"},
		{"reload observed but value zero still marked", 0, 1, "0ms*"},
		{"single reload → low confidence marker", 1400, 1, "1.4s*"},
		{"two reloads → still low confidence", 1400, 2, "1.4s*"},
		{"enough reloads → confident", 1400, 3, "1.4s"},
		{"many reloads → confident", 850, 10, "850ms"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fmtTLoad(tt.tLoadMs, tt.loaded); got != tt.want {
				t.Errorf("fmtTLoad(%v, %d) = %q, want %q", tt.tLoadMs, tt.loaded, got, tt.want)
			}
		})
	}
}

// TestRequestRowColor — table-driven unit test для функции определения цвета строки
// по статусу + INV-2 контекст (needsSwap).
func TestRequestRowColor(t *testing.T) {
	tests := []struct {
		name      string
		status    string
		needsSwap bool
		wantDim   bool // ожидаем StyleNeedsSwap (dim) или нет
	}{
		{
			name:      "running no swap",
			status:    "running",
			needsSwap: false,
			wantDim:   false,
		},
		{
			name:      "running needs swap overrides to dim",
			status:    "running",
			needsSwap: true,
			wantDim:   true,
		},
		{
			name:      "queued no swap",
			status:    "queued",
			needsSwap: false,
			wantDim:   false,
		},
		{
			name:      "queued needs swap → dim",
			status:    "queued",
			needsSwap: true,
			wantDim:   true,
		},
		{
			name:      "pending needs swap → dim",
			status:    "pending",
			needsSwap: true,
			wantDim:   true,
		},
		{
			name:      "completed no swap",
			status:    "completed",
			needsSwap: false,
			wantDim:   false,
		},
		{
			name:      "failed no swap",
			status:    "failed",
			needsSwap: false,
			wantDim:   false,
		},
		{
			name:      "in_progress no swap",
			status:    "in_progress",
			needsSwap: false,
			wantDim:   false,
		},
		{
			name:      "unknown status no swap",
			status:    "unknown",
			needsSwap: false,
			wantDim:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := RequestRowColor(tc.status, tc.needsSwap)
			// When needsSwap is true, we expect StyleNeedsSwap.
			// We compare by rendering a known string and checking equality.
			if tc.wantDim {
				want := StyleNeedsSwap
				if got.GetForeground() != want.GetForeground() {
					t.Errorf("status=%q needsSwap=%v: got foreground %v, want %v (StyleNeedsSwap)",
						tc.status, tc.needsSwap, got.GetForeground(), want.GetForeground())
				}
			} else {
				// Must not be StyleNeedsSwap.
				if got.GetForeground() == StyleNeedsSwap.GetForeground() &&
					tc.status != "completed" && tc.status != "unknown" {
					// completed and unknown legitimately use dim/gray colours,
					// so only assert for active statuses.
					if tc.status == "running" || tc.status == "in_progress" ||
						tc.status == "queued" || tc.status == "pending" {
						t.Errorf("status=%q needsSwap=false: got StyleNeedsSwap unexpectedly",
							tc.status)
					}
				}
			}
		})
	}
}

// TestCalcNeedsSwap verifies INV-2 visual logic:
// queued/pending requests show ~ when no healthy server has current_model == r.Model.
func TestCalcNeedsSwap(t *testing.T) {
	makeServer := func(name, model string, healthy bool) ipc.ServerState {
		return ipc.ServerState{Name: name, CurrentModel: model, Healthy: healthy}
	}
	makeReq := func(status, model string) ipc.RequestState {
		return ipc.RequestState{Status: status, Model: model}
	}

	tests := []struct {
		name    string
		req     ipc.RequestState
		servers []ipc.ServerState
		want    bool
	}{
		{
			name:    "queued, matching healthy server → no swap",
			req:     makeReq("queued", "qwen"),
			servers: []ipc.ServerState{makeServer("srv1", "qwen", true)},
			want:    false,
		},
		{
			name:    "queued, no matching server → needs swap",
			req:     makeReq("queued", "llama"),
			servers: []ipc.ServerState{makeServer("srv1", "qwen", true)},
			want:    true,
		},
		{
			name:    "queued, matching server unhealthy → needs swap",
			req:     makeReq("queued", "qwen"),
			servers: []ipc.ServerState{makeServer("srv1", "qwen", false)},
			want:    true,
		},
		{
			name:    "queued, no servers → needs swap",
			req:     makeReq("queued", "llama"),
			servers: nil,
			want:    true,
		},
		{
			name:    "pending, no match → needs swap",
			req:     makeReq("pending", "llama"),
			servers: []ipc.ServerState{makeServer("srv1", "qwen", true)},
			want:    true,
		},
		{
			name:    "running → never needs swap display",
			req:     makeReq("running", "llama"),
			servers: []ipc.ServerState{makeServer("srv1", "qwen", true)},
			want:    false,
		},
		{
			name:    "completed → never needs swap display",
			req:     makeReq("completed", "llama"),
			servers: []ipc.ServerState{makeServer("srv1", "qwen", true)},
			want:    false,
		},
		{
			name: "queued, second server matches → no swap",
			req:  makeReq("queued", "llama"),
			servers: []ipc.ServerState{
				makeServer("srv1", "qwen", true),
				makeServer("srv2", "llama", true),
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := calcNeedsSwap(tc.req, tc.servers)
			if got != tc.want {
				t.Errorf("calcNeedsSwap(%q, %q, servers): got %v, want %v",
					tc.req.Status, tc.req.Model, got, tc.want)
			}
		})
	}
}

// TestRequestVisible checks TTL expiry logic for completed/failed rows.
func TestRequestVisible(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name        string
		status      string
		completedAt time.Time
		ttlMinutes  int
		want        bool
	}{
		{
			name:        "running always visible",
			status:      "running",
			completedAt: time.Time{},
			ttlMinutes:  30,
			want:        true,
		},
		{
			name:        "queued always visible",
			status:      "queued",
			completedAt: time.Time{},
			ttlMinutes:  30,
			want:        true,
		},
		{
			name:        "completed within TTL",
			status:      "completed",
			completedAt: now.Add(-10 * time.Minute),
			ttlMinutes:  30,
			want:        true,
		},
		{
			name:        "completed exactly at TTL boundary",
			status:      "completed",
			completedAt: now.Add(-31 * time.Minute),
			ttlMinutes:  30,
			want:        false,
		},
		{
			name:        "failed past TTL",
			status:      "failed",
			completedAt: now.Add(-60 * time.Minute),
			ttlMinutes:  30,
			want:        false,
		},
		{
			name:        "completed zero time → visible",
			status:      "completed",
			completedAt: time.Time{},
			ttlMinutes:  30,
			want:        true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := ipc.RequestState{
				Status:      tc.status,
				CompletedAt: tc.completedAt,
			}
			got := requestVisible(r, now, tc.ttlMinutes)
			if got != tc.want {
				t.Errorf("requestVisible(%q, completedAt=%v, ttl=%d): got %v, want %v",
					tc.status, tc.completedAt, tc.ttlMinutes, got, tc.want)
			}
		})
	}
}

// TestRequestMatchesFilter verifies case-insensitive substring filtering.
func TestRequestMatchesFilter(t *testing.T) {
	r := ipc.RequestState{
		Model:      "qwen2.5:14b",
		ClientName: "service-a",
		ServerName: "srv1",
		Status:     "running",
	}

	tests := []struct {
		filter string
		want   bool
	}{
		{"", true},
		{"qwen", true},
		{"QWEN", true},
		{"14b", true},
		{"service", true},
		{"SERVICE-A", true},
		{"srv", true},
		{"run", true},
		{"llama", false},
		{"srv2", false},
	}

	for _, tc := range tests {
		t.Run("filter="+tc.filter, func(t *testing.T) {
			got := requestMatchesFilter(r, tc.filter)
			if got != tc.want {
				t.Errorf("requestMatchesFilter(filter=%q): got %v, want %v", tc.filter, got, tc.want)
			}
		})
	}
}

// TestStyleCIMargin проверяет, что styleCIMargin красит суффикс «±<margin>»,
// сохраняя ведущее значение, и не трогает строки без «±» (plain / «—» / пустые).
//
// Проверки специально dep-free и независимы от color-profile lipgloss: «ведущая
// часть сохранена как префикс» и «суффикс ±… присутствует» истинны и когда цвет
// активен (ANSI вставляется ПЕРЕД «±», подстрока остаётся непрерывной), и когда
// рендерер вне TTY возвращает plain. Это надёжнее, чем форсить TrueColor и
// ассертить конкретные escape-коды.
func TestStyleCIMargin(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantSame   bool   // true → результат должен совпадать со входом (нет «±»)
		wantPrefix string // ведущая часть до «±», должна сохраниться
		wantInfix  string // суффикс «±…», должен присутствовать
	}{
		{name: "value with CI suffix preserved", input: "38.5±5.1", wantPrefix: "38.5", wantInfix: "±5.1"},
		{name: "plain value without ± unchanged", input: "38.5", wantSame: true},
		{name: "dash unchanged", input: "—", wantSame: true},
		{name: "empty string unchanged", input: "", wantSame: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := styleCIMargin(tc.input)
			if tc.wantSame {
				if got != tc.input {
					t.Errorf("styleCIMargin(%q) = %q, want unchanged", tc.input, got)
				}
				return
			}
			if !strings.HasPrefix(got, tc.wantPrefix) {
				t.Errorf("styleCIMargin(%q) = %q: does not start with %q", tc.input, got, tc.wantPrefix)
			}
			if !strings.Contains(got, tc.wantInfix) {
				t.Errorf("styleCIMargin(%q) = %q: does not contain %q", tc.input, got, tc.wantInfix)
			}
		})
	}
}

// TestSortedRequests verifies that active requests float above completed ones.
func TestSortedRequests(t *testing.T) {
	t0 := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)
	reqs := []ipc.RequestState{
		{ID: "a", Status: "completed", CreatedAt: t0, CompletedAt: t0.Add(5 * time.Second)},
		{ID: "b", Status: "running", CreatedAt: t0.Add(1 * time.Second)},
		{ID: "c", Status: "queued", CreatedAt: t0.Add(2 * time.Second)},
		{ID: "d", Status: "failed", CreatedAt: t0, CompletedAt: t0.Add(3 * time.Second)},
	}

	sorted := sortedRequests(reqs)
	if len(sorted) != 4 {
		t.Fatalf("expected 4 rows, got %d", len(sorted))
	}

	// First two must be active (running, queued).
	if sorted[0].Status != "running" {
		t.Errorf("sorted[0]: want running, got %s", sorted[0].Status)
	}
	if sorted[1].Status != "queued" {
		t.Errorf("sorted[1]: want queued, got %s", sorted[1].Status)
	}
	// Last two must be terminal.
	for i := 2; i < 4; i++ {
		s := sorted[i].Status
		if s != "completed" && s != "failed" {
			t.Errorf("sorted[%d]: want completed/failed, got %s", i, s)
		}
	}
}
