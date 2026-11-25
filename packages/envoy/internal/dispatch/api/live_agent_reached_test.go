package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
	"github.com/sjawhar/envoy/internal/dispatch/model"
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

// The people of the reached-project rows: each a full member who sees PRIV in one way, and loses it.
const (
	reachedXavier = "xavier@example.com" // on PRIV's list
	reachedGina   = "gina@example.com"   // in a group PRIV's list names
	reachedOwen   = "owen@example.com"   // PRIV's owner for a while
	reachedGroup  = "priv-team"
)

// reachedCore is the public project the PUB-made-private row's sessions write in.
const reachedCore = "CORE"

// reachedReplay is the replay each session holds: the text of PRIV's issue, which it read while
// its person saw PRIV.
func reachedReplay(session string) agentstream.Frame {
	return agentstream.Frame(`{"v":1,"kind":"replay","session":"` + session + `","text":"The issue of PRIV"}`)
}

// newReachedRig is a private seed whose listener holds the two reached sessions, each holding a
// replay of PRIV's issue's text.
func newReachedRig(t *testing.T) contentAgentRig {
	t.Helper()
	return newReachedRigOf(t, `[
		{"session_id":"`+reachedListedSession+`","title":"Lists PUB","capabilities":["btw"],"last_seen":900,"projects":["PUB"]},
		{"session_id":"`+reachedUnlistedSession+`","title":"No list","capabilities":["btw"],"last_seen":800}
	]`, reachedListedSession, reachedUnlistedSession)
}

// newReachedRigOf is a private seed whose listener holds roster, the listener's records, each of
// sessions holding a replay of PRIV's issue's text.
func newReachedRigOf(t *testing.T, roster string, sessions ...string) contentAgentRig {
	t.Helper()
	return newReachedRigWith(t, testServerOptions{}, roster, sessions...)
}

