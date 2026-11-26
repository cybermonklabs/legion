package graphstore

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// The nodes the reads under test are taken on. probe is one node among tens of thousands, whose
// own tuples are a handful out of a log of more than a million: that is the shape a current read
// has in production, and the shape the four read indexes exist for. crowded is the node with a
// long inbox, where TuplesIntoNode naming its kinds reads one index scan per kind and stops at
// the limit, and naming none has no kind to scan by and reads that whole inbox - as a scan of
// the log where the planner sees the node's values, as one index range sorted where it does not.
const (
	probeKind   = "block"
	probeID     = "GRAPH/seed#probe"
	crowdedKind = "block"
	crowdedID   = "GRAPH/seed#crowded"
)

// crowdedInboxKinds are the kinds the seed spreads the crowded node's inbox over. The inbox read
// does not take its kind list from here: it names the kinds the store holds into that node
// (inboxKindsInTheStore), so a kind a later writer first states into a node is a kind the read
// names that same day, where a list written out in Go would leave it out of the inbox without a
// word. The seed adds one kind this list lacks (crowdedSupertagKind) to hold that to it.
var crowdedInboxKinds = []string{"replies_to", "mentions", "answers", "part_of", "anchored_to"}

// The supertag node that states one tuple into the crowded node under a kind crowdedInboxKinds
// lacks, at a time after every tuple the seed appends there, so it heads the crowded node's
// inbox: an inbox read that named the seed's own list would answer every row but that one.
const (
	crowdedSupertagID   = "decision"
	crowdedSupertagKind = "exemplified_by"
)

// How many rows each shape of the seed appends. The total is above a million, which is the size
// this test is for: below it the planner reads a table of any shape as a scan, and a plan proves
// nothing about the index set. The shapes are what a log of Dispatch's own writes holds - blocks
// citing issues, blocks carrying text, replies into one busy block, relations retracted and
// stated again, and tuples whose time runs against their log order.
const (
	seedOutboundRelations  = 300000
	seedAttributes         = 150000
	seedCrowdedInbox       = 400000
	seedOrdinaryInbox      = 120000
	seedRetractions        = 30000
	seedReassertions       = 30000
	seedTimeReversedTuples = 20000
	seedTotal              = seedOutboundRelations + seedAttributes + seedCrowdedInbox +
		seedOrdinaryInbox + seedRetractions + seedReassertions + seedTimeReversedTuples
)

// seedInboxBlocks is how many block nodes the ordinary inbound shape points into: a read of one
// node is only index-reachable while the column it binds has many values, so the seed spreads
// its from_ids and to_ids over tens of thousands of them.
const seedInboxBlocks = 40000

// longValueBytes is the size of the value this test writes and reads back. A btree key is
// bounded at about 2,704 bytes on an 8 KiB page, so an index keying to_id would refuse a value
// of this size at the insert; the three indexes carrying to_id are partial on to_kind <>
// 'value' for exactly that reason (20261109013400_graph_store.up.sql).
const longValueBytes = 3 * 1024

// inboxLimit is the bound the inbox reads under test take.
const inboxLimit = 50

// planCacheModes are the two plans Postgres holds one prepared statement to, and every read
// below is explained under both. pgx prepares the statements this package sends, so the plan
// production settles on is the generic one, built without the values a call binds; the custom
// plan is the one built for one call's own values. A read that reaches its index only while the
// planner can see its values scans in production, and either mode alone hides that.
var planCacheModes = []string{"force_custom_plan", "force_generic_plan"}

// TestEveryCurrentStoreReadRunsThroughTheTupleIndexes holds the four read indexes of the first
// migration to carrying every read the package states, against a log of more than a million
// tuples. It explains each read as the package itself sends it (recordedQueryer), so a statement
// that is edited is explained as edited rather than against a copy of it kept here; it checks
// each read's answer against a reference that walks the whole log in Go, so an index that
// answers fewer rows than the log holds fails here rather than passing as a faster plan; and it
// reads the migration files and the catalog, so an index moved to a later migration - where an
// existing deployment would build it on a full table - fails too, and so does a dropped one, and
// so does any index outside the set that a migration's create index statement declares on
// graph_tuples, however that statement spells the index and the table (indexDeclaration,
// TestIndexesOfReadsEveryCreateIndexSpelling). The scan of the files reads create index
// statements alone: an index a constraint makes, a primary key or a unique constraint of a create
// table or an alter table, is not one it reads.
//
// Each read is explained under both plan-cache modes (planCacheModes), and each plan of each
// mode is held to one rule: it reads graph_tuples through one of the four read indexes, that
// scan carries an index condition, and no plan scans that table sequentially. The index
// condition is the whole of it. An index that exists, and is even named for the column a read
// binds, can still fail to serve that read - through the expression it is keyed on, or through
// the order columns it lacks - and the scan then reads the index whole and drops what it does
// not want with a filter. A node that reads as an index scan and narrows on nothing is the
// whole-table read under a better name, so the rule is read off the plan and never off the
// index list.
//
// The current value of one attribute is held to more than that: to graph_tuples_values alone,
// the one read index partial on to_kind = 'value'. The current reads out of and into one node
// are held to graph_tuples_as_of_out and graph_tuples_as_of_in alone, and so is the walk of the
// nodes beneath one node, every step of it a read into a node it reached (storeReads says why).
//
// The one read held to the opposite is the inbox read naming no kind (requireWholeInbox), the
// control: with no kind to scan by it reads the crowded node's whole inbox under either mode,
// which is what naming the kinds buys. Under the generic plan it still reads that inbox through
// an index and not by a scan of the log, and the run prints the rows it read.
//
// It also reports the bytes a row the store costs at this size, which the issue's growth
// projection is read off.
func TestEveryCurrentStoreReadRunsThroughTheTupleIndexes(t *testing.T) {
	ctx := context.Background()
	requireIndexSetOfTheFirstMigration(t)
	database := storetest.Open(t)
	pool := database.Pool
	requireIndexSetInTheCatalog(t, ctx, pool)
	seedTupleLog(t, ctx, pool)
	writeSupertagIntoTheCrowdedNode(t, ctx, pool)
	writeProbeTuples(t, ctx, pool)
	longValue := writeOneLongValue(t, ctx, pool)
	writeSubtree(t, ctx, pool)
	// The planner reads row counts and column statistics off the catalog, and a table just
	// filled has neither until it is analysed; without this every plan below is built for a
	// table Postgres still believes is empty.
	if _, err := pool.Exec(ctx, `analyze graph_tuples`); err != nil {
		t.Fatalf("analyze the tuple log: %v", err)
	}
	reportBytesARow(t, ctx, pool)

	reference := walkTheWholeLog(t, ctx, pool)
	readback, ok, err := CurrentValue(ctx, pool, longValueKind, longValueID, longValueAttribute)
	if err != nil || !ok {
		t.Fatalf("read back the %d-byte value: found %v, %v", longValueBytes, ok, err)
	}
	if readback != longValue {
		t.Errorf("the %d-byte value reads back as %d bytes; an index keying to_id would have refused the write",
			longValueBytes, len(readback))
	}

	keys := explaintest.NewIndexKeys(pool)
	for _, read := range storeReads(t, ctx, pool, reference) {
		for _, mode := range planCacheModes {
			requireReadRunsThroughTheIndexes(t, ctx, pool, keys, read, mode)
		}
	}
}

// requireReadRunsThroughTheIndexes explains one read under one plan-cache mode and holds that
// plan to the rule the test's own comment states. Every failure names the read and the mode,
// because a read that is served under one mode and lost under the other is the failure this
// check exists for, and prints the plan it read.
func requireReadRunsThroughTheIndexes(
	t *testing.T, ctx context.Context, pool *store.Pool, keys *explaintest.IndexKeys, read storeRead, mode string,
) {
	t.Helper()
	plan, text := explainRecorded(t, ctx, pool, read.name, mode, read.statement)
	subject := read.name + " under " + mode
	t.Logf("%s:\n%s", subject, text)
	if read.wantWholeInbox {
		requireWholeInbox(t, subject, mode, plan, text)
		return
	}
	if explaintest.SeqScansRelation(t, plan, "graph_tuples") {
		t.Errorf("%s scans graph_tuples:\n%s", subject, text)
	}
	scans := readIndexScans(t, plan)
	switch {
	case read.wantMergedInTime && len(scans) != read.wantIndexScans:
		// A read merged in time order reads one index scan for each kind it names and no
		// more: a scan more than that is a branch reading graph_tuples for nothing the
		// answer holds, and the count of branches is the count of kinds.
		t.Errorf("%s reads graph_tuples through %d index scans, want exactly %d, one for each kind it names:\n%s",
			subject, len(scans), read.wantIndexScans, text)
	case len(scans) < read.wantIndexScans:
		t.Errorf("%s reads graph_tuples through %d index scans, want %d, one for each kind it names:\n%s",
			subject, len(scans), read.wantIndexScans, text)
	}
	if len(scans) == 0 {
		t.Errorf("%s reads graph_tuples through no index at all:\n%s", subject, text)
	}
	for _, scan := range scans {
		if !slices.Contains(graphTupleReadIndexes, scan.Index) {
			t.Errorf("%s reads graph_tuples through %s, which is no read index of the first migration (%v):\n%s",
				subject, scan.Index, graphTupleReadIndexes, text)
		}
		if read.wantOnlyIndex != "" && scan.Index != read.wantOnlyIndex {
			t.Errorf("%s reads graph_tuples through %s, want %s alone, the one read index storeReads holds this read to:\n%s",
				subject, scan.Index, read.wantOnlyIndex, text)
		}
		requireIndexCondition(t, subject, scan, text)
	}
	keys.RequireIndexKeyed(t, plan, explaintest.AnyIndex, subject, explaintest.CondOptional)
	if read.wantMergedInTime && !planHolds(t, plan, "Merge Append") {
		t.Errorf("%s does not merge its kinds in time order (no Merge Append node):\n%s", subject, text)
	}
}

// requireWholeInbox holds the read that names no kind to what naming none costs: the crowded
// node's whole inbox, every row of it read and all but inboxLimit of them thrown away. How the
// planner reaches those rows differs by mode - a sequential scan of the log where it sees the
// node's own values and knows the inbox is long, the whole of one index range sorted where it
// does not - so what is held here is the rows the plan read out of graph_tuples rather than the
// node it read them with. The generic plan is the one production runs, and it does not see the
// node's values, so it reads the inbox through an index: a generic plan that scans the log reads
// every node's tuples to answer one node's, which costs more than the whole inbox, so it fails
// here too. The rows read are printed on success as well, so the cost is on the record.
func requireWholeInbox(t *testing.T, subject, mode string, plan json.RawMessage, text string) {
	t.Helper()
	if mode == "force_generic_plan" && explaintest.SeqScansRelation(t, plan, "graph_tuples") {
		t.Errorf("%s scans graph_tuples, and under the generic plan this read takes the crowded node's inbox through an index and sorts it:\n%s",
			subject, text)
	}
	// EXPLAIN reports a parallel node's rows per loop and rounds them to two places, so a whole
	// inbox of 400,000 read by three workers reads back as 399,999.99. One row of slack covers
	// that rounding, and a plan that read any less than the inbox falls short of it by thousands.
	rows := tupleRowsRead(t, plan)
	if rows+1 < seedCrowdedInbox {
		t.Errorf("%s reads %.0f rows of graph_tuples, and naming no kind is supposed to cost the whole inbox of %d rows:\n%s",
			subject, rows, seedCrowdedInbox, text)
		return
	}
	t.Logf("%s reads %.0f rows of graph_tuples, the whole inbox of %d rows", subject, rows, seedCrowdedInbox)
}

