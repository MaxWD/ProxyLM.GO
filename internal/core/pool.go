package core

import (
	"sort"
	"sync"
)

// JobPool — общий FIFO-пул для стратегии deferred_model_then_capable.
//
// В отличие от push-стратегий (model_affinity_least_busy, least_busy, round_robin),
// здесь Submit не выбирает конкретный сервер заранее. Job лежит в пуле, и
// освобождающийся worker сам тянет совместимое (pull-семантика). Это позволяет
// балансировать нагрузку пропорционально скорости серверов, когда модель есть
// на нескольких узлах: пока один обрабатывает, второй тоже подхватывает работу
// из общей очереди, а не простаивает из-за affinity-привязки первого.
//
// Конкуренция:
//   - Все операции под одним mu, чтобы Push/PopFor/Remove были атомарными
//     друг относительно друга.
//   - Внутренний slice сохраняет FIFO-порядок (хвост = append).
//   - Поле visited живёт в Job под этим же mu — это безопасно, потому что
//     visited читается только из PopFor и пишется только из MarkVisited, и
//     обе операции под локом пула.
type JobPool struct {
	mu   sync.Mutex
	jobs []*Job
}

// NewJobPool создаёт пустой пул.
func NewJobPool() *JobPool {
	return &JobPool{}
}

// Push добавляет Job в конец очереди.
func (p *JobPool) Push(j *Job) {
	p.mu.Lock()
	p.jobs = append(p.jobs, j)
	p.mu.Unlock()
}

// PopFor возвращает следующий Job, который сервер srv может выполнить,
// в порядке приоритета (INV-2/3 — drain current model FIFO перед swap):
//
//  1. Если у сервера загружена модель (current != "") — первый Job в очереди,
//     для которого j.Model == current и сервер не помечен в j.visited.
//  2. Если для current_model задач нет — первый Job, для которого
//     j.Model присутствует в s.Models и сервер не в j.visited.
//
// Возвращает nil, если совместимого Job в очереди нет.
//
// Реактивный crash-detect (баг #78, v0.9.0): unhealthy сервер не получает
// Job из пула — иначе после connection-refused worker крутил бы pool и
// отправлял туда же следующую задачу. Discovery восстановит healthy и
// разбудит worker'а через Wake.
//
// PopFor читает s.CurrentModel / s.Models под собственными локами s,
// потом удаляет элемент из своего slice — без вложенной блокировки серверного mu
// при работе со slice (порядок: p.mu → s.mu, чтобы избежать инверсии с worker'ом).
func (p *JobPool) PopFor(s *ServerInfo) *Job {
	if !s.Healthy.Load() {
		return nil
	}
	current := s.CurrentModelString()
	allowed := s.modelsSnapshot()

	p.mu.Lock()
	defer p.mu.Unlock()

	// Шаг 1: drain current_model.
	if current != "" {
		for i, j := range p.jobs {
			if j.Model != current {
				continue
			}
			if j.visited != nil && j.visited[s.Name] {
				continue
			}
			p.jobs = append(p.jobs[:i], p.jobs[i+1:]...)
			return j
		}
	}
	// Шаг 2: первый совместимый.
	for i, j := range p.jobs {
		if !allowed[j.Model] {
			continue
		}
		if j.visited != nil && j.visited[s.Name] {
			continue
		}
		p.jobs = append(p.jobs[:i], p.jobs[i+1:]...)
		return j
	}
	return nil
}

