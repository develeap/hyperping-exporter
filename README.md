# hyperping-exporter

A standalone [Prometheus](https://prometheus.io/) exporter for [Hyperping](https://hyperping.io/) monitoring. Exposes monitor status, healthchecks, SLA ratios, outage metrics, and tenant health scores as Prometheus gauges.

Extracted from [develeap/terraform-provider-hyperping](https://github.com/develeap/terraform-provider-hyperping) to serve as a standalone, reusable exporter. The same battle-tested API client (with circuit breaker, retry, and rate-limit handling) is embedded here with zero runtime dependency on the provider.

---

## Quick start

**Docker**

```bash
docker run -p 9312:9312 \
  -e HYPERPING_API_KEY=your_key \
  ghcr.io/develeap/hyperping-exporter:latest
```

**Binary download** — grab the latest release from the [Releases](https://github.com/develeap/hyperping-exporter/releases) page, then:

```bash
HYPERPING_API_KEY=your_key ./hyperping-exporter
```

**Install with Go**

```bash
go install github.com/develeap/hyperping-exporter@latest
HYPERPING_API_KEY=your_key hyperping-exporter
```

Metrics are served at `http://localhost:9312/metrics`.

---

## Configuration

All flags can also be set via environment variables.

| Flag | Env var | Default | Description |
|------|---------|---------|-------------|
| `--api-key` | `HYPERPING_API_KEY` | *(required)* | Hyperping API key |
| `--listen-address` | — | `:9312` | Address to listen on |
| `--metrics-path` | — | `/metrics` | Path to expose metrics on |
| `--cache-ttl` | — | `60s` | How often to refresh data from the API |
| `--log-level` | — | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `--log-format` | — | `text` | Log format: `text` or `json` |

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

When using the Docker Compose stack (`make compose-up`), dashboards are provisioned automatically and available at `http://localhost:3000` (admin / admin).

---

## Kubernetes deployment

Manifests are in `deploy/k8s/`:

```bash
# Create namespace and secret
kubectl create namespace monitoring
kubectl create secret generic hyperping-credentials \
  --from-literal=api-key=YOUR_KEY -n monitoring

# Deploy
kubectl apply -f deploy/k8s/
```

Files:
- `deployment.yaml` — single-replica Deployment with liveness/readiness probes
- `service.yaml` — ClusterIP Service on port 9312
- `servicemonitor.yaml` — Prometheus Operator `ServiceMonitor` for automatic scrape discovery

---

## Docker Compose full stack

Starts the exporter, Prometheus (with alert + recording rules), and Grafana:

```bash
HYPERPING_API_KEY=your_key make compose-up
```

| Service | URL |
|---------|-----|
| Exporter metrics | http://localhost:9312/metrics |
| Prometheus | http://localhost:9090 |
| Grafana | http://localhost:3000 (admin/admin) |

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

## Contributing

1. Fork the repository
2. Create a feature branch: `git checkout -b feature/my-feature`
3. Write tests first (RED-GREEN-REFACTOR)
4. Ensure all checks pass: `make test && make lint`
5. Submit a pull request

---

## License

MPL-2.0. See [LICENSE](LICENSE).
