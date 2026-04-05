# Backlog

Issues identified during code review and PR #6 (`fix/code-review-issues`) review.

---

## Critical

- [x] **C1** `internal/client/client.go` - SSRF via `WithBaseURL()`: `validateBaseURL` rejects non-HTTPS/non-localhost URLs; nolint comment references the correct function
- [x] **C2** `deploy/docker-compose.yml` - Grafana password: now requires `GRAFANA_ADMIN_PASSWORD` env var (errors on missing)
- [x] **C3** `deploy/docker-compose.yml` - Prometheus `--web.enable-lifecycle` removed
- [x] **C4** `deploy/k8s/secret.yaml.example` - Placeholder is in `.example` file only; deployment uses `secretKeyRef`; `.gitignore` blocks `secret.yaml`

---

## High

- [x] **H1** `internal/collector/collector.go` - `IsReady()` uses `everSucceeded` latch; never reverts to false after first success. Staleness is surfaced by `hyperping_data_age_seconds`
- [x] **H2** `internal/client/outages.go` - `ListOutages` pagination reduced from 100 to 10 pages to prevent linear degradation with account age
- [x] **H3** `.github/workflows/release.yml` - `goreleaser-action` pinned to SHA; goreleaser version constrained to `~> v2`
- [x] **H4** `.github/workflows/ci.yml` - `govulncheck` pinned to `@v1.1.4`
- [x] **H5** `internal/collector/collector.go` - `NewCollector`, `Collect`, `Refresh` all refactored under 50-line limit
- [x] **H6** `internal/client/client.go` - `NewClient` refactored to 42 lines; circuit breaker init extracted to `newCircuitBreaker`
- [x] **H7** `main.go` - `http.Server` now has `ReadHeaderTimeout` (10s), `ReadTimeout` (30s), and `WriteTimeout` (30s)
- [x] **H8** `deploy/prometheus/alerts.yml` - `HyperpingCoreMonitorDown` alert references `hyperping_monitor_escalation_tier{tier="core"}` which the exporter now emits via `monitorTier` descriptor
- [x] **H9** `main.go` - Client `Metrics` interface wired via `collector.NewClientMetrics` and `client.WithMetrics`
- [x] **H10** `internal/collector/collector.go` - Dead field `lastScrapeTime` removed; replaced by `lastSuccessTime` which is read in `takeSnapshot()`

---

## Medium

- [x] **M1** `internal/collector/collector.go` - `Collect()` uses snapshot-then-release pattern; RWMutex held only during `takeSnapshot()`
- [x] **M2** `internal/collector/collector.go` - `url` label sanitised via `sanitizeURL()` which strips query params and fragments
- [x] **M3** `deploy/prometheus/alerts.yml` - `HyperpingDataStale` threshold documented with formula: `threshold = 2 x cache-ttl`
- [x] **M4** `internal/client/client.go` - Circuit breaker nil-guard is a documented option (`WithNoCircuitBreaker`) for test isolation, not a leaked hook
- [x] **M5** `Dockerfile` - Distroless base image pinned to digest (`@sha256:...`)
- [x] **M6** `deploy/docker-compose.yml` - All service ports bound to `127.0.0.1`
- [x] **M7** `deploy/k8s/` - Added `NetworkPolicy` (K8s manifest + Helm template); `automountServiceAccountToken: false` and `seccompProfile` already present in raw manifests, now also in Helm template
- [x] **M8** `.github/dependabot.yml` - `docker` ecosystem present
- [x] **M9** `internal/client/client.go` - Env var renamed from `TF_APPEND_USER_AGENT` to `HYPERPING_APPEND_USER_AGENT`
- [x] **M10** `deploy/helm/` - Helm template fails explicitly when neither `apiKey` nor `existingSecret` is set

---

## Low

- [x] **L1** Circuit breaker settings are operator-tunable via `WithCircuitBreakerSettings` option
- [x] **L2** `StatusPageService.ID` changed from `interface{}` to `*FlexibleString` for type-safe string/number handling
- [x] **L3** `computeHealthScore` formula weights documented in comment: `(upRatio x 60) + (avgSLA x 40) - (activeOutageRatio x 30)`
- [x] **L4** SBOM generation present via `anchore/sbom-action/download-syft` in release pipeline
- [x] **L5** `PodDisruptionBudget` available in Helm chart (disabled by default)
- [x] **L6** `go.mod` references `go 1.26.1` - matches project toolchain; no change needed

---

## Post-PR #6 Follow-up (unresolved items from the fix branch review)

- [x] **F1** Metric renamed to `hyperping_client_api_call_duration_seconds` (Prometheus convention)
- [x] **F2** `goreleaser-action@v7` pinned to commit SHA (`ec59f474b...`)
- [x] **F3** `WithCircuitBreakerSettings` copies settings by value to prevent post-construction mutation
- [x] **F4** `sanitizeURL` fallback strips query params via `strings.IndexAny("?#")` when `url.Parse()` fails
- [x] **F5** `deploy/k8s/secret.yaml.example` recommends `kubectl create secret --from-literal` over `kubectl apply`
- [x] **F6** `IsReady()` latch behavior documented in code comment; `HyperpingDataStale` alert covers permanent API key rotation
