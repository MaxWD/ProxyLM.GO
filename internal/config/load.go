package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// LoadOrCreate загружает конфиг из path. Если файла нет — записывает defaultTemplate
// и возвращает распаршенный конфиг. Возвращает (config, createdPath, error):
// createdPath != "" означает, что файл был только что создан.
//
// path должен быть абсолютным (резолв относительно бинарника — задача вызывающего:
// см. DefaultConfigPath()).
func LoadOrCreate(path string, defaultTemplate []byte) (*Config, string, error) {
	created := ""
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, "", fmt.Errorf("create config dir: %w", err)
		}
		if err := os.WriteFile(path, defaultTemplate, 0o644); err != nil {
			return nil, "", fmt.Errorf("write default config: %w", err)
		}
		created = path
	} else if err != nil {
		return nil, "", fmt.Errorf("stat config: %w", err)
	}

	cfg, err := Load(path)
	if err != nil {
		return nil, created, err
	}
	return cfg, created, nil
}

// Load читает и парсит YAML-файл. Validate не вызывается — это отдельный шаг.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	applyDefaults(cfg)
	return cfg, nil
}

// DefaultConfigPath возвращает <dir(executable)>/config.yaml.
// При недоступности os.Executable() — фоллбэк на ./config.yaml.
func DefaultConfigPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "config.yaml"
	}
	return filepath.Join(filepath.Dir(exe), "config.yaml")
}

// ResolveRelative делает path абсолютным относительно каталога бинарника.
// Используется для storage.database_path и других портабельных путей.
func ResolveRelative(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	exe, err := os.Executable()
	if err != nil {
		return path
	}
	return filepath.Join(filepath.Dir(exe), path)
}

// applyDefaults подставляет значения по умолчанию для пропущенных полей.
// Дефолты совпадают с config.example.yaml, чтобы поведение прокси без явных
// настроек было предсказуемым.
func applyDefaults(c *Config) {
	if c.Proxy.Host == "" {
		c.Proxy.Host = "0.0.0.0"
	}
	if c.Proxy.Port == 0 {
		c.Proxy.Port = 8080
	}
	if c.Proxy.LogLevel == "" {
		c.Proxy.LogLevel = LogLevelInfo
	}

	if c.Routing.Strategy == "" {
		c.Routing.Strategy = StrategyModelAffinityLeastBusy
	}

	if c.Retry.MaxAttempts == 0 {
		c.Retry.MaxAttempts = 3
	}
	if c.Retry.InitialBackoffMs == 0 {
		c.Retry.InitialBackoffMs = 500
	}
	if c.Retry.MaxBackoffMs == 0 {
		c.Retry.MaxBackoffMs = 8000
	}

	if c.Discovery.IntervalSeconds == 0 {
		c.Discovery.IntervalSeconds = 30
	}
	if c.Discovery.UnhealthyAfterFailedPolls == 0 {
		c.Discovery.UnhealthyAfterFailedPolls = 3
	}

	if c.Storage.DatabasePath == "" {
		c.Storage.DatabasePath = "./proxylm.db"
	}
	if c.Storage.HistoryRetentionDays == 0 {
		c.Storage.HistoryRetentionDays = 30
	}

	if c.TUI.ShowCompletedMinutes == 0 {
		c.TUI.ShowCompletedMinutes = 30
	}

	if c.Compat.ResponseFormatMode == "" {
		c.Compat.ResponseFormatMode = ResponseFormatPassthrough
	}

	for i := range c.Backends {
		if c.Backends[i].TimeoutSeconds == 0 {
			c.Backends[i].TimeoutSeconds = 600
		}
		if c.Backends[i].Type == "" {
			c.Backends[i].Type = BackendTypeOpenAI
		}
	}
}
