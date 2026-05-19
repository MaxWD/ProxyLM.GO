package ipc

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"proxylm/internal/core"
	"proxylm/internal/storage"
)

// Hub — публикатор состояния и логов подписчикам через WebSocket.
// Каждое соединение получает свою горутину; broadcast — через канал клиента
// с buffered queue. Если клиент не успевает читать, его соединение закрывается.
type Hub struct {
	version              string
	servers              []*core.ServerInfo
	history              *storage.History
	log                  *slog.Logger
	snapshotEvery        time.Duration
	maxAttempts          int
	showCompletedMinutes int
	perf                 *core.PerfTracker

	mu      sync.RWMutex
	clients map[*hubClient]struct{}
}

type hubClient struct {
	conn      *websocket.Conn
	out       chan []byte
	closed    chan struct{}
	closeOnce sync.Once
}

// closeConn закрывает соединение ровно один раз (sync.Once).
func (c *hubClient) closeConn(code websocket.StatusCode, reason string) {
	c.closeOnce.Do(func() {
		_ = c.conn.Close(code, reason)
	})
}

// NewHub создаёт публикатор. snapshotEvery определяет периодичность отправки
// полного снимка состояния всем подключённым клиентам. maxAttempts — значение
// retry.max_attempts из конфига; используется TUI для отображения "attempt N/M".
// showCompletedMinutes — значение tui.show_completed_minutes из конфига; передаётся
// TUI в Hello, чтобы тот скрывал завершённые записи через указанный интервал.
// perf — опционально; если передан, ServerState будет содержать TokensPerSec/TtftMs
// для пары (server, current_model). nil допустим — поля просто останутся пустыми.
func NewHub(version string, servers []*core.ServerInfo, history *storage.History, snapshotEvery time.Duration, maxAttempts, showCompletedMinutes int, perf *core.PerfTracker, log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	if snapshotEvery <= 0 {
		snapshotEvery = 1 * time.Second
	}
	return &Hub{
		version:              version,
		servers:              servers,
		history:              history,
		log:                  log,
		snapshotEvery:        snapshotEvery,
		maxAttempts:          maxAttempts,
		showCompletedMinutes: showCompletedMinutes,
		perf:                 perf,
		clients:              make(map[*hubClient]struct{}),
	}
}

// Run запускает периодический snapshot broadcaster. Блокирует до отмены ctx.
func (h *Hub) Run(ctx context.Context) {
	t := time.NewTicker(h.snapshotEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			h.closeAll()
			return
		case <-t.C:
			h.broadcastSnapshot(ctx)
		}
	}
}

// Handler возвращает http.HandlerFunc для регистрации в API-router'е под /admin/stream.
// Перед вызовом этого handler'а должна сработать AdminAuth middleware.
func (h *Hub) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true, // CORS вне scope MVP
		})
		if err != nil {
			h.log.Warn("ipc: accept failed", "error", err.Error())
			return
		}
		client := &hubClient{
			conn:   conn,
			out:    make(chan []byte, 64),
			closed: make(chan struct{}),
		}
		h.register(client)
		defer h.unregister(client)

		// Hello message + первый snapshot.
		h.sendTo(client, Envelope{
			Type: TypeHello,
			Time: time.Now().UTC(),
			Payload: HelloPayload{
				Version:              h.version,
				NumServers:           len(h.servers),
				ShowCompletedMinutes: h.showCompletedMinutes,
			},
		})
		h.sendTo(client, h.buildSnapshot(r.Context()))

		// writer goroutine — для строго одного producer'а на conn.
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		go client.writer(ctx, h.log)

		// reader: обрабатываем входящие фреймы (request_snapshot), детектим закрытие.
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				client.closeConn(websocket.StatusNormalClosure, "")
				return
			}
			h.handleClientFrame(ctx, client, data)
		}
	}
}

func (h *Hub) register(c *hubClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[c] = struct{}{}
}

