// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package collector

import (
	"context"
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
