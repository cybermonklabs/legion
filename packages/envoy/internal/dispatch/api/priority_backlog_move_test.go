// priority_backlog_move_test.go holds the read of each issue's move into `backlog` or `icebox`
// (backlogMoveLateral) to what it reads and to what it answers, in the two statements that splice
// it: the second fault of the priorities read's statement (priorityReadQuery) and the listing with
// `?moved=true` (listIssuesQuery with $14 true, issueMovedJoin). The lateral reads the issue's
// events newest first through the partial index events_backlog_moves
// (store/migrations/20261125131100_events_backlog_moves.up.sql) and stops at the first event that
// matches. Both statements answer the rows that they answer with that index dropped, which is the
// schema of main.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/benchtime"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// backlogMoveIndex is the partial index on events (issue_key, seq) whose predicate is the lateral's
// own conditions on the event: its type issue.updated, its changed list naming status, and its
// status backlog or icebox.
const backlogMoveIndex = "events_backlog_moves"

// backlogMoveProject is the one project of the corpus (seedBacklogMoveCorpus), and the project the
// listing names.
const backlogMoveProject = "BACK"

// The two priorities of the corpus: one stated on a root whose children stand in backlog or
// icebox, so that work advances it by inheritance, and one stated on each issue in backlog or
// icebox of its own. The second fault lists an issue that states a priority no issue above it
// carries (priorityBacklogTerms), so of the root's children in backlog or icebox it lists only the
// one that states the second priority as well; the listing reads the move of every one of them.
const (
	backlogMoveFleetPriority = "backlog-the-fleet"
	backlogMoveOwnPriority   = "backlog-on-its-own"
)

// The shape of the corpus. Each issue that was moved through backlog and icebox holds
// backlogMoveEventsFrom to backlogMoveEventsFrom+backlogMoveEventsSpan-1 events, several hundred,
// so that without the partial index the planner reads all of the events of an issue in backlog or
// icebox and sorts them rather than walk them back from the newest. Beside those issues stand
// backlogMoveBystanders issues in flight that advance no priority, each with 20 to 60 events.
const (
	backlogMoveEventsFrom     = 300
	backlogMoveEventsSpan     = 200
	backlogMoveFleetInBacklog = 8
	backlogMoveOwnInBacklog   = 8
	backlogMoveUnplacedCount  = 3
	backlogMoveBystanders     = 120
)

// backlogMoveMover is the actor of the last move of every issue of the corpus, and seededWriter,
// alice, is the actor of every other event; the two logins are theirs. A read that answered a move
// older than the last move of an issue in backlog or icebox would answer the wrong actor as well as
// the wrong time.
const (
	backlogMoveMover      = `{"kind":"user","id":"bob"}`
	backlogMoveMoverLogin = "bob"
	backlogMoveOtherLogin = "alice"
)

// backlogMoveStatuses are the statuses the moves before an issue's last move go to: work waiting
// and in flight, and backlog and icebox where the history moves through them.
var backlogMoveStatuses = []string{"triage", "todo", "in_progress", "needs_review", "backlog", "icebox"}

// backlogMove is the move into `backlog` or `icebox` that an issue's events record last: the event
// the lateral answers, by its time and the login of its actor.
type backlogMove struct {
	at    time.Time
	login string
}

// backlogMoveHistory is how the events of one issue of the corpus run.
type backlogMoveHistory struct {
	// events is how many events the issue holds.
	events int
	// backlogBefore puts four moves into and out of backlog and icebox before the last move, and
	// lets the moves at random go to them too.
	backlogBefore bool
	// recorded is whether an event records the last move, into the status the issue stands in.
	// Where it is false, the status was written with no event, as the status of an issue put in
	// backlog before Dispatch recorded its moves was.
	recorded bool
}

