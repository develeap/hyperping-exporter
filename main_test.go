// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
func (d *dummyCollectorAPI) ListOutages(_ context.Context, _ ...hyperping.OutageListOption) ([]hyperping.Outage, error) {
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

	mux, err := newMux("/metrics", reg, []*collector.Collector{c})
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

// TestNewMux_ReadyzOrSemantics verifies that /readyz returns 200 as soon
// as any one Collector has produced a successful refresh, even when other
// collectors remain perpetually unready. Multi-tenant deployments rely on
// this: a single misconfigured project (revoked key, persistent 429)
// must not strip the Pod from Service endpoints and drag healthy peers
// offline. The hyperping_project_ready gauge surfaces per-project status.
func TestNewMux_ReadyzOrSemantics(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	healthy := collector.NewCollector(
		&dummyCollectorAPI{}, nil, 60*time.Second, logger, "hyperping",
		collector.WithProject("hp_core"),
	)
	broken := collector.NewCollector(
		&dummyCollectorAPI{}, nil, 60*time.Second, logger, "hyperping",
		collector.WithProject("hp_infra"),
	)
	reg := prometheus.NewRegistry()
	mux, err := newMux("/metrics", reg, []*collector.Collector{healthy, broken})
	require.NoError(t, err)

	// Both unready: 503.
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code,
		"with no collector ready /readyz must return 503")

	// One ready, one perpetually unready: 200 (OR policy).
	healthy.Refresh(context.Background())
	req = httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code,
		"one healthy collector must keep /readyz at 200 even if peers stay unready")
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

// --- cache-mode and tier-TTL flag-parsing tests (chunk 7) ---

func TestParseConfig_CacheMode_DefaultLegacy(t *testing.T) {
	resetFlags(t, []string{"test"})
	t.Setenv("HYPERPING_API_KEY", "testkey")

	cfg, ok := parseConfig()
	require.True(t, ok)
	assert.Equal(t, "legacy", cfg.cacheMode, "default cache mode must be \"legacy\" so existing deployments are unchanged")
}

func TestParseConfig_CacheMode_Tiered(t *testing.T) {
	resetFlags(t, []string{"test", "--cache-mode", "tiered"})
	t.Setenv("HYPERPING_API_KEY", "testkey")

	cfg, ok := parseConfig()
	require.True(t, ok)
	assert.Equal(t, "tiered", cfg.cacheMode)
}

func TestParseConfig_CacheMode_Invalid(t *testing.T) {
	resetFlags(t, []string{"test", "--cache-mode", "fast"})
	t.Setenv("HYPERPING_API_KEY", "testkey")

	_, ok := parseConfig()
	assert.False(t, ok, "invalid cache mode must be rejected at parse time")
}

func TestParseConfig_TierTTLs_Override(t *testing.T) {
	resetFlags(t, []string{
		"test",
		"--cache-mode", "tiered",
		"--hot-ttl", "45s",
		"--warm-ttl", "3m",
		"--cold-ttl", "20m",
	})
	t.Setenv("HYPERPING_API_KEY", "testkey")

	cfg, ok := parseConfig()
	require.True(t, ok)
	assert.Equal(t, 45*time.Second, cfg.hotTTL)
	assert.Equal(t, 3*time.Minute, cfg.warmTTL)
	assert.Equal(t, 20*time.Minute, cfg.coldTTL)
}

// --- API key handling: deprecation, file source, args sanitization (HIGH-2) ---

// TestParseConfig_APIKeyFlag_DeprecationWarning asserts that passing --api-key
// emits a stderr deprecation warning. Operators with `ps`/`/proc` visibility
// can read the key from the cmdline; the warning steers them to the env var
// or --api-key-file.
func TestParseConfig_APIKeyFlag_DeprecationWarning(t *testing.T) {
	resetFlags(t, []string{"test", "--api-key", "supersecret"})
	t.Setenv("HYPERPING_API_KEY", "")
	os.Unsetenv("HYPERPING_API_KEY")

	var buf bytes.Buffer
	cfg, ok := parseConfigOut(&buf)
	require.True(t, ok)
	assert.Equal(t, "supersecret", cfg.apiKey)
	assert.Contains(t, strings.ToLower(buf.String()), "deprecat",
		"expected deprecation notice for --api-key on stderr")
}

// TestParseConfig_APIKeyFlag_OverwritesOsArgsSliceBestEffort asserts that
// after parsing, the in-process os.Args slice no longer carries the raw API
// key. This is best-effort defense in depth, NOT a guarantee that
// /proc/<pid>/cmdline is updated: Go's os.Args is a slice over a copy of
// argv, so mutating it does not propagate to the kernel's record. Process
// accounting, audit logs, or kernel rings that snapshotted argv before
// this point still carry the original secret; the deprecation warning
// explicitly documents that limitation.
func TestParseConfig_APIKeyFlag_OverwritesOsArgsSliceBestEffort(t *testing.T) {
	resetFlags(t, []string{"test", "--api-key", "supersecret"})
	t.Setenv("HYPERPING_API_KEY", "")
	os.Unsetenv("HYPERPING_API_KEY")

	var buf bytes.Buffer
	_, ok := parseConfigOut(&buf)
	require.True(t, ok)
	// Note: this verifies only the in-process os.Args slice. /proc/<pid>/cmdline
	// is sourced from the kernel's copy of argv[] which Go does not touch.
	for i, a := range os.Args {
		assert.NotContains(t, a, "supersecret",
			"os.Args[%d]=%q must not contain the raw API key after parse", i, a)
	}
}

