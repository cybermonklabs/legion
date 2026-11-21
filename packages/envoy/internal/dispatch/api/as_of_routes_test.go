// as_of_routes_test.go holds the table to its as_of kinds (readEntry.AsOf). Every GET row names
// a kind, and register refuses a table with a GET row that names none. The rows that answer
// as_of are the rows of the plan's table "Routes and their kinds" (INFRA-4620), and whoami alone
// describes the caller now. Every other row, every method, refuses a request that sends as_of
// 400 AS_OF_NOT_SERVED before its handler runs (asOfGate). An answers row refuses an as_of that
// is not a time, one given twice and one later than the database's now() 400 INVALID_AS_OF, and
// otherwise runs its handler under the cut, rounded down to the microsecond.
package api

import (
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/asof"
)

// thePlansAnswersRows is every row of kind asOfAnswers, written out from the plan's table
// "Routes and their kinds" (INFRA-4620) and not read from readClasses, so a row that joins the
// kind or leaves it is a decision made here on purpose.
var thePlansAnswersRows = []string{
	"GET /api/v1/issues/{key}",
	"GET /api/v1/issues",
	"GET /api/v1/issues/resolve",
	"GET /api/v1/issues/{key}/events",
	"GET /api/v1/artifacts/{id}/events",
	"GET /api/v1/events/log",
	"GET /api/v1/issues/{key}/comments",
	"GET /api/v1/artifacts/{id}/comments",
	"GET /api/v1/comments/{id}",
	"GET /api/v1/issues/{key}/messages",
	"GET /api/v1/comments/{id}/history",
	"GET /api/v1/messages/{id}/history",
	"GET /api/v1/issues/{key}/messages/{id}",
	"GET /api/v1/messages/{id}",
	"GET /api/v1/issues/{key}/asks",
	"GET /api/v1/artifacts/{id}/asks",
	"GET /api/v1/asks/{id}",
	"GET /api/v1/issues/{key}/artifacts",
	"GET /api/v1/issues/{key}/artifacts/{slug}",
	"GET /api/v1/projects/{key}/artifacts",
	"GET /api/v1/projects/{key}/artifacts/{slug}",
	"GET /api/v1/agents/{session_id}/artifacts/{slug}",
	"GET /api/v1/artifacts/{id}",
	"GET /api/v1/artifacts/{id}/text",
	"GET /api/v1/artifacts/{id}/versions/{number}",
	"GET /api/v1/issues/{key}/artifacts/{slug}/blocks",
	"GET /api/v1/projects/{key}/artifacts/{slug}/blocks",
	"GET /api/v1/issues/{key}/results",
	"GET /api/v1/search",
	"GET /api/v1/search/results",
	"GET /api/v1/agents/{session_id}/answers",
	"GET /api/v1/asks/open",
	"GET /api/v1/inbox",
}

// asOfKindName is kind as a failure names it.
func asOfKindName(kind asOfKind) string {
	switch kind {
	case asOfUnset:
		return "asOfUnset"
	case asOfAnswers:
		return "asOfAnswers"
	case asOfNoHistory:
		return "asOfNoHistory"
	case asOfCaller:
		return "asOfCaller"
	}
	return fmt.Sprintf("asOfKind(%d), which is no kind", int(kind))
}

// rowAnswersAsOf reports whether the row named name answers as_of. It reads readClasses and not
// the row (apiRoute.Read), as readOnlyAnswer does, so a table routes() stamped wrong fails the
// walk rather than agreeing with the gate. A row that is not a GET row answers no as_of.
func rowAnswersAsOf(route apiRoute) bool {
	return route.Method == http.MethodGet && readClasses[route.Method+" "+route.Pattern].AsOf == asOfAnswers
}

// asOfNotServedMessage is the message of the 400 AS_OF_NOT_SERVED a row that holds no history
// answers, written out here and not read from the server, so a refusal that drifted from it
// fails.
func asOfNotServedMessage(name string) string {
	return "as_of: " + name + " holds no history; it answers the present only"
}

