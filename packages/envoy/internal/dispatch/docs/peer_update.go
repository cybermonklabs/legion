package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"weak"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// This file holds the peer update hook itself (weighPeerUpdate), the allowance it builds for each
// browser's update, the roots it lets one name, and the closes it refuses one with.

// A browser's update reaches its room through no check of the server's own: applyLive weighs every
// write a caller makes by the document it leaves (growth.go), and a browser's update runs through
// none of it, so without a bound of its own a browser could carry a document past the bound every
// server write obeys (refuseGrowth). Two things bound it here: weighPeerUpdate, ygo's
// Server.OnPeerUpdate, which the room runs in place of its own apply for each update and
// whole-state message a read-write browser sends, and the room's apply check (renderCheck), which
// ygo runs inside that apply, on what it leaves, before it commits.
//
// The hook does not run applyLive's checks. Those weigh what a write leaves, which costs a fork
// of the document, a tree build and a full markdown render - about 420 ms for the fork alone at
// 1 MiB and a whole further copy of the document, with the render about 160 ms (AGENTS.md:563-602,
// :780-781) - against a keystroke on such a document that already takes between half a second and
// a second. What it does instead is hand ygo an allowance built from what it can see without
// reading the update at all - the update's own length, the structs the room's last committed
// apply reported it holding (crdt.ApplyReport.Structs, roomApplies.structsHeld), what its
// pending queue holds (crdt.Doc.PendingStats), the room's last published rendering
// (renderedReplica.publishSize), and its tracked state bytes with what its queue holds beside
// them - and let the apply itself count what the update decodes, integrates and adds against that
// allowance, rolling the whole apply back where it passes (crdt.WithApplyBudget). For a keystroke
// that is a handful of comparisons; nothing here walks the document, and the update itself is
// read only by the header scans a room can pay for - the first three numbers of every update,
// which name the one client a keystroke writes under (judgeClients, client_ids.go), and the whole
// header of an update writing under another client than its socket's and the retired clients'
// list of a rebuilt room (refusedBeforeApply) - none of which decodes any content.
//
// The hook weighs bytes and not elements. Counting a rendering's elements is a whole goldmark
// parse, 58 to 89 ms at 524 KiB, which no keystroke can pay on a one-processor pod. No cheap
// exact count exists either: a render that counts what it writes agreed with the server's count
// over 1,127 documents, but took 1,404 lines emulating goldmark and still disagreed for a live
// tree whose rendering reads back as something else, which is what a browser holds. So the
// element bound is kept where it already is: a server write carrying caller text is refused when
// it would leave a document past pmdoc.MaxDocumentElements and heavier than it was (weigh). What
// a browser can leave live past it - about 64 KiB of headings - goes on saving and syncing; the
// server's writes that would grow it are refused until someone shortens it, a write that leaves
// it no heavier passes, and a rebuild from its latest stored version restores it whatever it
// weighs (TestARebuildFromTheLatestVersionRestoresItWhateverItWeighs).
//
// The byte rule the allowance carries is not what holds the document to its bound. It weighs
// what an update carries against the room's last published rendering, and neither says what the
// update does to the rendering: where its items land and what a deleted format item leaves
// formatted depend on what the document holds, and a few bytes can grow the rendering by
// hundreds of kilobytes (renderCheck lists the shapes measured). The rendering is as recent as
// the room's last update observer, too, so the rule alone could be passed by the updates in
// flight when the room crosses it. What holds the bound is the room's apply check, which
// renders what each browser's apply leaves and refuses one whose rendering would be past the
// bound and its creep (maxPeerRenderingBytes) and larger than it was. The byte rule stays as the
// cheap test before the apply: it refuses a frame too large for the room before anything decodes
// it, holds what one update may carry into a document at the bound to what keeps an editor
// working (maxPeerInsertAtTheBound), and leaves one past the creep cap nothing to carry at all,
// so the check renders only what can pass it. What the room holds parked renders nothing until
// the apply that fills its gap lands it, and the check weighs that drain on what it lands: a drain
// it refuses is discarded, the update that filled the gap stands, and every browser that parked
// what was discarded is closed (closeDiscardedSenders).
//
// The document has a second bound of its own, which bytes cannot see: the decode cap. ygo decodes
// at most maxUpdateItems structs in one update, and a room's whole state is one update to the next
// load that reads it, so a room whose state holds more could never be loaded again once its
// updates were folded. A browser can get there without passing the byte bound - a frame of GC
// structs is two bytes a struct and renders nothing - so the apply's allowance of structs comes
// out of what is left of the cap above what the room holds: the structs the room's last committed
// apply reported it holding (roomApplies.structsHeld, which counts what the room has parked
// beside what its store holds), each parked struct again and each parked delete range
// (storedCeiling). Nothing a room parks is stored (leaveParkedOut), so those two terms no longer
// bound a state a load decodes but the room's pending queue in memory, out of the same headroom
// as what it integrates. An apply that would integrate more is refused with
// DocumentTooLargeCloseCode and taken back out whole.
//
// It can get there without integrating a struct at all. A delete inside a run cuts the item it
// falls in, and the halves are structs the room keeps until the items between them are gone:
// measured by the fork on a one-run text of "hello world", a delete of five characters inside
// the run cuts that one item into three (sjawhar/ygo v1.55.1-sami.8, crdt.ApplyBudget.Split,
// TestUnit_ApplyBudget_RefusesADeleteWhoseSplitsPassWhatItAllows). Those splits are not the
// update's own and Structs does not count them, so a room at the cap took a 17-byte delete and
// its stored state then answered `crdt: invalid update` on its next load - a document a person
// broke by deleting one character inside a word. Split is the second half of the same rule:
// Structs is what an apply may add by carrying, Split what it may add by cutting, and the cap
// bounds the two together by charging each what the room is weighed at for it against the cap
// (storedCeiling) - two items a struct, since a struct may park and the ceiling counts each
// parked struct twice to bound the queue in memory, and one a split - so that their sum is the
// headroom (capShares). One figure in both fields bounds neither, since
// ygo checks each field on its own: an update carrying k structs and cutting k items left a room
// k short of the cap k past it (TestR6Finding1StructsAndSplitShareNoHeadroom).
//
// The document has a third bound, which neither bytes nor structs see: what a room holds that is
// not text, a value of any size in one clock and one struct. The server's own writes bound the
// margin's records at maxMarginBytes and the anchor marks' ids at maxAnchorBytes (growth.go), and
// a browser's update runs through neither: measured on 2026-10-26, eighty browser updates each
// writing a million bytes into the margin were all admitted, and took the room's whole-state frame
// from 264 bytes to 80,001,457 with its rendering at 42, past the message floor every browser of
// the document then reconnects against. What answers that now is the state cap
// (maxRoomStateBytes), which bounds the room's whole encoded state whatever the bytes of it
// render. It bounds the apply's cutting and the structs it carries beside its content, since a
// split adds the second half's header to that state and a struct its own header, neither of which
// is content for ygo's byte count to see: a room the cap left no byte took a delete of 200,000
// mid-run cuts that added 4,575,248 bytes of state (TestR7SplitsOfADeleteTakeTheRoomPastTheStateCap,
// bytesOfStateASplit), and one frame of 50,000 GC structs took a room 1,000 bytes short of the cap
// 749,004 past it (TestFramesOfStructsCarryingNoContentStopAtTheStateCap, bytesOfStateAStruct).
//
// The hook has a third state, which is the room budget's rather than the document's: while the
// open rooms are past their budget and nothing can be closed (roomBudget.full), an apply may
// integrate no struct and add no byte of content in any room, and may cut at most pressureSplits
// of the items its room holds, so what is admitted is an update that grows the room in no measure
// or a person's own delete, and every other is refused with the transient DocumentBusyCloseCode.
// Bytes cannot be that test: an update can leave a document as long as it was, or shorter, and
// still multiply what its room holds - a delete of every other character of a run splits one item
// into a struct for each character, and one 2.2 MB frame took a room from 4 structs to 1,100,002
// and its two copies by 336.6 MB (roomWeight, room_prices.go). What an update integrates and what
// it cuts are what can grow the room, and an update that does neither is the one update that
// cannot. That refusal is not the document's own bound and is not folded into it: 4413 is
// permanent and the editor drops the refused edit, where 4503 is transient and the editor keeps
// it, to send once the pressure passes.
//
// Those cuts are bounded over the whole of the pressure and not only within one update: the room
// is marked at the ceiling it stood at as the pressure first weighed one of its updates, it may
// cut while it stands under pressureGrowth structs above that mark, and the end of the pressure
// clears the mark (splitsUnderPressure). Without it the eight an update was a bound on nothing:
// 5,000 updates of four mid-run deletes each were all admitted and took one room from 10 structs
// to 40,010 with the budget full throughout, which the pod has no memory for
// (TestR6Finding3PressureSplitsHaveNoCumulativeBound). A delete covering whole items cuts
// nothing and is admitted however long the pressure has run, so a full document can still be cut
// back down.
//
// The document has a fourth bound, which no count can see: the kinds of content it takes. One
// embed is one struct of a few bytes, inside every allowance above, and the Proof editor sends
// neither an embed nor binary content - pmdoc reads neither back as a document of the schema
// (pmdoc.ErrSchema, pmdoc/update.go) - so one crafted embed puts every browser of the document
// into the repair state until a person repairs it. Every room's document therefore names those
// two kinds as content a browser's apply may not carry (peerRefusedContent,
// crdt.WithRefusedContent): ygo refuses the whole apply where it reads one, the browser is closed
// with the document's own refusal, DOC_UPDATE_REFUSED, and the room is left as it was.
//
// The last of them is no bound at all but a drain the room cannot take. A drain of the room's
// pending queue that its apply check refuses, or whose scans or splits pass the allowance of the
// apply that filled the gap, is discarded whole while that apply stands, deliberately: the parked
// content is another sender's and the sender that filled the gap can neither see it nor send
// less of it (leaveParkedOut). Each browser that parked what was discarded is closed with the
// close its own frame would have earned (closeDiscardedSenders), and the room holds no backlog
// that no update will drain.

// maxPeerInsertAtTheBound is the most new content one update may carry into a document already at
// or over the bound: enough for the editor's own derived writes - heading ids, list labels, anchor
// marks - and for ordinary typing and deleting to carry on, and not enough for a paste. A document
// can therefore creep above the bound, and maxPeerByteCreep caps how far. What an update carries
// is not what it renders (renderCheck), so this bounds the content, and the room's apply check
// bounds the rendering the update leaves.
const maxPeerInsertAtTheBound = 8 << 10

// maxPeerByteCreep is how far past the bound a document's rendering may creep: a room whose
// measurement stands past it admits only an update that carries no content at all, and the room's
// apply check refuses any update whose rendering would end past the bound and this creep larger
// than it was (maxPeerRenderingBytes). The first is a test of what the update carries, which an
// update carrying none can still pass while growing the rendering - a delete of a code mark's
// format items carries nothing and leaves every character it held to be escaped - so it is the
// second that keeps a document that has crossed from growing further, while it can still be
// trimmed or split.
const maxPeerByteCreep = 64 << 10

// maxRoomStateBytes is the most encoded state one room may hold: a quarter of the message floor,
// derived from it here rather than written as a figure of its own, because the two are one rule.
// With the frame total gone, this is what keeps a browser's whole-state handoff inside the floor
// A1 asks it to clear: a room at the cap hands over a frame about the size of its state, and
// four such frames fit the floor. Writing the two apart is what let them drift: at 13.5 MB
// against a 54 MiB floor, the branch's own long-edited document at the decode cap, 14,427,285
// bytes, was already past the cap, and four times it was past the floor - so that document could
// take no byte from any browser and could not hand its state back either, which is a person's
// document that stops working. At 16,777,216 it has 2,349,931 bytes to spare
// (TestTheHeaviestStateAtTheDecodeCapFitsTheRoomsStateCapAndTheFloor). A room at the cap is
// charged 16,777,216 x 34 = 570,425,344 bytes of the budget, so three such rooms fit the
// calibration's 2 GiB.
const maxRoomStateBytes = maxMessageBytes / 4

