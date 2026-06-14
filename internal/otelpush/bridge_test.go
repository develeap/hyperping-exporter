// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package otelpush_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
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

// gaugeDataPoints returns the slice of float64 gauge data points for a metric.
func gaugeDataPoints(m *metricdata.Metrics) []metricdata.DataPoint[float64] {
	if m == nil {
		return nil
	}
	g, ok := m.Data.(metricdata.Gauge[float64])
	if !ok {
		return nil
	}
	return g.DataPoints
}

// findDPByAttr returns the first data point whose named attribute matches val.
func findDPByAttr(dps []metricdata.DataPoint[float64], key, val string) (metricdata.DataPoint[float64], bool) {
	for _, dp := range dps {
		if v, ok := dp.Attributes.Value(attribute.Key(key)); ok && v.AsString() == val {
			return dp, true
		}
	}
	return metricdata.DataPoint[float64]{}, false
}

func intPtr(v int) *int { return &v }

// TestBridge_MonitorPausedAndInfo verifies monitor_paused and monitor_info are emitted.
func TestBridge_MonitorPausedAndInfo(t *testing.T) {
	snap := collector.Snapshot{
		Monitors: []hyperping.Monitor{
			{UUID: "m1", Name: "Prod/web", Status: "up", Paused: true, Protocol: "https", URL: "https://example.com?q=1", ProjectUUID: "p1", HTTPMethod: "GET"},
			{UUID: "m2", Name: "Prod/db", Status: "down", Paused: false, Protocol: "tcp", URL: "db.example.com", ProjectUUID: "p1", HTTPMethod: ""},
		},
		ScrapeOK: true,
	}
	src := &fakeSource{snap: snap, project: "prod"}
	rm := bridgeFixture(t, []otelpush.CollectorSource{src}, "hyperping")

	paused := findMetric(rm, "hyperping_monitor_paused")
	require.NotNil(t, paused, "hyperping_monitor_paused must be present")
	assert.Equal(t, 2, dataPointCount(paused), "one data point per monitor")

	info := findMetric(rm, "hyperping_monitor_info")
	require.NotNil(t, info, "hyperping_monitor_info must be present")
	assert.Equal(t, 2, dataPointCount(info), "one data point per monitor")

	dps := gaugeDataPoints(info)
	dp, found := findDPByAttr(dps, "uuid", "m1")
	require.True(t, found, "data point for m1 not found in monitor_info")
	proto, _ := dp.Attributes.Value(attribute.Key("protocol"))
	assert.Equal(t, "https", proto.AsString(), "protocol attribute")
	projUUID, _ := dp.Attributes.Value(attribute.Key("project_uuid"))
	assert.Equal(t, "p1", projUUID.AsString(), "project_uuid attribute")
	// URL should have query params stripped
	urlAttr, _ := dp.Attributes.Value(attribute.Key("url"))
	assert.Equal(t, "https://example.com", urlAttr.AsString(), "url attribute must have query stripped")
}

// TestBridge_MonitorSSLExpiration verifies monitor_ssl_expiration_days is only emitted when non-nil.
func TestBridge_MonitorSSLExpiration(t *testing.T) {
	snap := collector.Snapshot{
		Monitors: []hyperping.Monitor{
			{UUID: "m1", Name: "Prod/web", Status: "up", SSLExpiration: intPtr(42)},
			{UUID: "m2", Name: "Prod/db", Status: "up"},
		},
		ScrapeOK: true,
	}
	src := &fakeSource{snap: snap, project: "prod"}
	rm := bridgeFixture(t, []otelpush.CollectorSource{src}, "hyperping")

	ssl := findMetric(rm, "hyperping_monitor_ssl_expiration_days")
	require.NotNil(t, ssl, "hyperping_monitor_ssl_expiration_days must be present")
	assert.Equal(t, 1, dataPointCount(ssl), "only monitor with non-nil SSLExpiration")

	dps := gaugeDataPoints(ssl)
	dp, found := findDPByAttr(dps, "uuid", "m1")
	require.True(t, found, "data point for m1 not found")
	assert.Equal(t, 42.0, dp.Value)
}

