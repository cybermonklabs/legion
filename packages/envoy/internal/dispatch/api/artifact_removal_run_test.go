package api

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// runDocumentRig is an issue of alice's and a run she asked, for a test to make a document under
// the run's header and then end the document.
type runDocumentRig struct {
	handler  http.Handler
	database *store.Store
	issue    string
	run      string
}

// runDocumentAuthor is alice, who asks the run of every runDocumentRig and reads what it changed.
var runDocumentAuthor = model.Actor{Kind: "user", ID: "alice"}

// newRunDocumentRig is a server of its own, with alice's issue on project and her run.
func newRunDocumentRig(t *testing.T, project string) runDocumentRig {
	t.Helper()
	handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{})
	issue := createTestIssue(t, handler, project, "The issue a run makes a document on")
	run, err := directServer(deps).recordGoingRequest(context.Background(), runDocumentAuthor, "", "Make a notes document on the issue.")
	if err != nil {
		t.Fatalf("record alice's run: %v", err)
	}
	return runDocumentRig{handler: handler, database: database, issue: issue, run: run.ID}
}

// intoRun is what alice reads into the run, each edge of its ?to= as its kind and its from end's
// `<kind>/<id>`, sorted, and it fails unless the run's backlink count is the length of that list:
// a count is the list behind it.
func (r runDocumentRig) intoRun(t *testing.T, what string) []string {
	t.Helper()
	ctx := context.Background()
	edges, err := refs.Edges(ctx, r.database.Pool, runDocumentAuthor, refs.Query{Direction: "in", Kind: graphstore.KindInteraction, ID: r.run})
	if err != nil {
		t.Fatalf("%s: read alice's ?to= of her run: %v", what, err)
	}
	counts, err := refs.BacklinkCounts(ctx, r.database.Pool, runDocumentAuthor, graphstore.KindInteraction, []string{r.run}, nil)
	if err != nil {
		t.Fatalf("%s: count the run's backlinks as alice: %v", what, err)
	}
	var from []string
	for _, edge := range edges {
		from = append(from, edge.Kind+" "+edge.Node.Kind+"/"+edge.Node.ID)
	}
	slices.Sort(from)
	t.Logf("%s: the run's backlink count is %d, and its ?to= lists %v", what, counts[r.run], from)
	if counts[r.run] != len(edges) {
		t.Errorf("%s: the run's backlink count is %d and its ?to= lists %d edge(s), %v: a count is the list behind it",
			what, counts[r.run], len(edges), from)
	}
	return from
}

// outOfDocument is each changed_by_run alice reads out of the document, its ?from= read by the
// document's uuid as GET /api/v1/references reads it, as the `<kind>/<id>` it ends at, sorted.
func (r runDocumentRig) outOfDocument(t *testing.T, what, documentID string) []string {
	t.Helper()
	edges, err := refs.Edges(context.Background(), r.database.Pool, runDocumentAuthor, refs.Query{Direction: "out", Kind: graphstore.KindArtifact, ID: documentID})
	if err != nil {
		t.Fatalf("%s: read alice's ?from= of document %s: %v", what, documentID, err)
	}
	var to []string
	for _, edge := range edges {
		if edge.Kind == graphstore.TupleChangedByRun {
			to = append(to, edge.Node.Kind+"/"+edge.Node.ID)
		}
	}
	slices.Sort(to)
	t.Logf("%s: the ?from= of document %s lists the changed_by_run edges to %v", what, documentID, to)
	return to
}

// storedNode is the state of the node of the document at refKey and the changed_by_run tuples it
// states now.
func (r runDocumentRig) storedNode(t *testing.T, what, refKey string) (string, []graphstore.Tuple) {
	t.Helper()
	ctx := context.Background()
	node, held, err := graphstore.ReadNode(ctx, r.database.Pool, graphstore.KindArtifact, refKey)
	if err != nil || !held {
		t.Fatalf("%s: read the node of %s: held=%t, %v", what, refKey, held, err)
	}
	changed, err := graphstore.CurrentOut(ctx, r.database.Pool, graphstore.KindArtifact, refKey, []string{graphstore.TupleChangedByRun})
	if err != nil {
		t.Fatalf("%s: read the changed_by_run out of %s: %v", what, refKey, err)
	}
	return node.State, changed
}

// madeForRun makes a blank document named name on the issue under the run's header, and fails
// unless the run is read in both directions as having made it: the run's ?to= lists the document
// by its uuid and the document's ?from= lists the run, so what a test then reads of the document's
// end proves something. The run's window is not moved ahead: this create is its one write, made
// at once inside the window its record gave it, so the run's row keeps the ends_at its node
// states and the census holds the two together.
func (r runDocumentRig) madeForRun(t *testing.T, name string) model.Artifact {
	t.Helper()
	made := requestForRun(t, r.handler, http.MethodPost, "/api/v1/issues/"+r.issue+"/artifacts", map[string]any{"name": name}, "alice", r.run)
	if made.Code != http.StatusCreated {
		t.Fatalf("make the blank document under the run's header: status=%d body=%s", made.Code, made.Body.String())
	}
	document := decodeBody[uploadedArtifact](t, made).Artifact
	if want := []string{graphstore.TupleChangedByRun + " artifact/" + document.ID}; !slices.Equal(r.intoRun(t, "after the run made the document"), want) {
		t.Fatalf("alice's ?to= of her run does not list %v, the document the run made", want)
	}
	if want := []string{graphstore.KindInteraction + "/" + r.run}; !slices.Equal(r.outOfDocument(t, "after the run made the document", document.ID), want) {
		t.Fatalf("alice's ?from= of the document the run made does not list %v", want)
	}
	return document
}

