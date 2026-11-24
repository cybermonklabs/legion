package workflow

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// workBackEngine is the engine the work-back proofs drive: the review round cap and the hooks
// each proof configures, and the review App's login, since a review that requests changes is one
// of the moves back they make.
func workBackEngine(reviewRoundCap int, hooks ...Hook) *Engine {
	return New(record.NewStore(), Config{
		Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(), ReviewRoundCap: reviewRoundCap,
		Linger: time.Hour, ProductionCheckRole: claim.RoleTester, ReviewAppLogin: testReviewApp,
		Repositories: testRepositories, Hooks: hooks,
		Clock: func() time.Time { return time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC) },
	}, nil)
}

// seedWorkBackIssue puts the issue in implementing with an open pull request at head, and the
// implementer's row the counts are kept on, which is what every move back below starts from.
func seedWorkBackIssue(t *testing.T, pool *pgxpool.Pool, head string) {
	t.Helper()
	standsIn(t, pool, phase.Implementing, "in_progress")
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-208", HeadSHA: head})
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
// review round the reviewer has completed, which sends the work back from reviewing. Its body
// carries the Legion footer naming the head it read, which is what lets a review-App review
// decide a round at all (refusesRound).
func reviewerRequestsChanges(t *testing.T, pool *pgxpool.Pool, engine *Engine, round, head string) {
	t.Helper()
	standsIn(t, pool, phase.Reviewing, "needs_review")
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim",
		HandoffCommit: "review-" + round})
	seedClaimSession(t, pool, reviewClaim, "ses-reviewer")
	applyRefusingNothing(t, pool, engine, intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42,
		State: "changes_requested", CommitID: head, HeadSHA: head, Author: testReviewApp,
		Body: reviewBody("the round "+round+" findings", head)})
}

// architectMovesBack is the architect's own move of the issue out of accepting, the one phase it
// holds, back to the phase named.
func architectMovesBack(t *testing.T, pool *pgxpool.Pool, engine *Engine, to phase.Phase) {
	t.Helper()
	standsIn(t, pool, phase.Accepting, "needs_review")
	applyRefusingNothing(t, pool, engine, rootsMoveBack(t, pool, engine, "LEGION-208", to, "the work does not stand"))
}

// mergerMovesBack is the merger's own move of the issue out of merging, the one phase it holds,
// back to reviewing: a worker's own backward move (request_backward_move) made from the far end
// of the line, for a head it would not merge.
func mergerMovesBack(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
	t.Helper()
	standsIn(t, pool, phase.Merging, "retro")
	applyRefusingNothing(t, pool, engine, intake.BackwardMove{Issue: "LEGION-208",
		Requester: claim.RoleMerger, To: phase.Reviewing,
		Reason: "the head GitHub shows is not the head this merger read"})
}

// marksConflicting is the daemon's two reads of GitHub, a minute apart, that judge head, the pull
// request's own recorded head, conflicting with its base: the confirmed conflict a conflict round
// is read against (record.PullRequest.ConflictingHead, classify.ConflictRound). The reads carry a
// base that requires no check and is not strict, and the issue stands short of accepting, so the
// conflict is recorded and nothing is asked or moved (workflow's mergeability,
// classify.ConflictNeedsMerge).
func marksConflicting(t *testing.T, pool *pgxpool.Pool, engine *Engine, head string) {
	t.Helper()
	read := intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", HeadSHA: head,
		Mergeable: record.MergeabilityConflicting, Rules: &record.BaseRules{}}
	applyRefusingNothing(t, pool, engine, read)
	read.ReadAt = engine.now().Add(conflictConfirmWait)
	applyRefusingNothing(t, pool, engine, read)
}

