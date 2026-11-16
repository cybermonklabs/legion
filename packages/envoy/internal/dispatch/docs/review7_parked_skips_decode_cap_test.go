// Scratch reproduction for review 7 of sjawhar/legion PR #2544 at 7e798f23 (R7Table), package docs.
// Asserts the correct behaviour, so it FAILS where the defect stands.
//
// The hook's allowance reads the room's structs as ygo's ApplyReport counts them: each parked
// struct once (heldStructs, growth.go:562-564). The room's encoded state carries each parked
// struct with a skip over the gap it waits behind (withParked; structCeiling, growth.go:527-547,
// counts it twice for that reason). A frame whose structs each wait behind their own one-clock
// gap therefore adds 2 structs to the stored state per struct the allowance charges, and two such
// frames, each within its allowance, leave a state the decode cap refuses.
package docs

import (
	"runtime"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// r7ParkedBehindSkips is a frame of one client whose n GC structs each sit behind a one-clock
// skip, so every one parks and each waits behind a gap of its own.
func r7ParkedBehindSkips(client uint64, n int) []byte {
	f := newV1Frame(1).group(2*n, client, 0)
	for range n {
		f = f.skip(1).gc(1)
	}
	return f.noDeletes()
}

func TestR7ParkedStructsBehindTheirOwnGapsPassTheDecodeCap(t *testing.T) {
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph.\n", 0)
	defer runtime.KeepAlive(replica)
	for i, client := range []uint64{9_000_001, 9_000_002} {
		held := heldStructs(room)
		allowed := peerApplyAllowance(0, 0, 0, held, false, peerPressure{}).budget.Structs
		frame := r7ParkedBehindSkips(client, allowed)
		err, called := weighInto(service, room, frame)
		pending := room.PendingStats()
		t.Logf("frame %d: %d parked GC structs each behind a skip, %d bytes: hook=%v applied=%t; held (report) %d, structCeiling %d, pending items %d ranges %d",
			i+1, allowed, len(frame), err, called, heldStructs(room), structCeiling(room), pending.Items, pending.DeleteRanges)
	}
	state := crdt.EncodeStateAsUpdateV1(room, nil)
	_, decodeErr := decodeStoredCopy(state, maxUpdateItems)
	t.Logf("stored state %d bytes; held (report) %d, structCeiling %d (cap %d); decodeStoredCopy=%v",
		len(state), heldStructs(room), structCeiling(room), maxUpdateItems, decodeErr)
	if decodeErr != nil {
		t.Fatalf("the hook admitted every frame, each within its allowance, and the room's stored state no longer decodes: %v", decodeErr)
	}
}
