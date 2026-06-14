// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/develeap/hyperping-exporter/internal/collector"
)

func newTestCollector(t *testing.T, projectID string) *collector.Collector {
	t.Helper()
	return collector.NewCollector(
		&dummyCollectorAPI{},
		nil,
		60*time.Second,
		newDiscardLogger(),
		"hyperping",
		collector.WithProject(projectID),
	)
}

func buildTestIndex(cs ...*collector.Collector) map[string]*collector.Collector {
	m := make(map[string]*collector.Collector, len(cs))
	for _, c := range cs {
		m[c.Project()] = c
	}
	return m
}

func TestProbeHandler_ValidModule(t *testing.T) {
	c := newTestCollector(t, "hyp_core")
	h := probeHandler(buildTestIndex(c))

	req := httptest.NewRequest(http.MethodGet, "/probe?module=hyp_core", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "hyperping_", "response must include hyperping_* metrics")
	assert.Contains(t, body, `project="hyp_core"`, "response must carry project label")
}

func TestProbeHandler_MissingModuleParam(t *testing.T) {
	h := probeHandler(map[string]*collector.Collector{})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "missing required parameter: module")
}

func TestProbeHandler_UnknownModule(t *testing.T) {
	h := probeHandler(map[string]*collector.Collector{})

	req := httptest.NewRequest(http.MethodGet, "/probe?module=nonexistent", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "unknown module")
}

func TestProbeHandler_MetricsIsolation(t *testing.T) {
	core := newTestCollector(t, "hyp_core")
	infra := newTestCollector(t, "hyp_infra")
	h := probeHandler(buildTestIndex(core, infra))

	req := httptest.NewRequest(http.MethodGet, "/probe?module=hyp_core", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `project="hyp_core"`, "response must include hyp_core series")
	assert.NotContains(t, body, `project="hyp_infra"`,
		"response for hyp_core must not include hyp_infra series")
}

func TestProbeHandler_NoProcessMetrics(t *testing.T) {
	c := newTestCollector(t, "hyp_core")
	h := probeHandler(buildTestIndex(c))

	req := httptest.NewRequest(http.MethodGet, "/probe?module=hyp_core", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.NotContains(t, body, "process_cpu_seconds_total",
		"probe response must not include process_* series")
	assert.NotContains(t, body, "go_goroutines",
		"probe response must not include go_* series")
}

func TestProbeHandler_LandingPageIncludesProbe(t *testing.T) {
	c := newTestCollector(t, "hyp_core")
	reg := prometheus.NewRegistry()
	index := buildTestIndex(c)

	mux, err := newMux("/metrics", reg, []*collector.Collector{c}, index)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	// The landing page HTML must contain a link to /probe so operators can
	// discover the multi-target endpoint without reading documentation.
	assert.True(t, strings.Contains(body, "/probe"),
		"landing page must include a link to /probe; got:\n%s", body)
}
