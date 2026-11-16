package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	gws "github.com/gorilla/websocket"
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

// dialLive opens the live socket at url as header presents and reads it.
func dialLive(t *testing.T, url string, header http.Header) *socketClient {
	t.Helper()
	return openLiveSocketAt(t, gws.DefaultDialer, url, header).read()
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
// event about a person's own thread, which reaches that person's every credential whatever it sees
// of any project (eventReadableSQL).
func agentStateOf(login string) func(liveEvent) bool {
	return func(event liveEvent) bool {
		var payload struct {
			Login string `json:"login"`
		}
		return event.Type == "user_agent_state.updated" && json.Unmarshal(event.Payload, &payload) == nil && payload.Login == login
	}
}

// markAgentState writes login's pin on the session s1, as that person at the browser, which
// appends one user_agent_state.updated of theirs (agentStateOf).
func markAgentState(t *testing.T, handler http.Handler, login string) {
	t.Helper()
	if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/agents/s1/state", map[string]any{"pinned": true}, login); response.Code != http.StatusOK {
		t.Fatalf("%s pins s1: status=%d body=%s", login, response.Code, response.Body.String())
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
// PRIV before it; every caller that sees PRIV is sent carol's first comment before her second.
func TestTheLiveSocketSendsNoEventOfAHiddenProject(t *testing.T) {
	seed := seedPrivateProject(t)
	url := liveURLOf(t, seed.handler)
	since := eventHead(t, seed.database)
	live := make(map[privateCaller]*socketClient, len(privateCallers))
	for _, caller := range privateCallers {
		client := dialLive(t, url, seed.liveHeader(t, caller))
		subscribeEvents(t, client, "live", nil)
		live[caller] = client
	}

	issue := "/api/v1/issues/" + seed.issue[seedPrivate] + "/comments"
	hidden := createdID(t, seed.request(t, callerCarol, http.MethodPost, issue, map[string]any{"body": "PRIV's first comment, for its readers alone."}))
	markAgentState(t, seed.handler, seedAlice)
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
		client := dialLive(t, url, seed.liveHeader(t, caller))
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
	client := dialLive(t, liveURLOf(t, seed.handler), seed.liveHeader(t, callerAlice))
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
			grant:  func(*testing.T, privateSeed) {},
			remove: func(t *testing.T, seed privateSeed) {
				seed.mustRequest(t, callerCarol, http.MethodDelete, "/api/v1/projects/"+seedPrivate+"/access/people/"+seedBob, nil, http.StatusOK)
			},
		},
	} {
		t.Run(removal.name, func(t *testing.T) {
			seed := seedPrivateProject(t)
			removal.grant(t, seed)
			client := dialLive(t, liveURLOf(t, seed.handler), asUser(removal.person))
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
			markAgentState(t, seed.handler, removal.person)
			requireNoEventOf(t, removal.person, "feed", seedPrivate, liveEventsThrough(t, client, "feed", agentStateOf(removal.person)))
		})
	}
}
