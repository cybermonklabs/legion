package docs

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// The room budget charges a room the larger of two terms, its tracked state at
// bytesOfHeapAStateByte and the structs it holds at bytesOfHeapAStruct, in each of its two
// copies (roomWeight). This measures the second term, as
// TestAResidentRoomsHeapPerByteOfItsState measures the first and by the same reading of the
// heap (heldByResidentRoom): the room itself - its live document beside the rendered replica its
// update observer keeps from the room's load on - against the structs it holds rather than
// against the bytes of its state.
//
// A second term is needed because a room's structs and a room's state bytes come apart. The
// shapes below are where they come apart furthest:
//
//   - a split-dense room: a run of a million characters, which ygo merges into one item, with
//     every other character of it deleted in one update, which splits that item into a million.
//     The room's state carries each of those splits, but the room's tracked state does not: what
//     the budget adds for that update is the update's own bytes, so one update takes the room
//     from a handful of structs to a million while its charge moves by the delete frame alone
//     (renderedReplica.chargeAdded). This is the shape the reviewer measured at 5.5 times its
//     charge (dispatch://INFRA-179, review 5891480448).
//   - a room of empty elements, which hold a struct each and almost no content.
//   - a room of elements outside the Proof schema, which render nothing at all and so publish no
//     measurement the byte bound could weigh.
//   - a mebibyte of prose, the control: the shape the budget is calibrated on
//     (roomBudgetBytes), whose heap the state term already covers, so the struct term must not
//     decide it.
//
// Each shape must hold no more than roomWeight charges a room of it, which is what keeps the
// budget's arithmetic honest. A shape whose heap the state term alone does not cover is one the
// per-struct price answers for, and the heaviest of those must hold at least leastOfThePrice of
// that price, which is what keeps the price from being set so far above every shape that the
// budget charges struct-dense rooms many times what they cost.
//
// Each row is measured in a process of its own (measuredAlone), as the per-byte rows are and for
// the same reason: every row collects eight times, and inside the package's whole run each
// collection walks the gigabyte or more that run holds by then.
func TestAResidentRoomsHeapPerStructItHolds(t *testing.T) {
	skipUnderRace(t)
	const leastOfThePrice = 0.75
	type shape struct {
		name  string
		state func(t *testing.T) []byte
	}
	shapes := []shape{
		{"a run of a million characters with every other one deleted", func(t *testing.T) []byte {
			return splitDenseState(t, splitDenseRun)
		}},
		{"200,000 empty paragraphs", func(t *testing.T) []byte { return emptyElementState(t, 200_000) }},
		{"200,000 elements outside the Proof schema", func(t *testing.T) []byte {
			return outsideSchemaState(t, 200_000)
		}},
		{"a mebibyte of prose", func(t *testing.T) []byte {
			state, _ := residentRoomState(t, 1<<20)
			return state
		}},
	}
	// heaviest is the most a shape the state term does not cover held for each struct in each
	// copy, and heaviestShape which shape that is. Each shape is measured in a process of its
	// own, so each reading is read back out of what that process printed (structPriceLine): the
	// heaviest of them is known only here, where the readings meet. A run that measured every
	// shape is the only one that can weigh the price against the heaviest of them, so a run of
	// one of them - the process a shape is measured in, which runs this test with that shape
	// alone - weighs none.
	heaviest, heaviestShape, read := 0.0, "", 0
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			output, here := measuredAloneOutput(t)
			if !here {
				reading, ok := structPriceReading(output)
				if !ok {
					t.Fatalf("the measurement in a process of its own printed no reading (%q)", structPriceLine)
				}
				read++
				if reading.coveredByTheStateTerm() {
					return
				}
				if perStruct := reading.bytesAStruct(); perStruct > heaviest {
					heaviest, heaviestShape = perStruct, shape.name
				}
				return
			}
			state := shape.state(t)
			structs, err := StateStructsForTest(state)
			if err != nil {
				t.Fatalf("walk the state this shape makes: %v", err)
			}
			if structs == 0 {
				t.Fatal("the shape's state holds no struct, so it prices none")
			}
			reading := structPrice{structs: structs, stateBytes: int64(len(state))}
			reading.held = heldByResidentRoom(t, state, false)
			t.Logf("%d structs in %d B of state; heap held %d B, %.2f B a struct and %.2f B a state byte for each of %d copies",
				reading.structs, reading.stateBytes, reading.held, reading.bytesAStruct(), reading.bytesAStateByte(), roomCopies)
			t.Logf("the state term charges %d B and the struct term %d B, so the room is charged %d B for the %d B it holds",
				reading.stateBytes*bytesOfHeapAStateByte*roomCopies, reading.structs*bytesOfHeapAStruct*roomCopies,
				roomWeight(reading.stateBytes, reading.structs), reading.held)
			fmt.Printf("%s%d %d %d\n", structPriceLine, reading.structs, reading.stateBytes, reading.held)
			if charged := roomWeight(reading.stateBytes, reading.structs); charged < reading.held {
				t.Errorf("the room holds %d B of heap and the budget charges it %d, under it: measure the prices again",
					reading.held, charged)
			}
		})
	}
	if read != len(shapes) {
		return
	}
	t.Logf("the heaviest of the %d shapes the state term does not cover is %s at %.2f B a struct, %.3f of the %d-byte price",
		read, heaviestShape, heaviest, heaviest/bytesOfHeapAStruct, bytesOfHeapAStruct)
	if heaviest == 0 {
		t.Fatal("the state term covered every shape measured, so none of them prices a struct: the shapes no longer reach where the two terms come apart")
	}
	if heaviest < leastOfThePrice*bytesOfHeapAStruct {
		t.Errorf("the heaviest shape the state term does not cover, %s, holds %.2f B of heap a struct, under %.2f of the %d the room budget charges: the price is set far past every shape",
			heaviestShape, heaviest, leastOfThePrice, bytesOfHeapAStruct)
	}
}

