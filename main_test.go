// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	hyperping "github.com/develeap/hyperping-go"
	"github.com/develeap/hyperping-exporter/internal/collector"
)

func TestValidateNamespace(t *testing.T) {
	tests := []struct {
		name    string
		ns      string
		wantErr bool
	}{
		{"valid default", "hyperping", false},
		{"valid underscore prefix", "_my_ns", false},
		{"valid uppercase", "MyNS", false},
		{"valid with digits", "ns123", false},
		{"valid mixed", "hyperping_exporter_2", false},
		{"empty string", "", true},
		{"starts with digit", "1ns", true},
		{"contains hyphen", "my-ns", true},
		{"contains dot", "my.ns", true},
		{"contains space", "my ns", true},
		{"too long (65 chars)", "a2345678901234567890123456789012345678901234567890123456789012345", true},
		{"exactly 64 chars", "a234567890123456789012345678901234567890123456789012345678901234", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateNamespace(tt.ns)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestSetupLogger(t *testing.T) {
	tests := []struct {
		level  string
		format string
	}{
		{"debug", "text"},
		{"info", "text"},
		{"warn", "text"},
		{"error", "text"},
		{"unknown", "text"}, // falls through to default (info)
		{"debug", "json"},
		{"info", "json"},
	}
	for _, tt := range tests {
		t.Run(tt.level+"_"+tt.format, func(t *testing.T) {
			logger := setupLogger(tt.level, tt.format)
			assert.NotNil(t, logger)
		})
	}
}

func TestNewBaseRegistry(t *testing.T) {
	reg := newBaseRegistry("testns")
	require.NotNil(t, reg)

	mfs, err := reg.Gather()
	require.NoError(t, err)

	names := make(map[string]bool)
	for _, mf := range mfs {
		names[mf.GetName()] = true
	}
	assert.True(t, names["testns_build_info"], "expected testns_build_info metric")
}

// dummyCollectorAPI implements collector.HyperpingAPI with no-op responses.
type dummyCollectorAPI struct{}

func (d *dummyCollectorAPI) ListMonitors(_ context.Context) ([]hyperping.Monitor, error) {
	return nil, nil
}
func (d *dummyCollectorAPI) ListHealthchecks(_ context.Context) ([]hyperping.Healthcheck, error) {
	return nil, nil
}
func (d *dummyCollectorAPI) ListOutages(_ context.Context) ([]hyperping.Outage, error) {
	return nil, nil
}
func (d *dummyCollectorAPI) ListMonitorReports(_ context.Context, _, _ string) ([]hyperping.MonitorReport, error) {
	return nil, nil
}
func (d *dummyCollectorAPI) ListMaintenance(_ context.Context) ([]hyperping.Maintenance, error) {
	return nil, nil
}
func (d *dummyCollectorAPI) ListIncidents(_ context.Context) ([]hyperping.Incident, error) {
	return nil, nil
}

func TestNewMux(t *testing.T) {
	c := collector.NewCollector(
		&dummyCollectorAPI{},
		nil,
		60*time.Second,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		"hyperping",
	)
	reg := prometheus.NewRegistry()

	mux, err := newMux("/metrics", reg, c)
	require.NoError(t, err)
	require.NotNil(t, mux)

	t.Run("healthz returns 200", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("readyz returns 503 before first refresh", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	})

	t.Run("readyz returns 200 after refresh", func(t *testing.T) {
		c.Refresh(context.Background())
		req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("landing page returns 200", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
	})
}

// resetFlags replaces flag.CommandLine with a fresh FlagSet and sets os.Args to
// the provided list. Returns a cleanup function that restores both globals.
func resetFlags(t *testing.T, args []string) {
	t.Helper()
	origArgs := os.Args
	origFlags := flag.CommandLine
	t.Cleanup(func() {
		os.Args = origArgs
		flag.CommandLine = origFlags
	})
	flag.CommandLine = flag.NewFlagSet("test", flag.ContinueOnError)
	os.Args = args
}

func TestParseConfig_MissingAPIKey(t *testing.T) {
	resetFlags(t, []string{"test"})
	t.Setenv("HYPERPING_API_KEY", "")
	os.Unsetenv("HYPERPING_API_KEY")

	_, ok := parseConfig()
	assert.False(t, ok)
}

func TestParseConfig_APIKeyFromEnv(t *testing.T) {
	resetFlags(t, []string{"test"})
	t.Setenv("HYPERPING_API_KEY", "testkey123")

	cfg, ok := parseConfig()
	require.True(t, ok)
	assert.Equal(t, "testkey123", cfg.apiKey)
	assert.Equal(t, ":9312", cfg.listenAddr)
	assert.Equal(t, "hyperping", cfg.namespace)
}

func TestParseConfig_NamespaceFromEnv(t *testing.T) {
	resetFlags(t, []string{"test"})
	t.Setenv("HYPERPING_API_KEY", "testkey")
	t.Setenv("HYPERPING_EXPORTER_NAMESPACE", "myns")

	cfg, ok := parseConfig()
	require.True(t, ok)
	assert.Equal(t, "myns", cfg.namespace)
}

func TestParseConfig_InvalidNamespace(t *testing.T) {
	resetFlags(t, []string{"test", "--namespace", "invalid-namespace"})
	t.Setenv("HYPERPING_API_KEY", "testkey")

	_, ok := parseConfig()
	assert.False(t, ok)
}

func TestParseConfig_FlagBeatsEnvVar(t *testing.T) {
	resetFlags(t, []string{"test", "--namespace", "hyperping"})
	t.Setenv("HYPERPING_API_KEY", "testkey")
	t.Setenv("HYPERPING_EXPORTER_NAMESPACE", "acme")

	cfg, ok := parseConfig()
	require.True(t, ok)
	assert.Equal(t, "hyperping", cfg.namespace, "explicit flag must beat env var")
}

func TestParseConfig_ExcludeNamePattern_Valid(t *testing.T) {
	resetFlags(t, []string{"test", "--exclude-name-pattern", `\[DRILL`})
	t.Setenv("HYPERPING_API_KEY", "testkey")

	cfg, ok := parseConfig()
	require.True(t, ok)
	assert.NotNil(t, cfg.excludeNameRx, "valid pattern must compile to a non-nil regexp")
}

func TestParseConfig_ExcludeNamePattern_Alternation(t *testing.T) {
	resetFlags(t, []string{"test", "--exclude-name-pattern", `\[DRILL|\[TEST|\[STAGING`})
	t.Setenv("HYPERPING_API_KEY", "testkey")

	cfg, ok := parseConfig()
	require.True(t, ok)
	assert.NotNil(t, cfg.excludeNameRx)
}

func TestParseConfig_ExcludeNamePattern_InvalidRegex(t *testing.T) {
	resetFlags(t, []string{"test", "--exclude-name-pattern", `[invalid(`})
	t.Setenv("HYPERPING_API_KEY", "testkey")

	_, ok := parseConfig()
	assert.False(t, ok, "invalid regex must cause parseConfig to fail")
}

func TestParseConfig_ExcludeNamePattern_Empty(t *testing.T) {
	resetFlags(t, []string{"test"})
	t.Setenv("HYPERPING_API_KEY", "testkey")

	cfg, ok := parseConfig()
	require.True(t, ok)
	assert.Nil(t, cfg.excludeNameRx, "empty pattern must leave excludeNameRx nil")
}
