package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"proxylm/internal/api"
	"proxylm/internal/config"
	"proxylm/internal/core"
	"proxylm/internal/core/backends"
	"proxylm/internal/daemon"
	"proxylm/internal/ipc"
	"proxylm/internal/storage"
)

const cfgEditTimeout = 10 * time.Second

// recvUntil читает фреймы клиента, пока не придёт сообщение типа want. Все
// просмотренные сырые payload'ы (включая искомый) возвращаются для проверок
// на утечку секретов. Синхронизация только через ctx-таймаут Recv.
func recvUntil(t *testing.T, c *ipc.Client, want ipc.MessageType) (json.RawMessage, []json.RawMessage) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), cfgEditTimeout)
	defer cancel()
	var seen []json.RawMessage
	for {
		env, payload, err := c.Recv(ctx)
		if err != nil {
			t.Fatalf("Recv while waiting for %s: %v", want, err)
		}
		seen = append(seen, payload)
		if env.Type == want {
			return payload, seen
		}
	}
}

func dialAdmin(t *testing.T, baseURL string) *ipc.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), cfgEditTimeout)
	defer cancel()
	c, err := ipc.Dial(ctx, baseURL, "sk-admin-replace-me")
	if err != nil {
		t.Fatalf("ipc.Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func chatStatus(t *testing.T, baseURL, key string) int {
	t.Helper()
	body := []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func commentLines(data []byte) []string {
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

func TestConfigEdit_EndToEnd(t *testing.T) {
	fb := newFakeBackend(t, []string{"test-model"})

	// 1. config.yaml из шаблона; первый бэкенд указывает на fake backend.
	tmpl, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	orig := bytes.ReplaceAll(tmpl, []byte("\r\n"), []byte("\n"))
	if !bytes.Contains(orig, []byte("http://127.0.0.1:1234")) {
		t.Fatal("example config no longer contains the lmstudio url; adapt the test")
	}
	orig = bytes.Replace(orig, []byte("http://127.0.0.1:1234"), []byte(fb.server.URL), 1)
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	history := storage.NewHistory(db)

	servers := make([]*core.ServerInfo, 0, len(cfg.Backends))
	backendMap := map[string]backends.Backend{}
	for i, b := range cfg.Backends {
		timeout := time.Duration(b.TimeoutSeconds) * time.Second
		o, err := backends.NewOpenAI(b.Name, b.URL, b.APIKey, timeout)
		if err != nil {
			t.Fatalf("NewOpenAI: %v", err)
		}
		s := core.NewServerInfo(b.Name, b.URL, b.Priority, b.Type, timeout, b.APIKey)
		if i == 0 { // только первый — наш fake backend
			s.Healthy.Store(true)
			s.Lock()
			s.Models = append(s.Models, core.ModelInfo{Name: "test-model"})
			s.Unlock()
		}
		servers = append(servers, s)
		backendMap[b.Name] = o
	}
	router := core.NewRouter(servers, core.RouterStrategy(cfg.Routing.Strategy))
	retry := core.RetryConfig{
		MaxAttempts:    cfg.Retry.MaxAttempts,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
	}
	sched := core.NewSchedulerWithOptions(servers, router, retry, nil, core.SchedulerOptions{
		MaxConsecutivePerModel: cfg.Scheduler.MaxConsecutivePerModel,
	})
	sched.Start(ctx)
	t.Cleanup(func() {
		cancel()
		sched.Wait()
		_ = db.Close()
	})

	hub := ipc.NewHub("test", servers, history, time.Hour, cfg.Retry.MaxAttempts,
		cfg.TUI.ShowCompletedMinutes, core.NewPerfTracker(), core.NewLiveTail(), nil)
	hub.SetRetentionDays(cfg.Storage.HistoryRetentionDays)
	go hub.Run(ctx)

	auth := api.NewAuthRegistry(cfg.Auth)
	srv := api.New(cfg.Proxy, api.Deps{
		Auth:     auth,
		Sched:    sched,
		Backends: backendMap,
		Servers:  servers,
		History:  history,
		Compat:   cfg.Compat,
		LiveTail: core.NewLiveTail(),
	})
	hub.SetConfigService(daemon.New(cfgPath, cfg, daemon.Targets{
		Auth:   auth,
		Server: srv,
		Router: router,
		Sched:  sched,
		Hub:    hub,
	}, nil))
	srv.SetAdminStream(hub.Handler())
	ts := httptest.NewServer(srv.Router())
	t.Cleanup(ts.Close)

	// 2. Два admin-клиента; A запрашивает конфиг.
	a := dialAdmin(t, ts.URL)
	b := dialAdmin(t, ts.URL)
	// B должен быть зарегистрирован в Hub до апдейта: hello приходит после регистрации.
	_, _ = recvUntil(t, b, ipc.TypeHello)

	gctx, gcancel := context.WithTimeout(context.Background(), cfgEditTimeout)
	defer gcancel()
	if err := a.GetConfig(gctx); err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	raw, _ := recvUntil(t, a, ipc.TypeConfigState)
	var st ipc.ConfigStatePayload
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("decode config_state: %v", err)
	}
	if st.Error != "" || st.Config == nil || st.Revision == "" {
		t.Fatalf("bad config_state: err=%q cfg=%v rev=%q", st.Error, st.Config != nil, st.Revision)
	}
	secrets := []string{"sk-proxy-replace-me-aaaaa", "sk-proxy-replace-me-bbbbb", "sk-admin-replace-me"}
	for _, s := range secrets {
		if strings.Contains(string(raw), s) {
			t.Errorf("config_state leaks full secret %q", s)
		}
	}
	startRev := st.Revision

	// 3. Апдейт: retry, удаление ключа service-b, новый клиент newsvc (generate), порт.
	upd := *st.Config
	upd.Retry.MaxAttempts = 5
	var keys []config.APIKey
	for _, k := range upd.Auth.APIKeys {
		if k.Name != "service-b" {
			keys = append(keys, k)
		}
	}
	keys = append(keys, config.APIKey{Name: "newsvc"})
	upd.Auth.APIKeys = keys
	upd.Proxy.Port = 18099

	if err := a.UpdateConfig(gctx, ipc.ConfigUpdatePayload{
		Revision:  startRev,
		Config:    &upd,
		SecretOps: []config.SecretOp{{Path: "auth.api_keys[newsvc].key", Op: config.SecretOpGenerate}},
	}); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	raw, _ = recvUntil(t, a, ipc.TypeConfigResult)
	var res ipc.ConfigResultPayload
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode config_result: %v", err)
	}
	if !res.OK || res.Conflict {
		t.Fatalf("update not ok: %+v", res)
	}
	assertContains := func(name string, list []string, want string) {
		t.Helper()
		for _, s := range list {
			if s == want || strings.HasPrefix(s, want) {
				return
			}
		}
		t.Errorf("%s = %v, want entry %q", name, list, want)
	}
	assertContains("applied", res.Applied, "retry.max_attempts")
	assertContains("applied", res.Applied, "auth.api_keys")
	if len(res.RestartRequired) != 1 || res.RestartRequired[0] != "proxy.port" {
		t.Errorf("restart_required = %v, want [proxy.port]", res.RestartRequired)
	}
	assertContains("restart_pending", res.RestartPending, "proxy.port")
	if len(res.Generated) != 1 {
		t.Fatalf("generated = %v, want exactly one key", res.Generated)
	}
	var newKey string
	for _, v := range res.Generated {
		newKey = v
	}
	if !strings.HasPrefix(newKey, config.ClientKeyPrefix) || newKey == config.ClientKeyPrefix {
		t.Fatalf("generated key %q lacks prefix %q", newKey, config.ClientKeyPrefix)
	}
	if res.Revision == "" || res.Revision == startRev {
		t.Errorf("revision did not change: %q", res.Revision)
	}

	// 4. B получает config_changed без сгенерированного ключа ни в одном фрейме.
	raw, seen := recvUntil(t, b, ipc.TypeConfigChanged)
	for _, f := range seen {
		if strings.Contains(string(f), newKey) {
			t.Errorf("generated key leaked to other client: %s", f)
		}
	}
	var ch ipc.ConfigChangedPayload
	if err := json.Unmarshal(raw, &ch); err != nil {
		t.Fatalf("decode config_changed: %v", err)
	}
	if ch.Revision != res.Revision {
		t.Errorf("config_changed revision = %q, want %q", ch.Revision, res.Revision)
	}
	assertContains("config_changed.restart_pending", ch.RestartPending, "proxy.port")

	// 5. HTTP: hot-применение ключей.
	httpTests := []struct {
		name string
		key  string
		want int
	}{
		{"generated_key_accepted", newKey, http.StatusOK},
		{"removed_key_rejected", "sk-proxy-replace-me-bbbbb", http.StatusUnauthorized},
		{"kept_key_accepted", "sk-proxy-replace-me-aaaaa", http.StatusOK},
	}
	for _, tt := range httpTests {
		t.Run(tt.name, func(t *testing.T) {
			if got := chatStatus(t, ts.URL, tt.key); got != tt.want {
				t.Errorf("status = %d, want %d", got, tt.want)
			}
		})
	}

	// 6. Файл: комментарии сохранены, .bak создан, ревизия изменилась.
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, c := range commentLines(after) {
		have[c] = true
	}
	for _, c := range commentLines(orig) {
		if !have[c] {
			t.Errorf("comment line lost after save: %q", c)
		}
	}
	if _, err := os.Stat(cfgPath + ".bak"); err != nil {
		t.Errorf("config.yaml.bak: %v", err)
	}
	if bytes.Equal(after, orig) {
		t.Error("config file unchanged after update")
	}
	if strings.Contains(string(after), "service-b") {
		t.Error("removed client service-b still present in file")
	}

	// 7. Повтор со старой ревизией — конфликт, файл не меняется.
	if err := a.UpdateConfig(gctx, ipc.ConfigUpdatePayload{Revision: startRev, Config: &upd}); err != nil {
		t.Fatalf("UpdateConfig(stale): %v", err)
	}
	raw, _ = recvUntil(t, a, ipc.TypeConfigResult)
	var stale ipc.ConfigResultPayload
	if err := json.Unmarshal(raw, &stale); err != nil {
		t.Fatalf("decode stale result: %v", err)
	}
	if !stale.Conflict || stale.OK {
		t.Errorf("stale update: conflict=%v ok=%v, want conflict=true ok=false (%s)", stale.Conflict, stale.OK, fmt.Sprint(stale.Error))
	}
	final, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(final, after) {
		t.Error("file changed after conflicting update")
	}
}
