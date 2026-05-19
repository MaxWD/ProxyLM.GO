package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"

	"proxylm/internal/ipc"
)

// ---- helpers ----------------------------------------------------------------

func truncStr(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 1 {
		return string(runes[:n])
	}
	return string(runes[:n-1]) + "…"
}

func shortID(id string) string {
	id = strings.ReplaceAll(id, "-", "")
	if len(id) > 4 {
		return id[len(id)-4:]
	}
	return id
}

func fmtElapsed(r ipc.RequestState, now time.Time) string {
	switch r.Status {
	case "running", "in_progress":
		if r.StartedAt.IsZero() {
			return "—"
		}
		d := now.Sub(r.StartedAt)
		return fmtDuration(d)
	case "completed", "failed":
		if r.StartedAt.IsZero() || r.CompletedAt.IsZero() {
			return "—"
		}
		return fmtDuration(r.CompletedAt.Sub(r.StartedAt))
	default:
		return "—"
	}
}

func fmtDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

func fmtTokens(r ipc.RequestState) string {
	in := "—"
	out := "—"
	if r.PromptTokens > 0 {
		in = fmt.Sprintf("%d", r.PromptTokens)
	}
	switch r.Status {
	case "running", "in_progress":
		if r.Stream {
			out = "…"
		} else if r.OutputTokens > 0 {
			out = fmt.Sprintf("%d", r.OutputTokens)
		}
	default:
		if r.OutputTokens > 0 {
			out = fmt.Sprintf("%d", r.OutputTokens)
		}
	}
	return in + GlyphArrow() + out
}

// requestVisible возвращает true, если запрос должен отображаться.
// Completed/failed исчезают через 30 минут от completed_at.
func requestVisible(r ipc.RequestState, now time.Time, ttlMinutes int) bool {
	if ttlMinutes <= 0 {
		ttlMinutes = 30
	}
	switch r.Status {
	case "completed", "failed":
		if r.CompletedAt.IsZero() {
			return true
		}
		return now.Sub(r.CompletedAt) < time.Duration(ttlMinutes)*time.Minute
	}
	return true
}

// requestMatchesFilter возвращает true, если запрос проходит фильтр (case-insensitive).
func requestMatchesFilter(r ipc.RequestState, filter string) bool {
	if filter == "" {
		return true
	}
	f := strings.ToLower(filter)
	return strings.Contains(strings.ToLower(r.Model), f) ||
		strings.Contains(strings.ToLower(r.ClientName), f) ||
		strings.Contains(strings.ToLower(r.ServerName), f) ||
		strings.Contains(strings.ToLower(r.Status), f)
}

// RequestRowColor возвращает стиль строки запроса с учётом статуса и INV-2 контекста.
// needsSwap = true означает, что для данного запроса сервер придётся переключать модель.
func RequestRowColor(status string, needsSwap bool) lipgloss.Style {
	if needsSwap {
		return StyleNeedsSwap
	}
	switch status {
	case "running", "in_progress":
		return StyleStatusRunning
	case "queued", "pending":
		return StyleStatusQueued
	case "completed":
		return StyleStatusDone
	case "failed":
		return StyleStatusFailed
	default:
		return StyleDim
	}
}

// statusGlyph возвращает символ для статуса.
func statusGlyph(status string) string {
	switch status {
	case "running", "in_progress":
		return GlyphRunning()
	case "queued", "pending":
		return GlyphQueued()
	case "completed":
		return GlyphCompleted()
	case "failed":
		return GlyphFailed()
	default:
		return "?"
	}
}

// ---- sortRequests -----------------------------------------------------------

// sortedRequests возвращает список запросов, отсортированных для отображения.
// Три группы по статусу, общий порядок групп: running → queued → done.
//
// Внутри групп (v0.9.1, баг #89):
//   - running/in_progress: StartedAt ASC — задача, взятая в работу раньше,
//     выше; так оператор видит «самое старое в обработке» сверху;
//   - queued/pending: CreatedAt ASC — кто первым пришёл, тот выше (FIFO);
//   - completed/failed: CompletedAt DESC — последние завершённые сверху,
//     старые опускаются и в итоге пропадают по TTL.
//
// Tie-break по ID: без него запросы с одинаковыми таймстампами (разрешение
// snapshot'а — секунды/миллисекунды) могли «прыгать» местами между
// апдейтами. ID лексикографически стабилен.
func sortedRequests(reqs []ipc.RequestState) []ipc.RequestState {
	running := make([]ipc.RequestState, 0, len(reqs))
	queued := make([]ipc.RequestState, 0, len(reqs))
	done := make([]ipc.RequestState, 0, len(reqs))
	for _, r := range reqs {
		switch r.Status {
		case "running", "in_progress":
			running = append(running, r)
		case "queued", "pending":
			queued = append(queued, r)
		default:
			done = append(done, r)
		}
	}
	sortSlice(running, func(i, j int) bool {
		// StartedAt может быть zero, если ещё не выставлен; такие случаи
		// формально не должны попадать в running, но на всякий случай —
		// используем CreatedAt как fallback.
		ti := running[i].StartedAt
		if ti.IsZero() {
			ti = running[i].CreatedAt
		}
		tj := running[j].StartedAt
		if tj.IsZero() {
			tj = running[j].CreatedAt
		}
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return running[i].ID < running[j].ID
	})
	sortSlice(queued, func(i, j int) bool {
		if !queued[i].CreatedAt.Equal(queued[j].CreatedAt) {
			return queued[i].CreatedAt.Before(queued[j].CreatedAt)
		}
		return queued[i].ID < queued[j].ID
	})
	sortSlice(done, func(i, j int) bool {
		// CompletedAt DESC — позднее завершившиеся выше. Zero (если вдруг
		// не выставлен) уходит в конец.
		ti, tj := done[i].CompletedAt, done[j].CompletedAt
		if ti.IsZero() && !tj.IsZero() {
			return false
		}
		if !ti.IsZero() && tj.IsZero() {
			return true
		}
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return done[i].ID < done[j].ID
	})
	out := make([]ipc.RequestState, 0, len(running)+len(queued)+len(done))
	out = append(out, running...)
	out = append(out, queued...)
	out = append(out, done...)
	return out
}

