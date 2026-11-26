package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The sessions of the agent-view content rows, by the uuids an agent view names. Each acts under a
// broker-issued token that proves it (asSessionOf): carol is a full member PRIV's list names, bob a
// restricted member PRIV's list names, and dora (contentDora) a full member PUB's list names and no
// private project's.
const (
	// carol's pod session, its token naming PRIV: it comments on PRIV's issue and reads PUB's, a
	// read that records nothing. Its listener lists PRIV.
	contentCarolPodSession = "1c9b4d2f-6a3e-4b8c-8d4f-2e3a4b5c6d71"
	// bob's, its token naming PRIV: it comments on PRIV's issue. Its listener lists PRIV.
	contentBobSession = "1c9b4d2f-6a3e-4b8c-8d4f-2e3a4b5c6d72"
	// bob's, as above, but its listener sent no list.
	contentBobUnlistedSession = "1c9b4d2f-6a3e-4b8c-8d4f-2e3a4b5c6d73"
	// proved by bob's token and by carol's: each comments on PRIV's issue. Its listener lists PRIV.
	contentSharedSession = "1c9b4d2f-6a3e-4b8c-8d4f-2e3a4b5c6d74"
	// carol's, its token naming PUB: it comments on PUB's issue. Its listener lists PUB.
	contentPubListedSession = "1c9b4d2f-6a3e-4b8c-8d4f-2e3a4b5c6d75"
	// carol's, its token naming PUB: it comments on PUB's issue. Its listener sent no list.
	contentPubUnlistedSession = "1c9b4d2f-6a3e-4b8c-8d4f-2e3a4b5c6d76"
	// carol's, its token naming PUB: it comments on PUB's issue. Its listener lists PUB and PRIV.
	contentBothListedSession = "1c9b4d2f-6a3e-4b8c-8d4f-2e3a4b5c6d77"
	// dora's, its token naming PUB: it comments on PUB's issue. Its listener sent no list.
	contentDoraUnlistedSession = "1c9b4d2f-6a3e-4b8c-8d4f-2e3a4b5c6d78"
	// nobody's: a session with history in PUB (a session_projects pair written as a stand-in, as no
	// proved write made it) that no proved write tied to a person. Its listener sent no list.
	contentUntiedSession = "1c9b4d2f-6a3e-4b8c-8d4f-2e3a4b5c6d79"
)

// contentDora is the full member PUB's access list names, and no private project's.
const contentDora = "dora@example.com"

// contentAgentRig is a private seed whose listener lists the nine sessions, with the lists their
// constants name, and whose agent stream holds a replay for each, which it also answers for.
type contentAgentRig struct {
	seed   privateSeed
	url    string
	stream *agentstream.Memory
}

func newContentAgentRig(t *testing.T, options testServerOptions) contentAgentRig {
	t.Helper()
	return newContentAgentRigWith(t, options, nil)
}

