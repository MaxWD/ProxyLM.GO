package config

// Config — корневая структура конфига proxylm (десериализация из YAML).
//
// Имена YAML-ключей совпадают с config.example.yaml; теги — explicit,
// чтобы переименование Go-полей не ломало парсинг.
type Config struct {
	Proxy     Proxy     `yaml:"proxy" json:"proxy"`
	Auth      Auth      `yaml:"auth" json:"auth"`
	Routing   Routing   `yaml:"routing" json:"routing"`
	Scheduler Scheduler `yaml:"scheduler" json:"scheduler"`
	Retry     Retry     `yaml:"retry" json:"retry"`
	Discovery Discovery `yaml:"discovery" json:"discovery"`
	Storage   Storage   `yaml:"storage" json:"storage"`
	TUI       TUI       `yaml:"tui" json:"tui"`
	Compat    Compat    `yaml:"compat" json:"compat"`
	Backends  []Backend `yaml:"backends" json:"backends"`
}

type Proxy struct {
	Host     string `yaml:"host" json:"host"`
	Port     int    `yaml:"port" json:"port"`
	LogLevel string `yaml:"log_level" json:"log_level"`
}

type Auth struct {
	APIKeys  []APIKey `yaml:"api_keys" json:"api_keys"`
	AdminKey string   `yaml:"admin_key" json:"admin_key"`
}

type APIKey struct {
	Name string `yaml:"name" json:"name"`
	Key  string `yaml:"key" json:"key"`
}

type Routing struct {
	Strategy string `yaml:"strategy" json:"strategy"`
}

// Scheduler — параметры планировщика, не зависящие от выбора стратегии напрямую
// (но некоторые параметры могут активироваться только определёнными стратегиями).
type Scheduler struct {
	// MaxConsecutivePerModel — лимит подряд выполненных задач одной модели
	// на одном сервере перед принудительным переключением. Действует только
	// для стратегии fair_share_round_robin (v0.10.0). 0 = лимит отключён
	// (стратегия деградирует к deferred_model_then_capable). При других
	// стратегиях значение игнорируется (но валидируется).
	MaxConsecutivePerModel int `yaml:"max_consecutive_per_model" json:"max_consecutive_per_model"`
}

type Retry struct {
	MaxAttempts      int `yaml:"max_attempts" json:"max_attempts"`
	InitialBackoffMs int `yaml:"initial_backoff_ms" json:"initial_backoff_ms"`
	MaxBackoffMs     int `yaml:"max_backoff_ms" json:"max_backoff_ms"`
}

type Discovery struct {
	Enabled                   bool `yaml:"enabled" json:"enabled"`
	IntervalSeconds           int  `yaml:"interval_seconds" json:"interval_seconds"`
	UnhealthyAfterFailedPolls int  `yaml:"unhealthy_after_failed_polls" json:"unhealthy_after_failed_polls"`
}

type Storage struct {
	DatabasePath         string `yaml:"database_path" json:"database_path"`
	HistoryRetentionDays int    `yaml:"history_retention_days" json:"history_retention_days"`
	VacuumOnStart        bool   `yaml:"vacuum_on_start" json:"vacuum_on_start"`
}

type TUI struct {
	ShowCompletedMinutes int `yaml:"show_completed_minutes" json:"show_completed_minutes"`
}

type Compat struct {
	ResponseFormatMode string `yaml:"response_format_mode" json:"response_format_mode"`
}

type Backend struct {
	Name     string `yaml:"name" json:"name"`
	URL      string `yaml:"url" json:"url"`
	Priority int    `yaml:"priority" json:"priority"`
	// Type — тип бэкенда. По протоколу делится на два класса:
	//   - "anthropic" — Anthropic Messages API (/v1/messages);
	//   - всё остальное — OpenAI-совместимый протокол (/v1/*).
	// Значения "ollama" | "lmstudio" | "llamacpp" работают по OpenAI-протоколу
	// (эти серверы дают OpenAI-совместимый shim), но дополнительно включают
	// нативную пробу реально загруженных в память моделей (Ollama /api/ps,
	// LM Studio /api/v0/models, llama.cpp /models). "openai" (по умолчанию) —
	// без такой пробы. Пустое значение заполняется applyDefaults() как "openai".
	Type           string   `yaml:"type" json:"type"`
	TimeoutSeconds int      `yaml:"timeout_seconds" json:"timeout_seconds"`
	APIKey         string   `yaml:"api_key" json:"api_key"`
	Models         []string `yaml:"models" json:"models"` // если задан — discovery для этого сервера отключён
}

// Допустимые значения для перечислимых полей. Используются в Validate и для подсказок.
const (
	StrategyModelAffinityLeastBusy = "model_affinity_least_busy"
	StrategyRoundRobin             = "round_robin"
	StrategyLeastBusy              = "least_busy"
	StrategyDeferredModelThenCap   = "deferred_model_then_capable"
	StrategyPreserveModelCoverage  = "preserve_model_coverage"
	StrategyFairShareRoundRobin    = "fair_share_round_robin"

	BackendTypeOpenAI    = "openai"
	BackendTypeOllama    = "ollama"
	BackendTypeLMStudio  = "lmstudio"
	BackendTypeLlamaCpp  = "llamacpp"
	BackendTypeAnthropic = "anthropic"

	ResponseFormatPassthrough         = "passthrough"
	ResponseFormatNormalizeJSONObject = "normalize_json_object"
	ResponseFormatStrictReject        = "strict_reject"

	LogLevelDebug   = "debug"
	LogLevelInfo    = "info"
	LogLevelWarning = "warning"
	LogLevelError   = "error"
)
