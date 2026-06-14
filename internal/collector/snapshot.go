// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package collector

import (
	"time"

	hyperping "github.com/develeap/hyperping-go"
)

// Snapshot is a public, read-only view of a Collector's cached state.
// It is intended for consumers outside the collector package (e.g. the OTLP
// push bridge in internal/otelpush). The struct is a shallow copy of the
// internal collectorSnapshot: map values are shared and must not be mutated
// by the caller.
type Snapshot struct {
	// Monitors is the last-refreshed list of visible monitors (after the
	// exclude-name-pattern filter is applied).
	Monitors []hyperping.Monitor
	// OutageIndex maps monitor UUID to its active (unresolved) outage. Absent
	// key means no active outage for that monitor.
	OutageIndex map[string]hyperping.Outage
	// MaintenanceIndex maps monitor UUID to true when the monitor is covered
	// by at least one ongoing maintenance window.
	MaintenanceIndex map[string]bool
	// Reports maps period token ("24h", "7d", "30d", ...) to the slice of
	// MonitorReport objects for that window. Absent key means no data for
	// that window has been fetched yet.
	Reports map[string][]hyperping.MonitorReport
	// ResponseTimeIndex maps monitor UUID to its average response time in
	// seconds sourced from the MCP warm tier.
	ResponseTimeIndex map[string]float64
	// MttaIndex maps monitor UUID to its MTTA in seconds for the 24h window
	// sourced from the MCP warm tier. Use MttaByPeriod for other windows.
	MttaIndex map[string]float64
	// MttaByPeriod maps period token to (UUID -> MTTA seconds) for the
	// cold-tier multi-period fan-out. Nil when no cold-tier MTTA data exists.
	MttaByPeriod map[string]map[string]float64
	// AnomalyCountIndex maps monitor UUID to its current anomaly count.
	AnomalyCountIndex map[string]int
	// AnomalyScoreIndex maps monitor UUID to its highest anomaly score.
	AnomalyScoreIndex map[string]float64
	// TotalAlerts is the total alert count across all monitors.
	TotalAlerts int
	// DataAges maps tier name ("hot", "warm", "cold") to the number of
	// seconds elapsed since the last successful refresh for that tier. Absent
	// key means the tier has not completed a successful refresh yet.
	DataAges map[string]float64
	// ExcludedCount is the number of monitors filtered out by the
	// --exclude-name-pattern on the last cache refresh.
	ExcludedCount int
	// ScrapeOK is true when the last scrape of core monitor data succeeded.
	ScrapeOK bool
}

// TakeSnapshot returns a public Snapshot of the Collector's current cached
// state. It is safe to call concurrently with Collect and Refresh.
//
// In tiered mode the snapshot is built from atomic tier pointers via
// buildCollectorSnapshot. In legacy mode the fields are copied under the
// collector's read lock. The returned Snapshot shares map backing arrays with
// the internal cache; callers must not mutate it.
func (c *Collector) TakeSnapshot() Snapshot {
	if c.cacheMode == CacheModeTiered && c.tiered != nil {
		return snapshotFromInternal(c.tiered.buildCollectorSnapshot())
	}
	c.mu.RLock()
	defer c.mu.RUnlock()

	reports := make(map[string][]hyperping.MonitorReport, len(c.reportsByPeriod))
	for k, v := range c.reportsByPeriod {
		reports[k] = v
	}
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
	dataAges := map[string]float64{}
	if !c.lastSuccessTime.IsZero() {
		dataAges["hot"] = time.Since(c.lastSuccessTime).Seconds()
	}
	return Snapshot{
		Monitors:          c.monitors,
		Reports:           reports,
		ResponseTimeIndex: rtIdx,
		MttaIndex:         mttaIdx,
		AnomalyCountIndex: anomCountIdx,
		AnomalyScoreIndex: anomScoreIdx,
		TotalAlerts:       c.totalAlerts,
		DataAges:          dataAges,
		ExcludedCount:     c.excludedCount,
		ScrapeOK:          c.lastScrapeOK,
	}
}

// snapshotFromInternal converts the unexported collectorSnapshot to the public
// Snapshot type. Called by TakeSnapshot in tiered mode.
func snapshotFromInternal(snap collectorSnapshot) Snapshot {
	return Snapshot{
		Monitors:          snap.monitors,
		OutageIndex:       snap.outageIndex,
		MaintenanceIndex:  snap.maintenanceIndex,
		Reports:           snap.reports,
		ResponseTimeIndex: snap.responseTimeIndex,
		MttaIndex:         snap.mttaIndex,
		MttaByPeriod:      snap.mttaByPeriod,
		AnomalyCountIndex: snap.anomalyCountIndex,
		AnomalyScoreIndex: snap.anomalyScoreIndex,
		TotalAlerts:       snap.totalAlerts,
		DataAges:          snap.dataAges,
		ExcludedCount:     snap.excludedCount,
		ScrapeOK:          snap.scrapeOK,
	}
}

// ExtractTenant returns the tenant prefix of a monitor name. It is the
// exported counterpart of extractTenant, intended for consumers outside
// this package such as the OTLP push bridge.
func ExtractTenant(monitorName string) string {
	return extractTenant(monitorName)
}

// EscalationTier returns the escalation tier label value for a monitor
// ("core" or "noncore"). Exported for the OTLP push bridge.
func EscalationTier(m hyperping.Monitor) string {
	return escalationTier(m)
}
