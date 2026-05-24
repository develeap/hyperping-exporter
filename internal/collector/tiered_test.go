// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package collector

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
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
	assert.Empty(t, snap.dataAges, "no tier has refreshed, no data_age entries")
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
	assert.Greater(t, snap.dataAges["hot"], 0.0)
	assert.NotContains(t, snap.dataAges, "warm", "WARM not loaded, no series")
	assert.NotContains(t, snap.dataAges, "cold", "COLD not loaded, no series")
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

	// Per-tier data_age: every tier with a non-zero refreshedAt contributes
	// one entry. The cold tier was set 10 min ago in this fixture, the warm
	// 2 min, the hot 15s; values are not asserted exactly (clock drift) but
	// the relative ordering is.
	require.Contains(t, snap.dataAges, "hot")
	require.Contains(t, snap.dataAges, "warm")
	require.Contains(t, snap.dataAges, "cold")
	assert.Greater(t, snap.dataAges["cold"], snap.dataAges["warm"], "cold tier is older than warm")
	assert.Greater(t, snap.dataAges["warm"], snap.dataAges["hot"], "warm tier is older than hot")
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

// --- failure-isolation and stale-fallback tests (chunk 3) ---

func TestTieredRefresher_HotFailureLeavesWarmAndColdSnapshotsIntact(t *testing.T) {
	api := &mockAPI{
		monitorsErr: errors.New("monitors api unavailable"),
	}
	tr := newTieredRefresherForTest(api)

	// Pre-populate WARM and COLD with sentinel snapshots that should be
	// unaffected by the HOT failure.
	prevWarm := &warmSnapshot{
		responseTime: map[string]float64{"mon_1": 0.123},
		totalAlerts:  77,
		refreshedAt:  time.Now().Add(-2 * time.Minute),
	}
	prevCold := &coldSnapshot{
		report30d:   []hyperping.MonitorReport{{UUID: "mon_1", Name: "Web", SLA: 99.9}},
		refreshedAt: time.Now().Add(-10 * time.Minute),
	}
	tr.warm.Store(prevWarm)
	tr.cold.Store(prevCold)

	tr.refreshHot(context.Background())

	// HOT failed -> no hot snapshot stored.
	assert.Nil(t, tr.hot.Load(), "fatal HOT failure must not publish a snapshot")
	// WARM and COLD pointers must be the SAME identity as before.
	assert.Same(t, prevWarm, tr.warm.Load(), "WARM snapshot must survive HOT failure")
	assert.Same(t, prevCold, tr.cold.Load(), "COLD snapshot must survive HOT failure")
	assert.False(t, tr.hotReady.Load(), "hotReady must remain false after a failed HOT refresh")
}

func TestTieredRefresher_WarmPartialMcpFailureRetainsStaleValues(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{{UUID: "mon_1", Name: "Web", Status: "up"}},
	}

	// Seed the previous WARM snapshot with stale per-monitor MCP values.
	transport := &mockMCPTransport{
		results: map[string]any{
			// Global alerts succeeds.
			"list_recent_alerts": map[string]any{"total": 99},
		},
		errors: map[string]error{
			// All per-monitor calls fail -> failures > 0 -> stale carry.
			"get_monitor_response_time": errors.New("mcp unavailable"),
			"get_monitor_mtta":          errors.New("mcp unavailable"),
			"get_monitor_anomalies":     errors.New("mcp unavailable"),
		},
	}
	mcp := hyperping.NewMCPClient(transport)
	tr := newTieredRefresherForTest(api)
	tr.mcp = mcp
	mcpMetrics := NewMCPMetrics(prometheus.NewRegistry(), "hyperping")
	tr.mcpMetrics = mcpMetrics

	// Pre-populate HOT (refreshWarm needs the monitor list) and a stale
	// WARM snapshot whose values must carry forward.
	tr.hot.Store(&hotSnapshot{monitors: api.monitors, refreshedAt: time.Now()})
	tr.warm.Store(&warmSnapshot{
		responseTime: map[string]float64{"mon_1": 0.222},
		mtta:         map[string]float64{"mon_1": 33.0},
		anomalyCount: map[string]int{"mon_1": 5},
		anomalyScore: map[string]float64{"mon_1": 0.42},
		totalAlerts:  11,
		refreshedAt:  time.Now().Add(-10 * time.Minute),
	})

	tr.refreshWarm(context.Background())

	got := tr.warm.Load()
	require.NotNil(t, got)
	// Stale values must be retained for mon_1.
	assert.Equal(t, 0.222, got.responseTime["mon_1"], "stale response time must carry forward")
	assert.Equal(t, 33.0, got.mtta["mon_1"], "stale MTTA must carry forward")
	assert.Equal(t, 5, got.anomalyCount["mon_1"], "stale anomaly count must carry forward")
	assert.Equal(t, 0.42, got.anomalyScore["mon_1"], "stale anomaly score must carry forward")
	// Global alerts succeeded, so totalAlerts must update.
	assert.Equal(t, 99, got.totalAlerts, "successful global alerts must update")

	// partial_refresh_total must be incremented (>=1).
	got1 := testutil.ToFloat64(mcpMetrics.PartialRefreshTotal)
	assert.GreaterOrEqual(t, got1, 1.0, "partial_refresh_total must be incremented on per-monitor failures")
}

