package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
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

// privateRoster is the stand-in listener's sessions, newest first by last_seen, so the agents list
// answers them in this order. session-idle has acted in no project, and the listener lists it with
// a credential that lists none ("projects":[]): a row with no projects key is a listener that does
// not know the session's projects, and reaches no open viewer while a project is private.
const privateRoster = `[
	{"session_id":"session-both","title":"Both","capabilities":["btw","steer"],"last_seen":400},
	{"session_id":"session-priv","title":"Private","capabilities":["btw","steer"],"last_seen":300},
	{"session_id":"session-pub","title":"Public","capabilities":["btw","steer"],"last_seen":200},
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

// TestARestrictedMemberSeesOnlyThePeopleAndAgentsOfItsProjects: GET /api/v1/users, GET
// /api/v1/agents and GET /api/v1/people/{login}/nodes as each caller of verification v16. bob, and
// a credential that lists projects, read only the people who see a project they see (PersonClause)
// and only the sessions that acted in one (SessionClause); an open viewer reads every person, and
// every session but one that acted in a project it does not see (session-priv and session-both, to
// a viewer that does not see PRIV), as every caller did before a project could be private. A
// session's open asks count only the asks of the projects the caller sees, and a person the caller
// may not see answers exactly as a login nobody holds.
func TestARestrictedMemberSeesOnlyThePeopleAndAgentsOfItsProjects(t *testing.T) {
	listener, _ := newBroadcastListener(t, privateRoster)
	seed := seedPrivateProjectWith(t, testServerOptions{envoyURL: listener.URL})
	seedSessionWork(t, seed)
	// A comment each, so each of the two has a person node and a page to open.
	seed.mustRequest(t, callerAlice, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPublic]+"/comments",
		map[string]any{"body": "alice was here."}, http.StatusCreated)
	seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPrivate]+"/comments",
		map[string]any{"body": "carol was here."}, http.StatusCreated)

	everyone := []string{"alice", "bob", seedBob, seedCarol}
	ofPrivate := []string{seedBob, seedCarol}
	// Each session as "id asks=N", in the list's order. session-both opened one ask on each
	// project, so its count is what the caller sees of the two.
	every := func(asks int) []string {
		return []string{fmt.Sprintf("%s asks=%d", sessionOfBoth, asks), sessionOfPrivate + " asks=0", sessionOfPublic + " asks=0", sessionIdle + " asks=0"}
	}
	privateOnly := []string{sessionOfBoth + " asks=1", sessionOfPrivate + " asks=0"}
	publicOnly := []string{sessionOfPublic + " asks=0", sessionIdle + " asks=0"}
	for _, test := range []struct {
		caller privateCaller
		// users is nil for a caller GET /api/v1/users refuses: it needs the person scope.
		users  []string
		agents []string
		// pages is which of alice's and carol's pages the caller opens; the other answers as a
		// login nobody holds. A page opens on what its person wrote, and a write of a session the
		// broker proved is that session's and opens no page (personPageShownQuery, R15 of the
		// person-identity change). carol's one comment is on PRIV, so a caller that sees PRIV opens
		// her page and one that does not cannot, though it may read her in the people list; her
		// agents' writes on PUB (seedSessionWork) open nothing. alice's one comment is on PUB, so a
		// caller that sees only PRIV cannot open hers. Every caller opens its own person's page.
		pages []string
	}{
		{callerAlice, everyone, publicOnly, []string{seedAlice}},
		{callerBob, ofPrivate, privateOnly, []string{seedCarol}},
		{callerCarol, everyone, every(2), []string{seedAlice, seedCarol}},
		{callerCarolPrivToken, ofPrivate, privateOnly, []string{seedCarol}},
		{callerAlicePrivToken, []string{seedAlice}, []string{}, []string{seedAlice}},
		{callerCarolToken, everyone, publicOnly, []string{seedAlice, seedCarol}},
		{callerSharedToken, nil, publicOnly, nil},
	} {
		t.Run(string(test.caller), func(t *testing.T) {
			users := seed.request(t, test.caller, http.MethodGet, "/api/v1/users", nil)
			if test.users == nil {
				if users.Code != http.StatusForbidden {
					t.Errorf("%s: GET /api/v1/users = %d %s, want 403", test.caller, users.Code, users.Body.String())
				}
			} else {
				got := []string{}
				for _, user := range decodeBody[listUsersResponse](t, users).Users {
					got = append(got, user.Login)
				}
				if !slices.Equal(got, test.users) {
					t.Errorf("%s: GET /api/v1/users = %v, want %v", test.caller, got, test.users)
				}
			}

			agents := seed.mustRequest(t, test.caller, http.MethodGet, "/api/v1/agents", nil, http.StatusOK)
			got := []string{}
			for _, agent := range decodeBody[[]struct {
				SessionID string `json:"session_id"`
				OpenAsks  int    `json:"open_asks"`
			}](t, agents) {
				got = append(got, fmt.Sprintf("%s asks=%d", agent.SessionID, agent.OpenAsks))
			}
			if !slices.Equal(got, test.agents) {
				t.Errorf("%s: GET /api/v1/agents = %v, want %v", test.caller, got, test.agents)
			}

			if test.pages == nil {
				return
			}
			missing := seed.request(t, test.caller, http.MethodGet, "/api/v1/people/nobody@example.com/nodes", nil)
			for _, person := range []string{seedAlice, seedCarol} {
				page := seed.request(t, test.caller, http.MethodGet, "/api/v1/people/"+person+"/nodes", nil)
				if slices.Contains(test.pages, person) {
					if page.Code != http.StatusOK {
						t.Errorf("%s: %s's page = %d %s, want 200", test.caller, person, page.Code, page.Body.String())
					}
					continue
				}
				if page.Code != missing.Code || page.Body.String() != missing.Body.String() {
					t.Errorf("%s: %s's page = %d %s, want what a login nobody holds answers: %d %s",
						test.caller, person, page.Code, page.Body.String(), missing.Code, missing.Body.String())
				}
			}
		})
	}
}

// listedRoster is privateRoster's two sessions of PRIV as a listener answers them once it names
// each session's projects (K6): session-both, S3, registered with a token that lists PRIV and PUB,
// and session-priv, S2, with one that lists PRIV, both of which acted in PRIV (seedSessionWork); and
// session-listed-pub, S4, with one that lists PUB, which has acted in no project.
const listedRoster = `[
	{"session_id":"session-both","title":"Both","capabilities":["btw","steer"],"projects":["PRIV","PUB"],"last_seen":400},
	{"session_id":"session-priv","title":"Private","capabilities":["btw","steer"],"projects":["PRIV"],"last_seen":300},
	{"session_id":"session-listed-pub","title":"On PUB","capabilities":["btw","steer"],"projects":["PUB"],"last_seen":200}
]`

// TestAFilteredAgentRowNamesNoProject (K6): GET /api/v1/agents passes a roster row when
// SessionClause passes on its id, and serves none of the list the listener names for it. alice,
// who sees PUB and not PRIV, reads S4's row, whose token lists PUB, and no row of S3 or S2, each of
// which acted in PRIV and lists it; no byte of her answer has a projects key or names PRIV. bob, on
// PRIV's list alone, reads S3's and S2's rows, neither with a projects key, and not S4's, whose
// token lists PUB.
//
// Control: drop agentResponse's own Projects field (plan C's t-c-28); the embedded session's list
// is served, every row carries it, and the test fails naming each.
func TestAFilteredAgentRowNamesNoProject(t *testing.T) {
	listener, _ := newBroadcastListener(t, listedRoster)
	seed := seedPrivateProjectWith(t, testServerOptions{envoyURL: listener.URL})
	seedSessionWork(t, seed)
	for _, test := range []struct {
		caller privateCaller
		want   []string
	}{
		{callerAlice, []string{"session-listed-pub"}},
		{callerBob, []string{sessionOfBoth, sessionOfPrivate}},
	} {
		response := seed.mustRequest(t, test.caller, http.MethodGet, "/api/v1/agents", nil, http.StatusOK)
		got := []string{}
		for _, row := range decodeBody[[]map[string]json.RawMessage](t, response) {
			var session string
			_ = json.Unmarshal(row["session_id"], &session)
			got = append(got, session)
			if projects, named := row["projects"]; named {
				t.Errorf("%s: the row of %s carries projects %s, want no projects key", test.caller, session, projects)
			}
		}
		if !slices.Equal(got, test.want) {
			t.Errorf("%s: GET /api/v1/agents = %v, want %v", test.caller, got, test.want)
		}
		if test.caller == callerAlice && strings.Contains(response.Body.String(), seedPrivate) {
			t.Errorf("alice's GET /api/v1/agents names PRIV, a project she does not see: %s", response.Body.String())
		}
	}
}

// newSessionsRoster is three sessions that have written nothing yet, so none has a
// session_projects pair, as the listener lists them: session-new-priv registered with a token that
// lists PRIV, session-new-pub with one that lists PUB, and session-new-none with one that lists no
// project ("projects":[]).
const newSessionsRoster = `[
	{"session_id":"session-new-priv","title":"New on PRIV","capabilities":["btw"],"projects":["PRIV"],"last_seen":300},
	{"session_id":"session-new-pub","title":"New on PUB","capabilities":["btw"],"last_seen":200,"projects":["PUB"]},
	{"session_id":"session-new-none","title":"New, no list","capabilities":["btw"],"projects":[],"last_seen":100}
]`

// TestAnOpenViewerListsNoNewSessionOfAPrivateProjectItCannotSee (K6): a session with no history is
// judged by its listener record's projects (access.SessionClause), so GET /api/v1/agents leaves out,
// for an open viewer that does not see PRIV (alice, carol's token that lists no project, the shared
// token), session-new-priv, whose listener names PRIV alone, and lists session-new-pub, whose
// listener names PUB, and session-new-none, whose listener names no project. carol, who sees PRIV,
// lists all three; bob and carol's token that lists PRIV list session-new-priv alone; alice's token
// that lists PRIV, which sees no project, lists none. A process that asks the listener on every read
// (DISPATCH_TEST_ROSTER_UNCACHED) answers the same: it stores each answer it reads, since the
// listener record the clause reads is that stored answer.
//
// Controls: the clause before its listener-record arms (access.SessionClause passing every session
// with no pair to an open viewer), under which alice lists session-new-priv; and the uncached read
// storing nothing, under which alice, in that mode, lists no session-new-none.
func TestAnOpenViewerListsNoNewSessionOfAPrivateProjectItCannotSee(t *testing.T) {
	for _, mode := range []struct {
		name     string
		uncached bool
	}{{"the stored listener answer", false}, {"DISPATCH_TEST_ROSTER_UNCACHED", true}} {
		t.Run(mode.name, func(t *testing.T) {
			listener, _ := newBroadcastListener(t, newSessionsRoster)
			seed := seedPrivateProjectWith(t, testServerOptions{envoyURL: listener.URL, rosterUncached: mode.uncached})
			public := []string{"session-new-pub", "session-new-none"}
			for _, test := range []struct {
				caller privateCaller
				want   []string
			}{
				{callerAlice, public},
				{callerCarolToken, public},
				{callerSharedToken, public},
				{callerCarol, []string{"session-new-priv", "session-new-pub", "session-new-none"}},
				{callerBob, []string{"session-new-priv"}},
				{callerCarolPrivToken, []string{"session-new-priv"}},
				{callerAlicePrivToken, []string{}},
			} {
				response := seed.mustRequest(t, test.caller, http.MethodGet, "/api/v1/agents", nil, http.StatusOK)
				got := []string{}
				for _, row := range decodeBody[[]struct {
					SessionID string `json:"session_id"`
				}](t, response) {
					got = append(got, row.SessionID)
				}
				if !slices.Equal(got, test.want) {
					t.Errorf("%s: GET /api/v1/agents = %v, want %v", test.caller, got, test.want)
				}
			}
		})
	}
}

// TestCIRepairPassesGoToAnOpenViewerOnly (check c-ci-passes-open): GET /api/v1/ci-reconcile/passes
// reads the whole deployment. A pass covers repositories of many projects and names no issue, so it
// has no project to hold a viewer to: an open viewer reads every pass, and every other viewer is
// refused the route. alice and carol read the same three passes, the one whose cursor rests in a
// repository the mapping of the old repository rule names for PRIV included; bob and carol's token
// that lists PRIV are 403 DEPLOYMENT_READ_REFUSED.
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
	if alice := seed.request(t, callerAlice, http.MethodGet, route, nil); alice.Code != http.StatusOK || alice.Body.String() != every.Body.String() {
		t.Errorf("alice, an open viewer, reads GET %s = %d %s, want carol's answer: %s", route, alice.Code, alice.Body.String(), every.Body.String())
	}
	for _, caller := range []privateCaller{callerBob, callerCarolPrivToken} {
		refused := seed.request(t, caller, http.MethodGet, route, nil)
		if refused.Code != http.StatusForbidden || !strings.Contains(refused.Body.String(), `"code":"DEPLOYMENT_READ_REFUSED"`) {
			t.Errorf("%s reads GET %s = %d %s, want 403 DEPLOYMENT_READ_REFUSED", caller, route, refused.Code, refused.Body.String())
		}
	}
}

// TestASessionsLastActivityCountsOnlyWhatTheCallerSees: an agent row's last_activity is the newest
// event of the session among the events the caller sees. session-both acts on PRIV and then, last,
// on PUB: bob sees it through PRIV but sees none of PUB's events, so his row for it is its ask on
// PRIV, older than the PUB comment carol reads. An open viewer that does not see PRIV (alice,
// carol's token that lists no project) does not see session-priv or session-both at all, each
// having acted in PRIV (access.SessionClause), and reads session-pub's comment on PUB.
func TestASessionsLastActivityCountsOnlyWhatTheCallerSees(t *testing.T) {
	listener, _ := newBroadcastListener(t, privateRoster)
	seed := seedPrivateProjectWith(t, testServerOptions{envoyURL: listener.URL})
	seedSessionWork(t, seed)
	seed.asSession(t, sessionOfBoth, seedPublic, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPublic]+"/comments",
		map[string]any{"body": "Done on PUB, last."}, http.StatusCreated)
	activity := func(caller privateCaller) map[string]*time.Time {
		t.Helper()
		rows := map[string]*time.Time{}
		for _, agent := range decodeBody[[]struct {
			SessionID    string     `json:"session_id"`
			LastActivity *time.Time `json:"last_activity"`
		}](t, seed.mustRequest(t, caller, http.MethodGet, "/api/v1/agents", nil, http.StatusOK)) {
			rows[agent.SessionID] = agent.LastActivity
		}
		return rows
	}
	alice, bob, carol, carolToken := activity(callerAlice), activity(callerBob), activity(callerCarol), activity(callerCarolToken)
	for caller, rows := range map[privateCaller]map[string]*time.Time{callerAlice: alice, callerCarolToken: carolToken} {
		for _, session := range []string{sessionOfPrivate, sessionOfBoth} {
			if at, listed := rows[session]; listed {
				t.Errorf("%s reads %s listed with last activity %v, want it not listed: it acted in PRIV", caller, session, at)
			}
		}
		if rows[sessionOfPublic] == nil {
			t.Errorf("%s reads no last activity of session-pub, want its comment on PUB", caller)
		}
	}
	for caller, rows := range map[privateCaller]map[string]*time.Time{callerBob: bob, callerCarol: carol} {
		if rows[sessionOfPrivate] == nil {
			t.Errorf("%s reads no last activity of session-priv, want its comment on PRIV", caller)
		}
	}
	if carol[sessionOfBoth] == nil || bob[sessionOfBoth] == nil {
		t.Fatalf("session-both's last activity: bob %v, carol %v, want one for each", bob[sessionOfBoth], carol[sessionOfBoth])
	}
	if !bob[sessionOfBoth].Before(*carol[sessionOfBoth]) {
		t.Errorf("bob reads session-both's last activity at %s and carol at %s, want bob's older: its ask on PRIV, not the later comment on PUB",
			bob[sessionOfBoth], carol[sessionOfBoth])
	}
}

// TestALastActivityCountsTheConversationItsReaderReads: a session's last_activity counts every
// event of the session its reader may read in the event log, under the log's one rule
// (eventReadableSQL), so a restricted member's row for a session it talks to counts the session's
// answer in their conversation, which bob reads in the log as one of its parties. bob, restricted,
// sends session-priv a direct message and session-priv answers it after its comment on PRIV: bob's
// row for session-priv is the answer's time, as carol's is.
func TestALastActivityCountsTheConversationItsReaderReads(t *testing.T) {
	listener, _ := newBroadcastListener(t, privateRoster)
	seed := seedPrivateProjectWith(t, testServerOptions{envoyURL: listener.URL})
	seedSessionWork(t, seed)
	sent := postAgentMessage(t, seed.handler, sessionOfPrivate, map[string]any{"body": "Status?", "delivery": "btw"}, seedBob)
	if sent.Code != http.StatusCreated {
		t.Fatalf("bob messages session-priv: %d %s", sent.Code, sent.Body.String())
	}
	answer := decodeBody[model.Message](t, seed.asSession(t, sessionOfPrivate, seedPrivate, http.MethodPost,
		"/api/v1/messages/"+decodeBody[model.Message](t, sent).ID+"/reply", map[string]any{"attempt": 1, "body": "Green."}, http.StatusCreated))
	answered := seed.database.Pool.QueryRow(t.Context(),
		`select max(created_at) from events where actor->>'kind' = 'session' and actor->>'id' = $1`, sessionOfPrivate)
	var newest time.Time
	if err := answered.Scan(&newest); err != nil {
		t.Fatalf("read session-priv's newest event: %v", err)
	}
	if !newest.After(answer.CreatedAt.Add(-time.Second)) {
		t.Fatalf("session-priv's newest event is at %s, before its answer at %s: the answer appended no event of its own", newest, answer.CreatedAt)
	}
	for _, caller := range []privateCaller{callerBob, callerCarol} {
		var row *time.Time
		for _, agent := range decodeBody[[]struct {
			SessionID    string     `json:"session_id"`
			LastActivity *time.Time `json:"last_activity"`
		}](t, seed.mustRequest(t, caller, http.MethodGet, "/api/v1/agents", nil, http.StatusOK)) {
			if agent.SessionID == sessionOfPrivate {
				row = agent.LastActivity
			}
		}
		if row == nil || !row.Equal(newest) {
			t.Errorf("%s reads session-priv's last activity as %v, want %s: its answer in the conversation %s reads", caller, row, newest, caller)
		}
	}
}

// TestEachReaderIsToldTheUnfetchableCountOfWhatItSees: the live hub counts, for each reader, the
// pull requests that can no longer be fetched that the reader is shown, in its snapshot and in the
// freshness frame a later mark sends it, never another reader's count. The server's own reader is
// shown both; an open reader that sees no project is shown only the one that names no issue, and
// not the one whose issue is HID's.
func TestEachReaderIsToldTheUnfetchableCountOfWhatItSees(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	createTestProject(t, rig.handler, "HID")
	key := issueIn(t, rig.handler, "alice", "HID")
	merged := rig.clock.Now().Add(-3 * time.Hour)
	upsertPR(t, rig.database, hubPR(1, merged))
	hidden := hubPR(2, merged)
	hidden.IssueKey = &key
	upsertPR(t, rig.database, hidden)
	if err := delivery.MarkPullRequestUnfetchable(t.Context(), rig.database.Pool, "acme/widgets", 2, "the pull request is gone"); err != nil {
		t.Fatalf("mark acme/widgets#2 unfetchable: %v", err)
	}

	readers := []struct {
		name                  string
		reader                model.Actor
		snapshot, afterMarked int
	}{
		{"the server's own reader", everyReader, 1, 2},
		{"an open reader that sees no project", model.Actor{Viewer: &model.Viewer{Open: true, Sees: []string{}}}, 0, 1},
	}
	conns := make([]*fakeLiveConn, len(readers))
	for index, reader := range readers {
		conns[index] = newFakeLiveConn()
		rig.hub.Subscribe(conns[index], deliverySubscription{Sub: "p", View: presetView(deliveryLiveTimeline, 7, ""), Reader: reader.reader})
		snapshot := conns[index].snapshot(t, 5*time.Second, "p")
		var body deliveryTimelineWithMeasures
		if err := json.Unmarshal(snapshot.Body, &body); err != nil {
			t.Fatalf("decode %s's snapshot: %v", reader.name, err)
		}
		if got := body.Freshness.UnfetchableCount; got != reader.snapshot {
			t.Errorf("%s's snapshot counts %d unfetchable pull requests, want %d", reader.name, got, reader.snapshot)
		}
	}
	// The mark reaches the hub through the delivery change log, which needs the listener alone.
	rig.waitListening(t)

	if err := delivery.MarkPullRequestUnfetchable(t.Context(), rig.database.Pool, "acme/widgets", 1, "the pull request is gone"); err != nil {
		t.Fatalf("mark acme/widgets#1 unfetchable: %v", err)
	}
	for index, reader := range readers {
		conns[index].delta(t, 3*time.Second, "p")
		fresh := conns[index].freshness(t, 3*time.Second, reader.name+"'s freshness frame after the mark")
		if got := fresh.Freshness.UnfetchableCount; got != reader.afterMarked {
			t.Errorf("%s is told %d unfetchable pull requests, want %d", reader.name, got, reader.afterMarked)
		}
	}
}

// TestADeliveryViewTakesItsCallersNewReaderOnAnAccessChange: a delivery subscription is read for
// its caller as resolved again on each event that can move what the caller sees. A reader who sees
// HID counts its unfetchable pull request; once the caller no longer sees HID and an access change
// commits, the page is sent a snapshot read for the new reader, which counts none, so the rows of a
// project the caller lost leave the page.
func TestADeliveryViewTakesItsCallersNewReaderOnAnAccessChange(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	createTestProject(t, rig.handler, "HID")
	issue := issueIn(t, rig.handler, "alice", "HID")
	hidden := hubPR(2, rig.clock.Now().Add(-3*time.Hour))
	hidden.IssueKey = &issue
	hidden.Repo, hidden.URL = "acme/hidden", "https://github.com/acme/hidden/pull/2"
	upsertPR(t, rig.database, hidden)
	if err := delivery.MarkPullRequestUnfetchable(t.Context(), rig.database.Pool, "acme/hidden", 2, "the pull request is gone"); err != nil {
		t.Fatalf("mark acme/hidden#2 unfetchable: %v", err)
	}
	// An open viewer that does not see HID is one of a deployment where HID is private: with no
	// private project an open viewer sees every project (seesEveryObject), so the caller's sight can
	// move only while some project is private.
	var lost atomic.Bool
	resolve := func(context.Context) (model.Actor, error) {
		if lost.Load() {
			return model.Actor{Viewer: &model.Viewer{Open: true, AnyPrivate: true, Sees: []string{}}}, nil
		}
		return model.Actor{Viewer: &model.Viewer{Open: true, AnyPrivate: true, Sees: []string{"HID"}}}, nil
	}
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, deliverySubscription{Sub: "p", View: presetView(deliveryLiveTimeline, 7, ""), Resolve: resolve})
	counted := func(snapshot DeliveryLiveSnapshot) int {
		var body deliveryTimelineWithMeasures
		if err := json.Unmarshal(snapshot.Body, &body); err != nil {
			t.Fatalf("decode the snapshot: %v", err)
		}
		return body.Freshness.UnfetchableCount
	}
	if got := counted(conn.snapshot(t, 5*time.Second, "p")); got != 1 {
		t.Fatalf("the reader who sees HID counts %d unfetchable pull requests, want 1", got)
	}
	rig.waitListening(t)

	lost.Store(true)
	key := "HID"
	rig.hear(t, appendHubEvent(t, rig, model.Event{ProjectKey: &key, Type: projectAccessChangedEvent, Actor: model.Actor{Kind: "user", ID: "alice"}, Payload: map[string]any{}}))
	if got := counted(conn.snapshot(t, 5*time.Second, "p")); got != 0 {
		t.Errorf("after the caller lost HID the page is sent a snapshot counting %d unfetchable pull requests, want 0", got)
	}
}

// TestADeploymentReadCountsOnlyTheProjectsTheCallerSees: GET /api/v1/priorities, a read of the whole
// deployment that an open viewer still makes, counts only the work of the projects the caller sees:
// PRIV's work is not counted for alice, the shared token or carol's token that lists no project. A
// restricted member or a credential that lists projects makes no such read (DEPLOYMENT_READ_REFUSED,
// readClasses), so only open viewers are asked. The delivery reads have their own rule and test
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

// TestABroadcastShowsOnlyToItsAuthorOrAViewerOfEveryRecipient: a broadcast is read by a caller that
// may see every one of its recipients, when the caller is its author or open; to anyone else GET
// /api/v1/broadcasts leaves it out and GET /api/v1/broadcasts/{id} answers it as an id nobody
// holds. session-priv has acted in PRIV, so an open viewer that does not see PRIV (alice) may not
// see it (access.SessionClause), and reads neither broadcast that went to it; nor does carol's
// token that lists no project, which acts for carol, who sent both, and sees the public projects
// alone (R3), not all carol sees. The replies under a copy are a conversation of no project, read
// when the caller's person or session is a party, or the caller is open, and it may see every
// session party (PartyClause): no caller reads what session-priv answered carol unless it reads
// that conversation, neither in the reply list, nor as the attempt's reply id, nor in the list's
// count of recipients who answered. session-priv answers under the one credential that proves it
// in PRIV (asSession). Both routes are deployment reads (readClasses), so a viewer that is not
// open, a restricted member or a credential that lists projects, is refused them whole, the same
// bytes for a broadcast it wrote, one it could not see and an id nobody holds.
func TestABroadcastShowsOnlyToItsAuthorOrAViewerOfEveryRecipient(t *testing.T) {
	listener, _ := newBroadcastListener(t, privateRoster)
	seed := seedPrivateProjectWith(t, testServerOptions{envoyURL: listener.URL})
	seedSessionWork(t, seed)
	send := func(sessions ...string) broadcastResponse {
		t.Helper()
		response := keyedRequest(t, seed.handler, http.MethodPost, "/api/v1/broadcasts", map[string]any{
			"body": "Status?", "delivery": "btw", "session_ids": sessions,
		}, seedCarol, uuid.NewString())
		if response.Code != http.StatusCreated {
			t.Fatalf("carol broadcasts to %v: %d %s", sessions, response.Code, response.Body.String())
		}
		return decodeBody[broadcastResponse](t, response)
	}
	toPrivate := send(sessionOfPrivate)
	toBoth := send(sessionOfPrivate, sessionOfPublic)
	seed.asSession(t, sessionOfPrivate, seedPrivate, http.MethodPost, "/api/v1/messages/"+toPrivate.Recipients[0].Message.ID+"/reply", map[string]any{
		"attempt": 1, "body": "Green.",
	}, http.StatusCreated)

	for _, test := range []struct {
		caller privateCaller
		// refused is a viewer that is not open, which the deployment class refuses both routes.
		refused bool
		// listed is the broadcasts the caller reads, newest first, and replies whether it reads
		// session-priv's answer under the broadcast to PRIV's session.
		listed  []string
		replies bool
	}{
		{callerAlice, false, []string{}, false},
		{callerBob, true, nil, false},
		{callerCarol, false, []string{toBoth.ID, toPrivate.ID}, true},
		{callerCarolPrivToken, true, nil, false},
		{callerAlicePrivToken, true, nil, false},
		{callerCarolToken, false, []string{}, false},
	} {
		t.Run(string(test.caller), func(t *testing.T) {
			if test.refused {
				list := seed.request(t, test.caller, http.MethodGet, "/api/v1/broadcasts", nil)
				if list.Code != http.StatusForbidden || !strings.Contains(list.Body.String(), `"code":"DEPLOYMENT_READ_REFUSED"`) {
					t.Errorf("%s: GET /api/v1/broadcasts = %d %s, want 403 DEPLOYMENT_READ_REFUSED", test.caller, list.Code, list.Body.String())
				}
				missing := seed.request(t, test.caller, http.MethodGet, "/api/v1/broadcasts/"+uuid.NewString(), nil)
				for _, sent := range []broadcastResponse{toBoth, toPrivate} {
					read := seed.request(t, test.caller, http.MethodGet, "/api/v1/broadcasts/"+sent.ID, nil)
					if read.Code != http.StatusForbidden || read.Body.String() != missing.Body.String() {
						t.Errorf("%s: GET /api/v1/broadcasts/%s = %d %s, want the refusal an id nobody holds gets: %d %s",
							test.caller, sent.ID, read.Code, read.Body.String(), missing.Code, missing.Body.String())
					}
				}
				return
			}
			got := []string{}
			for _, summary := range decodeBody[[]broadcastSummary](t, seed.mustRequest(t, test.caller, http.MethodGet, "/api/v1/broadcasts", nil, http.StatusOK)) {
				got = append(got, summary.ID)
				if answered := summary.Replies > 0; summary.ID == toPrivate.ID && answered != test.replies {
					t.Errorf("%s: GET /api/v1/broadcasts counts %d recipients who answered the broadcast to PRIV's session, want a count only where the answer is read (%t)",
						test.caller, summary.Replies, test.replies)
				}
			}
			if !slices.Equal(got, test.listed) {
				t.Errorf("%s: GET /api/v1/broadcasts = %v, want %v", test.caller, got, test.listed)
			}
			missing := seed.request(t, test.caller, http.MethodGet, "/api/v1/broadcasts/"+uuid.NewString(), nil)
			for _, sent := range []broadcastResponse{toBoth, toPrivate} {
				read := seed.request(t, test.caller, http.MethodGet, "/api/v1/broadcasts/"+sent.ID, nil)
				if !slices.Contains(test.listed, sent.ID) {
					if read.Code != missing.Code || read.Body.String() != missing.Body.String() {
						t.Errorf("%s: GET /api/v1/broadcasts/%s = %d %s, want what an id nobody holds answers: %d %s",
							test.caller, sent.ID, read.Code, read.Body.String(), missing.Code, missing.Body.String())
					}
					continue
				}
				if read.Code != http.StatusOK {
					t.Errorf("%s: GET /api/v1/broadcasts/%s = %d %s, want 200", test.caller, sent.ID, read.Code, read.Body.String())
					continue
				}
				if sent.ID != toPrivate.ID {
					continue
				}
				answered := false
				for _, recipient := range decodeBody[broadcastResponse](t, read).Recipients {
					for _, reply := range recipient.Replies {
						answered = answered || reply.Body == "Green."
					}
					for _, attempt := range recipient.Message.Deliveries {
						answered = answered || attempt.ReplyID != nil
					}
				}
				if answered != test.replies {
					t.Errorf("%s: the broadcast to PRIV's session shows session-priv's answer: %t, want %t", test.caller, answered, test.replies)
				}
			}
		})
	}
}

// TestWhoamiAnswersAPersonalTokensProjects (KF4): GET /api/v1/whoami answers projects, sorted,
// whenever the credential's reads are held to a list, a person's own dsp_ token included, in the
// agent shape; and project, deprecated, as the one key of a one-key list its writes are held to and
// null for any other list. The person shape is a browser's cookie's, which lists nothing, so it
// carries no list. The control is byte for byte: a credential that lists nothing answers exactly
// what it answered before the list existed. A broker token scoped to one project, whose credential
// lists that project (brokerTokenActor), answers it in both fields while a project is private, and
// while none is answers it as project alone, as it did before lists existed (R9); a broker token
// acting as its person answers its list only while a project is private.
func TestWhoamiAnswersAPersonalTokensProjects(t *testing.T) {
	carol := seedCarol
	project := seedPublic
	// A list as a write may hold it, unsorted.
	both := []string{seedPublic, seedPrivate}
	token := func(projects ...string) model.Actor {
		return model.Actor{Kind: "session", Owner: &carol, Credential: &model.Credential{Projects: projects}}
	}
	broker := func(hold model.ListHold) model.Actor {
		return model.Actor{Kind: "session", ID: "ses_x", Credential: &model.Credential{Session: "ses_x", Projects: []string{project}, ListHoldsWhileNoPrivate: hold}}
	}
	// What Resolve answers a broker token while a project is private (held to its list) and while
	// none is (open, no Lists).
	held := &model.Viewer{Sees: []string{project}, AnyPrivate: true, Lists: []string{project}}
	open := &model.Viewer{Open: true, Sees: []string{project, seedPrivate}}
	for _, test := range []struct {
		name string
		body any
		want string
	}{
		{"a token that lists two projects", newWhoamiAgent(token(both...), "", []string{}, nil),
			`{"kind":"agent","owner":"carol@example.com","projects":["PRIV","PUB"],"project":null,"scopes":[],"service":null,"session":null}`},
		{"a token that lists one project", newWhoamiAgent(token(seedPrivate), "", []string{}, nil),
			`{"kind":"agent","owner":"carol@example.com","projects":["PRIV"],"project":"PRIV","scopes":[],"service":null,"session":null}`},
		{"a token that lists none", newWhoamiAgent(token(), "", []string{}, nil),
			`{"kind":"agent","owner":"carol@example.com","project":null,"scopes":[],"service":null,"session":null}`},
		{"a broker token scoped to one project, while a project is private",
			newWhoamiAgent(broker(model.ListHoldsWrites), "", []string{}, held),
			`{"kind":"agent","owner":null,"projects":["PUB"],"project":"PUB","scopes":[],"service":null,"session":"ses_x"}`},
		{"a broker token scoped to one project, while none is",
			newWhoamiAgent(broker(model.ListHoldsWrites), "", []string{}, open),
			`{"kind":"agent","owner":null,"project":"PUB","scopes":[],"service":null,"session":"ses_x"}`},
		{"a broker token acting as its person, while a project is private",
			newWhoamiAgent(broker(model.ListHoldsNothing), "", []string{}, held),
			`{"kind":"agent","owner":null,"projects":["PUB"],"project":"PUB","scopes":[],"service":null,"session":"ses_x"}`},
		{"a broker token acting as its person, while none is",
			newWhoamiAgent(broker(model.ListHoldsNothing), "", []string{}, open),
			`{"kind":"agent","owner":null,"project":null,"scopes":[],"service":null,"session":"ses_x"}`},
		{"a person shape, whatever its credential lists",
			newWhoamiUser(model.Actor{Kind: "user", ID: carol, Credential: &model.Credential{Projects: both}}, "", []string{}),
			`{"kind":"user","login":"carol@example.com","scopes":[]}`},
		{"a person at a browser", newWhoamiUser(model.BrowserActor(seedAlice), "", whoamiScopes(model.BrowserActor(seedAlice).Credential)),
			`{"kind":"user","login":"alice","scopes":["person","approve"]}`},
	} {
		encoded, err := json.Marshal(test.body)
		if err != nil {
			t.Fatalf("encode %s: %v", test.name, err)
		}
		if string(encoded) != test.want {
			t.Errorf("%s answers %s, want %s", test.name, encoded, test.want)
		}
	}
	if !slices.Equal(both, []string{seedPublic, seedPrivate}) {
		t.Errorf("whoami sorted the credential's own list to %v: it answers a sorted copy", both)
	}

	// Through the route: a dsp_ token of carol's that lists both projects, written unsorted as
	// the seed writes a token's list, and the callers that list nothing, byte for byte.
	seed := seedPrivateProject(t)
	const callerCarolBothToken privateCaller = "carol's token that lists PUB and PRIV"
	minted := decodeBody[agentTokenResponse](t, dispatchRequest(t, seed.handler, http.MethodPost, "/api/v1/me/agent-tokens",
		map[string]string{"name": string(callerCarolBothToken)}, seedCarol))
	if _, err := seed.database.Pool.Exec(context.Background(), `update agent_tokens set projects = $2 where id = $1::uuid`, minted.ID, both); err != nil {
		t.Fatalf("write the list of %s: %v", callerCarolBothToken, err)
	}
	seed.tokens[callerCarolBothToken] = minted.Token
	for caller, want := range map[privateCaller]string{
		callerCarolBothToken: `{"kind":"agent","owner":"carol@example.com","projects":["PRIV","PUB"],"private_projects":["PRIV"],"project":null,"scopes":["person","approve"],"service":null,"session":null}`,
		callerCarolToken:     `{"kind":"agent","owner":"carol@example.com","project":null,"scopes":["person","approve"],"service":null,"session":null}`,
		callerSharedToken:    `{"kind":"agent","owner":null,"project":null,"scopes":[],"service":null,"session":null}`,
		callerAlice:          `{"kind":"user","login":"alice","scopes":["person","approve"]}`,
	} {
		response := seed.mustRequest(t, caller, http.MethodGet, "/api/v1/whoami", nil, http.StatusOK)
		if got := strings.TrimSpace(response.Body.String()); got != want {
			t.Errorf("%s: GET /api/v1/whoami = %s, want %s", caller, got, want)
		}
	}
}

// TestTheUsersOfAPublicListedProjectAreEveryFullMember reproduces the state in which the acceptance
// driver (packages/dispatch/e2e/acceptance/project-access.sh) reads GET /api/v1/users as bob and as
// carol's PRIV token: its "listed callers" section runs before its "private" section, so PRIV is
// still public, with bob and carol on its list. The driver's alice is alice@example.com, a full
// member no list names.
//
// A person is one of a project's people when the rule lets them see it (R2), so while PRIV is
// public every full member is one of PRIV's people, and both callers read alice: she can read and
// write PRIV like carol. Once PRIV is private, only its owner and its list see it, and neither
// caller reads her any more.
func TestTheUsersOfAPublicListedProjectAreEveryFullMember(t *testing.T) {
	seed := seedPrivateProject(t)
	const driverAlice = "alice@example.com"
	seedPeople(t, seed.database, driverAlice)
	users := func(caller privateCaller) []string {
		t.Helper()
		logins := []string{}
		for _, user := range decodeBody[listUsersResponse](t, seed.mustRequest(t, caller, http.MethodGet, "/api/v1/users", nil, http.StatusOK)).Users {
			logins = append(logins, user.Login)
		}
		return logins
	}
	for _, state := range []struct {
		visibility string
		want       []string
	}{
		{model.ProjectPublic, []string{"alice", driverAlice, "bob", seedBob, seedCarol}},
		{model.ProjectPrivate, []string{seedBob, seedCarol}},
	} {
		seed.setVisibility(t, seedPrivate, state.visibility)
		for _, caller := range []privateCaller{callerBob, callerCarolPrivToken} {
			if got := users(caller); !slices.Equal(got, state.want) {
				t.Errorf("PRIV %s: %s reads GET /api/v1/users = %v, want %v", state.visibility, caller, got, state.want)
			}
		}
	}
}

// TestAPersonPageOfOnlyHiddenActivityAnswersAsMissing: bob's person node exists only because he
// wrote in PRIV, so a caller that sees nothing of what he wrote reads his page exactly as the
// page of a login nobody holds (404): his page must not tell alice that he was active in a project
// she cannot see. A caller that sees what he wrote, and bob himself, read the page. With no
// private project, every caller that sees the page today reads it byte for byte as it did: alice
// then reads bob's page exactly as carol does.
func TestAPersonPageOfOnlyHiddenActivityAnswersAsMissing(t *testing.T) {
	seed := seedPrivateProject(t)
	seed.mustRequest(t, callerBob, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPrivate]+"/comments",
		map[string]any{"body": "bob was here."}, http.StatusCreated)
	page := "/api/v1/people/" + seedBob + "/nodes"
	missing := seed.request(t, callerAlice, http.MethodGet, "/api/v1/people/nobody@example.com/nodes", nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("a login nobody holds answers %d %s, want 404", missing.Code, missing.Body.String())
	}
	for _, caller := range []privateCaller{callerAlice, callerCarolToken} {
		got := seed.request(t, caller, http.MethodGet, page, nil)
		if got.Code != missing.Code || got.Body.String() != missing.Body.String() {
			t.Errorf("PRIV private: %s reads bob's page = %d %s, want what a login nobody holds answers: %d %s",
				caller, got.Code, got.Body.String(), missing.Code, missing.Body.String())
		}
	}
	for _, caller := range []privateCaller{callerBob, callerCarol, callerCarolPrivToken} {
		rows := decodeBody[personNodes](t, seed.mustRequest(t, caller, http.MethodGet, page, nil, http.StatusOK)).Rows
		if len(rows) != 1 || rows[0].Title != "bob was here." {
			t.Errorf("PRIV private: %s reads bob's page with rows %+v, want his comment", caller, rows)
		}
	}

	// No private project: alice reads bob's page as carol, who sees every project, reads it.
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	want := seed.mustRequest(t, callerCarol, http.MethodGet, page, nil, http.StatusOK)
	if got := seed.request(t, callerAlice, http.MethodGet, page, nil); got.Code != want.Code || got.Body.String() != want.Body.String() {
		t.Errorf("PRIV public: alice reads bob's page = %d %s, want carol's answer byte for byte: %d %s",
			got.Code, got.Body.String(), want.Code, want.Body.String())
	}
}
