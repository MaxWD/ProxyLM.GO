package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"proxylm/internal/ipc"
)

// paneID identifies which pane is currently active for keyboard scrolling.
type paneID int

const (
	paneHeader paneID = iota
	paneRequests
	paneInfo
)

// connState — состояние WebSocket-соединения с daemon'ом.
type connState int

const (
	connStateConnecting   connState = iota // первое подключение
	connStateLive                          // подключён, данные приходят
	connStateReconnecting                  // соединение оборвалось, идёт реконнект
)

// flashEntry tracks when a server's current_model last changed, for 2-second yellow flash.
type flashEntry struct {
	flashAt time.Time
}

// model is the Bubble Tea Model. All state is held here; Update is a pure function.
type model struct {
	// connection
	client  *ipc.Client
	connURL string // адрес daemon'а, для отображения в loading pane

	// ctx/cancel для остановки фоновых горутин при выходе из TUI.
	ctx    context.Context
	cancel context.CancelFunc

	// состояние соединения
	connStatus   connState
	reconnecting bool // true — в данный момент происходит реконнект

	// daemon state
	version          string
	servers          []ipc.ServerState
	requests         map[string]ipc.RequestState // keyed by request ID
	snapshotReceived bool                        // true после первого state_snapshot
	connError        string

	// последняя ошибка парсинга фрейма — показывается в footer
	lastParseError string

	// UI layout
	width  int
	height int

	// active pane for scrolling
	activePane paneID

	// viewports
	reqViewport  viewport.Model
	infoViewport viewport.Model

	// selected row in request table (index into visible sorted list)
	selectedIdx int

	// selected server in header (index into m.servers)
	selectedServerIdx int

	// model flash tracking: serverName -> flashEntry
	flashMap map[string]flashEntry

	// animPhase — счётчик фаз анимации пульса лампы работающих серверов.
	// Инкрементируется animTick'ом (~160ms). animRunning защищает от запуска
	// нескольких параллельных тикеров: тик работает только пока есть in-flight
	// сервер, иначе останавливается (экономия перерисовок на простое).
	animPhase   int
	animRunning bool

	// filter
	filterActive bool
	filterInput  textinput.Model
	filterText   string

	// InfoPane — режим и идентификаторы последнего выбранного элемента.
	// infoKind держит «что именно показывать» (empty / request / server);
	// конкретные RequestState/ServerState ищутся по ID / Name в render() —
	// это устойчиво к TTL-исчезновению записей из снапшота.
	infoKind       infoKind
	infoRequestID  string
	infoServerName string

	// TTL for completed/failed requests (minutes)
	ttlMinutes int

	// Help overlay
	showHelp bool

	// Models overlay — список моделей выбранного в шапке сервера (хоткей m).
	showModels bool

	// showTail — показывать ли «хвост» генерации (последние токены) для
	// выбранного streaming-запроса в InfoPane. По умолчанию false (приватность —
	// это контент ответа модели). Переключается хоткеем t.
	showTail bool
}

// Run starts the TUI with an already-dialled WebSocket client.
// addr — адрес daemon'а (для loading pane). The caller owns the client and closes it after Run returns.
func Run(ctx context.Context, client *ipc.Client) error {
	return RunWithAddr(ctx, client, "")
}

// RunWithAddr — как Run, но с явным указанием addr для отображения в loading pane.
func RunWithAddr(ctx context.Context, client *ipc.Client, addr string) error {
	ti := textinput.New()
	ti.Placeholder = "model / client / server / status"
	ti.CharLimit = 80

	mCtx, mCancel := context.WithCancel(ctx)

	m := &model{
		client:      client,
		connURL:     addr,
		ctx:         mCtx,
		cancel:      mCancel,
		connStatus:  connStateConnecting,
		requests:    make(map[string]ipc.RequestState),
		flashMap:    make(map[string]flashEntry),
		filterInput: ti,
		ttlMinutes:  30,
		activePane:  paneRequests,
	}
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx), tea.WithMouseCellMotion())
	_, err := p.Run()
	// Отменяем ctx горутин.
	mCancel()
	if err != nil {
		return err
	}
	// При обрыве IPC altscreen уничтожает экран и пользователь не видит причину
	// выхода. Возвращаем её как error, чтобы cobra напечатала в stderr.
	if m.connError != "" {
		return fmt.Errorf("daemon connection lost: %s", m.connError)
	}
	return nil
}

