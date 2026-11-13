package dispatch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// The change set is read as Dispatch's route writes it (its api.artifactChanges): the pair it
// compared, whether the decision blocks were the whole change, and each changed section and typed
// block with how it changed. The pair is asked for in the query, so the route compares the two
// versions the caller names.
func TestArtifactChangesReadsTheChangeSetDispatchAnswers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/artifacts/doc-7/changes" {
			t.Fatalf("request = %s %s, want GET /api/v1/artifacts/doc-7/changes", r.Method, r.URL.Path)
		}
		if from, to := r.URL.Query().Get("from"), r.URL.Query().Get("to"); from != "3" || to != "5" {
			t.Fatalf("query from=%q to=%q, want from=3 to=5", from, to)
		}
		assertBearer(t, r)
		writeJSON(t, w, map[string]any{
			"artifact_id":   "doc-7",
			"from":          3,
			"to":            5,
			"decision_only": false,
			"sections": []map[string]any{
				{"heading": "", "occurrence": 0, "change": "changed"},
				{"heading": "Requirements", "occurrence": 0, "change": "changed"},
				{"heading": "Rollout", "occurrence": 1, "change": "added"},
			},
			"blocks": []map[string]any{
				{"id": "r2", "type": "requirement", "change": "changed"},
				{"id": "c-live", "type": "check", "change": "removed"},
			},
		})
	}))
	defer server.Close()

	changes, err := New(server.URL, dispatchToken).ArtifactChanges(context.Background(), "doc-7", 3, 5)
	if err != nil {
		t.Fatalf("read the change set: %v", err)
	}
	want := ArtifactChanges{
		ArtifactID: "doc-7", From: 3, To: 5,
		Sections: []SectionChange{
			{Heading: "", Occurrence: 0, Change: ChangeChanged},
			{Heading: "Requirements", Occurrence: 0, Change: ChangeChanged},
			{Heading: "Rollout", Occurrence: 1, Change: ChangeAdded},
		},
		Blocks: []BlockChange{
			{ID: "r2", Type: "requirement", Change: ChangeChanged},
			{ID: "c-live", Type: "check", Change: ChangeRemoved},
		},
	}
	if !reflect.DeepEqual(changes, want) {
		t.Fatalf("change set = %#v, want %#v", changes, want)
	}
}

// Two versions whose only difference is an answered decision block are one decision-only change:
// the route says so and names no section and no block, and the client keeps both lists empty
// rather than reading the change as unknown.
func TestArtifactChangesReadsADecisionOnlyChange(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"artifact_id": "doc-7", "from": 4, "to": 5, "decision_only": true,
			"sections": []any{}, "blocks": []any{}})
	}))
	defer server.Close()

	changes, err := New(server.URL, dispatchToken).ArtifactChanges(context.Background(), "doc-7", 4, 5)
	if err != nil {
		t.Fatalf("read the change set: %v", err)
	}
	if !changes.DecisionOnly || len(changes.Sections) != 0 || len(changes.Blocks) != 0 {
		t.Fatalf("change set = %#v, want a decision-only change naming no section and no block", changes)
	}
}

