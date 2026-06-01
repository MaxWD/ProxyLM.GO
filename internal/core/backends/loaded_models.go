package backends

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Типы нативной пробы загруженных моделей (значение config.backends[].type).
const (
	ProbeOllama   = "ollama"
	ProbeLMStudio = "lmstudio"
	ProbeLlamaCpp = "llamacpp"
)

// loadedModelsProbeLimit — потолок размера тела ответа пробы (защита от
// неожиданно крупных ответов нестандартных бэкендов).
const loadedModelsProbeLimit = 256 << 10 // 256 KiB

// LoadedModels опрашивает нативный эндпоинт бэкенда и возвращает имена моделей,
// реально загруженных в память. supported=false ⇒ тип бэкенда пробу не
// поддерживает (generic openai / cloud) — discovery такие не трогает.
//
// Соответствие тип → эндпоинт:
//
//	ollama   → GET /api/ps          {"models":[{"name":...}]}
//	lmstudio → GET /api/v1/models   {"models":[{"key":...,"loaded_instances":[...]}]}
//	llamacpp → GET /models          {"data":[{"id":...,"status":"loaded"}]}
//
// Для LM Studio используется нативный v1 REST API (рекомендован с LM Studio
// 0.4.0): модель считается загруженной, если её массив loaded_instances непуст;
// идентификатор — поле key (совпадает с id из OpenAI-совместимого /v1/models).
//
// Реализует backends.LoadedModelsProber. Введено в v0.13.0.
func (o *OpenAI) LoadedModels(ctx context.Context) ([]string, bool, error) {
	var path string
	switch o.probeKind {
	case ProbeOllama:
		path = "/api/ps"
	case ProbeLMStudio:
		path = "/api/v1/models"
	case ProbeLlamaCpp:
		path = "/models"
	default:
		return nil, false, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.baseURL+path, nil)
	if err != nil {
		return nil, true, fmt.Errorf("build %s request: %w", path, err)
	}
	o.applyAuth(req.Header)

	resp, err := o.client.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, true, fmt.Errorf("GET %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, loadedModelsProbeLimit))
	if err != nil {
		return nil, true, fmt.Errorf("read %s body: %w", path, err)
	}

	switch o.probeKind {
	case ProbeOllama:
		return parseOllamaPS(body)
	case ProbeLMStudio:
		return parseLMStudioV1(body)
	case ProbeLlamaCpp:
		return parseStateModels(body, "status", "loaded")
	}
	return nil, false, nil
}

// parseLMStudioV1 извлекает ключи загруженных моделей из ответа LM Studio v1
// REST API (GET /api/v1/models). Модель загружена ⇔ её loaded_instances непуст.
func parseLMStudioV1(body []byte) ([]string, bool, error) {
	var shape struct {
		Models []struct {
			Key             string            `json:"key"`
			LoadedInstances []json.RawMessage `json:"loaded_instances"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &shape); err != nil {
		return nil, true, fmt.Errorf("parse /api/v1/models: %w", err)
	}
	out := make([]string, 0, len(shape.Models))
	for _, m := range shape.Models {
		if len(m.LoadedInstances) > 0 && m.Key != "" {
			out = append(out, m.Key)
		}
	}
	return out, true, nil
}

// parseOllamaPS извлекает имена моделей из ответа Ollama /api/ps.
func parseOllamaPS(body []byte) ([]string, bool, error) {
	var shape struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &shape); err != nil {
		return nil, true, fmt.Errorf("parse /api/ps: %w", err)
	}
	out := make([]string, 0, len(shape.Models))
	for _, m := range shape.Models {
		name := m.Name
		if name == "" {
			name = m.Model
		}
		if name != "" {
			out = append(out, name)
		}
	}
	return out, true, nil
}

// parseStateModels извлекает id моделей из ответа вида {"data":[{"id":...,
// "<stateKey>":"<wantState>"}]}. Используется для llama.cpp (stateKey=status).
// Возвращает только модели в нужном состоянии (loaded).
func parseStateModels(body []byte, stateKey, wantState string) ([]string, bool, error) {
	var shape struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &shape); err != nil {
		return nil, true, fmt.Errorf("parse models: %w", err)
	}
	out := make([]string, 0, len(shape.Data))
	for _, item := range shape.Data {
		var state string
		if raw, ok := item[stateKey]; ok {
			_ = json.Unmarshal(raw, &state)
		}
		if !strings.EqualFold(state, wantState) {
			continue
		}
		var id string
		if raw, ok := item["id"]; ok {
			_ = json.Unmarshal(raw, &id)
		}
		if id != "" {
			out = append(out, id)
		}
	}
	return out, true, nil
}
