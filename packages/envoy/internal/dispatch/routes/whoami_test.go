package routes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/horizontest"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The tests that read GET /auth/whoami's event_head off a real event log open a database of their
// own (storetest.Open); the rest of this package's tests hold none.
func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// standInEventLog gives a router built with no Store an event log whose newest event is head, so
// GET /auth/whoami answers it: the sign-in tests read whoami for the person it names, and their
// routers hold no database to read a head from.
func standInEventLog(ctx *AppContext, head int64) {
	ctx.eventHead = func(context.Context) (int64, error) { return head, nil }
}

// storeRouter is the router main serves over database, with header identity naming the person and
// a Lifetime, so it mounts GET /ws/live as production does. Its people are the database's own
// table, as main's are: a header names a person by recording them there, and a project create
// names its default from that same table (allowedLogin), so an in-memory store here would leave
// the person the header names unknown to the create. Its event horizon warns of a stall at
// horizontest.StallWarning, as a cmd/dispatch test process's does, so a row's output names whoever
// holds the horizon while it waits; the wait itself reads no warning (waitEventsStart).
func storeRouter(t *testing.T, database *store.Store) http.Handler {
	t.Helper()
	broker := events.NewBroker()
	documents := docs.New(docs.Deps{Store: database, Events: broker})
	t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
	people := store.NewPgPeopleStore(database.Pool, testSigningKey, nil)
	ctx, err := BuildAppContext(AppContextOptions{
		SigningKey:        testSigningKey,
		People:            people,
		Identity:          identity.HeaderIdentity{Header: "X-Dispatch-User", People: people},
		Store:             database,
		Docs:              documents,
		Events:            broker,
		Lifetime:          t.Context(),
		HorizonStallAfter: horizontest.StallWarning,
		HorizonWarnEvery:  horizontest.StallWarning,
	})
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	handler, background := New(ctx)
	t.Cleanup(background.Wait)
	return handler
}

// askWhoami answers GET /auth/whoami as alice@d.example, failing the test unless it answers 200.
func askWhoami(t *testing.T, handler http.Handler) whoamiAnswer {
	t.Helper()
	return whoamiAnswerOf(t, serveWhoami(handler))
}

// askWhoamiBeforeTheDeadline is askWhoami for a row whose subject is that whoami does not wait on
// a transaction the row holds open to its end: the request runs on a goroutine of its own, and the
// row waits for its answer until the test binary's deadline less a second (testwait.Bounded of a
// day, so it has no span of its own), then fails naming what it waited for. A whoami that waited
// on the held transaction would never answer, so that wait, not a wall-clock budget, is what says
// it did not; a loaded machine only makes the answer slower.
func askWhoamiBeforeTheDeadline(t *testing.T, handler http.Handler, what string) whoamiAnswer {
	t.Helper()
	served := make(chan *httptest.ResponseRecorder, 1)
	go func() { served <- serveWhoami(handler) }()
	bound := testwait.Bounded(t, 24*time.Hour, time.Second)
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case response := <-served:
		return whoamiAnswerOf(t, response)
	case <-timer.C:
		t.Fatalf("timed out after %s waiting for %s", bound.Round(time.Millisecond), what)
		return whoamiAnswer{}
	}
}

// serveWhoami serves GET /auth/whoami as alice@d.example and answers the response. It touches no
// *testing.T, so a row may run it on a goroutine of its own.
func serveWhoami(handler http.Handler) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/whoami", nil)
	request.Header.Set("X-Dispatch-User", "alice@d.example")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// whoamiAnswerOf decodes a whoami response, failing the test unless it is a 200 for alice@d.example.
func whoamiAnswerOf(t *testing.T, response *httptest.ResponseRecorder) whoamiAnswer {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("whoami: status=%d body=%s", response.Code, response.Body.String())
	}
	var answer whoamiAnswer
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode whoami %s: %v", response.Body.String(), err)
	}
	if answer.Kind != "user" || answer.Login != "alice@d.example" {
		t.Fatalf("whoami = %+v, want alice@d.example", answer)
	}
	return answer
}

// createProject creates the project key through the router's API, as alice@d.example, which
// appends one event, project.created.
func createProject(t *testing.T, handler http.Handler, key string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://dispatch.test/api/v1/projects", strings.NewReader(`{"key":"`+key+`","name":"`+key+` project"}`))
	request.Header.Set("X-Dispatch-User", "alice@d.example")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create project %s: status=%d body=%s", key, response.Code, response.Body.String())
	}
}

