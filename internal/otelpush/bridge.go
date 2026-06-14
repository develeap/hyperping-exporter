// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package otelpush

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	hyperping "github.com/develeap/hyperping-go"

	"github.com/develeap/hyperping-exporter/internal/collector"
)

// bridgeInstruments holds all OTel gauge instruments registered by the bridge.
// Using a struct avoids a growing parameter list on buildObserveFunc as new
// metrics are added.
type bridgeInstruments struct {
	// Existing (pre-EXP-11)
	monitorUp     metric.Float64ObservableGauge
	slaRatio      metric.Float64ObservableGauge
	dataAge       metric.Float64ObservableGauge
	tenantUpRatio metric.Float64ObservableGauge
	monitorsTotal metric.Float64ObservableGauge
	responseTime  metric.Float64ObservableGauge
	mtta          metric.Float64ObservableGauge

	// WI-2: per-monitor
	monitorPaused           metric.Float64ObservableGauge
	monitorSSLExpiration    metric.Float64ObservableGauge
	monitorCheckInterval    metric.Float64ObservableGauge
	monitorInfo             metric.Float64ObservableGauge
	monitorOutageActive     metric.Float64ObservableGauge
	monitorOutageStatusCode metric.Float64ObservableGauge
	monitorEscalationTier   metric.Float64ObservableGauge
	monitorInMaintenance    metric.Float64ObservableGauge
	monitorAnomalyCount     metric.Float64ObservableGauge
	monitorAnomalyScore     metric.Float64ObservableGauge

	// WI-3: healthcheck
	healthcheckUp     metric.Float64ObservableGauge
	healthcheckPaused metric.Float64ObservableGauge
	healthcheckPeriod metric.Float64ObservableGauge
	healthchecks      metric.Float64ObservableGauge

	// WI-4: report
	monitorOutages       metric.Float64ObservableGauge
	monitorDowntime      metric.Float64ObservableGauge
	monitorLongestOutage metric.Float64ObservableGauge
	monitorMTTR          metric.Float64ObservableGauge
	tenantAvgSLARatio    metric.Float64ObservableGauge

	// WI-5: tenant/operational
	tenantHealthScore        metric.Float64ObservableGauge
	tenantActiveOutages      metric.Float64ObservableGauge
	scrapeDuration           metric.Float64ObservableGauge
	scrapeSuccess            metric.Float64ObservableGauge
	incidentsOpen            metric.Float64ObservableGauge
	maintenanceWindowsActive metric.Float64ObservableGauge
	alerts                   metric.Float64ObservableGauge
	excludedMonitors         metric.Float64ObservableGauge

	// WI-6: region
	monitorUpByRegion metric.Float64ObservableGauge
}

// all returns every instrument as a metric.Observable slice for RegisterCallback.
func (b bridgeInstruments) all() []metric.Observable {
	return []metric.Observable{
		b.monitorUp, b.slaRatio, b.dataAge, b.tenantUpRatio, b.monitorsTotal, b.responseTime, b.mtta,
		b.monitorPaused, b.monitorSSLExpiration, b.monitorCheckInterval, b.monitorInfo,
		b.monitorOutageActive, b.monitorOutageStatusCode, b.monitorEscalationTier, b.monitorInMaintenance,
		b.monitorAnomalyCount, b.monitorAnomalyScore,
		b.healthcheckUp, b.healthcheckPaused, b.healthcheckPeriod, b.healthchecks,
		b.monitorOutages, b.monitorDowntime, b.monitorLongestOutage, b.monitorMTTR, b.tenantAvgSLARatio,
		b.tenantHealthScore, b.tenantActiveOutages, b.scrapeDuration, b.scrapeSuccess,
		b.incidentsOpen, b.maintenanceWindowsActive, b.alerts, b.excludedMonitors,
		b.monitorUpByRegion,
	}
}

// newGauge is a helper that creates a Float64ObservableGauge and wraps any
// error with the instrument name for easier diagnosis.
func newGauge(meter metric.Meter, name, desc, unit string) (metric.Float64ObservableGauge, error) {
	g, err := meter.Float64ObservableGauge(name,
		metric.WithDescription(desc),
		metric.WithUnit(unit),
	)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", name, err)
	}
	return g, nil
}

