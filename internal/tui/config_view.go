package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"proxylm/internal/config"
	"proxylm/internal/ipc"
)

// ---- Config overlay rendering ----------------------------------------------------

// cfgChromeLines — fixed lines besides the body: title, status, prompt, hints
// plus the top/bottom border.
const cfgChromeLines = 6

const (
	cfgLeftW   = 18 // nav column width
	cfgLabelW  = 20 // minimum field label column
	cfgNarrowW = 72 // inner width below which the nav collapses to a tab bar
)

var styleBold = lipgloss.NewStyle().Bold(true)

// cfgView is everything the pure renderer needs.
type cfgView struct {
	UI             *cfgUI
	RestartPending []string
	Servers        []ipc.ServerState // latest state_snapshot (backend health)
}

func cfgBodyHeight(termH int) int {
	h := termH - cfgChromeLines
	if h < 5 {
		h = 5
	}
	return h
}

func restartGlyph() string { return glyph("⟳", "R") }
func dotGlyph() string     { return glyph("●", "*") }
func rule(w int) string    { return StyleDim.Render(strings.Repeat(glyph("─", "-"), max(w, 0))) }

// cfgSectionDesc is the one-line description shown under the section title.
var cfgSectionDesc = map[string]string{
	"proxy":     "Listen address, port and logging.",
	"admin":     "Admin key and admin access.",
	"clients":   "Clients allowed to call the proxy (API keys).",
	"backends":  "LLM servers the proxy routes requests to.",
	"routing":   "How requests are spread across backends.",
	"scheduler": "Queueing and model-switching behaviour.",
	"retry":     "How failed backend calls are retried.",
	"discovery": "Automatic health checks and model discovery.",
	"storage":   "Request history database and retention.",
	"tui":       "Terminal UI display settings.",
	"compat":    "Protocol compatibility switches.",
}

func renderConfigOverlay(v cfgView, termW, termH int) string {
	if termW <= 0 {
		termW = 80
	}
	if termH <= 0 {
		termH = 24
	}
	inner := usageInnerWidth(termW)
	bodyH := cfgBodyHeight(termH)
	u := v.UI

	clip := func(s string) string {
		if lipgloss.Width(s) > inner {
			return truncStr(s, inner)
		}
		return s
	}

	var body []string
	switch {
	case !u.loaded && u.loadErr != "":
		body = []string{StyleError.Render("Error: " + u.loadErr)}
	case !u.loaded:
		body = []string{StyleDim.Render("Loading…")}
	case u.mode == cmReview:
		body = cfgReviewBody(u, inner)
	case u.mode == cmGenerated:
		body = cfgGeneratedBody(u, inner, bodyH)
	case inner < cfgNarrowW:
		body = append([]string{cfgTabBar(u, inner)}, cfgPane(u, v, inner, bodyH-1, false)...)
	default:
		body = cfgSplitBody(u, v, inner, bodyH)
	}
	body = fitLines(body, bodyH)

	status, prompt, hints := cfgFooter(v, inner)
	rows := []string{cfgTitle(u, inner)}
	rows = append(rows, body...)
	rows = append(rows, clip(status), clip(prompt), hints)
	return StyleBorderActive.Width(termW - 2).Height(termH - 2).Render(strings.Join(rows, "\n"))
}

func cfgTitle(u *cfgUI, inner int) string {
	left := "Configuration"
	right := ""
	if u.loaded && u.draft != nil {
		if n := len(u.draft.ChangeRows()); n > 0 {
			right = StyleFlash.Render(fmt.Sprintf("%s %d unsaved", dotGlyph(), n))
		}
	}
	if u.revision != "" {
		rev := u.revision
		if len(rev) > 7 {
			rev = rev[:7]
		}
		if right != "" {
			right += StyleDim.Render(" " + glyph("·", "-") + " ")
		}
		right += StyleDim.Render("rev " + rev)
	}
	pathW := inner - lipgloss.Width(right) - lipgloss.Width(left) - 3
	path := ""
	if u.path != "" && pathW >= 6 {
		path = "  " + StyleDim.Render(truncStr(u.path, pathW))
	}
	gap := inner - lipgloss.Width(left) - lipgloss.Width(path) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return StyleModalTitle.Render(left) + path + strings.Repeat(" ", gap) + right
}

