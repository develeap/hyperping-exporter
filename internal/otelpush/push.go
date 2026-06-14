// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

// Package otelpush drives periodic OTLP metric export from one or more
// Hyperping Collectors. It wraps the OTel Go SDK periodic reader and
// registers async gauge callbacks that read from the Collector's cached
// snapshot on every export tick.
package otelpush

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/develeap/hyperping-exporter/internal/collector"
)

// Config holds the OTLP push configuration.
type Config struct {
	// Endpoint is the gRPC or HTTP endpoint of the OTel collector.
	// Example: "localhost:4317" (gRPC) or "localhost:4318" (HTTP).
	Endpoint string
	// Protocol selects the transport: "grpc" (default) or "http".
	// Case-insensitive; any other value is rejected by NewPusher.
	Protocol string
	// Headers are key=value pairs appended to every export request.
	// Used for auth tokens (e.g. "Authorization=Bearer <token>").
	Headers map[string]string
	// Interval is the push cadence. Zero falls back to 60 seconds.
	Interval time.Duration
	// Insecure disables TLS verification. Intended for dev/localhost only.
	Insecure bool
	// Namespace is the metric name prefix, e.g. "hyperping". It is used as
	// both the OTel instrumentation scope name and the metric name prefix.
	Namespace string
}

// CollectorSource is the interface the bridge uses to read per-collector
// snapshot data. *collector.Collector satisfies it after WI-3.
// It is exported so tests can provide their own implementation.
type CollectorSource interface {
	TakeSnapshot() collector.Snapshot
	Project() string
}

// Pusher drives periodic OTLP metric export. Create it with NewPusher and
// call Shutdown when the process exits.
type Pusher struct {
	provider *sdkmetric.MeterProvider
}

// NewPusher creates a MeterProvider backed by an OTLP exporter and registers
// async gauge callbacks that read from the supplied collectors on every
// export tick.
//
// cfg.Protocol must be "grpc" or "http" (case-insensitive); any other value
// returns an error. The returned Pusher must be shut down via Shutdown.
func NewPusher(ctx context.Context, cfg Config, colls []*collector.Collector) (*Pusher, error) {
	exp, err := newExporter(ctx, cfg)
	if err != nil {
		return nil, err
	}

	interval := cfg.Interval
	if interval <= 0 {
		interval = 60 * time.Second
	}

	reader := sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(interval))
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	sources := make([]CollectorSource, len(colls))
	for i, c := range colls {
		sources[i] = c
	}

	if err := RegisterBridgeCallbacks(provider, sources, cfg.Namespace); err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = provider.Shutdown(shutdownCtx)
		return nil, fmt.Errorf("register bridge callbacks: %w", err)
	}

	return &Pusher{provider: provider}, nil
}

// Shutdown flushes buffered metrics and closes the underlying MeterProvider.
// Call it during graceful shutdown before the process exits.
func (p *Pusher) Shutdown(ctx context.Context) error {
	return p.provider.Shutdown(ctx)
}

// newExporter constructs the appropriate OTLP exporter based on cfg.Protocol.
func newExporter(ctx context.Context, cfg Config) (sdkmetric.Exporter, error) {
	switch strings.ToLower(cfg.Protocol) {
	case "grpc":
		return newGRPCExporter(ctx, cfg)
	case "http":
		return newHTTPExporter(ctx, cfg)
	default:
		return nil, fmt.Errorf("unsupported OTLP protocol %q: must be \"grpc\" or \"http\"", cfg.Protocol)
	}
}

func newGRPCExporter(ctx context.Context, cfg Config) (sdkmetric.Exporter, error) {
	opts := []otlpmetricgrpc.Option{
		otlpmetricgrpc.WithEndpoint(cfg.Endpoint),
	}
	if cfg.Insecure {
		opts = append(opts, otlpmetricgrpc.WithInsecure())
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlpmetricgrpc.WithHeaders(cfg.Headers))
	}
	return otlpmetricgrpc.New(ctx, opts...)
}

func newHTTPExporter(ctx context.Context, cfg Config) (sdkmetric.Exporter, error) {
	opts := []otlpmetrichttp.Option{
		otlpmetrichttp.WithEndpoint(cfg.Endpoint),
	}
	if cfg.Insecure {
		opts = append(opts, otlpmetrichttp.WithInsecure())
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlpmetrichttp.WithHeaders(cfg.Headers))
	}
	return otlpmetrichttp.New(ctx, opts...)
}
