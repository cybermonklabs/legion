package workflow

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// workBackRequired is the one check the pull request's base branch requires in these proofs, so
// that a settlement failing it makes the head red and the daemon moves the work back on its own.
const workBackRequired = "pr-checks-result"

// workBackEngine is the engine the work-back proofs drive: conflictRoundEngine's shape, with the
// review round cap and the hooks each proof configures, and with no merge queue, since every
// move below is made before the merge queue sees the pull request.
func workBackEngine(reviewRoundCap int, hooks ...Hook) *Engine {
	return New(record.NewStore(), Config{
		Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(), ReviewRoundCap: reviewRoundCap,
		Linger: time.Hour, ProductionCheckRole: claim.RoleTester, ReviewAppLogin: testReviewApp,
		Repositories: testRepositories, Hooks: hooks,
		Clock: func() time.Time { return time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC) },
	}, nil)
}

// seedWorkBackIssue puts the issue in implementing with an open pull request whose base branch
// requires one check, and the implementer's row the counts are kept on, which is what every move
// back below starts from.
func seedWorkBackIssue(t *testing.T, pool *pgxpool.Pool, head string) {
	t.Helper()
	standsIn(t, pool, phase.Implementing, "in_progress")
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-208", HeadSHA: head, Required: []string{workBackRequired}})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
}

// backwardMoves is the implementer row's count of the issue's moves back, each under the
// "<from>-<to>" pair it was made by (record.PhaseRow.BackwardMoves).
func backwardMoves(t *testing.T, pool *pgxpool.Pool) map[string]int {
	t.Helper()
	var encoded []byte
	if err := pool.QueryRow(context.Background(),
		"select backward_moves from phases where issue = 'LEGION-208' and role = 'implementer'").Scan(&encoded); err != nil {
		t.Fatalf("read the implementer's backward moves: %v", err)
	}
	moves := map[string]int{}
	if err := json.Unmarshal(encoded, &moves); err != nil {
		t.Fatalf("decode the implementer's backward moves %s: %v", encoded, err)
	}
	return moves
}

// testerFails is the tester's fail of the round named, which sends the work back from testing.
// The tester's row is seeded afresh each time because the transition before it cleared the
// handoff the completion writes (clearHandoff).
func testerFails(t *testing.T, pool *pgxpool.Pool, engine *Engine, round string) {
	t.Helper()
	standsIn(t, pool, phase.Testing, "testing")
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim"})
	applyRefusingNothing(t, pool, engine, intake.HandoffComplete{Generation: 1, Issue: "LEGION-208",
		Role: claim.RoleTester, Claim: "test-claim", Summary: "the fix is not in", Verdict: "fail",
		HandoffRecord: "test-" + round, HandoffVersion: 1})
}

// reviewerRequestsChanges is the review App's request for changes at head, the half that ends a
// review round the reviewer has completed, which sends the work back from reviewing.
func reviewerRequestsChanges(t *testing.T, pool *pgxpool.Pool, engine *Engine, round, head string) {
	t.Helper()
	standsIn(t, pool, phase.Reviewing, "needs_review")
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim",
		HandoffCommit: "review-" + round})
	applyRefusingNothing(t, pool, engine, intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42,
		State: "changes_requested", CommitID: head, HeadSHA: head, Author: testReviewApp})
}

// architectMovesBack is the architect's own move of the issue out of accepting, the one phase it
// holds, back to the phase named.
func architectMovesBack(t *testing.T, pool *pgxpool.Pool, engine *Engine, to phase.Phase) {
	t.Helper()
	standsIn(t, pool, phase.Accepting, "needs_review")
	applyRefusingNothing(t, pool, engine, intake.BackwardMove{Issue: "LEGION-208",
		Requester: claim.RoleArchitect, To: to, Reason: "the plan does not hold"})
}

// ciTurnsRed pushes head over the head before it and settles the required check red there, with
// the issue standing in at: the move back the daemon makes on its own, which no worker asked for.
// attempt numbers the settlement's run and its fence, so each round settles afresh.
func ciTurnsRed(t *testing.T, pool *pgxpool.Pool, engine *Engine, at phase.Phase, status, before, head string, attempt int) {
	t.Helper()
	applyRefusingNothing(t, pool, engine, syncedTo(head), pushedFromTo(before, head, "legion-implementer[bot]"))
	standsIn(t, pool, at, status)
	applyRefusingNothing(t, pool, engine, settledAt(head, workBackRequired, attempt, workBackRequired))
}

