package record

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
)

// sixTasks is a plan of issue at generation with six tasks, each with the steps `implementer
// reviewer`, the agents named as given: a at step 1, the implementer's step, whose agent is
// implementer; aa at step 1, whose agent is r; b at step 2, the reviewer's step, whose agent is r;
// c, d and e at step 1. Each task is written running at its place; the test then stops, holds and
// makes wait the ones it needs.
func sixTasks(issue string, generation uint64) (PlanVersion, []PlanTask, []PlanStep) {
	v := PlanVersion{Issue: issue, Generation: generation, Version: 1, ArtifactID: planAuthor, Source: PlanSourceOwner, GivenBy: planOwner, GivenAt: planGivenAt}
	agents := map[string][2]claim.Agent{
		"a": {"implementer", "reviewer"}, "aa": {"r", "reviewer-2"}, "b": {"implementer-2", "r"},
		"c": {"implementer-3", "reviewer-3"}, "d": {"implementer-4", "reviewer-4"}, "e": {"implementer-5", "reviewer-5"},
	}
	places := map[string]TaskPlace{"a": {Kind: PlaceStep, Position: 1}, "aa": {Kind: PlaceStep, Position: 1}, "b": {Kind: PlaceStep, Position: 2},
		"c": {Kind: PlaceStep, Position: 1}, "d": {Kind: PlaceStep, Position: 1}, "e": {Kind: PlaceStep, Position: 1}}
	var tasks []PlanTask
	var steps []PlanStep
	for _, task := range []string{"a", "aa", "b", "c", "d", "e"} {
		tasks = append(tasks, PlanTask{Issue: issue, Generation: generation, Task: task, PlanVersion: 1, BlockText: "Do " + task + ".",
			State: TaskRunning, Place: places[task]})
		steps = append(steps,
			PlanStep{Issue: issue, Generation: generation, Task: task, Position: 1, Role: "implementer", Type: plan.TypeImplementation, Agent: agents[task][0]},
			PlanStep{Issue: issue, Generation: generation, Task: task, Position: 2, Role: "reviewer", Type: plan.TypeReview, Agent: agents[task][1]})
	}
	return v, tasks, steps
}

// An issue reads each of its running steps with the issue, from its plan rows: of six tasks, a at
// the implementer's step, b at the reviewer's, c stopped, d held at the implementer's step, aa
// whose step waits for its agent r and e whose step waits for its join, the issue reads a, b and d
// in task id order, with the role, the phase and the agent of each and d held, through Issue,
// Issues, LiveIssues, SlottedIssues and TreeIssues alike. RunningAt(implementing) holds a and d,
// and StepOfAgent("r") is b, not aa, whose step waits for r. An issue with no plan rows holds one
// step of the task default, at the place of its phase on today's line, with the role of that phase,
// and none while it stands at admitted, where its task stands at no act.
//
// The fault it catches is a reader that sees one running step of several, or one that sees a step
// whose start waits (its controls: the read with `limit 1`, and the read without its filter of the
// wait).
func TestAnIssueReadsEachOfItsRunningSteps(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	records := NewStore()
	planned := issueFixture("LEGION-208")
	bare := issueFixture("LEGION-210")
	admitted := issueFixture("LEGION-211")
	admitted.Phase = phase.Admitted
	inTx(t, st, func(tx pgx.Tx) {
		for i, issue := range []Issue{planned, bare, admitted} {
			must(t, records.PutIssue(ctx, tx, issue))
			must(t, records.PutSlot(ctx, tx, Slot{Issue: issue.Key, Index: i, AdmittedAt: planGivenAt}))
		}
		v, tasks, steps := sixTasks(planned.Key, planned.Generation)
		putPlan(t, tx, v, tasks, steps)
		must(t, records.StopTask(ctx, tx, planned.Key, planned.Generation, "c", "its owner stopped it"))
		must(t, records.HoldTask(ctx, tx, planned.Key, planned.Generation, "d", ""))
		since := planGivenAt.Add(time.Minute)
		must(t, records.SetStepWait(ctx, tx, planned.Key, planned.Generation, "aa", StepWaitAgent, &since))
		must(t, records.SetStepWait(ctx, tx, planned.Key, planned.Generation, "e", StepWaitJoin, &since))
	})

	step1, step2 := TaskPlace{Kind: PlaceStep, Position: 1}, TaskPlace{Kind: PlaceStep, Position: 2}
	want := map[string][]RunningStep{
		planned.Key: {
			{Task: "a", Place: step1, Role: claim.RoleImplementer, Phase: phase.Implementing, Agent: "implementer"},
			{Task: "b", Place: step2, Role: claim.RoleReviewer, Phase: phase.Reviewing, Agent: "r"},
			{Task: "d", Place: step1, Role: claim.RoleImplementer, Phase: phase.Implementing, Agent: "implementer-4", Held: true},
		},
		bare.Key: {
			{Task: plan.DefaultTask, Place: step2, Role: claim.RoleImplementer, Phase: phase.Implementing, Agent: "implementer"},
		},
		admitted.Key: nil,
	}
	inTx(t, st, func(tx pgx.Tx) {
		reads := map[string]func() ([]Issue, error){
			"Issues":        func() ([]Issue, error) { return records.Issues(ctx, tx) },
			"LiveIssues":    func() ([]Issue, error) { return records.LiveIssues(ctx, tx, planned.Project) },
			"SlottedIssues": func() ([]Issue, error) { return records.SlottedIssues(ctx, tx, planned.Project) },
			"Issue": func() ([]Issue, error) {
				var issues []Issue
				for key := range want {
					issue, err := records.Issue(ctx, tx, key)
					if err != nil || issue == nil {
						return nil, err
					}
					issues = append(issues, *issue)
				}
				return issues, nil
			},
			"TreeIssues": func() ([]Issue, error) {
				var issues []Issue
				for key := range want {
					tree, err := records.TreeIssues(ctx, tx, key)
					if err != nil {
						return nil, err
					}
					issues = append(issues, tree...)
				}
				return issues, nil
			},
		}
		for name, read := range reads {
			issues, err := read()
			must(t, err)
			seen := map[string]bool{}
			for _, issue := range issues {
				if wanted, ok := want[issue.Key]; ok {
					seen[issue.Key] = true
					if !reflect.DeepEqual(issue.Running, wanted) {
						t.Errorf("%s reads %s with the running steps %+v, want %+v", name, issue.Key, issue.Running, wanted)
					}
				}
			}
			if len(seen) != len(want) {
				t.Errorf("%s read %d of the %d issues, so it proves nothing of the others: %v", name, len(seen), len(want), seen)
			}
		}

		read, err := records.Issue(ctx, tx, planned.Key)
		must(t, err)
		if got, wanted := read.RunningAt(phase.Implementing), []RunningStep{want[planned.Key][0], want[planned.Key][2]}; !reflect.DeepEqual(got, wanted) {
			t.Errorf("RunningAt(implementing) of %s = %+v, want a and d: %+v", planned.Key, got, wanted)
		}
		if got, ok := read.StepOfAgent("r"); !ok || got.Task != "b" {
			t.Errorf("StepOfAgent(r) of %s = %+v (%t), want b's step: aa's step waits for r and is no running step", planned.Key, got, ok)
		}
		if got, ok := read.StepOfAgent(""); ok {
			t.Errorf("StepOfAgent(\"\") of %s = %+v, want none: no agent is named \"\"", planned.Key, got)
		}
		// The read of the running steps alone, which a move of a task reads again, answers what
		// the issue's own read answers.
		alone, err := records.RunningSteps(ctx, tx, *read)
		must(t, err)
		if !reflect.DeepEqual(alone, read.Running) {
			t.Errorf("RunningSteps of %s = %+v, want the issue's own read %+v", planned.Key, alone, read.Running)
		}
	})
}

