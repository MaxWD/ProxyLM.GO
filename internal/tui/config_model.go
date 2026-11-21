package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"proxylm/internal/config"
	"proxylm/internal/ipc"
)

// Pure model logic of the Config editor overlay (key c): the editable draft,
// path accessors, diff, secret operations, renames and list add/delete.
// No I/O and no styling. The draft is a JSON-map round-trip of *config.Config,
// so every field is reachable by the schema path without reflection.

// Config list IDs (as used in Section.List and Rename.List).
const (
	cfgListClients  = "auth.api_keys"
	cfgListBackends = "backends"
)

// Secret operation names.
const (
	secOpSet      = "set"
	secOpGenerate = "generate"
	secOpClear    = "clear"
)

// itemMeta tracks the identity of one list item across renames/deletes.
type itemMeta struct {
	ID   int
	Orig string // name in the loaded config; "" for a new item
	New  bool
}

// secretOp is a recorded secret operation. For list items it is addressed by
// the stable item ID (the name can change before the save).
type secretOp struct {
	list  string // "" for non-list fields
	id    int
	field string // leaf key for list items, full path otherwise
	op    string
	value string
}

// cfgDraft is the editable copy of the daemon config plus pending operations.
type cfgDraft struct {
	schema config.Schema
	orig   map[string]any
	cur    map[string]any
	meta   map[string][]itemMeta // list ID -> metadata parallel to cur items
	nextID int
	ops    map[string]secretOp
	order  []string // insertion order of ops keys
}

// toJSONMap converts a Config into a generic JSON map (numbers as json.Number).
func toJSONMap(cfg *config.Config) (map[string]any, error) {
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	return decodeMap(b)
}

func decodeMap(b []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// newCfgDraft builds a draft from the loaded config.
func newCfgDraft(cfg *config.Config, schema config.Schema) (*cfgDraft, error) {
	if cfg == nil {
		return nil, fmt.Errorf("no config")
	}
	orig, err := toJSONMap(cfg)
	if err != nil {
		return nil, err
	}
	cur, err := toJSONMap(cfg)
	if err != nil {
		return nil, err
	}
	d := &cfgDraft{
		schema: schema, orig: orig, cur: cur,
		meta: map[string][]itemMeta{}, ops: map[string]secretOp{},
	}
	for _, s := range schema.Sections {
		if s.List == "" {
			continue
		}
		for _, it := range listItems(cur, s.List) {
			name := itemName(it)
			d.nextID++
			d.meta[s.List] = append(d.meta[s.List], itemMeta{ID: d.nextID, Orig: name})
		}
	}
	return d, nil
}

// ---- path parsing and access -------------------------------------------------

type pathSeg struct {
	key    string
	sel    string
	hasSel bool
}

// parsePath splits "auth.api_keys[crm].key" into segments, honouring brackets
// (item names may contain dots).
func parsePath(p string) []pathSeg {
	var segs []pathSeg
	var cur pathSeg
	var sel strings.Builder
	inSel := false
	flush := func() {
		segs = append(segs, cur)
		cur = pathSeg{}
	}
	for _, r := range p {
		switch {
		case inSel:
			if r == ']' {
				cur.sel = sel.String()
				cur.hasSel = true
				sel.Reset()
				inSel = false
			} else {
				sel.WriteRune(r)
			}
		case r == '[':
			inSel = true
		case r == '.':
			flush()
		default:
			cur.key += string(r)
		}
	}
	flush()
	return segs
}

func itemName(it any) string {
	if m, ok := it.(map[string]any); ok {
		if s, ok := m["name"].(string); ok {
			return s
		}
	}
	return ""
}

// findItem looks an item up by name or "#index".
func findItem(items []any, sel string) (map[string]any, int) {
	if strings.HasPrefix(sel, "#") {
		if n, err := strconv.Atoi(sel[1:]); err == nil && n >= 0 && n < len(items) {
			m, _ := items[n].(map[string]any)
			return m, n
		}
		return nil, -1
	}
	for i, it := range items {
		if itemName(it) == sel {
			m, _ := it.(map[string]any)
			return m, i
		}
	}
	return nil, -1
}

// resolvePath walks to the parent map of the last segment.
func resolvePath(root map[string]any, path string, create bool) (map[string]any, string, bool) {
	segs := parsePath(path)
	cur := root
	for i, sg := range segs {
		last := i == len(segs)-1
		if last && !sg.hasSel {
			return cur, sg.key, true
		}
		if sg.hasSel {
			items, _ := cur[sg.key].([]any)
			it, _ := findItem(items, sg.sel)
			if it == nil {
				return nil, "", false
			}
			if last {
				return cur, sg.key, true
			}
			cur = it
			continue
		}
		next, ok := cur[sg.key].(map[string]any)
		if !ok {
			if !create {
				return nil, "", false
			}
			next = map[string]any{}
			cur[sg.key] = next
		}
		cur = next
	}
	return nil, "", false
}

// pathGet returns the value at a concrete path ("retry.max_attempts",
// "backends[gpu-1].url"); a whole item path returns the item map.
func pathGet(root map[string]any, path string) (any, bool) {
	parent, leaf, ok := resolvePath(root, path, false)
	if !ok {
		return nil, false
	}
	segs := parsePath(path)
	if last := segs[len(segs)-1]; last.hasSel {
		items, _ := parent[leaf].([]any)
		it, _ := findItem(items, last.sel)
		return it, it != nil
	}
	v, ok := parent[leaf]
	return v, ok
}

// pathSet sets a leaf value at a concrete path.
func pathSet(root map[string]any, path string, v any) error {
	parent, leaf, ok := resolvePath(root, path, true)
	if !ok {
		return fmt.Errorf("path not found: %s", path)
	}
	if segs := parsePath(path); segs[len(segs)-1].hasSel {
		return fmt.Errorf("cannot set a whole item: %s", path)
	}
	parent[leaf] = v
	return nil
}

// listItems returns the items of a list ("backends", "auth.api_keys").
func listItems(root map[string]any, list string) []any {
	parent, leaf, ok := resolvePath(root, list, false)
	if !ok {
		return nil
	}
	items, _ := parent[leaf].([]any)
	return items
}

func setListItems(root map[string]any, list string, items []any) {
	if parent, leaf, ok := resolvePath(root, list, true); ok {
		parent[leaf] = items
	}
}

// splitSpecPath splits "backends[].url" into list "backends" and leaf "url".
func splitSpecPath(p string) (list, leaf string, isItem bool) {
	if i := strings.Index(p, "[]."); i >= 0 {
		return p[:i], p[i+3:], true
	}
	return "", p, false
}

// concretePath turns "backends[].url" + name into "backends[gpu-1].url".
func concretePath(specPath, name string) string {
	return strings.Replace(specPath, "[]", "["+name+"]", 1)
}

// rawString renders a JSON value as editable text.
func rawString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			parts = append(parts, rawString(e))
		}
		return strings.Join(parts, ", ")
	}
	return fmt.Sprint(v)
}

