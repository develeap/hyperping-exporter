// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

// Cold-tier MTTA fan-out tests (work item: v1.8.1).
//
// Background: pre-1.8.1 refreshCold called GetMonitorMtta with NO uuids
// based on a comment claim that "empty uuids returns every monitor in
// the project". The live MCP server semantics are different: an empty
// monitor_uuids field yields {monitors:[], totalAcknowledged:0, mtta:0},
// i.e. aggregate-only, no per-monitor entries. The exporter then iterated
// an empty Monitors slice and published an empty mttaByPeriod map, so
// mttaValueFor returned ok=false for every (uuid, cold-period) tuple and
// no cold MTTA series were ever emitted.
//
// The 1.8.1 fix sources per-monitor UUIDs from the HOT tier snapshot
// (populated by HOT's eager in-band refresh at startup) and passes them
// explicitly as the variadic uuids argument. When HOT has not yet
// published, the cold MTTA call is skipped this tick (Debug-logged) and
// recovers on the next cold ticker fire.
//
// These tests pin three contracts:
//   - cold MTTA is wired across periods when HOT has UUIDs available
//   - the MCP call carries non-empty monitor_uuids
//   - when HOT is empty, the MCP call is not issued (no panic, no error)

package collector

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	hyperping "github.com/develeap/hyperping-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingMCPTransport is a mockMCPTransport-shaped transport that
// records each (toolName, args) call so a test can inspect what the
// collector sent on the wire. It also implements the documented MCP
// semantic for get_monitor_mtta: empty monitor_uuids returns the empty-
// monitors aggregate shape; explicit monitor_uuids returns a Monitors[]
// array with one entry per requested UUID (mtta=42.0 + uuid index).
type recordingMCPTransport struct {
	mu            sync.Mutex
	mttaCalls     int
	lastMttaUUIDs []string
	mttaCallArgs  []map[string]any

	// callCount per tool name for cross-tier counter assertions.
	calls map[string]*atomic.Int32
}

func newRecordingMCPTransport() *recordingMCPTransport {
	return &recordingMCPTransport{
		calls: map[string]*atomic.Int32{},
	}
}

func (m *recordingMCPTransport) Initialize(_ context.Context) (map[string]any, error) {
	return nil, nil
}

func (m *recordingMCPTransport) CallTool(_ context.Context, toolName string, args map[string]any) (any, error) {
	m.mu.Lock()
	if m.calls[toolName] == nil {
		m.calls[toolName] = new(atomic.Int32)
	}
	m.calls[toolName].Add(1)
	m.mu.Unlock()

	switch toolName {
	case "get_monitor_mtta":
		m.mu.Lock()
		m.mttaCalls++
		uuids, _ := args["monitor_uuids"].([]string)
		// Copy the slice so later mutation by the caller cannot affect
		// the recorded value.
		got := make([]string, len(uuids))
		copy(got, uuids)
		m.lastMttaUUIDs = got
		copiedArgs := map[string]any{}
		for k, v := range args {
			copiedArgs[k] = v
		}
		m.mttaCallArgs = append(m.mttaCallArgs, copiedArgs)
		m.mu.Unlock()

		if len(uuids) == 0 {
			// Live MCP semantic for empty monitor_uuids: aggregate only,
			// no per-monitor entries. This is the bug repro shape.
			return map[string]any{
				"monitors":          []any{},
				"totalAcknowledged": 0,
				"mtta":              0.0,
			}, nil
		}
		// Explicit uuids: one entry per requested monitor.
		entries := make([]any, 0, len(uuids))
		for _, u := range uuids {
			entries = append(entries, map[string]any{
				"uuid":         u,
				"name":         u,
				"acknowledged": 1,
				"mtta":         42.0,
			})
		}
		return map[string]any{
			"monitors":          entries,
			"totalAcknowledged": len(uuids),
			"mtta":              42.0,
		}, nil
	case "list_recent_alerts":
		return map[string]any{"total": 0}, nil
	}
	return nil, nil
}

func (m *recordingMCPTransport) snapshot() (int, []string, []map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	uuids := make([]string, len(m.lastMttaUUIDs))
	copy(uuids, m.lastMttaUUIDs)
	callArgs := make([]map[string]any, len(m.mttaCallArgs))
	copy(callArgs, m.mttaCallArgs)
	return m.mttaCalls, uuids, callArgs
}

