package eval

//ci:reads packages/contracts/fixtures/shown-values.json os.ReadFile (../../../contracts/fixtures/shown-values.json): TestEachStatusWordIsThePagesWord holds the eval's status words (chips.StatusWords) to the words the web writes

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/going/chips"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// evalPerson is the person a test's try acts as, who is none of the seed's people.
const evalPerson = "eval-person@example.com"

// seededStore is a database holding the committed seed, the seed and the committed tasks, and the
// store as the seed made it.
func seededStore(t *testing.T) (*store.Store, *Seed, []Task, *Store) {
	t.Helper()
	ctx := context.Background()
	seed, err := LoadSeed(Files)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := LoadTasks(Files)
	if err != nil {
		t.Fatal(err)
	}
	database := storetest.Open(t)
	if written, err := WriteSeed(ctx, database, seed); err != nil || !written {
		t.Fatalf("WriteSeed = %v, %v; want the seed written", written, err)
	}
	before, err := ReadStore(ctx, database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	return database, seed, tasks, before
}

// taskByID is the committed task of that id.
func taskByID(t *testing.T, tasks []Task, id string) Task {
	t.Helper()
	for _, task := range tasks {
		if task.ID == id {
			return task
		}
	}
	t.Fatalf("the eval holds no task %s", id)
	return Task{}
}

// exec runs each statement on the database, failing the test on the first that fails.
func exec(t *testing.T, database *store.Store, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if _, err := database.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// TestAChangeTaskFailsOnAChangeItDidNotAskFor: on the seeded store, change-03 ("move DEMO-8,
// DEMO-10 and DEMO-37 to todo") passes with exactly its changes made, and fails once the store
// also holds one change it does not allow, a new priority on DEMO-9, which the verdict names.
func TestAChangeTaskFailsOnAChangeItDidNotAskFor(t *testing.T) {
	ctx := context.Background()
	database, seed, tasks, before := seededStore(t)
	task := taskByID(t, tasks, "change-03")

	exec(t, database, `update issues set status = 'todo' where key in ('DEMO-8', 'DEMO-10', 'DEMO-37')`)
	after, err := ReadStore(ctx, database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	if verdict := GradeEnd(seed, task, before, after, evalPerson); !verdict.Pass() {
		t.Fatalf("change-03 with exactly its changes graded %+v; want a pass", verdict)
	}

	exec(t, database, `update issues set priority = 0 where key = 'DEMO-9'`)
	after, err = ReadStore(ctx, database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	verdict := GradeEnd(seed, task, before, after, evalPerson)
	if verdict.Pass() {
		t.Fatalf("change-03 with its changes and a new priority on DEMO-9 graded a pass; want a fail")
	}
	if len(verdict.Missing) != 0 || len(verdict.Unasked) != 1 || !strings.Contains(verdict.Unasked[0], "priority of DEMO-9") {
		t.Errorf("the verdict is %+v; want nothing missing and one change not asked for, the priority of DEMO-9", verdict)
	}
}

// TestTheCheckerReportsAMessageNoApprovedRowMade: on the seeded store holding one message the
// person wrote on DEMO-3 to its architect, which handoff-02 asks for, and no plan, the checker of
// c13's message clause reports the message as made by no approved row; once a run of the person
// holds an approved row whose landed request made it, the checker reports nothing; and the same
// message under change-03, which asks for no message, is reported as not asked for.
func TestTheCheckerReportsAMessageNoApprovedRowMade(t *testing.T) {
	ctx := context.Background()
	database, seed, tasks, before := seededStore(t)
	handoff := taskByID(t, tasks, "handoff-02")
	message := uuid.NewString()
	exec(t, database, `insert into messages (id, issue_key, author, body, target, created_at) values ('`+message+
		`', 'DEMO-3', '{"kind": "user", "id": "`+evalPerson+`"}', 'Please review the spec of DEMO-3.', 'session:session-architect-01', now())`)
	after, err := ReadStore(ctx, database.Pool)
	if err != nil {
		t.Fatal(err)
	}

	broken, err := CheckPersonWrites(ctx, database.Pool, seed, handoff, before, after, evalPerson)
	if err != nil {
		t.Fatal(err)
	}
	if len(broken) != 1 || !strings.Contains(broken[0], "no approved row") || !strings.Contains(broken[0], "DEMO-3") {
		t.Fatalf("the checker reported %q on a message no approved row made; want that one message", broken)
	}

	run := uuid.NewString()
	exec(t, database,
		`insert into going_runs (id, author_login, words, state) values ('`+run+`', '`+evalPerson+`', 'ask the architect to review it', 'done')`,
		`insert into going_plans (run_id, plan, state) values ('`+run+`', 1, 'done')`,
		`insert into going_plan_rows (run_id, plan, row, change, state, approval, result) values ('`+run+
			`', 1, 1, '{"tool": "dispatch_message"}', 'done', '{"by": "person"}', '{"step": 2, "call": 1, "status": 201}')`,
		`insert into going_landings (run_id, phase, step, call, request, method, path, body_digest, answer) values ('`+run+
			`', 'do', 2, 1, 0, 'POST', '/api/v1/issues/DEMO-3/messages', 'digest', '{"status": 201, "body": {"id": "`+message+`"}}')`,
	)
	broken, err = CheckPersonWrites(ctx, database.Pool, seed, handoff, before, after, evalPerson)
	if err != nil {
		t.Fatal(err)
	}
	if len(broken) != 0 {
		t.Errorf("the checker reported %q on a message an approved row made; want nothing", broken)
	}

	broken, err = CheckPersonWrites(ctx, database.Pool, seed, taskByID(t, tasks, "change-03"), before, after, evalPerson)
	if err != nil {
		t.Fatal(err)
	}
	if len(broken) != 1 || !strings.Contains(broken[0], "does not ask for it") {
		t.Errorf("the checker reported %q on a message change-03 does not ask for; want that one message", broken)
	}
}

// TestTheCheckerMatchesAWriteToTheRequestThatMadeIt: the create of a comment hands no answer to
// its landing, so a suggestion's landing records none. On the seeded store, two suggestions the
// person's run posted on DEMO-3's spec, each by an approved row whose request posted to DEMO-3's
// comments, are made by those rows under document-01, which asks for them; a third suggestion of
// that run, which no third request made, and one whose author names another run, are not.
func TestTheCheckerMatchesAWriteToTheRequestThatMadeIt(t *testing.T) {
	ctx := context.Background()
	database, seed, tasks, before := seededStore(t)
	task := taskByID(t, tasks, "document-01")
	var spec string
	if err := database.Pool.QueryRow(ctx, `select id::text from artifacts where issue_key = 'DEMO-3' and slug = 'spec'`).Scan(&spec); err != nil {
		t.Fatal(err)
	}
	run := uuid.NewString()
	exec(t, database,
		`insert into going_runs (id, author_login, words, state) values ('`+run+`', '`+evalPerson+`', 'suggest an edit', 'done')`,
		`insert into going_plans (run_id, plan, state) values ('`+run+`', 1, 'done')`)
	for row := range 2 {
		step := fmt.Sprint(11 + row)
		exec(t, database,
			`insert into going_plan_rows (run_id, plan, row, change, state, approval, result) values ('`+run+`', 1, `+fmt.Sprint(row)+
				`, '{"tool": "dispatch_suggest"}', 'done', '{"by": "person"}', '{"step": `+step+`, "call": 1, "status": 201}')`,
			`insert into going_landings (run_id, phase, step, call, request, method, path, body_digest, answer) values ('`+run+
				`', 'do', `+step+`, 1, 0, 'POST', '/api/v1/issues/DEMO-3/comments', 'digest', '{"status": 0, "body": null}')`)
	}
	suggest := func(run, body string) {
		exec(t, database, `insert into comments (issue_key, author, body, anchor, suggestion) values ('DEMO-3', `+
			`'{"kind": "user", "id": "`+evalPerson+`", "act": {"run": "`+run+`", "agent": "command-bar"}}', '`+body+`', `+
			`'{"quote": "Back-links in issue specs", "artifact_id": "`+spec+`"}', '{"replace_with": "x", "accepted": null}')`)
	}
	check := func() []string {
		t.Helper()
		after, err := ReadStore(ctx, database.Pool)
		if err != nil {
			t.Fatal(err)
		}
		broken, err := CheckPersonWrites(ctx, database.Pool, seed, task, before, after, evalPerson)
		if err != nil {
			t.Fatal(err)
		}
		return broken
	}
	suggest(run, "Put the back-link first.")
	suggest(run, "Remove it from the end.")
	if broken := check(); len(broken) != 0 {
		t.Fatalf("the checker reported %q on two suggestions two approved rows made; want nothing", broken)
	}
	suggest(run, "A third one.")
	suggest(uuid.NewString(), "Another run.")
	if broken := check(); len(broken) != 2 || !strings.Contains(broken[0], "no approved row") || !strings.Contains(broken[1], "no approved row") {
		t.Errorf("the checker reported %q; want the two suggestions no approved row made", broken)
	}
}

// TestAChartShowsItsRowsByTheWeeksItDraws: chart-01's rows, written by ISO week, are shown by
// a drawn chart whose server chain names each week by its first day, as the server draws it; the
// same chart with one week's count wrong misses that row, as a number no row the server drew holds.
func TestAChartShowsItsRowsByTheWeeksItDraws(t *testing.T) {
	tasks, err := LoadTasks(Files)
	if err != nil {
		t.Fatal(err)
	}
	chart := taskByID(t, tasks, "chart-01").Grade.Chart
	drawn := func(counts ...int) []json.RawMessage {
		weeks := []string{"2026-09-28", "2026-10-05", "2026-10-12", "2026-10-19", "2026-10-26", "2026-11-02", "2026-11-09"}
		rows := make([]map[string]any, len(weeks))
		for i, week := range weeks {
			rows[i] = map[string]any{"week": week, "closed": counts[i]}
		}
		block, err := json.Marshal(map[string]any{"kind": "chart", "title": "Issues closed each week in DEMO", "rows": rows,
			"spec": map[string]any{"x": "week", "y": "closed", "chain": []map[string]any{{"op": "group", "field": "closed_at", "per": "week"}, {"op": "count", "as": "closed"}}}})
		if err != nil {
			t.Fatal(err)
		}
		return []json.RawMessage{block}
	}
	if missing := MissingChartRows(chart.Rows, "", drawn(2, 2, 1, 2, 1, 1, 2)); len(missing) != 0 {
		t.Errorf("the drawn chart of every week's count misses %v; want none", missing)
	}
	if missing := MissingChartRows(chart.Rows, "", drawn(2, 2, 1, 2, 1, 1, 3)); len(missing) != 1 || missing[0] != "2026-W46=2" {
		t.Errorf("the drawn chart with 3 closed in the week of 9 November misses %v; want 2026-W46=2", missing)
	}
}

// TestAChartShowsItsPriorityRowsByTheNumberItDraws: chart-04's rows, written P1 to P3, are shown by
// a drawn chart whose server chain groups by priority and so names each group by the number the
// issue reads carry, as run 3 of the eval drew it; a priority's number is not its count, so a row
// whose count is wrong still misses, and so does a number under another field.
func TestAChartShowsItsPriorityRowsByTheNumberItDraws(t *testing.T) {
	tasks, err := LoadTasks(Files)
	if err != nil {
		t.Fatal(err)
	}
	chart := taskByID(t, tasks, "chart-04").Grade.Chart
	drawn := func(field string, rows ...[2]int) []json.RawMessage {
		drawnRows := make([]map[string]any, len(rows))
		for i, row := range rows {
			drawnRows[i] = map[string]any{field: row[0], "bugs": row[1]}
		}
		block, err := json.Marshal(map[string]any{"kind": "chart", "title": "Open bugs in DEMO by priority", "rows": drawnRows,
			"spec": map[string]any{"x": field, "y": "bugs", "chain": []map[string]any{{"op": "group", "field": field}, {"op": "count", "as": "bugs"}}}})
		if err != nil {
			t.Fatal(err)
		}
		return []json.RawMessage{block}
	}
	if missing := MissingChartRows(chart.Rows, "", drawn("priority", [2]int{1, 6}, [2]int{2, 4}, [2]int{3, 1})); len(missing) != 0 {
		t.Errorf("the drawn chart of each priority's count misses %v; want none", missing)
	}
	if missing := MissingChartRows(chart.Rows, "", drawn("priority", [2]int{1, 6}, [2]int{2, 5}, [2]int{3, 1})); len(missing) != 1 || missing[0] != "P2=4" {
		t.Errorf("the drawn chart with 5 bugs at P2 misses %v; want P2=4", missing)
	}
	if missing := MissingChartRows(chart.Rows, "", drawn("estimate", [2]int{1, 6}, [2]int{2, 4}, [2]int{3, 1})); len(missing) != 3 {
		t.Errorf("the drawn chart by estimate misses %v; want every row, since no field of it is the priority", missing)
	}
}

// TestATablesCountIsReadInTheColumnItsChainWrites: chart-15 of the eval drew a table of the seven
// OPS alerts themselves, each with its status and its priority, and the priority 2 of OPS-28, whose
// status is done, was read as the count of the done alerts: three of the five rows were shown so.
// A count is read only in a column the block's chain counts into, beside its label: that table
// shows none of the rows, the table of the alerts grouped by status and counted shows each, and
// the same table with one count wrong misses that row, whose count no row the server drew holds.
func TestATablesCountIsReadInTheColumnItsChainWrites(t *testing.T) {
	tasks, err := LoadTasks(Files)
	if err != nil {
		t.Fatal(err)
	}
	chart := taskByID(t, tasks, "chart-15").Grade.Chart
	table := func(columns []string, chain []map[string]any, rows []map[string]any) []json.RawMessage {
		block, err := json.Marshal(map[string]any{"kind": "table", "title": "OPS alerts by status", "rows": rows,
			"spec": map[string]any{"kind": "table", "columns": columns, "chain": chain}})
		if err != nil {
			t.Fatal(err)
		}
		return []json.RawMessage{block}
	}
	alerts := table([]string{"key", "status", "priority", "title"},
		[]map[string]any{{"op": "filter", "field": "labels", "has": "alert"}, {"op": "sort", "field": "status", "order": "asc"}},
		[]map[string]any{
			{"key": "OPS-10", "status": "backlog", "priority": 2}, {"key": "OPS-7", "status": "done", "priority": 1},
			{"key": "OPS-28", "status": "done", "priority": 2}, {"key": "OPS-2", "status": "in_progress", "priority": 1},
			{"key": "OPS-6", "status": "todo", "priority": 2}, {"key": "OPS-30", "status": "todo", "priority": 2},
			{"key": "OPS-21", "status": "triage", "priority": nil},
		})
	if missing := MissingChartRows(chart.Rows, "", alerts); len(missing) != len(chart.Rows) {
		t.Errorf("the table of the alerts themselves misses %v; want every row, since none of its columns counts", missing)
	}
	counted := func(counts ...int) []json.RawMessage {
		statuses := []string{"backlog", "done", "in_progress", "todo", "triage"}
		rows := make([]map[string]any, len(statuses))
		for i, status := range statuses {
			rows[i] = map[string]any{"status": status, "alerts": counts[i], "priority": 2}
		}
		return table([]string{"status", "alerts", "priority"},
			[]map[string]any{{"op": "filter", "field": "labels", "has": "alert"}, {"op": "group", "field": "status"}, {"op": "count", "as": "alerts"}, {"op": "max", "field": "priority", "as": "priority"}}, rows)
	}
	if missing := MissingChartRows(chart.Rows, "", counted(1, 2, 1, 2, 1)); len(missing) != 0 {
		t.Errorf("the table of each status's count of alerts misses %v; want none", missing)
	}
	if missing := MissingChartRows(chart.Rows, "", counted(1, 2, 1, 3, 1)); len(missing) != 1 || missing[0] != "todo=2" {
		t.Errorf("the table with 3 alerts in todo misses %v; want todo=2", missing)
	}
}

// TestEachStatusWordIsThePagesWord: packages/contracts/fixtures/shown-values.json holds the words
// the board and the issue page write for each status, which the web's board-model.test.ts holds
// to its statusLabels. The eval renders a status's chip and reads a status's count in the prose by
// the same words (chips.StatusWords), so the eval and the page name each status alike.
func TestEachStatusWordIsThePagesWord(t *testing.T) {
	raw, err := os.ReadFile("../../../contracts/fixtures/shown-values.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Rows []struct {
			Kind  string `json:"kind"`
			Value any    `json:"value"`
			Shown string `json:"shown"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	held := map[string]string{}
	for _, row := range fixture.Rows {
		if row.Kind == "status" {
			held[fmt.Sprint(row.Value)] = row.Shown
		}
	}
	if !maps.Equal(held, chips.StatusWords) {
		t.Errorf("the fixture's statuses are written %v, and the eval reads them as %v; want one set of words", held, chips.StatusWords)
	}
}

// TestAStatusCountIsReadByItsValueOrThePagesWord: the agent writes a status by the page's words,
// and chart-15 of the eval wrote "1 in progress", which was read as no count of in_progress. A
// status's count in the prose is read beside its value or beside the page's words for it, each
// whole: "Todo" and "In progress" give todo and in_progress, and "Done", the words of another
// status, "To do", which are not the page's, and "Todos" give no count of todo.
func TestAStatusCountIsReadByItsValueOrThePagesWord(t *testing.T) {
	tasks, err := LoadTasks(Files)
	if err != nil {
		t.Fatal(err)
	}
	chart := taskByID(t, tasks, "chart-15").Grade.Chart
	const others = "Triage: 1\nBacklog: 1\nDone: 2\n"
	if missing := MissingChartRows(chart.Rows, others+"Todo: 2\nIn progress: 1", nil); len(missing) != 0 {
		t.Errorf("the prose of each status's words and count misses %v; want none", missing)
	}
	if missing := MissingChartRows(chart.Rows, others+"todo: 2\nin_progress: 1", nil); len(missing) != 0 {
		t.Errorf("the prose of each status's value and count misses %v; want none", missing)
	}
	for _, todo := range []string{"Done: 2", "To do: 2", "Todos: 2"} {
		if missing := MissingChartRows(chart.Rows, others+todo+"\nIn progress: 1", nil); len(missing) != 1 || missing[0] != "todo=2" {
			t.Errorf("the prose that writes %q for todo misses %v; want todo=2", todo, missing)
		}
	}
}

// TestAnAnswerLinksAnItemByItsDashboardPath: an answer links a project's document, an issue's
// document and a spec by the dashboard's path to each, as the bar's answers write them, or by its
// dispatch:// reference; a link to another issue's spec or to another document is no link of it.
func TestAnAnswerLinksAnItemByItsDashboardPath(t *testing.T) {
	answer := "The runbook is [Certificate rotation runbook](/projects/OPS/documents/cert-rotation-runbook); " +
		"the log is [export-timeout-log](/issues/DEMO-16/artifacts/export-timeout-log); " +
		"the spec is [spec.md](/issues/DEMO-3/spec), and the guide [guide](dispatch://DEMO/artifact/board-style-guide)."
	links := []string{"OPS/artifact/cert-rotation-runbook", "DEMO-16/artifact/export-timeout-log", "DEMO-3/spec", "DEMO/artifact/board-style-guide"}
	if missing := MissingLinks(links, answer, nil); len(missing) != 0 {
		t.Errorf("the answer misses the links %v; want none", missing)
	}
	others := []string{"DEMO-31/spec", "OPS/artifact/backup-runbook"}
	if missing := MissingLinks(others, answer+" See also /issues/DEMO-3/spec.", nil); len(missing) != 2 {
		t.Errorf("the answer links %v of items it never names; want both missing", others)
	}
}

// TestASuggestionOnASpecIsGradedOnItsDocument: on the seeded store, a suggestion the person made
// on DEMO-3's spec, stored as the server stores it (a comment of DEMO-3 whose anchor names the
// spec), that puts the back-link before the title, passes document-01 ("suggest an edit of the
// spec so the back-link comes first"), which allows a suggestion on DEMO-3/spec and no other change.
func TestASuggestionOnASpecIsGradedOnItsDocument(t *testing.T) {
	ctx := context.Background()
	database, seed, tasks, before := seededStore(t)
	task := taskByID(t, tasks, "document-01")

	var spec string
	if err := database.Pool.QueryRow(ctx, `select id::text from artifacts where issue_key = 'DEMO-3' and slug = 'spec'`).Scan(&spec); err != nil {
		t.Fatal(err)
	}
	author, err := json.Marshal(map[string]any{"kind": "user", "id": evalPerson})
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := json.Marshal(map[string]any{"quote": "Back-links in issue specs", "artifact_id": spec, "version": 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (issue_key, author, body, anchor, suggestion)
		values ('DEMO-3', $1, 'The back-link comes first, before the title.', $2,
		  jsonb_build_object('replace_with', $3::text, 'accepted', null))`,
		author, anchor, "Part of DEMO-3. Parent: none.\n\n# Back-links in issue specs"); err != nil {
		t.Fatal(err)
	}
	after, err := ReadStore(ctx, database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	if verdict := GradeEnd(seed, task, before, after, evalPerson); !verdict.Pass() {
		t.Errorf("document-01 with a suggestion on DEMO-3's spec that puts the back-link first graded %+v; want a pass", verdict)
	}
}

// TestSuggestionsAreGradedWhereEachStands: a try's open suggestions read as the document would be
// if each were accepted on it as it stands, whatever order their comments' ids sort in. Two that
// swap two lines swap them; one that inserts a line and one that deletes the line's old copy move
// it; one whose quote matches only as plain words (an escape, a line break) replaces the text it
// stands for; and of two whose places overlap, the one that starts first is applied.
func TestSuggestionsAreGradedWhereEachStands(t *testing.T) {
	document := StoredDocument{ID: "spec", Text: "Intro.\n\nFirst line.\n\nSecond line.\n\nUse the \\_config\\_\nfile.\n\nPart of DEMO-3.\n"}
	type suggestion struct{ quote, with string }
	for _, test := range []struct {
		name        string
		suggestions [2]suggestion
		want        string
	}{
		{
			name:        "a swap of two lines",
			suggestions: [2]suggestion{{"First line.", "Second line."}, {"Second line.", "First line."}},
			want:        "Intro.\n\nSecond line.\n\nFirst line.\n\nUse the \\_config\\_\nfile.\n\nPart of DEMO-3.\n",
		},
		{
			name:        "an insert of a line before the delete of its old copy",
			suggestions: [2]suggestion{{"Intro.", "Part of DEMO-3.\n\nIntro."}, {"Part of DEMO-3.", ""}},
			want:        "Part of DEMO-3.\n\nIntro.\n\nFirst line.\n\nSecond line.\n\nUse the \\_config\\_\nfile.\n\n\n",
		},
		{
			name:        "a quote that matches as plain words beside one that matches as written",
			suggestions: [2]suggestion{{"Use the _config_ file.", "Use the settings file."}, {"Intro.", "Opening."}},
			want:        "Opening.\n\nFirst line.\n\nSecond line.\n\nUse the settings file.\n\nPart of DEMO-3.\n",
		},
		{
			name:        "two places that overlap",
			suggestions: [2]suggestion{{"line.", "row."}, {"First line.", "One."}},
			want:        "Intro.\n\nOne.\n\nSecond line.\n\nUse the \\_config\\_\nfile.\n\nPart of DEMO-3.\n",
		},
	} {
		for _, ids := range [][2]string{{"a", "b"}, {"b", "a"}} {
			t.Run(test.name+", ids "+ids[0]+" then "+ids[1], func(t *testing.T) {
				after := &Store{Comments: map[string]StoredComment{}}
				for index, s := range test.suggestions {
					after.Comments[ids[index]] = StoredComment{
						ID: ids[index], Artifact: document.ID, Quote: s.quote, Suggestion: &Suggested{ReplaceWith: s.with},
					}
				}
				text, open := withSuggestions(after, document, map[string]bool{"a": true, "b": true})
				if open != 2 || text != test.want {
					t.Errorf("withSuggestions = %q, %d open; want %q, 2 open", text, open, test.want)
				}
			})
		}
	}
}

// document01Suggestions is the three suggestions of DEMO-3's spec that document-01 of the eval
// made to move its back-link first (at 584ef0d2, try 3), as the server stores them: the title's
// text "Back-links in issue specs" with the back-link written under it, the heading
// "## Back-link", whose stored quote is its text "Back-link", cleared to "##", and the line under
// that heading cleared.
var document01Suggestions = [3]struct{ quote, with string }{
	{"Back-link", "##"},
	{"Back-links in issue specs", "# Back-links in issue specs\n\n## Back-link\n\nPart of DEMO-3. Parent: none."},
	{"Part of DEMO-3. Parent: none.", ""},
}

// gradeDocument01 is DEMO-3's spec, doc, with document01Suggestions applied as withSuggestions
// reads them, each made by the comment of the id ids gives it and anchored to the block blockOf
// names for its quote ("" for none), and whether it holds the back-link before the summary, as
// document-01's end state asks.
func gradeDocument01(t *testing.T, doc StoredDocument, ids [3]string, blockOf func(text string) string) (string, bool) {
	t.Helper()
	after := &Store{Comments: map[string]StoredComment{}}
	fresh := map[string]bool{}
	for index, s := range document01Suggestions {
		after.Comments[ids[index]] = StoredComment{ID: ids[index], Artifact: doc.ID, Quote: s.quote, Block: blockOf(s.quote), Suggestion: &Suggested{ReplaceWith: s.with}}
		fresh[ids[index]] = true
	}
	text, open := withSuggestions(after, doc, fresh)
	if open != 3 {
		t.Errorf("withSuggestions read %d open suggestions; want 3", open)
	}
	return text, textHolds(text, []string{"Part of DEMO-3", "Summary"}, nil, true)
}

// TestASuggestionInsideAWordOrTiedIsGradedTheSameWhateverItsID: document-01's three suggestions,
// with no block known. Placed at its quote's first place, the heading's stood inside the title's
// word "Back-links", where the title's suggestion stood too, and the comments' ids, which are
// random, chose which of the two was applied: the try failed where the heading's sorted first. A
// quote stands at its first place on word bounds, so each stands where it was made, and both
// orders of the ids give the one spec, with the back-link first.
func TestASuggestionInsideAWordOrTiedIsGradedTheSameWhateverItsID(t *testing.T) {
	seed, err := LoadSeed(Files)
	if err != nil {
		t.Fatal(err)
	}
	issue, _ := seed.Issue("DEMO-3")
	doc := StoredDocument{ID: "spec", Text: issue.Spec}
	noBlock := func(string) string { return "" }
	headingFirst, passed := gradeDocument01(t, doc, [3]string{"a", "b", "c"}, noBlock)
	if !passed {
		t.Errorf("with the heading's id first, the spec reads %q; want the back-link before the summary", headingFirst)
	}
	titleFirst, passed := gradeDocument01(t, doc, [3]string{"c", "b", "a"}, noBlock)
	if !passed || titleFirst != headingFirst {
		t.Errorf("with the title's id first, the spec reads %q; want %q, as with the heading's first", titleFirst, headingFirst)
	}
}

// TestASuggestionStandsOnTheBlockItWasMadeOn: on the seeded store, a suggestion whose anchor names
// a block of DEMO-3's spec stands in that block, as the bar's Apply replaces the text of its mark
// there (internal/dispatch/docs/marks.go, applySuggestion). "issue" stands on word bounds in the
// title and in the summary: anchored to the summary, it replaces the summary's, and with no block,
// the title's. document-01's three suggestions, each anchored to its block, give one spec with
// the back-link first whatever order their ids sort in.
func TestASuggestionStandsOnTheBlockItWasMadeOn(t *testing.T) {
	_, _, _, before := seededStore(t)
	var spec StoredDocument
	for _, d := range before.Documents {
		if d.Issue == "DEMO-3" && d.Slug == "spec" {
			spec = d
		}
	}
	// blockOf is the id the index holds for the least block of the spec whose text holds text.
	blockOf := func(text string) string {
		t.Helper()
		least := ""
		size := len(spec.Text) + 1
		for _, offset := range blockPlaces(spec.Text) {
			body := spec.Text[offset.From:offset.To]
			if !strings.Contains(body, text) || len(body) >= size {
				continue
			}
			for id, block := range spec.Blocks {
				if block.Type == offset.Type && block.Sum == refs.BlockTextSum(body) {
					least, size = id, len(body)
				}
			}
		}
		if least == "" {
			t.Fatalf("the index of DEMO-3's spec holds no block whose text holds %q; it holds %v", text, spec.Blocks)
		}
		return least
	}

	for _, row := range []struct{ name, block, holds, lacks string }{
		{"anchored to the summary", blockOf("so a reader who opens a spec"), "a link back to the ticket it belongs to", "Back-links in ticket specs"},
		{"with no block", "", "Back-links in ticket specs", "a link back to the ticket it belongs to"},
		{"anchored to a block the index does not hold", "no-such-block", "Back-links in ticket specs", "a link back to the ticket it belongs to"},
	} {
		after := &Store{Comments: map[string]StoredComment{"a": {ID: "a", Artifact: spec.ID, Quote: "issue", Block: row.block, Suggestion: &Suggested{ReplaceWith: "ticket"}}}}
		text, _ := withSuggestions(after, spec, map[string]bool{"a": true})
		if !strings.Contains(text, row.holds) || strings.Contains(text, row.lacks) {
			t.Errorf("the suggestion of issue %s reads %q; want it to hold %q and not %q", row.name, text, row.holds, row.lacks)
		}
	}

	headingFirst, passed := gradeDocument01(t, spec, [3]string{"a", "b", "c"}, blockOf)
	if !passed {
		t.Errorf("with the heading's id first, the spec reads %q; want the back-link before the summary", headingFirst)
	}
	if titleFirst, passed := gradeDocument01(t, spec, [3]string{"c", "b", "a"}, blockOf); !passed || titleFirst != headingFirst {
		t.Errorf("with the title's id first, the spec reads %q; want %q, as with the heading's first", titleFirst, headingFirst)
	}
}

// TestPlainSpansReadsTextAsPlainDoes: plainSpans' text is plain's, and each byte of it stands for
// the bytes of the text it was read from.
func TestPlainSpansReadsTextAsPlainDoes(t *testing.T) {
	for _, text := range []string{
		"", "  \n\t ", "  Use the \\_config\\_\n\n file.  ", "\\\\_x", "a\\ b\\", "x\u00a0y\u2028z", "caf\u00e9 \xff end", "\\*bold\\* and \\é",
	} {
		flat, spans := plainSpans(text)
		if flat != plain(text) || len(spans) != len(flat) {
			t.Fatalf("plainSpans(%q) = %q with %d spans; want plain's %q with a span a byte", text, flat, len(spans), plain(text))
		}
		for index, span := range spans {
			read := text[span.from:span.to]
			if flat[index] == ' ' {
				if strings.TrimSpace(read) != "" {
					t.Fatalf("plainSpans(%q): the space at %d stands for %q; want white space", text, index, read)
				}
			} else if read[len(read)-1] != flat[index] {
				t.Fatalf("plainSpans(%q): byte %d (%q) stands for %q", text, index, flat[index], read)
			}
		}
	}
}
