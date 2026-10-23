package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"proxylm/internal/ipc"
)

var usageNow = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func usageRows() []ipc.UsageRow {
	return []ipc.UsageRow{
		{Client: "crm", Model: "qwen", Server: "gpu-1", Requests: 300, Failed: 3, BusyMs: 3_000_000, QueueMsAvg: 100, PromptTokens: 1_000_000, OutputTokens: 100_000, LastUsed: usageNow.Add(-2 * time.Minute)},
		{Client: "bot", Model: "qwen", Server: "gpu-2", Requests: 100, Failed: 0, BusyMs: 1_000_000, QueueMsAvg: 500, PromptTokens: 900_000, OutputTokens: 110_000, LastUsed: usageNow.Add(-10 * time.Minute)},
		{Client: "", Model: "llama", Server: "", Requests: 50, Failed: 10, BusyMs: 50_000, QueueMsAvg: 0, PromptTokens: 500, OutputTokens: 0, LastUsed: usageNow.Add(-3 * time.Hour)},
		{Client: "crm", Model: "llama", Server: "gpu-1", Requests: 40, Failed: 2, BusyMs: 40_000, QueueMsAvg: 200, PromptTokens: 100, OutputTokens: 50, LastUsed: usageNow.Add(-1 * time.Hour)},
	}
}