// sortSlice is a simple insertion sort to avoid importing sort for small slices.
func sortSlice(s []ipc.RequestState, less func(i, j int) bool) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && less(j, j-1); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ---- HeaderBar --------------------------------------------------------------

// renderHeaderBar рендерит верхнюю часть TUI (серверы + счётчики).
// flashModels: map serverName -> true, если модель сменилась < 2 секунд назад.
// ttlMinutes: окно для счётчиков Done/Failed; синхронизировано с tui.show_completed_minutes.
// headerActive: true — шапка является активным pane (рамка ярче, выбранный сервер
// получает курсор). selectedIdx — индекс выбранного сервера (актуален при headerActive).
func renderHeaderBar(
	servers []ipc.ServerState,
	requests []ipc.RequestState,
	flashModels map[string]bool,
	ttlMinutes int,
	width int,
	headerActive bool,
	selectedIdx int,
) string {
	// Серверы — каждый на отдельной строке (имена моделей могут быть длинными,
	// одной строкой не помещается). Высота шапки растёт по числу серверов;
	// resizeViewports вычитает её из суммы и пропорционально уменьшает log-панель.
	var chipLines []string
	for i, s := range servers {
		selected := headerActive && i == selectedIdx
		chipLines = append(chipLines, renderServerChip(s, flashModels[s.Name], selected))
	}
	serversBlock := strings.Join(chipLines, "\n")

	// Последняя строка: счётчики. Окно «недавно завершённых» совпадает с TTL таблицы,
	// чтобы счётчики и видимые строки описывали одно и то же окно времени.
	if ttlMinutes <= 0 {
		ttlMinutes = 30
	}
	var queued, running, doneRecent, failed int
	now := time.Now()
	cutoff := now.Add(-time.Duration(ttlMinutes) * time.Minute)
	for _, r := range requests {
		switch r.Status {
		case "queued", "pending":
			queued++
		case "running", "in_progress":
			running++
		case "completed":
			if !r.CompletedAt.IsZero() && r.CompletedAt.After(cutoff) {
				doneRecent++
			}
		case "failed":
			if !r.CompletedAt.IsZero() && r.CompletedAt.After(cutoff) {
				failed++
			}
		}
	}
	totalServers := len(servers)
	healthyServers := 0
	for _, s := range servers {
		if s.Healthy {
			healthyServers++
		}
	}
	countersLine := fmt.Sprintf(
		"Queued: %s   Running: %s   Done/%dm: %s   Failed: %s   Servers: %s",
		StyleStatusQueued.Render(fmt.Sprintf("%d", queued)),
		StyleStatusRunning.Render(fmt.Sprintf("%d", running)),
		ttlMinutes,
		StyleStatusDone.Render(fmt.Sprintf("%d", doneRecent)),
		StyleStatusFailed.Render(fmt.Sprintf("%d", failed)),
		renderServerHealth(healthyServers, totalServers),
	)

	content := serversBlock + "\n" + countersLine

	style := StyleBorderInactive
	if headerActive {
		style = StyleBorderActive
	}
	return style.Width(width-2).Padding(0, 1).Render(content)
}

// headerHeight возвращает высоту HeaderBar в терминальных строках:
// 2 (рамка) + N серверов + 1 (счётчики). Минимум — 4 (при 0 серверах
// строка-плейсхолдер всё равно занимает 1 высоту).
func headerHeight(numServers int) int {
	rows := numServers
	if rows < 1 {
		rows = 1
	}
	return 2 + rows + 1
}

func renderServerChip(s ipc.ServerState, flash, selected bool) string {
	// Selected-режим: full-row highlight через StyleSelected (как у задач в
	// RequestTable). Вложенные стили (ServerColor, lampStyle, StyleFlash)
	// здесь не используем — иначе их \x1b[0m сбросит фон. Slow-алерт всё-таки
	// остаётся со своим стилем — yellow background и так контрастен с blue
	// selection, и оператору важно увидеть SLOW даже на выбранной строке.
	if selected {
		var lamp string
		if s.Healthy {
			lamp = GlyphHealthy()
		} else {
			lamp = GlyphUnhealthy()
		}
		model := s.CurrentModel
		if model == "" {
			model = "idle"
		}
		parts := []string{truncStr(s.Name, 12), lamp, model}
		if s.QueueDepth > 0 {
			parts = append(parts, fmt.Sprintf("[Q:%d]", s.QueueDepth))
		}
		if metric := renderServerMetricPlain(s); metric != "" {
			parts = append(parts, metric)
		}
		line := strings.Join(parts, " ")
		rendered := StyleSelected.Render(line)
		if s.Slow {
			rendered += " " + StyleSlow.Render(" !!! МЕДЛЕННО !!! ")
		}
		return rendered
	}

	// Не-selected: каждый кусочек со своим стилем.
	var lamp string
	var lampStyle lipgloss.Style
	if s.Healthy {
		lamp = GlyphHealthy()
		if s.InFlight {
			lampStyle = StyleServerInFlight
		} else {
			lampStyle = StyleServerHealthy
		}
	} else {
		lamp = GlyphUnhealthy()
		lampStyle = StyleServerUnhealthy
	}

	model := s.CurrentModel
	if model == "" {
		model = "idle"
	}
	modelStr := model
	if flash {
		modelStr = StyleFlash.Render(model)
	}

	queueInfo := ""
	if s.QueueDepth > 0 {
		queueInfo = StyleDim.Render(fmt.Sprintf("[Q:%d]", s.QueueDepth))
	}

	nameStr := ServerColor(s.Name).Render(truncStr(s.Name, 12))
	parts := []string{nameStr, lampStyle.Render(lamp), modelStr}
	if queueInfo != "" {
		parts = append(parts, queueInfo)
	}
	if metric := renderServerMetric(s); metric != "" {
		parts = append(parts, metric)
	}
	line := strings.Join(parts, " ")
	if s.Slow {
		line += " " + StyleSlow.Render(" !!! МЕДЛЕННО !!! ")
	}
	return line
}

