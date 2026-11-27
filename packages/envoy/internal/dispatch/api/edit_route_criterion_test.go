package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestACriterionTheEditRouteWritesStalesTheSpecsApproval holds R14 for the write INFRA-1196's part
// Y found: an approval covers its query's record, and a criterion is in the Spec query's record
// wherever the issue's room holds it. An issue born converted holds a spec of one requirement and a
// person's comment, and its spec query is gated and approved. A person writes a criterion under the
// comment's first block through POST /api/v1/artifacts/{tree}/edits, into the tree artifact's own
// fragment of the room: the Spec record takes it and its digest moves, while the spec's own text
// holds none of it. The edit's version runs the room's pass, which states the criterion and marks
// the issue's queries owed (docs' markRoomQueries), and the edit's ledger drains that mark once
// it has committed, before the route answers (docs.Ledger.Commit). The settlement the edit
// schedules is held an hour away. So by then the spec holds the next version, whose digest is the
// moved record's, no mark stands, no review was carried to that version, and every gate reads the
// approval stale: the approval read answers it at the version alice approved, its latest version
// the next one ("Approved v1 · latest v2", not "Approved v2").
//
// The control is the same kind of write with no mark queued: a value of the criterion stated
// straight into the store moves the Spec record's digest again, and no version is written, so the
// approval of the version standing reads current on every gate.
func TestACriterionTheEditRouteWritesStalesTheSpecsApproval(t *testing.T) {
	t.Parallel()
	rig := newQueryMarkRig(t)
	handler, database, key := rig.handler, rig.database, rig.issue.Key
	tree, err := issueTreeOf(context.Background(), database.Pool, key)
	if err != nil || tree == "" {
		t.Fatalf("read the tree artifact of %s: %q, %v", key, tree, err)
	}
	treeKey := refKeyOf(t, database, tree)
	_, block := personComment(t, handler, database, key, "We want the urgent asks first.\n\nAnd we want them quickly.")
	awaitDigestsFilled(t, database, rig.spec.id)
	publishDesignGates(t, handler, key, []issueDesignGate{{Step: model.GatedStepSpec, ArtifactID: rig.spec.id}})
	gate := model.AskApprovalDocument{ArtifactID: rig.spec.id}
	approveStanding(t, rig, key, model.GatedStepSpec, rig.spec.id, "the criterion's write")
	approved := newestVersionOf(t, database, rig.spec.id)
	before := rig.record(t, key, graphstore.QuerySpec)
	reviews := len(listReviews(t, handler, rig.spec.id))

	const words = "I want the urgent asks first."
	edited := editDocument(t, handler, tree, insertAtEnd(":::criterion{#k1 parent=\""+block+"\"}\n"+words+"\n:::\n"))
	if edited.Code != http.StatusOK {
		t.Fatalf("write the criterion into the tree of %s: status=%d body=%s", key, edited.Code, edited.Body.String())
	}
	criterion := treeKey + "#k1"
	written := rig.record(t, key, graphstore.QuerySpec)
	if !slices.Contains(walkAddresses(written.Nodes), blockAddress(criterion)) || written.Digest == before.Digest {
		t.Fatalf("after the criterion's write the Spec record of %s takes %v at the digest %s, want %s in it and the digest moved from %s",
			key, walkAddresses(written.Nodes), written.Digest, blockAddress(criterion), before.Digest)
	}
	if text := artifactTextOf(t, handler, rig.spec.id); strings.Contains(text, words) {
		t.Errorf("the spec of %s reads %q, want none of the criterion the tree holds", key, text)
	}
	requireNoQueryMark(t, database, "once the edit route answered", key)
	latest := newestVersionOf(t, database, rig.spec.id)
	if latest != approved+1 {
		t.Fatalf("the criterion's write left version %d of the spec of %s newest, want %d: the edit's mark owes the Spec query a version", latest, key, approved+1)
	}
	awaitDigestsFilled(t, database, rig.spec.id)
	if got, was := versionDigestOf(t, database, rig.spec.id, latest), versionDigestOf(t, database, rig.spec.id, approved); got != written.Digest || got == was {
		t.Errorf("version %d of the spec of %s answers the digest %s, want the Spec record's %s, moved from the approved version %d's %s",
			latest, key, got, written.Digest, approved, was)
	}
	requireGate(t, rig, key, model.GatedStepSpec, gate, false, "the criterion written through the edit route")
	if got := readApproval(t, handler, rig.spec.id); got.Approval == nil || got.Approval.State != "stale" ||
		got.Approval.Version == nil || *got.Approval.Version != approved || got.Approval.LatestVersion != latest {
		t.Errorf("the approval of the spec of %s reads %+v, want stale at version %d with version %d its latest", key, got.Approval, approved, latest)
	}
	if after := listReviews(t, handler, rig.spec.id); len(after) != reviews || after[0].Version != approved || after[0].CarriedFromVersion != nil {
		t.Errorf("the criterion's write left the reviews of the spec of %s %+v, want alice's own approval of version %d newest and none carried", key, after, approved)
	}

	// The control: a write that moves the Spec record and queues no mark writes no version, so
	// the approval of the version standing reads current.
	approveStanding(t, rig, key, model.GatedStepSpec, rig.spec.id, "the control's write, which queues no mark")
	standing := newestVersionOf(t, database, rig.spec.id)
	rig.state(t, blockValue(criterion, "probe", "unmarked"))
	if moved := rig.record(t, key, graphstore.QuerySpec); moved.Digest == written.Digest {
		t.Fatalf("the control's value of %s leaves the Spec digest of %s at %s, want it moved", criterion, key, written.Digest)
	}
	if number := newestVersionOf(t, database, rig.spec.id); number != standing {
		t.Errorf("the control's write, which queues no mark, left version %d of the spec of %s newest, want %d", number, key, standing)
	}
	requireGate(t, rig, key, model.GatedStepSpec, gate, true, "a value of the criterion stated with no mark")
	if got := readApproval(t, handler, rig.spec.id); got.Approval == nil || got.Approval.State != "approved" ||
		got.Approval.Version == nil || *got.Approval.Version != standing || got.Approval.LatestVersion != standing {
		t.Errorf("the approval of the spec of %s reads %+v after the control's write, want approved at version %d, its latest", key, got.Approval, standing)
	}
}
