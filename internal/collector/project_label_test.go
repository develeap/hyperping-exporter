// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

// Project-label tests (work item: exporter-collector-project-label).
// These tests pin down the contract that every Desc constructed in
// newCollectorDescs, NewClientMetrics, and NewMCPMetrics carries a
// `project` constLabel — including the "unset" path which must collapse
// to project="default" rather than emit an empty label or omit the label.
//
// Why project as a constLabel (not a variable label):
//   - All series for a given Collector instance share one project value
//     by construction; making it a constLabel keeps Describe/Collect
//     identical and avoids widening every WithLabelValues call site.
//   - Two Collectors built with different projects produce *different*
//     Descs (constLabel is part of the Desc identity), so registering
//     both into the same Registry does NOT panic on
//     "duplicate metrics collector registration attempted".
//
// Run order: these tests are intentionally written before the
// implementation lands. `go test` must fail on the missing WithProject
// option / missing project param on NewClientMetrics / NewMCPMetrics.

package collector

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// descContainsProjectLabel returns true if the Desc's String() representation
// carries a `project="<value>"` constLabel. Walking the Desc text is the only
// way to inspect constLabels with the client_golang public API (Desc.String
// is the documented form: fqName, help, variableLabels, constLabels).
func descContainsProjectLabel(d *prometheus.Desc, value string) bool {
	s := d.String()
	return strings.Contains(s, `project="`+value+`"`)
}

// emittedMetricHasLabel collects from c into a channel and reports whether
// every emitted Metric carries project=<expected> as a label-pair on its
// dto.Metric. Empty channel => trivially true; that is acceptable here
// because the Collector under test always emits at least the scrape_*
// gauges from Collect even with an empty cache.
func emittedMetricsAllHaveProject(t *testing.T, c prometheus.Collector, expected string) bool {
	t.Helper()
	ch := make(chan prometheus.Metric, 256)
	go func() {
		c.Collect(ch)
		close(ch)
	}()
	saw := 0
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		found := false
		for _, lp := range pb.Label {
			if lp.GetName() == "project" && lp.GetValue() == expected {
				found = true
				break
			}
		}
		if !found {
			t.Logf("metric without project=%q label: desc=%s labels=%+v", expected, m.Desc().String(), pb.Label)
			return false
		}
		saw++
	}
	return saw > 0
}

// TestNewCollector_AddsProjectConstLabel: NewCollector(..., WithProject(...))
// results in every Desc emitted via Describe carrying constLabel
// project="hyp_core". This is the load-bearing test for the constLabel
// approach — if the implementation accidentally added project as a variable
// label, this test would still pass for Describe but break Collect.
func TestNewCollector_AddsProjectConstLabel(t *testing.T) {
	c := NewCollector(&mockAPI{}, nil, 60*time.Second, newTestLogger(), "hyperping",
		WithProject("hyp_core"),
	)
	ch := make(chan *prometheus.Desc, 64)
	c.Describe(ch)
	close(ch)
	count := 0
	for d := range ch {
		count++
		assert.True(t, descContainsProjectLabel(d, "hyp_core"),
			"every Desc must carry constLabel project=\"hyp_core\"; got %s", d.String())
	}
	assert.Greater(t, count, 0, "Describe must emit at least one Desc")
}

// TestNewCollector_DefaultProjectWhenUnset: WithProject("") or no option
// yields project="default" constLabel. Unconditional emission so dashboards
// and alerts see a stable label set on day 1 (the chart's back-compat single
// project is rendered as projects=[{id: default}]).
func TestNewCollector_DefaultProjectWhenUnset(t *testing.T) {
	cases := []struct {
		name string
		opts []Option
	}{
		{"no WithProject option", nil},
		{"empty string WithProject", []Option{WithProject("")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewCollector(&mockAPI{}, nil, 60*time.Second, newTestLogger(), "hyperping", tc.opts...)
			ch := make(chan *prometheus.Desc, 64)
			c.Describe(ch)
			close(ch)
			for d := range ch {
				assert.True(t, descContainsProjectLabel(d, "default"),
					"unset/empty project must collapse to constLabel project=\"default\"; got %s", d.String())
			}
		})
	}
}

