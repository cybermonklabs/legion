package workflow

import (
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

var allWorkflowTriggers = []TriggerKind{
	TriggerGateOpened, TriggerPlannerCompleted, TriggerImplementationReady, TriggerTesterPassed,
	TriggerTesterFailed, TriggerReviewApproved, TriggerReviewRejected, TriggerRetroCompleted,
	TriggerPullRequestMerged, TriggerSignOff, TriggerBackward,
}

func TestEveryPhaseAndTriggerHasARowOrNamedIgnore(t *testing.T) {
	if err := validateTable(Table); err != nil {
		t.Fatal(err)
	}
}

func TestDeletingAnyTransitionRowBreaksCoverage(t *testing.T) {
	for index := range Table {
		rows := append([]Row{}, Table[:index]...)
		rows = append(rows, Table[index+1:]...)
		if err := validateTable(rows); err == nil {
			t.Fatalf("deleting %s/%s -> %s left the table covered", Table[index].From, Table[index].Trigger, Table[index].To)
		}
	}
}

// The daemon counts a round for every row that moves an issue back (Engine.transition reads
// phase.MovesBack), and the config loader refuses a hook written for a pair outside
// phase.BackwardPairs. The two must name the same moves: a row added here and not there would be
// a move the daemon counts and the loader refuses, and a pair added there and not here a hook
// that never runs.
func TestBackwardRowsMatchPhaseBackwardPairs(t *testing.T) {
	fromTable := map[string]bool{}
	fromTrigger := map[string]bool{}
	for _, row := range Table {
		if !phase.MovesBack(row.From, row.To) {
			continue
		}
		fromTable[phase.Pair(row.From, row.To)] = true
		if row.Trigger == TriggerBackward {
			fromTrigger[phase.Pair(row.From, row.To)] = true
		}
	}
	fromPhase := map[string]bool{}
	for _, pair := range phase.BackwardPairs() {
		fromPhase[pair] = true
	}
	if !maps.Equal(fromTable, fromPhase) {
		t.Fatalf("table backward moves = %v, phase.BackwardPairs = %v", sortedPairs(fromTable), sortedPairs(fromPhase))
	}
	// The backward trigger carries every pair: a worker may ask for any move back the phases
	// allow, and a move back another trigger makes (a tester's fail, a review's request for
	// changes) is over a pair the backward trigger carries too, never one it is missing.
	if !maps.Equal(fromTrigger, fromPhase) {
		t.Fatalf("%s rows = %v, want %v", TriggerBackward, sortedPairs(fromTrigger), sortedPairs(fromPhase))
	}
}

func sortedPairs(pairs map[string]bool) []string {
	return slices.Sorted(maps.Keys(pairs))
}

func validateTable(rows []Row) error {
	for _, current := range phase.All {
		for _, trigger := range allWorkflowTriggers {
			hasRow := false
			for _, row := range rows {
				if row.From != current || row.Trigger != trigger {
					continue
				}
				hasRow = true
				if row.To == "" || row.Guard == nil || len(row.Effects) == 0 {
					return fmt.Errorf("%s/%s is not a complete transition row", current, trigger)
				}
			}
			if workflowTransitionRequired(current, trigger) {
				if !hasRow {
					return fmt.Errorf("%s/%s is missing a transition row", current, trigger)
				}
				continue
			}
			if hasRow {
				return fmt.Errorf("%s/%s has a row where %q says it is ignored", current, trigger, namedIgnore(current, trigger))
			}
			if namedIgnore(current, trigger) == "" {
				return fmt.Errorf("%s/%s has neither a row nor a named ignore", current, trigger)
			}
		}
	}
	for _, from := range phase.All {
		for _, to := range requiredBackwardTargets(from) {
			if !hasBackwardRow(rows, from, to) {
				return fmt.Errorf("%s/backward -> %s is missing a transition row", from, to)
			}
		}
	}
	for _, row := range rows {
		if !workflowTransitionRequired(row.From, row.Trigger) {
			return fmt.Errorf("%s/%s is not a declared transition", row.From, row.Trigger)
		}
		if row.Trigger == TriggerBackward && !containsPhase(requiredBackwardTargets(row.From), row.To) {
			return fmt.Errorf("%s/backward -> %s is not a declared backward edge", row.From, row.To)
		}
	}
	return nil
}

func hasBackwardRow(rows []Row, from, to phase.Phase) bool {
	for _, row := range rows {
		if row.From == from && row.Trigger == TriggerBackward && row.To == to {
			return true
		}
	}
	return false
}

func workflowTransitionRequired(from phase.Phase, trigger TriggerKind) bool {
	switch trigger {
	case TriggerGateOpened:
		return from == phase.Admitted
	case TriggerPlannerCompleted:
		return from == phase.Planning
	case TriggerImplementationReady:
		return from == phase.Implementing
	case TriggerTesterPassed, TriggerTesterFailed:
		return from == phase.Testing
	case TriggerReviewApproved, TriggerReviewRejected:
		return from == phase.Reviewing
	case TriggerRetroCompleted:
		return from == phase.Retro
	case TriggerPullRequestMerged:
		return from == phase.Merging
	case TriggerSignOff:
		return from == phase.Accepting
	case TriggerBackward:
		return len(requiredBackwardTargets(from)) != 0
	default:
		return false
	}
}

func requiredBackwardTargets(from phase.Phase) []phase.Phase {
	ordered := []phase.Phase{phase.Planning, phase.Implementing, phase.Testing, phase.Reviewing, phase.Accepting, phase.Retro, phase.Merging}
	for index, candidate := range ordered {
		if candidate == from {
			return ordered[:index]
		}
	}
	return nil
}

func containsPhase(phases []phase.Phase, wanted phase.Phase) bool {
	for _, candidate := range phases {
		if candidate == wanted {
			return true
		}
	}
	return false
}

func namedIgnore(current phase.Phase, trigger TriggerKind) string {
	if current == phase.AwaitingChildren {
		return awaitingChildrenIgnores[trigger]
	}
	return fmt.Sprintf("%s does not advance %s", trigger, current)
}

// awaitingChildrenIgnores is why each trigger leaves a parent in awaiting_children where it stands
// (dispatch://INFRA-3320 R9 to R12): the parent goes back to the start it held only at the
// admission pass that finds no open child under it, so the table has no row out of the phase, and
// a fact that would move a phase is recorded where it lands and starts nothing.
var awaitingChildrenIgnores = map[TriggerKind]string{
	TriggerGateOpened:          "a parent in awaiting_children has started its phases, so no gate's opening starts its first",
	TriggerPlannerCompleted:    "the planner's completion of a phase the wait stopped is refused, HANDOFF_NOT_CURRENT_PHASE",
	TriggerImplementationReady: "the implementer's completion of a phase the wait stopped is refused, HANDOFF_NOT_CURRENT_PHASE; a push of its moves the head and nothing else",
	TriggerTesterPassed:        "the tester's completion of a phase the wait stopped is refused, HANDOFF_NOT_CURRENT_PHASE",
	TriggerTesterFailed:        "the tester's completion of a phase the wait stopped is refused, HANDOFF_NOT_CURRENT_PHASE",
	TriggerReviewApproved:      "a review GitHub records during the wait is recorded and starts no phase",
	TriggerReviewRejected:      "a review GitHub records during the wait is recorded and starts no phase",
	TriggerRetroCompleted:      "no retro runs before the parent is accepted, after its children",
	TriggerPullRequestMerged:   "a merge during the wait is recorded on the pull request and read at the return",
	TriggerSignOff:             "sign_off on a parent in awaiting_children is refused, WAITING_ON_CHILDREN",
	TriggerBackward:            "request_backward_move on a parent in awaiting_children is refused, WAITING_ON_CHILDREN",
}

// Every trigger reaching a parent in awaiting_children is a named ignore: no row of the table
// leaves the phase, and validateTable (TestEveryPhaseAndTriggerHasARowOrNamedIgnore) holds each
// trigger there to its reason in awaitingChildrenIgnores. The two triggers of one act, a tester's
// pass and failure or a review's approval and rejection, share one reason.
func TestEveryTriggerInAwaitingChildrenIsANamedIgnore(t *testing.T) {
	for _, row := range Table {
		if row.From == phase.AwaitingChildren {
			t.Errorf("the table holds a row out of awaiting_children (%s -> %s), want none: only the admission pass's return leaves it", row.Trigger, row.To)
		}
	}
}
