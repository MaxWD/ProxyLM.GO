package core

import (
	"math"
	"sort"
	"sync"
)

// perfMinSamples — минимальное число успешных запросов на ключе
// (server, model, endpoint), после которого Snapshot отдаёт результат регрессии
// (OK=true). Меньше — нет данных для трёх неизвестных (t_load, k_in, k_out)
// даже теоретически.
const perfMinSamples = 3

// perfMaxObservations — максимальный размер кольцевого буфера наблюдений на ключ
// (server, model, endpoint). При превышении самое старое наблюдение дропается (M11).
const perfMaxObservations = 1000

// perfRidgeLambda — параметр L2-регуляризации (Tikhonov / ridge) нормальных
// уравнений: решаем (X^T X + λI) · θ = X^T y вместо X^T X · θ = X^T y.
//
// Зачем:
//
//  1. Гарантирует невырожденность системы при коллинеарных данных
//     (типичный случай — все наблюдения с loaded=0: первый столбец нулевой).
//  2. Уменьшает variance оценки коэффициентов за счёт небольшого bias —
//     полезно при малом числе точек и шумных замерах.
//  3. Численно стабилизирует решение при сильно различающихся масштабах
//     столбцов (loaded ∈ {0,1}, in/out — сотни–тысячи).
//
// Значение 1e-4 выбрано как «достаточно малое, чтобы не искажать коэффициенты
// на хорошо обусловленных данных (≈10⁻⁴ от диагональных элементов), и
// достаточное, чтобы решать вырожденные случаи». Введено в v0.10.0.
const perfRidgeLambda = 1e-4

// perfRSquaredGoodFit — порог R² (коэффициента детерминации), выше которого
// fit считается «хорошим» (FitQuality="good"); ниже — «degraded». Используется
// TUI для подсветки server-detail-modal: degraded fit обычно означает, что
// замеры зашумлены или ещё мало точек, и точечной оценке нельзя доверять
// без учёта доверительного интервала. Введено в v0.10.0.
const perfRSquaredGoodFit = 0.70

// perfZ95 — критическое значение нормального распределения для двустороннего
// 95% доверительного интервала (приближение Z вместо t для умеренных n).
// При n−p ≥ 30 ошибка приближения < 5%, в наших задачах с n до 1000 — пренебрежимо.
const perfZ95 = 1.96

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

// perfKey — ключ (server, model, endpoint). Endpoint введён в v0.10.0:
// запросы разных типов (/v1/chat/completions, /v1/embeddings) имеют
// принципиально разные профили tokens/ms — смешивать их регрессией нельзя
// без искажения оценок.
type perfKey struct {
	server   string
	model    string
	endpoint string
}

// PerfTracker — хранилище наблюдений и калькулятор линейной ridge-регрессии
// производительности по ключам (server, model, endpoint).
//
// Модель: для каждой пары решаем задачу наименьших квадратов вида
//
//	loaded_i · t_load + b_i · k_in + c_i · k_out  ≈  t_all_i,
//
// где loaded_i ∈ {0, 1}, b_i = prompt_tokens, c_i = output_tokens, t_all_i =
// общее время Run в миллисекундах. Минимизируем
//
//	Σ (t_all_i − fit_i)² + λ · (t_load² + k_in² + k_out²)
//
// (L2-регуляризация, см. perfRidgeLambda). Это эквивалентно решению
// (X^T X + λI) · θ = X^T y методом нормальных уравнений.
//
// Дополнительно вычисляем:
//   - R² (RSquared) — коэффициент детерминации модели, [0, 1];
//   - SE(θ_j) — стандартные ошибки коэффициентов через
//     σ̂² · diag((X^T X + λI)^-1), σ̂² = RSS / (n − p);
//   - 95% доверительные интервалы θ_j ± z₀.₉₇₅ · SE.
//
// Дизайн:
//   - Хранятся ВСЕ наблюдения (выбор пользователя): bySlot[key] = []obs.
//     Пересчёт регрессии — на каждый Snapshot, O(N) по сумме точек одного ключа.
//   - Коэффициенты — неотрицательные через NNLS (active-set brute force);
//     ridge применяется при формировании нормальной матрицы, NNLS не меняется.
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
//
// endpoint в v0.10.0 стал частью ключа. Пустая строка допускается — будет
// отдельным «безымянным» бакетом (на случай вызова до миграции данных в
// storage / тестового кода).
func (p *PerfTracker) Record(server, model, endpoint string, in, out int, totalMs int64, loaded bool) {
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
	k := perfKey{server: server, model: model, endpoint: endpoint}
	obs := p.bySlot[k]
	obs = append(obs, perfObservation{in: in, out: out, totalMs: totalMs, loaded: loaded})
	// Ring-buffer: при превышении cap дропаем самое старое наблюдение (M11).
	if len(obs) > perfMaxObservations {
		obs = obs[len(obs)-perfMaxObservations:]
	}
	p.bySlot[k] = obs
}

