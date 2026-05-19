package core

import (
	"testing"
)

// --- Юнит-тесты PopForFairShare ---------------------------------------------

// TestPopForFairShare_DisabledLimitBehavesLikePopFor: при maxConsecutive=0
// fair-share стратегия должна полностью совпадать с deferred (drain current
// model FIFO, затем первый совместимый).
func TestPopForFairShare_DisabledLimitBehavesLikePopFor(t *testing.T) {
	p := NewJobPool()
	srv := mkSrv(t, "a", 100, true, "m1", "m1", "m2")

	p.Push(&Job{ID: "j1", Model: "m1"})
	p.Push(&Job{ID: "j2", Model: "m2"})
	p.Push(&Job{ID: "j3", Model: "m1"})

	got := []string{}
	for {
		j := p.PopForFairShare(srv, 0)
		if j == nil {
			break
		}
		got = append(got, j.ID)
		// Имитируем смену current_model сервером после drain'а m1.
		if len(got) == 2 {
			srv.SetCurrentModel("m2")
		}
	}
	want := []string{"j1", "j3", "j2"}
	for i, w := range want {
		if i >= len(got) || got[i] != w {
			t.Errorf("disabled limit: ожидался порядок %v, получен %v", want, got)
			return
		}
	}
}

// TestPopForFairShare_ForcesSwitchAfterLimit: ConsecutiveModelCount уже достиг
// лимита, в очереди есть Job под другую модель — fair-share должен взять его,
// а НЕ продолжать drain текущей.
func TestPopForFairShare_ForcesSwitchAfterLimit(t *testing.T) {
	p := NewJobPool()
	srv := mkSrv(t, "a", 100, true, "m1", "m1", "m2")
	// Имитация: сервер уже выполнил 3 запроса подряд для m1.
	srv.Lock()
	srv.LastDispatchedModel = "m1"
	srv.ConsecutiveModelCount = 3
	srv.Unlock()

	p.Push(&Job{ID: "j_m1_a", Model: "m1"})
	p.Push(&Job{ID: "j_m2", Model: "m2"})
	p.Push(&Job{ID: "j_m1_b", Model: "m1"})

	got := p.PopForFairShare(srv, 3)
	if got == nil || got.ID != "j_m2" {
		t.Errorf("после достижения лимита 3 ожидался j_m2 (forced switch), получен %v", got)
	}
}

// TestPopForFairShare_NoOtherModelFallsBack: лимит достигнут, но в очереди
// есть только Job'ы под current_model — стратегия должна продолжить drain'ить
// её, а НЕ возвращать nil. Иначе worker будет крутиться без работы при
// доступных совместимых задачах.
func TestPopForFairShare_NoOtherModelFallsBack(t *testing.T) {
	p := NewJobPool()
	srv := mkSrv(t, "a", 100, true, "m1", "m1", "m2")
	srv.Lock()
	srv.LastDispatchedModel = "m1"
	srv.ConsecutiveModelCount = 5
	srv.Unlock()

	p.Push(&Job{ID: "j_m1_a", Model: "m1"})
	p.Push(&Job{ID: "j_m1_b", Model: "m1"})

	got := p.PopForFairShare(srv, 3)
	if got == nil || got.ID != "j_m1_a" {
		t.Errorf("при отсутствии других моделей ожидался j_m1_a (fallback на drain), получен %v", got)
	}
}

// TestPopForFairShare_BeforeLimitNormalDrain: ConsecutiveModelCount < лимита —
// поведение совпадает с обычным drain (j_m1_a перед j_m2 даже если j_m2 раньше
// в очереди).
func TestPopForFairShare_BeforeLimitNormalDrain(t *testing.T) {
	p := NewJobPool()
	srv := mkSrv(t, "a", 100, true, "m1", "m1", "m2")
	srv.Lock()
	srv.LastDispatchedModel = "m1"
	srv.ConsecutiveModelCount = 1
	srv.Unlock()

	p.Push(&Job{ID: "j_m2", Model: "m2"})
	p.Push(&Job{ID: "j_m1", Model: "m1"})

	got := p.PopForFairShare(srv, 3)
	if got == nil || got.ID != "j_m1" {
		t.Errorf("до лимита ожидался drain j_m1, получен %v", got)
	}
}

// TestPopForFairShare_RespectsVisited: forced-switch не отдаёт Job, помеченный
// visited[srv]=true — failover-логика сохраняется.
func TestPopForFairShare_RespectsVisited(t *testing.T) {
	p := NewJobPool()
	srv := mkSrv(t, "a", 100, true, "m1", "m1", "m2")
	srv.Lock()
	srv.LastDispatchedModel = "m1"
	srv.ConsecutiveModelCount = 3
	srv.Unlock()

	jM2 := &Job{ID: "j_m2", Model: "m2"}
	p.Push(&Job{ID: "j_m1", Model: "m1"})
	p.Push(jM2)
	p.MarkVisited(jM2, "a")

	// jM2 — единственный кандидат forced-switch, но он visited для "a";
	// fallback на drain m1 → j_m1.
	got := p.PopForFairShare(srv, 3)
	if got == nil || got.ID != "j_m1" {
		t.Errorf("visited jM2 должен быть пропущен, fallback на j_m1; получен %v", got)
	}
}

// TestPopForFairShare_NonHealthyReturnsNil: unhealthy сервер не получает Job
// (реактивный crash-detect). Аналогично PopFor.
func TestPopForFairShare_NonHealthyReturnsNil(t *testing.T) {
	p := NewJobPool()
	srv := mkSrv(t, "a", 100, false, "m1", "m1")
	p.Push(&Job{ID: "j", Model: "m1"})
	if got := p.PopForFairShare(srv, 0); got != nil {
		t.Errorf("unhealthy сервер не должен получать Job, получен %v", got)
	}
}
