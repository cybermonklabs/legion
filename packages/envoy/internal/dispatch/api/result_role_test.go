package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/metrics"
)

// The tests of this file hold the rule of dispatch://INFRA-3994: a result's stored role is the
// role its session holds on the recording issue (issue_role_holders, which the Legion daemon
// publishes), never the role the request names, and the empty string for a session or a person
// that holds none.

// roleAnswer is the answer of POST /api/v1/issues/{key}/results with the members that say what
// became of a named role.
type roleAnswer struct {
	Result       model.CheckResult `json:"result"`
	Line         string            `json:"line"`
	RoleNotice   string            `json:"role_notice"`
	RoleNotTaken string            `json:"role_not_taken"`
}

// roleIssue makes project ACME and one issue in it whose Verification document is twoChecks,
// with holders as the issue's role holders, and answers the issue's key.
func roleIssue(t *testing.T, handler http.Handler, holders []issueRoleHolder) string {
	t.Helper()
	issue := createInteractionIssue(t, handler, "ACME", "Role of a result", "A spec\n")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	publishRoleHolders(t, handler, issue.Key, holders)
	return issue.Key
}

// recordAs records one result on issue as session, or as person alice where session is "", and
// answers what the route answered, fatal unless it is 201.
func recordAs(t *testing.T, handler http.Handler, issue, session string, body map[string]any) roleAnswer {
	t.Helper()
	var response *httptest.ResponseRecorder
	if session == "" {
		response = dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/results", body, "alice")
	} else {
		body["actor"] = map[string]any{"kind": "session", "id": session}
		response = bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/results", body)
	}
	if response.Code != http.StatusCreated {
		t.Fatalf("record a result on %s as %q: status=%d body=%s", issue, session, response.Code, response.Body.String())
	}
	return decodeBody[roleAnswer](t, response)
}

// unnamed is a result body that names no role.
func unnamed(check, verdict, commit string) map[string]any {
	return map[string]any{"check": check, "verdict": verdict, "commit": commit}
}

// named is a result body that names role.
func named(check, verdict, commit, role string) map[string]any {
	body := unnamed(check, verdict, commit)
	body["role"] = role
	return body
}

// noRoleLine is the part of the pull-request body line of a result whose session holds no role of
// its recording issue. The line is public, so it names neither the session nor the issue.
func noRoleLine(string) string {
	return "recorded by a session that holds no role of the issue."
}

// A session that holds a role records with no role named, and the stored role is its own. It
// names that role, and the result is stored the same, with a notice that a later release refuses
// a named role and no role_not_taken.
func TestTheStoredRoleIsTheRoleTheSessionHolds(t *testing.T) {
	t.Parallel()
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := roleIssue(t, handler, []issueRoleHolder{{Role: "tester", Session: "ses-tester"}})

	plain := recordAs(t, handler, issue, "ses-tester", unnamed("k1", "pass", headCommit))
	if plain.Result.Role != "tester" || plain.Result.Session != "ses-tester" {
		t.Fatalf("the tester's session named no role: stored role %q as %q, want tester as ses-tester", plain.Result.Role, plain.Result.Session)
	}
	if !strings.Contains(plain.Line, "recorded by tester.") || plain.RoleNotice != "" || plain.RoleNotTaken != "" {
		t.Fatalf("the answer to a call that named no role = %+v, want the line recorded by tester and no notice", plain)
	}

	held := recordAs(t, handler, issue, "ses-tester", named("k1", "pass", headCommit, "tester"))
	if held.Result.Role != "tester" {
		t.Fatalf("the tester's session named tester: stored role %q, want tester", held.Result.Role)
	}
	if want := "This call named the role tester. Dispatch records the role that the session holds, and that is tester. A later release refuses a result that names a role."; held.RoleNotice != want || held.RoleNotTaken != "" {
		t.Fatalf("the answer to a call that named the held role: role_notice %q, role_not_taken %q, want %q and none", held.RoleNotice, held.RoleNotTaken, want)
	}
}

