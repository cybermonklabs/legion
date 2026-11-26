package replay

import (
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// Cause is the word of a send-back loop's cause: why a move took an issue back to an earlier phase
// of phase.Order. The trace names each move back's cause from the record (CauseOf), and the
// measures count the rounds and hours of the loops by this word (Scorecard.Loops), so a window's
// record and every replay of it name a loop's cause alike.
type Cause string

const (
	// CauseReviewChanges is a review that asks for changes: reviewing back to implementing by
	// review_rejected.
	CauseReviewChanges Cause = "review_changes"
	// CauseSendBack is a send-back by the architect or the merger: a backward move out of
	// accepting, retro or merging that no drop from the merge queue came before.
	CauseSendBack Cause = "send_back"
	// CauseConflict is a head that conflicts with its base, moved back to implementing by the
	// conflict trigger (the rule legion-v20.42.5, and the releases at 52b3cf5c and 958173da it comes
	// from).
	CauseConflict Cause = "conflict"
	// CauseRedChecksOrConflict is red checks or a conflict, which the release at 9542c6fb did
	// not tell apart: its checks_red moved an issue back to implementing from any phase
	// (assumptions 21 and 22).
	CauseRedChecksOrConflict Cause = "red_checks_or_conflict"
	// CauseTestFailed is a failed test: testing back to implementing by tester_failed.
	CauseTestFailed Cause = "test_failed"
	// CauseQueueDrop is a drop from the merge queue that sends the work back: a backward move out
	// of merging after a line of the issue's pull request leaving the merge queue.
	CauseQueueDrop Cause = "queue_drop"
	// CauseOtherBackward is a worker's own backward move before the review: out of planning,
	// implementing, testing or reviewing.
	CauseOtherBackward Cause = "other_backward"
)

// Causes is every cause word, in the order a report lists them: the one vocabulary the trace
// names a loop by and the measures and the forecast read.
var Causes = []Cause{
	CauseReviewChanges,
	CauseSendBack,
	CauseConflict,
	CauseRedChecksOrConflict,
	CauseTestFailed,
	CauseQueueDrop,
	CauseOtherBackward,
}

// ForecastGroup names how a forecast across a recorded change of the workflow scores the loops of
// a cause. The two releases on either side of the change do not define every cause alike, so a
// cause the change redefines is compared only inside the total of all send-back loops, and a cause
// both releases define the same way is compared apart.
type ForecastGroup string

const (
	// GroupRedefined is the causes the workflow change redefines: the release at 9542c6fb's red
	// checks or a conflict, and the later releases' conflict and send-back by the architect or
	// the merger. A forecast scores them only inside the total of all send-back loops.
	GroupRedefined ForecastGroup = "redefined"
	// GroupSame is the causes both releases define the same way: a review that asks for changes,
	// a failed test, a drop from the merge queue and a worker's own backward move before the
	// review, which neither release routes through red checks or a conflict. A forecast scores
	// each of them apart.
	GroupSame ForecastGroup = "same"
)

// ForecastGroups is the causes of each forecast group. Every cause stands in one of the two.
var ForecastGroups = map[ForecastGroup][]Cause{
	GroupRedefined: {CauseRedChecksOrConflict, CauseConflict, CauseSendBack},
	GroupSame:      {CauseReviewChanges, CauseTestFailed, CauseQueueDrop, CauseOtherBackward},
}

// SendBack is one move back as the trace reads it from the record: the move's trigger, the phase
// it left and the phase it entered, read on today's phases (a phase MoveSources folds is read as
// the phase it folds into), and Drop, the msg of the line of the issue's pull request leaving the
// merge queue that came before the move in the phase it left, or "" where none did.
type SendBack struct {
	Trigger  workflow.TriggerKind
	From, To phase.Phase
	Drop     string
}

// CauseOf is the cause of move, and false for a move it names no cause for: a review's
// rejection is review_changes and a failed test test_failed; a conflict move is conflict and a
// checks_red move red_checks_or_conflict; a backward move out of merging after a drop from the
// merge queue is queue_drop, any other backward move out of accepting, retro or merging is
// send_back, and one out of an earlier phase other_backward.
func CauseOf(move SendBack) (Cause, bool) {
	switch move.Trigger {
	case workflow.TriggerReviewRejected:
		return CauseReviewChanges, true
	case workflow.TriggerTesterFailed:
		return CauseTestFailed, true
	case workflow.TriggerConflict:
		return CauseConflict, true
	case workflow.TriggerChecksRed:
		return CauseRedChecksOrConflict, true
	case workflow.TriggerBackward:
		switch move.From {
		case phase.Merging:
			if move.Drop != "" {
				return CauseQueueDrop, true
			}
			return CauseSendBack, true
		case phase.Accepting, phase.Retro:
			return CauseSendBack, true
		case phase.Planning, phase.Implementing, phase.Testing, phase.Reviewing:
			return CauseOtherBackward, true
		}
	}
	return "", false
}