// newContentAgentRigWith is newContentAgentRig whose server reads its agent stream through wrap,
// given the rig's in-memory stream; nil reads the stream itself.
func newContentAgentRigWith(t *testing.T, options testServerOptions, wrap func(*agentstream.Memory) agentstream.Source) contentAgentRig {
	t.Helper()
	roster := `[
		{"session_id":"` + contentCarolPodSession + `","title":"Carol's pod","capabilities":["btw"],"last_seen":900,"projects":["PRIV"]},
		{"session_id":"` + contentBobSession + `","title":"Bob's","capabilities":["btw"],"last_seen":800,"projects":["PRIV"]},
		{"session_id":"` + contentBobUnlistedSession + `","title":"Bob's, no list","capabilities":["btw"],"last_seen":700},
		{"session_id":"` + contentSharedSession + `","title":"Shared","capabilities":["btw"],"last_seen":600,"projects":["PRIV"]},
		{"session_id":"` + contentPubListedSession + `","title":"PUB, listed","capabilities":["btw"],"last_seen":500,"projects":["PUB"]},
		{"session_id":"` + contentPubUnlistedSession + `","title":"PUB, no list","capabilities":["btw"],"last_seen":400},
		{"session_id":"` + contentBothListedSession + `","title":"Lists both","capabilities":["btw"],"last_seen":300,"projects":["PUB","PRIV"]},
		{"session_id":"` + contentDoraUnlistedSession + `","title":"Dora's, no list","capabilities":["btw"],"last_seen":200},
		{"session_id":"` + contentUntiedSession + `","title":"Nobody's, no list","capabilities":["btw"],"last_seen":100}
	]`
	listener, _ := newBroadcastListener(t, roster)
	stream := agentstream.NewMemory()
	for _, session := range []string{contentCarolPodSession, contentBobSession, contentBobUnlistedSession, contentSharedSession,
		contentPubListedSession, contentPubUnlistedSession, contentBothListedSession, contentDoraUnlistedSession, contentUntiedSession} {
		stream.SetReplay(session, agentstream.Frame(`{"v":1,"kind":"replay","session":"`+session+`"}`))
	}
	options.envoyURL, options.agentStream = listener.URL, stream
	if wrap != nil {
		options.agentStream = wrap(stream)
	}
	seed := seedPrivateProjectWith(t, options)
	seedPeople(t, seed.database, contentDora)
	seed.addPrincipal(t, seedPublic, access.PrincipalPerson, contentDora)
	for _, acted := range []struct{ person, session, project string }{
		{seedCarol, contentCarolPodSession, seedPrivate},
		{seedBob, contentBobSession, seedPrivate},
		{seedBob, contentBobUnlistedSession, seedPrivate},
		{seedBob, contentSharedSession, seedPrivate},
		{seedCarol, contentSharedSession, seedPrivate},
		{seedCarol, contentPubListedSession, seedPublic},
		{seedCarol, contentPubUnlistedSession, seedPublic},
		{seedCarol, contentBothListedSession, seedPublic},
		{contentDora, contentDoraUnlistedSession, seedPublic},
	} {
		seed.asSessionOf(t, acted.person, acted.session, acted.project, http.MethodPost,
			"/api/v1/issues/"+seed.issue[acted.project]+"/comments",
			map[string]any{"body": "Done on " + acted.project + "."}, http.StatusCreated)
	}
	// carol's pod session reads PUB's issue, which its full member's credential reads whatever its
	// token names: the read records nothing, so no project the session wrote in says it.
	seed.asSessionOf(t, seedCarol, contentCarolPodSession, seedPrivate, http.MethodGet,
		"/api/v1/issues/"+seed.issue[seedPublic], nil, http.StatusOK)
	// The listener's records, as the stored roster's projection holds them, and the untied
	// session's history in PUB.
	if _, err := seed.database.Pool.Exec(context.Background(), `
		insert into route_roster (answered, sessions, sent_at, expires_at)
		values (true, $1::jsonb, now(), now() + interval '1 hour')
		on conflict (id) do update set sessions = excluded.sessions, sent_at = excluded.sent_at, expires_at = excluded.expires_at`,
		roster); err != nil {
		t.Fatalf("store the listener's sessions: %v", err)
	}
	if _, err := seed.database.Pool.Exec(context.Background(), `insert into session_projects (session_id, project_key) values ($1, $2)`,
		contentUntiedSession, seedPublic); err != nil {
		t.Fatalf("record the untied session's history in PUB: %v", err)
	}
	return contentAgentRig{seed: seed, url: liveURLOf(t, seed.handler), stream: stream}
}

// agentVerdict is caller's first frame of its agent view of session: the responder verdict's data.
func agentVerdict(t *testing.T, seed privateSeed, url string, caller privateCaller, session string) (*socketClient, string) {
	t.Helper()
	client := seed.dialLive(t, url, seed.liveHeader(t, caller))
	client.send(t, liveSubscribe("v", agentView(session), ""))
	frame := client.next(t, string(caller)+"'s agent view of "+session)
	if agent, ok := frame.(LiveAgentFrame); ok {
		return client, string(agent.Data)
	}
	got, _ := json.Marshal(frame)
	return client, string(got)
}

// The two responder verdicts an agent view opens with.
const (
	agentViewHidden     = `{"v":1,"responding":false}`
	agentViewResponding = `{"v":1,"responding":true}`
)

// contentView is one agent view a row opens, and the verdict it wants.
type contentView struct {
	caller  privateCaller
	session string
	want    string
	why     string
}

