package core

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestScheduler_INV1_NoParallelDispatch: на одном сервере не может быть >1 in-flight Job
// одновременно. Запускаем 10 Job, в Run атомиком считаем concurrent count;
// max не должен превысить 1.
func TestScheduler_INV1_NoParallelDispatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := mkSrv(t, "a", 100, true, "", "m1")
	r := NewRouter([]*ServerInfo{srv}, RouterModelAffinityLeastBusy)
	s := NewScheduler([]*ServerInfo{srv}, r, RetryConfig{MaxAttempts: 1}, nil)
	s.Start(ctx)

	var concurrent atomic.Int32
	var maxConcurrent atomic.Int32

	run := func(_ context.Context, _ *ServerInfo) JobResult {
		c := concurrent.Add(1)
		// обновим max неблокирующим CAS
		for {
			m := maxConcurrent.Load()
			if c <= m || maxConcurrent.CompareAndSwap(m, c) {
				break
			}
		}
		// небольшая задержка чтобы overlap был возможен при баге
		time.Sleep(5 * time.Millisecond)
		concurrent.Add(-1)
		return JobResult{HTTPStatus: 200}
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			_, err := s.Submit(ctx, &Job{ID: fmt.Sprintf("j%d", i), Model: "m1", Run: run})
			if err != nil {
				t.Errorf("Submit err: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := maxConcurrent.Load(); got > 1 {
		t.Errorf("INV-1 нарушен: max concurrent dispatch = %d, ожидалось ≤ 1", got)
	}
}

// TestScheduler_INV2_DrainBeforeSwitch: пока в очереди есть Job для CurrentModel,
// другая модель не должна быть выбрана.
func TestScheduler_INV2_DrainBeforeSwitch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := mkSrv(t, "a", 100, true, "m1", "m1", "m2") // сервер сейчас на m1
	r := NewRouter([]*ServerInfo{srv}, RouterModelAffinityLeastBusy)
	s := NewScheduler([]*ServerInfo{srv}, r, RetryConfig{MaxAttempts: 1}, nil)

	// Подложим в очередь несколько Job для m1 и m2 ДО старта worker'а,
	// чтобы worker сначала увидел всё сразу.
	released := make(chan struct{})
	completionOrder := make([]string, 0, 4)
	var orderMu sync.Mutex

	mkRun := func(jid string, model string) func(context.Context, *ServerInfo) JobResult {
		return func(context.Context, *ServerInfo) JobResult {
			<-released // ждём общий старт
			orderMu.Lock()
			completionOrder = append(completionOrder, jid+"/"+model)
			orderMu.Unlock()
			return JobResult{HTTPStatus: 200}
		}
	}

	jobs := []*Job{
		{ID: "j1", Model: "m1", Run: mkRun("j1", "m1"), done: make(chan JobResult, 1)},
		{ID: "j2", Model: "m2", Run: mkRun("j2", "m2"), done: make(chan JobResult, 1)},
		{ID: "j3", Model: "m1", Run: mkRun("j3", "m1"), done: make(chan JobResult, 1)},
	}
	srv.Lock()
	srv.Queue = append(srv.Queue, jobs...)
	srv.Unlock()

	s.Start(ctx)
	srv.Wake()
	close(released)

	// Дождёмся всех done
	for _, j := range jobs {
		select {
		case <-j.done:
		case <-time.After(2 * time.Second):
			t.Fatal("timeout: scheduler не завершил Job")
		}
	}

	orderMu.Lock()
	defer orderMu.Unlock()
	// Ожидаемый порядок: j1 (m1), j3 (m1), j2 (m2) — m1 дренируется первым.
	want := []string{"j1/m1", "j3/m1", "j2/m2"}
	if len(completionOrder) != 3 {
		t.Fatalf("ожидалось 3 Job, получено %d: %v", len(completionOrder), completionOrder)
	}
	for i, w := range want {
		if completionOrder[i] != w {
			t.Errorf("INV-2 нарушен: completion[%d] = %s, ожидалось %s; full=%v",
				i, completionOrder[i], w, completionOrder)
		}
	}
}

