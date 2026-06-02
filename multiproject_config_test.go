// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

// Multi-project configuration tests (work item: exporter-config-multiproject).
// These tests pin down the contract for the new --projects-file flag and the
// `projects` slice on `type config`. They are intentionally written before the
// implementation lands; running `go test` now must produce compile-time
// failures pointing at the missing config.projects field / missing
// projectConfig type / missing --projects-file flag handling.
//
// Design notes that the implementation must satisfy:
//   - Single-project legacy path (--api-key / --api-key-file / HYPERPING_API_KEY
//     without --projects-file) MUST synthesise a single projectConfig with
//     ID="default" so the rest of the binary always iterates a non-empty list.
//   - --projects-file accepts a YAML list of {id, apiKey, apiKeyFile?,
//     mcpUrl?, excludeNamePattern?}.
//   - Mutual exclusion: --api-key (or --api-key-file or HYPERPING_API_KEY)
//     combined with --projects-file is a hard error.
//   - Each project ID must match [a-zA-Z0-9._-]{1,64} (same alphabet as the
//     tenant regex so the constLabel cannot inject invalid characters into
//     Prometheus label values).
//   - Duplicate IDs are a hard error.
//   - Per-project mcpUrl / excludeNamePattern override the global defaults
//     when set; absence falls back to the globals.

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeProjectsFile is a tiny helper that materialises a YAML fixture and
// returns its path. Tests use t.TempDir so each subtest is hermetic.
func writeProjectsFile(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "projects.yaml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

// TestParseConfig_SingleProjectLegacyPath: --api-key=k1 (no --projects-file)
// yields one project with id='default' and apiKey='k1'. The legacy single-key
// shape MUST be preserved for back-compat: existing operators who do not
// know about --projects-file see no observable change in flag semantics.
func TestParseConfig_SingleProjectLegacyPath(t *testing.T) {
	resetFlags(t, []string{"test"})
	t.Setenv("HYPERPING_API_KEY", "k1")
	t.Setenv("HYPERPING_PROJECTS_FILE", "")
	os.Unsetenv("HYPERPING_PROJECTS_FILE")

	cfg, ok := parseConfig()
	require.True(t, ok)
	require.Len(t, cfg.projects, 1, "legacy single-key path must synthesise one project")
	assert.Equal(t, "default", cfg.projects[0].ID,
		"legacy path must use id='default' so every metric carries a stable project constLabel")
	assert.Equal(t, "k1", cfg.projects[0].APIKey)
}

// TestParseConfig_ProjectsFileBasic: --projects-file=fixtures/projects-multi.yaml
// yields []projectConfig{{id:'hyp_core',apiKey:'a'},{id:'hyp_infra',apiKey:'b'}}.
func TestParseConfig_ProjectsFileBasic(t *testing.T) {
	body := `
- id: hyp_core
  apiKey: a
- id: hyp_infra
  apiKey: b
`
	path := writeProjectsFile(t, body)
	resetFlags(t, []string{"test", "--projects-file", path})
	t.Setenv("HYPERPING_API_KEY", "")
	os.Unsetenv("HYPERPING_API_KEY")
	os.Unsetenv("HYPERPING_PROJECTS_FILE")

	cfg, ok := parseConfig()
	require.True(t, ok)
	require.Len(t, cfg.projects, 2)
	assert.Equal(t, "hyp_core", cfg.projects[0].ID)
	assert.Equal(t, "a", cfg.projects[0].APIKey)
	assert.Equal(t, "hyp_infra", cfg.projects[1].ID)
	assert.Equal(t, "b", cfg.projects[1].APIKey)
}

// TestParseConfig_ProjectsFileEnvOverride: HYPERPING_PROJECTS_FILE env is
// honored when --projects-file is absent (matches the existing flag/env
// precedence pattern used by --api-key / HYPERPING_API_KEY).
func TestParseConfig_ProjectsFileEnvOverride(t *testing.T) {
	body := `
- id: only_one
  apiKey: k
`
	path := writeProjectsFile(t, body)
	resetFlags(t, []string{"test"})
	t.Setenv("HYPERPING_API_KEY", "")
	os.Unsetenv("HYPERPING_API_KEY")
	t.Setenv("HYPERPING_PROJECTS_FILE", path)

	cfg, ok := parseConfig()
	require.True(t, ok)
	require.Len(t, cfg.projects, 1)
	assert.Equal(t, "only_one", cfg.projects[0].ID)
	assert.Equal(t, "k", cfg.projects[0].APIKey)
}

// TestParseConfig_RejectsBothApiKeyAndProjects: providing --api-key AND
// --projects-file errors out with an explicit conflict message. Letting both
// through would silently shadow one source; explicit refusal forces operators
// to pick one mode of configuration.
func TestParseConfig_RejectsBothApiKeyAndProjects(t *testing.T) {
	path := writeProjectsFile(t, "- id: p\n  apiKey: k\n")
	resetFlags(t, []string{"test", "--api-key", "k1", "--projects-file", path})
	t.Setenv("HYPERPING_API_KEY", "")
	os.Unsetenv("HYPERPING_API_KEY")
	os.Unsetenv("HYPERPING_PROJECTS_FILE")

	_, ok := parseConfig()
	assert.False(t, ok, "--api-key combined with --projects-file must be rejected at parse time")
}

// TestParseConfig_RejectsDuplicateProjectID: projects file with two entries
// id='hyp_core' errors. Duplicate IDs would collide on the project constLabel
// and produce "duplicate metrics collector registration attempted" at startup.
func TestParseConfig_RejectsDuplicateProjectID(t *testing.T) {
	body := `
- id: hyp_core
  apiKey: a
- id: hyp_core
  apiKey: b
`
	path := writeProjectsFile(t, body)
	resetFlags(t, []string{"test", "--projects-file", path})
	t.Setenv("HYPERPING_API_KEY", "")
	os.Unsetenv("HYPERPING_API_KEY")
	os.Unsetenv("HYPERPING_PROJECTS_FILE")

	_, ok := parseConfig()
	assert.False(t, ok, "duplicate project IDs must be rejected before Collector construction")
}

// TestParseConfig_RejectsEmptyOrInvalidID: id must match [a-zA-Z0-9._-]{1,64};
// empty / whitespace / invalid char rejected. Same alphabet as the tenant
// regex so the constLabel cannot inject characters that downstream label
// matchers (recording-rules, alert selectors) cannot escape.
func TestParseConfig_RejectsEmptyOrInvalidID(t *testing.T) {
	cases := []struct {
		name string
		id   string
	}{
		{"empty", ""},
		{"whitespace", "   "},
		{"contains space", "hyp core"},
		{"contains slash", "hyp/core"},
		{"contains exclamation", "hyp!"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := "- id: \"" + tc.id + "\"\n  apiKey: k\n"
			path := writeProjectsFile(t, body)
			resetFlags(t, []string{"test", "--projects-file", path})
			t.Setenv("HYPERPING_API_KEY", "")
			os.Unsetenv("HYPERPING_API_KEY")
			os.Unsetenv("HYPERPING_PROJECTS_FILE")

			_, ok := parseConfig()
			assert.False(t, ok, "invalid project id %q must be rejected", tc.id)
		})
	}
}

