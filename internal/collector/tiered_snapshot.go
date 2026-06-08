// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package collector

import (
	"time"

	hyperping "github.com/develeap/hyperping-go"
)

// hotSnapshot is the per-tick result of the HOT tier refresh.
//
// HOT contains everything Collect needs to render the high-cardinality
// per-monitor up/down/outage/maintenance gauges, so a HOT-only snapshot is
// already useful for `/readyz` and for SLO dashboards that ignore SLA
// reports. activeOutages and activeMaintenance are pre-filtered to the
// records the HOT API call returned (status=ongoing for outages,
// Status=="ongoing" filtering for maintenance).
type hotSnapshot struct {
	monitors          []hyperping.Monitor
	healthchecks      []hyperping.Healthcheck
	incidents         []hyperping.Incident
	activeOutages     []hyperping.Outage
	activeMaintenance []hyperping.Maintenance
	excludedCount     int
	refreshedAt       time.Time
	scrapeDur         time.Duration
}

// warmSnapshot is the per-tick result of the WARM tier refresh.
//
// WARM carries the MCP-derived per-monitor metrics (response time, MTTA,
// anomalies, global alert count), the 24h SLA report, and the full
// maintenance list (not just active windows). Cold-start gap: WARM is
// empty for ~5 min after pod boot; Collect tolerates this by emitting
// zero entries for the affected metric families.
type warmSnapshot struct {
	responseTime map[string]float64
	mtta         map[string]float64
	anomalyCount map[string]int
	anomalyScore map[string]float64
	totalAlerts  int
	report24h    []hyperping.MonitorReport
	maintenance  []hyperping.Maintenance
	refreshedAt  time.Time
}

// coldSnapshot is the per-tick result of the COLD tier refresh.
//
// COLD carries the long-window SLA reports used for the per-monitor SLA
// gauges and for the tenant health score (which requires the 30d window).
// The reportsByPeriod map is keyed by period token ("7d"/"30d"/"90d"/
// "365d") and populated only for periods present in the refresher's
// configured set; absence means "this project did not opt into that
// window", not "fetch failed". A failed fetch for an opted-in period
// leaves that key absent and the WARM-style stale-carry semantics apply
// via the stitcher in buildCollectorSnapshot.
//
// mttaByPeriod is a per-period map of monitor UUID to the MCP-reported
// MTTA value for that window. Empty map (not nil) when the cold refresher
// published a snapshot but the MCP call returned no per-monitor entries;
// nil when the project has no cold-mapped period. MTTR is NOT stored
// here: emitReportMetrics reads r.MTTR off the per-period MonitorReport
// already fetched into reportsByPeriod, which avoids a redundant MCP
// per-period fetch.
//
// Cold-start gap: COLD is empty for ~15 min after pod boot, so
// hyperping_tenant_health_score is absent during that window. Documented in
// the chart CHANGELOG under "Behavior changes".
type coldSnapshot struct {
	// reportsByPeriod is the new multi-period storage. The legacy
	// report7d/report30d fields are preserved for backward compatibility
	// with existing tests; the stitcher prefers reportsByPeriod when
	// non-nil and falls back to the legacy fields otherwise.
	reportsByPeriod map[string][]hyperping.MonitorReport

	// Per-period MTTA map for cold-tier periods. Keys are monitor UUIDs;
	// outer map keys are period tokens ("7d"/"30d"/...). Absent map key
	// = period not configured or MCP fetch failed for that window;
	// absent inner key = monitor had no acknowledged alerts in the window.
	mttaByPeriod map[string]map[string]float64

	// Legacy fields retained so existing test constructors that build a
	// coldSnapshot{report7d: ..., report30d: ...} keep compiling. The
	// stitcher reads reportsByPeriod first; these fields are only read
	// when reportsByPeriod is nil.
	report7d    []hyperping.MonitorReport
	report30d   []hyperping.MonitorReport
	refreshedAt time.Time
}

