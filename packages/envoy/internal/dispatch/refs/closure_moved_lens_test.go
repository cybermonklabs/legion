package refs

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// movedLens is the lens writeLensNode writes.
const movedLens = "00000000-0000-4000-8000-0000000003a1"

// writeLensNode writes the lens movedLens under the issue OPS-1 of project OPS, live or gone,
// through the store's writer as the reasoning write stores one: the node and its part_of. A second
// call moves its state alone, as the state route's write does.
func writeLensNode(t *testing.T, database *store.Store, state string) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the lens's write: %v", err)
	}
	defer tx.Rollback(ctx)
	writer, at := model.Actor{Kind: "user", ID: "alice"}, time.Now().UTC()
	node, held, err := graphstore.ReadNode(ctx, tx, graphstore.KindLens, movedLens)
	if err != nil {
		t.Fatalf("read the lens %s: %v", movedLens, err)
	}
	var tuples []graphstore.Tuple
	if !held {
		node = graphstore.Node{Kind: graphstore.KindLens, ID: movedLens, Supertag: graphstore.KindLens,
			Visibility: graphstore.VisibilityProject, Project: "OPS"}
		tuples = []graphstore.Tuple{{
			FromKind: graphstore.KindLens, FromID: movedLens, Kind: graphstore.TuplePartOf, ToKind: graphstore.KindIssue, ToID: "OPS-1",
			At: at, Who: writer, SourceEvent: graphstore.NoEvent, Asserted: true,
		}}
	}
	node.State, node.MadeAt, node.MadeBy, node.SourceEvent = state, at, writer, graphstore.NoEvent
	node.Restates, node.KeepsCause, node.Changed = false, held, held
	if err := graphstore.Write(ctx, tx, []graphstore.Node{node}, tuples); err != nil {
		t.Fatalf("write the lens %s %s: %v", movedLens, state, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the lens's write: %v", err)
	}
}

// A link to a document whose own address moved to a lens - the pre-mortem conversion moves the
// address of the document it retires to the lens it wrote in its place (api.ConvertPremortem) -
// reaches that lens: the references of the linking issue list the lens among their links, and
// their members hold one member of kind lens at the lens's address, with the document's ref_key
// and the link's depth and via. The document the moved one cites is two hops away, and no member
// of either issue. A link to a block of the moved document is no link to its address, and still
// reaches the document itself. Once the lens is gone the move reaches nothing that stands
// (block_moved_to), and the link to the address reaches the document again.
func TestTheClosureAnswersTheLensACitedDocumentMovedTo(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the fixture: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, project := range []string{"CORE", "OPS", "DOC"} {
		createReferenceProject(t, tx, project)
		createReferenceIssue(t, tx, project+"-1", project)
	}
	moved, movedRef := createReferenceArtifact(t, tx, "OPS-1", "OPS", "graph-premortem-md")
	_, citedRef := createReferenceArtifact(t, tx, "", "CORE", "cited-md")
	insertDocumentBlock(t, tx, moved, "b1", movedRef+"#b1")
	insertReference(t, tx, "artifact", moved, "artifact", citedRef)
	addressCitation := createReferenceComment(t, tx, "CORE-1")
	insertReference(t, tx, "comment", addressCitation, "artifact", movedRef)
	blockCitation := createReferenceComment(t, tx, "DOC-1")
	insertReference(t, tx, "comment", blockCitation, "block", movedRef+"#b1")
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the fixture: %v", err)
	}
	writeLensNode(t, database, graphstore.StateLive)
	moveTx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the move: %v", err)
	}
	defer moveTx.Rollback(ctx)
	if err := graphstore.Write(ctx, moveTx, nil, []graphstore.Tuple{{
		FromKind: graphstore.KindArtifact, FromID: movedRef, Kind: graphstore.TupleMovedTo, ToKind: graphstore.KindLens, ToID: movedLens,
		At: time.Now().UTC(), Who: model.Actor{Kind: "user", ID: "alice"}, SourceEvent: graphstore.NoEvent, Asserted: true,
	}}); err != nil {
		t.Fatalf("move %s to the lens: %v", movedRef, err)
	}
	if err := moveTx.Commit(ctx); err != nil {
		t.Fatalf("commit the move: %v", err)
	}

	referencesOf := func(key string) References {
		t.Helper()
		references, err := Closure(ctx, database.Pool, model.Actor{}, key)
		if err != nil {
			t.Fatalf("read the references of %s: %v", key, err)
		}
		return references
	}
	linked := func(references References) []string {
		nodes := []string{}
		for _, link := range references.Links {
			nodes = append(nodes, link.Node.Kind+" "+link.Node.ID)
		}
		return nodes
	}
	movedDocument := documentMember(t, database.Pool, movedRef, model.ReferenceVia{Kind: "comment", ID: addressCitation})
	lensMember := movedDocument
	lensMember.Kind, lensMember.Address = graphstore.KindLens, "dispatch://"+graphstore.KindLens+"/"+movedLens
	byBlock := movedDocument
	byBlock.Via.ID = blockCitation

	core := referencesOf("CORE-1")
	if !reflect.DeepEqual(core.Members, []Member{lensMember}) {
		t.Errorf("the members of CORE-1, whose comment cites the moved document's address, are %#v; want the lens alone, %#v", core.Members, lensMember)
	}
	if got, want := linked(core), []string{graphstore.KindLens + " " + movedLens}; !slices.Equal(got, want) {
		t.Errorf("the links of CORE-1 are %v; want the lens alone, %v", got, want)
	}
	if got := referencesOf("DOC-1").Members; !reflect.DeepEqual(got, []Member{byBlock}) {
		t.Errorf("the members of DOC-1, whose comment cites a block of the moved document, are %#v; want the document alone, %#v",
			got, []Member{byBlock})
	}

	writeLensNode(t, database, graphstore.StateGone)
	if got := referencesOf("CORE-1").Members; !reflect.DeepEqual(got, []Member{movedDocument}) {
		t.Errorf("once the lens is gone the members of CORE-1 are %#v; want the document alone, %#v", got, []Member{movedDocument})
	}
}
