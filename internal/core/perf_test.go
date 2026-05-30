package core

import (
	"math"
	"sync"
	"testing"
)

// TestPerfTracker_EmptySnapshot: пустой tracker — Samples=0, OK=false.
func TestPerfTracker_EmptySnapshot(t *testing.T) {
	p := NewPerfTracker()
	st := p.Snapshot("a", "m1", "/v1/chat/completions")
	if st.Samples != 0 || st.OK {
		t.Errorf("пустой tracker: samples=%d ok=%v", st.Samples, st.OK)
	}
}

// TestPerfTracker_NilSafe: nil-приёмник не паникует.
func TestPerfTracker_NilSafe(t *testing.T) {
	var p *PerfTracker
	p.Record("a", "m1", "/v1/chat/completions", 50, 100, 1000, false)
	st := p.Snapshot("a", "m1", "/v1/chat/completions")
	if st.Samples != 0 || st.OK {
		t.Errorf("nil tracker должен возвращать нули, получено %+v", st)
	}
}

// TestPerfTracker_BelowMinSamples: <perfMinSamples — OK=false.
func TestPerfTracker_BelowMinSamples(t *testing.T) {
	p := NewPerfTracker()
	p.Record("a", "m1", "/v1/chat/completions", 10, 20, 1000, false)
	p.Record("a", "m1", "/v1/chat/completions", 10, 20, 1000, false)
	st := p.Snapshot("a", "m1", "/v1/chat/completions")
	if st.OK {
		t.Errorf("при samples<%d ok должен быть false, получено %+v", perfMinSamples, st)
	}
	if st.Samples != 2 {
		t.Errorf("samples = %d, want 2", st.Samples)
	}
}

// TestPerfTracker_FitsKnownCoefficients: подставляем данные, сгенерированные по
// формуле t = 500 + 10*in + 20*out, и проверяем восстановление коэффициентов.
// Ridge регуляризация даёт небольшое смещение к нулю; на этих числах оно
// порядка 0.001 — допуск выбран с запасом.
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
		p.Record("a", "m1", "/v1/chat/completions", c.in, c.out, mk(c.in, c.out, c.loaded), c.loaded)
	}
	st := p.Snapshot("a", "m1", "/v1/chat/completions")
	if !st.OK {
		t.Fatalf("регрессия не fit: %+v", st)
	}
	approx := func(got, want, tol float64) bool {
		return math.Abs(got-want) < tol
	}
	// Допуск для t_load — 1.0: ridge λ=1e-4 на матрице с диагональю >100000
	// даёт смещение порядка 1e-4 для k_in/k_out и нескольких десятых для
	// t_load (где знаменатель меньше).
	if !approx(st.TLoadMs, 500, 5.0) {
		t.Errorf("t_load = %v, want ~500 (±5)", st.TLoadMs)
	}
	if !approx(st.KInMsTok, 10, 0.1) {
		t.Errorf("k_in = %v, want ~10 (±0.1)", st.KInMsTok)
	}
	if !approx(st.KOutMsTok, 20, 0.1) {
		t.Errorf("k_out = %v, want ~20 (±0.1)", st.KOutMsTok)
	}
	// R² должен быть очень близок к 1 — данные синтетические и без шума.
	if st.RSquared < 0.99 {
		t.Errorf("R² = %v, want ≥0.99 для идеальных данных", st.RSquared)
	}
	if st.FitQuality != "good" {
		t.Errorf("FitQuality = %q, want 'good'", st.FitQuality)
	}
}

// TestPerfTracker_TwoStageTLoadFromSingleReload: реалистичный профиль —
// много «чистых» наблюдений loaded=0 и ОДНО loaded=1. Двухэтапная оценка
// (v0.12.0) должна устойчиво восстановить t_load из единственного reload'а:
// токенные коэффициенты берутся по чистым точкам, t_load — как остаток
// загруженной точки. Раньше совместный 3-var NNLS по одному остатку часто
// «прибивал» t_load к 0 → в TUI был «—», хотя load-время физически есть.
func TestPerfTracker_TwoStageTLoadFromSingleReload(t *testing.T) {
	p := NewPerfTracker()
	// Чистая токенная формула: t = 8*in + 12*out. Load добавляет 700 мс.
	p.Record("a", "m1", "/v1/chat/completions", 100, 50, 8*100+12*50, false)
	p.Record("a", "m1", "/v1/chat/completions", 200, 100, 8*200+12*100, false)
	p.Record("a", "m1", "/v1/chat/completions", 50, 150, 8*50+12*150, false)
	p.Record("a", "m1", "/v1/chat/completions", 300, 40, 8*300+12*40, false)
	// Единственное наблюдение с reload.
	p.Record("a", "m1", "/v1/chat/completions", 120, 60, 700+8*120+12*60, true)

	st := p.Snapshot("a", "m1", "/v1/chat/completions")
	if !st.OK {
		t.Fatalf("регрессия не fit: %+v", st)
	}
	if st.Loaded != 1 {
		t.Errorf("Loaded = %d, want 1", st.Loaded)
	}
	approx := func(got, want, tol float64) bool { return math.Abs(got-want) < tol }
	if !approx(st.TLoadMs, 700, 5.0) {
		t.Errorf("t_load = %v, want ~700 (±5) из единственного reload-наблюдения", st.TLoadMs)
	}
	if !approx(st.KInMsTok, 8, 0.1) {
		t.Errorf("k_in = %v, want ~8", st.KInMsTok)
	}
	if !approx(st.KOutMsTok, 12, 0.1) {
		t.Errorf("k_out = %v, want ~12", st.KOutMsTok)
	}
}

