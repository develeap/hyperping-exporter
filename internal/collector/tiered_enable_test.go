// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

// Per-tier enable plumbing tests (work item: feat/per-project-tier-cache).
//
// These tests pin the contract for a new tieredRefresher capability: each
// of the three tiers (HOT/WARM/COLD) can be independently disabled on a
// per-project Collector. A disabled tier:
//
//   - launches NO goroutine in (*tieredRefresher).start
//   - issues ZERO API calls (the relevant mockAPI call counters stay 0)
//   - leaves the corresponding atomic snapshot pointer nil so buildCollectorSnapshot
//     treats the tier as missing (already supported, exercised in the
//     existing TestTieredRefresher_BuildCollectorSnapshot_OnlyHotPopulated case).
//
// The new field surface is (warmEnabled, coldEnabled) bool on tieredRefresher;
// HOT is always enabled (a Collector with HOT disabled is rejected upstream
// at parseConfig time). A new WithTierEnable(hot, warm, cold bool) CollectorOption
// plumbs the flags from main.buildCollectors into the Collector.

package collector

import (
	"context"
	"testing"
	"time"

	hyperping "github.com/develeap/hyperping-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTieredRefresher_WarmDisabledSkipsAPI: when warm is disabled the
// refresher's WARM ticker goroutine is never started, so ListMonitorReports
// and ListMaintenance (the WARM-only call) record zero invocations
// attributable to the WARM tier within the test window. The HOT tier still
// calls ListMaintenance (active windows for incidents), so we differentiate
// by checking reports (WARM-only) and confirming WARM never publishes a
// snapshot.
func TestTieredRefresher_WarmDisabledSkipsAPI(t *testing.T) {
	api := &mockAPI{
		monitors:     []hyperping.Monitor{{UUID: "mon_1", Name: "Web"}},
		healthchecks: []hyperping.Healthcheck{},
	}
	tr := &tieredRefresher{
		api:          api,
		logger:       newTestLogger(),
		hotTTL:       10 * time.Millisecond,
		warmTTL:      10 * time.Millisecond,
		coldTTL:      1 * time.Hour, // suppress COLD ticks so the test isolates WARM
		warmDisabled: true,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	tr.start(ctx)

	// HOT must have run (at least the eager initial refresh).
	assert.GreaterOrEqual(t, int(api.monitorsCalls.Load()), 1,
		"HOT must still run; got monitorsCalls=%d", api.monitorsCalls.Load())
	// WARM is responsible for ListMonitorReports (24h window) in tiered
	// mode. With WARM disabled and COLD's first tick suppressed (1h TTL),
	// reports must remain at zero.
	assert.Equal(t, int32(0), api.reportsCalls.Load(),
		"WARM disabled must yield zero ListMonitorReports calls; got %d", api.reportsCalls.Load())
	// The WARM snapshot pointer must remain nil because no successful
	// WARM refresh ever ran.
	assert.Nil(t, tr.warm.Load(), "WARM snapshot must remain nil when warmDisabled=true")
}

// TestTieredRefresher_ColdDisabledSkipsAPI mirrors the warm-disabled case
// for the COLD tier. The WARM tier's first tick is suppressed (1h TTL) so
// the only path to ListMonitorReports is the COLD ticker.
func TestTieredRefresher_ColdDisabledSkipsAPI(t *testing.T) {
	api := &mockAPI{
		monitors:     []hyperping.Monitor{{UUID: "mon_1", Name: "Web"}},
		healthchecks: []hyperping.Healthcheck{},
	}
	tr := &tieredRefresher{
		api:          api,
		logger:       newTestLogger(),
		hotTTL:       10 * time.Millisecond,
		warmTTL:      1 * time.Hour, // suppress WARM ticks so the test isolates COLD
		coldTTL:      10 * time.Millisecond,
		coldDisabled: true,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	tr.start(ctx)

	assert.GreaterOrEqual(t, int(api.monitorsCalls.Load()), 1, "HOT must still run")
	assert.Equal(t, int32(0), api.reportsCalls.Load(),
		"COLD disabled must yield zero ListMonitorReports calls; got %d", api.reportsCalls.Load())
	assert.Nil(t, tr.cold.Load(), "COLD snapshot must remain nil when coldEnabled=false")
}

// TestTieredRefresher_BothWarmAndColdDisabled covers the all-disabled-
// except-hot edge case. The only goroutine started is HOT; ctx cancellation
// must still return cleanly with no leaked goroutines.
func TestTieredRefresher_BothWarmAndColdDisabled(t *testing.T) {
	api := &mockAPI{
		monitors:     []hyperping.Monitor{{UUID: "mon_1", Name: "Web"}},
		healthchecks: []hyperping.Healthcheck{},
	}
	tr := &tieredRefresher{
		api:          api,
		logger:       newTestLogger(),
		hotTTL:       10 * time.Millisecond,
		warmTTL:      10 * time.Millisecond,
		coldTTL:      10 * time.Millisecond,
		warmDisabled: true,
		coldDisabled: true,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		tr.start(ctx)
		close(done)
	}()

	require.Eventually(t, func() bool {
		return tr.hotReady.Load()
	}, time.Second, 5*time.Millisecond, "hot must become ready")

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("start did not return after ctx cancellation; warm/cold goroutines may be leaking")
	}

	assert.Equal(t, int32(0), api.reportsCalls.Load(),
		"both WARM and COLD disabled must yield zero ListMonitorReports calls")
	assert.Nil(t, tr.warm.Load(), "WARM snapshot must remain nil")
	assert.Nil(t, tr.cold.Load(), "COLD snapshot must remain nil")
}

// TestWithTierEnable_PlumbsFlagsIntoRefresher: the new CollectorOption
// must reach the tieredRefresher fields. Without this option the defaults
// are (true, true, true) so existing call sites stay byte-identical.
func TestWithTierEnable_PlumbsFlagsIntoRefresher(t *testing.T) {
	api := &mockAPI{
		monitors:     []hyperping.Monitor{},
		healthchecks: []hyperping.Healthcheck{},
	}
	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping",
		WithCacheMode(CacheModeTiered),
		WithTierTTLs(60*time.Second, 5*time.Minute, 15*time.Minute),
		WithTierEnable(true, false, true),
	)
	require.NotNil(t, c.tiered, "tiered refresher must be constructed in tiered mode")
	assert.False(t, c.tiered.hotDisabled, "hot is always enabled")
	assert.True(t, c.tiered.warmDisabled, "warm flag must propagate from option (enabled=false -> disabled=true)")
	assert.False(t, c.tiered.coldDisabled, "cold flag must propagate from option")
}

// TestWithTierEnable_DefaultsAllEnabled: a Collector built without
// WithTierEnable must behave exactly as today (all three tiers running).
// This is the explicit per-test backward-compat assertion at the
// collector-package level.
func TestWithTierEnable_DefaultsAllEnabled(t *testing.T) {
	api := &mockAPI{}
	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping",
		WithCacheMode(CacheModeTiered),
		WithTierTTLs(60*time.Second, 5*time.Minute, 15*time.Minute),
	)
	require.NotNil(t, c.tiered)
	assert.False(t, c.tiered.hotDisabled, "default must be all-enabled (HOT)")
	assert.False(t, c.tiered.warmDisabled, "default must be all-enabled (WARM)")
	assert.False(t, c.tiered.coldDisabled, "default must be all-enabled (COLD)")
}
