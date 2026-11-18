// packages/envoy/cmd/agent-secrets/identity_version_other.go
//go:build !linux

package main

// helperVersionSuffix adds nothing where there is no helper: a machine session and its helper run
// on Linux only (the helper's peer pinning is Linux-only), so no non-Linux client has a helper
// line to name a version on.
func helperVersionSuffix(string) string { return "" }