// buildCollectorSnapshot stitches the three tier pointers into the same
// collectorSnapshot shape that the legacy Refresh() path produces, so the
// Collect() metric-emission logic is mode-agnostic.
//
// Nil pointers are handled per-tier: missing tier -> empty slices/maps for
// that tier's fields. lastSuccessTime and scrapeOK follow the HOT tier
// because HOT is the "is anything fresh?" tier in tiered mode; WARM/COLD
// staleness is observable separately via per-tier last-success counters.
//
// The function does no I/O and holds no locks; atomic.Pointer.Load() is
// the only synchronization primitive needed for a consistent per-tier read.
// The three Load() calls may interleave with concurrent Store() calls in
// other tiers, but each tier's data is independent so the cross-tier torn
// read is benign.
func (t *tieredRefresher) buildCollectorSnapshot() collectorSnapshot {
	snap := collectorSnapshot{
		outageIndex:       make(map[string]hyperping.Outage),
		monitorIndex:      make(map[string]hyperping.Monitor),
		reports:           make(map[string][]hyperping.MonitorReport),
		maintenanceIndex:  make(map[string]bool),
		regionDownIndex:   make(map[string]map[string]bool),
		responseTimeIndex: make(map[string]float64),
		mttaIndex:         make(map[string]float64),
		anomalyCountIndex: make(map[string]int),
		anomalyScoreIndex: make(map[string]float64),
		dataAges:          make(map[string]float64),
	}

	if h := t.hot.Load(); h != nil {
		snap.monitors = h.monitors
		snap.healthchecks = h.healthchecks
		snap.outageIndex = buildActiveOutageIndex(h.activeOutages)
		for _, m := range h.monitors {
			snap.monitorIndex[m.UUID] = m
		}
		// HOT carries pre-filtered active maintenance windows. Reuse the
		// shared coverage builder so a future change to ongoing-window
		// semantics flows through both modes.
		snap.maintenanceIndex, snap.activeMaintenanceCount = buildMaintenanceIndex(h.activeMaintenance, h.monitors)
		snap.regionDownIndex = buildRegionDownIndex(snap.outageIndex)
		snap.openIncidentCount = countOpenIncidents(h.incidents)
		snap.excludedCount = h.excludedCount
		snap.scrapeOK = true
		snap.scrapeDur = h.scrapeDur
		snap.lastSuccessTime = h.refreshedAt
		if !h.refreshedAt.IsZero() {
			snap.dataAges["hot"] = time.Since(h.refreshedAt).Seconds()
		}
	}

	if w := t.warm.Load(); w != nil {
		// Copy maps so downstream callers cannot mutate the warm snapshot
		// via Collect's snapshot reference. WARM maps are typically small
		// (<= number of monitors); the copy is negligible.
		for k, v := range w.responseTime {
			snap.responseTimeIndex[k] = v
		}
		for k, v := range w.mtta {
			snap.mttaIndex[k] = v
		}
		for k, v := range w.anomalyCount {
			snap.anomalyCountIndex[k] = v
		}
		for k, v := range w.anomalyScore {
			snap.anomalyScoreIndex[k] = v
		}
		snap.totalAlerts = w.totalAlerts
		if len(w.report24h) > 0 {
			snap.reports["24h"] = w.report24h
		}
		if !w.refreshedAt.IsZero() {
			snap.dataAges["warm"] = time.Since(w.refreshedAt).Seconds()
		}
	}

	if c := t.cold.Load(); c != nil {
		// Prefer the per-period map when populated; fall back to the
		// legacy report7d/report30d fields for test callers that
		// construct a coldSnapshot{} literal without the multi-period
		// field set. The fallback is also what older snapshots in a
		// rolling restart would carry; the dual-write in refreshCold
		// keeps both forms in sync after the next tick.
		if len(c.reportsByPeriod) > 0 {
			for period, rep := range c.reportsByPeriod {
				if len(rep) > 0 {
					snap.reports[period] = rep
				}
			}
		} else {
			if len(c.report7d) > 0 {
				snap.reports["7d"] = c.report7d
			}
			if len(c.report30d) > 0 {
				snap.reports["30d"] = c.report30d
			}
		}
		if len(c.mttaByPeriod) > 0 {
			if snap.mttaByPeriod == nil {
				snap.mttaByPeriod = make(map[string]map[string]float64, len(c.mttaByPeriod))
			}
			for period, m := range c.mttaByPeriod {
				snap.mttaByPeriod[period] = m
			}
		}
		if !c.refreshedAt.IsZero() {
			snap.dataAges["cold"] = time.Since(c.refreshedAt).Seconds()
		}
	}

	return snap
}
