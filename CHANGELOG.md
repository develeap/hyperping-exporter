# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

## [1.5.0] - 2026-05-13 [Chart only, binary unchanged]

### Highlights

- **Production-readiness hardening for the Helm chart.** Ten peer-review items landed as one cutover: empirically-grounded `resources` defaults, reconciled probe periods, PSS-restricted documentation, a uniform safe-arg rendering helper, ExternalSecret support with mutual-exclusion guards, `replicaCount: 0` as a fully-supported scaled-to-zero state, a `validateReplicaCount` singleton guard, an opt-in `NetworkPolicy` (operator-configurable DNS and egress) with a `cilium.io/v2 CiliumNetworkPolicy` FQDN-restriction variant, and a `validateCacheTTL` guard that aborts the render on bare-integer cacheTTL.
- **New CI gates.** `kubeconform -strict` schema validation against a pinned `datreeio/CRDs-catalog` tag plus a `kind` PSS-restricted live admission job; every commit on the branch keeps `render_test.py`, `helm lint`, and `make helm-kubeconform` green (bisect-safe).

### Added

- `templates/externalsecret.yaml` rendering `external-secrets.io/v1beta1 ExternalSecret` (default; override via `externalSecret.apiVersion` to `external-secrets.io/v1` on ESO 0.10+). The plan originally targeted `external-secrets.io/v1` as the default, but the pinned `datreeio/CRDs-catalog` tag does not ship the `v1` schema, which would have left `kubeconform -strict` unable to validate the fixture. The default was therefore rolled back to `v1beta1` for chart 1.5.0 with the operator opt-in preserved (see Upgrade notes).
- `templates/networkpolicy-cilium.yaml` rendering `cilium.io/v2 CiliumNetworkPolicy` when `networkPolicy.fqdnRestriction.enabled: true`; mutually exclusive with the vanilla `templates/networkpolicy.yaml`.
- `_helpers.tpl` helpers: `hyperping-exporter.arg` (safe-arg rendering), `secretSourceCount`, `validateSecretSources`, `validateReplicaCount`, `validateCacheTTL`, `validateNoTestKeys`.
- `tests/admission_env.sh`, `tests/admission_test.sh`, `tests/kind-pss.yaml`, `tests/kind-pss-config/admission-config.yaml`, `tests/scripts/resolve_pins.sh`, `tests/pins.expected.yaml`: admission-test harness and pin resolver.
- 28 new render-harness cases covering external-secret positive/defaults/missing-store, replicas-zero / replicas-multi, three secret-conflict variants, missing-source abort, cache-ttl numeric and abort, log-level numeric (no `%!s` artefact), metrics-path special chars, networkpolicy-default, pdb-enabled/structural with multi-replica leakage proof, and seven Cilium variants (egress-only, matchLabels, matchExpressions, mixed, plus three fail paths).
- `Makefile` targets `helm-render`, `helm-kubeconform`, `helm-pss`, `helm-pss-clean`, `helm-ci-fast`, `helm-ci`.

### Changed

