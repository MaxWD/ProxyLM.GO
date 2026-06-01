package backends

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HopByHopHeaders — заголовки, которые не пробрасываются end-to-end (RFC 7230 §6.1).
// Экспортируется, чтобы api/copyHeaders использовал тот же источник правды.
// Ключи — Canonical-MIME-Header форма.
var HopByHopHeaders = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

// OpenAI — клиент OpenAI-совместимого backend'а (LM Studio, Ollama /v1/*, vLLM).
type OpenAI struct {
	name    string
	baseURL string // без хвостового '/'
	apiKey  string
	client  *http.Client
	// probeKind — тип нативной пробы загруженных моделей: "ollama" | "lmstudio"
	// | "llamacpp" | "" (нет пробы). Заполняется из config.backends[].type через
	// SetProbeKind. Управляет методом LoadedModels (см. loaded_models.go).
	probeKind string
}

// NewOpenAI создаёт клиент с настроенным timeout и keep-alive пулом.
// baseURL — корень backend'а (например http://127.0.0.1:1234).
// apiKey пустой → Authorization не добавляется.
func NewOpenAI(name, baseURL, apiKey string, timeout time.Duration) (*OpenAI, error) {
	if name == "" {
		return nil, fmt.Errorf("backends.openai: name пустое")
	}
	if _, err := url.Parse(baseURL); err != nil {
		return nil, fmt.Errorf("backends.openai: невалидный baseURL: %w", err)
	}
	transport := &http.Transport{
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		// DisableCompression: true — для streaming SSE предпочтительно;
		// upstream обычно сам отдаёт без gzip, но Accept-Encoding клиент мог поставить.
		DisableCompression: true,
	}
	return &OpenAI{
		name:    name,
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		client:  &http.Client{Timeout: timeout, Transport: transport},
	}, nil
}

func (o *OpenAI) Name() string     { return o.name }
func (o *OpenAI) Protocol() string { return "openai" }

// SetProbeKind задаёт тип нативной пробы загруженных моделей по типу бэкенда из
// конфига ("ollama" | "lmstudio" | "llamacpp"). Пустая строка или "openai"
// отключают пробу. Вызывается один раз при сборке в cmd/serve.go.
func (o *OpenAI) SetProbeKind(kind string) { o.probeKind = kind }

// ListModels опрашивает /v1/models. Ожидаемый формат — OpenAI:
//
//	{"data": [{"id": "model-a", ...}, ...]}
//
// Для совместимости со старыми вариантами также принимается верхнеуровневый массив строк.
func (o *OpenAI) ListModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.baseURL+"/v1/models", nil)
	if err != nil {
		return nil, fmt.Errorf("build /v1/models request: %w", err)
	}
	o.applyAuth(req.Header)

	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET /v1/models: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		// Читаем не более 1KiB тела для сообщения об ошибке.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("GET /v1/models: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	var openAIShape struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read /v1/models body: %w", err)
	}
	if err := json.Unmarshal(body, &openAIShape); err == nil && len(openAIShape.Data) > 0 {
		out := make([]string, 0, len(openAIShape.Data))
		for _, m := range openAIShape.Data {
			if m.ID != "" {
				out = append(out, m.ID)
			}
		}
		return out, nil
	}
	// Fallback: массив строк.
	var asStrings []string
	if err := json.Unmarshal(body, &asStrings); err == nil {
		return asStrings, nil
	}
	return nil, fmt.Errorf("распарсить /v1/models не удалось: %s", strings.TrimSpace(string(body)))
}

// Forward отправляет запрос на upstream. Заголовки приходящие от клиента копируются
// (кроме hop-by-hop и Authorization). Authorization подставляется из конфига бэкенда.
func (o *OpenAI) Forward(ctx context.Context, path string, body io.Reader, headers http.Header) (*http.Response, error) {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	for k, vs := range headers {
		if _, hop := HopByHopHeaders[http.CanonicalHeaderKey(k)]; hop {
			continue
		}
		if http.CanonicalHeaderKey(k) == "Authorization" {
			continue // подставим из конфига бэкенда
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	o.applyAuth(req.Header)

	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", path, err)
	}
	return resp, nil
}

func (o *OpenAI) applyAuth(h http.Header) {
	if o.apiKey != "" {
		h.Set("Authorization", "Bearer "+o.apiKey)
	}
}
