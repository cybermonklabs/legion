package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// What checks k2 of dispatch://INFRA-2915 and k23 of dispatch://INFRA-2916 hold the board's read
// path to, measured here: the facts are read as rows, each plan is an index read of issue_facts,
// no plan reads the event log or legion_observations, nothing on the request path computes a fact
// or holds a cached population, and the one computation it makes is the close of each open part
// of a row at the read's own clock (legionfacts.Row.At), the one place a length of time meets a
// stored value; two reads 30 s apart with the consumer stopped answer each open issue's work time
// grown by the clock time between them and each closed issue's unchanged, and a fresh process's
// first read is as fast as a warm one to the same order.
//
// Each of the four tests below is one of those readings. They share the rig
// legion_board_payload_test.go builds (boardAtInfraSize): one project of INFRA's size, the facts
// consumer run over it once and then stopped.

// ranStatement is one statement a traced pool ran, with the arguments it was given, which is
// what lets a plan be taken of the statement the request itself made rather than of a copy of it.
type ranStatement struct {
	sql  string
	args []any
}

// statementLog records every statement run on the pool it traces (storetest.Traced).
type statementLog struct {
	mutex sync.Mutex
	ran   []ranStatement
}

func (l *statementLog) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	l.ran = append(l.ran, ranStatement{sql: data.SQL, args: slices.Clone(data.Args)})
	return ctx
}

func (l *statementLog) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// reset drops what the log holds, so the statements after it are one request's own.
func (l *statementLog) reset() {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	l.ran = nil
}

// statements is every statement the log has recorded, in the order they started.
func (l *statementLog) statements() []ranStatement {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	return slices.Clone(l.ran)
}

// factsTableRead matches a statement that reads the issue_facts table itself, and never the
// cursor table beside it: `issue_facts` followed by anything but another name character.
var factsTableRead = regexp.MustCompile(`\bissue_facts\b`)

// otherFactsProjects and otherFactsRows are the rows of other projects this test puts in
// issue_facts before it takes the second plan. A deployment holds many projects' rows in the one
// table, and which plan Postgres takes for `where project_key = $1` depends on how much of the
// table one project is. The first plan is taken over the one project's rows alone, the second
// over ten projects' rows, and both are recorded.
const (
	otherFactsProjects = 9
	otherFactsRows     = 210
)

// explainedBoardRead is one plan of one statement: the statement, the plan as JSON, and the plan
// as text, which the record prints.
type explainedBoardRead struct {
	sql  string
	plan json.RawMessage
	text string
}

// explainBoardRead runs one statement under `explain (analyze, buffers)` twice, once as JSON for
// the checks and once as text for the record, in a transaction it rolls back.
func explainBoardRead(t *testing.T, database *store.Store, ran ranStatement) explainedBoardRead {
	t.Helper()
	ctx := t.Context()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the explain: %v", err)
	}
	defer tx.Rollback(ctx)
	var planJSON []byte
	if err := tx.QueryRow(ctx, "explain (analyze, buffers, format json) "+ran.sql, ran.args...).Scan(&planJSON); err != nil {
		t.Fatalf("explain %s: %v", ran.sql, err)
	}
	rows, err := tx.Query(ctx, "explain (analyze, buffers) "+ran.sql, ran.args...)
	if err != nil {
		t.Fatalf("explain %s as text: %v", ran.sql, err)
	}
	defer rows.Close()
	lines := []string{}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan a plan line: %v", err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain %s as text: %v", ran.sql, err)
	}
	return explainedBoardRead{sql: ran.sql, plan: explaintest.OnePlan(t, planJSON), text: strings.Join(lines, "\n")}
}

// boardFactsReads is every statement of the recorded request that reads the issue_facts table.
func boardFactsReads(ran []ranStatement) []ranStatement {
	reads := []ranStatement{}
	for _, statement := range ran {
		if factsTableRead.MatchString(statement.sql) {
			reads = append(reads, statement)
		}
	}
	return reads
}