// PerfStats — результат ridge-регрессии по ключу (server, model, endpoint).
//
// Размерности:
//   - TLoadMs    — миллисекунды (один «накладной» tariff за load модели).
//   - KInMsTok   — миллисекунды на 1 входной токен (s/tok × 1000).
//   - KOutMsTok  — миллисекунды на 1 выходной токен.
//   - RSquared   — коэффициент детерминации, [0, 1]; 1 = идеальный fit,
//     0 = модель не лучше константы mean(y).
//   - TLoadCI / KInCI / KOutCI — half-width 95% доверительного интервала
//     для соответствующего коэффициента (например, KInCI = 5.0 при
//     KInMsTok = 100.0 означает 95% CI [95.0, 105.0]). 0 если CI не
//     вычислим (n ≤ p, т. е. точек ≤ числу параметров) или коэффициент
//     зафиксирован NNLS на границе (=0).
//   - FitQuality — словесная классификация: "good" (R²≥perfRSquaredGoodFit),
//     "degraded" (< порога), "" если OK=false.
//
// Для UI коэффициенты обычно переводят в tok/s: 1000 / Kxx.
type PerfStats struct {
	Samples    int
	Loaded     int // сколько из Samples с loaded=true
	TLoadMs    float64
	KInMsTok   float64
	KOutMsTok  float64
	RSquared   float64
	TLoadCI    float64
	KInCI      float64
	KOutCI     float64
	FitQuality string
	OK         bool // false если samples < perfMinSamples или система вырождена
}

// Snapshot возвращает результат регрессии для ключа (server, model, endpoint).
// Если данных меньше perfMinSamples, OK=false и поля метрик не заполнены.
func (p *PerfTracker) Snapshot(server, model, endpoint string) PerfStats {
	if p == nil {
		return PerfStats{}
	}
	p.mu.RLock()
	obs := p.bySlot[perfKey{server: server, model: model, endpoint: endpoint}]
	cp := make([]perfObservation, len(obs))
	copy(cp, obs)
	p.mu.RUnlock()
	return fitRegression(cp)
}

// ModelSummary — агрегат по одной модели на одном сервере на одном endpoint
// для server-modal UI.
type ModelSummary struct {
	Model    string
	Endpoint string
	Stats    PerfStats
}

// ServerSummary возвращает список (модель, endpoint) сервера и их статистику,
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
		out = append(out, ModelSummary{
			Model:    k.model,
			Endpoint: k.endpoint,
			Stats:    p.Snapshot(k.server, k.model, k.endpoint),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Stats.Samples != out[j].Stats.Samples {
			return out[i].Stats.Samples > out[j].Stats.Samples
		}
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].Endpoint < out[j].Endpoint
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
//
// После определения коэффициентов вычисляются R² и 95% CI (см. computeFitQuality).
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
		fillFitQuality(&stats, obs, false /*hasLoadCol*/)
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
	fillFitQuality(&stats, obs, true /*hasLoadCol*/)
	return stats
}

