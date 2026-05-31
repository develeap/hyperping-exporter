// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/exporter-toolkit/web"

	hyperping "github.com/develeap/hyperping-go"
	"github.com/develeap/hyperping-exporter/internal/collector"
)

var version = "dev"
var revision = "unknown"

func main() {
	os.Exit(run())
}

type config struct {
	listenAddr         string
	metricsPath        string
	apiKey             string
	apiKeyFile         string
	cacheTTL           time.Duration
	cacheMode          string
	hotTTL             time.Duration
	warmTTL            time.Duration
	coldTTL            time.Duration
	logLevel           string
	logFormat          string
	webConfigFile      string
	namespace          string
	mcpURL             string
	excludeNamePattern string
	excludeNameRx      *regexp.Regexp
}

// parseConfig is the production entry point; it writes diagnostics to os.Stderr.
// Tests call parseConfigOut directly so they can capture the deprecation
// warning without racing on the real stderr.
func parseConfig() (config, bool) {
	return parseConfigOut(os.Stderr)
}

// parseConfigOut parses CLI flags + env vars and resolves the API key from one
// of three sources, in this priority order:
//  1. --api-key-file <path> (recommended: file is readable only by the
//     exporter user, never appears in /proc/<pid>/cmdline).
//  2. HYPERPING_API_KEY env var (recommended for container runtimes).
//  3. --api-key <key> (DEPRECATED: leaks via ps/proc; emits a stderr warning
//     and best-effort scrubs os.Args after parse).
//
// stderr is an io.Writer so tests can capture warnings deterministically.
func parseConfigOut(stderr io.Writer) (config, bool) {
	var cfg config
	flag.StringVar(&cfg.listenAddr, "listen-address", ":9312", "Address to listen on for metrics")
	flag.StringVar(&cfg.metricsPath, "metrics-path", "/metrics", "Path under which to expose metrics")
	flag.StringVar(&cfg.apiKey, "api-key", "",
		"DEPRECATED: Hyperping API key (visible via /proc/<pid>/cmdline). Prefer HYPERPING_API_KEY or --api-key-file.")
	flag.StringVar(&cfg.apiKeyFile, "api-key-file", "",
		"Path to a file containing the Hyperping API key (one trailing newline is stripped).")
	flag.DurationVar(&cfg.cacheTTL, "cache-ttl", 60*time.Second, "How often to refresh data from the API (legacy mode only)")
	flag.StringVar(&cfg.cacheMode, "cache-mode", "legacy", `Cache refresh strategy: "legacy" (single ticker, default) or "tiered" (three independent HOT/WARM/COLD tickers; see docs/tiered-cache-design.md)`)
	flag.DurationVar(&cfg.hotTTL, "hot-ttl", 60*time.Second, "Tiered mode HOT-tier refresh interval (only honored when --cache-mode=tiered)")
	flag.DurationVar(&cfg.warmTTL, "warm-ttl", 5*time.Minute, "Tiered mode WARM-tier refresh interval (only honored when --cache-mode=tiered)")
	flag.DurationVar(&cfg.coldTTL, "cold-ttl", 15*time.Minute, "Tiered mode COLD-tier refresh interval (only honored when --cache-mode=tiered)")
	flag.StringVar(&cfg.logLevel, "log-level", "info", "Log level (debug, info, warn, error)")
	flag.StringVar(&cfg.logFormat, "log-format", "text", "Log format (text, json)")
	flag.StringVar(&cfg.webConfigFile, "web.config.file", "", "Path to web config (TLS/basic-auth). See https://github.com/prometheus/exporter-toolkit/blob/master/docs/web-configuration.md")
	flag.StringVar(&cfg.namespace, "namespace", "", `Metric name prefix (env: HYPERPING_EXPORTER_NAMESPACE, default: "hyperping")`)
	flag.StringVar(&cfg.mcpURL, "mcp-url", "", "Custom Hyperping MCP server URL (default: https://api.hyperping.io/v1/mcp)")
	flag.StringVar(&cfg.excludeNamePattern, "exclude-name-pattern", "", "RE2 regex; monitors whose name matches are excluded from all metrics and tenant aggregates")
	flag.Parse()

	// Resolve the API key. Precedence: --api-key-file > HYPERPING_API_KEY > --api-key.
	// --api-key remains supported for one deprecation cycle to avoid breaking
	// existing deployments mid-upgrade; using it emits a stderr warning and
	// triggers best-effort os.Args scrubbing so /proc/<pid>/cmdline no longer
	// carries the secret. Process accounting or kernel logs that captured argv
	// before this scrub are still a leak, hence "best-effort".
	apiKeyFromFlag := cfg.apiKey
	if cfg.apiKeyFile != "" {
		data, err := os.ReadFile(cfg.apiKeyFile)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "error: read --api-key-file %q: %v\n", cfg.apiKeyFile, err)
			return cfg, false
		}
		// Strip any trailing CR/LF combo so Unix LF, Windows CRLF, and
		// classic-Mac CR endings all yield the same key. Multiple trailing
		// newlines (e.g. "key\n\n" from a here-doc) are also tolerated.
		// Internal and leading whitespace is preserved verbatim.
		cfg.apiKey = strings.TrimRight(string(data), "\r\n")
	} else if cfg.apiKey == "" {
		cfg.apiKey = os.Getenv("HYPERPING_API_KEY")
	}
	if cfg.apiKey == "" {
		_, _ = fmt.Fprintln(stderr, "error: API key required (use HYPERPING_API_KEY, --api-key-file, or --api-key)")
		return cfg, false
	}
	if apiKeyFromFlag != "" {
		_, _ = fmt.Fprintln(stderr, "warning: --api-key is DEPRECATED and exposes the secret via /proc/<pid>/cmdline; "+
			"use HYPERPING_API_KEY or --api-key-file instead. Scrubbing os.Args is best-effort; "+
			"process accounting or kernel logs may still have captured the original argv.")
		os.Args = sanitizeArgs(os.Args, apiKeyFromFlag)
	}
	if cfg.namespace == "" {
		cfg.namespace = os.Getenv("HYPERPING_EXPORTER_NAMESPACE")
	}
	if cfg.namespace == "" {
		cfg.namespace = "hyperping"
	}
	if err := validateNamespace(cfg.namespace); err != nil {
		_, _ = fmt.Fprintf(stderr, "error: invalid namespace: %v\n", err)
		return cfg, false
	}
	if cfg.mcpURL != "" {
		if !strings.HasPrefix(cfg.mcpURL, "https://") && !strings.HasPrefix(cfg.mcpURL, "http://localhost") {
			_, _ = fmt.Fprintf(stderr, "error: invalid mcp-url %q: must start with \"https://\" (or \"http://localhost\" for dev)\n", cfg.mcpURL)
			return cfg, false
		}
	}
	// Validate --cache-mode (case-insensitive) so an unrecognised value
	// fails fast at boot rather than silently falling back to one of the
	// two valid modes. Same shape as validateNamespace's negative path.
	switch strings.ToLower(cfg.cacheMode) {
	case "legacy", "tiered":
		cfg.cacheMode = strings.ToLower(cfg.cacheMode)
	default:
		_, _ = fmt.Fprintf(stderr, "error: invalid --cache-mode %q: must be \"legacy\" or \"tiered\"\n", cfg.cacheMode)
		return cfg, false
	}
	if cfg.excludeNamePattern != "" {
		rx, err := regexp.Compile(cfg.excludeNamePattern)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "error: invalid --exclude-name-pattern %q: %v\n", cfg.excludeNamePattern, err)
			return cfg, false
		}
		cfg.excludeNameRx = rx
	}
	return cfg, true
}

