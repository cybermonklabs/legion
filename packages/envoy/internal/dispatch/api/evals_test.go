package api

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// evalEnvelope is the four members every eval route answers (R7).
var evalEnvelope = []string{"items", "limit", "offset", "total"}

// evalItemFields is the field set of every items row, whatever its kind (R7).
var evalItemFields = []string{"cols", "href", "links", "measured_at", "range", "row_key", "row_kind", "source"}

// evalRequest sends body to target as alice, under key where key is not empty.
func evalRequest(t *testing.T, handler http.Handler, method, target string, body any, key string) *httptest.ResponseRecorder {
	t.Helper()
	return keyedRequest(t, handler, method, target, body, "alice", key)
}

// evalGet reads target as alice and decodes its 200 answer as T.
func evalGet[T any](t *testing.T, handler http.Handler, target string) T {
	t.Helper()
	response := evalRequest(t, handler, http.MethodGet, target, nil, "")
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d, body %s", target, response.Code, response.Body.String())
	}
	return decodeBody[T](t, response)
}

// checkEvalEnvelope fails unless body is the envelope: a JSON object holding items, an array, and
// total, limit and offset, each a whole number; it answers how many items the page holds.
func checkEvalEnvelope(t *testing.T, what string, body []byte) int {
	t.Helper()
	var members map[string]json.RawMessage
	if err := json.Unmarshal(body, &members); err != nil {
		t.Errorf("%s answers %.120s, which is no JSON object: %v", what, body, err)
		return 0
	}
	for _, member := range evalEnvelope {
		if _, held := members[member]; !held {
			t.Errorf("%s answers no %q among %v", what, member, slices.Sorted(maps.Keys(members)))
		}
	}
	var items []json.RawMessage
	if err := json.Unmarshal(members["items"], &items); err != nil || items == nil {
		t.Errorf("%s answers items %.80s, which is no array: %v", what, members["items"], err)
	}
	for _, member := range []string{"total", "limit", "offset"} {
		var number int
		if err := json.Unmarshal(members[member], &number); err != nil {
			t.Errorf("%s answers %s %.40s, which is no whole number: %v", what, member, members[member], err)
		}
	}
	return len(items)
}

// evalRefusal is a refusal of an eval route: its code, its message and, for a readings write, the
// refused rows.
type evalRefusal struct {
	Code  string           `json:"code"`
	Error string           `json:"error"`
	Rows  []evals.RowError `json:"rows"`
}

// findEval is the item of slug among a list read's items, failing where there is not exactly one.
func findEval(t *testing.T, items []evalListItem, project, slug string) evalItem {
	t.Helper()
	var found []evalItem
	for _, item := range items {
		if item.Definition.ProjectKey == project && item.Definition.Slug == slug {
			found = append(found, item.evalItem)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d items hold %s/%s, want one", len(found), project, slug)
	}
	return found[0]
}

// seriesValues is the series name of item, failing where it states none.
func seriesValues(t *testing.T, item evalItem, name string) evalSeriesValues {
	t.Helper()
	for _, series := range item.Series {
		if series.Series.Series == name {
			return series
		}
	}
	t.Fatalf("%s states no series %s", item.Definition.EvalID, name)
	return evalSeriesValues{}
}

// reading is one row of a readings write.
func reading(series string, value float64, at time.Time) map[string]any {
	return map[string]any{"series": series, "row_kind": "sample", "row_key": "run-" + fmt.Sprint(value), "value": value, "measured_at": at.Format(time.RFC3339Nano)}
}

// TestEveryEvalRouteAnswersTheEnvelope (k7): each of the four reads, with a window, a compare
// range, a limit and an offset, and each of the two writes answers {items, total, limit, offset},
// and a page of the reads holds the row the writes made.
func TestEveryEvalRouteAnswersTheEnvelope(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	createTestProject(t, handler, "EVL")
	now := time.Now().UTC()
	written := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/EVL/evals/pr-size/readings",
		map[string]any{"readings": []any{reading("lines", 120, now.Add(-time.Hour))}}, "envelope-readings")
	if written.Code != http.StatusCreated {
		t.Fatalf("log a reading: status %d, body %s", written.Code, written.Body.String())
	}
	registered := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/EVL/evals",
		map[string]any{"slug": "pr-size", "name": "PR size", "unit": "lines"}, "envelope-register")
	if registered.Code != http.StatusCreated {
		t.Fatalf("register: status %d, body %s", registered.Code, registered.Body.String())
	}
	query := url.Values{
		"from": {now.Add(-24 * time.Hour).Format(time.RFC3339)}, "to": {now.Add(time.Hour).Format(time.RFC3339)},
		"compare_from": {now.Add(-72 * time.Hour).Format(time.RFC3339)}, "compare_to": {now.Add(-48 * time.Hour).Format(time.RFC3339)},
		"limit": {"50"}, "offset": {"0"},
	}.Encode()
	answers := map[string][]byte{
		"POST readings": written.Body.Bytes(),
		"POST register": registered.Body.Bytes(),
	}
	for _, path := range []string{
		"/api/v1/projects/EVL/evals?" + query,
		"/api/v1/evals?project=EVL&" + query,
		"/api/v1/projects/EVL/evals/pr-size?" + query,
		"/api/v1/projects/EVL/evals/pr-size/items?" + query,
	} {
		response := evalRequest(t, handler, http.MethodGet, path, nil, "")
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d, body %s", path, response.Code, response.Body.String())
		}
		answers["GET "+path] = response.Body.Bytes()
	}
	for _, what := range slices.Sorted(maps.Keys(answers)) {
		held := checkEvalEnvelope(t, what, answers[what])
		if held == 0 {
			t.Errorf("%s holds no item, want the eval or the reading the writes made", what)
		}
		t.Logf("%s: the envelope, %d items", what, held)
	}
}