// createInstruments allocates every bridge instrument on the supplied meter.
func createInstruments(meter metric.Meter, ns string) (bridgeInstruments, error) {
	var b bridgeInstruments
	var err error

	if b.monitorUp, err = newGauge(meter, ns+"_monitor_up", "Whether the monitor is up (1) or down (0).", "1"); err != nil {
		return b, err
	}
	if b.slaRatio, err = newGauge(meter, ns+"_monitor_sla_ratio", "Monitor SLA as a ratio (0-1) over the labelled period.", "1"); err != nil {
		return b, err
	}
	if b.dataAge, err = newGauge(meter, ns+"_data_age_seconds", "Seconds elapsed since the last successful API cache refresh, labelled by tier.", "s"); err != nil {
		return b, err
	}
	if b.tenantUpRatio, err = newGauge(meter, ns+"_tenant_monitors_up_ratio", "Fraction of monitors currently up (0-1).", "1"); err != nil {
		return b, err
	}
	if b.monitorsTotal, err = newGauge(meter, ns+"_monitors", "Total number of visible monitors.", "{monitor}"); err != nil {
		return b, err
	}
	if b.responseTime, err = newGauge(meter, ns+"_monitor_response_time_seconds", "Average monitor response time in seconds.", "s"); err != nil {
		return b, err
	}
	if b.mtta, err = newGauge(meter, ns+"_monitor_mtta_seconds", "Mean Time To Acknowledge in seconds over the labelled period.", "s"); err != nil {
		return b, err
	}

	// WI-2: per-monitor additions
	if b.monitorPaused, err = newGauge(meter, ns+"_monitor_paused", "Whether the monitor is paused (1) or active (0).", "1"); err != nil {
		return b, err
	}
	if b.monitorSSLExpiration, err = newGauge(meter, ns+"_monitor_ssl_expiration_days", "Days until SSL certificate expiration.", "d"); err != nil {
		return b, err
	}
	if b.monitorCheckInterval, err = newGauge(meter, ns+"_monitor_check_interval_seconds", "Monitor check frequency in seconds.", "s"); err != nil {
		return b, err
	}
	if b.monitorInfo, err = newGauge(meter, ns+"_monitor_info", "Monitor metadata (value is always 1).", "1"); err != nil {
		return b, err
	}
	if b.monitorOutageActive, err = newGauge(meter, ns+"_monitor_outage_active", "Whether the monitor has an active outage (1) or not (0).", "1"); err != nil {
		return b, err
	}
	if b.monitorOutageStatusCode, err = newGauge(meter, ns+"_monitor_active_outage_status_code", "HTTP status code of the active outage, or 0 if none.", "1"); err != nil {
		return b, err
	}
	if b.monitorEscalationTier, err = newGauge(meter, ns+"_monitor_escalation_tier", "Escalation tier info (value is always 1).", "1"); err != nil {
		return b, err
	}
	if b.monitorInMaintenance, err = newGauge(meter, ns+"_monitor_in_maintenance", "Whether the monitor is covered by an active maintenance window (1) or not (0).", "1"); err != nil {
		return b, err
	}
	if b.monitorAnomalyCount, err = newGauge(meter, ns+"_monitor_anomaly_count", "Number of active anomalies for the monitor.", "1"); err != nil {
		return b, err
	}
	if b.monitorAnomalyScore, err = newGauge(meter, ns+"_monitor_anomaly_score", "Highest anomaly score for the monitor.", "1"); err != nil {
		return b, err
	}

	// WI-3: healthcheck
	if b.healthcheckUp, err = newGauge(meter, ns+"_healthcheck_up", "Whether the healthcheck is up (1) or down (0).", "1"); err != nil {
		return b, err
	}
	if b.healthcheckPaused, err = newGauge(meter, ns+"_healthcheck_paused", "Whether the healthcheck is paused (1) or active (0).", "1"); err != nil {
		return b, err
	}
	if b.healthcheckPeriod, err = newGauge(meter, ns+"_healthcheck_period_seconds", "Healthcheck expected ping interval in seconds.", "s"); err != nil {
		return b, err
	}
	if b.healthchecks, err = newGauge(meter, ns+"_healthchecks", "Total number of healthcheck monitors.", "{healthcheck}"); err != nil {
		return b, err
	}

	// WI-4: report
	if b.monitorOutages, err = newGauge(meter, ns+"_monitor_outages", "Number of outages in the labelled period.", "1"); err != nil {
		return b, err
	}
	if b.monitorDowntime, err = newGauge(meter, ns+"_monitor_downtime_seconds", "Total downtime in seconds over the labelled period.", "s"); err != nil {
		return b, err
	}
	if b.monitorLongestOutage, err = newGauge(meter, ns+"_monitor_longest_outage_seconds", "Duration of the longest outage in seconds over the labelled period.", "s"); err != nil {
		return b, err
	}
	if b.monitorMTTR, err = newGauge(meter, ns+"_monitor_mttr_seconds", "Mean Time To Recovery in seconds over the labelled period.", "s"); err != nil {
		return b, err
	}
	if b.tenantAvgSLARatio, err = newGauge(meter, ns+"_tenant_avg_sla_ratio", "Average SLA ratio across all monitors for the labelled period.", "1"); err != nil {
		return b, err
	}

	// WI-5: tenant/operational
	if b.tenantHealthScore, err = newGauge(meter, ns+"_tenant_health_score", "Composite tenant health score (0-100).", "1"); err != nil {
		return b, err
	}
	if b.tenantActiveOutages, err = newGauge(meter, ns+"_tenant_active_outages", "Number of monitors with an active outage.", "1"); err != nil {
		return b, err
	}
	if b.scrapeDuration, err = newGauge(meter, ns+"_scrape_duration_seconds", "Duration of the last API cache refresh in seconds.", "s"); err != nil {
		return b, err
	}
	if b.scrapeSuccess, err = newGauge(meter, ns+"_scrape_success", "Whether the last API cache refresh succeeded (1) or failed (0).", "1"); err != nil {
		return b, err
	}
	if b.incidentsOpen, err = newGauge(meter, ns+"_incidents_open", "Number of open (non-resolved) incidents.", "1"); err != nil {
		return b, err
	}
	if b.maintenanceWindowsActive, err = newGauge(meter, ns+"_maintenance_windows_active", "Number of currently active maintenance windows.", "1"); err != nil {
		return b, err
	}
	if b.alerts, err = newGauge(meter, ns+"_alerts", "Total number of alerts across all monitors.", "1"); err != nil {
		return b, err
	}
	if b.excludedMonitors, err = newGauge(meter, ns+"_excluded_monitors", "Number of monitors excluded by the name-pattern filter.", "1"); err != nil {
		return b, err
	}

	// WI-6: region
	if b.monitorUpByRegion, err = newGauge(meter, ns+"_monitor_up_by_region", "Whether the monitor is up (1) or down (0) in a specific region.", "1"); err != nil {
		return b, err
	}

	return b, nil
}

