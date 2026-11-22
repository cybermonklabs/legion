package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A thread mark that moves its row takes the project's lock before the row's update
// (putThreadRead), so when another writer of the project holds that lock the mark waits for it
// holding no transaction id. The shape it replaced updated the row first and met the project's
// lock inside the event's append, which waits with a transaction id already open - the wait
// INFRA-636 exists to take off the transaction id.
//
// The window test's thread-read step (xidWindowUserThreadRead) cannot tell the two apart: with
// nobody holding the project's lock both run the same three statements inside their window, the
// update, the batch appending the event (whose first statement is that lock) and the COMMIT, and
// a wait inside the append is statement time, not a gap. Only holding the lock and asking what
// the waiter holds does.
func TestAThreadMarkWaitsForTheProjectLockHoldingNoTransactionID(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "TEST", "Mark behind the project", "A spec")
	thread := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Waiting"}, "alice")
	// A session's reply leaves Alice's row unread, so her mark moves it and takes the project's
	// lock; a mark that moves nothing stops before that lock.
	sessionComment(t, handler, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{"body": "Reply", "reply_to": thread.ID})

	ctx := context.Background()
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder transaction: %v", err)
	}
	defer holder.Rollback(ctx)
	if err := store.LockOwner(ctx, holder, store.ProjectOwner("TEST")); err != nil {
		t.Fatalf("hold the project's lock: %v", err)
	}

	// The mark runs on a connection of its own, so the wait watches its backend alone
	// (storetest.WaitForQueuedBehind) and the transaction id read is that backend's: no other
	// backend queued on the project's lock, the server's own among them, is taken for the mark.
	markHandler, markPID := oneConnectionHandler(t, deps)
	mark := time.Now()
	marks := make(chan *httptest.ResponseRecorder, 1)
	go func() { marks <- markThreadRead(t, markHandler, "alice", thread.ID, mark) }()
	if response, early := storetest.WaitForQueuedBehind(t, holder, markPID, marks); early {
		t.Fatalf("the thread mark answered %d %s while this test held the project's lock: it moved the row "+
			"without the lock its event's append takes", response.Code, response.Body.String())
	}
	if holdingXid, err := storetest.HoldsTransactionID(ctx, database, markPID); err != nil {
		t.Fatal(err)
	} else if holdingXid {
		t.Fatal("the thread mark waits for the project's lock with a transaction id already open: it updated " +
			"its row before it took that lock, so the wait holds back every snapshot's horizon. Take the " +
			"project's lock (store.LockOwner, store.ProjectOwner) before the update.")
	}

	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release the project's lock: %v", err)
	}
	if marked := awaitResponse(t, marks); marked.Code != http.StatusNoContent {
		t.Fatalf("the thread mark once the lock was released: status=%d body=%s, want 204", marked.Code, marked.Body.String())
	}
	if through, unread := threadMark(t, database, "alice", thread.ID); through.Before(mark.Truncate(time.Microsecond)) || unread {
		t.Fatalf("Alice's read mark is %s (unread %v), want the mark %s the route was sent", through, unread, mark)
	}
}
