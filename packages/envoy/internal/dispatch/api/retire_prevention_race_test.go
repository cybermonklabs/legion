package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestAPreventedByAppendedWhileItsChecksDocumentIsRetiredIsNotLeftStanding retires the document of
// a check of another issue in a transaction held open, appends a prevented_by from a risk to that
// check through the pointer route meanwhile, and then commits the retirement. A retired document's
// checks are run by nobody, so no prevented_by may point at one (spec requirement r-constraint-risk):
// whichever write commits second must see the other, and the risk must hold no prevented_by to the
// check once both are done. The pointer route locks only the risk's issue and reads the document's
// retired_at with no lock, and the retirement reads the check's prevented_by tuples before the
// append commits, so both commit and the tuple is left standing at a check nobody runs.
func TestAPreventedByAppendedWhileItsChecksDocumentIsRetiredIsNotLeftStanding(t *testing.T) {
	t.Parallel()
	rig := newPremortemRig(t)
	requirePreventedByLosesToRetirement(t, rig, rig.otherIssueCheck,
		"The test retires the document that holds the check.", "a check of a retired document")
}

// requirePreventedByLosesToRetirement writes a risk under a pre-mortem of rig's spec, retires
// rig's other issue's document that holds the check in a transaction held open, with reason,
// appends a prevented_by from the risk to the check, named by to, through the pointer route
// meanwhile, and then commits the retirement. It fails unless the risk holds no prevented_by to the
// check once both are done, naming the check as check says.
func requirePreventedByLosesToRetirement(t *testing.T, rig premortemRig, to, reason, check string) {
	t.Helper()
	ctx := context.Background()
	premortem := rig.invoke(rig.spec, graphstore.LensKindPreMortem, "alice", nil)
	written := rig.post("/risks", map[string]any{
		"lens": premortem.Address, "heading": "A job runs twice.", "early_warning": "Two drainers.", "prevention": "One drainer.",
	}, "alice")
	if written.Code != http.StatusCreated {
		t.Fatalf("write the risk: status=%d body=%s", written.Code, written.Body.String())
	}
	risk := decodeBody[riskWritten](t, written)
	document := rig.otherIssueDocumentID

	tx, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the retirement: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := docs.RetireDocument(ctx, tx, document, model.Actor{Kind: "user", ID: "alice"}, reason); err != nil {
		t.Fatalf("retire %s: %v", document, err)
	}

	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- rig.post(nodePath(risk.Address, "pointers"), map[string]any{
			"kind": graphstore.TuplePreventedBy, "action": "append", "to": to,
		}, "alice")
	}()
	var appended *httptest.ResponseRecorder
	select {
	case appended = <-answered:
	case <-time.After(3 * time.Second):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the retirement: %v", err)
	}
	if appended == nil {
		select {
		case appended = <-answered:
		case <-time.After(30 * time.Second):
			t.Fatalf("the append to %s did not answer within 30 s of the retirement's commit", to)
		}
	}
	t.Logf("the append to %s while the document holding it was being retired: status=%d body=%s",
		to, appended.Code, appended.Body.String())

	if held := rig.current(graphstore.KindRisk, risk.ID, graphstore.TuplePreventedBy); len(held) != 0 {
		t.Errorf("after the retirement of %s committed, the risk %s is prevented by %v, %s (the append answered %d)",
			document, risk.Address, held, check, appended.Code)
	}
}

// TestAPreventedByAppendedAtAChecksRoomAddressWhileItsDocumentIsRetiredIsNotLeftStanding is
// TestAPreventedByAppendedWhileItsChecksDocumentIsRetiredIsNotLeftStanding with the check named by
// its room address, `<tree ref_key>#<block>`: the id the prevented_by tuple stores, of the second
// issue's room, which an issue born converted holds its Verification document's checks in. Before
// its '#' that address names the issue's tree artifact, not the document the test retires, so the
// append the route holds at the check document's owner lock (lockReasoningOwners) takes the tree's
// owner, and the retirement (docs.RetireDocument) the Verification document's. Both are the
// issue's owner lock, since every artifact of an issue, the tree among them, stands under the
// issue (events.OwnerRows, docs' lockArtifactOwner), so the two writes queue there whichever
// address the append names, and the risk holds no prevented_by to the check once both are done.
func TestAPreventedByAppendedAtAChecksRoomAddressWhileItsDocumentIsRetiredIsNotLeftStanding(t *testing.T) {
	t.Parallel()
	rig := newPremortemRig(t)
	room := rig.otherIssueCheckNode
	// Else the address names the document the test retires, and the case is the test above.
	var named string
	if err := rig.database.Pool.QueryRow(context.Background(), `select kind from artifacts where ref_key = split_part($1, '#', 1)`, room).Scan(&named); err != nil || named != "tree" {
		t.Fatalf("the check's room address %s names an artifact of kind %q before its '#' (%v), want the issue's tree", room, named, err)
	}
	requirePreventedByLosesToRetirement(t, rig, room,
		"The test retires the document whose fragment holds the check.", "a check of a retired document named by its room address")
}
