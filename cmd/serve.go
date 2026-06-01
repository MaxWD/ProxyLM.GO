package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"proxylm/internal/api"
	"proxylm/internal/config"
	"proxylm/internal/core"
	"proxylm/internal/core/backends"
	"proxylm/internal/ipc"
	"proxylm/internal/logging"
	"proxylm/internal/storage"
)

// daemonVersion прокидывается main'ом через cmd/root.go (см. SetVersion) для
// отображения в IPC hello payload подключающимся TUI.
var daemonVersion = "dev"

// SetDaemonVersion вызывается из cmd/root.go при SetVersion, чтобы Hub отдавал
// корректную версию подключённым TUI.
func SetDaemonVersion(v string) { daemonVersion = v }

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Запустить daemon (HTTP-прокси + IPC WebSocket)",
	Long: `Запускает daemon в foreground.

При отсутствии config.yaml рядом с бинарником он создаётся из встроенного шаблона.
БД (proxylm.db) создаётся при первом старте автоматически.

Для запуска как системного сервиса используйте: proxylm service install && proxylm service start.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfgPath, err := cmd.Flags().GetString("config")
		if err != nil {
			return err
		}
		return runDaemon(cmd.Context(), cfgPath)
	},
}

func init() {
	serveCmd.Flags().String("config", "", "Путь к config.yaml (по умолчанию — рядом с бинарником)")
	rootCmd.AddCommand(serveCmd)
}

// runDaemon — точка входа daemon'а. Используется обоими режимами: foreground и service.
// При запуске через kardianos/service внешний ctx идёт от Runner.Stop.
func runDaemon(parent context.Context, cfgPath string) error {
	if cfgPath == "" {
		cfgPath = config.DefaultConfigPath()
	}
	cfg, created, err := config.LoadOrCreate(cfgPath, defaultConfigTemplate)
	if err != nil {
		return fmt.Errorf("конфиг %s: %w", cfgPath, err)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("конфиг %s: %w", cfgPath, err)
	}

	log := logging.Init(cfg.Proxy.LogLevel, nil)
	if created != "" {
		log.Info("config файл создан из встроенного шаблона", "path", created)
	}
	log.Info("конфиг загружен", "path", cfgPath, "backends", len(cfg.Backends))
	for _, w := range cfg.Warnings() {
		log.Warn("конфиг: " + w)
	}

	// Обработка сигналов для foreground-режима.
	ctx, cancel := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// 1) Storage
	dbPath := config.ResolveRelative(cfg.Storage.DatabasePath)
	db, err := storage.Open(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("открыть БД %s: %w", dbPath, err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		return fmt.Errorf("миграции: %w", err)
	}
	if cfg.Storage.VacuumOnStart {
		if err := db.Vacuum(ctx); err != nil {
			log.Warn("vacuum завершился с ошибкой", "error", err.Error())
		}
	}
	history := storage.NewHistory(db)
	log.Info("БД открыта", "path", dbPath)

	// При предыдущей остановке могли остаться записи pending/running — они навечно
	// зависнут в TUI как «queued». Переводим их в cancelled, чтобы история была честной.
	if n, err := history.CancelStale(ctx); err != nil {
		log.Warn("не удалось отменить stale-запросы при старте", "error", err.Error())
	} else if n > 0 {
		log.Info("отменены stale-запросы предыдущей сессии", "count", n)
	}

	// 2) Backends + ServerInfo
	servers := make([]*core.ServerInfo, 0, len(cfg.Backends))
	backendMap := make(map[string]backends.Backend, len(cfg.Backends))
	for _, b := range cfg.Backends {
		timeout := time.Duration(b.TimeoutSeconds) * time.Second
		var bk backends.Backend
		switch b.Type {
		case config.BackendTypeAnthropic:
			bk, err = backends.NewAnthropic(b.Name, b.URL, b.APIKey, timeout)
		default:
			var o *backends.OpenAI
			o, err = backends.NewOpenAI(b.Name, b.URL, b.APIKey, timeout)
			if o != nil {
				// type → kind нативной пробы загруженных моделей (ollama/lmstudio/
				// llamacpp). Для "openai"/пусто проба отключена (no-op).
				o.SetProbeKind(b.Type)
				bk = o
			}
		}
		if err != nil {
			return fmt.Errorf("backend %q: %w", b.Name, err)
		}
		srv := core.NewServerInfo(b.Name, b.URL, b.Priority, b.Type, timeout, b.APIKey)
		servers = append(servers, srv)
		backendMap[b.Name] = bk
	}

	// 3) Discovery
	disc := core.NewDiscovery(
		time.Duration(cfg.Discovery.IntervalSeconds)*time.Second,
		cfg.Discovery.UnhealthyAfterFailedPolls,
		log)
	for i, b := range cfg.Backends {
		disc.AddServer(servers[i], backendMap[b.Name], b.Models)
	}

	// C1: initial healthcheck — всегда, независимо от cfg.Discovery.Enabled.
	// Один синхронный poll на старте: сразу знаем, кто healthy.
	// Если poll провалился — сервер помечается unhealthy, старт не блокируется (U-3).
	disc.InitialHealthcheck(ctx)

	if cfg.Discovery.Enabled {
		go disc.Run(ctx)
	}

	// 4) Router + Scheduler
	router := core.NewRouter(servers, core.RouterStrategy(cfg.Routing.Strategy))
	retry := core.RetryConfig{
		MaxAttempts:    cfg.Retry.MaxAttempts,
		InitialBackoff: time.Duration(cfg.Retry.InitialBackoffMs) * time.Millisecond,
		MaxBackoff:     time.Duration(cfg.Retry.MaxBackoffMs) * time.Millisecond,
	}
	perf := core.NewPerfTracker()
	sched := core.NewSchedulerWithOptions(servers, router, retry, log, core.SchedulerOptions{
		MaxConsecutivePerModel: cfg.Scheduler.MaxConsecutivePerModel,
	})
	sched.SetPerfTracker(perf)
	sched.Start(ctx)

	// 5) Retention (история): простой ticker под ctx.
	if cfg.Storage.HistoryRetentionDays > 0 {
		go runRetention(ctx, history, time.Duration(cfg.Storage.HistoryRetentionDays)*24*time.Hour, log)
	}

	// 6) IPC Hub (WebSocket publisher) + HTTP API
	// liveTail — общий in-memory стор «хвоста» генерации: пишется streaming-
	// обработчиком, читается Hub'ом при сборке snapshot'а. В БД/логи не попадает.
	liveTail := core.NewLiveTail()
	hub := ipc.NewHub(daemonVersion, servers, history, 1*time.Second, cfg.Retry.MaxAttempts, cfg.TUI.ShowCompletedMinutes, perf, liveTail, log)
	go hub.Run(ctx)

	auth := api.NewAuthRegistry(cfg.Auth)
	srv := api.New(cfg.Proxy, api.Deps{
		Auth:     auth,
		Sched:    sched,
		Backends: backendMap,
		Servers:  servers,
		History:  history,
		Compat:   cfg.Compat,
		LiveTail: liveTail,
		Log:      log,
	})
	srv.SetAdminStream(hub.Handler())
	if err := srv.ListenAndServe(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("HTTP server: %w", err)
	}

	log.Info("ожидание завершения worker'ов планировщика…")
	sched.Wait()
	log.Info("daemon остановлен")
	return nil
}

func runRetention(ctx context.Context, history *storage.History, retention time.Duration, log interface{ Warn(string, ...any) }) {
	t := time.NewTicker(1 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cutoff := time.Now().Add(-retention)
			n, err := history.DeleteOlderThan(ctx, cutoff)
			if err != nil {
				log.Warn("retention sweep failed", "error", err.Error())
				continue
			}
			if n > 0 {
				log.Warn("удалено старых записей истории", "count", n)
			}
		}
	}
}