// RegisterBridgeCallbacks registers OTel async gauge callbacks on the
// supplied MeterProvider. Each callback reads from the CollectorSource
// slice on every export tick, mapping the cached Snapshot data to OTel
// attributes with the same label structure used by the Prometheus path.
//
// If namespace is empty it defaults to "hyperping". The function is
// exported so tests can wire a ManualReader-backed provider directly
// without going through NewPusher.
func RegisterBridgeCallbacks(provider *sdkmetric.MeterProvider, sources []CollectorSource, namespace string) error {
	if namespace == "" {
		namespace = "hyperping"
	}
	meter := provider.Meter(namespace)

	instr, err := createInstruments(meter, namespace)
	if err != nil {
		return err
	}

	_, err = meter.RegisterCallback(
		buildObserveFunc(sources, instr),
		instr.all()...,
	)
	return err
}

// buildObserveFunc returns the callback function passed to RegisterCallback.
func buildObserveFunc(sources []CollectorSource, instr bridgeInstruments) metric.Callback {
	return func(ctx context.Context, o metric.Observer) error {
		for _, src := range sources {
			snap := src.TakeSnapshot()
			project := src.Project()
			projAttr := attribute.String("project", project)

			emitMonitorMetrics(o, snap, projAttr, instr)
			emitSLAMetrics(o, snap, projAttr, instr.slaRatio)
			emitMTTAMetrics(o, snap, projAttr, instr.mtta)
			emitDataAgeMetrics(o, snap, projAttr, instr.dataAge)
			emitTenantMetrics(o, snap, projAttr, instr)
			emitHealthcheckMetrics(o, snap, projAttr, instr)
			emitReportMetrics(o, snap, projAttr, instr)
			emitRegionMetrics(o, snap, projAttr, instr.monitorUpByRegion)
			emitOperationalMetrics(o, snap, projAttr, instr)
		}
		return nil
	}
}

