// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package collector

import (
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
	// Collect emitted metric fqNames from the live collector descriptors.
	c := NewCollector(&mockAPI{}, nil, 60*time.Second, newTestLogger(), "hyperping")
	ch := make(chan *prometheus.Desc, 64)
	c.Describe(ch)
	close(ch)

	fqNameRx := regexp.MustCompile(`fqName:\s*"([^"]+)"`)
	known := make(map[string]struct{})
	for d := range ch {
		m := fqNameRx.FindStringSubmatch(d.String())
		require.NotNil(t, m, "could not parse fqName from %s", d.String())
		known[m[1]] = struct{}{}
	}

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
