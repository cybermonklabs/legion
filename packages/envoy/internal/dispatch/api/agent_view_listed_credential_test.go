package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// listedCredentialSession is bob's session whose listener sent no list.
const listedCredentialSession = "4e1d6f4b-8c5a-4dae-8f6b-4a5b6c7d8e91"

// listedCredentialProject is the second project bob, a restricted member, is named on, public
// while his session reads it.
const listedCredentialProject = "P2"

// listedCredentialRig is a private seed whose listener holds bob's session with no list, its replay
// holding the text of P2's issue, with P2 made by carol and naming bob on its list.
type listedCredentialRig struct {
	seed   privateSeed
	stream *agentstream.Memory
	url    string
	// issue is P2's issue; target is the file bob uploads to the session's conversation once it
	// has read, and absent the same read of a file nobody holds.
	issue, target, absent string
}

func newListedCredentialRig(t *testing.T) *listedCredentialRig {
	t.Helper()
	listener, _ := newBroadcastListener(t, `[
		{"session_id":"`+listedCredentialSession+`","title":"bob's, no list","capabilities":["btw"],"last_seen":100}
	]`)
	stream := agentstream.NewMemory()
	stream.SetReplay(listedCredentialSession, agentstream.Frame(`{"v":1,"kind":"replay","session":"`+listedCredentialSession+`","text":"The issue of P2"}`))
	seed := seedPrivateProjectWith(t, testServerOptions{envoyURL: listener.URL, agentStream: stream})
	seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": listedCredentialProject, "name": "Project " + listedCredentialProject,
	}, http.StatusCreated)
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": listedCredentialProject, "title": "The issue of " + listedCredentialProject,
	}, http.StatusCreated)).Key
	seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/projects/"+listedCredentialProject+"/access/people/"+seedBob, nil, http.StatusOK)
	return &listedCredentialRig{seed: seed, stream: stream, url: liveURLOf(t, seed.handler), issue: issue,
		absent: "/api/v1/agents/" + listedCredentialSession + "/artifacts/nobody-holds-this-png"}
}

// upload has bob, at his browser, upload a file to the session's conversation.
func (rig *listedCredentialRig) upload(t *testing.T) {
	t.Helper()
	uploaded := multipartRequest(t, rig.seed.handler, "/api/v1/agents/"+listedCredentialSession+"/artifacts",
		map[string]string{"name": "p2.png"}, "p2.png", "image/png", pngBytes("what bob's session read of P2"), seedBob)
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("bob uploads to his session's conversation: %d %s", uploaded.Code, uploaded.Body.String())
	}
	rig.target = "/api/v1/agents/" + listedCredentialSession + "/artifacts/" + decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact.Slug
}

// mint mints bob's personal token named name that lists projects.
func (rig *listedCredentialRig) mint(t *testing.T, name string, projects []string) string {
	t.Helper()
	token := decodeBody[agentTokenResponse](t, dispatchRequest(t, rig.seed.handler, http.MethodPost, "/api/v1/me/agent-tokens",
		mintBody(name, projects), seedBob)).Token
	if token == "" {
		t.Fatalf("mint %s: no token in the answer", name)
	}
	return token
}

// read is the status of one GET of path under header.
func (rig *listedCredentialRig) read(t *testing.T, header http.Header, path string) int {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header = header.Clone()
	response := httptest.NewRecorder()
	rig.seed.handler.ServeHTTP(response, request)
	return response.Code
}

// watched holds the credential header carries, named who, to the verdict want for bob's session,
// and to the session's file: 200 for a view that responds, and what a file nobody holds answers
// for one that does not. A hidden view subscribes nothing and tells the session nothing.
func (rig *listedCredentialRig) watched(t *testing.T, state, who string, header http.Header, want string) {
	t.Helper()
	watches := rig.stream.Watches(listedCredentialSession)
	client := rig.seed.dialLive(t, rig.url, header)
	client.send(t, liveSubscribe("v", agentView(listedCredentialSession), ""))
	frame, ok := client.next(t, eventFeedWait, who+"'s agent view of bob's session").(LiveAgentFrame)
	switch {
	case !ok || string(frame.Data) != want:
		relayed := "nothing"
		if ok && string(frame.Data) == agentViewResponding {
			replay, _ := json.Marshal(client.next(t, eventFeedWait, who+"'s replay"))
			relayed = string(replay)
		}
		t.Errorf("%s, %s watches bob's session with no listener list as %+v and is relayed %s, want %s", state, who, frame, relayed, want)
	case want == agentViewHidden:
		if told := rig.stream.Watches(listedCredentialSession) - watches; told != 0 {
			t.Errorf("%s, %s's hidden view told the session %d times a viewer is attached, want none", state, who, told)
		}
	}
	got, nobody := rig.read(t, header, rig.target), rig.read(t, header, rig.absent)
	if want == agentViewResponding && got != http.StatusOK {
		t.Errorf("%s, %s reads bob's session's file as %d, want 200", state, who, got)
	}
	if want == agentViewHidden && (got != http.StatusNotFound || got != nobody) {
		t.Errorf("%s, %s reads bob's session's file as %d, want what one nobody holds answers, %d", state, who, got, nobody)
	}
}

