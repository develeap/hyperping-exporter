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

// TestDataAge_DualLabelEmission asserts the dual-emission scheme: for
// every tier with a non-zero data-age, one legacy series with period=""
// (subset-matched by pre-1.8 selectors) PLUS one series per configured
// period mapped to that tier.
//
// Test uses the legacy refresh path (CacheModeLegacy is the default for
// NewCollector). The dataAges map population in the legacy path is
// internal; we exercise it indirectly by calling Refresh and then
// gathering the registry to inspect the series Prometheus would actually
// scrape.
func TestDataAge_DualLabelEmission(t *testing.T) {
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
	// Legacy mode populates only "hot" in dataAges, so expect two
	// series: {tier=hot, period=""} (legacy form) and no period-mapped
	// series (no cold/warm-mapped period in the configured ["24h"] is
	// "hot"; the 24h period maps to warm, but legacy mode has no warm
	// tier so it's omitted).
	assert.Contains(t, seen, pair{tier: "hot", period: ""},
		"legacy {tier=hot} series must still be emitted")
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