// asOfProbe is the request the walks below send route: the probe path (readOnlyProbePath) with
// query, the probe body (routeProbeBody), a valid Idempotency-Key and the person alice by the
// test identity's header, whose credential holds every scope a row asks for.
func asOfProbe(route apiRoute, query string) *http.Request {
	target := readOnlyProbePath(route.Pattern)
	if query != "" {
		target += "?" + query
	}
	request := httptest.NewRequest(route.Method, target, strings.NewReader(routeProbeBody(route)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	request.Header.Set(contracts.IdempotencyKeyHeader, readOnlyProbeKey)
	return request
}

// cutRecorder is a row's handler replaced: it counts its runs and keeps the cut each run's
// context carried, empty for a run with none.
type cutRecorder struct {
	runs []string
}

func (c *cutRecorder) handler(w http.ResponseWriter, r *http.Request) {
	cut, carried := asof.CutFrom(r.Context())
	seen := ""
	if carried {
		seen = cut.String()
	}
	c.runs = append(c.runs, seen)
	w.WriteHeader(http.StatusNoContent)
}

// TestEveryGETRowNamesItsAsOfKind walks every GET row of the table with its test hooks in
// (readOnlyTable), read off the table and not listed, so a GET row added later fails here until
// its read entry names its as_of kind. It fails once, naming each GET row whose entry leaves AsOf
// unset or holds no kind. Then it takes the as_of kind away from one row's entry and holds
// register to refuse the table, naming that row, as it refuses a GET row with no entry.
func TestEveryGETRowNamesItsAsOfKind(t *testing.T) {
	gets := 0
	kinds := map[asOfKind]int{}
	var problems []string
	for _, route := range readOnlyTable() {
		if route.Method != http.MethodGet {
			continue
		}
		gets++
		name := route.Method + " " + route.Pattern
		kind := readClasses[name].AsOf
		kinds[kind]++
		switch kind {
		case asOfAnswers, asOfNoHistory, asOfCaller:
		default:
			problems = append(problems, name+": its read entry's AsOf is "+asOfKindName(kind)+", want asOfAnswers, asOfNoHistory or asOfCaller")
		}
	}
	if gets == 0 {
		t.Fatal("the table with its test hooks in holds no GET row, so the walk read nothing")
	}
	t.Logf("walked %d GET rows of the table (test hooks in): %d answer as_of, %d hold no history, %d describe the caller now",
		gets, kinds[asOfAnswers], kinds[asOfNoHistory], kinds[asOfCaller])
	if len(problems) > 0 {
		t.Errorf("the as_of kinds (read_classes.go) have %d problems:\n%s", len(problems), strings.Join(problems, "\n"))
	}

	t.Run("register refuses a GET row whose AsOf is unset, naming the row", func(t *testing.T) {
		kept := readClasses
		t.Cleanup(func() { readClasses = kept })
		if refusal := registerRefusal(t); refusal != "" {
			t.Fatalf("api.Register refused the read class as it stands: %s", refusal)
		}
		for _, row := range []string{"GET /api/v1/issues/{key}", "GET /api/v1/limits"} {
			readClasses = maps.Clone(kept)
			entry := readClasses[row]
			entry.AsOf = asOfUnset
			readClasses[row] = entry
			refusal := registerRefusal(t)
			if !strings.Contains(refusal, "read class") || !strings.Contains(refusal, row+": ") || !strings.Contains(refusal, "AsOf") {
				t.Errorf("with %s's AsOf unset, api.Register refused %q, want a refusal of the read class naming the row and its AsOf", row, refusal)
			}
		}
	})
}

// TestTheAsOfKindsAreThePlansTable holds the rows of kind asOfAnswers to the plan's table
// (thePlansAnswersRows), both ways: a row the plan lists that the table does not mount or that
// is not of kind answers fails by name, and so does a row of kind answers that the plan does not
// list. GET /api/v1/whoami is the one row of kind asOfCaller.
func TestTheAsOfKindsAreThePlansTable(t *testing.T) {
	listed := map[string]bool{}
	for _, name := range thePlansAnswersRows {
		if listed[name] {
			t.Fatalf("thePlansAnswersRows lists %s twice", name)
		}
		listed[name] = true
	}
	mounted := map[string]bool{}
	answers := map[string]bool{}
	var callers []string
	for _, route := range readOnlyTable() {
		name := route.Method + " " + route.Pattern
		mounted[name] = true
		if route.Method != http.MethodGet {
			continue
		}
		switch readClasses[name].AsOf {
		case asOfAnswers:
			answers[name] = true
		case asOfCaller:
			callers = append(callers, name)
		}
	}
	var problems []string
	for _, name := range thePlansAnswersRows {
		switch {
		case !mounted[name]:
			problems = append(problems, name+": the plan's table says it answers as_of, and the table mounts no such row")
		case !answers[name]:
			problems = append(problems, name+": the plan's table says it answers as_of, and its read entry's AsOf is "+asOfKindName(readClasses[name].AsOf))
		}
	}
	for name := range answers {
		if !listed[name] {
			problems = append(problems, name+": its read entry says it answers as_of, and the plan's table does not list it")
		}
	}
	if !slices.Equal(callers, []string{whoamiRow}) {
		problems = append(problems, fmt.Sprintf("the rows of kind asOfCaller are %v, want %s alone", callers, whoamiRow))
	}
	slices.Sort(problems)
	t.Logf("the table holds %d rows of kind asOfAnswers; the plan's table lists %d", len(answers), len(thePlansAnswersRows))
	if len(problems) > 0 {
		t.Errorf("the as_of kinds differ from the plan's table in %d ways:\n%s", len(problems), strings.Join(problems, "\n"))
	}
}

// TestEveryRowThatDoesNotAnswerAsOfRefusesIt builds every row's own chain as register does
// (routeHandler), test hooks in, with the row's handler replaced by a recorder, and sends each a
// request with as_of, its probe body and a valid Idempotency-Key from a person whose credential
// holds every scope. Every row that does not answer as_of, every method, answers 400
// AS_OF_NOT_SERVED naming the row, and its handler never runs. Every row of kind answers runs its
// handler once, with the cut on its context, rounded down to the microsecond; sent again with no
// as_of, it runs its handler with no cut. The test identity records the person it resolves, a
// write, and the store refuses every write under a cut, so a row whose own scopes resolve the
// caller behind the gate (GET /api/v1/inbox) runs its handler only when the gate resolved the
// caller before it put the cut on the context. Each row is counted once its assertions have run,
// and the count is held to the rows the registered server mounts, so a walk that skipped a row
// fails.
func TestEveryRowThatDoesNotAnswerAsOfRefusesIt(t *testing.T) {
	_, _, _, served := newRegisteredTestServer(t, testServerOptions{testHooks: true})
	replays := served.keyedReplays()
	mounted := len(served.routeIndex)
	const (
		asOf    = "2026-01-02T03:04:05.123456789Z"
		wantCut = "2026-01-02T03:04:05.123456Z"
	)
	query := url.Values{asOfParameter: {asOf}}.Encode()

	walked, refused, answered := 0, 0, 0
	for _, route := range readOnlyTable() {
		name := route.Method + " " + route.Pattern
		recorder := &cutRecorder{}
		route.Handler = recorder.handler
		chain := served.routeHandler(route, replays[name])

		response := httptest.NewRecorder()
		chain.ServeHTTP(response, asOfProbe(route, query))
		if !rowAnswersAsOf(route) {
			refusal := gateRefusalOf(response)
			if len(recorder.runs) != 0 {
				t.Errorf("%s holds no history, and its handler ran %d times for a request with as_of, want none", name, len(recorder.runs))
			}
			if response.Code != http.StatusBadRequest || refusal.Code != contracts.DispatchAsOfNotServed || refusal.Error != asOfNotServedMessage(name) {
				t.Errorf("%s answered a request with as_of %d %s, want 400 %s %q",
					name, response.Code, strings.TrimSpace(response.Body.String()), contracts.DispatchAsOfNotServed, asOfNotServedMessage(name))
			}
			refused++
			walked++
			continue
		}
		if response.Code != http.StatusNoContent || !slices.Equal(recorder.runs, []string{wantCut}) {
			t.Errorf("%s answers as_of, and a request with as_of=%s answered %d %s with its handler's runs seeing the cuts %q, want one run under the cut %s",
				name, asOf, response.Code, strings.TrimSpace(response.Body.String()), recorder.runs, wantCut)
		}
		recorder.runs = nil
		present := httptest.NewRecorder()
		chain.ServeHTTP(present, asOfProbe(route, ""))
		if present.Code != http.StatusNoContent || !slices.Equal(recorder.runs, []string{""}) {
			t.Errorf("%s answers as_of, and a request with none answered %d %s with its handler's runs seeing the cuts %q, want one run with no cut",
				name, present.Code, strings.TrimSpace(present.Body.String()), recorder.runs)
		}
		answered++
		walked++
	}
	if walked == 0 || walked != mounted {
		t.Fatalf("walked %d rows, want every one of the %d the server mounts", walked, mounted)
	}
	t.Logf("walked the table's %d rows (test hooks in): %d refused as_of %s, %d ran their handler under the cut",
		walked, refused, contracts.DispatchAsOfNotServed, answered)
}

// TestAnAnswersRowRefusesAMalformedOrFutureAsOf sends an answers row's own chain (routeHandler),
// its handler replaced by a recorder, each as_of below. A value that is not a time, a date alone,
// an as_of given twice (the same time both times) and a time one second after the database's
// now() each answer 400 INVALID_AS_OF naming as_of, and the handler never runs. A time with nine
// fractional digits is taken rounded down to the microsecond, never up into the next second, and
// the handler runs under that cut.
func TestAnAnswersRowRefusesAMalformedOrFutureAsOf(t *testing.T) {
	_, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	const name = "GET /api/v1/issues/{key}"
	var route apiRoute
	for _, row := range readOnlyTable() {
		if row.Method+" "+row.Pattern == name {
			route = row
		}
	}
	if route.Handler == nil {
		t.Fatalf("the table mounts no row %s", name)
	}
	now := databaseNow(t, database.Pool)
	past := now.Add(-time.Hour).UTC().Truncate(time.Second)
	pastText := past.Format(time.RFC3339)
	for _, row := range []struct {
		name    string
		values  []string
		wantCut string
	}{
		{name: "not a time", values: []string{"yesterday"}},
		{name: "a date alone", values: []string{past.Format(time.DateOnly)}},
		{name: "two values", values: []string{pastText, pastText}},
		{name: "one second after the database's now", values: []string{now.Add(time.Second).UTC().Format(time.RFC3339Nano)}},
		{
			name:    "nine fractional digits, taken rounded down",
			values:  []string{past.Format("2006-01-02T15:04:05") + ".999999999Z"},
			wantCut: past.Format("2006-01-02T15:04:05") + ".999999Z",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			recorder := &cutRecorder{}
			route.Handler = recorder.handler
			response := httptest.NewRecorder()
			served.routeHandler(route, nil).ServeHTTP(response, asOfProbe(route, url.Values{asOfParameter: row.values}.Encode()))
			if row.wantCut != "" {
				if response.Code != http.StatusNoContent || !slices.Equal(recorder.runs, []string{row.wantCut}) {
					t.Fatalf("as_of=%v answered %d %s with its handler's runs seeing the cuts %q, want one run under the cut %s",
						row.values, response.Code, strings.TrimSpace(response.Body.String()), recorder.runs, row.wantCut)
				}
				return
			}
			refusal := gateRefusalOf(response)
			if len(recorder.runs) != 0 {
				t.Errorf("as_of=%v ran the handler %d times, want none", row.values, len(recorder.runs))
			}
			if response.Code != http.StatusBadRequest || refusal.Code != contracts.DispatchInvalidAsOf || !strings.Contains(refusal.Error, asOfParameter) {
				t.Errorf("as_of=%v answered %d %s, want 400 %s naming as_of",
					row.values, response.Code, strings.TrimSpace(response.Body.String()), contracts.DispatchInvalidAsOf)
			}
		})
	}
}