// GET /auth/whoami carries event_head, the decimal id of the newest committed event, read after the
// sign-in check: "0" on an empty log, and the third event's id once three have committed.
func TestWhoamiCarriesTheNewestEventID(t *testing.T) {
	database := storetest.Open(t)
	handler := storeRouter(t, database)
	if answer := askWhoami(t, handler); answer.EventHead != "0" {
		t.Fatalf("whoami on an empty event log: event_head = %q, want \"0\"", answer.EventHead)
	}

	for _, key := range []string{"ONE", "TWO", "THREE"} {
		createProject(t, handler, key)
	}
	rows, err := database.Pool.Query(context.Background(), `select id from events order by id`)
	if err != nil {
		t.Fatalf("read the event log: %v", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("read an event id: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the event log: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("the log holds %d events, want the three projects' (%v)", len(ids), ids)
	}
	if answer := askWhoami(t, handler); answer.EventHead != strconv.FormatInt(ids[2], 10) {
		t.Fatalf("whoami after three events: event_head = %q, want the third's id %d", answer.EventHead, ids[2])
	}
}

// A page starts its event-affected reads only after whoami answers and resumes its first events
// subscription from whoami's event_head, so a change committed while the page loads -- after the
// answer, before the subscription -- reaches it during catch-up, and the subscription's
// events_start names that head.
func TestAnEventsSubscriptionFromTheWhoamiHeadReplaysWhatCommittedSince(t *testing.T) {
	database := storetest.Open(t)
	handler := storeRouter(t, database)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	createProject(t, handler, "BEFORE")
	head := askWhoami(t, handler).EventHead

	// Committed while the page loads, between whoami's answer and its subscription.
	createProject(t, handler, "LOADING")

	conn, response, err := gws.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/ws/live", http.Header{"X-Dispatch-User": {"alice@d.example"}})
	if err != nil {
		status := "no response"
		if response != nil {
			status = response.Status
		}
		t.Fatalf("open the live socket: %v (%s)", err, status)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.WriteJSON(map[string]any{
		"type": "subscribe", "sub": "events", "view": map[string]string{"kind": "events"},
		"resume": map[string]string{"since": head},
	}); err != nil {
		t.Fatalf("subscribe to the event feed: %v", err)
	}
	next := func(what string) (string, []byte) {
		t.Helper()
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("read %s: %v", what, err)
			}
			var frame struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(data, &frame); err != nil {
				t.Fatalf("decode %s %s: %v", what, data, err)
			}
			if frame.Type != "heartbeat" {
				return frame.Type, data
			}
		}
	}

	data := waitEventsStart(t, conn, database)
	var start api.LiveEventsStart
	if err := json.Unmarshal(data, &start); err != nil || start.Type != "events_start" || start.Sub != "events" || start.Since != head {
		t.Fatalf("first frame = %s (%v), want events' events_start from whoami's head %s", data, err, head)
	}
	// The replay follows the events_start out of what the subscription read before sending it,
	// with no wait on the horizon between them (feedEvents), so a deadline the test owns bounds it.
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	kind, data := next("the change committed while the page loaded")
	var frame api.LiveEventFrame
	if err := json.Unmarshal(data, &frame); err != nil || kind != "event" {
		t.Fatalf("second frame = %s (%v), want an event", data, err)
	}
	var event struct {
		Type    string `json:"type"`
		Payload struct {
			Key string `json:"key"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(frame.Event, &event); err != nil || event.Type != "project.created" || event.Payload.Key != "LOADING" {
		t.Fatalf("the catch-up's first event = %s (%v), want LOADING's project.created", frame.Event, err)
	}
}

// A whoami that cannot read the event log's head answers 503 EVENT_HEAD_READ_FAILED rather than a
// whoami without it: a page that took a missing head for an older server's answer would start its
// feed at the server's head and could miss a change committed while it loaded. It is not
// EVENT_HEAD_UNAVAILABLE, which the page asks again for without limit: a database that does not
// answer must end at the page's error, not keep it loading for ever.
func TestWhoamiRefusesWhenItCannotReadTheEventHead(t *testing.T) {
	for name, build := range map[string]func(t *testing.T) AppContextOptions{
		"a router with no store": func(*testing.T) AppContextOptions { return AppContextOptions{} },
		"a database that stopped answering": func(t *testing.T) AppContextOptions {
			database := storetest.Open(t)
			database.Pool.Close()
			return AppContextOptions{Store: database}
		},
	} {
		t.Run(name, func(t *testing.T) {
			options := build(t)
			people := newMemoryPeople()
			options.SigningKey = testSigningKey
			options.People = people
			options.Identity = identity.HeaderIdentity{Header: "X-Dispatch-User", People: people}
			options.Lifetime = t.Context()
			ctx, err := BuildAppContext(withDocuments(t, options))
			if err != nil {
				t.Fatalf("build context: %v", err)
			}
			handler, background := New(ctx)
			t.Cleanup(background.Wait)
			request := httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/whoami", nil)
			request.Header.Set("X-Dispatch-User", "alice@d.example")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			var body struct {
				Code  string `json:"code"`
				Error string `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusServiceUnavailable ||
				body.Code != "EVENT_HEAD_READ_FAILED" || body.Error == "" {
				t.Fatalf("whoami: status=%d body=%s, want 503 EVENT_HEAD_READ_FAILED", response.Code, response.Body.String())
			}
		})
	}
}

