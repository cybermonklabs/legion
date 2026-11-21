package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The sessions of the agent-view content rows, by the uuids an agent view names. Every session but
// contentBobSession is carol's, a full member's.
const (
	// acted in PRIV and PUB; its listener sent no list.
	contentMixedSession = "1c9b4d2f-6a3e-4b8c-8d4f-2e3a4b5c6d71"
	// acted in PUB; its listener lists PUB and PRIV.
	contentListedSession = "1c9b4d2f-6a3e-4b8c-8d4f-2e3a4b5c6d72"
	// acted in PRIV; its listener lists PRIV. A full member's credential reads every public
	// project too, PUB among them.
	contentCarolPrivSession = "1c9b4d2f-6a3e-4b8c-8d4f-2e3a4b5c6d73"
	// acted in PUB; its listener sent no list.
	contentPubNoListSession = "1c9b4d2f-6a3e-4b8c-8d4f-2e3a4b5c6d74"
	// acted in PUB; its listener lists PUB.
	contentPubListedSession = "1c9b4d2f-6a3e-4b8c-8d4f-2e3a4b5c6d75"
	// bob's, a restricted member PRIV's list names: acted in PRIV; its listener lists PRIV.
	contentBobSession = "1c9b4d2f-6a3e-4b8c-8d4f-2e3a4b5c6d76"
)

