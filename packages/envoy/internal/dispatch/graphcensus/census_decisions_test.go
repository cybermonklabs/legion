package graphcensus

import (
	"context"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// unmarkedAskKinds are the ask tie kinds whose rows the earlier release
// wrote with no node, in the order the census reads them: an ask's block is one of them
// (askBlockProjection), since that release wrote the row's block columns with no node either.
var unmarkedAskKinds = []string{graphstore.TupleFollowedBy, "ask_parent", "ask_anchor", "ask_answer", graphstore.TupleAskedOn}

// The rule that a tie kind holding no row and no node is no refusal is for a
// store nothing of that kind was written to. An asks table and an ask_followers table whose rows
// the earlier release wrote, and which no backfill has marked, hold rows all the same: their kinds
// read no row and no node, since the row side reads marked rows alone, while each counts its own
// unmarked rows. Over the agreeing store and one such ask - an ask block anchored to the document
// and answered by bob - and one follower row of it, every ask kind counts its unmarked row, none
// prints that nothing of it is written yet, each prints that it checked no row, and the census
// refuses with those five kinds as the kinds that checked no row. (The project census refuses the
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

// censusLens is the lens writeCensusLens writes.
const censusLens = "00000000-0000-4000-8000-0000000004a1"

// writeCensusLens writes the lens censusLens at state under CORE-1 through the store's writer,
// with the tuples beside it: its first write states its part_of, and a write of a lens the store
// holds moves its state alone, as the state route's write does.
func writeCensusLens(t *testing.T, database *store.Store, state string, tuples ...graphstore.Tuple) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the lens's write: %v", err)
	}
	defer tx.Rollback(ctx)
	at := time.Now().UTC()
	node, held, err := graphstore.ReadNode(ctx, tx, graphstore.KindLens, censusLens)
	if err != nil {
		t.Fatalf("read the lens %s: %v", censusLens, err)
	}
	if !held {
		node = graphstore.Node{Kind: graphstore.KindLens, ID: censusLens, Supertag: graphstore.KindLens,
			Visibility: graphstore.VisibilityProject, Project: "CORE"}
		tuples = append(tuples, graphstore.Tuple{FromKind: graphstore.KindLens, FromID: censusLens, Kind: graphstore.TuplePartOf,
			ToKind: graphstore.KindIssue, ToID: "CORE-1"})
	}
	node.State, node.MadeAt, node.MadeBy, node.SourceEvent = state, at, censusWriter, graphstore.NoEvent
	node.KeepsCause, node.Changed = held, held
	for index := range tuples {
		tuples[index].At, tuples[index].Who, tuples[index].SourceEvent, tuples[index].Asserted = at, censusWriter, graphstore.NoEvent, true
	}
	if err := graphstore.Write(ctx, tx, []graphstore.Node{node}, tuples); err != nil {
		t.Fatalf("write the lens %s %s: %v", censusLens, state, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the lens's write: %v", err)
	}
}

// The pre-mortem conversion moves the address of the document it retires to the lens it wrote in
// its place (api.ConvertPremortem): one moved_to from the document's address, kind artifact at its
// ref_key, to that lens. The census reads that far end resolved while the lens stands, as
// graphstore.MovedTo reads it, so the agreeing store with a converted document reads moved_to
// tuples=1 unresolved=0, no difference, and passes. Once the lens is gone the same tuple's far end
// resolves to nothing that stands, and the census refuses.
func TestTheCensusResolvesADocumentsMoveToItsLens(t *testing.T) {
	database := storetest.Open(t)
	seeded := seedAgreeingStore(t, database)
	writeCensusLens(t, database, graphstore.StateLive, graphstore.Tuple{
		FromKind: graphstore.KindArtifact, FromID: seeded.document.refKey, Kind: graphstore.TupleMovedTo,
		ToKind: graphstore.KindLens, ToID: censusLens,
	})

	report := storeCensusOf(t, database)
	var printed strings.Builder
	report.Write(&printed)
	moved := storeOnlyKind(t, report, graphstore.TupleMovedTo)
	line := "census: store-only kind=moved_to tuples=1 unresolved=0 left_out=0\n"
	if moved.Tuples != 1 || moved.Unresolved != 0 || report.Differences() != 0 || report.Refused() || !strings.Contains(printed.String(), line) {
		t.Fatalf("a document's move to the lens that stands reads %+v, %d difference(s) and refused=%t, want %q, none and a pass:\n%s",
			moved, report.Differences(), report.Refused(), line, printed.String())
	}

	writeCensusLens(t, database, graphstore.StateGone)
	report = storeCensusOf(t, database)
	moved = storeOnlyKind(t, report, graphstore.TupleMovedTo)
	if moved.Tuples != 1 || moved.Unresolved != 1 || !report.Refused() {
		t.Fatalf("a document's move to a lens that is gone reads %+v and refused=%t, want it unresolved and a refusal",
			moved, report.Refused())
	}
}

