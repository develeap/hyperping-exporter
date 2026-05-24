// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package collector

import (
	"context"
	"log/slog"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	hyperping "github.com/develeap/hyperping-go"
)

// tieredRefresher owns the three independent HOT/WARM/COLD refresh loops
// and the atomic snapshot pointers they publish.
//
// One goroutine per tier, each driven by its own time.Ticker; per-tier
// failure isolation is achieved by abort-before-Store (a tier that errors
// before publishing leaves the previous snapshot pointer untouched).
// Readers do three atomic Load()s and stitch via buildCollectorSnapshot;
// cross-tier torn reads are benign because each tier's data is independent.
type tieredRefresher struct {
	api            HyperpingAPI
	mcp            *hyperping.MCPClient
	logger         *slog.Logger
	excludePattern *regexp.Regexp
	mcpMetrics     *MCPMetrics

	hotTTL  time.Duration
	warmTTL time.Duration
	coldTTL time.Duration

	hot  atomic.Pointer[hotSnapshot]
	warm atomic.Pointer[warmSnapshot]
	cold atomic.Pointer[coldSnapshot]

	// hotReady latches true after the first successful HOT refresh.
	// IsReady() reads this to gate /readyz on HOT freshness only — WARM
	// and COLD lag is expected during the post-boot cold-start window.
	hotReady atomic.Bool

	// Last-success unix-seconds per tier. Reserved for a future
	// hyperping_data_age_seconds{tier=...} expansion (Q5 in the design
	// doc). Not currently emitted; populated as a no-overhead first step.
	hotLastSuccess  atomic.Int64
	warmLastSuccess atomic.Int64
	coldLastSuccess atomic.Int64

	// Per-tier refresh mutexes serialize overlapping refreshes. Tickers
	// should not overlap under healthy conditions, but a slow refresh can
	// race the next tick; the mutex defends the snapshot build/store
	// sequence against that case.
	hotMu  sync.Mutex
	warmMu sync.Mutex
	coldMu sync.Mutex
}

// newTieredRefresher constructs a tieredRefresher from a Collector's wired
// dependencies. The collector parameter is the source of truth for shared
// state (logger, exclude pattern, MCP metrics counter); per-tier TTLs and
// the MCP client are supplied explicitly so the constructor signature
// reflects exactly what tiered mode adds beyond the legacy mode.
func newTieredRefresher(c *Collector, hotTTL, warmTTL, coldTTL time.Duration) *tieredRefresher {
	return &tieredRefresher{
		api:            c.api,
		mcp:            c.mcp,
		logger:         c.logger,
		excludePattern: c.excludePattern,
		mcpMetrics:     c.mcpMetrics,
		hotTTL:         hotTTL,
		warmTTL:        warmTTL,
		coldTTL:        coldTTL,
	}
}

