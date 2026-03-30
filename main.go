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

func run() int {
	var (
		listenAddr  = flag.String("listen-address", ":9312", "Address to listen on for metrics")
		metricsPath = flag.String("metrics-path", "/metrics", "Path under which to expose metrics")
		apiKey      = flag.String("api-key", "", "Hyperping API key (env: HYPERPING_API_KEY)")
		cacheTTL    = flag.Duration("cache-ttl", 60*time.Second, "How often to refresh data from the API")
		logLevel      = flag.String("log-level", "info", "Log level (debug, info, warn, error)")
		logFormat     = flag.String("log-format", "text", "Log format (text, json)")
		webConfigFile = flag.String("web.config.file", "", "Path to web config (TLS/basic-auth). See https://github.com/prometheus/exporter-toolkit/blob/master/docs/web-configuration.md")
	)
	flag.Parse()

	if *apiKey == "" {
		*apiKey = os.Getenv("HYPERPING_API_KEY")
	}
	if *apiKey == "" {
		fmt.Fprintln(os.Stderr, "error: API key required (use --api-key or HYPERPING_API_KEY)")
		return 1
	}

	logger := setupLogger(*logLevel, *logFormat)

	apiClient := client.NewClient(*apiKey, client.WithMaxRetries(2))

	c := collector.NewCollector(apiClient, *cacheTTL, logger)
	registry := prometheus.NewRegistry()
	registry.MustRegister(c)
	registry.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	registry.MustRegister(collectors.NewGoCollector())

	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "hyperping",
		Name:      "build_info",
		Help:      "A metric with constant value 1 labeled with build metadata.",
	}, []string{"version", "revision", "goversion"})
	buildInfo.WithLabelValues(version, revision, runtime.Version()).Set(1)
	registry.MustRegister(buildInfo)

	mux := http.NewServeMux()
	mux.Handle(*metricsPath, promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	}))
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
			{Address: *metricsPath, Text: "Metrics"},
			{Address: "/healthz", Text: "Health"},
			{Address: "/readyz", Text: "Readiness"},
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: create landing page: %v\n", err)
		return 1
	}
	mux.Handle("/", landingPage)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go c.Start(ctx)

	srv := &http.Server{
		Addr:              *listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
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
		"address", *listenAddr,
		"metrics_path", *metricsPath,
		"cache_ttl", *cacheTTL,
	)

	webFlags := &web.FlagConfig{
		WebListenAddresses: &[]string{*listenAddr},
		WebSystemdSocket:   func() *bool { b := false; return &b }(),
		WebConfigFile:      webConfigFile,
	}
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
