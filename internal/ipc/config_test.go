package ipc

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"proxylm/internal/config"
)

const fakeFullKey = "sk-proxy-0123456789abcdef0123456789abcdef"

// fakeConfigService — ConfigService с программируемыми ответами.
type fakeConfigService struct {
	mu      sync.Mutex
	updates []ConfigUpdatePayload
	result  ConfigResultPayload
	pending []string
}

func (f *fakeConfigService) State(context.Context) ConfigStatePayload {
	return ConfigStatePayload{
		Path: "/etc/proxylm/config.yaml", Revision: "rev1",
		Config: config.Masked(&config.Config{Auth: config.Auth{
			AdminKey: "sk-admin-secret-value",
			APIKeys:  []config.APIKey{{Name: "crm", Key: fakeFullKey}},
		}}),
		Schema: config.ConfigSchema(),
	}
}

func (f *fakeConfigService) Update(_ context.Context, p ConfigUpdatePayload) ConfigResultPayload {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, p)
	return f.result
}

func (f *fakeConfigService) RestartPending() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pending
}

func recvType(t *testing.T, ctx context.Context, c *Client, want MessageType) json.RawMessage {
	t.Helper()
	env, raw, err := c.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv (want %s): %v", want, err)
	}
	if env.Type != want {
		t.Fatalf("type = %q, want %q (payload %s)", env.Type, want, raw)
	}
	return raw
}

