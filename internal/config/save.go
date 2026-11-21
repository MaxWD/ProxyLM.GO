package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ErrConflict — файл конфига изменился с момента, когда клиент прочитал его ревизию.
var ErrConflict = errors.New("config: revision conflict (file changed since it was read)")

// Revision возвращает ревизию содержимого файла (первые 12 hex-символов sha256).
func Revision(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:12]
}

// ReadRevision читает файл и возвращает его содержимое и ревизию.
func ReadRevision(path string) ([]byte, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read config: %w", err)
	}
	return data, Revision(data), nil
}

// Save записывает c в файл path, сохраняя комментарии исходного файла.
// См. SaveRenamed.
func Save(path string, c *Config, expectRev string) (newRev string, err error) {
	return SaveRenamed(path, c, expectRev, nil)
}

// SaveRenamed — Save с учётом переименований элементов списков: элемент c с
// именем To сопоставляется с элементом файла с именем From (сохраняя его
// комментарии).
//
// Алгоритм: читает файл и сверяет ревизию (иначе ErrConflict); парсит файл в
// yaml.Node; кодирует c во второй yaml.Node; сливает (значения обновляются на
// месте, комментарии сохраняются; недостающие ключи добавляются; элементы
// backends и auth.api_keys сопоставляются по name — совпавшие сохраняют
// комментарии, удалённые убираются, новые добавляются); пишет temp-файл в том же
// каталоге, fsync, копирует прежнюю версию в <path>.bak и переименовывает temp.
func SaveRenamed(path string, c *Config, expectRev string, renames []Rename) (newRev string, err error) {
	data, rev, err := ReadRevision(path)
	if err != nil {
		return "", err
	}
	if rev != expectRev {
		return "", ErrConflict
	}
	out, err := mergeYAML(data, c, renames)
	if err != nil {
		return "", err
	}
	if err := writeAtomic(path, data, out); err != nil {
		return "", err
	}
	return Revision(out), nil
}

// mergeYAML возвращает новое содержимое файла: data с применённым c.
func mergeYAML(data []byte, c *Config, renames []Rename) ([]byte, error) {
	// CRLF-файлы (Windows): работаем с LF и возвращаем CRLF на выходе, иначе ""
	// остаётся внутри комментариев и удваивает переводы строк.
	crlf := bytes.Contains(data, []byte("\r\n"))
	if crlf {
		data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	// Конфиг, как его видит daemon (с дефолтами), — база для «не менялось».
	old := &Config{}
	if err := yaml.Unmarshal(data, old); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	applyDefaults(old)

	var oldNode, newNode yaml.Node
	if err := oldNode.Encode(old); err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	if err := newNode.Encode(c); err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}

	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{&newNode}}
	} else {
		mergeMapping(doc.Content[0], &oldNode, &newNode, renames)
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}

	// Страховка: результат должен читаться обратно в тот же конфиг.
	check := &Config{}
	if err := yaml.Unmarshal(buf.Bytes(), check); err != nil {
		return nil, fmt.Errorf("merged config does not parse: %w", err)
	}
	applyDefaults(check)
	want := c.Clone()
	applyDefaults(want) // Load подставляет дефолты вместо нулей — сравниваем в той же нормализации
	if d := Diff(check, want); len(d) > 0 {
		return nil, fmt.Errorf("merged config differs from requested at %v", d)
	}
	out := restoreBlankLines(buf.Bytes())
	if crlf {
		out = bytes.ReplaceAll(out, []byte("\n"), []byte("\r\n"))
	}
	return out, nil
}

// restoreBlankLines возвращает пустые строки между верхнеуровневыми блоками
// (yaml.v3 их не сохраняет): перед строкой в колонке 0 (ключ или комментарий) и
// перед элементами `  - ` списка backends, если предыдущая строка — не пустая и не
// комментарий того же блока.
func restoreBlankLines(data []byte) []byte {
	lines := strings.Split(string(data), "\n")
	out := make([]string, 0, len(lines)+16)
	for i, l := range lines {
		if i > 0 && l != "" {
			prev := out[len(out)-1]
			col0 := l[0] != ' ' && l[0] != '	'
			prevCol0Comment := strings.HasPrefix(prev, "#")
			switch {
			case col0 && prev != "" && !prevCol0Comment && i > 1:
				out = append(out, "")
			case strings.HasPrefix(l, "  - ") && prev != "" && !strings.HasPrefix(prev, "  - ") &&
				!strings.HasPrefix(prev, "  #") && !strings.HasSuffix(prev, ":"):
				out = append(out, "")
			}
		}
		out = append(out, l)
	}
	return []byte(strings.Join(out, "\n"))
}

// listKeys — ключи mapping'ов, значения которых — списки элементов с `name`.
var listKeys = map[string]string{"backends": "backends", "api_keys": "auth.api_keys"}