// renderServerMetricPlain — версия renderServerMetric без StyleDim-окраски,
// для selected-режима (где вложенный стиль сломал бы общий фон). Возвращает
// строку без ANSI кроме glyph'ов (которые и так чистый UTF-8).
func renderServerMetricPlain(s ipc.ServerState) string {
	if !s.PerfOK || s.CurrentModel == "" {
		return ""
	}
	tLoadStr := "—"
	if s.TLoadMs > 0 {
		tLoadStr = fmtMs(int64(s.TLoadMs))
	}
	in := fmtTokPerSec(s.TokInPerSec)
	out := fmtTokPerSec(s.TokOutPerSec)
	return tLoadStr + " · " + GlyphTokIn() + in + " · " + GlyphTokOut() + out
}

// renderServerMetric форматирует строку производительности для текущей модели
// сервера. Формат: "t_load · ↓N tok/s · ↑M tok/s" — три значения регрессии.
// Если регрессия не отдала результат (PerfOK=false, samples < 3, либо модель
// не загружена) — пустая строка.
//
// Если ни одной задачи на этой модели не сопровождалась reload'ом
// (PerfOK=true, но TLoadMs==0), часть t_load заменяется на «—».
func renderServerMetric(s ipc.ServerState) string {
	if !s.PerfOK || s.CurrentModel == "" {
		return ""
	}
	tLoadStr := "—"
	if s.TLoadMs > 0 {
		tLoadStr = fmtMs(int64(s.TLoadMs))
	}
	in := fmtTokPerSec(s.TokInPerSec)
	out := fmtTokPerSec(s.TokOutPerSec)
	return StyleDim.Render(tLoadStr + " · " + GlyphTokIn() + in + " · " + GlyphTokOut() + out)
}

// fmtTokPerSec форматирует tok/s; для невалидных значений — прочерк.
func fmtTokPerSec(v float64) string {
	if v <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f tok/s", v)
}

// fmtMs форматирует миллисекунды в человекочитаемое значение:
//
//	<1000 ms → "850ms"
//	≥1 s     → "1.2s"
func fmtMs(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.1fs", float64(ms)/1000.0)
}

// fmtQueueWait форматирует время ожидания в очереди адаптивно (баг #83, v0.9.0):
//
//	<100 ms        → "N ms"           (точно, как в логах)
//	100 ms..60 s   → "N.N s"          (привычный «несколько секунд»)
//	≥60 s          → "M:SS m"         (минуты:секунды, человекочитаемо)
//
// Используется в InfoPane.Request для поля "Queue wait".
func fmtQueueWait(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	if ms < 100 {
		return fmt.Sprintf("%d ms", ms)
	}
	if ms < 60_000 {
		return fmt.Sprintf("%.1f s", float64(ms)/1000.0)
	}
	minutes := ms / 60_000
	seconds := (ms % 60_000) / 1000
	return fmt.Sprintf("%d:%02d m", minutes, seconds)
}

func renderServerHealth(healthy, total int) string {
	s := fmt.Sprintf("%d/%d healthy", healthy, total)
	if healthy == total && total > 0 {
		return StyleServerHealthy.Render(s)
	}
	if healthy == 0 {
		return StyleServerUnhealthy.Render(s)
	}
	return StyleFlash.Render(s)
}

// ---- RequestTable -----------------------------------------------------------

// colWidths определяет ширины колонок RequestTable.
// Слева от первой колонки всегда есть 2-char маркер (▶ для running, иначе пробелы) —
// он не входит в этот struct и добавляется при сборке строки.
type colWidths struct {
	id       int
	client   int
	model    int
	server   int
	status   int
	rm       int // ModelReloaded: ✓ / —
	queued   int // created_at HH:MM:SS — добавлена в v0.3 (постановка в очередь)
	started  int // started_at HH:MM:SS — момент передачи в LLM
	elapsed  int
	tokens   int
	endpoint int // только при width >= 200
}

// calcColWidths вычисляет ширины колонок RequestTable с учётом ширины терминала.
// Базовый набор колонок (~112 символов без маркера): при termWidth < 112 пропорционально
// сужаем model (минимум 18) и tokens (минимум 9), чтобы не допустить молчаливого overflow.
func calcColWidths(termWidth int, showEndpoint bool) colWidths {
	cw := colWidths{
		id:     4,
		client: 9,
		// model: 18 → 27 (×1.5, v0.9.2). Имена локальных моделей вроде
		// «qwen2.5-coder-14b-instruct» или «llama-3.1-8b-instruct-q4_k_m»
		// обрезались по середине; 27 вмещает большинство канонических имён.
		model: 27,
		// server: +2 для "✗ " префикса failed-сервера; имя до 8 символов.
		server:  10,
		status:  12,
		rm:      2,
		queued:  8,
		started: 8,
		elapsed: 7,
		tokens:  11,
	}
	if showEndpoint {
		cw.endpoint = 18
	}

	// fullWidth — сумма всех базовых колонок + пробелы + 2-char маркер + border(4).
	// 2 (marker) + 4+1 + 9+1 + 27+1 + 10+1 + 12+1 + 2+1 + 8+1 + 8+1 + 7+1 + 11 + 4(border) = ~116
	// Когда termWidth ≥ 100 проверяем запас; при нехватке сжимаем model и tokens.
	if termWidth >= 100 {
		const fixedCols = 2 + (4 + 1) + (9 + 1) + (10 + 1) + (12 + 1) + (2 + 1) + (8 + 1) + (8 + 1) + (7 + 1) + 4
		// fixedCols = 2+5+10+11+13+3+9+9+8+4 = 74; remaining for model+tokens+space = termWidth-74
		remaining := termWidth - fixedCols - 1 // -1 between model and server
		const minModel = 18
		const minTokens = 9
		minRequired := minModel + 1 + cw.tokens // space between model and tokens
		if remaining < cw.model+1+cw.tokens {
			// Сжимаем: сначала model до minModel, потом tokens до minTokens.
			if remaining >= minModel+1+cw.tokens {
				cw.model = remaining - 1 - cw.tokens
			} else if remaining >= minModel+1+minTokens {
				cw.model = minModel
				cw.tokens = remaining - 1 - minModel
			} else if remaining >= minRequired {
				cw.model = minModel
				cw.tokens = minTokens
			}
			// Если даже minRequired не влезает — оставляем минимумы,
			// renderRequestTable всё равно обрежет через truncStr.
		}
	}
	return cw
}

