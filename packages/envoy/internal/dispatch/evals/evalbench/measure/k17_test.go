package main

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/evals"
)

// paintWindow is a paint of two reads, the first from 12:00:00 to 12:00:01 and the last from
// 12:00:09 to 12:00:10, so its window is those ten seconds.
func paintWindow() ([]measuredCase, time.Time) {
	start := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	return []measuredCase{
		{paintCase: paintCase{name: "list-84d-compare"}, t0: start, t1: start.Add(time.Second)},
		{paintCase: paintCase{name: "timeline-28d"}, t0: start.Add(9 * time.Second), t1: start.Add(10 * time.Second)},
	}, start
}

// TestEveryRequestToGitHubInThePaintsWindowIsAFailLine: check k17 reads the stand-in's log and prints
// a FAIL line for each request received between the paint's first request and the answer of its
// last, the two instants included, and counts the requests outside the window, the server's own
// loops' before the paint and after it, for information alone.
func TestEveryRequestToGitHubInThePaintsWindowIsAFailLine(t *testing.T) {
	t.Parallel()
	measured, start := paintWindow()
	at := func(offset time.Duration) githubRequest {
		return githubRequest{Time: start.Add(offset), Method: http.MethodGet, Path: fmt.Sprintf("/repos/acme/infra-platform/commits/%d", offset.Milliseconds()),
			Accept: "application/vnd.github.diff"}
	}
	for _, row := range []struct {
		name     string
		requests []githubRequest
		inside   int
		outside  int
	}{
		{"no request", nil, 0, 0},
		{"requests before and after the window only", []githubRequest{at(-time.Hour), at(-time.Millisecond), at(10*time.Second + time.Millisecond), at(time.Hour)}, 0, 4},
		{"a request in the first read", []githubRequest{at(500 * time.Millisecond)}, 1, 0},
		{"a request between the reads", []githubRequest{at(5 * time.Second)}, 1, 0},
		{"a request at the first instant and one at the last", []githubRequest{at(0), at(10 * time.Second)}, 2, 0},
		{"three inside and two outside", []githubRequest{at(-time.Second), at(time.Second), at(2 * time.Second), at(9500 * time.Millisecond), at(11 * time.Second)}, 3, 2},
	} {
		judged := &report{prefix: "RESULT"}
		inside, outside := checkGitHubStub(judged, row.requests, measured)
		if len(judged.fails) != row.inside {
			t.Errorf("%s: %d FAIL lines %q, want %d", row.name, len(judged.fails), judged.fails, row.inside)
		}
		if inside != row.inside || outside != row.outside {
			t.Errorf("%s: the RESULT line counts %d requests in the paint and %d outside it, want %d and %d", row.name, inside, outside, row.inside, row.outside)
		}
	}
}

// TestAFailLineNamesTheRequestAndWhenItCame: the line says what the server asked GitHub for, so the
// negative control's log reads as the replay it stands for.
func TestAFailLineNamesTheRequestAndWhenItCame(t *testing.T) {
	t.Parallel()
	measured, start := paintWindow()
	judged := &report{prefix: "RESULT"}
	checkGitHubStub(judged, []githubRequest{{Time: start.Add(2500 * time.Millisecond), Method: http.MethodGet, Path: "/repos/acme/infra-platform/commits/abc", Accept: "application/vnd.github.diff"}}, measured)
	if len(judged.fails) != 1 {
		t.Fatalf("%d FAIL lines, want 1: %q", len(judged.fails), judged.fails)
	}
	for _, want := range []string{"k17", "GET", "/repos/acme/infra-platform/commits/abc", "application/vnd.github.diff", "2.5s"} {
		if !strings.Contains(judged.fails[0], want) {
			t.Errorf("the FAIL line %q does not name %q", judged.fails[0], want)
		}
	}
}