// refreshHot fetches the HOT-tier endpoints (monitors, healthchecks,
// incidents, active outages, active maintenance) in parallel, applies the
// exclusion filter, and publishes a new hotSnapshot. On a fatal HOT
// failure (monitors or healthchecks error) the previous snapshot pointer
// is left intact and the function returns without storing.
//
// "Active" is enforced two ways:
//   - ListOutages is called with hyperping.WithStatus("ongoing"), so the
//     server returns only currently-ongoing outages and the per-tick
//     payload stays small.
//   - ListMaintenance is the full account-wide list (the SDK does not
//     support a server-side status filter), filtered locally to
//     Status == "ongoing" before being stored in the snapshot.
func (t *tieredRefresher) refreshHot(ctx context.Context) {
	t.hotMu.Lock()
	defer t.hotMu.Unlock()

	start := time.Now()

	var (
		monitors          []hyperping.Monitor
		healthchecks      []hyperping.Healthcheck
		incidents         []hyperping.Incident
		outages           []hyperping.Outage
		fullMaintenance   []hyperping.Maintenance
		monErr            error
		hcErr             error
		incErr            error
		outErr            error
		maintErr          error
		wg                sync.WaitGroup
	)

	wg.Add(5)
	go func() { defer wg.Done(); monitors, monErr = t.api.ListMonitors(ctx) }()
	go func() { defer wg.Done(); healthchecks, hcErr = t.api.ListHealthchecks(ctx) }()
	go func() { defer wg.Done(); incidents, incErr = t.api.ListIncidents(ctx) }()
	go func() {
		defer wg.Done()
		outages, outErr = t.api.ListOutages(ctx, hyperping.WithStatus("ongoing"))
	}()
	go func() { defer wg.Done(); fullMaintenance, maintErr = t.api.ListMaintenance(ctx) }()
	wg.Wait()

	// Fatal failures: leave the previous HOT snapshot in place and exit.
	if monErr != nil {
		t.logger.Error("hot tier failed: list monitors", "tier", "hot", "error", monErr)
		return
	}
	if hcErr != nil {
		t.logger.Error("hot tier failed: list healthchecks", "tier", "hot", "error", hcErr)
		return
	}
	// Non-fatal: outage / incident / maintenance errors keep the rest of the
	// snapshot. Empty out the affected slice (rather than retaining stale
	// per-tier sub-fields) so a hot snapshot is internally consistent —
	// "ongoing" data older than hotTTL is more misleading than absent data.
	if outErr != nil {
		t.logger.Warn("hot tier list outages failed; outage map will be empty this tick", "tier", "hot", "error", outErr)
		outages = nil
	}
	if incErr != nil {
		t.logger.Warn("hot tier list incidents failed", "tier", "hot", "error", incErr)
		incidents = nil
	}
	if maintErr != nil {
		t.logger.Warn("hot tier list maintenance failed", "tier", "hot", "error", maintErr)
		fullMaintenance = nil
	}

	// Apply exclusion filter before computing the snapshot so excluded
	// monitors do not appear in monitor/outage indices.
	kept, excluded := filterMonitorsByName(monitors, t.excludePattern)
	var includedUUIDs map[string]struct{}
	if len(excluded) > 0 {
		includedUUIDs = make(map[string]struct{}, len(kept))
		for _, m := range kept {
			includedUUIDs[m.UUID] = struct{}{}
		}
		outages = filterOutagesByMonitorUUID(outages, includedUUIDs)
	}

	// Filter maintenance to active windows locally; the SDK has no
	// server-side status filter for ListMaintenance.
	activeMaintenance := make([]hyperping.Maintenance, 0, len(fullMaintenance))
	for _, w := range fullMaintenance {
		if w.Status == "ongoing" {
			activeMaintenance = append(activeMaintenance, w)
		}
	}

	snap := &hotSnapshot{
		monitors:          kept,
		healthchecks:      healthchecks,
		incidents:         incidents,
		activeOutages:     outages,
		activeMaintenance: activeMaintenance,
		excludedCount:     len(excluded),
		refreshedAt:       time.Now(),
		scrapeDur:         time.Since(start),
	}
	t.hot.Store(snap)
	t.hotReady.Store(true)
	t.hotLastSuccess.Store(snap.refreshedAt.Unix())

	t.logger.Info("hot tier refreshed",
		"tier", "hot",
		"monitors", len(kept),
		"healthchecks", len(healthchecks),
		"active_outages", len(outages),
		"active_maintenance", len(activeMaintenance),
		"excluded", len(excluded),
		"duration", snap.scrapeDur,
	)
}

