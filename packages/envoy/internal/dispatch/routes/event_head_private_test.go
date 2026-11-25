package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// whoamiHeadAs answers GET /auth/whoami's event_head as the person login, as a decimal event id.
func whoamiHeadAs(t *testing.T, handler http.Handler, login string) int64 {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/whoami", nil)
	request.Header.Set("X-Dispatch-User", login)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("whoami as %s: status=%d body=%s", login, response.Code, response.Body.String())
	}
	var answer whoamiAnswer
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode whoami as %s %s: %v", login, response.Body.String(), err)
	}
	return eventHeadID(t, answer)
}

// writeAs makes one write through the router's API as the person login, failing the test unless
// it answers want.
func writeAs(t *testing.T, handler http.Handler, login, method, path, body string, want int) {
	t.Helper()
	request := httptest.NewRequest(method, "http://dispatch.test"+path, strings.NewReader(body))
	request.Header.Set("X-Dispatch-User", login)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != want {
		t.Fatalf("%s %s as %s: status=%d body=%s, want %d", method, path, login, response.Code, response.Body.String(), want)
	}
}

// eventsStartSinceAs opens the live socket as login, subscribes to the events view with no resume
// and answers the since its events_start names. The events_start is read behind the event horizon
// (waitEventsStart), for as long as the horizon says another client of the instance holds it, so
// no fixed deadline fails the read on a shared cluster. own is the store the router reads.
func eventsStartSinceAs(t *testing.T, server *httptest.Server, own *store.Store, login string) int64 {
	t.Helper()
	conn, response, err := gws.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/ws/live", http.Header{"X-Dispatch-User": {login}})
	if err != nil {
		status := "no response"
		if response != nil {
			status = response.Status
		}
		t.Fatalf("open the live socket as %s: %v (%s)", login, err, status)
	}
	defer conn.Close()
	if err := conn.WriteJSON(map[string]any{"type": "subscribe", "sub": "events", "view": map[string]string{"kind": "events"}}); err != nil {
		t.Fatalf("subscribe as %s: %v", login, err)
	}
	var frame struct {
		Type  string `json:"type"`
		Since string `json:"since"`
	}
	if data := waitEventsStart(t, conn, own); json.Unmarshal(data, &frame) != nil || frame.Type != "events_start" {
		t.Fatalf("the events subscription's first frame as %s is %s, want its events_start", login, data)
	}
	since, err := strconv.ParseInt(frame.Since, 10, 64)
	if err != nil {
		t.Fatalf("events_start as %s names since %q, not a decimal event id: %v", login, frame.Since, err)
	}
	return since
}

// TestTheEventHeadsAPersonReadsDoNotMoveWithAProjectTheyCannotSee: the heads a person reads of
// the event log, GET /auth/whoami's event_head and an events subscription's events_start since,
// are the highest id of an event that person can see, so carol's writes in PRIV, a private
// project alice does not see, move carol's heads and leave alice's where they stood, while
// alice's own write moves hers. With no private project an open viewer reads the log's head, as
// before (TestWhoamiCarriesTheNewestEventID).
//
// Control: answer the log's head to every person (eventFeedHead alone); alice's head then moves
// with carol's comment on PRIV-1, and the test fails naming it.
func TestTheEventHeadsAPersonReadsDoNotMoveWithAProjectTheyCannotSee(t *testing.T) {
	database := storetest.Open(t)
	handler := storeRouter(t, database)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	const alice, carol = "alice@d.example", "carol@d.example"

	writeAs(t, handler, alice, http.MethodPost, "/api/v1/projects", `{"key":"PUB","name":"PUB project"}`, http.StatusCreated)
	writeAs(t, handler, carol, http.MethodPost, "/api/v1/projects", `{"key":"PRIV","name":"PRIV project"}`, http.StatusCreated)
	if _, err := database.Pool.Exec(context.Background(), `update projects set visibility = 'private' where key = 'PRIV'`); err != nil {
		t.Fatalf("make PRIV private: %v", err)
	}
	before := whoamiHeadAs(t, handler, alice)

	writeAs(t, handler, carol, http.MethodPost, "/api/v1/issues", `{"project":"PRIV","title":"A private piece of work"}`, http.StatusCreated)
	writeAs(t, handler, carol, http.MethodPost, "/api/v1/issues/PRIV-1/comments", `{"body":"A private comment."}`, http.StatusCreated)
	var newest int64
	if err := database.Pool.QueryRow(context.Background(), `select max(id) from events`).Scan(&newest); err != nil {
		t.Fatalf("read the log's newest id: %v", err)
	}
	if newest <= before {
		t.Fatalf("carol's writes in PRIV appended no event above alice's head %d (newest %d)", before, newest)
	}
	if head := whoamiHeadAs(t, handler, alice); head != before {
		t.Errorf("alice's whoami event_head moved from %d to %d with carol's comment on PRIV-1, a project alice does not see", before, head)
	}
	if since := eventsStartSinceAs(t, server, database, alice); since != before {
		t.Errorf("alice's events_start since = %d, want her head %d: it moved with PRIV's events", since, before)
	}
	if head := whoamiHeadAs(t, handler, carol); head != newest {
		t.Errorf("carol's whoami event_head = %d, want the newest event %d, her own comment on PRIV-1", head, newest)
	}
	if since := eventsStartSinceAs(t, server, database, carol); since != newest {
		t.Errorf("carol's events_start since = %d, want the newest event %d", since, newest)
	}

	writeAs(t, handler, alice, http.MethodPost, "/api/v1/issues", `{"project":"PUB","title":"A public piece of work"}`, http.StatusCreated)
	if err := database.Pool.QueryRow(context.Background(), `select max(id) from events`).Scan(&newest); err != nil {
		t.Fatalf("read the log's newest id: %v", err)
	}
	if head := whoamiHeadAs(t, handler, alice); head != newest {
		t.Errorf("alice's whoami event_head after her own write in PUB = %d, want that write's event %d", head, newest)
	}
}

// GET /auth/whoami answers the person its cookie or identity header names, and its event_head is
// that person's: a bearer sent beside it is never read for the head (api.personEventHead resolves
// what the person sees with the request's Authorization header removed). A bearer that does not
// verify, beside alice's identity, still answers alice's head; were it read, the head's resolution
// would refuse it and whoami would answer 503 EVENT_HEAD_READ_FAILED.
//
// Control: resolve the head's caller from the request as it came (no header removed); the whoami
// answers 503 and the test fails naming it.
func TestWhoamisEventHeadIsThePersonsWhateverBearerItCarries(t *testing.T) {
	database := storetest.Open(t)
	handler := storeRouter(t, database)
	const alice = "alice@d.example"
	writeAs(t, handler, alice, http.MethodPost, "/api/v1/projects", `{"key":"PUB","name":"PUB project"}`, http.StatusCreated)
	want := whoamiHeadAs(t, handler, alice)

	request := httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/whoami", nil)
	request.Header.Set("X-Dispatch-User", alice)
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("x", 12))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("whoami as %s with a bearer beside her identity answered %d %s, want 200 with her head %d: the bearer was read for the head", alice, response.Code, response.Body.String(), want)
	}
	var answer whoamiAnswer
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode whoami %s: %v", response.Body.String(), err)
	}
	if answer.Login != alice || eventHeadID(t, answer) != want {
		t.Errorf("whoami with a bearer beside %s's identity answered %+v, want her login and her head %d", alice, answer, want)
	}
}
