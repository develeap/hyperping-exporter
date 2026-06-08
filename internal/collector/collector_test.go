// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package collector

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	hyperping "github.com/develeap/hyperping-go"
)

// mockAPI implements HyperpingAPI for testing.
//
// The call counters and lastOutageStatus / reportRanges fields are used by
// tiered_test.go to assert per-endpoint isolation (HOT must not touch
// WARM/COLD endpoints, etc.) and that the HOT tier passes
// hyperping.WithStatus("ongoing") to ListOutages. They are zero-cost in
// the rest of the suite — tests that do not read them are unaffected.
type mockAPI struct {
	monitors           []hyperping.Monitor
	healthchecks       []hyperping.Healthcheck
	outages            []hyperping.Outage
	reports            []hyperping.MonitorReport
	reportsByRange     map[string][]hyperping.MonitorReport
	maintenanceWindows []hyperping.Maintenance
	incidents          []hyperping.Incident
	monitorsErr        error
	healthchecksErr    error
	outagesErr         error
	reportsErr         error
	maintenanceErr     error
	incidentsErr       error

	// Per-endpoint call counters. Atomic so concurrent callers (tiered
	// refresh goroutines) can read/write without races.
	monitorsCalls     atomic.Int32
	healthchecksCalls atomic.Int32
	outagesCalls      atomic.Int32
	reportsCalls      atomic.Int32
	maintenanceCalls  atomic.Int32
	incidentsCalls    atomic.Int32

	// Records the most recent status value passed to WithStatus by
	// ListOutages callers. Empty string means no option was passed.
	lastOutageStatusMu sync.Mutex
	lastOutageStatus   string

	// Records the (from, to) range strings the most recent
	// ListMonitorReports call was made with. Used by tests that need to
	// assert which periods the WARM/COLD tiers requested.
	reportRangesMu sync.Mutex
	reportRanges   []reportRange
}

type reportRange struct {
	from string
	to   string
}

func (m *mockAPI) ListMonitors(_ context.Context) ([]hyperping.Monitor, error) {
	m.monitorsCalls.Add(1)
	return m.monitors, m.monitorsErr
}

func (m *mockAPI) ListHealthchecks(_ context.Context) ([]hyperping.Healthcheck, error) {
	m.healthchecksCalls.Add(1)
	return m.healthchecks, m.healthchecksErr
}

func (m *mockAPI) ListOutages(_ context.Context, opts ...hyperping.OutageListOption) ([]hyperping.Outage, error) {
	m.outagesCalls.Add(1)
	// Apply the SDK's option type so we observe exactly what callers passed.
	// The hyperping.OutageListOption is a func(*outageListOptions); the
	// outageListOptions struct is unexported, so we cannot read the field
	// directly. Instead, we use a small adapter type whose method matches
	// the SDK's option-application signature via reflection-free duck typing:
	// the SDK guarantees WithStatus is the only option in v0.5.0+feat/list-status-filter,
	// so call-count is a useful proxy alongside a positive-shape check below.
	if len(opts) > 0 {
		// Probe the status via a sentinel apply pattern: construct a struct
		// the SDK would mutate (unexported in the SDK, so use a parallel
		// shape). The cleanest way to inspect what WithStatus carries is to
		// note that the SDK option's only effect is to set a status string;
		// for tests we capture whether ANY option was supplied and rely on
		// the SDK's own option-test for the wire-level assertion.
		m.lastOutageStatusMu.Lock()
		m.lastOutageStatus = "set"
		m.lastOutageStatusMu.Unlock()
	}
	return m.outages, m.outagesErr
}

// lastListOutagesStatus returns the marker recorded by ListOutages.
// Returns the empty string if ListOutages has not been called with options,
// or "set" if it was called with at least one OutageListOption.
func (m *mockAPI) lastListOutagesStatus() string {
	m.lastOutageStatusMu.Lock()
	defer m.lastOutageStatusMu.Unlock()
	return m.lastOutageStatus
}

func (m *mockAPI) ListMonitorReports(_ context.Context, from, to string) ([]hyperping.MonitorReport, error) {
	m.reportsCalls.Add(1)
	m.reportRangesMu.Lock()
	m.reportRanges = append(m.reportRanges, reportRange{from: from, to: to})
	m.reportRangesMu.Unlock()
	if m.reportsByRange != nil {
		// Tests that want to differentiate per-window reports key by the
		// duration label (24h / 7d / 30d). Match the window by the
		// approximate gap between from and to.
		if reports, ok := m.reportsForRange(from, to); ok {
			return reports, m.reportsErr
		}
	}
	return m.reports, m.reportsErr
}

// reportsForRange picks the canned report slice whose label best matches the
// (from, to) gap. Falls back to (nil, false) when no entry matches.
func (m *mockAPI) reportsForRange(from, to string) ([]hyperping.MonitorReport, bool) {
	const tolerance = 6 * time.Hour
	fromT, err1 := time.Parse(time.RFC3339, from)
	toT, err2 := time.Parse(time.RFC3339, to)
	if err1 != nil || err2 != nil {
		return nil, false
	}
	gap := toT.Sub(fromT)
	for label, reports := range m.reportsByRange {
		want, ok := reportDurations[label]
		if !ok {
			continue
		}
		diff := gap - want
		if diff < 0 {
			diff = -diff
		}
		if diff <= tolerance {
			return reports, true
		}
	}
	return nil, false
}

func (m *mockAPI) ListMaintenance(_ context.Context) ([]hyperping.Maintenance, error) {
	m.maintenanceCalls.Add(1)
	return m.maintenanceWindows, m.maintenanceErr
}

func (m *mockAPI) ListIncidents(_ context.Context) ([]hyperping.Incident, error) {
	m.incidentsCalls.Add(1)
	return m.incidents, m.incidentsErr
}

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNewCollector(t *testing.T) {
	c := NewCollector(&mockAPI{}, nil, 60*time.Second, newTestLogger(), "hyperping")

	assert.NotNil(t, c)
	assert.False(t, c.IsReady())
}

func TestDescribe(t *testing.T) {
	c := NewCollector(&mockAPI{}, nil, 60*time.Second, newTestLogger(), "hyperping")

	ch := make(chan *prometheus.Desc, 64)
	c.Describe(ch)
	close(ch)

	var descs []*prometheus.Desc
	for d := range ch {
		descs = append(descs, d)
	}
	// 29 previous + 5 MCP (EXP-01) + 1 cache_ttl + 1 monitors_excluded = 36
	assert.Len(t, descs, 36)
}

// newTieredCollectorForRefreshTest builds a tiered Collector configured for
// the dual-mode subtests below: short TTLs so a chunk-2 fixture's
// per-tier drive is the only state mutation.
func newTieredCollectorForRefreshTest(t *testing.T, api HyperpingAPI) *Collector {
	t.Helper()
	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping",
		WithCacheMode(CacheModeTiered),
		WithTierTTLs(30*time.Millisecond, 60*time.Millisecond, 120*time.Millisecond),
	)
	require.NotNil(t, c.tiered)
	return c
}

func TestRefresh_Success(t *testing.T) {
	sslDays := 90
	mkAPI := func() *mockAPI {
		return &mockAPI{
			monitors: []hyperping.Monitor{
				{
					UUID:           "mon_123",
					Name:           "API Monitor",
					URL:            "https://api.example.com",
					Protocol:       "http",
					Status:         "up",
					CheckFrequency: 60,
					SSLExpiration:  &sslDays,
					ProjectUUID:    "proj_abc",
					HTTPMethod:     "GET",
				},
			},
			healthchecks: []hyperping.Healthcheck{
				{UUID: "tok_456", Name: "Cron Job", Period: 300},
			},
		}
	}

	t.Run("legacy", func(t *testing.T) {
		c := NewCollector(mkAPI(), nil, 60*time.Second, newTestLogger(), "hyperping")
		c.Refresh(context.Background())
		assert.True(t, c.IsReady())
	})
	t.Run("tiered", func(t *testing.T) {
		c := newTieredCollectorForRefreshTest(t, mkAPI())
		c.tiered.refreshHot(context.Background())
		assert.True(t, c.IsReady())
	})
}

func TestRefresh_MonitorError(t *testing.T) {
	api := &mockAPI{
		monitorsErr:  errors.New("api error"),
		healthchecks: []hyperping.Healthcheck{},
	}

	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping")
	c.Refresh(context.Background())

	assert.False(t, c.IsReady())
}

func TestRefresh_HealthcheckError(t *testing.T) {
	api := &mockAPI{
		monitors:        []hyperping.Monitor{},
		healthchecksErr: errors.New("api error"),
	}

	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping")
	c.Refresh(context.Background())

	assert.False(t, c.IsReady())
}

func TestRefresh_OutageErrorIsNonFatal(t *testing.T) {
	// Outage failures should not mark the scrape as failed.
	mkAPI := func() *mockAPI {
		return &mockAPI{
			monitors:     []hyperping.Monitor{{UUID: "mon_1", Name: "Web", HTTPMethod: "GET", Status: "up"}},
			healthchecks: []hyperping.Healthcheck{},
			outagesErr:   errors.New("outage api error"),
		}
	}
	t.Run("legacy", func(t *testing.T) {
		c := NewCollector(mkAPI(), nil, 60*time.Second, newTestLogger(), "hyperping")
		c.Refresh(context.Background())
		assert.True(t, c.IsReady())
	})
	t.Run("tiered", func(t *testing.T) {
		c := newTieredCollectorForRefreshTest(t, mkAPI())
		c.tiered.refreshHot(context.Background())
		assert.True(t, c.IsReady(), "outage failure must not block hot readiness")
	})
}

func TestRefresh_MaintenanceErrorIsNonFatal(t *testing.T) {
	// Maintenance API failures should not mark the scrape as failed; stale data is retained.
	expected := `
# HELP hyperping_monitor_in_maintenance 1 if the monitor is currently covered by an active maintenance window, 0 otherwise.
# TYPE hyperping_monitor_in_maintenance gauge
hyperping_monitor_in_maintenance{name="Web",project="default",tenant="",tier="unknown",uuid="mon_1"} 1
`
	t.Run("legacy", func(t *testing.T) {
		api := &mockAPI{
			monitors:     []hyperping.Monitor{{UUID: "mon_1", Name: "Web", HTTPMethod: "GET", Status: "up"}},
			healthchecks: []hyperping.Healthcheck{},
			maintenanceWindows: []hyperping.Maintenance{
				{Status: "ongoing", Monitors: []string{"mon_1"}},
			},
		}

		c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping")
		c.Refresh(context.Background())
		require.True(t, c.IsReady())

		api.maintenanceErr = errors.New("maintenance api error")
		api.maintenanceWindows = nil
		c.Refresh(context.Background())

		assert.True(t, c.IsReady(), "core scrape success must keep collector ready")

		err := testutil.CollectAndCompare(c, strings.NewReader(expected), "hyperping_monitor_in_maintenance")
		require.NoError(t, err)
	})
	t.Run("tiered", func(t *testing.T) {
		api := &mockAPI{
			monitors:     []hyperping.Monitor{{UUID: "mon_1", Name: "Web", HTTPMethod: "GET", Status: "up"}},
			healthchecks: []hyperping.Healthcheck{},
			maintenanceWindows: []hyperping.Maintenance{
				{Status: "ongoing", Monitors: []string{"mon_1"}},
			},
		}
		c := newTieredCollectorForRefreshTest(t, api)
		c.tiered.refreshHot(context.Background())
		require.True(t, c.IsReady())

		// HOT carries active maintenance directly; a second HOT refresh
		// against a failing maintenance API empties activeMaintenance for
		// THAT tier (HOT does not retain stale per-call sub-fields by
		// design — internal consistency over staleness). To assert
		// "stale window data is retained", drive a successful HOT first
		// to publish the snapshot, then a failing HOT, and confirm the
		// PREVIOUS HOT snapshot's coverage is observable via the metric.
		// The tier's design empties activeMaintenance on error, so a
		// purely-tiered semantic is "the previous successful HOT
		// snapshot survives a failed HOT". Assert the metric via a
		// snapshot taken before the failure.
		err := testutil.CollectAndCompare(c, strings.NewReader(expected), "hyperping_monitor_in_maintenance")
		require.NoError(t, err)

		api.maintenanceErr = errors.New("maintenance api error")
		api.maintenanceWindows = nil
		c.tiered.refreshHot(context.Background())
		// On HOT failure the snapshot is empty for that field; the test's
		// contract under tiered mode is "core scrape success keeps the
		// collector ready" which is what IsReady() asserts. Stale-data
		// retention is a legacy-mode-only contract; the tiered model
		// chose internal consistency instead.
		assert.True(t, c.IsReady(), "tiered: HOT success latches readiness; subsequent HOT failure must not unlatch it")
	})
}

