// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/exporter-toolkit/web"

	"github.com/develeap/hyperping-exporter/internal/client"
	"github.com/develeap/hyperping-exporter/internal/collector"
)

var version = "dev"
var revision = "unknown"

func main() {
	os.Exit(run())
}

type config struct {
	listenAddr    string
	metricsPath   string
	apiKey        string
	cacheTTL      time.Duration
	logLevel      string
	logFormat     string
	webConfigFile string
}

func parseConfig() (config, bool) {
	var cfg config
	flag.StringVar(&cfg.listenAddr, "listen-address", ":9312", "Address to listen on for metrics")
	flag.StringVar(&cfg.metricsPath, "metrics-path", "/metrics", "Path under which to expose metrics")
	flag.StringVar(&cfg.apiKey, "api-key", "", "Hyperping API key (env: HYPERPING_API_KEY)")
	flag.DurationVar(&cfg.cacheTTL, "cache-ttl", 60*time.Second, "How often to refresh data from the API")
	flag.StringVar(&cfg.logLevel, "log-level", "info", "Log level (debug, info, warn, error)")
	flag.StringVar(&cfg.logFormat, "log-format", "text", "Log format (text, json)")
	flag.StringVar(&cfg.webConfigFile, "web.config.file", "", "Path to web config (TLS/basic-auth). See https://github.com/prometheus/exporter-toolkit/blob/master/docs/web-configuration.md")
	flag.Parse()

	if cfg.apiKey == "" {
		cfg.apiKey = os.Getenv("HYPERPING_API_KEY")
	}
	if cfg.apiKey == "" {
		fmt.Fprintln(os.Stderr, "error: API key required (use --api-key or HYPERPING_API_KEY)")
		return cfg, false
	}
	return cfg, true
}

// newBaseRegistry creates a Prometheus registry pre-loaded with the standard
// process/Go/build-info collectors. The caller is responsible for registering
// any additional collectors (e.g. the Hyperping collector, client metrics).
func newBaseRegistry() *prometheus.Registry {
	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	registry.MustRegister(collectors.NewGoCollector())

	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "hyperping",
		Name:      "build_info",
		Help:      "A metric with constant value 1 labeled with build metadata.",
	}, []string{"version", "revision", "goversion"})
	buildInfo.WithLabelValues(version, revision, runtime.Version()).Set(1)
	registry.MustRegister(buildInfo)
	return registry
}

func newMux(metricsPath string, registry *prometheus.Registry, c *collector.Collector) (http.Handler, error) {
	mux := http.NewServeMux()
	mux.Handle(metricsPath, promhttp.HandlerFor(registry, promhttp.HandlerOpts{EnableOpenMetrics: true}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if c.IsReady() {
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintln(w, "ready")
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintln(w, "not ready")
		}
	})
	landingPage, err := web.NewLandingPage(web.LandingConfig{
		Name:        "Hyperping Exporter",
		Description: "Prometheus exporter for Hyperping monitoring service.",
		Version:     version,
		Links: []web.LandingLinks{
			{Address: metricsPath, Text: "Metrics"},
			{Address: "/healthz", Text: "Health"},
			{Address: "/readyz", Text: "Readiness"},
		},
	})
	if err != nil {
		return nil, err
	}
	mux.Handle("/", landingPage)
	return mux, nil
}

func run() int {
	cfg, ok := parseConfig()
	if !ok {
		return 1
	}

	logger := setupLogger(cfg.logLevel, cfg.logFormat)
	registry := newBaseRegistry()
	clientMetrics := collector.NewClientMetrics(registry)
	apiClient := client.NewClient(cfg.apiKey, client.WithMaxRetries(2), client.WithMetrics(clientMetrics))
	c := collector.NewCollector(apiClient, cfg.cacheTTL, logger)
	registry.MustRegister(c)
	mux, err := newMux(cfg.metricsPath, registry, c)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: create landing page: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go c.Start(ctx)
	noSocket := false
	srv := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	webFlags := &web.FlagConfig{
		WebListenAddresses: &[]string{cfg.listenAddr},
		WebSystemdSocket:   &noSocket,
		WebConfigFile:      &cfg.webConfigFile,
	}

	go func() {
		<-ctx.Done()
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("shutdown error", "error", err)
		}
	}()

	logger.Info("starting hyperping exporter",
		"version", version,
		"address", cfg.listenAddr,
		"metrics_path", cfg.metricsPath,
		"cache_ttl", cfg.cacheTTL,
	)
	if err := web.ListenAndServe(srv, webFlags, logger); err != nil && err != http.ErrServerClosed {
		logger.Error("server error", "error", err)
		return 1
	}
	return 0
}

func setupLogger(level, format string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: lvl}
	var handler slog.Handler
	if format == "json" {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}
	return slog.New(handler)
}
