package record

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// The one task that merges, which a fact no row ties to a task is of, is one rule
// (StoredPlan.OnlyTaskThatMerges): the one task that has a merge (TasksThatMerge), whatever the
// plan's count of tasks, and none where no task, or more than one, has one. A task is read by its
// id through one lookup (StoredPlan.TaskOf), which a plan that is not there answers with no task.
func TestTheTaskThatMergesAndATaskByItsIdAreOneRuleEach(t *testing.T) {
	task := func(id string, needs ...string) PlanTask { return PlanTask{Task: id, Needs: needs} }
	says := PlanTask{Task: "a", Merges: true}
	for _, tc := range []struct {
		name  string
		tasks []PlanTask
		want  string
		ok    bool
	}{
		{"a plan of one task", []PlanTask{task("a")}, "a", true},
		{"a plan of one task that needs a task it does not hold", []PlanTask{task("a", "z")}, "a", true},
		{"b needs a", []PlanTask{task("a"), task("b", "a")}, "b", true},
		{"c needs a and b", []PlanTask{task("a"), task("b"), task("c", "a", "b")}, "c", true},
		{"two tasks no task needs", []PlanTask{task("a"), task("b")}, "", false},
		{"b needs a, which says merges", []PlanTask{says, task("b", "a")}, "", false},
		{"b needs a, which merged", []PlanTask{{Task: "a", State: TaskDone, Place: TaskPlace{Kind: PlaceMerge}}, task("b", "a")}, "", false},
		{"no task", nil, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := (&StoredPlan{Tasks: tc.tasks}).OnlyTaskThatMerges()
			if got != tc.want || ok != tc.ok {
				t.Fatalf("OnlyTaskThatMerges = %q, %v, want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
	stored := &StoredPlan{Tasks: []PlanTask{task("a"), task("b", "a")}}
	if got, ok := stored.TaskOf("b"); !ok || !reflect.DeepEqual(got, stored.Tasks[1]) {
		t.Fatalf("TaskOf(b) = %+v, %v, want task b", got, ok)
	}
	if got, ok := stored.TaskOf("c"); ok || !reflect.DeepEqual(got, PlanTask{}) {
		t.Fatalf("TaskOf(c), a task the plan does not hold = %+v, %v, want none", got, ok)
	}
	var none *StoredPlan
	if got, ok := none.TaskOf("a"); ok || !reflect.DeepEqual(got, PlanTask{}) {
		t.Fatalf("TaskOf(a) of no plan = %+v, %v, want none", got, ok)
	}
}

// A task stands where its place's phase is (at), and a held or a stopped task stands held from
// that phase: for its hold's reason, or for its stop (HoldTaskStopped). It is the one reading of a
// task's standing, which the workflow's copy of an issue read for one task and the completion
// route's position each take.
func TestATaskStandsAtItsPlaceOrHeldFromIt(t *testing.T) {
	for _, tc := range []struct {
		state  TaskState
		reason HoldReason
		want   phase.Phase
		hold   *Hold
	}{
		{TaskRunning, "", phase.Testing, nil},
		{TaskHeld, "", phase.Held, &Hold{From: phase.Testing}},
		{TaskHeld, HoldEscalated, phase.Held, &Hold{From: phase.Testing, Reason: HoldEscalated}},
		{TaskStopped, "", phase.Held, &Hold{From: phase.Testing, Reason: HoldTaskStopped}},
		{TaskWaiting, "", phase.Testing, nil},
	} {
		got, hold := TaskStanding(phase.Testing, tc.state, tc.reason)
		if got != tc.want || !reflect.DeepEqual(hold, tc.hold) {
			t.Fatalf("TaskStanding(testing, %s, %q) = %s, %+v, want %s, %+v", tc.state, tc.reason, got, hold, tc.want, tc.hold)
		}
	}
}

// An issue whose merge has no pull request yet has no pull request of its merge: of a, running with
// its pull request #41, and b, which needs a and so is the task that merges, waiting with no branch,
// the read of the merge answers none, not a's, and the read of the merges of several issues
// answers the others' and none of its, rather than failing for every issue it was asked with (the
// one rule, StoredPlan.TasksThatMerge).
func TestAnIssueWhoseMergeHasNoPullRequestHasNoMergePullRequest(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	issue, noPlan := issueFixture(planIssue), issueFixture("LEGION-209")
	a := taskPullRequest(planIssue, "a", "legion/LEGION-208", 41)
	alone := taskPullRequest(noPlan.Key, "", "legion/LEGION-209", 43)
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issue))
		must(t, records.PutIssue(ctx, tx, noPlan))
		v := PlanVersion{Issue: planIssue, Generation: issue.Generation, Version: 1, ArtifactID: planAuthor, Source: PlanSourceOwner,
			GivenBy: planOwner, GivenAt: planGivenAt}
		putPlan(t, tx, v, []PlanTask{
			{Issue: planIssue, Generation: issue.Generation, Task: "a", PlanVersion: 1, BlockText: "Build the widget.", State: TaskRunning},
			{Issue: planIssue, Generation: issue.Generation, Task: "b", PlanVersion: 1, BlockText: "Build the gadget.", Needs: []string{"a"},
				State: TaskWaiting},
		}, nil)
		if _, err := records.AssignTaskBranch(ctx, tx, planIssue, issue.Generation, "a"); err != nil {
			t.Fatalf("give task a its branch: %v", err)
		}
		must(t, records.PutPullRequest(ctx, tx, a))
		must(t, records.PutPullRequest(ctx, tx, alone))
	})
	inTx(t, st, func(tx pgx.Tx) {
		got, err := records.MergePullRequest(ctx, tx, planIssue)
		if err != nil || got != nil {
			t.Fatalf("the merge pull request of %s, whose merge is b's, which has none = %+v, %v, want none", planIssue, got, err)
		}
		merges, err := records.PullRequestsOf(ctx, tx, []string{planIssue, noPlan.Key})
		if err != nil || len(merges) != 1 || merges[noPlan.Key].Number != alone.Number {
			t.Fatalf("the merge pull requests of %s and %s = %+v, %v, want #%d of %s alone", planIssue, noPlan.Key, merges, err, alone.Number, noPlan.Key)
		}
	})
}