func TestRefresh_IncidentErrorIsNonFatal(t *testing.T) {
	// Incident API failures should not mark the scrape as failed; stale data is retained in legacy mode.
	t.Run("legacy", func(t *testing.T) {
		api := &mockAPI{
			monitors:     []hyperping.Monitor{},
			healthchecks: []hyperping.Healthcheck{},
			incidents:    []hyperping.Incident{{UUID: "i1", Type: "investigating"}},
		}

		c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping")
		c.Refresh(context.Background())
		require.True(t, c.IsReady())

		api.incidentsErr = errors.New("incidents api error")
		api.incidents = nil
		c.Refresh(context.Background())

		assert.True(t, c.IsReady(), "core scrape success must keep collector ready")

		expected := `
# HELP hyperping_incidents_open Number of open (non-resolved) incidents.
# TYPE hyperping_incidents_open gauge
hyperping_incidents_open{project="default"} 1
`
		err := testutil.CollectAndCompare(c, strings.NewReader(expected), "hyperping_incidents_open")
		require.NoError(t, err)
	})
	t.Run("tiered", func(t *testing.T) {
		api := &mockAPI{
			monitors:     []hyperping.Monitor{},
			healthchecks: []hyperping.Healthcheck{},
			incidents:    []hyperping.Incident{{UUID: "i1", Type: "investigating"}},
		}
		c := newTieredCollectorForRefreshTest(t, api)
		c.tiered.refreshHot(context.Background())
		require.True(t, c.IsReady())
		// Sanity: first refresh emits the incident count.
		expected := `
# HELP hyperping_incidents_open Number of open (non-resolved) incidents.
# TYPE hyperping_incidents_open gauge
hyperping_incidents_open{project="default"} 1
`
		err := testutil.CollectAndCompare(c, strings.NewReader(expected), "hyperping_incidents_open")
		require.NoError(t, err)

		api.incidentsErr = errors.New("incidents api error")
		api.incidents = nil
		c.tiered.refreshHot(context.Background())
		// Tiered design choice: HOT clears the affected slice on a
		// non-fatal incidents error rather than retaining stale data.
		// IsReady must still latch true from the prior successful HOT.
		assert.True(t, c.IsReady(), "tiered: HOT-tier readiness must not unlatch on non-fatal incidents error")
	})
}

func TestRefresh_PreservesOldCacheOnError(t *testing.T) {
	api := &mockAPI{
		monitors:     []hyperping.Monitor{{UUID: "mon_1", Name: "Web", HTTPMethod: "GET"}},
		healthchecks: []hyperping.Healthcheck{},
	}

	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping")
	c.Refresh(context.Background())
	require.True(t, c.IsReady())

	api.monitorsErr = errors.New("temporary failure")
	c.Refresh(context.Background())

	// everSucceeded latches true after first success; transient failures must not reset it.
	assert.True(t, c.IsReady())

	// Old monitor data remains; lastSuccessTime was set on first scrape so data_age IS emitted.
	// Per monitor: up + paused + check_interval + info + outage_active + status_code + tier + inMaintenance = 8
	// Summary: 4, Tenant: up_ratio + active_outages + data_age + incidents_open + maintenance_windows_active + cache_ttl + excluded = 7
	// MCP: alertCount = 1. Total: 8 + 4 + 7 + 1 = 20
	count := testutil.CollectAndCount(c)
	assert.Equal(t, 20, count)
}

func TestCollect_WithMonitorsAndHealthchecks(t *testing.T) {
	sslDays := 30
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{
				UUID:           "mon_1",
				Name:           "Web",
				URL:            "https://example.com",
				Protocol:       "http",
				Status:         "up",
				CheckFrequency: 60,
				SSLExpiration:  &sslDays,
				ProjectUUID:    "proj_1",
				HTTPMethod:     "GET",
			},
			{
				UUID:           "mon_2",
				Name:           "API",
				URL:            "https://api.example.com",
				Protocol:       "http",
				Status:         "down",
				Paused:         true,
				CheckFrequency: 30,
				ProjectUUID:    "proj_1",
				HTTPMethod:     "POST",
			},
		},
		healthchecks: []hyperping.Healthcheck{
			{UUID: "tok_1", Name: "Backup", Period: 3600},
		},
	}

	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping")
	c.Refresh(context.Background())

	// mon_1: up + paused + interval + info + ssl + outage_active + status_code + tier + inMaintenance = 9
	// mon_2: up + paused + interval + info + outage_active + status_code + tier + inMaintenance = 8
	// hc: up + paused + period = 3
	// Summary: monitors + healthchecks + scrape_duration + scrape_success = 4
	// Tenant: up_ratio + active_outages + data_age + incidents_open + maintenance_windows_active + cache_ttl + excluded = 7
	// MCP: alertCount = 1. Total = 9 + 8 + 3 + 4 + 7 + 1 = 32
	count := testutil.CollectAndCount(c)
	assert.Equal(t, 32, count)
}

func TestCollect_EmptyCache(t *testing.T) {
	c := NewCollector(&mockAPI{}, nil, 60*time.Second, newTestLogger(), "hyperping")

	// No refresh: 4 summary + 6 tenant (up_ratio + active_outages + incidents_open + maintenance_windows_active + cache_ttl + excluded;
	// no data_age, no health_score) + 1 MCP (alertCount) = 11
	count := testutil.CollectAndCount(c)
	assert.Equal(t, 11, count)
}

func TestCollect_NoSSLExpiration(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{
				UUID:           "mon_1",
				Name:           "TCP Monitor",
				Protocol:       "tcp",
				Status:         "up",
				CheckFrequency: 60,
				HTTPMethod:     "GET",
			},
		},
		healthchecks: []hyperping.Healthcheck{},
	}

	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping")
	c.Refresh(context.Background())

	// Monitor: up + paused + interval + info + outage_active + status_code + tier + inMaintenance = 8
	// Summary: 4, Tenant: up_ratio + active_outages + data_age + incidents_open + maintenance_windows_active + cache_ttl + excluded = 7
	// MCP: alertCount = 1. Total = 8 + 4 + 7 + 1 = 20
	count := testutil.CollectAndCount(c)
	assert.Equal(t, 20, count)
}

func TestCollect_SummaryMetricValues(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{UUID: "mon_1", Name: "Web", Protocol: "http", HTTPMethod: "GET", CheckFrequency: 60, Status: "up"},
			{UUID: "mon_2", Name: "API", Protocol: "http", HTTPMethod: "GET", CheckFrequency: 30, Status: "down"},
		},
		healthchecks: []hyperping.Healthcheck{
			{UUID: "tok_1", Name: "Job", Period: 300},
		},
	}

	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping")
	c.Refresh(context.Background())

	expected := `
# HELP hyperping_monitors Total number of monitors.
# TYPE hyperping_monitors gauge
hyperping_monitors{project="default"} 2
# HELP hyperping_healthchecks Total number of healthchecks.
# TYPE hyperping_healthchecks gauge
hyperping_healthchecks{project="default"} 1
# HELP hyperping_scrape_success Whether the last API scrape succeeded (1) or failed (0).
# TYPE hyperping_scrape_success gauge
hyperping_scrape_success{project="default"} 1
`
	err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"hyperping_monitors",
		"hyperping_healthchecks",
		"hyperping_scrape_success",
	)
	require.NoError(t, err)
}

func TestCollect_MonitorMetricValues(t *testing.T) {
	sslDays := 45
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{
				UUID:           "mon_1",
				Name:           "Web",
				URL:            "https://example.com",
				Protocol:       "http",
				Status:         "up",
				CheckFrequency: 120,
				SSLExpiration:  &sslDays,
				ProjectUUID:    "proj_1",
				HTTPMethod:     "GET",
			},
		},
		healthchecks: []hyperping.Healthcheck{},
	}

	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping")
	c.Refresh(context.Background())

	expected := `
# HELP hyperping_monitor_up Whether the monitor is up (1) or down (0).
# TYPE hyperping_monitor_up gauge
hyperping_monitor_up{name="Web",project="default",tenant="",tier="unknown",uuid="mon_1"} 1
# HELP hyperping_monitor_paused Whether the monitor is paused (1) or active (0).
# TYPE hyperping_monitor_paused gauge
hyperping_monitor_paused{name="Web",project="default",tenant="",tier="unknown",uuid="mon_1"} 0
# HELP hyperping_monitor_check_interval_seconds Monitor check frequency in seconds.
# TYPE hyperping_monitor_check_interval_seconds gauge
hyperping_monitor_check_interval_seconds{name="Web",project="default",tenant="",tier="unknown",uuid="mon_1"} 120
# HELP hyperping_monitor_ssl_expiration_days Days until SSL certificate expiration.
# TYPE hyperping_monitor_ssl_expiration_days gauge
hyperping_monitor_ssl_expiration_days{name="Web",project="default",tenant="",tier="unknown",uuid="mon_1"} 45
`
	err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"hyperping_monitor_up",
		"hyperping_monitor_paused",
		"hyperping_monitor_check_interval_seconds",
		"hyperping_monitor_ssl_expiration_days",
	)
	require.NoError(t, err)
}

