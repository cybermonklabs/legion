package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
)

// The two sessions of the reached-project rows, both of the person who loses PRIV in each row.
const (
	// Its listener lists PUB, as a listener does once the session's token was traded again
	// without PRIV.
	reachedListedSession = "2c9b4d2f-6a3e-4b8c-8d4f-2e3a4b5c6d81"
	// Its listener sent no list, as every listener does today.
	reachedUnlistedSession = "2c9b4d2f-6a3e-4b8c-8d4f-2e3a4b5c6d82"
)

// The people of the reached-project rows: each a full member who sees PRIV in one way, and loses it.
const (
	reachedXavier = "xavier@example.com" // on PRIV's list
	reachedGina   = "gina@example.com"   // in a group PRIV's list names
	reachedOwen   = "owen@example.com"   // PRIV's owner for a while
	reachedGroup  = "priv-team"
)

// reachedReplay is the replay each session holds: the text of PRIV's issue, which it read while
// its person saw PRIV.
func reachedReplay(session string) agentstream.Frame {
	return agentstream.Frame(`{"v":1,"kind":"replay","session":"` + session + `","text":"The issue of PRIV"}`)
}

// newReachedRig is a private seed whose listener holds the two reached sessions, each holding a
// replay of PRIV's issue's text.
func newReachedRig(t *testing.T) contentAgentRig {
	t.Helper()
	roster := `[
		{"session_id":"` + reachedListedSession + `","title":"Lists PUB","capabilities":["btw"],"last_seen":900,"projects":["PUB"]},
		{"session_id":"` + reachedUnlistedSession + `","title":"No list","capabilities":["btw"],"last_seen":800}
	]`
	listener, _ := newBroadcastListener(t, roster)
	stream := agentstream.NewMemory()
	for _, session := range []string{reachedListedSession, reachedUnlistedSession} {
		stream.SetReplay(session, reachedReplay(session))
	}
	seed := seedPrivateProjectWith(t, testServerOptions{envoyURL: listener.URL, agentStream: stream})
	seedPeople(t, seed.database, reachedXavier, reachedGina, reachedOwen)
	if _, err := seed.database.Pool.Exec(context.Background(), `
		insert into route_roster (answered, sessions, sent_at, expires_at)
		values (true, $1::jsonb, now(), now() + interval '1 hour')
		on conflict (id) do update set sessions = excluded.sessions, sent_at = excluded.sent_at, expires_at = excluded.expires_at`,
		roster); err != nil {
		t.Fatalf("store the listener's sessions: %v", err)
	}
	return contentAgentRig{seed: seed, url: liveURLOf(t, seed.handler), stream: stream}
}

// mustAs makes one browser request as login, failing the test unless the answer is status.
func (rig contentAgentRig) mustAs(t *testing.T, login, method, target string, body any, status int) {
	t.Helper()
	if response := dispatchRequest(t, rig.seed.handler, method, target, body, login); response.Code != status {
		t.Fatalf("%s %s as %s = %d %s, want %d", method, target, login, response.Code, response.Body.String(), status)
	}
}

