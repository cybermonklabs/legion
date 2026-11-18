package refs

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// A stale index pass retracts the links of the version's own kinds that another release dropped,
// and keeps every tuple another writer states out of a block the version still holds.
//
// Version 1 of a document holds a requirement, r1, and a paragraph, p1, whose text mentions r1; it
// is indexed for a run of the command bar, so both blocks hold a changed_by_run to that run's
// interaction. The pointer route's write then holds r1 to a constraint (constrained_by). Version 2
// is written and indexed as the release before this one does it: p1's mention is gone from its
// text, so that release deletes the mention's block_refs row, records the version indexed and the
// document's blocks standing at it, and states nothing in the store. Version 3, the same text, is
// then indexed by this build, which finds the store's statement stale (standsAtVersion) and reads
// what the store still asserts of the document's blocks (heldDocumentBlocks).
//
// The mention p1 no longer holds is retracted by that pass, which is the stale path doing what it
// is for. r1's constrained_by and each block's changed_by_run still stand afterwards, and the pass
// appended no retraction of either: no block_refs row projects them, so no version decides them.
func TestAStaleIndexPassKeepsTheTuplesAnotherWriterStatesOutOfAKeptBlock(t *testing.T) {
	database := parentDatabase(t)
	document := newParentDocument(t, database, "STALE", "spec")
	ctx := context.Background()
	writer := model.Actor{Kind: "user", ID: "alice"}
	at := time.Now().UTC().Truncate(time.Microsecond)
	const (
		run        = "5a1e0000-1e45-4000-8000-000000000001"
		constraint = "5a1e0000-1e45-4000-8000-0000000000c1"
	)
	requirement := "Every read is cached for a minute.\n"
	versionOf := func(paragraph string) blockDoc {
		markdown := requirement + paragraph
		return blockDoc{markdown: markdown, blocks: []model.ArtifactBlock{
			{ID: "r1", Type: "requirement", From: 0, To: len(requirement)},
			{ID: "p1", Type: "paragraph", From: len(requirement), To: len(markdown)},
		}}
	}
	first := versionOf("See dispatch://" + document.refKey + "#b-r1.\n")
	later := versionOf("See the requirement above.\n")
	r1, p1 := document.address("r1"), document.address("p1")

	// The run whose interaction version 1's blocks are marked changed by.
	interactionNodes, interactionTuples := graphstore.InteractionFacts(graphstore.Interaction{
		ID: run, Author: writer, Project: parentProject("STALE"),
		Words: "Cache every read for a minute.", At: at, EndsAt: at.Add(time.Minute),
	})
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		return graphstore.Write(ctx, tx, interactionNodes, interactionTuples)
	}); err != nil {
		t.Fatalf("write the run's interaction: %v", err)
	}

	firstRecord := document.writeVersion(t, 1, first.markdown, writer)
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		_, won, write, err := IndexBlocks(ctx, tx, document.artifact, OneVersion(document.artifact, 1, firstRecord.at), first.markdown, first.blocks,
			text.ExtractAt(first.markdown, "https://dispatch.example"), nil, TypedFacts{})
		if err != nil || !won {
			t.Fatalf("index version 1 (won %t): %v", won, err)
		}
		return graphstore.Caused(graphstore.WithRun(ctx, run), tx, write, firstRecord.event)
	}); err != nil {
		t.Fatalf("append what version 1 stated for the run: %v", err)
	}
	requireCurrentOut(t, database.Pool, p1, "mentions", []string{BlockNodeKind + "/" + r1}, "after version 1")
	for _, block := range []string{r1, p1} {
		requireCurrentOut(t, database.Pool, block, graphstore.TupleChangedByRun,
			[]string{graphstore.KindInteraction + "/" + run}, "after version 1, indexed for the run")
	}

	// The pointer route's write: a constraint node and r1's constrained_by to it.
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		return graphstore.Write(ctx, tx, []graphstore.Node{{
			Kind: graphstore.KindConstraint, ID: constraint, Supertag: graphstore.KindConstraint,
			Visibility: graphstore.VisibilityProject, State: graphstore.StateLive, Project: parentProject("STALE"),
			MadeAt: at, MadeBy: writer, SourceEvent: graphstore.NoEvent,
		}}, []graphstore.Tuple{{
			FromKind: BlockNodeKind, FromID: r1, Kind: graphstore.TupleConstrainedBy,
			ToKind: graphstore.KindConstraint, ToID: constraint,
			At: at, Who: writer, SourceEvent: graphstore.NoEvent, Asserted: true,
		}})
	}); err != nil {
		t.Fatalf("hold the requirement to the constraint: %v", err)
	}
	constrained := []string{graphstore.KindConstraint + "/" + constraint}
	requireCurrentOut(t, database.Pool, r1, graphstore.TupleConstrainedBy, constrained, "after the pointer's write")

	// Version 2, as the release before this one writes and indexes it: the mention's row deleted,
	// the version recorded indexed and the document's blocks standing at it, and no store write.
	document.writeVersion(t, 2, later.markdown, writer)
	for _, statement := range []string{
		`delete from block_refs where artifact_id = $1::uuid and kind = 'mentions'`,
		`insert into document_indexed_versions (artifact_id, version) values ($1::uuid, 2)`,
		`update document_block_versions set version = 2 where artifact_id = $1::uuid`,
	} {
		if _, err := database.Pool.Exec(ctx, statement, document.artifact); err != nil {
			t.Fatalf("index version 2 as the previous release: %s: %v", statement, err)
		}
	}
	var storeVersion, standing int
	if err := database.Pool.QueryRow(ctx, `select store_version, version from document_block_versions where artifact_id = $1::uuid`,
		document.artifact).Scan(&storeVersion, &standing); err != nil || storeVersion != 1 || standing != 2 {
		t.Fatalf("the document's blocks stand at version %d with the store's statement at %d (%v), want 2 and 1", standing, storeVersion, err)
	}

	won, stated := document.indexVersion(t, 3, later, document.writeVersion(t, 3, later.markdown, writer))
	if !won {
		t.Fatal("version 3 lost the revision fence")
	}
	for _, tuple := range stated {
		t.Logf("version 3 stated %s %s %s %s/%s asserted %t", tuple.FromKind, tuple.FromID, tuple.Kind, tuple.ToKind, tuple.ToID, tuple.Asserted)
		if !tuple.Asserted && (tuple.Kind == graphstore.TupleConstrainedBy || tuple.Kind == graphstore.TupleChangedByRun) {
			t.Errorf("the stale pass over version 3 retracted %s's %s to %s/%s, which no version decides",
				tuple.FromID, tuple.Kind, tuple.ToKind, tuple.ToID)
		}
	}
	requireCurrentOut(t, database.Pool, p1, "mentions", nil, "after the stale pass over version 3")
	requireCurrentOut(t, database.Pool, r1, graphstore.TupleConstrainedBy, constrained, "after the stale pass over version 3")
	for _, block := range []string{r1, p1} {
		requireCurrentOut(t, database.Pool, block, graphstore.TupleChangedByRun,
			[]string{graphstore.KindInteraction + "/" + run}, "after the stale pass over version 3")
	}
}

// requireCurrentOut fails unless the block node address holds exactly the current relations of
// kind want names, each `<to_kind>/<to_id>`, in the store's order.
func requireCurrentOut(t *testing.T, q Queryer, address, kind string, want []string, when string) {
	t.Helper()
	held, err := graphstore.CurrentOut(context.Background(), q, BlockNodeKind, address, []string{kind})
	if err != nil {
		t.Fatalf("%s: read the %s out of %s: %v", when, kind, address, err)
	}
	got := make([]string, len(held))
	for index, tuple := range held {
		got[index] = tuple.ToKind + "/" + tuple.ToID
	}
	if !slices.Equal(got, want) {
		t.Errorf("%s: %s holds the %s %v, want %v", when, address, kind, got, want)
	}
}