// write writes the events of the issue key, which stands in status, in rows of
// seededEventColumns, one minute apart from start. The first is the issue's creation, in triage.
// The last move, into status, comes 10 to 49 events before the end (fewer in a short history), so
// the issue's newest move is not its newest event. Before it, where backlogBefore holds, four moves
// go into and out of backlog and icebox (into backlog, out to todo, into icebox, out to
// in_progress), and about one event in twelve moves the issue to another status at random. Of the
// other events, about one in seven is an issue.updated that changes the title: its payload names
// the status the issue stands in, as every issue.updated payload names it, and its changed list
// does not name status. The rest are comments. Every move but the last is alice's, and the last
// is bob's.
//
// It answers the rows and the newest move into backlog or icebox that they record, which is the
// event the lateral answers, or nil where no event records one.
func (h backlogMoveHistory) write(t testing.TB, random *rand.Rand, key, status string, start time.Time) ([][]any, *backlogMove) {
	t.Helper()
	actorColumn := slices.Index(seededEventColumns, "actor")
	rows := make([][]any, 0, h.events)
	var newest *backlogMove
	current := "triage"
	add := func(seq int, kind, payload string, mover bool) {
		at := start.Add(time.Duration(seq) * time.Minute)
		row := seededEvent(key, seq, kind, payload, at)
		if mover {
			row[actorColumn] = backlogMoveMover
		}
		rows = append(rows, row)
	}
	move := func(seq int, to string, mover bool, changed ...string) {
		current = to
		add(seq, "issue.updated", seededIssuePayload(t, to, changed...), mover)
		if slices.Contains(priorityBacklogStatuses, to) {
			login := backlogMoveOtherLogin
			if mover {
				login = backlogMoveMoverLogin
			}
			newest = &backlogMove{at: start.Add(time.Duration(seq) * time.Minute), login: login}
		}
	}

	last := h.events - min(10+random.IntN(40), h.events/3)
	forced := map[int]string{}
	if h.backlogBefore {
		for index, to := range []string{"backlog", "todo", "icebox", "in_progress"} {
			forced[2+(index+1)*(last-2)/5] = to
		}
	}
	pool := backlogMoveStatuses[:4]
	if h.backlogBefore {
		pool = backlogMoveStatuses
	}
	add(1, "issue.created", seededIssuePayload(t, current), false)
	for seq := 2; seq <= h.events; seq++ {
		forcedTo, isForced := forced[seq]
		switch {
		case seq == last && h.recorded:
			changed := []string{"status"}
			if random.IntN(2) == 0 {
				changed = append(changed, "assignee")
			}
			move(seq, status, true, changed...)
		case seq == last:
			// The status is written with no event, and this event is a comment.
			current = status
			add(seq, "comment.created", `{}`, false)
		case seq == last-1 && current == status:
			// The last move changes the status, so the one before it leaves the status the
			// issue will be moved back into.
			away := "triage"
			if status == away {
				away = "todo"
			}
			move(seq, away, false, "status")
		case seq < last && (isForced || random.IntN(12) == 0):
			to := forcedTo
			if !isForced {
				to = pool[random.IntN(len(pool))]
			}
			// A write that leaves the status as it stands moves nothing, and this event is a
			// comment.
			if to == current {
				add(seq, "comment.created", `{}`, false)
				continue
			}
			move(seq, to, false, "status")
		case random.IntN(7) == 0:
			add(seq, "issue.updated", seededIssuePayload(t, current, "title"), false)
		default:
			add(seq, "comment.created", `{}`, false)
		}
	}
	return rows, newest
}

// backlogMoveCorpus is what seedBacklogMoveCorpus wrote: its store, the live priorities it holds,
// the status each issue stands in, the newest move into backlog or icebox each issue's events
// record (no entry for an issue whose events record none), the rows of the second fault, and the
// issue among them whose events record no move.
type backlogMoveCorpus struct {
	database   *store.Store
	priorities []string
	statuses   map[string]string
	moves      map[string]backlogMove
	backlog    []string
	unrecorded string
}

