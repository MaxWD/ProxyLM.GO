package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"sync"
	"syscall"
	"time"
)

// Job — единица работы, ставимая в очередь сервера.
// Run вызывается worker'ом и должен выполнить апстрим-вызов на srv.
// Поле done — package-private; используется Scheduler.Submit для приёма результата.
type Job struct {
	ID    string
	Model string
	// Endpoint — путь OpenAI-API, по которому пришёл запрос
	// (/v1/chat/completions, /v1/completions, /v1/embeddings и т. п.).
	// Используется PerfTracker как часть ключа регрессии: разные endpoint'ы
	// имеют принципиально разные профили tokens/ms и не должны смешиваться
	// (FUTURE.md #3 → v0.10.0). Пустая строка допустима для совместимости со
	// старыми сценариями и тестами — она попадёт в «безымянный» бакет.
	Endpoint string

	// Run — пользовательский handler. Должен вернуть JobResult со статусом
	// и пометить ResponseStarted=true как только первый байт ушёл клиенту (INV-6).
	Run func(ctx context.Context, srv *ServerInfo) JobResult

	done chan JobResult // создаётся внутри Submit; capacity 1

	// visited — для стратегии deferred_model_then_capable. Заполняется Submit'ом
	// при failover'е: сервер, на котором Job уже падал, помечается, чтобы pool
	// не отдал ему этот же Job повторно. Доступ — только под локом JobPool.
	visited map[string]bool
}

// JobResult — итог одной попытки выполнения Job на одном сервере.
type JobResult struct {
	Err             error
	HTTPStatus      int
	ResponseStarted bool // true → ретрай/failover запрещены (INV-6)
	PromptTokens    int
	OutputTokens    int
	// Attempt — номер итоговой попытки (1-based), на которой завершился Submit.
	// При успехе с первой попытки = 1, при retry/failover увеличивается.
	Attempt int
	// Server — имя сервера, на котором был выполнен этот результат. Заполняется
	// dispatch'ем; в push-режиме совпадает с уже известным Submit'у сервером,
	// в deferred-режиме это единственный способ узнать, кто взял Job из пула.
	Server string
	// TtftMs — время «до первого байта клиенту» в миллисекундах, измеренное
	// внутри Run. Заполняется handler'ом (runStream при первом w.Write,
	// runNonStream при финальном w.Write). 0 означает «не измерено» — Scheduler
	// в этом случае подставит общий totalMs (для non-stream это и есть TTFT).
	TtftMs int64
	// ModelReloaded — true, если на старте dispatch'а CurrentModel сервера не
	// совпадал с j.Model и был переключён. Заполняется самим dispatch'ем (Run
	// его не видит и не должен трогать). Используется регрессией perf для
	// отделения «холодных» вызовов (с временем загрузки модели) от «горячих».
	ModelReloaded bool
}

// Scheduler — оркестратор: per-server worker'ы + интеграция с router/retry.
type Scheduler struct {
	router  *Router
	retry   RetryConfig
	servers []*ServerInfo
	log     *slog.Logger
	wg      sync.WaitGroup

	// pool активен только для pull-стратегий (deferred_model_then_capable и
	// preserve_model_coverage). В push-стратегиях остаётся nil, и Submit/worker
	// идут по классическому пути через router.Pick + srv.Queue.
	pool *JobPool
	// coverageAware включён для стратегии preserve_model_coverage; меняет
	// логику PopFor: не drain'им current_model вслепую, а только если этот
	// сервер — единственный её держатель.
	coverageAware bool
	// fairShare включён для стратегии fair_share_round_robin
	// (FUTURE.md #8 → v0.10.0). При maxConsecutivePerModel > 0 воркер
	// принудительно переключает модель после N подряд диспатчей одной.
	// 0 (или отрицательное) — лимит отключён, поведение совпадает с
	// deferred_model_then_capable.
	fairShare              bool
	maxConsecutivePerModel int

	// perf — скользящее окно tok/s и TTFT по парам (server, model).
	// nil-safe: если не передан, Record просто игнорируется.
	perf *PerfTracker
}

// ErrSchedulerStopped — попытка Submit после остановки планировщика.
var ErrSchedulerStopped = errors.New("scheduler: остановлен")