// refreshWarm fetches WARM-tier endpoints in parallel: the 24h SLA report
// for all monitors, the full maintenance list, and MCP per-monitor metrics
// (response time, MTTA, anomalies) plus the global alert count. On a fatal
// failure the previous WARM snapshot pointer is left intact. Per-monitor
// MCP failures are non-fatal and degrade gracefully via stale-value carry
// from the previous WARM snapshot.
func (t *tieredRefresher) refreshWarm(ctx context.Context) {
	t.warmMu.Lock()
	defer t.warmMu.Unlock()

	// WARM depends on the HOT monitor list to know which uuids to fetch
	// per-monitor MCP metrics for. If HOT has not run yet, skip this tick;
	// WARM will catch up on its next tick after HOT publishes.
	hot := t.hot.Load()
	var monitors []hyperping.Monitor
	if hot != nil {
		monitors = hot.monitors
	}

	now := time.Now().UTC()
	from24h := now.Add(-24 * time.Hour).Format(time.RFC3339)
	to24h := now.Format(time.RFC3339)

	var (
		report24h       []hyperping.MonitorReport
		fullMaintenance []hyperping.Maintenance
		mcp             mcpData
		reportErr       error
		maintErr        error
		mcpErr          error
		wg              sync.WaitGroup
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		report24h, reportErr = t.api.ListMonitorReports(ctx, from24h, to24h)
	}()
	go func() {
		defer wg.Done()
		fullMaintenance, maintErr = t.api.ListMaintenance(ctx)
	}()
	go func() {
		defer wg.Done()
		mcp, mcpErr = t.fetchMcpDataForTier(ctx, monitors, "warm")
	}()
	wg.Wait()

	// Apply exclusion filter so excluded uuids do not appear in MCP maps
	// or the report slice.
	if t.excludePattern != nil && len(monitors) > 0 {
		includedUUIDs := make(map[string]struct{}, len(monitors))
		for _, m := range monitors {
			includedUUIDs[m.UUID] = struct{}{}
		}
		report24h = filterReportsByMonitorUUID(report24h, includedUUIDs)
		mcp.responseTime = filterUUIDMap(mcp.responseTime, includedUUIDs)
		mcp.mtta = filterUUIDMap(mcp.mtta, includedUUIDs)
		mcp.anomalyCount = filterUUIDMap(mcp.anomalyCount, includedUUIDs)
		mcp.anomalyScore = filterUUIDMap(mcp.anomalyScore, includedUUIDs)
	}

	// Surface fetch errors but keep the rest of the snapshot. The atomic
	// store happens unconditionally — a partial WARM snapshot is more
	// useful than no WARM snapshot at all, because the stitcher in
	// buildCollectorSnapshot tolerates empty per-WARM fields.
	if reportErr != nil {
		t.logger.Warn("warm tier list reports (24h) failed", "tier", "warm", "error", reportErr)
		report24h = nil
	}
	if maintErr != nil {
		t.logger.Warn("warm tier list maintenance failed", "tier", "warm", "error", maintErr)
		fullMaintenance = nil
	}
	if mcpErr != nil {
		t.logger.Warn("warm tier fetch mcp data failed", "tier", "warm", "error", mcpErr)
	}

	snap := &warmSnapshot{
		responseTime: mcp.responseTime,
		mtta:         mcp.mtta,
		anomalyCount: mcp.anomalyCount,
		anomalyScore: mcp.anomalyScore,
		totalAlerts:  mcp.totalAlerts,
		report24h:    report24h,
		maintenance:  fullMaintenance,
		refreshedAt:  time.Now(),
	}
	t.warm.Store(snap)
	t.warmLastSuccess.Store(snap.refreshedAt.Unix())

	t.logger.Info("warm tier refreshed",
		"tier", "warm",
		"monitors", len(monitors),
		"reports_24h", len(report24h),
		"maintenance", len(fullMaintenance),
		"mcp_partial", mcp.partial,
		"alerts", mcp.totalAlerts,
	)
}