// TestNewCollector_MultipleProjectsNoDescCollision: registering two
// Collectors with project="hyp_core" and project="hyp_infra" into the same
// prometheus.Registry does NOT panic on duplicate Desc. constLabel value is
// part of the Desc identity, so the Descs do not collide.
func TestNewCollector_MultipleProjectsNoDescCollision(t *testing.T) {
	reg := prometheus.NewRegistry()
	c1 := NewCollector(&mockAPI{}, nil, 60*time.Second, newTestLogger(), "hyperping",
		WithProject("hyp_core"))
	c2 := NewCollector(&mockAPI{}, nil, 60*time.Second, newTestLogger(), "hyperping",
		WithProject("hyp_infra"))
	require.NotPanics(t, func() {
		reg.MustRegister(c1)
		reg.MustRegister(c2)
	}, "two Collectors with distinct project constLabels must coexist in one Registry")
}

// TestNewCollector_CollectEmitsProjectLabel verifies that the constLabel is
// actually carried on emitted metrics (not just on Descs). This is the
// dual of the Describe check: an emitted prom.Metric written to a
// dto.Metric carries every constLabel as a LabelPair.
func TestNewCollector_CollectEmitsProjectLabel(t *testing.T) {
	c := NewCollector(&mockAPI{}, nil, 60*time.Second, newTestLogger(), "hyperping",
		WithProject("hyp_infra"),
	)
	// Refresh once so the scrape_* gauges have a value; otherwise Collect
	// only emits build_info etc., which is owned by the registry not the
	// Collector under test.
	c.Refresh(context.Background())
	assert.True(t, emittedMetricsAllHaveProject(t, c, "hyp_infra"),
		"every metric emitted by Collect must carry project=\"hyp_infra\"")
}

// TestClientMetrics_ProjectConstLabel: NewClientMetrics(reg, ns, "hyp_core")
// labels hyperping_client_api_call_duration_seconds (and the rest) with
// constLabel project="hyp_core". The signature gains a trailing project
// string parameter; the implementation MUST default empty to "default".
func TestClientMetrics_ProjectConstLabel(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewClientMetrics(reg, "hyperping", "hyp_core")
	require.NotNil(t, m)

	// Record one observation so the histogram materialises a child.
	m.RecordAPICall(context.Background(), "GET", "/monitors", 200, 0.1)

	mfs, err := reg.Gather()
	require.NoError(t, err)
	found := false
	for _, mf := range mfs {
		if mf.GetName() != "hyperping_client_api_call_duration_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "project" && lp.GetValue() == "hyp_core" {
					found = true
				}
			}
		}
	}
	assert.True(t, found,
		"hyperping_client_api_call_duration_seconds must carry project=\"hyp_core\" constLabel")
}

// TestMCPMetrics_ProjectConstLabel: NewMCPMetrics(reg, ns, "hyp_core")
// labels every mcp_* metric with project="hyp_core". Same shape as the
// client-metrics test: the constructor signature gains a project string.
func TestMCPMetrics_ProjectConstLabel(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMCPMetrics(reg, "hyperping", "hyp_core")
	require.NotNil(t, m)
	m.InitializeTotal.Inc()
	m.SessionLostTotal.Inc()
	m.PartialRefreshTotal.Inc()
	m.CallRateLimited.WithLabelValues("getMonitor").Inc()

	mfs, err := reg.Gather()
	require.NoError(t, err)
	want := map[string]bool{
		"hyperping_mcp_initialize_total":        false,
		"hyperping_mcp_session_lost_total":      false,
		"hyperping_mcp_call_rate_limited_total": false,
		"hyperping_mcp_partial_refresh_total":   false,
	}
	for _, mf := range mfs {
		if _, ok := want[mf.GetName()]; !ok {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "project" && lp.GetValue() == "hyp_core" {
					want[mf.GetName()] = true
				}
			}
		}
	}
	for name, ok := range want {
		assert.True(t, ok, "%s must carry project=\"hyp_core\" constLabel", name)
	}
}

// TestCollector_ExtractTenantUnchanged: extractTenant behavior identical
// before/after the project-label change (regression guard on the
// L1257-1270 helper). Adding project as a constLabel must not silently
// touch the tenant extraction logic that drives the existing `tenant`
// variable label.
func TestCollector_ExtractTenantUnchanged(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"valid bracketed prefix", "[acme-prod]-CheckoutAPI", "acme-prod"},
		{"no leading bracket", "CheckoutAPI", ""},
		{"unmatched bracket", "[acme-CheckoutAPI", ""},
		{"contains space inside brackets", "[acme prod]-CheckoutAPI", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractTenant(tc.in)
			assert.Equal(t, tc.want, got)
		})
	}
}

// --- shared test doubles -------------------------------------------------
// These tests reuse the file-local mockAPI from collector_test.go (same
// package), so no additional fake is needed here. The mockAPI's zero value
// returns empty slices and nil errors which is exactly what these tests
// want: they care about Desc shape and emitted label sets, not refresh
// content.
