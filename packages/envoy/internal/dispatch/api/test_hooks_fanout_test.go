package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// holdTestHookLock is a process that is alive but never acknowledges a test hook: a connection of
// its own holding the lock a process with the hooks on holds, keyed as each process keys it, by
// its own backend pid, and listening on nothing. It answers that key; the lock goes when the
// returned connection closes, as a killed process's goes with its connection.
func holdTestHookLock(t *testing.T, database *store.Store) (*pgx.Conn, int32) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, database.Pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatalf("open the silent process's connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	key := int32(conn.PgConn().PID())
	if _, err := conn.Exec(ctx, `select pg_advisory_lock($1, $2)`, testHookLockNamespace, key); err != nil {
		t.Fatalf("take the silent process's test-hook lock: %v", err)
	}
	return conn, key
}

// A hook answers only once every process holding the test-hook lock has acted, so a process that
// holds it and never acknowledges is reported rather than passed over: the caller answers
// 504 TEST_HOOK_TIMEOUT once the timeout has run out, naming that process's key. The timeout runs
// on the server's clock, a fake the test moves to it - and only once the call waits on the silent
// process alone (awaitHookWaitingOnOthers): moved as soon as the timeout is armed, it ends a call
// that has not yet waited on anyone, which answers that this process's listener did not take its
// lock.
func TestATestHookNamesTheProcessThatNeverAcknowledged(t *testing.T) {
	t.Parallel()
	fake := clocktest.New(time.Now())
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{testHooks: true, clock: fake})
	_, silent := holdTestHookLock(t, database)

	deadline := fake.Now().Add(testHookTimeout)
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/live/_test/disconnect", nil, "alice")
	}()
	awaitHookWaitingOnOthers(t, fake, served, deadline, answered)
	fake.Set(deadline)
	response := testwait.Receive(t, "the hook's answer once its timeout has run out", answered)
	if response.Code != http.StatusGatewayTimeout {
		t.Fatalf("disconnect hook with a silent process: status=%d body=%s, want 504", response.Code, response.Body.String())
	}
	body := decodeBody[struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}](t, response)
	if body.Code != "TEST_HOOK_TIMEOUT" || !strings.Contains(body.Error, strconv.Itoa(int(silent))) {
		t.Fatalf("timeout answer = %+v, want TEST_HOOK_TIMEOUT naming the silent process %d", body, silent)
	}
}

// awaitHookWaitingOnOthers waits until the hook call whose answer comes on answered has its
// timeout armed for deadline on fake, the server's clock, and holds this process's own
// acknowledgement: from then on it waits on the other processes holding the test-hook lock alone.
// The timeout is armed before the call waits for this process's own listener to hold its lock and
// before the call is sent (testHookFanout.call), so the timeout alone says neither. It is one wait,
// which fails the test at once should the call answer: a call that answered before its timeout
// disarms that timeout as it returns, so a wait for the timeout alone would outlast such a call for
// the test's whole deadline.
func awaitHookWaitingOnOthers(
	t *testing.T, fake *clocktest.Fake, served *server, deadline time.Time, answered <-chan *httptest.ResponseRecorder,
) {
	t.Helper()
	unanswered := func() {
		t.Helper()
		select {
		case response := <-answered:
			t.Fatalf("the hook answered %d while another process still owed its acknowledgement: %s",
				response.Code, response.Body.String())
		default:
		}
	}
	hooks := served.testHooks
	testwait.Until(t, "the hook call's timeout armed for "+deadline.Format(time.RFC3339Nano)+
		" and the call holding this process's own acknowledgement", func() (hookCallWait, bool) {
		unanswered()
		read := hookCallWait{Armed: fake.Armed()}
		hooks.mu.Lock()
		for _, wait := range hooks.waiting {
			if _, acked := wait.acks[hooks.key]; acked && hooks.key != 0 {
				read.OwnAcknowledged = true
			}
		}
		hooks.mu.Unlock()
		return read, read.OwnAcknowledged && slices.ContainsFunc(read.Armed, deadline.Equal)
	})
	unanswered()
}

