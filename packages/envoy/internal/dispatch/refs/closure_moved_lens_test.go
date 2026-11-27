package refs

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
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
		references, err := Closure(ctx, database.Pool, model.Actor{}, key, "")
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

// A finding of a lens is a block the store holds no excerpt of: its loader reads its whole text
// (loadReasoningNodes), and every read that names it cuts that text where it names it, as a
// document block's stored excerpt is cut (resolvedNode.name, BlockExcerpt). The finding here,
// written under writeLensNode's lens through the store's writer as the census writes one, holds a
// text past BlockExcerptRunes with the key CORE-1234 across rune 480. TargetNames, ExcerptedNodes
// and the excerpt of the finding's part_of edge on the lens's in read (Edges) each answer exactly
// BlockExcerpt of that text: neither the whole text, which a name that took the finding's text as
// an already cut block excerpt would answer, nor the plain cut at 480, which leaves half the key,
// `CO`, at the end of the words.
func TestAFindingIsNamedByItsTextCutAsABlockExcerpt(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the fixture: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "OPS")
	createReferenceIssue(t, tx, "OPS-1", "OPS")
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the fixture: %v", err)
	}
	writeLensNode(t, database, graphstore.StateLive)

	// 476 runes and a space, so the key runs from rune 477 to 486, across the bound.
	body := strings.Repeat("x", 476) + " CORE-1234 decides it."
	want := BlockExcerpt(body, "")
	if plain := text.HeadRunes(body, BlockExcerptRunes); want == body || want == plain {
		t.Fatalf("BlockExcerpt(%d runes) = %q, the whole text or the plain cut %q: the fixture no longer tells the three apart", len(body), want, plain)
	}
	alice, at := model.Actor{Kind: "user", ID: "alice"}, time.Now().UTC()
	finding := graphstore.KindLens + "/" + movedLens + "#f1"
	findingTx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the finding's write: %v", err)
	}
	defer findingTx.Rollback(ctx)
	tuple := func(kind, toKind, toID string) graphstore.Tuple {
		return graphstore.Tuple{
			FromKind: BlockNodeKind, FromID: finding, Kind: kind, ToKind: toKind, ToID: toID,
			At: at, Who: alice, SourceEvent: graphstore.NoEvent, Asserted: true,
		}
	}
	if err := graphstore.Write(ctx, findingTx, []graphstore.Node{{
		Kind: BlockNodeKind, ID: finding, Supertag: BlockNodeKind, Visibility: graphstore.VisibilityProject,
		State: graphstore.StateLive, Project: "OPS", MadeAt: at, MadeBy: alice, SourceEvent: graphstore.NoEvent,
	}}, []graphstore.Tuple{
		tuple(graphstore.TuplePartOf, graphstore.KindLens, movedLens),
		tuple(graphstore.AttributeText, graphstore.KindValue, body),
	}); err != nil {
		t.Fatalf("write the finding %s through the store's writer: %v", finding, err)
	}
	if err := findingTx.Commit(ctx); err != nil {
		t.Fatalf("commit the finding's write: %v", err)
	}

	target := Target{Kind: graphstore.KindLensBlock, ID: finding}
	names, err := TargetNames(ctx, database.Pool, alice, []Target{target}, "")
	if err != nil {
		t.Fatalf("name the finding: %v", err)
	}
	if got := names[target].Name; got != want {
		t.Errorf("TargetNames names the finding %q (%d runes); want its text cut as a block excerpt, %q", got, len([]rune(got)), want)
	}
	excerpted, err := ExcerptedNodes(ctx, database.Pool, alice, []Target{target}, "")
	if err != nil {
		t.Fatalf("read the finding with its excerpt: %v", err)
	}
	if got := excerpted[target].Excerpt; got == nil || *got != (model.GraphExcerpt{BlockID: "f1", Text: want}) {
		t.Errorf("ExcerptedNodes answers the finding's excerpt %+v; want block f1 with %q", got, want)
	}
	edges, err := Edges(ctx, database.Pool, alice, Query{Direction: "in", Kind: graphstore.KindLens, ID: movedLens})
	if err != nil {
		t.Fatalf("read the edges into the lens: %v", err)
	}
	index := slices.IndexFunc(edges, func(edge model.GraphEdge) bool {
		return edge.Kind == graphstore.TuplePartOf && edge.Node.Kind == graphstore.KindLensBlock && edge.Node.ID == finding
	})
	if index < 0 {
		t.Fatalf("the lens's in read lists no part_of from the finding %s: %+v", finding, edges)
	}
	if got := edges[index].Excerpt; got == nil || *got != (model.GraphExcerpt{BlockID: "f1", Text: want}) {
		t.Errorf("the finding's part_of edge shows the excerpt %+v; want block f1 with %q", got, want)
	}
}