// requireIndexCondition holds one index scan of graph_tuples to narrowing the index on a
// condition, which is the only thing that makes it read part of that index rather than all of
// it. A scan carrying no condition fails in one of two ways, and the failure says which: its
// only predicate is a filter, which is every row of the index read and most of them thrown away
// - the whole-table read under a better name - or it carries no predicate at all, which is the
// index read end to end. The read, the mode and the plan node are all in the message, so the
// reader knows what to fix without explaining the statement again.
func requireIndexCondition(t *testing.T, subject string, scan planNode, text string) {
	t.Helper()
	if scan.Cond != "" {
		return
	}
	if scan.Filter != "" {
		t.Errorf("%s reads graph_tuples through %s under no index condition: its only predicate is the filter `%s`, and an index scan that filters every row is the whole-table read under a better name:\n%s",
			subject, scan.scanName(), scan.Filter, text)
		return
	}
	t.Errorf("%s reads graph_tuples through %s under no index condition and no filter either, which is the whole of that index read end to end:\n%s",
		subject, scan.scanName(), text)
}

// storeRead is one read under test: what it is called, the statement and arguments the package
// sent for it, whether its plan is the whole-inbox read that naming no kind costs, how many
// index scans of graph_tuples it must hold, whether those are merged in time order, and the one
// read index every scan must take where the read is held to one. The answer each read gave is
// compared with the reference walk before the plan is explained, so a read that is wrong fails
// on its answer rather than on its plan.
type storeRead struct {
	name             string
	statement        recordedStatement
	wantWholeInbox   bool
	wantIndexScans   int
	wantMergedInTime bool
	wantOnlyIndex    string
}

// storeReads runs each read the package states, checks its answer against the reference walk and
// returns the statement each one sent. The eight reads are the five current and as-of reads of
// one node, the history of one of its relations, the walk of the nodes beneath the subtree's root,
// and the inbox read of the crowded node naming every kind the store holds into it; the ninth, the
// inbox read naming no kind, is the negative control that shows the kind list is what keeps that
// read off the whole inbox.
func storeReads(t *testing.T, ctx context.Context, pool *store.Pool, reference logReference) []storeRead {
	t.Helper()
	recorder := &recordedQueryer{Queryer: pool}
	reads := make([]storeRead, 0, 9)
	record := func(name string, wantWholeInbox bool, wantIndexScans int, merged bool) {
		t.Helper()
		reads = append(reads, storeRead{
			name: name, statement: recorder.last(t), wantWholeInbox: wantWholeInbox,
			wantIndexScans: wantIndexScans, wantMergedInTime: merged,
		})
	}

	out, err := CurrentOut(ctx, recorder, probeKind, probeID, nil)
	if err != nil {
		t.Fatalf("CurrentOut: %v", err)
	}
	requireSameTuples(t, "CurrentOut", out, reference.currentOut)
	record("CurrentOut", false, 1, false)
	// graph_tuples_current leaves delivered_to out, and a read of every kind, or of the kinds it
	// is handed, cannot prove that kind absent: it takes the index that keys every kind out of a node.
	reads[len(reads)-1].wantOnlyIndex = "graph_tuples_as_of_out"

	in, err := CurrentIn(ctx, recorder, probeKind, probeID, nil)
	if err != nil {
		t.Fatalf("CurrentIn: %v", err)
	}
	requireSameTuples(t, "CurrentIn", in, reference.currentIn)
	record("CurrentIn", false, 1, false)
	// No index keys the far end with the near end after it; the read into a node walks this one.
	reads[len(reads)-1].wantOnlyIndex = "graph_tuples_as_of_in"

	value, found, err := CurrentValue(ctx, recorder, probeKind, probeID, probeAttribute)
	if err != nil {
		t.Fatalf("CurrentValue: %v", err)
	}
	if value != reference.currentValue || found != reference.currentValueFound {
		t.Errorf("CurrentValue answers %q/%v, and the walk of the whole log answers %q/%v",
			value, found, reference.currentValue, reference.currentValueFound)
	}
	record("CurrentValue", false, 1, false)
	// The current value binds to_kind = 'value', and the plan is where that is read. Its one
	// read index is the one partial on that predicate, and the planner takes a partial index
	// only where the statement's own predicate implies the index's, under the generic plan as
	// much as under the custom one. A read that dropped the binding answers the same value
	// through graph_tuples_as_of_out, which keys every tuple out of a node, attributes among
	// them, so the answer alone would not catch it.
	reads[len(reads)-1].wantOnlyIndex = valueReadIndex(t)

	asOfOut, err := AsOfOut(ctx, recorder, probeKind, probeID, probeAsOf(), nil)
	if err != nil {
		t.Fatalf("AsOfOut: %v", err)
	}
	requireSameTuples(t, "AsOfOut", asOfOut, reference.asOfOut)
	record("AsOfOut", false, 1, false)

	asOfIn, err := AsOfIn(ctx, recorder, probeKind, probeID, probeAsOf(), nil)
	if err != nil {
		t.Fatalf("AsOfIn: %v", err)
	}
	requireSameTuples(t, "AsOfIn", asOfIn, reference.asOfIn)
	record("AsOfIn", false, 1, false)

	// History answers in log order, so its answer is held to the walk's row for row.
	history, err := History(ctx, recorder, probeKind, probeID, probeRelation)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	requireSameOrder(t, "History", history, reference.history)
	record("History", false, 1, false)

	// The walk beneath a node is one recursive statement, each step of it the current part_of
	// tuples into a node the step before reached, read by that node's kind and id as CurrentIn's
	// read into a node is: no index keys the far end with the near end after it, so every step
	// walks this one.
	beneath, err := nodesBeneath(ctx, recorder, subtreeKind, subtreeRoot)
	if err != nil {
		t.Fatalf("nodesBeneath: %v", err)
	}
	requireSameNodes(t, "nodesBeneath", beneath, reference.beneath)
	record("nodesBeneath", false, 1, false)
	reads[len(reads)-1].wantOnlyIndex = "graph_tuples_as_of_in"

	kinds := inboxKindsInTheStore(t, ctx, pool)
	inbox, err := TuplesIntoNode(ctx, recorder, crowdedKind, crowdedID, kinds, inboxLimit)
	if err != nil {
		t.Fatalf("TuplesIntoNode: %v", err)
	}
	// Naming every kind the store holds into the node is reading every tuple into it, so the
	// answer is the whole inbox's.
	requireSameOrder(t, "TuplesIntoNode naming the kinds the store holds", inbox, reference.inboxAnyKind)
	record("TuplesIntoNode naming the kinds the store holds", false, len(kinds), true)
	requireBindsKind(t, reads[len(reads)-1], crowdedSupertagKind)

	unkinded, err := TuplesIntoNode(ctx, recorder, crowdedKind, crowdedID, nil, inboxLimit)
	if err != nil {
		t.Fatalf("TuplesIntoNode naming no kind: %v", err)
	}
	requireSameOrder(t, "TuplesIntoNode naming no kind", unkinded, reference.inboxAnyKind)
	record("TuplesIntoNode naming no kind", true, 0, false)
	return reads
}

// inboxKindsInTheStore is the kinds of every tuple the store holds into the crowded node, which
// is the kind list the inbox read names. It is read off graph_tuples rather than written out
// here, and held to holding both the seed's own kinds and the supertag's, which the seed's list
// lacks: a list that missed the supertag's kind would read an inbox without its newest row.
func inboxKindsInTheStore(t *testing.T, ctx context.Context, pool *store.Pool) []string {
	t.Helper()
	if slices.Contains(crowdedInboxKinds, crowdedSupertagKind) {
		t.Fatalf("crowdedInboxKinds holds %s, so a read naming that list would name it too, and nothing tells the store's kinds from the list", crowdedSupertagKind)
	}
	rows, err := pool.Query(ctx, `
		select distinct kind from graph_tuples
		where to_kind = $1 and to_id = $2 and to_kind <> '`+KindValue+`'
		order by kind`, crowdedKind, crowdedID)
	if err != nil {
		t.Fatalf("read the kinds the store holds into %s/%s: %v", crowdedKind, crowdedID, err)
	}
	kinds, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the kinds the store holds into %s/%s: %v", crowdedKind, crowdedID, err)
	}
	for _, want := range append(slices.Clone(crowdedInboxKinds), crowdedSupertagKind) {
		if !slices.Contains(kinds, want) {
			t.Fatalf("the store holds the kinds %v into %s/%s, and the seed wrote %s there", kinds, crowdedKind, crowdedID, want)
		}
	}
	t.Logf("the store holds %d kinds into %s/%s: %v", len(kinds), crowdedKind, crowdedID, kinds)
	return kinds
}

// requireBindsKind holds the inbox read to binding kind, the one the store holds and the seed's
// own list lacks: the kinds are the statement's arguments, so a read that named a list of its
// own rather than the store's would bind every kind but that one.
func requireBindsKind(t *testing.T, read storeRead, kind string) {
	t.Helper()
	bound := slices.ContainsFunc(read.statement.args, func(argument any) bool {
		text, ok := argument.(string)
		return ok && text == kind
	})
	if !bound {
		t.Errorf("%s binds %v, which is not the kinds the store holds: it leaves out %s", read.name, read.statement.args, kind)
	}
}

// recordedStatement is one statement a read sent, with the arguments it bound.
type recordedStatement struct {
	sql  string
	args []any
}

// recordedQueryer is the Queryer a read is given so the plan can be explained from the statement
// the package itself sent. A test that kept its own copy of each statement would explain a copy,
// and an edit to a read would be explained as the copy rather than refused.
type recordedQueryer struct {
	Queryer
	sent []recordedStatement
}

func (r *recordedQueryer) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	r.sent = append(r.sent, recordedStatement{sql: sql, args: args})
	return r.Queryer.Query(ctx, sql, args...)
}

func (r *recordedQueryer) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	r.sent = append(r.sent, recordedStatement{sql: sql, args: args})
	return r.Queryer.QueryRow(ctx, sql, args...)
}

// last is the statement the read just sent. A read that sends more than one statement would make
// the plans below ambiguous, so the recorder is read one statement at a time.
func (r *recordedQueryer) last(t *testing.T) recordedStatement {
	t.Helper()
	if len(r.sent) == 0 {
		t.Fatal("the read sent no statement")
	}
	statement := r.sent[len(r.sent)-1]
	r.sent = r.sent[:0]
	return statement
}

