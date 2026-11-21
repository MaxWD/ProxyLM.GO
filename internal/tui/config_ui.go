package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"proxylm/internal/config"
	"proxylm/internal/ipc"
)

// State machine of the Config overlay. It performs no I/O: key handlers return
// a cfgAction and the app turns it into a tea.Cmd.

type cfgAction int

const (
	actNone cfgAction = iota
	actGet
	actUpdate
	actClose
)

type cfgMode int

const (
	cmNormal cfgMode = iota
	cmEdit
	cmSecretMenu
	cmSecretType
	cmAdd
	cmDelete
	cmReview
	cmDiscard
	cmSaving
	cmGenerated
)

const (
	cfgBannerChanged  = "Changed by another client — press r to reload"
	cfgBannerConflict = "Config changed elsewhere — press r to reload (your edits will be lost)"
)

// genEntry is one generated secret shown once after saving.
type genEntry struct{ Path, Key string }

// cfgUI is the whole Config overlay state.
type cfgUI struct {
	loaded   bool
	loadErr  string
	path     string
	revision string
	draft    *cfgDraft

	secIdx      int
	focusFields bool
	inItem      bool // item form of a list section is open
	itemIdx     int  // selected list row / open item
	fieldIdx    int

	mode    cfgMode
	input   textinput.Model
	editErr string

	toast    string
	banner   string
	conflict bool

	errs    map[string]string // concrete path -> message (last result)
	errList []config.FieldError

	review    []string
	generated []genEntry

	out ipc.ConfigUpdatePayload // payload to send with actUpdate
}

func newCfgUI() *cfgUI {
	ti := textinput.New()
	ti.CharLimit = 256
	ti.Cursor.SetMode(cursor.CursorStatic)
	return &cfgUI{input: ti, errs: map[string]string{}}
}

// ---- schema navigation ------------------------------------------------------------

func (u *cfgUI) sections() []config.Section {
	if u.draft == nil {
		return nil
	}
	return u.draft.schema.Sections
}

func (u *cfgUI) section() (config.Section, bool) {
	s := u.sections()
	if u.secIdx < 0 || u.secIdx >= len(s) {
		return config.Section{}, false
	}
	return s[u.secIdx], true
}

// sectionFields returns the field specs of a section (item fields for lists).
func (u *cfgUI) sectionFields(sec config.Section) []config.FieldSpec {
	var out []config.FieldSpec
	for _, f := range u.draft.schema.Fields {
		if f.Section == sec.ID {
			out = append(out, f)
		}
	}
	return out
}

// onListRows reports whether the right pane currently shows list rows.
func (u *cfgUI) onListRows() bool {
	sec, ok := u.section()
	return ok && sec.List != "" && !u.inItem
}

// curFields returns the focused-pane field specs and the item index (-1 for scalar).
func (u *cfgUI) curFields() ([]config.FieldSpec, int) {
	sec, ok := u.section()
	if !ok {
		return nil, -1
	}
	if sec.List != "" {
		return u.sectionFields(sec), u.itemIdx
	}
	return u.sectionFields(sec), -1
}

// rowCount is the number of navigable rows in the right pane.
func (u *cfgUI) rowCount() int {
	sec, ok := u.section()
	if !ok {
		return 0
	}
	if u.onListRows() {
		return u.draft.itemCount(sec.List)
	}
	return len(u.sectionFields(sec))
}

func (u *cfgUI) cursorRef() *int {
	if u.onListRows() {
		return &u.itemIdx
	}
	return &u.fieldIdx
}

func (u *cfgUI) focusedSpec() (config.FieldSpec, int, bool) {
	if !u.focusFields || u.onListRows() {
		return config.FieldSpec{}, -1, false
	}
	fs, idx := u.curFields()
	if u.fieldIdx < 0 || u.fieldIdx >= len(fs) {
		return config.FieldSpec{}, -1, false
	}
	return fs[u.fieldIdx], idx, true
}

