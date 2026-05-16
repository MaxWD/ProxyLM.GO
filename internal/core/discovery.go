package core

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"proxylm/internal/core/backends"
)

// Discovery — периодический воркер, опрашивающий /v1/models на каждом бэкенде
// и обновляющий ServerInfo.Models / Healthy. При unhealthyAfterFailedPolls
// подряд неуспехах сервер помечается Healthy=false.
type Discovery struct {
	interval                  time.Duration
	unhealthyAfterFailedPolls int
	log                       *slog.Logger

	mu      sync.Mutex
	entries []*discoveryEntry
}

type discoveryEntry struct {
	server  *ServerInfo
	backend backends.Backend

	// explicitModels — если не пусто, discovery для сервера выключен;
	// мы только пингуем /v1/models для healthcheck, но список не обновляем.
	explicitModels []string

	failedPolls int
}

// NewDiscovery создаёт пустого воркера. Серверы добавляются через AddServer.
// interval — пауза между циклами опроса. unhealthyAfterFailedPolls — после
// скольких подряд неуспешных опросов сервер помечается unhealthy.
func NewDiscovery(interval time.Duration, unhealthyAfterFailedPolls int, log *slog.Logger) *Discovery {
	if log == nil {
		log = slog.Default()
	}
	return &Discovery{
		interval:                  interval,
		unhealthyAfterFailedPolls: unhealthyAfterFailedPolls,
		log:                       log,
	}
}

// AddServer регистрирует сервер для опроса. explicitModels из config.backends[].models —
// если задан, источником списка моделей становится конфиг, а discovery лишь проверяет
// доступность (healthy/unhealthy).
func (d *Discovery) AddServer(s *ServerInfo, b backends.Backend, explicitModels []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.entries = append(d.entries, &discoveryEntry{
		server:         s,
		backend:        b,
		explicitModels: append([]string(nil), explicitModels...),
	})
	if len(explicitModels) > 0 {
		// Сразу выставим список из конфига, чтобы router мог работать
		// ещё до первого цикла discovery.
		s.Lock()
		s.Models = toModelInfos(explicitModels)
		s.Unlock()
	}
}

// Run блокирует горутину и опрашивает все зарегистрированные серверы каждые interval,
// пока ctx не отменён. Один цикл — параллельный sweep по всем серверам.
func (d *Discovery) Run(ctx context.Context) {
	// Первый цикл — сразу, без ожидания interval, чтобы daemon после старта
	// быстрее увидел доступные модели.
	d.pollAll(ctx)

	t := time.NewTicker(d.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.pollAll(ctx)
		}
	}
}

func (d *Discovery) pollAll(ctx context.Context) {
	d.mu.Lock()
	entries := append([]*discoveryEntry(nil), d.entries...)
	d.mu.Unlock()

	var wg sync.WaitGroup
	for _, e := range entries {
		wg.Add(1)
		go func(e *discoveryEntry) {
			defer wg.Done()
			d.pollOne(ctx, e)
		}(e)
	}
	wg.Wait()
}

func (d *Discovery) pollOne(ctx context.Context, e *discoveryEntry) {
	models, err := e.backend.ListModels(ctx)
	if err != nil {
		e.failedPolls++
		if e.failedPolls >= d.unhealthyAfterFailedPolls && e.server.Healthy.Load() {
			e.server.Healthy.Store(false)
			d.log.Warn("сервер помечен unhealthy",
				"server", e.server.Name,
				"failed_polls", e.failedPolls,
				"error", err.Error())
		} else {
			d.log.Debug("discovery: ошибка опроса",
				"server", e.server.Name,
				"failed_polls", e.failedPolls,
				"error", err.Error())
		}
		return
	}

	e.failedPolls = 0
	if !e.server.Healthy.Load() {
		e.server.Healthy.Store(true)
		d.log.Info("сервер помечен healthy", "server", e.server.Name, "models", len(models))
		// Будим воркера сервера: после восстановления он немедленно проверит
		// очередь (push-режим) или общий pool (pull-режим). Без этого воркер
		// продолжал бы спать на Notify до следующей входящей задачи, и
		// pending'и из очереди ждали бы случайного триггера.
		e.server.Wake()
	}
	if len(e.explicitModels) > 0 {
		// Список фиксирован в конфиге — не перезаписываем.
		return
	}
	e.server.Lock()
	e.server.Models = toModelInfos(models)
	e.server.Unlock()
}

func toModelInfos(names []string) []ModelInfo {
	out := make([]ModelInfo, 0, len(names))
	for _, n := range names {
		if n != "" {
			out = append(out, ModelInfo{Name: n})
		}
	}
	return out
}