// A Dispatch whose release has no changes route answers 404 from its router, or 405 where the
// path matches a route of another method; either is ErrNoChangesRoute, wrapped around Dispatch's
// own refusal so the reason a caller records keeps what Dispatch said. A 500 is Dispatch failing
// the read, which a caller rides out, and is not that error: read as one, an outage would be
// recorded as a pair nobody can ever read.
func TestArtifactChangesNamesADispatchWithoutTheRoute(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    any
		noRoute bool
	}{
		{name: "the router's 404", status: http.StatusNotFound, body: map[string]string{"code": "NOT_FOUND", "error": "no route for GET /api/v1/artifacts/doc-7/changes"}, noRoute: true},
		{name: "a 405 for a path another method serves", status: http.StatusMethodNotAllowed, body: map[string]string{"error": "method not allowed"}, noRoute: true},
		{name: "a 500", status: http.StatusInternalServerError, body: map[string]string{"code": "INTERNAL", "error": "the read failed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeJSONStatus(t, w, tc.status, tc.body)
			}))
			defer server.Close()

			_, err := New(server.URL, dispatchToken).ArtifactChanges(context.Background(), "doc-7", 3, 5)
			if err == nil {
				t.Fatalf("read the change set against a %d = no error, want one", tc.status)
			}
			if got := errors.Is(err, ErrNoChangesRoute); got != tc.noRoute {
				t.Fatalf("errors.Is(%v, ErrNoChangesRoute) = %t, want %t", err, got, tc.noRoute)
			}
			var refusal *Error
			if !errors.As(err, &refusal) || refusal.Status != tc.status {
				t.Fatalf("error = %#v, want Dispatch's own %d refusal kept inside it", err, tc.status)
			}
		})
	}
}

// The approved version before a version is read from the document's review history, which
// Dispatch serves newest first with every review, changes requests among them. The answer is the
// highest version below the one asked about that a review approved: a changes request names no
// approved version however high it stands, an approval at or above the version asked about is not
// before it, and a history with no approval below it answers 0, the version a document with no
// earlier approval is read from.
func TestApprovedVersionBeforeReadsTheHighestApprovalBelowTheVersion(t *testing.T) {
	history := []map[string]any{
		{"id": "r6", "artifact_id": "doc-7", "version": 6, "state": "approved", "kind": "person", "actor": map[string]string{"kind": "user", "id": "alice"}},
		{"id": "r5", "artifact_id": "doc-7", "version": 5, "state": "changes_requested", "kind": "person", "reason": "Name the rollback."},
		{"id": "r4", "artifact_id": "doc-7", "version": 4, "state": "approved", "kind": "person"},
		{"id": "r3", "artifact_id": "doc-7", "version": 3, "state": "changes_requested", "kind": "person", "reason": "Too broad."},
		{"id": "r2", "artifact_id": "doc-7", "version": 2, "state": "approved", "kind": "person"},
	}
	for _, tc := range []struct {
		name    string
		reviews []map[string]any
		version int
		want    int
	}{
		{name: "the newest approval below the version", reviews: history, version: 7, want: 6},
		{name: "past a changes request above the newest approval below it", reviews: history, version: 6, want: 4},
		{name: "an approval of the version itself is not before it", reviews: history, version: 4, want: 2},
		{name: "no approval below the version", reviews: history, version: 2, want: 0},
		{name: "a history of changes requests alone", reviews: history[1:2], version: 9, want: 0},
		{name: "no review at all", reviews: []map[string]any{}, version: 3, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/artifacts/doc-7/reviews" {
					t.Fatalf("request = %s %s, want GET /api/v1/artifacts/doc-7/reviews", r.Method, r.URL.Path)
				}
				assertBearer(t, r)
				writeJSON(t, w, tc.reviews)
			}))
			defer server.Close()

			got, err := New(server.URL, dispatchToken).ApprovedVersionBefore(context.Background(), "doc-7", tc.version)
			if err != nil {
				t.Fatalf("read the review history: %v", err)
			}
			if got != tc.want {
				t.Fatalf("approved version before %d = %d, want %d", tc.version, got, tc.want)
			}
		})
	}
}

// A failed read of the review history is the request's failure, and never 0: answering 0 would
// read a document with an earlier approval as one with none.
func TestApprovedVersionBeforeReportsAFailedRead(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONStatus(t, w, http.StatusServiceUnavailable, map[string]string{"error": "maintenance"})
	}))
	defer server.Close()

	if got, err := New(server.URL, dispatchToken).ApprovedVersionBefore(context.Background(), "doc-7", 5); err == nil {
		t.Fatalf("approved version before 5 against a 503 = %d with no error, want the failure", got)
	}
}