// TestBridge_OutageMetrics verifies monitor_outage_active and monitor_active_outage_status_code.
func TestBridge_OutageMetrics(t *testing.T) {
	snap := collector.Snapshot{
		Monitors: []hyperping.Monitor{
			{UUID: "m1", Name: "Prod/web", Status: "down"},
			{UUID: "m2", Name: "Prod/db", Status: "up"},
		},
		OutageIndex: map[string]hyperping.Outage{
			"m1": {StatusCode: 503},
		},
		ScrapeOK: true,
	}
	src := &fakeSource{snap: snap, project: "prod"}
	rm := bridgeFixture(t, []otelpush.CollectorSource{src}, "hyperping")

	active := findMetric(rm, "hyperping_monitor_outage_active")
	require.NotNil(t, active, "hyperping_monitor_outage_active must be present")
	assert.Equal(t, 2, dataPointCount(active))

	dps := gaugeDataPoints(active)
	dpM1, _ := findDPByAttr(dps, "uuid", "m1")
	assert.Equal(t, 1.0, dpM1.Value, "m1 should be 1.0 (active outage)")
	dpM2, _ := findDPByAttr(dps, "uuid", "m2")
	assert.Equal(t, 0.0, dpM2.Value, "m2 should be 0.0 (no outage)")

	statusCode := findMetric(rm, "hyperping_monitor_active_outage_status_code")
	require.NotNil(t, statusCode, "hyperping_monitor_active_outage_status_code must be present")
	assert.Equal(t, 2, dataPointCount(statusCode))

	scDps := gaugeDataPoints(statusCode)
	scM1, _ := findDPByAttr(scDps, "uuid", "m1")
	assert.Equal(t, 503.0, scM1.Value)
	scM2, _ := findDPByAttr(scDps, "uuid", "m2")
	assert.Equal(t, 0.0, scM2.Value)
}

// TestBridge_MaintenanceAndEscalationTier verifies monitor_in_maintenance and monitor_escalation_tier.
func TestBridge_MaintenanceAndEscalationTier(t *testing.T) {
	snap := collector.Snapshot{
		Monitors: []hyperping.Monitor{
			{UUID: "m1", Name: "Prod/web", Status: "up"},
			{UUID: "m2", Name: "Prod/db", Status: "up"},
		},
		MaintenanceIndex: map[string]bool{"m1": true},
		ScrapeOK:         true,
	}
	src := &fakeSource{snap: snap, project: "prod"}
	rm := bridgeFixture(t, []otelpush.CollectorSource{src}, "hyperping")

	maint := findMetric(rm, "hyperping_monitor_in_maintenance")
	require.NotNil(t, maint, "hyperping_monitor_in_maintenance must be present")
	assert.Equal(t, 2, dataPointCount(maint))

	dps := gaugeDataPoints(maint)
	dpM1, _ := findDPByAttr(dps, "uuid", "m1")
	assert.Equal(t, 1.0, dpM1.Value, "m1 is in maintenance")
	dpM2, _ := findDPByAttr(dps, "uuid", "m2")
	assert.Equal(t, 0.0, dpM2.Value, "m2 is not in maintenance")

	tier := findMetric(rm, "hyperping_monitor_escalation_tier")
	require.NotNil(t, tier, "hyperping_monitor_escalation_tier must be present")
	assert.Equal(t, 2, dataPointCount(tier), "one info point per monitor")
	for _, dp := range gaugeDataPoints(tier) {
		assert.Equal(t, 1.0, dp.Value, "escalation tier info metric always has value 1")
	}
}