// explainRecorded explains one recorded statement under one plan-cache mode, as JSON for the
// checks and as text for the report, under EXPLAIN (ANALYZE, BUFFERS) with every plan type left
// enabled: the plans read here are the ones the planner really takes at this size, not ones a
// pin forced on it.
//
// The statement is prepared and executed rather than explained with the read's arguments bound,
// because a plan-cache mode is what Postgres does with a prepared statement and an EXPLAIN that
// binds its own parameters has no plan cache to set a mode on. The parameter types are the ones
// Postgres infers from the statement itself, which is what pgx's own parse leaves it to; EXECUTE
// binds no parameter, so the arguments the read sent are written into it as literals. The
// transaction is rolled back, which is what deallocates the prepared statement.
func explainRecorded(
	t *testing.T, ctx context.Context, pool *store.Pool, name, mode string, statement recordedStatement,
) (json.RawMessage, string) {
	t.Helper()
	transaction, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction %s's plan is read in: %v", name, err)
	}
	defer transaction.Rollback(ctx)
	if _, err := transaction.Exec(ctx, "set local plan_cache_mode = "+mode); err != nil {
		t.Fatalf("set %s for %s: %v", mode, name, err)
	}
	prepared := preparedStatementName(name, mode)
	if _, err := transaction.Exec(ctx, "prepare "+prepared+" as "+statement.sql); err != nil {
		t.Fatalf("prepare %s: %v", name, err)
	}
	execute := "execute " + prepared + argumentLiterals(t, statement.args)
	var planJSON []byte
	if err := transaction.QueryRow(ctx, "explain (analyze, buffers, format json) "+execute).Scan(&planJSON); err != nil {
		t.Fatalf("explain %s under %s: %v", name, mode, err)
	}
	rows, err := transaction.Query(ctx, "explain (analyze, buffers) "+execute)
	if err != nil {
		t.Fatalf("explain %s under %s as text: %v", name, mode, err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("read a plan line of %s under %s: %v", name, mode, err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain %s under %s as text: %v", name, mode, err)
	}
	return explaintest.OnePlan(t, planJSON), strings.Join(lines, "\n")
}

// preparedStatementName is the name one read's plan is prepared under: the read's own name and
// the mode, written as an identifier, so the two plans of one read are two prepared statements
// and neither settles on the other's cached plan.
func preparedStatementName(name, mode string) string {
	return "plan_" + notAnIdentifier.ReplaceAllString(strings.ToLower(name), "_") + "_" + mode
}

// notAnIdentifier matches what a read's name holds that an identifier cannot.
var notAnIdentifier = regexp.MustCompile(`[^a-z0-9]+`)

// argumentLiterals writes the arguments a read bound as the literal list EXECUTE takes in their
// place, empty for a statement that binds none.
func argumentLiterals(t *testing.T, args []any) string {
	t.Helper()
	if len(args) == 0 {
		return ""
	}
	literals := make([]string, 0, len(args))
	for _, argument := range args {
		literals = append(literals, asLiteral(t, argument))
	}
	return "(" + strings.Join(literals, ", ") + ")"
}

// asLiteral writes one bound argument as the literal EXECUTE takes. These are the shapes this
// package's reads bind: a node address or a kind as text, a kind list or the null that stands
// for every kind, a moment, and a row bound. Each literal is written untyped, since the
// prepared statement's own parameter type is what Postgres reads it as.
func asLiteral(t *testing.T, argument any) string {
	t.Helper()
	switch value := argument.(type) {
	case nil:
		return "null"
	case string:
		return quoted(value)
	case []string:
		quotedValues := make([]string, 0, len(value))
		for _, element := range value {
			quotedValues = append(quotedValues, quoted(element))
		}
		return "array[" + strings.Join(quotedValues, ", ") + "]"
	case []*string:
		quotedValues := make([]string, 0, len(value))
		for _, element := range value {
			if element == nil {
				quotedValues = append(quotedValues, "null")
				continue
			}
			quotedValues = append(quotedValues, quoted(*element))
		}
		return "array[" + strings.Join(quotedValues, ", ") + "]::text[]"
	case int:
		return strconv.Itoa(value)
	case time.Time:
		return quoted(value.UTC().Format(time.RFC3339Nano))
	default:
		t.Fatalf("a read bound %T, which this test cannot write as a literal", argument)
		return ""
	}
}

// quoted writes one value as a SQL string literal, the one quote of it doubled.
func quoted(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }

// planNode is what the checks here read of one node of EXPLAIN (ANALYZE, FORMAT JSON): its
// type, the relation and the index it names, the two predicates it may carry, the rows it
// answered and the times it ran, and its children. Both predicates are kept, because the rule
// is read off the pair: an index condition is what makes a scan read part of an index, and a
// filter is what the scan does with rows it has already read. A parallel node answers its rows
// once per loop, so the rows it read are the two numbers multiplied.
type planNode struct {
	NodeType    string            `json:"Node Type"`
	Relation    string            `json:"Relation Name"`
	Index       string            `json:"Index Name"`
	Cond        string            `json:"Index Cond"`
	Filter      string            `json:"Filter"`
	ActualRows  float64           `json:"Actual Rows"`
	ActualLoops float64           `json:"Actual Loops"`
	Plans       []json.RawMessage `json:"Plans"`
}

// scanName names one scan node as a failure message cites it.
func (n planNode) scanName() string { return n.NodeType + " using " + n.Index }

// planNodes flattens plan - one "Plan" node of EXPLAIN (FORMAT JSON) - into every node of the
// tree under it, each node before its children. Every check below is a filter over this one
// walk, so the tree is descended in one place rather than once per check.
func planNodes(t *testing.T, plan json.RawMessage) []planNode {
	t.Helper()
	var node planNode
	if err := json.Unmarshal(plan, &node); err != nil {
		t.Fatalf("decode plan node: %v", err)
	}
	nodes := []planNode{node}
	for _, child := range node.Plans {
		nodes = append(nodes, planNodes(t, child)...)
	}
	return nodes
}

// readIndexScans names every index scan of graph_tuples in plan, which is what a read of one
// node must reach the log through. A node naming the relation and an index is an index scan of
// it; a bitmap's own index node names no relation, so a plan that reaches the log through a
// bitmap holds none of these and fails as a read through no index at all.
func readIndexScans(t *testing.T, plan json.RawMessage) []planNode {
	t.Helper()
	var scans []planNode
	for _, node := range planNodes(t, plan) {
		if node.Relation == "graph_tuples" && node.Index != "" {
			scans = append(scans, node)
		}
	}
	return scans
}

// tupleRowsRead is how many rows of graph_tuples the plan read: every node that names the
// relation, its rows times the times it ran. It is what tells a read of a page of an index from
// a read of the whole log, whichever node either one took.
func tupleRowsRead(t *testing.T, plan json.RawMessage) float64 {
	t.Helper()
	var rows float64
	for _, node := range planNodes(t, plan) {
		if node.Relation == "graph_tuples" {
			rows += node.ActualRows * node.ActualLoops
		}
	}
	return rows
}

// planHolds reports whether plan holds a node of the named type at any depth.
func planHolds(t *testing.T, plan json.RawMessage, nodeType string) bool {
	t.Helper()
	return slices.ContainsFunc(planNodes(t, plan), func(node planNode) bool {
		return node.NodeType == nodeType
	})
}

// reportBytesARow prints what one tuple costs the database at this size: the row count, the
// table with its indexes and TOAST (pg_total_relation_size), and the quotient. The growth
// projection of this change is read off this figure, so the test prints all three
// rather than the quotient alone.
func reportBytesARow(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	var rows, total int64
	if err := pool.QueryRow(ctx,
		`select count(*), pg_total_relation_size('graph_tuples') from graph_tuples`).Scan(&rows, &total); err != nil {
		t.Fatalf("measure the tuple log: %v", err)
	}
	if rows <= 1000000 {
		t.Fatalf("the log holds %d tuples; this test is only worth running above a million", rows)
	}
	t.Logf("graph_tuples: %d rows, pg_total_relation_size %d bytes (%.1f MiB), %d bytes a row",
		rows, total, float64(total)/float64(1<<20), total/rows)
}

// --- the seed ---------------------------------------------------------------------------

// seedTupleLog appends the log this test reads against, in bulk: one statement per shape, every
// row of a shape written by one insert ... select ... generate_series. Write appends row by row
// through the caller's slices, which at this size would be the test's whole running time, and
// the shapes below are what the log holds rather than how it came to hold it - the reads under
// test are checked against the log itself (walkTheWholeLog), not against the writer.
func seedTupleLog(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	const who = `'{"kind":"system","id":"seed"}'::jsonb`
	const at = `timestamptz '2026-01-01 00:00:00Z' + (n || ' seconds')::interval`
	const insert = `insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted) `
	for _, shape := range []struct {
		name string
		rows int
		sql  string
	}{
		{
			// Blocks citing issues: the outbound relation a current read answers.
			name: "outbound relations",
			rows: seedOutboundRelations,
			sql: insert + `select 'block', 'GRAPH/seed#b' || (n / 3 + 1), 'mentions', 'issue', 'GRAPH-' || (n % 3 + 1),
				` + at + `, ` + who + `, 1000000 + n, true from generate_series(0, $1::bigint) as n`,
		},
		{
			// A block's text: an attribute, whose to_id holds the value itself.
			name: "attributes",
			rows: seedAttributes,
			sql: insert + `select 'block', 'GRAPH/seed#b' || (n + 1), 'text', 'value', 'the seeded text of block ' || n,
				` + at + `, ` + who + `, 2000000 + n, true from generate_series(0, $1::bigint) as n`,
		},
		{
			// The crowded node's inbox, spread over its kinds.
			name: "the crowded node's inbox",
			rows: seedCrowdedInbox,
			sql: insert + `select 'comment', 'GRAPH/c' || n, (` + kindArray(crowdedInboxKinds) + `)[n % ` +
				strconv.Itoa(len(crowdedInboxKinds)) + ` + 1], '` + crowdedKind + `', '` + crowdedID + `',
				` + at + `, ` + who + `, 3000000 + n, true from generate_series(0, $1::bigint) as n`,
		},
		{
			// Replies into ordinary blocks: an inbox of a handful rather than of hundreds of
			// thousands, which is what CurrentIn and AsOfIn are read on.
			name: "ordinary inboxes",
			rows: seedOrdinaryInbox,
			sql: insert + `select 'comment', 'GRAPH/r' || n, 'replies_to', 'block', 'GRAPH/seed#b' || (n % ` +
				strconv.Itoa(seedInboxBlocks) + ` + 1),
				` + at + `, ` + who + `, 4000000 + n, true from generate_series(0, $1::bigint) as n`,
		},
		{
			// A relation that ended: a second tuple with asserted false, which a current read
			// must answer as absent rather than as ended.
			name: "retractions",
			rows: seedRetractions,
			sql: insert + `select 'block', 'GRAPH/seed#b' || (n / 3 + 1), 'mentions', 'issue', 'GRAPH-' || (n % 3 + 1),
				timestamptz '2026-02-01 00:00:00Z' + (n || ' seconds')::interval, ` + who + `, 5000000 + n, false
				from generate_series(0, $1::bigint) as n`,
		},
		{
			// The same relation stated again afterwards, so the newest word on it asserts.
			name: "re-assertions",
			rows: seedReassertions,
			sql: insert + `select 'block', 'GRAPH/seed#b' || (n / 3 + 1), 'mentions', 'issue', 'GRAPH-' || (n % 3 + 1),
				timestamptz '2026-03-01 00:00:00Z' + (n || ' seconds')::interval, ` + who + `, 6000000 + n, true
				from generate_series(0, $1::bigint) as n`,
		},
		{
			// Tuples whose at runs backwards as their seq runs forwards: a log's order and its
			// times are two orders, and an as-of read answers in the second.
			name: "tuples whose time runs against the log",
			rows: seedTimeReversedTuples,
			sql: insert + `select 'block', 'GRAPH/seed#b' || (n + 1), 'part_of', 'artifact', 'GRAPH/doc',
				timestamptz '2025-06-01 00:00:00Z' - (n || ' seconds')::interval, ` + who + `, 7000000 + n, true
				from generate_series(0, $1::bigint) as n`,
		},
	} {
		started := time.Now()
		tag, err := pool.Exec(ctx, shape.sql, shape.rows-1)
		if err != nil {
			t.Fatalf("seed %s: %v", shape.name, err)
		}
		if tag.RowsAffected() != int64(shape.rows) {
			t.Fatalf("seed %s appended %d rows, want %d", shape.name, tag.RowsAffected(), shape.rows)
		}
		t.Logf("seeded %d %s in %s", shape.rows, shape.name, time.Since(started).Round(time.Millisecond))
	}
}

// kindArray writes kinds as a SQL text array literal.
func kindArray(kinds []string) string {
	literals := make([]string, len(kinds))
	for index, kind := range kinds {
		literals[index] = quoted(kind)
	}
	return "array[" + strings.Join(literals, ",") + "]"
}

// writeSupertagIntoTheCrowdedNode writes one supertag node through Write, with one tuple into
// the crowded node under crowdedSupertagKind, a kind the seed's own list lacks. It is timed after
// every tuple the seed appends into that node, so it heads that node's inbox and an inbox read
// that names the seed's list rather than the store's kinds answers every row but this one.
func writeSupertagIntoTheCrowdedNode(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	at := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	who := model.Actor{Kind: "user", ID: "alice"}
	const event = 9200001
	node := Node{
		Kind: "supertag", ID: crowdedSupertagID, Supertag: "supertag",
		Visibility: VisibilityProject, State: StateLive,
		MadeAt: at, MadeBy: who, SourceEvent: event,
	}
	tuple := Tuple{
		FromKind: "supertag", FromID: crowdedSupertagID, Kind: crowdedSupertagKind,
		ToKind: crowdedKind, ToID: crowdedID, At: at, Who: who, SourceEvent: event, Asserted: true,
	}
	writeInOneTransaction(t, ctx, pool, []Node{node}, []Tuple{tuple},
		fmt.Sprintf("the supertag %s's tuple into %s/%s", crowdedSupertagID, crowdedKind, crowdedID))
}

// The probe node's own attribute and relations, and the moment the as-of reads are taken at.
const (
	probeAttribute = "text"
	probeRelation  = "mentions"
)

// probeStart is the probe's first write; every probe tuple is written at an offset from it, and
// Postgres keeps a timestamptz to the microsecond, so the times written are the times read back.
func probeStart() time.Time {
	return time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
}

// probeAsOf is the moment AsOfOut and AsOfIn are read at: inside the probe's own writes, so the
// answer is neither its whole history nor nothing.
func probeAsOf() time.Time { return probeStart().Add(150 * time.Minute) }

// writeProbeTuples states the probe node's facts through Write, the package's own writer, in
// five transactions: an attribute changed, a relation retracted and another stated with a time
// before the one already in the log, and an inbound relation retracted the same way. The reads
// under test are taken on this node, so every case they have to tell apart is in it.
func writeProbeTuples(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	start := probeStart()
	alice := model.Actor{Kind: "user", ID: "alice"}
	bob := model.Actor{Kind: "user", ID: "bob"}
	for _, write := range []struct {
		event  int64
		nodes  []Node
		tuples []Tuple
	}{
		{
			event: 9000001,
			nodes: []Node{probeNode("the probe as first written", start, alice, 9000001)},
			tuples: []Tuple{
				{FromKind: probeKind, FromID: probeID, Kind: probeAttribute, ToKind: KindValue,
					ToID: "the probe as first written", At: start, Who: alice, SourceEvent: 9000001, Asserted: true},
				{FromKind: probeKind, FromID: probeID, Kind: "part_of", ToKind: "artifact", ToID: "GRAPH/plan",
					At: start, Who: alice, SourceEvent: 9000001, Asserted: true},
				{FromKind: probeKind, FromID: probeID, Kind: probeRelation, ToKind: "issue", ToID: "GRAPH-9",
					At: start, Who: alice, SourceEvent: 9000001, Asserted: true},
			},
		},
		{
			event: 9000002,
			nodes: []Node{probeNode("the probe after Bob rewrote it", start.Add(2*time.Hour), bob, 9000002)},
			tuples: []Tuple{
				{FromKind: probeKind, FromID: probeID, Kind: probeAttribute, ToKind: KindValue,
					ToID: "the probe after Bob rewrote it", At: start.Add(2 * time.Hour), Who: bob,
					SourceEvent: 9000002, Asserted: true},
				{FromKind: probeKind, FromID: probeID, Kind: probeRelation, ToKind: "issue", ToID: "GRAPH-9",
					At: start.Add(2 * time.Hour), Who: bob, SourceEvent: 9000002, Asserted: false},
			},
		},
		{
			// Written after the two above and timed before them: an as-of read answers it at a
			// moment the log had not reached it.
			event: 9000003,
			tuples: []Tuple{
				{FromKind: probeKind, FromID: probeID, Kind: probeRelation, ToKind: "issue", ToID: "GRAPH-8",
					At: start.Add(-time.Hour), Who: alice, SourceEvent: 9000003, Asserted: true},
			},
		},
		{
			// One inbound relation before the as-of moment and one after it, so the as-of read
			// into the node answers a part of its inbox rather than all of it or none.
			event: 9000004,
			tuples: []Tuple{
				{FromKind: "comment", FromID: "GRAPH/cp1", Kind: "replies_to", ToKind: probeKind, ToID: probeID,
					At: start.Add(time.Hour), Who: bob, SourceEvent: 9000004, Asserted: true},
				{FromKind: "comment", FromID: "GRAPH/cp2", Kind: probeRelation, ToKind: probeKind, ToID: probeID,
					At: start.Add(3 * time.Hour), Who: bob, SourceEvent: 9000004, Asserted: true},
			},
		},
		{
			// The retraction of that second one, timed before the tuple it retracts: the newest
			// word by log order and the newest word at a moment are two different tuples here.
			event: 9000005,
			tuples: []Tuple{
				{FromKind: "comment", FromID: "GRAPH/cp2", Kind: probeRelation, ToKind: probeKind, ToID: probeID,
					At: start.Add(2 * time.Hour), Who: bob, SourceEvent: 9000005, Asserted: false},
			},
		},
	} {
		writeInOneTransaction(t, ctx, pool, write.nodes, write.tuples,
			fmt.Sprintf("the probe's write under event %d", write.event))
	}
}

// probeNode is the probe's row as one write leaves it.
func probeNode(value string, at time.Time, who model.Actor, event int64) Node {
	return Node{
		Kind: probeKind, ID: probeID, Supertag: "block",
		Visibility: VisibilityProject, State: StateLive,
		Attributes: map[string]any{probeAttribute: value},
		MadeAt:     at, MadeBy: who, SourceEvent: event,
	}
}

// The node whose one attribute is the long value.
const (
	longValueKind      = "block"
	longValueID        = "GRAPH/seed#long"
	longValueAttribute = "text"
)

// writeOneLongValue writes a value of longValueBytes and returns it. An index keying to_id
// would refuse the insert with "index row size ... exceeds btree version 4 maximum", so this
// write succeeding is the proof that no index of the set keys a column a value can reach.
//
// The value is random rather than written out, because an index tuple's datum is compressed
// before its size is judged: 3 KiB of one repeated sentence compresses to a few dozen bytes
// and fits in a key that a block's real text would not, so a readable value would pass this
// check against an index that a document's own text fails against.
func writeOneLongValue(t *testing.T, ctx context.Context, pool *store.Pool) string {
	t.Helper()
	value := randomValue(t, longValueBytes)
	at := probeStart()
	who := model.Actor{Kind: "user", ID: "alice"}
	node := Node{
		Kind: longValueKind, ID: longValueID, Supertag: "block",
		Visibility: VisibilityProject, State: StateLive,
		Attributes: map[string]any{longValueAttribute: value},
		MadeAt:     at, MadeBy: who, SourceEvent: 9100001,
	}
	tuple := Tuple{
		FromKind: longValueKind, FromID: longValueID, Kind: longValueAttribute, ToKind: KindValue,
		ToID: value, At: at, Who: who, SourceEvent: 9100001, Asserted: true,
	}
	writeInOneTransaction(t, ctx, pool, []Node{node}, []Tuple{tuple},
		fmt.Sprintf("the %d-byte value", longValueBytes))
	return value
}

// The subtree nodesBeneath is read on, which writeSubtree states through Write. Its root, a lens,
// holds a finding and a lens; the finding holds a lens, whose part_of names the finding by its
// address kind as a lens invoked on a finding does, with a finding of its own, which holds a lens
// whose part_of names that finding as a block, three levels further down; a gone lens under the
// root holds a lens that is not gone; a lens moved from under the root to another node holds a
// lens of its own; and the lens on the finding is a parent of the root too, so the walk meets the
// root again and has to end there.
const (
	subtreeKind          = KindLens
	subtreeRoot          = "GRAPH-tree-root"
	subtreeFinding       = "lens/GRAPH-tree-root#finding"
	subtreeOnRoot        = "GRAPH-tree-on-root"
	subtreeOnFinding     = "GRAPH-tree-on-finding"
	subtreeDeepFinding   = "lens/GRAPH-tree-on-finding#finding"
	subtreeOnDeepFinding = "GRAPH-tree-on-deep-finding"
	subtreeGone          = "GRAPH-tree-gone"
	subtreeUnderGone     = "GRAPH-tree-under-gone"
	subtreeMoved         = "GRAPH-tree-moved"
	subtreeUnderMoved    = "GRAPH-tree-under-moved"
)

// subtreeBeneath is what nodesBeneath answers of the subtree's root, each node as `<kind>/<id>`,
// sorted: every node under it but the gone lens, which the walk goes on through, and the moved
// lens with the lens under it, which stand under the root no more.
func subtreeBeneath() []string {
	beneath := []string{
		"block/" + subtreeFinding, "block/" + subtreeDeepFinding, KindLens + "/" + subtreeOnRoot,
		KindLens + "/" + subtreeOnFinding, KindLens + "/" + subtreeOnDeepFinding, KindLens + "/" + subtreeUnderGone,
	}
	slices.Sort(beneath)
	return beneath
}

// writeSubtree states the subtree through Write in two transactions: its nodes and their part_of
// tuples, then the move of the moved lens, which retracts its part_of at the root and states one
// at another node. Every node is live but the gone lens.
func writeSubtree(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	at := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	who := model.Actor{Kind: "user", ID: "alice"}
	const placed, moved = 9300001, 9300002
	node := func(kind, id, state string) Node {
		return Node{
			Kind: kind, ID: id, Supertag: kind, Visibility: VisibilityProject, State: state,
			MadeAt: at, MadeBy: who, SourceEvent: placed,
		}
	}
	partOf := func(fromKind, fromID, toKind, toID string, event int64, asserted bool) Tuple {
		return Tuple{
			FromKind: fromKind, FromID: fromID, Kind: TuplePartOf, ToKind: toKind, ToID: toID,
			At: at, Who: who, SourceEvent: event, Asserted: asserted,
		}
	}
	writeInOneTransaction(t, ctx, pool, []Node{
		node(KindLens, subtreeRoot, StateLive), node("block", subtreeFinding, StateLive),
		node(KindLens, subtreeOnRoot, StateLive), node(KindLens, subtreeOnFinding, StateLive),
		node("block", subtreeDeepFinding, StateLive), node(KindLens, subtreeOnDeepFinding, StateLive),
		node(KindLens, subtreeGone, StateGone), node(KindLens, subtreeUnderGone, StateLive),
		node(KindLens, subtreeMoved, StateLive), node(KindLens, subtreeUnderMoved, StateLive),
	}, []Tuple{
		partOf(KindLens, subtreeRoot, KindIssue, "GRAPH-1", placed, true),
		partOf("block", subtreeFinding, KindLens, subtreeRoot, placed, true),
		partOf(KindLens, subtreeOnRoot, KindLens, subtreeRoot, placed, true),
		partOf(KindLens, subtreeOnFinding, KindLensBlock, subtreeFinding, placed, true),
		partOf("block", subtreeDeepFinding, KindLens, subtreeOnFinding, placed, true),
		partOf(KindLens, subtreeOnDeepFinding, "block", subtreeDeepFinding, placed, true),
		partOf(KindLens, subtreeRoot, KindLens, subtreeOnFinding, placed, true),
		partOf(KindLens, subtreeGone, KindLens, subtreeRoot, placed, true),
		partOf(KindLens, subtreeUnderGone, KindLens, subtreeGone, placed, true),
		partOf(KindLens, subtreeMoved, KindLens, subtreeRoot, placed, true),
		partOf(KindLens, subtreeUnderMoved, KindLens, subtreeMoved, placed, true),
	}, "the subtree")
	// The root and the move are to an issue's node: a lens placed on a document at whose address no
	// artifacts row stands is refused (NoDocumentRowError), and this store holds no artifacts row.
	writeInOneTransaction(t, ctx, pool, nil, []Tuple{
		partOf(KindLens, subtreeMoved, KindLens, subtreeRoot, moved, false),
		partOf(KindLens, subtreeMoved, KindIssue, "GRAPH-1", moved, true),
	}, "the moved lens's move")
}

// valueAlphabet is what randomValue draws from: a text column holds text, and these are the
// characters a value is written in with no escaping anywhere between here and the index.
const valueAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// randomValue is bytes bytes of text no compressor can shorten.
func randomValue(t *testing.T, bytes int) string {
	t.Helper()
	drawn := make([]byte, bytes)
	if _, err := rand.Read(drawn); err != nil {
		t.Fatalf("draw a %d-byte value: %v", bytes, err)
	}
	for index, draw := range drawn {
		drawn[index] = valueAlphabet[int(draw)%len(valueAlphabet)]
	}
	return string(drawn)
}

// writeInOneTransaction commits one Write.
func writeInOneTransaction(t *testing.T, ctx context.Context, pool *store.Pool, nodes []Node, tuples []Tuple, what string) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin %s: %v", what, err)
	}
	defer tx.Rollback(ctx)
	if err := Write(ctx, tx, nodes, tuples); err != nil {
		t.Fatalf("write %s: %v", what, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit %s: %v", what, err)
	}
}

