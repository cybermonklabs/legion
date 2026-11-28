package api

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/legionclaim"
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
	t.Parallel()
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
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := roleHolderIssue(t, handler)

	publishRoleHolders(t, handler, issue, []issueRoleHolder{{Role: "tester", Session: "ses-a"}, {Role: "implementer", Session: "ses-b"}})
	publishRoleHolders(t, handler, issue, []issueRoleHolder{{Role: "tester", Session: "ses-b"}, {Role: "implementer", Session: "ses-a"}})
	requireHeldRole(t, database, issue, "ses-a", "implementer")
	requireHeldRole(t, database, issue, "ses-b", "tester")
}

// Two claims of one issue can hold one role (the Legion daemon's record.WorkerClaim): here the
// agent of a key on the tester's step, beside the tester's own claim of an earlier version of the
// plan. The daemon publishes the session of each, with the claim it holds the role by and whether
// that claim works the role's step that started last, and Dispatch stores both: each session holds
// the role, the set reads back with both, and a later set that names one of them leaves the other's
// session with no role.
func TestTwoClaimsOfOneRoleBothHoldIt(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := roleHolderIssue(t, handler)
	claimOf := func(agent string) string {
		project, number, _ := strings.Cut(strings.ToLower(issue), "-")
		return "legion-acme-" + project + "-" + number + "-" + agent
	}
	keyed := issueRoleHolder{Role: "tester", Session: "ses-keyed", Claim: claimOf("implementer"), WorksStep: true}
	own := issueRoleHolder{Role: "tester", Session: "ses-own", Claim: claimOf("tester")}
	holders := func(answer issueRoleHolders) []issueRoleHolder {
		got := []issueRoleHolder{}
		for _, holder := range answer.Holders {
			got = append(got, issueRoleHolder{Role: holder.Role, Session: holder.Session, Claim: holder.Claim, WorksStep: holder.WorksStep})
		}
		return got
	}

	want := []issueRoleHolder{keyed, own}
	if got := holders(publishRoleHolders(t, handler, issue, []issueRoleHolder{own, keyed})); !slices.Equal(got, want) {
		t.Fatalf("the publish answered %#v, want both claims of the tester %#v", got, want)
	}
	if got := holders(readRoleHolders(t, handler, issue)); !slices.Equal(got, want) {
		t.Fatalf("the read answered %#v, want both claims of the tester %#v", got, want)
	}
	requireHeldRole(t, database, issue, "ses-keyed", "tester")
	requireHeldRole(t, database, issue, "ses-own", "tester")

	publishRoleHolders(t, handler, issue, []issueRoleHolder{keyed})
	requireHeldRole(t, database, issue, "ses-keyed", "tester")
	requireHeldRole(t, database, issue, "ses-own", "")
}

