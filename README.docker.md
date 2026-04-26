# hyperping-exporter

A standalone [Prometheus](https://prometheus.io/) exporter for [Hyperping](https://hyperping.io/) monitoring.
Exposes monitor status, SLA ratios, outage metrics, healthchecks, and tenant health scores as Prometheus gauges.

Built and maintained by [Develeap](https://develeap.com).

---

## Quick start

Get your API key: **Hyperping** → Account Settings → API → Create API Key.

```bash
docker run -p 9312:9312 \
  -e HYPERPING_API_KEY=your_key \
  khaledsalhabdeveleap/hyperping-exporter:latest
```

```bash
curl -s http://localhost:9312/metrics | grep hyperping_monitor_up
```

Metrics are served at `http://localhost:9312/metrics`.

---

## Configuration

| Flag | Env var | Default | Description |
|------|---------|---------|-------------|
| `--api-key` | `HYPERPING_API_KEY` | *(required)* | Hyperping API key |
| `--listen-address` | — | `:9312` | Address to listen on |
| `--cache-ttl` | — | `60s` | How often to refresh data from the API |
| `--log-level` | — | `info` | `debug`, `info`, `warn`, `error` |
| `--log-format` | — | `text` | `text` or `json` |
| `--namespace` | `HYPERPING_EXPORTER_NAMESPACE` | `hyperping` | Metric name prefix |

Example with flags:

```bash
docker run -p 9312:9312 \
  -e HYPERPING_API_KEY=your_key \
  khaledsalhabdeveleap/hyperping-exporter:latest \
  --cache-ttl=30s \
  --log-format=json
```

---

## Docker Compose (full stack)

```yaml
services:
  hyperping-exporter:
    image: khaledsalhabdeveleap/hyperping-exporter:latest
    environment:
      HYPERPING_API_KEY: "${HYPERPING_API_KEY}"
    ports:
      - "127.0.0.1:9312:9312"
    restart: unless-stopped

  prometheus:
    image: prom/prometheus:latest
    volumes:
      - ./prometheus.yml:/etc/prometheus/prometheus.yml:ro
    ports:
      - "127.0.0.1:9090:9090"
```

`prometheus.yml` scrape config:

```yaml
scrape_configs:
  - job_name: hyperping
    static_configs:
      - targets: ['hyperping-exporter:9312']
```

---

## Endpoints

| Endpoint | Description |
|----------|-------------|
| `/metrics` | Prometheus metrics |
| `/healthz` | Liveness probe — always `ok` if the process is running |
| `/readyz` | Readiness probe — `ok` after the first successful API scrape |

---

## Available tags

| Tag | Description |
|-----|-------------|
| `latest` | Latest stable release (currently `1.3.1`) |
| `1.3.1`, `1.3.0`, ... | Pinned version |
| `v1` | Latest 1.x release |

Multi-arch: `linux/amd64` and `linux/arm64`.

---

## Links

- **GitHub**: https://github.com/develeap/hyperping-exporter
- **Releases & changelogs**: https://github.com/develeap/hyperping-exporter/releases
- **Hyperping**: https://hyperping.io
