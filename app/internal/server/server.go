package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Config struct {
	Port        string
	MetricsPort string
	Version     string
}

func ConfigFromEnv() Config {
	return Config{
		Port:        getEnv("PORT", "8080"),
		MetricsPort: getEnv("METRICS_PORT", "9090"),
		Version:     getEnv("APP_VERSION", "dev"),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Metrics holds all Prometheus instruments for the service.
type Metrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	registry *prometheus.Registry
}

func newMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total number of HTTP requests by method, path, and status code.",
	}, []string{"method", "path", "status"})

	duration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request latency by method and path.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
	}, []string{"method", "path"})

	reg.MustRegister(requests, duration)
	return &Metrics{requests: requests, duration: duration, registry: reg}
}

// Server manages the app HTTP server and the metrics HTTP server.
type Server struct {
	cfg        Config
	logger     *slog.Logger
	appSrv     *http.Server
	metricsSrv *http.Server
}

func New(cfg Config, logger *slog.Logger) *Server {
	m := newMetrics()
	h := &handlers{logger: logger, metrics: m, version: cfg.Version}

	appMux := http.NewServeMux()
	appMux.HandleFunc("GET /health", h.health)
	appMux.HandleFunc("GET /ready", h.ready)
	appMux.HandleFunc("GET /ping", h.ping)

	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	}))

	return &Server{
		cfg:    cfg,
		logger: logger,
		appSrv: &http.Server{
			Addr:         ":" + cfg.Port,
			Handler:      h.middleware(appMux),
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 30 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
		metricsSrv: &http.Server{
			Addr:        ":" + cfg.MetricsPort,
			Handler:     metricsMux,
			ReadTimeout: 5 * time.Second,
		},
	}
}

// Start launches both servers. It blocks until the app server exits.
func (s *Server) Start() error {
	go func() {
		s.logger.Info("metrics server listening", "port", s.cfg.MetricsPort)
		if err := s.metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("metrics server error", "err", err)
		}
	}()

	s.logger.Info("app server listening", "port", s.cfg.Port, "version", s.cfg.Version)
	if err := s.appSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("app server: %w", err)
	}
	return nil
}

// Shutdown gracefully drains both servers within the given context deadline.
func (s *Server) Shutdown(ctx context.Context) error {
	var appErr, metricsErr error
	appErr = s.appSrv.Shutdown(ctx)
	metricsErr = s.metricsSrv.Shutdown(ctx)
	return errors.Join(appErr, metricsErr)
}
