package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
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
