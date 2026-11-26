package ordered

import (
	"testing"

	"example.com/gotestfixtures/state"
)

// TestThreeOrderedSubtests runs three subtests in order, as kind's tree tests run their
// checkpoints: the second fails on its first attempt and stops the test there, and the third
// reads what the second left. A rerun of the root test runs all three again and passes; a rerun
// of the second subtest alone passes too, with the third never run, which is the rerun the
// harness's case refuses.
func TestThreeOrderedSubtests(t *testing.T) {
	checkpoint := 0
	if !t.Run("first", func(t *testing.T) { checkpoint = 1 }) {
		return
	}
	if !t.Run("second", func(t *testing.T) {
		if state.Attempt(t, "second") == 1 {
			t.Fatal("fails on its first attempt")
		}
		checkpoint = 2
	}) {
		return
	}
	t.Run("third", func(t *testing.T) {
		if checkpoint != 2 {
			t.Fatalf("the second subtest left checkpoint %d, not 2", checkpoint)
		}
	})
}
