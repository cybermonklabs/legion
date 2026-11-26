package record

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/store"
)

const (
	fixIssue = "LEGION-208"
	fixCheck = "k-ci"
	fixHeadX = "1111111111111111111111111111111111111111"
	fixHeadY = "2222222222222222222222222222222222222222"
)

// readingFix is fix n of task a of fixIssue at generation 3 (issueFixture's), as a fix is written at
// its start: reading its failed job, for fixCheck at fixHeadX, to run the task's first step again.
func readingFix(n int) TaskFix {
	return TaskFix{Issue: fixIssue, Generation: 3, Task: "a", Fix: n, State: FixReading, Check: fixCheck, Head: fixHeadX, Place: 1}
}

// fixesOf is the count of the fixes the store holds of issue, of any task and generation.
func fixesOf(t *testing.T, tx pgx.Tx, issue string) int {
	t.Helper()
	var n int
	if err := tx.QueryRow(context.Background(), `select count(*) from task_fixes where issue = $1`, issue).Scan(&n); err != nil {
		t.Fatalf("count the fixes of %s: %v", issue, err)
	}
	return n
}

// A task runs one fix at a time: PutFix refuses a second fix while the first reads its failed job
// and while it runs, and writes nothing, so the transaction goes on. The index task_fixes_one_open
// holds the same rule where no writer of the store asks: the same insert made by hand is refused.
// Once the first fix ended, the second is written, and its number is the one after the first: a
// number past it is refused.
func TestASecondOpenFixOfATaskIsRefused(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	records := NewStore()
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutFix(ctx, tx, readingFix(1)))
		if err := records.PutFix(ctx, tx, readingFix(2)); !errors.Is(err, ErrFixOpen) {
			t.Fatalf("PutFix of fix 2 while fix 1 reads its job = %v, want ErrFixOpen", err)
		}
		if n := fixesOf(t, tx, fixIssue); n != 1 {
			t.Fatalf("after the refusal the store holds %d fixes, want fix 1 alone", n)
		}
		running := readingFix(1)
		running.State, running.Entry = FixRunning, 7
		must(t, records.UpdateFix(ctx, tx, running))
		if err := records.PutFix(ctx, tx, readingFix(2)); !errors.Is(err, ErrFixOpen) {
			t.Fatalf("PutFix of fix 2 while fix 1 runs = %v, want ErrFixOpen", err)
		}
		if n := fixesOf(t, tx, fixIssue); n != 1 {
			t.Fatalf("after the second refusal the store holds %d fixes, want fix 1 alone", n)
		}
	})
	err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `insert into task_fixes (issue, generation, task, fix, state, check_id, head, place)
			values ($1, 3, 'a', 2, 'reading', $2, $3, 1)`, fixIssue, fixCheck, fixHeadY)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "task_fixes_one_open") {
		t.Fatalf("a second open fix written by hand = %v, want the index task_fixes_one_open to refuse it", err)
	}
	inTx(t, st, func(tx pgx.Tx) {
		ended := readingFix(1)
		ended.State, ended.Entry, ended.Result, ended.Way = FixEnded, 7, "passed", 1
		must(t, records.UpdateFix(ctx, tx, ended))
		if err := records.PutFix(ctx, tx, readingFix(3)); err == nil || errors.Is(err, ErrFixOpen) {
			t.Fatalf("PutFix of fix 3 after fix 1 = %v, want it refused as no next number", err)
		}
		second := readingFix(2)
		second.Head = fixHeadY
		must(t, records.PutFix(ctx, tx, second))
		latest, err := records.LatestFix(ctx, tx, fixIssue, 3, "a")
		must(t, err)
		if latest == nil || !reflect.DeepEqual(*latest, second) {
			t.Fatalf("the latest fix after fix 1 ended = %+v, want fix 2 as written: %+v", latest, second)
		}
	})
}

// A failure of a check at a head of a task is one row: the first write of it is taken, and a later
// write of the same check at the same head is not, and leaves the first as it was. The same check
// at another head is a failure of its own, which is the control: the second write's false is the
// key's, not a write that always refuses.
func TestOneFailureOfACheckAtAHeadHasOneRow(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	records := NewStore()
	at := time.Date(2026, 11, 23, 15, 0, 0, 0, time.UTC)
	failure := CIFailure{Issue: fixIssue, Generation: 3, Task: "a", Check: fixCheck, Head: fixHeadX, WentTo: CIFailureToImplementer, At: at}
	inTx(t, st, func(tx pgx.Tx) {
		if first, err := records.PutCIFailure(ctx, tx, failure); err != nil || !first {
			t.Fatalf("the first failure of %s at %s = %t, %v; want it written", fixCheck, fixHeadX, first, err)
		}
		again := failure
		again.WentTo, again.At = CIFailureToOwner, at.Add(time.Minute)
		if second, err := records.PutCIFailure(ctx, tx, again); err != nil || second {
			t.Fatalf("the second failure of %s at %s = %t, %v; want it refused, the row being there", fixCheck, fixHeadX, second, err)
		}
		got, err := records.CIFailureAt(ctx, tx, fixIssue, 3, "a", fixCheck, fixHeadX)
		must(t, err)
		if got == nil || !reflect.DeepEqual(*got, failure) {
			t.Fatalf("the failure of %s at %s = %+v, want the first as written: %+v", fixCheck, fixHeadX, got, failure)
		}
		other := failure
		other.Head = fixHeadY
		if first, err := records.PutCIFailure(ctx, tx, other); err != nil || !first {
			t.Fatalf("the failure of %s at another head = %t, %v; want a row of its own", fixCheck, first, err)
		}
		if got, err := records.CIFailureAt(ctx, tx, fixIssue, 3, "a", "k-other", fixHeadX); err != nil || got != nil {
			t.Fatalf("the failure of a check that has none = %+v, %v; want none", got, err)
		}
	})
}

