package backends

import (
	"context"
	"io"
	"net/http"
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
