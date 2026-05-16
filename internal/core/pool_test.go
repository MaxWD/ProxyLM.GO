package core

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- Unit-тесты pool.go ------------------------------------------------------

// TestJobPool_PopFor_DrainCurrentModelFirst: pool содержит запросы для разных
// моделей; сервер с current_model=m1 должен выгрести все m1 до того, как
// возьмёт m2 (INV-2/3).
func TestJobPool_PopFor_DrainCurrentModelFirst(t *testing.T) {
	p := NewJobPool()
	srv := mkSrv(t, "a", 100, true, "m1", "m1", "m2")

	j1 := &Job{ID: "j1", Model: "m1"}
	j2 := &Job{ID: "j2", Model: "m2"}
	j3 := &Job{ID: "j3", Model: "m1"}
	p.Push(j1)
	p.Push(j2)
	p.Push(j3)

	got := []string{}
	for {
		j := p.PopFor(srv)
		if j == nil {
			break
		}
		got = append(got, j.ID)
		// Для теста имитируем switch модели сервером после drain'а current_model.
		if len(got) == 2 {
			srv.SetCurrentModel(j2.Model)
		}
	}
	want := []string{"j1", "j3", "j2"}
	for i, w := range want {
		if i >= len(got) || got[i] != w {
			t.Errorf("ожидался порядок %v, получен %v", want, got)
			return
		}
	}
}

// TestJobPool_PopFor_FirstCompatibleAfterEmptyCurrent: current="" → берётся
// первый Job, для которого j.Model ∈ s.Models (несовместимые пропускаются).
func TestJobPool_PopFor_FirstCompatibleAfterEmptyCurrent(t *testing.T) {
	p := NewJobPool()
	srv := mkSrv(t, "a", 100, true, "", "m1") // только m1

	p.Push(&Job{ID: "j1", Model: "mX"}) // несовместим
	p.Push(&Job{ID: "j2", Model: "m1"}) // совместим
	p.Push(&Job{ID: "j3", Model: "m1"})

	got := p.PopFor(srv)
	if got == nil || got.ID != "j2" {
		t.Errorf("ожидался j2, получен %v", got)
	}
}

// TestJobPool_PopFor_RespectsVisited: Job, помеченный visited[srv]=true,
// серверу srv не отдаётся (failover-логика).
func TestJobPool_PopFor_RespectsVisited(t *testing.T) {
	p := NewJobPool()
	a := mkSrv(t, "a", 100, true, "", "m1")
	b := mkSrv(t, "b", 200, true, "", "m1")

	j := &Job{ID: "j", Model: "m1"}
	p.Push(j)
	p.MarkVisited(j, "a")

	if got := p.PopFor(a); got != nil {
		t.Errorf("PopFor(a) после MarkVisited(a) должен вернуть nil, получен %v", got)
	}
	if got := p.PopFor(b); got == nil || got.ID != "j" {
		t.Errorf("PopFor(b) должен вернуть j, получен %v", got)
	}
}

// TestJobPool_Remove: cancel-ветка Submit'а удаляет Job до того, как worker
// его подхватил.
func TestJobPool_Remove(t *testing.T) {
	p := NewJobPool()
	p.Push(&Job{ID: "a", Model: "m1"})
	p.Push(&Job{ID: "b", Model: "m1"})
	if !p.Remove("a") {
		t.Error("Remove(a) ожидался true")
	}
	if p.Remove("a") {
		t.Error("повторный Remove(a) должен дать false")
	}
	if p.Len() != 1 {
		t.Errorf("Len = %d, want 1", p.Len())
	}
}

