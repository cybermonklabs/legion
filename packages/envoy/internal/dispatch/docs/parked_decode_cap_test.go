package docs

import (
	"runtime"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// What a drain of parked content would land is charged to the decode cap as it parks, two items a
// struct: the room's stored state carries each parked struct with a skip over the gap it waits
// behind, so the hook asks the cap of each parked struct again (storedCeiling), and each struct
// an apply carries costs two items of the structs' share (capShares).
//
// Frames of 100,000 GC structs, each struct behind a one-clock gap of its own, are sent until one
// is refused. Charged two items a struct, the fifth meets a structs' share of 62,141 above a room
// whose state carries 800,010 items, and the state the four admitted frames leave still decodes.
// With the hook reading the room's structs as the apply's report counts them - each parked struct
// once (crdt.ApplyReport.Structs) - six frames were admitted and left a state carrying 1,200,010
// items, which the next load cannot decode (dispatch://INFRA-179, review 7).
//
// TestR7ParkedStructsBehindTheirOwnGapsPassTheDecodeCap no longer finds that: its frames are each
// the whole structs' share, and the state cap's price of a struct (bytesOfStateAStruct) now
// refuses its second frame first. These frames are far under that price.
func TestFramesParkedBehindGapsOfTheirOwnStopWhereTheStoredStateStillDecodes(t *testing.T) {
	const perFrame, most = 100_000, 12
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph.\n", 0)
	defer runtime.KeepAlive(replica)
	admitted := 0
	var refusal error
	for i := range most {
		err, called := weighInto(service, room, r7ParkedBehindSkips(uint64(9_100_001+i), perFrame))
		if err != nil {
			refusal = err
			break
		}
		if !called {
			t.Fatalf("the hook admitted frame %d without applying it", i+1)
		}
		admitted++
	}
	state := crdt.EncodeStateAsUpdateV1(room, nil)
	_, decodeErr := decodeStoredCopy(state, maxUpdateItems)
	t.Logf("%d frames of %d structs parked behind gaps of their own admitted, then %v; the room reports %d structs, its state carries %d by structCeiling (cap %d); its %d-byte state decodes: %v",
		admitted, perFrame, refusal, heldStructs(room), structCeiling(room), maxUpdateItems, len(state), decodeErr)
	if decodeErr != nil {
		t.Fatalf("the hook admitted %d frames of %d structs parked behind gaps of their own, and the room's stored state no longer decodes: %v",
			admitted, perFrame, decodeErr)
	}
	if refusal == nil {
		t.Fatalf("all %d frames of %d parked structs were admitted, want the decode cap to refuse the run", most, perFrame)
	}
}

// The decode cap's headroom above a room is split between what one apply may carry and what it
// may cut, and the structs' half is halved again, since a struct that parks behind a gap of its own
// costs the stored state two items (capShares): 2 x carried + cut is then the headroom itself.
// With the structs' half not halved, each struct bought one item of it, and one frame could spend
// both halves whole - carrying the structs' half in structs that each park behind a gap of their
// own, which the state carries twice over, and cutting the splits' half out of the items the room
// holds - and leave a state past the cap.
//
// That is this frame. The room is taken to 600,000 structs, and its headroom h is then 448,576;
// the frame parks h/4 + 1,000 GC structs, each behind a one-clock gap of its own, and its delete
// set cuts every other character of a heading's run, h/2 - 1,000 splits. Neither count passes the
// half of the headroom it is weighed against without the second halving, and together they pass
// the headroom by 1,000. The hook refuses the frame, since its structs pass the structs' share,
// and the room's stored state still decodes.
func TestOneFrameThatParksBehindGapsAndCutsItemsLeavesTheStoredStateDecodable(t *testing.T) {
	const filled, margin = 600_000, 1_000
	headroom := maxUpdateItems - filled
	cuts := headroom/4 - margin/2
	service, room, replica := peerUpdateRoom(t, "# "+strings.Repeat("a", 2*cuts+16)+"\n\nOne paragraph.\n", 0)
	defer runtime.KeepAlive(replica)
	// The cuts are made on the browser's copy of the small room, before the fill, so the delete
	// set names the heading's own clocks and nothing else; deleting from the end keeps each index
	// where the run first put it.
	browser, text := peerDocument(t, room)
	browser.Transact(func(txn *crdt.Transaction) {
		for j := cuts - 1; j >= 0; j-- {
			text.Delete(txn, 2*j+1, 1)
		}
	})
	cut := crdt.EncodeStateAsUpdateV1(browser, room.StateVector())
	if len(cut) == 0 || cut[0] != 0 {
		t.Fatalf("the browser's delete carries structs of its own (first byte %v), want a delete set alone", cut[:min(len(cut), 1)])
	}
	fillToStructs(t, service, room, filled)
	if ceiling := structCeiling(room); ceiling != uint64(filled) {
		t.Fatalf("the room's state carries %d items, want the %d this frame is sized for", ceiling, filled)
	}
	parked := headroom/4 + margin
	frame := newV1Frame(1).group(2*parked, 9_200_001, 0)
	for range parked {
		frame.skip(1).gc(1)
	}
	// The delete-only update's struct section is its first byte, no client; what follows is its
	// delete set, which this frame carries after its own structs.
	frame.raw(cut[1:]...)
	update := frame.done()
	before := crdt.EncodeStateAsUpdateV1(room, nil)
	allowance := peerApplyAllowance(int64(len(update)), 0, int64(service.roomStateBytes(room)), structCeiling(room), false, peerPressure{})
	err, called := weighInto(service, room, update)
	state := crdt.EncodeStateAsUpdateV1(room, nil)
	_, decodeErr := decodeStoredCopy(state, maxUpdateItems)
	t.Logf("a %d-byte frame parking %d structs behind gaps of their own and cutting %d items, into a room whose state carries %d of the %d cap (allowance Structs=%d Split=%d): hook=%v applied=%t; the state carries %d, and its %d bytes decode: %v",
		len(update), parked, 2*cuts, filled, maxUpdateItems, allowance.budget.Structs, allowance.budget.Split,
		err, called, structCeiling(room), len(state), decodeErr)
	if decodeErr != nil {
		t.Fatalf("one frame that parked %d structs behind gaps of their own and cut %d items was admitted, and the room's stored state no longer decodes: %v",
			parked, 2*cuts, decodeErr)
	}
	requireRefusedTooLarge(t, err, room, before)
}
