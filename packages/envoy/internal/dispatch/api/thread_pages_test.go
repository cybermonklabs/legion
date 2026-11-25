package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestThreadPageRefusesAQueryItCannotAnswerExactly(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "PROJ", "Thread query refusals", "A spec")
	const at = "2025-01-01T00:00:00.000000Z"
	const id = "ab000000-0000-4000-8000-000000000001"
	common := []struct{ query, key, values string }{
		{"order=created", "order", "activity"}, {"order=", "order", "activity"},
		{"order=activity&order=activity", "order", "activity"},
		{"order=activity&limit=0", "limit", "1 to 100"}, {"order=activity&limit=101", "limit", "1 to 100"},
		{"order=activity&limit=abc", "limit", "1 to 100"}, {"order=activity&limit=20&limit=20", "limit", "1 to 100"},
		{"limit=20", "limit", "order=activity"},
		{"before=" + at + "," + id, "before", "order=activity"},
		{"order=activity&before=yesterday", "before", "UUID"},
		{"order=activity&before=" + at + ",nope", "before", "UUID"},
		{"order=activity&before=2025-01-01T00:00:00.000000," + id, "before", "UTC"},
		{"order=activity&before=2025-01-01T00:00:00.000000%2B00:00," + id, "before", "UTC"},
		{"order=activity&before=2025-01-01T00:00:00.0000000Z," + id, "before", "six"},
		{"order=activity&before=" + at + "," + strings.ToUpper(id), "before", "lower-case"},
		{"order=activity&before=" + at + "," + id + "&before=" + at + "," + id, "before", "UUID"},
		{"author=x", "author", "order=activity"}, {"order=activity&author=", "author", "non-empty"},
		{"order=activity&author=x&author=x", "author", "non-empty"},
	}
	for _, route := range []struct{ name, personValues string }{{"comments", "in"}, {"messages", "in or none"}, {"asks", "absent"}} {
		t.Run(route.name, func(t *testing.T) {
			cases := append([]struct{ query, key, values string }{}, common...)
			cases = append(cases, struct{ query, key, values string }{"order=activity&person=out", "person", route.personValues}, struct{ query, key, values string }{"order=activity&person=in&person=in", "person", route.personValues})
			switch route.name {
			case "asks":
				cases = append(cases, struct{ query, key, values string }{"order=activity&person=in", "person", "absent"})
			case "comments":
				cases = append(cases, struct{ query, key, values string }{"order=activity&person=none", "person", "in"})
			case "messages":
				cases = append(cases, struct{ query, key, values string }{"person=in", "person", "order=activity"})
			}
			for _, c := range cases {
				t.Run(c.query, func(t *testing.T) {
					response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/"+route.name+"?"+c.query, nil, "alice")
					if response.Code != http.StatusBadRequest {
						t.Fatalf("status = %d, want 400: %s", response.Code, response.Body)
					}
					refusal := decodeBody[map[string]any](t, response)
					message, _ := refusal["error"].(string)
					if refusal["code"] != "INVALID_QUERY" || !strings.HasPrefix(message, c.key+" must be ") || !strings.Contains(message, c.values) || len(refusal) != 2 {
						t.Fatalf("refusal = %v, want INVALID_QUERY naming %s and %q, without threads", refusal, c.key, c.values)
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
