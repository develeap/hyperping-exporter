// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResolvePeriods_Default covers the absent / empty -> ["24h"] path
// that keeps a pre-1.8 projects-file byte-identical on the wire.
func TestResolvePeriods_Default(t *testing.T) {
	t.Run("nil input defaults to 24h", func(t *testing.T) {
		out, err := resolvePeriods(nil)
		require.NoError(t, err)
		assert.Equal(t, []string{"24h"}, out)
	})

	t.Run("empty slice defaults to 24h", func(t *testing.T) {
		out, err := resolvePeriods([]string{})
		require.NoError(t, err)
		assert.Equal(t, []string{"24h"}, out)
	})

	t.Run("returned slice is independent of defaultPeriods", func(t *testing.T) {
		// Mutating the returned slice must not affect the package-level
		// default; otherwise a misbehaving caller could poison every
		// subsequent project's resolved periods.
		out, err := resolvePeriods(nil)
		require.NoError(t, err)
		out[0] = "POISONED"
		assert.Equal(t, []string{"24h"}, defaultPeriods, "defaultPeriods leaked into caller mutation")
	})
}

// TestResolvePeriods_AllowedTokens documents every legal input by
// exhaustively asserting the full opt-in set parses to itself.
func TestResolvePeriods_AllowedTokens(t *testing.T) {
	full := []string{"24h", "7d", "30d", "90d", "365d"}
	out, err := resolvePeriods(full)
	require.NoError(t, err)
	assert.Equal(t, full, out)
}

// TestResolvePeriods_InvalidToken rejects any token outside the closed
// set with an actionable error message (the offending token + the
// allowed-list, no stack trace required).
func TestResolvePeriods_InvalidToken(t *testing.T) {
	cases := []string{"42d", "1h", "all", "", "  24h", "30D", "1y"}
	for _, bad := range cases {
		bad := bad
		t.Run(bad, func(t *testing.T) {
			_, err := resolvePeriods([]string{bad})
			require.Error(t, err)
			assert.Contains(t, err.Error(), bad,
				"error message must echo the offending token so operators can fix the YAML")
			assert.Contains(t, err.Error(), "24h",
				"error message must mention an allowed token as a hint")
		})
	}
}

// TestResolvePeriods_Duplicates collapses duplicates to a single entry,
// preserving first-seen order. Documented (per the spec's open-ended
// "your call; document either way") as dedup-not-reject because
// config-overlay tooling can legitimately stack the same key.
func TestResolvePeriods_Duplicates(t *testing.T) {
	out, err := resolvePeriods([]string{"24h", "7d", "24h", "30d", "7d"})
	require.NoError(t, err)
	assert.Equal(t, []string{"24h", "7d", "30d"}, out)
}

// TestPeriodToTier covers the closed mapping. Any future tier expansion
// must update this table alongside the production mapping.
func TestPeriodToTier(t *testing.T) {
	cases := map[string]string{
		"24h":  "warm",
		"7d":   "cold",
		"30d":  "cold",
		"90d":  "cold",
		"365d": "cold",
		"42d":  "", // unknown -> empty
	}
	for p, want := range cases {
		assert.Equal(t, want, periodToTier(p), "periodToTier(%q)", p)
	}
}

// TestLoadProjectsFile_PeriodsParsed verifies a projects-file with an
// explicit periods list survives parse + validate intact.
func TestLoadProjectsFile_PeriodsParsed(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key.txt")
	require.NoError(t, os.WriteFile(keyFile, []byte("test-key"), 0o600))

	yaml := `- id: alpha
  apiKeyFile: ` + keyFile + `
  periods: ["24h", "7d", "30d", "90d", "365d"]
`
	path := filepath.Join(dir, "projects.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))

	projects, err := loadProjectsFile(path, "", "", nil, os.Stderr)
	require.NoError(t, err)
	require.Len(t, projects, 1)
	assert.Equal(t, []string{"24h", "7d", "30d", "90d", "365d"}, projects[0].Periods)
}

// TestLoadProjectsFile_PeriodsAbsent confirms the default ["24h"] is
// injected when the key is omitted, preserving pre-1.8 behaviour.
func TestLoadProjectsFile_PeriodsAbsent(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key.txt")
	require.NoError(t, os.WriteFile(keyFile, []byte("test-key"), 0o600))

	yaml := `- id: alpha
  apiKeyFile: ` + keyFile + `
`
	path := filepath.Join(dir, "projects.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))

	projects, err := loadProjectsFile(path, "", "", nil, os.Stderr)
	require.NoError(t, err)
	require.Len(t, projects, 1)
	assert.Equal(t, []string{"24h"}, projects[0].Periods,
		"absent periods must default to [\"24h\"] for byte-identical legacy behaviour")
}

// TestLoadProjectsFile_PeriodsInvalid surfaces the bad token through the
// project-id wrapper so the operator sees the project at fault.
func TestLoadProjectsFile_PeriodsInvalid(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key.txt")
	require.NoError(t, os.WriteFile(keyFile, []byte("test-key"), 0o600))

	yaml := `- id: alpha
  apiKeyFile: ` + keyFile + `
  periods: ["24h", "42d"]
`
	path := filepath.Join(dir, "projects.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))

	_, err := loadProjectsFile(path, "", "", nil, os.Stderr)
	require.Error(t, err)
	msg := err.Error()
	assert.True(t, strings.Contains(msg, "42d"), "error should mention bad token, got %q", msg)
	assert.True(t, strings.Contains(msg, "alpha"), "error should mention project id, got %q", msg)
}
