package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The tester's rows for INFRA-2916: inputs a careless or hostile agent can send to the eval
// routes. Every one of them is a value the readings write's own validation lets through or a
// window the reads' own parser lets through, and each must leave the registry answerable: a
// write is refused with a stable {code, error} naming its row, or it lands and every read of it
// answers the {items, total, limit, offset} envelope (R4, R7). No input of a caller is a 500 or an
// empty 200.

// checkAnswered fails unless response is a 200 holding the eval envelope.
func checkAnswered(t *testing.T, what string, response *httptest.ResponseRecorder) {
	t.Helper()
	body := response.Body.Bytes()
	if response.Code != http.StatusOK {
		t.Errorf("%s: status %d, body %.200s; want 200 and the envelope", what, response.Code, body)
		return
	}
	if len(body) == 0 {
		t.Errorf("%s: 200 with an empty body; want the envelope", what)
		return
	}
	checkEvalEnvelope(t, what, body)
}

// checkRefusedWithACode fails unless body is a 4xx {code, error} answer.
func checkRefusedWithACode(t *testing.T, what string, status int, body []byte) {
	t.Helper()
	var refusal evalRefusal
	if status < 400 || status >= 500 || json.Unmarshal(body, &refusal) != nil || refusal.Code == "" || refusal.Error == "" {
		t.Errorf("%s: status %d, body %.300s; want a 4xx refusal with a stable {code, error}", what, status, body)
	}
}

// TestReadingsWhoseFoldOverflowsLeaveEveryEvalReadAnswerable: two finite readings of 1e308 on one
// day of an eval whose series folds by sum make the day's stored sum +Inf. The write either refuses
// them, or every read that folds or lists that day - the project's list, the cross-project read
// with the project and with no project named (the Delivery page's and the Evals tab's reads), the
// eval's own read and its items - answers the envelope. One agent's readings take no project's
// page down, nor every project's.
func TestReadingsWhoseFoldOverflowsLeaveEveryEvalReadAnswerable(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "EVL")
	now := time.Now().UTC()
	registered := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/EVL/evals",
		map[string]any{"slug": "huge", "series": []any{map[string]any{"series": "value", "agg": "sum"}}}, "huge-register")
	if registered.Code != http.StatusCreated {
		t.Fatalf("register huge: status %d, body %s", registered.Code, registered.Body.String())
	}
	at := now.Add(-2 * time.Hour).Truncate(time.Hour)
	written := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/EVL/evals/huge/readings", map[string]any{"readings": []any{
		map[string]any{"series": "value", "row_kind": "run", "row_key": "a", "value": 1e308, "measured_at": at.Format(time.RFC3339)},
		map[string]any{"series": "value", "row_kind": "run", "row_key": "b", "value": 1e308, "measured_at": at.Add(time.Minute).Format(time.RFC3339)},
	}}, "huge-readings")
	if written.Code != http.StatusCreated {
		checkRefusedWithACode(t, "the readings write", written.Code, written.Body.Bytes())
		if stored := countRows(t, database, `select count(*) from eval_readings where eval_id = 'EVL/huge'`); stored != 0 {
			t.Errorf("the refused write stored %d readings, want none", stored)
		}
		return
	}
	window := url.Values{"from": {now.Add(-24 * time.Hour).Format(time.RFC3339)}, "to": {now.Add(time.Hour).Format(time.RFC3339)}}.Encode()
	for _, path := range []string{
		"/api/v1/projects/EVL/evals?" + window,
		"/api/v1/evals?project=EVL&" + window,
		"/api/v1/evals?" + window,
		"/api/v1/projects/EVL/evals/huge?" + window,
		"/api/v1/projects/EVL/evals/huge/items?" + window,
	} {
		response := evalRequest(t, handler, http.MethodGet, path, nil, "")
		checkAnswered(t, "GET "+path, response)
	}
}

