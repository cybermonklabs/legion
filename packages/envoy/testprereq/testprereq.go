// Package testprereq holds Require, the one rule of this repository's Go tests for a test whose
// prerequisite is missing: a server a variable names, a program on PATH, a capability of the
// machine or of the server a test runs against. Off GitHub Actions the test skips, naming what it
// lacks. On GitHub Actions it fails, naming what it lacks and the ci.yaml job that must give it: a
// CI job is set up to give each test it runs what that test needs, so a skip there passes the job
// with the test's proof gone from it, and nobody reads the skip line.
//
// It is a public package of the envoy module, outside internal/, so packages/daemon takes the same
// rule rather than a copy of it: Go's internal rule reads a path prefix, and the daemon module
// imports this one through its replace of packages/envoy, as it imports brokertest and clitest.
// It imports the standard library alone, so every package of either module can take it, among
// them the daemon's internal/testpg/testdb, which imports nothing of its own module.
package testprereq

import (
	"fmt"
	"os"
	"testing"
)

// Require returns when present is true. Otherwise it skips t with the reason format and args give,
// which names the missing prerequisite; under GITHUB_ACTIONS=true it fails t instead, with that
// reason and job, the ci.yaml job that runs the test and must give it the prerequisite. Both
// happen at the test that called it (t.Helper) and never in a TestMain, so the package's other
// tests still run and report.
func Require(t testing.TB, present bool, job, format string, args ...any) {
	t.Helper()
	if present {
		return
	}
	if !Enforced() {
		t.Skipf(format, args...)
		return
	}
	t.Fatalf("a prerequisite that ci.yaml's %s job must give on GitHub Actions is missing: %s", job, fmt.Sprintf(format, args...))
}

// Enforced says whether a missing prerequisite fails the test rather than skips it: under
// GITHUB_ACTIONS=true. A test that prints its own verdict line before it calls Require reads it,
// so the line says FAIL where Require fails.
func Enforced() bool { return os.Getenv("GITHUB_ACTIONS") == "true" }
