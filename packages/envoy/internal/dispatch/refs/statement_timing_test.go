package refs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/benchtime"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// TestStatementTimings measures this package's changed statements on the seeded measurement
// database (benchtime), which DISPATCH_BENCH_DATABASE_URL names. CI never sets that variable, so
// CI never runs a measurement; api/search_bench_test.go and api/related_recall_test.go are gated
// the same way.
//
// The test holds no closure pair: closureQuery's text is main's, and a pair of one text with
// itself measures no change and passes or fails by chance.
//
// Which protocol each measurement is under, and why:
//
//   - The two backlink counts are protocol 1 pairs. The release before this change counts over
//     the whole graph_edges view (this package holds that read as backlinkCountOracle, with the
//     arm over a counted item's own current blocks), and that text runs on this release's schema,
//     so one database holds both sides.
//     The before side is read with ask_followers_session_id present, the index migration
//     20261101182419 adds for the followed_by arm, which only helps it, so each pair is the
//     conservative reading of the head text's own gain.
//   - Five of refs.Edges' in reads are strict protocol 1 pairs (Pair): the seed's root message
//     with the most replies, subjects.AnchoredDocument as the route reads it, the ask whose
//     current parts most links end at, the same ask as its panel reads it, and the comment whose
//     current blocks most links end at. The before side is main's statement for the same Query,
//     held in graph_test.go letter for letter as b459fa923705 wrote it and bound with the
//     arguments its comment names; the after side is the statement readEdgeRows sends at the
//     head, recorded as it sent it, and the two must bind the same arguments. Both texts run on
//     this release's schema, so one database holds both sides. Each is strict because the earlier
//     builds Job that measured these pairs read the head at least 8 times main's own spread below
//     main's lowest median there, the closest being the root message, 141.6 ms below 147.248 ms
//     against a spread of 17.6 ms.
//   - The same document's in read as refs.ReferencedBy sends it (WholeDocuments, Kinds mentions)
//     is a bounded pair (PairWithin), built the same way. It is bounded because the head's 4.715
//     ms there was 0.482 ms below main's lowest median, 5.197 ms, while two runs of main's own
//     statement differed by 0.992 ms, so a strict pair could fail on a correct head.
//   - The two index pairs on subjects.Issue, refs.BacklinkCounts over it and refs.Edges' in read
//     of it, are bounded pairs whose text is the same on both sides and whose sides differ by
//     their index of block_refs' targets alone. Each reads block_refs by to_kind and to_id, which
//     block_refs_to keyed whole and the wider block_refs_to_source keys by its first two columns,
//     so neither has a gain to show and each is held to no loss. subjects.Issue is linked by
//     children and blockers rather than built so that documents mention it in their text and in
//     their blocks, so the probe's gain does not hide that read's cost there, as it does on the
//     probe fixture's issue.
//     Each side's Setup builds its index for the one read, in the transaction benchtime's explain
//     opens for the run and rolls back, so one database holds both sides: the before side's puts
//     main's index back (mainsTargetIndex: block_refs_to_source dropped and block_refs_to built),
//     and the after side's builds the head's again (headsTargetIndex: block_refs_to_source
//     dropped and built again). Both sides change the index, each in its own rolled-back
//     transaction, because a side with no Setup runs just after the other side's rollback and
//     reads slower whatever index it reads, so a pair with a Setup on one side alone measures the
//     Setup, not the index: under such pairs refs.DocumentIsCited's after median reached 94% to
//     98% of its bound in several rounds at --scale 1, and with a Setup on each side it was 0.095
//     ms above the before median, inside main's own spread.
//   - heldByABlock's probe of block_refs is a strict pair on the probe fixture (writeProbeFixture):
//     the in read of an issue that 1,000 documents mention, each in its own row and in two of its
//     blocks, the text the same on both sides, the before side under main's index and the after
//     side under the head's, each built by its side's Setup as the index pairs build them.
//     Through block_refs_to the probe read every block link into the issue and kept its
//     document's by a filter; round 12 of the earlier measurement read 105.034 ms against 10.661
//     ms.
//   - refs.DocumentIsCited of the first of the fixture's documents, as api.artifactIsUntouched
//     reads it, is a bounded pair on the same fixture, its text the same on both sides, the
//     before side under main's index and the after side under the head's, each built by its
//     side's Setup as the index pairs build them: its reads of block_refs use to_kind and to_id
//     alone, and the new index is wider, so it is held to no loss rather than to a gain.
//
// Before the index pairs, and again once the fixture is written, the test reads the plan under
// each of the two Setups and stops unless it reads the index that Setup builds
// (requireTargetIndexRead), so the after side is held to the same rule as the before side.
//
// The fixture is written after every other pair, so they measure the seed as it is, and through
// a connection of its own, since the runner's connection is for reading (benchtime.Runner.Conn);
// it is committed and analyzed for its pairs, and t.Cleanup deletes it.
func TestStatementTimings(t *testing.T) {
	runner := benchtime.Open(t, os.Getenv(benchtime.DatabaseURLEnv))
	subjects := runner.Subjects()

	// The issue read's count: one issue's inbound edges, the number its page carries.
	issueCount, ok := backlinkCountSQL("issue", nil)
	if !ok {
		t.Fatal("backlinkArms declares no arm for an issue, so the issue read's count has no statement")
	}
	issueIDs := []string{subjects.Issue}
	runner.Pair(t, "refs.BacklinkCounts over an issue",
		benchtime.Statement{SQL: oracleBacklinkCountSQL("issue", false), Args: []any{"issue", issueIDs, nodeKinds}},
		benchtime.Statement{SQL: issueCount, Args: []any{issueIDs, nodeKinds}})

	// The ask page's count: every ask of one issue at once, each ask's own clarification thread
	// and its own parts excluded, which is what the page's cards show. Its subject is the issue
	// carrying the most asks, read from the database rather than taken from subjects.Issue: the
	// heavily linked issue the other readings bind is linked by children and blockers, and
	// carries no ask.
	askIDs := askPageSubject(t, runner)
	askCount, ok := backlinkCountSQL("ask", AskBacklinkExclusions)
	if !ok {
		t.Fatal("backlinkArms declares no arm for an ask, so the ask page's count has no statement")
	}
	runner.Pair(t, "refs.BacklinkCounts over an issue's asks",
		benchtime.Statement{SQL: oracleBacklinkCountSQL("ask", true), Args: []any{"ask", askIDs, nodeKinds, AskBacklinkExclusions}},
		benchtime.Statement{SQL: askCount, Args: []any{askIDs, nodeKinds, AskBacklinkExclusions}})

	// The edges pairs' subjects, each with the count it was chosen by.
	message := timingSubject(t, runner, "the root message with the most replies", "replies", `
		select m.id::text, count(*) from messages m join messages r on r.in_reply_to = m.id
		where m.in_reply_to is null
		group by m.id order by count(*) desc, m.id limit 1`)
	ask := itemBlocksSubject(t, runner, ItemAsk)
	comment := itemBlocksSubject(t, runner, ItemComment)
	document := timingSubject(t, runner, "subjects.AnchoredDocument, by its ref_key", "links that end at its current blocks", `
		select a.ref_key, (select count(*) from document_blocks d
		                   join graph_edges g on g.to_kind = 'block' and g.to_id = d.node_id
		                   where d.artifact_id = a.id)
		from artifacts a where a.id = $1::uuid`, subjects.AnchoredDocument)

	part := []string{"part_of"}
	strict := []timedEdgesRead{
		{"refs.Edges in, the root message with the most replies",
			Query{Direction: "in", Kind: "message", ID: message, ExcludeKinds: part},
			mainEdgesInMessage, []any{"message", message, message, part}},
		{"refs.Edges in, the anchored document",
			Query{Direction: "in", Kind: "artifact", ID: document, ExcludeKinds: part},
			mainEdgesInDocument, []any{"artifact", document, part}},
		{"refs.Edges in, the ask whose current parts most links end at",
			Query{Direction: "in", Kind: "ask", ID: ask},
			mainEdgesInAsk, []any{"ask", ask, ask}},
		{"refs.Edges in, the same ask as its panel reads it",
			Query{Direction: "in", Kind: "ask", ID: ask, ExcludeKinds: AskBacklinkExclusions},
			mainEdgesInAskPanel, []any{"ask", ask, ask, AskBacklinkExclusions}},
		{"refs.Edges in, the comment whose current blocks most links end at",
			Query{Direction: "in", Kind: "comment", ID: comment, ExcludeKinds: part},
			mainEdgesInComment, []any{"comment", comment, comment, part}},
	}
	referencedBy := timedEdgesRead{"refs.Edges in, the anchored document as refs.ReferencedBy reads it",
		Query{Direction: "in", Kind: "artifact", ID: document, Kinds: []string{"mentions"}, WholeDocuments: true},
		mainEdgesInWholeDocument, []any{"artifact", document, []string{"mentions"}}}
	// Every side is recorded and checked before any is measured, so a read whose two sides bind
	// different arguments stops the run before it spends its time on the others.
	strictSides := make([][2]benchtime.Statement, len(strict))
	for index, read := range strict {
		strictSides[index][0], strictSides[index][1] = read.sides(t, runner)
	}
	referencedByBefore, referencedByAfter := referencedBy.sides(t, runner)
	for index, read := range strict {
		runner.Pair(t, read.name, strictSides[index][0], strictSides[index][1])
	}
	runner.PairWithin(t, referencedBy.name, []benchtime.Statement{referencedByBefore}, []benchtime.Statement{referencedByAfter})

	// The index pairs on subjects.Issue, after the plan under each side's Setup is known to read
	// the index that Setup builds.
	requireTargetIndexRead(t, runner, mainsTargetIndex, "block_refs_to", subjects.Issue, false)
	requireTargetIndexRead(t, runner, headsTargetIndex, "block_refs_to_source", subjects.Issue, false)
	issueEdges := []any{"issue", subjects.Issue}
	runner.PairWithin(t, "refs.BacklinkCounts over an issue, under main's index of targets",
		[]benchtime.Statement{{SQL: issueCount, Args: []any{issueIDs, nodeKinds}, Setup: mainsTargetIndex}},
		[]benchtime.Statement{{SQL: issueCount, Args: []any{issueIDs, nodeKinds}, Setup: headsTargetIndex}})
	runner.PairWithin(t, "refs.Edges in, the issue, under main's index of targets",
		[]benchtime.Statement{{SQL: mainEdgesInIssue, Args: issueEdges, Setup: mainsTargetIndex}},
		[]benchtime.Statement{{SQL: mainEdgesInIssue, Args: issueEdges, Setup: headsTargetIndex}})

	// The probe fixture's pairs, after every pair that measures the seed as it is.
	fixture := writeProbeFixture(t)
	requireTargetIndexRead(t, runner, mainsTargetIndex, "block_refs_to", fixture.issue, true)
	requireTargetIndexRead(t, runner, headsTargetIndex, "block_refs_to_source", fixture.issue, true)
	probeEdges := []any{"issue", fixture.issue}
	runner.Pair(t, "refs.Edges in, an issue 1,000 documents mention, under main's index of targets",
		benchtime.Statement{SQL: mainEdgesInIssue, Args: probeEdges, Setup: mainsTargetIndex},
		benchtime.Statement{SQL: mainEdgesInIssue, Args: probeEdges, Setup: headsTargetIndex})
	recorder := &statementRecorder{Queryer: runner.Conn()}
	if _, err := DocumentIsCited(context.Background(), recorder, fixture.documentID, fixture.documentRefKey); err != nil {
		t.Fatalf("read whether %s is cited: %v", fixture.documentRefKey, err)
	}
	if len(recorder.statements) != 1 {
		t.Fatalf("DocumentIsCited sent %d statements; the pair measures its one", len(recorder.statements))
	}
	cited := recorder.statements[0]
	runner.PairWithin(t, "refs.DocumentIsCited, a document that mentions that issue, under main's index of targets",
		[]benchtime.Statement{{SQL: cited.sql, Args: cited.args, Setup: mainsTargetIndex}},
		[]benchtime.Statement{{SQL: cited.sql, Args: cited.args, Setup: headsTargetIndex}})
}