// watch opens v and holds it to its verdict. A hidden view is the no-responder verdict and nothing
// behind it: the relay never subscribed to the session for it (no viewer of it beyond those views
// opened before it left) and never told it a viewer is attached, the call that asks it for its
// replay (no watch). A view that responds is the verdict, then the session's replay, then a frame
// the session publishes.
func (rig contentAgentRig) watch(t *testing.T, state string, v contentView) {
	t.Helper()
	watches, viewers := rig.stream.Watches(v.session), rig.stream.Viewers(v.session)
	client, got := agentVerdict(t, rig.seed, rig.url, v.caller, v.session)
	if got != v.want {
		t.Errorf("%s, %s's agent view of %s (%s) answers %s, want %s", state, v.caller, v.session, v.why, got, v.want)
		return
	}
	if v.want == agentViewHidden {
		if added, told := rig.stream.Viewers(v.session)-viewers, rig.stream.Watches(v.session)-watches; added != 0 || told != 0 {
			t.Errorf("%s, %s's hidden view of %s (%s) subscribed %d viewers and told the session %d times a viewer is attached, want none",
				state, v.caller, v.session, v.why, added, told)
		}
		return
	}
	replay := client.next(t, string(v.caller)+"'s replay of "+v.session)
	if frame, ok := replay.(LiveAgentFrame); !ok || frame.Event != agentEventReplay || !strings.Contains(string(frame.Data), v.session) {
		got, _ := json.Marshal(replay)
		t.Errorf("%s, %s's agent view of %s sends %s after its verdict, want the session's replay", state, v.caller, v.session, got)
		return
	}
	live := `{"v":1,"kind":"turn","session":"` + v.session + `","caller":"` + string(v.caller) + `"}`
	rig.stream.Publish(v.session, agentstream.Frame(live))
	if frame, ok := client.next(t, string(v.caller)+"'s live frame of "+v.session).(LiveAgentFrame); !ok || string(frame.Data) != live {
		t.Errorf("%s, %s's agent view of %s relays no frame the session published", state, v.caller, v.session)
	}
}

// TestARestrictedViewersOwnSessionEndsOnceAnotherPersonActsInIt: bob, a restricted member, watches
// his own session whose listener sent no list. carol's credential then writes as that session,
// which ties it to carol too (session_owners), and bob's view ends (dropped) before the frame the
// session publishes next: the rule is asked again before each batch of frames. The re-arm and the
// heartbeat are set to an hour, so only the write can end the view.
//
// Control: the rule asked only on the re-arm, the heartbeat and access changes, not before each
// batch; the frame reaches bob, and the test fails.
func TestARestrictedViewersOwnSessionEndsOnceAnotherPersonActsInIt(t *testing.T) {
	t.Parallel()
	rig := newContentAgentRig(t, testServerOptions{agentWatchInterval: time.Hour, streamHeartbeat: time.Hour})
	client, got := agentVerdict(t, rig.seed, rig.url, callerBob, contentBobUnlistedSession)
	if got != agentViewResponding {
		t.Fatalf("bob's agent view of his own unlisted session answers %s, want its responder: the row proves nothing", got)
	}
	_ = client.next(t, "bob's replay of his own session")
	rig.seed.asSessionOf(t, seedCarol, contentBobUnlistedSession, seedPublic, http.MethodPost,
		"/api/v1/issues/"+rig.seed.issue[seedPublic]+"/comments", map[string]any{"body": "carol, as bob's session."}, http.StatusCreated)
	rig.stream.Publish(contentBobUnlistedSession, agentstream.Frame(`{"v":1,"kind":"turn","after":"carol's write"}`))
	frame := client.next(t, "bob's agent view after carol's write as his session")
	if ended, ok := frame.(LiveEnded); !ok || ended.Reason != liveEndedDropped {
		sent, _ := json.Marshal(frame)
		t.Errorf("bob's agent view of his own session after carol's credential wrote as it sends %s, want it ended (dropped) before any frame", sent)
	}
}

