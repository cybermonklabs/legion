package refs

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// heldDocumentPlanBlocks is how many blocks the document under test holds in the store: a plan
// that reads graph_nodes again for each member reads it that many times, which is what the
// check counts, and the control, which runs that plan, still finishes in a second.
const heldDocumentPlanBlocks = 1000

// TestTheHeldDocumentReadProbesEachMembersNodeOnce explains heldDocumentBlocks, the store's
// statement of a document that a stale block index pass reads, over a document of
// heldDocumentPlanBlocks block nodes, each a member of the document and stating one link, with
// graph_nodes and graph_tuples analyzed while they held the migrations' seed rows alone: the
// planner then counts no block node and no membership, as it does for a document whose blocks
// the statistics have not reached yet. The blocks are written in the explaining transaction,
// before the read, by the store's own writer (graphstore.QueueWrite). With hash and merge joins
// off, under each plan-cache mode the plan must read each member's node by its own key through
// graph_nodes_pkey, once (explaintest.RequireEachRowProbedOnce).
//
// The statement is the one the read sends, with the argument it binds (explaintest.RecordedTx).
// The control is that statement with its join to graph_nodes as it stood before
// (heldDocumentJoinBefore), which the planner served with `kind = 'block'` for the index's
// condition, reading every block node again for each member: it must plan and run, and the check
// must fail on it under both modes.
func TestTheHeldDocumentReadProbesEachMembersNodeOnce(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	pool := database.Pool
	if _, err := pool.Exec(ctx, `analyze graph_nodes, graph_tuples`); err != nil {
		t.Fatalf("analyze the store's tables: %v", err)
	}
	keys := explaintest.NewIndexKeys(pool)

	const refKey, cause = "PLAN-1/spec", int64(9900001)
	at := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	who := model.Actor{Kind: "user", ID: "alice"}
	nodes := make([]graphstore.Node, 0, heldDocumentPlanBlocks)
	tuples := make([]graphstore.Tuple, 0, 2*heldDocumentPlanBlocks)
	for index := range heldDocumentPlanBlocks {
		address := blockNodeAddress(refKey, fmt.Sprintf("b%05d", index))
		node := blockNode(address, IndexedVersion{RefKey: refKey, Writer: who}, at, graphstore.StateLive)
		node.SourceEvent = cause
		nodes = append(nodes, node)
		tuples = append(tuples,
			graphstore.Tuple{
				FromKind: BlockNodeKind, FromID: address, Kind: TupleInDocument, ToKind: "artifact", ToID: refKey,
				At: at, Who: who, SourceEvent: cause, Asserted: true,
			},
			graphstore.Tuple{
				FromKind: BlockNodeKind, FromID: address, Kind: "mentions", ToKind: "issue", ToID: "PLAN-2",
				At: at, Who: who, SourceEvent: cause, Asserted: true,
			})
	}
	seed := explaintest.BatchedBy(t, func(tx pgx.Tx) error {
		return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			graphstore.QueueWrite(b, nodes, tuples)
			return nil
		})
	})
	read := heldDocumentStatement(t, ctx, database, seed, refKey)
	keyed := map[string]string{"graph_nodes": "graph_nodes_pkey", "graph_tuples": explaintest.AnyIndex}
	explaintest.RequireEachRowProbedOnce(t, ctx, pool, keys, keyed, "the held document read", seed, read)
	explaintest.RequireTheCheckFails(t, ctx, pool, keys, keyed, "the held document read as it stood",
		explaintest.PlanCacheModes, seed,
		explaintest.Statement{SQL: explaintest.Spliced(t, read.SQL, heldDocumentJoinBefore...), Args: read.Args})
}

// heldDocumentStatement runs heldDocumentBlocks over refKey after seed, in a transaction it rolls
// back, holds its answer to every block the seed wrote, and answers the statement it sent.
func heldDocumentStatement(t *testing.T, ctx context.Context, database *store.Store, seed []explaintest.Statement, refKey string) explaintest.Statement {
	t.Helper()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the read: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, statement := range seed {
		if _, err := tx.Exec(ctx, statement.SQL, statement.Args...); err != nil {
			t.Fatalf("seed the document's blocks: %v", err)
		}
	}
	recorded := &explaintest.RecordedTx{Tx: tx}
	held, err := heldDocumentBlocks(ctx, recorded, refKey)
	if err != nil {
		t.Fatalf("read what the store states of %s: %v", refKey, err)
	}
	if len(held) != heldDocumentPlanBlocks {
		t.Fatalf("the read answered %d blocks of %s, want the %d the seed wrote", len(held), refKey, heldDocumentPlanBlocks)
	}
	return recorded.Holding(t, "'"+TupleInDocument+"'")
}

// heldDocumentJoinBefore is heldDocumentBlocks' join to graph_nodes as it stood before
// its probe, written with the constants the read is written with, put back into the statement
// the read sends now in place of its probe of each member's node and of the test of that node's
// state after it: the members joined to graph_nodes plainly, under that same test.
var heldDocumentJoinBefore = []explaintest.Part{
	{
		Name: "the join to graph_nodes", Opens: "cross join lateral (", Closes: "left join lateral (",
		Text: "join graph_nodes n on n.kind = '" + BlockNodeKind + "' and n.id = member.id\n\t\t\tand n.state <> '" +
			graphstore.StateGone + "'\n\t\t",
	},
	{Name: "the test of each member's state", Opens: "where node.state <> ", Closes: "order by member.id"},
}