// TestBridge_HealthcheckMetrics verifies healthcheck_up, healthcheck_paused, healthcheck_period_seconds, healthchecks.
func TestBridge_HealthcheckMetrics(t *testing.T) {
	snap := collector.Snapshot{
		Healthchecks: []hyperping.Healthcheck{
			{UUID: "hc1", Name: "heartbeat-1", IsDown: true, IsPaused: false, Period: 300},
			{UUID: "hc2", Name: "heartbeat-2", IsDown: false, IsPaused: true, Period: 600},
		},
		ScrapeOK: true,
	}
	src := &fakeSource{snap: snap, project: "prod"}
	rm := bridgeFixture(t, []otelpush.CollectorSource{src}, "hyperping")

	hcUp := findMetric(rm, "hyperping_healthcheck_up")
	require.NotNil(t, hcUp, "hyperping_healthcheck_up must be present")
	assert.Equal(t, 2, dataPointCount(hcUp))

	hcPaused := findMetric(rm, "hyperping_healthcheck_paused")
	require.NotNil(t, hcPaused, "hyperping_healthcheck_paused must be present")
	assert.Equal(t, 2, dataPointCount(hcPaused))

	hcPeriod := findMetric(rm, "hyperping_healthcheck_period_seconds")
	require.NotNil(t, hcPeriod, "hyperping_healthcheck_period_seconds must be present")
	assert.Equal(t, 2, dataPointCount(hcPeriod))

	hcTotal := findMetric(rm, "hyperping_healthchecks")
	require.NotNil(t, hcTotal, "hyperping_healthchecks must be present")
	assert.Equal(t, 1, dataPointCount(hcTotal))
	dps := gaugeDataPoints(hcTotal)
	assert.Equal(t, 2.0, dps[0].Value, "total healthchecks should be 2")
}

// TestBridge_ReportMetrics verifies per-monitor report metrics and tenant_avg_sla_ratio.
func TestBridge_ReportMetrics(t *testing.T) {
	snap := collector.Snapshot{
		Monitors: []hyperping.Monitor{
			{UUID: "m1", Name: "Prod/web", Status: "up"},
			{UUID: "m2", Name: "Prod/db", Status: "up"},
		},
		Reports: map[string][]hyperping.MonitorReport{
			"24h": {
				{UUID: "m1", SLA: 99.9, MTTR: 120, Outages: hyperping.OutageStats{Count: 1, TotalDowntime: 60, LongestOutage: 60}},
				{UUID: "m2", SLA: 95.0, MTTR: 300, Outages: hyperping.OutageStats{Count: 2, TotalDowntime: 180, LongestOutage: 120}},
			},
		},
		ScrapeOK: true,
	}
	src := &fakeSource{snap: snap, project: "prod"}
	rm := bridgeFixture(t, []otelpush.CollectorSource{src}, "hyperping")

	outages := findMetric(rm, "hyperping_monitor_outages")
	require.NotNil(t, outages, "hyperping_monitor_outages must be present")
	assert.Equal(t, 2, dataPointCount(outages), "one point per monitor per period")

	downtime := findMetric(rm, "hyperping_monitor_downtime_seconds")
	require.NotNil(t, downtime, "hyperping_monitor_downtime_seconds must be present")
	assert.Equal(t, 2, dataPointCount(downtime))

	longest := findMetric(rm, "hyperping_monitor_longest_outage_seconds")
	require.NotNil(t, longest, "hyperping_monitor_longest_outage_seconds must be present")
	assert.Equal(t, 2, dataPointCount(longest))

	mttr := findMetric(rm, "hyperping_monitor_mttr_seconds")
	require.NotNil(t, mttr, "hyperping_monitor_mttr_seconds must be present")
	assert.Equal(t, 2, dataPointCount(mttr), "both monitors have MTTR > 0")

	avgSLA := findMetric(rm, "hyperping_tenant_avg_sla_ratio")
	require.NotNil(t, avgSLA, "hyperping_tenant_avg_sla_ratio must be present")
	assert.Equal(t, 1, dataPointCount(avgSLA), "one avg per period")
}