// renderRequestTable рендерит таблицу запросов в виде строки.
// vp — viewport, управляющий прокруткой. Метод заполняет его контентом и возвращает View().
func renderRequestTable(
	vp *viewport.Model,
	requests []ipc.RequestState,
	servers []ipc.ServerState,
	selectedIdx int,
	filter string,
	width int,
	activePane paneID,
	now time.Time,
	ttlMinutes int,
) string {
	showEndpoint := width >= 200
	compact := width < 100
	cw := calcColWidths(width, showEndpoint)

	// Заголовки.
	var header string
	if !compact {
		header = renderTableHeader(cw, showEndpoint)
	}

	// Фильтрация и сортировка.
	sorted := sortedRequests(requests)
	visible := make([]ipc.RequestState, 0, len(sorted))
	for _, r := range sorted {
		if requestVisible(r, now, ttlMinutes) && requestMatchesFilter(r, filter) {
			visible = append(visible, r)
		}
	}

	// Строки.
	// Selection виден только когда активен paneRequests — это решение единого
	// фокуса (см. план v0.8): если оператор перешёл в paneInfo / paneHeader,
	// фон-курсор в таблице исчезает, чтобы не отвлекать.
	var rowLines []string
	for i, r := range visible {
		needsSwap := calcNeedsSwap(r, servers)
		isSelected := i == selectedIdx && activePane == paneRequests
		line := renderRequestRow(r, cw, showEndpoint, compact, isSelected, needsSwap, now)
		rowLines = append(rowLines, line)
	}

	var sb strings.Builder
	if header != "" {
		sb.WriteString(header)
		sb.WriteByte('\n')
	}
	sb.WriteString(strings.Join(rowLines, "\n"))

	vp.SetContent(sb.String())

	// Рамка вокруг viewport.
	var borderStyle lipgloss.Style
	if activePane == paneRequests {
		borderStyle = StyleBorderActive
	} else {
		borderStyle = StyleBorderInactive
	}
	title := StyleTitle.Render("Requests")
	if filter != "" {
		title += StyleDim.Render(fmt.Sprintf("  filter: %s", filter))
	}
	inner := vp.View()
	return borderStyle.Width(width - 2).Render(title + "\n" + inner)
}

func renderTableHeader(cw colWidths, showEndpoint bool) string {
	parts := []string{
		pad("#", cw.id),
		pad("Client", cw.client),
		pad("Model", cw.model),
		pad("Server", cw.server),
		pad("Status", cw.status),
		pad("RM", cw.rm),
		pad("Queued", cw.queued),
		pad("Started", cw.started),
		pad("Elapsed", cw.elapsed),
		pad("I→O tok", cw.tokens),
	}
	if showEndpoint {
		parts = append(parts, pad("Endpoint", cw.endpoint))
	}
	// "  " — резерв под 2-char маркер (▶ или пробелы) в начале каждой data-строки.
	return StyleColHeader.Render("  " + strings.Join(parts, " "))
}

// rmCell возвращает значение колонки RM. Прочерк для незавершённых/не запускавшихся
// и для тех, у кого reload модели не происходил. Галочка только когда сервер
// реально переключал current_model в начале задачи.
func rmCell(r ipc.RequestState) string {
	if r.Status == "queued" || r.Status == "pending" {
		return "—"
	}
	if r.ModelReloaded {
		return GlyphCheck()
	}
	return "—"
}

// serverDisplay решает, что показать в колонке Server и нужен ли failed-стиль.
//
// Возвращает (display_name, isFailed):
//   - failed-status                              → "✗ ServerName"  (failed)
//   - LastFailedServer != "" и ServerName пуст / совпадает с LastFailedServer
//     (между retry-попытками)                    → "✗ LastFailedServer" (failed)
//   - всё остальное (в т.ч. новая попытка после fail на другом сервере) →
//     "ServerName" (обычный).
//
// Полная история failed-серверов отображается в InfoPane.
func serverDisplay(r ipc.RequestState) (string, bool) {
	if r.Status == "failed" {
		name := r.ServerName
		if name == "" {
			name = r.LastFailedServer
		}
		return GlyphFailedServer() + " " + truncStr(name, 8), true
	}
	if r.LastFailedServer != "" && (r.ServerName == "" || r.ServerName == r.LastFailedServer) {
		return GlyphFailedServer() + " " + truncStr(r.LastFailedServer, 8), true
	}
	return truncStr(r.ServerName, 10), false
}