// TestPerfTracker_NoLoadedFallsBackTo2Vars: все loaded=false → решаем 2×2,
// TLoadMs=0, OK=true.
func TestPerfTracker_NoLoadedFallsBackTo2Vars(t *testing.T) {
	p := NewPerfTracker()
	// t = 5*in + 8*out (нет t_load).
	gen := func(in, out int) int64 { return int64(5*in + 8*out) }
	p.Record("a", "m1", "/v1/chat/completions", 100, 50, gen(100, 50), false)
	p.Record("a", "m1", "/v1/chat/completions", 200, 100, gen(200, 100), false)
	p.Record("a", "m1", "/v1/chat/completions", 50, 200, gen(50, 200), false)
	st := p.Snapshot("a", "m1", "/v1/chat/completions")
	if !st.OK {
		t.Fatalf("регрессия не fit при loaded=0: %+v", st)
	}
	if st.TLoadMs != 0 {
		t.Errorf("при loaded=0 t_load должен быть 0, получено %v", st.TLoadMs)
	}
	approx := func(got, want, tol float64) bool { return math.Abs(got-want) < tol }
	if !approx(st.KInMsTok, 5, 0.1) {
		t.Errorf("k_in = %v, want ~5 (±0.1)", st.KInMsTok)
	}
	if !approx(st.KOutMsTok, 8, 0.1) {
		t.Errorf("k_out = %v, want ~8 (±0.1)", st.KOutMsTok)
	}
	if st.RSquared < 0.99 {
		t.Errorf("R² = %v, want ≥0.99 для идеальных данных", st.RSquared)
	}
}

// TestPerfTracker_RSquaredDegradedOnNoisyData: на шумных данных R² заметно ниже 1
// и FitQuality становится "degraded" если ниже порога.
func TestPerfTracker_RSquaredDegradedOnNoisyData(t *testing.T) {
	p := NewPerfTracker()
	// Чистая формула t = 10*in + 20*out, но добавляем большой шум.
	noisy := []struct {
		in, out int
		t       int64
	}{
		{100, 50, 200},   // ожидается 1000+1000=2000, факт 200 — выброс
		{200, 100, 3000}, // ожидается 4000, факт 3000
		{50, 200, 8000},  // ожидается 4500, факт 8000 — выброс
		{300, 30, 3500},  // ожидается 3600, факт 3500
		{150, 150, 4500}, // ожидается 4500, факт 4500
	}
	for _, c := range noisy {
		p.Record("a", "m1", "/v1/chat/completions", c.in, c.out, c.t, false)
	}
	st := p.Snapshot("a", "m1", "/v1/chat/completions")
	if !st.OK {
		t.Fatalf("регрессия не fit: %+v", st)
	}
	if st.RSquared >= 0.99 {
		t.Errorf("R² = %v, при шуме ожидалось значительно <0.99", st.RSquared)
	}
}

// TestPerfTracker_CIPopulated: при достаточном n > p доверительные интервалы
// заполнены (>0) для ненулевых коэффициентов.
func TestPerfTracker_CIPopulated(t *testing.T) {
	p := NewPerfTracker()
	// 6 точек, добавляем небольшой шум — иначе RSS=0 и CI=0.
	mk := func(in, out int, loaded bool, noise int64) int64 {
		tLoad := int64(0)
		if loaded {
			tLoad = 500
		}
		return tLoad + int64(10*in+20*out) + noise
	}
	cases := []struct {
		in, out int
		loaded  bool
		noise   int64
	}{
		{100, 50, true, 10},
		{200, 100, false, -5},
		{50, 200, false, 15},
		{300, 30, false, -10},
		{150, 150, true, 8},
		{80, 80, false, -3},
	}
	for _, c := range cases {
		p.Record("a", "m1", "/v1/chat/completions",
			c.in, c.out, mk(c.in, c.out, c.loaded, c.noise), c.loaded)
	}
	st := p.Snapshot("a", "m1", "/v1/chat/completions")
	if !st.OK {
		t.Fatalf("регрессия не fit: %+v", st)
	}
	if st.KInCI <= 0 {
		t.Errorf("KInCI = %v, want >0 при n=6 точек с шумом", st.KInCI)
	}
	if st.KOutCI <= 0 {
		t.Errorf("KOutCI = %v, want >0", st.KOutCI)
	}
	if st.TLoadMs > 0 && st.TLoadCI <= 0 {
		t.Errorf("TLoadCI = %v при TLoadMs=%v, ожидалось >0", st.TLoadCI, st.TLoadMs)
	}
}