// askPageSubject is every ask of the issue that carries the most, the set the ask page counts
// backlinks for in one call. The issue is read from the database so the measurement follows a
// re-seed, and a database with no ask at all fails the run rather than measuring a count over an
// empty array, which answers no row whatever the statement reads.
func askPageSubject(t *testing.T, runner *benchtime.Runner) []string {
	t.Helper()
	var issueKey string
	if err := runner.Conn().QueryRow(context.Background(), `
		select issue_key from asks where issue_key is not null
		group by issue_key order by count(*) desc, issue_key limit 1
	`).Scan(&issueKey); err != nil {
		t.Fatalf("read the issue carrying the most asks: %v", err)
	}
	rows, err := runner.Conn().Query(context.Background(),
		`select a.id::text from asks a where a.issue_key = $1 order by a.created_at, a.id`, issueKey)
	if err != nil {
		t.Fatalf("read %s's asks: %v", issueKey, err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read %s's asks: %v", issueKey, err)
	}
	if len(ids) == 0 {
		t.Fatalf("%s holds no ask, so the ask page's count has no subject", issueKey)
	}
	return ids
}

// timingSubject reads one node an edges pair reads, with statement, which answers the node's id
// and the count of counted it was chosen by, ties broken by the id. It logs both, and fails the
// run when the count is 0 or the statement answers no row: a read of a node nothing of that
// kind ends at measures arms that answer nothing, and the seed then holds no subject of the
// kind the pair is about.
func timingSubject(t *testing.T, runner *benchtime.Runner, what, counted, statement string, args ...any) string {
	t.Helper()
	var id string
	var count int64
	err := runner.Conn().QueryRow(context.Background(), statement, args...).Scan(&id, &count)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("read %s: %v", what, err)
	}
	if count == 0 {
		t.Fatalf("%s: 0 %s, so the edges pair over it measures arms that answer nothing", what, counted)
	}
	t.Logf("edges subject, %s: %s, %d %s", what, id, count, counted)
	return id
}

