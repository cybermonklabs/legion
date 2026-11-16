package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/legionagents"
)

// putRoleHoldersAs sends PUT /api/v1/issues/{key}/role-holders over the deployment's bearer, the
// credential every pane of a Legion project holds, with the body given whole, so a row can send
// what the daemon never would.
func putRoleHoldersAs(t *testing.T, handler http.Handler, issueKey string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return bearerRequest(t, handler, http.MethodPut, "/api/v1/issues/"+issueKey+"/role-holders", body)
}

// daemonSessionOf is the actor the Legion daemon of project writes as.
func daemonSessionOf(project string) map[string]any {
	return map[string]any{"kind": "session", "id": "legion-daemon:" + project}
}

// publishRoleHolders publishes an issue's whole set as the daemon of project ACME does, and
// answers what the route stored.
func publishRoleHolders(t *testing.T, handler http.Handler, issueKey string, holders []issueRoleHolder) issueRoleHolders {
	t.Helper()
	response := putRoleHoldersAs(t, handler, issueKey, map[string]any{"holders": holders, "actor": daemonSessionOf("ACME")})
	if response.Code != http.StatusOK {
		t.Fatalf("publish %v on %s: status=%d body=%s", holders, issueKey, response.Code, response.Body.String())
	}
	return decodeBody[issueRoleHolders](t, response)
}

// readRoleHolders reads an issue's stored set back off GET /api/v1/issues/{key}/role-holders.
func readRoleHolders(t *testing.T, handler http.Handler, issueKey string) issueRoleHolders {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issueKey+"/role-holders", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the role holders of %s: status=%d body=%s", issueKey, response.Code, response.Body.String())
	}
	return decodeBody[issueRoleHolders](t, response)
}

// requireHeldRole fails unless heldRole, the read the results route makes in its own write
// transaction, answers want for session on issueKey.
func requireHeldRole(t *testing.T, database *store.Store, issueKey, session, want string) {
	t.Helper()
	got, err := heldRole(context.Background(), database.Pool, issueKey, session)
	if err != nil {
		t.Fatalf("read the role %s holds on %s: %v", session, issueKey, err)
	}
	if got != want {
		t.Fatalf("session %q holds %q on %s, want %q", session, got, issueKey, want)
	}
}

// roleHolderIssue makes project ACME and one issue in it.
func roleHolderIssue(t *testing.T, handler http.Handler) string {
	t.Helper()
	createAdviceProject(t, handler, "ACME")
	return createAdviceIssue(t, handler, "ACME", "Role holders", nil).Key
}