// The owner is told of a failure of a check at a head once: TellOwnerOnce answers true the first
// time and false after, and the row keeps that it was told. The same check at another head is told
// once of its own, which is the control. A failure the store holds no row of is refused, since a
// telling of it would be told of nothing recorded.
func TestTheOwnerIsToldOnceForACheckAtAHead(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	records := NewStore()
	at := time.Date(2026, 11, 23, 15, 0, 0, 0, time.UTC)
	failure := CIFailure{Issue: fixIssue, Generation: 3, Task: "a", Check: fixCheck, Head: fixHeadX, WentTo: CIFailureToOwner, At: at}
	inTx(t, st, func(tx pgx.Tx) {
		if told, err := records.TellOwnerOnce(ctx, tx, fixIssue, 3, "a", fixCheck, fixHeadX); err == nil || told {
			t.Fatalf("TellOwnerOnce of a failure the store holds no row of = %t, %v; want it refused", told, err)
		}
		if _, err := records.PutCIFailure(ctx, tx, failure); err != nil {
			t.Fatal(err)
		}
		if told, err := records.TellOwnerOnce(ctx, tx, fixIssue, 3, "a", fixCheck, fixHeadX); err != nil || !told {
			t.Fatalf("the first telling = %t, %v; want true", told, err)
		}
		if told, err := records.TellOwnerOnce(ctx, tx, fixIssue, 3, "a", fixCheck, fixHeadX); err != nil || told {
			t.Fatalf("the second telling = %t, %v; want false: the owner was told", told, err)
		}
		got, err := records.CIFailureAt(ctx, tx, fixIssue, 3, "a", fixCheck, fixHeadX)
		must(t, err)
		if got == nil || !got.OwnerTold {
			t.Fatalf("the failure after its telling = %+v, want it marked told", got)
		}
		other := failure
		other.Head = fixHeadY
		if _, err := records.PutCIFailure(ctx, tx, other); err != nil {
			t.Fatal(err)
		}
		if told, err := records.TellOwnerOnce(ctx, tx, fixIssue, 3, "a", fixCheck, fixHeadY); err != nil || !told {
			t.Fatalf("the first telling at another head = %t, %v; want true", told, err)
		}
	})
}

// A failure is taken by one fix: MarkCIFailureFix writes the fix's number on the row and that it
// went to a fix, and refuses a row that names a fix already, a fix that is no number and a failure
// the store holds no row of, each writing nothing.
func TestACIFailureTakesOneFix(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	records := NewStore()
	at := time.Date(2026, 11, 23, 15, 0, 0, 0, time.UTC)
	failure := CIFailure{Issue: fixIssue, Generation: 3, Task: "a", Check: fixCheck, Head: fixHeadX, WentTo: CIFailureToOwner, At: at}
	inTx(t, st, func(tx pgx.Tx) {
		if err := records.MarkCIFailureFix(ctx, tx, fixIssue, 3, "a", fixCheck, fixHeadX, 1); err == nil {
			t.Fatal("MarkCIFailureFix of a failure the store holds no row of was taken, want it refused")
		}
		if _, err := records.PutCIFailure(ctx, tx, failure); err != nil {
			t.Fatal(err)
		}
		if err := records.MarkCIFailureFix(ctx, tx, fixIssue, 3, "a", fixCheck, fixHeadX, 0); err == nil {
			t.Fatal("MarkCIFailureFix of fix 0 was taken, want it refused: a fix counts from 1")
		}
		must(t, records.MarkCIFailureFix(ctx, tx, fixIssue, 3, "a", fixCheck, fixHeadX, 1))
		if err := records.MarkCIFailureFix(ctx, tx, fixIssue, 3, "a", fixCheck, fixHeadX, 2); err == nil {
			t.Fatal("MarkCIFailureFix of a failure fix 1 took was taken for fix 2, want it refused")
		}
		got, err := records.CIFailureAt(ctx, tx, fixIssue, 3, "a", fixCheck, fixHeadX)
		must(t, err)
		want := failure
		want.WentTo, want.Fix = CIFailureToFix, 1
		if got == nil || !reflect.DeepEqual(*got, want) {
			t.Fatalf("the failure after its fix = %+v, want %+v", got, want)
		}
	})
}

