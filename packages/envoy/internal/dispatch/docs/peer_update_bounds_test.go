package docs

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A room loaded holding a backlog weighs the drain of that backlog against the document bound,
// as the room that parked it did: the stored state an earlier build wrote carries what parked and
// the load parks it again, and the one update that fills every gap lands it only as the room's
// apply check passes what it lands, discarding it otherwise (renderCheck, leaveParkedOut). This is
// review 7's TestR7ParkedFramesDrainPastTheDocumentBound across a load: eight frames of 900 KiB
// park in one room, each behind a gap of a browser of its own, the room's state is loaded as a
// room's load loads it (loadAndType), and the one update that fills every gap is handed to the
// loaded room's hook. Weighed on the loaded room's rendering alone, it was admitted and the drain
// left the room rendering past the document bound and its creep.
func TestALoadedBacklogsDrainIsWeighedAgainstTheDocumentBound(t *testing.T) {
	const frames, each = 8, 900 << 10
	service, parker, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph.\n", 0)
	// Every browser copies the room before any frame is sent, so no frame carries another's.
	var fills, parking [][]byte
	for range frames {
		browser, into := peerDocument(t, parker)
		browser.Transact(func(txn *crdt.Transaction) { into.Insert(txn, 0, "a", nil) })
		fills = append(fills, crdt.EncodeStateAsUpdateV1(browser, parker.StateVector()))
		held := browser.StateVector()
		browser.Transact(func(txn *crdt.Transaction) { into.Insert(txn, 1, strings.Repeat("p", each), nil) })
		parking = append(parking, crdt.EncodeStateAsUpdateV1(browser, held))
	}
	for _, frame := range parking {
		if err, called := weighInto(service, parker, frame); err != nil || !called {
			t.Fatalf("a parked frame was answered %v (applied %t), want it admitted", err, called)
		}
	}
	// The room leaves what it parks out of its state (leaveParkedOut), so the rows an earlier
	// build stored of what parked are the frames themselves, folded into the state after it.
	state := mergeStoredOrder(t, append([][]byte{crdt.EncodeStateAsUpdateV1(parker, nil)}, parking...)...)
	runtime.KeepAlive(replica)

	loaded := loadAndType(t, state, nil)
	parked := loaded.live.PendingStats()
	rendering, _ := loaded.replica.size()
	t.Logf("the loaded room parks %d structs from its %d-byte state, against the %d of content the frames carried, rendering %d bytes",
		parked.Items, len(state), frames*each, rendering)
	if parked.Items == 0 {
		t.Fatal("the loaded room parks nothing: the fixture does not load a backlog")
	}
	fill := mergeStoredOrder(t, fills...)
	browser := new(int)
	info := websocket.PeerUpdateInfo{Room: "room", Doc: loaded.live, Update: fill, Origin: browser}
	err := loaded.service.weighPeerUpdate(info, func() error { return crdt.ApplyUpdateV1(loaded.live, fill, browser) })
	markdown, renderErr := renderDocument(loaded.live)
	if renderErr != nil {
		t.Fatal(renderErr)
	}
	t.Logf("one %d-byte fill of every gap: hook=%v; pending items %d; rendering %d bytes (document bound %d, creep cap %d)",
		len(fill), err, loaded.live.PendingStats().Items, len(markdown), pmdoc.MaxDocumentBytes, pmdoc.MaxDocumentBytes+maxPeerByteCreep)
	if len(markdown) > pmdoc.MaxDocumentBytes+maxPeerByteCreep {
		t.Fatalf("a room loaded with a backlog took the update that drained it and renders %d bytes, past the %d the document bound and its creep allow",
			len(markdown), pmdoc.MaxDocumentBytes+maxPeerByteCreep)
	}
}