func TestHumanizeCount(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{-5, "0"}, {0, "0"}, {999, "999"}, {1000, "1.0k"}, {1234, "1.2k"},
		{9999, "10.0k"}, {10_000, "10k"}, {210_000, "210k"}, {999_999, "999k"},
		{1_000_000, "1.0M"}, {1_900_000, "1.9M"}, {25_000_000, "25M"}, {3_000_000_000, "3.0G"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := humanizeCount(tt.in); got != tt.want {
				t.Errorf("humanizeCount(%d) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestFailPct(t *testing.T) {
	tests := []struct {
		failed, req int64
		want        string
	}{
		{0, 0, "0%"}, {0, 100, "0%"}, {3, 300, "1.0%"}, {1, 8, "12%"}, {10, 50, "20%"},
	}
	for _, tt := range tests {
		if got := fmtFailPct(tt.failed, tt.req); got != tt.want {
			t.Errorf("fmtFailPct(%d,%d) = %q, want %q", tt.failed, tt.req, got, tt.want)
		}
	}
}

func TestFmtBusyAndAgo(t *testing.T) {
	busy := []struct {
		ms   int64
		want string
	}{{500, "500ms"}, {45_000, "45s"}, {22*60_000 + 5_000, "22m05s"}, {3*3600_000 + 12*60_000, "3h12m"}}
	for _, tt := range busy {
		if got := fmtBusy(tt.ms); got != tt.want {
			t.Errorf("fmtBusy(%d) = %q, want %q", tt.ms, got, tt.want)
		}
	}
	ago := []struct {
		d    time.Duration
		want string
	}{{time.Second, "just now"}, {30 * time.Second, "30s ago"}, {2 * time.Minute, "2m ago"}, {3 * time.Hour, "3h ago"}, {72 * time.Hour, "3d ago"}}
	for _, tt := range ago {
		if got := fmtAgo(usageNow, usageNow.Add(-tt.d)); got != tt.want {
			t.Errorf("fmtAgo(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
	if got := fmtAgo(usageNow, time.Time{}); got != "—" {
		t.Errorf("zero time = %q", got)
	}
}

func TestPivotByModel(t *testing.T) {
	aggs := pivotByModel(usageRows())
	if len(aggs) != 2 {
		t.Fatalf("len = %d, want 2", len(aggs))
	}
	q, l := aggs[0], aggs[1]
	if q.Name != "qwen" || l.Name != "llama" {
		t.Fatalf("order = %s,%s; want qwen,llama (requests desc)", q.Name, l.Name)
	}
	if q.Requests != 400 || q.Failed != 3 || q.BusyMs != 4_000_000 {
		t.Errorf("qwen sums wrong: %+v", q)
	}
	// weighted queue avg: (300*100 + 100*500)/400 = 200
	if q.QueueMsAvg != 200 {
		t.Errorf("qwen queue avg = %d, want 200", q.QueueMsAvg)
	}
	if q.PromptTokens != 1_900_000 || q.OutputTokens != 210_000 {
		t.Errorf("qwen tokens = %d/%d", q.PromptTokens, q.OutputTokens)
	}
	if !q.LastUsed.Equal(usageNow.Add(-2 * time.Minute)) {
		t.Errorf("qwen last used = %v", q.LastUsed)
	}
	if strings.Join(q.Servers, ",") != "gpu-1,gpu-2" || strings.Join(q.Clients, ",") != "crm,bot" {
		t.Errorf("qwen servers/clients = %v / %v", q.Servers, q.Clients)
	}
	// llama: never-dispatched server shown as em-dash, anonymous client.
	if strings.Join(l.Servers, ",") != "—,gpu-1" {
		t.Errorf("llama servers = %v", l.Servers)
	}
	if strings.Join(l.Clients, ",") != "(anonymous),crm" {
		t.Errorf("llama clients = %v", l.Clients)
	}
	// (50*0 + 40*200)/90 = 88.9 -> 89
	if l.QueueMsAvg != 89 {
		t.Errorf("llama queue avg = %d, want 89", l.QueueMsAvg)
	}
}

func TestPivotByClient(t *testing.T) {
	aggs := pivotByClient(usageRows())
	if len(aggs) != 3 {
		t.Fatalf("len = %d, want 3", len(aggs))
	}
	if aggs[0].Name != "crm" || aggs[0].Requests != 340 {
		t.Errorf("first = %+v", aggs[0])
	}
	if aggs[2].Name != anonymousClient {
		t.Errorf("last = %q, want anonymous", aggs[2].Name)
	}
	if got := topModelsLabel(aggs[0].TopModels, 3); got != "qwen 300 "+glyph("·", "|")+" llama 40" {
		t.Errorf("top models = %q", got)
	}
	many := []nameCount{{"a", 5}, {"b", 4}, {"c", 3}, {"d", 2}, {"e", 1}}
	if got := topModelsLabel(many, 3); !strings.HasSuffix(got, "+2") {
		t.Errorf("top models overflow = %q", got)
	}
}

func TestPivotEmpty(t *testing.T) {
	if got := pivotByModel(nil); len(got) != 0 {
		t.Errorf("pivotByModel(nil) = %v", got)
	}
	if got := pivotByClient(nil); len(got) != 0 {
		t.Errorf("pivotByClient(nil) = %v", got)
	}
	mx := buildMatrix(nil)
	if len(mx.Models) != 0 || len(mx.Clients) != 0 || mx.Max != 0 {
		t.Errorf("empty matrix = %+v", mx)
	}
}

func TestBuildMatrix(t *testing.T) {
	mx := buildMatrix(usageRows())
	if strings.Join(mx.Models, ",") != "qwen,llama" {
		t.Fatalf("models = %v", mx.Models)
	}
	if strings.Join(mx.Clients, ",") != "crm,bot,(anonymous)" {
		t.Fatalf("clients = %v", mx.Clients)
	}
	if mx.Cells[0][0] != 300 || mx.Cells[0][1] != 100 || mx.Cells[0][2] != 0 || mx.Cells[1][0] != 40 || mx.Cells[1][2] != 50 {
		t.Errorf("cells = %v", mx.Cells)
	}
	if mx.RowTotals[0] != 400 || mx.RowTotals[1] != 90 || mx.ColTotals[0] != 340 || mx.ColTotals[2] != 50 {
		t.Errorf("totals = %v / %v", mx.RowTotals, mx.ColTotals)
	}
	if mx.Max != 300 {
		t.Errorf("max = %d", mx.Max)
	}
}

func TestShadeLevel(t *testing.T) {
	tests := []struct {
		c, max int64
		want   int
	}{{0, 100, 0}, {1, 100, 1}, {25, 100, 1}, {26, 100, 2}, {50, 100, 2}, {75, 100, 3}, {76, 100, 4}, {100, 100, 4}, {5, 0, 0}}
	for _, tt := range tests {
		if got := shadeLevel(tt.c, tt.max); got != tt.want {
			t.Errorf("shadeLevel(%d,%d) = %d, want %d", tt.c, tt.max, got, tt.want)
		}
	}
}

func TestUsageLabels(t *testing.T) {
	tests := []struct {
		name         string
		window       string
		retention    int
		wantLabel    string
		wantEmptyMsg string
	}{
		{"24h", "24h", 0, "last 24h", "No requests in the last 24h"},
		{"1h", "1h", 30, "last 1h", "No requests in the last 1h"},
		{"all retention", "all", 30, "all time (retention 30 days)", "No requests (history kept 30 days)"},
		{"all forever", "all", 0, "all time", "No requests (history kept forever)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := usageWindowLabel(tt.window, tt.retention); got != tt.wantLabel {
				t.Errorf("label = %q, want %q", got, tt.wantLabel)
			}
			if got := usageEmptyText(tt.window, tt.retention); got != tt.wantEmptyMsg {
				t.Errorf("empty = %q, want %q", got, tt.wantEmptyMsg)
			}
		})
	}
}

func TestFitList(t *testing.T) {
	names := []string{"alpha", "bravo", "charlie", "delta"}
	tests := []struct {
		budget int
		want   int
	}{{100, 4}, {1, 1}, {0, 1}, {20, 2}}
	for _, tt := range tests {
		if got := fitList(names, tt.budget); got != tt.want {
			t.Errorf("fitList(budget=%d) = %d, want %d", tt.budget, got, tt.want)
		}
	}
	if fitList(nil, 10) != 0 {
		t.Error("fitList(nil) != 0")
	}
}

// ---- render ----

func renderedWidthOK(t *testing.T, out string, w int) {
	t.Helper()
	for i, line := range strings.Split(out, "\n") {
		if lw := lipgloss.Width(line); lw > w {
			t.Errorf("line %d width %d > %d: %q", i, lw, w, line)
		}
	}
}

func usageTestView(tab int, rows []ipc.UsageRow, window string) usageView {
	return usageView{
		Data: &ipc.UsageStatsPayload{
			Window: window, GeneratedAt: usageNow.Add(-3 * time.Second), RetentionDays: 30, Rows: rows,
		},
		Window:    window,
		Tab:       tab,
		ServerIdx: map[string]int{"gpu-1": 0, "gpu-2": 1},
		Now:       usageNow,
	}
}

func TestRenderUsageOverlay(t *testing.T) {
	tests := []struct {
		name     string
		view     usageView
		w, h     int
		contains []string
	}{
		{"loading", usageView{Window: "24h", Now: usageNow}, 100, 30, []string{"Usage", "last 24h", "Loading"}},
		{"empty 24h", usageTestView(usageTabModels, []ipc.UsageRow{}, "24h"), 100, 30, []string{"No requests in the last 24h"}},
		{"empty all", usageTestView(usageTabModels, nil, "all"), 100, 30, []string{"No requests (history kept 30 days)", "all time (retention 30 days)"}},
		{"error", func() usageView {
			v := usageTestView(usageTabModels, nil, "24h")
			v.Data.Error = "db locked"
			return v
		}(), 100, 30, []string{"Error: db locked"}},
		{"models", usageTestView(usageTabModels, usageRows(), "24h"), 100, 30,
			[]string{"MODEL", "qwen", "llama", "400", "0.8%", "13%", "1.9M/210k", "2m ago", "servers: gpu-1, gpu-2", "clients: crm, bot", "updated 3s ago", "[2]24h"}},
		{"clients", usageTestView(usageTabClients, usageRows(), "24h"), 100, 30,
			[]string{"CLIENT", "crm", "(anonymous)", "TOP MODELS", "qwen 300"}},
		{"matrix", usageTestView(usageTabMatrix, usageRows(), "24h"), 100, 30,
			[]string{"TOTAL", "crm", "bot", "300", "490"}},
		{"models 80 cols", usageTestView(usageTabModels, usageRows(), "24h"), 80, 24,
			[]string{"MODEL", "qwen", "TOK IN/OUT"}},
		{"matrix 80 cols many clients", func() usageView {
			var rows []ipc.UsageRow
			for i := 0; i < 15; i++ {
				rows = append(rows, ipc.UsageRow{Client: "client-" + string(rune('a'+i)), Model: "m", Requests: int64(i + 1)})
			}
			return usageTestView(usageTabMatrix, rows, "24h")
		}(), 80, 24, []string{"more clients"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := renderUsageOverlay(tt.view, tt.w, tt.h)
			for _, want := range tt.contains {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q:\n%s", want, out)
				}
			}
			renderedWidthOK(t, out, tt.w)
			if n := len(strings.Split(out, "\n")); n > tt.h {
				t.Errorf("height %d > %d", n, tt.h)
			}
		})
	}
}

func TestRenderUsageOverlayScroll(t *testing.T) {
	var rows []ipc.UsageRow
	for i := 0; i < 30; i++ {
		rows = append(rows, ipc.UsageRow{Client: "c", Model: "model-" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Requests: int64(100 - i)})
	}
	v := usageTestView(usageTabModels, rows, "24h")
	top := renderUsageOverlay(v, 100, 20)
	if !strings.Contains(top, "model-aa") {
		t.Errorf("top view wrong:\n%s", top)
	}
	if !strings.Contains(top, "lines 1-") {
		t.Errorf("missing scroll indicator:\n%s", top)
	}
	v.Scroll = 10000 // clamped
	bottom := renderUsageOverlay(v, 100, 20)
	if strings.Contains(bottom, "model-aa") {
		t.Errorf("bottom view still shows first row:\n%s", bottom)
	}
	if !strings.Contains(bottom, "of 60") {
		t.Errorf("bottom view lacks line total:\n%s", bottom)
	}
	if got := usageLineCount(v, 100); got != 60 {
		t.Errorf("usageLineCount = %d, want 60", got)
	}
}

func TestRenderUsageOverlayNoUnicode(t *testing.T) {
	old := noUnicodeFlag
	noUnicodeFlag = true
	t.Cleanup(func() { noUnicodeFlag = old })

	for _, tab := range []int{usageTabModels, usageTabClients, usageTabMatrix} {
		out := renderUsageOverlay(usageTestView(tab, usageRows(), "24h"), 100, 30)
		for _, r := range out {
			// Border (rounded box-drawing) and the "—" placeholder are allowed;
			// glyphs from styles.go must degrade to ASCII.
			if strings.ContainsRune("·░▒▓█←→↑↓", r) {
				t.Errorf("tab %d: unicode glyph %q in no-unicode output:\n%s", tab, r, out)
				break
			}
		}
	}
	if got := shadeGlyph(4); got != "#" {
		t.Errorf("shadeGlyph(4) = %q", got)
	}
}

func TestUsageKeyFlow(t *testing.T) {
	m := &model{width: 100, height: 30}
	press := func(s string) {
		t.Helper()
		var k tea.KeyMsg
		switch s {
		case "esc":
			k = tea.KeyMsg{Type: tea.KeyEsc}
		case "right":
			k = tea.KeyMsg{Type: tea.KeyRight}
		default:
			k = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
		}
		_, _ = m.handleKey(k)
	}
	press("s")
	if !m.showUsage || m.usageGen != 1 {
		t.Fatalf("open: show=%v gen=%d", m.showUsage, m.usageGen)
	}
	press("3")
	if m.usageWindow != "7d" {
		t.Errorf("window = %q, want 7d", m.usageWindow)
	}
	press("right")
	if m.usageTab != usageTabClients {
		t.Errorf("tab = %d", m.usageTab)
	}
	// stale tick is ignored, current tick continues
	if _, cmd := m.Update(usageTickMsg{gen: 0}); cmd != nil {
		t.Error("stale tick must not reschedule")
	}
	if _, cmd := m.Update(usageTickMsg{gen: m.usageGen}); cmd == nil {
		t.Error("current tick must reschedule")
	}
	press("esc")
	if m.showUsage {
		t.Fatal("overlay not closed")
	}
	if _, cmd := m.Update(usageTickMsg{gen: m.usageGen - 1}); cmd != nil {
		t.Error("tick after close must stop")
	}
	// window is remembered on reopen
	press("s")
	if m.usageWindowOrDefault() != "7d" {
		t.Errorf("window not remembered: %q", m.usageWindowOrDefault())
	}
}