// whoami checks sign-in before it reads the event log's head, so a caller with no sign-in is
// answered 401 NO_IDENTITY, which sends a page to sign in, even while the head cannot be read,
// and the head is not read for it: a 503 there would have a signed-out page retry a server that
// only needs its sign-in. The same router signed in is answered the 503.
func TestAnAnonymousWhoamiIsUnauthorizedWhileTheEventHeadCannotBeRead(t *testing.T) {
	people := newMemoryPeople()
	ctx, err := BuildAppContext(withDocuments(t, AppContextOptions{
		SigningKey: testSigningKey,
		People:     people,
		Identity:   identity.HeaderIdentity{Header: "X-Dispatch-User", People: people},
		Lifetime:   t.Context(),
	}))
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	var reads atomic.Int32
	ctx.eventHead = func(context.Context) (int64, error) {
		reads.Add(1)
		return 0, errors.New("the database stopped answering")
	}
	handler, background := New(ctx)
	t.Cleanup(background.Wait)
	whoami := func(login string) (int, string) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/whoami", nil)
		if login != "" {
			request.Header.Set("X-Dispatch-User", login)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		var body struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatalf("whoami as %q: status=%d body=%s: %v", login, response.Code, response.Body.String(), err)
		}
		return response.Code, body.Code
	}

	if status, code := whoami(""); status != http.StatusUnauthorized || code != "NO_IDENTITY" {
		t.Fatalf("an anonymous whoami while the head cannot be read: %d %s, want 401 NO_IDENTITY", status, code)
	}
	if n := reads.Load(); n != 0 {
		t.Fatalf("an anonymous whoami read the event head %d times, want none", n)
	}
	if status, code := whoami("alice@d.example"); status != http.StatusServiceUnavailable || code != "EVENT_HEAD_READ_FAILED" {
		t.Fatalf("a signed-in whoami while the head cannot be read: %d %s, want 503 EVENT_HEAD_READ_FAILED", status, code)
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("a signed-in whoami read the event head %d times, want once", n)
	}
}

// whoami answers 503 EVENT_HEAD_UNAVAILABLE only while no event id is proven final yet
// (api.ErrEventHeadUnproven), the one answer the page asks again for without limit, since it ends
// on its own once the open event insert does. The same error wrapped is answered the same, and a
// failed read is EVENT_HEAD_READ_FAILED.
func TestWhoamiAnswersEventHeadUnavailableOnlyWhileNoHeadIsProven(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		code string
	}{
		"no head proven":          {api.ErrEventHeadUnproven, "EVENT_HEAD_UNAVAILABLE"},
		"no head proven, wrapped": {fmt.Errorf("event head: %w", api.ErrEventHeadUnproven), "EVENT_HEAD_UNAVAILABLE"},
		"a failed read":           {errors.New("read the oldest transaction that can write the event log: dial: connection refused"), "EVENT_HEAD_READ_FAILED"},
	} {
		t.Run(name, func(t *testing.T) {
			people := newMemoryPeople()
			ctx, err := BuildAppContext(withDocuments(t, AppContextOptions{
				SigningKey: testSigningKey,
				People:     people,
				Identity:   identity.HeaderIdentity{Header: "X-Dispatch-User", People: people},
				Lifetime:   t.Context(),
			}))
			if err != nil {
				t.Fatalf("build context: %v", err)
			}
			ctx.eventHead = func(context.Context) (int64, error) { return 0, tc.err }
			handler, background := New(ctx)
			t.Cleanup(background.Wait)
			request := httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/whoami", nil)
			request.Header.Set("X-Dispatch-User", "alice@d.example")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			var body struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusServiceUnavailable || body.Code != tc.code {
				t.Fatalf("whoami: status=%d body=%s, want 503 %s", response.Code, response.Body.String(), tc.code)
			}
		})
	}
}

