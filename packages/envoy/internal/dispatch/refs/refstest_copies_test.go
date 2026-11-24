package refs

import (
	"maps"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/refs/refstest"
)

// TestRefstestBacklinkKindsAreTheArmTablesKinds holds refstest's copy of the node kinds
// BacklinkCounts answers for to the keys of backlinkArms, sorted, so a kind the table gains or
// loses is gained or lost by the list the tests outside this package count.
func TestRefstestBacklinkKindsAreTheArmTablesKinds(t *testing.T) {
	if copied, declared := refstest.BacklinkKinds(), slices.Sorted(maps.Keys(backlinkArms)); !slices.Equal(copied, declared) {
		t.Errorf("refstest.BacklinkKinds is %q and backlinkArms declares %q: change the copy with the table", copied, declared)
	}
}
