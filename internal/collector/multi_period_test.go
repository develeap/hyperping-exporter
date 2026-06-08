// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package collector

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	hyperping "github.com/develeap/hyperping-go"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMtta_DefaultPeriodLabel verifies the documented v1.8.0 breaking
// change: hyperping_monitor_mtta_seconds now ALWAYS carries a period
// label, defaulting to "24h" for projects that did not opt into
// additional windows. The series identity changes versus pre-1.8 (no
// period label) and that change must be visible at the Prometheus
// exposition layer.
func TestMtta_DefaultPeriodLabel(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{UUID: "mon_1", Name: "Web", Status: "up"},
		},
	}
	transport := &mockMCPTransport{
		results: map[string]any{
			"list_recent_alerts":             map[string]any{"total": 0},
			"get_monitor_response_time:mon_1": map[string]any{"avgResponseTime": 0.0},
			"get_monitor_mtta:mon_1":         map[string]any{"mtta": 12.5},
			"get_monitor_anomalies:mon_1":    map[string]any{"anomalies": []any{}},
		},
	}
	mcp := hyperping.NewMCPClient(transport)
	c := NewCollector(api, mcp, 60*time.Second, newTestLogger(), "hyperping")
	c.Refresh(context.Background())

	expected := `
# HELP hyperping_monitor_mtta_seconds Mean Time To Acknowledge in seconds over the labelled period. v1.8.0 BREAKING: a period label is now ALWAYS present on this metric, defaulting to "24h" for projects that do not opt into additional windows.
# TYPE hyperping_monitor_mtta_seconds gauge
hyperping_monitor_mtta_seconds{name="Web",period="24h",project="default",tenant="",tier="unknown",uuid="mon_1"} 12.5
`
	err := testutil.CollectAndCompare(c, strings.NewReader(expected), "hyperping_monitor_mtta_seconds")
	require.NoError(t, err)
}

// TestMtta_PeriodLabelOnAllSeries asserts that across a multi-period
// configuration, every emitted mtta_seconds series carries a non-empty
// period label. A regression where the period label silently empties
// (e.g. via a future Desc tweak) would silently degrade dashboards.
func TestMtta_PeriodLabelOnAllSeries(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{UUID: "mon_1", Name: "Web", Status: "up"},
		},
	}
	transport := &mockMCPTransport{
		results: map[string]any{
			"list_recent_alerts":             map[string]any{"total": 0},
			"get_monitor_response_time:mon_1": map[string]any{"avgResponseTime": 0.0},
			"get_monitor_mtta:mon_1":         map[string]any{"mtta": 17.0},
			"get_monitor_anomalies:mon_1":    map[string]any{"anomalies": []any{}},
		},
	}
	mcp := hyperping.NewMCPClient(transport)
	c := NewCollector(api, mcp, 60*time.Second, newTestLogger(), "hyperping",
		WithPeriods([]string{"24h"}),
	)
	c.Refresh(context.Background())

	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(c))
	mfs, err := reg.Gather()
	require.NoError(t, err)
	var seenMtta bool
	for _, mf := range mfs {
		if mf.GetName() != "hyperping_monitor_mtta_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			seenMtta = true
			period := labelValue(m, "period")
			assert.NotEmpty(t, period, "every mtta_seconds series must carry a non-empty period label")
		}
	}
	assert.True(t, seenMtta, "expected at least one mtta_seconds series")
}

// TestDataAge_SingleSeriesPerTierPeriod asserts the v1.8.0 single-emit
// scheme: HOT tier emits exactly one series with period="" (no window),
// warm/cold tiers emit one series per configured period that maps to
// them, and no legacy empty-period series is emitted for warm/cold.
//
// Test uses the legacy refresh path (CacheModeLegacy is the default for
// NewCollector). Legacy populates only "hot" in dataAges, so the
// expected series set is exactly {tier="hot", period=""}.
func TestDataAge_SingleSeriesPerTierPeriod(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{UUID: "mon_1", Name: "Web", Status: "up"},
		},
	}
	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping",
		WithPeriods([]string{"24h"}),
	)
	c.Refresh(context.Background())

	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(c))
	mfs, err := reg.Gather()
	require.NoError(t, err)

	type pair struct{ tier, period string }
	var seen []pair
	for _, mf := range mfs {
		if mf.GetName() != "hyperping_data_age_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			seen = append(seen, pair{
				tier:   labelValue(m, "tier"),
				period: labelValue(m, "period"),
			})
		}
	}
	// Legacy populates only "hot" so we expect exactly one series.
	assert.ElementsMatch(t, []pair{{tier: "hot", period: ""}}, seen,
		"legacy mode must emit exactly one data_age series: {tier=hot, period=\"\"}")
}

