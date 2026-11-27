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

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/benchtime"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// TestStatementTimings measures this package's changed statements on the seeded measurement
// database (benchtime), which DISPATCH_BENCH_DATABASE_URL names. CI never sets that variable, so
// CI never runs a measurement; api/search_bench_test.go and api/related_recall_test.go are gated
// the same way.
//
// A pair of one text with itself measures no change and passes or fails by chance, so a read whose
// text is main's is no pair of main's text against the head's: refs.Edges' out reads and
// refs.BacklinkCounts carry heldByABlock in main's one form and keep main's text. The pairs below
// measure refs.BacklinkCounts against the release before this change, and under main's index of
// targets.
//
// Which protocol each measurement is under, and why:
//
//   - The two backlink counts are protocol 1 pairs. The release before this change counts over
//     the whole graph_edges view (docstest holds that read as ViewBacklinkCountSQL, with the arm
//     over a counted item's own current blocks), and that text runs on this release's schema, so
//     one database holds both sides.
//     The before side is read with ask_followers_session_id present, the index migration
//     20261101182419 adds for the followed_by arm, which only helps it, so each pair is the
//     conservative reading of the head text's own gain.
//   - Five of refs.Edges' in reads are strict protocol 1 pairs (Pair): the seed's root message
//     with the most replies, subjects.AnchoredDocument as the route reads it, the ask whose
//     current parts most links end at, the same ask as its panel reads it, and the comment whose
//     current blocks most links end at. The before side is main's statement for the same Query,
//     held in graph_test.go letter for letter as b459fa923705 wrote it and bound with the
//     arguments its comment names; the after side is the statement readEdgeRows sends at the
//     head, recorded as it sent it, and it must bind the arguments the head binds for that same
//     Query (timedEdgesRead.headArgs): the node's id, the acting credential of somebody who
//     proves no author, and the same kinds. Both texts run on this release's schema, so one
//     database holds both sides. Each is strict because the earlier builds Job that measured
//     these pairs read the head at least 8 times main's own spread below main's lowest median
//     there, the closest being the root message, 141.6 ms below 147.248 ms against a spread of
//     17.6 ms.
//   - The same document's in read as refs.ReferencedBy sends it (WholeDocuments, Kinds mentions)
//     is a bounded pair (PairWithin), built the same way. It is bounded because the head's 4.715
//     ms there was 0.482 ms below main's lowest median, 5.197 ms, while two runs of main's own
//     statement differed by 0.992 ms, so a strict pair could fail on a correct head.
//   - refs.Closure, the references read, is a strict series pair (PairSeries) over each of two
//     subjects: subjects.Issue, and the issue of the largest project with the most link rows into
//     it and its own content (linksIntoIssue), the worst case for the in statement. The before
//     side is mainClosureQuery, main's eight-hop statement, held in closure_test.go letter for
//     letter; the after side is every statement Closure sends for the subject, recorded as it
//     sent them (the out statement, the in statement and each loader that names their nodes),
//     each after the set_config statement the route's budget sends before it
//     (closureBudgetStatement). Both texts run on this release's schema, so one database holds
//     both sides. It is strict because the change makes the read faster: the earlier builds Job
//     read the whole new read, its set_config statements included, in 5.820 ms against main's
//     39.754 ms for subjects.Issue and in 9.674 ms against 40.100 ms for the issue with the most
//     links in, at --scale 1 with jit off.
//   - The out statement alone is a strict series pair against mainClosureQuery too, the
//     like-for-like comparison, since main's statement names no node; it read 1.70 ms against
//     main's 49.7 ms for subjects.Issue. The in statement alone is an AfterSeries, one side that
//     is held to no bound: it reads the links into the issue and its own content, new work that
//     no statement of main did, so it has no before side.
//   - The four loaders the change keys, loadCommentNodes, loadAskNodes, loadMessageNodes and
//     loadBlockNodes, are bounded pairs (PairWithin): main's statement, held in graph_test.go
//     letter for letter, against the head's, recorded as the loader sent it for the same ids
//     (timedLoader.statement). They are bounded because the change keys their reads, each join
//     a probe by key that stops at its first row, rather than making them faster, so a slower
//     head is a regression and a faster one is not required. The bound is main's median times
//     1.10 plus 0.505 ms (benchtime's withinBound): the absolute term is the largest spread of a
//     loader statement against itself below 25 ms that the earlier builds Job read at --scale 1
//     with jit off, the block loader at 256 ids reading 3.871 against 3.366 ms, and the tenth
//     covers the larger statements, whose spread grows with them.
//     Each loader is bound to the ids a graph read of the seed passes it (loaderList):
//     refs.Edges' in read of subjects.AnchoredDocument passes the comment, the ask and the block
//     loader theirs, 10,581, 1,145 and 22 ids at --scale 1, and its in read of the root message
//     with the most replies passes the message loader 2. Each is measured at 1 id, the first of
//     that list, as refs.Node passes one; at 256 ids, the most refs.TargetNames passes
//     (contracts.ReferenceTargetsMax), the list first and then the rows of the loader's table in
//     md5 order of their ids; and at the whole list. The comment loader is measured at the first
//     1,000 ids of its list too, where main read about 32 ms against the head's 9.4 ms, so that
//     pair with its sides swapped fails and shows the measurement tells the two apart. A longer
//     list filled from a table, which no read passes, is no pair: on the comments filled that
//     way main against itself read +14.6% in one round, beyond the bound.
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
// The closure's and the loaders' pairs run after the edges pairs and before the index pairs, so
// they measure the seed as it is, and each of their reads is recorded, its subject or its list
// logged, before the first of them is measured, so a subject or a list that fails stops the run
// before it spends its time on the others. An empty list of a loader fails the run, naming the
// loader (loaderList.pair, timedLoader.passedBy).
//
// The fixture is written after every other pair, so they measure the seed as it is, and through
// a connection of its own, since the runner's connection is for reading (benchtime.Runner.Conn);
// it is committed and analyzed for its pairs, and t.Cleanup deletes it.
func TestStatementTimings(t *testing.T) {
	// Not parallel: its assertion compares two timings. Each pair fails unless the after text's
	// median is below the before text's (benchtime.Pair, PairSeries) or within a bound of it
	// (PairWithin), and a reading taken beside another test's work is that test's reading too.
	runner := benchtime.Open(t, os.Getenv(benchtime.DatabaseURLEnv))
	subjects := runner.Subjects()

	// The issue read's count: one issue's inbound edges, the number its page carries.
	issueCount, ok := backlinkCountSQL("issue", nil, false)
	if !ok {
		t.Fatal("backlinkArms declares no arm for an issue, so the issue read's count has no statement")
	}
	issueIDs := []string{subjects.Issue}
	runner.Pair(t, "refs.BacklinkCounts over an issue",
		benchtime.Statement{SQL: docstest.ViewBacklinkCountSQL("graph_edges", "issue", false), Args: []any{"issue", issueIDs, nodeKinds}},
		benchtime.Statement{SQL: issueCount, Args: []any{issueIDs, nodeKinds, VisibilityArg(model.Actor{})}})

	// The ask page's count: every ask of one issue at once, each ask's own clarification thread
	// and its own parts excluded, which is what the page's cards show. Its subject is the issue
	// carrying the most asks, read from the database rather than taken from subjects.Issue: the
	// heavily linked issue the other readings bind is linked by children and blockers, and
	// carries no ask.
	askIDs := askPageSubject(t, runner)
	askCount, ok := backlinkCountSQL("ask", AskBacklinkExclusions, false)
	if !ok {
		t.Fatal("backlinkArms declares no arm for an ask, so the ask page's count has no statement")
	}
	runner.Pair(t, "refs.BacklinkCounts over an issue's asks",
		benchtime.Statement{SQL: docstest.ViewBacklinkCountSQL("graph_edges", "ask", true), Args: []any{"ask", askIDs, nodeKinds, AskBacklinkExclusions}},
		benchtime.Statement{SQL: askCount, Args: []any{askIDs, nodeKinds, VisibilityArg(model.Actor{}), AskBacklinkExclusions}})

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
	// The acting credential the head's reads bind: somebody who proves no author, for whom no
	// node of the seed is reserved.
	seen := VisibilityArg(model.Actor{})
	// The root message's and the anchored document's in reads as the route reads them: the edges
	// pairs measure their statements, and their loaders' statements bind the loaders' lists below.
	messageQuery := Query{Direction: "in", Kind: "message", ID: message, ExcludeKinds: part}
	documentQuery := Query{Direction: "in", Kind: "artifact", ID: document, ExcludeKinds: part}
	strict := []timedEdgesRead{
		{"refs.Edges in, the root message with the most replies",
			messageQuery,
			mainEdgesInMessage, []any{"message", message, message, part}, []any{message, message, seen, part}},
		{"refs.Edges in, the anchored document",
			documentQuery,
			mainEdgesInDocument, []any{"artifact", document, part}, []any{document, seen, part}},
		{"refs.Edges in, the ask whose current parts most links end at",
			Query{Direction: "in", Kind: "ask", ID: ask},
			mainEdgesInAsk, []any{"ask", ask, ask}, []any{ask, ask, seen}},
		{"refs.Edges in, the same ask as its panel reads it",
			Query{Direction: "in", Kind: "ask", ID: ask, ExcludeKinds: AskBacklinkExclusions},
			mainEdgesInAskPanel, []any{"ask", ask, ask, AskBacklinkExclusions}, []any{ask, ask, seen, AskBacklinkExclusions}},
		{"refs.Edges in, the comment whose current blocks most links end at",
			Query{Direction: "in", Kind: "comment", ID: comment, ExcludeKinds: part},
			mainEdgesInComment, []any{"comment", comment, comment, part}, []any{comment, comment, seen, part}},
	}
	referencedBy := timedEdgesRead{"refs.Edges in, the anchored document as refs.ReferencedBy reads it",
		Query{Direction: "in", Kind: "artifact", ID: document, Kinds: []string{"mentions"}, WholeDocuments: true},
		mainEdgesInWholeDocument, []any{"artifact", document, []string{"mentions"}}, []any{document, seen, []string{"mentions"}}}
	// Every side is recorded and checked before any is measured, so a read whose head side binds
	// other arguments than its Query's stops the run before it spends its time on the others.
	strictSides := make([][2]benchtime.Statement, len(strict))
	for index, read := range strict {
		strictSides[index][0], strictSides[index][1] = read.sides(t, runner)
	}
	referencedByBefore, referencedByAfter := referencedBy.sides(t, runner)
	for index, read := range strict {
		runner.Pair(t, read.name, strictSides[index][0], strictSides[index][1])
	}
	runner.PairWithin(t, referencedBy.name, []benchtime.Statement{referencedByBefore}, []benchtime.Statement{referencedByAfter})

	// The references read over its two subjects, and the four loaders it keys over the lists the
	// graph read passes them. Every read is recorded, its subject or its list logged, before the
	// first of them is measured.
	mostLinked := timingSubject(t, runner, "the issue of "+subjects.Project+" with the most link rows into it and its own content",
		"link rows into it and its own content", `
		select i.key, `+linksIntoIssue("i.key")+` from issues i
		where i.project_key = $1
		order by 2 desc, i.key limit 1`, subjects.Project)
	closures := []closureRead{
		readClosure(t, runner, "subjects.Issue", subjects.Issue),
		readClosure(t, runner, "the issue with the most links into it and its own content", mostLinked),
	}
	documentRead := recordGraphRead(t, runner, "the anchored document's read", documentQuery)
	messageRead := recordGraphRead(t, runner, "the root message's read", messageQuery)
	comments := commentLoader.passedBy(t, runner, documentRead)
	lists := []loaderList{
		comments,
		askLoader.passedBy(t, runner, documentRead),
		blockLoader.passedBy(t, runner, documentRead),
		messageLoader.passedBy(t, runner, messageRead),
	}
	for _, closure := range closures {
		closure.measure(t, runner)
	}
	for _, list := range lists {
		list.measure(t, runner)
	}
	// The comment loader at the first 1,000 ids of its list, or at the whole list where the read
	// passes fewer, which its name then counts.
	comments.pair(t, runner, "the first of "+comments.read, comments.ids[:min(1000, len(comments.ids))])

	// The index pairs on subjects.Issue, after the plan under each side's Setup is known to read
	// the index that Setup builds.
	requireTargetIndexRead(t, runner, mainsTargetIndex, "block_refs_to", subjects.Issue, false)
	requireTargetIndexRead(t, runner, headsTargetIndex, "block_refs_to_source", subjects.Issue, false)
	issueEdges := []any{"issue", subjects.Issue}
	runner.PairWithin(t, "refs.BacklinkCounts over an issue, under main's index of targets",
		[]benchtime.Statement{{SQL: issueCount, Args: []any{issueIDs, nodeKinds, seen}, Setup: mainsTargetIndex}},
		[]benchtime.Statement{{SQL: issueCount, Args: []any{issueIDs, nodeKinds, seen}, Setup: headsTargetIndex}})
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

// timingSubject reads one node a pair reads, with statement, which answers the node's id and the
// count of counted it was chosen by, ties broken by the id. It logs both, and fails the run when
// the count is 0 or the statement answers no row: a read of a node nothing of that kind ends at
// measures reads that answer nothing, and the seed then holds no subject of the kind the pair is
// about.
func timingSubject(t *testing.T, runner *benchtime.Runner, what, counted, statement string, args ...any) string {
	t.Helper()
	var id string
	var count int64
	err := runner.Conn().QueryRow(context.Background(), statement, args...).Scan(&id, &count)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("read %s: %v", what, err)
	}
	if count == 0 {
		t.Fatalf("%s: 0 %s, so a pair over it measures reads that answer nothing", what, counted)
	}
	t.Logf("timing subject, %s: %s, %d %s", what, id, count, counted)
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

// timedEdgesRead is one edges pair: its name, the Query the head's read is for, main's held
// statement for that Query with the arguments its comment in graph_test.go names, in order, and
// the arguments the head's statement binds for the same Query, in order: the node's id, the id
// again where the read keys the node's blocks by it, the acting credential (VisibilityArg), and
// the kinds or the exclusions. The head writes the node's kind into its text rather than binding
// it (readEdgeRows), so the two lists differ in shape and name the same node.
type timedEdgesRead struct {
	name     string
	query    Query
	main     string
	mainArgs []any
	headArgs []any
}

// sides are read's two sides: main's held statement with its arguments, and the statement
// readEdgeRows sends for read.query at the head, as somebody who proves no author, recorded as it
// sent it, text and arguments. The run fails unless the head's statement binds read.headArgs,
// since sides that read different nodes measure no change of text.
func (read timedEdgesRead) sides(t *testing.T, runner *benchtime.Runner) (benchtime.Statement, benchtime.Statement) {
	t.Helper()
	recorder := &statementRecorder{Queryer: runner.Conn()}
	if _, err := readEdgeRows(context.Background(), recorder, model.Actor{}, read.query); err != nil {
		t.Fatalf("%s: read the head's statement: %v", read.name, err)
	}
	if len(recorder.statements) != 1 {
		t.Fatalf("%s: readEdgeRows sent %d statements; the pair measures its one", read.name, len(recorder.statements))
	}
	head := recorder.statements[0]
	if !reflect.DeepEqual(head.args, read.headArgs) {
		t.Fatalf("%s: the head's statement binds %#v, and the pair names %#v for its Query", read.name, head.args, read.headArgs)
	}
	return benchtime.Statement{SQL: read.main, Args: read.mainArgs}, benchtime.Statement{SQL: head.sql, Args: head.args}
}

// linksIntoIssue is a statement's count of the link rows that end at the issue whose key issue
// names, a column or a parameter, or at a node of its own content. Its targets are the in
// statement's (linksInQuery): the issue, its documents by ref_key and their blocks, and its asks,
// comments and messages and their current blocks, each read by the issue's key. For each target
// it reads each link table the in statement reads, refs, block_refs and the items' block link
// tables, through that table's index of targets on to_kind and to_id, so every read is keyed and
// a count over each issue of a project reads no link table whole. It counts link rows, not the
// nodes the in statement answers: a node that links twice counts twice, as the in statement
// reads both.
func linksIntoIssue(issue string) string {
	targets := []string{
		`select 'issue', ` + issue,
		`select 'artifact', a.ref_key from artifacts a where a.issue_key = ` + issue,
		`select 'block', d.node_id from artifacts a join document_blocks d on d.artifact_id = a.id where a.issue_key = ` + issue,
	}
	links := []string{"refs", "block_refs"}
	for _, item := range itemKinds {
		tables, _ := item.tables()
		targets = append(targets,
			`select '`+string(item)+`', x.id::text from `+tables.items+` x where x.issue_key = `+issue,
			`select '`+item.BlockKind()+`', b.node_id from `+tables.items+` x join `+tables.blocks+` b on b.`+tables.item+` = x.id
			where x.issue_key = `+issue+` and `+itemBlockCurrent)
		links = append(links, tables.refs)
	}
	arms := make([]string, len(links))
	for index, table := range links {
		arms[index] = `select 1 from ` + table + ` r where r.to_kind = t.kind and r.to_id = t.id`
	}
	return `(select count(*)
		from (` + strings.Join(targets, `
			union all `) + `) t (kind, id)
		cross join lateral (` + strings.Join(arms, `
			union all `) + `) l)`
}

// closureBudgetStatement is the statement the route's budget sends before each statement of the
// references read (setStatementBudget, api/references.go; api imports this package, so the text
// after ClosureTag is written again here), with the whole of the read's 2 s bound. The route sets
// what is left of the bound, a value that changes from one statement to the next but not what a
// statement reads: the earlier builds Job read the whole read with these statements at -3.3% and
// +5.4% of the read without them, inside the 19.4% one side read against itself.
var closureBudgetStatement = benchtime.Statement{SQL: ClosureTag + `select set_config('statement_timeout', $1, true)`, Args: []any{"2000ms"}}

// closureRead is the references read of one issue, recorded: name, the subject as the pairs over
// it name it; issue, its key; and sent, every statement Closure sent for it as it sent them, the
// out statement and the in statement first.
type closureRead struct {
	name, issue string
	sent        []recordedStatement
}

// readClosure reads issue's references through a statementRecorder on the runner's connection,
// as the route reads them for somebody who proves no author, and logs the subject: its link rows
// in (linksIntoIssue), the links out and in the read answers, and the rows mainClosureQuery
// answers for it. It fails unless the read sent its out statement and then its in statement
// first, which the out and the in measurements read alone.
func readClosure(t *testing.T, runner *benchtime.Runner, name, issue string) closureRead {
	t.Helper()
	ctx := context.Background()
	var linksIn int64
	if err := runner.Conn().QueryRow(ctx, `select `+linksIntoIssue("$1::text"), issue).Scan(&linksIn); err != nil {
		t.Fatalf("%s: count the link rows into %s and its own content: %v", name, issue, err)
	}
	recorder := &statementRecorder{Queryer: runner.Conn()}
	references, err := Closure(ctx, recorder, model.Actor{}, issue, "")
	if err != nil {
		t.Fatalf("%s: read %s's references: %v", name, issue, err)
	}
	sent := recorder.statements
	if len(sent) < 2 || sent[0].sql != ClosureTag+linksOutQuery() || sent[1].sql != ClosureTag+linksInQuery() {
		t.Fatalf("%s: Closure sent %d statements for %s, and not its out statement and then its in statement first", name, len(sent), issue)
	}
	var mainRows, depthOne int64
	if err := runner.Conn().QueryRow(ctx,
		`select count(*), count(*) filter (where m.depth = 1) from (`+mainClosureQuery+`) m`, issue).Scan(&mainRows, &depthOne); err != nil {
		t.Fatalf("%s: count the rows mainClosureQuery answers for %s: %v", name, issue, err)
	}
	t.Logf("closure subject, %s: %s, %d link rows into it and its own content; refs.Closure sent %d statements and answered %d links out and %d linked from; mainClosureQuery answers %d rows, %d of them at depth 1",
		name, issue, linksIn, len(sent), len(references.Links), len(references.LinkedFrom), mainRows, depthOne)
	return closureRead{name: name, issue: issue, sent: sent}
}

// measure runs read's three measurements (TestStatementTimings says why each is under its
// protocol): the whole read against main's statement for the same issue, every statement Closure
// sent each after closureBudgetStatement, as the route sends them; the out statement alone
// against the same; and the in statement alone, one side with no bound.
func (read closureRead) measure(t *testing.T, runner *benchtime.Runner) {
	t.Helper()
	main := benchtime.Statement{SQL: mainClosureQuery, Args: []any{read.issue}}
	whole := make([]benchtime.Statement, 0, 2*len(read.sent))
	for _, statement := range read.sent {
		whole = append(whole, closureBudgetStatement, benchtime.Statement{SQL: statement.sql, Args: statement.args})
	}
	out, in := read.sent[0], read.sent[1]
	runner.PairSeries(t, "refs.Closure over "+read.name, main, whole)
	runner.PairSeries(t, "refs.Closure's out statement over "+read.name, main, []benchtime.Statement{{SQL: out.sql, Args: out.args}})
	runner.AfterSeries(t, "refs.Closure's in statement over "+read.name, []benchtime.Statement{{SQL: in.sql, Args: in.args}})
}

// timedGraphRead is a read of refs.Edges whose statements bind the loaders' lists: name, the list
// as the loaders' pairs name it; query, the read; and sent, every statement Edges sent for it, as
// it sent them. (visibility_census_test.go's graphRead is the census's record of a read.)
type timedGraphRead struct {
	name  string
	query Query
	sent  []recordedStatement
}

// recordGraphRead reads query's edges through a statementRecorder on the runner's connection, as
// a graph read of the seed reads them, for somebody who proves no author.
func recordGraphRead(t *testing.T, runner *benchtime.Runner, name string, query Query) timedGraphRead {
	t.Helper()
	recorder := &statementRecorder{Queryer: runner.Conn()}
	if _, err := Edges(context.Background(), recorder, model.Actor{}, query); err != nil {
		t.Fatalf("%s, refs.Edges' %s read of %s %s: %v", name, query.Direction, query.Kind, query.ID, err)
	}
	return timedGraphRead{name: name, query: query, sent: recorder.statements}
}

// timedLoader is one of the four loaders the change keys: its name; the node kind resolveNodes
// calls it for; the function; main's statement, held in graph_test.go; the table, with the
// loader's id as an expression over it, that fills the loader's list of 256 ids; and bound, the
// arguments the head's statement binds for ids, read as somebody who proves no author: the ids
// and the acting credential (VisibilityArg), the block loader's as blockNodesAddressed writes
// them.
type timedLoader struct {
	name, kind string
	load       nodeLoader
	main       string
	table, id  string
	bound      func(ids []string) []any
}

// idsAndCredential is the arguments the comment, ask and message loaders bind for ids, read as
// somebody who proves no author.
func idsAndCredential(ids []string) []any { return []any{ids, VisibilityArg(model.Actor{})} }

// blockNodesBound is the arguments loadBlockNodes binds for ids, read as somebody who proves no
// author (blockNodesAddressed).
func blockNodesBound(ids []string) []any {
	_, args := blockNodesAddressed(ids, model.Actor{})
	return args
}

var (
	commentLoader = timedLoader{"loadCommentNodes", "comment", loadCommentNodes, mainLoadCommentNodes, "comments", "id::text", idsAndCredential}
	askLoader     = timedLoader{"loadAskNodes", "ask", loadAskNodes, mainLoadAskNodes, "asks", "id::text", idsAndCredential}
	messageLoader = timedLoader{"loadMessageNodes", "message", loadMessageNodes, mainLoadMessageNodes, "messages", "id::text", idsAndCredential}
	blockLoader   = timedLoader{"loadBlockNodes", "block", loadBlockNodes, mainLoadBlockNodes, "document_blocks", "node_id", blockNodesBound}
)

// statement is the one statement loader sends for ids, read as somebody who proves no author,
// recorded as it sent it. It fails unless the loader sends one statement that binds the ids and
// the acting credential as loader.bound names them.
func (loader timedLoader) statement(t *testing.T, runner *benchtime.Runner, ids []string) recordedStatement {
	t.Helper()
	recorder := &statementRecorder{Queryer: runner.Conn()}
	if err := loader.load(context.Background(), recorder, model.Actor{}, loader.kind, false, ids, map[[2]string]resolvedNode{}); err != nil {
		t.Fatalf("%s over %s: %v", loader.name, idCount(len(ids)), err)
	}
	if len(recorder.statements) != 1 {
		t.Fatalf("%s sent %d statements; its pairs measure its one", loader.name, len(recorder.statements))
	}
	head := recorder.statements[0]
	if !reflect.DeepEqual(head.args, loader.bound(ids)) {
		t.Fatalf("%s binds %d arguments, and not the ids it was passed with the acting credential", loader.name, len(head.args))
	}
	return head
}

// loaderList is the ids a graph read passed a loader, in the order it passed them, and the read's
// name.
type loaderList struct {
	loader timedLoader
	read   string
	ids    []string
}

// passedBy is the list read passed loader: the ids loader's statement bound, found among the
// statements read sent by the text loader sends, as the one list of ids among its arguments. It
// logs how many, and fails naming the read and the loader when the read passed the loader no id,
// since the seed then holds no list a graph read passes it.
func (loader timedLoader) passedBy(t *testing.T, runner *benchtime.Runner, read timedGraphRead) loaderList {
	t.Helper()
	text := loader.statement(t, runner, nil).sql
	for _, statement := range read.sent {
		if statement.sql != text {
			continue
		}
		for _, arg := range statement.args {
			if ids, _ := arg.([]string); len(ids) > 0 {
				t.Logf("loader list, %s (refs.Edges' %s read of %s %s): %s passed %s",
					read.name, read.query.Direction, read.query.Kind, read.query.ID, loader.name, idCount(len(ids)))
				return loaderList{loader: loader, read: read.name, ids: ids}
			}
		}
	}
	t.Fatalf("%s, refs.Edges' %s read of %s %s, passed %s no id, so the loader's pairs have no list a graph read passes it",
		read.name, read.query.Direction, read.query.Kind, read.query.ID, loader.name)
	return loaderList{}
}

// measure runs list's three pairs: the loader at 1 id, the first of the list, as refs.Node passes
// one; at the most ids refs.TargetNames passes, the list first and then the rows of the loader's
// table in md5 order of their ids (filled); and at the whole list.
func (list loaderList) measure(t *testing.T, runner *benchtime.Runner) {
	t.Helper()
	list.pair(t, runner, "the first of "+list.read, list.ids[:1])
	list.pair(t, runner, list.read+" first, then "+list.loader.table+" in md5 order",
		list.filled(t, runner, contracts.ReferenceTargetsMax))
	list.pair(t, runner, list.read, list.ids)
}

// filled is the first size ids of list, and where the list holds fewer, the rows of the loader's
// table after them that the list does not hold, in md5 order of their ids, as the earlier
// measurement filled its lists. It fails when the table holds too few such rows to reach size.
func (list loaderList) filled(t *testing.T, runner *benchtime.Runner, size int) []string {
	t.Helper()
	if len(list.ids) >= size {
		return list.ids[:size]
	}
	id := "x." + list.loader.id
	rows, err := runner.Conn().Query(context.Background(),
		`select `+id+` from `+list.loader.table+` x where not (`+id+` = any($1)) order by md5(`+id+`) limit $2`,
		list.ids, size-len(list.ids))
	if err != nil {
		t.Fatalf("%s: read %s to fill %s to %s: %v", list.loader.name, list.loader.table, list.read, idCount(size), err)
	}
	fill, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("%s: read %s to fill %s to %s: %v", list.loader.name, list.loader.table, list.read, idCount(size), err)
	}
	if len(list.ids)+len(fill) < size {
		t.Fatalf("%s: %s holds %s besides the %s %s passed, too few for a list of %d",
			list.loader.name, list.loader.table, idCount(len(fill)), idCount(len(list.ids)), list.read, size)
	}
	return append(slices.Clone(list.ids), fill...)
}

// pair measures list's loader over ids as a bounded pair, main's statement against the head's
// recorded for the same ids, named by the loader, the count and what, which says which list ids
// is. An empty list fails it, naming the loader: a loader over no id reads nothing whatever its
// text, the fastest reading there is.
func (list loaderList) pair(t *testing.T, runner *benchtime.Runner, what string, ids []string) {
	t.Helper()
	name := list.loader.name + " over " + idCount(len(ids)) + ": " + what
	if len(ids) == 0 {
		t.Fatalf("%s: the list is empty, so %s's pair would measure a read of nothing", name, list.loader.name)
	}
	head := list.loader.statement(t, runner, ids)
	runner.PairWithin(t, name,
		[]benchtime.Statement{{SQL: list.loader.main, Args: []any{ids}}},
		[]benchtime.Statement{{SQL: head.sql, Args: head.args}})
}

// idCount is n ids in words, as the loaders' pairs and lines count a list.
func idCount(n int) string {
	if n == 1 {
		return "1 id"
	}
	return fmt.Sprintf("%d ids", n)
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