// SchedulerOptions — параметры конструктора Scheduler помимо обязательных
// (router/retry/servers/log). Используются для опций, добавленных позднее MVP
// (v0.10.0+) — чтобы не ломать сигнатуру каждым новым полем.
type SchedulerOptions struct {
	// MaxConsecutivePerModel — лимит подряд выполненных задач одной модели
	// на одном сервере перед принудительным переключением. Действует только
	// для стратегии fair_share_round_robin; для других стратегий
	// игнорируется. 0 или отрицательное — лимит отключён.
	MaxConsecutivePerModel int
}

// NewScheduler создаёт планировщик. Для pull-стратегий
// (deferred_model_then_capable, preserve_model_coverage, fair_share_round_robin)
// дополнительно инициализирует общий JobPool.
func NewScheduler(servers []*ServerInfo, router *Router, retry RetryConfig, log *slog.Logger) *Scheduler {
	return NewSchedulerWithOptions(servers, router, retry, log, SchedulerOptions{})
}

// NewSchedulerWithOptions — расширенная версия NewScheduler, принимающая
// дополнительные параметры (см. SchedulerOptions). Обычные вызовы могут
// использовать NewScheduler.
func NewSchedulerWithOptions(servers []*ServerInfo, router *Router, retry RetryConfig, log *slog.Logger, opts SchedulerOptions) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	s := &Scheduler{
		router:  router,
		retry:   retry,
		servers: servers,
		log:     log,
	}
	if router != nil {
		switch router.strategy {
		case RouterDeferredModelThenCapable:
			s.pool = NewJobPool()
		case RouterPreserveModelCoverage:
			s.pool = NewJobPool()
			s.coverageAware = true
		case RouterFairShareRoundRobin:
			s.pool = NewJobPool()
			s.fairShare = true
			s.maxConsecutivePerModel = opts.MaxConsecutivePerModel
		}
	}
	return s
}

// SetPerfTracker подключает скользящее окно метрик; Scheduler начнёт
// записывать данные о каждом успешном Run. Вызвать до Start; thread-safe не
// требуется (Start ещё не дал worker'ам ссылку на perf).
func (s *Scheduler) SetPerfTracker(p *PerfTracker) { s.perf = p }

// Start запускает по одному worker'у на каждый зарегистрированный сервер.
func (s *Scheduler) Start(ctx context.Context) {
	for _, srv := range s.servers {
		s.wg.Add(1)
		go s.worker(ctx, srv)
	}
}

// Wait блокирует до завершения всех worker'ов.
func (s *Scheduler) Wait() { s.wg.Wait() }

// Submit оркеструет выполнение Job: выбирает сервер через router, ставит в очередь,
// ждёт результата. При ошибке — следующая попытка стремится уйти на другой сервер
// (rolling exclusion size 1: только последний упавший исключён); общее число
// попыток ≤ retry.MaxAttempts.
//
// INV-5 (новая редакция): попыток ≤ MaxAttempts всего, и две подряд попытки не
// идут на один сервер, пока есть альтернатива.
// INV-6: после ResponseStarted=true — ни retry, ни failover не выполняются.
//
// Для pull-стратегий (deferred_model_then_capable, preserve_model_coverage)
// работа делегируется в submitDeferred: Job уходит в общий JobPool, а воркеры
// сами тянут совместимое (см. pool.go).
func (s *Scheduler) Submit(ctx context.Context, j *Job) (JobResult, error) {
	if j == nil || j.Run == nil {
		return JobResult{}, fmt.Errorf("scheduler: пустой Job/Run")
	}
	if s.pool != nil {
		return s.submitDeferred(ctx, j)
	}
	return s.submitPush(ctx, j)
}

