# hyperping-exporter

[![CI](https://github.com/develeap/hyperping-exporter/actions/workflows/ci.yml/badge.svg)](https://github.com/develeap/hyperping-exporter/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/develeap/hyperping-exporter)](https://github.com/develeap/hyperping-exporter/releases/latest)
[![Go Report Card](https://goreportcard.com/badge/github.com/develeap/hyperping-exporter)](https://goreportcard.com/report/github.com/develeap/hyperping-exporter)
[![License: MIT](https://img.shields.io/badge/License-MIT-brightgreen.svg)](LICENSE)
[![Docker](https://img.shields.io/badge/ghcr.io-develeap%2Fhyperping--exporter-blue?logo=docker)](https://github.com/develeap/hyperping-exporter/pkgs/container/hyperping-exporter)

> Get Hyperping monitor metrics into Prometheus in 30 seconds.

A standalone [Prometheus](https://prometheus.io/) exporter for [Hyperping](https://hyperping.io/) monitoring. Exposes monitor status, healthchecks, SLA ratios, outage metrics, and tenant health scores as Prometheus gauges.

Extracted from [develeap/terraform-provider-hyperping](https://github.com/develeap/terraform-provider-hyperping) to serve as a standalone, reusable exporter. The same battle-tested API client (with circuit breaker, retry, and rate-limit handling) is embedded here with zero runtime dependency on the provider.

---

## Quick start

> [!NOTE]
> **Get your API key first:** Log in to [Hyperping](https://hyperping.io) →
> Account Settings → API → Create API Key.

```bash
docker run -p 9312:9312 \
  -e HYPERPING_API_KEY=your_key \
  ghcr.io/develeap/hyperping-exporter:latest
```

```bash
curl -s http://localhost:9312/metrics | grep hyperping_monitor_up
```

If you see output, you're done.

<details><summary>Other installation methods</summary>

**Binary download** — grab the latest release from the [Releases](https://github.com/develeap/hyperping-exporter/releases) page. Each archive includes an SBOM (`*.sbom.json`) for supply-chain verification. Then:

```bash
HYPERPING_API_KEY=your_key ./hyperping-exporter
```

**Install with Go**

```bash
go install github.com/develeap/hyperping-exporter@latest
HYPERPING_API_KEY=your_key hyperping-exporter
```

</details>

Metrics are served at `http://localhost:9312/metrics`.

---

## Configuration

All flags can also be set via environment variables.

| Flag | Env var | Default | Description |
|------|---------|---------|-------------|
| `--api-key` | `HYPERPING_API_KEY` | *(required)* | Hyperping API key. Also accepts `HYPERPING_TOKEN` (compatibility alias) |
| `--listen-address` | `(flag only)` | `:9312` | Address to listen on |
| `--metrics-path` | `(flag only)` | `/metrics` | Path to expose metrics on |
| `--cache-ttl` | `(flag only)` | `60s` | How often to refresh data from the API |
| `--log-level` | `(flag only)` | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `--log-format` | `(flag only)` | `text` | Log format: `text` or `json` |
| `--namespace` | `HYPERPING_EXPORTER_NAMESPACE` | `hyperping` | Metric name prefix. Must match `[a-zA-Z_][a-zA-Z0-9_]{0,63}`. **Changing from the default requires updating any alert rules and Grafana dashboards that reference hardcoded `hyperping_*` metric names.** |

> Only `HYPERPING_API_KEY` is read from the environment by default;
> all other options use CLI flags, which map cleanly to Docker `command:`
> entries. Use `HYPERPING_EXPORTER_NAMESPACE` for the namespace flag.

### Performance Tuning
| Concern | Recommendation |
|---------|---------------|
| **Freshness vs API load** | Default 60s TTL makes 5 parallel API calls per refresh. Hyperping's rate limit is 60 req/min — stay above 30s TTL for comfortable headroom. |
| **Cardinality** | Each monitor contributes ~9 time series. 100 monitors ≈ 900 series — negligible for any Prometheus setup. |
| **Scrape interval** | Set Prometheus scrape interval ≥ cache-ttl. Scraping faster than the cache refreshes returns identical data. |

### TLS and Basic Authentication
Use `--web.config.file` to enable TLS or HTTP basic auth:
```yaml
# web-config.yml
tls_server_config:
  cert_file: server.crt
  key_file:  server.key
basic_auth_users:
  prometheus: $2y$12$...  # bcrypt hash
```
See the [exporter-toolkit web configuration](https://github.com/prometheus/exporter-toolkit/blob/master/docs/web-configuration.md) for the full schema.

---

## Available metrics

### Monitor metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `hyperping_monitor_up` | Gauge | `uuid`, `name` | Whether the monitor is up (1) or down (0) |
| `hyperping_monitor_paused` | Gauge | `uuid`, `name` | Whether the monitor is paused (1) or active (0) |
| `hyperping_monitor_ssl_expiration_days` | Gauge | `uuid`, `name` | Days until SSL certificate expiration |
| `hyperping_monitor_check_interval_seconds` | Gauge | `uuid`, `name` | Monitor check frequency in seconds |
| `hyperping_monitor_info` | Gauge | `uuid`, `name`, `protocol`, `url`, `project_uuid`, `http_method` | Monitor metadata (always 1) |
| `hyperping_monitor_outage_active` | Gauge | `uuid`, `name` | Whether the monitor has an active unresolved outage (1) or not (0) |
| `hyperping_monitor_active_outage_status_code` | Gauge | `uuid`, `name` | HTTP status code of the current active outage; 0 when none |
| `hyperping_monitor_sla_ratio` | Gauge | `uuid`, `name`, `period` | SLA ratio (0–1) for `period` = `24h`, `7d`, or `30d` |
| `hyperping_monitor_outages` | Gauge | `uuid`, `name`, `period` | Outage count for the period |
| `hyperping_monitor_downtime_seconds` | Gauge | `uuid`, `name`, `period` | Total downtime in seconds for the period |
| `hyperping_monitor_longest_outage_seconds` | Gauge | `uuid`, `name`, `period` | Longest single outage in seconds for the period |
| `hyperping_monitor_mttr_seconds` | Gauge | `uuid`, `name`, `period` | Mean Time To Recovery in seconds (omitted when 0) |
| `hyperping_monitor_escalation_tier` | Gauge | `uuid`, `name`, `tier` | Escalation tier info (always 1); `tier=core` when an escalation policy is set |

### Healthcheck metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `hyperping_healthcheck_up` | Gauge | `uuid`, `name` | Whether the healthcheck is up (1) or down (0) |
| `hyperping_healthcheck_paused` | Gauge | `uuid`, `name` | Whether the healthcheck is paused (1) or active (0) |
| `hyperping_healthcheck_period_seconds` | Gauge | `uuid`, `name` | Expected ping period in seconds |

### Tenant / fleet metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `hyperping_monitors` | Gauge | — | Total number of monitors |
| `hyperping_healthchecks` | Gauge | — | Total number of healthchecks |
| `hyperping_tenant_monitors_up_ratio` | Gauge | — | Fraction of monitors currently up (0–1) |
| `hyperping_tenant_active_outages` | Gauge | — | Total active unresolved outages across all monitors |
| `hyperping_tenant_avg_sla_ratio` | Gauge | `period` | Average SLA ratio across all monitors for the period |
| `hyperping_tenant_health_score` | Gauge | — | Composite health score 0–100; omitted until 30d reports are loaded |

### Exporter self-metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `hyperping_scrape_duration_seconds` | Gauge | — | Duration of the last API scrape |
| `hyperping_scrape_success` | Gauge | — | Whether the last API scrape succeeded (1) or failed (0) |
| `hyperping_data_age_seconds` | Gauge | — | Seconds since the last successful cache refresh |
| `hyperping_build_info` | Gauge | `version`, `revision`, `goversion` | Build metadata (always 1) |

### Client API metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `hyperping_client_api_call_duration_seconds` | Histogram | `method`, `path`, `status_code` | Duration of Hyperping API calls in seconds |
| `hyperping_client_retry_total` | Counter | `method`, `path`, `attempt` | Total retried API requests per endpoint and attempt number |
| `hyperping_client_circuit_breaker_state` | Gauge | `state` | Circuit breaker state: 1 for the active state (`closed`, `half-open`, or `open`) |

### Example PromQL Queries
```promql
# Monitors currently down (excluding paused)
hyperping_monitor_up{} == 0 unless on(uuid) hyperping_monitor_paused == 1

# Number of active outages across fleet
sum(hyperping_monitor_outage_active)

# Fleet 24-hour SLA
hyperping_tenant_avg_sla_ratio{period="24h"}

# Monitors below 99% SLA over 7 days
hyperping_monitor_sla_ratio{period="7d"} < 0.99

# SSL certificates expiring within 14 days
hyperping_monitor_ssl_expiration_days < 14

# Stale data watchdog (alert if data older than 2 minutes)
hyperping_data_age_seconds > 120
```

### Escalation tier join pattern (PromQL)

```promql
# Add tier label to any per-monitor metric:
hyperping_monitor_up * on(uuid, name) group_left(tier) hyperping_monitor_escalation_tier
```

---

## Grafana dashboards

Three pre-built dashboards are included in `deploy/grafana/dashboards/`:

| Dashboard | File | Description |
|-----------|------|-------------|
| Fleet Overview | `fleet-overview.json` | All monitors at a glance: up/down status, active outages, SLA by period |
| Shared Infrastructure | `shared-infrastructure.json` | Core vs. noncore tier breakdown; multi-outage correlation |
| Tenant Health | `tenant-health.json` | Composite health score, MTTR trends, downtime heatmap |

**Import instructions:** In Grafana, go to **Dashboards → Import**, upload the JSON file, and select your Prometheus data source.

When using the Docker Compose stack (`make compose-up`), dashboards are provisioned automatically and available at `http://localhost:3000` (admin / {GRAFANA_ADMIN_PASSWORD}).

---

## Kubernetes deployment

Manifests are in `deploy/k8s/`:

```bash
# Create namespace
kubectl create namespace monitoring

# Create the secret (preferred — avoids leaking the key in the object annotation)
kubectl create secret generic hyperping-credentials \
  --from-literal=api-key=YOUR_KEY -n monitoring

# Deploy
kubectl apply -f deploy/k8s/
```

Files:
- `deployment.yaml` — single-replica Deployment with liveness/readiness probes
- `service.yaml` — ClusterIP Service on port 9312
- `servicemonitor.yaml` — Prometheus Operator `ServiceMonitor` for automatic scrape discovery
- `secret.yaml.example` — Secret manifest template (copy, fill in your key, do not commit)

Security hardening applied to the Deployment: `automountServiceAccountToken: false` and `seccompProfile: RuntimeDefault` on the pod security context.

---

## Helm deployment

A Helm chart is available in `deploy/helm/hyperping-exporter/`. Either `config.apiKey` or `config.existingSecret` must be set — the chart fails at render time if both are empty.

```bash
helm install hyperping-exporter ./deploy/helm/hyperping-exporter \
  --set config.apiKey=your_key \
  -n monitoring --create-namespace
```

Key configuration values:

| Value | Default | Description |
|-------|---------|-------------|
| `config.apiKey` | `""` | Hyperping API key (required if `existingSecret` not set) |
| `config.existingSecret` | `""` | Name of an existing Secret with key `api-key` |
| `config.cacheTTL` | `60s` | How often to refresh data from the API |
| `config.logLevel` | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `serviceMonitor.enabled` | `false` | Create a Prometheus Operator ServiceMonitor |
| `podDisruptionBudget.enabled` | `false` | Create a PodDisruptionBudget |
| `podDisruptionBudget.minAvailable` | `1` | Minimum available pods during disruption |

Includes a PodDisruptionBudget template (disabled by default; enable with `--set podDisruptionBudget.enabled=true`).

---

## Docker Compose full stack

### Exporter only (bring-your-own Prometheus)

If you already run Prometheus, add just the exporter:

```yaml
services:
  hyperping-exporter:
    image: ghcr.io/develeap/hyperping-exporter:latest
    environment:
      HYPERPING_API_KEY: "${HYPERPING_API_KEY}"
    ports:
      - "127.0.0.1:9312:9312"
    restart: unless-stopped
```

Then add this scrape config to your `prometheus.yml`:

```yaml
scrape_configs:
  - job_name: hyperping
    static_configs:
      - targets: ['localhost:9312']
    scrape_interval: 60s
```

### Full stack

Starts the exporter, Prometheus (with alert + recording rules), and Grafana:

```bash
HYPERPING_API_KEY=your_key GRAFANA_ADMIN_PASSWORD=your_password make compose-up
```

`GRAFANA_ADMIN_PASSWORD` is required — the stack will fail loudly if it is not set. All services bind to `127.0.0.1` only for local dev safety.

| Service | URL |
|---------|-----|
| Exporter metrics | http://localhost:9312/metrics |
| Prometheus | http://localhost:9090 |
| Grafana | http://localhost:3000 (admin / {GRAFANA_ADMIN_PASSWORD}) |

```bash
make compose-down
```

---

## Prometheus alerting rules

Pre-configured rules in `deploy/prometheus/alerts.yml`:

| Alert | Severity | Condition |
|-------|----------|-----------|
| `HyperpingMonitorDown` | critical | Monitor down for > 2 min |
| `HyperpingMonitorActiveOutage` | critical | Active outage for > 1 min |
| `HyperpingMultipleActiveOutages` | critical | > 3 concurrent active outages |
| `HyperpingCoreMonitorDown` | critical | Core-tier monitor down for > 1 min |
| `HyperpingSSLExpiryWarning` | warning | SSL cert expiry < 14 days |
| `HyperpingSSLExpiryCritical` | critical | SSL cert expiry < 3 days |
| `HyperpingMonitorSLABreach24h` | warning | 24h SLA < 99% |
| `HyperpingMonitorSLABreach7d` | warning | 7d SLA < 99.5% |
| `HyperpingTenantSLADegraded` | critical | Fleet-wide 24h SLA < 95% |
| `HyperpingHealthcheckDown` | warning | Healthcheck missed for > 5 min |
| `HyperpingTenantHealthDegraded` | warning | Health score < 80 |
| `HyperpingTenantHealthCritical` | critical | Health score < 60 |
| `HyperpingExporterScrapeFailure` | warning | API unreachable for > 5 min |
| `HyperpingDataStale` | warning | Data age > 120s (2× cache TTL) |

---

## Relationship to terraform-provider-hyperping

This exporter uses the same Hyperping API client as [develeap/terraform-provider-hyperping](https://github.com/develeap/terraform-provider-hyperping), but the client code is fully vendored here — there is no runtime dependency on the provider. The exporter was originally developed inside PR #104 of the provider and extracted here to serve as an independent, reusable tool.

If you use both projects, a cleanup PR to remove the basic exporter from the provider repo is tracked separately.

---

## Troubleshooting

| Symptom | Cause | Fix |
|---------|-------|-----|
| `error: API key required` at startup | `HYPERPING_API_KEY` not set | Export the env var or use `--api-key` |
| `/readyz` returns 503 | No successful API scrape yet (only on first start; once the exporter has scraped successfully once, it stays ready through transient API failures) | Wait up to `--cache-ttl`; check logs for API errors |
| `hyperping_scrape_success 0` | API unreachable or auth failure | Check API key, network connectivity, and logs |
| `hyperping_data_age_seconds` rising | Circuit breaker open after repeated failures | Check Hyperping API status; wait 30s for circuit to recover |
| SSL metrics missing for a monitor | Monitor is not HTTP or SSL is not configured | Only HTTP/HTTPS monitors with SSL configured expose SSL days |
| Rate limit errors in logs | `--cache-ttl` too low | Increase cache-ttl to 60s or higher |

---

## Contributing

1. Fork the repository
2. Create a feature branch: `git checkout -b feature/my-feature`
3. Write tests first (RED-GREEN-REFACTOR)
4. Ensure all checks pass: `make test && make lint`
5. Submit a pull request

---

## License

MIT. See [LICENSE](LICENSE).
