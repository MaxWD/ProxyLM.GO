package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"proxylm/internal/config"
	"proxylm/internal/core"
	"proxylm/internal/core/backends"
	"proxylm/internal/storage"
)

// proxyHandler — общий handler для /v1/* POST-эндпоинтов
// (chat/completions, completions, embeddings).
//
// Поведение:
//  1. Читает тело, парсит JSON, извлекает model и stream.
//  2. Создаёт RequestRecord и пишет в БД (pending).
//  3. Создаёт core.Job с Run, выполняющим апстрим-вызов и пишущим ответ клиенту.
//  4. scheduler.Submit оркеструет очередь/retry/failover; ServeHTTP блокирует до завершения.
//
// stream=true в этой версии возвращает 501 (этап 21 добавит SSE).
type proxyHandler struct {
	endpoint string // относительный путь, например "/v1/chat/completions"
	sched    *core.Scheduler
	backends map[string]backends.Backend
	history  *storage.History
	compat   config.Compat
	log      *slog.Logger
}

func newProxyHandler(endpoint string, sched *core.Scheduler, bks map[string]backends.Backend, history *storage.History, compat config.Compat, log *slog.Logger) *proxyHandler {
	return &proxyHandler{
		endpoint: endpoint,
		sched:    sched,
		backends: bks,
		history:  history,
		compat:   compat,
		log:      log,
	}
}

func (h *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	clientName := ClientFromContext(r.Context())

	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20)) // 32 MiB
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	model, stream, err := peekModelAndStream(body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if model == "" {
		writeJSONError(w, http.StatusBadRequest, "missing model field")
		return
	}
	// Применяем compat.response_format_mode (normalize_json_object | strict_reject).
	// passthrough / пусто — no-op. Только для /v1/chat/completions: для completions/
	// embeddings поле response_format нерелевантно.
	body, err = applyResponseFormatCompat(body, h.endpoint, h.compat.ResponseFormatMode, h.log)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	rec := &core.RequestRecord{
		ID:         uuid.NewString(),
		ClientName: clientName,
		Model:      model,
		Endpoint:   h.endpoint,
		Stream:     stream,
		Status:     core.StatusPending,
		CreatedAt:  time.Now().UTC(),
	}
	// Все Insert идут через dbWriteCtx — отвязанный от r.Context() ctx с deadline.
	// Иначе при разрыве клиента финальный Insert падает с "context canceled",
	// и запись навсегда остаётся в pending — TUI видит фантомный queued.
	h.persistRecord(rec, "pending")

	job := &core.Job{
		ID:         rec.ID,
		Model:      rec.Model,
		Endpoint:   rec.Endpoint,
		ClientName: rec.ClientName,
		Stream:     rec.Stream,
		Run: func(_ context.Context, srv *core.ServerInfo) core.JobResult {
			rec.ServerName = srv.Name
			rec.StartedAt = time.Now().UTC()
			// running → Hub.broadcastSnapshot увидит выполняющуюся задачу в следующем
			// тике (раньше статус прыгал pending→completed без промежутка, и TUI не
			// видел выполняющиеся задачи).
			rec.Status = core.StatusRunning
			rec.ErrorMessage = ""
			h.persistRecord(rec, "running")

			var result core.JobResult
			if stream {
				result = h.runStream(r, w, srv, body)
			} else {
				result = h.runNonStream(r, w, srv, body)
			}

			// Если попытка провалилась И клиенту ещё ничего не отдано — scheduler
			// сделает backoff и retry/failover. Возвращаем статус в pending, иначе
			// между attempts (сотни миллисекунд backoff) в БД висит «фантомный»
			// running, и TUI показывает их по 5–6 штук одновременно при retry-burst.
			// Финальный Insert (failed/completed) перепишет статус после Submit.
			//
			// LastFailedServer фиксируется здесь же: текущий srv.Name — это сервер,
			// где запрос только что упал, и его имя нужно UI для отрисовки крестика
			// в колонке Server. На следующей попытке rec.ServerName поменяется
			// (если router выбрал другой сервер), а LastFailedServer останется
			// до финального завершения запроса.
			if result.Err != nil && !result.ResponseStarted {
				rec.Status = core.StatusPending
				rec.ErrorMessage = result.Err.Error()
				rec.LastFailedServer = srv.Name
				h.persistRecord(rec, "pending-retry")
			}
			return result
		},
	}

	res, err := h.sched.Submit(r.Context(), job)
	rec.CompletedAt = time.Now().UTC()
	rec.HTTPStatus = res.HTTPStatus
	rec.PromptTokens = res.PromptTokens
	rec.OutputTokens = res.OutputTokens
	rec.Attempt = res.Attempt
	rec.ModelReloaded = res.ModelReloaded

	if err != nil {
		rec.Status = core.StatusFailed
		rec.ErrorMessage = err.Error()
		if !res.ResponseStarted {
			// ответ ещё не начат — отдаём JSON-ошибку
			writeJSONError(w, mapErrorStatus(res.HTTPStatus), err.Error())
		}
	} else {
		rec.Status = core.StatusCompleted
	}
	h.persistRecord(rec, "final")
	h.log.Info("request finished",
		"request_id", rec.ID,
		"client", rec.ClientName,
		"model", rec.Model,
		"server", rec.ServerName,
		"status", string(rec.Status),
		"http_status", rec.HTTPStatus,
		"prompt_tokens", rec.PromptTokens,
		"output_tokens", rec.OutputTokens)
}

