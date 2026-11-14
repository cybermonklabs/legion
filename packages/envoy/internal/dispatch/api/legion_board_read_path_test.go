package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
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

// What check k2 of dispatch://INFRA-2915 holds the board's read path to, measured here: the facts
// are read as rows, each plan is an index read of issue_facts, no plan reads the event log or
// legion_observations, nothing on the request path computes a fact, adds elapsed time to a stored
// value or holds a cached population, two reads 30 s apart with the consumer stopped answer the
// same work time for every open issue, and a fresh process's first read is as fast as a warm one
// to the same order.
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

// planRelations is every relation a plan reads, at any depth, each once and sorted: what says
// whether a plan reaches the event log or the daemon's observations.
func planRelations(t *testing.T, planJSON json.RawMessage) []string {
	t.Helper()
	var node struct {
		Relation string            `json:"Relation Name"`
		Plans    []json.RawMessage `json:"Plans"`
	}
	if err := json.Unmarshal(planJSON, &node); err != nil {
		t.Fatalf("decode a plan node: %v", err)
	}
	named := []string{}
	if node.Relation != "" {
		named = append(named, node.Relation)
	}
	for _, child := range node.Plans {
		named = append(named, planRelations(t, child)...)
	}
	slices.Sort(named)
	return slices.Compact(named)
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

// TestTheBoardsFactsReadIsAnIndexReadOfIssueFacts: the board's read of the stored facts is one
// index read of issue_facts, and it reads neither the event log nor the daemon's observations.
//
// The statement explained is the statement the request itself ran: the server reads through a
// traced pool, the request's statements are recorded with their arguments, and every one of them
// that names issue_facts is explained with those arguments. So no copy of the read can drift from
// the read.
//
// Two plans are taken. The first is over the one project's rows alone, which is what a deployment
// of one project holds; the second is over ten projects' rows, which is what Dispatch holds. The
// second is the plan held to the index: which plan Postgres takes for `where project_key = $1`
// depends on how much of the table the one project is, and reading a whole table every row of
// which is wanted is the right plan for that table.
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
		for _, read := range reads {
			explained := explainBoardRead(t, database, read)
			examined++
			relations := planRelations(t, explained.plan)
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
			if len(scans) == 0 {
				t.Errorf("over ten projects' rows the board's read of the stored facts reads no index:\n%s", explained.text)
				continue
			}
			for _, scan := range scans {
				t.Logf("the read takes %s", scan)
			}
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

// TestTwoBoardReadsThirtySecondsApartAnswerTheSameWorkTime: with the facts consumer stopped, two
// board reads SweepEvery apart answer the same work time for every open issue.
//
// That is what shows the read adds nothing to a stored value. An open issue's work time grows
// with the clock, so a read that computed it, or that added the time since the row was written to
// it, would answer a larger number on the second read. The consumer is the one writer of the
// rows, and the rig stopped it, so nothing moved the rows between the two reads.
func TestTwoBoardReadsThirtySecondsApartAnswerTheSameWorkTime(t *testing.T) {
	handler, _, fixture := boardAtInfraSize(t, testServerOptions{})

	firstAt := time.Now()
	first := legionBoardOf(t, handler, fixture.Project)
	time.Sleep(30 * time.Second)
	second := legionBoardOf(t, handler, fixture.Project)
	apart := time.Since(firstAt)

	open, moved := 0, 0
	for _, key := range slices.Sorted(maps.Keys(first.Issues)) {
		was := first.Issues[key]
		if was.ClosedAt != nil {
			continue
		}
		now, has := second.Issues[key]
		if !has {
			t.Errorf("%s is open and the second read holds no entry of it", key)
			continue
		}
		open++
		if was.WorkHours != now.WorkHours {
			moved++
			t.Errorf("%s: the first read says %g work hours and the second, %s later, says %g",
				key, was.WorkHours, apart.Round(time.Second), now.WorkHours)
		}
	}
	if open == 0 {
		t.Fatal("the board holds no open issue, so the two reads compare nothing")
	}
	t.Logf("two reads %s apart, the consumer stopped: %d open issues compared, %d moved; the project holds %d issues, %d of them open",
		apart.Round(time.Second), open, moved, len(fixture.Keys), len(fixture.Open))
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
//   - legion_board.go is the handler, the reads it makes and the body it writes;
//   - legion_phase_work.go is the one thing the handler keeps between requests, the issues that
//     carry Legion phase work, which the tree walk reads;
//   - legionfacts/store.go holds Read, the one read of the stored rows;
//   - the legion package's own files hold every rule the board calls: the settings read, the
//     column an issue stands in, why a card waits, and the tree walk.
//
// The legion package is taken whole rather than by the files the board reaches, so the search is
// wider than the path rather than narrower.
var boardReadPathFiles = []string{
	"legion_board.go",
	"legion_phase_work.go",
	"../legionfacts/store.go",
	"../legion/board_rules.go",
	"../legion/phase_work.go",
	"../legion/session_title.go",
	"../legion/state.go",
	"../legion/store.go",
	"../legion/tree.go",
}

var (
	// computesAFact matches a call into the one computation of the Legion facts: an exported
	// function of the metrics package, or legionfacts' own Compute or ComputeProject.
	computesAFact = regexp.MustCompile(`\bmetrics\.[A-Z]\w*\(|\blegionfacts\.Compute\w*\(|(?:^|[^.\w])Compute(?:Project)?\(`)
	// elapsedTime matches a line that turns the clock into a length: what an open issue's hours
	// are made of.
	elapsedTime = regexp.MustCompile(`time\.Since\(|\.Sub\(|\.Hours\(\)|\.Minutes\(\)|\.Seconds\(\)`)
	// storedValue matches a line that names one of the stored facts. A line that matches both
	// this and elapsedTime adds elapsed time to a stored value.
	storedValue = regexp.MustCompile(`WorkHours|HoursByPhase|SlotHours|ReviewRounds|TreeDepth|RootKey|work_hours|hours_by_phase|slot_hours`)
	// cachedFacts matches a struct field whose type holds the stored facts or the population
	// they are computed from.
	cachedFacts = regexp.MustCompile(`legionfacts\.Row|metrics\.Population|\[\]Row\b|map\[string\]Row\b`)
)

// TestNoCodeOnTheBoardsReadPathComputesAFact: three searches over the board's read path, each
// with the control that shows it finds what it looks for.
//
// The three are what check k2 names: no code on the request path computes a fact, none adds
// elapsed time to a stored value, and none holds a cached population. Each search prints every
// line it matched, so a reader judges the finding rather than taking a count. Each control runs
// the same search over code that does hold what the search looks for, so finding nothing on the
// read path is a reading and not an empty answer.
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

	// 2. Elapsed time added to a stored value.
	hits, lines = searchSource(t, boardReadPathFiles, elapsedTime, storedValue)
	for _, hit := range hits {
		t.Errorf("the board's read path adds elapsed time to a stored value: %s", hit)
	}
	elapsed, _ := searchSource(t, boardReadPathFiles, elapsedTime, nil)
	t.Logf("search 2, elapsed time on a line that names a stored fact (%s, with %s): %d lines read, %d lines make a length at all, %d of them name a stored fact",
		elapsedTime, storedValue, lines, len(elapsed), len(hits))
	for _, hit := range elapsed {
		t.Logf("search 2 read this line, which makes a length and names no stored fact: %s", hit)
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
