package admit

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// ownersVersion is register_plan's fact for key at generation 1: version of pathsArtifact, its one
// task a with text and the steps implementer and reviewer, given by the architect of pathsRoot, the
// owner of every issue of these tests, as the route records it.
func ownersVersion(t *testing.T, key string, version int, text string) intake.PlanGiven {
	t.Helper()
	return intake.PlanGiven{Issue: key, Generation: 1, GivenBy: architectClaim(t, pathsRoot), Plan: plan.Plan{ArtifactID: pathsArtifact, Version: version,
		Tasks: []plan.Task{{ID: "a", Text: text, Steps: []plan.Step{{Role: "implementer"}, {Role: "reviewer"}}}}}}
}

// applyOwnersCall applies fact as the route of an owner's call applies it, and fails unless the
// workflow took it: no refusal, and spent. Admission runs its pass after it where the pass rule
// (Admission.Apply) gives one.
func applyOwnersCall(t *testing.T, pool *pgxpool.Pool, admission *Admission, id string, fact intake.Fact) {
	t.Helper()
	result, err := intake.ApplyFact(context.Background(), pool, "api", id, fact, admission.engine, admission)
	if err != nil || result.Refusal != nil || result.Unspent || result.Duplicate {
		t.Fatalf("apply %s = %+v (%v), want it taken", id, result, err)
	}
}

// taskOf is the one task of key's plan at generation 1, which the test fails without.
func taskOf(t *testing.T, pool *pgxpool.Pool, key string) record.PlanTask {
	t.Helper()
	stored := storedPlanAt(t, pool, key, 1)
	if stored == nil || len(stored.Tasks) != 1 {
		t.Fatalf("the plan of %s = %+v, want one task", key, stored)
	}
	return stored.Tasks[0]
}

// implementerEntries is the phase entry of each start of key's implementer, oldest first.
func implementerEntries(t *testing.T, pool *pgxpool.Pool, key string) []int64 {
	t.Helper()
	var entries []int64
	for _, got := range effects(t, pool) {
		if request, ok := got.payload.(record.SuperviseRequest); ok && got.issue == key && request.Op == "start" && request.Role == claim.RoleImplementer {
			entries = append(entries, request.Entry)
		}
	}
	return entries
}

// stopWaitingTask has key take its owner's version 1 before its first step, and then has the owner
// stop task a there (stop_task). It fails unless key then stands held from admitted for the stopped
// task, task a stopped at no act, and no worker of key started.
func stopWaitingTask(t *testing.T, pool *pgxpool.Pool, admission *Admission, key string) {
	t.Helper()
	applyOwnersCall(t, pool, admission, "plan-v1", ownersVersion(t, key, 1, "Build the widget."))
	applyOwnersCall(t, pool, admission, "stop", intake.TaskStopped{Issue: key, Task: "a", By: architectClaim(t, pathsRoot), Generation: 1})
	got, task := issue(t, pool, key), taskOf(t, pool, key)
	if got.Phase != phase.Held || got.Hold == nil || *got.Hold != (record.Hold{From: phase.Admitted, Reason: record.HoldTaskStopped}) ||
		task.State != record.TaskStopped || task.Place != (record.TaskPlace{}) {
		t.Fatalf("the premise: %s stands in %s (hold %+v), its task a %s at %+v, want it held from admitted for task a, stopped at no act",
			key, got.Phase, got.Hold, task.State, task.Place)
	}
	if starts := workerStarts(t, pool, key); len(starts) != 0 {
		t.Fatalf("the premise: the worker starts of %s = %v, want none before its first step", key, starts)
	}
}

// assertWaitsForItsFirstStep fails unless key stands at admitted with no hold, its task a waiting at
// no act with no stop reason and no pass counted, and no worker of key started: what is still to
// open (when) holds its first step.
func assertWaitsForItsFirstStep(t *testing.T, pool *pgxpool.Pool, key, when string) {
	t.Helper()
	got, task := issue(t, pool, key), taskOf(t, pool, key)
	if got.Phase != phase.Admitted || got.Hold != nil {
		t.Fatalf("%s %s stands in %s (hold %+v), want admitted with no hold, waiting for its first step", key, when, got.Phase, got.Hold)
	}
	if task.State != record.TaskWaiting || task.Place != (record.TaskPlace{}) || task.StoppedReason != "" || task.Counts.Pass != 0 {
		t.Fatalf("task a of %s %s = %+v, want it waiting at no act with no stop reason and no pass", key, when, task)
	}
	if starts := workerStarts(t, pool, key); len(starts) != 0 {
		t.Fatalf("the worker starts of %s %s = %v, want none", key, when, starts)
	}
}

// assertStartsItsFirstStepOnce fails unless key stands in implementing, the first step of its plan,
// with no hold, its task a running at step 1 with no pass counted (the first start of its work, not
// a restart), and the implementer's start of generation 1 is the one worker start key has had.
func assertStartsItsFirstStepOnce(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	got, task := issue(t, pool, key), taskOf(t, pool, key)
	if got.Phase != phase.Implementing || got.Hold != nil {
		t.Fatalf("%s stands in %s (hold %+v), want implementing, the first step of its plan", key, got.Phase, got.Hold)
	}
	if task.State != record.TaskRunning || task.Place != (record.TaskPlace{Kind: record.PlaceStep, Position: 1}) || task.Counts.Pass != 0 {
		t.Fatalf("task a of %s = %+v, want it running at step 1 with no pass counted", key, task)
	}
	if got, want := workerStarts(t, pool, key), []string{"implementer/implementing@1"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("the worker starts of %s = %v, want %v, once", key, got, want)
	}
}