func renderRequestRow(
	r ipc.RequestState,
	cw colWidths,
	showEndpoint bool,
	compact bool,
	selected bool,
	needsSwap bool,
	now time.Time,
) string {
	rowStyle := RequestRowColor(r.Status, needsSwap)
	running := r.Status == "running" || r.Status == "in_progress"

	// 2-char маркер: '▶ ' для running, иначе '  '. Никогда не попадает в truncStr/pad —
	// добавляется снаружи, поэтому ANSI escapes от StyleStatusRunning не ломают вёрстку.
	marker := "  "
	if running {
		marker = StyleStatusRunning.Render(GlyphRunning()) + " "
	}

	idStr := shortID(r.ID)
	// statusPlain — статус БЕЗ ANSI (для truncate/pad).
	statusPlain := statusGlyph(r.Status) + " " + r.Status
	// retrySuffix — добавляется ПОСЛЕ pad'а, чтобы ANSI-escapes не попали в truncStr/pad.
	var retrySuffix string
	if running && r.Attempt > 1 && r.MaxAttempts > 0 {
		retrySuffix = " " + StyleRetry.Render(fmt.Sprintf("(%d/%d)", r.Attempt, r.MaxAttempts))
	}

	queuedStr := "—"
	if !r.CreatedAt.IsZero() {
		queuedStr = r.CreatedAt.Local().Format("15:04:05")
	}
	startedStr := "—"
	if !r.StartedAt.IsZero() {
		startedStr = r.StartedAt.Local().Format("15:04:05")
	}

	modelStr := r.Model
	if needsSwap {
		modelStr = r.Model + "~"
	}

	if compact {
		// Compact (width<100): минимум колонок; маркер тоже виден. Для failed-сервера
		// 6-ширинная колонка не вмещает "✗ name", поэтому ставим короткий "!" префикс.
		srvCompact := truncStr(r.ServerName, 6)
		_, srvFailedCompact := serverDisplay(r)
		if srvFailedCompact {
			n := r.ServerName
			if r.Status != "failed" {
				n = r.LastFailedServer
			}
			srvCompact = "!" + truncStr(n, 5)
		}
		plain := fmt.Sprintf("%s %s %s %s %s %s",
			pad(idStr, cw.id),
			pad(truncStr(r.ClientName, 8), 8),
			pad(truncStr(modelStr, 14), 14),
			pad(srvCompact, 6),
			pad(truncStr(r.Status, 10), 10),
			fmtElapsed(r, now),
		)
		if selected {
			return marker + StyleSelected.Render(plain)
		}
		return marker + rowStyle.Render(plain)
	}

	// Plain версии полей (для selected-режима — один общий StyleSelected на всю строку).
	clientPlain := truncStr(r.ClientName, cw.client)
	modelPlain := truncStr(modelStr, cw.model)
	srvName, srvFailed := serverDisplay(r)
	serverPlain := pad(srvName, cw.server)
	statusPadded := pad(truncStr(statusPlain, cw.status), cw.status)
	rmStr := rmCell(r)

	if selected {
		// Курсор: непрерывная фоновая подсветка на всю строку, включая
		// разделительные пробелы и server-сегмент. Failed-сервер сохраняет
		// красный+bold (главный сигнал), но получает синий фон Selected
		// — иначе вокруг него были чёрные «дырки» (бага #87, v0.9.1).
		leftParts := []string{
			pad(idStr, cw.id),
			pad(clientPlain, cw.client),
			pad(modelPlain, cw.model),
		}
		rightParts := []string{
			statusPadded,
			pad(rmStr, cw.rm),
			pad(queuedStr, cw.queued),
			pad(startedStr, cw.started),
			pad(fmtElapsed(r, now), cw.elapsed),
			pad(fmtTokens(r), cw.tokens),
		}
		if showEndpoint {
			rightParts = append(rightParts, pad(truncStr(r.Endpoint, cw.endpoint), cw.endpoint))
		}
		var serverSeg string
		if srvFailed {
			// StyleSelected.Foreground/Bold возвращает копию (lipgloss.Style —
			// value-type), оригинал не модифицируется. Background colorBlue
			// наследуется из StyleSelected, foreground/bold переопределяются.
			serverSeg = StyleSelected.Foreground(colorBrightRed).Bold(true).Render(serverPlain)
		} else {
			serverSeg = StyleSelected.Render(serverPlain)
		}
		return marker +
			StyleSelected.Render(strings.Join(leftParts, " ")+" ") +
			serverSeg +
			StyleSelected.Render(" "+strings.Join(rightParts, " ")) +
			retrySuffix
	}

	// Не-selected: серверная колонка раскрашивается ServerColor (стабильный hash)
	// или StyleServerFailed при провале; остальные — общим rowStyle. Каждая
	// колонка рендерится отдельно, иначе вложенный \x1b[0m сбросит rowStyle.
	var serverColored string
	if srvFailed {
		serverColored = StyleServerFailed.Render(serverPlain)
	} else {
		serverColored = ServerColor(r.ServerName).Render(serverPlain)
	}
	styledParts := []string{
		rowStyle.Render(pad(idStr, cw.id)),
		rowStyle.Render(pad(clientPlain, cw.client)),
		rowStyle.Render(pad(modelPlain, cw.model)),
		serverColored,
		rowStyle.Render(statusPadded),
		rowStyle.Render(pad(rmStr, cw.rm)),
		rowStyle.Render(pad(queuedStr, cw.queued)),
		rowStyle.Render(pad(startedStr, cw.started)),
		rowStyle.Render(pad(fmtElapsed(r, now), cw.elapsed)),
		rowStyle.Render(pad(fmtTokens(r), cw.tokens)),
	}
	if showEndpoint {
		styledParts = append(styledParts, rowStyle.Render(pad(truncStr(r.Endpoint, cw.endpoint), cw.endpoint)))
	}
	return marker + strings.Join(styledParts, " ") + retrySuffix
}

// calcNeedsSwap возвращает true, если для данного запроса сервер (если назначен)
// потребует swap модели: нет сервера с current_model == r.Model.
func calcNeedsSwap(r ipc.RequestState, servers []ipc.ServerState) bool {
	switch r.Status {
	case "queued", "pending":
	default:
		return false
	}
	// Проверяем: есть ли хотя бы один healthy сервер с current_model == r.Model.
	for _, s := range servers {
		if s.Healthy && s.CurrentModel == r.Model {
			return false
		}
	}
	return true
}

