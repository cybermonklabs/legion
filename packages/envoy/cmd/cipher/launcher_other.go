// packages/envoy/cmd/cipher/launcher_other.go
//go:build !linux

// On a platform with no /proc, no session launcher can be named and no root a helper answers can
// be checked against this process's ancestry, so `register --session-id` without `--exec`
// refuses rather than print `enrolled` for a root it cannot place. The
// machine sessions this matters for run on Linux; the helper's own peer pinning is Linux-only too.
package main

import "errors"

var errNoLauncher = errors.New("no session launcher is an ancestor of this process")

var nearestLauncher = func() (int, error) {
	return 0, errNoLauncher
}

func isAncestor(int) (bool, error) {
	return false, errors.New("this platform has no /proc to read this process's ancestry from")
}

// nearestHarness answers "" where there is no /proc to walk: requestSessionID then keeps the
// order it had before Codex was a host.
var nearestHarness = func(...string) string { return "" }