// TestTheEvalRoutesRefuseWithStableCodes (k7, k4, k9): each bad request is refused with its stable
// code, a readings write refused for a row names the row by its index, and no refused write
// stores a reading or a version. A readings write carrying the global session parameter alone is
// accepted.
func TestTheEvalRoutesRefuseWithStableCodes(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "EVL")
	at := time.Now().UTC().Add(-time.Hour)
	good := reading("minutes", 12, at)
	withKey := func(extra map[string]any) map[string]any {
		row := maps.Clone(good)
		maps.Copy(row, extra)
		return row
	}
	readings := "/api/v1/projects/EVL/evals/review-time/readings"
	cases := []struct {
		name         string
		method, path string
		body         any
		key          string
		status       int
		code         string
		names        string
		rowIndex     int
	}{
		{name: "a window that is no RFC3339 time", method: http.MethodGet, path: "/api/v1/projects/EVL/evals?from=yesterday", status: 400, code: "INVALID_QUERY", names: "from"},
		{name: "a compare range given by half", method: http.MethodGet, path: "/api/v1/projects/EVL/evals?compare_from=2026-11-01T00:00:00Z", status: 400, code: "INVALID_QUERY", names: "compare_to"},
		{name: "a window over 84 days", method: http.MethodGet, path: "/api/v1/projects/EVL/evals?from=2026-01-01T00:00:00Z&to=2026-11-01T00:00:00Z", status: 400, code: "INVALID_QUERY", names: "84 days"},
		{name: "a compare range over 84 days", method: http.MethodGet, path: "/api/v1/evals?compare_from=2026-01-01T00:00:00Z&compare_to=2026-11-01T00:00:00Z", status: 400, code: "INVALID_QUERY", names: "compare_from"},
		{name: "a kind no eval has", method: http.MethodGet, path: "/api/v1/projects/EVL/evals?kind=weather", status: 400, code: "INVALID_QUERY", names: "weather"},
		{name: "a project no row holds", method: http.MethodGet, path: "/api/v1/projects/NONE/evals", status: 404, code: "PROJECT_NOT_FOUND"},
		{name: "a project filter no row holds", method: http.MethodGet, path: "/api/v1/evals?project=EVL&project=NONE", status: 404, code: "PROJECT_NOT_FOUND", names: "NONE"},
		{name: "an unknown slug's versions", method: http.MethodGet, path: "/api/v1/projects/EVL/evals/no-such-eval", status: 404, code: codeEvalNotFound},
		{name: "an unknown slug's items", method: http.MethodGet, path: "/api/v1/projects/EVL/evals/no-such-eval/items", status: 404, code: codeEvalNotFound},
		{name: "a sort no table has", method: http.MethodGet, path: "/api/v1/projects/EVL/evals/pr-ci/items?sort=fastest", status: 400, code: "INVALID_QUERY", names: "fastest"},
		{name: "a readings write with a window", method: http.MethodPost, path: readings + "?from=2026-11-01T00:00:00Z", body: map[string]any{"readings": []any{good}}, key: "r-query", status: 400, code: "INVALID_QUERY", names: `"from"`},
		{name: "a readings write with no Idempotency-Key", method: http.MethodPost, path: readings, body: map[string]any{"readings": []any{good}}, status: 400, code: codeIdempotencyKeyRequired},
		{name: "a reading carrying recorded_by", method: http.MethodPost, path: readings, body: map[string]any{"readings": []any{withKey(map[string]any{"recorded_by": map[string]any{"kind": "user", "id": "mallory"}})}}, key: "r-by", status: 400, code: evals.CodeFieldServerSet, names: "recorded_by"},
		{name: "a reading carrying recorded_at", method: http.MethodPost, path: readings, body: map[string]any{"readings": []any{withKey(map[string]any{"recorded_at": at.Format(time.RFC3339)})}}, key: "r-at", status: 400, code: evals.CodeFieldServerSet, names: "recorded_at"},
		{name: "a batch whose second row has no value", method: http.MethodPost, path: readings, body: map[string]any{"readings": []any{good, map[string]any{"series": "minutes", "row_kind": "sample", "measured_at": at.Format(time.RFC3339)}}}, key: "r-row", status: 400, code: evals.CodeValueOrReason, rowIndex: 1},
		{name: "a reading's link with a relative href", method: http.MethodPost, path: readings, body: map[string]any{"readings": []any{withKey(map[string]any{"links": []any{map[string]any{"rel": "via", "href": "/runs/1"}}})}}, key: "r-link", status: 400, code: evals.CodeLinkHrefNotAbsolute},
		{name: "a register write's link with a relative href", method: http.MethodPost, path: "/api/v1/projects/EVL/evals", body: map[string]any{"slug": "review-time", "links": []any{map[string]any{"rel": "describedby", "href": "docs/method.md"}}}, key: "g-link", status: 400, code: evals.CodeLinkHrefNotAbsolute},
		{name: "a PUT of a reading", method: http.MethodPut, path: readings + "/5d4a3b2c-1e0f-4a9b-8c7d-6e5f4a3b2c1d", body: good, status: 405, code: codeReadingAppendOnly},
		{name: "a DELETE of a reading", method: http.MethodDelete, path: readings + "/5d4a3b2c-1e0f-4a9b-8c7d-6e5f4a3b2c1d", status: 405, code: codeReadingAppendOnly},
	}
	countReadings := `select count(*) from eval_readings where eval_id like 'EVL/%'`
	countVersions := `select count(*) from eval_versions where project_key = 'EVL'`
	readingsBefore, versionsBefore := countRows(t, database, countReadings), countRows(t, database, countVersions)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			response := evalRequest(t, handler, c.method, c.path, c.body, c.key)
			refusal := decodeBody[evalRefusal](t, response)
			if response.Code != c.status || refusal.Code != c.code {
				t.Fatalf("%s %s: %d %s, want %d %s", c.method, c.path, response.Code, response.Body.String(), c.status, c.code)
			}
			// A row's refusal names its field in the row (rows[i].field); every other refusal in
			// its message.
			named := refusal.Error
			for _, row := range refusal.Rows {
				named += " " + row.Field + " " + row.Message
			}
			if c.names != "" && !strings.Contains(named, c.names) {
				t.Errorf("the refusal %q, rows %+v, does not name %s", refusal.Error, refusal.Rows, c.names)
			}
			if c.rowIndex > 0 && (len(refusal.Rows) != 1 || refusal.Rows[0].Index != c.rowIndex || refusal.Rows[0].Code != c.code) {
				t.Errorf("the refusal names rows %+v, want row %d alone, %s", refusal.Rows, c.rowIndex, c.code)
			}
			t.Logf("%s %s: %d %s %q rows %+v", c.method, c.path, response.Code, refusal.Code, refusal.Error, refusal.Rows)
		})
	}
	readingsAfter, versionsAfter := countRows(t, database, countReadings), countRows(t, database, countVersions)
	if readingsAfter != readingsBefore || versionsAfter != versionsBefore {
		t.Errorf("the refused requests changed the readings %d -> %d and the versions %d -> %d, want neither moved",
			readingsBefore, readingsAfter, versionsBefore, versionsAfter)
	}
	t.Logf("%d refusals; readings %d -> %d, versions %d -> %d", len(cases), readingsBefore, readingsAfter, versionsBefore, versionsAfter)

	// The global session parameter is the one query key the write takes.
	accepted := evalRequest(t, handler, http.MethodPost, readings+"?session=s-1", map[string]any{"readings": []any{good}}, "r-session")
	if accepted.Code != http.StatusCreated {
		t.Fatalf("a readings write carrying ?session= alone: status %d, body %s, want 201", accepted.Code, accepted.Body.String())
	}
	if got := countRows(t, database, countReadings); got != readingsAfter+1 {
		t.Errorf("the write with ?session= stored %d readings, want 1", got-readingsAfter)
	}
}

