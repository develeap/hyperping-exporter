// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

// Warm-tier MTTA emit-suppression tests (work item: v1.8.2).
//
// Background: pre-1.8.2 refreshWarm issued one get_monitor_mtta call per
// monitor with a 24h window and stored res.mtta[uuid] = report.Mtta. The
// aggregate report.Mtta field is the project-wide MTTA value, NOT the
// per-monitor value. When the per-UUID call returned an empty Monitors
// slice (which happens for every monitor whose project has no acknowledged
// alerts in the window), the aggregate was zero and got stored as the
// per-UUID value. The emit path then published N zero-valued period="24h"
// MTTA series that look like real per-monitor zeros but are aggregate
// leaks from empty responses.
//
// The 1.8.2 fix mirrors the cold-tier semantic at tiered.go:523-526: skip
// the res.mtta[uuid] assignment when the response carries no per-monitor
// entries, and when it does, read the value from the matching per-monitor
// entry rather than the aggregate field.
//
// These tests pin three contracts:
//   - Empty Monitors response -> no mtta entry stored, no series emitted.
//   - Populated Monitors response -> per-monitor mtta stored.
//   - Aggregate Mtta is never used: a mock returning per-monitor mtta=100
//     and aggregate mtta=9999 must yield mtta[uuid]=100.

package collector

import (
	"context"
	"sync"
	"testing"
	"time"

	hyperping "github.com/develeap/hyperping-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// warmMttaMockTransport is an mcpTransport that returns scripted shapes
// for get_monitor_mtta keyed by the queried UUID. It also returns the
// minimum bookkeeping needed by refreshWarm for the other per-monitor
// calls so the warm path completes without spurious failures.
type warmMttaMockTransport struct {
	mu sync.Mutex
	// responses[uuid] returns the map[string]any payload for that uuid's
	// get_monitor_mtta call. A nil entry means "respond with empty
	// aggregate-only shape" (Monitors:[], mtta:0).
	responses map[string]map[string]any
	// aggregateOnlyDefault, when true, makes any uuid missing from the
	// responses map return an empty-Monitors aggregate-only shape with
	// the aggregateMtta value as the top-level mtta. This is the live
	// wire shape that the v1.8.2 fix targets.
	aggregateOnlyDefault bool
	aggregateMtta        float64
}

func (m *warmMttaMockTransport) Initialize(_ context.Context) (map[string]any, error) {
	return nil, nil
}

func (m *warmMttaMockTransport) CallTool(_ context.Context, toolName string, args map[string]any) (any, error) {
	switch toolName {
	case "get_monitor_mtta":
		m.mu.Lock()
		defer m.mu.Unlock()
		uuids, _ := args["monitor_uuids"].([]string)
		if len(uuids) == 0 {
			return map[string]any{
				"monitors":          []any{},
				"totalAcknowledged": 0,
				"mtta":              0.0,
			}, nil
		}
		uuid := uuids[0]
		if resp, ok := m.responses[uuid]; ok && resp != nil {
			return resp, nil
		}
		if m.aggregateOnlyDefault {
			return map[string]any{
				"monitors":          []any{},
				"totalAcknowledged": 0,
				"mtta":              m.aggregateMtta,
			}, nil
		}
		return map[string]any{
			"monitors":          []any{},
			"totalAcknowledged": 0,
			"mtta":              0.0,
		}, nil
	case "get_monitor_response_time":
		return map[string]any{"avgResponseTime": 0.0}, nil
	case "get_monitor_anomalies":
		return map[string]any{"anomalies": []any{}}, nil
	case "list_recent_alerts":
		return map[string]any{"totalAlerts": 0}, nil
	}
	return nil, nil
}

// TestWarmMtta_SuppressesEmissionWhenMonitorsEmpty asserts that when the
// MCP server returns the aggregate-only shape (Monitors:[]) for a per-UUID
// query, the warm tier does NOT store an entry in res.mtta. This is the
// regression that produced N zero-valued 24h MTTA series for projects with
// no acknowledged alerts.
func TestWarmMtta_SuppressesEmissionWhenMonitorsEmpty(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{UUID: "mon_1", Name: "Web"},
			{UUID: "mon_2", Name: "API"},
		},
	}
	// Every monitor returns the aggregate-only shape with aggregate
	// mtta=9999. Pre-fix this leaks 9999 into per-monitor values; the
	// fix must skip the assignment entirely.
	transport := &warmMttaMockTransport{
		aggregateOnlyDefault: true,
		aggregateMtta:        9999.0,
	}
	mcp := hyperping.NewMCPClient(transport)

	tr := newTieredRefresherForTest(api)
	tr.mcp = mcp
	tr.hot.Store(&hotSnapshot{
		monitors:    api.monitors,
		refreshedAt: time.Now(),
	})

	tr.refreshWarm(context.Background())

	warm := tr.warm.Load()
	require.NotNil(t, warm, "warm snapshot must publish")
	_, hasMon1 := warm.mtta["mon_1"]
	_, hasMon2 := warm.mtta["mon_2"]
	assert.False(t, hasMon1, "warm.mtta must NOT contain mon_1 when MCP returned empty Monitors slice")
	assert.False(t, hasMon2, "warm.mtta must NOT contain mon_2 when MCP returned empty Monitors slice")
	assert.Empty(t, warm.mtta, "warm.mtta must be empty when every per-UUID call returned aggregate-only data")
}

