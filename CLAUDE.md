# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
make build        # compile binary
make test         # run tests with race detector and coverage
make lint         # golangci-lint
make clean        # remove binary and coverage artifacts

make docker-build # build Docker image
make compose-up   # start full stack (exporter + Prometheus + Grafana)
make compose-down # stop full stack
```

Run a single test:
```bash
go test -run TestFunctionName ./internal/collector/
```

## Architecture

**Hyperping Prometheus exporter** — polls the Hyperping API and exposes monitor health, SLA, outages, and healthcheck metrics on port `9312/metrics`.

Two internal packages:

- **`internal/client/`** — Hyperping API client. Built with circuit breaker (`gobreaker`), retry logic, rate-limit handling, and error sanitization. Interfaces defined in `interface.go`; entity models in `models_*.go`.

- **`internal/collector/`** — Implements `prometheus.Collector`. Fetches data from the client, caches results with a configurable TTL (default 60s), and maps API entities to 29+ Prometheus metric descriptors covering monitor status, SLA, outages, healthchecks, and tenant-level aggregates.

**`main.go`** wires it together: flag/env parsing (`HYPERPING_API_KEY` required), HTTP server with `/healthz` and `/readyz` endpoints, Prometheus registry, and graceful shutdown.

Configuration is via CLI flags with `HYPERPING_*` env var fallback. Tests use `go-vcr` cassettes for recorded HTTP interactions.
