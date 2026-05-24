// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package collector

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	hyperping "github.com/develeap/hyperping-go"
)

// newTieredRefresherForTest builds a tieredRefresher wired to the supplied
// mockAPI with sensible test defaults. TTLs are non-zero so any code path
// that branches on them (defensive validation, ticker construction) sees a
// realistic value; tests that need ticker semantics override these.
func newTieredRefresherForTest(api *mockAPI) *tieredRefresher {
	return &tieredRefresher{
		api:     api,
		logger:  newTestLogger(),
		hotTTL:  60 * time.Second,
		warmTTL: 5 * time.Minute,
		coldTTL: 15 * time.Minute,
	}
}

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

// --- endpoint-isolation tests (chunk 2) ---
//
// These tests assert that each per-tier refresh function touches ONLY the
// endpoints that belong to its tier. The contract is:
//   - HOT: ListMonitors, ListHealthchecks, ListIncidents, ListOutages, ListMaintenance
//   - WARM: ListMonitorReports (24h only), ListMaintenance (full), and MCP calls
//   - COLD: ListMonitorReports (7d, 30d)
// ListMaintenance is called by both HOT (filtered to active) and WARM (full
// list), so per-tier counters are read in isolation per test.

func TestTieredRefresher_HotOnlyTouchesHotEndpoints(t *testing.T) {
	api := &mockAPI{
		monitors:     []hyperping.Monitor{{UUID: "mon_1", Name: "Web"}},
		healthchecks: []hyperping.Healthcheck{{UUID: "hc_1"}},
	}
	tr := newTieredRefresherForTest(api)

	tr.refreshHot(context.Background())

	assert.Equal(t, int32(1), api.monitorsCalls.Load(), "HOT must call ListMonitors")
	assert.Equal(t, int32(1), api.healthchecksCalls.Load(), "HOT must call ListHealthchecks")
	assert.Equal(t, int32(1), api.outagesCalls.Load(), "HOT must call ListOutages")
	assert.Equal(t, int32(1), api.incidentsCalls.Load(), "HOT must call ListIncidents")
	assert.Equal(t, int32(1), api.maintenanceCalls.Load(), "HOT must call ListMaintenance (filtered to active)")
	assert.Equal(t, int32(0), api.reportsCalls.Load(), "HOT must NOT call ListMonitorReports")
}

func TestTieredRefresher_WarmOnlyTouchesWarmEndpoints(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{{UUID: "mon_1", Name: "Web"}},
	}
	tr := newTieredRefresherForTest(api)
	// WARM depends on the HOT monitor list to know which uuids to fetch
	// per-monitor MCP metrics for. Pre-populate the HOT snapshot directly
	// rather than triggering a HOT refresh (which would dirty the
	// per-endpoint counters this test asserts).
	tr.hot.Store(&hotSnapshot{
		monitors:    api.monitors,
		refreshedAt: time.Now(),
	})

	tr.refreshWarm(context.Background())

	assert.Equal(t, int32(0), api.monitorsCalls.Load(), "WARM must NOT call ListMonitors")
	assert.Equal(t, int32(0), api.healthchecksCalls.Load(), "WARM must NOT call ListHealthchecks")
	assert.Equal(t, int32(0), api.outagesCalls.Load(), "WARM must NOT call ListOutages")
	assert.Equal(t, int32(0), api.incidentsCalls.Load(), "WARM must NOT call ListIncidents")
	assert.Equal(t, int32(1), api.maintenanceCalls.Load(), "WARM must call ListMaintenance (full list)")
	// WARM fetches only the 24h report window (7d and 30d are COLD).
	assert.Equal(t, int32(1), api.reportsCalls.Load(), "WARM must call ListMonitorReports exactly once for 24h")
	require.Len(t, api.reportRanges, 1)
	gap := mustParseTime(t, api.reportRanges[0].to).Sub(mustParseTime(t, api.reportRanges[0].from))
	assert.InDelta(t, (24 * time.Hour).Seconds(), gap.Seconds(), float64((1*time.Minute).Seconds()),
		"WARM must request the 24h window")
}

func TestTieredRefresher_ColdOnlyTouchesColdEndpoints(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{{UUID: "mon_1", Name: "Web"}},
	}
	tr := newTieredRefresherForTest(api)

	tr.refreshCold(context.Background())

	assert.Equal(t, int32(0), api.monitorsCalls.Load(), "COLD must NOT call ListMonitors")
	assert.Equal(t, int32(0), api.healthchecksCalls.Load(), "COLD must NOT call ListHealthchecks")
	assert.Equal(t, int32(0), api.outagesCalls.Load(), "COLD must NOT call ListOutages")
	assert.Equal(t, int32(0), api.incidentsCalls.Load(), "COLD must NOT call ListIncidents")
	assert.Equal(t, int32(0), api.maintenanceCalls.Load(), "COLD must NOT call ListMaintenance")
	assert.Equal(t, int32(2), api.reportsCalls.Load(), "COLD must call ListMonitorReports twice (7d, 30d)")
	require.Len(t, api.reportRanges, 2)
	gaps := []time.Duration{}
	for _, r := range api.reportRanges {
		gaps = append(gaps, mustParseTime(t, r.to).Sub(mustParseTime(t, r.from)))
	}
	// Order isn't guaranteed (the two requests run concurrently), so just
	// assert both 7d and 30d gaps appear.
	hasWindow := func(want time.Duration) bool {
		for _, g := range gaps {
			diff := g - want
			if diff < 0 {
				diff = -diff
			}
			if diff < 6*time.Hour {
				return true
			}
		}
		return false
	}
	assert.True(t, hasWindow(7*24*time.Hour), "COLD must request a 7d window; got %v", gaps)
	assert.True(t, hasWindow(30*24*time.Hour), "COLD must request a 30d window; got %v", gaps)
}

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err, "parse %q", s)
	return parsed
}
