package record

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/plan"
)

// baseTask is a task of the plans the tests of a task's base read: its id, the ids it needs, the
// branch its first start gave it ("" for the issue's own) and the role of each of its steps.
type baseTask struct {
	id     string
	needs  []string
	branch string
	roles  []string
}

// basePlan is a plan of planIssue at generation 1 holding tasks, each running, each step typed as
// Legion's own roles type it (plan.BuiltinRoles).
func basePlan(tasks ...baseTask) StoredPlan {
	roles := plan.BuiltinRoles()
	stored := StoredPlan{Version: PlanVersion{Issue: planIssue, Generation: 1, Version: 1}, Steps: map[string][]PlanStep{}}
	for _, task := range tasks {
		stored.Tasks = append(stored.Tasks, PlanTask{Issue: planIssue, Generation: 1, Task: task.id, Needs: task.needs, Branch: task.branch, State: TaskRunning})
		for i, role := range task.roles {
			stored.Steps[task.id] = append(stored.Steps[task.id], PlanStep{Issue: planIssue, Generation: 1, Task: task.id, Position: i + 1, Role: role,
				Type: roles[role].Type})
		}
	}
	return stored
}

// The base of each task's pull request. A and B need nothing, C needs B, D needs A and C, and E,
// `tester` alone, needs D and r; r, `reviewer` alone, needs D. So E is the one task no task needs,
// the task that merges, and r is a task with a need and no step of the type implementation that does
// not merge. A, B and E are based on the default branch: two tasks that need none and the task that
// merges. C is based on its one need's branch, B's. D, which needs two tasks, is based on its join
// branch, its own branch and claim.JoinBranchSuffix, where the work of both is joined. r is based on
// the default branch: its pull request shows the joined work of its needs, which its review reads.
// The one task of a plan of one is based on the default branch.
//
// The fault it catches is D's pull request against A's branch, which shows C's work as D's (its
// control: BaseOf answering the first need's branch).
func TestTheBaseOfATask(t *testing.T) {
	issue := Issue{Key: planIssue, DefaultBranch: "main"}
	stored := basePlan(
		baseTask{"a", nil, "", []string{"implementer", "reviewer"}},
		baseTask{"b", nil, "legion/LEGION-208.b", []string{"implementer", "reviewer"}},
		baseTask{"c", []string{"b"}, "legion/LEGION-208.c", []string{"implementer", "reviewer"}},
		baseTask{"d", []string{"a", "c"}, "legion/LEGION-208.d", []string{"implementer", "reviewer"}},
		baseTask{"e", []string{"d", "r"}, "legion/LEGION-208.e", []string{"tester"}},
		baseTask{"r", []string{"d"}, "legion/LEGION-208.r", []string{"reviewer"}})
	for _, tc := range []struct {
		task, base string
		join       bool
	}{
		{"a", "main", false},
		{"b", "main", false},
		{"c", "legion/LEGION-208.b", false},
		{"d", "legion/LEGION-208.d.join", true},
		{"e", "main", false},
		{"r", "main", false},
	} {
		if base, join, err := stored.BaseOf(issue, tc.task); err != nil || base != tc.base || join != tc.join {
			t.Errorf("BaseOf(%s) = %s, %t, %v, want %s, %t", tc.task, base, join, err, tc.base, tc.join)
		}
	}

	one := basePlan(baseTask{"a", nil, "", []string{"implementer", "reviewer"}})
	if base, join, err := one.BaseOf(issue, "a"); err != nil || base != "main" || join {
		t.Errorf("BaseOf(a) of a plan of one task = %s, %t, %v, want main, false", base, join, err)
	}
}

