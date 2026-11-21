package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"proxylm/internal/config"
)

// ---- Config overlay rendering ----------------------------------------------------

// cfgChromeLines — fixed lines besides the body: title, status, help, hints
// plus the top/bottom border.
const cfgChromeLines = 6

const cfgLeftW = 16

// cfgView is everything the pure renderer needs.
type cfgView struct {
	UI             *cfgUI
	RestartPending []string
}

func cfgBodyHeight(termH int) int {
	h := termH - cfgChromeLines
	if h < 5 {
		h = 5
	}
	return h
}

func restartGlyph() string { return glyph("⟳", "R") }

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

	// Title.
	left := "Config"
	if u.path != "" {
		left += " " + glyph("·", "-") + " " + u.path
	}
	right := ""
	if u.revision != "" {
		rev := u.revision
		if len(rev) > 8 {
			rev = rev[:8]
		}
		right = "rev " + rev
	}
	if w := inner - lipgloss.Width(right) - 1; lipgloss.Width(left) > w {
		left = truncStr(left, max(w, 4))
	}
	gap := inner - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	title := StyleModalTitle.Render(left) + strings.Repeat(" ", gap) + StyleDim.Render(right)

	var body []string
	switch {
	case !u.loaded && u.loadErr != "":
		body = []string{StyleError.Render("Error: " + u.loadErr)}
	case !u.loaded:
		body = []string{StyleDim.Render("Loading…")}
	case u.mode == cmReview:
		body = cfgReviewBody(u, inner)
	case u.mode == cmGenerated:
		body = cfgGeneratedBody(u)
	default:
		body = cfgSplitBody(u, inner, bodyH)
	}
	body = fitLines(body, bodyH)

	status, help, hints := cfgFooter(v, inner)
	rows := []string{title}
	rows = append(rows, body...)
	rows = append(rows, clip(status), clip(help), StyleDim.Render(clip(hints)))
	return StyleBorderActive.Width(termW - 2).Height(termH - 2).Render(strings.Join(rows, "\n"))
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

func cfgReviewBody(u *cfgUI, inner int) []string {
	lines := []string{StyleHelpKey.Render("Review changes:")}
	for _, c := range u.review {
		lines = append(lines, "  "+truncStr(c, inner-2))
	}
	return lines
}

func cfgGeneratedBody(u *cfgUI) []string {
	lines := []string{StyleFlash.Render("Generated keys"), ""}
	for _, g := range u.generated {
		lines = append(lines, "  "+g.Path, "    "+StyleHelpKey.Render(g.Key), "")
	}
	lines = append(lines, StyleFlash.Render("Shown once — copy it now. Press Enter to close."))
	return lines
}

// cfgSplitBody renders the sections column next to the fields column.
func cfgSplitBody(u *cfgUI, inner, bodyH int) []string {
	rightW := inner - cfgLeftW - 3
	if rightW < 20 {
		rightW = 20
	}
	var leftLines []string
	for i, s := range u.sections() {
		label := s.Label
		if s.List != "" {
			label = fmt.Sprintf("%s (%d)", label, u.draft.itemCount(s.List))
		}
		mark := " "
		if i == u.secIdx {
			mark = glyph("▸", ">")
		}
		cell := padDisplay(mark+truncStr(label, cfgLeftW-1), cfgLeftW)
		switch {
		case i == u.secIdx && !u.focusFields:
			cell = StyleSelected.Render(cell)
		case i == u.secIdx:
			cell = StyleHeader.Render(cell)
		}
		leftLines = append(leftLines, cell)
	}
	rightLines, cursorLine := cfgRightLines(u, rightW)

	// Scroll the right column so the cursor line stays visible.
	start := 0
	if cursorLine >= bodyH {
		start = cursorLine - bodyH + 1
	}
	if start > len(rightLines) {
		start = len(rightLines)
	}
	rightLines = rightLines[start:]

	sep := StyleDim.Render(glyph("│", "|"))
	out := make([]string, 0, bodyH)
	for i := 0; i < bodyH; i++ {
		l := strings.Repeat(" ", cfgLeftW)
		if i < len(leftLines) {
			l = leftLines[i]
		}
		r := ""
		if i < len(rightLines) {
			r = rightLines[i]
		}
		out = append(out, l+" "+sep+" "+r)
	}
	return out
}

const cfgLabelW = 22

// cfgRightLines builds the right column and returns the cursor's line index.
func cfgRightLines(u *cfgUI, w int) ([]string, int) {
	sec, ok := u.section()
	if !ok {
		return []string{StyleDim.Render("(no schema)")}, 0
	}
	arrow := glyph("▸", ">")
	if u.onListRows() {
		return cfgListLines(u, sec, w, arrow)
	}
	var lines []string
	cursorLine := 0
	if sec.List != "" {
		lines = append(lines, StyleDim.Render(fmt.Sprintf("%s [%s]  Esc: back to list", sec.Label, u.draft.itemNameAt(sec.List, u.itemIdx))))
	}
	fields, idx := u.curFields()
	for i, f := range fields {
		focused := u.focusFields && i == u.fieldIdx
		if focused {
			cursorLine = len(lines)
		}
		lines = append(lines, cfgFieldLine(u, f, idx, focused, w, arrow))
		if msg := u.fieldError(f, idx); msg != "" {
			lines = append(lines, "    "+StyleError.Render(truncStr(glyph("✗ ", "! ")+msg, w-4)))
		}
	}
	if len(fields) == 0 {
		lines = append(lines, StyleDim.Render("(no fields)"))
	}
	return lines, cursorLine
}

