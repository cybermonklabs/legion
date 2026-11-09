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
// has in production, and the shape the five read indexes exist for. crowded is the node with a
// long inbox, where TuplesIntoNode naming its kinds reads one index scan per kind and naming
// none has no kind to scan by and falls to a scan of the log.
const (
	probeKind   = "block"
	probeID     = "GRAPH/seed#probe"
	crowdedKind = "block"
	crowdedID   = "GRAPH/seed#crowded"
)

// crowdedInboxKinds are the kinds the crowded node's inbox holds, which the kinded inbox read
// names. TupleKinds answers the same list from the store plus the kinds no tuple points into
// this node under; the list is written out here so the count of index scans the plan must hold
// is a number this test knows.
var crowdedInboxKinds = []string{"replies_to", "mentions", "answers", "part_of", "anchored_to"}

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

// TestEveryCurrentStoreReadRunsThroughTheTupleIndexes holds the five read indexes of the first
// migration to carrying every read the package states, against a log of more than a million
// tuples. It explains each read as the package itself sends it (recordedQueryer), so a statement
// that is edited is explained as edited rather than against a copy of it kept here; it checks
// each read's answer against a reference that walks the whole log in Go, so an index that
// answers fewer rows than the log holds fails here rather than passing as a faster plan; and it
// reads the migration files themselves, so an index moved to a later migration - where an
// existing deployment would build it on a full table - fails too.
//
// It also reports the bytes a row the store costs at this size, which the issue's growth
// projection is read off.
func TestEveryCurrentStoreReadRunsThroughTheTupleIndexes(t *testing.T) {
	ctx := context.Background()
	requireIndexSetOfTheFirstMigration(t)

	database := storetest.Open(t)
	pool := database.Pool
	seedTupleLog(t, ctx, pool)
	writeProbeTuples(t, ctx, pool)
	longValue := writeOneLongValue(t, ctx, pool)
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
		plan, text := explainRecorded(t, ctx, pool, read.name, read.statement)
		t.Logf("%s:\n%s", read.name, text)
		if explaintest.SeqScansRelation(t, plan, "graph_tuples") != read.wantScan {
			if read.wantScan {
				t.Errorf("%s does not scan graph_tuples, which is what naming no kind is supposed to cost:\n%s", read.name, text)
			} else {
				t.Errorf("%s scans graph_tuples:\n%s", read.name, text)
			}
		}
		if read.wantScan {
			continue
		}
		scans := readIndexScans(t, plan)
		if len(scans) < read.wantIndexScans {
			t.Errorf("%s reads graph_tuples through %d index scans, want %d, one for each kind it names:\n%s",
				read.name, len(scans), read.wantIndexScans, text)
		}
		for _, scan := range scans {
			if !slices.Contains(graphTupleReadIndexes, scan.Index) {
				t.Errorf("%s reads graph_tuples through %s, which is no read index of the first migration (%v):\n%s",
					read.name, scan.Index, graphTupleReadIndexes, text)
			}
		}
		if len(scans) == 0 {
			t.Errorf("%s reads graph_tuples through no index at all:\n%s", read.name, text)
		}
		keys.RequireIndexKeyed(t, plan, explaintest.AnyIndex, read.name, explaintest.CondOptional)
		if read.wantMergedInTime && !planHolds(t, plan, "Merge Append") {
			t.Errorf("%s does not merge its kinds in time order (no Merge Append node):\n%s", read.name, text)
		}
	}
}

// storeRead is one read under test: what it is called, the statement and arguments the package
// sent for it, whether its plan may scan graph_tuples, how many index scans of graph_tuples it
// must hold and whether those are merged in time order. The answer each read gave is compared
// with the reference walk before the plan is explained, so a read that is wrong fails on its
// answer rather than on its plan.
type storeRead struct {
	name             string
	statement        recordedStatement
	wantScan         bool
	wantIndexScans   int
	wantMergedInTime bool
}

