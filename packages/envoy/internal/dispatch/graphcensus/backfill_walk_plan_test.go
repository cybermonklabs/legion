package graphcensus

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// The seed of TestTheBackfillsWalksReadOnlyTheirBatch: walkPlanDocuments documents of an issue each,
// with walkPlanBlocksPerDocument block rows naming no node and one block reference naming no node
// out of each block. That is a hundred batches of each walk (backfillBatch, the limit these walks
// are measured at: a run takes its batch from its Throttle, whose default is smaller, so a walk
// that reads only its batch at 200 rows reads only its batch at any batch a run uses), enough that reading
// every row still naming no node, and sorting it, costs the planner more than reading one batch
// through an index in the walk's order, as it does on any deployment's tables.
const (
	walkPlanDocuments         = 40
	walkPlanBlocksPerDocument = 500
)

// walkPlanRead is one of the backfill's walks over a projected table, the index each batch must
// read from its cursor on, and the batch's statement from the walk's start.
type walkPlanRead struct {
	name, table, index string
	statement          explaintest.Statement
}

// TestTheBackfillsWalksReadOnlyTheirBatch holds each of the two walks the backfill makes over a
// table whose order no other index keeps to reading one batch through an index of its own: the
// block rows naming no node, by node_id (BlockRowsStatement, through
// document_blocks_naming_no_node, migration 20261119020900), and the block references naming no
// node, by their key (BlockRefRowsStatement, through block_refs_naming_no_node, migration
// 20261119021000). node_id and from_node are hash indexed, which keeps no order, so without them
// each batch reads every row still naming no node and sorts them, once per batch of the run, which
// on a seeded database of 270,564 block rows cost the block pass 0.66 ms a block against 0.17 ms a
// block over a tenth of them.
//
// The seed is read first, and each walk's first batch must answer the first backfillBatch rows in
// the walk's order. Then, with hash and merge joins off and under each plan-cache mode, every read
// of the table in the analyzed plan must be a probe of the walk's index by its first key, and the
// reads must answer no more than one batch of rows (relationReads). The control is the same batch
// with that index dropped inside the explaining transaction, as the database stood before its
// migration: it must plan, run, and read more than a batch of the table, other than through it.
func TestTheBackfillsWalksReadOnlyTheirBatch(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	pool := database.Pool
	seedWalkPlan(t, pool)

	blocks, err := readBlockRows(ctx, database, "", backfillBatch)
	if err != nil {
		t.Fatalf("read the first block batch: %v", err)
	}
	gotBlocks := make([]string, 0, len(blocks))
	for _, row := range blocks {
		gotBlocks = append(gotBlocks, row.address)
	}
	requireFirstBatch(t, "the block batch", gotBlocks, walkKeys(t, pool, `
		select node_id from document_blocks where node_kind is null order by node_id limit $1`))

	refs, err := readBlockRefRows(ctx, database, []any{"", "", "", ""}, backfillBatch)
	if err != nil {
		t.Fatalf("read the first block reference batch: %v", err)
	}
	gotRefs := make([]string, 0, len(refs))
	for _, row := range refs {
		gotRefs = append(gotRefs, row.address+" "+row.kind+" "+row.toKind+" "+row.toID)
	}
	requireFirstBatch(t, "the block reference batch", gotRefs, walkKeys(t, pool, `
		select from_node || ' ' || kind || ' ' || to_kind || ' ' || to_id from block_refs where node_kind is null
		order by from_node, kind, to_kind, to_id limit $1`))

	keys := explaintest.NewIndexKeys(pool)
	for _, walk := range []walkPlanRead{{
		name:      "the block batch",
		table:     "document_blocks",
		index:     "document_blocks_naming_no_node",
		statement: explaintest.Statement{SQL: BlockRowsStatement, Args: []any{"", backfillBatch, graphstore.NoEvent}},
	}, {
		name:      "the block reference batch",
		table:     "block_refs",
		index:     "block_refs_naming_no_node",
		statement: explaintest.Statement{SQL: BlockRefRowsStatement, Args: []any{"", "", "", "", backfillBatch, graphstore.NoEvent}},
	}} {
		t.Run(walk.index, func(t *testing.T) {
			dropped := []explaintest.Statement{{SQL: "set local lock_timeout = '10s'"}, {SQL: "drop index " + walk.index}}
			for _, mode := range explaintest.PlanCacheModes {
				reads := relationReads(t, keys, explaintest.ExplainAfter(t, ctx, pool, nil, walk.statement, mode, "json"), walk.table, walk.index)
				switch {
				case len(reads.unprobed) > 0 || reads.probeLoops == 0:
					t.Errorf("%s under %s reads %s other than from its cursor through %s:\n  %s\nplan:\n%s", walk.name, mode, walk.table, walk.index,
						strings.Join(slices.Concat(reads.unprobed, reads.probed), "\n  "), explaintest.ExplainAfter(t, ctx, pool, nil, walk.statement, mode, "text"))
				case reads.rows > backfillBatch:
					t.Errorf("%s under %s reads %.0f rows of %s, want one batch, %d at most:\n  %s\nplan:\n%s", walk.name, mode, reads.rows, walk.table,
						backfillBatch, strings.Join(reads.probed, "\n  "), explaintest.ExplainAfter(t, ctx, pool, nil, walk.statement, mode, "text"))
				default:
					t.Logf("%s under %s reads %.0f rows of %s:\n  %s", walk.name, mode, reads.rows, walk.table, strings.Join(reads.probed, "\n  "))
				}

				control := relationReads(t, keys, explaintest.ExplainAfter(t, ctx, pool, dropped, walk.statement, mode, "json"), walk.table, walk.index)
				if len(control.unprobed) == 0 || control.rows <= backfillBatch {
					t.Errorf("%s with %s dropped, the control, reads %.0f rows of %s under %s, so the check cannot tell a read of every row from a batch:\n%s",
						walk.name, walk.index, control.rows, walk.table, mode, explaintest.ExplainAfter(t, ctx, pool, dropped, walk.statement, mode, "text"))
				} else {
					t.Logf("%s with %s dropped under %s reads %.0f rows of %s and fails the check, as the control must:\n  %s",
						walk.name, walk.index, mode, control.rows, walk.table, strings.Join(control.unprobed, "\n  "))
				}
			}
		})
	}
}

