// Review 7 of sjawhar/legion PR #2544 found this at 7e798f23 (R7Table), package docs.
//
// The hook's allowance read the room's structs as ygo's ApplyReport counts them: each parked
// struct once (heldStructs). A frame whose structs each wait behind their own one-clock gap parks
// every one of them, so a count that charged a parked struct once let two such frames, each
// within its allowance, take the room past the decode cap (dispatch://INFRA-179, review 7).
//
// The hook now asks the cap of the ceiling the room is weighed at, which counts each parked struct
// again (ceilingWithQueue). Nothing a room parks is stored any more (leaveParkedOut), so that charge
// bounds the room's pending queue in memory, out of the same headroom as what it integrates.
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

// Each frame is the structs' share the report's count leaves the room: the first, 262,141 above the
// seeded room, is admitted and parks whole. The second is the 196,606 that count leaves above
// it, and is refused DOC_TOO_LARGE against the 131,071 the ceiling leaves, which counts the first
// frame's parked structs twice, so the room stays within the cap. Weighed by the report's count,
// the second frame would be admitted.
func TestR7ParkedStructsBehindTheirOwnGapsPassTheDecodeCap(t *testing.T) {
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph.\n", 0)
	defer runtime.KeepAlive(replica)
	var refusal error
	var before []byte
	for i, client := range []uint64{9_000_001, 9_000_002} {
		held := heldStructs(room)
		allowed := peerApplyAllowance(0, 0, 0, held, false, peerPressure{}).budget.Structs
		frame := r7ParkedBehindSkips(client, allowed)
		before = crdt.EncodeStateAsUpdateV1(room, nil)
		err, called := weighInto(service, room, frame)
		pending := room.PendingStats()
		t.Logf("frame %d: %d parked GC structs each behind a skip, %d bytes: hook=%v applied=%t; held (report) %d, structCeiling %d, pending items %d ranges %d",
			i+1, allowed, len(frame), err, called, heldStructs(room), structCeiling(room), pending.Items, pending.DeleteRanges)
		if i == 0 && err != nil {
			t.Fatalf("the first frame, the whole structs' share above the seeded room, was refused: %v", err)
		}
		refusal = err
	}
	requireRefusedTooLarge(t, refusal, room, before)
	if ceiling := structCeiling(room); ceiling > maxUpdateItems {
		t.Fatalf("the hook admitted frames of structs parked behind gaps of their own, and the room is weighed at %d items, past the %d cap", ceiling, maxUpdateItems)
	}
}
