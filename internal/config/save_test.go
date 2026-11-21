package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupExample копирует config.example.yaml во временный каталог.
func setupExample(t *testing.T) (path string, rev string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, Revision(data)
}

func commentLines(data []byte) []string {
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		if s := strings.TrimSpace(l); strings.HasPrefix(s, "#") {
			out = append(out, s)
		}
	}
	return out
}

func mustLoad(t *testing.T, path string) *Config {
	t.Helper()
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSave_PreservesComments(t *testing.T) {
	path, rev := setupExample(t)
	orig, _ := os.ReadFile(path)
	c := mustLoad(t, path)
	c.Retry.MaxAttempts = 5
	c.Proxy.LogLevel = "debug"
	c.Routing.Strategy = StrategyRoundRobin
	c.Backends[1].Priority = 250
	c.Auth.APIKeys[0].Key = "sk-proxy-changed-key"

	newRev, err := Save(path, c, rev)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if newRev != Revision(after) || newRev == rev {
		t.Errorf("bad revision %q", newRev)
	}
	for _, want := range commentLines(orig) {
		if !strings.Contains(string(after), want) {
			t.Errorf("comment lost: %q", want)
		}
	}
	got := mustLoad(t, path)
	if got.Retry.MaxAttempts != 5 || got.Proxy.LogLevel != "debug" || got.Routing.Strategy != StrategyRoundRobin ||
		got.Backends[1].Priority != 250 || got.Auth.APIKeys[0].Key != "sk-proxy-changed-key" {
		t.Errorf("values not saved: %+v", got)
	}
	if d := Diff(got, c); len(d) != 0 {
		t.Errorf("saved config differs: %v", d)
	}
	// не менялось — формат строк сохранён (например, `api_key: null` и inline-комментарий)
	if !strings.Contains(string(after), "api_key: null") {
		t.Error("untouched `api_key: null` was rewritten")
	}
}

func TestSave_NoChangeKeepsContent(t *testing.T) {
	path, rev := setupExample(t)
	c := mustLoad(t, path)
	if _, err := Save(path, c, rev); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	orig, _ := os.ReadFile(path + ".bak")
	if d := Diff(mustLoad(t, path), c); len(d) != 0 {
		t.Errorf("differs: %v", d)
	}
	if len(commentLines(after)) != len(commentLines(orig)) {
		t.Errorf("comment count %d -> %d", len(commentLines(orig)), len(commentLines(after)))
	}
}

func TestSave_ListItems(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(c *Config)
		renames []Rename
		check   func(t *testing.T, got *Config, text string)
	}{
		{
			name: "add backend and key",
			mutate: func(c *Config) {
				c.Backends = append(c.Backends, Backend{Name: "gpu-3", URL: "http://10.0.0.3:1234", Type: "ollama", Priority: 300, TimeoutSeconds: 60, APIKey: "up"})
				c.Auth.APIKeys = append(c.Auth.APIKeys, APIKey{Name: "crm", Key: "sk-proxy-crm"})
			},
			check: func(t *testing.T, got *Config, text string) {
				if len(got.Backends) != 3 || got.Backends[2].Name != "gpu-3" || got.Backends[2].APIKey != "up" {
					t.Errorf("backend not added: %+v", got.Backends)
				}
				if len(got.Auth.APIKeys) != 3 || got.Auth.APIKeys[2].Name != "crm" {
					t.Errorf("key not added: %+v", got.Auth.APIKeys)
				}
			},
		},
		{
			name: "remove backend and key",
			mutate: func(c *Config) {
				c.Backends = c.Backends[:1]
				c.Auth.APIKeys = c.Auth.APIKeys[1:]
			},
			check: func(t *testing.T, got *Config, text string) {
				if len(got.Backends) != 1 || got.Backends[0].Name != "lmstudio-desktop" {
					t.Errorf("backends: %+v", got.Backends)
				}
				if len(got.Auth.APIKeys) != 1 || got.Auth.APIKeys[0].Name != "service-b" {
					t.Errorf("keys: %+v", got.Auth.APIKeys)
				}
				if strings.Contains(text, "ollama-rack") {
					t.Error("removed backend still in file")
				}
			},
		},
		{
			name:   "edit backend in place keeps its comment",
			mutate: func(c *Config) { c.Backends[0].URL = "http://192.168.1.5:1234" },
			check: func(t *testing.T, got *Config, text string) {
				if got.Backends[0].URL != "http://192.168.1.5:1234" || got.Backends[1].URL != "http://127.0.0.1:11434" {
					t.Errorf("urls: %+v", got.Backends)
				}
				if !strings.Contains(text, "# включает пробу загруженных моделей") {
					t.Error("inline comment of the edited item lost")
				}
			},
		},
		{
			name: "rename keeps item comments",
			mutate: func(c *Config) {
				c.Backends[1].Name = "ollama-new"
			},
			renames: []Rename{{List: "backends", From: "ollama-rack", To: "ollama-new"}},
			check: func(t *testing.T, got *Config, text string) {
				if got.Backends[1].Name != "ollama-new" || got.Backends[1].Priority != 200 {
					t.Errorf("backends: %+v", got.Backends)
				}
				if !strings.Contains(text, "# включает пробу загруженных моделей (/api/ps)") {
					t.Error("comment of the renamed item lost")
				}
			},
		},
		{
			name:   "rename without hint drops comments but stays valid",
			mutate: func(c *Config) { c.Backends[1].Name = "ollama-new" },
			check: func(t *testing.T, got *Config, text string) {
				if got.Backends[1].Name != "ollama-new" {
					t.Errorf("backends: %+v", got.Backends)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, rev := setupExample(t)
			c := mustLoad(t, path)
			tt.mutate(c)
			if _, err := SaveRenamed(path, c, rev, tt.renames); err != nil {
				t.Fatal(err)
			}
			text, _ := os.ReadFile(path)
			tt.check(t, mustLoad(t, path), string(text))
		})
	}
}

func TestSave_Conflict(t *testing.T) {
	path, rev := setupExample(t)
	c := mustLoad(t, path)
	// файл правят «руками»
	data, _ := os.ReadFile(path)
	if err := os.WriteFile(path, append(data, []byte("\n# hand edit\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	c.Retry.MaxAttempts = 9
	_, err := Save(path, c, rev)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if _, err := os.Stat(path + ".bak"); err == nil {
		t.Error(".bak must not be written on conflict")
	}
	if got := mustLoad(t, path); got.Retry.MaxAttempts == 9 {
		t.Error("file changed despite conflict")
	}
}

func TestSave_BackupAndAtomic(t *testing.T) {
	path, rev := setupExample(t)
	orig, _ := os.ReadFile(path)
	c := mustLoad(t, path)
	c.Retry.MaxAttempts = 4
	if _, err := Save(path, c, rev); err != nil {
		t.Fatal(err)
	}
	bak, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	if string(bak) != string(orig) {
		t.Error(".bak is not the previous version")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
	// второй Save со старой ревизией — конфликт; с новой — успех
	newData, _ := os.ReadFile(path)
	c.Retry.MaxAttempts = 6
	if _, err := Save(path, c, rev); !errors.Is(err, ErrConflict) {
		t.Errorf("stale revision: err = %v", err)
	}
	if _, err := Save(path, c, Revision(newData)); err != nil {
		t.Errorf("fresh revision: %v", err)
	}
}

func TestSave_MissingKeysAppended(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	min := "# top comment\nproxy:\n  host: 1.2.3.4\n  port: 9000 # my port\nauth:\n  api_keys:\n    - name: a\n      key: k1\n  admin_key: adm\nbackends:\n  - name: b\n    url: http://127.0.0.1:1\n"
	if err := os.WriteFile(path, []byte(min), 0o644); err != nil {
		t.Fatal(err)
	}
	c := mustLoad(t, path)
	c.Retry.MaxAttempts = 7 // секции retry в файле нет
	c.Proxy.Port = 9001
	if _, err := Save(path, c, Revision([]byte(min))); err != nil {
		t.Fatal(err)
	}
	text, _ := os.ReadFile(path)
	got := mustLoad(t, path)
	if got.Retry.MaxAttempts != 7 || got.Proxy.Port != 9001 {
		t.Errorf("got %+v", got)
	}
	for _, want := range []string{"# top comment", "# my port"} {
		if !strings.Contains(string(text), want) {
			t.Errorf("lost %q:\n%s", want, text)
		}
	}
	if strings.Contains(string(text), "scheduler") {
		t.Errorf("unchanged defaults must not be appended:\n%s", text)
	}
}
