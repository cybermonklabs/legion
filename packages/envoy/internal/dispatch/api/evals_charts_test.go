package api

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/evals/charts"
)

// Two declarations the eval platform's validate_charts accepts (evals/charts' corpus holds both
// shapes), from which the tests below make their lists of declared charts.
const (
	waitChart  = `{"id":"wait","title":"Wait","kind":"xy","y":{"series":"wait"}}`
	countChart = `{"id":"count","title":"Count","kind":"bar","y":{"series":"count"}}`
)

// chartList is a list of declared charts as a register write's body carries it.
func chartList(declarations ...string) json.RawMessage {
	return json.RawMessage("[" + strings.Join(declarations, ",") + "]")
}

// stepChart is a declaration of an xy over measured time that draws series as a step line
// (curve step), one line for each col of it, and further for each row or row group of a col where
// group names one; validate_charts accepts it (evals/charts' corpus holds the shape).
func stepChart(id, series, group string) string {
	chart := `{"id":"` + id + `","title":"` + id + `","kind":"xy","y":{"series":"` + series + `"},"curve":"step"`
	if group != "" {
		chart += `,"group":"` + group + `"`
	}
	return chart + `}`
}

// stepReading is one reading of series under col and row key at noon of day, of value, or with no
// value where value is nil, its absence's reason then given.
func stepReading(series, col, rowKey string, value *float64, day time.Time) map[string]any {
	written := map[string]any{
		"series": series, "col": col, "row_kind": "sample", "row_key": rowKey, "measured_at": day.Add(12 * time.Hour).Format(time.RFC3339),
	}
	if value == nil {
		written["value_absent_reason"] = "limit cleared"
	} else {
		written["value"] = *value
	}
	return written
}

// registerLastSeries registers slug of project with series, each folded by its last value, and
// charts, failing the test unless the write stores it.
func registerLastSeries(t *testing.T, handler http.Handler, project, slug string, series []string, declared json.RawMessage) {
	t.Helper()
	listed := make([]any, len(series))
	for index, name := range series {
		listed[index] = map[string]any{"series": name, "agg": evals.AggLast, "listed": true}
	}
	if registered := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/"+project+"/evals",
		map[string]any{"slug": slug, "name": slug, "series": listed, "charts": declared}, "register-"+slug); registered.Code != http.StatusCreated {
		t.Fatalf("register %s: status %d, body %s", slug, registered.Code, registered.Body.String())
	}
}

// logReadings logs readings under slug of project with key, failing the test unless the write
// stores them.
func logReadings(t *testing.T, handler http.Handler, project, slug, key string, readings ...map[string]any) {
	t.Helper()
	rows := make([]any, len(readings))
	for index, one := range readings {
		rows[index] = one
	}
	if written := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/"+project+"/evals/"+slug+"/readings",
		map[string]any{"readings": rows}, key); written.Code != http.StatusCreated {
		t.Fatalf("log %d readings of %s: status %d, body %s", len(readings), slug, written.Code, written.Body.String())
	}
}

// rangesQuery is the query of a read of window and compare.
func rangesQuery(window, compare evals.Range) url.Values {
	return url.Values{
		"from": {window.From.Format(time.RFC3339)}, "to": {window.To.Format(time.RFC3339)},
		"compare_from": {compare.From.Format(time.RFC3339)}, "compare_to": {compare.To.Format(time.RFC3339)},
	}
}

// sameJSON reports whether two JSON texts hold one value, whatever their spacing and key order:
// the store answers its own spelling of the charts a write sent (evals.Definition.Charts).
func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("decode %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return reflect.DeepEqual(x, y)
}