// Parked content that swaps drains no room past the document bound. eight browsers each park one
// character behind one they keep back, and then each of eight frames sends one of those
// characters, releasing that browser's parked struct, and parks 900 KiB of a browser of its own
// behind a character the room never receives. Each swap leaves eight structs parked, so no count
// of the queue tells it from an apply that left the queue alone, and one update filling every gap
// would land all eight frames at once. The room's apply check weighs that drain on what it lands
// and discards it where it would render past the bound (renderCheck, leaveParkedOut), and the
// queue's bytes are charged on every apply that leaves it holding anything (noteQueued), so the
// swaps are weighed against the state cap as they park.
func TestParkedContentThatSwapsDrainsNoRoomPastTheDocumentBound(t *testing.T) {
	const swaps, each = 8, 900 << 10
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
	var small, swapping, fills [][]byte
	for range swaps {
		keeper, into := peerDocument(t, room)
		keeper.Transact(func(txn *crdt.Transaction) { into.Insert(txn, 0, "a", nil) })
		kept := crdt.EncodeStateAsUpdateV1(keeper, room.StateVector())
		held := keeper.StateVector()
		keeper.Transact(func(txn *crdt.Transaction) { into.Insert(txn, 1, "t", nil) })
		small = append(small, crdt.EncodeStateAsUpdateV1(keeper, held))

		parker, at := peerDocument(t, room)
		parker.Transact(func(txn *crdt.Transaction) { at.Insert(txn, 0, "a", nil) })
		fills = append(fills, crdt.EncodeStateAsUpdateV1(parker, room.StateVector()))
		held = parker.StateVector()
		parker.Transact(func(txn *crdt.Transaction) { at.Insert(txn, 1, strings.Repeat("p", each), nil) })
		swap := mergeStoredOrder(t, kept, crdt.EncodeStateAsUpdateV1(parker, held))
		swapping = append(swapping, swap)
	}
	parks := mergeStoredOrder(t, small...)
	if err, called := weighInto(service, room, parks); err != nil || !called {
		t.Fatalf("the frame parking one character for each of %d browsers was answered %v (apply ran %t), want it admitted", swaps, err, called)
	}
	t.Logf("one %d-byte frame parks %d structs; the room's queue is charged %d bytes, rendering %d",
		len(parks), room.PendingStats().Items, service.roomWeighingOf(room).applies().queuedBytes(), publish())
	for i, swap := range swapping {
		found := room.PendingStats().Items
		err, called := weighInto(service, room, swap)
		t.Logf("swap %d: %d bytes, hook=%v, apply ran %t; parked structs %d -> %d; queue charged %d bytes; rendering %d bytes",
			i+1, len(swap), err, called, found, room.PendingStats().Items, service.roomWeighingOf(room).applies().queuedBytes(), publish())
	}
	fill := mergeStoredOrder(t, fills...)
	err, called := weighInto(service, room, fill)
	rendered := publish()
	t.Logf("one %d-byte fill of every gap: hook=%v, apply ran %t; parked structs %d; rendering %d bytes (document bound %d, creep cap %d)",
		len(fill), err, called, room.PendingStats().Items, rendered, pmdoc.MaxDocumentBytes, pmdoc.MaxDocumentBytes+maxPeerByteCreep)
	if rendered > pmdoc.MaxDocumentBytes+maxPeerByteCreep {
		t.Fatalf("one browser's swaps of parked content, then one fill, left the room rendering %d bytes, past the %d the document bound and its creep allow",
			rendered, pmdoc.MaxDocumentBytes+maxPeerByteCreep)
	}
}

// gcStructsOfClientsOfTheirOwn is a frame of n GC structs, each of a client of its own from first
// at its client's clock 0, so each integrates and the state's delete set gains a range for each:
// structs that carry no content for ygo's byte count to see (crdt.ApplyBudget.Bytes).
func gcStructsOfClientsOfTheirOwn(first uint64, n int) []byte {
	f := newV1Frame(n)
	for client := range uint64(n) {
		f = f.group(1, first+client, 0).gc(1)
	}
	return f.noDeletes()
}

// Structs that carry no content stop at the state cap: each struct an update carries is charged
// bytesOfStateAStruct of what the cap leaves (peerApplyAllowance), so a frame of GC structs, each
// of a client of its own and 15 bytes of state, is admitted only while the room has that price
// for each of its structs left. Charged nothing, one frame of 50,000 into a room tracked 1,000
// bytes short of the cap was admitted and left it 749,004 bytes past it, and six frames of 20,000
// into a room 1.5 MiB short left it 227,150 past it. Charged, the first room takes none of its
// frames, and the second takes three, each of 300,002 bytes of state, and stops 672,856 bytes
// short of the cap, where less than the 960,000 a frame of 20,000 is priced at is left.
func TestFramesOfStructsCarryingNoContentStopAtTheStateCap(t *testing.T) {
	for _, row := range []struct {
		name            string
		left            int
		frames, structs int
	}{
		{"a room 1,000 bytes short of the cap", 1_000, 4, 50_000},
		{"a room 1.5 MiB short of the cap", 3 << 19, 6, 20_000},
	} {
		t.Run(row.name, func(t *testing.T) {
			service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph.\n", 0)
			defer runtime.KeepAlive(replica)
			service.chargeLoadedRoom("room", replica, int64(maxRoomStateBytes-row.left))
			admitted, refused := 0, 0
			for i := range row.frames {
				tracked := service.roomStateBytes(room)
				frame := gcStructsOfClientsOfTheirOwn(uint64(9_500_001+i*row.structs), row.structs)
				err, called := weighInto(service, room, frame)
				after := service.roomStateBytes(room)
				t.Logf("frame %d of %d GC structs, %d bytes: hook=%v applied=%t; tracked %d -> %d, %d short of the cap",
					i+1, row.structs, len(frame), err, called, tracked, after, maxRoomStateBytes-after)
				if after > maxRoomStateBytes {
					t.Fatalf("frame %d took the room's tracked state to %d, %d past the %d one room may hold",
						i+1, after, after-maxRoomStateBytes, maxRoomStateBytes)
				}
				if err == nil {
					admitted++
					continue
				}
				var closed *websocket.PeerClose
				if !errors.As(err, &closed) || closed.Code != contracts.DocumentTooLargeCloseCode {
					t.Fatalf("frame %d was answered %v, want it admitted or refused %d", i+1, err, contracts.DocumentTooLargeCloseCode)
				}
				refused++
			}
			if refused == 0 {
				t.Fatalf("all %d frames were admitted into a room %d bytes short of the cap", admitted, row.left)
			}
		})
	}
}

