package docs

import (
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/goroutinetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// persistBuffer is the size of ygo's per-room persistence channel: the number of committed updates
// the room's persistence worker could be behind before a commit's hand-off to it had to wait.
const persistBuffer = 256

// fillPersistBuffer commits as many peer updates into live as ygo's persistence channel holds.
func fillPersistBuffer(live *crdt.Doc) {
	for index := range persistBuffer {
		live.Transact(func(tx *crdt.Transaction) { tx.GetMap("backpressure").Set(tx, "counter", index) }, "peer")
	}
}

// A settlement commits its block-id stamp into the room while it holds the document's advisory
// lock, and the room's persistence worker, behind a full buffer of browser commits, waits on that
// same lock to store a browser's edit. The stamp's commit does not wait on the worker: ygo hands an
// update to a worker that is behind without waiting for it (persistUpdate). Before that, the
// commit blocked on the full buffer while the worker blocked on the lock the committer held, and
// neither moved until the lock holder's database session was ended (INFRA-6061's lock path).
func TestASettlementsCommitDoesNotWaitOnAWorkerWaitingOnItsLock(t *testing.T) {
	// Not parallel: settleWithin reads every goroutine's stack in the process when it fails.
	service, room := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, room, "before")
	settleCurrentGeneration(t, service, room)
	editLiveTree(t, service, room, appendUnidentifiedBlocks(t, "added"))
	live := service.srv.GetDoc(room)
	var held bool
	service.afterSettleLock = func(got string) {
		if got != room || held {
			return
		}
		held = true
		// A browser's edit: the worker's append of it waits on the lock this settlement holds.
		editAsPeer(t, service, room, replaceRun("before", "before, edited"))
		testwait.Eventually(t, "the worker's append to wait on the document's lock", func() bool { return waitingAdvisoryLocks(t, service.store) > 0 })
		fillPersistBuffer(live)
	}
	settleWithin(t, service, room, func() {
		// Ending the lock holder's session lets the worker take the lock, and the cycle unwinds.
		endRoomLockHolders(t, service)
	})
	service.afterSettleLock = nil
	if !held {
		t.Fatal("the settlement never took the document's lock")
	}
	if failure := service.roomFailure(room); failure != nil {
		t.Fatalf("the room failed: %v", failure)
	}
	waitForPersistedProofText(t, service.store, room, "before, edited\n\nadded\n")
}

// A settlement whose stamp's update the room's worker is waiting on - the worker waits for the
// stamp's suppression slot, which the settlement finishes only at its commit (finishSlots) - writes
// its second repair into the room behind a full buffer of browser commits. The second repair's
// commit does not wait on the worker (persistUpdate). Before that, it blocked on the full buffer
// while the worker waited on the slot only the blocked settlement could finish, and neither moved
// until the room's slots were purged (INFRA-6061's slot path).
func TestASettlementsSecondRepairDoesNotWaitOnAWorkerWaitingOnItsStamp(t *testing.T) {
	// Not parallel: settleWithin reads every goroutine's stack in the process when it fails.
	service, room := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, room, ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n")
	service.settleRoom(room, 0)
	answerBlockAsk(t, service, room)
	editLiveTree(t, service, room, appendUnidentifiedBlocks(t, "added"))
	live := service.srv.GetDoc(room)
	var held bool
	service.afterSettleReconcile = func(got string) {
		if got != room || held {
			return
		}
		held = true
		testwait.Eventually(t, "the worker to take the stamp's update", func() bool {
			state := service.lockExistingState(room)
			if state == nil {
				return false
			}
			defer service.unlockState(room, state)
			for _, class := range state.updateClasses {
				if !class.durable {
					return class.handed
				}
			}
			return false
		})
		fillPersistBuffer(live)
	}
	settleWithin(t, service, room, func() { service.purgeSuppressedPersistence(room) })
	service.afterSettleReconcile = nil
	if !held {
		t.Fatal("the settlement never reached the window between its stamp and its repairs")
	}
	if failure := service.roomFailure(room); failure != nil {
		t.Fatalf("the room failed: %v", failure)
	}
	requireNoSuppressedSlots(t, service, room, "after the settlement")
}