// ---- tea.Msg types ----------------------------------------------------------

// recvMsg carries a raw WebSocket frame from the daemon.
type recvMsg struct {
	env     ipc.Envelope
	payload json.RawMessage
	err     error
}

// connLostMsg — соединение оборвалось, идёт реконнект.
type connLostMsg struct{}

// connRestoredMsg — реконнект успешен, соединение восстановлено.
type connRestoredMsg struct{}

// tickMsg is sent every second for elapsed-time refresh and flash expiry.
type tickMsg time.Time

// animTickMsg — частый тик (~160ms) для анимации пульса лампы работающих
// серверов. Активен только пока есть in-flight сервер (см. maybeStartAnim).
type animTickMsg time.Time

// refreshDoneMsg — визуальный сигнал «refresh sent» отработал (500ms).
type refreshDoneMsg struct{}

// animTickInterval — шаг анимации пульса. 4 кадра × 160ms ≈ 640ms на полный цикл.
const animTickInterval = 160 * time.Millisecond

// ---- Init -------------------------------------------------------------------

func (m *model) Init() tea.Cmd {
	return tea.Batch(
		m.waitForMessage(),
		tickEvery(),
	)
}

func tickEvery() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func animTick() tea.Cmd {
	return tea.Tick(animTickInterval, func(t time.Time) tea.Msg {
		return animTickMsg(t)
	})
}

// anyInFlight возвращает true, если хотя бы один сервер сейчас обрабатывает запрос.
func (m *model) anyInFlight() bool {
	for _, s := range m.servers {
		if s.Healthy && s.InFlight {
			return true
		}
	}
	return false
}

// maybeStartAnim запускает анимационный тик, если есть работающий сервер и тик
// ещё не запущен. Возвращает nil, если тик не нужен (всё простаивает).
func (m *model) maybeStartAnim() tea.Cmd {
	if m.animRunning {
		return nil
	}
	if m.anyInFlight() {
		m.animRunning = true
		return animTick()
	}
	return nil
}

// ---- waitForMessage ---------------------------------------------------------

// waitForMessage запускает горутину, которая читает следующий WS-фрейм.
// Если Recv возвращает ipc.ErrReconnecting — это сигнал для TUI о потере связи.
// Если ctx отменён — горутина завершается.
func (m *model) waitForMessage() tea.Cmd {
	return func() tea.Msg {
		env, payload, err := m.client.Recv(m.ctx)
		if err != nil {
			if m.ctx.Err() != nil {
				// TUI завершается — не нужно ничего делать.
				return recvMsg{err: m.ctx.Err()}
			}
			return recvMsg{err: err}
		}
		return recvMsg{env: env, payload: payload}
	}
}

