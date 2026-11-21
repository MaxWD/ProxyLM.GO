// Package daemon — рантайм-слой daemon'а поверх подсистем: редактирование
// конфигурации на лету (config_get / config_update по admin-WS).
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"proxylm/internal/api"
	"proxylm/internal/config"
	"proxylm/internal/core"
	"proxylm/internal/ipc"
	"proxylm/internal/logging"
)

// Targets — подсистемы, которым Applier пушит hot-изменения конфигурации.
// Любое поле может быть nil (соответствующее изменение просто не применяется).
type Targets struct {
	Auth   *api.AuthRegistry
	Server *api.Server
	Router *core.Router
	Sched  *core.Scheduler
	Disc   *core.Discovery
	Hub    *ipc.Hub
	// RetentionDays — storage.history_retention_days для runMaintenance.
	RetentionDays *atomic.Int64
}

// Applier хранит «работающую» конфигурацию (то, что реально используют
// подсистемы), путь к файлу и сериализует обновления. Реализует ipc.ConfigService.
type Applier struct {
	path string
	log  *slog.Logger
	t    Targets

	mu        sync.Mutex     // сериализует State/Update
	effective *config.Config // запущенная конфигурация + уже применённые hot-изменения

	// pending — снимок restart_pending для hello. Обновляется под mu ДО пуша
	// hot-изменений (Hub.SetShowCompletedMinutes вызывает RestartPending() из
	// того же потока, поэтому отдельный атомарный снимок, а не mu).
	pending atomic.Pointer[[]string]
}

// New создаёт Applier. running — конфиг, с которым стартовал daemon (не
// изменяется; Applier хранит копию).
func New(path string, running *config.Config, t Targets, log *slog.Logger) *Applier {
	if log == nil {
		log = slog.Default()
	}
	a := &Applier{path: path, log: log, t: t, effective: running.Clone()}
	empty := []string{}
	a.pending.Store(&empty)
	return a
}

var _ ipc.ConfigService = (*Applier)(nil)

// RestartPending возвращает поля, отличающиеся от работающей конфигурации.
func (a *Applier) RestartPending() []string {
	if p := a.pending.Load(); p != nil {
		return append([]string{}, *p...)
	}
	return []string{}
}

// State читает файл и возвращает masked-конфиг, схему и ревизию.
func (a *Applier) State(_ context.Context) ipc.ConfigStatePayload {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := ipc.ConfigStatePayload{Path: a.path, Schema: config.ConfigSchema(), RestartPending: []string{}}
	data, rev, err := config.ReadRevision(a.path)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	cfg, err := config.Parse(data)
	if err != nil {
		st.Revision = rev
		st.Error = err.Error()
		return st
	}
	st.Revision = rev
	st.Config = config.Masked(cfg)
	st.RestartPending = a.computePending(cfg)
	a.pending.Store(&st.RestartPending)
	return st
}

// computePending — пути, по которым файл отличается от работающей конфигурации.
// Вызывать под mu.
func (a *Applier) computePending(file *config.Config) []string {
	p := config.Diff(a.effective, file)
	if p == nil {
		return []string{}
	}
	return p
}

// Update применяет config_update: объединяет с текущим файлом, валидирует,
// сохраняет (с проверкой ревизии) и пушит hot-изменения подсистемам.
func (a *Applier) Update(ctx context.Context, p ipc.ConfigUpdatePayload) ipc.ConfigResultPayload {
	a.mu.Lock()
	defer a.mu.Unlock()

	res := ipc.ConfigResultPayload{Applied: []string{}, RestartRequired: []string{}, RestartPending: a.RestartPending()}
	if p.Config == nil {
		res.Error = "config is required"
		return res
	}
	data, rev, err := config.ReadRevision(a.path)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Revision = rev
	if rev != p.Revision {
		return a.conflict(res, data)
	}
	cur, err := config.Parse(data)
	if err != nil {
		res.Error = err.Error()
		return res
	}

	next, generated, errs := config.MergeUpdate(cur, p.Config, p.Renames, p.SecretOps)
	errs = append(errs, next.ValidateFields()...)
	if len(errs) > 0 {
		res.Errors = errs
		res.Error = "validation failed"
		return res
	}

	// Как при Load: нули полей с дефолтами читаются после рестарта как дефолт, поэтому
	// и работающая конфигурация нормализуется так же (иначе — ложный restart_pending).
	config.ApplyDefaults(next)

	changed := config.Diff(cur, next)
	if len(changed) > 0 || len(p.Renames) > 0 {
		newRev, err := config.SaveRenamed(a.path, next, p.Revision, p.Renames)
		switch {
		case errors.Is(err, config.ErrConflict):
			return a.conflict(res, nil)
		case err != nil:
			res.Error = fmt.Sprintf("save failed: %v", err)
			return res
		}
		rev = newRev
	}
	res.Revision = rev

	// Новое состояние «работающей» конфигурации после пуша hot-изменений.
	hotPaths, _ := config.DiffByClass(config.Diff(a.effective, next))
	_, restartChanged := config.DiffByClass(changed)
	newEffective := a.effective.Clone()
	copyHot(newEffective, next)
	pending := config.Diff(newEffective, next)
	if pending == nil {
		pending = []string{}
	}
	a.pending.Store(&pending) // до пуша: hello из Hub.SetShowCompletedMinutes увидит актуальное
	a.applyHot(a.effective, next)
	a.effective = newEffective

	if hotPaths != nil {
		res.Applied = hotPaths
	}
	if restartChanged != nil {
		res.RestartRequired = restartChanged
	}
	res.RestartPending = append([]string{}, pending...)
	if len(generated) > 0 {
		res.Generated = generated
	}
	res.OK = true
	// Аудит: только пути, значения (и секреты) в лог не попадают.
	a.log.InfoContext(ctx, "config updated", "paths", changed, "applied", len(res.Applied), "restart_required", len(res.RestartRequired))
	return res
}

