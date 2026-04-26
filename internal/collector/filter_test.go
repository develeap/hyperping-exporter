// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package collector

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	hyperping "github.com/develeap/hyperping-go"
)

// --- filterMonitorsByName ---

func TestFilterMonitorsByName_NilPattern(t *testing.T) {
	monitors := []hyperping.Monitor{
		{UUID: "1", Name: "prod-api"},
		{UUID: "2", Name: "[DRILL] test"},
	}
	kept, excluded := filterMonitorsByName(monitors, nil)
	assert.Equal(t, monitors, kept)
	assert.Empty(t, excluded)
}

func TestFilterMonitorsByName_MatchesNone(t *testing.T) {
	monitors := []hyperping.Monitor{
		{UUID: "1", Name: "prod-api"},
		{UUID: "2", Name: "staging-api"},
	}
	rx := regexp.MustCompile(`\[DRILL`)
	kept, excluded := filterMonitorsByName(monitors, rx)
	assert.Equal(t, monitors, kept)
	assert.Empty(t, excluded)
}

func TestFilterMonitorsByName_MatchesSome(t *testing.T) {
	monitors := []hyperping.Monitor{
		{UUID: "1", Name: "[DRILL-TA]-PaymentAPI-NOOP"},
		{UUID: "2", Name: "prod-api"},
		{UUID: "3", Name: "[DRILL-TB]-CheckoutAPI-NOOP"},
	}
	rx := regexp.MustCompile(`\[DRILL`)
	kept, excluded := filterMonitorsByName(monitors, rx)

	assert.Len(t, kept, 1)
	assert.Equal(t, "prod-api", kept[0].Name)
	assert.Len(t, excluded, 2)
	assert.Equal(t, "[DRILL-TA]-PaymentAPI-NOOP", excluded[0].Name)
	assert.Equal(t, "[DRILL-TB]-CheckoutAPI-NOOP", excluded[1].Name)
}

func TestFilterMonitorsByName_MatchesAll(t *testing.T) {
	monitors := []hyperping.Monitor{
		{UUID: "1", Name: "[DRILL-TA]-PaymentAPI"},
		{UUID: "2", Name: "[DRILL-TB]-CheckoutAPI"},
	}
	rx := regexp.MustCompile(`\[DRILL`)
	kept, excluded := filterMonitorsByName(monitors, rx)
	assert.Empty(t, kept)
	assert.Len(t, excluded, 2)
}

func TestFilterMonitorsByName_EmptySlice(t *testing.T) {
	rx := regexp.MustCompile(`\[DRILL`)
	kept, excluded := filterMonitorsByName(nil, rx)
	assert.Empty(t, kept)
	assert.Empty(t, excluded)
}

func TestFilterMonitorsByName_Alternation(t *testing.T) {
	monitors := []hyperping.Monitor{
		{UUID: "1", Name: "[DRILL] prod"},
		{UUID: "2", Name: "[TEST] staging"},
		{UUID: "3", Name: "real-prod"},
	}
	rx := regexp.MustCompile(`\[DRILL|\[TEST`)
	kept, excluded := filterMonitorsByName(monitors, rx)
	assert.Len(t, kept, 1)
	assert.Equal(t, "real-prod", kept[0].Name)
	assert.Len(t, excluded, 2)
}

// --- filterOutagesByMonitorUUID ---

func TestFilterOutagesByMonitorUUID_AllIncluded(t *testing.T) {
	outages := []hyperping.Outage{
		{Monitor: hyperping.MonitorReference{UUID: "prod-1"}},
		{Monitor: hyperping.MonitorReference{UUID: "prod-2"}},
	}
	included := map[string]struct{}{"prod-1": {}, "prod-2": {}}
	result := filterOutagesByMonitorUUID(outages, included)
	assert.Len(t, result, 2)
}

func TestFilterOutagesByMonitorUUID_SomeExcluded(t *testing.T) {
	outages := []hyperping.Outage{
		{Monitor: hyperping.MonitorReference{UUID: "prod-1"}},
		{Monitor: hyperping.MonitorReference{UUID: "drill-1"}},
	}
	included := map[string]struct{}{"prod-1": {}}
	result := filterOutagesByMonitorUUID(outages, included)
	assert.Len(t, result, 1)
	assert.Equal(t, "prod-1", result[0].Monitor.UUID)
}

func TestFilterOutagesByMonitorUUID_EmptyIncluded(t *testing.T) {
	outages := []hyperping.Outage{
		{Monitor: hyperping.MonitorReference{UUID: "mon-1"}},
	}
	result := filterOutagesByMonitorUUID(outages, map[string]struct{}{})
	assert.Empty(t, result)
}

func TestFilterOutagesByMonitorUUID_NilOutages(t *testing.T) {
	result := filterOutagesByMonitorUUID(nil, map[string]struct{}{"mon-1": {}})
	assert.Empty(t, result)
}

func TestFilterOutagesByMonitorUUID_AllExcluded(t *testing.T) {
	outages := []hyperping.Outage{
		{Monitor: hyperping.MonitorReference{UUID: "drill-1"}},
		{Monitor: hyperping.MonitorReference{UUID: "drill-2"}},
	}
	included := map[string]struct{}{"prod-1": {}}
	result := filterOutagesByMonitorUUID(outages, included)
	assert.Empty(t, result)
}

// --- filterReportsByMonitorUUID ---

func TestFilterReportsByMonitorUUID_AllIncluded(t *testing.T) {
	reports := []hyperping.MonitorReport{
		{UUID: "a", Name: "Alpha", SLA: 99.0},
		{UUID: "b", Name: "Beta", SLA: 100.0},
	}
	included := map[string]struct{}{"a": {}, "b": {}}
	result := filterReportsByMonitorUUID(reports, included)
	assert.Equal(t, reports, result)
}

func TestFilterReportsByMonitorUUID_PartialInclusion(t *testing.T) {
	reports := []hyperping.MonitorReport{
		{UUID: "prod-1", Name: "prod-api", SLA: 99.0},
		{UUID: "drill-1", Name: "[DRILL]", SLA: 50.0},
		{UUID: "prod-2", Name: "prod-db", SLA: 100.0},
	}
	included := map[string]struct{}{"prod-1": {}, "prod-2": {}}
	result := filterReportsByMonitorUUID(reports, included)
	require.Len(t, result, 2)
	assert.Equal(t, "prod-1", result[0].UUID)
	assert.Equal(t, "prod-2", result[1].UUID)
}

func TestFilterReportsByMonitorUUID_AllExcluded(t *testing.T) {
	reports := []hyperping.MonitorReport{
		{UUID: "drill-1"},
		{UUID: "drill-2"},
	}
	result := filterReportsByMonitorUUID(reports, map[string]struct{}{"prod-1": {}})
	assert.Empty(t, result)
}

func TestFilterReportsByMonitorUUID_EmptyReports(t *testing.T) {
	result := filterReportsByMonitorUUID(nil, map[string]struct{}{"prod-1": {}})
	assert.Empty(t, result)
}