// A document a write made for a run created, and that a person then removed, is no longer the
// run's (spec requirement r-interaction, R4). Alice's run makes a blank document on
// her issue, which marks the document's node changed by the run: the run's ?to= lists the
// document, its count is that list, and the document's ?from= lists the run. Alice removes the
// document; its node is stated gone and the changed_by_run out of it retracted, so the run counts
// nothing of it and lists nothing, and the census reads zero differences. A document made again
// under the same title takes the same ref_key, which is the id of the same node: it brings the
// node back live with no changed_by_run to the old run, so the run neither lists nor counts it,
// its ?from= lists no run, and the census still reads zero differences. A removal that left the
// node live counted the removed document while listing it no more, and one that left its
// changed_by_run asserted listed the new document under the run that made the old one, both ways.
func TestARunListsAndCountsNoDocumentItMadeOnceThatDocumentIsRemoved(t *testing.T) {
	rig := newRunDocumentRig(t, "GONE")
	document := rig.madeForRun(t, "notes.md")

	removedDocument(t, rig.handler, document.ID)
	if listed := rig.intoRun(t, "after the removal"); len(listed) != 0 {
		t.Errorf("after the removal alice's ?to= of her run lists %v, want nothing: the document is gone", listed)
	}
	if state, changed := rig.storedNode(t, "after the removal", document.RefKey); state != graphstore.StateGone || len(changed) != 0 {
		t.Errorf("after the removal the node of %s is %s and states the changed_by_run %+v, want it gone and stating none",
			document.RefKey, state, changed)
	}
	requireTheCensusAgreesOver(t, rig.database, "after the removal", graphstore.KindInteraction)

	again := emptyDocument(t, rig.handler, rig.issue, "notes.md")
	if again.RefKey != document.RefKey || again.ID == document.ID {
		t.Fatalf("the document made again is %s at %s, want a new document at the removed one's %s, so it is the same node",
			again.ID, again.RefKey, document.RefKey)
	}
	if listed := rig.intoRun(t, "after the title was made again"); len(listed) != 0 {
		t.Errorf("after the title was made again alice's ?to= of her run lists %v, want nothing: the run made only the removed document",
			listed)
	}
	if listed := rig.outOfDocument(t, "after the title was made again", again.ID); len(listed) != 0 {
		t.Errorf("after the title was made again alice's ?from= of the new document lists the run %v, want none", listed)
	}
	if state, changed := rig.storedNode(t, "after the title was made again", again.RefKey); state != graphstore.StateLive || len(changed) != 0 {
		t.Errorf("after the title was made again the node of %s is %s and states the changed_by_run %+v, want it live and stating none",
			again.RefKey, state, changed)
	}
	requireTheCensusAgreesOver(t, rig.database, "after the title was made again", graphstore.KindInteraction)
}

// A document a write made for a run created, and that was then retired (docs.RetireDocument,
// R11 of cybermonklabs/legion#4173), is counted and listed alike by the run, in both directions. Alice's
// run makes a blank document on her issue, which the run's ?to= lists and its count counts, and
// whose ?from= lists the run. The document is retired: its node is stated gone, and its row, its
// slug and its ref_key stay, so no later document takes its node. The retirement retracts no
// changed_by_run, and none is owed: every read of the run's edges goes through graph_edges, whose
// tuple arm answers no relation out of a node that is gone (migration 20261117020856,
// `node.state <> 'gone'`), and the run's count holds each tuple only while the node it goes out of
// is not gone (refs' liveVisibleNodeOf), so the run's ?to= and its count both leave the document
// out, and its ?from= lists no run.
func TestARunListsAndCountsNoDocumentItMadeOnceThatDocumentIsRetired(t *testing.T) {
	rig := newRunDocumentRig(t, "RETIRED")
	document := rig.madeForRun(t, "notes.md")

	retireTestDocument(t, rig.database, document.ID)
	state, changed := rig.storedNode(t, "after the retirement", document.RefKey)
	t.Logf("after the retirement the node of %s is %s and states the changed_by_run %+v", document.RefKey, state, changed)
	if state != graphstore.StateGone {
		t.Fatalf("after the retirement the node of %s is %s, want gone", document.RefKey, state)
	}
	if listed := rig.intoRun(t, "after the retirement"); len(listed) != 0 {
		t.Errorf("after the retirement alice's ?to= of her run lists %v, want nothing: the document's node is gone", listed)
	}
	if listed := rig.outOfDocument(t, "after the retirement", document.ID); len(listed) != 0 {
		t.Errorf("after the retirement alice's ?from= of the document lists the run %v, want none: its node is gone", listed)
	}
}
