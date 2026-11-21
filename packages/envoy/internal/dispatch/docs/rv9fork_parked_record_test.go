package docs

import (
	"runtime"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
)

// rv9DeleteFrame is a frame of no structs whose delete set holds ranges of client's.
func rv9DeleteFrame(client uint64, ranges []crdt.DeleteRange) []byte {
	f := newV1Frame(0).uint(1).uint(client).uint(uint64(len(ranges)))
	for _, r := range ranges {
		f.uint(r.Clock).uint(r.Len)
	}
	return f.done()
}

// Review 8's finding 3 with theorem.10's record: the room parks one delete range and is charged
// 32 bytes for it (bytesOfHeapAParkedRange) while the record of who parked what
// (crdt.Doc.parkedDsBy, leaveParkedOut) keeps every range each pair of frames newly parked. A
// frame of 4,095 one-clock ranges with gaps parks within maxParkedDeleteRanges; one frame of one
// range over them and their gaps compacts the pending deletes to one range and records 4,096
// pieces. Each pair records 8,191 ranges; none is pruned, since each overlaps the one range.
func TestRV9ForkParkedDeleteRecordIsHeapTheRoomBudgetDoesNotCharge(t *testing.T) {
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph.\n", 0)
	defer runtime.KeepAlive(replica)
	room.SetMaxPendingDeleteRanges(maxParkedDeleteRanges)
	service.budget.limit = roomBudgetBytes
	charge := service.chargeLoadedRoom("room", replica, int64(len(crdt.EncodeStateAsUpdateV1(room, nil))))
	loaded := chargedWeight(charge)
	const client, pairs, m = 8_300_001, 300, maxParkedDeleteRanges - 1
	typing := peerTyping(t, room, "a")
	start := time.Now()
	if err, called := weighInto(service, room, typing); err != nil || !called {
		t.Fatalf("a keystroke before was refused: %v", err)
	}
	first := time.Since(start)
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	wire := 0
	var end uint64
	for p := range pairs {
		singles := make([]crdt.DeleteRange, m)
		for j := range singles {
			singles[j] = crdt.DeleteRange{Clock: end + 1 + 2*uint64(j), Len: 1}
		}
		a := rv9DeleteFrame(client, singles)
		span := uint64(2*m + 1)
		b := rv9DeleteFrame(client, []crdt.DeleteRange{{Clock: end, Len: span}})
		for _, frame := range [][]byte{a, b} {
			if err, called := weighInto(service, room, frame); err != nil || !called {
				t.Fatalf("pair %d: a frame was refused: %v (called %v)", p, err, called)
			}
			wire += len(frame)
		}
		end += span
	}
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	typing = peerTyping(t, room, "b")
	start = time.Now()
	if err, called := weighInto(service, room, typing); err != nil || !called {
		t.Fatalf("a keystroke after was refused: %v", err)
	}
	last := time.Since(start)
	grew := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	charged := chargedWeight(charge) - loaded
	t.Logf("%d pairs, %d wire bytes: the room parks %+v, its charge moved by %d bytes, its live heap grew %d bytes; a keystroke's weighed apply took %v before and %v after",
		pairs, wire, room.PendingStats(), charged, grew, first, last)
	if grew > 8*charged+(8<<20) {
		t.Fatalf("the room's heap grew %d bytes for parked delete ranges it is charged %d for", grew, charged)
	}
}
