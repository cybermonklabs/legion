package workflow

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// ownerAtReviewStep is the root LEGION-208 on the plan `implementer reviewer:owner`, with its open
// pull request #42 at head: the implementer's work is done and the task stands at step 2, the
// review step the owner does, which the move into it told the owner.
func ownerAtReviewStep(t *testing.T, pool *pgxpool.Pool, engine *Engine) record.Issue {
	t.Helper()
	root := stepRoot(t, pool, phase.Implementing)
	seedStepsAt(t, pool, root.Key, root.Generation, 1, nil, "implementer", "reviewer:owner")
	move(t, pool, engine, root.Key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
	assertWalksAt(t, pool, engine, root.Key, phase.Reviewing, stepPlace(2))
	if told := ownerSteps(t, pool, root.Key); len(told) != 1 || told[0].Role != claim.RoleReviewer {
		t.Fatalf("the move into the owner's review step told %+v, want one owner-step of the reviewer's step", told)
	}
	return root
}

// ownersReview is the owner's report of its review step 2 of key's task a, as the route
// complete_step applies it: the verdict, the step returnTo names (0 for none), the head the
// review read and the entry the owner's start raised on the reviewer's row.
func ownersReview(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, verdict string, returnTo int) intake.HandoffComplete {
	t.Helper()
	row := roleRowOf(t, pool, engine, key, claim.RoleReviewer)
	return intake.HandoffComplete{Issue: key, Role: claim.RoleReviewer, Claim: claim.Token(treeRoleToken(t, key, claim.RoleArchitect)), Generation: 1,
		Entry: row.Entry, Step: 2, Summary: "The rollback path is unnamed.", Commit: "head", Verdict: verdict, ReturnTo: returnTo,
		HandoffRecord: "review-1", HandoffVersion: 1}
}

// A review step the owner does ends by the owner's own report, with or without a review on
// GitHub: its fail returns the work to the step its returnTo names, whose agent is told the
// owner's report, and its pass starts the retro. No review on GitHub decides it: a request for
// changes from an account with write access, which settles a reviewer's round on its own, moves
// nothing at the owner's step and tells nothing.
//
// The controls: today's reviewing case (every completion to settleRound) leaves the owner's fail
// and pass at reviewing; reviewRound without the owner's case takes the person's request for
// changes back to step 1 while the owner's step runs.
func TestAnOwnersReviewStepEndsByItsReport(t *testing.T) {
	t.Run("the owner's fail returns the work to step 1", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := ownerAtReviewStep(t, pool, engine)
		if result := applyFacts(t, pool, engine)("fail", ownersReview(t, pool, engine, root.Key, "fail", 1)); result.Refusal != nil {
			t.Fatalf("the owner's fail was refused: %+v", result.Refusal)
		}
		assertWalksAt(t, pool, engine, root.Key, phase.Implementing, stepPlace(1))
		if start := lastStartOf(t, pool, root.Key, claim.RoleImplementer); start.Phase != phase.Implementing {
			t.Fatalf("the implementer's start after the owner's fail = %+v, want one in implementing", start)
		}
	})

	t.Run("the owner's pass starts the retro", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := ownerAtReviewStep(t, pool, engine)
		if result := applyFacts(t, pool, engine)("pass", ownersReview(t, pool, engine, root.Key, "pass", 0)); result.Refusal != nil {
			t.Fatalf("the owner's pass was refused: %+v", result.Refusal)
		}
		assertWalksAt(t, pool, engine, root.Key, phase.Retro, record.TaskPlace{Kind: record.PlaceRetro})
		if start := lastStartOf(t, pool, root.Key, claim.RoleImplementer); start.Phase != phase.Retro {
			t.Fatalf("the implementer's last start = %+v, want the retro's", start)
		}
	})

	t.Run("a review on GitHub decides nothing at the owner's step", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := ownerAtReviewStep(t, pool, engine)
		notices := len(issueOutboxOf(t, pool, root.Key, string(record.OutboxKindNotice)))
		applyFacts(t, pool, engine)("person-requests-changes", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 3,
			State: "changes_requested", CommitID: "head", HeadSHA: "head", Author: "a-writer", AuthorCanWrite: true, Body: "rename it"})
		assertWalksAt(t, pool, engine, root.Key, phase.Reviewing, stepPlace(2))
		if after := len(issueOutboxOf(t, pool, root.Key, string(record.OutboxKindNotice))); after != notices {
			t.Fatalf("the person's review wrote %d notices, want none", after-notices)
		}
	})
}