// fillFitQuality вычисляет R², стандартные ошибки и 95% CI для текущей точки θ
// (которая уже лежит в stats). hasLoadCol определяет, входит ли столбец loaded
// в матрицу плана (true — 3-var модель, false — 2-var fallback при Loaded=0).
//
// Если RSS = 0 (идеальный fit) — R²=1, CI=0. Если n ≤ p (точек не больше числа
// параметров) — σ̂² не определена, CI остаются нулевыми, R² всё равно
// вычисляется. На NNLS-границе (коэффициент = 0) CI принудительно зануляется
// — иначе интервал «[0, +SE]» содержит фиктивную свободу, которой нет.
func fillFitQuality(stats *PerfStats, obs []perfObservation, hasLoadCol bool) {
	rss := rss3(obs, stats.TLoadMs, stats.KInMsTok, stats.KOutMsTok)
	tss := tssY(obs)
	if tss > 0 {
		stats.RSquared = 1.0 - rss/tss
		if stats.RSquared < 0 {
			stats.RSquared = 0
		}
		if stats.RSquared > 1 {
			stats.RSquared = 1
		}
	} else {
		// Все y одинаковые — модель константы предсказывает идеально.
		stats.RSquared = 1
	}
	if stats.RSquared >= perfRSquaredGoodFit {
		stats.FitQuality = "good"
	} else {
		stats.FitQuality = "degraded"
	}
	n := len(obs)
	p := 3
	if !hasLoadCol {
		p = 2
	}
	if n <= p {
		return
	}
	sigma2 := rss / float64(n-p)
	if sigma2 <= 0 {
		return
	}
	// Диагональ (X^T X + λI)^-1: вычисляется обращением 3×3 (или 2×2) симметричной
	// матрицы с λ на диагонали — той же, что использовалась при решении.
	diag := normalInverseDiag(obs, hasLoadCol)
	if hasLoadCol {
		if stats.TLoadMs > 0 && diag[0] > 0 {
			stats.TLoadCI = perfZ95 * math.Sqrt(sigma2*diag[0])
		}
		if stats.KInMsTok > 0 && diag[1] > 0 {
			stats.KInCI = perfZ95 * math.Sqrt(sigma2*diag[1])
		}
		if stats.KOutMsTok > 0 && diag[2] > 0 {
			stats.KOutCI = perfZ95 * math.Sqrt(sigma2*diag[2])
		}
	} else {
		if stats.KInMsTok > 0 && diag[0] > 0 {
			stats.KInCI = perfZ95 * math.Sqrt(sigma2*diag[0])
		}
		if stats.KOutMsTok > 0 && diag[1] > 0 {
			stats.KOutCI = perfZ95 * math.Sqrt(sigma2*diag[1])
		}
	}
}

// tssY возвращает TSS = Σ (y_i − mean(y))² — total sum of squares, знаменатель R².
func tssY(obs []perfObservation) float64 {
	if len(obs) == 0 {
		return 0
	}
	var sum float64
	for _, o := range obs {
		sum += float64(o.totalMs)
	}
	mean := sum / float64(len(obs))
	var tss float64
	for _, o := range obs {
		d := float64(o.totalMs) - mean
		tss += d * d
	}
	return tss
}

// normalInverseDiag возвращает диагональ матрицы (X^T X + λI)^-1, где X — план
// модели (3 столбца loaded/in/out при hasLoadCol=true, 2 столбца in/out иначе).
// Используется computeCI для оценки SE коэффициентов.
//
// Реализация: формируем матрицу A = X^T X + λI, обращаем её через adjugate
// (трёхмерный случай: A^-1 = adj(A) / det(A)). Возвращаемый размер — len 3
// для hasLoadCol=true, иначе len 2. При сингулярности (det ≈ 0) возвращает
// нули — CI не вычисляются.
func normalInverseDiag(obs []perfObservation, hasLoadCol bool) []float64 {
	if hasLoadCol {
		var m [3][3]float64
		for _, o := range obs {
			x0 := 0.0
			if o.loaded {
				x0 = 1.0
			}
			x1 := float64(o.in)
			x2 := float64(o.out)
			m[0][0] += x0 * x0
			m[0][1] += x0 * x1
			m[0][2] += x0 * x2
			m[1][1] += x1 * x1
			m[1][2] += x1 * x2
			m[2][2] += x2 * x2
		}
		m[0][0] += perfRidgeLambda
		m[1][1] += perfRidgeLambda
		m[2][2] += perfRidgeLambda
		m[1][0] = m[0][1]
		m[2][0] = m[0][2]
		m[2][1] = m[1][2]
		d := det3(m)
		if math.Abs(d) < 1e-12 {
			return []float64{0, 0, 0}
		}
		// Диагональ обратной = соответствующий cofactor / det.
		c00 := m[1][1]*m[2][2] - m[1][2]*m[2][1]
		c11 := m[0][0]*m[2][2] - m[0][2]*m[2][0]
		c22 := m[0][0]*m[1][1] - m[0][1]*m[1][0]
		return []float64{c00 / d, c11 / d, c22 / d}
	}
	var a11, a12, a22 float64
	for _, o := range obs {
		x1 := float64(o.in)
		x2 := float64(o.out)
		a11 += x1 * x1
		a12 += x1 * x2
		a22 += x2 * x2
	}
	a11 += perfRidgeLambda
	a22 += perfRidgeLambda
	det := a11*a22 - a12*a12
	if math.Abs(det) < 1e-12 {
		return []float64{0, 0}
	}
	return []float64{a22 / det, a11 / det}
}

