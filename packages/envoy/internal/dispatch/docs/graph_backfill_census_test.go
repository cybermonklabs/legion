package docs

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// backfilledDocuments is how many documents this test seeds. The block index reads twenty owed
// documents a tick (blockIndexDocumentBatch), so more than twenty is a drain of more than one
// tick: the test reports each tick's count, and a change that made the reconciliation stop
// after its first batch would leave documents owed here.
const backfilledDocuments = 25

// nestedSpec is one seeded document: three callouts one inside the other, each holding a
// paragraph, beside a paragraph of the document's own. Its blocks are four deep counting the
// document, so most of them have a block parent rather than the document, which is the parent
// only the block index can state. The innermost paragraph cites CORE-2, so the document holds
// a block reference too and the census reads that kind against rows rather than against
// nothing.
func nestedSpec(slug string) string {
	return ":::::callout{#outer kind=\"note\" title=\"\"}\nOuter of " + slug + ".\n\n" +
		"::::callout{#middle kind=\"note\" title=\"\"}\nMiddle of " + slug + ".\n\n" +
		":::callout{#inner kind=\"note\" title=\"\"}\nInner of " + slug + ", after dispatch://CORE-2.\n:::\n\n::::\n\n:::::\n\n" +
		"Tail of " + slug + ".\n"
}

// blockState is one document_blocks row as this test holds the reconciliation to it: the node
// it projects, the parent it projects, and the version and time its text last changed at.
type blockState struct {
	nodeKind, nodeAddress string
	parent                string
	hasParent             bool
	changedVersion        int
	changedAt             time.Time
}