// readAndWrite has each reached session of person read PRIV's issue, under a token naming PRIV,
// which records nothing, and comment on PUB's, under a token naming PUB, which records the pair
// PUB and ties the session to person.
func (rig contentAgentRig) readAndWrite(t *testing.T, person string) {
	t.Helper()
	for _, session := range []string{reachedListedSession, reachedUnlistedSession} {
		rig.seed.asSessionOf(t, person, session, seedPrivate, http.MethodGet, "/api/v1/issues/"+rig.seed.issue[seedPrivate], nil, http.StatusOK)
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
			replay, _ := json.Marshal(client.next(t, eventFeedWait, string(caller)+"'s replay of "+session))
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

// TestAPersonsLostProjectStaysOnTheSessionsThatReadIt: the live agent view relays a session's
// transcript, which holds what the session read while its person saw a private project. So an
// access change that takes PRIV from a person records PRIV against every session tied to that
// person (session_reached_projects), in the change's own transaction, and the content rule
// (access.SessionContentClause) treats it as a project the session acted in. In each row a full
// member's two sessions read PRIV's issue and comment on PUB's; then the person loses PRIV:
//
//   - xavier is taken off PRIV's list;
//   - the group that admits gina is taken off PRIV's list;
//   - gina is taken out of that group;
//   - that group is deleted;
//   - owen, PRIV's owner for a while, gives it back to carol.
//
// After the change alice, an open viewer PRIV's list does not name, gets the no-responder verdict
// for both sessions, the one whose listener lists PUB and the one with no list, and the relay
// neither subscribes to them nor asks them for a replay. carol, who sees PRIV, still watches both.
//
// Control: the reached pairs left unwritten; alice is relayed each session's replay, which holds
// PRIV's issue's text, and the test fails.
func TestAPersonsLostProjectStaysOnTheSessionsThatReadIt(t *testing.T) {
	for _, row := range []struct {
		name   string
		person string
		before func(t *testing.T, rig contentAgentRig)
		change func(t *testing.T, rig contentAgentRig)
	}{
		{
			name:   "taken off the list",
			person: reachedXavier,
			before: func(t *testing.T, rig contentAgentRig) {
				rig.seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/projects/"+seedPrivate+"/access/people/"+reachedXavier, nil, http.StatusOK)
			},
			change: func(t *testing.T, rig contentAgentRig) {
				rig.seed.mustRequest(t, callerCarol, http.MethodDelete, "/api/v1/projects/"+seedPrivate+"/access/people/"+reachedXavier, nil, http.StatusOK)
			},
		},
		{
			name:   "group taken off the list",
			person: reachedGina,
			before: reachedGroupOnPRIV,
			change: func(t *testing.T, rig contentAgentRig) {
				rig.seed.mustRequest(t, callerCarol, http.MethodDelete, "/api/v1/projects/"+seedPrivate+"/access/groups/"+reachedGroup, nil, http.StatusOK)
			},
		},
		{
			name:   "taken out of the group",
			person: reachedGina,
			before: reachedGroupOnPRIV,
			change: func(t *testing.T, rig contentAgentRig) {
				rig.seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/access-groups/"+reachedGroup,
					map[string]any{"members": []string{}}, http.StatusOK)
			},
		},
		{
			name:   "group deleted",
			person: reachedGina,
			before: reachedGroupOnPRIV,
			change: func(t *testing.T, rig contentAgentRig) {
				rig.seed.mustRequest(t, callerCarol, http.MethodDelete, "/api/v1/access-groups/"+reachedGroup, nil, http.StatusNoContent)
			},
		},
		{
			name:   "owner changed",
			person: reachedOwen,
			before: func(t *testing.T, rig contentAgentRig) {
				rig.seed.mustRequest(t, callerCarol, http.MethodPatch, "/api/v1/projects/"+seedPrivate+"/access", map[string]any{"owner": reachedOwen}, http.StatusOK)
			},
			change: func(t *testing.T, rig contentAgentRig) {
				rig.mustAs(t, reachedOwen, http.MethodPatch, "/api/v1/projects/"+seedPrivate+"/access", map[string]any{"owner": seedCarol}, http.StatusOK)
			},
		},
	} {
		t.Run(strings.ReplaceAll(row.name, " ", "_"), func(t *testing.T) {
			rig := newReachedRig(t)
			row.before(t, rig)
			rig.readAndWrite(t, row.person)
			row.change(t, rig)
			state := row.person + " " + row.name
			for _, session := range []string{reachedListedSession, reachedUnlistedSession} {
				rig.hidden(t, state, callerAlice, session)
				rig.watch(t, state, contentView{callerCarol, session, agentViewResponding, "carol sees PRIV"})
			}
		})
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
