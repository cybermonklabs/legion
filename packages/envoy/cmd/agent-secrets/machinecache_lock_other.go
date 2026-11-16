//go:build !unix

// packages/envoy/cmd/agent-secrets/machinecache_lock_other.go
//
// The machine cache pairs its key.pem and enrollment under a flock (machinecache_lock_unix.go).
// Where there is no flock the machine source refuses rather than register a session two commands
// could split between them; this client is released for Linux and macOS alone.
package main

import "errors"

func lockMachineCache(string, bool) (func(), error) {
	return nil, errors.New("the machine cache needs a flock to keep its key and enrollment paired, and this platform has none")
}