// fieldError returns the daemon error for a field's concrete path.
func (u *cfgUI) fieldError(spec config.FieldSpec, idx int) string {
	if len(u.errs) == 0 {
		return ""
	}
	if msg, ok := u.errs[u.draft.fieldPath(spec, idx)]; ok {
		return msg
	}
	if list, _, isItem := splitSpecPath(spec.Path); isItem {
		if msg, ok := u.errs[concretePath(spec.Path, fmt.Sprintf("#%d", idx))]; ok {
			return msg
		}
		_ = list
	}
	return ""
}

// itemHasError reports whether any error targets the item (name or #idx).
func (u *cfgUI) itemHasError(list string, idx int) bool {
	name := u.draft.itemNameAt(list, idx)
	for p := range u.errs {
		for _, pre := range []string{fmt.Sprintf("%s[%s]", list, name), fmt.Sprintf("%s[#%d]", list, idx)} {
			if p == pre || strings.HasPrefix(p, pre+".") {
				return true
			}
		}
	}
	return false
}

// ---- incoming daemon messages ------------------------------------------------------

// onState applies a config_state payload (replaces the draft).
func (u *cfgUI) onState(p ipc.ConfigStatePayload) {
	if p.Error != "" {
		u.loadErr = p.Error
		return
	}
	d, err := newCfgDraft(p.Config, p.Schema)
	if err != nil {
		u.loadErr = err.Error()
		return
	}
	prevSec := u.secIdx
	u.draft, u.loaded, u.loadErr = d, true, ""
	u.path, u.revision = p.Path, p.Revision
	u.secIdx = 0
	if prevSec < len(d.schema.Sections) {
		u.secIdx = prevSec
	}
	u.inItem, u.itemIdx, u.fieldIdx = false, 0, 0
	u.banner, u.conflict = "", false
	u.errs, u.errList = map[string]string{}, nil
	if u.mode != cmGenerated {
		u.mode = cmNormal
	}
}

// onResult applies a config_result payload; returns actGet to reload after success.
func (u *cfgUI) onResult(p ipc.ConfigResultPayload) cfgAction {
	if u.mode == cmSaving {
		u.mode = cmNormal
	}
	switch {
	case p.Conflict:
		u.conflict = true
		u.banner = cfgBannerConflict
		return actNone
	case p.OK:
		u.toast = fmt.Sprintf("Saved %s applied %d %s restart required %d",
			glyph("·", "-"), len(p.Applied), glyph("·", "-"), len(p.RestartRequired))
		u.errs, u.errList = map[string]string{}, nil
		if len(p.Generated) > 0 {
			u.generated = u.generated[:0]
			for path, key := range p.Generated {
				u.generated = append(u.generated, genEntry{Path: path, Key: key})
			}
			sort.Slice(u.generated, func(i, j int) bool { return u.generated[i].Path < u.generated[j].Path })
			u.mode = cmGenerated
		}
		return actGet
	}
	u.errs = make(map[string]string, len(p.Errors))
	for _, e := range p.Errors {
		u.errs[e.Path] = e.Msg
	}
	u.errList = p.Errors
	if len(p.Errors) == 0 {
		msg := p.Error
		if msg == "" {
			msg = "save failed"
		}
		u.toast = "Error: " + msg
	} else {
		u.toast = fmt.Sprintf("Save failed: %d error(s)", len(p.Errors))
	}
	return actNone
}

// onChanged handles config_changed from another client.
func (u *cfgUI) onChanged(p ipc.ConfigChangedPayload) {
	if u.loaded && p.Revision != "" && p.Revision != u.revision && !u.conflict {
		u.banner = cfgBannerChanged
	}
}

// ---- keys ---------------------------------------------------------------------------

func isEnter(s string) bool { return s == "enter" }
func isEsc(s string) bool   { return s == "esc" }

