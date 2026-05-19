package config

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Validate проверяет конфиг на согласованность. Возвращает первую найденную ошибку
// (агрегации нет — для CLI это удобнее, чтобы пользователь чинил по одной).
func (c *Config) Validate() error {
	if err := c.Proxy.validate(); err != nil {
		return fmt.Errorf("proxy: %w", err)
	}
	if err := c.Auth.validate(); err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	if err := c.Routing.validate(); err != nil {
		return fmt.Errorf("routing: %w", err)
	}
	if err := c.Retry.validate(); err != nil {
		return fmt.Errorf("retry: %w", err)
	}
	if err := c.Discovery.validate(); err != nil {
		return fmt.Errorf("discovery: %w", err)
	}
	if err := c.Storage.validate(); err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	if err := c.TUI.validate(); err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	if err := c.Compat.validate(); err != nil {
		return fmt.Errorf("compat: %w", err)
	}
	if err := validateBackends(c.Backends); err != nil {
		return fmt.Errorf("backends: %w", err)
	}
	return nil
}

func (p *Proxy) validate() error {
	if p.Host == "" {
		return fmt.Errorf("host обязателен")
	}
	if !isValidPort(p.Port) {
		return fmt.Errorf("port вне диапазона 1..65535: %d", p.Port)
	}
	switch strings.ToLower(p.LogLevel) {
	case LogLevelDebug, LogLevelInfo, LogLevelWarning, "warn", LogLevelError:
	default:
		return fmt.Errorf("log_level: ожидалось debug|info|warning|error, получено %q", p.LogLevel)
	}
	return nil
}

func (a *Auth) validate() error {
	if len(a.APIKeys) == 0 {
		return fmt.Errorf("api_keys: список пустой — нужен хотя бы один ключ")
	}
	seenName := map[string]bool{}
	seenKey := map[string]bool{}
	for i, k := range a.APIKeys {
		if k.Name == "" {
			return fmt.Errorf("api_keys[%d].name пустое", i)
		}
		if k.Key == "" {
			return fmt.Errorf("api_keys[%d].key пустое", i)
		}
		if seenName[k.Name] {
			return fmt.Errorf("api_keys: дубликат name=%q", k.Name)
		}
		if seenKey[k.Key] {
			return fmt.Errorf("api_keys: дубликат key для name=%q", k.Name)
		}
		seenName[k.Name] = true
		seenKey[k.Key] = true
	}
	if a.AdminKey == "" {
		return fmt.Errorf("admin_key пустой")
	}
	if seenKey[a.AdminKey] {
		return fmt.Errorf("admin_key совпадает с одним из api_keys[].key")
	}
	return nil
}

func (r *Routing) validate() error {
	switch r.Strategy {
	case StrategyModelAffinityLeastBusy,
		StrategyRoundRobin,
		StrategyLeastBusy,
		StrategyDeferredModelThenCap,
		StrategyPreserveModelCoverage:
		return nil
	default:
		return fmt.Errorf("strategy: неизвестная стратегия %q", r.Strategy)
	}
}

func (r *Retry) validate() error {
	if r.MaxAttempts < 1 {
		return fmt.Errorf("max_attempts ≥ 1, получено %d", r.MaxAttempts)
	}
	if r.InitialBackoffMs < 0 {
		return fmt.Errorf("initial_backoff_ms ≥ 0, получено %d", r.InitialBackoffMs)
	}
	if r.MaxBackoffMs < r.InitialBackoffMs {
		return fmt.Errorf("max_backoff_ms (%d) < initial_backoff_ms (%d)", r.MaxBackoffMs, r.InitialBackoffMs)
	}
	return nil
}

func (d *Discovery) validate() error {
	if d.IntervalSeconds < 1 {
		return fmt.Errorf("interval_seconds ≥ 1, получено %d", d.IntervalSeconds)
	}
	if d.UnhealthyAfterFailedPolls < 1 {
		return fmt.Errorf("unhealthy_after_failed_polls ≥ 1, получено %d", d.UnhealthyAfterFailedPolls)
	}
	return nil
}

func (s *Storage) validate() error {
	if s.DatabasePath == "" {
		return fmt.Errorf("database_path пустой")
	}
	if s.HistoryRetentionDays < 0 {
		return fmt.Errorf("history_retention_days ≥ 0, получено %d", s.HistoryRetentionDays)
	}
	return nil
}

func (t *TUI) validate() error {
	if t.ShowCompletedMinutes < 0 {
		return fmt.Errorf("show_completed_minutes ≥ 0, получено %d", t.ShowCompletedMinutes)
	}
	return nil
}

func (c *Compat) validate() error {
	switch c.ResponseFormatMode {
	case ResponseFormatPassthrough, ResponseFormatNormalizeJSONObject, ResponseFormatStrictReject:
		return nil
	default:
		return fmt.Errorf("response_format_mode: ожидалось passthrough|normalize_json_object|strict_reject, получено %q", c.ResponseFormatMode)
	}
}

func validateBackends(bs []Backend) error {
	if len(bs) == 0 {
		return fmt.Errorf("список backends пуст — нужен хотя бы один")
	}
	seenName := map[string]bool{}
	for i, b := range bs {
		if b.Name == "" {
			return fmt.Errorf("backends[%d].name пустое", i)
		}
		if seenName[b.Name] {
			return fmt.Errorf("backends: дубликат name=%q", b.Name)
		}
		seenName[b.Name] = true
		if err := validateBackendURL(b.URL); err != nil {
			return fmt.Errorf("backends[%q].url: %w", b.Name, err)
		}
		// backends[].type — reserved for future native-protocol backends
		// (Ollama /api/*, Anthropic, Gemini). MVP ignores the value;
		// applyDefaults() turns "" into "openai". Unknown values are accepted
		// but surface in Config.Warnings().
		if b.TimeoutSeconds < 1 {
			return fmt.Errorf("backends[%q].timeout_seconds ≥ 1, получено %d", b.Name, b.TimeoutSeconds)
		}
	}
	return nil
}

func validateBackendURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("url пустой")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("невалидный URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("схема должна быть http|https, получено %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("host пустой")
	}
	// host может быть с портом — net.SplitHostPort обработает оба случая
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		host = u.Host
	} else {
		if _, err := strconv.Atoi(port); err != nil {
			return fmt.Errorf("невалидный port в URL")
		}
	}
	if host == "" {
		return fmt.Errorf("host пустой")
	}
	return nil
}

func isValidPort(p int) bool { return p >= 1 && p <= 65535 }

// Warnings возвращает non-fatal замечания к конфигу — то, что не блокирует старт,
// но стоит показать оператору при загрузке. Сейчас покрывает поле
// backends[].type: значения, отличные от "openai", принимаются (поле зарезервировано
// под будущие native-протоколы — Ollama /api/*, Anthropic, Gemini), но в MVP
// игнорируются — об этом полезно предупредить, иначе пользователь будет думать,
// что type: ollama что-то меняет в поведении прокси.
func (c *Config) Warnings() []string {
	var out []string
	for _, b := range c.Backends {
		if b.Type != "" && b.Type != BackendTypeOpenAI {
			out = append(out,
				fmt.Sprintf("backends[%q].type=%q: поле зарезервировано под будущие native-протоколы и в MVP игнорируется; прокси использует OpenAI-совместимый /v1/* shim для всех бэкендов",
					b.Name, b.Type))
		}
	}
	return out
}
