// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package collector

import (
	"context"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	hyperping "github.com/develeap/hyperping-go"
)

// CacheMode selects how the Collector refreshes its cache.
//
//   - CacheModeLegacy (default) drives a single cacheTTL ticker that
//     refreshes every endpoint together. Behaviour is unchanged from
//     pre-1.6 binaries.
//   - CacheModeTiered drives three independent HOT/WARM/COLD tickers,
//     each owning its own subset of endpoints and atomic snapshot
//     pointer. See docs/tiered-cache-design.md.
//
// The mode is selected via WithCacheMode and exposed to operators via
// --cache-mode in main.go. Helm renders it through config.cacheMode.
type CacheMode int

const (
	// CacheModeLegacy uses a single cacheTTL ticker (default).
	CacheModeLegacy CacheMode = iota
	// CacheModeTiered uses three independent per-tier tickers.
	CacheModeTiered
)

// CollectorOption configures a Collector after construction.
type CollectorOption func(*Collector)

// WithCacheMode selects the cache refresh strategy.
func WithCacheMode(m CacheMode) CollectorOption {
	return func(c *Collector) {
		c.cacheMode = m
	}
}

// WithTierTTLs configures the HOT/WARM/COLD refresh intervals. Only honored
// when CacheModeTiered is also set. Floors should already be enforced by
// the Helm chart's validateTierTTLs (hot >= 30s, warm >= 60s, cold >= 300s);
// the floors are intentionally NOT re-enforced here because legitimate test
// callers need millisecond-scale TTLs.
func WithTierTTLs(hot, warm, cold time.Duration) CollectorOption {
	return func(c *Collector) {
		c.hotTTL = hot
		c.warmTTL = warm
		c.coldTTL = cold
	}
}

// WithTierEnable configures which of the HOT/WARM/COLD tiers run for this
// Collector. Only honored when CacheModeTiered is also set; a disabled
// tier launches no goroutine, issues zero API calls, and emits no series
// for that tier on this project. The default (option unset) is all-enabled
// so existing call sites that don't know about the option behave exactly
// as before.
//
// HOT must remain enabled in normal operation: a Collector with HOT
// disabled would produce no scrape and trap /readyz at "not ready"
// forever. The hot parameter is accepted for symmetry and is enforced
// at parse time by the operator-facing config layer (main.parseConfig).
func WithTierEnable(hot, warm, cold bool) CollectorOption {
	return func(c *Collector) {
		c.hotEnabled = hot
		c.warmEnabled = warm
		c.coldEnabled = cold
		c.tierEnableSet = true
	}
}

// WithExcludePattern sets a compiled RE2 regex; any monitor whose Name matches
// is dropped from the monitor list immediately after the API fetch, before any
// metric computation or tenant aggregate calculation.
func WithExcludePattern(rx *regexp.Regexp) CollectorOption {
	return func(c *Collector) {
		c.excludePattern = rx
	}
}

// WithMCPMetrics enables MCP observability counters (initialize, session
// refresh, rate-limit, partial refresh). The same MCPMetrics value should be
// supplied to NewObservedTransport so transport-layer and refresh-layer
// counters share one registration.
func WithMCPMetrics(m *MCPMetrics) CollectorOption {
	return func(c *Collector) {
		c.mcpMetrics = m
	}
}

// WithProject sets the value of the `project` constLabel emitted on every
// metric Desc this Collector produces. Empty string collapses to "default"
// so legacy single-project deployments still see a stable label set on
// every series.
func WithProject(id string) CollectorOption {
	return func(c *Collector) {
		c.project = id
	}
}

// WithPeriods sets the list of SLA report windows this collector fans out
// over for the period-bearing metric set. main.resolvePeriods is the
// canonical validator + defaulter; this option performs no second
// validation pass so an unvalidated slice from a test harness can produce
// an unknown-period label. Callers that don't supply WithPeriods inherit
// defaultReportPeriods (["24h","7d","30d"]) which preserves the legacy
// fetch/emit behaviour byte-for-byte.
func WithPeriods(periods []string) CollectorOption {
	return func(c *Collector) {
		// Defensive copy so a caller mutating their slice after
		// constructing the Collector does not silently change emission.
		c.periods = make([]string, len(periods))
		copy(c.periods, periods)
	}
}

// defaultProjectID is the constLabel value used when WithProject is unset
// or empty. Kept explicit so downstream dashboards/alerts can always
// select on `project=<value>` without special-casing the unset path.
const defaultProjectID = "default"

// resolveProjectID returns id with the unset/empty path collapsed to
// "default". Used by NewCollector and the per-Desc construction helpers.
func resolveProjectID(id string) string {
	if id == "" {
		return defaultProjectID
	}
	return id
}

// defaultReportPeriods is the windows the legacy fetchReports path uses
// when no per-collector override is set. Kept as a slice (not the
// per-collector field) so test callers that construct a Collector
// without WithPeriods get the same behaviour they got before the
// multi-period feature.
var defaultReportPeriods = []string{"24h", "7d", "30d"}

// reportDurations maps period labels to their lookback durations. The
// table covers every token in main.allowedPeriods even when a particular
// build of the exporter never asks for 90d/365d, because the cold-tier
// refresher resolves durations dynamically from the per-collector periods
// slice.
var reportDurations = map[string]time.Duration{
	"24h":  24 * time.Hour,
	"7d":   7 * 24 * time.Hour,
	"30d":  30 * 24 * time.Hour,
	"90d":  90 * 24 * time.Hour,
	"365d": 365 * 24 * time.Hour,
}

// mttaValueFor returns the MTTA value for (uuid, period) from a snapshot,
// preferring the explicit per-period map (snap.mttaByPeriod) and falling
// back to the legacy single-window snap.mttaIndex only when the period is
// "24h". This preserves the warm-tier MCP fetch source as the source of
// truth for the 24h slice while letting cold-tier fan-out populate the
// other windows independently.
func mttaValueFor(snap collectorSnapshot, uuid, period string) (float64, bool) {
	if snap.mttaByPeriod != nil {
		if m, ok := snap.mttaByPeriod[period]; ok {
			if v, ok := m[uuid]; ok {
				return v, true
			}
		}
	}
	if period == "24h" {
		if v, ok := snap.mttaIndex[uuid]; ok {
			return v, true
		}
	}
	return 0, false
}

// PeriodTier maps a period token to the cache tier it lives on. Duplicated
// here (one-line table) rather than imported from main so the collector
// package stays free of the main->collector compile dependency. The
// canonical mapping is defined by main.periodToTier; any future tier
// expansion must update both tables together.
func PeriodTier(period string) string {
	switch period {
	case "24h":
		return "warm"
	case "7d", "30d", "90d", "365d":
		return "cold"
	default:
		return ""
	}
}

// HyperpingAPI defines the Hyperping API methods used by the collector.
//
// ListOutages takes variadic OutageListOption values so the tiered cache
// HOT tier can pass hyperping.WithStatus("ongoing") to keep the per-tick
// payload small. The legacy Refresh() loop passes no options, preserving
// the pre-tiered-cache semantics of fetching the full outage list.
type HyperpingAPI interface {
	ListMonitors(ctx context.Context) ([]hyperping.Monitor, error)
	ListHealthchecks(ctx context.Context) ([]hyperping.Healthcheck, error)
	ListOutages(ctx context.Context, opts ...hyperping.OutageListOption) ([]hyperping.Outage, error)
	ListMonitorReports(ctx context.Context, from, to string) ([]hyperping.MonitorReport, error)
	ListMaintenance(ctx context.Context) ([]hyperping.Maintenance, error)
	ListIncidents(ctx context.Context) ([]hyperping.Incident, error)
}

