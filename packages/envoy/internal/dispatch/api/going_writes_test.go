// going_writes_test.go holds the writes a command-bar run makes to what they run through: every
// row of the catalogue executes through GoingCaller.Do against the server's own route table, as
// its caller (c8), refusing what its route refuses exactly as that route does, with the person's
// own mode held on the server.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// goingCookieOrHeader names the person a session cookie names, as a browser's request is named,
// and otherwise the person X-Dispatch-User names, which is how the fixtures below write.
type goingCookieOrHeader struct {
	cookie identity.CookieIdentity
	header identity.HeaderIdentity
}

func (i goingCookieOrHeader) Login(r *http.Request) (string, error) {
	if auth.HasSessionCookie(r) {
		return i.cookie.Login(r)
	}
	return i.header.Login(r)
}

// goingWritesRig is a server whose people sign in with a session cookie, beside the header the
// fixtures write with, and whose stand-in listener holds one live session, s1, titled planner,
// and records every send to it in sent (newGoingWritesRigOn reaches another listener instead).
// mux is the route table as register mounts it; network is the same behind the cross-site origin
// check routes.New puts in front of the whole server, which is what a browser reaches.
type goingWritesRig struct {
	mux      http.Handler
	network  http.Handler
	served   *server
	database *store.Store
	sessions *store.PgSessionStore
	sent     *[]map[string]any
}

// goingPlannerTitle is the stand-in listener's one session's title, and goingPlanner its id.
const (
	goingPlannerTitle = "planner"
	goingPlanner      = "s1"
)

func newGoingWritesRig(t *testing.T) goingWritesRig {
	t.Helper()
	return newGoingWritesRigServing(t, func(database *store.Store) *store.Store { return database }, 0)
}

// newGoingWritesRigServing is the rig whose server runs on the store serving makes of the test's
// database - a pool of a smaller size, say - while the fixtures and the sign-ins use the database's
// own, its documents settling settle after their last write (0 for the test servers' own).
func newGoingWritesRigServing(t *testing.T, serving func(*store.Store) *store.Store, settle time.Duration) goingWritesRig {
	t.Helper()
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	t.Cleanup(listener.Close)
	rig := newGoingWritesRigSettling(t, listener.URL, serving, settle)
	rig.sent = &sent
	return rig
}

// newGoingWritesRigOn is the rig whose server reaches the Envoy listener at envoyURL rather than
// the stand-in holding planner, and keeps no record of the sends (sent is nil).
func newGoingWritesRigOn(t *testing.T, envoyURL string, serving func(*store.Store) *store.Store) goingWritesRig {
	t.Helper()
	return newGoingWritesRigSettling(t, envoyURL, serving, 0)
}

// newGoingWritesRigSettling is newGoingWritesRigOn whose documents settle settle after their last
// write (0 for the test servers' own): an hour holds back the version a settlement writes of a
// live edit, so the edit's text stands in no version for the test's length.
func newGoingWritesRigSettling(t *testing.T, envoyURL string, serving func(*store.Store) *store.Store, settle time.Duration) goingWritesRig {
	t.Helper()
	database := storetest.Open(t)
	sessions := store.NewPgSessionStore(database.Pool)
	people := goingCookieOrHeader{
		cookie: identity.CookieIdentity{SigningKey: testSigningKey, Sessions: sessions},
		header: headerIdentity(database),
	}
	app, githubAPIBase := goingFixtureArchitectureGitHub(t)
	mux, _, _, served := newRegisteredTestServer(t, testServerOptions{store: serving(database), identity: people, envoyURL: envoyURL, app: app,
		githubAPIBase: githubAPIBase, settle: settle})
	return goingWritesRig{
		mux: mux, network: EnforceCookieOrigin(testServerURL, mux), served: served,
		database: database, sessions: sessions,
	}
}