// A task stopped before its first step went out never ran, so neither a later version of its plan
// that changes it nor a person's todo on its root starts that step: each ends the stop, and the task
// goes back the way a task that was never stopped goes into its first step, where the design gate,
// the waiting line and admission decide when it starts. A root whose design gate is closed stays at
// admitted with no worker started, and the approval that opens the gate starts its first step,
// once. A child that waits on an unfinished Dispatch issue stays at admitted with no worker started,
// and the close of that issue starts its first step, once. The control is a task whose work ran: it
// is stopped at its first step, and a later version that changes it starts it again there at once,
// its pass raised, as a stopped task restarts.
func TestATaskStoppedBeforeItsFirstStepWaitsForWhatStartsThatStep(t *testing.T) {
	const blocker = "LEGION-BLOCKER"
	openGate := intake.GateRegistered{Issue: pathsRoot, ArtifactID: "artifact-root", Version: 1, ApprovedVersion: 1,
		ApprovedMark: record.EventMark{Seq: 9, At: reviewedAt}}
	for _, end := range []struct {
		name string
		end  func(*testing.T, *pgxpool.Pool, *Admission)
	}{
		{"a later version that changes the task", func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
			applyOwnersCall(t, pool, admission, "plan-v2", ownersVersion(t, pathsRoot, 2, "Build the widget, and its manual."))
		}},
		{"a person's todo", func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
			apply(t, pool, admission, "root-todo", intake.DispatchIssue{Key: pathsRoot, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
				Status: "todo", Title: pathsRoot, Rank: "A", HandedOver: handed}, admission.engine)
		}},
	} {
		t.Run("a root whose design gate is closed, after "+end.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := quiet(t, 1)
			seedSlotted(t, pool, pathsRoot, "A")
			stopWaitingTask(t, pool, admission, pathsRoot)

			end.end(t, pool, admission)

			assertWaitsForItsFirstStep(t, pool, pathsRoot, "with its design gate closed")
			applyFact(t, pool, admission, "root-gate", openGate)
			assertStartsItsFirstStepOnce(t, pool, pathsRoot)
		})
	}

	t.Run("a child that waits on an unfinished issue, after a later version that changes the task", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 2)
		seedSlottedInPhase(t, pool, testProject, pathsRoot, "A", phase.Implementing, nil)
		seedGate(t, pool, record.DesignGate{Issue: pathsRoot, ArtifactID: "artifact-root", LatestVersion: 1, ApprovedVersion: new(1)})
		parent := pathsRoot
		putIssue(t, pool, record.Issue{Key: pathsChild, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: pathsRoot,
			Project: testProject, Title: "child", Parent: &parent, Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: "B", LastDispatchSeq: 1})
		seedMirrorChild(t, pool, pathsChild, "todo", pathsRoot, blocker)
		seedMirror(t, pool, blocker, "in_progress")
		stopWaitingTask(t, pool, admission, pathsChild)

		applyOwnersCall(t, pool, admission, "plan-v2", ownersVersion(t, pathsChild, 2, "Build the widget, and its manual."))

		assertWaitsForItsFirstStep(t, pool, pathsChild, "while the issue it waits on is open")
		applyMirrored(t, pool, admission, "blocker-done", intake.DispatchIssue{Key: blocker, ResolvedRepo: "acme/widgets", Seq: 5, Type: "issue.updated",
			Status: "done", Title: blocker, Rank: "Z"}, engineStub{})
		assertStartsItsFirstStepOnce(t, pool, pathsChild)
	})

	t.Run("control: a task whose work ran starts again at its first step at once", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 1)
		seedSlotted(t, pool, pathsRoot, "A")
		seedGate(t, pool, record.DesignGate{Issue: pathsRoot, ArtifactID: "artifact-root", LatestVersion: 1, ApprovedVersion: new(1)})
		applyOwnersCall(t, pool, admission, "plan-v1", ownersVersion(t, pathsRoot, 1, "Build the widget."))
		assertStartsItsFirstStepOnce(t, pool, pathsRoot)
		first := implementerEntries(t, pool, pathsRoot)
		applyOwnersCall(t, pool, admission, "stop", intake.TaskStopped{Issue: pathsRoot, Task: "a", By: architectClaim(t, pathsRoot), Generation: 1})
		if got := issue(t, pool, pathsRoot); got.Phase != phase.Held || got.Hold == nil || got.Hold.From != phase.Implementing {
			t.Fatalf("the premise: %s stands in %s (hold %+v), want it held from implementing for its stopped task", pathsRoot, got.Phase, got.Hold)
		}

		applyOwnersCall(t, pool, admission, "plan-v2", ownersVersion(t, pathsRoot, 2, "Build the widget, and its manual."))

		got, task := issue(t, pool, pathsRoot), taskOf(t, pool, pathsRoot)
		if got.Phase != phase.Implementing || got.Hold != nil {
			t.Fatalf("%s stands in %s (hold %+v) after version 2, want implementing at once, the first step of its plan", pathsRoot, got.Phase, got.Hold)
		}
		if task.State != record.TaskRunning || task.Place != (record.TaskPlace{Kind: record.PlaceStep, Position: 1}) || task.Counts.Pass != 1 {
			t.Fatalf("task a after version 2 = %+v, want it running at step 1 with its pass raised to 1", task)
		}
		if entries := implementerEntries(t, pool, pathsRoot); len(first) != 1 || len(entries) == 0 || entries[len(entries)-1] <= first[0] {
			t.Fatalf("the implementer's start entries = %v after version 2 (%v before the stop), want its newest start at a raised entry", entries, first)
		}
	})
}