// Every move back is counted under its own "<from>-<to>" pair, and the pass the daemon has always
// kept rises with all of them.
//
// Four moves are driven here, each of a different pair: a tester's fail (testing-implementing),
// the review App's request for changes (reviewing-implementing), the architect's own move back to
// the implementer (accepting-implementing) and the architect's move back to the plan
// (accepting-planning). Each pair's count is read on its own, so a build that kept one number for
// every move back reads 4 under the first pair and nothing under the others, and a build that
// counted only the moves that end in implementing reads nothing under accepting-planning. The
// architect's two moves are the same role's, made from the same phase: what tells them apart is
// the phase they send the work to, which is half of what a pair is.
//
// The fifth move is made while the daemon's record holds the head conflicting with its base. It
// raises no pair at all: nobody sent the work back, so the move is of no kind a hook or an
// operator counts (recordPass). The pass rises on all five, because a phase completion's key is
// built from it (record.PhaseRow.Pass, api's handoffPosition) and two passes through
// one phase that carried the same number would collide.
//
// The review rounds review_round_cap bounds are counted apart from the pairs, and only a review
// that requested changes opens one (recordRound): of the five moves here the one review's is the
// only round, and no move at the conflicting head makes a second.
func TestEveryBackwardMoveIsCountedByItsPair(t *testing.T) {
	pool := migratedPool(t)
	// The cap is set above every round this proof opens: what it posts is another test's subject
	// (TestTheArchitectIsToldOncePerCapValue), and a cap reached here would say nothing about the
	// counts.
	engine := workBackEngine(50)
	seedWorkBackIssue(t, pool, headOne)

	testerFails(t, pool, engine, "1")
	reviewerRequestsChanges(t, pool, engine, "1", headOne)
	architectMovesBack(t, pool, engine, phase.Planning)
	architectMovesBack(t, pool, engine, phase.Implementing)

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
		{pair: "accepting-implementing", count: 1, by: "the architect's move back to the implementer"},
		{pair: "accepting-planning", count: 1, by: "the architect's move back to the plan"},
	} {
		if moves[want.pair] != want.count {
			t.Fatalf("the moves back counted under %s = %d, want %d, from %s; the whole count is %v",
				want.pair, moves[want.pair], want.count, want.by, moves)
		}
	}
	if pass := implementerPass(t, pool); pass != 4 {
		t.Fatalf("after the four moves back the implementer's row counts %d passes, want 4: every move back raises the pass, whatever kind it is", pass)
	}
	if rounds, conflicts := implementerRounds(t, pool), implementerConflictRounds(t, pool); rounds != 1 || conflicts != 0 {
		t.Fatalf("after the four moves back the implementer's row counts %d review rounds of which %d conflict rounds, want the one review's round and no conflict round: a round is a review that requested changes alone (recordRound), and none of these was made at a conflicting head", rounds, conflicts)
	}

	// The fifth move is made while the record holds the pull request's own head conflicting with
	// its base, which is what keeps it out of the pairs whatever fact makes the move.
	marksConflicting(t, pool, engine, headOne)
	testerFails(t, pool, engine, "2")

	if pass := implementerPass(t, pool); pass != 5 {
		t.Fatalf("after the move made at the conflicting head the implementer's row counts %d passes, want 5: the pass rises on a move no pair counts, since the completion key is built from it", pass)
	}
	if rounds, conflicts := implementerRounds(t, pool), implementerConflictRounds(t, pool); rounds != 1 || conflicts != 0 {
		t.Fatalf("after the move made at the conflicting head the implementer's row counts %d review rounds of which %d conflict rounds, want the one review's round and no conflict round: a tester's fail opens no review round, at a conflicting head or anywhere else", rounds, conflicts)
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
		t.Fatalf("the pairs add up to %d moves back, want the 4 moves somebody made, of the 5 passes the implementer's row counts; the whole count is %v", counted, conflicted)
	}
}

// taskCounts is the counts of the returns of key's task a at generation, as recordPass keeps them
// on its plan_tasks row (record.Store.ReturnCounts).
func taskCounts(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, generation uint64) record.ReturnCounts {
	t.Helper()
	var counts record.ReturnCounts
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		counts, err = engine.store.ReturnCounts(ctx, tx, key, generation, "a")
		return err
	})
	return counts
}

// testerMovesBack is the tester's own move of key out of testing back to the phase named
// (request_backward_move).
func testerMovesBack(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, to phase.Phase) {
	t.Helper()
	applyRefusingNothing(t, pool, engine, intake.BackwardMove{Issue: key, Requester: claim.RoleTester, To: to, Reason: "the review missed a case"})
}