// At a review step the owner does, an approval on GitHub from an account with write access still
// writes its decision on the reviewer's row, but no round waits on GitHub's merge state there
// (classify.RoundWaitsOnMergeState): the step ends by the owner's own report, and reviewRound
// answers no round there. So the review, a fact the gate owes no read for in reviewing
// (classify.MergeStateReadOwed), asks for no read of GitHub's merge state, although the approval
// stands on a head GitHub holds `blocked`. The control is the rule before this one, where the
// round of that approval waited on GitHub and the review asked for a read that nothing would ever
// settle.
func TestAnApprovalOnGitHubAtTheOwnersReviewStepAsksForNoMergeStateRead(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := ownerAtReviewStep(t, pool, engine)
	pr := recordedPR(t, pool, root.Key)
	pr.MergeState = record.MergeStateBlocked
	seedPR(t, pool, pr)
	var asked []string
	engine.cfg.ReadMergeState = func(issue string) { asked = append(asked, issue) }

	applyFacts(t, pool, engine)("person-approves", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 3,
		State: "approved", CommitID: "head", HeadSHA: "head", Author: "a-writer", AuthorCanWrite: true, Body: "ship it"})
	if row := roleRowOf(t, pool, engine, root.Key, claim.RoleReviewer); row.Decision == nil || row.Decision.State != "approved" || row.Decision.Head != "head" {
		t.Fatalf("the reviewer's row after the approval holds the decision %+v, want the approval of head: this test proves nothing without it", row.Decision)
	}
	if len(asked) != 0 {
		t.Fatalf("the approval at the owner's review step asked for %d reads of GitHub's merge state %v, want none: no round waits there", len(asked), asked)
	}
	assertWalksAt(t, pool, engine, root.Key, phase.Reviewing, stepPlace(2))
}

// A failed review step of the owner counts no review round: the owner reviewed nothing another
// agent wrote, so its fail is no round review_round_cap bounds. A reviewer's request for changes on
// GitHub that rejects its round counts one, as it always has. The control is the round counted on
// every request for changes: the owner's fail raises the count to 1.
func TestAFailedReviewStepOfTheOwnerCountsNoRound(t *testing.T) {
	t.Run("the owner's fail", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := ownerAtReviewStep(t, pool, engine)
		if result := applyFacts(t, pool, engine)("fail", ownersReview(t, pool, engine, root.Key, "fail", 1)); result.Refusal != nil {
			t.Fatalf("the owner's fail was refused: %+v", result.Refusal)
		}
		assertWalksAt(t, pool, engine, root.Key, phase.Implementing, stepPlace(1))
		if rounds := taskCounts(t, pool, engine, root.Key, root.Generation).Rounds; rounds != 0 {
			t.Fatalf("task a counts %d rounds after the owner's fail, want 0", rounds)
		}
	})

	t.Run("a reviewer's request for changes on GitHub", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Implementing)
		seedStepsAt(t, pool, root.Key, root.Generation, 1, nil, "implementer", "reviewer")
		move(t, pool, engine, root.Key, TriggerImplementationReady, record.TaskPlace{}, record.PhaseRow{})
		applyFacts(t, pool, engine)("person-requests-changes", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 3,
			State: "changes_requested", CommitID: "head", HeadSHA: "head", Author: "a-writer", AuthorCanWrite: true, Body: "rename it"})
		assertWalksAt(t, pool, engine, root.Key, phase.Implementing, stepPlace(1))
		if rounds := taskCounts(t, pool, engine, root.Key, root.Generation).Rounds; rounds != 1 {
			t.Fatalf("task a counts %d rounds after the request for changes, want 1", rounds)
		}
	})
}