// itemBlocksSubject is the item of kind whose current blocks most links end at. What it counts
// is every graph_edges row whose to_id is one of the item's current blocks (position not null)
// under the kind's block kind: the view's eight arms that can end at a block, each under its own
// rule for a current block, which are refs, block_refs while block_ref_listed lists the
// pointer, comment_block_refs, message_block_refs and ask_block_refs while the source block is
// current, and the replies whose replies_to_block names the block (comments for a comment's
// block or an ask's part, told apart by the prefix). Those are the rows the keyed arm reads, so
// the subject is the item that arm reads most of.
func itemBlocksSubject(t *testing.T, runner *benchtime.Runner, kind ItemKind) string {
	t.Helper()
	tables, err := kind.tables()
	if err != nil {
		t.Fatal(err)
	}
	blocks := "current blocks"
	if kind == ItemAsk {
		blocks = "current parts"
	}
	return timingSubject(t, runner, "the "+string(kind)+" whose "+blocks+" most links end at", "links", `
		select b.`+tables.item+`::text, count(*) from `+tables.blocks+` b
		join graph_edges g on g.to_kind = '`+kind.BlockKind()+`' and g.to_id = b.node_id
		where b.position is not null
		group by b.`+tables.item+` order by count(*) desc, b.`+tables.item+` limit 1`)
}