// ---- Update -----------------------------------------------------------------

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.resizeViewports()
		return m, nil

	case tickMsg:
		// Expire flash entries older than 2 seconds.
		now := time.Time(msg)
		for k, fe := range m.flashMap {
			if now.Sub(fe.flashAt) > 2*time.Second {
				delete(m.flashMap, k)
			}
		}
		// TTL может сократить visibleRequests — клампим selectedIdx, чтобы курсор
		// не остался за пределами списка.
		m.clampSelected()
		// Если появился работающий сервер, а анимация не запущена — запускаем
		// (страховка на случай, если snapshot пришёл без смены числа серверов).
		return m, tea.Batch(tickEvery(), m.maybeStartAnim())

	case animTickMsg:
		m.animPhase++
		if m.anyInFlight() {
			return m, animTick()
		}
		// Никто не работает — останавливаем частый тик до следующего in-flight.
		m.animRunning = false
		return m, nil

	case recvMsg:
		if msg.err != nil {
			// ctx отменён — тихо выходим.
			if m.ctx.Err() != nil {
				return m, tea.Quit
			}
			// Любая другая ошибка после исчерпания reconnect-попыток
			// (Recv уже пробовал реконнектиться сам).
			m.connError = msg.err.Error()
			return m, tea.Quit
		}
		// Восстановление после реконнекта.
		if m.connStatus == connStateReconnecting {
			m.connStatus = connStateLive
			m.reconnecting = false
		} else {
			m.connStatus = connStateLive
		}
		if err := m.handleEnvelope(msg.env, msg.payload); err != nil {
			// Сохраняем parse-error в footer, не валим TUI.
			m.lastParseError = err.Error()
		} else {
			m.lastParseError = ""
		}
		// Diff/snapshot могли убрать выбранную строку — клампим.
		m.clampSelected()
		// Новый snapshot мог принести in-flight сервер — стартуем пульс-анимацию.
		return m, tea.Batch(m.waitForMessage(), m.maybeStartAnim())

	case connLostMsg:
		m.connStatus = connStateReconnecting
		m.reconnecting = true
		return m, nil

	case connRestoredMsg:
		m.connStatus = connStateLive
		m.reconnecting = false
		return m, nil

	case refreshDoneMsg:
		// Сброс визуального flash «refreshing…» если нужен.
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		return m.handleMouse(msg)
	}
	return m, nil
}

// paneAtY возвращает pane, в который попадает Y-координата.
// Layout: [0] title, [1..1+hH-1] header (hH = headerHeight(numServers)),
// далее request panel высотой reqViewport.Height+2 (рамка+заголовок), затем info.
func (m *model) paneAtY(y int) paneID {
	hH := headerHeight(len(m.servers))
	headerEnd := 1 + hH
	if y < headerEnd {
		return paneHeader
	}
	reqEnd := headerEnd + m.reqViewport.Height + 2
	if y < reqEnd {
		return paneRequests
	}
	return paneInfo
}

// handleMouse обрабатывает мышь:
//   - колесо: скроллит тот pane, на который указывает курсор; делает его активным.
//   - левый клик: выбирает строку (request / server), делает соответствующий pane
//     активным и обновляет InfoPane.
//
// 3 строки за тик колеса — стандарт btop/htop.
func (m *model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.filterActive {
		return m, nil
	}
	const step = 3
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		pane := m.paneAtY(msg.Y)
		m.activePane = pane
		switch pane {
		case paneHeader:
			if m.selectedServerIdx > 0 {
				m.selectedServerIdx--
				m.refreshInfoFromHeader()
			}
		case paneRequests:
			m.reqViewport.ScrollUp(step)
		case paneInfo:
			m.infoViewport.ScrollUp(step)
		}
	case tea.MouseButtonWheelDown:
		pane := m.paneAtY(msg.Y)
		m.activePane = pane
		switch pane {
		case paneHeader:
			if m.selectedServerIdx < len(m.servers)-1 {
				m.selectedServerIdx++
				m.refreshInfoFromHeader()
			}
		case paneRequests:
			m.reqViewport.ScrollDown(step)
		case paneInfo:
			m.infoViewport.ScrollDown(step)
		}
	case tea.MouseButtonLeft:
		// Только релиз — иначе одно нажатие отрабатывает дважды (press + release).
		if msg.Action != tea.MouseActionRelease {
			return m, nil
		}
		m.handleClick(msg.X, msg.Y)
	}
	return m, nil
}

