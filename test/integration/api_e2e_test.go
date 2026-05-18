package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"proxylm/internal/api"
	"proxylm/internal/config"
	"proxylm/internal/core"
	"proxylm/internal/core/backends"
	"proxylm/internal/storage"
)

// fakeBackend — http-сервер, имитирующий OpenAI-совместимый backend.
// Поддерживает /v1/models и /v1/chat/completions (non-stream + stream).
type fakeBackend struct {
	server *httptest.Server
	models []string
}

func newFakeBackend(t *testing.T, models []string) *fakeBackend {
	t.Helper()
	fb := &fakeBackend{models: models}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", fb.handleModels)
	mux.HandleFunc("/v1/chat/completions", fb.handleChat)
	fb.server = httptest.NewServer(mux)
	t.Cleanup(fb.server.Close)
	return fb
}

func (f *fakeBackend) handleModels(w http.ResponseWriter, _ *http.Request) {
	data := []map[string]any{}
	for _, m := range f.models {
		data = append(data, map[string]any{"id": m, "object": "model"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func (f *fakeBackend) handleChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	body, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(body, &req)

	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		flusher.Flush()
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\n")
		flusher.Flush()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":      "chatcmpl-1",
		"model":   req.Model,
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "hello"}}},
		"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 1},
	})
}

// startProxy поднимает всю даemon-цепочку и возвращает базовый URL прокси.
func startProxy(t *testing.T, fb *fakeBackend) (string, *storage.History, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	history := storage.NewHistory(db)

	bk, err := backends.NewOpenAI("srv1", fb.server.URL, "", 10*time.Second)
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}
	srv := core.NewServerInfo("srv1", fb.server.URL, 100, "openai", 10*time.Second, "")
	srv.Healthy.Store(true)
	srv.Lock()
	for _, m := range fb.models {
		srv.Models = append(srv.Models, core.ModelInfo{Name: m})
	}
	srv.Unlock()
	servers := []*core.ServerInfo{srv}

	router := core.NewRouter(servers, core.RouterModelAffinityLeastBusy)
	retry := core.RetryConfig{MaxAttempts: 1, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}
	sched := core.NewScheduler(servers, router, retry, nil)
	sched.Start(ctx)

	auth := api.NewAuthRegistry(config.Auth{
		APIKeys:  []config.APIKey{{Name: "client1", Key: "client-key"}},
		AdminKey: "admin-key",
	})
	srvAPI := api.New(config.Proxy{Host: "127.0.0.1", Port: 0, LogLevel: "error"}, api.Deps{
		Auth:     auth,
		Sched:    sched,
		Backends: map[string]backends.Backend{"srv1": bk},
		Servers:  servers,
		History:  history,
		Log:      nil, // logging.Init не нужен — slog.Default подходит
	})

	// ListenAndServe в горутине; httptest нельзя — нужен полный chi-router из api.Server.
	// Поднимем на динамическом порту через временный listener? Проще: используем httptest.NewServer
	// с router'ом из API.
	// API.Server.router() приватный — добавим httptest-обёртку напрямую через api.Deps,
	// либо передадим в обход. Здесь поднимаем настоящий *http.Server с :0:
	_ = srvAPI // см. proxyTransport ниже

	// Альтернатива: используем httptest со скопированным router'ом.
	// Здесь — самый простой и стабильный путь: пробросим запросы напрямую в router
	// через api.Server.HTTPHandler() (см. ниже доп. метод). Если такой публичной точки нет,
	// тест поднимает реальный сервер. Для проверки целостности пайплайна используем
	// httptest.NewServer.
	ts := httptest.NewServer(srvAPI.Router())
	t.Cleanup(ts.Close)

	t.Cleanup(func() {
		cancel()
		sched.Wait()
	})

	return ts.URL, history, cancel
}

func TestE2E_ChatCompletions_NonStream(t *testing.T) {
	fb := newFakeBackend(t, []string{"test-model"})
	url, history, _ := startProxy(t, fb)

	req := []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
	httpReq, _ := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", bytes.NewReader(req))
	httpReq.Header.Set("Authorization", "Bearer client-key")
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d, body=%s", resp.StatusCode, body)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["model"] != "test-model" {
		t.Errorf("model в ответе = %v, want test-model", got["model"])
	}

	// История пишется асинхронно (persistRecord goroutine) — polling до записи
	// в статусе completed (cap 2s). Заменяет фиксированный time.Sleep, который
	// был нестабильным на медленных CI-раннерах.
	var records []*core.RequestRecord
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		records, err = history.List(context.Background(), 10)
		if err != nil {
			t.Fatalf("history.List: %v", err)
		}
		if len(records) > 0 && records[0].Status == core.StatusCompleted {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(records) == 0 {
		t.Fatal("в истории должна быть запись")
	}
	r := records[0]
	if r.ClientName != "client1" || r.Model != "test-model" || r.Status != core.StatusCompleted {
		t.Errorf("history record: client=%s, model=%s, status=%s",
			r.ClientName, r.Model, r.Status)
	}
	if r.PromptTokens != 5 || r.OutputTokens != 1 {
		t.Errorf("usage tokens: prompt=%d, output=%d (want 5/1)", r.PromptTokens, r.OutputTokens)
	}
}

func TestE2E_ChatCompletions_Stream(t *testing.T) {
	fb := newFakeBackend(t, []string{"test-model"})
	url, _, _ := startProxy(t, fb)

	req := []byte(`{"model":"test-model","stream":true}`)
	httpReq, _ := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", bytes.NewReader(req))
	httpReq.Header.Set("Authorization", "Bearer client-key")
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "data:") || !strings.Contains(s, "[DONE]") {
		t.Errorf("ожидался SSE с [DONE], получено: %s", s)
	}
	if !strings.Contains(s, "hello") || !strings.Contains(s, "world") {
		t.Errorf("ожидалось 'hello' и 'world' в стриме, получено: %s", s)
	}
}

func TestE2E_AuthRejectsBadKey(t *testing.T) {
	fb := newFakeBackend(t, []string{"test-model"})
	url, _, _ := startProxy(t, fb)

	req := []byte(`{"model":"test-model"}`)
	httpReq, _ := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", bytes.NewReader(req))
	httpReq.Header.Set("Authorization", "Bearer WRONG")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("ожидался 401, получен %d", resp.StatusCode)
	}
}

func TestE2E_ListModels(t *testing.T) {
	fb := newFakeBackend(t, []string{"m1", "m2"})
	url, _, _ := startProxy(t, fb)

	httpReq, _ := http.NewRequest(http.MethodGet, url+"/v1/models", nil)
	httpReq.Header.Set("Authorization", "Bearer client-key")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	var got struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Data) != 2 {
		t.Errorf("ожидалось 2 модели, получено %d", len(got.Data))
	}
}