// TestParseConfig_RejectsInvalidPerProjectMcpUrl: per-project mcpUrl must
// satisfy the same scheme rule as the global --mcp-url flag (https:// or
// http://localhost). Without this check a typo like "htttps://..." would
// route the per-project API key to an attacker-controlled plaintext host.
func TestParseConfig_RejectsInvalidPerProjectMcpUrl(t *testing.T) {
	cases := []struct {
		name   string
		mcpURL string
	}{
		{"typo_scheme", "htttps://mcp.example.com/v1/mcp"},
		{"plain_http_remote", "http://mcp.example.com/v1/mcp"},
		{"ftp", "ftp://mcp.example.com/v1/mcp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := "- id: p\n  apiKey: k\n  mcpUrl: " + tc.mcpURL + "\n"
			path := writeProjectsFile(t, body)
			resetFlags(t, []string{"test", "--projects-file", path})
			t.Setenv("HYPERPING_API_KEY", "")
			os.Unsetenv("HYPERPING_API_KEY")
			os.Unsetenv("HYPERPING_PROJECTS_FILE")

			_, ok := parseConfig()
			assert.False(t, ok, "per-project mcpUrl %q must be rejected at parse time", tc.mcpURL)
		})
	}
}

// TestParseConfig_PerProjectOptionalOverrides: per-project mcpUrl and
// excludeNamePattern in projects-file override globals; absence falls back to
// globals. The same RE2 pattern can apply per-project (regulated tenants on
// different Hyperping plans may exclude different drill series).
func TestParseConfig_PerProjectOptionalOverrides(t *testing.T) {
	body := `
- id: hyp_core
  apiKey: a
  mcpUrl: https://mcp.hyperping.io/v1/mcp
  excludeNamePattern: '\[DRILL'
- id: hyp_infra
  apiKey: b
`
	path := writeProjectsFile(t, body)
	resetFlags(t, []string{
		"test",
		"--projects-file", path,
		"--mcp-url", "https://global.example.com/mcp",
		"--exclude-name-pattern", `\[GLOBAL`,
	})
	t.Setenv("HYPERPING_API_KEY", "")
	os.Unsetenv("HYPERPING_API_KEY")
	os.Unsetenv("HYPERPING_PROJECTS_FILE")

	cfg, ok := parseConfig()
	require.True(t, ok)
	require.Len(t, cfg.projects, 2)

	// hyp_core: per-project overrides win.
	assert.Equal(t, "https://mcp.hyperping.io/v1/mcp", cfg.projects[0].MCPURL,
		"per-project mcpUrl must override the global --mcp-url")
	assert.Equal(t, `\[DRILL`, cfg.projects[0].ExcludeNamePattern,
		"per-project excludeNamePattern must override the global --exclude-name-pattern")

	// hyp_infra: absence falls back to globals.
	assert.Equal(t, "https://global.example.com/mcp", cfg.projects[1].MCPURL,
		"absent per-project mcpUrl must fall back to the global --mcp-url")
	assert.Equal(t, `\[GLOBAL`, cfg.projects[1].ExcludeNamePattern,
		"absent per-project excludeNamePattern must fall back to the global --exclude-name-pattern")
}
