// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package collector

import (
	"context"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// prometheusClientMetrics implements client.Metrics using Prometheus counters,
// histograms, and gauges registered under the "hyperping_client" namespace.
type prometheusClientMetrics struct {
	apiCallDuration     *prometheus.HistogramVec
	retryTotal          *prometheus.CounterVec
	circuitBreakerState *prometheus.GaugeVec
}

// NewClientMetrics creates and registers all client operational metrics.
func NewClientMetrics(registry *prometheus.Registry, namespace string) *prometheusClientMetrics {
	clientNS := namespace + "_client"
	m := &prometheusClientMetrics{
		apiCallDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: clientNS,
			Name:      "api_call_duration_seconds",
			Help:      "Duration of Hyperping API calls in seconds.",
			Buckets:   prometheus.DefBuckets,
		}, []string{"method", "path", "status_code"}),

		retryTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: clientNS,
			Name:      "retry_total",
			Help:      "Total number of Hyperping API call retries.",
		}, []string{"method", "path", "attempt"}),

		circuitBreakerState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: clientNS,
			Name:      "circuit_breaker_state",
			Help:      "Current circuit breaker state (1 = active). Labels: state={closed,open,half-open}.",
		}, []string{"state"}),
	}

	registry.MustRegister(m.apiCallDuration, m.retryTotal, m.circuitBreakerState)
	return m
}

// RecordAPICall implements client.Metrics.
func (m *prometheusClientMetrics) RecordAPICall(_ context.Context, method, path string, statusCode int, durationSec float64) {
	path = strings.SplitN(path, "?", 2)[0]
	m.apiCallDuration.WithLabelValues(method, path, strconv.Itoa(statusCode)).Observe(durationSec)
}

// RecordRetry implements client.Metrics.
func (m *prometheusClientMetrics) RecordRetry(_ context.Context, method, path string, attempt int) {
	path = strings.SplitN(path, "?", 2)[0]
	m.retryTotal.WithLabelValues(method, path, strconv.Itoa(attempt)).Inc()
}

// RecordCircuitBreakerState implements client.Metrics.
// It resets all state gauges to 0 before setting the current state to 1
// so the active state is always unambiguous.
func (m *prometheusClientMetrics) RecordCircuitBreakerState(_ context.Context, state string) {
	for _, s := range []string{"closed", "open", "half-open"} {
		m.circuitBreakerState.WithLabelValues(s).Set(0)
	}
	m.circuitBreakerState.WithLabelValues(state).Set(1)
}

// MCPMetrics groups the four MCP-transport-layer counters that surface the
// rate-limit / session-id failure class described in
// https://github.com/develeap/hyperping-exporter/issues/60.
//
// With hyperping-go v0.5.0+ (which propagates Mcp-Session-Id and performs
// one-shot session-loss recovery transparently), a healthy steady state is:
//   - InitializeTotal == 1 (set at startup by main.go's eager Initialize
//     through ObservedTransport; see CounterOpts.Help below for why
//     in-process transparent recoveries do not bump this counter)
//   - SessionLostTotal == 0
//   - CallRateLimited{*} == 0
//   - PartialRefreshTotal == 0 modulo legitimate per-monitor MCP outages
//
// SessionLostTotal counts ONLY cases where the SDK's one-shot recovery
// exhausted and the error bubbled to the caller. Transparent recoveries
// (success on first retry after re-init) are invisible at this layer
// because the SDK's lazy init bypasses the MCPTransport interface (calls
// its own receiver method directly).
type MCPMetrics struct {
	InitializeTotal     prometheus.Counter
	SessionLostTotal    prometheus.Counter
	CallRateLimited     *prometheus.CounterVec
	PartialRefreshTotal prometheus.Counter
}

// NewMCPMetrics creates and registers the MCP observability counters.
// Namespace matches the exporter's chosen prefix (default "hyperping") so the
// resulting metric names are e.g. hyperping_mcp_initialize_total.
func NewMCPMetrics(registry *prometheus.Registry, namespace string) *MCPMetrics {
	m := &MCPMetrics{
		InitializeTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "mcp",
			Name:      "initialize_total",
			Help:      "Number of MCP `initialize` handshakes observed through the ObservedTransport. main.go eagerly initializes once at process start, so this is `1` in steady state. Transparent session-loss recoveries inside the SDK bypass the MCPTransport interface and are NOT counted here — they appear only via SessionLostTotal (if recovery exhausts) or CallRateLimited (if a regression sends sessionless requests). A value of 0 means the eager init failed and the SDK is doing a lazy init on first tool call instead.",
		}),
		SessionLostTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "mcp",
			Name:      "session_lost_total",
			Help:      "Number of MCP tool calls that returned ErrSessionLost after the SDK's one-shot recovery (re-initialize + retry) failed. Steady state is 0. Non-zero values mean two consecutive session expirations on the same call OR the server is rejecting newly-issued session ids. Transparent successful recoveries are NOT counted here (see InitializeTotal).",
		}),
		CallRateLimited: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "mcp",
			Name:      "call_rate_limited_total",
			Help:      "Number of MCP tool calls rejected by the server with a rate-limit error. Steady state is 0. Non-zero indicates either the Hyperping account has burst above its tool-call budget, or (regression) the SDK is sending sessionless requests again (issue #60).",
		}, []string{"method"}),
		PartialRefreshTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "mcp",
			Name:      "partial_refresh_total",
			Help:      "Number of cache refreshes where one or more per-monitor MCP fetches failed and the cached values were retained as graceful degradation. A non-zero rate indicates intermittent MCP-side errors not captured by InitializeTotal/SessionLostTotal/CallRateLimited.",
		}),
	}
	registry.MustRegister(m.InitializeTotal, m.SessionLostTotal, m.CallRateLimited, m.PartialRefreshTotal)
	return m
}
