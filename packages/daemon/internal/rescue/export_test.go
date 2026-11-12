package rescue

// What this package's own tests reach into, and why each is here rather than in the fixture.
//
// The snapshot pins its size limit on its command line (runner.snapshot), and a command line
// outranks every configuration file, so a test cannot lower that limit from a repository, a
// workspace or a user file it plants: with 64 GiB pinned, jj refuses nothing and the refusal
// path the check reads is unreachable. Lowering the package's own value is the one way to it.
//
// And no test of this package may put a stand-in binary on PATH — the fixture runs the real git
// and jj of the machine, which is what makes its answers worth anything — so the only place a
// test can read the commands this check runs, with the arguments it runs them with, is the
// check itself, through the observer it calls before each one.

import "testing"

// SetSnapshotMaxNewFileSize pins size as the snapshot's size limit for the rest of t, putting
// the limit under a file the test writes so that jj refuses it.
func SetSnapshotMaxNewFileSize(t testing.TB, size string) {
	t.Helper()
	was := snapshotMaxNewFileSize
	snapshotMaxNewFileSize = size
	t.Cleanup(func() { snapshotMaxNewFileSize = was })
}

// ObserveCommands has observe called with the directory, the program and the arguments of every
// git and jj this package starts, for the rest of t.
func ObserveCommands(t testing.TB, observe func(dir, name string, args []string)) {
	t.Helper()
	was := observeCommand
	observeCommand = observe
	t.Cleanup(func() { observeCommand = was })
}