// TestPickIdleByPriority: самый приоритетный idle-сервер выбран. Если он busy —
// следующий по priority.
func TestPickIdleByPriority(t *testing.T) {
	a := mkSrv(t, "a", 100, true, "", "m1") // высокий приоритет
	b := mkSrv(t, "b", 200, true, "", "m1")
	c := mkSrv(t, "c", 300, true, "", "m1")
	servers := []*ServerInfo{a, b, c}

	if got := pickIdleByPriority(servers, "m1", nil); got != a {
		t.Errorf("ожидался a (priority 100), получен %v", got)
	}
	a.InFlight.Store(true)
	if got := pickIdleByPriority(servers, "m1", nil); got != b {
		t.Errorf("a занят — ожидался b, получен %v", got)
	}
	b.InFlight.Store(true)
	if got := pickIdleByPriority(servers, "m1", nil); got != c {
		t.Errorf("a,b заняты — ожидался c, получен %v", got)
	}
	c.InFlight.Store(true)
	if got := pickIdleByPriority(servers, "m1", nil); got != nil {
		t.Errorf("все заняты — ожидался nil, получен %v", got)
	}
}

// TestPickIdleByPriority_SkipsNonCompatible: высокоприоритетный сервер без
// модели m1 пропускается; берётся следующий совместимый.
func TestPickIdleByPriority_SkipsNonCompatible(t *testing.T) {
	a := mkSrv(t, "a", 100, true, "", "mX") // только mX
	b := mkSrv(t, "b", 200, true, "", "m1") // совместим
	servers := []*ServerInfo{a, b}

	if got := pickIdleByPriority(servers, "m1", nil); got != b {
		t.Errorf("ожидался b (совместим), получен %v", got)
	}
}

// --- E2E-тесты deferred-режима Scheduler'а ----------------------------------

// makeDeferredSched — хелпер: создаёт Scheduler в режиме deferred над servers.
func makeDeferredSched(t *testing.T, servers []*ServerInfo, retry RetryConfig) (*Scheduler, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := NewRouter(servers, RouterDeferredModelThenCapable)
	s := NewScheduler(servers, r, retry, nil)
	if s.pool == nil {
		t.Fatal("ожидался initialized pool в deferred-режиме")
	}
	s.Start(ctx)
	return s, cancel
}

// TestScheduler_Deferred_PriorityOnColdStart: оба сервера свободны и имеют m1;
// первый Submit должен пойти на сервер с минимальным Priority.
func TestScheduler_Deferred_PriorityOnColdStart(t *testing.T) {
	a := mkSrv(t, "a", 100, true, "", "m1") // более приоритетный
	b := mkSrv(t, "b", 200, true, "", "m1")
	s, cancel := makeDeferredSched(t, []*ServerInfo{a, b}, RetryConfig{MaxAttempts: 1})
	defer cancel()

	run := func(_ context.Context, srv *ServerInfo) JobResult {
		return JobResult{HTTPStatus: 200, Server: srv.Name}
	}

	res, err := s.Submit(context.Background(), &Job{ID: "j1", Model: "m1", Run: run})
	if err != nil {
		t.Fatalf("Submit err: %v", err)
	}
	if res.Server != "a" {
		t.Errorf("Q1: первый Job должен попасть на приоритетный сервер 'a', попал на %q", res.Server)
	}
}