// handleClick реализует выбор строки кликом. Координата Y трактуется относительно
// текущего layout'а: 1 — title, далее header (по 1 строке на сервер + рамка),
// далее requests pane, далее info pane. Внутри requests pane строка вычисляется
// с учётом reqViewport.YOffset (скролл).
func (m *model) handleClick(_, y int) {
	pane := m.paneAtY(y)
	m.activePane = pane
	switch pane {
	case paneHeader:
		// y=0 — title; y=1 — top border шапки; y=2..2+N-1 — сервер-чипы.
		idx := y - 2
		if idx >= 0 && idx < len(m.servers) {
			m.selectedServerIdx = idx
			m.refreshInfoFromHeader()
		}
	case paneRequests:
		hH := headerHeight(len(m.servers))
		// Layout request-панели по вертикали:
		//   y = 1+hH     — top border;
		//   y = 1+hH+1   — заголовок "Requests";
		//   y = 1+hH+2   — строка заголовков колонок (Client/Model/Server/...);
		//   y = 1+hH+3   — первая data-строка (row=0 при YOffset==0).
		// Поэтому dataTop = 1 + hH + 3, а не +2: иначе клик уезжал на 1 строку
		// ниже точки нажатия (баг #82, v0.9.0).
		dataTop := 1 + hH + 3
		row := y - dataTop + m.reqViewport.YOffset
		visible := m.visibleRequests()
		if row >= 0 && row < len(visible) {
			m.selectedIdx = row
			m.refreshInfoFromRequests()
			m.ensureSelected()
		}
	}
}

// refreshInfoFromRequests обновляет InfoPane на основе текущего selectedIdx.
func (m *model) refreshInfoFromRequests() {
	visible := m.visibleRequests()
	if m.selectedIdx >= 0 && m.selectedIdx < len(visible) {
		m.infoKind = infoKindRequest
		m.infoRequestID = visible[m.selectedIdx].ID
	}
}

// refreshInfoFromHeader обновляет InfoPane на основе текущего selectedServerIdx.
func (m *model) refreshInfoFromHeader() {
	if m.selectedServerIdx >= 0 && m.selectedServerIdx < len(m.servers) {
		m.infoKind = infoKindServer
		m.infoServerName = m.servers[m.selectedServerIdx].Name
	}
}

// handleEnvelope processes a decoded WebSocket message. Returns non-nil error
// only for parse failures (TUI should display but not quit on these).
func (m *model) handleEnvelope(env ipc.Envelope, payload json.RawMessage) error {
	switch env.Type {
	case ipc.TypeHello:
		var h ipc.HelloPayload
		if err := json.Unmarshal(payload, &h); err != nil {
			return fmt.Errorf("bad envelope (hello): %w", err)
		}
		m.version = h.Version
		// Если daemon явно передал TTL — применяем; 0 трактуем как «не задано»
		// (daemon тоже подставляет дефолт 30 при загрузке конфига, так что
		// 0 здесь означает «старый daemon без поля»). Сохраняем встроенный
		// fallback 30 минут.
		if h.ShowCompletedMinutes > 0 {
			m.ttlMinutes = h.ShowCompletedMinutes
		}

	case ipc.TypeStateSnapshot:
		var snap ipc.StateSnapshotPayload
		if err := json.Unmarshal(payload, &snap); err != nil {
			return fmt.Errorf("bad envelope (state_snapshot): %w", err)
		}
		m.snapshotReceived = true
		// Track flash: detect model changes vs existing server state.
		oldCount := len(m.servers)
		m.applyServerList(snap.Servers)
		// Replace request map entirely.
		newMap := make(map[string]ipc.RequestState, len(snap.Requests))
		for _, r := range snap.Requests {
			newMap[r.ID] = r
		}
		m.requests = newMap
		// header высота зависит от числа серверов — пересчитать layout.
		if oldCount != len(m.servers) {
			m.resizeViewports()
		}

	}
	return nil
}

