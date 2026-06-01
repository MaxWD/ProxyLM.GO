package core

import (
	"strings"
	"testing"
)

func TestLiveTail_AppendAndGet(t *testing.T) {
	lt := NewLiveTail()
	lt.Append("req1", "Hello")
	lt.Append("req1", ", world")
	if got := lt.Get("req1"); got != "Hello, world" {
		t.Fatalf("Get = %q, want %q", got, "Hello, world")
	}
	if got := lt.Get("missing"); got != "" {
		t.Fatalf("Get(missing) = %q, want empty", got)
	}
}

func TestLiveTail_TrimsToMaxRunes(t *testing.T) {
	lt := NewLiveTail()
	// Заполняем больше лимита; хвост должен сохранить только последние liveTailMaxRunes.
	for range liveTailMaxRunes * 2 {
		lt.Append("r", "x")
	}
	got := lt.Get("r")
	if n := len([]rune(got)); n != liveTailMaxRunes {
		t.Fatalf("tail length = %d, want %d", n, liveTailMaxRunes)
	}
}

func TestLiveTail_SanitizesControlChars(t *testing.T) {
	lt := NewLiveTail()
	lt.Append("r", "line1\nline2\tend\r")
	got := lt.Get("r")
	if strings.ContainsAny(got, "\n\r\t") {
		t.Fatalf("tail contains control chars: %q", got)
	}
	if got != "line1 line2 end " {
		t.Fatalf("tail = %q, want %q", got, "line1 line2 end ")
	}
}

func TestLiveTail_Delete(t *testing.T) {
	lt := NewLiveTail()
	lt.Append("r", "data")
	lt.Delete("r")
	if got := lt.Get("r"); got != "" {
		t.Fatalf("after Delete Get = %q, want empty", got)
	}
}

func TestLiveTail_NilSafe(t *testing.T) {
	var lt *LiveTail // nil-приёмник: все операции — no-op, без паники
	lt.Append("r", "x")
	lt.Delete("r")
	if got := lt.Get("r"); got != "" {
		t.Fatalf("nil LiveTail Get = %q, want empty", got)
	}
}