// storeReads runs each read the package states, checks its answer against the reference walk and
// returns the statement each one sent. The six reads are the five current and as-of reads of one
// node and the inbox read of the crowded node; the seventh, the inbox read naming no kind, is
// the negative control that shows the kind list is what keeps that read off a scan.
func storeReads(t *testing.T, ctx context.Context, pool *store.Pool, reference logReference) []storeRead {
	t.Helper()
	recorder := &recordedQueryer{Queryer: pool}
	reads := make([]storeRead, 0, 7)
	record := func(name string, wantScan bool, wantIndexScans int, merged bool) {
		t.Helper()
		reads = append(reads, storeRead{
			name: name, statement: recorder.last(t), wantScan: wantScan,
			wantIndexScans: wantIndexScans, wantMergedInTime: merged,
		})
	}

	out, err := CurrentOut(ctx, recorder, probeKind, probeID, nil)
	if err != nil {
		t.Fatalf("CurrentOut: %v", err)
	}
	requireSameTuples(t, "CurrentOut", out, reference.currentOut)
	record("CurrentOut", false, 1, false)

	in, err := CurrentIn(ctx, recorder, probeKind, probeID, nil)
	if err != nil {
		t.Fatalf("CurrentIn: %v", err)
	}
	requireSameTuples(t, "CurrentIn", in, reference.currentIn)
	record("CurrentIn", false, 1, false)

	value, found, err := CurrentValue(ctx, recorder, probeKind, probeID, probeAttribute)
	if err != nil {
		t.Fatalf("CurrentValue: %v", err)
	}
	if value != reference.currentValue || found != reference.currentValueFound {
		t.Errorf("CurrentValue answers %q/%v, and the walk of the whole log answers %q/%v",
			value, found, reference.currentValue, reference.currentValueFound)
	}
	record("CurrentValue", false, 1, false)

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

	inbox, err := TuplesIntoNode(ctx, recorder, crowdedKind, crowdedID, crowdedInboxKinds, inboxLimit)
	if err != nil {
		t.Fatalf("TuplesIntoNode: %v", err)
	}
	requireSameOrder(t, "TuplesIntoNode naming its kinds", inbox, reference.inboxByKind)
	record("TuplesIntoNode naming its kinds", false, len(crowdedInboxKinds), true)

	unkinded, err := TuplesIntoNode(ctx, recorder, crowdedKind, crowdedID, nil, inboxLimit)
	if err != nil {
		t.Fatalf("TuplesIntoNode naming no kind: %v", err)
	}
	requireSameOrder(t, "TuplesIntoNode naming no kind", unkinded, reference.inboxAnyKind)
	record("TuplesIntoNode naming no kind", true, 0, false)
	return reads
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

// explainRecorded explains one recorded statement, as JSON for the checks and as text for the
// report, under EXPLAIN (ANALYZE, BUFFERS) with every plan type left enabled: the plans read
// here are the ones the planner really takes at this size, not ones a pin forced on it.
func explainRecorded(
	t *testing.T, ctx context.Context, pool *store.Pool, name string, statement recordedStatement,
) (json.RawMessage, string) {
	t.Helper()
	var planJSON []byte
	if err := pool.QueryRow(ctx, "explain (analyze, buffers, format json) "+statement.sql, statement.args...).Scan(&planJSON); err != nil {
		t.Fatalf("explain %s: %v", name, err)
	}
	rows, err := pool.Query(ctx, "explain (analyze, buffers) "+statement.sql, statement.args...)
	if err != nil {
		t.Fatalf("explain %s as text: %v", name, err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("read a plan line of %s: %v", name, err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain %s as text: %v", name, err)
	}
	return explaintest.OnePlan(t, planJSON), strings.Join(lines, "\n")
}

// readIndexScans names every index scan of graph_tuples in plan, which is what a read of one
// node must reach the log through.
func readIndexScans(t *testing.T, plan json.RawMessage) []explaintest.IndexScan {
	t.Helper()
	var scans []explaintest.IndexScan
	for _, scan := range explaintest.IndexScans(t, plan) {
		if scan.Relation == "graph_tuples" {
			scans = append(scans, scan)
		}
	}
	return scans
}

// planHolds reports whether plan holds a node of the named type at any depth.
func planHolds(t *testing.T, plan json.RawMessage, nodeType string) bool {
	t.Helper()
	var node struct {
		NodeType string            `json:"Node Type"`
		Plans    []json.RawMessage `json:"Plans"`
	}
	if err := json.Unmarshal(plan, &node); err != nil {
		t.Fatalf("decode plan node: %v", err)
	}
	if node.NodeType == nodeType {
		return true
	}
	for _, child := range node.Plans {
		if planHolds(t, child, nodeType) {
			return true
		}
	}
	return false
}

// reportBytesARow prints what one tuple costs the database at this size: the row count, the
// table with its indexes and TOAST (pg_total_relation_size), and the quotient. The growth
// projection of dispatch://INFRA-1149 is read off this figure, so the test prints all three
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
	quoted := make([]string, len(kinds))
	for index, kind := range kinds {
		quoted[index] = "'" + kind + "'"
	}
	return "array[" + strings.Join(quoted, ",") + "]"
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
// sequences.
type logReference struct {
	currentOut        []int64
	currentIn         []int64
	currentValue      string
	currentValueFound bool
	asOfOut           []int64
	asOfIn            []int64
	inboxByKind       []int64
	inboxAnyKind      []int64
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
// the newest at or before a moment, the top of an inbox by time - so the walk needs no sort and
// keeps no more rows in memory than the answers hold. It binds nothing and names no node, so no
// index can serve it and nothing it answers can be short for the same reason a read's index
// could be: that is what makes it a reference.
func walkTheWholeLog(t *testing.T, ctx context.Context, pool *store.Pool) logReference {
	t.Helper()
	started := time.Now()
	asOf := probeAsOf()
	currentOut := newestOfEachGroup{by: bySeq}
	currentIn := newestOfEachGroup{by: bySeq}
	currentValue := newestOfEachGroup{by: bySeq}
	asOfOut := newestOfEachGroup{by: byTimeThenSeq}
	asOfIn := newestOfEachGroup{by: byTimeThenSeq}
	inboxByKind := topOfInbox{limit: inboxLimit}
	inboxAnyKind := topOfInbox{limit: inboxLimit}

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
		if intoCrowded && fact.toKind != KindValue {
			inboxAnyKind.offer(fact)
			if slices.Contains(crowdedInboxKinds, fact.kind) {
				inboxByKind.offer(fact)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("walk the whole log: %v", err)
	}
	if walked < seedTotal {
		t.Fatalf("the walk read %d tuples, want at least the %d the seed appended", walked, seedTotal)
	}
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
		inboxByKind:       inboxByKind.seqs(),
		inboxAnyKind:      inboxAnyKind.seqs(),
	}
	t.Logf("walked %d tuples in %s; the reference holds %d current out, %d current in, %d as-of out, %d as-of in",
		walked, time.Since(started).Round(time.Millisecond), len(reference.currentOut), len(reference.currentIn),
		len(reference.asOfOut), len(reference.asOfIn))
	for name, answer := range map[string][]int64{
		"CurrentOut": reference.currentOut, "CurrentIn": reference.currentIn,
		"AsOfOut": reference.asOfOut, "AsOfIn": reference.asOfIn,
		"the kinded inbox": reference.inboxByKind, "the unkinded inbox": reference.inboxAnyKind,
	} {
		if len(answer) == 0 {
			t.Fatalf("the reference answers nothing for %s, so comparing a read with it proves nothing", name)
		}
	}
	if !reference.currentValueFound {
		t.Fatal("the reference answers no current value for the probe, so comparing CurrentValue with it proves nothing")
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

// graphTupleReadIndexes are the five read indexes, which every plan above must reach the log
// through. They are named here as well as in requiredGraphIndexes because the plan checks ask
// which index a scan read, and the migration check asks which migration declares it.
var graphTupleReadIndexes = []string{
	"graph_tuples_current",
	"graph_tuples_values",
	"graph_tuples_inbound",
	"graph_tuples_as_of_out",
	"graph_tuples_as_of_in",
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

// requiredGraphIndexes is the whole index set of the node and tuple store: the five read indexes
// of graph_tuples, its two partial repeat keys, and the primary key and the one index of
// graph_nodes. The primary key is declared inside the create table rather than as a create
// index, so it is checked apart (requireIndexSetOfTheFirstMigration).
var requiredGraphIndexes = []requiredGraphIndex{
	{
		name: "graph_tuples_current", table: "graph_tuples",
		keys: "from_kind, from_id, kind, to_kind, to_id, seq desc", predicate: "to_kind <> 'value'",
	},
	{
		name: "graph_tuples_values", table: "graph_tuples",
		keys: "from_kind, from_id, kind, seq desc", predicate: "to_kind = 'value'",
	},
	{
		name: "graph_tuples_inbound", table: "graph_tuples",
		keys: "to_kind, to_id, kind, from_kind, from_id, seq desc", predicate: "to_kind <> 'value'",
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
		name: "graph_tuples_value_once", table: "graph_tuples", unique: true,
		keys:      "from_kind, from_id, kind, source_event, asserted",
		predicate: "to_kind = 'value' and source_event <> 0",
	},
	{
		name: "graph_nodes_supertag", table: "graph_nodes",
		keys: "supertag, kind, id", predicate: "",
	},
}

// declaredIndex is one create index a migration file holds.
type declaredIndex struct {
	migration            string
	name, table          string
	keys, predicate      string
	unique, concurrently bool
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
		body := withoutLineComments(readMigration(t, file))
		for _, index := range indexesOf(file, body) {
			declared[index.name] = append(declared[index.name], index)
		}
		if table := graphNodesCreateTable.FindStringSubmatch(body); table != nil {
			if graphNodesTable != "" {
				t.Errorf("%s creates graph_nodes, which %s already created", file, graphStoreMigration)
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

	// A value has no length bound and a btree key is bounded at about 2,704 bytes, so an index
	// keying to_id refuses the insert of a tuple whose to_id holds one. Every such index is
	// partial on to_kind, which is what keeps the values out of it.
	for _, indexes := range declared {
		for _, index := range indexes {
			if index.table != "graph_tuples" || !slices.Contains(strings.Split(index.keys, ", "), "to_id") {
				continue
			}
			if !strings.Contains(index.predicate, "to_kind") {
				t.Errorf("%s (%s) in %s keys to_id under no to_kind predicate: a tuple whose to_id holds a value would fail its insert",
					index.name, index.keys, index.migration)
			}
		}
	}
	t.Logf("the index set of %s: %d indexes read from the migration files in order, over %d files",
		graphStoreMigration, len(requiredGraphIndexes)+1, len(files))
}

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

// indexDeclaration matches one create index up to the semicolon that ends it; no index of this
// tree writes a semicolon inside its own definition.
var indexDeclaration = regexp.MustCompile(`(?is)\bcreate\s+(unique\s+)?index\s+(concurrently\s+)?(?:if\s+not\s+exists\s+)?([a-z0-9_]+)\s+on\s+([a-z0-9_]+)\s*([^;]*);`)

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
// the way requiredGraphIndexes writes them.
func indexesOf(migration, body string) []declaredIndex {
	var indexes []declaredIndex
	for _, match := range indexDeclaration.FindAllStringSubmatch(body, -1) {
		keys, predicate := splitIndexBody(match[5])
		indexes = append(indexes, declaredIndex{
			migration:    migration,
			name:         match[3],
			table:        match[4],
			keys:         keys,
			predicate:    predicate,
			unique:       strings.TrimSpace(match[1]) != "",
			concurrently: strings.TrimSpace(match[2]) != "",
		})
	}
	return indexes
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
