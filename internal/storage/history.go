package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"proxylm/internal/core"
)

// History — операции над таблицей requests.
type History struct {
	db *DB
}

// NewHistory возвращает обёртку над DB для работы с историей запросов.
func NewHistory(db *DB) *History { return &History{db: db} }

// Insert вставляет новую запись (статус обычно pending или running).
// Существующая запись с тем же id будет заменена (UPSERT) — это удобно при ретраях
// и failover, когда меняется server_name/attempt/status.
func (h *History) Insert(ctx context.Context, r *core.RequestRecord) error {
	const q = `
        INSERT INTO requests (
            id, client_name, model, endpoint, stream, server_name, status,
            http_status, prompt_tokens, output_tokens, attempt, error_message,
            model_reloaded, last_failed_server, created_at, started_at, completed_at
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(id) DO UPDATE SET
            client_name        = excluded.client_name,
            model              = excluded.model,
            endpoint           = excluded.endpoint,
            stream             = excluded.stream,
            server_name        = excluded.server_name,
            status             = excluded.status,
            http_status        = excluded.http_status,
            prompt_tokens      = excluded.prompt_tokens,
            output_tokens      = excluded.output_tokens,
            attempt            = excluded.attempt,
            error_message      = excluded.error_message,
            model_reloaded     = excluded.model_reloaded,
            last_failed_server = excluded.last_failed_server,
            started_at         = excluded.started_at,
            completed_at       = excluded.completed_at`
	_, err := h.db.ExecContext(ctx, q,
		r.ID, r.ClientName, r.Model, r.Endpoint, boolToInt(r.Stream),
		r.ServerName, string(r.Status), r.HTTPStatus,
		r.PromptTokens, r.OutputTokens, r.Attempt, r.ErrorMessage,
		boolToInt(r.ModelReloaded), r.LastFailedServer,
		formatTime(r.CreatedAt), formatTime(r.StartedAt), formatTime(r.CompletedAt),
	)
	if err != nil {
		return fmt.Errorf("history insert %s: %w", r.ID, err)
	}
	return nil
}

// List возвращает последние N записей (упорядоченные по created_at DESC).
// limit <= 0 → 100. Используется TUI и тестами.
func (h *History) List(ctx context.Context, limit int) ([]*core.RequestRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	const q = `
        SELECT id, client_name, model, endpoint, stream, server_name, status,
               http_status, prompt_tokens, output_tokens, attempt, error_message,
               model_reloaded, last_failed_server, created_at, started_at, completed_at
        FROM requests
        ORDER BY created_at DESC
        LIMIT ?`
	rows, err := h.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("history list: %w", err)
	}
	defer rows.Close()
	out := make([]*core.RequestRecord, 0, limit)
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CancelStale помечает все «зависшие» записи (pending / running / устаревший in_progress)
// как cancelled — вызывается при старте daemon'а. Иначе записи, которые в момент
// предыдущей остановки были в очереди или выполнялись, навсегда остаются в pending/running
// и TUI показывает их как «вечно queued». error_message выставляется в "daemon was restarted".
// Возвращает количество обновлённых строк.
func (h *History) CancelStale(ctx context.Context) (int64, error) {
	const q = `
        UPDATE requests
        SET status        = 'cancelled',
            error_message = 'daemon was restarted',
            completed_at  = ?
        WHERE status IN ('pending', 'running', 'in_progress')`
	res, err := h.db.ExecContext(ctx, q, formatTime(time.Now().UTC()))
	if err != nil {
		return 0, fmt.Errorf("history cancel stale: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// DeleteOlderThan удаляет записи старше cutoff. Возвращает количество удалённых строк.
// Вызывается периодически согласно storage.history_retention_days.
func (h *History) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := h.db.ExecContext(ctx,
		"DELETE FROM requests WHERE created_at < ?",
		formatTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("history delete: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func scanRecord(rows *sql.Rows) (*core.RequestRecord, error) {
	var (
		r                                 core.RequestRecord
		streamI, reloadedI                int
		status                            string
		createdAt, startedAt, completedAt string
	)
	if err := rows.Scan(
		&r.ID, &r.ClientName, &r.Model, &r.Endpoint, &streamI, &r.ServerName, &status,
		&r.HTTPStatus, &r.PromptTokens, &r.OutputTokens, &r.Attempt, &r.ErrorMessage,
		&reloadedI, &r.LastFailedServer, &createdAt, &startedAt, &completedAt,
	); err != nil {
		return nil, fmt.Errorf("scan request: %w", err)
	}
	r.Stream = streamI != 0
	r.ModelReloaded = reloadedI != 0
	r.Status = core.Status(status)
	r.CreatedAt = parseTime(createdAt)
	r.StartedAt = parseTime(startedAt)
	r.CompletedAt = parseTime(completedAt)
	return &r, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
