// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package collector

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
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
	// Sanity check: the regex depends on prometheus/client_golang's Desc.String()
	// format, which is not a documented stability guarantee. If a future client
	// upgrade silently changes the format in a way the regex still matches but
	// extracts garbage, we'd lose all known names and every reference would
	// register as "undefined". This bound catches that failure mode early.
	require.GreaterOrEqual(t, len(known), 30,
		"extracted only %d metric names from Desc.String(); the parsing regex may have stopped working — check client_golang format", len(known))

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

// --- Helm chart render tests for cache-mode / tier TTLs (chunk 9) ---
//
// These tests shell out to `helm template` against the chart with inline
// --set overrides and assert on the Deployment args list. They are skipped
// gracefully when helm is not on PATH (matches the convention in the
// existing python render harness).

// helmTemplate runs `helm template testrel <chart> --set k=v ...` and returns
// the rendered YAML or an error if helm exits non-zero (e.g. validation
// fail()). t.Skip is called when helm is not on PATH.
func helmTemplate(t *testing.T, setFlags ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}
	root := repoRoot(t)
	chart := filepath.Join(root, "deploy", "helm", "hyperping-exporter")
	args := []string{"template", "testrel", chart, "--set", "config.apiKey=devonly"}
	for _, s := range setFlags {
		args = append(args, "--set", s)
	}
	// args originate from test code (a fixed prefix + caller-supplied --set
	// flags from the same package), never from external input. The helm
	// binary path is resolved from PATH on the CI runner. Safe by
	// construction; gosec G204 false positive.
	cmd := exec.Command("helm", args...) // #nosec G204
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// deploymentArgsFromHelm extracts the first container's args slice from the
// rendered Deployment manifest. Returns an empty slice if not found, so a
// test that expects a flag to be absent can assert directly.
func deploymentArgsFromHelm(t *testing.T, rendered string) []string {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			break
		}
		if doc == nil {
			continue
		}
		if doc["kind"] != "Deployment" {
			continue
		}
		spec, _ := doc["spec"].(map[string]any)
		template, _ := spec["template"].(map[string]any)
		podSpec, _ := template["spec"].(map[string]any)
		containers, _ := podSpec["containers"].([]any)
		if len(containers) == 0 {
			break
		}
		first, _ := containers[0].(map[string]any)
		rawArgs, _ := first["args"].([]any)
		args := make([]string, 0, len(rawArgs))
		for _, a := range rawArgs {
			if s, ok := a.(string); ok {
				args = append(args, s)
			}
		}
		return args
	}
	return nil
}

func TestDeployment_CacheMode_Legacy_OmitsTierFlags(t *testing.T) {
	rendered, err := helmTemplate(t)
	require.NoError(t, err, "default render must succeed: %s", rendered)

	args := deploymentArgsFromHelm(t, rendered)
	require.NotEmpty(t, args, "deployment args must not be empty")
	joined := strings.Join(args, " ")
	assert.NotContains(t, joined, "--cache-mode", "legacy default must NOT emit --cache-mode")
	assert.NotContains(t, joined, "--hot-ttl", "legacy default must NOT emit --hot-ttl")
	assert.NotContains(t, joined, "--warm-ttl", "legacy default must NOT emit --warm-ttl")
	assert.NotContains(t, joined, "--cold-ttl", "legacy default must NOT emit --cold-ttl")
}

func TestDeployment_CacheMode_Tiered_RendersTierFlags(t *testing.T) {
	rendered, err := helmTemplate(t,
		"config.cacheMode=tiered",
		"config.hotTTL=45s",
		"config.warmTTL=4m",
		"config.coldTTL=20m",
	)
	require.NoError(t, err, "tiered render must succeed: %s", rendered)

	args := deploymentArgsFromHelm(t, rendered)
	require.NotEmpty(t, args)
	joined := strings.Join(args, " ")
	assert.Contains(t, joined, "--cache-mode=tiered")
	assert.Contains(t, joined, "--hot-ttl=45s")
	assert.Contains(t, joined, "--warm-ttl=4m")
	assert.Contains(t, joined, "--cold-ttl=20m")
}

func TestDeployment_TierTTLs_BelowFloor_FailsRender(t *testing.T) {
	// hotTTL below the 30s floor must abort the render via
	// validateTierTTLs. Same error shape as validateCacheTTL.
	_, err := helmTemplate(t,
		"config.cacheMode=tiered",
		"config.hotTTL=10s",
	)
	require.Error(t, err, "hotTTL below floor must fail the render")
}