- Helm chart `Chart.yaml` `version` `1.1.0` → `1.5.0`.
- Helm chart `appVersion` `"1.4.0"` → `"1.4.1"` to track the published binary.
- `values.yaml` `resources` block re-tuned from a 30-minute Docker observation (peak RSS 11.47 MiB, peak CPU 6.33%): requests `cpu: 50m, memory: 64Mi`; limits `cpu: 200m, memory: 256Mi` (~30% request headroom; ~4x limit headroom).
- `values.yaml` `livenessProbe` `initialDelaySeconds: 5 → 10`, `periodSeconds: 30 → 10`, `failureThreshold` introduced at `3`; `readinessProbe` `initialDelaySeconds: 10 → 5`, `periodSeconds: 15 → 5`, `failureThreshold: 3`.
- `values.yaml` `networkPolicy` block reshaped. `enabled` stays `false` (cluster-coupled decision: the chart cannot know your CNI capability, DNS topology, or whether your platform team reserves NetworkPolicy authority). New `networkPolicy.dns` (namespace, podLabels) and `networkPolicy.egress` (cidr, except, port) knobs let operators retarget the DNS rule and reshape egress without forking the chart. `egress.except` defaults to the RFC1918 ranges, blocking intra-cluster lateral movement on TCP/443; operators routing api.hyperping.io through an in-cluster egress gateway set `egress.except: []`. The DNS rule's default selector (`k8s-app: kube-dns`) is applied via a template fallback rather than a deep-merging values default, so a non-empty operator `dns.podLabels` REPLACES the selector wholesale (avoids the Helm map-merge footgun that would leak `k8s-app: kube-dns` into clusters with different DNS labels).
- `values.yaml` `serviceMonitor` documents the cluster-coupled assumption (Prometheus Operator CRDs must be installed) that motivates the existing `enabled: false` default.
- `values.yaml` `resources` comment no longer references a gitignored measurement-artifact path and explicitly tells operators to measure their own workload before pinning these defaults in production.
- `templates/deployment.yaml` arg lines migrated to the safe-arg helper; the legacy 2-line `apiKey || existingSecret` guard replaced by validator includes; the env block now gates on `secretSourceCount > 0` so `replicaCount: 0` deployments without a secret source render cleanly.
- `templates/secret.yaml` now suppresses when `externalSecret.enabled: true`.

### Removed

- The `podDisruptionBudget` block has been removed from `values.yaml` and the `templates/pdb.yaml` template has been deleted. The exporter is a singleton (`validateReplicaCount` aborts the render at `replicaCount > 1`), which means a PodDisruptionBudget rendering gate is unreachable for any production caller: you cannot run a maxUnavailable budget across a single replica without blocking node drains. The previous knob was therefore dead config and is no longer documented. Operators who had previously set `podDisruptionBudget.enabled: true` should remove the entry from their values overlays; Helm ignores unknown values keys, so the chart will still render cleanly but the value has no effect. If you need to gate voluntary disruptions for the singleton pod, the supported pattern is a cluster-level node-drain orchestration policy (e.g. PriorityClass + scheduling rules), not a PDB.

### Upgrade notes