// A session that holds no role, one that holds another role, and a person each name tester, and
// none is stored as tester: each gets the role it holds, or none. The answer names the role it
// did not take, and Dispatch counts and logs each write that named a role, by whether it was held.
// This is not the refusal of a named role, which a later change of dispatch://INFRA-3994 makes.
func TestANamedRoleThatTheSessionDoesNotHoldIsNotTaken(t *testing.T) {
	logs := &bytes.Buffer{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, nil))
	registry := metrics.New()
	handler, _, _ := newTestServer(t, testServerOptions{metrics: registry})
	issue := roleIssue(t, handler, []issueRoleHolder{
		{Role: "implementer", Session: "ses-impl"},
		{Role: "tester", Session: "ses-tester"},
	})

	for _, recorder := range []struct {
		session, held, notice string
	}{
		{session: "ses-none", held: "", notice: "no role"},
		{session: "ses-impl", held: "implementer", notice: "implementer"},
		{session: "", held: "", notice: "no role"},
	} {
		answer := recordAs(t, handler, issue, recorder.session, named("k1", "pass", headCommit, "tester"))
		if answer.Result.Role != recorder.held {
			t.Errorf("caller %q, which holds role %q of %s, named tester: stored role %q, want %q",
				recorder.session, recorder.held, issue, answer.Result.Role, recorder.held)
		}
		if answer.RoleNotTaken != "tester" {
			t.Errorf("caller %q: role_not_taken %q, want tester", recorder.session, answer.RoleNotTaken)
		}
		want := "Dispatch did not take the role tester that this call named. It recorded the role that the session holds on " +
			issue + ": " + recorder.notice + ". A later release refuses a result that names a role."
		if answer.RoleNotice != want {
			t.Errorf("caller %q: role_notice %q, want %q", recorder.session, answer.RoleNotice, want)
		}
	}
	recordAs(t, handler, issue, "ses-tester", named("k1", "pass", headCommit, "tester"))
	recordAs(t, handler, issue, "ses-tester", unnamed("k1", "pass", headCommit))

	scraped := scrape(t, registry)
	for _, want := range []string{
		resultRolesNamedMetric + `{held="no"} 3`,
		resultRolesNamedMetric + `{held="yes"} 1`,
	} {
		if !strings.Contains(scraped, want) {
			t.Errorf("the metrics hold no line %q: %s", want, scraped)
		}
	}
	var lines []string
	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, resultRoleNamedMessage) {
			lines = append(lines, line)
		}
	}
	if len(lines) != 4 {
		t.Fatalf("%d lines logged a named role, want 4, one for each call that named one: %s", len(lines), logs.String())
	}
	for index, want := range []string{"session=ses-none held=no", "session=ses-impl held=no", "held=no", "session=ses-tester held=yes"} {
		if !strings.Contains(lines[index], "issue="+issue) || !strings.Contains(lines[index], want) {
			t.Errorf("log line %d = %q, want it to name issue %s and %q", index+1, lines[index], issue, want)
		}
	}
}

// A session that holds no role records its result with no role: the stored role is the empty
// string, the answer carries the stored row's role and session (which the command prints), the
// pull-request body line says the session holds no role of the issue, and the read says that a
// pass of a session with no role stands, in no_role and not as a role named "". A person's
// result is stored with no role too.
func TestASessionWithNoRoleRecordsItsResultWithNoRole(t *testing.T) {
	t.Parallel()
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := roleIssue(t, handler, []issueRoleHolder{})

	answer := recordAs(t, handler, issue, "ses-none", unnamed("k1", "pass", headCommit))
	if answer.Result.Role != "" || answer.Result.Session != "ses-none" {
		t.Fatalf("a session with no role: stored role %q as %q, want no role as ses-none", answer.Result.Role, answer.Result.Session)
	}
	if !strings.Contains(answer.Line, noRoleLine(issue)) {
		t.Fatalf("the line = %q, want it to say %q", answer.Line, noRoleLine(issue))
	}
	if answer.RoleNotice != "" || answer.RoleNotTaken != "" {
		t.Fatalf("a call that named no role got role_notice %q, role_not_taken %q, want neither", answer.RoleNotice, answer.RoleNotTaken)
	}
	if person := recordAs(t, handler, issue, "", unnamed("k1", "pass", headCommit)); person.Result.Role != "" {
		t.Fatalf("a person's result: stored role %q, want no role", person.Result.Role)
	}

	stands := checkOf(t, readResultsAt(t, handler, issue, headCommit), "k1").PassStands
	if stands.Answer != model.PassStandsYes || !stands.NoRole || len(stands.Roles) != 0 {
		t.Fatalf("k1 at %s reads %+v, want yes from a session with no role, and no role named", headCommit, stands)
	}
}

