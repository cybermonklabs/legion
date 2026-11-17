package workflow

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Only the conflict move posts a reason on the issue. transition also carries a reason for a
// review that requests changes (the review's whole body) and for a worker's backward move (its own
// words), and validateOutboxPayload takes a reason on any ordinary row, so this guard is the one
// thing that keeps those texts off the issue: a tester's verdict, a review and a backward move are
// already on Dispatch as the worker's own act. The control is the guard removed, which makes every
// other trigger here post a sentence.
func TestOnlyTheConflictMovePostsAReasonOnTheIssue(t *testing.T) {
	pr := &record.PullRequest{Number: 42, HeadSHA: "0123456789abcdef0123", Base: "main"}
	for _, trigger := range allWorkflowTriggers {
		got := moveReason(trigger, phase.Testing, phase.Implementing, pr)
		if trigger == TriggerConflict {
			want := "Legion moved this issue from testing back to implementing, because pull request #42 at head 0123456789ab conflicts with main. " +
				"GitHub runs no checks on a head that conflicts with its base, so the implementer merges main forward."
			if got != want {
				t.Fatalf("the conflict move posts %q, want %q", got, want)
			}
			continue
		}
		if got != "" {
			t.Fatalf("a %s move posts %q on the issue, want nothing: the worker's own act already records it", trigger, got)
		}
	}
}

// A conflict move whose pull request record is missing, or carries no number, head or base, still
// posts a whole sentence, naming what it does not know in general words.
func TestAConflictMoveWithoutItsPullRequestStillSaysWhy(t *testing.T) {
	want := "Legion moved this issue from merging back to implementing, because its pull request conflicts with its base. " +
		"GitHub runs no checks on a head that conflicts with its base, so the implementer merges its base forward."
	for _, pr := range []*record.PullRequest{nil, {}} {
		if got := moveReason(TriggerConflict, phase.Merging, phase.Implementing, pr); got != want {
			t.Fatalf("pull request %+v: the conflict move posts %q, want %q", pr, got, want)
		}
	}
}
