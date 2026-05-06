package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

type handlers struct {
	logger  *slog.Logger
	metrics *Metrics
	version string
}

// middleware wraps every app route with request logging and metrics recording.
func (h *handlers) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rw, r)

		duration := time.Since(start).Seconds()
		status := strconv.Itoa(rw.status)
		path := r.URL.Path

		h.metrics.requests.WithLabelValues(r.Method, path, status).Inc()
		h.metrics.duration.WithLabelValues(r.Method, path).Observe(duration)

		h.logger.Info("request",
			"method", r.Method,
			"path", path,
			"status", rw.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote_addr", r.RemoteAddr,
		)
	})
}

// responseWriter captures the status code written by a handler.
type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

// health is the liveness probe — returns 200 as long as the process is running.
func (h *handlers) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ready is the readiness probe — returns 503 if the service should not receive traffic.
// Extend this to check downstream dependencies (DB, cache) before returning 200.
func (h *handlers) ready(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (h *handlers) ping(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"message": "pong",
		"version": h.version,
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
