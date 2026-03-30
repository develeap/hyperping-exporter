// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MPL-2.0

package collector

import (
	"context"
	"strconv"

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
func NewClientMetrics(registry *prometheus.Registry) *prometheusClientMetrics {
	m := &prometheusClientMetrics{
		apiCallDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "hyperping_client",
			Name:      "api_call_duration_milliseconds",
			Help:      "Duration of Hyperping API calls in milliseconds.",
			Buckets:   prometheus.DefBuckets,
		}, []string{"method", "path", "status_code"}),

		retryTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "hyperping_client",
			Name:      "retry_total",
			Help:      "Total number of Hyperping API call retries.",
		}, []string{"method", "path", "attempt"}),

		circuitBreakerState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "hyperping_client",
			Name:      "circuit_breaker_state",
			Help:      "Current circuit breaker state (1 = active). Labels: state={closed,open,half-open}.",
		}, []string{"state"}),
	}

	registry.MustRegister(m.apiCallDuration, m.retryTotal, m.circuitBreakerState)
	return m
}

// RecordAPICall implements client.Metrics.
func (m *prometheusClientMetrics) RecordAPICall(_ context.Context, method, path string, statusCode int, durationMs int64) {
	m.apiCallDuration.WithLabelValues(method, path, strconv.Itoa(statusCode)).Observe(float64(durationMs))
}

// RecordRetry implements client.Metrics.
func (m *prometheusClientMetrics) RecordRetry(_ context.Context, method, path string, attempt int) {
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