// sanitizeArgs overwrites the API-key value carried in args with an equal
// number of 'x' bytes. Both forms are handled:
//   - "--api-key=value"   → suffix after '=' is replaced
//   - "--api-key" "value" → the next arg is replaced
//
// Only args literally tied to --api-key are touched. An unrelated arg whose
// value happens to contain the secret bytes as a substring is left alone, so
// a listen address, log path, or similar value that incidentally shares
// bytes with the key is not mangled. Returns the same slice (mutated in
// place) for caller convenience. An empty secret is a no-op so callers can
// unconditionally invoke this without guarding.
//
// Limitation: this only scrubs the in-process copy of argv that Go exposes via
// os.Args. The kernel's copy in /proc/<pid>/cmdline is updated only when the
// process modifies its argv[] memory directly, which Go does not do. Callers
// should treat this as defense-in-depth, not a substitute for using
// --api-key-file or the env var.
func sanitizeArgs(args []string, secret string) []string {
	if secret == "" {
		return args
	}
	mask := strings.Repeat("x", len(secret))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--api-key":
			// The value lives in the next arg, if present.
			if i+1 < len(args) {
				if args[i+1] == secret {
					args[i+1] = mask
				}
				i++ // skip the value arg; it has been handled
			}
		case strings.HasPrefix(a, "--api-key="):
			suffix := a[len("--api-key="):]
			if suffix == secret {
				args[i] = "--api-key=" + mask
			}
		}
	}
	return args
}