// bytesOfStateASplit is what one split of an item a room already holds adds to the room's
// encoded state, as the state cap charges it (peerApplyAllowance). A split carries no content of
// its own - it leaves the same text, cut in two - so ygo's count of the content an apply adds
// never sees it (crdt.ApplyBudget.Bytes), and what the state gains is the second half's header
// and the length prefix each half now carries (crdt/state_bytes.go, splitMeasure.note).
//
// 24 is twice the heavier of the two shapes measured on 2026-11-16, both through the library's
// own report of what the apply added to the encoded state (crdt.ApplyReport.StateBytes, which
// is exact against an encode): a delete of every other character of a 500,000-character run
// made 499,711 splits and grew the state by 4,723,070 bytes, 9.45 a split, and the 200,000
// mid-run cuts of TestR7SplitsOfADeleteTakeTheRoomPastTheStateCap made 400,000 and grew it by
// 4,575,248, 11.44 a split. Both rooms carry the client ids a browser writes, two and three
// thousand million, so the five bytes of varuint the right half's origin spends on one is in
// those figures; a split whose right half also carries a right origin spends about eight more.
//
// It is a price and not a ceiling. The widest a split can be is about 75 bytes - the info byte,
// an origin and a right origin at a client of up to 1<<55-1 and a clock of up to 1<<53, eight
// varuint bytes each, the extra length prefix, the section counts, and the cut a parked struct
// sharing those clocks can make of either half (overlapSlack) - and a room whose every split
// were that wide could pass the cap by about twice what it has left. Charging that worst case
// would refuse a delete the room has three times the state for: the 500,000-character run
// above adds 4.7 MB to a room with 16.2 MB to spare, and at 75 a split it would be refused.
// The state cap is already a bound of this kind rather than a ceiling - the header of every
// struct an update carries is charged at a price the same way (bytesOfStateAStruct) - and what
// it holds exactly is the room that has nothing left, which takes no cutting at all
// (TestR7SplitsOfADeleteTakeTheRoomPastTheStateCap).
const bytesOfStateASplit = 24

// bytesOfStateAStruct is what one struct an update carries adds to the room's encoded state past
// its content, as the state cap charges it (peerApplyAllowance): its header - the info byte, its
// origins or its parent, and its client's section where it is the client's first - and, for a
// struct carrying no content, the range the state's delete set gains for it. ygo's count of what
// an apply adds sees the content and the map key alone (crdt.ApplyBudget.Bytes: "A deleted item
// is not counted"), so without this a struct carrying no content cost the state cap nothing.
//
// Measured 2026-11-16 through the library's own report of what each apply added to the encoded
// state (crdt.ApplyReport.StateBytes, exact against an encode), 10,000 structs a frame: a GC
// struct each of a client of its own at clock 0 added 15 bytes a struct where the client ids
// take four bytes of varuint and 23 where they stand at 1<<53 and take eight, the heaviest; a
// GC struct of its own client parked at clock 1 added 8, at clock 1<<40 13; a run of GC structs
// of one client 2; a keystroke 16 for its one byte of content; and a margin record of a client
// of its own, parked, 27 in all, its key and map value among them.
//
// 48 is twice the heaviest, as bytesOfStateASplit is set, and, like it, a price and not a
// ceiling: the widest struct is about 76 bytes past its content - an origin and a right origin at
// a client of up to ten bytes of varuint and a clock of up to eight, its client's section header,
// and the delete-set range of a deleted one - and the content an apply carries is charged against
// the same headroom by Bytes, which ygo checks apart from Structs. So what the cap holds exactly
// is the room it leaves nothing, which takes no struct at all. One apply into a room with h bytes
// left can end past the cap by what its content and its structs' headers spend together past h:
// h at most where every struct is within this price, and about 1.6 h where every one is the
// widest; the next apply is refused every struct.
const bytesOfStateAStruct = 48

// maxDecodedValuesPerApply is the most values one apply of a browser's update may decode: each
// struct, each lib0 Any value at any depth, each Any map entry and each JSON member
// (crdt.ApplyBudget.Decoded). 1,200,000 is past the decode cap itself (maxUpdateItems,
// 1,048,576), so an honest whole-state handoff of a document at the cap decodes within it, and
// far under what the heaviest honest apply the pod allows holds: 25,165,779 null values in one
// struct for a client whose every clock the room held allocated 805,307,736 bytes in one apply
// though it inserted nothing, and the decode count is what refuses such a frame as it is read
// rather than once it is all in memory
// (dispatch://INFRA-179/artifact/infra179-simplify-measured-md).
const maxDecodedValuesPerApply = 1_200_000

// peerRule names the rule that set one field of a browser's update's allowance, so a refusal the
// apply's own counters report can say which rule refused it and with which close.
type peerRule uint8

const (
	// ruleDecodedValues: the apply decoded more values than any apply may (maxDecodedValuesPerApply).
	ruleDecodedValues peerRule = iota
	// ruleDecodeCap: the structs it integrated would take the room past maxUpdateItems.
	ruleDecodeCap
	// ruleByteBound: the room's rendering is at or over pmdoc.MaxDocumentBytes, so the update may
	// carry at most maxPeerInsertAtTheBound bytes of content.
	ruleByteBound
	// ruleCreepCap: the rendering has crept maxPeerByteCreep past the bound, so the update may
	// carry no content at all.
	ruleCreepCap
	// ruleStateCap: the bytes it added would take the room's state past maxRoomStateBytes.
	ruleStateCap
	// rulePressure: the open rooms are past their budget and nothing can be closed, so no apply
	// may integrate anything (roomBudget.full).
	rulePressure
)

// refusal is the close a browser is given for an update this rule refused: the document's own
// permanent close for the rules a rebuild cannot clear, the busy close for the pressure, which
// passes by itself, and for the two caps a rebuild can clear - the state cap and the decode cap -
// the rebuild offer where the rebuild route takes the room as it stands (rebuildTaken,
// nearACapUnencoded) and the document's own close where it does not.
//
// The decode cap offers the rebuild where nothing else helps: a room at it refuses every update
// that adds a struct, so the person's next edit is refused too, and the route accepts exactly
// such a document (nearACap reads the struct count first). Closing it DOC_TOO_LARGE told the
// person their document "is larger than a Dispatch document may be" and offered no remedy where
// the one remedy was a click away (dispatch://INFRA-179, review 5820480859's finding 6).
//
// Neither cap offers it where the route would refuse it. Each refuses an update for what that
// update would add, so a room far from both refuses a frame larger than its share as readily as
// a room at them: a frame of 60,000 structs into a room 100,000 short of the decode cap, and a
// paste of 200 KiB into a room 100 KiB short of the state cap, were each closed DOC_NEEDS_REBUILD,
// and the route answered the rebuild the editor then offered 409 DOCUMENT_LIVE (the
// TestR7A...RefusalIsOfferedARebuildTheRouteRefuses rows, dispatch://INFRA-179, review 7). There
// the edit is what is too large and a smaller one lands, which is what DOC_TOO_LARGE tells the
// person.
func (r peerRule) refusal(rebuildTaken bool) *websocket.PeerClose {
	switch r {
	case ruleStateCap, ruleDecodeCap:
		if rebuildTaken {
			return documentNeedsRebuild()
		}
		return documentTooLarge()
	case rulePressure:
		return documentBusy()
	default:
		return documentTooLarge()
	}
}

// why is the rule in plain words, for the one line a refusal leaves (logRefusal).
func (r peerRule) why() string {
	switch r {
	case ruleDecodeCap:
		return "its apply would take the document past the items ygo decodes in one update"
	case ruleByteBound:
		return "its apply inserts into a document already at its byte bound"
	case ruleCreepCap:
		return "its apply inserts into a document that has crept past its byte bound"
	case ruleStateCap:
		return "its apply would take the document's state past what one room may hold"
	case rulePressure:
		return "the open documents are past their memory budget and nothing can be closed"
	default:
		return "its apply decodes more than the pod lets any apply decode"
	}
}

// peerAllowance is what one browser's update applies under: the budget ygo holds its apply to
// (crdt.ApplyBudget) and, for each of the fields a rule can close the browser for, the rule that
// set it and that rule's own figure, so a refusal names the rule rather than the arithmetic.
type peerAllowance struct {
	budget crdt.ApplyBudget
	// structs, bytes and splits name the rule each of budget's bounded fields came from, and
	// structsLimit, bytesLimit and splitsLimit that rule's own figure, which the refusal's
	// line carries. Split has a pair of its own because two rules can set it: the decode cap,
	// whose headroom it divides with the structs, and the state cap, whose bytes a split
	// spends without carrying any content of its own (peerApplyAllowance).
	structs, bytes, splits                peerRule
	structsLimit, bytesLimit, splitsLimit int64
}

// peerApplyAllowance is the allowance an update of update bytes applies under in a room weighed at
// structs items against the decode cap (storedCeiling) and holding stateBytes bytes resident - its
// tracked state and what its pending queue holds (roomResidentBytes) - whose rendering measures
// rendering bytes, its last published rendering (weighPeerUpdate; measured false for a room with
// no rendering at all), while the budget is full or not.
//
// Each field is the least of the rules that apply to it, and each rule's own figure is kept so
// the refusal can name it:
//
//   - Decoded is maxDecodedValuesPerApply always: no origin decodes more.
//   - Structs and Split divide what is left of the decode cap above the ceiling the room is
//     weighed at (storedCeiling: the decode cap rule), each charged what that ceiling counts for
//     it: two items a struct, since a struct may park and the ceiling counts each parked struct
//     twice to bound the queue in memory, and one an item cut in two. A split is a struct the
//     room keeps and the next load must decode, so what bounds what an update carries in bounds
//     what it cuts; but ygo counts the two apart and checks each against its own field
//     (crdt.ApplyBudget.Structs, .Split,
//     sjawhar/ygo v1.55.1-sami.9), so the whole headroom written into both bounded neither. An
//     update inserting k characters into the middle of a run carries k structs and cuts k items:
//     against a room k short of the cap each count stayed within its own field and the room
//     ended k past the cap holding a state that no longer decodes - 1,048,577 structs for k=1
//     and 1,049,576 for k=1,000, both `crdt: invalid update` on the next load
//     (TestR6Finding1StructsAndSplitShareNoHeadroom, dispatch://INFRA-179, review 6's
//     finding 1). Charged at what each adds, the two sums cannot pass the headroom whatever the
//     update's mix of carrying, parking and cutting (capShares).
//   - Bytes is no limit below the byte bound; maxPeerInsertAtTheBound at or over it (the bound);
//     nothing past the creep cap (the creep cap); and never more than what is left of
//     maxRoomStateBytes above what the room holds resident, stateBytes (the state cap).
//   - Split is also what the state cap leaves above stateBytes and this update's own bytes, at
//     bytesOfStateASplit a split, where that is less than the decode cap's share
//     (the state cap rule): a split adds no content, so ygo's Bytes count never sees it, and a
//     room the state cap left no byte took a delete of 200,000 mid-run cuts whose splits added
//     4,575,248 bytes of encoded state (TestR7SplitsOfADeleteTakeTheRoomPastTheStateCap,
//     review 7). The update's own bytes come off that headroom because they bound the content
//     the same apply can add to it, which Bytes may spend to the last byte.
//   - Structs is also what the state cap leaves above stateBytes, at bytesOfStateAStruct a
//     struct, where that is less than the decode cap's share (the state cap rule): ygo's Bytes
//     count sees a struct's content and map key and nothing of its header
//     or of the delete-set range a deleted one adds, so a struct carrying no content - a GC
//     struct, a deleted one - was charged nothing at all, and one frame of 50,000 of them, each of
//     a client of its own, took a room tracked 1,000 bytes short of the cap 749,004 past it, and
//     six frames of 20,000 a room 1.5 MiB short 227,150 past it, every one admitted
//     (TestFramesOfStructsCarryingNoContentStopAtTheStateCap).
//   - With the budget full, Structs and Bytes are nothing and Split is what the pressure leaves
//     this room at most (pressure.splits, splitsUnderPressure), so the one update admitted is
//     one that integrates nothing of its own, adds no content and cuts at most that many of the
//     items the room holds: a delete of whole items, which cuts none and is admitted however
//     long the pressure has run, or a frame of structs the room already holds.
//
// A negative figure is clamped to nothing: a room already past a bound takes no more, and ygo
// reads a field below zero as no limit at all. A room with no rendering to measure is weighed on
// the update's own bytes alone, so the caller passes a rendering of nothing for it
// (roomWeighing.size answers sizeUnmeasured, which is no length).
func peerApplyAllowance(update, rendering, stateBytes int64, structs uint64, measured bool, pressure peerPressure) peerAllowance {
	if !measured {
		rendering = 0
	}
	allowance := peerAllowance{
		budget:       crdt.ApplyBudget{Decoded: maxDecodedValuesPerApply, Bytes: -1},
		structs:      ruleDecodeCap,
		bytes:        ruleStateCap,
		splits:       ruleDecodeCap,
		structsLimit: maxUpdateItems,
		bytesLimit:   maxRoomStateBytes,
		splitsLimit:  maxUpdateItems,
	}
	allowance.budget.Structs, allowance.budget.Split = capShares(structs)
	switch {
	case measured && rendering >= maxPeerRenderingBytes:
		allowance.budget.Bytes, allowance.bytes, allowance.bytesLimit = 0, ruleCreepCap, maxPeerByteCreep
	case rendering+update >= pmdoc.MaxDocumentBytes:
		allowance.budget.Bytes, allowance.bytes, allowance.bytesLimit = maxPeerInsertAtTheBound, ruleByteBound, maxPeerInsertAtTheBound
	}
	if state := clampedBytes(maxRoomStateBytes - stateBytes); allowance.budget.Bytes < 0 || state < allowance.budget.Bytes {
		allowance.budget.Bytes, allowance.bytes, allowance.bytesLimit = state, ruleStateCap, maxRoomStateBytes
	}
	if cuts := clampedBytes(maxRoomStateBytes-stateBytes-update) / bytesOfStateASplit; cuts < allowance.budget.Split {
		allowance.budget.Split, allowance.splits, allowance.splitsLimit = cuts, ruleStateCap, maxRoomStateBytes
	}
	if carried := clampedBytes(maxRoomStateBytes-stateBytes) / bytesOfStateAStruct; carried < allowance.budget.Structs {
		allowance.budget.Structs, allowance.structs, allowance.structsLimit = carried, ruleStateCap, maxRoomStateBytes
	}
	if pressure.full {
		allowance.budget.Structs = 0
		allowance.structs, allowance.structsLimit = rulePressure, 0
		allowance.budget.Bytes, allowance.bytes, allowance.bytesLimit = 0, rulePressure, 0
		if pressure.splits < allowance.budget.Split {
			allowance.budget.Split, allowance.splits, allowance.splitsLimit = pressure.splits, rulePressure, 0
		}
	}
	return allowance
}

