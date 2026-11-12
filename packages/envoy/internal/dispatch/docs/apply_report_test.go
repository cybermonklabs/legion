package docs

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/contracts"
)

// What a resident room holds is the library's to say: ygo reports, as each apply that commits
// ends, the structs the document then holds with those parked in its pending queue
// (crdt.WithApplyReport), and the room keeps that figure (roomApplies.structs). Its charge, the
// copies it is weighed for and the allowance of each browser's update are built from it, and
// this package reads the document itself at two paths alone, which carry no report: the room's
// load from its stored state and a compaction's re-base (anchorStructs).
//
// Those two readings are the anchor, and this is what holds them to the reports: for each shape
// the package's drives build, a room is loaded and driven through the hook as a browser drives
// it, and the figure the applies reported must be the figure a reading of the document finds.
// A difference is a report that has stopped answering for the room - the charge and the next
// update's allowance would be built from a figure the document no longer stands at - so any
// difference at all fails here.
//
// The shapes are the ones the room budget's own measurements are made of (room_struct_price_test
// .go, room_heap_test.go) with the two the reports have to count rather than the store:
//
//   - a mebibyte of prose typed and pasted into, the control;
//   - a room of empty elements, which holds a struct an element and almost no content;
//   - a run of characters with every other one deleted, which splits one item into a struct for
//     each character, none of which the update carried;
//   - a room holding content parked behind a clock gap, which the store's own count leaves out
//     (dispatch://INFRA-179, review 6's finding 2);
//   - a room holding the margin's map keys, a struct and a value of any size apiece.
//
// They are built at smaller sizes than the heap measurements use: what the reports count turns
// on a room's shape and not on how much of it there is, and these run inside the package's own
// suite rather than in a process of their own.
func TestTheAnchorFindsNoDifferenceFromWhatTheRoomsAppliesReported(t *testing.T) {
	for _, shape := range []struct {
		name  string
		build func(t *testing.T) (state []byte, updates [][]byte)
	}{
		{"a mebibyte of prose typed and pasted into", func(t *testing.T) ([]byte, [][]byte) {
			state, _ := residentRoomState(t, 1<<20)
			return state, typedAndPastedUpdates(t, state, 100, 10, 2, 1<<10)
		}},
		{"20,000 empty paragraphs with a thousand more inserted", func(t *testing.T) ([]byte, [][]byte) {
			state := emptyElementState(t, 20_000)
			return state, [][]byte{elementInsert(t, state, 1_000)}
		}},
		{"a run of 100,000 characters with every other one deleted", func(t *testing.T) ([]byte, [][]byte) {
			before, cut := splitDenseRoom(t, 100_000)
			return before, [][]byte{cut}
		}},
		{"two frames parked behind a clock gap", func(t *testing.T) ([]byte, [][]byte) {
			return seededState(t), [][]byte{parkedGCFrame(8_100_001, 2_000), parkedGCFrame(8_100_002, 3_000)}
		}},
		{"eight margin records under keys of their own", func(t *testing.T) ([]byte, [][]byte) {
			updates := make([][]byte, 0, 8)
			for i := range 8 {
				updates = append(updates, marginRecord(uint64(7_100_001+i), 0, fmt.Sprintf("k%02d", i), "a margin record"))
			}
			return seededState(t), updates
		}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			state, updates := shape.build(t)
			room := loadAndType(t, state, updates)
			defer runtime.KeepAlive(room)
			applies := room.replica.applies()
			reported, ok := applies.reportedStructs()
			if !ok {
				t.Fatalf("the room took %d updates through the hook and none of them reported what it holds", len(updates))
			}
			parked := room.live.PendingStats()
			read := anchorStructs("room", applies, room.live)
			t.Logf("%d updates left the room reporting %d structs; the anchor reads %d, of which %d are in the store and %d parked behind %d gaps",
				len(updates), reported, read, room.live.StructCount(), parked.Items, len(parked.MissingFor))
			if reported != read {
				t.Fatalf("the room's applies reported %d structs and the anchor reads %d, a difference of %d: the charge and the next update's allowance are built from the report",
					reported, read, int64(reported)-int64(read))
			}
			if structs := applies.structsHeld(room.live); structs != reported {
				t.Fatalf("the room is weighed at %d structs, want the %d its last apply reported", structs, reported)
			}
			// The bytes are the other half of the anchor, and they are an accumulation rather than
			// a total: each committed update's own logged bytes, its parked ones included, are
			// added to the room's tracked state (renderedReplica.chargeAdded), because a total
			// needs a whole-state encode and that costs 53 ms on a room of 494,309 structs against
			// a keystroke answered in 128 ms. An accumulation of overlapping updates runs at or
			// above the merged state, never below it, and the load and the compaction re-base it
			// to what they read (chargeLoadedRoom, rebaseRoomState). So the anchor's difference
			// for bytes is held to never under-charge: a tracked state below the encode would be a
			// path that adds to the room without reporting through the one tally.
			tracked := room.service.roomStateBytes(room.live)
			encoded := len(crdt.EncodeStateAsUpdateV1(room.live, nil))
			t.Logf("the room's tracked state is %d bytes and an encode of it holds %d, %d over", tracked, encoded, tracked-encoded)
			if tracked < encoded {
				t.Fatalf("the room's tracked state is %d bytes, %d under the %d an encode of it holds: some path added to the room without reporting through its tally",
					tracked, encoded-tracked, encoded)
			}
		})
	}
}

