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
// COLD carries the 7d and 30d SLA reports used for the long-window per-monitor
// SLA gauges and for the tenant health score (which requires the 30d window).
// Cold-start gap: COLD is empty for ~15 min after pod boot, so
// hyperping_tenant_health_score is absent during that window. Documented in
// the chart CHANGELOG under "Behavior changes".
type coldSnapshot struct {
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
			snap.dataAge = time.Since(h.refreshedAt).Seconds()
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
	}

	if c := t.cold.Load(); c != nil {
		if len(c.report7d) > 0 {
			snap.reports["7d"] = c.report7d
		}
		if len(c.report30d) > 0 {
			snap.reports["30d"] = c.report30d
		}
	}

	return snap
}
