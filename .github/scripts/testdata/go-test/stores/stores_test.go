package stores

import (
	"testing"

	"example.com/gotestfixtures/state"
)

// TestCountsOnEachStore stands for a test packages/envoy/scripts/backend-count.sh counts: an
// `aws` and a `gcp` subtest, as onEachStore runs them, the second failing on its first attempt.
func TestCountsOnEachStore(t *testing.T) {
	t.Run("aws", func(t *testing.T) {})
	t.Run("gcp", func(t *testing.T) {
		if state.Attempt(t, "gcp") == 1 {
			t.Fatal("fails on its first attempt")
		}
	})
}