func emitMonitorMetrics(
	o metric.Observer,
	snap collector.Snapshot,
	projAttr attribute.KeyValue,
	instr bridgeInstruments,
) {
	for _, m := range snap.Monitors {
		tenant := collector.ExtractTenant(m.Name)
		tier := collector.EscalationTier(m)
		name := collector.CapLabel(m.Name)
		attrs := metric.WithAttributes(
			attribute.String("uuid", m.UUID),
			attribute.String("name", name),
			attribute.String("tenant", tenant),
			attribute.String("tier", tier),
			projAttr,
		)
		val := 0.0
		if m.Status == "up" {
			val = 1.0
		}
		o.ObserveFloat64(instr.monitorUp, val, attrs)

		if rt, ok := snap.ResponseTimeIndex[m.UUID]; ok {
			o.ObserveFloat64(instr.responseTime, rt, attrs)
		}

		// WI-2
		pausedVal := 0.0
		if m.Paused {
			pausedVal = 1.0
		}
		o.ObserveFloat64(instr.monitorPaused, pausedVal, attrs)
		o.ObserveFloat64(instr.monitorCheckInterval, float64(m.CheckFrequency), attrs)

		if m.SSLExpiration != nil {
			o.ObserveFloat64(instr.monitorSSLExpiration, float64(*m.SSLExpiration), attrs)
		}

		// monitor_info: value always 1, extra attrs for metadata
		o.ObserveFloat64(instr.monitorInfo, 1,
			metric.WithAttributes(
				attribute.String("uuid", m.UUID),
				attribute.String("name", name),
				attribute.String("protocol", m.Protocol),
				attribute.String("url", collector.SanitizeURL(m.URL)),
				attribute.String("project_uuid", m.ProjectUUID),
				attribute.String("http_method", m.HTTPMethod),
				projAttr,
			),
		)

		// Active outage state
		activeOutage, hasActive := snap.OutageIndex[m.UUID]
		outageVal := 0.0
		if hasActive {
			outageVal = 1.0
		}
		o.ObserveFloat64(instr.monitorOutageActive, outageVal, attrs)
		statusCode := 0
		if hasActive {
			statusCode = activeOutage.StatusCode
		}
		o.ObserveFloat64(instr.monitorOutageStatusCode, float64(statusCode), attrs)

		// Escalation tier info (value always 1, tier in attrs)
		o.ObserveFloat64(instr.monitorEscalationTier, 1,
			metric.WithAttributes(
				attribute.String("uuid", m.UUID),
				attribute.String("name", name),
				attribute.String("tier", tier),
				projAttr,
			),
		)

		// Maintenance coverage
		maintVal := 0.0
		if snap.MaintenanceIndex[m.UUID] {
			maintVal = 1.0
		}
		o.ObserveFloat64(instr.monitorInMaintenance, maintVal, attrs)

		// MCP anomaly metrics (only emit when data is present)
		if count, ok := snap.AnomalyCountIndex[m.UUID]; ok {
			o.ObserveFloat64(instr.monitorAnomalyCount, float64(count), attrs)
		}
		if score, ok := snap.AnomalyScoreIndex[m.UUID]; ok {
			o.ObserveFloat64(instr.monitorAnomalyScore, score, attrs)
		}
	}
}

func emitSLAMetrics(
	o metric.Observer,
	snap collector.Snapshot,
	projAttr attribute.KeyValue,
	slaRatio metric.Float64ObservableGauge,
) {
	for period, reports := range snap.Reports {
		for _, r := range reports {
			o.ObserveFloat64(slaRatio, r.SLA/100.0,
				metric.WithAttributes(
					attribute.String("uuid", r.UUID),
					attribute.String("period", period),
					projAttr,
				),
			)
		}
	}
}

func emitMTTAMetrics(
	o metric.Observer,
	snap collector.Snapshot,
	projAttr attribute.KeyValue,
	mtta metric.Float64ObservableGauge,
) {
	// 24h MTTA from the warm tier
	for uuid, val := range snap.MttaIndex {
		o.ObserveFloat64(mtta, val,
			metric.WithAttributes(
				attribute.String("uuid", uuid),
				attribute.String("period", "24h"),
				projAttr,
			),
		)
	}
	// Per-period MTTA from the cold tier
	for period, byUUID := range snap.MttaByPeriod {
		if period == "24h" {
			continue // already covered by MttaIndex above
		}
		for uuid, val := range byUUID {
			o.ObserveFloat64(mtta, val,
				metric.WithAttributes(
					attribute.String("uuid", uuid),
					attribute.String("period", period),
					projAttr,
				),
			)
		}
	}
}

