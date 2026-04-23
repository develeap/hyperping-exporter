// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package collector

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	hyperping "github.com/develeap/hyperping-go"
)

// mockAPI implements HyperpingAPI for testing.
type mockAPI struct {
	monitors           []hyperping.Monitor
	healthchecks       []hyperping.Healthcheck
	outages            []hyperping.Outage
	reports            []hyperping.MonitorReport
	maintenanceWindows []hyperping.Maintenance
	incidents          []hyperping.Incident
	monitorsErr        error
	healthchecksErr    error
	outagesErr         error
	reportsErr         error
	maintenanceErr     error
	incidentsErr       error
}

func (m *mockAPI) ListMonitors(_ context.Context) ([]hyperping.Monitor, error) {
	return m.monitors, m.monitorsErr
}

func (m *mockAPI) ListHealthchecks(_ context.Context) ([]hyperping.Healthcheck, error) {
	return m.healthchecks, m.healthchecksErr
}

func (m *mockAPI) ListOutages(_ context.Context) ([]hyperping.Outage, error) {
	return m.outages, m.outagesErr
}

func (m *mockAPI) ListMonitorReports(_ context.Context, _, _ string) ([]hyperping.MonitorReport, error) {
	return m.reports, m.reportsErr
}

func (m *mockAPI) ListMaintenance(_ context.Context) ([]hyperping.Maintenance, error) {
	return m.maintenanceWindows, m.maintenanceErr
}

func (m *mockAPI) ListIncidents(_ context.Context) ([]hyperping.Incident, error) {
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

	ch := make(chan *prometheus.Desc, 40)
	c.Describe(ch)
	close(ch)

	var descs []*prometheus.Desc
	for d := range ch {
		descs = append(descs, d)
	}
	// 29 previous + 5 MCP (EXP-01) = 34
	assert.Len(t, descs, 34)
}