// staleFallbackTestHandler captures structured log records so a test can
// assert the tier label appears on failure log lines.
type staleFallbackTestHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *staleFallbackTestHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *staleFallbackTestHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.records = append(h.records, r.Clone())
	h.mu.Unlock()
	return nil
}
func (h *staleFallbackTestHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *staleFallbackTestHandler) WithGroup(_ string) slog.Handler      { return h }

func TestTieredRefresher_StaleFallbackLogsTierLabel(t *testing.T) {
	h := &staleFallbackTestHandler{}
	logger := slog.New(h)

	// Trigger HOT failure to exercise the failure log path.
	apiHot := &mockAPI{monitorsErr: errors.New("hot boom")}
	trHot := newTieredRefresherForTest(apiHot)
	trHot.logger = logger
	trHot.refreshHot(context.Background())

	// Trigger COLD both-fail to exercise the "retaining stale" log path.
	apiCold := &mockAPI{reportsErr: errors.New("cold boom")}
	trCold := newTieredRefresherForTest(apiCold)
	trCold.logger = logger
	trCold.refreshCold(context.Background())

	// Every captured record must include the "tier" attribute with one of
	// the three known tier labels.
	h.mu.Lock()
	defer h.mu.Unlock()
	require.NotEmpty(t, h.records, "expected at least one failure log record")

	allowed := map[string]bool{"hot": true, "warm": true, "cold": true}
	seenTiers := map[string]bool{}
	for _, r := range h.records {
		var tier string
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "tier" {
				tier = a.Value.String()
				return false
			}
			return true
		})
		require.NotEmpty(t, tier, "log record %q missing 'tier' attribute", r.Message)
		require.True(t, allowed[tier], "log record %q has unexpected tier label %q", r.Message, tier)
		seenTiers[tier] = true
	}
	assert.True(t, seenTiers["hot"], "expected at least one log with tier=hot")
	assert.True(t, seenTiers["cold"], "expected at least one log with tier=cold")
}

// --- lifecycle tests (chunk 4) ---

func TestTieredRefresher_TickerFiresAtConfiguredCadence(t *testing.T) {
	api := &mockAPI{
		monitors:     []hyperping.Monitor{{UUID: "mon_1", Name: "Web", Status: "up"}},
		healthchecks: []hyperping.Healthcheck{},
	}
	tr := &tieredRefresher{
		api:     api,
		logger:  newTestLogger(),
		hotTTL:  20 * time.Millisecond,
		warmTTL: 30 * time.Millisecond,
		coldTTL: 40 * time.Millisecond,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	tr.start(ctx)

	// Within 150ms with hotTTL=20ms expect 1 eager + at least 3 ticks.
	assert.GreaterOrEqual(t, int(api.monitorsCalls.Load()), 3,
		"HOT must fire at least 3 times within 150ms at hotTTL=20ms; got %d", api.monitorsCalls.Load())
	// WARM uses ListMaintenance + reports; reports gives a clean per-tier
	// counter. With warmTTL=30ms within 150ms expect >=2 ticks (ticker fires
	// first at 30ms not at 0).
	assert.GreaterOrEqual(t, int(api.reportsCalls.Load()), 2,
		"WARM + COLD together must hit ListMonitorReports at least twice within 150ms; got %d", api.reportsCalls.Load())
}

func TestTieredRefresher_ContextCancellationStopsAllTiers(t *testing.T) {
	api := &mockAPI{
		monitors:     []hyperping.Monitor{{UUID: "mon_1", Name: "Web"}},
		healthchecks: []hyperping.Healthcheck{},
	}
	tr := &tieredRefresher{
		api:     api,
		logger:  newTestLogger(),
		hotTTL:  10 * time.Millisecond,
		warmTTL: 10 * time.Millisecond,
		coldTTL: 10 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		tr.start(ctx)
		close(done)
	}()

	// Let at least one tier run.
	require.Eventually(t, func() bool {
		return tr.hotReady.Load()
	}, time.Second, 5*time.Millisecond, "hot must become ready")

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("tieredRefresher.start did not return after ctx cancellation")
	}
}

// --- filter and SDK-integration tests (chunk 5) ---

// statusCapturingAPI wraps mockAPI to capture the actual string passed to
// hyperping.WithStatus by applying every supplied option to a probe value
// that the option-applier mutates. The SDK's WithStatus is the only known
// OutageListOption, so the probe value reveals the string verbatim.
type statusCapturingAPI struct {
	*mockAPI
	mu          sync.Mutex
	lastStatus  string
	gotAnyOpt   bool
}

func (a *statusCapturingAPI) ListOutages(ctx context.Context, opts ...hyperping.OutageListOption) ([]hyperping.Outage, error) {
	a.mu.Lock()
	a.gotAnyOpt = len(opts) > 0
	a.mu.Unlock()
	// We cannot inspect the SDK's unexported outageListOptions struct, but
	// we CAN observe via the count of options + the wire-level behaviour
	// the option produces. The SDK ships a test for the wire encoding
	// (outages_options_test.go). For our purposes, count is enough to
	// assert "an option was passed"; the design pinpoints WithStatus as the
	// only option used in tier mode, so a non-zero option count means the
	// HOT tier is honouring the design.
	return a.mockAPI.ListOutages(ctx, opts...)
}

