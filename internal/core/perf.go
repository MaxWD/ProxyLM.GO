package core

import (
	"math"
	"sort"
	"sync"
)

// perfMinSamples — минимальное число успешных запросов на паре (server, model),
// после которого Snapshot отдаёт результат регрессии (OK=true). Меньше — нет
// данных для трёх неизвестных (t_load, k_in, k_out) даже теоретически.
const perfMinSamples = 3

// perfObservation — одна точка измерения по результату успешного запроса.
// in/out — usage.prompt_tokens / usage.completion_tokens (b и c в формуле),
// totalMs — полное время Run (t_all), loaded — был ли это первый запрос после
// смены current_model на сервере (1 в столбце t_load, иначе 0).
type perfObservation struct {
	in      int
	out     int
	totalMs int64
	loaded  bool
}

// perfKey — ключ пары (server, model).
type perfKey struct {
	server string
	model  string
}

// PerfTracker — хранилище наблюдений и калькулятор линейной регрессии
// производительности по парам (server, model).
//
// Модель: для каждой пары решаем задачу наименьших квадратов вида
//
//	loaded_i · t_load + b_i · k_in + c_i · k_out  ≈  t_all_i,
//
// где loaded_i ∈ {0, 1}, b_i = prompt_tokens, c_i = output_tokens, t_all_i =
// общее время Run в миллисекундах. Минимизируем Σ (t_all_i − fit_i)² методом
// нормальных уравнений X^T X · θ = X^T y.
//
// Дизайн:
//   - Хранятся ВСЕ наблюдения (выбор пользователя): bySlot[key] = []obs.
//     Пересчёт регрессии — на каждый Snapshot, O(N) по сумме точек одной пары.
//   - При loaded==0 во всех точках столбец «load» нулевой → 3×3 singular.
//     Деградируем до 2×2 (k_in, k_out), t_load = 0.
//   - При <perfMinSamples точках Snapshot возвращает OK=false (UI рисует «—»).
//   - Record nil-safe: nil-приёмник просто игнорируется.
type PerfTracker struct {
	mu     sync.RWMutex
	bySlot map[perfKey][]perfObservation
}

// NewPerfTracker создаёт пустой tracker.
func NewPerfTracker() *PerfTracker {
	return &PerfTracker{bySlot: make(map[perfKey][]perfObservation)}
}

