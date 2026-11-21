package admit

import (
	"context"
	"slices"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// passMoves is what one admission pass did with the cap's slots, gathered across the whole of
// promoteHolds — its releases, its grants and every round of its promotion loop — for the one
// record line the pass writes once its fact commits (logPassMoves; the line's types are
// record/pass_record.go's). A replay of a recorded window reads the daemon's decisions back from
// that line: the per-slot lines (logSlot) say which slots moved, and only this one says why and
// which candidates of the line the pass went past.
type passMoves struct {
	// slots is the slots in use after the pass's latest move: the count the per-slot line of that
	// move logged (logSlot), so the record and those lines name one count of the cap's slots.
	slots int
	// given is each slot the pass gave out, and taken each slot it took back, in the order the
	// pass moved them.
	given []record.SlotGiven
	taken []record.SlotTaken
	// passed is each candidate the last round of the promotion loop went past, in the line's
	// order. Each round reads the whole line again after the round before it promoted (the loop's
	// reread), so an earlier round's passes are verdicts on a line the later round replaced, and
	// the last round's are the pass's word on every candidate it left waiting.
	passed []record.SlotPassed
}

// give records the slot issue took as an entry of kind, after which the project held inUse.
func (m *passMoves) give(issue string, kind record.LineKind, inUse int) {
	m.given = append(m.given, record.SlotGiven{Issue: issue, Kind: kind.String()})
	m.slots = inUse
}

// take records the slot the pass took back from issue by the path reason names, after which the
// project held inUse.
func (m *passMoves) take(issue string, reason record.SlotTakenReason, inUse int) {
	m.taken = append(m.taken, record.SlotTaken{Issue: issue, Reason: reason})
	m.slots = inUse
}

// round begins a round of the promotion loop over a line read afresh: the passes of the round
// before it are dropped (passed says why).
func (m *passMoves) round() {
	m.passed = m.passed[:0]
}

// pass records the candidate the loop went past at place in the line, counted from 1, and why.
func (m *passMoves) pass(candidate record.LineEntry, place int, reason record.SlotPassedReason) {
	m.passed = append(m.passed, record.SlotPassed{Issue: candidate.Issue.Key, Kind: candidate.Kind.String(),
		Place: place, Reason: reason})
}

// logPassMoves writes the record line of a pass that gave out or took back at least one slot,
// once its fact commits: the slots in use after the pass, the cap, and the three lists. A pass
// that moved no slot writes none, whatever it passed over, and so does one whose fact rolls back,
// since its moves never happened.
func (a *Admission) logPassMoves(ctx context.Context, moves *passMoves) {
	if len(moves.given) == 0 && len(moves.taken) == 0 {
		return
	}
	slots, given, taken, passed := moves.slots, moves.given, moves.taken, moves.passed
	intake.OnCommit(ctx, func() {
		a.log.Info("admission: a pass moved slots", "slots", slots, "cap", a.cap, "given", given, "taken", taken, "passed", passed)
	})
}

// releaseReason is the path of releaseSlots that took issue's slot back, by the term of
// record.LineFacts.NeedsSlot it failed: its tree's root lingers after its close; its tree no
// longer runs, or the issue has left the workflow; or the issue stands where the pass's rule
// keeps no slot, past its review (or at the merger's ready mark under a rule that gives the slot
// back there). root is the record of its tree's root, and running whether that tree is live.
func releaseReason(issue, root record.Issue, running bool) record.SlotTakenReason {
	switch {
	case root.Lingers():
		return record.TakenTreeLingers
	case !running || !record.InWorkflow(issue):
		return record.TakenOutOfWork
	}
	return record.TakenPastReview
}

// waitReason is the path of releaseSlotsForWaits that took issue's slot back for waits, what it
// waits on (record.LineFacts.Waits): a wait on open children alone, which the issue's own first
// phase waits for (record.LineFacts.ChildWaits), or a wait on an unfinished Dispatch blocked_by
// link among them. A rule whose wait of one kind keeps the slot gives it back for the other kind
// alone (blockers.waitsOf), so its waits are of that kind whatever keys they share.
func waitReason(issue string, waits []string, facts record.LineFacts) record.SlotTakenReason {
	rule := facts.Rule.OrMain()
	switch {
	case !rule.ChildrenWaitGivesBack:
		return record.TakenLinkWait
	case !rule.LinkWaitGivesBack:
		return record.TakenChildrenWait
	}
	open := facts.ChildWaits[issue].Open
	for _, key := range waits {
		if !slices.ContainsFunc(open, func(child record.OpenChild) bool { return child.Key == key }) {
			return record.TakenLinkWait
		}
	}
	return record.TakenChildrenWait
}
