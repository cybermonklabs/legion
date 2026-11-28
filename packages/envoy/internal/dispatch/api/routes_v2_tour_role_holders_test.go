package api

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

func init() { addTourSteps("role-holders", roleHoldersTourSteps) }

// roleHoldersTourSteps are the route tour's steps of an issue's role holders: the read, the
// publication the issue's daemon makes, and that publication again naming the stored set in
// another order.
func roleHoldersTourSteps() []tourStep {
	issueMissing := []tourMissing{{Param: "key", Value: "NOPE-404", Code: apicode.IssueNotFound}}
	return []tourStep{
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/role-holder-set", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				return tourRequest{Path: map[string]string{"key": tour.issue(t).Key}, As: tourPerson("alice")}
			},
			Missing: issueMissing,
		},
		{
			Method: http.MethodPut, Pattern: "/issues/{key}/role-holder-set", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				return roleHoldersPublication(tour.issue(t), tourHolders)
			},
			Missing: issueMissing,
		},
		{
			Method: http.MethodPut, Pattern: "/issues/{key}/role-holder-set", Name: "the set it holds, in another order",
			Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				issue := tour.issue(t)
				tour.publishRoleHolders(t, issue, tourHolders)
				return roleHoldersPublication(issue, reversed(tourHolders))
			},
			Missing: issueMissing,
		},
	}
}

// tourHolders is the set the tour publishes: two roles, in workflow order.
var tourHolders = []issueRoleHolder{
	{Role: "implementer", Session: "ses-tour-implementer"},
	{Role: "tester", Session: "ses-tour-tester"},
}

// reversed is holders in the opposite order, holders itself left as it was.
func reversed(holders []issueRoleHolder) []issueRoleHolder {
	turned := make([]issueRoleHolder, len(holders))
	for index, holder := range holders {
		turned[len(holders)-1-index] = holder
	}
	return turned
}

// roleHoldersPublication is the publication of holders on issue as its project's Legion daemon
// sends it: the shared token, naming the daemon's session in actor.
func roleHoldersPublication(issue tourIssue, holders []issueRoleHolder) tourRequest {
	return tourRequest{
		Path: map[string]string{"key": issue.Key},
		Body: map[string]any{
			"holders": holders,
			"actor":   map[string]any{"kind": "session", "id": legionDaemonSessionOf(issue.Key)},
		},
		As: tourBearer("agent-token"),
	}
}

// publishRoleHolders stores holders as issue's set through the /api/v2 route, answering the set
// stored.
func (tour *routeTour) publishRoleHolders(t *testing.T, issue tourIssue, holders []issueRoleHolder) issueRoleHolders {
	t.Helper()
	published := tour.send(t, tour.stepRoute(t, tourStep{Method: http.MethodPut, Pattern: "/issues/{key}/role-holder-set"}),
		roleHoldersPublication(issue, holders), "")
	if published.Code != http.StatusOK {
		t.Fatalf("publish the role holders of %s: %d %s", issue.Key, published.Code, published.Body.String())
	}
	return decodeBody[issueRoleHolders](t, published)
}

// TestV2RoleHoldersPublicationOfTheStoredSetWritesNothing: a publication naming the set an issue
// already holds, in any order, is the no-op PUT of decision 8. It answers the stored set as it
// stands, each pair's recorded_at kept, and makes no write: it answers while another transaction
// holds the issue's row, which a write's lock would wait on. A publication that moves one role
// to another session is a change, and stores it.
func TestV2RoleHoldersPublicationOfTheStoredSetWritesNothing(t *testing.T) {
	tour := newRouteTour(t)
	route := tour.stepRoute(t, tourStep{Method: http.MethodPut, Pattern: "/issues/{key}/role-holder-set"})
	issue := tour.issue(t)
	stored := tour.publishRoleHolders(t, issue, tourHolders)

	holding, err := tour.database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin the transaction that holds the issue: %v", err)
	}
	defer holding.Rollback(t.Context())
	if _, err := holding.Exec(t.Context(), `select key from issues where key = $1 for update`, issue.Key); err != nil {
		t.Fatalf("lock the issue row: %v", err)
	}
	answered := make(chan *httptest.ResponseRecorder, 1)
	republished := tour.request(t, route, roleHoldersPublication(issue, reversed(tourHolders)), "")
	go func() { answered <- tour.serve(republished) }()
	select {
	case again := <-answered:
		if again.Code != http.StatusOK {
			t.Fatalf("the stored set again, in another order: %d %s, want 200 with the set", again.Code, again.Body.String())
		}
		if answer := decodeBody[issueRoleHolders](t, again); !sameRoleHolderAnswer(answer, stored) {
			t.Fatalf("the stored set again answered %+v, want the set as it was stored, %+v", answer, stored)
		}
	case <-time.After(10 * time.Second):
		_ = holding.Rollback(t.Context())
		<-answered
		t.Fatal("the stored set again waited on the lock of the issue's row: it wrote, where an equal publication writes nothing")
	}
	if err := holding.Rollback(t.Context()); err != nil {
		t.Fatalf("release the issue row: %v", err)
	}

	moved := tour.publishRoleHolders(t, issue, []issueRoleHolder{tourHolders[0], {Role: "tester", Session: "ses-tour-tester-2"}})
	kept := issueRoleHolders{Issue: stored.Issue, Holders: stored.Holders[:1]}
	if len(moved.Holders) != 2 || !sameRoleHolderAnswer(issueRoleHolders{Issue: moved.Issue, Holders: moved.Holders[:1]}, kept) ||
		moved.Holders[1].Session != "ses-tour-tester-2" || !moved.Holders[1].RecordedAt.After(stored.Holders[1].RecordedAt) {
		t.Fatalf("the tester moved to another session answered %+v, want the implementer kept as stored (%+v) and the tester's new session recorded anew",
			moved.Holders, stored.Holders[0])
	}
}

// sameRoleHolderAnswer reports whether two answers name one issue and one set, each pair recorded
// at one instant (sameRoleHolders, which a write compares by, reads the pairs alone).
func sameRoleHolderAnswer(left, right issueRoleHolders) bool {
	return left.Issue == right.Issue && slices.EqualFunc(left.Holders, right.Holders, func(a, b issueRoleHolderRecord) bool {
		return a.Role == b.Role && a.Session == b.Session && a.RecordedAt.Equal(b.RecordedAt)
	})
}
