// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package collector

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	hyperping "github.com/develeap/hyperping-go"
)

// ruleFile mirrors the subset of Prometheus rule-file schema we care about for
// the metric-reference audit. It deliberately ignores labels/annotations because
// metric typos in those fields are cosmetic, while typos in `expr:` produce
// silent alerts.
type ruleFile struct {
	Groups []struct {
		Name  string `yaml:"name"`
		Rules []struct {
			Alert  string `yaml:"alert,omitempty"`
			Record string `yaml:"record,omitempty"`
			Expr   string `yaml:"expr"`
		} `yaml:"rules"`
	} `yaml:"groups"`
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller failed")
	// internal/collector/deployconfig_test.go → repo root is two levels up
	return filepath.Join(filepath.Dir(file), "..", "..")
}

func loadRuleFile(t *testing.T, path string) ruleFile {
	t.Helper()
	bytes, err := os.ReadFile(path)
	require.NoError(t, err, "read %s", path)
	var rf ruleFile
	require.NoError(t, yaml.Unmarshal(bytes, &rf), "parse %s", path)
	return rf
}

// TestPrometheusRulesReferenceOnlyEmittedMetrics catches the class of bug
// fixed in this PR's predecessor: an alert references a metric name that the
// exporter does not emit (e.g. `hyperping_monitors_total` when the actual
// metric is `hyperping_monitors`). promtool check rules validates PromQL
// syntax but cannot know what the exporter emits, so the alert silently
// produces no series and never fires.
//
// The test extracts every `hyperping_*` and `hyperping:*` identifier from
// each `expr:` field in alerts.yml and recording-rules.yml, and asserts that
// each one is either an emitted Prometheus descriptor or a recording-rule
// output name.
func TestPrometheusRulesReferenceOnlyEmittedMetrics(t *testing.T) {
	// Build a populated collector covering every metric family emitted by the
	// codebase. Any descriptor whose emission depends on data presence (SSL,
	// regions, MCP indexes, reports, etc.) needs at least one entity here, or
	// Gather() will silently drop that family and the rules audit below will
	// flag valid alert references as "undefined".
	known := gatherEmittedMetricNames(t)

	root := repoRoot(t)
	rules := loadRuleFile(t, filepath.Join(root, "deploy", "prometheus", "recording-rules.yml"))
	alerts := loadRuleFile(t, filepath.Join(root, "deploy", "prometheus", "alerts.yml"))

	// Recording-rule outputs become valid references for downstream rules and alerts.
	for _, g := range rules.Groups {
		for _, r := range g.Rules {
			if r.Record != "" {
				known[r.Record] = struct{}{}
			}
		}
	}

	refRx := regexp.MustCompile(`hyperping[_:][a-zA-Z_:0-9]+`)

	var problems []string
	check := func(file string, rf ruleFile) {
		for _, g := range rf.Groups {
			for _, r := range g.Rules {
				name := r.Alert
				kind := "alert"
				if name == "" {
					name = r.Record
					kind = "rule"
				}
				seen := make(map[string]struct{})
				for _, ref := range refRx.FindAllString(r.Expr, -1) {
					if _, dup := seen[ref]; dup {
						continue
					}
					seen[ref] = struct{}{}
					if _, ok := known[ref]; !ok {
						problems = append(problems,
							fmt.Sprintf("%s: %s %q references undefined metric %q", file, kind, name, ref))
					}
				}
			}
		}
	}
	check("alerts.yml", alerts)
	check("recording-rules.yml", rules)

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Errorf("%d undefined metric reference(s) in deploy/prometheus/:\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// gatherEmittedMetricNames builds a fully-populated collector, registers it
// with a pedantic registry, calls Gather(), and returns the set of emitted
// metric family names. Compared to parsing prometheus.Desc.String(), this
// reads names through client_golang's documented public API and does not
// depend on internal Desc formatting.
func gatherEmittedMetricNames(t *testing.T) map[string]struct{} {
	t.Helper()

	sslDays := 30
	endTime := "2026-04-26T11:00:00Z"
	monitor := hyperping.Monitor{
		UUID:           "mon_1",
		Name:           "[acme]-Web",
		URL:            "https://example.com",
		Protocol:       "http",
		HTTPMethod:     "GET",
		ProjectUUID:    "proj_1",
		Status:         "down",
		CheckFrequency: 60,
		SSLExpiration:  &sslDays,
		Regions:        []string{"us-east", "eu-west"},
		EscalationPolicy: &hyperping.EscalationPolicyRef{
			UUID: "ep_1",
			Name: "Core",
		},
	}
	api := &mockAPI{
		monitors:     []hyperping.Monitor{monitor},
		healthchecks: []hyperping.Healthcheck{{UUID: "hc_1", Name: "Backup", Period: 300}},
		outages: []hyperping.Outage{{
			UUID:               "out_1",
			StartDate:          "2026-04-26T10:00:00Z",
			EndDate:            nil,
			StatusCode:         500,
			IsResolved:         false,
			DetectedLocation:   "us-east",
			ConfirmedLocations: "us-east",
			Monitor:            hyperping.MonitorReference{UUID: "mon_1", Name: "[acme]-Web"},
		}},
		maintenanceWindows: []hyperping.Maintenance{{
			UUID:     "mw_1",
			Name:     "Planned",
			Status:   "ongoing",
			Monitors: []string{"mon_1"},
		}},
		incidents: []hyperping.Incident{{UUID: "inc_1", Type: "investigating"}},
		reports: []hyperping.MonitorReport{{
			UUID:     "mon_1",
			Name:     "[acme]-Web",
			Protocol: "http",
			Period:   hyperping.ReportPeriod{From: "2026-03-27T00:00:00Z", To: endTime},
			SLA:      99.5,
			MTTR:     120,
			Outages: hyperping.OutageStats{
				Count:         1,
				TotalDowntime: 60,
				LongestOutage: 60,
			},
		}},
	}

	c := NewCollector(api, nil, 60*time.Second, newTestLogger(), "hyperping")
	c.Refresh(context.Background())

	// MCP-derived metric families are emitted only when the per-monitor index
	// has data. The MCP client is a concrete *hyperping.MCPClient (not an
	// interface), so populate the cache directly to make those families appear.
	// Refresh() with mcp == nil overwrites the indexes with nil maps via the
	// (mcpData{}, nil) return from fetchMcpData; reallocate before writing.
	c.mu.Lock()
	c.responseTimeIndex = map[string]float64{"mon_1": 0.42}
	c.mttaIndex = map[string]float64{"mon_1": 30.0}
	c.anomalyCountIndex = map[string]int{"mon_1": 1}
	c.anomalyScoreIndex = map[string]float64{"mon_1": 0.5}
	c.totalAlerts = 1
	c.mu.Unlock()

	registry := prometheus.NewPedanticRegistry()
	require.NoError(t, registry.Register(c))

	families, err := registry.Gather()
	require.NoError(t, err)

	known := make(map[string]struct{}, len(families))
	for _, mf := range families {
		known[mf.GetName()] = struct{}{}
	}
	return known
}
