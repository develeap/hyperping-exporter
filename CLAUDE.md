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

One internal package:

- **`internal/collector/`** — Implements `prometheus.Collector`. Fetches data via `github.com/develeap/hyperping-go` (REST client + MCP client), caches results with a configurable TTL (default 60s), and maps API entities to 35+ Prometheus metric descriptors covering monitor status, SLA, outages, healthchecks, MCP response-time/MTTA/anomaly metrics, and tenant-level aggregates.

The REST and MCP calls run in parallel on each cache refresh. MCP metrics use a worker pool of 10 concurrent goroutines with an 8-second per-call timeout; MCP failure is non-fatal (REST metrics remain available).

**`main.go`** wires it together: flag/env parsing (`HYPERPING_API_KEY` required, `--mcp-url` optional), HTTP server with `/healthz` and `/readyz` endpoints, Prometheus registry, and graceful shutdown.

Configuration is via CLI flags with `HYPERPING_*` env var fallback. Tests use `go-vcr` cassettes for recorded HTTP interactions.
