package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The idle view reads the running steps of the issue as the issue's own record reads them
// (record.Issue.Running): store and record import neither each other, so both read the one SQL of
// the running steps (pods.RunningColumn) and record.RunningOf reads the view's rows, and this holds
// the two to one answer. An issue whose two tasks stand at step 1 and at step 2 of `implementer
// checker` reads both steps in both; one whose task stands at step 2 reads the checker's in both,
// one at step 1 the implementer's; one held from step 2, its task running under the issue's hold,
// reads the checker's held in both; and an issue with no plan rows reads the one step of its phase
// in both.
//
// The fault it catches is an idle view that reads one running step of several (its control: the
// view's read of the single row of one step, which fails on the issue of two tasks).
func TestTheIdleViewReadsEachRunningStep(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	records := record.NewStore()
	givenAt := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	issue := func(key string, p phase.Phase) record.Issue {
		return record.Issue{Key: key, Tree: key, Project: "LEGION", Title: key, Phase: p, Generation: 1,
			Status: "in_progress", Rank: "A", LastDispatchSeq: 1,
			Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	}
	// planAt writes key's plan of one task for each of positions, a, b and on, each with the steps
	// `implementer checker` and running at its position.
	planAt := func(tx pgx.Tx, key string, positions ...int) error {
		project, err := claim.ProjectToken("LEGION")
		if err != nil {
			return err
		}
		owner, err := claim.NewToken(project, key, claim.RoleArchitect)
		if err != nil {
			return err
		}
		v := record.PlanVersion{Issue: key, Generation: 1, Version: 1, ArtifactID: "plan-artifact",
			Source: record.PlanSourceOwner, GivenBy: owner, GivenAt: givenAt}
		var tasks []record.PlanTask
		var steps []record.PlanStep
		for i, position := range positions {
			task := string(rune('a' + i))
			tasks = append(tasks, record.PlanTask{Issue: key, Generation: 1, Task: task, PlanVersion: 1, BlockText: "Build the widget.",
				State: record.TaskRunning, Place: record.TaskPlace{Kind: record.PlaceStep, Position: position}})
			steps = append(steps,
				record.PlanStep{Issue: key, Generation: 1, Task: task, Position: 1, Role: "implementer", Type: "implementation", Agent: claim.Agent("implementer-" + task)},
				record.PlanStep{Issue: key, Generation: 1, Task: task, Position: 2, Role: "checker", Type: "testing", Agent: claim.Agent("checker-" + task)})
		}
		return records.PutPlan(ctx, tx, v, tasks, steps)
	}
	held := issue("LEGION-212", phase.Held)
	held.Hold = &record.Hold{From: phase.Step, Reason: record.HoldEscalated}
	if err := Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		for _, seeded := range []record.Issue{issue("LEGION-208", phase.Step), issue("LEGION-209", phase.Implementing),
			issue("LEGION-210", phase.Implementing), issue("LEGION-211", phase.Implementing), held} {
			if err := records.PutIssue(ctx, tx, seeded); err != nil {
				return err
			}
		}
		for key, positions := range map[string][]int{"LEGION-208": {2}, "LEGION-209": {1}, "LEGION-211": {1, 2}, "LEGION-212": {2}} {
			if err := planAt(tx, key, positions...); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the issues and their plans: %v", err)
	}

	steps := map[string]int{"LEGION-208": 1, "LEGION-209": 1, "LEGION-210": 1, "LEGION-211": 2, "LEGION-212": 1}
	for key, count := range steps {
		view := idleViewOf(t, st, key)
		var recorded *record.Issue
		if err := Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			var err error
			recorded, err = records.Issue(ctx, tx, key)
			return err
		}); err != nil || recorded == nil {
			t.Fatalf("read %s as the record does: %v", key, err)
		}
		read := record.Issue{Key: view.Issue, Phase: view.Phase}
		if view.HeldFrom != "" {
			read.Hold = &record.Hold{From: view.HeldFrom}
		}
		viewed := record.RunningOf(read, view.Planned, view.Running)
		if !view.Found || !reflect.DeepEqual(viewed, recorded.Running) || len(viewed) != count {
			t.Errorf("%s: the idle view reads the running steps %+v and the issue's record %+v, want the same %d", key, viewed, recorded.Running, count)
		}
	}
}