// seedBacklogMoveCorpus writes the corpus the backlog move is planned and compared over, in bulk
// (priority_fixture_test.go), in one project:
//
//   - a root in todo that states backlogMoveFleetPriority, with backlogMoveFleetInBacklog children
//     in icebox and backlog by turns, the first of which also states backlogMoveOwnPriority, and
//     two children moved through backlog and icebox that stand in todo and in_progress now;
//   - backlogMoveOwnInBacklog issues in icebox and backlog by turns, each stating
//     backlogMoveOwnPriority, and one more stating it that was moved through backlog and icebox
//     and stands in needs_review now;
//   - one issue that states backlogMoveOwnPriority and stands in backlog, whose status was written
//     with no event, so no event of it records a move into backlog or icebox, although its title
//     edits after that name backlog as its status;
//   - backlogMoveUnplacedCount issues in icebox and backlog by turns that advance no priority,
//     which the listing reads the move of and the second fault does not list;
//   - backlogMoveBystanders issues in triage, todo, in_progress and needs_review by turns that
//     advance no priority and were never in backlog or icebox.
//
// Each issue's events start a day after the previous issue's, so no two moves have the same time
// and the second fault's order, newest move first, is one order. The random source has a fixed
// seed, so every run writes the same rows. The second fault lists 10 rows: the root's first child
// in backlog or icebox, which states backlogMoveOwnPriority, which the root does not carry, the
// backlogMoveOwnInBacklog issues and the issue whose status was written with no event. The root's
// other children in backlog or icebox only inherit backlogMoveFleetPriority, so the fault leaves
// them out, and the listing reads their moves. That is fewer than the 50 the fault cuts its list
// at, so it lists every row it counts.
func seedBacklogMoveCorpus(t *testing.T) backlogMoveCorpus {
	t.Helper()
	_, database, _ := newTestServer(t, testServerOptions{})
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	random := rand.New(rand.NewPCG(5, 6))
	corpus := backlogMoveCorpus{
		database:   database,
		priorities: []string{backlogMoveFleetPriority, backlogMoveOwnPriority},
		statuses:   map[string]string{}, moves: map[string]backlogMove{},
	}
	var issues, eventRows, nodes, tuples [][]any
	backlogEvents := func() backlogMoveHistory {
		return backlogMoveHistory{events: backlogMoveEventsFrom + random.IntN(backlogMoveEventsSpan), backlogBefore: true, recorded: true}
	}
	addIssue := func(status string, parent *string, history backlogMoveHistory, priorities ...string) string {
		number := len(issues) + 1
		key := seededIssueKey(backlogMoveProject, number)
		rows, move := history.write(t, random, key, status, base.Add(time.Duration(number)*24*time.Hour))
		issues = append(issues, seededIssueRow{
			key: key, project: backlogMoveProject, number: number, title: "Backlog move work " + key,
			status: status, parent: parent, rank: seededRank(number), at: base, lastSeq: len(rows),
		}.values())
		eventRows = append(eventRows, rows...)
		corpus.statuses[key] = status
		if move != nil {
			corpus.moves[key] = *move
		}
		for _, priority := range priorities {
			tuples = append(tuples, seededAdvancesTuple(key, priority, base))
		}
		return key
	}
	backlogStatus := func(index int) string { return priorityBacklogStatuses[index%len(priorityBacklogStatuses)] }

	root := addIssue("todo", nil, backlogMoveHistory{events: 60, recorded: true}, backlogMoveFleetPriority)
	for index := range backlogMoveFleetInBacklog {
		if index == 0 {
			corpus.backlog = append(corpus.backlog, addIssue(backlogStatus(index), &root, backlogEvents(), backlogMoveOwnPriority))
			continue
		}
		addIssue(backlogStatus(index), &root, backlogEvents())
	}
	for _, status := range []string{"todo", "in_progress"} {
		addIssue(status, &root, backlogEvents())
	}
	for index := range backlogMoveOwnInBacklog {
		corpus.backlog = append(corpus.backlog, addIssue(backlogStatus(index), nil, backlogEvents(), backlogMoveOwnPriority))
	}
	addIssue("needs_review", nil, backlogEvents(), backlogMoveOwnPriority)
	corpus.unrecorded = addIssue("backlog", nil,
		backlogMoveHistory{events: backlogMoveEventsFrom + random.IntN(backlogMoveEventsSpan)}, backlogMoveOwnPriority)
	corpus.backlog = append(corpus.backlog, corpus.unrecorded)
	for index := range backlogMoveUnplacedCount {
		addIssue(backlogStatus(index), nil, backlogEvents())
	}
	for index := range backlogMoveBystanders {
		addIssue(backlogMoveStatuses[index%4], nil, backlogMoveHistory{events: 20 + random.IntN(41), recorded: true})
	}

	for index, id := range corpus.priorities {
		nodes = append(nodes, seededPriorityNode(t, id, "Priority "+id, "Work in backlog under it.",
			"alice", fmt.Sprintf("p%02d", index), base))
	}
	copySeededRows(t, database, "projects", seededProjectColumns, [][]any{seededProject(backlogMoveProject, len(issues), base)})
	copySeededRows(t, database, "issues", seededIssueColumns, issues)
	copySeededRows(t, database, "events", seededEventColumns, eventRows)
	copySeededRows(t, database, "priority_nodes", seededNodeColumns, nodes)
	copySeededRows(t, database, "priority_tuples", seededTupleColumns, tuples)
	recorded := 0
	for _, key := range corpus.backlog {
		if _, moved := corpus.moves[key]; moved {
			recorded++
		}
	}
	if _, moved := corpus.moves[corpus.unrecorded]; moved || recorded != len(corpus.backlog)-1 {
		t.Fatalf("the corpus records a move of %d of the second fault's %d rows, want every one but %s", recorded, len(corpus.backlog), corpus.unrecorded)
	}
	t.Logf("the corpus holds %d issues and %d events; the second fault lists %d of the issues, %d of them with a recorded move",
		len(issues), len(eventRows), len(corpus.backlog), recorded)
	return corpus
}

