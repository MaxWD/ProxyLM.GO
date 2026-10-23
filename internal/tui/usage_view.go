package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"proxylm/internal/ipc"
)

// ---- Usage overlay rendering ------------------------------------------------

// usageChromeLines — fixed lines of the overlay besides the scrollable body:
// title, tabs, column header, two footer lines, plus top/bottom border.
const usageChromeLines = 7

// usageFailWarnPct — fail rate above which a row is rendered in the failed style.
const usageFailWarnPct = 5.0

// usageView is everything the pure renderer needs.
type usageView struct {
	Data      *ipc.UsageStatsPayload // nil until the first usage_stats arrives
	Window    string
	Tab       int
	Scroll    int
	ServerIdx map[string]int // server name -> palette index (from the latest snapshot)
	Now       time.Time
}

// usageBodyHeight — number of scrollable body lines for terminal height termH.
func usageBodyHeight(termH int) int {
	h := termH - usageChromeLines
	if h < 3 {
		h = 3
	}
	return h
}

// usageInnerWidth — usable width inside the border.
func usageInnerWidth(termW int) int {
	w := termW - 4
	if w < 40 {
		w = 40
	}
	return w
}

// cellL renders a left-aligned styled cell of width w (truncating).
func cellL(text string, st lipgloss.Style, w int) string {
	return padDisplay(st.Render(truncStr(text, w)), w)
}

// cellR renders a right-aligned styled cell of width w (truncating).
func cellR(text string, st lipgloss.Style, w int) string {
	t := truncStr(text, w)
	return padLeftDisplay(st.Render(t), len([]rune(t)), w)
}

// usageColumns — fixed column widths shared by the tables.
const (
	ucReq   = 6
	ucFail  = 6
	ucBusy  = 8
	ucAvg   = 8
	ucQueue = 7
	ucTok   = 11
	ucLast  = 9
)

// usageTableLayout returns the column header and the body lines for the active tab.
func usageTable(v usageView, inner int) (header string, body []string) {
	if v.Data == nil {
		return "", []string{StyleDim.Render("Loading…")}
	}
	if v.Data.Error != "" {
		return "", []string{StyleError.Render("Error: " + v.Data.Error)}
	}
	if len(v.Data.Rows) == 0 {
		return "", []string{StyleDim.Render(usageEmptyText(v.Window, v.Data.RetentionDays))}
	}
	switch v.Tab {
	case usageTabClients:
		return usageClientsTable(v, inner)
	case usageTabMatrix:
		return usageMatrixTable(v, inner)
	default:
		return usageModelsTable(v, inner)
	}
}

func failStyle(failed, requests int64) lipgloss.Style {
	if failPct(failed, requests) > usageFailWarnPct {
		return StyleStatusFailed
	}
	return lipgloss.NewStyle()
}

var plain = lipgloss.NewStyle()

func usageModelsTable(v usageView, inner int) (string, []string) {
	aggs := pivotByModel(v.Data.Rows)
	nameW := inner - (ucReq + ucFail + ucBusy + ucAvg + ucQueue + ucTok + ucLast) - 7
	if nameW < 8 {
		nameW = 8
	}
	header := StyleColHeader.Render(strings.Join([]string{
		padDisplay("MODEL", nameW),
		cellR("REQ", plain, ucReq),
		cellR("FAIL", plain, ucFail),
		cellR("BUSY", plain, ucBusy),
		cellR("AVG RUN", plain, ucAvg),
		cellR("QUEUE", plain, ucQueue),
		cellR("TOK IN/OUT", plain, ucTok),
		cellR("LAST", plain, ucLast),
	}, " "))
	var body []string
	for _, a := range aggs {
		avgRun := int64(0)
		if a.Requests > 0 {
			avgRun = a.BusyMs / a.Requests
		}
		body = append(body, strings.Join([]string{
			cellL(a.Name, StyleHeader, nameW),
			cellR(fmt.Sprintf("%d", a.Requests), plain, ucReq),
			cellR(fmtFailPct(a.Failed, a.Requests), failStyle(a.Failed, a.Requests), ucFail),
			cellR(fmtBusy(a.BusyMs), plain, ucBusy),
			cellR(fmtMs(avgRun), plain, ucAvg),
			cellR(fmtMs(a.QueueMsAvg), plain, ucQueue),
			cellR(fmtTokInOut(a.PromptTokens, a.OutputTokens), plain, ucTok),
			cellR(fmtAgo(v.Now, a.LastUsed), StyleDim, ucLast),
		}, " "))
		body = append(body, usageDetailLine(a, v.ServerIdx, inner))
	}
	return header, body
}

