package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"proxylm/internal/api/translate"
	"proxylm/internal/core"
)

// runStream выполняет POST на бэкенд и копирует SSE-поток клиенту.
//
// Контракт:
//   - До первого записанного байта в ResponseWriter ResponseStarted=false → retry/failover разрешены.
//   - После первого записанного байта ResponseStarted=true → INV-6 запрещает retry/failover
//     (даже если соединение позже оборвётся).
//   - Парсим [DONE] и завершаем чтение; читаем usage из последнего data-чанка, если есть.
//
// Streaming реализуется через bufio.Scanner с увеличенным буфером (SSE-чанки от LLM
// могут быть крупными — особенно при стрим-режиме с tool_calls).
func (h *proxyHandler) runStream(r *http.Request, w http.ResponseWriter, srv *core.ServerInfo, body []byte) core.JobResult {
	bk, ok := h.backends[srv.Name]
	if !ok {
		return core.JobResult{Err: fmt.Errorf("backend %q не зарегистрирован", srv.Name)}
	}

	// Cross-protocol: OpenAI client → Anthropic backend
	if !isOpenAIProtocol(srv) {
		if h.endpoint != "/v1/chat/completions" {
			return core.JobResult{Err: fmt.Errorf("endpoint %s not supported by anthropic backend %s", h.endpoint, srv.Name)}
		}
		translated, err := translate.OpenAIChatToAnthropicMessages(body)
		if err != nil {
			return core.JobResult{Err: fmt.Errorf("translate request: %w", err)}
		}
		return runStreamFromAnthropicBackend(r, w, bk, srv, translated, h.log)
	}

	runStart := time.Now()
	resp, err := bk.Forward(r.Context(), h.endpoint, bytes.NewReader(body), r.Header)
	if err != nil {
		return core.JobResult{Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		// Ошибка до первого SSE-чанка → разрешаем retry/failover.
		return core.JobResult{
			Err:        fmt.Errorf("upstream %s: HTTP %d", srv.Name, resp.StatusCode),
			HTTPStatus: resp.StatusCode,
		}
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		return core.JobResult{Err: fmt.Errorf("ResponseWriter не поддерживает Flush")}
	}

	// SSE-заголовки (если бэкенд не поставил — поставим сами).
	if resp.Header.Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/event-stream")
	}
	copyHeaders(w.Header(), resp.Header)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(resp.StatusCode)

	scanner := bufio.NewScanner(resp.Body)
	// SSE-чанки могут быть до нескольких сотен KB при tool_calls/embeddings → 1 MiB.
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)

	responseStarted := false
	var promptTokens, outputTokens int
	var lastData []byte
	var ttftMs int64

	for scanner.Scan() {
		line := scanner.Bytes()
		if _, err := w.Write(append(line, '\n')); err != nil {
			return core.JobResult{
				Err:             fmt.Errorf("write to client: %w", err),
				HTTPStatus:      resp.StatusCode,
				ResponseStarted: true,
				PromptTokens:    promptTokens,
				OutputTokens:    outputTokens,
				TtftMs:          ttftMs,
			}
		}
		if !responseStarted {
			ttftMs = time.Since(runStart).Milliseconds()
		}
		responseStarted = true
		flusher.Flush()

		// Парсим data: чанки для usage и [DONE].
		if data, ok := trimDataPrefix(line); ok {
			trimmed := strings.TrimSpace(string(data))
			if trimmed == "[DONE]" {
				break
			}
			lastData = append(lastData[:0], data...) // последний non-[DONE] data
			if pt, ot, ok := extractUsage(data); ok {
				promptTokens, outputTokens = pt, ot
			}
		}

		// Уважаем cancel клиента (он может отключиться посреди стрима).
		if err := r.Context().Err(); err != nil {
			return core.JobResult{
				Err:             err,
				HTTPStatus:      resp.StatusCode,
				ResponseStarted: true,
				PromptTokens:    promptTokens,
				OutputTokens:    outputTokens,
				TtftMs:          ttftMs,
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return core.JobResult{
			Err:             fmt.Errorf("read upstream stream: %w", err),
			HTTPStatus:      resp.StatusCode,
			ResponseStarted: responseStarted,
			PromptTokens:    promptTokens,
			OutputTokens:    outputTokens,
			TtftMs:          ttftMs,
		}
	}

	// Если usage не пришёл в data, попробуем извлечь из lastData (некоторые backend'ы
	// кладут usage только в финальный delta перед [DONE]).
	if promptTokens == 0 && outputTokens == 0 && len(lastData) > 0 {
		if pt, ot, ok := extractUsage(lastData); ok {
			promptTokens, outputTokens = pt, ot
		}
	}

	return core.JobResult{
		HTTPStatus:      resp.StatusCode,
		ResponseStarted: true,
		PromptTokens:    promptTokens,
		OutputTokens:    outputTokens,
		TtftMs:          ttftMs,
	}
}

// trimDataPrefix отделяет "data: " префикс SSE-строки.
func trimDataPrefix(line []byte) ([]byte, bool) {
	const prefix = "data:"
	if !bytes.HasPrefix(line, []byte(prefix)) {
		return nil, false
	}
	rest := line[len(prefix):]
	// Опциональный пробел после "data:".
	if len(rest) > 0 && rest[0] == ' ' {
		rest = rest[1:]
	}
	return rest, true
}

// extractUsage пытается распарсить data-чанк как JSON с полем usage.
func extractUsage(data []byte) (prompt, output int, ok bool) {
	var p struct {
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &p); err != nil || p.Usage == nil {
		return 0, 0, false
	}
	return p.Usage.PromptTokens, p.Usage.CompletionTokens, true
}