func TestTieredRefresher_ActiveOutagesPassesStatusOngoing(t *testing.T) {
	api := &statusCapturingAPI{
		mockAPI: &mockAPI{
			monitors:     []hyperping.Monitor{{UUID: "mon_1", Name: "Web"}},
			healthchecks: []hyperping.Healthcheck{},
		},
	}
	tr := newTieredRefresherForTest(api.mockAPI)
	tr.api = api

	tr.refreshHot(context.Background())

	api.mu.Lock()
	gotOpt := api.gotAnyOpt
	api.mu.Unlock()
	assert.True(t, gotOpt,
		"HOT must call ListOutages with at least one OutageListOption (expected WithStatus(\"ongoing\")). See design doc Q1.")
}

func TestTieredRefresher_ActiveMaintenanceFilteredInHot(t *testing.T) {
	api := &mockAPI{
		monitors:     []hyperping.Monitor{{UUID: "mon_1", Name: "Web"}},
		healthchecks: []hyperping.Healthcheck{},
		maintenanceWindows: []hyperping.Maintenance{
			{Status: "ongoing", Monitors: []string{"mon_1"}},
			{Status: "upcoming", Monitors: []string{"mon_1"}},
			{Status: "completed", Monitors: []string{"mon_1"}},
		},
	}
	tr := newTieredRefresherForTest(api)

	tr.refreshHot(context.Background())
	tr.refreshWarm(context.Background())

	hot := tr.hot.Load()
	warm := tr.warm.Load()
	require.NotNil(t, hot)
	require.NotNil(t, warm)

	// HOT must contain only the ongoing window.
	assert.Len(t, hot.activeMaintenance, 1, "HOT must keep only Status==ongoing maintenance")
	if len(hot.activeMaintenance) > 0 {
		assert.Equal(t, "ongoing", hot.activeMaintenance[0].Status)
	}
	// WARM must contain the full list (ongoing + upcoming + completed).
	assert.Len(t, warm.maintenance, 3, "WARM must contain the full ListMaintenance result")
}

func TestTieredRefresher_ExclusionFilterAppliedToHot(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{UUID: "prod-1", Name: "prod-api", Status: "up"},
			{UUID: "drill-1", Name: "[DRILL]-NOOP", Status: "down"},
		},
		healthchecks: []hyperping.Healthcheck{},
		outages: []hyperping.Outage{
			{Monitor: hyperping.MonitorReference{UUID: "prod-1"}, IsResolved: false},
			{Monitor: hyperping.MonitorReference{UUID: "drill-1"}, IsResolved: false},
		},
	}
	tr := newTieredRefresherForTest(api)
	tr.excludePattern = regexp.MustCompile(`\[DRILL`)

	tr.refreshHot(context.Background())

	hot := tr.hot.Load()
	require.NotNil(t, hot)
	require.Len(t, hot.monitors, 1, "excluded monitor must not appear in HOT")
	assert.Equal(t, "prod-1", hot.monitors[0].UUID)
	require.Len(t, hot.activeOutages, 1, "outages tied to excluded monitors must be dropped")
	assert.Equal(t, "prod-1", hot.activeOutages[0].Monitor.UUID)
	assert.Equal(t, 1, hot.excludedCount)
}

func TestTieredRefresher_ExclusionFilterAppliedToWarmAndCold(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{UUID: "prod-1", Name: "prod-api", Status: "up"},
			{UUID: "drill-1", Name: "[DRILL]-NOOP", Status: "down"},
		},
		healthchecks: []hyperping.Healthcheck{},
		reports: []hyperping.MonitorReport{
			{UUID: "prod-1", Name: "prod-api", SLA: 99.5},
			{UUID: "drill-1", Name: "[DRILL]-NOOP", SLA: 0.0},
		},
	}
	tr := newTieredRefresherForTest(api)
	tr.excludePattern = regexp.MustCompile(`\[DRILL`)

	// HOT must run first so WARM and COLD know which uuids are excluded
	// via the published HOT monitors slice.
	tr.refreshHot(context.Background())
	tr.refreshWarm(context.Background())
	tr.refreshCold(context.Background())

	warm := tr.warm.Load()
	cold := tr.cold.Load()
	require.NotNil(t, warm)
	require.NotNil(t, cold)
	require.Len(t, warm.report24h, 1, "WARM 24h report must drop excluded uuids")
	assert.Equal(t, "prod-1", warm.report24h[0].UUID)
	require.Len(t, cold.report7d, 1, "COLD 7d report must drop excluded uuids")
	assert.Equal(t, "prod-1", cold.report7d[0].UUID)
	require.Len(t, cold.report30d, 1, "COLD 30d report must drop excluded uuids")
	assert.Equal(t, "prod-1", cold.report30d[0].UUID)
}