// A room that has taken no apply since its load has no report to read: ygo gives the document
// the report as the load's replica is kept (keepReplica, newRoomApplies), after the load has
// decoded the stored state into it (bootstrapLoadedState), so the load's own decode reports
// nothing and the room's first update arrives with the record empty. Such a room is weighed
// from a reading of the document instead (heldStructs), by the same three readers the report
// answers once there is one: the budget's charge, the copy heap and the hook's allowance. The
// first apply that commits then takes over, and the allowance of the update after it is the
// report's.
func TestARoomWithNoReportYetIsWeighedFromItsDocument(t *testing.T) {
	state, _ := residentRoomState(t, 64<<10)
	room := loadAndType(t, state, nil)
	defer runtime.KeepAlive(room)
	applies := room.replica.applies()
	if reported, ok := applies.reportedStructs(); ok {
		t.Fatalf("a room that has taken no apply since its load reports %d structs, want no report at all", reported)
	}
	read := heldStructs(room.live)
	if structs := applies.structsHeld(room.live); structs != read {
		t.Fatalf("the room is weighed at %d structs with no report to read, want the %d its document holds", structs, read)
	}
	if want, charged := roomWeight(int64(len(state)), int64(read)), chargedWeight(room.charge); charged != want {
		t.Fatalf("the budget charges the loaded room %d bytes, want the %d its %d bytes of state and its %d structs come to",
			charged, want, len(state), read)
	}
	tracked := room.replica.roomStateBytes()
	if want, cost := copyWeight(tracked, int64(read)), room.service.copyOfRoom(room.live); cost != want {
		t.Fatalf("a whole copy of the room costs the load pool %d bytes, want the %d its %d tracked bytes and its %d structs come to",
			cost, want, tracked, read)
	}

	// The first update's allowance: built from the reading, since no apply has reported.
	update := peerTyping(t, room.live, "a browser's first keystrokes")
	rendering, measured := room.replica.size()
	want := peerApplyAllowance(int64(len(update)), rendering, tracked, read, measured, peerPressure{})
	if given := weighOneUpdate(t, room, update); given != want.budget {
		t.Fatalf("the first update after the load applied under %+v, want the %+v a room of %d structs allows", given, want.budget, read)
	}
	reported, ok := applies.reportedStructs()
	if !ok {
		t.Fatal("the room took an update through the hook and the apply that committed it reported nothing")
	}
	if held := heldStructs(room.live); reported != held {
		t.Fatalf("the admitted update reported %d structs and the document holds %d", reported, held)
	}
	if reported <= read {
		t.Fatalf("the admitted update left the room reporting %d structs, no more than the %d it was loaded at: it carried none", reported, read)
	}
	t.Logf("the loaded room was weighed at the %d structs its document holds, and its first update left it reporting %d", read, reported)

	// The update after it is bounded by the report rather than by a reading of its own.
	second := peerTyping(t, room.live, "more")
	rendering, measured = room.replica.size()
	want = peerApplyAllowance(int64(len(second)), rendering, room.replica.roomStateBytes(), reported, measured, peerPressure{})
	if given := weighOneUpdate(t, room, second); given != want.budget {
		t.Fatalf("the update after the first applied under %+v, want the %+v the %d structs its room reported allow", given, want.budget, reported)
	}
}