// --- the reference walk -----------------------------------------------------------------

// logReference is every read's answer as a walk of the whole log in Go computes it: the slow
// reference each index-served read is held to. It carries sequences rather than whole tuples,
// since a sequence names one row of the log and an index that answers fewer rows answers fewer
// sequences; beneath, the answer of the one read of nodes, carries each node as `<kind>/<id>`.
type logReference struct {
	currentOut        []int64
	currentIn         []int64
	currentValue      string
	currentValueFound bool
	asOfOut           []int64
	asOfIn            []int64
	history           []int64
	inboxAnyKind      []int64
	beneath           []string
}

// loggedFact is what the walk keeps of one row: enough to decide every read under test.
type loggedFact struct {
	seq              int64
	fromKind, fromID string
	kind             string
	toKind, toID     string
	at               time.Time
	asserted         bool
}

// walkTheWholeLog reads every tuple the store holds, in no particular order, and folds each
// read's answer out of it. Each fold is order-independent - the newest of a group by sequence,
// the newest at or before a moment, the top of an inbox by time, the rows of one relation sorted
// once at the end - so the walk keeps no more rows in memory than the answers hold. It binds
// nothing and names no node, so no index can serve it and nothing it answers can be short for
// the same reason a read's index could be: that is what makes it a reference.
func walkTheWholeLog(t *testing.T, ctx context.Context, pool *store.Pool) logReference {
	t.Helper()
	started := time.Now()
	asOf := probeAsOf()
	currentOut := newestOfEachGroup{by: bySeq}
	currentIn := newestOfEachGroup{by: bySeq}
	currentValue := newestOfEachGroup{by: bySeq}
	asOfOut := newestOfEachGroup{by: byTimeThenSeq}
	asOfIn := newestOfEachGroup{by: byTimeThenSeq}
	inboxAnyKind := topOfInbox{limit: inboxLimit}
	partOf := newestOfEachGroup{by: bySeq}
	var history []int64
	supertagTuple := int64(0)

	rows, err := pool.Query(ctx,
		`select seq, from_kind, from_id, kind, to_kind, to_id, at, asserted from graph_tuples`)
	if err != nil {
		t.Fatalf("walk the whole log: %v", err)
	}
	defer rows.Close()
	walked := 0
	for rows.Next() {
		var fact loggedFact
		if err := rows.Scan(&fact.seq, &fact.fromKind, &fact.fromID, &fact.kind,
			&fact.toKind, &fact.toID, &fact.at, &fact.asserted); err != nil {
			t.Fatalf("walk the whole log: %v", err)
		}
		walked++
		outOfProbe := fact.fromKind == probeKind && fact.fromID == probeID
		intoProbe := fact.toKind == probeKind && fact.toID == probeID
		intoCrowded := fact.toKind == crowdedKind && fact.toID == crowdedID
		switch {
		case outOfProbe && fact.toKind != KindValue:
			currentOut.offer([3]string{fact.kind, fact.toKind, fact.toID}, fact)
		case outOfProbe && fact.kind == probeAttribute:
			currentValue.offer([3]string{fact.kind}, fact)
		}
		if intoProbe && fact.toKind != KindValue {
			currentIn.offer([3]string{fact.kind, fact.fromKind, fact.fromID}, fact)
		}
		// AsOfOut names no to_kind, so an attribute's value at that moment is one of its
		// answers; AsOfIn reads into a node, and a value is no node.
		if outOfProbe && !fact.at.After(asOf) {
			asOfOut.offer([3]string{fact.kind, fact.toKind, fact.toID}, fact)
		}
		if intoProbe && fact.toKind != KindValue && !fact.at.After(asOf) {
			asOfIn.offer([3]string{fact.kind, fact.fromKind, fact.fromID}, fact)
		}
		// History answers every row of one kind out of a node, asserting and retracting alike.
		if outOfProbe && fact.kind == probeRelation {
			history = append(history, fact.seq)
		}
		if intoCrowded && fact.toKind != KindValue {
			inboxAnyKind.offer(fact)
			if fact.kind == crowdedSupertagKind {
				supertagTuple = fact.seq
			}
		}
		// The walk beneath a node reads the newest part_of tuple of each relation into a node.
		if fact.kind == TuplePartOf && fact.toKind != KindValue {
			partOf.offer([3]string{fact.fromKind + "\x00" + fact.fromID, fact.toKind, fact.toID}, fact)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("walk the whole log: %v", err)
	}
	if walked < seedTotal {
		t.Fatalf("the walk read %d tuples, want at least the %d the seed appended", walked, seedTotal)
	}
	slices.Sort(history)
	value, found := "", false
	if newest, ok := currentValue.by3([3]string{probeAttribute}); ok && newest.asserted {
		value, found = newest.toID, true
	}
	reference := logReference{
		currentOut:        currentOut.assertedSeqs(),
		currentIn:         currentIn.assertedSeqs(),
		currentValue:      value,
		currentValueFound: found,
		asOfOut:           asOfOut.assertedSeqs(),
		asOfIn:            asOfIn.assertedSeqs(),
		history:           history,
		inboxAnyKind:      inboxAnyKind.seqs(),
		beneath:           nodesBeneathInTheLog(t, ctx, pool, partOf),
	}
	t.Logf("walked %d tuples in %s; the reference holds %d current out, %d current in, %d as-of out, %d as-of in, %d of history",
		walked, time.Since(started).Round(time.Millisecond), len(reference.currentOut), len(reference.currentIn),
		len(reference.asOfOut), len(reference.asOfIn), len(reference.history))
	for name, answer := range map[string][]int64{
		"CurrentOut": reference.currentOut, "CurrentIn": reference.currentIn,
		"AsOfOut": reference.asOfOut, "AsOfIn": reference.asOfIn,
		"History": reference.history, "the inbox": reference.inboxAnyKind,
	} {
		if len(answer) == 0 {
			t.Fatalf("the reference answers nothing for %s, so comparing a read with it proves nothing", name)
		}
	}
	if !reference.currentValueFound {
		t.Fatal("the reference answers no current value for the probe, so comparing CurrentValue with it proves nothing")
	}
	// The walk beneath the subtree's root answers what the subtree holds beneath it: a walk that
	// stopped at the gone lens, kept the moved lens or looped at the cycle through the root would
	// answer otherwise, and holding nodesBeneath to it would hold the read to that.
	if want := subtreeBeneath(); !slices.Equal(reference.beneath, want) {
		t.Fatalf("the walk of the whole log answers the nodes beneath %s/%s as %v, and the subtree writeSubtree wrote holds %v beneath it",
			subtreeKind, subtreeRoot, reference.beneath, want)
	}
	// The supertag's tuple heads the crowded node's inbox, so a read that left its kind out
	// answers a different inbox; were it outside the newest inboxLimit rows, the two would
	// answer the same and the kind list would go untested.
	if supertagTuple == 0 || !slices.Contains(reference.inboxAnyKind, supertagTuple) {
		t.Fatalf("the supertag's %s tuple (seq %d) is not among the newest %d tuples into %s/%s, so an inbox read naming only %v would answer the same rows",
			crowdedSupertagKind, supertagTuple, inboxLimit, crowdedKind, crowdedID, crowdedInboxKinds)
	}
	return reference
}

// newestOfEachGroup folds the log into the newest fact of each group, under the order by says
// newest is: the log order for a current read, time and then log order for an as-of read.
type newestOfEachGroup struct {
	by     func(held, offered loggedFact) bool
	groups map[[3]string]loggedFact
}

// bySeq and byTimeThenSeq are the two orders a read calls one tuple newer than another in.
func bySeq(held, offered loggedFact) bool { return offered.seq > held.seq }
func byTimeThenSeq(held, offered loggedFact) bool {
	if !offered.at.Equal(held.at) {
		return offered.at.After(held.at)
	}
	return offered.seq > held.seq
}

func (n *newestOfEachGroup) offer(group [3]string, fact loggedFact) {
	if n.groups == nil {
		n.groups = map[[3]string]loggedFact{}
	}
	held, ok := n.groups[group]
	if !ok || n.by(held, fact) {
		n.groups[group] = fact
	}
}

func (n *newestOfEachGroup) by3(group [3]string) (loggedFact, bool) {
	fact, ok := n.groups[group]
	return fact, ok
}

// assertedSeqs is each group's newest fact, kept only where that newest one asserts, as the
// sequences of those rows in ascending order: a relation whose last word is a retraction is
// absent rather than answered as ended.
func (n *newestOfEachGroup) assertedSeqs() []int64 {
	seqs := make([]int64, 0, len(n.groups))
	for _, fact := range n.groups {
		if fact.asserted {
			seqs = append(seqs, fact.seq)
		}
	}
	slices.Sort(seqs)
	return seqs
}

// topOfInbox folds the log into the newest limit tuples into one node, by time and then by log
// order, which is the order the inbox read answers in. It keeps the answer and nothing else, so
// a node with half the log pointing into it costs the walk no memory.
type topOfInbox struct {
	limit int
	held  []loggedFact
}

func (i *topOfInbox) offer(fact loggedFact) {
	at, _ := slices.BinarySearchFunc(i.held, fact, func(held, offered loggedFact) int {
		if !held.at.Equal(offered.at) {
			if held.at.After(offered.at) {
				return -1
			}
			return 1
		}
		return int(offered.seq - held.seq)
	})
	if at >= i.limit {
		return
	}
	i.held = slices.Insert(i.held, at, fact)
	if len(i.held) > i.limit {
		i.held = i.held[:i.limit]
	}
}

func (i *topOfInbox) seqs() []int64 {
	seqs := make([]int64, len(i.held))
	for index, fact := range i.held {
		seqs[index] = fact.seq
	}
	return seqs
}

// requireSameTuples holds a read's answer to the reference's, as the set of log rows each
// names. The set rather than the order, because a read orders its answer by node addresses,
// which Postgres sorts in the database's collation and Go in byte order; the rows answered are
// what an index that reaches fewer of them gets wrong.
func requireSameTuples(t *testing.T, name string, answered []Tuple, want []int64) {
	t.Helper()
	got := make([]int64, len(answered))
	for index, tuple := range answered {
		got[index] = tuple.Seq
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("%s answers the log rows %v, and a walk of the whole log answers %v", name, got, want)
	}
}

// nodesBeneathInTheLog is nodesBeneath's answer for the subtree's root as a walk in Go computes
// it: the current part_of relations, each the newest part_of tuple of its relation where that one
// asserts, walked breadth first from the root to every node they reach, each once, a node reached
// through the tuples into it under its own kind and under its address kind (AddressKind); and of
// those, each node graph_nodes holds and not gone, but for the root, as `<kind>/<id>`, sorted.
func nodesBeneathInTheLog(t *testing.T, ctx context.Context, pool *store.Pool, partOf newestOfEachGroup) []string {
	t.Helper()
	under := map[[2]string][][2]string{}
	for _, fact := range partOf.groups {
		if fact.asserted {
			parent := [2]string{fact.toKind, fact.toID}
			under[parent] = append(under[parent], [2]string{fact.fromKind, fact.fromID})
		}
	}
	root := [2]string{subtreeKind, subtreeRoot}
	reached := map[[2]string]bool{root: true}
	for frontier := [][2]string{root}; len(frontier) > 0; {
		var next [][2]string
		for _, parent := range frontier {
			for _, spelled := range []string{parent[0], AddressKind(parent[0], parent[1])} {
				for _, child := range under[[2]string{spelled, parent[1]}] {
					if !reached[child] {
						reached[child] = true
						next = append(next, child)
					}
				}
			}
		}
		frontier = next
	}
	rows, err := pool.Query(ctx, `select kind, id, state from graph_nodes`)
	if err != nil {
		t.Fatalf("read the node rows: %v", err)
	}
	defer rows.Close()
	var beneath []string
	for rows.Next() {
		var held [2]string
		var state string
		if err := rows.Scan(&held[0], &held[1], &state); err != nil {
			t.Fatalf("read the node rows: %v", err)
		}
		if reached[held] && held != root && state != StateGone {
			beneath = append(beneath, held[0]+"/"+held[1])
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the node rows: %v", err)
	}
	slices.Sort(beneath)
	return beneath
}

// requireSameNodes holds a node read's answer to the reference's, as the set of `<kind>/<id>`
// each names, sorted in Go's byte order for the reason requireSameTuples gives.
func requireSameNodes(t *testing.T, name string, answered []Node, want []string) {
	t.Helper()
	got := make([]string, len(answered))
	for index, node := range answered {
		got[index] = node.Kind + "/" + node.ID
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("%s answers the nodes %v, and a walk of the whole log answers %v", name, got, want)
	}
}

// requireSameOrder holds an inbox read's answer to the reference's row for row and in order: an
// inbox read answers newest first by time and then by log order, both of them numbers, so the
// order is the same on either side.
func requireSameOrder(t *testing.T, name string, answered []Tuple, want []int64) {
	t.Helper()
	got := make([]int64, len(answered))
	for index, tuple := range answered {
		got[index] = tuple.Seq
	}
	if !slices.Equal(got, want) {
		t.Errorf("%s answers the log rows %v in that order, and a walk of the whole log answers %v", name, got, want)
	}
}

// --- the index set of the first migration ------------------------------------------------

// graphTupleReadIndexes are the four read indexes, which every plan above must reach the log
// through. They are named here as well as in requiredGraphIndexes because the plan checks ask
// which index a scan read, and the migration check asks which migration declares it.
var graphTupleReadIndexes = []string{
	"graph_tuples_current",
	"graph_tuples_values",
	"graph_tuples_as_of_out",
	"graph_tuples_as_of_in",
}

// valueReadPredicate is the predicate a read of one attribute binds, and the one read index
// partial on it is the index that read is held to.
const valueReadPredicate = "to_kind = '" + KindValue + "'"

// valueReadIndex is the read index declared partial on valueReadPredicate, taken from
// requiredGraphIndexes, so the index the value read is held to is the one the migration check
// and the catalog check hold to that predicate, and not a name written a second time here.
func valueReadIndex(t *testing.T) string {
	t.Helper()
	var partial []string
	for _, index := range requiredGraphIndexes {
		if slices.Contains(graphTupleReadIndexes, index.name) && index.predicate == valueReadPredicate {
			partial = append(partial, index.name)
		}
	}
	if len(partial) != 1 {
		t.Fatalf("the read indexes partial on %s are %v, want exactly one", valueReadPredicate, partial)
	}
	return partial[0]
}

// graphStoreMigration is the migration that must declare every index of the store. An index
// declared in a later migration is an index an existing deployment builds on a full
// graph_tuples, which is the thing this check exists to refuse.
const graphStoreMigration = "20261109013400_graph_store.up.sql"

// requiredGraphIndex is one index the first migration must declare, with the key list and the
// predicate it must declare it under.
type requiredGraphIndex struct {
	name, table, keys, predicate string
	unique                       bool
}

// requiredGraphIndexes is the whole index set of the node and tuple store: the four read indexes
// of graph_tuples, its two partial repeat keys, and the primary key and the one index of
// graph_nodes. The primary key is declared inside the create table rather than as a create
// index, so it is checked apart (requireIndexSetOfTheFirstMigration, requireIndexSetInTheCatalog).
var requiredGraphIndexes = []requiredGraphIndex{
	{
		name: "graph_tuples_current", table: "graph_tuples",
		keys: "from_kind, from_id, kind, to_kind, to_id, seq desc", predicate: "to_kind <> 'value' and kind <> 'delivered_to'",
	},
	{
		name: "graph_tuples_values", table: "graph_tuples",
		keys: "from_kind, from_id, kind, seq desc", predicate: "to_kind = 'value'",
	},
	{
		name: "graph_tuples_as_of_out", table: "graph_tuples",
		keys: "from_kind, from_id, kind, at desc, seq desc", predicate: "",
	},
	{
		name: "graph_tuples_as_of_in", table: "graph_tuples",
		keys: "to_kind, to_id, kind, at desc, seq desc", predicate: "to_kind <> 'value'",
	},
	{
		name: "graph_tuples_once", table: "graph_tuples", unique: true,
		keys:      "from_kind, from_id, kind, to_kind, to_id, source_event, asserted",
		predicate: "to_kind <> 'value' and source_event <> 0",
	},
	{
		// The value key carries the value through md5, because a value has no length bound and
		// a btree key is bounded at about 2,704 bytes: the digest keeps one event from stating
		// two values of one attribute of one node as one fact, which `do nothing` would drop,
		// without refusing the three-kilobyte value this test writes and reads back.
		name: "graph_tuples_value_once", table: "graph_tuples", unique: true,
		keys:      "from_kind, from_id, kind, md5(to_id), source_event, asserted",
		predicate: "to_kind = 'value' and source_event <> 0",
	},
	{
		name: "graph_nodes_supertag", table: "graph_nodes",
		keys: "supertag, kind, id", predicate: "",
	},
}

// laterGraphNodesIndexes are the indexes of graph_nodes a migration after the first declares. A
// table of a deployment already holds rows by then, so each is held to the one build that does not
// stop the store's writers while it scans: `create index concurrently` in a migration of its own
// that runs outside the runner's transaction (`-- pgmigrate:non-transactional`,
// pgmigrate/nontransactional.go). graph_nodes_project is the read of one project's nodes by kind
// (20261116015042 gave graph_nodes its project_key), graph_nodes_proposed the read of the
// nodes at proposed under one issue (api/node_state.go, issueProposedQuery), which holds the few
// rows at that state alone, and graph_nodes_interaction_live the sweep's read of the runs past the
// hour whose row is gone (api/going.go, goingRowlessRuns), which holds the interaction nodes of
// the source going_requests not gone, about an hour of runs.
var laterGraphNodesIndexes = []requiredGraphIndex{
	{
		name: "graph_nodes_project", table: "graph_nodes",
		keys: "project_key, kind", predicate: "project_key is not null",
	},
	{
		name: "graph_nodes_proposed", table: "graph_nodes",
		keys: "kind, id", predicate: "state = 'proposed'",
	},
	{
		name: "graph_nodes_interaction_live", table: "graph_nodes",
		keys:      "made_at",
		predicate: "kind = 'interaction' and state <> 'gone' and attributes->>'source' = 'going_requests'",
	},
}

// laterGraphTuplesIndexes are the indexes of graph_tuples a migration after the first declares,
// held as laterGraphNodesIndexes' are to a concurrent build in a non-transactional migration of its
// own, since graph_tuples holds every tuple of a deployment by then. graph_tuples_workflow_inbound
// is the index of the workflow store's old tuples table that the release before's read of an
// issue's runs and plans needs, through the view that took the table's name
// (workflowstore.ProjectPlans at the release before: the view's rows are the tuples out of a step
// or a plan, and the read keys them by the issue they are part of and by the near end's kind). It
// is partial on the near end being a step or a plan, so no other tuple pays a write for it, and on
// to_kind <> 'value', which keeps a value, a plan's tasks among them, out of a key a btree bounds.
// startup.StoresMovedRetirement drops it with the views.
var laterGraphTuplesIndexes = []requiredGraphIndex{
	{
		name: "graph_tuples_workflow_inbound", table: "graph_tuples",
		keys: "to_kind, to_id, kind, from_kind, from_id, seq desc", predicate: "to_kind <> 'value' and from_kind in ('step', 'plan')",
	},
}

// declaredIndex is one create index a migration file holds, with the statement that declares
// it. An index declared under no name of its own has the empty name: Postgres makes one up.
type declaredIndex struct {
	migration, statement string
	name, table          string
	keys, predicate      string
	unique               bool
	// concurrent is a build `create index concurrently` in a migration whose first line is
	// `-- pgmigrate:non-transactional`, the one way such a build reaches the database.
	concurrent bool
}

// called is how a failure names the index: by its name, or as one Postgres names.
func (index declaredIndex) called() string {
	if index.name == "" {
		return "an index under no name of its own, which Postgres names,"
	}
	return index.name
}

// requireIndexSetOfTheFirstMigration reads the migration files in version order and holds the
// store's index set to being declared in the first of the two, exactly as it is written there.
// It is a read of the files rather than of the catalog because the catalog cannot tell an index
// the first migration built on an empty table from one a later migration builds on a full one,
// and the second is a build that locks graph_tuples against every writer of a running
// deployment.
func requireIndexSetOfTheFirstMigration(t *testing.T) {
	t.Helper()
	files := migrationFilesInOrder(t)
	declared := map[string][]declaredIndex{}
	graphNodesTable := ""
	for _, file := range files {
		raw := readMigration(t, file)
		firstLine, _, _ := strings.Cut(raw, "\n")
		body := withoutLineComments(raw)
		for _, index := range indexesOf(file, body, firstLine) {
			declared[index.name] = append(declared[index.name], index)
		}
		if table := graphNodesCreateTable.FindStringSubmatch(body); table != nil {
			if graphNodesTable != "" {
				t.Errorf("%s creates graph_nodes, which %s already created", file, graphNodesTable)
			}
			graphNodesTable = file
			if !strings.Contains(normalise(table[1]), "primary key (kind, id)") {
				t.Errorf("%s creates graph_nodes without `primary key (kind, id)`; every read of one node and every foreign key of a projection row rests on it", file)
			}
		}
	}
	if graphNodesTable != graphStoreMigration {
		t.Errorf("graph_nodes is created in %q, want %s", graphNodesTable, graphStoreMigration)
	}

	for _, want := range requiredGraphIndexes {
		found := declared[want.name]
		if len(found) == 0 {
			t.Errorf("no migration declares %s on %s; the store's reads rest on it", want.name, want.table)
			continue
		}
		for _, index := range found[1:] {
			t.Errorf("%s is declared again in %s; an index of this set belongs in %s, where it is built on an empty table",
				want.name, index.migration, graphStoreMigration)
		}
		index := found[0]
		if index.migration != graphStoreMigration {
			t.Errorf("%s is declared in %s, not in %s: an existing deployment would build it on a full graph_tuples",
				want.name, index.migration, graphStoreMigration)
		}
		if index.table != want.table || index.keys != want.keys || index.predicate != want.predicate || index.unique != want.unique {
			t.Errorf("%s is declared as unique=%v on %s (%s)%s, want unique=%v on %s (%s)%s",
				want.name, index.unique, index.table, index.keys, wherePart(index.predicate),
				want.unique, want.table, want.keys, wherePart(want.predicate))
		}
	}
	for _, want := range slices.Concat(laterGraphNodesIndexes, laterGraphTuplesIndexes) {
		found := declared[want.name]
		if len(found) != 1 {
			t.Errorf("%s is declared %d times, want once, in a migration of its own", want.name, len(found))
			continue
		}
		index := found[0]
		if index.migration == graphStoreMigration || !index.concurrent {
			t.Errorf("%s is declared in %s with concurrent=%v, want a concurrent build in a later, non-transactional migration:"+
				" %s holds a row for every block or tuple by then, and a plain build stops every writer of the store while it scans",
				want.name, index.migration, index.concurrent, want.table)
		}
		if index.table != want.table || index.keys != want.keys || index.predicate != want.predicate || index.unique != want.unique {
			t.Errorf("%s is declared as unique=%v on %s (%s)%s, want unique=%v on %s (%s)%s",
				want.name, index.unique, index.table, index.keys, wherePart(index.predicate),
				want.unique, want.table, want.keys, wherePart(want.predicate))
		}
	}

	// A value has no length bound and a btree key is bounded at about 2,704 bytes, so an index
	// keying to_id refuses the insert of a tuple whose to_id holds one. Every such index is
	// partial on to_kind, which is what keeps the values out of it. A key holds the far end
	// whether it names to_id bare, sorted (to_id desc) or through an expression (md5(to_id),
	// left(to_id, 64)), so the rule reads every key that mentions the column: an expression that
	// shrinks the value passes the long write this test makes and still keys a value and an
	// address in one index, which is what a to_kind predicate keeps apart.
	for _, indexes := range declared {
		for _, index := range indexes {
			if index.table != "graph_tuples" || !keysTheFarEnd.MatchString(index.keys) {
				continue
			}
			if !strings.Contains(index.predicate, "to_kind") {
				t.Errorf("%s (%s) in %s keys the far end under no to_kind predicate: a value and an address share that key, and a key over to_id itself refuses the insert of a long value: %s",
					index.called(), index.keys, index.migration, index.statement)
			}
		}
	}

	// The set is the whole of graph_tuples' indexes, and the one later index of laterGraphTuplesIndexes.
	// Each index on that table is one more write every tuple pays for, and the set leaves out the
	// ones no read needs: an index keyed on the far end with the near end after it cost a
	// delivered_to tuple 8.1 WAL records where the four read indexes cost 6.1, and served no read
	// graph_tuples_as_of_in does not. The later index is partial on the near end's kind, so a
	// delivered_to tuple pays nothing for it.
	for _, indexes := range declared {
		for _, index := range indexes {
			inTheSet := slices.ContainsFunc(requiredGraphIndexes, func(want requiredGraphIndex) bool { return want.name == index.name }) ||
				slices.ContainsFunc(laterGraphTuplesIndexes, func(want requiredGraphIndex) bool { return want.name == index.name })
			if index.table == "graph_tuples" && !inTheSet {
				t.Errorf("%s declares %s on graph_tuples (%s)%s, an index outside the store's index set: graph_tuples carries its four read indexes %v and its two repeat keys, and %d later index, and no other; the statement: %s",
					index.migration, index.called(), index.keys, wherePart(index.predicate), graphTupleReadIndexes, len(laterGraphTuplesIndexes), index.statement)
			}
		}
	}
	t.Logf("the index set of %s: %d indexes read from the migration files in order, over %d files",
		graphStoreMigration, len(requiredGraphIndexes)+1, len(files))
}

// requireIndexSetInTheCatalog holds the migrated database to the index set of
// requiredGraphIndexes, read off its catalog. requireIndexSetOfTheFirstMigration reads the
// migration files for `create index` alone, so a later migration that drops one of the eight
// passes it, and the reads that index served then run on whatever is left. Each of the seven
// must stand on its table under its name, unique where the set says so, with the set's key list
// and predicate; graph_nodes' primary key must key (kind, id). An index the set does not name,
// such as graph_tuples' own primary key, is not this check's business.
//
// The catalog writes a definition back in its own form: `seq DESC`, and a literal with its type
// cast (`'value'::text`). The key list is cut out of pg_indexes' indexdef and the predicate is
// pg_get_expr's pretty form, which writes no parentheses it does not need; both are then brought
// to the form the set writes them in before they are compared.
func requireIndexSetInTheCatalog(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	rows, err := pool.Query(ctx, `
		select i.indexname, i.tablename, i.indexdef, x.indisunique, x.indisprimary,
		       coalesce(pg_get_expr(x.indpred, x.indrelid, true), '')
		from pg_indexes i
		join pg_index x on x.indexrelid = format('%I.%I', i.schemaname, i.indexname)::regclass
		where i.schemaname = current_schema() and i.tablename in ('graph_tuples', 'graph_nodes')
		order by i.tablename, i.indexname`)
	if err != nil {
		t.Fatalf("read the store's indexes out of the catalog: %v", err)
	}
	defer rows.Close()
	standing := map[string]requiredGraphIndex{}
	nodesPrimaryKey := ""
	for rows.Next() {
		var name, table, definition, predicate string
		var unique, primary bool
		if err := rows.Scan(&name, &table, &definition, &unique, &primary, &predicate); err != nil {
			t.Fatalf("read an index of the store out of the catalog: %v", err)
		}
		keys := catalogIndexKeys.FindStringSubmatch(definition)
		if keys == nil {
			t.Fatalf("read the key list of %s out of the catalog's definition %q", name, definition)
		}
		standing[name] = requiredGraphIndex{
			name: name, table: table, unique: unique,
			keys: normaliseKeys(keys[1]), predicate: normalise(strings.ReplaceAll(predicate, "::text", "")),
		}
		if table == "graph_nodes" && primary {
			nodesPrimaryKey = normaliseKeys(keys[1])
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the store's indexes out of the catalog: %v", err)
	}
	for _, want := range requiredGraphIndexes {
		got, held := standing[want.name]
		switch {
		case !held:
			t.Errorf("the migrated database holds no index %s on %s; the store's reads rest on it, so a migration dropped it or never built it",
				want.name, want.table)
		case got != want:
			t.Errorf("the migrated database holds %s as unique=%v on %s (%s)%s, want unique=%v on %s (%s)%s",
				want.name, got.unique, got.table, got.keys, wherePart(got.predicate),
				want.unique, want.table, want.keys, wherePart(want.predicate))
		}
	}
	// The catalog writes the later index's predicate in its own form (`from_kind = ANY (ARRAY[...])`
	// for `from_kind in (...)`), so it is held to its table, its key list and a predicate that
	// names both columns it is partial on.
	for _, want := range laterGraphTuplesIndexes {
		got, held := standing[want.name]
		switch {
		case !held:
			t.Errorf("the migrated database holds no index %s on %s; the release before's read of the workflow store's tuples through their view rests on it, so a migration dropped it or never built it",
				want.name, want.table)
		case got.table != want.table || got.keys != want.keys || got.unique != want.unique ||
			!strings.Contains(got.predicate, "to_kind <> 'value'") || !strings.Contains(got.predicate, "from_kind"):
			t.Errorf("the migrated database holds %s as unique=%v on %s (%s)%s, want unique=%v on %s (%s), partial on to_kind <> 'value' and from_kind",
				want.name, got.unique, got.table, got.keys, wherePart(got.predicate), want.unique, want.table, want.keys)
		}
	}
	if nodesPrimaryKey != "kind, id" {
		t.Errorf("the migrated database keys graph_nodes' primary key on (%s), want (kind, id); every read of one node and every foreign key of a projection row rests on it",
			nodesPrimaryKey)
	}
	t.Logf("the migrated database's catalog holds %d indexes on graph_tuples and graph_nodes, read against the %d of the store's index set",
		len(standing), len(requiredGraphIndexes)+1)
}

// catalogIndexKeys cuts the key list out of a definition pg_indexes writes, which reads
// `CREATE [UNIQUE] INDEX name ON schema.table USING method (keys)` with ` WHERE predicate` after
// it on a partial index. The lazy match stops at the bracket that closes the key list, since a
// key's own brackets (md5(to_id)) are followed by a comma or by that bracket and never by the end
// of the definition or ` WHERE`.
var catalogIndexKeys = regexp.MustCompile(`(?is) using \w+ (\(.*?\))(?: where .*)?$`)

// keysTheFarEnd matches an index key list that mentions to_id anywhere, bare or inside an
// expression.
var keysTheFarEnd = regexp.MustCompile(`\bto_id\b`)

// migrationsDirectory is where the migration files are, read from this package's own directory.
const migrationsDirectory = "../store/migrations"

// migrationFilesInOrder names every up migration in the order the runner applies them, by the
// version its file name starts with: an index is held to the migration it is declared in, so the
// order the files are read in is the order they run in and not the order a directory lists them.
func migrationFilesInOrder(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(migrationsDirectory)
	if err != nil {
		t.Fatalf("read the migrations directory: %v", err)
	}
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".up.sql") {
			files = append(files, entry.Name())
		}
	}
	if len(files) == 0 {
		t.Fatalf("%s holds no up migration", migrationsDirectory)
	}
	slices.SortFunc(files, func(left, right string) int {
		return int(migrationVersion(t, left) - migrationVersion(t, right))
	})
	return files
}

// migrationVersion is the number a migration's file name starts with, which is the order the
// runner applies it in (pgmigrate.Load).
func migrationVersion(t *testing.T, file string) int64 {
	t.Helper()
	digits := file[:strings.IndexByte(file, '_')]
	version, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		t.Fatalf("read the version of migration %s: %v", file, err)
	}
	return version
}

