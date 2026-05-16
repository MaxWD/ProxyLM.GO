package core

import (
	"testing"
	"time"
)

func TestRetryConfig_Backoff(t *testing.T) {
	cfg := RetryConfig{
		MaxAttempts:    5,
		InitialBackoff: 500 * time.Millisecond,
		MaxBackoff:     4 * time.Second,
	}
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 0},
		{2, 500 * time.Millisecond},
		{3, 1 * time.Second},
		{4, 2 * time.Second},
		{5, 4 * time.Second}, // 4s = MaxBackoff (cap)
		{6, 4 * time.Second}, // выше — всё равно cap
	}
	for _, tc := range tests {
		got := cfg.Backoff(tc.attempt)
		if got != tc.want {
			t.Errorf("Backoff(attempt=%d) = %v, want %v", tc.attempt, got, tc.want)
		}
	}
}

func TestRetryConfig_Backoff_NoInitial(t *testing.T) {
	cfg := RetryConfig{InitialBackoff: 0, MaxBackoff: time.Second}
	if got := cfg.Backoff(3); got != 0 {
		t.Errorf("Backoff с InitialBackoff=0 должен возвращать 0, got %v", got)
	}
}