// cookie is login's session cookie, signed in now.
func (rig goingWritesRig) cookie(t *testing.T, login string) *http.Cookie {
	t.Helper()
	generation, err := rig.sessions.EnsureSession(context.Background(), login)
	if err != nil {
		t.Fatalf("sign %s in: %v", login, err)
	}
	cookie, err := http.ParseSetCookie(auth.IssueSessionCookie(login, generation, testSigningKey, true))
	if err != nil {
		t.Fatalf("parse %s's session cookie: %v", login, err)
	}
	return cookie
}

// request is one request to handler, with headers set on it.
func goingRequestTo(t *testing.T, handler http.Handler, method, target string, body any, headers map[string]string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode the request: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, target, reader)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// pageOrigin is the header a page of this deployment's own sends.
var pageOrigin = map[string]string{"Origin": testServerURL}

// browser is one request of login's browser on this deployment's own page, through the origin
// check, as a browser's reaches the server.
func (rig goingWritesRig) browser(t *testing.T, login, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return goingRequestTo(t, rig.network, method, target, body, pageOrigin, rig.cookie(t, login))
}

// fixture is one write of the fixtures, as login through the header, which must succeed.
func (rig goingWritesRig) fixture(t *testing.T, login, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	response := dispatchRequest(t, rig.mux, method, target, body, login)
	if response.Code >= http.StatusMultipleChoices {
		t.Fatalf("fixture %s %s: status=%d body=%s", method, target, response.Code, response.Body.String())
	}
	return response
}

// issue makes one more issue of DEMO, which the first fixture issue's project already holds.
func (rig goingWritesRig) issue(t *testing.T, title string) string {
	t.Helper()
	return decodeBody[model.Issue](t, rig.fixture(t, "alice", http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "DEMO", "title": title, "force": true,
	})).Key
}

// ask opens one question ask on issue.
func (rig goingWritesRig) ask(t *testing.T, issue, question string) string {
	t.Helper()
	return decodeBody[model.Ask](t, rig.fixture(t, "alice", http.MethodPost, "/api/v1/issues/"+issue+"/asks", map[string]any{
		"question": question,
	})).ID
}

// TestGoingSettingsAreThePersonsOwnAndReadAskUntilSet: the mode of the bar is held for the
// person, read ask until they set it, read back as they set it, refused when it is neither mode,
// and nobody else's.
func TestGoingSettingsAreThePersonsOwnAndReadAskUntilSet(t *testing.T) {
	rig := newGoingWritesRig(t)
	read := func(login string) goingSettings {
		t.Helper()
		response := rig.browser(t, login, http.MethodGet, "/api/v1/me/going/settings", nil)
		if response.Code != http.StatusOK {
			t.Fatalf("GET settings as %s answered %d %s", login, response.Code, response.Body.String())
		}
		return decodeBody[goingSettings](t, response)
	}
	if settings := read("alice"); settings.Mode != goingModeAsk {
		t.Fatalf("a new person's mode is %q, want ask", settings.Mode)
	}
	if response := rig.browser(t, "alice", http.MethodPut, "/api/v1/me/going/settings", map[string]any{"mode": "auto"}); response.Code != http.StatusBadRequest {
		t.Fatalf("a mode of auto answered %d, want 400", response.Code)
	}
	if response := rig.browser(t, "alice", http.MethodPut, "/api/v1/me/going/settings", map[string]any{"mode": goingModeAutoReview}); response.Code != http.StatusOK {
		t.Fatalf("PUT auto_review answered %d %s", response.Code, response.Body.String())
	}
	if settings := read("alice"); settings.Mode != goingModeAutoReview {
		t.Fatalf("alice's mode reads %q after she set auto_review", settings.Mode)
	}
	if settings := read("bob"); settings.Mode != goingModeAsk {
		t.Fatalf("bob's mode reads %q after alice set hers, want his own ask", settings.Mode)
	}
}

// issueRead is the issue as its own route reads it.
func (rig goingWritesRig) issueRead(t *testing.T, key string) model.Issue {
	t.Helper()
	return decodeBody[model.Issue](t, rig.fixture(t, "alice", http.MethodGet, "/api/v1/issues/"+key, nil))
}

