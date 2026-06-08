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

	// Per-tier disable flags. Zero-value (false, false, false) means
	// "all enabled", which is the backward-compat default for callers
	// that construct tieredRefresher{} literals without populating these
	// fields (notably the existing collector tests). main.parseConfig
	// flips warmDisabled/coldDisabled to true for a project whose
	// cache: block sets the corresponding <tier>Enabled: false. A
	// disabled tier launches no goroutine in start, performs zero API
	// calls, and leaves its snapshot pointer nil. HOT must remain
	// enabled (enforced upstream at parse time); hotDisabled is carried
	// for symmetry but is always false in normal operation.
	hotDisabled  bool
	warmDisabled bool
	coldDisabled bool

	// periods is the per-collector list of configured SLA report windows.
	// The cold-tier refresh uses this to decide which long-window reports
	// to fetch and which per-period MTTA/MTTR calls to make. The warm
	// tier ignores periods today (warm always fetches the 24h slice in
	// the legacy shape); a future change could swap to a period-driven
	// warm fetch without affecting the tiered.go public surface.
	periods []string

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

// start launches one goroutine per enabled tier and blocks until ctx is
// cancelled. HOT runs an eager initial refresh in-band (so /readyz can flip
// on as soon as start returns the boot path to the caller) before its
// ticker is installed.
//
// v1.8.1: WARM and COLD also run an eager refresh, but inside their own
// goroutines so the caller is not blocked on the slow paths. Each tier's
// goroutine performs ONE refresh before entering the ticker loop, so
// freshly started exporters publish all three tiers in seconds rather
// than waiting up to warmTTL/coldTTL for the first tick. Per-tier
// mutexes (warmMu/coldMu) defend against the unlikely overlap between a
// slow eager refresh and the first scheduled tick.
//
// On ctx cancellation start returns after every tier goroutine exits its
// select loop. Each tier's refresh function holds a per-tier mutex, so an
// in-progress refresh completes before the goroutine returns. Ctx
// cancellation during an eager WARM/COLD refresh propagates into the
// underlying API/MCP calls so no goroutine leak results.
func (t *tieredRefresher) start(ctx context.Context) {
	// Eager initial HOT refresh: blocks until either HOT publishes a
	// snapshot or ctx is cancelled. This mirrors the legacy Collector.Start
	// behaviour (Refresh runs once before the ticker) so /readyz semantics
	// stay the same: ready latches as soon as the first HOT succeeds.
	if !t.hotDisabled {
		initCtx, cancelInit := context.WithTimeout(ctx, 30*time.Second)
		t.refreshHot(initCtx)
		cancelInit()
	}

	var wg sync.WaitGroup
	// Only launch a goroutine per enabled tier. A disabled tier issues
	// zero API calls and leaves its snapshot pointer at nil; the
	// stitcher in buildCollectorSnapshot already tolerates a nil
	// pointer so the read path needs no further change.
	if !t.hotDisabled {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(t.hotTTL)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					t.refreshHot(ctx)
				}
			}
		}()
	}

	if !t.warmDisabled {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Eager pre-ticker refresh so warm-tier data is available
			// soon after start, not after the first warmTTL elapses.
			t.refreshWarm(ctx)
			if ctx.Err() != nil {
				return
			}
			ticker := time.NewTicker(t.warmTTL)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					t.refreshWarm(ctx)
				}
			}
		}()
	}

	if !t.coldDisabled {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Eager pre-ticker refresh so cold-tier (long-window SLA)
			// data is available soon after start, not after the first
			// coldTTL elapses.
			t.refreshCold(ctx)
			if ctx.Err() != nil {
				return
			}
			ticker := time.NewTicker(t.coldTTL)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					t.refreshCold(ctx)
				}
			}
		}()
	}

	wg.Wait()
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
		periods:        c.periods,
		// Default *Disabled to zero (i.e. all tiers enabled). NewCollector
		// flips warm/cold *Disabled to true only when the Collector's
		// WithTierEnable option set the corresponding *Enabled flag to
		// false. Test callers that construct tieredRefresher{} literals
		// inherit the zero-value "all enabled" default from Go.
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