// fitLines pads or truncates lines to exactly n.
func fitLines(lines []string, n int) []string {
	if len(lines) > n {
		return lines[:n]
	}
	for len(lines) < n {
		lines = append(lines, "")
	}
	return lines
}

// ---- review / generated -------------------------------------------------------------

func cfgReviewBody(u *cfgUI, inner int) []string {
	lines := []string{StyleHelpKey.Render("Review changes") + StyleDim.Render(fmt.Sprintf(" (%d)", max(len(u.reviewRows), len(u.review))))}
	if len(u.reviewRows) == 0 {
		for _, c := range u.review {
			lines = append(lines, "  "+truncStr(c, inner-2))
		}
		return lines
	}
	applyW := 8
	pathW := 12
	for _, r := range u.reviewRows {
		pathW = max(pathW, lipgloss.Width(r.Path))
	}
	pathW = min(pathW, max(inner*40/100, 12))
	chW := max(inner-pathW-applyW-4, 8)
	lines = append(lines, StyleDim.Render(padDisplay(" Path", pathW+1)+" "+padDisplay("Change", chW)+" "+"Apply"))
	for _, r := range u.reviewRows {
		var ch string
		switch {
		case r.Note == "renamed":
			ch = r.Old + " " + glyph("→", "->") + " " + r.New
		case r.Note != "":
			ch = r.Note
		case r.New == "changed":
			ch = "changed"
		default:
			ch = r.Old + " " + glyph("→", "->") + " " + r.New
		}
		apply := StyleDim.Render(glyph("–", "-"))
		switch r.Apply {
		case "restart":
			apply = StyleFlash.Render("restart")
		case "hot":
			apply = StyleServerHealthy.Render("live")
		}
		lines = append(lines, " "+padDisplay(r.Path, pathW)+" "+padDisplay(ch, chW)+" "+apply)
	}
	return lines
}

func asciiBorder() lipgloss.Border {
	if noUnicode() {
		return lipgloss.Border{Top: "-", Bottom: "-", Left: "|", Right: "|", TopLeft: "+", TopRight: "+", BottomLeft: "+", BottomRight: "+"}
	}
	return lipgloss.RoundedBorder()
}

func cfgGeneratedBody(u *cfgUI, inner, bodyH int) []string {
	lines := []string{StyleFlash.Render("Generated keys"), ""}
	for _, g := range u.generated {
		lines = append(lines, StyleDim.Render(g.Path), StyleHelpKey.Render(g.Key), "")
	}
	lines = append(lines,
		"Copy it now "+glyph("—", "-")+" press v later to reveal again.",
		StyleHelpKey.Render("y")+StyleDim.Render(" copy  ")+StyleHelpKey.Render("Enter")+StyleDim.Render(" close"))
	box := lipgloss.NewStyle().Border(asciiBorder()).Padding(0, 2).Render(strings.Join(lines, "\n"))
	return strings.Split(lipgloss.Place(inner, bodyH, lipgloss.Center, lipgloss.Center, box), "\n")
}

// ---- nav --------------------------------------------------------------------------

type secBadge struct {
	g     string
	style lipgloss.Style
}

// sectionState reports modified / validation-error / restart-class-modified.
func sectionState(u *cfgUI, sec config.Section) (mod, bad, rst bool) {
	fields := u.sectionFields(sec)
	if sec.List == "" {
		for _, f := range fields {
			if u.draft.Modified(f, -1) {
				mod = true
				rst = rst || string(f.Apply) == "restart"
			}
			if u.fieldError(f, -1) != "" {
				bad = true
			}
		}
		return
	}
	n := u.draft.itemCount(sec.List)
	if n != len(listItems(u.draft.orig, sec.List)) {
		mod = true
	}
	for i := 0; i < n; i++ {
		if u.draft.ItemState(sec.List, i) != "" {
			mod = true
		}
		if u.itemHasError(sec.List, i) {
			bad = true
		}
		for _, f := range fields {
			if u.draft.Modified(f, i) && string(f.Apply) == "restart" {
				rst = true
			}
		}
	}
	return
}

func secBadges(u *cfgUI, sec config.Section) []secBadge {
	mod, bad, rst := sectionState(u, sec)
	var out []secBadge
	if bad {
		out = append(out, secBadge{"!", StyleError})
	}
	if mod {
		out = append(out, secBadge{dotGlyph(), StyleFlash})
	}
	if rst {
		out = append(out, secBadge{restartGlyph(), StyleFlash})
	}
	return out
}

