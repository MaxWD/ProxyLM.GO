package tui

import (
	"context"
	"encoding/json"
	"fmt"
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

// flashEntry tracks when a server's current_model last changed, for 2-second yellow flash.
type flashEntry struct {
	model   string
	flashAt time.Time
}

// model is the Bubble Tea Model. All state is held here; Update is a pure function.
type model struct {
	// connection
	client *ipc.Client

	// daemon state
	version   string
	servers   []ipc.ServerState
	requests  map[string]ipc.RequestState // keyed by request ID
	connError string

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
}

// Run starts the TUI with an already-dialled WebSocket client.
// The caller owns the client and closes it after Run returns.
func Run(ctx context.Context, client *ipc.Client) error {
	ti := textinput.New()
	ti.Placeholder = "model / client / server / status"
	ti.CharLimit = 80

	m := &model{
		client:      client,
		requests:    make(map[string]ipc.RequestState),
		flashMap:    make(map[string]flashEntry),
		filterInput: ti,
		ttlMinutes:  30,
		activePane:  paneRequests,
	}
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
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

// tickMsg is sent every second for elapsed-time refresh and flash expiry.
type tickMsg time.Time

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

// ---- waitForMessage ---------------------------------------------------------

func (m *model) waitForMessage() tea.Cmd {
	return func() tea.Msg {
		env, payload, err := m.client.Recv(context.Background())
		return recvMsg{env: env, payload: payload, err: err}
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
		return m, tickEvery()

	case recvMsg:
		if msg.err != nil {
			m.connError = msg.err.Error()
			return m, tea.Quit
		}
		m.handleEnvelope(msg.env, msg.payload)
		// Diff/snapshot могли убрать выбранную строку — клампим.
		m.clampSelected()
		return m, m.waitForMessage()

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

// handleEnvelope processes a decoded WebSocket message.
func (m *model) handleEnvelope(env ipc.Envelope, payload json.RawMessage) {
	switch env.Type {
	case ipc.TypeHello:
		var h ipc.HelloPayload
		if err := json.Unmarshal(payload, &h); err == nil {
			m.version = h.Version
			// Если daemon явно передал TTL — применяем; 0 трактуем как «не задано»
			// (daemon тоже подставляет дефолт 30 при загрузке конфига, так что
			// 0 здесь означает «старый daemon без поля»). Сохраняем встроенный
			// fallback 30 минут.
			if h.ShowCompletedMinutes > 0 {
				m.ttlMinutes = h.ShowCompletedMinutes
			}
		}

	case ipc.TypeStateSnapshot:
		var snap ipc.StateSnapshotPayload
		if err := json.Unmarshal(payload, &snap); err != nil {
			return
		}
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

	case ipc.TypeStateDiff:
		var diff struct {
			Servers          []ipc.ServerState  `json:"servers"`
			RequestsUpserted []ipc.RequestState `json:"requests_upserted"`
			RequestsRemoved  []string           `json:"requests_removed"`
		}
		if err := json.Unmarshal(payload, &diff); err != nil {
			return
		}
		oldCount := len(m.servers)
		if len(diff.Servers) > 0 {
			// Merge: update only the named servers.
			for _, updated := range diff.Servers {
				m.applyServerUpdate(updated)
			}
		}
		for _, r := range diff.RequestsUpserted {
			m.requests[r.ID] = r
		}
		for _, id := range diff.RequestsRemoved {
			delete(m.requests, id)
		}
		if oldCount != len(m.servers) {
			m.resizeViewports()
		}

	case ipc.TypeLogLine:
		// LogPane убран в v0.8.0 — лог-поток от daemon'а пока приходит, но
		// мы его игнорируем (поток оставлен ради обратной совместимости wire-
		// протокола). Если в будущем понадобится «events»-feed в InfoPane,
		// он встанет сюда.
	}
}

// applyServerList replaces m.servers and tracks model changes for flash.
func (m *model) applyServerList(incoming []ipc.ServerState) {
	now := time.Now()
	oldByName := make(map[string]ipc.ServerState, len(m.servers))
	for _, s := range m.servers {
		oldByName[s.Name] = s
	}
	for _, s := range incoming {
		if old, ok := oldByName[s.Name]; ok {
			if old.CurrentModel != s.CurrentModel {
				m.flashMap[s.Name] = flashEntry{model: s.CurrentModel, flashAt: now}
			}
		}
	}
	m.servers = incoming
}

// applyServerUpdate merges a single server diff into m.servers.
func (m *model) applyServerUpdate(updated ipc.ServerState) {
	now := time.Now()
	for i, s := range m.servers {
		if s.Name == updated.Name {
			if updated.CurrentModel != "" && s.CurrentModel != updated.CurrentModel {
				m.flashMap[s.Name] = flashEntry{model: updated.CurrentModel, flashAt: now}
			}
			// Merge non-zero fields.
			if updated.CurrentModel != "" {
				m.servers[i].CurrentModel = updated.CurrentModel
			}
			if updated.URL != "" {
				m.servers[i].URL = updated.URL
			}
			m.servers[i].Healthy = updated.Healthy
			m.servers[i].InFlight = updated.InFlight
			m.servers[i].QueueDepth = updated.QueueDepth
			if len(updated.Models) > 0 {
				m.servers[i].Models = updated.Models
			}
			return
		}
	}
	// Server not found in existing list — append.
	m.servers = append(m.servers, updated)
}

// handleKey handles all keyboard events, respecting filter mode.
func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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
		return m, tea.Quit

	case key.Matches(msg, keys.Refresh):
		// F5: send ping to daemon to prompt a fresh snapshot.
		// The daemon will reply with state_snapshot on next message.
		// Nothing special needed client-side; waitForMessage loop is already running.
		return m, nil

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
		max := len(m.visibleRequests()) - 1
		m.selectedIdx += m.reqViewport.Height
		if m.selectedIdx > max {
			m.selectedIdx = max
		}
		if m.selectedIdx < 0 {
			m.selectedIdx = 0
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

	// Collect requests slice from map.
	reqSlice := make([]ipc.RequestState, 0, len(m.requests))
	for _, r := range m.requests {
		reqSlice = append(reqSlice, r)
	}

	now := time.Now()

	// Клампим selectedServerIdx — серверы могли уйти/прийти.
	if m.selectedServerIdx >= len(m.servers) {
		m.selectedServerIdx = len(m.servers) - 1
	}
	if m.selectedServerIdx < 0 {
		m.selectedServerIdx = 0
	}

	// Panels.
	header := renderHeaderBar(m.servers, reqSlice, flashModels, m.ttlMinutes, m.width,
		m.activePane == paneHeader, m.selectedServerIdx)
	reqPanel := renderRequestTable(
		&m.reqViewport,
		reqSlice,
		m.servers,
		m.selectedIdx,
		m.filterText,
		m.width,
		m.activePane,
		now,
		m.ttlMinutes,
	)
	infoPanel := renderInfoPane(
		&m.infoViewport,
		m.activePane,
		m.infoKind,
		m.lookupInfoRequest(),
		m.lookupInfoServer(),
		m.width,
	)
	footer := renderFooter(m.filterActive, m.width)

	// Version in title area.
	ver := m.version
	if ver == "" {
		ver = "connecting…"
	}
	// "Proxy" — bright blue (StyleTitle), "LM.GO" — white bold (StyleHeader),
	// версия — обратно в StyleTitle.
	title := StyleTitle.Render("Proxy") + StyleHeader.Render("LM.GO") + StyleTitle.Render(" "+ver)

	sections := []string{title, header, reqPanel, infoPanel}

	if m.filterActive {
		sections = append(sections, renderFilterBar(m.filterInput, m.width))
	}
	sections = append(sections, footer)

	return lipgloss.JoinVertical(lipgloss.Left, sections...)
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