func (h *Hub) unregister(c *hubClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		// Защита от двойного закрытия: closeAll может уже закрыть c.closed.
		select {
		case <-c.closed:
		default:
			close(c.closed)
		}
	}
}

func (h *Hub) broadcastSnapshot(ctx context.Context) {
	env := h.buildSnapshot(ctx)
	data, err := json.Marshal(env)
	if err != nil {
		h.log.Warn("ipc: marshal snapshot", "error", err.Error())
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		select {
		case c.out <- data:
		default:
			// клиент не успевает — обрываем соединение, writer закроет conn
			c.closeConn(websocket.StatusPolicyViolation, "slow consumer")
		}
	}
}

func (h *Hub) buildSnapshot(ctx context.Context) Envelope {
	serverStates := make([]ServerState, 0, len(h.servers))
	for _, s := range h.servers {
		s.Lock()
		models := make([]string, 0, len(s.Models))
		for _, m := range s.Models {
			models = append(models, m.Name)
		}
		queueDepth := len(s.Queue)
		s.Unlock()
		current := s.CurrentModelString()
		state := ServerState{
			Name:         s.Name,
			URL:          s.URL,
			Healthy:      s.Healthy.Load(),
			InFlight:     s.InFlight.Load(),
			CurrentModel: current,
			QueueDepth:   queueDepth,
			Models:       models,
			Slow:         s.LastSlow.Load(),
			FailureCount: s.FailureCount.Load(),
		}
		// Метрики для шапки — пара (server, current_model). Если у пары есть
		// статистика на нескольких endpoint'ах (с v0.10.0 ключ включает endpoint),
		// для header'а агрегируем по «наиболее представительному» (с максимумом
		// samples). Это совпадает с интуицией оператора, когда модель в основном
		// используется через один endpoint. Полный per-(model,endpoint) разрез
		// доступен в server-modal через PerModelStats.
		if h.perf != nil {
			if current != "" {
				best := bestModelStats(h.perf, s.Name, current)
				if best.Samples > 0 {
					state.PerfSamples = best.Samples
					state.PerfOK = best.OK
					if best.OK {
						state.TLoadMs = best.TLoadMs
						state.TokInPerSec = tokPerSec(best.KInMsTok)
						state.TokOutPerSec = tokPerSec(best.KOutMsTok)
						state.RSquared = best.RSquared
						state.FitQuality = best.FitQuality
					}
				}
			}
			summary := h.perf.ServerSummary(s.Name)
			if len(summary) > 0 {
				state.PerModelStats = make([]ModelStats, 0, len(summary))
				for _, ms := range summary {
					state.PerModelStats = append(state.PerModelStats, ModelStats{
						Model:        ms.Model,
						Endpoint:     ms.Endpoint,
						Samples:      ms.Stats.Samples,
						Loaded:       ms.Stats.Loaded,
						OK:           ms.Stats.OK,
						TLoadMs:      ms.Stats.TLoadMs,
						TokInPerSec:  tokPerSec(ms.Stats.KInMsTok),
						TokOutPerSec: tokPerSec(ms.Stats.KOutMsTok),
						RSquared:     ms.Stats.RSquared,
						FitQuality:   ms.Stats.FitQuality,
						TLoadCI:      ms.Stats.TLoadCI,
						KInCI:        ms.Stats.KInCI,
						KOutCI:       ms.Stats.KOutCI,
					})
				}
			}
		}
		serverStates = append(serverStates, state)
	}

	requests := make([]RequestState, 0, 100)
	recs, err := h.history.List(ctx, 100)
	if err == nil {
		for _, r := range recs {
			var queueWait int64
			if !r.StartedAt.IsZero() && !r.CreatedAt.IsZero() && r.StartedAt.After(r.CreatedAt) {
				queueWait = r.StartedAt.Sub(r.CreatedAt).Milliseconds()
			}
			requests = append(requests, RequestState{
				ID:               r.ID,
				ClientName:       r.ClientName,
				Model:            r.Model,
				Endpoint:         r.Endpoint,
				Stream:           r.Stream,
				ServerName:       r.ServerName,
				Status:           string(r.Status),
				HTTPStatus:       r.HTTPStatus,
				PromptTokens:     r.PromptTokens,
				OutputTokens:     r.OutputTokens,
				CreatedAt:        r.CreatedAt,
				StartedAt:        r.StartedAt,
				CompletedAt:      r.CompletedAt,
				Attempt:          r.Attempt,
				MaxAttempts:      h.maxAttempts,
				QueueWaitMs:      queueWait,
				ModelReloaded:    r.ModelReloaded,
				LastFailedServer: r.LastFailedServer,
				ErrorMessage:     r.ErrorMessage,
			})
		}
	}

	return Envelope{
		Type:    TypeStateSnapshot,
		Time:    time.Now().UTC(),
		Payload: StateSnapshotPayload{Servers: serverStates, Requests: requests},
	}
}