// TestAReadingsBatchRepeatedUnderItsKeyIsWrittenOnce (k4): a batch of three under one key stores
// three readings; the repeat under that key stores none and is answered the first answer, marked
// replayed; the same batch under a fresh key stores three more, the negative control; and a
// correction stores a new reading and leaves the one it replaces stored, superseded, and out of the
// items read.
func TestAReadingsBatchRepeatedUnderItsKeyIsWrittenOnce(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "EVL")
	at := time.Now().UTC().Add(-time.Hour)
	batch := map[string]any{"readings": []any{reading("minutes", 10, at), reading("minutes", 15, at), reading("minutes", 40, at)}}
	path := "/api/v1/projects/EVL/evals/review-time/readings"
	count := func() int64 {
		return countRows(t, database, `select count(*) from eval_readings where eval_id = 'EVL/review-time'`)
	}

	first := evalRequest(t, handler, http.MethodPost, path, batch, "batch-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("the batch: status %d, body %s", first.Code, first.Body.String())
	}
	afterFirst := count()
	repeat := evalRequest(t, handler, http.MethodPost, path, batch, "batch-1")
	afterRepeat := count()
	if repeat.Code != http.StatusCreated || repeat.Header().Get(contracts.IdempotencyReplayedHeader) == "" {
		t.Fatalf("the repeat: status %d, %s %q, want 201 replayed", repeat.Code, contracts.IdempotencyReplayedHeader,
			repeat.Header().Get(contracts.IdempotencyReplayedHeader))
	}
	if repeat.Body.String() != first.Body.String() {
		t.Errorf("the repeat answered %s, want the first answer %s", repeat.Body.String(), first.Body.String())
	}
	fresh := evalRequest(t, handler, http.MethodPost, path, batch, "batch-2")
	afterFresh := count()
	if fresh.Code != http.StatusCreated || afterFirst != 3 || afterRepeat != 3 || afterFresh != 6 {
		t.Fatalf("readings after the batch %d, after its repeat %d, after the batch under a fresh key %d (status %d); want 3, 3, 6",
			afterFirst, afterRepeat, afterFresh, fresh.Code)
	}

	written := decodeBody[evalReadingsResponse](t, first)
	replaced := written.Items[2].ID
	correction := evalRequest(t, handler, http.MethodPost, path, map[string]any{"readings": []any{
		map[string]any{"series": "minutes", "row_kind": "sample", "row_key": "run-40", "value": 4, "measured_at": at.Format(time.RFC3339Nano), "replaces": replaced},
	}}, "correction-1")
	if correction.Code != http.StatusCreated {
		t.Fatalf("the correction: status %d, body %s", correction.Code, correction.Body.String())
	}
	superseded := countRows(t, database, `select count(*) from eval_readings where id = $1 and superseded_at is not null`, replaced)
	// run-40 stands twice: the fresh batch's own reading, and the correction of the first batch's.
	items := evalGet[evalItemsResponse](t, handler, "/api/v1/projects/EVL/evals/review-time/items?row_kind=sample&row=run-40")
	if count() != 7 || superseded != 1 || items.Total != 2 {
		t.Errorf("after the correction %d readings, the replaced one superseded %d, %d standing under run-40 (items %+v); want 7, 1, 2",
			count(), superseded, items.Total, items.Items)
	}
	t.Logf("readings: %d after the batch, %d after its repeat (replayed), %d after a fresh key, %d after a correction; the replaced reading superseded",
		afterFirst, afterRepeat, afterFresh, count())
}

// TestAReadingUnderAnUnknownSlugRegistersItsEvalOnce (k3, k5): readings under a slug no eval holds
// register exactly one eval with the caller as owner, no target and state active; the register
// write then adds the target, the unit, the direction, the name and the method, the items route
// still answers every reading logged before it, and the eval's read answers the method's text. A
// reading under a computed eval's slug is stored and answered by its items, and the computed value
// is the same before and after it.
func TestAReadingUnderAnUnknownSlugRegistersItsEvalOnce(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "EVL")
	now := time.Now().UTC()
	today := utcDayOf(now)
	window := url.Values{"from": {today.Format(time.RFC3339)}, "to": {today.AddDate(0, 0, 1).Format(time.RFC3339)}}.Encode()
	for index, value := range []float64{0.8, 0.9} {
		response := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/EVL/evals/cache-hits/readings",
			map[string]any{"readings": []any{reading("share", value, now.Add(-time.Minute))}}, fmt.Sprintf("cache-%d", index))
		if response.Code != http.StatusCreated {
			t.Fatalf("reading %d: status %d, body %s", index, response.Code, response.Body.String())
		}
		if got := decodeBody[evalReadingsResponse](t, response).Registered; got != (index == 0) {
			t.Errorf("reading %d answered registered %t, want %t", index, got, index == 0)
		}
	}
	list := evalGet[evalListResponse](t, handler, "/api/v1/projects/EVL/evals?kind=experiment&"+window)
	item := findEval(t, list.Items, "EVL", "cache-hits")
	versions := countRows(t, database, `select count(*) from eval_versions where eval_id = 'EVL/cache-hits'`)
	definition := item.Definition
	if versions != 1 || definition.Target != nil || definition.State != evals.StateActive || item.Owner.Person != "alice" ||
		item.Owner.WrittenBy.ID != "alice" || definition.WrittenVia != evals.ViaReading {
		t.Fatalf("the slug holds %d versions, %+v owned by %+v; want one, no target, active, alice's, written by a reading",
			versions, definition, item.Owner)
	}

	method := "The share of cache reads served without a fetch, one reading per run."
	registered := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/EVL/evals", map[string]any{
		"slug": "cache-hits", "name": "Cache hits", "unit": "ratio", "direction": "higher_better",
		"target": 0.85, "target_inclusive": true, "doc_text": method,
	}, "cache-register")
	if registered.Code != http.StatusCreated {
		t.Fatalf("register: status %d, body %s", registered.Code, registered.Body.String())
	}
	// The readings alone: the items of a logged eval also answer its day rows (row_kind day).
	items := evalGet[evalItemsResponse](t, handler, "/api/v1/projects/EVL/evals/cache-hits/items?row_kind=sample&"+window)
	detail := evalGet[evalDetailResponse](t, handler, "/api/v1/projects/EVL/evals/cache-hits?"+window)
	newest := detail.Items[0]
	if items.Total != 2 || detail.DocText == nil || *detail.DocText != method || newest.Definition.Target == nil ||
		*newest.Definition.Target != 0.85 || newest.Definition.Unit != "ratio" || newest.Definition.Name != "Cache hits" {
		t.Fatalf("after registration the items answer %d readings, the method %v, the newest version %+v; want 2, %q, the registered fields",
			items.Total, detail.DocText, newest.Definition, method)
	}
	t.Logf("cache-hits: %d version before registration, %d readings answered after it, method %q, verdict %+v",
		versions, items.Total, *detail.DocText, newest.Verdict)

	// A computed eval with a day row of its own: three deploys today.
	if _, err := database.Pool.Exec(t.Context(), `
		insert into eval_days (project_key, eval_id, series, repo, day, listed, n, sum, counts, built_by)
		values ('EVL', 'EVL/deploy-frequency', 'deploys', '', $1, true, 3, 3, '{"with_prs": 2}', 'test')
	`, today); err != nil {
		t.Fatalf("write deploy-frequency's day row: %v", err)
	}
	value := func() string {
		list := evalGet[evalListResponse](t, handler, "/api/v1/projects/EVL/evals?kind=delivery&"+window)
		encoded, err := json.Marshal(seriesValues(t, findEval(t, list.Items, "EVL", evals.SlugDeployFrequency), "deploys").Window)
		if err != nil {
			t.Fatalf("encode deploy-frequency's statistics: %v", err)
		}
		return string(encoded)
	}
	before := value()
	logged := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/EVL/evals/deploy-frequency/readings",
		map[string]any{"readings": []any{reading("deploys", 500, now.Add(-time.Minute))}}, "computed-reading")
	if logged.Code != http.StatusCreated || decodeBody[evalReadingsResponse](t, logged).Registered {
		t.Fatalf("a reading under deploy-frequency: status %d, body %s, want 201 registering nothing", logged.Code, logged.Body.String())
	}
	answered := evalGet[evalItemsResponse](t, handler, "/api/v1/projects/EVL/evals/deploy-frequency/items?row_kind=sample&"+window)
	after := value()
	if answered.Total != 1 || before != after || !strings.Contains(before, `"deploys":3`) {
		t.Errorf("the reading under deploy-frequency is answered by %d items; the value read %s before it and %s after; want 1 item and 3 deploys both times",
			answered.Total, before, after)
	}
	t.Logf("deploy-frequency's deploys before the reading %s, after it %s; its items answer the reading %d time(s)", before, after, answered.Total)
}