// persistRecord пишет/обновляет запись истории с ctx, отвязанным от клиентского
// r.Context() — иначе разрыв клиента терял бы финальное состояние и оставлял
// pending-фантом в БД. Deadline 5s страхует от зависания записи при сбоях БД.
func (h *proxyHandler) persistRecord(rec *core.RequestRecord, phase string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.history.Insert(ctx, rec); err != nil {
		h.log.Warn("history insert failed", "phase", phase, "request_id", rec.ID, "error", err.Error())
	}
}

// runNonStream выполняет один POST к upstream и копирует ответ клиенту целиком.
// Возвращает JobResult, который scheduler использует для решения retry/failover.
func (h *proxyHandler) runNonStream(r *http.Request, w http.ResponseWriter, srv *core.ServerInfo, body []byte) core.JobResult {
	bk, ok := h.backends[srv.Name]
	if !ok {
		return core.JobResult{Err: fmt.Errorf("backend %q не зарегистрирован", srv.Name)}
	}
	resp, err := bk.Forward(r.Context(), h.endpoint, bytes.NewReader(body), r.Header)
	if err != nil {
		return core.JobResult{Err: err}
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return core.JobResult{Err: fmt.Errorf("read upstream body: %w", err), HTTPStatus: resp.StatusCode}
	}
	prompt, output := peekUsage(respBody)

	if resp.StatusCode/100 != 2 {
		// Тело ответа LLM при 4xx/5xx часто содержит человекочитаемую причину
		// ("model not loaded", "context length exceeded" и т.п.) — логируем
		// первые 512 байт. Authorization и прочие секреты сюда не попадают:
		// это тело ответа upstream, не наш request.
		snippet := respBody
		if len(snippet) > 512 {
			snippet = snippet[:512]
		}
		h.log.Warn("upstream non-2xx",
			"server", srv.Name,
			"endpoint", h.endpoint,
			"http_status", resp.StatusCode,
			"body", string(snippet))
		return core.JobResult{
			Err:        fmt.Errorf("upstream %s: HTTP %d", srv.Name, resp.StatusCode),
			HTTPStatus: resp.StatusCode,
		}
	}

	// 2xx — копируем заголовки и тело клиенту.
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(respBody); err != nil {
		// тело уже частично отдано → ResponseStarted, INV-6 блокирует retry
		return core.JobResult{
			Err:             fmt.Errorf("write to client: %w", err),
			HTTPStatus:      resp.StatusCode,
			ResponseStarted: true,
			PromptTokens:    prompt,
			OutputTokens:    output,
		}
	}
	return core.JobResult{
		HTTPStatus:      resp.StatusCode,
		ResponseStarted: true,
		PromptTokens:    prompt,
		OutputTokens:    output,
	}
}

