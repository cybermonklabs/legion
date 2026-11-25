package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestThreadPageRefusesAQueryItCannotAnswerExactly(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "PROJ", "Thread query refusals", "A spec")
	const at = "2025-01-01T00:00:00.000000Z"
	const id = "ab000000-0000-4000-8000-000000000001"
	// Each refusal says whole which parameter it refuses and every value that parameter takes:
	// must refuses a value the page cannot read, absent a page parameter sent without order.
	const (
		limit  = "one integer from 1 to 100"
		before = "one position: a UTC timestamp with six fractional digits and Z, a comma, and a lower-case UUID"
		author = "one non-empty login or session ID"
	)
	must := func(key, values string) string { return key + " must be " + values }
	absent := func(key, values string) string {
		return key + " must be absent without order=activity; with it, " + values
	}
	type refusalCase struct{ query, message string }
	common := []refusalCase{
		{"order=created", must("order", "activity")}, {"order=", must("order", "activity")},
		{"order=activity&order=activity", must("order", "activity")},
		{"order=activity&limit=0", must("limit", limit)}, {"order=activity&limit=101", must("limit", limit)},
		{"order=activity&limit=abc", must("limit", limit)}, {"order=activity&limit=20&limit=20", must("limit", limit)},
		{"limit=20", absent("limit", limit)},
		{"before=" + at + "," + id, absent("before", before)},
		{"order=activity&before=yesterday", must("before", before)},
		{"order=activity&before=" + at + ",nope", must("before", before)},
		{"order=activity&before=2025-01-01T00:00:00.000000," + id, must("before", before)},
		{"order=activity&before=2025-01-01T00:00:00.000000%2B00:00," + id, must("before", before)},
		{"order=activity&before=2025-01-01T00:00:00.0000000Z," + id, must("before", before)},
		{"order=activity&before=" + at + "," + strings.ToUpper(id), must("before", before)},
		{"order=activity&before=" + at + "," + id + "&before=" + at + "," + id, must("before", before)},
		{"author=x", absent("author", author)}, {"order=activity&author=", must("author", author)},
		{"order=activity&author=x&author=x", must("author", author)},
	}
	for _, route := range []struct{ name, person string }{
		{"comments", "one value: in"},
		{"messages", "one value: in or none"},
		{"asks", "absent (this route does not take a person filter)"},
	} {
		t.Run(route.name, func(t *testing.T) {
			cases := append(slices.Clone(common),
				refusalCase{"order=activity&person=out", must("person", route.person)},
				refusalCase{"order=activity&person=in&person=in", must("person", route.person)})
			switch route.name {
			case "asks":
				cases = append(cases, refusalCase{"order=activity&person=in", must("person", route.person)})
			case "comments":
				cases = append(cases, refusalCase{"order=activity&person=none", must("person", route.person)})
			case "messages":
				cases = append(cases, refusalCase{"person=in", absent("person", route.person)})
			}
			for _, c := range cases {
				t.Run(c.query, func(t *testing.T) {
					response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/"+route.name+"?"+c.query, nil, "alice")
					if response.Code != http.StatusBadRequest {
						t.Fatalf("status = %d, want 400: %s", response.Code, response.Body)
					}
					refusal := decodeBody[map[string]any](t, response)
					if refusal["code"] != "INVALID_QUERY" || refusal["error"] != c.message || len(refusal) != 2 {
						t.Fatalf("refusal = %v, want INVALID_QUERY saying %q, without threads", refusal, c.message)
					}
				})
			}
		})
	}
	for _, c := range []struct{ query, code string }{{"state=bad", "INVALID_ASK_STATE"}, {"thread=bad", "INVALID_ASK_THREAD"}} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks?order=bad&"+c.query, nil, "alice")
		if response.Code != http.StatusBadRequest || decodeBody[map[string]any](t, response)["code"] != c.code {
			t.Errorf("%s with invalid order: %d %s, want 400 %s", c.query, response.Code, response.Body, c.code)
		}
	}
}
