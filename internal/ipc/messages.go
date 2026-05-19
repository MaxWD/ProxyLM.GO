package ipc

import "time"

// Тип сообщения, передаваемого через WebSocket /admin/stream.
// Используется как для server→client (TUI), так и (потенциально) для client→server.
type MessageType string

const (
	TypeStateSnapshot   MessageType = "state_snapshot"
	TypeHello           MessageType = "hello"
	TypeRequestSnapshot MessageType = "request_snapshot" // client→server: запрос немедленного snapshot
)

// Envelope — внешний контейнер для всех сообщений. Конкретный payload зависит от Type.
type Envelope struct {
	Type    MessageType `json:"type"`
	Time    time.Time   `json:"time"`
	Payload any         `json:"payload"`
}

// HelloPayload — первое сообщение при подключении: версия daemon'а, число серверов
// и UI-настройки, релевантные для TUI (см. FR-36: TTL для скрытия completed/failed).
type HelloPayload struct {
	Version              string `json:"version"`
	NumServers           int    `json:"num_servers"`
	ShowCompletedMinutes int    `json:"show_completed_minutes,omitempty"`
}

// ServerState — снимок состояния одного backend-сервера.
type ServerState struct {
	Name         string   `json:"name"`
	URL          string   `json:"url"`
	Healthy      bool     `json:"healthy"`
	InFlight     bool     `json:"in_flight"`
	CurrentModel string   `json:"current_model"`
	QueueDepth   int      `json:"queue_depth"`
	Models       []string `json:"models"`

	// PerfSamples — число замеров для регрессии на паре (Name, CurrentModel).
	// 0 ⇒ нет данных. perf_min_samples=3 — порог, ниже которого регрессия не
	// отдаётся (PerfOK=false). omitempty убирает поля у серверов без статистики.
	PerfSamples int  `json:"perf_samples,omitempty"`
	PerfOK      bool `json:"perf_ok,omitempty"`
	// TLoadMs — оценка времени загрузки модели (мс). 0, если ни одного запроса
	// этой модели не сопровождался reload'ом (см. RequestState.ModelReloaded).
	TLoadMs float64 `json:"t_load_ms,omitempty"`
	// TokInPerSec / TokOutPerSec — производительности по входным и выходным
	// токенам, токенов в секунду. Преобразование 1000 / k_in_ms_tok.
	TokInPerSec  float64 `json:"tok_in_per_sec,omitempty"`
	TokOutPerSec float64 `json:"tok_out_per_sec,omitempty"`
	// RSquared — коэффициент детерминации R² регрессии для header-метрики
	// (выбран endpoint с наибольшим числом samples для CurrentModel). [0,1].
	// 0 ⇒ нет данных или fit не определён. См. FUTURE.md #7 → v0.10.0.
	RSquared float64 `json:"r_squared,omitempty"`
	// FitQuality — словесная оценка fit: "good" / "degraded" / "" (нет данных).
	// Порог "good" — R² ≥ 0.70. TUI подсвечивает «degraded» как индикатор того,
	// что метрики ненадёжны без учёта доверительных интервалов.
	FitQuality string `json:"fit_quality,omitempty"`

	// PerModelStats — расширенная статистика для server-modal (см. FR в /docs).
	// Слайс отсортирован по числу samples DESC (см. PerfTracker.ServerSummary).
	// omitempty — экономия трафика для серверов без данных.
	PerModelStats []ModelStats `json:"per_model_stats,omitempty"`

	// Slow — сервер в «медленном» режиме: последний завершённый запрос взял
	// ≥2× от рассчётного по регрессии. Источник — ServerInfo.LastSlow.
	// Сбрасывается на стороне сервера при следующем «нормальном» запросе.
	Slow bool `json:"slow,omitempty"`
	// FailureCount — суммарное число попыток (включая внутренние retry одного
	// запроса), упавших на этом сервере с ошибкой. Источник —
	// ServerInfo.FailureCount; не сбрасывается на стороне daemon'а в течение
	// его жизни. TUI показывает значение в InfoPane.Server.
	FailureCount int64 `json:"failure_count,omitempty"`
}

// ModelStats — агрегат регрессии по одному ключу (Model, Endpoint) на одном
// сервере. Endpoint введён в v0.10.0 (FUTURE.md #3): запросы разных типов
// (/v1/chat/completions, /v1/embeddings) имеют принципиально разные профили
// tokens/ms — смешивать их в одну регрессию нельзя.
//
// RSquared / FitQuality / TLoadCI / KInCI / KOutCI — диагностика fit'а
// (FUTURE.md #7 → v0.10.0). CI-поля — half-width 95% доверительного интервала
// для соответствующего коэффициента.
type ModelStats struct {
	Model        string  `json:"model"`
	Endpoint     string  `json:"endpoint,omitempty"`
	Samples      int     `json:"samples"`
	Loaded       int     `json:"loaded"`
	OK           bool    `json:"ok"`
	TLoadMs      float64 `json:"t_load_ms,omitempty"`
	TokInPerSec  float64 `json:"tok_in_per_sec,omitempty"`
	TokOutPerSec float64 `json:"tok_out_per_sec,omitempty"`
	RSquared     float64 `json:"r_squared,omitempty"`
	FitQuality   string  `json:"fit_quality,omitempty"`
	TLoadCI      float64 `json:"t_load_ci,omitempty"`
	KInCI        float64 `json:"k_in_ci,omitempty"`
	KOutCI       float64 `json:"k_out_ci,omitempty"`
}

// RequestState — снимок одной записи о запросе (актуальной или недавней).
type RequestState struct {
	ID           string    `json:"id"`
	ClientName   string    `json:"client_name"`
	Model        string    `json:"model"`
	Endpoint     string    `json:"endpoint"`
	Stream       bool      `json:"stream"`
	ServerName   string    `json:"server_name"`
	Status       string    `json:"status"`
	HTTPStatus   int       `json:"http_status"`
	PromptTokens int       `json:"prompt_tokens"`
	OutputTokens int       `json:"output_tokens"`
	CreatedAt    time.Time `json:"created_at"`
	StartedAt    time.Time `json:"started_at,omitempty"`
	CompletedAt  time.Time `json:"completed_at,omitempty"`

	// Attempt — 1-based номер итоговой попытки (см. INV-5). 0 если ещё не запускалась.
	Attempt int `json:"attempt"`
	// MaxAttempts — конфиг retry.max_attempts, передаётся для отображения "2/3".
	MaxAttempts int `json:"max_attempts"`
	// QueueWaitMs — миллисекунды между created_at и started_at. 0 если ещё в очереди.
	QueueWaitMs int64 `json:"queue_wait_ms"`
	// ModelReloaded — сервер переключал current_model в начале этой задачи.
	// Колонка RM в TUI: ✓ при true, — при false.
	ModelReloaded bool `json:"model_reloaded,omitempty"`
	// LastFailedServer — имя сервера, на котором предыдущая попытка этого
	// запроса упала. Пустая строка означает «ни одной неудачной попытки не
	// было». TUI использует поле для отрисовки крестика в колонке Server.
	LastFailedServer string `json:"last_failed_server,omitempty"`
	// ErrorMessage — последняя ошибка (для failed-строк); пусто для completed/queued.
	ErrorMessage string `json:"error_message,omitempty"`
}

// StateSnapshotPayload — полный снимок состояния (отправляется при подключении и периодически).
type StateSnapshotPayload struct {
	Servers  []ServerState  `json:"servers"`
	Requests []RequestState `json:"requests"`
}
