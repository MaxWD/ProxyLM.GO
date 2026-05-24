// Package translate обеспечивает кросс-протокольную трансляцию
// между OpenAI Chat Completions API и Anthropic Messages API.
package translate

import (
	"crypto/rand"
	"encoding/hex"
)

func randomID(prefix string, n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

const defaultMaxTokens = 4096