// TestDataAge_NoOrphanEmptyPeriodForWarmCold asserts that for the default
// `periods=["24h"]`, neither warm nor cold tier emits an empty-period
// legacy series. The fix in v1.8.0 removes the dual-emission that caused
// `sum(data_age_seconds{tier="warm"})` to silently double-count.
func TestDataAge_NoOrphanEmptyPeriodForWarmCold(t *testing.T) {
	api := &mockAPI{
		monitors:     []hyperping.Monitor{{UUID: "mon_1", Name: "Web", Status: "up"}},
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
	// Drive the warm + cold tier ticks by waiting a moment for the test
	// refresher to publish; tiered_test uses sub-second TTLs so this is
	// fast in practice.
	require.Eventually(t, func() bool {
		reg := prometheus.NewRegistry()
		if err := reg.Register(c); err != nil {
			return false
		}
		mfs, _ := reg.Gather()
		for _, mf := range mfs {
			if mf.GetName() != "hyperping_data_age_seconds" {
				continue
			}
			for _, m := range mf.GetMetric() {
				if labelValue(m, "tier") == "warm" {
					return true
				}
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond, "warm tier must publish data_age within 2s")
	cancel()
	<-done

	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(c))
	mfs, err := reg.Gather()
	require.NoError(t, err)

	type pair struct{ tier, period string }
	var warmSeries []pair
	for _, mf := range mfs {
		if mf.GetName() != "hyperping_data_age_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			tier := labelValue(m, "tier")
			if tier != "warm" {
				continue
			}
			warmSeries = append(warmSeries, pair{tier: tier, period: labelValue(m, "period")})
		}
	}
	// Default test config has periods=["24h"] (warm-mapped). Expect
	// exactly one warm series labeled period="24h", and ZERO with
	// period="". This guards against the dual-emit regression.
	assert.ElementsMatch(t, []pair{{tier: "warm", period: "24h"}}, warmSeries,
		"warm tier must emit exactly one {period=24h} series and NO orphan {period=\"\"} series")
}

// labelValue returns the named label's value from a dto.Metric, or "" if
// absent. Tiny helper kept local so the test file does not depend on
// any sibling test util.
func labelValue(m *dto.Metric, name string) string {
	for _, lp := range m.GetLabel() {
		if lp.GetName() == name {
			return lp.GetValue()
		}
	}
	return ""
}

// TestRegistry_DefaultConfigDescSetParity is the v1.8.0 registry-surface
// regression guard. For the default config (WithPeriods unset is the
// chart-1.7.x behaviour; the loader collapses to ["24h"]) the Prometheus
// Desc set a single Collector publishes must equal the v1.7.x set EXCEPT
// for the documented `mtta_seconds` change (period label gained) and the
// `data_age_seconds` change (period label gained on its label set).
//
// The test pins every fully-qualified Desc string in a sorted snapshot
// so a future stray label addition to ANY emitted metric fails here. The
// pattern mirrors per_project_tier_disabled_metric_test.go's
// `collectDescs`-driven assertion; the difference is this guard is
// total (full set equality), not subset (one added Desc).
//
// When this test fails as part of an intentional metric-surface change,
// the fix is two-fold: (1) re-run with -v, copy the actual snapshot from
// the failure diff into the wantDescs literal below, (2) document the
// label addition/removal in the chart CHANGELOG and the PR body.
func TestRegistry_DefaultConfigDescSetParity(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{{UUID: "mon_1", Name: "Web", Status: "up"}},
	}
	// Default config path: callers that do not pass WithPeriods get the
	// historical ["24h","7d","30d"] fan-out per the doc on NewCollector,
	// which still produces the same Desc identity set (the period label
	// is a runtime label, not part of Desc identity). To pin the chart-
	// default rendering precisely we pass WithPeriods(["24h"]) so the
	// test mirrors the projects-file path that always supplies it.
	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping",
		WithPeriods([]string{"24h"}),
	)

	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(c))

	descCh := make(chan *prometheus.Desc, 256)
	go func() {
		reg.Describe(descCh)
		close(descCh)
	}()
	var got []string
	for d := range descCh {
		got = append(got, d.String())
	}
	sort.Strings(got)

	// Pinned snapshot of the v1.8.0 default-config Desc set, sorted. The
	// labels list inside each Desc.String() is the load-bearing field;
	// the help text is included verbatim because Desc.String() emits it
	// and dropping it would weaken the guard (a HELP rewording also
	// constitutes a documented metric-surface change).
	wantDescs := []string{
		`Desc{fqName: "hyperping_alerts", help: "Snapshot count of alerts in history.", constLabels: {project="default"}, variableLabels: {}}`,
		`Desc{fqName: "hyperping_cache_ttl_seconds", help: "Cache refresh interval in seconds (value of --cache-ttl).", constLabels: {project="default"}, variableLabels: {}}`,
		`Desc{fqName: "hyperping_data_age_seconds", help: "Seconds elapsed since the last successful API cache refresh, labelled by tier and period. In legacy mode and tiered-mode hot tier (which serve data without a window) the series is emitted once per active tier with ` + "`" + `period=\"\"` + "`" + `. For warm and cold tiers the metric is emitted once per configured period that maps to the tier (24h -> warm; 7d/30d/90d/365d -> cold), so operators can write ` + "`" + `max(data_age_seconds{period=\"30d\"})` + "`" + `-style queries. v1.8.0 BREAKING: the prior empty-period legacy series for warm/cold tiers has been removed; aggregations like ` + "`" + `sum(data_age_seconds{tier=\"warm\"})` + "`" + ` now sum across the configured periods mapped to that tier instead of double-counting a legacy series.", constLabels: {project="default"}, variableLabels: {tier,period}}`,
		`Desc{fqName: "hyperping_excluded_monitors", help: "Number of monitors filtered out by --exclude-name-pattern on the last cache refresh; hyperping_monitors counts the visible remainder.", constLabels: {project="default"}, variableLabels: {}}`,
		`Desc{fqName: "hyperping_healthcheck_paused", help: "Whether the healthcheck is paused (1) or active (0).", constLabels: {project="default"}, variableLabels: {uuid,name}}`,
		`Desc{fqName: "hyperping_healthcheck_period_seconds", help: "Expected healthcheck ping period in seconds.", constLabels: {project="default"}, variableLabels: {uuid,name}}`,
		`Desc{fqName: "hyperping_healthcheck_up", help: "Whether the healthcheck is up (1) or down (0).", constLabels: {project="default"}, variableLabels: {uuid,name}}`,
		`Desc{fqName: "hyperping_healthchecks", help: "Total number of healthchecks.", constLabels: {project="default"}, variableLabels: {}}`,
		`Desc{fqName: "hyperping_incidents_open", help: "Number of open (non-resolved) incidents.", constLabels: {project="default"}, variableLabels: {}}`,
		`Desc{fqName: "hyperping_maintenance_windows_active", help: "Number of currently active (ongoing) maintenance windows.", constLabels: {project="default"}, variableLabels: {}}`,
		`Desc{fqName: "hyperping_monitor_active_outage_status_code", help: "HTTP status code of the current active outage; 0 when no active outage.", constLabels: {project="default"}, variableLabels: {uuid,name,tenant,tier}}`,
		`Desc{fqName: "hyperping_monitor_anomaly_count", help: "Number of detected anomalies for the monitor.", constLabels: {project="default"}, variableLabels: {uuid,name,tenant,tier}}`,
		`Desc{fqName: "hyperping_monitor_anomaly_score", help: "Highest anomaly score for the monitor.", constLabels: {project="default"}, variableLabels: {uuid,name,tenant,tier}}`,
		`Desc{fqName: "hyperping_monitor_check_interval_seconds", help: "Monitor check frequency in seconds.", constLabels: {project="default"}, variableLabels: {uuid,name,tenant,tier}}`,
		`Desc{fqName: "hyperping_monitor_downtime_seconds", help: "Total downtime in seconds over the labelled period.", constLabels: {project="default"}, variableLabels: {uuid,name,tenant,tier,period}}`,
		`Desc{fqName: "hyperping_monitor_escalation_tier", help: "Escalation tier info (always 1). Join on uuid+name; use tier label to filter core/noncore.", constLabels: {project="default"}, variableLabels: {uuid,name,tier}}`,
		`Desc{fqName: "hyperping_monitor_in_maintenance", help: "1 if the monitor is currently covered by an active maintenance window, 0 otherwise.", constLabels: {project="default"}, variableLabels: {uuid,name,tenant,tier}}`,
		`Desc{fqName: "hyperping_monitor_info", help: "Monitor metadata (value is always 1).", constLabels: {project="default"}, variableLabels: {uuid,name,protocol,url,project_uuid,http_method}}`,
		`Desc{fqName: "hyperping_monitor_longest_outage_seconds", help: "Duration of the longest single outage in seconds over the labelled period.", constLabels: {project="default"}, variableLabels: {uuid,name,tenant,tier,period}}`,
		`Desc{fqName: "hyperping_monitor_mtta_seconds", help: "Mean Time To Acknowledge in seconds over the labelled period. v1.8.0 BREAKING: a period label is now ALWAYS present on this metric, defaulting to \"24h\" for projects that do not opt into additional windows.", constLabels: {project="default"}, variableLabels: {uuid,name,tenant,tier,period}}`,
		`Desc{fqName: "hyperping_monitor_mttr_seconds", help: "Mean Time To Recovery in seconds over the labelled period.", constLabels: {project="default"}, variableLabels: {uuid,name,tenant,tier,period}}`,
		`Desc{fqName: "hyperping_monitor_outage_active", help: "Whether the monitor has an active (unresolved) outage (1) or not (0).", constLabels: {project="default"}, variableLabels: {uuid,name,tenant,tier}}`,
		`Desc{fqName: "hyperping_monitor_outages", help: "Number of outages over the labelled period.", constLabels: {project="default"}, variableLabels: {uuid,name,tenant,tier,period}}`,
		`Desc{fqName: "hyperping_monitor_paused", help: "Whether the monitor is paused (1) or active (0).", constLabels: {project="default"}, variableLabels: {uuid,name,tenant,tier}}`,
		`Desc{fqName: "hyperping_monitor_response_time_seconds", help: "Average monitor response time in seconds.", constLabels: {project="default"}, variableLabels: {uuid,name,tenant,tier}}`,
		`Desc{fqName: "hyperping_monitor_sla_ratio", help: "Monitor SLA as a ratio (0–1) over the labelled period.", constLabels: {project="default"}, variableLabels: {uuid,name,tenant,tier,period}}`,
		`Desc{fqName: "hyperping_monitor_ssl_expiration_days", help: "Days until SSL certificate expiration.", constLabels: {project="default"}, variableLabels: {uuid,name,tenant,tier}}`,
		`Desc{fqName: "hyperping_monitor_up", help: "Whether the monitor is up (1) or down (0).", constLabels: {project="default"}, variableLabels: {uuid,name,tenant,tier}}`,
		`Desc{fqName: "hyperping_monitor_up_by_region", help: "1 if the monitor is up in the given region, 0 if confirmed down. Derived from active outage confirmed locations; approximation only.", constLabels: {project="default"}, variableLabels: {uuid,name,tenant,tier,region}}`,
		`Desc{fqName: "hyperping_monitors", help: "Total number of monitors.", constLabels: {project="default"}, variableLabels: {}}`,
		`Desc{fqName: "hyperping_scrape_duration_seconds", help: "Duration of the last API scrape in seconds.", constLabels: {project="default"}, variableLabels: {}}`,
		`Desc{fqName: "hyperping_scrape_success", help: "Whether the last API scrape succeeded (1) or failed (0).", constLabels: {project="default"}, variableLabels: {}}`,
		`Desc{fqName: "hyperping_tenant_active_outages", help: "Total number of active (unresolved) outages across all monitors.", constLabels: {project="default"}, variableLabels: {}}`,
		`Desc{fqName: "hyperping_tenant_avg_sla_ratio", help: "Average SLA ratio across all monitors for the labelled period.", constLabels: {project="default"}, variableLabels: {period}}`,
		`Desc{fqName: "hyperping_tenant_health_score", help: "Composite tenant health score from 0 to 100.", constLabels: {project="default"}, variableLabels: {}}`,
		`Desc{fqName: "hyperping_tenant_monitors_up_ratio", help: "Fraction of monitors currently up (0–1).", constLabels: {project="default"}, variableLabels: {}}`,
	}
	sort.Strings(wantDescs)

	assert.Equal(t, wantDescs, got,
		"registry Desc set drift: any addition/removal/label-change to an emitted metric requires updating wantDescs + the chart CHANGELOG")
}
