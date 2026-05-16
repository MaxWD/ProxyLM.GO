package api

import (
	"encoding/json"
	"net/http"

	"proxylm/internal/core"
)

// healthzHandler возвращает {"status":"ok", "servers":[{"name":...,"healthy":...}]}.
// Не требует аутентификации — простой liveness probe.
func healthzHandler(servers []*core.ServerInfo) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list := make([]map[string]any, 0, len(servers))
		for _, s := range servers {
			list = append(list, map[string]any{
				"name":          s.Name,
				"healthy":       s.Healthy.Load(),
				"current_model": s.CurrentModelString(),
			})
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"servers": list,
		})
	}
}
