package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The issue's route is resolved through the Envoy listener before the comment's transaction
// opens, so the route can move between that resolution and the transaction's read of the issue
// under its owner lock. The locked issue row is the authority: a resolution it contradicts is taken
// again against the route the lock found, and a route that keeps moving is a conflict rather than
// a comment whose route suppression was decided from a route the issue no longer has.
//
// Each move is a route change as a PATCH makes one: the issue's owner lock and the route written,
// in a transaction of the test's own on the test's context, committed only once the comment's
// write waits behind that lock (storetest.WaitForQueuedBehind, on the one connection the comment
// runs on). So the comment resolved the route before the move and reads it after the move however
// slow the machine is. The listener only answers. A move it made itself, inside the lookup, would
// run on that lookup's request context, which in production ends when Dispatch stops waiting for
// the lookup (mentionLookupTimeout, 250 ms): a write slower than that was cancelled, and one that
// started later than that raced the comment's locked read. So the moves are made outside the
// lookup, and this test's lookups and the listener client's window run on a clock that never
// fires, so every resolution here is the listener's answer.
func TestCommentRouteResolutionIsVerifiedAgainstTheLockedIssue(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	var lookupsMu sync.Mutex
	var lookups []string
	readLookups := func() []string {
		lookupsMu.Lock()
		defer lookupsMu.Unlock()
		return slices.Clone(lookups)
	}
	const sessions = `[{"session_id":"s1","title":"planner","capabilities":["btw","steer"]}]`
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/roles/"):
			role := strings.TrimPrefix(r.URL.Path, "/v1/roles/")
			lookupsMu.Lock()
			lookups = append(lookups, role)
			lookupsMu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"role": role, "holder": "s1", "title": "planner", "capabilities": []string{"btw", "steer"},
			})
		case r.URL.Path == "/v1/registry/s1":
			writeSessionRecord(t, w, sessions, "s1")
		case r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(sessions))
		case r.URL.Path == "/v1/messages/send":
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	// A clock whose timers never fire: each route lookup is answered as the listener answers it,
	// and is never cut short by its bound (mentionLookupTimeout) on a loaded machine.
	handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.URL, clock: clocktest.Held()})
	issue := createInteractionIssue(t, handler, "TEST", "Route moves mid-resolution", "before")
	patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{
		"route": "role:reviewer",
	}, "alice")
	if patched.Code != http.StatusOK {
		t.Fatalf("set route: status=%d body=%s", patched.Code, patched.Body.String())
	}
	// beginRouteChange opens a route change's transaction; takeLock takes the issue's owner lock in
	// it, and moveRoute writes the route it moves to.
	beginRouteChange := func() pgx.Tx {
		change, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin a route change: %v", err)
		}
		t.Cleanup(func() { _ = change.Rollback(context.Background()) })
		return change
	}
	takeLock := func(change pgx.Tx) error {
		return store.LockOwner(ctx, change, store.IssueOwner(issue.Key))
	}
	moveRoute := func(change pgx.Tx, route string) error {
		_, err := change.Exec(ctx, `update issues set route = $2 where key = $1`, issue.Key, route)
		return err
	}
	// queuedBehind returns once the comment's backend waits behind the route change, and fails the
	// row when the comment answers first: it never read the route under its lock after the move.
	// Its one bound is the test binary's deadline (storetest.WaitForQueuedBehind).
	queuedBehind := func(change pgx.Tx, callPID uint32, answered <-chan *httptest.ResponseRecorder, what string) {
		t.Helper()
		if response, early := storetest.WaitForQueuedBehind(t, change, callPID, answered); early {
			t.Fatalf("%s answered %d before its write waited behind the route change: %s", what, response.Code, response.Body.String())
		}
	}

	// One move: the redo resolves the route the locked row actually carries.
	callHandler, callPID := oneConnectionHandler(t, deps)
	change := beginRouteChange()
	if err := takeLock(change); err != nil {
		t.Fatalf("take the issue's owner lock for the route change: %v", err)
	}
	if err := moveRoute(change, "role:auditor"); err != nil {
		t.Fatalf("move the route to role:auditor: %v", err)
	}
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- postMentionedComment(t, callHandler, issue.Key, map[string]any{
			"body": "Mentioning the session the new route holds.", "mentions": []map[string]any{{"target": "session:s1"}},
		})
	}()
	queuedBehind(change, callPID, answered, "the comment across one route move")
	if err := change.Commit(ctx); err != nil {
		t.Fatalf("commit the move to role:auditor: %v", err)
	}
	created := testwait.Receive(t, "the answer of the comment across one route move", answered)
	if created.Code != http.StatusCreated {
		t.Fatalf("comment across one route move: status=%d body=%s", created.Code, created.Body.String())
	}
	settledCommentDeliveries(t, database, decodeMentionedComment(t, created).ID)
	if got := readLookups(); !slices.Equal(got, []string{"reviewer", "auditor"}) {
		t.Fatalf("route lookups = %#v, want the stale reviewer then the locked auditor", got)
	}
	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK || !strings.Contains(events.Body.String(), `"suppressed_route":"role:auditor"`) {
		t.Fatalf("suppression after the route moved: status=%d body=%s", events.Code, events.Body.String())
	}

	// A route that keeps moving is refused rather than decided from a stale resolution: each of the
	// two resolutions finds the route moved under the lock. The second change asks for the lock
	// once the comment's first write waits for it, so that write is granted it first and reads the
	// first move, and the second change then holds the lock while the comment resolves again.
	lookupsMu.Lock()
	lookups = nil
	lookupsMu.Unlock()
	callHandler, callPID = oneConnectionHandler(t, deps)
	first := beginRouteChange()
	if err := takeLock(first); err != nil {
		t.Fatalf("take the issue's owner lock for the first move: %v", err)
	}
	if err := moveRoute(first, "role:moved-1"); err != nil {
		t.Fatalf("move the route to role:moved-1: %v", err)
	}
	refused := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		refused <- postMentionedComment(t, callHandler, issue.Key, map[string]any{
			"body": "The route will not hold still.", "mentions": []map[string]any{{"target": "session:s1"}},
		})
	}()
	queuedBehind(first, callPID, refused, "the comment's first resolution")
	second := beginRouteChange()
	secondPID := second.Conn().PgConn().PID()
	secondMoved := make(chan error, 1)
	go func() {
		err := takeLock(second)
		if err == nil {
			err = moveRoute(second, "role:moved-2")
		}
		secondMoved <- err
	}()
	testwait.Until(t, "the second route change to queue behind the comment's first write", func() (string, bool) {
		queued, err := storetest.QueuedBehind(ctx, first, secondPID)
		if err != nil {
			return err.Error(), false
		}
		return fmt.Sprintf("backend %d queued: %t", secondPID, queued), queued
	})
	if err := first.Commit(ctx); err != nil {
		t.Fatalf("commit the move to role:moved-1: %v", err)
	}
	if err := testwait.Receive(t, "the second route change's lock and move to role:moved-2", secondMoved); err != nil {
		t.Fatalf("move the route to role:moved-2: %v", err)
	}
	queuedBehind(second, callPID, refused, "the comment's second resolution")
	if err := second.Commit(ctx); err != nil {
		t.Fatalf("commit the move to role:moved-2: %v", err)
	}
	conflict := testwait.Receive(t, "the answer of the comment across a moving route", refused)
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), "ROUTE_CHANGED") {
		t.Fatalf("comment across a moving route: status=%d body=%s", conflict.Code, conflict.Body.String())
	}
	if got := readLookups(); !slices.Equal(got, []string{"auditor", "moved-1"}) {
		t.Fatalf("route lookups = %#v, want the resolution taken exactly twice before the conflict: auditor, then the first move's moved-1", got)
	}
}
