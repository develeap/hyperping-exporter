// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package collector

import (
	"context"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// ClientMetrics implements client.Metrics using Prometheus counters,
// histograms, and gauges registered under the "hyperping_client" namespace.
type ClientMetrics struct {
	apiCallDuration     *prometheus.HistogramVec
	retryTotal          *prometheus.CounterVec
	circuitBreakerState *prometheus.GaugeVec
}

// NewClientMetrics creates and registers all client operational metrics.
func NewClientMetrics(registry *prometheus.Registry, namespace string) *ClientMetrics {
	clientNS := namespace + "_client"
	m := &ClientMetrics{
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
func (m *ClientMetrics) RecordAPICall(_ context.Context, method, path string, statusCode int, durationSec float64) {
	path = strings.SplitN(path, "?", 2)[0]
	m.apiCallDuration.WithLabelValues(method, path, strconv.Itoa(statusCode)).Observe(durationSec)
}

// RecordRetry implements client.Metrics.
func (m *ClientMetrics) RecordRetry(_ context.Context, method, path string, attempt int) {
	path = strings.SplitN(path, "?", 2)[0]
	m.retryTotal.WithLabelValues(method, path, strconv.Itoa(attempt)).Inc()
}

// RecordCircuitBreakerState implements client.Metrics.
// It resets all state gauges to 0 before setting the current state to 1
// so the active state is always unambiguous.
func (m *ClientMetrics) RecordCircuitBreakerState(_ context.Context, state string) {
	for _, s := range []string{"closed", "open", "half-open"} {
		m.circuitBreakerState.WithLabelValues(s).Set(0)
	}
	m.circuitBreakerState.WithLabelValues(state).Set(1)
}