// TestAnOpenViewerWatchesAnUnlistedSessionOnlyWhereItSeesWhatItsPeopleSee: while PRIV is private,
// an open viewer is relayed a session whose listener lists projects only when it sees each one,
// and a session whose listener sent no list (every listener today) only when it sees every private
// project of each person whose credential proved the session (session_owners), so:
//
//   - alice, an open viewer PRIV's list does not name, is relayed dora's session with no list, since
//     dora is on no private project's list, and the session that lists PUB; she is not relayed
//     carol's session with no list (carol sees PRIV), the session that lists PUB and PRIV, carol's
//     pod session, which lists PRIV, or the session with no list that no proved write tied to
//     anybody;
//   - carol, and carol's token that lists PRIV, are relayed carol's session with no list, since
//     they see PRIV, and not the untied one.
//
// With no project private alice is relayed carol's session with no list, as before (R9).
//
// Controls: the person-tie test dropped (a session with no list passes every open viewer), under
// which alice is relayed carol's session with no list; the tie itself not required, under which
// alice is relayed the untied session. Each fails the test.
func TestAnOpenViewerWatchesAnUnlistedSessionOnlyWhereItSeesWhatItsPeopleSee(t *testing.T) {
	t.Parallel()
	rig := newContentAgentRig(t, testServerOptions{})
	for _, v := range []contentView{
		{callerAlice, contentPubUnlistedSession, agentViewHidden, "carol's, no list, and carol sees PRIV"},
		{callerAlice, contentUntiedSession, agentViewHidden, "no list, tied to nobody"},
		{callerAlice, contentBothListedSession, agentViewHidden, "lists PUB and PRIV"},
		{callerAlice, contentCarolPodSession, agentViewHidden, "lists PRIV"},
		{callerCarol, contentUntiedSession, agentViewHidden, "no list, tied to nobody"},
		{callerAlice, contentDoraUnlistedSession, agentViewResponding, "dora's, no list, and dora is on no private project"},
		{callerAlice, contentPubListedSession, agentViewResponding, "lists PUB"},
		{callerCarol, contentPubUnlistedSession, agentViewResponding, "carol's, no list, and carol sees PRIV"},
		{callerCarolPrivToken, contentPubUnlistedSession, agentViewResponding, "carol's, no list, and the token sees PRIV"},
		{callerCarol, contentCarolPodSession, agentViewResponding, "lists PRIV, which carol sees"},
	} {
		rig.watch(t, "while PRIV is private", v)
	}
	rig.seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	rig.watch(t, "with every project public", contentView{callerAlice, contentPubUnlistedSession, agentViewResponding, "alice is open and nothing is private"})
}

// TestAnAccessChangeEndsAnAgentViewItsViewerNoLongerReads: bob, a restricted member PRIV's list
// names, watches his own session, which wrote in PRIV and lists PRIV. carol takes bob off PRIV's
// list, and his view ends (dropped) on that access change, well inside the re-arm
// (agentWatchInterval) and the heartbeat, both set far past the wait here.
//
// Control: the view asking the rule again on its tickers alone, not on the access change; the view
// is still relaying at the end of the wait, and the test fails.
func TestAnAccessChangeEndsAnAgentViewItsViewerNoLongerReads(t *testing.T) {
	t.Parallel()
	rig := newContentAgentRig(t, testServerOptions{agentWatchInterval: time.Hour, streamHeartbeat: time.Hour})
	client, got := agentVerdict(t, rig.seed, rig.url, callerBob, contentBobSession)
	if got != agentViewResponding {
		t.Fatalf("bob's agent view of his own PRIV session answers %s, want its responder", got)
	}
	// The replay the view sends after the verdict.
	_ = client.next(t, "bob's agent view's replay")
	rig.seed.removePrincipal(t, seedPrivate, access.PrincipalPerson, seedBob)
	frame := client.next(t, "bob's agent view after he left PRIV's list")
	if ended, ok := frame.(LiveEnded); !ok || ended.Reason != liveEndedDropped {
		got, _ := json.Marshal(frame)
		t.Errorf("bob's agent view after he left PRIV's list sends %s, want it ended (dropped)", got)
	}
}

// TestASessionsProvedWritesTieItToItsPerson: a write a session's own credential proved records
// the person that credential acts for (session_owners), as it records the session's project, once
// per pair; a write of a browser, which proves no session, records none.
func TestASessionsProvedWritesTieItToItsPerson(t *testing.T) {
	t.Parallel()
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
	defer rows.Close()
	var got []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatalf("read session_owners: %v", err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read session_owners: %v", err)
	}
	if want := contentBobSession + " " + model.CanonicalLogin(seedBob); len(got) != 1 || got[0] != want {
		t.Errorf("session_owners after two proved writes of bob's session and a browser's write = %q, want [%q]", got, want)
	}
}