// TestARegisterWriteCarriesChartsAndAnswerForward: a register write that declares one chart and
// an answer writes version 1 with both; one that changes only the target writes version 2, which
// keeps both; one that changes the charts writes version 3 with the new list while version 2
// keeps its own; one that changes the answer alone writes version 4; and the same list sent again,
// spaced and keyed otherwise, writes nothing (200). The detail read answers each version's charts
// beside it and its answer in its definition.
func TestARegisterWriteCarriesChartsAndAnswerForward(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "EVL")
	const firstAnswer, secondAnswer = "Most waits end within an hour.", "Most waits end within forty minutes."
	one, two := chartList(waitChart), chartList(waitChart, countChart)
	register := func(key string, body map[string]any, status int) evals.Definition {
		t.Helper()
		body["slug"] = "wait-time"
		response := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/EVL/evals", body, key)
		if response.Code != status {
			t.Fatalf("register %s: status %d, body %s, want %d", key, response.Code, response.Body.String(), status)
		}
		written := decodeBody[evalRegisterResponse](t, response)
		return written.Items[0].Definition
	}
	first := register("v1", map[string]any{
		"name": "Wait time", "answer": firstAnswer, "charts": one,
		"series": []any{map[string]any{"series": "wait", "agg": "quantile"}},
	}, http.StatusCreated)
	register("v2", map[string]any{"target": 30}, http.StatusCreated)
	register("v3", map[string]any{"charts": two}, http.StatusCreated)
	newest := register("v4", map[string]any{"answer": secondAnswer}, http.StatusCreated)
	// The same two declarations, spaced and with their keys in another order.
	respelled := json.RawMessage(`[ {"y": {"series": "wait"}, "kind": "xy", "title": "Wait", "id": "wait"},
		{"kind": "bar", "id": "count", "y": {"series": "count"}, "title": "Count"} ]`)
	again := register("v4-again", map[string]any{"charts": respelled}, http.StatusOK)
	if first.Answer != firstAnswer || newest.Answer != secondAnswer || again.Version != 4 {
		t.Fatalf("the writes answered %q at version 1 and %q at version 4, and the repeat version %d; want %q, %q and 4",
			first.Answer, newest.Answer, again.Version, firstAnswer, secondAnswer)
	}

	detail := evalGet[evalDetailResponse](t, handler, "/api/v1/projects/EVL/evals/wait-time")
	stored := countRows(t, database, `select count(*) from eval_versions where eval_id = 'EVL/wait-time'`)
	want := []struct {
		version int
		charts  json.RawMessage
		answer  string
		target  *float64
	}{
		{4, two, secondAnswer, new(30.0)},
		{3, two, firstAnswer, new(30.0)},
		{2, one, firstAnswer, new(30.0)},
		{1, one, firstAnswer, nil},
	}
	if stored != 4 || len(detail.Items) != len(want) {
		t.Fatalf("%d versions stored and %d answered, want 4 of each", stored, len(detail.Items))
	}
	for index, item := range detail.Items {
		w, got := want[index], item.Definition
		if got.Version != w.version || !sameJSON(t, item.Charts, w.charts) || got.Answer != w.answer || !reflect.DeepEqual(got.Target, w.target) {
			t.Errorf("version %d answers charts %s, answer %q and target %v; want version %d with %s, %q and %v",
				got.Version, item.Charts, got.Answer, got.Target, w.version, w.charts, w.answer, w.target)
		}
		t.Logf("version %d: charts %s, answer %q", got.Version, item.Charts, got.Answer)
	}
}

// TestTheListReadsAnswerTheAnswerAndNoCharts: an eval registered with an answer and two declared
// charts is answered with that answer in its definition by the project's list read, the
// cross-project read and the evals view's snapshot, and none of the three bodies holds a
// `charts` member anywhere; the eval's own read answers its charts.
func TestTheListReadsAnswerTheAnswerAndNoCharts(t *testing.T) {
	rig := newHubRig(t, nil)
	createTestProject(t, rig.handler, "EVL")
	const answer = "Most waits end within an hour."
	registered := evalRequest(t, rig.handler, http.MethodPost, "/api/v1/projects/EVL/evals",
		map[string]any{"slug": "wait-time", "name": "Wait time", "answer": answer, "charts": chartList(waitChart, countChart)}, "register")
	if registered.Code != http.StatusCreated {
		t.Fatalf("register: status %d, body %s", registered.Code, registered.Body.String())
	}
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("evals", evalsLiveView("EVL", DeliveryLiveWindow{Preset: new(7)}, "")))
	bodies := map[string]json.RawMessage{
		"GET /api/v1/projects/EVL/evals":   getBody(t, rig.handler, "/api/v1/projects/EVL/evals"),
		"GET /api/v1/evals?project=EVL":    getBody(t, rig.handler, "/api/v1/evals?project=EVL"),
		"the evals view's snapshot of EVL": conn.snapshot(t, "evals").Body,
	}
	for _, what := range slices.Sorted(maps.Keys(bodies)) {
		body := bodies[what]
		var list evalListResponse
		if err := json.Unmarshal(body, &list); err != nil {
			t.Fatalf("%s: decode: %v", what, err)
		}
		item := findEval(t, list.Items, "EVL", "wait-time")
		if item.Definition.Answer != answer {
			t.Errorf("%s answers the answer %q, want %q", what, item.Definition.Answer, answer)
		}
		var tree any
		if err := json.Unmarshal(body, &tree); err != nil {
			t.Fatalf("%s: decode: %v", what, err)
		}
		if at := memberAt(tree, "charts", "$"); at != "" {
			t.Errorf("%s holds a charts member at %s, want none", what, at)
		}
		t.Logf("%s: %d evals, wait-time answering %q, no charts member", what, len(list.Items), item.Definition.Answer)
	}
	detail := evalGet[evalDetailResponse](t, rig.handler, "/api/v1/projects/EVL/evals/wait-time")
	if len(detail.Items) != 1 {
		t.Fatalf("the eval's own read answers %d versions, want 1", len(detail.Items))
	}
	if !sameJSON(t, detail.Items[0].Charts, chartList(waitChart, countChart)) {
		t.Errorf("the eval's own read answers the charts %s, want the two declared", detail.Items[0].Charts)
	}
}

