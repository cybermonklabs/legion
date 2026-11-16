package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
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
			if test.sees {
				wantPRs = []string{"acme/other#3", "acme/priv#2", "acme/pub#1"}
				wantPasses = []string{"acme/other", "acme/priv", "acme/pub"}
				wantUnowned = []string{seedPrivate, seedPublic}
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

			timeline := decodeBody[deliveryTimelineWithMeasures](t, seed.mustRequest(t, test.caller, http.MethodGet, "/api/v1/delivery/timeline", nil, http.StatusOK))
			gotPRs := []string{}
			for _, pr := range timeline.PRs {
				gotPRs = append(gotPRs, pr.ID)
			}
			slices.Sort(gotPRs)
			if !slices.Equal(gotPRs, wantPRs) {
				t.Errorf("%s: the delivery timeline lists %v, want %v", test.caller, gotPRs, wantPRs)
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
// viewer (PartyClause): bob, who sees the PRIV session a broadcast reached, reads the broadcast but
// not what that session answered carol, neither in the reply list, nor as the attempt's reply id,
// nor in the list's count of recipients who answered.
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
		// listed is the broadcasts the caller reads, newest first, and replies whether it reads
		// session-priv's answer under the broadcast to PRIV's session.
		listed  []string
		replies bool
	}{
		{callerAlice, []string{toBoth.ID, toPrivate.ID}, true},
		{callerBob, []string{toPrivate.ID}, false},
		{callerCarol, []string{toBoth.ID, toPrivate.ID}, true},
		{callerCarolPrivToken, []string{toBoth.ID, toPrivate.ID}, true},
		{callerAlicePrivToken, []string{}, false},
		{callerCarolToken, []string{toBoth.ID, toPrivate.ID}, true},
	} {
		t.Run(string(test.caller), func(t *testing.T) {
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
