package tui

import (
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"proxylm/internal/config"
	"proxylm/internal/ipc"
)

func testCfg() *config.Config {
	return &config.Config{
		Proxy: config.Proxy{Host: "0.0.0.0", Port: 8080, LogLevel: "info"},
		Auth: config.Auth{
			AdminKey: "…wxyz",
			APIKeys:  []config.APIKey{{Name: "crm", Key: "…abcd"}, {Name: "bot", Key: "…1234"}},
		},
		Routing:   config.Routing{Strategy: config.StrategyRoundRobin},
		Retry:     config.Retry{MaxAttempts: 3, InitialBackoffMs: 500, MaxBackoffMs: 8000},
		Discovery: config.Discovery{Enabled: true, IntervalSeconds: 30, UnhealthyAfterFailedPolls: 3},
		Backends: []config.Backend{
			{Name: "gpu-1", URL: "http://a:1", Type: "openai", Priority: 1, TimeoutSeconds: 600, APIKey: "…key1", Models: []string{"m1", "m2"}},
			{Name: "gpu-2", URL: "http://b:2", Type: "ollama", Priority: 2, TimeoutSeconds: 600},
		},
	}
}

func testDraft(t *testing.T) *cfgDraft {
	t.Helper()
	d, err := newCfgDraft(testCfg(), config.ConfigSchema())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func specOf(t *testing.T, d *cfgDraft, path string) config.FieldSpec {
	t.Helper()
	for _, f := range d.schema.Fields {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("no spec %s", path)
	return config.FieldSpec{}
}

func TestPathAccessor(t *testing.T) {
	d := testDraft(t)
	tests := []struct {
		path string
		want string
		ok   bool
	}{
		{"retry.max_attempts", "3", true},
		{"proxy.host", "0.0.0.0", true},
		{"backends[gpu-1].url", "http://a:1", true},
		{"backends[#1].name", "gpu-2", true},
		{"auth.api_keys[crm].key", "…abcd", true},
		{"backends[nope].url", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			v, ok := pathGet(d.cur, tt.path)
			if ok != tt.ok || rawString(v) != tt.want {
				t.Errorf("pathGet(%q) = %q,%v want %q,%v", tt.path, rawString(v), ok, tt.want, tt.ok)
			}
		})
	}
	if err := pathSet(d.cur, "backends[gpu-2].url", "http://z"); err != nil {
		t.Fatal(err)
	}
	if v, _ := pathGet(d.cur, "backends[gpu-2].url"); v != "http://z" {
		t.Errorf("set not applied: %v", v)
	}
	if err := pathSet(d.cur, "backends[nope].url", "x"); err == nil {
		t.Error("expected error for unknown item")
	}
	if it, ok := pathGet(d.cur, "backends[gpu-1]"); !ok || itemName(it) != "gpu-1" {
		t.Errorf("whole item get failed")
	}
	if got := parsePath("backends[gpu.1].url"); len(got) != 2 || got[0].sel != "gpu.1" {
		t.Errorf("dotted name parse: %+v", got)
	}
}

func TestSetTextValidation(t *testing.T) {
	d := testDraft(t)
	tests := []struct {
		name    string
		path    string
		idx     int
		text    string
		wantErr bool
		want    string
	}{
		{"int ok", "retry.max_attempts", -1, "5", false, "5"},
		{"int nan", "retry.max_attempts", -1, "x", true, "5"},
		{"int below min", "retry.max_attempts", -1, "0", true, "5"},
		{"int above max", "proxy.port", -1, "70000", true, "8080"},
		{"enum ok", "routing.strategy", -1, "least_busy", false, "least_busy"},
		{"enum bad", "routing.strategy", -1, "nope", true, "least_busy"},
		{"string", "proxy.host", -1, "127.0.0.1", false, "127.0.0.1"},
		{"item string", "backends[].url", 1, "http://q", false, "http://q"},
		{"name empty", "backends[].name", 0, " ", true, "gpu-1"},
		{"name dup", "backends[].name", 0, "gpu-2", true, "gpu-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := specOf(t, d, tt.path)
			err := d.SetText(spec, tt.idx, tt.text)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got := d.Text(spec, tt.idx); got != tt.want {
				t.Errorf("value = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestModelsListField(t *testing.T) {
	d := testDraft(t)
	spec := config.FieldSpec{Path: "backends[].models", Kind: config.KindString}
	if got := d.Text(spec, 0); got != "m1, m2" {
		t.Fatalf("Text = %q", got)
	}
	if err := d.SetText(spec, 0, " a, b ,, c "); err != nil {
		t.Fatal(err)
	}
	if got := d.Text(spec, 0); got != "a, b, c" {
		t.Errorf("Text = %q", got)
	}
	if !d.Modified(spec, 0) {
		t.Error("models should be modified")
	}
	p, err := d.BuildUpdate("r1")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Config.Backends[0].Models; len(got) != 3 || got[2] != "c" {
		t.Errorf("models in payload: %v", got)
	}
}

func TestDiffAndDirty(t *testing.T) {
	d := testDraft(t)
	if d.Dirty() {
		t.Fatal("fresh draft must be clean")
	}
	_ = d.SetText(specOf(t, d, "retry.max_attempts"), -1, "5")
	_ = d.SetText(specOf(t, d, "backends[].url"), 1, "http://new")
	d.Toggle(specOf(t, d, "discovery.enabled"), -1)
	got := strings.Join(d.Changes(), "\n")
	for _, want := range []string{
		"retry.max_attempts: 3",
		"backends[gpu-2].url: http://b:2",
		"discovery.enabled: true",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("changes missing %q in:\n%s", want, got)
		}
	}
	if !d.Modified(specOf(t, d, "retry.max_attempts"), -1) || d.Modified(specOf(t, d, "retry.max_backoff_ms"), -1) {
		t.Error("Modified markers wrong")
	}
	_ = d.SetText(specOf(t, d, "retry.max_attempts"), -1, "3")
	if strings.Contains(strings.Join(d.Changes(), "\n"), "max_attempts") {
		t.Error("reverted field still in diff")
	}
}

func TestRenameAndSecretOps(t *testing.T) {
	d := testDraft(t)
	nameSpec := specOf(t, d, "auth.api_keys[].name")
	keySpec := specOf(t, d, "auth.api_keys[].key")
	if err := d.SetText(nameSpec, 0, "crm2"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetSecretOp(keySpec, 0, secOpSet, "sk-new"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetSecretOp(specOf(t, d, config.PathAdminKey), -1, secOpGenerate, ""); err != nil {
		t.Fatal(err)
	}
	if err := d.SetSecretOp(specOf(t, d, "backends[].api_key"), 0, secOpClear, ""); err != nil {
		t.Fatal(err)
	}
	p, err := d.BuildUpdate("rev")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Renames) != 1 || p.Renames[0] != (config.Rename{List: "auth.api_keys", From: "crm", To: "crm2"}) {
		t.Errorf("renames = %+v", p.Renames)
	}
	wantOps := []config.SecretOp{
		{Path: "auth.api_keys[crm2].key", Op: "set", Value: "sk-new"},
		{Path: "auth.admin_key", Op: "generate"},
		{Path: "backends[gpu-1].api_key", Op: "clear"},
	}
	if len(p.SecretOps) != len(wantOps) {
		t.Fatalf("ops = %+v", p.SecretOps)
	}
	for i, w := range wantOps {
		if p.SecretOps[i] != w {
			t.Errorf("op[%d] = %+v, want %+v", i, p.SecretOps[i], w)
		}
	}
	if p.Revision != "rev" || p.Config.Auth.APIKeys[0].Name != "crm2" {
		t.Errorf("payload = %+v", p)
	}
	for _, c := range d.Changes() {
		if strings.Contains(c, "sk-new") {
			t.Errorf("secret value leaked in diff: %s", c)
		}
	}
	_ = d.SetSecretOp(keySpec, 0, "", "")
	if len(d.SecretOps()) != 2 {
		t.Errorf("undo failed: %+v", d.SecretOps())
	}
}

func TestAddClientAutoGenerate(t *testing.T) {
	d := testDraft(t)
	idx, err := d.AddItem(cfgListClients, "newbie")
	if err != nil || idx != 2 {
		t.Fatalf("AddItem = %d, %v", idx, err)
	}
	ops := d.SecretOps()
	if len(ops) != 1 || ops[0] != (config.SecretOp{Path: "auth.api_keys[newbie].key", Op: "generate"}) {
		t.Fatalf("ops = %+v", ops)
	}
	if d.ItemState(cfgListClients, idx) != "+" {
		t.Error("new item marker")
	}
	if _, err := d.AddItem(cfgListClients, "crm"); err == nil {
		t.Error("duplicate name must fail")
	}
	if err := d.SetSecretOp(specOf(t, d, "auth.api_keys[].key"), idx, secOpClear, ""); err == nil {
		t.Error("clear on a new item must fail")
	}
	_ = d.SetText(specOf(t, d, "auth.api_keys[].name"), idx, "newer")
	if got := d.SecretOps()[0].Path; got != "auth.api_keys[newer].key" {
		t.Errorf("op path = %s", got)
	}
	if len(d.Renames()) != 0 {
		t.Error("new item rename must not be a Rename")
	}
	if !strings.Contains(strings.Join(d.Changes(), "\n"), "+ auth.api_keys[newer]") {
		t.Errorf("changes = %v", d.Changes())
	}
	bi, err := d.AddItem(cfgListBackends, "gpu-3")
	if err != nil {
		t.Fatal(err)
	}
	if d.Text(specOf(t, d, "backends[].type"), bi) != "openai" {
		t.Error("backend default type")
	}
	if len(d.SecretOps()) != 1 {
		t.Errorf("backend add must not record ops: %+v", d.SecretOps())
	}
}

func TestDeleteItem(t *testing.T) {
	d := testDraft(t)
	_ = d.SetSecretOp(specOf(t, d, "auth.api_keys[].key"), 1, secOpSet, "x")
	d.DeleteItem(cfgListClients, 1)
	if d.itemCount(cfgListClients) != 1 || d.itemNameAt(cfgListClients, 0) != "crm" {
		t.Fatalf("delete failed")
	}
	if len(d.SecretOps()) != 0 {
		t.Error("ops of deleted item must be dropped")
	}
	if got := strings.Join(d.Changes(), "\n"); !strings.Contains(got, "- auth.api_keys[bot]: removed") {
		t.Errorf("changes = %s", got)
	}
	p, _ := d.BuildUpdate("r")
	if len(p.Config.Auth.APIKeys) != 1 {
		t.Errorf("payload keys = %+v", p.Config.Auth.APIKeys)
	}
}

func TestCycleEnum(t *testing.T) {
	d := testDraft(t)
	spec := specOf(t, d, "proxy.log_level")
	d.Cycle(spec, -1, -1)
	if got := d.Text(spec, -1); got != "debug" {
		t.Errorf("prev from info = %q", got)
	}
	d.Cycle(spec, -1, -1)
	if got := d.Text(spec, -1); got != "error" {
		t.Errorf("wrap = %q", got)
	}
}

// ---- key flow ---------------------------------------------------------------------

func kmsg(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "space":
		return tea.KeyMsg{Type: tea.KeySpace}
	case "ctrl+s":
		return tea.KeyMsg{Type: tea.KeyCtrlS}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func press(u *cfgUI, keys ...string) cfgAction {
	act := actNone
	for _, k := range keys {
		act = u.handleKey(kmsg(k))
	}
	return act
}

func loadedUI(t *testing.T) *cfgUI {
	t.Helper()
	u := newCfgUI()
	u.onState(ipc.ConfigStatePayload{Path: "/x/config.yaml", Revision: "abcdef123456", Config: testCfg(), Schema: config.ConfigSchema()})
	if !u.loaded {
		t.Fatalf("not loaded: %s", u.loadErr)
	}
	return u
}

// goSection moves the section cursor to the section with the given ID.
func goSection(t *testing.T, u *cfgUI, id string) {
	t.Helper()
	u.focusFields = false
	for i, s := range u.sections() {
		if s.ID == id {
			u.secIdx = i
			u.inItem, u.itemIdx, u.fieldIdx = false, 0, 0
			return
		}
	}
	t.Fatalf("no section %s", id)
}

func TestKeyFlowEditInt(t *testing.T) {
	u := loadedUI(t)
	goSection(t, u, "retry")
	press(u, "right", "enter") // focus fields, edit Max attempts
	if u.mode != cmEdit {
		t.Fatalf("mode = %v", u.mode)
	}
	// Typing captures q/s/c/1-4 as text.
	press(u, "backspace", "q", "s", "c", "1")
	if u.mode != cmEdit {
		t.Fatal("text keys must not leave edit mode")
	}
	if got := press(u, "enter"); got != actNone || u.mode != cmEdit || u.editErr == "" {
		t.Errorf("invalid int must keep editing with error (mode=%v err=%q)", u.mode, u.editErr)
	}
	for range 4 {
		press(u, "backspace")
	}
	press(u, "7", "enter")
	if u.mode != cmNormal {
		t.Fatalf("mode = %v err=%q", u.mode, u.editErr)
	}
	if v, _ := pathGet(u.draft.cur, "retry.max_attempts"); rawString(v) != "7" {
		t.Errorf("value = %v", v)
	}
	press(u, "enter", "9", "esc")
	if v, _ := pathGet(u.draft.cur, "retry.max_attempts"); rawString(v) != "7" {
		t.Errorf("esc must cancel, got %v", v)
	}
}

func TestKeyFlowEnumAndBool(t *testing.T) {
	u := loadedUI(t)
	goSection(t, u, "routing")
	press(u, "right", "right") // focus fields, cycle enum forward
	if v, _ := pathGet(u.draft.cur, "routing.strategy"); v == config.StrategyRoundRobin {
		t.Error("enum did not cycle on right")
	}
	press(u, "left")
	if v, _ := pathGet(u.draft.cur, "routing.strategy"); v != config.StrategyRoundRobin {
		t.Errorf("left should cycle back, got %v", v)
	}
	press(u, "enter")
	if v, _ := pathGet(u.draft.cur, "routing.strategy"); v == config.StrategyRoundRobin {
		t.Error("enter should cycle enum")
	}
	goSection(t, u, "discovery")
	press(u, "right", "space")
	if v, _ := pathGet(u.draft.cur, "discovery.enabled"); v != false {
		t.Errorf("space should toggle bool, got %v", v)
	}
	press(u, "enter")
	if v, _ := pathGet(u.draft.cur, "discovery.enabled"); v != true {
		t.Errorf("enter should toggle bool, got %v", v)
	}
}

func TestKeyFlowSaveReview(t *testing.T) {
	u := loadedUI(t)
	press(u, "ctrl+s")
	if u.toast != "No changes" || u.mode != cmNormal {
		t.Fatalf("toast=%q mode=%v", u.toast, u.mode)
	}
	goSection(t, u, "retry")
	press(u, "right", "enter", "backspace", "9", "enter")
	press(u, "ctrl+s")
	if u.mode != cmReview || len(u.review) != 1 {
		t.Fatalf("mode=%v review=%v", u.mode, u.review)
	}
	if got := press(u, "esc"); got != actNone || u.mode != cmNormal {
		t.Fatal("esc must return to the form")
	}
	press(u, "ctrl+s")
	if got := press(u, "enter"); got != actUpdate {
		t.Fatalf("action = %v", got)
	}
	if u.out.Revision != "abcdef123456" || u.out.Config.Retry.MaxAttempts != 9 {
		t.Errorf("payload = %+v", u.out)
	}
	act := u.onResult(ipc.ConfigResultPayload{OK: true, Applied: []string{"retry.max_attempts"}, RestartRequired: []string{"a", "b"}})
	if act != actGet || !strings.Contains(u.toast, "applied 1") || !strings.Contains(u.toast, "restart required 2") {
		t.Errorf("act=%v toast=%q", act, u.toast)
	}
}

func TestResultGeneratedErrorsConflict(t *testing.T) {
	u := loadedUI(t)
	u.mode = cmSaving
	u.onResult(ipc.ConfigResultPayload{OK: true, Generated: map[string]string{"auth.api_keys[b].key": "sk-proxy-b", "auth.api_keys[a].key": "sk-proxy-a"}})
	if u.mode != cmGenerated || len(u.generated) != 2 || u.generated[0].Path != "auth.api_keys[a].key" {
		t.Fatalf("generated modal: mode=%v %+v", u.mode, u.generated)
	}
	u.onState(ipc.ConfigStatePayload{Revision: "r2", Config: testCfg(), Schema: config.ConfigSchema()})
	if u.mode != cmGenerated {
		t.Error("state reload closed the modal")
	}
	press(u, "enter")
	if u.mode != cmNormal || u.generated != nil {
		t.Error("enter must close modal")
	}

	u.mode = cmSaving
	u.onResult(ipc.ConfigResultPayload{Errors: []config.FieldError{{Path: "backends[gpu-1].url", Msg: "bad url"}}})
	if u.mode != cmNormal || u.errs["backends[gpu-1].url"] != "bad url" {
		t.Fatalf("errors not recorded: %+v", u.errs)
	}
	spec := specOf(t, u.draft, "backends[].url")
	if u.fieldError(spec, 0) != "bad url" || u.fieldError(spec, 1) != "" {
		t.Error("fieldError mapping")
	}
	if !u.itemHasError("backends", 0) || u.itemHasError("backends", 1) {
		t.Error("itemHasError mapping")
	}

	u.onResult(ipc.ConfigResultPayload{Conflict: true})
	if !u.conflict || u.banner != cfgBannerConflict {
		t.Error("conflict banner")
	}
	if got := press(u, "r"); got != actGet {
		t.Errorf("r after conflict = %v", got)
	}
}

func TestChangedBannerAndDiscard(t *testing.T) {
	u := loadedUI(t)
	u.onChanged(ipc.ConfigChangedPayload{Revision: "abcdef123456"})
	if u.banner != "" {
		t.Error("same revision must not raise banner")
	}
	u.onChanged(ipc.ConfigChangedPayload{Revision: "other"})
	if u.banner != cfgBannerChanged {
		t.Error("banner not set")
	}
	if got := press(u, "esc"); got != actClose {
		t.Errorf("esc clean = %v", got)
	}
	goSection(t, u, "retry")
	press(u, "right", "enter", "backspace", "9", "enter")
	if got := press(u, "esc"); got != actNone || u.mode != cmDiscard {
		t.Fatalf("esc dirty: act=%v mode=%v", got, u.mode)
	}
	if got := press(u, "n"); got != actNone || u.mode != cmNormal {
		t.Error("n must keep editing")
	}
	press(u, "q")
	if got := press(u, "y"); got != actClose {
		t.Errorf("y = %v", got)
	}
	u2 := loadedUI(t)
	goSection(t, u2, "retry")
	press(u2, "right", "enter", "backspace", "9", "enter")
	if got := press(u2, "r"); got != actNone {
		t.Error("r must not discard silently")
	}
}

func TestKeyFlowSecretMenu(t *testing.T) {
	u := loadedUI(t)
	goSection(t, u, "admin")
	press(u, "right", "enter")
	if u.mode != cmSecretMenu {
		t.Fatalf("mode = %v", u.mode)
	}
	press(u, "g")
	if u.mode != cmNormal || len(u.draft.SecretOps()) != 1 || u.draft.SecretOps()[0].Op != "generate" {
		t.Fatalf("generate: %+v", u.draft.SecretOps())
	}
	press(u, "enter", "t", "h", "i", "enter")
	ops := u.draft.SecretOps()
	if len(ops) != 1 || ops[0].Op != "set" || ops[0].Value != "hi" {
		t.Errorf("type new: %+v", ops)
	}
	press(u, "enter", "c")
	if u.draft.SecretOps()[0].Op != "clear" {
		t.Error("clear")
	}
	press(u, "enter", "esc")
	if u.mode != cmNormal {
		t.Error("esc from menu")
	}
}

func TestKeyFlowAddDeleteClient(t *testing.T) {
	u := loadedUI(t)
	goSection(t, u, "clients")
	press(u, "a")
	if u.mode != cmAdd {
		t.Fatalf("mode = %v", u.mode)
	}
	press(u, "n", "e", "w", "enter")
	if u.mode != cmNormal || u.draft.itemCount(cfgListClients) != 3 {
		t.Fatalf("add failed: mode=%v err=%q", u.mode, u.editErr)
	}
	if ops := u.draft.SecretOps(); len(ops) != 1 || ops[0].Op != "generate" {
		t.Errorf("auto generate: %+v", ops)
	}
	press(u, "d")
	if u.mode != cmDelete {
		t.Fatalf("mode = %v", u.mode)
	}
	press(u, "y")
	if u.draft.itemCount(cfgListClients) != 2 || len(u.draft.SecretOps()) != 0 {
		t.Error("delete failed")
	}
	press(u, "a", "c", "r", "m", "enter")
	if u.mode != cmAdd || u.editErr == "" {
		t.Error("duplicate must show an error")
	}
	press(u, "esc")
	goSection(t, u, "backends")
	press(u, "a", "g", "3", "enter")
	if !u.inItem || u.itemIdx != 2 {
		t.Errorf("inItem=%v idx=%d", u.inItem, u.itemIdx)
	}
	press(u, "esc")
	if u.inItem {
		t.Error("esc must leave the item form")
	}
}

// ---- render -------------------------------------------------------------------------

func renderCfg(u *cfgUI, pending []string, w, h int) string {
	return renderConfigOverlay(cfgView{UI: u, RestartPending: pending}, w, h)
}

func TestRenderLoadingAndError(t *testing.T) {
	u := newCfgUI()
	if out := renderCfg(u, nil, 100, 30); !strings.Contains(out, "Loading") {
		t.Errorf("loading missing:\n%s", out)
	}
	u.onState(ipc.ConfigStatePayload{Error: "boom"})
	if out := renderCfg(u, nil, 100, 30); !strings.Contains(out, "boom") {
		t.Errorf("error missing:\n%s", out)
	}
}

func TestRenderSectionAndList(t *testing.T) {
	u := loadedUI(t)
	goSection(t, u, "retry")
	_ = u.draft.SetText(specOf(t, u.draft, "retry.max_attempts"), -1, "5")
	out := renderCfg(u, []string{"proxy.port", "backends[gpu-2]"}, 100, 30)
	for _, want := range []string{"Config", "/x/config.yaml", "rev abcdef1", "CLIENTS (2)", "BACKENDS (2)", "1 unsaved", "was 3", "live", "Max attempts", "5", "restart pending: proxy.port, backends[gpu-2]"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	goSection(t, u, "proxy")
	out = renderCfg(u, nil, 100, 30)
	if !strings.Contains(out, "⟳") {
		t.Errorf("restart marker missing:\n%s", out)
	}
	goSection(t, u, "backends")
	u.focusFields = true
	out = renderCfg(u, nil, 100, 30)
	if !strings.Contains(out, "gpu-1") || !strings.Contains(out, "gpu-2") {
		t.Errorf("list rows missing:\n%s", out)
	}
	press(u, "enter")
	out = renderCfg(u, nil, 100, 30)
	if !strings.Contains(out, "URL") || !strings.Contains(out, "http://a:1") || !strings.Contains(out, "••••••••key1") {
		t.Errorf("item form:\n%s", out)
	}
}

func TestRenderErrorsAndDialogs(t *testing.T) {
	u := loadedUI(t)
	goSection(t, u, "backends")
	u.focusFields = true
	press(u, "enter")
	u.onResult(ipc.ConfigResultPayload{Errors: []config.FieldError{{Path: "backends[gpu-1].url", Msg: "invalid URL"}}})
	if out := renderCfg(u, nil, 100, 30); !strings.Contains(out, "invalid URL") {
		t.Errorf("field error missing:\n%s", out)
	}
	u.mode = cmGenerated
	u.generated = []genEntry{{Path: "auth.api_keys[x].key", Key: "sk-proxy-FULLKEY"}}
	out := renderCfg(u, nil, 100, 30)
	if !strings.Contains(out, "sk-proxy-FULLKEY") || !strings.Contains(out, "press v later") {
		t.Errorf("generated modal:\n%s", out)
	}
	u.mode = cmReview
	u.review = []string{"retry.max_attempts: 3 -> 5"}
	if out := renderCfg(u, nil, 100, 30); !strings.Contains(out, "retry.max_attempts: 3 -> 5") {
		t.Errorf("review:\n%s", out)
	}
	u.mode = cmDiscard
	if out := renderCfg(u, nil, 100, 30); !strings.Contains(out, "Discard changes? y/n") {
		t.Errorf("discard:\n%s", out)
	}
	u.mode = cmNormal
	u.banner = cfgBannerChanged
	if out := renderCfg(u, nil, 100, 30); !strings.Contains(out, "Changed by another client") {
		t.Errorf("banner:\n%s", out)
	}
}

func TestRenderNarrowAndNoUnicode(t *testing.T) {
	old := noUnicodeFlag
	noUnicodeFlag = true
	t.Cleanup(func() { noUnicodeFlag = old })

	u := loadedUI(t)
	goSection(t, u, "proxy")
	u.focusFields = true
	out := renderCfg(u, []string{"proxy.port"}, 80, 24)
	if !strings.Contains(out, "R restart pending") {
		t.Errorf("ascii restart badge missing:\n%s", out)
	}
	if strings.ContainsAny(out, "⟳▸‹›") {
		t.Errorf("unicode glyph in no-unicode render:\n%s", out)
	}
	for i, line := range strings.Split(out, "\n") {
		if w := len([]rune(stripANSI(line))); w > 80 {
			t.Errorf("line %d wider than 80: %d", i, w)
		}
	}
	if !strings.Contains(restartBadge([]string{"a", "b"}), "(2)") {
		t.Error("badge count")
	}
}

func stripANSI(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == 0x1b:
			in = true
		case in && r == 'm':
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func TestRestartBadgeEmpty(t *testing.T) {
	if restartBadge(nil) != "" {
		t.Error("empty pending must give no badge")
	}
}

func TestAppConfigOverlayFlow(t *testing.T) {
	m := &model{width: 100, height: 30, requests: map[string]ipc.RequestState{}, flashMap: map[string]flashEntry{}}
	_, cmd := m.handleKey(kmsg("c"))
	if !m.showConfig || cmd == nil {
		t.Fatalf("c must open the overlay and request config (cmd=%v)", cmd != nil)
	}
	if out := m.View(); !strings.Contains(out, "Loading") {
		t.Errorf("loading expected:\n%s", out)
	}
	payload := mustJSON(t, ipc.ConfigStatePayload{Path: "/x/c.yaml", Revision: "r1", Config: testCfg(), Schema: config.ConfigSchema(), RestartPending: []string{"proxy.port"}})
	if err := m.handleEnvelope(ipc.Envelope{Type: ipc.TypeConfigState}, payload); err != nil {
		t.Fatal(err)
	}
	if !m.cfg.loaded || len(m.restartPending) != 1 {
		t.Fatalf("state not applied")
	}
	// config_changed from another client raises the banner.
	ch := mustJSON(t, ipc.ConfigChangedPayload{Revision: "r2", RestartPending: []string{"a", "b"}})
	_ = m.handleEnvelope(ipc.Envelope{Type: ipc.TypeConfigChanged}, ch)
	if m.cfg.banner != cfgBannerChanged || len(m.restartPending) != 2 {
		t.Error("changed banner / restart pending")
	}
	// A successful result triggers a reload cmd.
	res := mustJSON(t, ipc.ConfigResultPayload{OK: true})
	_ = m.handleEnvelope(ipc.Envelope{Type: ipc.TypeConfigResult}, res)
	if m.extraCmd == nil || len(m.restartPending) != 0 {
		t.Error("result must schedule reload and clear restart pending")
	}
	// q closes the overlay when clean.
	_, _ = m.handleKey(kmsg("q"))
	if m.showConfig {
		t.Error("q must close the overlay")
	}
	m.restartPending = []string{"x"}
	if out := m.View(); !strings.Contains(out, "restart pending (1)") {
		t.Errorf("badge missing on main screen:\n%s", out)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func renderWith(u *cfgUI, servers []ipc.ServerState, w, h int) string {
	return renderConfigOverlay(cfgView{UI: u, Servers: servers}, w, h)
}

func TestRenderSizesAndASCII(t *testing.T) {
	tests := []struct {
		name    string
		w, h    int
		ascii   bool
		want    []string
		notWant []string
	}{
		{"wide", 140, 40, false, []string{"SERVER", "BEHAVIOUR", "Retry", "live", "Max attempts"}, nil},
		{"80x24", 80, 24, false, []string{"SERVER", "Retry"}, nil},
		{"narrow tab bar", 60, 24, false, []string{"Retry"}, []string{"BEHAVIOUR"}},
		{"ascii", 100, 30, true, []string{"SERVER", "Retry"}, []string{"⟳", "●", "•", "›", "▸"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			old := noUnicodeFlag
			noUnicodeFlag = tc.ascii
			t.Cleanup(func() { noUnicodeFlag = old })
			u := loadedUI(t)
			goSection(t, u, "retry")
			u.focusFields = true
			_ = u.draft.SetText(specOf(t, u.draft, "retry.max_attempts"), -1, "5")
			out := renderWith(u, nil, tc.w, tc.h)
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("missing %q in:\n%s", w, out)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(out, w) {
					t.Errorf("unexpected %q in:\n%s", w, out)
				}
			}
		})
	}
}

func TestRenderValueKinds(t *testing.T) {
	u := loadedUI(t)
	goSection(t, u, "retry")
	u.focusFields = true
	out := renderWith(u, nil, 140, 40)
	for _, w := range []string{"500 ms", "8000 ms", "Total attempts per request", "1" + "–" + "10"} {
		if w == "1–10" {
			continue // range depends on the schema bounds
		}
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
	goSection(t, u, "proxy")
	u.focusFields = true
	out = renderWith(u, nil, 140, 40)
	for _, w := range []string{"debug", "info", "warn", "error"} {
		if !strings.Contains(out, w) {
			t.Errorf("enum options missing %q in:\n%s", w, out)
		}
	}
	goSection(t, u, "discovery")
	u.focusFields = true
	if out = renderWith(u, nil, 140, 40); !strings.Contains(out, "[x] on") {
		t.Errorf("bool on missing:\n%s", out)
	}
	press(u, "enter") // toggles the first field (enabled)
	if out = renderWith(u, nil, 140, 40); !strings.Contains(out, "[ ] off") || !strings.Contains(out, "was true") {
		t.Errorf("bool off / was missing:\n%s", out)
	}
}

func TestSectionBadges(t *testing.T) {
	u := loadedUI(t)
	_ = u.draft.SetText(specOf(t, u.draft, "proxy.port"), -1, "9090")
	u.onResult(ipc.ConfigResultPayload{Errors: []config.FieldError{{Path: "retry.max_attempts", Msg: "bad"}}})
	out := renderWith(u, nil, 140, 40)
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(strings.TrimLeft(line, "│"), "  Proxy"):
			if !strings.Contains(line, "●") || !strings.Contains(line, "⟳") {
				t.Errorf("proxy badges (modified+restart): %q", line)
			}
		case strings.HasPrefix(strings.TrimLeft(line, "│"), "  Retry"):
			if !strings.Contains(line, "!") {
				t.Errorf("retry error badge: %q", line)
			}
		}
	}
}

func TestSecretReveal(t *testing.T) {
	u := loadedUI(t)
	goSection(t, u, "clients")
	u.focusFields = true
	if act := press(u, "y"); act != actNone || u.toast != "press v to reveal first" {
		t.Fatalf("y without reveal: act=%v toast=%q", act, u.toast)
	}
	if act := press(u, "v"); act != actReveal || u.revealWant != "auth.api_keys[crm].key" {
		t.Fatalf("v: act=%v want=%q", act, u.revealWant)
	}
	u.onSecretValue(ipc.SecretValuePayload{Path: "auth.api_keys[crm].key", Value: "sk-FULL-CRM"})
	if out := renderWith(u, nil, 140, 40); !strings.Contains(out, "sk-FULL-CRM") {
		t.Errorf("revealed value not rendered:\n%s", out)
	}
	if act := press(u, "y"); act != actCopy || u.copyText != "sk-FULL-CRM" {
		t.Errorf("y copy: act=%v", act)
	}
	press(u, "down")
	if len(u.revealed) != 0 {
		t.Error("moving the cursor must hide the secret")
	}
	if out := renderWith(u, nil, 140, 40); strings.Contains(out, "sk-FULL-CRM") {
		t.Error("secret still rendered after move")
	}
	// Error frame -> toast, nothing stored.
	press(u, "v")
	u.onSecretValue(ipc.SecretValuePayload{Path: "auth.api_keys[bot].key", Error: "denied"})
	if u.toast != "Error: denied" || len(u.revealed) != 0 {
		t.Errorf("error handling: toast=%q", u.toast)
	}
	// Item form: v on a backend api_key, section change hides.
	goSection(t, u, "backends")
	u.focusFields = true
	press(u, "enter")
	u.revealed["x"] = "y"
	press(u, "tab")
	if len(u.revealed) != 0 {
		t.Error("pane switch must hide secrets")
	}
}

func TestSecretValueFrameInApp(t *testing.T) {
	m := &model{width: 100, height: 30, requests: map[string]ipc.RequestState{}, flashMap: map[string]flashEntry{}}
	m.showConfig, m.cfg = true, loadedUI(t)
	goSection(t, m.cfg, "clients")
	m.cfg.focusFields = true
	_, cmd := m.handleKey(kmsg("v"))
	if cmd == nil || m.cfg.revealWant == "" {
		t.Fatal("v must emit a reveal cmd")
	}
	p := mustJSON(t, ipc.SecretValuePayload{Path: m.cfg.revealWant, Value: "sk-X"})
	if err := m.handleEnvelope(ipc.Envelope{Type: ipc.TypeSecretValue}, p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.View(), "sk-X") {
		t.Error("revealed value missing from the view")
	}
}

func TestReviewTableAndGenerated(t *testing.T) {
	u := loadedUI(t)
	_ = u.draft.SetText(specOf(t, u.draft, "retry.max_attempts"), -1, "5")
	_ = u.draft.SetSecretOp(specOf(t, u.draft, "auth.admin_key"), -1, secOpGenerate, "")
	press(u, "ctrl+s")
	out := renderWith(u, nil, 120, 30)
	for _, w := range []string{"Path", "Apply", "retry.max_attempts", "3 → 5", "auth.admin_key", "changed", "live"} {
		if !strings.Contains(out, w) {
			t.Errorf("review missing %q:\n%s", w, out)
		}
	}
	u.mode = cmGenerated
	u.generated = []genEntry{{Path: "auth.admin_key", Key: "sk-GEN"}}
	out = renderWith(u, nil, 120, 30)
	if !strings.Contains(out, "sk-GEN") || !strings.Contains(out, "y copy") {
		t.Errorf("generated modal:\n%s", out)
	}
	if act := press(u, "y"); act != actCopy || u.copyText != "sk-GEN" {
		t.Errorf("generated copy: %v", act)
	}
}

func TestBackendsTableHealth(t *testing.T) {
	u := loadedUI(t)
	goSection(t, u, "backends")
	u.focusFields = true
	out := renderWith(u, []ipc.ServerState{{Name: "gpu-1", Healthy: true}, {Name: "gpu-2", Healthy: false}}, 140, 40)
	for _, w := range []string{"Name", "URL", "Protocol", "Models", "Health", "● up", "● down", "http://a:1"} {
		if !strings.Contains(out, w) {
			t.Errorf("backends table missing %q:\n%s", w, out)
		}
	}
	out = renderWith(u, nil, 140, 40)
	if strings.Contains(out, "● up") || strings.Contains(out, "● down") {
		t.Errorf("unknown health must render as a dash:\n%s", out)
	}
	press(u, "enter")
	if out = renderWith(u, nil, 140, 40); !strings.Contains(out, "Backends › gpu-1") {
		t.Errorf("breadcrumb missing:\n%s", out)
	}
}
