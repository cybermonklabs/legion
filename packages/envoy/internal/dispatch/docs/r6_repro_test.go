// Scratch reproductions for review 6 of sjawhar/legion PR #2544 at 0abf2096, package docs
// (packages/envoy/internal/dispatch/docs). Each test asserts the correct behaviour, so it FAILS at
// that head. Findings 1-4 of the review (see each test's comment).
//
// Run (from packages/envoy):
//
//	DISPATCH_TEST_DATABASE_URL=$LEGION_TEST_PG_DSN GOCACHE=$TMPDIR/gocache go test \
//	  -overlay /legion/scratch/tmp/r6repro/overlay-docs.json ./internal/dispatch/docs \
//	  -run 'TestR6' -count=1 -v -timeout 30m
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
)

// r6InsertMidRun is a browser's update inserting one character at each of the given indexes of the
// room's first text (descending, so each lands inside the original run).
func r6InsertMidRun(t *testing.T, room *crdt.Doc, indexes []int) []byte {
	t.Helper()
	browser, into := peerDocument(t, room)
	browser.Transact(func(txn *crdt.Transaction) {
		for _, at := range indexes {
			into.Insert(txn, at, "x", nil)
		}
	})
	return crdt.EncodeStateAsUpdateV1(browser, room.StateVector())
}

func r6Decodes(room *crdt.Doc) error {
	_, err := decodeStoredCopy(crdt.EncodeStateAsUpdateV1(room, nil), maxUpdateItems)
	return err
}

// Finding 1 (peer_update.go:260-261): Structs and Split each get the whole decode-cap headroom,
// and the fork checks them separately, so k inserted structs + k splits end at cap+k.
func TestR6Finding1StructsAndSplitShareNoHeadroom(t *testing.T) {
	for _, k := range []int{1, 1000} {
		t.Run(map[int]string{1: "k=1 one mid-word character", 1000: "k=1000 mid-run characters"}[k], func(t *testing.T) {
			service, room, replica := peerUpdateRoom(t, "# "+strings.Repeat("a", 4*k+8)+"\n\nOne paragraph.\n", 0)
			defer runtime.KeepAlive(replica)
			fillToStructs(t, service, room, maxUpdateItems-k)
			indexes := make([]int, k)
			for i := range k {
				indexes[i] = 2*(k-i) + 1
			}
			update := r6InsertMidRun(t, room, indexes)
			before := room.StructCount()
			err, called := weighInto(service, room, update)
			after := room.StructCount()
			decodeErr := r6Decodes(room)
			t.Logf("k=%d cap=%d before=%d after=%d (cap%+d) hook=%v applied=%t stored-state decode=%v",
				k, maxUpdateItems, before, after, after-maxUpdateItems, err, called, decodeErr)
			if err == nil && (after > maxUpdateItems || decodeErr != nil) {
				t.Fatalf("admitted an update that left the room at %d structs (cap %d) and its state undecodable: %v", after, maxUpdateItems, decodeErr)
			}
		})
	}
}

// Finding 2 (peer_update.go:544): headroom reads info.Doc.StructCount(), which excludes the
// pending queue, so frames parked behind a clock gap escape the decode cap. At this head the gap
// fill itself is refused DOC_NEEDS_REBUILD by refusedStuckBacklog, but both parked frames are
// admitted and the room's stored state (EncodeStateAsUpdateV1, pending included) no longer
// decodes under decodeStoredCopy.
func TestR6Finding2ParkedStructsEscapeTheDecodeCap(t *testing.T) {
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph.\n", 0)
	defer runtime.KeepAlive(replica)
	fillToStructs(t, service, room, 600_000)
	const n = 448_568
	const a, b = uint64(8_000_001), uint64(8_000_002)
	parked := func(client uint64) []byte {
		f := newV1Frame(1).group(n, client, 1)
		for range n {
			f.gc(1)
		}
		return f.noDeletes()
	}
	for _, client := range []uint64{a, b} {
		frame := parked(client)
		err, called := weighInto(service, room, frame)
		t.Logf("parked frame client %d: %d bytes, hook=%v applied=%t, room StructCount=%d", client, len(frame), err, called, room.StructCount())
	}
	for _, client := range []uint64{a, b} {
		err, called := weighInto(service, room, newV1Frame(1).group(1, client, 0).gc(1).noDeletes())
		t.Logf("gap fill client %d: hook=%v applied=%t, room StructCount=%d", client, err, called, room.StructCount())
	}
	after := room.StructCount()
	state := crdt.EncodeStateAsUpdateV1(room, nil)
	_, decodeErr := decodeStoredCopy(state, maxUpdateItems)
	t.Logf("after: StructCount=%d (cap %d) excludes %d parked structs; stored state %d bytes, decodeStoredCopy=%v",
		after, maxUpdateItems, 2*n, len(state), decodeErr)
	if decodeErr != nil {
		t.Fatalf("the hook admitted %d parked structs over a room of %d (cap %d) and the room's stored state no longer decodes: %v", 2*n, after, maxUpdateItems, decodeErr)
	}
}