// TestBridge_TenantAndOperationalMetrics verifies the eight operational metrics.
func TestBridge_TenantAndOperationalMetrics(t *testing.T) {
	snap := collector.Snapshot{
		Monitors: []hyperping.Monitor{
			{UUID: "m1", Name: "Prod/web", Status: "up"},
			{UUID: "m2", Name: "Prod/db", Status: "down"},
		},
		OutageIndex: map[string]hyperping.Outage{
			"m2": {StatusCode: 500},
		},
		ScrapeOK:               true,
		ScrapeDuration:         150 * time.Millisecond,
		TotalAlerts:            7,
		ExcludedCount:          3,
		OpenIncidentCount:      2,
		ActiveMaintenanceCount: 1,
	}
	src := &fakeSource{snap: snap, project: "prod"}
	rm := bridgeFixture(t, []otelpush.CollectorSource{src}, "hyperping")

	scrapeDur := findMetric(rm, "hyperping_scrape_duration_seconds")
	require.NotNil(t, scrapeDur, "hyperping_scrape_duration_seconds must be present")
	assert.Equal(t, 1, dataPointCount(scrapeDur))
	assert.InDelta(t, 0.15, gaugeDataPoints(scrapeDur)[0].Value, 1e-9)

	scrapeOK := findMetric(rm, "hyperping_scrape_success")
	require.NotNil(t, scrapeOK, "hyperping_scrape_success must be present")
	assert.Equal(t, 1, dataPointCount(scrapeOK))
	assert.Equal(t, 1.0, gaugeDataPoints(scrapeOK)[0].Value)

	alerts := findMetric(rm, "hyperping_alerts")
	require.NotNil(t, alerts, "hyperping_alerts must be present")
	assert.Equal(t, 1, dataPointCount(alerts))
	assert.Equal(t, 7.0, gaugeDataPoints(alerts)[0].Value)

	excluded := findMetric(rm, "hyperping_excluded_monitors")
	require.NotNil(t, excluded, "hyperping_excluded_monitors must be present")
	assert.Equal(t, 1, dataPointCount(excluded))
	assert.Equal(t, 3.0, gaugeDataPoints(excluded)[0].Value)

	activeOutages := findMetric(rm, "hyperping_tenant_active_outages")
	require.NotNil(t, activeOutages, "hyperping_tenant_active_outages must be present")
	assert.Equal(t, 1, dataPointCount(activeOutages))
	assert.Equal(t, 1.0, gaugeDataPoints(activeOutages)[0].Value, "one monitor in OutageIndex")

	incidents := findMetric(rm, "hyperping_incidents_open")
	require.NotNil(t, incidents, "hyperping_incidents_open must be present")
	assert.Equal(t, 1, dataPointCount(incidents))
	assert.Equal(t, 2.0, gaugeDataPoints(incidents)[0].Value)

	maint := findMetric(rm, "hyperping_maintenance_windows_active")
	require.NotNil(t, maint, "hyperping_maintenance_windows_active must be present")
	assert.Equal(t, 1, dataPointCount(maint))
	assert.Equal(t, 1.0, gaugeDataPoints(maint)[0].Value)
}

// TestBridge_TenantHealthScore verifies tenant_health_score is emitted when 30d data is available.
func TestBridge_TenantHealthScore(t *testing.T) {
	snap := collector.Snapshot{
		Monitors: []hyperping.Monitor{
			{UUID: "m1", Name: "Prod/web", Status: "up"},
			{UUID: "m2", Name: "Prod/db", Status: "up"},
		},
		Reports: map[string][]hyperping.MonitorReport{
			"30d": {
				{UUID: "m1", SLA: 99.9},
				{UUID: "m2", SLA: 99.5},
			},
		},
		OutageIndex: map[string]hyperping.Outage{},
		ScrapeOK:    true,
	}
	src := &fakeSource{snap: snap, project: "prod"}
	rm := bridgeFixture(t, []otelpush.CollectorSource{src}, "hyperping")

	healthScore := findMetric(rm, "hyperping_tenant_health_score")
	require.NotNil(t, healthScore, "hyperping_tenant_health_score must be present")
	assert.Equal(t, 1, dataPointCount(healthScore))

	// Expected: upRatio=1.0, avgSLA=(0.999+0.995)/2=0.997, activeOutages=0, total=2
	// score = 1.0*60 + 0.997*40 - 0 = 99.88, clamped to 99.88
	dps := gaugeDataPoints(healthScore)
	assert.InDelta(t, 99.88, dps[0].Value, 0.01)
}

