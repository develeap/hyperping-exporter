# hyperping-exporter

[![CI](https://github.com/develeap/hyperping-exporter/actions/workflows/ci.yml/badge.svg)](https://github.com/develeap/hyperping-exporter/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/develeap/hyperping-exporter)](https://github.com/develeap/hyperping-exporter/releases/latest)
[![Go Report Card](https://goreportcard.com/badge/github.com/develeap/hyperping-exporter)](https://goreportcard.com/report/github.com/develeap/hyperping-exporter)
[![License: MIT](https://img.shields.io/badge/License-MIT-brightgreen.svg)](LICENSE)
[![Docker](https://img.shields.io/docker/v/khaledsalhabdeveleap/hyperping-exporter/latest?logo=docker&label=docker%20hub)](https://hub.docker.com/r/khaledsalhabdeveleap/hyperping-exporter)

> Get Hyperping monitor metrics into Prometheus in 30 seconds.

A standalone [Prometheus](https://prometheus.io/) exporter for [Hyperping](https://hyperping.io/) monitoring. Exposes monitor status, healthchecks, SLA ratios, outage metrics, and tenant health scores as Prometheus gauges.

Extracted from [develeap/terraform-provider-hyperping](https://github.com/develeap/terraform-provider-hyperping) to serve as a standalone, reusable exporter. The same battle-tested API client (with circuit breaker, retry, and rate-limit handling) is embedded here with zero runtime dependency on the provider.

Maintained by [Develeap](https://develeap.com).

---

## Quick start

> [!NOTE]
> **Get your API key first:** Log in to [Hyperping](https://hyperping.io) →
> Account Settings → API → Create API Key.

```bash
docker run -p 9312:9312 \
  -e HYPERPING_API_KEY=your_key \
  khaledsalhabdeveleap/hyperping-exporter:latest
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

**Helm chart** — published to a GitHub Pages helm repo on `chart-v*` tags.

```bash
helm repo add develeap https://develeap.github.io/hyperping-exporter
helm repo update
helm install hyperping-exporter develeap/hyperping-exporter \
  --version 1.5.2 \
  --set config.existingSecret=hyperping-api-key
```

See `deploy/helm/hyperping-exporter/values.yaml` for the full value reference and `CHANGELOG.md` for upgrade notes between chart versions.

</details>

Metrics are served at `http://localhost:9312/metrics`.

> [!WARNING]
> `/metrics` is unauthenticated by default and binds any-interface
> (`:9312`). The exposed payload includes monitor names, full URLs
> (query parameters stripped), tenant tags, and project UUIDs. Do
> not expose the listen port to the public internet without
> protection. Restrict the port to a trusted network (reverse proxy,
> k8s NetworkPolicy, host firewall) or enable basic-auth/TLS via
> `--web.config.file`. See `README.docker.md` for a one-line example
> and the [exporter-toolkit web-configuration docs](https://github.com/prometheus/exporter-toolkit/blob/master/docs/web-configuration.md).

---

## Configuration

All flags can also be set via environment variables.

| Flag | Env var | Default | Description |
|------|---------|---------|-------------|
| `--api-key-file` | `(flag only)` | *(none)* | Path to a file containing the Hyperping API key (one trailing newline is stripped). Recommended for shared hosts. |
| `--api-key` | `HYPERPING_API_KEY` | *(required)* | Hyperping API key. **`--api-key` is DEPRECATED**: the value is visible to any local user via `ps`, `/proc/<pid>/cmdline`, process accounting, and container introspection. Prefer the env var or `--api-key-file`. |
| `--listen-address` | `(flag only)` | `:9312` | Address to listen on |
| `--metrics-path` | `(flag only)` | `/metrics` | Path to expose metrics on |
| `--cache-ttl` | `(flag only)` | `60s` | How often to refresh data from the API |
| `--log-level` | `(flag only)` | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `--log-format` | `(flag only)` | `text` | Log format: `text` or `json` |
| `--namespace` | `HYPERPING_EXPORTER_NAMESPACE` | `hyperping` | Metric name prefix. Must match `[a-zA-Z_][a-zA-Z0-9_]{0,63}`. |
| `--mcp-url` | `(flag only)` | `(official)` | Custom Hyperping MCP server URL. |
| `--exclude-name-pattern` | `(flag only)` | *(none)* | RE2 regex; monitors whose name matches are dropped from all per-monitor metrics and tenant aggregates. Typical use: `'\[DRILL|\[TEST'` to keep synthetic monitors out of fleet health. |
| `--projects-file` | `HYPERPING_PROJECTS_FILE` | *(none)* | Path to a YAML list of `{id, apiKey\|apiKeyFile, mcpUrl?, excludeNamePattern?}` entries. Mutually exclusive with `--api-key` / `--api-key-file` / `HYPERPING_API_KEY`. Enables multi-project mode: every metric series carries a `project` constLabel set to the entry's `id`. See the [Multi-project deployments](#multi-project-deployments) section. |
| `--web.config.file` | `(flag only)` | *(none)* | Path to web config file for TLS / basic auth. See [exporter-toolkit web-configuration](https://github.com/prometheus/exporter-toolkit/blob/master/docs/web-configuration.md). |

> Only `HYPERPING_API_KEY` is read from the environment by default;
> all other options use CLI flags, which map cleanly to Docker `command:`
> entries. Use `HYPERPING_EXPORTER_NAMESPACE` for the namespace flag.

### Performance Tuning
| Concern | Recommendation |
|---------|---------------|
| **Freshness vs API load** | Default 60s TTL makes 5 REST parallel API calls + per-monitor MCP calls. For >100 monitors, keep TTL above 60s to avoid rate limits. |
| **Worker Pool** | MCP metrics use a worker pool of 10 concurrent requests to minimize latency while protecting the API. |
| **Cardinality** | Each monitor contributes ~13 time series. 100 monitors ≈ 1300 series — negligible for any Prometheus setup. |
| **Scrape interval** | Set Prometheus scrape interval ≥ cache-ttl. Scraping faster than the cache refreshes returns identical data. |

### Multi-project deployments

A single exporter Pod can serve multiple Hyperping projects (separate
accounts / API keys / billing scopes). Each project's metrics carry a
`project` constLabel set to the project's `id`, so series from
different projects coexist in one Prometheus tenant without name
collisions.

Single-project (legacy) deployments need no changes: the binary
synthesises a single `project="default"` entry from `--api-key` /
`HYPERPING_API_KEY` / `--api-key-file` and every metric series keeps
the same names as in chart 1.5.x.

To opt in, point `--projects-file` (or `HYPERPING_PROJECTS_FILE`) at a
YAML list:

```yaml
- id: hyp_core
  apiKeyFile: /etc/hyperping/api-key-hyp_core
  excludeNamePattern: '\[DRILL|\[TEST'  # per-project override
- id: hyp_infra
  apiKeyFile: /etc/hyperping/api-key-hyp_infra
```

Project ids must match `[a-zA-Z0-9._-]{1,64}` (same alphabet as the
tenant regex) and are globally unique within the file. Each entry sets
exactly one of `apiKey` (inline; dev only) or `apiKeyFile` (path to a
file containing the API key); the per-project `mcpUrl` and
`excludeNamePattern` fall back to the corresponding global flags when
empty.

The Helm chart's `config.projects` values knob renders the projects
file and the projected Secret volume that surfaces each project's API
key on disk; see `deploy/helm/hyperping-exporter/values.yaml` for the
full schema. Dashboards and alerts must be updated to add a
`project=<id>` (or `project=~<regex>`) selector during the rollout;
the dashboard migration in
[hyperping-automation](https://github.com/develeap/hyperping-automation)
tracks alongside this release.

> Set Grafana dashboard `$project` variable defaults to an explicit
> project id, **not** `All` or `.*`. The latter silently includes drill
> or staging series in headline numbers.

### Per-project cache tier overrides

When `--cache-mode=tiered` is set, the global `--hot-ttl` / `--warm-ttl`
/ `--cold-ttl` flags define the default refresh intervals applied to
every project. Some workloads want different settings per project: the
core production tenant may want HOT refreshes every 30s while a noisy
third-party tenant only needs SLA snapshots every few hours. The
projects file accepts an optional `cache:` block on any entry that
overrides any subset of the global tier knobs for that project:

```yaml
- id: hyp_core
  apiKeyFile: /etc/hyperping/api-key-hyp_core
  # No cache: block — inherits every global tier setting verbatim.

- id: hyp_infra
  apiKeyFile: /etc/hyperping/api-key-hyp_infra
  cache:
    warmTTL: "30m"  # Override only WARM; hot/cold inherit the globals.
    coldTTL: "2h"

- id: hyp_thirdparty
  apiKeyFile: /etc/hyperping/api-key-hyp_thirdparty
  cache:
    warmEnabled: false   # Skip WARM ticker entirely on this project.
    coldEnabled: false   # Skip COLD ticker entirely on this project.
```

Fields are all optional; absent keys inherit the corresponding global.
TTL values are Go duration strings (`60s`, `30m`, `2h`); enable flags
are bools.

Semantics:

- `<tier>TTL`: overrides the matching global TTL for this project only.
- `<tier>Enabled: false`: skips that tier's ticker for this project.
  No API calls land for that tier, and no series for that tier appear
  on the project's `/metrics` output. The other tiers and other
  projects are untouched.
- `hotEnabled: false` is rejected at startup: HOT carries every
  per-monitor up/down series and is the readiness gate, so disabling
  it produces a Pod that never readies and never scrapes.
- `<tier>TTL` set alongside `<tier>Enabled: false` is accepted with a
  startup warning. The enable flag wins; the TTL is ignored. Operators
  using config-merging tooling (overlays, patches) can land in this
  state during a rollout; we surface it instead of failing the boot.

At startup the binary emits one structured log line per project
showing the effective tier configuration after override resolution:

```
level=INFO msg="effective tier configuration" project=hyp_infra hot_ttl=1m0s warm_ttl=30m0s cold_ttl=2h0m0s hot_enabled=true warm_enabled=true cold_enabled=true
```

For dashboards, a new self-metric distinguishes "tier disabled by
operator" from "tier broken / no data":

```
hyperping_exporter_tier_disabled{project="hyp_thirdparty",tier="warm"} 1
hyperping_exporter_tier_disabled{project="hyp_thirdparty",tier="cold"} 1
```

The series is absence-based: only disabled (project, tier) pairs emit
a sample. A no-override config produces zero series here, and an
upgrade with no `cache:` blocks anywhere adds zero new series to the
registry.

A config with no `cache:` blocks anywhere produces byte-identical
behaviour to chart 1.6.x: same effective TTLs, same API call volume,
same series. The Helm chart's `config.projects[*].cache` knob passes
the block through to the rendered projects file verbatim; see
`deploy/helm/hyperping-exporter/values.yaml` for the schema.

---

## Available metrics

### Monitor metrics

| Metric | Type | Description | Labels |
|--------|------|-------------|--------|
| `hyperping_monitor_up` | Gauge | 1 if the monitor is up, 0 if down. | `uuid`, `name`, `tenant`, `tier` |
| `hyperping_monitor_paused` | Gauge | 1 if paused, 0 if active. | `uuid`, `name`, `tenant`, `tier` |
| `hyperping_monitor_check_interval_seconds` | Gauge | Frequency of checks. | `uuid`, `name`, `tenant`, `tier` |
| `hyperping_monitor_info` | Gauge | Metadata about the monitor. | `uuid`, `name`, `tenant`, `tier`, `url`, `protocol`, `method` |
| `hyperping_monitor_ssl_expiration_days` | Gauge | Days until SSL cert expires. | `uuid`, `name`, `tenant`, `tier` |
| `hyperping_monitor_outage_active` | Gauge | 1 if currently in outage. | `uuid`, `name`, `tenant`, `tier` |
| `hyperping_monitor_active_outage_status_code` | Gauge | HTTP status code of active outage. | `uuid`, `name`, `tenant`, `tier` |
| `hyperping_monitor_escalation_tier` | Gauge | 1 (info only). | `uuid`, `name`, `tier` |
| `hyperping_monitor_in_maintenance` | Gauge | 1 if in a maintenance window. | `uuid`, `name`, `tenant`, `tier` |
| `hyperping_monitor_up_by_region` | Gauge | 1 if up in region, 0 if down. | `uuid`, `name`, `tenant`, `tier`, `region` |
| `hyperping_monitor_response_time_seconds` | Gauge | Average response time via MCP. | `uuid`, `name`, `tenant`, `tier` |
| `hyperping_monitor_mtta_seconds` | Gauge | Mean Time To Acknowledge via MCP. | `uuid`, `name`, `tenant`, `tier` |
| `hyperping_monitor_anomaly_count` | Gauge | Detected anomalies count via MCP. | `uuid`, `name`, `tenant`, `tier` |
| `hyperping_monitor_anomaly_score` | Gauge | Highest anomaly score via MCP. | `uuid`, `name`, `tenant`, `tier` |
| `hyperping_alerts` | Gauge | Recent alert snapshot count via MCP. | `uuid`, `name` |
| `hyperping_monitor_sla_ratio` | Gauge | Monitor SLA (0–1). | `uuid`, `name`, `tenant`, `tier`, `period` |
| `hyperping_monitor_outages` | Gauge | Count of outages in period. | `uuid`, `name`, `tenant`, `tier`, `period` |
| `hyperping_monitor_downtime` | Gauge | Total downtime seconds in period. | `uuid`, `name`, `tenant`, `tier`, `period` |
| `hyperping_monitor_longest_outage` | Gauge | Longest outage seconds in period. | `uuid`, `name`, `tenant`, `tier`, `period` |
| `hyperping_monitor_mttr` | Gauge | Mean Time To Resolve in period. | `uuid`, `name`, `tenant`, `tier`, `period` |

### Healthcheck metrics

| Metric | Type | Description | Labels |
|--------|------|-------------|--------|
| `hyperping_healthcheck_up` | Gauge | 1 if healthcheck is up. | `uuid`, `name` |
| `hyperping_healthcheck_paused` | Gauge | 1 if paused. | `uuid`, `name` |
| `hyperping_healthcheck_period_seconds` | Gauge | Expected ping interval. | `uuid`, `name` |

### Tenant & Global metrics

| Metric | Type | Description | Labels |
|--------|------|-------------|--------|
| `hyperping_tenant_monitors_up_ratio` | Gauge | Fraction of monitors up (0–1). | |
| `hyperping_tenant_active_outages` | Gauge | Total active outages. | |
| `hyperping_tenant_avg_sla_ratio` | Gauge | Average SLA ratio across visible monitors for the labelled period. | `period` |
| `hyperping_tenant_health_score` | Gauge | Composite health score (0–100). | |
| `hyperping_incidents_open` | Gauge | Count of open incidents. | |
| `hyperping_maintenance_windows_active` | Gauge | Active maintenance windows. | |
| `hyperping_incident_active` | Gauge | 1 per active incident. | `tenant`, `tier`, `severity` |
| `hyperping_maintenance_active` | Gauge | 1 per active maintenance window. | `tenant`, `tier`, `severity` |
| `hyperping_monitors` | Gauge | Visible monitors (post `--exclude-name-pattern`). | |
| `hyperping_excluded_monitors` | Gauge | Monitors filtered out by `--exclude-name-pattern` (0 when no pattern set). | |
| `hyperping_healthchecks` | Gauge | Total healthchecks discovered. | |
| `hyperping_scrape_success` | Gauge | 1 if last API scrape succeeded. | |
| `hyperping_scrape_duration_seconds` | Gauge | Duration of last scrape. | |
| `hyperping_data_age_seconds` | Gauge | Seconds since last successful scrape. | |
| `hyperping_cache_ttl_seconds` | Gauge | Cache refresh interval as configured via `--cache-ttl`. Used by `HyperpingDataStale` for self-configuring thresholds. | |

---

## Docker Compose full stack

### Exporter only (bring-your-own Prometheus)

If you already run Prometheus, add just the exporter:

```yaml
services:
  hyperping-exporter:
    image: khaledsalhabdeveleap/hyperping-exporter:latest
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
# One-time setup: copy the example and fill in your credentials
cp deploy/.env.example deploy/.env
$EDITOR deploy/.env

# Then start the stack
make compose-up
```

`deploy/.env` is gitignored — never commit it. `GRAFANA_ADMIN_PASSWORD` is required; the stack will fail loudly if it is not set. All services bind to `127.0.0.1` only for local dev safety.

---

## Prometheus alerting rules

Pre-configured rules in `deploy/prometheus/alerts.yml`:

| Alert | Severity | Condition |
|-------|----------|-----------|
| `HyperpingMonitorDown` | critical | Monitor down for > 2 min (suppressed during active maintenance) |
| `HyperpingMonitorActiveOutage` | critical | Active outage for > 1 min (suppressed during active maintenance) |
| `HyperpingMultipleActiveOutages` | critical | > 10% of monitors with concurrent active outages |
| `HyperpingCoreMonitorDown` | critical | Core-tier monitor down for > 1 min (suppressed during active maintenance) |
| `HyperpingNoMonitors` | warning | Scrape succeeds but API returns zero monitors for > 10 min |
| `HyperpingOpenIncidents` | warning | Unresolved Hyperping incidents > 0 for 30 min |
| `HyperpingMonitorRegionalOutage` | warning | Monitor down in some regions but up in others (partial regional failure) for > 5 min |
| `HyperpingSSLExpiryWarning` | warning | SSL cert expiry < 14 days |
| `HyperpingSSLExpiryCritical` | critical | SSL cert expiry < 3 days |
| `HyperpingMonitorSLABreach24h` | warning | 24h SLA < 99% |
| `HyperpingMonitorSLABreach7d` | warning | 7d SLA < 99.5% |
| `HyperpingTenantSLADegraded` | critical | Fleet-wide 24h SLA < 95% |
| `HyperpingHealthcheckDown` | warning | Healthcheck missed for > 5 min |
| `HyperpingTenantHealthDegraded` | warning | Health score < 80 |
| `HyperpingTenantHealthCritical` | critical | Health score < 60 |
| `HyperpingMonitorAnomalyHigh` | warning | MCP anomaly score > 0.8 for > 15 min (requires `--mcp-url`) |
| `HyperpingMonitorMTTAHigh` | warning | MCP MTTA > 10 min for 1 hr (requires `--mcp-url`) |
| `HyperpingExporterScrapeFailure` | warning | API unreachable for > 5 min |
| `HyperpingDataStale` | warning | Data age > 2× `hyperping_cache_ttl_seconds` (auto-adapts to `--cache-ttl`) |

---

## Relationship to terraform-provider-hyperping

This exporter shares the same API client as [develeap/terraform-provider-hyperping](https://github.com/develeap/terraform-provider-hyperping) via the [`github.com/develeap/hyperping-go`](https://github.com/develeap/hyperping-go) module. There is no runtime dependency on the provider.

---

## Troubleshooting

| Symptom | Cause | Fix |
|---------|-------|-----|
| `error: API key required` at startup | `HYPERPING_API_KEY` not set | Export the env var or use `--api-key` |
| `/readyz` returns 503 | No successful API scrape yet | Wait up to `--cache-ttl`; check logs |
| `hyperping_scrape_success 0` | API unreachable or auth failure | Check API key, network, and logs |
| `hyperping_data_age_seconds` rising | Circuit breaker open | Check Hyperping API status; wait 30s |
| Rate limit errors in logs | `--cache-ttl` too low | Increase cache-ttl to 60s or higher |

---

## License

MIT. See [LICENSE](LICENSE).