func readMigration(t *testing.T, file string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(migrationsDirectory, file))
	if err != nil {
		t.Fatalf("read migration %s: %v", file, err)
	}
	return string(body)
}

// indexDeclaration matches one create index up to the semicolon that ends it, in every form
// Postgres takes one in: in either case; unique or not; concurrently or not; if not exists or
// not; under a name of its own, or none, which Postgres then makes up; on the table, or on only
// the table; and with the table under its schema or not. Each name is a SQL identifier, bare or
// quoted (sqlIdentifier, read through sqlName). It captures whether the index is unique, whether
// it is built concurrently, its name, its table and what follows the table. No index of this tree
// writes a semicolon inside its own definition.
var indexDeclaration = regexp.MustCompile(`(?is)\bcreate\s+(unique\s+)?index\s+(concurrently\s+)?(?:if\s+not\s+exists\s+)?` +
	`(?:(` + sqlIdentifier + `)\s+)?on\s+(?:only\s+)?(?:` + sqlIdentifier + `\s*\.\s*)?(` + sqlIdentifier + `)\s*([^;]*);`)

// sqlIdentifier matches one SQL identifier: a bare one, or a quoted one, inside which a doubled
// quote stands for one.
const sqlIdentifier = `(?:[a-z_][a-z0-9_$]*|"(?:[^"]|"")+")`