// PopForFairShare — реализация стратегии fair_share_round_robin
// (FUTURE.md #8 → v0.10.0).
//
// Базовая семантика совпадает с PopFor (drain current_model FIFO, затем —
// первый совместимый Job из очереди). Дополнительно учитывается счётчик
// последовательных диспатчей одной модели на этом сервере:
//
//   - если maxConsecutive ≤ 0 — лимит отключён, поведение идентично PopFor;
//   - если ConsecutiveModelCount ≥ maxConsecutive И в очереди есть Job
//     под другую модель, которую этот сервер тоже умеет обрабатывать —
//     ПРИНУДИТЕЛЬНО берём этот «другой» Job (после dispatch'а сервер
//     переключит current_model и счётчик сбросится в 1);
//   - если других совместимых моделей в очереди нет — продолжаем drain'ить
//     текущую (никогда не блокируемся).
//
// Это решает проблему голодания (R-1, SRS §9.2): непрерывный поток запросов
// к модели A не запирает в очереди запросы к моделям B/C/…, если у сервера
// они тоже есть в Models. Цена — лишняя загрузка модели каждые N запросов,
// которая отображается в perf-регрессии (t_load × loaded=1).
//
// Реактивный crash-detect: unhealthy сервер не получает Job из пула — см. PopFor.
func (p *JobPool) PopForFairShare(s *ServerInfo, maxConsecutive int) *Job {
	if !s.Healthy.Load() {
		return nil
	}
	current := s.CurrentModelString()
	allowed := s.modelsSnapshot()

	s.Lock()
	forceSwitch := maxConsecutive > 0 &&
		s.LastDispatchedModel != "" &&
		s.ConsecutiveModelCount >= maxConsecutive
	s.Unlock()

	p.mu.Lock()
	defer p.mu.Unlock()

	// Forced switch: ищем первый совместимый Job под модель ≠ current.
	// Если такого нет — fall through на обычный drain (лучше продолжить
	// одной моделью, чем блокироваться).
	if forceSwitch && current != "" {
		for i, j := range p.jobs {
			if !allowed[j.Model] || j.Model == current {
				continue
			}
			if j.visited != nil && j.visited[s.Name] {
				continue
			}
			p.jobs = append(p.jobs[:i], p.jobs[i+1:]...)
			return j
		}
	}

	// Обычный путь: drain current_model FIFO.
	if current != "" {
		for i, j := range p.jobs {
			if j.Model != current {
				continue
			}
			if j.visited != nil && j.visited[s.Name] {
				continue
			}
			p.jobs = append(p.jobs[:i], p.jobs[i+1:]...)
			return j
		}
	}
	// Очередь не содержит current — берём первый совместимый.
	for i, j := range p.jobs {
		if !allowed[j.Model] {
			continue
		}
		if j.visited != nil && j.visited[s.Name] {
			continue
		}
		p.jobs = append(p.jobs[:i], p.jobs[i+1:]...)
		return j
	}
	return nil
}

// PopForCoverage — реализация стратегии preserve_model_coverage.
//
// В отличие от PopFor (всегда сначала drain current_model FIFO), этот выбор
// активно сохраняет уникальное «покрытие» модели в кластере, но не залипает
// на текущей модели, если другой сервер её тоже держит:
//
//  1. HEAD очереди (первый совместимый job, который этот сервер может взять)
//     совпадает с current_model сервера или сервер ещё без модели — берём HEAD.
//  2. HEAD на другую модель:
//     a. Если у другого healthy-сервера CurrentModel == current — берём HEAD
//     (swap безопасен: «старая» модель остаётся доступной на другом сервере).
//     b. Если этот сервер — единственный держатель current — ищем в очереди
//     первый job под current_model и берём его (защита покрытия). Если таких
//     нет — берём HEAD (swap неизбежен).
//
// allServers — полный список серверов планировщика; нужен для проверки покрытия.
// Конкуренция: всё под p.mu; чтения s.* через атомики/собственные локи s.
//
// Реактивный crash-detect: см. PopFor — unhealthy сервер не получает Job.
func (p *JobPool) PopForCoverage(s *ServerInfo, allServers []*ServerInfo) *Job {
	if !s.Healthy.Load() {
		return nil
	}
	current := s.CurrentModelString()
	allowed := s.modelsSnapshot()

	p.mu.Lock()
	defer p.mu.Unlock()

	headIdx := -1
	for i, j := range p.jobs {
		if !allowed[j.Model] {
			continue
		}
		if j.visited != nil && j.visited[s.Name] {
			continue
		}
		headIdx = i
		break
	}
	if headIdx == -1 {
		return nil
	}
	head := p.jobs[headIdx]

	// Случай 1а: на сервере ещё нет модели — свободно берём HEAD.
	if current == "" {
		p.jobs = append(p.jobs[:headIdx], p.jobs[headIdx+1:]...)
		return head
	}
	// Случай 1б: HEAD под текущую модель — FIFO под current.
	if head.Model == current {
		p.jobs = append(p.jobs[:headIdx], p.jobs[headIdx+1:]...)
		return head
	}
	// Случай 2: HEAD под другую модель. Смотрим, кто ещё держит current.
	otherHasCurrent := false
	for _, srv := range allServers {
		if srv == s {
			continue
		}
		if !srv.Healthy.Load() {
			continue
		}
		if srv.CurrentModelString() == current {
			otherHasCurrent = true
			break
		}
	}
	if otherHasCurrent {
		// swap безопасен — берём HEAD.
		p.jobs = append(p.jobs[:headIdx], p.jobs[headIdx+1:]...)
		return head
	}
	// Этот сервер — единственный держатель current. Сохраняем покрытие:
	// ищем job под current. Если нет — swap всё равно неизбежен, берём HEAD.
	for i, j := range p.jobs {
		if j.Model != current {
			continue
		}
		if j.visited != nil && j.visited[s.Name] {
			continue
		}
		p.jobs = append(p.jobs[:i], p.jobs[i+1:]...)
		return j
	}
	p.jobs = append(p.jobs[:headIdx], p.jobs[headIdx+1:]...)
	return head
}

