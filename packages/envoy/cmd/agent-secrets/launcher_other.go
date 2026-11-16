// packages/envoy/cmd/agent-secrets/launcher_other.go
//go:build !linux

// On a platform with no /proc, no session launcher can be named, so `register --session-id`
// without `--exec` refuses rather than pinning its own process (dispatch://INFRA-3890). The
// machine sessions this matters for run on Linux; the helper's own peer pinning is Linux-only too.
package main

import "errors"

var errNoLauncher = errors.New("no session launcher is an ancestor of this process")

var nearestLauncher = func() (int, error) {
	return 0, errNoLauncher
}
