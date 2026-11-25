package workflow

import (
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A part whose join of its task's branch GitHub refused is told that conflict with its task's
// branch. f1, a part of f, needs g, a task of the top level, so it starts from f's branch and then
// g's (record.StoredPlan.StartsFrom), joined on its join branch, and its row holds the conflict of
// f, the task whose work f's branch holds, beside g's (record.PlanTask.JoinConflicts). Its first
// step is told both, each with the branch that holds its work.
//
// The fault it catches is a reader that takes the ids a start branch names for the task's needs, so
// it drops f, which the part does not need (its control: planToldOf that tells a conflict only of
// a need of the task, under which f1 is told g alone).
func TestAPartIsToldTheConflictOfItsTasksBranch(t *testing.T) {
	issue := record.Issue{Key: "LEGION-208", DefaultBranch: "main"}
	implementer := func(task string) []record.PlanStep {
		return []record.PlanStep{{Issue: issue.Key, Task: task, Position: 1, Role: "implementer", Type: plan.TypeImplementation}}
	}
	stored := &record.StoredPlan{
		Tasks: []record.PlanTask{
			{Issue: issue.Key, Task: "f", OwnerRole: "architect", Branch: "legion/LEGION-208.f", State: record.TaskRunning},
			{Issue: issue.Key, Task: "f1", PartOf: "f", Needs: []string{"g"}, Branch: "legion/LEGION-208.f1", State: record.TaskRunning,
				JoinConflicts: []string{"f", "g"}},
			{Issue: issue.Key, Task: "g", Branch: "legion/LEGION-208.g", State: record.TaskRunning},
		},
		Steps: map[string][]record.PlanStep{"f1": implementer("f1"), "g": implementer("g")},
	}
	told, err := planToldOf(issue, stored, "f1")
	if err != nil {
		t.Fatalf("planToldOf(f1): %v", err)
	}
	want := planTold{base: "legion/LEGION-208.f1.join", join: "legion/LEGION-208.f1.join", conflicts: []joinConflict{
		{task: "f", branch: "legion/LEGION-208.f"}, {task: "g", branch: "legion/LEGION-208.g"}}}
	if !reflect.DeepEqual(told, want) {
		t.Fatalf("planToldOf(f1) = %+v, want %+v", told, want)
	}
}
