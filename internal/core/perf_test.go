package core

import (
	"math"
	"sync"
	"testing"
)

// TestPerfTracker_EmptySnapshot: пустой tracker — Samples=0, OK=false.
func TestPerfTracker_EmptySnapshot(t *testing.T) {
	p := NewPerfTracker()
	st := p.Snapshot("a", "m1")
	if st.Samples != 0 || st.OK {
		t.Errorf("пустой tracker: samples=%d ok=%v", st.Samples, st.OK)
	}
}

// TestPerfTracker_NilSafe: nil-приёмник не паникует.
func TestPerfTracker_NilSafe(t *testing.T) {
	var p *PerfTracker
	p.Record("a", "m1", 50, 100, 1000, false)
	st := p.Snapshot("a", "m1")
	if st.Samples != 0 || st.OK {
		t.Errorf("nil tracker должен возвращать нули, получено %+v", st)
	}
}

// TestPerfTracker_BelowMinSamples: <perfMinSamples — OK=false.
func TestPerfTracker_BelowMinSamples(t *testing.T) {
	p := NewPerfTracker()
	p.Record("a", "m1", 10, 20, 1000, false)
	p.Record("a", "m1", 10, 20, 1000, false)
	st := p.Snapshot("a", "m1")
	if st.OK {
		t.Errorf("при samples<%d ok должен быть false, получено %+v", perfMinSamples, st)
	}
	if st.Samples != 2 {
		t.Errorf("samples = %d, want 2", st.Samples)
	}
}

// TestPerfTracker_FitsKnownCoefficients: подставляем данные, сгенерированные по
// формуле t = 500 + 10*in + 20*out, точно восстанавливаем коэффициенты.
// Берём 4 точки, одна с loaded=true — все 3 параметра определены.
func TestPerfTracker_FitsKnownCoefficients(t *testing.T) {
	p := NewPerfTracker()
	mk := func(in, out int, loaded bool) int64 {
		tLoad := int64(0)
		if loaded {
			tLoad = 500
		}
		return tLoad + int64(10*in+20*out)
	}
	cases := []struct {
		in, out int
		loaded  bool
	}{
		{100, 50, true},
		{200, 100, false},
		{50, 200, false},
		{300, 30, false},
	}
	for _, c := range cases {
		p.Record("a", "m1", c.in, c.out, mk(c.in, c.out, c.loaded), c.loaded)
	}
	st := p.Snapshot("a", "m1")
	if !st.OK {
		t.Fatalf("регрессия не fit: %+v", st)
	}
	approx := func(got, want float64) bool {
		return math.Abs(got-want) < 0.01
	}
	if !approx(st.TLoadMs, 500) {
		t.Errorf("t_load = %v, want ~500", st.TLoadMs)
	}
	if !approx(st.KInMsTok, 10) {
		t.Errorf("k_in = %v, want ~10", st.KInMsTok)
	}
	if !approx(st.KOutMsTok, 20) {
		t.Errorf("k_out = %v, want ~20", st.KOutMsTok)
	}
}

// TestPerfTracker_NoLoadedFallsBackTo2Vars: все loaded=false → решаем 2×2,
// TLoadMs=0, OK=true.
func TestPerfTracker_NoLoadedFallsBackTo2Vars(t *testing.T) {
	p := NewPerfTracker()
	// t = 5*in + 8*out (нет t_load).
	gen := func(in, out int) int64 { return int64(5*in + 8*out) }
	p.Record("a", "m1", 100, 50, gen(100, 50), false)
	p.Record("a", "m1", 200, 100, gen(200, 100), false)
	p.Record("a", "m1", 50, 200, gen(50, 200), false)
	st := p.Snapshot("a", "m1")
	if !st.OK {
		t.Fatalf("регрессия не fit при loaded=0: %+v", st)
	}
	if st.TLoadMs != 0 {
		t.Errorf("при loaded=0 t_load должен быть 0, получено %v", st.TLoadMs)
	}
	approx := func(got, want float64) bool { return math.Abs(got-want) < 0.01 }
	if !approx(st.KInMsTok, 5) {
		t.Errorf("k_in = %v, want ~5", st.KInMsTok)
	}
	if !approx(st.KOutMsTok, 8) {
		t.Errorf("k_out = %v, want ~8", st.KOutMsTok)
	}
}

