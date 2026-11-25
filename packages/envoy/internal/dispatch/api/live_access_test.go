package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	gws "github.com/gorilla/websocket"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// liveURLOf serves handler behind a real listener until the test ends and answers its GET /ws/live
// address, so a caller of verification v16's seed opens the live socket as production serves it.
func liveURLOf(t *testing.T, handler http.Handler) string {
	t.Helper()
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	return "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/live"
}

// liveHeader is what caller presents on the live socket's upgrade: a person at a browser by the
// test identity's header, a token by its bearer.
func (s privateSeed) liveHeader(t *testing.T, caller privateCaller) http.Header {
	t.Helper()
	if login := caller.login(); login != "" {
		return asUser(login)
	}
	return withBearer(s.bearer(t, caller))
}

// dialLive opens the live socket at url, a server of the seed's handler (liveURLOf), as header
// presents and reads it. The socket names the seed's store (servedBy), the one its server reads,
// so a wait behind the event horizon reads that horizon's own account of itself.
func (s privateSeed) dialLive(t *testing.T, url string, header http.Header) *socketClient {
	t.Helper()
	return openLiveSocketAt(t, gws.DefaultDialer, url, header).servedBy(s.database).read()
}

// liveEvent is what these tests read of an event a live socket sent.
type liveEvent struct {
	ID      int64           `json:"id"`
	Type    string          `json:"type"`
	Project string          `json:"project"`
	Payload json.RawMessage `json:"payload"`
}

// liveEventsThrough reads sub's events on client until marker holds for one, and answers each event
// it read, the marker's included. Events go out in id order, so an event of a lower id than the
// marker's that is not among them was not sent.
func liveEventsThrough(t *testing.T, client *socketClient, sub string, marker func(liveEvent) bool) []liveEvent {
	t.Helper()
	var events []liveEvent
	for {
		frame := client.event(t, eventFeedWait, sub)
		var event liveEvent
		if err := json.Unmarshal(frame.Event, &event); err != nil {
			t.Fatalf("decode the event of %s's frame %+v: %v", sub, frame, err)
		}
		events = append(events, event)
		if marker(event) {
			return events
		}
	}
}

// commentCreated is the marker of the comment.created event of the comment id.
func commentCreated(id string) func(liveEvent) bool {
	return func(event liveEvent) bool {
		var payload struct {
			ID string `json:"id"`
		}
		return event.Type == "comment.created" && json.Unmarshal(event.Payload, &payload) == nil && payload.ID == id
	}
}

// agentStateOf is the marker of a user_agent_state.updated event of login's: the one ownerless
// event about a person's own thread, which reaches that person's credentials, and an open viewer,
// that may see the session it names (eventReadableSQL).
func agentStateOf(login string) func(liveEvent) bool {
	return func(event liveEvent) bool {
		var payload struct {
			Login string `json:"login"`
		}
		return event.Type == "user_agent_state.updated" && json.Unmarshal(event.Payload, &payload) == nil && payload.Login == login
	}
}

// markAgentState writes login's pin on session, as that person at the browser, which appends one
// user_agent_state.updated of theirs (agentStateOf): a marker that ends a read. The session must be
// one login sees (access.SessionClause): one that acts in no project, for an open viewer, or one
// that acts in a project login sees. The listener is first made to list it with no project, as
// Dispatch stores the listener's answer (the route_roster row), since while a project is private a
// session that acts in no project reaches an open viewer only through that record.
func (s privateSeed) markAgentState(t *testing.T, login, session string) {
	t.Helper()
	if _, err := s.database.Pool.Exec(t.Context(), `
		insert into route_roster (answered, sessions, sent_at, expires_at)
		values (true, jsonb_build_array(jsonb_build_object('session_id', $1::text, 'projects', '[]'::jsonb)), now(), now())
		on conflict (id) do update
		set answered = true, sessions = case when route_roster.sessions @> excluded.sessions then route_roster.sessions
			else route_roster.sessions || excluded.sessions end`, session); err != nil {
		t.Fatalf("list %s as the listener's stored answer: %v", session, err)
	}
	if response := dispatchRequest(t, s.handler, http.MethodPut, "/api/v1/me/agents/"+session+"/state", map[string]any{"pinned": true}, login); response.Code != http.StatusOK {
		t.Fatalf("%s pins %s: status=%d body=%s", login, session, response.Code, response.Body.String())
	}
}

// requireNoEventOf fails naming each of events that belongs to project.
func requireNoEventOf(t *testing.T, who, sub, project string, events []liveEvent) {
	t.Helper()
	for _, event := range events {
		if event.Project == project {
			t.Errorf("%s's live socket sent %s %d of %s on %s, a project it does not see", who, event.Type, event.ID, project, sub)
		}
	}
}