// TestTheBoardsFactsReadIsAnIndexReadOfIssueFacts: the board's reads of the stored facts read
// issue_facts through index ranges of the one project they name, in three arms that share no row
// and are keyed on closed_at (R22), the arm of the open rows a read of its own that tests no list
// of cards or roots on them; they scan the table nowhere and read the project's range under no
// condition nowhere; and they read neither the event log nor the daemon's observations.
//
// Which index serves each arm is the planner's at the size it is given, and this rig is the facts'
// own (210 rows of the board's project, 42 of them open, beside nine other projects' 210): every
// index of issue_facts leads with project_key, so at this size the planner may answer an arm
// through another of them, and it still reads one project's rows through an index. The arms are
// held to their own indexes — issue_facts_closed for the open rows, issue_facts_root for the
// trees and the primary key for the cards — where the question is answered, at a size like
// INFRA's 13,000 rows (legionfacts' TestTheBoardsReadReadsItsRowsByKeyAndByRootAlone, over
// 14,000 rows).
//
// The statement explained is the statement the request itself ran: the server reads through a
// traced pool, the request's statements are recorded with their arguments, and every one of them
// that names issue_facts is explained with those arguments. So no copy of the read can drift from
// the read.
//
// Two plans are taken. The first is over the one project's rows alone, which is what a deployment
// of one project holds; the second is over ten projects' rows, which is what Dispatch holds: which
// plan Postgres takes depends on how much of the table the one project is, and reading a whole
// table every row of which is wanted is the right plan for that table, so the second is the one
// held to reading by index.
func TestTheBoardsFactsReadIsAnIndexReadOfIssueFacts(t *testing.T) {
	log := &statementLog{}
	handler, database, fixture := boardAtInfraSize(t, testServerOptions{store: storetest.TracedStore(t, storetest.Open(t), log)})
	target := "/api/v1/projects/" + fixture.Project + "/legion/board"
	analyzeFacts := func() {
		if _, err := database.Pool.Exec(t.Context(), `analyze issue_facts`); err != nil {
			t.Fatalf("analyze issue_facts: %v", err)
		}
	}

	// The plans of the request's own reads, at each of the two populations.
	examined := 0
	for _, population := range []string{"one project's rows", "ten projects' rows"} {
		analyzeFacts()
		log.reset()
		if response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice"); response.Code != http.StatusOK {
			t.Fatalf("GET the board: status=%d body=%s", response.Code, response.Body.String())
		}
		reads := boardFactsReads(log.statements())
		if len(reads) == 0 {
			t.Fatalf("the board read of %s ran no statement naming issue_facts, so there is no plan to judge", population)
		}
		var rows int
		if err := database.Pool.QueryRow(t.Context(), `select count(*) from issue_facts`).Scan(&rows); err != nil {
			t.Fatalf("count the stored rows: %v", err)
		}
		openReads := 0
		for _, read := range reads {
			explained := explainBoardRead(t, database, read)
			examined++
			relations := planRelations(explained.plan)
			t.Logf("over %s (%d rows in issue_facts), the board's read\n%s\nplans as:\n%s",
				population, rows, strings.TrimSpace(read.sql), explained.text)
			for _, forbidden := range []string{"events", "legion_observations"} {
				if slices.Contains(relations, forbidden) {
					t.Errorf("the board's read of the stored facts reads %s:\n%s", forbidden, explained.text)
				}
			}
			scans := explaintest.IndexScans(t, explained.plan)
			if population != "ten projects' rows" {
				continue
			}
			if explaintest.SeqScansRelation(t, explained.plan, "issue_facts") {
				t.Errorf("over ten projects' rows the board's read of the stored facts scans issue_facts:\n%s", explained.text)
			}
			if unkeyed := explaintest.ScansWithoutIndexCond(t, explained.plan); len(unkeyed) > 0 {
				t.Errorf("over ten projects' rows the board's read of the stored facts reads %v under no index condition:\n%s",
					unkeyed, explained.text)
			}
			for _, scan := range scans {
				if !strings.Contains(scan.Cond, "project_key") {
					t.Errorf("over ten projects' rows the board's read takes %s keyed by %q, which names no project: it reads more than the board's project:\n%s",
						scan, scan.Cond, explained.text)
				}
				t.Logf("the read takes %s, keyed by %s", scan, scan.Cond)
			}
			openReads += openRowsTestNoList(t, explained.plan, explained.text)
		}
		if population == "ten projects' rows" && openReads == 0 {
			t.Errorf("over ten projects' rows no read of the board's request reads the open rows under a condition naming closed_at IS NULL")
		}
		if population == "one project's rows" {
			seedOtherProjectsFacts(t, database, fixture.Project)
		}
	}
	if examined == 0 {
		t.Fatal("no plan was examined, so this measurement counts nothing")
	}
	t.Logf("%d plans examined, each of a statement the board's own request ran", examined)
}

// listTest matches a condition that tests a value against a list, `= ANY (…)` or `<> ALL (…)`.
var listTest = regexp.MustCompile(`\b(ANY|ALL) \(`)