// sqlName is the name an identifier sqlIdentifier matched names, as Postgres reads it: a bare
// one folded to lower case, a quoted one as written, without its quotes.
func sqlName(identifier string) string {
	if quoted, ok := strings.CutPrefix(identifier, `"`); ok {
		return strings.ReplaceAll(strings.TrimSuffix(quoted, `"`), `""`, `"`)
	}
	return strings.ToLower(identifier)
}

// indexPredicateStart matches where an index's key list ends and its partial predicate begins.
var indexPredicateStart = regexp.MustCompile(`(?is)\)\s*where\b`)

// graphNodesCreateTable matches the create table of graph_nodes and captures its body, which is
// where its primary key is declared.
var graphNodesCreateTable = regexp.MustCompile(`(?is)\bcreate\s+table\s+graph_nodes\s*\((.*?)\n\);`)

// lineComment matches a whole line of SQL comment, which the migrations write their reasoning
// in: one of them spells out `create index concurrently` in prose, and reading the prose as a
// declaration would hold the tree to an index nobody built.
var lineComment = regexp.MustCompile(`(?m)^[ \t]*--.*$`)

func withoutLineComments(body string) string { return lineComment.ReplaceAllString(body, "") }

// indexesOf is every index one migration file declares, with its key list and predicate written
// the way requiredGraphIndexes writes them, its name and its table as Postgres reads them
// (sqlName), the table without its schema, and the statement as it is written, on one line. body
// is the file with its comments taken out, so whether the file runs outside the runner's
// transaction is read off firstLine, the file's own.
func indexesOf(migration, body, firstLine string) []declaredIndex {
	nonTransactional := strings.TrimSpace(firstLine) == "-- pgmigrate:non-transactional"
	var indexes []declaredIndex
	for _, match := range indexDeclaration.FindAllStringSubmatch(body, -1) {
		keys, predicate := splitIndexBody(match[5])
		indexes = append(indexes, declaredIndex{
			migration:  migration,
			statement:  strings.TrimSpace(whitespaceRun.ReplaceAllString(match[0], " ")),
			name:       sqlName(match[3]),
			table:      sqlName(match[4]),
			keys:       keys,
			predicate:  predicate,
			unique:     strings.TrimSpace(match[1]) != "",
			concurrent: nonTransactional && strings.TrimSpace(match[2]) != "",
		})
	}
	return indexes
}

