// packages/envoy/cmd/cipher/identity_version_other.go
//go:build !linux

package main

import "github.com/sjawhar/envoy/internal/clock"

// helperVersionSuffix adds nothing where there is no helper: a machine session and its helper run
// on Linux only (the helper's peer pinning is Linux-only), so no non-Linux client has a helper
// line to name a version on.
func helperVersionSuffix(clock.Clock, string) string { return "" }
