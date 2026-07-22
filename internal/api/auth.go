package api

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"

	"proxylm/internal/config"
)

// ctxKey — приватный тип для context.WithValue, чтобы избежать коллизий.
type ctxKey int

const (
	ctxClientName ctxKey = iota
	ctxRequestID
)

// AuthRegistry — таблица соответствия "api-key → client name" + admin_key.
type AuthRegistry struct {
	keys     map[string]string // key → client name
	adminKey string
}

// NewAuthRegistry строит реестр из config.Auth. Ключи проходят простое сравнение
// без констант-тайма — мы внутри корпоративного периметра и не пытаемся защититься
// от timing-атак (это явное упрощение MVP).
func NewAuthRegistry(a config.Auth) *AuthRegistry {
	m := make(map[string]string, len(a.APIKeys))
	for _, k := range a.APIKeys {
		m[k.Key] = k.Name
	}
	return &AuthRegistry{keys: m, adminKey: a.AdminKey}
}

// LookupClient возвращает имя клиента по ключу. Пустая строка → ключ не найден.
func (r *AuthRegistry) LookupClient(key string) string { return r.keys[key] }

// IsAdmin проверяет, совпадает ли ключ с admin_key.
func (r *AuthRegistry) IsAdmin(key string) bool { return key != "" && key == r.adminKey }

// ClientAuth — middleware для /v1/*. Принимает два стиля аутентификации:
//   - Authorization: Bearer <key> (OpenAI-стиль)
//   - x-api-key: <key> (Anthropic-стиль)
//
// При успехе кладёт client_name в context.
func (r *AuthRegistry) ClientAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		key := extractKey(req)
		if key == "" {
			writeJSONError(w, http.StatusUnauthorized, "missing api key")
			return
		}
		name := r.LookupClient(key)
		if name == "" {
			writeJSONError(w, http.StatusUnauthorized, "unknown api key")
			return
		}
		ctx := context.WithValue(req.Context(), ctxClientName, name)
		next.ServeHTTP(w, req.WithContext(ctx))
	})
}

// AdminAuth — middleware для /admin/*. Требует admin_key через Authorization:
// Bearer (TUI/curl-клиенты) либо через Sec-WebSocket-Protocol (браузерный Web
// UI — см. extractAdminKey).
func (r *AuthRegistry) AdminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		key := extractAdminKey(req)
		if !r.IsAdmin(key) {
			writeJSONError(w, http.StatusUnauthorized, "admin auth required")
			return
		}
		next.ServeHTTP(w, req)
	})
}

// ClientFromContext извлекает имя клиента, ранее положенное ClientAuth.
func ClientFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxClientName).(string); ok {
		return v
	}
	return ""
}

// RequestIDFromContext извлекает request_id из контекста.
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxRequestID).(string); ok {
		return v
	}
	return ""
}

// extractKey извлекает API-ключ из запроса, поддерживая оба стиля:
//   - Authorization: Bearer <key> (OpenAI)
//   - x-api-key: <key> (Anthropic)
func extractKey(req *http.Request) string {
	if bearer := extractBearer(req); bearer != "" {
		return bearer
	}
	if key := req.Header.Get("X-Api-Key"); key != "" {
		return key
	}
	return ""
}

func extractBearer(req *http.Request) string {
	h := req.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// wsTokenSubprotocolPrefix — префикс значения Sec-WebSocket-Protocol, в
// котором браузерный Web UI передаёт admin_key. Браузерный WebSocket API не
// позволяет установить произвольные заголовки запроса (в частности
// Authorization) при хендшейке — единственный доступный канал для передачи
// токена из JS — список предлагаемых подпротоколов.
const wsTokenSubprotocolPrefix = "proxylm-token."

// extractAdminKey извлекает admin_key из запроса: сначала Authorization:
// Bearer (обычный путь для TUI/curl — поведение не меняется), иначе — из
// Sec-WebSocket-Protocol (браузерный WS-клиент /admin/stream). Значение
// НИКОГДА не логируется.
func extractAdminKey(req *http.Request) string {
	if bearer := extractBearer(req); bearer != "" {
		return bearer
	}
	return extractAdminKeyFromSubprotocol(req)
}

// extractAdminKeyFromSubprotocol разбирает Sec-WebSocket-Protocol (может
// присутствовать несколько заголовков, каждый — список значений через
// запятую) и декодирует admin_key из элемента вида
// "proxylm-token.<base64url-без-padding>". Первое совпадение побеждает; при
// ошибке декодирования возвращает "" (AdminAuth ответит 401).
func extractAdminKeyFromSubprotocol(req *http.Request) string {
	for _, line := range req.Header.Values("Sec-WebSocket-Protocol") {
		for _, part := range strings.Split(line, ",") {
			part = strings.TrimSpace(part)
			encoded, ok := strings.CutPrefix(part, wsTokenSubprotocolPrefix)
			if !ok {
				continue
			}
			decoded, err := base64.RawURLEncoding.DecodeString(encoded)
			if err != nil {
				return ""
			}
			return string(decoded)
		}
	}
	return ""
}