// A used-up room the rebuild route would not take is not offered a rebuild. The hook's scan total
// is what the room has scanned since its load, apply by apply; the route's test is a re-scan of
// the room's state as one fresh decode (nearACap, rescan), which integrates the state's client
// groups by descending client and breaks at the first of a run of inserts naming one anchor and
// no right origin. So browsers sending one such insert a frame after a typed run each scan the
// run as they arrive, and use the total up, while the room's state re-scans a few hundred items.
// The update the used-up room then refuses for its scans was closed DOC_NEEDS_REBUILD, offering a
// rebuild the route answered 409 DOCUMENT_LIVE; it is DOC_TOO_TANGLED now, as an update too
// tangled for what is left (rebuildTakes), and the room's next load re-measures its total from the
// re-scan. A room whose state re-scans near the total is still offered the rebuild
// (TestAUsedUpRoomStillTakesTypingAndRefusesALargerApplyAsNeedingARebuild).
func TestAUsedUpRoomTheRouteWouldNotRebuildIsNotOfferedARebuild(t *testing.T) {
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n", 0)
	defer runtime.KeepAlive(replica)
	// A run of 20,000 keystrokes, each its own item, after the paragraph's last character: each
	// insert below names the run's first and no right origin, so it scans the run as it arrives.
	const typist crdt.ClientID = 5
	const typed = 20_000
	chainedRun(t, room, lastCharacter(t, room), typist, typed)
	anchor := crdt.ID{Client: typist}
	client := crdt.ClientID(100)
	for frames := 0; ; frames++ {
		if _, left := replica.scans.figures(); left <= usedUpScanBudget {
			break
		}
		if frames == 200 {
			t.Fatal("two hundred scanning frames left the room's scan total unused")
		}
		weighInto(service, room, scanningFrame(client, anchor, 1))
		client++
	}
	used, _ := replica.scans.figures()
	rescans, err := rescan(crdt.EncodeStateAsUpdateV1(room, nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d scanning frames used the room's scan total up at %d; its state re-scans %d items, against the %d less %d the route reads near",
		client-100, used, rescans, roomScanTotal, honestHandoffScans)
	if rescans+honestHandoffScans >= roomScanTotal {
		t.Fatalf("the room's state re-scans %d items, near the total: this is not the shape where the two readings differ", rescans)
	}
	err, called := weighInto(service, room, scanningFrame(client, anchor, 1))
	why, near, nearErr := nearACap(context.Background(), room, service.copyOfRoom(room), int64(service.roomStateBytes(room)))
	t.Logf("the used-up room's next scanning frame: hook=%v applied=%t; the route reads near=%t (%q, %v)", err, called, near, why, nearErr)
	var closed *websocket.PeerClose
	if !called || !errors.As(err, &closed) || closed.Code != contracts.DocumentTooLargeCloseCode {
		t.Fatalf("the hook answered the frame %v (applied %t), want it refused %d", err, called, contracts.DocumentTooLargeCloseCode)
	}
	if closed.Reason == contracts.DocumentNeedsRebuildCloseReason && !near {
		t.Fatalf("the used-up room closed the browser %q, whose editor offers the rebuild, and the route reads it far, so it answers that offer 409 DOCUMENT_LIVE", closed.Reason)
	}
	if closed.Reason != contracts.DocumentTooTangledCloseReason {
		t.Fatalf("the used-up room closed the browser %q, want %q", closed.Reason, contracts.DocumentTooTangledCloseReason)
	}
}
