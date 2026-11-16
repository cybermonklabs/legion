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
// answers them in this order.
const privateRoster = `[
	{"session_id":"session-both","title":"Both","capabilities":["btw","steer"],"last_seen":400},
	{"session_id":"session-priv","title":"Private","capabilities":["btw","steer"],"last_seen":300},
	{"session_id":"session-pub","title":"Public","capabilities":["btw","steer"],"last_seen":200},
	{"session_id":"session-idle","title":"Idle","capabilities":["btw","steer"],"last_seen":100}
]`

// seedSessionWork has the roster's sessions act in the seed's projects, each through a personal
// token of carol's that may write there: what a session writes in a project is what makes it one
// of the project's agents (session_projects).
func seedSessionWork(t *testing.T, seed privateSeed) {
	t.Helper()
	act := func(caller privateCaller, project, route, session string, body map[string]any) {
		t.Helper()
		body["actor"] = map[string]any{"kind": "session", "id": session}
		seed.mustRequest(t, caller, http.MethodPost, "/api/v1/issues/"+seed.issue[project]+"/"+route, body, http.StatusCreated)
	}
	act(callerCarolToken, seedPublic, "comments", sessionOfPublic, map[string]any{"body": "Done on PUB."})
	act(callerCarolPrivToken, seedPrivate, "comments", sessionOfPrivate, map[string]any{"body": "Done on PRIV."})
	act(callerCarolToken, seedPublic, "asks", sessionOfBoth, map[string]any{"question": "Ship PUB?"})
	act(callerCarolPrivToken, seedPrivate, "asks", sessionOfBoth, map[string]any{"question": "Ship PRIV?"})
}

