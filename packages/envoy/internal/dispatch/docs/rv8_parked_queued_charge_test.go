package docs

// Review 8 scratch repro (not for the tree). noteQueued charges the whole content of every apply
// that leaves the room's pending queue holding anything to queuedContent, and only an apply that
// leaves the queue empty takes the charge back. One struct parked behind a gap no one fills keeps
// the queue non-empty while the room is loaded, so every later paste is charged as queued content
// on top of the tracked state, and deleting the paste takes none of it back. residentBytes (state
// + queued) is what the state cap's allowance and the room budget weigh, so a room whose encoded
// state is small ends refusing every keystroke at the state cap.

import (
	"runtime"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
)

func rv8PasteAndDeleteCycles(t *testing.T, park bool) (refusedAt int, err error, service *Service, room *crdt.Doc) {
	t.Helper()
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph.\n", 0)
	defer runtime.KeepAlive(replica)
	if park {
		frame := newV1Frame(1).group(1, 9_100_001, 5).gc(1).noDeletes()
		if err, called := weighInto(service, room, frame); err != nil || !called {
			t.Fatalf("the one parked struct was refused: %v", err)
		}
		if items := room.PendingStats().Items; items != 1 {
			t.Fatalf("the room parks %d structs, want the one", items)
		}
	}
	applies := service.roomWeighingOf(room).applies()
	paste := 900 << 10
	for cycle := 0; cycle < 200 && paste > 0; cycle++ {
		err, _ := weighInto(service, room, peerTyping(t, room, strings.Repeat("p", paste)))
		if err != nil {
			t.Logf("park=%t cycle %d: a paste of %d bytes was refused: %v", park, cycle, paste, err)
			paste /= 2
			continue
		}
		if err, _ := weighInto(service, room, peerDeleting(t, room, paste)); err != nil {
			t.Fatalf("park=%t cycle %d: the delete of the paste was refused: %v", park, cycle, err)
		}
		if cycle%5 == 4 {
			t.Logf("park=%t after %d paste+delete cycles: encoded state %d bytes, tracked %d, queued %d, resident %d",
				park, cycle+1, len(crdt.EncodeStateAsUpdateV1(room, nil)), applies.stateBytes(), applies.queuedBytes(), applies.residentBytes())
		}
	}
	t.Logf("park=%t: the room's budget weight is %d bytes (roomWeight of resident %d and %d structs)",
		park, roomWeight(applies.residentBytes(), int64(heldStructs(room))), applies.residentBytes(), heldStructs(room))
	for keystroke := range 3 {
		if err, _ := weighInto(service, room, peerTyping(t, room, "x")); err != nil {
			t.Logf("park=%t: keystroke %d refused: %v; encoded state %d bytes, tracked %d, queued %d, resident %d",
				park, keystroke+1, err, len(crdt.EncodeStateAsUpdateV1(room, nil)), applies.stateBytes(), applies.queuedBytes(), applies.residentBytes())
			return keystroke + 1, err, service, room
		}
	}
	t.Logf("park=%t: three keystrokes applied; encoded state %d bytes, tracked %d, queued %d, resident %d",
		park, len(crdt.EncodeStateAsUpdateV1(room, nil)), applies.stateBytes(), applies.queuedBytes(), applies.residentBytes())
	return 0, nil, service, room
}

func TestRV8OneParkedStructMakesPastesThatWereDeletedLockTheRoomAtTheStateCap(t *testing.T) {
	if at, err, _, _ := rv8PasteAndDeleteCycles(t, false); err != nil {
		t.Fatalf("control: with nothing parked, keystroke %d after the paste+delete cycles was refused: %v", at, err)
	}
	if at, err, _, room := rv8PasteAndDeleteCycles(t, true); err != nil {
		t.Fatalf("with one struct parked behind a gap no one fills, keystroke %d after the paste+delete cycles was refused (%v), in a room whose encoded state is %d bytes",
			at, err, len(crdt.EncodeStateAsUpdateV1(room, nil)))
	}
}
