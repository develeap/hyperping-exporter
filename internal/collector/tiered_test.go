// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package collector

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	hyperping "github.com/develeap/hyperping-go"
)

// --- buildCollectorSnapshot tests (chunk 1) ---
//
// These tests exercise the snapshot-stitching path that the Collector's
// Collect() method calls when running in tiered mode. They cover the three
// shapes that matter for the collect path:
//   1. All three tier pointers nil  -> zero-value snapshot, no panic.
//   2. Only HOT populated           -> HOT fields visible, WARM/COLD blank.
//   3. All three populated          -> every field stitched into the snapshot.
//
// The tests construct a tieredRefresher manually (no goroutines, no API
// calls) and store snapshots directly into the atomic pointers so the
// stitcher is exercised in isolation from the refresh code paths.

func TestTieredRefresher_BuildCollectorSnapshot_AllNilTiers(t *testing.T) {
	tr := &tieredRefresher{}

	snap := tr.buildCollectorSnapshot()

	// Zero-value snapshot: no panic, all slices/maps empty, no data age yet.
	assert.Empty(t, snap.monitors)
	assert.Empty(t, snap.healthchecks)
	assert.Empty(t, snap.outageIndex)
	assert.Empty(t, snap.reports)
	assert.Empty(t, snap.maintenanceIndex)
	assert.Empty(t, snap.responseTimeIndex)
	assert.Empty(t, snap.mttaIndex)
	assert.Empty(t, snap.anomalyCountIndex)
	assert.Empty(t, snap.anomalyScoreIndex)
	assert.Equal(t, 0, snap.totalAlerts)
	assert.Equal(t, 0, snap.openIncidentCount)
	assert.Equal(t, 0, snap.activeMaintenanceCount)
	assert.Equal(t, 0, snap.excludedCount)
	assert.False(t, snap.scrapeOK)
	assert.True(t, snap.lastSuccessTime.IsZero())
	assert.Equal(t, 0.0, snap.dataAge)
}

func TestTieredRefresher_BuildCollectorSnapshot_OnlyHotPopulated(t *testing.T) {
	tr := &tieredRefresher{}

	hotTime := time.Now().Add(-30 * time.Second)
	hot := &hotSnapshot{
		monitors: []hyperping.Monitor{
			{UUID: "mon_1", Name: "Web", Status: "up"},
		},
		healthchecks: []hyperping.Healthcheck{
			{UUID: "hc_1", Name: "Job"},
		},
		incidents: []hyperping.Incident{
			{UUID: "i1", Type: "investigating"},
			{UUID: "i2", Type: "resolved"},
		},
		activeOutages: []hyperping.Outage{
			{UUID: "o1", IsResolved: false, Monitor: hyperping.MonitorReference{UUID: "mon_1"}},
		},
		activeMaintenance: []hyperping.Maintenance{
			{Status: "ongoing", Monitors: []string{"mon_1"}},
		},
		excludedCount: 2,
		refreshedAt:   hotTime,
		scrapeDur:     50 * time.Millisecond,
	}
	tr.hot.Store(hot)

	snap := tr.buildCollectorSnapshot()

	assert.Len(t, snap.monitors, 1)
	assert.Equal(t, "Web", snap.monitors[0].Name)
	assert.Len(t, snap.healthchecks, 1)
	// HOT carries active outages and active maintenance, which produce the
	// outage and maintenance indices that Collect emits per monitor.
	assert.Contains(t, snap.outageIndex, "mon_1")
	assert.True(t, snap.maintenanceIndex["mon_1"])
	assert.Equal(t, 1, snap.activeMaintenanceCount)
	assert.Equal(t, 1, snap.openIncidentCount, "two incidents, one resolved -> one open")
	assert.Equal(t, 2, snap.excludedCount)
	assert.True(t, snap.scrapeOK)
	assert.Equal(t, hotTime.Unix(), snap.lastSuccessTime.Unix())
	assert.Greater(t, snap.dataAge, 0.0)
	assert.Equal(t, 50*time.Millisecond, snap.scrapeDur)
	// WARM/COLD blank: no reports, no MCP metrics.
	assert.Empty(t, snap.reports)
	assert.Empty(t, snap.responseTimeIndex)
	assert.Equal(t, 0, snap.totalAlerts)
}

func TestTieredRefresher_BuildCollectorSnapshot_AllPopulated(t *testing.T) {
	tr := &tieredRefresher{}

	hotTime := time.Now().Add(-15 * time.Second)
	hot := &hotSnapshot{
		monitors: []hyperping.Monitor{
			{UUID: "mon_1", Name: "Web", Status: "up"},
		},
		healthchecks: []hyperping.Healthcheck{
			{UUID: "hc_1", Name: "Job"},
		},
		refreshedAt: hotTime,
	}
	tr.hot.Store(hot)

	warm := &warmSnapshot{
		responseTime: map[string]float64{"mon_1": 0.123},
		mtta:         map[string]float64{"mon_1": 45.0},
		anomalyCount: map[string]int{"mon_1": 2},
		anomalyScore: map[string]float64{"mon_1": 0.95},
		totalAlerts:  42,
		report24h: []hyperping.MonitorReport{
			{UUID: "mon_1", Name: "Web", SLA: 99.5},
		},
		maintenance: []hyperping.Maintenance{},
		refreshedAt: time.Now().Add(-2 * time.Minute),
	}
	tr.warm.Store(warm)

	cold := &coldSnapshot{
		report7d:    []hyperping.MonitorReport{{UUID: "mon_1", Name: "Web", SLA: 99.7}},
		report30d:   []hyperping.MonitorReport{{UUID: "mon_1", Name: "Web", SLA: 99.9}},
		refreshedAt: time.Now().Add(-10 * time.Minute),
	}
	tr.cold.Store(cold)

	snap := tr.buildCollectorSnapshot()

	// HOT fields.
	assert.Len(t, snap.monitors, 1)
	assert.Len(t, snap.healthchecks, 1)
	// WARM fields stitched in.
	assert.Equal(t, 0.123, snap.responseTimeIndex["mon_1"])
	assert.Equal(t, 45.0, snap.mttaIndex["mon_1"])
	assert.Equal(t, 2, snap.anomalyCountIndex["mon_1"])
	assert.Equal(t, 0.95, snap.anomalyScoreIndex["mon_1"])
	assert.Equal(t, 42, snap.totalAlerts)
	assert.Len(t, snap.reports["24h"], 1)
	// COLD fields stitched in.
	assert.Len(t, snap.reports["7d"], 1)
	assert.Len(t, snap.reports["30d"], 1)
	assert.Equal(t, 99.9, snap.reports["30d"][0].SLA)
}