// submitPush — классический режим: router.Pick → srv.Queue → ждём j.done.
//
// Новая модель retry: одна попытка на сервер, exclude={lastFailed}. На каждом
// шаге total++; цикл прерывается на total >= MaxAttempts. Если router.Pick
// c exclude вернул ErrNoServer (единственный совместимый сервер — это и есть
// lastFailed), очищаем exclude и продолжаем на том же узле — лучше
// деградировать, чем сдаваться раньше MaxAttempts.
func (s *Scheduler) submitPush(ctx context.Context, j *Job) (JobResult, error) {
	j.done = make(chan JobResult, 1)
	var (
		lastResult JobResult
		lastErr    error
		lastFailed string
		total      int
	)

	for {
		exclude := map[string]bool{}
		if lastFailed != "" {
			exclude[lastFailed] = true
		}
		srv, err := s.router.Pick(j.Model, exclude)
		if err != nil {
			// Единственный совместимый — упавший. Деградируем и ретраим на нём.
			if lastFailed != "" {
				srv, err = s.router.Pick(j.Model, nil)
			}
			if err != nil {
				if lastErr != nil {
					return lastResult, lastErr
				}
				return JobResult{}, err
			}
		}

		total++
		if total > 1 {
			if d := s.retry.Backoff(total); d > 0 {
				select {
				case <-ctx.Done():
					lastResult.Attempt = total
					return lastResult, ctx.Err()
				case <-time.After(d):
				}
			}
		}

		res, runErr := s.runOnServer(ctx, srv, j)
		res.Attempt = total
		res.Server = srv.Name
		lastResult, lastErr = res, runErr
		if runErr == nil {
			return res, nil
		}
		if res.ResponseStarted {
			return res, runErr
		}
		if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			return res, runErr
		}
		s.log.Warn("попытка не удалась",
			"request_id", j.ID,
			"server", srv.Name,
			"model", j.Model,
			"attempt", total,
			"error", runErr.Error())
		if total >= s.retry.MaxAttempts {
			return lastResult, lastErr
		}
		lastFailed = srv.Name
	}
}

// submitDeferred — режим late-binding для pull-стратегий
// (deferred_model_then_capable, preserve_model_coverage).
//
// Job кладётся в общий JobPool; конкретный сервер не выбирается заранее.
// Освобождающийся worker сам берёт первый совместимый Job из пула.
//
// Wake-стратегия (Q1 — приоритет на холодном старте):
//   - Если есть свободный (!InFlight) healthy сервер с j.Model в Models —
//     будим только самого приоритетного из них.
//   - Если все compatible серверы заняты — пуш в pool без Wake: освободившийся
//     сам пойдёт в pool через свой внутренний for-loop после dispatch'а.
//
// Retry-модель (v1.0+): одна попытка = одно владение Job воркером (dispatchOnce
// делает ровно один Run). После неуспеха submitDeferred делает rolling
// exclusion: MarkVisited(j, lastFailedServer) — упавший сервер не возьмёт
// этот же Job на СЛЕДУЮЩИЙ попытке, но он же может взять через одну. Общий
// cap — retry.MaxAttempts; backoff применяется между попытками. Если
// rolling-exclusion оставил Job без кандидатов (упавший — единственный
// healthy совместимый), ClearVisited и продолжаем на нём же.
func (s *Scheduler) submitDeferred(ctx context.Context, j *Job) (JobResult, error) {
	j.done = make(chan JobResult, 1)

	// Q3: синхронная проверка совместимости. Если ни один healthy сервер не
	// поддерживает модель — отказ сразу, в pool не кладём.
	if !anyHealthyCompatible(s.servers, j.Model, nil) {
		return JobResult{}, &ErrNoServer{Model: j.Model}
	}

	var lastResult JobResult
	total := 0

	for {
		// Между попытками — backoff. На первой total ещё 0, паузы нет.
		if total >= 1 {
			if d := s.retry.Backoff(total + 1); d > 0 {
				select {
				case <-ctx.Done():
					return lastResult, ctx.Err()
				case <-time.After(d):
				}
			}
		}
		// done пересоздаём на каждую новую попытку: предыдущая уже прочитана.
		if total > 0 {
			j.done = make(chan JobResult, 1)
		}
		s.pool.Push(j)
		if best := pickIdleByPriority(s.servers, j.Model, j.visited); best != nil {
			best.Wake()
		}

		select {
		case res := <-j.done:
			total++
			res.Attempt = total
			lastResult = res

			if res.Err == nil {
				return res, nil
			}
			if res.ResponseStarted {
				return res, res.Err
			}
			if errors.Is(res.Err, context.Canceled) || errors.Is(res.Err, context.DeadlineExceeded) {
				return res, res.Err
			}
			if total >= s.retry.MaxAttempts {
				return res, res.Err
			}
			// Rolling exclusion: исключаем только этот сервер на следующую попытку.
			if res.Server != "" {
				s.pool.MarkVisited(j, res.Server)
				s.log.Info("следующая попытка пойдёт на другой сервер",
					"request_id", j.ID,
					"failed_server", res.Server,
					"model", j.Model,
					"attempts_done", total)
			}
			// Если других совместимых нет — снимаем exclusion, чтобы не зависнуть
			// в pool навсегда. Деградируем: ретраим на том же.
			if !anyHealthyCompatible(s.servers, j.Model, j.visited) {
				s.pool.ClearVisited(j)
			}
			// loop: положим Job обратно в pool после backoff'а.

		case <-ctx.Done():
			// Если Job ещё не схватили — вынимаем из pool, фиксируем cancel.
			if s.pool.Remove(j.ID) {
				return JobResult{Attempt: total + 1}, ctx.Err()
			}
			// Уже взят воркером — ждём финального результата, чтобы не уйти
			// раньше Run() (иначе worker запишет в брошенный канал).
			res := <-j.done
			return res, ctx.Err()
		}
	}
}

