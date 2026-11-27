package api

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The people, agents and deployment rows of verification v16's seed (private_seed_test.go): which
// of them each caller reads once PRIV is private. Each object is written through the route that
// writes it, as the caller who would write it; the delivery settings, the stored pull requests and
// the CI reconcile passes are written as the code that stores them writes them, because no route
// of this build writes those.

// The sessions the stand-in listener lists, by what each does in the seed: session-pub comments on
// PUB's issue, session-priv on PRIV's, session-both opens an ask on each, and session-idle does
// nothing, so it has no history in any project.
const (
	sessionOfPublic  = "session-pub"
	sessionOfPrivate = "session-priv"
	sessionOfBoth    = "session-both"
	sessionIdle      = "session-idle"
)

// privateRosterListed is privateRoster with the listener listing the projects of session-pub's
// and session-priv's credentials, as a listener that knows them does: each is carol's, its broker
// token scoped to one project (asSession). A session's conversations and files are its content
// (access.SessionContentClause), and carol sees PRIV, so with no list a caller that does not see
// PRIV reads no conversation of either; the list bounds what session-pub's credential reads to PUB.
// A read stores the list where the content rule reads it only once Dispatch has read the
// listener's sessions, as a list of the agents does.
const privateRosterListed = `[
	{"session_id":"session-both","title":"Both","capabilities":["btw","steer"],"last_seen":400},
	{"session_id":"session-priv","title":"Private","capabilities":["btw","steer"],"projects":["PRIV"],"last_seen":300},
	{"session_id":"session-pub","title":"Public","capabilities":["btw","steer"],"projects":["PUB"],"last_seen":200},
	{"session_id":"session-idle","title":"Idle","capabilities":["btw","steer"],"projects":[],"last_seen":100}
]`

// seedSessionWork has the roster's sessions act in the seed's projects, each under a credential
// that proves the session (asSession): what a proven session writes in a project is what makes it
// one of the project's agents (session_projects).
func seedSessionWork(t *testing.T, seed privateSeed) {
	t.Helper()
	act := func(project, route, session string, body map[string]any) {
		t.Helper()
		seed.asSession(t, session, project, http.MethodPost, "/api/v1/issues/"+seed.issue[project]+"/"+route, body, http.StatusCreated)
	}
	act(seedPublic, "comments", sessionOfPublic, map[string]any{"body": "Done on PUB."})
	act(seedPrivate, "comments", sessionOfPrivate, map[string]any{"body": "Done on PRIV."})
	act(seedPublic, "asks", sessionOfBoth, map[string]any{"question": "Ship PUB?"})
	act(seedPrivate, "asks", sessionOfBoth, map[string]any{"question": "Ship PRIV?"})
}

