package workflow

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Only the two moves Dispatch holds no record of post a reason on the issue: the daemon's own move
// on a conflicting head and a worker's backward move. transition also carries a reason for a review
// that requests changes (the review's whole body), which the review hand-off already records, and
// validateOutboxPayload takes a reason on any ordinary row, so this switch is the one thing that
// keeps that text off the issue. Every trigger is given the same reason here, so a trigger that
// passed it through would post it. The control is the default branch removed, which makes every
// other trigger here post a sentence.
func TestOnlyTheConflictAndTheBackwardMovePostAReasonOnTheIssue(t *testing.T) {
	pr := &record.PullRequest{Number: 42, HeadSHA: "0123456789abcdef0123", Base: "main"}
	const given = "the review's whole body, which must not reach the issue a second time"
	for _, trigger := range allWorkflowTriggers {
		got := moveReason(trigger, phase.Testing, phase.Implementing, pr, given)
		switch trigger {
		case TriggerConflict:
			want := "Legion moved this issue from testing back to implementing, because pull request #42 at head 0123456789ab conflicts with main. " +
				"GitHub runs no checks on a head that conflicts with its base, so the implementer merges main forward."
			if got != want {
				t.Fatalf("the conflict move posts %q, want %q", got, want)
			}
		case TriggerBackward:
			want := "Legion moved this issue from testing back to implementing at the tester's request: " + given + "."
			if got != want {
				t.Fatalf("the backward move posts %q, want %q", got, want)
			}
		default:
			if got != "" {
				t.Fatalf("a %s move posts %q on the issue, want nothing: the act that makes it is already on Dispatch", trigger, got)
			}
		}
	}
}

// A conflict move whose pull request record is missing, or carries no number, head or base, still
// posts a whole sentence, naming what it does not know in general words.
func TestAConflictMoveWithoutItsPullRequestStillSaysWhy(t *testing.T) {
	want := "Legion moved this issue from merging back to implementing, because its pull request conflicts with its base. " +
		"GitHub runs no checks on a head that conflicts with its base, so the implementer merges its base forward."
	for _, pr := range []*record.PullRequest{nil, {}} {
		if got := moveReason(TriggerConflict, phase.Merging, phase.Implementing, pr, ""); got != want {
			t.Fatalf("pull request %+v: the conflict move posts %q, want %q", pr, got, want)
		}
	}
}

// A backward move names the role that asked, the role running the phase it left, and gives that
// role's words as they end, with one full stop added where they end in none; words that are only
// space read as none.
func TestABackwardMoveNamesWhoAskedAndGivesTheirWords(t *testing.T) {
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
		if got := moveReason(TriggerBackward, tc.from, tc.to, nil, tc.reason); got != tc.want {
			t.Fatalf("%s to %s with %q posts %q, want %q", tc.from, tc.to, tc.reason, got, tc.want)
		}
	}
}