// A moved_to into an ask resolves only to an ask node that is not gone, as a copied_from does: the
// heading of a section that cites an answered ask a person removed from its spec takes one moved_to to that
// ask's node (R9), and a citation of the heading resolves through it. Over the agreeing store and
// one ask the backfill marked, a moved_to from a heading address into that ask's live node reads
// tuples=1 unresolved=0 left_out=0, and the census passes. Once the ask's node is stated gone
// through the store's writer, its asks row still standing, the same far end does not resolve, and
// the census refuses. A census that resolved such a far end by the asks row alone would read it
// resolved both times.
func TestTheCensusResolvesAMovedToIntoAnAskOnlyToAnAskNodeNotGone(t *testing.T) {
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
// holds (currentOutOfNodes, statedOutOfNodes) through graph_tuples_current alone, an index of the
// store's set, in three keyed scans: the walk's first node of each kind, the walk's next node past
// each one, and the probe of each node's tuples of those kinds. graph_edges' answer of each tie the
// census holds it to (graphEdgesOut) is read node by node, so the view's tuple arm reads
// graph_tuples through one scan keyed by the node's kind and its id: a plan that keys the kind
// alone reads every tuple of that kind, and reads them again for each node. The census holds the
// view's answer of each tie it answers, each by name.
func TestTheCensusReadsTheStoresLogThroughItsIndexes(t *testing.T) {
	database := storetest.Open(t)
	keys := explaintest.NewIndexKeys(database.Pool)
	if keyed := requireKeyedTupleReads(t, keys, "the stored authors' one read",
		planWithoutSeqScans(t, database, `select stored.kind, stored.id, stored.at from `+inlineAuthors)); keyed == 0 {
		t.Errorf("the stored authors' one read reads graph_tuples through no index")
	}
	bound := []any{[]string{}, []string{}, []time.Time{}}
	if keyed := requireKeyedTupleReads(t, keys, "the person row side",
		planWithoutSeqScans(t, database, `select p.login from (`+commentPeople(censusAuthors)+`) p`, bound...),
		"graph_tuples_current"); keyed != 1 {
		t.Errorf("the person row side reads graph_tuples through %d keyed scan(s), want the one of the owners' alias_of", keyed)
	}
	if keyed := requireKeyedTupleReads(t, keys, "the session row side",
		planWithoutSeqScans(t, database, `select s.id from (`+recordedSessions(censusAuthors)+`) s`, bound...),
		"graph_tuples_current"); keyed != 0 {
		t.Errorf("the session row side reads graph_tuples through %d scan(s), want none: the stored authors are bound", keyed)
	}

	for _, read := range []struct {
		name      string
		statement string
		arguments []any
	}{
		{"the store-only read", storeOnlyRead, nil},
		{"the dangling-citation counter", danglingRead, []any{[]string{}, []string{}}},
	} {
		if keyed := requireKeyedTupleReads(t, keys, read.name,
			planWithoutSeqScans(t, database, read.statement, read.arguments...), "graph_tuples_current"); keyed != 3 {
			t.Errorf("%s reads graph_tuples through %d keyed scan(s), want 3: the walk's first node of each kind, its next node and the probe of each node's tuples",
				read.name, keyed)
		}
	}

	var edges []string
	for _, held := range projections {
		if held.table != "graph_edges" {
			continue
		}
		edges = append(edges, held.name)
		plan := planWithoutSeqScans(t, database, held.rows)
		if keyed := requireKeyedTupleReads(t, keys, "graph_edges' "+held.name, plan); keyed != 1 {
			t.Errorf("graph_edges' %s reads graph_tuples through %d keyed scan(s), want 1: the probe of each node's tuples",
				held.name, keyed)
		}
		for _, scan := range explaintest.IndexScans(t, plan) {
			if strings.HasPrefix(scan.Index, "graph_tuples") && !strings.Contains(scan.Cond, "from_id = n.id") {
				t.Errorf("graph_edges' %s reads %s under the condition %q, want it keyed by each node's id: %s",
					held.name, scan, scan.Cond, plan)
			}
		}
	}
	for _, tie := range []string{refs.TupleShaped, refs.TupleFromAsk, graphstore.TupleDecidedBy,
		graphstore.TupleFollowedBy, "ask_parent", "ask_anchor"} {
		if !slices.Contains(edges, tie+edgeSuffix) {
			t.Errorf("the census holds graph_edges' answer of %v, and not of %s, a tie graph_edges answers", edges, tie)
		}
	}
}

// typedBlock writes a block node of document at address, of supertag and in state, as the typed
// node pass writes a decision's or a reading's (refs.PlanDecisionNodes), with the in_document and
// the part_of to its document a block the document holds directly states, and with nothing else.
func typedBlock(t *testing.T, database *store.Store, document preStoreDocument, address, supertag, state string) {
	t.Helper()
	now := time.Now().UTC()
	node := graphstore.Node{
		Kind: refs.BlockNodeKind, ID: address, Supertag: supertag, Visibility: graphstore.VisibilityProject,
		State: state, Project: "CORE", MadeAt: now, MadeBy: censusWriter, SourceEvent: graphstore.NoEvent,
	}
	var tuples []graphstore.Tuple
	for _, kind := range []string{refs.TupleInDocument, graphstore.TuplePartOf} {
		tuples = append(tuples, graphstore.Tuple{
			FromKind: refs.BlockNodeKind, FromID: address, Kind: kind, ToKind: graphstore.KindArtifact, ToID: document.refKey,
			At: now, Who: censusWriter, SourceEvent: graphstore.NoEvent, Asserted: true,
		})
	}
	writeTestTuples(t, database, []graphstore.Node{node}, tuples)
}

// Above the deferral bound a process of the release before runs no block index in the write, and
// its typed-node pass states each decision and reading the version no longer holds gone in that
// write's own transaction, while the document_blocks rows of their blocks and the block_refs rows
// they hold stay until the block-index reconciliation deletes them. Over the agreeing store, whose
// document owes its block index, a decision and the reading inside it are stated gone with every
// row of theirs standing - both blocks' rows, the reading's parent, the decision's decided_by and
// its mention of the issue - as that release leaves them, and the census reads zero differences:
// those rows are owed their deletion (typedRowOwed, kept for one release). Once the document's
// block index stands at its version, the same rows name gone nodes of a document that owes
// nothing, and the census refuses, naming each under the gone cause in its kind. This build's
// deferred write deletes those rows in the write that states the nodes gone
// (refs.PlanDecisionNodes; api's TestADecisionADeferredVersionRemovesIsGoneBeforeTheBlockIndexRuns
// and TestATypedBlockADeferredVersionRemovesReadsGoneAtOnce build it through the server): through
// the real write, a decision and its reading a version indexed and the next, deferred version
// removes leave neither row behind, and the census of that store, its document owing its index,
// reads zero differences.
func TestTheCensusOwesTheRowsOfADecisionADeferredVersionRemoved(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedAgreeingStore(t, database)
	decision, reading := seeded.document.refKey+"#d1", seeded.document.refKey+"#g1"
	typedBlock(t, database, seeded.document, decision, refs.DecisionSupertag, graphstore.StateGone)
	typedBlock(t, database, seeded.document, reading, refs.ReadingSupertag, graphstore.StateGone)
	dave := model.Actor{Kind: "user", ID: "dave"}
	writeTestTuples(t, database, []graphstore.Node{graphstore.PersonNode("dave", dave, time.Now().UTC(), graphstore.NoEvent)}, nil)
	if _, err := database.Pool.Exec(ctx, `
		insert into document_blocks (artifact_id, block_id, node_id, node_kind, node_address, type, text_sha256, excerpt,
		                             changed_version, changed_at, parent_block_id)
		values ($1::uuid, 'd1', $2, 'block', $2, 'decision', '', 'Keep one.', 1, now(), null),
		       ($1::uuid, 'g1', $3, 'block', $3, 'reading', '', 'The agent reads it.', 1, now(), 'd1')
	`, seeded.document.artifactID, decision, reading); err != nil {
		t.Fatalf("write the rows of the decision and the reading: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version, node_kind, node_address)
		values ($1::uuid, 'd1', $2, 'person', 'dave', 'decided_by', 1, 'block', $2),
		       ($1::uuid, 'd1', $2, 'issue', 'CORE-1', 'mentions', 1, 'block', $2)
	`, seeded.document.artifactID, decision); err != nil {
		t.Fatalf("write the decision's block_refs rows: %v", err)
	}
	requireTheCensusAgrees(t, "the census of a decision and a reading a deferred version removed", storeCensusOf(t, database))

	if _, err := database.Pool.Exec(ctx, `
		update document_block_versions set version = 1 where artifact_id = $1::uuid
	`, seeded.document.artifactID); err != nil {
		t.Fatalf("stand the document's block index at version 1: %v", err)
	}
	report := storeCensusOf(t, database)
	const gone = "the row names a node the store holds gone"
	for kind, at := range map[string]string{
		"block":                   "block " + decision,
		"block_ref":               "block_ref " + decision + " mentions -> issue CORE-1",
		graphstore.TupleDecidedBy: "block_ref " + decision + " decided_by -> person dave",
		"reading_parent":          "reading " + reading + " part_of -> block " + decision,
	} {
		if !slices.Contains(censusKind(t, report, kind).ReportedRows, Difference{Cause: gone, At: at}) {
			t.Errorf("once the document owes nothing the census reads kind %s as %+v, want the row %s under %q",
				kind, censusKind(t, report, kind).ReportedRows, at, gone)
		}
	}
	if !slices.Contains(censusKind(t, report, "block").ReportedRows, Difference{Cause: gone, At: "block " + reading}) || !report.Refused() {
		t.Fatalf("once the document owes nothing the census reads kind block as %+v and refused=%t, want the reading's row under %q and a refusal",
			censusKind(t, report, "block").ReportedRows, report.Refused(), gone)
	}
	requireTheDeferredWriteLeavesNoRow(t)
}

// requireTheDeferredWriteLeavesNoRow is TestTheCensusOwesTheRowsOfADecisionADeferredVersionRemoved's
// case of this build's deferred write: beside the agreeing store (seedAgreeingStore), whose kinds
// keep their rows, version 1 of a second document holding a decision that closes with a reading is
// indexed whole (refs.IndexBlocks), and version 2, which holds neither and whose block index its
// write leaves to the reconciliation, runs the typed-node pass on its own (refs.PlanDecisionNodes),
// as a write above the deferral bound runs it. That write states both nodes gone and deletes their
// document_blocks and block_refs rows, so the census of the store, whose second document owes its
// block index, reads zero differences.
func requireTheDeferredWriteLeavesNoRow(t *testing.T) {
	t.Helper()
	database := storetest.Open(t)
	ctx := context.Background()
	seedAgreeingStore(t, database)
	var document preStoreDocument
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('CORE-1', 'CORE', 'decisions-md', 'decisions.md', 'doc', false, '{"kind":"user","id":"alice"}')
		returning id::text, ref_key`).Scan(&document.artifactID, &document.refKey); err != nil {
		t.Fatalf("create the second document: %v", err)
	}
	alice := model.Actor{Kind: "user", ID: "alice"}
	sources := creditedSources(alice)
	write := func(number int, markdown string, whole bool) {
		t.Helper()
		version := renderVersion(t, parsedTree(t, markdown))
		at := censusVersion(t, database, document.artifactID, number, version.markdown, alice)
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		versions := refs.OneVersion(document.artifactID, number, at)
		record, err := refs.ReadIndexedVersion(ctx, tx, document.artifactID, document.artifactID, number)
		if err != nil {
			t.Fatalf("read the record of version %d: %v", number, err)
		}
		facts, err := refs.DecisionFacts(ctx, tx, document.artifactID, record, versions, version.blocks, version.tree, nil, sources)
		if err != nil {
			t.Fatalf("read the typed facts of version %d: %v", number, err)
		}
		var stated graphstore.Uncaused
		if whole {
			if _, _, stated, _, err = refs.IndexBlocks(ctx, tx, document.artifactID, versions, version.markdown, version.blocks, nil, nil, "", facts); err != nil {
				t.Fatalf("index version %d: %v", number, err)
			}
		} else {
			index, err := refs.ReadBlockIndex(ctx, tx, document.artifactID)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := refs.PlanDecisionNodes(ctx, tx, index, document.artifactID, facts, graphstore.Uncaused{}, "")
			if err != nil {
				t.Fatalf("plan the typed-node pass of version %d: %v", number, err)
			}
			if err := store.RunWrite(ctx, tx, plan.Queue); err != nil {
				t.Fatalf("write the typed-node pass of version %d: %v", number, err)
			}
			stated = plan.Stated()
		}
		if err := graphstore.Caused(ctx, tx, stated, graphstore.NoEvent); err != nil {
			t.Fatalf("state version %d: %v", number, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit version %d: %v", number, err)
		}
		// The version's own node, as the version writer states it beside its row, for the
		// census's version kind.
		if _, err := Backfill(ctx, database, io.Discard); err != nil {
			t.Fatalf("backfill the node of version %d: %v", number, err)
		}
	}
	write(1, "::::decision{#d1}\nKeep one.\n\n:::reading{#g1}\nThe agent reads it.\n:::\n::::\n", true)
	requireNoCensusDifference(t, "the census of the second document whose version 1 holds a decision", storeCensusOf(t, database))
	write(2, "A paragraph and no decision.\n", false)
	var rows, links int
	if err := database.Pool.QueryRow(ctx, `
		select (select count(*) from document_blocks where artifact_id = $1 and block_id in ('d1', 'g1')),
		       (select count(*) from block_refs where artifact_id = $1 and block_id in ('d1', 'g1'))`, document.artifactID).Scan(&rows, &links); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || links != 0 {
		t.Fatalf("this build's deferred write left %d document_blocks rows and %d block_refs rows of the decision and the reading it removed, want none", rows, links)
	}
	var indexed *int
	if err := database.Pool.QueryRow(ctx, `select version from document_block_versions where artifact_id = $1`, document.artifactID).Scan(&indexed); err != nil || indexed != nil {
		t.Fatalf("the document stands indexed at %v (%v) after its deferred version 2, want it owed: this case proves nothing", indexed, err)
	}
	requireNoCensusDifference(t, "the census after this build's deferred write removed the decision", storeCensusOf(t, database))
}

// requireNoCensusDifference fails t unless the census read no difference in either direction over
// any kind and passes. Unlike requireTheCensusAgrees it asks no kind to hold rows: a document whose
// block index its last version left owed holds no block row until the reconciliation writes it.
func requireNoCensusDifference(t *testing.T, what string, report *StoreReport) {
	t.Helper()
	for _, kind := range report.Kinds {
		for _, difference := range differencesOf(kind) {
			t.Errorf("%s reads a difference of kind %s at %s: %s", what, kind.Kind, difference.At, difference.Cause)
		}
	}
	if report.Differences() != 0 || report.Refused() {
		t.Fatalf("%s read %d difference(s) and refused=%t, want none and a pass", what, report.Differences(), report.Refused())
	}
}

// A decision's tie whose far end is past the store's bound is no relation of the store: its
// writer leaves its block_refs row unmarked and states no tuple of it (refs' queueBlockRefs), and
// graph_edges' block_refs arm answers that row alone. Over the agreeing store and a live decision
// holding a shaped row to an address past the bound, the census counts that row past the bound
// in the shaped kind and in graph_edges' shaped, and reads no difference in either: a census that
// held the view's answer to the tuples would read the edge as a row no tuple states.
func TestTheCensusCountsATieEdgePastTheBoundApart(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedAgreeingStore(t, database)
	decision := seeded.document.refKey + "#d1"
	typedBlock(t, database, seeded.document, decision, refs.DecisionSupertag, graphstore.StateLive)
	far := seeded.document.refKey + "#" + strings.Repeat("x", graphstore.MaxAddressBytes)
	if _, err := database.Pool.Exec(ctx, `
		insert into document_blocks (artifact_id, block_id, node_id, node_kind, node_address, type, text_sha256, excerpt,
		                             changed_version, changed_at)
		values ($1::uuid, 'd1', $2, 'block', $2, 'decision', '', 'Keep one.', 1, now())
	`, seeded.document.artifactID, decision); err != nil {
		t.Fatalf("write the decision's row: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version)
		values ($1::uuid, 'd1', $2, 'block', $3, 'shaped', 1)
	`, seeded.document.artifactID, decision, far); err != nil {
		t.Fatalf("write the decision's shaped row past the bound: %v", err)
	}
	var answered int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from graph_edges where from_kind = 'block' and from_id = $1 and kind = 'shaped' and to_id = $2
	`, decision, far).Scan(&answered); err != nil || answered != 1 {
		t.Fatalf("graph_edges answers the shaped row past the bound %d time(s) (%v), want once, so this test would hold nothing", answered, err)
	}
	report := storeCensusOf(t, database)
	requireTheCensusAgrees(t, "the census of a shaped row past the bound", report)
	for _, kind := range []string{refs.TupleShaped, refs.TupleShaped + edgeSuffix} {
		if held := censusKind(t, report, kind); held.PastBound != 1 {
			t.Errorf("the census counts %d row(s) of kind %s past the bound, want the shaped row", held.PastBound, kind)
		}
	}
}