// TestAnEvalsVersionsAreAnsweredNewestFirst (k1, k5): a register write, a definition change and a
// target change each write a version, a write that changes nothing writes none, and the eval's read
// answers the versions newest first, the newest with the window's values and its verdict, and the
// method its section holds.
func TestAnEvalsVersionsAreAnsweredNewestFirst(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "EVL")
	register := func(key string, body map[string]any, status int) {
		t.Helper()
		body["slug"] = "review-time"
		response := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/EVL/evals", body, key)
		if response.Code != status {
			t.Fatalf("register %v: status %d, body %s, want %d", body, response.Code, response.Body.String(), status)
		}
		checkEvalEnvelope(t, "the register write", response.Body.Bytes())
	}
	register("v1", map[string]any{
		"name": "Review time", "unit": "minutes", "direction": "lower_better", "target": 30, "target_inclusive": false,
		"series": []any{map[string]any{"series": "minutes", "agg": "quantile", "unit": "minutes"}},
	}, http.StatusCreated)
	register("v2", map[string]any{"name": "Review time (median)"}, http.StatusCreated)
	register("v3", map[string]any{"target": 20}, http.StatusCreated)
	register("v3-again", map[string]any{"target": 20}, http.StatusOK)
	method := "The minutes from a review's request to its first answer."
	register("v4", map[string]any{"doc_text": method}, http.StatusCreated)

	now := time.Now().UTC()
	today := utcDayOf(now)
	logged := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/EVL/evals/review-time/readings", map[string]any{
		"readings": []any{reading("minutes", 10, now.Add(-time.Minute)), reading("minutes", 15, now.Add(-time.Minute)), reading("minutes", 40, now.Add(-time.Minute))},
	}, "minutes")
	if logged.Code != http.StatusCreated {
		t.Fatalf("log readings: status %d, body %s", logged.Code, logged.Body.String())
	}
	detail := evalGet[evalDetailResponse](t, handler, "/api/v1/projects/EVL/evals/review-time?from="+today.Format(time.RFC3339))
	stored := countRows(t, database, `select count(*) from eval_versions where eval_id = 'EVL/review-time'`)
	var numbers, targets []string
	for _, item := range detail.Items {
		numbers = append(numbers, fmt.Sprint(item.Definition.Version))
		targets = append(targets, fmt.Sprint(*item.Definition.Target))
	}
	if stored != 4 || detail.Total != 4 || !slices.Equal(numbers, []string{"4", "3", "2", "1"}) ||
		!slices.Equal(targets, []string{"20", "20", "30", "30"}) {
		t.Fatalf("%d versions stored, the read answers %d: versions %v, targets %v; want 4 newest first, targets 20 20 30 30", stored, detail.Total, numbers, targets)
	}
	newest := detail.Items[0]
	minutes := seriesValues(t, newest, "minutes")
	if newest.Verdict == nil || newest.Verdict.Met == nil || !*newest.Verdict.Met || minutes.Window["median"] == nil || *minutes.Window["median"] != 15 {
		t.Errorf("the newest version reads %+v with the verdict %+v, want the median 15 under the target 20, met", minutes.Window, newest.Verdict)
	}
	for _, older := range detail.Items[1:] {
		if older.Series != nil || older.Verdict != nil {
			t.Errorf("version %d answers values, want only the newest to", older.Definition.Version)
		}
	}
	if detail.DocText == nil || *detail.DocText != method {
		t.Errorf("the read answers the method %v, want %q", detail.DocText, method)
	}
	t.Logf("%d versions newest first %v, targets %v; the newest reads median %v, verdict met %v; method %q",
		detail.Total, numbers, targets, *minutes.Window["median"], *newest.Verdict.Met, *detail.DocText)
}

// TestAProjectScopedCredentialWritesNoOtherProjectsEval (k3's negative control): a broker token
// scoped to one project is refused 403 PROJECT_SCOPE for a readings write and a register write of
// another project's eval, and neither stores a row; the same readings write in its own project is
// answered by the handler.
func TestAProjectScopedCredentialWritesNoOtherProjectsEval(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-scoped-to-one-project"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	for _, key := range []string{brokerTestProject, "OTH"} {
		if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": key, "name": key}); created.Code != http.StatusCreated {
			t.Fatalf("create project %s: status %d, body %s", key, created.Code, created.Body.String())
		}
	}
	body := map[string]any{"readings": []any{reading("hits", 3, time.Now().UTC().Add(-time.Minute))}}
	send := func(method, path string, body any, key string) *httptest.ResponseRecorder {
		request := caller.presenting(t, method, path, token, body)
		request.Header.Set(contracts.IdempotencyKeyHeader, key)
		return rig.serve(request)
	}
	for _, write := range []struct {
		path string
		body any
	}{
		{"/api/v1/projects/OTH/evals/scoped-hits/readings", body},
		{"/api/v1/projects/OTH/evals", map[string]any{"slug": "scoped-hits", "name": "Scoped hits"}},
	} {
		refused := send(http.MethodPost, write.path, write.body, "scoped-"+write.path)
		if refusal := decodeBody[evalRefusal](t, refused); refused.Code != http.StatusForbidden || refusal.Code != codeProjectScope {
			t.Fatalf("POST %s: status %d, body %s, want 403 %s", write.path, refused.Code, refused.Body.String(), codeProjectScope)
		}
	}
	stored := countRows(t, rig.database, `select count(*) from eval_versions where eval_id = 'OTH/scoped-hits'`)
	readings := countRows(t, rig.database, `select count(*) from eval_readings where eval_id = 'OTH/scoped-hits'`)
	own := send(http.MethodPost, "/api/v1/projects/"+brokerTestProject+"/evals/scoped-hits/readings", body, "scoped-own")
	if stored != 0 || readings != 0 || own.Code != http.StatusCreated {
		t.Fatalf("the refused writes stored %d versions and %d readings, and the write to %s answered %d %s; want none, none, 201",
			stored, readings, brokerTestProject, own.Code, own.Body.String())
	}
	t.Logf("both writes to OTH refused %s, storing nothing; the same readings write to %s answered %d", codeProjectScope, brokerTestProject, own.Code)
}