// Remove удаляет Job по ID. Возвращает true, если Job был в очереди (Submit
// вызывает это при cancel'е контекста, чтобы корректно завершить запрос, который
// ещё не успели подхватить).
func (p *JobPool) Remove(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, j := range p.jobs {
		if j.ID == id {
			p.jobs = append(p.jobs[:i], p.jobs[i+1:]...)
			return true
		}
	}
	return false
}

// MarkVisited помечает сервер как «упавший на прошлой попытке» — Pool
// не отдаст этот Job тому же серверу СЛЕДУЮЩИМ. Semантика — rolling
// exclusion size 1: каждая новая отметка ЗАМЕЩАЕТ предыдущую, чтобы Job
// не «выгорал» по всем серверам подряд. Когда дальше упадёт уже другой
// сервер, изначально упавший снова становится кандидатом.
//
// Конфликт с pool-семантикой single-server: если совместимый сервер
// один и он же помечен — PopFor вернёт nil. submitDeferred это ловит
// и очищает visited (см. ClearVisited), чтобы single-server setup мог
// просто ретраить на том же узле.
func (p *JobPool) MarkVisited(j *Job, serverName string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	j.visited = map[string]bool{serverName: true}
}

// ClearVisited сбрасывает множество исключений. Используется submitDeferred
// в крайнем случае, когда rolling-exclusion оставил Job без кандидатов
// (только один совместимый сервер, и он же — последний упавший).
func (p *JobPool) ClearVisited(j *Job) {
	p.mu.Lock()
	defer p.mu.Unlock()
	j.visited = nil
}

// Len возвращает текущее число Job'ов в пуле (для метрик/тестов).
func (p *JobPool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.jobs)
}

// drainAll атомарно забирает все Job'ы из пула и обнуляет внутренний slice.
// Используется failPending при остановке планировщика, чтобы корректно ответить
// ошибкой всем висящим запросам.
func (p *JobPool) drainAll() []*Job {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.jobs
	p.jobs = nil
	return out
}

// modelsSnapshot возвращает множество имён моделей сервера. Под s.mu.
func (s *ServerInfo) modelsSnapshot() map[string]bool {
	s.Lock()
	defer s.Unlock()
	out := make(map[string]bool, len(s.Models))
	for _, m := range s.Models {
		out[m.Name] = true
	}
	return out
}

// hasModel возвращает true, если у сервера загружено имя model (под s.mu).
// Используется submitDeferred для проверки совместимости и pickIdleByPriority.
func (s *ServerInfo) hasModel(model string) bool {
	s.Lock()
	defer s.Unlock()
	for _, m := range s.Models {
		if m.Name == model {
			return true
		}
	}
	return false
}

// pickIdleByPriority выбирает свободный (!InFlight) healthy сервер с минимальным
// Priority, у которого модель model в Models и который не помечен visited.
// Возвращает nil, если такого нет.
//
// Используется submitDeferred для холодного старта (Q1): когда несколько серверов
// свободны и приходит новый Job, его должен подхватить самый приоритетный.
func pickIdleByPriority(servers []*ServerInfo, model string, visited map[string]bool) *ServerInfo {
	type cand struct {
		srv  *ServerInfo
		prio int
	}
	idle := make([]cand, 0, len(servers))
	for _, srv := range servers {
		if !srv.Healthy.Load() {
			continue
		}
		if srv.InFlight.Load() {
			continue
		}
		if visited != nil && visited[srv.Name] {
			continue
		}
		if !srv.hasModel(model) {
			continue
		}
		idle = append(idle, cand{srv: srv, prio: srv.Priority})
	}
	if len(idle) == 0 {
		return nil
	}
	sort.SliceStable(idle, func(i, j int) bool { return idle[i].prio < idle[j].prio })
	return idle[0].srv
}

// anyHealthyCompatible возвращает true, если есть хоть один healthy сервер
// с моделью model, не помеченный в visited. Используется submitDeferred:
//   - с visited=nil — синхронная проверка «модель ещё поддерживается»
//     (ErrNoServer на старте, или решение «стоит ли пытаться дальше»).
//   - с visited=j.visited — «есть ли другой совместимый помимо упавшего»;
//     если нет, submitDeferred делает ClearVisited (rolling exclusion
//     не блокирует ретрай при single-server setup).
func anyHealthyCompatible(servers []*ServerInfo, model string, visited map[string]bool) bool {
	for _, srv := range servers {
		if !srv.Healthy.Load() {
			continue
		}
		if visited != nil && visited[srv.Name] {
			continue
		}
		if srv.hasModel(model) {
			return true
		}
	}
	return false
}