// RunningFix is the task's fix that reads its job or runs, every field as it was written, and none
// once that fix ended; LatestFix is the task's newest fix in any state. Neither reads a fix of
// another task or another generation.
func TestARunningFixIsReadAndAnEndedOneIsNot(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	records := NewStore()
	started := time.Date(2026, 11, 23, 15, 0, 0, 0, time.UTC)
	ended := started.Add(time.Hour)
	inTx(t, st, func(tx pgx.Tx) {
		if got, err := records.RunningFix(ctx, tx, fixIssue, 3, "a"); err != nil || got != nil {
			t.Fatalf("RunningFix of a task with no fix = %+v, %v; want none", got, err)
		}
		if got, err := records.LatestFix(ctx, tx, fixIssue, 3, "a"); err != nil || got != nil {
			t.Fatalf("LatestFix of a task with no fix = %+v, %v; want none", got, err)
		}
		must(t, records.PutFix(ctx, tx, readingFix(1)))
		if got, err := records.RunningFix(ctx, tx, fixIssue, 3, "a"); err != nil || got == nil || !reflect.DeepEqual(*got, readingFix(1)) {
			t.Fatalf("RunningFix while fix 1 reads its job = %+v, %v; want it: %+v", got, err, readingFix(1))
		}
		running := readingFix(1)
		running.State, running.Entry, running.GotReport, running.ReturnTo = FixRunning, 7, true, 2
		running.JobName, running.JobURL, running.StartedAt = "unit", "https://github.com/o/r/actions/runs/7/job/9", &started
		must(t, records.UpdateFix(ctx, tx, running))
		if got, err := records.RunningFix(ctx, tx, fixIssue, 3, "a"); err != nil || got == nil || !reflect.DeepEqual(*got, running) {
			t.Fatalf("RunningFix while fix 1 runs = %+v, %v; want it: %+v", got, err, running)
		}
		for _, other := range []struct {
			generation uint64
			task       string
		}{{3, "b"}, {2, "a"}} {
			if got, err := records.RunningFix(ctx, tx, fixIssue, other.generation, other.task); err != nil || got != nil {
				t.Fatalf("RunningFix of task %s at generation %d = %+v, %v; want none", other.task, other.generation, got, err)
			}
		}
		// Every field of a fix is written and read back: an ended fix carries the ones a running
		// fix leaves at zero.
		done := running
		done.State, done.Failed, done.EndHead, done.Result, done.ReadFailed = FixEnded, true, fixHeadY, "stopped", "Dispatch answered 502"
		done.ClaimFailed, done.Way, done.EndedAt = "the pod did not start", 4, &ended
		must(t, records.UpdateFix(ctx, tx, done))
		if got, err := records.RunningFix(ctx, tx, fixIssue, 3, "a"); err != nil || got != nil {
			t.Fatalf("RunningFix once fix 1 ended = %+v, %v; want none", got, err)
		}
		if got, err := records.LatestFix(ctx, tx, fixIssue, 3, "a"); err != nil || got == nil || !reflect.DeepEqual(*got, done) {
			t.Fatalf("LatestFix once fix 1 ended = %+v, %v; want it whole: %+v", got, err, done)
		}
		if err := records.UpdateFix(ctx, tx, readingFix(5)); err == nil {
			t.Fatal("UpdateFix of a fix the store does not hold was taken, want it refused")
		}
	})
}

// The issue read names the fix that runs at its generation, from the moment its entry is raised
// (state running): none while it reads its job, its number while it runs, none once it ended, and
// none for a fix of another generation. Every read behind the same columns names it.
func TestTheIssueReadsOnlyAFixWhoseEntryIsRaised(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	records := NewStore()
	issue := issueFixture(fixIssue)
	runningFixOf := func(tx pgx.Tx) int {
		t.Helper()
		got, err := records.Issue(ctx, tx, fixIssue)
		must(t, err)
		live, err := records.LiveIssues(ctx, tx, "LEGION")
		must(t, err)
		if len(live) != 1 || live[0].RunningFix != got.RunningFix {
			t.Fatalf("the live issues name %+v, want %s naming fix %d as Issue does", live, fixIssue, got.RunningFix)
		}
		return got.RunningFix
	}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issue))
		if n := runningFixOf(tx); n != 0 {
			t.Fatalf("the issue with no fix names fix %d, want none", n)
		}
		must(t, records.PutFix(ctx, tx, readingFix(1)))
		if n := runningFixOf(tx); n != 0 {
			t.Fatalf("the issue whose fix reads its job names fix %d, want none: its entry is not raised", n)
		}
		running := readingFix(1)
		running.State, running.Entry = FixRunning, 7
		must(t, records.UpdateFix(ctx, tx, running))
		if n := runningFixOf(tx); n != 1 {
			t.Fatalf("the issue whose fix runs names fix %d, want 1", n)
		}
		running.State = FixEnded
		must(t, records.UpdateFix(ctx, tx, running))
		if n := runningFixOf(tx); n != 0 {
			t.Fatalf("the issue whose fix ended names fix %d, want none", n)
		}
		earlier := readingFix(1)
		earlier.Generation = 2
		must(t, records.PutFix(ctx, tx, earlier))
		earlier.State = FixRunning
		must(t, records.UpdateFix(ctx, tx, earlier))
		if n := runningFixOf(tx); n != 0 {
			t.Fatalf("the issue at generation 3 names fix %d, which runs at generation 2; want none", n)
		}
	})
}

