// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

// Eager warm/cold startup prefetch tests (work item: v1.8.1).
//
// Pre-1.8.1 behaviour: in (*tieredRefresher).start, each of the WARM and
// COLD goroutines constructed time.NewTicker(d) and entered a select
// loop. time.NewTicker(d) fires FIRST at +d, not at 0, so a freshly
// started exporter had no 7d/30d/90d/365d SLA series for coldTTL after
// boot and no 24h SLA series for warmTTL after boot. That dark-dashboard
// window is operationally bad for an SLA-exec surface that may have a
// 30m/1h cold TTL on real deployments.
//
// The 1.8.1 fix runs ONE refresh inside each enabled tier's goroutine
// BEFORE the ticker loop, so all tiers are populated soon after process
// start. HOT remains in-band on start() (gates /readyz); WARM/COLD eager
// fires are async on the caller. Disabled tiers do not run an eager
// refresh.
//
// These tests use sub-second TTLs that are STILL much larger than the
// test window, so any observed refresh count > 0 must come from the
// eager pre-ticker call, not from a tick.

package collector

import (
	"context"
	"testing"
	"time"

	hyperping "github.com/develeap/hyperping-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTieredRefresher_EagerWarmPrefetch: WARM must refresh once before
// the warm ticker fires. warmTTL is set to a value much larger than the
// test window so any reportsCalls increment must be the eager call.
func TestTieredRefresher_EagerWarmPrefetch(t *testing.T) {
	api := &mockAPI{
		monitors:     []hyperping.Monitor{{UUID: "mon_1", Name: "Web"}},
		healthchecks: []hyperping.Healthcheck{},
	}
	tr := &tieredRefresher{
		api:     api,
		logger:  newTestLogger(),
		hotTTL:  10 * time.Millisecond,
		warmTTL: 30 * time.Second, // far larger than the test window
		coldTTL: 30 * time.Second, // suppress cold ticks
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		tr.start(ctx)
		close(done)
	}()

	// Eager warm refresh must complete in well under warmTTL. 1s budget
	// is generous; expected real-world latency is single-digit ms.
	require.Eventually(t, func() bool {
		return tr.warm.Load() != nil
	}, time.Second, 5*time.Millisecond, "warm tier must publish via eager refresh before the first warm tick")
	require.GreaterOrEqual(t, int(api.reportsCalls.Load()), 1,
		"warm eager refresh must call ListMonitorReports at least once before warmTTL elapses")

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("start did not return after cancel; tier goroutine leak?")
	}
}

// TestTieredRefresher_EagerColdPrefetch: COLD must refresh once before
// the cold ticker fires. Same shape as the warm test.
func TestTieredRefresher_EagerColdPrefetch(t *testing.T) {
	api := &mockAPI{
		monitors:     []hyperping.Monitor{{UUID: "mon_1", Name: "Web"}},
		healthchecks: []hyperping.Healthcheck{},
		reports: []hyperping.MonitorReport{
			{UUID: "mon_1", Name: "Web", SLA: 99.9},
		},
	}
	tr := &tieredRefresher{
		api:     api,
		logger:  newTestLogger(),
		hotTTL:  10 * time.Millisecond,
		warmTTL: 30 * time.Second, // suppress warm ticks
		coldTTL: 30 * time.Second, // far larger than the test window
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		tr.start(ctx)
		close(done)
	}()

	require.Eventually(t, func() bool {
		return tr.cold.Load() != nil
	}, time.Second, 5*time.Millisecond, "cold tier must publish via eager refresh before the first cold tick")

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("start did not return after cancel; tier goroutine leak?")
	}
}

// TestTieredRefresher_EagerWarmDisabledNoEagerCall: warmDisabled=true
// must NOT run an eager warm refresh. With cold also disabled the only
// path to ListMonitorReports is the eager warm call, which must NOT
// happen. The WARM snapshot pointer must remain nil.
func TestTieredRefresher_EagerWarmDisabledNoEagerCall(t *testing.T) {
	api := &mockAPI{
		monitors:     []hyperping.Monitor{{UUID: "mon_1", Name: "Web"}},
		healthchecks: []hyperping.Healthcheck{},
	}
	tr := &tieredRefresher{
		api:          api,
		logger:       newTestLogger(),
		hotTTL:       10 * time.Millisecond,
		warmTTL:      30 * time.Second,
		coldTTL:      30 * time.Second,
		warmDisabled: true,
		coldDisabled: true, // isolate the warm-eager path from cold's report fetch
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	tr.start(ctx)

	assert.Equal(t, int32(0), api.reportsCalls.Load(),
		"warmDisabled (with cold also disabled) must yield zero ListMonitorReports calls")
	assert.Nil(t, tr.warm.Load(), "WARM snapshot must remain nil under warmDisabled")
}

// TestTieredRefresher_EagerColdDisabledNoEagerCall mirrors the warm-
// disabled assertion for the cold tier. WARM is also suppressed so the
// only path to ListMonitorReports is the cold eager call.
func TestTieredRefresher_EagerColdDisabledNoEagerCall(t *testing.T) {
	api := &mockAPI{
		monitors:     []hyperping.Monitor{{UUID: "mon_1", Name: "Web"}},
		healthchecks: []hyperping.Healthcheck{},
	}
	tr := &tieredRefresher{
		api:          api,
		logger:       newTestLogger(),
		hotTTL:       10 * time.Millisecond,
		warmTTL:      30 * time.Second,
		coldTTL:      30 * time.Second,
		warmDisabled: true, // isolate the cold-eager path from any warm work
		coldDisabled: true,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	tr.start(ctx)

	assert.Equal(t, int32(0), api.reportsCalls.Load(),
		"coldDisabled (with warmDisabled) must yield zero ListMonitorReports calls")
	assert.Nil(t, tr.cold.Load(), "COLD snapshot must remain nil under coldDisabled")
}