// openRowsTestNoList holds a plan of one of the board's reads of the stored facts to answering the
// open rows with no list tested on them, and answers how many of its nodes read them: an open row
// is answered whatever cards and trees the board shows, so its read names neither list, and a
// filter that did would test each of the project's open rows against every card or root, which
// under a generic plan scans the list for each row rather than hashing it.
//
// The open arm is the node that names closed_at IS NULL, in its index condition where the index
// answers it and in its filter where the node tests it (`closed_at IS NOT NULL`, the other arms',
// holds no such text). Which index serves it is the planner's, so the node is found by that
// condition and not by an index name.
func openRowsTestNoList(t *testing.T, plan json.RawMessage, text string) int {
	t.Helper()
	open := 0
	for _, node := range explaintest.Nodes(t, plan) {
		conditions := node.IndexCond + " " + node.RecheckCond + " " + node.Filter
		if !strings.Contains(conditions, "closed_at IS NULL") {
			continue
		}
		open++
		if listTest.MatchString(conditions) {
			t.Errorf("the board's read of the open rows tests a list on each of them (%s under %q, filter %q):\n%s",
				node, node.IndexCond, node.Filter, text)
		}
	}
	return open
}

// boardReadGenericBudget is the most the board's reads of the stored facts may take together at
// INFRA's size under a generic plan, in milliseconds: the three parts keyed on closed_at test no
// list on the open rows, so a statement pgx caches stays inside it whichever plan Postgres settles
// on (dispatch://INFRA-2916, round 15's cost design, 1b).
const boardReadGenericBudget = 20.0

// boardReadRuns is how many times TestTheBoardsFactsReadUnderAGenericPlanStaysWithinItsBudget
// runs the read under each plan mode, of which it takes the median.
const boardReadRuns = 11

// planMilliseconds is how long an analyzed plan's root node ran, in milliseconds: the time the
// statement's execution took, less the planning.
func planMilliseconds(t *testing.T, plan json.RawMessage) float64 {
	t.Helper()
	var node struct {
		Total float64 `json:"Actual Total Time"`
	}
	if err := json.Unmarshal(plan, &node); err != nil {
		t.Fatalf("read a plan's time: %v", err)
	}
	return node.Total
}

// customBoardRead runs the statement the board's request ran with its arguments, planned with
// them (plan_cache_mode force_custom_plan) under the settings the read runs with
// (legionfacts.readByIndex), analyzed, and answers its plan.
func customBoardRead(t *testing.T, database *store.Store, ran ranStatement) json.RawMessage {
	t.Helper()
	ctx := t.Context()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the custom plan's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `set local enable_seqscan = off; set local plan_cache_mode = force_custom_plan`); err != nil {
		t.Fatalf("set the custom plan's settings: %v", err)
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "explain (analyze, buffers, format json) "+ran.sql, ran.args...).Scan(&raw); err != nil {
		t.Fatalf("explain %s: %v", ran.sql, err)
	}
	return explaintest.OnePlan(t, raw)
}

// boardReadLiterals is a board read's arguments, written as the literals an EXECUTE of the
// prepared statement takes, each quoted by Postgres itself, and their types: the project, for the
// read of the open rows, and the project and the lists of the cards' keys and of their trees'
// roots, for the read of the closed rows.
func boardReadLiterals(t *testing.T, database *store.Store, args []any) (types, literals []string) {
	t.Helper()
	switch len(args) {
	case 1:
		var project string
		if err := database.Pool.QueryRow(t.Context(), `select quote_literal($1::text)`, args...).Scan(&project); err != nil {
			t.Fatalf("quote the board read's argument: %v", err)
		}
		return []string{"text"}, []string{project}
	case 3:
		var project, keys, roots string
		if err := database.Pool.QueryRow(t.Context(),
			`select quote_literal($1::text), quote_literal($2::text[]::text), quote_literal($3::text[]::text)`,
			args...).Scan(&project, &keys, &roots); err != nil {
			t.Fatalf("quote the board read's arguments: %v", err)
		}
		return []string{"text", "text[]", "text[]"}, []string{project, keys + "::text[]", roots + "::text[]"}
	}
	t.Fatalf("a board read of the stored facts ran with %d arguments, want the project alone or the project, the keys and the roots", len(args))
	return nil, nil
}