// collectorDescs holds all Prometheus metric descriptor fields.
type collectorDescs struct {
	monitorUp                 *prometheus.Desc
	monitorPaused             *prometheus.Desc
	monitorSSLExpDays         *prometheus.Desc
	monitorCheckInterval      *prometheus.Desc
	monitorInfo               *prometheus.Desc
	healthcheckUp             *prometheus.Desc
	healthcheckPaused         *prometheus.Desc
	healthcheckPeriod         *prometheus.Desc
	monitorsTotal             *prometheus.Desc
	healthchecksTotal         *prometheus.Desc
	scrapeDurationDesc        *prometheus.Desc
	scrapeSuccessDesc         *prometheus.Desc
	dataAgeDesc               *prometheus.Desc
	monitorOutageActive       *prometheus.Desc
	monitorActiveOutageStatus *prometheus.Desc
	monitorSLA                *prometheus.Desc
	monitorOutages            *prometheus.Desc
	monitorDowntime           *prometheus.Desc
	monitorLongestOutage      *prometheus.Desc
	monitorMTTR               *prometheus.Desc
	tenantHealthScore         *prometheus.Desc
	tenantUpRatio             *prometheus.Desc
	tenantActiveOutages       *prometheus.Desc
	tenantAvgSLA              *prometheus.Desc
	monitorTier               *prometheus.Desc
	monitorInMaintenance     *prometheus.Desc
	monitorUpByRegion        *prometheus.Desc
	incidentsOpen            *prometheus.Desc
	maintenanceWindowsActive *prometheus.Desc
	monitorResponseTimeAvg    *prometheus.Desc
	monitorMtta               *prometheus.Desc
	monitorAnomalyCount       *prometheus.Desc
	monitorAnomalyScore       *prometheus.Desc
	alertCount                *prometheus.Desc
	cacheTTLDesc              *prometheus.Desc
	excludedDesc              *prometheus.Desc
}

// newCollectorDescs initialises all Prometheus metric descriptors. The
// `project` constLabel is applied to every Desc so series produced by
// distinct Collector instances coexist in one Registry (constLabel value
// is part of Desc identity).
func newCollectorDescs(ns, project string) collectorDescs {
	fqn := prometheus.BuildFQName
	ml := []string{"uuid", "name", "tenant", "tier"}
	mpl := []string{"uuid", "name", "tenant", "tier", "period"}
	cl := prometheus.Labels{"project": resolveProjectID(project)}
	return collectorDescs{
		monitorUp:                 prometheus.NewDesc(fqn(ns, "monitor", "up"), "Whether the monitor is up (1) or down (0).", ml, cl),
		monitorPaused:             prometheus.NewDesc(fqn(ns, "monitor", "paused"), "Whether the monitor is paused (1) or active (0).", ml, cl),
		monitorSSLExpDays:         prometheus.NewDesc(fqn(ns, "monitor", "ssl_expiration_days"), "Days until SSL certificate expiration.", ml, cl),
		monitorCheckInterval:      prometheus.NewDesc(fqn(ns, "monitor", "check_interval_seconds"), "Monitor check frequency in seconds.", ml, cl),
		monitorInfo:               prometheus.NewDesc(fqn(ns, "monitor", "info"), "Monitor metadata (value is always 1).", []string{"uuid", "name", "protocol", "url", "project_uuid", "http_method"}, cl),
		healthcheckUp:             prometheus.NewDesc(fqn(ns, "healthcheck", "up"), "Whether the healthcheck is up (1) or down (0).", []string{"uuid", "name"}, cl),
		healthcheckPaused:         prometheus.NewDesc(fqn(ns, "healthcheck", "paused"), "Whether the healthcheck is paused (1) or active (0).", []string{"uuid", "name"}, cl),
		healthcheckPeriod:         prometheus.NewDesc(fqn(ns, "healthcheck", "period_seconds"), "Expected healthcheck ping period in seconds.", []string{"uuid", "name"}, cl),
		monitorsTotal:             prometheus.NewDesc(fqn(ns, "", "monitors"), "Total number of monitors.", nil, cl),
		healthchecksTotal:         prometheus.NewDesc(fqn(ns, "", "healthchecks"), "Total number of healthchecks.", nil, cl),
		scrapeDurationDesc:        prometheus.NewDesc(fqn(ns, "scrape", "duration_seconds"), "Duration of the last API scrape in seconds.", nil, cl),
		scrapeSuccessDesc:         prometheus.NewDesc(fqn(ns, "scrape", "success"), "Whether the last API scrape succeeded (1) or failed (0).", nil, cl),
		dataAgeDesc: prometheus.NewDesc(
			fqn(ns, "data", "age_seconds"),
			"Seconds elapsed since the last successful API cache refresh, labelled by tier and period. "+
				"In legacy mode and tiered-mode hot tier (which serve data without a window) the series "+
				"is emitted once per active tier with `period=\"\"`. For warm and cold tiers the metric "+
				"is emitted once per configured period that maps to the tier (24h -> warm; 7d/30d/90d/365d "+
				"-> cold), so operators can write `max(data_age_seconds{period=\"30d\"})`-style queries. "+
				"v1.8.0 BREAKING: the prior empty-period legacy series for warm/cold tiers has been "+
				"removed; aggregations like `sum(data_age_seconds{tier=\"warm\"})` now sum across the "+
				"configured periods mapped to that tier instead of double-counting a legacy series.",
			[]string{"tier", "period"}, cl,
		),
		monitorOutageActive:       prometheus.NewDesc(fqn(ns, "monitor", "outage_active"), "Whether the monitor has an active (unresolved) outage (1) or not (0).", ml, cl),
		monitorActiveOutageStatus: prometheus.NewDesc(fqn(ns, "monitor", "active_outage_status_code"), "HTTP status code of the current active outage; 0 when no active outage.", ml, cl),
		monitorSLA:                prometheus.NewDesc(fqn(ns, "monitor", "sla_ratio"), "Monitor SLA as a ratio (0–1) over the labelled period.", mpl, cl),
		monitorOutages:            prometheus.NewDesc(fqn(ns, "monitor", "outages"), "Number of outages over the labelled period.", mpl, cl),
		monitorDowntime:           prometheus.NewDesc(fqn(ns, "monitor", "downtime_seconds"), "Total downtime in seconds over the labelled period.", mpl, cl),
		monitorLongestOutage:      prometheus.NewDesc(fqn(ns, "monitor", "longest_outage_seconds"), "Duration of the longest single outage in seconds over the labelled period.", mpl, cl),
		monitorMTTR:               prometheus.NewDesc(fqn(ns, "monitor", "mttr_seconds"), "Mean Time To Recovery in seconds over the labelled period.", mpl, cl),
		tenantHealthScore:         prometheus.NewDesc(fqn(ns, "tenant", "health_score"), "Composite tenant health score from 0 to 100.", nil, cl),
		tenantUpRatio:             prometheus.NewDesc(fqn(ns, "tenant", "monitors_up_ratio"), "Fraction of monitors currently up (0–1).", nil, cl),
		tenantActiveOutages:       prometheus.NewDesc(fqn(ns, "tenant", "active_outages"), "Total number of active (unresolved) outages across all monitors.", nil, cl),
		tenantAvgSLA:              prometheus.NewDesc(fqn(ns, "tenant", "avg_sla_ratio"), "Average SLA ratio across all monitors for the labelled period.", []string{"period"}, cl),
		monitorTier:               prometheus.NewDesc(fqn(ns, "monitor", "escalation_tier"), "Escalation tier info (always 1). Join on uuid+name; use tier label to filter core/noncore.", []string{"uuid", "name", "tier"}, cl),
		monitorInMaintenance: prometheus.NewDesc(
			fqn(ns, "monitor", "in_maintenance"),
			"1 if the monitor is currently covered by an active maintenance window, 0 otherwise.",
			[]string{"uuid", "name", "tenant", "tier"}, cl,
		),
		monitorUpByRegion: prometheus.NewDesc(
			fqn(ns, "monitor", "up_by_region"),
			"1 if the monitor is up in the given region, 0 if confirmed down. "+
				"Derived from active outage confirmed locations; approximation only.",
			[]string{"uuid", "name", "tenant", "tier", "region"}, cl,
		),
		incidentsOpen: prometheus.NewDesc(
			fqn(ns, "", "incidents_open"),
			"Number of open (non-resolved) incidents.",
			nil, cl,
		),
		maintenanceWindowsActive: prometheus.NewDesc(
			fqn(ns, "", "maintenance_windows_active"),
			"Number of currently active (ongoing) maintenance windows.",
			nil, cl,
		),
		// previously named response_time_avg_seconds
		monitorResponseTimeAvg: prometheus.NewDesc(
			fqn(ns, "monitor", "response_time_seconds"),
			"Average monitor response time in seconds.",
			ml, cl,
		),
		monitorMtta: prometheus.NewDesc(
			fqn(ns, "monitor", "mtta_seconds"),
			"Mean Time To Acknowledge in seconds over the labelled period. "+
				"v1.8.0 BREAKING: a period label is now ALWAYS present on this metric, "+
				"defaulting to \"24h\" for projects that do not opt into additional windows.",
			mpl, cl,
		),
		monitorAnomalyCount: prometheus.NewDesc(
			fqn(ns, "monitor", "anomaly_count"),
			"Number of detected anomalies for the monitor.",
			ml, cl,
		),
		monitorAnomalyScore: prometheus.NewDesc(
			fqn(ns, "monitor", "anomaly_score"),
			"Highest anomaly score for the monitor.",
			ml, cl,
		),
		alertCount: prometheus.NewDesc(
			fqn(ns, "", "alerts"),
			"Snapshot count of alerts in history.",
			nil, cl,
		),
		cacheTTLDesc: prometheus.NewDesc(
			fqn(ns, "cache", "ttl_seconds"),
			"Cache refresh interval in seconds (value of --cache-ttl).",
			nil, cl,
		),
		excludedDesc: prometheus.NewDesc(
			fqn(ns, "excluded", "monitors"),
			"Number of monitors filtered out by --exclude-name-pattern on the last cache refresh; hyperping_monitors counts the visible remainder.",
			nil, cl,
		),
	}
}