func groupOf(id string) string {
	for _, g := range cfgGroups {
		for _, x := range g.IDs {
			if x == id {
				return g.Name
			}
		}
	}
	return "Other"
}

// cfgNavLines renders the grouped section column and returns the line of the
// current section.
func cfgNavLines(u *cfgUI) ([]string, int) {
	var lines []string
	cur := 0
	lastGroup := ""
	for i, s := range u.sections() {
		indent := ""
		label := s.Label
		if s.List != "" {
			label = fmt.Sprintf("%s (%d)", strings.ToUpper(s.Label), u.draft.itemCount(s.List))
			lastGroup = ""
		} else {
			if g := groupOf(s.ID); g != lastGroup {
				lines = append(lines, StyleDim.Render(padDisplay("  "+strings.ToUpper(g), cfgLeftW)))
				lastGroup = g
			}
			indent = "  "
		}
		if i == u.secIdx {
			cur = len(lines)
		}
		lines = append(lines, navCell(u, i, s, indent, label))
	}
	return lines, cur
}

func navCell(u *cfgUI, i int, s config.Section, indent, label string) string {
	mark := "  "
	if i == u.secIdx {
		mark = glyph("▸", ">") + " "
	}
	badges := secBadges(u, s)
	bw := 0
	if len(badges) > 0 {
		bw = len(badges) + 1
	}
	left := padDisplay(indent+mark+label, cfgLeftW-bw)
	sel := i == u.secIdx
	var plain strings.Builder
	var colored strings.Builder
	if bw > 0 {
		plain.WriteString(" ")
		colored.WriteString(" ")
	}
	for _, b := range badges {
		plain.WriteString(b.g)
		colored.WriteString(b.style.Render(b.g))
	}
	switch {
	case sel && !u.focusFields:
		return StyleSelected.Render(left + plain.String())
	case sel:
		return StyleHeader.Render(left) + colored.String()
	}
	return left + colored.String()
}

// cfgTabBar is the narrow-terminal replacement of the nav column.
func cfgTabBar(u *cfgUI, inner int) string {
	secs := u.sections()
	labels := make([]string, len(secs))
	for i, s := range secs {
		labels[i] = s.Label
		if s.List != "" {
			labels[i] = fmt.Sprintf("%s(%d)", s.Label, u.draft.itemCount(s.List))
		}
		if mod, bad, _ := sectionState(u, s); bad {
			labels[i] += "!"
		} else if mod {
			labels[i] += dotGlyph()
		}
	}
	start := 0
	for start < u.secIdx {
		w := 0
		for i := start; i <= u.secIdx && i < len(labels); i++ {
			w += lipgloss.Width(labels[i]) + 3
		}
		if w <= inner-2 {
			break
		}
		start++
	}
	var b strings.Builder
	used := 0
	if start > 0 {
		b.WriteString(StyleDim.Render(glyph("‹", "<")))
		used++
	}
	for i := start; i < len(labels); i++ {
		w := lipgloss.Width(labels[i]) + 3
		if used+w > inner {
			b.WriteString(StyleDim.Render(glyph("›", ">")))
			break
		}
		cell := " " + labels[i] + " "
		switch {
		case i == u.secIdx && !u.focusFields:
			cell = StyleSelected.Render(cell)
		case i == u.secIdx:
			cell = StyleHeader.Render(cell)
		default:
			cell = StyleDim.Render(cell)
		}
		b.WriteString(cell + " ")
		used += w
	}
	return b.String()
}

// cfgSplitBody renders the sections column next to the right pane.
func cfgSplitBody(u *cfgUI, v cfgView, inner, bodyH int) []string {
	rightW := max(inner-cfgLeftW-3, 20)
	left, cur := cfgNavLines(u)
	start := 0
	if cur >= bodyH {
		start = cur - bodyH + 1
	}
	if start < len(left) {
		left = left[start:]
	} else {
		left = nil
	}
	right := cfgPane(u, v, rightW, bodyH, true)
	sep := StyleDim.Render(glyph("│", "|"))
	out := make([]string, 0, bodyH)
	for i := 0; i < bodyH; i++ {
		l := strings.Repeat(" ", cfgLeftW)
		if i < len(left) {
			l = left[i]
		}
		r := ""
		if i < len(right) {
			r = right[i]
		}
		out = append(out, l+" "+sep+" "+r)
	}
	return out
}