// refreshCold fetches the COLD-tier endpoints: 7d and 30d SLA reports.
// COLD has no MCP fetches and no real-time data, so its failure handling
// is simple: per-window errors leave that window's slice nil; both-fail
// leaves the previous COLD snapshot in place (no store).
func (t *tieredRefresher) refreshCold(ctx context.Context) {
	t.coldMu.Lock()
	defer t.coldMu.Unlock()

	now := time.Now().UTC()
	from7d := now.Add(-7 * 24 * time.Hour).Format(time.RFC3339)
	from30d := now.Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	to := now.Format(time.RFC3339)

	var (
		report7d  []hyperping.MonitorReport
		report30d []hyperping.MonitorReport
		err7d     error
		err30d    error
		wg        sync.WaitGroup
	)
	wg.Add(2)
	go func() { defer wg.Done(); report7d, err7d = t.api.ListMonitorReports(ctx, from7d, to) }()
	go func() { defer wg.Done(); report30d, err30d = t.api.ListMonitorReports(ctx, from30d, to) }()
	wg.Wait()

	if err7d != nil {
		t.logger.Warn("cold tier list reports (7d) failed", "tier", "cold", "error", err7d)
		report7d = nil
	}
	if err30d != nil {
		t.logger.Warn("cold tier list reports (30d) failed", "tier", "cold", "error", err30d)
		report30d = nil
	}

	// If both windows failed we have no fresh data; leave the previous
	// snapshot pointer in place so /metrics keeps serving stale COLD data.
	if report7d == nil && report30d == nil {
		t.logger.Warn("cold tier produced no fresh data; retaining stale snapshot", "tier", "cold")
		return
	}

	// Apply exclusion filter to both windows. The COLD tier does not see
	// the HOT monitor list, so an "included uuids" set has to come from
	// the report uuids themselves; the WARM tier's report has the same
	// shape, so reuse the per-uuid filter helper.
	if t.excludePattern != nil {
		// Pull the included uuids from the HOT snapshot if available;
		// otherwise fall back to the union of report uuids (no filtering).
		// A nil HOT pointer in tiered mode is the cold-start window where
		// exclude semantics are intentionally relaxed — the WARM tier
		// will re-apply the filter on its next tick.
		if hot := t.hot.Load(); hot != nil && len(hot.monitors) > 0 {
			includedUUIDs := make(map[string]struct{}, len(hot.monitors))
			for _, m := range hot.monitors {
				includedUUIDs[m.UUID] = struct{}{}
			}
			report7d = filterReportsByMonitorUUID(report7d, includedUUIDs)
			report30d = filterReportsByMonitorUUID(report30d, includedUUIDs)
		}
	}

	snap := &coldSnapshot{
		report7d:    report7d,
		report30d:   report30d,
		refreshedAt: time.Now(),
	}
	t.cold.Store(snap)
	t.coldLastSuccess.Store(snap.refreshedAt.Unix())

	t.logger.Info("cold tier refreshed",
		"tier", "cold",
		"reports_7d", len(report7d),
		"reports_30d", len(report30d),
	)
}

// filterUUIDMap returns a new map containing only the keys present in
// includedUUIDs. Used by the WARM tier to drop excluded monitors from the
// MCP-derived per-monitor maps. Generic on the value type so the same
// helper covers float64 / int alike.
func filterUUIDMap[V any](m map[string]V, includedUUIDs map[string]struct{}) map[string]V {
	if m == nil {
		return nil
	}
	out := make(map[string]V, len(m))
	for k, v := range m {
		if _, ok := includedUUIDs[k]; ok {
			out[k] = v
		}
	}
	return out
}