// collectorSnapshot is a point-in-time copy of the cache for lock-free metric emission.
type collectorSnapshot struct {
	monitors               []hyperping.Monitor
	healthchecks           []hyperping.Healthcheck
	outageIndex            map[string]hyperping.Outage
	monitorIndex           map[string]hyperping.Monitor // uuid -> monitor, for report label enrichment
	reports                map[string][]hyperping.MonitorReport
	lastSuccessTime        time.Time
	scrapeOK               bool
	scrapeDur              time.Duration
	// dataAges is the per-tier seconds-since-last-success, keyed by tier name
	// ("hot"|"warm"|"cold"). Legacy mode populates only "hot" (the single
	// refresh loop is treated as the HOT tier for label parity with tiered
	// mode). Tiered mode populates all three when each tier has succeeded at
	// least once; tiers that have not yet succeeded are omitted (no series
	// emitted) so the metric semantics stay "elapsed since last success".
	dataAges               map[string]float64
	maintenanceIndex       map[string]bool              // monitor uuid -> covered by active window
	regionDownIndex        map[string]map[string]bool   // uuid -> region -> is_down
	openIncidentCount      int
	activeMaintenanceCount int

	excludedCount int

	// MCP Metrics
	responseTimeIndex map[string]float64
	mttaIndex         map[string]float64
	anomalyCountIndex map[string]int
	anomalyScoreIndex map[string]float64
	totalAlerts       int

	// Per-period MTTA for the multi-period fan-out. Outer map keyed by
	// period token ("24h"/"7d"/"30d"/"90d"/"365d"). When nil/empty, the
	// period-bearing MTTA series for that window is not emitted. The
	// 24h entry shadows mttaIndex (above) for the warm-tier source;
	// cold-mapped periods are populated from the cold-tier MCP fetch only.
	//
	// Per-period MTTR uses a different source: the MonitorReport.MTTR
	// field already on every cold-tier report fetch (see
	// emitReportMetrics). No separate snapshot field is needed because
	// the emission path reads r.MTTR off snap.reports[period] directly.
	mttaByPeriod map[string]map[string]float64
}

// Collector fetches Hyperping data on a background timer and serves
// cached results as Prometheus metrics. It implements prometheus.Collector.
type Collector struct {
	api            HyperpingAPI
	mcp            *hyperping.MCPClient
	mcpMetrics     *MCPMetrics // optional; nil-safe
	cacheTTL       time.Duration
	logger         *slog.Logger
	excludePattern *regexp.Regexp
	project        string // resolved via resolveProjectID at NewCollector time

	// Tiered cache (CacheModeTiered only). When cacheMode == CacheModeLegacy
	// the tiered field is nil and Start/Collect take the legacy path.
	cacheMode CacheMode
	hotTTL    time.Duration
	warmTTL   time.Duration
	coldTTL   time.Duration
	// hot/warm/coldEnabled are honored only in tiered mode. tierEnableSet
	// records whether WithTierEnable was supplied; when false the defaults
	// (true/true/true) apply so existing call sites that pre-date the
	// per-project enable feature behave exactly as before.
	hotEnabled    bool
	warmEnabled   bool
	coldEnabled   bool
	tierEnableSet bool
	tiered        *tieredRefresher

	// periods is the per-collector list of SLA report windows fanned out
	// on the period-bearing metrics. Defaulted by NewCollector to
	// defaultReportPeriods when WithPeriods was not supplied; main's
	// resolvePeriods is the authoritative validator at config-load time.
	periods []string

	// Cache (protected by mu).
	mu                 sync.RWMutex
	excludedCount      int
	monitors           []hyperping.Monitor
	healthchecks       []hyperping.Healthcheck
	outages            []hyperping.Outage
	maintenanceWindows []hyperping.Maintenance
	incidents          []hyperping.Incident
	reportsByPeriod    map[string][]hyperping.MonitorReport
	lastSuccessTime    time.Time
	lastScrapeOK       bool
	lastScrapeDur      time.Duration
	everSucceeded      bool // latches true after first successful scrape; never resets

	// MCP Cache (protected by mu)
	responseTimeIndex map[string]float64
	mttaIndex         map[string]float64
	anomalyCountIndex map[string]int
	anomalyScoreIndex map[string]float64
	totalAlerts       int

	collectorDescs
}

// Verify Collector implements prometheus.Collector at compile time.
var _ prometheus.Collector = (*Collector)(nil)

// NewCollector creates a new Hyperping metrics collector.
func NewCollector(api HyperpingAPI, mcp *hyperping.MCPClient, cacheTTL time.Duration, logger *slog.Logger, namespace string, opts ...CollectorOption) *Collector {
	c := &Collector{
		api:               api,
		mcp:               mcp,
		cacheTTL:          cacheTTL,
		logger:            logger,
		reportsByPeriod:   make(map[string][]hyperping.MonitorReport),
		responseTimeIndex: make(map[string]float64),
		mttaIndex:         make(map[string]float64),
		anomalyCountIndex: make(map[string]int),
		anomalyScoreIndex: make(map[string]float64),
	}
	for _, opt := range opts {
		opt(c)
	}
	// Build Descs AFTER options so WithProject can influence the
	// constLabel; project="default" is the unconditional fallback so
	// downstream dashboards see a stable label set even before the
	// chart's multi-project knob is enabled.
	c.project = resolveProjectID(c.project)
	c.collectorDescs = newCollectorDescs(namespace, c.project)
	// Default the per-tier enable flags to all-true when WithTierEnable
	// was not supplied. Doing this after the options loop keeps
	// WithTierEnable purely additive: a caller that does not invoke it
	// gets exactly the pre-feature behaviour.
	if !c.tierEnableSet {
		c.hotEnabled = true
		c.warmEnabled = true
		c.coldEnabled = true
	}
	// Default periods to the historical full set when WithPeriods was
	// not supplied. Tests that construct a Collector directly (no
	// WithPeriods) get the legacy fan-out exactly as before; the
	// projects-file path always supplies WithPeriods via buildCollectors.
	if c.periods == nil {
		c.periods = make([]string, len(defaultReportPeriods))
		copy(c.periods, defaultReportPeriods)
	}
	// In tiered mode build the refresher up front so callers can drive
	// per-tier refreshes directly (tests do this) and so Start has nothing
	// to allocate on the hot path. Legacy mode leaves c.tiered nil.
	if c.cacheMode == CacheModeTiered {
		c.tiered = newTieredRefresher(c, c.hotTTL, c.warmTTL, c.coldTTL)
		c.tiered.hotDisabled = !c.hotEnabled
		c.tiered.warmDisabled = !c.warmEnabled
		c.tiered.coldDisabled = !c.coldEnabled
	}
	return c
}