func mergeMapping(file, old, nw *yaml.Node, renames []Rename) {
	for i := 0; i+1 < len(nw.Content); i += 2 {
		k, nv := nw.Content[i], nw.Content[i+1]
		ov := mapGet(old, k.Value)
		if ov != nil && nodesEqual(ov, nv) {
			continue // не менялось — не трогаем файл (форматирование и комментарии)
		}
		fv := mapGet(file, k.Value)
		if fv == nil {
			file.Content = append(file.Content, k, nv)
			continue
		}
		switch {
		case fv.Kind == yaml.MappingNode && nv.Kind == yaml.MappingNode:
			if ov == nil || ov.Kind != yaml.MappingNode {
				ov = &yaml.Node{Kind: yaml.MappingNode}
			}
			mergeMapping(fv, ov, nv, renames)
		case fv.Kind == yaml.SequenceNode && nv.Kind == yaml.SequenceNode && listKeys[k.Value] != "":
			if ov == nil || ov.Kind != yaml.SequenceNode {
				ov = &yaml.Node{Kind: yaml.SequenceNode}
			}
			mergeItems(fv, ov, nv, renamesFor(renames, listKeys[k.Value]))
		default:
			replaceValue(fv, nv)
		}
	}
}

// renamesFor возвращает переименования to→from для списка.
func renamesFor(renames []Rename, list string) map[string]string {
	m := map[string]string{}
	for _, r := range renames {
		if r.List == list {
			m[r.To] = r.From
		}
	}
	return m
}

// mergeItems сливает элементы списка по name. Порядок — как в nw.
func mergeItems(file, old, nw *yaml.Node, fromName map[string]string) {
	used := make([]bool, len(file.Content))
	result := make([]*yaml.Node, 0, len(nw.Content))
	for _, ni := range nw.Content {
		name := itemName(ni)
		if from, ok := fromName[name]; ok {
			name = from
		}
		idx := -1
		if name != "" {
			for j, fi := range file.Content {
				if !used[j] && itemName(fi) == name {
					idx = j
					break
				}
			}
		}
		if idx < 0 || file.Content[idx].Kind != yaml.MappingNode || ni.Kind != yaml.MappingNode {
			pruneEmpty(ni)
			result = append(result, ni)
			continue
		}
		used[idx] = true
		oi := &yaml.Node{Kind: yaml.MappingNode}
		if idx < len(old.Content) && old.Content[idx].Kind == yaml.MappingNode {
			oi = old.Content[idx]
		}
		mergeMapping(file.Content[idx], oi, ni, nil)
		result = append(result, file.Content[idx])
	}
	file.Content = result
}

// pruneEmpty убирает из нового mapping'а ключи с пустым значением (""/null/[]),
// чтобы добавленный элемент не нёс шум вроде `api_key: ""` и `models: []`.
func pruneEmpty(m *yaml.Node) {
	if m.Kind != yaml.MappingNode {
		return
	}
	kept := m.Content[:0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		v := m.Content[i+1]
		empty := (v.Kind == yaml.ScalarNode && (v.Value == "" || v.Tag == "!!null")) ||
			((v.Kind == yaml.SequenceNode || v.Kind == yaml.MappingNode) && len(v.Content) == 0)
		if !empty {
			kept = append(kept, m.Content[i], v)
		}
	}
	m.Content = kept
}

// itemName возвращает значение ключа name в mapping-узле элемента.
func itemName(n *yaml.Node) string {
	if v := mapGet(n, "name"); v != nil {
		return v.Value
	}
	return ""
}

func mapGet(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func nodesEqual(a, b *yaml.Node) bool {
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case yaml.ScalarNode:
		return a.Value == b.Value
	case yaml.MappingNode, yaml.SequenceNode:
		if len(a.Content) != len(b.Content) {
			return false
		}
		for i := range a.Content {
			if !nodesEqual(a.Content[i], b.Content[i]) {
				return false
			}
		}
		return true
	}
	return false
}

// replaceValue подменяет содержимое dst узлом src, сохраняя комментарии dst.
func replaceValue(dst, src *yaml.Node) {
	head, line, foot := dst.HeadComment, dst.LineComment, dst.FootComment
	*dst = *src
	if head != "" {
		dst.HeadComment = head
	}
	if line != "" {
		dst.LineComment = line
	}
	if foot != "" {
		dst.FootComment = foot
	}
}

// writeAtomic: temp в том же каталоге → fsync → копия прежней версии в .bak → rename.
func writeAtomic(path string, oldData, newData []byte) error {
	dir := filepath.Dir(path)
	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(newData); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("fsync temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		cleanup()
		return fmt.Errorf("chmod temp config: %w", err)
	}
	if err := os.WriteFile(path+".bak", oldData, mode); err != nil {
		cleanup()
		return fmt.Errorf("write config backup: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}
