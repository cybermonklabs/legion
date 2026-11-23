//go:build unix && !linux

// packages/envoy/cmd/cipher/secret_gcp_calls_other_test.go
//
// The unix platforms other than Linux have no pidfd: the rows that time gcloud calls take the
// call's wait for the end of the process it started (recordGcloudCalls).
package main

import (
	"errors"
	"time"
)

// errNoExitWatch is watchProcessExit's answer where this platform has no way to watch a process
// end, as here.
var errNoExitWatch = errors.New("this platform has no watch of a process's end")

// watchProcessExit cannot watch a process end here: it answers errNoExitWatch.
func watchProcessExit(int) (exited func() (time.Time, error), stop func()) {
	return func() (time.Time, error) { return time.Time{}, errNoExitWatch }, func() {}
}
