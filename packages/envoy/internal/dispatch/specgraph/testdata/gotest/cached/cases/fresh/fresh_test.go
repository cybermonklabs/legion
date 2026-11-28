// Package fresh runs again on each go test run: its test reads GOTEST_RUN, which the cache keys
// on, and make-reports.sh gives each run another value.
package fresh

import (
	"os"
	"testing"
)

func TestFresh(t *testing.T) {
	t.Log("run", os.Getenv("GOTEST_RUN"))
}
