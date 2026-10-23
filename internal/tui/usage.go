package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"proxylm/internal/ipc"
)

// Pure aggregation helpers for the Usage overlay (key s). The daemon sends flat
// rows per (client, model, server); everything below pivots them for display.
// No I/O and no styling here, so the logic is table-testable.

// Usage overlay tabs.
const (
	usageTabModels = iota
	usageTabClients
	usageTabMatrix
	usageTabCount
)

// usageWindows — selectable windows in key order (1..4).
var usageWindows = []string{"1h", "24h", "7d", "all"}

// defaultUsageWindow — window used until the user picks another.
const defaultUsageWindow = "24h"

// usageRefreshInterval — how often stats are re-requested while the overlay is open.
const usageRefreshInterval = 10 * time.Second

// anonymousClient — label for rows with an empty client name.
const anonymousClient = "(anonymous)"

// noServerLabel — label for rows that were never dispatched to a server.
const noServerLabel = "—"

// usageAgg — aggregate for one model or one client.
type usageAgg struct {
	Name         string
	Requests     int64
	Failed       int64
	BusyMs       int64
	PromptTokens int64
	OutputTokens int64
	QueueMsAvg   int64 // requests-weighted average
	LastUsed     time.Time
	Servers      []string // distinct servers, requests desc
	Clients      []string // distinct clients, requests desc
	TopModels    []nameCount
}

// nameCount — a name with a request count.
type nameCount struct {
	Name  string
	Count int64
}

// clientLabel substitutes the anonymous label for an empty client name.
func clientLabel(c string) string {
	if c == "" {
		return anonymousClient
	}
	return c
}

// serverLabel substitutes the em-dash for an empty (never dispatched) server.
func serverLabel(s string) string {
	if s == "" {
		return noServerLabel
	}
	return s
}

// failPct returns failed/requests in percent (0 when there are no requests).
func failPct(failed, requests int64) float64 {
	if requests <= 0 {
		return 0
	}
	return float64(failed) * 100 / float64(requests)
}

// fmtFailPct formats a failure percentage: "0%", "1.2%", "12%".
func fmtFailPct(failed, requests int64) string {
	p := failPct(failed, requests)
	switch {
	case p == 0:
		return "0%"
	case p < 10:
		return fmt.Sprintf("%.1f%%", p)
	default:
		return fmt.Sprintf("%.0f%%", p)
	}
}

// humanizeCount formats a big number compactly: 950, 1.2k, 210k, 1.9M.
func humanizeCount(n int64) string {
	if n < 0 {
		n = 0
	}
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 10_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	case n < 1_000_000:
		return fmt.Sprintf("%dk", n/1000)
	case n < 10_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n < 1_000_000_000:
		return fmt.Sprintf("%dM", n/1_000_000)
	default:
		return fmt.Sprintf("%.1fG", float64(n)/1_000_000_000)
	}
}

// fmtTokInOut formats "in/out" token counts.
func fmtTokInOut(in, out int64) string {
	return humanizeCount(in) + "/" + humanizeCount(out)
}

