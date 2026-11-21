package config

import (
	"fmt"
	"strings"
)

// FieldKind — тип поля формы редактора конфигурации.
type FieldKind string

const (
	KindInt    FieldKind = "int"
	KindString FieldKind = "string"
	KindBool   FieldKind = "bool"
	KindEnum   FieldKind = "enum"
	KindSecret FieldKind = "secret"
)

// ApplyClass — как изменение поля применяется в работающем daemon'е.
type ApplyClass string

const (
	// ApplyHot — применяется сразу, без перезапуска.
	ApplyHot ApplyClass = "hot"
	// ApplyRestart — вступает в силу только после перезапуска daemon'а.
	ApplyRestart ApplyClass = "restart"
)

// Section — раздел формы редактора.
type Section struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// List — путь списка, если раздел состоит из элементов (clients, backends).
	List string `json:"list,omitempty"`
}

// FieldSpec описывает одно поле конфигурации. Для полей элементов списка Path
// содержит "[]" вместо имени элемента: "backends[].url", "auth.api_keys[].name".
type FieldSpec struct {
	Path    string     `json:"path"`
	Section string     `json:"section"`
	Label   string     `json:"label"`
	Help    string     `json:"help,omitempty"`
	Kind    FieldKind  `json:"kind"`
	Enum    []string   `json:"enum,omitempty"`
	Apply   ApplyClass `json:"apply"`
	Min     *int       `json:"min,omitempty"`
	Max     *int       `json:"max,omitempty"`
}

// Schema — каталог полей: единый источник правды и для daemon'а, и для формы TUI.
type Schema struct {
	Sections []Section   `json:"sections"`
	Fields   []FieldSpec `json:"fields"`
}

// FieldError — ошибка валидации конкретного поля.
type FieldError struct {
	Path string `json:"path"`
	Msg  string `json:"msg"`
}

func (e FieldError) Error() string { return e.Path + ": " + e.Msg }

func intp(v int) *int { return &v }

// Секретные поля (значения никогда не отдаются клиентам целиком).
const (
	PathAdminKey = "auth.admin_key"
)

