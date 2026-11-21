package api

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// restrictedOwnSession is bob's session with no listener list, tied to bob alone by its proved write.
const restrictedOwnSession = "5c0e7e17-3a1b-4c2d-9e8f-0a1b2c3d4e61"

// TestARestrictedMemberReadsTheFilesOfASessionTiedToHimAlone: a restricted member watches a session
// tied to him alone, with or without a listener list (access.SessionContentClause), and a session's
// files are its content, read under the same rule (access.DocumentClause through
// access.PartyClause, whose doc says a viewer that is not open reads a conversation with a session
// "only where its own person proved that session, as its agent view watches it"). bob, a restricted
// member PRIV's list names, proves a session by a comment on PRIV's issue, uploads a file to its
// conversation and sends it a direct message: he reads both. alice, an open viewer PRIV's list does
// not name, reads the file as one nobody holds: the session wrote in PRIV. Then carol's credential
// writes as the session, which ties it to carol too, and bob reads the file and his direct message
// as ones nobody holds from that write on.
//
// At cybermonklabs/legion#4177's 3338788cd1 bob's read of the file answers 404 ARTIFACT_NOT_FOUND
// while his direct message answers 200: the file's parties are the session alone, and PartyClause
// passes a viewer that is not open only when its own person or session is a party, before it holds
// the session to the content rule. Found by the tester's drive of the agent view as its users
// (Tester4177, round 4: carol, restricted on PUB, could not read the file of her own session).
func TestARestrictedMemberReadsTheFilesOfASessionTiedToHimAlone(t *testing.T) {
	listener, _ := newBroadcastListener(t, `[
		{"session_id":"`+restrictedOwnSession+`","title":"bob's, no list","capabilities":["btw"],"last_seen":100}
	]`)
	seed := seedPrivateProjectWith(t, testServerOptions{envoyURL: listener.URL})
	seed.asSessionOf(t, seedBob, restrictedOwnSession, seedPrivate, http.MethodPost,
		"/api/v1/issues/"+seed.issue[seedPrivate]+"/comments", map[string]any{"body": "Done on PRIV."}, http.StatusCreated)
	seed.mustRequest(t, callerCarol, http.MethodGet, "/api/v1/agents", nil, http.StatusOK)

	uploaded := multipartRequest(t, seed.handler, "/api/v1/agents/"+restrictedOwnSession+"/artifacts",
		map[string]string{"name": "seen.png"}, "seen.png", "image/png", pngBytes("what bob's session saw"), seedBob)
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("bob uploads to his session's conversation: %d %s", uploaded.Code, uploaded.Body.String())
	}
	file := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact
	direct := createdID(t, seed.mustRequest(t, callerBob, http.MethodPost, "/api/v1/agents/"+restrictedOwnSession+"/messages",
		map[string]any{"body": "What did you read today?", "delivery": "btw"}, http.StatusCreated))

	if read := seed.request(t, callerBob, http.MethodGet, "/api/v1/messages/"+direct, nil); read.Code != http.StatusOK {
		t.Fatalf("bob reads his direct message to his own session: %d %s; the row proves nothing", read.Code, read.Body.String())
	}
	target := "/api/v1/agents/" + restrictedOwnSession + "/artifacts/" + file.Slug
	if read := seed.request(t, callerBob, http.MethodGet, target, nil); read.Code != http.StatusOK {
		t.Errorf("bob, a restricted member, reads the file of his own session, tied to him alone, as %d %s, want 200: his agent view relays the session",
			read.Code, read.Body.String())
	}
	hidden := seed.request(t, callerAlice, http.MethodGet, target, nil)
	absent := seed.request(t, callerAlice, http.MethodGet, "/api/v1/agents/"+restrictedOwnSession+"/artifacts/nobody-holds-this-png", nil)
	if hidden.Code != http.StatusNotFound || absent.Code != http.StatusNotFound {
		t.Errorf("alice, who does not see PRIV, reads the file of bob's PRIV session as %d and a file nobody holds as %d, want 404 both", hidden.Code, absent.Code)
	}

	seed.asSessionOf(t, seedCarol, restrictedOwnSession, seedPrivate, http.MethodPost,
		"/api/v1/issues/"+seed.issue[seedPrivate]+"/comments", map[string]any{"body": "carol, as bob's session."}, http.StatusCreated)
	for name, read := range map[string][2]string{
		"the file":           {target, "/api/v1/agents/" + restrictedOwnSession + "/artifacts/nobody-holds-this-png"},
		"his direct message": {"/api/v1/messages/" + direct, "/api/v1/messages/00000000-0000-4000-8000-000000000000"},
	} {
		got, nobody := seed.request(t, callerBob, http.MethodGet, read[0], nil), seed.request(t, callerBob, http.MethodGet, read[1], nil)
		if got.Code != http.StatusNotFound || got.Code != nobody.Code {
			t.Errorf("once carol's write tied the session to carol too, bob reads %s as %d %s, want what one nobody holds answers, %d",
				name, got.Code, got.Body.String(), nobody.Code)
		}
	}
}