// TestCIRepairPassesGoToAnOpenViewerOnly (check c-ci-passes-open): GET /api/v1/ci-reconcile/passes
// reads the whole deployment. A pass covers repositories of many projects and names no issue, so it
// has no project to hold a viewer to: an open viewer reads every pass, and every other viewer is
// refused the route. alice, carol and carol's token that lists PRIV, a full member's and so open,
// read the same three passes, the one whose cursor rests in a repository the mapping of the old
// repository rule names for PRIV included; bob and bob's token that lists PRIV are 403
// DEPLOYMENT_READ_REFUSED.
//
// Control: answer the passes to every viewer (the route's class without deployment); bob then
// reads a pass, and the test fails naming it.
func TestCIRepairPassesGoToAnOpenViewerOnly(t *testing.T) {
	seed := seedPrivateProject(t)
	if _, err := seed.database.Pool.Exec(t.Context(), `
		insert into repo_projects (repo, project, created_by) values ('acme/priv', 'PRIV', '{"kind":"user","id":"operator"}')
	`); err != nil {
		t.Fatalf("map acme/priv to PRIV: %v", err)
	}
	instance := seedServerProcess(t, seed.database, 0)
	for index, repo := range []string{"acme/pub", "acme/priv", "acme/other"} {
		id := endedCIPass(t, seed.database, instance, time.Duration(index+1)*time.Hour, "complete")
		if _, err := seed.database.Pool.Exec(t.Context(), `update ci_reconcile_passes set cursor_repository = $2 where id = $1`, id, repo); err != nil {
			t.Fatalf("leave pass %d in %s: %v", id, repo, err)
		}
	}

	const route = "/api/v1/ci-reconcile/passes"
	every := seed.mustRequest(t, callerCarol, http.MethodGet, route, nil, http.StatusOK)
	got := []string{}
	for _, pass := range decodeBody[[]ciPassView](t, every) {
		if pass.Cursor != nil {
			got = append(got, pass.Cursor.Repository)
		}
	}
	if want := []string{"acme/pub", "acme/priv", "acme/other"}; !slices.Equal(got, want) {
		t.Fatalf("carol reads passes left in %v, want %v, newest first", got, want)
	}
	for _, open := range []privateCaller{callerAlice, callerCarolPrivToken} {
		if read := seed.request(t, open, http.MethodGet, route, nil); read.Code != http.StatusOK || read.Body.String() != every.Body.String() {
			t.Errorf("%s, an open viewer, reads GET %s = %d %s, want carol's answer: %s", open, route, read.Code, read.Body.String(), every.Body.String())
		}
	}
	for _, caller := range []privateCaller{callerBob, callerBobPrivToken} {
		refused := seed.request(t, caller, http.MethodGet, route, nil)
		if refused.Code != http.StatusForbidden || !strings.Contains(refused.Body.String(), `"code":"DEPLOYMENT_READ_REFUSED"`) {
			t.Errorf("%s reads GET %s = %d %s, want 403 DEPLOYMENT_READ_REFUSED", caller, route, refused.Code, refused.Body.String())
		}
	}
}

// TestACIPassCursorInAHiddenRepositoryReadsAsTheStartOfTheOrder: PRIV's issue names acme/priv,
// so PRIV owns it (refs.RepositoryClause), and PUB's names acme/pub. One CI reconcile pass left its
// cursor in each. carol and carol's token that lists PRIV, which see PRIV, read both cursors as the
// passes left them. alice, alice's token that lists PRIV, carol's token that lists no project and
// the shared token, open viewers that do not see PRIV, read the acme/priv pass's cursor as the zero
// cursor - no repository, no page, no pull request, the start of the order - and the acme/pub
// pass's as it was left, so neither the name of PRIV's repository nor the numbers of its pull
// requests reach them, as the App-installation route answers that repository as not installed.
//
// Control: the route answering every cursor as stored (leaveOutHiddenCursors returning at once):
// alice reads acme/priv in a cursor, and the test fails naming her.
func TestACIPassCursorInAHiddenRepositoryReadsAsTheStartOfTheOrder(t *testing.T) {
	seed := seedPrivateProject(t)
	instance := seedServerProcess(t, seed.database, 0)
	passes := map[string]int64{}
	for index, owned := range []struct{ project, repository string }{{seedPrivate, "acme/priv"}, {seedPublic, "acme/pub"}} {
		if _, err := seed.database.Pool.Exec(t.Context(), `update issues set repo = $2 where key = $1`,
			seed.issue[owned.project], owned.repository); err != nil {
			t.Fatalf("name %s on %s's issue: %v", owned.repository, owned.project, err)
		}
		id := endedCIPass(t, seed.database, instance, time.Duration(index+1)*time.Hour, "complete")
		if _, err := seed.database.Pool.Exec(t.Context(), `update ci_reconcile_passes set cursor_repository = $2 where id = $1`,
			id, owned.repository); err != nil {
			t.Fatalf("leave pass %d in %s: %v", id, owned.repository, err)
		}
		passes[owned.repository] = id
	}

	const route = "/api/v1/ci-reconcile/passes"
	const left = "21 23 true true"
	for _, row := range []struct {
		callers []privateCaller
		priv    string
	}{
		{[]privateCaller{callerCarol, callerCarolPrivToken}, "acme/priv " + left},
		{[]privateCaller{callerAlice, callerAlicePrivToken, callerCarolToken, callerSharedToken}, " 0 0 false false"},
	} {
		want := map[int64]string{passes["acme/priv"]: row.priv, passes["acme/pub"]: "acme/pub " + left}
		for _, caller := range row.callers {
			response := seed.mustRequest(t, caller, http.MethodGet, route, nil, http.StatusOK)
			got := map[int64]string{}
			for _, pass := range decodeBody[[]ciPassView](t, response) {
				if cursor := pass.Cursor; cursor != nil {
					got[pass.ID] = fmt.Sprintf("%s %d %d %v %v", cursor.Repository, cursor.From, cursor.Number, cursor.Rest, cursor.Counted)
				}
			}
			if !maps.Equal(got, want) {
				t.Errorf("%s reads the passes' cursors %v, want %v", caller, got, want)
			}
			if hides := !strings.HasPrefix(row.priv, "acme/priv"); hides && strings.Contains(response.Body.String(), "acme/priv") {
				t.Errorf("%s reads GET %s naming acme/priv, which it may not see: %s", caller, route, response.Body.String())
			}
		}
	}
}

