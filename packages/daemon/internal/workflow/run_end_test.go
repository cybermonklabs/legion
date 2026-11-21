package workflow

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Every move of the table that leaves a step names the end of the run it leaves: the result the
// table's trigger gives it, which the record does not hold, so a move that named none would end
// its run stopped whatever it was. Each row is moved on an issue of its own, standing at the
// row's phase with an open pull request; the move's named end is read before the fact's settle,
// and the end the settle writes after it.
func TestEveryMoveOutOfAStepNamesItsEnd(t *testing.T) {
	pool := migratedPool(t)
	engine := stepEngine(nil)
	moved := 0
	for i, row := range Table {
		from, _, _, runs := stepOf(row.From)
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
		target := phase.Phase("")
		if row.Trigger == TriggerBackward {
			target = row.To
		}
		var named StepEnd
		var found bool
		applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
			pr, err := engine.store.PullRequest(ctx, tx, key)
			if err != nil {
				return err
			}
			if err := engine.transition(ctx, tx, issue, row.Trigger, target, record.PhaseRow{}, pr, "the table test's move"); err != nil {
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
		from, _, _, runs := stepOf(row.From)
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
		target := phase.Phase("")
		if row.Trigger == TriggerBackward {
			target = row.To
		}
		applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
			pr, err := engine.store.PullRequest(ctx, tx, key)
			if err != nil {
				return err
			}
			return engine.transition(ctx, tx, issue, row.Trigger, target, record.PhaseRow{}, pr, "the held move")
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
