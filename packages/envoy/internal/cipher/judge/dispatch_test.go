// packages/envoy/internal/cipher/judge/dispatch_test.go
// The broker's read of Dispatch: what it asks for, what it presents, which answers are the
// issue's absence and which are a failure, and that it only ever reads.
package judge

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
)

// newDispatchClient is the judge's Dispatch pointed at the stand-in, with a token file holding
// token.
func newDispatchClient(t *testing.T, d *dispatchStandIn, token string) Dispatch {
	t.Helper()
	return NewDispatch(d.server.URL, ciphertoken.NewAccountToken("BROKER_JUDGE_DISPATCH_TOKEN_FILE", tokenFile(t, token)), d.server.Client())
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
// lapsed here (TTL zero, as ciphertoken.AccountToken's own doc says such a test does), since a
// broker's reader holds what it read for ciphertoken.AccountTokenCacheTTL and the rotation this
// proves is one a run meets 30 seconds later.
func TestIssueReadsTheTokenAgainForEveryCall(t *testing.T) {
	stand := newDispatch(t, "doing", testSpec)
	path := filepath.Join(t.TempDir(), "dispatch-token")
	if err := os.WriteFile(path, []byte("first-token\n"), 0o600); err != nil {
		t.Fatalf("write the token file: %v", err)
	}
	dispatch := NewDispatch(stand.server.URL, &ciphertoken.AccountToken{Path: path}, stand.server.Client())
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

// TestIssueTellsAMissingIssueFromAFailedRead: a 404 on the issue is ErrNoIssue, which the judge
// shows the model as an issue that is not found, and every other refusal is a failure of the
// read, which hands the request to its person.
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
	missing := NewDispatch(stand.server.URL, ciphertoken.NewAccountToken("BROKER_JUDGE_DISPATCH_TOKEN_FILE", filepath.Join(t.TempDir(), "absent")), stand.server.Client())
	if _, err := missing.Issue(t.Context(), testIssue); err == nil {
		t.Error("Issue read an issue with no token file; want a refusal")
	}
	if calls := stand.got(); len(calls) != 0 {
		t.Errorf("Dispatch got %d calls with no token to present; want none", len(calls))
	}
}

// TestAssigneeReadsTheIssueAlone: the broker asks Dispatch whose issue a Legion pod's claim names
// in one GET of the issue, with the projected token, and never reads its spec. The answer is the
// issue's assignee, and an issue nobody is assigned to answers "". A 404 is ErrNoIssue and any
// other refusal a failure of the read, as for Issue.
func TestAssigneeReadsTheIssueAlone(t *testing.T) {
	stand := newDispatch(t, "doing", testSpec)
	stand.issue["assignee"] = "ada@example.com"
	dispatch := newDispatchClient(t, stand, "projected-token")
	if got, err := dispatch.Assignee(t.Context(), testIssue); err != nil || got != "ada@example.com" {
		t.Fatalf("Assignee = %q, %v; want ada@example.com", got, err)
	}
	if calls := stand.got(); len(calls) != 1 || calls[0] != (dispatchCall{http.MethodGet, "/api/v1/issues/" + testIssue, "Bearer projected-token"}) {
		t.Fatalf("Dispatch got %+v; want exactly the one issue read", calls)
	}

	stand.issue["assignee"] = nil
	if got, err := dispatch.Assignee(t.Context(), testIssue); err != nil || got != "" {
		t.Fatalf("Assignee of an unassigned issue = %q, %v; want \"\"", got, err)
	}
	stand.issueStatus = http.StatusNotFound
	if _, err := dispatch.Assignee(t.Context(), testIssue); !errors.Is(err, ErrNoIssue) {
		t.Fatalf("Assignee of an issue Dispatch does not hold = %v; want ErrNoIssue", err)
	}
	stand.issueStatus = http.StatusInternalServerError
	if _, err := dispatch.Assignee(t.Context(), testIssue); err == nil || errors.Is(err, ErrNoIssue) {
		t.Fatalf("Assignee with Dispatch answering 500 = %v; want a failure of the read", err)
	}
}

// TestTheJudgesIssueReadDoesNotReadTheAssignee: the judge's Issue decodes the issue's key, title
// and status and nothing else, so an assignee Dispatch answers in another shape than a string
// leaves the judge's read as it is, and fails the assignee read alone.
func TestTheJudgesIssueReadDoesNotReadTheAssignee(t *testing.T) {
	stand := newDispatch(t, "doing", testSpec)
	stand.issue["assignee"] = map[string]any{"login": "ada@example.com"}
	dispatch := newDispatchClient(t, stand, "projected-token")
	if issue, err := dispatch.Issue(t.Context(), testIssue); err != nil || issue.Key != testIssue || issue.Status != "doing" || !issue.HasSpec {
		t.Fatalf("Issue with an assignee that is no string = %+v, %v; want the issue read as before", issue, err)
	}
	if got, err := dispatch.Assignee(t.Context(), testIssue); err == nil {
		t.Fatalf("Assignee with an assignee that is no string = %q, nil; want a failure of the read", got)
	}
}

// TestDispatchReadsAnAsk: the third read, GET /api/v1/asks/{id}, which the judge makes when a
// request's reason cites an ask. It presents the same projected token, reads the ask
// out of the body the route answers with, tells an ask Dispatch does not hold (404, ErrNoAsk)
// from a read that failed, and refuses an answer that is no JSON object.
func TestDispatchReadsAnAsk(t *testing.T) {
	t.Run("the route, the token and the fields", func(t *testing.T) {
		stand := newDispatch(t, "doing", testSpec)
		ask, err := newDispatchClient(t, stand, "projected-token").Ask(t.Context(), testAsk)
		if err != nil {
			t.Fatalf("Ask: %v", err)
		}
		if ask.ID != testAsk || ask.IssueKey != testIssue || ask.State != "answered" || ask.Question != testQuestion {
			t.Errorf("Ask = %+v; want the id, issue, state and question Dispatch showed", ask)
		}
		if ask.AnsweredBy != testApprover || len(ask.Selected) != 1 || ask.Selected[0] != "Spend it" || ask.Text != testAnswerText {
			t.Errorf("the answer reads %q / %v / %q; want the person, their option and their text", ask.AnsweredBy, ask.Selected, ask.Text)
		}
		if len(ask.Options) != 2 || ask.Options[0] != "Spend it" || ask.Options[1] != "Leave it" {
			t.Errorf("the options read %v; want the labels the ask offered", ask.Options)
		}
		calls := stand.got()
		want := dispatchCall{http.MethodGet, "/api/v1/asks/" + testAsk, "Bearer projected-token"}
		if len(calls) != 1 || calls[0] != want {
			t.Errorf("Dispatch got %+v; want exactly %+v", calls, want)
		}
	})

	t.Run("an ask Dispatch does not hold, and a read that failed", func(t *testing.T) {
		for _, c := range []struct {
			status int
			want   error
		}{
			{http.StatusNotFound, ErrNoAsk},
			{http.StatusUnauthorized, nil},
			{http.StatusInternalServerError, nil},
		} {
			stand := newDispatch(t, "doing", testSpec)
			stand.askStatus = c.status
			_, err := newDispatchClient(t, stand, "projected-token").Ask(t.Context(), testAsk)
			switch {
			case err == nil:
				t.Errorf("Dispatch answering %d read as an ask; want a failure", c.status)
			case c.want != nil && !errors.Is(err, c.want):
				t.Errorf("Dispatch answering %d = %v; want %v", c.status, err, c.want)
			case c.want == nil && errors.Is(err, ErrNoAsk):
				t.Errorf("Dispatch answering %d read as no such ask; want a failure of the read", c.status)
			}
		}
	})

	t.Run("an answer that is no JSON object", func(t *testing.T) {
		stand := newDispatch(t, "doing", testSpec)
		stand.askRaw = "the stand-in forgot how to write JSON"
		if _, err := newDispatchClient(t, stand, "projected-token").Ask(t.Context(), testAsk); err == nil {
			t.Error("Ask read an ask out of an answer that is no JSON; want a failure of the read")
		}
	})

	t.Run("a token the broker cannot read", func(t *testing.T) {
		stand := newDispatch(t, "doing", testSpec)
		if _, err := newDispatchClient(t, stand, "").Ask(t.Context(), testAsk); err == nil {
			t.Error("Ask read an ask with no token; want a refusal")
		}
		if calls := stand.got(); len(calls) != 0 {
			t.Errorf("Dispatch got %d calls with no token to present; want none", len(calls))
		}
	})
}
