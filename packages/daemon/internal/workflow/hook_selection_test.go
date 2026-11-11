package workflow

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// firedAt is the counts hook has fired at so far, oldest first: one number for each firing, read
// off the role messages the firings wrote (fireHook). Each hook below names one role, so each
// firing is one row.
func firedAt(t *testing.T, pool *pgxpool.Pool, hook string) []int {
	t.Helper()
	counts := []int{}
	for _, prompt := range hookPrompts(t, pool) {
		if prompt.Hook == hook {
			counts = append(counts, prompt.Count)
		}
	}
	return counts
}

// clearsConflicting is the daemon's read of GitHub that judges the pull request's own head
// mergeable again, which takes the conflict mark off it (workflow's mergeability).
func clearsConflicting(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
	t.Helper()
	applyRefusingNothing(t, pool, engine, intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42,
		Base: "main", Mergeable: record.MergeabilityMergeable})
}

// A hook counts the moves back it selects and fires on its own multiple of them.
//
// Two hooks stand on the work-back event here: one over every move back, every 5, and one over
// the reviewing-implementing move alone, every 3. The run drives moves of four kinds and reads
// each hook's firings by the counts they fired at, after every move.
//
// A build that counted every move back for both hooks fires the second hook at the third move
// back of the run, which is a tester's and two reviews, where the hook has counted two of its
// own; a build that read the first hook of the list and stopped leaves the second hook's firings
// missing altogether. The move made at a conflicting head fires neither hook, since it raised no
// pair: nobody sent the work back. Every move whose hook's count is not a multiple fires nothing,
// which is what the steps between the firings read.
func TestAHookFiresOnTheMovesItSelects(t *testing.T) {
	pool := migratedPool(t)
	engine := workBackEngine(50,
		Hook{Name: "every-move", On: HookEventWorkBack, Every: 5,
			Roles: []claim.Role{claim.RoleImplementer}, Prompt: saySoFar},
		Hook{Name: "review-loop", On: HookEventWorkBack, Moves: []string{"reviewing-implementing"}, Every: 3,
			Roles: []claim.Role{claim.RoleReviewer}, Prompt: readTheThreads})
	seedWorkBackIssue(t, pool, "h0")

	for _, step := range []struct {
		// moved says what the step drives and what it leaves the counts at, in the failure
		// message's own words.
		moved string
		drive func()
		// everyMove and reviewLoop are the counts each hook has fired at by the end of the step.
		everyMove, reviewLoop []int
	}{
		{
			moved: "a tester's fail, the 1st move back and the 1st of any kind",
			drive: func() { testerFails(t, pool, engine, "1") },
		},
		{
			moved: "a review's request for changes, the 2nd move back and the 1st the review-loop hook counts",
			drive: func() { reviewerRequestsChanges(t, pool, engine, "1", "h0") },
		},
		{
			moved: "a review's request for changes, the 3rd move back and the 2nd the review-loop hook counts",
			drive: func() { reviewerRequestsChanges(t, pool, engine, "2", "h0") },
		},
		{
			moved:      "a review's request for changes, the 4th move back and the 3rd the review-loop hook counts",
			drive:      func() { reviewerRequestsChanges(t, pool, engine, "3", "h0") },
			reviewLoop: []int{3},
		},
		{
			moved:      "a red CI verdict while the architect holds the issue, the 5th move back and the 3rd the review-loop hook counts",
			drive:      func() { ciTurnsRed(t, pool, engine, phase.Accepting, "needs_review", "h0", "h1", 1) },
			everyMove:  []int{5},
			reviewLoop: []int{3},
		},
		{
			moved: "a tester's fail at a head the record holds conflicting with its base, which raises no pair",
			drive: func() {
				marksConflicting(t, pool, engine)
				testerFails(t, pool, engine, "2")
				clearsConflicting(t, pool, engine)
			},
			everyMove:  []int{5},
			reviewLoop: []int{3},
		},
		{
			moved:      "a review's request for changes, the 6th move back and the 4th the review-loop hook counts",
			drive:      func() { reviewerRequestsChanges(t, pool, engine, "4", "h1") },
			everyMove:  []int{5},
			reviewLoop: []int{3},
		},
		{
			moved:      "a review's request for changes, the 7th move back and the 5th the review-loop hook counts",
			drive:      func() { reviewerRequestsChanges(t, pool, engine, "5", "h1") },
			everyMove:  []int{5},
			reviewLoop: []int{3},
		},
		{
			moved:      "a review's request for changes, the 8th move back and the 6th the review-loop hook counts",
			drive:      func() { reviewerRequestsChanges(t, pool, engine, "6", "h1") },
			everyMove:  []int{5},
			reviewLoop: []int{3, 6},
		},
		{
			moved:      "the architect's move back to the plan, the 9th move back and the 6th the review-loop hook counts",
			drive:      func() { architectMovesBack(t, pool, engine, phase.Planning) },
			everyMove:  []int{5},
			reviewLoop: []int{3, 6},
		},
		{
			moved:      "a tester's fail, the 10th move back and the 6th the review-loop hook counts",
			drive:      func() { testerFails(t, pool, engine, "3") },
			everyMove:  []int{5, 10},
			reviewLoop: []int{3, 6},
		},
	} {
		step.drive()
		if got := firedAt(t, pool, "every-move"); !slices.Equal(got, nonNil(step.everyMove)) {
			t.Fatalf("after %s, the every-move hook, which counts every move back and fires every 5, has fired at %v moves back, want %v",
				step.moved, got, nonNil(step.everyMove))
		}
		if got := firedAt(t, pool, "review-loop"); !slices.Equal(got, nonNil(step.reviewLoop)) {
			t.Fatalf("after %s, the review-loop hook, which counts the reviewing-implementing move alone and fires every 3, has fired at %v moves back, want %v",
				step.moved, got, nonNil(step.reviewLoop))
		}
	}

	// What each hook counted, read off the issue's own counts: the every-move hook's 10 is every
	// move somebody made, and the review-loop hook's 6 is the reviews alone.
	moves := backwardMoves(t, pool)
	counted := 0
	for _, count := range moves {
		counted += count
	}
	if counted != 10 || moves["reviewing-implementing"] != 6 {
		t.Fatalf("the issue counts %d moves back of which %d reviews, want 10 and 6: the two hooks fired on those two numbers; the whole count is %v",
			counted, moves["reviewing-implementing"], moves)
	}
	var prompts int
	if err := pool.QueryRow(context.Background(), "select count(*) from outbox where kind = 'hook_prompt'").Scan(&prompts); err != nil {
		t.Fatalf("count the role messages the firings wrote: %v", err)
	}
	if prompts != 4 {
		t.Fatalf("the firings wrote %d role messages, want 4: two firings of each hook, each hook naming one role", prompts)
	}
}

// nonNil is counts as an empty slice rather than a nil one, so that a step that expects no firing
// is compared against a slice of no numbers.
func nonNil(counts []int) []int {
	if counts == nil {
		return []int{}
	}
	return counts
}