// handleKey processes one key press and returns the requested action.
func (u *cfgUI) handleKey(msg tea.KeyMsg) cfgAction {
	s := msg.String()
	if u.mode != cmNormal {
		return u.handleModeKey(msg, s)
	}
	u.toast = ""
	if !u.loaded {
		if isEsc(s) || s == "q" || s == "c" {
			return actClose
		}
		if s == "r" {
			return actGet
		}
		return actNone
	}
	switch s {
	case "ctrl+s":
		return u.startReview()
	case "esc", "q", "c":
		if s == "esc" && u.inItem {
			u.inItem = false
			u.fieldIdx = 0
			return actNone
		}
		return u.tryClose()
	case "r":
		if u.banner != "" || u.conflict || !u.draft.Dirty() {
			return actGet
		}
	case "tab":
		u.focusFields = !u.focusFields
	case "up", "k":
		u.move(-1)
	case "down", "j":
		u.move(1)
	case "pgup":
		u.move(-10)
	case "pgdown":
		u.move(10)
	case "home":
		u.moveTo(0)
	case "end":
		u.moveTo(1 << 30)
	case "left", "h":
		if spec, idx, ok := u.focusedSpec(); ok && string(spec.Kind) == "enum" {
			u.draft.Cycle(spec, idx, -1)
		} else {
			u.focusFields = false
		}
	case "right", "l":
		if !u.focusFields {
			u.focusFields = true
		} else if spec, idx, ok := u.focusedSpec(); ok && string(spec.Kind) == "enum" {
			u.draft.Cycle(spec, idx, 1)
		}
	case " ", "space":
		if spec, idx, ok := u.focusedSpec(); ok && string(spec.Kind) == "bool" {
			u.draft.Toggle(spec, idx)
		}
	case "enter":
		u.activate()
	case "a":
		if sec, ok := u.section(); ok && sec.List != "" && !u.inItem {
			u.mode = cmAdd
			u.editErr = ""
			u.startInput("", "name", false)
		}
	case "d":
		if u.onListRows() && u.focusFields && u.rowCount() > 0 {
			u.mode = cmDelete
		}
	}
	return actNone
}

func (u *cfgUI) tryClose() cfgAction {
	if u.draft != nil && u.draft.Dirty() {
		u.mode = cmDiscard
		return actNone
	}
	return actClose
}

func (u *cfgUI) startReview() cfgAction {
	u.review = u.draft.Changes()
	if len(u.review) == 0 {
		u.toast = "No changes"
		return actNone
	}
	u.mode = cmReview
	return actNone
}

// move moves the cursor of the focused pane by delta rows.
func (u *cfgUI) move(delta int) {
	if !u.focusFields {
		n := len(u.sections())
		if n == 0 {
			return
		}
		u.secIdx = clampInt(u.secIdx+delta, 0, n-1)
		u.inItem, u.itemIdx, u.fieldIdx = false, 0, 0
		return
	}
	ref := u.cursorRef()
	n := u.rowCount()
	if n == 0 {
		*ref = 0
		return
	}
	*ref = clampInt(*ref+delta, 0, n-1)
}

