package docs

// R7 fork review, finding "a parked room's answer is granted the whole copy heap".
//
// Dispatch's Service.weighStateAnswer grants each browser's first sync a share of the pod's copy
// heap by the path it believes ygo's answer takes (answerWeight, websocket.go:943): a room that
// parks nothing is cut from its own structs (cutAnswerWeight), and a room that parks anything is
// cut from its *encoded state*, "which decodes the whole document twice" (websocket.go:883-885,
// copy_heap.go:52-55), so it is charged parkedAnswerWeight - 149 per cent of a whole copy of the
// room, capped at one whole load slot.
//
// At the tag this branch pins, github.com/theorem-labs/ygo v1.55.1-theorem.6, there is no such
// path: crdt.SplitStateAsUpdateV1 walks the document's own structs for every room and reads back
// only what the room parks, one parked struct at a time (crdt/split.go:81-115, readParked at
// :155). The fork's own release notes for v1.55.1-sami.14, carried by this tag, say so: a
// 1,000,000-struct room holding one parked struct fell from 6,005,774 allocations of 844 MB to
// 683 of 8.4 MB, "as the same room with nothing parked does".
//
// These tests measure both halves of that at the pinned tag.

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/reearth/ygo/crdt"
	ygsync "github.com/reearth/ygo/sync"
)

// r7ForkAnswerCost is what one answer of doc's whole state to a browser holding nothing
// allocates: the bytes and the objects, with the frames held until both are read.
func r7ForkAnswerCost(t *testing.T, doc *crdt.Doc) (bytes uint64, objects uint64, frames int) {
	t.Helper()
	step1 := ygsync.EncodeSyncStep1(crdt.New())
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	reply, err := ygsync.EncodeSyncStep2Split(doc, step1, stateAnswerMessageBytes)
	if err != nil {
		t.Fatalf("answer the browser: %v", err)
	}
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(reply)
	return after.TotalAlloc - before.TotalAlloc, after.Mallocs - before.Mallocs, len(reply)
}

// r7ForkParkOneStruct parks one struct in doc with the 11-byte frame this package's own
// parked_row_first_test.go crafts: one struct of client 1000 at clock 5, whose clocks 0 to 4 the
// document does not hold, so it waits behind that gap for ever.
func r7ForkParkOneStruct(t *testing.T, doc *crdt.Doc) {
	t.Helper()
	frame := parkedOverTheKeystrokes()
	if len(frame) != 11 {
		t.Fatalf("the crafted frame is %d bytes, the finding's is 11", len(frame))
	}
	if err := crdt.ApplyUpdateV1(doc, frame, nil); err != nil {
		t.Fatalf("park the crafted struct: %v", err)
	}
	if parked := doc.PendingStats(); parked.Items != 1 {
		t.Fatalf("the document parks %+v after the crafted frame, want its one struct", parked)
	}
	if !parks(doc) {
		t.Fatal("the document does not read as parking anything after the crafted frame")
	}
}

// One 11-byte frame that parks a struct moves a room onto answerWeight's parked branch, and that
// branch grants an answer many times what the answer actually costs at this pin: the same room
// answers the same frames, allocating the same bytes, parked or not.
func TestR7ForkAParkedRoomsAnswerCostsWhatTheUnparkedRoomsDoes(t *testing.T) {
	doc := seededDocumentCopy(t)
	chainedRun(t, doc, lastCharacter(t, doc), 1_000, 40_000)

	state := int64(len(crdt.EncodeStateAsUpdateV1(doc, nil)))
	structs := int64(doc.StructCount())
	cutGrant := answerWeight(doc, state, structs)
	cutBytes, cutObjects, cutFrames := r7ForkAnswerCost(t, doc)

	r7ForkParkOneStruct(t, doc)
	parkedState := int64(len(crdt.EncodeStateAsUpdateV1(doc, nil)))
	parkedGrant := answerWeight(doc, parkedState, structs+1)
	parkedBytes, parkedObjects, parkedFrames := r7ForkAnswerCost(t, doc)

	t.Logf("room: %d structs, %d clients, state %d bytes", structs, doc.ClientCount(), state)
	t.Logf("parks nothing: grant %d (cutAnswerWeight), answer allocated %d bytes in %d objects, %d frames",
		cutGrant, cutBytes, cutObjects, cutFrames)
	t.Logf("parks one struct: grant %d (parkedAnswerWeight), answer allocated %d bytes in %d objects, %d frames",
		parkedGrant, parkedBytes, parkedObjects, parkedFrames)
	t.Logf("the grant grew %.1fx on one 11-byte frame; the answer's own allocation grew %.2fx",
		float64(parkedGrant)/float64(cutGrant), float64(parkedBytes)/float64(cutBytes))

	// The library's answer costs the same either way at this pin, so the grant must too, within
	// the margin a parked struct's own read-back costs.
	if parkedGrant > cutGrant*2 {
		t.Fatalf("one parked struct multiplied the grant by %.1f (%d -> %d) while the answer it "+
			"pays for allocated %d bytes against %d: answerWeight charges a path "+
			"theorem-labs/ygo v1.55.1-theorem.6 does not take",
			float64(parkedGrant)/float64(cutGrant), cutGrant, parkedGrant, parkedBytes, cutBytes)
	}
}

