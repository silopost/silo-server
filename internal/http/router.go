// Package http builds the HTTP router and middleware stack.
package http

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/silopost/silo-server/internal/blob"
	"github.com/silopost/silo-server/internal/http/api"
)

// Deps are the dependencies the router needs.
type Deps struct {
	Logger *slog.Logger
	// DB and Blobs are the shared stores; no handler uses them yet.
	DB    *pgxpool.Pool
	Blobs blob.BlobStore
	API   api.Options
}

// NewRouter returns the root handler with the common middleware stack.
func NewRouter(d Deps) http.Handler {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(requestLogger(logger))
	r.Use(middleware.Recoverer)

	r.Get("/healthz", healthz)
	api.Mount(r, d.API)
	return r
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// requestLogger logs one structured line per request.
func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			status := ww.Status()
			if status == 0 {
				status = http.StatusOK
			}
			logger.LogAttrs(r.Context(), slog.LevelInfo, "http request",
				slog.String("request_id", middleware.GetReqID(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", status),
				slog.Int("bytes", ww.BytesWritten()),
				slog.Duration("duration", time.Since(start)),
				slog.String("remote", r.RemoteAddr),
			)
		})
	}
}
