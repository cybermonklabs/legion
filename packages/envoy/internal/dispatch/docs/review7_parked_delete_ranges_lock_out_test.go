// Reproduction for review 7 of sjawhar/legion PR #2544 at 7e798f23 (R7Table), package docs, and
// for review 8's C2 thread at fe2b9ddd. Asserts the correct behaviour, so it fails where the
// defect stands.
//
// A parked delete range is charged to no field of a browser's allowance but Decoded
// (crdt.ApplyBudget.Bytes: "The number of delete ranges an update carries is bounded by Decoded
// instead"), so one browser's frame of delete ranges over a client the room has never seen parks
// them all. At 7e798f23 a rule on the stalled queue's drain then refused every person's keystroke
// DOC_NEEDS_REBUILD, offering a rebuild the route refused. That rule went with design F, and at
// fe2b9ddd the decode cap counted each parked range once instead (storedCeiling, as it was named),
// so a frame of 1,048,565 ranges, 4,186,013 bytes, filled a small room's whole headroom: every
// keystroke was refused at the cap, closed DOC_NEEDS_REBUILD or DOC_TOO_LARGE, and so was every
// API or server write of two structs or more. A parked range is no item of the cap's now
// (ceilingWithQueue): the splits its landing makes are the drain's, which the apply that lands it
// spends from its own Split share.
package docs

import (
	"context"
	"runtime"
	"testing"
)

// The frame is the largest one browser's frame of parked ranges can be: the room's pending cap,
// maxUpdateItems ranges (Server.MaxPendingItems, which ygo holds the queue's ranges to as it parks
// them), which is within the maxDecodedValuesPerApply values one apply may decode, every range
// read counting one. Each honest keystroke after it must apply, the room's ceiling must not move
// for it, and the rebuild must not be offered for a room it leaves as small as it was.
func TestR7OneFrameOfParkedDeleteRangesLocksEveryEditorOutOfASmallDocument(t *testing.T) {
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n", 0)
	defer runtime.KeepAlive(replica)
	const n = maxUpdateItems
	// Four people's keystrokes, each typed in a browser that opened the document before the
	// attack, so each is a few bytes, as the editor sends a keystroke.
	var keystrokes [][]byte
	for range 4 {
		keystrokes = append(keystrokes, peerTyping(t, room, "x"))
	}
	ceiling := structCeiling(room)
	frame := deleteRangesFrom(8_200_001, n)
	err, called := weighInto(service, room, frame)
	t.Logf("the attacker's frame of %d delete ranges over an unknown client, %d bytes: hook=%v applied=%t; pending %+v; held %d, structCeiling %d",
		n, len(frame), err, called, room.PendingStats(), heldStructs(room), structCeiling(room))
	if err != nil || !called {
		t.Fatalf("the frame was refused: %v", err)
	}
	if parked := room.PendingStats().DeleteRanges; parked != n {
		t.Fatalf("the room parks %d delete ranges of the frame's %d, so the frame is not the one this sizes", parked, n)
	}
	if after := structCeiling(room); after != ceiling {
		t.Errorf("the room is weighed at %d items against the decode cap after the frame, want the %d it stood at: a parked range is no item of the cap's", after, ceiling)
	}
	for i, keystroke := range keystrokes {
		err, called := weighInto(service, room, keystroke)
		t.Logf("an honest person's keystroke %d, %d bytes: hook=%v applied=%t", i+1, len(keystroke), err, called)
		if err != nil || !called {
			t.Errorf("honest keystroke %d of 4 into a %d-struct room was refused (%v, applied=%t) after one browser parked %d delete ranges",
				i+1, heldStructs(room), err, called, n)
		}
	}
	why, near, nearErr := nearACap(context.Background(), room, 1, service.roomWeighingOf(room).roomStateBytes())
	t.Logf("nearACap near=%t why=%q err=%v", near, why, nearErr)
	if nearErr != nil || near {
		t.Errorf("the room is read as near a cap (%q, %v) after one browser parked %d delete ranges, so a rebuild is offered for a room that holds a handful of structs", why, nearErr, n)
	}
}