// TestScheduler_INV3_FIFOWithinModel: для одного и того же model порядок прихода
// сохраняется в порядке завершения.
func TestScheduler_INV3_FIFOWithinModel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := mkSrv(t, "a", 100, true, "", "m1")
	r := NewRouter([]*ServerInfo{srv}, RouterModelAffinityLeastBusy)
	s := NewScheduler([]*ServerInfo{srv}, r, RetryConfig{MaxAttempts: 1}, nil)

	var orderMu sync.Mutex
	order := make([]string, 0, 5)

	gate := make(chan struct{})
	mkRun := func(id string) func(context.Context, *ServerInfo) JobResult {
		return func(context.Context, *ServerInfo) JobResult {
			<-gate
			orderMu.Lock()
			order = append(order, id)
			orderMu.Unlock()
			return JobResult{HTTPStatus: 200}
		}
	}
	jobs := []*Job{
		{ID: "1", Model: "m1", Run: mkRun("1"), done: make(chan JobResult, 1)},
		{ID: "2", Model: "m1", Run: mkRun("2"), done: make(chan JobResult, 1)},
		{ID: "3", Model: "m1", Run: mkRun("3"), done: make(chan JobResult, 1)},
	}
	srv.Lock()
	srv.Queue = append(srv.Queue, jobs...)
	srv.Unlock()

	s.Start(ctx)
	srv.Wake()
	close(gate)

	for _, j := range jobs {
		select {
		case <-j.done:
		case <-time.After(2 * time.Second):
			t.Fatal("timeout")
		}
	}
	orderMu.Lock()
	defer orderMu.Unlock()
	want := []string{"1", "2", "3"}
	for i, w := range want {
		if order[i] != w {
			t.Errorf("FIFO нарушен: order[%d] = %s, want %s; full=%v", i, order[i], w, order)
		}
	}
}

// TestScheduler_RetryOnError: ошибка → retry до MaxAttempts → успех
func TestScheduler_RetryOnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := mkSrv(t, "a", 100, true, "", "m1")
	r := NewRouter([]*ServerInfo{srv}, RouterModelAffinityLeastBusy)
	s := NewScheduler([]*ServerInfo{srv}, r,
		RetryConfig{MaxAttempts: 3, InitialBackoff: 1 * time.Millisecond, MaxBackoff: 1 * time.Millisecond}, nil)
	s.Start(ctx)

	var calls atomic.Int32
	run := func(context.Context, *ServerInfo) JobResult {
		n := calls.Add(1)
		if n < 3 {
			return JobResult{Err: fmt.Errorf("транзитная ошибка #%d", n)}
		}
		return JobResult{HTTPStatus: 200}
	}
	res, err := s.Submit(ctx, &Job{ID: "j", Model: "m1", Run: run})
	if err != nil {
		t.Fatalf("ожидался успех после retry, получено %v", err)
	}
	if res.HTTPStatus != 200 {
		t.Errorf("HTTPStatus = %d, want 200", res.HTTPStatus)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("ожидалось 3 вызова, получено %d", got)
	}
}

// TestScheduler_RunPanicDoesNotDeadlock: panic в Job.Run должна превращаться
// в JobResult{Err: ...}, иначе Submit зависнет навечно (worker умер, j.done пуст).
func TestScheduler_RunPanicDoesNotDeadlock(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := mkSrv(t, "a", 100, true, "", "m1")
	r := NewRouter([]*ServerInfo{srv}, RouterModelAffinityLeastBusy)
	s := NewScheduler([]*ServerInfo{srv}, r,
		RetryConfig{MaxAttempts: 1, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}, nil)
	s.Start(ctx)

	run := func(context.Context, *ServerInfo) JobResult {
		panic("nil pointer в handler'е")
	}

	// Submit должен вернуть управление в пределах разумного времени.
	done := make(chan struct{})
	var submitErr error
	go func() {
		_, submitErr = s.Submit(ctx, &Job{ID: "j", Model: "m1", Run: run})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Submit завис → panic-recovery не сработал")
	}
	if submitErr == nil {
		t.Fatal("ожидалась ошибка после panic")
	}
	if !strings.Contains(submitErr.Error(), "panic") {
		t.Errorf("ошибка не упоминает panic: %v", submitErr)
	}

	// После panic worker должен быть жив: следующий Submit на тот же сервер проходит.
	res, err := s.Submit(ctx, &Job{
		ID: "j2", Model: "m1",
		Run: func(context.Context, *ServerInfo) JobResult { return JobResult{HTTPStatus: 200} },
	})
	if err != nil {
		t.Fatalf("worker умер после panic: %v", err)
	}
	if res.HTTPStatus != 200 {
		t.Errorf("HTTPStatus = %d, want 200", res.HTTPStatus)
	}
}

