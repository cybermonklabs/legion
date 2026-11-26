package record

// The record of an admission pass that moves a slot: one structured log line per pass that gives
// out or takes back at least one slot (admit's promoteHolds), written once the pass's fact
// commits, naming the slots in use after the pass, the cap, each slot given out, each slot taken
// back and each candidate of the line the pass passed over. A replay of a recorded window reads
// the daemon's own decisions back from it, where the window holds it, rather than rebuilding them
// from the per-slot lines. The types below are the line's three lists, each element a JSON object
// whose fields are named issue, kind, reason and place.

// SlotGiven is one slot a pass gave out: the issue that took it and the kind of entry of the
// waiting line it stood as (LineKind.String()), which says what its promotion was.
type SlotGiven struct {
	Issue string `json:"issue"`
	Kind  string `json:"kind"`
}

// SlotTaken is one slot a pass took back: the issue that held it and the path of the pass that
// took it.
type SlotTaken struct {
	Issue  string          `json:"issue"`
	Reason SlotTakenReason `json:"reason"`
}

// SlotPassed is one candidate of the line the promotion passed over: its issue, the kind of entry
// it stood as (LineKind.String()), its place in the line the pass read, counted from 1, and why
// the promotion went past it.
type SlotPassed struct {
	Issue  string           `json:"issue"`
	Kind   string           `json:"kind"`
	Place  int              `json:"place"`
	Reason SlotPassedReason `json:"reason"`
}

// SlotTakenReason is the path of a pass that took a slot back, the word SlotTaken carries.
type SlotTakenReason string

const (
	// TakenPastReview is the release of an issue whose phase keeps no slot: its work stands past
	// its review (NeedsSlot, admit's releaseSlots).
	TakenPastReview SlotTakenReason = "past_review"
	// TakenLinkWait is the release for a wait of a started issue that waits on an unfinished
	// Dispatch blocked_by link (admit's releaseSlotsForWaits, and a cover that gives the waiter's
	// slot to the issue it waits on, admit's coverReason).
	TakenLinkWait SlotTakenReason = "link_wait"
	// TakenChildrenWait is the release for a wait of a started parent that waits on its open
	// Dispatch children (admit's releaseSlotsForWaits, and a cover that gives the parent's slot to
	// one of those children, admit's coverReason).
	TakenChildrenWait SlotTakenReason = "children_wait"
	// TakenOutOfWork is the release of an issue out of the workflow: done, a Dispatch status the
	// workflow does not run, or a tree that no longer runs (NeedsSlot, admit's releaseSlots).
	TakenOutOfWork SlotTakenReason = "out_of_work"
	// TakenTreeLingers is the release of an issue whose tree's root lingers after its close
	// (NeedsSlot, admit's releaseSlots).
	TakenTreeLingers SlotTakenReason = "tree_lingers"
	// TakenIdleReturn is the release, under a rule with an idle return (SlotRule.IdleReturn), of a
	// holder that waits on its children or on a person and none of whose roles had a turn for that
	// long (LineFacts.IdleReturns, admit's releaseIdleSlots).
	TakenIdleReturn SlotTakenReason = "idle_return"
)

// SlotPassedReason is why the promotion passed over a candidate, the word SlotPassed carries.
type SlotPassedReason string

const (
	// PassedCap is a candidate that needs a slot while the cap has none free (admit's capLets).
	PassedCap SlotPassedReason = "cap"
	// PassedWaits is a candidate that waits on another issue (blockers.LineWaitsOn).
	PassedWaits SlotPassedReason = "waits"
	// PassedHeldKey is the candidate a hold stopped the pass at: one the Dispatch consumer has
	// still to catch up on, or one the boot's catch-up still holds.
	PassedHeldKey SlotPassedReason = "held_key"
	// PassedTreeCleanup is a queued root whose tree's cleanup, or another authority over its
	// tree, has still to end before the tree can open.
	PassedTreeCleanup SlotPassedReason = "tree_cleanup"
	// PassedNotResumable is an answered root that may not resume in its own session
	// (workflow.Unresumable).
	PassedNotResumable SlotPassedReason = "not_resumable"
	// PassedSignal is a candidate below P0 that a signal with a limit holds: admission by signals
	// (SlotRule.SignalsLet) lets no start through while a signal binds (admit's capLets).
	PassedSignal SlotPassedReason = "signal"
	// PassedTick is a candidate below P0 that a tick's pass leaves for an event's or a signal's
	// deadline: admission by signals starts nothing below P0 at the tick (SlotRule.SignalsLet).
	PassedTick SlotPassedReason = "tick"
)