// TestARestrictedMembersNoListTokenWatchesNoSessionThatReadAProjectMadePrivate: the content rule
// passes a restricted member's own session with no listener list only to a credential that sees
// every project its person sees (access.SessionContentClause), since nothing else bounds what the
// session read. A restricted member's credential is held to its list for every project, and holds
// exactly one project (R16): no mint makes one that lists none, and one that a mint before that rule
// made acts for nobody (personalTokenActor). bob, a restricted member, is named on PUB and P2, both
// public; his session comments on PUB's issue under a broker token listing PUB, and reads P2's issue
// under one listing P2, which records nothing, and bob uploads a file to its conversation. bob's
// personal token that lists no project, written as such a mint wrote it, reads neither the
// session's file nor P2's issue while P2 is public, 403 RESTRICTED_MEMBER. carol makes P2 private,
// with bob still on its list, so no pair is recorded: bob at his browser still watches the session
// and reads its file, and the token that lists no project is refused as before.
//
// Control: let a restricted owner's token act whatever it lists (personalTokenActor not refusing a
// list of other than one project), and its first read answers 200 and the row fails.
func TestARestrictedMembersNoListTokenWatchesNoSessionThatReadAProjectMadePrivate(t *testing.T) {
	rig := newListedCredentialRig(t)
	// bob is named on PUB and P2 alone, so a credential of his that lists nothing would see what he
	// sees while P2 is public.
	rig.seed.mustRequest(t, callerCarol, http.MethodDelete, "/api/v1/projects/"+seedPrivate+"/access/people/"+seedBob, nil, http.StatusOK)
	rig.seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/projects/"+seedPublic+"/access/people/"+seedBob, nil, http.StatusOK)
	rig.seed.asSessionOf(t, seedBob, listedCredentialSession, seedPublic, http.MethodPost, "/api/v1/issues/"+rig.seed.issue[seedPublic]+"/comments",
		map[string]any{"body": "Done on PUB."}, http.StatusCreated)
	rig.seed.asSessionOf(t, seedBob, listedCredentialSession, listedCredentialProject, http.MethodGet, "/api/v1/issues/"+rig.issue, nil, http.StatusOK)
	rig.upload(t)
	unlisted := withBearer(personalTokenListing(t, rig.seed.database, seedBob, nil, model.ScopePerson))
	refused := func(state string) {
		t.Helper()
		for _, path := range []string{rig.target, "/api/v1/issues/" + rig.issue} {
			if got := rig.read(t, unlisted, path); got != http.StatusForbidden {
				t.Errorf("%s, bob's token that lists no project reads %s as %d, want 403: a restricted member's token lists exactly one project", state, path, got)
			}
		}
	}
	refused("P2 public")

	rig.seed.setVisibility(t, listedCredentialProject, model.ProjectPrivate)
	const state = "P2 made private, bob on its list"
	rig.watched(t, state, "bob at his browser", asUser(seedBob), agentViewResponding)
	refused(state)
}

// TestARestrictedMembersTokenThatListsFewerProjectsWatchesNoUnlistedSessionOfHis: bob, a restricted
// member named on PRIV and on P2, which is public, has a session that comments on PRIV's issue under
// a broker token listing PRIV and reads P2's issue under one listing P2; its listener sent no list.
// The read of a public project records nothing. bob at his browser watches the session and reads
// its file; his personal token that lists PRIV alone, which sees every private project bob sees but
// not P2, reads the file as one nobody holds: a restricted member's credential is held to its list
// for public projects too, so a session with no list passes only a credential that sees every
// project its person sees. The token watches no agent at all: it holds no scope (a restricted
// member's token, R16), and the view needs the person scope, so its subscribe is refused 403
// OAUTH_SCOPE_REQUIRED.
//
// Control: the credential tested on private projects alone (every private project the viewer's
// person sees is one it sees), and the PRIV token reads the session's file.
func TestARestrictedMembersTokenThatListsFewerProjectsWatchesNoUnlistedSessionOfHis(t *testing.T) {
	rig := newListedCredentialRig(t)
	rig.seed.asSessionOf(t, seedBob, listedCredentialSession, seedPrivate, http.MethodPost, "/api/v1/issues/"+rig.seed.issue[seedPrivate]+"/comments",
		map[string]any{"body": "Done on PRIV."}, http.StatusCreated)
	rig.seed.asSessionOf(t, seedBob, listedCredentialSession, listedCredentialProject, http.MethodGet, "/api/v1/issues/"+rig.issue, nil, http.StatusOK)
	rig.upload(t)
	const state = "PRIV private, P2 public"
	rig.watched(t, state, "bob at his browser", asUser(seedBob), agentViewResponding)
	token := withBearer(rig.mint(t, "bob's token that lists PRIV", []string{seedPrivate}))
	if got, nobody := rig.read(t, token, rig.target), rig.read(t, token, rig.absent); got != http.StatusNotFound || got != nobody {
		t.Errorf("%s, bob's token that lists PRIV alone reads bob's session's file as %d, want what one nobody holds answers, %d", state, got, nobody)
	}
	client := rig.seed.dialLive(t, rig.url, token)
	client.send(t, liveSubscribe("v", agentView(listedCredentialSession), ""))
	if refusal, ok := client.next(t, eventFeedWait, "bob's token's agent view").(LiveError); !ok || refusal.Code != codeScopeRequired {
		t.Errorf("%s, bob's token that lists PRIV alone subscribes to bob's session's view as %+v, want %s", state, refusal, codeScopeRequired)
	}
}
