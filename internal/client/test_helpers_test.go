// Copyright (c) 2026 Develeap
// SPDX-License-Identifier: MIT

package client

// Package-level test pointer helpers.

func boolPtr(b bool) *bool       { return &b }
func strPtr(s string) *string    { return &s }
func stringPtr(s string) *string { return &s }
func intPtr(i int) *int          { return &i }