// marksConflicting is the daemon's read of GitHub that judges the pull request's own head
// conflicting with its base, the mark a conflict round is read against (classify.ConflictRound).
// It is applied outside awaiting_merge, where it records the judgement and moves nothing.
func marksConflicting(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
	t.Helper()
	applyRefusingNothing(t, pool, engine, intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42,
		Base: "main", Mergeable: record.MergeabilityConflicting})
}

// Every move back is counted under its own "<from>-<to>" pair, and the total the daemon has
// always kept rises with all of them.
//
// Four moves of different kinds are driven here, each of a different pair: a tester's fail
// (testing-implementing), the review App's request for changes (reviewing-implementing), a red CI
// verdict at the head while the architect holds the issue (accepting-implementing) and the
// architect's own move back to the plan (accepting-planning). Each pair's count is read on its
// own, so a build that kept one number for every move back reads 4 under the first pair and
// nothing under the others, and a build that counted only the moves that end in implementing
// reads nothing under accepting-planning.
//
// The fifth move is made while the daemon's record holds the head conflicting with its base. It
// raises the conflict count and no pair at all: nobody sent the work back, so the move is of no
// kind a hook or an operator counts (recordRound). The total rises on all five, because a phase
// completion's key is built from it (api/handoff.go's handoffPosition) and two passes through one
// phase that counted the same number would collide.
func TestEveryBackwardMoveIsCountedByItsPair(t *testing.T) {
	pool := migratedPool(t)
	// The cap is set above every move this proof makes: what it posts is another test's subject
	// (TestTheArchitectIsToldOncePerCapValue), and a cap reached here would say nothing about the
	// counts.
	engine := workBackEngine(50)
	seedWorkBackIssue(t, pool, "h0")

	testerFails(t, pool, engine, "1")
	reviewerRequestsChanges(t, pool, engine, "1", "h0")
	architectMovesBack(t, pool, engine, phase.Planning)
	ciTurnsRed(t, pool, engine, phase.Accepting, "needs_review", "h0", "h1", 1)

	if got := issuePhase(t, pool); got != phase.Implementing {
		t.Fatalf("after the four moves back the issue is in %s, want implementing: each of them hands the work to the implementer or the planner", got)
	}
	moves := backwardMoves(t, pool)
	for _, want := range []struct {
		pair  string
		count int
		by    string
	}{
		{pair: "testing-implementing", count: 1, by: "the tester's fail"},
		{pair: "reviewing-implementing", count: 1, by: "the review App's request for changes"},
		{pair: "accepting-implementing", count: 1, by: "the red CI verdict at the head"},
		{pair: "accepting-planning", count: 1, by: "the architect's move back to the plan"},
	} {
		if moves[want.pair] != want.count {
			t.Fatalf("the moves back counted under %s = %d, want %d, from %s; the whole count is %v",
				want.pair, moves[want.pair], want.count, want.by, moves)
		}
	}
	if rounds, conflicts := implementerRounds(t, pool), implementerConflictRounds(t, pool); rounds != 4 || conflicts != 0 {
		t.Fatalf("after the four moves back the implementer's row counts %d rounds of which %d conflict rounds, want 4 and none: every move back raises the total, and none of these was made at a conflicting head", rounds, conflicts)
	}

	// The fifth move is made while the record holds the pull request's own head conflicting with
	// its base, which is what makes it a conflict round whatever fact makes the move.
	marksConflicting(t, pool, engine)
	testerFails(t, pool, engine, "2")

	if rounds, conflicts := implementerRounds(t, pool), implementerConflictRounds(t, pool); rounds != 5 || conflicts != 1 {
		t.Fatalf("after the move made at the conflicting head the implementer's row counts %d rounds of which %d conflict rounds, want 5 and 1", rounds, conflicts)
	}
	conflicted := backwardMoves(t, pool)
	if conflicted["testing-implementing"] != 1 {
		t.Fatalf("the moves back counted under testing-implementing = %d, want the 1 the tester's own fail left: a move made at a conflicting head raises no pair, since nobody sent the work back; the whole count is %v",
			conflicted["testing-implementing"], conflicted)
	}
	counted := 0
	for _, count := range conflicted {
		counted += count
	}
	if counted != 4 {
		t.Fatalf("the pairs add up to %d moves back, want the 4 moves somebody made, of the 5 the implementer's row counts; the whole count is %v", counted, conflicted)
	}
}