// peerPressure is what the room budget's pressure allows one update in one room: full, whether
// the open rooms are past their budget and nothing can be closed (Service.underPressure), and
// splits, how many of the room's items that update may cut while they are, which is what is
// left of the room's growth under this pressure (splitsUnderPressure). splits is read only
// while full stands, so the zero value is a room under no pressure at all.
type peerPressure struct {
	full   bool
	splits int
}

// pressureSplits is how many items a browser's update may cut while the open rooms are past
// their budget and nothing can be closed: eight, which is four delete ranges at their two edges
// each, and far more than an editor sends - a person's delete of a selection is one range and
// cuts at most two items.
//
// It is not nothing, which is what the pressure allows an update to carry. R3 promises that a
// full document can still be cut back down, and a delete that falls inside a word or a paragraph
// cuts the item it lands in: with no split allowed at all, the hundred shrinking updates A4
// drives were refused 4503 DOC_BUSY on a full server and a person could shrink a document only by
// deleting whole items (TestAFullServerRefusesWhatGrowsAndTakesWhatShrinks caught it).
//
// It is also not the decode cap's headroom, which is what an unpressed room allows: the measured
// attack this field exists for is one 2.2 MB frame whose delete set cut a run into 1,100,002
// pieces and grew the room's two copies by 336.6 MB (room_prices.go), and under pressure the pod
// has nothing to spare for it. Eight splits cost the two copies 10,080 bytes at the measured
// per-struct price, which is nothing beside the budget they are weighed against, so this is
// arithmetic rather than a shape anyone measured.
//
// It bounds one update and nothing else, which is why pressureGrowth bounds the room over the
// whole of the pressure beside it.
const pressureSplits = 8

// pressureGrowth is how many structs one room may grow by over the whole of one pressure,
// however many updates it sends: the cumulative bound pressureSplits is not. Eight splits an
// update bound one update alone, so the review drove 5,000 updates of four mid-run deletes each
// into one room with the budget full throughout, every one was admitted, and the room went from
// 10 structs to 40,010 - growth nothing short of the decode cap would have stopped, and a room
// at that cap holds 1,048,576 structs, which at the 185.31 bytes a split-dense room was
// measured to hold a struct (bytesOfHeapAStruct) is 388 MB over the room's two copies, on a pod
// that has no memory to give (TestR6Finding3PressureSplitsHaveNoCumulativeBound,
// dispatch://INFRA-179, review 6's finding 3).
//
// 256 is measured from the two ends it sits between rather than chosen, both read on
// 2026-11-11:
//
//   - What an honest person's editing needs. A person's delete of one character inside a word
//     cuts the item it lands in at both edges and leaves the room 2 structs heavier, so 256 is
//     128 of those cuts in one room under one pressure, measured as the drive that reaches it
//     (TestThePressureBoundsARoomsGrowthAcrossItAndTheEndOfItClearsTheMark admitted 128 and was
//     refused the 129th). The shrinking drive A4 holds this against is a hundred shrinking
//     updates, ten 50-byte cuts into each of ten rooms, each one delete range cutting at most
//     the two items its edges fall in: at most 20 structs of any one room's 256, and all
//     hundred still apply (TestAFullServerRefusesWhatGrowsAndTakesWhatShrinks). A hundred of
//     them driven into one room apply too.
//   - What the attack would take. The review's 5,000 updates meet it after 32 and every one
//     after that is refused, so the room grows by 256 structs where it grew by 40,000: 95 KB
//     over its two copies at the measured 185.31 bytes a split struct, and 322,560 bytes of the
//     budget at the 630 it charges a struct in each copy (roomWeight). Across a budget full of
//     rooms - the thousand the room cap allows (maxLiveRooms), every one of them attacked at
//     once - that is 256,000 structs: 95 MB of heap, 323 MB of the 2 GiB budget, where one room
//     alone could take 388 MB before.
//
// A delete that cuts no item is outside this bound and is admitted whatever the room has grown
// by: a delete range covering whole items splits nothing, so it needs no split allowance at all
// and a person can still shorten a document in a room that has reached its growth
// (peerApplyAllowance, TestAPeerUpdateIsWeighedAgainstTheRoomsLastMeasurement's row for it).
const pressureGrowth = 256

// capHeadroom is what is left of the decode cap above a room weighed at structs items against it
// (storedCeiling), never below nothing: a room already at or past the cap may add no
// struct, and a field below zero would read as no limit at all (crdt.ApplyBudget). It is what
// the apply's structs and splits spend between them (capShares).
func capHeadroom(structs uint64) int {
	if structs >= maxUpdateItems {
		return 0
	}
	return int(maxUpdateItems - structs)
}

// capShares is what one apply may carry and what it may cut out of that headroom: the structs it
// may integrate or park, and the splits it may make of items the room already holds. Each is
// charged what the room's ceiling counts for it (storedCeiling) - two items a struct, since a
// struct may park and the ceiling counts each parked struct twice, and one an item cut in two -
// so 2 x carried + cut is the headroom itself and no mix of carrying, parking and cutting passes
// it.
//
// The headroom is halved between the two, as it has been since review 6's finding 1, and the
// structs' half then buys half as many structs as it did: the splits are what they were and the
// carrying is what the parking made dearer. That is the way round a room near the cap needs -
// a person who cannot type there can still delete, and a delete of whole items, which cuts
// nothing, is admitted whatever is left - and it is what keeps the shapes a room reaches by
// deleting reachable: a delete of every other character of a 500,000-character run makes
// 499,711 splits, which the splits' half admits over a room holding a few hundred structs
// and no division that gave the splits a third would
// (TestASplitDenseRoomIsChargedNoLessThanItsStructsHeap).
//
// A struct is charged two items whether it parks or integrates, because what an update's structs
// will do is not known until the apply does it: ygo counts a struct it parks against the same
// field as one it integrates (crdt.ApplyBudget.Structs). A frame whose structs each wait behind a
// one-clock gap of their own parks every one of them, and a count that charged them once let two
// such frames, each within its allowance, take the room's ceiling past the decode cap
// (dispatch://INFRA-179, review 7); charged two items a struct, the second such frame is refused
// (TestR7ParkedStructsBehindTheirOwnGapsPassTheDecodeCap).
func capShares(structs uint64) (carried, cut int) {
	headroom := capHeadroom(structs)
	cut = headroom / 2
	return (headroom - cut) / 2, cut
}

// clampedBytes is what is left of a byte bound, never below nothing. It is a count of bytes, so
// nothing caps it but the bound it came from: capping it at the decode cap's count of structs,
// which is the smaller number, would refuse a paste of a mebibyte into an empty room in the name
// of a state cap of 13.5 MB.
func clampedBytes(left int64) int {
	if left <= 0 {
		return 0
	}
	return int(left)
}

// peerAllowedRoots is the roots a browser's update may name: the document's fragment and its
// margin, and nothing else, so a subdocument, an unknown root and a write of the retired clients'
// list a rebuild left are all refused by ygo as it decodes them (crdt.WithAllowedRoots), with no
// walk of the update and no apply of any part of it. Only a rebuild writes the retired clients'
// list, and nothing the editor runs writes or deletes an entry of it.
var peerAllowedRoots = []string{fragmentName, marksMapName}

// peerRefusedContent is the kinds of content a browser's update may not carry: an embed and raw
// binary. The Proof editor writes neither - pmdoc reads a text embed back as a document outside
// the schema and has no node for binary content at all (pmdoc/update.go, pmdoc.ErrSchema) - and
// no count can keep one out, since one embed is one struct of a few bytes, inside every
// allowance the hook can build. What it costs is every reader of the document: a document
// holding one is shown to each of them as content their editor cannot render, which is the
// repair state, until a person repairs it. ygo refuses the whole apply where it reads one and
// rolls it back, so the room is left as it was and the browser is closed DOC_UPDATE_REFUSED
// (crdt.WithRefusedContent, refusedByApply).
var peerRefusedContent = []crdt.ContentKind{crdt.ContentKindEmbed, crdt.ContentKindBinary}

