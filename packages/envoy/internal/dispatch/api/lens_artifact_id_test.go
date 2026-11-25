package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// TestALensMovedOntoADocumentRecordsThatDocumentsRow: every write that puts a lens on a document
// or on one of its blocks records, in the same write, the artifacts.id of the row the artifacts
// table holds at the document's address (graphstore.AttributeArtifactID), the move of a lens under
// a new parent as much as its invoke, and a move off a document retracts the row it recorded
// (spec requirement r-proposed, c-settle). On reasoningRig's issue alice makes two documents by
// typing a title, A and C, invokes trade-off analyses LM and LX on A, and on the spec a trade-off
// analysis with one finding F and a trade-off analysis LF on F: LM and LX record A's row, and LF
// none. The test moves LM from A to C and LX from A to F through graphstore.Write, as the move step
// of TestATriggerPointsOnlyAtALensInvokedBefore writes one (movePartOf): one write that retracts
// the lens's part_of to its old parent and asserts one to its new parent; and LF from F to C
// through the store's other write path, graphstore.PlanUncaused with the retraction and the
// assertion as its stated tuples and graphstore.QueueNodes, then graphstore.Caused under an event
// the test appends (QueueCaused, planPointerCheck). LM and LF then read live, answer their on in
// GET /api/v1/lenses?on= naming C and record C's row; LX reads live on F and records no row.
// Alice's removal of A leaves LM, LF and LX live, each answering its on in GET /api/v1/lenses?on=
// naming the parent it was moved to; her removal of C takes LM and LF gone, each answering GET
// /api/v1/lenses?on= and GET /api/v1/references?to= by its own address what the same read of a
// lens nobody wrote answers, 404 NOT_FOUND byte for byte once the id is substituted, and leaves LX
// live on F.
func TestALensMovedOntoADocumentRecordsThatDocumentsRow(t *testing.T) {
	t.Parallel()
	rig := newReasoningRig(t)
	a := emptyDocument(t, rig.handler, rig.issue, "Notes A.md")
	c := emptyDocument(t, rig.handler, rig.issue, "Notes C.md")
	aAddress := "dispatch://" + rig.issue + "/artifact/" + a.Slug
	cAddress := "dispatch://" + rig.issue + "/artifact/" + c.Slug
	lm := rig.invoke(aAddress, "trade-off", "alice", nil)
	lx := rig.invoke(aAddress, "trade-off", "alice", nil)
	onSpec := rig.invoke(rig.spec, "trade-off", "alice", map[string]any{"findings": []string{"F, the one finding of the spec's lens."}})
	f := onSpec.Findings[0]
	lf := rig.invoke(f.Address, "trade-off", "alice", nil)

	for _, invoked := range []struct {
		lens      lensInvoked
		on, wants string
	}{{lm, aAddress, a.ID}, {lx, aAddress, a.ID}, {lf, f.Address, ""}} {
		if got := recordedArtifactID(t, rig.database, invoked.lens); got != rowWords(invoked.wants) {
			t.Errorf("the lens %s invoked on %s records the row %s, want %s", invoked.lens.Address, invoked.on, got, rowWords(invoked.wants))
		}
	}

	for _, moved := range []struct {
		lens            lensInvoked
		from, onto      string
		toKind, toID    string
		caused          bool
		wants, wantedOn string
	}{
		{lm, aAddress, cAddress, graphstore.KindArtifact, c.RefKey, false, c.ID, cAddress},
		{lx, aAddress, f.Address, graphstore.KindLensBlock, f.ID, false, "", f.Address},
		{lf, f.Address, cAddress, graphstore.KindArtifact, c.RefKey, true, c.ID, cAddress},
	} {
		movePartOf(t, rig.database, rig.issue, graphstore.KindLens, moved.lens.ID, moved.toKind, moved.toID, moved.caused)
		if got := recordedArtifactID(t, rig.database, moved.lens); got != rowWords(moved.wants) {
			t.Errorf("the lens %s moved from %s onto %s records the row %s, want %s", moved.lens.Address, moved.from, moved.onto, got, rowWords(moved.wants))
		}
		state := storedNodeOf(t, rig.database, moved.lens.Address).State
		read, on := rig.lensesResponse(moved.lens.Address, "alice"), ""
		if read.Code == http.StatusOK {
			on = lensOn(decodeBody[nodeLenses](t, read))
		}
		if state != graphstore.StateLive || on != moved.wantedOn {
			t.Errorf("the lens %s moved from %s onto %s reads %s, and GET /api/v1/lenses?on= it answers %d on %q, want live and 200 on %s",
				moved.lens.Address, moved.from, moved.onto, state, read.Code, on, moved.wantedOn)
		}
	}

	// requireAfterRemoval fails, naming the lens, unless each of lenses on parent reads want after
	// the removal of the document at removed: a lens that reads live answers GET /api/v1/lenses?on=
	// its address 200 with its on naming parent, and one that reads gone answers alice's GET
	// /api/v1/lenses?on= and GET /api/v1/references?to= of its address what the same read of a lens
	// nobody wrote answers (requireReadsAsALensNobodyWrote).
	requireAfterRemoval := func(removed, parent, want string, lenses ...lensInvoked) {
		t.Helper()
		for _, lens := range lenses {
			if state := storedNodeOf(t, rig.database, lens.Address).State; state != want {
				t.Errorf("after the removal of %s, the lens %s on %s reads %s, want %s", removed, lens.Address, parent, state, want)
			}
			if want != graphstore.StateGone {
				read, on := rig.lensesResponse(lens.Address, "alice"), ""
				if read.Code == http.StatusOK {
					on = lensOn(decodeBody[nodeLenses](t, read))
				}
				if read.Code != http.StatusOK || on != parent {
					t.Errorf("after the removal of %s, GET /api/v1/lenses?on=%s answers %d on %q, want 200 on %s", removed, lens.Address, read.Code, on, parent)
				}
				continue
			}
			requireReadsAsALensNobodyWrote(t, func(read, status, login string) string {
				return fmt.Sprintf("after the removal of %s, %s's %s answers %s, want 404 NOT_FOUND as for a lens nobody wrote", removed, login, read, status)
			}, lensReader{"alice", rig.as(), lens})
		}
	}
	removedDocument(t, rig.handler, a.ID)
	requireAfterRemoval(aAddress, cAddress, graphstore.StateLive, lm, lf)
	requireAfterRemoval(aAddress, f.Address, graphstore.StateLive, lx)
	removedDocument(t, rig.handler, c.ID)
	requireAfterRemoval(cAddress, cAddress, graphstore.StateGone, lm, lf)
	requireAfterRemoval(cAddress, f.Address, graphstore.StateLive, lx)
}

