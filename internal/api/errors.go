package api

import (
	"encoding/json"
	"net/http"
)

// writeJSONError отдаёт ошибку в формате, близком к OpenAI:
//
//	{"error": {"message": "...", "type": "<http-status>"}}
func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    http.StatusText(status),
		},
	})
}
