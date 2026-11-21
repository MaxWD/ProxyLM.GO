package config

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

func secretConfig() *Config {
	c := defaultValidConfig()
	c.Auth.APIKeys = []APIKey{{Name: "crm", Key: "sk-proxy-crm-secret-1234"}, {Name: "bot", Key: "short"}}
	c.Auth.AdminKey = "sk-admin-very-secret-9999"
	c.Backends = []Backend{
		{Name: "gpu-1", URL: "http://127.0.0.1:1", Type: "openai", TimeoutSeconds: 5, APIKey: "sk-upstream-abcdef-7777"},
		{Name: "gpu-2", URL: "http://127.0.0.1:2", Type: "openai", TimeoutSeconds: 5},
	}
	return &c
}

func TestMasked(t *testing.T) {
	c := secretConfig()
	m := Masked(c)
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"sk-proxy-crm-secret-1234", "sk-admin-very-secret-9999", "sk-upstream-abcdef-7777", "short"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("masked config leaks %q", secret)
		}
	}
	tests := []struct{ name, got, want string }{
		{"long api key", m.Auth.APIKeys[0].Key, "…1234"},
		{"short api key", m.Auth.APIKeys[1].Key, "…set"},
		{"admin", m.Auth.AdminKey, "…9999"},
		{"backend key", m.Backends[0].APIKey, "…7777"},
		{"unset backend key", m.Backends[1].APIKey, ""},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s: got %q want %q", tt.name, tt.got, tt.want)
		}
	}
	// оригинал не изменён
	if c.Auth.APIKeys[0].Key != "sk-proxy-crm-secret-1234" || c.Backends[0].APIKey != "sk-upstream-abcdef-7777" {
		t.Error("Masked mutated the source config")
	}
}