// roomApplies is the allowance of each of the hook's applies in flight in one room, by the origin
// ygo applies it with (provider/websocket/peer_update.go:84-98), and the room's answer to ygo's
// three per-apply questions: what this apply may decode, integrate, cut and add
// (crdt.WithApplyBudget), which roots it may name (crdt.WithAllowedRoots) and which kinds of
// content it may not carry (crdt.WithRefusedContent). ygo asks all three under the document's
// lock as each remote apply begins, so none may call the document; they take mu and, for the
// budget, the count of the room's commits (began, loggedCommits.now, which takes its own lock),
// and neither lock is held across a call into the document.
//
// It is also where the room keeps what the library answers back: as each apply commits, ygo
// reports what the document then holds and the bytes the apply added to its encoded state, and
// those figures are the room's from then on - its charge, the copies it is weighed for and the
// next update's allowance all read them rather than the document (crdt.WithApplyReport,
// structs, structsHeld, state).
//
// An apply of any other origin is the server's own - an API write's publish, an injected update,
// settlement's repair - and is bounded by nothing here, allowed every root and refused no kind of
// content: each of those weighs what it leaves by its own rules (growth.go), a repair that could
// not write the margin would leave the document broken, and a room whose stored history already
// holds an embed must still load, so that a rebuild can take it out.
type roomApplies struct {
	mu sync.Mutex
	// inFlight is one record for each of the hook's applies running in this room, by the origin
	// ygo applies it with: what the apply may do, and what its own report said it did
	// (peerApply, applying).
	inFlight map[any]*peerApply
	// queuedContent is the content bytes the room holds in its pending queue, as the applies that
	// left the queue holding something were charged (noteQueued, queuedBytes): resident memory the
	// room's encoded state leaves out (leaveParkedOut), which the state cap's allowance and the
	// room budget weigh beside the tracked state (residentBytes). It is written by the hook after
	// its apply and read by the hook before one and by the budget's drains, so it is an atomic.
	queuedContent atomic.Int64
	// parked is each browser connection whose frames left something parked that has not landed
	// yet, which the room resyncs once the commit that lands it is stored (parked_resync.go).
	parked parkedWaits
	// pressureMark is the room's struct total as the budget's pressure first weighed one of
	// its updates, and pressureMarked whether the room carries such a mark: what bounds the
	// room's growth across the whole of one pressure rather than within one update
	// (splitsUnderPressure). The end of the pressure clears both, so a room that has waited one
	// out starts the next afresh.
	pressureMark   uint64
	pressureMarked bool
	// structs is what the last apply that committed in this room reported the document then
	// held, those parked in its pending queue included (crdt.ApplyReport.Structs), and
	// reported whether one has reported at all. They are the room's own record of the
	// library's figure: every reader that weighs the room takes it from here rather than
	// reading the document again (structsHeld), and the room's load and a compaction, which
	// have no report to read, hold their own reading against it (anchorStructs).
	//
	// Each is written by the report callback alone, under the document's lock as the apply
	// commits, and read without one by the budget's drains, the copy heap and the hook on the
	// peer's read loop, so they are atomics rather than fields of mu: the figure is written
	// before the flag, and every value either carries is a total the document stood at.
	structs  atomic.Int64
	reported atomic.Bool
	// state is the bytes of encoded state the room is tracked at (stateBytes): what the room's
	// load or a re-base read (loaded, rebase), plus what every commit since added. A remote
	// apply's is its own report (crdt.ApplyReport.StateBytes), added as it commits
	// (noteApply); a commit no report covers - the server's own writes and ygo's format
	// cleanup - is added by the drain that hands it over, from the replica's report of the
	// same update (renderedReplica.countCommit). The hook bounds what each browser's apply may
	// add by what is left of maxRoomStateBytes above it, and the budget charges the room by it,
	// neither encoding the document. It is written under the document's lock and outside it,
	// so it is an atomic. commits counts the commits it has taken - every report, and every
	// commit a drain counted that no report covered - so a reading of the room can tell whether
	// the room committed anything while it was taken (reread, rebaseMark).
	//
	// owed says the figure may stand below the room's encoded state: a drain handed over a
	// commit that neither a report nor the replica measured (owe). The next re-read or re-base
	// whose reading is exact clears it (take, reread).
	state   atomic.Int64
	commits atomic.Uint64
	owed    atomic.Bool
	// unpaired is the reports no drained commit has been paired with yet, in commit order, each
	// with its apply's origin and the rendering the room's apply check made of the state it
	// left, guarded by mu; paired is whether the room keeps them, which a room whose commits a
	// replica drains does (newRoomApplies). ygo logs a commit and then reports it under the same
	// hold of the document's lock, so a drain that takes the reports made before it takes the log
	// finds the report of every remote apply it hands over (reportsMade, reportPairing).
	unpaired []madeReport
	paired   bool
	// checked is the room's rendering as the last browser's apply the check passed left it, at
	// the commit position that apply's commit stood at (noteApply): what the next browser's
	// apply is weighed against and spliced into while no other commit has landed and the
	// replica has not yet taken this one (renderCheck.base). It is written under the document's
	// lock and dropped by the replica once it stands at or past it (forgetChecked), so it is an
	// atomic.
	checked atomic.Pointer[roomRendered]
}

// madeReport is one apply's report as the room keeps it for the drain that hands its commit over
// (reportPairing): the apply's origin, and checked, the rendering the room's apply check made of
// the state the apply's commit stood at, with that commit's position, nil for an apply the check
// did not render - the server's own, or one that changed nothing the check reads - and for one
// whose position could not be counted.
type madeReport struct {
	origin  any
	checked *roomRendered
}

// checkedAt is the rendering report carries for the commit at position seq, and nil where it
// carries none or carries one of another commit. The position is what makes the hand-over exact:
// the pairing matches a commit with the first unpaired report of its origin, and a report made by
// an apply that logged nothing - one the room already held - is such a report too, so the
// report a commit is paired with can be another commit's (reportPairing.covers). Counting the
// commit that way costs nothing, since every report of one origin counts alike; taking another
// commit's rendering would make the replica's rendering a state it does not hold.
func (report madeReport) checkedAt(seq uint64) *replicaRendering {
	if report.checked == nil || report.checked.at != seq {
		return nil
	}
	return report.checked.rendering
}

// peerApply is one of the hook's applies while it runs: the allowance ygo holds it to, and
// content, the content bytes its own report said it added, parked or integrated
// (crdt.ApplyReport.Bytes, crdt.WithApplyBudget). The hook reads content once the apply has
// returned, to charge what the apply left in the room's pending queue (roomApplies.noteQueued).
//
// It is also what the room's apply check keeps of the apply (renderCheck): startAt, how many
// commits the room had made as the apply began, and counted, whether that was known
// (loggedCommits.now), both read as ygo asks for the apply's budget; and checked and rendering,
// whether a check of what the apply left has passed and the rendering that check made of it, nil
// for a state that renders nothing. The apply report hands that rendering on with the commit
// (noteApply). All four are read and written under the document's lock, which ygo holds for the
// budget, the check and the report alike, and under mu besides.
type peerApply struct {
	budget    crdt.ApplyBudget
	content   int64
	startAt   uint64
	counted   bool
	checked   bool
	rendering *replicaRendering
}

// newRoomApplies sets live, a room's document, to apply every browser's update under the
// allowance the hook reserved for it, to refuse one naming any root but the fragment and the
// margin, to refuse one carrying a kind of content the editor never sends, and to report what
// each apply that commits leaves it holding, as newRoomScans sets its scan budget. From here
// live applies every remote update whole or not at all. paired is whether a replica drains the
// room's commits from its update log, and so pairs each one with its report (unpaired). logged
// is the count of the room's commits (loggedCommits), which the budget and the report read to
// place each of the hook's applies at its commit position for the room's apply check.
//
// logged is held by the callbacks live holds and by nothing the record keeps: the record is
// listed strongly for the hook (roomWeighing), and the count reaches live through its update log,
// so a record holding it would keep every evicted room's document resident.
//
// The report is what makes the library and not this package the authority on what a room holds
// (crdt.WithApplyReport, dispatch://INFRA-179): ygo reads the two counters that answer it - the
// store's structs and the pending queue's - as each apply commits, under the lock that apply
// held, and hands them over, where this package used to walk the client slices again at each
// drain, each copy and each update weighed. It costs the room less than the readings it
// replaces: one reading an apply, against one a drain plus one an update weighed plus one a
// copy taken (the fork's measurement is 13.4 µs for the pair of counters,
// crdt/doc.go:446-448).
func newRoomApplies(live *crdt.Doc, paired bool, logged *loggedCommits) *roomApplies {
	applies := &roomApplies{paired: paired}
	live.SetApplyBudget(func(origin any) crdt.ApplyBudget {
		applies.began(origin, logged)
		return applies.budget(origin)
	})
	live.SetAllowedRoots(applies.roots)
	live.SetRefusedContent(applies.refusedContent)
	live.SetApplyReport(func(origin any, report crdt.ApplyReport) { applies.noteApply(origin, report, logged) })
	return applies
}

// noteApply keeps what an apply that has just committed in this room reported: the structs the
// document now holds, the bytes the apply added to its encoded state, and the apply's origin,
// which the drain that hands the commit over pairs it with (reportPairing). ygo calls it under
// the document's lock and inside the transaction that applied the update, so it takes nothing
// but mu and the count of the room's commits (loggedCommits.now), and calls nothing back into the
// document (crdt.WithApplyReport).
//
// A refused or rolled-back apply reports nothing, so what is kept here is always a total the
// document stood at. An apply of a local transaction reports nothing either - the report is of
// remote applies - so a server write made in the room's own transaction leaves the struct
// figure behind the document until the next remote apply reports again or the anchor reads the
// document itself (anchorStructs, which logs the difference), and leaves the byte figure to the
// drain (renderedReplica.countCommit).
//
// The bytes are the library's count of the direct form of the encoded state: each struct and
// skip where the store holds it. The pending queue is no part of that state (leaveParkedOut), so
// a park adds nothing to it, the drain that lands a parked struct adds that struct's whole
// length, and a drain discarded takes nothing off, since nothing it held had been added; an
// encode of the room merges nothing of the queue in and reads the same direct form, so a
// reading of the room is exact against the total whatever it holds queued (reread). What the
// queue holds in memory is weighed beside it (noteQueued).
//
// The content bytes go to the apply's own record where the apply is one of the hook's, so the
// hook can charge what its apply left in the queue to the room once it returns (peerApply,
// noteQueued). An apply of any other origin has no record and nothing is kept for it.
//
// So does the rendering the room's apply check made of what a hook's apply left: the report is
// made once the apply has passed its last check and its commit is logged, so that rendering is
// the commit's, and it becomes the room's record of its rendering at the commit's position
// (checked, noteChecked) and rides the report to the drain that hands the commit over
// (madeReport, takeChecked).
func (a *roomApplies) noteApply(origin any, report crdt.ApplyReport, logged *loggedCommits) {
	a.structs.Store(int64(report.Structs))
	a.reported.Store(true)
	a.state.Add(int64(report.StateBytes))
	a.commits.Add(1)
	at, counted := logged.now()
	a.mu.Lock()
	made := madeReport{origin: origin}
	if running, mine := a.inFlight[origin]; mine {
		running.content += int64(report.Bytes)
		made.checked = a.noteChecked(running, at, counted)
	}
	if a.paired {
		a.unpaired = append(a.unpaired, made)
	}
	a.mu.Unlock()
}

// noteChecked keeps, as the room's record of its rendering (checked), the rendering running's
// commit left the room at: the one the room's apply check made of what the apply left, where a
// check passed it, at at, the room's commit position once the commit is logged. An apply no check
// ran for changed nothing the check reads - ygo checks every apply that changes the document -
// so a record standing at the position the apply began at is carried to at unchanged. A position
// that cannot be counted now (counted false) places nothing, and the record keeps the position
// it had. It answers what the commit's report carries (madeReport): the record it made, nil where
// no check rendered the apply or none could be placed. The caller holds the document's lock and
// mu.
func (a *roomApplies) noteChecked(running *peerApply, at uint64, counted bool) *roomRendered {
	held := a.checked.Load()
	switch {
	case !counted:
		return nil
	case !running.checked:
		if held != nil && running.counted && held.at == running.startAt {
			a.checked.Store(&roomRendered{at: at, bytes: held.bytes, rendering: held.rendering})
		}
		return nil
	}
	made := &roomRendered{at: at, bytes: renderedSize(running.rendering), rendering: running.rendering}
	a.checked.Store(made)
	if running.rendering == nil {
		return nil
	}
	return made
}

// forgetChecked drops the room's record of its rendering (checked) where it stands before seq, a
// commit position whose rendering the replica has published (publishRendered): a commit past the
// record has landed, and the replica's rendering says what the room renders now. A record standing
// at seq itself stays: it is of the same state as the replica's and shares its markdown
// (takeChecked keeps the check's string), and it carries the index of the room's own blocks the
// next apply's check splices by (blockIndex), which the replica's rendering, of its own document's
// blocks, cannot.
func (a *roomApplies) forgetChecked(seq uint64) {
	if a == nil {
		return
	}
	if held := a.checked.Load(); held != nil && held.at < seq {
		a.checked.CompareAndSwap(held, nil)
	}
}