// The grant a parked room's answer takes, by the arithmetic of answerWeight's two branches, for
// rooms this branch's own bounds allow: the decode cap is 1,048,576 structs (maxUpdateItems) and
// the state cap 16,777,216 bytes (maxRoomStateBytes). The whole pod's copy heap at the
// calibration is one slot, loadSlotBytes.
func TestR7ForkTheParkedGrantReachesTheWholeCopyHeapSlot(t *testing.T) {
	for _, room := range []struct {
		name           string
		state, structs int64
		clients        int64
	}{
		{"a run of a million keystrokes", 7_800_000, 1_000_010, 2},
		{"640,000 structs, under the decode cap", 5_000_000, 640_000, 2},
		{"a mebibyte of prose", 1_050_000, 12, 2},
		{"a document at the state cap", maxRoomStateBytes, 200_000, 2},
	} {
		cut := cutAnswerWeight(room.state, room.clients, room.structs)
		parked := parkedAnswerWeight(cut, crdt.PendingStats{Items: 1})
		t.Logf("%s: cut grant %d, parked grant %d (%.1fx), one load slot %d, pool at the calibration %d",
			room.name, cut, parked, float64(parked)/float64(cut), int64(loadSlotBytes),
			int64(BoundsForLimit(0).LoadSlots)*loadSlotBytes)
		if parked == loadSlotBytes {
			t.Logf("  -> one browser joining %s while it parks one struct takes the whole slot, "+
				"which is the whole pool at the calibration (LoadSlots=%d)",
				room.name, BoundsForLimit(0).LoadSlots)
		}
	}
	// The bound the fix gives: a parked room's grant stays within the same factor of what its
	// own answer allocates as the same room's grant parking nothing. Each room is answered
	// parking nothing and then parking the finding's one struct, and each grant is held against
	// what that answer allocates, the fewest bytes of five answers after one that warms what the
	// answer reuses (answerAllocation): one answer alone read 2,019,832 and 2,305,248 bytes for
	// the same keystroke room. One parked struct moves the factor by its read-back alone, so it
	// may move by a tenth at most.
	step1 := ygsync.EncodeSyncStep1(crdt.New())
	for _, shape := range []struct {
		name string
		doc  func(t *testing.T) *crdt.Doc
	}{
		{"a run of 40,000 keystrokes", func(t *testing.T) *crdt.Doc {
			doc := seededDocumentCopy(t)
			chainedRun(t, doc, lastCharacter(t, doc), 1_000, 40_000)
			return doc
		}},
		{"640,000 GC structs, under the decode cap", func(t *testing.T) *crdt.Doc { return smallStructsDocument(t, 0, 0, 640_000) }},
		{"a mebibyte of prose", func(t *testing.T) *crdt.Doc { return residentRoomDocument(t, 1<<20) }},
	} {
		doc := shape.doc(t)
		cutGrant := answerWeight(doc, int64(len(crdt.EncodeStateAsUpdateV1(doc, nil))), int64(doc.StructCount()))
		cutBytes := answerAllocation(t, doc, step1)
		r7ForkParkOneStruct(t, doc)
		parkedGrant := answerWeight(doc, int64(len(crdt.EncodeStateAsUpdateV1(doc, nil))), int64(doc.StructCount()+doc.PendingStats().Items))
		parkedBytes := answerAllocation(t, doc, step1)
		cutFactor, parkedFactor := float64(cutGrant)/float64(cutBytes), float64(parkedGrant)/float64(parkedBytes)
		what := fmt.Sprintf("%s, %d structs", shape.name, doc.StructCount())
		t.Logf("%s: parking nothing, grant %d for an answer allocating %d (%.2f of it); parking one struct, grant %d for %d (%.2f of it)",
			what, cutGrant, cutBytes, cutFactor, parkedGrant, parkedBytes, parkedFactor)
		if parkedFactor > cutFactor*1.1 {
			t.Errorf("%s parking one struct is granted %.2f times what its answer allocates, against %.2f parking nothing: the grant prices a path theorem-labs/ygo v1.55.1-theorem.6 does not take",
				what, parkedFactor, cutFactor)
		}
	}
}