// ---- field access by (spec, item index) -----------------------------------------

// itemCount returns the number of items in a list.
func (d *cfgDraft) itemCount(list string) int { return len(listItems(d.cur, list)) }

// itemNameAt returns the current name of item idx.
func (d *cfgDraft) itemNameAt(list string, idx int) string {
	items := listItems(d.cur, list)
	if idx < 0 || idx >= len(items) {
		return ""
	}
	return itemName(items[idx])
}

// fieldPath returns the concrete path of a field (for a list item: by name,
// fallback "#idx" when the name is empty).
func (d *cfgDraft) fieldPath(spec config.FieldSpec, idx int) string {
	list, _, isItem := splitSpecPath(spec.Path)
	if !isItem {
		return spec.Path
	}
	name := d.itemNameAt(list, idx)
	if name == "" {
		name = "#" + strconv.Itoa(idx)
	}
	return concretePath(spec.Path, name)
}

// target returns the map and leaf key holding the field in root.
func (d *cfgDraft) target(root map[string]any, spec config.FieldSpec, idx int) (map[string]any, string, bool) {
	list, leaf, isItem := splitSpecPath(spec.Path)
	if !isItem {
		parent, l, ok := resolvePath(root, spec.Path, true)
		return parent, l, ok
	}
	its := listItems(root, list)
	if idx < 0 || idx >= len(its) {
		return nil, "", false
	}
	m, ok := its[idx].(map[string]any)
	return m, leaf, ok
}

func (d *cfgDraft) curRaw(spec config.FieldSpec, idx int) any {
	m, leaf, ok := d.target(d.cur, spec, idx)
	if !ok {
		return nil
	}
	return m[leaf]
}