// TestTheLiveSocketSendsNoEventOfAHiddenProject: the live socket sends an event under the event
// log's one rule (eventReadableSQL), for the viewer the socket resolved, on the live feed and on a
// resume's replay alike. carol comments twice on PRIV's issue, and alice pins a session between the
// two. Every caller of verification v16 that does not see PRIV is sent alice's pin and nothing of
// PRIV before it, alice's token that lists PRIV among them, an open viewer that sees PUB alone
// since alice does not see PRIV; every caller that sees PRIV is sent carol's first comment before
// her second.
func TestTheLiveSocketSendsNoEventOfAHiddenProject(t *testing.T) {
	seed := seedPrivateProject(t)
	url := liveURLOf(t, seed.handler)
	since := eventHead(t, seed.database)
	live := make(map[privateCaller]*socketClient, len(privateCallers))
	for _, caller := range privateCallers {
		client := seed.dialLive(t, url, seed.liveHeader(t, caller))
		subscribeEvents(t, client, "live", nil)
		live[caller] = client
	}

	issue := "/api/v1/issues/" + seed.issue[seedPrivate] + "/comments"
	hidden := createdID(t, seed.request(t, callerCarol, http.MethodPost, issue, map[string]any{"body": "PRIV's first comment, for its readers alone."}))
	seed.markAgentState(t, seedAlice, "s1")
	last := createdID(t, seed.request(t, callerCarol, http.MethodPost, issue, map[string]any{"body": "PRIV's second comment."}))

	requireSent := func(t *testing.T, caller privateCaller, sub string, client *socketClient) {
		t.Helper()
		if !slices.Contains(callersOfPrivate, caller) {
			requireNoEventOf(t, string(caller), sub, seedPrivate, liveEventsThrough(t, client, sub, agentStateOf(seedAlice)))
			return
		}
		if !slices.ContainsFunc(liveEventsThrough(t, client, sub, commentCreated(last)), commentCreated(hidden)) {
			t.Errorf("%s sees PRIV, and its live socket sent no comment.created of carol's first PRIV comment %s on %s", caller, hidden, sub)
		}
	}
	for _, caller := range privateCallers {
		requireSent(t, caller, "live", live[caller])
	}
	// A page that resumes from before the comments is replayed them under the same rule.
	for _, caller := range privateCallers {
		client := seed.dialLive(t, url, seed.liveHeader(t, caller))
		if start := subscribeEventsStart(t, client, "replay", &since); !start.ReplayComplete {
			t.Fatalf("%s's resume from %d says its replay is not complete: %+v", caller, since, start)
		}
		requireSent(t, caller, "replay", client)
	}
}

// TestAnOpenSocketIsSentTheEventsOfAProjectMadeAfterItSubscribed: what a socket's subscription sees
// is resolved as it starts, and a project made after that is public, so every full member sees it.
// The subscription resolves the caller again at the project.created event, so alice's open feed is
// sent the project's events, as every feed was before a project could be private, and goes on
// without ending: a project that has just been made holds nothing the page read before.
func TestAnOpenSocketIsSentTheEventsOfAProjectMadeAfterItSubscribed(t *testing.T) {
	seed := seedPrivateProject(t)
	client := seed.dialLive(t, liveURLOf(t, seed.handler), seed.liveHeader(t, callerAlice))
	subscribeEvents(t, client, "feed", nil)
	seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/projects", map[string]string{"key": "NEW", "name": "Project NEW"}, http.StatusCreated)
	seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues", map[string]string{"project": "NEW", "title": "The issue of NEW"}, http.StatusCreated)
	liveEventsThrough(t, client, "feed", func(event liveEvent) bool { return event.Type == "issue.created" && event.Project == "NEW" })
}