// splitDenseRun is the characters the split-dense shape types in one transaction before one
// update deletes every other one of them, which leaves the room a struct for each. It is half a
// million and not the 1,100,000 of the reviewer's room because that room's 1,100,002 structs
// are past the 1,048,576 ygo decodes in one update: its stored state could never be loaded
// again, which is the defect crdt.ApplyBudget.Split was cut for - every browser's allowance now
// divides what is left of the decode cap above what the room holds between the structs it
// carries and the items it splits, so the hook refuses that delete with DOC_NEEDS_REBUILD and
// leaves the room as it was (peerApplyAllowance). Half a million leaves the room about 500,002
// structs after the same cut, split-dense by the same wide margin - the state term
// under-charges it just as badly - and well inside the cap.
const splitDenseRun = 500_000

// splitDenseState is the state of a room whose first paragraph took a run of run characters in
// one transaction, which ygo merges into one item (squashRuns), and then one browser update
// deleting every other character of that run, which splits that one item into a struct for each
// character.
func splitDenseState(t *testing.T, run int) []byte {
	t.Helper()
	before, cut := splitDenseRoom(t, run)
	doc := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(doc, before, nil); err != nil {
		t.Fatalf("decode the run: %v", err)
	}
	held := doc.StructCount()
	if err := crdt.ApplyUpdateV1(doc, cut, nil); err != nil {
		t.Fatalf("delete every other character of the run: %v", err)
	}
	t.Logf("a run of %d characters took the room from %d structs to %d on one delete", run, held, doc.StructCount())
	return crdt.EncodeStateAsUpdateV1(doc, nil)
}

// splitDenseRoom is a room's stored state before the split-dense delete - a small document whose
// first paragraph took a run of run characters in one transaction, which ygo merges into one
// item - and the one browser update that deletes every other character of that run. The update
// is written by hand (splitEveryOtherCharacter) rather than made by a browser's own
// transaction, because a ygo text deletes by index and an index is found by walking the text,
// which would make run/2 deletes a walk of the run each.
func splitDenseRoom(t *testing.T, run int) (before, cut []byte) {
	t.Helper()
	doc := seededDocumentCopy(t)
	text := paragraphTexts(doc)[0]
	// The run is the document's own client's first items, so it takes clocks 0 to run-1: the
	// seed was written by the client encodeDocumentTree built it with, and this document has
	// written nothing before it.
	if held := doc.StateVector()[doc.ClientID()]; held != 0 {
		t.Fatalf("the seeded document already holds %d clocks of its own client, so the run does not start at clock 0", held)
	}
	chunk := strings.Repeat("x", 1<<10)
	at := text.Len()
	doc.Transact(func(txn *crdt.Transaction) {
		for typed := 0; typed+len(chunk) <= run; typed += len(chunk) {
			text.Insert(txn, at, chunk, nil)
			at += len(chunk)
		}
	})
	return crdt.EncodeStateAsUpdateV1(doc, nil), splitEveryOtherCharacter(doc.ClientID(), run)
}