// applyServerList replaces m.servers and tracks model changes for flash.
// Серверы сортируются по приоритету (меньше число = выше предпочтение, наверху),
// tiebreak по имени — порядок стабилен между снапшотами, что важно и для
// индексных цветов (ServerColorByIndex), и для курсора selectedServerIdx.
func (m *model) applyServerList(incoming []ipc.ServerState) {
	now := time.Now()
	oldByName := make(map[string]ipc.ServerState, len(m.servers))
	for _, s := range m.servers {
		oldByName[s.Name] = s
	}
	for _, s := range incoming {
		if old, ok := oldByName[s.Name]; ok {
			if old.CurrentModel != s.CurrentModel {
				m.flashMap[s.Name] = flashEntry{flashAt: now}
			}
		}
	}
	sort.SliceStable(incoming, func(i, j int) bool {
		if incoming[i].Priority != incoming[j].Priority {
			return incoming[i].Priority < incoming[j].Priority
		}
		return incoming[i].Name < incoming[j].Name
	})
	m.servers = incoming
}

// handleKey handles all keyboard events, respecting filter mode.
func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Help overlay перехватывает все клавиши кроме закрывающих.
	if m.showHelp {
		switch {
		case key.Matches(msg, keys.Help),
			key.Matches(msg, keys.Esc),
			key.Matches(msg, keys.Quit):
			m.showHelp = false
		}
		return m, nil
	}

	// Models overlay перехватывает все клавиши кроме закрывающих (m / Esc / q).
	if m.showModels {
		switch {
		case key.Matches(msg, keys.Models),
			key.Matches(msg, keys.Esc),
			key.Matches(msg, keys.Quit):
			m.showModels = false
		}
		return m, nil
	}

	// Filter input mode.
	if m.filterActive {
		switch {
		case key.Matches(msg, keys.Esc):
			m.filterActive = false
			m.filterText = ""
			m.filterInput.Reset()
			m.filterInput.Blur()
		case key.Matches(msg, keys.Enter):
			m.filterText = strings.TrimSpace(m.filterInput.Value())
			m.filterActive = false
			m.filterInput.Blur()
		default:
			var cmd tea.Cmd
			m.filterInput, cmd = m.filterInput.Update(msg)
			return m, cmd
		}
		return m, nil
	}

	// Normal mode.
	switch {
	case key.Matches(msg, keys.Quit):
		m.cancel()
		return m, tea.Quit

	case key.Matches(msg, keys.Help):
		m.showHelp = true
		return m, nil

	case key.Matches(msg, keys.Models):
		// Показываем модели выбранного в шапке сервера; если серверов нет —
		// no-op (оверлей бессмыслен).
		if len(m.servers) > 0 {
			m.showModels = true
		}
		return m, nil

	case key.Matches(msg, keys.Tail):
		// Тогл «хвоста» генерации в InfoPane (контент ответа — по явному запросу).
		m.showTail = !m.showTail
		return m, nil

	case key.Matches(msg, keys.Refresh):
		// F5: отправить daemon'у запрос немедленного snapshot.
		// Snapshot придёт через обычный Recv-поток.
		return m, m.sendRequestSnapshot()

	case key.Matches(msg, keys.Filter):
		m.filterActive = true
		m.filterInput.Focus()
		return m, textinput.Blink

	case key.Matches(msg, keys.Tab):
		// Циклический Tab: header → requests → info → header.
		switch m.activePane {
		case paneHeader:
			m.activePane = paneRequests
			m.refreshInfoFromRequests()
		case paneRequests:
			m.activePane = paneInfo
		default:
			m.activePane = paneHeader
			m.refreshInfoFromHeader()
		}
		return m, nil

	case key.Matches(msg, keys.Enter):
		// Enter обновляет InfoPane (как клик по строке).
		switch m.activePane {
		case paneRequests:
			m.refreshInfoFromRequests()
		case paneHeader:
			m.refreshInfoFromHeader()
		}
		return m, nil

	case key.Matches(msg, keys.Esc):
		// Clear filter if active text set.
		if m.filterText != "" {
			m.filterText = ""
			m.filterInput.Reset()
		}
		return m, nil

	case key.Matches(msg, keys.Up):
		return m, m.scrollUp()

	case key.Matches(msg, keys.Down):
		return m, m.scrollDown()

	case key.Matches(msg, keys.PageUp):
		return m, m.pageUp()

	case key.Matches(msg, keys.PageDown):
		return m, m.pageDown()

	case key.Matches(msg, keys.Home):
		return m, m.scrollHome()

	case key.Matches(msg, keys.End):
		return m, m.scrollEnd()
	}
	return m, nil
}

