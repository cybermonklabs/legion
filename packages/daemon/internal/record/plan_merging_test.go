package record

import (
	"slices"
	"testing"
)

// Which tasks of a stored plan merge is the one rule of the plan (plan.Merging), read over each
// task that is not left, as its row gives it. A left task, whose issue left the workflow before it
// was done, neither merges nor needs: a need that only it names keeps no task from merging. So in
// the plan of a (done), b (left, needing a) and c (waiting), a and c merge; with b waiting, b and
// c merge, and a, which b needs, does not.
//
// The fault it catches is a left task's need keeping the task it needs from merging (its control:
// the rule read over every row, under which the first plan gives [b c]).
func TestTheMergeRuleReadsEachTaskThatIsNotLeft(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    TaskState
		want []string
	}{
		{"b left", TaskLeft, []string{"a", "c"}},
		{"b waiting", TaskWaiting, []string{"b", "c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored := StoredPlan{Tasks: []PlanTask{
				{Task: "a", State: TaskDone},
				{Task: "b", State: tc.b, Needs: []string{"a"}},
				{Task: "c", State: TaskWaiting},
			}}
			if got := stored.Merging(); !slices.Equal(got, tc.want) {
				t.Fatalf("Merging = %q, want %q", got, tc.want)
			}
		})
	}
}

// Whether a task's work merged is a fact its row holds: the task is done at its merge. So a plan
// of b, done at its merge, and f, waiting and needing b, says b's work merged, though f's need takes
// b out of the tasks that will merge, and the tasks that have a merge, done or to come, are b and f.
// A task done at its last step did not merge, and neither did a task running at its merge or one
// left there, nor a task the plan does not hold.
//
// The fault it catches is "its work merged" read from the rule of the plan in force, which a version
// that needs the merged task changes (its control: a task merged when it is in Merging and done,
// under which b's work did not merge).
func TestAMergedTaskStaysMergedWhenAVersionNeedsIt(t *testing.T) {
	merge := TaskPlace{Kind: PlaceMerge}
	stored := StoredPlan{Tasks: []PlanTask{
		{Task: "b", State: TaskDone, Place: merge},
		{Task: "f", State: TaskWaiting, Needs: []string{"b"}},
	}}
	if !stored.WorkMerged("b") {
		t.Fatalf("WorkMerged(b), done at its merge = false, want true")
	}
	if got := stored.Merging(); slices.Contains(got, "b") {
		t.Fatalf("Merging = %q, want no b: f needs it", got)
	}
	if got, want := stored.TasksThatMerge(), []string{"b", "f"}; !slices.Equal(got, want) {
		t.Fatalf("TasksThatMerge = %q, want %q: b, whose work merged, and f, which will merge", got, want)
	}
	for _, tc := range []struct {
		name string
		row  PlanTask
	}{
		{"done at its last step", PlanTask{Task: "a", State: TaskDone, Place: TaskPlace{Kind: PlaceStep, Position: 2}}},
		{"running at its merge", PlanTask{Task: "a", State: TaskRunning, Place: merge}},
		{"left at its merge", PlanTask{Task: "a", State: TaskLeft, Place: merge}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if (StoredPlan{Tasks: []PlanTask{tc.row}}).WorkMerged("a") {
				t.Fatalf("WorkMerged(a), %s = true, want false", tc.name)
			}
		})
	}
	if stored.WorkMerged("z") {
		t.Fatalf("WorkMerged(z), a task the plan does not hold = true, want false")
	}
}

// A task that its issue's leave wrote left still has the merge it stood to have: a person parked the
// issue, and the pull request of a task can merge after that, by a person's merge or a merge queue's,
// or have merged before it, before a later leave sets the issue done. So the tasks that have a merge
// read each left row as it stood before the leave, with its needs and its merges, beside each task
// whose work merged, while the tasks that will merge (Merging) still read no left row. Of a child of
// one task c, parked at its review, the task with a merge is c, the one task that merges, and no
// task will merge. Of a, left at its first step, and b, which needs a, left at its retro, the task
// with a merge is b, as the plan stood. A task left at its merge has one too.
//
// The fault it catches is a parked issue's merge that no later leave at done checks in production
// (its controls: the tasks that have a merge read as the tasks that will merge and those whose work
// merged, under which the child of one task has none; and a left task counted only where it stands
// at its merge, under which the same).
func TestTasksThatMergeReadsALeftRowAsItStoodBeforeItsLeave(t *testing.T) {
	for _, tc := range []struct {
		name          string
		tasks         []PlanTask
		have, merging []string
	}{
		{"a child of one task parked at its review", []PlanTask{{Task: "c", State: TaskLeft, Place: TaskPlace{Kind: PlaceStep, Position: 3}}},
			[]string{"c"}, nil},
		{"b needs a, each left", []PlanTask{
			{Task: "a", State: TaskLeft, Place: TaskPlace{Kind: PlaceStep, Position: 1}},
			{Task: "b", State: TaskLeft, Needs: []string{"a"}, Place: TaskPlace{Kind: PlaceRetro}},
		}, []string{"b"}, nil},
		{"a task left at its merge", []PlanTask{{Task: "m", State: TaskLeft, Place: TaskPlace{Kind: PlaceMerge}}},
			[]string{"m"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored := StoredPlan{Tasks: tc.tasks}
			if got := stored.TasksThatMerge(); !slices.Equal(got, tc.have) {
				t.Fatalf("TasksThatMerge = %q, want %q", got, tc.have)
			}
			if got := stored.Merging(); !slices.Equal(got, tc.merging) {
				t.Fatalf("Merging = %q, want %q: it reads no left row", got, tc.merging)
			}
		})
	}
	parked := StoredPlan{Tasks: []PlanTask{{Task: "c", State: TaskLeft, Place: TaskPlace{Kind: PlaceStep, Position: 3}}}}
	if got, ok := parked.OnlyTaskThatMerges(); got != "c" || !ok {
		t.Fatalf("OnlyTaskThatMerges of a child of one task parked at its review = %q, %v, want c", got, ok)
	}
}