// memberAt is the path, from at, of the first object member named name in value, depth first in
// key order, or "" where value holds none.
func memberAt(value any, name, at string) string {
	switch held := value.(type) {
	case map[string]any:
		for _, key := range slices.Sorted(maps.Keys(held)) {
			if key == name {
				return at + "." + key
			}
			if found := memberAt(held[key], name, at+"."+key); found != "" {
				return found
			}
		}
	case []any:
		for index, element := range held {
			if found := memberAt(element, name, fmt.Sprintf("%s[%d]", at, index)); found != "" {
				return found
			}
		}
	}
	return ""
}

// TestAChartsWriteByATokenOfAnotherProjectIsRefused: a broker token scoped to one project is
// refused 403 PROJECT_SCOPE for a register write of charts and an answer of another project's
// eval, which keeps its one version and the charts a person declared; the same write to an eval
// of its own project is written, 201.
func TestAChartsWriteByATokenOfAnotherProjectIsRefused(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-scoped-to-one-project"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	for _, key := range []string{brokerTestProject, "OTH"} {
		if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": key, "name": key}); created.Code != http.StatusCreated {
			t.Fatalf("create project %s: status %d, body %s", key, created.Code, created.Body.String())
		}
	}
	declared := chartList(waitChart)
	if registered := rig.asPerson(t, http.MethodPost, "/api/v1/projects/OTH/evals",
		map[string]any{"slug": "wait-time", "answer": "A person's answer.", "charts": declared}); registered.Code != http.StatusCreated {
		t.Fatalf("register OTH's eval as a person: status %d, body %s", registered.Code, registered.Body.String())
	}
	send := func(project, key string) (int, evalRefusal) {
		t.Helper()
		request := caller.presenting(t, http.MethodPost, "/api/v1/projects/"+project+"/evals", token,
			map[string]any{"slug": "wait-time", "answer": "The token's answer.", "charts": chartList(waitChart, countChart)})
		request.Header.Set(contracts.IdempotencyKeyHeader, key)
		response := rig.serve(request)
		return response.Code, decodeBody[evalRefusal](t, response)
	}
	status, refused := send("OTH", "charts-oth")
	if status != http.StatusForbidden || refused.Code != codeProjectScope {
		t.Fatalf("the token's write to OTH answers %d %s %q, want 403 %s", status, refused.Code, refused.Error, codeProjectScope)
	}
	versions := countRows(t, rig.database, `select count(*) from eval_versions where eval_id = 'OTH/wait-time'`)
	var kept bool
	var answer string
	if err := rig.database.Pool.QueryRow(t.Context(), `select charts = $1::jsonb, answer from evals_current where eval_id = 'OTH/wait-time'`,
		string(declared)).Scan(&kept, &answer); err != nil {
		t.Fatalf("read OTH's eval: %v", err)
	}
	if versions != 1 || !kept || answer != "A person's answer." {
		t.Fatalf("OTH's eval holds %d versions, its person's charts %v and the answer %q after the refused write; want 1, true and the person's",
			versions, kept, answer)
	}
	if status, _ := send(brokerTestProject, "charts-own"); status != http.StatusCreated {
		t.Fatalf("the token's write to its own project %s answers %d, want 201", brokerTestProject, status)
	}
	own := countRows(t, rig.database, `select count(*) from eval_versions where eval_id = $1 and charts <> '[]'::jsonb`, brokerTestProject+"/wait-time")
	if own != 1 {
		t.Errorf("the token's write to %s stored %d versions with charts, want 1", brokerTestProject, own)
	}
	t.Logf("refused %s %q to OTH, which keeps %d version; the same write to %s stored %d", refused.Code, refused.Error, versions, brokerTestProject, own)
}