// ---- right pane -----------------------------------------------------------------------

func applyBadge(fields []config.FieldSpec) string {
	hot, rst := 0, 0
	for _, f := range fields {
		if string(f.Apply) == "restart" {
			rst++
		} else {
			hot++
		}
	}
	switch {
	case hot+rst == 0:
		return ""
	case rst == 0:
		return StyleServerHealthy.Render("live")
	case hot == 0:
		return StyleFlash.Render("restart")
	}
	return StyleDim.Render("mixed")
}

// cfgPane renders header + rows into exactly h lines (header stays fixed, rows scroll).
func cfgPane(u *cfgUI, v cfgView, w, h int, _ bool) []string {
	sec, ok := u.section()
	if !ok {
		return []string{StyleDim.Render("(no schema)")}
	}
	fields := u.sectionFields(sec)
	title := styleBold.Render(sec.Label)
	desc := cfgSectionDesc[sec.ID]
	if sec.List != "" && !u.onListRows() {
		title = styleBold.Render(sec.Label) + StyleDim.Render(" "+glyph("›", ">")+" ") + styleBold.Render(u.draft.itemNameAt(sec.List, u.itemIdx))
		desc = "Esc back to the list"
	}
	badge := applyBadge(fields)
	gap := max(w-lipgloss.Width(title)-lipgloss.Width(badge), 1)
	header := []string{
		title + strings.Repeat(" ", gap) + badge,
		StyleDim.Render(truncStr(desc, w)),
		rule(w),
	}

	var rows []string
	curStart, curEnd := 0, 0
	if u.onListRows() {
		rows, curStart = cfgListRows(u, v, sec, w)
		curEnd = curStart
	} else {
		for i, f := range fields {
			focused := u.focusFields && i == u.fieldIdx
			_, idx := u.curFields()
			if focused {
				curStart = len(rows)
			}
			rows = append(rows, cfgFieldRows(u, f, idx, focused, w)...)
			if focused {
				curEnd = len(rows) - 1
			}
		}
		if len(fields) == 0 {
			rows = append(rows, StyleDim.Render("(no fields)"))
		}
	}
	area := max(h-len(header), 1)
	start := 0
	if curEnd >= area {
		start = min(curEnd-area+1, curStart)
	}
	if start > len(rows) {
		start = len(rows)
	}
	return append(header, rows[start:]...)
}

func cfgListRows(u *cfgUI, v cfgView, sec config.Section, w int) ([]string, int) {
	n := u.draft.itemCount(sec.List)
	if n == 0 {
		return []string{StyleDim.Render("(none) " + glyph("—", "-") + " press a to add")}, 0
	}
	arrow := glyph("▸", ">")
	isBackends := sec.List == cfgListBackends
	const nameW = 16
	showCols := w >= 62
	health := map[string]bool{}
	for _, s := range v.Servers {
		health[s.Name] = s.Healthy
	}
	urlW := max(w-3-nameW-1-6-1, 8) // prefix, name, gap, health, gap
	if showCols {
		urlW = max(w-3-nameW-1-1-9-1-6-1-6, 10)
	}
	var lines []string
	hdr := "   " + padDisplay("Name", nameW) + " "
	switch {
	case isBackends && showCols:
		hdr += padDisplay("URL", urlW) + " " + padDisplay("Protocol", 9) + " " + padDisplay("Models", 6) + " Health"
	case isBackends:
		hdr += padDisplay("URL", urlW) + " Health"
	default:
		hdr += "Key"
	}
	lines = append(lines, StyleDim.Render(truncStr(hdr, w)))
	items := listItems(u.draft.cur, sec.List)
	for i := 0; i < n; i++ {
		name := u.draft.itemNameAt(sec.List, i)
		mark := " "
		if st := u.draft.ItemState(sec.List, i); st != "" {
			mark = st
		}
		if u.itemHasError(sec.List, i) {
			mark = "!"
		}
		sel := u.focusFields && i == u.itemIdx
		cur := " "
		if sel {
			cur = arrow
		}
		m, _ := items[i].(map[string]any)
		head := cur + mark + " " + padDisplay(name, nameW) + " "
		var cells, tail string
		if isBackends {
			url := padDisplay(rawString(m["url"]), urlW)
			cells = url
			if showCols {
				mods, _ := m["models"].([]any)
				cells += " " + padDisplay(rawString(m["type"]), 9) + " " + padDisplay(fmt.Sprint(len(mods)), 6)
			}
			cells += " "
			up, known := health[name]
			switch {
			case !known:
				tail = StyleDim.Render(glyph("–", "-"))
			case up:
				tail = StyleServerHealthy.Render(dotGlyph() + " up")
			default:
				tail = StyleServerUnhealthy.Render(dotGlyph() + " down")
			}
			if sel {
				// keep the row highlight uniform: plain health text inside
				plain := glyph("–", "-")
				if known {
					plain = dotGlyph() + " down"
					if up {
						plain = dotGlyph() + " up"
					}
				}
				lines = append(lines, StyleSelected.Render(padDisplay(head+cells+plain, w)))
				continue
			}
		} else {
			for _, f := range u.sectionFields(sec) {
				if string(f.Kind) == "secret" {
					tail = cfgSecretText(u, f, i)
				}
			}
			if sel {
				lines = append(lines, StyleSelected.Render(padDisplay(head, 3+nameW+1))+tail)
				continue
			}
		}
		if mark == "!" {
			head = cur + StyleError.Render(mark) + " " + padDisplay(name, nameW) + " "
		}
		lines = append(lines, truncStyled(head+cells+tail, w))
	}
	return lines, 1 + u.itemIdx
}