// TestPerfTracker_PerKey: разные (server, model, endpoint) — независимые наборы.
func TestPerfTracker_PerKey(t *testing.T) {
	p := NewPerfTracker()
	p.Record("a", "m1", "/v1/chat/completions", 100, 50, 1000, false)
	p.Record("a", "m1", "/v1/chat/completions", 200, 100, 2000, false)
	p.Record("a", "m1", "/v1/chat/completions", 50, 200, 1500, false)
	// Та же модель, другой endpoint — отдельный бакет.
	p.Record("a", "m1", "/v1/embeddings", 100, 50, 100, false)
	p.Record("a", "m1", "/v1/embeddings", 200, 100, 200, false)
	p.Record("a", "m1", "/v1/embeddings", 50, 200, 150, false)
	p.Record("a", "m2", "/v1/chat/completions", 100, 50, 2000, false)
	p.Record("a", "m2", "/v1/chat/completions", 200, 100, 4000, false)
	p.Record("a", "m2", "/v1/chat/completions", 50, 200, 3000, false)
	p.Record("b", "m1", "/v1/chat/completions", 100, 50, 500, false)
	p.Record("b", "m1", "/v1/chat/completions", 200, 100, 1000, false)
	p.Record("b", "m1", "/v1/chat/completions", 50, 200, 750, false)

	if st := p.Snapshot("a", "m1", "/v1/chat/completions"); !st.OK || st.Samples != 3 {
		t.Errorf("a/m1/chat samples=%d ok=%v", st.Samples, st.OK)
	}
	if st := p.Snapshot("a", "m1", "/v1/embeddings"); !st.OK || st.Samples != 3 {
		t.Errorf("a/m1/embeddings samples=%d ok=%v", st.Samples, st.OK)
	}
	if st := p.Snapshot("a", "m2", "/v1/chat/completions"); st.Samples != 3 {
		t.Errorf("a/m2 samples = %d", st.Samples)
	}
	if st := p.Snapshot("b", "m1", "/v1/chat/completions"); st.Samples != 3 {
		t.Errorf("b/m1 samples = %d", st.Samples)
	}
	if st := p.Snapshot("c", "mX", "/v1/chat/completions"); st.Samples != 0 {
		t.Errorf("несуществующий ключ: samples = %d", st.Samples)
	}
}

// TestPerfTracker_IgnoresNoise: totalMs<=0 или out<=0 — точка игнорируется.
func TestPerfTracker_IgnoresNoise(t *testing.T) {
	p := NewPerfTracker()
	p.Record("a", "m1", "/v1/chat/completions", 100, 0, 1000, false)
	p.Record("a", "m1", "/v1/chat/completions", 100, 50, 0, false)
	p.Record("a", "m1", "/v1/chat/completions", 100, 50, 1000, false)
	st := p.Snapshot("a", "m1", "/v1/chat/completions")
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
			p.Record("a", "m1", "/v1/chat/completions", 100, 50, 1000, false)
			_ = p.Snapshot("a", "m1", "/v1/chat/completions")
		}()
	}
	wg.Wait()
	st := p.Snapshot("a", "m1", "/v1/chat/completions")
	if st.Samples == 0 {
		t.Errorf("concurrent: samples = 0")
	}
}

// TestPerfTracker_ServerSummary: возвращает (модель, endpoint), отсортированные
// по числу samples DESC.
func TestPerfTracker_ServerSummary(t *testing.T) {
	p := NewPerfTracker()
	for range 5 {
		p.Record("a", "popular", "/v1/chat/completions", 100, 50, 1000, false)
	}
	for range 2 {
		p.Record("a", "rare", "/v1/chat/completions", 100, 50, 1000, false)
	}
	// popular на /v1/embeddings — отдельный бакет, 3 наблюдения.
	for range 3 {
		p.Record("a", "popular", "/v1/embeddings", 100, 50, 100, false)
	}
	p.Record("b", "other", "/v1/chat/completions", 100, 50, 1000, false)

	sum := p.ServerSummary("a")
	if len(sum) != 3 {
		t.Fatalf("ServerSummary(a): want 3 buckets, got %d", len(sum))
	}
	// [0]: popular/chat (5 samples).
	if sum[0].Model != "popular" || sum[0].Endpoint != "/v1/chat/completions" || sum[0].Stats.Samples != 5 {
		t.Errorf("[0]: %+v, want popular/chat/5", sum[0])
	}
	// [1]: popular/embeddings (3 samples).
	if sum[1].Model != "popular" || sum[1].Endpoint != "/v1/embeddings" || sum[1].Stats.Samples != 3 {
		t.Errorf("[1]: %+v, want popular/embeddings/3", sum[1])
	}
	// [2]: rare (2 samples).
	if sum[2].Model != "rare" || sum[2].Stats.Samples != 2 {
		t.Errorf("[2]: %+v, want rare/2", sum[2])
	}
}
