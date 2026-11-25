package api

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// taskStanding is where one task of a seeded plan stands: its state, its place (a step's place
// from 1, 0 for none) and its pass.
type taskStanding struct {
	task  string
	state record.TaskState
	place int
	pass  int
}

// seedTaskPlan gives key, at generation 1, an owner's plan of one task, which runs the steps
// implementer and tester, and stands it as s says, its pass on the task.
func seedTaskPlan(t *testing.T, h *harness, key string, s taskStanding) {
	t.Helper()
	ctx, records := context.Background(), record.NewStore()
	v := record.PlanVersion{Issue: key, Generation: 1, Version: 1, ArtifactID: "plan-" + key, Source: record.PlanSourceOwner,
		GivenBy: "owner", GivenAt: time.Date(2026, 11, 17, 6, 0, 0, 0, time.UTC)}
	tasks := []record.PlanTask{{Issue: key, Generation: 1, Task: s.task, PlanVersion: 1, BlockText: "Task " + s.task + ".", State: record.TaskWaiting}}
	steps := record.NameAgents(nil, nil, nil, []record.PlanStep{
		{Issue: key, Generation: 1, Task: s.task, Position: 1, Role: "implementer", Type: "implementation"},
		{Issue: key, Generation: 1, Task: s.task, Position: 2, Role: "tester", Type: "testing"}})
	if err := store.Transact(ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := records.PutPlan(ctx, tx, v, tasks, steps); err != nil {
			return err
		}
		if s.place > 0 {
			if err := records.MoveTask(ctx, tx, key, 1, s.task, record.TaskPlace{Kind: record.PlaceStep, Position: s.place}, record.TaskRunning); err != nil {
				return err
			}
		}
		if s.state == record.TaskStopped {
			if err := records.StopTask(ctx, tx, key, 1, s.task, "task "+s.task+" of "+key+" was stopped by its owner"); err != nil {
				return err
			}
		}
		return records.PutReturnCounts(ctx, tx, key, 1, s.task, record.ReturnCounts{Pass: s.pass})
	}); err != nil {
		t.Fatalf("seed the plan of %s: %v", key, err)
	}
}

// seedImplementerPass writes pass on key's implementer's phases row, where an issue keeps its
// counts of returns while it has no plan rows, and where it kept them before plans.
func seedImplementerPass(t *testing.T, h *harness, key string, pass int) {
	t.Helper()
	ctx := context.Background()
	if err := store.Transact(ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().PutReturnCounts(ctx, tx, key, 1, "", record.ReturnCounts{Pass: pass})
	}); err != nil {
		t.Fatalf("seed the implementer's pass of %s: %v", key, err)
	}
}

// holdForTheStop holds key from testing for its stopped task, as the owner's stop leaves it
// (workflow's stopTask holds the issue from the phase the task stood in).
func holdForTheStop(t *testing.T, h *harness, key string) {
	t.Helper()
	ctx := context.Background()
	if err := store.Transact(ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		records := record.NewStore()
		issue, err := records.Issue(ctx, tx, key)
		if err != nil {
			return err
		}
		issue.Phase, issue.Hold = phase.Held, &record.Hold{From: phase.Testing, Reason: record.HoldTaskStopped}
		return records.PutIssue(ctx, tx, *issue)
	}); err != nil {
		t.Fatalf("hold %s for its stopped task: %v", key, err)
	}
}

// The completion of an issue with plan rows names the task of its plan in its key, and its pass is
// that task's count of returns, never the implementer's phases row's: a new task at pass 0 takes no
// key an earlier task of the issue used. The task is the plan's one task whatever its state
// (record.StoredPlan.Task), a stopped one among them, whose issue is held for the stop. An issue
// with no plan rows keeps today's key byte for byte, its pass read from its implementer's row (the
// control).
func TestTheCompletionKeyNamesTheTaskOfAnIssueWithPlanRows(t *testing.T) {
	h, _ := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	for _, tc := range []struct {
		name  string
		issue string
		seed  func(t *testing.T)
		want  func(record string) string
	}{
		{
			name:  "the plan's one task, running at the tester's step",
			issue: "LEGION-208",
			seed: func(t *testing.T) {
				// Written before any plan row, the count lands on the implementer's row, which the
				// key must then not read.
				seedImplementerPass(t, h, "LEGION-208", 5)
				seedTaskPlan(t, h, "LEGION-208", taskStanding{task: "a", state: record.TaskRunning, place: 2, pass: 2})
			},
			want: func(record string) string { return "handoff:LEGION-208:1:1:1:tester:testing:a:2:" + record + ":pass" },
		},
		{
			name:  "the plan's one task, stopped by its owner, its issue held for the stop",
			issue: "LEGION-210",
			seed: func(t *testing.T) {
				seedTaskPlan(t, h, "LEGION-210", taskStanding{task: "a", state: record.TaskStopped, place: 2, pass: 4})
				holdForTheStop(t, h, "LEGION-210")
			},
			// The issue is held for the stop, so the key names the phase it stands in, held.
			want: func(record string) string { return "handoff:LEGION-210:1:1:1:tester:held:a:4:" + record + ":pass" },
		},
		{
			name:  "control: no plan rows, today's key",
			issue: "LEGION-209",
			seed:  func(t *testing.T) { seedImplementerPass(t, h, "LEGION-209", 3) },
			want:  func(record string) string { return "handoff:LEGION-209:1:1:1:tester:testing:3:" + record + ":pass" },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seedIssueAt(t, h, tc.issue, phase.Testing)
			tc.seed(t)
			tester := newLiveClaim(t, h, tc.issue, claim.RoleTester)
			written := tester.writeHandoff("test", 1)
			completion(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", HandoffID: written, HandoffVersion: 1}))
			if got, want := completionKeys(t, h, tc.issue), tc.want(written); len(got) != 1 || got[0] != want {
				t.Fatalf("the completion keys of %s = %q, want [%q]", tc.issue, got, want)
			}
		})
	}
}