// truncStyled truncates only when needed (truncStr is ANSI-unaware for styled
// strings, so it is applied to the plain width check only).
func truncStyled(s string, w int) string {
	if lipgloss.Width(s) <= w {
		return s
	}
	return truncStr(s, w)
}

// cfgUnit derives a unit suffix from the field path.
func cfgUnit(path string) string {
	for suf, unit := range map[string]string{"_ms": "ms", "_seconds": "s", "_days": "days", "_minutes": "min"} {
		if strings.HasSuffix(path, suf) {
			return unit
		}
	}
	return ""
}

func cfgRange(f config.FieldSpec) string {
	dash := glyph("–", "-")
	switch {
	case f.Min != nil && f.Max != nil:
		return fmt.Sprintf("%d%s%d", *f.Min, dash, *f.Max)
	case f.Min != nil:
		return fmt.Sprintf("min %d", *f.Min)
	case f.Max != nil:
		return fmt.Sprintf("max %d", *f.Max)
	}
	return ""
}

// cfgLabelWidth fits the label column to the longest label of the current
// fields, between cfgLabelW and 40% of the pane.
func cfgLabelWidth(u *cfgUI, w int) int {
	lw := cfgLabelW
	fields, _ := u.curFields()
	for _, f := range fields {
		lw = max(lw, lipgloss.Width(f.Label)+1)
	}
	return max(min(lw, w*2/5), 8)
}

// cfgFieldRows renders one field: the row, its inline help (when focused) and errors.
func cfgFieldRows(u *cfgUI, f config.FieldSpec, idx int, focused bool, w int) []string {
	cur := " "
	if focused {
		cur = glyph("▸", ">")
	}
	lw := cfgLabelWidth(u, w)
	label := padDisplay(truncStr(f.Label, lw), lw)
	valW := max(w-lw-3, 8)

	var val string
	switch {
	case focused && (u.mode == cmEdit || u.mode == cmSecretType):
		u.input.Width = max(valW-2, 4)
		val = u.input.View()
	default:
		val = cfgValueText(u, f, idx, valW)
	}
	prefix := cur + " " + label + " "
	if focused && u.mode == cmNormal {
		prefix = StyleHeader.Render(cur+" "+label) + " "
	}
	line := prefix + val
	used := lipgloss.Width(line)
	var extras []string
	if u.draft.Modified(f, idx) {
		was := "changed"
		if string(f.Kind) != "secret" {
			if old, ok := u.draft.origRaw(f, idx); ok {
				was = "was " + truncStr(quoteEmpty(rawString(old)), 20)
			}
		}
		extras = append(extras, StyleFlash.Render(dotGlyph()), StyleDim.Render(was))
	}
	if string(f.Apply) == "restart" {
		extras = append(extras, StyleDim.Render(restartGlyph()))
	}
	if r := cfgRange(f); r != "" && string(f.Kind) == "int" {
		extras = append(extras, StyleDim.Render(r))
	}
	if pad := 14 - lipgloss.Width(val); pad > 0 && len(extras) > 0 {
		line += strings.Repeat(" ", pad)
		used += pad
	}
	for _, e := range extras {
		if used+2+lipgloss.Width(e) > w {
			break
		}
		line += "  " + e
		used += 2 + lipgloss.Width(e)
	}
	lines := []string{truncStyled(line, w)}
	if msg := u.fieldError(f, idx); msg != "" {
		lines = append(lines, "    "+StyleError.Render(truncStr(glyph("✗ ", "! ")+msg, w-4)))
	}
	if focused && f.Help != "" {
		for _, l := range wrapWords(f.Help, max(w-6, 10), 2) {
			lines = append(lines, "    "+StyleDim.Render(l))
		}
	}
	return lines
}