- **PSS namespace labelling.** The chart does not create or label the target Namespace. Operators MUST label the target namespace `pod-security.kubernetes.io/enforce=restricted` to enforce; the chart's container and pod SecurityContext defaults already satisfy that profile.
- **Helm-to-ESO migration.** Switching an existing release from `apiKey` / `existingSecret` to `externalSecret.enabled: true` requires a brief window where the chart-managed Secret is replaced by an ESO-reconciled Secret of the same name. Stage: (1) deploy ESO and the `(Cluster)SecretStore`; (2) bump the chart with `externalSecret.enabled: true` AND clear `apiKey` / `existingSecret` in the SAME release; (3) verify the ExternalSecret reaches `SecretSynced` before scaling the Deployment.
- **`replicaCount: 2` (or higher) now aborts the render** with a `validateReplicaCount` error. The exporter is a singleton; scale horizontally by sharding monitor namespaces across separate releases.
- **`cacheTTL` MUST be a quoted Go duration string** (e.g. `"60s"`). Bare integers abort the render via `validateCacheTTL`.
- **NetworkPolicy is opt-in.** The default remains `networkPolicy.enabled: false` (the chart cannot know your cluster's CNI, DNS topology, or NP-authority story). Operators who want default-deny egress with a Hyperping-API allow rule set `enabled: true` and review `networkPolicy.dns` and `networkPolicy.egress` against their cluster: the DNS rule's default selector matches vanilla kube-dns / CoreDNS (`k8s-app: kube-dns` in `kube-system`); clusters running NodeLocal DNSCache or non-default CoreDNS labels override `networkPolicy.dns.podLabels` (a non-empty value replaces the default selector wholesale to avoid Helm map-merge leakage). The egress rule defaults to TCP/443 to `0.0.0.0/0` with RFC1918 excluded; clear `networkPolicy.egress.except` to permit an in-cluster egress gateway.
- **`config.webConfigFile` is currently unsupported and aborts the render** via the new `validateWebConfigFile` helper. The binary's `--web.config.file` flag puts it into TLS mode, but the chart's probes (`livenessProbe.httpGet.scheme`, `readinessProbe.httpGet.scheme`) and the ServiceMonitor template's `endpoints[].scheme` hardcode HTTP. Enabling TLS in isolation would leave the pod permanently NotReady and silently break Prometheus scrapes; failing the render loud is the safer default. Operators who need TLS today should terminate at a sidecar or at the Service edge. A future chart release will wire `httpGet.scheme` + ServiceMonitor `scheme` + `tlsConfig` together so the knob becomes load-bearing.
- **ExternalSecret apiVersion default is `external-secrets.io/v1beta1`.** Operators running ESO 0.10+ (where the `v1` CRD is GA and `v1beta1` is deprecated) MUST set `externalSecret.apiVersion: external-secrets.io/v1` explicitly. The chart's `kubeconform` job pins a CRDs-catalog tag that does not yet ship the `v1` schema; the chart will roll the default to `v1` in a subsequent chart bump once the catalog tag does.

## [1.4.1] - 2026-05-12

### Added

- **`config.excludeNamePattern`** (RE2 regex) and **`config.mcpUrl`** Helm chart values, rendered into the Deployment container args via `toJson` so backslash-bearing regexes round-trip byte-for-byte through Kubernetes' YAML decoder.
- **Render-test harness** at `deploy/helm/hyperping-exporter/tests/render_test.py` driven by PyYAML, with fixture files exercising defaults, existingSecret-only, plain-ASCII regex, the README example, a single-quote-containing regex, `mcpUrl` alone, and both flags together.
- **Helm CI workflow** (`.github/workflows/helm-ci.yml`) that runs `helm lint` and the render harness on chart-touching PRs.

### Changed

- Helm chart `Chart.yaml` `version` `1.0.0` → `1.1.0`.
- Helm chart `appVersion` `"1.0.3"` → `"1.4.0"` to match the released binary that understands `--exclude-name-pattern` and `--mcp-url`. Side-effect: the `app.kubernetes.io/version` label on Secret, Service, and Deployment flips from `"1.0.3"` to `"1.4.0"`.
- Helm chart `image.repository` default `develeap/hyperping-exporter` → `khaledsalhabdeveleap/hyperping-exporter` to match the published Docker Hub repo.
- `.github/workflows/ci.yml` `paths-ignore` lists extended with `.github/workflows/helm-ci.yml` so future chart-only changes skip the seven Go jobs.
- **`goreleaser/goreleaser-action`** `7.1.0` → `7.2.1` (dependabot).
- **`aquasecurity/trivy-action`** `0.35.0` → `0.36.0` (dependabot).

### Security

- **`golang.org/x/net`** `v0.51.0` → `v0.54.0` (GO-2026-4918: infinite loop in HTTP/2 transport on bad `SETTINGS_MAX_FRAME_SIZE`, reachable via `hyperping.Client.ListIncidents`). Companion bumps: `golang.org/x/crypto` `v0.49.0` → `v0.51.0`, `golang.org/x/sys` `v0.42.0` → `v0.44.0`, `golang.org/x/text` `v0.35.0` → `v0.37.0`.

## [1.4.0] - 2026-04-26

### Added

- **Maintenance-window suppression** for `HyperpingMonitorDown`, `HyperpingMonitorActiveOutage`, and `HyperpingCoreMonitorDown`. Each alert now has `unless on(uuid) hyperping_monitor_in_maintenance == 1` appended, so monitors covered by an active maintenance window do not page during the planned downtime.
- **`HyperpingOpenIncidents`** alert: fires when there are unresolved Hyperping incidents for more than 30 minutes.
- **`HyperpingMonitorRegionalOutage`** alert: fires when a monitor is down in some regions but up in others (partial regional infrastructure failure rather than complete outage). Requires monitors configured with multiple regions.
- **`HyperpingMonitorAnomalyHigh`** alert: fires when MCP-derived anomaly score is sustained above 0.8 for 15 minutes. Requires `--mcp-url`.
- **`HyperpingMonitorMTTAHigh`** alert: fires when MCP-derived MTTA exceeds 10 minutes for over an hour. Requires `--mcp-url`.
- **`hyperping:fleet:anomalies_high`** recording rule: count of monitors currently above the 0.8 anomaly threshold; useful for fleet-wide trend panels.
- **promtool unit tests** for bundled alerts (`deploy/prometheus/tests/alerts.test.yml`). Verifies maintenance suppression, regional outage detection, anomaly threshold behaviour, and recording-rule output. New CI job runs `promtool test rules` on every PR.
- **Sanity assertion in `TestPrometheusRulesReferenceOnlyEmittedMetrics`**: asserts at least 30 metric names were extracted, catching the case where the underlying `Desc.String()` regex silently breaks in a future client_golang upgrade.

### Changed

- **BREAKING (small impact): `hyperping_monitors_excluded` renamed to `hyperping_excluded_monitors`.** The metric was introduced in v1.3.0 (one day before v1.3.1) and the new name reads more naturally ("excluded monitors" parses cleanly as adjective + noun). Anyone who already started using the v1.3.0 name needs to update dashboards/alerts. No backwards-compatibility shim is shipped.
- **`avgSLAForPeriod` now returns `(float64, bool)`.** When no report's monitor is in the index (e.g., `--exclude-name-pattern` removes every monitor that has reports), the function returns `(0, false)` and the caller skips emitting `hyperping_tenant_health_score` rather than emitting a misleadingly low value. Internal API change; not user-facing.
- **`HyperpingMonitorAnomalyHigh` annotation** uses `printf "%.2f"` instead of `humanize` for the score. Prevents the score 0.95 from rendering as "950m" (SI prefix).
- CI's `prom/prometheus` Docker image is now pinned by digest (`@sha256:378f4e...`) for supply-chain reproducibility, matching the rest of the workflow's habit of pinning actions by SHA.
- CI's `promtool` job no longer waits on `verify`; it can run in parallel with the Go module check.
- `deploy/k8s/deployment.yaml` example image tag changed from `:<VERSION>` to `:REPLACE_ME` for unambiguous "fill this in" semantics.
- Makefile `test` target now enforces the same 90% coverage threshold as CI, so locally-run tests fail in the same conditions as CI.

## [1.3.1] - 2026-04-26

### Fixed

- **Tenant SLA aggregates ignored `--exclude-name-pattern`.** `hyperping_tenant_avg_sla_ratio` summed only visible monitors' SLA but divided by `len(reports)` (which still counted excluded monitors), and `hyperping_tenant_health_score`'s `avgSLAForPeriod` summed reports for every monitor unconditionally. Both metrics were systematically pulled down by the visible/total ratio. `hyperping_tenant_active_outages` and `hyperping_tenant_monitors_up_ratio` were unaffected.
- **Bundled alerts that never fired.** `HyperpingMultipleActiveOutages` and `HyperpingNoMonitors` referenced `hyperping_monitors_total`, but the exporter emits `hyperping_monitors` (no `_total` suffix). Dividing by an absent series produced no result, so the alerts were silent in v1.3.0. Fixed by using the correct metric name. Users who deployed v1.3.0 alerts must redeploy `alerts.yml`.
- Removed unreachable `if coreErr != nil` branch after the lock in `Refresh()`; the early return at the top of the function already handles this case.
- `fetchMcpData` MTTA branch logged "failed to fetch" debug lines even when the API legitimately returned `(nil, nil)` with no error. Mirrored the Response Time branch's `else if err != nil` to silence noise on empty results.

### Added

- `filterReportsByMonitorUUID` (private) applied in `Refresh()` alongside the existing outage filter, using the same `includedUUIDs` set. Root-cause fix for the SLA aggregate bug above.
- Defensive `slaCount` counter in `emitReportMetrics` so the tenant SLA average can never again be divided by a count larger than what was actually summed.
- `TestPrometheusRulesReferenceOnlyEmittedMetrics`: walks `deploy/prometheus/alerts.yml` and `recording-rules.yml`, extracts every `hyperping_*` / `hyperping:*` identifier from each `expr:` field, and asserts each one is either an emitted Prometheus descriptor or a recording-rule output. Catches the typo class of bug that `promtool check rules` cannot.
- New `promtool` CI job that validates rule syntax on every PR.

### Changed

- `avgSLAForPeriod` now requires the `monitorIndex` and skips reports whose UUID is absent. The function is correct independently of upstream filtering, so a future caller cannot reintroduce the bug by passing unfiltered reports.
- `fetchMcpData` short-circuits with an explicit early return when `len(monitors) == 0`, instead of relying on `numWorkers=0` to coincidentally produce the same outcome.
- `hyperping_monitors_excluded` help text now states the relationship with `hyperping_monitors` explicitly: "...hyperping_monitors counts the visible remainder."
- `validateNamespace` now documents why the 64-character cap exists (downstream Kubernetes label and storage backend limits, not a Prometheus protocol requirement).
- Per-tier `hyperping:tier:monitors_up_ratio` recording rule now documents that the `* 0` zero-guard cannot synthesise an empty tier — if every monitor for a tier is removed, no series is emitted (which is correct).
- CI no longer ignores `deploy/prometheus/**` paths. Changes to alert and recording rules now trigger the full CI matrix, including the new metric-reference and promtool checks.
- `deploy/k8s/deployment.yaml` example image tag changed from `:latest` to a `:<VERSION>` placeholder so an unmodified `kubectl apply -f` fails with a clear `ImagePullBackOff` instead of silently following whatever tag is current.

## [1.3.0] - 2026-04-25

### Added

- **`--exclude-name-pattern` flag**: RE2 regex that filters monitors by name before any metric computation. Excluded monitors are dropped from all `hyperping_monitor_*` metrics and from tenant aggregates (`up_ratio`, `active_outages`, `health_score`). Outages belonging to excluded monitors are filtered out too, so synthetic drill monitors no longer inflate fleet health dashboards. Typical use: `--exclude-name-pattern='\[DRILL|\[TEST'`.
- **`hyperping_cache_ttl_seconds`** constant gauge exposing the configured `--cache-ttl` value. Enables the bundled `HyperpingDataStale` alert to self-configure its threshold (`2 × cache_ttl`) without manual updates when the flag changes.
- **`hyperping_monitors_excluded`** gauge: count of monitors filtered out by `--exclude-name-pattern` on the last refresh; always emitted (value 0 when no pattern is configured) so exclusions are observable in Prometheus without log scraping.
- New `HyperpingNoMonitors` alert: fires when scrape succeeds but the API returns zero monitors (deleted monitors, API key mismatch, or an over-aggressive `--exclude-name-pattern`).
- Alertmanager inhibition rule examples added as comments in `alerts.yml`, including a multi-cluster `equal: [cluster]` note.

### Changed

- **`HyperpingMultipleActiveOutages`**: replaced absolute `> 3` threshold with a fleet-size-aware ratio (`> 10% of monitors`) plus a division-by-zero guard. (Note: this alert and `HyperpingNoMonitors` referenced an incorrect metric name in v1.3.0 and were silent in production. Fixed in v1.3.1.)
- **`HyperpingDataStale`**: threshold is now `2 * hyperping_cache_ttl_seconds` instead of a hardcoded value, adapting automatically when `--cache-ttl` is changed.
- **Recording rules** (`deploy/prometheus/recording-rules.yml`): fleet counts (`monitors_up`, `monitors_down`, `monitors_paused`) and SLA breach counts now use `or vector(0)` so they return 0 instead of disappearing when all monitors match the filter condition. Per-tier `monitors_up_ratio` numerator uses a label-preserving `* 0` pattern so the ratio evaluates to 0 (not absent) when all monitors in a tier are down.
- **Grafana dashboards** (`fleet-overview`, `shared-infrastructure`, `tenant-health`): can now be imported directly via the Grafana UI ("Dashboards → Import → Upload JSON file"). Added `__inputs` block and replaced all 43 bare datasource references with `${DS_PROMETHEUS}`.
- Provisioning datasource pinned to a stable `uid: hyperping-prometheus` so dashboards can reference it by ID across redeploys.

### Fixed

- **Nil pointer panic in `fetchMcpData` (intermittent).** `GetMonitorResponseTime` and `GetMonitorMtta` could return `(nil, nil)`; accessing fields on the nil pointer crashed the scrape. The v1.2.1 release claimed this fix but the guard was never actually added. Now correctly guarded with `report != nil` alongside the existing `err == nil` check.

## [1.2.1] - 2026-04-25

### Fixed

- Nil pointer panic in MCP worker pool: `GetMonitorResponseTime` and `GetMonitorMtta` return `(nil, nil)` when the MCP server returns an empty result. Accessing `report.Avg` / `report.AvgWait` on a nil pointer caused a crash during scrape. Added `report != nil` guard alongside the existing `err == nil` check.
- Removed stale `hyperping-go v0.3.0` hashes from `go.sum` (left behind after upgrading to v0.4.0).

## [1.2.0] - 2026-04-25

### Added

- **MCP-sourced metrics** via the Hyperping MCP server (requires `--mcp-url`):
  - `hyperping_monitor_response_time_seconds{uuid,name}` - average response time per monitor.
  - `hyperping_monitor_mtta_seconds{uuid,name}` - mean time to acknowledge per monitor.
  - `hyperping_monitor_anomaly_count{uuid,name}` - number of detected anomalies.
  - `hyperping_monitor_anomaly_score{uuid,name}` - highest anomaly score.
  - `hyperping_alerts{uuid,name}` - recent alert snapshot count (gauge).
- MCP metrics are fetched in parallel with REST metrics. If MCP is unavailable or unconfigured, all existing REST metrics continue to work (graceful degradation).
- Per-operation 8-second timeout on MCP worker pool calls to prevent scrape blocking.

### Changed

- `--mcp-url` flag now validates the URL at startup: must start with `https://` (or `http://localhost` for local dev). Invalid URLs cause a non-zero exit instead of silently misbehaving.
- Upgraded `github.com/develeap/hyperping-go` from v0.3.0 to v0.4.0.

### Fixed

- MCP graceful degradation: REST metrics are now covered by a dedicated test (`TestRefresh_McpErrorIsNonFatal`) confirming they remain available when all MCP calls return errors.

### Breaking: metric renames

Dashboards referencing the old names must be updated:

| Old name | New name | Reason |
|----------|----------|--------|
| `hyperping_alerts_total` | `hyperping_alerts` | Was typed as Counter but is a snapshot gauge; `_total` is reserved for counters |
| `hyperping_monitor_response_time_avg_seconds` | `hyperping_monitor_response_time_seconds` | `_avg` is not a Prometheus unit suffix |

## [1.1.0] - 2026-04-09

### Added

- **`tenant` and `tier` labels** on all per-monitor metrics (`hyperping_monitor_up`,
  `hyperping_monitor_sla`, etc.). `tenant` is the tenant ID; `tier` is `core`,
  `noncore`, or `unknown` derived from escalation policy name (EXP-01).
- **`hyperping_monitor_in_maintenance`** gauge - 1 when monitor has an active
  maintenance window, 0 otherwise. Prevents false-positive alerts during planned
  downtime (EXP-02).
- **`hyperping_monitor_up_by_region{uuid,name,region}`** gauge - per-region up/down
  status derived from active outage `detectedLocation`/`confirmedLocations`. Additive
  alongside the existing `hyperping_monitor_up` family (EXP-03).
- **`hyperping_incident_active`** and **`hyperping_maintenance_active`** event gauges
  with `tenant`, `tier`, `severity` labels (EXP-04).
- **`WatchdogStalled`** alert rule - fires when the watchdog has not updated its
  state file within the expected interval (AUTO-03).
- **`WatchdogNeverRan`** alert rule - fires when no watchdog state file exists
  at all (AUTO-03).
- Dependabot group for `hyperping-go` module auto-updates.

### Changed

- Migrated from vendored `internal/client/` to the shared
  `github.com/develeap/hyperping-go` module. No API or metric changes - internal
  only.
- Alert and recording rules updated to include `tenant` and `tier` label selectors
  matching the new EXP-01 schema. **Existing dashboards must add `tenant` and `tier`
  to their variable filters** - see `deploy/grafana/` for updated dashboard JSONs (OPS-40).

### Fixed

- Release workflow: GoReleaser archive name corrected.
- Collector: minor field mapping corrections from phase 0 audit.

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
