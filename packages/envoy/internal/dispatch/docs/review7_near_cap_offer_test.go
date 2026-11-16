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

// The stalled backlog's branch (refusedStuckBacklog): a room 300,000 structs short of the cap
// holding 110,000 parked structs behind one gap, whose drain's splits pass the splits' half.
func TestR7AStalledBacklogRefusalIsOfferedARebuildTheRouteRefuses(t *testing.T) {
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph.\n", 0)
	defer runtime.KeepAlive(replica)
	fillToStructs(t, service, room, maxUpdateItems-300_000)
	const parkedN = 110_000
	f := newV1Frame(1).group(parkedN, 8_100_001, 1)
	for range parkedN {
		f = f.gc(1)
	}
	if err, called := weighInto(service, room, f.noDeletes()); err != nil || !called {
		t.Fatalf("the parked frame was answered %v (applied %t)", err, called)
	}
	// The next two updates: the first reads the backlog and sets its floor, the second finds it
	// no lower.
	var err error
	for i := range 2 {
		err, _ = weighInto(service, room, deleteRangesFrom(gcFillClient, 1))
		t.Logf("update %d after the park: %v; pending %+v", i+1, err, room.PendingStats())
	}
	why, near, nearErr := nearACap(context.Background(), room, 1, service.roomWeighingOf(room).roomStateBytes())
	r7RequireOfferAgrees(t, service, err, room, why, near, nearErr)
}
