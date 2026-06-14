// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package collector

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	hyperping "github.com/develeap/hyperping-go"
)

// TestCollector_TakeSnapshot_Legacy verifies that TakeSnapshot returns a
// populated snapshot after a successful Refresh in legacy cache mode.
func TestCollector_TakeSnapshot_Legacy(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{UUID: "uuid-1", Name: "Prod/web", Status: "up"},
			{UUID: "uuid-2", Name: "Prod/db", Status: "down"},
		},
		healthchecks: []hyperping.Healthcheck{},
		reports: []hyperping.MonitorReport{
			{UUID: "uuid-1", SLA: 0.999},
		},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := NewCollector(api, nil, 60*time.Second, logger, "hyperping")

	c.Refresh(context.Background())
	require.True(t, c.IsReady(), "collector should be ready after Refresh")

	snap := c.TakeSnapshot()

	assert.True(t, snap.ScrapeOK, "ScrapeOK should be true after a successful refresh")
	require.Len(t, snap.Monitors, 2, "snapshot should contain both monitors")
	assert.Equal(t, "uuid-1", snap.Monitors[0].UUID)
	assert.Equal(t, "uuid-2", snap.Monitors[1].UUID)
	assert.NotNil(t, snap.Reports, "Reports map must be non-nil")
	assert.NotNil(t, snap.ResponseTimeIndex, "ResponseTimeIndex must be non-nil")
	assert.NotNil(t, snap.DataAges, "DataAges must be non-nil")
	assert.Greater(t, snap.DataAges["hot"], 0.0, "hot data age must be positive")
}

// TestCollector_TakeSnapshot_Tiered verifies that TakeSnapshot works in
// tiered cache mode, returning a non-empty snapshot after the HOT tier
// has been driven at least once.
func TestCollector_TakeSnapshot_Tiered(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{UUID: "uuid-t1", Name: "Staging/api", Status: "up"},
		},
		healthchecks: []hyperping.Healthcheck{},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := NewCollector(api, nil, 60*time.Second, logger, "hyperping",
		WithCacheMode(CacheModeTiered),
		WithTierTTLs(50*time.Millisecond, 200*time.Millisecond, 500*time.Millisecond),
	)
	require.NotNil(t, c.tiered, "tiered refresher must be initialised in tiered mode")

	// Drive the HOT tier directly so the test is not timing-dependent.
	c.tiered.refreshHot(context.Background())

	snap := c.TakeSnapshot()

	require.Len(t, snap.Monitors, 1, "snapshot should contain the one monitor")
	assert.Equal(t, "uuid-t1", snap.Monitors[0].UUID)
	assert.NotNil(t, snap.DataAges)
}
