// Package testtmp is a temporary directory for a test that binds a unix socket under it, short
// enough that the socket's path fits what the system holds. It is an ordinary package rather than a
// _test.go file because a command is package main and can import no other package's test helpers,
// the reason packages/envoy/internal/cmdtest gives for itself; the tests of cmd/cipher and
// cmd/cipher-helper bind sockets too.
//
// The daemon module keeps the same file at packages/daemon/internal/testtmp/testtmp.go. The two
// are copies, not one package imported twice, because Go's internal rule reads a path prefix:
// nothing outside packages/envoy may import github.com/sjawhar/envoy/internal/..., and nothing
// outside packages/daemon may import github.com/sjawhar/legion/daemon/internal/..., so neither
// module can reach the other's copy. Change one and change the other, except the paragraph of
// Dir's comment naming the sites that keep a directory of their own, which each module writes
// for itself.
package testtmp

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// maxSocketPath is the longest unix socket path goos takes: its sockaddr_un's sun_path less the
// terminating zero, so Go answers anything longer with `invalid argument` at bind, listen or dial
// (SockaddrUnix.sockaddr in the syscall package's syscall_linux.go and syscall_bsd.go), and a tmux
// server socket with `Filename too long` (docs/testing.md). Linux's sun_path holds 108 bytes
// (UNIX_PATH_MAX in the kernel's include/uapi/linux/un.h) and macOS's 104 (xnu's bsd/sys/un.h).
// Any other system gets macOS's 103, the smaller of the two. Dir asks for runtime.GOOS's; the test
// names each system itself, so it checks both limits on whatever machine runs it.
func maxSocketPath(goos string) int {
	if goos == "linux" {
		return 107
	}
	return 103
}

// prefix names every directory this package makes. It is two bytes because each byte it spends is
// one the caller's own path under the directory cannot have, and nothing has to recognise the
// directory later: the helper removes it itself when the test ends.
const prefix = "lg"

// fallbackBase is the shortest base a Linux machine has, where Dir makes its directory when the
// machine's own temporary directory is too long for the caller's path or holds no directory at
// all.
const fallbackBase = "/tmp"

// Dir is a fresh directory of the test's own, removed when the test ends, with room under it for
// longest: the longest path the test or the code under test makes inside it, written relative to
// the directory, such as "g9/.s.PGSQL.5432". A test that binds a unix socket under a temporary
// directory takes it from here rather than from t.TempDir(), whose path spells the whole of the
// test's name under TMPDIR and so passes the socket bound on a machine whose temporary directory
// is itself long (docs/testing.md).
//
// Dir measures the directory it made rather than predicting one, so a caller gets one of four
// answers:
//
//   - a TMPDIR that fits and takes a directory gives a directory under TMPDIR;
//   - a TMPDIR that fits but takes none, being unwritable, gives one under /tmp;
//   - a TMPDIR too long for longest gives one under /tmp;
//   - neither base usable fails the test here, naming both bases and what each answered, where
//     code that predicted its base instead returns a path that fails later at bind with
//     `invalid argument`.
//
// One kind of site keeps a directory of its own on purpose and does not come here: the
// once-per-binary directories a package's TestMain owns, the sign-in secret directory of
// packages/envoy/internal/cipher/config/config_test.go and the certificate directory of
// packages/envoy/infra/seed/main_test.go. They bind no socket, so they need no byte budget, and
// no *testing.T outlives them, so they keep os.MkdirTemp("", …) and a removal of their own. The
// daemon copy of this file names that module's sites.
func Dir(t testing.TB, longest string) string {
	t.Helper()
	limit := maxSocketPath(runtime.GOOS)
	base := os.TempDir()
	dir, size, err := attempt(t, base, longest)
	if err == nil && size <= limit {
		return dir
	}
	if err != nil {
		t.Logf("testtmp: %s holds no directory of this test's own (%v); falling back to %s", base, err, fallbackBase)
	} else {
		_ = os.RemoveAll(dir)
		t.Logf("testtmp: %q under %s is %d bytes, past the %d a unix socket path holds; falling back to %s",
			longest, base, size, limit, fallbackBase)
	}
	fallbackDir, fallbackSize, fallbackErr := attempt(t, fallbackBase, longest)
	if fallbackErr != nil || fallbackSize > limit {
		t.Fatalf("testtmp: no base leaves room for %q within the %d bytes a unix socket path holds: %s; %s",
			longest, limit, outcome(base, size, err), outcome(fallbackBase, fallbackSize, fallbackErr))
	}
	return fallbackDir
}

// Socket is the path of a socket named name in a directory of the test's own, for a caller that
// wants the socket path rather than the directory holding it: Dir of room for name, with name
// under it.
func Socket(t testing.TB, name string) string {
	t.Helper()
	return filepath.Join(Dir(t, name), name)
}

// attempt makes a fresh directory under base, removed when the test ends, and answers it with the
// length of longest's path under it. A base that holds no such directory gives the error instead.
func attempt(t testing.TB, base, longest string) (string, int, error) {
	t.Helper()
	dir, err := os.MkdirTemp(base, prefix)
	if err != nil {
		return "", 0, err
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir, len(filepath.Join(dir, longest)), nil
}

// outcome is what a base answered, for the failure a person reads: the length of the caller's path
// under the directory it made, or why it made none.
func outcome(base string, size int, err error) string {
	if err != nil {
		return fmt.Sprintf("%s made no directory (%v)", base, err)
	}
	return fmt.Sprintf("%s made one leaving a path of %d bytes", base, size)
}