// The base of a task the plan does not hold, and of a task with one need the plan does not hold, is
// an error naming it, never the default branch: plan.Validate refuses such a plan, so a plan that
// holds one is a defect, and the default branch would be a silent wrong base. Of A, which needs
// nothing, C, which needs B, and z, which needs C, the base of x is an error naming x, and the base
// of C, whose one need B the plan does not hold, an error naming B.
//
// The fault it catches is a wrong base answered with no error (its control: BaseOf answering the
// default branch for both, as it did).
func TestTheBaseOfATaskOrANeedThePlanDoesNotHoldIsAnError(t *testing.T) {
	issue := Issue{Key: planIssue, DefaultBranch: "main"}
	stored := basePlan(
		baseTask{"a", nil, "", []string{"implementer"}},
		baseTask{"c", []string{"b"}, "legion/LEGION-208.c", []string{"implementer"}},
		baseTask{"z", []string{"c"}, "legion/LEGION-208.z", []string{"implementer"}})
	for _, tc := range []struct{ task, names string }{
		{"x", "task x"},
		{"c", "needs b"},
	} {
		base, join, err := stored.BaseOf(issue, tc.task)
		if err == nil || !strings.Contains(err.Error(), tc.names) {
			t.Errorf("BaseOf(%s) = %q, %t, %v, want an error naming %q", tc.task, base, join, err, tc.names)
		}
	}
}

// The branches each task's work starts from, and the base of its pull request, read a need whose
// work merged as the default branch, which holds that work. b and h are done at their merge, so
// GitHub can have deleted each one's branch then; c, d and g, which need b, came in a later
// version, so b is in no Merging. a is done at its last step, and its work did not merge. c needs
// b, d needs a and b, e needs a, g needs b and h, and z, which needs c, d, e and g, is the task that
// merges; each has an implementer. c starts from main alone, which holds b's work, and is based on
// main. d starts from a's branch and then main, and is based on its join branch, where the two are
// joined. g, both of whose needs merged, starts from main once, and is based on it. e starts from
// a's branch and is based on it. BaseOf answers what PlannedBase answers. Where d's block says
// merges, d is based on main, and it starts from the same branches.
//
// The fault it catches is a task that starts from a merged need's branch (its controls: PlannedBase
// as BaseOf was before, under which c is based on b's branch; a need read as merged where it is in
// Merging and done, under which c starts from b's branch).
func TestTheBaseOfATaskReadsAMergedNeedAsTheDefaultBranch(t *testing.T) {
	issue := Issue{Key: planIssue, DefaultBranch: "main"}
	implementer := []string{"implementer"}
	stored := basePlan(
		baseTask{"a", nil, "legion/LEGION-208.a", []string{"implementer", "reviewer"}},
		baseTask{"b", nil, "legion/LEGION-208.b", implementer},
		baseTask{"c", []string{"b"}, "legion/LEGION-208.c", implementer},
		baseTask{"d", []string{"a", "b"}, "legion/LEGION-208.d", implementer},
		baseTask{"e", []string{"a"}, "legion/LEGION-208.e", implementer},
		baseTask{"g", []string{"b", "h"}, "legion/LEGION-208.g", implementer},
		baseTask{"h", nil, "legion/LEGION-208.h", implementer},
		baseTask{"z", []string{"c", "d", "e", "g"}, "legion/LEGION-208.z", implementer})
	for i, row := range stored.Tasks {
		switch row.Task {
		case "a":
			stored.Tasks[i].State, stored.Tasks[i].Place = TaskDone, TaskPlace{Kind: PlaceStep, Position: 2}
		case "b", "h":
			stored.Tasks[i].State, stored.Tasks[i].Place = TaskDone, TaskPlace{Kind: PlaceMerge}
		}
	}
	if merging := stored.Merging(); !slices.Equal(merging, []string{"z"}) {
		t.Fatalf("Merging = %q, want z alone: c, d and g need b", merging)
	}
	a := StartBranch{Branch: "legion/LEGION-208.a", Holds: []string{"a"}}
	main := func(needs ...string) StartBranch { return StartBranch{Branch: "main", Holds: needs} }
	type want struct {
		task   string
		starts []StartBranch
		base   string
		join   bool
	}
	check := func(t *testing.T, stored StoredPlan, tc want) {
		t.Helper()
		if starts, err := stored.StartsFrom(issue, tc.task); err != nil || !reflect.DeepEqual(starts, tc.starts) {
			t.Errorf("StartsFrom(%s) = %v, %v, want %v", tc.task, starts, err, tc.starts)
		}
		if base, join, err := stored.PlannedBase(issue, tc.task); err != nil || base != tc.base || join != tc.join {
			t.Errorf("PlannedBase(%s) = %s, %t, %v, want %s, %t", tc.task, base, join, err, tc.base, tc.join)
		}
		if base, join, err := stored.BaseOf(issue, tc.task); err != nil || base != tc.base || join != tc.join {
			t.Errorf("BaseOf(%s) = %s, %t, %v, want %s, %t", tc.task, base, join, err, tc.base, tc.join)
		}
	}
	for _, tc := range []want{
		{"c", []StartBranch{main("b")}, "main", false},
		{"d", []StartBranch{a, main("b")}, "legion/LEGION-208.d.join", true},
		{"g", []StartBranch{main("b", "h")}, "main", false},
		{"e", []StartBranch{a}, "legion/LEGION-208.a", false},
	} {
		check(t, stored, tc)
	}

	for i, row := range stored.Tasks {
		if row.Task == "d" {
			stored.Tasks[i].Merges = true
		}
	}
	check(t, stored, want{"d", []StartBranch{a, main("b")}, "main", false})
}

