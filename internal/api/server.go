package api

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"proxylm/internal/config"
	"proxylm/internal/core"
	"proxylm/internal/core/backends"
	"proxylm/internal/storage"
	"proxylm/internal/webui"
)

// Server — HTTP-сервер прокси.
// Управляет жизненным циклом *http.Server и роутингом /v1/*, /admin/*, /healthz.
type Server struct {
	cfg      config.Proxy
	compat   config.Compat
	auth     *AuthRegistry
	sched    *core.Scheduler
	backends map[string]backends.Backend
	servers  []*core.ServerInfo
	history  *storage.History
	liveTail *core.LiveTail
	log      *slog.Logger
	srv      *http.Server

	adminStream http.HandlerFunc // /admin/stream, регистрируется через SetAdminStream
}

// SetAdminStream регистрирует WebSocket-обработчик для /admin/stream.
// Должно быть вызвано до ListenAndServe.
func (s *Server) SetAdminStream(h http.HandlerFunc) { s.adminStream = h }

// Deps — зависимости для конструктора, собранные в одном месте.
type Deps struct {
	Auth     *AuthRegistry
	Sched    *core.Scheduler
	Backends map[string]backends.Backend
	Servers  []*core.ServerInfo
	History  *storage.History
	Compat   config.Compat
	LiveTail *core.LiveTail
	Log      *slog.Logger
}

// New собирает Server. ListenAndServe запускает его и блокирует.
func New(cfg config.Proxy, d Deps) *Server {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		cfg:      cfg,
		compat:   d.Compat,
		auth:     d.Auth,
		sched:    d.Sched,
		backends: d.Backends,
		servers:  d.Servers,
		history:  d.History,
		liveTail: d.LiveTail,
		log:      log,
	}
}

// Router возвращает http.Handler сервера — полезно для интеграционных тестов
// (httptest.NewServer без поднятия реального listener'а).
func (s *Server) Router() http.Handler { return s.router() }

// router строит chi-router с middleware и маршрутами.
func (s *Server) router() http.Handler {
	r := chi.NewRouter()
	r.Use(s.recoverer)
	r.Use(s.accessLog)

	r.Get("/healthz", healthzHandler(s.servers))

	// /ui/* — встроенный Web UI. Без auth-middleware: статические файлы сами
	// по себе не раскрывают ничего чувствительного, а WS-соединение, которое
	// фронтенд открывает к /admin/stream, аутентифицируется отдельно (см.
	// internal/api/auth.go extractAdminKey и internal/ipc/server.go).
	r.Get("/ui", http.RedirectHandler("/ui/", http.StatusMovedPermanently).ServeHTTP)
	r.Handle("/ui/*", http.StripPrefix("/ui/", webui.Handler()))

	// /v1/* — клиентский API (OpenAI + Anthropic)
	r.Route("/v1", func(r chi.Router) {
		r.Use(s.auth.ClientAuth)
		r.Use(requestIDMiddleware) // X-Request-Id в response headers
		r.Get("/models", listModelsHandler(s.servers))
		for _, endpoint := range []string{"/chat/completions", "/completions", "/embeddings"} {
			h := newProxyHandler("/v1"+endpoint, s.sched, s.backends, s.history, s.compat, s.liveTail, s.log)
			r.Post(endpoint, h.ServeHTTP)
		}
		ah := newAnthropicHandler(s.sched, s.backends, s.history, s.log)
		r.Post("/messages", ah.ServeHTTP)
	})

	// /admin/* — IPC. AdminHandler регистрируется через SetAdminStream.
	r.Route("/admin", func(r chi.Router) {
		r.Use(s.auth.AdminAuth)
		if s.adminStream != nil {
			r.Get("/stream", s.adminStream)
		}
	})

	return r
}

// ListenAndServe запускает HTTP-сервер. Блокирует до Shutdown / ошибки.
// Возвращает http.ErrServerClosed при штатной остановке.
func (s *Server) ListenAndServe(ctx context.Context) error {
	addr := s.cfg.Host + ":" + strconv.Itoa(s.cfg.Port)
	s.srv = &http.Server{
		Addr:              addr,
		Handler:           s.router(),
		ReadHeaderTimeout: 30 * time.Second,
		// Read/Write timeouts оставляем 0 — иначе SSE-стримы порвутся.
		// Защита от медленных читателей — на уровне backend.Timeout и context.
	}
	s.log.Info("HTTP server listen", "addr", addr)
	errCh := make(chan error, 1)
	go func() { errCh <- s.srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutdownCtx)
		<-errCh
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

// requestIDMiddleware генерирует X-Request-Id перед хендлером и возвращает
// его клиенту в заголовке ответа. Хендлер может получить значение через
// RequestIDFromContext. Если клиент прислал X-Request-Id — используем его.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get("X-Request-Id")
		if reqID == "" {
			reqID = uuid.NewString()
		}
		w.Header().Set("X-Request-Id", reqID)
		ctx := context.WithValue(r.Context(), ctxRequestID, reqID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// recoverer перехватывает panic в handler'ах, логирует и отдаёт 500.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rv := recover(); rv != nil {
				s.log.Error("panic в HTTP handler",
					"path", r.URL.Path,
					"method", r.Method,
					"value", fmt.Sprint(rv))
				writeJSONError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// accessLog — минимальный access-лог через slog.
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(ww, r)
		s.log.Debug("http access",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.status,
			"duration_ms", time.Since(start).Milliseconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.wroteHeader = true
	}
	return r.ResponseWriter.Write(b)
}

// Flush проксирует http.Flusher если поддерживается (нужно для SSE-стриминга).
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack проксирует http.Hijacker — нужен WebSocket-серверу (coder/websocket)
// для переключения TCP-соединения в WS. Без этого /admin/stream отдаёт 501.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("underlying ResponseWriter does not implement http.Hijacker")
	}
	return h.Hijack()
}

// Unwrap нужен для http.ResponseController — позволяет stdlib искать поддерживаемые
// интерфейсы сквозь обёртки middleware.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
