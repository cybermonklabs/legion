// Scratch reproduction for review 7 of sjawhar/legion PR #2544 at 7e798f23 (R7Table), package docs.
// Asserts the correct behaviour, so it FAILS where the defect stands.
//
// The PR body's table says the document bound (1 MiB rendered) is "charged where it parks" for a
// drain of parked content. What an update parks is charged against Bytes, and Bytes is the
// document bound's maxPeerInsertAtTheBound only where the room's rendering plus the update's own
// length reach pmdoc.MaxDocumentBytes (peerApplyAllowance); otherwise it is
// what the state cap leaves (16 MiB). Parked content renders nothing, so the rendering the next
// frame is weighed against does not move: one browser parks frame after frame of just under a
// mebibyte each, then one tiny update fills every gap and the drain lands them all at once.
package docs

import (
	"runtime"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

func TestR7ParkedFramesDrainPastTheDocumentBound(t *testing.T) {
	const frames, each = 8, 900 << 10
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph.\n", 0)
	defer runtime.KeepAlive(replica)
	// publish re-renders the room and publishes its size, as the room's update observer does
	// after each apply that commits.
	publish := func() int {
		markdown, err := renderDocument(room)
		if err != nil {
			t.Fatal(err)
		}
		replica.publishSize(&markdown)
		return len(markdown)
	}
	// Every browser copies the room before any frame is sent, so no frame carries another's.
	var fills, frameList [][]byte
	for range frames {
		browser, into := peerDocument(t, room)
		browser.Transact(func(txn *crdt.Transaction) { into.Insert(txn, 0, "a", nil) })
		fills = append(fills, crdt.EncodeStateAsUpdateV1(browser, room.StateVector()))
		held := browser.StateVector()
		browser.Transact(func(txn *crdt.Transaction) { into.Insert(txn, 1, strings.Repeat("p", each), nil) })
		frameList = append(frameList, crdt.EncodeStateAsUpdateV1(browser, held))
	}
	for i, parked := range frameList {
		err, called := weighInto(service, room, parked)
		rendered := publish()
		t.Logf("parked frame %d: %d bytes, hook=%v applied=%t; pending items %d; rendering %d bytes; tracked state %d",
			i+1, len(parked), err, called, room.PendingStats().Items, rendered, service.roomWeighingOf(room).roomStateBytes())
		if err != nil || !called {
			t.Fatalf("frame %d refused: %v", i+1, err)
		}
	}
	fill, err := crdt.MergeUpdatesV1(fills...)
	if err != nil {
		t.Fatal(err)
	}
	err, called := weighInto(service, room, fill)
	rendered := publish()
	t.Logf("one %d-byte fill of every gap: hook=%v applied=%t; pending items %d; rendering %d bytes (document bound %d, creep cap %d)",
		len(fill), err, called, room.PendingStats().Items, rendered, pmdoc.MaxDocumentBytes, pmdoc.MaxDocumentBytes+maxPeerByteCreep)
	if rendered > pmdoc.MaxDocumentBytes+maxPeerByteCreep {
		t.Fatalf("one browser's updates, sent one after another, left the room rendering %d bytes, past the %d the document bound and its creep allow",
			rendered, pmdoc.MaxDocumentBytes+maxPeerByteCreep)
	}
}
