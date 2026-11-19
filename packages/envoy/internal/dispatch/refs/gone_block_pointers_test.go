package refs

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
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

// lineBlocks is a document of one block per line, each of the type it is named with, as a render
// hands it to the block index.
func lineBlocks(blocks ...[2]string) blockDoc {
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

// A transaction can index one document twice before either indexing is written: a writer of two
// versions of one document, or of two documents of one issue room, each of whose versions runs the
// room's one pass, plans the second against the Held of the first (BlockIndexing.Held), since no
// read of the store sees the first's rows until the write phase runs. Two of the facts a pass
// states are read from the store rather than from the rows it writes: who wrote a block whose node
// records its authors (blockAuthors) and the pointers a block it drops takes with it
// (retractGonePointers). Versions 2 and 3 are planned in one transaction, version 3 against
// version 2's Held, and then both are written: version 2 adds the requirement r-new and drops the
// check and the requirement a risk's prevented_by and a constrained_by point at, and version 3
// keeps r-new and holds neither dropped block. The store then states r-new's authored_by once, and
// each dropped pointer's retraction once, under version 2's event.
func TestTwoIndexingsOfOneDocumentInOneTransactionStateEachFactOnce(t *testing.T) {
	ctx := context.Background()
	database := parentDatabase(t)
	writer := model.Actor{Kind: "user", ID: "alice"}
	document := newParentDocument(t, database, "TWICE", "twice-md")
	const (
		risk       = "9a5e0000-0000-4000-8000-000000000011"
		constraint = "9a5e0000-0000-4000-8000-000000000012"
	)
	kept := [][2]string{{"r-kept", "requirement"}, {"p", "paragraph"}}
	first := lineBlocks(append([][2]string{{"r-dropped", "requirement"}, {"c-dropped", "check"}}, kept...)...)
	record := document.writeVersion(t, 1, first.markdown, writer)
	if won, _ := document.indexVersion(t, 1, first, record); !won {
		t.Fatal("version 1 did not become the version the document's blocks stand at")
	}
	at := record.at
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		node := func(kind, id string) graphstore.Node {
			return graphstore.Node{
				Kind: kind, ID: id, Supertag: kind, Visibility: graphstore.VisibilityProject, State: graphstore.StateLive,
				MadeAt: at, MadeBy: writer, SourceEvent: graphstore.NoEvent,
			}
		}
		return graphstore.Write(ctx, tx, []graphstore.Node{node(graphstore.KindRisk, risk), node(graphstore.KindConstraint, constraint)}, []graphstore.Tuple{
			{FromKind: graphstore.KindRisk, FromID: risk, Kind: graphstore.TuplePreventedBy, ToKind: BlockNodeKind, ToID: document.address("c-dropped"),
				At: at, Who: writer, SourceEvent: graphstore.NoEvent, Asserted: true},
			{FromKind: BlockNodeKind, FromID: document.address("r-dropped"), Kind: graphstore.TupleConstrainedBy, ToKind: graphstore.KindConstraint, ToID: constraint,
				At: at, Who: writer, SourceEvent: graphstore.NoEvent, Asserted: true},
		})
	}); err != nil {
		t.Fatalf("write the risk, the constraint and their pointers: %v", err)
	}

	second := lineBlocks(append([][2]string{{"r-new", "requirement"}}, kept...)...)
	third := lineBlocks(append([][2]string{{"r-new", "requirement"}}, kept...)...)
	third.markdown += "\n"
	secondRecord := document.writeVersion(t, 2, second.markdown, writer)
	thirdRecord := document.writeVersion(t, 3, third.markdown, writer)
	before := document.highestTuple(t)
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the two indexings: %v", err)
	}
	defer tx.Rollback(ctx)
	var now time.Time
	if err := tx.QueryRow(ctx, `select now()`).Scan(&now); err != nil {
		t.Fatalf("read the transaction's time: %v", err)
	}
	index, err := ReadBlockIndex(ctx, tx, document.artifact)
	if err != nil {
		t.Fatalf("read the document's block index: %v", err)
	}
	plan := func(index BlockIndex, number int, doc blockDoc, record versionRecord) BlockIndexing {
		t.Helper()
		indexed, err := ReadIndexedVersion(ctx, tx, document.artifact, document.artifact, number)
		if err != nil {
			t.Fatalf("read the record of version %d: %v", number, err)
		}
		indexing, err := PlanBlocks(ctx, tx, index, document.artifact, indexed, OneVersion(document.artifact, number, record.at), now,
			doc.markdown, doc.blocks, nil, nil, TypedFacts{}, graphstore.Uncaused{})
		if err != nil {
			t.Fatalf("plan the index of version %d: %v", number, err)
		}
		if !indexing.Won() {
			t.Fatalf("the plan of version %d lost the revision fence", number)
		}
		return indexing
	}
	secondPlan := plan(index, 2, second, secondRecord)
	thirdPlan := plan(secondPlan.Held(), 3, third, thirdRecord)
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		if err := secondPlan.Queue(b); err != nil {
			return err
		}
		if err := graphstore.QueueCaused(ctx, b, secondPlan.Stated(), secondRecord.event); err != nil {
			return err
		}
		if err := thirdPlan.Queue(b); err != nil {
			return err
		}
		return graphstore.QueueCaused(ctx, b, thirdPlan.Stated(), thirdRecord.event)
	}); err != nil {
		t.Fatalf("write the two indexings: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the two indexings: %v", err)
	}

	var authored, retracted []string
	for _, tuple := range document.tuplesAfter(t, database.Pool, before) {
		switch {
		case tuple.Kind == graphstore.TupleAuthoredBy && tuple.FromID == document.address("r-new"):
			authored = append(authored, tuple.ToKind+":"+tuple.ToID)
		case !tuple.Asserted && (tuple.Kind == graphstore.TuplePreventedBy || tuple.Kind == graphstore.TupleConstrainedBy):
			retracted = append(retracted, tuple.FromID+" -"+tuple.Kind+"-> "+tuple.ToID)
			if tuple.SourceEvent != secondRecord.event {
				t.Errorf("the retraction %s -%s-> %s is under event %d, want version 2's %d", tuple.FromID, tuple.Kind, tuple.ToID, tuple.SourceEvent, secondRecord.event)
			}
		}
	}
	if len(authored) != 1 {
		t.Errorf("the two indexings stated r-new's authored_by %v, want it once", authored)
	}
	slices.Sort(retracted)
	want := []string{
		risk + " -" + graphstore.TuplePreventedBy + "-> " + document.address("c-dropped"),
		document.address("r-dropped") + " -" + graphstore.TupleConstrainedBy + "-> " + constraint,
	}
	if !slices.Equal(retracted, want) {
		t.Errorf("the two indexings retracted %v, want each dropped pointer once: %v", retracted, want)
	}
}
