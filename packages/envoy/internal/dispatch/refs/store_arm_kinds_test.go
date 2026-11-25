package refs

import (
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
)

// The store arm of the graph read (keyedStoreArm) names every placement graphstore's pointerRules
// holds as a placement, and none of them as a pointer. Each pointer of the table is among the
// arm's pointers by construction (storeArmPointers is built from graphstore.PointerKinds), so what
// can drift is the placements: each kind graphstore.PlacementKinds lists is in storeArmPlacements,
// which the arm reads only out of a node of the store's own kinds or a finding, so a placement
// added to the table is never dropped from the read unsaid; and no kind of storeArmPlacements is
// in graphstore.PointerKinds, since a placement that became a pointer would be read out of any
// node.
//
// The negative control is a placement rule of a new kind added to pointerRules alone, which this
// test names.
func TestTheStoreArmReadsEveryKindThePointerRulesHold(t *testing.T) {
	for _, kind := range graphstore.PlacementKinds() {
		if !slices.Contains(storeArmPlacements, kind) {
			t.Errorf("pointerRules holds %s as a placement, which keyedStoreArm does not name among its placements (storeArmPlacements)", kind)
		}
	}
	pointers := graphstore.PointerKinds()
	for _, kind := range storeArmPlacements {
		if slices.Contains(pointers, kind) {
			t.Errorf("storeArmPlacements names %s, which pointerRules holds as a pointer (graphstore.PointerKinds), so keyedStoreArm reads it out of any node", kind)
		}
	}
}
