package api

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// runCloseSpec is the spec of TestACloseMadeForARunSettlesTheIssuesDocumentsForNoRun: a paragraph
// a browser rewords, and a decision whose block it deletes.
const runCloseSpec = "Context\n\n" +
	":::ask{#deleted-decision urgency=\"med\" multiple=\"false\"}\nShip it?\n\n- Yes: Now.\n- No: Later.\n:::\n"

// changedByRunInto is the `<kind>/<id>` of the node each changed_by_run tuple to the run's
// interaction goes out of, as the store holds them, one for each tuple, sorted.
func changedByRunInto(t *testing.T, database *store.Store, run string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select from_kind || '/' || from_id from graph_tuples
		where kind = $1 and to_kind = $2 and to_id = $3
		order by 1
	`, graphstore.TupleChangedByRun, graphstore.KindInteraction, run)
	if err != nil {
		t.Fatalf("read the changed_by_run tuples to run %s: %v", run, err)
	}
	var from []string
	for rows.Next() {
		var node string
		if err := rows.Scan(&node); err != nil {
			rows.Close()
			t.Fatalf("read a changed_by_run tuple to run %s: %v", run, err)
		}
		from = append(from, node)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read the changed_by_run tuples to run %s: %v", run, err)
	}
	return from
}

// changedByRunOutOf is each changed_by_run alice reads out of the node kind/id, its ?from= as
// GET /api/v1/references reads it (refs.Edges), as the `<kind>/<id>` it ends at, sorted, as
// runDocumentRig.outOfDocument reads a document's.
func changedByRunOutOf(t *testing.T, database *store.Store, kind, id string) []string {
	t.Helper()
	edges, err := refs.Edges(context.Background(), database.Pool, runDocumentAuthor, refs.Query{Direction: "out", Kind: kind, ID: id})
	if err != nil {
		t.Fatalf("read alice's ?from= of %s/%s: %v", kind, id, err)
	}
	var to []string
	for _, edge := range edges {
		if edge.Kind == graphstore.TupleChangedByRun {
			to = append(to, edge.Node.Kind+"/"+edge.Node.ID)
		}
	}
	slices.Sort(to)
	return to
}

// storedBlockNodes is the state of each block node the store holds of the document at refKey, by
// its address (the document's ref_key, '#', the block's id).
func storedBlockNodes(t *testing.T, database *store.Store, refKey string) map[string]string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select id, state from graph_nodes where kind = $1 and starts_with(id, $2)
	`, refs.BlockNodeKind, refKey+"#")
	if err != nil {
		t.Fatalf("read the block nodes of %s: %v", refKey, err)
	}
	nodes := map[string]string{}
	for rows.Next() {
		var address, state string
		if err := rows.Scan(&address, &state); err != nil {
			rows.Close()
			t.Fatalf("read a block node of %s: %v", refKey, err)
		}
		nodes[address] = state
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read the block nodes of %s: %v", refKey, err)
	}
	return nodes
}

