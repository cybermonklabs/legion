package graphcensus

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// unmarkedAskKinds are the ask tie kinds whose rows the earlier release
// wrote with no node, in the order the census reads them.
var unmarkedAskKinds = []string{graphstore.TupleFollowedBy, "ask_parent", "ask_anchor", "ask_answer"}

// The rule that a tie kind holding no row and no node is no refusal is for a
// store nothing of that kind was written to. An asks table and an ask_followers table whose rows
// the earlier release wrote, and which no backfill has marked, hold rows all the same: their kinds
// read no row and no node, since the row side reads marked rows alone, while each counts its own
// unmarked rows. Over the agreeing store and one such ask - an ask block anchored to the document
// and answered by bob - and one follower row of it, every ask kind counts its unmarked row, none
// prints that nothing of it is written yet, each prints that it checked no row, and the census
// refuses with those four kinds as the kinds that checked no row. (The project census refuses the
// unmarked asks row as well, as a row with no node; that refusal is the project census's own and
// not this rule's, so the test reads the kinds the census names unpopulated.)
func TestTheCensusRefusesAskRowsNoBackfillMarked(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedAgreeingStore(t, database)
	var ask string
	if err := database.Pool.QueryRow(ctx, `
		insert into asks (issue_key, author, question, anchor, block_artifact_id, block_id, state, answer)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'Ship it this week?', jsonb_build_object('artifact_id', $1::text),
		        $1::uuid, 'a1', 'answered',
		        jsonb_build_object('user', 'bob', 'text', 'Yes.', 'selected', '[]'::jsonb, 'at', now()))
		returning id::text
	`, seeded.document.artifactID).Scan(&ask); err != nil {
		t.Fatalf("write the earlier release's ask: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `insert into ask_followers (ask_id, session_id) values ($1::uuid, $2)`,
		ask, agreeingSession); err != nil {
		t.Fatalf("write the earlier release's follower row: %v", err)
	}

	report := storeCensusOf(t, database)
	var printed strings.Builder
	report.Write(&printed)
	t.Logf("the census over the unmarked ask:\n%s", printed.String())
	for _, name := range unmarkedAskKinds {
		kind := censusKind(t, report, name)
		if kind.Rows != 0 || kind.Nodes != 0 || kind.Unmarked != 1 || kind.Empty() {
			t.Errorf("kind %s reads %d rows, %d tuples and %d unmarked row(s), empty=%t, want no row, no tuple, the one unmarked row and a kind that is not empty",
				name, kind.Rows, kind.Nodes, kind.Unmarked, kind.Empty())
		}
		if nothing := "census: store kind=" + name + " holds no row and no "; strings.Contains(printed.String(), nothing) {
			t.Errorf("the census prints that nothing of kind %s is written yet, over a table holding a row of it", name)
		}
		if checked := "census: store kind=" + name + " checked no row, so the census proves nothing of it\n"; !strings.Contains(printed.String(), checked) {
			t.Errorf("the census prints no line %q", checked)
		}
	}
	if unpopulated := report.Unpopulated(); !report.Refused() || report.Differences() != 0 ||
		strings.Join(unpopulated, ", ") != strings.Join(unmarkedAskKinds, ", ") {
		t.Fatalf("the census over rows no backfill marked reads refused=%t, %d difference(s) and the kinds %v unpopulated, want a refusal naming %v",
			report.Refused(), report.Differences(), unpopulated, unmarkedAskKinds)
	}
}

// statedMovedTo appends one moved_to tuple from the block address from to the block address to,
// as the restore and the conversion state one.
func statedMovedTo(t *testing.T, database *store.Store, from, to string) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the moved_to's write: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := graphstore.Write(ctx, tx, nil, []graphstore.Tuple{{
		FromKind: refs.BlockNodeKind, FromID: from, Kind: graphstore.TupleMovedTo, ToKind: refs.BlockNodeKind, ToID: to,
		At: time.Now().UTC(), Who: censusWriter, SourceEvent: graphstore.NoEvent, Asserted: true,
	}}); err != nil {
		t.Fatalf("state the moved_to from %s to %s: %v", from, to, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the moved_to's write: %v", err)
	}
}

