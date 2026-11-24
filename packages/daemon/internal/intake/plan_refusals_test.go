package intake

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// inPlanningB is an issue's row of the experiment planning, drawn into its arm b, whose issues run
// the project's default steps: the group the refusals below name.
var inPlanningB = record.Assignment{Experiment: "planning", Unit: "issue", Variant: "b", Mode: "random", Plan: "default"}

// The refusals of register_plan and stop_task that the workflow answers, each with its exact
// words: the route and the workflow name them through these constructors, so an owner reads the
// same text whichever of the two refused, and a change of a word is a change of this test.
func TestThePlanRefusalsSayWhy(t *testing.T) {
	accepting := func(starts int64) record.StepEntry {
		return record.StepEntry{Place: record.StepPlace{Task: "a", Kind: "step", Place: 2}, Starts: starts}
	}
	running := RunningTaskChanged("a")
	if want := (plan.Refusal{Task: "a", Reason: "task a is running: stop it with stop_task before you change it."}); running != want {
		t.Fatalf("RunningTaskChanged(a) = %+v, want %+v", running, want)
	}
	reasons := []plan.Refusal{
		{Reason: "the plan has 2 tasks (a, b), and this release runs one task for each issue. This release of Legion does not carry it out yet."},
		running,
	}
	for _, tc := range []struct {
		name string
		got  *Refusal
		want Refusal
	}{
		{"a refused plan", PlanRefused(reasons), Refusal{Status: http.StatusConflict, Code: "PLAN_REFUSED",
			Message: "Legion refused this plan and recorded nothing: the plan has 2 tasks (a, b), and this release runs one task for each issue. " +
				"This release of Legion does not carry it out yet. task a is running: stop it with stop_task before you change it.",
			Reasons: reasons}},
		{"a version at or below the one in force", PlanVersionOld("LEGION-208", 3, 2), Refusal{Status: http.StatusConflict, Code: "PLAN_VERSION_OLD",
			Message: "the plan in force of LEGION-208 is version 3, and Legion takes only a later version: version 2 changed nothing."}},
		{"a version of another document", PlanDocumentChanged("LEGION-208", "6a1f0c3e-2b7d-4e5a-9c81-3d4f5e6a7b80"), Refusal{Status: http.StatusConflict,
			Code: "PLAN_DOCUMENT_CHANGED", Message: "the plan in force of LEGION-208 is document 6a1f0c3e-2b7d-4e5a-9c81-3d4f5e6a7b80; give a later version of that document."}},
		{"a task the plan does not hold", TaskNotFound("LEGION-208", "b", []string{"a"}), Refusal{Status: http.StatusNotFound, Code: "TASK_NOT_FOUND",
			Message: "the plan Legion runs for LEGION-208 has no task b; its tasks are a. This stop changed nothing."}},
		{"a task of an issue with no plan rows", TaskNotFound("LEGION-208", "a", nil), Refusal{Status: http.StatusNotFound, Code: "TASK_NOT_FOUND",
			Message: "LEGION-208 has no plan of tasks in Legion's record, so it has no task a. This stop changed nothing."}},
		{"a stop of a task that is done", FinishedTask("LEGION-208", "a", record.TaskDone, StopOfTask), Refusal{Status: http.StatusConflict, Code: "TASK_DONE",
			Message: "task a of LEGION-208 is done, so nothing of it is left to stop. This stop changed nothing."}},
		{"a stop of a task that left the workflow with its issue", FinishedTask("LEGION-208", "a", record.TaskLeft, StopOfTask), Refusal{Status: http.StatusConflict, Code: "TASK_DONE",
			Message: "task a of LEGION-208 left the workflow with its issue before it was done, so nothing of it is left to stop. This stop changed nothing."}},
		{"a task of a root that waits on a person before its first step", TaskAwaitsPerson("LEGION-208", "a"), Refusal{Status: http.StatusConflict, Code: "TASK_AWAITING_PERSON",
			Message: "LEGION-208 waits on a person before its first step, and Legion cannot stop task a there. To change the task, give a later version of the plan with register_plan; " +
				"to stop it, call stop_task again once the person has answered. This stop changed nothing."}},
		{"a retry of a task its owner stopped", RetryOfStoppedTask("LEGION-208", "a", "task a of LEGION-208 was stopped by its owner"), Refusal{Status: http.StatusConflict, Code: "TASK_STOPPED",
			Message: "task a of LEGION-208 was stopped by its owner; a later version of the plan that changes the task starts it again at its first step (register_plan), " +
				"and a task that never started waits for its gate and its line first. This retry changed nothing."}},
		{"a retry of a task a failed step stopped", RetryOfStoppedTask("LEGION-208", "a", "step 2 (tester) failed and no earlier step of the type implementation can take the work"),
			Refusal{Status: http.StatusConflict, Code: "TASK_STOPPED", Message: "task a of LEGION-208 was stopped: step 2 (tester) failed and no earlier step of the type implementation can take the work; " +
				"a later version of the plan that changes the task starts it again at its first step (register_plan), and a task that never started waits for its gate and its line first. This retry changed nothing."}},
		{"a version that changes a task that is done", FinishedTask("LEGION-208", "a", record.TaskDone, ChangeOfTask), Refusal{Status: http.StatusConflict, Code: "TASK_DONE",
			Message: "task a of LEGION-208 is done, so nothing of it is left to change. This plan changed nothing."}},
		{"a version that changes a task that left the workflow", FinishedTask("LEGION-208", "a", record.TaskLeft, ChangeOfTask), Refusal{Status: http.StatusConflict, Code: "TASK_DONE",
			Message: "task a of LEGION-208 left the workflow with its issue before it was done, so nothing of it is left to change. This plan changed nothing."}},
		{"an issue that started before Legion took plans", PlanRefused([]plan.Refusal{StartedBeforePlans("LEGION-208")}), Refusal{Status: http.StatusConflict,
			Code: "PLAN_REFUSED", Message: "Legion refused this plan and recorded nothing: LEGION-208 started its work before Legion took plans, " +
				"so it runs the steps it started with to their end. This plan changed nothing.",
			Reasons: []plan.Refusal{{Reason: "LEGION-208 started its work before Legion took plans, so it runs the steps it started with to their end. This plan changed nothing."}}}},
		{"a plan given at a generation the issue has left", PlanStaleGeneration("LEGION-208", 1, 2), Refusal{Status: http.StatusConflict, Code: "PLAN_STALE_GENERATION",
			Message: "the plan was given for generation 1 of LEGION-208, and LEGION-208 is on generation 2 now. Give the plan again. This plan changed nothing."}},
		{"a plan of an issue whose group runs the default steps", PlanNotTaken("LEGION-208", inPlanningB), Refusal{Status: http.StatusConflict,
			Code: "PLAN_NOT_TAKEN", Message: "LEGION-208 is in the variant b of the experiment planning, whose issues run the project's default steps. " +
				"Legion takes no plan from its owner for this issue. This plan changed nothing."}},
		{"a stop of a task of an issue whose group runs the default steps", TaskNotStopped("LEGION-208", inPlanningB), Refusal{Status: http.StatusConflict,
			Code: "PLAN_NOT_TAKEN", Message: "LEGION-208 is in the variant b of the experiment planning, whose issues run the project's default steps. " +
				"Legion takes no plan from its owner for this issue, so its owner stops none of its tasks. This stop changed nothing."}},
		{"a stop asked at a generation the issue has left", TaskStaleGeneration("LEGION-208", 1, 2), Refusal{Status: http.StatusConflict, Code: "TASK_STALE_GENERATION",
			Message: "the stop was asked for generation 1 of LEGION-208, and LEGION-208 is on generation 2 now. Ask for the stop again if the task of this run must stop. This stop changed nothing."}},
		{"a request of an architect that is no architect of the issue's tree now", IssueOutsideTree("LEGION-209", "legion-legion-legion-210-architect", "stop_task"), Refusal{Status: http.StatusForbidden,
			Code: "ISSUE_OUTSIDE_TREE", Message: "LEGION-209 is outside the architect tree now: legion-legion-legion-210-architect is no architect of its tree, so stop_task changed nothing."}},
		{"a request of an architect that is not above the issue now", IssueNotOwned("sign_off", "LEGION-211", "legion-legion-legion-209-architect"), Refusal{Status: http.StatusForbidden,
			Code: "ISSUE_NOT_OWNED", Message: "sign_off of LEGION-211 belongs to its own architect or one above it in the tree, and legion-legion-legion-209-architect is neither now, so sign_off changed nothing."}},
		{"a sign-off asked at a generation the issue has left", SignOff{Issue: "LEGION-208", Generation: 1}.StaleGeneration(2), Refusal{Status: http.StatusConflict, Code: "SIGNOFF_STALE_GENERATION",
			Message: "the sign-off was asked for generation 1 of LEGION-208, and LEGION-208 is on generation 2 now. Read the issue again before you sign it off. This sign-off changed nothing."}},
		{"a sign-off asked of a run of accepting that is over", SignOff{Issue: "LEGION-208", Entry: accepting(1)}.StaleEntry(accepting(2)), Refusal{Status: http.StatusConflict,
			Code: "SIGNOFF_STALE_ENTRY", Message: "the sign-off was asked of run 1 of the step at place 2 of task a of LEGION-208, and LEGION-208 stands at run 2 of the step at place 2 of task a now. " +
				"Read the evidence of the run that stands before you sign it off. This sign-off changed nothing."}},
		{"a backward move asked at a generation the issue has left", BackwardMove{Issue: "LEGION-208", Generation: 1}.StaleGeneration(2), Refusal{Status: http.StatusConflict, Code: "BACKWARD_STALE_GENERATION",
			Message: "the backward move was asked for generation 1 of LEGION-208, and LEGION-208 is on generation 2 now. Read the issue again before you move it back. This move changed nothing."}},
		{"a backward move asked of a run of accepting that is over", BackwardMove{Issue: "LEGION-208", Entry: accepting(1)}.StaleEntry(record.StepEntry{}), Refusal{Status: http.StatusConflict,
			Code: "BACKWARD_STALE_ENTRY", Message: "the backward move was asked of run 1 of the step at place 2 of task a of LEGION-208, and LEGION-208 stands at no run of a step now. " +
				"Read the run that stands before you move it back. This move changed nothing."}},
		{"a retry_or_escalate asked at a generation the issue has left", RetryOrEscalate{Issue: "LEGION-208", Generation: 1}.StaleGeneration(2), Refusal{Status: http.StatusConflict, Code: "RETRY_STALE_GENERATION",
			Message: "the retry_or_escalate was asked for generation 1 of LEGION-208, and LEGION-208 is on generation 2 now. Read the issue again before you decide its hold. This decision changed nothing."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got == nil || !reflect.DeepEqual(*tc.got, tc.want) {
				t.Fatalf("refusal = %+v, want %+v", tc.got, tc.want)
			}
		})
	}
	// A task that is neither done nor left has its work still before it, so no act on it is
	// refused TASK_DONE.
	for _, state := range []record.TaskState{record.TaskWaiting, record.TaskRunning, record.TaskStopped} {
		for _, act := range []TaskAct{ChangeOfTask, StopOfTask} {
			if got := FinishedTask("LEGION-208", "a", state, act); got != nil {
				t.Fatalf("FinishedTask of a task %s = %+v, want no refusal", state, got)
			}
		}
	}
	// Only a refused plan carries reasons: every other refusal is a code and a sentence, which is
	// what tells the route to write the plain failure for it.
	if got := PlanVersionOld("LEGION-208", 3, 2).Reasons; got != nil {
		t.Fatalf("PLAN_VERSION_OLD carries reasons %+v, want none", got)
	}
}