// TestAPaintWithNoReadHasNoWindowToLookInAndFails: a paint that measured no read has no window, and
// a check that looked at none proves nothing.
func TestAPaintWithNoReadHasNoWindowToLookInAndFails(t *testing.T) {
	t.Parallel()
	judged := &report{prefix: "RESULT"}
	checkGitHubStub(judged, nil, nil)
	if len(judged.fails) != 1 {
		t.Errorf("%d FAIL lines for a paint of no read, want 1: %q", len(judged.fails), judged.fails)
	}
}

// codeStatement is a statement of a read of the paint, with the plan it was explained as.
func codeStatement(queryID int64, query, plan string) (statement, string) {
	return statement{queryID: queryID, query: query, calls: 1, execMs: 1}, plan
}

// TestNoStatementOfThePaintMayNameTheCodeSurvivalResults: check k17's SQL clause fails a statement
// of any read that names code_changes, code_pairs or code_days, in its text or in the relations
// its plan scans, one FAIL line each naming the tables; a statement of the stored day rows and
// figures, or of the repository's row, passes, and a paint with no statement examined fails.
func TestNoStatementOfThePaintMayNameTheCodeSurvivalResults(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name       string
		query      string
		plan       string
		failed     bool
		namesTable string
	}{
		{"the stored range figures", "select * from eval_range_figures where project_key = $1", "Index Scan using eval_range_figures_pkey on eval_range_figures", false, ""},
		{"the day rows", "select * from eval_days d join eval_project_owners o on o.owner = d.project_key", "Index Scan using eval_days_window on eval_days d", false, ""},
		{"the repository's row", "select state from code_repos where repo = $1", "Index Scan using code_repos_pkey on code_repos", false, ""},
		{"the projects' digests", "select groups_digest from code_projects", "Seq Scan on code_projects", false, ""},
		{"code_changes in the text", "select * from code_changes where repo = $1", "", true, "code_changes"},
		{"code_pairs in a join", "select c.sha from eval_days d join code_pairs p on p.repo = $1 join eval_days e on true", "", true, "code_pairs"},
		{"code_days in upper case", "SELECT day FROM CODE_DAYS WHERE repo = $1", "", true, "code_days"},
		{"code_days in a subquery", "select (select count(*) from code_days) from eval_days", "", true, "code_days"},
		{"a scan of code_pairs the text does not name", "select * from some_view", "Seq Scan on code_pairs p", true, "code_pairs"},
		{"an index of code_days is no table", "select 1", "Index Only Scan using code_days_pkey on eval_days", false, ""},
	} {
		one := measuredCase{paintCase: paintCase{name: "list-84d-compare"}}
		stated, plan := codeStatement(7, row.query, row.plan)
		one.own = []statement{stated}
		one.plans = map[int64]string{7: plan}
		judged := &report{prefix: "RESULT"}
		checkCodeResultReads(judged, []measuredCase{one})
		switch {
		case row.failed && (len(judged.fails) != 1 || !strings.Contains(judged.fails[0], row.namesTable) || !strings.Contains(judged.fails[0], "k17")):
			t.Errorf("%s: judged %q, want one k17 FAIL line naming %s", row.name, judged.fails, row.namesTable)
		case !row.failed && len(judged.fails) != 0:
			t.Errorf("%s: judged %q, want no FAIL line", row.name, judged.fails)
		}
	}
	judged := &report{prefix: "RESULT"}
	checkCodeResultReads(judged, []measuredCase{{paintCase: paintCase{name: "list-84d-compare"}}})
	if len(judged.fails) != 1 {
		t.Errorf("a paint with no statement of its own was judged %q, want one FAIL line", judged.fails)
	}
}

// TestADeliveryReadIsHeldToTheSameRule: the Delivery timeline's reads, which are no read of the
// evals, read no code result either.
func TestADeliveryReadIsHeldToTheSameRule(t *testing.T) {
	t.Parallel()
	one := measuredCase{paintCase: paintCase{name: "timeline-84d", delivery: true}, own: []statement{{queryID: 1, query: "select * from code_days", calls: 1}}}
	judged := &report{prefix: "RESULT"}
	checkCodeResultReads(judged, []measuredCase{one})
	if len(judged.fails) != 1 {
		t.Errorf("a Delivery read of code_days was judged %q, want one FAIL line", judged.fails)
	}
}

