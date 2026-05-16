package core

import (
	"errors"
	"testing"
	"time"
)

// mkSrv создаёт ServerInfo с указанными моделями и здоровьем.
func mkSrv(t *testing.T, name string, priority int, healthy bool, currentModel string, models ...string) *ServerInfo {
	t.Helper()
	s := NewServerInfo(name, "http://"+name, priority, "openai", time.Second, "")
	s.Healthy.Store(healthy)
	s.SetCurrentModel(currentModel)
	infos := make([]ModelInfo, len(models))
	for i, m := range models {
		infos[i] = ModelInfo{Name: m}
	}
	s.Lock()
	s.Models = infos
	s.Unlock()
	return s
}

func TestRouter_PickPrefersServerWithModelLoaded(t *testing.T) {
	a := mkSrv(t, "a", 100, true, "", "modelX")
	b := mkSrv(t, "b", 200, true, "modelX", "modelX") // модель уже загружена
	r := NewRouter([]*ServerInfo{a, b}, RouterModelAffinityLeastBusy)

	got, err := r.Pick("modelX", nil)
	if err != nil {
		t.Fatalf("Pick err: %v", err)
	}
	if got.Name != "b" {
		t.Errorf("ожидался b (модель загружена), получен %s", got.Name)
	}
}

func TestRouter_SkipsUnhealthy(t *testing.T) {
	a := mkSrv(t, "a", 100, false, "modelX", "modelX")
	b := mkSrv(t, "b", 200, true, "", "modelX")
	r := NewRouter([]*ServerInfo{a, b}, RouterModelAffinityLeastBusy)
	got, err := r.Pick("modelX", nil)
	if err != nil {
		t.Fatalf("Pick err: %v", err)
	}
	if got.Name != "b" {
		t.Errorf("ожидался b (a unhealthy), получен %s", got.Name)
	}
}

func TestRouter_SkipsServerWithoutModel(t *testing.T) {
	a := mkSrv(t, "a", 100, true, "", "modelY")
	b := mkSrv(t, "b", 200, true, "", "modelX")
	r := NewRouter([]*ServerInfo{a, b}, RouterModelAffinityLeastBusy)
	got, err := r.Pick("modelX", nil)
	if err != nil {
		t.Fatalf("Pick err: %v", err)
	}
	if got.Name != "b" {
		t.Errorf("ожидался b, получен %s", got.Name)
	}
}

func TestRouter_ErrNoServer(t *testing.T) {
	a := mkSrv(t, "a", 100, true, "", "modelY")
	r := NewRouter([]*ServerInfo{a}, RouterModelAffinityLeastBusy)
	_, err := r.Pick("modelX", nil)
	if err == nil {
		t.Fatal("ожидалась ошибка ErrNoServer")
	}
	var noServerErr *ErrNoServer
	if !errors.As(err, &noServerErr) {
		t.Errorf("ожидался *ErrNoServer, получен %T", err)
	}
}

func TestRouter_Exclude(t *testing.T) {
	a := mkSrv(t, "a", 100, true, "", "modelX")
	b := mkSrv(t, "b", 200, true, "", "modelX")
	r := NewRouter([]*ServerInfo{a, b}, RouterModelAffinityLeastBusy)
	got, err := r.Pick("modelX", map[string]bool{"a": true})
	if err != nil {
		t.Fatalf("Pick err: %v", err)
	}
	if got.Name != "b" {
		t.Errorf("ожидался b (a в exclude), получен %s", got.Name)
	}
}

func TestRouter_LeastBusyByQueueLen(t *testing.T) {
	a := mkSrv(t, "a", 100, true, "", "modelX")
	b := mkSrv(t, "b", 200, true, "", "modelX")
	// a имеет 5 в очереди, b — пуст; least busy должен выбрать b
	a.Lock()
	a.Queue = make([]*Job, 5)
	a.Unlock()
	r := NewRouter([]*ServerInfo{a, b}, RouterLeastBusy)
	got, err := r.Pick("modelX", nil)
	if err != nil {
		t.Fatalf("Pick err: %v", err)
	}
	if got.Name != "b" {
		t.Errorf("ожидался b (меньше очередь), получен %s (a.queue=%d)", got.Name, len(a.Queue))
	}
}

func TestRouter_RoundRobin(t *testing.T) {
	a := mkSrv(t, "a", 100, true, "", "modelX")
	b := mkSrv(t, "b", 200, true, "", "modelX")
	r := NewRouter([]*ServerInfo{a, b}, RouterRoundRobin)
	picks := []string{}
	for i := 0; i < 4; i++ {
		got, err := r.Pick("modelX", nil)
		if err != nil {
			t.Fatalf("Pick err: %v", err)
		}
		picks = append(picks, got.Name)
	}
	want := []string{"a", "b", "a", "b"}
	for i, w := range want {
		if picks[i] != w {
			t.Errorf("RR pick[%d] = %s, want %s; sequence=%v", i, picks[i], w, picks)
		}
	}
}