// Start begins the background cache refresh loop. It blocks until ctx is cancelled.
//
// In CacheModeLegacy this is a single ticker driving Refresh at cacheTTL
// intervals. In CacheModeTiered this delegates to tieredRefresher.start,
// which runs three independent per-tier tickers.
func (c *Collector) Start(ctx context.Context) {
	if c.cacheMode == CacheModeTiered && c.tiered != nil {
		c.tiered.start(ctx)
		return
	}

	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c.Refresh(initCtx)

	ticker := time.NewTicker(c.cacheTTL)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.Refresh(ctx)
		}
	}
}

// coreData holds the results of a parallel core API fetch.
type coreData struct {
	monitors     []hyperping.Monitor
	healthchecks []hyperping.Healthcheck
	outages      []hyperping.Outage
	maintenance  []hyperping.Maintenance
	incidents    []hyperping.Incident
}

// fetchCoreData fetches monitors, healthchecks, outages, maintenance windows, and incidents in parallel.
// Outage, maintenance, and incident failures are non-fatal: errors are logged and nil slices are returned,
// signalling the caller to retain stale data. Monitor or healthcheck failures are fatal.
func (c *Collector) fetchCoreData(ctx context.Context) (coreData, error) {
	var (
		result         coreData
		monErr         error
		hcErr          error
		outageErr      error
		maintenanceErr error
		incidentsErr   error
		wg             sync.WaitGroup
	)

	wg.Add(5)
	go func() { defer wg.Done(); result.monitors, monErr = c.api.ListMonitors(ctx) }()
	go func() { defer wg.Done(); result.healthchecks, hcErr = c.api.ListHealthchecks(ctx) }()
	go func() { defer wg.Done(); result.outages, outageErr = c.api.ListOutages(ctx) }()
	go func() { defer wg.Done(); result.maintenance, maintenanceErr = c.api.ListMaintenance(ctx) }()
	go func() { defer wg.Done(); result.incidents, incidentsErr = c.api.ListIncidents(ctx) }()
	wg.Wait()

	if monErr != nil {
		c.logger.Error("failed to list monitors", "error", monErr)
		return coreData{}, monErr
	}
	if hcErr != nil {
		c.logger.Error("failed to list healthchecks", "error", hcErr)
		return coreData{}, hcErr
	}
	if outageErr != nil {
		c.logger.Warn("failed to list outages; outage metrics will use stale data", "error", outageErr)
		result.outages = nil
	}
	if maintenanceErr != nil {
		c.logger.Warn("failed to list maintenance windows; metrics will use stale data", "error", maintenanceErr)
		result.maintenance = nil
	}
	if incidentsErr != nil {
		c.logger.Warn("failed to list incidents; metrics will use stale data", "error", incidentsErr)
		result.incidents = nil
	}
	return result, nil
}

// fetchReports fetches SLA reports for all periods in parallel. Failures per
// period are logged as warnings; the returned map omits periods that failed.
//
// The set of periods is sourced from c.periods (defaulted by NewCollector
// to defaultReportPeriods when WithPeriods was not used) so per-project
// configuration controls which windows are fetched in legacy mode.
func (c *Collector) fetchReports(ctx context.Context, now time.Time) map[string][]hyperping.MonitorReport {
	results := make(map[string][]hyperping.MonitorReport, len(c.periods))
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		failures int
	)
	for _, p := range c.periods {
		dur := reportDurations[p]
		from := now.Add(-dur).Format(time.RFC3339)
		to := now.Format(time.RFC3339)
		wg.Add(1)
		go func(period, fromStr, toStr string) {
			defer wg.Done()
			reports, err := c.api.ListMonitorReports(ctx, fromStr, toStr)
			if err != nil {
				c.logger.Warn("failed to fetch monitor reports", "period", period, "error", err)
				mu.Lock()
				failures++
				mu.Unlock()
				return
			}
			mu.Lock()
			results[period] = reports
			mu.Unlock()
		}(p, from, to)
	}
	wg.Wait()
	if failures == len(c.periods) {
		c.logger.Warn("all report period fetches failed; SLA metrics will use stale data")
	}
	return results
}

type mcpData struct {
	responseTime map[string]float64
	mtta         map[string]float64
	anomalyCount map[string]int
	anomalyScore map[string]float64
	totalAlerts  int
	// partial is true when at least one per-monitor MCP fetch failed and a
	// stale cached value was retained in place of a fresh one. Surfaced
	// via the mcp_partial=true field in the cache-refreshed log line and
	// via the hyperping_mcp_partial_refresh_total counter.
	partial bool
}

// fetchMcpData fetches advanced metrics from the MCP server in parallel.
// It uses a worker pool to limit concurrency and handles per-monitor failures gracefully.
func (c *Collector) fetchMcpData(ctx context.Context, monitors []hyperping.Monitor) (mcpData, error) {
	if c.mcp == nil {
		return mcpData{}, nil
	}

	// Initialise results with current cached values to ensure graceful degradation
	// on per-monitor failures (partial updates).
	c.mu.RLock()
	res := mcpData{
		responseTime: make(map[string]float64, len(c.responseTimeIndex)),
		mtta:         make(map[string]float64, len(c.mttaIndex)),
		anomalyCount: make(map[string]int, len(c.anomalyCountIndex)),
		anomalyScore: make(map[string]float64, len(c.anomalyScoreIndex)),
		totalAlerts:  c.totalAlerts,
	}
	for k, v := range c.responseTimeIndex {
		res.responseTime[k] = v
	}
	for k, v := range c.mttaIndex {
		res.mtta[k] = v
	}
	for k, v := range c.anomalyCountIndex {
		res.anomalyCount[k] = v
	}
	for k, v := range c.anomalyScoreIndex {
		res.anomalyScore[k] = v
	}
	c.mu.RUnlock()

	var mu sync.Mutex
	var wg sync.WaitGroup
	// failures counts the number of per-call MCP errors observed across this
	// refresh (global alerts fetch + per-monitor response-time / mtta /
	// anomalies). A non-zero value at the end of the refresh means we served
	// at least one stale-cached MCP value instead of a fresh one, which
	// increments mcp_partial_refresh_total. Context-cancellation aborts are
	// not counted; the refresh is being abandoned, not partial.
	var failures atomic.Int64

	// 1. Fetch global alert history
	wg.Add(1)
	go func() {
		defer wg.Done()
		alerts, err := c.mcp.ListRecentAlerts(ctx)
		if err != nil {
			if ctx.Err() == nil {
				failures.Add(1)
			}
			c.logger.Warn("failed to fetch recent alerts from MCP", "error", err)
			return
		}
		// hyperping-go returns (nil, nil) when the MCP server responds with
		// an empty content array (a legitimate "no alerts" shape). Retain
		// the previous cached totalAlerts in that case rather than zeroing.
		if alerts == nil {
			return
		}
		mu.Lock()
		res.totalAlerts = alerts.Total()
		mu.Unlock()
	}()

	// 2. Fetch per-monitor metrics using a worker pool. Skip the pool entirely
	// when there are no monitors to process; the alert-fetch goroutine added
	// above is independent and will be drained by the wg.Wait() below.
	if len(monitors) == 0 {
		wg.Wait()
		return res, nil
	}
	monitorChan := make(chan hyperping.Monitor, len(monitors))
	for _, m := range monitors {
		monitorChan <- m
	}
	close(monitorChan)

	// Limit concurrency to 10 workers (or fewer if fewer monitors).
	numWorkers := 10
	if len(monitors) < numWorkers {
		numWorkers = len(monitors)
	}

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case m, ok := <-monitorChan:
					if !ok {
						return
					}
					// Double-check client is still available
					if c.mcp == nil {
						return
					}
					uuid := m.UUID

					// 1. Response Time
					//
					// v0.7.0 BREAKING: GetMonitorResponseTime now takes
					// (ctx, from, to, uuids...). Preserve legacy behaviour by
					// passing a 24h window centered on now and a single uuid.
					{
						opCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
						now := time.Now().UTC()
						report, err := c.mcp.GetMonitorResponseTime(opCtx, now.Add(-24*time.Hour), now, uuid)
						cancel()
						if err == nil && report != nil {
							mu.Lock()
							res.responseTime[uuid] = report.AvgResponseTime
							mu.Unlock()
						} else if ctx.Err() != nil {
							return
						} else if err != nil {
							failures.Add(1)
							c.logger.Debug("failed to fetch response time", "uuid", uuid, "error", err)
						}
					}

					// 2. MTTA
					//
					// v0.7.0 BREAKING: GetMonitorMtta now takes
					// (ctx, from, to, uuids...). Preserve legacy behaviour by
					// passing a 24h window. The pre-v0.7.0 call silently
					// decoded into zero values; this exporter therefore
					// emitted mtta_seconds=0 for every monitor for months.
					{
						opCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
						now := time.Now().UTC()
						report, err := c.mcp.GetMonitorMtta(opCtx, now.Add(-24*time.Hour), now, uuid)
						cancel()
						if err == nil && report != nil {
							mu.Lock()
							res.mtta[uuid] = report.Mtta
							mu.Unlock()
						} else if ctx.Err() != nil {
							return
						} else if err != nil {
							failures.Add(1)
							c.logger.Debug("failed to fetch mtta", "uuid", uuid, "error", err)
						}
					}

					// 3. Anomalies
					{
						opCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
						anomalies, err := c.mcp.GetMonitorAnomalies(opCtx, uuid)
						cancel()
						if err == nil {
							mu.Lock()
							res.anomalyCount[uuid] = len(anomalies)
							maxScore := 0.0
							for _, a := range anomalies {
								if a.Score > maxScore {
									maxScore = a.Score
								}
							}
							res.anomalyScore[uuid] = maxScore
							mu.Unlock()
						} else if ctx.Err() != nil {
							return
						} else {
							failures.Add(1)
							c.logger.Debug("failed to fetch anomalies", "uuid", uuid, "error", err)
						}
					}
				}
			}
		}()
	}

	wg.Wait()

	// Surface "we kept stale cached values for at least one MCP call" both
	// as a flag on the returned struct (for the log line) and as a counter
	// increment (for /metrics). Reading failures after wg.Wait is safe; the
	// writers have synchronized through the WaitGroup.
	if failures.Load() > 0 {
		res.partial = true
		if c.mcpMetrics != nil {
			c.mcpMetrics.PartialRefreshTotal.Inc()
		}
	}
	return res, nil
}

