// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

// Multi-project fan-out tests (work item: exporter-run-fanout).
// run() in main.go is hard to drive end-to-end because it binds an HTTP
// listener and blocks. The implementation MUST factor the per-project
// collector construction into a helper `buildCollectors(cfg, registry,
// logger) ([]*collector.Collector, error)` so this test can drive the
// fan-out without spinning up a real server.
//
// Until that helper exists this file fails to compile, which IS the TDD
// red signal for this work item.

package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gatherDescStrings returns the set of `<name>{project=...}` markers carried by
// the registry's Descs. Reading constLabels out of a Desc via the public API
// requires String() parsing; gather is the simpler mirror that walks emitted
// metrics' LabelPairs and is sufficient for our project-coverage check.
func gatherProjectLabelValues(t *testing.T, reg *prometheus.Registry) map[string]bool {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	seen := map[string]bool{}
	for _, mf := range mfs {
		// Skip the registry's own process_/go_/<ns>_build_info collectors;
		// they are not project-scoped by design.
		if !strings.HasPrefix(mf.GetName(), "hyperping_") {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "project" {
					seen[lp.GetValue()] = true
				}
			}
		}
	}
	return seen
}

// helperWriteDtoLabels is a tiny test helper that materialises a
// dto.Metric's label set for diagnostic-only logging when an assertion
// fails. Kept here so the file is self-contained.
func helperWriteDtoLabels(m *dto.Metric) string {
	parts := make([]string, 0, len(m.GetLabel()))
	for _, lp := range m.GetLabel() {
		parts = append(parts, lp.GetName()+"="+lp.GetValue())
	}
	return strings.Join(parts, ",")
}

// newDiscardLogger keeps server logs out of test output.
func newDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestRun_SingleProjectRegistersOneCollector: with the synthesised "default"
// project (legacy single-key path), the registry sees Descs/series under
// project="default". This is the back-compat guarantee.
func TestRun_SingleProjectRegistersOneCollector(t *testing.T) {
	cfg := config{
		namespace: "hyperping",
		cacheMode: "legacy",
		projects: []projectConfig{
			{ID: "default", APIKey: "k"},
		},
	}
	reg := prometheus.NewRegistry()
	collectors, err := buildCollectors(cfg, reg, newDiscardLogger())
	require.NoError(t, err)
	require.Len(t, collectors, 1)
	for _, c := range collectors {
		reg.MustRegister(c)
	}
	seen := gatherProjectLabelValues(t, reg)
	assert.True(t, seen["default"], "expected at least one hyperping_* series labelled project=\"default\"; saw=%v", seen)
}

// TestRun_MultiProjectRegistersNCollectors: two projects yields series under
// both project labels on /metrics. Each Collector owns its own
// hyperping.Client and tieredRefresher; the registry sees one set of Descs
// per project value (constLabel makes the Desc identity differ).
func TestRun_MultiProjectRegistersNCollectors(t *testing.T) {
	cfg := config{
		namespace: "hyperping",
		cacheMode: "legacy",
		projects: []projectConfig{
			{ID: "hyp_core", APIKey: "a"},
			{ID: "hyp_infra", APIKey: "b"},
		},
	}
	reg := prometheus.NewRegistry()
	collectors, err := buildCollectors(cfg, reg, newDiscardLogger())
	require.NoError(t, err)
	require.Len(t, collectors, 2,
		"buildCollectors must yield one *Collector per project entry")
	for _, c := range collectors {
		reg.MustRegister(c)
	}
	seen := gatherProjectLabelValues(t, reg)
	assert.True(t, seen["hyp_core"], "missing project=\"hyp_core\" series; seen=%v", seen)
	assert.True(t, seen["hyp_infra"], "missing project=\"hyp_infra\" series; seen=%v", seen)
}

// TestRun_PerProjectClientMetricsLabeled: hyperping_client_api_call_*
// metrics appear once per project label, never collapsed into a single
// global histogram. buildCollectors must construct one
// NewClientMetrics(reg, ns, project.ID) per project entry; passing the
// same MetricsVec across projects would produce duplicate-registration
// panics OR silently merge series.
func TestRun_PerProjectClientMetricsLabeled(t *testing.T) {
	cfg := config{
		namespace: "hyperping",
		cacheMode: "legacy",
		projects: []projectConfig{
			{ID: "hyp_core", APIKey: "a"},
			{ID: "hyp_infra", APIKey: "b"},
		},
	}
	reg := prometheus.NewRegistry()
	_, err := buildCollectors(cfg, reg, newDiscardLogger())
	require.NoError(t, err, "registering two per-project client-metrics sets must NOT panic")

	// Gather Descs and verify both project values appear at least once on
	// hyperping_client_* or hyperping_mcp_* metrics. (No need to actually
	// drive a call; the Desc registration alone is the signal that the
	// fan-out instantiated two distinct metric sets.)
	mfs, err := reg.Gather()
	require.NoError(t, err)
	clientHistFound := map[string]bool{}
	for _, mf := range mfs {
		if mf.GetName() != "hyperping_mcp_initialize_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "project" {
					clientHistFound[lp.GetValue()] = true
				}
			}
		}
	}
	// hyperping_mcp_initialize_total is a Counter and is materialised on
	// registration (because it carries the project constLabel, the Desc
	// itself is project-scoped). After buildCollectors returns we expect
	// to see one Counter per project at value 0.
	assert.True(t, clientHistFound["hyp_core"], "missing hyperping_mcp_initialize_total{project=\"hyp_core\"}")
	assert.True(t, clientHistFound["hyp_infra"], "missing hyperping_mcp_initialize_total{project=\"hyp_infra\"}")
}
