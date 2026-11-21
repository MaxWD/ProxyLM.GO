package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"proxylm/internal/api"
	"proxylm/internal/config"
	"proxylm/internal/core"
	"proxylm/internal/ipc"
)

type rig struct {
	t      *testing.T
	path   string
	app    *Applier
	auth   *api.AuthRegistry
	router *core.Router
	sched  *core.Scheduler
	hub    *ipc.Hub
	srv    *api.Server
	ret    *atomic.Int64
	logBuf *bytes.Buffer
	cfg    *config.Config
}

func newRig(t *testing.T) *rig {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	logBuf := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(logBuf, nil))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	srvInfo := core.NewServerInfo("lmstudio-desktop", "http://127.0.0.1:1", 100, "lmstudio", time.Second, "")
	srvInfo.Healthy.Store(true)
	srvInfo.Lock()
	srvInfo.Models = []core.ModelInfo{{Name: "m"}}
	srvInfo.Unlock()
	servers := []*core.ServerInfo{srvInfo}

	router := core.NewRouter(servers, core.RouterStrategy(cfg.Routing.Strategy))
	sched := core.NewSchedulerWithOptions(servers, router, core.RetryConfig{
		MaxAttempts:    cfg.Retry.MaxAttempts,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     2 * time.Millisecond,
	}, log, core.SchedulerOptions{})
	sched.Start(ctx)

	hub := ipc.NewHub("test", servers, nil, time.Hour, cfg.Retry.MaxAttempts, cfg.TUI.ShowCompletedMinutes, nil, core.NewLiveTail(), log)
	auth := api.NewAuthRegistry(cfg.Auth)
	srv := api.New(cfg.Proxy, api.Deps{Auth: auth, Sched: sched, Servers: servers, Compat: cfg.Compat, Log: log})
	disc := core.NewDiscovery(30*time.Second, 3, log)
	var ret atomic.Int64
	ret.Store(int64(cfg.Storage.HistoryRetentionDays))

	app := New(path, cfg, Targets{Auth: auth, Server: srv, Router: router, Sched: sched, Disc: disc, Hub: hub, RetentionDays: &ret}, log)
	return &rig{t: t, path: path, app: app, auth: auth, router: router, sched: sched, hub: hub, srv: srv, ret: &ret, logBuf: logBuf, cfg: cfg}
}

// update читает состояние (masked), применяет mutate к копии и отправляет update.
func (r *rig) update(mutate func(c *config.Config), ops []config.SecretOp, renames []config.Rename) ipc.ConfigResultPayload {
	r.t.Helper()
	st := r.app.State(context.Background())
	if st.Error != "" {
		r.t.Fatalf("State: %s", st.Error)
	}
	c := st.Config.Clone()
	if mutate != nil {
		mutate(c)
	}
	return r.app.Update(context.Background(), ipc.ConfigUpdatePayload{Revision: st.Revision, Config: c, SecretOps: ops, Renames: renames})
}

