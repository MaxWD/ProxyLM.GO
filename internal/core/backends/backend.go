package backends

import (
	"context"
	"io"
	"net/http"
)

// Backend — единый интерфейс к OpenAI-совместимым серверам (LM Studio / Ollama / vLLM).
// Реализация — openai.go. В будущем сюда может быть добавлен native Ollama-client.
type Backend interface {
	// Name — стабильное имя backend'а из config.backends[].name; используется в логах и БД.
	Name() string

	// ListModels возвращает список моделей с /v1/models. Используется discovery.
	ListModels(ctx context.Context) ([]string, error)

	// Forward отправляет body на бэкенд по относительному пути path (например "/v1/chat/completions").
	// Заголовки в headers копируются 1-в-1 (кроме hop-by-hop). Поскольку прокси сам управляет
	// аутентификацией к бэкенду, Authorization подставляется внутри Forward, если в конфиге
	// бэкенда задан api_key — приходящий от клиента не пробрасывается.
	//
	// Возвращает *http.Response с открытым Body, который вызывающий обязан закрыть.
	// Для streaming-ответов Body — поток (io.ReadCloser); прокси читает его построчно
	// и реflush'ит клиенту.
	Forward(ctx context.Context, path string, body io.Reader, headers http.Header) (*http.Response, error)
}
