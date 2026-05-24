package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"proxylm/internal/api/translate"
	"proxylm/internal/core"
	"proxylm/internal/core/backends"
)

// runStreamWithTranslator выполняет streaming-запрос к бэкенду.
// Если needTranslate=true (Anthropic-клиент → OpenAI-бэкенд), читает OpenAI SSE
// и транслирует в Anthropic SSE формат через OpenAIToAnthropicTranslator.
// Если needTranslate=false — passthrough Anthropic SSE.
func runStreamWithTranslator(
	r *http.Request, w http.ResponseWriter,
	bk backends.Backend, srv *core.ServerInfo,
	backendPath string, requestBody []byte,
	needTranslate bool, log *slog.Logger,
) core.JobResult {
	runStart := time.Now()
	resp, err := bk.Forward(r.Context(), backendPath, bytes.NewReader(requestBody), r.Header)
	if err != nil {
		return core.JobResult{Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return core.JobResult{
			Err:        fmt.Errorf("upstream %s: HTTP %d", srv.Name, resp.StatusCode),
			HTTPStatus: resp.StatusCode,
		}
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		return core.JobResult{Err: fmt.Errorf("ResponseWriter does not support Flush")}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(resp.StatusCode)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)

	responseStarted := false
	var ttftMs int64

	if needTranslate {
		// OpenAI SSE → Anthropic SSE
		translator := translate.NewOpenAIToAnthropicTranslator(srv.CurrentModelString())
		for scanner.Scan() {
			line := scanner.Bytes()
			outLines, err := translator.ProcessLine(line)
			if err != nil {
				log.Warn("stream translate error", "error", err.Error())
				continue
			}
			for _, ol := range outLines {
				if _, err := w.Write(append(ol, '\n', '\n')); err != nil {
					p, o := translator.Usage()
					return core.JobResult{
						Err: fmt.Errorf("write to client: %w", err), HTTPStatus: resp.StatusCode,
						ResponseStarted: true, PromptTokens: p, OutputTokens: o, TtftMs: ttftMs,
					}
				}
				if !responseStarted {
					ttftMs = time.Since(runStart).Milliseconds()
				}
				responseStarted = true
				flusher.Flush()
			}
			if translator.Done() {
				break
			}
			if err := r.Context().Err(); err != nil {
				p, o := translator.Usage()
				return core.JobResult{
					Err: err, HTTPStatus: resp.StatusCode,
					ResponseStarted: true, PromptTokens: p, OutputTokens: o, TtftMs: ttftMs,
				}
			}
		}
		// Emit finish events
		finishLines, _ := translator.Finish()
		for _, ol := range finishLines {
			_, _ = w.Write(append(ol, '\n', '\n'))
		}
		flusher.Flush()
		p, o := translator.Usage()
		return core.JobResult{
			HTTPStatus: resp.StatusCode, ResponseStarted: true,
			PromptTokens: p, OutputTokens: o, TtftMs: ttftMs,
		}
	}

	// Passthrough: Anthropic SSE → Anthropic SSE
	var promptTokens, outputTokens int
	for scanner.Scan() {
		line := scanner.Bytes()
		if _, err := w.Write(append(line, '\n')); err != nil {
			return core.JobResult{
				Err: fmt.Errorf("write to client: %w", err), HTTPStatus: resp.StatusCode,
				ResponseStarted: true, PromptTokens: promptTokens, OutputTokens: outputTokens, TtftMs: ttftMs,
			}
		}
		if !responseStarted {
			ttftMs = time.Since(runStart).Milliseconds()
		}
		responseStarted = true
		flusher.Flush()

		// Extract usage from message_delta events
		if data, ok := trimDataPrefix(line); ok {
			if pt, ot, ok := extractAnthropicStreamUsage(data); ok {
				promptTokens, outputTokens = pt, ot
			}
		}

		if err := r.Context().Err(); err != nil {
			return core.JobResult{
				Err: err, HTTPStatus: resp.StatusCode,
				ResponseStarted: true, PromptTokens: promptTokens, OutputTokens: outputTokens, TtftMs: ttftMs,
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return core.JobResult{
			Err: fmt.Errorf("read upstream stream: %w", err), HTTPStatus: resp.StatusCode,
			ResponseStarted: responseStarted, PromptTokens: promptTokens, OutputTokens: outputTokens, TtftMs: ttftMs,
		}
	}

	return core.JobResult{
		HTTPStatus: resp.StatusCode, ResponseStarted: true,
		PromptTokens: promptTokens, OutputTokens: outputTokens, TtftMs: ttftMs,
	}
}

// runStreamFromAnthropicBackend обрабатывает streaming от Anthropic-бэкенда
// для OpenAI-клиента. Читает Anthropic SSE и транслирует в OpenAI SSE формат.
func runStreamFromAnthropicBackend(
	r *http.Request, w http.ResponseWriter,
	bk backends.Backend, srv *core.ServerInfo,
	requestBody []byte, log *slog.Logger,
) core.JobResult {
	runStart := time.Now()
	resp, err := bk.Forward(r.Context(), "/v1/messages", bytes.NewReader(requestBody), r.Header)
	if err != nil {
		return core.JobResult{Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return core.JobResult{
			Err:        fmt.Errorf("upstream %s: HTTP %d", srv.Name, resp.StatusCode),
			HTTPStatus: resp.StatusCode,
		}
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		return core.JobResult{Err: fmt.Errorf("ResponseWriter does not support Flush")}
	}

	if resp.Header.Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/event-stream")
	}
	copyHeaders(w.Header(), resp.Header)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(resp.StatusCode)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)

	translator := translate.NewAnthropicToOpenAITranslator()
	responseStarted := false
	var ttftMs int64

	for scanner.Scan() {
		line := scanner.Bytes()
		outLines, err := translator.ProcessLine(line)
		if err != nil {
			log.Warn("stream translate error", "error", err.Error())
			continue
		}
		for _, ol := range outLines {
			if _, err := w.Write(append(ol, '\n', '\n')); err != nil {
				p, o := translator.Usage()
				return core.JobResult{
					Err: fmt.Errorf("write to client: %w", err), HTTPStatus: resp.StatusCode,
					ResponseStarted: true, PromptTokens: p, OutputTokens: o, TtftMs: ttftMs,
				}
			}
			if !responseStarted {
				ttftMs = time.Since(runStart).Milliseconds()
			}
			responseStarted = true
			flusher.Flush()
		}
		if translator.Done() {
			break
		}
		if err := r.Context().Err(); err != nil {
			p, o := translator.Usage()
			return core.JobResult{
				Err: err, HTTPStatus: resp.StatusCode,
				ResponseStarted: true, PromptTokens: p, OutputTokens: o, TtftMs: ttftMs,
			}
		}
	}

	finishLines, _ := translator.Finish()
	for _, ol := range finishLines {
		_, _ = w.Write(append(ol, '\n', '\n'))
	}
	flusher.Flush()

	p, o := translator.Usage()
	return core.JobResult{
		HTTPStatus: resp.StatusCode, ResponseStarted: true,
		PromptTokens: p, OutputTokens: o, TtftMs: ttftMs,
	}
}

func extractAnthropicStreamUsage(data []byte) (prompt, output int, ok bool) {
	var p struct {
		Type  string `json:"type"`
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
		Message *struct {
			Usage *struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return 0, 0, false
	}
	if p.Usage != nil {
		return p.Usage.InputTokens, p.Usage.OutputTokens, true
	}
	if p.Message != nil && p.Message.Usage != nil {
		return p.Message.Usage.InputTokens, p.Message.Usage.OutputTokens, true
	}
	return 0, 0, false
}
