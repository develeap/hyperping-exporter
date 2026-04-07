// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package collector

import (
	"context"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	hyperping "github.com/develeap/hyperping-go"
)

// reportPeriods defines the SLA/outage report windows fetched on each refresh.
var reportPeriods = []string{"24h", "7d", "30d"}

// reportDurations maps period labels to their lookback durations.
var reportDurations = map[string]time.Duration{
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
}

// HyperpingAPI defines the Hyperping API methods used by the collector.
type HyperpingAPI interface {
	ListMonitors(ctx context.Context) ([]hyperping.Monitor, error)
	ListHealthchecks(ctx context.Context) ([]hyperping.Healthcheck, error)
	ListOutages(ctx context.Context) ([]hyperping.Outage, error)
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
}

// newCollectorDescs initialises all Prometheus metric descriptors.
func newCollectorDescs(ns string) collectorDescs {
	fqn := prometheus.BuildFQName
	ml := []string{"uuid", "name", "tenant", "tier"}
	mpl := []string{"uuid", "name", "tenant", "tier", "period"}
	return collectorDescs{
		monitorUp:                 prometheus.NewDesc(fqn(ns, "monitor", "up"), "Whether the monitor is up (1) or down (0).", ml, nil),
		monitorPaused:             prometheus.NewDesc(fqn(ns, "monitor", "paused"), "Whether the monitor is paused (1) or active (0).", ml, nil),
		monitorSSLExpDays:         prometheus.NewDesc(fqn(ns, "monitor", "ssl_expiration_days"), "Days until SSL certificate expiration.", ml, nil),
		monitorCheckInterval:      prometheus.NewDesc(fqn(ns, "monitor", "check_interval_seconds"), "Monitor check frequency in seconds.", ml, nil),
		monitorInfo:               prometheus.NewDesc(fqn(ns, "monitor", "info"), "Monitor metadata (value is always 1).", []string{"uuid", "name", "protocol", "url", "project_uuid", "http_method"}, nil),
		healthcheckUp:             prometheus.NewDesc(fqn(ns, "healthcheck", "up"), "Whether the healthcheck is up (1) or down (0).", []string{"uuid", "name"}, nil),
		healthcheckPaused:         prometheus.NewDesc(fqn(ns, "healthcheck", "paused"), "Whether the healthcheck is paused (1) or active (0).", []string{"uuid", "name"}, nil),
		healthcheckPeriod:         prometheus.NewDesc(fqn(ns, "healthcheck", "period_seconds"), "Expected healthcheck ping period in seconds.", []string{"uuid", "name"}, nil),
		monitorsTotal:             prometheus.NewDesc(fqn(ns, "", "monitors"), "Total number of monitors.", nil, nil),
		healthchecksTotal:         prometheus.NewDesc(fqn(ns, "", "healthchecks"), "Total number of healthchecks.", nil, nil),
		scrapeDurationDesc:        prometheus.NewDesc(fqn(ns, "scrape", "duration_seconds"), "Duration of the last API scrape in seconds.", nil, nil),
		scrapeSuccessDesc:         prometheus.NewDesc(fqn(ns, "scrape", "success"), "Whether the last API scrape succeeded (1) or failed (0).", nil, nil),
		dataAgeDesc:               prometheus.NewDesc(fqn(ns, "data", "age_seconds"), "Seconds elapsed since the last successful API cache refresh.", nil, nil),
		monitorOutageActive:       prometheus.NewDesc(fqn(ns, "monitor", "outage_active"), "Whether the monitor has an active (unresolved) outage (1) or not (0).", ml, nil),
		monitorActiveOutageStatus: prometheus.NewDesc(fqn(ns, "monitor", "active_outage_status_code"), "HTTP status code of the current active outage; 0 when no active outage.", ml, nil),
		monitorSLA:                prometheus.NewDesc(fqn(ns, "monitor", "sla_ratio"), "Monitor SLA as a ratio (0–1) over the labelled period.", mpl, nil),
		monitorOutages:            prometheus.NewDesc(fqn(ns, "monitor", "outages"), "Number of outages over the labelled period.", mpl, nil),
		monitorDowntime:           prometheus.NewDesc(fqn(ns, "monitor", "downtime_seconds"), "Total downtime in seconds over the labelled period.", mpl, nil),
		monitorLongestOutage:      prometheus.NewDesc(fqn(ns, "monitor", "longest_outage_seconds"), "Duration of the longest single outage in seconds over the labelled period.", mpl, nil),
		monitorMTTR:               prometheus.NewDesc(fqn(ns, "monitor", "mttr_seconds"), "Mean Time To Recovery in seconds over the labelled period.", mpl, nil),
		tenantHealthScore:         prometheus.NewDesc(fqn(ns, "tenant", "health_score"), "Composite tenant health score from 0 to 100.", nil, nil),
		tenantUpRatio:             prometheus.NewDesc(fqn(ns, "tenant", "monitors_up_ratio"), "Fraction of monitors currently up (0–1).", nil, nil),
		tenantActiveOutages:       prometheus.NewDesc(fqn(ns, "tenant", "active_outages"), "Total number of active (unresolved) outages across all monitors.", nil, nil),
		tenantAvgSLA:              prometheus.NewDesc(fqn(ns, "tenant", "avg_sla_ratio"), "Average SLA ratio across all monitors for the labelled period.", []string{"period"}, nil),
		monitorTier:               prometheus.NewDesc(fqn(ns, "monitor", "escalation_tier"), "Escalation tier info (always 1). Join on uuid+name; use tier label to filter core/noncore.", []string{"uuid", "name", "tier"}, nil),
		monitorInMaintenance: prometheus.NewDesc(
			fqn(ns, "monitor", "in_maintenance"),
			"1 if the monitor is currently covered by an active maintenance window, 0 otherwise.",
			[]string{"uuid", "name", "tenant", "tier"}, nil,
		),
		monitorUpByRegion: prometheus.NewDesc(
			fqn(ns, "monitor", "up_by_region"),
			"1 if the monitor is up in the given region, 0 if confirmed down. "+
				"Derived from active outage confirmed locations; approximation only.",
			[]string{"uuid", "name", "tenant", "tier", "region"}, nil,
		),
		incidentsOpen: prometheus.NewDesc(
			fqn(ns, "", "incidents_open"),
			"Number of open (non-resolved) incidents.",
			nil, nil,
		),
		maintenanceWindowsActive: prometheus.NewDesc(
			fqn(ns, "", "maintenance_windows_active"),
			"Number of currently active (ongoing) maintenance windows.",
			nil, nil,
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
	dataAge                float64
	maintenanceIndex       map[string]bool              // monitor uuid -> covered by active window
	regionDownIndex        map[string]map[string]bool   // uuid -> region -> is_down
	openIncidentCount      int
	activeMaintenanceCount int
}

// Collector fetches Hyperping data on a background timer and serves
// cached results as Prometheus metrics. It implements prometheus.Collector.
type Collector struct {
	api      HyperpingAPI
	cacheTTL time.Duration
	logger   *slog.Logger

	// Cache (protected by mu).
	mu                 sync.RWMutex
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

	collectorDescs
}

// Verify Collector implements prometheus.Collector at compile time.
var _ prometheus.Collector = (*Collector)(nil)

// NewCollector creates a new Hyperping metrics collector.
func NewCollector(api HyperpingAPI, cacheTTL time.Duration, logger *slog.Logger, namespace string) *Collector {
	return &Collector{
		api:             api,
		cacheTTL:        cacheTTL,
		logger:          logger,
		reportsByPeriod: make(map[string][]hyperping.MonitorReport),
		collectorDescs:  newCollectorDescs(namespace),
	}
}

// Start begins the background cache refresh loop. It blocks until ctx is cancelled.
func (c *Collector) Start(ctx context.Context) {
	c.Refresh(ctx)

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
func (c *Collector) fetchReports(ctx context.Context, now time.Time) map[string][]hyperping.MonitorReport {
	results := make(map[string][]hyperping.MonitorReport, len(reportPeriods))
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for _, p := range reportPeriods {
		dur := reportDurations[p]
		from := now.Add(-dur).Format(time.RFC3339)
		to := now.Format(time.RFC3339)
		wg.Add(1)
		go func(period, fromStr, toStr string) {
			defer wg.Done()
			reports, err := c.api.ListMonitorReports(ctx, fromStr, toStr)
			if err != nil {
				c.logger.Warn("failed to fetch monitor reports", "period", period, "error", err)
				return
			}
			mu.Lock()
			results[period] = reports
			mu.Unlock()
		}(p, from, to)
	}
	wg.Wait()
	return results
}

// Refresh performs a single API scrape and updates the cache.
// Monitors and healthchecks are required; outages and reports are best-effort.
func (c *Collector) Refresh(ctx context.Context) {
	start := time.Now()

	core, err := c.fetchCoreData(ctx)
	reportResults := map[string][]hyperping.MonitorReport{}
	if err == nil {
		reportResults = c.fetchReports(ctx, start.UTC())
	}
	dur := time.Since(start)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.lastScrapeDur = dur

	if err != nil {
		c.lastScrapeOK = false
		return
	}

	if core.outages != nil {
		c.outages = core.outages
	}
	if core.maintenance != nil {
		c.maintenanceWindows = core.maintenance
	}
	if core.incidents != nil {
		c.incidents = core.incidents
	}
	c.monitors = core.monitors
	c.healthchecks = core.healthchecks
	// Merge successful period results; periods that failed retain previous data.
	for period, reports := range reportResults {
		c.reportsByPeriod[period] = reports
	}
	c.lastScrapeOK = true
	c.everSucceeded = true
	c.lastSuccessTime = time.Now()

	c.logger.Info("cache refreshed",
		"monitors", len(core.monitors),
		"healthchecks", len(core.healthchecks),
		"outages", len(c.outages),
		"maintenance_windows", len(c.maintenanceWindows),
		"incidents", len(c.incidents),
		"report_periods", len(reportResults),
		"duration", dur,
	)
}

// IsReady returns true once at least one successful API scrape has completed.
// It never reverts to false: transient failures after the first success do not
// affect readiness — staleness is surfaced by hyperping_data_age_seconds instead.
func (c *Collector) IsReady() bool {
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
}

// Collect implements prometheus.Collector.
// Cached slices are copied under a minimal read lock; all index building and
// metric emission happen outside the lock to avoid blocking concurrent Refresh calls.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
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
	c.mu.RUnlock()

	// STEP 2: Build all derived indices outside the lock.
	var dataAge float64
	if !lastSuccess.IsZero() {
		dataAge = time.Since(lastSuccess).Seconds()
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
		dataAge:                dataAge,
		maintenanceIndex:       maintenanceIdx,
		regionDownIndex:        buildRegionDownIndex(outageIdx),
		openIncidentCount:      countOpenIncidents(incidents),
		activeMaintenanceCount: activeMaintenanceCount,
	}

	c.emitMonitorMetrics(ch, snap)
	c.emitHealthcheckMetrics(ch, snap)
	c.emitReportMetrics(ch, snap)
	c.emitTenantMetrics(ch, snap)
}

// emitMonitorMetrics sends per-monitor metrics derived from the snapshot.
func (c *Collector) emitMonitorMetrics(ch chan<- prometheus.Metric, snap collectorSnapshot) {
	for _, m := range snap.monitors {
		tenant := extractTenant(m.Name)
		tier := escalationTier(m)
		ch <- prometheus.MustNewConstMetric(c.monitorUp, prometheus.GaugeValue,
			boolToFloat64(m.Status == "up"), m.UUID, m.Name, tenant, tier)
		ch <- prometheus.MustNewConstMetric(c.monitorPaused, prometheus.GaugeValue,
			boolToFloat64(m.Paused), m.UUID, m.Name, tenant, tier)
		ch <- prometheus.MustNewConstMetric(c.monitorCheckInterval, prometheus.GaugeValue,
			float64(m.CheckFrequency), m.UUID, m.Name, tenant, tier)
		// M2: strip query params to prevent label cardinality explosion.
		ch <- prometheus.MustNewConstMetric(c.monitorInfo, prometheus.GaugeValue, 1,
			m.UUID, m.Name, m.Protocol, sanitizeURL(m.URL), m.ProjectUUID, m.HTTPMethod)

		if m.SSLExpiration != nil {
			ch <- prometheus.MustNewConstMetric(c.monitorSSLExpDays, prometheus.GaugeValue,
				float64(*m.SSLExpiration), m.UUID, m.Name, tenant, tier)
		}

		// OPS-32: active outage state and HTTP status code.
		activeOutage, hasActive := snap.outageIndex[m.UUID]
		ch <- prometheus.MustNewConstMetric(c.monitorOutageActive, prometheus.GaugeValue,
			boolToFloat64(hasActive), m.UUID, m.Name, tenant, tier)
		statusCode := 0
		if hasActive {
			statusCode = activeOutage.StatusCode
		}
		ch <- prometheus.MustNewConstMetric(c.monitorActiveOutageStatus, prometheus.GaugeValue,
			float64(statusCode), m.UUID, m.Name, tenant, tier)

		// OPS-39: escalation tier info.
		ch <- prometheus.MustNewConstMetric(c.monitorTier, prometheus.GaugeValue, 1,
			m.UUID, m.Name, tier)

		// EXP-02: maintenance window coverage.
		inMaint := 0.0
		if snap.maintenanceIndex[m.UUID] {
			inMaint = 1.0
		}
		ch <- prometheus.MustNewConstMetric(c.monitorInMaintenance, prometheus.GaugeValue,
			inMaint, m.UUID, m.Name, tenant, tier)

		// EXP-03: per-region up/down status.
		if len(m.Regions) > 0 {
			downRegions := snap.regionDownIndex[m.UUID]
			for _, region := range m.Regions {
				val := 1.0
				if downRegions[region] {
					val = 0.0
				}
				ch <- prometheus.MustNewConstMetric(c.monitorUpByRegion, prometheus.GaugeValue,
					val, m.UUID, m.Name, tenant, tier, region)
			}
		}
	}
}

// emitHealthcheckMetrics sends per-healthcheck metrics derived from the snapshot.
func (c *Collector) emitHealthcheckMetrics(ch chan<- prometheus.Metric, snap collectorSnapshot) {
	for _, hc := range snap.healthchecks {
		ch <- prometheus.MustNewConstMetric(c.healthcheckUp, prometheus.GaugeValue,
			boolToFloat64(!hc.IsDown), hc.UUID, hc.Name)
		ch <- prometheus.MustNewConstMetric(c.healthcheckPaused, prometheus.GaugeValue,
			boolToFloat64(hc.IsPaused), hc.UUID, hc.Name)
		ch <- prometheus.MustNewConstMetric(c.healthcheckPeriod, prometheus.GaugeValue,
			float64(hc.Period), hc.UUID, hc.Name)
	}
}

// emitReportMetrics sends per-monitor SLA/outage report metrics and per-period tenant averages.
func (c *Collector) emitReportMetrics(ch chan<- prometheus.Metric, snap collectorSnapshot) {
	for _, period := range reportPeriods {
		reports := snap.reports[period]
		slaSum := 0.0
		for _, r := range reports {
			mon := snap.monitorIndex[r.UUID]
			tenant := extractTenant(mon.Name)
			tier := escalationTier(mon)
			sla := r.SLA / 100.0 // API returns 0–100; expose as 0–1
			ch <- prometheus.MustNewConstMetric(c.monitorSLA, prometheus.GaugeValue,
				sla, r.UUID, r.Name, tenant, tier, period)
			ch <- prometheus.MustNewConstMetric(c.monitorOutages, prometheus.GaugeValue,
				float64(r.Outages.Count), r.UUID, r.Name, tenant, tier, period)
			ch <- prometheus.MustNewConstMetric(c.monitorDowntime, prometheus.GaugeValue,
				float64(r.Outages.TotalDowntime), r.UUID, r.Name, tenant, tier, period)
			ch <- prometheus.MustNewConstMetric(c.monitorLongestOutage, prometheus.GaugeValue,
				float64(r.Outages.LongestOutage), r.UUID, r.Name, tenant, tier, period)
			if r.MTTR > 0 {
				ch <- prometheus.MustNewConstMetric(c.monitorMTTR, prometheus.GaugeValue,
					float64(r.MTTR), r.UUID, r.Name, tenant, tier, period)
			}
			slaSum += sla
		}
		if len(reports) > 0 {
			ch <- prometheus.MustNewConstMetric(c.tenantAvgSLA, prometheus.GaugeValue,
				slaSum/float64(len(reports)), period)
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

	// OPS-31: data age — only after at least one successful scrape.
	if snap.dataAge > 0 {
		ch <- prometheus.MustNewConstMetric(c.dataAgeDesc, prometheus.GaugeValue, snap.dataAge)
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
		avg30dSLA := avgSLAForPeriod(reports30d)
		ch <- prometheus.MustNewConstMetric(c.tenantHealthScore, prometheus.GaugeValue,
			computeHealthScore(upRatio, avg30dSLA, len(snap.outageIndex), len(snap.monitors)))
	}

	// EXP-04: open incidents and active maintenance windows.
	ch <- prometheus.MustNewConstMetric(c.incidentsOpen, prometheus.GaugeValue,
		float64(snap.openIncidentCount))
	ch <- prometheus.MustNewConstMetric(c.maintenanceWindowsActive, prometheus.GaugeValue,
		float64(snap.activeMaintenanceCount))
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

// extractTenant derives the tenant ID from monitor name convention "[TENANT-ID]-SuffixName".
// Returns empty string for monitors whose name does not start with "[".
func extractTenant(monitorName string) string {
	if !strings.HasPrefix(monitorName, "[") {
		return ""
	}
	end := strings.Index(monitorName, "]")
	if end < 0 {
		return ""
	}
	return monitorName[1:end]
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

// avgSLAForPeriod computes the mean SLA ratio (0-1) across a set of reports.
func avgSLAForPeriod(reports []hyperping.MonitorReport) float64 {
	if len(reports) == 0 {
		return 0
	}
	sum := 0.0
	for _, r := range reports {
		sum += r.SLA / 100.0
	}
	return sum / float64(len(reports))
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