// runOnServer ставит Job в очередь сервера, будит worker и ждёт результат.
func (s *Scheduler) runOnServer(ctx context.Context, srv *ServerInfo, j *Job) (JobResult, error) {
	srv.Lock()
	srv.Queue = append(srv.Queue, j)
	srv.Unlock()
	srv.Wake()

	select {
	case res := <-j.done:
		return res, res.Err
	case <-ctx.Done():
		if s.cancelQueued(srv, j.ID) {
			return JobResult{Err: ctx.Err()}, ctx.Err()
		}
		// Worker уже взял Job — дождёмся завершения, чтобы не уйти раньше Run().
		res := <-j.done
		return res, res.Err
	}
}

func (s *Scheduler) cancelQueued(srv *ServerInfo, id string) bool {
	srv.Lock()
	defer srv.Unlock()
	for i, item := range srv.Queue {
		if item != nil && item.ID == id {
			srv.Queue = append(srv.Queue[:i], srv.Queue[i+1:]...)
			return true
		}
	}
	return false
}

// worker — единственный потребитель очереди одного сервера.
// Реализует INV-1 (≤1 in-flight) и INV-2/3 (drain current model FIFO перед switch).
//
// В push-режиме источник работы — srv.Queue (s.pickNext).
// В pull-режиме (deferred_model_then_capable) — общий s.pool (PopFor).
func (s *Scheduler) worker(ctx context.Context, srv *ServerInfo) {
	defer s.wg.Done()
	for {
		select {
		case <-ctx.Done():
			s.failPending(srv, ctx.Err())
			return
		case <-srv.Notify:
		}
		for {
			// M8: проверяем ctx перед каждой итерацией inner-loop, чтобы при
			// shutdown не зависать в dispatch'е ещё одного Job.
			if ctx.Err() != nil {
				s.failPending(srv, ctx.Err())
				return
			}
			var j *Job
			switch {
			case s.pool != nil && s.coverageAware:
				j = s.pool.PopForCoverage(srv, s.servers)
			case s.pool != nil && s.fairShare:
				j = s.pool.PopForFairShare(srv, s.maxConsecutivePerModel)
			case s.pool != nil:
				j = s.pool.PopFor(srv)
			default:
				j = s.pickNext(srv)
			}
			if j == nil {
				break
			}
			s.dispatch(ctx, srv, j)
		}
	}
}

// runWithPanicGuard оборачивает j.Run в recover, чтобы panic не убила worker'а.
// Возвращает JobResult с Err при panic (см. также TestScheduler_RunPanicDoesNotDeadlock).
func (s *Scheduler) runWithPanicGuard(ctx context.Context, srv *ServerInfo, j *Job) (res JobResult) {
	defer func() {
		if rv := recover(); rv != nil {
			s.log.Error("panic в Job.Run",
				"server", srv.Name,
				"request_id", j.ID,
				"value", fmt.Sprint(rv))
			res = JobResult{Err: fmt.Errorf("panic в Job.Run: %v", rv)}
		}
	}()
	return j.Run(ctx, srv)
}

// pickNext извлекает следующий элемент очереди в соответствии с INV-2/3:
//  1. Если CurrentModel задан — ищем первый Job с тем же model (FIFO).
//  2. Если для CurrentModel ничего не осталось — берём первый Job из очереди
//     и переключаем CurrentModel (это разрешено: INV-2 запрещает переключение
//     только пока для текущей модели есть pending'и).
//  3. Пустая очередь → nil.
//
// Реактивный crash-detect (баг #78, v0.9.0): unhealthy сервер не получает
// следующего Job. failServerQueue к этому моменту уже дренировал srv.Queue,
// но между failServerQueue и worker'ным break Submit мог успеть добавить
// Job напрямую в srv.Queue — здесь страхуем.
func (s *Scheduler) pickNext(srv *ServerInfo) *Job {
	if !srv.Healthy.Load() {
		return nil
	}
	srv.Lock()
	defer srv.Unlock()
	if len(srv.Queue) == 0 {
		return nil
	}
	current := srv.CurrentModelString()
	if current != "" {
		for i, j := range srv.Queue {
			if j.Model == current {
				srv.Queue = append(srv.Queue[:i], srv.Queue[i+1:]...)
				return j
			}
		}
	}
	first := srv.Queue[0]
	srv.Queue = srv.Queue[1:]
	srv.SetCurrentModel(first.Model)
	return first
}