// TestTheBoardsFactsReadUnderAGenericPlanStaysWithinItsBudget: the board's two reads of the
// stored facts, the statements its request runs with their own arguments, the read of the open
// rows and the read of the closed rows of what it shows, take at most boardReadGenericBudget
// together at INFRA's size under the generic plan a statement pgx caches can settle on after its
// fifth run, as under the plan with its values. The reads set that plan themselves
// (legionfacts.readByIndex), and this holds the statements to their budget without the setting:
// an earlier shape left the rows of the cards and the trees out of the open rows, which tested
// both lists on every open row, and a generic plan scans a list for each row it tests rather than
// hashing it. The two are held together, as the one read they were split from was, though the
// board runs the open rows' read beside its card reads.
//
// Each read runs boardReadRuns times under each mode, under the reads' own settings, and its
// median execution is what is held and recorded with the plan, so the record of each run says
// whether the plan mode moves a read's cost.
func TestTheBoardsFactsReadUnderAGenericPlanStaysWithinItsBudget(t *testing.T) {
	log := &statementLog{}
	handler, database, fixture := boardAtInfraSize(t, testServerOptions{store: storetest.TracedStore(t, storetest.Open(t), log)})
	ctx := t.Context()
	if _, err := database.Pool.Exec(ctx, `analyze issue_facts`); err != nil {
		t.Fatalf("analyze issue_facts: %v", err)
	}
	log.reset()
	if response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/"+fixture.Project+"/legion/board", nil, "alice"); response.Code != http.StatusOK {
		t.Fatalf("GET the board: status=%d body=%s", response.Code, response.Body.String())
	}
	reads := boardFactsReads(log.statements())
	if len(reads) != 2 {
		t.Fatalf("the board's request ran %d statements naming issue_facts, want its two reads of the stored facts", len(reads))
	}
	var open, rows int
	if err := database.Pool.QueryRow(ctx, `select count(*) filter (where closed_at is null), count(*) from issue_facts where project_key = $1`,
		fixture.Project).Scan(&open, &rows); err != nil {
		t.Fatalf("count the stored rows: %v", err)
	}
	connection, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the generic plans: %v", err)
	}
	defer connection.Release()

	medians := map[string]float64{}
	keys, roots := 0, 0
	for index, read := range reads {
		types, literals := boardReadLiterals(t, database, read.args)
		if len(read.args) == 3 {
			keys, roots = len(read.args[1].([]string)), len(read.args[2].([]string))
		}
		for _, mode := range []struct {
			name string
			plan func(run int) json.RawMessage
		}{
			{"custom", func(int) json.RawMessage { return customBoardRead(t, database, read) }},
			{"generic", func(run int) json.RawMessage {
				return explaintest.AnalyzedGenericPlan(t, ctx, connection, fmt.Sprintf("board_facts_read_%d_%d", index, run), read.sql,
					types, literals, "set local enable_seqscan = off")
			}},
		} {
			taken := make([]float64, 0, boardReadRuns)
			var last json.RawMessage
			for run := range boardReadRuns {
				last = mode.plan(run)
				taken = append(taken, planMilliseconds(t, last))
			}
			slices.Sort(taken)
			median := taken[len(taken)/2]
			medians[mode.name] += median
			t.Logf("under the %s plan the board's read\n%s\nran %d times, median %.3f ms, fastest %.3f ms, slowest %.3f ms; its last plan:\n%s",
				mode.name, strings.TrimSpace(read.sql), boardReadRuns, median, taken[0], taken[len(taken)-1], explaintest.Outline(t, last))
			for _, node := range explaintest.Nodes(t, last) {
				if node.Relation == "issue_facts" {
					t.Logf("  %s: condition %q, filter %q, %g rows a loop over %g loops, %g removed by the filter a loop",
						node, node.IndexCond, node.Filter, node.Rows, node.Loops, node.RowsRemovedByFilter)
				}
			}
		}
	}
	if medians["generic"] > boardReadGenericBudget {
		t.Errorf("under a generic plan the board's reads of the stored facts take a median %.3f ms together, above their %.0f ms budget",
			medians["generic"], boardReadGenericBudget)
	}
	t.Logf("project %s: %d stored rows, %d open; the read of the closed rows names %d card keys and %d roots; medians %.3f ms together under the custom plan and %.3f ms under the generic plan, against the %.0f ms budget",
		fixture.Project, rows, open, keys, roots, medians["custom"], medians["generic"], boardReadGenericBudget)
}

// eventLogRead and observationsRead match a statement that reads the event log and one that
// reads the daemon's observations.
var (
	eventLogRead     = regexp.MustCompile(`\bevents\b`)
	observationsRead = regexp.MustCompile(`\blegion_observations\b`)
)