// origRaw returns the loaded value; ok=false for new items.
func (d *cfgDraft) origRaw(spec config.FieldSpec, idx int) (any, bool) {
	list, leaf, isItem := splitSpecPath(spec.Path)
	if !isItem {
		parent, l, ok := resolvePath(d.orig, spec.Path, false)
		if !ok {
			return nil, true
		}
		return parent[l], true
	}
	metas := d.meta[list]
	if idx < 0 || idx >= len(metas) || metas[idx].New {
		return nil, false
	}
	m, _ := findItem(listItems(d.orig, list), metas[idx].Orig)
	if m == nil {
		return nil, false
	}
	return m[leaf], true
}

// isListValue reports whether the field is a []string edited as comma text.
func (d *cfgDraft) isListValue(spec config.FieldSpec, idx int) bool {
	if _, ok := d.curRaw(spec, idx).([]any); ok {
		return true
	}
	_, leaf, _ := splitSpecPath(spec.Path)
	return leaf == "models"
}

// Text returns the editable text of a field.
func (d *cfgDraft) Text(spec config.FieldSpec, idx int) string {
	return rawString(d.curRaw(spec, idx))
}

// SetText parses text per the field kind and stores it (client-side checks:
// int parse, Min/Max, enum membership, non-empty unique names).
func (d *cfgDraft) SetText(spec config.FieldSpec, idx int, text string) error {
	m, leaf, ok := d.target(d.cur, spec, idx)
	if !ok {
		return fmt.Errorf("field not found: %s", spec.Path)
	}
	list, _, isItem := splitSpecPath(spec.Path)
	switch string(spec.Kind) {
	case "int":
		text = strings.TrimSpace(text)
		n, err := strconv.Atoi(text)
		if err != nil {
			return fmt.Errorf("must be an integer")
		}
		if spec.Min != nil && n < *spec.Min {
			return fmt.Errorf("must be >= %d", *spec.Min)
		}
		if spec.Max != nil && n > *spec.Max {
			return fmt.Errorf("must be <= %d", *spec.Max)
		}
		m[leaf] = json.Number(strconv.Itoa(n))
	case "enum":
		for _, e := range spec.Enum {
			if e == text {
				m[leaf] = text
				return nil
			}
		}
		return fmt.Errorf("must be one of: %s", strings.Join(spec.Enum, ", "))
	case "bool":
		b, err := strconv.ParseBool(strings.TrimSpace(text))
		if err != nil {
			return fmt.Errorf("must be true or false")
		}
		m[leaf] = b
	default:
		if isItem && leaf == "name" {
			text = strings.TrimSpace(text)
			if err := d.checkName(list, text, idx); err != nil {
				return err
			}
			m[leaf] = text
			return nil
		}
		if d.isListValue(spec, idx) {
			var out []any
			for _, p := range strings.Split(text, ",") {
				if p = strings.TrimSpace(p); p != "" {
					out = append(out, p)
				}
			}
			m[leaf] = out
			return nil
		}
		m[leaf] = text
	}
	return nil
}

// checkName validates a list item name: non-empty and unique (except self).
func (d *cfgDraft) checkName(list, name string, self int) error {
	if name == "" {
		return fmt.Errorf("name must not be empty")
	}
	for i, it := range listItems(d.cur, list) {
		if i != self && itemName(it) == name {
			return fmt.Errorf("name %q already exists", name)
		}
	}
	return nil
}

// Toggle flips a bool field.
func (d *cfgDraft) Toggle(spec config.FieldSpec, idx int) {
	if m, leaf, ok := d.target(d.cur, spec, idx); ok {
		b, _ := m[leaf].(bool)
		m[leaf] = !b
	}
}

// Cycle moves an enum field by dir (+1/-1), wrapping around.
func (d *cfgDraft) Cycle(spec config.FieldSpec, idx, dir int) {
	n := len(spec.Enum)
	if n == 0 {
		return
	}
	m, leaf, ok := d.target(d.cur, spec, idx)
	if !ok {
		return
	}
	cur := rawString(m[leaf])
	pos := -1
	for i, e := range spec.Enum {
		if e == cur {
			pos = i
		}
	}
	if pos < 0 && dir < 0 {
		pos = 0
	}
	m[leaf] = spec.Enum[((pos+dir)%n+n)%n]
}

// ---- secrets ---------------------------------------------------------------------

