// Scratch reproduction for review 7 of sjawhar/legion PR #2544 at 7e798f23 (R7Table), package docs.
// Asserts the correct behaviour, so it FAILS where the defect stands.
//
// Review 6's finding 4, re-checked against every refusal that closes DOC_NEEDS_REBUILD: the editor
// offers the rebuild on that close, and the route takes only a document nearACap answers near
// (RebuildDocument, rebuild.go:421-427, 409 DOCUMENT_LIVE otherwise). nearACap's state-cap margin
// is 64 KiB (nearTheStateCap); the hook refuses any update whose content passes what the state cap
// leaves (peer_update.go:304-306), the decode cap's structs' half, or a stalled backlog's drain.
package docs

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/contracts"
)

func r7RequireOfferAgrees(t *testing.T, service *Service, err error, room interface{}, why string, near bool, nearErr error) {
	t.Helper()
	var closed *websocket.PeerClose
	refused := errors.As(err, &closed) && closed.Reason == contracts.DocumentNeedsRebuildCloseReason
	t.Logf("hook=%v refusedNeedsRebuild=%t; nearACap near=%t why=%q err=%v", err, refused, near, why, nearErr)
	if refused && !near {
		t.Fatal("the room closed the browser DOC_NEEDS_REBUILD, whose editor offers the rebuild, and nearACap answers near=false, so the route answers that offer 409 DOCUMENT_LIVE")
	}
}

// A long-edited document whose history fills the state cap and whose text is short: a paste of
// 200 KiB is no business of the document bound (rendering + update < 1 MiB), the state cap
// refuses it, and a rebuild, which drops the history, would take it.
func TestR7AStateCapRefusalOfAPastePastTheMarginIsOfferedARebuildTheRouteRefuses(t *testing.T) {
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph.\n", 0)
	defer runtime.KeepAlive(replica)
	tracked := int64(maxRoomStateBytes - 100<<10)
	service.chargeLoadedRoom("room", replica, tracked)
	err, _ := weighInto(service, room, peerTyping(t, room, strings.Repeat("p", 200<<10)))
	why, near, nearErr := nearACap(context.Background(), room, 1, tracked)
	r7RequireOfferAgrees(t, service, err, room, why, near, nearErr)
}

// The decode cap's branch: a room 100,000 structs short of the cap refuses a frame of 60,000
// (more than the structs' half) DOC_NEEDS_REBUILD; its text is a few blocks, so nearTheDecodeCap
// reads it far.
func TestR7ADecodeCapRefusalIsOfferedARebuildTheRouteRefuses(t *testing.T) {
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph.\n", 0)
	defer runtime.KeepAlive(replica)
	fillToStructs(t, service, room, maxUpdateItems-100_000)
	err, _ := weighInto(service, room, gcFrameFrom(7, 60_000))
	why, near, nearErr := nearACap(context.Background(), room, 1, service.roomWeighingOf(room).roomStateBytes())
	r7RequireOfferAgrees(t, service, err, room, why, near, nearErr)
}

// The stalled backlog's branch (refusedStuckBacklog): a small room whose queue a browser filled
// with two frames of GC structs, each behind a gap of a client of its own and each as many as its
// allowance takes (capShares), until the drain's splits pass the splits' share of what the decode
// cap leaves, while the room stands far from every cap. One frame cannot get there since every
// struct is charged two items of the decode cap's headroom (storedCeiling): its share is a quarter
// of the headroom, and the drain passes the splits' share only past a quarter. The second frame's
// share is a quarter of what the first left, so the two together pass it: 262,141 and 131,071
// structs over the seeded paragraph's 10, drain splits of 393,212 against a share of 131,071.
func TestR7AStalledBacklogRefusalIsOfferedARebuildTheRouteRefuses(t *testing.T) {
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph.\n", 0)
	defer runtime.KeepAlive(replica)
	// Two people's keystrokes, each typed in a browser that opened the document before the
	// frames, so each is a few bytes, as the editor sends a keystroke; a browser that copied the
	// room after them would send the parked frames back with its keystroke.
	keystrokes := [][]byte{peerTyping(t, room, "x"), peerTyping(t, room, "y")}
	for _, client := range []uint64{8_100_001, 8_100_002} {
		n := peerApplyAllowance(0, 0, 0, structCeiling(room), false, peerPressure{}).budget.Structs
		if err, called := weighInto(service, room, parkedGCFrame(client, n)); err != nil || !called {
			t.Fatalf("the parked frame of %d structs was answered %v (applied %t)", n, err, called)
		}
	}
	parked := room.PendingStats()
	_, share := capShares(structCeiling(room))
	if drain := parked.Items + 2*parked.DeleteRanges; drain <= share {
		t.Fatalf("the room parks %+v, whose drain's %d splits are within the splits' share %d: no backlog the hook refuses for", parked, drain, share)
	}
	// The second frame's reading of the queue set its floor, and each keystroke finds it no
	// lower (backlogStalled).
	var err error
	for i, keystroke := range keystrokes {
		err, _ = weighInto(service, room, keystroke)
		t.Logf("keystroke %d after the park, %d bytes: %v; pending %+v, drain splits %d against a share of %d",
			i+1, len(keystroke), err, room.PendingStats(), parked.Items+2*parked.DeleteRanges, share)
	}
	why, near, nearErr := nearACap(context.Background(), room, 1, service.roomWeighingOf(room).roomStateBytes())
	r7RequireOfferAgrees(t, service, err, room, why, near, nearErr)
}