// splitEveryOtherCharacter is one update of no structs whose delete set names every other clock
// of client's first run characters, from clock 1: the delete a browser sends that takes out every
// other character of a run, leaving the first. Each range is one clock and no two of them touch,
// so the run's one item is split into a struct for each character.
func splitEveryOtherCharacter(client crdt.ClientID, run int) []byte {
	ranges := run / 2
	frame := newV1Frame(0).uint(1).uint(uint64(client)).uint(uint64(ranges))
	for i := range ranges {
		frame = frame.uint(uint64(2*i + 1)).uint(1)
	}
	return frame.done()
}

// A room one browser update makes split-dense is charged no less than the heap its structs
// measure. This is the reviewer's room (dispatch://INFRA-179, review 5891480448) at the size
// the decode cap leaves room for (splitDenseRun): a document whose paragraph took a long run of
// characters in one transaction, which ygo merges into one item, and then one update deleting
// every other character of it, which splits that item into a struct for each character and
// takes the room from a handful of structs to half a million on one delete. The splits are not
// the update's own and the room stays inside the decode cap, so the hook admits the update,
// which is the point: the room is charged for what it took, not refused for it. What
// the drain adds to the room's tracked state is the delete frame's own bytes
// (renderedReplica.chargeAdded), so the state term alone charges the room a fraction of what
// its two copies then hold, and the struct term is what answers it.
//
// What it is weighed against is the structs' own measured heap, bytesOfHeapAStruct in each of
// the room's two copies (TestAResidentRoomsHeapPerStructItHolds measured that price on this
// very shape among others). Before the second term the room was charged its tracked state
// alone, far under that, which is the under-charge this holds against.
//
// The charge counts the room's ceiling and not its store alone (structCeiling): the run is 488
// chunks of 1,024 characters, 499,712 in all, while the delete frame names every other clock up
// to the 500,000 asked for, so its last 144 ranges name clocks the room does not hold and park.
// The room stores them and its next load decodes them, so the budget is charged for them too.
func TestASplitDenseRoomIsChargedNoLessThanItsStructsHeap(t *testing.T) {
	skipAtTheBoundsUnderRace(t)
	const run = splitDenseRun
	before, cut := splitDenseRoom(t, run)
	room := loadAndType(t, before, [][]byte{cut})
	structs := int64(room.live.StructCount())
	ceiling := int64(structCeiling(room.live))
	tracked := room.replica.roomStateBytes()
	charged := chargedWeight(room.charge)
	t.Logf("a run of %d characters with every other one deleted leaves the room %d structs, a ceiling of %d with what its delete frame parked, tracked at %d bytes of state: its state term charges %d bytes and its struct term %d, and it is charged %d",
		run, structs, ceiling, tracked, tracked*bytesOfHeapAStateByte*roomCopies, ceiling*bytesOfHeapAStruct*roomCopies, charged)
	if structs < run/2 {
		t.Fatalf("the room holds %d structs after the delete, want the %d the splits make: this is no longer the split-dense shape", structs, run/2)
	}
	if want := structs * bytesOfHeapAStruct * roomCopies; charged < want {
		t.Errorf("the room is charged %d bytes for the %d structs its two copies hold, under the %d those structs measure: the budget under-charges a split-dense room",
			charged, structs, want)
	}
	if charged != roomWeight(tracked, ceiling) {
		t.Errorf("the room is charged %d bytes, want the %d its %d tracked bytes of state and its ceiling of %d structs come to (roomWeight)",
			charged, roomWeight(tracked, ceiling), tracked, ceiling)
	}
}