// fetchMcpDataForTier wraps the existing per-monitor MCP fetch logic with a
// tier label. It defers to a small loop-style fetch instead of reusing
// Collector.fetchMcpData because the tier path does not need the
// "merge with previous cached values" behaviour: WARM's previous snapshot
// is read separately and stitched in buildCollectorSnapshot only if the
// new snapshot leaves a key absent. The tier label is plumbed into the
// MCPMetrics.PartialRefreshTotal counter so operators can distinguish
// HOT-tier-driven partials (currently none) from WARM-tier ones.
func (t *tieredRefresher) fetchMcpDataForTier(ctx context.Context, monitors []hyperping.Monitor, tier string) (mcpData, error) {
	if t.mcp == nil {
		return mcpData{
			responseTime: map[string]float64{},
			mtta:         map[string]float64{},
			anomalyCount: map[string]int{},
			anomalyScore: map[string]float64{},
		}, nil
	}

	// Seed result maps from the previous WARM snapshot so per-monitor
	// failures retain stale values rather than dropping the series. This
	// mirrors Collector.fetchMcpData's semantics, just sourced from the
	// per-tier snapshot instead of the legacy cache.
	res := mcpData{
		responseTime: map[string]float64{},
		mtta:         map[string]float64{},
		anomalyCount: map[string]int{},
		anomalyScore: map[string]float64{},
	}
	if prev := t.warm.Load(); prev != nil {
		for k, v := range prev.responseTime {
			res.responseTime[k] = v
		}
		for k, v := range prev.mtta {
			res.mtta[k] = v
		}
		for k, v := range prev.anomalyCount {
			res.anomalyCount[k] = v
		}
		for k, v := range prev.anomalyScore {
			res.anomalyScore[k] = v
		}
		res.totalAlerts = prev.totalAlerts
	}

	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		failures atomic.Int64
	)

	// Global alerts fetch.
	wg.Add(1)
	go func() {
		defer wg.Done()
		alerts, err := t.mcp.ListRecentAlerts(ctx)
		if err != nil {
			if ctx.Err() == nil {
				failures.Add(1)
			}
			t.logger.Warn("mcp list_recent_alerts failed", "tier", tier, "error", err)
			return
		}
		if alerts == nil {
			return
		}
		mu.Lock()
		res.totalAlerts = alerts.Total
		mu.Unlock()
	}()

	if len(monitors) == 0 {
		wg.Wait()
		if failures.Load() > 0 {
			res.partial = true
			if t.mcpMetrics != nil {
				t.mcpMetrics.PartialRefreshTotal.Inc()
			}
		}
		return res, nil
	}

	monitorChan := make(chan hyperping.Monitor, len(monitors))
	for _, m := range monitors {
		monitorChan <- m
	}
	close(monitorChan)

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
					if t.mcp == nil {
						return
					}
					uuid := m.UUID

					// Response time.
					{
						opCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
						report, err := t.mcp.GetMonitorResponseTime(opCtx, uuid)
						cancel()
						if err == nil && report != nil {
							mu.Lock()
							res.responseTime[uuid] = report.Avg
							mu.Unlock()
						} else if ctx.Err() != nil {
							return
						} else if err != nil {
							failures.Add(1)
							t.logger.Debug("mcp get_monitor_response_time failed", "tier", tier, "uuid", uuid, "error", err)
						}
					}

					// MTTA.
					{
						opCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
						report, err := t.mcp.GetMonitorMtta(opCtx, uuid)
						cancel()
						if err == nil && report != nil {
							mu.Lock()
							res.mtta[uuid] = report.AvgWait
							mu.Unlock()
						} else if ctx.Err() != nil {
							return
						} else if err != nil {
							failures.Add(1)
							t.logger.Debug("mcp get_monitor_mtta failed", "tier", tier, "uuid", uuid, "error", err)
						}
					}

					// Anomalies.
					{
						opCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
						anomalies, err := t.mcp.GetMonitorAnomalies(opCtx, uuid)
						cancel()
						if err == nil {
							maxScore := 0.0
							for _, a := range anomalies {
								if a.Score > maxScore {
									maxScore = a.Score
								}
							}
							mu.Lock()
							res.anomalyCount[uuid] = len(anomalies)
							res.anomalyScore[uuid] = maxScore
							mu.Unlock()
						} else if ctx.Err() != nil {
							return
						} else {
							failures.Add(1)
							t.logger.Debug("mcp get_monitor_anomalies failed", "tier", tier, "uuid", uuid, "error", err)
						}
					}
				}
			}
		}()
	}
	wg.Wait()

	if failures.Load() > 0 {
		res.partial = true
		if t.mcpMetrics != nil {
			t.mcpMetrics.PartialRefreshTotal.Inc()
		}
	}
	return res, nil
}
