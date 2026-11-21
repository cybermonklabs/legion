package docs

// Review 8's repro, kept as a regression test. Under design F a parked delete range is out of the
// room's encoded state (leaveParkedOut), so the tracked state does not count it; the queued
// content noteQueued records is content bytes, which a delete range has none of; and the struct
// figure counts parked structs, not ranges. So the room budget charged nothing for a room's
// parked delete ranges. It now charges each range the room parks (noteParkedRanges,
// roomChargeWeight, bytesOfHeapAParkedRange), and the test holds the live heap one frame of
// parked ranges takes to within four times what the room's charge grew by, plus 1 MiB.
//
// Review 8's repro is kept as it was written, and only its two reads of the charge moved: each
// reads the charge the room budget holds for the room (chargedWeight), as
// TestParkedDeleteRangesAreChargedToTheRoomBudgetUntilTheyLand does, the room charged as a load
// charges it (chargedPeerRoom, chargeLoadedRoom), where the repro read the two copies' weight
// alone (roomWeight). Round 8's fix computed roomChargeWeight itself here, over a room the budget
// charged nothing, so it passed with the hook's re-weigh of the room taken out (the round 9 class
// search, D4). The fix charges a parked range outside roomCopies by design, since only the live
// copy holds one, so roomWeight cannot carry it.

import (
	"runtime"
	"testing"
)

func TestRV8ParkedDeleteRangesHoldHeapTheRoomBudgetDoesNotCharge(t *testing.T) {
	service, room, replica, charge := chargedPeerRoom(t)
	defer runtime.KeepAlive(replica)
	applies := service.roomWeighingOf(room).applies()
	n := capHeadroom(heldStructs(room)) - 1
	frame := deleteRangesFrom(8_200_001, n)
	chargedBefore := chargedWeight(charge)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&before)
	err, called := weighInto(service, room, frame)
	frame = nil
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(room)
	if err != nil || !called {
		t.Fatalf("the frame was refused: %v", err)
	}
	held := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	chargedAfter := chargedWeight(charge)
	t.Logf("one frame of %d parked delete ranges: pending %+v; live heap grew %d bytes (%.1f a range); the room's budget charge %d -> %d (+%d); resident %d, structs %d",
		n, room.PendingStats(), held, float64(held)/float64(n), chargedBefore, chargedAfter, chargedAfter-chargedBefore, applies.residentBytes(), heldStructs(room))
	if held > 4*(chargedAfter-chargedBefore)+(1<<20) {
		t.Fatalf("the room holds %d more bytes of live heap for %d parked delete ranges and the budget charges it %d more", held, n, chargedAfter-chargedBefore)
	}
}
