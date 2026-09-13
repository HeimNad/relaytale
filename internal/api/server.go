package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	project "relaytale"
	"time"
)

type Pinger interface{ PingContext(context.Context) error }

func Handler(db Pinger, storageCheck func() error, metrics ...http.Handler) http.Handler {
	mux := http.NewServeMux()
	if len(metrics) > 1 && metrics[1] != nil {
		mux.Handle("/admin/", metrics[1])
	}
	if len(metrics) > 0 && metrics[0] != nil {
		mux.Handle("GET /metrics", metrics[0])
	}
	mux.HandleFunc("GET /license", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, project.LicenseText)
	})
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, "alive") })
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			respond(w, 503, "not_ready")
			return
		}
		if err := storageCheck(); err != nil {
			respond(w, 503, "not_ready")
			return
		}
		respond(w, 200, "ready")
	})
	return mux
}

func respond(w http.ResponseWriter, code int, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": status})
}