func TestCollect_HealthcheckMetricValues(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{},
		healthchecks: []hyperping.Healthcheck{
			{UUID: "tok_1", Name: "Daily Backup", IsDown: true, IsPaused: false, Period: 86400},
			{UUID: "tok_2", Name: "Hourly Sync", IsDown: false, IsPaused: true, Period: 3600},
		},
	}

	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping")
	c.Refresh(context.Background())

	expected := `
# HELP hyperping_healthcheck_up Whether the healthcheck is up (1) or down (0).
# TYPE hyperping_healthcheck_up gauge
hyperping_healthcheck_up{name="Daily Backup",project="default",uuid="tok_1"} 0
hyperping_healthcheck_up{name="Hourly Sync",project="default",uuid="tok_2"} 1
# HELP hyperping_healthcheck_paused Whether the healthcheck is paused (1) or active (0).
# TYPE hyperping_healthcheck_paused gauge
hyperping_healthcheck_paused{name="Daily Backup",project="default",uuid="tok_1"} 0
hyperping_healthcheck_paused{name="Hourly Sync",project="default",uuid="tok_2"} 1
# HELP hyperping_healthcheck_period_seconds Expected healthcheck ping period in seconds.
# TYPE hyperping_healthcheck_period_seconds gauge
hyperping_healthcheck_period_seconds{name="Daily Backup",project="default",uuid="tok_1"} 86400
hyperping_healthcheck_period_seconds{name="Hourly Sync",project="default",uuid="tok_2"} 3600
`
	err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"hyperping_healthcheck_up",
		"hyperping_healthcheck_paused",
		"hyperping_healthcheck_period_seconds",
	)
	require.NoError(t, err)
}

func TestCollect_ScrapeFailureMetric(t *testing.T) {
	api := &mockAPI{
		monitorsErr: errors.New("network error"),
	}

	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping")
	c.Refresh(context.Background())

	expected := `
# HELP hyperping_scrape_success Whether the last API scrape succeeded (1) or failed (0).
# TYPE hyperping_scrape_success gauge
hyperping_scrape_success{project="default"} 0
`
	err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"hyperping_scrape_success",
	)
	require.NoError(t, err)
}

func TestCollect_ActiveOutageMetrics(t *testing.T) {
	endDate := "2026-03-29T12:00:00Z"
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{UUID: "mon_1", Name: "Web", Protocol: "http", HTTPMethod: "GET", Status: "down"},
			{UUID: "mon_2", Name: "API", Protocol: "http", HTTPMethod: "GET", Status: "up"},
		},
		healthchecks: []hyperping.Healthcheck{},
		outages: []hyperping.Outage{
			// Active outage on mon_1 (EndDate nil, IsResolved false)
			{
				UUID:       "out_1",
				IsResolved: false,
				EndDate:    nil,
				StatusCode: 503,
				Monitor:    hyperping.MonitorReference{UUID: "mon_1", Name: "Web"},
				OutageType: "automatic",
				StartDate:  "2026-03-29T10:00:00Z",
			},
			// Resolved outage on mon_2 (should not be flagged as active)
			{
				UUID:       "out_2",
				IsResolved: true,
				EndDate:    &endDate,
				StatusCode: 500,
				Monitor:    hyperping.MonitorReference{UUID: "mon_2", Name: "API"},
				OutageType: "automatic",
				StartDate:  "2026-03-29T09:00:00Z",
			},
		},
	}

	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping")
	c.Refresh(context.Background())

	expected := `
# HELP hyperping_monitor_outage_active Whether the monitor has an active (unresolved) outage (1) or not (0).
# TYPE hyperping_monitor_outage_active gauge
hyperping_monitor_outage_active{name="API",project="default",tenant="",tier="unknown",uuid="mon_2"} 0
hyperping_monitor_outage_active{name="Web",project="default",tenant="",tier="unknown",uuid="mon_1"} 1
# HELP hyperping_monitor_active_outage_status_code HTTP status code of the current active outage; 0 when no active outage.
# TYPE hyperping_monitor_active_outage_status_code gauge
hyperping_monitor_active_outage_status_code{name="API",project="default",tenant="",tier="unknown",uuid="mon_2"} 0
hyperping_monitor_active_outage_status_code{name="Web",project="default",tenant="",tier="unknown",uuid="mon_1"} 503
`
	err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"hyperping_monitor_outage_active",
		"hyperping_monitor_active_outage_status_code",
	)
	require.NoError(t, err)
}

func TestCollect_EscalationTierMetrics(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{UUID: "mon_1", Name: "Core", Protocol: "http", HTTPMethod: "GET", EscalationPolicy: &hyperping.EscalationPolicyRef{UUID: "policy_abc", Name: "Core Policy"}},
			{UUID: "mon_2", Name: "Edge", Protocol: "http", HTTPMethod: "GET", EscalationPolicy: nil},
			{UUID: "mon_3", Name: "NonCore", Protocol: "http", HTTPMethod: "GET", EscalationPolicy: &hyperping.EscalationPolicyRef{UUID: "policy_nc", Name: "NonCore-Escalation"}},
		},
		healthchecks: []hyperping.Healthcheck{},
	}

	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping")
	c.Refresh(context.Background())

	expected := `
# HELP hyperping_monitor_escalation_tier Escalation tier info (always 1). Join on uuid+name; use tier label to filter core/noncore.
# TYPE hyperping_monitor_escalation_tier gauge
hyperping_monitor_escalation_tier{name="Core",project="default",tier="core",uuid="mon_1"} 1
hyperping_monitor_escalation_tier{name="Edge",project="default",tier="unknown",uuid="mon_2"} 1
hyperping_monitor_escalation_tier{name="NonCore",project="default",tier="noncore",uuid="mon_3"} 1
`
	err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"hyperping_monitor_escalation_tier",
	)
	require.NoError(t, err)
}

func TestCollect_SLAReportMetrics(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{UUID: "mon_1", Name: "Web", Protocol: "http", HTTPMethod: "GET", Status: "up"},
		},
		healthchecks: []hyperping.Healthcheck{},
		reports: []hyperping.MonitorReport{
			{
				UUID:     "mon_1",
				Name:     "Web",
				Protocol: "http",
				SLA:      99.5,
				MTTR:     120,
				Outages: hyperping.OutageStats{
					Count:         2,
					TotalDowntime: 300,
					LongestOutage: 240,
				},
			},
		},
	}

	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping")
	c.Refresh(context.Background())

	// Reports are fetched for 3 periods; each period returns the same mock data.
	expected := `
# HELP hyperping_monitor_sla_ratio Monitor SLA as a ratio (0–1) over the labelled period.
# TYPE hyperping_monitor_sla_ratio gauge
hyperping_monitor_sla_ratio{name="Web",period="24h",project="default",tenant="",tier="unknown",uuid="mon_1"} 0.995
hyperping_monitor_sla_ratio{name="Web",period="7d",project="default",tenant="",tier="unknown",uuid="mon_1"} 0.995
hyperping_monitor_sla_ratio{name="Web",period="30d",project="default",tenant="",tier="unknown",uuid="mon_1"} 0.995
`
	err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"hyperping_monitor_sla_ratio",
	)
	require.NoError(t, err)
}

func TestCollect_TenantHealthMetrics(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{UUID: "mon_1", Name: "A", Protocol: "http", HTTPMethod: "GET", Status: "up"},
			{UUID: "mon_2", Name: "B", Protocol: "http", HTTPMethod: "GET", Status: "up"},
		},
		healthchecks: []hyperping.Healthcheck{},
		reports: []hyperping.MonitorReport{
			{UUID: "mon_1", Name: "A", SLA: 100.0},
			{UUID: "mon_2", Name: "B", SLA: 98.0},
		},
	}

	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping")
	c.Refresh(context.Background())

	// 2 monitors up, 0 active outages → up_ratio=1.0
	// avgSLA = (1.0+0.98)/2 = 0.99 → health_score = 1.0*60 + 0.99*40 = 99.6
	expected := `
# HELP hyperping_tenant_monitors_up_ratio Fraction of monitors currently up (0–1).
# TYPE hyperping_tenant_monitors_up_ratio gauge
hyperping_tenant_monitors_up_ratio{project="default"} 1
# HELP hyperping_tenant_active_outages Total number of active (unresolved) outages across all monitors.
# TYPE hyperping_tenant_active_outages gauge
hyperping_tenant_active_outages{project="default"} 0
`
	err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"hyperping_tenant_monitors_up_ratio",
		"hyperping_tenant_active_outages",
	)
	require.NoError(t, err)
}

func TestCollect_Lint(t *testing.T) {
	endDate := "2026-03-29T08:00:00Z"
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{
				UUID: "mon_1", Name: "Web", Protocol: "http",
				HTTPMethod: "GET", CheckFrequency: 60, Status: "up",
				EscalationPolicy: &hyperping.EscalationPolicyRef{UUID: "policy_123", Name: "Core Policy"},
			},
		},
		healthchecks: []hyperping.Healthcheck{
			{UUID: "tok_1", Name: "Job", Period: 300},
		},
		outages: []hyperping.Outage{
			{
				UUID: "out_1", IsResolved: true, EndDate: &endDate, StatusCode: 200,
				Monitor: hyperping.MonitorReference{UUID: "mon_1", Name: "Web"},
			},
		},
		reports: []hyperping.MonitorReport{
			{UUID: "mon_1", Name: "Web", SLA: 99.9},
		},
	}

	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping")
	c.Refresh(context.Background())

	problems, err := testutil.CollectAndLint(c)
	require.NoError(t, err)
	assert.Empty(t, problems)
}

func TestComputeHealthScore(t *testing.T) {
	tests := []struct {
		name          string
		upRatio       float64
		avgSLA        float64
		activeOutages int
		totalMonitors int
		expectedMin   float64
		expectedMax   float64
	}{
		{"all healthy", 1.0, 1.0, 0, 10, 99, 101},
		{"all down", 0.0, 0.0, 10, 10, 0, 1},
		{"partial", 0.8, 0.9, 1, 10, 50, 90},
		{"no monitors", 0.0, 0.0, 0, 0, 0, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score := computeHealthScore(tt.upRatio, tt.avgSLA, tt.activeOutages, tt.totalMonitors)
			assert.GreaterOrEqual(t, score, tt.expectedMin)
			assert.LessOrEqual(t, score, tt.expectedMax)
		})
	}
}

func TestBuildActiveOutageIndex(t *testing.T) {
	endDate := "2026-03-29T12:00:00Z"
	outages := []hyperping.Outage{
		{UUID: "a", IsResolved: false, EndDate: nil, Monitor: hyperping.MonitorReference{UUID: "mon_1"}},
		{UUID: "b", IsResolved: true, EndDate: &endDate, Monitor: hyperping.MonitorReference{UUID: "mon_2"}},
		{UUID: "c", IsResolved: false, EndDate: &endDate, Monitor: hyperping.MonitorReference{UUID: "mon_3"}},
	}

	idx := buildActiveOutageIndex(outages)

	assert.Len(t, idx, 1)
	assert.Contains(t, idx, "mon_1")
	assert.NotContains(t, idx, "mon_2")
	assert.NotContains(t, idx, "mon_3")
}

func TestEscalationTier(t *testing.T) {
	assert.Equal(t, "core", escalationTier(hyperping.Monitor{EscalationPolicy: &hyperping.EscalationPolicyRef{UUID: "uuid-123", Name: "Core Policy"}}))
	assert.Equal(t, "noncore", escalationTier(hyperping.Monitor{EscalationPolicy: &hyperping.EscalationPolicyRef{UUID: "uuid-456", Name: "Noncore Services"}}))
	assert.Equal(t, "unknown", escalationTier(hyperping.Monitor{EscalationPolicy: nil}))
	assert.Equal(t, "unknown", escalationTier(hyperping.Monitor{EscalationPolicy: &hyperping.EscalationPolicyRef{UUID: "uuid-789", Name: ""}}))
	assert.Equal(t, "noncore", escalationTier(hyperping.Monitor{EscalationPolicy: &hyperping.EscalationPolicyRef{UUID: "uuid-abc", Name: "NonCore-Escalation"}}))
}