// Refresh performs a single API scrape and updates the cache.
// Monitors and healthchecks are required; outages and reports are best-effort.
func (c *Collector) Refresh(ctx context.Context) {
	start := time.Now()

	// REST core data fetch (monitors, healthchecks, etc.)
	core, coreErr := c.fetchCoreData(ctx)
	if coreErr != nil {
		c.mu.Lock()
		c.lastScrapeOK = false
		c.lastScrapeDur = time.Since(start)
		c.mu.Unlock()
		return
	}

	// Apply name exclusion filter before any downstream processing.
	monitors, excluded := filterMonitorsByName(core.monitors, c.excludePattern)
	var includedUUIDs map[string]struct{}
	if len(excluded) > 0 {
		names := make([]string, len(excluded))
		for i, m := range excluded {
			names[i] = m.Name
		}
		c.logger.Debug("excluded monitors by name pattern", "count", len(excluded), "names", names)
		// Build inclusion set once and reuse it to filter outages now and
		// reports after the parallel fetch completes below.
		includedUUIDs = make(map[string]struct{}, len(monitors))
		for _, m := range monitors {
			includedUUIDs[m.UUID] = struct{}{}
		}
		core.outages = filterOutagesByMonitorUUID(core.outages, includedUUIDs)
	}
	core.monitors = monitors

	var (
		mcp           mcpData
		mcpErr        error
		reportResults map[string][]hyperping.MonitorReport
		wg            sync.WaitGroup
	)

	// Now that we have monitors, fetch reports and MCP metrics in parallel
	wg.Add(2)
	go func() {
		defer wg.Done()
		reportResults = c.fetchReports(ctx, start.UTC())
	}()
	go func() {
		defer wg.Done()
		mcp, mcpErr = c.fetchMcpData(ctx, core.monitors)
	}()
	wg.Wait()

	// ListMonitorReports returns reports for every monitor in the account, so
	// the same exclusion filter has to be applied here. Without this, excluded
	// monitors would inflate len(reports) in the tenant SLA average denominator
	// and would be summed into avgSLAForPeriod for the health score.
	if includedUUIDs != nil {
		for period, reports := range reportResults {
			reportResults[period] = filterReportsByMonitorUUID(reports, includedUUIDs)
		}
	}

	dur := time.Since(start)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.lastScrapeDur = dur

	if core.outages != nil {
		c.outages = core.outages
	}
	if core.maintenance != nil {
		c.maintenanceWindows = core.maintenance
	}
	if core.incidents != nil {
		c.incidents = core.incidents
	}
	c.excludedCount = len(excluded)
	c.monitors = core.monitors
	c.healthchecks = core.healthchecks

	if mcpErr == nil {
		c.responseTimeIndex = mcp.responseTime
		c.mttaIndex = mcp.mtta
		c.anomalyCountIndex = mcp.anomalyCount
		c.anomalyScoreIndex = mcp.anomalyScore
		c.totalAlerts = mcp.totalAlerts
	}

	// Merge successful period results; periods that failed retain previous data.
	for period, reports := range reportResults {
		c.reportsByPeriod[period] = reports
	}
	c.lastScrapeOK = true
	c.everSucceeded = true
	c.lastSuccessTime = time.Now()

	// mcp_metrics is true when the top-level fetchMcpData call succeeded
	// (no fatal error); mcp_partial is true when fetchMcpData succeeded
	// but at least one per-monitor call retained its stale cached value.
	// The latter is also surfaced as hyperping_mcp_partial_refresh_total.
	c.logger.Info("cache refreshed",
		"monitors", len(core.monitors),
		"healthchecks", len(core.healthchecks),
		"outages", len(c.outages),
		"maintenance_windows", len(c.maintenanceWindows),
		"incidents", len(c.incidents),
		"mcp_metrics", mcpErr == nil,
		"mcp_partial", mcp.partial,
		"duration", dur,
	)
}

// Project returns the resolved project constLabel value for this Collector
// (the value of WithProject, or "default" if unset). main.go consumes this
// to label the per-project readiness gauge so dashboards/alerts can see
// which project is failing under the OR readiness policy.
func (c *Collector) Project() string {
	return c.project
}