// sendRequestSnapshot возвращает tea.Cmd, отправляющую daemon'у request_snapshot.
// Выполняется в горутине чтобы не блокировать Update.
func (m *model) sendRequestSnapshot() tea.Cmd {
	return func() tea.Msg {
		_ = m.client.RequestSnapshot(m.ctx)
		// Snapshot придёт через Recv — просто возвращаем nil msg.
		return nil
	}
}

// visibleRequests returns the filtered+sorted list that is currently shown.
func (m *model) visibleRequests() []ipc.RequestState {
	all := make([]ipc.RequestState, 0, len(m.requests))
	for _, r := range m.requests {
		all = append(all, r)
	}
	sorted := sortedRequests(all)
	now := time.Now()
	visible := make([]ipc.RequestState, 0, len(sorted))
	for _, r := range sorted {
		if requestVisible(r, now, m.ttlMinutes) && requestMatchesFilter(r, m.filterText) {
			visible = append(visible, r)
		}
	}
	return visible
}

// ---- Scroll helpers ---------------------------------------------------------

func (m *model) scrollUp() tea.Cmd {
	switch m.activePane {
	case paneHeader:
		if m.selectedServerIdx > 0 {
			m.selectedServerIdx--
			m.refreshInfoFromHeader()
		}
	case paneRequests:
		if m.selectedIdx > 0 {
			m.selectedIdx--
		}
		m.refreshInfoFromRequests()
		m.ensureSelected()
	case paneInfo:
		m.infoViewport.ScrollUp(1)
	}
	return nil
}

func (m *model) scrollDown() tea.Cmd {
	switch m.activePane {
	case paneHeader:
		if m.selectedServerIdx < len(m.servers)-1 {
			m.selectedServerIdx++
			m.refreshInfoFromHeader()
		}
	case paneRequests:
		max := len(m.visibleRequests()) - 1
		if m.selectedIdx < max {
			m.selectedIdx++
		}
		m.refreshInfoFromRequests()
		m.ensureSelected()
	case paneInfo:
		m.infoViewport.ScrollDown(1)
	}
	return nil
}

func (m *model) pageUp() tea.Cmd {
	switch m.activePane {
	case paneRequests:
		m.selectedIdx -= m.reqViewport.Height
		if m.selectedIdx < 0 {
			m.selectedIdx = 0
		}
		m.refreshInfoFromRequests()
	case paneInfo:
		m.infoViewport.HalfPageUp()
	}
	return nil
}

func (m *model) pageDown() tea.Cmd {
	switch m.activePane {
	case paneRequests:
		visible := m.visibleRequests()
		max := len(visible) - 1
		if max < 0 {
			m.selectedIdx = 0
			return nil
		}
		m.selectedIdx += m.reqViewport.Height
		if m.selectedIdx > max {
			m.selectedIdx = max
		}
		m.refreshInfoFromRequests()
	case paneInfo:
		m.infoViewport.HalfPageDown()
	}
	return nil
}

func (m *model) scrollHome() tea.Cmd {
	switch m.activePane {
	case paneRequests:
		m.selectedIdx = 0
		m.refreshInfoFromRequests()
	case paneInfo:
		m.infoViewport.GotoTop()
	}
	return nil
}