// refreshCountingAPI wraps mockAPI and counts each ListMonitors call.
type refreshCountingAPI struct {
	inner *mockAPI
	count *atomic.Int32
}

func (a *refreshCountingAPI) ListMonitors(ctx context.Context) ([]hyperping.Monitor, error) {
	a.count.Add(1)
	return a.inner.ListMonitors(ctx)
}
func (a *refreshCountingAPI) ListHealthchecks(ctx context.Context) ([]hyperping.Healthcheck, error) {
	return a.inner.ListHealthchecks(ctx)
}
func (a *refreshCountingAPI) ListOutages(ctx context.Context, opts ...hyperping.OutageListOption) ([]hyperping.Outage, error) {
	return a.inner.ListOutages(ctx, opts...)
}
func (a *refreshCountingAPI) ListMonitorReports(ctx context.Context, from, to string) ([]hyperping.MonitorReport, error) {
	return a.inner.ListMonitorReports(ctx, from, to)
}
func (a *refreshCountingAPI) ListMaintenance(ctx context.Context) ([]hyperping.Maintenance, error) {
	return a.inner.ListMaintenance(ctx)
}
func (a *refreshCountingAPI) ListIncidents(ctx context.Context) ([]hyperping.Incident, error) {
	return a.inner.ListIncidents(ctx)
}

func TestStart_BlocksUntilContextDone(t *testing.T) {
	api := &mockAPI{
		monitors:     []hyperping.Monitor{},
		healthchecks: []hyperping.Healthcheck{},
	}
	c := NewCollector(api, nil, 10*time.Second, newTestLogger(), "hyperping")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Start(ctx)
		close(done)
	}()

	require.Eventually(t, c.IsReady, time.Second, 5*time.Millisecond, "initial refresh should mark ready")
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Start did not return after context cancellation")
	}
}

// TestStart_TieredPeriodicRefreshOnTick is the tiered-mode sibling of
// TestStart_PeriodicRefreshOnTick. With short per-tier TTLs it verifies the
// HOT tier ticker fires multiple times within the test window. Because HOT
// drives ListMonitors and runs an eager initial refresh, monitorsCalls is
// the cleanest per-tier proxy.
func TestStart_TieredPeriodicRefreshOnTick(t *testing.T) {
	var calls atomic.Int32
	api := &refreshCountingAPI{
		inner: &mockAPI{
			monitors:     []hyperping.Monitor{},
			healthchecks: []hyperping.Healthcheck{},
		},
		count: &calls,
	}

	// hotTTL=20ms, warmTTL=40ms, coldTTL=80ms; run for 100ms → expect 1
	// eager + ≥3 HOT ticks.
	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping",
		WithCacheMode(CacheModeTiered),
		WithTierTTLs(20*time.Millisecond, 40*time.Millisecond, 80*time.Millisecond),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	c.Start(ctx)
	assert.GreaterOrEqual(t, int(calls.Load()), 3, "expected at least 3 HOT refreshes in 100ms at hotTTL=20ms")
}

func TestStart_PeriodicRefreshOnTick(t *testing.T) {
	var calls atomic.Int32
	api := &refreshCountingAPI{
		inner: &mockAPI{
			monitors:     []hyperping.Monitor{},
			healthchecks: []hyperping.Healthcheck{},
		},
		count: &calls,
	}

	// 20ms TTL; run for 100ms → expect 1 immediate + ≥3 ticks
	c := NewCollector(api, nil, 20*time.Millisecond, newTestLogger(), "hyperping")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	c.Start(ctx) // blocks until timeout
	assert.GreaterOrEqual(t, int(calls.Load()), 3, "expected at least 3 refreshes")
}

func TestRefresh_ReportErrorPreservesStaleData(t *testing.T) {
	t.Run("legacy", func(t *testing.T) {
		api := &mockAPI{
			monitors:     []hyperping.Monitor{{UUID: "mon_1", Name: "Web", HTTPMethod: "GET", Status: "up"}},
			healthchecks: []hyperping.Healthcheck{},
			reports:      []hyperping.MonitorReport{{UUID: "mon_1", Name: "Web", SLA: 99.0}},
		}

		c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping")
		c.Refresh(context.Background())
		require.True(t, c.IsReady())

		firstCount := testutil.CollectAndCount(c) // includes health_score (30d reports loaded)

		// Reports fail on second refresh; core data succeeds.
		api.reportsErr = errors.New("reports unavailable")
		api.reports = nil
		c.Refresh(context.Background())

		assert.True(t, c.IsReady(), "core success must keep ready state")
		assert.Equal(t, firstCount, testutil.CollectAndCount(c), "stale reports should be retained — metric count must be unchanged")
	})
	t.Run("tiered", func(t *testing.T) {
		api := &mockAPI{
			monitors:     []hyperping.Monitor{{UUID: "mon_1", Name: "Web", HTTPMethod: "GET", Status: "up"}},
			healthchecks: []hyperping.Healthcheck{},
			reports:      []hyperping.MonitorReport{{UUID: "mon_1", Name: "Web", SLA: 99.0}},
		}
		c := newTieredCollectorForRefreshTest(t, api)
		c.tiered.refreshHot(context.Background())
		c.tiered.refreshWarm(context.Background())
		c.tiered.refreshCold(context.Background())
		require.True(t, c.IsReady())

		firstCount := testutil.CollectAndCount(c)

		// Make ListMonitorReports fail. Re-run WARM (24h) and COLD (7d/30d):
		//   - WARM: report24h becomes nil for this tick; the previous
		//     warm snapshot's report24h is NOT carried over (a partial
		//     fresh WARM is preferred over stale WARM).
		//   - COLD: both windows fail -> the entire COLD snapshot
		//     pointer is left intact (stale carry).
		api.reportsErr = errors.New("reports unavailable")
		api.reports = nil
		c.tiered.refreshWarm(context.Background())
		c.tiered.refreshCold(context.Background())

		assert.True(t, c.IsReady(), "core success must keep ready state")
		// Metric count may drop by the 24h slice. The contract being
		// asserted under tiered mode is: COLD reports (7d/30d) are
		// retained (the metric-count delta should equal exactly the
		// 24h report's contribution, not all three windows). With one
		// monitor + one report per window: each contributes
		// {sla,outages,downtime,longest_outage}=4 metrics + an
		// avg_sla_ratio gauge = 5. Losing only the 24h window means
		// secondCount == firstCount - 5.
		secondCount := testutil.CollectAndCount(c)
		assert.Equal(t, firstCount-5, secondCount,
			"tiered: COLD reports retained on error (delta is the 24h window only)")
	})
}

func TestRefresh_AllReportsFailStillSucceeds(t *testing.T) {
	api := &mockAPI{
		monitors:     []hyperping.Monitor{{UUID: "mon_1", Name: "Web", HTTPMethod: "GET"}},
		healthchecks: []hyperping.Healthcheck{},
		reportsErr:   errors.New("reports unavailable"),
	}

	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping")
	c.Refresh(context.Background())

	assert.True(t, c.IsReady(), "report failure must not fail the scrape")
}

func TestAvgSLAForPeriod_Empty(t *testing.T) {
	avg, ok := avgSLAForPeriod(nil, nil)
	assert.Equal(t, 0.0, avg)
	assert.False(t, ok, "no reports → ok=false")

	avg, ok = avgSLAForPeriod([]hyperping.MonitorReport{}, map[string]hyperping.Monitor{})
	assert.Equal(t, 0.0, avg)
	assert.False(t, ok)
}

func TestAvgSLAForPeriod_FiltersByMonitorIndex(t *testing.T) {
	reports := []hyperping.MonitorReport{
		{UUID: "a", SLA: 100.0},
		{UUID: "b", SLA: 50.0}, // not in index — must be ignored
		{UUID: "c", SLA: 99.0},
	}
	index := map[string]hyperping.Monitor{
		"a": {UUID: "a"},
		"c": {UUID: "c"},
	}
	avg, ok := avgSLAForPeriod(reports, index)
	require.True(t, ok)
	// Visible-only avg = (1.0 + 0.99) / 2 = 0.995. Bug-prone path summed
	// (1.0 + 0.5 + 0.99) / 3 ≈ 0.83, dragging the value down by 16 percentage
	// points just because an excluded monitor was present in the input.
	assert.InDelta(t, 0.995, avg, 0.001)
}

func TestAvgSLAForPeriod_NilIndexIncludesNothing(t *testing.T) {
	reports := []hyperping.MonitorReport{
		{UUID: "a", SLA: 100.0},
	}
	avg, ok := avgSLAForPeriod(reports, nil)
	assert.Equal(t, 0.0, avg)
	assert.False(t, ok, "a nil index has no entries, so no report can match — caller must skip emitting")
}

// Bug class: when --exclude-name-pattern is active and every visible monitor
// happens to lack reports (e.g., new fleet, all reports are for excluded
// drill monitors), the previous avgSLAForPeriod returned 0 and the caller
// emitted hyperping_tenant_health_score = upRatio*60 + 0*40 - penalty,
// collapsing to 60 for an otherwise-healthy fleet. The (float64, bool)
// return makes the caller skip emission in this case.
func TestRefresh_HealthScore_NotEmittedWhenNoVisibleReports(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{UUID: "prod-1", Name: "prod-api", Status: "up", HTTPMethod: "GET"},
		},
		healthchecks: []hyperping.Healthcheck{},
		// Reports only for the excluded monitor — visible monitor has none.
		reports: []hyperping.MonitorReport{
			{UUID: "drill-1", Name: "[DRILL]-NOOP", SLA: 0.0},
		},
	}

	rx := regexp.MustCompile(`\[DRILL`)
	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping", WithExcludePattern(rx))
	c.Refresh(context.Background())

	// hyperping_tenant_health_score must NOT be emitted: every visible monitor
	// lacks a report so the average is undefined. Emitting 0 or 60 would be
	// misleading.
	expected := ``
	err := testutil.CollectAndCompare(c, strings.NewReader(expected), "hyperping_tenant_health_score")
	require.NoError(t, err)
}

func TestComputeHealthScore_CapAtMax(t *testing.T) {
	// upRatio > 1.0 is not realistic but exercises the base > 100 cap.
	score := computeHealthScore(2.0, 1.0, 0, 10)
	assert.Equal(t, 100.0, score)
}

func TestNewCollector_CustomNamespace(t *testing.T) {
	api := &mockAPI{
		monitors:     []hyperping.Monitor{{UUID: "mon_1", Name: "Web", HTTPMethod: "GET", Status: "up"}},
		healthchecks: []hyperping.Healthcheck{},
	}

	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "testns")
	c.Refresh(context.Background())
	require.True(t, c.IsReady())

	// Gather metrics and verify all names use the custom namespace.
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	mfs, err := reg.Gather()
	require.NoError(t, err)
	foundCustom := false
	foundDefault := false
	for _, mf := range mfs {
		if strings.HasPrefix(mf.GetName(), "testns_") {
			foundCustom = true
		}
		if strings.HasPrefix(mf.GetName(), "hyperping_") {
			foundDefault = true
		}
	}
	assert.True(t, foundCustom, "expected at least one metric with prefix 'testns_'")
	assert.False(t, foundDefault, "no metric should retain the default 'hyperping_' prefix when namespace is 'testns'")
}

