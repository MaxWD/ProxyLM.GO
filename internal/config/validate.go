package config

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// fieldIssue — внутренняя ошибка валидации: FieldError (путь + английский текст для
// форм редактора) и legacy-представление (раздел + русский текст) для Validate().
type fieldIssue struct {
	FieldError
	section string
	ru      string
}

type issueList []fieldIssue

func (l *issueList) add(section, path, msg, ru string) {
	*l = append(*l, fieldIssue{FieldError: FieldError{Path: path, Msg: msg}, section: section, ru: ru})
}

// Validate проверяет конфиг на согласованность. Возвращает первую найденную ошибку
// (агрегации нет — для CLI это удобнее, чтобы пользователь чинил по одной).
// Все ошибки с путями полей — через ValidateFields.
func (c *Config) Validate() error {
	issues := c.issues()
	if len(issues) == 0 {
		return nil
	}
	first := issues[0]
	return fmt.Errorf("%s: %s", first.section, first.ru)
}

// ValidateFields возвращает ВСЕ найденные ошибки, каждая — с конкретным путём поля
// (элементы списков адресуются по имени: "backends[gpu-1].url", при пустом или
// повторяющемся имени — по индексу: "backends[#2].url").
func (c *Config) ValidateFields() []FieldError {
	issues := c.issues()
	if len(issues) == 0 {
		return nil
	}
	out := make([]FieldError, len(issues))
	for i, is := range issues {
		out[i] = is.FieldError
	}
	return out
}

func (c *Config) issues() issueList {
	var l issueList
	c.Proxy.validate(&l)
	c.Auth.validate(&l)
	c.Routing.validate(&l)
	c.Scheduler.validate(&l)
	c.Retry.validate(&l)
	c.Discovery.validate(&l)
	c.Storage.validate(&l)
	c.TUI.validate(&l)
	c.Compat.validate(&l)
	validateBackends(&l, c.Backends)
	return l
}

func (p *Proxy) validate(l *issueList) {
	if p.Host == "" {
		l.add("proxy", "proxy.host", "required", "host обязателен")
	}
	if !isValidPort(p.Port) {
		l.add("proxy", "proxy.port", fmt.Sprintf("must be in 1..65535, got %d", p.Port),
			fmt.Sprintf("port вне диапазона 1..65535: %d", p.Port))
	}
	switch strings.ToLower(p.LogLevel) {
	case LogLevelDebug, LogLevelInfo, LogLevelWarning, "warn", LogLevelError:
	default:
		l.add("proxy", "proxy.log_level", fmt.Sprintf("expected debug|info|warning|error, got %q", p.LogLevel),
			fmt.Sprintf("log_level: ожидалось debug|info|warning|error, получено %q", p.LogLevel))
	}
}

func (a *Auth) validate(l *issueList) {
	if len(a.APIKeys) == 0 {
		l.add("auth", "auth.api_keys", "at least one key is required", "api_keys: список пустой — нужен хотя бы один ключ")
	}
	keys := itemKeys(apiKeyNames(a.APIKeys))
	seenName := map[string]bool{}
	seenKey := map[string]bool{}
	for i, k := range a.APIKeys {
		base := itemPath("auth.api_keys", keys[i])
		if k.Name == "" {
			l.add("auth", base+".name", "required", fmt.Sprintf("api_keys[%d].name пустое", i))
			continue
		}
		if k.Key == "" {
			l.add("auth", base+".key", "required", fmt.Sprintf("api_keys[%d].key пустое", i))
			continue
		}
		if seenName[k.Name] {
			l.add("auth", base+".name", fmt.Sprintf("duplicate name %q", k.Name), fmt.Sprintf("api_keys: дубликат name=%q", k.Name))
			continue
		}
		if seenKey[k.Key] {
			l.add("auth", base+".key", "duplicate key", fmt.Sprintf("api_keys: дубликат key для name=%q", k.Name))
			continue
		}
		seenName[k.Name] = true
		seenKey[k.Key] = true
	}
	if a.AdminKey == "" {
		l.add("auth", PathAdminKey, "required", "admin_key пустой")
	} else if seenKey[a.AdminKey] {
		l.add("auth", PathAdminKey, "must differ from every client key", "admin_key совпадает с одним из api_keys[].key")
	}
}

func (r *Routing) validate(l *issueList) {
	switch r.Strategy {
	case StrategyModelAffinityLeastBusy,
		StrategyRoundRobin,
		StrategyLeastBusy,
		StrategyDeferredModelThenCap,
		StrategyPreserveModelCoverage,
		StrategyFairShareRoundRobin:
	default:
		l.add("routing", "routing.strategy", fmt.Sprintf("unknown strategy %q", r.Strategy),
			fmt.Sprintf("strategy: неизвестная стратегия %q", r.Strategy))
	}
}

func (s *Scheduler) validate(l *issueList) {
	if s.MaxConsecutivePerModel < 0 {
		l.add("scheduler", "scheduler.max_consecutive_per_model", fmt.Sprintf("must be >= 0, got %d", s.MaxConsecutivePerModel),
			fmt.Sprintf("max_consecutive_per_model ≥ 0, получено %d", s.MaxConsecutivePerModel))
	}
}

