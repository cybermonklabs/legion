package rescue

// What this package's own tests reach into, and why it is here rather than in the fixture.
//
// No test of this package may put a stand-in binary on PATH — the fixture runs the real git
// and jj of the machine, which is what makes its answers worth anything — so the only place a
// test can read the commands this check runs, with the arguments it runs them with, is the
// check itself, through the observer it calls before each one.

import "testing"

// ObserveCommands has observe called with the directory, the program and the arguments of every
// git and jj this package starts, for the rest of t.
func ObserveCommands(t testing.TB, observe func(dir, name string, args []string)) {
	t.Helper()
	was := observeCommand
	observeCommand = observe
	t.Cleanup(func() { observeCommand = was })
}
