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
// none of it, so a browser can carry a document past the bound every server write obeys
// (refuseGrowth). What stops that here is weighPeerUpdate, ygo's Server.OnPeerUpdate, which the
// room runs in place of its own apply for each update and whole-state message a read-write browser
// sends.
//
// It does not run applyLive's checks. Those weigh what a write leaves, which costs a fork of the
// document, a tree build and a full markdown render - about 420 ms for the fork alone at 1 MiB and
// a whole further copy of the document, with the render about 160 ms (AGENTS.md:563-602,
// :780-781) - against a keystroke on such a document that already takes between half a second and
// a second. What it does instead is hand ygo an allowance built from what it can see without
// reading the update at all - the update's own length, the structs the room's last committed
// apply reported it holding (crdt.ApplyReport.Structs, roomApplies.structsHeld), the room's
// last published rendering (renderedReplica.publishSize) and
// its tracked state bytes - and let the apply itself count what the update decodes, integrates
// and adds against that allowance, rolling the whole apply back where it passes
// (crdt.WithApplyBudget). For a keystroke that is a handful of comparisons; nothing here is
// proportional to the document or to the update.
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
// The bound is not a ceiling the document cannot pass: the rendering the hook compares against is
// as recent as the room's last update observer, so a document passes it by the updates in flight
// when it crosses, and this is all that is claimed here.
//
// The document has a second bound of its own, which bytes cannot see: the decode cap. ygo decodes
// at most maxUpdateItems structs in one update, and a room's whole state is one update to the next
// load that reads it, so a room whose state holds more could never be loaded again once its
// updates were folded. A browser can get there without passing the byte bound - a frame of GC
// structs is two bytes a struct and renders nothing - so the apply's allowance of structs comes
// out of what is left of the cap above the structs the room holds, as the room's last
// committed apply reported them (roomApplies.structsHeld, which counts what the room has
// parked beside what its store holds), and an apply that would integrate more is
// refused with DocumentTooLargeCloseCode and taken back out whole.
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
// bounds the two together by dividing its headroom between them, half to the splits and the
// rest to the structs (peerApplyAllowance). One figure in both fields bounds neither, since ygo
// checks each field on its own: an update carrying k structs and cutting k items left a room k
// short of the cap k past it (TestR6Finding1StructsAndSplitShareNoHeadroom).
//
// The document has a third bound, which neither bytes nor structs see: what a room holds that is
// not text, a value of any size in one clock and one struct. The server's own writes bound the
// margin's records at maxMarginBytes and the anchor marks' ids at maxAnchorBytes (growth.go), and
// a browser's update runs through neither: measured on 2026-10-26, eighty browser updates each
// writing a million bytes into the margin were all admitted, and took the room's whole-state frame
// from 264 bytes to 80,001,457 with its rendering at 42, past the message floor every browser of
// the document then reconnects against. What answers that now is the state cap
// (maxRoomStateBytes), which bounds the room's whole encoded state whatever the bytes of it
// render.
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
// The last of them is no bound at all but a room that has stopped converging. A drain of the
// room's pending queue whose splits pass Split is refused and taken back while the apply that
// filled the gap stands, deliberately: the parked content is another sender's and the sender
// that filled the gap can neither see it nor send less of it. So a room can hold a backlog no
// update will drain, and no error says so; what answers it is the room's own reading of its
// queue (refusedStuckBacklog).

// maxPeerInsertAtTheBound is the most new content one update may carry into a document already at
// or over the bound: enough for the editor's own derived writes - heading ids, list labels, anchor
// marks - and for ordinary typing and deleting to carry on, and not enough for a paste. A document
// can therefore creep above the bound, and maxPeerByteCreep caps how far.
const maxPeerInsertAtTheBound = 8 << 10

