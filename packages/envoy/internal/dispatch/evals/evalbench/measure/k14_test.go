package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/evals/evalbench"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
)

// k14Base is the moment every k14 test's times are counted from.
var k14Base = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

// at is k14Base plus ms milliseconds.
func at(ms int) time.Time { return k14Base.Add(time.Duration(ms) * time.Millisecond) }

// heldPage is a page that has held the values of id at the moments given, and shows items now.
func heldPage(name, id string, items map[string]string, values ...pageItem) *livePage {
	return &livePage{name: name, items: items, history: map[string][]pageItem{id: values}}
}

// measuredWrite is a write committed at 100 ms, whose pages first showed its value at the times
// reached gives (ms from the commit), each showing every item a fresh read answers.
func measuredWrite(reached ...int) k14Measured {
	m := k14Measured{write: k14Write{name: "source-move", kind: kindSourceMove, project: evalbench.PairProject, watched: "PAIR/deploy-frequency"},
		status: 200, sent: at(0), answered: at(150), committed: new(at(100)), settled: at(4000)}
	for index, ms := range reached {
		m.reaches = append(m.reaches, pageReach{page: fmt.Sprintf("page-%d", index), reached: new(at(100 + ms)), updates: 1})
	}
	m.transactions = []transaction{{pid: 7, began: at(80), end: at(100), ended: "commit", statements: 3, writes: []string{"project_deploy_sources"}}}
	m.kinds = []string{txRoute}
	m.longest, m.longestKind = &m.transactions[0], txRoute
	return m
}

// TestK14AWriteIsTimedFromItsCommitToTheLastPagesUpdate: judgeWrite holds a write to R14's 2 s from
// its commit, read from the statement log, to the update of the last of its open pages, at both
// rates; it fails a page that never showed the value a fresh read answers once the write settled, a
// page that shows another item than the fresh read (a wrong delta), a write that moved no value the
// pages show, and a write whose commit the log does not hold. Each row breaks one rule and is
// judged one FAIL line naming it; a write within its bound is judged none, and counted.
func TestK14AWriteIsTimedFromItsCommitToTheLastPagesUpdate(t *testing.T) {
	t.Parallel()
	for _, tenTimes := range []bool{false, true} {
		judged := &report{prefix: "RESULT", tenTimes: tenTimes}
		if !judgeWrite(judged, measuredWrite(300, 1999), "stamp") || len(judged.fails) != 0 {
			t.Fatalf("ten times %t: a write whose last page came 1999 ms after its commit was judged %q; want it measured and no failure", tenTimes, judged.fails)
		}
		if last, whole := measuredWrite(300, 1999).lastPageMs(); !whole || last != 1999 {
			t.Fatalf("the last page's update came %v ms (whole %t) after the commit; want 1999", last, whole)
		}
		for _, row := range []struct {
			name   string
			breaks func(*k14Measured)
			names  string
		}{
			{"a last page past 2 s", func(m *k14Measured) { m.reaches[1].reached = new(at(100 + 2001)) }, "2001.0 ms after the write's commit, over R14's 2000 ms"},
			{"a page that never showed the value", func(m *k14Measured) { m.reaches[0].reached = nil }, "page=page-0: the page never came to show"},
			{"a wrong delta", func(m *k14Measured) { m.reaches[1].differing = []string{"PAIR/pr-ci"} }, "a wrong delta: the page shows items other than a fresh read answers: PAIR/pr-ci"},
			{"a write that moved nothing", func(m *k14Measured) { m.unmoved = []string{"page-0"} }, "moved no value of PAIR/deploy-frequency on page page-0"},
			{"no commit in the log", func(m *k14Measured) { m.committed = nil }, "holds no commit of the write's statement"},
		} {
			m := measuredWrite(300, 1999)
			row.breaks(&m)
			judged := &report{prefix: "RESULT", tenTimes: tenTimes}
			counted := judgeWrite(judged, m, "stamp")
			if counted || len(judged.fails) != 1 || !strings.Contains(judged.fails[0], row.names) || len(judged.records) != 0 {
				t.Errorf("ten times %t, %s: counted %t, fails %q, records %q; want one FAIL naming %q and the write not counted", tenTimes, row.name, counted,
					judged.fails, judged.records, row.names)
			}
		}
	}
}

