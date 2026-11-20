package testdb

import (
	"os"
	"strconv"
	"testing"
)

// SkipOutsideCI skips a test whose test server lacks what the test needs, saying what is missing,
// and fails it with the same words on GitHub Actions, where GITHUB_ACTIONS is "true" in each step
// of each job: the job there is given a server with what the test needs, so a skip would hide the
// one run that counts. CI is not read, since an agent's shell sets CI=true too.
func SkipOutsideCI(t testing.TB, format string, args ...any) {
	t.Helper()
	if os.Getenv(gitHubActionsEnv) == "true" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

// DispatchURLEnv names the Postgres server every Postgres-backed Dispatch test runs on: its admin
// connection string, on whose server each test makes a database of its own.
const DispatchURLEnv = "DISPATCH_TEST_DATABASE_URL"

// gitHubActionsEnv is the variable GitHub sets to true on every Actions runner, and ciEnv the one
// GitHub, and most other CI systems, set to true in every job.
const (
	gitHubActionsEnv = "GITHUB_ACTIONS"
	ciEnv            = "CI"
)

// DispatchURL is DispatchURLEnv's value. When it is unset it skips t, so a Postgres-backed test
// runs wherever a database is named and nowhere else - except in a CI run (CI set true, or
// GITHUB_ACTIONS=true), where it fails t naming the variable to set and the variable that made it
// fail: go test reads a skip as a pass, so a CI job whose database went missing would pass having
// run none of these tests. A shell that sets CI=true for its own tools, as an agent's shell does,
// therefore fails these tests too until it names a database or unsets CI.
//
// Every helper through which a Postgres-backed test of the module reads its server calls it: the
// store's storetest.Open, the store package's own tests, and internal/pgmigrate's.
func DispatchURL(t testing.TB) string {
	t.Helper()
	if raw := os.Getenv(DispatchURLEnv); raw != "" {
		return raw
	}
	if os.Getenv(gitHubActionsEnv) == "true" {
		t.Fatal(DispatchURLEnv + " must be set to run Postgres-backed Dispatch tests on a GitHub Actions runner (GITHUB_ACTIONS=true): without it the test would skip, and go test reads a skip as a pass")
	}
	if ci, _ := strconv.ParseBool(os.Getenv(ciEnv)); ci {
		t.Fatal(DispatchURLEnv + " must be set to run Postgres-backed Dispatch tests in a CI run (CI=" + os.Getenv(ciEnv) + "): without it the test would skip, and go test reads a skip as a pass; set it, or unset CI to skip these tests")
	}
	t.Skip(DispatchURLEnv + " must be set to run Postgres-backed Dispatch tests")
	return ""
}
