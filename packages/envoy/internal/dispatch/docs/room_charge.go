package docs

import (
	"log/slog"

	"github.com/reearth/ygo/crdt"
)

// This file holds one loaded room's charge against the budget (roomCharge): the one weight it
// carries, the two writes that move it - the drain that adds each committed update's bytes
// to the room's tracked state, and the re-base that replaces the tracked state with a figure
// read from bytes already in hand - and the anchor the two paths that carry no report weigh
// their own reading of the room against (anchorStructs).

// roomCharge is one loaded room's charge against the budget: one weight, the larger of its
// tracked state at bytesOfHeapAStateByte and its structs at bytesOfHeapAStruct in each of its
// two copies (roomWeight), and the room it belongs to.
//
// Its only writer is the room's update observer, whose drain of the room's update log adds each
// committed update's bytes to the room's tracked state and weighs the room again from the sum
// (renderedReplica.chargeAdded), whoever wrote the update: a browser's update the peer hook
// admitted, an API write's publish, an injected update, settlement's repair. The peer hook
// charges nothing of its own: an update it admits is drained by that observer inside the apply
// that committed it (crdt/doc.go:984-992), and one its apply refused committed nothing to drain.
//
// The charge lasts as long as the room's instance. Its load makes it, from the stored state the
// load decoded (chargeLoadedRoom); each drain raises it; a compaction that folds the room's
// whole stored history into one row re-bases it on what that fold wrote
// (Service.rebaseRoomState); and the room's retirement drops it whole (roomBudget.retire).
type roomCharge struct {
	budget *roomBudget
	room   string
	// weight is what the budget charges the room now, both copies together (roomWeight), and
	// retired says the room's instance has gone, after which nothing is charged again.
	weight  int64
	retired bool
	// closeOnceUnused is set on a room loaded while the budget was over its limit, which the
	// server's own work opened over it; it is closed once unused
	// (Service.closeIfOpenedOverBudget) unless a browser joins it first (join).
	closeOnceUnused bool
}

// weigh charges the room what its tracked state of stateBytes bytes and its structs structs come
// to (roomWeight) and re-weighs its entry under the budget's lock. A nil charge, a replica no
// room's load charged (a test's), charges nothing.
func (c *roomCharge) weigh(stateBytes, structs int64) {
	if c == nil {
		return
	}
	c.budget.mu.Lock()
	defer c.budget.unlock()
	c.budget.reweighLocked(c, roomWeight(stateBytes, structs))
}

// anchorStructs is how many structs live holds, read from the document itself, for the two
// paths that weigh a room with no report to read: the room's load from its stored state, whose
// decode runs before the document is given the report at all, and a compaction's re-base,
// which is no apply (chargeLoadedRoom, Service.rebaseRoomState). Every other reader takes the
// figure the room's last committed apply reported (roomApplies.structsHeld).
//
// Those two readings are the anchor on the reports. Each is of the same quantity a report
// carries - the store's structs with the pending queue's beside them, each counted once
// (heldStructs, crdt.ApplyReport.Structs) - so where the room already holds a reported figure
// the two can be held against each other, and a difference is logged with the room and both
// figures. The reading is then taken as the truth: it is of the document, where a report is of
// the document as some earlier apply left it, and only a reading sees what no remote apply
// made - a server write in the room's own transaction, which reports nothing.
//
// It logs at Warn and refuses nothing. A difference is an accounting fault, not a document
// fault: the room is charged and bounded from the reading either way, and what the line is for
// is an operator or a drive that can then say which path left the figure behind
// (dispatch://INFRA-179).
func anchorStructs(room string, applies *roomApplies, live *crdt.Doc) uint64 {
	read := heldStructs(live)
	if reported, ok := applies.reportedStructs(); ok && reported != read {
		slog.Warn("dispatch: a room's reported structs differ from the count read at the anchor",
			"room", room, "reported", reported, "read", read)
	}
	return read
}