// TestK14APageReachesAWriteWhenItShowsTheFreshReadsValueToTheEnd: reachOf reads a page's update
// off its own frames: the first frame after the write from which on it showed the value the fresh
// read answers, however many frames came before; none where it showed the value and then another,
// or never; and every item it shows that the fresh read does not answer alike.
func TestK14APageReachesAWriteWhenItShowsTheFreshReadsValueToTheEnd(t *testing.T) {
	t.Parallel()
	id, want := "PAIR/deploy-frequency", `{"id":"PAIR/deploy-frequency","v":2}`
	fresh := map[string]string{id: want, "PAIR/pr-ci": `{"id":"PAIR/pr-ci"}`}
	shown := map[string]string{id: want, "PAIR/pr-ci": `{"id":"PAIR/pr-ci"}`}
	old := pageItem{value: `{"id":"PAIR/deploy-frequency","v":1}`, at: at(-5000)}
	for _, row := range []struct {
		name      string
		values    []pageItem
		shown     map[string]string
		reached   *time.Time
		updates   int
		differing []string
	}{
		{"one update", []pageItem{old, {value: want, at: at(700)}}, shown, new(at(700)), 1, nil},
		{"an update the page then corrected", []pageItem{old, {value: `{"id":"PAIR/deploy-frequency","v":9}`, at: at(300)}, {value: want, at: at(900)}},
			shown, new(at(900)), 2, nil},
		{"the value, then another", []pageItem{old, {value: want, at: at(300)}, {value: `{"v":3}`, at: at(900)}}, shown, nil, 2, nil},
		{"no update", []pageItem{old}, shown, nil, 0, nil},
		{"another item than the fresh read's", []pageItem{old, {value: want, at: at(700)}}, map[string]string{id: want, "PAIR/pr-ci": `{"id":"PAIR/pr-ci","x":1}`, "PAIR/gone": "{}"},
			new(at(700)), 1, []string{"PAIR/gone", "PAIR/pr-ci"}},
	} {
		reach := reachOf(heldPage("page", id, row.shown, row.values...), id, at(0), want, fresh)
		if (reach.reached == nil) != (row.reached == nil) || reach.reached != nil && !reach.reached.Equal(*row.reached) || reach.updates != row.updates ||
			!slices.Equal(reach.differing, row.differing) {
			t.Errorf("%s: reached %v, %d updates, differing %q; want %v, %d, %q", row.name, reach.reached, reach.updates, reach.differing, row.reached, row.updates, row.differing)
		}
	}
}

// TestK14FewerThanThreeWritesFail: k14 fails a run that measured fewer than three writes whole
// within their bound, and one that measured no write of one of the three kinds, at any rate.
func TestK14FewerThanThreeWritesFail(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		measured map[string]int
		fails    []string
	}{
		{map[string]int{kindSourceMove: 1, kindDefaultMove: 1, kindRuleChange: 1}, nil},
		{map[string]int{kindSourceMove: 2, kindDefaultMove: 2, kindRuleChange: 2}, nil},
		{map[string]int{kindSourceMove: 1, kindDefaultMove: 1}, []string{"no rule-change write", "2 writes were measured whole within their bound, fewer than 3"}},
		{map[string]int{kindSourceMove: 2, kindDefaultMove: 2}, []string{"no rule-change write"}},
		{map[string]int{}, []string{"no source-move write", "no default-move write", "no rule-change write", "0 writes"}},
	} {
		judged := &report{prefix: "RESULT", tenTimes: true}
		judgeWriteCount(judged, row.measured)
		if len(judged.fails) != len(row.fails) {
			t.Errorf("%v: fails %q; want %d naming %q", row.measured, judged.fails, len(row.fails), row.fails)
			continue
		}
		for index, want := range row.fails {
			if !strings.Contains(judged.fails[index], want) {
				t.Errorf("%v: fail %q; want it to name %q", row.measured, judged.fails[index], want)
			}
		}
	}
}