// A fix's task is its issue's work while that fix runs, whatever phase the issue stands in: in
// reviewing beside the review, and held for the fix. It is not once the fix ended, nor while
// another fix runs, nor on another run. A task of no fix keeps the rule of today, which is the
// control: an issue in reviewing holds a review's task and no implementer's.
func TestAFixTaskHoldsWhileItsFixRuns(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	records := NewStore()
	issue := issueFixture(fixIssue)
	issue.Phase = phase.Reviewing
	read := func(tx pgx.Tx) *Issue {
		t.Helper()
		got, err := records.Issue(ctx, tx, fixIssue)
		must(t, err)
		return got
	}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issue))
		must(t, records.PutFix(ctx, tx, readingFix(1)))
		running := readingFix(1)
		running.State, running.Entry = FixRunning, 7
		must(t, records.UpdateFix(ctx, tx, running))
		got := read(tx)
		for _, tc := range []struct {
			name       string
			queuedFor  phase.Phase
			generation uint64
			fix        int
			want       bool
		}{
			{"fix 1's task in reviewing", phase.Implementing, 3, 1, true},
			{"fix 1's task of no run", phase.Implementing, 0, 1, true},
			{"fix 1's task of another run", phase.Implementing, 2, 1, false},
			{"fix 2's task while fix 1 runs", phase.Implementing, 3, 2, false},
			{"an implementer's task of no fix in reviewing", phase.Implementing, 3, 0, false},
			{"a reviewer's task of no fix in reviewing", phase.Reviewing, 3, 0, true},
		} {
			if holds := PhaseHolds(got, tc.queuedFor, tc.generation, tc.fix); holds != tc.want {
				t.Errorf("PhaseHolds of %s = %t, want %t", tc.name, holds, tc.want)
			}
		}
		held := issue
		held.Phase, held.Hold = phase.Held, &Hold{From: phase.Reviewing, Reason: HoldForFix}
		must(t, records.PutIssue(ctx, tx, held))
		if got := read(tx); !PhaseHolds(got, phase.Implementing, 3, 1) {
			t.Errorf("PhaseHolds of fix 1's task with the issue held for it = false, want true")
		}
		running.State = FixEnded
		must(t, records.UpdateFix(ctx, tx, running))
		if got := read(tx); PhaseHolds(got, phase.Implementing, 3, 1) {
			t.Errorf("PhaseHolds of fix 1's task once fix 1 ended = true, want false")
		}
		second := readingFix(2)
		must(t, records.PutFix(ctx, tx, second))
		second.State = FixRunning
		must(t, records.UpdateFix(ctx, tx, second))
		got = read(tx)
		if PhaseHolds(got, phase.Implementing, 3, 1) || !PhaseHolds(got, phase.Implementing, 3, 2) {
			t.Errorf("PhaseHolds while fix 2 runs = %t for fix 1 and %t for fix 2, want false and true",
				PhaseHolds(got, phase.Implementing, 3, 1), PhaseHolds(got, phase.Implementing, 3, 2))
		}
	})
	if !PhaseHolds(nil, phase.Implementing, 3, 1) {
		t.Errorf("PhaseHolds of a fix's task on an issue the record does not hold = false, want true")
	}
}

// Whether a task's run takes a fix is the task's (plan_tasks.fixes_ci), and a copy of the plan to
// another generation copies it, in both forms: a hand-over that carries the run, and a run again
// from its start. A task that takes none copies none, which is the control.
func TestACarriedPlanKeepsFixesCI(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	records := NewStore()
	v, tasks, steps := ownersPlan(planIssue, 1, 2, planOwner, planGivenAt)
	tasks[0].FixesCI = true
	none, noneTasks, noneSteps := ownersPlan("LEGION-209", 1, 2, planOwner, planGivenAt)
	noneTasks[0].FixesCI = false
	inTx(t, st, func(tx pgx.Tx) {
		putPlan(t, tx, v, tasks, steps)
		putPlan(t, tx, none, noneTasks, noneSteps)
		for _, issue := range []string{planIssue, "LEGION-209"} {
			must(t, records.CarryPlan(ctx, tx, issue, 1, 2, true))
			must(t, records.CarryPlan(ctx, tx, issue, 1, 3, false))
		}
	})
	inTx(t, st, func(tx pgx.Tx) {
		for _, tc := range []struct {
			issue      string
			generation uint64
			want       bool
		}{{planIssue, 2, true}, {planIssue, 3, true}, {"LEGION-209", 2, false}, {"LEGION-209", 3, false}} {
			plan := readPlan(t, tx, tc.issue, tc.generation)
			if plan == nil || len(plan.Tasks) != 1 || plan.Tasks[0].FixesCI != tc.want {
				t.Errorf("the plan of %s copied to generation %d = %+v, want its task's fixes_ci %t", tc.issue, tc.generation, plan, tc.want)
			}
		}
	})
}

