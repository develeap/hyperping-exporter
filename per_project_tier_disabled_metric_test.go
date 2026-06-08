// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

// Per-project disabled-tier self-metric test (work item: feat/per-project-tier-cache, D5).
//
// When a project disables warm or cold via its cache: block, the exporter
// must publish a self-metric
//
//	hyperping_exporter_tier_disabled{project="<id>", tier="warm"|"cold"} 1
//
// so dashboards can distinguish "no data because operator disabled the tier"
// from "no data because the tier is broken". The series is only emitted for
// disabled tiers; an all-enabled project produces zero series here. This
// keeps the metric absence-based and avoids polluting Prometheus with
// useless `=0` series for the vast majority of projects.

package main

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRun_TierDisabledMetricEmittedOnlyForDisabledTiers: builds a multi-
// project cfg where one project disables WARM and another disables both
// WARM and COLD, then asserts the registry exposes one series per
// disabled (project, tier) pair and NO series for projects/tiers that
// remain enabled.
func TestRun_TierDisabledMetricEmittedOnlyForDisabledTiers(t *testing.T) {
	bTrue := true
	bFalse := false
	cfg := config{
		namespace: "hyperping",
		cacheMode: "tiered",
		hotTTL:    60_000_000_000, // 60s
		warmTTL:   300_000_000_000,
		coldTTL:   900_000_000_000,
		projects: []projectConfig{
			{ID: "hyp_core", APIKey: "a"},
			{ID: "hyp_infra", APIKey: "b", Cache: &projectCacheOverride{WarmEnabled: &bFalse}},
			{ID: "hyp_thirdparty", APIKey: "c", Cache: &projectCacheOverride{WarmEnabled: &bFalse, ColdEnabled: &bFalse}},
			{ID: "hyp_all_on", APIKey: "d", Cache: &projectCacheOverride{
				HotEnabled: &bTrue, WarmEnabled: &bTrue, ColdEnabled: &bTrue}},
		},
	}
	reg := prometheus.NewRegistry()
	_, err := buildCollectors(cfg, reg, newDiscardLogger())
	require.NoError(t, err)

	registerTierDisabledMetric(reg, cfg)

	mfs, err := reg.Gather()
	require.NoError(t, err)
	found := map[string]float64{}
	for _, mf := range mfs {
		if mf.GetName() != "hyperping_exporter_tier_disabled" {
			continue
		}
		for _, m := range mf.GetMetric() {
			var proj, tier string
			for _, lp := range m.GetLabel() {
				switch lp.GetName() {
				case "project":
					proj = lp.GetValue()
				case "tier":
					tier = lp.GetValue()
				}
			}
			found[proj+"/"+tier] = m.GetGauge().GetValue()
		}
	}

	// Expected: hyp_infra/warm, hyp_thirdparty/warm, hyp_thirdparty/cold, all=1.
	require.Equal(t, 1.0, found["hyp_infra/warm"], "hyp_infra warm-disabled series missing; saw=%v", found)
	require.Equal(t, 1.0, found["hyp_thirdparty/warm"], "hyp_thirdparty warm-disabled series missing; saw=%v", found)
	require.Equal(t, 1.0, found["hyp_thirdparty/cold"], "hyp_thirdparty cold-disabled series missing; saw=%v", found)

	// Disallowed: any series for hyp_core or hyp_all_on (every tier enabled),
	// or for hot tiers on any project (HOT is never disabled).
	for k := range found {
		assert.False(t, strings.HasPrefix(k, "hyp_core/"),
			"hyp_core has no disabled tiers; series %q must be absent", k)
		assert.False(t, strings.HasPrefix(k, "hyp_all_on/"),
			"hyp_all_on has no disabled tiers; series %q must be absent", k)
		assert.False(t, strings.HasSuffix(k, "/hot"),
			"hot tier is never disabled; series %q must be absent", k)
	}
}

// TestRun_TierDisabledMetricEmptyWhenAllEnabled: zero-override path
// produces zero hyperping_exporter_tier_disabled series. The self-metric
// is intentionally absence-based; an upgrade with no cache: blocks does
// NOT introduce a flood of new `=0` series.
func TestRun_TierDisabledMetricEmptyWhenAllEnabled(t *testing.T) {
	cfg := config{
		namespace: "hyperping",
		cacheMode: "tiered",
		hotTTL:    60_000_000_000,
		warmTTL:   300_000_000_000,
		coldTTL:   900_000_000_000,
		projects: []projectConfig{
			{ID: "hyp_core", APIKey: "a"},
			{ID: "hyp_infra", APIKey: "b"},
		},
	}
	reg := prometheus.NewRegistry()
	_, err := buildCollectors(cfg, reg, newDiscardLogger())
	require.NoError(t, err)
	registerTierDisabledMetric(reg, cfg)

	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() == "hyperping_exporter_tier_disabled" {
			assert.Empty(t, mf.GetMetric(),
				"hyperping_exporter_tier_disabled must have zero series when no tier is disabled")
		}
	}
}