// The base of a task that has started is the base its start fixed (PlanTask.Base), whether or not
// the plan in force merges it: of d, which needs a and c, holding the base legion/LEGION-208.d.join,
// BaseOf answers that, join true, with d needed by e and with no task needing it, where the plan
// alone gives main. With no base fixed, a task that has not started or that a release before
// started, it answers the plan's base (PlannedBase).
//
// The fault it catches is a base that a later version moves within a task's run (its control: BaseOf
// as PlannedBase alone, the revision before, under which d without e reads main).
func TestTheBaseOfAStartedTaskIsTheBaseItsStartFixed(t *testing.T) {
	issue := Issue{Key: planIssue, DefaultBranch: "main"}
	for _, withE := range []bool{true, false} {
		tasks := []baseTask{{"a", nil, "", []string{"implementer"}}, {"c", nil, "legion/LEGION-208.c", []string{"implementer"}},
			{"d", []string{"a", "c"}, "legion/LEGION-208.d", []string{"implementer", "reviewer"}}}
		if withE {
			tasks = append(tasks, baseTask{"e", []string{"d"}, "legion/LEGION-208.e", []string{"implementer"}})
		}
		stored := basePlan(tasks...)
		planned, _, err := stored.PlannedBase(issue, "d")
		if err != nil {
			t.Fatal(err)
		}
		for i, row := range stored.Tasks {
			if row.Task == "d" {
				stored.Tasks[i].Base = "legion/LEGION-208.d.join"
			}
		}
		if base, join, err := stored.BaseOf(issue, "d"); err != nil || base != "legion/LEGION-208.d.join" || !join {
			t.Errorf("BaseOf(d) with e %t and the base its start fixed = %s, %t, %v, want legion/LEGION-208.d.join, true", withE, base, join, err)
		}
		for i, row := range stored.Tasks {
			if row.Task == "d" {
				stored.Tasks[i].Base = ""
			}
		}
		if base, _, err := stored.BaseOf(issue, "d"); err != nil || base != planned {
			t.Errorf("BaseOf(d) with e %t and no base fixed = %s, %v, want the plan's base %s", withE, base, err, planned)
		}
	}
}

