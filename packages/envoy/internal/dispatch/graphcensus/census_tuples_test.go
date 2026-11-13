package graphcensus

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// deleteTuples takes out of the log every tuple of one relation out of one node, as a writer
// that never stated it would have left the log. It is this test's own fixture: no production
// path deletes a tuple but the erase.
func deleteTuples(t *testing.T, database *store.Store, fromKind, fromID, kind string) {
	t.Helper()
	tag, err := database.Pool.Exec(context.Background(), `
		delete from graph_tuples where from_kind = $1 and from_id = $2 and kind = $3
	`, fromKind, fromID, kind)
	if err != nil || tag.RowsAffected() == 0 {
		t.Fatalf("delete the %s tuples out of %s %s: %d rows, %v", kind, fromKind, fromID, tag.RowsAffected(), err)
	}
}

// appendTuple states one more fact through the store's own writer: a retraction where asserted
// is false, which is how the store ends a relation.
func appendTuple(t *testing.T, database *store.Store, tuple graphstore.Tuple) {
	t.Helper()
	ctx := context.Background()
	tuple.At, tuple.Who, tuple.SourceEvent = time.Now().UTC(), censusWriter, graphstore.NoEvent
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the tuple's write: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := graphstore.Write(ctx, tx, nil, []graphstore.Tuple{tuple}); err != nil {
		t.Fatalf("append %s %s %s -> %s %s: %v", tuple.FromKind, tuple.FromID, tuple.Kind, tuple.ToKind, tuple.ToID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the tuple's write: %v", err)
	}
}

// The census holds each row of the three projected kinds to the tuples that answer it, and not
// to its node row alone. Once a kind's cutover is complete, graph_edges, refs.Edges and
// refs.BacklinkCounts answer its links from the tuples, so a tuple that is missing or retracted
// takes a link out of every read while every node row still stands: a census of the node rows
// alone reads zero differences over a graph that has lost edges.
//
// Each case starts from a store the census agrees with (seedAgreeingStore), breaks one link of
// one row in the store alone, and requires the census to read exactly that row as a difference
// of its kind, and nothing anywhere else. The pairs are the ones each row states: a block's
// in_document to its document, a comment's owned_by and anchored_to to the artifacts its
// artifact_id and its anchor name and its authored_by to the person graphstore.AuthorOf reads
// off its author, and a block reference's own link under its kind to its far end. The last case
// is the other direction: a link the store states out of a block that no row of that block
// names.
func TestTheStoreCensusHoldsEachRowToTheTuplesThatAnswerIt(t *testing.T) {
	for _, test := range []struct {
		name string
		// kind is the census kind that reads the difference, at the row at names; lose breaks
		// the one link in the store alone.
		kind string
		at   func(seeded agreeingStore) string
		lose func(t *testing.T, database *store.Store, seeded agreeingStore)
	}{{
		name: "a block whose in_document tuple is missing",
		kind: "block",
		at:   func(seeded agreeingStore) string { return "block " + seeded.outer },
		lose: func(t *testing.T, database *store.Store, seeded agreeingStore) {
			deleteTuples(t, database, refs.BlockNodeKind, seeded.outer, refs.TupleInDocument)
		},
	}, {
		name: "a comment whose owned_by is retracted",
		kind: "comment",
		at:   func(seeded agreeingStore) string { return "comment " + seeded.comment },
		lose: func(t *testing.T, database *store.Store, seeded agreeingStore) {
			appendTuple(t, database, graphstore.Tuple{
				FromKind: commentNodeKind, FromID: seeded.comment, Kind: tupleOwnedBy,
				ToKind: "artifact", ToID: seeded.document.refKey, Asserted: false,
			})
		},
	}, {
		name: "a comment whose anchored_to is retracted",
		kind: "comment",
		at:   func(seeded agreeingStore) string { return "comment " + seeded.comment },
		lose: func(t *testing.T, database *store.Store, seeded agreeingStore) {
			appendTuple(t, database, graphstore.Tuple{
				FromKind: commentNodeKind, FromID: seeded.comment, Kind: tupleAnchoredTo,
				ToKind: "artifact", ToID: seeded.document.refKey, Asserted: false,
			})
		},
	}, {
		name: "a comment whose authored_by is retracted",
		kind: "comment",
		at:   func(seeded agreeingStore) string { return "comment " + seeded.comment },
		lose: func(t *testing.T, database *store.Store, seeded agreeingStore) {
			appendTuple(t, database, graphstore.Tuple{
				FromKind: commentNodeKind, FromID: seeded.comment, Kind: graphstore.TupleAuthoredBy,
				ToKind: graphstore.KindPerson, ToID: "alice", Asserted: false,
			})
		},
	}, {
		name: "a block reference whose link tuple is missing",
		kind: "block_ref",
		at:   func(seeded agreeingStore) string { return seeded.mention },
		lose: func(t *testing.T, database *store.Store, seeded agreeingStore) {
			deleteTuples(t, database, refs.BlockNodeKind, seeded.inner, "mentions")
		},
	}, {
		name: "a block's link that no block reference row names",
		kind: "block_ref",
		at:   func(seeded agreeingStore) string { return seeded.mention },
		lose: func(t *testing.T, database *store.Store, seeded agreeingStore) {
			appendTuple(t, database, graphstore.Tuple{
				FromKind: refs.BlockNodeKind, FromID: seeded.inner, Kind: "mentions",
				ToKind: "issue", ToID: "CORE-2", Asserted: true,
			})
		},
	}} {
		t.Run(test.name, func(t *testing.T) {
			database := storetest.Open(t)
			seeded := seedAgreeingStore(t, database)
			test.lose(t, database, seeded)
			report := storeCensusOf(t, database)
			wantAt := test.at(seeded)
			for _, kind := range report.Kinds {
				t.Logf("kind=%s rows=%d nodes=%d row differences=%d node differences=%d %+v",
					kind.Kind, kind.Rows, kind.Nodes, kind.RowDifferences, kind.NodeDifferences, kind.ReportedRows)
				if kind.Kind != test.kind {
					if kind.RowDifferences != 0 || kind.NodeDifferences != 0 {
						t.Errorf("%s: the census reads %d row and %d node difference(s) of kind %s (%+v %+v), want none there",
							test.name, kind.RowDifferences, kind.NodeDifferences, kind.Kind, kind.ReportedRows, kind.ReportedNodes)
					}
					continue
				}
				if kind.RowDifferences != 1 || len(kind.ReportedRows) != 1 || kind.ReportedRows[0].At != wantAt ||
					!strings.HasPrefix(kind.ReportedRows[0].Cause, "the row and its node disagree") || kind.NodeDifferences != 0 {
					t.Fatalf("%s: the census reads %d row difference(s) %+v and %d node difference(s) of kind %s, want the one at %s, whose node and row disagree: the census does not hold that row to the tuple that answers it",
						test.name, kind.RowDifferences, kind.ReportedRows, kind.NodeDifferences, kind.Kind, wantAt)
				}
				t.Logf("%s: the census reads kind %s at %s: %s", test.name, kind.Kind, kind.ReportedRows[0].At, kind.ReportedRows[0].Cause)
			}
			if !report.Refused() {
				t.Errorf("%s: the census does not refuse a store that lost a link", test.name)
			}
		})
	}
}