// heldBlockRefGuardAsItStood is blockNodeStands over r as it stood before it was a scalar subquery:
// an exists, which the planner pulls up into a semi-join.
const heldBlockRefGuardAsItStood = `exists (select 1 from graph_nodes block
		where block.kind = 'block' and block.id = r.from_node
			and block.state <> 'gone')`

// TestTheHeldBlockRefReadProbesEachRowsBlockNode holds the block-reference batch's read again under
// its locks (HeldBlockRefRowsStatement) to reading graph_nodes and block_refs for the batch's own
// rows alone, however many block nodes the store holds and whatever the statistics say of them.
// The seed is the walk test's, analyzed, and then a live node for every one of its 20,000 blocks,
// written after the analysis as the block pass writes them, so the statistics still count the
// few block nodes there were before: the database as the block-reference pass meets it. The batch
// is the walk's first, its 200 rows' keys bound as the pass binds them. With hash and merge joins
// off and under each plan-cache mode the analyzed plan must answer all 200 rows and read at most
// those 200 rows of each table (relationReads). The control is the read with the guard as it
// stood, an exists: it must plan, run, and read every block node.
//
// Those statistics are the control's whole footing: once graph_nodes is analyzed after the
// write, the planner probes the exists by each row's key too and the control reads 200 rows, so
// the test can no longer tell the two reads apart and fails at the control. The 20,000 rows the
// test writes are past autovacuum's analyze threshold (50 rows and a tenth of the table), and
// autovacuum visits a database once a minute: on a loaded runner its analyze landed during the
// plan loop, after the custom pass, and the control read 200 rows under force_generic_plan alone
// (envoy-go of #4726 at 6aeba5f7, run 45524193041). So autovacuum is off for graph_nodes on this
// test's own database before it is analyzed, and its statistics stay as the write left them.
func TestTheHeldBlockRefReadProbesEachRowsBlockNode(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	pool := database.Pool
	seedWalkPlan(t, pool)
	if _, err := pool.Exec(ctx, `alter table graph_nodes set (autovacuum_enabled = false)`); err != nil {
		t.Fatalf("keep autovacuum from analyzing graph_nodes during the test: %v", err)
	}
	if _, err := pool.Exec(ctx, `analyze graph_nodes`); err != nil {
		t.Fatalf("analyze graph_nodes before the block nodes: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
		select 'block', d.node_id, 'block', 'project', 'live', '{}'::jsonb, now(), '{"kind":"system","id":"walk-plan"}'::jsonb, 0
		from document_blocks d join artifacts a on a.id = d.artifact_id where a.project_key = 'BWK'
	`); err != nil {
		t.Fatalf("write a node for every block: %v", err)
	}
	batch, err := readBlockRefRows(ctx, database, []any{"", "", "", ""}, backfillBatch)
	if err != nil || len(batch) != backfillBatch {
		t.Fatalf("read the first block reference batch: %d rows, %v", len(batch), err)
	}
	keys := make([][]string, 5)
	for _, row := range batch {
		for i, value := range []string{row.artifactID, row.blockID, row.kind, row.toKind, row.toID} {
			keys[i] = append(keys[i], value)
		}
	}
	args := []any{keys[0], keys[1], keys[2], keys[3], keys[4]}
	control := strings.Replace(HeldBlockRefRowsStatement, blockNodeStands("r"), heldBlockRefGuardAsItStood, 1)
	if control == HeldBlockRefRowsStatement {
		t.Fatalf("the control is the statement itself: the guard was not found in it")
	}
	indexKeys := explaintest.NewIndexKeys(pool)
	for _, mode := range explaintest.PlanCacheModes {
		plan := explaintest.ExplainAfter(t, ctx, pool, nil, explaintest.Statement{SQL: HeldBlockRefRowsStatement, Args: args}, mode, "json")
		answered := explaintest.Nodes(t, plan)[0].Rows
		nodes := relationReads(t, indexKeys, plan, "graph_nodes", "graph_nodes_pkey")
		refs := relationReads(t, indexKeys, plan, "block_refs", "block_refs_pkey")
		if answered != backfillBatch || nodes.rows > backfillBatch || refs.rows > backfillBatch {
			t.Errorf("the held read under %s answered %.0f rows and read %.0f of graph_nodes and %.0f of block_refs, want %d, at most %d and at most %d:\n%s",
				mode, answered, nodes.rows, refs.rows, backfillBatch, backfillBatch, backfillBatch,
				explaintest.ExplainAfter(t, ctx, pool, nil, explaintest.Statement{SQL: HeldBlockRefRowsStatement, Args: args}, mode, "text"))
		} else {
			t.Logf("the held read under %s reads %.0f rows of graph_nodes and %.0f of block_refs", mode, nodes.rows, refs.rows)
		}
		stood := relationReads(t, indexKeys, explaintest.ExplainAfter(t, ctx, pool, nil, explaintest.Statement{SQL: control, Args: args}, mode, "json"), "graph_nodes", "graph_nodes_pkey")
		if stood.rows <= backfillBatch {
			t.Errorf("the held read with its guard as it stood, the control, reads %.0f rows of graph_nodes under %s, so this test cannot tell a read of every block node from one per row:\n%s",
				stood.rows, mode, explaintest.ExplainAfter(t, ctx, pool, nil, explaintest.Statement{SQL: control, Args: args}, mode, "text"))
		} else {
			t.Logf("the held read with its guard as it stood under %s reads %.0f rows of graph_nodes, as the control must", mode, stood.rows)
		}
	}
}

// TestTheBackfillAnalyzesTheColumnsItsWalksReadBy holds Backfill to giving the planner statistics
// of node_kind on the three tables it walks (walkedColumnsAnalysis) before its passes: the deploy
// adds those columns after any analysis, and with no statistics of them the planner reads every
// row still naming no node for each batch. The tables each hold a row naming no node, and before
// the run no statistics of the column exist, which is the database as the deploy leaves it; after
// it, each table's node_kind has its statistics. The precondition is the control: a run that does
// not analyze leaves the count at zero.
func TestTheBackfillAnalyzesTheColumnsItsWalksReadBy(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	document := seedPreStoreDocument(t, database, "only")
	if _, err := database.Pool.Exec(ctx, `
		insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version)
		values ($1::uuid, 'only', $2, 'issue', 'CORE-' || 1, 'mentions', 1)
	`, document.artifactID, document.refKey+"#only"); err != nil {
		t.Fatalf("write a block reference naming no node: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (issue_key, author, body)
		select key, '{"kind":"user","id":"alice"}', 'A remark.' from issues where project_key = 'CORE'
	`); err != nil {
		t.Fatalf("write a comment naming no node: %v", err)
	}
	analyzed := func() int {
		var count int
		if err := database.Pool.QueryRow(ctx, `
			select count(*) from pg_stats
			where schemaname = current_schema() and attname = 'node_kind'
			  and tablename in ('document_blocks', 'block_refs', 'comments')
		`).Scan(&count); err != nil {
			t.Fatalf("read the statistics of node_kind: %v", err)
		}
		return count
	}
	if before := analyzed(); before != 0 {
		t.Fatalf("node_kind has statistics on %d of the three tables before the backfill, so this test cannot tell the backfill's analysis from another's", before)
	}
	if _, err := Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if after := analyzed(); after != 3 {
		t.Errorf("node_kind has statistics on %d of the three tables after the backfill, want 3", after)
	}
}