// fmtBusy formats accumulated busy time: 120ms, 45s, 22m05s, 3h12m.
func fmtBusy(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", ms)
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// fmtAgo formats a relative time: "just now", "2m ago", "3h ago", "5d ago";
// "—" for a zero time.
func fmtAgo(now, t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := now.Sub(t)
	switch {
	case d < 5*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// usageAccum is the mutable accumulator behind usageAgg.
type usageAccum struct {
	agg      usageAgg
	queueSum float64 // sum(requests * queue_ms_avg)
	servers  map[string]int64
	clients  map[string]int64
	models   map[string]int64
}

func newAccum(name string) *usageAccum {
	return &usageAccum{
		agg:     usageAgg{Name: name},
		servers: map[string]int64{},
		clients: map[string]int64{},
		models:  map[string]int64{},
	}
}

func (a *usageAccum) add(r ipc.UsageRow) {
	a.agg.Requests += r.Requests
	a.agg.Failed += r.Failed
	a.agg.BusyMs += r.BusyMs
	a.agg.PromptTokens += r.PromptTokens
	a.agg.OutputTokens += r.OutputTokens
	a.queueSum += float64(r.Requests) * float64(r.QueueMsAvg)
	if r.LastUsed.After(a.agg.LastUsed) {
		a.agg.LastUsed = r.LastUsed
	}
	a.servers[serverLabel(r.Server)] += r.Requests
	a.clients[clientLabel(r.Client)] += r.Requests
	a.models[r.Model] += r.Requests
}

func (a *usageAccum) finish() usageAgg {
	if a.agg.Requests > 0 {
		a.agg.QueueMsAvg = int64(a.queueSum/float64(a.agg.Requests) + 0.5)
	}
	a.agg.Servers = sortedByCount(a.servers)
	a.agg.Clients = sortedByCount(a.clients)
	a.agg.TopModels = sortedCounts(a.models)
	return a.agg
}

// sortedCounts returns entries ordered by count desc, then name asc.
func sortedCounts(m map[string]int64) []nameCount {
	out := make([]nameCount, 0, len(m))
	for k, v := range m {
		out = append(out, nameCount{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func sortedByCount(m map[string]int64) []string {
	nc := sortedCounts(m)
	out := make([]string, len(nc))
	for i, x := range nc {
		out[i] = x.Name
	}
	return out
}

// pivotUsage groups rows by key (model or client) and returns aggregates sorted
// by requests desc, then name asc.
func pivotUsage(rows []ipc.UsageRow, keyOf func(ipc.UsageRow) string) []usageAgg {
	accs := map[string]*usageAccum{}
	for _, r := range rows {
		k := keyOf(r)
		a, ok := accs[k]
		if !ok {
			a = newAccum(k)
			accs[k] = a
		}
		a.add(r)
	}
	out := make([]usageAgg, 0, len(accs))
	for _, a := range accs {
		out = append(out, a.finish())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// pivotByModel — one aggregate per model.
func pivotByModel(rows []ipc.UsageRow) []usageAgg {
	return pivotUsage(rows, func(r ipc.UsageRow) string { return r.Model })
}

// pivotByClient — one aggregate per client (empty name becomes "(anonymous)").
func pivotByClient(rows []ipc.UsageRow) []usageAgg {
	return pivotUsage(rows, func(r ipc.UsageRow) string { return clientLabel(r.Client) })
}

// usageMatrix — model x client request counts. Models and Clients are ordered
// by total requests desc; Cells[i][j] is the count for Models[i], Clients[j].
type usageMatrix struct {
	Models    []string
	Clients   []string
	Cells     [][]int64
	RowTotals []int64
	ColTotals []int64
	Max       int64 // largest single cell
}

// buildMatrix pivots rows into a model x client matrix.
func buildMatrix(rows []ipc.UsageRow) usageMatrix {
	cell := map[[2]string]int64{}
	rowT := map[string]int64{}
	colT := map[string]int64{}
	for _, r := range rows {
		c := clientLabel(r.Client)
		cell[[2]string{r.Model, c}] += r.Requests
		rowT[r.Model] += r.Requests
		colT[c] += r.Requests
	}
	mx := usageMatrix{}
	for _, nc := range sortedCounts(rowT) {
		mx.Models = append(mx.Models, nc.Name)
		mx.RowTotals = append(mx.RowTotals, nc.Count)
	}
	for _, nc := range sortedCounts(colT) {
		mx.Clients = append(mx.Clients, nc.Name)
		mx.ColTotals = append(mx.ColTotals, nc.Count)
	}
	mx.Cells = make([][]int64, len(mx.Models))
	for i, m := range mx.Models {
		mx.Cells[i] = make([]int64, len(mx.Clients))
		for j, c := range mx.Clients {
			v := cell[[2]string{m, c}]
			mx.Cells[i][j] = v
			if v > mx.Max {
				mx.Max = v
			}
		}
	}
	return mx
}

// shadeLevel maps a cell count to 0..4 relative to max (0 only for count 0).
func shadeLevel(count, max int64) int {
	if count <= 0 || max <= 0 {
		return 0
	}
	l := int((count*4 + max - 1) / max) // ceil(4*count/max)
	if l < 1 {
		l = 1
	}
	if l > 4 {
		l = 4
	}
	return l
}

// shadeGlyph returns the glyph for a shade level (ASCII fallback aware).
func shadeGlyph(level int) string {
	switch level {
	case 0:
		return glyph("·", "-")
	case 1:
		return glyph("░", ".")
	case 2:
		return glyph("▒", ":")
	case 3:
		return glyph("▓", "*")
	default:
		return glyph("█", "#")
	}
}

// topModelsLabel renders "qwen 300 · llama 90 · +2" for up to n models.
func topModelsLabel(top []nameCount, n int) string {
	if len(top) == 0 {
		return ""
	}
	sep := " " + glyph("·", "|") + " "
	parts := make([]string, 0, n+1)
	for i, nc := range top {
		if i >= n {
			break
		}
		parts = append(parts, fmt.Sprintf("%s %d", nc.Name, nc.Count))
	}
	if extra := len(top) - n; extra > 0 {
		parts = append(parts, fmt.Sprintf("+%d", extra))
	}
	return strings.Join(parts, sep)
}

// usageWindowLabel — header label for a window.
func usageWindowLabel(window string, retentionDays int) string {
	if window == "all" {
		if retentionDays > 0 {
			return fmt.Sprintf("all time (retention %d days)", retentionDays)
		}
		return "all time"
	}
	return "last " + window
}

// usageEmptyText — message shown when the window has no rows.
func usageEmptyText(window string, retentionDays int) string {
	if window == "all" {
		if retentionDays > 0 {
			return fmt.Sprintf("No requests (history kept %d days)", retentionDays)
		}
		return "No requests (history kept forever)"
	}
	return "No requests in the last " + window
}

// fitList returns how many leading names fit into budget cells when joined by
// ", " (reserving room for a " +NN" suffix when truncated). At least one name
// is always kept.
func fitList(names []string, budget int) int {
	if len(names) == 0 {
		return 0
	}
	used := 0
	for i, n := range names {
		w := len([]rune(n))
		if i > 0 {
			w += 2
		}
		suffix := 0
		if i < len(names)-1 {
			suffix = 5
		}
		if used+w+suffix > budget && i > 0 {
			return i
		}
		used += w
	}
	return len(names)
}