// movePartOf moves the node the store keys as kind and id off the one parent its current part_of
// names and under the node keyed toKind and toID, through the store's write of a part_of tuple in
// a transaction of its own: one write that retracts its current part_of tuple and asserts one to
// its new parent, by alice now. Through graphstore.Write at the sentinel; or, where caused says so,
// through the store's other write path: graphstore.PlanUncaused with the two as its stated tuples
// and graphstore.QueueNodes, then graphstore.Caused under an event the test appends to issue
// (appendIssueEventDirectly).
func movePartOf(t *testing.T, database *store.Store, issue, kind, id, toKind, toID string, caused bool) {
	t.Helper()
	ctx := context.Background()
	place, err := graphstore.CurrentOut(ctx, database.Pool, kind, id, []string{graphstore.TuplePartOf})
	if err != nil || len(place) != 1 {
		t.Fatalf("the node %s %s stands on %v (%v), want one parent to move it from", kind, id, place, err)
	}
	moved := graphstore.Tuple{
		FromKind: kind, FromID: id, Kind: graphstore.TuplePartOf, ToKind: place[0].ToKind, ToID: place[0].ToID,
		At: time.Now().UTC(), Who: model.Actor{Kind: "user", ID: "alice"}, SourceEvent: graphstore.NoEvent,
	}
	placed := moved
	placed.ToKind, placed.ToID, placed.Asserted = toKind, toID, true
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if !caused {
			return graphstore.Write(ctx, tx, nil, []graphstore.Tuple{moved, placed})
		}
		write := graphstore.PlanUncaused(nil, []graphstore.Tuple{moved, placed})
		if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			graphstore.QueueNodes(b, write)
			return nil
		}); err != nil {
			return err
		}
		event, err := appendIssueEventDirectly(ctx, tx, issue, kind+".moved", model.Artifact{ID: id})
		if err != nil {
			return err
		}
		return graphstore.Caused(ctx, tx, write, event)
	}); err != nil {
		t.Fatalf("move %s %s from %s %s to %s %s: %v", kind, id, moved.ToKind, moved.ToID, toKind, toID, err)
	}
}