// listModelsHandler — /v1/models. Объединяет модели со всех healthy-серверов.
// Возвращает OpenAI-совместимый шейп.
func listModelsHandler(servers []*core.ServerInfo) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		seen := map[string]bool{}
		data := make([]map[string]any, 0)
		for _, s := range servers {
			if !s.Healthy.Load() {
				continue
			}
			s.Lock()
			models := append([]core.ModelInfo(nil), s.Models...)
			s.Unlock()
			for _, m := range models {
				if seen[m.Name] {
					continue
				}
				seen[m.Name] = true
				data = append(data, map[string]any{
					"id":       m.Name,
					"object":   "model",
					"owned_by": "proxylm",
				})
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   data,
		})
	}
}

// applyResponseFormatCompat применяет режим compat.response_format_mode к телу
// chat/completions-запроса:
//   - passthrough / "" — ничего не меняет;
//   - normalize_json_object — переписывает {"response_format":{"type":"json_object"}}
//     в эквивалент json_schema с allow-any-object schema (его понимают и OpenAI,
//     и LM Studio: последний отвергает старый "json_object" как HTTP 400);
//   - strict_reject — если type задан и не равен "json_schema"|"text", возвращает
//     ошибку для writeJSONError (early 400 на прокси, без обращения к upstream).
//
// Тело модифицируется только для /v1/chat/completions; для completions/embeddings
// поле response_format нерелевантно и трогать его незачем.
func applyResponseFormatCompat(body []byte, endpoint, mode string, log *slog.Logger) ([]byte, error) {
	if endpoint != "/v1/chat/completions" {
		return body, nil
	}
	if mode == "" || mode == config.ResponseFormatPassthrough {
		return body, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body, nil // не наш JSON — оставляем как есть, дальше peek уже ругнётся
	}
	rfRaw, ok := obj["response_format"]
	if !ok {
		return body, nil
	}
	rf, ok := rfRaw.(map[string]any)
	if !ok {
		return body, nil
	}
	typ, _ := rf["type"].(string)

	switch mode {
	case config.ResponseFormatStrictReject:
		if typ != "" && typ != "json_schema" && typ != "text" {
			return nil, fmt.Errorf("response_format.type must be 'json_schema' or 'text', got %q", typ)
		}
		return body, nil

	case config.ResponseFormatNormalizeJSONObject:
		if typ != "json_object" {
			return body, nil
		}
		// Заменяем на минимальный json_schema, допускающий любой JSON-объект.
		// strict=false — иначе LM Studio попытается жёстко валидировать ответ
		// против пустой schema и может отказать.
		rf["type"] = "json_schema"
		rf["json_schema"] = map[string]any{
			"name":   "response",
			"strict": false,
			"schema": map[string]any{
				"type":                 "object",
				"additionalProperties": true,
			},
		}
		obj["response_format"] = rf
		newBody, err := json.Marshal(obj)
		if err != nil {
			return body, nil
		}
		if log != nil {
			log.Debug("compat: response_format json_object → json_schema applied")
		}
		return newBody, nil
	}
	return body, nil
}

func peekModelAndStream(body []byte) (string, bool, error) {
	if len(body) == 0 {
		return "", false, fmt.Errorf("empty body")
	}
	var p struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return "", false, fmt.Errorf("invalid JSON: %w", err)
	}
	return p.Model, p.Stream, nil
}

func peekUsage(body []byte) (prompt, output int) {
	var p struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(body, &p)
	return p.Usage.PromptTokens, p.Usage.CompletionTokens
}

// copyHeaders копирует заголовки src→dst, опуская hop-by-hop (RFC 7230 §6.1).
// Единый источник правды по списку — backends.HopByHopHeaders.
func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if _, hop := backends.HopByHopHeaders[http.CanonicalHeaderKey(k)]; hop {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func mapErrorStatus(upstream int) int {
	switch {
	case upstream >= 400 && upstream < 500:
		return upstream
	case upstream >= 500:
		return http.StatusBadGateway
	default:
		return http.StatusBadGateway
	}
}