// TestSanitizeArgs verifies the os.Args scrubber in isolation: every byte of
// the secret is overwritten with 'x' wherever it appears, including when the
// secret is glued to the flag with '='. Non-matching args are untouched.
func TestSanitizeArgs(t *testing.T) {
	t.Run("separate flag and value", func(t *testing.T) {
		args := []string{"prog", "--api-key", "abc123", "--other", "keep"}
		got := sanitizeArgs(args, "abc123")
		assert.Equal(t, []string{"prog", "--api-key", "xxxxxx", "--other", "keep"}, got)
	})
	t.Run("flag=value form", func(t *testing.T) {
		args := []string{"prog", "--api-key=abc123"}
		got := sanitizeArgs(args, "abc123")
		// '=' is preserved; only the secret bytes are overwritten.
		assert.Equal(t, "--api-key=xxxxxx", got[1])
	})
	t.Run("empty secret is a no-op", func(t *testing.T) {
		args := []string{"prog", "--flag", "value"}
		got := sanitizeArgs(args, "")
		assert.Equal(t, args, got)
	})
	// Tightened contract: scrub only the values attached to --api-key. An
	// unrelated arg whose value happens to contain the secret bytes as a
	// substring must NOT be mangled. Realistic risk is low, but the looser
	// contract violated least-surprise for any operator whose listen address,
	// log file path, or similar contained the same bytes.
	t.Run("does not mangle unrelated arg that contains secret as substring", func(t *testing.T) {
		// secret bytes ":9312" happen to appear inside the --listen-address value.
		// The scrubber must leave --listen-address alone and only touch --api-key.
		args := []string{
			"prog",
			"--listen-address=:9312",
			"--api-key=:9312",
			"--debug",
		}
		got := sanitizeArgs(args, ":9312")
		assert.Equal(t, "--listen-address=:9312", got[1],
			"unrelated --listen-address must be untouched")
		assert.Equal(t, "--api-key=xxxxx", got[2],
			"--api-key=value must be scrubbed (suffix only, '=' preserved)")
		assert.Equal(t, "--debug", got[3], "unrelated --debug must be untouched")
	})
	t.Run("separate --api-key followed by value scrubs only the next arg", func(t *testing.T) {
		args := []string{
			"prog",
			"--listen-address", "secretvalue", // secret as substring elsewhere; must be left alone
			"--api-key", "secretvalue",
			"--other", "secretvalue",
		}
		got := sanitizeArgs(args, "secretvalue")
		// Only the value immediately after --api-key is replaced.
		assert.Equal(t, "secretvalue", got[2], "--listen-address value must be untouched")
		assert.Equal(t, "xxxxxxxxxxx", got[4], "value after --api-key must be scrubbed")
		assert.Equal(t, "secretvalue", got[6], "--other value must be untouched")
	})
}

// TestParseConfig_APIKeyFile reads the key from the file given to
// --api-key-file, stripping a single trailing newline. Trailing whitespace
// inside the key is preserved on purpose: the file format is "exact bytes
// minus one trailing LF", matching the common `echo "$key" > /var/run/...`
// pattern without surprising operators who legitimately use whitespace.
func TestParseConfig_APIKeyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	require.NoError(t, os.WriteFile(path, []byte("filekey-abc\n"), 0o600))

	resetFlags(t, []string{"test", "--api-key-file", path})
	t.Setenv("HYPERPING_API_KEY", "")
	os.Unsetenv("HYPERPING_API_KEY")

	var buf bytes.Buffer
	cfg, ok := parseConfigOut(&buf)
	require.True(t, ok)
	assert.Equal(t, "filekey-abc", cfg.apiKey)
}