// goldenLegionWindow is what this package reads of one of legion/metrics' golden files: the
// history it was recorded over, its window and, by scope, the figures the base head's Legion
// section answered for it.
type goldenLegionWindow struct {
	Spec struct {
		Project string    `json:"project"`
		Trees   int       `json:"trees"`
		Now     time.Time `json:"now"`
	} `json:"spec"`
	Window struct {
		From time.Time `json:"from"`
		To   time.Time `json:"to"`
	} `json:"window"`
	Reports map[string]struct {
		Finished int `json:"finished"`
		WorkTime struct {
			MedianHours float64           `json:"median_hours"`
			P90Hours    float64           `json:"p90_hours"`
			Issues      []json.RawMessage `json:"issues"`
		} `json:"work_time"`
		PhaseTime struct {
			Bars []struct {
				Role                string  `json:"role"`
				Hours               float64 `json:"hours"`
				Issues              int     `json:"issues"`
				MedianHoursPerIssue float64 `json:"median_hours_per_issue"`
			} `json:"bars"`
			IdleShare float64 `json:"idle_share"`
		} `json:"phase_time"`
		Rework struct {
			MedianReviewRounds    float64 `json:"median_review_rounds"`
			SentBackShare         float64 `json:"sent_back_share"`
			HoursAfterFirstReview float64 `json:"hours_after_first_review"`
		} `json:"rework"`
	} `json:"reports"`
}

// readGoldenLegionWindows reads every golden file of legion/metrics (golden_record_test.go
// recorded them at the base head 02a7b3c6, one legiontest history).
func readGoldenLegionWindows(t *testing.T) []goldenLegionWindow {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "legion", "metrics", "testdata", "golden_compute", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("list the golden files: %d, %v", len(files), err)
	}
	windows := make([]goldenLegionWindow, len(files))
	for index, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if err := json.Unmarshal(raw, &windows[index]); err != nil {
			t.Fatalf("decode %s: %v", file, err)
		}
	}
	return windows
}

// seedEvalHistories writes into database, behind handler, the two histories the eval reads are
// checked over, each with its evals seeded (evals.Store.Ensure) and its day rows written as the
// refresher writes them: deliverytest's (DELIV, every pull request of it, and DELIVX, which holds
// none) over days of history, and the legiontest history legion/metrics' golden files were recorded
// over (GOLDEN). It answers the delivery fixture and the golden windows.
func seedEvalHistories(t *testing.T, database *store.Store, deps Deps, days int) (deliverytest.Fixture, []goldenLegionWindow) {
	t.Helper()
	ctx := t.Context()
	registry := evals.New(database, deps.Docs, deps.Events)
	fixture := deliverytest.Seed(t, database.Pool, deliverytest.Spec{Seed: 2916, OneProject: true, Days: days})
	settings, err := delivery.GetSettings(ctx, database.Pool)
	if err != nil {
		t.Fatalf("read the delivery settings: %v", err)
	}
	for day := fixture.From; day.Before(fixture.To); day = day.AddDate(0, 0, 1) {
		if _, err := registry.WriteDeliveryDay(ctx, settings, day, "", testServerBuild); err != nil {
			t.Fatalf("write the delivery rows of %s: %v", day.Format(time.DateOnly), err)
		}
	}

	windows := readGoldenLegionWindows(t)
	spec := windows[0].Spec
	history := legiontest.Seed(t, database.Pool, legiontest.Spec{Project: spec.Project, Trees: spec.Trees, Now: spec.Now})
	rows, err := legionfacts.ComputeProject(ctx, database.Pool, history.Project, spec.Now)
	if err != nil {
		t.Fatalf("compute the facts of %s: %v", history.Project, err)
	}
	if _, err := legionfacts.WriteProject(ctx, database.Pool, legionfacts.ProjectWrite{Project: history.Project, Rows: rows, Whole: true}); err != nil {
		t.Fatalf("write the facts of %s: %v", history.Project, err)
	}
	population, err := metrics.ReadProject(ctx, database.Pool, nil, history.Project, spec.Now)
	if err != nil {
		t.Fatalf("read the population of %s: %v", history.Project, err)
	}
	var filings []legionfacts.Filing
	seen := map[string]bool{}
	for _, filed := range metrics.ProjectFilings(population) {
		if !seen[filed.Key] {
			seen[filed.Key] = true
			filings = append(filings, legionfacts.Filing{
				Project: history.Project, Issue: filed.Key, FiledAt: filed.At, FilerRole: filed.Filer, FilerIssue: filed.FilerIssue,
				FilerPhase: filed.FilerPhase, Class: filed.Class, RootKey: filed.Root,
			})
		}
	}
	if err := legionfacts.WriteFilings(ctx, database.Pool, legionfacts.FilingsWrite{Project: history.Project, Filings: filings, Whole: true}); err != nil {
		t.Fatalf("write the filings of %s: %v", history.Project, err)
	}
	since, err := registry.ReadLegionSince(ctx, history.Project)
	if err != nil {
		t.Fatalf("read the first observations of %s: %v", history.Project, err)
	}
	for day := utcDayOf(history.From); day.Before(spec.Now); day = day.AddDate(0, 0, 1) {
		if err := registry.WriteLegionDay(ctx, history.Project, day, since, testServerBuild); err != nil {
			t.Fatalf("write the Legion rows of %s: %v", day.Format(time.DateOnly), err)
		}
	}
	for _, project := range []string{fixture.Project, fixture.SecondProject, history.Project} {
		if _, err := registry.Ensure(ctx, project); err != nil {
			t.Fatalf("seed the evals of %s: %v", project, err)
		}
	}
	return fixture, windows
}

// nearly reports whether got is want within tolerance, a nil got standing for want's absence.
func nearly(got *float64, want, tolerance float64) bool {
	return got != nil && math.Abs(*got-want) <= tolerance
}

