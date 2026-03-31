// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package client

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/dnaeon/go-vcr.v3/cassette"
	"gopkg.in/dnaeon/go-vcr.v3/recorder"
)

// vcrMode determines how VCR handles HTTP requests.
type vcrMode int

const (
	// modeReplay replays from cassette, fails if no recording exists.
	modeReplay vcrMode = iota
	// modeRecord always records new interactions.
	modeRecord
	// modeAuto replays if cassette exists, otherwise skips the test.
	modeAuto
)

// vcrConfig configures VCR recording behavior.
type vcrConfig struct {
	CassetteName string
	Mode         vcrMode
	CassetteDir  string
}

// newVCRRecorder creates a new VCR recorder for contract testing.
func newVCRRecorder(t *testing.T, cfg vcrConfig) (*recorder.Recorder, *http.Client) {
	t.Helper()

	if cfg.CassetteDir == "" {
		cfg.CassetteDir = filepath.Join("testdata", "cassettes")
	}

	cassettePath := filepath.Join(cfg.CassetteDir, cfg.CassetteName)

	if err := os.MkdirAll(cfg.CassetteDir, 0o750); err != nil {
		t.Fatalf("failed to create cassette directory: %v", err)
	}

	var mode recorder.Mode
	switch cfg.Mode {
	case modeReplay:
		mode = recorder.ModeReplayOnly
	case modeRecord:
		mode = recorder.ModeRecordOnly
	case modeAuto:
		if _, err := os.Stat(cassettePath + ".yaml"); os.IsNotExist(err) {
			t.Skipf("Skipping: no cassette exists at %s.yaml (set RECORD_MODE=true to record)", cassettePath)
		}
		mode = recorder.ModeReplayOnly
	}

	r, err := recorder.NewWithOptions(&recorder.Options{
		CassetteName:       cassettePath,
		Mode:               mode,
		SkipRequestLatency: true,
	})
	if err != nil {
		t.Fatalf("failed to create VCR recorder: %v", err)
	}

	r.AddHook(func(i *cassette.Interaction) error {
		if auth := i.Request.Headers.Get("Authorization"); auth != "" {
			i.Request.Headers.Set("Authorization", "Bearer [MASKED]")
		}
		if strings.Contains(i.Request.URL, "api_key=") {
			i.Request.URL = strings.ReplaceAll(i.Request.URL, "api_key=", "api_key=[MASKED]")
		}
		if cookie := i.Response.Headers.Get("Set-Cookie"); cookie != "" {
			i.Response.Headers.Set("Set-Cookie", "[MASKED]")
		}
		return nil
	}, recorder.AfterCaptureHook)

	client := &http.Client{
		Transport: r,
	}

	return r, client
}

// getRecordMode returns the VCR mode based on environment variables.
func getRecordMode() vcrMode {
	if os.Getenv("RECORD_MODE") == "true" {
		return modeRecord
	}
	return modeAuto
}

// requireEnvForRecording skips the test if recording mode is enabled
// but the required environment variable is not set.
func requireEnvForRecording(t *testing.T, envVar string) {
	t.Helper()
	if getRecordMode() == modeRecord && os.Getenv(envVar) == "" {
		t.Skipf("Skipping recording test: %s not set", envVar)
	}
}