// A move back is counted by the task's own order, not by phase.Order: with the steps
// `implementer reviewer tester`, the reviewer's approval moves the task on from reviewing to
// testing and counts nothing, though testing comes before reviewing in phase.Order, and the
// tester's move back to reviewing is a move back of the task and counts its pair, testing-reviewing,
// and a pass. The tester's fail after it adds its own pair. The twin: an issue with no plan rows
// walks today's line, where the tester's pass into reviewing counts nothing and the reviewer's
// move back to testing counts reviewing-testing.
func TestAReturnInAnyOrderCountsItsPair(t *testing.T) {
	pool := migratedPool(t)
	engine := stepEngine(nil)
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "reviewer", "tester")
	move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
	move(t, pool, engine, root.Key, TriggerImplementationReady, "", record.PhaseRow{})
	move(t, pool, engine, root.Key, TriggerReviewApproved, "", record.PhaseRow{})
	if counts := taskCounts(t, pool, engine, root.Key, root.Generation); counts.Pass != 0 || len(counts.BackwardMoves) != 0 {
		t.Fatalf("after the approval of reviewing into testing the task counts %+v, want nothing: the approval moves it on", counts)
	}
	testerMovesBack(t, pool, engine, root.Key, phase.Reviewing)
	if counts := taskCounts(t, pool, engine, root.Key, root.Generation); counts.Pass != 1 || !maps.Equal(counts.BackwardMoves, map[string]int{"testing-reviewing": 1}) {
		t.Fatalf("after the tester's move back to reviewing the task counts %+v, want pass 1 and testing-reviewing 1", counts)
	}
	move(t, pool, engine, root.Key, TriggerReviewApproved, "", record.PhaseRow{})
	move(t, pool, engine, root.Key, TriggerTesterFailed, "", record.PhaseRow{})
	if counts := taskCounts(t, pool, engine, root.Key, root.Generation); counts.Pass != 2 ||
		!maps.Equal(counts.BackwardMoves, map[string]int{"testing-reviewing": 1, "testing-implementing": 1}) {
		t.Fatalf("after the tester's fail the task counts %+v, want pass 2, testing-reviewing 1 and testing-implementing 1", counts)
	}

	// The twin: an issue with no plan rows counts by today's line.
	todays := migratedPool(t)
	seedWorkBackIssue(t, todays, headOne)
	move(t, todays, engine, "LEGION-208", TriggerImplementationReady, "", record.PhaseRow{})
	move(t, todays, engine, "LEGION-208", TriggerTesterPassed, "", record.PhaseRow{})
	if moves := backwardMoves(t, todays); len(moves) != 0 {
		t.Fatalf("with no plan rows the tester's pass into reviewing counted %v, want nothing", moves)
	}
	applyRefusingNothing(t, todays, engine, intake.BackwardMove{Issue: "LEGION-208", Requester: claim.RoleReviewer, To: phase.Testing, Reason: "test it again"})
	if moves := backwardMoves(t, todays); !maps.Equal(moves, map[string]int{"reviewing-testing": 1}) {
		t.Fatalf("with no plan rows the reviewer's move back to testing counted %v, want reviewing-testing 1", moves)
	}
}

// The hook a deployment that writes no `hooks:` key runs fires at the fifth move back of a task in
// another order, and its message lists each count under its own pair, the move back that is a
// forward pair of phase.Order among them: `testing-reviewing=4`. The fifth move is the tester's
// move back to reviewing, which the prompt names.
func TestAHookNamesAMoveOfAnyOrderAtItsFifthReturn(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "reviewer", "tester")
	move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{})
	move(t, pool, engine, root.Key, TriggerImplementationReady, "", record.PhaseRow{})
	move(t, pool, engine, root.Key, TriggerReviewApproved, "", record.PhaseRow{})
	testerMovesBack(t, pool, engine, root.Key, phase.Reviewing)
	move(t, pool, engine, root.Key, TriggerReviewApproved, "", record.PhaseRow{})
	move(t, pool, engine, root.Key, TriggerTesterFailed, "", record.PhaseRow{})
	move(t, pool, engine, root.Key, TriggerImplementationReady, "", record.PhaseRow{})
	for range 3 {
		move(t, pool, engine, root.Key, TriggerReviewApproved, "", record.PhaseRow{})
		if prompts := hookPrompts(t, pool); len(prompts) != 0 {
			t.Fatalf("the hook fired %+v before the fifth move back", prompts)
		}
		testerMovesBack(t, pool, engine, root.Key, phase.Reviewing)
	}
	prompts := hookPrompts(t, pool)
	if want := []string{"implementer", "tester", "reviewer"}; !sameStrings(promptRoles(prompts), want) {
		t.Fatalf("the hook prompted %v at the fifth move back, want %v", promptRoles(prompts), want)
	}
	for _, prompt := range prompts {
		if prompt.Hook != "work-back" || prompt.Count != 5 || prompt.From != phase.Testing || prompt.To != phase.Reviewing {
			t.Fatalf("the %s's prompt = %+v, want work-back fired at the fifth move back, testing to reviewing", prompt.Role, prompt)
		}
		if message := prompt.Message(root.Key); !strings.Contains(message, "Counts by move: testing-implementing=1, testing-reviewing=4.") {
			t.Fatalf("the %s's message lists other counts than testing-implementing=1, testing-reviewing=4:\n%s", prompt.Role, message)
		}
	}
}