// TestColdMtta_FansOutAcrossPeriods asserts that with HOT populated and
// cold periods configured (7d + 30d), the refresher publishes a
// mttaByPeriod map keyed by each cold period with one entry per HOT-
// known monitor UUID.
func TestColdMtta_FansOutAcrossPeriods(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{UUID: "mon_1", Name: "Web"},
			{UUID: "mon_2", Name: "API"},
		},
		reports: []hyperping.MonitorReport{
			{UUID: "mon_1", Name: "Web", SLA: 99.9},
			{UUID: "mon_2", Name: "API", SLA: 99.5},
		},
	}
	transport := newRecordingMCPTransport()
	mcp := hyperping.NewMCPClient(transport)

	tr := newTieredRefresherForTest(api)
	tr.mcp = mcp
	tr.periods = []string{"7d", "30d"}

	// Seed HOT so refreshCold can source the UUID list. The fix relies
	// on currentMonitorUUIDs() reading t.hot.Load().
	tr.hot.Store(&hotSnapshot{
		monitors:    api.monitors,
		refreshedAt: time.Now(),
	})

	tr.refreshCold(context.Background())

	got := tr.cold.Load()
	require.NotNil(t, got, "cold snapshot must publish when at least one report fetch succeeds")
	require.NotNil(t, got.mttaByPeriod, "mttaByPeriod must be populated when MCP returned per-monitor entries")
	for _, period := range []string{"7d", "30d"} {
		entries, ok := got.mttaByPeriod[period]
		require.True(t, ok, "mttaByPeriod must contain key %q after cold refresh", period)
		assert.Equal(t, 42.0, entries["mon_1"], "period=%s mon_1 MTTA must be set", period)
		assert.Equal(t, 42.0, entries["mon_2"], "period=%s mon_2 MTTA must be set", period)
	}
}

// TestColdMtta_PassesExplicitUUIDs asserts that get_monitor_mtta is
// called with a non-empty monitor_uuids slice that matches the HOT
// snapshot's monitor list. Pre-1.8.1 the call sent zero uuids.
func TestColdMtta_PassesExplicitUUIDs(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{
			{UUID: "mon_alpha"},
			{UUID: "mon_beta"},
		},
		reports: []hyperping.MonitorReport{{UUID: "mon_alpha", Name: "alpha", SLA: 100}},
	}
	transport := newRecordingMCPTransport()
	mcp := hyperping.NewMCPClient(transport)

	tr := newTieredRefresherForTest(api)
	tr.mcp = mcp
	tr.periods = []string{"7d"}

	tr.hot.Store(&hotSnapshot{
		monitors:    api.monitors,
		refreshedAt: time.Now(),
	})

	tr.refreshCold(context.Background())

	calls, lastUUIDs, allArgs := transport.snapshot()
	require.Equal(t, 1, calls, "exactly one get_monitor_mtta call expected for one cold period")
	require.NotEmpty(t, lastUUIDs, "monitor_uuids must be non-empty; empty uuids returns aggregate-only data and breaks fan-out")
	assert.ElementsMatch(t, []string{"mon_alpha", "mon_beta"}, lastUUIDs,
		"explicit uuids must match the HOT-known monitor set")

	// Belt-and-suspenders: the args map MUST contain the monitor_uuids key.
	require.Len(t, allArgs, 1)
	_, hasKey := allArgs[0]["monitor_uuids"]
	assert.True(t, hasKey, "args[monitor_uuids] must be present on the wire")
}

// TestColdMtta_SkipsWhenHotEmpty asserts that when HOT has not published
// yet (or has zero monitors), the cold MTTA MCP call is skipped: no call,
// no panic, no error. The next cold tick after HOT publishes will pick
// it up; mttaByPeriod is absent or empty in the published snapshot.
func TestColdMtta_SkipsWhenHotEmpty(t *testing.T) {
	api := &mockAPI{
		monitors: []hyperping.Monitor{},
		reports: []hyperping.MonitorReport{
			{UUID: "mon_orphan", Name: "orphan", SLA: 99.9},
		},
	}
	transport := newRecordingMCPTransport()
	mcp := hyperping.NewMCPClient(transport)

	tr := newTieredRefresherForTest(api)
	tr.mcp = mcp
	tr.periods = []string{"7d", "30d"}

	// Do NOT seed HOT. currentMonitorUUIDs must return an empty slice.

	tr.refreshCold(context.Background())

	calls, _, _ := transport.snapshot()
	assert.Equal(t, 0, calls,
		"cold MTTA call must be skipped when HOT has no monitors; got %d call(s)", calls)
}