// TestBridge_RegionMetrics verifies monitor_up_by_region.
func TestBridge_RegionMetrics(t *testing.T) {
	snap := collector.Snapshot{
		Monitors: []hyperping.Monitor{
			{UUID: "m1", Name: "Prod/web", Status: "up", Regions: []string{"us-east-1", "eu-west-1"}},
		},
		RegionDownIndex: map[string]map[string]bool{
			"m1": {"eu-west-1": true},
		},
		ScrapeOK: true,
	}
	src := &fakeSource{snap: snap, project: "prod"}
	rm := bridgeFixture(t, []otelpush.CollectorSource{src}, "hyperping")

	regionUp := findMetric(rm, "hyperping_monitor_up_by_region")
	require.NotNil(t, regionUp, "hyperping_monitor_up_by_region must be present")
	assert.Equal(t, 2, dataPointCount(regionUp), "one data point per region")

	dps := gaugeDataPoints(regionUp)
	usEast, found := findDPByAttr(dps, "region", "us-east-1")
	require.True(t, found, "us-east-1 data point must exist")
	assert.Equal(t, 1.0, usEast.Value, "us-east-1 is up")

	euWest, found := findDPByAttr(dps, "region", "eu-west-1")
	require.True(t, found, "eu-west-1 data point must exist")
	assert.Equal(t, 0.0, euWest.Value, "eu-west-1 is down")
}

// TestBridge_AnomalyMetrics verifies monitor_anomaly_count and monitor_anomaly_score.
func TestBridge_AnomalyMetrics(t *testing.T) {
	snap := collector.Snapshot{
		Monitors: []hyperping.Monitor{
			{UUID: "m1", Name: "Prod/web", Status: "up"},
			{UUID: "m2", Name: "Prod/db", Status: "up"},
		},
		AnomalyCountIndex: map[string]int{"m1": 3},
		AnomalyScoreIndex: map[string]float64{"m1": 0.85},
		ScrapeOK:          true,
	}
	src := &fakeSource{snap: snap, project: "prod"}
	rm := bridgeFixture(t, []otelpush.CollectorSource{src}, "hyperping")

	anomCount := findMetric(rm, "hyperping_monitor_anomaly_count")
	require.NotNil(t, anomCount, "hyperping_monitor_anomaly_count must be present")
	assert.Equal(t, 1, dataPointCount(anomCount), "only m1 has anomaly data")
	dps := gaugeDataPoints(anomCount)
	dp, found := findDPByAttr(dps, "uuid", "m1")
	require.True(t, found)
	assert.Equal(t, 3.0, dp.Value)

	anomScore := findMetric(rm, "hyperping_monitor_anomaly_score")
	require.NotNil(t, anomScore, "hyperping_monitor_anomaly_score must be present")
	assert.Equal(t, 1, dataPointCount(anomScore))
	sDps := gaugeDataPoints(anomScore)
	sdp, found := findDPByAttr(sDps, "uuid", "m1")
	require.True(t, found)
	assert.Equal(t, 0.85, sdp.Value)
}

// TestBridge_MonitorCheckInterval verifies monitor_check_interval_seconds.
func TestBridge_MonitorCheckInterval(t *testing.T) {
	snap := collector.Snapshot{
		Monitors: []hyperping.Monitor{
			{UUID: "m1", Name: "Prod/web", Status: "up", CheckFrequency: 30},
			{UUID: "m2", Name: "Prod/db", Status: "up", CheckFrequency: 60},
		},
		ScrapeOK: true,
	}
	src := &fakeSource{snap: snap, project: "prod"}
	rm := bridgeFixture(t, []otelpush.CollectorSource{src}, "hyperping")

	interval := findMetric(rm, "hyperping_monitor_check_interval_seconds")
	require.NotNil(t, interval, "hyperping_monitor_check_interval_seconds must be present")
	assert.Equal(t, 2, dataPointCount(interval))

	dps := gaugeDataPoints(interval)
	dpM1, found := findDPByAttr(dps, "uuid", "m1")
	require.True(t, found)
	assert.Equal(t, 30.0, dpM1.Value)
	dpM2, found := findDPByAttr(dps, "uuid", "m2")
	require.True(t, found)
	assert.Equal(t, 60.0, dpM2.Value)
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
		if p, ok := dp.Attributes.Value(attribute.Key("project")); ok {
			seenProjects[p.AsString()] = true
		}
	}
	assert.True(t, seenProjects["prod"], "expected project=prod in attributes")
	assert.True(t, seenProjects["staging"], "expected project=staging in attributes")
}
