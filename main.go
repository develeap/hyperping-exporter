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
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/exporter-toolkit/web"
	"gopkg.in/yaml.v3"

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
	projectsFile       string
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

	// projects is the resolved list of per-project configurations. The
	// legacy single-key path (--api-key / --api-key-file / HYPERPING_API_KEY
	// without --projects-file) synthesises a single entry with ID="default"
	// so downstream code can iterate a non-empty list unconditionally.
	projects []projectConfig
}

// projectConfig is one entry in cfg.projects. ID is the Prometheus
// `project` constLabel value emitted on every series sourced from this
// project's Collector. MCPURL and ExcludeNamePattern are per-project
// overrides; when empty, the globals (--mcp-url / --exclude-name-pattern)
// apply.
type projectConfig struct {
	ID                 string `yaml:"id"`
	APIKey             string `yaml:"apiKey"`
	APIKeyFile         string `yaml:"apiKeyFile"`
	MCPURL             string `yaml:"mcpUrl"`
	ExcludeNamePattern string `yaml:"excludeNamePattern"`
}

// reProjectID is the alphabet for project ids. It matches the tenant
// regex used in the collector so the `project` constLabel cannot inject
// characters that downstream label matchers (recording-rules, alert
// selectors) cannot escape.
var reProjectID = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)

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
	flag.StringVar(&cfg.projectsFile, "projects-file", "", "Path to a YAML list of {id, apiKey|apiKeyFile, mcpUrl?, excludeNamePattern?} entries. Mutually exclusive with --api-key/--api-key-file/HYPERPING_API_KEY. Env: HYPERPING_PROJECTS_FILE.")
	flag.Parse()

	// HYPERPING_PROJECTS_FILE env fallback (mirrors --api-key/HYPERPING_API_KEY pattern).
	if cfg.projectsFile == "" {
		cfg.projectsFile = os.Getenv("HYPERPING_PROJECTS_FILE")
	}

	// Mutual exclusion: --api-key / --api-key-file / HYPERPING_API_KEY
	// cannot combine with --projects-file. Letting both through would
	// silently shadow one source; explicit refusal forces operators to
	// pick one mode.
	envAPIKey := os.Getenv("HYPERPING_API_KEY")
	if cfg.projectsFile != "" && (cfg.apiKey != "" || cfg.apiKeyFile != "" || envAPIKey != "") {
		_, _ = fmt.Fprintln(stderr, "error: --projects-file is mutually exclusive with --api-key / --api-key-file / HYPERPING_API_KEY; pick one configuration source")
		return cfg, false
	}

	// Resolve the API key. Precedence: --api-key-file > HYPERPING_API_KEY > --api-key.
	// --api-key remains supported for one deprecation cycle to avoid breaking
	// existing deployments mid-upgrade; using it emits a stderr warning and
	// triggers best-effort os.Args scrubbing so /proc/<pid>/cmdline no longer
	// carries the secret. Process accounting or kernel logs that captured argv
	// before this scrub are still a leak, hence "best-effort".
	apiKeyFromFlag := cfg.apiKey
	if cfg.projectsFile != "" {
		// Skip the single-key resolution path entirely.
	} else if cfg.apiKeyFile != "" {
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
	if cfg.projectsFile == "" && cfg.apiKey == "" {
		_, _ = fmt.Fprintln(stderr, "error: API key required (use HYPERPING_API_KEY, --api-key-file, --api-key, or --projects-file)")
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

	// Resolve cfg.projects. Two shapes:
	//   1. --projects-file=<path>: parse the YAML list, validate each entry,
	//      apply global fallbacks for empty MCPURL / ExcludeNamePattern.
	//   2. Legacy single-key path: synthesise one project with ID="default"
	//      so downstream code always iterates a non-empty list.
	if cfg.projectsFile != "" {
		projects, err := loadProjectsFile(cfg.projectsFile, cfg.mcpURL, cfg.excludeNamePattern)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
			return cfg, false
		}
		cfg.projects = projects
	} else {
		cfg.projects = []projectConfig{{
			ID:                 "default",
			APIKey:             cfg.apiKey,
			MCPURL:             cfg.mcpURL,
			ExcludeNamePattern: cfg.excludeNamePattern,
		}}
	}
	return cfg, true
}

