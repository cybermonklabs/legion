package intake

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/plan"
)

// The refusals of register_plan and stop_task that the workflow answers, each with its exact
// words: the route and the workflow name them through these constructors, so an owner reads the
// same text whichever of the two refused, and a change of a word is a change of this test.
func TestThePlanRefusalsSayWhy(t *testing.T) {
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
		{"a task that is done", TaskDone("LEGION-208", "a"), Refusal{Status: http.StatusConflict, Code: "TASK_DONE",
			Message: "task a of LEGION-208 is done, so nothing of it is left to stop. This stop changed nothing."}},
		{"a task that left the workflow with its issue", TaskLeft("LEGION-208", "a"), Refusal{Status: http.StatusConflict, Code: "TASK_DONE",
			Message: "task a of LEGION-208 left the workflow with its issue before it was done, so nothing of it is left to stop. This stop changed nothing."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got == nil || !reflect.DeepEqual(*tc.got, tc.want) {
				t.Fatalf("refusal = %+v, want %+v", tc.got, tc.want)
			}
		})
	}
	// Only a refused plan carries reasons: every other refusal is a code and a sentence, which is
	// what tells the route to write the plain failure for it.
	if got := PlanVersionOld("LEGION-208", 3, 2).Reasons; got != nil {
		t.Fatalf("PLAN_VERSION_OLD carries reasons %+v, want none", got)
	}
}
