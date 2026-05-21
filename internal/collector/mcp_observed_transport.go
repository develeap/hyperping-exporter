// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package collector

import (
	"context"
	"strings"

	hyperping "github.com/develeap/hyperping-go"
)

// ObservedTransport wraps a hyperping.MCPTransport and increments the
// MCPMetrics counters. Used by main.go to observe the SDK's MCP transport
// without modifying it.
//
// It implements hyperping.MCPTransport (Initialize + CallTool) so it can be
// dropped into hyperping.NewMCPClient in place of the raw transport.
//
// See client_metrics.go MCPMetrics doc comment for what each counter means
// and how it surfaces issue #60.
type ObservedTransport struct {
	inner   hyperping.MCPTransport
	metrics *MCPMetrics
}

// NewObservedTransport wraps inner so its events are counted in metrics.
// metrics may be nil — the decorator becomes a no-op pass-through in that
// case so the exporter can still run with no Prometheus registry attached
// (e.g. in tests).
func NewObservedTransport(inner hyperping.MCPTransport, metrics *MCPMetrics) *ObservedTransport {
	return &ObservedTransport{inner: inner, metrics: metrics}
}

// Initialize implements hyperping.MCPTransport. Counts every successful
// handshake. A healthy process emits exactly one increment over its lifetime;
// values >>1 indicate the SDK is re-handshaking on every tool call (issue #60).
func (o *ObservedTransport) Initialize(ctx context.Context) (map[string]any, error) {
	result, err := o.inner.Initialize(ctx)
	if err == nil && o.metrics != nil {
		o.metrics.InitializeTotal.Inc()
	}
	return result, err
}

// CallTool implements hyperping.MCPTransport. On error, inspects the error
// for the documented rate-limit and session-loss signatures and increments
// the matching counter. Successful calls are not counted (RecordAPICall on
// the underlying HTTP client already covers latency / status-code).
func (o *ObservedTransport) CallTool(ctx context.Context, toolName string, args map[string]any) (any, error) {
	result, err := o.inner.CallTool(ctx, toolName, args)
	if err != nil && o.metrics != nil {
		if isRateLimitError(err) {
			o.metrics.CallRateLimited.WithLabelValues(toolName).Inc()
		}
		if isSessionLostError(err) {
			o.metrics.SessionRefreshTotal.Inc()
		}
	}
	return result, err
}

// isRateLimitError matches Hyperping's MCP rate-limit JSON-RPC response shape.
// The current SDK (hyperping-go v0.4.0) does not expose a typed sentinel for
// this error class; matching the documented server-side string is the only
// portable signal until the SDK ships a typed error. The substrings are
// stable parts of the message Hyperping returns:
//
//	"MCP error -32000: Hyperping MCP rate limit exceeded for \"initialize\"
//	 (N/5 per minute). Slow down and reuse your existing MCP session ..."
//
// See issue #60 for the captured error string.
func isRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "rate limit exceeded for") ||
		strings.Contains(msg, "reuse your existing MCP session")
}

// isSessionLostError matches the sentinel that hyperping-go v0.5.0+ will
// return when the server drops our session (see the upstream PR linked from
// issue #60). Until v0.5.0 ships, no error matches this and the counter
// stays at 0. When the SDK exports ErrSessionLost, this function can switch
// to errors.Is(err, hyperping.ErrSessionLost) — substring is a transitional
// shim.
func isSessionLostError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "MCP session lost")
}
