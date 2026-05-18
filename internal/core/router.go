package core

import (
	"fmt"
	"sort"
	"sync/atomic"
)

// RouterStrategy — стратегия выбора сервера для модели.
type RouterStrategy string

const (
	RouterModelAffinityLeastBusy   RouterStrategy = "model_affinity_least_busy"
	RouterRoundRobin               RouterStrategy = "round_robin"
	RouterLeastBusy                RouterStrategy = "least_busy"
	RouterDeferredModelThenCapable RouterStrategy = "deferred_model_then_capable"
	RouterPreserveModelCoverage    RouterStrategy = "preserve_model_coverage"
)

// Router — выбор сервера для запроса. Имеет доступ к списку серверов и реализует
// стратегии. Состояние хранится в ServerInfo (атомики), поэтому Router stateless
// и потокобезопасен. Round-robin счётчик — единственное локальное состояние.
type Router struct {
	servers  []*ServerInfo
	strategy RouterStrategy
	rrIdx    atomic.Int64 // только для round_robin; атомарный счётчик без блокировок
}

// ErrNoServer — нет ни одного healthy-сервера с указанной моделью.
type ErrNoServer struct{ Model string }

func (e *ErrNoServer) Error() string {
	return fmt.Sprintf("router: нет healthy-сервера с моделью %q", e.Model)
}

// NewRouter создаёт router с заданной стратегией над фиксированным списком серверов.
func NewRouter(servers []*ServerInfo, strategy RouterStrategy) *Router {
	return &Router{servers: servers, strategy: strategy}
}

// Pick выбирает следующий сервер для запроса с моделью model, исключая серверы
// из exclude (используется для failover: уже посещённые серверы). Учитывает только
// healthy-серверы, у которых model присутствует в Models (INV-8).
func (r *Router) Pick(model string, exclude map[string]bool) (*ServerInfo, error) {
	candidates := r.candidates(model, exclude)
	if len(candidates) == 0 {
		return nil, &ErrNoServer{Model: model}
	}
	switch r.strategy {
	case RouterRoundRobin:
		return r.pickRoundRobin(candidates), nil
	case RouterLeastBusy:
		return pickLeastBusy(candidates), nil
	case RouterDeferredModelThenCapable, RouterPreserveModelCoverage, RouterModelAffinityLeastBusy:
		fallthrough
	default:
		return pickModelAffinityLeastBusy(model, candidates), nil
	}
}

func (r *Router) candidates(model string, exclude map[string]bool) []*ServerInfo {
	out := make([]*ServerInfo, 0, len(r.servers))
	for _, s := range r.servers {
		if exclude[s.Name] {
			continue
		}
		if !s.Healthy.Load() {
			continue
		}
		if !serverHasModel(s, model) {
			continue
		}
		out = append(out, s)
	}
	return out
}

func serverHasModel(s *ServerInfo, model string) bool {
	s.Lock()
	defer s.Unlock()
	for _, m := range s.Models {
		if m.Name == model {
			return true
		}
	}
	return false
}

// pickModelAffinityLeastBusy: сначала отделяем «модель уже загружена» (CurrentModel == model)
// от остальных. В каждой группе ищем сервер с наименьшей нагрузкой (queue len + inflight).
// Tiebreaker — наименьший Priority.
func pickModelAffinityLeastBusy(model string, candidates []*ServerInfo) *ServerInfo {
	preferred := make([]*ServerInfo, 0, len(candidates))
	others := make([]*ServerInfo, 0, len(candidates))
	for _, s := range candidates {
		if s.CurrentModelString() == model {
			preferred = append(preferred, s)
		} else {
			others = append(others, s)
		}
	}
	if len(preferred) > 0 {
		return pickLeastBusy(preferred)
	}
	return pickLeastBusy(others)
}

func pickLeastBusy(candidates []*ServerInfo) *ServerInfo {
	type scored struct {
		s    *ServerInfo
		load int
		prio int
	}
	ranked := make([]scored, 0, len(candidates))
	for _, s := range candidates {
		s.Lock()
		ql := len(s.Queue)
		s.Unlock()
		load := ql
		if s.InFlight.Load() {
			load++
		}
		ranked = append(ranked, scored{s: s, load: load, prio: s.Priority})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].load != ranked[j].load {
			return ranked[i].load < ranked[j].load
		}
		return ranked[i].prio < ranked[j].prio
	})
	return ranked[0].s
}

func (r *Router) pickRoundRobin(candidates []*ServerInfo) *ServerInfo {
	// candidates всегда non-empty в этом месте (проверено в Pick).
	// Add(1) - 1: первый вызов возвращает 0, сохраняя порядок a→b→a→...
	idx := (r.rrIdx.Add(1) - 1) % int64(len(candidates))
	return candidates[idx]
}
