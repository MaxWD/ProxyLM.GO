package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// leafPaths обходит тип Config по yaml-тегам и собирает пути листовых полей.
func leafPaths(t *testing.T, typ reflect.Type, prefix string, out map[string]bool) {
	t.Helper()
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		yamlName := strings.Split(f.Tag.Get("yaml"), ",")[0]
		jsonName := strings.Split(f.Tag.Get("json"), ",")[0]
		if yamlName == "" || jsonName != yamlName {
			t.Errorf("%s.%s: yaml tag %q and json tag %q must be equal and non-empty", typ.Name(), f.Name, yamlName, jsonName)
		}
		path := yamlName
		if prefix != "" {
			path = prefix + "." + yamlName
		}
		ft := f.Type
		switch {
		case ft.Kind() == reflect.Struct:
			leafPaths(t, ft, path, out)
		case ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.Struct:
			leafPaths(t, ft.Elem(), path+"[]", out)
		default:
			out[path] = true
		}
	}
}

func TestConfigSchema_CoversEveryField(t *testing.T) {
	leaves := map[string]bool{}
	leafPaths(t, reflect.TypeOf(Config{}), "", leaves)

	exempt := map[string]bool{}

	s := ConfigSchema()
	inSchema := map[string]bool{}
	sections := map[string]bool{}
	for _, sec := range s.Sections {
		sections[sec.ID] = true
	}
	for _, f := range s.Fields {
		if inSchema[f.Path] {
			t.Errorf("duplicate schema path %s", f.Path)
		}
		inSchema[f.Path] = true
		if !leaves[f.Path] {
			t.Errorf("schema path %s does not exist in Config", f.Path)
		}
		if !sections[f.Section] {
			t.Errorf("%s: unknown section %q", f.Path, f.Section)
		}
		if f.Apply != ApplyHot && f.Apply != ApplyRestart {
			t.Errorf("%s: bad apply class %q", f.Path, f.Apply)
		}
		if f.Kind == KindEnum && len(f.Enum) == 0 {
			t.Errorf("%s: enum without values", f.Path)
		}
		if f.Label == "" {
			t.Errorf("%s: empty label", f.Path)
		}
	}
	for p := range leaves {
		if !inSchema[p] && !exempt[p] {
			t.Errorf("Config field %s is missing from the schema", p)
		}
	}
}

func TestConfigSchema_JSON(t *testing.T) {
	data, err := json.Marshal(ConfigSchema())
	if err != nil {
		t.Fatal(err)
	}
	var back Schema
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Fields) != len(ConfigSchema().Fields) {
		t.Errorf("round trip lost fields")
	}
}

func TestClassOf(t *testing.T) {
	tests := []struct {
		path string
		want ApplyClass
	}{
		{"proxy.log_level", ApplyHot},
		{"proxy.port", ApplyRestart},
		{"proxy.host", ApplyRestart},
		{"auth.admin_key", ApplyRestart},
		{"auth.api_keys[crm]", ApplyHot},
		{"auth.api_keys[crm].key", ApplyHot},
		{"auth.api_keys[#1].name", ApplyHot},
		{"backends[gpu-1]", ApplyRestart},
		{"backends[gpu-1].url", ApplyRestart},
		{"retry.max_attempts", ApplyHot},
		{"routing.strategy", ApplyHot},
		{"scheduler.max_consecutive_per_model", ApplyHot},
		{"discovery.enabled", ApplyRestart},
		{"discovery.interval_seconds", ApplyHot},
		{"storage.database_path", ApplyRestart},
		{"storage.history_retention_days", ApplyHot},
		{"tui.show_completed_minutes", ApplyHot},
		{"compat.response_format_mode", ApplyHot},
		{"something.unknown", ApplyRestart},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := ClassOf(tt.path); got != tt.want {
				t.Errorf("ClassOf(%q) = %s, want %s", tt.path, got, tt.want)
			}
		})
	}
}

func TestValidateFields_Paths(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   []string
	}{
		{"ok", func(c *Config) {}, nil},
		{"bad port", func(c *Config) { c.Proxy.Port = 0 }, []string{"proxy.port"}},
		{"bad strategy", func(c *Config) { c.Routing.Strategy = "x" }, []string{"routing.strategy"}},
		{"bad url by name", func(c *Config) { c.Backends[0].URL = "ftp://x" }, []string{"backends[srv1].url"}},
		{"duplicate backend uses index", func(c *Config) { c.Backends = append(c.Backends, c.Backends[0]) },
			[]string{"backends[#1].name"}},
		{"empty api key name uses index", func(c *Config) { c.Auth.APIKeys[1].Name = "" }, []string{"auth.api_keys[#1].name"}},
		{"new key without value", func(c *Config) { c.Auth.APIKeys = append(c.Auth.APIKeys, APIKey{Name: "x"}) },
			[]string{"auth.api_keys[x].key"}},
		{"admin equals client key", func(c *Config) { c.Auth.AdminKey = "k1" }, []string{"auth.admin_key"}},
		{"several", func(c *Config) { c.Retry.MaxAttempts = 0; c.Backends[0].TimeoutSeconds = 0 },
			[]string{"retry.max_attempts", "backends[srv1].timeout_seconds"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := defaultValidConfig()
			tt.mutate(&c)
			var got []string
			for _, e := range c.ValidateFields() {
				got = append(got, e.Path)
				if e.Msg == "" {
					t.Errorf("%s: empty message", e.Path)
				}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("paths = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDiff(t *testing.T) {
	base := func() *Config {
		c := defaultValidConfig()
		c.Backends = append(c.Backends, Backend{Name: "srv2", URL: "http://127.0.0.1:2", TimeoutSeconds: 10, Type: "openai"})
		return &c
	}
	tests := []struct {
		name   string
		mutate func(*Config)
		want   []string
	}{
		{"same", func(c *Config) {}, nil},
		{"scalar", func(c *Config) { c.Retry.MaxAttempts = 9 }, []string{"retry.max_attempts"}},
		{"admin key", func(c *Config) { c.Auth.AdminKey = "z" }, []string{"auth.admin_key"}},
		{"backend changed", func(c *Config) { c.Backends[1].URL = "http://x:1" }, []string{"backends[srv2]"}},
		{"backend removed", func(c *Config) { c.Backends = c.Backends[:1] }, []string{"backends[srv2]"}},
		{"backend added", func(c *Config) { c.Backends = append(c.Backends, Backend{Name: "n"}) }, []string{"backends[n]"}},
		{"reorder is not a change", func(c *Config) { c.Backends[0], c.Backends[1] = c.Backends[1], c.Backends[0] }, nil},
		{"api key value", func(c *Config) { c.Auth.APIKeys[0].Key = "new" }, []string{"auth.api_keys[a]"}},
		{"empty vs nil models", func(c *Config) { c.Backends[0].Models = []string{} }, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b := base(), base()
			tt.mutate(b)
			if got := Diff(a, b); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Diff = %v, want %v", got, tt.want)
			}
		})
	}
}
