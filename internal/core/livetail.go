package core

import "sync"

// liveTailMaxRunes — сколько последних рун генерации храним как «хвост».
// ~160 символов достаточно, чтобы оператор видел последние слова ответа,
// и при этом строка помещается в InfoPane без чрезмерного шума.
const liveTailMaxRunes = 160

// LiveTail хранит «хвост» генерации (последние символы ответа) для in-flight
// streaming-запросов, ключ — request ID. Контент существует ТОЛЬКО в памяти и
// ТОЛЬКО пока запрос выполняется: в БД и логи он не пишется (инвариант «в БД/логи
// — только client_name»). Конкурентно-безопасен. Введено в v0.13.0.
//
// Все методы nil-safe: nil-приёмник превращает операции в no-op, чтобы код-пути
// без сконфигурированного LiveTail (часть тестов) работали без проверок.
type LiveTail struct {
	mu sync.RWMutex
	m  map[string]string
}

// NewLiveTail создаёт пустой стор.
func NewLiveTail() *LiveTail {
	return &LiveTail{m: make(map[string]string)}
}

// Append добавляет фрагмент сгенерированного текста к хвосту запроса id и
// обрезает результат до liveTailMaxRunes последних рун. Управляющие символы
// (переводы строк, табы) заменяются пробелами — хвост показывается одной строкой.
func (lt *LiveTail) Append(id, chunk string) {
	if lt == nil || id == "" || chunk == "" {
		return
	}
	lt.mu.Lock()
	defer lt.mu.Unlock()
	combined := lt.m[id] + sanitizeTail(chunk)
	r := []rune(combined)
	if len(r) > liveTailMaxRunes {
		r = r[len(r)-liveTailMaxRunes:]
	}
	lt.m[id] = string(r)
}

// Get возвращает текущий хвост запроса id (или "").
func (lt *LiveTail) Get(id string) string {
	if lt == nil {
		return ""
	}
	lt.mu.RLock()
	defer lt.mu.RUnlock()
	return lt.m[id]
}

// Delete удаляет хвост запроса id. Вызывается по завершении стрима, чтобы
// освободить память и не показывать устаревший контент.
func (lt *LiveTail) Delete(id string) {
	if lt == nil || id == "" {
		return
	}
	lt.mu.Lock()
	delete(lt.m, id)
	lt.mu.Unlock()
}

// sanitizeTail заменяет управляющие символы (\n, \r, \t) на пробелы, чтобы хвост
// оставался однострочным в TUI.
func sanitizeTail(s string) string {
	out := []rune(s)
	for i, r := range out {
		if r == '\n' || r == '\r' || r == '\t' {
			out[i] = ' '
		}
	}
	return string(out)
}