// newReachedRigWith is newReachedRigOf on a test server built from options.
func newReachedRigWith(t *testing.T, options testServerOptions, roster string, sessions ...string) contentAgentRig {
	t.Helper()
	listener, _ := newBroadcastListener(t, roster)
	stream := agentstream.NewMemory()
	for _, session := range sessions {
		stream.SetReplay(session, reachedReplay(session))
	}
	options.envoyURL, options.agentStream = listener.URL, stream
	seed := seedPrivateProjectWith(t, options)
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
// The last two rows switch a public project to private, by the switch route and by an SQL
// statement alone, which takes PUB from every full member its list does not name. A switch hides
// what is written in the project from the switch on and withdraws nothing a session read while it
// was public: the projects table's trigger records the pair only for a session whose stored
// listener list names the project (testPUBMadePrivate).
//
// Control: the reached pairs left unwritten; alice is relayed each session's replay, which holds
// PRIV's issue's text, and the test fails.
func TestAPersonsLostProjectStaysOnTheSessionsThatReadIt(t *testing.T) {
	t.Parallel()
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
	for _, made := range []struct {
		name        string
		makePrivate func(t *testing.T, rig contentAgentRig)
	}{
		{"PUB_made_private_by_the_switch", func(t *testing.T, rig contentAgentRig) {
			rig.seed.setVisibility(t, seedPublic, model.ProjectPrivate)
		}},
		{"PUB_made_private_by_SQL", func(t *testing.T, rig contentAgentRig) {
			if _, err := rig.seed.database.Pool.Exec(t.Context(), `update projects set visibility = 'private' where key = $1`, seedPublic); err != nil {
				t.Fatalf("make PUB private by an SQL statement alone: %v", err)
			}
		}},
	} {
		t.Run(made.name, func(t *testing.T) { testPUBMadePrivate(t, made.makePrivate) })
	}
}

// testPUBMadePrivate is TestAPersonsLostProjectStaysOnTheSessionsThatReadIt's row for a project
// switched from public to private, by switchPUB: the server's switch (access.SetVisibility, the
// write the visibility route makes, committed), or an SQL statement alone. xavier, a full member
// no list names, has two sessions: one whose listener lists CORE, a public project, and one with
// no list. Each reads PUB's issue while PUB is public, which records nothing, and comments on
// CORE's, which ties it to xavier. A third session, listed PUB and CORE, is tied to nobody, its
// history in CORE written as a stand-in, since every proved request ties its session. xavier also
// uploads a file to his unlisted session's conversation and sends it a direct message, both of
// which alice, an open viewer, reads while PUB is public. Then carol, PUB's owner, makes PUB
// private with a list naming nobody, so neither xavier nor alice sees it, and the third session's
// listener lists CORE alone afterwards, as a token traded again without PUB lists it.
//
// A switch to private withdraws nothing xavier's sessions read while PUB was public, to every full
// member: the switch records no pair against a session for the people tied to it, so xavier reads
// his own session's file and direct message, and alice watches both his sessions and reads both
// too. The third session's list named PUB at the switch, so its credential reads PUB from the
// switch on: the switch records PUB against it, and alice gets the no-responder verdict for it.
// carol, who sees PUB, watches all three.
//
// A listed session reads a private project only where its credential's list names it, and a
// full member's credential reads every public project its person sees whatever it lists
// (access.CredentialCovers, credential.go:42, and its SQL twin in access.VisibleProjectsSQL).
// While PUB is public, xavier's session listed with CORE and a session of carol's, both under
// broker tokens that list CORE alone, read PUB's issue; once PUB is private both read it exactly as
// an issue nobody holds, while carol at her browser still reads it, so for carol's session the list
// alone decides.
//
// Controls: the trigger's pass over the people tied to the sessions restored (migration
// 20261121173500's function), and xavier reads his own session's file and message as 404, alice
// gets the no-responder verdict for his sessions, and 2 tie pairs are recorded; the trigger's
// listener pass removed, and alice is relayed the third session; VisibleProjectsSQL's list arm
// covering every project for a full member's credential, and carol's session reads PUB's issue
// once PUB is private.
func testPUBMadePrivate(t *testing.T, switchPUB func(t *testing.T, rig contentAgentRig)) {
	listed := func(third string) string {
		return `[
		{"session_id":"` + reachedListedSession + `","title":"Lists CORE","capabilities":["btw"],"last_seen":900,"projects":["CORE"]},
		{"session_id":"` + reachedUnlistedSession + `","title":"No list","capabilities":["btw"],"last_seen":800},
		{"session_id":"` + reachedUntiedSession + `","title":"Tied to nobody","capabilities":["btw"],"last_seen":700,"projects":` + third + `}
	]`
	}
	rig := newReachedRigOf(t, listed(`["PUB","CORE"]`),
		reachedListedSession, reachedUnlistedSession, reachedUntiedSession)
	rig.seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": reachedCore, "name": "Project " + reachedCore,
	}, http.StatusCreated)
	core := decodeBody[struct {
		Key string `json:"key"`
	}](t, rig.seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": reachedCore, "title": "The issue of " + reachedCore,
	}, http.StatusCreated)).Key
	for _, session := range []string{reachedListedSession, reachedUnlistedSession} {
		rig.seed.asSessionOf(t, reachedXavier, session, seedPublic, http.MethodGet, "/api/v1/issues/"+rig.seed.issue[seedPublic], nil, http.StatusOK)
		rig.seed.asSessionOf(t, reachedXavier, session, reachedCore, http.MethodPost, "/api/v1/issues/"+core+"/comments",
			map[string]any{"body": "Done on CORE."}, http.StatusCreated)
	}
	if _, err := rig.seed.database.Pool.Exec(context.Background(), `insert into session_projects (session_id, project_key) values ($1, $2)`,
		reachedUntiedSession, reachedCore); err != nil {
		t.Fatalf("record the untied session's history in CORE: %v", err)
	}
	uploaded := multipartRequest(t, rig.seed.handler, "/api/v1/agents/"+reachedUnlistedSession+"/artifacts",
		map[string]string{"name": "pub.png"}, "pub.png", "image/png", pngBytes("what xavier's session read of PUB"), reachedXavier)
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("xavier uploads to his session's conversation: %d %s", uploaded.Code, uploaded.Body.String())
	}
	file := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact
	direct := createdID(t, dispatchRequest(t, rig.seed.handler, http.MethodPost, "/api/v1/agents/"+reachedUnlistedSession+"/messages",
		map[string]any{"body": "What did you read of PUB?", "delivery": "btw"}, reachedXavier))
	reads := map[string]string{
		"the file":           "/api/v1/agents/" + reachedUnlistedSession + "/artifacts/" + file.Slug,
		"the direct message": "/api/v1/messages/" + direct,
	}
	for name, read := range reads {
		if got := rig.seed.request(t, callerAlice, http.MethodGet, read, nil); got.Code != http.StatusOK {
			t.Fatalf("alice reads %s of xavier's session while PUB is public as %d %s, want 200: the row proves nothing", name, got.Code, got.Body.String())
		}
	}
	// The tokens that list CORE alone read PUB's issue while PUB is public: a full member's list
	// decides only private projects.
	readsPUB := func(person, session string) *httptest.ResponseRecorder {
		t.Helper()
		return rig.seed.sessionRequest(t, person, session, reachedCore, http.MethodGet, "/api/v1/issues/"+rig.seed.issue[seedPublic], nil)
	}
	readsNobody := func(person, session string) *httptest.ResponseRecorder {
		t.Helper()
		return rig.seed.sessionRequest(t, person, session, reachedCore, http.MethodGet, "/api/v1/issues/"+listedNobody, nil)
	}
	listedReaders := []struct{ person, session string }{{reachedXavier, reachedListedSession}, {seedCarol, reachedCarolListedSession}}
	for _, reader := range listedReaders {
		if got := readsPUB(reader.person, reader.session); got.Code != http.StatusOK {
			t.Fatalf("PUB public: %s's session %s under a token listing CORE reads PUB's issue as %d %s, want 200: the row proves nothing",
				reader.person, reader.session, got.Code, strings.TrimSpace(got.Body.String()))
		}
	}
	switchPUB(t, rig)
	if _, err := rig.seed.database.Pool.Exec(context.Background(), `
		insert into route_roster (answered, sessions, sent_at, expires_at)
		values (true, $1::jsonb, now(), now() + interval '1 hour')
		on conflict (id) do update set sessions = excluded.sessions, sent_at = excluded.sent_at, expires_at = excluded.expires_at`,
		listed(`["CORE"]`)); err != nil {
		t.Fatalf("store the listener's sessions, the third listed CORE alone: %v", err)
	}
	const state = "PUB made private"
	reached := func(session string) int {
		t.Helper()
		var pairs int
		if err := rig.seed.database.Pool.QueryRow(context.Background(), `
			select count(*) from session_reached_projects where session_id = $1 and project_key = $2`,
			session, seedPublic).Scan(&pairs); err != nil {
			t.Fatalf("read %s's reached pairs: %v", session, err)
		}
		return pairs
	}
	for _, session := range []string{reachedListedSession, reachedUnlistedSession} {
		if pairs := reached(session); pairs != 0 {
			t.Errorf("%s, the switch recorded %d pairs of PUB against xavier's session %s, want none: a switch withdraws nothing read while PUB was public", state, pairs, session)
		}
		rig.watch(t, state, contentView{callerAlice, session, agentViewResponding, "alice watches what xavier's session read while PUB was public"})
		rig.watch(t, state, contentView{callerCarol, session, agentViewResponding, "carol sees PUB"})
	}
	if pairs := reached(reachedUntiedSession); pairs != 1 {
		t.Errorf("%s, the switch recorded %d pairs of PUB against the session whose list named PUB, want 1", state, pairs)
	}
	rig.hidden(t, state, callerAlice, reachedUntiedSession)
	rig.watch(t, state, contentView{callerCarol, reachedUntiedSession, agentViewResponding, "carol sees PUB"})
	for name, read := range reads {
		if got := dispatchRequest(t, rig.seed.handler, http.MethodGet, read, nil, reachedXavier); got.Code != http.StatusOK {
			t.Errorf("%s, xavier reads %s of his own session as %d %s, want 200", state, name, got.Code, got.Body.String())
		}
		if got := rig.seed.request(t, callerAlice, http.MethodGet, read, nil); got.Code != http.StatusOK {
			t.Errorf("%s, alice reads %s of xavier's session as %d %s, want 200", state, name, got.Code, got.Body.String())
		}
	}
	if got := rig.seed.request(t, callerCarol, http.MethodGet, "/api/v1/issues/"+rig.seed.issue[seedPublic], nil); got.Code != http.StatusOK {
		t.Fatalf("%s, carol at her browser reads PUB's issue as %d %s, want 200: the row proves nothing", state, got.Code, strings.TrimSpace(got.Body.String()))
	}
	for _, reader := range listedReaders {
		requireAnsweredAsMissing(t, fmt.Sprintf("%s, %s's session %s under a token listing CORE reads PUB's issue", state, reader.person, reader.session),
			readsPUB(reader.person, reader.session), readsNobody(reader.person, reader.session), rig.seed.issue[seedPublic], listedNobody)
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
	t.Parallel()
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