// standingInBacklog is how many issues of the corpus the listing reads the move of: every issue
// that stands in backlog or icebox, whatever it advances.
func (c backlogMoveCorpus) standingInBacklog() int {
	listed := 0
	for _, status := range c.statuses {
		if slices.Contains(priorityBacklogStatuses, status) {
			listed++
		}
	}
	return listed
}

// projectListingArguments are the arguments the listing route binds for
// `?project=<project>&moved=<moved>` (listIssues): no filter but the project, the gate of the
// move read ($14) moved, and `own` ($15) false. The three times it names none of, updated_since
// and the closed window's two ends, are typed nils: pgx sends one as null, as it sends an untyped
// nil, and benchtime.PreparedArguments writes one as a timestamptz null.
func projectListingArguments(project string, moved bool) []any {
	return []any{project, []string{}, "", (*time.Time)(nil), []string{}, false, []int16{}, false,
		(*time.Time)(nil), (*time.Time)(nil), []string{}, false, false, moved, false}
}

// backlogMoveSchemas are the index under the name events_backlog_moves that each reading runs
// with: the migration's own, and the three a control runs with. Each is made by statements that
// the reading's transaction runs before its EXPLAIN, and the transaction is rolled back after it.
var backlogMoveSchemas = []struct {
	name    string
	setup   []string
	control bool
}{
	{"with events_backlog_moves", nil, false},
	{"with events_backlog_moves dropped, as on main", []string{"drop index " + backlogMoveIndex}, true},
	{"with an index of every event under the name events_backlog_moves", []string{
		"drop index " + backlogMoveIndex,
		"create index " + backlogMoveIndex + " on events (issue_key, seq)",
	}, true},
	{"with a partial index of every issue.updated under the name events_backlog_moves", []string{
		"drop index " + backlogMoveIndex,
		"create index " + backlogMoveIndex + " on events (issue_key, seq) where type = 'issue.updated'",
	}, true},
}

