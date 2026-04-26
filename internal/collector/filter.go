// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package collector

import (
	"regexp"

	hyperping "github.com/develeap/hyperping-go"
)

// filterMonitorsByName splits monitors into kept and excluded slices.
// Monitors whose Name matches rx are excluded; the rest are kept.
// If rx is nil every monitor is kept.
func filterMonitorsByName(monitors []hyperping.Monitor, rx *regexp.Regexp) (kept, excluded []hyperping.Monitor) {
	if rx == nil {
		return monitors, nil
	}
	kept = make([]hyperping.Monitor, 0, len(monitors))
	excluded = make([]hyperping.Monitor, 0)
	for _, m := range monitors {
		if rx.MatchString(m.Name) {
			excluded = append(excluded, m)
		} else {
			kept = append(kept, m)
		}
	}
	return kept, excluded
}

// filterOutagesByMonitorUUID returns only outages whose Monitor.UUID is present
// in includedUUIDs. Used to remove outages belonging to excluded monitors so
// they do not inflate tenant-level aggregate metrics.
func filterOutagesByMonitorUUID(outages []hyperping.Outage, includedUUIDs map[string]struct{}) []hyperping.Outage {
	if len(outages) == 0 {
		return outages
	}
	result := make([]hyperping.Outage, 0, len(outages))
	for _, o := range outages {
		if _, ok := includedUUIDs[o.Monitor.UUID]; ok {
			result = append(result, o)
		}
	}
	return result
}

// filterReportsByMonitorUUID returns only reports whose UUID is present in
// includedUUIDs. Used so excluded monitors do not contribute to per-period
// SLA averages or to the tenant health score.
func filterReportsByMonitorUUID(reports []hyperping.MonitorReport, includedUUIDs map[string]struct{}) []hyperping.MonitorReport {
	if len(reports) == 0 {
		return reports
	}
	result := make([]hyperping.MonitorReport, 0, len(reports))
	for _, r := range reports {
		if _, ok := includedUUIDs[r.UUID]; ok {
			result = append(result, r)
		}
	}
	return result
}