// wrapWords wraps s to width, at most maxLines lines (last one ellipsised).
func wrapWords(s string, width, maxLines int) []string {
	var lines []string
	cur := ""
	for _, word := range strings.Fields(s) {
		switch {
		case cur == "":
			cur = word
		case lipgloss.Width(cur)+1+lipgloss.Width(word) <= width:
			cur += " " + word
		default:
			lines = append(lines, cur)
			cur = word
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		lines[maxLines-1] = truncStr(lines[maxLines-1]+"…", width)
	}
	return lines
}

// cfgSecretText is the displayed secret: pending op > revealed > masked hint.
func cfgSecretText(u *cfgUI, f config.FieldSpec, idx int) string {
	switch u.draft.SecretOpFor(f, idx) {
	case secOpSet:
		return StyleFlash.Render("(will be set)")
	case secOpGenerate:
		return StyleFlash.Render("(will be generated)")
	case secOpClear:
		return StyleFlash.Render("(will be cleared)")
	}
	path := u.draft.fieldPath(f, idx)
	if val, ok := u.revealed[path]; ok {
		return StyleFlash.Render(val)
	}
	if u.revealWant == path {
		return StyleDim.Render("revealing…")
	}
	t := u.draft.Text(f, idx)
	if t == "" {
		return StyleDim.Render("(not set)")
	}
	return strings.Repeat(glyph("•", "*"), 8) + strings.TrimLeft(t, "….")
}

// cfgValueText is the displayed (non-editing) value of a field.
func cfgValueText(u *cfgUI, f config.FieldSpec, idx, valW int) string {
	switch string(f.Kind) {
	case "secret":
		return cfgSecretText(u, f, idx)
	case "bool":
		if b, _ := u.draft.curRaw(f, idx).(bool); b {
			return StyleServerHealthy.Render("[x] on")
		}
		return StyleDim.Render("[ ] off")
	case "enum":
		t := u.draft.Text(f, idx)
		if t == "" {
			return StyleDim.Render("(unset)")
		}
		plainW := 0
		for _, e := range f.Enum {
			plainW += lipgloss.Width(e) + 2
		}
		if plainW > valW {
			return glyph("‹ ", "< ") + t + glyph(" ›", " >")
		}
		var b strings.Builder
		for _, e := range f.Enum {
			switch {
			case e == t && noUnicode():
				b.WriteString(StyleSelected.Render("[" + e + "]"))
			case e == t:
				b.WriteString(StyleSelected.Render(" " + e + " "))
			default:
				b.WriteString(StyleDim.Render(" " + e + " "))
			}
		}
		return b.String()
	}
	t := u.draft.Text(f, idx)
	if t == "" {
		return StyleDim.Render(glyph("—", "-"))
	}
	if string(f.Kind) == "int" {
		if unit := cfgUnit(f.Path); unit != "" {
			return t + " " + StyleDim.Render(unit)
		}
	}
	return t
}

// ---- footer ---------------------------------------------------------------------------

type hint struct{ k, d string }

func renderHints(hs []hint, w int) string {
	var b strings.Builder
	used := 0
	for _, h := range hs {
		piece := StyleHelpKey.Render(h.k) + " " + StyleDim.Render(h.d)
		pw := lipgloss.Width(h.k) + 1 + lipgloss.Width(h.d)
		if used > 0 {
			pw += 2
		}
		if used+pw > w {
			break
		}
		if used > 0 {
			b.WriteString("  ")
		}
		b.WriteString(piece)
		used += pw
	}
	return b.String()
}

func cfgHints(u *cfgUI) []hint {
	updn := glyph("↑↓", "up/dn")
	lr := glyph("←→", "l/r")
	switch u.mode {
	case cmEdit, cmAdd, cmSecretType:
		return []hint{{"Enter", "confirm"}, {"Esc", "cancel"}}
	case cmSecretMenu:
		return []hint{{"t", "type new"}, {"g", "generate"}, {"c", "clear"}, {"u", "undo"}, {"Esc", "cancel"}}
	case cmDelete, cmDiscard:
		return []hint{{"y", "yes"}, {"n", "no"}}
	case cmReview:
		return []hint{{"Enter", "send"}, {"Esc", "back"}}
	case cmGenerated:
		return []hint{{"y", "copy"}, {"Enter", "close"}}
	case cmSaving:
		return []hint{{"Esc", "dismiss"}}
	}
	if !u.loaded {
		return []hint{{"r", "reload"}, {"Esc", "close"}}
	}
	var hs []hint
	back := hint{"Esc", "close"}
	if u.inItem {
		back = hint{"Esc", "back"}
	}
	if !u.focusFields {
		return []hint{{updn, "section"}, {"Enter", "open"}, {"Tab", "pane"}, {"Ctrl+S", "save"}, back}
	}
	revealDesc := "reveal"
	if p, ok := u.focusedSecretPath(); ok {
		if _, shown := u.revealed[p]; shown {
			revealDesc = "hide"
		}
		hs = append(hs, hint{updn, "field"}, hint{"v", revealDesc}, hint{"y", "copy"})
	}
	if u.onListRows() {
		if len(hs) == 0 {
			hs = append(hs, hint{updn, "row"})
		}
		hs = append(hs, hint{"Enter", "open"}, hint{"a", "add"}, hint{"d", "delete"})
	} else {
		if len(hs) == 0 {
			hs = append(hs, hint{updn, "field"})
		}
		hs = append(hs, hint{"Enter", "edit"}, hint{lr, "change"})
	}
	return append(hs, hint{"Tab", "pane"}, hint{"Ctrl+S", "save"}, back)
}

// cfgFooter returns the status line, the prompt/error line and the key hints.
func cfgFooter(v cfgView, inner int) (status, prompt, hints string) {
	u := v.UI
	switch {
	case u.banner != "":
		status = StyleFlash.Render(u.banner)
	case u.toast != "":
		status = StyleFlash.Render(u.toast)
	case len(v.RestartPending) > 0:
		status = StyleFlash.Render(fmt.Sprintf("%s restart pending: %s %s restart the daemon to apply",
			restartGlyph(), strings.Join(v.RestartPending, ", "), glyph("—", "-")))
	}

	switch u.mode {
	case cmEdit, cmAdd, cmSecretType:
		switch u.mode {
		case cmAdd:
			prompt = "New name: " + u.input.View()
		case cmSecretType:
			prompt = "New secret: " + u.input.View()
		}
		if u.editErr != "" && u.mode != cmSecretType {
			prompt += "  " + StyleError.Render(u.editErr)
		}
	case cmDelete:
		prompt = StyleFlash.Render("Delete this item? y/n")
	case cmDiscard:
		prompt = StyleFlash.Render("Discard changes? y/n")
	case cmSaving:
		prompt = StyleDim.Render("Saving…")
	case cmNormal:
		if spec, idx, ok := u.focusedSpecSafe(); ok {
			if msg := u.fieldError(spec, idx); msg != "" {
				prompt = StyleError.Render(msg)
			}
		} else if len(u.errList) > 0 {
			e := u.errList[0]
			prompt = StyleError.Render(fmt.Sprintf("%s: %s", e.Path, e.Msg))
		}
	}
	return status, prompt, renderHints(cfgHints(u), inner)
}

// focusedSpecSafe is focusedSpec guarded against an unloaded draft.
func (u *cfgUI) focusedSpecSafe() (config.FieldSpec, int, bool) {
	if !u.loaded || u.draft == nil {
		return config.FieldSpec{}, -1, false
	}
	return u.focusedSpec()
}

// restartBadge is the compact main-screen indicator ("" when nothing pending).
func restartBadge(pending []string) string {
	if len(pending) == 0 {
		return ""
	}
	return StyleFlash.Render(fmt.Sprintf("%s restart pending (%d)", restartGlyph(), len(pending)))
}
