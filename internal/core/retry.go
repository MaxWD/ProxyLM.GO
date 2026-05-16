package core

import "time"

// RetryConfig — параметры retry (соответствует config.Retry).
//
// Семантика попыток (новая, начиная с v1.0.0): MaxAttempts — это ОБЩЕЕ
// число попыток у Submit'а, независимо от распределения по серверам.
// Каждая следующая попытка идёт на сервер, исключая только тот, на котором
// упала предыдущая (rolling exclusion size 1); см. INV-5 в docs/SRS.md.
// Failover больше не настраивается — следующая попытка всегда стремится
// уйти на другой сервер, если такой есть.
type RetryConfig struct {
	MaxAttempts    int // общее число попыток у Submit, INV-5
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

// Backoff возвращает паузу между attempt-й попыткой (1-based) на одном сервере.
// Экспоненциальный рост 2^(attempt-1) с насыщением в MaxBackoff.
//
//	attempt=1 → 0 (первая попытка без задержки)
//	attempt=2 → initial
//	attempt=3 → initial*2
//	...        до max
func (c RetryConfig) Backoff(attempt int) time.Duration {
	if attempt <= 1 {
		return 0
	}
	d := c.InitialBackoff
	for i := 2; i < attempt; i++ {
		d *= 2
		if d >= c.MaxBackoff {
			return c.MaxBackoff
		}
	}
	if d > c.MaxBackoff {
		return c.MaxBackoff
	}
	return d
}