func (r *rig) clientStatus(key string) int {
	r.t.Helper()
	h := r.auth.ClientAuth(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestApplier_HotVsRestart(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(c *config.Config)
		wantApplied []string
		wantRestart []string
	}{
		{"retry is hot", func(c *config.Config) { c.Retry.MaxAttempts = 5 }, []string{"retry.max_attempts"}, nil},
		{"strategy is hot", func(c *config.Config) { c.Routing.Strategy = config.StrategyRoundRobin }, []string{"routing.strategy"}, nil},
		{"log level is hot", func(c *config.Config) { c.Proxy.LogLevel = "debug" }, []string{"proxy.log_level"}, nil},
		{"compat is hot", func(c *config.Config) { c.Compat.ResponseFormatMode = config.ResponseFormatStrictReject }, []string{"compat.response_format_mode"}, nil},
		{"port needs restart", func(c *config.Config) { c.Proxy.Port = 9999 }, nil, []string{"proxy.port"}},
		{"backend url needs restart", func(c *config.Config) { c.Backends[0].URL = "http://127.0.0.1:5555" }, nil, []string{"backends[lmstudio-desktop]"}},
		{"added backend needs restart", func(c *config.Config) {
			c.Backends = append(c.Backends, config.Backend{Name: "x", URL: "http://127.0.0.1:9", TimeoutSeconds: 10})
		}, nil, []string{"backends[x]"}},
		{"mixed", func(c *config.Config) { c.Retry.MaxAttempts = 4; c.Storage.VacuumOnStart = true },
			[]string{"retry.max_attempts"}, []string{"storage.vacuum_on_start"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t)
			res := r.update(tt.mutate, nil, nil)
			if !res.OK {
				t.Fatalf("not ok: %+v", res)
			}
			if !slices.Equal(nonNil(res.Applied), nonNil(tt.wantApplied)) {
				t.Errorf("Applied = %v, want %v", res.Applied, tt.wantApplied)
			}
			if !slices.Equal(nonNil(res.RestartRequired), nonNil(tt.wantRestart)) {
				t.Errorf("RestartRequired = %v, want %v", res.RestartRequired, tt.wantRestart)
			}
			// restart_pending = только то, что отличается от РАБОТАЮЩЕЙ конфигурации
			if !slices.Equal(nonNil(res.RestartPending), nonNil(tt.wantRestart)) {
				t.Errorf("RestartPending = %v, want %v", res.RestartPending, tt.wantRestart)
			}
			if got := r.app.RestartPending(); !slices.Equal(nonNil(got), nonNil(tt.wantRestart)) {
				t.Errorf("RestartPending() = %v, want %v", got, tt.wantRestart)
			}
			if st := r.app.State(context.Background()); !slices.Equal(nonNil(st.RestartPending), nonNil(tt.wantRestart)) {
				t.Errorf("State.RestartPending = %v, want %v", st.RestartPending, tt.wantRestart)
			}
			if got := r.app.State(context.Background()).Revision; res.Revision == "" || res.Revision != got {
				t.Errorf("revision = %q, file revision = %q", res.Revision, got)
			}
		})
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func TestApplier_PendingTracksRunningConfig(t *testing.T) {
	r := newRig(t)
	if res := r.update(func(c *config.Config) { c.Proxy.Port = 9999 }, nil, nil); !slices.Equal(res.RestartPending, []string{"proxy.port"}) {
		t.Fatalf("pending = %v", res.RestartPending)
	}
	// последующий hot-апдейт не сбрасывает pending
	res := r.update(func(c *config.Config) { c.Retry.MaxAttempts = 7 }, nil, nil)
	if !slices.Equal(res.RestartPending, []string{"proxy.port"}) || len(res.RestartRequired) != 0 {
		t.Errorf("pending = %v required = %v", res.RestartPending, res.RestartRequired)
	}
	// возврат порта к работающему значению снимает pending
	res = r.update(func(c *config.Config) { c.Proxy.Port = r.cfg.Proxy.Port }, nil, nil)
	if len(res.RestartPending) != 0 {
		t.Errorf("pending after revert = %v", res.RestartPending)
	}
}

func TestApplier_HotSettersTakeEffect(t *testing.T) {
	r := newRig(t)

	// api_keys: сгенерированный ключ принимается сразу, удалённый — 401.
	oldKey := r.cfg.Auth.APIKeys[0].Key
	if got := r.clientStatus(oldKey); got != http.StatusOK {
		t.Fatalf("old key before: %d", got)
	}
	res := r.update(func(c *config.Config) {
		c.Auth.APIKeys = append(c.Auth.APIKeys[1:], config.APIKey{Name: "crm"})
	}, []config.SecretOp{{Path: "auth.api_keys[crm].key", Op: config.SecretOpGenerate}}, nil)
	if !res.OK {
		t.Fatalf("not ok: %+v", res)
	}
	newKey := res.Generated["auth.api_keys[crm].key"]
	if !strings.HasPrefix(newKey, "sk-proxy-") || len(newKey) != len("sk-proxy-")+32 {
		t.Fatalf("generated key = %q", newKey)
	}
	if got := r.clientStatus(newKey); got != http.StatusOK {
		t.Errorf("new key: %d, want 200", got)
	}
	if got := r.clientStatus(oldKey); got != http.StatusUnauthorized {
		t.Errorf("removed key: %d, want 401", got)
	}

	// strategy
	r.update(func(c *config.Config) { c.Routing.Strategy = config.StrategyLeastBusy }, nil, nil)
	if got := r.router.Strategy(); got != core.RouterLeastBusy {
		t.Errorf("router strategy = %s", got)
	}

	// retry.max_attempts реально меняет число попыток Submit'а
	r.update(func(c *config.Config) {
		c.Retry.MaxAttempts = 4
		c.Retry.InitialBackoffMs = 1
		c.Retry.MaxBackoffMs = 2
	}, nil, nil)
	var attempts atomic.Int32
	_, err := r.sched.Submit(context.Background(), &core.Job{ID: "j", Model: "m", Run: func(context.Context, *core.ServerInfo) core.JobResult {
		attempts.Add(1)
		return core.JobResult{Err: errors.New("boom")}
	}})
	if err == nil || attempts.Load() != 4 {
		t.Errorf("attempts = %d err = %v, want 4 attempts", attempts.Load(), err)
	}

	// retention
	r.update(func(c *config.Config) { c.Storage.HistoryRetentionDays = 7 }, nil, nil)
	if got := r.ret.Load(); got != 7 {
		t.Errorf("retention = %d", got)
	}

	// compat
	r.update(func(c *config.Config) { c.Compat.ResponseFormatMode = config.ResponseFormatStrictReject }, nil, nil)
}

func TestApplier_Conflict(t *testing.T) {
	r := newRig(t)
	st := r.app.State(context.Background())
	// файл правят «руками»
	data, _ := os.ReadFile(r.path)
	if err := os.WriteFile(r.path, append(data, []byte("\n# edited by hand\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	c := st.Config.Clone()
	c.Retry.MaxAttempts = 9
	res := r.app.Update(context.Background(), ipc.ConfigUpdatePayload{Revision: st.Revision, Config: c})
	if res.OK || !res.Conflict || res.Error == "" {
		t.Errorf("want conflict, got %+v", res)
	}
	if res.Applied == nil || res.RestartRequired == nil || res.RestartPending == nil {
		t.Errorf("slices must not be nil: %+v", res)
	}
	if got, _ := config.Load(r.path); got.Retry.MaxAttempts == 9 {
		t.Error("file changed despite conflict")
	}
}

func TestApplier_ValidationErrors(t *testing.T) {
	r := newRig(t)
	before := r.app.State(context.Background()).Revision
	res := r.update(func(c *config.Config) {
		c.Proxy.Port = 0
		c.Backends[0].URL = "ftp://x"
		c.Auth.APIKeys = append(c.Auth.APIKeys, config.APIKey{Name: "nokey"})
	}, nil, nil)
	if res.OK {
		t.Fatal("expected failure")
	}
	var paths []string
	for _, e := range res.Errors {
		paths = append(paths, e.Path)
	}
	for _, want := range []string{"proxy.port", "backends[lmstudio-desktop].url", "auth.api_keys[nokey].key"} {
		if !slices.Contains(paths, want) {
			t.Errorf("missing error path %s in %v", want, paths)
		}
	}
	if after := r.app.State(context.Background()).Revision; after != before {
		t.Error("file must not change on validation failure")
	}
}

func TestApplier_SecretsNeverLeak(t *testing.T) {
	r := newRig(t)
	secrets := []string{r.cfg.Auth.AdminKey, r.cfg.Auth.APIKeys[0].Key, r.cfg.Auth.APIKeys[1].Key}

	st := r.app.State(context.Background())
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range secrets {
		if bytes.Contains(data, []byte(s)) {
			t.Errorf("config_state leaks %q", s)
		}
	}

	// клиент подсовывает чужие значения секретов — они игнорируются
	res := r.update(func(c *config.Config) {
		c.Auth.AdminKey = "hijack-admin"
		c.Auth.APIKeys[0].Key = "hijack-key"
		c.Retry.MaxAttempts = 6
	}, nil, nil)
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	got, _ := config.Load(r.path)
	if got.Auth.AdminKey != r.cfg.Auth.AdminKey || got.Auth.APIKeys[0].Key != r.cfg.Auth.APIKeys[0].Key {
		t.Error("client-sent secret values were applied")
	}
	if r.clientStatus("hijack-key") != http.StatusUnauthorized {
		t.Error("client-sent key must not authenticate")
	}

	// generate: ключ только в Generated, нигде больше (и не в логах)
	res = r.update(nil, []config.SecretOp{{Path: "auth.api_keys[service-a].key", Op: "generate"}, {Path: "auth.admin_key", Op: "generate"}}, nil)
	if !res.OK || len(res.Generated) != 2 {
		t.Fatalf("%+v", res)
	}
	gen := res.Generated
	res.Generated = nil
	rest, _ := json.Marshal(res)
	logs := r.logBuf.String()
	st2, _ := json.Marshal(r.app.State(context.Background()))
	for path, key := range gen {
		for name, blob := range map[string]string{"result": string(rest), "logs": logs, "state": string(st2)} {
			if strings.Contains(blob, key) {
				t.Errorf("generated key for %s leaked into %s", path, name)
			}
		}
	}
	// admin_key — restart-class: в pending, но работающий ключ не менялся
	if !slices.Contains(res.RestartPending, "auth.admin_key") || !r.auth.IsAdmin(r.cfg.Auth.AdminKey) {
		t.Errorf("admin key handling: pending=%v", res.RestartPending)
	}
	if !strings.Contains(logs, "config updated") {
		t.Error("expected audit log entry")
	}
}

func TestApplier_RenameKeepsKey(t *testing.T) {
	r := newRig(t)
	oldKey := r.cfg.Auth.APIKeys[0].Key
	res := r.update(func(c *config.Config) { c.Auth.APIKeys[0].Name = "renamed" }, nil,
		[]config.Rename{{List: "auth.api_keys", From: "service-a", To: "renamed"}})
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	if r.clientStatus(oldKey) != http.StatusOK {
		t.Error("renamed client must keep its key")
	}
	if got, _ := config.Load(r.path); got.Auth.APIKeys[0].Name != "renamed" || got.Auth.APIKeys[0].Key != oldKey {
		t.Errorf("file: %+v", got.Auth.APIKeys)
	}
}

func TestApplier_NoChange(t *testing.T) {
	r := newRig(t)
	before := r.app.State(context.Background()).Revision
	res := r.update(nil, nil, nil)
	if !res.OK || res.Revision != before || len(res.Applied) != 0 || len(res.RestartRequired) != 0 {
		t.Errorf("%+v", res)
	}
	if _, err := os.Stat(r.path + ".bak"); err == nil {
		t.Error("no-op update must not rewrite the file")
	}
}