// TestTheBacklogMoveReadsTheNewestMoveThroughItsIndex holds the read of each issue's move into
// backlog or icebox (backlogMoveLateral) to one walk back through events_backlog_moves for each
// row it reads the move of, which stops at the first event it reads
// (requireBacklogMoveThroughItsIndex), in the list of the second fault of the priorities read's
// statement (priorityReadQuery), the plan of its CTE priorityBacklogListCTE, and in the listing
// with `?moved=true`. The statement is read with the arguments readPriorities binds: the live
// priorities, and no project. Its newest event reads events too, which is no read of a move, so
// the check takes the list's own plan. The listing is read with the arguments the route binds for
// `?project=BACK&moved=true`, and the check takes its whole plan. Each is read under four plans,
// each run and measured: the generic plan that a prepared statement
// settles on (explaintest.AnalyzedGenericPlan) and a plan made for the values, both with
// sequential scans off and no other setting, as the package's plan tests read them
// (explaintest.GenericPlan); a plan made for the values with the planner's default settings; and
// a plan made for the values with bitmap scans off as well.
//
// The controls are three other indexes under the same name (backlogMoveSchemas): none, which is
// the schema of main; an index of every event on (issue_key, seq); and a partial index whose
// predicate is the type alone. The check run on each must fail under each plan, and the test fails
// when it passes, since the check would then be one that cannot tell the read through the partial
// index from a read of the issue's other events. Under the first three plans the planner reads
// every event of the issue that the index of each control holds through a bitmap and sorts them
// (PostgreSQL 18.7, 2026-11-24); the fourth plan is there so that the planner walks the index of
// a control backwards instead, which only the check's Filter and its rows removed tell from the
// read through the partial index.
func TestTheBacklogMoveReadsTheNewestMoveThroughItsIndex(t *testing.T) {
	t.Parallel()
	corpus := seedBacklogMoveCorpus(t)
	settleSpeedCorpus(t, corpus.database)
	checkedPlan := func(t testing.TB, plan json.RawMessage, part, subject string) json.RawMessage {
		if part == "" {
			return plan
		}
		return planSubplan(t, plan, part, subject)
	}
	ctx := context.Background()
	keys := explaintest.NewIndexKeys(corpus.database.Pool)
	connection, err := corpus.database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the generic plans: %v", err)
	}
	defer connection.Release()

	statements := []struct {
		name, sql, part string
		arguments       []any
		reads           int
	}{
		{"the second fault", priorityReadQuery, priorityBacklogListCTE, []any{corpus.priorities, []string{}}, len(corpus.backlog)},
		{"the listing with ?moved=true", listIssuesQuery, "", projectListingArguments(backlogMoveProject, true), corpus.standingInBacklog()},
	}
	for statementNumber, statement := range statements {
		parameters, literals := benchtime.PreparedArguments(t, statement.name, statement.arguments...)
		for schemaNumber, schema := range backlogMoveSchemas {
			withSeqScansOff := append([]string{"set local enable_seqscan = off"}, schema.setup...)
			withBitmapScansOff := append([]string{"set local enable_seqscan = off", "set local enable_bitmapscan = off"}, schema.setup...)
			plans := []struct {
				name string
				plan json.RawMessage
			}{
				{"the generic plan", explaintest.AnalyzedGenericPlan(t, ctx, connection,
					fmt.Sprintf("backlog_move_%d_%d", statementNumber, schemaNumber), statement.sql,
					parameters, literals, withSeqScansOff...)},
				{"the plan for its values", explaintest.AnalyzedPlan(t, ctx, corpus.database.Pool, statement.sql,
					withSeqScansOff, statement.arguments...)},
				{"the plan for its values with the default settings", explaintest.AnalyzedPlan(t, ctx, corpus.database.Pool,
					statement.sql, schema.setup, statement.arguments...)},
				{"the plan for its values with bitmap scans off", explaintest.AnalyzedPlan(t, ctx, corpus.database.Pool,
					statement.sql, withBitmapScansOff, statement.arguments...)},
			}
			for _, plan := range plans {
				subject := fmt.Sprintf("%s under %s %s", statement.name, plan.name, schema.name)
				read := checkedPlan(t, plan.plan, statement.part, subject)
				if !schema.control {
					requireBacklogMoveThroughItsIndex(t, keys, read, subject, statement.reads)
					continue
				}
				explaintest.RequireControlFails(t, subject, "a control",
					"a read through the partial index from a read of the issue's other events", read,
					func(t testing.TB) { requireBacklogMoveThroughItsIndex(t, keys, read, subject, statement.reads) })
			}
		}
	}
}

