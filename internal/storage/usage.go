package storage

import (
	"context"
	"fmt"
	"time"
)

// UsageRow — агрегат использования одной связки (клиент, модель, сервер).
type UsageRow struct {
	Client, Model, Server string
	Requests, Failed      int64
	// BusyMs — суммарное время выполнения на бэкендах (мс).
	BusyMs int64
	// QueueMsAvg — среднее ожидание в очереди (мс) по строкам, где оно измерено.
	QueueMsAvg   int64
	PromptTokens int64
	OutputTokens int64
	LastUsed     time.Time
}

// UsageStats агрегирует терминальные запросы (completed / failed / cancelled)
// по (client_name, model, server_name). since.IsZero() — без фильтра по времени
// (ограничено retention); иначе берутся записи с created_at >= since.
//
// created_at хранится как RFC3339Nano TEXT, где хвостовые нули дробной части
// обрезаны, поэтому чистое строковое сравнение не строго хронологично. Фильтр
// двухступенчатый: грубая нижняя граница по строке (секундная точность,
// использует индекс; заведомо не отсекает подходящие строки) + точное
// julianday-сравнение.
//
// BusyMs для строк до v0.15 (run_ms IS NULL) считается как completed_at -
// started_at; отрицательные и невычислимые значения игнорируются.
func (h *History) UsageStats(ctx context.Context, since time.Time) ([]UsageRow, error) {
	q := `
        SELECT COALESCE(client_name, ''), model, COALESCE(server_name, ''),
               COUNT(*),
               COALESCE(SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END), 0),
               COALESCE(SUM(MAX(0, COALESCE(run_ms,
                   CAST((julianday(completed_at) - julianday(started_at)) * 86400000 AS INTEGER), 0))), 0),
               COALESCE(CAST(AVG(queue_ms) AS INTEGER), 0),
               COALESCE(SUM(prompt_tokens), 0),
               COALESCE(SUM(output_tokens), 0),
               MAX(julianday(created_at)),
               created_at
        FROM requests
        WHERE status IN ('completed', 'failed', 'cancelled')`
	var args []any
	if !since.IsZero() {
		coarse := since.UTC().Truncate(time.Second).Format("2006-01-02T15:04:05")
		q += `
          AND created_at >= ?
          AND julianday(created_at) >= julianday(?)`
		args = append(args, coarse, formatTime(since))
	}
	q += `
        GROUP BY client_name, model, server_name
        ORDER BY COUNT(*) DESC, client_name, model, server_name`

	rows, err := h.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("history usage stats: %w", err)
	}
	defer rows.Close()
	out := make([]UsageRow, 0, 16)
	for rows.Next() {
		var (
			r        UsageRow
			lastJD   *float64
			lastText string
		)
		if err := rows.Scan(&r.Client, &r.Model, &r.Server, &r.Requests, &r.Failed,
			&r.BusyMs, &r.QueueMsAvg, &r.PromptTokens, &r.OutputTokens, &lastJD, &lastText); err != nil {
			return nil, fmt.Errorf("scan usage row: %w", err)
		}
		r.LastUsed = parseTime(lastText)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history usage stats: %w", err)
	}
	return out, nil
}
