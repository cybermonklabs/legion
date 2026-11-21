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
// out of the inner block, one comment that the document owns and is anchored to, written by a
// person, one comment on the issue written by a session, and the issue routed to a role. outer and
// inner are the two blocks' node addresses, comment the person's comment's id, and mention the
// block reference's row as the census names it.
type agreeingStore struct {
	document              preStoreDocument
	outer, inner, comment string
	mention               string
}

// agreeingSession and agreeingRole are the session that wrote the agreeing store's second
// comment and the role its issue is routed to: they give the census's session, role and page
// kinds rows of their own, so the store the census agrees with is one it passes.
const (
	agreeingSession = "census-agreeing-session"
	agreeingRole    = "census-agreeing-role"
)

// censusWriter is who this file's own fixture writes are made by.
var censusWriter = backfillActor

// seedAgreeingStore writes the rows as a build before the store existed wrote them, runs the
// backfill over them, and states each block's parent as the block index states it, which is the
// one fact the backfill leaves to the index (Backfill). This package cannot import the block
// index (docs imports it), so the parent is stated here with the tuples the block index plans
// (refs' planBlockParents) and the column it projects them into (queueBlockParents): the outer
// block's part_of to the document, the inner block's to the outer block, and the inner row's
// parent_block_id beside it.
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
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (issue_key, author, body)
		values ('CORE-1', jsonb_build_object('kind', 'session', 'id', $1::text), 'A session''s remark on the issue.')
	`, agreeingSession); err != nil {
		t.Fatalf("write the session's comment on the issue: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `update issues set route = 'role:' || $1 where key = 'CORE-1'`, agreeingRole); err != nil {
		t.Fatalf("route the issue to role %s: %v", agreeingRole, err)
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

// seededKinds are the kinds seedAgreeingStore writes rows of, which are every earlier kind the
// census reads, each of which must count rows and live nodes: a kind
// that read zero of either compared nothing, and its zero differences would prove nothing. The
// tie kinds of decisions, readings and asks (KindReport.EmptyAgrees) are none of them: the fixture
// writes no decision and no ask, and those kinds read empty on both sides, which is no refusal.
var seededKinds = []string{"comment", "block", "block_ref", graphstore.KindPerson, graphstore.KindSession,
	graphstore.KindRole, graphstore.KindPage}

// requireTheCensusAgrees fails the test unless the census read no difference in either direction
// over any kind, read rows and live nodes of every kind the fixture seeded, and passes.
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
	if report.Differences() != 0 || report.Refused() {
		t.Fatalf("%s read %d difference(s) and refused=%t, want none and a pass", what, report.Differences(), report.Refused())
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

// The census passes only where it checked more than zero rows of every kind it covers and found
// zero differences (R19): a total over the kinds is no such proof, because two empty sides of one
// kind agree. The census of the agreeing store, which checked rows of every earlier kind,
// passes. The same report with any one of those kinds' rows and nodes taken
// out, which still reads zero differences and still holds rows of every other kind, must refuse
// and name that kind in its verdict. A tie kind of decisions, readings and asks is a kind a
// deployment holds none of until somebody writes one, and the agreeing store holds none: no row
// and no node of it
// is no refusal, and the report says so on that kind's own line.
func TestTheStoreCensusRefusesAKindThatCheckedNoRow(t *testing.T) {
	database := storetest.Open(t)
	seedAgreeingStore(t, database)
	report := storeCensusOf(t, database)
	if report.Refused() {
		t.Fatalf("the census of the agreeing store refuses, so taking a kind out of it would prove nothing")
	}
	for index, held := range report.Kinds {
		emptied := *report
		emptied.Kinds = append([]KindReport{}, report.Kinds...)
		emptied.Kinds[index].Rows, emptied.Kinds[index].Nodes = 0, 0
		var printed strings.Builder
		emptied.Write(&printed)
		if held.EmptyAgrees {
			side := "node"
			if held.Triples {
				side = "tuple"
			}
			line := "census: store kind=" + held.Kind + " holds no row and no " + side + ": nothing of it is written yet, which is no refusal\n"
			if emptied.Refused() || !strings.Contains(printed.String(), line) || !strings.HasSuffix(printed.String(), "census: store ok\n") {
				t.Errorf("a census whose %s kind holds no row and no node reads refused=%t, want a pass printing %q:\n%s",
					held.Kind, emptied.Refused(), line, printed.String())
			}
			continue
		}
		verdict := "census: store REFUSED (kind(s) " + held.Kind + " checked no row, so the census proves nothing of them)\n"
		if !emptied.Refused() || emptied.Rows() == 0 || !strings.HasSuffix(printed.String(), verdict) {
			t.Errorf("a census that checked no %s row but %d rows of the other kinds reads refused=%t, want a refusal ending %q:\n%s",
				held.Kind, emptied.Rows(), emptied.Refused(), verdict, printed.String())
		}
	}
}

// The census reads one snapshot: every kind it reports is read in the one repeatable-read
// transaction it opens, so a write another connection commits while it runs reaches none of
// its reads after the first.
//
// The write is placed between two of its reads rather than raced against them. The census's
// own hook (storeCensus's afterKind) runs once the comment kind has been read, which is after
// the census's first statement, the one its snapshot is taken at, and there the test commits, on
// a connection of its own, a live block node no document_blocks row projects. The block kind is
// read next, and a read under a fresh snapshot would count that node as a live node no row
// projects. The census reads zero differences, because its block read answers the snapshot its
// first statement took; a census taken afterwards reads the node, which is what says the write
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
