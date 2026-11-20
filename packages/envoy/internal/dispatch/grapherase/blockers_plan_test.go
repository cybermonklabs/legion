package grapherase

import (
	"context"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// blockersPlanNodes is how many blocks of the seeded document cite the node under erasure: a plan
// that reads graph_nodes again for each tuple into the node reads it that many times, which is
// what the check counts.
const blockersPlanNodes = 1000

// TestTheBlockersReadProbesEachStatingNodeOnce explains the statement readBlockers sends, the
// erase's read of every live node stating a tuple into the node under erasure, over a block that
// blockersPlanNodes other blocks cite, each with its mentions tuple into the block and its
// block_refs row naming its node, with graph_nodes and graph_tuples analyzed while they held the
// migrations' seed rows alone. The citing blocks are written in the explaining transaction,
// before the read, by the store's own writer (graphstore.QueueWrite) and a block_refs insert.
// With hash and merge joins off, under each plan-cache mode the plan must read the tuples into
// the node through an index and each citing block's node by its own key through
// graph_nodes_pkey, once (explaintest.RequireEachRowProbedOnce).
//
// The statement is the one the read sends, with the arguments it binds (explaintest.RecordedTx),
// and its answer is held to every citing block. The control is that statement with its read of
// the tuples and its join to graph_nodes as they stood before (blockersBefore), which must plan
// and run under both modes and is held to failing under the generic plan, the one it fails
// under: without the node's kind in hand that plan cannot prove graph_tuples_as_of_in's predicate
// from `to_kind = $1`, and read graph_tuples whole. Under the custom plan the planner put the
// tuples outside and probed graph_nodes by key for each, the join's estimates there giving it no
// reason to do otherwise; that reading is recorded beside it.
func TestTheBlockersReadProbesEachStatingNodeOnce(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	pool := database.Pool
	if _, err := pool.Exec(ctx, `analyze graph_nodes, graph_tuples`); err != nil {
		t.Fatalf("analyze the store's tables: %v", err)
	}
	keys := explaintest.NewIndexKeys(pool)
	document := seedPreStoreDocument(t, database)

	const cause = int64(9900002)
	erased := document.refKey + "#erased"
	at := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	who := model.Actor{Kind: "user", ID: "alice"}
	node := func(id string) graphstore.Node {
		return graphstore.Node{
			Kind: refs.BlockNodeKind, ID: id, Supertag: refs.BlockSupertag,
			Visibility: graphstore.VisibilityProject, State: graphstore.StateLive,
			MadeAt: at, MadeBy: who, SourceEvent: cause,
		}
	}
	nodes := []graphstore.Node{node(erased)}
	tuples := make([]graphstore.Tuple, 0, blockersPlanNodes)
	blockIDs := make([]string, 0, blockersPlanNodes)
	addresses := make([]string, 0, blockersPlanNodes)
	for index := range blockersPlanNodes {
		blockID := fmt.Sprintf("b%05d", index)
		address := document.refKey + "#" + blockID
		nodes = append(nodes, node(address))
		tuples = append(tuples, graphstore.Tuple{
			FromKind: refs.BlockNodeKind, FromID: address, Kind: "mentions", ToKind: refs.BlockNodeKind, ToID: erased,
			At: at, Who: who, SourceEvent: cause, Asserted: true,
		})
		blockIDs, addresses = append(blockIDs, blockID), append(addresses, address)
	}
	seed := explaintest.BatchedBy(t, func(tx pgx.Tx) error {
		return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error { return graphstore.QueueWrite(b, nodes, tuples) })
	})
	seed = append(seed, explaintest.Statement{SQL: `
		insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version, node_kind, node_address)
		select $1::uuid, block.id, block.address, $2, $3, 'mentions', 1, $2, block.address
		from unnest($4::text[], $5::text[]) as block(id, address)
	`, Args: []any{document.artifactID, refs.BlockNodeKind, erased, blockIDs, addresses}})

	read := blockersStatement(t, ctx, database, seed, refs.BlockNodeKind, erased)
	keyed := map[string]string{"graph_nodes": "graph_nodes_pkey", "graph_tuples": explaintest.AnyIndex}
	explaintest.RequireEachRowProbedOnce(t, ctx, pool, keys, keyed, "the blockers read", seed, read)
	explaintest.RequireTheCheckFails(t, ctx, pool, keys, keyed, "the blockers read as it stood",
		[]string{"force_generic_plan"}, seed,
		explaintest.Statement{SQL: explaintest.Spliced(t, read.SQL, blockersBefore(t, read.SQL)...), Args: read.Args})
}

// blockersStatement runs readBlockers over kind:id after seed, in a transaction it rolls back,
// holds its answer to every citing block the seed wrote, and answers the statement it sent.
func blockersStatement(t *testing.T, ctx context.Context, database *store.Store, seed []explaintest.Statement, kind, id string) explaintest.Statement {
	t.Helper()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the read: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, statement := range seed {
		if _, err := tx.Exec(ctx, statement.SQL, statement.Args...); err != nil {
			t.Fatalf("seed the blocks citing %s:%s: %v", kind, id, err)
		}
	}
	recorded := &explaintest.RecordedTx{Tx: tx}
	erasure := &Erasure{Kind: kind, ID: id}
	var read erasedRead
	if err := erasure.readBlockers(ctx, recorded, &read); err != nil {
		t.Fatalf("read the blockers of %s:%s: %v", kind, id, err)
	}
	if len(erasure.Blockers) != blockersPlanNodes {
		t.Fatalf("the read names %d blockers of %s:%s, want the %d blocks the seed wrote citing it",
			len(erasure.Blockers), kind, id, blockersPlanNodes)
	}
	return recorded.Holding(t, "graph_nodes n")
}

// blockersBefore is readBlockers' read of the tuples into the node and its join to graph_nodes as
// they stood before its probe, written with the constants the read is written with and the
// parameter sql, the statement the read sends now, binds the state at: the tuples read naming no
// `to_kind <> 'value'`, and joined to graph_nodes plainly, put back in place of the probe of each
// stating node and of the test of its state.
func blockersBefore(t *testing.T, sql string) []explaintest.Part {
	t.Helper()
	state := explaintest.Parameter(t, sql, regexp.MustCompile(`where n\.state = (\$[0-9]+) and`))
	return []explaintest.Part{
		{Name: "the read's test that a value is no node", Opens: " and t.to_kind <> '" + graphstore.KindValue + "'", Closes: "and not (", Text: " "},
		{
			Name: "the join to graph_nodes", Opens: "cross join lateral (", Closes: "t.asserted and (",
			Text: "join graph_nodes n on n.kind = t.from_kind and n.id = t.from_id and n.state = " + state + "\n\t\twhere ",
		},
	}
}
