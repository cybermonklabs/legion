package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The sessions of the agent-view content rows, by the uuids an agent view names.
const (
	contentMixedSession  = "1c9b4d2f-6a3e-4b8c-8d4f-2e3a4b5c6d71"
	contentListedSession = "1c9b4d2f-6a3e-4b8c-8d4f-2e3a4b5c6d72"
	contentPrivSession   = "1c9b4d2f-6a3e-4b8c-8d4f-2e3a4b5c6d73"
)

// contentAgentRig is a private seed whose listener lists the three sessions and whose agent stream
// holds a replay for each: contentMixedSession has acted in PRIV and PUB, contentListedSession in
// PUB alone with a listener record that lists PUB and PRIV, and contentPrivSession in PRIV alone.
func contentAgentRig(t *testing.T, options testServerOptions) (privateSeed, string) {
	t.Helper()
	roster := `[
		{"session_id":"` + contentMixedSession + `","title":"Mixed","capabilities":["btw"],"last_seen":300},
		{"session_id":"` + contentListedSession + `","title":"Listed","capabilities":["btw"],"last_seen":200,"projects":["PUB","PRIV"]},
		{"session_id":"` + contentPrivSession + `","title":"Private","capabilities":["btw"],"last_seen":100}
	]`
	listener, _ := newBroadcastListener(t, roster)
	source := agentstream.NewMemory()
	for _, session := range []string{contentMixedSession, contentListedSession, contentPrivSession} {
		source.SetReplay(session, agentstream.Frame(`{"v":1,"kind":"replay","session":"`+session+`"}`))
	}
	options.envoyURL, options.agentStream = listener.URL, source
	seed := seedPrivateProjectWith(t, options)
	for _, acted := range []struct{ session, project string }{
		{contentMixedSession, seedPrivate}, {contentMixedSession, seedPublic},
		{contentListedSession, seedPublic}, {contentPrivSession, seedPrivate},
	} {
		seed.asSession(t, acted.session, acted.project, http.MethodPost, "/api/v1/issues/"+seed.issue[acted.project]+"/comments",
			map[string]any{"body": "Done on " + acted.project + "."}, http.StatusCreated)
	}
	// The listener record of contentListedSession, as the stored roster's projection holds it.
	if _, err := seed.database.Pool.Exec(context.Background(), `
		insert into route_roster (answered, sessions, sent_at, expires_at)
		values (true, $1::jsonb, now(), now() + interval '1 hour')
		on conflict (id) do update set sessions = excluded.sessions, sent_at = excluded.sent_at, expires_at = excluded.expires_at`,
		roster); err != nil {
		t.Fatalf("store the listener's sessions: %v", err)
	}
	return seed, liveURLOf(t, seed.handler)
}

// agentVerdict is caller's first frame of its agent view of session: the responder verdict's data.
func agentVerdict(t *testing.T, seed privateSeed, url string, caller privateCaller, session string) (*socketClient, string) {
	t.Helper()
	client := seed.dialLive(t, url, seed.liveHeader(t, caller))
	client.send(t, liveSubscribe("v", agentView(session), ""))
	frame := client.next(t, eventFeedWait, string(caller)+"'s agent view of "+session)
	if agent, ok := frame.(LiveAgentFrame); ok {
		return client, string(agent.Data)
	}
	got, _ := json.Marshal(frame)
	return client, string(got)
}

// TestTheAgentViewRelaysNoSessionThatWorkedInAProjectItsViewerDoesNotSee: the live agent view
// relays a session's raw transcript, whose tool results hold whatever the session read in any
// project it worked in, so it is held to every project the session acted in and every project its
// listener record lists (access.SessionContentClause), where GET /api/v1/agents/{id}/messages
// filters each message by its own project.
//
//   - With every project public, bob, a restricted member PRIV's list names, watches
//     contentMixedSession, which acted in PRIV and in PUB, a project no list names him on: the view
//     answers the no-responder verdict, as a session nobody answers for. carol, who sees both,
//     is answered its responder.
//   - While PRIV is private, alice, an open viewer PRIV's list does not name, watches
//     contentListedSession, which acted in PUB alone and whose listener record lists PUB and PRIV:
//     the no-responder verdict. carol is answered its responder.
//
// Control: the view held to access.SessionClause (sessionHiddenFrom), under which bob's and
// alice's views relay the session, and the test fails naming each.
func TestTheAgentViewRelaysNoSessionThatWorkedInAProjectItsViewerDoesNotSee(t *testing.T) {
	seed, url := contentAgentRig(t, testServerOptions{})
	const hidden, responding = `{"v":1,"responding":false}`, `{"v":1,"responding":true}`

	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	for _, view := range []struct {
		caller privateCaller
		want   string
	}{{callerBob, hidden}, {callerCarol, responding}} {
		if _, got := agentVerdict(t, seed, url, view.caller, contentMixedSession); got != view.want {
			t.Errorf("with every project public, %s's agent view of a session that acted in PRIV and PUB answers %s, want %s", view.caller, got, view.want)
		}
	}

	seed.makePrivate(t, seedPrivate)
	for _, view := range []struct {
		caller privateCaller
		want   string
	}{{callerAlice, hidden}, {callerCarol, responding}} {
		if _, got := agentVerdict(t, seed, url, view.caller, contentListedSession); got != view.want {
			t.Errorf("while PRIV is private, %s's agent view of a session that acted in PUB and whose record lists PRIV answers %s, want %s", view.caller, got, view.want)
		}
	}
}

// TestAnAccessChangeEndsAnAgentViewItsViewerNoLongerReads: bob, a restricted member PRIV's list
// names, watches contentPrivSession, which acted in PRIV alone. carol takes bob off PRIV's list,
// and his view ends (dropped) on that access change, well inside the re-arm (agentWatchInterval)
// and the heartbeat, both set far past the wait here.
//
// Control: the view asking the rule again on its tickers alone, not on the access change; the view
// is still relaying at the end of the wait, and the test fails.
func TestAnAccessChangeEndsAnAgentViewItsViewerNoLongerReads(t *testing.T) {
	seed, url := contentAgentRig(t, testServerOptions{agentWatchInterval: time.Hour, streamHeartbeat: time.Hour})
	client, got := agentVerdict(t, seed, url, callerBob, contentPrivSession)
	if got != `{"v":1,"responding":true}` {
		t.Fatalf("bob's agent view of PRIV's session answers %s, want its responder", got)
	}
	// The replay the view sends after the verdict.
	_ = client.next(t, eventFeedWait, "bob's agent view's replay")
	seed.mustRequest(t, callerCarol, http.MethodDelete, "/api/v1/projects/"+seedPrivate+"/access/people/"+seedBob, nil, http.StatusOK)
	frame := client.next(t, 5*time.Second, "bob's agent view after he left PRIV's list")
	if ended, ok := frame.(LiveEnded); !ok || ended.Reason != liveEndedDropped {
		got, _ := json.Marshal(frame)
		t.Errorf("bob's agent view after he left PRIV's list sends %s, want it ended (dropped)", got)
	}
}
