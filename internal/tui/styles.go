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

	// StylePerf — перф-метрики сервера (t_load · ↓in · ↑out) в шапке. Отдельный
	// teal-цвет, чтобы значения визуально отличались от прочего «тусклого»
	// контента (StyleDim) и читались как самостоятельный блок (v0.12.0).
	StylePerf = lipgloss.NewStyle().Foreground(colorTeal)

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

// serverPalette — 5 ярких ANSI цветов, стабильно отображаемых в cmd.exe / PowerShell /
// Windows Terminal без truecolor. Имя сервера → fnv32a(name) % 5 → цвет из палитры.
// Bright red (9) намеренно исключён: он зарезервирован под StyleServerFailed,
// чтобы провалившийся сервер визуально не сливался с обычным «красным» сервером.
var serverPalette = []lipgloss.Color{
	lipgloss.Color("10"), // bright green
	lipgloss.Color("11"), // yellow
	lipgloss.Color("12"), // bright blue
	lipgloss.Color("13"), // bright magenta
	lipgloss.Color("14"), // bright cyan
}

// ServerColor возвращает Style с Foreground из serverPalette, стабильным для name.
// Используется и в HeaderBar (chip), и в RequestTable (колонка Server).
func ServerColor(name string) lipgloss.Style {
	if name == "" {
		return StyleDim
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return lipgloss.NewStyle().Foreground(serverPalette[int(h.Sum32())%len(serverPalette)]).Bold(true)
}