func TestHub_ConfigGetUpdateChanged(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	hub, _, url := newTestHub(t, true)
	svc := &fakeConfigService{
		result: ConfigResultPayload{
			OK: true, Revision: "rev2",
			Applied:   []string{"retry.max_attempts"},
			Generated: map[string]string{"auth.api_keys[crm].key": fakeFullKey},
		},
		pending: []string{"proxy.port"},
	}
	hub.SetConfigService(svc)

	c1, c2 := dialTest(t, url), dialTest(t, url)
	// hello несёт restart_pending
	for _, c := range []*Client{c1, c2} {
		var hello HelloPayload
		if err := json.Unmarshal(recvType(t, ctx, c, TypeHello), &hello); err != nil {
			t.Fatal(err)
		}
		if len(hello.RestartPending) != 1 || hello.RestartPending[0] != "proxy.port" {
			t.Errorf("hello.RestartPending = %v", hello.RestartPending)
		}
		recvType(t, ctx, c, TypeStateSnapshot)
	}

	// config_get -> config_state только запросившему
	if err := c1.GetConfig(ctx); err != nil {
		t.Fatal(err)
	}
	rawState := recvType(t, ctx, c1, TypeConfigState)
	var st ConfigStatePayload
	if err := json.Unmarshal(rawState, &st); err != nil {
		t.Fatal(err)
	}
	if st.Revision != "rev1" || st.Config == nil || len(st.Schema.Fields) == 0 || st.RestartPending == nil {
		t.Errorf("state = %+v", st)
	}
	if !strings.Contains(string(rawState), `"restart_pending":[]`) {
		t.Errorf("restart_pending must be [] not null: %s", rawState)
	}
	for _, secret := range []string{fakeFullKey, "sk-admin-secret-value"} {
		if strings.Contains(string(rawState), secret) {
			t.Errorf("config_state leaks %q", secret)
		}
	}

	// config_update (успех): result с generated -> c1; config_changed без секретов -> c2
	upd := ConfigUpdatePayload{
		Revision:  "rev1",
		Config:    &config.Config{},
		SecretOps: []config.SecretOp{{Path: "auth.api_keys[crm].key", Op: "generate"}},
		Renames:   []config.Rename{{List: "backends", From: "a", To: "b"}},
	}
	if err := c1.UpdateConfig(ctx, upd); err != nil {
		t.Fatal(err)
	}
	rawRes := recvType(t, ctx, c1, TypeConfigResult)
	var res ConfigResultPayload
	if err := json.Unmarshal(rawRes, &res); err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Generated["auth.api_keys[crm].key"] != fakeFullKey {
		t.Errorf("result = %+v", res)
	}
	if !strings.Contains(string(rawRes), `"restart_required":[]`) || !strings.Contains(string(rawRes), `"restart_pending":[]`) {
		t.Errorf("slices must be [] not null: %s", rawRes)
	}
	rawCh := recvType(t, ctx, c2, TypeConfigChanged)
	var ch ConfigChangedPayload
	if err := json.Unmarshal(rawCh, &ch); err != nil {
		t.Fatal(err)
	}
	if ch.Revision != "rev2" || ch.RestartPending == nil {
		t.Errorf("changed = %+v", ch)
	}
	if strings.Contains(string(rawCh), fakeFullKey) || strings.Contains(string(rawCh), "generated") {
		t.Errorf("config_changed leaks generated key: %s", rawCh)
	}
	svc.mu.Lock()
	if len(svc.updates) != 1 || svc.updates[0].Revision != "rev1" || len(svc.updates[0].SecretOps) != 1 || len(svc.updates[0].Renames) != 1 {
		t.Errorf("service got %+v", svc.updates)
	}
	// неуспешный апдейт (conflict) — никакого config_changed
	svc.result = ConfigResultPayload{OK: false, Conflict: true, Revision: "rev3", Error: "conflict"}
	svc.mu.Unlock()
	if err := c1.UpdateConfig(ctx, upd); err != nil {
		t.Fatal(err)
	}
	var bad ConfigResultPayload
	if err := json.Unmarshal(recvType(t, ctx, c1, TypeConfigResult), &bad); err != nil {
		t.Fatal(err)
	}
	if bad.OK || !bad.Conflict {
		t.Errorf("result = %+v", bad)
	}

	// Следующий фрейм у c2 — НЕ config_changed от неуспешного апдейта: проверяем
	// порядком — hello после смены show_completed_minutes.
	hub.SetShowCompletedMinutes(7)
	for _, c := range []*Client{c1, c2} {
		var hello HelloPayload
		if err := json.Unmarshal(recvType(t, ctx, c, TypeHello), &hello); err != nil {
			t.Fatal(err)
		}
		if hello.ShowCompletedMinutes != 7 {
			t.Errorf("hello.ShowCompletedMinutes = %d", hello.ShowCompletedMinutes)
		}
	}
	// то же значение повторно — без hello: следующий ответ на stats_request.
	hub.SetShowCompletedMinutes(7)
	if err := c2.RequestStats(ctx, "24h"); err != nil {
		t.Fatal(err)
	}
	recvType(t, ctx, c2, TypeUsageStats)
}

func TestHub_ConfigWithoutService(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, url := newTestHub(t, true)
	c := dialTest(t, url)
	drainInitial(t, ctx, c)

	if err := c.GetConfig(ctx); err != nil {
		t.Fatal(err)
	}
	var st ConfigStatePayload
	if err := json.Unmarshal(recvType(t, ctx, c, TypeConfigState), &st); err != nil {
		t.Fatal(err)
	}
	if st.Error == "" || st.RestartPending == nil {
		t.Errorf("state = %+v", st)
	}

	tests := []struct {
		name string
		p    ConfigUpdatePayload
	}{
		{"service missing", ConfigUpdatePayload{Revision: "r", Config: &config.Config{}}},
		{"payload without config", ConfigUpdatePayload{Revision: "r"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := c.UpdateConfig(ctx, tt.p); err != nil {
				t.Fatal(err)
			}
			var res ConfigResultPayload
			if err := json.Unmarshal(recvType(t, ctx, c, TypeConfigResult), &res); err != nil {
				t.Fatal(err)
			}
			if res.OK || res.Error == "" || res.Applied == nil {
				t.Errorf("result = %+v", res)
			}
		})
	}
}