// contentAgentRig is a private seed whose listener lists the six sessions, with the lists their
// constants name, and whose agent stream holds a replay for each.
func contentAgentRig(t *testing.T, options testServerOptions) (privateSeed, string) {
	t.Helper()
	roster := `[
		{"session_id":"` + contentMixedSession + `","title":"Mixed","capabilities":["btw"],"last_seen":600},
		{"session_id":"` + contentListedSession + `","title":"Listed","capabilities":["btw"],"last_seen":500,"projects":["PUB","PRIV"]},
		{"session_id":"` + contentCarolPrivSession + `","title":"Carol in PRIV","capabilities":["btw"],"last_seen":400,"projects":["PRIV"]},
		{"session_id":"` + contentPubNoListSession + `","title":"PUB, no list","capabilities":["btw"],"last_seen":300},
		{"session_id":"` + contentPubListedSession + `","title":"PUB, listed","capabilities":["btw"],"last_seen":200,"projects":["PUB"]},
		{"session_id":"` + contentBobSession + `","title":"Bob in PRIV","capabilities":["btw"],"last_seen":100,"projects":["PRIV"]}
	]`
	listener, _ := newBroadcastListener(t, roster)
	source := agentstream.NewMemory()
	for _, session := range []string{contentMixedSession, contentListedSession, contentCarolPrivSession,
		contentPubNoListSession, contentPubListedSession, contentBobSession} {
		source.SetReplay(session, agentstream.Frame(`{"v":1,"kind":"replay","session":"`+session+`"}`))
	}
	options.envoyURL, options.agentStream = listener.URL, source
	seed := seedPrivateProjectWith(t, options)
	for _, acted := range []struct{ person, session, project string }{
		{seedCarol, contentMixedSession, seedPrivate}, {seedCarol, contentMixedSession, seedPublic},
		{seedCarol, contentListedSession, seedPublic}, {seedCarol, contentCarolPrivSession, seedPrivate},
		{seedCarol, contentPubNoListSession, seedPublic}, {seedCarol, contentPubListedSession, seedPublic},
		{seedBob, contentBobSession, seedPrivate},
	} {
		seed.asSessionOf(t, acted.person, acted.session, acted.project, http.MethodPost,
			"/api/v1/issues/"+seed.issue[acted.project]+"/comments",
			map[string]any{"body": "Done on " + acted.project + "."}, http.StatusCreated)
	}
	// The listener's records, as the stored roster's projection holds them.
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

// TestTheAgentViewRelaysASessionOnlyToAViewerItsCredentialsReachIsWithin: the live agent view
// relays a session's raw transcript, whose tool results hold whatever the session read, and a read
// records nothing. So the view is held to what the session's credential can read
// (access.SessionContentClause): its listener's list of the credential's projects, every project
// the session wrote in, and, for a restricted viewer, that the session acts for restricted members
// alone, since a full member's credential reads every public project whatever it lists.
//
//   - With every project public, bob, a restricted member PRIV's list names, watching carol's
//     session that wrote in PRIV and PUB, and carol's session that wrote in PRIV alone and lists
//     PRIV (it may have read PUB, which bob does not see), answers the no-responder verdict, as a
//     session nobody answers for. His own session, which lists PRIV, relays. carol, open, is
//     relayed every session.
//   - While PRIV is private, alice, an open viewer PRIV's list does not name, watching a session
//     whose listener lists PUB and PRIV, and one that wrote in PUB whose listener sent no list,
//     answers the no-responder verdict; a session that wrote in PUB and lists PUB relays. carol is
//     relayed the one that lists PRIV.
//
// Controls: the restricted-owner condition dropped (a restricted viewer judged by the list alone),
// under which bob is relayed carol's PRIV session; a missing list read as an empty one, under which
// alice is relayed the session with no list. Each fails the row.
func TestTheAgentViewRelaysASessionOnlyToAViewerItsCredentialsReachIsWithin(t *testing.T) {
	seed, url := contentAgentRig(t, testServerOptions{})
	const hidden, responding = `{"v":1,"responding":false}`, `{"v":1,"responding":true}`
	type view struct {
		caller  privateCaller
		session string
		want    string
		why     string
	}
	check := func(state string, views []view) {
		t.Helper()
		for _, v := range views {
			if _, got := agentVerdict(t, seed, url, v.caller, v.session); got != v.want {
				t.Errorf("%s, %s's agent view of %s (%s) answers %s, want %s", state, v.caller, v.session, v.why, got, v.want)
			}
		}
	}

	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	check("with every project public", []view{
		{callerBob, contentMixedSession, hidden, "carol's, wrote in PRIV and PUB"},
		{callerBob, contentCarolPrivSession, hidden, "carol's, wrote in PRIV, lists PRIV, reads every public project"},
		{callerBob, contentBobSession, responding, "bob's own person's, lists PRIV"},
		{callerCarol, contentMixedSession, responding, "carol is open and nothing is private"},
	})

	seed.makePrivate(t, seedPrivate)
	check("while PRIV is private", []view{
		{callerAlice, contentListedSession, hidden, "lists PRIV"},
		{callerAlice, contentPubNoListSession, hidden, "wrote in PUB, its listener sent no list"},
		{callerAlice, contentPubListedSession, responding, "wrote in PUB, lists PUB"},
		{callerCarol, contentListedSession, responding, "carol sees PUB and PRIV"},
		{callerBob, contentBobSession, responding, "bob's own person's, lists PRIV"},
	})
}

// TestAnAccessChangeEndsAnAgentViewItsViewerNoLongerReads: bob, a restricted member PRIV's list
// names, watches his own session, which wrote in PRIV and lists PRIV. carol takes bob off PRIV's
// list, and his view ends (dropped) on that access change, well inside the re-arm
// (agentWatchInterval) and the heartbeat, both set far past the wait here.
//
// Control: the view asking the rule again on its tickers alone, not on the access change; the view
// is still relaying at the end of the wait, and the test fails.
func TestAnAccessChangeEndsAnAgentViewItsViewerNoLongerReads(t *testing.T) {
	seed, url := contentAgentRig(t, testServerOptions{agentWatchInterval: time.Hour, streamHeartbeat: time.Hour})
	client, got := agentVerdict(t, seed, url, callerBob, contentBobSession)
	if got != `{"v":1,"responding":true}` {
		t.Fatalf("bob's agent view of his own PRIV session answers %s, want its responder", got)
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

// TestASessionsProvedWritesTieItToItsPerson: a write a session's own credential proved records
// the person that credential acts for (session_owners), as it records the session's project, once
// per pair; a write of a token that proves no session records none.
func TestASessionsProvedWritesTieItToItsPerson(t *testing.T) {
	seed := seedPrivateProject(t)
	for range 2 {
		seed.asSessionOf(t, seedBob, contentBobSession, seedPrivate, http.MethodPost,
			"/api/v1/issues/"+seed.issue[seedPrivate]+"/comments", map[string]any{"body": "Done."}, http.StatusCreated)
	}
	seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPublic]+"/comments",
		map[string]any{"body": "A browser's write."}, http.StatusCreated)
	rows, err := seed.database.Pool.Query(context.Background(), `select session_id || ' ' || login from session_owners order by 1`)
	if err != nil {
		t.Fatalf("read session_owners: %v", err)
	}
	var got []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatalf("read session_owners: %v", err)
		}
		got = append(got, row)
	}
	if want := contentBobSession + " " + strings.ToLower(seedBob); len(got) != 1 || got[0] != want {
		t.Errorf("session_owners after two proved writes of bob's session and a browser's write = %q, want [%q]", got, want)
	}
}