// conflict заполняет результат при расхождении ревизий.
func (a *Applier) conflict(res ipc.ConfigResultPayload, data []byte) ipc.ConfigResultPayload {
	res.Conflict = true
	res.Error = "conflict: config file changed since it was read; reload it"
	if data != nil {
		if cfg, err := config.Parse(data); err == nil {
			res.RestartPending = a.computePending(cfg)
		}
	}
	return res
}

// copyHot копирует из src в dst только hot-поля.
func copyHot(dst, src *config.Config) {
	dst.Auth.APIKeys = append([]config.APIKey(nil), src.Auth.APIKeys...)
	dst.Proxy.LogLevel = src.Proxy.LogLevel
	dst.Routing.Strategy = src.Routing.Strategy
	dst.Scheduler = src.Scheduler
	dst.Retry = src.Retry
	dst.Discovery.IntervalSeconds = src.Discovery.IntervalSeconds
	dst.Discovery.UnhealthyAfterFailedPolls = src.Discovery.UnhealthyAfterFailedPolls
	dst.Storage.HistoryRetentionDays = src.Storage.HistoryRetentionDays
	dst.TUI = src.TUI
	dst.Compat = src.Compat
}

// applyHot пушит в подсистемы hot-поля, отличающиеся между old (работающая
// конфигурация) и next. Каждый setter — атомарная замена значения.
func (a *Applier) applyHot(old, next *config.Config) {
	t := a.t
	if !sameKeys(old.Auth.APIKeys, next.Auth.APIKeys) && t.Auth != nil {
		t.Auth.Update(next.Auth)
	}
	if old.Proxy.LogLevel != next.Proxy.LogLevel {
		logging.SetLevel(next.Proxy.LogLevel)
	}
	if old.Routing.Strategy != next.Routing.Strategy && t.Router != nil {
		t.Router.SetStrategy(core.RouterStrategy(next.Routing.Strategy))
		if t.Sched != nil {
			t.Sched.WakeAll() // воркеры перепроверяют очереди/пул в новом режиме
		}
	}
	if t.Sched != nil {
		if old.Scheduler.MaxConsecutivePerModel != next.Scheduler.MaxConsecutivePerModel {
			t.Sched.SetMaxConsecutivePerModel(next.Scheduler.MaxConsecutivePerModel)
		}
		if old.Retry != next.Retry {
			t.Sched.SetRetry(core.RetryConfig{
				MaxAttempts:    next.Retry.MaxAttempts,
				InitialBackoff: time.Duration(next.Retry.InitialBackoffMs) * time.Millisecond,
				MaxBackoff:     time.Duration(next.Retry.MaxBackoffMs) * time.Millisecond,
			})
		}
	}
	if t.Disc != nil {
		if old.Discovery.IntervalSeconds != next.Discovery.IntervalSeconds {
			t.Disc.SetInterval(time.Duration(next.Discovery.IntervalSeconds) * time.Second)
		}
		if old.Discovery.UnhealthyAfterFailedPolls != next.Discovery.UnhealthyAfterFailedPolls {
			t.Disc.SetUnhealthyAfter(next.Discovery.UnhealthyAfterFailedPolls)
		}
	}
	if old.Storage.HistoryRetentionDays != next.Storage.HistoryRetentionDays {
		if t.RetentionDays != nil {
			t.RetentionDays.Store(int64(next.Storage.HistoryRetentionDays))
		}
		if t.Hub != nil {
			t.Hub.SetRetentionDays(next.Storage.HistoryRetentionDays)
		}
	}
	if t.Hub != nil {
		if old.Retry.MaxAttempts != next.Retry.MaxAttempts {
			t.Hub.SetMaxAttempts(next.Retry.MaxAttempts)
		}
		if old.TUI.ShowCompletedMinutes != next.TUI.ShowCompletedMinutes {
			t.Hub.SetShowCompletedMinutes(next.TUI.ShowCompletedMinutes)
		}
	}
	if old.Compat != next.Compat && t.Server != nil {
		t.Server.SetCompat(next.Compat)
	}
}

func sameKeys(a, b []config.APIKey) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
