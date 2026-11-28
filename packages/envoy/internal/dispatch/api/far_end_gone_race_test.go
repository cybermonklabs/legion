package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
)

// TestAFarEndTakenGoneWhileAWriteWaitsAnswersNotFound holds open, in a transaction, a person's gone
// of a lens (holdGone), posts meanwhile a lens triggered by it through POST .../lenses, and then
// commits the gone; and the same for a triggered_by at a lens out of a lens invoked after it, and
// for a requirement's constrained_by at a constraint, each through POST .../nodes/{id}/pointers.
// Each post finds its far end live before the gone commits and then waits at the issue's owner
// lock, so it reads the far end again once the gone has committed (refuseGoneFarEnds) and answers
// the 404 NOT_FOUND an address nobody holds answers, naming the far end as the post did, and no
// tuple points at the gone node: no lens is triggered by the gone lens, and no requirement is
// constrained by the gone constraint. A write that did not read its far end again under that lock
// would wait there and then point at the gone node.
func TestAFarEndTakenGoneWhileAWriteWaitsAnswersNotFound(t *testing.T) {
	t.Parallel()
	rig := newReasoningRig(t)
	// requireNothingAt fails unless no tuple of kind points at the node the store keys by toKind
	// and toID.
	requireNothingAt := func(kind, toKind, toID string) {
		t.Helper()
		if pointed := countRows(t, rig.database, `select count(*) from graph_tuples where kind = $1 and to_kind = $2 and to_id = $3`,
			kind, toKind, toID); pointed != 0 {
			t.Errorf("%d %s tuples point at the gone %s %s, want none", pointed, kind, toKind, toID)
		}
	}

	trigger := rig.invoke(rig.spec, "trade-off", "alice", nil)
	lenses, _ := rig.counts(graphstore.KindLens, graphstore.TupleTriggeredBy)
	invoked := holdGone(t, rig, "a lens triggered by "+trigger.Address, lensTarget(trigger), func() *httptest.ResponseRecorder {
		return rig.post("/lenses", map[string]any{"on": rig.spec, "kind": "botec", "triggered_by": trigger.Address}, "alice")
	})
	requireRefusedNaming(t, "a lens triggered by "+trigger.Address+" as it is taken gone", invoked, http.StatusNotFound, "NOT_FOUND", trigger.Address)
	requireNothingAt(graphstore.TupleTriggeredBy, graphstore.KindLens, trigger.ID)
	if after, _ := rig.counts(graphstore.KindLens, graphstore.TupleTriggeredBy); after != lenses {
		t.Errorf("the store holds %d lenses after the refused invocation, and held %d before", after, lenses)
	}

	earlier := rig.invoke(rig.spec, "trade-off", "alice", nil)
	later := rig.invoke(rig.spec, "botec", "alice", nil)
	pointed := holdGone(t, rig, "a triggered_by at "+earlier.Address, lensTarget(earlier), func() *httptest.ResponseRecorder {
		return rig.post(nodePath(later.Address, "pointers"), map[string]any{
			"kind": graphstore.TupleTriggeredBy, "action": pointerAppend, "to": earlier.Address,
		}, "alice")
	})
	requireRefusedNaming(t, "a triggered_by at "+earlier.Address+" as it is taken gone", pointed, http.StatusNotFound, "NOT_FOUND", earlier.Address)
	requireNothingAt(graphstore.TupleTriggeredBy, graphstore.KindLens, earlier.ID)

	written := rig.post("/constraints", map[string]any{"standard": "Every read answers in a second", "reference": "the service's own target"}, "alice")
	if written.Code != http.StatusCreated {
		t.Fatalf("write the constraint: status=%d body=%s", written.Code, written.Body.String())
	}
	constraint := decodeBody[constraintRead](t, written)
	target := stateTarget{kind: graphstore.KindConstraint, address: constraint.Address, end: storedEnd{kind: graphstore.KindConstraint, id: constraint.ID}}
	constrained := holdGone(t, rig, "a constrained_by at "+constraint.Address, target, func() *httptest.ResponseRecorder {
		return rig.post(nodePath(rig.requirement, "pointers"), map[string]any{
			"kind": graphstore.TupleConstrainedBy, "action": pointerAppend, "to": constraint.Address,
		}, "alice")
	})
	requireRefusedNaming(t, "a constrained_by at "+constraint.Address+" as it is taken gone", constrained, http.StatusNotFound, "NOT_FOUND", constraint.Address)
	requireNothingAt(graphstore.TupleConstrainedBy, graphstore.KindConstraint, constraint.ID)
}
