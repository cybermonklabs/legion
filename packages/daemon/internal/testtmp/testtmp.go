// Package testtmp is a temporary directory for a test that binds a unix socket under it, short
// enough that the socket's path fits what Linux holds. It is an ordinary package rather than a
// _test.go file because a command is package main and can import no other package's test helpers,
// the reason packages/envoy/internal/cmdtest gives for itself; cmd/legion's own tests bind sockets
// too.
//
// The envoy module keeps the same file at packages/envoy/internal/testtmp/testtmp.go. The two are
// copies, not one package imported twice, because Go's internal rule reads a path prefix: nothing
// outside packages/envoy may import github.com/sjawhar/envoy/internal/..., and nothing outside
// packages/daemon may import github.com/sjawhar/legion/daemon/internal/..., so neither module can
// reach the other's copy. Change one and change the other, except the paragraph of Dir's comment
// naming the sites that keep a directory of their own, which each module writes for itself.
//
// Without is the other directory it gives: one whose path holds none of the characters a test's
// tool cannot take in a path, for a machine whose TMPDIR holds one.
package testtmp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// maxSocketPath is the longest unix socket path Linux takes. Its sun_path holds 108 bytes with the
// terminating zero, so Go answers anything longer with `invalid argument` at bind, listen or dial,
// and a tmux server socket with `Filename too long` (docs/testing.md).
const maxSocketPath = 107

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
// Two kinds of site keep a directory of their own on purpose and do not come here:
//
//   - packages/daemon/internal/shim/socketpath_test.go's roleTempDir, which builds a deliberately
//     long directory, as long as the TMPDIR a Legion role is told, so that its test proves sockets
//     still bind where a role runs. A short directory would take the proof away.
//   - the once-per-binary directories a package's TestMain owns: the build directories of
//     packages/daemon/internal/daemon/idle_rig_listener_test.go,
//     packages/daemon/internal/stream/integration_test.go and
//     packages/daemon/internal/runtime/tmux/tmux_real_test.go, the jj configuration directory of
//     packages/daemon/cmd/legion/main_test.go and the home of
//     packages/daemon/internal/workspace/workspace_test.go. They bind no socket, so they need no
//     byte budget, and no *testing.T outlives them, so they keep os.MkdirTemp("", …) and a
//     removal of their own.
func Dir(t testing.TB, longest string) string {
	t.Helper()
	base := os.TempDir()
	dir, size, err := attempt(t, base, longest)
	if err == nil && size <= maxSocketPath {
		return dir
	}
	if err != nil {
		t.Logf("testtmp: %s holds no directory of this test's own (%v); falling back to %s", base, err, fallbackBase)
	} else {
		_ = os.RemoveAll(dir)
		t.Logf("testtmp: %q under %s is %d bytes, past the %d a unix socket path holds; falling back to %s",
			longest, base, size, maxSocketPath, fallbackBase)
	}
	fallbackDir, fallbackSize, fallbackErr := attempt(t, fallbackBase, longest)
	if fallbackErr != nil || fallbackSize > maxSocketPath {
		t.Fatalf("testtmp: no base leaves room for %q within the %d bytes a unix socket path holds: %s; %s",
			longest, maxSocketPath, outcome(base, size, err), outcome(fallbackBase, fallbackSize, fallbackErr))
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

// Without is Dir for a test whose tool also cannot take some characters in a path: a fresh
// directory of the test's own, removed when the test ends, whose path holds none of chars and has
// room for longest as Dir measures it ("" for a test that binds no socket there). Bun, which Oh My
// Pi runs on, loads no module from a path holding a '?'; agent-secrets-helper install refuses to
// write a unit whose ExecStart or Environment= line would need escaping, a space or a quote among
// them; vm-sample splits its -ssh command on spaces. The directory is made under the first base
// that holds none of chars, takes a directory and fits longest: os.TempDir(); then its closest
// ancestor below the root that holds none of chars, which is most often on its filesystem; then
// the module's own .cache/testtmp, which .gitignore leaves out; then /tmp. With none, the test
// fails here, naming what each base answered; it never skips.
func Without(t testing.TB, longest, chars string) string {
	t.Helper()
	return WithoutAccepting(t, longest, chars, nil)
}

// WithoutAccepting is Without whose directory must also pass accept, which gets the directory it
// made and answers why that directory will not do, or nil: a test whose tool must also reach the
// directory some other way, such as a hardlink from a cache on one filesystem, says so there, and
// a base whose directory fails it is passed over for the next like any other.
func WithoutAccepting(t testing.TB, longest, chars string, accept func(dir string) error) string {
	t.Helper()
	bases, noted := []string{os.TempDir()}, []string(nil)
	if ancestor := cleanAncestor(os.TempDir(), chars); ancestor != "" {
		bases = append(bases, ancestor)
	}
	if scratch, err := moduleScratch(); err == nil {
		bases = append(bases, scratch)
	} else {
		noted = append(noted, "the module's .cache/testtmp is unknown ("+err.Error()+")")
	}
	return without(t, longest, chars, accept, noted, append(bases, fallbackBase)...)
}

// cleanAncestor is the closest directory above dir, below the root, whose path holds none of
// chars; "" when dir holds none itself, or when only the root would.
func cleanAncestor(dir, chars string) string {
	if !strings.ContainsAny(dir, chars) {
		return ""
	}
	for parent := filepath.Dir(dir); parent != filepath.Dir(parent); parent = filepath.Dir(parent) {
		if !strings.ContainsAny(parent, chars) {
			return parent
		}
	}
	return ""
}

// without is WithoutAccepting over the bases it is given, in order, after the answers already
// noted.
func without(t testing.TB, longest, chars string, accept func(string) error, noted []string, bases ...string) string {
	t.Helper()
	answers := noted
	for _, base := range bases {
		if strings.ContainsAny(base, chars) {
			answers = append(answers, fmt.Sprintf("%s holds one of %q", base, chars))
			continue
		}
		if err := os.MkdirAll(base, 0o700); err != nil {
			answers = append(answers, outcome(base, 0, err))
			continue
		}
		dir, size, err := attempt(t, base, longest)
		if err != nil || (longest != "" && size > maxSocketPath) {
			if err == nil {
				_ = os.RemoveAll(dir)
			}
			answers = append(answers, outcome(base, size, err))
			continue
		}
		if accept != nil {
			if err := accept(dir); err != nil {
				_ = os.RemoveAll(dir)
				answers = append(answers, fmt.Sprintf("%s made a directory that will not do (%v)", base, err))
				continue
			}
		}
		return dir
	}
	t.Fatalf("testtmp: no base gives a directory whose path holds none of %q with room for %q within the %d bytes a unix socket path holds: %s",
		chars, longest, maxSocketPath, strings.Join(answers, "; "))
	return ""
}

// moduleScratch is .cache/testtmp under the module the test runs in: the closest directory above
// the test's working directory, its package directory, that holds a go.mod.
func moduleScratch() (string, error) {
	start, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for dir := start; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, ".cache", "testtmp"), nil
		}
		if filepath.Dir(dir) == dir {
			return "", fmt.Errorf("no go.mod above %s", start)
		}
	}
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
