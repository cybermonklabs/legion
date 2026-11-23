package testdb

import (
	"os"
	"testing"
)

// SkipOutsideCI skips a test whose test server lacks what the test needs, saying what is missing,
// and fails it with the same words on GitHub Actions, where GITHUB_ACTIONS is "true" in each step
// of each job: the job there is given a server with what the test needs, so a skip would hide the
// one run that counts. CI is not read, since an agent's shell sets CI=true too.
func SkipOutsideCI(t testing.TB, format string, args ...any) {
	t.Helper()
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}