// newestEventID is the newest id in the event log, which is what whoami's event_head answers while
// no write of this database is open.
func newestEventID(t *testing.T, database *store.Store) int64 {
	t.Helper()
	var newest int64
	if err := database.Pool.QueryRow(context.Background(), `select coalesce(max(id), 0) from events`).Scan(&newest); err != nil {
		t.Fatalf("read the event log's newest id: %v", err)
	}
	return newest
}

// eventHeadID is the decimal event id an answer's event_head names.
func eventHeadID(t *testing.T, answer whoamiAnswer) int64 {
	t.Helper()
	head, err := strconv.ParseInt(answer.EventHead, 10, 64)
	if err != nil {
		t.Fatalf("whoami's event_head = %q, not a decimal event id: %v", answer.EventHead, err)
	}
	return head
}

// appendOutOfOrder appends two events of Dispatch's own log so they commit out of id order: tx A
// appends on project first (the lower id, low) and stays open, then tx B appends on project second
// (high) and commits. commitLow commits A; until it runs, low is an id below a committed one that
// no reader can see. A that the test never commits rolls back when the test ends.
func appendOutOfOrder(t *testing.T, database *store.Store, first, second string) (low, high int64, commitLow func()) {
	t.Helper()
	ctx := t.Context()
	broker := events.NewBroker()
	appendEvent := func(tx pgx.Tx, project string) model.Event {
		t.Helper()
		event, err := broker.Append(ctx, tx, model.Event{
			ProjectKey: new(project),
			Type:       "test.concurrent",
			Actor:      model.Actor{Kind: "session", ID: "seed"},
			Payload:    map[string]any{},
		})
		if err != nil {
			t.Fatalf("append an event of %s: %v", project, err)
		}
		return event
	}
	open, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the lower id's transaction: %v", err)
	}
	t.Cleanup(func() { _ = open.Rollback(context.Background()) })
	lowEvent := appendEvent(open, first)
	committed, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the higher id's transaction: %v", err)
	}
	defer func() { _ = committed.Rollback(context.Background()) }()
	highEvent := appendEvent(committed, second)
	if err := committed.Commit(ctx); err != nil {
		t.Fatalf("commit the higher id: %v", err)
	}
	if highEvent.ID <= lowEvent.ID {
		t.Fatalf("event ids %d (open) and %d (committed) are not out of commit order, so this test proves nothing", lowEvent.ID, highEvent.ID)
	}
	return lowEvent.ID, highEvent.ID, func() {
		t.Helper()
		if err := open.Commit(ctx); err != nil {
			t.Fatalf("commit the lower id: %v", err)
		}
	}
}

// holdClusterTransactionID opens a transaction in the test cluster's postgres database and takes a
// transaction id, as a job in another database or another package's test does. It commits when
// release runs, and by the time the test ends; nothing else ends it, so a whoami that waited on it
// would never answer.
func holdClusterTransactionID(t *testing.T) (release func()) {
	t.Helper()
	config, err := pgx.ParseConfig(os.Getenv(storetest.DatabaseURLEnv))
	if err != nil {
		t.Fatalf("parse %s: %v", storetest.DatabaseURLEnv, err)
	}
	config.Database = "postgres"
	conn, err := pgx.ConnectConfig(t.Context(), config)
	if err != nil {
		t.Fatalf("connect to the cluster's postgres database: %v", err)
	}
	tx, err := conn.Begin(t.Context())
	if err != nil {
		_ = conn.Close(context.Background())
		t.Fatalf("begin the transaction in the cluster's postgres database: %v", err)
	}
	var xid string
	if err := tx.QueryRow(t.Context(), `select pg_current_xact_id()::text`).Scan(&xid); err != nil {
		_ = conn.Close(context.Background())
		t.Fatalf("take a transaction id: %v", err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			_ = tx.Commit(context.Background())
			_ = conn.Close(context.Background())
		})
	}
	t.Cleanup(release)
	return release
}

