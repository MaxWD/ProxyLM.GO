package config

// Config — корневая структура конфига proxylm (десериализация из YAML).
//
// Имена YAML-ключей совпадают с config.example.yaml; теги — explicit,
// чтобы переименование Go-полей не ломало парсинг.
type Config struct {
	Proxy     Proxy     `yaml:"proxy"`
	Auth      Auth      `yaml:"auth"`
	Routing   Routing   `yaml:"routing"`
	Retry     Retry     `yaml:"retry"`
	Discovery Discovery `yaml:"discovery"`
	Storage   Storage   `yaml:"storage"`
	TUI       TUI       `yaml:"tui"`
	Compat    Compat    `yaml:"compat"`
	Backends  []Backend `yaml:"backends"`
}

type Proxy struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	LogLevel string `yaml:"log_level"`
}

type Auth struct {
	APIKeys  []APIKey `yaml:"api_keys"`
	AdminKey string   `yaml:"admin_key"`
}

type APIKey struct {
	Name string `yaml:"name"`
	Key  string `yaml:"key"`
}

type Routing struct {
	Strategy string `yaml:"strategy"`
}

type Retry struct {
	MaxAttempts      int `yaml:"max_attempts"`
	InitialBackoffMs int `yaml:"initial_backoff_ms"`
	MaxBackoffMs     int `yaml:"max_backoff_ms"`
}

type Discovery struct {
	Enabled                   bool `yaml:"enabled"`
	IntervalSeconds           int  `yaml:"interval_seconds"`
	UnhealthyAfterFailedPolls int  `yaml:"unhealthy_after_failed_polls"`
}

type Storage struct {
	DatabasePath         string `yaml:"database_path"`
	HistoryRetentionDays int    `yaml:"history_retention_days"`
	VacuumOnStart        bool   `yaml:"vacuum_on_start"`
}

type TUI struct {
	ShowCompletedMinutes int `yaml:"show_completed_minutes"`
}

type Compat struct {
	ResponseFormatMode string `yaml:"response_format_mode"`
}

type Backend struct {
	Name     string `yaml:"name"`
	URL      string `yaml:"url"`
	Priority int    `yaml:"priority"`
	// Type зарезервировано под будущие native-протоколы (Ollama /api/*,
	// Anthropic, Gemini). В MVP игнорируется: прокси одинаково работает с любым
	// OpenAI-совместимым /v1/* сервером (LM Studio, Ollama OpenAI-shim, vLLM,
	// llama.cpp, OpenRouter, Groq, Together AI, OpenAI, …). Пустое значение
	// заполняется через applyDefaults() как "openai".
	Type           string   `yaml:"type"`
	TimeoutSeconds int      `yaml:"timeout_seconds"`
	APIKey         string   `yaml:"api_key"`
	Models         []string `yaml:"models"` // если задан — discovery для этого сервера отключён
}

// Допустимые значения для перечислимых полей. Используются в Validate и для подсказок.
const (
	StrategyModelAffinityLeastBusy = "model_affinity_least_busy"
	StrategyRoundRobin             = "round_robin"
	StrategyLeastBusy              = "least_busy"
	StrategyDeferredModelThenCap   = "deferred_model_then_capable"
	StrategyPreserveModelCoverage  = "preserve_model_coverage"

	BackendTypeOpenAI = "openai"
	BackendTypeOllama = "ollama"

	ResponseFormatPassthrough         = "passthrough"
	ResponseFormatNormalizeJSONObject = "normalize_json_object"
	ResponseFormatStrictReject        = "strict_reject"

	LogLevelDebug   = "debug"
	LogLevelInfo    = "info"
	LogLevelWarning = "warning"
	LogLevelError   = "error"
)
