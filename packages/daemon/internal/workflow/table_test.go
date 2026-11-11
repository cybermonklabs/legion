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
	TriggerReady, TriggerPullRequestMerged, TriggerSignOff, TriggerBackward, TriggerChecksRed,
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
	// The backward trigger carries every pair but one: awaiting_merge goes back to implementing on
	// the explicit checks_red row, because a queued pull request whose checks turn red is sent back
	// by the daemon, not by a worker asking for a move.
	delete(fromPhase, phase.Pair(phase.AwaitingMerge, phase.Implementing))
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
	case TriggerChecksRed:
		return from == phase.Testing || from == phase.Reviewing || from == phase.Accepting || from == phase.Retro || from == phase.Merging || from == phase.AwaitingMerge
	case TriggerRetroCompleted:
		return from == phase.Retro
	case TriggerReady:
		return from == phase.Merging
	case TriggerPullRequestMerged:
		return from == phase.AwaitingMerge
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
	return fmt.Sprintf("%s does not advance %s", trigger, current)
}