// goingRecordingDoer is a Doer that records each request it sends, by method and path.
type goingRecordingDoer struct {
	doer GoingDoer
	sent []string
}

func (d *goingRecordingDoer) Do(request *http.Request) (*http.Response, error) {
	d.sent = append(d.sent, request.Method+" "+request.URL.Path)
	return d.doer.Do(request)
}

// goingPathMatches reports whether path is a path of the route pattern.
func goingPathMatches(pattern, path string) bool {
	want, got := strings.Split(pattern, "/"), strings.Split(path, "/")
	if len(want) != len(got) {
		return false
	}
	for index := range want {
		if strings.HasPrefix(want[index], "{") {
			if got[index] == "" {
				return false
			}
			continue
		}
		if want[index] != got[index] {
			return false
		}
	}
	return true
}

// goingTestRun is the run the command bar's act names on a call a test makes through the route
// table in-process outside any run's write.
const goingTestRun = "0190c7d2-1111-7222-8333-444444444444"

// goingCallerOf is the caller a write is performed as, resolved as a route of the bar resolves its
// own from a request standing for it - a person's browser on this deployment's page, or a
// session's bearer - with the command bar's act beside it.
func goingCallerOf(t *testing.T, rig goingWritesRig, bearer bool) resolvedCaller {
	t.Helper()
	caller := httptest.NewRequest(http.MethodPost, "/api/v1/going/runs/caller/plan/approve", nil)
	if bearer {
		caller.Header.Set("Authorization", "Bearer agent-token")
	} else {
		caller.Header.Set("Origin", testServerURL)
		caller.AddCookie(rig.cookie(t, "alice"))
	}
	actor, human, err := rig.served.optionalActor(caller)
	if err != nil {
		t.Fatalf("resolve the caller: %v", err)
	}
	return goingBarCaller(actor, human, goingTestRun)
}

// goingRowCase is the call one row of the catalogue is executed with: the arguments the fake
// answer writes, the title of the session the caller names where the row addresses one, whether
// the row's route takes a session's bearer alone, and the issue a send to an agent session is
// posted on where its row is the one that posts it on an issue (GoingCaller.sendIssue).
type goingRowCase struct {
	arguments map[string]any
	session   string
	bearer    bool
	sendIssue string
}

// goingRowFixtures are the items the rows' calls name, made before any row runs.
type goingRowFixtures struct {
	issue, released, claimable                  string
	notesSlug, notesID, guideSlug, planSlug     string
	planVersion                                 int
	editAsk, resolveAsk, unfollowAsk, followAsk string
	comment, result, direct                     string
}

