//go:build unix

package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// A process that forks a child holds each descriptor of the parent in the child too, until the
// child starts its program, and exec closes each descriptor that O_CLOEXEC marks. A test binary
// forks in one goroutine while print runs in another, and so can a program that calls print. The
// flock of print is on the open directory, and a copy of its descriptor in such a child shares
// that flock, so a release that only closes the descriptor leaves the lock held until the child
// starts its program, and the next run is refused. Each row holds a copy of one descriptor whose
// flock the run took, the directory's or the new directory's, as a child that has not started its
// program does, then releases the run: the next run on that directory takes the lock at once. The
// control is a release with no copy.
func TestReleaseEndsTheLockWhileACopyOfItsDescriptorIsOpen(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name string
		copy string // "", "dir" or "new": the descriptor that a child holds a copy of
	}{
		{"the control: no copy", ""},
		{"a copy of the descriptor of the directory", "dir"},
		{"a copy of the descriptor of the new directory", "new"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			dir, built := filepath.Join(root, "spec"), filepath.Join(root, "new")
			if err := os.Mkdir(built, 0o755); err != nil {
				t.Fatal(err)
			}
			lock, err := lockSpecDir(osFileSystem{}, dir, 0)
			if err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(built)
			if err != nil {
				t.Fatal(err)
			}
			lock.hold(f)
			if err := tryLock(f); err != nil {
				t.Fatal(err)
			}
			held := map[string]*os.File{"dir": lock.dir, "new": f}
			if c, ok := held[row.copy]; ok {
				copied, err := unix.Dup(int(c.Fd()))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = unix.Close(copied) })
			}
			lock.release(true)
			for _, next := range []string{dir, built} {
				again, err := lockSpecDir(osFileSystem{}, next, 0)
				if err != nil {
					t.Fatalf("the next run on %s, after the release: %v", next, err)
				}
				again.release(true)
			}
		})
	}
}