// storeOnlyKind is the report's store-only line for one kind.
func storeOnlyKind(t *testing.T, report *StoreReport, name string) StoreOnlyReport {
	t.Helper()
	for _, kind := range report.StoreOnly {
		if kind.Kind == name {
			return kind
		}
	}
	t.Fatalf("the census reported no store-only kind %s", name)
	return StoreOnlyReport{}
}

// The census leaves out documents that still owe their block index: a
// moved_to whose far end is a block of such a document, which holds no row yet for a block its
// latest version holds, is counted apart and is no unresolved far end. The agreeing store's
// document owes its index, since its version was written with no block index, and a moved_to into
// a block of it no row holds reads left_out=1, unresolved=0, and passes. Once the document's block
// index stands at that version, the same far end is a block no row holds of a document that owes
// nothing, which reads unresolved=1, and the census refuses.
func TestTheCensusLeavesOutAMovedToIntoADocumentThatOwesItsBlockIndex(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedAgreeingStore(t, database)
	var owed bool
	if err := database.Pool.QueryRow(ctx, `
		select version is null from document_block_versions where artifact_id = $1::uuid
	`, seeded.document.artifactID).Scan(&owed); err != nil || !owed {
		t.Fatalf("the agreeing store's document owes its block index %t (%v), want it owed, so this test would prove nothing", owed, err)
	}
	statedMovedTo(t, database, seeded.document.refKey+"#moved", seeded.document.refKey+"#later")

	report := storeCensusOf(t, database)
	var printed strings.Builder
	report.Write(&printed)
	moved := storeOnlyKind(t, report, graphstore.TupleMovedTo)
	line := "census: store-only kind=moved_to tuples=1 unresolved=0 left_out=1\n"
	if moved.Tuples != 1 || moved.Unresolved != 0 || moved.LeftOut != 1 || report.Refused() || !strings.Contains(printed.String(), line) {
		t.Fatalf("a moved_to into a document that owes its block index reads %+v and refused=%t, want %q and a pass:\n%s",
			moved, report.Refused(), line, printed.String())
	}

	if _, err := database.Pool.Exec(ctx, `
		update document_block_versions set version = 1 where artifact_id = $1::uuid
	`, seeded.document.artifactID); err != nil {
		t.Fatalf("stand the document's block index at version 1: %v", err)
	}
	report = storeCensusOf(t, database)
	moved = storeOnlyKind(t, report, graphstore.TupleMovedTo)
	if moved.Tuples != 1 || moved.Unresolved != 1 || moved.LeftOut != 0 || !report.Refused() {
		t.Fatalf("a moved_to into a block no row holds of a document owing nothing reads %+v and refused=%t, want it unresolved and a refusal",
			moved, report.Refused())
	}
}