// A block's parent reaches document_blocks.parent_block_id only through the block index, and
// the census is what says it got there.
//
// The documents here are written as a build before the node and tuple store existed wrote
// them: their rows are in document_blocks, nothing names a node, no block has a parent and no
// tuple says anything about them. Nothing edits them afterwards, so every version stays at 1
// and every block's text stays as it was written.
//
// The run is the whole route, in order:
//
//  1. the backfill writes each block's node and its in_document tuple and marks its document
//     owed a block index, which is how the parent is reached: the reconciliation is the one
//     writer of parent_block_id, and the backfill never writes that column;
//  2. right after it `census --store` refuses, because every block's row says the document
//     holds it directly while its node states no parent at all - the gate is closed before
//     the pass that opens it runs, so a census that passed here would prove nothing;
//  3. the reconciliation drains the mark, tick by tick, and every block's parent_block_id
//     comes to be exactly the block its part_of tuple names;
//  4. the census then reads zero differences in both directions, over every kind;
//  5. and the pass moved no block's changed_version or changed_at: a block that only learned
//     its parent is no block whose text changed.
func TestTheBlockParentCensusReachesZeroAfterTheBackfill(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()

	documents := make([]string, 0, backfilledDocuments)
	for index := range backfilledDocuments {
		slug := fmt.Sprintf("spec%02d", index)
		id := createOwedDocument(t, database, service, "", slug, nestedSpec(slug), "")
		settle(t, database, id)
		documents = append(documents, id)
	}
	// One comment of the same build: a row of another projected kind, so the census reads
	// the comment and the person who wrote it against rows rather than against nothing.
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (issue_key, author, body)
		values ('CORE-1', '{"kind":"user","id":"Alice"}', 'A remark written before the store existed.')
	`); err != nil {
		t.Fatalf("seed the comment: %v", err)
	}
	// The first indexing writes the rows a build before the store would have written. Taking
	// the store back off them is what makes this a pre-store database: the rows stay exactly
	// as that build left them, and nothing in graph_nodes or graph_tuples knows of them.
	ticks := drainBlockIndex(t, service, "seed")
	t.Logf("the seed's own indexing took %d tick(s)", ticks)
	before := blockStates(t, database)
	if len(before) == 0 {
		t.Fatal("the seed indexed no block, so this test would prove nothing")
	}
	takeTheStoreOff(t, database)
	for address, state := range blockStates(t, database) {
		if state.nodeKind != "" || state.hasParent {
			t.Fatalf("block %s still names node %q and parent %q after the store was taken off",
				address, state.nodeKind, state.parent)
		}
	}
	t.Logf("%d blocks over %d documents, written as a build before the store existed",
		len(before), len(documents))

	report, err := graphcensus.Backfill(ctx, database, io.Discard)
	if err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	blocks := backfilledKind(t, report, refs.BlockNodeKind)
	if blocks.Rows != int64(len(before)) || blocks.Owed != int64(len(documents)) || !blocks.Complete {
		t.Fatalf("the backfill wrote %#v, want every one of the %d blocks, %d documents marked owed and the cutover complete",
			blocks, len(before), len(documents))
	}

	// The gate: right after the backfill no block has a parent, and the census says so rather
	// than passing.
	afterBackfill := blockStates(t, database)
	for address, state := range afterBackfill {
		if state.hasParent {
			t.Errorf("block %s has parent %q right after the backfill; the backfill writes no parent_block_id",
				address, state.parent)
		}
		if state.nodeKind != refs.BlockNodeKind || state.nodeAddress != address {
			t.Errorf("block %s names node (%q, %q), want its own address under kind %s",
				address, state.nodeKind, state.nodeAddress, refs.BlockNodeKind)
		}
	}
	gate := takeStoreCensus(t, database)
	gateBlocks := censusOfBlocks(t, gate)
	if !gate.Refused() || gateBlocks.RowDifferences != int64(len(before)) {
		t.Fatalf("the census right after the backfill refused=%t with %d block difference(s), want a refusal naming all %d blocks",
			gate.Refused(), gateBlocks.RowDifferences, len(before))
	}
	t.Logf("the census right after the backfill refuses: %d block difference(s), the first being %q at %s",
		gateBlocks.RowDifferences, gateBlocks.ReportedRows[0].Cause, gateBlocks.ReportedRows[0].At)

	// The reconciliation drains the mark the backfill set.
	owedTicks := drainBlockIndex(t, service, "the backfill's mark")
	var owed int
	if err := database.Pool.QueryRow(ctx,
		`select count(*) from document_block_versions where version is null`).Scan(&owed); err != nil {
		t.Fatalf("count the documents still owed a block index: %v", err)
	}
	if owed != 0 {
		t.Fatalf("%d document(s) still owe a block index after %d tick(s)", owed, owedTicks)
	}

	// Every block's parent_block_id is the block its part_of tuple names now.
	after := blockStates(t, database)
	if len(after) != len(before) {
		t.Fatalf("the pass left %d blocks, want the %d it started with", len(after), len(before))
	}
	for address, state := range after {
		parent, stated := currentBlockParent(t, database, address)
		if !stated {
			t.Errorf("block %s states no parent after the pass", address)
			continue
		}
		if state.hasParent != (parent != "") || state.parent != parent {
			t.Errorf("block %s projects parent %q (set %t), want the %q its part_of tuple names",
				address, state.parent, state.hasParent, parent)
		}
	}

	// The census reads zero differences, in both directions and over every kind.
	passed := takeStoreCensus(t, database)
	for _, kind := range passed.Kinds {
		for _, difference := range append(append([]graphcensus.Difference{},
			kind.ReportedRows...), kind.ReportedNodes...) {
			t.Errorf("the census reads a difference of kind %s at %s: %s",
				kind.Kind, difference.At, difference.Cause)
		}
		t.Logf("census kind=%s rows=%d nodes=%d row differences=%d node differences=%d",
			kind.Kind, kind.Rows, kind.Nodes, kind.RowDifferences, kind.NodeDifferences)
	}
	if passed.Differences() != 0 || passed.Rows() == 0 || passed.Refused() {
		t.Fatalf("the census read %d difference(s) over %d row(s) and refused=%t, want it to pass",
			passed.Differences(), passed.Rows(), passed.Refused())
	}

	// A block that only learned its parent is no block whose text changed.
	for address, state := range after {
		was := before[address]
		if state.changedVersion != was.changedVersion || !state.changedAt.Equal(was.changedAt) {
			t.Errorf("block %s changed at version %d, %s; it was version %d, %s before the pass, and nothing edited it",
				address, state.changedVersion, state.changedAt, was.changedVersion, was.changedAt)
		}
	}

	rows, bytes := tableSize(t, database, "document_blocks")
	t.Logf("document_blocks holds %d rows at pg_total_relation_size %d bytes, against the census limit of %d",
		rows, bytes, pgmigrate.CensusTableLimit)
	if bytes >= pgmigrate.CensusTableLimit {
		t.Fatalf("document_blocks is %d bytes, at or above the census limit of %d: a migration that alters it would be refused",
			bytes, pgmigrate.CensusTableLimit)
	}
}

// drainBlockIndex runs ticks until none indexes a document, and answers how many it took. It
// logs each tick's count, which is what says the drain took more than one batch.
func drainBlockIndex(t *testing.T, service *Service, what string) int {
	t.Helper()
	cursor := ""
	for ticks := 1; ticks <= 64; ticks++ {
		tick := reconcileBlockIndex(t, service, cursor)
		cursor = tick.Cursor.Documents
		t.Logf("%s: tick %d indexed %d document(s), %d owed after it", what, ticks, tick.Documents, tick.OwedDocuments)
		if tick.Documents == 0 && tick.OwedDocuments == 0 {
			return ticks
		}
	}
	t.Fatalf("%s: the block index still owes documents after 64 ticks", what)
	return 0
}

// takeTheStoreOff removes everything the store knows of this database's blocks and leaves the
// rows as a build before the store existed wrote them. It is the test's own fixture and no
// production path: the one thing that writes graph_nodes and graph_tuples is graphstore.
func takeTheStoreOff(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	for _, statement := range []string{
		`delete from graph_tuples where from_kind = 'block'`,
		`delete from graph_tuples where to_kind = 'block'`,
		`update document_blocks set node_kind = null, node_address = null, parent_block_id = null`,
		`update block_refs set node_kind = null, node_address = null`,
		`delete from graph_nodes where kind = 'block'`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("take the store off the seeded rows (%s): %v", statement, err)
		}
	}
}

// blockStates reads every document_blocks row by its node address, which is the address the
// store knows a block by whether or not the row names it yet.
func blockStates(t *testing.T, database *store.Store) map[string]blockState {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select node_id, coalesce(node_kind, ''), coalesce(node_address, ''),
		       parent_block_id, changed_version, changed_at
		from document_blocks
	`)
	if err != nil {
		t.Fatalf("read the blocks: %v", err)
	}
	defer rows.Close()
	states := map[string]blockState{}
	for rows.Next() {
		var address string
		var state blockState
		var parent *string
		if err := rows.Scan(&address, &state.nodeKind, &state.nodeAddress, &parent,
			&state.changedVersion, &state.changedAt); err != nil {
			t.Fatalf("scan a block: %v", err)
		}
		if parent != nil {
			state.parent, state.hasParent = *parent, true
		}
		states[address] = state
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the blocks: %v", err)
	}
	return states
}

