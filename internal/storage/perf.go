package storage

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"proxylm/internal/core"
)

// perfEnqueueCap — ёмкость канала между PerfTracker.SetSink (вызывается на
// горячем пути scheduler'а через recordPerf) и единственной writer-горутиной
// PerfStore.Run. Наблюдения — best-effort persistence (FUTURE #1): при полном
// канале строка дропается, а не блокирует scheduler.
const perfEnqueueCap = 1024

// perfRow — одно наблюдение PerfTracker, ожидающее записи в perf_observations.
type perfRow struct {
	server   string
	model    string
	endpoint string
	obs      core.Observation
}

// PerfKey — ключ (server, model, endpoint), под которым PerfStore.LoadAll
// группирует восстановленные наблюдения. Зеркалит internal/core.perfKey,
// но экспортирован, т.к. storage не может использовать неэкспортированный
// тип другого пакета.
type PerfKey struct {
	Server   string
	Model    string
	Endpoint string
}

// PerfStore — асинхронный single-writer персистер наблюдений PerfTracker в
// таблицу perf_observations. Подключается как core.PerfSink через Enqueue;
// сама запись в БД происходит в отдельной горутине (Run), чтобы не блокировать
// scheduler синхронным I/O (см. CLAUDE.md: "не блокировать HTTP-handler/scheduler
// синхронным I/O").
type PerfStore struct {
	db  *DB
	log *slog.Logger
	ch  chan perfRow
}

// NewPerfStore создаёт PerfStore поверх уже открытой и мигрированной БД.
func NewPerfStore(db *DB, log *slog.Logger) *PerfStore {
	return &PerfStore{db: db, log: log, ch: make(chan perfRow, perfEnqueueCap)}
}

// Enqueue — core.PerfSink. Неблокирующая отправка в очередь writer'а; при
// заполненном канале (writer не успевает / БД недоступна) наблюдение
// дропается с Debug-логом — это ожидаемый best-effort trade-off, чтобы не
// тормозить scheduler. Nil-safe (nil *PerfStore игнорируется).
func (s *PerfStore) Enqueue(server, model, endpoint string, o core.Observation) {
	if s == nil {
		return
	}
	row := perfRow{server: server, model: model, endpoint: endpoint, obs: o}
	select {
	case s.ch <- row:
	default:
		if s.log != nil {
			s.log.Debug("perf observation dropped: writer queue full",
				"server", server, "model", model, "endpoint", endpoint)
		}
	}
}

// Run — единственная writer-горутина PerfStore. Читает из внутреннего канала
// до отмены ctx и пишет по одной строке за раз (объём мал — по наблюдению за
// запрос, батчинг не нужен для ясности кода). Каждая запись выполняется с
// собственным detached-контекстом (context.Background + 5s timeout), НЕ
// производным от ctx: это гарантирует, что уже начатая запись переживёт
// отмену ctx при штатном shutdown (тот же паттерн, что и persistRecord в
// internal/api/routes_openai.go). Ошибки вставки — Warn-логом, без остановки
// цикла (наблюдения не критичны для работы прокси).
func (s *PerfStore) Run(ctx context.Context) {
	if s == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case row := <-s.ch:
			// insert намеренно использует detached-контекст (см. doc-comment Run):
			// уже начатая запись должна пережить отмену ctx при shutdown.
			s.insert(row) //nolint:contextcheck
		}
	}
}

func (s *PerfStore) insert(row perfRow) {
	writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const q = `
        INSERT INTO perf_observations (
            server, model, endpoint, in_tokens, out_tokens, total_ms, loaded, created_at
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := s.db.ExecContext(writeCtx, q,
		row.server, row.model, row.endpoint,
		row.obs.In, row.obs.Out, row.obs.TotalMs, boolToInt(row.obs.Loaded),
		formatTime(time.Now().UTC()))
	if err != nil && s.log != nil {
		s.log.Warn("perf observation insert failed",
			"server", row.server, "model", row.model, "endpoint", row.endpoint,
			"error", err.Error())
	}
}

// LoadAll читает все сохранённые наблюдения, сгруппированные по (server,
// model, endpoint), в порядке id ASC (старые → новые — тот же порядок, в
// котором PerfTracker.Load ожидает точки). Вызывается один раз при старте
// daemon'а для восстановления PerfTracker после рестарта.
func (s *PerfStore) LoadAll(ctx context.Context) (map[PerfKey][]core.Observation, error) {
	const q = `
        SELECT server, model, endpoint, in_tokens, out_tokens, total_ms, loaded
        FROM perf_observations
        ORDER BY id ASC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("perf load all: %w", err)
	}
	defer rows.Close()

	out := make(map[PerfKey][]core.Observation)
	for rows.Next() {
		var (
			k        PerfKey
			in, outT int
			totalMs  int64
			loadedI  int
		)
		if err := rows.Scan(&k.Server, &k.Model, &k.Endpoint, &in, &outT, &totalMs, &loadedI); err != nil {
			return nil, fmt.Errorf("perf load all: scan: %w", err)
		}
		out[k] = append(out[k], core.Observation{In: in, Out: outT, TotalMs: totalMs, Loaded: loadedI != 0})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("perf load all: %w", err)
	}
	return out, nil
}

// Trim удаляет из perf_observations все наблюдения, кроме самых новых perKeyCap
// по каждому ключу (server, model, endpoint) — тот же ring-buffer cap, что
// PerfTracker держит в памяти (core.PerfMaxObservations). Вызывается один раз
// при старте (до LoadAll) и затем периодически из hourly-maintenance, чтобы
// таблица не росла безгранично на серверах, крутящихся месяцами. Возвращает
// количество удалённых строк.
func (s *PerfStore) Trim(ctx context.Context, perKeyCap int) (int64, error) {
	if perKeyCap <= 0 {
		return 0, nil
	}
	const q = `
        DELETE FROM perf_observations
        WHERE id IN (
            SELECT p.id FROM perf_observations p
            WHERE (
                SELECT COUNT(*) FROM perf_observations q
                WHERE q.server = p.server AND q.model = p.model AND q.endpoint = p.endpoint
                  AND q.id > p.id
            ) >= ?
        )`
	res, err := s.db.ExecContext(ctx, q, perKeyCap)
	if err != nil {
		return 0, fmt.Errorf("perf trim: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