func emitDataAgeMetrics(
	o metric.Observer,
	snap collector.Snapshot,
	projAttr attribute.KeyValue,
	dataAge metric.Float64ObservableGauge,
) {
	for tier, age := range snap.DataAges {
		o.ObserveFloat64(dataAge, age,
			metric.WithAttributes(
				attribute.String("tier", tier),
				projAttr,
			),
		)
	}
}

// emitTenantMetrics emits tenant-wide aggregate metrics.
func emitTenantMetrics(
	o metric.Observer,
	snap collector.Snapshot,
	projAttr attribute.KeyValue,
	instr bridgeInstruments,
) {
	total := len(snap.Monitors)
	if total == 0 {
		return
	}
	up := 0
	for _, m := range snap.Monitors {
		if m.Status == "up" {
			up++
		}
	}
	upRatio := float64(up) / float64(total)
	o.ObserveFloat64(instr.monitorsTotal, float64(total), metric.WithAttributes(projAttr))
	o.ObserveFloat64(instr.tenantUpRatio, upRatio, metric.WithAttributes(projAttr))
	o.ObserveFloat64(instr.tenantActiveOutages, float64(len(snap.OutageIndex)), metric.WithAttributes(projAttr))
	o.ObserveFloat64(instr.incidentsOpen, float64(snap.OpenIncidentCount), metric.WithAttributes(projAttr))
	o.ObserveFloat64(instr.maintenanceWindowsActive, float64(snap.ActiveMaintenanceCount), metric.WithAttributes(projAttr))

	// Health score requires 30d SLA data to avoid misleadingly low scores.
	if reports30d := snap.Reports["30d"]; len(reports30d) > 0 {
		monitorIndex := buildMonitorIndex(snap.Monitors)
		if avgSLA, ok := bridgeAvgSLA(reports30d, monitorIndex); ok {
			score := bridgeHealthScore(upRatio, avgSLA, len(snap.OutageIndex), total)
			o.ObserveFloat64(instr.tenantHealthScore, score, metric.WithAttributes(projAttr))
		}
	}
}

// emitHealthcheckMetrics emits per-healthcheck metrics.
func emitHealthcheckMetrics(
	o metric.Observer,
	snap collector.Snapshot,
	projAttr attribute.KeyValue,
	instr bridgeInstruments,
) {
	o.ObserveFloat64(instr.healthchecks, float64(len(snap.Healthchecks)), metric.WithAttributes(projAttr))
	for _, hc := range snap.Healthchecks {
		name := collector.CapLabel(hc.Name)
		attrs := metric.WithAttributes(
			attribute.String("uuid", hc.UUID),
			attribute.String("name", name),
			projAttr,
		)
		upVal := 1.0
		if hc.IsDown {
			upVal = 0.0
		}
		o.ObserveFloat64(instr.healthcheckUp, upVal, attrs)
		pausedVal := 0.0
		if hc.IsPaused {
			pausedVal = 1.0
		}
		o.ObserveFloat64(instr.healthcheckPaused, pausedVal, attrs)
		o.ObserveFloat64(instr.healthcheckPeriod, float64(hc.Period), attrs)
	}
}

// emitReportMetrics emits per-monitor period report metrics and per-period tenant averages.
func emitReportMetrics(
	o metric.Observer,
	snap collector.Snapshot,
	projAttr attribute.KeyValue,
	instr bridgeInstruments,
) {
	monitorIndex := buildMonitorIndex(snap.Monitors)

	for period, reports := range snap.Reports {
		slaSum := 0.0
		slaCount := 0

		for _, r := range reports {
			mon, ok := monitorIndex[r.UUID]
			if !ok {
				continue
			}
			tenant := collector.ExtractTenant(mon.Name)
			tier := collector.EscalationTier(mon)
			name := collector.CapLabel(mon.Name)
			sla := r.SLA / 100.0

			attrs := metric.WithAttributes(
				attribute.String("uuid", r.UUID),
				attribute.String("name", name),
				attribute.String("tenant", tenant),
				attribute.String("tier", tier),
				attribute.String("period", period),
				projAttr,
			)
			o.ObserveFloat64(instr.monitorOutages, float64(r.Outages.Count), attrs)
			o.ObserveFloat64(instr.monitorDowntime, float64(r.Outages.TotalDowntime), attrs)
			o.ObserveFloat64(instr.monitorLongestOutage, float64(r.Outages.LongestOutage), attrs)
			if r.MTTR > 0 {
				o.ObserveFloat64(instr.monitorMTTR, float64(r.MTTR), attrs)
			}

			slaSum += sla
			slaCount++
		}

		if slaCount > 0 {
			o.ObserveFloat64(instr.tenantAvgSLARatio, slaSum/float64(slaCount),
				metric.WithAttributes(
					attribute.String("period", period),
					projAttr,
				),
			)
		}
	}
}

