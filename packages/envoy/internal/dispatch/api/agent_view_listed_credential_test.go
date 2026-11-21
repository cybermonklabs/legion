package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// listedCredentialSession is bob's session whose listener sent no list: it reads PRIV's issue
// under a broker token naming PRIV and comments on P2's under one naming P2.
const listedCredentialSession = "4e1d6f4b-8c5a-4dae-8f6b-4a5b6c7d8e91"

// listedCredentialProject is the second project bob, a restricted member, is named on.
const listedCredentialProject = "P2"

// TestACredentialThatListsProjectsWatchesNoUnlistedSessionOfItsPerson: the content rule passes a
// restricted member's own session, tied to that person alone, with no listener list only to a
// viewer whose own credential lists no project, the person's browser
// (access.SessionContentClause). A credential of the same person that lists fewer projects must not
// read what a session on a wider list read, and with no listener list nothing bounds what the
// session read. bob, restricted and named on PRIV and on P2, has a session that reads PRIV's issue
// and comments on P2's; its listener sent no list. bob uploads a file to its conversation. bob at
// his browser watches the session and reads its file. bob's personal token that lists P2 alone gets
// the no-responder verdict for it, with no subscribe and no replay, and reads its file as one
// nobody holds.
//
// Control: the own-session arm asking nothing of the viewer's list (`not ($2 ? 'lists')` dropped
// from the function's no-list branch); the token is relayed the session's replay, which holds
// PRIV's issue's text.
func TestACredentialThatListsProjectsWatchesNoUnlistedSessionOfItsPerson(t *testing.T) {
	listener, _ := newBroadcastListener(t, `[
		{"session_id":"`+listedCredentialSession+`","title":"bob's, no list","capabilities":["btw"],"last_seen":100}
	]`)
	stream := agentstream.NewMemory()
	stream.SetReplay(listedCredentialSession, agentstream.Frame(`{"v":1,"kind":"replay","session":"`+listedCredentialSession+`","text":"The issue of PRIV"}`))
	seed := seedPrivateProjectWith(t, testServerOptions{envoyURL: listener.URL, agentStream: stream})
	seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": listedCredentialProject, "name": "Project " + listedCredentialProject,
	}, http.StatusCreated)
	second := decodeBody[struct {
		Key string `json:"key"`
	}](t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": listedCredentialProject, "title": "The issue of " + listedCredentialProject,
	}, http.StatusCreated)).Key
	seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/projects/"+listedCredentialProject+"/access/people/"+seedBob, nil, http.StatusOK)
	seed.asSessionOf(t, seedBob, listedCredentialSession, seedPrivate, http.MethodGet, "/api/v1/issues/"+seed.issue[seedPrivate], nil, http.StatusOK)
	seed.asSessionOf(t, seedBob, listedCredentialSession, listedCredentialProject, http.MethodPost, "/api/v1/issues/"+second+"/comments",
		map[string]any{"body": "Done on P2."}, http.StatusCreated)
	uploaded := multipartRequest(t, seed.handler, "/api/v1/agents/"+listedCredentialSession+"/artifacts",
		map[string]string{"name": "priv.png"}, "priv.png", "image/png", pngBytes("what bob's session read of PRIV"), seedBob)
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("bob uploads to his session's conversation: %d %s", uploaded.Code, uploaded.Body.String())
	}
	file := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact
	token := decodeBody[agentTokenResponse](t, dispatchRequest(t, seed.handler, http.MethodPost, "/api/v1/me/agent-tokens",
		mintBody("bob's token that lists P2", []string{listedCredentialProject}), seedBob)).Token
	if token == "" {
		t.Fatal("mint bob's token that lists P2: no token in the answer")
	}
	url := liveURLOf(t, seed.handler)
	target := "/api/v1/agents/" + listedCredentialSession + "/artifacts/" + file.Slug
	absent := "/api/v1/agents/" + listedCredentialSession + "/artifacts/nobody-holds-this-png"
	read := func(header http.Header, path string) int {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header = header.Clone()
		response := httptest.NewRecorder()
		seed.handler.ServeHTTP(response, request)
		return response.Code
	}

	browser := seed.dialLive(t, url, asUser(seedBob))
	browser.send(t, liveSubscribe("v", agentView(listedCredentialSession), ""))
	if frame, ok := browser.next(t, eventFeedWait, "bob's browser's agent view of his session").(LiveAgentFrame); !ok || string(frame.Data) != agentViewResponding {
		t.Fatalf("bob at his browser watches his own session as %+v, want its responder: the row proves nothing", frame)
	}
	if got := read(asUser(seedBob), target); got != http.StatusOK {
		t.Fatalf("bob at his browser reads his own session's file as %d, want 200: the row proves nothing", got)
	}

	watches := stream.Watches(listedCredentialSession)
	listed := seed.dialLive(t, url, withBearer(token))
	listed.send(t, liveSubscribe("v", agentView(listedCredentialSession), ""))
	if frame, ok := listed.next(t, eventFeedWait, "bob's P2 token's agent view of his session").(LiveAgentFrame); !ok || string(frame.Data) != agentViewHidden {
		relayed := "nothing"
		if ok && string(frame.Data) == agentViewResponding {
			replay, _ := json.Marshal(listed.next(t, eventFeedWait, "bob's P2 token's replay"))
			relayed = string(replay)
		}
		t.Errorf("bob's token that lists P2 alone watches his session that read PRIV, with no listener list, as %+v and is relayed %s, want the no-responder verdict",
			frame, relayed)
	} else if told := stream.Watches(listedCredentialSession) - watches; told != 0 {
		t.Errorf("bob's P2 token's hidden view told the session %d times a viewer is attached, want none", told)
	}
	if got, nobody := read(withBearer(token), target), read(withBearer(token), absent); got != http.StatusNotFound || got != nobody {
		t.Errorf("bob's token that lists P2 alone reads his PRIV-reading session's file as %d, want what one nobody holds answers, %d", got, nobody)
	}
}