// refreshCold fetches the COLD-tier endpoints for every cold-mapped
// period configured on this refresher. The legacy 7d + 30d windows are
// always fetched when t.periods is empty (backward compat for test
// callers constructing tieredRefresher{} literals); otherwise the
// concrete cold-mapped subset of t.periods drives the fan-out.
//
// Per-window report errors leave that window's slice nil but the snapshot
// is still published with the survivors. Per-period MTTA/MTTR failures
// leave the corresponding map entry nil and degrade emission to skip the
// (uuid, period) series. When every report fetch failed the snapshot is
// not stored (previous snapshot kept) so /metrics continues serving stale
// cold-tier data.
func (t *tieredRefresher) refreshCold(ctx context.Context) {
	t.coldMu.Lock()
	defer t.coldMu.Unlock()

	now := time.Now().UTC()
	to := now

	// coldPeriods are the period tokens this refresher is responsible
	// for. Empty t.periods means a test caller did not configure
	// periods; fall back to the legacy 7d+30d set so existing tiered_test
	// expectations keep passing.
	coldPeriods := make([]string, 0, len(t.periods))
	if len(t.periods) == 0 {
		coldPeriods = append(coldPeriods, "7d", "30d")
	} else {
		for _, p := range t.periods {
			if PeriodTier(p) == "cold" {
				coldPeriods = append(coldPeriods, p)
			}
		}
	}

	var (
		mu              sync.Mutex
		wg              sync.WaitGroup
		reportsByPeriod = make(map[string][]hyperping.MonitorReport, len(coldPeriods))
		mttaByPeriod    = make(map[string]map[string]float64, len(coldPeriods))
		anyReportOK     bool
	)

	for _, period := range coldPeriods {
		dur, ok := reportDurations[period]
		if !ok {
			// Defensive: a period token validated by main is always in
			// the table, but the refresher constructor accepts any
			// slice. Skip unknown tokens rather than crashing.
			continue
		}
		from := now.Add(-dur).Format(time.RFC3339)
		toStr := to.Format(time.RFC3339)

		wg.Add(1)
		go func(p, fromStr, toStr string) {
			defer wg.Done()
			rep, err := t.api.ListMonitorReports(ctx, fromStr, toStr)
			if err != nil {
				t.logger.Warn("cold tier list reports failed", "tier", "cold", "period", p, "error", err)
				return
			}
			mu.Lock()
			reportsByPeriod[p] = rep
			anyReportOK = true
			mu.Unlock()
		}(period, from, toStr)

		// MTTA per period: one call per period that fans out across all
		// HOT-known monitor UUIDs.
		//
		// v1.8.1 fix: MCP get_monitor_mtta requires an explicit non-empty
		// monitor_uuids list to return per-monitor entries. An empty
		// uuids slice yields {monitors:[], totalAcknowledged:0, mtta:0}
		// (project-level aggregate only), which silently collapsed every
		// cold MTTA series in pre-1.8.1. UUIDs are sourced from the HOT
		// snapshot via currentMonitorUUIDs(); when HOT has not yet
		// published (cold-start race) the call is skipped and recovers
		// on the next cold tick.
		if t.mcp != nil {
			uuids := t.currentMonitorUUIDs()
			from := now.Add(-dur)
			toT := now

			if len(uuids) == 0 {
				t.logger.Debug("cold tier mcp mtta skipped: HOT snapshot has no monitors yet",
					"tier", "cold", "period", period)
			} else {
				wg.Add(1)
				go func(p string, uuidArg []string) {
					defer wg.Done()
					opCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
					defer cancel()
					resp, err := t.mcp.GetMonitorMtta(opCtx, from, toT, uuidArg...)
					if err != nil || resp == nil {
						if err != nil {
							t.logger.Warn("cold tier mcp mtta failed", "tier", "cold", "period", p, "error", err)
						}
						return
					}
					m := make(map[string]float64, len(resp.Monitors))
					for _, e := range resp.Monitors {
						m[e.UUID] = e.Mtta
					}
					mu.Lock()
					mttaByPeriod[p] = m
					mu.Unlock()
				}(period, uuids)
			}

			// Note: MTTR for cold-tier periods is sourced from the
			// per-period MonitorReport's r.MTTR field at emit time
			// (see emitReportMetrics). No separate MCP per-period MTTR
			// fetch is issued here: the reports endpoint already carries
			// MTTR per window and adding an MCP call per period would
			// cost +P MCP calls per cold tick against the rate-limit
			// budget for zero new data.
			_ = period
		}
	}
	wg.Wait()

	// If every report fetch failed we have no fresh report data; leave
	// the previous snapshot pointer in place so /metrics keeps serving
	// stale COLD data. MTTA/MTTR-only freshness is not enough to
	// publish a new snapshot because the per-monitor SLA gauges depend
	// on the report slice.
	if !anyReportOK {
		t.logger.Warn("cold tier produced no fresh report data; retaining stale snapshot", "tier", "cold")
		return
	}

	// Apply exclusion filter to every window. The COLD tier does not see
	// the HOT monitor list directly, so an "included uuids" set has to
	// come from the HOT snapshot if available; absence falls back to no
	// filtering (cold-start window).
	if t.excludePattern != nil {
		if hot := t.hot.Load(); hot != nil && len(hot.monitors) > 0 {
			includedUUIDs := make(map[string]struct{}, len(hot.monitors))
			for _, m := range hot.monitors {
				includedUUIDs[m.UUID] = struct{}{}
			}
			for p, rep := range reportsByPeriod {
				reportsByPeriod[p] = filterReportsByMonitorUUID(rep, includedUUIDs)
			}
			for p, m := range mttaByPeriod {
				mttaByPeriod[p] = filterUUIDMap(m, includedUUIDs)
			}
		}
	}

	// Populate legacy fields for backward compat with existing tests
	// that read coldSnapshot.report7d / report30d directly. The
	// stitcher prefers reportsByPeriod when non-nil so this is a
	// dual-write to keep both readers consistent.
	snap := &coldSnapshot{
		reportsByPeriod: reportsByPeriod,
		mttaByPeriod:    mttaByPeriod,
		report7d:        reportsByPeriod["7d"],
		report30d:       reportsByPeriod["30d"],
		refreshedAt:     time.Now(),
	}
	t.cold.Store(snap)
	t.coldLastSuccess.Store(snap.refreshedAt.Unix())

	t.logger.Info("cold tier refreshed",
		"tier", "cold",
		"periods", coldPeriods,
		"reports_total", len(reportsByPeriod),
	)
}