// jsonLine is one line of Postgres's jsonlog: a statement that ended at ms after k14Base on
// backend pid of application app, after duration milliseconds, with its parameters' detail.
func jsonLine(ms int, pid int, app, kind, text string, duration float64, detail string) string {
	stamp := at(ms).Format("2006-01-02 15:04:05.000 MST")
	appField := ""
	if app != "" {
		appField = fmt.Sprintf(`"application_name":%q,`, app)
	}
	return fmt.Sprintf(`{"timestamp":%q,"pid":%d,%s"message":%q,"detail":%q,"query_id":1}`, stamp, pid, appField,
		fmt.Sprintf("duration: %.3f ms  %s: %s", duration, kind, text), detail)
}

// logOf parses raw jsonlog lines as readLogLines reads them.
func logOf(t *testing.T, raw ...string) []logLine {
	t.Helper()
	var lines []logLine
	for _, one := range raw {
		line, ok := parseLogLine([]byte(one))
		if !ok {
			t.Fatalf("parseLogLine refused %s", one)
		}
		lines = append(lines, line)
	}
	return lines
}

// TestK14TheStatementLogGivesTheWriteItsCommitAndEachTransactionItsSpan: transactionsOf reads each
// backend's transactions from the statement log, from its begin's start to its commit's end, a
// statement outside a transaction as a transaction of its own; writeOf finds the write's own by its
// statement, and kindOf tells the refresher's, the production signals', the change log's drain,
// reads and other background work apart; the instrument's own connections are none of them.
func TestK14TheStatementLogGivesTheWriteItsCommitAndEachTransactionItsSpan(t *testing.T) {
	t.Parallel()
	lines := logOf(t,
		jsonLine(10, 101, "", "statement", "begin", 0.2, ""),
		jsonLine(12, 101, "", "execute stmtcache_1", "select key from projects where key = $1 for update", 1, "Parameters: $1 = 'PAIR'"),
		jsonLine(30, 101, "", "execute stmtcache_2", "insert into project_delivery_settings (project_key) values ($1) on conflict (project_key) do update set delivery = true", 2, "Parameters: $1 = 'PAIR'"),
		jsonLine(40, 102, "", "execute <unnamed>", "select 1", 0.5, ""),
		jsonLine(45, 101, "", "statement", "commit", 4.5, ""),
		jsonLine(50, 103, measureApp, "statement", "begin", 0.1, ""),
		jsonLine(60, 103, measureApp, "execute <unnamed>", "delete from eval_days where false", 1, ""),
		jsonLine(61, 103, measureApp, "statement", "commit", 0.1, ""),
		jsonLine(70, 104, "", "statement", "begin", 0.1, ""),
		jsonLine(80, 104, "", "execute <unnamed>", "insert into eval_figure_cuts_stale (project_key) values ($1)", 1, "Parameters: $1 = 'PAIR'"),
		jsonLine(700, 104, "", "statement", "commit", 1, ""),
		jsonLine(720, 105, "", "execute <unnamed>", "insert into production_events (id) values ($1)", 3, "Parameters: $1 = 'x'"),
		jsonLine(730, 106, "", "execute <unnamed>", "with moved as (delete from delivery_changes_pending returning *) insert into delivery_changes select * from moved", 2, ""),
		jsonLine(740, 107, "", "execute <unnamed>", "update issue_facts set x = 1", 2, ""),
	)
	transactions := transactionsOf(lines, markWrite(settingsStatement))
	own, found := writeOf(transactions, at(0), at(50))
	if !found || own.pid != 101 || own.ended != "commit" || !own.began.Equal(at(10).Add(-200*time.Microsecond)) || !own.end.Equal(at(45)) || own.statements != 2 {
		t.Fatalf("the write's transaction is %+v (found %t); want pid 101 from its begin's start to its commit's end at 45 ms, two statements", own, found)
	}
	if !slices.Equal(own.writes, []string{"project_delivery_settings"}) {
		t.Errorf("the write's transaction writes %q; want project_delivery_settings alone (a lock and a conflict clause write no table)", own.writes)
	}
	kinds := map[int64]string{}
	for _, one := range transactions {
		if one.app == measureApp {
			continue
		}
		kinds[one.pid] = kindOf(one, own)
	}
	want := map[int64]string{101: txRoute, 102: txReads, 104: txRefresher, 105: txSignals, 106: txChanges, 107: txOther}
	for pid, kind := range want {
		if kinds[pid] != kind {
			t.Errorf("pid %d's transaction is %q; want %q", pid, kinds[pid], kind)
		}
	}
	if _, held := kinds[103]; held || len(kinds) != len(want) {
		t.Errorf("the kinds read are %v; want %v and the instrument's own transaction left out", kinds, want)
	}
	for _, kind := range []string{txRoute, txRefresher, txSignals, txChanges} {
		if !startedBy(kind) {
			t.Errorf("a %s transaction is not one a write starts", kind)
		}
	}
	if startedBy(txReads) || startedBy(txOther) {
		t.Error("a read or the server's other background work is counted as a transaction the write starts")
	}
	if _, found := writeOf(transactionsOf(lines, markWrite(defaultStatement)), at(0), at(50)); found {
		t.Error("writeOf found a default-repository write in a log that holds none")
	}
}