func (u *cfgUI) moveTo(pos int) {
	if !u.focusFields {
		u.move(pos - u.secIdx)
		return
	}
	ref := u.cursorRef()
	u.move(pos - *ref)
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// activate handles Enter in normal mode.
func (u *cfgUI) activate() {
	if !u.focusFields {
		if u.rowCount() > 0 || u.onListRows() {
			u.focusFields = true
		}
		return
	}
	if u.onListRows() {
		if u.rowCount() > 0 {
			u.inItem = true
			u.fieldIdx = 0
		}
		return
	}
	spec, idx, ok := u.focusedSpec()
	if !ok {
		return
	}
	switch string(spec.Kind) {
	case "enum":
		u.draft.Cycle(spec, idx, 1)
	case "bool":
		u.draft.Toggle(spec, idx)
	case "secret":
		u.mode = cmSecretMenu
	default:
		u.mode = cmEdit
		u.editErr = ""
		u.startInput(u.draft.Text(spec, idx), "", false)
	}
}

func (u *cfgUI) startInput(val, placeholder string, password bool) {
	u.input.SetValue(val)
	u.input.Placeholder = placeholder
	u.input.CursorEnd()
	if password {
		u.input.EchoMode = textinput.EchoPassword
	} else {
		u.input.EchoMode = textinput.EchoNormal
	}
	u.input.Focus()
}

// handleModeKey routes keys while a prompt / dialog is active.
func (u *cfgUI) handleModeKey(msg tea.KeyMsg, s string) cfgAction {
	switch u.mode {
	case cmEdit:
		switch {
		case isEsc(s):
			u.mode = cmNormal
		case isEnter(s):
			spec, idx, ok := u.focusedSpec()
			if !ok {
				u.mode = cmNormal
				break
			}
			if err := u.draft.SetText(spec, idx, u.input.Value()); err != nil {
				u.editErr = err.Error()
				break
			}
			u.mode = cmNormal
		default:
			u.input, _ = u.input.Update(msg)
		}
	case cmAdd:
		switch {
		case isEsc(s):
			u.mode = cmNormal
		case isEnter(s):
			sec, _ := u.section()
			idx, err := u.draft.AddItem(sec.List, u.input.Value())
			if err != nil {
				u.editErr = err.Error()
				break
			}
			u.mode = cmNormal
			u.focusFields = true
			u.itemIdx = idx
			if sec.List == cfgListBackends {
				u.inItem, u.fieldIdx = true, 0
			}
		default:
			u.input, _ = u.input.Update(msg)
		}
	case cmSecretMenu:
		spec, idx, ok := u.focusedSpec()
		switch s {
		case "esc":
			u.mode = cmNormal
		case "t":
			u.mode = cmSecretType
			u.startInput("", "new value", true)
		case "g":
			u.setSecret(ok, spec, idx, secOpGenerate, "")
		case "c":
			u.setSecret(ok, spec, idx, secOpClear, "")
		case "u": // undo a pending op
			u.setSecret(ok, spec, idx, "", "")
		}
	case cmSecretType:
		switch {
		case isEsc(s):
			u.mode = cmNormal
		case isEnter(s):
			spec, idx, ok := u.focusedSpec()
			if v := u.input.Value(); v != "" {
				u.setSecret(ok, spec, idx, secOpSet, v)
			} else {
				u.mode = cmNormal
			}
			u.input.SetValue("")
		default:
			u.input, _ = u.input.Update(msg)
		}
	case cmDelete:
		switch s {
		case "y", "Y":
			if sec, ok := u.section(); ok {
				u.draft.DeleteItem(sec.List, u.itemIdx)
				u.itemIdx = clampInt(u.itemIdx, 0, max(u.rowCount()-1, 0))
			}
			u.mode = cmNormal
		case "n", "N", "esc":
			u.mode = cmNormal
		}
	case cmReview:
		switch {
		case isEsc(s):
			u.mode = cmNormal
		case isEnter(s):
			p, err := u.draft.BuildUpdate(u.revision)
			if err != nil {
				u.mode = cmNormal
				u.toast = "Error: " + err.Error()
				return actNone
			}
			u.out = p
			u.mode = cmSaving
			return actUpdate
		}
	case cmDiscard:
		switch s {
		case "y", "Y":
			return actClose
		case "n", "N", "esc":
			u.mode = cmNormal
		}
	case cmSaving:
		if isEsc(s) {
			u.mode = cmNormal
		}
	case cmGenerated:
		if isEnter(s) || isEsc(s) {
			u.mode = cmNormal
			u.generated = nil
		}
	}
	return actNone
}

func (u *cfgUI) setSecret(ok bool, spec config.FieldSpec, idx int, op, value string) {
	u.mode = cmNormal
	if !ok {
		return
	}
	if err := u.draft.SetSecretOp(spec, idx, op, value); err != nil {
		u.toast = "Error: " + err.Error()
	}
}
