package tui

import (
	"hash/fnv"
	"os"

	"github.com/charmbracelet/lipgloss"
)

// noUnicodeFlag кешируется один раз при старте пакета, чтобы не делать
// os.Getenv на каждый рендер Glyph*.
var noUnicodeFlag = os.Getenv("PROXYLM_NO_UNICODE") == "1"

// noUnicode возвращает true, если задана переменная окружения PROXYLM_NO_UNICODE=1.
func noUnicode() bool {
	return noUnicodeFlag
}

// glyph выбирает unicode-символ или ASCII-fallback в зависимости от noUnicode().
func glyph(unicode, ascii string) string {
	if noUnicode() {
		return ascii
	}
	return unicode
}

// GlyphHealthy — символ здорового сервера (●).
func GlyphHealthy() string { return glyph("●", "*") }

// GlyphUnhealthy — символ нездорового сервера (✗).
func GlyphUnhealthy() string { return glyph("✗", "!") }

// GlyphRunning — символ выполняющегося запроса (▶).
func GlyphRunning() string { return glyph("▶", ">") }

// GlyphQueued — символ ожидающего запроса.
// ⏳ (U+23F3) — emoji wide-character (2 cell), ломает выравнивание соседних
// колонок: pad считает rune'ы и думает что это 1 cell. Заменили на … (U+2026,
// HORIZONTAL ELLIPSIS) — single-cell, без зависимости от runewidth-учёта.
func GlyphQueued() string { return glyph("…", ".") }

// GlyphCompleted — символ завершённого запроса (✓).
func GlyphCompleted() string { return glyph("✓", "+") }

// GlyphFailed — символ ошибки (✗).
func GlyphFailed() string { return glyph("✗", "!") }

// GlyphArrow — стрелка для токенов (→).
func GlyphArrow() string { return glyph("→", "->") }

// GlyphTokIn — стрелка вниз для входных (prompt) токенов в метрике.
// Префиксует tok/s значение в header'е (см. renderServerMetric).
func GlyphTokIn() string { return glyph("↓", "in:") }

// GlyphTokOut — стрелка вверх для выходных (completion) токенов в метрике.
func GlyphTokOut() string { return glyph("↑", "out:") }

// GlyphCheck — отметка для колонки RM (модель грузилась для задачи).
func GlyphCheck() string { return glyph("✓", "+") }

// GlyphFailedServer — крестик перед именем сервера в колонке Server / InfoPane,
// маркирующий упавшую попытку. ASCII-fallback: "X".
func GlyphFailedServer() string { return glyph("✗", "X") }

// GlyphUnloaded — маркер «модель выгружена из памяти»: прокси считает current_model
// загруженной, но нативная проба её в памяти не видит (idle-unload). ⏏ (eject).
func GlyphUnloaded() string { return glyph("⏏", "^") }

// Цветовая палитра — ANSI 16 цветов.
// Числа соответствуют стандартным ANSI codes 0–15, безопасно для cmd.exe / PowerShell / Windows Terminal.
var (
	colorDimmed      = lipgloss.Color("8")  // dark gray
	colorBrightRed   = lipgloss.Color("9")  // bright red
	colorBrightGreen = lipgloss.Color("10") // bright green
	colorYellow      = lipgloss.Color("11") // yellow
	colorBrightBlue  = lipgloss.Color("12") // bright blue
	colorCyan        = lipgloss.Color("14") // bright cyan
	colorWhite       = lipgloss.Color("15") // bright white
	colorBlue        = lipgloss.Color("4")  // blue — фон выделенной строки
	colorMagenta     = lipgloss.Color("5")  // magenta — retry counter
	colorTeal        = lipgloss.Color("6")  // teal/cyan (non-bright) — перф-метрики
)