// TestARestrictedMemberSeesOnlyThePeopleAndAgentsOfItsProjects: GET /api/v1/users, GET
// /api/v1/agents and GET /api/v1/people/{login}/nodes as each caller of verification v16. bob, and
// a credential that lists projects, read only the people who see a project they see (PersonClause)
// and only the sessions that acted in one (SessionClause); an open viewer reads every person and
// every session, as every caller did before. A session's open asks count only the asks of the
// projects the caller sees, and a person the caller may not see answers exactly as a login nobody
// holds.
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
	for _, test := range []struct {
		caller privateCaller
		// users is nil for a caller GET /api/v1/users refuses: it needs the person scope.
		users  []string
		agents []string
		// pages is which of alice's and carol's pages the caller opens; the other answers as a
		// login nobody holds.
		pages []string
	}{
		{callerAlice, everyone, every(1), []string{seedAlice, seedCarol}},
		{callerBob, ofPrivate, privateOnly, []string{seedCarol}},
		{callerCarol, everyone, every(2), []string{seedAlice, seedCarol}},
		{callerCarolPrivToken, ofPrivate, privateOnly, []string{seedCarol}},
		{callerAlicePrivToken, []string{seedAlice}, []string{}, []string{seedAlice}},
		{callerCarolToken, everyone, every(1), []string{seedAlice, seedCarol}},
		{callerSharedToken, nil, every(1), nil},
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

// TestASessionsLastActivityCountsOnlyWhatTheCallerSees: an agent row's last_activity is the newest
// event of the session among the events the caller sees. alice sees session-priv, as an open
// viewer sees every session, but none of its events, so its row answers as a session with no
// activity; session-both's newest event is its ask on PRIV, so alice's row for it is its ask on
// PUB, older than the one bob and carol read.
func TestASessionsLastActivityCountsOnlyWhatTheCallerSees(t *testing.T) {
	listener, _ := newBroadcastListener(t, privateRoster)
	seed := seedPrivateProjectWith(t, testServerOptions{envoyURL: listener.URL})
	seedSessionWork(t, seed)
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
		if at, listed := rows[sessionOfPrivate]; !listed || at != nil {
			t.Errorf("%s reads session-priv listed %t with last activity %v, want it listed with none: every event of it is PRIV's", caller, listed, at)
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
	if alice[sessionOfBoth] == nil || carol[sessionOfBoth] == nil || bob[sessionOfBoth] == nil {
		t.Fatalf("session-both's last activity: alice %v, bob %v, carol %v, want one for each", alice[sessionOfBoth], bob[sessionOfBoth], carol[sessionOfBoth])
	}
	if !alice[sessionOfBoth].Before(*carol[sessionOfBoth]) {
		t.Errorf("alice reads session-both's last activity at %s and carol at %s, want alice's older: its ask on PUB, not the later one on PRIV",
			alice[sessionOfBoth], carol[sessionOfBoth])
	}
	if !bob[sessionOfBoth].Equal(*carol[sessionOfBoth]) {
		t.Errorf("bob reads session-both's last activity at %s and carol at %s, want the same: its ask on PRIV, which both see",
			bob[sessionOfBoth], carol[sessionOfBoth])
	}
}

// TestEachReaderIsToldTheUnfetchableCountOfWhatItSees: the live hub counts, for each reader, the
// pull requests that can no longer be fetched in the repositories that reader sees, in its
// snapshot and in the freshness frame a later mark sends it, never another reader's count. The
// server's own reader sees both repositories; an open reader that sees no project sees only the one
// no project maps.
func TestEachReaderIsToldTheUnfetchableCountOfWhatItSees(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	createTestProject(t, rig.handler, "HID")
	if response := dispatchRequest(t, rig.handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/hidden",
		map[string]any{"project": "HID"}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("map acme/hidden to HID: %d %s", response.Code, response.Body.String())
	}
	merged := rig.clock.Now().Add(-3 * time.Hour)
	upsertPR(t, rig.database, hubPR(1, merged))
	hidden := hubPR(2, merged)
	hidden.Repo, hidden.URL = "acme/hidden", "https://github.com/acme/hidden/pull/2"
	upsertPR(t, rig.database, hidden)
	if err := delivery.MarkPullRequestUnfetchable(t.Context(), rig.database.Pool, "acme/hidden", 2, "the pull request is gone"); err != nil {
		t.Fatalf("mark acme/hidden#2 unfetchable: %v", err)
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
	rig.waitConnected(t)

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
	if response := dispatchRequest(t, rig.handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/hidden",
		map[string]any{"project": "HID"}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("map acme/hidden to HID: %d %s", response.Code, response.Body.String())
	}
	hidden := hubPR(2, rig.clock.Now().Add(-3*time.Hour))
	hidden.Repo, hidden.URL = "acme/hidden", "https://github.com/acme/hidden/pull/2"
	upsertPR(t, rig.database, hidden)
	if err := delivery.MarkPullRequestUnfetchable(t.Context(), rig.database.Pool, "acme/hidden", 2, "the pull request is gone"); err != nil {
		t.Fatalf("mark acme/hidden#2 unfetchable: %v", err)
	}
	var lost atomic.Bool
	resolve := func(context.Context) (model.Actor, error) {
		if lost.Load() {
			return model.Actor{Viewer: &model.Viewer{Open: true, Sees: []string{}}}, nil
		}
		return model.Actor{Viewer: &model.Viewer{Open: true, Sees: []string{"HID"}}}, nil
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
	rig.waitConnected(t)

	lost.Store(true)
	key := "HID"
	appendHubEvent(t, rig, model.Event{ProjectKey: &key, Type: projectAccessChangedEvent, Actor: model.Actor{Kind: "user", ID: "alice"}, Payload: map[string]any{}})
	if got := counted(conn.snapshot(t, 5*time.Second, "p")); got != 0 {
		t.Errorf("after the caller lost HID the page is sent a snapshot counting %d unfetchable pull requests, want 0", got)
	}
}

// TestADeploymentReadCountsOnlyTheProjectsTheCallerSees: the reads of the whole deployment that an
// open viewer still makes, GET /api/v1/priorities, the delivery timeline and measures and GET
// /api/v1/ci-reconcile/passes, count and list only the work of the projects the caller sees. A
// pull request or a pass of a repository mapped to PRIV is not there for alice, the shared token
// or carol's token that lists no project, and one of a repository no project maps stays for each
// of them. A restricted member or a credential that lists projects makes none of these reads
// (DEPLOYMENT_READ_REFUSED, routes_table.go), so only open viewers are asked.
func TestADeploymentReadCountsOnlyTheProjectsTheCallerSees(t *testing.T) {
	seed := seedPrivateProject(t)
	ctx := context.Background()
	for repo, project := range map[string]string{"acme/pub": seedPublic, "acme/priv": seedPrivate} {
		seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/settings/repo-projects/"+repo,
			map[string]any{"project": project}, http.StatusOK)
	}

	// The priorities: one priority each project's work advances, and each project's seed issue
	// in flight under no priority.
	seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/priorities", map[string]any{"id": "ship-it", "title": "Ship it"}, http.StatusCreated)
	for _, project := range []string{seedPublic, seedPrivate} {
		seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": project, "title": "Advance on " + project, "advances": []string{"ship-it"}, "force": true,
		}, http.StatusCreated)
		seed.mustRequest(t, callerCarol, http.MethodPatch, "/api/v1/issues/"+seed.issue[project],
			map[string]any{"status": "in_progress"}, http.StatusOK)
		// An open P0 nobody claims, which the measures list.
		seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": project, "title": "Unowned P0 of " + project, "priority": 0, "force": true,
		}, http.StatusCreated)
	}

	// The delivery timeline's settings and pull requests, as the intake stores them.
	if _, err := delivery.PutSettings(ctx, seed.database.Pool, delivery.DeliverySettings{
		DeployRepo: "acme/deploy", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobNames: []string{"release / release"}, PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{"octocat"},
	}, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("write the delivery settings: %v", err)
	}
	merged := time.Now().UTC().Add(-time.Hour)
	opened := merged.Add(-time.Hour)
	for number, repo := range map[int]string{1: "acme/pub", 2: "acme/priv", 3: "acme/other"} {
		if err := delivery.UpsertPullRequest(ctx, seed.database.Pool, delivery.DeliveryPullRequest{
			Repo: repo, Number: number, Title: "change of " + repo, Author: "octocat",
			URL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, number), CreatedAt: &opened, MergedAt: &merged,
		}); err != nil {
			t.Fatalf("store %s#%d: %v", repo, number, err)
		}
	}
	// Two of them can no longer be fetched from GitHub, as the intake marks one.
	for number, repo := range map[int]string{2: "acme/priv", 3: "acme/other"} {
		if err := delivery.MarkPullRequestUnfetchable(ctx, seed.database.Pool, repo, number, "the pull request is gone"); err != nil {
			t.Fatalf("mark %s#%d unfetchable: %v", repo, number, err)
		}
	}

	// Three ended passes, each leaving its cursor in another repository, as a pass writes its row.
	instance := seedServerProcess(t, seed.database, 0)
	for index, repo := range []string{"acme/pub", "acme/priv", "acme/other"} {
		id := endedCIPass(t, seed.database, instance, time.Duration(index+1)*time.Hour, "complete")
		if _, err := seed.database.Pool.Exec(ctx, `update ci_reconcile_passes set cursor_repository = $2 where id = $1`, id, repo); err != nil {
			t.Fatalf("leave pass %d in %s: %v", id, repo, err)
		}
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
			wantPRs := []string{"acme/other#3", "acme/pub#1"}
			wantPasses := []string{"acme/other", "acme/pub"}
			wantUnowned := []string{seedPublic}
			wantByProject := []string{seedPublic}
			wantAdvancing := 1
			wantUnfetchable := 1
			if test.sees {
				wantPRs = []string{"acme/other#3", "acme/priv#2", "acme/pub#1"}
				wantPasses = []string{"acme/other", "acme/priv", "acme/pub"}
				wantUnowned = []string{seedPrivate, seedPublic}
				wantByProject = []string{seedPrivate, seedPublic}
				wantAdvancing = 2
				wantUnfetchable = 2
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

			timeline := decodeBody[deliveryTimelineWithMeasures](t, seed.mustRequest(t, test.caller, http.MethodGet, "/api/v1/delivery/timeline", nil, http.StatusOK))
			gotPRs := []string{}
			for _, pr := range timeline.PRs {
				gotPRs = append(gotPRs, pr.ID)
			}
			slices.Sort(gotPRs)
			if !slices.Equal(gotPRs, wantPRs) {
				t.Errorf("%s: the delivery timeline lists %v, want %v", test.caller, gotPRs, wantPRs)
			}
			if got := timeline.Freshness.UnfetchableCount; got != wantUnfetchable {
				t.Errorf("%s: the delivery timeline counts %d pull requests that can no longer be fetched, want %d", test.caller, got, wantUnfetchable)
			}
			measured := decodeBody[deliveryMeasuresResponse](t, seed.mustRequest(t, test.caller, http.MethodGet, "/api/v1/delivery/measures", nil, http.StatusOK))
			gotUnowned := []string{}
			for _, issue := range measured.UnownedP0 {
				project, _, _ := strings.Cut(issue.Key, "-")
				gotUnowned = append(gotUnowned, project)
			}
			slices.Sort(gotUnowned)
			if !slices.Equal(gotUnowned, wantUnowned) {
				t.Errorf("%s: the measures list unowned P0 issues of %v, want %v", test.caller, gotUnowned, wantUnowned)
			}
			if got := measured.Freshness.UnfetchableCount; got != wantUnfetchable {
				t.Errorf("%s: the measures count %d pull requests that can no longer be fetched, want %d", test.caller, got, wantUnfetchable)
			}

			passes := decodeBody[[]ciPassView](t, seed.mustRequest(t, test.caller, http.MethodGet, "/api/v1/ci-reconcile/passes", nil, http.StatusOK))
			gotPasses := []string{}
			for _, pass := range passes {
				if pass.Cursor != nil {
					gotPasses = append(gotPasses, pass.Cursor.Repository)
				}
			}
			slices.Sort(gotPasses)
			if !slices.Equal(gotPasses, wantPasses) {
				t.Errorf("%s: GET /api/v1/ci-reconcile/passes answers passes left in %v, want %v", test.caller, gotPasses, wantPasses)
			}
		})
	}
}

