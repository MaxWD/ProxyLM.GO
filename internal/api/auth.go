package api

import (
	"context"
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

// ClientAuth — middleware для /v1/*. Требует Authorization: Bearer <key>.
// При успехе кладёт client_name в context.
func (r *AuthRegistry) ClientAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		key := extractBearer(req)
		if key == "" {
			writeJSONError(w, http.StatusUnauthorized, "missing Authorization Bearer")
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

// AdminAuth — middleware для /admin/*. Требует Bearer = admin_key.
func (r *AuthRegistry) AdminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		key := extractBearer(req)
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