// dispatch запускает Run под флагом InFlight (INV-1) — РОВНО одна попытка.
// При panic в Run — превращает её в JobResult с ошибкой, чтобы Submit не
// зависал в ожидании j.done. Используется и в push, и в pull режимах: retry
// между попытками целиком оркестрируется в Submit*.
//
// В pull-режиме pool может отдать Job под модель, отличную от CurrentModel —
// в push-режиме pickNext уже выставил current. SetCurrentModel идемпотентен,
// поэтому делаем его всегда и не вводим отдельную ветку.
//
// Реактивный crash-detect: если Run вернул connection-error (refused / no route /
// dial timeout), сервер немедленно помечается unhealthy — не ждём
// discovery.unhealthy_after_failed_polls. Discovery восстановит healthy при
// следующем удачном polll'е.
func (s *Scheduler) dispatch(ctx context.Context, srv *ServerInfo, j *Job) {
	reloaded := srv.CurrentModelString() != j.Model
	if reloaded {
		srv.SetCurrentModel(j.Model)
	}
	// Учёт серии подряд диспатчей одной модели (FUTURE.md #8 → v0.10.0).
	// Обновляем безусловно — overhead минимальный (один lock + int++); даже
	// если стратегия не fair_share, поле остаётся как honest «последний
	// диспатченный» для возможной диагностики в будущем.
	srv.Lock()
	if srv.LastDispatchedModel == j.Model {
		srv.ConsecutiveModelCount++
	} else {
		srv.LastDispatchedModel = j.Model
		srv.ConsecutiveModelCount = 1
	}
	srv.Unlock()
	if !srv.InFlight.CompareAndSwap(false, true) {
		s.log.Error("INV-1 нарушен: InFlight=true перед dispatch",
			"server", srv.Name, "request_id", j.ID)
	}
	defer srv.InFlight.Store(false)

	runStart := time.Now()
	res := s.runWithPanicGuard(ctx, srv, j)
	res.Server = srv.Name
	res.ModelReloaded = reloaded
	if res.Err != nil && !res.ResponseStarted {
		srv.FailureCount.Add(1)
		if isConnectionError(res.Err) {
			if srv.Healthy.CompareAndSwap(true, false) {
				s.log.Warn("сервер помечен unhealthy по connection-error при dispatch",
					"server", srv.Name,
					"request_id", j.ID,
					"error", res.Err.Error())
				// Сервер только что упал. Очередь push-режима (срез srv.Queue)
				// продолжать обрабатывать нельзя — следующие задачи получат тот
				// же connection-refused. Отправляем всем висящим в Queue ошибку
				// «server unhealthy»; их Submit'ы сделают retry/failover на
				// другом сервере. В pull-режиме pool остаётся общим — там worker
				// сам перестанет тянуть Job из-за Healthy-check в worker-loop.
				s.failServerQueue(srv, fmt.Errorf("server %q стал unhealthy", srv.Name))
			}
		}
	}
	if res.Err == nil {
		s.recordPerf(srv.Name, j.Model, j.Endpoint, &res, runStart, reloaded)
	}
	j.done <- res
}

// failServerQueue атомарно очищает srv.Queue и отправляет всем висящим Job
// ошибку — чтобы Submit (push-режим) ушёл на retry/failover, а не ждал
// dispatch'а от только что упавшего сервера. Безопасно при пустой очереди.
func (s *Scheduler) failServerQueue(srv *ServerInfo, cause error) {
	srv.Lock()
	queue := srv.Queue
	srv.Queue = nil
	srv.Unlock()
	for _, j := range queue {
		// j.done — buffered(1), Submit-сторона уже читает — не блокируем.
		select {
		case j.done <- JobResult{Err: cause, Server: srv.Name}:
		default:
		}
	}
}

