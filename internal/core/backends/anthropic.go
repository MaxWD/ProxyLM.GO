package backends

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const anthropicVersion = "2023-06-01"

// Anthropic — клиент Anthropic Messages API (/v1/messages).
type Anthropic struct {
	name    string
	baseURL string
	apiKey  string
	client  *http.Client
}

// NewAnthropic создаёт клиент с настроенным timeout и keep-alive пулом.
func NewAnthropic(name, baseURL, apiKey string, timeout time.Duration) (*Anthropic, error) {
	if name == "" {
		return nil, fmt.Errorf("backends.anthropic: name пустое")
	}
	if _, err := url.Parse(baseURL); err != nil {
		return nil, fmt.Errorf("backends.anthropic: невалидный baseURL: %w", err)
	}
	transport := &http.Transport{
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}
	return &Anthropic{
		name:    name,
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		client:  &http.Client{Timeout: timeout, Transport: transport},
	}, nil
}

func (a *Anthropic) Name() string     { return a.name }
func (a *Anthropic) Protocol() string { return "anthropic" }

// ListModels опрашивает /v1/models с Anthropic-аутентификацией.
// Anthropic API возвращает shape, совместимый с OpenAI: {"data":[{"id":"..."}]}.
// {"data": []} — валидная форма (0 моделей), ошибкой не считается. Любая
// другая форма ответа (объекты в "data" без непустого строкового "id",
// HTML, произвольный JSON) — ошибка, оборачивающая ErrModelsShape (см.
// parseModelsBody в backend.go).
func (a *Anthropic) ListModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+"/v1/models", nil)
	if err != nil {
		return nil, fmt.Errorf("build /v1/models request: %w", err)
	}
	a.applyAuth(req.Header)

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET /v1/models: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("GET /v1/models: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read /v1/models body: %w", err)
	}
	return parseModelsBody(body)
}

// Forward отправляет запрос на Anthropic-бэкенд с x-api-key аутентификацией.
func (a *Anthropic) Forward(ctx context.Context, path string, body io.Reader, headers http.Header) (*http.Response, error) {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	for k, vs := range headers {
		if _, hop := HopByHopHeaders[http.CanonicalHeaderKey(k)]; hop {
			continue
		}
		ck := http.CanonicalHeaderKey(k)
		if ck == "Authorization" || ck == "X-Api-Key" {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	a.applyAuth(req.Header)

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", path, err)
	}
	return resp, nil
}

func (a *Anthropic) applyAuth(h http.Header) {
	if a.apiKey != "" {
		h.Set("x-api-key", a.apiKey)
	}
	h.Set("anthropic-version", anthropicVersion)
}
