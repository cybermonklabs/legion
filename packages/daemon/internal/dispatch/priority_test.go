package dispatch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// An issue's own priority is Dispatch's integer from 0 (P0) to 3 (P3), or null for none: the
// listing and the detail read both carry it as Dispatch answers it, and a summary made an Issue
// keeps it.
func TestListAndDetailCarryTheIssuesOwnPriority(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBearer(t, r)
		switch r.URL.Path {
		case "/api/v1/issues":
			writeJSON(t, w, []map[string]any{
				{"key": "LEGION-1", "status": "todo", "rank": "a", "priority": 0},
				{"key": "LEGION-2", "status": "todo", "rank": "b", "priority": nil},
				{"key": "LEGION-3", "status": "todo", "rank": "c", "priority": 3},
			})
		case "/api/v1/issues/LEGION-4":
			writeJSON(t, w, map[string]any{"key": "LEGION-4", "status": "todo", "rank": "d", "priority": 1})
		default:
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	client := New(server.URL, dispatchToken)
	level := func(n int) *int { return &n }

	issues, err := client.ListIssues(context.Background(), "LEGION", nil)
	if err != nil {
		t.Fatalf("list issues: %v", err)
	}
	want := []*int{level(0), nil, level(3)}
	if len(issues) != len(want) {
		t.Fatalf("listed issues = %#v, want three", issues)
	}
	for i, issue := range issues {
		if !reflect.DeepEqual(issue.Priority, want[i]) {
			t.Fatalf("the priority of %s = %v, want %v as the listing answered it", issue.Key, issue.Priority, want[i])
		}
		if made := issue.Issue(); !reflect.DeepEqual(made.Priority, want[i]) {
			t.Fatalf("%s made an Issue carries priority %v, want %v", issue.Key, made.Priority, want[i])
		}
	}
	issue, err := client.GetIssue(context.Background(), "LEGION-4")
	if err != nil {
		t.Fatalf("get issue: %v", err)
	}
	if !reflect.DeepEqual(issue.Priority, level(1)) {
		t.Fatalf("the priority of LEGION-4 = %v, want 1 as the detail answered it", issue.Priority)
	}
}
