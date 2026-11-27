package docs

import (
	"runtime"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A drain of parked content is weighed against the document bound on what it lands: the room's
// apply check renders what the drain left and refuses it past the bound and its creep, and the
// refused drain is discarded while the update that released it stands (renderCheck,
// leaveParkedOut, closeDiscardedSenders). The update's own allowance cannot do it: the drain is
// not the update's own content, and ygo charges an apply nothing for what its drain lands
// (crdt.ApplyBudget, "A struct the drain integrates is outside Structs"), so past the creep cap,
// where the allowance leaves no content at all (peerApplyAllowance, crdt.ApplyBudget.Bytes), it
// refuses only a fill that carries some. A fill that carries none - its one character deleted by
// the same update, which ygo counts as no content, or a GC struct at the gap's clock, which
// carries none at all - is what this drives: eight browsers each park 900 KiB behind a character
// the room never received, and one update fills every gap. Before any check of the drain either
// fill was admitted and left the room rendering 7,372,824 bytes, against a document bound of
// 1,048,576.
func TestAGapFillCarryingNoContentDrainsNoRoomPastTheDocumentBound(t *testing.T) {
	const frames, each = 8, 900 << 10
	for _, row := range []struct {
		name string
		// gap is one browser's frame that parks its run behind a clock the room does not hold,
		// and the update that fills that clock and carries no content.
		gap func(t *testing.T, room *crdt.Doc) (parked, fill []byte)
	}{
		{
			name: "a fill whose one character the same update deletes",
			gap: func(t *testing.T, room *crdt.Doc) ([]byte, []byte) {
				browser, into := peerDocument(t, room)
				browser.Transact(func(txn *crdt.Transaction) { into.Insert(txn, 0, "a", nil) })
				typed := crdt.EncodeStateAsUpdateV1(browser, room.StateVector())
				held := browser.StateVector()
				browser.Transact(func(txn *crdt.Transaction) { into.Insert(txn, 1, strings.Repeat("p", each), nil) })
				parked := crdt.EncodeStateAsUpdateV1(browser, held)
				// The character, and its delete in the same update: a copy that took it and
				// deleted it holds it as an item whose content is gone (ygo's collection of a
				// deleted item's content), and hands it on that way.
				filler, from := peerDocument(t, room)
				if err := crdt.ApplyUpdateV1(filler, typed, nil); err != nil {
					t.Fatal(err)
				}
				filler.Transact(func(txn *crdt.Transaction) { from.Delete(txn, 0, 1) })
				return parked, crdt.EncodeStateAsUpdateV1(filler, room.StateVector())
			},
		},
		{
			name: "a fill of one GC struct at the gap's clock",
			gap: func(t *testing.T, room *crdt.Doc) ([]byte, []byte) {
				browser, into := peerDocument(t, room)
				// Clock 0 is the gap: a character the room never receives, which the run below
				// is not written against, so the run waits on its clock alone.
				browser.Transact(func(txn *crdt.Transaction) { into.Insert(txn, 0, "a", nil) })
				held := browser.StateVector()
				browser.Transact(func(txn *crdt.Transaction) { into.Insert(txn, 3, strings.Repeat("p", each), nil) })
				parked := crdt.EncodeStateAsUpdateV1(browser, held)
				return parked, newV1Frame(1).group(1, uint64(browser.ClientID()), 0).gc(1).noDeletes()
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph.\n", 0)
			defer runtime.KeepAlive(replica)
			// publish re-renders the room and publishes its size, as the room's update observer
			// does after each apply that commits.
			publish := func() int {
				markdown, err := renderDocument(room)
				if err != nil {
					t.Fatal(err)
				}
				replica.publishSize(&markdown)
				return len(markdown)
			}
			// Every browser copies the room before any frame is sent, so no frame carries another's.
			var fills, parkedFrames [][]byte
			for range frames {
				parked, fill := row.gap(t, room)
				parkedFrames = append(parkedFrames, parked)
				fills = append(fills, fill)
			}
			for i, parked := range parkedFrames {
				if err, called := weighInto(service, room, parked); err != nil || !called {
					t.Fatalf("parked frame %d was answered %v (applied %t), want it admitted", i+1, err, called)
				}
				publish()
			}
			pending := room.PendingStats().Items
			fill, err := crdt.MergeUpdatesV1(fills...)
			if err != nil {
				t.Fatal(err)
			}
			err, called := weighInto(service, room, fill)
			rendered := publish()
			t.Logf("%d frames of %d KiB parked (%d structs pending); one %d-byte fill carrying no content: hook=%v applied=%t; pending %d; rendering %d bytes (document bound %d, creep cap %d)",
				frames, each>>10, pending, len(fill), err, called, room.PendingStats().Items, rendered,
				pmdoc.MaxDocumentBytes, pmdoc.MaxDocumentBytes+maxPeerByteCreep)
			if rendered > pmdoc.MaxDocumentBytes+maxPeerByteCreep {
				t.Fatalf("a fill carrying no content released a drain that left the room rendering %d bytes, past the %d the document bound and its creep allow",
					rendered, pmdoc.MaxDocumentBytes+maxPeerByteCreep)
			}
		})
	}
}