// describeBacklogMoveRead names node, a read of events, as EXPLAIN's text names it: its type, its
// direction where it reads an index backwards (an Index Scan reads an issue's entries of the index
// newest first only where it is Backward), its index and its relation. A Bitmap Heap Scan names no
// index, so the indexes of the bitmap it reads its rows through follow it.
func describeBacklogMoveRead(node analyzedNode) string {
	described := node.NodeType
	if node.ScanDirection == "Backward" {
		described += " Backward"
	}
	if node.IndexName != "" {
		described += " using " + node.IndexName
	}
	if node.RelationName != "" {
		described += " on " + node.RelationName
	}
	var bitmap []string
	for _, child := range node.Plans {
		for _, under := range child.everyNode() {
			if under.NodeType == "Bitmap Index Scan" {
				bitmap = append(bitmap, under.IndexName)
			}
		}
	}
	if len(bitmap) > 0 {
		described += " through a bitmap of " + strings.Join(bitmap, " and ")
	}
	return described
}

// backlogMoveRead is one read of events in a plan, with the type of the node it runs under: the
// first node above it that is not a Result. The listing's gate on the read is a one-time filter,
// which a Result between the lateral's Limit and its scan tests.
type backlogMoveRead struct {
	node  analyzedNode
	under string
}

// backlogMoveReads is every node of plan that reads events, each with the node it runs under.
func backlogMoveReads(t testing.TB, plan json.RawMessage) []backlogMoveRead {
	t.Helper()
	var root analyzedNode
	if err := json.Unmarshal(plan, &root); err != nil {
		t.Fatalf("decode the plan: %v\n%s", err, plan)
	}
	var reads []backlogMoveRead
	var visit func(node analyzedNode, under string)
	visit = func(node analyzedNode, under string) {
		if node.RelationName == "events" {
			reads = append(reads, backlogMoveRead{node: node, under: under})
		}
		if node.NodeType != "Result" {
			under = node.NodeType
		}
		for _, child := range node.Plans {
			visit(child, under)
		}
	}
	visit(root, "")
	return reads
}

// requireBacklogMoveThroughItsIndex holds plan, an analyzed plan of a statement that splices
// backlogMoveLateral, to reading, for each row in backlog or icebox, one walk back through
// backlogMoveIndex that reads the issue's newest move and no other event. Every read of events is
// an Index Scan Backward of backlogMoveIndex whose Index Cond names issue_key, the index's first
// key column (IndexKeys.RequireIndexKeyed), and that runs right under the lateral's Limit, so it
// stops at the first entry it reads. It carries no Filter: the index's predicate is the lateral's
// every condition on the event, so every entry it reads is a move the lateral wants. Each time it
// runs it answers at most one row, and neither its Filter nor a recheck removes any row: it reads
// one event for an issue whose events record a move, and none for one whose events record none.
// The reads of events run reads times in all, once for each row in backlog or icebox the statement
// reads the move of, and at least once, so a plan that read no event shows nothing. Each failure
// names subject and prints the plan through t.Errorf, which is what a control reads
// (explaintest.RequireControlFails).
func requireBacklogMoveThroughItsIndex(t testing.TB, keys *explaintest.IndexKeys, plan json.RawMessage, subject string, reads int) {
	t.Helper()
	keys.RequireIndexKeyed(t, plan, backlogMoveIndex, subject, explaintest.CondRequired)
	var loops float64
	for _, read := range backlogMoveReads(t, plan) {
		node, described := read.node, describeBacklogMoveRead(read.node)
		loops += node.Loops
		t.Logf("%s reads events by %s under a %s, on `%s`, with the Filter `%s`: %v times, %v rows each time, %v rows removed by its Filter and %v by a recheck each time",
			subject, described, read.under, node.IndexCond, node.Filter, node.Loops, node.Rows, node.RemovedByFilter, node.RemovedByRecheck)
		if node.NodeType != "Index Scan" || node.ScanDirection != "Backward" || node.IndexName != backlogMoveIndex {
			t.Errorf("%s reads events by %s, want only an Index Scan Backward using %s:\n%s", subject, described, backlogMoveIndex, plan)
			continue
		}
		if read.under != "Limit" {
			t.Errorf("%s reads %s under a %s, want it right under the lateral's Limit, so it stops at the first entry it reads:\n%s",
				subject, described, read.under, plan)
		}
		if node.Filter != "" {
			t.Errorf("%s reads %s with the Filter `%s`, so the index holds events the lateral does not want:\n%s",
				subject, described, node.Filter, plan)
		}
		if node.Rows > 1 || node.RemovedByFilter > 0 || node.RemovedByRecheck > 0 {
			t.Errorf("%s reads %s for %v rows each time it runs, and removes %v by its Filter and %v by a recheck each time, want at most one row and none removed:\n%s",
				subject, described, node.Rows, node.RemovedByFilter, node.RemovedByRecheck, plan)
		}
	}
	if loops == 0 {
		t.Errorf("%s ran no read of events, so its plan shows nothing of how it reads them:\n%s", subject, plan)
	} else if loops != float64(reads) {
		t.Errorf("%s reads events %v times in all, want once for each of the %d rows in backlog or icebox it reads the move of:\n%s",
			subject, loops, reads, plan)
	}
}

