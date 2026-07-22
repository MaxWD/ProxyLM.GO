// Package webui встраивает статические файлы Web UI (placeholder-фронтенд,
// заменяется реальной сборкой в дальнейшем) в бинарник proxylm и отдаёт их
// через http.Handler.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var staticFS embed.FS

// Handler возвращает http.Handler, отдающий встроенные статические файлы
// Web UI (static/*). Предполагается монтирование под префиксом — например
// http.StripPrefix("/ui/", Handler()) — так что путь "/" внутри этого
// handler'а соответствует "/ui/" снаружи и отдаёт static/index.html.
//
// Каждый ответ получает Cache-Control: no-cache — бинарник (а вместе с ним и
// встроенный фронтенд) обновляется при пересборке daemon'а, и закэшированный
// в браузере index.html/app.js не должен переживать обновление до следующей
// перезагрузки страницы.
func Handler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// На практике недостижимо: "static" — константный путь, проверяемый
		// компилятором на этапе //go:embed. Явный error-ответ вместо panic —
		// на случай будущих изменений embed-директивы.
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "webui: embedded assets unavailable", http.StatusInternalServerError)
		})
	}

	fileServer := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fileServer.ServeHTTP(&cacheControlWriter{ResponseWriter: w}, r)
	})
}

// cacheControlWriter добавляет "Cache-Control: no-cache" непосредственно
// перед отправкой заголовков ответа.
//
// Это не может быть сделано простым w.Header().Set(...) до вызова
// fileServer.ServeHTTP: начиная с Go 1.23 net/http.serveError снимает
// Cache-Control (вместе с Content-Encoding/Etag/Last-Modified) со всех
// error-ответов file-сервера (404 и т.п.), чтобы заголовки, предназначенные
// для успешного ответа, не утекали в error-страницу — см.
// https://pkg.go.dev/net/http#ServeContent. Поэтому заголовок выставляется
// в момент WriteHeader, когда serveError/ServeContent уже закончили любые
// свои Del и готовы фактически отправить ответ.
type cacheControlWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *cacheControlWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		w.ResponseWriter.Header().Set("Cache-Control", "no-cache")
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *cacheControlWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}