// TestADeploymentReadCountsOnlyTheProjectsTheCallerSees: GET /api/v1/priorities, a read of the whole
// deployment that an open viewer still makes, counts only the work of the projects the caller sees:
// PRIV's work is not counted for alice, the shared token or carol's token that lists no project. A
// restricted member makes no such read (DEPLOYMENT_READ_REFUSED, readClasses), so only open
// viewers are asked. The delivery reads have their own rule and test
// (TestADeliveryReadShowsOnlyThePullRequestsItsViewerSees), and so do the CI reconcile passes
// (TestCIRepairPassesGoToAnOpenViewerOnly).
func TestADeploymentReadCountsOnlyTheProjectsTheCallerSees(t *testing.T) {
	seed := seedPrivateProject(t)
	// One priority each project's work advances, and each project's seed issue in flight under no
	// priority.
	seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/priorities", map[string]any{"id": "ship-it", "title": "Ship it"}, http.StatusCreated)
	for _, project := range []string{seedPublic, seedPrivate} {
		seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": project, "title": "Advance on " + project, "advances": []string{"ship-it"}, "force": true,
		}, http.StatusCreated)
		seed.mustRequest(t, callerCarol, http.MethodPatch, "/api/v1/issues/"+seed.issue[project],
			map[string]any{"status": "in_progress"}, http.StatusOK)
	}

	for _, test := range []struct {
		caller privateCaller
		// sees is whether the caller sees PRIV.
		sees bool
	}{
		{callerAlice, false},
		{callerCarol, true},
		{callerCarolToken, false},
		{callerSharedToken, false},
	} {
		t.Run(string(test.caller), func(t *testing.T) {
			wantByProject := []string{seedPublic}
			wantAdvancing := 1
			if test.sees {
				wantByProject = []string{seedPrivate, seedPublic}
				wantAdvancing = 2
			}
			priorities := decodeBody[model.Priorities](t, seed.mustRequest(t, test.caller, http.MethodGet, "/api/v1/priorities", nil, http.StatusOK))
			advancing := -1
			for _, row := range priorities.Priorities {
				if row.ID == "ship-it" {
					advancing = 0
					for _, open := range row.Work.ByStatus {
						advancing += open
					}
				}
			}
			if advancing != wantAdvancing {
				t.Errorf("%s: the priority ship-it counts %d open issues, want %d", test.caller, advancing, wantAdvancing)
			}
			if got := slices.Sorted(maps.Keys(priorities.WorkUnderNoPriority.ByProject)); !slices.Equal(got, wantByProject) {
				t.Errorf("%s: the work under no priority is counted in %v, want %v", test.caller, got, wantByProject)
			}
		})
	}
}
