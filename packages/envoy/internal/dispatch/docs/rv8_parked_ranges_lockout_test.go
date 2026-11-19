package docs

// Review 8 scratch repro (not for the tree). Review 7's C2 at its in-tree size passes; this is the
// same frame at the size the pending cap allows: one frame of delete ranges over a client the room
// has never seen, enough to fill the decode cap's headroom (storedCeiling counts each parked range
// once). Every later edit that adds a struct is then refused at the decode cap.

import (
	"errors"
	"runtime"
	"testing"

	"github.com/reearth/ygo/provider/websocket"
)

func TestRV8OneFrameOfParkedDeleteRangesAtTheHeadroomRefusesEveryKeystroke(t *testing.T) {
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n", 0)
	defer runtime.KeepAlive(replica)
	n := capHeadroom(heldStructs(room)) - 1
	keystroke := peerTyping(t, room, "x")
	frame := deleteRangesFrom(8_200_001, n)
	err, called := weighInto(service, room, frame)
	t.Logf("a frame of %d delete ranges over an unknown client, %d bytes: hook=%v applied=%t; pending %+v; held %d, structCeiling %d",
		n, len(frame), err, called, room.PendingStats(), heldStructs(room), structCeiling(room))
	if err != nil || !called {
		t.Skipf("the frame was refused (%v), so no lock-out", err)
	}
	err, _ = weighInto(service, room, keystroke)
	var closed *websocket.PeerClose
	if errors.As(err, &closed) {
		t.Fatalf("an honest one-character keystroke into a %d-struct document was refused %d %s after one browser's frame of %d parked delete ranges",
			heldStructs(room)-0, closed.Code, closed.Reason, n)
	}
	t.Logf("the keystroke: %v", err)
}
