// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package otelpush_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/develeap/hyperping-exporter/internal/collector"
	"github.com/develeap/hyperping-exporter/internal/otelpush"
	hyperping "github.com/develeap/hyperping-go"
)

// bridgeFixture creates a MeterProvider with a ManualReader, registers bridge
// callbacks for the supplied sources, forces a collection cycle, and returns
// the collected ResourceMetrics. It is the entry point for all bridge tests.
func bridgeFixture(t *testing.T, sources []otelpush.CollectorSource, namespace string) metricdata.ResourceMetrics {
	t.Helper()

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = provider.Shutdown(ctx)
	})

	require.NoError(t, otelpush.RegisterBridgeCallbacks(provider, sources, namespace))

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	return rm
}

// findMetric returns the ScopeMetrics entry whose name matches, or nil.
func findMetric(rm metricdata.ResourceMetrics, name string) *metricdata.Metrics {
	for i := range rm.ScopeMetrics {
		for j := range rm.ScopeMetrics[i].Metrics {
			if rm.ScopeMetrics[i].Metrics[j].Name == name {
				return &rm.ScopeMetrics[i].Metrics[j]
			}
		}
	}
	return nil
}

// dataPointCount returns the number of float64 gauge data points for a metric.
func dataPointCount(m *metricdata.Metrics) int {
	if m == nil {
		return 0
	}
	g, ok := m.Data.(metricdata.Gauge[float64])
	if !ok {
		return 0
	}
	return len(g.DataPoints)
}

// fakeSource implements CollectorSource for bridge tests.
type fakeSource struct {
	snap    collector.Snapshot
	project string
}

func (f *fakeSource) TakeSnapshot() collector.Snapshot { return f.snap }
func (f *fakeSource) Project() string                  { return f.project }

// TestBridge_EmitsGauges verifies that the bridge registers callbacks that
// emit monitor_up, sla_ratio, data_age_seconds, and tenant-level gauges for
// a single collector with known snapshot data.
func TestBridge_EmitsGauges(t *testing.T) {
	snap := collector.Snapshot{
		Monitors: []hyperping.Monitor{
			{UUID: "m1", Name: "Prod/web", Status: "up"},
			{UUID: "m2", Name: "Prod/db", Status: "down"},
		},
		Reports: map[string][]hyperping.MonitorReport{
			"24h": {
				{UUID: "m1", SLA: 0.999},
				{UUID: "m2", SLA: 0.950},
			},
		},
		ResponseTimeIndex: map[string]float64{"m1": 0.042},
		DataAges:          map[string]float64{"hot": 5.0},
		ScrapeOK:          true,
	}
	src := &fakeSource{snap: snap, project: "prod"}
	rm := bridgeFixture(t, []otelpush.CollectorSource{src}, "hyperping")

	monUp := findMetric(rm, "hyperping_monitor_up")
	require.NotNil(t, monUp, "hyperping_monitor_up metric must be present")
	assert.Equal(t, 2, dataPointCount(monUp), "expected one data point per monitor")

	slaRatio := findMetric(rm, "hyperping_monitor_sla_ratio")
	require.NotNil(t, slaRatio, "hyperping_monitor_sla_ratio metric must be present")
	assert.GreaterOrEqual(t, dataPointCount(slaRatio), 2)

	dataAge := findMetric(rm, "hyperping_data_age_seconds")
	require.NotNil(t, dataAge, "hyperping_data_age_seconds metric must be present")
	assert.GreaterOrEqual(t, dataPointCount(dataAge), 1)
}

// TestBridge_MultiProject verifies that two sources with different project IDs
// produce metrics with distinct "project" attribute values in the same export.
func TestBridge_MultiProject(t *testing.T) {
	snapA := collector.Snapshot{
		Monitors: []hyperping.Monitor{
			{UUID: "a1", Name: "Prod/web", Status: "up"},
		},
		DataAges: map[string]float64{"hot": 3.0},
		ScrapeOK: true,
	}
	snapB := collector.Snapshot{
		Monitors: []hyperping.Monitor{
			{UUID: "b1", Name: "Staging/api", Status: "down"},
		},
		DataAges: map[string]float64{"hot": 7.0},
		ScrapeOK: false,
	}
	sources := []otelpush.CollectorSource{
		&fakeSource{snap: snapA, project: "prod"},
		&fakeSource{snap: snapB, project: "staging"},
	}
	rm := bridgeFixture(t, sources, "hyperping")

	monUp := findMetric(rm, "hyperping_monitor_up")
	require.NotNil(t, monUp, "hyperping_monitor_up must be present")
	assert.Equal(t, 2, dataPointCount(monUp), "expected one data point per monitor across both projects")

	g, ok := monUp.Data.(metricdata.Gauge[float64])
	require.True(t, ok)

	seenProjects := map[string]bool{}
	for _, dp := range g.DataPoints {
		if p, ok := dp.Attributes.Value("project"); ok {
			seenProjects[p.AsString()] = true
		}
	}
	assert.True(t, seenProjects["prod"], "expected project=prod in attributes")
	assert.True(t, seenProjects["staging"], "expected project=staging in attributes")
}
