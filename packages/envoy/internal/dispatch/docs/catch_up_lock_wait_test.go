package docs

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A catch-up's confirmation of a replaced history under the document's lock
// (historyReplacedUnderLock) is no request's work: the room-sync catch-up lane runs it with no
// request's ticket on its context, so it waits for the lock's holder for as long as the holder
// holds it, as every taker of the document's lock without a ticket does (lockDocumentRoom). The
// pool its transaction runs on must not bound that wait either: on the request pool, whose every
// transaction carries the write lock bound by default (store.Pool's begin), the confirmation was
// refused 55P03 at that bound while the holder still held the lock (INFRA-592, tester round 48).
// The test's own bound on the context stands for the write lock bound, so the row runs in a
// second rather than in ten.
func TestACatchUpsConfirmationUnderTheDocumentLockWaitsForTheHolder(t *testing.T) {
	const bound = 300 * time.Millisecond
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	service, _ := newTestServiceInstance(t, database)
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	ctx := context.Background()
	if err := service.warmLiveDocument(ctx, artifactID); err != nil {
		t.Fatalf("load the room: %v", err)
	}
	waitForLandedAppends(t, service, artifactID)
	room := service.srv.GetDoc(artifactID)
	if service.cursorOf(room) == nil {
		t.Fatal("the room has no cursor, so its confirmation would take no lock and nothing here is tested")
	}

	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction that holds the document's lock: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if err := lockDocumentRoom(ctx, holder, artifactID); err != nil {
		t.Fatalf("hold the document's lock: %v", err)
	}

	type answer struct {
		replaced bool
		err      error
	}
	answered := make(chan answer, 1)
	started := time.Now()
	go func() {
		replaced, err := service.historyReplacedUnderLock(store.WithLockWaitForTest(ctx, bound), artifactID, room)
		answered <- answer{replaced, err}
	}()
	if waiting := storetest.WaitForLockWaiters(t, holder, 1, roomWait(t)); waiting < 1 {
		t.Fatal("the confirmation never came to wait on the document's lock")
	}
	select {
	case got := <-answered:
		t.Fatalf("the catch-up's confirmation answered (replaced %v, err %v) %s after it began, while the holder still held the "+
			"document's lock: a taker with no request's ticket must wait for the holder",
			got.replaced, got.err, time.Since(started).Round(time.Millisecond))
	case <-time.After(4 * bound):
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("let the document's lock go: %v", err)
	}
	select {
	case got := <-answered:
		if got.err != nil || got.replaced {
			t.Fatalf("the confirmation once the lock was free answered (replaced %v, err %v), want a history that was not replaced", got.replaced, got.err)
		}
	case <-time.After(roomWait(t)):
		t.Fatal("the confirmation never answered once the document's lock was free")
	}
}