// hookCallWait is what awaitHookWaitingOnOthers reads of a hook call: the deadlines pending on the
// server's clock, and whether the call holds this process's own acknowledgement.
type hookCallWait struct {
	Armed           []time.Time
	OwnAcknowledged bool
}

// A process that dies while a call waits for it takes its lock with its connection, and the call
// stops waiting for it: it answers once the processes still holding the lock have acknowledged,
// not at the timeout, which the server's clock, a fake, never reaches here. Until then it does not
// answer, since that process was alive when the call was sent. The call polls the held locks on
// that clock (testHookPoll), and the row moves it to each poll: the call arms its next poll only
// once a poll has returned and the call has gone round again owing an acknowledgement, so the
// third poll armed shows the first two found the silent process's lock held and left the call
// waiting on it. A call that passed over a live process answers from the first poll instead,
// which the wait for each arm fails on at once.
func TestATestHookStopsWaitingForAProcessWhoseLockIsGone(t *testing.T) {
	t.Parallel()
	fake := clocktest.New(time.Now())
	started := fake.Now()
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{testHooks: true, clock: fake})
	conn, silent := holdTestHookLock(t, database)

	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/live/_test/disconnect", nil, "alice")
	}()
	awaitHookWaitingOnOthers(t, fake, served, started.Add(testHookTimeout), answered)
	polled := func(at time.Time) {
		t.Helper()
		testwait.Until(t, "the call's poll of the held locks armed for "+at.Format(time.RFC3339Nano), func() ([]time.Time, bool) {
			select {
			case response := <-answered:
				t.Fatalf("the hook answered %d while the silent process still held its lock: %s",
					response.Code, response.Body.String())
			default:
			}
			armed := fake.Armed()
			return armed, slices.ContainsFunc(armed, at.Equal)
		})
	}
	poll := started.Add(testHookPoll)
	for range 2 {
		polled(poll)
		fake.Set(poll)
		poll = poll.Add(testHookPoll)
	}
	polled(poll)
	_ = conn.Close(context.Background())
	// The lock goes once the server has ended the closed connection's backend, which the next poll
	// must find gone.
	testwait.Until(t, "the silent process's lock to go with its connection", func() ([]int32, bool) {
		held, err := served.testHooks.heldKeys(context.Background())
		if err != nil {
			t.Fatalf("read the held test-hook locks: %v", err)
		}
		return held, !slices.Contains(held, silent)
	})
	fake.Set(poll)
	response := testwait.Receive(t, "the hook's answer once the silent process's lock went", answered)
	if response.Code != http.StatusOK {
		t.Fatalf("disconnect hook after the silent process died: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
}

// secondHookProcess is a second server process on database: a pool of its own (and so a listening
// connection and a lock of its own), its own document service and its own in-process conversation
// relay, as a second envoy-dispatch process holds. It answers that process's server once that
// process hears test hooks. register starts the process's listener and returns before it has
// dialed, and a call waits only for the processes holding a test-hook lock when it is sent
// (testHookFanout.call): a call sent sooner answers 200 without this process, which never hears
// it, so a viewer here waits for a frame that never comes. The process takes its lock only once
// it listens (testHookFanout.dial), so from then on every call reaches it and waits for its
// acknowledgement. The wait is the one the caller makes for its own lock (awaitOwnLock), bounded
// by the test binary's deadline (testwait.Receive): how long a loaded machine takes to connect is
// not under test.
func secondHookProcess(t *testing.T, database *store.Store) *httptest.Server {
	t.Helper()
	second := storetest.SecondProcess(t, database)
	broker := events.NewBroker()
	documents := docs.New(docs.Deps{Store: second, Events: broker, ServerURL: "https://dispatch.example", Settle: 20 * time.Millisecond})
	t.Cleanup(func() { _ = docstest.Shutdown(t, context.Background(), documents) })
	deps, err := NewDeps(DepsInput{
		Store:            second,
		Identity:         headerIdentity(second),
		AgentTokens:      sharedAgentTokens(t, "agent-token"),
		ServerURL:        "https://dispatch.example",
		Docs:             documents,
		Events:           broker,
		TestHooksEnabled: true,
		AgentStream:      agentstream.NewMemory(),
	})
	if err != nil {
		t.Fatalf("second process's dependencies: %v", err)
	}
	mux := http.NewServeMux()
	served := register(mux, t.Context(), deps)
	t.Cleanup(func() { waitBackgroundOf(t, served) })
	listening := testwait.Context(t)
	if _, err := served.testHooks.awaitOwnLock(listening); err != nil {
		t.Fatalf("the second process's test-hook listener did not take its lock: %v", err)
	}
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// A published frame travels to every process inside the hook's notification, and one
// notification carries less than Postgres's 8000-byte payload. A frame several times that - a
// replay of a long transcript, which the browser suite sets - reaches a viewer on the other
// process byte for byte, multi-byte characters included, whatever notification boundary they fall
// on.
func TestAFrameLargerThanOneNotificationReachesAViewerOnTheOtherProcessWhole(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{testHooks: true, agentStream: agentstream.NewMemory()})
	other := secondHookProcess(t, database)

	viewer := openLiveSocketAt(t, gws.DefaultDialer, "ws"+strings.TrimPrefix(other.URL, "http")+"/ws/live", asUser("alice")).read()
	viewer.subscribeAgent(t, "agent", plannerSessionID)
	if data := viewer.agentFrame(t, "agent", "responder"); data == "" {
		t.Fatal("the viewer's first frame is not the responder verdict")
	}

	frame := map[string]any{"v": 1, "kind": "message", "text": strings.Repeat("ünïcødé ✓ the relay carries this turn. ", 700)}
	want, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("encode the frame: %v", err)
	}
	if len(want) < 3*8000 {
		t.Fatalf("the frame is %d bytes, want several notifications' worth", len(want))
	}
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/"+plannerSessionID+"/stream/_test/publish", frame, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("publish a %d-byte frame: status=%d body=%s", len(want), response.Code, response.Body.String())
	}
	if data := viewer.agentFrame(t, "agent", "frame"); data != string(want) {
		t.Fatalf("the other process's viewer read %d bytes, want the %d-byte frame whole", len(data), len(want))
	}
}