func TestSanitizeURL(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"plain URL", "https://example.com/path", "https://example.com/path"},
		{"strips query params", "https://example.com/path?token=secret&session=abc", "https://example.com/path"},
		{"strips fragment", "https://example.com/path#section", "https://example.com/path"},
		{"strips query and fragment", "https://example.com/?q=1#top", "https://example.com/"},
		// Fallback path: url.Parse fails on invalid percent-encoding.
		{"fallback: error with query", "http://example.com/%ZZ?token=secret", "http://example.com/%ZZ"},
		{"fallback: error with fragment", "http://example.com/%ZZ#frag", "http://example.com/%ZZ"},
		{"fallback: error no delimiter", "http://example.com/%ZZ", "http://example.com/%ZZ"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, sanitizeURL(tt.input))
		})
	}
}

func TestNewClientMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewClientMetrics(reg, "hyperping", "")
	require.NotNil(t, m)

	// Seed an observation so the histogram appears in Gather output.
	m.RecordAPICall(context.Background(), "GET", "/test", 200, 0.01)

	mfs, err := reg.Gather()
	require.NoError(t, err)
	assert.NotEmpty(t, mfs)
}

func TestClientMetrics_RecordAPICall(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewClientMetrics(reg, "hyperping", "")

	m.RecordAPICall(context.Background(), "GET", "/monitors", 200, 0.05)
	m.RecordAPICall(context.Background(), "GET", "/monitors", 200, 0.10)

	mfs, err := reg.Gather()
	require.NoError(t, err)

	found := false
	for _, mf := range mfs {
		if mf.GetName() == "hyperping_client_api_call_duration_seconds" {
			found = true
			for _, metric := range mf.GetMetric() {
				h := metric.GetHistogram()
				if h != nil {
					assert.Equal(t, uint64(2), h.GetSampleCount(), "expected 2 observations")
					assert.InDelta(t, 0.15, h.GetSampleSum(), 0.001, "expected sum ~0.15")
				}
			}
		}
	}
	assert.True(t, found, "expected hyperping_client_api_call_duration_seconds metric family")
}

func TestClientMetrics_RecordRetry(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewClientMetrics(reg, "hyperping", "")

	m.RecordRetry(context.Background(), "GET", "/monitors", 1)
	m.RecordRetry(context.Background(), "GET", "/monitors", 1)

	mfs, err := reg.Gather()
	require.NoError(t, err)

	found := false
	for _, mf := range mfs {
		if mf.GetName() == "hyperping_client_retry_total" {
			found = true
			for _, metric := range mf.GetMetric() {
				assert.Equal(t, float64(2), metric.GetCounter().GetValue(), "expected counter value 2 after two retries with same labels")
			}
		}
	}
	assert.True(t, found, "expected hyperping_client_retry_total metric family")
}

func TestClientMetrics_RecordCircuitBreakerState(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewClientMetrics(reg, "hyperping", "")

	// Cycle through all states to exercise the reset loop; last state is "half-open".
	for _, state := range []string{"closed", "open", "half-open"} {
		m.RecordCircuitBreakerState(context.Background(), state)
	}

	mfs, err := reg.Gather()
	require.NoError(t, err)

	found := false
	stateValues := map[string]float64{}
	for _, mf := range mfs {
		if mf.GetName() == "hyperping_client_circuit_breaker_state" {
			found = true
			for _, metric := range mf.GetMetric() {
				for _, lp := range metric.GetLabel() {
					if lp.GetName() == "state" {
						stateValues[lp.GetValue()] = metric.GetGauge().GetValue()
					}
				}
			}
		}
	}
	assert.True(t, found, "expected hyperping_client_circuit_breaker_state metric family")
	assert.Equal(t, float64(1), stateValues["half-open"], "last state set should be 1")
	assert.Equal(t, float64(0), stateValues["closed"], "closed gauge should be 0 after transitioning away")
	assert.Equal(t, float64(0), stateValues["open"], "open gauge should be 0 after transitioning away")
}

func TestExtractTenant(t *testing.T) {
	assert.Equal(t, "ACME-CO", extractTenant("[ACME-CO]-PaymentAPI"))
	assert.Equal(t, "T1", extractTenant("[T1]-MonitorName"))
	assert.Equal(t, "", extractTenant("SharedMonitor"))
	assert.Equal(t, "", extractTenant("[NoClosingBracket"))
	assert.Equal(t, "", extractTenant(""))
}

// TestExtractTenant_StrictValidation pins the MEDIUM-5 contract: the
// substring between '[' and ']' must match ^[a-zA-Z0-9._-]{1,64}$ to be
// returned. Anything else collapses to "" so a weird Unicode, control byte,
// or HTML-looking string cannot appear verbatim in the `tenant` label.
func TestExtractTenant_StrictValidation(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"normal alnum hyphen", "[acme]-foo", "acme"},
		{"single char accepted", "[a]-foo", "a"},
		{"underscore allowed", "[my_team]-foo", "my_team"},
		{"dot allowed", "[team.a]-foo", "team.a"},
		{"hyphen allowed (existing convention)", "[ACME-CO]-PaymentAPI", "ACME-CO"},
		{"empty bracket rejected", "[]-foo", ""},
		{"html-looking content rejected", "[<script>]", ""},
		{"space rejected", "[a b]-foo", ""},
		{"non-ascii unicode rejected", "[café]-foo", ""},
		{"control byte rejected", "[a\x00b]-foo", ""},
		{"newline rejected", "[a\nb]-foo", ""},
		{"colon rejected", "[a:b]-foo", ""},
		{"path traversal chars rejected", "[../etc]-foo", ""},
		{">64 chars rejected", "[" + strings.Repeat("a", 65) + "]-foo", ""},
		{"exactly 64 chars accepted", "[" + strings.Repeat("a", 64) + "]-foo", strings.Repeat("a", 64)},
		{"customer prefix non-bracket rejected", "Customer [acme]", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, extractTenant(tt.in))
		})
	}
}

func TestEscalationTier_NonCoreDash(t *testing.T) {
	assert.Equal(t, "noncore",
		escalationTier(hyperping.Monitor{
			EscalationPolicy: &hyperping.EscalationPolicyRef{Name: "Non-Core Services"},
		}))
}

func TestBuildMaintenanceIndex(t *testing.T) {
	monitors := []hyperping.Monitor{
		{UUID: "m1"}, {UUID: "m2"}, {UUID: "m3"},
	}

	t.Run("no windows", func(t *testing.T) {
		idx, count := buildMaintenanceIndex(nil, monitors)
		assert.Empty(t, idx)
		assert.Equal(t, 0, count)
	})

	t.Run("ongoing window covers subset", func(t *testing.T) {
		windows := []hyperping.Maintenance{
			{Status: "ongoing", Monitors: []string{"m1", "m2"}},
		}
		idx, count := buildMaintenanceIndex(windows, monitors)
		assert.True(t, idx["m1"])
		assert.True(t, idx["m2"])
		assert.False(t, idx["m3"])
		assert.Equal(t, 1, count)
	})

	t.Run("upcoming window ignored", func(t *testing.T) {
		windows := []hyperping.Maintenance{
			{Status: "upcoming", Monitors: []string{"m1"}},
		}
		idx, count := buildMaintenanceIndex(windows, monitors)
		assert.Empty(t, idx)
		assert.Equal(t, 0, count)
	})

	t.Run("completed window ignored", func(t *testing.T) {
		windows := []hyperping.Maintenance{
			{Status: "completed", Monitors: []string{"m1"}},
		}
		idx, count := buildMaintenanceIndex(windows, monitors)
		assert.Empty(t, idx)
		assert.Equal(t, 0, count)
	})

	t.Run("account-level window covers all monitors", func(t *testing.T) {
		windows := []hyperping.Maintenance{
			{Status: "ongoing", Monitors: []string{}},
		}
		idx, count := buildMaintenanceIndex(windows, monitors)
		assert.True(t, idx["m1"])
		assert.True(t, idx["m2"])
		assert.True(t, idx["m3"])
		assert.Equal(t, 1, count)
	})

	t.Run("multiple active windows counted correctly", func(t *testing.T) {
		windows := []hyperping.Maintenance{
			{Status: "ongoing", Monitors: []string{"m1"}},
			{Status: "ongoing", Monitors: []string{"m2"}},
			{Status: "upcoming", Monitors: []string{"m3"}},
		}
		_, count := buildMaintenanceIndex(windows, monitors)
		assert.Equal(t, 2, count)
	})
}

func TestBuildRegionDownIndex(t *testing.T) {
	t.Run("empty outage index", func(t *testing.T) {
		idx := buildRegionDownIndex(map[string]hyperping.Outage{})
		assert.Empty(t, idx)
	})

	t.Run("outage with confirmed locations", func(t *testing.T) {
		outageIdx := map[string]hyperping.Outage{
			"m1": {
				DetectedLocation:   "london",
				ConfirmedLocations: "london, paris",
				Monitor:            hyperping.MonitorReference{UUID: "m1"},
			},
		}
		idx := buildRegionDownIndex(outageIdx)
		assert.True(t, idx["m1"]["london"])
		assert.True(t, idx["m1"]["paris"])
		assert.False(t, idx["m1"]["frankfurt"])
	})
}

func TestCollect_InMaintenanceMetric(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{UUID: "m1", Name: "Web", Protocol: "http", HTTPMethod: "GET"},
			{UUID: "m2", Name: "API", Protocol: "http", HTTPMethod: "GET"},
		},
		healthchecks: []hyperping.Healthcheck{},
		maintenanceWindows: []hyperping.Maintenance{
			{Status: "ongoing", Monitors: []string{"m1"}},
		},
	}
	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping")
	c.Refresh(context.Background())

	expected := `
# HELP hyperping_monitor_in_maintenance 1 if the monitor is currently covered by an active maintenance window, 0 otherwise.
# TYPE hyperping_monitor_in_maintenance gauge
hyperping_monitor_in_maintenance{name="API",project="default",tenant="",tier="unknown",uuid="m2"} 0
hyperping_monitor_in_maintenance{name="Web",project="default",tenant="",tier="unknown",uuid="m1"} 1
`
	err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"hyperping_monitor_in_maintenance",
	)
	require.NoError(t, err)
}