// IsReady returns true once at least one successful API scrape has completed.
// It never reverts to false: transient failures after the first success do not
// affect readiness, staleness is surfaced by hyperping_data_age_seconds instead.
//
// In tiered mode "successful" means the HOT tier has published at least once;
// WARM and COLD lag is expected during the post-boot cold-start window and
// must not gate /readyz.
func (c *Collector) IsReady() bool {
	if c.cacheMode == CacheModeTiered && c.tiered != nil {
		return c.tiered.hotReady.Load()
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.everSucceeded
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.monitorUp
	ch <- c.monitorPaused
	ch <- c.monitorSSLExpDays
	ch <- c.monitorCheckInterval
	ch <- c.monitorInfo
	ch <- c.healthcheckUp
	ch <- c.healthcheckPaused
	ch <- c.healthcheckPeriod
	ch <- c.monitorsTotal
	ch <- c.healthchecksTotal
	ch <- c.scrapeDurationDesc
	ch <- c.scrapeSuccessDesc
	ch <- c.dataAgeDesc
	ch <- c.monitorOutageActive
	ch <- c.monitorActiveOutageStatus
	ch <- c.monitorSLA
	ch <- c.monitorOutages
	ch <- c.monitorDowntime
	ch <- c.monitorLongestOutage
	ch <- c.monitorMTTR
	ch <- c.tenantHealthScore
	ch <- c.tenantUpRatio
	ch <- c.tenantActiveOutages
	ch <- c.tenantAvgSLA
	ch <- c.monitorTier
	ch <- c.monitorInMaintenance
	ch <- c.monitorUpByRegion
	ch <- c.incidentsOpen
	ch <- c.maintenanceWindowsActive
	ch <- c.monitorResponseTimeAvg
	ch <- c.monitorMtta
	ch <- c.monitorAnomalyCount
	ch <- c.monitorAnomalyScore
	ch <- c.alertCount
	ch <- c.cacheTTLDesc
	ch <- c.excludedDesc
}

// Collect implements prometheus.Collector.
// Cached slices are copied under a minimal read lock; all index building and
// metric emission happen outside the lock to avoid blocking concurrent Refresh calls.
//
// In tiered mode the snapshot comes from buildCollectorSnapshot (three atomic
// Loads + stitch); the legacy snapshot path below is skipped entirely.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	if c.cacheMode == CacheModeTiered && c.tiered != nil {
		snap := c.tiered.buildCollectorSnapshot()
		c.emitMonitorMetrics(ch, snap)
		c.emitHealthcheckMetrics(ch, snap)
		c.emitReportMetrics(ch, snap)
		c.emitTenantMetrics(ch, snap)
		c.emitMcpMetrics(ch, snap)
		return
	}
	// STEP 1: Copy raw cached state under read lock (no CPU-heavy work here).
	c.mu.RLock()
	monitors := c.monitors
	healthchecks := c.healthchecks
	outages := c.outages
	maintenance := c.maintenanceWindows
	incidents := c.incidents
	reports := make(map[string][]hyperping.MonitorReport, len(c.reportsByPeriod))
	for k, v := range c.reportsByPeriod {
		reports[k] = v
	}
	lastSuccess := c.lastSuccessTime
	scrapeOK := c.lastScrapeOK
	scrapeDur := c.lastScrapeDur

	// Copy MCP cache
	rtIdx := make(map[string]float64, len(c.responseTimeIndex))
	for k, v := range c.responseTimeIndex {
		rtIdx[k] = v
	}
	mttaIdx := make(map[string]float64, len(c.mttaIndex))
	for k, v := range c.mttaIndex {
		mttaIdx[k] = v
	}
	anomCountIdx := make(map[string]int, len(c.anomalyCountIndex))
	for k, v := range c.anomalyCountIndex {
		anomCountIdx[k] = v
	}
	anomScoreIdx := make(map[string]float64, len(c.anomalyScoreIndex))
	for k, v := range c.anomalyScoreIndex {
		anomScoreIdx[k] = v
	}
	totalAlerts := c.totalAlerts
	excludedCount := c.excludedCount
	c.mu.RUnlock()

	// STEP 2: Build all derived indices outside the lock.
	// Legacy mode treats its single refresh ticker as the HOT tier for
	// label parity with tiered mode. Only emit a data_age series once a
	// successful refresh has been observed.
	dataAges := map[string]float64{}
	if !lastSuccess.IsZero() {
		dataAges["hot"] = time.Since(lastSuccess).Seconds()
	}
	outageIdx := buildActiveOutageIndex(outages)
	monIdx := make(map[string]hyperping.Monitor, len(monitors))
	for _, m := range monitors {
		monIdx[m.UUID] = m
	}
	maintenanceIdx, activeMaintenanceCount := buildMaintenanceIndex(maintenance, monitors)

	snap := collectorSnapshot{
		monitors:               monitors,
		healthchecks:           healthchecks,
		outageIndex:            outageIdx,
		monitorIndex:           monIdx,
		reports:                reports,
		lastSuccessTime:        lastSuccess,
		scrapeOK:               scrapeOK,
		scrapeDur:              scrapeDur,
		dataAges:               dataAges,
		maintenanceIndex:       maintenanceIdx,
		regionDownIndex:        buildRegionDownIndex(outageIdx),
		openIncidentCount:      countOpenIncidents(incidents),
		activeMaintenanceCount: activeMaintenanceCount,

		excludedCount: excludedCount,

		// MCP Metrics
		responseTimeIndex: rtIdx,
		mttaIndex:         mttaIdx,
		anomalyCountIndex: anomCountIdx,
		anomalyScoreIndex: anomScoreIdx,
		totalAlerts:       totalAlerts,
	}

	c.emitMonitorMetrics(ch, snap)
	c.emitHealthcheckMetrics(ch, snap)
	c.emitReportMetrics(ch, snap)
	c.emitTenantMetrics(ch, snap)
	c.emitMcpMetrics(ch, snap)
}

// emitMonitorMetrics sends per-monitor metrics derived from the snapshot.
//
// Every label value derived from m.Name is passed through capLabel so a
// multi-kilobyte monitor name (rename-rights abuse, compromised account)
// cannot fan out across the ~13 series per monitor; see maxLabelValueBytes.
func (c *Collector) emitMonitorMetrics(ch chan<- prometheus.Metric, snap collectorSnapshot) {
	for _, m := range snap.monitors {
		tenant := extractTenant(m.Name)
		tier := escalationTier(m)
		name := capLabel(m.Name)
		ch <- prometheus.MustNewConstMetric(c.monitorUp, prometheus.GaugeValue,
			boolToFloat64(m.Status == "up"), m.UUID, name, tenant, tier)
		ch <- prometheus.MustNewConstMetric(c.monitorPaused, prometheus.GaugeValue,
			boolToFloat64(m.Paused), m.UUID, name, tenant, tier)
		ch <- prometheus.MustNewConstMetric(c.monitorCheckInterval, prometheus.GaugeValue,
			float64(m.CheckFrequency), m.UUID, name, tenant, tier)
		// M2: strip query params to prevent label cardinality explosion.
		ch <- prometheus.MustNewConstMetric(c.monitorInfo, prometheus.GaugeValue, 1,
			m.UUID, name, m.Protocol, sanitizeURL(m.URL), m.ProjectUUID, m.HTTPMethod)

		if m.SSLExpiration != nil {
			ch <- prometheus.MustNewConstMetric(c.monitorSSLExpDays, prometheus.GaugeValue,
				float64(*m.SSLExpiration), m.UUID, name, tenant, tier)
		}

		// OPS-32: active outage state and HTTP status code.
		activeOutage, hasActive := snap.outageIndex[m.UUID]
		ch <- prometheus.MustNewConstMetric(c.monitorOutageActive, prometheus.GaugeValue,
			boolToFloat64(hasActive), m.UUID, name, tenant, tier)
		statusCode := 0
		if hasActive {
			statusCode = activeOutage.StatusCode
		}
		ch <- prometheus.MustNewConstMetric(c.monitorActiveOutageStatus, prometheus.GaugeValue,
			float64(statusCode), m.UUID, name, tenant, tier)

		// OPS-39: escalation tier info.
		ch <- prometheus.MustNewConstMetric(c.monitorTier, prometheus.GaugeValue, 1,
			m.UUID, name, tier)

		// EXP-02: maintenance window coverage.
		inMaint := 0.0
		if snap.maintenanceIndex[m.UUID] {
			inMaint = 1.0
		}
		ch <- prometheus.MustNewConstMetric(c.monitorInMaintenance, prometheus.GaugeValue,
			inMaint, m.UUID, name, tenant, tier)

		// EXP-03: per-region up/down status.
		if len(m.Regions) > 0 {
			downRegions := snap.regionDownIndex[m.UUID]
			for _, region := range m.Regions {
				val := 1.0
				if downRegions[region] {
					val = 0.0
				}
				ch <- prometheus.MustNewConstMetric(c.monitorUpByRegion, prometheus.GaugeValue,
					val, m.UUID, name, tenant, tier, region)
			}
		}

		// MCP Metrics
		if val, ok := snap.responseTimeIndex[m.UUID]; ok {
			ch <- prometheus.MustNewConstMetric(c.monitorResponseTimeAvg, prometheus.GaugeValue,
				val, m.UUID, name, tenant, tier)
		}
		// v1.8.0: MTTA now fans out across configured periods. The 24h
		// slice is sourced from the warm-tier per-monitor map
		// (snap.mttaIndex) when no per-period override exists; other
		// periods come from snap.mttaByPeriod populated by the cold tier.
		// When neither source has a value for (uuid, period), no series
		// is emitted for that combination (matches the pre-1.8 behaviour
		// of skipping the metric on missing data).
		for _, period := range c.periods {
			val, ok := mttaValueFor(snap, m.UUID, period)
			if !ok {
				continue
			}
			ch <- prometheus.MustNewConstMetric(c.monitorMtta, prometheus.GaugeValue,
				val, m.UUID, name, tenant, tier, period)
		}
		if val, ok := snap.anomalyCountIndex[m.UUID]; ok {
			ch <- prometheus.MustNewConstMetric(c.monitorAnomalyCount, prometheus.GaugeValue,
				float64(val), m.UUID, name, tenant, tier)
		}
		if val, ok := snap.anomalyScoreIndex[m.UUID]; ok {
			ch <- prometheus.MustNewConstMetric(c.monitorAnomalyScore, prometheus.GaugeValue,
				val, m.UUID, name, tenant, tier)
		}
	}
}

