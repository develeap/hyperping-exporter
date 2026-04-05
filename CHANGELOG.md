# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

## [1.0.3] - 2026-04-05

### Added
- Helm chart committed to VCS (previously excluded by `.gitignore` binary pattern)
- Kubernetes `NetworkPolicy` for raw manifests and Helm chart (DNS scoped to kube-system, HTTPS egress blocks RFC1918)
- Cosign keyless image signing in release pipeline (Sigstore/Fulcio)
- Trivy container image scanning in CI (fails on CRITICAL/HIGH CVEs)
- GHCR mirror (`ghcr.io/develeap/hyperping-exporter`) alongside Docker Hub
- License compliance check in CI (rejects copyleft: GPL-3, AGPL, SSPL, EUPL)
- VCR cassette sanitization hooks: PII, infrastructure headers, and sensitive request header values are automatically redacted on recording
- Outage pagination truncation warning when page cap is reached
- `StatusPageService.ID` null/absent tests for group-header edge case
- `automountServiceAccountToken: false` in Helm deployment template

### Changed
- Outage pagination limit reduced from 100 to 10 pages to prevent linear degradation on long-lived accounts
- `StatusPageService.ID` type changed from `interface{}` to `*FlexibleString` for type-safe string/number handling
- All GitHub Actions pinned to commit SHAs (supply chain hardening)
- Helm `securityContext` defaults now include `seccompProfile: RuntimeDefault` and `runAsGroup: 65534`
- Helm PDB changed from `minAvailable` to `maxUnavailable` (safe with single replica)
- Docker Compose hardened: `read_only`, `cap_drop: ALL`, `no-new-privileges`, resource limits on all services
- K8s deployment pod-level `securityContext` expanded to full Restricted PSS compliance
- Grafana admin username configurable via `GRAFANA_ADMIN_USER` env var
- Exporter image version in Compose configurable via `EXPORTER_VERSION` env var
- SBOM output format explicitly pinned to `spdx-json`
- `govulncheck` pinned to `@v1.1.4` in Makefile (matches CI)
- `HyperpingDataStale` alert threshold documented with formula: `threshold = 2 x cache-ttl`
- Helm `apiKey` value marked as development-only; `existingSecret` recommended for production
- Dependabot expanded to cover Helm chart directory

### Fixed
- `.gitignore` binary pattern `hyperping-exporter` scoped to root (`/hyperping-exporter`) to stop excluding `deploy/helm/hyperping-exporter/`
- VCR `api_key=` URL sanitization now replaces the full key=value pair (was only replacing the prefix)
- Scrubbed PII (emails, profile URLs), infrastructure headers (Cf-Ray, Nel, Ratelimit-Policy), and token values from 27 VCR cassettes
- Removed mutable `latest` Docker tag from GoReleaser release pipeline
- Cosign signs images by digest (not tag) to prevent TOCTOU race

### Removed
- `latest` Docker tag from release pipeline (use versioned tags instead)

## [1.0.2] - 2026-03-31

### Added
- Docker Hub description auto-sync on release via `peter-evans/dockerhub-description`

### Changed
- Migrated from `goreleaser dockers` to `dockers_v2` for native multi-arch Docker builds (linux/amd64, linux/arm64)
- SBOM and provenance attestations attached to Docker images

### Fixed
- `--push` flag added to buildx to support SBOM and provenance attestations

## [1.0.1] - 2026-03-31

### Changed
- Container registry switched from GHCR to Docker Hub (`khaledsalhabdeveleap/hyperping-exporter`)

### Fixed
- Reduced cyclomatic complexity in status page test

## [1.0.0] - 2026-03-31

### Added
- `deploy/.env.example` — credential template for Docker Compose (`HYPERPING_API_KEY`, `GRAFANA_ADMIN_PASSWORD`); `deploy/.env` is gitignored
- Binary releases now include Windows (386, amd64), Linux 386, and Linux arm (v6, v7) targets
- Cross-compile CI job validates all 5 new target platforms on every PR
- `--namespace` flag (env: `HYPERPING_EXPORTER_NAMESPACE`, default: `hyperping`) to customise the Prometheus metric prefix; explicit flag always beats env var
- Client observability metrics: `hyperping_client_api_call_duration_seconds`, `hyperping_client_retry_total`, `hyperping_client_circuit_breaker_state` expose API call latency, retry counts, and circuit breaker state
- `WithCircuitBreakerSettings(gobreaker.Settings)` and `WithNoCircuitBreaker()` client options
- `deploy/k8s/secret.yaml.example` — Secret manifest template for Kubernetes deployments
- PodDisruptionBudget template in Helm chart (`podDisruptionBudget.enabled`)
- SBOM generation (`${artifact}.sbom.json`) for every release archive
- `govulncheck` dedicated CI job (pinned to v1.1.4)
- HTTP server `ReadTimeout` and `WriteTimeout` (30s each)

### Changed
- Relicensed from MPL-2.0 to MIT
- README quick-start restructured: API key note, verification step, and 30-second framing added
- Exporter-only Docker Compose snippet added for users with existing Prometheus
- Configuration table clarifies which options are flag-only vs env-var
- `IsReady()` readiness probe now latches on first successful scrape — pod stays ready through transient API outages
- `make build` now uses `CGO_ENABLED=0` and `-trimpath` to produce a static binary compatible with distroless containers
- `make compose-up` now pre-builds the Go binary before running `docker compose up --build`
- Docker Compose port bindings changed to `127.0.0.1:PORT:PORT` for local dev safety
- `GRAFANA_ADMIN_PASSWORD` is now required in Docker Compose (no fallback default)
- Removed redundant `--cache-ttl=60s` from docker-compose.yml (equals compiled default)
- Kubernetes `deployment.yaml` no longer embeds a Secret object; use `secret.yaml.example`
- Kubernetes Deployment hardened: `automountServiceAccountToken: false`, `seccompProfile: RuntimeDefault`
- Helm chart validates that either `apiKey` or `existingSecret` is set
- `Collect()` snapshots cache under a brief read lock then releases before metric emission
- URL labels strip query parameters before use
- `golangci-lint` pinned to v2.11.4 in CI
- `goreleaser` pinned to `~> v2` in release workflow

### Fixed
- `make build` produced a dynamically-linked binary that crashed in `distroless/static`; fixed with `CGO_ENABLED=0`
- `--namespace` env var override incorrectly beat an explicit flag value; env var now only applies when flag is unset
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
