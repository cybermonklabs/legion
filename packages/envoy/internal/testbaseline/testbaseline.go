// Package testbaseline names the commit a test of the `basebuild` tag compares this build with.
// The commit is the one in the environment variable BASELINE_SHA, and only that one: no test
// works it out from the repository itself. Work done on a branch that lands on main compares
// with `git merge-base origin/main HEAD`. A child stacked on another branch compares with that
// branch's head, and the fork point with main would hold none of the routes, tables or rows its
// parent added, so a child that deleted one of them would pass. The run, which knows which
// branch the work lands on, names the commit, as the Verification document's runs of
// this change do with `BASELINE_SHA=${BASELINE_SHA:-$(git merge-base origin/main HEAD)}`.
//
// It is an ordinary package rather than a _test.go file because cmd/dispatch is package main and
// can import no other package's test helpers, the reason packages/envoy/internal/testtmp gives
// for itself.
package testbaseline

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Variable is the environment variable that names the commit.
const Variable = "BASELINE_SHA"

// Commit answers the full id of the commit BASELINE_SHA names. It fails the test at once when the
// variable is unset or empty, naming the variable, and when git cannot read the commit, as in a
// shallow clone that does not hold it. It runs git in the test's own working directory, which is
// inside the checkout.
func Commit(t testing.TB) string {
	t.Helper()
	named := strings.TrimSpace(os.Getenv(Variable))
	if named == "" {
		t.Fatalf("%s is unset: name the commit this build is compared with, such as "+
			"%s=$(git merge-base origin/main HEAD) for a branch that lands on main, or the "+
			"parent branch's head for a child stacked on it", Variable, Variable)
	}
	out, err := exec.Command("git", "rev-parse", "--verify", "--quiet", named+"^{commit}").Output()
	commit := strings.TrimSpace(string(out))
	if err != nil || commit == "" {
		t.Fatalf("%s=%s names no commit this checkout holds (git rev-parse --verify: %v)", Variable, named, err)
	}
	return commit
}
