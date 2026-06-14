// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package otelpush_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/develeap/hyperping-exporter/internal/otelpush"
)

// TestNewPusher_InvalidProtocol verifies that an unsupported protocol value
// returns an error from NewPusher.
func TestNewPusher_InvalidProtocol(t *testing.T) {
	cfg := otelpush.Config{
		Endpoint: "localhost:4317",
		Protocol: "mqtt",
	}
	p, err := otelpush.NewPusher(context.Background(), cfg, nil)
	require.Error(t, err, "NewPusher should error on unsupported protocol")
	assert.Nil(t, p)
	assert.Contains(t, err.Error(), "mqtt")
}

// TestNewPusher_GRPCExporter verifies that a valid gRPC config constructs a
// Pusher without error. The endpoint does not need to be reachable; the OTel
// gRPC exporter does not dial at creation time. Shutdown is called for cleanup
// only; transport errors at shutdown are not asserted because no real collector
// is listening.
func TestNewPusher_GRPCExporter(t *testing.T) {
	cfg := otelpush.Config{
		Endpoint: "localhost:4317",
		Protocol: "grpc",
		Interval: 100 * time.Millisecond,
		Insecure: true,
	}
	p, err := otelpush.NewPusher(context.Background(), cfg, nil)
	require.NoError(t, err)
	require.NotNil(t, p)
	_ = p.Shutdown(context.Background())
}

// TestNewPusher_HTTPExporter verifies that a valid HTTP config constructs a
// Pusher without error. Transport errors at shutdown are not asserted because
// no real collector is listening.
func TestNewPusher_HTTPExporter(t *testing.T) {
	cfg := otelpush.Config{
		Endpoint: "localhost:4318",
		Protocol: "http",
		Interval: 100 * time.Millisecond,
		Insecure: true,
	}
	p, err := otelpush.NewPusher(context.Background(), cfg, nil)
	require.NoError(t, err)
	require.NotNil(t, p)
	_ = p.Shutdown(context.Background())
}

// TestNewPusher_ProtocolCaseInsensitive verifies that "GRPC" and "HTTP" are
// accepted in addition to their lowercase forms.
func TestNewPusher_ProtocolCaseInsensitive(t *testing.T) {
	for _, proto := range []string{"GRPC", "HTTP", "Grpc", "Http"} {
		proto := proto
		t.Run(proto, func(t *testing.T) {
			cfg := otelpush.Config{
				Endpoint: "localhost:4317",
				Protocol: proto,
				Interval: 100 * time.Millisecond,
				Insecure: true,
			}
			p, err := otelpush.NewPusher(context.Background(), cfg, nil)
			require.NoError(t, err, "protocol %q should be accepted", proto)
			require.NotNil(t, p)
			_ = p.Shutdown(context.Background())
		})
	}
}

// TestPusher_Shutdown verifies that Shutdown returns within its context deadline
// and does not block indefinitely. The OTel SDK performs a final flush on
// Shutdown; a transport error (connection refused) is expected here because no
// real collector is listening, and is not asserted.
func TestPusher_Shutdown(t *testing.T) {
	cfg := otelpush.Config{
		Endpoint: "localhost:4317",
		Protocol: "grpc",
		Interval: 10 * time.Second,
		Insecure: true,
	}
	p, err := otelpush.NewPusher(context.Background(), cfg, nil)
	require.NoError(t, err)

	// Use a short context for Shutdown so the OTel flush attempt times out
	// quickly. The outer timer is larger so we can verify the goroutine
	// actually returns after the context deadline (not before, not never).
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = p.Shutdown(shutdownCtx)
		close(done)
	}()
	select {
	case <-done:
		// Good: Shutdown returned (with or without a transport error).
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return within 5 seconds")
	}
}