// TestK14AReconcilesUpdateOfASourceBeforeThePutIsNotTheWrite: the reconcile records a source's
// bookkeeping in one statement outside a transaction (delivery.RecordReconcileSuccess), an update
// of project_deploy_sources that settingsStatement matches as it matches the settings route's own
// writes. One that starts in the window before the PUT's transaction is not the write: the route
// writes in a transaction of its own begin, so writeOf skips a statement whose end is implicit and
// finds the PUT's, with its commit. Red where writeOf takes the first match: the reconcile's
// statement, which holds no commit.
func TestK14AReconcilesUpdateOfASourceBeforeThePutIsNotTheWrite(t *testing.T) {
	t.Parallel()
	lines := logOf(t,
		jsonLine(5, 201, "", "execute <unnamed>", "update project_deploy_sources src set last_reconcile_at = $2, last_error = null where lower(src.repo) = lower($1)", 1,
			"Parameters: $1 = 'acme/pair', $2 = '2026-11-13 00:00:00+00'"),
		jsonLine(10, 101, "", "statement", "begin", 0.2, ""),
		jsonLine(30, 101, "", "execute stmtcache_2", "update project_deploy_sources set deploy_workflow_path = $2 where id = $1", 2, "Parameters: $1 = 'x', $2 = 'y'"),
		jsonLine(45, 101, "", "statement", "commit", 4.5, ""),
	)
	own, found := writeOf(transactionsOf(lines, markWrite(settingsStatement)), at(0), at(50))
	if !found || own.pid != 101 || own.ended != "commit" || !own.end.Equal(at(45)) {
		t.Fatalf("the write's transaction is %+v (found %t); want the PUT's, pid 101, committed at 45 ms", own, found)
	}
}

// TestK14TheDailyFailureWriteOfEachProjectIsRead: dailyWrites finds each project's whole write of
// its failure rows in the statement log by the statement only that write runs, under the project
// its first parameter names, and judgeDaily prints each with the longest, the bound on what an owed
// change waits, and fails a k14 project with none.
func TestK14TheDailyFailureWriteOfEachProjectIsRead(t *testing.T) {
	t.Parallel()
	whole := "delete from eval_days where project_key = $1 and eval_id = any($2) and writer = $3 and (day < $4::date or day > $5::date) returning eval_id, day"
	write := func(pid, from int, project string, ms int) []string {
		return []string{
			jsonLine(from, pid, "", "statement", "begin", 0.1, ""),
			jsonLine(from+5, pid, "", "execute stmtcache_9", whole, 3, fmt.Sprintf("Parameters: $1 = '%s', $2 = '{%s/change-fail-rate}', $3 = 'computation'", project, project)),
			jsonLine(from+ms, pid, "", "statement", "commit", 1, ""),
		}
	}
	lines := logOf(t, slices.Concat(write(201, 0, evalbench.PairProject, 400), write(202, 500, evalbench.MixedProject, 900))...)
	writes := dailyWrites(transactionsOf(lines, markWholeFailureWrite))
	if len(writes) != 2 || writes[0].project != evalbench.PairProject || writes[1].project != evalbench.MixedProject {
		t.Fatalf("the daily writes read are %+v; want PAIR's and then MIXED's", writes)
	}
	judged := &report{prefix: "RESULT"}
	judgeDaily(judged, writes, []map[string]string{{"daily_ms": "1400", "yielded": "false"}}, "stamp")
	if len(judged.fails) != 0 {
		t.Fatalf("both projects' daily writes were judged %q; want no failure", judged.fails)
	}
	judged = &report{prefix: "RESULT"}
	judgeDaily(judged, writes[:1], nil, "stamp")
	if len(judged.fails) != 1 || !strings.Contains(judged.fails[0], "no whole write of MIXED's failure rows") {
		t.Errorf("a run with PAIR's daily write alone was judged %q; want one FAIL naming MIXED", judged.fails)
	}
}