// A moved_to into an ask resolves only to a live ask node, as a copied_from does: the heading of a
// section that cites an answered ask a person removed from its spec takes one moved_to to that
// ask's node (R9), and a citation of the heading resolves through it. Over the agreeing store and
// one ask the backfill marked, a moved_to from a heading address into that ask's live node reads
// tuples=1 unresolved=0 left_out=0, and the census passes. Once the ask's node is stated gone
// through the store's writer, its asks row still standing, the same far end does not resolve, and
// the census refuses. A census that resolved such a far end by the asks row alone would read it
// resolved both times.
func TestTheCensusResolvesAMovedToIntoAnAskOnlyToALiveAskNode(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedAgreeingStore(t, database)
	var ask string
	if err := database.Pool.QueryRow(ctx, `
		insert into asks (issue_key, author, question) values ('CORE-1', '{"kind":"user","id":"alice"}', 'Which shelf?')
		returning id::text
	`).Scan(&ask); err != nil {
		t.Fatalf("write the ask: %v", err)
	}
	if _, err := Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the ask: %v", err)
	}
	requireTheCensusAgrees(t, "the census of the agreeing store and the marked ask", storeCensusOf(t, database))
	heading := seeded.document.refKey + "#removed-heading"
	writeTestTuples(t, database, nil, []graphstore.Tuple{{
		FromKind: refs.BlockNodeKind, FromID: heading, Kind: graphstore.TupleMovedTo, ToKind: graphstore.KindAsk, ToID: ask,
		At: time.Now().UTC(), Who: censusWriter, SourceEvent: graphstore.NoEvent, Asserted: true,
	}})

	report := storeCensusOf(t, database)
	var printed strings.Builder
	report.Write(&printed)
	moved := storeOnlyKind(t, report, graphstore.TupleMovedTo)
	line := "census: store-only kind=moved_to tuples=1 unresolved=0 left_out=0\n"
	if moved.Tuples != 1 || moved.Unresolved != 0 || moved.LeftOut != 0 || report.Refused() || !strings.Contains(printed.String(), line) {
		t.Fatalf("a moved_to into a live ask node reads %+v and refused=%t, want %q and a pass:\n%s",
			moved, report.Refused(), line, printed.String())
	}

	gone := graphstore.AskNode(ask, "CORE", nil, censusWriter, time.Now().UTC())
	gone.State = graphstore.StateGone
	writeTestTuples(t, database, []graphstore.Node{gone}, nil)
	var state string
	if err := database.Pool.QueryRow(ctx, `select state from graph_nodes where kind = 'ask' and id = $1`, ask).Scan(&state); err != nil || state != graphstore.StateGone {
		t.Fatalf("the ask's node reads state %q (%v), want gone, so this test would prove nothing", state, err)
	}
	var rows int
	if err := database.Pool.QueryRow(ctx, `select count(*) from asks where id = $1::uuid`, ask).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("the ask's row reads %d rows (%v), want it standing, so this test would prove nothing", rows, err)
	}
	report = storeCensusOf(t, database)
	printed.Reset()
	report.Write(&printed)
	moved = storeOnlyKind(t, report, graphstore.TupleMovedTo)
	line = "census: store-only kind=moved_to tuples=1 unresolved=1 left_out=0\n"
	if moved.Tuples != 1 || moved.Unresolved != 1 || moved.LeftOut != 0 || !report.Refused() || !strings.Contains(printed.String(), line) {
		t.Fatalf("a moved_to into a gone ask node whose row stands reads %+v and refused=%t, want %q and a refusal:\n%s",
			moved, report.Refused(), line, printed.String())
	}
}

// planWithoutSeqScans is the plan Postgres builds for statement with sequential scans off, in a
// transaction of its own, as api's helper of the same name builds one: a fixture's handful of rows
// is read fastest whole however the statement is written, so only with that setting does the plan
// say which index each read can take, and a read no index serves shows as a scan with none.
func planWithoutSeqScans(t *testing.T, database *store.Store, statement string, arguments ...any) json.RawMessage {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the plan's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "set local enable_seqscan = off"); err != nil {
		t.Fatalf("disable sequential scans: %v", err)
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "explain (format json) "+statement, arguments...).Scan(&raw); err != nil {
		t.Fatalf("explain the statement: %v", err)
	}
	return explaintest.OnePlan(t, raw)
}

// requireKeyedTupleReads fails unless plan reads graph_tuples through no sequential scan and only
// through index scans whose condition keys the first column of the index they read, and through
// one of indexes alone where any are named, and answers how many such scans it holds.
func requireKeyedTupleReads(t *testing.T, keys *explaintest.IndexKeys, what string, plan json.RawMessage, indexes ...string) int {
	t.Helper()
	if explaintest.SeqScansRelation(t, plan, "graph_tuples") {
		t.Errorf("%s reads graph_tuples whole: %s", what, plan)
	}
	keyed := 0
	for _, scan := range explaintest.IndexScans(t, plan) {
		if !strings.HasPrefix(scan.Index, "graph_tuples") {
			continue
		}
		first := keys.FirstKey(t, scan.Index)
		if scan.Cond == "" || !explaintest.CondNamesFirstKey(scan.Cond, first) ||
			(len(indexes) > 0 && !slices.Contains(indexes, scan.Index)) {
			t.Errorf("%s reads %s under the condition %q, want a scan keying its first column %s through one of %v: %s",
				what, scan, scan.Cond, first, indexes, plan)
			continue
		}
		keyed++
	}
	t.Logf("%s reads graph_tuples through %d keyed index scan(s)", what, keyed)
	return keyed
}