// ConfigSchema возвращает каталог всех редактируемых полей Config в порядке
// config.example.yaml. backends[].models — список строк; Kind=string,
// TUI редактирует его как значения через запятую.
func ConfigSchema() Schema {
	logLevels := []string{LogLevelDebug, LogLevelInfo, LogLevelWarning, LogLevelError}
	strategies := []string{
		StrategyModelAffinityLeastBusy, StrategyRoundRobin, StrategyLeastBusy,
		StrategyDeferredModelThenCap, StrategyPreserveModelCoverage, StrategyFairShareRoundRobin,
	}
	backendTypes := []string{BackendTypeOpenAI, BackendTypeOllama, BackendTypeLMStudio, BackendTypeLlamaCpp, BackendTypeAnthropic}
	formatModes := []string{ResponseFormatPassthrough, ResponseFormatNormalizeJSONObject, ResponseFormatStrictReject}

	return Schema{
		Sections: []Section{
			{ID: "proxy", Label: "Proxy"},
			{ID: "clients", Label: "Clients", List: "auth.api_keys"},
			{ID: "admin", Label: "Admin"},
			{ID: "routing", Label: "Routing"},
			{ID: "retry", Label: "Retry"},
			{ID: "discovery", Label: "Discovery"},
			{ID: "storage", Label: "Storage"},
			{ID: "tui", Label: "TUI"},
			{ID: "compat", Label: "Compat"},
			{ID: "backends", Label: "Backends", List: "backends"},
		},
		Fields: []FieldSpec{
			{Path: "proxy.host", Section: "proxy", Label: "Host", Help: "Listen address", Kind: KindString, Apply: ApplyRestart},
			{Path: "proxy.port", Section: "proxy", Label: "Port", Help: "Listen port", Kind: KindInt, Apply: ApplyRestart, Min: intp(1), Max: intp(65535)},
			{Path: "proxy.log_level", Section: "proxy", Label: "Log level", Kind: KindEnum, Enum: logLevels, Apply: ApplyHot},

			{Path: "auth.api_keys[].name", Section: "clients", Label: "Name", Help: "Client name shown in logs and history", Kind: KindString, Apply: ApplyHot},
			{Path: "auth.api_keys[].key", Section: "clients", Label: "Key", Help: "Bearer key of the client", Kind: KindSecret, Apply: ApplyHot},

			{Path: PathAdminKey, Section: "admin", Label: "Admin key", Help: "Key for the TUI / Web UI / admin endpoints", Kind: KindSecret, Apply: ApplyRestart},

			{Path: "routing.strategy", Section: "routing", Label: "Strategy", Kind: KindEnum, Enum: strategies, Apply: ApplyHot},
			{Path: "scheduler.max_consecutive_per_model", Section: "routing", Label: "Max consecutive per model", Help: "fair_share_round_robin only; 0 = off", Kind: KindInt, Apply: ApplyHot, Min: intp(0)},

			{Path: "retry.max_attempts", Section: "retry", Label: "Max attempts", Help: "Total attempts per request", Kind: KindInt, Apply: ApplyHot, Min: intp(1)},
			{Path: "retry.initial_backoff_ms", Section: "retry", Label: "Initial backoff ms", Kind: KindInt, Apply: ApplyHot, Min: intp(0)},
			{Path: "retry.max_backoff_ms", Section: "retry", Label: "Max backoff ms", Kind: KindInt, Apply: ApplyHot, Min: intp(0)},

			{Path: "discovery.enabled", Section: "discovery", Label: "Enabled", Help: "Periodic /v1/models polling", Kind: KindBool, Apply: ApplyRestart},
			{Path: "discovery.interval_seconds", Section: "discovery", Label: "Interval seconds", Kind: KindInt, Apply: ApplyHot, Min: intp(1)},
			{Path: "discovery.unhealthy_after_failed_polls", Section: "discovery", Label: "Unhealthy after failed polls", Kind: KindInt, Apply: ApplyHot, Min: intp(1)},

			{Path: "storage.database_path", Section: "storage", Label: "Database path", Kind: KindString, Apply: ApplyRestart},
			{Path: "storage.history_retention_days", Section: "storage", Label: "History retention days", Kind: KindInt, Apply: ApplyHot, Min: intp(0)},
			{Path: "storage.vacuum_on_start", Section: "storage", Label: "Vacuum on start", Kind: KindBool, Apply: ApplyRestart},

			{Path: "tui.show_completed_minutes", Section: "tui", Label: "Show completed minutes", Kind: KindInt, Apply: ApplyHot, Min: intp(0)},

			{Path: "compat.response_format_mode", Section: "compat", Label: "Response format mode", Kind: KindEnum, Enum: formatModes, Apply: ApplyHot},

			{Path: "backends[].name", Section: "backends", Label: "Name", Kind: KindString, Apply: ApplyRestart},
			{Path: "backends[].url", Section: "backends", Label: "URL", Kind: KindString, Apply: ApplyRestart},
			{Path: "backends[].type", Section: "backends", Label: "Type", Kind: KindEnum, Enum: backendTypes, Apply: ApplyRestart},
			{Path: "backends[].priority", Section: "backends", Label: "Priority", Help: "Lower = preferred", Kind: KindInt, Apply: ApplyRestart},
			{Path: "backends[].timeout_seconds", Section: "backends", Label: "Timeout seconds", Kind: KindInt, Apply: ApplyRestart, Min: intp(1)},
			{Path: "backends[].api_key", Section: "backends", Label: "API key", Help: "Upstream key, optional", Kind: KindSecret, Apply: ApplyRestart},
			{Path: "backends[].models", Section: "backends", Label: "Models", Help: "Comma-separated; if set, discovery is off for this server", Kind: KindString, Apply: ApplyRestart},
		},
	}
}

// ClassOf возвращает класс применения для пути (скалярного, "backends[gpu-1].url",
// "backends[gpu-1]", "auth.api_keys[crm]" и т. п.). Неизвестные пути считаются
// restart — безопасный вариант.
func ClassOf(path string) ApplyClass {
	p := stripItemKey(path)
	for _, f := range schemaIndex {
		if f.Path == p {
			return f.Apply
		}
	}
	switch {
	case p == "auth.api_keys" || strings.HasPrefix(p, "auth.api_keys[]"):
		return ApplyHot
	case p == "backends" || strings.HasPrefix(p, "backends[]"):
		return ApplyRestart
	}
	return ApplyRestart
}

var schemaIndex = ConfigSchema().Fields

// stripItemKey превращает "backends[gpu-1].url" в "backends[].url".
func stripItemKey(path string) string {
	var b strings.Builder
	depth := 0
	for _, r := range path {
		switch {
		case r == '[' && depth == 0:
			depth++
			b.WriteString("[]")
		case r == ']' && depth > 0:
			depth--
		case depth > 0:
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// itemPath строит конкретный путь элемента списка: "backends[gpu-1]".
func itemPath(list, key string) string { return fmt.Sprintf("%s[%s]", list, key) }

// itemKeys возвращает для каждого элемента ключ адресации: имя, если оно не
// пустое и уникально, иначе "#<индекс>" (0-based).
func itemKeys(names []string) []string {
	count := make(map[string]int, len(names))
	for _, n := range names {
		count[n]++
	}
	out := make([]string, len(names))
	for i, n := range names {
		if n == "" || count[n] > 1 {
			out[i] = fmt.Sprintf("#%d", i)
		} else {
			out[i] = n
		}
	}
	return out
}

func backendNames(bs []Backend) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.Name
	}
	return out
}

func apiKeyNames(ks []APIKey) []string {
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = k.Name
	}
	return out
}