// newGoingRowFixtures makes the items the rows' calls name, in the project DEMO, which it makes
// where the rig has not made it already (an agent rig has).
func newGoingRowFixtures(t *testing.T, rig goingWritesRig) goingRowFixtures {
	t.Helper()
	var f goingRowFixtures
	var demo bool
	if err := rig.database.Pool.QueryRow(context.Background(), `select exists (select 1 from projects where key = 'DEMO')`).Scan(&demo); err != nil {
		t.Fatalf("read whether DEMO is made: %v", err)
	}
	if demo {
		f.issue = decodeBody[model.Issue](t, rig.fixture(t, "alice", http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "DEMO", "title": "The first issue", "spec": "The spec says hello to everyone.\n", "force": true,
		})).Key
	} else {
		f.issue = createInteractionIssue(t, rig.mux, "DEMO", "The first issue", "The spec says hello to everyone.\n").Key
	}
	// One architecture source the project's sync imports. The rig wires a GitHub stand-in where
	// acme/arch is readable (goingFixtureArchitectureGitHub), so both the every-source and the
	// one-source sync answer 200.
	if _, err := rig.database.Pool.Exec(context.Background(), `
		insert into architecture_sources (project_key, id, repo, branch, installation_id, created_by)
		values ('DEMO', 1, 'acme/arch', 'main', 1, '{"kind":"system","id":"test"}')`); err != nil {
		t.Fatalf("add an architecture source: %v", err)
	}
	f.released = rig.issue(t, "An issue alice holds")
	f.claimable = rig.issue(t, "An issue nobody holds")
	owner := "/api/v1/issues/" + f.issue
	notes := uploadDocument(t, rig.mux, owner, "notes.md", "# Notes\n\nThe first line.\n")
	f.notesSlug, f.notesID = notes.Artifact.Slug, notes.Artifact.ID
	f.guideSlug = uploadDocument(t, rig.mux, "/api/v1/projects/DEMO", "guide.md", "# Guide\n\nRead this first.\n").Artifact.Slug
	plan := uploadDocument(t, rig.mux, owner, "plan.md", "# Plan\n\nDo the work.\n")
	f.planSlug, f.planVersion = plan.Artifact.Slug, plan.Version.Number
	uploadDocument(t, rig.mux, owner, "verification.md", twoChecks)
	f.editAsk = rig.ask(t, f.issue, "Ship it on Friday?")
	f.resolveAsk = rig.ask(t, f.issue, "Is the old flag still read?")
	f.unfollowAsk = rig.ask(t, f.issue, "Which region first?")
	f.followAsk = rig.ask(t, f.issue, "Who reviews the change?")
	rig.fixture(t, "alice", http.MethodPut, "/api/v1/asks/"+f.unfollowAsk+"/followers/"+goingPlanner, nil)
	f.comment = decodeBody[model.Comment](t, rig.fixture(t, "alice", http.MethodPost, owner+"/comments", map[string]any{"body": "A thread to resolve."})).ID
	f.result = decodeBody[recordedResult](t, rig.fixture(t, "alice", http.MethodPost, owner+"/results", resultBody("k1", "pass", headCommit, nil))).Result.ID
	rig.fixture(t, "alice", http.MethodPost, "/api/v1/issues/"+f.released+"/claim", map[string]any{})
	f.direct = decodeBody[model.Message](t, rig.fixture(t, "alice", http.MethodPost, "/api/v1/agents/"+goingPlanner+"/messages", map[string]any{
		"body": "Where is the dashboard?", "delivery": "aside",
	})).ID
	return f
}