// Whether a run takes a fix is marked on every task of the run at once (MarkRunFixesCI), and read
// as the run's (RunFixesCI): true where a task of the generation holds it. The issue's other
// generation is another run, which the mark leaves as it was; that is the control. A run with no
// plan rows has no task to mark, and takes none.
func TestMarkRunFixesCIMarksEveryTaskOfItsRun(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	records := NewStore()
	v, tasks, steps := ownersPlan(planIssue, 1, 2, planOwner, planGivenAt)
	tasks[0].FixesCI = false
	second := tasks[0]
	second.Task, second.Needs = "b", nil
	other, otherTasks, otherSteps := ownersPlan(planIssue, 2, 2, planOwner, planGivenAt)
	otherTasks[0].FixesCI = false
	inTx(t, st, func(tx pgx.Tx) {
		putPlan(t, tx, v, []PlanTask{tasks[0], second}, steps)
		putPlan(t, tx, other, otherTasks, otherSteps)
		if takes, err := records.RunFixesCI(ctx, tx, planIssue, 1); err != nil || takes {
			t.Fatalf("RunFixesCI of a run none of whose tasks takes a fix = %t, %v; want false", takes, err)
		}
		must(t, records.MarkRunFixesCI(ctx, tx, planIssue, 1))
		if takes, err := records.RunFixesCI(ctx, tx, planIssue, 1); err != nil || !takes {
			t.Fatalf("RunFixesCI of the marked run = %t, %v; want true", takes, err)
		}
		if takes, err := records.RunFixesCI(ctx, tx, planIssue, 2); err != nil || takes {
			t.Fatalf("RunFixesCI of the issue's other run = %t, %v; want false", takes, err)
		}
		for _, task := range readPlan(t, tx, planIssue, 1).Tasks {
			if !task.FixesCI {
				t.Errorf("task %s of the marked run reads fixes_ci false, want every task marked", task.Task)
			}
		}
		must(t, records.MarkRunFixesCI(ctx, tx, "LEGION-OLD", 1))
		if takes, err := records.RunFixesCI(ctx, tx, "LEGION-OLD", 1); err != nil || takes {
			t.Fatalf("RunFixesCI of a run with no plan rows = %t, %v; want false", takes, err)
		}
	})
}

// A later version of a plan keeps whether each task it gives again takes a fix, since the run
// decided that at its start: PutPlan's update of a task the generation holds names no fixes_ci. A
// new task of the later version is written with the value the version gives it, which is the
// control: the column is written, and the version's other attributes of a kept task are taken.
func TestALaterVersionKeepsATasksFixesCI(t *testing.T) {
	st := migratedStore(t)
	v, tasks, steps := ownersPlan(planIssue, 1, 2, planOwner, planGivenAt)
	tasks[0].FixesCI = true
	later := v
	later.Version = 3
	again := tasks[0]
	again.PlanVersion, again.BlockText, again.FixesCI = 3, "Build the widget, and its gadget.", false
	added := PlanTask{Issue: planIssue, Generation: 1, Task: "b", PlanVersion: 3, BlockText: "Test the widget.", State: TaskWaiting, FixesCI: true}
	plain := PlanTask{Issue: planIssue, Generation: 1, Task: "c", PlanVersion: 3, BlockText: "Ship the widget.", State: TaskWaiting}
	inTx(t, st, func(tx pgx.Tx) {
		putPlan(t, tx, v, tasks, steps)
		putPlan(t, tx, later, []PlanTask{again, added, plain}, nil)
	})
	inTx(t, st, func(tx pgx.Tx) {
		plan := readPlan(t, tx, planIssue, 1)
		if plan == nil || len(plan.Tasks) != 3 {
			t.Fatalf("the plan after the later version = %+v, want three tasks", plan)
		}
		byTask := map[string]PlanTask{}
		for _, task := range plan.Tasks {
			byTask[task.Task] = task
		}
		if a := byTask["a"]; !a.FixesCI || a.BlockText != again.BlockText {
			t.Errorf("task a after the later version = %+v, want its text taken and its fixes_ci kept true", a)
		}
		if b, c := byTask["b"], byTask["c"]; !b.FixesCI || c.FixesCI {
			t.Errorf("the new tasks read fixes_ci %t and %t, want true and false as the version gave them", b.FixesCI, c.FixesCI)
		}
	})
}

// The read of a fix's failed job rides the role-message lane beside the merge-queue read, so no
// start of its issue waits behind it on the lifecycle lane. The row is written and read back
// strictly as every outbox row is, and the store's kind check takes it.
func TestFixJobReadRidesTheRoleMessageLane(t *testing.T) {
	if lane := LaneOf(OutboxKindFixJobRead); lane != LaneRoleMessage {
		t.Fatalf("LaneOf(%s) = %q, want %q", OutboxKindFixJobRead, lane, LaneRoleMessage)
	}
	if slices.Contains(LaneKinds(LaneLifecycle), OutboxKindFixJobRead) {
		t.Fatalf("the lifecycle lane runs %s, which would hold the issue's starts behind the read", OutboxKindFixJobRead)
	}
	now := time.Date(2026, 11, 23, 15, 0, 0, 0, time.UTC)
	read := FixJobRead{Generation: 3, Task: "a", Fix: 1, Check: fixCheck, Head: fixHeadX}
	row, err := NewOutboxRow(fixIssue, read, now)
	if err != nil || row.Kind != "fix_job_read" {
		t.Fatalf("NewOutboxRow of the read = %+v, %v; want a fix_job_read row", row, err)
	}
	decoded, err := DecodeOutboxPayload(row)
	if err != nil || !reflect.DeepEqual(decoded, read) {
		t.Fatalf("DecodeOutboxPayload of the read = %+v, %v; want %+v", decoded, err, read)
	}
	st := migratedStore(t)
	inTx(t, st, func(tx pgx.Tx) {
		must(t, NewStore().Enqueue(context.Background(), tx, row))
	})
}