// pad добавляет пробелы справа до ширины n (по рунам).
func pad(s string, n int) string {
	runes := []rune(s)
	if len(runes) >= n {
		return string(runes[:n])
	}
	return s + strings.Repeat(" ", n-len(runes))
}

// ---- FilterBar --------------------------------------------------------------

// renderFilterBar рендерит строку ввода фильтра.
func renderFilterBar(ti textinput.Model, width int) string {
	prompt := StyleTitle.Render("Filter: ")
	inner := prompt + ti.View()
	return StyleBorderInactive.Width(width-2).Padding(0, 1).Render(inner)
}

// ---- InfoPane ---------------------------------------------------------------
//
// InfoPane заменил LogPane в v0.8.0. Это двухколоночная панель деталей
// последнего выбранного элемента (request / server). Модальные окна (Request
// Details / Server Details) больше не используются — вся информация всегда
// видна оператору в нижней трети экрана.
//
// Состояния:
//   - infoKindEmpty   — ничего ещё не выбиралось, показывается hint.
//   - infoKindRequest — последним выбран был request (стрелки/клик в paneRequests).
//   - infoKindServer  — последним выбран был server  (стрелки/клик в paneHeader).
//
// Содержимое не привязано к активному pane: даже при Tab → paneInfo
// последнее выбранное всё ещё видно. Это позволяет оператору перейти к
// «чтению» без потери контекста.

// infoKind — какой набор данных показывать в InfoPane.
type infoKind int

const (
	infoKindEmpty infoKind = iota
	infoKindRequest
	infoKindServer
)

// renderInfoPane рендерит панель деталей. req / srv — указатели на последний
// выбранный элемент (могут быть nil даже при infoKindRequest, если запись ушла
// из снапшота по TTL).
func renderInfoPane(
	vp *viewport.Model,
	activePane paneID,
	kind infoKind,
	req *ipc.RequestState,
	srv *ipc.ServerState,
	width int,
) string {
	innerW := width - 6 // border (2) + padding (2) + safety (2)
	if innerW < 40 {
		innerW = 40
	}

	var title, content string
	switch {
	case kind == infoKindRequest && req != nil:
		title = "Info — Request " + shortID(req.ID)
		content = renderInfoRequest(*req, innerW)
	case kind == infoKindServer && srv != nil:
		title = "Info — Server " + srv.Name
		content = renderInfoServer(*srv, innerW)
	default:
		title = "Info"
		content = StyleDim.Render("Tab — переключение pane    ←↑↓→ — навигация    Enter/Click — выбор")
	}
	vp.SetContent(content)

	var borderStyle lipgloss.Style
	if activePane == paneInfo {
		borderStyle = StyleBorderActive
	} else {
		borderStyle = StyleBorderInactive
	}
	titleStr := StyleTitle.Render(title)
	if srv != nil && kind == infoKindServer && srv.Slow {
		titleStr += "   " + StyleSlow.Render(" !!! МЕДЛЕННО !!! ")
	}
	return borderStyle.Width(width - 2).Render(titleStr + "\n" + vp.View())
}

// renderInfoRequest рендерит детали запроса в двухколоночной сетке.
// innerW — доступная ширина (внутренняя, без рамки).
func renderInfoRequest(r ipc.RequestState, innerW int) string {
	labelW := 12
	halfW := innerW / 2
	if halfW < labelW+10 {
		halfW = labelW + 10
	}
	valW := halfW - labelW - 1
	if valW < 8 {
		valW = 8
	}

	streamStr := "no"
	if r.Stream {
		streamStr = "yes"
	}
	statusStr := r.Status
	if r.Attempt > 0 && r.MaxAttempts > 0 {
		statusStr += fmt.Sprintf(" (%d/%d)", r.Attempt, r.MaxAttempts)
	}
	serverStr := r.ServerName
	if r.LastFailedServer != "" {
		switch {
		case r.Status == "failed":
			serverStr = StyleServerFailed.Render(GlyphFailedServer()+" "+r.ServerName) +
				" prev: " + StyleServerFailed.Render(r.LastFailedServer)
		case r.ServerName == "" || r.ServerName == r.LastFailedServer:
			serverStr = StyleServerFailed.Render(GlyphFailedServer() + " " + r.LastFailedServer)
		default:
			serverStr = r.ServerName + " (" + StyleServerFailed.Render(GlyphFailedServer()+" "+r.LastFailedServer) + ")"
		}
	} else if r.Status == "failed" {
		serverStr = StyleServerFailed.Render(GlyphFailedServer() + " " + r.ServerName)
	}
	rmStr := "—"
	if r.ModelReloaded {
		rmStr = GlyphCheck()
	}

	rows := [][2][2]string{
		{{"ID", r.ID}, {"Created", fmtTime(r.CreatedAt)}},
		{{"Client", r.ClientName}, {"Started", fmtTime(r.StartedAt)}},
		{{"Model", r.Model}, {"Completed", fmtTime(r.CompletedAt)}},
		{{"Endpoint", r.Endpoint}, {"Queue wait", fmtQueueWait(r.QueueWaitMs)}},
		{{"Stream", streamStr}, {"Prompt tok", fmt.Sprintf("%d", r.PromptTokens)}},
		{{"Server", serverStr}, {"Output tok", fmt.Sprintf("%d", r.OutputTokens)}},
		{{"Status", statusStr}, {"RM", rmStr}},
	}
	var sb strings.Builder
	for _, row := range rows {
		leftL, leftV := row[0][0], row[0][1]
		rightL, rightV := row[1][0], row[1][1]
		sb.WriteString(StyleColHeader.Render(pad(leftL, labelW)))
		sb.WriteString(" ")
		sb.WriteString(padDisplay(leftV, valW))
		sb.WriteString("  ")
		sb.WriteString(StyleColHeader.Render(pad(rightL, labelW)))
		sb.WriteString(" ")
		sb.WriteString(padDisplay(rightV, valW))
		sb.WriteByte('\n')
	}
	if r.ErrorMessage != "" {
		sb.WriteByte('\n')
		sb.WriteString(StyleColHeader.Render("Error:"))
		sb.WriteString(" ")
		sb.WriteString(StyleStatusFailed.Render(truncStr(r.ErrorMessage, innerW-7)))
	}
	return sb.String()
}