// usageDetailLine renders the dim "servers: … · clients: …" line under a model.
func usageDetailLine(a usageAgg, serverIdx map[string]int, inner int) string {
	const prefix = "  servers: "
	sep := " " + glyph("·", "|") + " clients: "
	rem := inner - len(prefix) - len([]rune(sep)) - 2
	if rem < 8 {
		rem = 8
	}
	ns := fitList(a.Servers, rem/2)
	var sb strings.Builder
	sb.WriteString(StyleDim.Render(prefix))
	used := 0
	for i := 0; i < ns; i++ {
		if i > 0 {
			sb.WriteString(StyleDim.Render(", "))
			used += 2
		}
		name := a.Servers[i]
		if name == noServerLabel {
			sb.WriteString(StyleDim.Render(name))
		} else {
			sb.WriteString(serverColorFor(name, serverIdx).Render(name))
		}
		used += len([]rune(name))
	}
	if extra := len(a.Servers) - ns; extra > 0 {
		s := fmt.Sprintf(" +%d", extra)
		sb.WriteString(StyleDim.Render(s))
		used += len(s)
	}
	nc := fitList(a.Clients, rem-used)
	clients := strings.Join(a.Clients[:nc], ", ")
	if extra := len(a.Clients) - nc; extra > 0 {
		clients += fmt.Sprintf(" +%d", extra)
	}
	sb.WriteString(StyleDim.Render(sep + clients))
	return sb.String()
}

func usageClientsTable(v usageView, inner int) (string, []string) {
	aggs := pivotByClient(v.Data.Rows)
	rem := inner - (ucReq + ucFail + ucBusy + ucTok + ucLast) - 6
	nameW := rem * 35 / 100
	if nameW < 10 {
		nameW = 10
	}
	topW := rem - nameW
	if topW < 10 {
		topW = 10
	}
	header := StyleColHeader.Render(strings.Join([]string{
		padDisplay("CLIENT", nameW),
		cellR("REQ", plain, ucReq),
		cellR("FAIL", plain, ucFail),
		cellR("BUSY", plain, ucBusy),
		cellR("TOK IN/OUT", plain, ucTok),
		padDisplay("TOP MODELS", topW),
		cellR("LAST", plain, ucLast),
	}, " "))
	var body []string
	for _, a := range aggs {
		nameSt := StyleHeader
		if a.Name == anonymousClient {
			nameSt = StyleDim
		}
		body = append(body, strings.Join([]string{
			cellL(a.Name, nameSt, nameW),
			cellR(fmt.Sprintf("%d", a.Requests), plain, ucReq),
			cellR(fmtFailPct(a.Failed, a.Requests), failStyle(a.Failed, a.Requests), ucFail),
			cellR(fmtBusy(a.BusyMs), plain, ucBusy),
			cellR(fmtTokInOut(a.PromptTokens, a.OutputTokens), plain, ucTok),
			cellL(topModelsLabel(a.TopModels, 3), StyleDim, topW),
			cellR(fmtAgo(v.Now, a.LastUsed), StyleDim, ucLast),
		}, " "))
	}
	return header, body
}

// matrixLevelStyles — style per shade level 0..4.
var matrixLevelStyles = [5]lipgloss.Style{
	StyleDim,
	StyleDim,
	StyleStatusQueued,
	lipgloss.NewStyle().Foreground(colorCyan),
	StyleStatusRunning,
}

const (
	matrixColW   = 8
	matrixTotalW = 7
)

func usageMatrixTable(v usageView, inner int) (string, []string) {
	mx := buildMatrix(v.Data.Rows)
	nameW := 8
	for _, m := range mx.Models {
		if l := len([]rune(m)); l > nameW {
			nameW = l
		}
	}
	if nameW > 24 {
		nameW = 24
	}
	if nameW > inner/3 {
		nameW = inner / 3
	}
	avail := inner - nameW - 1 - (matrixTotalW + 1)
	nCols := avail / (matrixColW + 1)
	if nCols < 1 {
		nCols = 1
	}
	if nCols > len(mx.Clients) {
		nCols = len(mx.Clients)
	}
	hidden := len(mx.Clients) - nCols

	hdr := []string{padDisplay("MODEL", nameW)}
	for j := 0; j < nCols; j++ {
		hdr = append(hdr, cellR(mx.Clients[j], plain, matrixColW))
	}
	hdr = append(hdr, cellR("TOTAL", plain, matrixTotalW))
	header := StyleColHeader.Render(strings.Join(hdr, " "))

	var body []string
	for i, m := range mx.Models {
		parts := []string{cellL(m, StyleHeader, nameW)}
		for j := 0; j < nCols; j++ {
			c := mx.Cells[i][j]
			lvl := shadeLevel(c, mx.Max)
			parts = append(parts, matrixCell(c, lvl))
		}
		parts = append(parts, cellR(humanizeCount(mx.RowTotals[i]), plain, matrixTotalW))
		body = append(body, strings.Join(parts, " "))
	}
	totals := []string{cellL("TOTAL", StyleColHeader, nameW)}
	var grand int64
	for _, t := range mx.ColTotals {
		grand += t
	}
	for j := 0; j < nCols; j++ {
		totals = append(totals, cellR(humanizeCount(mx.ColTotals[j]), StyleColHeader, matrixColW))
	}
	totals = append(totals, cellR(humanizeCount(grand), StyleColHeader, matrixTotalW))
	body = append(body, strings.Join(totals, " "))
	if hidden > 0 {
		body = append(body, StyleDim.Render(fmt.Sprintf("+%d more clients (widen the terminal)", hidden)))
	}
	return header, body
}

