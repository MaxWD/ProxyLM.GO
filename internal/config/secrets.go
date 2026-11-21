package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// Операции над секретами и префиксы генерируемых ключей.
const (
	SecretOpSet      = "set"
	SecretOpGenerate = "generate"
	SecretOpClear    = "clear"

	ClientKeyPrefix = "sk-proxy-"
	AdminKeyPrefix  = "sk-admin-"
)

// SecretOp — явная операция над секретом. Значения секретов, пришедшие в самом
// Config от клиента, ВСЕГДА игнорируются; менять секрет можно только так.
type SecretOp struct {
	Path  string `json:"path"` // "auth.api_keys[crm].key", "auth.admin_key", "backends[gpu-1].api_key"
	Op    string `json:"op"`   // "set" | "generate" | "clear"
	Value string `json:"value,omitempty"`
}

// Rename — переименование элемента списка ("backends" | "auth.api_keys").
type Rename struct {
	List string `json:"list"`
	From string `json:"from"`
	To   string `json:"to"`
}

// Clone возвращает глубокую копию конфига.
func (c *Config) Clone() *Config {
	out := *c
	out.Auth.APIKeys = append([]APIKey(nil), c.Auth.APIKeys...)
	out.Backends = make([]Backend, len(c.Backends))
	for i, b := range c.Backends {
		b.Models = append([]string(nil), b.Models...)
		out.Backends[i] = b
	}
	if c.Backends == nil {
		out.Backends = nil
	}
	if c.Auth.APIKeys == nil {
		out.Auth.APIKeys = nil
	}
	return &out
}

// secretHint маскирует секрет: "" если не задан, "…" + последние 4 символа,
// а для коротких (<= 8) значений — "…set".
func secretHint(s string) string {
	switch {
	case s == "":
		return ""
	case len(s) <= 8:
		return "…set"
	default:
		return "…" + s[len(s)-4:]
	}
}

// Masked возвращает глубокую копию конфига, в которой каждый секрет заменён
// подсказкой (см. secretHint). Именно эта копия уходит клиентам.
func Masked(c *Config) *Config {
	out := c.Clone()
	out.Auth.AdminKey = secretHint(out.Auth.AdminKey)
	for i := range out.Auth.APIKeys {
		out.Auth.APIKeys[i].Key = secretHint(out.Auth.APIKeys[i].Key)
	}
	for i := range out.Backends {
		out.Backends[i].APIKey = secretHint(out.Backends[i].APIKey)
	}
	return out
}

