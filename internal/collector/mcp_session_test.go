// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package collector

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	hyperping "github.com/develeap/hyperping-go"
)

// TestMCPSessionPropagation locks the contract from issue #60: hyperping-go
// (v0.5.0+) captures Mcp-Session-Id from the initialize response and echoes
// it on every subsequent tools/call. A fake MCP server enforces session
// ownership the way Hyperping production does: sessionless tools/call
// requests get JSON-RPC -32000 (the rate-limit-on-initialize error string);
// sessioned requests succeed.
//
// Runs the real exporter wiring (hyperping.NewMcpTransport ->
// ObservedTransport -> MCPClient -> Collector.fetchMcpData) against 50
// mock monitors and asserts:
//   - the server observed exactly 1 "initialize" JSON-RPC method;
//   - every "tools/call" request carried Mcp-Session-Id: <captured-on-init>;
//   - the ObservedTransport counters match: initialize_total == 1,
//     call_rate_limited_total{*} == 0.
//
// If a future SDK bump regresses session-id propagation, this test goes red
// on a clean checkout — no production deploy needed to detect the regression.
func TestMCPSessionPropagation(t *testing.T) {
	const sessionID = "test-sess-abc"
	var (
		initializeCount   atomic.Int64
		toolsCallCount    atomic.Int64
		sessionLessCalls  atomic.Int64
		toolsCallMu       sync.Mutex
		toolsCallMethods  = map[string]int{}
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		method, _ := req["method"].(string)
		id := req["id"]

		switch method {
		case "initialize":
			initializeCount.Add(1)
			w.Header().Set("Mcp-Session-Id", sessionID)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]any{
					"protocolVersion": "2025-03-26",
					"serverInfo": map[string]any{
						"name":    "fake-hyperping-mcp",
						"version": "test",
					},
				},
			})

		case "tools/call":
			toolsCallCount.Add(1)
			toolName := ""
			if params, ok := req["params"].(map[string]any); ok {
				toolName, _ = params["name"].(string)
			}
			toolsCallMu.Lock()
			toolsCallMethods[toolName]++
			toolsCallMu.Unlock()

			if r.Header.Get("Mcp-Session-Id") != sessionID {
				// Hyperping's documented response when the client is
				// sessionless: -32000 rate-limit-on-initialize.
				sessionLessCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"error": map[string]any{
						"code":    -32000,
						"message": "Hyperping MCP rate limit exceeded for \"initialize\" (1/5 per minute). Slow down and reuse your existing MCP session — do NOT call \"initialize\" on every tool call. Retry after 3s.",
					},
				})
				return
			}

			// Sessioned: return an empty content array. The high-level
			// MCPClient.Get* methods parse this to (nil, nil) and the
			// collector's nil-guard silently skips the per-monitor update.
			// The point of the test is that CallTool returned no error.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]any{
					"content": []any{},
				},
			})

		default:
			http.Error(w, "unknown method "+method, http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	// Real SDK transport against the fake server. localhost is exempt from
	// the TLS-only enforcement in hyperping-go/transport.go.
	rawTransport, err := hyperping.NewMcpTransport("test-api-key", srv.URL)
	require.NoError(t, err)

	reg := prometheus.NewRegistry()
	mcpMetrics := NewMCPMetrics(reg, "hyperping")
	observed := NewObservedTransport(rawTransport, mcpMetrics)
	// Eager init through the wrapper, mirroring what main.go does at process
	// startup. The SDK's lazy init bypasses the MCPTransport interface
	// (calls its own receiver method directly), so without this explicit
	// call the wrapper's InitializeTotal counter would stay at 0 even when
	// the SDK successfully handshakes.
	_, err = observed.Initialize(context.Background())
	require.NoError(t, err)
	mcpClient := hyperping.NewMCPClient(observed)

	// 50 mock monitors. We don't need real API data; only the MCP path is
	// exercised here.
	monitors := make([]hyperping.Monitor, 50)
	for i := range monitors {
		monitors[i] = hyperping.Monitor{UUID: "mon-" + string(rune('a'+i%26))}
	}

	api := &mockAPI{monitors: monitors}
	c := NewCollector(api, mcpClient, 60*time.Second, newTestLogger(), "hyperping",
		WithMCPMetrics(mcpMetrics),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = c.fetchMcpData(ctx, monitors)
	require.NoError(t, err)

	// Invariant 1: exactly one initialize JSON-RPC method per process.
	require.Equal(t, int64(1), initializeCount.Load(),
		"expected exactly one initialize call across the refresh; saw %d", initializeCount.Load())

	// Invariant 2: every tools/call carried the session header.
	require.Equal(t, int64(0), sessionLessCalls.Load(),
		"expected zero sessionless tools/call requests; saw %d (this is the issue #60 symptom)", sessionLessCalls.Load())

	// Invariant 3: observed-transport counters reflect the same story.
	require.Equal(t, float64(1), counterValue(t, reg, "hyperping_mcp_initialize_total"),
		"hyperping_mcp_initialize_total must be 1")
	require.Equal(t, float64(0), counterVecSum(t, reg, "hyperping_mcp_call_rate_limited_total"),
		"hyperping_mcp_call_rate_limited_total{*} must sum to 0")

	t.Logf("tools/call breakdown: %v (total=%d)", toolsCallMethods, toolsCallCount.Load())
}

// counterValue reads a single-series counter's current value out of a
// Prometheus registry by name. Returns 0 if the metric is absent.
func counterValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if c := m.GetCounter(); c != nil {
				return c.GetValue()
			}
		}
	}
	return 0
}

// counterVecSum reads the sum across all label sets of a counter vec.
// Useful for "the whole vector must be 0" assertions.
func counterVecSum(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	var total float64
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if c := m.GetCounter(); c != nil {
				total += c.GetValue()
			}
		}
	}
	return total
}
