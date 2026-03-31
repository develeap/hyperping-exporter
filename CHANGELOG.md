# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

### Added
- Binary releases now include Windows (386, amd64), Linux 386, and Linux arm (v6, v7) targets
- Cross-compile CI job validates all 5 new target platforms on every PR
- `--namespace` flag (env: `HYPERPING_EXPORTER_NAMESPACE`, default: `hyperping`) to customise the Prometheus metric prefix
- Client observability metrics: `hyperping_client_api_call_duration_seconds`, `hyperping_client_retry_total`, `hyperping_client_circuit_breaker_state` expose API call latency, retry counts, and circuit breaker state
- `WithCircuitBreakerSettings(gobreaker.Settings)` and `WithNoCircuitBreaker()` client options
- `deploy/k8s/secret.yaml.example` — Secret manifest template for Kubernetes deployments
- PodDisruptionBudget template in Helm chart (`podDisruptionBudget.enabled`)
- SBOM generation (`${artifact}.sbom.json`) for every release archive
- `govulncheck` dedicated CI job (pinned to v1.1.4)
- HTTP server `ReadTimeout` and `WriteTimeout` (30s each)

### Changed
- README quick-start restructured: API key note, verification step, and 30-second framing added
- Exporter-only Docker Compose snippet added for users with existing Prometheus
- Removed redundant `--cache-ttl=60s` from docker-compose.yml (equals compiled default)
- Configuration table clarifies which options are flag-only vs env-var
- Relicensed from MPL-2.0 to MIT
- `IsReady()` readiness probe now latches on first successful scrape — pod stays ready through transient API outages
- `make compose-up` now pre-builds the Go binary before running `docker compose up --build`
- Docker Compose port bindings changed to `127.0.0.1:PORT:PORT` for local dev safety
- `GRAFANA_ADMIN_PASSWORD` is now required in Docker Compose (no fallback default)
- Kubernetes `deployment.yaml` no longer embeds a Secret object; use `secret.yaml.example`
- Kubernetes Deployment hardened: `automountServiceAccountToken: false`, `seccompProfile: RuntimeDefault`
- Helm chart validates that either `apiKey` or `existingSecret` is set
- `Collect()` snapshots cache under a brief read lock then releases before metric emission
- URL labels strip query parameters before use
- `golangci-lint` pinned to v2.11.4 in CI
- `goreleaser` pinned to `~> v2` in release workflow

### Fixed
- `validateBaseURL()` enforces https (or localhost) scheme in `WithBaseURL()`
- `HyperpingCoreMonitorDown` alert expression corrected to match actual metric labels
- Prometheus `--web.enable-lifecycle` removed from Docker Compose args

## [0.1.0] - 2026-03-30

### Added

- Initial release — standalone Prometheus exporter for Hyperping monitoring service
- Full metric coverage: monitors, healthchecks, outages, SLA ratios, health scores, and escalation tiers
- Background cache refresh with configurable TTL (default 60s)
- Circuit breaker and retry logic in the Hyperping API client
- 3 Grafana dashboards: Fleet Overview, Shared Infrastructure, Tenant Health
- Prometheus alert rules and recording rules
- Docker Compose stack (exporter + Prometheus + Grafana)
- Kubernetes manifests: Deployment, Service, ServiceMonitor
- Multi-arch Dockerfile (linux/amd64, linux/arm64) based on distroless/static
- GoReleaser configuration for multi-arch releases
- GitHub Actions CI and release pipelines