// The step a stop ended is no running step, and is read from the plan rows while the issue's stop
// holds it (StoppedStep): the role, the phase and the agent of the step the stopped task stands
// at, here the checker's step 2 of `implementer checker`. An issue held for any other reason, and
// one not held, has none, and so does a stop at no act. A running task is no stopped step either.
func TestTheStepAStopEndedIsReadFromThePlanRows(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	records := NewStore()
	stopped := issueFixture("LEGION-208")
	stopped.Phase, stopped.Hold = phase.Held, &Hold{From: phase.Step, Reason: HoldTaskStopped}
	escalated := issueFixture("LEGION-209")
	escalated.Phase, escalated.Hold = phase.Held, &Hold{From: phase.Step, Reason: HoldEscalated}
	inTx(t, st, func(tx pgx.Tx) {
		for _, issue := range []Issue{stopped, escalated} {
			must(t, records.PutIssue(ctx, tx, issue))
			v, tasks, steps := checkerPlan(issue.Key, issue.Generation, TaskRunning, TaskPlace{Kind: PlaceStep, Position: 2})
			putPlan(t, tx, v, tasks, steps)
		}
		must(t, records.StopTask(ctx, tx, stopped.Key, stopped.Generation, "a", "its owner stopped it"))
	})
	inTx(t, st, func(tx pgx.Tx) {
		read, err := records.Issue(ctx, tx, stopped.Key)
		must(t, err)
		if len(read.Running) != 0 {
			t.Errorf("%s, its task stopped, reads the running steps %+v, want none", stopped.Key, read.Running)
		}
		step, ok, err := records.StoppedStep(ctx, tx, *read)
		must(t, err)
		if want := (RunningStep{Task: "a", Place: TaskPlace{Kind: PlaceStep, Position: 2}, Role: "checker", Phase: phase.Step, Agent: "checker"}); !ok || step != want {
			t.Errorf("StoppedStep of %s = %+v (%t), want %+v", stopped.Key, step, ok, want)
		}
		other, err := records.Issue(ctx, tx, escalated.Key)
		must(t, err)
		if step, ok, err := records.StoppedStep(ctx, tx, *other); err != nil || ok {
			t.Errorf("StoppedStep of %s, held for an escalation with its task running, = %+v (%t, %v), want none", escalated.Key, step, ok, err)
		}
	})
}