func (r *Retry) validate(l *issueList) {
	if r.MaxAttempts < 1 {
		l.add("retry", "retry.max_attempts", fmt.Sprintf("must be >= 1, got %d", r.MaxAttempts),
			fmt.Sprintf("max_attempts ≥ 1, получено %d", r.MaxAttempts))
	}
	if r.InitialBackoffMs < 0 {
		l.add("retry", "retry.initial_backoff_ms", fmt.Sprintf("must be >= 0, got %d", r.InitialBackoffMs),
			fmt.Sprintf("initial_backoff_ms ≥ 0, получено %d", r.InitialBackoffMs))
	}
	if r.MaxBackoffMs < r.InitialBackoffMs {
		l.add("retry", "retry.max_backoff_ms", fmt.Sprintf("must be >= initial_backoff_ms (%d), got %d", r.InitialBackoffMs, r.MaxBackoffMs),
			fmt.Sprintf("max_backoff_ms (%d) < initial_backoff_ms (%d)", r.MaxBackoffMs, r.InitialBackoffMs))
	}
}

func (d *Discovery) validate(l *issueList) {
	if d.IntervalSeconds < 1 {
		l.add("discovery", "discovery.interval_seconds", fmt.Sprintf("must be >= 1, got %d", d.IntervalSeconds),
			fmt.Sprintf("interval_seconds ≥ 1, получено %d", d.IntervalSeconds))
	}
	if d.UnhealthyAfterFailedPolls < 1 {
		l.add("discovery", "discovery.unhealthy_after_failed_polls", fmt.Sprintf("must be >= 1, got %d", d.UnhealthyAfterFailedPolls),
			fmt.Sprintf("unhealthy_after_failed_polls ≥ 1, получено %d", d.UnhealthyAfterFailedPolls))
	}
}

func (s *Storage) validate(l *issueList) {
	if s.DatabasePath == "" {
		l.add("storage", "storage.database_path", "required", "database_path пустой")
	}
	if s.HistoryRetentionDays < 0 {
		l.add("storage", "storage.history_retention_days", fmt.Sprintf("must be >= 0, got %d", s.HistoryRetentionDays),
			fmt.Sprintf("history_retention_days ≥ 0, получено %d", s.HistoryRetentionDays))
	}
}

func (t *TUI) validate(l *issueList) {
	if t.ShowCompletedMinutes < 0 {
		l.add("tui", "tui.show_completed_minutes", fmt.Sprintf("must be >= 0, got %d", t.ShowCompletedMinutes),
			fmt.Sprintf("show_completed_minutes ≥ 0, получено %d", t.ShowCompletedMinutes))
	}
}

func (c *Compat) validate(l *issueList) {
	switch c.ResponseFormatMode {
	case ResponseFormatPassthrough, ResponseFormatNormalizeJSONObject, ResponseFormatStrictReject:
	default:
		l.add("compat", "compat.response_format_mode", fmt.Sprintf("expected passthrough|normalize_json_object|strict_reject, got %q", c.ResponseFormatMode),
			fmt.Sprintf("response_format_mode: ожидалось passthrough|normalize_json_object|strict_reject, получено %q", c.ResponseFormatMode))
	}
}

func validateBackends(l *issueList, bs []Backend) {
	if len(bs) == 0 {
		l.add("backends", "backends", "at least one backend is required", "список backends пуст — нужен хотя бы один")
		return
	}
	keys := itemKeys(backendNames(bs))
	seenName := map[string]bool{}
	for i, b := range bs {
		base := itemPath("backends", keys[i])
		if b.Name == "" {
			l.add("backends", base+".name", "required", fmt.Sprintf("backends[%d].name пустое", i))
			continue
		}
		if seenName[b.Name] {
			l.add("backends", base+".name", fmt.Sprintf("duplicate name %q", b.Name), fmt.Sprintf("backends: дубликат name=%q", b.Name))
			continue
		}
		seenName[b.Name] = true
		if en, ru := backendURLProblem(b.URL); en != "" {
			l.add("backends", base+".url", en, fmt.Sprintf("backends[%q].url: %s", b.Name, ru))
		}
		switch b.Type {
		case "", BackendTypeOpenAI, BackendTypeOllama, BackendTypeLMStudio, BackendTypeLlamaCpp, BackendTypeAnthropic:
		default:
			l.add("backends", base+".type", fmt.Sprintf("expected openai|ollama|lmstudio|llamacpp|anthropic, got %q", b.Type),
				fmt.Sprintf("backends[%q].type: ожидалось openai|ollama|lmstudio|llamacpp|anthropic, получено %q", b.Name, b.Type))
		}
		if b.TimeoutSeconds < 1 {
			l.add("backends", base+".timeout_seconds", fmt.Sprintf("must be >= 1, got %d", b.TimeoutSeconds),
				fmt.Sprintf("backends[%q].timeout_seconds ≥ 1, получено %d", b.Name, b.TimeoutSeconds))
		}
	}
}

// backendURLProblem возвращает описание проблемы URL на английском и русском
// ("" — URL корректен).
func backendURLProblem(raw string) (en, ru string) {
	if raw == "" {
		return "required", "url пустой"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "invalid URL: " + err.Error(), fmt.Sprintf("невалидный URL: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Sprintf("scheme must be http or https, got %q", u.Scheme), fmt.Sprintf("схема должна быть http|https, получено %q", u.Scheme)
	}
	if u.Host == "" {
		return "host is empty", "host пустой"
	}
	// host может быть с портом — net.SplitHostPort обработает оба случая
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		host = u.Host
	} else if _, err := strconv.Atoi(port); err != nil {
		return "invalid port in URL", "невалидный port в URL"
	}
	if host == "" {
		return "host is empty", "host пустой"
	}
	return "", ""
}

func isValidPort(p int) bool { return p >= 1 && p <= 65535 }

// Warnings возвращает non-fatal замечания к конфигу — то, что не блокирует старт,
// но стоит показать оператору при загрузке.
func (c *Config) Warnings() []string {
	return nil
}