// TestTheEvalReadsFoldTheStoredDayRows (k7's one fold, k11, k12, R12, R16): over a project with
// written day rows, the list read answers for one window the numbers the delivery routes answer
// (GET /api/v1/delivery/measures and /pipeline, which build the same parts from the population and
// fold them through the same folds), and the Legion evals answer the numbers the base head's
// Legion section answered (legion/metrics' golden files); a series with no day row reads null and
// names no first day, and a window that starts before a series' first day reads null and names it
// while its per-day array answers the days that exist; GET /api/v1/evals answers two projects, one,
// and with none named every project the store holds, each item equal to the project read's; and the
// items route answers one field set for every row kind.
func TestTheEvalReadsFoldTheStoredDayRows(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	fixture, windows := seedEvalHistories(t, database, deps, 14)

	// The delivery evals against the delivery routes, over seven whole days of the history.
	from, to := fixture.From.AddDate(0, 0, 4), fixture.From.AddDate(0, 0, 11)
	window := url.Values{"from": {from.Format(time.RFC3339)}, "to": {to.Format(time.RFC3339)}}.Encode()
	list := evalGet[evalListResponse](t, handler, "/api/v1/projects/"+fixture.Project+"/evals?limit=200&"+window)
	if list.Window.From != from || list.Window.To != to || list.Window.Days != 7 {
		t.Fatalf("the list states the window %+v, want %s..%s, 7 days", list.Window, from, to)
	}
	route := getDeliveryMeasures(t, handler, window).Measures
	pipeline := evalGet[deliveryPipelineResponse](t, handler, "/api/v1/delivery/pipeline?"+window)
	stat := func(slug, series, name string) *float64 {
		return seriesValues(t, findEval(t, list.Items, fixture.Project, slug), series).Window[name]
	}
	compared := 0
	same := func(what string, got *float64, want float64) {
		t.Helper()
		compared++
		if !nearly(got, want, 1e-9) {
			t.Errorf("%s: the eval read answers %s, the delivery route %v", what, showFigure(got), want)
		}
	}
	sameOptional := func(what string, got, want *float64) {
		t.Helper()
		compared++
		if (got == nil) != (want == nil) || (got != nil && !nearly(got, *want, 1e-9)) {
			t.Errorf("%s: the eval read answers %s, the delivery route %s", what, showFigure(got), showFigure(want))
		}
	}
	frequency := route.DeployFrequency
	same("deploy-frequency per_day", stat(evals.SlugDeployFrequency, "deploys", "per_day"), frequency.PerDay)
	same("deploy-frequency deploys", stat(evals.SlugDeployFrequency, "deploys", "deploys"), float64(frequency.SuccessfulDeploys))
	same("deploy-frequency with_prs", stat(evals.SlugDeployFrequency, "deploys", "with_prs"), float64(frequency.DeploysWithPRs))
	same("deploy-frequency with_prs_per_day", stat(evals.SlugDeployFrequency, "deploys", "with_prs_per_day"), frequency.WithPRsPerDay)
	success := route.DeployRunSuccess
	sameOptional("deploy-run-success rate", stat(evals.SlugDeployRunSuccess, "runs", "rate"), success.Rate)
	same("deploy-run-success concluded", stat(evals.SlugDeployRunSuccess, "runs", "concluded"), float64(success.Concluded))
	same("deploy-run-success cancelled", stat(evals.SlugDeployRunSuccess, "runs", "cancelled"), float64(success.Cancelled))
	for slug, spread := range map[string]struct {
		median, p90, max *float64
	}{
		evals.SlugMergeToProduction:       {route.LeadTime.MergeToProduction.MedianMinutes, route.LeadTime.MergeToProduction.P90Minutes, route.LeadTime.MergeToProduction.MaxMinutes},
		evals.SlugFirstCommitToProduction: {route.LeadTime.FirstCommitToProduction.MedianMinutes, route.LeadTime.FirstCommitToProduction.P90Minutes, route.LeadTime.FirstCommitToProduction.MaxMinutes},
		evals.SlugOpenToProduction:        {route.LeadTime.OpenedToProduction.MedianMinutes, route.LeadTime.OpenedToProduction.P90Minutes, route.LeadTime.OpenedToProduction.MaxMinutes},
		evals.SlugPROpenToMerge:           {route.LeadTime.OpenedToMerge.MedianMinutes, route.LeadTime.OpenedToMerge.P90Minutes, route.LeadTime.OpenedToMerge.MaxMinutes},
	} {
		sameOptional(slug+" median", stat(slug, "minutes", "median"), spread.median)
		sameOptional(slug+" p90", stat(slug, "minutes", "p90"), spread.p90)
		sameOptional(slug+" max", stat(slug, "minutes", "max"), spread.max)
	}
	rework := stat(evals.SlugReworkShare, "prs", "rate")
	if rework == nil {
		rework = new(0.0)
	}
	same("rework-share rate", rework, route.ReworkShare)
	same("pr-ci n", stat(evals.SlugPRCI, "minutes", "n"), float64(pipeline.PRChecks.Run.Total.N))
	sameOptional("pr-ci median", stat(evals.SlugPRCI, "minutes", "median"), pipeline.PRChecks.Run.Total.MedianMinutes)
	sameOptional("pr-ci success_rate", stat(evals.SlugPRCI, "minutes", "success_rate"), pipeline.PRChecks.Run.Total.SuccessRate)
	if frequency.SuccessfulDeploys == 0 || route.LeadTime.MergeToProduction.MedianMinutes == nil {
		t.Fatalf("the window holds %d deploys and no merge-to-production median, so the comparison compares nothing", frequency.SuccessfulDeploys)
	}
	t.Logf("%s %s..%s: %d delivery and CI figures equal the delivery routes' (deploys %d, merge-to-production median %v)",
		fixture.Project, from.Format(time.DateOnly), to.Format(time.DateOnly), compared, frequency.SuccessfulDeploys,
		*route.LeadTime.MergeToProduction.MedianMinutes)

	// A series with no day row reads null and names no first day; a window that starts before a
	// series' first day reads null, names it, and answers the days that exist.
	failures := seriesValues(t, findEval(t, list.Items, fixture.Project, evals.SlugChangeFailRate), "per_deploy")
	if failures.Window["rate"] != nil || failures.DataSince != nil || len(failures.Days) != 0 {
		t.Errorf("change-fail-rate, which holds no day row, reads %+v since %v over %d days, want null, no first day, no day", failures.Window, failures.DataSince, len(failures.Days))
	}
	early := url.Values{"from": {fixture.From.AddDate(0, 0, -3).Format(time.RFC3339)}, "to": {fixture.From.AddDate(0, 0, 4).Format(time.RFC3339)}}.Encode()
	before := evalGet[evalListResponse](t, handler, "/api/v1/projects/"+fixture.Project+"/evals?kind=delivery&"+early)
	deploys := seriesValues(t, findEval(t, before.Items, fixture.Project, evals.SlugDeployFrequency), "deploys")
	first := fixture.From.Format(time.DateOnly)
	if deploys.DataSince == nil || *deploys.DataSince != first || deploys.Window["deploys"] != nil || len(deploys.Days) == 0 {
		t.Errorf("deploy-frequency over a window starting before its first day reads %+v since %v with %d days, want null since %s and the days that exist",
			deploys.Window, deploys.DataSince, len(deploys.Days), first)
	} else {
		t.Logf("deploy-frequency from %s: no data, since %s; %d days answered, the first %s %v", early, *deploys.DataSince, len(deploys.Days),
			deploys.Days[0].Day, showFigure(deploys.Days[0].Statistics["deploys"]))
	}

	// The Legion evals against the base head's Legion section, over a golden window the day rows
	// cover from its first day on.
	project := windows[0].Spec.Project
	checked := 0
	for _, golden := range windows {
		goldenWindow := url.Values{"from": {golden.Window.From.Format(time.RFC3339)}, "to": {golden.Window.To.Format(time.RFC3339)}}.Encode()
		legion := evalGet[evalListResponse](t, handler, "/api/v1/projects/"+project+"/evals?kind=legion&"+goldenWindow)
		work := seriesValues(t, findEval(t, legion.Items, project, evals.SlugLegionWorkTime), metrics.SeriesHours)
		if work.DataSince == nil || *work.DataSince > golden.Window.From.Format(time.DateOnly) || golden.Reports["all"].Finished == 0 {
			continue
		}
		checked++
		for scope, report := range golden.Reports {
			cut := metrics.Scope(scope)
			legionStat := func(slug, series, name string) *float64 {
				return seriesValues(t, findEval(t, legion.Items, project, slug), metrics.CutSeries(series, cut)).Window[name]
			}
			where := fmt.Sprintf("%s %s..%s %s", project, golden.Window.From.Format(time.DateOnly), golden.Window.To.Format(time.DateOnly), scope)
			within := func(what string, got *float64, want, tolerance float64) {
				t.Helper()
				compared++
				value := 0.0
				if got != nil {
					value = *got
				}
				if math.Abs(value-want) > tolerance {
					t.Errorf("%s: %s reads %s, the base head answered %g", where, what, showFigure(got), want)
				}
			}
			within("work time's issues", legionStat(evals.SlugLegionWorkTime, metrics.SeriesHours, "n"), float64(len(report.WorkTime.Issues)), 0)
			within("work time's median", legionStat(evals.SlugLegionWorkTime, metrics.SeriesHours, "median"), report.WorkTime.MedianHours, 0.1)
			within("work time's p90", legionStat(evals.SlugLegionWorkTime, metrics.SeriesHours, "p90"), report.WorkTime.P90Hours, 0.1)
			for _, bar := range report.PhaseTime.Bars {
				within(bar.Role+"'s hours", legionStat(evals.SlugLegionPhaseTime, bar.Role, "hours"), bar.Hours, 0.1)
				within(bar.Role+"'s issues", legionStat(evals.SlugLegionPhaseTime, bar.Role, "issues"), float64(bar.Issues), 0)
				within(bar.Role+"'s median per issue", legionStat(evals.SlugLegionPhaseTime, bar.Role, "median_per_issue"), bar.MedianHoursPerIssue, 0.1)
			}
			within("the idle share", legionStat(evals.SlugLegionPhaseTime, "implementer", "idle_share"), report.PhaseTime.IdleShare, 1e-9)
			within("review rounds' median", legionStat(evals.SlugLegionRework, metrics.SeriesReviewRounds, "median"), report.Rework.MedianReviewRounds, 1e-9)
			within("the sent-back share", legionStat(evals.SlugLegionRework, metrics.SeriesSentBack, "share"), report.Rework.SentBackShare, 1e-9)
			within("hours after the first review", legionStat(evals.SlugLegionRework, metrics.SeriesHoursAfterFirstReview, "hours"), report.Rework.HoursAfterFirstReview, 0.1)
			within("issues finished", legionStat(evals.SlugLegionSplitting, metrics.SeriesFinished, "issues"), float64(report.Finished), 0)
		}
		t.Logf("%s %s..%s: the Legion evals equal the base head's section in every scope (%d finished), data since %s",
			project, golden.Window.From.Format(time.DateOnly), golden.Window.To.Format(time.DateOnly), golden.Reports["all"].Finished, *work.DataSince)
	}
	if checked == 0 {
		t.Fatalf("no golden window starts on or after the Legion day rows' first day and counts a finished issue, so the comparison compared nothing")
	}
	t.Logf("%d figures compared in all, over %d golden windows", compared, checked)

	// GET /api/v1/evals: two projects, one, and every project with none named; each item the
	// project read's own.
	projects := countRows(t, database, `select count(*) from projects`)
	answered := map[string]map[string]bool{}
	for _, filter := range []string{"project=" + fixture.Project + "&project=" + project, "project=" + fixture.Project, ""} {
		across := evalGet[evalListResponse](t, handler, "/api/v1/evals?limit=200&"+filter+"&"+window)
		answered[filter] = map[string]bool{}
		for _, item := range across.Items {
			answered[filter][item.Definition.ProjectKey] = true
		}
		if across.Total != len(across.Items) && len(across.Items) != 200 {
			t.Errorf("?%s answers %d items of a total %d", filter, len(across.Items), across.Total)
		}
	}
	single := evalGet[evalListResponse](t, handler, "/api/v1/evals?project="+fixture.Project+"&limit=200&"+window)
	ownRead, err := json.Marshal(list.Items)
	if err != nil {
		t.Fatalf("encode the project read: %v", err)
	}
	acrossRead, err := json.Marshal(single.Items)
	if err != nil {
		t.Fatalf("encode the cross-project read: %v", err)
	}
	if len(answered["project="+fixture.Project+"&project="+project]) != 2 || len(answered["project="+fixture.Project]) != 1 ||
		int64(len(answered[""])) != projects || string(ownRead) != string(acrossRead) {
		t.Errorf("the cross-project read answers %d, %d and %d projects (the store holds %d), and its items of %s equal the project read's: %t",
			len(answered["project="+fixture.Project+"&project="+project]), len(answered["project="+fixture.Project]), len(answered[""]),
			projects, fixture.Project, string(ownRead) == string(acrossRead))
	}
	t.Logf("GET /api/v1/evals answers %v, %v and %v; the store holds %d projects; %d items of %s equal the project read's, byte for byte",
		slices.Sorted(maps.Keys(answered["project="+fixture.Project+"&project="+project])), slices.Sorted(maps.Keys(answered["project="+fixture.Project])),
		slices.Sorted(maps.Keys(answered[""])), projects, len(single.Items), fixture.Project)

	// The items route: one field set for every row kind.
	logged := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/"+fixture.Project+"/evals/deploy-notes/readings",
		map[string]any{"readings": []any{map[string]any{"series": "notes", "row_kind": "sample", "value": 1, "measured_at": from.Add(time.Hour).Format(time.RFC3339),
			"links": []any{map[string]any{"rel": "via", "href": "https://ci.example/runs/1", "label": "run 1"}}}}}, "items-reading")
	if logged.Code != http.StatusCreated {
		t.Fatalf("log a reading: status %d, body %s", logged.Code, logged.Body.String())
	}
	compare := url.Values{
		"from": {from.Format(time.RFC3339)}, "to": {to.Format(time.RFC3339)},
		"compare_from": {fixture.From.Format(time.RFC3339)}, "compare_to": {from.Format(time.RFC3339)},
	}
	kinds := map[string]map[string]bool{}
	rangesSeen := map[string]bool{}
	for _, read := range []struct {
		project, slug, query string
	}{
		{fixture.Project, evals.SlugMergeToProduction, "row_kind=pull_request"},
		{fixture.Project, evals.SlugDeployFrequency, "row_kind=deploy"},
		{fixture.Project, evals.SlugPRCI, "row_kind=run"},
		{fixture.Project, evals.SlugPRCI, "row_kind=job"},
		{fixture.Project, evals.SlugDeployFrequency, "row_kind=day"},
		{fixture.Project, "deploy-notes", "row_kind=sample"},
		{project, evals.SlugLegionWorkTime, "row_kind=issue&from=" + url.QueryEscape(windows[0].Window.From.Format(time.RFC3339)) + "&to=" + url.QueryEscape(windows[0].Window.To.Format(time.RFC3339))},
	} {
		query := read.query
		if !strings.Contains(query, "from=") {
			query += "&" + compare.Encode()
		}
		response := evalRequest(t, handler, http.MethodGet, "/api/v1/projects/"+read.project+"/evals/"+read.slug+"/items?limit=200&"+query, nil, "")
		if response.Code != http.StatusOK {
			t.Fatalf("GET the items of %s?%s: status %d, body %s", read.slug, query, response.Code, response.Body.String())
		}
		checkEvalEnvelope(t, read.slug+" items", response.Body.Bytes())
		var page struct {
			Items []map[string]json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
			t.Fatalf("decode the items of %s: %v", read.slug, err)
		}
		if len(page.Items) == 0 {
			t.Errorf("%s answers no row for %s", read.slug, read.query)
			continue
		}
		for _, row := range page.Items {
			var kind, rng string
			if err := json.Unmarshal(row["row_kind"], &kind); err != nil {
				t.Fatalf("decode a row kind of %s: %v", read.slug, err)
			}
			_ = json.Unmarshal(row["range"], &rng)
			rangesSeen[rng] = true
			if kinds[kind] == nil {
				kinds[kind] = map[string]bool{}
			}
			if names := slices.Sorted(maps.Keys(row)); !slices.Equal(names, evalItemFields) {
				t.Errorf("a %s row of %s carries %v, want %v", kind, read.slug, names, evalItemFields)
			}
			for name := range row {
				kinds[kind][name] = true
			}
		}
		t.Logf("%s items ?%s: %d rows, the first %.300s", read.slug, read.query, len(page.Items), mustJSON(t, page.Items[0]))
	}
	if len(kinds) < 6 || !rangesSeen[evals.RangeWindow] || !rangesSeen[evals.RangeCompare] {
		t.Errorf("the items answered the row kinds %v and the ranges %v, want issue, pull_request, deploy, run, job, day and a reading's, of both ranges",
			slices.Sorted(maps.Keys(kinds)), slices.Sorted(maps.Keys(rangesSeen)))
	}
	for kind, fields := range kinds {
		if names := slices.Sorted(maps.Keys(fields)); !slices.Equal(names, evalItemFields) {
			t.Errorf("the %s rows carry %v, want %v", kind, names, evalItemFields)
		}
	}
	t.Logf("%d row kinds %v, each with the fields %v, of the ranges %v", len(kinds), slices.Sorted(maps.Keys(kinds)), evalItemFields,
		slices.Sorted(maps.Keys(rangesSeen)))
}