// matrixCell renders one matrix cell: shade glyph + right-aligned count.
func matrixCell(count int64, level int) string {
	st := matrixLevelStyles[level]
	txt := "0"
	if count > 0 {
		txt = humanizeCount(count)
	}
	g := shadeGlyph(level)
	w := matrixColW - 2
	fill := w - len([]rune(txt))
	if fill < 0 {
		fill = 0
	}
	return st.Render(g + " " + strings.Repeat(" ", fill) + txt)
}

// renderUsageOverlay renders the full-screen Usage overlay.
func renderUsageOverlay(v usageView, termW, termH int) string {
	if termW <= 0 {
		termW = 80
	}
	if termH <= 0 {
		termH = 24
	}
	inner := usageInnerWidth(termW)
	bodyH := usageBodyHeight(termH)

	// Title line: label left, window selector right.
	dot := glyph("·", "-")
	left := StyleModalTitle.Render("Usage " + dot + " " + usageWindowLabel(v.Window, retentionOf(v.Data)))
	var wins []string
	for i, w := range usageWindows {
		label := fmt.Sprintf("[%d]%s", i+1, w)
		if w == v.Window {
			wins = append(wins, StyleHelpKey.Render(label))
		} else {
			wins = append(wins, StyleDim.Render(label))
		}
	}
	right := strings.Join(wins, " ")
	gap := inner - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	title := left + strings.Repeat(" ", gap) + right

	// Tabs.
	names := []string{"Models", "Clients", "Matrix"}
	var tabs []string
	for i, n := range names {
		if i == v.Tab {
			tabs = append(tabs, StyleSelected.Render(" "+n+" "))
		} else {
			tabs = append(tabs, StyleDim.Render(" "+n+" "))
		}
	}
	tabLine := strings.Join(tabs, " ") + "  " + StyleDim.Render("("+glyph("←/→", "</>")+")")

	header, lines := usageTable(v, inner)

	// Scroll window.
	maxScroll := len(lines) - bodyH
	if maxScroll < 0 {
		maxScroll = 0
	}
	scroll := v.Scroll
	if scroll > maxScroll {
		scroll = maxScroll
	}
	if scroll < 0 {
		scroll = 0
	}
	end := scroll + bodyH
	if end > len(lines) {
		end = len(lines)
	}
	visible := append([]string(nil), lines[scroll:end]...)
	for len(visible) < bodyH {
		visible = append(visible, "")
	}

	hints := fmt.Sprintf("s/Esc close %s 1-4 window %s %s tab %s %s scroll %s refresh %ds",
		dot, dot, glyph("←/→", "</>"), dot, glyph("↑↓", "up/dn"), dot, int(usageRefreshInterval/time.Second))
	status := "waiting for data"
	if v.Data != nil && !v.Data.GeneratedAt.IsZero() {
		age := v.Now.Sub(v.Data.GeneratedAt)
		if age < 0 {
			age = 0
		}
		status = fmt.Sprintf("updated %ds ago", int(age.Seconds()))
	}
	if len(lines) > bodyH {
		status += fmt.Sprintf(" %s lines %d-%d of %d", dot, scroll+1, end, len(lines))
	}

	clip := func(s string) string {
		if lipgloss.Width(s) > inner {
			return truncStr(s, inner)
		}
		return s
	}
	rows := []string{title, tabLine, header}
	rows = append(rows, visible...)
	rows = append(rows, StyleDim.Render(clip(hints)), StyleDim.Render(clip(status)))

	return StyleBorderActive.Width(termW - 2).Height(termH - 2).Render(strings.Join(rows, "\n"))
}

func retentionOf(d *ipc.UsageStatsPayload) int {
	if d == nil {
		return 0
	}
	return d.RetentionDays
}

// usageLineCount returns the number of scrollable body lines (for scroll clamping).
func usageLineCount(v usageView, termW int) int {
	_, lines := usageTable(v, usageInnerWidth(termW))
	return len(lines)
}