// A call's part count is read off the notification, which any connection to the database can
// send, so a process caps it at the most any call is split into before it allocates a slot per
// part. The cap still admits the largest call a request can make: a frame of maxJSONRequestBytes
// made wholly of a byte json.Marshal escapes widest, to a session id in its longest form.
func TestAHookNotificationNamingMorePartsThanAnyCallIsDropped(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(testHookCall{
		Hook: hookPublishFrame, Session: "urn:uuid:" + plannerSessionID, As: "replay",
		Frame: strings.Repeat("\x01", int(maxJSONRequestBytes)),
	})
	if err != nil {
		t.Fatalf("encode the largest call: %v", err)
	}
	largest := splitTestHookCall("largest", string(encoded))
	if len(largest) > testHookMaxParts {
		t.Fatalf("the largest call a request can make is split into %d parts, past the cap of %d", len(largest), testHookMaxParts)
	}
	fanout := &testHookFanout{partial: map[string]*testHookParts{}}
	fanout.heard(largest[0])
	if partial := fanout.partial["largest"]; partial == nil || len(partial.pieces) != len(largest) || partial.have != 1 {
		t.Fatalf("the largest call's first part was not kept for its %d parts: %#v", len(largest), partial)
	}

	fanout.heard("too-many 1 " + strconv.Itoa(testHookMaxParts+1) + " {")
	if partial, kept := fanout.partial["too-many"]; kept {
		t.Fatalf("a notification naming %d parts, one past the cap, was kept for %d parts", testHookMaxParts+1, len(partial.pieces))
	}
}