// TestPerfTracker_PerKey: разные (server, model) — независимые наборы.
// Точки должны быть линейно независимыми, иначе X^TX вырождается и OK=false.
func TestPerfTracker_PerKey(t *testing.T) {
	p := NewPerfTracker()
	p.Record("a", "m1", 100, 50, 1000, false)
	p.Record("a", "m1", 200, 100, 2000, false)
	p.Record("a", "m1", 50, 200, 1500, false)
	p.Record("a", "m2", 100, 50, 2000, false)
	p.Record("a", "m2", 200, 100, 4000, false)
	p.Record("a", "m2", 50, 200, 3000, false)
	p.Record("b", "m1", 100, 50, 500, false)
	p.Record("b", "m1", 200, 100, 1000, false)
	p.Record("b", "m1", 50, 200, 750, false)

	if st := p.Snapshot("a", "m1"); !st.OK || st.Samples != 3 {
		t.Errorf("a/m1 samples=%d ok=%v", st.Samples, st.OK)
	}
	if st := p.Snapshot("a", "m2"); st.Samples != 3 {
		t.Errorf("a/m2 samples = %d", st.Samples)
	}
	if st := p.Snapshot("b", "m1"); st.Samples != 3 {
		t.Errorf("b/m1 samples = %d", st.Samples)
	}
	if st := p.Snapshot("c", "mX"); st.Samples != 0 {
		t.Errorf("несуществующий ключ: samples = %d", st.Samples)
	}
}

// TestPerfTracker_IgnoresNoise: totalMs<=0 или out<=0 — точка игнорируется.
func TestPerfTracker_IgnoresNoise(t *testing.T) {
	p := NewPerfTracker()
	p.Record("a", "m1", 100, 0, 1000, false)
	p.Record("a", "m1", 100, 50, 0, false)
	p.Record("a", "m1", 100, 50, 1000, false)
	st := p.Snapshot("a", "m1")
	if st.Samples != 1 {
		t.Errorf("samples = %d, ожидалось 1 (мусорные точки отброшены)", st.Samples)
	}
}

// TestPerfTracker_Concurrent: одновременные Record/Snapshot не паникуют.
func TestPerfTracker_Concurrent(t *testing.T) {
	p := NewPerfTracker()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.Record("a", "m1", 100, 50, 1000, false)
			_ = p.Snapshot("a", "m1")
		}()
	}
	wg.Wait()
	st := p.Snapshot("a", "m1")
	if st.Samples == 0 {
		t.Errorf("concurrent: samples = 0")
	}
}

// TestPerfTracker_ServerSummary: возвращает модели сервера, отсортированные
// по числу samples DESC.
func TestPerfTracker_ServerSummary(t *testing.T) {
	p := NewPerfTracker()
	for range 5 {
		p.Record("a", "popular", 100, 50, 1000, false)
	}
	for range 2 {
		p.Record("a", "rare", 100, 50, 1000, false)
	}
	p.Record("b", "other", 100, 50, 1000, false)

	sum := p.ServerSummary("a")
	if len(sum) != 2 {
		t.Fatalf("ServerSummary(a): want 2 models, got %d", len(sum))
	}
	if sum[0].Model != "popular" || sum[0].Stats.Samples != 5 {
		t.Errorf("[0]: %+v, want popular/5", sum[0])
	}
	if sum[1].Model != "rare" || sum[1].Stats.Samples != 2 {
		t.Errorf("[1]: %+v, want rare/2", sum[1])
	}
}
