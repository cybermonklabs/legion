// packages/envoy/internal/broker/judge/dispatch_test.go
// The broker's read of Dispatch: what it asks for, what it presents, which answers are the
// issue's absence and which are a failure, and that it only ever reads.
package judge

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/sjawhar/envoy/internal/brokertoken"
)

// newDispatchClient is the judge's Dispatch pointed at the stand-in, with a token file holding
// token.
func newDispatchClient(t *testing.T, d *dispatchStandIn, token string) Dispatch {
	t.Helper()
	return Dispatch{
		BaseURL: d.server.URL,
		Account: brokertoken.NewAccountToken(tokenFile(t, token)),
		HTTP:    d.server.Client(),
	}
}

// TestIssueReadsTheIssueAndItsSpecWithTheProjectedToken: two GETs, each presenting the token as
// it stands in the file, and nothing else. A read that writes would be a read the broker's
// Dispatch credential is not there for.
func TestIssueReadsTheIssueAndItsSpecWithTheProjectedToken(t *testing.T) {
	stand := newDispatch(t, "doing", testSpec)
	issue, err := newDispatchClient(t, stand, "projected-token").Issue(t.Context(), testIssue)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if issue.Key != testIssue || issue.Title != "The broker's model judge" || issue.Status != "doing" {
		t.Errorf("Issue = %+v; want the key, title and status Dispatch showed", issue)
	}
	if !issue.HasSpec || issue.Spec != testSpec {
		t.Errorf("the spec reads has_spec %v, %q; want the document's whole markdown", issue.HasSpec, issue.Spec)
	}
	calls := stand.got()
	want := []dispatchCall{
		{http.MethodGet, "/api/v1/issues/" + testIssue, "Bearer projected-token"},
		{http.MethodGet, "/api/v1/issues/" + testIssue + "/artifacts/spec/text", "Bearer projected-token"},
	}
	if len(calls) != len(want) {
		t.Fatalf("Dispatch got %+v; want exactly the two reads", calls)
	}
	for i, call := range calls {
		if call != want[i] {
			t.Errorf("call %d is %+v; want %+v", i, call, want[i])
		}
	}
}

// TestIssueReadsTheTokenAgainForEveryCall: a token rotated under the broker is used by the next
// call, with no restart, as secrets.AzureLogin's projected token is. The reader's cache is
// lapsed here (TTL zero, as brokertoken.AccountToken's own doc says such a test does), since a
// broker's reader holds what it read for brokertoken.AccountTokenCacheTTL and the rotation this
// proves is one a run meets 30 seconds later.
func TestIssueReadsTheTokenAgainForEveryCall(t *testing.T) {
	stand := newDispatch(t, "doing", testSpec)
	path := filepath.Join(t.TempDir(), "dispatch-token")
	if err := os.WriteFile(path, []byte("first-token\n"), 0o600); err != nil {
		t.Fatalf("write the token file: %v", err)
	}
	dispatch := Dispatch{
		BaseURL: stand.server.URL,
		Account: &brokertoken.AccountToken{Path: path},
		HTTP:    stand.server.Client(),
	}
	if _, err := dispatch.Issue(t.Context(), testIssue); err != nil {
		t.Fatalf("the first read: %v", err)
	}
	if err := os.WriteFile(path, []byte("second-token\n"), 0o600); err != nil {
		t.Fatalf("rotate the token file: %v", err)
	}
	if _, err := dispatch.Issue(t.Context(), testIssue); err != nil {
		t.Fatalf("the read after the rotation: %v", err)
	}
	calls := stand.got()
	if len(calls) != 4 || calls[0].authorization != "Bearer first-token" || calls[2].authorization != "Bearer second-token" {
		t.Errorf("Dispatch got %+v; want the first read on the first token and the second on the rotated one", calls)
	}
}

// TestIssueTellsAMissingIssueFromAFailedRead: a 404 on the issue is ErrNoIssue, which is the
// judge's issue check, the last of its six, failing, and every other refusal is a failure of the
// read, which is a judge that cannot tell whether the issue is open.
func TestIssueTellsAMissingIssueFromAFailedRead(t *testing.T) {
	for _, c := range []struct {
		status int
		want   error
	}{
		{http.StatusNotFound, ErrNoIssue},
		{http.StatusUnauthorized, nil},
		{http.StatusInternalServerError, nil},
		{http.StatusBadGateway, nil},
	} {
		stand := newDispatch(t, "doing", testSpec)
		stand.issueStatus = c.status
		_, err := newDispatchClient(t, stand, "projected-token").Issue(t.Context(), testIssue)
		switch {
		case err == nil:
			t.Errorf("Dispatch answering %d read as an issue; want a failure", c.status)
		case c.want != nil && !errors.Is(err, c.want):
			t.Errorf("Dispatch answering %d = %v; want %v", c.status, err, c.want)
		case c.want == nil && errors.Is(err, ErrNoIssue):
			t.Errorf("Dispatch answering %d read as no such issue; want a failure of the read", c.status)
		}
		if calls := stand.got(); len(calls) != 1 {
			t.Errorf("Dispatch got %d calls for a refused issue read; want the spec never asked for", len(calls))
		}
	}
}

// TestAnIssueWithNoSpecDocumentIsNoFailure: the spec read answering 404, or saying what it holds
// is no document, leaves HasSpec false and the issue itself intact.
func TestAnIssueWithNoSpecDocumentIsNoFailure(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusBadRequest} {
		stand := newDispatch(t, "doing", testSpec)
		stand.specStatus = status
		issue, err := newDispatchClient(t, stand, "projected-token").Issue(t.Context(), testIssue)
		switch {
		case err != nil:
			t.Errorf("the spec answering %d = %v; want no error", status, err)
		case issue.HasSpec || issue.Spec != "":
			t.Errorf("the spec answering %d left has_spec %v, %q; want the absence", status, issue.HasSpec, issue.Spec)
		case issue.Key != testIssue || issue.Status != "doing":
			t.Errorf("the spec answering %d left the issue %+v; want it read all the same", status, issue)
		}
	}
	stand := newDispatch(t, "doing", testSpec)
	stand.specStatus = http.StatusInternalServerError
	if _, err := newDispatchClient(t, stand, "projected-token").Issue(t.Context(), testIssue); err == nil {
		t.Error("the spec answering 500 read as an issue with no spec; want a failure of the read")
	}
}

// TestIssueRefusesAnUnreadableToken: a token file the broker cannot read, or one that is empty,
// is this deployment's own misconfiguration, and no call is made at all.
func TestIssueRefusesAnUnreadableToken(t *testing.T) {
	stand := newDispatch(t, "doing", testSpec)
	for _, c := range []struct{ name, token string }{
		{"an empty file", ""},
		{"a file of spaces", "   \n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := newDispatchClient(t, stand, c.token).Issue(t.Context(), testIssue); err == nil {
				t.Error("Issue read an issue with no token; want a refusal")
			}
		})
	}
	missing := Dispatch{
		BaseURL: stand.server.URL,
		Account: brokertoken.NewAccountToken(filepath.Join(t.TempDir(), "absent")),
		HTTP:    stand.server.Client(),
	}
	if _, err := missing.Issue(t.Context(), testIssue); err == nil {
		t.Error("Issue read an issue with no token file; want a refusal")
	}
	if calls := stand.got(); len(calls) != 0 {
		t.Errorf("Dispatch got %d calls with no token to present; want none", len(calls))
	}
}