// Стили виджетов. Палитра соответствует утверждённому дизайн-документу.
var (
	// Рамки и заголовки.
	StyleBorderActive = lipgloss.NewStyle().
				BorderStyle(lipgloss.RoundedBorder()).
				BorderForeground(colorBrightBlue).
				Bold(true)

	StyleBorderInactive = lipgloss.NewStyle().
				BorderStyle(lipgloss.RoundedBorder()).
				BorderForeground(colorDimmed)

	StyleTitle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorBrightBlue)

	StyleHeader = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorWhite)

	StyleColHeader = lipgloss.NewStyle().Bold(true).Foreground(colorDimmed)

	// Статусы запросов (по дизайну: running=cyan, queued=white, done=green, failed=red).
	StyleStatusRunning = lipgloss.NewStyle().Foreground(colorCyan).Bold(true)
	StyleStatusQueued  = lipgloss.NewStyle().Foreground(colorWhite)
	StyleStatusDone    = lipgloss.NewStyle().Foreground(colorBrightGreen)
	StyleStatusFailed  = lipgloss.NewStyle().Foreground(colorBrightRed).Bold(true)

	// INV-2: запрос требует swap модели — dark gray.
	StyleNeedsSwap = lipgloss.NewStyle().Foreground(colorDimmed)

	// Серверы (чипы в HeaderBar).
	StyleServerHealthy   = lipgloss.NewStyle().Foreground(colorBrightGreen)
	StyleServerUnhealthy = lipgloss.NewStyle().Foreground(colorBrightRed)
	StyleServerInFlight  = lipgloss.NewStyle().Foreground(colorBrightGreen).Bold(true)
	// serverPulseFrames — кадры «дыхания» лампы работающего (in-flight) сервера:
	// яркость точки плавно меняется bright→mid→dim→mid, читается как пульс (v0.13.0).
	// Только ANSI 16-color (10/2/8) — безопасно во всех терминалах, включая cmd.exe.
	// Idle-сервер использует ровный StyleServerHealthy (без пульса).
	// StyleServerFailed — сервер, на котором попытка запроса упала. Bold + red.
	// Используется в колонке Server для текущего/прошлого имени и в InfoPane.
	// Дополнительный сигнал — глиф «✗» перед именем (см. GlyphFailedServer).
	// Зачёркивание убрано в v0.9.1 — выглядело визуально шумно. Цвет
	// bright red в обычной палитре ServerColor зарезервирован (см. ниже).
	StyleServerFailed = lipgloss.NewStyle().
				Foreground(colorBrightRed).
				Bold(true)
	// StyleSlow — алерт «!!! МЕДЛЕННО !!!» в чипе и InfoPane.
	StyleSlow = lipgloss.NewStyle().
			Foreground(colorBrightRed).
			Background(colorYellow).
			Bold(true)

	// Flash жёлтым при смене current_model (2 секунды).
	StyleFlash = lipgloss.NewStyle().Foreground(colorYellow).Bold(true)

	// Retry-suffix "(N/M)" в running-строке.
	StyleRetry = lipgloss.NewStyle().Foreground(colorMagenta)

	// StyleCI — цвет для значений погрешности (±CI) в per-model таблице
	// server-detail (InfoPane). Суффикс «±<margin>» красится teal, чтобы
	// оператор сразу видел, что это доверительный интервал, а не само значение.
	StyleCI = lipgloss.NewStyle().Foreground(colorTeal)

	// Разное.
	StyleDim      = lipgloss.NewStyle().Foreground(colorDimmed)
	StyleError    = lipgloss.NewStyle().Foreground(colorBrightRed).Bold(true)
	StyleSelected = lipgloss.NewStyle().Background(colorBlue).Foreground(colorWhite)
	StyleFooter   = lipgloss.NewStyle().Foreground(colorDimmed)

	// StyleModalTitle — заголовок help overlay (см. renderHelpOverlay).
	StyleModalTitle = lipgloss.NewStyle().Bold(true).Foreground(colorCyan)

	// Help overlay.
	StyleHelpBorder = lipgloss.NewStyle().
			BorderStyle(lipgloss.RoundedBorder()).
			BorderForeground(colorBrightBlue).
			Padding(1, 2)
	StyleHelpKey  = lipgloss.NewStyle().Bold(true).Foreground(colorBrightBlue)
	StyleHelpDesc = lipgloss.NewStyle().Foreground(colorWhite)
)

// serverPalette — палитра различимых цветов для серверных чипов. Первые пять —
// яркие ANSI (10–14), безопасные даже в legacy cmd.exe; остальные — 256-color
// коды (поддерживаются Windows Terminal / PowerShell / современными эмуляторами),
// добавлены чтобы серверов 6+ не повторялись по цвету (v0.13.0). Bright red (9)
// намеренно исключён: зарезервирован под StyleServerFailed, чтобы провалившийся
// сервер не сливался с обычным «красным».
//
// Цвет назначается по ИНДЕКСУ сервера в отсортированном по приоритету списке
// (см. ServerColorByIndex) — это гарантирует различимость до len(serverPalette)
// серверов и стабильность (порядок фиксирован конфигом), в отличие от прежнего
// fnv-хэша имени, где коллизии в 5 «корзин» давали повтор уже на 3–4 серверах.
var serverPalette = []lipgloss.Color{
	lipgloss.Color("10"),  // bright green
	lipgloss.Color("12"),  // bright blue
	lipgloss.Color("11"),  // yellow
	lipgloss.Color("13"),  // bright magenta
	lipgloss.Color("14"),  // bright cyan
	lipgloss.Color("208"), // orange
	lipgloss.Color("45"),  // azure
	lipgloss.Color("207"), // light pink
	lipgloss.Color("156"), // light green
	lipgloss.Color("99"),  // purple
	lipgloss.Color("214"), // amber
	lipgloss.Color("51"),  // aqua
}

// ServerColorByIndex возвращает стиль сервера по его позиции в отсортированном
// списке. Различимо до len(serverPalette) серверов; дальше цвета повторяются
// (по модулю), но это уже редкий случай для прокси.
func ServerColorByIndex(idx int) lipgloss.Style {
	if idx < 0 {
		return StyleDim
	}
	return lipgloss.NewStyle().Foreground(serverPalette[idx%len(serverPalette)]).Bold(true)
}

// serverColorFor выбирает цвет сервера name по карте index (serverName→позиция
// в отсортированном списке). Для имён вне карты (например, сервер запроса, уже
// пропавший из шапки) — стабильный fallback по fnv-хэшу, чтобы колонка Server в
// таблице запросов оставалась окрашенной детерминированно.
func serverColorFor(name string, index map[string]int) lipgloss.Style {
	if name == "" {
		return StyleDim
	}
	if i, ok := index[name]; ok {
		return ServerColorByIndex(i)
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return ServerColorByIndex(int(h.Sum32() % uint32(len(serverPalette))))
}

// serverPulseFrames — последовательность стилей лампы работающего сервера.
// Циклически проходится по фазе анимации (animPhase) с шагом ~160 мс →
// полный цикл ≈ 640 мс. Только базовые ANSI-цвета для совместимости.
var serverPulseFrames = []lipgloss.Style{
	lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true), // bright green
	lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Bold(true),  // green
	lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Bold(true),  // dim gray
	lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Bold(true),  // green
}

// pulseStyle возвращает кадр пульса лампы по текущей фазе анимации.
func pulseStyle(phase int) lipgloss.Style {
	if phase < 0 {
		phase = -phase
	}
	return serverPulseFrames[phase%len(serverPulseFrames)]
}
