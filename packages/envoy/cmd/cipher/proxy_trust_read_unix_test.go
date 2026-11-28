//go:build unix

// packages/envoy/cmd/cipher/proxy_trust_read_unix_test.go
//
// readTrustSource retries its open and its fstat when a signal interrupts them (EINTR), as the os
// package does. A real EINTR needs a slow file system and a signal that lands inside the call, so
// the two system calls are replaced with ones that answer EINTR first.
package main

import (
	"syscall"
	"testing"
)

// TestReadTrustSourceRetriesAnInterruptedOpenAndFstat: an open and an fstat that each answer EINTR
// twice before they succeed still read the file whole, after three calls each.
func TestReadTrustSourceRetriesAnInterruptedOpenAndFstat(t *testing.T) {
	path := writeLoginFile(t, "trust.pem", "the file's trust\n")
	oldOpen, oldFstat := trustSourceOpen, trustSourceFstat
	t.Cleanup(func() { trustSourceOpen, trustSourceFstat = oldOpen, oldFstat })
	opens, fstats := 0, 0
	trustSourceOpen = func(path string, mode int, perm uint32) (int, error) {
		if opens++; opens <= 2 {
			return -1, syscall.EINTR
		}
		return syscall.Open(path, mode, perm)
	}
	trustSourceFstat = func(fd int, st *syscall.Stat_t) error {
		if fstats++; fstats <= 2 {
			return syscall.EINTR
		}
		return syscall.Fstat(fd, st)
	}

	data, err := readTrustSource(path)
	if err != nil {
		t.Fatalf("readTrustSource after two interrupted opens and fstats: %v", err)
	}
	if string(data) != "the file's trust\n" {
		t.Errorf("readTrustSource read %q, want the whole file", data)
	}
	if opens != 3 || fstats != 3 {
		t.Errorf("readTrustSource made %d opens and %d fstats, want 3 of each", opens, fstats)
	}
}
