// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package collector

import (
	"log/slog"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	hyperping "github.com/develeap/hyperping-go"
)

// tieredRefresher owns the three independent HOT/WARM/COLD refresh loops
// and the atomic snapshot pointers they publish.
//
// One goroutine per tier, each driven by its own time.Ticker; per-tier
// failure isolation is achieved by abort-before-Store (a tier that errors
// before publishing leaves the previous snapshot pointer untouched).
// Readers do three atomic Load()s and stitch via buildCollectorSnapshot;
// cross-tier torn reads are benign because each tier's data is independent.
type tieredRefresher struct {
	api            HyperpingAPI
	mcp            *hyperping.MCPClient
	logger         *slog.Logger
	excludePattern *regexp.Regexp
	mcpMetrics     *MCPMetrics

	hotTTL  time.Duration
	warmTTL time.Duration
	coldTTL time.Duration

	hot  atomic.Pointer[hotSnapshot]
	warm atomic.Pointer[warmSnapshot]
	cold atomic.Pointer[coldSnapshot]

	// hotReady latches true after the first successful HOT refresh.
	// IsReady() reads this to gate /readyz on HOT freshness only — WARM
	// and COLD lag is expected during the post-boot cold-start window.
	hotReady atomic.Bool

	// Last-success unix-seconds per tier. Reserved for a future
	// hyperping_data_age_seconds{tier=...} expansion (Q5 in the design
	// doc). Not currently emitted; populated as a no-overhead first step.
	hotLastSuccess  atomic.Int64
	warmLastSuccess atomic.Int64
	coldLastSuccess atomic.Int64

	// Per-tier refresh mutexes serialize overlapping refreshes. Tickers
	// should not overlap under healthy conditions, but a slow refresh can
	// race the next tick; the mutex defends the snapshot build/store
	// sequence against that case.
	hotMu  sync.Mutex
	warmMu sync.Mutex
	coldMu sync.Mutex
}