// TestTheBoardsFactsReadStandsApartFromItsCardReads: no statement of the board's request reads
// the stored facts and the event log, or the stored facts and the daemon's observations,
// together.
//
// The board answers two things: the stored facts, which this issue adds, and the cards, which
// the daemon's state draws (dispatch://INFRA-1077). The card half reads the daemon's
// observations, and the tree walk's phase-work read reads the event log, so the request does
// reach both tables. This records every statement it ran and holds the two halves apart: a
// statement that read a fact out of the event log would be a fact computed on the request path,
// and there is none.
func TestTheBoardsFactsReadStandsApartFromItsCardReads(t *testing.T) {
	log := &statementLog{}
	handler, _, fixture := boardAtInfraSize(t, testServerOptions{store: storetest.TracedStore(t, storetest.Open(t), log)})

	log.reset()
	if response := dispatchRequest(t, handler, http.MethodGet,
		"/api/v1/projects/"+fixture.Project+"/legion/board", nil, "alice"); response.Code != http.StatusOK {
		t.Fatalf("GET the board: status=%d", response.Code)
	}

	ran := log.statements()
	facts, eventLog, observations := 0, 0, 0
	for _, statement := range ran {
		reads := []string{}
		if factsTableRead.MatchString(statement.sql) {
			facts++
			reads = append(reads, "issue_facts")
		}
		if eventLogRead.MatchString(statement.sql) {
			eventLog++
			reads = append(reads, "events")
		}
		if observationsRead.MatchString(statement.sql) {
			observations++
			reads = append(reads, "legion_observations")
		}
		if len(reads) < 2 {
			continue
		}
		t.Errorf("one statement of the board's request reads %s together:\n%s",
			strings.Join(reads, " and "), strings.TrimSpace(statement.sql))
	}
	if facts == 0 {
		t.Fatal("the board's request ran no statement reading issue_facts, so this measurement counts nothing")
	}
	t.Logf("the board's request ran %d statements: %d read issue_facts, %d read the event log, %d read legion_observations, and none reads the stored facts beside either",
		len(ran), facts, eventLog, observations)
}

// seedOtherProjectsFacts writes the stored rows of nine other projects, so the board's project is
// a tenth of the table rather than the whole of it: the deployment holds many projects, and which
// plan Postgres takes for one project's rows depends on that.
func seedOtherProjectsFacts(t *testing.T, database *store.Store, except string) {
	t.Helper()
	keys := make([]string, 0, otherFactsProjects)
	for index := range otherFactsProjects {
		keys = append(keys, fmt.Sprintf("OTHER%d", index))
	}
	if slices.Contains(keys, except) {
		t.Fatalf("the filler projects name the board's own project %s", except)
	}
	if _, err := database.Pool.Exec(t.Context(), `
		insert into issue_facts (project_key, issue_key, opened_at, hours_by_phase, work_hours,
		                         review_rounds, root_key, tree_depth, slot_hours, updated_at)
		select project, project || '-' || number, now() - interval '30 days', '{}'::jsonb, 1.5,
		       1, project || '-1', 1, 0.5, now()
		from unnest($1::text[]) as project, generate_series(1, $2::int) as number
	`, keys, otherFactsRows); err != nil {
		t.Fatalf("write the other projects' stored rows: %v", err)
	}
}