// TestALensOnABlockRecordsTheRowItsAddressNames holds the lens on a block to the document its
// address names (spec requirement r-proposed): the store's write of a lens's part_of reads the
// document of a block it places the lens on off the address before the `#` of the block's node
// id, and not off the node the block stands under. On reasoningRig's issue alice uploads a
// document X with a heading and one paragraph and makes a document Y by typing a title; through
// one graphstore.Write the test retracts the part_of of X's first block, X#b, to X's node and
// asserts one to Y's node (movePartOf), so X#b stands under Y while its address still names X, and
// alice invokes a trade-off analysis LK on X#b's address. LK records X's row, and not Y's; alice's
// removal of Y leaves LK live, and GET /api/v1/lenses?on= LK answers its on naming X#b's address.
func TestALensOnABlockRecordsTheRowItsAddressNames(t *testing.T) {
	t.Parallel()
	rig := newReasoningRig(t)
	x := uploadDocument(t, rig.handler, "/api/v1/issues/"+rig.issue, "x-notes.md", "# X\n\nThe one paragraph of X.\n").Artifact
	y := emptyDocument(t, rig.handler, rig.issue, "Notes Y.md")
	xAddress := "dispatch://" + rig.issue + "/artifact/" + x.Slug
	yAddress := "dispatch://" + rig.issue + "/artifact/" + y.Slug
	block := firstBlockOf(t, rig.handler, x.ID)
	blockNode, blockAddress := x.RefKey+"#"+block, refs.BlockRef(xAddress, block)
	movePartOf(t, rig.database, rig.issue, refs.BlockNodeKind, blockNode, graphstore.KindArtifact, y.RefKey, false)
	lk := rig.invoke(blockAddress, "trade-off", "alice", nil)
	if got := recordedArtifactID(t, rig.database, lk); got != x.ID {
		t.Errorf("the lens %s on %s records the row %s, want %s, the row of the document the block's address names, and not of %s, the node the block stands under",
			lk.Address, blockAddress, got, x.ID, yAddress)
	}
	removedDocument(t, rig.handler, y.ID)
	if state := storedNodeOf(t, rig.database, lk.Address).State; state != graphstore.StateLive {
		t.Errorf("after the removal of %s, the lens %s on %s reads %s, want live", yAddress, lk.Address, blockAddress, state)
	}
	if read := rig.lensesResponse(lk.Address, "alice"); read.Code != http.StatusOK {
		t.Errorf("after the removal of %s, GET /api/v1/lenses?on=%s answers %d %s, want 200 with its on naming %s", yAddress, lk.Address, read.Code, read.Body.String(), blockAddress)
	} else if on := lensOn(decodeBody[nodeLenses](t, read)); on != blockAddress {
		t.Errorf("after the removal of %s, GET /api/v1/lenses?on=%s answers its on naming %s, want %s", yAddress, lk.Address, on, blockAddress)
	}
}