func TestStaleEntryRefusalsNameARecordedCountZeroRun(t *testing.T) {
	entry := record.StepEntry{Place: record.StepPlace{Task: "a", Kind: "step", Place: 2}}
	for _, stale := range []struct {
		name string
		make func(record.StepEntry) *Refusal
	}{
		{"sign-off", func(given record.StepEntry) *Refusal {
			return SignOff{Issue: "LEGION-208", Entry: given}.StaleEntry(record.StepEntry{Place: entry.Place, Starts: 1})
		}},
		{"backward move", func(given record.StepEntry) *Refusal {
			return BackwardMove{Issue: "LEGION-208", Entry: given}.StaleEntry(record.StepEntry{Place: entry.Place, Starts: 1})
		}},
	} {
		t.Run(stale.name, func(t *testing.T) {
			refusal := stale.make(entry)
			if strings.Contains(refusal.Message, "no run of a step") {
				t.Fatalf("recorded count-zero entry described as absent: %s", refusal.Message)
			}
			if !strings.Contains(refusal.Message, "asked of run 0 of the step at place 2 of task a") {
				t.Fatalf("recorded count-zero entry is not named as its run: %s", refusal.Message)
			}
		})
	}
}

func TestStaleEntryRefusalsNameAZeroEntryAsNoRun(t *testing.T) {
	for _, stale := range []struct {
		name string
		make func(record.StepEntry) *Refusal
	}{
		{"sign-off", func(given record.StepEntry) *Refusal {
			return SignOff{Issue: "LEGION-208", Entry: given}.StaleEntry(record.StepEntry{Starts: 1})
		}},
		{"backward move", func(given record.StepEntry) *Refusal {
			return BackwardMove{Issue: "LEGION-208", Entry: given}.StaleEntry(record.StepEntry{Starts: 1})
		}},
	} {
		t.Run(stale.name, func(t *testing.T) {
			if refusal := stale.make(record.StepEntry{}); !strings.Contains(refusal.Message, "asked of no run of a step") {
				t.Fatalf("zero entry is not described as absent: %s", refusal.Message)
			}
		})
	}
}
