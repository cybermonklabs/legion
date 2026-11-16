package testtmp

import (
	"errors"
	"net"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// Each system's limit is named here rather than read from the machine that runs the test, so
// both are checked wherever it runs: a path that passes macOS's 104-byte sun_path is refused there
// though Linux takes it.
//
// packages/daemon/internal/testtmp keeps the same file. Change one and change the other.
func TestEachSystemTakesASocketPathAsLongAsItsSunPathLessTheTerminatingZero(t *testing.T) {
	for _, c := range []struct {
		goos    string
		longest int
	}{
		{"linux", 107},
		{"darwin", 103},
		// A system whose sun_path this package cites no header for takes macOS's, the smaller.
		{"freebsd", 103},
	} {
		if got := maxSocketPath(c.goos); got != c.longest {
			t.Errorf("on %s a unix socket path of up to %d bytes is taken, want %d", c.goos, got, c.longest)
		}
	}
}

// The limit of the system running the test is the one Go's own bind holds it to: a socket whose
// path has that many bytes listens, and on Linux and macOS, whose sun_path the limit is read from,
// one a byte longer is refused with `invalid argument`. Any other system takes a limit that may be
// shorter than its own, so only the first half holds there.
func TestTheLimitIsTheLongestPathThisSystemBinds(t *testing.T) {
	limit := maxSocketPath(runtime.GOOS)
	dir := Dir(t, strings.Repeat("x", 60))
	longest := filepath.Join(dir, strings.Repeat("s", limit-len(dir)-1))
	listener, err := net.Listen("unix", longest)
	if err != nil {
		t.Fatalf("a socket path of %d bytes, the limit on %s, does not listen: %v", len(longest), runtime.GOOS, err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close the listener at %s: %v", longest, err)
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return
	}
	past, err := net.Listen("unix", longest+"s")
	if err == nil {
		_ = past.Close()
	}
	if !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("a socket path of %d bytes, one past the limit on %s, answered %v, want invalid argument",
			len(longest)+1, runtime.GOOS, err)
	}
}
