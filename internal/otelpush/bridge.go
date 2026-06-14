// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package otelpush

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/develeap/hyperping-exporter/internal/collector"
)

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

	// All instruments must be created before RegisterCallback is called so
	// the SDK can associate them with the correct scope.
	monitorUp, err := meter.Float64ObservableGauge(
		namespace+"_monitor_up",
		metric.WithDescription("Whether the monitor is up (1) or down (0)."),
		metric.WithUnit("1"),
	)
	if err != nil {
		return fmt.Errorf("create %s_monitor_up: %w", namespace, err)
	}

	slaRatio, err := meter.Float64ObservableGauge(
		namespace+"_monitor_sla_ratio",
		metric.WithDescription("Monitor SLA as a ratio (0-1) over the labelled period."),
		metric.WithUnit("1"),
	)
	if err != nil {
		return fmt.Errorf("create %s_monitor_sla_ratio: %w", namespace, err)
	}

	dataAge, err := meter.Float64ObservableGauge(
		namespace+"_data_age_seconds",
		metric.WithDescription("Seconds elapsed since the last successful API cache refresh, labelled by tier."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return fmt.Errorf("create %s_data_age_seconds: %w", namespace, err)
	}

	tenantUpRatio, err := meter.Float64ObservableGauge(
		namespace+"_tenant_monitors_up_ratio",
		metric.WithDescription("Fraction of monitors currently up (0-1)."),
		metric.WithUnit("1"),
	)
	if err != nil {
		return fmt.Errorf("create %s_tenant_monitors_up_ratio: %w", namespace, err)
	}

	monitorsTotal, err := meter.Float64ObservableGauge(
		namespace+"_monitors",
		metric.WithDescription("Total number of visible monitors."),
		metric.WithUnit("{monitor}"),
	)
	if err != nil {
		return fmt.Errorf("create %s_monitors: %w", namespace, err)
	}

	responseTime, err := meter.Float64ObservableGauge(
		namespace+"_monitor_response_time_seconds",
		metric.WithDescription("Average monitor response time in seconds."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return fmt.Errorf("create %s_monitor_response_time_seconds: %w", namespace, err)
	}

	mtta, err := meter.Float64ObservableGauge(
		namespace+"_monitor_mtta_seconds",
		metric.WithDescription("Mean Time To Acknowledge in seconds over the labelled period."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return fmt.Errorf("create %s_monitor_mtta_seconds: %w", namespace, err)
	}

	_, err = meter.RegisterCallback(
		buildObserveFunc(sources, namespace, monitorUp, slaRatio, dataAge, tenantUpRatio, monitorsTotal, responseTime, mtta),
		monitorUp, slaRatio, dataAge, tenantUpRatio, monitorsTotal, responseTime, mtta,
	)
	return err
}

// buildObserveFunc returns the callback function passed to RegisterCallback.
// Keeping it as a named function (rather than an inline closure) makes the
// hot path easier to profile and lets tests inject instruments directly.
func buildObserveFunc(
	sources []CollectorSource,
	namespace string,
	monitorUp metric.Float64ObservableGauge,
	slaRatio metric.Float64ObservableGauge,
	dataAge metric.Float64ObservableGauge,
	tenantUpRatio metric.Float64ObservableGauge,
	monitorsTotal metric.Float64ObservableGauge,
	responseTime metric.Float64ObservableGauge,
	mtta metric.Float64ObservableGauge,
) metric.Callback {
	return func(ctx context.Context, o metric.Observer) error {
		for _, src := range sources {
			snap := src.TakeSnapshot()
			project := src.Project()
			projAttr := attribute.String("project", project)

			emitMonitorMetrics(o, snap, projAttr, monitorUp, responseTime)
			emitSLAMetrics(o, snap, projAttr, slaRatio)
			emitMTTAMetrics(o, snap, projAttr, mtta)
			emitDataAgeMetrics(o, snap, projAttr, dataAge)
			emitTenantMetrics(o, snap, projAttr, tenantUpRatio, monitorsTotal)
		}
		return nil
	}
}

func emitMonitorMetrics(
	o metric.Observer,
	snap collector.Snapshot,
	projAttr attribute.KeyValue,
	monitorUp metric.Float64ObservableGauge,
	responseTime metric.Float64ObservableGauge,
) {
	for _, m := range snap.Monitors {
		tenant := collector.ExtractTenant(m.Name)
		tier := collector.EscalationTier(m)
		attrs := metric.WithAttributes(
			attribute.String("uuid", m.UUID),
			attribute.String("name", m.Name),
			attribute.String("tenant", tenant),
			attribute.String("tier", tier),
			projAttr,
		)
		val := 0.0
		if m.Status == "up" {
			val = 1.0
		}
		o.ObserveFloat64(monitorUp, val, attrs)

		if rt, ok := snap.ResponseTimeIndex[m.UUID]; ok {
			o.ObserveFloat64(responseTime, rt, attrs)
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
			o.ObserveFloat64(slaRatio, r.SLA,
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

func emitTenantMetrics(
	o metric.Observer,
	snap collector.Snapshot,
	projAttr attribute.KeyValue,
	tenantUpRatio metric.Float64ObservableGauge,
	monitorsTotal metric.Float64ObservableGauge,
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
	o.ObserveFloat64(monitorsTotal, float64(total), metric.WithAttributes(projAttr))
	o.ObserveFloat64(tenantUpRatio, float64(up)/float64(total), metric.WithAttributes(projAttr))
}