// emitHealthcheckMetrics sends per-healthcheck metrics derived from the snapshot.
// Healthcheck names are capped through capLabel for the same rename-rights /
// label-bytes blowup reason as monitor names.
func (c *Collector) emitHealthcheckMetrics(ch chan<- prometheus.Metric, snap collectorSnapshot) {
	for _, hc := range snap.healthchecks {
		name := capLabel(hc.Name)
		ch <- prometheus.MustNewConstMetric(c.healthcheckUp, prometheus.GaugeValue,
			boolToFloat64(!hc.IsDown), hc.UUID, name)
		ch <- prometheus.MustNewConstMetric(c.healthcheckPaused, prometheus.GaugeValue,
			boolToFloat64(hc.IsPaused), hc.UUID, name)
		ch <- prometheus.MustNewConstMetric(c.healthcheckPeriod, prometheus.GaugeValue,
			float64(hc.Period), hc.UUID, name)
	}
}

// emitReportMetrics sends per-monitor SLA/outage report metrics and per-period tenant averages.
//
// Iterates over c.periods rather than the package-level defaultReportPeriods
// so a project that opts into a subset (e.g. just 24h) emits exactly that
// subset's series. Periods whose mapped tier is disabled on this project
// produce no entry in snap.reports[period] (the refresher never populates
// them), so the inner loop is naturally a no-op for those.
func (c *Collector) emitReportMetrics(ch chan<- prometheus.Metric, snap collectorSnapshot) {
	for _, period := range c.periods {
		reports := snap.reports[period]
		slaSum := 0.0
		// slaCount tracks reports actually summed (visible monitors only); using
		// len(reports) as the denominator silently skews the average whenever a
		// report's monitor is absent from the index, e.g. excluded by
		// --exclude-name-pattern or removed between fetches.
		slaCount := 0
		for _, r := range reports {
			mon, ok := snap.monitorIndex[r.UUID]
			if !ok {
				continue
			}
			tenant := extractTenant(mon.Name)
			tier := escalationTier(mon)
			// Use the live monitor's name, not the report's Name field. The
			// API records the monitor's name at report-generation time, so
			// a rename mid-window leaves the report carrying the stale
			// label. Using mon.Name keeps all per-uuid series (base + SLA)
			// on a single, consistent `name` label.
			name := capLabel(mon.Name)
			sla := r.SLA / 100.0 // API returns 0–100; expose as 0–1
			ch <- prometheus.MustNewConstMetric(c.monitorSLA, prometheus.GaugeValue,
				sla, r.UUID, name, tenant, tier, period)
			ch <- prometheus.MustNewConstMetric(c.monitorOutages, prometheus.GaugeValue,
				float64(r.Outages.Count), r.UUID, name, tenant, tier, period)
			ch <- prometheus.MustNewConstMetric(c.monitorDowntime, prometheus.GaugeValue,
				float64(r.Outages.TotalDowntime), r.UUID, name, tenant, tier, period)
			ch <- prometheus.MustNewConstMetric(c.monitorLongestOutage, prometheus.GaugeValue,
				float64(r.Outages.LongestOutage), r.UUID, name, tenant, tier, period)
			if r.MTTR > 0 {
				ch <- prometheus.MustNewConstMetric(c.monitorMTTR, prometheus.GaugeValue,
					float64(r.MTTR), r.UUID, name, tenant, tier, period)
			}
			slaSum += sla
			slaCount++
		}
		if slaCount > 0 {
			ch <- prometheus.MustNewConstMetric(c.tenantAvgSLA, prometheus.GaugeValue,
				slaSum/float64(slaCount), period)
		}
	}
}

// emitTenantMetrics sends tenant-wide and scrape self-metrics derived from the snapshot.
func (c *Collector) emitTenantMetrics(ch chan<- prometheus.Metric, snap collectorSnapshot) {
	// Summary self-metrics.
	ch <- prometheus.MustNewConstMetric(c.monitorsTotal, prometheus.GaugeValue,
		float64(len(snap.monitors)))
	ch <- prometheus.MustNewConstMetric(c.healthchecksTotal, prometheus.GaugeValue,
		float64(len(snap.healthchecks)))
	ch <- prometheus.MustNewConstMetric(c.scrapeDurationDesc, prometheus.GaugeValue,
		snap.scrapeDur.Seconds())
	ch <- prometheus.MustNewConstMetric(c.scrapeSuccessDesc, prometheus.GaugeValue,
		boolToFloat64(snap.scrapeOK))

	// OPS-31: data age, labelled by tier and period. Tier order is fixed
	// for stable scrape ordering across ticks; tiers with no prior success
	// are omitted so the metric continues to mean "elapsed since last
	// success".
	//
	// v1.8.0: the Desc carries (tier, period) labels. Emission scheme:
	//   - HOT tier (legacy single-refresh ticker, or the tiered HOT tier
	//     that serves monitors/healthchecks/outages) has no window; it is
	//     emitted once with `period=""` to retain pre-1.8 observability of
	//     up/down freshness.
	//   - WARM and COLD tiers serve report data tied to a window; one
	//     series is emitted per configured period whose PeriodTier maps
	//     to the tier. No empty-period legacy series is emitted for these
	//     tiers; aggregations like `sum(data_age_seconds{tier="warm"})`
	//     now sum across periods mapped to warm (1 series for the default
	//     `periods=["24h"]`, N series for multi-period configs) instead of
	//     double-counting a legacy empty-period series.
	for _, tier := range []string{"hot", "warm", "cold"} {
		age, ok := snap.dataAges[tier]
		if !ok || age <= 0 {
			continue
		}
		if tier == "hot" {
			// HOT carries no window; one series with period="" preserves
			// pre-1.8 `data_age_seconds{tier="hot"}` selectors.
			ch <- prometheus.MustNewConstMetric(c.dataAgeDesc, prometheus.GaugeValue, age, tier, "")
			continue
		}
		for _, period := range c.periods {
			if PeriodTier(period) != tier {
				continue
			}
			ch <- prometheus.MustNewConstMetric(c.dataAgeDesc, prometheus.GaugeValue, age, tier, period)
		}
	}

	// OPS-34: tenant-wide health metrics.
	upCount := 0
	for _, m := range snap.monitors {
		if m.Status == "up" {
			upCount++
		}
	}
	upRatio := 0.0
	if len(snap.monitors) > 0 {
		upRatio = float64(upCount) / float64(len(snap.monitors))
	}
	ch <- prometheus.MustNewConstMetric(c.tenantUpRatio, prometheus.GaugeValue, upRatio)
	ch <- prometheus.MustNewConstMetric(c.tenantActiveOutages, prometheus.GaugeValue,
		float64(len(snap.outageIndex)))
	// Health score requires 30d SLA data; omit until reports are loaded to avoid
	// misleadingly low scores (upRatio×60 + 0×40 = 60 even for a healthy fleet).
	if reports30d := snap.reports["30d"]; len(reports30d) > 0 {
		// Only emit health score when at least one visible monitor has a 30d
		// report. Otherwise avg30dSLA would be 0, collapsing the score to
		// upRatio*60 - penalty for an otherwise-healthy fleet.
		if avg30dSLA, ok := avgSLAForPeriod(reports30d, snap.monitorIndex); ok {
			ch <- prometheus.MustNewConstMetric(c.tenantHealthScore, prometheus.GaugeValue,
				computeHealthScore(upRatio, avg30dSLA, len(snap.outageIndex), len(snap.monitors)))
		}
	}

	// EXP-04: open incidents and active maintenance windows.
	ch <- prometheus.MustNewConstMetric(c.incidentsOpen, prometheus.GaugeValue,
		float64(snap.openIncidentCount))
	ch <- prometheus.MustNewConstMetric(c.maintenanceWindowsActive, prometheus.GaugeValue,
		float64(snap.activeMaintenanceCount))

	ch <- prometheus.MustNewConstMetric(c.cacheTTLDesc, prometheus.GaugeValue,
		c.cacheTTL.Seconds())
	ch <- prometheus.MustNewConstMetric(c.excludedDesc, prometheus.GaugeValue,
		float64(snap.excludedCount))
}

