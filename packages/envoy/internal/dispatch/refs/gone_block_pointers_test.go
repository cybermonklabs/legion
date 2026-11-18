package refs

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestTheIndexPassRetractsThePointersOfABlockItMarksGone holds the block index to what a block
// the version drops takes with it (dispatch://INFRA-1950/spec#b-r-constraint-risk): a risk's
// prevented_by at a check block and a requirement block's constrained_by to a constraint are each
// retracted, in the pass's own transaction and under the version's event, once a version drops
// the block, so the store holds neither now. The same risk's prevented_by at a check the version
// keeps, and the constrained_by of a requirement it keeps, stand as they stood, and the pass
// retracts nothing else.
func TestTheIndexPassRetractsThePointersOfABlockItMarksGone(t *testing.T) {
	ctx := context.Background()
	database := parentDatabase(t)
	writer := model.Actor{Kind: "user", ID: "alice"}
	document := newParentDocument(t, database, "GONE", "gone-md")
	const (
		risk       = "9a5e0000-0000-4000-8000-000000000001"
		constraint = "9a5e0000-0000-4000-8000-000000000002"
	)

	// version is a document of one block per line, each of the type it is named with.
	version := func(blocks ...[2]string) blockDoc {
		var doc blockDoc
		var markdown strings.Builder
		for _, block := range blocks {
			line := block[0] + " " + block[1] + ".\n"
			doc.blocks = append(doc.blocks, model.ArtifactBlock{ID: block[0], Type: block[1], From: markdown.Len(), To: markdown.Len() + len(line)})
			markdown.WriteString(line)
		}
		doc.markdown = markdown.String()
		return doc
	}
	kept := [][2]string{{"r-kept", "requirement"}, {"c-kept", "check"}, {"p", "paragraph"}}
	first := version(append([][2]string{{"r-dropped", "requirement"}, {"c-dropped", "check"}}, kept...)...)
	record := document.writeVersion(t, 1, first.markdown, writer)
	if won, _ := document.indexVersion(t, 1, first, record); !won {
		t.Fatal("version 1 did not become the version the document's blocks stand at")
	}

	at := record.at
	pointer := func(fromKind, fromID, kind, toKind, toID string) graphstore.Tuple {
		return graphstore.Tuple{
			FromKind: fromKind, FromID: fromID, Kind: kind, ToKind: toKind, ToID: toID,
			At: at, Who: writer, SourceEvent: graphstore.NoEvent, Asserted: true,
		}
	}
	node := func(kind, id string) graphstore.Node {
		return graphstore.Node{
			Kind: kind, ID: id, Supertag: kind, Visibility: graphstore.VisibilityProject, State: graphstore.StateLive,
			MadeAt: at, MadeBy: writer, SourceEvent: graphstore.NoEvent,
		}
	}
	pointers := []graphstore.Tuple{
		pointer(graphstore.KindRisk, risk, graphstore.TuplePreventedBy, BlockNodeKind, document.address("c-dropped")),
		pointer(graphstore.KindRisk, risk, graphstore.TuplePreventedBy, BlockNodeKind, document.address("c-kept")),
		pointer(BlockNodeKind, document.address("r-dropped"), graphstore.TupleConstrainedBy, graphstore.KindConstraint, constraint),
		pointer(BlockNodeKind, document.address("r-kept"), graphstore.TupleConstrainedBy, graphstore.KindConstraint, constraint),
	}
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		return graphstore.Write(ctx, tx, []graphstore.Node{node(graphstore.KindRisk, risk), node(graphstore.KindConstraint, constraint)}, pointers)
	}); err != nil {
		t.Fatalf("write the risk, the constraint and their pointers: %v", err)
	}

	second := version(kept...)
	record = document.writeVersion(t, 2, second.markdown, writer)
	won, stated := document.indexVersion(t, 2, second, record)
	if !won {
		t.Fatal("version 2 did not become the version the document's blocks stand at")
	}
	requireRestatedBlock(t, document, "c-dropped", graphstore.StateGone, []string{graphstore.StateLive, graphstore.StateGone})
	requireRestatedBlock(t, document, "r-dropped", graphstore.StateGone, []string{graphstore.StateLive, graphstore.StateGone})

	// The pass retracted the two pointers at the blocks it dropped, dated by the version and
	// signed by its writer, under the event that recorded it, and nothing else.
	var retracted []string
	for _, tuple := range stated {
		if tuple.Asserted {
			continue
		}
		retracted = append(retracted, tuple.FromKind+" "+tuple.FromID+" -"+tuple.Kind+"-> "+tuple.ToKind+" "+tuple.ToID)
		if !tuple.At.Equal(record.at) || !tuple.Who.SameCredential(record.writer) || tuple.SourceEvent != record.event {
			t.Errorf("the retraction %s %s -%s-> %s is dated %s by %+v under event %d, want %s by %+v under %d",
				tuple.FromKind, tuple.FromID, tuple.Kind, tuple.ToID, tuple.At, tuple.Who, tuple.SourceEvent,
				record.at, record.writer, record.event)
		}
	}
	slices.Sort(retracted)
	want := []string{
		BlockNodeKind + " " + document.address("r-dropped") + " -" + graphstore.TupleConstrainedBy + "-> " + graphstore.KindConstraint + " " + constraint,
		graphstore.KindRisk + " " + risk + " -" + graphstore.TuplePreventedBy + "-> " + BlockNodeKind + " " + document.address("c-dropped"),
	}
	if !slices.Equal(retracted, want) {
		t.Errorf("the pass over version 2 retracted %v, want exactly %v", retracted, want)
	}

	farEnds := func(kind, id, tupleKind string) []string {
		t.Helper()
		held, err := graphstore.CurrentOut(ctx, database.Pool, kind, id, []string{tupleKind})
		if err != nil {
			t.Fatalf("read the %s of %s %s: %v", tupleKind, kind, id, err)
		}
		ends := make([]string, len(held))
		for index, tuple := range held {
			ends[index] = tuple.ToID
		}
		return ends
	}
	if ends := farEnds(graphstore.KindRisk, risk, graphstore.TuplePreventedBy); !slices.Equal(ends, []string{document.address("c-kept")}) {
		t.Errorf("the risk is prevented by %v now, want the kept check alone", ends)
	}
	if ends := farEnds(BlockNodeKind, document.address("r-dropped"), graphstore.TupleConstrainedBy); len(ends) != 0 {
		t.Errorf("the dropped requirement is held to %v now, want nothing", ends)
	}
	if ends := farEnds(BlockNodeKind, document.address("r-kept"), graphstore.TupleConstrainedBy); !slices.Equal(ends, []string{constraint}) {
		t.Errorf("the kept requirement is held to %v now, want the constraint", ends)
	}
	checked, err := graphstore.CurrentIn(ctx, database.Pool, BlockNodeKind, document.address("c-dropped"), []string{graphstore.TuplePreventedBy})
	if err != nil {
		t.Fatalf("read what the dropped check prevents: %v", err)
	}
	if len(checked) != 0 {
		t.Errorf("the dropped check still prevents %+v, want nothing", checked)
	}
}