// evalListBody is the list read's answer holding the slugs, each stating a value of its status
// series where stated names it.
func evalListBody(project string, stated map[string]bool, slugs ...string) []byte {
	var items []string
	for _, slug := range slugs {
		value := "null"
		if stated[slug] {
			value = "12"
		}
		items = append(items, fmt.Sprintf(`{"id":%q,"definition":{},"series":[{"series":"status","window":{"sum":%s,"mean":null},"compare":{}}]}`, project+"/"+slug, value))
	}
	return []byte(`{"items":[` + strings.Join(items, ",") + `],"total":` + fmt.Sprint(len(items)) + `}`)
}

// TestEachReadOfTheEvalsListHoldsBothCodeEvalsAndStatesTheirValuesOverMeasuredDays: at this head the
// paint's list reads each hold both code evals, and the 84-day reads, whose windows hold days the
// code evals measured, state a value of each; the 28-day reads' windows hold only days that still
// wait, so they may state none. A list that lacks either eval, or an 84-day read that states no
// value of one, fails, and a paint that examined no list proves nothing.
func TestEachReadOfTheEvalsListHoldsBothCodeEvalsAndStatesTheirValuesOverMeasuredDays(t *testing.T) {
	t.Parallel()
	both := []string{"deploy-frequency", evals.SlugCodeSurvival, evals.SlugPostMergeRework}
	read := func(name string, codeStated bool, body []byte) measuredCase {
		return measuredCase{paintCase: paintCase{name: name, storedFigures: true, codeStated: codeStated}, body: body}
	}
	both12 := map[string]bool{evals.SlugCodeSurvival: true, evals.SlugPostMergeRework: true}
	for _, row := range []struct {
		name   string
		reads  []measuredCase
		fails  int
		wanted string
	}{
		{"an 84-day read that states both, and a 28-day read that states none",
			[]measuredCase{read("list-84d-compare", true, evalListBody("INFRA", both12, both...)), read("list-28d", false, evalListBody("INFRA", nil, both...))}, 0, ""},
		{"an 84-day read that states one", []measuredCase{read("list-84d-compare", true, evalListBody("INFRA", map[string]bool{evals.SlugCodeSurvival: true}, both...))}, 1, evals.SlugPostMergeRework},
		{"an 84-day read that states none", []measuredCase{read("list-84d-compare-repo", true, evalListBody("INFRA", nil, both...))}, 2, "states no value"},
		{"a list that lacks one", []measuredCase{read("list-28d", false, evalListBody("INFRA", nil, "deploy-frequency", evals.SlugCodeSurvival))}, 1, evals.SlugPostMergeRework},
		{"a list that lacks both", []measuredCase{read("list-28d-repo", false, evalListBody("INFRA", nil, "deploy-frequency"))}, 2, "holds no"},
		{"an empty list", []measuredCase{read("list-28d", false, evalListBody("INFRA", nil))}, 2, "holds no"},
		{"an answer that is no list", []measuredCase{read("list-28d", false, []byte("<html>"))}, 1, "read the evals list"},
		{"no list read", []measuredCase{{paintCase: paintCase{name: "timeline-84d", delivery: true}}}, 1, "no read of the evals list"},
	} {
		judged := &report{prefix: "RESULT"}
		checkCodeEvals(judged, row.reads)
		if len(judged.fails) != row.fails {
			t.Errorf("%s: %d FAIL lines %q, want %d", row.name, len(judged.fails), judged.fails, row.fails)
		}
		if row.wanted != "" && !strings.Contains(strings.Join(judged.fails, "\n"), row.wanted) {
			t.Errorf("%s: the FAIL lines %q do not name %q", row.name, judged.fails, row.wanted)
		}
	}
}