var reNamespace = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// validateNamespace enforces Prometheus metric-name compatible characters and
// caps length at 64 so a typo cannot generate metric names that exceed the
// 63-character limit imposed by Kubernetes label values and many storage
// backends. Prometheus itself does not impose a length limit, so 64 is
// chosen for safety in downstream systems rather than for protocol reasons.
func validateNamespace(ns string) error {
	if ns == "" {
		return fmt.Errorf("must not be empty")
	}
	if len(ns) > 64 {
		return fmt.Errorf("must be 64 characters or fewer (got %d)", len(ns))
	}
	if !reNamespace.MatchString(ns) {
		return fmt.Errorf("%q must match [a-zA-Z_][a-zA-Z0-9_]*", ns)
	}
	return nil
}

// newBaseRegistry creates a Prometheus registry pre-loaded with the standard
// process/Go/build-info collectors. The caller is responsible for registering
// any additional collectors (e.g. the Hyperping collector, client metrics).
func newBaseRegistry(namespace string) *prometheus.Registry {
	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	registry.MustRegister(collectors.NewGoCollector())

	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "build_info",
		Help:      "A metric with constant value 1 labeled with build metadata.",
	}, []string{"version", "revision", "goversion"})
	buildInfo.WithLabelValues(version, revision, runtime.Version()).Set(1)
	registry.MustRegister(buildInfo)
	return registry
}

// maybeWarnUnauthenticatedBind emits a single warning at startup when the
// exporter binds to any-interface (":port", "0.0.0.0:port", "[::]:port")
// without a web-config file. The default bind is intentionally any-interface
// (operators rely on network policy / k8s NetworkPolicies / firewalls to
// restrict access), but a startup hint catches the case where someone
// forgot the reverse-proxy / basic-auth step on a host that is reachable
// from the public internet.
//
// The check is conservative: any specific IP, loopback, or the presence of
// --web.config.file keeps the warning silent. The word "unauthenticated"
// appears in the log line so operators can grep for it during incident
// response.
func maybeWarnUnauthenticatedBind(logger *slog.Logger, listenAddr, webConfigFile string) {
	if webConfigFile != "" {
		return
	}
	if !isAnyInterfaceBind(listenAddr) {
		return
	}
	logger.Warn("metrics endpoint is unauthenticated and bound to any interface; "+
		"restrict via network policy or set --web.config.file for basic-auth/TLS "+
		"(see https://github.com/prometheus/exporter-toolkit/blob/master/docs/web-configuration.md)",
		"listen_address", listenAddr,
	)
}