// TestChartsAreRefusedWithTheirFieldNamed: a register write whose charts the eval platform's
// validate_charts rules refuse answers 400 CHART_SPEC_INVALID, its field the first refusal's
// (charts[2].y.series for the third declaration's series, charts[1].kind for a module) and its
// error every message evals/charts answers for those charts, one a line; charts that are no list
// or null are refused naming charts, and charts that are no JSON value the platform's reader takes
// (an integer of 4,301 digits) 400 INVALID_JSON naming charts. None of them writes a version,
// whether of an eval that holds charts already, which keeps its list, or of a slug no eval holds.
func TestChartsAreRefusedWithTheirFieldNamed(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "EVL")
	held := chartList(waitChart)
	if registered := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/EVL/evals",
		map[string]any{"slug": "wait-time", "charts": held}, "register"); registered.Code != http.StatusCreated {
		t.Fatalf("register: status %d, body %s", registered.Code, registered.Body.String())
	}
	badSeries := `{"id":"late","title":"Late","kind":"xy","y":{"series":"Not A Series"}}`
	module := `{"id":"old","title":"Old","kind":"module","module":"retired-module"}`
	huge := `{"id":"big","title":"Big","kind":"xy","y":{"series":"wait","max":1` + strings.Repeat("0", 4300) + `}}`
	cases := []struct {
		name, slug string
		charts     json.RawMessage
		code       string
		field      string
	}{
		{"the third declaration's series", "wait-time", chartList(waitChart, countChart, badSeries), evals.CodeChartSpecInvalid, "charts[2].y.series"},
		{"a module", "wait-time", chartList(waitChart, module), evals.CodeChartSpecInvalid, "charts[1].kind"},
		{"an object", "wait-time", json.RawMessage(waitChart), evals.CodeChartSpecInvalid, "charts"},
		{"null", "wait-time", json.RawMessage("null"), evals.CodeChartSpecInvalid, "charts"},
		{"a first registration's bad series", "never-written", chartList(badSeries), evals.CodeChartSpecInvalid, "charts[0].y.series"},
		{"an integer of 4,301 digits", "wait-time", chartList(huge), evals.CodeInvalidJSON, "charts"},
	}
	countVersions := `select count(*) from eval_versions where project_key = 'EVL'`
	before := countRows(t, database, countVersions)
	for index, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			response := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/EVL/evals",
				map[string]any{"slug": c.slug, "answer": "Refused with its charts.", "charts": c.charts}, fmt.Sprintf("refused-%d", index))
			refusal := decodeBody[evalRefusal](t, response)
			if response.Code != http.StatusBadRequest || refusal.Code != c.code || refusal.Field != c.field {
				t.Fatalf("status %d, body %s; want 400 %s naming %s", response.Code, response.Body.String(), c.code, c.field)
			}
			if c.code == evals.CodeChartSpecInvalid {
				refusals, err := charts.Validate(c.charts)
				if err != nil || len(refusals) == 0 {
					t.Fatalf("the validator answers %v, %v for these charts, want refusals", refusals, err)
				}
				messages := make([]string, len(refusals))
				for at, one := range refusals {
					messages[at] = one.Message
				}
				if want := strings.Join(messages, "\n"); refusal.Error != want {
					t.Errorf("the refusal's error is %q, want the validator's messages %q", refusal.Error, want)
				}
			}
			t.Logf("%d %s %s: %q", response.Code, refusal.Code, refusal.Field, refusal.Error)
		})
	}
	var kept bool
	if err := database.Pool.QueryRow(t.Context(), `select charts = $1::jsonb from evals_current where eval_id = 'EVL/wait-time'`,
		string(held)).Scan(&kept); err != nil {
		t.Fatalf("read wait-time's charts: %v", err)
	}
	if after := countRows(t, database, countVersions); after != before || !kept {
		t.Errorf("the refused writes moved EVL's versions from %d to %d, and wait-time keeps its charts %v; want no version and its list kept",
			before, after, kept)
	}
	if never := countRows(t, database, `select count(*) from eval_versions where eval_id = 'EVL/never-written'`); never != 0 {
		t.Errorf("the refused first registration stored %d versions, want none", never)
	}
}

