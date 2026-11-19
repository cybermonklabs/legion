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
// reads the charge the budget holds for the room through the function the budget calls
// (roomChargeWeight, roomCharge.weigh), where the repro read the two copies' weight alone
// (roomWeight). The fix charges a parked range outside roomCopies by design, since only the live
// copy holds one, so roomWeight cannot carry it.

import (
	"runtime"
	"testing"
)

func TestRV8ParkedDeleteRangesHoldHeapTheRoomBudgetDoesNotCharge(t *testing.T) {
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n", 0)
	defer runtime.KeepAlive(replica)
	applies := service.roomWeighingOf(room).applies()
	n := capHeadroom(heldStructs(room)) - 1
	frame := deleteRangesFrom(8_200_001, n)
	chargedBefore := roomChargeWeight(applies.residentBytes(), int64(heldStructs(room)), applies.parkedRangeCount())
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
	chargedAfter := roomChargeWeight(applies.residentBytes(), int64(heldStructs(room)), applies.parkedRangeCount())
	t.Logf("one frame of %d parked delete ranges: pending %+v; live heap grew %d bytes (%.1f a range); the room's budget weight %d -> %d (+%d); resident %d, structs %d",
		n, room.PendingStats(), held, float64(held)/float64(n), chargedBefore, chargedAfter, chargedAfter-chargedBefore, applies.residentBytes(), heldStructs(room))
	if held > 4*(chargedAfter-chargedBefore)+(1<<20) {
		t.Fatalf("the room holds %d more bytes of live heap for %d parked delete ranges and the budget charges it %d more", held, n, chargedAfter-chargedBefore)
	}
}