// recordPerf — приватный хелпер, заполняющий TtftMs (если не выставлен Run'ом)
// и пишущий точку в PerfTracker. Никогда не паникует и nil-safe.
//
// loaded=true означает, что эта попытка инициировала смену current_model на
// сервере. PerfTracker строит линейную регрессию по формуле
// loaded·t_load + b·k_in + c·k_out = t_all (см. perf.go).
//
// Slow-detect: до Record вычисляет predicted = perf.Predict(...) на основе
// УЖЕ накопленных данных. Если predicted > 0 и actual ≥ 2×predicted —
// srv.LastSlow ← true (флаг публикуется в IPC и отображается в TUI).
// При actual < 2×predicted флаг сбрасывается. Текущая точка добавляется в
// статистику ПОСЛЕ сравнения, чтобы slow-выброс не «отравил» predicted.
func (s *Scheduler) recordPerf(server, model, endpoint string, res *JobResult, runStart time.Time, loaded bool) {
	if s.perf == nil {
		return
	}
	totalMs := time.Since(runStart).Milliseconds()
	if res.TtftMs == 0 {
		// non-stream: первый байт == последний (целиком ответ записан одной операцией).
		res.TtftMs = totalMs
	}
	predicted := s.perf.Predict(server, model, endpoint, res.PromptTokens, res.OutputTokens, loaded)
	s.updateSlowFlag(server, totalMs, predicted)
	s.perf.Record(server, model, endpoint, res.PromptTokens, res.OutputTokens, totalMs, loaded)
}

// updateSlowFlag устанавливает / сбрасывает ServerInfo.LastSlow по сравнению
// фактического времени с прогнозом. predicted=0 (мало данных) — флаг не трогаем,
// чтобы не сбрасывать и не выставлять без основания.
func (s *Scheduler) updateSlowFlag(server string, actualMs int64, predictedMs float64) {
	if predictedMs <= 0 {
		return
	}
	srv := s.findServer(server)
	if srv == nil {
		return
	}
	if float64(actualMs) >= 2.0*predictedMs {
		if srv.LastSlow.CompareAndSwap(false, true) {
			s.log.Warn("сервер помечен SLOW",
				"server", server,
				"actual_ms", actualMs,
				"predicted_ms", int64(predictedMs))
		}
	} else {
		if srv.LastSlow.CompareAndSwap(true, false) {
			s.log.Info("SLOW-флаг сброшен", "server", server)
		}
	}
}

func (s *Scheduler) findServer(name string) *ServerInfo {
	for _, srv := range s.servers {
		if srv.Name == name {
			return srv
		}
	}
	return nil
}

// isConnectionError возвращает true для ошибок установления / поддержания
// TCP-соединения: ECONNREFUSED, dial timeout, EHOSTUNREACH, no route. HTTP 5xx
// от живого сервера сюда НЕ попадают — те идут как обычные транспортные ошибки
// и unhealthy не выставляют.
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	// url.Error.Op == "Get"/"Post" — внутри Err. Распаковываем.
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		if urlErr.Timeout() {
			return true
		}
		err = urlErr.Err
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		// dial / read / write на не-доступный peer.
		if opErr.Op == "dial" || opErr.Op == "read" || opErr.Op == "write" {
			return true
		}
	}
	var sysErr *os.SyscallError
	if errors.As(err, &sysErr) {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) {
		return true
	}
	return false
}

// failPending завершает все висящие в очередях Job с указанной ошибкой —
// вызывается при остановке worker'а.
//
// В push-режиме отказывает свой srv.Queue. В pull-режиме общий pool разделяется
// между worker'ами; при shutdown'е достаточно, чтобы один worker (любой)
// «слил» pool в ошибки. Чтобы не было гонки за то, кто это сделает, делаем
// «опустошить и отказать» атомарно через pool.drainAll.
func (s *Scheduler) failPending(srv *ServerInfo, err error) {
	srv.Lock()
	queue := srv.Queue
	srv.Queue = nil
	srv.Unlock()
	for _, j := range queue {
		// j.done буферизован (1); guard защищает от отправки в уже прочитанный канал
		// (симметрично с failServerQueue — H6).
		select {
		case j.done <- JobResult{Err: fmt.Errorf("scheduler stopped: %w", err)}:
		default:
		}
	}
	if s.pool != nil {
		for _, j := range s.pool.drainAll() {
			// Submit может ещё держать сторону приёма; буфер j.done=1 не даст
			// блокироваться, даже если Submit уже отменился по ctx.
			select {
			case j.done <- JobResult{Err: fmt.Errorf("scheduler stopped: %w", err)}:
			default:
			}
		}
	}
}