// TestTwoLegionBoardReadsThirtySecondsApartGrowEachOpenIssueByTheClock: with the facts consumer
// stopped, two board reads thirty seconds apart answer each open issue's work time grown by the
// clock time between them at the rate its row stores, and each closed issue's unchanged, and no
// row is written between them (k23).
//
// That is the one computation the read makes: an open issue's row holds its facts closed at the
// newest input they read and how fast each grows after it, and the read closes each at its own
// clock (legionfacts.Row.At). The consumer is the one writer of the rows, and the rig stopped it,
// so nothing moved the rows between the two reads, and the clock is all that moved the answers.
func TestTwoLegionBoardReadsThirtySecondsApartGrowEachOpenIssueByTheClock(t *testing.T) {
	handler, database, fixture := boardAtInfraSize(t, testServerOptions{})
	rates := map[string]float64{}
	rows, err := database.Pool.Query(t.Context(),
		`select issue_key, coalesce((growth->>'work_hours')::double precision, 0) from issue_facts where project_key = $1`, fixture.Project)
	if err != nil {
		t.Fatalf("read the stored growth: %v", err)
	}
	for rows.Next() {
		var key string
		var rate float64
		if err := rows.Scan(&key, &rate); err != nil {
			t.Fatalf("read the stored growth: %v", err)
		}
		rates[key] = rate
	}
	rows.Close()
	written := func() (count int64, newest time.Time) {
		t.Helper()
		if err := database.Pool.QueryRow(t.Context(),
			`select count(*), coalesce(max(updated_at), 'epoch') from issue_facts`).Scan(&count, &newest); err != nil {
			t.Fatalf("read the stored rows' writes: %v", err)
		}
		return count, newest
	}
	countBefore, newestBefore := written()

	first := legionBoardOf(t, handler, fixture.Project)
	time.Sleep(30 * time.Second)
	second := legionBoardOf(t, handler, fixture.Project)
	apart := second.ComputedAt.Sub(first.ComputedAt)

	open, closed, wrong := 0, 0, 0
	for _, key := range slices.Sorted(maps.Keys(first.Issues)) {
		was := first.Issues[key]
		now, has := second.Issues[key]
		if !has {
			t.Errorf("%s holds an entry in the first read and none in the second", key)
			continue
		}
		want := was.WorkHours
		if was.ClosedAt == nil {
			open++
			want += rates[key] * apart.Hours()
		} else {
			closed++
		}
		if math.Abs(now.WorkHours-want) > 1e-3 {
			wrong++
			t.Errorf("%s (closed %t): the first read says %g work hours and the second, %s later, says %g, want %g",
				key, was.ClosedAt != nil, was.WorkHours, apart, now.WorkHours, want)
		}
	}
	if open == 0 || closed == 0 {
		t.Fatalf("the board holds %d open and %d closed issues, so the two reads compare too little", open, closed)
	}
	if countAfter, newestAfter := written(); countAfter != countBefore || !newestAfter.Equal(newestBefore) {
		t.Errorf("a row was written between the two reads: %d rows newest %s before, %d newest %s after",
			countBefore, newestBefore, countAfter, newestAfter)
	}
	t.Logf("two reads %s apart, the consumer stopped: %d open issues each grown by its rate times the time between, %d closed issues unchanged, %d wrong; no row written between them; the project holds %d issues",
		apart, open, closed, wrong, len(fixture.Keys))
}

// TestAFreshProcessFirstBoardReadIsAsFastAsAWarmOne: the first board read of a process that has
// served none is as fast as the read of a process that has served reads for a while, to the same
// order.
//
// A process that held the facts between calls would answer its first read slowly and every read
// after it quickly, because the first would fill what the rest read. The second process here is
// the deployment's second envoy-dispatch task (storetest.SecondProcess): the same database, a
// pool of its own and every cache of its own empty.
func TestAFreshProcessFirstBoardReadIsAsFastAsAWarmOne(t *testing.T) {
	handler, database, fixture := boardAtInfraSize(t, testServerOptions{})
	target := "/api/v1/projects/" + fixture.Project + "/legion/board"
	read := func(served http.Handler) time.Duration {
		started := time.Now()
		if response := dispatchRequest(t, served, http.MethodGet, target, nil, "alice"); response.Code != http.StatusOK {
			t.Fatalf("GET the board: status=%d body=%s", response.Code, response.Body.String())
		}
		return time.Since(started)
	}

	// The warm process: it has served reads for a while, and the time of record is the read
	// after them.
	warmUntil := time.Now().Add(3 * time.Second)
	served := 0
	for time.Now().Before(warmUntil) {
		read(handler)
		served++
	}
	warm := read(handler)

	// The fresh process: a second server on the same database, its own pool and its own caches,
	// timed on the first read it ever serves.
	other, _, _ := newTestServer(t, testServerOptions{store: storetest.SecondProcess(t, database)})
	fresh := read(other)

	ratio := float64(fresh) / float64(warm)
	if ratio >= 10 {
		t.Errorf("the fresh process's first read took %s and a warm read takes %s, %.1f times as long: not the same order",
			fresh.Round(time.Millisecond), warm.Round(time.Millisecond), ratio)
	}
	t.Logf("after %d reads served, a warm read of %s's board took %s; a fresh process's first read of the same board took %s, %.2f times the warm one",
		served, fixture.Project, warm.Round(100*time.Microsecond), fresh.Round(100*time.Microsecond), ratio)
}

// sourceHit is one line one search matched.
type sourceHit struct {
	file string
	line int
	text string
}

func (h sourceHit) String() string {
	return fmt.Sprintf("%s:%d: %s", h.file, h.line, strings.TrimSpace(h.text))
}