// TestTheBaseBeforeTheCodeEvalsHoldsNeitherOfThem: at a base that holds the eval registry the
// answer of the evals list holds neither code eval, which is what k17's base clause says; an answer
// that holds one fails, naming it, and so does one that lists no eval at all, which shows nothing of
// the base.
func TestTheBaseBeforeTheCodeEvalsHoldsNeitherOfThem(t *testing.T) {
	t.Parallel()
	read := func(body []byte) []measuredCase {
		return []measuredCase{{paintCase: paintCase{name: "base-list-84d-compare"}, body: body}}
	}
	for _, row := range []struct {
		name   string
		reads  []measuredCase
		fails  int
		wanted string
	}{
		{"neither", read(evalListBody("INFRA", nil, "deploy-frequency", "pr-ci")), 0, ""},
		{"the survival eval", read(evalListBody("INFRA", nil, "deploy-frequency", evals.SlugCodeSurvival)), 1, evals.SlugCodeSurvival},
		{"both", read(evalListBody("INFRA", nil, evals.SlugCodeSurvival, evals.SlugPostMergeRework)), 1, evals.SlugPostMergeRework},
		{"no eval", read(evalListBody("INFRA", nil)), 1, "lists no eval"},
		{"no list", read([]byte("{")), 1, "read the evals list"},
		{"no read", nil, 1, "no read"},
	} {
		judged := &report{prefix: "RESULT"}
		checkBaseEvals(judged, row.reads)
		if len(judged.fails) != row.fails {
			t.Errorf("%s: %d FAIL lines %q, want %d", row.name, len(judged.fails), judged.fails, row.fails)
		}
		if row.wanted != "" && !strings.Contains(strings.Join(judged.fails, "\n"), row.wanted) {
			t.Errorf("%s: the FAIL lines %q do not name %q", row.name, judged.fails, row.wanted)
		}
	}
}

// TestABaseThatHoldsTheRegistryIsPaintedByTheEvalsListAlone: its reads are the four list reads of
// this head's first paint, with none of their budgets, and neither the live view nor the Delivery
// timeline, so the base's judgment is the answer and not this head's budgets.
func TestABaseThatHoldsTheRegistryIsPaintedByTheEvalsListAlone(t *testing.T) {
	t.Parallel()
	cases := registryBaseCases("INFRA", time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC))
	if len(cases) != 4 {
		t.Fatalf("%d reads, want the four reads of the evals list", len(cases))
	}
	for _, one := range cases {
		if !strings.HasPrefix(one.path, "/api/v1/projects/INFRA/evals?") || one.view != nil || one.delivery || one.budgetMs != 0 || one.serverBudgetMs != 0 || one.storedFigures || !one.bodyUnjudged {
			t.Errorf("the read %+v is no budget-free read of the evals list", one)
		}
	}
}

// A read of the registry base whose body passes the first paint's body budget is measured and not
// failed, while a read of this head's evals list with the same body is.
func TestTheRegistryBasesBodyIsJudgedByNoBudget(t *testing.T) {
	t.Parallel()
	body := make([]byte, 2*budgetGzip)
	if _, err := rand.Read(body); err != nil {
		t.Fatal(err)
	}
	own := []statement{{queryID: 1, query: "select 1", calls: 1}}
	for _, row := range []struct {
		name  string
		read  paintCase
		fails int
	}{
		{"the registry base's read", paintCase{name: "base-list-84d-compare", bodyUnjudged: true}, 0},
		{"this head's read", paintCase{name: "list-84d-compare"}, 1},
	} {
		judged := &report{prefix: "RESULT"}
		judgePaint(judged, options{}, []measuredCase{{paintCase: row.read, status: http.StatusOK, body: body, own: own}})
		var bodies []string
		for _, line := range judged.fails {
			if strings.Contains(line, "gzip_bytes=") {
				bodies = append(bodies, line)
			}
		}
		if len(bodies) != row.fails {
			t.Errorf("%s: %d FAIL lines of the body %q, want %d", row.name, len(bodies), bodies, row.fails)
		}
	}
}