// TestAKeyTooLongForTheStoreIsRefusedNamingItsRow: a row_key, a row_kind, a series or a slug of
// 3,000 characters that do not compress is longer than a B-tree index entry holds. A readings write
// carrying one is refused with a stable code naming the row, or stored; it is never a 500.
func TestAKeyTooLongForTheStoreIsRefusedNamingItsRow(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "EVL")
	// 3,000 characters with no repeat a compressor can use: the store keeps an index entry
	// compressed, so a run of one letter fits where this does not.
	var long strings.Builder
	for index := 0; long.Len() < 3000; index++ {
		fmt.Fprintf(&long, "%x", uint32(index)*2654435761)
	}
	key := long.String()[:3000]
	at := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	for _, field := range []string{"row_key", "row_kind", "series"} {
		row := map[string]any{"series": "value", "row_kind": "run", "row_key": "r", "value": 1, "measured_at": at}
		row[field] = key
		response := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/EVL/evals/long-"+strings.ReplaceAll(field, "_", "-")+"/readings",
			map[string]any{"readings": []any{row}}, "long-"+field)
		if response.Code == http.StatusCreated {
			continue
		}
		checkRefusedWithACode(t, "a reading whose "+field+" is 3,000 characters", response.Code, response.Body.Bytes())
		var refusal evalRefusal
		if json.Unmarshal(response.Body.Bytes(), &refusal) == nil && (len(refusal.Rows) != 1 || refusal.Rows[0].Index != 0) {
			t.Errorf("a reading whose %s is 3,000 characters is refused naming rows %+v, want row 0", field, refusal.Rows)
		}
	}
	slug := "s" + strings.ToLower(key[:2999])
	response := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/EVL/evals",
		map[string]any{"slug": slug, "name": "long"}, "long-slug")
	if response.Code != http.StatusCreated {
		checkRefusedWithACode(t, "a register write whose slug is 3,000 characters", response.Code, response.Body.Bytes())
	}
	if stored := countRows(t, database, `select count(*) from eval_versions where length(slug) >= 3000`); response.Code != http.StatusCreated && stored != 0 {
		t.Errorf("the refused register write stored %d versions, want none", stored)
	}
}

// TestACorrectedDayFoldsTheReadingsThatStand: a correction takes the reading it replaces out of
// its day's row, and the day then folds the readings that stand (R4, R12). A weighted mean is the
// summed numerator over the summed denominator, so after readings of 1e16 and 1 (n 1 each) and the
// correction of 1e16 to 2, the day's mean is (2 + 1) / 2 = 1.5, whatever the corrected reading's
// size made of the running sum.
func TestACorrectedDayFoldsTheReadingsThatStand(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	createTestProject(t, handler, "EVL")
	registered := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/EVL/evals",
		map[string]any{"slug": "weighted", "series": []any{map[string]any{"series": "value", "agg": "weighted_mean"}}}, "weighted-register")
	if registered.Code != http.StatusCreated {
		t.Fatalf("register weighted: status %d, body %s", registered.Code, registered.Body.String())
	}
	day := time.Now().UTC().Add(-48 * time.Hour).Truncate(24 * time.Hour)
	at := func(hour int) string { return day.Add(time.Duration(hour) * time.Hour).Format(time.RFC3339) }
	path := "/api/v1/projects/EVL/evals/weighted/readings"
	first := evalRequest(t, handler, http.MethodPost, path, map[string]any{"readings": []any{
		map[string]any{"series": "value", "row_kind": "run", "row_key": "a", "value": 1e16, "n": 1, "measured_at": at(8)},
		map[string]any{"series": "value", "row_kind": "run", "row_key": "b", "value": 1, "n": 1, "measured_at": at(9)},
	}}, "weighted-readings")
	if first.Code != http.StatusCreated {
		t.Fatalf("log the readings: status %d, body %s", first.Code, first.Body.String())
	}
	replaced := decodeBody[evalReadingsResponse](t, first).Items[0].ID
	correction := evalRequest(t, handler, http.MethodPost, path, map[string]any{"readings": []any{
		map[string]any{"series": "value", "row_kind": "run", "row_key": "a", "value": 2, "n": 1, "measured_at": at(8), "replaces": replaced},
	}}, "weighted-correction")
	if correction.Code != http.StatusCreated {
		t.Fatalf("correct the first reading: status %d, body %s", correction.Code, correction.Body.String())
	}
	window := url.Values{"from": {day.Format(time.RFC3339)}, "to": {day.Add(24 * time.Hour).Format(time.RFC3339)}}.Encode()
	detail := evalGet[evalDetailResponse](t, handler, "/api/v1/projects/EVL/evals/weighted?"+window)
	if len(detail.Items) == 0 || len(detail.Items[0].Series) == 0 {
		t.Fatalf("the eval's read answers no series: %+v", detail)
	}
	mean := detail.Items[0].Series[0].Window["mean"]
	if mean == nil {
		t.Fatalf("the corrected day's weighted mean is null, want 1.5 from the standing readings 2 and 1")
	}
	if *mean < 1.5-1e-9 || *mean > 1.5+1e-9 {
		t.Errorf("the corrected day's weighted mean is %v, want 1.5 from the standing readings 2 and 1", *mean)
	}
}