// TestARemovedPersonsSocketRefreshesWithoutTheProject: a person who loses PRIV while their page
// holds an events subscription - taken off a group PRIV's list names, or off the list itself - has
// that subscription ended (behind), and the page's resubscribe from the last event it applied says
// its replay is not complete, the frame on which the page refreshes everything it read of PRIV. From
// then on the socket sends nothing of PRIV.
func TestARemovedPersonsSocketRefreshesWithoutTheProject(t *testing.T) {
	const dave = "dave@example.com"
	for _, removal := range []struct {
		name   string
		person string
		// grant lets person see PRIV, and remove takes it away again.
		grant, remove func(t *testing.T, seed privateSeed)
	}{
		{
			name:   "a group removal",
			person: dave,
			grant: func(t *testing.T, seed privateSeed) {
				seedPeople(t, seed.database, dave)
				seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/access-groups/team", map[string]any{"members": []string{dave}}, http.StatusOK)
				seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/projects/"+seedPrivate+"/access/groups/team", nil, http.StatusOK)
			},
			remove: func(t *testing.T, seed privateSeed) {
				seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/access-groups/team", map[string]any{"members": []string{}}, http.StatusOK)
			},
		},
		{
			name:   "a direct removal",
			person: seedBob,
			// bob is restricted, so he sees only the projects a list names him on, and a session
			// only when it acted in one of them under a credential that proved it (asSession): PUB
			// lists him too, and s1 acts in PUB, so the pin that marks the end of his feed
			// (markAgentState) names a session he still sees.
			grant: func(t *testing.T, seed privateSeed) {
				seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/projects/"+seedPublic+"/access/people/"+seedBob, nil, http.StatusOK)
				seed.asSession(t, "s1", seedPublic, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPublic]+"/comments",
					map[string]any{"body": "s1 works on PUB."}, http.StatusCreated)
			},
			remove: func(t *testing.T, seed privateSeed) {
				seed.mustRequest(t, callerCarol, http.MethodDelete, "/api/v1/projects/"+seedPrivate+"/access/people/"+seedBob, nil, http.StatusOK)
			},
		},
	} {
		t.Run(removal.name, func(t *testing.T) {
			seed := seedPrivateProject(t)
			removal.grant(t, seed)
			client := seed.dialLive(t, liveURLOf(t, seed.handler), asUser(removal.person))
			subscribeEvents(t, client, "feed", nil)
			issue := "/api/v1/issues/" + seed.issue[seedPrivate] + "/comments"
			seen := createdID(t, seed.request(t, callerCarol, http.MethodPost, issue, map[string]any{"body": "Before the removal."}))
			read := liveEventsThrough(t, client, "feed", commentCreated(seen))
			applied := read[len(read)-1].ID

			removal.remove(t, seed)
			frame := client.next(t, eventFeedWait, "feed's end")
			if ended, ok := frame.(LiveEnded); !ok || ended.Sub != "feed" || ended.Reason != liveEndedBehind {
				t.Fatalf("after %s lost PRIV the socket sent %+v, want feed's end, behind", removal.person, frame)
			}
			if start := subscribeEventsStart(t, client, "feed", &applied); start.ReplayComplete {
				t.Errorf("%s's resubscribe from %d after losing PRIV says its replay is complete, so the page keeps what it read of PRIV: %+v",
					removal.person, applied, start)
			}
			createdID(t, seed.request(t, callerCarol, http.MethodPost, issue, map[string]any{"body": "After the removal."}))
			seed.markAgentState(t, removal.person, "s1")
			requireNoEventOf(t, removal.person, "feed", seedPrivate, liveEventsThrough(t, client, "feed", agentStateOf(removal.person)))
		})
	}
}

// TestAResumeOverEventsTheCallerMayNotReadIsComplete: a resume's gap is the events that change
// what its caller's page holds. Once PRIV is private its log goes on - a comment, a person added
// to its list and one taken off it - and a caller that does not see PRIV is replayed none of it
// (TestTheLiveSocketSendsNoEventOfAHiddenProject), so its resume across that misses nothing it
// may read and says replay_complete true, as it does on a deployment where PRIV was never written.
// Every caller that does not see PRIV is asked, carol's token that lists no project among them,
// though PRIV's list names carol: an addition, and another's removal, take nothing from her.
func TestAResumeOverEventsTheCallerMayNotReadIsComplete(t *testing.T) {
	const dave = "dave@example.com"
	seed := seedPrivateProject(t)
	seedPeople(t, seed.database, dave)
	since := eventHead(t, seed.database)
	createdID(t, seed.request(t, callerCarol, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPrivate]+"/comments",
		map[string]any{"body": "PRIV's comment, after the resume point."}))
	seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/projects/"+seedPrivate+"/access/people/"+dave, nil, http.StatusOK)
	seed.mustRequest(t, callerCarol, http.MethodDelete, "/api/v1/projects/"+seedPrivate+"/access/people/"+seedBob, nil, http.StatusOK)
	url := liveURLOf(t, seed.handler)
	for _, caller := range callersBlindToPrivate {
		client := seed.dialLive(t, url, seed.liveHeader(t, caller))
		if got := subscribeEventsStart(t, client, "feed", &since); !got.ReplayComplete {
			t.Errorf("%s's resume from %d across PRIV's events says its replay is not complete: %+v", caller, since, got)
		}
	}
}

// requireResumeComplete fails unless caller's resume from since says replay_complete want, naming
// why the caller's page does or does not owe a refresh.
func requireResumeComplete(t *testing.T, seed privateSeed, url string, caller privateCaller, since int64, want bool, why string) {
	t.Helper()
	client := seed.dialLive(t, url, seed.liveHeader(t, caller))
	if got := subscribeEventsStart(t, client, "feed", &since); got.ReplayComplete != want {
		t.Errorf("%s's resume from %d says replay_complete %t, want %t: %s", caller, since, got.ReplayComplete, want, why)
	}
}