// TestWarmMtta_EmitsPerMonitorEntryAndIgnoresAggregate asserts the
// positive path: when the per-UUID call returns a populated Monitors
// slice, the warm tier records the per-monitor value and IGNORES the
// aggregate field. The asymmetric per-monitor=100 / aggregate=9999
// fixture proves the aggregate is not used.
func TestWarmMtta_EmitsPerMonitorEntryAndIgnoresAggregate(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{UUID: "mon_1", Name: "Web"},
		},
	}
	transport := &warmMttaMockTransport{
		responses: map[string]map[string]any{
			"mon_1": {
				"monitors": []any{
					map[string]any{
						"uuid":         "mon_1",
						"name":         "Web",
						"acknowledged": 3,
						"mtta":         100.0,
					},
				},
				"totalAcknowledged": 3,
				"mtta":              9999.0,
			},
		},
	}
	mcp := hyperping.NewMCPClient(transport)

	tr := newTieredRefresherForTest(api)
	tr.mcp = mcp
	tr.hot.Store(&hotSnapshot{
		monitors:    api.monitors,
		refreshedAt: time.Now(),
	})

	tr.refreshWarm(context.Background())

	warm := tr.warm.Load()
	require.NotNil(t, warm, "warm snapshot must publish")
	got, ok := warm.mtta["mon_1"]
	require.True(t, ok, "warm.mtta must contain mon_1 when MCP returned a per-monitor entry")
	assert.Equal(t, 100.0, got,
		"warm.mtta[mon_1] must be the per-monitor value (100.0), not the aggregate (9999.0)")
}

// TestWarmMtta_PartialPopulationOnlyEmitsKnownEntries asserts the mixed
// case: of two monitors, one project has an ack (Monitors populated for
// that uuid), the other does not (empty Monitors). The fix must store the
// populated one and skip the empty one. Both monitors share the same
// project aggregate, so without the fix both would carry the aggregate
// value.
func TestWarmMtta_PartialPopulationOnlyEmitsKnownEntries(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{UUID: "mon_acked", Name: "WithAck"},
			{UUID: "mon_clean", Name: "NoAcks"},
		},
	}
	transport := &warmMttaMockTransport{
		responses: map[string]map[string]any{
			"mon_acked": {
				"monitors": []any{
					map[string]any{
						"uuid":         "mon_acked",
						"name":         "WithAck",
						"acknowledged": 1,
						"mtta":         42.0,
					},
				},
				"totalAcknowledged": 1,
				"mtta":              42.0,
			},
			// mon_clean missing -> aggregate-only default kicks in.
		},
		aggregateOnlyDefault: true,
		aggregateMtta:        42.0,
	}
	mcp := hyperping.NewMCPClient(transport)

	tr := newTieredRefresherForTest(api)
	tr.mcp = mcp
	tr.hot.Store(&hotSnapshot{
		monitors:    api.monitors,
		refreshedAt: time.Now(),
	})

	tr.refreshWarm(context.Background())

	warm := tr.warm.Load()
	require.NotNil(t, warm)
	got, ok := warm.mtta["mon_acked"]
	require.True(t, ok, "warm.mtta must contain mon_acked")
	assert.Equal(t, 42.0, got)
	_, hasClean := warm.mtta["mon_clean"]
	assert.False(t, hasClean,
		"warm.mtta must NOT contain mon_clean when per-UUID call returned empty Monitors")
}