func TestRefresh_Success(t *testing.T) {
	sslDays := 90
	api := &mockAPI{
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

	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping")
	c.Refresh(context.Background())

	assert.True(t, c.IsReady())
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
	api := &mockAPI{
		monitors:     []hyperping.Monitor{{UUID: "mon_1", Name: "Web", HTTPMethod: "GET", Status: "up"}},
		healthchecks: []hyperping.Healthcheck{},
		outagesErr:   errors.New("outage api error"),
	}

	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping")
	c.Refresh(context.Background())

	assert.True(t, c.IsReady())
}

func TestRefresh_MaintenanceErrorIsNonFatal(t *testing.T) {
	// Maintenance API failures should not mark the scrape as failed; stale data is retained.
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

	// Second refresh: maintenance API fails; stale window data should be retained.
	api.maintenanceErr = errors.New("maintenance api error")
	api.maintenanceWindows = nil
	c.Refresh(context.Background())

	assert.True(t, c.IsReady(), "core scrape success must keep collector ready")

	// Monitor should still show in_maintenance=1 from the retained stale window.
	expected := `
# HELP hyperping_monitor_in_maintenance 1 if the monitor is currently covered by an active maintenance window, 0 otherwise.
# TYPE hyperping_monitor_in_maintenance gauge
hyperping_monitor_in_maintenance{name="Web",tenant="",tier="unknown",uuid="mon_1"} 1
`
	err := testutil.CollectAndCompare(c, strings.NewReader(expected), "hyperping_monitor_in_maintenance")
	require.NoError(t, err)
}

func TestRefresh_IncidentErrorIsNonFatal(t *testing.T) {
	// Incident API failures should not mark the scrape as failed; stale data is retained.
	api := &mockAPI{
		monitors:     []hyperping.Monitor{},
		healthchecks: []hyperping.Healthcheck{},
		incidents:    []hyperping.Incident{{UUID: "i1", Type: "investigating"}},
	}

	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping")
	c.Refresh(context.Background())
	require.True(t, c.IsReady())

	// Second refresh: incidents API fails; stale count should be retained.
	api.incidentsErr = errors.New("incidents api error")
	api.incidents = nil
	c.Refresh(context.Background())

	assert.True(t, c.IsReady(), "core scrape success must keep collector ready")

	expected := `
# HELP hyperping_incidents_open Number of open (non-resolved) incidents.
# TYPE hyperping_incidents_open gauge
hyperping_incidents_open 1
`
	err := testutil.CollectAndCompare(c, strings.NewReader(expected), "hyperping_incidents_open")
	require.NoError(t, err)
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
	// Summary: 4, Tenant: up_ratio + active_outages + data_age + incidents_open + maintenance_windows_active = 5
	// Total: 8 + 4 + 5 = 17
	count := testutil.CollectAndCount(c)
	assert.Equal(t, 18, count)
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
	// Tenant: up_ratio + active_outages + data_age + incidents_open + maintenance_windows_active = 5
	// Total = 29
	count := testutil.CollectAndCount(c)
	assert.Equal(t, 30, count)
}

func TestCollect_EmptyCache(t *testing.T) {
	c := NewCollector(&mockAPI{}, nil, 60*time.Second, newTestLogger(), "hyperping")

	// No refresh: 4 summary + 4 tenant (up_ratio + active_outages + incidents_open + maintenance_windows_active;
	// no data_age, no health_score) = 8
	count := testutil.CollectAndCount(c)
	assert.Equal(t, 9, count)
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
	// Summary: 4, Tenant: up_ratio + active_outages + data_age + incidents_open + maintenance_windows_active = 5
	// Total = 17
	count := testutil.CollectAndCount(c)
	assert.Equal(t, 18, count)
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
hyperping_monitors 2
# HELP hyperping_healthchecks Total number of healthchecks.
# TYPE hyperping_healthchecks gauge
hyperping_healthchecks 1
# HELP hyperping_scrape_success Whether the last API scrape succeeded (1) or failed (0).
# TYPE hyperping_scrape_success gauge
hyperping_scrape_success 1
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
hyperping_monitor_up{name="Web",tenant="",tier="unknown",uuid="mon_1"} 1
# HELP hyperping_monitor_paused Whether the monitor is paused (1) or active (0).
# TYPE hyperping_monitor_paused gauge
hyperping_monitor_paused{name="Web",tenant="",tier="unknown",uuid="mon_1"} 0
# HELP hyperping_monitor_check_interval_seconds Monitor check frequency in seconds.
# TYPE hyperping_monitor_check_interval_seconds gauge
hyperping_monitor_check_interval_seconds{name="Web",tenant="",tier="unknown",uuid="mon_1"} 120
# HELP hyperping_monitor_ssl_expiration_days Days until SSL certificate expiration.
# TYPE hyperping_monitor_ssl_expiration_days gauge
hyperping_monitor_ssl_expiration_days{name="Web",tenant="",tier="unknown",uuid="mon_1"} 45
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
hyperping_healthcheck_up{name="Daily Backup",uuid="tok_1"} 0
hyperping_healthcheck_up{name="Hourly Sync",uuid="tok_2"} 1
# HELP hyperping_healthcheck_paused Whether the healthcheck is paused (1) or active (0).
# TYPE hyperping_healthcheck_paused gauge
hyperping_healthcheck_paused{name="Daily Backup",uuid="tok_1"} 0
hyperping_healthcheck_paused{name="Hourly Sync",uuid="tok_2"} 1
# HELP hyperping_healthcheck_period_seconds Expected healthcheck ping period in seconds.
# TYPE hyperping_healthcheck_period_seconds gauge
hyperping_healthcheck_period_seconds{name="Daily Backup",uuid="tok_1"} 86400
hyperping_healthcheck_period_seconds{name="Hourly Sync",uuid="tok_2"} 3600
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
hyperping_scrape_success 0
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
hyperping_monitor_outage_active{name="API",tenant="",tier="unknown",uuid="mon_2"} 0
hyperping_monitor_outage_active{name="Web",tenant="",tier="unknown",uuid="mon_1"} 1
# HELP hyperping_monitor_active_outage_status_code HTTP status code of the current active outage; 0 when no active outage.
# TYPE hyperping_monitor_active_outage_status_code gauge
hyperping_monitor_active_outage_status_code{name="API",tenant="",tier="unknown",uuid="mon_2"} 0
hyperping_monitor_active_outage_status_code{name="Web",tenant="",tier="unknown",uuid="mon_1"} 503
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
hyperping_monitor_escalation_tier{name="Core",tier="core",uuid="mon_1"} 1
hyperping_monitor_escalation_tier{name="Edge",tier="unknown",uuid="mon_2"} 1
hyperping_monitor_escalation_tier{name="NonCore",tier="noncore",uuid="mon_3"} 1
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
hyperping_monitor_sla_ratio{name="Web",period="24h",tenant="",tier="unknown",uuid="mon_1"} 0.995
hyperping_monitor_sla_ratio{name="Web",period="7d",tenant="",tier="unknown",uuid="mon_1"} 0.995
hyperping_monitor_sla_ratio{name="Web",period="30d",tenant="",tier="unknown",uuid="mon_1"} 0.995
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
hyperping_tenant_monitors_up_ratio 1
# HELP hyperping_tenant_active_outages Total number of active (unresolved) outages across all monitors.
# TYPE hyperping_tenant_active_outages gauge
hyperping_tenant_active_outages 0
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
func (a *refreshCountingAPI) ListOutages(ctx context.Context) ([]hyperping.Outage, error) {
	return a.inner.ListOutages(ctx)
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
	assert.Equal(t, 0.0, avgSLAForPeriod(nil))
	assert.Equal(t, 0.0, avgSLAForPeriod([]hyperping.MonitorReport{}))
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
	m := NewClientMetrics(reg, "hyperping")
	require.NotNil(t, m)

	// Seed an observation so the histogram appears in Gather output.
	m.RecordAPICall(context.Background(), "GET", "/test", 200, 0.01)

	mfs, err := reg.Gather()
	require.NoError(t, err)
	assert.NotEmpty(t, mfs)
}

func TestClientMetrics_RecordAPICall(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewClientMetrics(reg, "hyperping")

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
	m := NewClientMetrics(reg, "hyperping")

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
	m := NewClientMetrics(reg, "hyperping")

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
hyperping_monitor_in_maintenance{name="API",tenant="",tier="unknown",uuid="m2"} 0
hyperping_monitor_in_maintenance{name="Web",tenant="",tier="unknown",uuid="m1"} 1
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
hyperping_monitor_up_by_region{name="Web",region="frankfurt",tenant="",tier="unknown",uuid="m1"} 1
hyperping_monitor_up_by_region{name="Web",region="london",tenant="",tier="unknown",uuid="m1"} 0
hyperping_monitor_up_by_region{name="Web",region="paris",tenant="",tier="unknown",uuid="m1"} 0
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
hyperping_incidents_open 2
# HELP hyperping_maintenance_windows_active Number of currently active (ongoing) maintenance windows.
# TYPE hyperping_maintenance_windows_active gauge
hyperping_maintenance_windows_active 2
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
	if toolName == "get_monitor_response_time" || toolName == "get_monitor_mtta" || toolName == "get_monitor_anomalies" {
		uuid := args["uuid"].(string)
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
			"get_monitor_response_time:mon_1": map[string]any{"uuid": "mon_1", "avg": 0.123},
			"get_monitor_mtta:mon_1":         map[string]any{"uuid": "mon_1", "avg_wait": 45.0},
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
# HELP hyperping_alerts_total Total number of alerts in history.
# TYPE hyperping_alerts_total counter
hyperping_alerts_total 42
# HELP hyperping_monitor_anomaly_count Number of detected anomalies for the monitor.
# TYPE hyperping_monitor_anomaly_count gauge
hyperping_monitor_anomaly_count{name="Web",tenant="",tier="unknown",uuid="mon_1"} 2
# HELP hyperping_monitor_anomaly_score Highest anomaly score for the monitor.
# TYPE hyperping_monitor_anomaly_score gauge
hyperping_monitor_anomaly_score{name="Web",tenant="",tier="unknown",uuid="mon_1"} 0.95
# HELP hyperping_monitor_mtta_seconds Mean Time To Acknowledge in seconds.
# TYPE hyperping_monitor_mtta_seconds gauge
hyperping_monitor_mtta_seconds{name="Web",tenant="",tier="unknown",uuid="mon_1"} 45
# HELP hyperping_monitor_response_time_avg_seconds Average monitor response time in seconds.
# TYPE hyperping_monitor_response_time_avg_seconds gauge
hyperping_monitor_response_time_avg_seconds{name="Web",tenant="",tier="unknown",uuid="mon_1"} 0.123
`

	err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"hyperping_alerts_total",
		"hyperping_monitor_response_time_avg_seconds",
		"hyperping_monitor_mtta_seconds",
		"hyperping_monitor_anomaly_count",
		"hyperping_monitor_anomaly_score",
	)
	assert.NoError(t, err)
}