// stepValueReads is the value each read of a list item states of slug's series over window and
// compare (from the list read, the read across projects, the eval's own read and the evals view's
// snapshot), each as `<window> <compare> <change>` of its `value` statistic, by series and then by
// read; it fails the test where the four disagree, so a caller judges one of them.
func stepValueReads(t *testing.T, rig *hubRig, project, slug string, window, compare evals.Range, series ...string) map[string]string {
	t.Helper()
	query := rangesQuery(window, compare)
	stated := func(item evalItem) map[string]string {
		values := map[string]string{}
		for _, name := range series {
			value := seriesValues(t, item, name)
			values[name] = showFigure(value.Window["value"]) + " " + showFigure(value.Compare["value"]) + " " + showFigure(value.Change["value"])
		}
		return values
	}
	reads := map[string]map[string]string{}
	list := evalGet[evalListResponse](t, rig.handler, "/api/v1/projects/"+project+"/evals?"+query.Encode())
	reads["the list read"] = stated(findEval(t, list.Items, project, slug))
	across := evalGet[evalListResponse](t, rig.handler, "/api/v1/evals?project="+project+"&"+query.Encode())
	reads["the read across projects"] = stated(findEval(t, across.Items, project, slug))
	detail := evalGet[evalDetailResponse](t, rig.handler, "/api/v1/projects/"+project+"/evals/"+slug+"?"+query.Encode())
	if len(detail.Items) == 0 {
		t.Fatalf("the eval's own read answers no version of %s", slug)
	}
	reads["the eval's own read"] = stated(detail.Items[0].evalItem)
	conn := newFakeLiveConn()
	compared := url.Values{"compare_from": query["compare_from"], "compare_to": query["compare_to"]}
	rig.hub.Subscribe(conn, subscribeFrame("evals", evalsLiveView(project, evalsDays(window.From, window.To), compared.Encode())))
	var view evalListResponse
	if err := json.Unmarshal(conn.snapshot(t, "evals").Body, &view); err != nil {
		t.Fatalf("decode the evals view's snapshot: %v", err)
	}
	reads["the evals view"] = stated(findEval(t, view.Items, project, slug))
	want := reads["the list read"]
	for _, what := range slices.Sorted(maps.Keys(reads)) {
		if !maps.Equal(reads[what], want) {
			t.Errorf("%s states %v, the list read %v", what, reads[what], want)
		}
	}
	return want
}

// TestALastStepSeriesAnswersTheNewestReadingAtTheWindowsEnd (dispatch://INFRA-2918, D20 (b), k26):
// a `last` series a step chart draws states as a range's value the newest standing reading at or
// before the range's end, read from eval_readings and not from the day rows, for the window and
// the compare range alike; of a series of two cols, the newest reading of either; and none where
// that reading has no value. cap holds 3 on day -20 and 5 on day -10, queue 4 under col value on
// day -9 and 10 under col limit on day -8: the window of the 7 days before today holds no reading of
// either and states cap 5 and queue 10, the newest cap and the newest queue reading of either col,
// and the compare range of the 6 days from day -20, cap's first day, to 14 days before today, which
// ends after cap's reading of day -20 alone, states cap 3 and a change of 2, and no queue value,
// since it starts before queue's first day (R18). A window that starts before cap's first day,
// though it holds that day's reading, states no cap value either. A reading of queue's limit with no
// value on day -2 then leaves the window no queue value. The list read, the read across projects,
// the eval's own read and the evals view state alike. With the day rows' fold alone, the window,
// which holds no day row of either, states no value of either.
func TestALastStepSeriesAnswersTheNewestReadingAtTheWindowsEnd(t *testing.T) {
	rig := newHubRig(t, nil)
	createTestProject(t, rig.handler, "EVL")
	today := measures.Midnight(rig.clock.Now())
	day := func(offset int) time.Time { return today.AddDate(0, 0, offset) }
	registerLastSeries(t, rig.handler, "EVL", "admission", []string{"cap", "queue"},
		chartList(stepChart("cap", "cap", ""), stepChart("queue", "queue", "col")))
	logReadings(t, rig.handler, "EVL", "admission", "step-readings",
		stepReading("cap", "", "a", new(3.0), day(-20)), stepReading("cap", "", "b", new(5.0), day(-10)),
		stepReading("queue", "value", "c", new(4.0), day(-9)), stepReading("queue", "limit", "d", new(10.0), day(-8)))
	window, compare := evalDaysEnding(7, today), evalDaysEnding(6, day(-14))

	stated := stepValueReads(t, rig, "EVL", "admission", window, compare, "cap", "queue")
	if want := map[string]string{"cap": "5 3 2", "queue": "10 null null"}; !maps.Equal(stated, want) {
		t.Errorf("the reads state (window compare change) %v, want %v: the newest reading at each range's end, of either col", stated, want)
	}
	early := stepValueReads(t, rig, "EVL", "admission", evalDaysEnding(7, day(-14)), evalDaysEnding(7, day(-21)), "cap")
	if want := map[string]string{"cap": "null null null"}; !maps.Equal(early, want) {
		t.Errorf("a window from day -21, before cap's first day, states %v, want %v: no data before the series' first day", early, want)
	}
	logReadings(t, rig.handler, "EVL", "admission", "step-cleared", stepReading("queue", "limit", "e", nil, day(-2)))
	cleared := stepValueReads(t, rig, "EVL", "admission", window, compare, "cap", "queue")
	if want := map[string]string{"cap": "5 3 2", "queue": "null null null"}; !maps.Equal(cleared, want) {
		t.Errorf("after a reading with no value the reads state %v, want %v: a reading with no value ends the line", cleared, want)
	}
	t.Logf("the window %s..%s and the compare range %s..%s state %v, and after queue's limit is cleared %v",
		window.From.Format(time.DateOnly), window.To.Format(time.DateOnly), compare.From.Format(time.DateOnly), compare.To.Format(time.DateOnly), stated, cleared)
}

