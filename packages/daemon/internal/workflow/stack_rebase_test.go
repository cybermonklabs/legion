package workflow

import (
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// GitHub's server-side "Rebase stack", which it also runs by itself when the pull request below
// in a stack merges, replaces the head of each pull request above with new commits and, where
// the bottom one merged, retargets its base; it keeps the approvals when the diff does not change
// (docs.github.com/en/pull-requests/reference/stacked-pull-requests, "Rebasing a stack"). Once
// the issue is past its review, that move sends nothing back: the head is recorded, the phase
// stays, no review round or pass is counted, no role is started or told and the architect is
// told nothing. The move reaches the daemon as a synchronize whose push may arrive before it,
// after it, or never. The daemon then reads GitHub's merge state of the new head, as it does
// for any moved head, and a clean answer moves nothing either; in merging, the merge GitHub then
// reports at the rebased head closes the issue.
func TestAStackRebasePastTheReviewSendsNothingBack(t *testing.T) {
	t.Parallel()
	rebase := intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208",
		Before: "head", Sender: "web-flow", HeadSHA: "rebased", Base: "main"}
	for _, tc := range []struct {
		name  string
		facts []intake.Fact
	}{
		{"the synchronize alone", []intake.Fact{rebase}},
		{"the push, then the synchronize", []intake.Fact{pushedFromTo("head", "rebased", "web-flow"), rebase}},
		{"the synchronize, then the push", []intake.Fact{rebase, pushedFromTo("head", "rebased", "web-flow")}},
	} {
		for _, at := range []struct {
			phase  phase.Phase
			status string
		}{{phase.Accepting, "needs_review"}, {phase.Retro, "retro"}, {phase.Merging, "retro"}} {
			t.Run(tc.name+", in "+string(at.phase), func(t *testing.T) {
				pool := migratedPool(t)
				seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
					Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: at.phase, Generation: 1, Status: at.status, Rank: "U"})
				// The pull request stands on the one below it in the stack, whose merge retargets it
				// to main.
				seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
					Number: 42, Branch: "legion/LEGION-208", Base: "legion/LEGION-207", HeadSHA: "head",
					CheckedHead: "head", CheckRuns: []record.AttemptRun{{Name: acceptingCheck, ID: 1}}, Failing: []string{}})
				seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", HandoffCommit: "head"})
				seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", HandoffRecord: "review-1",
					HandoffVersion: 1, Summary: "reviewed", Decision: &record.ReviewDecision{State: "approved", Head: "head"}})
				engine := mergingEngine()
				var reads []string
				engine.cfg.ReadMergeState = func(issue record.PullRequestKey) { reads = append(reads, issue.Issue) }
				applyRefusingNothing(t, pool, engine, tc.facts...)
				if !slices.Contains(reads, "LEGION-208") {
					t.Fatalf("after the rebase the daemon asked a merge-state read of %v, want LEGION-208's", reads)
				}
				applyRefusingNothing(t, pool, engine, intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main",
					HeadSHA: "rebased", Mergeable: record.MergeabilityMergeable, MergeState: record.MergeStateClean})

				if got := recordedPR(t, pool, "LEGION-208"); got.HeadSHA != "rebased" || got.Base != "main" || got.MergeForward != nil {
					t.Fatalf("after the rebase the pull request is at %q on %q with merge forward %+v, want rebased on main and none", got.HeadSHA, got.Base, got.MergeForward)
				}
				if got := issuePhase(t, pool); got != at.phase {
					t.Fatalf("after the rebase the issue is in %s, want it left in %s", got, at.phase)
				}
				if got := phaseCounts(t, pool, "LEGION-208", claim.RoleImplementer); got.Rounds != 0 || got.ConflictRounds != 0 || len(got.BackwardMoves) != 0 {
					t.Fatalf("after the rebase the implementer's row counts %d rounds, %d conflict rounds and moves %v, want none", got.Rounds, got.ConflictRounds, got.BackwardMoves)
				}
				if got := phaseRowOf(t, pool, claim.RoleImplementer); got.Pass != 0 {
					t.Fatalf("after the rebase the implementer's row counts %d passes, want none", got.Pass)
				}
				// The rebase and the read queue nothing at all: no message to a role or to the
				// architect, no worker start, no write to Dispatch.
				if got := outboxKinds(t, pool); len(got) != 0 {
					t.Fatalf("after the rebase the outbox holds %v, want nothing", got)
				}
				if at.phase != phase.Merging {
					return
				}
				applyRefusingNothing(t, pool, engine, intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge-42", HeadSHA: "rebased"})
				if got := issuePhase(t, pool); got != phase.Done {
					t.Fatalf("after the merge at the rebased head the issue is in %s, want done", got)
				}
			})
		}
	}
}

// outboxKinds is the kind of every row the outbox holds, in the order queued.
func outboxKinds(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select kind from outbox order by id")
	if err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	kinds, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	return kinds
}