// goingRowCases is the call each row of the catalogue is executed with, by action and row.
func goingRowCases(f goingRowFixtures) map[string]goingRowCase {
	return map[string]goingRowCase{
		"dispatch_issue POST /api/v1/issues": {arguments: map[string]any{"project": "DEMO", "title": "Made by the bar", "force": true}},
		"dispatch_issue_update PATCH /api/v1/issues/{key}": {arguments: map[string]any{
			"issue": f.issue, "status": "in_progress", "labels": []any{"bar"}, "external_links": []any{"https://example.com/pull/1"},
		}},
		"dispatch_claim DELETE /api/v1/issues/{key}/claim": {arguments: map[string]any{"issue": f.released, "release": true}},
		"dispatch_claim POST /api/v1/issues/{key}/claim":   {arguments: map[string]any{"issue": f.claimable}},
		"dispatch_ask POST /api/v1/issues/{key}/asks": {arguments: map[string]any{
			"issue": f.issue, "question": "Ship the notes as they stand?",
			"anchor": map[string]any{"artifact": f.notesSlug, "quote": "The first line."},
		}},
		"dispatch_ask POST /api/v1/artifacts/{id}/asks": {arguments: map[string]any{
			"project": "DEMO", "artifact": f.guideSlug, "question": "Is the guide right?",
		}},
		"dispatch_edit_ask PATCH /api/v1/asks/{id}":                   {arguments: map[string]any{"ask": f.editAsk, "question": "Ship it on Monday?"}},
		"dispatch_resolve_ask POST /api/v1/asks/{id}/resolve":         {arguments: map[string]any{"ask": f.resolveAsk, "kind": "resolved", "reason": "Settled elsewhere."}},
		"dispatch_resolve_comment POST /api/v1/comments/{id}/resolve": {arguments: map[string]any{"comment": f.comment}},
		"dispatch_follow DELETE /api/v1/asks/{id}/followers/{session_id}": {
			arguments: map[string]any{"ask": f.unfollowAsk, "action": "unfollow"}, session: goingPlannerTitle,
		},
		"dispatch_follow PUT /api/v1/asks/{id}/followers/{session_id}": {
			arguments: map[string]any{"ask": f.followAsk, "action": "follow"}, session: goingPlannerTitle,
		},
		"dispatch_comment POST /api/v1/issues/{key}/comments": {arguments: map[string]any{
			"issue": f.issue, "body": "Reads well.", "artifact": f.notesSlug, "quote": "The first line.",
			"mentions": []any{"role:legion-reviewer"},
		}},
		"dispatch_comment POST /api/v1/artifacts/{id}/comments": {arguments: map[string]any{
			"project": "DEMO", "artifact": f.guideSlug, "body": "Clear.", "quote": "Read this first.",
			"mentions": []any{"session:s-guide", "role:legion-reviewer"},
		}},
		"dispatch_suggest POST /api/v1/issues/{key}/comments": {arguments: map[string]any{
			"issue": f.issue, "quote": "says hello", "replace_with": "says hi",
		}},
		"dispatch_suggest POST /api/v1/artifacts/{id}/comments": {arguments: map[string]any{
			"project": "DEMO", "artifact": f.guideSlug, "quote": "Read this", "replace_with": "Read that",
		}},
		"dispatch_message POST /api/v1/issues/{key}/messages": {arguments: map[string]any{"issue": f.issue, "body": "Where are we?"}},
		"dispatch_message POST /api/v1/messages/{id}/reply": {
			arguments: map[string]any{"in_reply_to": f.direct, "body": "Working on it."}, bearer: true,
		},
		"dispatch_send_to_agent POST /api/v1/issues/{key}/messages": {arguments: map[string]any{
			"session": goingPlannerTitle, "body": "Have a look at the notes on the issue.", "delivery": "btw",
		}, sendIssue: f.issue},
		"dispatch_send_to_agent POST /api/v1/agents/{session_id}/messages": {arguments: map[string]any{
			"session": goingPlannerTitle, "body": "Have a look at the notes.", "delivery": "btw",
		}},
		"dispatch_doc_edit POST /api/v1/issues/{key}/artifacts/{slug}/edits": {arguments: map[string]any{
			"issue": f.issue, "artifact": f.notesSlug, "ops": []any{map[string]any{"op": "replace", "find": "first line", "with": "opening line"}},
		}},
		"dispatch_doc_edit POST /api/v1/projects/{key}/artifacts/{slug}/edits": {arguments: map[string]any{
			"project": "DEMO", "artifact": f.guideSlug, "ops": []any{map[string]any{"op": "replace", "find": "first", "with": "before anything"}},
		}},
		"dispatch_doc_edit POST /api/v1/artifacts/{id}/edits": {arguments: map[string]any{
			"artifact": f.notesID, "ops": []any{map[string]any{"op": "replace", "find": "Notes", "with": "Jottings"}},
		}},
		"dispatch_request_approval POST /api/v1/artifacts/{id}/approval-requests": {arguments: map[string]any{
			"issue": f.issue, "artifact": "spec", "summary": "Ready for review.",
		}},
		"dispatch_plan_approve POST /api/v1/artifacts/{id}/plan-reviews": {arguments: map[string]any{
			"issue": f.issue, "artifact": f.planSlug, "version": f.planVersion,
		}},
		"dispatch_result POST /api/v1/results/{id}/retract": {arguments: map[string]any{"retract": f.result, "reason": "Recorded at the wrong commit."}},
		"dispatch_result POST /api/v1/issues/{key}/results": {arguments: map[string]any{
			"issue": f.issue, "check": "k1", "role": "implementer", "verdict": "pass", "commit": headCommit,
		}},
		"dispatch_artifact POST /api/v1/issues/{key}/artifacts": {arguments: map[string]any{
			"issue": f.issue, "name": "extra.md", "content": "# Extra\n\nMore words.\n",
		}},
		"dispatch_artifact POST /api/v1/projects/{key}/artifacts": {arguments: map[string]any{
			"project": "DEMO", "name": "handbook.md", "content": "# Handbook\n",
		}},
		"dispatch_artifact POST /api/v1/agents/{session_id}/artifacts": {
			arguments: map[string]any{"name": "notes.txt", "content": "Plain words for the planner."}, session: goingPlannerTitle,
		},
		"dispatch_project POST /api/v1/projects":                                           {arguments: map[string]any{"key": "NEWP", "name": "A new project"}},
		"dispatch_architecture_sync POST /api/v1/projects/{key}/architecture-sources/sync": {arguments: map[string]any{"project": "DEMO"}},
		"dispatch_architecture_sync POST /api/v1/projects/{key}/architecture-sources/{owner}/{repo}/sync": {
			arguments: map[string]any{"project": "DEMO", "repo": "acme/arch"},
		},
		"dispatch_handoff_write POST /api/v1/issues/{key}/handoffs/{phase}": {
			arguments: map[string]any{"issue": f.issue, "phase": "plan", "body": `{"summary":"planned"}`}, bearer: true,
		},
	}
}