// TestAReadingOutsideTheYearsAReadAnswersIsRefused: a measured_at whose UTC year is 10000
// (9999-12-31T23:59:59-14:00, a time RFC 3339 writes) is refused, or stored and answered; a write
// that stores the reading and answers an empty 201 leaves its caller unable to tell whether it
// landed, and its retry under the same key answers the same nothing.
func TestAReadingOutsideTheYearsAReadAnswersIsRefused(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	createTestProject(t, handler, "EVL")
	body := map[string]any{"readings": []any{
		map[string]any{"series": "value", "row_kind": "run", "row_key": "far", "value": 1, "measured_at": "9999-12-31T23:59:59-14:00"},
	}}
	for _, attempt := range []string{"first", "retry"} {
		response := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/EVL/evals/far-future/readings", body, "far-future")
		if response.Code == http.StatusCreated {
			checkEvalEnvelope(t, "the "+attempt+" readings write", response.Body.Bytes())
			continue
		}
		checkRefusedWithACode(t, "the "+attempt+" readings write", response.Code, response.Body.Bytes())
	}
}

// TestAWindowOutsideTheYearsAReadAnswersIsRefused: from, to, compare_from and compare_to each parse
// as RFC 3339 and may name a UTC year after 9999 or before 1, which no answer can write. Each of the
// four reads refuses such a window 400 INVALID_QUERY, as it refuses any other malformed window,
// rather than answer an empty 200.
func TestAWindowOutsideTheYearsAReadAnswersIsRefused(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	createTestProject(t, handler, "EVL")
	registerReviewTime(t, handler)
	for _, window := range []url.Values{
		{"from": {"9999-12-30T00:00:00Z"}, "to": {"9999-12-31T23:00:00-14:00"}},
		{"from": {"0000-01-01T00:00:00+01:00"}, "to": {"0000-01-03T00:00:00Z"}},
		{"from": {"2026-11-01T00:00:00Z"}, "to": {"2026-11-02T00:00:00Z"},
			"compare_from": {"9999-12-30T00:00:00Z"}, "compare_to": {"9999-12-31T23:00:00-14:00"}},
	} {
		for _, path := range []string{
			"/api/v1/projects/EVL/evals?",
			"/api/v1/evals?",
			"/api/v1/projects/EVL/evals/review-time?",
			"/api/v1/projects/EVL/evals/review-time/items?",
		} {
			target := path + window.Encode()
			response := evalRequest(t, handler, http.MethodGet, target, nil, "")
			var refusal evalRefusal
			if response.Code != http.StatusBadRequest || json.Unmarshal(response.Body.Bytes(), &refusal) != nil || refusal.Code != "INVALID_QUERY" {
				t.Errorf("GET %s: status %d, body %.200s; want 400 INVALID_QUERY", target, response.Code, response.Body.String())
			}
		}
	}
}