// backlogMoveAnswer is what the two statements answer over the corpus: the second fault's count,
// its counts by priority and its rows, as readPriorities resolves them (priorityFault.resolve),
// and the listing's rows as the route resolves them (issueSummaryRow), each with its two cells of
// the move.
type backlogMoveAnswer struct {
	Count      int                   `json:"count"`
	ByPriority map[string]int        `json:"by_priority"`
	Backlog    []model.PriorityIssue `json:"backlog"`
	Listing    []model.IssueSummary  `json:"listing"`
}

// backlogMoveAnswers runs the priorities read's statement (priorityReadQuery), with the arguments
// readPriorities binds for priorities and no project, for its second fault, and the listing with
// `?project=BACK&moved=true`, in one transaction after setup, and rolls the transaction back.
func backlogMoveAnswers(t *testing.T, database *store.Store, priorities, setup []string) backlogMoveAnswer {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the read's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, statement := range setup {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	var answer backlogMoveAnswer
	var read priorityReadAnswer
	if err := tx.QueryRow(ctx, priorityReadQuery, priorities, []string{}).Scan(read.targets()...); err != nil {
		t.Fatalf("run the priorities read's statement: %v", err)
	}
	answer.Count, answer.ByPriority, answer.Backlog = read.parked.resolve(true)
	listed, err := tx.Query(ctx, listIssuesQuery, projectListingArguments(backlogMoveProject, true)...)
	if err != nil {
		t.Fatalf("run the listing: %v", err)
	}
	defer listed.Close()
	for listed.Next() {
		var row issueSummaryRow
		if err := listed.Scan(row.targets()...); err != nil {
			t.Fatalf("read a row of the listing: %v", err)
		}
		issue, err := row.resolve()
		if err != nil {
			t.Fatalf("resolve a row of the listing: %v", err)
		}
		answer.Listing = append(answer.Listing, issue)
	}
	if err := listed.Err(); err != nil {
		t.Fatalf("read the listing: %v", err)
	}
	return answer
}

// requireBacklogMoveCells holds the two cells of the move that read answers for key to the move
// the corpus wrote, want: its time and its actor, or neither where want is nil. It answers whether
// the cells hold a move.
func requireBacklogMoveCells(t *testing.T, read, key string, want *backlogMove, at *time.Time, by *model.Actor) bool {
	t.Helper()
	if want == nil {
		if at != nil || by != nil {
			t.Errorf("%s says %s was moved at %v by %v, want neither: no event of it records a move into backlog or icebox", read, key, at, by)
		}
		return false
	}
	if at == nil || !at.Equal(want.at) || by == nil || by.Kind != "user" || by.ID != want.login {
		t.Errorf("%s says %s was moved at %v by %v, want its newest move into backlog or icebox, at %v by %s",
			read, key, at, by, want.at, want.login)
	}
	return true
}

