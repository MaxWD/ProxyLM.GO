package config

import (
	"strings"
	"testing"
)

func TestConfig_Validate_OK(t *testing.T) {
	c := defaultValidConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("ожидался успех, получено: %v", err)
	}
}

func TestConfig_Validate_Errors(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{
			name:    "пустой admin_key",
			mutate:  func(c *Config) { c.Auth.AdminKey = "" },
			wantSub: "admin_key",
		},
		{
			name:    "дубликат api_keys[].name",
			mutate:  func(c *Config) { c.Auth.APIKeys = append(c.Auth.APIKeys, APIKey{Name: "a", Key: "diff"}) },
			wantSub: "дубликат name",
		},
		{
			name:    "неизвестная стратегия",
			mutate:  func(c *Config) { c.Routing.Strategy = "magic" },
			wantSub: "неизвестная стратегия",
		},
		{
			name:    "max_attempts < 1",
			mutate:  func(c *Config) { c.Retry.MaxAttempts = 0 },
			wantSub: "max_attempts",
		},
		{
			name:    "max_backoff < initial",
			mutate:  func(c *Config) { c.Retry.MaxBackoffMs = 100 },
			wantSub: "max_backoff_ms",
		},
		{
			name:    "невалидный url бэкенда",
			mutate:  func(c *Config) { c.Backends[0].URL = "ftp://x" },
			wantSub: "схема",
		},
		{
			name:    "дубликат имени бэкенда",
			mutate:  func(c *Config) { c.Backends = append(c.Backends, c.Backends[0]) },
			wantSub: "дубликат name",
		},
		{
			name:    "admin_key совпал с client api_key",
			mutate:  func(c *Config) { c.Auth.AdminKey = c.Auth.APIKeys[0].Key },
			wantSub: "admin_key",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := defaultValidConfig()
			tc.mutate(&c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("ожидалась ошибка")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("ожидался текст %q в ошибке, получено: %v", tc.wantSub, err)
			}
		})
	}
}

func defaultValidConfig() Config {
	c := Config{
		Auth: Auth{
			APIKeys:  []APIKey{{Name: "a", Key: "k1"}, {Name: "b", Key: "k2"}},
			AdminKey: "admin-key",
		},
		Backends: []Backend{
			{Name: "srv1", URL: "http://127.0.0.1:1234"},
		},
	}
	applyDefaults(&c)
	return c
}

func TestConfig_Warnings(t *testing.T) {
	t.Run("без type — без предупреждений", func(t *testing.T) {
		c := defaultValidConfig()
		if w := c.Warnings(); len(w) != 0 {
			t.Errorf("ожидалось 0 предупреждений, получено %d: %v", len(w), w)
		}
	})
	t.Run("type=openai — без предупреждений", func(t *testing.T) {
		c := defaultValidConfig()
		c.Backends[0].Type = "openai"
		if w := c.Warnings(); len(w) != 0 {
			t.Errorf("ожидалось 0 предупреждений, получено %d: %v", len(w), w)
		}
	})
	t.Run("type=ollama — одно предупреждение про reserved field", func(t *testing.T) {
		c := defaultValidConfig()
		c.Backends[0].Type = "ollama"
		w := c.Warnings()
		if len(w) != 1 {
			t.Fatalf("ожидалось 1 предупреждение, получено %d: %v", len(w), w)
		}
		if !strings.Contains(w[0], "зарезервировано") || !strings.Contains(w[0], "ollama") {
			t.Errorf("неожиданный текст: %s", w[0])
		}
	})
	t.Run("type=ollama не блокирует Validate", func(t *testing.T) {
		c := defaultValidConfig()
		c.Backends[0].Type = "ollama"
		if err := c.Validate(); err != nil {
			t.Errorf("type=ollama должно быть валидным, получено: %v", err)
		}
	})
}

func TestApplyDefaults(t *testing.T) {
	c := Config{
		Backends: []Backend{{Name: "srv1", URL: "http://1.1.1.1"}},
	}
	applyDefaults(&c)
	if c.Proxy.Port != 8080 {
		t.Errorf("Proxy.Port default = %d, want 8080", c.Proxy.Port)
	}
	if c.Routing.Strategy != StrategyModelAffinityLeastBusy {
		t.Errorf("Routing.Strategy default = %s", c.Routing.Strategy)
	}
	if c.Retry.MaxAttempts != 3 {
		t.Errorf("Retry.MaxAttempts default = %d", c.Retry.MaxAttempts)
	}
	if c.Backends[0].TimeoutSeconds != 600 {
		t.Errorf("Backend.TimeoutSeconds default = %d", c.Backends[0].TimeoutSeconds)
	}
}