// TestEveryGoingRowExecutesThroughTheCallerAgainstTheServedTable is c8: every row of the catalogue
// BuildGoingCatalogue holds, counted against that builder, executes through GoingCaller.Do against
// the server's own route table called in-process, under the credential kind its route accepts,
// with its case's arguments translated into what the route takes; and each call reaches the row
// the catalogue names and is taken by it.
func TestEveryGoingRowExecutesThroughTheCallerAgainstTheServedTable(t *testing.T) {
	rig := newGoingWritesRig(t)
	cases := goingRowCases(newGoingRowFixtures(t, rig))
	catalogue, err := BuildGoingCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	rows := 0
	for _, action := range catalogue.Actions() {
		rows += len(action.Routes)
	}
	if len(cases) != rows {
		t.Fatalf("%d row cases for the %d rows the builder holds: every row is executed, and only those", len(cases), rows)
	}
	for _, action := range catalogue.Actions() {
		for _, route := range action.Routes {
			name := action.Action + " " + route.Method + " " + route.Pattern
			t.Run(name, func(t *testing.T) {
				row, held := cases[name]
				if !held {
					t.Fatalf("no case executes %s", name)
				}
				session := ""
				if row.bearer {
					// The session a bearer names itself as, as `dispatch` names it in actor.
					session = goingPlanner
				}
				doer := &goingRecordingDoer{doer: rig.served.goingDoer(goingCallerOf(t, rig, row.bearer), testServerURL)}
				if row.session != "" {
					// The session a person names by its title, resolved as the person
					// may read the roster.
					resolved, err := (goingReader{doer: doer}).session(context.Background(), row.session)
					if err != nil {
						t.Fatalf("resolve the session %q: %v", row.session, err)
					}
					session = resolved
				}
				caller := GoingCaller{Catalogue: catalogue, Session: session, Doer: doer, sendIssue: row.sendIssue}
				response, err := caller.Do(context.Background(), action.Action, row.arguments)
				if err != nil {
					t.Fatalf("perform %s: %v", name, err)
				}
				answer := new(bytes.Buffer)
				_, _ = answer.ReadFrom(response.Body)
				if response.StatusCode >= http.StatusMultipleChoices {
					t.Fatalf("%s refused the call: status=%d body=%s (sent %v)", name, response.StatusCode, answer.String(), doer.sent)
				}
				if targets, mentions := row.arguments["mentions"].([]any); mentions {
					// The comment the route stored mentions each target the call named.
					var made struct {
						ID string `json:"id"`
					}
					if err := json.Unmarshal(answer.Bytes(), &made); err != nil {
						t.Fatalf("read the comment %s made: %v", name, err)
					}
					var stored []string
					if err := rig.database.Pool.QueryRow(context.Background(), `
						select coalesce(array_agg(target order by target), '{}') from comment_mentions where comment_id = $1::uuid`,
						made.ID).Scan(&stored); err != nil {
						t.Fatalf("read the mentions of comment %s: %v", made.ID, err)
					}
					want := make([]string, 0, len(targets))
					for _, target := range targets {
						text, isText := target.(string)
						if !isText {
							t.Fatalf("the row %s mentions %v, want each target a session id", name, target)
						}
						want = append(want, text)
					}
					slices.Sort(want)
					if !slices.Equal(stored, want) {
						t.Fatalf("the comment %s stored mentions %v, want %v", name, stored, want)
					}
				}
				var writes []string
				for _, sent := range doer.sent {
					if !strings.HasPrefix(sent, http.MethodGet+" ") {
						writes = append(writes, sent)
					}
				}
				last := writes[len(writes)-1]
				method, path, _ := strings.Cut(last, " ")
				if method != route.Method || !goingPathMatches(route.Pattern, path) {
					t.Fatalf("the call's write was %s, want the row %s %s", last, route.Method, route.Pattern)
				}
			})
		}
	}
}