// searchSource answers every line of files that matches want, and also matches with where with is
// given, with how many lines it read. A search over files it cannot read fails the test rather
// than answering nothing: a search that examined nothing is no evidence.
func searchSource(t *testing.T, files []string, want, with *regexp.Regexp) ([]sourceHit, int) {
	t.Helper()
	hits, read := []sourceHit{}, 0
	for _, file := range files {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s for the search: %v", file, err)
		}
		for index, line := range strings.Split(string(body), "\n") {
			read++
			if !want.MatchString(line) {
				continue
			}
			if with != nil && !with.MatchString(line) {
				continue
			}
			hits = append(hits, sourceHit{file: file, line: index + 1, text: line})
		}
	}
	if read == 0 {
		t.Fatalf("the search read no line of %v", files)
	}
	return hits, read
}

// boardReadPathFiles is every file that holds a function the board's GET runs below the router:
//
//   - legion_board.go is the handler, the reads it makes and the body it writes, the read of the
//     phase work of the issues its tree walk climbs onto among them (readPhaseWork);
//   - legionfacts/store.go holds Read, the one read of the stored rows;
//   - legionfacts/close.go and legion/metrics/grow.go are the close of each open part of a row at
//     the read's own clock (boardCloseFiles), the one computation the read makes;
//   - the legion package's own files hold every rule the board calls: the settings read, the
//     column an issue stands in, why a card waits, and the tree walk.
//
// The legion package is taken whole rather than by the files the board reaches, so the search is
// wider than the path rather than narrower.
var boardReadPathFiles = append([]string{
	"legion_board.go",
	"../legionfacts/store.go",
	"../legion/board_rules.go",
	"../legion/phase_work.go",
	"../legion/session_title.go",
	"../legion/state.go",
	"../legion/store.go",
	"../legion/tree.go",
}, boardCloseFiles...)

// boardCloseFiles is the close of a stored row at the read's own clock (legionfacts.Row.At and
// metrics.IssueFacts.Grown), the one place on the read path a length of time may meet a stored
// fact (R21).
var boardCloseFiles = []string{"../legionfacts/close.go", "../legion/metrics/grow.go"}

var (
	// computesAFact matches a call into the one computation of the Legion facts: an exported
	// function of the metrics package, or legionfacts' own Compute or ComputeProject.
	computesAFact = regexp.MustCompile(`\bmetrics\.[A-Z]\w*\(|\blegionfacts\.Compute\w*\(|(?:^|[^.\w])Compute(?:Project)?\(`)
	// elapsedTime matches a line that turns the clock into a length: what an open issue's hours
	// are made of.
	elapsedTime = regexp.MustCompile(`time\.Since\(|\.Sub\(|\.Hours\(\)|\.Minutes\(\)|\.Seconds\(\)`)
	// storedValue matches a line that names one of the stored facts, by the wire's name or the
	// computation's (PhaseHours, which the board's HoursByPhase carries). A line that matches both
	// this and elapsedTime adds elapsed time to a stored value.
	storedValue = regexp.MustCompile(`WorkHours|HoursByPhase|PhaseHours|SlotHours|ReviewRounds|TreeDepth|RootKey|work_hours|hours_by_phase|slot_hours`)
	// cachedFacts matches a struct field whose type holds the stored facts or the population
	// they are computed from.
	cachedFacts = regexp.MustCompile(`legionfacts\.Row|metrics\.Population|\[\]Row\b|map\[string\]Row\b`)
)

