package dbcount

import (
	"os"
	"testing"

	"example.com/gotestfixtures/state"
)

// TestReadsItsDatabase stands for a test of a package packages/envoy/scripts/database-skips.sh
// runs: it skips without its database variable, as those do, and with it set fails on its first
// attempt alone.
func TestReadsItsDatabase(t *testing.T) {
	if os.Getenv("FIXTURE_DATABASE_URL") == "" {
		t.Skip("FIXTURE_DATABASE_URL is unset")
	}
	if state.Attempt(t, "dbcount") == 1 {
		t.Fatal("fails on its first attempt")
	}
}

func TestPassesBeside(t *testing.T) {}