// currentBlockParent is the block id the store says holds this block now, read as the store
// reads a current relation (graphstore.CurrentOut): the empty string where the far end is the
// document itself, which document_blocks projects as no parent at all. stated is false where
// the node holds no asserted part_of tuple.
func currentBlockParent(t *testing.T, database *store.Store, address string) (string, bool) {
	t.Helper()
	held, err := graphstore.CurrentOut(context.Background(), database.Pool,
		refs.BlockNodeKind, address, []string{graphstore.TuplePartOf})
	if err != nil {
		t.Fatalf("read the parent of %s: %v", address, err)
	}
	if len(held) != 1 {
		if len(held) > 1 {
			t.Errorf("block %s is part of %d things at once: %#v", address, len(held), held)
		}
		return "", false
	}
	if held[0].ToKind != refs.BlockNodeKind {
		return "", true
	}
	for index := range held[0].ToID {
		if held[0].ToID[index] == '#' {
			return held[0].ToID[index+1:], true
		}
	}
	t.Fatalf("the parent of %s is block %q, which holds no block id", address, held[0].ToID)
	return "", false
}

// takeStoreCensus runs `census --store` over the test's own database, on a connection of its
// own, exactly as the subcommand runs it. It takes no growth pass: that is `--sizing`, and
// nothing here reads it.
func takeStoreCensus(t *testing.T, database *store.Store) *graphcensus.StoreReport {
	t.Helper()
	report, err := graphcensus.StoreCensus(context.Background(), database.Pool.Config().ConnString(), false)
	if err != nil {
		t.Fatalf("take the store census: %v", err)
	}
	return report
}

func censusOfBlocks(t *testing.T, report *graphcensus.StoreReport) graphcensus.KindReport {
	t.Helper()
	for _, kind := range report.Kinds {
		if kind.Kind == refs.BlockNodeKind {
			return kind
		}
	}
	t.Fatalf("the census holds no block kind: %#v", report.Kinds)
	return graphcensus.KindReport{}
}

func backfilledKind(t *testing.T, report graphcensus.BackfillReport, kind string) graphcensus.KindBackfill {
	t.Helper()
	for _, done := range report.Kinds {
		if done.Kind == kind {
			return done
		}
	}
	t.Fatalf("the backfill reports no kind %s: %#v", kind, report.Kinds)
	return graphcensus.KindBackfill{}
}

// tableSize is a table's rows and its total relation size, table, indexes and TOAST, which is
// the figure pgmigrate.CensusTableLimit bounds.
func tableSize(t *testing.T, database *store.Store, name string) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	var rows, bytes int64
	if err := database.Pool.QueryRow(ctx, `select count(*) from `+name).Scan(&rows); err != nil {
		t.Fatalf("count the rows of %s: %v", name, err)
	}
	if err := database.Pool.QueryRow(ctx, `select pg_total_relation_size($1)`, name).Scan(&bytes); err != nil {
		t.Fatalf("read the size of %s: %v", name, err)
	}
	return rows, bytes
}