// stateBytes is the bytes of encoded state the room is tracked at (state), and zero for a
// document no room's load listed.
func (a *roomApplies) stateBytes() int64 {
	if a == nil {
		return 0
	}
	return a.state.Load()
}

// loaded sets the room's tracked state to stateBytes, what the stored state its load decoded
// came to (chargeLoadedRoom). A load that decoded rows an earlier build stored of what parked
// holds them in its pending queue, out of its encoded state (leaveParkedOut), so the figure then
// stands above an encode of the room by those rows: the safe side, and the bytes the queue holds
// of them in memory, which nothing else counts (noteQueued).
func (a *roomApplies) loaded(stateBytes int64) {
	if a != nil {
		a.state.Store(stateBytes)
	}
}

// added raises the room's tracked state by bytes, the figure of a commit no report covered
// (renderedReplica.countCommit), and counts the commit.
func (a *roomApplies) added(bytes int64) {
	if a != nil {
		a.state.Add(bytes)
		a.commits.Add(1)
	}
}

// owe counts a commit nothing measured, which the drain that handed it over charges nothing
// for, and marks the room's tracked state as possibly below its encoded state (owed), which
// the drain re-reads the room for in its place (reread).
func (a *roomApplies) owe() {
	if a != nil {
		a.commits.Add(1)
		a.owed.Store(true)
	}
}

// take sets the room's tracked state to read, a reading of the room's whole encoded state,
// where it still stands at tracked, and reports whether it did. An exact reading - an encode of
// the room itself, or a store that holds everything the room does (rebaseMark.covers) - replaces
// the figure and settles what was owed; any other reading is a lower bound, so the figure keeps
// the larger of the two and stays owed if it was. Taking a lower bound as the truth would put
// the figure below the state, and the room would then be admitted past maxRoomStateBytes by the
// difference.
func (a *roomApplies) take(tracked, read int64, exact bool) bool {
	want := read
	if !exact {
		want = max(tracked, read)
	}
	if !a.state.CompareAndSwap(tracked, want) {
		return false
	}
	if exact {
		a.owed.Store(false)
	}
	return true
}

// rebase takes read, what a compaction that folded the room's whole stored history wrote, as
// the room's tracked state (take), exact where exact says the reading covers everything the
// room holds, and returns the figure it replaced. A report or a drain that moves the figure
// while it is weighed makes it weigh it again.
func (a *roomApplies) rebase(read int64, exact func() bool) (tracked int64) {
	for {
		tracked = a.state.Load()
		if a.take(tracked, read, exact()) {
			return tracked
		}
	}
}

// rereadTries is how many encodes of a room one re-read takes before it leaves the figure owed
// to the next: each is thrown away when a report lands while it is taken.
const rereadTries = 3

// reread re-bases a room whose tracked state is owed (owe) on an encode of live itself, which is
// exact whatever the room holds queued (noteApply), and reports whether it did. The caller holds
// the replica's lock, so no drain adds to the figure meanwhile, and the commit count is read on
// both sides of the encode, so a reading is kept only where no report landed while it was taken:
// it then holds every report the figure holds and no other. A commit after the encode is added to
// the figure as it lands, and one the encode holds whose drain is still to come is added again by
// that drain, which is the safe side. The encode costs what a read of the room costs (53.0 ms on
// 494,309 structs), and the first drain to find the room owed pays it, a keystroke's as well:
// once for each time a room is owed one, which only a recover or an unmeasured commit makes it.
func (a *roomApplies) reread(live *crdt.Doc) bool {
	if a == nil || live == nil || !a.owed.Load() {
		return false
	}
	for range rereadTries {
		seen := a.commits.Load()
		tracked := a.state.Load()
		read := int64(len(crdt.EncodeStateAsUpdateV1(live, nil)))
		if a.commits.Load() != seen {
			continue
		}
		if a.take(tracked, read, true) {
			if read != tracked {
				slog.Info("dispatch: a room's tracked state was re-read from the room itself",
					"tracked", tracked, "read", read, "difference", tracked-read)
			}
			return true
		}
	}
	return false
}

// reportsMade is the reports made in the room that no drained commit has been paired with yet, in
// commit order: what a drain about to take the room's log pairs its commits against
// (reportPairing). The slice is capped at its length, so the reports made after it are
// appended past what it holds, and only the drain that holds it drops what it holds
// (settleReports). Nil for a room that keeps no reports.
func (a *roomApplies) reportsMade() []madeReport {
	if a == nil || !a.paired {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.unpaired[:len(a.unpaired):len(a.unpaired)]
}

// settleReports drops the first n of the room's unpaired reports, the ones a drain took before
// it took the log (reportsMade). Each was either paired with a commit the drain handed over or
// made by an apply that logged nothing - one that changed the room in no way - so none is
// wanted by a later drain.
func (a *roomApplies) settleReports(n int) {
	if a == nil || n == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	kept := copy(a.unpaired, a.unpaired[n:])
	clear(a.unpaired[kept:])
	a.unpaired = a.unpaired[:kept]
}

// reportPairing walks one drain's commits, in commit order, against reports, the reports made
// before the drain took the log (roomApplies.reportsMade), and next, the first of them no commit
// has been paired with.
type reportPairing struct {
	reports []madeReport
	next    int
}

// covers answers the report that covers the commit the drain has reached, which origin wrote,
// and pairs the two, or false for a commit no report covers. ygo logs a commit and reports it under
// one hold of the document's lock, so a remote apply's report is among those the drain took and
// after every report of a commit before it: the first one from next with the commit's origin is
// its own, or one an apply with that origin made without logging anything, which leaves the count
// the same (and is why the rendering a report carries names its commit's position,
// madeReport.checkedAt).
//
// A commit no report covers is a local transaction - a server write, or ygo's format cleanup,
// whose origin is nil - since no origin a local transaction is made with is one a remote apply
// is made with: a browser's apply has its peer, a publish its liveWriteOrigin, a catch-up its
// remoteUpdateOrigin, and the server's own writes take an origin of their own each. A remote
// apply whose report is made after a concurrent drain has already taken its commit is taken
// for one as well, and counted again from the replica on top of its report, which is the safe
// side (renderedReplica.countCommit). Every origin here went through the apply budget's map
// (roomApplies.budget), so each compares.
func (p *reportPairing) covers(origin any) (madeReport, bool) {
	for at := p.next; at < len(p.reports); at++ {
		if p.reports[at].origin == origin {
			p.next = at + 1
			return p.reports[at], true
		}
	}
	return madeReport{}, false
}

// reportedStructs is the structs the last apply that committed in the room reported it holding,
// and whether any apply has reported: nothing for a document no room's load listed, and nothing
// for a room between its load and its first remote apply.
func (a *roomApplies) reportedStructs() (uint64, bool) {
	if a == nil || !a.reported.Load() {
		return 0, false
	}
	return uint64(max(a.structs.Load(), 0)), true
}

// structsHeld is how many structs the room holding live holds now: the figure its last
// committed apply reported (reportedStructs), and a reading of the document itself where no
// apply has reported one yet - a room's first update after a load, and a document no room's
// load listed, a test's (heldStructs). It is what every reader that weighs a room takes: the
// room's charge as each drain raises it (renderedReplica.chargeAdded), what a whole copy of the
// room costs the pod's load concurrency (Service.copyOfRoom) and the allowance of each
// browser's update (Service.weighPeerUpdate).
func (a *roomApplies) structsHeld(live *crdt.Doc) uint64 {
	if structs, reported := a.reportedStructs(); reported {
		return structs
	}
	return heldStructs(live)
}

// budget is ygo's per-apply allowance: the one reserved for the hook's apply with this origin,
// and no limit for every other origin. Every field is named: a field left out is zero, which ygo
// reads as "this apply may do none of that", so an unset Split would refuse every server write
// that deletes inside a run (crdt.ApplyBudget).
func (a *roomApplies) budget(origin any) crdt.ApplyBudget {
	a.mu.Lock()
	defer a.mu.Unlock()
	if running, reserved := a.inFlight[origin]; reserved {
		return running.budget
	}
	return crdt.ApplyBudget{Decoded: -1, Structs: -1, Bytes: -1, Split: -1}
}

// began places the hook's apply with origin at the commit position the room stands at as it
// begins (peerApply.startAt), which the room's apply check weighs the apply against
// (renderCheck.base). ygo asks for the apply's budget under the document's lock as the apply
// begins, before the apply changes anything, so the room's document asks this there, beside the
// budget (newRoomApplies). An apply of any other origin is placed nowhere.
func (a *roomApplies) began(origin any, logged *loggedCommits) {
	at, counted := logged.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if running, reserved := a.inFlight[origin]; reserved {
		running.startAt, running.counted = at, counted
	}
}

// roots is ygo's per-apply root list: the browser's two for the hook's applies, and nil - every
// root - for every other origin.
func (a *roomApplies) roots(origin any) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, reserved := a.inFlight[origin]; reserved {
		return peerAllowedRoots
	}
	return nil
}

// refusedContent is ygo's per-apply list of content kinds the update may not carry: the browser's
// two for the hook's applies (peerRefusedContent), and nil - every kind taken - for every other
// origin.
func (a *roomApplies) refusedContent(origin any) []crdt.ContentKind {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, reserved := a.inFlight[origin]; reserved {
		return peerRefusedContent
	}
	return nil
}

// applying runs apply, one of the hook's own applies with origin, under budget: reserved before
// the apply and dropped as it returns, so ygo's three questions are answered from it for exactly
// this apply and every other origin's apply is unbounded. Nil, a room no load listed, runs the
// apply under whatever the document itself was built with.
//
// content is what the apply's own report counted of the content it added, parked or integrated
// (noteApply), and nothing for an apply that reported nothing - one refused, or one in a room
// no load listed.
func (a *roomApplies) applying(origin any, budget crdt.ApplyBudget, apply func() error) (content int64, err error) {
	if a == nil {
		return 0, apply()
	}
	running := &peerApply{budget: budget}
	a.mu.Lock()
	if a.inFlight == nil {
		a.inFlight = make(map[any]*peerApply)
	}
	a.inFlight[origin] = running
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.inFlight, origin)
		content = running.content
		a.mu.Unlock()
	}()
	return 0, apply()
}

// checking is the record of the hook's apply with origin that the room's apply check is judging,
// nil for an apply of any other origin, which the check passes (renderCheck.check). The caller
// holds the document's lock.
func (a *roomApplies) checking(origin any) *peerApply {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inFlight[origin]
}

// passed records that a check of what running's apply left has passed, with rendering, that
// check's rendering of it, nil for a state that renders nothing: the later of an update's check
// and its drain's is the one that stands, since a drain the check refuses is taken back and leaves
// what the update's own check rendered (noteChecked). The caller holds the document's lock.
func (a *roomApplies) passed(running *peerApply, rendering *replicaRendering) {
	a.mu.Lock()
	defer a.mu.Unlock()
	running.checked, running.rendering = true, rendering
}