// TestTheFailingJobsItemsAreThePipelinesFailingJobs: pr-ci's items with sort=failures answer the
// list GET /api/v1/delivery/pipeline answers in pr_checks.failing_jobs for the same from and to,
// one list and two readers (measures.FailingJobs over the runs the Pipeline reads for that
// window): over a window whose 24 hours before its end hold a run of the deliverytest history
// that started before them and failed a job inside them, and over a whole-day window.
func TestTheFailingJobsItemsAreThePipelinesFailingJobs(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	ctx := t.Context()
	fixture := deliverytest.Seed(t, database.Pool, deliverytest.Spec{Seed: 2916, OneProject: true, Days: 14})
	registry := evals.New(database, deps.Docs, deps.Events)
	for day := fixture.From; day.Before(fixture.To); day = day.AddDate(0, 0, 1) {
		if _, err := registry.WriteDeliveryDay(ctx, fixture.Settings, day, "", testServerBuild); err != nil {
			t.Fatalf("write the delivery rows of %s: %v", day.Format(time.DateOnly), err)
		}
	}
	if _, err := registry.Ensure(ctx, fixture.Project); err != nil {
		t.Fatalf("seed the evals of %s: %v", fixture.Project, err)
	}

	// A pull-request checks run that failed a job, ending the window one second past the 24 hours
	// after its start: it started before the 24 hours the table counts and completed inside them.
	var straddling *delivery.DeliveryRun
	for index, run := range fixture.Runs {
		outcome, timed := measures.OutcomeOf(run.Conclusion)
		failed := slices.ContainsFunc(fixture.Jobs[run.RunID], func(job delivery.DeliveryRunJob) bool {
			jobOutcome, ok := measures.OutcomeOf(job.Conclusion)
			return ok && jobOutcome == measures.OutcomeFailure
		})
		if run.Kind == delivery.DeliveryRunKindPRChecks && timed && outcome != measures.OutcomeCancelled && failed &&
			run.CompletedAt != nil && run.CompletedAt.Sub(run.StartedAt) > 2*time.Second &&
			run.StartedAt.Add(measures.FailingWindow+time.Second).Before(fixture.To) && run.StartedAt.After(fixture.From.AddDate(0, 0, 1)) {
			straddling = &fixture.Runs[index]
			break
		}
	}
	if straddling == nil {
		t.Fatal("the history holds no pull-request checks run that failed a job, so no window straddles one")
	}
	straddleTo := straddling.StartedAt.Add(measures.FailingWindow + time.Second)
	for _, window := range []struct {
		name     string
		from, to time.Time
	}{
		{"a window whose 24 hours hold a run started before them", straddleTo.AddDate(0, 0, -7), straddleTo},
		{"a whole-day window", fixture.From.AddDate(0, 0, 4), fixture.From.AddDate(0, 0, 11)},
	} {
		query := url.Values{"from": {window.from.Format(time.RFC3339Nano)}, "to": {window.to.Format(time.RFC3339Nano)}}.Encode()
		pipeline := evalGet[deliveryPipelineResponse](t, handler, "/api/v1/delivery/pipeline?"+query).PRChecks.FailingJobs
		items := evalGet[evalItemsResponse](t, handler, "/api/v1/projects/"+fixture.Project+"/evals/pr-ci/items?sort=failures&limit=200&"+query)
		answered := make([]measures.FailingJob, 0, len(items.Items))
		for _, row := range items.Items {
			var job measures.FailingJob
			if err := json.Unmarshal([]byte(mustJSON(t, row.Cols)), &job); err != nil {
				t.Fatalf("decode the failing job row %s: %v", row.RowKey, err)
			}
			answered = append(answered, job)
		}
		if len(pipeline) == 0 {
			t.Fatalf("%s (%s): the Pipeline answers no failing job, so the comparison compares nothing", window.name, query)
		}
		if got, want := mustJSON(t, answered), mustJSON(t, pipeline); got != want {
			t.Errorf("%s (%s): the items answer the failing jobs %s, the Pipeline %s", window.name, query, got, want)
			continue
		}
		t.Logf("%s (%s): the items and the Pipeline answer the same %d failing jobs, the first %+v", window.name, query, len(pipeline), pipeline[0])
	}
	t.Logf("the straddling run %d started %s, completed %s; its window ends %s", straddling.RunID,
		straddling.StartedAt.Format(time.RFC3339), straddling.CompletedAt.Format(time.RFC3339), straddleTo.Format(time.RFC3339))
}

// TestEveryCatalogueSourceAnswersItsRows: every computed eval a project is seeded with names a
// source the items route reads rows of (evalComputedItems), so no computed eval's items are its
// readings and day rows alone by an omission. The folds' own table is held to the catalogue when
// the evals package starts.
func TestEveryCatalogueSourceAnswersItsRows(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	createTestProject(t, handler, "EVL")
	list := evalGet[evalListResponse](t, handler, "/api/v1/projects/EVL/evals?limit=200")
	computed := 0
	for _, item := range list.Items {
		if !item.Definition.Computed() {
			continue
		}
		computed++
		if _, held := evalComputedItems[item.Definition.Source]; !held {
			t.Errorf("%s names %s, which evalComputedItems reads no row of", item.Definition.EvalID, item.Definition.Source)
		}
	}
	if computed == 0 {
		t.Fatal("the seeded project holds no computed eval, so the check checked nothing")
	}
	t.Logf("%d seeded computed evals, each with its rows", computed)
}

// showFigure is a statistic for a failure message.
func showFigure(value *float64) string {
	if value == nil {
		return "null"
	}
	return fmt.Sprint(*value)
}