// TestAnEvalReadsRepositoryListIsHeldToItsBounds: every statement of an eval read sends its repo
// list, so a read takes at most evalRepoMax repositories, a repository the query repeats counted
// once, each of at most evalRepoBytes bytes. Each of the four reads answers a list at both bounds
// whose every repository the query names twice, and refuses one repository more, or one byte more
// of a name, 400 INVALID_QUERY naming the bound.
func TestAnEvalReadsRepositoryListIsHeldToItsBounds(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	createTestProject(t, handler, "EVL")
	registerReviewTime(t, handler)
	// repos names count repositories of bytes bytes each, every one twice.
	repos := func(count, bytes int) url.Values {
		query := url.Values{}
		for index := range count {
			head := fmt.Sprintf("acme/r%03d-", index)
			name := head + strings.Repeat("x", bytes-len(head))
			query["repo"] = append(query["repo"], name, name)
		}
		return query
	}
	for _, path := range []string{
		"/api/v1/projects/EVL/evals?",
		"/api/v1/evals?",
		"/api/v1/projects/EVL/evals/review-time?",
		"/api/v1/projects/EVL/evals/review-time/items?",
	} {
		checkAnswered(t, fmt.Sprintf("GET %s with %d repositories of %d bytes, each named twice", path, evalRepoMax, evalRepoBytes),
			evalRequest(t, handler, http.MethodGet, path+repos(evalRepoMax, evalRepoBytes).Encode(), nil, ""))
		for _, past := range []struct {
			what  string
			query url.Values
			names string
		}{
			{what: fmt.Sprintf("%d repositories", evalRepoMax+1), query: repos(evalRepoMax+1, evalRepoBytes), names: fmt.Sprintf("at most %d", evalRepoMax)},
			{what: fmt.Sprintf("a repository of %d bytes", evalRepoBytes+1), query: repos(1, evalRepoBytes+1), names: fmt.Sprintf("at most %d", evalRepoBytes)},
		} {
			response := evalRequest(t, handler, http.MethodGet, path+past.query.Encode(), nil, "")
			var refusal evalRefusal
			if response.Code != http.StatusBadRequest || json.Unmarshal(response.Body.Bytes(), &refusal) != nil || refusal.Code != "INVALID_QUERY" ||
				!strings.Contains(refusal.Error, past.names) {
				t.Errorf("GET %s with %s: status %d, body %.300s; want 400 INVALID_QUERY naming %q", path, past.what, response.Code, response.Body.String(), past.names)
			}
		}
	}
}

// TestADeliveryReadRefusesARepeatedWindowKey: the delivery routes read their window through the
// parser the eval routes do (parseDeliveryWindow), and the eval routes refuse a key they take one
// value of when a query names it twice (C9 of the review of 2fdcf77f). A delivery read answered
// ?from=A&from=B as the window from A, so the window a reader sent and the one its answer folds
// differ with nothing to say so; it refuses that query 400 INVALID_QUERY, as the eval reads do.
// The Pipeline's window is at alone, so its repeated key is at.
func TestADeliveryReadRefusesARepeatedWindowKey(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "EVL")
	seedMeasuresSettings(t, database)
	var targets []string
	for _, window := range []url.Values{
		{"from": {"2026-11-01T00:00:00Z", "2026-10-01T00:00:00Z"}, "to": {"2026-11-14T00:00:00Z"}},
		{"from": {"2026-11-01T00:00:00Z"}, "to": {"2026-11-14T00:00:00Z", "2026-11-10T00:00:00Z"}},
	} {
		for _, path := range []string{
			"/api/v1/delivery/measures?",
			"/api/v1/delivery/timeline?",
		} {
			targets = append(targets, path+window.Encode())
		}
	}
	targets = append(targets, "/api/v1/delivery/pipeline?"+url.Values{"at": {"2026-11-14T00:00:00Z", "2026-11-10T00:00:00Z"}}.Encode())
	for _, target := range targets {
		response := evalRequest(t, handler, http.MethodGet, target, nil, "")
		var refusal evalRefusal
		if response.Code != http.StatusBadRequest || json.Unmarshal(response.Body.Bytes(), &refusal) != nil || refusal.Code != "INVALID_QUERY" {
			t.Errorf("GET %s: status %d, body %.120s; want 400 INVALID_QUERY", target, response.Code, response.Body.String())
		}
	}
}