// The census reads the store's log, its largest table, once per run for who wrote or answered a
// decision, a reading or an ask (readStoredAuthors), and never whole. That one read's plan reads
// graph_tuples through keyed index scans alone, its history probes stating graph_tuples_as_of_in's
// predicate (to_kind <> 'value') beside their joins so that index is one they can take. The person
// and the session row sides read what it read bound as arrays (censusAuthors), so neither reads
// the history again: the person side's one read of graph_tuples is the proved owners' alias_of,
// through graph_tuples_current, and the session side reads it not at all. The store-only kinds
// and the dangling-citation counter's moved_to sources read their tuples out of the nodes the log
// holds (currentOutOfNodes) through graph_tuples_current alone, an index of the store's set, in
// three keyed scans: the walk's first node of each kind, the walk's next node past each one, and
// the probe of each node's tuples of those kinds. graph_edges' answer of each tie the census holds
// it to (graphEdgesOut) is read node by node, through exactly two probes of graph_tuples, each
// keyed by the node's id: the view's tuple arm on graph_tuples_as_of_out, keyed by the node's kind
// and its id, and the arm over an item's blocks on graph_tuples_current, keyed by (block, the
// node's id, the tie's kind). That second arm is migration 20261116140008's: it answers a block at
// an item's address under a from_kind it reads off the address (comment_block, message_block,
// ask_block), and PostgreSQL drops an arm from a plan only where the read's value and the arm's
// are both constants, so a read that binds from_kind as each node's kind keeps it; the reply arms
// render their kind as the constant replies_to (20261116114517, 20261116124448), and every other
// kind's read drops them. A third probe, or one not keyed by the node's id, fails this; so does the
// item block arm's probe answering a row for a node that is no block, which it cannot, no block's
// address being an ask's id. A plan that keys the kind alone reads every tuple of that kind, and
// reads them again for each node.
func TestTheCensusReadsTheStoresLogThroughItsIndexes(t *testing.T) {
	database := storetest.Open(t)
	keys := explaintest.NewIndexKeys(database.Pool)
	if keyed := requireKeyedTupleReads(t, keys, "the stored authors' one read",
		planWithoutSeqScans(t, database, `select stored.kind, stored.id, stored.at from `+inlineAuthors)); keyed == 0 {
		t.Errorf("the stored authors' one read reads graph_tuples through no index")
	}
	bound := []any{[]string{}, []string{}, []time.Time{}}
	if keyed := requireKeyedTupleReads(t, keys, "the person row side",
		planWithoutSeqScans(t, database, `select p.login from (`+commentPeople(censusAuthors, true)+`) p`, bound...),
		"graph_tuples_current"); keyed != 1 {
		t.Errorf("the person row side reads graph_tuples through %d keyed scan(s), want the one of the owners' alias_of", keyed)
	}
	if keyed := requireKeyedTupleReads(t, keys, "the session row side",
		planWithoutSeqScans(t, database, `select s.id from (`+recordedSessions(censusAuthors, MessagesNameTheirNode)+`) s`, bound...),
		"graph_tuples_current"); keyed != 0 {
		t.Errorf("the session row side reads graph_tuples through %d scan(s), want none: the stored authors are bound", keyed)
	}

	for _, read := range []struct {
		name      string
		statement string
		arguments []any
	}{
		{"the store-only read", storeOnlyRead, nil},
		{"the dangling-citation counter", danglingRead, []any{[]string{}}},
	} {
		if keyed := requireKeyedTupleReads(t, keys, read.name,
			planWithoutSeqScans(t, database, read.statement, read.arguments...), "graph_tuples_current"); keyed != 3 {
			t.Errorf("%s reads graph_tuples through %d keyed scan(s), want 3: the walk's first node of each kind, its next node and the probe of each node's tuples",
				read.name, keyed)
		}
	}

	// The six ties graph_edges answers, each with the tuple kind its read keys.
	edgeKinds := map[string]string{
		refs.TupleShaped + edgeSuffix:           refs.TupleShaped,
		refs.TupleFromAsk + edgeSuffix:          refs.TupleFromAsk,
		graphstore.TupleDecidedBy + edgeSuffix:  graphstore.TupleDecidedBy,
		graphstore.TupleFollowedBy + edgeSuffix: graphstore.TupleFollowedBy,
		"ask_parent" + edgeSuffix:               graphstore.TupleOwnedBy,
		"ask_anchor" + edgeSuffix:               graphstore.TupleAnchoredTo,
	}
	var edges []string
	for _, held := range projections {
		if held.table != "graph_edges" {
			continue
		}
		edges = append(edges, held.name)
		kind, known := edgeKinds[held.name]
		if !known {
			t.Errorf("the census holds graph_edges' answer of %s, which this test names no tuple kind of", held.name)
			continue
		}
		plan := planWithoutSeqScans(t, database, held.rows)
		requireKeyedTupleReads(t, keys, "graph_edges' "+held.name, plan)
		var probes []string
		for _, scan := range explaintest.IndexScans(t, plan) {
			if strings.HasPrefix(scan.Index, "graph_tuples") {
				probes = append(probes, scan.Index+" "+scan.Cond)
			}
		}
		slices.Sort(probes)
		want := []string{
			"graph_tuples_as_of_out ((from_kind = n.kind) AND (from_id = n.id))",
			"graph_tuples_current ((from_kind = 'block'::text) AND (from_id = n.id) AND (kind = '" + kind + "'::text))",
		}
		if !slices.Equal(probes, want) {
			t.Errorf("graph_edges' %s reads graph_tuples through %q, want exactly the tuple arm's and the item block arm's probes %q, each keyed by the node's id: %s",
				held.name, probes, want, plan)
		}
	}
	slices.Sort(edges)
	if names := slices.Sorted(maps.Keys(edgeKinds)); !slices.Equal(edges, names) {
		t.Errorf("the census holds graph_edges' answer of %v, want the six ties it answers, %v", edges, names)
	}

	// The item block arm's probe finds nothing for a node that is no block: no block's address is
	// an ask's id. Over a store holding an ask's node, each read of a tie an ask holds, narrowed to
	// that node, reaches it (its tuple arm probe ran) and its item block arm probe answers no row,
	// as EXPLAIN ANALYZE measures them.
	seedAgreeingStore(t, database)
	const ask = "00000000-0000-4000-8000-0000000000a5"
	writeTestTuples(t, database, []graphstore.Node{graphstore.AskNode(ask, "CORE", nil, censusWriter, time.Now().UTC())}, nil)
	for _, held := range projections {
		if held.table != "graph_edges" || !slices.Contains(strings.Split(held.nodeKind, ", "), graphstore.KindAsk) {
			continue
		}
		plan := analyzedPlanWithoutSeqScans(t, database,
			held.rows+"\n\t\t  and n.kind = '"+graphstore.KindAsk+"' and n.id = $1", ask)
		var general, item []explaintest.Node
		for _, node := range explaintest.Nodes(t, plan) {
			switch {
			case node.Index == "graph_tuples_as_of_out":
				general = append(general, node)
			case node.Index == "graph_tuples_current" && strings.Contains(node.IndexCond, "(from_kind = 'block'::text)"):
				item = append(item, node)
			}
		}
		if len(general) != 1 || general[0].Loops < 1 {
			t.Errorf("graph_edges' %s read of ask %s runs its tuple arm probe %v, want once at least, so the bound below is read for that node: %s",
				held.name, ask, general, plan)
		}
		if len(item) != 1 || item[0].Rows*item[0].Loops != 0 {
			t.Errorf("graph_edges' %s read of ask %s has the item block arm probes %+v, want one that answers no row for a node that is no block: %s",
				held.name, ask, item, plan)
		}
	}
}

// analyzedPlanWithoutSeqScans is planWithoutSeqScans run under EXPLAIN ANALYZE, so each node of
// the plan carries how many times it ran and how many rows it answered.
func analyzedPlanWithoutSeqScans(t *testing.T, database *store.Store, statement string, arguments ...any) json.RawMessage {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the analyzed plan's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "set local enable_seqscan = off"); err != nil {
		t.Fatalf("disable sequential scans: %v", err)
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "explain (analyze, format json) "+statement, arguments...).Scan(&raw); err != nil {
		t.Fatalf("explain and run the statement: %v", err)
	}
	return explaintest.OnePlan(t, raw)
}
