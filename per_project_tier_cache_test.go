// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

// Per-project cache tier override tests (work item: feat/per-project-tier-cache).
//
// These tests pin down the contract for an OPTIONAL `cache:` block on each
// projects-file entry that overrides any subset of the four global tiered-mode
// knobs (hotTTL, warmTTL, coldTTL, hotEnabled, warmEnabled, coldEnabled).
//
// Semantics under test:
//
//   - Absent `cache:` block on a project = inherit every global value
//     byte-for-byte (TestPerProjectTierCache_AbsentBlockInheritsGlobals).
//   - cache.<tier>TTL overrides the corresponding global for that project
//     only (TestPerProjectTierCache_PartialOverride_TTLs).
//   - cache.<tier>Enabled: false skips that tier's ticker entirely for
//     the project (no API calls, no series). hotEnabled is the exception:
//     a project with hotEnabled: false is a hard parse error because no
//     HOT means no scrape at all
//     (TestPerProjectTierCache_HotEnabledFalseIsFatal,
//      TestPerProjectTierCache_WarmDisabled,
//      TestPerProjectTierCache_ColdDisabled).
//   - <tier>TTL alongside <tier>Enabled: false: WARN at parse time,
//     enabled wins, TTL ignored. NOT a fatal error so config-merging
//     tooling does not blow up; we surface it via warnings written to
//     stderr (TestPerProjectTierCache_TTLWithEnabledFalseWarnsButPasses).
//   - Backward compat: a projects-file with no `cache:` blocks anywhere
//     produces the same effective tier configuration as the pre-feature
//     binary, i.e. (hotTTL, warmTTL, coldTTL) = (cfg.hotTTL, cfg.warmTTL,
//     cfg.coldTTL) and all three tiers enabled
//     (TestPerProjectTierCache_BackwardCompat_NoCacheBlockAnywhere).
//
// File-level note: tests assume `*time.Duration` and `*bool` pointer
// semantics on the new fields so "absent" cleanly distinguishes from
// "zero". That choice is locked in by these tests; flipping to value
// types would make the absent-vs-zero ambiguity surface here first.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeProjectsFileForCache is a local helper so this file is self-contained.
// (writeProjectsFile lives in multiproject_config_test.go; same package, same
// behaviour, but reused under a distinct name keeps the new test file readable
// without changing the existing helper's signature.)
func writeProjectsFileForCache(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "projects.yaml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

// loadCacheFixture wires the projects-file path through parseConfigOut with a
// captured stderr buffer so tests can assert on warning text. Returns the
// resolved cfg, the captured stderr, and the success flag from parseConfig.
func loadCacheFixture(t *testing.T, body string, extraFlags ...string) (config, string, bool) {
	t.Helper()
	path := writeProjectsFileForCache(t, body)
	args := []string{"test", "--projects-file", path, "--cache-mode", "tiered"}
	args = append(args, extraFlags...)
	resetFlags(t, args)
	t.Setenv("HYPERPING_API_KEY", "")
	os.Unsetenv("HYPERPING_API_KEY")
	os.Unsetenv("HYPERPING_PROJECTS_FILE")
	var buf strings.Builder
	cfg, ok := parseConfigOut(&buf)
	return cfg, buf.String(), ok
}

// TestPerProjectTierCache_AbsentBlockInheritsGlobals: a project without a
// `cache:` block uses the global TTLs. Critical back-compat invariant.
func TestPerProjectTierCache_AbsentBlockInheritsGlobals(t *testing.T) {
	body := `
- id: hyp_core
  apiKey: a
- id: hyp_infra
  apiKey: b
`
	cfg, _, ok := loadCacheFixture(t, body,
		"--hot-ttl", "45s",
		"--warm-ttl", "7m",
		"--cold-ttl", "20m",
	)
	require.True(t, ok)
	require.Len(t, cfg.projects, 2)

	for i, p := range cfg.projects {
		hot, warm, cold := effectiveTierTTLs(p, cfg)
		assert.Equal(t, 45*time.Second, hot, "project %d hot ttl must inherit global", i)
		assert.Equal(t, 7*time.Minute, warm, "project %d warm ttl must inherit global", i)
		assert.Equal(t, 20*time.Minute, cold, "project %d cold ttl must inherit global", i)
		hotE, warmE, coldE := effectiveTierEnabled(p)
		assert.True(t, hotE, "project %d hot tier must be enabled (absent block defaults true)", i)
		assert.True(t, warmE, "project %d warm tier must be enabled", i)
		assert.True(t, coldE, "project %d cold tier must be enabled", i)
	}
}

// TestPerProjectTierCache_PartialOverride_TTLs: only the set fields override;
// unset fields still inherit the global.
func TestPerProjectTierCache_PartialOverride_TTLs(t *testing.T) {
	body := `
- id: hyp_infra
  apiKey: a
  cache:
    warmTTL: 30m
    coldTTL: 2h
`
	cfg, _, ok := loadCacheFixture(t, body,
		"--hot-ttl", "60s",
		"--warm-ttl", "5m",
		"--cold-ttl", "15m",
	)
	require.True(t, ok)
	require.Len(t, cfg.projects, 1)

	hot, warm, cold := effectiveTierTTLs(cfg.projects[0], cfg)
	assert.Equal(t, 60*time.Second, hot, "hot must inherit global (unset in cache block)")
	assert.Equal(t, 30*time.Minute, warm, "warm must use the per-project override")
	assert.Equal(t, 2*time.Hour, cold, "cold must use the per-project override")
}

// TestPerProjectTierCache_HotEnabledFalseIsFatal: a project with
// hotEnabled: false is rejected at parse time. HOT is required because
// /readyz and every per-monitor up/down series depend on it.
func TestPerProjectTierCache_HotEnabledFalseIsFatal(t *testing.T) {
	body := `
- id: bad_project
  apiKey: a
  cache:
    hotEnabled: false
`
	_, stderr, ok := loadCacheFixture(t, body)
	assert.False(t, ok, "hotEnabled: false must be a fatal config error")
	assert.Contains(t, stderr, "hotEnabled",
		"error message must name the offending field so operators can locate it")
}

// TestPerProjectTierCache_WarmDisabled: warmEnabled: false skips the WARM
// ticker for that project. Other projects and other tiers are untouched.
func TestPerProjectTierCache_WarmDisabled(t *testing.T) {
	body := `
- id: hyp_thirdparty
  apiKey: a
  cache:
    warmEnabled: false
`
	cfg, _, ok := loadCacheFixture(t, body)
	require.True(t, ok)
	require.Len(t, cfg.projects, 1)

	hotE, warmE, coldE := effectiveTierEnabled(cfg.projects[0])
	assert.True(t, hotE, "hot stays enabled")
	assert.False(t, warmE, "warm must be disabled")
	assert.True(t, coldE, "cold stays enabled (independent flag)")
}

// TestPerProjectTierCache_ColdDisabled mirrors the warm-disabled case for
// the COLD tier so cold's enable flag is plumbed end-to-end.
func TestPerProjectTierCache_ColdDisabled(t *testing.T) {
	body := `
- id: hyp_thirdparty
  apiKey: a
  cache:
    coldEnabled: false
`
	cfg, _, ok := loadCacheFixture(t, body)
	require.True(t, ok)
	require.Len(t, cfg.projects, 1)

	hotE, warmE, coldE := effectiveTierEnabled(cfg.projects[0])
	assert.True(t, hotE)
	assert.True(t, warmE)
	assert.False(t, coldE, "cold must be disabled")
}

// TestPerProjectTierCache_TTLWithEnabledFalseWarnsButPasses: setting a TTL on
// the same tier that is disabled emits a warning but does NOT fail. Enabled
// wins; the TTL is ignored. This is the behaviour config-merging tooling
// (overlays, kustomize-style patches) needs so that a partial override does
// not blow up an otherwise-valid config.
func TestPerProjectTierCache_TTLWithEnabledFalseWarnsButPasses(t *testing.T) {
	body := `
- id: hyp_thirdparty
  apiKey: a
  cache:
    warmEnabled: false
    warmTTL: 30m
`
	cfg, stderr, ok := loadCacheFixture(t, body)
	require.True(t, ok, "TTL alongside Enabled:false must NOT fail; got stderr=%q", stderr)
	require.Len(t, cfg.projects, 1)
	_, warmE, _ := effectiveTierEnabled(cfg.projects[0])
	assert.False(t, warmE, "Enabled:false must still win")
	assert.Contains(t, strings.ToLower(stderr), "warn",
		"a warning must be surfaced when TTL is set alongside Enabled:false")
	assert.Contains(t, stderr, "warmTTL",
		"warning must name the ignored field so operators can locate the dead config")
}

// TestPerProjectTierCache_BackwardCompat_NoCacheBlockAnywhere is the
// explicit byte-identical guarantee for upgrades. A projects-file with NO
// cache blocks anywhere must yield the same (hot, warm, cold) effective
// TTLs as the pre-feature binary (which used the globals verbatim) and
// must leave all three tiers enabled.
func TestPerProjectTierCache_BackwardCompat_NoCacheBlockAnywhere(t *testing.T) {
	body := `
- id: hyp_core
  apiKey: a
- id: hyp_infra
  apiKey: b
- id: hyp_thirdparty
  apiKey: c
`
	cfg, stderr, ok := loadCacheFixture(t, body,
		"--hot-ttl", "60s",
		"--warm-ttl", "5m",
		"--cold-ttl", "15m",
	)
	require.True(t, ok)
	require.NotContains(t, strings.ToLower(stderr), "warn",
		"a no-override config must produce ZERO warnings; got %q", stderr)
	require.Len(t, cfg.projects, 3)

	for _, p := range cfg.projects {
		hot, warm, cold := effectiveTierTTLs(p, cfg)
		assert.Equal(t, cfg.hotTTL, hot, "project %q hot must equal global", p.ID)
		assert.Equal(t, cfg.warmTTL, warm, "project %q warm must equal global", p.ID)
		assert.Equal(t, cfg.coldTTL, cold, "project %q cold must equal global", p.ID)
		hotE, warmE, coldE := effectiveTierEnabled(p)
		assert.True(t, hotE)
		assert.True(t, warmE)
		assert.True(t, coldE)
	}
}

// TestPerProjectTierCache_RejectsNonPositiveTTL: time.NewTicker panics
// on d <= 0, so a cache.warmTTL: 0s would crash the binary when the WARM
// goroutine launches. The parser must reject non-positive per-project
// TTLs at config load. Covers hot/warm/cold for symmetry.
func TestPerProjectTierCache_RejectsNonPositiveTTL(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		field string
	}{
		{
			name:  "hotTTL zero",
			body:  "- id: p\n  apiKey: k\n  cache:\n    hotTTL: 0s\n",
			field: "hotTTL",
		},
		{
			name:  "warmTTL zero",
			body:  "- id: p\n  apiKey: k\n  cache:\n    warmTTL: 0s\n",
			field: "warmTTL",
		},
		{
			name:  "coldTTL negative",
			body:  "- id: p\n  apiKey: k\n  cache:\n    coldTTL: -5m\n",
			field: "coldTTL",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, ok := loadCacheFixture(t, tc.body)
			assert.False(t, ok, "non-positive %s must be a fatal config error", tc.field)
			assert.Contains(t, stderr, tc.field,
				"error message must name the offending field; got %q", stderr)
		})
	}
}