// loadProjectsFile reads, parses, and validates the YAML projects file
// at path. globalMCPURL and globalExcludeNamePattern are applied as
// fallbacks for any project entry whose own value is empty. The returned
// slice is guaranteed to have unique, regex-clean IDs and exactly one
// API key source (inline APIKey OR APIKeyFile) per project.
func loadProjectsFile(path, globalMCPURL, globalExcludeNamePattern string) ([]projectConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read --projects-file %q: %w", path, err)
	}
	var projects []projectConfig
	if err := yaml.Unmarshal(data, &projects); err != nil {
		return nil, fmt.Errorf("parse --projects-file %q: %w", path, err)
	}
	if len(projects) == 0 {
		return nil, fmt.Errorf("--projects-file %q: must contain at least one project entry", path)
	}
	seen := make(map[string]struct{}, len(projects))
	for i := range projects {
		p := &projects[i]
		p.ID = strings.TrimSpace(p.ID)
		if !reProjectID.MatchString(p.ID) {
			return nil, fmt.Errorf("--projects-file: invalid project id %q at index %d (must match [a-zA-Z0-9._-]{1,64})", p.ID, i)
		}
		if _, dup := seen[p.ID]; dup {
			return nil, fmt.Errorf("--projects-file: duplicate project id %q at index %d", p.ID, i)
		}
		seen[p.ID] = struct{}{}

		if p.APIKey != "" && p.APIKeyFile != "" {
			return nil, fmt.Errorf("--projects-file: project %q has both apiKey and apiKeyFile; pick one", p.ID)
		}
		if p.APIKey == "" && p.APIKeyFile == "" {
			return nil, fmt.Errorf("--projects-file: project %q must set apiKey or apiKeyFile", p.ID)
		}
		if p.APIKeyFile != "" {
			body, err := os.ReadFile(p.APIKeyFile)
			if err != nil {
				return nil, fmt.Errorf("--projects-file: project %q apiKeyFile %q: %w", p.ID, p.APIKeyFile, err)
			}
			p.APIKey = strings.TrimRight(string(body), "\r\n")
			if p.APIKey == "" {
				return nil, fmt.Errorf("--projects-file: project %q apiKeyFile %q is empty", p.ID, p.APIKeyFile)
			}
		}
		// Fall back to globals when the per-project override is empty.
		if p.MCPURL == "" {
			p.MCPURL = globalMCPURL
		}
		if p.ExcludeNamePattern == "" {
			p.ExcludeNamePattern = globalExcludeNamePattern
		}
		// Apply the same scheme validation the global --mcp-url path
		// uses (parseConfigOut). A typo like "htttps://..." in the
		// projects file would otherwise pass parseConfig unchecked and
		// either surface as an opaque MCP transport error or silently
		// route the project's API key to an attacker-controlled http://
		// endpoint over plaintext. Reject anything that is not https://
		// or http://localhost so the per-project path cannot bypass the
		// defence already in place for the global flag.
		if p.MCPURL != "" {
			if !strings.HasPrefix(p.MCPURL, "https://") && !strings.HasPrefix(p.MCPURL, "http://localhost") {
				return nil, fmt.Errorf("--projects-file: project %q invalid mcpUrl %q: must start with \"https://\" (or \"http://localhost\" for dev)", p.ID, p.MCPURL)
			}
		}
	}
	return projects, nil
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