// timedEdgesRead is one edges pair: its name, the Query the head's read is for, and main's held
// statement for that Query with the arguments its comment in graph_test.go names, in order.
type timedEdgesRead struct {
	name     string
	query    Query
	main     string
	mainArgs []any
}

// sides are read's two sides: main's held statement with its arguments, and the statement
// readEdgeRows sends for read.query at the head, recorded as it sent it, text and arguments. The
// run fails unless the two bind the same arguments, since sides that read different nodes
// measure no change of text.
func (read timedEdgesRead) sides(t *testing.T, runner *benchtime.Runner) (benchtime.Statement, benchtime.Statement) {
	t.Helper()
	recorder := &statementRecorder{Queryer: runner.Conn()}
	if _, err := readEdgeRows(context.Background(), recorder, read.query); err != nil {
		t.Fatalf("%s: read the head's statement: %v", read.name, err)
	}
	if len(recorder.statements) != 1 {
		t.Fatalf("%s: readEdgeRows sent %d statements; the pair measures its one", read.name, len(recorder.statements))
	}
	head := recorder.statements[0]
	if !reflect.DeepEqual(head.args, read.mainArgs) {
		t.Fatalf("%s: the head's statement binds %#v, and main's held statement is bound with %#v", read.name, head.args, read.mainArgs)
	}
	return benchtime.Statement{SQL: read.main, Args: read.mainArgs}, benchtime.Statement{SQL: head.sql, Args: head.args}
}