// TestPerProjectTierCache_StartupLogIncludesEffectiveConfig: D4 contract.
// run() emits a structured log line per project showing the effective TTLs
// and enable flags so operators can verify what's running without reading
// source. The line carries the project id as an attribute so a multi-tenant
// log search can pinpoint the failing project.
func TestPerProjectTierCache_StartupLogIncludesEffectiveConfig(t *testing.T) {
	body := `
- id: hyp_infra
  apiKey: a
  cache:
    warmTTL: 30m
    coldEnabled: false
`
	cfg, _, ok := loadCacheFixture(t, body,
		"--hot-ttl", "60s",
		"--warm-ttl", "5m",
		"--cold-ttl", "15m",
	)
	require.True(t, ok)
	require.Len(t, cfg.projects, 1)

	var buf strings.Builder
	logEffectiveTierConfig(cfg, jsonLoggerTo(&buf))
	out := buf.String()
	assert.Contains(t, out, "hyp_infra", "log line must name the project")
	assert.Contains(t, out, "30m0s", "log line must show the resolved warmTTL")
	// time.Duration.String() normalises 60s to "1m0s"; assert on that
	// canonical form rather than the operator-facing input string.
	assert.Contains(t, out, "1m0s", "log line must show inherited hotTTL (normalised by time.Duration.String)")
	// coldEnabled=false must appear as a structured attribute, regardless of
	// JSON key ordering. We assert on the substring "cold_enabled" (the snake
	// case slog key) and the value "false".
	assert.Contains(t, out, "cold_enabled")
	assert.Contains(t, out, "false")
}
