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

	"proxylm/internal/api/translate"
	"proxylm/internal/config"
	"proxylm/internal/core"
	"proxylm/internal/core/backends"
	"proxylm/internal/storage"
)

// anthropicHandler — handler для POST /v1/messages (Anthropic Messages API).
type anthropicHandler struct {
	sched    *core.Scheduler
	backends map[string]backends.Backend
	history  *storage.History
	log      *slog.Logger
}

func newAnthropicHandler(sched *core.Scheduler, bks map[string]backends.Backend, history *storage.History, log *slog.Logger) *anthropicHandler {
	return &anthropicHandler{
		sched:    sched,
		backends: bks,
		history:  history,
		log:      log,
	}
}

func (h *anthropicHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	clientName := ClientFromContext(r.Context())

	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}

	model, stream, maxTokens, err := peekAnthropicRequest(body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if model == "" {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "missing model field")
		return
	}
	if maxTokens <= 0 {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "max_tokens is required and must be > 0")
		return
	}

	rec := &core.RequestRecord{
		ID:         uuid.NewString(),
		ClientName: clientName,
		Model:      model,
		Endpoint:   "/v1/messages",
		Stream:     stream,
		Status:     core.StatusPending,
		CreatedAt:  time.Now().UTC(),
	}
	h.persistRecord(rec, "pending")

	job := &core.Job{
		ID:       rec.ID,
		Model:    rec.Model,
		Endpoint: rec.Endpoint,
		Run: func(_ context.Context, srv *core.ServerInfo) core.JobResult {
			rec.ServerName = srv.Name
			rec.StartedAt = time.Now().UTC()
			rec.Status = core.StatusRunning
			rec.ErrorMessage = ""
			h.persistRecord(rec, "running")

			var result core.JobResult
			if stream {
				result = h.runStream(r, w, srv, body)
			} else {
				result = h.runNonStream(r, w, srv, body)
			}
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
			writeAnthropicError(w, mapErrorStatus(res.HTTPStatus), "api_error", err.Error())
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

func (h *anthropicHandler) runNonStream(r *http.Request, w http.ResponseWriter, srv *core.ServerInfo, body []byte) core.JobResult {
	bk, ok := h.backends[srv.Name]
	if !ok {
		return core.JobResult{Err: fmt.Errorf("backend %q не зарегистрирован", srv.Name)}
	}

	requestBody := body
	backendPath := "/v1/messages"

	needTranslate := isOpenAIProtocol(srv)
	if needTranslate {
		translated, err := translate.AnthropicMessagesToOpenAIChat(body)
		if err != nil {
			return core.JobResult{Err: fmt.Errorf("translate request: %w", err)}
		}
		requestBody = translated
		backendPath = "/v1/chat/completions"
	}

	resp, err := bk.Forward(r.Context(), backendPath, bytes.NewReader(requestBody), r.Header)
	if err != nil {
		return core.JobResult{Err: err}
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return core.JobResult{Err: fmt.Errorf("read upstream body: %w", err), HTTPStatus: resp.StatusCode}
	}

	if resp.StatusCode/100 != 2 {
		snippet := respBody
		if len(snippet) > 512 {
			snippet = snippet[:512]
		}
		h.log.Warn("upstream non-2xx",
			"server", srv.Name,
			"endpoint", backendPath,
			"http_status", resp.StatusCode,
			"body", string(snippet))
		return core.JobResult{
			Err:        fmt.Errorf("upstream %s: HTTP %d", srv.Name, resp.StatusCode),
			HTTPStatus: resp.StatusCode,
		}
	}

	var prompt, output int
	if needTranslate {
		// OpenAI response → translate to Anthropic format
		prompt, output = peekUsage(respBody)
		translated, err := translate.OpenAIChatResponseToAnthropicMessage(respBody, srv.CurrentModelString())
		if err != nil {
			return core.JobResult{
				Err:             fmt.Errorf("translate response: %w", err),
				HTTPStatus:      resp.StatusCode,
				ResponseStarted: false,
			}
		}
		respBody = translated
	} else {
		prompt, output = translate.PeekAnthropicUsage(respBody)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(respBody); err != nil {
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

func (h *anthropicHandler) runStream(r *http.Request, w http.ResponseWriter, srv *core.ServerInfo, body []byte) core.JobResult {
	bk, ok := h.backends[srv.Name]
	if !ok {
		return core.JobResult{Err: fmt.Errorf("backend %q не зарегистрирован", srv.Name)}
	}

	requestBody := body
	backendPath := "/v1/messages"
	needTranslate := isOpenAIProtocol(srv)

	if needTranslate {
		translated, err := translate.AnthropicMessagesToOpenAIChat(body)
		if err != nil {
			return core.JobResult{Err: fmt.Errorf("translate request: %w", err)}
		}
		requestBody = translated
		backendPath = "/v1/chat/completions"
	}

	return runStreamWithTranslator(r, w, bk, srv, backendPath, requestBody, needTranslate, h.log)
}

//nolint:contextcheck // detached context: запись в БД должна пережить разрыв клиента
func (h *anthropicHandler) persistRecord(rec *core.RequestRecord, phase string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.history.Insert(ctx, rec); err != nil {
		h.log.Warn("history insert failed", "phase", phase, "request_id", rec.ID, "error", err.Error())
	}
}

func peekAnthropicRequest(body []byte) (model string, stream bool, maxTokens int, err error) {
	if len(body) == 0 {
		return "", false, 0, fmt.Errorf("empty body")
	}
	var p struct {
		Model     string `json:"model"`
		Stream    bool   `json:"stream"`
		MaxTokens int    `json:"max_tokens"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return "", false, 0, fmt.Errorf("invalid JSON: %w", err)
	}
	return p.Model, p.Stream, p.MaxTokens, nil
}

func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": message,
		},
	})
}

func isOpenAIProtocol(srv *core.ServerInfo) bool {
	return srv.Type != config.BackendTypeAnthropic
}