// TestEveryGoingRowRefusesAnActionTheCatalogueDoesNotHoldWithNoRequest: an action the catalogue
// does not hold is refused with its own error and nothing is sent.
func TestEveryGoingRowRefusesAnActionTheCatalogueDoesNotHoldWithNoRequest(t *testing.T) {
	catalogue, err := BuildGoingCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	doer := &goingRecordingDoer{doer: &countingDoer{}}
	_, err = GoingCaller{Catalogue: catalogue, Doer: doer}.Do(context.Background(), "dispatch_paint_widget", map[string]any{"widget": "7"})
	var unknown GoingUnknownActionError
	if !errors.As(err, &unknown) || unknown.Action != "dispatch_paint_widget" || len(doer.sent) != 0 {
		t.Fatalf("an action outside the catalogue answered %v and sent %v, want its own refusal and nothing sent", err, doer.sent)
	}
}

// TestGoingOffersNoActionToACallerWhoseTokenNeedsAProof: a caller presenting a broker token under
// DPoP is offered no action, since the bar acts with a person's own credential and a broker token
// is none; the same credential presented as a bearer is offered the catalogue.
func TestGoingOffersNoActionToACallerWhoseTokenNeedsAProof(t *testing.T) {
	catalogue, err := BuildGoingCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	caller := model.Actor{Credential: personCredential}
	presented := func(authorization string) []GoingAction {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/going/requests", nil)
		request.Header.Set("Authorization", authorization)
		return catalogue.runnableBy(goingCredentialOf(request, caller, false))
	}
	if offered := presented("DPoP a-broker-token"); len(offered) != 0 {
		t.Errorf("a caller whose token needs a DPoP proof is offered %d actions, want none", len(offered))
	}
	if offered := presented("Bearer a-personal-token"); len(offered) == 0 {
		t.Error("the same credential presented as a bearer is offered no action, want the catalogue")
	}
}

// The route a send to an agent goes through answers 201 even when the delivery failed, so the
// write's line carries the state and the error of the message's last delivery: a failed send is
// told apart from one that reached the session.
func TestGoingWriteOfASendToAgentRecordsItsLastDeliverysState(t *testing.T) {
	prepared := goingPrepared{action: "dispatch_send_to_agent", call: goingCall{path: map[string]string{"session_id": goingPlanner}}}
	answer := []byte(`{"id":"msg-1","deliveries":[
		{"attempt":1,"state":"sent","error":null},
		{"attempt":2,"state":"failed","error":"no live session s1"}
	]}`)
	after, err := (goingObserver{}).observe(t.Context(), prepared, answer, http.StatusCreated, goingObservation{})
	if err != nil {
		t.Fatalf("observe the send: %v", err)
	}
	if after.target != "msg-1" || after.delivery == nil || after.delivery.State != "failed" ||
		after.delivery.Error == nil || *after.delivery.Error != "no live session s1" {
		t.Errorf("the send's delivery after = %+v (target %q), want msg-1 with its last delivery failed and why", after.delivery, after.target)
	}
}
