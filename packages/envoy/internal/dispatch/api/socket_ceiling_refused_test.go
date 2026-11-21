package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A socket whose caller's heartbeat checks find the pool busy heartbeatTransientCeiling times in a
// row while its document loads is closed by the heartbeat, with a cause of the server's own
// (errCallerNoLongerResolves), and its browser - still waiting for its handshake's answer - is
// refused 503 with Retry-After, never answered the empty 200 logged 499 of a browser that left.
// What tells the two apart is the cause the request's context ended with (refuseAdmission:
// only net/http's plain context.Canceled is a client gone). The row beside it
// (TestASocketWhoseCallerStopsResolvingDuringTheLoadIsAnsweredAsItsLoadAndNotAsAGoneClient) stops
// short of the ceiling, so it never ends the context and passes with that check taken back to
// r.Context().Err() != nil; this one reaches the ceiling, so it fails then.
func TestASocketWhoseHeartbeatReachesItsCeilingDuringTheLoadIsRefused503AndNotAGoneClient(t *testing.T) {
	const heartbeat = 250 * time.Millisecond
	database := storetest.Open(t)
	caller := &busyCallerIdentity{Identity: headerIdentity(database)}
	held := &heldSocketLoadStore{loading: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-held.release:
		default:
			close(held.release)
		}
	})
	handler, _, deps := newTestServer(t, testServerOptions{
		store: database, identity: caller, browserPeers: true, settle: time.Hour, streamHeartbeat: heartbeat,
		persistence: func(database *store.Store) docs.VersionedStore {
			held.VersionedStore = docs.NewPgVersioned(database)
			return held
		},
	})
	room := createInteractionIssue(t, handler, "CEIL", "Heartbeat ceiling during the load", "Before.\n").PrimaryArtifactID
	if err := deps.Docs.(*docs.Service).Evict(context.Background(), room); err != nil {
		t.Fatalf("evict the document's room: %v", err)
	}
	logged := captureLog(t)
	held.hold(room)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/doc/" + room + "?schema_version=" + strconv.Itoa(pmdoc.SchemaVersion())
	type handshake struct {
		status     int
		retryAfter string
		err        error
	}
	answered := make(chan handshake, 1)
	go func() {
		connection, response, err := gws.DefaultDialer.Dial(url, http.Header{"X-Dispatch-User": []string{"alice"}})
		if connection != nil {
			_ = connection.Close()
		}
		got := handshake{err: err}
		if response != nil {
			got.status, got.retryAfter = response.StatusCode, response.Header.Get("Retry-After")
			_ = response.Body.Close()
		}
		answered <- got
	}()
	select {
	case <-held.loading:
	case <-time.After(30 * time.Second):
		t.Fatal("the socket's admission never loaded its document, so nothing here is tested")
	}

	// Every check from here finds the pool busy, so the heartbeat reaches its ceiling while the
	// load is held and the browser is still waiting.
	caller.busy.Store(1 << 20)
	var got handshake
	select {
	case got = <-answered:
	case <-time.After(30 * time.Second):
		t.Fatal("the socket's handshake was never answered after its heartbeat reached the ceiling")
	}
	if cause := context.Cause(held.ctx); cause == nil || cause == context.Canceled {
		t.Fatalf("the held load's context ended with %v, want the heartbeat's own cause, so nothing here is tested", cause)
	}
	if got.status != http.StatusServiceUnavailable {
		t.Errorf("the waiting browser's handshake was answered %d (%v), want the 503 of a refused socket", got.status, got.err)
	}
	if got.retryAfter != admission.RetryAfter {
		t.Errorf("the refused socket's Retry-After is %q, want %q", got.retryAfter, admission.RetryAfter)
	}
	var access string
	pollUntil(t, 10*time.Second, "the socket's access-log line", func() bool {
		for _, line := range accessLines(t, logged) {
			if strings.Contains(line, "path=/ws/doc/"+room) {
				access = line
				return true
			}
		}
		return false
	})
	if !strings.Contains(access, "status=503") {
		t.Errorf("the socket's access-log line %q does not carry status=503, the refusal its browser read", access)
	}
}