// TestALensWriteThatWaitsBehindARemovalIsRefused races a lens write with a removal as main removes
// a document (spec requirement r-proposed). Alice makes a document D of an issue by typing a title.
// The test holds the issue's owner lock in a transaction of its own (lockstep.HoldOwner) and sends
// alice's POST /api/v1/issues/{key}/lenses on D's address through a handler whose store holds one
// connection of its own (oneConnectionHandler), so the backend it watches is the write's alone: the
// write finds D and then waits for that lock. Once that backend waits behind the held lock
// (storetest.WaitForQueuedBehind) - and not merely some backend, since a document's settlement
// queues at the same lock - the test deletes D's row in its held transaction as main's removal
// deletes it (deleteArtifactRows), writing no node, and commits. A write that answers before its
// backend waits fails the test. The lens write then answers 409 NODE_GONE, naming D by its ref_key
// as refuseGoneNode names a gone document, and the store holds no new lens node and no new part_of
// tuple out of a lens.
func TestALensWriteThatWaitsBehindARemovalIsRefused(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	issue := createInteractionIssue(t, handler, "RSN", "Reasoned work", "An opening paragraph.\n").Key
	d := emptyDocument(t, handler, issue, "Notes D.md")
	dAddress := "dispatch://" + issue + "/artifact/" + d.Slug
	placements := func() (lenses, partOfs int) {
		t.Helper()
		if err := database.Pool.QueryRow(ctx, `
			select (select count(*) from graph_nodes where kind = $1),
			       (select count(*) from graph_tuples where from_kind = $1 and kind = $2)
		`, graphstore.KindLens, graphstore.TuplePartOf).Scan(&lenses, &partOfs); err != nil {
			t.Fatalf("count the lens nodes and their part_of tuples: %v", err)
		}
		return lenses, partOfs
	}
	lensesBefore, partOfsBefore := placements()
	writeHandler, writePID := oneConnectionHandler(t, deps)
	owner := lockstep.HoldOwner(t, database, &lockstep.Hold{
		Name: "issue " + issue + "'s owner lock", Step: lockstep.StepOwner, Taken: true, Statement: lockstep.OwnerLockStatement,
	}, store.IssueOwner(issue))
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- dispatchRequest(t, writeHandler, http.MethodPost, "/api/v1/issues/"+issue+"/lenses",
			map[string]any{"on": dAddress, "kind": "trade-off"}, "alice")
	}()
	if early, answeredEarly := storetest.WaitForQueuedBehind(t, owner.Tx, writePID, answered, 30*time.Second); answeredEarly {
		t.Fatalf("the lens write on %s answered %d %s before the removal, want it waiting for the issue's owner lock", dAddress, early.Code, early.Body.String())
	}
	if err := deleteArtifactRows(ctx, owner.Tx, d); err != nil {
		t.Fatalf("delete the row of %s as main's removal does: %v", d.RefKey, err)
	}
	if err := owner.Tx.Commit(ctx); err != nil {
		t.Fatalf("commit the removal of %s: %v", d.RefKey, err)
	}
	var response *httptest.ResponseRecorder
	select {
	case response = <-answered:
	case <-time.After(30 * time.Second):
		t.Fatalf("the lens write on %s did not answer within 30s of the removal", dAddress)
	}
	want := nodeGone(graphstore.KindArtifact, d.RefKey).(*apiError)
	refusal := decodeBody[refusalAnswer](t, response)
	if response.Code != want.status || refusal.Code != want.code || refusal.Error != want.message {
		t.Errorf("the lens write on %s that waited behind its removal answers %d %s, want 409 NODE_GONE naming %s", dAddress, response.Code, response.Body.String(), d.RefKey)
	}
	if lenses, partOfs := placements(); lenses != lensesBefore || partOfs != partOfsBefore {
		t.Errorf("the refused lens write left %d lens node(s) and %d part_of tuple(s), want none", lenses-lensesBefore, partOfs-partOfsBefore)
	}
}

// recordedArtifactID is the row lens records (graphstore.AttributeArtifactID) as the store states
// it now, its newest artifact_id tuple, in rowWords' words; a lens's row that carries another value
// than that tuple fails the test, since the row is the projection of the tuples.
func recordedArtifactID(t *testing.T, database *store.Store, lens lensInvoked) string {
	t.Helper()
	ctx := context.Background()
	stated, held, err := graphstore.CurrentValue(ctx, database.Pool, graphstore.KindLens, lens.ID, graphstore.AttributeArtifactID)
	if err != nil {
		t.Fatalf("read the artifact_id of the lens %s: %v", lens.Address, err)
	}
	if !held {
		stated = ""
	}
	var carried *string
	if err := database.Pool.QueryRow(ctx, `select attributes->>$3 from graph_nodes where kind = $1 and id = $2`,
		graphstore.KindLens, lens.ID, graphstore.AttributeArtifactID).Scan(&carried); err != nil {
		t.Fatalf("read the row of the lens %s: %v", lens.Address, err)
	}
	row := ""
	if carried != nil {
		row = *carried
	}
	if row != stated {
		t.Errorf("the row of the lens %s carries the artifact_id %s, and its newest artifact_id tuple states %s", lens.Address, rowWords(row), rowWords(stated))
	}
	return rowWords(stated)
}

// rowWords is the row id as a failure names it, and "none" for no row.
func rowWords(id string) string {
	if id == "" {
		return "none"
	}
	return id
}
