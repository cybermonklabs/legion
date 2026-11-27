package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
)

// The sessions of the reached-project rows, both of the person who loses PRIV in each row.
const (
	// Its listener lists PUB, as a listener does once the session's token was traded again
	// without PRIV.
	reachedListedSession = "2c9b4d2f-6a3e-4b8c-8d4f-2e3a4b5c6d81"
	// Its listener sent no list, as every listener does today.
	reachedUnlistedSession = "2c9b4d2f-6a3e-4b8c-8d4f-2e3a4b5c6d82"
	// The PUB-made-private row's third session: its listener lists CORE, and no person is tied to
	// it.
	reachedUntiedSession = "2c9b4d2f-6a3e-4b8c-8d4f-2e3a4b5c6d83"
	// A session of carol's, PUB's owner, under a broker token that lists CORE alone, which the
	// PUB-made-private row reads PUB's issue as.
	reachedCarolListedSession = "2c9b4d2f-6a3e-4b8c-8d4f-2e3a4b5c6d84"
)

// listedNobody is an issue key no row holds, which a hidden issue is read beside.
const listedNobody = "NOPE-404"

// reachedCore is the public project the PUB-made-private row's sessions write in.
const reachedCore = "CORE"

// newReachedRig is a private seed whose listener holds the two reached sessions, each holding a
// replay of PRIV's issue's text.
func newReachedRig(t *testing.T) contentAgentRig {
	t.Helper()
	return newReachedRigOf(t, `[
		{"session_id":"`+reachedListedSession+`","title":"Lists PUB","capabilities":["btw"],"last_seen":900,"projects":["PUB"]},
		{"session_id":"`+reachedUnlistedSession+`","title":"No list","capabilities":["btw"],"last_seen":800}
	]`, reachedListedSession, reachedUnlistedSession)
}

// mustAs makes one browser request as login, failing the test unless the answer is status.
func (rig contentAgentRig) mustAs(t *testing.T, login, method, target string, body any, status int) {
	t.Helper()
	if response := dispatchRequest(t, rig.seed.handler, method, target, body, login); response.Code != status {
		t.Fatalf("%s %s as %s = %d %s, want %d", method, target, login, response.Code, response.Body.String(), status)
	}
}

// readAndWrite has each reached session of person comment on PUB's issue, under a token naming
// PUB, which ties the session to person and records the pair PUB. What the session read of PRIV
// is in its transcript (reachedReplay) and Dispatch holds no record of it: a read of PRIV through
// Dispatch would be under a token listing PRIV, which records PRIV as reached at once
// (access.RecordListedReach), and the row would prove nothing of the change.
func (rig contentAgentRig) readAndWrite(t *testing.T, person string) {
	t.Helper()
	for _, session := range []string{reachedListedSession, reachedUnlistedSession} {
		rig.seed.asSessionOf(t, person, session, seedPublic, http.MethodPost, "/api/v1/issues/"+rig.seed.issue[seedPublic]+"/comments",
			map[string]any{"body": "Done on PUB."}, http.StatusCreated)
	}
}

// hidden holds caller's view of session to the no-responder verdict and nothing behind it. A view
// that answers otherwise is read on to the replay it relays, which the failure names.
func (rig contentAgentRig) hidden(t *testing.T, state string, caller privateCaller, session string) {
	t.Helper()
	watches := rig.stream.Watches(session)
	client, got := agentVerdict(t, rig.seed, rig.url, caller, session)
	if got != agentViewHidden {
		relayed := "nothing"
		if got == agentViewResponding {
			replay, _ := json.Marshal(client.next(t, string(caller)+"'s replay of "+session))
			relayed = string(replay)
		}
		t.Errorf("%s, %s's agent view of %s answers %s and relays %s, want the no-responder verdict", state, caller, session, got, relayed)
		return
	}
	if viewers, told := rig.stream.Viewers(session), rig.stream.Watches(session)-watches; viewers != 0 || told != 0 {
		t.Errorf("%s, %s's hidden view of %s left %d viewers subscribed and told the session %d times a viewer is attached, want none",
			state, caller, session, viewers, told)
	}
}

// reachedGroupOnPRIV makes the group that admits gina, a full member, and names it on PRIV's list.
func reachedGroupOnPRIV(t *testing.T, rig contentAgentRig) {
	t.Helper()
	rig.seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/access-groups/"+reachedGroup,
		map[string]any{"members": []string{reachedGina}}, http.StatusOK)
	rig.seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/projects/"+seedPrivate+"/access/groups/"+reachedGroup, nil, http.StatusOK)
}

// TestANarrowedListenerListKeepsTheSessionsPastReach: a listener record that lists a private
// project for a session records PRIV as a project the session reached (session_reached_projects),
// in the roster write's own transaction (route_roster's trigger), and nothing removes it; so a list
// narrowed later, as a token traded again without PRIV narrows it, keeps what the session could
// read before. carol's session is listed with PRIV, then listed with PUB alone: alice, an open viewer
// PRIV's list does not name, gets the no-responder verdict for it, and the relay neither subscribes
// to it nor asks it for a replay; carol, who sees PRIV, still watches it.
//
// Control: the roster trigger writing no reached pair; alice is relayed the session's replay,
// which holds PRIV's issue's text, and the test fails.
func TestANarrowedListenerListKeepsTheSessionsPastReach(t *testing.T) {
	listed := func(projects string) string {
		return `[{"session_id":"` + reachedListedSession + `","title":"Narrowed","capabilities":["btw"],"last_seen":900,"projects":` + projects + `}]`
	}
	listener, _ := newBroadcastListener(t, listed(`["PUB"]`))
	stream := agentstream.NewMemory()
	stream.SetReplay(reachedListedSession, reachedReplay(reachedListedSession))
	seed := seedPrivateProjectWith(t, testServerOptions{envoyURL: listener.URL, agentStream: stream})
	rig := contentAgentRig{seed: seed, url: liveURLOf(t, seed.handler), stream: stream}
	seed.asSessionOf(t, seedCarol, reachedListedSession, seedPublic, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPublic]+"/comments",
		map[string]any{"body": "Done on PUB."}, http.StatusCreated)
	for _, roster := range []string{listed(`["PRIV"]`), listed(`["PUB"]`)} {
		if _, err := seed.database.Pool.Exec(context.Background(), `
			insert into route_roster (answered, sessions, sent_at, expires_at)
			values (true, $1::jsonb, now(), now() + interval '1 hour')
			on conflict (id) do update set sessions = excluded.sessions, sent_at = excluded.sent_at, expires_at = excluded.expires_at`,
			roster); err != nil {
			t.Fatalf("store the listener's sessions %s: %v", roster, err)
		}
	}
	const state = "listed with PRIV, then with PUB alone"
	rig.hidden(t, state, callerAlice, reachedListedSession)
	rig.watch(t, state, contentView{callerCarol, reachedListedSession, agentViewResponding, "carol sees PRIV"})
}