// isAnyInterfaceBind returns true when addr resolves to "all interfaces":
// the bare ":port" form, "0.0.0.0:port", and "[::]:port". A specific IP
// (loopback or otherwise) is treated as an intentional choice.
func isAnyInterfaceBind(addr string) bool {
	if strings.HasPrefix(addr, ":") {
		return true
	}
	if strings.HasPrefix(addr, "0.0.0.0:") {
		return true
	}
	if strings.HasPrefix(addr, "[::]:") {
		return true
	}
	return false
}

// newHTTPServer builds the exporter's http.Server with conservative timeouts
// and a header-size cap. The values are chosen for an unauthenticated metrics
// endpoint that may be exposed to opportunistic scanners:
//
//	ReadHeaderTimeout 10s   slow-loris guard during request line + headers
//	ReadTimeout       30s   request body bound (we do not read bodies, but the
//	                        promhttp handler may briefly read on POST attempts)
//	WriteTimeout      30s   response body bound for slow clients
//	IdleTimeout      120s   keep-alive idle-connection DoS guard; without this
//	                        a peer can pin file descriptors indefinitely
//	MaxHeaderBytes   1 MiB  header-bomb guard; default is 1 MiB in stdlib but
//	                        explicit so a future refactor cannot accidentally
//	                        bump it
func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
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
	registry := newBaseRegistry(cfg.namespace)
	clientMetrics := collector.NewClientMetrics(registry, cfg.namespace)
	mcpMetrics := collector.NewMCPMetrics(registry, cfg.namespace)
	apiClient := hyperping.NewClient(cfg.apiKey, hyperping.WithMaxRetries(2), hyperping.WithMetrics(clientMetrics))

	// Initialize MCP client for advanced metrics. The raw SDK transport is
	// wrapped in an ObservedTransport so handshake/recovery/rate-limit events
	// flow into hyperping_mcp_* counters; see issue #60 for context.
	mcpTransport, err := hyperping.NewMcpTransport(cfg.apiKey, cfg.mcpURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: initialize MCP transport: %v\n", err)
		return 1
	}
	observedTransport := collector.NewObservedTransport(mcpTransport, mcpMetrics)
	// Eagerly initialize the MCP session at startup so:
	//   1) MCP connectivity issues surface at boot (not mid-first-scrape).
	//   2) The handshake goes through ObservedTransport.Initialize so
	//      hyperping_mcp_initialize_total counts it. The SDK's lazy init
	//      inside CallTool calls its own *McpTransport.Initialize directly,
	//      bypassing the decorator — pre-initializing here is the only way
	//      to observe the handshake from outside the SDK. Transparent
	//      session-loss recoveries remain invisible (SDK-private).
	// On error, log and continue; the SDK will lazy-retry on first tool call.
	initCtx, initCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if _, initErr := observedTransport.Initialize(initCtx); initErr != nil {
		logger.Warn("eager MCP initialize failed; SDK will lazy-retry on first tool call", "error", initErr)
	}
	initCancel()
	mcpClient := hyperping.NewMCPClient(observedTransport)

	if cfg.excludeNameRx != nil {
		logger.Info("monitor exclusion filter active", "pattern", cfg.excludeNamePattern)
	}
	collectorOpts := []collector.CollectorOption{
		collector.WithExcludePattern(cfg.excludeNameRx),
		collector.WithMCPMetrics(mcpMetrics),
	}
	if cfg.cacheMode == "tiered" {
		collectorOpts = append(collectorOpts,
			collector.WithCacheMode(collector.CacheModeTiered),
			collector.WithTierTTLs(cfg.hotTTL, cfg.warmTTL, cfg.coldTTL),
		)
		logger.Info("cache mode: tiered",
			"hot_ttl", cfg.hotTTL,
			"warm_ttl", cfg.warmTTL,
			"cold_ttl", cfg.coldTTL,
		)
	}
	c := collector.NewCollector(
		apiClient, mcpClient, cfg.cacheTTL, logger, cfg.namespace,
		collectorOpts...,
	)
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
	srv := newHTTPServer(cfg.listenAddr, mux)
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
		"namespace", cfg.namespace,
	)
	maybeWarnUnauthenticatedBind(logger, cfg.listenAddr, cfg.webConfigFile)
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