// Finding 3 (peer_update.go:273, 0006b73c): under a full budget Split = min(headroom, 8) per
// update with no cumulative bound, so a stream of small cutting deletes grows the room freely.
func TestR6Finding3PressureSplitsHaveNoCumulativeBound(t *testing.T) {
	const updates, perUpdate = 5000, 4
	service, room, replica := peerUpdateRoom(t, "# "+strings.Repeat("a", 3*updates*perUpdate+16)+"\n\nOne paragraph.\n", 0)
	defer runtime.KeepAlive(replica)
	fillRoomBudget(t, &service.budget)
	browser, text := peerDocument(t, room)
	start := room.StructCount()
	admitted := 0
	deleted := 0
	for u := range updates {
		// Original index 3j+1 sits at visible index 2j+1 once j earlier ones are gone; delete
		// the four of this update in descending order so none shifts the next.
		browser.Transact(func(txn *crdt.Transaction) {
			for i := perUpdate - 1; i >= 0; i-- {
				j := deleted + i
				text.Delete(txn, 2*j+1-0, 1)
			}
		})
		_ = u
		deleted += perUpdate
		update := crdt.EncodeStateAsUpdateV1(browser, room.StateVector())
		if err, called := weighInto(service, room, update); err == nil && called {
			admitted++
		} else {
			var close *websocket.PeerClose
			if errors.As(err, &close) && close.Reason == contracts.DocumentBusyCloseReason {
				break
			}
			t.Fatalf("update %d answered %v (applied %t)", u, err, called)
		}
	}
	end := room.StructCount()
	t.Logf("budget full throughout=%t; %d of %d updates of %d mid-run deletes admitted; structs %d -> %d",
		service.underPressure("room"), admitted, updates, perUpdate, start, end)
	if admitted == updates {
		t.Fatalf("all %d cutting updates were admitted under a full budget; the room grew from %d to %d structs", updates, start, end)
	}
}

// Finding 4 (rebuild.go:74-86): a paste refused DOC_NEEDS_REBUILD under the state cap leaves
// nearACap answering near=false, since it adds only smallestReplacementBytes (64), so the
// dashboard's rebuild offer is answered 409.
func TestR6Finding4StateCapRefusalYetNotNearACap(t *testing.T) {
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph.\n", 0)
	defer runtime.KeepAlive(replica)
	tracked := int64(maxRoomStateBytes - 10<<10)
	service.chargeLoadedRoom("room", replica, tracked)
	update := peerTyping(t, room, strings.Repeat("p", 20<<10))
	err, called := weighInto(service, room, update)
	var close *websocket.PeerClose
	refused := errors.As(err, &close) && close.Reason == contracts.DocumentNeedsRebuildCloseReason
	why, near, nearErr := nearACap(context.Background(), room, 1, tracked)
	t.Logf("20 KiB paste with %d bytes of headroom (tracked %d of %d): hook=%v applied=%t refusedNeedsRebuild=%t; nearACap near=%t why=%q err=%v",
		maxRoomStateBytes-tracked, tracked, maxRoomStateBytes, err, called, refused, near, why, nearErr)
	if refused && !near {
		t.Fatal("the paste was refused DOC_NEEDS_REBUILD but nearACap answers near=false, so the rebuild offer meets 409")
	}
}