// TestABroadcastShowsOnlyToItsAuthorOrAViewerOfEveryRecipient: a broadcast is read by its author,
// and by a caller that may see every one of its recipients; to anyone else GET
// /api/v1/broadcasts leaves it out and GET /api/v1/broadcasts/{id} answers it as an id nobody
// holds. The replies under a copy are a conversation of no project, read by its parties or an open
// viewer (PartyClause): no caller reads what session-priv answered carol unless it reads that
// conversation, neither in the reply list, nor as the attempt's reply id, nor in the list's count
// of recipients who answered. Both routes are deployment reads (plan A, "Read classes"), so a
// viewer that is not open, a restricted member or a credential that lists projects, is refused
// them whole, the same bytes for a broadcast it wrote, one it could not see and an id nobody holds.
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
	if reply := bearerRequest(t, seed.handler, http.MethodPost, "/api/v1/messages/"+toPrivate.Recipients[0].Message.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": sessionOfPrivate}, "attempt": 1, "body": "Green.",
	}); reply.Code != http.StatusCreated {
		t.Fatalf("session-priv answers carol: %d %s", reply.Code, reply.Body.String())
	}

	for _, test := range []struct {
		caller privateCaller
		// refused is a viewer that is not open, which the deployment class refuses both routes.
		refused bool
		// listed is the broadcasts the caller reads, newest first, and replies whether it reads
		// session-priv's answer under the broadcast to PRIV's session.
		listed  []string
		replies bool
	}{
		{callerAlice, false, []string{toBoth.ID, toPrivate.ID}, true},
		{callerBob, true, nil, false},
		{callerCarol, false, []string{toBoth.ID, toPrivate.ID}, true},
		{callerCarolPrivToken, true, nil, false},
		{callerAlicePrivToken, true, nil, false},
		{callerCarolToken, false, []string{toBoth.ID, toPrivate.ID}, true},
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
// whenever the credential lists projects, a person's own dsp_ token included, in the person shape
// as in the agent shape; and project, deprecated, as the one key of a one-key list and null for
// any other list. The control is byte for byte: a credential that lists nothing answers exactly
// what it answered before the list existed, and the broker token of this stack, which names its
// one project in Actor.Project and no list yet, still answers that project.
func TestWhoamiAnswersAPersonalTokensProjects(t *testing.T) {
	carol := seedCarol
	project := seedPublic
	// A list as a write may hold it, unsorted.
	both := []string{seedPublic, seedPrivate}
	token := func(projects ...string) model.Actor {
		return model.Actor{Kind: "session", Owner: &carol, Credential: &model.Credential{Projects: projects}}
	}
	for _, test := range []struct {
		name string
		body any
		want string
	}{
		{"a token that lists two projects", newWhoamiAgent(token(both...), "", []string{}),
			`{"kind":"agent","owner":"carol@example.com","projects":["PRIV","PUB"],"project":null,"scopes":[],"service":null,"session":null}`},
		{"a token that lists one project", newWhoamiAgent(token(seedPrivate), "", []string{}),
			`{"kind":"agent","owner":"carol@example.com","projects":["PRIV"],"project":"PRIV","scopes":[],"service":null,"session":null}`},
		{"a token that lists none", newWhoamiAgent(token(), "", []string{}),
			`{"kind":"agent","owner":"carol@example.com","project":null,"scopes":[],"service":null,"session":null}`},
		{"a broker token naming one project and no list",
			newWhoamiAgent(model.Actor{Kind: "session", ID: "ses_x", Project: &project, Credential: &model.Credential{Session: "ses_x"}}, "", []string{}),
			`{"kind":"agent","owner":null,"project":"PUB","scopes":[],"service":null,"session":"ses_x"}`},
		{"a person whose credential lists two projects",
			newWhoamiUser(model.Actor{Kind: "user", ID: carol, Credential: &model.Credential{Projects: both}}, "", []string{}),
			`{"kind":"user","login":"carol@example.com","projects":["PRIV","PUB"],"scopes":[]}`},
		{"a person at a browser", newWhoamiUser(model.Actor{Kind: "user", ID: seedAlice, Credential: personCredential}, "", whoamiScopes(personCredential)),
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
		callerCarolBothToken: `{"kind":"agent","owner":"carol@example.com","projects":["PRIV","PUB"],"project":null,"scopes":["person","approve"],"service":null,"session":null}`,
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