// The dry run prints the line the record would answer, so it names the role the session holds,
// not the one the body names.
func TestTheDryRunPrintsTheRoleTheSessionHolds(t *testing.T) {
	t.Parallel()
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := roleIssue(t, handler, []issueRoleHolder{{Role: "tester", Session: "ses-tester"}})
	for _, each := range []struct{ session, want string }{
		{session: "ses-tester", want: "recorded by tester."},
		{session: "ses-none", want: noRoleLine(issue)},
	} {
		body := named("k1", "pass", headCommit, "implementer")
		body["actor"] = map[string]any{"kind": "session", "id": each.session}
		response := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/results/dry-run", body)
		if response.Code != http.StatusOK {
			t.Fatalf("the dry run as %s: status=%d body=%s", each.session, response.Code, response.Body.String())
		}
		if line := decodeBody[checkResultDryRun](t, response).Line; !strings.Contains(line, each.want) {
			t.Fatalf("the dry run as %s that named implementer printed %q, want %q", each.session, line, each.want)
		}
	}
}

// The role comes from the recording issue, not the check's issue: a child's implementer records
// its parent's check on the child as implementer, and the parent's tester records on the child,
// where it holds no role, with no role.
func TestAChildsSessionRecordsAParentsCheckUnderItsRoleOnTheChild(t *testing.T) {
	t.Parallel()
	handler, _, _ := newTestServer(t, testServerOptions{})
	parent := roleIssue(t, handler, []issueRoleHolder{{Role: "tester", Session: "ses-parent"}})
	child := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "ACME", "title": "Child", "parent": parent, "force": true}, "alice"))
	publishRoleHolders(t, handler, child.Key, []issueRoleHolder{{Role: "implementer", Session: "ses-child"}})

	own := recordAs(t, handler, child.Key, "ses-child", unnamed("k1", "pass", headCommit))
	if own.Result.CheckIssueKey != parent || own.Result.Role != "implementer" {
		t.Fatalf("the child's implementer recorded the parent's k1 on %s: check issue %s, role %q, want %s and implementer",
			child.Key, own.Result.CheckIssueKey, own.Result.Role, parent)
	}
	other := recordAs(t, handler, child.Key, "ses-parent", unnamed("k1", "pass", headCommit))
	if other.Result.Role != "" {
		t.Fatalf("the parent's tester recorded on the child %s, where it holds no role: stored role %q, want none", child.Key, other.Result.Role)
	}
}

// A relaunch gives a role a new session: the daemon's next publication names the new one, the
// old session holds no role from then, and the role it names is not taken.
func TestARelaunchedRoleSessionTakesTheRoleFromTheOldSession(t *testing.T) {
	t.Parallel()
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := roleIssue(t, handler, []issueRoleHolder{{Role: "tester", Session: "ses-1"}})
	if first := recordAs(t, handler, issue, "ses-1", unnamed("k1", "pass", headCommit)); first.Result.Role != "tester" {
		t.Fatalf("ses-1 holds tester: stored role %q", first.Result.Role)
	}
	publishRoleHolders(t, handler, issue, []issueRoleHolder{{Role: "tester", Session: "ses-2"}})
	if old := recordAs(t, handler, issue, "ses-1", unnamed("k1", "pass", headCommit)); old.Result.Role != "" {
		t.Fatalf("ses-1 after the relaunch: stored role %q, want none", old.Result.Role)
	}
	if old := recordAs(t, handler, issue, "ses-1", named("k1", "pass", headCommit, "tester")); old.Result.Role != "" || old.RoleNotTaken != "tester" {
		t.Fatalf("ses-1 named tester after the relaunch: stored role %q, role_not_taken %q, want none and tester", old.Result.Role, old.RoleNotTaken)
	}
	if fresh := recordAs(t, handler, issue, "ses-2", unnamed("k1", "pass", headCommit)); fresh.Result.Role != "tester" {
		t.Fatalf("ses-2 holds tester: stored role %q", fresh.Result.Role)
	}
}

// A role ends with its claim: the daemon publishes the set without the session, and from then the
// session records with no role.
func TestAnEndedClaimsSessionRecordsWithNoRole(t *testing.T) {
	t.Parallel()
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := roleIssue(t, handler, []issueRoleHolder{{Role: "tester", Session: "ses-t"}})
	if before := recordAs(t, handler, issue, "ses-t", unnamed("k1", "pass", headCommit)); before.Result.Role != "tester" {
		t.Fatalf("ses-t holds tester: stored role %q", before.Result.Role)
	}
	publishRoleHolders(t, handler, issue, []issueRoleHolder{})
	after := recordAs(t, handler, issue, "ses-t", unnamed("k1", "pass", headCommit))
	if after.Result.Role != "" || !strings.Contains(after.Line, noRoleLine(issue)) {
		t.Fatalf("ses-t after its claim ended: stored role %q, line %q, want no role", after.Result.Role, after.Line)
	}
}