// TestScheduler_Push_RollingExclusion: проверяет новую модель retry (v1.0+):
// после ошибки на сервере следующая попытка стремится уйти на ДРУГОЙ сервер,
// исключение действует только на 1 следующую попытку.
//
// Сценарий: 3 сервера a/b/c, попытки 1,2 падают, попытка 3 — успех. Ожидаем,
// что попытки распределились НЕ подряд по одному серверу: между двумя
// подряд попытками сервер обязан отличаться.
func TestScheduler_Push_RollingExclusion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := mkSrv(t, "a", 100, true, "", "m1")
	b := mkSrv(t, "b", 200, true, "", "m1")
	c := mkSrv(t, "c", 300, true, "", "m1")
	r := NewRouter([]*ServerInfo{a, b, c}, RouterModelAffinityLeastBusy)
	s := NewScheduler([]*ServerInfo{a, b, c}, r,
		RetryConfig{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}, nil)
	s.Start(ctx)

	var mu sync.Mutex
	visited := []string{}
	var calls atomic.Int32
	run := func(_ context.Context, srv *ServerInfo) JobResult {
		mu.Lock()
		visited = append(visited, srv.Name)
		mu.Unlock()
		if calls.Add(1) < 3 {
			return JobResult{Err: fmt.Errorf("транзитная ошибка")}
		}
		return JobResult{HTTPStatus: 200}
	}
	if _, err := s.Submit(ctx, &Job{ID: "j", Model: "m1", Run: run}); err != nil {
		t.Fatalf("Submit err: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(visited) != 3 {
		t.Fatalf("ожидалось 3 попытки, получено %d (%v)", len(visited), visited)
	}
	for i := 1; i < len(visited); i++ {
		if visited[i] == visited[i-1] {
			t.Errorf("две подряд попытки на одном сервере %s (rolling exclusion нарушен): %v",
				visited[i], visited)
		}
	}
}

// TestScheduler_Push_SingleServerRetries: единственный совместимый сервер →
// после ошибки rolling exclusion очищается, ретраи идут на ТОМ ЖЕ сервере
// (деградация при отсутствии альтернатив, чтобы single-server setup работал).
func TestScheduler_Push_SingleServerRetries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := mkSrv(t, "a", 100, true, "", "m1")
	r := NewRouter([]*ServerInfo{a}, RouterModelAffinityLeastBusy)
	s := NewScheduler([]*ServerInfo{a}, r,
		RetryConfig{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}, nil)
	s.Start(ctx)

	var calls atomic.Int32
	run := func(_ context.Context, srv *ServerInfo) JobResult {
		if srv.Name != "a" {
			t.Errorf("ретрай ушёл не на a: %s", srv.Name)
		}
		if calls.Add(1) < 3 {
			return JobResult{Err: fmt.Errorf("транзитная ошибка")}
		}
		return JobResult{HTTPStatus: 200}
	}
	if _, err := s.Submit(ctx, &Job{ID: "j", Model: "m1", Run: run}); err != nil {
		t.Fatalf("Submit err: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("ожидалось 3 попытки, получено %d", got)
	}
}

// TestScheduler_INV6_NoRetryAfterResponseStarted: после первого чанка клиенту
// ретрай запрещён, даже если Run вернул err.
func TestScheduler_INV6_NoRetryAfterResponseStarted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := mkSrv(t, "a", 100, true, "", "m1")
	r := NewRouter([]*ServerInfo{srv}, RouterModelAffinityLeastBusy)
	s := NewScheduler([]*ServerInfo{srv}, r,
		RetryConfig{MaxAttempts: 5, InitialBackoff: 1 * time.Millisecond, MaxBackoff: 1 * time.Millisecond}, nil)
	s.Start(ctx)

	var calls atomic.Int32
	run := func(context.Context, *ServerInfo) JobResult {
		calls.Add(1)
		return JobResult{
			Err:             fmt.Errorf("сетевой обрыв в середине стрима"),
			ResponseStarted: true,
		}
	}
	_, err := s.Submit(ctx, &Job{ID: "j", Model: "m1", Run: run})
	if err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("INV-6 нарушен: Run вызван %d раз, должен быть 1", got)
	}
}
