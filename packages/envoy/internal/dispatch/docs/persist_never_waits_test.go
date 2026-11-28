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