// weighOneUpdate hands update to the hook as one browser's and returns the allowance the hook
// reserved for its apply, which is what ygo asks the room for as that apply begins
// (roomApplies.budget). The update must be admitted: an allowance is what this reads, and a
// refusal reserves none.
func weighOneUpdate(t *testing.T, room *typedResidentRoom, update []byte) crdt.ApplyBudget {
	t.Helper()
	origin := new(int)
	var given crdt.ApplyBudget
	info := websocket.PeerUpdateInfo{Room: "room", Doc: room.live, Update: update, Origin: origin}
	err := room.service.weighPeerUpdate(info, func() error {
		given = room.replica.applies().budget(origin)
		return crdt.ApplyUpdateV1(room.live, update, origin)
	})
	if err != nil {
		t.Fatalf("the hook answered %v, want the update admitted", err)
	}
	return given
}

// The cap the hook holds a room to is the decode cap, and what a load decodes is the room's
// encoded state, which carries the pending queue beside the store (withParked,
// crdt/update.go:268-296). The hook now asks that cap of the structs the room reports holding,
// where it asked it of a ceiling that counted each parked struct twice over, for a skip over
// the gap it waits behind (structCeiling): this is what the weaker counting leaves, driven at
// the sizes that tell the two apart.
//
// Frames of 300,000 GC structs, each parked behind a gap of its own client. Two are admitted -
// 300,010 structs after the first, leaving headroom for 374,283 - where the doubled ceiling
// admitted one and refused the second at headroom 0. Measured here: the room is then left
// holding 10 structs in its store and 600,000 parked, its stored state is 1,200,262 bytes, and
// that state still decodes under the cap, because the merge that builds it writes one skip
// over each gap and not one over each parked struct - the structs of a client's contiguous
// clocks travel as one group. The third frame is refused DOC_NEEDS_REBUILD by the room's
// stalled queue (refusedStuckBacklog) before the cap is reached at all.
//
// So the room a browser can park its way into is still one that loads, which is what review
// 6's finding 2 asks for (TestR6Finding2ParkedStructsEscapeTheDecodeCap holds the same rule at
// the cap itself). What the doubling bought over this is a margin, not the bound.
func TestAParkedBacklogTheHookAdmitsLeavesTheRoomsStateDecodable(t *testing.T) {
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph.\n", 0)
	defer runtime.KeepAlive(replica)
	const perFrame = 300_000
	admitted := 0
	for client := uint64(8_200_001); client <= 8_200_004; client++ {
		err, called := weighInto(service, room, parkedGCFrame(client, perFrame))
		if err != nil {
			t.Logf("the frame of %d parked structs after %d admitted was refused: %v", perFrame, admitted, err)
			break
		}
		if !called {
			t.Fatalf("the hook admitted a frame without applying it")
		}
		admitted++
	}
	parked := room.PendingStats()
	state := crdt.EncodeStateAsUpdateV1(room, nil)
	_, err := decodeStoredCopy(state, maxUpdateItems)
	t.Logf("%d frames of %d parked structs admitted; the room holds %d structs in its store and %d parked, and its %d-byte stored state decodes: %v",
		admitted, perFrame, room.StructCount(), parked.Items, len(state), err == nil)
	if err != nil {
		t.Fatalf("the hook admitted %d frames of %d parked structs over a room of %d, and the room's stored state no longer decodes: %v",
			admitted, perFrame, room.StructCount(), err)
	}
}