// TestScheduler_Deferred_DistributesBetweenIdleServers: пока приоритетный
// сервер занят медленным Job'ом, второй должен взять параллельные Submit'ы
// той же модели. Это и есть фикс наблюдаемой проблемы — affinity больше
// не «прибивает» очередь к одному серверу.
func TestScheduler_Deferred_DistributesBetweenIdleServers(t *testing.T) {
	a := mkSrv(t, "a", 100, true, "", "m1")
	b := mkSrv(t, "b", 200, true, "", "m1")
	s, cancel := makeDeferredSched(t, []*ServerInfo{a, b}, RetryConfig{MaxAttempts: 1})
	defer cancel()

	// Первый Job — «удерживающий» сервер a, чтобы последующие пошли на b.
	holdRelease := make(chan struct{})
	hold := make(chan struct{})
	holdRun := func(_ context.Context, srv *ServerInfo) JobResult {
		close(hold) // сигнал: a захвачен
		<-holdRelease
		return JobResult{HTTPStatus: 200, Server: srv.Name}
	}

	go func() {
		_, _ = s.Submit(context.Background(), &Job{ID: "hold", Model: "m1", Run: holdRun})
	}()
	select {
	case <-hold:
	case <-time.After(2 * time.Second):
		t.Fatal("hold-Job не начал работу — что-то не так с initial dispatch")
	}

	// Теперь a занят. Параллельный Submit того же model — должен пойти на b
	// (а не ждать освобождения a, как было в affinity-режиме).
	resCh := make(chan JobResult, 1)
	go func() {
		res, _ := s.Submit(context.Background(), &Job{
			ID:    "j2",
			Model: "m1",
			Run: func(_ context.Context, srv *ServerInfo) JobResult {
				return JobResult{HTTPStatus: 200, Server: srv.Name}
			},
		})
		resCh <- res
	}()

	select {
	case res := <-resCh:
		if res.Server != "b" {
			t.Errorf("ожидался server=b (a занят hold-Job'ом), получен %q", res.Server)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("второй Job не подхвачен — pull-распределение не работает")
	}
	close(holdRelease)
}

// TestScheduler_Deferred_DrainBeforeSwap: один сервер, current_model=m1,
// pool заполнен [m1, m2, m1]. Worker должен отработать оба m1, и только потом m2.
// Это INV-2/3, перенесённый из push-режима в pull.
func TestScheduler_Deferred_DrainBeforeSwap(t *testing.T) {
	srv := mkSrv(t, "a", 100, true, "m1", "m1", "m2")
	s, cancel := makeDeferredSched(t, []*ServerInfo{srv}, RetryConfig{MaxAttempts: 1})
	defer cancel()

	var orderMu sync.Mutex
	order := []string{}
	mkRun := func(id, model string) func(context.Context, *ServerInfo) JobResult {
		return func(context.Context, *ServerInfo) JobResult {
			orderMu.Lock()
			order = append(order, id+"/"+model)
			orderMu.Unlock()
			return JobResult{HTTPStatus: 200}
		}
	}

	var wg sync.WaitGroup
	submit := func(id, model string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = s.Submit(context.Background(), &Job{ID: id, Model: model, Run: mkRun(id, model)})
		}()
	}
	// Запускаем все три Submit'а почти одновременно — порядок в pool определяет
	// порядок их Push'а; чтобы получить детерминированный [m1, m2, m1] вход,
	// делаем sequential Submit с маленькой синхронизацией через WaitGroup
	// (а не time.Sleep, как требует CLAUDE.md).
	//
	// Для детерминированной последовательности добавления — последовательный
	// Push в pool через прямые вызовы pool.Push, ручной Wake и блок жидания.
	// Но это потребует ручного управления done-каналом. Проще: один Submit
	// в фоне держит first slot, далее серийный Push.
	//
	// Здесь идём проще: первый Submit стартует и идёт в работу (m1 уже current);
	// остальные два кладутся в pool через свои Submit'ы быстро друг за другом.
	submit("j1", "m1")
	submit("j2", "m2")
	submit("j3", "m1")
	wg.Wait()

	orderMu.Lock()
	defer orderMu.Unlock()
	// j2 (m2) ДОЛЖЕН быть последним: drain m1 → потом switch на m2.
	if len(order) != 3 || order[len(order)-1] != "j2/m2" {
		t.Errorf("INV-2/3 нарушен: ожидали m2 в конце, получили %v", order)
	}
}

// TestScheduler_Deferred_FailoverViaPool: первая попытка на a падает →
// Job возвращается в pool с visited[a]=true → b его подхватывает и успешно
// завершает.
func TestScheduler_Deferred_FailoverViaPool(t *testing.T) {
	a := mkSrv(t, "a", 100, true, "", "m1")
	b := mkSrv(t, "b", 200, true, "", "m1")
	s, cancel := makeDeferredSched(t, []*ServerInfo{a, b},
		RetryConfig{MaxAttempts: 2,
			InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond})
	defer cancel()

	var callsA, callsB atomic.Int32
	run := func(_ context.Context, srv *ServerInfo) JobResult {
		if srv.Name == "a" {
			callsA.Add(1)
			return JobResult{Err: fmt.Errorf("a всегда падает"), Server: "a"}
		}
		callsB.Add(1)
		return JobResult{HTTPStatus: 200, Server: "b"}
	}

	res, err := s.Submit(context.Background(), &Job{ID: "j", Model: "m1", Run: run})
	if err != nil {
		t.Fatalf("ожидался успех после failover, получено %v", err)
	}
	if res.Server != "b" {
		t.Errorf("ожидался финальный server=b, получен %q", res.Server)
	}
	if callsA.Load() == 0 {
		t.Error("a должен был быть выбран первым (priority=100) и провалиться")
	}
	if callsB.Load() != 1 {
		t.Errorf("b должен быть выбран 1 раз после failover, получено %d", callsB.Load())
	}
}

