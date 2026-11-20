package api

import (
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// unknownSessionsRoster is four sessions that have written nothing, so none has a
// session_projects pair: session-mixed registered with a token that lists PRIV and PUB,
// session-listed-pub with one that lists PUB, session-empty with one that lists no project ([]),
// and session-unknown, for which the listener sends no projects key, as every listener does until
// it records each session's projects (K6).
const unknownSessionsRoster = `[
	{"session_id":"session-mixed","title":"Mixed","capabilities":["btw"],"projects":["PRIV","PUB"],"last_seen":400},
	{"session_id":"session-listed-pub","title":"On PUB","capabilities":["btw"],"projects":["PUB"],"last_seen":300},
	{"session_id":"session-empty","title":"No list","capabilities":["btw"],"projects":[],"last_seen":200},
	{"session_id":"session-unknown","title":"Unknown","capabilities":["btw"],"last_seen":100}
]`

// TestAnAgentsListHidesASessionWhoseProjectsAreUnknownOrUnseen (B1, M4): while PRIV is private, GET
// /api/v1/agents asks SessionClause about every roster session and nothing else. session-unknown,
// whose listener sent no projects, is one nothing Dispatch holds places in any project, so it
// reaches no caller: a private project's session that writes through a personal token, or a broker
// session before its first write, is such a session. session-mixed, whose list names PRIV and PUB,
// reaches only a caller that sees both: not alice, who sees PUB, nor bob or carol's token that
// lists PRIV, who see PRIV alone. session-empty, whose token lists no project, reaches each open
// viewer (K6). With no private project every open viewer lists all four, as before (R9), and a
// viewer that is not open lists a session by its history alone, none of these.
//
// Controls: the migration's function reading a missing projects key as [] (B1), under which alice
// lists session-unknown; and visibleRosterSessions passing a session whose list meets what the
// caller sees without asking SessionClause (M4), under which alice and bob list session-mixed.
func TestAnAgentsListHidesASessionWhoseProjectsAreUnknownOrUnseen(t *testing.T) {
	listener, _ := newBroadcastListener(t, unknownSessionsRoster)
	seed := seedPrivateProjectWith(t, testServerOptions{envoyURL: listener.URL})
	every := []string{"session-mixed", "session-listed-pub", "session-empty", "session-unknown"}
	ofPublic := []string{"session-listed-pub", "session-empty"}
	list := func(caller privateCaller) []string {
		t.Helper()
		got := []string{}
		for _, row := range decodeBody[[]struct {
			SessionID string `json:"session_id"`
		}](t, seed.mustRequest(t, caller, http.MethodGet, "/api/v1/agents", nil, http.StatusOK)) {
			got = append(got, row.SessionID)
		}
		return got
	}
	for _, test := range []struct {
		caller  privateCaller
		private []string
	}{
		{callerAlice, ofPublic},
		{callerCarol, []string{"session-mixed", "session-listed-pub", "session-empty"}},
		{callerCarolToken, ofPublic},
		{callerSharedToken, ofPublic},
		{callerBob, []string{}},
		{callerCarolPrivToken, []string{}},
		{callerAlicePrivToken, []string{}},
	} {
		if got := list(test.caller); !slices.Equal(got, test.private) {
			t.Errorf("with PRIV private: %s lists %v, want %v", test.caller, got, test.private)
		}
	}
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	for _, test := range []struct {
		caller privateCaller
		public []string
	}{
		{callerAlice, every},
		{callerCarol, every},
		{callerCarolToken, every},
		{callerSharedToken, every},
		{callerBob, []string{}},
		{callerCarolPrivToken, []string{}},
		{callerAlicePrivToken, []string{}},
	} {
		if got := list(test.caller); !slices.Equal(got, test.public) {
			t.Errorf("with no private project: %s lists %v, want %v (R9)", test.caller, got, test.public)
		}
	}
}