// TestALastSeriesWithNoStepChartReadsNoValueForAnEmptyWindow (dispatch://INFRA-2918, D20 (b) and
// (c), k26): a `last` series no step chart draws keeps the day rows' answer. depth holds 5 on day
// -10 and is drawn by an xy with no curve, beside a step chart of cap, which draws no step of
// depth: the window of the 7 days before today, which holds no reading of depth, states no value
// of it, and, since no zero row follows a `last` series' reading, holds no day row of it either: its
// n is null and its days hold no point. The compare range of the 3 days from the reading's day to
// the window, which holds the reading, states 5. With the zero fill of every series, the readings
// write left a zero row on each day from the reading's to today, and the window stated n 0 and a
// point of each day.
func TestALastSeriesWithNoStepChartReadsNoValueForAnEmptyWindow(t *testing.T) {
	rig := newHubRig(t, nil)
	createTestProject(t, rig.handler, "EVL")
	today := measures.Midnight(rig.clock.Now())
	day := func(offset int) time.Time { return today.AddDate(0, 0, offset) }
	registerLastSeries(t, rig.handler, "EVL", "admission", []string{"depth", "cap"},
		chartList(`{"id":"depth","title":"Depth","kind":"xy","y":{"series":"depth"}}`, stepChart("cap", "cap", "")))
	logReadings(t, rig.handler, "EVL", "admission", "depth-readings",
		stepReading("depth", "", "a", new(5.0), day(-10)), stepReading("cap", "", "b", new(2.0), day(-10)))
	window, compare := evalDaysEnding(7, today), evalDaysEnding(3, day(-7))

	stated := stepValueReads(t, rig, "EVL", "admission", window, compare, "depth", "cap")
	if want := map[string]string{"depth": "null 5 null", "cap": "2 2 0"}; !maps.Equal(stated, want) {
		t.Errorf("the reads state (window compare change) %v, want %v: no value of depth for the window that holds none of its readings", stated, want)
	}
	list := evalGet[evalListResponse](t, rig.handler, "/api/v1/projects/EVL/evals?"+rangesQuery(window, compare).Encode())
	depth := seriesValues(t, findEval(t, list.Items, "EVL", "admission"), "depth")
	if n := depth.Window["n"]; n != nil || len(depth.Days) != 0 {
		t.Errorf("depth's window states n %s and the days %s, want n null and no day: no zero row follows the reading of a last series",
			showFigure(n), mustJSON(t, depth.Days))
	}
	if zeros := countRows(t, rig.database, `select count(*) from eval_days where eval_id = 'EVL/admission' and n = 0`); zeros != 0 {
		t.Errorf("the eval holds %d zero rows, want none: each of its series folds by its last value", zeros)
	}
	t.Logf("depth states %s over the window and %s over the compare range, its window n %s and %d days", showFigure(depth.Window["value"]),
		showFigure(depth.Compare["value"]), showFigure(depth.Window["n"]), len(depth.Days))
}