// An issue's close made for a run settles the issue's documents first (settleIssueBeforeClose),
// on the request's own context, which carries the run (withinGoingRun), and that settlement is
// made for no run: it saves what the room's writers left in it, whoever they were, in a
// transaction of its own, so nothing it writes is the run's (spec requirement r-interaction:
// a write appends the tuple from each node it created or changed, in its own transaction), as the
// same settlement on the room's timer writes none. alice's browser, in one update, deletes the
// block of an open decision, rewords the spec's first paragraph and adds a paragraph; the room
// stores the update, and the document owes the settlement that saves it, which no timer runs
// while the test does. alice then closes the issue through PATCH /api/v1/issues/{key} under the
// header of her run. The close's settlement saves the update: it retracts the decision in its own
// name and indexes the blocks it moved - the deleted block's node gone, the reworded and the added
// paragraph's live. Neither the retracted ask's node nor any block node of the spec holds a
// changed_by_run to the run, in the store or in what alice reads out of it, while the issue node
// holds the one the close's own write appends, and nothing else does.
func TestACloseMadeForARunSettlesTheIssuesDocumentsForNoRun(t *testing.T) {
	// An hour, so the only settlement of the browser's update is the one the close runs.
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour, browserPeers: true})
	documentService := deps.Docs.(*docs.Service)
	ctx := context.Background()
	issue := createInteractionIssue(t, handler, "RCL", "A close made for a run", runCloseSpec)
	spec := readDecisionDocument(t, database, issue.Key, "spec")
	settleIssue(t, documentService, issue.Key)
	awaitSettled(t, database, spec.id)
	ask := requireBlockAsk(t, handler, issue.Key, spec.id, "deleted-decision")

	const reworded, appended = "Context, reworded in the browser", "Added in the browser."
	added, err := pmdoc.Parse(appended + "\n")
	if err != nil {
		t.Fatalf("parse the browser's paragraph: %v", err)
	}
	peer := connectBrowserPeer(t, documentService, spec.id)
	t.Cleanup(peer.close)
	stored := storedUpdateCount(t, database, spec.id)
	peer.edit(t, func(tree *pmdoc.Node) error {
		kept := slices.DeleteFunc(slices.Clone(tree.Children), func(child *pmdoc.Node) bool {
			id, _ := child.Attrs[pmdoc.BlockIDAttr].(string)
			return id == "deleted-decision"
		})
		if len(kept) == len(tree.Children) {
			return fmt.Errorf("the document holds no block %q to delete", "deleted-decision")
		}
		tree.Children = append(kept, added.Children...)
		return replaceFirstParagraph("Context", reworded)(tree)
	})
	peer.barrier(t)
	awaitStoredUpdates(t, database, spec.id, stored+1)
	awaitOwedSettlement(t, database, spec.id)
	if before := readBlockAsk(t, handler, ask); before.State != "open" {
		t.Fatalf("the decision whose block the browser deleted reads %s before the close, want open: no settlement may have run yet", before.State)
	}

	run, err := directServer(deps).recordGoingRequest(ctx, model.Actor{Kind: "user", ID: "alice"}, "", "Close the issue for me.")
	if err != nil {
		t.Fatalf("record alice's run: %v", err)
	}
	runWindowAhead(t, database, run.ID)
	closed := requestForRun(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{"status": "done"}, "alice", run.ID)
	t.Logf("alice's close of %s under run %s: status=%d body=%s", issue.Key, run.ID, closed.Code, closed.Body.String())
	if closed.Code != http.StatusOK {
		t.Fatalf("close %s under run %s's header: status=%d body=%s", issue.Key, run.ID, closed.Code, closed.Body.String())
	}
	if status := decodeBody[model.Issue](t, closed).Status; status != "done" {
		t.Fatalf("the close under run %s left %s %s, want done", run.ID, issue.Key, status)
	}

	// The close's settlement saved the browser's update: it retracted the decision in its own name
	// and indexed the blocks the update moved.
	retracted := readBlockAsk(t, handler, ask)
	if resolution := retracted.Resolution; retracted.State != "resolved" || resolution == nil || resolution.Kind != "retracted" ||
		!resolution.Actor.SameAs(docs.SettlementActor) || !strings.HasPrefix(resolution.Reason, docs.SettlementRetractionReason) {
		t.Fatalf("the decision whose block the browser deleted reads %s with %+v after the close, want retracted by %+v: the close's settlement did not save the update",
			retracted.State, resolution, docs.SettlementActor)
	}
	if _, version := documentTexts(t, handler, spec.id); !strings.Contains(version, reworded) || !strings.Contains(version, appended) ||
		strings.Contains(version, "#deleted-decision") {
		t.Fatalf("the spec's latest version reads:\n%s\nwant the browser's update saved", version)
	}
	ids := blockIDsByText(t, handler, spec.id)
	if ids[reworded] == "" || ids[appended] == "" {
		t.Fatalf("the spec's blocks by text are %v, want the reworded and the added paragraph", ids)
	}
	blocks := storedBlockNodes(t, database, spec.refKey)
	t.Logf("the block nodes of %s after the close: %v", spec.refKey, blocks)
	for _, moved := range []struct{ what, address, state string }{
		{"deleted ask block", spec.address("deleted-decision"), graphstore.StateGone},
		{"reworded paragraph", spec.address(ids[reworded]), graphstore.StateLive},
		{"added paragraph", spec.address(ids[appended]), graphstore.StateLive},
	} {
		if state := blocks[moved.address]; state != moved.state {
			t.Fatalf("the node block/%s of the %s is %q after the close, want %s: the close's settlement did not index it", moved.address, moved.what, state, moved.state)
		}
	}

	// Nothing the settlement wrote holds a changed_by_run to the run: not the retracted ask's node,
	// not any block node of the spec, in the store or in what alice reads out of them.
	if held := changedByRunCount(t, database, graphstore.KindAsk, ask, run.ID); held != 0 {
		t.Errorf("the node ask/%s, which the close's settlement retracted, holds %d changed_by_run tuples to run %s, want none: the settlement is made for no run",
			ask, held, run.ID)
	}
	if read := changedByRunOutOf(t, database, graphstore.KindAsk, ask); len(read) != 0 {
		t.Errorf("alice's ?from= of the node ask/%s, which the close's settlement retracted, lists the changed_by_run edges to %v, want none, to run %s or any other",
			ask, read, run.ID)
	}
	for _, address := range slices.Sorted(maps.Keys(blocks)) {
		if held := changedByRunCount(t, database, refs.BlockNodeKind, address, run.ID); held != 0 {
			t.Errorf("the node block/%s (%s), which the close's settlement indexed, holds %d changed_by_run tuples to run %s, want none: the settlement is made for no run",
				address, blocks[address], held, run.ID)
		}
		if read := changedByRunOutOf(t, database, refs.BlockNodeKind, address); len(read) != 0 {
			t.Errorf("alice's ?from= of the node block/%s (%s), which the close's settlement indexed, lists the changed_by_run edges to %v, want none, to run %s or any other",
				address, blocks[address], read, run.ID)
		}
	}

	// The close's own write of the issue's status holds the run's one tuple, and nothing else does.
	if held := changedByRunCount(t, database, graphstore.KindIssue, issue.Key, run.ID); held != 1 {
		t.Errorf("the node issue/%s holds %d changed_by_run tuples to run %s after the close made for it, want the one the close's own write appends",
			issue.Key, held, run.ID)
	}
	if read, want := changedByRunOutOf(t, database, graphstore.KindIssue, issue.Key), []string{graphstore.KindInteraction + "/" + run.ID}; !slices.Equal(read, want) {
		t.Errorf("alice's ?from= of the node issue/%s lists the changed_by_run edges to %v, want %v, the close's own", issue.Key, read, want)
	}
	issueNode := graphstore.KindIssue + "/" + issue.Key
	for _, node := range changedByRunInto(t, database, run.ID) {
		if node != issueNode {
			t.Errorf("the node %s holds a changed_by_run tuple to run %s, want only %s's, the close's own write", node, run.ID, issueNode)
		}
	}
	readers := runDocumentRig{handler: handler, database: database, issue: issue.Key, run: run.ID}
	if read, want := readers.intoRun(t, "after the close"), []string{graphstore.TupleChangedByRun + " " + issueNode}; !slices.Equal(read, want) {
		t.Errorf("alice's ?to= of run %s lists %v, want %v, the close's own write of the issue alone", run.ID, read, want)
	}
}