// buildActiveOutageIndex returns a map of monitor UUID → active Outage.
// An outage is active when IsResolved is false and EndDate is nil (ongoing).
func buildActiveOutageIndex(outages []hyperping.Outage) map[string]hyperping.Outage {
	idx := make(map[string]hyperping.Outage, len(outages))
	for _, o := range outages {
		if !o.IsResolved && o.EndDate == nil {
			idx[o.Monitor.UUID] = o
		}
	}
	return idx
}

// buildMaintenanceIndex returns a coverage map and the count of active windows.
// The map contains monitor UUID -> true for all monitors covered by at least one
// currently-active window (Status == "ongoing"). If an active window has no monitor
// UUIDs (account-level window), every monitor in the monitors slice is considered covered.
func buildMaintenanceIndex(windows []hyperping.Maintenance, monitors []hyperping.Monitor) (map[string]bool, int) {
	idx := make(map[string]bool)
	activeCount := 0
	for _, w := range windows {
		if w.Status != "ongoing" {
			continue
		}
		activeCount++
		if len(w.Monitors) == 0 {
			// Account-level window: covers all monitors.
			for _, m := range monitors {
				idx[m.UUID] = true
			}
		} else {
			for _, uuid := range w.Monitors {
				idx[uuid] = true
			}
		}
	}
	return idx, activeCount
}

// buildRegionDownIndex returns a map of monitor UUID -> set of region names
// that are confirmed down based on active outages.
// ConfirmedLocations is a comma-separated string of region names.
func buildRegionDownIndex(outageIndex map[string]hyperping.Outage) map[string]map[string]bool {
	idx := make(map[string]map[string]bool)
	for monUUID, o := range outageIndex {
		regionSet := make(map[string]bool)
		if o.DetectedLocation != "" {
			regionSet[o.DetectedLocation] = true
		}
		if o.ConfirmedLocations != "" {
			for _, r := range strings.Split(o.ConfirmedLocations, ",") {
				r = strings.TrimSpace(r)
				if r != "" {
					regionSet[r] = true
				}
			}
		}
		if len(regionSet) > 0 {
			idx[monUUID] = regionSet
		}
	}
	return idx
}

// countOpenIncidents returns the number of incidents where Type != "resolved".
func countOpenIncidents(incidents []hyperping.Incident) int {
	count := 0
	for _, inc := range incidents {
		if inc.Type != "resolved" {
			count++
		}
	}
	return count
}

// sanitizeURL strips query parameters and fragments from a URL to prevent
// label cardinality explosion from session tokens or trace IDs in query strings.
// On parse failure it falls back to a simple string truncation at '?' or '#'
// so that query params are never leaked even when the URL is malformed.
func sanitizeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		// Fallback: strip at the first query/fragment delimiter without url.Parse.
		if i := strings.IndexAny(raw, "?#"); i != -1 {
			return raw[:i]
		}
		return raw
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// maxLabelValueBytes caps every monitor-name-derived label value so a
// compromised Hyperping account, or an operator with rename rights, cannot
// mint multi-kilobyte names that fan out across ~13 series per monitor and
// inflate Prometheus memory on the ingest side. 256 bytes is well above
// realistic human-readable monitor names and below the 1 KiB threshold at
// which most Prometheus deployments start to feel label pressure.
const maxLabelValueBytes = 256

// capLabel returns s if it is at most maxLabelValueBytes long, otherwise it
// returns the longest valid-UTF-8 prefix of s that fits within the cap. The
// truncation never splits a multibyte rune, which would emit U+FFFD on the
// scrape and be rejected by stricter ingesters.
func capLabel(s string) string {
	if len(s) <= maxLabelValueBytes {
		return s
	}
	// Walk forward in rune steps, stopping just before we would exceed cap.
	out := 0
	for i := range s {
		if i > maxLabelValueBytes {
			break
		}
		out = i
	}
	return s[:out]
}

// reTenantID restricts the tenant label to ASCII alphanumerics plus a small
// set of punctuation that is safe in dashboards, alerting queries, and log
// pipelines. The 1-64 length range mirrors validateNamespace and gives
// cardinality predictability: a tenant label can never exceed 64 bytes.
var reTenantID = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)

// extractTenant derives the tenant ID from the monitor name convention
// "[TENANT-ID]-SuffixName". The substring between the leading '[' and the
// first ']' is returned only if it matches reTenantID; anything else
// collapses to "" so a weird Unicode character, control byte, HTML payload,
// or path-traversal sequence cannot appear verbatim in the `tenant` label.
//
// The strict-input contract also prevents cardinality collisions: legitimate
// tenants whose names contain stray whitespace or punctuation are no longer
// silently aggregated under a slightly-different string than their peers.
// Operators see the empty-tenant bucket instead and can fix the rename.
func extractTenant(monitorName string) string {
	if !strings.HasPrefix(monitorName, "[") {
		return ""
	}
	end := strings.Index(monitorName, "]")
	if end < 0 {
		return ""
	}
	candidate := monitorName[1:end]
	if !reTenantID.MatchString(candidate) {
		return ""
	}
	return candidate
}

// escalationTier classifies the monitor as "core", "noncore", or "unknown".
// Returns "unknown" when no escalation policy is set (nil) or the policy name is empty.
// Returns "noncore" when the policy name contains "noncore" or "non-core" (case-insensitive).
// Returns "core" otherwise.
func escalationTier(m hyperping.Monitor) string {
	if m.EscalationPolicy == nil || m.EscalationPolicy.Name == "" {
		return "unknown"
	}
	name := strings.ToLower(m.EscalationPolicy.Name)
	if strings.Contains(name, "noncore") || strings.Contains(name, "non-core") {
		return "noncore"
	}
	return "core"
}

// avgSLAForPeriod computes the mean SLA ratio (0-1) over reports whose monitor
// is present in monitorIndex. The index parameter exists so the function cannot
// silently include monitors that should be excluded (e.g. by --exclude-name-pattern):
// summing over reports while dividing by len(reports) was the original bug shape,
// and trusting "the caller filters first" was the way that bug crept in.
//
// Returns (avg, true) when at least one report matched, or (0, false) when no
// report's UUID was in monitorIndex. The boolean lets the caller distinguish
// "average is 0" from "no input to average" — the difference matters for
// derived metrics like hyperping_tenant_health_score, which would otherwise
// emit a misleadingly low score for an empty-but-healthy fleet.
func avgSLAForPeriod(reports []hyperping.MonitorReport, monitorIndex map[string]hyperping.Monitor) (float64, bool) {
	sum := 0.0
	count := 0
	for _, r := range reports {
		if _, ok := monitorIndex[r.UUID]; !ok {
			continue
		}
		sum += r.SLA / 100.0
		count++
	}
	if count == 0 {
		return 0, false
	}
	return sum / float64(count), true
}

// computeHealthScore returns a composite 0–100 health score.
// Health score = (upRatio × 60) + (avgSLA × 40) − (activeOutageRatio × 30)
// Weights: 60% current status, 40% historical SLA, up to −30 penalty for active outages.
// Score is clamped to [0, 100].
func computeHealthScore(upRatio, avgSLA float64, activeOutages, totalMonitors int) float64 {
	base := upRatio*60.0 + avgSLA*40.0
	if totalMonitors > 0 {
		penalty := float64(activeOutages) / float64(totalMonitors) * 30.0
		base -= penalty
	}
	if base < 0 {
		return 0
	}
	if base > 100 {
		return 100
	}
	return base
}

func boolToFloat64(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// emitMcpMetrics sends global MCP-sourced metrics.
func (c *Collector) emitMcpMetrics(ch chan<- prometheus.Metric, snap collectorSnapshot) {
	ch <- prometheus.MustNewConstMetric(c.alertCount, prometheus.GaugeValue,
		float64(snap.totalAlerts))
}