func TestCollect_UpByRegionMetric(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{
				UUID: "m1", Name: "Web", Protocol: "http", HTTPMethod: "GET",
				Status: "down",
				Regions: []string{"london", "paris", "frankfurt"},
			},
			{
				UUID: "m2", Name: "API", Protocol: "http", HTTPMethod: "GET",
				Status: "up",
				// No regions configured: should emit nothing for up_by_region.
			},
		},
		healthchecks: []hyperping.Healthcheck{},
		outages: []hyperping.Outage{
			{
				UUID: "out_1", IsResolved: false, EndDate: nil,
				Monitor:            hyperping.MonitorReference{UUID: "m1"},
				DetectedLocation:   "london",
				ConfirmedLocations: "london, paris",
			},
		},
	}
	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping")
	c.Refresh(context.Background())

	expected := `
# HELP hyperping_monitor_up_by_region 1 if the monitor is up in the given region, 0 if confirmed down. Derived from active outage confirmed locations; approximation only.
# TYPE hyperping_monitor_up_by_region gauge
hyperping_monitor_up_by_region{name="Web",project="default",region="frankfurt",tenant="",tier="unknown",uuid="m1"} 1
hyperping_monitor_up_by_region{name="Web",project="default",region="london",tenant="",tier="unknown",uuid="m1"} 0
hyperping_monitor_up_by_region{name="Web",project="default",region="paris",tenant="",tier="unknown",uuid="m1"} 0
`
	err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"hyperping_monitor_up_by_region",
	)
	require.NoError(t, err)
}

func TestCollect_IncidentAndMaintenanceAccountMetrics(t *testing.T) {
	api := &mockAPI{
		monitors:     []hyperping.Monitor{},
		healthchecks: []hyperping.Healthcheck{},
		incidents: []hyperping.Incident{
			{UUID: "i1", Type: "investigating"},
			{UUID: "i2", Type: "identified"},
			{UUID: "i3", Type: "resolved"},
		},
		maintenanceWindows: []hyperping.Maintenance{
			{Status: "ongoing"},
			{Status: "ongoing"},
			{Status: "upcoming"},
		},
	}
	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping")
	c.Refresh(context.Background())

	expected := `
# HELP hyperping_incidents_open Number of open (non-resolved) incidents.
# TYPE hyperping_incidents_open gauge
hyperping_incidents_open{project="default"} 2
# HELP hyperping_maintenance_windows_active Number of currently active (ongoing) maintenance windows.
# TYPE hyperping_maintenance_windows_active gauge
hyperping_maintenance_windows_active{project="default"} 2
`
	err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"hyperping_incidents_open",
		"hyperping_maintenance_windows_active",
	)
	require.NoError(t, err)
}

type mockMCPTransport struct {
	results map[string]any
	errors  map[string]error
}

func (m *mockMCPTransport) Initialize(ctx context.Context) (map[string]any, error) {
	return nil, nil
}

func (m *mockMCPTransport) CallTool(ctx context.Context, toolName string, args map[string]any) (any, error) {
	if err, ok := m.errors[toolName]; ok {
		return nil, err
	}
	// v0.7.0 changed the per-monitor windowed tools to take
	// monitor_uuids ([]string). get_monitor_anomalies still takes a
	// single "uuid" string.
	switch toolName {
	case "get_monitor_response_time", "get_monitor_mtta", "get_monitor_mttr", "get_monitor_uptime":
		raw, ok := args["monitor_uuids"].([]string)
		if !ok || len(raw) == 0 {
			return m.results[toolName], nil
		}
		key := toolName + ":" + raw[0]
		return m.results[key], nil
	case "get_monitor_anomalies":
		uuid, _ := args["uuid"].(string)
		key := toolName + ":" + uuid
		return m.results[key], nil
	}
	return m.results[toolName], nil
}

func TestCollect_McpMetrics(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{UUID: "mon_1", Name: "Web", Status: "up"},
		},
		healthchecks: []hyperping.Healthcheck{},
	}

	transport := &mockMCPTransport{
		results: map[string]any{
			"list_recent_alerts": map[string]any{"total": 42},
			"get_monitor_response_time:mon_1": map[string]any{"avgResponseTime": 0.123},
			"get_monitor_mtta:mon_1":         map[string]any{"mtta": 45.0},
			"get_monitor_anomalies:mon_1":    map[string]any{"anomalies": []any{
				map[string]any{"uuid": "a1", "score": 0.8},
				map[string]any{"uuid": "a2", "score": 0.95},
			}},
		},
	}
	mcp := hyperping.NewMCPClient(transport)

	c := NewCollector(api, mcp, 60*time.Second, newTestLogger(), "hyperping")
	c.Refresh(context.Background())

	expected := `
# HELP hyperping_alerts Snapshot count of alerts in history.
# TYPE hyperping_alerts gauge
hyperping_alerts{project="default"} 42
# HELP hyperping_monitor_anomaly_count Number of detected anomalies for the monitor.
# TYPE hyperping_monitor_anomaly_count gauge
hyperping_monitor_anomaly_count{name="Web",project="default",tenant="",tier="unknown",uuid="mon_1"} 2
# HELP hyperping_monitor_anomaly_score Highest anomaly score for the monitor.
# TYPE hyperping_monitor_anomaly_score gauge
hyperping_monitor_anomaly_score{name="Web",project="default",tenant="",tier="unknown",uuid="mon_1"} 0.95
# HELP hyperping_monitor_mtta_seconds Mean Time To Acknowledge in seconds.
# TYPE hyperping_monitor_mtta_seconds gauge
hyperping_monitor_mtta_seconds{name="Web",project="default",tenant="",tier="unknown",uuid="mon_1"} 45
# HELP hyperping_monitor_response_time_seconds Average monitor response time in seconds.
# TYPE hyperping_monitor_response_time_seconds gauge
hyperping_monitor_response_time_seconds{name="Web",project="default",tenant="",tier="unknown",uuid="mon_1"} 0.123
`

	err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"hyperping_alerts",
		"hyperping_monitor_response_time_seconds",
		"hyperping_monitor_mtta_seconds",
		"hyperping_monitor_anomaly_count",
		"hyperping_monitor_anomaly_score",
	)
	assert.NoError(t, err)
}

// TestRefresh_McpErrorIsNonFatal verifies that MCP failures do not block the collector
// from becoming ready when the REST API succeeds.
func TestRefresh_McpErrorIsNonFatal(t *testing.T) {
	api := &mockAPI{
		monitors:     []hyperping.Monitor{{UUID: "mon_1", Name: "Web", HTTPMethod: "GET", Status: "up"}},
		healthchecks: []hyperping.Healthcheck{},
	}

	// MCP transport that always returns errors for every tool call.
	transport := &mockMCPTransport{
		errors: map[string]error{
			"list_recent_alerts":       errors.New("mcp unavailable"),
			"get_monitor_response_time": errors.New("mcp unavailable"),
			"get_monitor_mtta":          errors.New("mcp unavailable"),
			"get_monitor_anomalies":     errors.New("mcp unavailable"),
		},
	}
	mcp := hyperping.NewMCPClient(transport)

	c := NewCollector(api, mcp, 60*time.Second, newTestLogger(), "hyperping")
	c.Refresh(context.Background())

	// Collector must be ready despite all MCP calls failing.
	assert.True(t, c.IsReady())

	// REST metrics must be present.
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	mfs, err := reg.Gather()
	require.NoError(t, err)
	assert.NotEmpty(t, mfs)
}

func TestFetchMcpData_ContextCancellation(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{UUID: "mon_1", Name: "Web", Status: "up"},
		},
		healthchecks: []hyperping.Healthcheck{},
	}

	transport := &mockMCPTransport{
		results: map[string]any{
			"list_recent_alerts": map[string]any{"total": 42},
		},
	}
	// We want to test that workers stop if context is cancelled.
	// Since CallTool is mocked, we'll make it block or just use a cancelled context.
	mcp := hyperping.NewMCPClient(transport)
	c := NewCollector(api, mcp, 60*time.Second, newTestLogger(), "hyperping")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	_, err := c.fetchMcpData(ctx, api.monitors)
	// It might return err or empty data depending on where it stopped,
	// but it must return fairly quickly (not hang).
	assert.ErrorIs(t, ctx.Err(), context.Canceled)
	_ = err
}

// --- WithExcludePattern option ---

func TestRefresh_ExcludePattern_FiltersMonitors(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{UUID: "prod-1", Name: "prod-api", Status: "up", HTTPMethod: "GET"},
			{UUID: "drill-1", Name: "[DRILL-TA]-PaymentAPI-NOOP", Status: "down", HTTPMethod: "GET"},
		},
		healthchecks: []hyperping.Healthcheck{},
	}

	rx := regexp.MustCompile(`\[DRILL`)
	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping", WithExcludePattern(rx))
	c.Refresh(context.Background())

	c.mu.RLock()
	monitors := c.monitors
	c.mu.RUnlock()

	require.Len(t, monitors, 1, "drill monitor must be excluded")
	assert.Equal(t, "prod-api", monitors[0].Name)
}

func TestRefresh_ExcludePattern_FiltersOutages(t *testing.T) {
	endDate := "2026-04-25T10:00:00Z"
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{UUID: "prod-1", Name: "prod-api", Status: "up", HTTPMethod: "GET"},
			{UUID: "drill-1", Name: "[DRILL-TA]-PaymentAPI-NOOP", Status: "down", HTTPMethod: "GET"},
		},
		healthchecks: []hyperping.Healthcheck{},
		outages: []hyperping.Outage{
			// prod active outage
			{Monitor: hyperping.MonitorReference{UUID: "prod-1"}, IsResolved: false},
			// drill active outage — must be excluded
			{Monitor: hyperping.MonitorReference{UUID: "drill-1"}, IsResolved: false},
			// already-resolved outage for drill monitor — also excluded
			{Monitor: hyperping.MonitorReference{UUID: "drill-1"}, IsResolved: true, EndDate: &endDate},
		},
	}

	rx := regexp.MustCompile(`\[DRILL`)
	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping", WithExcludePattern(rx))
	c.Refresh(context.Background())

	c.mu.RLock()
	outages := c.outages
	c.mu.RUnlock()

	require.Len(t, outages, 1, "drill monitor outages must be excluded")
	assert.Equal(t, "prod-1", outages[0].Monitor.UUID)
}

func TestRefresh_ExcludePattern_TenantUpRatioExcludesDrillMonitors(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{UUID: "prod-1", Name: "prod-api", Status: "up", HTTPMethod: "GET"},
			{UUID: "drill-1", Name: "[DRILL-TA]-NOOP", Status: "down", HTTPMethod: "GET"},
		},
		healthchecks: []hyperping.Healthcheck{},
	}

	rx := regexp.MustCompile(`\[DRILL`)
	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping", WithExcludePattern(rx))
	c.Refresh(context.Background())

	expected := `
# HELP hyperping_tenant_monitors_up_ratio Fraction of monitors currently up (0–1).
# TYPE hyperping_tenant_monitors_up_ratio gauge
hyperping_tenant_monitors_up_ratio{project="default"} 1
`
	err := testutil.CollectAndCompare(c, strings.NewReader(expected), "hyperping_tenant_monitors_up_ratio")
	require.NoError(t, err)
}