// The daemon publishes again at each boot and at each start and end of a claim. A pair that did
// not change keeps the time that Dispatch first recorded it.
func TestAnUnchangedHolderKeepsItsTime(t *testing.T) {
	t.Parallel()
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

// A publication that changes nothing writes no row. recorded_at cannot show it, since no write of
// the route sets it again, so the test reads each row's version: the transaction that wrote it
// (xmin) and where it stands (ctid), which an update replaces and a write that leaves the row
// does not. Control: a publication whose one change is that a claim now works the role's step
// writes that holder's row, and leaves the row of the holder beside it.
func TestAnUnchangedPublicationWritesNoRow(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := roleHolderIssue(t, handler)
	versions := func() map[string]string {
		t.Helper()
		rows, err := database.Pool.Query(context.Background(), `
			select session, xmin::text || ' ' || ctid::text from issue_role_holders where issue_key = $1
		`, issue)
		if err != nil {
			t.Fatalf("read the row versions of %s: %v", issue, err)
		}
		defer rows.Close()
		read := map[string]string{}
		for rows.Next() {
			var session, version string
			if err := rows.Scan(&session, &version); err != nil {
				t.Fatalf("read a row version of %s: %v", issue, err)
			}
			read[session] = version
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read the row versions of %s: %v", issue, err)
		}
		return read
	}
	set := []issueRoleHolder{{Role: "tester", Session: "ses-a"}, {Role: "implementer", Session: "ses-b", WorksStep: true}}
	publishRoleHolders(t, handler, issue, set)
	first := versions()
	if len(first) != 2 {
		t.Fatalf("the first publication stored the rows %v, want one of ses-a and one of ses-b", first)
	}

	publishRoleHolders(t, handler, issue, set)
	if again := versions(); !maps.Equal(again, first) {
		t.Fatalf("a publication of the same set wrote its rows again: their versions were %v, and are %v", first, again)
	}

	set[0].WorksStep = true
	published := publishRoleHolders(t, handler, issue, set)
	changed := versions()
	if changed["ses-a"] == first["ses-a"] {
		t.Fatalf("control: the publication in which ses-a's claim works the step left its row at version %s, want a new one", first["ses-a"])
	}
	if changed["ses-b"] != first["ses-b"] {
		t.Fatalf("the publication that changed ses-a wrote the row of ses-b too: its version was %s, and is %s", first["ses-b"], changed["ses-b"])
	}
	for _, holder := range published.Holders {
		if holder.Session == "ses-a" && !holder.WorksStep {
			t.Fatalf("the publication answered %#v, want ses-a's claim working the step", published)
		}
	}
}

// Only the daemon's own session for the issue's project writes the set. A role session, the daemon
// of another project and a person are each refused, and no row is written.
func TestOnlyTheDaemonOfTheIssuesProjectNamesTheRoleHolders(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := roleHolderIssue(t, handler)

	refused := map[string]map[string]any{
		"no holders":              {"actor": daemonSessionOf("ACME")},
		"the role controller":     {"holders": []issueRoleHolder{{Role: "controller", Session: "ses-a"}}, "actor": daemonSessionOf("ACME")},
		"a role with a capital":   {"holders": []issueRoleHolder{{Role: "Checker", Session: "ses-a"}}, "actor": daemonSessionOf("ACME")},
		"a role of 33 characters": {"holders": []issueRoleHolder{{Role: "c" + strings.Repeat("h", 32), Session: "ses-a"}}, "actor": daemonSessionOf("ACME")},
		"a claim named twice": {"holders": []issueRoleHolder{
			{Role: "tester", Session: "ses-a", Claim: "legion-acme-acme-1-tester"}, {Role: "tester", Session: "ses-b", Claim: "legion-acme-acme-1-tester"},
		}, "actor": daemonSessionOf("ACME")},
		"a claim of another issue": {"holders": []issueRoleHolder{{Role: "tester", Session: "ses-a", Claim: "legion-acme-acme-999-tester"}},
			"actor": daemonSessionOf("ACME")},
		"a claim that is no claim token": {"holders": []issueRoleHolder{{Role: "tester", Session: "ses-a", Claim: "tester"}},
			"actor": daemonSessionOf("ACME")},
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

// A session holds one role of an issue, so a publication that names one session twice is refused
// whole, whether the two entries name one role or two: two claims of one role name two sessions.
// The refusal names the session, and nothing is stored. Without it the write meets the session key
// twice in one statement, which the database refuses, and the route answers 500.
func TestASessionNamedTwiceIsRefusedNamingTheSession(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := roleHolderIssue(t, handler)
	claimOf := func(agent string) string {
		project, number, _ := strings.Cut(strings.ToLower(issue), "-")
		return "legion-acme-" + project + "-" + number + "-" + agent
	}
	const twice = "ses-twice"
	for name, holders := range map[string][]issueRoleHolder{
		"for one role": {{Role: "tester", Session: twice}, {Role: "tester", Session: twice}},
		"for one role by two claims": {
			{Role: "tester", Session: twice, Claim: claimOf("tester")}, {Role: "tester", Session: twice, Claim: claimOf("implementer")},
		},
		"for two roles": {{Role: "tester", Session: twice}, {Role: "implementer", Session: twice}},
	} {
		response := putRoleHoldersAs(t, handler, issue, map[string]any{"holders": holders, "actor": daemonSessionOf("ACME")})
		code, message := refusalOf(t, response)
		if response.Code != http.StatusBadRequest || code != "INVALID_ROLE_HOLDER" || !strings.Contains(message, twice) {
			t.Errorf("a session named twice %s: status=%d body=%s, want 400 INVALID_ROLE_HOLDER naming %s", name, response.Code, response.Body.String(), twice)
		}
	}
	if count := countRoleHolders(t, database); count != 0 {
		t.Fatalf("issue_role_holders holds %d rows after the refusals, want 0", count)
	}
}

// The tester's production check comes after the close, so a closed issue takes its role holders.
func TestAClosedIssueTakesItsRoleHolders(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := roleHolderIssue(t, handler)
	setIssueStatus(t, handler, issue, "done")

	publishRoleHolders(t, handler, issue, []issueRoleHolder{{Role: "tester", Session: "ses-a"}})
	requireHeldRole(t, database, issue, "ses-a", "tester")
}

// A role the deployment defines holds a role of an issue as Legion's six do (legionclaim.IsRole):
// a set naming checker and linter beside the tester is stored whole, and the read the results route
// makes finds the checker's session. A role is a name of the role grammar, so one of 32 characters
// is taken too. The set reads back with Legion's six in the order of the default steps, then
// every other role by name. The controller, a name with a capital and a name of 33 characters are
// each refused 400 INVALID_ROLE_HOLDER naming the grammar, and the stored set stays as it was.
func TestARoleTheDeploymentDefinesHoldsARoleOfAnIssue(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := roleHolderIssue(t, handler)
	longest := "a" + strings.Repeat("b", 31)

	published := publishRoleHolders(t, handler, issue, []issueRoleHolder{
		{Role: "linter", Session: "ses-l"}, {Role: "checker", Session: "ses-c"}, {Role: "tester", Session: "ses-t"}, {Role: longest, Session: "ses-32"},
	})
	want := []issueRoleHolder{{Role: "tester", Session: "ses-t"}, {Role: longest, Session: "ses-32"},
		{Role: "checker", Session: "ses-c"}, {Role: "linter", Session: "ses-l"}}
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
	requireHeldRole(t, database, issue, "ses-c", "checker")

	for name, role := range map[string]string{
		"the controller":          "controller",
		"a name with a capital":   "Checker",
		"a name of 33 characters": "c" + strings.Repeat("h", 32),
	} {
		response := putRoleHoldersAs(t, handler, issue, map[string]any{
			"holders": []issueRoleHolder{{Role: "tester", Session: "ses-t2"}, {Role: role, Session: "ses-x"}},
			"actor":   daemonSessionOf("ACME"),
		})
		code, message := refusalOf(t, response)
		if response.Code != http.StatusBadRequest || code != "INVALID_ROLE_HOLDER" || !strings.Contains(message, legionclaim.RolePattern) {
			t.Errorf("%s: status=%d body=%s, want 400 INVALID_ROLE_HOLDER naming the role grammar", name, response.Code, response.Body.String())
		}
	}
	if got := pairs(readRoleHolders(t, handler, issue)); !slices.Equal(got, want) {
		t.Fatalf("after the refusals the read answered %#v, want the stored set %#v", got, want)
	}
}

// The set reads back in the order of legionclaim.Roles, the order of the default steps (each
// task names its own), whatever order the publication named them in. The PUT answers in that
// order too.
func TestTheRoleHoldersOfAnIssueReadInWorkflowOrder(t *testing.T) {
	t.Parallel()
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

// sortedPairs is the set an answer names, as role and session pairs in role order.
func sortedPairs(holders []issueRoleHolderRecord) []issueRoleHolder {
	pairs := []issueRoleHolder{}
	for _, holder := range holders {
		pairs = append(pairs, issueRoleHolder{Role: holder.Role, Session: holder.Session})
	}
	slices.SortFunc(pairs, func(a, b issueRoleHolder) int { return strings.Compare(a.Role, b.Role) })
	return pairs
}

// Two publications of one issue that run at the same time leave one of the two sets, whole, and
// never a mix of both: the route replaces the issue's whole set. Each publication that answers 200
// answers the set that it stored, so the one that finished first answers its own set, and the one
// that finished last leaves its set stored. Twenty rounds of each shape, because one round can pass
// by the order the two happen to run in.
func TestTwoPublicationsOfOneIssueAtOnceLeaveOneWholeSet(t *testing.T) {
	t.Parallel()
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := roleHolderIssue(t, handler)
	for _, shape := range []struct {
		name string
		a, b []issueRoleHolder
	}{
		{"disjoint roles", []issueRoleHolder{{Role: "tester", Session: "a1"}}, []issueRoleHolder{{Role: "implementer", Session: "b1"}}},
		{"the same roles, other sessions",
			[]issueRoleHolder{{Role: "implementer", Session: "a2"}, {Role: "tester", Session: "a1"}},
			[]issueRoleHolder{{Role: "implementer", Session: "b2"}, {Role: "tester", Session: "b1"}}},
		{"two sessions that swap roles",
			[]issueRoleHolder{{Role: "implementer", Session: "s1"}, {Role: "tester", Session: "s2"}},
			[]issueRoleHolder{{Role: "implementer", Session: "s2"}, {Role: "tester", Session: "s1"}}},
	} {
		for round := range 20 {
			publishRoleHolders(t, handler, issue, []issueRoleHolder{{Role: "architect", Session: "z0"}})
			sets := [][]issueRoleHolder{shape.a, shape.b}
			codes := make([]int, 2)
			answers := make([][]issueRoleHolder, 2)
			var start, done sync.WaitGroup
			start.Add(1)
			for index, set := range sets {
				done.Add(1)
				go func() {
					defer done.Done()
					start.Wait()
					response := putRoleHoldersAs(t, handler, issue, map[string]any{"holders": set, "actor": daemonSessionOf("ACME")})
					codes[index] = response.Code
					if response.Code == http.StatusOK {
						answers[index] = sortedPairs(decodeBody[issueRoleHolders](t, response).Holders)
					}
				}()
			}
			start.Done()
			done.Wait()
			stored := sortedPairs(readRoleHolders(t, handler, issue).Holders)
			if !slices.Equal(stored, shape.a) && !slices.Equal(stored, shape.b) {
				t.Errorf("%s, round %d: the issue stores %v, want exactly %v or %v", shape.name, round+1, stored, shape.a, shape.b)
			}
			for index, set := range sets {
				if codes[index] != http.StatusOK {
					t.Errorf("%s, round %d: publication %d answered %d, want 200", shape.name, round+1, index, codes[index])
				} else if !slices.Equal(answers[index], set) {
					t.Errorf("%s, round %d: publication %d of %v answered %v, want the set it stored", shape.name, round+1, index, set, answers[index])
				}
			}
		}
	}
}

// A session that Dispatch's own rule does not take as a session id (text.IsSessionID) is refused
// with 400, and so is one so long that the table's unique index could not hold it: no body gives a
// 500, which the daemon would retry with no end. The stored set stays as it was.
func TestASessionThatIsNoSessionIDIsRefused(t *testing.T) {
	t.Parallel()
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := roleHolderIssue(t, handler)
	before := []issueRoleHolder{{Role: "tester", Session: "ses-t"}}
	// Characters that do not compress, so Postgres stores the session as long as it is.
	random := rand.New(rand.NewPCG(7, 7))
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	long := make([]byte, 3000)
	for index := range long {
		long[index] = letters[random.IntN(len(letters))]
	}
	for _, session := range []string{"ses\nx", " ses-t", "ses t", "ses\tx", "ses/x", "ses#x", string(long)} {
		publishRoleHolders(t, handler, issue, before)
		response := putRoleHoldersAs(t, handler, issue, map[string]any{
			"holders": []issueRoleHolder{{Role: "reviewer", Session: session}}, "actor": daemonSessionOf("ACME"),
		})
		label := session
		if len(label) > 20 {
			label = label[:20] + "…"
		}
		if code, _ := refusalOf(t, response); response.Code != http.StatusBadRequest || code != "INVALID_ROLE_HOLDER" {
			t.Errorf("session %q: status=%d body=%.300s, want 400 INVALID_ROLE_HOLDER", label, response.Code, response.Body.String())
		}
		if stored := sortedPairs(readRoleHolders(t, handler, issue).Holders); !slices.Equal(stored, before) {
			t.Errorf("session %q: the issue stores %v after the refusal, want %v", label, stored, before)
		}
	}

	// The bound itself: a session of exactly maxRoleHolderSessionBytes is stored, and one byte more
	// is refused. Both are well inside what the unique index holds.
	atBound := string(long[:maxRoleHolderSessionBytes])
	publishRoleHolders(t, handler, issue, []issueRoleHolder{{Role: "reviewer", Session: atBound}})
	if stored := sortedPairs(readRoleHolders(t, handler, issue).Holders); len(stored) != 1 || stored[0].Session != atBound {
		t.Fatalf("a session of %d bytes: the issue stores %v, want it stored", len(atBound), stored)
	}
	over := putRoleHoldersAs(t, handler, issue, map[string]any{
		"holders": []issueRoleHolder{{Role: "reviewer", Session: atBound + "x"}}, "actor": daemonSessionOf("ACME"),
	})
	if code, _ := refusalOf(t, over); over.Code != http.StatusBadRequest || code != "INVALID_ROLE_HOLDER" {
		t.Fatalf("a session of %d bytes: status=%d body=%.300s, want 400 INVALID_ROLE_HOLDER", len(atBound)+1, over.Code, over.Body.String())
	}
}

// The contract spells a role as the route takes it, a name of the role grammar, in the `pattern` of
// each shape, and lists no enum of roles: the route takes a role the deployment defines, which no
// list names. The tag must spell the pattern legionclaim.IsRole matches.
func TestTheRoleHolderShapesSpellTheRoleGrammar(t *testing.T) {
	t.Parallel()
	for _, shape := range []any{issueRoleHolder{}, issueRoleHolderRecord{}} {
		field, ok := reflect.TypeOf(shape).FieldByName("Role")
		if !ok {
			t.Fatalf("%T has no Role", shape)
		}
		if got, want := field.Tag.Get("pattern"), legionclaim.RolePattern; got != want {
			t.Errorf("%T.Role spells the pattern %q, want %q", shape, got, want)
		}
		if enum, listed := field.Tag.Lookup("enum"); listed {
			t.Errorf("%T.Role lists the roles %q, want no enum: a role the deployment defines is a role of an issue", shape, enum)
		}
	}
}

// Each pair of a publication names its role and its session: the route refuses a pair that leaves
// either out (putIssueRoleHolders), so the contract a caller compiles against requires both. They
// are read from the committed document (committedOpenAPI), from which the TypeScript contract is
// generated, walking from the PUT's own request body to the schema of one pair, so the schema
// read is the one the route takes and not a name that only looks like it.
func TestTheContractRequiresTheRoleAndTheSessionOfEachHolder(t *testing.T) {
	t.Parallel()
	var document map[string]any
	if err := json.Unmarshal(committedOpenAPI(t), &document); err != nil {
		t.Fatalf("read the committed document: %v", err)
	}
	paths, _ := document["paths"].(map[string]any)
	item, _ := paths["/api/v1/issues/{key}/role-holders"].(map[string]any)
	put, _ := item["put"].(map[string]any)
	body, _ := put["requestBody"].(map[string]any)
	content, _ := body["content"].(map[string]any)
	media, _ := content["application/json"].(map[string]any)
	schema, declared := media["schema"].(map[string]any)
	if !declared {
		t.Fatal("the document declares no JSON request body for PUT /api/v1/issues/{key}/role-holders")
	}
	pair := openAPISchemaChild(document, openAPISchemaChild(document, schema, "holders"), "0")
	if reference, names := pair["$ref"].(string); names {
		components, _ := document["components"].(map[string]any)
		schemas, _ := components["schemas"].(map[string]any)
		pair, _ = schemas[strings.TrimPrefix(reference, "#/components/schemas/")].(map[string]any)
	}
	properties, _ := pair["properties"].(map[string]any)
	if _, role := properties["role"]; !role {
		t.Fatalf("the schema of one pair of the PUT's holders is %v, which names no role: the walk reached no pair", pair)
	}
	required := []string{}
	listed, _ := pair["required"].([]any)
	for _, field := range listed {
		name, _ := field.(string)
		required = append(required, name)
	}
	slices.Sort(required)
	if want := []string{"role", "session"}; !slices.Equal(required, want) {
		t.Fatalf("one pair of the PUT's holders requires %q in the committed contract, want %q: the route refuses a pair that leaves either out",
			required, want)
	}
}

// The table's own check on role holds the role grammar the route takes (legionclaim.IsRole), so a
// row the route writes is never refused by the table, a 500, and a row written past the route is
// held to the same rule: a role the deployment defines and a name of 32 characters are stored, and
// the controller, a name with a capital and a name of 33 characters are refused by the check.
func TestTheRoleHoldersTableChecksTheRoleGrammar(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := roleHolderIssue(t, handler)
	insert := func(role, session string) error {
		_, err := database.Pool.Exec(context.Background(),
			`insert into issue_role_holders (issue_key, role, session) values ($1, $2, $3)`, issue, role, session)
		return err
	}
	for role, session := range map[string]string{"checker": "ses-c", "a" + strings.Repeat("b", 31): "ses-32"} {
		if err := insert(role, session); err != nil {
			t.Errorf("the table refused the role %q: %v", role, err)
		}
	}
	for _, role := range []string{"controller", "Checker", "c" + strings.Repeat("h", 32)} {
		var refusal *pgconn.PgError
		if err := insert(role, "ses-x"); !errors.As(err, &refusal) || refusal.Code != "23514" {
			t.Errorf("the table answered the role %q with %v, want its check refusing it (23514)", role, err)
		}
	}
}