// fixStart is the start of fix n's task of fixIssue, as a fix's start is written: of the
// implementer, in implementing, naming the fix and the head its working copy is made at, with no
// slot mark.
func fixStart(t *testing.T, n int, task string, at time.Time) OutboxRow {
	t.Helper()
	row, err := NewOutboxRow(fixIssue, SuperviseRequest{Op: "start", Tree: fixIssue, Role: claim.RoleImplementer, Generation: 3,
		Phase: phase.Implementing, Entry: 7, Task: task, Fix: n, FixFrom: fixHeadX}, at)
	if err != nil {
		t.Fatalf("the start of fix %d: %v", n, err)
	}
	return row
}

// queuedStart is one supervise row of an issue as the outbox holds it.
type queuedStart struct {
	ID        int64
	Payload   SuperviseRequest
	Attempts  int
	NextAt    time.Time
	LastError string
	CreatedAt time.Time
}

// queuedStarts is every supervise row of issue, in the order they were written.
func queuedStarts(t *testing.T, tx pgx.Tx, issue string) []queuedStart {
	t.Helper()
	rows, err := tx.Query(context.Background(), `select id, payload, attempts, next_at, coalesce(last_error, ''), created_at
		from outbox where issue = $1 and kind = 'supervise' order by id`, issue)
	if err != nil {
		t.Fatalf("read the starts of %s: %v", issue, err)
	}
	var starts []queuedStart
	var start queuedStart
	var payload []byte
	if _, err := pgx.ForEachRow(rows, []any{&start.ID, &payload, &start.Attempts, &start.NextAt, &start.LastError, &start.CreatedAt}, func() error {
		start.Payload = SuperviseRequest{}
		if err := json.Unmarshal(payload, &start.Payload); err != nil {
			return err
		}
		starts = append(starts, start)
		return nil
	}); err != nil {
		t.Fatalf("read the starts of %s: %v", issue, err)
	}
	return starts
}

// tasksOf is each start's task, in order.
func tasksOf(starts []queuedStart) []string {
	tasks := make([]string, 0, len(starts))
	for _, start := range starts {
		tasks = append(tasks, start.Payload.Task)
	}
	return tasks
}

// A fix's start is no start of the slot line. EnqueueStart of a reviewer's start leaves the issue's
// unleased fix start where it is, and EnqueueFixStart leaves the unleased reviewer's start. A
// second start of the same fix, as a resume's resend, takes the first out and carries its attempts,
// its backoff, its error and its place, which is the control: EnqueueFixStart does take a start
// out, its own fix's alone. A start of another fix, and a start of the same fix a lane holds, are
// left. A start that names no fix, or that waits for a slot, is refused before anything is
// written, and no fix start stands in the waiting line.
func TestAFixStartIsNoPendingPhaseStart(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	records := NewStore()
	now := time.Date(2026, 11, 23, 15, 0, 0, 0, time.UTC)
	backoff, queued := now.Add(time.Hour), now.Add(-time.Hour)
	review, err := NewOutboxRow(fixIssue, SuperviseRequest{Op: "start", Tree: fixIssue, Role: claim.RoleReviewer, Generation: 3,
		Phase: phase.Reviewing, Entry: 4, Task: "Review the change."}, now)
	must(t, err)
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.EnqueueFixStart(ctx, tx, fixStart(t, 1, "Fix k-ci.", now)))
		if _, err := tx.Exec(ctx, `update outbox set attempts = 2, next_at = $2, last_error = 'the pod did not start', created_at = $3
			where issue = $1 and kind = 'supervise'`, fixIssue, backoff, queued); err != nil {
			t.Fatalf("back the fix's start off: %v", err)
		}
		must(t, records.EnqueueStart(ctx, tx, review))
		if got := tasksOf(queuedStarts(t, tx, fixIssue)); !slices.Equal(got, []string{"Fix k-ci.", "Review the change."}) {
			t.Fatalf("after the reviewer's start the issue holds the starts %q, want the fix's and the reviewer's", got)
		}
		must(t, records.EnqueueFixStart(ctx, tx, fixStart(t, 1, "Fix k-ci again.", now)))
		starts := queuedStarts(t, tx, fixIssue)
		if got := tasksOf(starts); !slices.Equal(got, []string{"Review the change.", "Fix k-ci again."}) {
			t.Fatalf("after the fix's second start the issue holds the starts %q, want the reviewer's and the fix's second", got)
		}
		again := starts[1]
		if again.Attempts != 2 || !again.NextAt.Equal(backoff) || again.LastError != "the pod did not start" || !again.CreatedAt.Equal(queued) {
			t.Fatalf("the fix's second start = %+v, want the first's attempts 2, backoff %s, error and place %s", again, backoff, queued)
		}
		must(t, records.EnqueueFixStart(ctx, tx, fixStart(t, 2, "Fix k-ci, second fix.", now)))
		if got := tasksOf(queuedStarts(t, tx, fixIssue)); !slices.Equal(got, []string{"Review the change.", "Fix k-ci again.", "Fix k-ci, second fix."}) {
			t.Fatalf("after fix 2's start the issue holds the starts %q, want fix 1's left beside it", got)
		}
		if _, err := tx.Exec(ctx, `update outbox set lease_token = 'lane-1', lease_until = $2 where id = $1`, again.ID, now.Add(time.Minute)); err != nil {
			t.Fatalf("lease fix 1's start: %v", err)
		}
		must(t, records.EnqueueFixStart(ctx, tx, fixStart(t, 1, "Fix k-ci a third time.", now)))
		if got := tasksOf(queuedStarts(t, tx, fixIssue)); !slices.Equal(got, []string{"Review the change.", "Fix k-ci again.", "Fix k-ci, second fix.", "Fix k-ci a third time."}) {
			t.Fatalf("after a start of fix 1 while a lane holds its start the issue holds the starts %q, want the held one left", got)
		}
		before := len(queuedStarts(t, tx, fixIssue))
		if err := records.EnqueueFixStart(ctx, tx, review); err == nil {
			t.Fatal("EnqueueFixStart of a start that names no fix was taken, want it refused")
		}
		slotted := OutboxRow{Kind: OutboxKindSupervise, Issue: fixIssue, NextAt: now, Payload: json.RawMessage(`{"op":"start","tree":"` + fixIssue +
			`","role":"implementer","generation":3,"phase":"implementing","task":"Fix k-ci.","fix":1,"needsSlot":true}`)}
		if err := records.EnqueueFixStart(ctx, tx, slotted); err == nil {
			t.Fatal("EnqueueFixStart of a fix's start that waits for a slot was taken, want it refused")
		}
		if after := len(queuedStarts(t, tx, fixIssue)); after != before {
			t.Fatalf("the refusals left %d starts, want the %d before them", after, before)
		}
		owed, err := records.OwedStarts(ctx, tx, "LEGION")
		must(t, err)
		if len(owed) != 0 {
			t.Fatalf("the waiting line holds %+v, want no fix start in it", owed)
		}
	})
}