// The gate's answer names a pass of a session with no role in no_role, never as a role named "",
// at one basis and at each head: a head whose only pass has no role stands, and the whole answer
// says no_role only where each head holds such a pass.
func TestAPassOfASessionWithNoRoleStandsAndNamesNoRole(t *testing.T) {
	t.Parallel()
	const first, second = "f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1", "f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2"
	moved := false
	pass := func(id, role, commit string) model.CheckResult {
		return model.CheckResult{ID: id, Verdict: "pass", CommitSHA: commit, Role: role,
			CommitStands: &model.ResultCommitStands{Answer: model.CommitAtBasis}, CheckWordsMoved: &moved,
			CreatedAt: time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)}
	}
	seventy, seventyOne := 70, 71
	heads := []headSubject{
		{basis: model.CheckBasis{Kind: model.CheckBasisPullRequest, Commit: first, PullRequest: &seventy}, repo: "acme/widgets"},
		{basis: model.CheckBasis{Kind: model.CheckBasisPullRequest, Commit: second, PullRequest: &seventyOne}, repo: "acme/widgets"},
	}
	compared := comparison{bases: []model.CheckBasis{heads[0].basis, heads[1].basis}, subjects: heads}
	for _, each := range []struct {
		name    string
		results []model.CheckResult
		roles   []string
		noRole  bool
	}{
		{name: "no role at both heads", results: []model.CheckResult{pass("a", "", first), pass("b", "", second)}, noRole: true},
		{name: "tester and no role at both heads", results: []model.CheckResult{pass("a", "", first), pass("b", "tester", first), pass("c", "tester", second), pass("d", "", second)}, roles: []string{"tester"}, noRole: true},
		{name: "no role at one head only", results: []model.CheckResult{pass("a", "", first), pass("b", "tester", second)}},
	} {
		stands := passStandsOf(model.CheckBlock{BlockID: "k1", When: docs.CheckWhenBeforeMerge, Results: each.results}, compared)
		if stands.Answer != model.PassStandsYes || !slices.Equal(stands.Roles, each.roles) || stands.NoRole != each.noRole {
			t.Errorf("%s: %+v, want yes with roles %v and no_role %v", each.name, stands, each.roles, each.noRole)
		}
		for _, subject := range stands.Subjects {
			if slices.Contains(subject.Roles, "") {
				t.Errorf("%s: subject %s names the role \"\": %+v", each.name, subject.Basis.Commit, subject)
			}
		}
	}
	at := comparison{bases: []model.CheckBasis{{Kind: model.CheckBasisNamed, Commit: first}}}
	if stands := passStandsOf(model.CheckBlock{BlockID: "k1", When: docs.CheckWhenBeforeMerge, Results: []model.CheckResult{pass("a", "", first)}}, at); stands.Answer != model.PassStandsYes || !stands.NoRole || len(stands.Roles) != 0 {
		t.Errorf("at one basis: %+v, want yes with no_role and no roles", stands)
	}
}

// The test helper holdRole gives one role without taking the issue's other roles away, although a
// publication replaces the issue's whole set: the implementer's session records after the tester
// was given its role, and its result is still the implementer's. Control: a holdRole that publishes
// the one pair alone leaves the implementer's second result with no role.
func TestHoldRoleKeepsTheOtherRolesOfTheIssue(t *testing.T) {
	t.Parallel()
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := roleIssue(t, handler, []issueRoleHolder{})
	first := decodeBody[roleAnswer](t, recordResult(t, handler, issue, resultBody("k1", "pass", headCommit, nil)))
	tester := decodeBody[roleAnswer](t, recordResult(t, handler, issue, resultBody("k1", "pass", headCommit, map[string]any{"role": "tester"})))
	again := decodeBody[roleAnswer](t, recordResult(t, handler, issue, resultBody("k1", "pass", newHeadCommit, nil)))
	for _, each := range []struct {
		name, got, want string
	}{
		{"the implementer's first result", first.Result.Role, "implementer"},
		{"the tester's result", tester.Result.Role, "tester"},
		{"the implementer's result after the tester was given its role", again.Result.Role, "implementer"},
	} {
		if each.got != each.want {
			t.Errorf("%s: stored role %q, want %q", each.name, each.got, each.want)
		}
	}
	held := sortedPairs(readRoleHolders(t, handler, issue).Holders)
	if want := []issueRoleHolder{{Role: "implementer", Session: "ses-implementer"}, {Role: "tester", Session: "ses-tester"}}; !slices.Equal(held, want) {
		t.Fatalf("the issue's role holders are %v, want %v", held, want)
	}
}
