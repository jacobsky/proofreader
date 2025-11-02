package health

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/a-h/templ"
)

type Handler struct{}

func NewHandler() *Handler {
	return &Handler{}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		contentType := r.Header.Get("Content-Type")

		if strings.Contains(contentType, "application/json") {
			err := writeJSON(w, http.StatusOK, map[string]string{"message": "Health OK"})
			if err != nil {
				slog.Error("Health Check", "error", err)
			}
		} else {
			templ.Handler(HealthOkay()).ServeHTTP(w, r)
		}
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// Simple wrapper to reduce boilerplate over writing json in the api endpoints
func writeJSON(w http.ResponseWriter, status int, content any) error {
	w.WriteHeader(status)
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(content)
}
