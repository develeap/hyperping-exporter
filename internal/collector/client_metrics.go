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
// In a healthy steady state with a fixed hyperping-go SDK (v0.5.0+ — see
// issue #60 for the upstream fix):
//   - InitializeTotal == 1 over the process lifetime
//   - SessionRefreshTotal == 0
//   - CallRateLimited{*} == 0
//   - PartialRefreshTotal == 0 (modulo legitimate per-monitor MCP outages)
//
// Until the upstream SDK fix lands, CallRateLimited{*} is expected to tick up
// on every refresh: it is the load-bearing diagnostic that the production
// rate-limit error in #60 is the same class of bug as anticipated.
type MCPMetrics struct {
	InitializeTotal     prometheus.Counter
	SessionRefreshTotal prometheus.Counter
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
			Help:      "Number of successful MCP `initialize` handshakes. Steady state is 1 per process lifetime; values >>1 indicate the SDK is re-handshaking on every tool call (issue #60).",
		}),
		SessionRefreshTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "mcp",
			Name:      "session_refresh_total",
			Help:      "Number of times the MCP transport detected a lost session and re-initialized. Steady state is 0; rare events indicate server-side session expiry or restart.",
		}),
		CallRateLimited: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "mcp",
			Name:      "call_rate_limited_total",
			Help:      "Number of MCP tool calls rejected by the server with a rate-limit error. Steady state is 0; values >0 indicate the sessionless-request bug from issue #60 is still active (upstream SDK does not yet propagate Mcp-Session-Id).",
		}, []string{"method"}),
		PartialRefreshTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "mcp",
			Name:      "partial_refresh_total",
			Help:      "Number of cache refreshes where one or more per-monitor MCP fetches failed and the cached values were retained as graceful degradation. A non-zero rate indicates intermittent MCP-side errors not captured by InitializeTotal/SessionRefreshTotal/CallRateLimited.",
		}),
	}
	registry.MustRegister(m.InitializeTotal, m.SessionRefreshTotal, m.CallRateLimited, m.PartialRefreshTotal)
	return m
}