// emptyElementState is the state of a room holding elements empty paragraphs and nothing else: a
// struct for each element, each carrying its tag and no content at all.
func emptyElementState(t *testing.T, elements int) []byte {
	t.Helper()
	return elementRunState(t, elements, "paragraph", false)
}

// outsideSchemaState is the state of a room holding elements of a tag the Proof schema has no
// node for: a room that renders nothing, so it publishes no measurement for the byte bound to
// weigh and the state is the only thing the budget could read of it.
func outsideSchemaState(t *testing.T, elements int) []byte {
	t.Helper()
	return elementRunState(t, elements, "widget", true)
}

// elementRunState is the state of a room whose fragment holds elements elements of tag, each
// inserted at the fragment's head so no insert walks the elements before it, and outside says
// whether the document those elements make must be outside the Proof schema, which is checked
// here rather than assumed.
func elementRunState(t *testing.T, elements int, tag string, outside bool) []byte {
	t.Helper()
	doc := newDocumentCopy(crdt.WithClientID(0x5eed))
	fragment := doc.GetXmlFragment(fragmentName)
	doc.Transact(func(txn *crdt.Transaction) {
		for range elements {
			fragment.InsertElement(txn, 0, crdt.NewYXmlElement(tag))
		}
	})
	if _, err := treeOf(doc); outside != (err != nil) {
		t.Fatalf("a document of %q elements reads %v, want outside the Proof schema %t", tag, err, outside)
	}
	return crdt.EncodeStateAsUpdateV1(doc, nil)
}

// seededDocumentCopy is a small document - a heading and one paragraph - in a document copy of
// its own client, which has written nothing of its own yet.
func seededDocumentCopy(t *testing.T) *crdt.Doc {
	t.Helper()
	tree, err := pmdoc.Parse("# Title\n\nOne paragraph of ordinary prose.\n")
	if err != nil {
		t.Fatal(err)
	}
	seed, err := encodeDocumentTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	doc := newDocumentCopy(crdt.WithClientID(0x5eed))
	if err := crdt.ApplyUpdateV1(doc, seed, nil); err != nil {
		t.Fatal(err)
	}
	return doc
}

// structPrice is one shape's reading: the structs its state holds, the bytes of that state, and
// the heap a room that loaded it holds in its two copies together (heldByResidentRoom).
type structPrice struct {
	structs    int64
	stateBytes int64
	held       int64
}

// bytesAStruct is the heap the shape holds for each struct in each copy, and bytesAStateByte the
// heap it holds for each byte of state in each copy, which is what
// TestAResidentRoomsHeapPerByteOfItsState prices.
func (p structPrice) bytesAStruct() float64 {
	return float64(p.held) / float64(p.structs*roomCopies)
}

func (p structPrice) bytesAStateByte() float64 {
	return float64(p.held) / float64(p.stateBytes*roomCopies)
}

// coveredByTheStateTerm reports whether the state term alone charges this shape no less than the
// heap it holds. Such a shape prices no struct: the larger of the two terms answers for it
// whatever the per-struct price is (roomWeight), so it is left out of the price the heaviest
// shape sets. The test does not read the per-struct price here, so which shapes set it does not
// depend on what it is set to.
func (p structPrice) coveredByTheStateTerm() bool {
	return p.stateBytes*bytesOfHeapAStateByte*roomCopies >= p.held
}

// structPriceLine is how a process measuring one shape reports its reading to the process that
// started it (measuredAloneOutput), which is where the shapes' readings meet: the structs, the
// state bytes and the heap held, in that order.
const structPriceLine = "heap-structs-state-held: "

// structPriceReading is the reading a measuring process printed, and whether it printed one.
func structPriceReading(output string) (structPrice, bool) {
	for line := range strings.SplitSeq(output, "\n") {
		printed, found := strings.CutPrefix(strings.TrimSpace(line), structPriceLine)
		if !found {
			continue
		}
		fields := strings.Fields(printed)
		if len(fields) != 3 {
			continue
		}
		var reading structPrice
		for at, into := range []*int64{&reading.structs, &reading.stateBytes, &reading.held} {
			figure, err := strconv.ParseInt(fields[at], 10, 64)
			if err != nil {
				return structPrice{}, false
			}
			*into = figure
		}
		return reading, true
	}
	return structPrice{}, false
}