// emitRegionMetrics emits per-monitor per-region up/down status.
func emitRegionMetrics(
	o metric.Observer,
	snap collector.Snapshot,
	projAttr attribute.KeyValue,
	monitorUpByRegion metric.Float64ObservableGauge,
) {
	for _, m := range snap.Monitors {
		if len(m.Regions) == 0 {
			continue
		}
		tenant := collector.ExtractTenant(m.Name)
		tier := collector.EscalationTier(m)
		name := collector.CapLabel(m.Name)
		downRegions := snap.RegionDownIndex[m.UUID]
		for _, region := range m.Regions {
			val := 1.0
			if downRegions[region] {
				val = 0.0
			}
			o.ObserveFloat64(monitorUpByRegion, val,
				metric.WithAttributes(
					attribute.String("uuid", m.UUID),
					attribute.String("name", name),
					attribute.String("tenant", tenant),
					attribute.String("tier", tier),
					attribute.String("region", region),
					projAttr,
				),
			)
		}
	}
}

// emitOperationalMetrics emits scrape self-metrics, alerts, and excluded count.
func emitOperationalMetrics(
	o metric.Observer,
	snap collector.Snapshot,
	projAttr attribute.KeyValue,
	instr bridgeInstruments,
) {
	o.ObserveFloat64(instr.scrapeDuration, snap.ScrapeDuration.Seconds(), metric.WithAttributes(projAttr))
	scrapeOKVal := 0.0
	if snap.ScrapeOK {
		scrapeOKVal = 1.0
	}
	o.ObserveFloat64(instr.scrapeSuccess, scrapeOKVal, metric.WithAttributes(projAttr))
	o.ObserveFloat64(instr.alerts, float64(snap.TotalAlerts), metric.WithAttributes(projAttr))
	o.ObserveFloat64(instr.excludedMonitors, float64(snap.ExcludedCount), metric.WithAttributes(projAttr))
}

// buildMonitorIndex constructs a UUID -> Monitor map from the snapshot's Monitors slice.
// Used by emitReportMetrics and emitTenantMetrics to look up monitor metadata for labels.
func buildMonitorIndex(monitors []hyperping.Monitor) map[string]hyperping.Monitor {
	idx := make(map[string]hyperping.Monitor, len(monitors))
	for _, m := range monitors {
		idx[m.UUID] = m
	}
	return idx
}

// bridgeAvgSLA computes the mean SLA ratio (0-1) over reports whose monitor UUID
// is present in monitorIndex. Mirrors avgSLAForPeriod in the collector package.
func bridgeAvgSLA(reports []hyperping.MonitorReport, monitorIndex map[string]hyperping.Monitor) (float64, bool) {
	sum := 0.0
	count := 0
	for _, r := range reports {
		if _, ok := monitorIndex[r.UUID]; !ok {
			continue
		}
		sum += r.SLA / 100.0
		count++
	}
	if count == 0 {
		return 0, false
	}
	return sum / float64(count), true
}

// bridgeHealthScore computes the composite health score (0-100).
// Mirrors computeHealthScore in the collector package:
// score = upRatio*60 + avgSLA*40 - (activeOutages/total)*30, clamped to [0,100].
func bridgeHealthScore(upRatio, avgSLA float64, activeOutages, total int) float64 {
	base := upRatio*60.0 + avgSLA*40.0
	if total > 0 {
		base -= float64(activeOutages) / float64(total) * 30.0
	}
	if base < 0 {
		return 0
	}
	if base > 100 {
		return 100
	}
	return base
}
