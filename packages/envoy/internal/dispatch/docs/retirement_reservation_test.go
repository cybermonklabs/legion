package docs

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A writer that opens on the document while the room a late departure's settlement loaded
// retires - once ygo's close of the room has taken it out of ygo's map and run its unload, before
// that close has returned - waits for the retirement, as a writer that opened before the close
// began does (TestAWriterOpeningAsASettlementRoomRetiresTakesUpNoRoomTheCloseTakesAway). ygo runs
// the room's unload inside its close (CloseRoomWithin), and the unload forgets a record nothing
// holds once its room has gone (releaseUnloadedRoom, releaseIfUnusedLocked). The retirement's
// reservation is one of the things that hold it (unusedLocked), so the record stays with the
// reservation on it: the writer finds the reservation, and takes no writer slot until the
// retirement has left the room and ended it (openLiveWrite). A record the unload forgot would hand
// the writer a new record holding no reservation, and the writer slot with it, while the close
// the reservation stands for has not returned.
func TestAWriterOpeningAsARetiringRoomUnloadsWaitsForTheRetirementsReservation(t *testing.T) {
	r := unloadAcrossAReload(t)
	r.releaseFirstRoom()

	forked, waiting := make(chan struct{}, 1), make(chan struct{}, 1)
	var waited atomic.Bool
	r.service.afterForkRead = func(room string) {
		if room == r.room {
			signal(forked)
		}
	}
	r.service.awaitingRetirement = func(room string) {
		if room == r.room {
			waited.Store(true)
			signal(waiting)
		}
	}
	var reserved atomic.Bool
	r.service.afterRetireReserved = func(room string) {
		if room == r.room {
			reserved.Store(true)
		}
	}
	// The writer opens once the unload of the room a retirement has reserved has run, still inside
	// ygo's close of the room. The unload returns once the writer has forked (afterForkRead), which
	// it does with the writer slot taken, or once it waits for the retirement
	// (awaitingRetirement). kept is whether the unload left the document's record holding the
	// reservation.
	wrote := make(chan error, 1)
	var opened, kept atomic.Bool
	unload := r.service.srv.OnUnloadDocument
	r.service.srv.OnUnloadDocument = func(ctx context.Context, room string) {
		unload(ctx, room)
		if room != r.room || !reserved.Load() || opened.Swap(true) {
			return
		}
		if state := r.service.lockExistingState(r.room); state != nil {
			kept.Store(state.retiring != nil)
			r.service.unlockState(r.room, state)
		}
		go func() {
			_, err := joinedReplaceText(r.service, r.room, "A write that opens as the retiring room unloads.\n", model.Actor{Kind: "user", ID: "alice"})
			wrote <- err
		}()
		select {
		case <-forked:
		case <-waiting:
		case <-time.After(roomWait(t)):
			t.Error("the writer that opened as the retiring room unloaded neither forked nor waited for the retirement")
		}
	}
	// The retirement keeps its reservation until it has left the room, which is after ygo's close
	// of the room has returned (afterRetireLeft): the writer is still waiting for it then, with no
	// writer slot.
	var left atomic.Bool
	r.service.afterRetireLeft = func(room string) {
		if room != r.room || !opened.Load() || left.Swap(true) {
			return
		}
		state := r.service.lockExistingState(r.room)
		writing := state != nil && state.liveWriter != nil
		if state != nil {
			r.service.unlockState(r.room, state)
		}
		if !waited.Load() || writing {
			t.Errorf("the writer that opened as the retiring room unloaded, inside ygo's close of it, waited for the retirement: %v, and holds the writer slot once the close has returned: %v; the unload left the document's record holding the retirement's reservation: %v; want the record kept and the writer waiting, with no slot, until the retirement ends its reservation", waited.Load(), writing, kept.Load())
		}
	}
	r.departLate()
	if !opened.Load() {
		t.Fatal("no unload of the room ran while a retirement held its reservation, so nothing here is tested")
	}
	if !left.Load() {
		t.Fatal("the retirement whose close unloaded the room never left it holding its reservation (afterRetireLeft never ran), so nothing here is tested")
	}
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatalf("the write that opened as the retiring room unloaded: %v", err)
		}
	case <-time.After(roomWait(t)):
		t.Fatal("the write that opened as the retiring room unloaded never returned")
	}
}
