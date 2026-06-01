package core

import (
	"sync"
	"sync/atomic"
	"time"
)

// Status — статус запроса в пайплайне планировщика.
type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// RequestRecord — единая запись о запросе, разделяемая планировщиком, БД и IPC.
// Поля заполняются по мере прохождения пайплайна; отсутствующие значения — zero value.
type RequestRecord struct {
	ID           string // UUID
	ClientName   string // имя клиента из auth.api_keys[].name
	Model        string // имя модели из тела запроса
	Endpoint     string // /v1/chat/completions | /v1/completions | /v1/embeddings | ...
	Stream       bool   // SSE-режим
	ServerName   string // имя выбранного backend'а (может меняться при failover)
	Status       Status
	HTTPStatus   int    // итоговый HTTP-статус, отданный клиенту (0 если не отдан)
	PromptTokens int    // usage.prompt_tokens (если известно)
	OutputTokens int    // usage.completion_tokens
	Attempt      int    // 0-based номер попытки на текущем сервере
	ErrorMessage string // последняя ошибка (для failed / completed-с-предупреждением)
	// ModelReloaded — true, если для финальной попытки серверу пришлось
	// переключить current_model. Источник — JobResult.ModelReloaded; копируется
	// после Submit. Влияет на регрессию perf и UI-колонку RM.
	ModelReloaded bool
	// LastFailedServer — имя сервера, на котором предыдущая попытка этого
	// запроса упала. Пустая строка — неудачных попыток ещё не было. Поле
	// заполняется api/routes_openai.go в фазе "pending-retry" Run-замыкания.
	LastFailedServer string
	CreatedAt        time.Time
	StartedAt        time.Time // момент захвата воркером (выход из очереди)
	CompletedAt      time.Time
}

// ModelInfo — описание модели на конкретном бэкенде (источник: discovery или явный список в конфиге).
type ModelInfo struct {
	Name string
}

// ServerInfo — runtime-состояние одного backend-сервера.
//
// Конкуренция:
//   - CurrentModel читается роутером без блокировок через atomic.Pointer[string]
//     (eventual consistency допустима — INV-2 проверяется уже под mu).
//   - Queue — slice под Mutex. Index scan нужен в drain-current-model: воркер
//     выбирает следующий запрос с моделью == CurrentModel, иначе ждёт пока
//     очередь данной модели опустеет и переключает модель.
//   - Notify — chan struct{} ёмкости 1: producer (router) шлёт пробуждение,
//     consumer (worker) спит на <-Notify. Capacity 1 + select+default
//     гарантирует «edge-triggered» без потерь и без блокировки producer.
//   - Healthy / InFlight — атомики, читаются роутером без mu.
type ServerInfo struct {
	Name     string
	URL      string
	Priority int
	Type     string // "openai" | "ollama"
	Timeout  time.Duration
	APIKey   string // не логировать

	CurrentModel atomic.Pointer[string] // nil => модель не загружена / неизвестна
	Healthy      atomic.Bool
	InFlight     atomic.Bool // INV-1: ≤1 in-flight; true пока воркер обрабатывает запрос
	// LastSlow — сервер в «медленном» режиме: последний завершённый запрос
	// взял ≥2× от рассчётного predicted (PerfTracker.Predict). Сбрасывается
	// при следующем «нормальном» запросе. Публикуется как ServerState.Slow.
	LastSlow atomic.Bool
	// FailureCount — счётчик попыток, упавших на этом сервере с ошибкой
	// (Err != nil && !ResponseStarted). Инкрементируется в Scheduler.dispatch
	// и публикуется как ServerState.FailureCount. Включает повторные попытки
	// одного и того же запроса (retry+failover) — каждая «отпавшая» попытка
	// учитывается отдельно. Не сбрасывается за время жизни процесса.
	FailureCount atomic.Int64

	Models []ModelInfo // последний снимок discovery; защищён mu
	Queue  []*Job      // FIFO задания; защищён mu; обрабатывается одним worker'ом
	mu     sync.Mutex
	Notify chan struct{} // capacity 1

	// LastDispatchedModel / ConsecutiveModelCount — учёт серии подряд
	// выполненных задач одной модели (введено в v0.10.0). Используется
	// стратегией fair_share_round_robin: при достижении лимита воркер
	// принудительно берёт job под ДРУГУЮ модель. Поля защищены mu;
	// обновляются в Scheduler.dispatch перед стартом Run.
	LastDispatchedModel   string
	ConsecutiveModelCount int

	// loadedModels — снимок реально загруженных в память моделей бэкенда,
	// обновляется discovery через нативную пробу (см. backends.LoadedModelsProber).
	// nil-снимок ⇒ проба ещё не выполнялась; probed=false ⇒ тип бэкенда пробу не
	// поддерживает. Читается без блокировок через atomic.Pointer. Введено в v0.13.0.
	loadedModels atomic.Pointer[loadedModelsState]
}

// loadedModelsState — неизменяемый снимок результата пробы загруженных моделей.
type loadedModelsState struct {
	models []string
	probed bool
}

// NewServerInfo создаёт ServerInfo с инициализированными atomic-полями и каналом Notify.
func NewServerInfo(name, url string, priority int, typ string, timeout time.Duration, apiKey string) *ServerInfo {
	s := &ServerInfo{
		Name:     name,
		URL:      url,
		Priority: priority,
		Type:     typ,
		Timeout:  timeout,
		APIKey:   apiKey,
		Notify:   make(chan struct{}, 1),
	}
	s.Healthy.Store(false)
	return s
}

// Lock / Unlock — для координированного доступа к Queue и Models из планировщика.
func (s *ServerInfo) Lock()   { s.mu.Lock() }
func (s *ServerInfo) Unlock() { s.mu.Unlock() }

// Wake шлёт неблокирующий сигнал воркеру. Безопасно при capacity-1 канале.
func (s *ServerInfo) Wake() {
	select {
	case s.Notify <- struct{}{}:
	default:
	}
}

// CurrentModelString возвращает имя текущей модели или пустую строку, если не задана.
func (s *ServerInfo) CurrentModelString() string {
	p := s.CurrentModel.Load()
	if p == nil {
		return ""
	}
	return *p
}

// SetCurrentModel атомарно обновляет CurrentModel. Передача "" сбрасывает в nil.
func (s *ServerInfo) SetCurrentModel(model string) {
	if model == "" {
		s.CurrentModel.Store(nil)
		return
	}
	m := model
	s.CurrentModel.Store(&m)
}

// SetLoadedModels атомарно сохраняет снимок реально загруженных моделей.
// probed=true помечает, что бэкенд поддерживает пробу (даже если models пуст).
func (s *ServerInfo) SetLoadedModels(models []string, probed bool) {
	st := &loadedModelsState{models: append([]string(nil), models...), probed: probed}
	s.loadedModels.Store(st)
}

// LoadedModelsSnapshot возвращает последний снимок загруженных моделей и флаг,
// поддерживается ли проба этим бэкендом. (nil, false) — пробы ещё не было или
// тип бэкенда её не поддерживает.
func (s *ServerInfo) LoadedModelsSnapshot() (models []string, probed bool) {
	st := s.loadedModels.Load()
	if st == nil {
		return nil, false
	}
	return st.models, st.probed
}
