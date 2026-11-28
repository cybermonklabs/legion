package record

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// Running names each running task's act and the role whose agent works it, and no task that
// waits: at a step of a role the deployment defines, which runs as step, the role its step names.
// A running task whose place no role works is an error, so no answer names a running task with no
// role: a step its steps do not hold, and no act at all. The fault is a role read from the phase
// alone (phase.RoleFor), which names no role at step.
func TestRunningNamesTheRoleOfEachRunningActAndRefusesAPlaceNoRoleWorks(t *testing.T) {
	steps := []PlanStep{{Task: "a", Position: 1, Role: "implementer"}, {Task: "a", Position: 2, Role: "reviewer"}}
	checked := []PlanStep{{Task: "a", Position: 1, Role: "implementer"}, {Task: "a", Position: 2, Role: "checker"}}
	at := func(place TaskPlace, steps []PlanStep) *StoredPlan {
		return &StoredPlan{
			Tasks: []PlanTask{{Issue: planIssue, Task: "a", State: TaskRunning, Place: place}, {Issue: planIssue, Task: "b", State: TaskWaiting}},
			Steps: map[string][]PlanStep{"a": steps},
		}
	}
	for _, tc := range []struct {
		place TaskPlace
		steps []PlanStep
		role  claim.Role
	}{
		{TaskPlace{Kind: PlaceStep, Position: 2}, steps, claim.RoleReviewer},
		{TaskPlace{Kind: PlaceRetro}, steps, claim.RoleImplementer},
		{TaskPlace{Kind: PlaceMerge}, steps, claim.RoleMerger},
		{TaskPlace{Kind: PlaceStep, Position: 2}, checked, "checker"},
	} {
		got, err := at(tc.place, tc.steps).Running()
		if want := []RunningStep{{Task: "a", Place: tc.place, Role: tc.role}}; err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("Running at %+v = %+v, %v; want %+v", tc.place, got, err, want)
		}
	}
	for _, tc := range []struct {
		name  string
		place TaskPlace
		steps []PlanStep
	}{
		{"a step its steps do not hold", TaskPlace{Kind: PlaceStep, Position: 3}, steps},
		{"no act", TaskPlace{}, steps},
	} {
		if got, err := at(tc.place, tc.steps).Running(); err == nil || !strings.Contains(err.Error(), "task a of "+planIssue) {
			t.Errorf("Running of a task running at %s = %+v, %v; want an error naming task a of %s", tc.name, got, err, planIssue)
		}
	}
}