// countRoleHolders counts every row of issue_role_holders, on any issue.
func countRoleHolders(t *testing.T, database *store.Store) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from issue_role_holders`).Scan(&count); err != nil {
		t.Fatalf("count the role holders: %v", err)
	}
	return count
}

// The daemon publishes the issue's whole set each time. A session that a later set does not name
// holds no role of the issue, and the empty set leaves no session with a role.
func TestTheDaemonPublishesTheRoleHoldersOfAnIssue(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := roleHolderIssue(t, handler)

	published := publishRoleHolders(t, handler, issue, []issueRoleHolder{{Role: "tester", Session: "ses-a"}})
	if published.Issue != issue || len(published.Holders) != 1 || published.Holders[0].Role != "tester" || published.Holders[0].Session != "ses-a" {
		t.Fatalf("the publish answered %#v, want %s with tester ses-a", published, issue)
	}
	requireHeldRole(t, database, issue, "ses-a", "tester")
	requireHeldRole(t, database, issue, "", "")

	publishRoleHolders(t, handler, issue, []issueRoleHolder{{Role: "tester", Session: "ses-b"}})
	requireHeldRole(t, database, issue, "ses-a", "")
	requireHeldRole(t, database, issue, "ses-b", "tester")

	publishRoleHolders(t, handler, issue, []issueRoleHolder{})
	requireHeldRole(t, database, issue, "ses-a", "")
	requireHeldRole(t, database, issue, "ses-b", "")
}

// Two sessions that change roles in one publication meet neither key of the table: the
// publication deletes each pair that it does not name before it inserts the new pairs.
func TestTwoSessionsThatChangeRolesInOnePublication(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := roleHolderIssue(t, handler)

	publishRoleHolders(t, handler, issue, []issueRoleHolder{{Role: "tester", Session: "ses-a"}, {Role: "implementer", Session: "ses-b"}})
	publishRoleHolders(t, handler, issue, []issueRoleHolder{{Role: "tester", Session: "ses-b"}, {Role: "implementer", Session: "ses-a"}})
	requireHeldRole(t, database, issue, "ses-a", "implementer")
	requireHeldRole(t, database, issue, "ses-b", "tester")
}

// The daemon publishes again at each boot and at each start and end of a claim. A pair that did
// not change keeps the time that Dispatch first recorded it.
func TestAnUnchangedHolderKeepsItsTime(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := roleHolderIssue(t, handler)

	first := publishRoleHolders(t, handler, issue, []issueRoleHolder{{Role: "tester", Session: "ses-a"}})
	if len(first.Holders) != 1 || first.Holders[0].RecordedAt.IsZero() {
		t.Fatalf("the first publish answered %#v, want tester ses-a with its time", first)
	}
	// Each write is its own transaction, so a write that sets now() again sets a later time.
	time.Sleep(5 * time.Millisecond)
	again := publishRoleHolders(t, handler, issue, []issueRoleHolder{{Role: "tester", Session: "ses-a"}})
	if len(again.Holders) != 1 || !again.Holders[0].RecordedAt.Equal(first.Holders[0].RecordedAt) {
		t.Fatalf("the second publish answered %#v, want the time %s of the first", again, first.Holders[0].RecordedAt)
	}
	if read := readRoleHolders(t, handler, issue); len(read.Holders) != 1 || !read.Holders[0].RecordedAt.Equal(first.Holders[0].RecordedAt) {
		t.Fatalf("the read answered %#v, want the time %s of the first publish", read, first.Holders[0].RecordedAt)
	}
}

// Only the daemon's own session for the issue's project writes the set. A role session, the daemon
// of another project and a person are each refused, and no row is written.
func TestOnlyTheDaemonOfTheIssuesProjectNamesTheRoleHolders(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := roleHolderIssue(t, handler)
	holders := []issueRoleHolder{{Role: "tester", Session: "ses-a"}}

	refusals := map[string]*httptest.ResponseRecorder{
		"a role session": putRoleHoldersAs(t, handler, issue, map[string]any{
			"holders": holders, "actor": map[string]any{"kind": "session", "id": "ses-a"},
		}),
		"the daemon of another project": putRoleHoldersAs(t, handler, issue, map[string]any{
			"holders": holders, "actor": daemonSessionOf("OTHER"),
		}),
		// A person's own credential decides the actor, so a body that names the daemon does not
		// make the person the daemon.
		"a person": dispatchRequest(t, handler, http.MethodPut, "/api/v1/issues/"+issue+"/role-holders", map[string]any{
			"holders": holders, "actor": daemonSessionOf("ACME"),
		}, "alice"),
	}
	for caller, response := range refusals {
		code, _ := refusalOf(t, response)
		if response.Code != http.StatusForbidden || code != "ROLE_HOLDER_FORBIDDEN" {
			t.Errorf("%s: status=%d body=%s, want 403 ROLE_HOLDER_FORBIDDEN", caller, response.Code, response.Body.String())
		}
	}
	if count := countRoleHolders(t, database); count != 0 {
		t.Fatalf("issue_role_holders holds %d rows after the refusals, want 0", count)
	}
}

// A body that is not the whole set of an issue is refused before the database sees it, and an
// issue that Dispatch does not know is refused with the code the daemon ends a publication on.
func TestARoleHoldersPublicationRefusesWhatIsNoSet(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := roleHolderIssue(t, handler)

	refused := map[string]map[string]any{
		"no holders":          {"actor": daemonSessionOf("ACME")},
		"the role controller": {"holders": []issueRoleHolder{{Role: "controller", Session: "ses-a"}}, "actor": daemonSessionOf("ACME")},
		"the role ci":         {"holders": []issueRoleHolder{{Role: "ci", Session: "ses-a"}}, "actor": daemonSessionOf("ACME")},
		"a role named twice": {"holders": []issueRoleHolder{
			{Role: "tester", Session: "ses-a"}, {Role: "tester", Session: "ses-b"},
		}, "actor": daemonSessionOf("ACME")},
		"an empty session": {"holders": []issueRoleHolder{{Role: "tester", Session: "  "}}, "actor": daemonSessionOf("ACME")},
		"a session named twice": {"holders": []issueRoleHolder{
			{Role: "tester", Session: "ses-a"}, {Role: "implementer", Session: "ses-a"},
		}, "actor": daemonSessionOf("ACME")},
	}
	for name, body := range refused {
		response := putRoleHoldersAs(t, handler, issue, body)
		code, _ := refusalOf(t, response)
		if response.Code != http.StatusBadRequest || code != "INVALID_ROLE_HOLDER" {
			t.Errorf("%s: status=%d body=%s, want 400 INVALID_ROLE_HOLDER", name, response.Code, response.Body.String())
		}
	}
	if count := countRoleHolders(t, database); count != 0 {
		t.Fatalf("issue_role_holders holds %d rows after the refusals, want 0", count)
	}

	unknown := putRoleHoldersAs(t, handler, "ACME-999", map[string]any{"holders": []issueRoleHolder{}, "actor": daemonSessionOf("ACME")})
	if code, _ := refusalOf(t, unknown); unknown.Code != http.StatusNotFound || code != "ISSUE_NOT_FOUND" {
		t.Fatalf("an unknown issue: status=%d body=%s, want 404 ISSUE_NOT_FOUND", unknown.Code, unknown.Body.String())
	}
}

// The tester's production check comes after the close, so a closed issue takes its role holders.
func TestAClosedIssueTakesItsRoleHolders(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := roleHolderIssue(t, handler)
	setIssueStatus(t, handler, issue, "done")

	publishRoleHolders(t, handler, issue, []issueRoleHolder{{Role: "tester", Session: "ses-a"}})
	requireHeldRole(t, database, issue, "ses-a", "tester")
}

// The set reads back in the order the workflow reaches the roles, whatever order the publication
// named them in. The PUT answers in that order too.
func TestTheRoleHoldersOfAnIssueReadInWorkflowOrder(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := roleHolderIssue(t, handler)
	if none := readRoleHolders(t, handler, issue); none.Issue != issue || none.Holders == nil || len(none.Holders) != 0 {
		t.Fatalf("the read of an issue with no holder answered %#v, want %s with []", none, issue)
	}

	published := publishRoleHolders(t, handler, issue, []issueRoleHolder{{Role: "tester", Session: "ses-t"}, {Role: "implementer", Session: "ses-i"}})
	want := []issueRoleHolder{{Role: "implementer", Session: "ses-i"}, {Role: "tester", Session: "ses-t"}}
	pairs := func(answer issueRoleHolders) []issueRoleHolder {
		got := []issueRoleHolder{}
		for _, holder := range answer.Holders {
			got = append(got, issueRoleHolder{Role: holder.Role, Session: holder.Session})
		}
		return got
	}
	if got := pairs(published); !slices.Equal(got, want) {
		t.Fatalf("the publish answered %#v, want %#v", got, want)
	}
	if got := pairs(readRoleHolders(t, handler, issue)); !slices.Equal(got, want) {
		t.Fatalf("the read answered %#v, want %#v", got, want)
	}
	// The deployment's bearer reads it too: the daemon and every pane hold that credential.
	bearer := bearerRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue+"/role-holders", nil)
	if bearer.Code != http.StatusOK {
		t.Fatalf("the bearer's read: status=%d body=%s", bearer.Code, bearer.Body.String())
	}
	if got := pairs(decodeBody[issueRoleHolders](t, bearer)); !slices.Equal(got, want) {
		t.Fatalf("the bearer's read answered %#v, want %#v", got, want)
	}

	unknown := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/ACME-999/role-holders", nil, "alice")
	if code, _ := refusalOf(t, unknown); unknown.Code != http.StatusNotFound || code != "ISSUE_NOT_FOUND" {
		t.Fatalf("the read of an unknown issue: status=%d body=%s, want 404 ISSUE_NOT_FOUND", unknown.Code, unknown.Body.String())
	}
}

// The contract names the roles in the enum of each shape, which a tag must spell. The route refuses
// every role that legionagents.Roles does not name, so the two lists must be the same.
func TestTheRoleHolderShapesNameTheLegionRoles(t *testing.T) {
	want := strings.Join(legionagents.Roles, ",")
	for _, shape := range []any{issueRoleHolder{}, issueRoleHolderRecord{}} {
		field, ok := reflect.TypeOf(shape).FieldByName("Role")
		if !ok {
			t.Fatalf("%T has no Role", shape)
		}
		if got := field.Tag.Get("enum"); got != want {
			t.Errorf("%T.Role names the roles %q, want %q", shape, got, want)
		}
	}
}