// Each part starts from its task's branch, which holds the joined work of its task's needs,
// whatever else it needs, and is based where that branch makes its pull request show its own work
// alone. f names a sub-owner and no step, and needs nothing; g is a task of the top level; each
// other task is a part of f. f1 (`implementer` and `reviewer`) and f3 (`reviewer` alone) need
// nothing, f2 (`implementer`) needs f1, f4 (`reviewer` alone) and f5 (`implementer`) need f1 and
// f2, f6 (`implementer`) needs nothing and its block says merges, and f7 (`implementer`) needs g,
// a task outside f. Every part starts from f's branch first (StartsFrom), named by f's id, then
// from the branches of its own needs. f, g and f6 are based on the default branch: f and f6 merge,
// and g needs nothing at the top level. f1 is based on f's branch, its one start, and so are f3
// and f4, none of whose steps implements, whose pull requests show the joined work of their needs
// against that branch. f2, f5 and f7, which implement and start from more than one branch, are
// based on their join branches. BaseOf answers what PlannedBase answers.
//
// The fault it catches is a part that starts without its task's joined work: a part that merges
// and needs nothing started at the default branch, and a part that needs a task outside its task
// started at that need's branch (its control: StartsFrom that lists a part's own needs alone,
// which fails every part's row). The base of a part none of whose steps implements, f3 and f4, is
// read from the plan's PartOf, never from where StartsFrom lists its task's branch (its control:
// StartsFrom that lists that branch last, under which only the StartsFrom rows fail; a base read
// from the first branch StartsFrom lists makes f3's and f4's base main there).
func TestTheBaseOfATaskThatIsAPart(t *testing.T) {
	issue := Issue{Key: planIssue, DefaultBranch: "main"}
	stored := basePlan(
		baseTask{"f", nil, "legion/LEGION-208.f", nil},
		baseTask{"f1", nil, "legion/LEGION-208.f1", []string{"implementer", "reviewer"}},
		baseTask{"f2", []string{"f1"}, "legion/LEGION-208.f2", []string{"implementer"}},
		baseTask{"f3", nil, "legion/LEGION-208.f3", []string{"reviewer"}},
		baseTask{"f4", []string{"f1", "f2"}, "legion/LEGION-208.f4", []string{"reviewer"}},
		baseTask{"f5", []string{"f1", "f2"}, "legion/LEGION-208.f5", []string{"implementer"}},
		baseTask{"f6", nil, "legion/LEGION-208.f6", []string{"implementer"}},
		baseTask{"f7", []string{"g"}, "legion/LEGION-208.f7", []string{"implementer"}},
		baseTask{"g", nil, "legion/LEGION-208.g", []string{"implementer"}})
	for i, row := range stored.Tasks {
		switch row.Task {
		case "f":
			stored.Tasks[i].OwnerRole = "architect"
		case "g":
		case "f6":
			stored.Tasks[i].PartOf, stored.Tasks[i].Merges = "f", true
		default:
			stored.Tasks[i].PartOf = "f"
		}
	}
	ofF := StartBranch{Branch: "legion/LEGION-208.f", Holds: []string{"f"}, OfTask: true}
	f1 := StartBranch{Branch: "legion/LEGION-208.f1", Holds: []string{"f1"}}
	f2 := StartBranch{Branch: "legion/LEGION-208.f2", Holds: []string{"f2"}}
	for _, tc := range []struct {
		task, base string
		join       bool
		starts     []StartBranch
	}{
		{"f", "main", false, nil},
		{"f1", "legion/LEGION-208.f", false, []StartBranch{ofF}},
		{"f2", "legion/LEGION-208.f2.join", true, []StartBranch{ofF, f1}},
		{"f3", "legion/LEGION-208.f", false, []StartBranch{ofF}},
		{"f4", "legion/LEGION-208.f", false, []StartBranch{ofF, f1, f2}},
		{"f5", "legion/LEGION-208.f5.join", true, []StartBranch{ofF, f1, f2}},
		{"f6", "main", false, []StartBranch{ofF}},
		{"f7", "legion/LEGION-208.f7.join", true, []StartBranch{ofF, {Branch: "legion/LEGION-208.g", Holds: []string{"g"}}}},
		{"g", "main", false, nil},
	} {
		planned, plannedJoin, err := stored.PlannedBase(issue, tc.task)
		if err != nil || planned != tc.base || plannedJoin != tc.join {
			t.Errorf("PlannedBase(%s) = %s, %t, %v, want %s, %t", tc.task, planned, plannedJoin, err, tc.base, tc.join)
		}
		if base, join, err := stored.BaseOf(issue, tc.task); err != nil || base != tc.base || join != tc.join {
			t.Errorf("BaseOf(%s) = %s, %t, %v, want %s, %t", tc.task, base, join, err, tc.base, tc.join)
		}
		if starts, err := stored.StartsFrom(issue, tc.task); err != nil || !reflect.DeepEqual(starts, tc.starts) {
			t.Errorf("StartsFrom(%s) = %+v, %v, want %+v", tc.task, starts, err, tc.starts)
		}
	}

	// A part whose task the plan does not hold is a defect plan.Validate refuses, so its base is an
	// error naming that task, never the default branch.
	orphan := basePlan(baseTask{"g1", nil, "legion/LEGION-208.g1", []string{"implementer"}})
	orphan.Tasks[0].PartOf = "g"
	if base, _, err := orphan.BaseOf(issue, "g1"); err == nil || !strings.Contains(err.Error(), "g1") || !strings.Contains(err.Error(), " g,") {
		t.Errorf("BaseOf(g1), a part of a task g the plan does not hold, = %s, %v, want an error naming g1 and g", base, err)
	}
}