func (m *model) scrollEnd() tea.Cmd {
	switch m.activePane {
	case paneRequests:
		max := len(m.visibleRequests()) - 1
		if max < 0 {
			max = 0
		}
		m.selectedIdx = max
		m.refreshInfoFromRequests()
	case paneInfo:
		m.infoViewport.GotoBottom()
	}
	return nil
}

// clampSelected приводит m.selectedIdx в допустимый диапазон [0, len(visible)-1].
// Вызывается после tick (TTL может убрать строки) и после обработки diff/snapshot.
func (m *model) clampSelected() {
	n := len(m.visibleRequests())
	if n == 0 {
		m.selectedIdx = 0
		return
	}
	if m.selectedIdx >= n {
		m.selectedIdx = n - 1
	}
	if m.selectedIdx < 0 {
		m.selectedIdx = 0
	}
}

// ensureSelected scrolls the request viewport to keep selected row visible.
func (m *model) ensureSelected() {
	// Each row is 1 line; header is 1 line.
	lineOffset := m.selectedIdx + 1 // +1 for header
	vpH := m.reqViewport.Height
	if vpH <= 0 {
		return
	}
	top := m.reqViewport.YOffset
	bottom := top + vpH - 1
	if lineOffset < top {
		m.reqViewport.SetYOffset(lineOffset)
	} else if lineOffset > bottom {
		m.reqViewport.SetYOffset(lineOffset - vpH + 1)
	}
}

// ---- resizeViewports --------------------------------------------------------

func (m *model) resizeViewports() {
	if m.width < 60 || m.height < 10 {
		return
	}
	// Layout breakdown (all heights in terminal lines):
	//   titleHeight   = 1 (заголовок ProxyLM.GO vX)
	//   headerHeight  = 2 (рамка) + len(servers) + 1 (counters), мин. 4
	//   footerHeight  = 1
	//   filterHeight  = 3 (only when active: border top/bottom + 1 content)
	//   remaining     = height - title - header - footer - filter
	//   requests pane = remaining * 2/3
	//   info pane     = remaining - requests
	titleHeight := 1
	hH := headerHeight(len(m.servers))
	footerHeight := 1
	filterH := 0
	if m.filterActive {
		filterH = 3
	}
	remaining := m.height - titleHeight - hH - footerHeight - filterH
	if remaining < 4 {
		remaining = 4
	}
	reqH := remaining * 2 / 3
	if reqH < 3 {
		reqH = 3
	}
	infoH := remaining - reqH
	if infoH < 2 {
		infoH = 2
	}

	// Viewport internal heights = panel height - border(2) - title(1) = height - 3.
	reqInner := reqH - 3
	if reqInner < 1 {
		reqInner = 1
	}
	infoInner := infoH - 3
	if infoInner < 1 {
		infoInner = 1
	}

	m.reqViewport.Width = m.width - 4
	m.reqViewport.Height = reqInner
	m.infoViewport.Width = m.width - 4
	m.infoViewport.Height = infoInner
}

// ---- View -------------------------------------------------------------------