// currentMonitorUUIDs returns the monitor UUIDs from the most recently
// published HOT snapshot. Empty result means HOT has not run yet (cold-
// start race) or the project has zero monitors; callers must treat the
// empty case as "skip this fan-out and wait for the next tick".
//
// The accessor reads through a single atomic.Pointer.Load() and copies
// the slice so concurrent HOT publications cannot mutate the result the
// caller is iterating over.
func (t *tieredRefresher) currentMonitorUUIDs() []string {
	hot := t.hot.Load()
	if hot == nil || len(hot.monitors) == 0 {
		return nil
	}
	uuids := make([]string, 0, len(hot.monitors))
	for _, m := range hot.monitors {
		uuids = append(uuids, m.UUID)
	}
	return uuids
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
					//
					// v0.7.0 BREAKING: GetMonitorResponseTime now takes
					// (ctx, from, to, uuids...). Preserve WARM-tier
					// behaviour by passing a 24h window per call.
					{
						opCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
						now := time.Now().UTC()
						report, err := t.mcp.GetMonitorResponseTime(opCtx, now.Add(-24*time.Hour), now, uuid)
						cancel()
						if err == nil && report != nil {
							mu.Lock()
							res.responseTime[uuid] = report.AvgResponseTime
							mu.Unlock()
						} else if ctx.Err() != nil {
							return
						} else if err != nil {
							failures.Add(1)
							t.logger.Debug("mcp get_monitor_response_time failed", "tier", tier, "uuid", uuid, "error", err)
						}
					}

					// MTTA.
					//
					// v0.7.0 BREAKING: GetMonitorMtta now takes
					// (ctx, from, to, uuids...). Pre-v0.7.0 the old call
					// shape silently decoded into zero values; the WARM
					// snapshot has therefore been carrying mtta=0 for every
					// monitor. This commit restores real values.
					{
						opCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
						now := time.Now().UTC()
						report, err := t.mcp.GetMonitorMtta(opCtx, now.Add(-24*time.Hour), now, uuid)
						cancel()
						if err == nil && report != nil {
							mu.Lock()
							res.mtta[uuid] = report.Mtta
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