// maxPeerByteCreep is how far past the bound a room's measurement may creep before only an update
// that inserts nothing at all is admitted, so a document that has crossed can still be trimmed or
// split and nothing can grow it further.
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
// permanent close for the rules a rebuild cannot clear, the rebuild offer for the two caps only a
// rebuild clears - the state cap and the decode cap - and the busy close for the pressure, which
// passes by itself.
//
// The decode cap offers the rebuild because nothing else helps: a room at it refuses every update
// that adds a struct, so the person's next edit is refused too, and the route accepts exactly
// such a document (nearACap reads the struct count first). Closing it DOC_TOO_LARGE told the
// person their document "is larger than a Dispatch document may be" and offered no remedy where
// the one remedy was a click away (dispatch://INFRA-179, review 5820480859's finding 6).
func (r peerRule) refusal() *websocket.PeerClose {
	switch r {
	case ruleStateCap, ruleDecodeCap:
		return documentNeedsRebuild()
	case rulePressure:
		return &websocket.PeerClose{Code: contracts.DocumentBusyCloseCode, Reason: contracts.DocumentBusyCloseReason}
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
	// structs and bytes name the rule each of budget's bounded fields came from, and
	// structsLimit and bytesLimit that rule's own figure, which the refusal's line carries.
	// Split has no pair of its own: it comes from the rule structs names and carries that
	// rule's own figure, since the two fields divide one headroom - what that rule leaves
	// above the room - rather than bounding the apply one each (peerApplyAllowance).
	structs, bytes           peerRule
	structsLimit, bytesLimit int64
}

// peerApplyAllowance is the allowance an update of update bytes applies under in a room holding
// structs structs (roomApplies.structsHeld) and which holds stateBytes bytes of
// tracked state, whose last published rendering measures rendering bytes (measured false for a
// room with none), while the budget is full or not.
//
// Each field is the least of the rules that apply to it, and each rule's own figure is kept so
// the refusal can name it:
//
//   - Decoded is maxDecodedValuesPerApply always: no origin decodes more.
//   - Structs and Split divide what is left of the decode cap above what the room holds (the
//     decode cap rule): the splits take half of that headroom and the structs the rest. A split
//     is a struct the room keeps and the next load must decode, so what bounds what an update
//     carries in bounds what it cuts; but ygo counts the two apart and checks each against its
//     own field (crdt.ApplyBudget.Structs, .Split, sjawhar/ygo v1.55.1-sami.9), so the whole
//     headroom written into both bounded neither. An update inserting k characters into the
//     middle of a run carries k structs and cuts k items: against a room k short of the cap
//     each count stayed within its own field and the room ended k past the cap holding a state
//     that no longer decodes - 1,048,577 structs for k=1 and 1,049,576 for k=1,000, both
//     `crdt: invalid update` on the next load
//     (TestR6Finding1StructsAndSplitShareNoHeadroom, dispatch://INFRA-179, review 6's
//     finding 1). Divided, the two sums cannot pass the headroom whatever the update's mix of
//     the two, and an update that fills both fields leaves the room at the cap exactly.
//   - Bytes is no limit below the byte bound; maxPeerInsertAtTheBound at or over it (the bound);
//     nothing past the creep cap (the creep cap); and never more than what is left of
//     maxRoomStateBytes above the room's tracked state (the state cap).
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
		structsLimit: maxUpdateItems,
		bytesLimit:   maxRoomStateBytes,
	}
	// The one headroom the decode cap leaves, divided between the two fields that spend it:
	// half to the splits, the rest to the structs, so their sum is the headroom and no mix of
	// the two passes it. The structs take the odd one, because typing is what a room near the
	// cap still has to take: a room one struct short of it admits an insert that splits no
	// item, where the odd one given to the splits would refuse every insert and admit only a
	// delete that cuts one item.
	headroom := capHeadroom(structs)
	allowance.budget.Split = headroom / 2
	allowance.budget.Structs = headroom - allowance.budget.Split
	switch {
	case measured && rendering >= pmdoc.MaxDocumentBytes+maxPeerByteCreep:
		allowance.budget.Bytes, allowance.bytes, allowance.bytesLimit = 0, ruleCreepCap, maxPeerByteCreep
	case rendering+update >= pmdoc.MaxDocumentBytes:
		allowance.budget.Bytes, allowance.bytes, allowance.bytesLimit = maxPeerInsertAtTheBound, ruleByteBound, maxPeerInsertAtTheBound
	}
	if state := clampedBytes(maxRoomStateBytes - stateBytes); allowance.budget.Bytes < 0 || state < allowance.budget.Bytes {
		allowance.budget.Bytes, allowance.bytes, allowance.bytesLimit = state, ruleStateCap, maxRoomStateBytes
	}
	if pressure.full {
		allowance.budget.Structs = 0
		allowance.budget.Split = min(allowance.budget.Split, pressure.splits)
		allowance.structs, allowance.structsLimit = rulePressure, 0
		allowance.budget.Bytes, allowance.bytes, allowance.bytesLimit = 0, rulePressure, 0
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

// capHeadroom is what is left of the decode cap above a room holding structs structs
// (roomApplies.structsHeld), never below nothing: a room already at or past the cap may
// add no struct, and a field below zero would read as no limit at all (crdt.ApplyBudget). It is
// what the apply's structs and splits divide between them (peerApplyAllowance).
func capHeadroom(structs uint64) int {
	if structs >= maxUpdateItems {
		return 0
	}
	return int(maxUpdateItems - structs)
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
// lock as each remote apply begins, so none may call the document; all take nothing but mu, which
// nothing holds across a call into the document.
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
	mu        sync.Mutex
	allowance map[any]crdt.ApplyBudget
	// parkedFloor is the fewest items and delete ranges the room's pending queue has held at one
	// of the hook's readings since that queue was last empty, and parkedSeen whether there has
	// been such a reading: what tells a queue that has stopped falling from one that is still
	// draining (backlogStalled, refusedStuckBacklog).
	parkedFloor int
	parkedSeen  bool
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
	// commit that neither a report nor the replica measured, or the room was loaded while it
	// held something pending, which makes the load's reading a lower bound. The next re-read or
	// re-base whose reading is exact clears it (take, reread).
	state   atomic.Int64
	commits atomic.Uint64
	owed    atomic.Bool
	// unpaired is the origins of the reports no drained commit has been paired with yet, in
	// commit order, guarded by mu; paired is whether the room keeps them, which a room whose
	// commits a replica drains does (newRoomApplies). ygo logs a commit and then reports it
	// under the same hold of the document's lock, so a drain that takes the reports made
	// before it takes the log finds the report of every remote apply it hands over
	// (reportsMade, reportPairing).
	unpaired []any
	paired   bool
}

// newRoomApplies sets live, a room's document, to apply every browser's update under the
// allowance the hook reserved for it, to refuse one naming any root but the fragment and the
// margin, to refuse one carrying a kind of content the editor never sends, and to report what
// each apply that commits leaves it holding, as newRoomScans sets its scan budget. From here
// live applies every remote update whole or not at all. paired is whether a replica drains the
// room's commits from its update log, and so pairs each one with its report (unpaired).
//
// The report is what makes the library and not this package the authority on what a room holds
// (crdt.WithApplyReport, dispatch://INFRA-179): ygo reads the two counters that answer it - the
// store's structs and the pending queue's - as each apply commits, under the lock that apply
// held, and hands them over, where this package used to walk the client slices again at each
// drain, each copy and each update weighed. It costs the room less than the readings it
// replaces: one reading an apply, against one a drain plus one an update weighed plus one a
// copy taken (the fork's measurement is 13.4 µs for the pair of counters,
// crdt/doc.go:446-448).
func newRoomApplies(live *crdt.Doc, paired bool) *roomApplies {
	applies := &roomApplies{paired: paired}
	live.SetApplyBudget(applies.budget)
	live.SetAllowedRoots(applies.roots)
	live.SetRefusedContent(applies.refusedContent)
	live.SetApplyReport(applies.noteApply)
	return applies
}

// noteApply keeps what an apply that has just committed in this room reported: the structs the
// document now holds, the bytes the apply added to its encoded state, and the apply's origin,
// which the drain that hands the commit over pairs it with (reportPairing). ygo calls it under
// the document's lock and inside the transaction that applied the update, so it takes nothing
// but mu and calls nothing back into the document (crdt.WithApplyReport).
//
// A refused or rolled-back apply reports nothing, so what is kept here is always a total the
// document stood at. An apply of a local transaction reports nothing either - the report is of
// remote applies - so a server write made in the room's own transaction leaves the struct
// figure behind the document until the next remote apply reports again or the anchor reads the
// document itself (anchorStructs, which logs the difference), and leaves the byte figure to the
// drain (renderedReplica.countCommit).
//
// The bytes are the library's count of the direct form of the encoded state: each struct and
// skip where the store or the pending queue holds it. A room whose pending queue or pending
// deletes hold anything encodes through a merge instead, up to 4 bytes shorter for each value
// the queue touches, so one apply's figure can fall short of what the encode grew by and the
// running total still never does: measured in the library, a park reported 5 while the state
// grew 1, the fill that followed reported 6 while it grew 10, and an overwrite collecting ten
// such values reported -76 while the state moved -36. That is why a reading of a room holding
// something pending is a lower bound and never replaces the total (take).
func (a *roomApplies) noteApply(origin any, report crdt.ApplyReport) {
	a.structs.Store(int64(report.Structs))
	a.reported.Store(true)
	a.state.Add(int64(report.StateBytes))
	a.commits.Add(1)
	if a.paired {
		a.mu.Lock()
		a.unpaired = append(a.unpaired, origin)
		a.mu.Unlock()
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
// came to (chargeLoadedRoom). A load that left live holding something pending read a lower
// bound (noteApply), so the figure is owed a re-read from the room itself.
func (a *roomApplies) loaded(live *crdt.Doc, stateBytes int64) {
	if a == nil {
		return
	}
	a.state.Store(stateBytes)
	if live != nil && holdsPending(live) {
		a.owed.Store(true)
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
// where it still stands at tracked, and reports whether it did. An exact reading - one of a
// room holding nothing pending, and nothing the reading leaves out - replaces the figure and
// settles what was owed; any other reading is a lower bound, so the figure keeps the larger of
// the two and stays owed if it was. Taking a lower bound as the truth would put the figure
// below the state, and the room would then be admitted past maxRoomStateBytes by the
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

// reread re-bases a room whose tracked state is owed (owe, loaded) on an encode of live itself,
// once live holds nothing pending, where an encode is exact, and reports whether it did; while
// live holds something it waits, since that reading is a lower bound. The caller holds the
// replica's lock, so no drain adds to the figure meanwhile, and the commit count is read on both
// sides of the encode, so a reading is kept only where no report landed while it was taken: it
// then holds every report the figure holds and no other. A commit after the encode is added to
// the figure as it lands, and one the encode holds whose drain is still to come is added again by
// that drain, which is the safe side. The encode costs what a read of the room costs (53.0 ms on
// 494,309 structs), and the first drain to find the room owed and holding nothing pending pays
// it, a keystroke's as well: once for each time a room is owed one, which only a recover, an
// unmeasured commit or a load of a room holding something pending makes it. Until then each
// drain reads the room's pending counts (holdsPending).
func (a *roomApplies) reread(live *crdt.Doc) bool {
	if a == nil || live == nil || !a.owed.Load() || holdsPending(live) {
		return false
	}
	for range rereadTries {
		seen := a.commits.Load()
		tracked := a.state.Load()
		read := int64(len(crdt.EncodeStateAsUpdateV1(live, nil)))
		exact := !holdsPending(live)
		if a.commits.Load() != seen {
			continue
		}
		if a.take(tracked, read, exact) {
			if read != tracked {
				slog.Info("dispatch: a room's tracked state was re-read from the room itself",
					"tracked", tracked, "read", read, "difference", tracked-read)
			}
			return true
		}
	}
	return false
}

// holdsPending is whether live's pending queue or pending deletes hold anything, which makes an
// encode of it a lower bound on the state the room's reports track (noteApply).
func holdsPending(live *crdt.Doc) bool {
	pending := live.PendingStats()
	return pending.Items > 0 || pending.DeleteRanges > 0
}

// reportsMade is the origins of the reports made in the room that no drained commit has been
// paired with yet, in commit order: what a drain about to take the room's log pairs its commits
// against (reportPairing). The slice is capped at its length, so the reports made after it are
// appended past what it holds, and only the drain that holds it drops what it holds
// (settleReports). Nil for a room that keeps no reports.
func (a *roomApplies) reportsMade() []any {
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
	reports []any
	next    int
}

// covers reports whether a report covers the commit the drain has reached, which origin wrote,
// and pairs the two. ygo logs a commit and reports it under one hold of the document's lock, so
// a remote apply's report is among those the drain took and after every report of a commit
// before it: the first one from next with the commit's origin is its own, or one an apply with
// that origin made without logging anything, which leaves the count the same.
//
// A commit no report covers is a local transaction - a server write, or ygo's format cleanup,
// whose origin is nil - since no origin a local transaction is made with is one a remote apply
// is made with: a browser's apply has its peer, a publish its liveWriteOrigin, a catch-up its
// remoteUpdateOrigin, and the server's own writes take an origin of their own each. A remote
// apply whose report is made after a concurrent drain has already taken its commit is taken
// for one as well, and counted again from the replica on top of its report, which is the safe
// side (renderedReplica.countCommit). Every origin here went through the apply budget's map
// (roomApplies.budget), so each compares.
func (p *reportPairing) covers(origin any) bool {
	for at := p.next; at < len(p.reports); at++ {
		if p.reports[at] == origin {
			p.next = at + 1
			return true
		}
	}
	return false
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
	if budget, reserved := a.allowance[origin]; reserved {
		return budget
	}
	return crdt.ApplyBudget{Decoded: -1, Structs: -1, Bytes: -1, Split: -1}
}

// roots is ygo's per-apply root list: the browser's two for the hook's applies, and nil - every
// root - for every other origin.
func (a *roomApplies) roots(origin any) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, reserved := a.allowance[origin]; reserved {
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
	if _, reserved := a.allowance[origin]; reserved {
		return peerRefusedContent
	}
	return nil
}

// applying runs apply, one of the hook's own applies with origin, under budget: reserved before
// the apply and dropped as it returns, so ygo's three questions are answered from it for exactly
// this apply and every other origin's apply is unbounded. Nil, a room no load listed, runs the
// apply under whatever the document itself was built with.
func (a *roomApplies) applying(origin any, budget crdt.ApplyBudget, apply func() error) error {
	if a == nil {
		return apply()
	}
	a.mu.Lock()
	if a.allowance == nil {
		a.allowance = make(map[any]crdt.ApplyBudget)
	}
	a.allowance[origin] = budget
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.allowance, origin)
		a.mu.Unlock()
	}()
	return apply()
}

// backlogStalled records parked, the items and delete ranges the room's pending queue holds as
// this update arrives, and reports whether that queue has stopped falling: it holds something and
// holds no fewer than the fewest this room has read since the queue was last empty. An empty
// queue forgets the floor, so a room that drains and parks again is read afresh, and the first
// reading of a new backlog sets the floor and reports nothing - the room has not yet had a
// chance to see it fall (refusedStuckBacklog).
func (a *roomApplies) backlogStalled(parked int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case parked == 0:
		a.parkedSeen, a.parkedFloor = false, 0
		return false
	case !a.parkedSeen:
		a.parkedSeen, a.parkedFloor = true, parked
		return false
	case parked < a.parkedFloor:
		a.parkedFloor = parked
		return false
	}
	return true
}

// splitsUnderPressure is how many of the room's items the update now being weighed may cut: what
// is left of the room's own growth under this pressure, pressureSplits at most, while the open
// rooms are past their budget (full, Service.underPressure), and pressureSplits while they are
// not, where the splits' half of the decode cap's headroom bounds the apply instead
// (peerApplyAllowance).
//
// structs is the structs the room holds as this update arrives (structsHeld). The first update
// weighed under a pressure marks it: that update is already refused its structs and its bytes by
// the pressure, so the mark is the room as the pressure first refused it something, and what the
// room may reach is that mark and pressureGrowth above it. Each later update is allowed what is
// left between the room's structs and that reach, so the splits an update may make fall to nothing as
// the room nears it and no mix of updates passes it, where eight an update bounded one update
// alone (TestR6Finding3PressureSplitsHaveNoCumulativeBound).
//
// The end of the pressure clears the mark, so a room that has waited one out starts the next
// afresh and a person who could not cut inside a word can again. Nothing else clears it: a room
// whose structs fall under its mark - a rebuild's, which writes a history without the
// tombstones - is read against the mark as it stands and is allowed the whole of pressureSplits.
//
// A room no load listed carries no record to mark and is allowed pressureSplits by each update,
// as it is bounded by nothing else here either (roomWeighing.applies, refusedStuckBacklog).
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
// It refuses two updates before the apply, and every other refusal is the apply's own. The first
// is an update carrying a client group of the history a rebuild replaced - a group of a client the
// rebuilt history lists as retired, starting below the clock it had reached (retiredMapName),
// read from the update's own header (crdt.ParseUpdateMeta, which decodes no content). It comes
// from a browser that held the document before the rebuild and still holds that history:
// applied, its whole-state message would put the old document beside the rebuilt one, every
// block twice, and carry the room back toward the cap the rebuild trimmed. It is refused with
// DocumentTooLargeCloseCode and DocumentRebuiltCloseReason, a reason of its own rather than the
// one a used-up scan total is refused with: the document was just rebuilt, so no rebuild of it
// would help, and the editor tells the person it was rebuilt, builds its document afresh from
// the rebuilt room and drops its saved rows typed against the old history as rows of a rebuilt
// document (connection.ts refusedEditNotice, pending-sync.ts addsParking). An update whose
// header does not read at all is refused DocumentUpdateRefusedCloseReason there and then: ygo
// would refuse the same bytes.
//
// The second is no judgement of the update at all but of the room it arrives in: a room whose
// pending queue has stopped falling and whose decode cap leaves less than the drain that would
// clear it can need is a room no update will make converge, and the browser is closed
// DOC_NEEDS_REBUILD (refusedStuckBacklog).
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
// against.
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
// total.
//
// An update over unslottedApplyBytes takes what its apply costs out of the pod's load
// concurrency before it applies (loadSlots, copyWeight), so a reconnect storm's whole-state
// handoffs decode as many at a time as the pool holds rather than all at once; typing never
// waits on it.
//
// A room with no measurement at all is a document that left the Proof schema while it was live,
// which the socket path refuses to open in the first place (refuseOutsideSchema), or one no load
// listed: the byte bound weighs its updates on their own bytes, as if the room rendered nothing,
// and the creep cap, which needs a rendering to measure, never applies to it.
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
// A successful apply charges nothing here: its report has already added what it added to the
// room's encoded state to the room's tracked state as it committed (roomApplies.noteApply), and
// the room's update observer, draining the update inside the apply that committed it, weighs
// the room again from that figure (renderedReplica.chargeAdded, roomCharge).
func (s *Service) weighPeerUpdate(info websocket.PeerUpdateInfo, apply func() error) error {
	weighing := s.roomWeighingOf(info.Doc)
	if refusal := s.refusedBeforeApply(info, weighing); refusal != nil {
		return refusal
	}
	// The structs the room holds, read once: what is left of the decode cap above them is both
	// the apply's allowance of structs and splits and the headroom a drain of the room's
	// backlog would have to come out of.
	//
	// It is the figure the room's last committed apply reported (roomApplies.structsHeld,
	// crdt.ApplyReport.Structs), and a reading of the document only for a room's first update
	// after a load, which no apply has reported on yet. The report counts the pending queue
	// beside the store, which is what this hook has to weigh: a frame whose structs park
	// behind a clock gap adds nothing the store's own count sees, so that count left the whole
	// queue outside the cap, and two frames of 448,568 structs each parked behind a gap were
	// admitted over a room of 600,000 structs whose count read 600,000 throughout, leaving a
	// stored state of 2,994,565 bytes holding 897,136 parked structs that no longer decoded
	// (TestR6Finding2ParkedStructsEscapeTheDecodeCap, dispatch://INFRA-179, review 6's
	// finding 2). The library counts both as each apply commits, so the room is weighed on
	// what it holds rather than on what this package could read of it between applies.
	structs := weighing.applies().structsHeld(info.Doc)
	if refusal := s.refusedStuckBacklog(info, weighing, structs); refusal != nil {
		return refusal
	}
	rendering, measured := weighing.size()
	// The pressure is read once for this update: whether the open rooms are past their budget,
	// and what the room's own growth under that pressure has left it to cut with
	// (splitsUnderPressure), which the mark that reading makes is what bounds.
	full := s.underPressure(info.Room)
	allowance := peerApplyAllowance(int64(len(info.Update)), rendering, weighing.roomStateBytes(),
		structs, measured, peerPressure{full: full, splits: weighing.applies().splitsUnderPressure(structs, full)})
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
	// The scan allowance is reserved last, so no refusal above leaves one in flight, and settled
	// as soon as the apply returns, with what its scans passed.
	scans := weighing.roomScans()
	scanning := scans.reserve(info.Origin, scanCeiling(len(info.Update)))
	err := weighing.applies().applying(info.Origin, allowance.budget, apply)
	scans.settle(scanning, err)
	if err == nil {
		return nil
	}
	return s.refusedByApply(info, allowance, scanning, err)
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

// refusedBeforeApply is the one refusal the hook makes without applying: an update carrying the
// history its document's rebuild replaced (weighPeerUpdate). A room whose history retired no
// client reads nothing of the update. It carries the test hook's armed refusal too
// (refusesNextPeerUpdateForTest), which stands for a refusal of the rules below it.
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

// refusedStuckBacklog is the second refusal the hook makes without applying: a browser's update
// into a room whose pending queue has stopped falling and which the room can no longer drain.
//
// ygo parks a struct that arrives before the struct it is written against and drains the queue
// inside the next apply that fills the gap. From sjawhar/ygo v1.55.1-sami.8 the splits that drain
// makes are charged to the apply that lands them, and a drain whose splits pass Split is refused
// and taken back while that apply stands: the parked content is another sender's, already in this
// room's queue, and the sender that filled the gap can neither see it nor send less of it, so
// refusing that sender would refuse every sender of that clock, for ever (crdt.ApplyBudget.Split).
// The library therefore leaves the room holding a backlog it will not drain, tells no caller, and
// names what an owner watches instead: a pending queue that stops falling (crdt.Doc.PendingStats).
//
// This is that reading, and it is of the room's own state rather than of anything the hook
// counts: the queue holds something, it holds no fewer than the fewest this room has seen since
// it was last empty (backlogStalled), and the splits the drain that would clear it can make -
// one for each parked struct, which cuts the item it lands inside, and two for each parked
// delete range, which cuts the items its two edges fall in - are more than the apply that fills
// the gap may make. That figure is the splits' half of what the decode cap leaves above the
// room's own structs (peerApplyAllowance), the field the drain is charged against, rather than
// the whole headroom: the other half is the structs' and no drain can be paid out of it.
// The third test is what tells a stuck room from a room waiting for frames a browser has not
// sent yet: with headroom above that, the drain cannot be refused for its splits at all, so a
// queue that is not falling is one whose gap has not been filled, and the room is left alone.
//
// The close is DOC_NEEDS_REBUILD, the one close that offers the person the remedy that works.
// Nothing else is one: the backlog is stored as it parks and parked again by every later load
// (storeParked), so no reconnect, no eviction and no restart of the pod clears it, while a
// rebuild writes a history without it. A room this reads wrongly - one whose decode cap leaves
// that little and whose queue is waiting on a browser that will yet send - is a room whose next
// insert is refused with the same close for the cap alone (peerRule.refusal).
//
// A room no load listed keeps no floor and is read for nothing, as it is bounded by nothing else
// here. The reading itself is a read lock on the document and a handful of fields, and allocates
// nothing at all for the room with an empty queue, which is every room that is converging
// (crdt.Doc.PendingStats).
func (s *Service) refusedStuckBacklog(info websocket.PeerUpdateInfo, weighing *roomWeighing, structs uint64) error {
	applies := weighing.applies()
	if applies == nil {
		return nil
	}
	parked := info.Doc.PendingStats()
	if !applies.backlogStalled(parked.Items + parked.DeleteRanges) {
		return nil
	}
	splits := parked.Items + 2*parked.DeleteRanges
	left := capHeadroom(structs) / 2
	if splits <= left {
		return nil
	}
	refusal := documentNeedsRebuild()
	logRefusal(info, refusal, "dispatch: refused a browser's update into a room holding a backlog no update will drain",
		"rule", "the room's pending queue has stopped falling and the splits of its drain are past what the decode cap leaves it",
		"parked_structs", parked.Items, "parked_ranges", parked.DeleteRanges,
		"drain_splits", splits, "split_limit", left,
		"structs", structs, "struct_rule_limit", maxUpdateItems)
	return refusal
}

// refusedByApply is the close a failed apply of a browser's update becomes.
//
// An apply the allowance refused (crdt.ErrApplyBudget) reports what it reached of each of the
// three counts, and the first of them at or past its own field names the rule that set that
// field, which names the close: the decode count is every origin's and is DOC_TOO_LARGE, and the
// structs and the bytes carry whichever rule set them (peerRule). One ygo refused for a root the
// browser may not name (crdt.ErrRootNotAllowed) is DOC_UPDATE_REFUSED: the editor never sends
// one. One whose merge would scan past its allowance (crdt.ErrIntegrateScanBudget) is
// DOC_TOO_TANGLED, or DOC_NEEDS_REBUILD where the room's scan total was used up. One the room's
// pending cap refused (crdt.ErrPendingCap) is returned as it is, the one error the hook answers
// that is no *websocket.PeerClose, so ygo answers that browser SyncStatus(0) and leaves it
// connected, exactly as a room without this hook does: the update parks past the cap only
// because the structs it depends on have not arrived, and the browser that sends it again once
// its gap fills is right to stay. Any other refusal of the bytes themselves
// (crdt.ErrInvalidUpdate) is DOC_UPDATE_REFUSED: ygo will not read them, and the client that
// sent them sends them again on every reconnect.
func (s *Service) refusedByApply(info websocket.PeerUpdateInfo, allowance peerAllowance, scanning *scanAllowance, err error) error {
	var refused *crdt.RefusedUpdateError
	errors.As(err, &refused)
	switch {
	case errors.Is(err, crdt.ErrApplyBudget):
		rule := allowance.rule(refused)
		refusal := rule.refusal()
		logRefusal(info, refusal, "dispatch: refused a browser's update its apply may not take", "rule", rule.why(),
			"decoded", refused.Decoded, "decode_limit", allowance.budget.Decoded,
			"structs", refused.Structs, "structs_limit", allowance.budget.Structs, "struct_rule_limit", allowance.structsLimit,
			"content_bytes", refused.Bytes, "content_limit", allowance.budget.Bytes, "byte_rule_limit", allowance.bytesLimit,
			"split", refused.Split, "split_limit", allowance.budget.Split)
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
		weighing := s.roomWeighingOf(info.Doc)
		scanned, left := weighing.roomScans().figures()
		slog.Warn("dispatch: refused a browser's update its room could not merge within its scan allowance",
			"room", info.Room, "update_bytes", len(info.Update), "charged", used, "limit", limit,
			"structs", weighing.applies().structsHeld(info.Doc), "allowance", scanning.limit(),
			"scan_total", scanned, "scans_left", left)
		if scanning.roomUsedUp() {
			return documentNeedsRebuild()
		}
		return documentTooTangled()
	case errors.Is(err, crdt.ErrPendingCap):
		// No line: a browser whose gap has not filled meets this on every frame it sends until
		// the gap does fill, and the socket stays open, so one line here would be a flood where
		// every other refusal here closes the socket and so logs at most once.
		return err
	case errors.Is(err, crdt.ErrInvalidUpdate):
		refusal := documentUpdateRefused()
		logRefusal(info, refusal, "dispatch: refused a browser's update ygo will not read", "error", err.Error())
		return refusedWith(refusal, err)
	}
	return err
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
// after that point (crdt.ApplyBudget.Split). It names the rule that set Structs, because Split
// comes from that same rule - it is the splits' half of the decode cap's headroom, or what the
// room's growth under the pressure has left it, pressureSplits at most (splitsUnderPressure) -
// and that rule's own figure is what the refusal's line carries for both.
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
		return a.structs
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

// documentTooTangled is the refusal of an update the room's document could not merge within its
// scan allowance while the room's scan total was not used up (roomScans.reserve), permanent as
// documentTooLarge is and under the same code, with a reason of its own so the editor can say why
// the edit was dropped.
func documentTooTangled() *websocket.PeerClose {
	return &websocket.PeerClose{
		Code:   contracts.DocumentTooLargeCloseCode,
		Reason: contracts.DocumentTooTangledCloseReason,
	}
}

// documentNeedsRebuild is the refusal of an update the document cannot take until it is rebuilt -
// one its room's used-up scan total refuses, and one that would take its state past
// maxRoomStateBytes - permanent as documentTooLarge is and under the same code, with a reason of
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