// EnqueueFixStart takes out only an older start of the same fix of the same task, which the start's
// own branch names (claim.BesideBranch of the implementer). Two tasks of one issue at one
// generation give two branches (claim's besideTask), so the fix 1 of task b never takes out the
// fix 1 of task a: without the branch in the predicate it would, and task a's implementer would
// never start. The control, kept: a second start of task a's fix 1, which names task a's branch,
// does take the first out, so the predicate still replaces a resend of the same fix.
func TestEnqueueFixStartKeepsTwoTasksApart(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	records := NewStore()
	now := time.Date(2026, 11, 23, 15, 0, 0, 0, time.UTC)
	branchA, err := claim.BesideBranch(fixIssue, 3, "a", 1, claim.RoleImplementer)
	must(t, err)
	branchB, err := claim.BesideBranch(fixIssue, 3, "b", 1, claim.RoleImplementer)
	must(t, err)
	if branchA == branchB {
		t.Fatalf("the branches of tasks a and b are both %q, want two names", branchA)
	}
	start := func(task, branch string) OutboxRow {
		row, err := NewOutboxRow(fixIssue, SuperviseRequest{Op: "start", Tree: fixIssue, Role: claim.RoleImplementer, Generation: 3,
			Phase: phase.Implementing, Entry: 7, Task: task, Fix: 1, FixFrom: fixHeadX, FixBranch: branch}, now)
		must(t, err)
		return row
	}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.EnqueueFixStart(ctx, tx, start("Fix a's CI.", branchA)))
		must(t, records.EnqueueFixStart(ctx, tx, start("Fix b's CI.", branchB)))
		if got := tasksOf(queuedStarts(t, tx, fixIssue)); !slices.Equal(got, []string{"Fix a's CI.", "Fix b's CI."}) {
			t.Fatalf("after fix 1 of tasks a and b the issue holds the starts %q, want both: b's branch must not take a's out", got)
		}
		must(t, records.EnqueueFixStart(ctx, tx, start("Fix a's CI again.", branchA)))
		if got := tasksOf(queuedStarts(t, tx, fixIssue)); !slices.Equal(got, []string{"Fix b's CI.", "Fix a's CI again."}) {
			t.Fatalf("after a second start of task a's fix the issue holds the starts %q, want b's left and a's resend in a's place", got)
		}
	})
}

// A step's binding to a branch beside a fix names that fix's number (StepStart.PushBranch), and
// the text of the step reads that fix's pushes whichever fix of the task is the latest: FixAt
// answers fix 1, ended, with its recorded push, while fix 2 runs; a number the task has not reached
// and another generation's fix are none.
func TestFixAtReadsTheFixABoundBranchNames(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	records := NewStore()
	inTx(t, st, func(tx pgx.Tx) {
		first := readingFix(1)
		must(t, records.PutFix(ctx, tx, first))
		must(t, records.RecordBesidePush(ctx, tx, fixIssue, 3, "a", 1, "tester", fixHeadY))
		first.State, first.Entry, first.Result, first.Way = FixEnded, 7, "passed", 2
		must(t, records.UpdateFix(ctx, tx, first))
		second := readingFix(2)
		second.Head = fixHeadY
		must(t, records.PutFix(ctx, tx, second))

		got, err := records.FixAt(ctx, tx, fixIssue, 3, "a", 1)
		must(t, err)
		first.Pushed = map[string]string{"tester": fixHeadY}
		if got == nil || !reflect.DeepEqual(*got, first) {
			t.Fatalf("FixAt of fix 1 while fix 2 reads its job = %+v, want fix 1 with its push: %+v", got, first)
		}
		latest, err := records.LatestFix(ctx, tx, fixIssue, 3, "a")
		must(t, err)
		if latest == nil || latest.Fix != 2 {
			t.Fatalf("the latest fix = %+v, want fix 2", latest)
		}
		for _, none := range []struct {
			generation uint64
			fix        int
		}{{3, 3}, {4, 1}} {
			if got, err := records.FixAt(ctx, tx, fixIssue, none.generation, "a", none.fix); err != nil || got != nil {
				t.Errorf("FixAt of fix %d at generation %d = %+v, %v; want none", none.fix, none.generation, got, err)
			}
		}
	})
}