// noteQueued charges what an apply of the hook's left in the room's pending queue to the bytes
// that queue holds in memory (queuedContent, queuedBytes). after is what the queue holds once the
// apply has returned, and content what the apply's report counted of the content it added.
//
// The queue is resident in the room and is no part of its encoded state (leaveParkedOut), so the
// library's count of that state, which the room's tracked state follows, counts nothing an apply
// parks (crdt.ApplyReport.StateBytes) and reports no figure of the queue's bytes. This is that
// figure, which the state cap's allowance and the room budget weigh beside the tracked state
// (roomApplies.residentBytes): without it a browser could park frame after frame each as large as
// the state cap leaves one apply, and the room would hold them all while its tracked state stood
// still (TestARunOfParkedFramesIsRefusedAtTheStateCapHoweverManyArrive).
//
// An apply that left the queue holding anything may have parked something of its own, and is
// charged the whole of its content for it. ygo says what a queue holds and not which apply put
// it there (crdt.Doc.PendingStats), and counts what an apply parks and what it integrates in one
// figure (crdt.ApplyReport.Bytes), so no reading tells a park from an integrate, and no count
// tells a swap from an apply that left the queue alone: an update that releases one parked
// struct and parks 900 KiB of its own leaves as many parked as it found. Charging every apply
// that leaves the queue holding anything is the safe side of the bound, and what it costs is an
// over-count in a room whose queue stays full while its browsers type, since their keystrokes are
// charged as queued though they integrate. An apply that left the queue empty drained or
// discarded all of it, and what landed is in the tracked state from the report of the apply that
// landed it, so the charge goes back to nothing. The figure otherwise stands above what the room
// still holds queued until the queue empties.
//
// A room loaded holding something queued - rows an earlier build stored of what parked, which
// the load decodes into the queue again - starts at nothing here, since its tracked state starts
// at the bytes of the stored state the load decoded, those rows included (chargeLoadedRoom).
//
// It answers the figure it leaves.
func (a *roomApplies) noteQueued(after crdt.PendingStats, content int64) int64 {
	switch {
	case a == nil:
		return 0
	case after.Items == 0:
		a.queuedContent.Store(0)
	case content > 0:
		a.queuedContent.Add(content)
	}
	return a.queuedContent.Load()
}

// queuedBytes is the content the room holds in its pending queue (noteQueued), and nothing for a
// room no load listed.
func (a *roomApplies) queuedBytes() int64 {
	if a == nil {
		return 0
	}
	return a.queuedContent.Load()
}

// residentBytes is what the room holds of encoded state and pending queue together: its tracked
// state (stateBytes) and the content its queue holds (queuedBytes), the figure the state cap's
// allowance and the room budget weigh, and nothing for a room no load listed.
func (a *roomApplies) residentBytes() int64 {
	return a.stateBytes() + a.queuedBytes()
}