// GenerateKey возвращает prefix + 32 hex-символа из crypto/rand.
func GenerateKey(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate key: %w", err)
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

// findIndex ищет элемент списка по ключу адресации ("name" или "#idx").
func findIndex(keys []string, key string) int {
	for i, k := range keys {
		if k == key {
			return i
		}
	}
	return -1
}

// MergeUpdate строит новую версию конфига из текущей (current — содержимое файла)
// и входящей от клиента (in):
//  1. к копии current применяются переименования;
//  2. из in берутся все НЕсекретные поля и состав/порядок списков;
//  3. секреты берутся из current (элементы списков сопоставляются по имени после
//     переименований), любые секреты в in игнорируются;
//  4. применяются secret-операции (имена в путях — после переименований).
//
// Возвращает новый конфиг, сгенерированные ключи (путь → ключ целиком) и ошибки
// уровня полей. Валидацию (ValidateFields) вызывает вызывающая сторона.
func MergeUpdate(current, in *Config, renames []Rename, ops []SecretOp) (next *Config, generated map[string]string, errs []FieldError) {
	cur := current.Clone()
	for _, r := range renames {
		switch r.List {
		case "backends":
			idx := findIndex(backendNames(cur.Backends), r.From)
			if idx < 0 {
				errs = append(errs, FieldError{Path: itemPath("backends", r.From), Msg: "rename: item not found"})
				continue
			}
			cur.Backends[idx].Name = r.To
		case "auth.api_keys":
			idx := findIndex(apiKeyNames(cur.Auth.APIKeys), r.From)
			if idx < 0 {
				errs = append(errs, FieldError{Path: itemPath("auth.api_keys", r.From), Msg: "rename: item not found"})
				continue
			}
			cur.Auth.APIKeys[idx].Name = r.To
		default:
			errs = append(errs, FieldError{Path: r.List, Msg: "rename: unknown list"})
		}
	}

	next = in.Clone()
	next.Auth.AdminKey = cur.Auth.AdminKey
	curKeys := map[string]string{}
	for _, k := range cur.Auth.APIKeys {
		if _, dup := curKeys[k.Name]; !dup {
			curKeys[k.Name] = k.Key
		}
	}
	for i := range next.Auth.APIKeys {
		next.Auth.APIKeys[i].Key = curKeys[next.Auth.APIKeys[i].Name]
	}
	curBackendKeys := map[string]string{}
	for _, b := range cur.Backends {
		if _, dup := curBackendKeys[b.Name]; !dup {
			curBackendKeys[b.Name] = b.APIKey
		}
	}
	for i := range next.Backends {
		b := &next.Backends[i]
		b.APIKey = curBackendKeys[b.Name]
		if b.Type == "" {
			b.Type = BackendTypeOpenAI
		}
		if b.TimeoutSeconds == 0 {
			b.TimeoutSeconds = 600
		}
	}

	generated = map[string]string{}
	for _, op := range ops {
		if err := applySecretOp(next, op, generated); err != nil {
			errs = append(errs, FieldError{Path: op.Path, Msg: err.Error()})
		}
	}
	return next, generated, errs
}

// applySecretOp применяет одну операцию к next.
func applySecretOp(c *Config, op SecretOp, generated map[string]string) error {
	var (
		target *string
		prefix string
	)
	switch {
	case op.Path == PathAdminKey:
		target, prefix = &c.Auth.AdminKey, AdminKeyPrefix
	case strings.HasPrefix(op.Path, "auth.api_keys[") && strings.HasSuffix(op.Path, "].key"):
		key := op.Path[len("auth.api_keys[") : len(op.Path)-len("].key")]
		idx := resolveItem(apiKeyNames(c.Auth.APIKeys), key)
		if idx < 0 {
			return fmt.Errorf("item not found")
		}
		target, prefix = &c.Auth.APIKeys[idx].Key, ClientKeyPrefix
	case strings.HasPrefix(op.Path, "backends[") && strings.HasSuffix(op.Path, "].api_key"):
		key := op.Path[len("backends[") : len(op.Path)-len("].api_key")]
		idx := resolveItem(backendNames(c.Backends), key)
		if idx < 0 {
			return fmt.Errorf("item not found")
		}
		target = &c.Backends[idx].APIKey
	default:
		return fmt.Errorf("not a secret path")
	}
	switch op.Op {
	case SecretOpSet:
		if op.Value == "" {
			return fmt.Errorf("value required for set")
		}
		*target = op.Value
	case SecretOpClear:
		*target = ""
	case SecretOpGenerate:
		if prefix == "" {
			return fmt.Errorf("cannot generate an upstream key")
		}
		k, err := GenerateKey(prefix)
		if err != nil {
			return err
		}
		*target = k
		generated[op.Path] = k
	default:
		return fmt.Errorf("unknown op %q", op.Op)
	}
	return nil
}

// resolveItem находит элемент по ключу адресации: "#N" — индекс, иначе — имя
// (имя должно быть уникальным).
func resolveItem(names []string, key string) int {
	if rest, ok := strings.CutPrefix(key, "#"); ok {
		n, err := strconv.Atoi(rest)
		if err != nil || n < 0 || n >= len(names) {
			return -1
		}
		return n
	}
	return findIndex(itemKeys(names), key)
}