// TestIndexesOfReadsEveryCreateIndexSpelling holds indexesOf to reading the name, the table and
// the rest of the index out of each spelling of a create index that indexDeclaration lists, one
// row a spelling, so an index a migration declares in any of them is one the index-set scan
// reads rather than one it passes over. It reads no database.
func TestIndexesOfReadsEveryCreateIndexSpelling(t *testing.T) {
	for _, row := range []struct {
		spelling, statement string
		want                declaredIndex
	}{
		{
			spelling: "under a name of its own", statement: `create index graph_tuples_named on graph_tuples (kind, to_id) where to_kind <> 'value';`,
			want: declaredIndex{name: "graph_tuples_named", table: "graph_tuples", keys: "kind, to_id", predicate: "to_kind <> 'value'"},
		},
		{
			spelling: "under no name, which Postgres makes up", statement: `create index on graph_tuples (kind);`,
			want: declaredIndex{name: "", table: "graph_tuples", keys: "kind"},
		},
		{
			spelling: "under a quoted name holding a doubled quote", statement: `create index "Graph ""Quoted"" Index" on graph_tuples (kind);`,
			want: declaredIndex{name: `Graph "Quoted" Index`, table: "graph_tuples", keys: "kind"},
		},
		{
			spelling: "on a quoted table", statement: `create index graph_tuples_quoted_table on "Graph_Tuples" (kind);`,
			want: declaredIndex{name: "graph_tuples_quoted_table", table: "Graph_Tuples", keys: "kind"},
		},
		{
			spelling: "on the table under its schema", statement: `create index graph_tuples_schema on public.graph_tuples (kind);`,
			want: declaredIndex{name: "graph_tuples_schema", table: "graph_tuples", keys: "kind"},
		},
		{
			spelling: "on the table under its quoted schema, spaced round the dot", statement: `create index graph_tuples_quoted_schema on "public" . graph_tuples (kind);`,
			want: declaredIndex{name: "graph_tuples_quoted_schema", table: "graph_tuples", keys: "kind"},
		},
		{
			spelling: "on only the table", statement: `create index graph_tuples_only on only graph_tuples (kind);`,
			want: declaredIndex{name: "graph_tuples_only", table: "graph_tuples", keys: "kind"},
		},
		{
			spelling: "in upper case", statement: `CREATE UNIQUE INDEX GRAPH_TUPLES_UPPER ON GRAPH_TUPLES (FROM_KIND, KIND) WHERE SOURCE_EVENT <> 0;`,
			want: declaredIndex{name: "graph_tuples_upper", table: "graph_tuples", keys: "from_kind, kind", predicate: "source_event <> 0", unique: true},
		},
		{
			spelling: "unique", statement: `create unique index graph_tuples_unique on graph_tuples (from_kind, md5(to_id)) where to_kind = 'value';`,
			want: declaredIndex{name: "graph_tuples_unique", table: "graph_tuples", keys: "from_kind, md5(to_id)", predicate: "to_kind = 'value'", unique: true},
		},
		{
			spelling: "if not exists", statement: `create index if not exists graph_tuples_maybe on graph_tuples (kind);`,
			want: declaredIndex{name: "graph_tuples_maybe", table: "graph_tuples", keys: "kind"},
		},
		{
			spelling: "concurrently", statement: `create index concurrently graph_tuples_concurrent on graph_tuples (kind);`,
			want: declaredIndex{name: "graph_tuples_concurrent", table: "graph_tuples", keys: "kind"},
		},
		{
			spelling: "concurrently, under no name", statement: `create index concurrently on graph_tuples (kind);`,
			want: declaredIndex{name: "", table: "graph_tuples", keys: "kind"},
		},
		{
			spelling: "every option at once, over several lines", statement: "create unique index concurrently if not exists\n\tgraph_tuples_every\n\ton only public.graph_tuples (\n\t\tfrom_kind,\n\t\tkind\n\t)\n\twhere source_event <> 0;",
			want: declaredIndex{name: "graph_tuples_every", table: "graph_tuples", keys: "from_kind, kind", predicate: "source_event <> 0", unique: true},
		},
	} {
		const migration = "0_spelling.up.sql"
		read := indexesOf(migration, "create table unrelated (id bigint);\n"+row.statement+"\nselect 1;\n", "")
		if len(read) != 1 {
			t.Errorf("%s: indexesOf reads %d indexes out of %q, want one", row.spelling, len(read), row.statement)
			continue
		}
		got := read[0]
		row.want.migration = migration
		row.want.statement = strings.TrimSpace(whitespaceRun.ReplaceAllString(row.statement, " "))
		if got != row.want {
			t.Errorf("%s: indexesOf reads %q as %q unique=%v on %q (%s)%s, statement %q; want %q unique=%v on %q (%s)%s, statement %q",
				row.spelling, row.statement, got.name, got.unique, got.table, got.keys, wherePart(got.predicate), got.statement,
				row.want.name, row.want.unique, row.want.table, row.want.keys, wherePart(row.want.predicate), row.want.statement)
		}
	}
}

