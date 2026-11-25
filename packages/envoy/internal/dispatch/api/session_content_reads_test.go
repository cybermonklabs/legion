package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// sessionContentSession is the session of the content rows: carol's, its broker token naming PUB.
// It wrote in PUB alone, and its listener sent no list of its credential's projects.
const sessionContentSession = "5c0e7e17-3a1b-4c2d-9e8f-0a1b2c3d4e51"

// sessionContentRead is one read of a session's content: the target that reads the session's
// object, the same read of an object nobody holds, and a text of the object that no refusal holds.
type sessionContentRead struct {
	name, target, absent, secret string
}

// TestASessionsFilesNodesAndDirectMessagesFollowTheContentRule: a session's files, the graph node
// of each, and its direct messages can hold whatever the session read, so they are its content and
// follow the rule its live agent view keeps (access.SessionContentClause, through
// access.PartyClause), while the agents list, which carries no content, keeps access.SessionClause
// (K6). carol's session wrote in PUB alone, so alice, an open viewer PRIV's list does not name,
// lists it among the agents; but carol proved it and carol sees PRIV, so its transcript, files and
// messages can hold what PRIV holds. alice reads its file, its file's node and carol's direct
// message to it as she reads objects nobody holds (404, the same code), and carol reads all three.
// With no private project alice reads all three, as before (R9).
//
// Control: PartyClause holding a session party to SessionClause alone, as before this rule, and
// alice reads all three while PRIV is private; the test names each.
func TestASessionsFilesNodesAndDirectMessagesFollowTheContentRule(t *testing.T) {
	listener, _ := newBroadcastListener(t, `[
		{"session_id":"`+sessionContentSession+`","title":"carol's PUB session","capabilities":["btw"],"last_seen":100}
	]`)
	seed := seedPrivateProjectWith(t, testServerOptions{envoyURL: listener.URL})
	// The proved write places the session in PUB (session_projects) and ties it to carol
	// (session_owners); a list of the agents stores the listener's list.
	seed.asSessionOf(t, seedCarol, sessionContentSession, seedPublic, http.MethodPost,
		"/api/v1/issues/"+seed.issue[seedPublic]+"/comments", map[string]any{"body": "Done on PUB."}, http.StatusCreated)
	seed.mustRequest(t, callerCarol, http.MethodGet, "/api/v1/agents", nil, http.StatusOK)

	const fileText, directText = "what the session saw", "What did you read today?"
	uploaded := multipartRequest(t, seed.handler, "/api/v1/agents/"+sessionContentSession+"/artifacts",
		map[string]string{"name": "seen.png"}, "seen.png", "image/png", pngBytes(fileText), seedCarol)
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("carol uploads to her session's conversation: %d %s", uploaded.Code, uploaded.Body.String())
	}
	file := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact
	direct := createdID(t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/agents/"+sessionContentSession+"/messages",
		map[string]any{"body": directText, "delivery": "btw"}, http.StatusCreated))
	address := func(slug string) string {
		return "/api/v1/references?" + url.Values{"to": {"dispatch://agent/" + sessionContentSession + "/artifact/" + slug}}.Encode()
	}
	reads := []sessionContentRead{
		{"its file", "/api/v1/agents/" + sessionContentSession + "/artifacts/" + file.Slug,
			"/api/v1/agents/" + sessionContentSession + "/artifacts/nobody-holds-this-png", file.ID},
		{"its file's node", address(file.Slug), address("nobody-holds-this-png"), file.ID},
		{"its direct message", "/api/v1/messages/" + direct, "/api/v1/messages/" + uuid.NewString(), directText},
	}

	listed := false
	for _, agent := range decodeBody[[]struct {
		SessionID string `json:"session_id"`
	}](t, seed.mustRequest(t, callerAlice, http.MethodGet, "/api/v1/agents", nil, http.StatusOK)) {
		listed = listed || agent.SessionID == sessionContentSession
	}
	if !listed {
		t.Fatalf("alice's list of the agents does not hold carol's PUB session: the agents list keeps SessionClause, which passes it")
	}
	for _, read := range reads {
		requireReadAsMissing(t, seed, callerAlice, read)
		if found := seed.request(t, callerCarol, http.MethodGet, read.target, nil); found.Code != http.StatusOK {
			t.Errorf("carol reads %s of her session as %d %s, want 200", read.name, found.Code, found.Body.String())
		}
	}

	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	for _, read := range reads {
		if found := seed.request(t, callerAlice, http.MethodGet, read.target, nil); found.Code != http.StatusOK {
			t.Errorf("with no private project alice reads %s of carol's session as %d %s, want 200 (R9)", read.name, found.Code, found.Body.String())
		}
	}
}

// requireReadAsMissing fails unless caller reads read.target as it reads read.absent - the same
// status and the same error code, a 404 - and the answer holds nothing of the object.
func requireReadAsMissing(t *testing.T, seed privateSeed, caller privateCaller, read sessionContentRead) {
	t.Helper()
	hidden := seed.request(t, caller, http.MethodGet, read.target, nil)
	absent := seed.request(t, caller, http.MethodGet, read.absent, nil)
	code := func(response string) string {
		var body struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal([]byte(response), &body)
		return body.Code
	}
	if hidden.Code != http.StatusNotFound || hidden.Code != absent.Code || code(hidden.Body.String()) != code(absent.Body.String()) {
		t.Errorf("%s reads %s as %d %s; an object nobody holds answers %d %s", caller, read.name,
			hidden.Code, hidden.Body.String(), absent.Code, absent.Body.String())
	}
	if strings.Contains(hidden.Body.String(), read.secret) {
		t.Errorf("%s's refusal of %s holds %q: %s", caller, read.name, read.secret, hidden.Body.String())
	}
}