// TestK14TheFirstPaintsBudgetsBindAtBothRates: k14's first paint is the list read at 84 days with a
// compare range and at 28 days with none, each the first read after a restart in one of two runs,
// each held to the first paint's server budget and its database budget (150 and 60 ms), which bind
// at ten times the stated rate too: there the read over its budget prints a FAIL line, not the
// RECORD line a head paint's read prints.
func TestK14TheFirstPaintsBudgetsBindAtBothRates(t *testing.T) {
	t.Parallel()
	today := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	for run, first := range map[int]string{1: "list-84d-compare", 2: "list-28d", 3: "list-84d-compare"} {
		cases := k14Cases(evalbench.PairProject, today, run)
		if len(cases) != 2 || cases[0].name != first {
			t.Fatalf("run %d's k14 paint reads %v first of %d; want %s first of two", run, cases[0].name, len(cases), first)
		}
		for _, one := range cases {
			budget := map[string]float64{"list-84d-compare": budget84Ms, "list-28d": budget28Ms}[one.name]
			if !one.strict || !one.storedFigures || one.budgetMs != budget || one.serverBudgetMs != budgetServerMs ||
				!strings.HasPrefix(one.path, "/api/v1/projects/PAIR/evals?") || strings.Contains(one.path, "repo=") {
				t.Errorf("run %d's %s is %+v; want a strict read of PAIR's own cut under %v ms and the server's %d ms", run, one.name, one, budget, budgetServerMs)
			}
		}
	}
	over := withinEveryBudget()
	over.paintCase = k14Cases(evalbench.PairProject, today, 1)[1]
	over.own[0].execMs = 61
	for _, tenTimes := range []bool{false, true} {
		judged := &report{prefix: "RESULT", tenTimes: tenTimes}
		judgePaint(judged, options{mode: "k14"}, []measuredCase{over})
		if len(judged.fails) != 1 || !strings.Contains(judged.fails[0], "db_ms=62.00 is over its budget of 60 ms, which binds at both rates") || len(judged.records) != 0 {
			t.Errorf("ten times %t: a k14 read 2 ms over its budget was judged fails %q, records %q; want one FAIL and no RECORD", tenTimes, judged.fails, judged.records)
		}
	}
}

// k14Paint is a k14 project's 84-day read that read both stored-figures statements, its range read
// serving served, and answered body.
func k14Paint(served, body string) measuredCase {
	read := listRead(map[int64]string{1: rangeFiguresRead, 2: dayFiguresRead}, map[int64][]string{
		1: {"'{PAIR}'", "''", "'{" + served + "}'", "'{PAIR/deploy-frequency}'", "'{PAIR}'", "84", "'2026-11-13'", "84", "'2026-08-21'"},
		2: {"'{PAIR}'", "''", "'2026-08-21'", "'2026-11-13'", "'{" + served + "}'", "'{PAIR/deploy-frequency}'", "'{PAIR}'"},
	})
	read.status, read.body = 200, []byte(body)
	return read
}

