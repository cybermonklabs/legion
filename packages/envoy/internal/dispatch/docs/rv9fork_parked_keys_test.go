package docs

import (
	"runtime"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// TestARunOfParkedFramesIsRefusedAtTheStateCapHoweverManyArrive, with the
// frame's bytes moved from the value into the map key: the record is a root
// map entry (no origin, so the key is on the wire and held by the parked
// struct), its value empty. crdt.PendingStats.ContentBytes leaves the key out,
// so the room's queue figure (noteQueued) does not move and every frame is
// admitted, while the live heap holds each key.
func TestRV9ForkParkedMapKeysEscapeTheQueueFigure(t *testing.T) {
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph.\n", 0)
	defer runtime.KeepAlive(replica)
	service.budget.limit = roomBudgetBytes
	charge := service.chargeLoadedRoom("room", replica, int64(len(crdt.EncodeStateAsUpdateV1(room, nil))))
	weighing := service.roomWeighingOf(room)
	applies := weighing.applies()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	admitted := 0
	for i := range 40 {
		key := strings.Repeat(string(rune('a'+i%26)), 900<<10) + strings.Repeat("x", i)
		err, applied := weighInto(service, room, marginRecord(6_100_001, uint64(5+i), key, ""))
		if err != nil || !applied {
			t.Logf("frame %d answered %v (applied %v)", i, err, applied)
			break
		}
		admitted++
	}
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	t.Logf("%d of 40 frames each parking a 900 KiB map key admitted; room parks %+v; queued figure %d, resident %d of %d, charged %d; live heap grew %d bytes",
		admitted, room.PendingStats(), applies.queuedBytes(), weighing.roomResidentBytes(), maxRoomStateBytes, chargedWeight(charge),
		int64(after.HeapAlloc)-int64(before.HeapAlloc))
	if held := int64(admitted) * (900 << 10); held > maxRoomStateBytes {
		t.Fatalf("the room admitted %d parked frames holding %d bytes of map keys, past the %d one room may hold, while its queue figure is %d",
			admitted, held, maxRoomStateBytes, applies.queuedBytes())
	}
}
