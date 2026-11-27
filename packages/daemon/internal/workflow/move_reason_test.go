package workflow

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// Only the one move Dispatch holds no record of posts a reason on the issue: a worker's backward
// move. transition also carries a reason for a review that requests changes (the review's whole
// body), which the review hand-off already records, and validateOutboxPayload takes a reason on
// any ordinary row, so this guard is the one thing that keeps that text off the issue. Every
// trigger is given the same reason here, so a trigger that passed it through would post it. The
// control is the guard removed, which makes every other trigger here post a sentence.
func TestOnlyTheBackwardMovePostsAReasonOnTheIssue(t *testing.T) {
	t.Parallel()
	const given = "the review's whole body, which must not reach the issue a second time"
	for _, trigger := range allWorkflowTriggers {
		got := moveReason(trigger, phase.Testing, phase.Implementing, mover{}, given)
		if trigger == TriggerBackward {
			want := "Legion moved this issue from testing back to implementing at the tester's request: " + given + "."
			if got != want {
				t.Fatalf("the backward move posts %q, want %q", got, want)
			}
			continue
		}
		if got != "" {
			t.Fatalf("a %s move posts %q on the issue, want nothing: the act that makes it is already on Dispatch", trigger, got)
		}
	}
}

// A backward move names the role that asked, the role running the phase it left, and gives that
// role's words as they end, with one full stop added where they end in none; words that are only
// space read as none.
func TestABackwardMoveNamesWhoAskedAndGivesTheirWords(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		from, to phase.Phase
		reason   string
		want     string
	}{
		{phase.Accepting, phase.Implementing, "the evidence does not cover the migration",
			"Legion moved this issue from accepting back to implementing at the architect's request: the evidence does not cover the migration."},
		{phase.Reviewing, phase.Planning, "the plan names the wrong table.",
			"Legion moved this issue from reviewing back to planning at the reviewer's request: the plan names the wrong table."},
		{phase.Merging, phase.Testing, "  \n",
			"Legion moved this issue from merging back to testing at the merger's request, which gave no reason."},
	} {
		if got := moveReason(TriggerBackward, tc.from, tc.to, mover{}, tc.reason); got != tc.want {
			t.Fatalf("%s to %s with %q posts %q, want %q", tc.from, tc.to, tc.reason, got, tc.want)
		}
	}
}