func (d *cfgDraft) opKey(spec config.FieldSpec, idx int) (string, secretOp, bool) {
	list, leaf, isItem := splitSpecPath(spec.Path)
	if !isItem {
		return "op:" + spec.Path, secretOp{field: spec.Path}, true
	}
	metas := d.meta[list]
	if idx < 0 || idx >= len(metas) {
		return "", secretOp{}, false
	}
	return fmt.Sprintf("op:%s|%d|%s", list, metas[idx].ID, leaf), secretOp{list: list, id: metas[idx].ID, field: leaf}, true
}

// SetSecretOp records (or, with op == "", removes) a secret operation.
// A new item cannot "clear" a secret that does not exist yet.
func (d *cfgDraft) SetSecretOp(spec config.FieldSpec, idx int, op, value string) error {
	key, base, ok := d.opKey(spec, idx)
	if !ok {
		return fmt.Errorf("field not found: %s", spec.Path)
	}
	if op == "" {
		d.dropOp(key)
		return nil
	}
	if op == secOpClear {
		if list, _, isItem := splitSpecPath(spec.Path); isItem && d.meta[list][idx].New {
			return fmt.Errorf("new item: use type or generate")
		}
	}
	base.op, base.value = op, value
	if _, exists := d.ops[key]; !exists {
		d.order = append(d.order, key)
	}
	d.ops[key] = base
	return nil
}

func (d *cfgDraft) dropOp(key string) {
	if _, ok := d.ops[key]; !ok {
		return
	}
	delete(d.ops, key)
	for i, k := range d.order {
		if k == key {
			d.order = append(d.order[:i], d.order[i+1:]...)
			break
		}
	}
}

// SecretOpFor returns the pending op of a secret field ("" if none).
func (d *cfgDraft) SecretOpFor(spec config.FieldSpec, idx int) string {
	key, _, ok := d.opKey(spec, idx)
	if !ok {
		return ""
	}
	return d.ops[key].op
}

// ---- list add/delete ---------------------------------------------------------------

// AddItem appends a new item with the given name and returns its index. A new
// client automatically gets a "generate" op for each of its secret fields.
func (d *cfgDraft) AddItem(list, name string) (int, error) {
	name = strings.TrimSpace(name)
	if err := d.checkName(list, name, -1); err != nil {
		return -1, err
	}
	item := map[string]any{"name": name}
	switch list {
	case cfgListBackends:
		item["type"] = "openai"
		item["priority"] = json.Number("1")
		item["timeout_seconds"] = json.Number("600")
	}
	items := append(listItems(d.cur, list), item)
	setListItems(d.cur, list, items)
	d.nextID++
	idx := len(items) - 1
	d.meta[list] = append(d.meta[list], itemMeta{ID: d.nextID, New: true})
	if list == cfgListClients {
		for _, f := range d.schema.Fields {
			if string(f.Kind) == "secret" && strings.HasPrefix(f.Path, list+"[].") {
				_ = d.SetSecretOp(f, idx, secOpGenerate, "")
			}
		}
	}
	return idx, nil
}

// DeleteItem removes an item and its pending secret ops.
func (d *cfgDraft) DeleteItem(list string, idx int) {
	items := listItems(d.cur, list)
	metas := d.meta[list]
	if idx < 0 || idx >= len(items) || idx >= len(metas) {
		return
	}
	id := metas[idx].ID
	for k, op := range d.ops {
		if op.list == list && op.id == id {
			d.dropOp(k)
		}
	}
	out := append([]any(nil), items[:idx]...)
	setListItems(d.cur, list, append(out, items[idx+1:]...))
	d.meta[list] = append(append([]itemMeta(nil), metas[:idx]...), metas[idx+1:]...)
}

// ItemState reports "+" (new), "*" (modified or renamed) or "" for a list item.
func (d *cfgDraft) ItemState(list string, idx int) string {
	metas := d.meta[list]
	if idx < 0 || idx >= len(metas) {
		return ""
	}
	if metas[idx].New {
		return "+"
	}
	for _, f := range d.schema.Fields {
		if l, _, isItem := splitSpecPath(f.Path); isItem && l == list && d.Modified(f, idx) {
			return "*"
		}
	}
	return ""
}

// ---- modification tracking and diff ---------------------------------------------------