// TestK14AFirstPaintThatFoldedTheDayRowsOrAnsweredOtherNumbersFails: a k14 first paint fails where
// its range figures read did not serve the project's deploy-frequency (the deploy evals folded from
// the day rows, as the code before the union cut folded a project with two deploy sources on each
// read), where it answers no value of deploy-frequency over the window, and where its items are not
// the warm read's from before the restart; one that read its union cut and answered as the warm read
// did fails none.
func TestK14AFirstPaintThatFoldedTheDayRowsOrAnsweredOtherNumbersFails(t *testing.T) {
	t.Parallel()
	answer := `{"items":[{"id":"PAIR/deploy-frequency","series":[{"series":"deploys","window":{"count":41}}]},{"id":"PAIR/pr-ci","series":[]}]}`
	items, err := itemsOf([]byte(answer))
	if err != nil {
		t.Fatal(err)
	}
	warm := []k14Expected{{Project: evalbench.PairProject, Case: "list-84d-compare", Items: items}}
	if problems := k14PaintProblems(k14Paint("PAIR/deploy-frequency,PAIR/pr-ci", answer), warm, evalbench.PairProject, "PAIR/deploy-frequency"); len(problems) != 0 {
		t.Fatalf("a first paint that read its union cut and answered as the warm read did breaks %q; want none", problems)
	}
	for _, row := range []struct {
		name  string
		read  measuredCase
		warm  []k14Expected
		names string
	}{
		{"the deploy evals folded from the day rows", k14Paint("PAIR/pr-ci", answer), warm, "served no PAIR/deploy-frequency"},
		{"no stored-figures read", listRead(map[int64]string{1: dayRowsUnion}, map[int64][]string{1: {}}), warm, "did not run both stored-figures reads"},
		{"no value over the window", k14Paint("PAIR/deploy-frequency", `{"items":[{"id":"PAIR/deploy-frequency","series":[{"series":"deploys","window":{"count":null}}]},{"id":"PAIR/pr-ci","series":[]}]}`),
			nil, "no value of PAIR/deploy-frequency over the window"},
		{"other numbers than the warm read's", k14Paint("PAIR/deploy-frequency", strings.Replace(answer, "41", "40", 1)), warm, "differ from the warm read's before the restart: PAIR/deploy-frequency"},
		{"no warm read of it", k14Paint("PAIR/deploy-frequency", answer), []k14Expected{{Project: evalbench.MixedProject, Case: "list-84d-compare", Items: items}}, "the warm reads hold none of it"},
	} {
		problems := k14PaintProblems(row.read, row.warm, evalbench.PairProject, "PAIR/deploy-frequency")
		if !slices.ContainsFunc(problems, func(problem string) bool { return strings.Contains(problem, row.names) }) {
			t.Errorf("%s: problems %q; want one naming %q", row.name, problems, row.names)
		}
	}
}

// TestK14BothProjectsFirstPaintsAreMeasured: k14-paints fails where fewer than both k14 projects
// had each of their two reads measured as the first read after a restart.
func TestK14BothProjectsFirstPaintsAreMeasured(t *testing.T) {
	t.Parallel()
	runs := func(project string) []k14PaintRecord {
		return []k14PaintRecord{
			{Project: project, Run: 1, Reads: []k14PaintRead{{Name: "list-84d-compare", First: true, Answered: true}, {Name: "list-28d", Answered: true}}},
			{Project: project, Run: 2, Reads: []k14PaintRead{{Name: "list-28d", First: true, Answered: true}, {Name: "list-84d-compare", Answered: true}}},
		}
	}
	judged := &report{prefix: "RESULT"}
	judgeK14Paints(judged, slices.Concat(runs(evalbench.PairProject), runs(evalbench.MixedProject)))
	if len(judged.fails) != 0 {
		t.Fatalf("both projects' two runs were judged %q; want no failure", judged.fails)
	}
	judged = &report{prefix: "RESULT"}
	judgeK14Paints(judged, slices.Concat(runs(evalbench.PairProject), runs(evalbench.MixedProject)[:1]))
	if len(judged.fails) != 2 || !strings.Contains(judged.fails[0], "project=MIXED case=list-28d") || !strings.Contains(judged.fails[1], "fewer than two") {
		t.Errorf("a run in which MIXED's 28-day read was never first was judged %q; want it and the count failed", judged.fails)
	}
}

