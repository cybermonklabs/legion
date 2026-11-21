package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

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
// same chart with one week's count wrong misses that row.
func TestAChartShowsItsRowsByTheWeeksItDraws(t *testing.T) {
	tasks, err := LoadTasks(Files)
	if err != nil {
		t.Fatal(err)
	}
	chart := taskByID(t, tasks, "chart-01").Grade.Chart
	drawn := func(counts ...int) []json.RawMessage {
		weeks := []string{"2026-09-28", "2026-10-05", "2026-10-12", "2026-10-19", "2026-10-26", "2026-11-02", "2026-11-09", "2026-11-16"}
		rows := make([]map[string]any, len(weeks))
		for i, week := range weeks {
			rows[i] = map[string]any{"week": week, "closed": counts[i]}
		}
		block, err := json.Marshal(map[string]any{"kind": "chart", "title": "Issues closed each week in DEMO", "rows": rows})
		if err != nil {
			t.Fatal(err)
		}
		return []json.RawMessage{block}
	}
	if missing := MissingChartRows(chart.Rows, "", drawn(2, 2, 1, 2, 2, 2, 2, 1)); len(missing) != 0 {
		t.Errorf("the drawn chart of every week's count misses %v; want none", missing)
	}
	if missing := MissingChartRows(chart.Rows, "", drawn(2, 2, 1, 2, 2, 2, 3, 1)); len(missing) != 1 || missing[0] != "2026-W46=2" {
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
		block, err := json.Marshal(map[string]any{"kind": "chart", "title": "Open bugs in DEMO by priority", "rows": drawnRows})
		if err != nil {
			t.Fatal(err)
		}
		return []json.RawMessage{block}
	}
	if missing := MissingChartRows(chart.Rows, "", drawn("priority", [2]int{1, 5}, [2]int{2, 2}, [2]int{3, 1})); len(missing) != 0 {
		t.Errorf("the drawn chart of each priority's count misses %v; want none", missing)
	}
	if missing := MissingChartRows(chart.Rows, "", drawn("priority", [2]int{1, 5}, [2]int{2, 3}, [2]int{3, 1})); len(missing) != 1 || missing[0] != "P2=2" {
		t.Errorf("the drawn chart with 3 bugs at P2 misses %v; want P2=2", missing)
	}
	if missing := MissingChartRows(chart.Rows, "", drawn("estimate", [2]int{1, 5}, [2]int{2, 2}, [2]int{3, 1})); len(missing) != 3 {
		t.Errorf("the drawn chart by estimate misses %v; want every row, since no field of it is the priority", missing)
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