// TestParseConfig_APIKeyFile_TrimsTrailingCRLF covers the line-ending
// normalisation applied to --api-key-file contents. Operators may produce
// the file with `echo`, here-docs, Windows tooling, or scripts that append
// multiple newlines; the exporter must accept all of these and yield the
// same clean key string. Any leading/internal whitespace is preserved on
// purpose (the file contract is "bytes minus trailing CR/LF").
func TestParseConfig_APIKeyFile_TrimsTrailingCRLF(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"single LF", "key\n", "key"},
		{"CRLF", "key\r\n", "key"},
		{"double LF", "key\n\n", "key"},
		{"no trailing newline", "key", "key"},
		{"empty file", "", ""},
		{"lone trailing CR", "key\r", "key"},
		{"mixed CR LF trailing", "key\r\n\r\n", "key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "key")
			require.NoError(t, os.WriteFile(path, []byte(tc.content), 0o600))

			resetFlags(t, []string{"test", "--api-key-file", path})
			t.Setenv("HYPERPING_API_KEY", "")
			os.Unsetenv("HYPERPING_API_KEY")

			var buf bytes.Buffer
			cfg, ok := parseConfigOut(&buf)
			if tc.want == "" {
				// Empty key cannot pass the "API key required" gate, so
				// parseConfig returns ok=false. The trim helper still has
				// to yield "" to reach that gate cleanly.
				assert.False(t, ok, "empty key file must fail the required-key check")
				return
			}
			require.True(t, ok)
			assert.Equal(t, tc.want, cfg.apiKey)
		})
	}
}

// TestParseConfig_APIKeyFile_Missing rejects a path that cannot be read so
// boot fails fast rather than degrading to "no API key configured".
func TestParseConfig_APIKeyFile_Missing(t *testing.T) {
	resetFlags(t, []string{"test", "--api-key-file", "/nonexistent/path/key"})
	t.Setenv("HYPERPING_API_KEY", "")
	os.Unsetenv("HYPERPING_API_KEY")

	var buf bytes.Buffer
	_, ok := parseConfigOut(&buf)
	assert.False(t, ok, "missing api-key-file must cause parseConfig to fail")
}

// --- HTTP server hardening (MEDIUM-3) ---

// --- Unauthenticated bind warning (MEDIUM-6) ---

// TestUnauthenticatedBindWarning_Fires asserts that binding any-interface
// (":port" or "0.0.0.0:port") without --web.config.file logs a stderr
// warning. The default is not changed; this is a hint, not a hard fail.
func TestUnauthenticatedBindWarning_Fires(t *testing.T) {
	tests := []struct {
		name       string
		listenAddr string
	}{
		{"colon-port form", ":9312"},
		{"0.0.0.0 form", "0.0.0.0:9312"},
		{"[::] form", "[::]:9312"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
			maybeWarnUnauthenticatedBind(logger, tt.listenAddr, "")
			assert.Contains(t, buf.String(), "unauthenticated",
				"warning must mention unauthenticated state")
		})
	}
}

// TestUnauthenticatedBindWarning_Silent_WhenWebConfigSet covers the
// "operator set --web.config.file" path: the warning must not fire because
// exporter-toolkit will require auth/TLS.
func TestUnauthenticatedBindWarning_Silent_WhenWebConfigSet(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	maybeWarnUnauthenticatedBind(logger, ":9312", "/etc/exporter/web.yaml")
	assert.NotContains(t, buf.String(), "unauthenticated")
}

// TestUnauthenticatedBindWarning_Silent_WhenLoopback covers the "operator
// chose a loopback / specific IP bind" path. The warning is only useful for
// "any-interface + no auth"; an explicit 127.0.0.1 or a private IP is a
// deliberate choice and does not need scolding.
func TestUnauthenticatedBindWarning_Silent_WhenLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:9312", "[::1]:9312", "10.0.0.5:9312"} {
		t.Run(addr, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
			maybeWarnUnauthenticatedBind(logger, addr, "")
			assert.NotContains(t, buf.String(), "unauthenticated",
				"non-any-interface bind should not warn")
		})
	}
}

// TestNewHTTPServer_Hardening pins the server timeouts and header-bytes cap
// against regression. ReadHeaderTimeout and ReadTimeout were already set;
// IdleTimeout and MaxHeaderBytes are the new guards against keep-alive
// idle-connection DoS and header-bomb DoS respectively.
func TestNewHTTPServer_Hardening(t *testing.T) {
	srv := newHTTPServer(":9312", http.NewServeMux())
	require.NotNil(t, srv)

	assert.NotZero(t, srv.ReadHeaderTimeout, "ReadHeaderTimeout must be set")
	assert.NotZero(t, srv.ReadTimeout, "ReadTimeout must be set")
	assert.NotZero(t, srv.WriteTimeout, "WriteTimeout must be set")
	assert.NotZero(t, srv.IdleTimeout, "IdleTimeout must be set (keep-alive DoS guard)")
	assert.NotZero(t, srv.MaxHeaderBytes, "MaxHeaderBytes must be set (header-bomb DoS guard)")

	// Pin the documented values. A future refactor that silently shrinks
	// MaxHeaderBytes to 64 KiB or drops IdleTimeout to single-digit seconds
	// must trip a test, not slip through as "still non-zero".
	assert.Equal(t, 10*time.Second, srv.ReadHeaderTimeout)
	assert.Equal(t, 30*time.Second, srv.ReadTimeout)
	assert.Equal(t, 30*time.Second, srv.WriteTimeout)
	assert.Equal(t, 120*time.Second, srv.IdleTimeout)
	assert.Equal(t, 1<<20, srv.MaxHeaderBytes)
}