// TestNoCodeOnTheBoardsReadPathComputesAFact: three searches over the board's read path, each
// with the control that shows it finds what it looks for.
//
// The three are what checks k2 and k23 name: no code on the request path computes a fact, none
// adds elapsed time to a stored value but the close of each open part at the read's own clock
// (boardCloseFiles), and none holds a cached population. Each search prints every line it
// matched, so a reader judges the finding rather than taking a count. Each control runs the same
// search over code that does hold what the search looks for, so finding nothing on the read path
// is a reading and not an empty answer.
func TestNoCodeOnTheBoardsReadPathComputesAFact(t *testing.T) {
	// 1. A call that computes a fact.
	hits, lines := searchSource(t, boardReadPathFiles, computesAFact, nil)
	for _, hit := range hits {
		t.Errorf("the board's read path computes a fact: %s", hit)
	}
	t.Logf("search 1, a call into the fact computation (%s): %d lines of %d files read, %d matched",
		computesAFact, lines, len(boardReadPathFiles), len(hits))
	control, controlLines := searchSource(t, []string{"../legionfacts/facts.go"}, computesAFact, nil)
	if len(control) == 0 {
		t.Errorf("the control found no call in legionfacts/facts.go, which computes the facts, so search 1 proves nothing")
	}
	for _, hit := range control {
		t.Logf("search 1's control (legionfacts/facts.go, the consumer's computation): %s", hit)
	}
	t.Logf("search 1's control read %d lines and matched %d", controlLines, len(control))

	// 2. Elapsed time added to a stored value, anywhere but the close.
	hits, lines = searchSource(t, boardReadPathFiles, elapsedTime, storedValue)
	for _, hit := range hits {
		if !slices.Contains(boardCloseFiles, hit.file) {
			t.Errorf("the board's read path adds elapsed time to a stored value outside the close: %s", hit)
		}
	}
	elapsed, _ := searchSource(t, boardReadPathFiles, elapsedTime, nil)
	t.Logf("search 2, elapsed time on a line that names a stored fact (%s, with %s): %d lines read, %d lines make a length at all, %d of them name a stored fact",
		elapsedTime, storedValue, lines, len(elapsed), len(hits))
	closing := 0
	for _, hit := range elapsed {
		if slices.Contains(boardCloseFiles, hit.file) {
			closing++
			t.Logf("search 2 read this line of the close, the one computation of the read: %s", hit)
			continue
		}
		t.Logf("search 2 read this line, which makes a length and names no stored fact: %s", hit)
	}
	if closing == 0 {
		t.Errorf("no line of %v makes a length of time, so the close the read makes is not where search 2 reads it", boardCloseFiles)
	}
	if closes, _ := searchSource(t, []string{"../legionfacts/store.go"}, regexp.MustCompile(`\.At\(at\)`), nil); len(closes) == 0 {
		t.Errorf("legionfacts/store.go closes no row it reads at the read's clock (Row.At), so the board would serve the rows as stored")
	}
	control, controlLines = searchSource(t, []string{"../legion/legiontest/legiontest.go"}, elapsedTime, storedValue)
	if len(control) == 0 {
		t.Errorf("the control found no line in legiontest.go, which makes a slot's hours out of two moments, so search 2 proves nothing")
	}
	for _, hit := range control {
		t.Logf("search 2's control (legiontest.go, which makes hours from elapsed time): %s", hit)
	}
	t.Logf("search 2's control read %d lines and matched %d", controlLines, len(control))

	// 3. A cached population: a field of the server, which is what lives between requests.
	fields := serverFields(t)
	if len(fields) == 0 {
		t.Fatal("no field of type server was read, so search 3 counts nothing")
	}
	held := []sourceHit{}
	for _, field := range fields {
		if cachedFacts.MatchString(field.text) {
			held = append(held, field)
		}
	}
	for _, hit := range held {
		t.Errorf("the server holds the facts between requests: %s", hit)
	}
	t.Logf("search 3, a field of type server whose type holds the facts or the population (%s): %d fields read, %d matched",
		cachedFacts, len(fields), len(held))
	for _, field := range fields {
		if strings.Contains(strings.ToLower(field.text), "legion") {
			t.Logf("search 3 read this Legion field of the server, which holds issue keys and no fact: %s", field)
		}
	}
	control, controlLines = searchSource(t, []string{"legion_board.go"}, cachedFacts, nil)
	if len(control) == 0 {
		t.Errorf("the control found no field holding the facts in legion_board.go, whose per-request population holds them, so search 3 proves nothing")
	}
	for _, hit := range control {
		t.Logf("search 3's control (legion_board.go's per-request population, which does hold the rows): %s", hit)
	}
	t.Logf("search 3's control read %d lines and matched %d", controlLines, len(control))
}

// serverFields is every line of `type server struct` that declares a field: what this process
// keeps between requests. The block runs from its opening line to the first line that closes it.
func serverFields(t *testing.T) []sourceHit {
	t.Helper()
	body, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	lines := strings.Split(string(body), "\n")
	start := slices.Index(lines, "type server struct {")
	if start < 0 {
		t.Fatal("server.go holds no `type server struct {` line, so the search cannot read the server's fields")
	}
	fields := []sourceHit{}
	for index := start + 1; index < len(lines); index++ {
		line := lines[index]
		if line == "}" {
			return fields
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		fields = append(fields, sourceHit{file: "server.go", line: index + 1, text: line})
	}
	t.Fatal("server.go's `type server struct` block has no closing line")
	return nil
}