// requireEventUncommitted fails the test if the event with id has committed: the row read whoami's
// answer while the insert that holds it was open, and this says the insert was still open then.
func requireEventUncommitted(t *testing.T, database *store.Store, id int64, what string) {
	t.Helper()
	var visible bool
	if err := database.Pool.QueryRow(t.Context(), `select exists (select 1 from events where id = $1)`, id).Scan(&visible); err != nil {
		t.Fatalf("read whether %s has committed: %v", what, err)
	}
	if visible {
		t.Fatalf("%s, event %d, has committed, so this row did not hold it open while whoami answered", what, id)
	}
}

// TestWhoamiEventHeadStaysBelowAnEventStillOpen: event ids do not commit in id order, because
// writes to different owners take no lock in common (events.Broker.Append), so the newest id a read
// can see says nothing about the ids below it. A page starts its reads after whoami answers and
// resumes its first events subscription from event_head, which is replayed every event above that
// id alone: an event_head of max(id) with a lower id still open would have the page miss that event
// for good. So whoami answers the newest of the server's marks taken before every event insert
// still open, which stands below the open id. Once that write
// commits, it answers at least the higher id.
func TestWhoamiEventHeadStaysBelowAnEventStillOpen(t *testing.T) {
	database := storetest.Open(t)
	handler := storeRouter(t, database)
	createProject(t, handler, "ONE")
	createProject(t, handler, "TWO")
	// A mark taken before the write below opens, as a page's earlier load takes one: the boot mark
	// may be taken late on a loaded machine, after the write, and then no mark would count until the
	// write ends.
	askWhoami(t, handler)

	low, high, commitLow := appendOutOfOrder(t, database, "ONE", "TWO")
	if head := eventHeadID(t, askWhoami(t, handler)); head >= low {
		t.Fatalf("whoami answered event_head %d while event %d was still open below the committed %d, want an id below it", head, low, high)
	}

	commitLow()
	if head := eventHeadID(t, askWhoami(t, handler)); head < high {
		t.Fatalf("whoami answered event_head %d once every write had committed, want at least the newest id %d", head, high)
	}
}

// TestWhoamiDoesNotWaitOnATransactionInAnotherDatabase: only a transaction in Dispatch's own
// database can write its event log, so whoami's event_head waits on those alone
// (Horizon.WriterFloor), and another service's job on the same instance, or another package's
// test, does not hold every page's load for the two seconds eventFeedHead would otherwise wait --
// and the head it answers is still the log's newest id. The transaction stands until the row
// releases it after whoami's answer, so a whoami that waited on it would never answer.
func TestWhoamiDoesNotWaitOnATransactionInAnotherDatabase(t *testing.T) {
	database := storetest.Open(t)
	handler := storeRouter(t, database)
	createProject(t, handler, "ONE")
	newest := newestEventID(t, database)

	release := holdClusterTransactionID(t)
	answer := askWhoamiBeforeTheDeadline(t, handler, "whoami's answer while a transaction in another database holds a transaction id")
	if head := eventHeadID(t, answer); head != newest {
		t.Fatalf("whoami answered event_head %d while another database's transaction was open, want the log's newest id %d", head, newest)
	}
	release()
}

// TestWhoamiAnswersAtOnceWhileAWriteOfThisDatabaseIsOpen: a page's load waits on whoami, and at load
// a write of Dispatch's own database is often open, so whoami must not wait for it. A mark it took
// on an earlier call stands below every id that write holds, the write having taken its transaction
// id after that mark, so it answers from that mark at once: below the open id, and at or above
// what the earlier call answered. The write stays open to the end of the row, so a whoami that
// waited on it would never answer.
func TestWhoamiAnswersAtOnceWhileAWriteOfThisDatabaseIsOpen(t *testing.T) {
	database := storetest.Open(t)
	handler := storeRouter(t, database)
	createProject(t, handler, "ONE")
	createProject(t, handler, "TWO")
	earlier := eventHeadID(t, askWhoami(t, handler))

	low, high, _ := appendOutOfOrder(t, database, "ONE", "TWO")
	head := eventHeadID(t, askWhoamiBeforeTheDeadline(t, handler, "whoami's answer while a write of this database is open"))
	requireEventUncommitted(t, database, low, "the open write")
	if head >= low || head < earlier {
		t.Fatalf("whoami answered event_head %d while event %d was open below the committed %d, want an id below it and at or above the earlier answer %d", head, low, high, earlier)
	}
}