// splitIndexBody takes an index's key list and its partial predicate out of what follows the
// table name, each normalised to one line of single spaces.
func splitIndexBody(body string) (keys, predicate string) {
	if at := indexPredicateStart.FindStringIndex(body); at != nil {
		return normaliseKeys(body[:at[0]+1]), normalise(body[at[1]:])
	}
	return normaliseKeys(body), ""
}

// normaliseKeys is an index's key list without its brackets, each key separated by one comma and
// one space, so a key list is compared by what it keys rather than by how it is laid out.
func normaliseKeys(keys string) string {
	keys = strings.TrimSpace(normalise(keys))
	keys = strings.TrimPrefix(keys, "(")
	keys = strings.TrimSuffix(keys, ")")
	parts := strings.Split(keys, ",")
	for index, part := range parts {
		parts[index] = strings.TrimSpace(part)
	}
	return strings.Join(parts, ", ")
}

// normalise writes one SQL fragment as one lower-case line of single spaces.
var whitespaceRun = regexp.MustCompile(`\s+`)

func normalise(fragment string) string {
	return strings.TrimSpace(whitespaceRun.ReplaceAllString(strings.ToLower(fragment), " "))
}

// wherePart writes a predicate as a failure message cites it.
func wherePart(predicate string) string {
	if predicate == "" {
		return ""
	}
	return " where " + predicate
}