// besidePushes is what a fix's Pushed reads at each point of besidePushRace.
type besidePushes struct{ afterPushes, afterStaleUpdate, afterDelete map[string]string }

// besidePushRace plays, on records, the two ways a push beside fix 1 can be lost, each in
// transactions of its own. A fact's transaction reads fix 1 (stale), then the implementer's push P
// is written and, before it commits, the tester's push Q waits on its row; P commits, Q commits,
// and the stale transaction writes fix 1's way from what it read. Then the tester's branch is
// deleted. It answers Pushed after the two pushes, after the stale update and after the delete.
func besidePushRace(t *testing.T, records *Postgres) besidePushes {
	t.Helper()
	st := migratedStore(t)
	ctx := context.Background()
	inTx(t, st, func(tx pgx.Tx) {
		fix := readingFix(1)
		must(t, records.PutFix(ctx, tx, fix))
		fix.State, fix.Entry = FixRunning, 7
		must(t, records.UpdateFix(ctx, tx, fix))
	})
	pushed := func() map[string]string {
		t.Helper()
		var got map[string]string
		inTx(t, st, func(tx pgx.Tx) {
			fix, err := records.FixAt(ctx, tx, fixIssue, 3, "a", 1)
			must(t, err)
			got = fix.Pushed
		})
		return got
	}
	begin := func() pgx.Tx {
		t.Helper()
		tx, err := st.Pool().BeginTx(ctx, pgx.TxOptions{})
		must(t, err)
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		return tx
	}

	staleTx := begin()
	stale, err := records.LatestFix(ctx, staleTx, fixIssue, 3, "a")
	must(t, err)

	pushTx := begin()
	must(t, records.RecordBesidePush(ctx, pushTx, fixIssue, 3, "a", 1, "implementer", fixHeadX))
	other := make(chan error, 1)
	go func() {
		tx, err := st.Pool().BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			other <- err
			return
		}
		if err := records.RecordBesidePush(ctx, tx, fixIssue, 3, "a", 1, "tester", fixHeadY); err != nil {
			_ = tx.Rollback(ctx)
			other <- err
			return
		}
		other <- tx.Commit(ctx)
	}()
	// The barrier at the push's commit: the tester's write waits on fix 1's row, which the
	// uncommitted push holds.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		must(t, st.Pool().QueryRow(ctx, `select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock' and query like '%task_fixes%'`).Scan(&waiting))
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the tester's push never waited on fix 1's row: the race this test plays did not happen")
		}
		time.Sleep(10 * time.Millisecond)
	}
	must(t, pushTx.Commit(ctx))
	must(t, <-other)
	var race besidePushes
	race.afterPushes = pushed()

	stale.Way = 2
	must(t, records.UpdateFix(ctx, staleTx, *stale))
	must(t, staleTx.Commit(ctx))
	race.afterStaleUpdate = pushed()

	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.RecordBesidePush(ctx, tx, fixIssue, 3, "a", 1, "tester", ""))
	})
	race.afterDelete = pushed()
	return race
}

// A push of a branch beside a fix changes its own role's key of the fix's Pushed and nothing
// else, so neither a write of the fix from a read made before the push nor another role's push
// made at the same time loses it: after the implementer's push P, the tester's push Q that waited
// on it and a stale update of the fix's way, Pushed holds P and Q, and the delete of the tester's
// branch takes Q alone.
//
// The control, a kept subtest: with the seam rewritePushedWithFix, which writes the whole map from
// the fix the writer read, the tester's push loses P and the stale update loses Q, which the
// subtest asserts.
func TestBesidePushRecordingDoesNotOverwriteAnotherRoleOrGetLost(t *testing.T) {
	both := map[string]string{"implementer": fixHeadX, "tester": fixHeadY}
	t.Run("each push keeps every other role's head", func(t *testing.T) {
		race := besidePushRace(t, NewStore())
		if !reflect.DeepEqual(race.afterPushes, both) {
			t.Errorf("Pushed after the two pushes = %v, want %v", race.afterPushes, both)
		}
		if !reflect.DeepEqual(race.afterStaleUpdate, both) {
			t.Errorf("Pushed after the stale update of the fix's way = %v, want %v", race.afterStaleUpdate, both)
		}
		if want := map[string]string{"implementer": fixHeadX}; !reflect.DeepEqual(race.afterDelete, want) {
			t.Errorf("Pushed after the tester's branch was deleted = %v, want %v", race.afterDelete, want)
		}
	})
	t.Run("control: rewritePushedWithFix loses a push to each writer", func(t *testing.T) {
		race := besidePushRace(t, &Postgres{rewritePushedWithFix: true})
		if want := map[string]string{"tester": fixHeadY}; !reflect.DeepEqual(race.afterPushes, want) {
			t.Errorf("Pushed after the two pushes with the whole map rewritten = %v, want %v: the tester's push lost P", race.afterPushes, want)
		}
		if race.afterStaleUpdate != nil {
			t.Errorf("Pushed after the stale update with the whole map rewritten = %v, want none: the stale fix lost Q", race.afterStaleUpdate)
		}
	})
}