// seededState is the stored state of a small document of ordinary prose, the base the shapes
// that park content and write margin keys are driven onto.
func seededState(t *testing.T) []byte {
	t.Helper()
	return crdt.EncodeStateAsUpdateV1(seededDocumentCopy(t), nil)
}

// parkedGCFrame is one browser's frame of n GC structs of client's, written from clock 1, so
// the room holds nothing of the clock they are written against and parks every one of them
// behind that gap. It is the frame review 6's finding 2 was found with, at a size that leaves
// the room far short of the decode cap.
func parkedGCFrame(client uint64, n int) []byte {
	frame := newV1Frame(1).group(n, client, 1)
	for range n {
		frame.gc(1)
	}
	return frame.noDeletes()
}

// elementInsert is one browser's update inserting elements empty paragraphs at the head of the
// room's fragment, in one transaction, as a browser pasting a run of blocks sends.
func elementInsert(t *testing.T, state []byte, elements int) []byte {
	t.Helper()
	browser := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(browser, state, nil); err != nil {
		t.Fatalf("open the document in the browser: %v", err)
	}
	fragment := browser.GetXmlFragment(fragmentName)
	browser.Transact(func(txn *crdt.Transaction) {
		for range elements {
			fragment.InsertElement(txn, 0, crdt.NewYXmlElement("paragraph"))
		}
	})
	return crdt.EncodeStateAsUpdateV1(browser, nil)
}

// A run of frames that each park content behind a gap of their own client is refused at the
// state cap, however many arrive. Each frame is under the document bound, so its allowance is
// what the state cap leaves; and an apply that only parks fires no update, so a tally built from
// committed updates never rose and every frame was given the whole headroom again. Measured
// before the fix, on a room tracked at nothing: 24 frames of 900 KiB were all admitted and the
// room's encoded state reached 22,119,318 bytes against the 16,777,216 one room may hold
// (dispatch://INFRA-179, found by the byte half of the anchor). One parked frame is refused at the
// cap and always was, which is why the review's one-frame test passed: the run is the shape that
// finds it, so the run is the test.
func TestARunOfParkedFramesIsRefusedAtTheStateCapHoweverManyArrive(t *testing.T) {
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph.\n", 0)
	defer runtime.KeepAlive(replica)
	text := strings.Repeat("m", 900<<10)
	admitted, refusedAt := 0, -1
	for i := range 40 {
		err, applied := weighInto(service, room, marginRecord(uint64(6_100_001+i), 5, "k", text))
		encoded := len(crdt.EncodeStateAsUpdateV1(room, nil))
		if encoded > maxRoomStateBytes {
			t.Fatalf("after %d parked frames the room's encoded state is %d bytes, past the %d one room may hold; the tally reads %d",
				i+1, encoded, maxRoomStateBytes, service.roomStateBytes(room))
		}
		if err == nil && applied {
			admitted++
			continue
		}
		var closed *websocket.PeerClose
		if !errors.As(err, &closed) || closed.Reason != contracts.DocumentNeedsRebuildCloseReason {
			t.Fatalf("parked frame %d was answered %v, want it admitted or refused at the state cap", i, err)
		}
		if refusedAt < 0 {
			refusedAt = i
		}
	}
	t.Logf("%d of 40 frames of 900 KiB parked behind gaps of their own were admitted; the first refused was frame %d; the room's encoded state is %d bytes of %d",
		admitted, refusedAt, len(crdt.EncodeStateAsUpdateV1(room, nil)), maxRoomStateBytes)
	if refusedAt < 0 {
		t.Fatalf("all 40 parked frames were admitted, want the state cap to refuse the run")
	}
}