// TestAMinNPastWhatTheStoreHoldsIsRefusedNamingItsField: a series' min_n is a whole number the
// register write takes on pr-ci's job series (R5), the one series a read ranks by one, and the
// store holds it as a 32-bit integer. A min_n past 2,147,483,647 is refused with a stable {code,
// error} and stores no version, as a min_n below 0 is, rather than reaching the store and
// answering 500; the largest value the store holds is still taken. A logged eval's series is ranked
// by no read, so any min_n on it, the largest the store holds included, is refused
// EVAL_FIELD_INVALID naming the series' min_n.
func TestAMinNPastWhatTheStoreHoldsIsRefusedNamingItsField(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "EVL")
	logged := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/EVL/evals",
		map[string]any{"slug": "logged-min-n", "series": []any{map[string]any{"series": "value"}}}, "logged-min-n")
	if logged.Code != http.StatusCreated {
		t.Fatalf("register logged-min-n: status %d, body %s", logged.Code, logged.Body.String())
	}
	for _, eval := range []struct {
		slug, series string
		ranked       bool
	}{{"pr-ci", "job:*", true}, {"logged-min-n", "value", false}} {
		versions := func() int64 {
			return countRows(t, database, `select count(*) from eval_series where eval_id = $1 and series = $2`, "EVL/"+eval.slug, eval.series)
		}
		for _, minN := range []int64{2147483648, 3000000000, 9223372036854775807} {
			before := versions()
			response := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/EVL/evals", map[string]any{
				"slug": eval.slug, "series": []any{map[string]any{"series": eval.series, "min_n": minN}},
			}, fmt.Sprintf("%s-min-n-%d", eval.slug, minN))
			checkRefusedWithACode(t, fmt.Sprintf("%s's series %s with min_n %d", eval.slug, eval.series, minN), response.Code, response.Body.Bytes())
			if after := versions(); after != before {
				t.Errorf("%s's refused min_n %d wrote versions of %s: %d before, %d after", eval.slug, minN, eval.series, before, after)
			}
		}
		before := versions()
		held := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/EVL/evals", map[string]any{
			"slug": eval.slug, "series": []any{map[string]any{"series": eval.series, "min_n": 2147483647}},
		}, eval.slug+"-min-n-largest")
		switch refusal := decodeBody[evalRefusal](t, held); {
		case eval.ranked && held.Code != http.StatusCreated:
			t.Errorf("%s's series %s with min_n 2147483647: status %d, body %.300s; want 201", eval.slug, eval.series, held.Code, held.Body.String())
		case !eval.ranked && (held.Code != http.StatusBadRequest || refusal.Code != evals.CodeFieldInvalid || refusal.Field != "series[0].min_n" || versions() != before):
			t.Errorf("%s's series %s with min_n 2147483647: status %d, body %.300s, versions %d to %d; want 400 %s naming series[0].min_n and no version",
				eval.slug, eval.series, held.Code, held.Body.String(), before, versions(), evals.CodeFieldInvalid)
		}
	}
}