// TestWhoamiDoesNotSendAPageToReplayTheWholeLogWhileAWriteFromBeforeBootIsOpen: a page resumes its
// first events subscription from event_head and is replayed every event above it. On a process
// just started while a write of Dispatch's own database that took its transaction id before boot
// is still open (a restart under load, where writes hold their ids for seconds), no mark the server
// holds counts and no horizon is proven, and an event_head of 0 has every page that loads then
// replayed the whole log: on 5,000,000 events a page so resumed was sent 1,998,016 frames in its
// first 120 s and had not reached the newest. Every event that committed before that write took its
// id is final, so event_head may stand below the open write but not below those.
func TestWhoamiDoesNotSendAPageToReplayTheWholeLogWhileAWriteFromBeforeBootIsOpen(t *testing.T) {
	const committed = 1000
	database := storetest.Open(t)
	ctx := t.Context()
	if _, err := database.Pool.Exec(ctx, `
		insert into events (seq, type, actor, payload, notify, published_at)
		select 0, 'test.filler', '{"kind":"system","id":"test"}', '{}', false, now()
		from generate_series(1, $1)
	`, committed); err != nil {
		t.Fatalf("fill the event log: %v", err)
	}
	var final int64
	if err := database.Pool.QueryRow(ctx, `select max(id) from events`).Scan(&final); err != nil {
		t.Fatalf("read the event log's head: %v", err)
	}
	writer, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the write: %v", err)
	}
	var once sync.Once
	end := func() { once.Do(func() { _ = writer.Rollback(context.Background()) }) }
	t.Cleanup(end)
	if _, err := writer.Exec(ctx, `select pg_current_xact_id()`); err != nil {
		t.Fatalf("take the write's transaction id: %v", err)
	}
	// The write took its id before the server below takes its boot mark, and ends three seconds
	// later, as a write holding its id for seconds does.
	handler := storeRouter(t, database)
	ended := time.AfterFunc(3*time.Second, end)
	t.Cleanup(func() { ended.Stop() })
	if head := eventHeadID(t, askWhoami(t, handler)); head < final {
		t.Fatalf("whoami answered event_head %d while a write from before boot was open, want at least %d, the newest of the events that committed before it: the page would be replayed the whole log", head, final)
	}
}

// TestWhoamiAnswersThePersistedHorizonWhileAnEventInsertFromBeforeBootIsOpen: after a restart under
// load an event insert can be open since before the new process took any mark, so no mark counts
// until it ends, and the process has proven no horizon of its own yet. Every event at or below the
// horizon the process before it persisted is final, so whoami answers that at once, below the open
// event, rather than waiting for the insert or answering 503. The insert stays open to the end of
// the row, so a whoami that waited on it would never answer.
func TestWhoamiAnswersThePersistedHorizonWhileAnEventInsertFromBeforeBootIsOpen(t *testing.T) {
	database := storetest.Open(t)
	ctx := t.Context()
	for _, key := range []string{"ONE", "TWO"} {
		if _, err := database.Pool.Exec(ctx, `
			insert into events (seq, type, actor, payload, notify, published_at)
			values (0, 'test.filler', '{"kind":"system","id":"test"}', jsonb_build_object('key', $1::text), false, now())
		`, key); err != nil {
			t.Fatalf("seed an event: %v", err)
		}
	}
	newest := newestEventID(t, database)
	before := events.NewHorizon(database.Pool)
	before.PersistProven()
	horizontest.AwaitPersisted(t, before, newest, database)

	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the event insert: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	var open int64
	if _, err := tx.Exec(ctx, `select pg_current_xact_id()`); err != nil {
		t.Fatalf("take the insert's transaction id: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		insert into events (seq, type, actor, payload, notify)
		values (0, 'test.open', '{"kind":"system","id":"test"}', '{}', false) returning id
	`).Scan(&open); err != nil {
		t.Fatalf("insert the open event: %v", err)
	}

	handler := storeRouter(t, database)
	head := eventHeadID(t, askWhoamiBeforeTheDeadline(t, handler, "whoami's answer while an event insert from before boot is open"))
	requireEventUncommitted(t, database, open, "the event insert from before boot")
	if head < newest || head >= open {
		t.Fatalf("whoami answered event_head %d, want the persisted horizon %d or above, and below the open event %d", head, newest, open)
	}
}
