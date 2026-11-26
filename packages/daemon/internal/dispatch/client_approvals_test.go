package dispatch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

// An issue's assignee is a login, null for none, or nothing at all from a Dispatch that predates
// assignees; the listing and the detail answer it alike, and a member that is neither login nor null
// reads as not carried rather than failing the read the workflow acts on.
func TestListAndDetailCarryTheAssignee(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBearer(t, r)
		switch r.URL.Path {
		case "/api/v1/issues":
			writeJSON(t, w, []map[string]any{
				{"key": "LEGION-1", "status": "todo", "last_seq": 4, "assignee": "alice"},
				{"key": "LEGION-2", "status": "todo", "last_seq": 5, "assignee": nil},
				{"key": "LEGION-3", "status": "todo", "last_seq": 6},
				{"key": "LEGION-4", "status": "todo", "last_seq": 7, "assignee": 7},
			})
		case "/api/v1/issues/LEGION-1":
			writeJSON(t, w, map[string]any{"key": "LEGION-1", "status": "todo", "last_seq": 4, "assignee": "alice"})
		case "/api/v1/issues/LEGION-2":
			writeJSON(t, w, map[string]any{"key": "LEGION-2", "status": "todo", "last_seq": 5, "assignee": nil})
		case "/api/v1/issues/LEGION-3":
			writeJSON(t, w, map[string]any{"key": "LEGION-3", "status": "todo", "last_seq": 6})
		default:
			t.Errorf("request = %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client := New(server.URL, BearerTransport(dispatchToken, nil))

	listed, err := client.ListIssues(context.Background(), "LEGION", nil)
	if err != nil {
		t.Fatalf("list issues: %v", err)
	}
	type assignee struct {
		person string
		known  bool
	}
	var got []assignee
	for _, issue := range listed {
		got = append(got, assignee{issue.Assignee, issue.AssigneeKnown})
	}
	if want := []assignee{{"alice", true}, {"", true}, {"", false}, {"", false}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the listing's assignees = %v, want %v: a login, null, nothing, and a member that is no login", got, want)
	}
	got = nil
	for _, key := range []string{"LEGION-1", "LEGION-2", "LEGION-3"} {
		issue, err := client.GetIssue(context.Background(), key)
		if err != nil {
			t.Fatalf("get %s: %v", key, err)
		}
		got = append(got, assignee{issue.Assignee, issue.AssigneeKnown})
	}
	if want := []assignee{{"alice", true}, {"", true}, {"", false}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the detail's assignees = %v, want %v", got, want)
	}
}

// A project's open asks answer, beside the asks, the default assignee the project stores
// (Dispatch's openAsksResponse). The daemon keeps the asks of issues alone: an ask of a project
// document is the owner's `document` and is dropped. A Dispatch before the member, and a project it
// stores no default of (null), both answer no default assignee, which is never guessed from the
// person an ask waits on.
func TestOpenAsksReadsTheIssuesWithAnAskOpenAndTheProjectsDefaultAssignee(t *testing.T) {
	rows := []map[string]any{
		{"id": "ask-1", "owner": map[string]any{"issue": map[string]any{"key": "LEGION-1", "title": "t", "assignee": nil}}, "waiting_on": "human", "waiting_on_person": "dana"},
		{"id": "ask-2", "owner": map[string]any{"document": map[string]any{"project": "LEGION", "slug": "spec", "name": "spec.md"}}, "waiting_on": "human", "waiting_on_person": "dana"},
		{"id": "ask-3", "owner": map[string]any{"issue": map[string]any{"key": "LEGION-2", "title": "t", "assignee": "alice"}}, "waiting_on": "agent", "waiting_on_person": "alice"},
	}
	for name, tc := range map[string]struct {
		body map[string]any
		want string
	}{
		"a default assignee":                {body: map[string]any{"default_assignee": "dana"}, want: "dana"},
		"a project Dispatch stores none of": {body: map[string]any{"default_assignee": nil}},
		"a Dispatch before the member":      {body: map[string]any{}},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertBearer(t, r)
				if r.URL.Path != "/api/v1/asks/open" || r.URL.Query().Get("project") != "LEGION" || r.URL.Query().Get("author_session") != "" {
					t.Errorf("request = %s, want the open asks of project LEGION", r.URL)
				}
				body := map[string]any{"session_id": "", "as_of": "2026-11-26T09:00:00.123456Z", "opened_since": false, "count": 3, "asks": rows}
				for key, value := range tc.body {
					body[key] = value
				}
				writeJSON(t, w, body)
			}))
			defer server.Close()
			open, err := New(server.URL, BearerTransport(dispatchToken, nil)).OpenAsks(context.Background(), "LEGION")
			if err != nil {
				t.Fatalf("read the open asks: %v", err)
			}
			if want := (OpenAsks{AsOf: time.Date(2026, 11, 26, 9, 0, 0, 123456000, time.UTC), DefaultAssignee: tc.want,
				Asks: []OpenAsk{{ID: "ask-1", Issue: "LEGION-1"}, {ID: "ask-3", Issue: "LEGION-2"}}}); !reflect.DeepEqual(open, want) {
				t.Fatalf("open asks = %+v, want %+v: the asks of issues, the document's ask dropped", open, want)
			}
		})
	}
	for name, body := range map[string]map[string]any{
		"an answer with no time":         {"asks": []any{}},
		"an ask of an issue with no key": {"as_of": "2026-11-26T09:00:00Z", "asks": []any{map[string]any{"id": "ask-1", "owner": map[string]any{"issue": map[string]any{"key": ""}}}}},
		"an ask of an issue with no id":  {"as_of": "2026-11-26T09:00:00Z", "asks": []any{map[string]any{"id": "", "owner": map[string]any{"issue": map[string]any{"key": "LEGION-1"}}}}},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeJSON(t, w, body) }))
		if open, err := New(server.URL, BearerTransport(dispatchToken, nil)).OpenAsks(context.Background(), "LEGION"); err == nil {
			t.Errorf("%s read as %+v, want an error rather than a read with a hole", name, open)
		}
		server.Close()
	}
}