// TestAFirstRegistrationIntoAnUnloadableEvalsMdIsRefusedAsTheDocumentIs: a project's evals-md whose
// stored history does not decode (every stored update replaced by bytes no document takes, with no
// room holding it and no settlement owed, makeDocumentUnloadable), and a first registration of an
// eval into it: by a reading, and by a register write, each into a project of its own. Each is
// refused 409 DOCUMENT_UNLOADABLE, what a write that meets that history answers (handler_errors.go,
// docs.ErrDocumentUnloadable), stores no version, and leaves the document's room as a write's own
// read of the history leaves it: failed once, and no room resident.
func TestAFirstRegistrationIntoAnUnloadableEvalsMdIsRefusedAsTheDocumentIs(t *testing.T) {
	database := storetest.Open(t)
	// Settlement is an hour away, so no background settlement loads the document while the test
	// reads its room; the test hooks count the room's failures.
	documents := docs.New(docs.Deps{Store: database, Identity: headerIdentity(database), Settle: time.Hour, TestHooks: true})
	t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
	seedPeople(t, database, "alice", "bob")
	deps, err := NewDeps(DepsInput{
		Store: database, Identity: headerIdentity(database), AgentTokens: sharedAgentTokens(t, "agent-token"),
		Docs: documents, ServerURL: "https://dispatch.example", Build: testServerBuild,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	t.Cleanup(Register(mux, t.Context(), deps).Wait)
	fixture := repairFixture{handler: mux, database: database, documents: documents, deps: deps}
	for _, write := range []struct {
		project, slug string
		send          func() *httptest.ResponseRecorder
	}{
		{"EVR", "cache-hits", func() *httptest.ResponseRecorder {
			return evalRequest(t, mux, http.MethodPost, "/api/v1/projects/EVR/evals/cache-hits/readings",
				map[string]any{"readings": []any{reading("share", 0.8, time.Now().UTC())}}, "unloadable-reading")
		}},
		{"EVG", "review-time", func() *httptest.ResponseRecorder {
			return evalRequest(t, mux, http.MethodPost, "/api/v1/projects/EVG/evals",
				map[string]any{"slug": "review-time", "name": "Review time"}, "unloadable-register")
		}},
	} {
		createTestProject(t, mux, write.project)
		var document string
		if err := database.Pool.QueryRow(t.Context(), `
			select id::text from artifacts where project_key = $1 and issue_key is null and slug = $2
		`, write.project, evals.DocumentSlug).Scan(&document); err != nil {
			t.Fatalf("find %s's %s: %v", write.project, evals.DocumentSlug, err)
		}
		if corrupted := makeDocumentUnloadable(t, fixture, document); corrupted == 0 {
			t.Fatalf("%s's %s holds no stored update to corrupt", write.project, evals.DocumentSlug)
		}
		response := write.send()
		refusal := decodeBody[evalRefusal](t, response)
		counters, err := documents.RoomCounters(document)
		if err != nil {
			t.Fatalf("read the room counters of %s's %s: %v", write.project, evals.DocumentSlug, err)
		}
		resident := slices.Contains(documents.ResidentRooms(), document)
		var versions int
		if err := database.Pool.QueryRow(t.Context(), `select count(*) from eval_versions where eval_id = $1`,
			evals.EvalID(write.project, write.slug)).Scan(&versions); err != nil {
			t.Fatalf("count the versions of %s: %v", write.slug, err)
		}
		t.Logf("%s's first registration into an unloadable %s: status %d, body %.300s; room failures %d, resident %t, versions %d",
			write.slug, evals.DocumentSlug, response.Code, response.Body.String(), counters.RoomFailures, resident, versions)
		if response.Code != http.StatusConflict || refusal.Code != codeDocumentUnloadable {
			t.Errorf("%s: status %d, body %.300s; want 409 %s", write.slug, response.Code, response.Body.String(), codeDocumentUnloadable)
		}
		if counters.RoomFailures != 1 || resident || versions != 0 {
			t.Errorf("%s: the room failed %d times, resident %t, and the eval holds %d versions; want one failure, no resident room and no version",
				write.slug, counters.RoomFailures, resident, versions)
		}
	}
}