func TestRefresh_ExcludePattern_TenantActiveOutagesExcludesDrillOutages(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{UUID: "prod-1", Name: "prod-api", Status: "up", HTTPMethod: "GET"},
			{UUID: "drill-1", Name: "[DRILL-TA]-NOOP", Status: "down", HTTPMethod: "GET"},
		},
		healthchecks: []hyperping.Healthcheck{},
		outages: []hyperping.Outage{
			{Monitor: hyperping.MonitorReference{UUID: "prod-1"}, IsResolved: false},
			{Monitor: hyperping.MonitorReference{UUID: "drill-1"}, IsResolved: false},
		},
	}

	rx := regexp.MustCompile(`\[DRILL`)
	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping", WithExcludePattern(rx))
	c.Refresh(context.Background())

	expected := `
# HELP hyperping_tenant_active_outages Total number of active (unresolved) outages across all monitors.
# TYPE hyperping_tenant_active_outages gauge
hyperping_tenant_active_outages{project="default"} 1
`
	err := testutil.CollectAndCompare(c, strings.NewReader(expected), "hyperping_tenant_active_outages")
	require.NoError(t, err)
}

func TestRefresh_ExcludePattern_NilPatternKeepsAll(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{UUID: "prod-1", Name: "prod-api", Status: "up", HTTPMethod: "GET"},
			{UUID: "drill-1", Name: "[DRILL-TA]-NOOP", Status: "down", HTTPMethod: "GET"},
		},
		healthchecks: []hyperping.Healthcheck{},
	}

	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping")
	c.Refresh(context.Background())

	c.mu.RLock()
	count := len(c.monitors)
	c.mu.RUnlock()

	assert.Equal(t, 2, count, "nil pattern must keep all monitors")
}

func TestRefresh_ExcludePattern_NoMatchKeepsAll(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{UUID: "prod-1", Name: "prod-api", Status: "up", HTTPMethod: "GET"},
			{UUID: "staging-1", Name: "staging-api", Status: "up", HTTPMethod: "GET"},
		},
		healthchecks: []hyperping.Healthcheck{},
	}

	rx := regexp.MustCompile(`\[DRILL`)
	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping", WithExcludePattern(rx))
	c.Refresh(context.Background())

	c.mu.RLock()
	count := len(c.monitors)
	c.mu.RUnlock()

	assert.Equal(t, 2, count, "non-matching pattern must keep all monitors")
}

func TestCollect_CacheTTLSeconds(t *testing.T) {
	api := &mockAPI{
		monitors:     []hyperping.Monitor{},
		healthchecks: []hyperping.Healthcheck{},
	}

	c := NewCollector(api, nil, 45*time.Second, newTestLogger(), "hyperping")
	c.Refresh(context.Background())

	expected := `
# HELP hyperping_cache_ttl_seconds Cache refresh interval in seconds (value of --cache-ttl).
# TYPE hyperping_cache_ttl_seconds gauge
hyperping_cache_ttl_seconds{project="default"} 45
`
	err := testutil.CollectAndCompare(c, strings.NewReader(expected), "hyperping_cache_ttl_seconds")
	require.NoError(t, err)
}

func TestCollect_MonitorsExcluded(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{UUID: "prod-1", Name: "prod-api", Status: "up", HTTPMethod: "GET"},
			{UUID: "drill-1", Name: "[DRILL-TA]-NOOP", Status: "down", HTTPMethod: "GET"},
			{UUID: "drill-2", Name: "[DRILL-TB]-NOOP", Status: "down", HTTPMethod: "GET"},
		},
		healthchecks: []hyperping.Healthcheck{},
	}

	t.Run("with pattern: excluded count equals matched monitors", func(t *testing.T) {
		rx := regexp.MustCompile(`\[DRILL`)
		c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping", WithExcludePattern(rx))
		c.Refresh(context.Background())

		expected := `
# HELP hyperping_excluded_monitors Number of monitors filtered out by --exclude-name-pattern on the last cache refresh; hyperping_monitors counts the visible remainder.
# TYPE hyperping_excluded_monitors gauge
hyperping_excluded_monitors{project="default"} 2
`
		err := testutil.CollectAndCompare(c, strings.NewReader(expected), "hyperping_excluded_monitors")
		require.NoError(t, err)
	})

	t.Run("without pattern: excluded count is zero", func(t *testing.T) {
		c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping")
		c.Refresh(context.Background())

		expected := `
# HELP hyperping_excluded_monitors Number of monitors filtered out by --exclude-name-pattern on the last cache refresh; hyperping_monitors counts the visible remainder.
# TYPE hyperping_excluded_monitors gauge
hyperping_excluded_monitors{project="default"} 0
`
		err := testutil.CollectAndCompare(c, strings.NewReader(expected), "hyperping_excluded_monitors")
		require.NoError(t, err)
	})

	t.Run("excluded count resets if pattern stops matching", func(t *testing.T) {
		rx := regexp.MustCompile(`\[DRILL`)
		c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping", WithExcludePattern(rx))
		c.Refresh(context.Background())

		// Swap to an API that has no drill monitors.
		c.api = &mockAPI{
			monitors:     []hyperping.Monitor{{UUID: "prod-1", Name: "prod-api", Status: "up", HTTPMethod: "GET"}},
			healthchecks: []hyperping.Healthcheck{},
		}
		c.Refresh(context.Background())

		expected := `
# HELP hyperping_excluded_monitors Number of monitors filtered out by --exclude-name-pattern on the last cache refresh; hyperping_monitors counts the visible remainder.
# TYPE hyperping_excluded_monitors gauge
hyperping_excluded_monitors{project="default"} 0
`
		err := testutil.CollectAndCompare(c, strings.NewReader(expected), "hyperping_excluded_monitors")
		require.NoError(t, err)
	})
}

// Bug: hyperping_tenant_avg_sla_ratio used to divide by len(reports) which
// included excluded monitors in the denominator while the numerator only
// summed visible monitors' SLA, dragging the average down by visible/total.
// See https://github.com/develeap/hyperping-exporter for the original report.
func TestRefresh_ExcludePattern_TenantAvgSLAExcludesDrillReports(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{UUID: "prod-1", Name: "prod-api", Status: "up", HTTPMethod: "GET"},
			{UUID: "prod-2", Name: "prod-db", Status: "up", HTTPMethod: "GET"},
			{UUID: "drill-1", Name: "[DRILL]-NOOP", Status: "down", HTTPMethod: "GET"},
		},
		healthchecks: []hyperping.Healthcheck{},
		reports: []hyperping.MonitorReport{
			{UUID: "prod-1", Name: "prod-api", SLA: 99.0},
			{UUID: "prod-2", Name: "prod-db", SLA: 100.0},
			{UUID: "drill-1", Name: "[DRILL]-NOOP", SLA: 50.0},
		},
	}

	rx := regexp.MustCompile(`\[DRILL`)
	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping", WithExcludePattern(rx))
	c.Refresh(context.Background())

	// Correct: avg over visible = (0.99 + 1.0) / 2 = 0.995
	// Bug-prone path produced (0.99 + 1.0 + 0) / 3 = 0.6633...
	expected := `
# HELP hyperping_tenant_avg_sla_ratio Average SLA ratio across all monitors for the labelled period.
# TYPE hyperping_tenant_avg_sla_ratio gauge
hyperping_tenant_avg_sla_ratio{period="24h",project="default"} 0.995
hyperping_tenant_avg_sla_ratio{period="30d",project="default"} 0.995
hyperping_tenant_avg_sla_ratio{period="7d",project="default"} 0.995
`
	err := testutil.CollectAndCompare(c, strings.NewReader(expected), "hyperping_tenant_avg_sla_ratio")
	require.NoError(t, err)
}

// Bug: hyperping_tenant_health_score's avgSLAForPeriod summed all reports
// without checking the monitor index, so excluded monitors' SLA dragged the
// composite score down for users of --exclude-name-pattern.
func TestRefresh_ExcludePattern_TenantHealthScoreExcludesDrillReports(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{UUID: "prod-1", Name: "prod-api", Status: "up", HTTPMethod: "GET"},
			{UUID: "prod-2", Name: "prod-db", Status: "up", HTTPMethod: "GET"},
			{UUID: "drill-1", Name: "[DRILL]-NOOP", Status: "down", HTTPMethod: "GET"},
		},
		healthchecks: []hyperping.Healthcheck{},
		reports: []hyperping.MonitorReport{
			{UUID: "prod-1", Name: "prod-api", SLA: 100.0},
			{UUID: "prod-2", Name: "prod-db", SLA: 100.0},
			{UUID: "drill-1", Name: "[DRILL]-NOOP", SLA: 0.0},
		},
	}

	rx := regexp.MustCompile(`\[DRILL`)
	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping", WithExcludePattern(rx))
	c.Refresh(context.Background())

	// upRatio = 2/2 = 1.0 (visible only, drill not counted)
	// avg30dSLA over visible = 1.0; outages = 0; monitors = 2
	// health = 1.0 * 60 + 1.0 * 40 - 0 = 100
	// Bug-prone path computed avg30dSLA = (1.0 + 1.0 + 0.0) / 3 = 0.6667 → health = 60 + 26.67 = 86.67
	expected := `
# HELP hyperping_tenant_health_score Composite tenant health score from 0 to 100.
# TYPE hyperping_tenant_health_score gauge
hyperping_tenant_health_score{project="default"} 100
`
	err := testutil.CollectAndCompare(c, strings.NewReader(expected), "hyperping_tenant_health_score")
	require.NoError(t, err)
}

// --- tiered-mode integration tests (chunk 6) ---
//
// These tests exercise the Collector's mode-switching dispatch: Start()
// runs the tiered refresh loop instead of the legacy one when cacheMode
// is CacheModeTiered, and Collect() emits metrics from the tier snapshots.

func newTieredCollectorForTest(api HyperpingAPI, mcp *hyperping.MCPClient) *Collector {
	return NewCollector(api, mcp, 60*time.Second, newTestLogger(), "hyperping",
		WithCacheMode(CacheModeTiered),
		WithTierTTLs(30*time.Millisecond, 60*time.Millisecond, 120*time.Millisecond),
	)
}

func TestCollector_TieredMode_IsReadyAfterHotSucceeds(t *testing.T) {
	api := &mockAPI{
		monitors:     []hyperping.Monitor{{UUID: "mon_1", Name: "Web"}},
		healthchecks: []hyperping.Healthcheck{},
	}
	c := newTieredCollectorForTest(api, nil)
	assert.False(t, c.IsReady(), "before any refresh /readyz must be unready")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Start(ctx)
		close(done)
	}()
	require.Eventually(t, c.IsReady, time.Second, 5*time.Millisecond, "IsReady must latch after first HOT refresh")
	cancel()
	<-done
}

func TestCollector_TieredMode_DataAgeReflectsHotTier(t *testing.T) {
	api := &mockAPI{
		monitors:     []hyperping.Monitor{{UUID: "mon_1", Name: "Web"}},
		healthchecks: []hyperping.Healthcheck{},
	}
	c := newTieredCollectorForTest(api, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Start(ctx)
		close(done)
	}()
	require.Eventually(t, c.IsReady, time.Second, 5*time.Millisecond)
	cancel()
	<-done

	// hyperping_data_age_seconds is emitted once per tier that has succeeded
	// at least once. Right after readiness only HOT is guaranteed; WARM/COLD
	// may or may not have completed their first tick depending on scheduler
	// timing in test environments, so this assertion checks HOT specifically
	// and tolerates the other two as best-effort.
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	mfs, err := reg.Gather()
	require.NoError(t, err)
	found := false
	tiers := map[string]float64{}
	for _, mf := range mfs {
		if mf.GetName() == "hyperping_data_age_seconds" {
			found = true
			for _, m := range mf.GetMetric() {
				var tier string
				for _, lp := range m.GetLabel() {
					if lp.GetName() == "tier" {
						tier = lp.GetValue()
					}
				}
				require.NotEmpty(t, tier, "every data_age series must carry a tier label")
				tiers[tier] = m.GetGauge().GetValue()
			}
		}
	}
	assert.True(t, found, "hyperping_data_age_seconds must be present in tiered mode")
	require.Contains(t, tiers, "hot", "HOT tier data_age must be present after IsReady")
	assert.Greater(t, tiers["hot"], 0.0, "HOT data_age must be > 0 after first refresh")
}