// Record добавляет одну точку измерения. Игнорирует невалидные значения
// (totalMs ≤ 0 или out ≤ 0) — это шум от пустых ответов или ошибок апстрима.
// in допускается ≥ 0 (для embedding-запросов prompt_tokens может быть нулевым).
func (p *PerfTracker) Record(server, model string, in, out int, totalMs int64, loaded bool) {
	if p == nil {
		return
	}
	if totalMs <= 0 || out <= 0 {
		return
	}
	if in < 0 {
		in = 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	k := perfKey{server: server, model: model}
	p.bySlot[k] = append(p.bySlot[k], perfObservation{
		in: in, out: out, totalMs: totalMs, loaded: loaded,
	})
}

// PerfStats — результат регрессии по паре (server, model).
//
// Размерности:
//   - TLoadMs    — миллисекунды (один «накладной» tariff за load модели).
//   - KInMsTok   — миллисекунды на 1 входной токен (s/tok × 1000).
//   - KOutMsTok  — миллисекунды на 1 выходной токен.
//
// Для UI коэффициенты обычно переводят в tok/s: 1000 / Kxx.
type PerfStats struct {
	Samples   int
	Loaded    int // сколько из Samples с loaded=true
	TLoadMs   float64
	KInMsTok  float64
	KOutMsTok float64
	OK        bool // false если samples < perfMinSamples или система вырождена
}

// Snapshot возвращает результат регрессии для пары (server, model).
// Если данных меньше perfMinSamples, OK=false и поля метрик не заполнены.
func (p *PerfTracker) Snapshot(server, model string) PerfStats {
	if p == nil {
		return PerfStats{}
	}
	p.mu.RLock()
	obs := p.bySlot[perfKey{server: server, model: model}]
	cp := make([]perfObservation, len(obs))
	copy(cp, obs)
	p.mu.RUnlock()
	return fitRegression(cp)
}

// ModelSummary — агрегат по одной модели на одном сервере для server-modal UI.
type ModelSummary struct {
	Model string
	Stats PerfStats
}

// ServerSummary возвращает список моделей сервера и их статистику,
// отсортированный по числу наблюдений (DESC) — самые «нагруженные» сверху.
// Пустой результат, если по серверу ещё ничего не выполнялось.
func (p *PerfTracker) ServerSummary(server string) []ModelSummary {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	keys := make([]perfKey, 0)
	for k := range p.bySlot {
		if k.server == server {
			keys = append(keys, k)
		}
	}
	p.mu.RUnlock()
	out := make([]ModelSummary, 0, len(keys))
	for _, k := range keys {
		out = append(out, ModelSummary{Model: k.model, Stats: p.Snapshot(k.server, k.model)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Stats.Samples != out[j].Stats.Samples {
			return out[i].Stats.Samples > out[j].Stats.Samples
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// fitRegression — основная вычислительная процедура. Возвращает PerfStats
// с заполненными полями. Никогда не паникует: при сингулярной матрице
// возвращает OK=false (или OK=true с TLoadMs=0 при fallback'е на 2×2).
//
// Коэффициенты t_load / k_in / k_out гарантированно ≥ 0 НА УРОВНЕ РАСЧЁТА
// (не post-clamp): используется наивный NNLS (Non-negative Least Squares,
// active-set brute force) — см. solveNNLS3. Физически отрицательные значения
// бессмысленны (загрузка модели или обработка токена не уменьшают время) и
// при unconstrained LSQ возникают на коллинеарных / шумных данных. NNLS
// выбирает решение из неотрицательного ортанта с минимальным RSS, что и
// требуется.
func fitRegression(obs []perfObservation) PerfStats {
	stats := PerfStats{Samples: len(obs)}
	if len(obs) < perfMinSamples {
		return stats
	}
	for _, o := range obs {
		if o.loaded {
			stats.Loaded++
		}
	}

	if stats.Loaded == 0 {
		// Столбец load полностью нулевой — задача редуцируется к двум переменным.
		// 2-var NNLS: попробуем безусловное решение; если хоть один коэф <0,
		// fall back на одномерные подзадачи.
		k1, k2, ok := solveNNLS2(obs)
		if !ok {
			return stats
		}
		stats.KInMsTok = k1
		stats.KOutMsTok = k2
		stats.OK = true
		return stats
	}
	a, k1, k2, ok := solveNNLS3(obs)
	if !ok {
		return stats
	}
	stats.TLoadMs = a
	stats.KInMsTok = k1
	stats.KOutMsTok = k2
	stats.OK = true
	return stats
}

// solveNNLS3 — наивный NNLS для 3-переменной системы.
//
// Алгоритм (active-set brute force):
//  1. Решаем безусловное 3×3 (solveNormal3). Если все компоненты ≥ 0 — это
//     глобальный минимум LSQ и одновременно решение NNLS.
//  2. Иначе перебираем 6 граничных подзадач (KKT-углы):
//     • a=0 → 2-var (k1, k2);
//     • k1=0 → 2-var (a, k2);
//     • k2=0 → 2-var (a, k1);
//     • a=k1=0 → 1-var k2;
//     • a=k2=0 → 1-var k1;
//     • k1=k2=0 → 1-var a.
//     Для каждой допустимой подзадачи (все свободные переменные ≥ 0) считаем
//     residual RSS = Σ(y - fit)². Выбираем кандидата с минимальным RSS.
//  3. Если ни один граничный кандидат не оказался допустимым — возвращаем
//     all-zero c ok=true (это случай вырожденных данных, по которым лучшее
//     неотрицательное предсказание — нулевое).
//
// Возвращает (a, k_in, k_out, ok). ok=false только при «нет вообще данных»
// (≥ perfMinSamples гарантирован вызывающим, так что фактически всегда true).
func solveNNLS3(obs []perfObservation) (float64, float64, float64, bool) {
	bestA, bestK1, bestK2 := 0.0, 0.0, 0.0
	bestRSS := math.Inf(1)

	// 1. Unconstrained.
	if a, k1, k2, ok := solveNormal3(obs); ok && a >= 0 && k1 >= 0 && k2 >= 0 {
		return a, k1, k2, true
	}
	// 2a. a=0 → (k1, k2)
	if k1, k2, ok := solveNormal2(obs); ok && k1 >= 0 && k2 >= 0 {
		if r := rss3(obs, 0, k1, k2); r < bestRSS {
			bestRSS, bestA, bestK1, bestK2 = r, 0, k1, k2
		}
	}
	// 2b. k1=0 → (a, k2): нормальные уравнения по столбцам {loaded, c}.
	if a, k2, ok := solveNormal2Cols(obs, colLoaded, colOut); ok && a >= 0 && k2 >= 0 {
		if r := rss3(obs, a, 0, k2); r < bestRSS {
			bestRSS, bestA, bestK1, bestK2 = r, a, 0, k2
		}
	}
	// 2c. k2=0 → (a, k1)
	if a, k1, ok := solveNormal2Cols(obs, colLoaded, colIn); ok && a >= 0 && k1 >= 0 {
		if r := rss3(obs, a, k1, 0); r < bestRSS {
			bestRSS, bestA, bestK1, bestK2 = r, a, k1, 0
		}
	}
	// 3a. a=k1=0 → k2 = Σ(c·y) / Σ(c²)
	if k2, ok := solveNormal1(obs, colOut); ok && k2 >= 0 {
		if r := rss3(obs, 0, 0, k2); r < bestRSS {
			bestRSS, bestA, bestK1, bestK2 = r, 0, 0, k2
		}
	}
	// 3b. a=k2=0 → k1
	if k1, ok := solveNormal1(obs, colIn); ok && k1 >= 0 {
		if r := rss3(obs, 0, k1, 0); r < bestRSS {
			bestRSS, bestA, bestK1, bestK2 = r, 0, k1, 0
		}
	}
	// 3c. k1=k2=0 → a
	if a, ok := solveNormal1(obs, colLoaded); ok && a >= 0 {
		if r := rss3(obs, a, 0, 0); r < bestRSS {
			bestRSS, bestA, bestK1, bestK2 = r, a, 0, 0
		}
	}
	// 4. all-zero — всегда допустимо.
	if rss3(obs, 0, 0, 0) < bestRSS {
		bestA, bestK1, bestK2 = 0, 0, 0
	}
	return clampNonNeg(bestA), clampNonNeg(bestK1), clampNonNeg(bestK2), true
}

// solveNNLS2 — 2-переменный аналог solveNNLS3 (только k_in / k_out, без
// t_load). Используется в Loaded==0 ветке fitRegression.
func solveNNLS2(obs []perfObservation) (float64, float64, bool) {
	bestK1, bestK2 := 0.0, 0.0
	bestRSS := math.Inf(1)
	if k1, k2, ok := solveNormal2(obs); ok && k1 >= 0 && k2 >= 0 {
		return k1, k2, true
	}
	if k2, ok := solveNormal1(obs, colOut); ok && k2 >= 0 {
		if r := rss3(obs, 0, 0, k2); r < bestRSS {
			bestRSS, bestK1, bestK2 = r, 0, k2
		}
	}
	if k1, ok := solveNormal1(obs, colIn); ok && k1 >= 0 {
		if r := rss3(obs, 0, k1, 0); r < bestRSS {
			bestRSS, bestK1, bestK2 = r, k1, 0
		}
	}
	if r := rss3(obs, 0, 0, 0); r < bestRSS {
		bestK1, bestK2 = 0, 0
	}
	return clampNonNeg(bestK1), clampNonNeg(bestK2), true
}

// rss3 возвращает Σ (y_i − a·loaded_i − k1·in_i − k2·out_i)² — RSS для NNLS.
func rss3(obs []perfObservation, a, k1, k2 float64) float64 {
	rss := 0.0
	for _, o := range obs {
		l := 0.0
		if o.loaded {
			l = 1.0
		}
		fit := a*l + k1*float64(o.in) + k2*float64(o.out)
		d := float64(o.totalMs) - fit
		rss += d * d
	}
	return rss
}

// colKind — какая колонка в наблюдении используется для 1-var подзадачи NNLS.
type colKind int

const (
	colLoaded colKind = iota // x = 1 если loaded, иначе 0
	colIn                    // x = in
	colOut                   // x = out
)

func colValue(o perfObservation, c colKind) float64 {
	switch c {
	case colLoaded:
		if o.loaded {
			return 1.0
		}
		return 0.0
	case colIn:
		return float64(o.in)
	case colOut:
		return float64(o.out)
	}
	return 0
}

// solveNormal1 решает одномерную задачу min Σ(y - β·x)² → β = Σ(x·y) / Σ(x²).
// Возвращает (β, ok). ok=false при Σ(x²) близком к 0 — переменная неинформативна.
func solveNormal1(obs []perfObservation, c colKind) (float64, bool) {
	var sxx, sxy float64
	for _, o := range obs {
		x := colValue(o, c)
		y := float64(o.totalMs)
		sxx += x * x
		sxy += x * y
	}
	if sxx < 1e-9 {
		return 0, false
	}
	return sxy / sxx, true
}

// solveNormal2Cols — двумерная задача наименьших квадратов для произвольной
// пары колонок (c1, c2). Возвращает (β1, β2, ok). ok=false при сингулярной
// нормальной матрице (одна из колонок ≈ 0 или колонки коллинеарны).
func solveNormal2Cols(obs []perfObservation, c1, c2 colKind) (float64, float64, bool) {
	var a11, a12, a22 float64
	var r1, r2 float64
	for _, o := range obs {
		x1 := colValue(o, c1)
		x2 := colValue(o, c2)
		y := float64(o.totalMs)
		a11 += x1 * x1
		a12 += x1 * x2
		a22 += x2 * x2
		r1 += x1 * y
		r2 += x2 * y
	}
	det := a11*a22 - a12*a12
	if math.Abs(det) < 1e-9 {
		return 0, 0, false
	}
	return (a22*r1 - a12*r2) / det, (-a12*r1 + a11*r2) / det, true
}

// clampNonNeg возвращает v, если v >= 0, иначе 0. Используется как
// финальный страховочный clamp для float-шумов внутри NNLS-кандидатов
// (например, −1e-13 из-за округления Крамера).
func clampNonNeg(v float64) float64 {
	if v < 0 || math.IsNaN(v) {
		return 0
	}
	return v
}

// Predict возвращает оценку totalMs для запроса (in, out, loaded) на паре
// (server, model) по текущей регрессии. Возвращает 0, если данных недостаточно
// (samples < perfMinSamples) или регрессия вырождена. Используется slow-detect.
func (p *PerfTracker) Predict(server, model string, in, out int, loaded bool) float64 {
	if p == nil {
		return 0
	}
	st := p.Snapshot(server, model)
	if !st.OK {
		return 0
	}
	predicted := st.KInMsTok*float64(in) + st.KOutMsTok*float64(out)
	if loaded {
		predicted += st.TLoadMs
	}
	if predicted <= 0 {
		return 0
	}
	return predicted
}

// solveNormal3 решает нормальные уравнения 3×3 для модели
// loaded·a + b·k_in + c·k_out = y методом Крамера. Возвращает (a, k_in, k_out, ok).
func solveNormal3(obs []perfObservation) (float64, float64, float64, bool) {
	var m [3][3]float64
	var rhs [3]float64
	for _, o := range obs {
		x0 := 0.0
		if o.loaded {
			x0 = 1.0
		}
		x1 := float64(o.in)
		x2 := float64(o.out)
		y := float64(o.totalMs)
		m[0][0] += x0 * x0
		m[0][1] += x0 * x1
		m[0][2] += x0 * x2
		m[1][1] += x1 * x1
		m[1][2] += x1 * x2
		m[2][2] += x2 * x2
		rhs[0] += x0 * y
		rhs[1] += x1 * y
		rhs[2] += x2 * y
	}
	m[1][0] = m[0][1]
	m[2][0] = m[0][2]
	m[2][1] = m[1][2]
	return cramer3(m, rhs)
}

// solveNormal2 — то же, но без столбца loaded: b·k_in + c·k_out = y.
func solveNormal2(obs []perfObservation) (float64, float64, bool) {
	var a11, a12, a22 float64
	var r1, r2 float64
	for _, o := range obs {
		x1 := float64(o.in)
		x2 := float64(o.out)
		y := float64(o.totalMs)
		a11 += x1 * x1
		a12 += x1 * x2
		a22 += x2 * x2
		r1 += x1 * y
		r2 += x2 * y
	}
	det := a11*a22 - a12*a12
	if math.Abs(det) < 1e-9 {
		return 0, 0, false
	}
	k1 := (a22*r1 - a12*r2) / det
	k2 := (-a12*r1 + a11*r2) / det
	return k1, k2, true
}

// cramer3 — решение 3×3 СЛАУ методом Крамера. Возвращает (x0, x1, x2, ok),
// ok=false при близком к нулю определителе (matrix is singular).
func cramer3(m [3][3]float64, b [3]float64) (float64, float64, float64, bool) {
	d := det3(m)
	if math.Abs(d) < 1e-9 {
		return 0, 0, 0, false
	}
	var m0, m1, m2 [3][3]float64
	m0 = m
	m1 = m
	m2 = m
	for i := 0; i < 3; i++ {
		m0[i][0] = b[i]
		m1[i][1] = b[i]
		m2[i][2] = b[i]
	}
	return det3(m0) / d, det3(m1) / d, det3(m2) / d, true
}

func det3(m [3][3]float64) float64 {
	return m[0][0]*(m[1][1]*m[2][2]-m[1][2]*m[2][1]) -
		m[0][1]*(m[1][0]*m[2][2]-m[1][2]*m[2][0]) +
		m[0][2]*(m[1][0]*m[2][1]-m[1][1]*m[2][0])
}