// renderInfoServer рендерит детали сервера + per-model таблицу.
func renderInfoServer(s ipc.ServerState, innerW int) string {
	labelW := 12
	halfW := innerW / 2
	if halfW < labelW+10 {
		halfW = labelW + 10
	}
	valW := halfW - labelW - 1
	if valW < 8 {
		valW = 8
	}

	healthyStr := StyleStatusFailed.Render("down")
	if s.Healthy {
		healthyStr = StyleStatusDone.Render("healthy")
	}
	current := s.CurrentModel
	if current == "" {
		current = "idle"
	}
	tLoadStr := "—"
	tokIn := "—"
	tokOut := "—"
	if s.PerfOK {
		if s.TLoadMs > 0 {
			tLoadStr = fmtMs(int64(s.TLoadMs))
		}
		if s.TokInPerSec > 0 {
			tokIn = fmt.Sprintf("%.1f tok/s", s.TokInPerSec)
		}
		if s.TokOutPerSec > 0 {
			tokOut = fmt.Sprintf("%.1f tok/s", s.TokOutPerSec)
		}
	}

	failedStr := fmt.Sprintf("%d", s.FailureCount)
	if s.FailureCount > 0 {
		failedStr = StyleStatusFailed.Render(failedStr)
	}
	// Fit-индикатор для header-метрики: R² и качественная оценка
	// (good / degraded). Помогает оператору понять, насколько надёжна
	// текущая точечная оценка t_load / tok/s. См. FUTURE.md #7 → v0.10.0.
	fitStr := "—"
	if s.PerfOK {
		fitStr = renderFitIndicator(s.RSquared, s.FitQuality)
	}
	rows := [][2][2]string{
		{{"Server", s.Name}, {"TL", tLoadStr}},
		{{"URL", s.URL}, {GlyphTokIn() + " in", tokIn}},
		{{"Health", healthyStr}, {GlyphTokOut() + " out", tokOut}},
		{{"Current", current}, {"Samples", fmt.Sprintf("%d", s.PerfSamples)}},
		{{"Queue", fmt.Sprintf("%d", s.QueueDepth)}, {"Fit", fitStr}},
		{{"Models", fmt.Sprintf("%d discovered", len(s.Models))}, {"Failed", failedStr}},
	}
	var sb strings.Builder
	for _, row := range rows {
		leftL, leftV := row[0][0], row[0][1]
		rightL, rightV := row[1][0], row[1][1]
		sb.WriteString(StyleColHeader.Render(pad(leftL, labelW)))
		sb.WriteString(" ")
		sb.WriteString(padDisplay(leftV, valW))
		sb.WriteString("  ")
		sb.WriteString(StyleColHeader.Render(pad(rightL, labelW)))
		sb.WriteString(" ")
		sb.WriteString(padDisplay(rightV, valW))
		sb.WriteByte('\n')
	}

	// Per-(model, endpoint) таблица (v0.10.0, FUTURE.md #3/#7).
	// Endpoint выводится отдельной колонкой; R² + CI — диагностика fit'а.
	sb.WriteByte('\n')
	sb.WriteString(StyleColHeader.Render("Per-model performance"))
	sb.WriteByte('\n')
	if len(s.PerModelStats) == 0 {
		sb.WriteString(StyleDim.Render("  (no completed requests yet)"))
		return sb.String()
	}
	header := fmt.Sprintf("  %-20s %-16s %5s %4s %7s %12s %12s %5s",
		"Model", "Endpoint", "Reqs", "Ld", "TL", "↓tok/s", "↑tok/s", "R²")
	sb.WriteString(StyleColHeader.Render(header))
	sb.WriteByte('\n')
	for _, ms := range s.PerModelStats {
		tLoad := "—"
		ti := "—"
		to := "—"
		r2 := "—"
		if ms.OK {
			if ms.TLoadMs > 0 {
				tLoad = fmtMs(int64(ms.TLoadMs))
			}
			if ms.TokInPerSec > 0 {
				if ms.KInCI > 0 {
					ci := tokPerSecCIHalf(ms.TokInPerSec, ms.KInCI)
					ti = fmt.Sprintf("%.1f±%.1f", ms.TokInPerSec, ci)
				} else {
					ti = fmt.Sprintf("%.1f", ms.TokInPerSec)
				}
			}
			if ms.TokOutPerSec > 0 {
				if ms.KOutCI > 0 {
					ci := tokPerSecCIHalf(ms.TokOutPerSec, ms.KOutCI)
					to = fmt.Sprintf("%.1f±%.1f", ms.TokOutPerSec, ci)
				} else {
					to = fmt.Sprintf("%.1f", ms.TokOutPerSec)
				}
			}
			r2 = fmt.Sprintf("%.2f", ms.RSquared)
		}
		row := fmt.Sprintf("  %-20s %-16s %5d %4d %7s %12s %12s %5s",
			truncStr(ms.Model, 20), truncStr(displayEndpoint(ms.Endpoint), 16),
			ms.Samples, ms.Loaded, tLoad, ti, to, r2)
		// Подсвечиваем строки с degraded fit, чтобы оператор видел, какие
		// оценки сейчас нельзя считать надёжными.
		if ms.OK && ms.FitQuality == "degraded" {
			row = StyleFlash.Render(row)
		}
		sb.WriteString(row)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// renderFitIndicator форматирует «R² + словесная оценка» одной строкой для
// header-секции server-info: например, "0.94 good" или "0.42 degraded".
// При degraded строка подсвечивается StyleFlash, чтобы привлечь внимание.
func renderFitIndicator(r2 float64, quality string) string {
	if quality == "" {
		return "—"
	}
	text := fmt.Sprintf("%.2f %s", r2, quality)
	if quality == "degraded" {
		return StyleFlash.Render(text)
	}
	return text
}

// displayEndpoint сокращает имена /v1/* для табличного отображения. Возвращает
// "chat", "completions", "embeddings" и т. п. при стандартных путях, иначе —
// исходную строку. Пустая строка маппится в "—" (статистика, накопленная до
// миграции на per-endpoint ключи).
func displayEndpoint(ep string) string {
	switch ep {
	case "":
		return "—"
	case "/v1/chat/completions":
		return "chat"
	case "/v1/completions":
		return "completions"
	case "/v1/embeddings":
		return "embeddings"
	case "/v1/responses":
		return "responses"
	}
	return ep
}

// tokPerSecCIHalf переводит half-width 95% CI коэффициента регрессии
// (мс/токен) в тот же диапазон, выраженный в tok/s. Поскольку tok/s = 1000/k
// — функция нелинейная, точное преобразование — половина разницы между
// 1000/(k−Δk) и 1000/(k+Δk). Для умеренных Δk/k (< 50%) этот результат
// почти равен 1000·Δk/k², но мы считаем точно во избежание перекосов.
func tokPerSecCIHalf(tokPerSec, kCI float64) float64 {
	if tokPerSec <= 0 || kCI <= 0 {
		return 0
	}
	k := 1000.0 / tokPerSec
	lower := k + kCI
	upper := k - kCI
	if upper <= 0 {
		// CI «прокрутил» k в ноль/отрицательное — отдаём asymmetric верхнюю
		// границу, чтобы не делить на ноль. На практике редкий случай при
		// очень малой выборке.
		return tokPerSec
	}
	return (1000.0/upper - 1000.0/lower) / 2.0
}

// padDisplay — pad ИЛИ truncate с учётом ANSI-escape'ов. Для plain-строк
// работает через pad/truncStr по рунам. Для строк с ANSI-стилями (lipgloss
// Render) считает visible width через lipgloss.Width и дополняет пробелами
// до n. Без этого ANSI-значение (например, healthyStr) ломало выравнивание
// двухколоночной сетки в renderInfoServer/Request — правый столбец уезжал
// влево на ширину «не-добитого» значения (баг #79, v0.9.0).
func padDisplay(s string, n int) string {
	if !strings.Contains(s, "\x1b[") {
		return pad(truncStr(s, n), n)
	}
	w := lipgloss.Width(s)
	if w >= n {
		return s
	}
	return s + strings.Repeat(" ", n-w)
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// ---- Footer -----------------------------------------------------------------

func renderFooter(showFilter bool, width int) string {
	var text string
	if showFilter {
		text = "Esc Cancel filter   Enter Confirm"
	} else if width < 80 {
		// Narrow terminal — show minimal hints only.
		text = "F1 Help · q Quit"
	} else {
		text = "F1 Help   F5 Refresh   / Filter   Tab Header/Requests/Info   Click — выбор   q/F10 Quit"
	}
	return StyleFooter.Width(width - 2).Render(text)
}

// ---- TooNarrow --------------------------------------------------------------

func renderTooNarrow() string {
	return StyleError.Render("terminal too narrow (min 60)")
}

// ---- HelpOverlay ------------------------------------------------------------

// helpItem описывает одну строку в help overlay.
type helpItem struct {
	key  string
	desc string
}

var helpItems = []helpItem{
	{"F1", "help (toggle this overlay)"},
	{"F5", "refresh (request snapshot from daemon)"},
	{"Tab", "switch pane (Header → Requests → Info)"},
	{"↑/k", "navigate up"},
	{"↓/j", "navigate down"},
	{"PgUp/PgDn", "page up / page down"},
	{"Home/End", "jump to first / last row"},
	{"Enter", "show details in Info pane"},
	{"/", "filter requests (model / client / server / status)"},
	{"Esc", "cancel filter / close overlay"},
	{"mouse wheel", "scroll pane under cursor; select server in header"},
	{"q / F10", "quit"},
}

// renderHelpOverlay возвращает help overlay, отцентрированный в терминале
// шириной termW и высотой termH.
func renderHelpOverlay(termW, termH int) string {
	// Строим содержимое: заголовок + строки хоткеев.
	var sb strings.Builder
	sb.WriteString(StyleModalTitle.Render("Keyboard shortcuts"))
	sb.WriteString("\n\n")

	// Ширина колонки key = максимальная длина key + 2.
	maxKey := 0
	for _, it := range helpItems {
		if len(it.key) > maxKey {
			maxKey = len(it.key)
		}
	}
	keyW := maxKey + 2

	for _, it := range helpItems {
		sb.WriteString(StyleHelpKey.Render(pad(it.key, keyW)))
		sb.WriteString("  ")
		sb.WriteString(StyleHelpDesc.Render(it.desc))
		sb.WriteByte('\n')
	}
	sb.WriteString("\n")
	sb.WriteString(StyleDim.Render("Press F1, Esc or q to close"))

	content := StyleHelpBorder.Render(sb.String())

	// Центрируем в экране.
	return lipgloss.Place(termW, termH, lipgloss.Center, lipgloss.Center, content)
}

// ---- LoadingPane ------------------------------------------------------------

// renderLoadingPane отображается в request-панели до прихода первого snapshot.
func renderLoadingPane(addr string, width int, activePane paneID) string {
	msg := StyleDim.Render("Waiting for daemon…")
	if addr != "" {
		msg += " " + StyleDim.Render("(connected to "+addr+")")
	}
	// Минимальный placeholder во viewport.
	vp := viewport.Model{}
	vp.Width = width - 4
	vp.Height = 3
	vp.SetContent(msg)

	var borderStyle lipgloss.Style
	if activePane == paneRequests {
		borderStyle = StyleBorderActive
	} else {
		borderStyle = StyleBorderInactive
	}
	return borderStyle.Width(width - 2).Render(StyleTitle.Render("Requests") + "\n" + vp.View())
}