// An issue's open asks answer the person each was handed to (asked_of, null for none) and whose turn
// it is as Dispatch computes it. An open ask whose turn is neither is an error: the daemon never
// guesses it. An issue Dispatch has none of is Missing.
func TestIssueAsksReadsTheTurnAndThePersonEachOpenAskWaitsOn(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBearer(t, r)
		if r.Method != http.MethodGet || r.URL.Query().Get("state") != "open" {
			t.Errorf("request = %s %s, want the open asks", r.Method, r.URL)
		}
		switch r.URL.Path {
		case "/api/v1/issues/LEGION-1/asks":
			writeJSON(t, w, []map[string]any{
				{"id": "ask-1", "state": "open", "asked_of": "dana", "waiting_on": "human", "waiting_on_person": "dana"},
				{"id": "ask-2", "state": "open", "asked_of": nil, "waiting_on": "agent", "waiting_on_person": "alice"},
				{"id": "ask-3", "state": "open", "asked_of": nil, "waiting_on": "human", "waiting_on_person": "alice"},
			})
		case "/api/v1/issues/LEGION-2/asks":
			writeJSON(t, w, []map[string]any{{"id": "ask-4", "state": "open", "asked_of": nil, "waiting_on": ""}})
		case "/api/v1/issues/LEGION-3/asks":
			writeJSON(t, w, []map[string]any{})
		default:
			writeJSONStatus(t, w, http.StatusNotFound, map[string]string{"code": "ISSUE_NOT_FOUND", "error": "issue not found"})
		}
	}))
	defer server.Close()
	client := New(server.URL, BearerTransport(dispatchToken, nil))

	asks, err := client.IssueAsks(context.Background(), "LEGION-1")
	if err != nil {
		t.Fatalf("read the asks: %v", err)
	}
	if want := []IssueAsk{{ID: "ask-1", AskedOf: "dana", WaitsOnPerson: true}, {ID: "ask-2"}, {ID: "ask-3", WaitsOnPerson: true}}; !reflect.DeepEqual(asks, want) {
		t.Fatalf("the asks = %+v, want %+v", asks, want)
	}
	if asks, err := client.IssueAsks(context.Background(), "LEGION-3"); err != nil || len(asks) != 0 {
		t.Fatalf("an issue with no ask open: %+v, %v; want none", asks, err)
	}
	if asks, err := client.IssueAsks(context.Background(), "LEGION-2"); err == nil {
		t.Fatalf("an open ask that waits on no one read as %+v, want an error", asks)
	}
	if _, err := client.IssueAsks(context.Background(), "LEGION-9"); !Missing(err) {
		t.Fatalf("an issue Dispatch has none of: %v, want a 404 Missing reports", err)
	}
}