func newMux(metricsPath string, registry *prometheus.Registry, collectors []*collector.Collector) (http.Handler, error) {
	mux := http.NewServeMux()
	mux.Handle(metricsPath, promhttp.HandlerFor(registry, promhttp.HandlerOpts{EnableOpenMetrics: true}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})
	// Readiness is the AND over every Collector. A single project that has
	// not yet completed its first successful refresh keeps /readyz red so
	// orchestrators do not flip the pod to Ready until every project has
	// fresh data. With a single project (legacy path) this is identical to
	// the pre-multi-project behaviour.
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		ready := true
		for _, c := range collectors {
			if !c.IsReady() {
				ready = false
				break
			}
		}
		if ready {
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

	if cfg.excludeNameRx != nil {
		logger.Info("monitor exclusion filter active", "pattern", cfg.excludeNamePattern)
	}
	if cfg.cacheMode == "tiered" {
		logger.Info("cache mode: tiered",
			"hot_ttl", cfg.hotTTL,
			"warm_ttl", cfg.warmTTL,
			"cold_ttl", cfg.coldTTL,
		)
	}

	collectors, err := buildCollectors(cfg, registry, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: build collectors: %v\n", err)
		return 1
	}
	for _, c := range collectors {
		registry.MustRegister(c)
	}
	mux, err := newMux(cfg.metricsPath, registry, collectors)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: create landing page: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// One Start per Collector so each project's tieredRefresher (or
	// legacy ticker) runs independently. A rate-limit pause on one
	// project does not stall the others.
	for _, c := range collectors {
		go c.Start(ctx)
	}
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

// buildCollectors constructs one *collector.Collector per cfg.projects
// entry, registering per-project client/MCP metrics into the supplied
// registry. Each Collector carries its project ID as a constLabel so
// series from different projects coexist without Desc collision.
//
// Per-project state:
//   - hyperping.NewClient with the project's APIKey, wired to its own
//     NewClientMetrics(reg, ns, project) so hyperping_client_* histograms
//     are project-scoped.
//   - NewMCPMetrics(reg, ns, project) so hyperping_mcp_* counters are
//     project-scoped.
//   - hyperping.NewMcpTransport with the project's APIKey and resolved
//     MCPURL (per-project override, falling back to the global flag).
//   - collector.NewCollector with WithProject(project), the project's
//     compiled excludePattern, and shared cache-mode / tier-TTL options.
//
// Errors short-circuit (no partial registration); the caller wraps the
// returned error for stderr output. The MCP eager-initialize step is
// best-effort per project: a failure is logged but does not block the
// other projects.
func buildCollectors(cfg config, registry *prometheus.Registry, logger *slog.Logger) ([]*collector.Collector, error) {
	if len(cfg.projects) == 0 {
		return nil, fmt.Errorf("buildCollectors called with empty cfg.projects (parseConfig should have synthesised a default project)")
	}
	type pending struct {
		project   projectConfig
		transport *collector.ObservedTransport
		excludeRx *regexp.Regexp
		client    *hyperping.Client
		mcpMx     *collector.MCPMetrics
	}
	out := make([]*collector.Collector, 0, len(cfg.projects))
	queue := make([]pending, 0, len(cfg.projects))
	for _, p := range cfg.projects {
		clientMetrics := collector.NewClientMetrics(registry, cfg.namespace, p.ID)
		mcpMetrics := collector.NewMCPMetrics(registry, cfg.namespace, p.ID)
		apiClient := hyperping.NewClient(p.APIKey, hyperping.WithMaxRetries(2), hyperping.WithMetrics(clientMetrics))

		mcpTransport, err := hyperping.NewMcpTransport(p.APIKey, p.MCPURL)
		if err != nil {
			return nil, fmt.Errorf("project %q: initialize MCP transport: %w", p.ID, err)
		}
		observedTransport := collector.NewObservedTransport(mcpTransport, mcpMetrics)

		// Per-project exclude pattern. Empty pattern means "no filter".
		var excludeRx *regexp.Regexp
		if p.ExcludeNamePattern != "" {
			rx, err := regexp.Compile(p.ExcludeNamePattern)
			if err != nil {
				return nil, fmt.Errorf("project %q: invalid excludeNamePattern %q: %w", p.ID, p.ExcludeNamePattern, err)
			}
			excludeRx = rx
		}
		queue = append(queue, pending{
			project:   p,
			transport: observedTransport,
			excludeRx: excludeRx,
			client:    apiClient,
			mcpMx:     mcpMetrics,
		})
	}

	// Best-effort eager MCP init, fanned out so total wall-time is
	// bounded by the slowest project's 10s budget rather than the sum.
	// Sequential init at 10s per project would otherwise blow past the
	// default Kubernetes liveness probe budget (~40s) for N>=4 hung
	// projects and trap the Pod in CrashLoopBackOff before the HTTP
	// server is even up. A failure on any single project is logged
	// and does not block the others (the SDK lazy-retries on first
	// tool call).
	var wg sync.WaitGroup
	for i := range queue {
		wg.Add(1)
		go func(item *pending) {
			defer wg.Done()
			initCtx, initCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer initCancel()
			if _, initErr := item.transport.Initialize(initCtx); initErr != nil {
				logger.Warn("eager MCP initialize failed; SDK will lazy-retry on first tool call",
					"project", item.project.ID, "error", initErr)
			}
		}(&queue[i])
	}
	wg.Wait()

	for _, q := range queue {
		mcpClient := hyperping.NewMCPClient(q.transport)
		opts := []collector.CollectorOption{
			collector.WithProject(q.project.ID),
			collector.WithExcludePattern(q.excludeRx),
			collector.WithMCPMetrics(q.mcpMx),
		}
		if cfg.cacheMode == "tiered" {
			opts = append(opts,
				collector.WithCacheMode(collector.CacheModeTiered),
				collector.WithTierTTLs(cfg.hotTTL, cfg.warmTTL, cfg.coldTTL),
			)
		}
		c := collector.NewCollector(
			q.client, mcpClient, cfg.cacheTTL, logger, cfg.namespace,
			opts...,
		)
		out = append(out, c)
	}
	return out, nil
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
