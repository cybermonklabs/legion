// The client's two reads of an issue's change request (ChangeRequest, SpecEnding) against
// Dispatch's change-request routes, as the daemon's design gate makes them.
package dispatch

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// specIssue is the issue key every case reads, built at run time from a project name and a number.
var specIssue = fmt.Sprintf("%s-%d", "ACME", 7)

// specHead is the pull request head the ending cases read.
const specHead = "0123456789abcdef0123456789abcdef01234567"

// specServer is Dispatch's stand-in for one read: it takes the GET the client makes on path with
// query under the daemon's bearer and answers status with body. A call to any other route, query or
// method stops the test, so a client that reads the wrong route fails by name.
func specServer(t *testing.T, path, query string, status int, body string) *HTTPClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.EscapedPath() != path || r.URL.RawQuery != query {
			t.Errorf("request = %s %s?%s, want GET %s?%s", r.Method, r.URL.EscapedPath(), r.URL.RawQuery, path, query)
			http.Error(w, "no route", http.StatusTeapot)
			return
		}
		assertBearer(t, r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return New(server.URL, dispatchToken)
}

// A change request is read at the version the caller names, and the answer carries what the gate
// decides from: whether the repository is a spec source, whether the version proposes a node or
// changes a check, the judgment of its conclusion and the version Dispatch read.
func TestChangeRequestReadsTheVersionTheCallerNames(t *testing.T) {
	client := specServer(t, "/api/v1/issues/"+specIssue+"/change-request", "version=3", http.StatusOK, `{
		"issue": "`+specIssue+`", "version": 3, "spec_source": true, "repo": "acme/widgets", "verification_version": 2,
		"proposes": false, "changes_checks": false, "proposals": [], "changed_checks": [], "problems": [],
		"conclusion": null, "judgment": "none", "judgment_reasons": [], "final_list": null}`)
	got, err := client.ChangeRequest(context.Background(), specIssue, 3)
	if err != nil {
		t.Fatalf("read the change request of %s: %v", specIssue, err)
	}
	want := ChangeRequest{Issue: specIssue, Version: 3, SpecSource: true, Repo: "acme/widgets", VerificationVersion: 2, Judgment: "none"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("change request = %+v, want %+v", got, want)
	}
	if !got.ProposesNothing() {
		t.Fatalf("a version that proposes no node and changes no check reads as proposing something: %+v", got)
	}
	for _, proposing := range []ChangeRequest{{Proposes: true}, {ChangesChecks: true}} {
		if proposing.ProposesNothing() {
			t.Errorf("%+v reads as proposing nothing", proposing)
		}
	}
}

// A Dispatch older than the route answers its router's 404 under NOT_FOUND, "no route for ...":
// the caller tells it by NoRoute and keeps today's rule. A refusal of the route itself, such as a
// version the spec does not hold, is no such answer.
func TestChangeRequestOfAnOlderDispatchIsNoRoute(t *testing.T) {
	older := specServer(t, "/api/v1/issues/"+specIssue+"/change-request", "version=3", http.StatusNotFound,
		`{"code":"NOT_FOUND","error":"no route for GET /api/v1/issues/`+specIssue+`/change-request"}`)
	if _, err := older.ChangeRequest(context.Background(), specIssue, 3); !NoRoute(err) {
		t.Fatalf("an older Dispatch's answer = %v, want NoRoute", err)
	}
	missing := specServer(t, "/api/v1/issues/"+specIssue+"/change-request", "version=9", http.StatusNotFound,
		`{"code":"VERSION_NOT_FOUND","error":"the spec holds no version 9"}`)
	if _, err := missing.ChangeRequest(context.Background(), specIssue, 9); err == nil || NoRoute(err) {
		t.Fatalf("a version the spec does not hold = %v, want an error that is no NoRoute", err)
	}
}

// The ending is read at the head the caller names, and its answer carries the state, each reason
// with its code and detail in Dispatch's order, the version Dispatch read and the head.
func TestSpecEndingReadsTheHeadTheCallerNames(t *testing.T) {
	client := specServer(t, "/api/v1/issues/"+specIssue+"/change-request/ending", "head="+specHead, http.StatusOK, `{
		"issue": "`+specIssue+`", "head": "`+specHead+`", "version": 4, "state": "waiting",
		"reasons": [{"code": "no_final_list_for_head", "detail": "No final list is recorded."},
			{"code": "no_conclusion", "detail": "The spec holds no conclusion block."}],
		"judgment": "none", "judgment_reasons": [], "approved": false, "final_list": null}`)
	got, err := client.SpecEnding(context.Background(), specIssue, specHead)
	if err != nil {
		t.Fatalf("read the ending of %s: %v", specIssue, err)
	}
	want := SpecEnding{Issue: specIssue, Head: specHead, Version: 4, State: "waiting", Judgment: "none", Reasons: []EndingReason{
		{Code: "no_final_list_for_head", Detail: "No final list is recorded."},
		{Code: "no_conclusion", Detail: "The spec holds no conclusion block."}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ending = %+v, want %+v", got, want)
	}
	if got.Ready() {
		t.Fatalf("a waiting ending reads as ready")
	}
	if !(SpecEnding{State: "ready"}).Ready() {
		t.Fatalf("a ready ending reads as waiting")
	}
}

// An ending of an older Dispatch is NoRoute too, and a refusal of the head is no such answer.
func TestSpecEndingOfAnOlderDispatchIsNoRoute(t *testing.T) {
	older := specServer(t, "/api/v1/issues/"+specIssue+"/change-request/ending", "head="+specHead, http.StatusNotFound,
		`{"code":"NOT_FOUND","error":"no route for GET /api/v1/issues/`+specIssue+`/change-request/ending"}`)
	if _, err := older.SpecEnding(context.Background(), specIssue, specHead); !NoRoute(err) {
		t.Fatalf("an older Dispatch's answer = %v, want NoRoute", err)
	}
	refused := specServer(t, "/api/v1/issues/"+specIssue+"/change-request/ending", "head=nothex", http.StatusBadRequest,
		`{"code":"INVALID_HEAD","error":"head is no commit"}`)
	if _, err := refused.SpecEnding(context.Background(), specIssue, "nothex"); err == nil || NoRoute(err) {
		t.Fatalf("a refused head = %v, want an error that is no NoRoute", err)
	}
}