// TestTheBacklogMoveAnswersTheRowsItAnswersWithoutItsIndex holds the two statements that splice
// backlogMoveLateral to the rows they answer on the schema of main, with events_backlog_moves
// dropped in a transaction that is rolled back. Over the corpus of seedBacklogMoveCorpus, the
// second fault of the priorities read's statement answers the same count, the same counts by
// priority and the same rows, each with the time and the actor of its move and its `advances`
// entries, and the listing with `?moved=true` answers the same rows with the same two cells of
// the move.
//
// Before the comparison, the answer read through the index is held to the corpus, so the
// comparison is of answers that hold the moves: each row of the second fault carries the newest
// move into backlog or icebox that the corpus wrote of it, the issue whose events record no such
// move carries neither cell, and in the listing a row that stands in neither status carries
// neither cell, although most of those were moved through backlog and icebox once.
func TestTheBacklogMoveAnswersTheRowsItAnswersWithoutItsIndex(t *testing.T) {
	t.Parallel()
	corpus := seedBacklogMoveCorpus(t)
	settleSpeedCorpus(t, corpus.database)
	withIndex := backlogMoveAnswers(t, corpus.database, corpus.priorities, nil)
	withoutIndex := backlogMoveAnswers(t, corpus.database, corpus.priorities, backlogMoveSchemas[1].setup)

	if withIndex.Count != len(corpus.backlog) || len(withIndex.Backlog) != len(corpus.backlog) {
		t.Fatalf("the second fault counts %d rows and lists %d, want all %d of its rows of the corpus listed",
			withIndex.Count, len(withIndex.Backlog), len(corpus.backlog))
	}
	movedIntoBacklog := 0
	for _, issue := range withIndex.Backlog {
		var want *backlogMove
		if move, moved := corpus.moves[issue.Key]; moved {
			want = &move
		}
		if requireBacklogMoveCells(t, "the second fault", issue.Key, want, issue.MovedAt, issue.MovedBy) {
			movedIntoBacklog++
		}
	}
	if len(withIndex.Listing) != len(corpus.statuses) {
		t.Fatalf("the listing answers %d rows, want the %d issues of the project", len(withIndex.Listing), len(corpus.statuses))
	}
	movedListed := 0
	for _, issue := range withIndex.Listing {
		var want *backlogMove
		if move, moved := corpus.moves[issue.Key]; moved && slices.Contains(priorityBacklogStatuses, issue.Status) {
			want = &move
		}
		if requireBacklogMoveCells(t, "the listing with ?moved=true", issue.Key, want, issue.MovedAt, issue.MovedBy) {
			movedListed++
		}
	}
	if movedIntoBacklog != len(corpus.backlog)-1 || movedListed != corpus.standingInBacklog()-1 {
		t.Fatalf("the second fault answers a move for %d rows and the listing for %d, want %d and %d: every row but %s",
			movedIntoBacklog, movedListed, len(corpus.backlog)-1, corpus.standingInBacklog()-1, corpus.unrecorded)
	}

	requireSameBacklogMoveAnswer(t, "the second fault",
		backlogMoveAnswer{Count: withIndex.Count, ByPriority: withIndex.ByPriority, Backlog: withIndex.Backlog},
		backlogMoveAnswer{Count: withoutIndex.Count, ByPriority: withoutIndex.ByPriority, Backlog: withoutIndex.Backlog})
	requireSameBacklogMoveAnswer(t, "the listing with ?moved=true",
		backlogMoveAnswer{Listing: withIndex.Listing}, backlogMoveAnswer{Listing: withoutIndex.Listing})
}

// requireSameBacklogMoveAnswer fails t when read answers through the index, got, other than it
// answers with the index dropped, want. The two are compared as the JSON the routes write them
// in, so every field of every row is compared.
func requireSameBacklogMoveAnswer(t *testing.T, read string, got, want backlogMoveAnswer) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("encode what %s answers through %s: %v", read, backlogMoveIndex, err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("encode what %s answers without %s: %v", read, backlogMoveIndex, err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("%s answers through %s\n%s\nwant what it answers with the index dropped\n%s", read, backlogMoveIndex, gotJSON, wantJSON)
	}
}