// mainsTargetIndex is the Setup that puts main's index of block_refs' targets back for one read:
// block_refs_to on (to_kind, to_id), which this release's block_refs_to_source replaced. It runs
// in the transaction benchtime's explain opens for the run and rolls back, so the change never
// outlasts the read, and at a head without block_refs_to_source its drop errors.
var mainsTargetIndex = []string{
	`drop index block_refs_to_source`,
	`create index block_refs_to on block_refs (to_kind, to_id)`,
}

// headsTargetIndex is the Setup that builds the head's index of block_refs' targets again for one
// read: block_refs_to_source dropped and created again, by the text of migration 20261120222732's
// statement. It is the after side's Setup of every pair whose before side's is mainsTargetIndex,
// so each side changes the index in its own rolled-back transaction and the two differ by that
// index alone (TestStatementTimings says why). It runs and rolls back as mainsTargetIndex does,
// and at a head without block_refs_to_source its drop errors.
var headsTargetIndex = []string{
	`drop index block_refs_to_source`,
	`create index block_refs_to_source on block_refs (to_kind, to_id, ((artifact_id)::text))`,
}

// requireTargetIndexRead reads the analyzed plan of mainEdgesInIssue for the issue under setup,
// `explain (analyze, format json)` on the runner's connection in a transaction of its own that it
// rolls back, and stops the test unless the scans of block_refs that ran read targetIndex, the
// index of block_refs' targets setup builds: block_refs_to under mainsTargetIndex, the before
// side's Setup of the pairs under main's index of targets, and block_refs_to_source under
// headsTargetIndex, their after side's. With probe false, at least one scan of block_refs that
// ran reads targetIndex and none that ran reads block_refs sequentially; with probe true, at
// least one of heldByABlock's probes, the scans of block_refs whose conditions name artifact_id,
// ran, and each probe that ran reads targetIndex.
//
// Both sides of those pairs change the index, each in its own rolled-back transaction, because a
// side with no Setup runs just after the other side's rollback and reads slower whatever index it
// reads, so a pair with a Setup on one side alone measures the Setup, not the index. The test
// checks the plan under each of the two Setups, so the after side is held to the same rule as the
// before side.
//
// A scan ran when its Actual Loops is above 0, a Bitmap Heap Scan by its own loops. It judges
// only the scans that ran, as the Verification document judges every plan: a scan that the
// executor never ran reads no row and is not judged. A plain plan holds such a scan: PostgreSQL
// 18 attaches a hashed subplan of block_refs, a Seq Scan, to an arm of graph_edges whose
// from_kind is a constant other than 'artifact', where the OR short-circuits it, so a plan
// explained without ANALYZE names a sequential read of block_refs that the executor never runs.
// The failure and the log list every scan of block_refs with its loops, and mark each scan that
// never ran as not judged.
//
// An index built in the same transaction over a broken HOT chain carries indcheckxmin, and the
// planner may then not use it in that transaction: a side under that Setup would time a
// sequential read, or another index, in its place, and read as targetIndex when it is not. The
// failure names targetIndex's indcheckxmin and its value, which says whether that is why.
func requireTargetIndexRead(t *testing.T, runner *benchtime.Runner, setup []string, targetIndex, issue string, probe bool) {
	t.Helper()
	ctx := context.Background()
	under := "the Setup `" + strings.Join(setup, "; ") + "`"
	tx, err := runner.Conn().Begin(ctx)
	if err != nil {
		t.Fatalf("begin the plan's transaction under %s: %v", under, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, statement := range setup {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "explain (analyze, format json) "+mainEdgesInIssue, "issue", issue).Scan(&raw); err != nil {
		t.Fatalf("explain the in read of %s under %s: %v", issue, under, err)
	}
	var checkXmin bool
	if err := tx.QueryRow(ctx, `select indcheckxmin from pg_index where indexrelid = $1::text::regclass`, targetIndex).Scan(&checkXmin); err != nil {
		t.Fatalf("read %s's indcheckxmin under %s: %v", targetIndex, under, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll back %s: %v", under, err)
	}
	scans := blockRefsScans(t, explaintest.OnePlan(t, raw))
	ran := func(scan blockRefsScan) bool { return scan.node.Loops > 0 }
	readsIt := func(scan blockRefsScan) bool { return slices.Contains(scan.indexes, targetIndex) }
	var failure, judged string
	if probe {
		judged = "heldByABlock's probe of block_refs ran, and each probe that ran reads " + targetIndex
		probes := 0
		for _, scan := range scans {
			if !ran(scan) || !slices.ContainsFunc(scan.conds, namesSourceDocument.MatchString) {
				continue
			}
			probes++
			if !readsIt(scan) {
				failure = "heldByABlock's probe, " + scan.String() + ", ran and does not read " + targetIndex
			}
		}
		if probes == 0 {
			failure = "no scan of block_refs that names artifact_id ran, so the plan ran no probe of heldByABlock"
		}
	} else {
		judged = "a scan of block_refs that ran reads " + targetIndex + " and none that ran reads block_refs sequentially"
		for _, scan := range scans {
			if ran(scan) && scan.node.Type == "Seq Scan" {
				failure = scan.String() + " ran and reads block_refs sequentially"
			}
		}
		if failure == "" && !slices.ContainsFunc(scans, func(scan blockRefsScan) bool { return ran(scan) && readsIt(scan) }) {
			failure = "no scan of block_refs that ran reads " + targetIndex
		}
	}
	described := make([]string, len(scans))
	for index, scan := range scans {
		loops := fmt.Sprintf("loops %g", scan.node.Loops)
		if !ran(scan) {
			loops += ", never ran, not judged"
		}
		described[index] = "  " + loops + ": " + scan.String()
	}
	if failure != "" {
		t.Fatalf("under %s, the in read of %s: %s; %s's indcheckxmin is %t, and a pair under that Setup would time another read in its place. The analyzed plan's scans of block_refs:\n%s",
			under, issue, failure, targetIndex, checkXmin, strings.Join(described, "\n"))
	}
	t.Logf("under %s, the in read of %s: %s (%s's indcheckxmin is %t). The analyzed plan's scans of block_refs:\n%s",
		under, issue, judged, targetIndex, checkXmin, strings.Join(described, "\n"))
}

// blockRefsScan is one scan of block_refs in a plan: its node, the indexes it reads through, and
// every condition it reads them or tests a row by. A Bitmap Heap Scan names no index of its own,
// so its indexes are those of the Bitmap Index Scans its bitmap is built from, and their Index
// Conds are among its conditions with its own Recheck Cond and Filter.
type blockRefsScan struct {
	node    explaintest.Node
	indexes []string
	conds   []string
}

// String names the scan with its indexes and its conditions, as a failure cites it.
func (scan blockRefsScan) String() string {
	description := scan.node.String()
	if scan.node.Type == "Bitmap Heap Scan" {
		description += " through " + strings.Join(scan.indexes, ", ")
	}
	var conds []string
	for _, cond := range scan.conds {
		if cond != "" {
			conds = append(conds, cond)
		}
	}
	if len(conds) > 0 {
		description += ", on " + strings.Join(conds, " and ")
	}
	return description
}

// blockRefsScans is every scan of block_refs in plan: each sequential, index and index-only scan
// of it with its Index Cond and Filter (explaintest.Nodes, since an IndexScan carries no Filter,
// and block_refs_to's probe names artifact_id in its Filter alone), and each Bitmap Heap Scan of
// it with its bitmap (explaintest.BitmapHeapScans).
func blockRefsScans(t *testing.T, plan json.RawMessage) []blockRefsScan {
	t.Helper()
	var scans []blockRefsScan
	for _, node := range explaintest.Nodes(t, plan) {
		if node.Relation != "block_refs" || node.Type == "Bitmap Heap Scan" {
			continue
		}
		scan := blockRefsScan{node: node, conds: []string{node.IndexCond, node.Filter}}
		if node.Index != "" {
			scan.indexes = []string{node.Index}
		}
		scans = append(scans, scan)
	}
	for _, heap := range explaintest.BitmapHeapScans(t, plan, "block_refs") {
		scan := blockRefsScan{node: heap.Scan, conds: []string{heap.Scan.RecheckCond, heap.Scan.Filter}}
		for _, index := range heap.BitmapIndexScans {
			scan.indexes = append(scan.indexes, index.Index)
			scan.conds = append(scan.conds, index.IndexCond)
		}
		scans = append(scans, scan)
	}
	return scans
}

// probeFixture is what writeProbeFixture wrote that its pairs read: the key of the issue X that
// its documents mention, and the id and ref_key of the first of them.
type probeFixture struct {
	issue          string
	documentID     string
	documentRefKey string
}

// probeFixtureDocuments is how many documents mention the fixture's issue.
const probeFixtureDocuments = 1000

// probeFixtureWrites are round 12's statements (TestR22HeldFixture of the earlier measurement) for
// its one N of 1,000, under a project of the test's own, E4855: an issue X, E4855-1000, and 1,000
// documents that each mention it in their own row (refs) and in two of their blocks (block_refs),
// so heldByABlock hides each document's own mention behind its blocks and probes block_refs once
// for each of them.
var probeFixtureWrites = []string{
	`insert into projects (key, name, default_assignee) values ('E4855', 'E4855 project', 'alice')`,
	`insert into issues (key, project_key, number, title, created_by, rank)
	 values ('E4855-1000', 'E4855', 1000, 'held issue', '{"kind":"user","id":"alice"}', 'U1000')`,
	`insert into artifacts (project_key, slug, name, kind, created_by)
	 select 'E4855', 'x1000-doc-' || j, 'x1000-doc-' || j, 'doc', '{"kind":"user","id":"alice"}'
	 from generate_series(1, 1000) j`,
	`insert into refs (from_kind, from_id, to_kind, to_id)
	 select 'artifact', a.id::text, 'issue', 'E4855-1000'
	 from artifacts a where a.project_key = 'E4855'`,
	`insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version)
	 select a.id, 'b' || g, a.ref_key || '#b' || g, 'issue', 'E4855-1000', 'mentions', 1
	 from artifacts a, generate_series(1, 2) g where a.project_key = 'E4855'`,
}

// probeFixtureRemovals delete everything probeFixtureWrites wrote, children before what they
// name. The issue's insert also queued its embedding (issues_embeddings_enqueue) and its words
// (issues_search_term_queue), and its delete queues its words again, so those rows go too, after
// the issue's: no pass runs against the measurement database, so the two queue rows have moved no
// count.
var probeFixtureRemovals = []string{
	`delete from block_refs where artifact_id in (select id from artifacts where project_key = 'E4855')`,
	`delete from refs where to_kind = 'issue' and to_id like 'E4855-%'`,
	`delete from artifacts where project_key = 'E4855'`,
	`delete from issues where project_key = 'E4855'`,
	`delete from search_term_queue where kind = 'issue' and issue_key like 'E4855-%'`,
	`delete from embeddings where kind = 'issue' and id like 'E4855-%'`,
	`delete from projects where key = 'E4855'`,
}

// probeFixtureAnalyze refreshes the statistics of the tables the fixture writes, so the planner
// plans its pairs, and every read after its removal, from the rows the tables then hold.
const probeFixtureAnalyze = `analyze projects, issues, artifacts, refs, block_refs`

// writeProbeFixture writes the probe fixture (probeFixtureWrites) through a connection of its own
// to the measurement database, never the runner's, which is for reading (benchtime.Runner.Conn):
// it first removes what an interrupted run left, commits the fixture in one transaction, analyzes
// the tables it wrote, and holds them to the rows round 12 wrote. t.Cleanup removes it
// (removeProbeFixture) and then analyzes the tables again (probeFixtureAnalyze).
func writeProbeFixture(t *testing.T) probeFixture {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv(benchtime.DatabaseURLEnv))
	if err != nil {
		t.Fatalf("connect to write the probe fixture: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	removeProbeFixture(t, conn)
	t.Cleanup(func() {
		removeProbeFixture(t, conn)
		if _, err := conn.Exec(context.Background(), probeFixtureAnalyze); err != nil {
			t.Errorf("remove the probe fixture: %s: %v", probeFixtureAnalyze, err)
		}
	})
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the probe fixture: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, statement := range probeFixtureWrites {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatalf("write the probe fixture: %s: %v", statement, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the probe fixture: %v", err)
	}
	if _, err := conn.Exec(ctx, probeFixtureAnalyze); err != nil {
		t.Fatalf("%s: %v", probeFixtureAnalyze, err)
	}
	fixture := probeFixture{issue: "E4855-1000"}
	var mentions, blockLinks int
	if err := conn.QueryRow(ctx, `
		select (select count(*) from refs where to_kind = 'issue' and to_id = $1)::int,
		       (select count(*) from block_refs where to_kind = 'issue' and to_id = $1)::int,
		       (select id::text from artifacts where project_key = 'E4855' and slug = 'x1000-doc-1'),
		       (select ref_key from artifacts where project_key = 'E4855' and slug = 'x1000-doc-1')`,
		fixture.issue).Scan(&mentions, &blockLinks, &fixture.documentID, &fixture.documentRefKey); err != nil {
		t.Fatalf("read the probe fixture back: %v", err)
	}
	if mentions != probeFixtureDocuments || blockLinks != 2*probeFixtureDocuments {
		t.Fatalf("the probe fixture holds %d mentions of %s in refs and %d in block_refs; want %d and %d",
			mentions, fixture.issue, blockLinks, probeFixtureDocuments, 2*probeFixtureDocuments)
	}
	t.Logf("probe fixture: %s, which %d documents mention in refs and %d of their blocks in block_refs; the first of them is %s (%s)",
		fixture.issue, mentions, blockLinks, fixture.documentRefKey, fixture.documentID)
	return fixture
}

// removeProbeFixture deletes the probe fixture through conn (probeFixtureRemovals) and analyzes
// nothing: before the fixture is written its own analyze follows the writes, and the cleanup
// analyzes after the removal. It reports each statement that fails and goes on with the rest,
// since it also runs as a cleanup, where a removal that stopped at its first error would leave
// the others behind.
func removeProbeFixture(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	for _, statement := range probeFixtureRemovals {
		if _, err := conn.Exec(context.Background(), statement); err != nil {
			t.Errorf("remove the probe fixture: %s: %v", statement, err)
		}
	}
}