// Modified reports whether a field differs from the loaded config.
func (d *cfgDraft) Modified(spec config.FieldSpec, idx int) bool {
	if string(spec.Kind) == "secret" {
		return d.SecretOpFor(spec, idx) != ""
	}
	old, ok := d.origRaw(spec, idx)
	if !ok {
		return false
	}
	return rawString(old) != rawString(d.curRaw(spec, idx))
}

// Changes returns the human-readable change list for the review dialog.
func (d *cfgDraft) Changes() []string {
	var out []string
	arrow := glyph("→", "->")
	for _, f := range d.schema.Fields {
		if _, _, isItem := splitSpecPath(f.Path); isItem || !d.Modified(f, -1) {
			continue
		}
		out = append(out, d.changeLine(f, -1, arrow))
	}
	for _, s := range d.schema.Sections {
		if s.List == "" {
			continue
		}
		metas := d.meta[s.List]
		kept := map[string]bool{}
		for i, mt := range metas {
			name := d.itemNameAt(s.List, i)
			if mt.New {
				out = append(out, fmt.Sprintf("+ %s[%s]: added", s.List, name))
			} else {
				kept[mt.Orig] = true
				if mt.Orig != name {
					out = append(out, fmt.Sprintf("%s[%s]: renamed %s %s", s.List, mt.Orig, arrow, name))
				}
			}
			for _, f := range d.schema.Fields {
				l, leaf, isItem := splitSpecPath(f.Path)
				if !isItem || l != s.List || !d.Modified(f, i) {
					continue
				}
				if leaf == "name" && !mt.New {
					continue // already reported as a rename
				}
				out = append(out, d.changeLine(f, i, arrow))
			}
		}
		for _, it := range listItems(d.orig, s.List) {
			if n := itemName(it); !kept[n] {
				out = append(out, fmt.Sprintf("- %s[%s]: removed", s.List, n))
			}
		}
	}
	return out
}

func (d *cfgDraft) changeLine(f config.FieldSpec, idx int, arrow string) string {
	path := d.fieldPath(f, idx)
	if string(f.Kind) == "secret" {
		switch d.SecretOpFor(f, idx) {
		case secOpGenerate:
			return path + ": changed (generated)"
		case secOpClear:
			return path + ": changed (cleared)"
		}
		return path + ": changed"
	}
	old, _ := d.origRaw(f, idx)
	return fmt.Sprintf("%s: %s %s %s", path, quoteEmpty(rawString(old)), arrow, quoteEmpty(rawString(d.curRaw(f, idx))))
}

func quoteEmpty(s string) string {
	if s == "" {
		return `""`
	}
	return s
}

// Dirty reports whether anything differs from the loaded config.
func (d *cfgDraft) Dirty() bool { return len(d.Changes()) > 0 }

// Renames returns the rename operations (existing items whose name changed).
func (d *cfgDraft) Renames() []config.Rename {
	var out []config.Rename
	for _, s := range d.schema.Sections {
		if s.List == "" {
			continue
		}
		for i, mt := range d.meta[s.List] {
			if name := d.itemNameAt(s.List, i); !mt.New && mt.Orig != name {
				out = append(out, config.Rename{List: s.List, From: mt.Orig, To: name})
			}
		}
	}
	return out
}

// SecretOps returns the recorded secret operations in insertion order. List
// item paths use the item's current (post-rename) name.
func (d *cfgDraft) SecretOps() []config.SecretOp {
	var out []config.SecretOp
	for _, k := range d.order {
		op := d.ops[k]
		path := op.field
		if op.list != "" {
			idx := -1
			for i, mt := range d.meta[op.list] {
				if mt.ID == op.id {
					idx = i
				}
			}
			if idx < 0 {
				continue
			}
			path = fmt.Sprintf("%s[%s].%s", op.list, d.itemNameAt(op.list, idx), op.field)
		}
		out = append(out, config.SecretOp{Path: path, Op: op.op, Value: op.value})
	}
	return out
}

// BuildUpdate assembles the config_update payload.
func (d *cfgDraft) BuildUpdate(revision string) (ipc.ConfigUpdatePayload, error) {
	b, err := json.Marshal(d.cur)
	if err != nil {
		return ipc.ConfigUpdatePayload{}, err
	}
	var cfg config.Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return ipc.ConfigUpdatePayload{}, fmt.Errorf("draft is not a valid config: %w", err)
	}
	return ipc.ConfigUpdatePayload{
		Revision:  revision,
		Config:    &cfg,
		SecretOps: d.SecretOps(),
		Renames:   d.Renames(),
	}, nil
}