// solveNNLS3 — наивный NNLS для 3-переменной системы.
//
// Алгоритм (active-set brute force):
//  1. Решаем безусловное 3×3 (solveNormal3) с ridge-регуляризацией. Если все
//     компоненты ≥ 0 — это глобальный минимум LSQ-с-ridge и одновременно
//     решение NNLS-с-ridge.
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

	// 1. Unconstrained (с ridge).
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
	// 3a. a=k1=0 → k2 = Σ(c·y) / (Σ(c²) + λ)
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

// solveNormal1 решает одномерную ridge-задачу
//
//	min Σ(y - β·x)² + λβ²  →  β = Σ(x·y) / (Σ(x²) + λ).
//
// Возвращает (β, ok). ok=false при Σ(x²)+λ близком к 0 (на практике λ>0
// сразу даёт ok=true, но проверка оставлена ради единообразия с solveNormalN).
func solveNormal1(obs []perfObservation, c colKind) (float64, bool) {
	var sxx, sxy float64
	for _, o := range obs {
		x := colValue(o, c)
		y := float64(o.totalMs)
		sxx += x * x
		sxy += x * y
	}
	sxx += perfRidgeLambda
	if sxx < 1e-9 {
		return 0, false
	}
	return sxy / sxx, true
}

// solveNormal2Cols — двумерная ridge-задача для произвольной пары колонок
// (c1, c2). Возвращает (β1, β2, ok). ok=false при сингулярной (даже с λI)
// нормальной матрице — в практике λ>0 устраняет сингулярность, но порог
// проверяется.
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
	a11 += perfRidgeLambda
	a22 += perfRidgeLambda
	det := a11*a22 - a12*a12
	if math.Abs(det) < 1e-12 {
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

// Predict возвращает оценку totalMs для запроса (in, out, loaded) на ключе
// (server, model, endpoint) по текущей регрессии. Возвращает 0, если данных
// недостаточно (samples < perfMinSamples) или регрессия вырождена. Используется
// slow-detect.
func (p *PerfTracker) Predict(server, model, endpoint string, in, out int, loaded bool) float64 {
	if p == nil {
		return 0
	}
	st := p.Snapshot(server, model, endpoint)
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

// solveNormal3 решает нормальные уравнения 3×3 с ridge-регуляризацией:
//
//	(X^T X + λI) · θ = X^T y
//
// для модели loaded·a + b·k_in + c·k_out = y методом Крамера. Возвращает
// (a, k_in, k_out, ok).
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
	m[0][0] += perfRidgeLambda
	m[1][1] += perfRidgeLambda
	m[2][2] += perfRidgeLambda
	m[1][0] = m[0][1]
	m[2][0] = m[0][2]
	m[2][1] = m[1][2]
	return cramer3(m, rhs)
}

// solveNormal2 — то же, но без столбца loaded: b·k_in + c·k_out = y.
// Также с ridge-регуляризацией.
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
	a11 += perfRidgeLambda
	a22 += perfRidgeLambda
	det := a11*a22 - a12*a12
	if math.Abs(det) < 1e-12 {
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
	if math.Abs(d) < 1e-12 {
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