func (h *Hub) sendTo(c *hubClient, env Envelope) {
	data, err := json.Marshal(env)
	if err != nil {
		return
	}
	select {
	case c.out <- data:
	default:
	}
}

func (h *Hub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		c.closeConn(websocket.StatusGoingAway, "shutdown")
		// Закрываем c.closed, чтобы writer-горутина вышла из select.
		select {
		case <-c.closed:
		default:
			close(c.closed)
		}
	}
}

// handleClientFrame разбирает входящий фрейм от TUI-клиента и реагирует на него.
// Сейчас поддерживается только TypeRequestSnapshot (F5): отправляет немедленный
// snapshot именно этому клиенту через c.out (без broadcast).
// Неизвестные типы игнорируются с debug-логом.
func (h *Hub) handleClientFrame(ctx context.Context, c *hubClient, data []byte) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		h.log.Debug("ipc: invalid frame from client", "error", err.Error())
		return
	}
	switch env.Type {
	case TypeRequestSnapshot:
		// ctx — это per-connection context из reader-loop'а (`r.Context()` +
		// WithCancel). Если клиент уже отключился — buildSnapshot отменится
		// и мы не тратим ресурсы на формирование snapshot'а в никуда.
		snap := h.buildSnapshot(ctx)
		h.sendTo(c, snap)
	default:
		h.log.Debug("ipc: unknown frame type from client", "type", env.Type)
	}
}

// tokPerSec переводит коэффициент регрессии k (ms/token) в tok/s.
// Отрицательные или нулевые значения трактуем как «нет данных» (0).
func tokPerSec(kMsPerTok float64) float64 {
	if kMsPerTok <= 0 {
		return 0
	}
	return 1000.0 / kMsPerTok
}

// bestModelStats возвращает PerfStats наиболее представительного endpoint'а
// для пары (server, model). С v0.10.0 ключ регрессии включает endpoint,
// поэтому для header-метрики (одна строка на сервер) нужно агрегировать по
// одному endpoint'у — выбираем тот, у которого больше всего samples (это,
// как правило, основной режим использования модели). Если ни одного
// endpoint'а не нашлось — возвращает zero-value PerfStats.
func bestModelStats(p *core.PerfTracker, server, model string) core.PerfStats {
	if p == nil {
		return core.PerfStats{}
	}
	var best core.PerfStats
	for _, ms := range p.ServerSummary(server) {
		if ms.Model != model {
			continue
		}
		if ms.Stats.Samples > best.Samples {
			best = ms.Stats
		}
	}
	return best
}

func (c *hubClient) writer(ctx context.Context, log *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.closed:
			return
		case data, ok := <-c.out:
			if !ok {
				return
			}
			wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := c.conn.Write(wctx, websocket.MessageText, data)
			cancel()
			if err != nil {
				log.Debug("ipc: write failed, closing", "error", err.Error())
				c.closeConn(websocket.StatusInternalError, "write failed")
				return
			}
		}
	}
}
