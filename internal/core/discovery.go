package core

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"proxylm/internal/core/backends"
)

// InitialHealthcheck выполняет один синхронный poll /v1/models для каждого
// зарегистрированного сервера. Используется при старте daemon'а вне зависимости
// от cfg.discovery.enabled, чтобы сразу выставить Healthy и Models (C1).
//
// Если сервер имеет explicitModels — он помечается healthy без poll'а (FR-25).
// Для серверов без explicit-списка — вызывает ListModels с таймаутом из
// backends[].timeout_seconds (capped 10s). Ошибка → Healthy=false + warn-лог,
// старт не блокируется (U-3 в SRS).
func (d *Discovery) InitialHealthcheck(ctx context.Context) {
	d.mu.Lock()
	entries := append([]*discoveryEntry(nil), d.entries...)
	d.mu.Unlock()

	var wg sync.WaitGroup
	for _, e := range entries {
		wg.Add(1)
		go func(e *discoveryEntry) {
			defer wg.Done()
			// Если модели заданы явно — сервер сразу healthy; poll не нужен.
			if len(e.explicitModels) > 0 {
				e.server.Healthy.Store(true)
				d.log.Info("initial healthcheck: explicit models, marking healthy",
					"server", e.server.Name,
					"models", len(e.explicitModels))
				return
			}
			// Ограничиваем таймаут: берём timeout из ServerInfo, но не более 10s.
			timeout := e.server.Timeout
			const maxInitTimeout = 10 * time.Second
			if timeout <= 0 || timeout > maxInitTimeout {
				timeout = maxInitTimeout
			}
			pollCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			models, err := e.backend.ListModels(pollCtx)
			if err != nil {
				e.server.Healthy.Store(false)
				if errors.Is(err, backends.ErrModelsShape) {
					d.log.Warn("initial healthcheck: /v1/models is not OpenAI-compatible — check backends[].url",
						"server", e.server.Name,
						"url", e.server.URL,
						"error", err.Error())
				} else {
					d.log.Warn("initial healthcheck: server unavailable",
						"server", e.server.Name,
						"error", err.Error())
				}
				return
			}
			e.server.Healthy.Store(true)
			e.server.Lock()
			e.server.Models = toModelInfos(models)
			e.server.Unlock()
			if len(models) == 0 {
				d.log.Warn("server healthy but reports zero models",
					"server", e.server.Name)
			}
			d.log.Info("initial healthcheck: server healthy",
				"server", e.server.Name,
				"models", len(models))
		}(e)
	}
	wg.Wait()
}

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
		shapeErr := errors.Is(err, backends.ErrModelsShape)
		if len(e.explicitModels) > 0 && shapeErr {
			// Explicit-models сервер: /v1/models ответил (endpoint жив), но не
			// в OpenAI-форме. Модели у нас и так берутся из конфига — считаем
			// опрос успешным для целей healthcheck, список не трогаем.
			e.failedPolls = 0
			wasUnhealthy := !e.server.Healthy.Load()
			e.server.Healthy.Store(true)
			if wasUnhealthy {
				d.log.Debug("discovery: explicit-models server healthy despite non-OpenAI /v1/models shape",
					"server", e.server.Name)
				e.server.Wake()
			}
			d.probeLoadedModels(ctx, e)
			return
		}

		e.failedPolls++
		if e.failedPolls >= d.unhealthyAfterFailedPolls && e.server.Healthy.Load() {
			e.server.Healthy.Store(false)
			if shapeErr {
				d.log.Warn("сервер помечен unhealthy: /v1/models is not OpenAI-compatible — check backends[].url",
					"server", e.server.Name,
					"url", e.server.URL,
					"failed_polls", e.failedPolls,
					"error", err.Error())
			} else {
				d.log.Warn("сервер помечен unhealthy",
					"server", e.server.Name,
					"failed_polls", e.failedPolls,
					"error", err.Error())
			}
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
	// Проба реально загруженных в память моделей (опционально, по типу бэкенда).
	// Ошибка пробы НЕ влияет на healthy — это вспомогательная наблюдаемость.
	d.probeLoadedModels(ctx, e)
	if len(e.explicitModels) > 0 {
		// Список фиксирован в конфиге — не перезаписываем.
		return
	}
	if len(models) == 0 {
		d.log.Warn("server healthy but reports zero models", "server", e.server.Name)
	}
	e.server.Lock()
	e.server.Models = toModelInfos(models)
	e.server.Unlock()
}

// probeLoadedModels опрашивает нативный эндпоинт бэкенда (если он реализует
// LoadedModelsProber) и сохраняет снимок реально загруженных моделей в
// ServerInfo. Бэкенды без пробы пропускаются молча.
func (d *Discovery) probeLoadedModels(ctx context.Context, e *discoveryEntry) {
	prober, ok := e.backend.(backends.LoadedModelsProber)
	if !ok {
		return
	}
	loaded, supported, err := prober.LoadedModels(ctx)
	if !supported {
		return
	}
	if err != nil {
		d.log.Debug("discovery: проба загруженных моделей не удалась",
			"server", e.server.Name, "error", err.Error())
		// Не затираем прошлый снимок при разовой ошибке сети.
		return
	}
	e.server.SetLoadedModels(loaded, true)
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