// A connected browser's next edit waits, before the room applies it, while the room's updates queue
// past ygo's persistence channel, so storage lag still slows a fast browser: ygo's read loop waits
// there for the updates queued before it to be handed on (awaitPersistence), holding nothing a
// committer or the worker needs. The room's worker waits on the document's lock to store the
// browser's first edit, a full buffer of commits stands behind it, and one more queues past the
// buffer. The browser's next edit is not applied while the lock is held; once the lock is released,
// the room applies it and stores it. A read loop that applied it at once would let every browser
// grow the queue for as long as storage lags (INFRA-6061's R2).
func TestABrowsersEditWaitsWhileTheRoomsUpdatesQueuePastItsBuffer(t *testing.T) {
	// Not parallel: it reads every goroutine's stack in the process to find the browser's read loop
	// waiting in ygo.
	service, room := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, room, "before")
	browser := connectBrowser(t, newTestServer(t, service).URL, room, "carol")
	live := service.srv.GetDoc(room)
	unlock := holdDocumentLock(t, service.store, room)
	editAsBrowser(t, service, room, browser, "before, edited\n")
	testwait.Eventually(t, "the worker's append to wait on the document's lock", func() bool { return waitingAdvisoryLocks(t, service.store) > 0 })
	queued := make(chan struct{})
	go func() {
		defer close(queued)
		fillPersistBuffer(live)
		live.Transact(func(tx *crdt.Transaction) { tx.GetMap("backpressure").Set(tx, "queued", true) }, "peer")
	}()
	select {
	case <-queued:
	case <-time.After(roomWait(t)):
		unlock()
		testwait.Receive(t, "the commits past the buffer once the lock was released", queued)
		t.Fatalf("the commits past the buffer did not return within %s: a commit into the room waited on its persistence worker", roomWait(t))
	}

	applied, unsubscribe := sendAsBrowser(t, service, room, browser, "before, edited\n\nafter\n")
	defer unsubscribe()
	waiting := false
	testwait.Eventually(t, "the browser's read loop to wait in ygo's awaitPersistence, or the room to apply the browser's edit", func() bool {
		waiting = readLoopWaitsOnPersistence()
		return waiting || isClosed(applied)
	})
	if !waiting || isClosed(applied) {
		t.Fatal("the room applied the browser's edit while its updates queued past the persistence channel: storage lag no longer slows a browser")
	}
	unlock()
	testwait.Receive(t, "the room to apply the browser's edit once its worker could store again", applied)
	if failure := service.roomFailure(room); failure != nil {
		t.Fatalf("the room failed: %v", failure)
	}
	waitForPersistedProofText(t, service.store, room, "before, edited\n\nafter\n")
}

// readLoopWaitsOnPersistence reports whether a goroutine of the process is a peer's read loop
// waiting, before it applies the peer's next update, for the updates queued past its room's
// persistence channel to be handed on.
func readLoopWaitsOnPersistence() bool {
	for _, stack := range goroutinetest.Traces() {
		if strings.Contains(stack, "provider/websocket.(*room).awaitPersistence") && strings.Contains(stack, "provider/websocket.(*peer).applySyncUpdate") {
			return true
		}
	}
	return false
}

// isClosed reports whether ch is closed.
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// settleWithin settles room's current generation and fails the test if the settlement has not
// returned within roomWait, naming the goroutine parked in ygo's persistence hand-off, the one a
// commit that waits on the worker parks in. unwind breaks the cycle so the test binary is not held
// by it.
func settleWithin(t *testing.T, service *Service, room string, unwind func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		settleCurrentGeneration(t, service, room)
	}()
	select {
	case <-done:
		return
	case <-time.After(roomWait(t)):
	}
	var parked string
	for _, stack := range goroutinetest.Traces() {
		if strings.Contains(stack, "provider/websocket.(*Server).loadRoom") && strings.Contains(stack, "settle") {
			parked = stack
		}
	}
	unwind()
	testwait.Receive(t, "the settlement once the cycle was broken", done)
	t.Fatalf("the settlement did not return within %s: a commit into the room waited on its persistence worker\n%s", roomWait(t), parked)
}