func cfgListLines(u *cfgUI, sec config.Section, w int, arrow string) ([]string, int) {
	n := u.draft.itemCount(sec.List)
	if n == 0 {
		return []string{StyleDim.Render("(none) — press a to add")}, 0
	}
	lines := make([]string, 0, n)
	for i := 0; i < n; i++ {
		name := u.draft.itemNameAt(sec.List, i)
		st := u.draft.ItemState(sec.List, i)
		mark := " "
		if st != "" {
			mark = st
		}
		if u.itemHasError(sec.List, i) {
			mark = StyleError.Render("!")
		}
		cur := " "
		if u.focusFields && i == u.itemIdx {
			cur = arrow
		}
		row := cur + mark + " " + truncStr(name, w-4)
		if u.focusFields && i == u.itemIdx {
			row = StyleSelected.Render(padDisplay(row, w))
		}
		lines = append(lines, row)
	}
	return lines, u.itemIdx
}

// cfgFieldLine renders one "label  value  markers" row.
func cfgFieldLine(u *cfgUI, f config.FieldSpec, idx int, focused bool, w int, arrow string) string {
	cur := " "
	if focused {
		cur = arrow
	}
	label := padDisplay(truncStr(f.Label, cfgLabelW), cfgLabelW)
	valW := w - cfgLabelW - 7
	if valW < 8 {
		valW = 8
	}

	var val string
	switch {
	case focused && (u.mode == cmEdit || u.mode == cmSecretType):
		u.input.Width = valW - 2
		val = u.input.View()
	default:
		val = cfgValueText(u, f, idx)
	}
	if lipgloss.Width(val) > valW && !(focused && u.mode == cmEdit) {
		val = truncStr(val, valW)
	}

	marks := ""
	if u.draft.Modified(f, idx) {
		marks += " *"
	}
	if string(f.Apply) == "restart" {
		marks += " " + restartGlyph()
	}
	line := cur + " " + label + " " + val + StyleFlash.Render(marks)
	if focused && u.mode == cmNormal {
		return StyleHeader.Render(cur+" "+label+" ") + val + StyleFlash.Render(marks)
	}
	return line
}

// cfgValueText is the displayed (non-editing) value of a field.
func cfgValueText(u *cfgUI, f config.FieldSpec, idx int) string {
	switch string(f.Kind) {
	case "secret":
		switch u.draft.SecretOpFor(f, idx) {
		case secOpSet:
			return StyleFlash.Render("(will be set)")
		case secOpGenerate:
			return StyleFlash.Render("(will be generated)")
		case secOpClear:
			return StyleFlash.Render("(will be cleared)")
		}
		if t := u.draft.Text(f, idx); t != "" {
			return t
		}
		return StyleDim.Render("(not set)")
	case "bool":
		if b, _ := u.draft.curRaw(f, idx).(bool); b {
			return "on"
		}
		return "off"
	case "enum":
		t := u.draft.Text(f, idx)
		if t == "" {
			return StyleDim.Render("(unset)")
		}
		return glyph("‹ ", "< ") + t + glyph(" ›", " >")
	}
	t := u.draft.Text(f, idx)
	if t == "" {
		return StyleDim.Render("—")
	}
	return t
}

// cfgFooter returns the status line, the help/prompt line and the key hints.
func cfgFooter(v cfgView, inner int) (status, help, hints string) {
	u := v.UI
	dot := glyph("·", "-")

	// Status: banner > toast > restart pending > blank.
	switch {
	case u.banner != "":
		status = StyleFlash.Render(u.banner)
	case u.toast != "":
		status = StyleFlash.Render(u.toast)
	case len(v.RestartPending) > 0:
		status = StyleFlash.Render(fmt.Sprintf("%s restart pending: %s — restart the daemon to apply",
			restartGlyph(), strings.Join(v.RestartPending, ", ")))
	}

	// Help line: prompts first, then focused field help, then a short error list.
	switch u.mode {
	case cmEdit, cmAdd, cmSecretType:
		prompt := "Enter confirm " + dot + " Esc cancel"
		switch u.mode {
		case cmAdd:
			prompt = "New name: " + u.input.View() + "  " + prompt
		case cmSecretType:
			prompt = "New secret: " + u.input.View() + "  " + prompt
		}
		if u.editErr != "" && u.mode != cmSecretType {
			prompt += "  " + StyleError.Render(u.editErr)
		}
		help = prompt
	case cmSecretMenu:
		help = "[t] type new  [g] generate  [c] clear  [u] undo  Esc cancel"
	case cmDelete:
		help = StyleFlash.Render("Delete this item? y/n")
	case cmDiscard:
		help = StyleFlash.Render("Discard changes? y/n")
	case cmReview:
		help = "Enter send " + dot + " Esc back"
	case cmSaving:
		help = StyleDim.Render("Saving…")
	case cmGenerated:
		help = "Enter close"
	default:
		if spec, idx, ok := u.focusedSpecSafe(); ok {
			if msg := u.fieldError(spec, idx); msg != "" {
				help = StyleError.Render(msg)
			} else if spec.Help != "" {
				help = StyleDim.Render(spec.Help)
			}
		} else if len(u.errList) > 0 {
			e := u.errList[0]
			help = StyleError.Render(fmt.Sprintf("%s: %s", e.Path, e.Msg))
		}
	}

	switch {
	case u.mode == cmNormal && u.loaded:
		hints = strings.Join([]string{
			glyph("↑↓", "up/dn") + " field",
			"Tab pane",
			"Enter edit",
			"a/d add/del",
			"Ctrl+S save",
			"Esc close",
		}, " "+dot+" ")
	default:
		hints = "Esc cancel"
	}
	return status, help, hints
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