// splitsUnderPressure is how many of the room's items the update now being weighed may cut: what
// is left of the room's own growth under this pressure, pressureSplits at most, while the open
// rooms are past their budget (full, Service.underPressure), and pressureSplits while they are
// not, where the splits' share of the decode cap's headroom and what the state cap leaves bound
// the apply instead (peerApplyAllowance).
//
// structs is the ceiling the room is weighed at as this update arrives (storedCeiling). The
// first update weighed under a pressure marks it: that update is already refused its structs
// and its bytes by the pressure, so the mark is the room as the pressure first refused it
// something, and what the room may reach is that mark and pressureGrowth above it. Each later
// update is allowed what is left between the room's own figure and that reach, so the splits an
// update may make fall to nothing as the room nears it and no mix of updates passes it, where
// eight an update bounded one update
// alone (TestR6Finding3PressureSplitsHaveNoCumulativeBound).
//
// The end of the pressure clears the mark, so a room that has waited one out starts the next
// afresh and a person who could not cut inside a word can again. Nothing else clears it: a room
// whose structs fall under its mark - a rebuild's, which writes a history without the
// tombstones - is read against the mark as it stands and is allowed the whole of pressureSplits.
//
// A room no load listed carries no record to mark and is allowed pressureSplits by each update,
// as it is bounded by nothing else here either (roomWeighing.applies).
func (a *roomApplies) splitsUnderPressure(structs uint64, full bool) int {
	if a == nil {
		return pressureSplits
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !full {
		a.pressureMark, a.pressureMarked = 0, false
		return pressureSplits
	}
	if !a.pressureMarked {
		a.pressureMark, a.pressureMarked = structs, true
	}
	if structs <= a.pressureMark {
		return pressureSplits
	}
	if grown := structs - a.pressureMark; grown < pressureGrowth {
		return min(pressureSplits, pressureGrowth-int(grown))
	}
	return 0
}

// weighPeerUpdate is ygo's Server.OnPeerUpdate: the room hands it every update and whole-state
// message a read-write browser sends, with the apply that would put it in the room, and a
// read-only browser never reaches it (sjawhar/ygo v1.55.1-sami.8,
// provider/websocket/peer_update.go:84-154, provider/websocket/peer.go:265-272).
// An update it does not apply is neither stored nor relayed to the room's other browsers, and the
// *websocket.PeerClose it refuses with closes that one browser with that code and reason.
//
// It refuses two kinds of update before the apply, and every other refusal is the apply's. The
// first is an update carrying a client group of the history a rebuild replaced - a group of a
// client the rebuilt history lists as retired, starting below the clock it had reached
// (retiredMapName), read from the update's own header (crdt.ParseUpdateMeta, which decodes no
// content) (refusedBeforeApply). It comes
// from a browser that held the document before the rebuild and still holds that history:
// applied, its whole-state message would put the old document beside the rebuilt one, every
// block twice, and carry the room back toward the cap the rebuild trimmed. It is refused with
// DocumentTooLargeCloseCode and DocumentRebuiltCloseReason, a reason of its own rather than the
// one a used-up scan total is refused with: the document was just rebuilt, so no rebuild of it
// would help, and the editor tells the person it was rebuilt, builds its document afresh from
// the rebuilt room and drops its saved rows typed against the old history as rows of a rebuilt
// document (connection.ts refusedEditNotice, pending-sync.ts addsParking). An update whose
// header does not read refuses nothing there and falls through to the apply, whose decoder
// refuses the same bytes, DocumentUpdateRefusedCloseReason. The second is an update carrying
// structs under a client id that is not its sender's, refused DocumentClientIdRefusedCloseReason,
// or one whose client ids the table could not be read for, refused for a retry with the transient
// DocumentBusyCloseCode (judgeClients, client_ids.go).
//
// Everything else is the apply's. The hook builds the allowance this update applies under
// (peerApplyAllowance) and the roots it may name (peerAllowedRoots), hands both to the room's
// document for this one apply (roomApplies.applying) and runs it. ygo counts what the apply
// decodes, integrates and adds as it goes and refuses the whole apply where any count passes its
// field of the allowance, leaving the document exactly as it was, firing no observer and
// reporting what it reached (crdt.RefusedUpdateError); the dimension at or past its limit names
// the rule that set that field, and the rule names the close (peerRule.refusal). So the walk that
// read every browser's update before it applied - a second decode of the same bytes on the peer's
// read loop - is gone, and with it the three running totals the room kept to weigh that walk
// against. Once the apply has integrated what it may, the room's apply check renders what it left
// and refuses it the same way where that rendering would pass the bound and its creep and grow
// (renderCheck, crdt.ErrApplyCheck); a drain the check refuses, or whose scans or splits pass the
// allowance, is discarded whole while the update stands, the hook answers the update as the
// success it is, and the browsers that parked what was discarded are closed
// (closeDiscardedSenders).
//
// Every refusal before or inside the apply closes the browser. Under sjawhar/ygo v1.51.3-sami.7
// a hook's refusal answered the browser SyncStatus(0) and left it connected; upstream
// reearth/ygo#304 was merged in the form that closes it, with 4205 "Reset Connection" for a hook
// error that is no *websocket.PeerClose. 4205 is what @hocuspocus/provider 2.x reconnects at once
// after, and the client would send the refused frame again, a loop of full syncs; 4413 is the
// permanent band, which the editor drops the refused edit for, so every refusal of a rule the
// browser cannot pass carries it.
//
// Every apply's scans are held to an allowance besides (roomScans): the per-frame ceiling for an
// update that holds a load slot or what is left of the room's scan total, whichever is less,
// reserved before the apply and settled from what its scans passed once it returns, and
// usedUpScanBudget once the room's total is used up. An update whose merge would scan past it is
// refused DocumentTooTangledCloseReason, or DocumentNeedsRebuildCloseReason on a used-up room
// total in a room the rebuild route takes (rebuildTakes).
//
// An update over unslottedApplyBytes takes what its apply costs out of the pod's load
// concurrency before it applies (loadSlots, copyWeight), so a reconnect storm's whole-state
// handoffs decode as many at a time as the pool holds rather than all at once; typing never
// waits on it.
//
// A room with no measurement at all is a document that left the Proof schema while it was live,
// which the socket path refuses to open in the first place (refuseOutsideSchema), or one no load
// listed: the byte bound weighs its updates on their own bytes, as if the room rendered nothing,
// and the creep cap, which needs a rendering to measure, never applies to it. In a room its load
// listed, the apply check renders the document itself, so where the update leaves a document that
// renders, the check weighs it by that rendering all the same (renderCheck).
//
// An apply that fails leaves no trace but its scans. The room's bounded document takes every
// remote update it refuses back out whole, whatever the error, and fires no observer for it
// (crdt/apply_rollback.go:9-21), so the hook has nothing to give back: what its scans passed
// before the refusal it reports (crdt.RefusedUpdateError), and the room's scan total keeps it
// (roomScans.settle), since the work was done. Any other error the apply returns - one that would
// park structs or delete ranges past the room's pending cap (MaxPendingItems, which the room
// holds from the end of its load) - is returned as it is, the one error the hook answers that is
// no *websocket.PeerClose, so ygo answers that browser SyncStatus(0) and leaves it connected,
// exactly as a room without this hook does: the apply ran, which is what keeps a refused browser
// connected where a refusal before the apply closes it
// (provider/websocket/peer_update.go:139-154).
//
// A successful apply's report has already added what it added to the room's encoded state to the
// room's tracked state as it committed (roomApplies.noteApply), and the room's update observer,
// draining the update inside the apply that committed it, weighs the room again from that figure
// (renderedReplica.chargeAdded, roomCharge). What the hook keeps from it is what the apply left in
// the room's pending queue, which the encoded state leaves out and the room holds in memory all
// the same (roomApplies.noteQueued); where that moves, the hook weighs the room again itself.
func (s *Service) weighPeerUpdate(info websocket.PeerUpdateInfo, apply func() error) error {
	weighing := s.roomWeighingOf(info.Doc)
	applies := weighing.applies()
	if refusal := s.refusedBeforeApply(info, weighing); refusal != nil {
		return refusal
	}
	// What the room holds, read once: what is left of the decode cap above it is the apply's
	// allowance of structs and splits.
	//
	// Its structs are the figure the room's last committed apply reported
	// (roomApplies.structsHeld, crdt.ApplyReport.Structs), and a reading of the document only
	// for a room's first update after a load, which no apply has reported on yet. The report
	// counts the pending queue beside the store, which is what this hook has to weigh: a frame
	// whose structs park behind a clock gap adds nothing the store's own count sees, so that
	// count left the whole queue outside the cap, and two frames of 448,568 structs each parked
	// behind a gap were admitted over a room of 600,000 structs whose count read 600,000
	// throughout, leaving a stored state of 2,994,565 bytes holding 897,136 parked structs that
	// no longer decoded (TestR6Finding2ParkedStructsEscapeTheDecodeCap, dispatch://INFRA-179,
	// review 6's finding 2). The library counts both as each apply commits, so the room is
	// weighed on what it holds rather than on what this package could read of it between
	// applies.
	//
	// The queue is then read once itself (crdt.Doc.PendingStats, a read lock and a handful of
	// fields, allocating nothing at all for the empty queue every converging room has), and each
	// parked struct is charged again and each parked delete range once (storedCeiling). Nothing
	// parked is stored any more (leaveParkedOut), so those terms bound the queue the room holds in
	// memory, out of the same headroom as what it integrates.
	structs := applies.structsHeld(info.Doc)
	parkedBefore := info.Doc.PendingStats()
	ceiling := storedCeiling(structs, parkedBefore)
	rendering, measured := weighing.size()
	// The pressure is read once for this update: whether the open rooms are past their budget,
	// and what the room's own growth under that pressure has left it to cut with
	// (splitsUnderPressure), which the mark that reading makes is what bounds. The state cap
	// weighs what the room holds resident, its tracked state and what its pending queue holds
	// (roomResidentBytes, noteQueued), so frames that park take the room no further than frames
	// that land.
	full := s.underPressure(info.Room)
	allowance := peerApplyAllowance(int64(len(info.Update)), rendering, weighing.roomResidentBytes(),
		ceiling, measured, peerPressure{full: full, splits: applies.splitsUnderPressure(ceiling, full)})
	// A heavy update takes its bytes before the apply and gives them back once the apply has
	// returned, so the whole decode runs inside one grant; a keystroke takes none. What it takes
	// is what its own apply adds, the update's bytes at copyWeight, rather than a whole slot, so
	// a reconnect storm's handoffs decode as many at a time as the pool holds. The wait has no
	// deadline, since the peer's read loop carries none and every holder gives its bytes back
	// (loadSlots), so it cannot fail. The structs the update would add are what the apply's own
	// allowance bounds and are not counted until the apply decodes them, so the state term
	// alone weighs the grant (copyWeight).
	if len(info.Update) > unslottedApplyBytes {
		slot := newLoadSlot()
		_ = slot.take(context.Background(), copyWeight(int64(len(info.Update)), 0))
		defer slot.release()
	}
	// The client ids the update writes under are judged last before the apply, since the
	// judgement can claim ids for the sender (judgeClients), which a refusal above would then
	// have to give back; a claim whose apply does not stand is given back below, and the first
	// apply after which the room holds a struct under the socket's own id marks that id's row
	// (settleClients).
	judged, refusal := s.judgeClients(info)
	if refusal != nil {
		return refusal
	}
	// The scan allowance is reserved last, so no refusal above leaves one in flight, and settled
	// as soon as the apply returns, with what its scans passed.
	scans := weighing.roomScans()
	scanning := scans.reserve(info.Origin, scanCeiling(len(info.Update)))
	content, err := applies.applying(info.Origin, allowance.budget, apply)
	scans.settle(scanning, err)
	// A drain the room took back leaves the update that released it standing and committed
	// (crdt.ErrApplyCheckDrain), which ygo answers as the success it is and relays as what it
	// integrated (provider/websocket/peer_update.go, updateStood); the drain itself was discarded,
	// and the browsers whose frames it held are told (closeDiscardedSenders).
	stood := err == nil || errors.Is(err, crdt.ErrApplyCheckDrain)
	s.settleClients(info, judged, stood)
	if discarded := (*crdt.DrainDiscardedError)(nil); errors.As(err, &discarded) {
		s.closeDiscardedSenders(info, discarded)
	}
	if !stood {
		return s.refusedByApply(info, weighing, allowance, scanning, err)
	}
	// What the apply left in the room's pending queue is the room's from here, in memory beside
	// its encoded state (noteQueued). The room's update observer charged the room's budget inside
	// the apply, before the queue could be read, so the room is weighed again where its queue's
	// charge moved. A refused apply left the queue as it was and is charged nothing. A frame that
	// left something of its own parked was answered not applied, and its sender waits for what
	// parked to land (noteParked, parked_resync.go).
	parkedAfter := info.Doc.PendingStats()
	if before := applies.queuedBytes(); applies.noteQueued(parkedAfter, content) != before {
		weighing.roomCharge().weigh(applies.residentBytes(), int64(applies.structsHeld(info.Doc)))
	}
	applies.noteParked(info, parkedBefore, parkedAfter)
	return nil
}

// closeDiscardedSenders closes every browser whose parked frame a refused drain discarded
// (crdt.DrainDiscardedError.Senders, leaveParkedOut): the update that filled the gap stood and
// was relayed as what it integrated, and what parked behind the gap is gone from the room, which
// never relayed, answered applied or stored it. Each sender is given the close of what refused the
// drain: where the render check refused what the drain would land, the check's own close
// (renderRefusal.close) - DOC_TOO_LARGE for a rendering past the bound, DOC_UPDATE_REFUSED for a
// document this build cannot render - and DOC_TOO_TANGLED where the drain's scans or splits passed
// the apply's allowance. The editor drops the frame the server last answered not applied, which
// the parked frame was, and tells the person why (connection.ts). A drain's check weighs it against
// the rendering the check of the update it follows made, which is exact, so it is refused for now
// (renderRefusal.unsure) and closed with the transient DOC_BUSY only where that update went
// unchecked and what the drain started from was not known: nothing says the edit is too large
// then, so the honest answer is the one the editor keeps the edit and sends it again for, and its
// resend is weighed whole, its gap now filled. A sender already gone is closed by nothing, and its
// saved row is weighed the same way when it comes back. The close is safe from this goroutine and
// goes out after the answers the sender is still owed (websocket.Server.ClosePeer).
func (s *Service) closeDiscardedSenders(info websocket.PeerUpdateInfo, discarded *crdt.DrainDiscardedError) {
	refusal := documentTooTangled()
	var rendered *renderRefusal
	if errors.As(discarded.Cause, &rendered) {
		refusal = rendered.close()
	}
	closed := 0
	for _, sender := range discarded.Senders {
		if s.srv.ClosePeer(sender, refusal.Code, refusal.Reason) {
			closed++
		}
	}
	slog.Warn("dispatch: the room took a browser's update and discarded the drain it released; the browsers that parked what was discarded are closed",
		"room", info.Room, "update_bytes", len(info.Update), "discarded_structs", discarded.Structs,
		"discarded_ranges", discarded.Ranges, "drain_scanned", discarded.Scanned,
		"senders", len(discarded.Senders), "closed", closed, "close_code", refusal.Code,
		"close_reason", refusal.Reason, "error", discarded.Error())
}

// underPressure is whether room's next update is weighed under the budget's pressure: the budget
// is full, which is a sweep having closed every room it could and left the charges over the
// limit (roomBudget.swept).
//
// It starts a sweep and does not wait for it. R3 puts closing the rooms nothing is still using
// before refusing anything, and that close is the sweep started here; what the refusal no
// longer does is hold a person's keystroke while it runs. The update is refused at once with
// the transient DocumentBusyCloseCode, which the editor retries, and the retry applies where
// the sweep has freed a room - so R3's order is kept by the retry rather than by the peer's
// read loop waiting on it.
//
// It waited two seconds before, the time a sweep that has rooms to close needs. Every
// measurement of that wait freed nothing: thirteen waits over the growing-rooms drive, each
// `freed=false waited_ms=0`, because a browser's departure closes its own room by the handler's
// path before any sweep looks, and no sweep line named any of the ten rooms that had gone. What
// it cost is a person's typing: under the burst of 320 concurrent API writes it took 2,066 ms
// of one keystroke's 2,069, and 1,574 ms of another's 1,574
// (cmd/dispatch, TestABurstOfConcurrentLiveWritesStaysUnderThePodsMemory, INFRA-179's keystroke
// measurement). The sweep stays and the refusal stays; only the wait is gone.
func (s *Service) underPressure(room string) bool {
	if !s.budget.full.Load() {
		return false
	}
	used, limit := s.budget.sweepForPressure()
	slog.Info("dispatch: a browser's update started the room budget's sweep and was weighed under the pressure at once",
		"room", room, "charged", used, "limit", limit)
	return true
}

// refusedBeforeApply is the refusal the hook makes before the apply of an update carrying the
// history its document's rebuild replaced (weighPeerUpdate); the client id rule's, also made
// before the apply, are judgeClients'. A room whose history retired no client reads nothing of
// the update. It carries the test hook's armed refusal too (refusesNextPeerUpdateForTest), which
// stands for a refusal of the rules below it.
//
// An update whose header the scan cannot read refuses nothing here and falls through to the
// apply, which judges the update itself: ygo's own decoder refuses the same bytes, and the hook
// answers that refusal DOC_UPDATE_REFUSED below. The scan has one bound the decoder's own apply
// does not share - it reads no update carrying more structs than one update may decode
// (maxV2Items) - and refusing such an update here would close an honest whole-state handoff of
// a rebuilt document at the decode cap before the apply could weigh it at all.
func (s *Service) refusedBeforeApply(info websocket.PeerUpdateInfo, weighing *roomWeighing) error {
	if s.refusesNextPeerUpdateForTest(info.Room) {
		refusal := documentUpdateRefused()
		logRefusal(info, refusal, "dispatch: refused a browser's update the test hook armed this document for",
			"rule", "the test hook")
		return refusal
	}
	retired := weighing.retiredClients()
	if len(retired) == 0 {
		return nil
	}
	from, _, err := crdt.ParseUpdateMeta(info.Update)
	if err != nil {
		slog.Info("dispatch: a browser's update the retired-client scan could not read; the apply will judge it",
			"room", info.Room, "update_bytes", len(info.Update), "error", err.Error())
		return nil
	}
	for client, clock := range from {
		if at, listed := retired[client]; listed && clock < at {
			slog.Info("dispatch: refused a browser's update carrying the history its document's rebuild replaced",
				"room", info.Room, "update_bytes", len(info.Update), "client", uint64(client),
				"client_clock", clock, "retired_at", at)
			return documentRebuilt()
		}
	}
	return nil
}

// refusedByApply is the close a failed apply of a browser's update becomes.
//
// An apply the allowance refused (crdt.ErrApplyBudget) reports what it reached of each of the
// three counts, and the first of them at or past its own field names the rule that set that
// field, which names the close: the decode count is every origin's and is DOC_TOO_LARGE, and the
// structs and the bytes carry whichever rule set them (peerRule). A cap's close offers the
// rebuild only where the route takes the room as the refusal leaves it, which a refused apply
// leaves as it was (nearACapUnencoded, read only here, once the apply has failed). One ygo
// refused for a root the browser may not name (crdt.ErrRootNotAllowed) is DOC_UPDATE_REFUSED:
// the editor never sends one. One whose merge would scan past its allowance
// (crdt.ErrIntegrateScanBudget) is DOC_TOO_TANGLED, or DOC_NEEDS_REBUILD where the room's scan
// total was used up and the rebuild route takes the room (rebuildTakes). One the room's pending
// cap refused (crdt.ErrPendingCap) is returned as it
// is, the one error the hook answers that is no *websocket.PeerClose, so ygo answers that browser
// SyncStatus(0) and leaves it connected, exactly as a room without this hook does: the update
// parks past the cap only because the structs it depends on have not arrived, and the browser
// that sends it again once its gap fills is right to stay. One the room's apply check refused
// (crdt.ErrApplyCheck, renderCheck) carries the check's own refusal, whose close is
// DOC_TOO_LARGE for a rendering past the bound and DOC_UPDATE_REFUSED for one this build cannot
// render (renderRefusal.close). Any other refusal of the bytes themselves (crdt.ErrInvalidUpdate)
// is DOC_UPDATE_REFUSED: ygo will not read them, and the client that sent them sends them again
// on every reconnect.
func (s *Service) refusedByApply(info websocket.PeerUpdateInfo, weighing *roomWeighing, allowance peerAllowance, scanning *scanAllowance, err error) error {
	var refused *crdt.RefusedUpdateError
	errors.As(err, &refused)
	switch {
	case errors.Is(err, crdt.ErrApplyBudget):
		rule := allowance.rule(refused)
		// Only the two caps offer the rebuild, so only their refusal pays the reading of the room.
		rebuildTaken := (rule == ruleStateCap || rule == ruleDecodeCap) && nearACapUnencoded(info.Doc, weighing.roomStateBytes())
		refusal := rule.refusal(rebuildTaken)
		logRefusal(info, refusal, "dispatch: refused a browser's update its apply may not take", "rule", rule.why(),
			"decoded", refused.Decoded, "decode_limit", allowance.budget.Decoded,
			"structs", refused.Structs, "structs_limit", allowance.budget.Structs, "struct_rule_limit", allowance.structsLimit,
			"content_bytes", refused.Bytes, "content_limit", allowance.budget.Bytes, "byte_rule_limit", allowance.bytesLimit,
			"split", refused.Split, "split_limit", allowance.budget.Split, "split_rule_limit", allowance.splitsLimit)
		return refusedWith(refusal, err)
	case errors.Is(err, crdt.ErrRootNotAllowed):
		refusal := documentUpdateRefused()
		logRefusal(info, refusal, "dispatch: refused a browser's update naming a root the editor never writes",
			"root", refused.Root)
		return refusedWith(refusal, err)
	case errors.Is(err, crdt.ErrContentNotAllowed):
		refusal := documentUpdateRefused()
		logRefusal(info, refusal, "dispatch: refused a browser's update carrying a kind of content the editor never sends",
			"refused_kinds", fmt.Sprint(peerRefusedContent), "error", err.Error())
		return refusedWith(refusal, err)
	case errors.Is(err, crdt.ErrIntegrateScanBudget):
		// The honest long session's updates scan 12 items at most, so a refusal is worth a line,
		// with the budget's figures and the room's scan total as the refusal leaves them.
		used, limit := s.budget.figures()
		scanned, left := weighing.roomScans().figures()
		slog.Warn("dispatch: refused a browser's update its room could not merge within its scan allowance",
			"room", info.Room, "update_bytes", len(info.Update), "charged", used, "limit", limit,
			"structs", weighing.applies().structsHeld(info.Doc), "allowance", scanning.limit(),
			"scan_total", scanned, "scans_left", left)
		if scanning.roomUsedUp() && s.rebuildTakes(info.Doc, weighing) {
			return documentNeedsRebuild()
		}
		return documentTooTangled()
	case errors.Is(err, crdt.ErrPendingCap):
		// No line: a browser whose gap has not filled meets this on every frame it sends until
		// the gap does fill, and the socket stays open, so one line here would be a flood where
		// every other refusal here closes the socket and so logs at most once.
		return err
	case errors.Is(err, crdt.ErrApplyCheck):
		var checked *renderRefusal
		if !errors.As(err, &checked) {
			checked = &renderRefusal{before: sizeUnmeasured, after: sizeUnmeasured, cause: err}
		}
		refusal := checked.close()
		logRefusal(info, refusal, checked.why(), "rendering_before", checked.before, "rendering_after", checked.after,
			"rendering_limit", maxPeerRenderingBytes, "error", err.Error())
		return refusedWith(refusal, err)
	case errors.Is(err, crdt.ErrInvalidUpdate):
		refusal := documentUpdateRefused()
		logRefusal(info, refusal, "dispatch: refused a browser's update ygo will not read", "error", err.Error())
		return refusedWith(refusal, err)
	}
	return err
}

// rebuildTakes reports whether the rebuild route takes live, a resident room whose scan total
// the room's applies have used up, as a refusal of a browser's update leaves it: by the route's
// own reading of a resident room, nearACap on the room's own figures (RebuildDocument), after the
// two of its tests nearACapUnencoded reads without encoding, which answer near only where it
// does.
//
// The hook's total and the route's test of the scans differ, so neither can stand for the other.
// The total is what the room has scanned since its load, each apply's scans as it merged; the
// route re-scans the room's state as one fresh decode (rescan), and a decode integrates the
// state's client groups by descending client, where inserts naming one anchor and no right
// origin are symmetric and ygo breaks at the first of them. So a room whose browsers used the
// total up one scanning frame at a time can re-scan a few per cent of it: such a room was closed
// DOC_NEEDS_REBUILD and the route answered the rebuild it offered 409 DOCUMENT_LIVE
// (TestAUsedUpRoomTheRouteWouldNotRebuildIsNotOfferedARebuild, dispatch://INFRA-179). There the
// update is too tangled for what is left, and the room's next load re-measures its total from
// that re-scan, which is what DOC_TOO_TANGLED leaves to happen.
//
// The re-scan is a whole-document encode and decode, under a grant of the pod's copy heap
// weighed as the route weighs it (copyOfRoom) and waited for within WriteWaitBound, and it is
// made only here: once a browser's update has been refused for its scans in a room whose total
// is used up, which closes that browser. A reading that cannot be made answers far, so no close
// offers a rebuild nothing has read the room near for.
func (s *Service) rebuildTakes(live *crdt.Doc, weighing *roomWeighing) bool {
	tracked := weighing.roomStateBytes()
	if nearACapUnencoded(live, tracked) {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), WriteWaitBound)
	defer cancel()
	_, near, err := nearACap(ctx, live, s.copyOfRoom(live), tracked)
	return err == nil && near
}

