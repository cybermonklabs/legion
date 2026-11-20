//go:build !unix

// packages/envoy/cmd/agent-secrets/machinecache_lock_other.go
//
// The machine cache pairs its key.pem and enrollment, and orders the writers of its audience, under
// a flock (machinecache_lock_unix.go). Where there is no flock the machine source refuses rather
// than keep state two commands could split between them; this client is released for Linux and
// macOS alone.
package main

import "errors"

func lockFile(string, bool, bool) (func(), error) {
	return nil, errNoFlock
}

var errNoFlock = errors.New("the machine cache needs a flock to keep its state whole, and this platform has none")
