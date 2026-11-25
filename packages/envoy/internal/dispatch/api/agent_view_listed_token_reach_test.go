package api

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken"
)

// asPersonSessionOf makes one request as session, an agent of person, under a broker-issued token
// that acts as its person and lists projects (none for nil), presented under DPoP with a fresh
// proof of the seed's agent key, and fails the test unless the answer is status. Such a token reads
// what its person sees, held to the private projects it lists (access.CredentialCovers).
func (s privateSeed) asPersonSessionOf(t *testing.T, person, session string, projects []string, method, target string, body any, status int) *httptest.ResponseRecorder {
	t.Helper()
	token := "broker-token-of-" + session + "-for-" + person + "-as-person-listing-" + strings.Join(projects, "-")
	s.broker.Says(token, ciphertoken.Introspection{
		Active: true, Session: session, Person: person,
		ProjectScope: ciphertoken.ProjectScopePerson, Projects: projects, Scope: "dispatch.write",
		ExpiresAt: time.Now().Add(time.Hour).Unix(), ClientID: ciphertoken.ExchangeClientID, Thumbprint: s.agent.jkt,
	})
	response := httptest.NewRecorder()
	s.handler.ServeHTTP(response, s.agent.presenting(t, method, target, token, body))
	if response.Code != status {
		t.Fatalf("session %s of %s listing %v: %s %s = %d %s, want %d", session, person, projects, method, target, response.Code, response.Body.String(), status)
	}
	return response
}

// listedTokenSession is carol's session whose token is traded to list PRIV for one request while
// its listener lists PUB alone.
const listedTokenSession = "5f2e7a5c-9d6b-4ebf-8a7c-5b6c7d8e9fa1"

// TestATokensListedPrivateProjectStaysOnItsSession: the listener's record of a session's list is a
// sample, so a token traded to a wider list and back between two samples reads under the wider
// list with no record of it saying so. So the tie Dispatch writes when it verifies a broker-issued
// token also records each private project the token lists as one the session reached
// (access.RecordListedReach). carol's session is listed [PUB]; one request of its, under a token
// acting as carol and listing PUB and PRIV, reads PRIV's issue. Its listener still lists [PUB]. alice,
// an open viewer PRIV's list does not name, gets the no-responder verdict for it, with no subscribe
// and no replay; carol, who sees PRIV, still watches it.
//
// Control: the tie recording no listed project; alice is relayed the session, whose list holds
// only PUB, which she sees.
func TestATokensListedPrivateProjectStaysOnItsSession(t *testing.T) {
	t.Parallel()
	rig := newReachedRigOf(t, `[
		{"session_id":"`+listedTokenSession+`","title":"Lists PUB","capabilities":["btw"],"last_seen":900,"projects":["PUB"]}
	]`, listedTokenSession)
	rig.seed.asPersonSessionOf(t, seedCarol, listedTokenSession, []string{seedPrivate, seedPublic}, http.MethodGet,
		"/api/v1/issues/"+rig.seed.issue[seedPrivate], nil, http.StatusOK)
	const state = "after one request under a token that listed PRIV"
	rig.hidden(t, state, callerAlice, listedTokenSession)
	rig.watch(t, state, contentView{callerCarol, listedTokenSession, agentViewResponding, "carol sees PRIV"})
}

// listedThenPrivateSession is carol's session whose token lists PRIV while PRIV is public, and
// which no listener lists.
const listedThenPrivateSession = "5f2e7a5c-9d6b-4ebf-8a7c-5b6c7d8e9fa2"

// TestAListedProjectMadePrivateIsRecordedOnTheTokensNextRequest: access.RecordListedReach records
// only the listed projects that are private at the request, and a switch to private records the
// project only for a session whose stored listener list names it. So a token that listed PRIV
// while it was public records PRIV as reached on its first request after the switch, whatever this
// process holds of the session's tie (tieBrokerSession). carol's session reads PRIV's issue under a
// token acting as carol and listing PRIV while PRIV is public, which records nothing; carol makes
// PRIV private, which records nothing against the session either; the same token reads PRIV's issue
// again on the same server, and the session holds PRIV as a project it reached.
//
// Control: the tie's cache holding the token's list too (tieBrokerSession skipping
// access.RecordListedReach for a list it recorded already), and the second request records nothing.
func TestAListedProjectMadePrivateIsRecordedOnTheTokensNextRequest(t *testing.T) {
	t.Parallel()
	seed := seedPublicProjectsWith(t, testServerOptions{busProjects: acceptingBus{}})
	reached := func() []string {
		t.Helper()
		var projects []string
		if err := seed.database.Pool.QueryRow(t.Context(), `
			select array(select project_key from session_reached_projects where session_id = $1 order by project_key collate "C")`,
			listedThenPrivateSession).Scan(&projects); err != nil {
			t.Fatalf("read the projects session %s reached: %v", listedThenPrivateSession, err)
		}
		return projects
	}
	readsPRIV := func() {
		t.Helper()
		seed.asPersonSessionOf(t, seedCarol, listedThenPrivateSession, []string{seedPrivate}, http.MethodGet,
			"/api/v1/issues/"+seed.issue[seedPrivate], nil, http.StatusOK)
	}
	readsPRIV()
	if got := reached(); len(got) != 0 {
		t.Fatalf("PRIV public, the session reached %v after its token listing PRIV read, want none: the row proves nothing", got)
	}
	seed.makePrivate(t, seedPrivate)
	if got := reached(); len(got) != 0 {
		t.Fatalf("PRIV made private, the switch recorded %v against the session, want none: the row proves nothing", got)
	}
	readsPRIV()
	if got, want := reached(), []string{seedPrivate}; !slices.Equal(got, want) {
		t.Errorf("PRIV made private, the session reached %v after its token listing PRIV read again, want %v", got, want)
	}
}