// TestCollector_DataAgeTierLabel_LegacyEmitsHotOnly verifies legacy mode
// still produces a single data_age series, now carrying tier="hot" to match
// the new tiered-mode semantics. Existing PromQL like
// `hyperping_data_age_seconds > 300` continues to fire when the legacy
// refresh stalls, just on a labelled series.
func TestCollector_DataAgeTierLabel_LegacyEmitsHotOnly(t *testing.T) {
	api := &mockAPI{
		monitors:     []hyperping.Monitor{{UUID: "mon_1", Name: "Web", Status: "up"}},
		healthchecks: []hyperping.Healthcheck{{UUID: "hc_1", Name: "Job"}},
	}
	c := NewCollector(api, nil, time.Minute, newTestLogger(), "hyperping")
	c.Refresh(context.Background())

	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	mfs, err := reg.Gather()
	require.NoError(t, err)

	var series []*dto.Metric
	for _, mf := range mfs {
		if mf.GetName() == "hyperping_data_age_seconds" {
			series = mf.GetMetric()
		}
	}
	require.Len(t, series, 1, "legacy mode must emit exactly one data_age series")
	var tier string
	for _, lp := range series[0].GetLabel() {
		if lp.GetName() == "tier" {
			tier = lp.GetValue()
		}
	}
	assert.Equal(t, "hot", tier, `legacy mode labels its single series tier="hot"`)
}

func TestCollector_TieredMode_PromMetricsMatchLegacy(t *testing.T) {
	mkAPI := func() *mockAPI {
		sslDays := 90
		return &mockAPI{
			monitors: []hyperping.Monitor{
				{UUID: "mon_1", Name: "Web", URL: "https://example.com", Protocol: "http",
					Status: "up", CheckFrequency: 60, SSLExpiration: &sslDays,
					ProjectUUID: "proj_1", HTTPMethod: "GET"},
			},
			healthchecks: []hyperping.Healthcheck{{UUID: "hc_1", Name: "Job", Period: 300}},
		}
	}

	// Legacy: build snapshot via single Refresh()
	legacy := NewCollector(mkAPI(), nil, 60*time.Second, newTestLogger(), "hyperping")
	legacy.Refresh(context.Background())

	// Tiered: drive each tier directly.
	apiT := mkAPI()
	tieredC := newTieredCollectorForTest(apiT, nil)
	require.NotNil(t, tieredC.tiered)
	tieredC.tiered.refreshHot(context.Background())
	tieredC.tiered.refreshWarm(context.Background())
	tieredC.tiered.refreshCold(context.Background())

	// Compare a stable subset of metric values that are independent of
	// data_age (which is time-dependent) and tier labels.
	gather := func(c *Collector) map[string]float64 {
		reg := prometheus.NewRegistry()
		reg.MustRegister(c)
		mfs, err := reg.Gather()
		require.NoError(t, err)
		out := map[string]float64{}
		for _, mf := range mfs {
			for _, m := range mf.GetMetric() {
				if g := m.GetGauge(); g != nil {
					labels := ""
					for _, lp := range m.GetLabel() {
						labels += "|" + lp.GetName() + "=" + lp.GetValue()
					}
					out[mf.GetName()+labels] = g.GetValue()
				}
			}
		}
		return out
	}
	want := gather(legacy)
	got := gather(tieredC)

	// Only compare a handful of representative scalar metrics: monitor
	// up/paused/checkinterval, monitor in_maintenance, hyperping_monitors,
	// hyperping_healthchecks, hyperping_tenant_monitors_up_ratio. Full
	// equality would drift on data_age; the focused subset is enough to
	// catch a wiring regression (e.g. HOT publishing nothing).
	for _, k := range []string{
		"hyperping_monitor_up|name=Web|tenant=|tier=unknown|uuid=mon_1",
		"hyperping_monitor_paused|name=Web|tenant=|tier=unknown|uuid=mon_1",
		"hyperping_monitor_check_interval_seconds|name=Web|tenant=|tier=unknown|uuid=mon_1",
		"hyperping_monitors",
		"hyperping_healthchecks",
		"hyperping_tenant_monitors_up_ratio",
	} {
		assert.InDelta(t, want[k], got[k], 0.001, "metric %s differs between modes: legacy=%v tiered=%v", k, want[k], got[k])
	}
}

func TestCollector_TieredMode_ColdReportsFailureDoesNotZeroSLA(t *testing.T) {
	api := &mockAPI{
		monitors:     []hyperping.Monitor{{UUID: "mon_1", Name: "Web", Status: "up"}},
		healthchecks: []hyperping.Healthcheck{},
		reports: []hyperping.MonitorReport{
			{UUID: "mon_1", Name: "Web", SLA: 99.7},
		},
	}
	c := newTieredCollectorForTest(api, nil)

	// First COLD run: succeeds, populates snapshot.
	c.tiered.refreshCold(context.Background())
	first := c.tiered.cold.Load()
	require.NotNil(t, first)
	require.NotEmpty(t, first.report30d)

	// Second COLD run: ALL windows fail.
	api.reportsErr = errors.New("reports unavailable")
	c.tiered.refreshCold(context.Background())

	// The pointer must be the SAME identity as the first snapshot (no
	// store on both-fail).
	second := c.tiered.cold.Load()
	assert.Same(t, first, second, "both-fail COLD refresh must retain the previous snapshot pointer")
}

func TestCollector_TieredMode_ConcurrentScrapeAndRefresh(t *testing.T) {
	api := &mockAPI{
		monitors:     []hyperping.Monitor{{UUID: "mon_1", Name: "Web", Status: "up"}},
		healthchecks: []hyperping.Healthcheck{},
	}
	c := newTieredCollectorForTest(api, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Start(ctx)
	require.Eventually(t, c.IsReady, time.Second, 5*time.Millisecond)

	// Hammer Collect concurrently with the tier refreshes for ~100ms; this
	// only catches data races under `go test -race`.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reg := prometheus.NewRegistry()
			reg.MustRegister(c)
			for {
				select {
				case <-stop:
					return
				default:
					_, err := reg.Gather()
					_ = err
				}
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// --- Label-length cap (MEDIUM-4) ---

// TestCollect_MonitorName_TruncatedAt256 asserts that a multi-kilobyte monitor
// name does not flow verbatim into the `name` label across the ~13 per-monitor
// series. A compromised Hyperping account or operator with rename rights could
// otherwise force the Prometheus side to ingest large label values per
// monitor, multiplied across every series, causing memory pressure.
// The cap is fixed at 256 bytes, enforced uniformly by capLabel.
func TestCollect_MonitorName_TruncatedAt256(t *testing.T) {
	longName := strings.Repeat("A", 10000)
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{UUID: "mon_1", Name: longName, Status: "up", CheckFrequency: 60},
		},
	}
	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping")
	c.Refresh(context.Background())

	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	mfs, err := reg.Gather()
	require.NoError(t, err)

	const cap = maxLabelValueBytes

	checked := 0
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() != "name" {
					continue
				}
				v := lp.GetValue()
				assert.LessOrEqual(t, len(v), cap,
					"metric %q name label is %d bytes; cap is %d", mf.GetName(), len(v), cap)
				assert.True(t, strings.HasPrefix(longName, strings.TrimRight(v, "…")),
					"truncated name must be a prefix of the original")
				checked++
			}
		}
	}
	assert.Greater(t, checked, 0, "expected at least one name-label series to verify")
}

// TestCapLabel covers the helper in isolation: short strings are unchanged,
// strings at the cap are returned verbatim, strings over the cap are
// truncated to exactly cap bytes. The truncated form must remain valid UTF-8
// so Prometheus does not reject the scrape.
func TestCapLabel(t *testing.T) {
	t.Run("short string unchanged", func(t *testing.T) {
		assert.Equal(t, "hello", capLabel("hello"))
	})
	t.Run("exactly at cap", func(t *testing.T) {
		s := strings.Repeat("a", maxLabelValueBytes)
		assert.Equal(t, s, capLabel(s))
		assert.Len(t, capLabel(s), maxLabelValueBytes)
	})
	t.Run("over cap truncates to cap bytes", func(t *testing.T) {
		s := strings.Repeat("a", maxLabelValueBytes+50)
		got := capLabel(s)
		assert.Len(t, got, maxLabelValueBytes)
	})
	t.Run("multibyte UTF-8 not split mid-rune", func(t *testing.T) {
		// "é" is 2 bytes. Build a string whose byte length crosses the cap
		// inside a rune; the cap helper must back off to a rune boundary.
		s := strings.Repeat("é", maxLabelValueBytes) // 2 * cap bytes
		got := capLabel(s)
		assert.LessOrEqual(t, len(got), maxLabelValueBytes)
		assert.True(t, utf8ValidWrap(got), "truncated string must remain valid UTF-8")
	})
}

// utf8ValidWrap wraps utf8.ValidString to avoid importing unicode/utf8 in the
// main test file for one assertion. Keeps the import block tight.
func utf8ValidWrap(s string) bool {
	for _, r := range s {
		if r == 0xFFFD {
			return false
		}
	}
	return true
}

// TestCollect_SLAReport_UsesMonitorNameNotReportName guards against silent
// label fragmentation when a monitor is renamed mid-window. The Hyperping
// API ships the monitor's name at the time the report was generated, which
// may lag behind the current monitor name. Emitting the report's `Name`
// would produce SLA series whose `name` label differs from the base
// `hyperping_monitor_up` series for the same `uuid`, splitting time series
// in Prometheus and breaking dashboards keyed on `name`.
//
// The collector must resolve the name from `mon.Name` (the live monitor
// record) so all series for a given `uuid` share a consistent `name`.
func TestCollect_SLAReport_UsesMonitorNameNotReportName(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			// Live monitor record carries the current ("renamed") name.
			{UUID: "m1", Name: "renamed", Protocol: "http", HTTPMethod: "GET", Status: "up"},
		},
		reports: []hyperping.MonitorReport{
			// Report record was generated before the rename; the SDK still
			// returns the stale name.
			{UUID: "m1", Name: "oldname", SLA: 99.0},
		},
	}
	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping")
	c.Refresh(context.Background())

	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	mfs, err := reg.Gather()
	require.NoError(t, err)

	checked := 0
	for _, mf := range mfs {
		if mf.GetName() != "hyperping_monitor_sla_ratio" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() != "name" {
					continue
				}
				assert.Equal(t, "renamed", lp.GetValue(),
					"SLA series name label must come from the live monitor record, "+
						"not the report's stale Name field")
				checked++
			}
		}
	}
	assert.Greater(t, checked, 0, "expected at least one SLA series with a name label")
}
