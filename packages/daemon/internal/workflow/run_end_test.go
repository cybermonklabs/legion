package workflow

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Every move of the table that leaves a step names the end of the run it leaves: the result the
// table's trigger gives it, which the record does not hold, so a move that named none would end
// its run stopped whatever it was. Each row is moved on an issue of its own, standing at the
// row's phase with an open pull request; the move's named end is read before the fact's settle,
// and the end the settle writes after it.
func TestEveryMoveOutOfAStepNamesItsEnd(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	engine := stepEngine(nil)
	moved := 0
	for i, row := range Table {
		from, _, _, runs := placeOnWalk(todaysWalk(), standingIn(row.From))
		if !runs {
			continue
		}
		moved++
		name := fmt.Sprintf("%s --%s--> %s", row.From, row.Trigger, row.To)
		key := fmt.Sprintf("LEGION-%d", 400+i)
		issue := record.Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: key, Project: "LEGION",
			Title: name, Phase: row.From, Generation: 1, Status: PhaseStatus(row.From), Rank: "U", LastDispatchSeq: 1}
		seedIssue(t, pool, issue)
		seedTreeLifecycle(t, pool, key)
		seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: key, Repo: "sjawhar/legion", Number: 400 + i,
			Branch: "legion/" + key, HeadSHA: "head", Failing: []string{}})
		target := record.TaskPlace{}
		if row.Trigger == TriggerBackward {
			target = row.ToPlace
		}
		var named StepEnd
		var found bool
		applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
			pr, err := engine.store.MergePullRequest(ctx, tx, key)
			if err != nil {
				return err
			}
			s, err := firstScope(ctx, tx, engine, issue)
			if err != nil {
				return err
			}
			if err := engine.transition(ctx, tx, s, row.Trigger, target, record.PhaseRow{}, pr, "the table test's move"); err != nil {
				return err
			}
			runs, err := factRunsOf(ctx)
			if err != nil {
				return err
			}
			named, found = runs.ends[runAt{issue: key, generation: 1, place: from}]
			return nil
		})
		want, _ := endOf(row.Trigger)
		if !found {
			t.Errorf("%s leaves a step and names no end", name)
			continue
		}
		if named.Result != want {
			t.Errorf("%s names the end %q, want %q", name, named.Result, want)
		}
		var ends []string
		for _, w := range writesAt(stepWrites(t, pool, key), from.Kind, from.Place) {
			if isEnd(w) {
				ends = append(ends, resultOf(w))
			}
		}
		if !slices.Equal(ends, []string{string(want)}) {
			t.Errorf("%s wrote the ends %v of the step it left, want one, %s", name, ends, want)
		}
	}
	if moved < 15 {
		t.Fatalf("moved %d rows of the table out of a step, want every row that leaves one", moved)
	}
}

// A move whose start an open Dispatch child holds (dispatch://INFRA-3320 R9) ends the step it
// leaves as the move it holds would: the tester's pass passed, a request for changes failed. The
// parent's wait in awaiting_children follows the step, and it runs no step of its own. With the
// move's end named after the guard instead of before it, every one of these steps ends stopped.
func TestAMoveAnOpenChildHoldsNamesTheEndOfTheStepItLeaves(t *testing.T) {
	pool := migratedPool(t)
	engine := stepEngine(nil)
	held := 0
	for i, row := range Table {
		from, _, _, runs := placeOnWalk(todaysWalk(), standingIn(row.From))
		if !runs {
			continue
		}
		name := fmt.Sprintf("%s --%s--> %s", row.From, row.Trigger, row.To)
		key, child := fmt.Sprintf("LEGION-%d", 600+i), fmt.Sprintf("LEGION-%d", 900+i)
		issue := record.Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: key, Project: "LEGION",
			Title: name, Phase: row.From, Generation: 1, Status: PhaseStatus(row.From), Rank: "U", LastDispatchSeq: 1}
		seedIssue(t, pool, issue)
		seedTreeLifecycle(t, pool, key)
		seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: key, Repo: "sjawhar/legion", Number: 600 + i,
			Branch: "legion/" + key, HeadSHA: "head", Failing: []string{}})
		seedMirrorChild(t, pool, child, "todo", key)
		target := record.TaskPlace{}
		if row.Trigger == TriggerBackward {
			target = row.ToPlace
		}
		applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
			pr, err := engine.store.MergePullRequest(ctx, tx, key)
			if err != nil {
				return err
			}
			s, err := firstScope(ctx, tx, engine, issue)
			if err != nil {
				return err
			}
			return engine.transition(ctx, tx, s, row.Trigger, target, record.PhaseRow{}, pr, "the held move")
		})
		if issueRecord(t, pool, key).Phase != phase.AwaitingChildren {
			continue
		}
		held++
		want, _ := endOf(row.Trigger)
		var ends []string
		for _, w := range writesAt(stepWrites(t, pool, key), from.Kind, from.Place) {
			if isEnd(w) {
				ends = append(ends, resultOf(w))
			}
		}
		if !slices.Equal(ends, []string{string(want)}) {
			t.Errorf("%s, held by the open child %s, wrote the ends %v of the step it left, want one, %s", name, child, ends, want)
		}
	}
	if held < 8 {
		t.Fatalf("an open child held %d moves of the table out of a step, want every move into planning through accepting", held)
	}
}

// A hand-over fence an earlier linger of the tree left can stand beside a member whose merge waits
// in the tree's linger now (record.Handover outlives its linger). It is that earlier linger's, so
// it hands the member to no readmit: the unmerged close of the member's pull request after this
// linger's deadline ends the merge stopped. Only a fence of this tree at the generation of the
// linger the merge waits in keeps the merge open (mergeCanLand).
func TestAnUnmergedCloseEndsAMergeWhoseFenceIsAnEarlierLingers(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	root := stepRoot(t, pool, phase.Reviewing)
	root.Generation = 2
	seedIssue(t, pool, root)
	engine := stepEngine(nil)
	mergingChild(t, pool, engine)
	apply := applyFacts(t, pool, engine)
	apply("park-root", intake.DispatchIssue{Key: root.Key, ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.updated",
		Status: "backlog", Title: "root", Rank: "U"})
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return engine.store.PutHandover(t.Context(), tx, record.Handover{Issue: "LEGION-209", Tree: root.Tree, Generation: root.Generation - 1})
	})
	if _, end := runOf(stepWrites(t, pool, "LEGION-209"), "merge", 0); isEnd(end) {
		t.Fatalf("the park ended LEGION-209's merge %+v; a merge that awaits its landing in the linger stays open", end)
	}
	parked := issueRecord(t, pool, root.Key)
	engine.cfg.Clock = func() time.Time { return parked.LingerUntil.Add(time.Second) }
	apply("close-unmerged", intake.PullRequestClosed{Repo: "sjawhar/legion", Number: 43, HeadSHA: "head-43"})
	start, end := runOf(stepWrites(t, pool, "LEGION-209"), "merge", 0)
	if !isEnd(end) || end.ID != start.ID {
		t.Fatalf("after the unmerged close past the deadline LEGION-209's merge is %+v then %+v, want its start and its end", start, end)
	}
	assertEnd(t, end, "merge", 0, 3, "stopped", nil)
}