// seedWalkPlan writes the seed TestTheBackfillsWalksReadOnlyTheirBatch reads (the constants above
// say what it holds) and analyzes the tables its walks plan over.
func seedWalkPlan(t *testing.T, pool *store.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, step := range []struct {
		what, sql string
		args      []any
	}{
		{"seed project BWK", `insert into projects (key, name, default_assignee) values ('BWK', 'Backfill walks', 'alice')`, nil},
		{"seed the issues", `
			insert into issues (key, project_key, number, title, created_by, rank)
			select 'BWK-' || i, 'BWK', i, 'Issue ' || i, '{"kind":"user","id":"alice"}', 'U'
			from generate_series(1, $1::int) i`, []any{walkPlanDocuments}},
		{"seed each issue's document", `
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			select key, 'BWK', 'spec', 'spec.md', 'doc', false, '{"kind":"user","id":"alice"}'
			from issues where project_key = 'BWK'`, nil},
		{"write the block rows naming no node", `
			insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
			select a.id, 'b' || k, a.ref_key || '#b' || k, 'paragraph', '', 'Block ' || k, 1, now()
			from artifacts a cross join generate_series(1, $1::int) k
			where a.project_key = 'BWK'`, []any{walkPlanBlocksPerDocument}},
		{"write a block reference naming no node out of each block", `
			insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version)
			select d.artifact_id, d.block_id, d.node_id, 'issue', a.issue_key, 'mentions', 1
			from document_blocks d join artifacts a on a.id = d.artifact_id
			where a.project_key = 'BWK'`, nil},
		{"analyze the walked tables", `analyze document_blocks, block_refs, artifacts, issues`, nil},
	} {
		if _, err := pool.Exec(ctx, step.sql, step.args...); err != nil {
			t.Fatalf("%s: %v", step.what, err)
		}
	}
}

// walkKeys reads query, a select of one text column limited to $1, with backfillBatch for $1.
func walkKeys(t *testing.T, pool *store.Pool, query string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), query, backfillBatch)
	if err != nil {
		t.Fatalf("read the seeded keys: %v", err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the seeded keys: %v", err)
	}
	return keys
}

// requireFirstBatch fails unless got, what batch answered from the walk's start, is want, the first
// backfillBatch rows in the walk's order, and want is a whole batch: a seed of fewer rows would
// leave the plans below about a walk with nothing past its first batch.
func requireFirstBatch(t *testing.T, batch string, got, want []string) {
	t.Helper()
	if len(want) != backfillBatch {
		t.Fatalf("the seed holds %d rows for %s, want at least %d", len(want), batch, backfillBatch)
	}
	if !slices.Equal(got, want) {
		t.Errorf("%s answered %d rows, want the first %d in the walk's order:\n got %q\nwant %q", batch, len(got), len(want), got, want)
	}
}