// rule is the rule a refused apply broke: the first count past its own field of the allowance,
// read in the order the fields bound the apply in - the decode as the bytes are read, then the
// structs and the bytes as they integrate. A field of no limit (below zero) bounds nothing and
// names nothing. A refusal with no counters at all, which ygo does not make for this error,
// falls to the decode rule, whose close is the document's own.
//
// Past, not at: ygo refuses a count that passes its field and admits one that reaches it
// exactly (fork crdt/apply_rollback.go, integratedErr's `>`), so a count equal to its field is a
// field the apply kept. Reading it as broken named the wrong rule where two fields were tight at
// once - a refusal of 1 struct and 50 bytes against an allowance of 1 struct and 10 bytes was
// named the decode cap, which closes DOC_TOO_LARGE with no rebuild offered, where the bytes rule
// meant DOC_NEEDS_REBUILD - and under pressure it turned a refusal that should pass by itself
// into a permanent one, so the editor dropped the person's edit rather than retrying it
// (dispatch://INFRA-179, review 5820480859's finding 7).
//
// The split count is read last, after the bytes: the struct and byte counts are final where the
// apply's struct phase ends, before its delete set, while most of an update's splits are made
// after that point (crdt.ApplyBudget.Split). It names the rule that set Split, which is the
// decode cap, whose headroom the splits spend beside the structs; the state cap, whose bytes a
// split spends without carrying content; or the room's growth under the pressure,
// pressureSplits at most (splitsUnderPressure).
func (a peerAllowance) rule(refused *crdt.RefusedUpdateError) peerRule {
	switch {
	case refused == nil:
		return ruleDecodedValues
	case a.budget.Decoded >= 0 && refused.Decoded > a.budget.Decoded:
		return ruleDecodedValues
	case a.budget.Structs >= 0 && refused.Structs > a.budget.Structs:
		return a.structs
	case a.budget.Bytes >= 0 && refused.Bytes > a.budget.Bytes:
		return a.bytes
	case a.budget.Split >= 0 && refused.Split > a.budget.Split:
		return a.splits
	}
	return ruleDecodedValues
}

// documentTooLarge is the document's own refusal of an update, permanent: the editor drops the
// refused edit and rebuilds from a fresh read of the document.
func documentTooLarge() *websocket.PeerClose {
	return &websocket.PeerClose{
		Code:   contracts.DocumentTooLargeCloseCode,
		Reason: contracts.DocumentTooLargeCloseReason,
	}
}

// documentBusy is the transient refusal of what the room cannot take for now - an update under the
// room budget's pressure (peerRule.refusal), an update whose client ids the table could not be
// read for (clientCheckFailed), an update whose rendering the check could not size yet
// (renderRefusal.close), and a first sync the pod's copy heap had no room to answer
// (refusedStateAnswer): the editor keeps its edits and sends them again.
func documentBusy() *websocket.PeerClose {
	return &websocket.PeerClose{Code: contracts.DocumentBusyCloseCode, Reason: contracts.DocumentBusyCloseReason}
}

// documentTooTangled is the refusal of an update the room's document could not merge within its
// scan allowance while the room's scan total was not used up (roomScans.reserve), or was used up
// in a room the rebuild route would not take (rebuildTakes), permanent as documentTooLarge is and
// under the same code, with a reason of its own so the editor can say why the edit was dropped.
func documentTooTangled() *websocket.PeerClose {
	return &websocket.PeerClose{
		Code:   contracts.DocumentTooLargeCloseCode,
		Reason: contracts.DocumentTooTangledCloseReason,
	}
}

// documentNeedsRebuild is the refusal of an update the document cannot take until it is rebuilt -
// one its room's used-up scan total refuses (rebuildTakes), and one that would take its state past
// maxRoomStateBytes or its structs past maxUpdateItems (peerRule.refusal), each in a room the
// rebuild route takes - permanent as documentTooLarge is and under the same code, with a reason of
// its own: the editor offers the rebuild that would take the edit.
func documentNeedsRebuild() *websocket.PeerClose {
	return &websocket.PeerClose{
		Code:   contracts.DocumentTooLargeCloseCode,
		Reason: contracts.DocumentNeedsRebuildCloseReason,
	}
}

// documentRebuilt is the refusal of an update carrying the history a rebuild of the document
// replaced (weighPeerUpdate), permanent as documentTooLarge is and under the same code, with a
// reason of its own: the document has just been rebuilt, so the editor says so and offers no
// rebuild, where documentNeedsRebuild's does.
func documentRebuilt() *websocket.PeerClose {
	return &websocket.PeerClose{
		Code:   contracts.DocumentTooLargeCloseCode,
		Reason: contracts.DocumentRebuiltCloseReason,
	}
}

// documentUpdateRefused is the refusal of an update the room will not read: one whose bytes ygo
// refuses, and one naming a root the editor never writes - a subdocument, an unknown root, the
// retired clients' list only a rebuild writes. Permanent as documentTooLarge is and under the
// same code, with a reason of its own: the client that sent it sends it again on every reconnect
// (@hocuspocus/provider 2.x reconnects at once after a close it does not read as permanent), so
// the one answer that ends the exchange is the one the editor drops the edit for.
func documentUpdateRefused() *websocket.PeerClose {
	return &websocket.PeerClose{
		Code:   contracts.DocumentTooLargeCloseCode,
		Reason: contracts.DocumentUpdateRefusedCloseReason,
	}
}

// documentClientIDRefused is the refusal of an update carrying structs under a client id that is
// not its sender's (judgeClients, client_ids.go), permanent as documentTooLarge is and under the
// same code, with a reason of its own: the editor drops the refused edit and rebuilds its document
// under a fresh client id.
func documentClientIDRefused() *websocket.PeerClose {
	return &websocket.PeerClose{
		Code:   contracts.DocumentTooLargeCloseCode,
		Reason: contracts.DocumentClientIdRefusedCloseReason,
	}
}

// refusedWith is refusal, the close ygo answers the browser with - it reads it through errors.As
// (sjawhar/ygo v1.55.1-sami.8, provider/websocket/peer_update.go:142-149) - with cause behind it,
// which no close frame carries: ygo's own refusal of the update, for this package's tests. ygo
// logs that cause at Debug alone (provider/websocket/peer_update.go:142-143), which the pod's
// level drops, so what an operator reads of a refusal is the hook's own line (logRefusal) and
// never this error.
func refusedWith(refusal *websocket.PeerClose, cause error) error {
	return fmt.Errorf("%w: %w", refusal, cause)
}

// logRefusal is the one line a refusal of a browser's update leaves. ygo logs a refused peer
// update at Debug alone (sjawhar/ygo v1.55.1-sami.8, provider/websocket/peer_update.go:142-143),
// which the pod's level drops, so without this line an operator sees an editor's socket close and
// nothing says why. message names the rule in plain words and figures are what refused it, beside
// the room, the update's bytes and the close that browser is given. Neither the document's
// content nor the update's bytes are ever logged: both are the person's own text.
func logRefusal(info websocket.PeerUpdateInfo, refusal *websocket.PeerClose, message string, figures ...any) {
	slog.Warn(message, append([]any{
		"room", info.Room, "update_bytes", len(info.Update),
		"close_code", refusal.Code, "close_reason", refusal.Reason,
	}, figures...)...)
}

// roomWeighingOf is what the hook reads of the room holding live (keepReplica), or nil for a
// document no room's load listed. It is a load off the listing the room's reads already go through
// (holdLive), so the peer's read loop takes neither the replica's lock, which every browser in the
// room waits behind (AGENTS.md:559-562), nor the room's state lock; and the listing holds the
// weighing strongly, so the read makes no weak pointer strong, which the collector can block for
// the rest of a mark phase (roomWeighing).
func (s *Service) roomWeighingOf(live *crdt.Doc) *roomWeighing {
	listed, ok := s.replicas.Load(weak.Make(live))
	if !ok {
		return nil
	}
	return listed.(*roomListing).weighing
}
