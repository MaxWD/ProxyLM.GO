package backends

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Backend — единый интерфейс к LLM-серверам.
// Реализации: openai.go (OpenAI-совместимые), anthropic.go (Anthropic Messages API).
type Backend interface {
	// Name — стабильное имя backend'а из config.backends[].name; используется в логах и БД.
	Name() string

	// Protocol — протокол, по которому бэкенд ожидает запросы: "openai" или "anthropic".
	Protocol() string

	// ListModels возвращает список моделей. Используется discovery.
	ListModels(ctx context.Context) ([]string, error)

	// Forward отправляет body на бэкенд по относительному пути path (например "/v1/chat/completions"
	// или "/v1/messages"). Заголовки в headers копируются 1-в-1 (кроме hop-by-hop).
	// Аутентификация к бэкенду подставляется внутри Forward из конфига — приходящий
	// от клиента токен не пробрасывается.
	//
	// Возвращает *http.Response с открытым Body, который вызывающий обязан закрыть.
	Forward(ctx context.Context, path string, body io.Reader, headers http.Header) (*http.Response, error)
}

// LoadedModelsProber — опциональная способность бэкенда сообщить, какие модели
// реально загружены в память (VRAM/RAM) прямо сейчас. Реализуется только теми
// бэкендами, у которых есть нативный эндпоинт (Ollama /api/ps, LM Studio
// /api/v0/models state, llama.cpp /models status). Discovery делает type-assert
// к этому интерфейсу — бэкенды без него (cloud / generic openai) не пробуются.
//
// supported=false означает «этот бэкенд пробу не поддерживает» (даже если err
// nil); err != nil — проба поддерживается, но запрос упал (сеть/HTTP). Введено
// в v0.13.0.
type LoadedModelsProber interface {
	LoadedModels(ctx context.Context) (models []string, supported bool, err error)
}

// ErrModelsShape reports that /v1/models responded 2xx but the body is not
// an OpenAI-compatible models list. Detect with errors.Is.
var ErrModelsShape = errors.New("response shape is not OpenAI-compatible")

// maxModelsShapeSnippet caps the body snippet included in ErrModelsShape-wrapping
// errors, mirroring the existing non-2xx error paths.
const maxModelsShapeSnippet = 1024

// parseModelsBody parses a 2xx /v1/models response body shared by openai.go and
// anthropic.go (both expose OpenAI-compatible shapes).
//
// Accepted shapes:
//   - {"data": [{"id": "model-a"}, ...]} — all entries must carry a non-empty
//     string "id"; {"data": []} is valid and returns an empty, non-nil slice.
//   - top-level JSON string array (legacy fallback), including [].
//
// Anything else — objects in "data" missing "id", HTML, error JSON without
// "data", scalars — returns an error wrapping ErrModelsShape with a ≤1KiB
// body snippet.
func parseModelsBody(body []byte) ([]string, error) {
	var asObject map[string]json.RawMessage
	if err := json.Unmarshal(body, &asObject); err == nil {
		if raw, ok := asObject["data"]; ok {
			var entries []struct {
				ID *string `json:"id"`
			}
			if err := json.Unmarshal(raw, &entries); err != nil {
				return nil, fmt.Errorf("parse /v1/models \"data\": %w: %s", ErrModelsShape, snippet(body))
			}
			out := make([]string, 0, len(entries))
			for _, e := range entries {
				if e.ID == nil || *e.ID == "" {
					return nil, fmt.Errorf("parse /v1/models: entry in \"data\" missing non-empty \"id\": %w: %s", ErrModelsShape, snippet(body))
				}
				out = append(out, *e.ID)
			}
			return out, nil
		}
	}

	var asStrings []string
	if err := json.Unmarshal(body, &asStrings); err == nil {
		if asStrings == nil {
			asStrings = []string{}
		}
		return asStrings, nil
	}

	return nil, fmt.Errorf("parse /v1/models: %w: %s", ErrModelsShape, snippet(body))
}

// snippet returns a trimmed, size-capped preview of body for error messages.
func snippet(body []byte) string {
	if len(body) > maxModelsShapeSnippet {
		body = body[:maxModelsShapeSnippet]
	}
	return strings.TrimSpace(string(body))
}
