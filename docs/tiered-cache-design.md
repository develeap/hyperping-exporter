# Tiered Cache Refactor — Design Plan

Refactor `hyperping-exporter`'s `Collector.Refresh()` from a single-TTL fetch loop into three independent tiers with their own timers and atomic-pointer snapshots, behind a feature flag.

## Design parameters (decided)

- **Q1 SDK dependency**: Use `go.mod replace github.com/develeap/hyperping-go => ../hyperping-go` against the local `feat/list-status-filter` branch during dev. Call `ListOutages(ctx, hyperping.WithStatus("ongoing"))` in the HOT tier. Swap to a tagged version once L1 PR merges.
- **Q2 partial_refresh_total label**: Add a `tier="hot|warm|cold"` label. **Breaking change for any existing PromQL.** Document loudly in CHANGELOG. Consumer-side query updates (e.g. `/home/khaledsa/projects/hyp/hyperping-automation/grafana/` and `recording_rules.yaml`) are a follow-up — NOT in this PR. Add the TODO to the exporter repo's BACKLOG.md.
- **Q3 health-score 30d dependency**: Accept the cold-start gap. For ~15 min after pod restart, `hyperping_tenant_health_score` will be absent in tiered mode. Document in CHANGELOG under "Behavior changes".
- **Q4 MCP rate-limit headroom**: No staggering. The existing 10-worker pool smooths bursts. 5min WARM + 135 monitors = ~82 MCP calls/min, well under Hyperping's 300/min/key cap. Add a `hyperping_warm_refresh_duration_seconds` histogram so we can revisit if bursts cause problems.
- **Q5 data_age_seconds semantics**: Add a `tier` label to `hyperping_data_age_seconds`. `tier="hot"` matches the old semantics most closely. Document in CHANGELOG.
- **Q6 TTL floor enforcement**: Yes. `validateTierTTLs` in `_helpers.tpl` must hard-fail render when `hot < 30s`, `warm < 60s`, or `cold < 300s`. Same error shape as `validateCacheTTL`.
- **Q7 backlog tracking**: First commit of this work creates `docs/tiered-cache-design.md` (this file's content) and adds a BACKLOG.md entry linking to it.

## Tier assignments

| Tier | TTL  | Data fetched |
|------|------|--------------|
| HOT  | 60s  | ListMonitors, ListHealthchecks, ListIncidents, ListOutages(`status=ongoing`), ListMaintenance filtered to active (Status=="ongoing") |
| WARM | 5min | MCP ListRecentAlerts (global), MCP per-monitor (response time, MTTA, anomalies), ListMonitorReports for 24h, full ListMaintenance |
| COLD | 15min| ListMonitorReports for 7d, ListMonitorReports for 30d |

The HOT tier MUST call `ListOutages(ctx, hyperping.WithStatus("ongoing"))`. Until L1 ships an official tag, use a `go.mod replace` directive.

## File map and LOC budget

| File | Status | LOC |
|---|---|---|
| `internal/collector/tiered.go` | NEW | ~280 |
| `internal/collector/tiered_snapshot.go` | NEW | ~120 |
| `internal/collector/tiered_test.go` | NEW | ~600 |
| `internal/collector/collector.go` | MODIFIED | +120 / -10 |
| `internal/collector/collector_test.go` | MODIFIED | +180 |
| `main.go` | MODIFIED | +30 |
| `main_test.go` | MODIFIED | +60 |
| `deploy/helm/hyperping-exporter/values.yaml` | MODIFIED | +25 |
| `deploy/helm/hyperping-exporter/templates/deployment.yaml` | MODIFIED | +10 |
| `deploy/helm/hyperping-exporter/templates/_helpers.tpl` | MODIFIED | +30 |
| `internal/collector/deployconfig_test.go` | MODIFIED | +40 |
| `BACKLOG.md`, `CHANGELOG.md`, `docs/tiered-cache-design.md` | NEW/MODIFIED | ~120 |
| `go.mod` | MODIFIED | +3 (replace directive, removed before merge) |

Approximate total: ~1,000 new LOC of production code, ~700 LOC of tests.

## Public/private interface sketch

```go
type CacheMode int
const (
    CacheModeLegacy CacheMode = iota
    CacheModeTiered
)

func WithCacheMode(m CacheMode) CollectorOption
func WithTierTTLs(hot, warm, cold time.Duration) CollectorOption

type Collector struct {
    // ... existing fields ...
    cacheMode CacheMode
    hotTTL, warmTTL, coldTTL time.Duration
    tiered    *tieredRefresher  // nil when CacheModeLegacy
}

type tieredRefresher struct {
    api    HyperpingAPI
    mcp    *hyperping.MCPClient
    logger *slog.Logger
    excludePattern *regexp.Regexp
    mcpMetrics *MCPMetrics

    hotTTL, warmTTL, coldTTL time.Duration

    hot   atomic.Pointer[hotSnapshot]
    warm  atomic.Pointer[warmSnapshot]
    cold  atomic.Pointer[coldSnapshot]

    hotReady atomic.Bool

    hotLastSuccess, warmLastSuccess, coldLastSuccess atomic.Int64

    // Per-tier refresh mutexes serialize overlapping refreshes
    // (defensive — tickers shouldn't overlap, but defend the snapshot
    // build/store sequence).
    hotMu, warmMu, coldMu sync.Mutex
}

type hotSnapshot struct {
    monitors          []hyperping.Monitor
    healthchecks      []hyperping.Healthcheck
    incidents         []hyperping.Incident
    activeOutages     []hyperping.Outage
    activeMaintenance []hyperping.Maintenance
    excludedCount     int
    refreshedAt       time.Time
    scrapeDur         time.Duration
}

type warmSnapshot struct {
    responseTime  map[string]float64
    mtta          map[string]float64
    anomalyCount  map[string]int
    anomalyScore  map[string]float64
    totalAlerts   int
    report24h     []hyperping.MonitorReport
    maintenance   []hyperping.Maintenance
    refreshedAt   time.Time
}

type coldSnapshot struct {
    report7d    []hyperping.MonitorReport
    report30d   []hyperping.MonitorReport
    refreshedAt time.Time
}

func newTieredRefresher(c *Collector) *tieredRefresher
func (t *tieredRefresher) start(ctx context.Context)
func (t *tieredRefresher) refreshHot(ctx context.Context)
func (t *tieredRefresher) refreshWarm(ctx context.Context)
func (t *tieredRefresher) refreshCold(ctx context.Context)
func (t *tieredRefresher) buildCollectorSnapshot() collectorSnapshot
```

## Concurrency model

- One goroutine per tier with its own `time.Ticker`. Pattern matches existing `Start()` loop (`collector.go:283-293`).
- Each tier uses `atomic.Pointer[snapshot]` — lock-free reads, atomic swap on successful refresh.
- Each tier holds a per-tier `sync.Mutex` only during the refresh function body to serialize any accidental overlap.
- Eager initial HOT fetch in `start()` blocks until first HOT succeeds or context errors. WARM and COLD start their tickers and fire on first tick (do not block startup).
- Readers (Collect, IsReady, etc.) do 3 `Load()` calls and stitch. Acceptable torn read across tiers — each tier's data is independent.
- Per-tier failure isolation: a tier whose refresh function aborts before `Store(...)` leaves the previous pointer in place. Stale data continues to serve.
- `partial_refresh_total{tier=hot|warm|cold}` counter increments on per-call MCP failures (existing semantics preserved per tier).

## Lifecycle, startup, restart

- Initial pointers are nil. `buildCollectorSnapshot()` handles nil per-tier by returning empty maps / zero values for that tier's fields.
- `IsReady()` returns true only after the first HOT refresh succeeds.
- Cold-start gap (Q3): WARM populated within ~5 min after boot; COLD within ~15 min. Health-score absent during cold-start.
- On context cancellation, `start()` returns after all three tier goroutines complete their current refresh and exit their select loops.

## Test plan

### Unit tests in `internal/collector/tiered_test.go` (target: 15)

1. `TestTieredRefresher_HotOnlyTouchesHotEndpoints` — after `refreshHot`, only HOT-tier endpoints are called.
2. `TestTieredRefresher_WarmOnlyTouchesWarmEndpoints` — after `refreshWarm`, only WARM-tier endpoints are called.
3. `TestTieredRefresher_ColdOnlyTouchesColdEndpoints` — after `refreshCold`, only COLD-tier endpoints are called.
4. `TestTieredRefresher_HotFailureLeavesWarmAndColdSnapshotsIntact` — pointer-identity check after HOT failure.
5. `TestTieredRefresher_WarmPartialMcpFailureRetainsStaleValues` — stale per-uuid carry-forward; `partial_refresh_total{tier="warm"}` incremented.
6. `TestTieredRefresher_BuildCollectorSnapshot_AllNilTiers` — zero-value snapshot, no panic.
7. `TestTieredRefresher_BuildCollectorSnapshot_OnlyHotPopulated` — partial snapshot wiring.
8. `TestTieredRefresher_BuildCollectorSnapshot_AllPopulated` — full snapshot wiring.
9. `TestTieredRefresher_TickerFiresAtConfiguredCadence` — short TTLs, count refreshes over a fixed window.
10. `TestTieredRefresher_ContextCancellationStopsAllTiers` — clean shutdown.
11. `TestTieredRefresher_ActiveOutagesPassesStatusOngoing` — verify the HOT-tier call passes `WithStatus("ongoing")` to ListOutages.
12. `TestTieredRefresher_ActiveMaintenanceFilteredInHot` — HOT contains only Status=="ongoing" maintenance; WARM contains the full list.
13. `TestTieredRefresher_ExclusionFilterAppliedToHot` — excluded monitors absent from HOT snapshot.
14. `TestTieredRefresher_ExclusionFilterAppliedToWarmAndCold` — excluded uuids absent from WARM/COLD maps and slices.
15. `TestTieredRefresher_StaleFallbackLogsTierLabel` — tier label on failure log lines.

### Integration tests in `internal/collector/collector_test.go` (target: 5)

1. `TestCollector_TieredMode_PromMetricsMatchLegacy` — identical mock state, `/metrics` output equal under both modes (modulo data_age tier label).
2. `TestCollector_TieredMode_IsReadyAfterHotSucceeds` — readiness gating on HOT only.
3. `TestCollector_TieredMode_DataAgeReflectsHotTier` — `hyperping_data_age_seconds{tier="hot"}` tracks HOT last-success.
4. `TestCollector_TieredMode_ColdReportsFailureDoesNotZeroSLA` — stale COLD data retained on error.
5. `TestCollector_TieredMode_ConcurrentScrapeAndRefresh` — race-tagged.

### Feature flag tests in `main_test.go` (target: 4)

1. `TestParseConfig_CacheMode_DefaultLegacy` — default behavior.
2. `TestParseConfig_CacheMode_Tiered` — explicit selection.
3. `TestParseConfig_CacheMode_Invalid` — invalid value rejected.
4. `TestParseConfig_TierTTLs_Override` — per-tier overrides applied.

### Chart tests in `internal/collector/deployconfig_test.go` (target: 3)

1. `TestDeployment_CacheMode_Tiered_RendersTierFlags`
2. `TestDeployment_CacheMode_Legacy_OmitsTierFlags`
3. `TestDeployment_TierTTLs_BelowFloor_FailsRender`

### Pre-existing tests promoted to dual-mode subtests

Wrap the following with `t.Run("legacy", ...)` and `t.Run("tiered", ...)`:
- `TestRefresh_Success`
- `TestRefresh_OutageErrorIsNonFatal`
- `TestRefresh_MaintenanceErrorIsNonFatal`
- `TestRefresh_IncidentErrorIsNonFatal`
- `TestRefresh_ReportErrorPreservesStaleData`

Mode-specific tests stay legacy-only (e.g. `TestStart_PeriodicRefreshOnTick` which uses cacheTTL specifically). Add a sibling `TestStart_TieredPeriodicRefreshOnTick`.

## Feature flag

- CLI: `--cache-mode=legacy|tiered` (default `legacy`)
- CLI: `--hot-ttl=60s`, `--warm-ttl=5m`, `--cold-ttl=15m` (only honored when tiered)
- Helm: `config.cacheMode`, `config.hotTTL`, `config.warmTTL`, `config.coldTTL`
- `validateCacheMode` accepts `legacy|tiered` (case-insensitive).
- `validateTierTTLs` enforces floors: `hot >= 30s`, `warm >= 60s`, `cold >= 300s`. Hard-fail render with same error shape as `validateCacheTTL`.
- Deployment template conditionally renders the tier flags only when `cacheMode != legacy`.

## Migration plan

- Phase 1 (this PR): ship behind flag, default `legacy`. No behavior change for existing deployments.
- Phase 2 (separate change): enable `cacheMode: tiered` on DEV via helm value. Observe for 24h.
- Phase 3 (next chart minor, e.g. 1.6.0): flip default to `tiered` in `values.yaml`.
- Phase 4 (later): remove legacy `Refresh()`. Out of scope.

## Risk register

- Race conditions between snapshot pointer-store and read-stitcher → `go test -race` in CI; atomic.Pointer pattern.
- Time-based test flakiness → generous tolerances; the existing test suite already uses this style.
- Operators on chart 1.5.x staying on legacy-binary → no incompatibility, just no opt-in.
- SDK dependency not yet tagged → use `replace` directive locally; CI must accept replace during this PR's lifetime, removed before merge.
- `partial_refresh_total` label addition breaks downstream alerts → CHANGELOG callout; consumer-side update is a follow-up TODO.

## Definition of Done (verbatim checklist for Agent D)

- [ ] Branch `feat/tiered-cache` created off the current default branch
- [ ] `docs/tiered-cache-design.md` written (this file's content) in first commit
- [ ] `BACKLOG.md` entry added with link to design doc and the partial_refresh_total consumer-side migration TODO
- [ ] `go.mod` replace directive added pointing at `../hyperping-go` on `feat/list-status-filter` branch
- [ ] All new tests written BEFORE implementation (each test file has at least one observed RED failure captured in the agent report)
- [ ] All existing tests pass (capture before/after count)
- [ ] All new tests pass
- [ ] `go test -race ./...` passes (no data races)
- [ ] `go vet ./...` clean
- [ ] `go build ./...` clean
- [ ] Helm chart renders cleanly with `cacheMode=legacy` (no tier flags emitted)
- [ ] Helm chart renders cleanly with `cacheMode=tiered` (tier flags present, values correct)
- [ ] Helm chart fails render with `hotTTL=10s` (below floor)
- [ ] CHANGELOG entries:
  - `### Added` — tiered cache mode behind `--cache-mode=tiered` flag
  - `### Changed` — `partial_refresh_total` gains `tier` label (breaking for downstream)
  - `### Changed` — `hyperping_data_age_seconds` gains `tier` label
  - `### Note` — tenant health score absent for ~15 min after restart in tiered mode
- [ ] Diff of every committed file fits the LOC budget within +/- 20%
- [ ] Branch is local + pushed to `origin/feat/tiered-cache`
- [ ] No PR opened
- [ ] No deploy attempted