// TestK14TheGitHubStandInAnswersTheSourceCheck: the stand-in answers the App's source check of a
// repository of an owner it is installed on (githubapp.Client.CheckSource, the settings route's), a
// repository of another owner as GitHub answers one the App is not installed on, and an App whose
// key is not the one it made with no installation at all; it grants no Actions read, so the
// delivery reconcile's permission check refuses every repository and the reconcile reads no run.
func TestK14TheGitHubStandInAnswersTheSourceCheck(t *testing.T) {
	t.Parallel()
	key, encoded, err := newGitHubKey()
	if err != nil {
		t.Fatal(err)
	}
	fake := &gitHubFake{public: &key.PublicKey, clientID: "Iv1.evalbench", owners: []string{"acme"}, now: time.Now, log: log.New(io.Discard, "", 0),
		minted: map[string]time.Time{}}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	client, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.evalbench", PEM: encoded}, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := client.CheckSource(ctx, "acme", "pair-spare"); err != nil {
		t.Errorf("the source check of acme/pair-spare failed: %v; want it answered", err)
	}
	if _, err := client.CheckSource(ctx, "other", "repo"); !errors.Is(err, githubapp.ErrNoInstallation) {
		t.Errorf("the source check of other/repo answered %v; want ErrNoInstallation", err)
	}
	if _, err := client.DeliveryPermissions(ctx, "acme", "pair-spare"); !errors.Is(err, githubapp.ErrNoActionsRead) {
		t.Errorf("the reconcile's permission check answered %v; want ErrNoActionsRead, so the reconcile reads no run", err)
	}
	_, other, err := newGitHubKey()
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.evalbench", PEM: other}, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stranger.CheckSource(ctx, "acme", "pair-spare"); err == nil {
		t.Error("an App whose key the stand-in did not make passed the source check")
	}
}

// TestK14APageShowsItsSnapshotWithEveryDeltaApplied: an open page subscribes its evals view over
// /ws/live, holds the snapshot's items by id, applies each delta's upserts and removals as its frame
// arrives, and ignores the frames of other subscriptions.
func TestK14APageShowsItsSnapshotWithEveryDeltaApplied(t *testing.T) {
	t.Parallel()
	upgrader := websocket.Upgrader{}
	subscribed := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var frame map[string]any
		if conn.ReadJSON(&frame) != nil {
			return
		}
		subscribed <- frame
		sub := frame["sub"].(string)
		for _, out := range []string{
			`{"type":"snapshot","sub":"` + sub + `","body":{"items":[{"id":"PAIR/a","v":1},{"id":"PAIR/b","v":1}]}}`,
			`{"type":"delta","sub":"other","items":{"items":{"upsert":[{"id":"PAIR/a","v":7}],"remove":[]}}}`,
			`{"type":"delta","sub":"` + sub + `","items":{"items":{"upsert":[{"id":"PAIR/a","v":2},{"id":"PAIR/c","v":1}],"remove":["PAIR/b"]}}}`,
		} {
			if conn.WriteMessage(websocket.TextMessage, []byte(out)) != nil {
				return
			}
		}
		_, _, _ = conn.ReadMessage()
	}))
	t.Cleanup(server.Close)
	today := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	page, err := openPage(context.Background(), server.URL, "token", "PAIR-28d", evalbench.PairProject, before(today, 28), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer page.close()
	frame := <-subscribed
	view, _ := frame["view"].(map[string]any)
	if frame["type"] != "subscribe" || view["kind"] != "evals" || view["project"] != "PAIR" || view["query"] != "limit=200" {
		t.Errorf("the page subscribed %v; want the evals view of PAIR with the list's page and no compare range", frame)
	}
	deadline := time.Now().Add(5 * time.Second)
	for page.value("PAIR/c") == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	want := map[string]string{"PAIR/a": `{"id":"PAIR/a","v":2}`, "PAIR/c": `{"id":"PAIR/c","v":1}`}
	if shown := page.shown(); len(shown) != len(want) || shown["PAIR/a"] != want["PAIR/a"] || shown["PAIR/c"] != want["PAIR/c"] {
		t.Errorf("the page shows %v; want %v", shown, want)
	}
	if changes := page.changesOf("PAIR/b", time.Time{}); len(changes) != 2 || changes[1].value != "" {
		t.Errorf("the page held PAIR/b as %+v; want its snapshot's value and then its removal", changes)
	}
}