func TestMergeUpdate(t *testing.T) {
	hex32 := func(prefix string) *regexp.Regexp {
		return regexp.MustCompile("^" + regexp.QuoteMeta(prefix) + "[0-9a-f]{32}$")
	}
	tests := []struct {
		name      string
		mutate    func(in *Config)
		renames   []Rename
		ops       []SecretOp
		check     func(t *testing.T, next *Config, gen map[string]string, errs []FieldError)
		wantErrOn string
	}{
		{
			name: "client-sent secret values are ignored",
			mutate: func(in *Config) {
				in.Auth.AdminKey = "hacked"
				in.Auth.APIKeys[0].Key = "hacked"
				in.Backends[0].APIKey = "hacked"
				in.Retry.MaxAttempts = 7
			},
			check: func(t *testing.T, next *Config, gen map[string]string, errs []FieldError) {
				if next.Auth.AdminKey != "sk-admin-very-secret-9999" || next.Auth.APIKeys[0].Key != "sk-proxy-crm-secret-1234" || next.Backends[0].APIKey != "sk-upstream-abcdef-7777" {
					t.Errorf("secrets were not preserved: %+v", next)
				}
				if next.Retry.MaxAttempts != 7 {
					t.Error("non-secret field was not applied")
				}
			},
		},
		{
			name: "rename keeps secret",
			mutate: func(in *Config) {
				in.Auth.APIKeys[0].Name = "crm2"
				in.Backends[0].Name = "gpu-x"
			},
			renames: []Rename{{List: "auth.api_keys", From: "crm", To: "crm2"}, {List: "backends", From: "gpu-1", To: "gpu-x"}},
			check: func(t *testing.T, next *Config, gen map[string]string, errs []FieldError) {
				if next.Auth.APIKeys[0].Key != "sk-proxy-crm-secret-1234" || next.Backends[0].APIKey != "sk-upstream-abcdef-7777" {
					t.Errorf("rename lost secret: %+v", next)
				}
			},
		},
		{
			name:   "generate api key",
			mutate: func(in *Config) {},
			ops:    []SecretOp{{Path: "auth.api_keys[crm].key", Op: "generate"}},
			check: func(t *testing.T, next *Config, gen map[string]string, errs []FieldError) {
				k := gen["auth.api_keys[crm].key"]
				if !hex32("sk-proxy-").MatchString(k) || next.Auth.APIKeys[0].Key != k {
					t.Errorf("bad generated key %q (cfg %q)", k, next.Auth.APIKeys[0].Key)
				}
			},
		},
		{
			name:   "generate admin key",
			mutate: func(in *Config) {},
			ops:    []SecretOp{{Path: "auth.admin_key", Op: "generate"}},
			check: func(t *testing.T, next *Config, gen map[string]string, errs []FieldError) {
				if !hex32("sk-admin-").MatchString(gen["auth.admin_key"]) {
					t.Errorf("bad admin key %q", gen["auth.admin_key"])
				}
			},
		},
		{
			name:   "set and clear",
			mutate: func(in *Config) {},
			ops: []SecretOp{
				{Path: "backends[gpu-1].api_key", Op: "clear"},
				{Path: "backends[gpu-2].api_key", Op: "set", Value: "up-key"},
				{Path: "auth.api_keys[bot].key", Op: "set", Value: "newbot"},
			},
			check: func(t *testing.T, next *Config, gen map[string]string, errs []FieldError) {
				if next.Backends[0].APIKey != "" || next.Backends[1].APIKey != "up-key" || next.Auth.APIKeys[1].Key != "newbot" {
					t.Errorf("ops not applied: %+v", next)
				}
				if len(gen) != 0 {
					t.Errorf("set/clear must not report generated keys: %v", gen)
				}
			},
		},
		{
			name: "new item with generate",
			mutate: func(in *Config) {
				in.Auth.APIKeys = append(in.Auth.APIKeys, APIKey{Name: "new", Key: "ignored"})
			},
			ops: []SecretOp{{Path: "auth.api_keys[new].key", Op: "generate"}},
			check: func(t *testing.T, next *Config, gen map[string]string, errs []FieldError) {
				if next.Auth.APIKeys[2].Key == "ignored" || next.Auth.APIKeys[2].Key == "" {
					t.Errorf("new key = %q", next.Auth.APIKeys[2].Key)
				}
				if len(next.ValidateFields()) != 0 {
					t.Errorf("unexpected validation errors: %v", next.ValidateFields())
				}
			},
		},
		{
			name: "new item without key fails validation",
			mutate: func(in *Config) {
				in.Auth.APIKeys = append(in.Auth.APIKeys, APIKey{Name: "x", Key: "ignored"})
			},
			check: func(t *testing.T, next *Config, gen map[string]string, errs []FieldError) {
				fe := next.ValidateFields()
				if len(fe) != 1 || fe[0].Path != "auth.api_keys[x].key" {
					t.Errorf("got %v", fe)
				}
			},
		},
		{
			name:      "unknown item",
			mutate:    func(in *Config) {},
			ops:       []SecretOp{{Path: "auth.api_keys[nope].key", Op: "clear"}},
			wantErrOn: "auth.api_keys[nope].key",
		},
		{
			name:      "set without value",
			mutate:    func(in *Config) {},
			ops:       []SecretOp{{Path: "auth.admin_key", Op: "set"}},
			wantErrOn: "auth.admin_key",
		},
		{
			name:      "generate upstream key forbidden",
			mutate:    func(in *Config) {},
			ops:       []SecretOp{{Path: "backends[gpu-1].api_key", Op: "generate"}},
			wantErrOn: "backends[gpu-1].api_key",
		},
		{
			name:      "not a secret path",
			mutate:    func(in *Config) {},
			ops:       []SecretOp{{Path: "retry.max_attempts", Op: "set", Value: "1"}},
			wantErrOn: "retry.max_attempts",
		},
		{
			name:      "rename of missing item",
			mutate:    func(in *Config) {},
			renames:   []Rename{{List: "backends", From: "ghost", To: "x"}},
			wantErrOn: "backends[ghost]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cur := secretConfig()
			in := Masked(cur) // клиент получил маску и отправляет её обратно
			tt.mutate(in)
			next, gen, errs := MergeUpdate(cur, in, tt.renames, tt.ops)
			if tt.wantErrOn != "" {
				if len(errs) != 1 || errs[0].Path != tt.wantErrOn {
					t.Fatalf("errs = %v, want one on %s", errs, tt.wantErrOn)
				}
				return
			}
			if len(errs) != 0 {
				t.Fatalf("unexpected errs: %v", errs)
			}
			tt.check(t, next, gen, errs)
			if cur.Auth.AdminKey != "sk-admin-very-secret-9999" {
				t.Error("MergeUpdate mutated current")
			}
		})
	}
}
