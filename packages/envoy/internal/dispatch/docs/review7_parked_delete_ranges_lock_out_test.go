// Reproduction for review 7 of sjawhar/legion PR #2544 at 7e798f23 (R7Table), package docs.
// Asserts the correct behaviour, so it fails where the defect stands.
//
// At 7e798f23 the room refused every browser's update into a room whose pending queue had stopped
// falling and whose drain's splits (parked items + 2 x parked delete ranges) passed the splits'
// half of the decode cap's headroom, closing it DOC_NEEDS_REBUILD. A parked delete range is
// charged to no field of the allowance (crdt.ApplyBudget.Bytes: "The number of delete ranges an
// update carries is bounded by Decoded instead"), so one browser's frame of delete ranges over a
// client the room has never seen parked them all, and two updates later the room refused every
// person's keystroke, offering a rebuild the route refused: nearACap read the room far from every
// cap, so RebuildDocument answered 409 DOCUMENT_LIVE. That rule is gone: a
// drain the room cannot take is discarded while the update that released it stands
// (leaveParkedOut), so no queue stands still for a rule to read, and this holds that a frame of
// parked delete ranges locks no editor out.
package docs

import (
	"context"
	"errors"
	"runtime"
	"testing"

	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/contracts"
)

func TestR7OneFrameOfParkedDeleteRangesLocksEveryEditorOutOfASmallDocument(t *testing.T) {
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n", 0)
	defer runtime.KeepAlive(replica)
	// What the drain of n parked ranges is weighed at (2n) must pass half of what the decode cap
	// leaves above the room's few structs.
	n := capHeadroom(heldStructs(room))/4 + 1
	// Four people's keystrokes, each typed in a browser that opened the document before the
	// attack, so each is a few bytes, as the editor sends a keystroke.
	var keystrokes [][]byte
	for range 4 {
		keystrokes = append(keystrokes, peerTyping(t, room, "x"))
	}
	frame := deleteRangesFrom(8_200_001, n)
	err, called := weighInto(service, room, frame)
	t.Logf("the attacker's frame of %d delete ranges over an unknown client, %d bytes: hook=%v applied=%t; pending %+v; held %d, structCeiling %d",
		n, len(frame), err, called, room.PendingStats(), heldStructs(room), structCeiling(room))
	if err != nil || !called {
		t.Fatalf("the frame was refused: %v", err)
	}
	var refusals int
	for i, keystroke := range keystrokes {
		err, called := weighInto(service, room, keystroke)
		var closed *websocket.PeerClose
		if errors.As(err, &closed) && closed.Reason == contracts.DocumentNeedsRebuildCloseReason {
			refusals++
		}
		t.Logf("an honest person's keystroke %d, %d bytes: hook=%v applied=%t", i+1, len(keystroke), err, called)
	}
	why, near, nearErr := nearACap(context.Background(), room, 1, service.roomWeighingOf(room).roomStateBytes())
	t.Logf("nearACap near=%t why=%q err=%v", near, why, nearErr)
	if refusals > 0 {
		t.Fatalf("%d of 4 honest keystrokes into a %d-struct room were refused DOC_NEEDS_REBUILD after one browser parked %d delete ranges; nearACap near=%t, so the rebuild the editor offers is refused",
			refusals, heldStructs(room), n, near)
	}
}
