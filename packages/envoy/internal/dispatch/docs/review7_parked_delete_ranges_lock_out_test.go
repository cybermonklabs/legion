// Scratch reproduction for review 7 of sjawhar/legion PR #2544 at 7e798f23 (R7Table), package docs.
// Asserts the correct behaviour, so it FAILS where the defect stands.
//
// refusedStuckBacklog (peer_update.go:1126-1147) refuses EVERY browser's update into a room whose
// pending queue has stopped falling and whose drain's splits (parked items + 2 x parked delete
// ranges) pass the splits' half of the decode cap's headroom. A parked delete range is charged to
// no field of the allowance (crdt.ApplyBudget.Bytes: "The number of delete ranges an update carries
// is bounded by Decoded instead"), so one browser's frame of delete ranges over a client the room
// has never seen parks them all, and two updates later the room refuses every person's keystroke
// DOC_NEEDS_REBUILD. The rebuild that close offers is refused: nearACap reads the room far from
// every cap (structCeiling counts each parked range once), so RebuildDocument answers 409
// DOCUMENT_LIVE (rebuild.go:421-427) while the room is resident, and ErrDocumentLoads once it is not.
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