// TestScheduler_Deferred_ErrNoServerOnNoCompatible: модели нет ни на одном
// сервере → Submit возвращает ErrNoServer синхронно, Job в pool не попадает
// (см. Q3).
func TestScheduler_Deferred_ErrNoServerOnNoCompatible(t *testing.T) {
	a := mkSrv(t, "a", 100, true, "", "mX")
	b := mkSrv(t, "b", 200, true, "", "mY")
	s, cancel := makeDeferredSched(t, []*ServerInfo{a, b}, RetryConfig{MaxAttempts: 1})
	defer cancel()

	_, err := s.Submit(context.Background(), &Job{
		ID: "j", Model: "mZ",
		Run: func(context.Context, *ServerInfo) JobResult { return JobResult{HTTPStatus: 200} },
	})
	var noSrv *ErrNoServer
	if !errors.As(err, &noSrv) {
		t.Errorf("ожидалась *ErrNoServer, получено %v", err)
	}
	if s.pool.Len() != 0 {
		t.Errorf("при ErrNoServer Job не должен попасть в pool, осталось %d", s.pool.Len())
	}
}

// TestScheduler_Deferred_RollingExclusion: после ошибки на сервере следующая
// попытка идёт на другой совместимый — даже если этот другой раньше тоже падал.
// Проверяем поведение rolling exclusion size 1: исключение действует только
// на одну следующую попытку, не накапливается.
func TestScheduler_Deferred_RollingExclusion(t *testing.T) {
	a := mkSrv(t, "a", 100, true, "", "m1")
	b := mkSrv(t, "b", 200, true, "", "m1")
	s, cancel := makeDeferredSched(t, []*ServerInfo{a, b},
		RetryConfig{MaxAttempts: 4, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond})
	defer cancel()

	var mu sync.Mutex
	visited := []string{}
	var attempts atomic.Int32
	run := func(_ context.Context, srv *ServerInfo) JobResult {
		mu.Lock()
		visited = append(visited, srv.Name)
		mu.Unlock()
		n := attempts.Add(1)
		// Первые 3 попытки падают, 4-я — успех.
		if n < 4 {
			return JobResult{Err: fmt.Errorf("транзитная ошибка #%d", n)}
		}
		return JobResult{HTTPStatus: 200, Server: srv.Name}
	}

	res, err := s.Submit(context.Background(), &Job{ID: "j", Model: "m1", Run: run})
	if err != nil {
		t.Fatalf("ожидался успех на 4-й попытке, получено %v", err)
	}
	if res.HTTPStatus != 200 {
		t.Errorf("HTTPStatus = %d, want 200", res.HTTPStatus)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(visited) != 4 {
		t.Fatalf("ожидалось 4 попытки, получено %d (%v)", len(visited), visited)
	}
	// Главное свойство: никаких двух подряд попыток на одном сервере.
	for i := 1; i < len(visited); i++ {
		if visited[i] == visited[i-1] {
			t.Errorf("две подряд попытки на %s (rolling exclusion нарушен): %v",
				visited[i], visited)
		}
	}
	// Также: каждый сервер должен был использоваться хотя бы раз
	// (4 попытки чередуются между 2 серверами).
	counts := map[string]int{}
	for _, n := range visited {
		counts[n]++
	}
	if counts["a"] == 0 || counts["b"] == 0 {
		t.Errorf("оба сервера должны были использоваться, получено %v", counts)
	}
}

// TestScheduler_Deferred_SingleServerRetries: единственный совместимый сервер
// в pool-режиме. Rolling exclusion не должен зависеть от количества серверов —
// при отсутствии альтернатив submitDeferred делает ClearVisited и ретраит на
// том же. Это критично для single-server setup'а.
func TestScheduler_Deferred_SingleServerRetries(t *testing.T) {
	a := mkSrv(t, "a", 100, true, "", "m1")
	s, cancel := makeDeferredSched(t, []*ServerInfo{a},
		RetryConfig{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond})
	defer cancel()

	var attempts atomic.Int32
	run := func(_ context.Context, srv *ServerInfo) JobResult {
		if srv.Name != "a" {
			t.Errorf("в pool single-server case ретрай ушёл не на a: %s", srv.Name)
		}
		if attempts.Add(1) < 3 {
			return JobResult{Err: fmt.Errorf("транзитная ошибка")}
		}
		return JobResult{HTTPStatus: 200}
	}

	if _, err := s.Submit(context.Background(), &Job{ID: "j", Model: "m1", Run: run}); err != nil {
		t.Fatalf("Submit err: %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("ожидалось 3 попытки, получено %d", got)
	}
}

// TestScheduler_Deferred_INV1_NoParallelDispatch: проверяем INV-1 в pull-режиме.
// На одном сервере одновременно может быть только один Job в Run'е.
func TestScheduler_Deferred_INV1_NoParallelDispatch(t *testing.T) {
	srv := mkSrv(t, "a", 100, true, "", "m1")
	s, cancel := makeDeferredSched(t, []*ServerInfo{srv}, RetryConfig{MaxAttempts: 1})
	defer cancel()

	var concurrent atomic.Int32
	var maxC atomic.Int32
	run := func(context.Context, *ServerInfo) JobResult {
		c := concurrent.Add(1)
		for {
			m := maxC.Load()
			if c <= m || maxC.CompareAndSwap(m, c) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		concurrent.Add(-1)
		return JobResult{HTTPStatus: 200}
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			_, _ = s.Submit(context.Background(), &Job{ID: fmt.Sprintf("j%d", i), Model: "m1", Run: run})
		}()
	}
	wg.Wait()
	if got := maxC.Load(); got > 1 {
		t.Errorf("INV-1 нарушен в pull-режиме: max concurrent = %d", got)
	}
}

// --- Unit-тесты PopForCoverage (стратегия preserve_model_coverage) ----------

// TestPopForCoverage_HeadEqualsCurrent: HEAD очереди == current_model → берём HEAD.
func TestPopForCoverage_HeadEqualsCurrent(t *testing.T) {
	p := NewJobPool()
	a := mkSrv(t, "a", 100, true, "m1", "m1", "m2")
	all := []*ServerInfo{a}

	p.Push(&Job{ID: "j1", Model: "m1"})
	p.Push(&Job{ID: "j2", Model: "m2"})

	got := p.PopForCoverage(a, all)
	if got == nil || got.ID != "j1" {
		t.Errorf("ожидался j1, получен %v", got)
	}
}

// TestPopForCoverage_CurrentEmpty: current_model = "" → свободно берём HEAD.
func TestPopForCoverage_CurrentEmpty(t *testing.T) {
	p := NewJobPool()
	a := mkSrv(t, "a", 100, true, "", "m1", "m2")
	all := []*ServerInfo{a}

	p.Push(&Job{ID: "j1", Model: "m2"})
	p.Push(&Job{ID: "j2", Model: "m1"})

	got := p.PopForCoverage(a, all)
	if got == nil || got.ID != "j1" {
		t.Errorf("при пустом current ожидался HEAD (j1), получен %v", got)
	}
}

// TestPopForCoverage_OtherHasCurrent_TakesHead: HEAD на другую модель, но у
// другого сервера загружена current — swap безопасен, берём HEAD.
func TestPopForCoverage_OtherHasCurrent_TakesHead(t *testing.T) {
	p := NewJobPool()
	a := mkSrv(t, "a", 100, true, "m1", "m1", "m2")
	b := mkSrv(t, "b", 200, true, "m1", "m1") // b тоже держит m1
	all := []*ServerInfo{a, b}

	p.Push(&Job{ID: "j1", Model: "m2"}) // HEAD на другую модель
	p.Push(&Job{ID: "j2", Model: "m1"})

	got := p.PopForCoverage(a, all)
	if got == nil || got.ID != "j1" {
		t.Errorf("у b есть m1 → swap безопасен, ожидался j1 (HEAD), получен %v", got)
	}
}

// TestPopForCoverage_UniqueHolder_PrefersCurrent: a — единственный держатель m1,
// в очереди есть job под m1 ниже HEAD — должен взять его, чтобы сохранить покрытие.
func TestPopForCoverage_UniqueHolder_PrefersCurrent(t *testing.T) {
	p := NewJobPool()
	a := mkSrv(t, "a", 100, true, "m1", "m1", "m2")
	b := mkSrv(t, "b", 200, true, "mX", "mX") // b держит другую модель
	all := []*ServerInfo{a, b}

	p.Push(&Job{ID: "j1", Model: "m2"}) // HEAD
	p.Push(&Job{ID: "j2", Model: "m1"}) // current — должен быть выбран

	got := p.PopForCoverage(a, all)
	if got == nil || got.ID != "j2" {
		t.Errorf("a — единственный с m1, в очереди есть m1 → ожидался j2, получен %v", got)
	}
}

// TestPopForCoverage_UniqueHolder_NoCurrentJob_TakesHead: a — единственный
// держатель m1, но job'ов под m1 в очереди нет — swap неизбежен, берём HEAD.
func TestPopForCoverage_UniqueHolder_NoCurrentJob_TakesHead(t *testing.T) {
	p := NewJobPool()
	a := mkSrv(t, "a", 100, true, "m1", "m1", "m2")
	b := mkSrv(t, "b", 200, true, "mX", "mX")
	all := []*ServerInfo{a, b}

	p.Push(&Job{ID: "j1", Model: "m2"})
	p.Push(&Job{ID: "j2", Model: "m2"}) // ни одного под m1

	got := p.PopForCoverage(a, all)
	if got == nil || got.ID != "j1" {
		t.Errorf("под current job'ов нет → swap неизбежен, ожидался j1, получен %v", got)
	}
}

// TestPopForCoverage_RespectsVisited: visited[srv]=true пропускается, не блокирует.
func TestPopForCoverage_RespectsVisited(t *testing.T) {
	p := NewJobPool()
	a := mkSrv(t, "a", 100, true, "m1", "m1", "m2")
	all := []*ServerInfo{a}

	j1 := &Job{ID: "j1", Model: "m1"}
	j2 := &Job{ID: "j2", Model: "m1"}
	p.Push(j1)
	p.Push(j2)
	p.MarkVisited(j1, "a")

	got := p.PopForCoverage(a, all)
	if got == nil || got.ID != "j2" {
		t.Errorf("visited пропущен → ожидался j2, получен %v", got)
	}
}

// --- E2E-тесты Scheduler в режиме preserve_model_coverage -------------------

func makeCoverageSched(t *testing.T, servers []*ServerInfo, retry RetryConfig) (*Scheduler, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := NewRouter(servers, RouterPreserveModelCoverage)
	s := NewScheduler(servers, r, retry, nil)
	if s.pool == nil || !s.coverageAware {
		t.Fatal("ожидался pool + coverageAware=true в режиме preserve_model_coverage")
	}
	s.Start(ctx)
	return s, cancel
}

// TestScheduler_Coverage_PreservesUniqueModel: a — единственный с m1. Пока a
// занят первой задачей (m1), в pool кладутся j_m2 затем j_m1. После освобождения
// a должен взять j_m1 (coverage) до j_m2 — несмотря на FIFO-HEAD m2 в pool.
//
// Hold-паттерн фиксирует порядок Push'а в pool: оба ожидающих Submit'а попадают
// в pool пока a крутит holder, и k моменту PopForCoverage current_model ещё m1.
func TestScheduler_Coverage_PreservesUniqueModel(t *testing.T) {
	a := mkSrv(t, "a", 100, true, "m1", "m1", "m2")
	b := mkSrv(t, "b", 200, true, "mX", "mX")
	s, cancel := makeCoverageSched(t, []*ServerInfo{a, b}, RetryConfig{MaxAttempts: 1})
	defer cancel()

	var orderMu sync.Mutex
	order := []string{}
	record := func(id string) {
		orderMu.Lock()
		order = append(order, id)
		orderMu.Unlock()
	}

	holdStarted := make(chan struct{})
	holdRelease := make(chan struct{})
	holdRun := func(context.Context, *ServerInfo) JobResult {
		record("hold")
		close(holdStarted)
		<-holdRelease
		return JobResult{HTTPStatus: 200}
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = s.Submit(context.Background(), &Job{ID: "hold", Model: "m1", Run: holdRun})
	}()
	<-holdStarted // a держит m1

	// Push'аем j_m2 и j_m1 в pool. Submit блокирует до результата — запускаем в горутинах.
	// Чтобы гарантировать, что j_m2 попал в pool раньше j_m1, используем pool.Len()-based
	// синхронизатор: ждём, пока first Submit действительно положил Job (через busy-poll —
	// безопасно, потому что без time.Sleep, дёргаем атомарный счётчик).
	mkRun := func(id string) func(context.Context, *ServerInfo) JobResult {
		return func(context.Context, *ServerInfo) JobResult {
			record(id)
			return JobResult{HTTPStatus: 200}
		}
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = s.Submit(context.Background(), &Job{ID: "j_m2", Model: "m2", Run: mkRun("j_m2")})
	}()
	// busy-wait до появления j_m2 в pool (без sleep — CAS на pool.Len).
	for {
		if s.pool.Len() >= 1 {
			break
		}
		runtime.Gosched()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = s.Submit(context.Background(), &Job{ID: "j_m1", Model: "m1", Run: mkRun("j_m1")})
	}()
	for {
		if s.pool.Len() >= 2 {
			break
		}
		runtime.Gosched()
	}

	// Теперь pool=[j_m2, j_m1], a держит m1, b не имеет m1. Отпускаем hold.
	close(holdRelease)
	wg.Wait()

	orderMu.Lock()
	defer orderMu.Unlock()
	if len(order) != 3 {
		t.Fatalf("ожидалось 3 записи, получено %v", order)
	}
	// hold всегда первый; coverage-стратегия требует j_m1 до j_m2.
	if order[0] != "hold" || order[1] != "j_m1" || order[2] != "j_m2" {
		t.Errorf("ожидался порядок [hold j_m1 j_m2], получен %v", order)
	}
}

// TestScheduler_Coverage_OtherHolderAllowsSwap: оба сервера держат m1. В очередь
// одновременно прилетает [m2, m1]. Хотя бы на одном из них swap на m2 случится
// без задержек — потому что покрытие m1 сохраняется на втором.
func TestScheduler_Coverage_OtherHolderAllowsSwap(t *testing.T) {
	a := mkSrv(t, "a", 100, true, "m1", "m1", "m2")
	b := mkSrv(t, "b", 200, true, "m1", "m1", "m2")
	s, cancel := makeCoverageSched(t, []*ServerInfo{a, b}, RetryConfig{MaxAttempts: 1})
	defer cancel()

	run := func(_ context.Context, srv *ServerInfo) JobResult {
		return JobResult{HTTPStatus: 200, Server: srv.Name}
	}
	resM2Ch := make(chan JobResult, 1)
	go func() {
		res, _ := s.Submit(context.Background(), &Job{ID: "j_m2", Model: "m2", Run: run})
		resM2Ch <- res
	}()
	select {
	case res := <-resM2Ch:
		if res.Err != nil {
			t.Fatalf("ошибка: %v", res.Err)
		}
		// swap прошёл — проверка, что один из серверов взял m2 без задержки.
	case <-time.After(2 * time.Second):
		t.Fatal("m2 не подхватили — coverage-стратегия зря удерживает m1 на обоих")
	}
}

// --- стабильный sort helper (используется не в продакшене) ------------------
var _ = sort.Sort
