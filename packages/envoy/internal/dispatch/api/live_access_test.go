package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		frame := client.event(t, sub)
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