func (m *model) View() string {
	if m.width == 0 {
		// Not yet sized.
		return ""
	}
	if m.width < 60 {
		return renderTooNarrow()
	}

	// Error state: show message and exit hint.
	if m.connError != "" {
		return StyleError.Render("Connection error: "+m.connError) + "\n" +
			StyleDim.Render("(TUI exiting)")
	}

	// Build flash map for this render.
	flashModels := make(map[string]bool, len(m.flashMap))
	for name := range m.flashMap {
		flashModels[name] = true
	}

	// Карта цветов серверов: serverName → позиция в отсортированном списке.
	// Назначает различимые цвета по индексу (см. ServerColorByIndex) и
	// переиспользуется в шапке и в колонке Server таблицы запросов.
	colorIdx := make(map[string]int, len(m.servers))
	for i, s := range m.servers {
		colorIdx[s.Name] = i
	}

	// Collect requests slice from map.
	reqSlice := make([]ipc.RequestState, 0, len(m.requests))
	for _, r := range m.requests {
		reqSlice = append(reqSlice, r)
	}

	now := time.Now()

	// Клампим selectedServerIdx — серверы могли уйти/прийти.
	// (мутация в View допустима только для клампинга индекса — чистый инвариант
	// «не выходить за пределы слайса» без побочных эффектов на логику).
	clampedIdx := m.selectedServerIdx
	if clampedIdx >= len(m.servers) {
		clampedIdx = len(m.servers) - 1
	}
	if clampedIdx < 0 {
		clampedIdx = 0
	}

	// Title — показывает версию daemon'а или статус соединения.
	ver := m.connStatusLabel()
	title := StyleTitle.Render("Proxy") + StyleHeader.Render("LM.GO") + StyleTitle.Render(" "+ver)

	// Panels.
	header := renderHeaderBar(m.servers, reqSlice, flashModels, colorIdx, m.animPhase,
		m.ttlMinutes, m.width, m.activePane == paneHeader, clampedIdx)

	var reqPanel string
	if !m.snapshotReceived {
		reqPanel = renderLoadingPane(m.connURL, m.width, m.activePane)
	} else {
		reqPanel = renderRequestTable(
			&m.reqViewport,
			reqSlice,
			m.servers,
			colorIdx,
			m.selectedIdx,
			m.filterText,
			m.width,
			m.activePane,
			now,
			m.ttlMinutes,
		)
	}

	infoPanel := renderInfoPane(
		&m.infoViewport,
		m.activePane,
		m.infoKind,
		m.lookupInfoRequest(),
		m.lookupInfoServer(),
		m.showTail,
		m.width,
	)

	footer := m.buildFooter()

	sections := []string{title, header, reqPanel, infoPanel}

	if m.filterActive {
		sections = append(sections, renderFilterBar(m.filterInput, m.width))
	}
	sections = append(sections, footer)

	base := lipgloss.JoinVertical(lipgloss.Left, sections...)

	// Help overlay — рендерится поверх всего содержимого.
	if m.showHelp {
		return renderHelpOverlay(m.width, m.height)
	}

	// Models overlay — список моделей выбранного сервера.
	if m.showModels && len(m.servers) > 0 {
		return renderModelsOverlay(m.servers[clampedIdx], m.width, m.height)
	}

	return base
}

// connStatusLabel возвращает строку статуса соединения для title.
func (m *model) connStatusLabel() string {
	switch m.connStatus {
	case connStateConnecting:
		return "connecting…"
	case connStateReconnecting:
		return StyleFlash.Render("reconnecting…")
	default:
		if m.version != "" {
			return m.version
		}
		return "live"
	}
}

// buildFooter собирает строку footer с учётом parse-error и ширины.
func (m *model) buildFooter() string {
	// parse-error имеет приоритет над обычным footer.
	if m.lastParseError != "" {
		errText := StyleError.Render("parse error: " + m.lastParseError)
		return StyleFooter.Width(m.width - 2).Render(errText)
	}
	return renderFooter(m.filterActive, m.width)
}

// lookupInfoRequest возвращает указатель на RequestState с id == infoRequestID,
// или nil если запись ушла из снапшота (TTL / снова свежий snapshot без неё).
func (m *model) lookupInfoRequest() *ipc.RequestState {
	if m.infoRequestID == "" {
		return nil
	}
	if r, ok := m.requests[m.infoRequestID]; ok {
		return &r
	}
	return nil
}

// lookupInfoServer возвращает указатель на ServerState с name == infoServerName,
// или nil если сервер ушёл из конфига.
func (m *model) lookupInfoServer() *ipc.ServerState {
	if m.infoServerName == "" {
		return nil
	}
	for i, s := range m.servers {
		if s.Name == m.infoServerName {
			return &m.servers[i]
		}
	}
	return nil
}
