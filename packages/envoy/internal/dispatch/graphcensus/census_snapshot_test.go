package graphcensus

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// agreeingStore is a database whose store and projections agree on every kind the census reads
// rows of: one document of two blocks, the inner one inside the outer one, one block reference
// out of the inner block, and one comment that the document owns and is anchored to, written by
// a person. outer and inner are the two blocks' node addresses, comment the comment's id, and
// mention the block reference's row as the census names it.
type agreeingStore struct {
	document              preStoreDocument
	outer, inner, comment string
	mention               string
}

// censusWriter is who this file's own fixture writes are made by.
var censusWriter = backfillActor

// seedAgreeingStore writes the rows as a build before the store existed wrote them, runs the
// backfill over them, and states each block's parent as the block index states it, which is the
// one fact the backfill leaves to the index (Backfill). This package cannot import the block
// index (docs imports it), so the parent is stated here with the tuples and the column the
// reconciliation writes (refs' reconcileBlockParents): the outer block's part_of to the
// document, the inner block's to the outer block, and the inner row's parent_block_id beside it.
//
// It then takes the census and fails unless it reads zero differences over rows of every kind
// it seeded, so a test that breaks one fact afterwards starts from a census that agreed.
func seedAgreeingStore(t *testing.T, database *store.Store) agreeingStore {
	t.Helper()
	ctx := context.Background()
	document := seedPreStoreDocument(t, database, "outer", "inner")
	seeded := agreeingStore{
		document: document,
		outer:    document.refKey + "#outer",
		inner:    document.refKey + "#inner",
	}
	seeded.mention = "block_ref " + seeded.inner + " mentions -> issue CORE-1"
	if _, err := database.Pool.Exec(ctx, `
		insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version)
		values ($1::uuid, 'inner', $2, 'issue', 'CORE-1', 'mentions', 1)
	`, document.artifactID, seeded.inner); err != nil {
		t.Fatalf("write the inner block's reference to CORE-1: %v", err)
	}
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (artifact_id, author, body, anchor)
		values ($1::uuid, '{"kind":"user","id":"alice"}', 'A remark on the spec.', jsonb_build_object('artifact_id', $1::text))
		returning id::text
	`, document.artifactID).Scan(&seeded.comment); err != nil {
		t.Fatalf("write the comment on the document: %v", err)
	}
	if _, err := Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the parents' write: %v", err)
	}
	defer tx.Rollback(ctx)
	now := time.Now().UTC()
	parent := func(address, toKind, toID string) graphstore.Tuple {
		return graphstore.Tuple{
			FromKind: refs.BlockNodeKind, FromID: address, Kind: graphstore.TuplePartOf,
			ToKind: toKind, ToID: toID, At: now, Who: censusWriter, SourceEvent: graphstore.NoEvent, Asserted: true,
		}
	}
	if err := graphstore.Write(ctx, tx, nil, []graphstore.Tuple{
		parent(seeded.outer, "artifact", document.refKey),
		parent(seeded.inner, refs.BlockNodeKind, seeded.outer),
	}); err != nil {
		t.Fatalf("state the blocks' parents: %v", err)
	}
	if tag, err := tx.Exec(ctx, `
		update document_blocks set parent_block_id = 'outer' where artifact_id = $1::uuid and block_id = 'inner'
	`, document.artifactID); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("project the inner block's parent: %d rows, %v", tag.RowsAffected(), err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the parents' write: %v", err)
	}
	requireTheCensusAgrees(t, "the census of the seeded store", storeCensusOf(t, database))
	return seeded
}

// storeCensusOf takes the store census over database as `envoy-dispatch census --store` takes
// it, on a connection of its own.
func storeCensusOf(t *testing.T, database *store.Store) *StoreReport {
	t.Helper()
	report, err := StoreCensus(context.Background(), database.Pool.Config().ConnString(), false)
	if err != nil {
		t.Fatalf("take the store census: %v", err)
	}
	return report
}

// seededKinds are the kinds seedAgreeingStore writes rows of, each of which must count rows and
// live nodes: a kind that read zero of either compared nothing, and its zero differences would
// prove nothing.
var seededKinds = []string{"comment", "block", "block_ref", graphstore.KindPerson}

// requireTheCensusAgrees fails the test unless the census read no difference in either direction
// over any kind, and read rows and live nodes of every kind the fixture seeded.
func requireTheCensusAgrees(t *testing.T, what string, report *StoreReport) {
	t.Helper()
	for _, kind := range report.Kinds {
		t.Logf("%s: kind=%s rows=%d nodes=%d row differences=%d node differences=%d",
			what, kind.Kind, kind.Rows, kind.Nodes, kind.RowDifferences, kind.NodeDifferences)
		for _, difference := range append(append([]Difference{}, kind.ReportedRows...), kind.ReportedNodes...) {
			t.Errorf("%s reads a difference of kind %s at %s: %s", what, kind.Kind, difference.At, difference.Cause)
		}
	}
	for _, name := range seededKinds {
		if kind := censusKind(t, report, name); kind.Rows == 0 || kind.Nodes == 0 {
			t.Errorf("%s read %d rows and %d live nodes of kind %s, which the fixture seeded: it compared nothing there",
				what, kind.Rows, kind.Nodes, name)
		}
	}
	if report.Differences() != 0 {
		t.Fatalf("%s read %d difference(s), want none", what, report.Differences())
	}
}

// censusKind is the report's line for one kind.
func censusKind(t *testing.T, report *StoreReport, name string) KindReport {
	t.Helper()
	for _, kind := range report.Kinds {
		if kind.Kind == name {
			return kind
		}
	}
	t.Fatalf("the census reported no kind %s", name)
	return KindReport{}
}

// The census reads one snapshot: every kind it reports is read in the one repeatable-read
// transaction it opens, so a write another connection commits while it runs reaches none of
// its reads after the first.
//
// The write is placed between two of its reads rather than raced against them. The census's
// own hook (storeCensus's afterKind) runs once the comment kind has been read, which is the
// census's first statement and the one its snapshot is taken at, and there the test commits, on
// a connection of its own, a live block node no document_blocks row projects. The block kind is
// read next, and a read under a fresh snapshot would count that node as a live node no row
// projects. The census reads zero differences, because its block read answers the snapshot its
// comment read took; a census taken afterwards reads the node, which is what says the write
// committed and the census would have counted it.
func TestTheStoreCensusReadsOneSnapshot(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	seeded := seedAgreeingStore(t, database)
	ghost := seeded.document.refKey + "#ghost"
	committedAfter := ""
	report, err := storeCensus(ctx, database.Pool.Config().ConnString(), false, func(ctx context.Context, kind string) error {
		if committedAfter != "" {
			return nil
		}
		committedAfter = kind
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if err := graphstore.Write(ctx, tx, []graphstore.Node{{
			Kind: refs.BlockNodeKind, ID: ghost, Supertag: refs.BlockSupertag,
			Visibility: graphstore.VisibilityProject, State: graphstore.StateLive,
			MadeAt: time.Now().UTC(), MadeBy: censusWriter, SourceEvent: graphstore.NoEvent,
		}}, nil); err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
	if err != nil {
		t.Fatalf("take the store census with a write committed between its reads: %v", err)
	}
	if committedAfter != "comment" {
		t.Fatalf("the census's hook first ran after kind %q, want after comment, its first read", committedAfter)
	}
	t.Logf("a live block node %s no row projects was committed on another connection after the census read kind %s and before it read kind block",
		ghost, committedAfter)
	blocks := censusKind(t, report, "block")
	if blocks.NodeDifferences != 0 || report.Differences() != 0 {
		t.Fatalf("the census read %d block node difference(s) (%+v) and %d in all after a write committed between its read of comment and its read of block: its reads did not all answer one snapshot",
			blocks.NodeDifferences, blocks.ReportedNodes, report.Differences())
	}
	requireTheCensusAgrees(t, "the census with a write committed between its reads", report)

	// The positive control: the same census taken afterwards reads the node the write committed,
	// so the zero above is the snapshot's and not a census blind to such a node.
	after := censusKind(t, storeCensusOf(t, database), "block")
	wantAt := "block " + ghost
	if after.NodeDifferences != 1 || len(after.ReportedNodes) != 1 || after.ReportedNodes[0].At != wantAt ||
		!strings.HasPrefix(after.ReportedNodes[0].Cause, "a live node no row projects") {
		t.Fatalf("the census taken after the write read block node differences %d %+v, want the one at %s: a live node no row projects",
			after.NodeDifferences, after.ReportedNodes, wantAt)
	}
	t.Logf("the census taken after the write reads it: kind block, %s: %s", after.ReportedNodes[0].At, after.ReportedNodes[0].Cause)
}