// TestAResumeAcrossAProjectTurnedPrivateOwesARefreshToWhoLostIt: PUB turns private with nobody on
// its list but its owner carol. Every open caller read PUB while it was public and reads it no
// more, so its resume across that change is not complete, though it may not read the change
// itself: its page holds PUB's rows. alice's token that lists PRIV is one of them, a full member's
// credential being open whatever it lists. A caller that never saw PUB - bob, a restricted member
// PUB's list does not name, and bob's token that lists PRIV - resumes complete.
func TestAResumeAcrossAProjectTurnedPrivateOwesARefreshToWhoLostIt(t *testing.T) {
	seed := seedPrivateProject(t)
	since := eventHead(t, seed.database)
	seed.makePrivate(t, seedPublic)
	url := liveURLOf(t, seed.handler)
	for _, caller := range []privateCaller{callerAlice, callerAlicePrivToken, callerCarolToken, callerSharedToken} {
		requireResumeComplete(t, seed, url, caller, since, false, "it read PUB while PUB was public, and its page holds PUB's rows")
	}
	for _, caller := range []privateCaller{callerBob, callerBobPrivToken} {
		requireResumeComplete(t, seed, url, caller, since, true, "it never read PUB")
	}
}

// TestAResumeAcrossAnOwnerChangeOwesARefreshToThePreviousOwner: carol, PRIV's owner and on its
// list no longer, gives PRIV to dave. carol reads PRIV no more, so her resume across the change is
// not complete, though she may not read the change itself; alice, who never read PRIV while it was
// private, resumes complete.
func TestAResumeAcrossAnOwnerChangeOwesARefreshToThePreviousOwner(t *testing.T) {
	const dave = "dave@example.com"
	seed := seedPrivateProject(t)
	seedPeople(t, seed.database, dave)
	seed.mustRequest(t, callerCarol, http.MethodDelete, "/api/v1/projects/"+seedPrivate+"/access/people/"+seedCarol, nil, http.StatusOK)
	since := eventHead(t, seed.database)
	seed.mustRequest(t, callerCarol, http.MethodPatch, "/api/v1/projects/"+seedPrivate+"/access", map[string]any{"owner": dave}, http.StatusOK)
	url := liveURLOf(t, seed.handler)
	requireResumeComplete(t, seed, url, callerCarol, since, false, "she owned PRIV, reads it no more, and her page holds PRIV's rows")
	requireResumeComplete(t, seed, url, callerAlice, since, true, "she never read PRIV")
}

// TestAResumeAcrossAProjectTurnedPrivateOwesARefreshToATokenThatListedIt: PRIV is public, then
// turns private. alice's token lists PRIV, and read it while it was public, alice being a full
// member; once PRIV is private no list of it names alice, so the token reads it no more and its
// resume across the change is not complete, though it may not read the change itself. carol's token
// lists PRIV too, and PRIV's list names carol, so the token reads PRIV before the change and after:
// it reads the change, which moved nothing it sees, and its resume is complete.
func TestAResumeAcrossAProjectTurnedPrivateOwesARefreshToATokenThatListedIt(t *testing.T) {
	seed := seedPrivateProject(t)
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	since := eventHead(t, seed.database)
	seed.makePrivate(t, seedPrivate)
	url := liveURLOf(t, seed.handler)
	requireResumeComplete(t, seed, url, callerAlicePrivToken, since, false, "it listed PRIV and read it while PRIV was public, and reads it no more")
	requireResumeComplete(t, seed, url, callerCarolPrivToken, since, true, "it read PRIV before the change and after it")
}

// TestAResumeAcrossAnAccessChangeOwesARefreshOnlyToWhomItMoved: carol adds dave to PRIV's list.
// dave now reads PRIV, which his page lacks, so his resume across the addition is not complete;
// carol, and her token that lists PRIV, read the addition and read PRIV before it and after it, so
// their resumes are complete: an access change is a gap only for a caller whose projects it moved.
func TestAResumeAcrossAnAccessChangeOwesARefreshOnlyToWhomItMoved(t *testing.T) {
	const dave = "dave@example.com"
	seed := seedPrivateProject(t)
	seedPeople(t, seed.database, dave)
	since := eventHead(t, seed.database)
	seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/projects/"+seedPrivate+"/access/people/"+dave, nil, http.StatusOK)
	url := liveURLOf(t, seed.handler)
	client := seed.dialLive(t, url, asUser(dave))
	if got := subscribeEventsStart(t, client, "feed", &since); got.ReplayComplete {
		t.Errorf("dave's resume from %d across his addition to PRIV says its replay is complete, so his page lacks PRIV: %+v", since, got)
	}
	for _, caller := range []privateCaller{callerCarol, callerCarolPrivToken} {
		requireResumeComplete(t, seed, url, caller, since, true, "dave's addition moved nothing it sees")
	}
}
