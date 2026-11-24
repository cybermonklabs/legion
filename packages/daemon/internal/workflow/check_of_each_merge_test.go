package workflow

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// twoChecks is a closed root LEGION-208 holding two open production checks, of two merges, each at
// look 1: the first opened at checkOpened, the second an hour later, each bound checkBound after
// its open. lookOpen says whether each check's look 1 is still open, first then second. It
// answers the two checks as seeded.
func twoChecks(t *testing.T, rig checkRig, lookOpen [2]bool) (first, second record.ProductionCheck) {
	t.Helper()
	issue := record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
		Project: "LEGION", Title: "root", Phase: phase.Done, Generation: 7, Status: "done", Rank: "U", LastDispatchSeq: 1}
	seedIssue(t, rig.pool, issue)
	check := func(number int, sha string, opened time.Time, open bool) record.ProductionCheck {
		return record.ProductionCheck{Issue: issue.Key, Project: issue.Project, Tree: issue.Tree, Repo: "sjawhar/legion", Number: number, MergeSHA: sha,
			Role: claim.RoleTester, OpenedAt: opened, BoundAt: opened.Add(checkBound), Looks: 1, LookOpen: open, LookWakeAt: &opened,
			LastWakeKind: wakeMerge, LastWakeDetail: sha, LastWakeAt: &opened}
	}
	first = check(1, "merge-1", checkOpened, lookOpen[0])
	second = check(2, "merge-2", checkOpened.Add(time.Hour), lookOpen[1])
	seedRecord(t, rig.pool, func(tx pgx.Tx) error {
		if err := record.NewStore().PutProductionCheck(t.Context(), tx, first); err != nil {
			return err
		}
		return record.NewStore().PutProductionCheck(t.Context(), tx, second)
	})
	return first, second
}

// checkOpenedAt is issue's check opened at opened, as recorded.
func checkOpenedAt(t *testing.T, rig checkRig, issue string, opened time.Time) record.ProductionCheck {
	t.Helper()
	var check *record.ProductionCheck
	seedRecord(t, rig.pool, func(tx pgx.Tx) error {
		var err error
		check, err = record.NewStore().ProductionCheckOf(t.Context(), tx, issue, opened)
		return err
	})
	if check == nil {
		t.Fatalf("%s holds no check opened at %s", issue, opened)
	}
	return *check
}

// Each row and report of a check acts on the check it names, where an issue holds two: a look's
// end names its check, a report names its check, and a report that names none — as the route sends
// until it names the look's — acts on the newest. The bound's row names no check, and the record
// decides which bound came due: each open check whose last look has not started and whose bound is
// not after now starts its last look, another is left as it stands, and while such a check has a
// look open the issue gets one row at the look bound, however many checks it defers.
func TestEachRowOfACheckActsOnTheCheckItNames(t *testing.T) {
	t.Run("a look's end ends the look of the check it names", func(t *testing.T) {
		rig := newCheckRig(t)
		first, second := twoChecks(t, rig, [2]bool{true, true})
		rig.apply("end-first", intake.CheckLookEnded{Issue: first.Issue, Look: 1, Cause: "look bound", OpenedAt: first.OpenedAt})
		if got := checkOpenedAt(t, rig, first.Issue, first.OpenedAt); got.LookOpen || !got.Open() {
			t.Fatalf("the first check after its look's end = %+v, want its look ended and the check open", got)
		}
		if got := checkOpenedAt(t, rig, second.Issue, second.OpenedAt); !got.LookOpen {
			t.Fatalf("the second check after the first's look ended = %+v, want its look 1 left open", got)
		}
	})
	t.Run("a report closes the check it names", func(t *testing.T) {
		rig := newCheckRig(t)
		first, second := twoChecks(t, rig, [2]bool{true, true})
		if result := rig.apply("pass-second", intake.CheckReport{Issue: second.Issue, Look: 1, Verdict: "pass", Summary: "driven in production", OpenedAt: second.OpenedAt}); result.Refusal != nil {
			t.Fatalf("the report of the second check = %+v, want it recorded", result.Refusal)
		}
		if got := checkOpenedAt(t, rig, second.Issue, second.OpenedAt); got.Open() || got.Outcome != record.CheckRecorded {
			t.Fatalf("the second check after its pass = %+v, want it closed as recorded", got)
		}
		if got := checkOpenedAt(t, rig, first.Issue, first.OpenedAt); !got.Open() || !got.LookOpen {
			t.Fatalf("the first check after the second's pass = %+v, want it open at its look 1", got)
		}
	})
	t.Run("a report that names no check acts on the newest", func(t *testing.T) {
		rig := newCheckRig(t)
		first, second := twoChecks(t, rig, [2]bool{true, true})
		if result := rig.apply("pass-unnamed", intake.CheckReport{Issue: second.Issue, Look: 1, Verdict: "pass", Summary: "driven in production"}); result.Refusal != nil {
			t.Fatalf("the report that names no check = %+v, want it recorded", result.Refusal)
		}
		if got := checkOpenedAt(t, rig, second.Issue, second.OpenedAt); got.Open() {
			t.Fatalf("the newest check after a report that names none = %+v, want it closed", got)
		}
		if got := checkOpenedAt(t, rig, first.Issue, first.OpenedAt); !got.Open() {
			t.Fatalf("the first check after a report that names none = %+v, want it open", got)
		}
	})
	t.Run("the bound of the first starts the first's last look alone", func(t *testing.T) {
		rig := newCheckRig(t)
		first, second := twoChecks(t, rig, [2]bool{false, false})
		rig.clock.now = first.BoundAt.Add(time.Minute)
		rig.apply("bound", intake.CheckBound{Issue: first.Issue})
		if got := checkOpenedAt(t, rig, first.Issue, first.OpenedAt); !got.Last || got.Looks != 2 || !got.LookOpen {
			t.Fatalf("the first check past its bound = %+v, want its last look, look 2, started", got)
		}
		if got := checkOpenedAt(t, rig, second.Issue, second.OpenedAt); got.Last || got.Looks != 1 || got.LookOpen {
			t.Fatalf("the second check before its bound = %+v, want it at look 1 as it stood", got)
		}
		if rows := issueOutboxOf(t, rig.pool, first.Issue, string(record.OutboxKindCheckBound)); len(rows) != 0 {
			t.Fatalf("check_bound rows after the first's bound = %d, want none: the second's own row comes at its bound", len(rows))
		}
	})
	t.Run("past both bounds the first starts its last look and the open look defers once", func(t *testing.T) {
		rig := newCheckRig(t)
		first, second := twoChecks(t, rig, [2]bool{false, true})
		rig.clock.now = second.BoundAt.Add(time.Minute)
		rig.apply("bound", intake.CheckBound{Issue: first.Issue})
		if got := checkOpenedAt(t, rig, first.Issue, first.OpenedAt); !got.Last || !got.LookOpen {
			t.Fatalf("the first check past both bounds = %+v, want its last look started", got)
		}
		if got := checkOpenedAt(t, rig, second.Issue, second.OpenedAt); got.Last || got.Looks != 1 {
			t.Fatalf("the second check with its look open = %+v, want it at look 1, its last look not started", got)
		}
		rows := issueOutboxOf(t, rig.pool, first.Issue, string(record.OutboxKindCheckBound))
		if len(rows) != 1 || !rows[0].nextAt.Equal(rig.clock.now.Add(CheckLookBound)) {
			t.Fatalf("check_bound rows after both bounds = %+v, want one at the look bound %s", rows, rig.clock.now.Add(CheckLookBound))
		}
	})
	t.Run("two open looks past both bounds queue one row for the issue", func(t *testing.T) {
		rig := newCheckRig(t)
		first, second := twoChecks(t, rig, [2]bool{true, true})
		rig.clock.now = second.BoundAt.Add(time.Minute)
		rig.apply("bound", intake.CheckBound{Issue: first.Issue})
		for _, check := range []record.ProductionCheck{first, second} {
			if got := checkOpenedAt(t, rig, check.Issue, check.OpenedAt); got.Last || got.Looks != 1 {
				t.Fatalf("the check of #%d with its look open = %+v, want it at look 1, its last look not started", check.Number, got)
			}
		}
		if rows := issueOutboxOf(t, rig.pool, first.Issue, string(record.OutboxKindCheckBound)); len(rows) != 1 {
			t.Fatalf("check_bound rows after both bounds with both looks open = %d, want one for the issue", len(rows))
		}
	})
}

// openMerge opens issue's check of the merge of #number at sha, as a merge's close opens it
// (openCheck), at the rig's clock.
func openMerge(t *testing.T, rig checkRig, issue record.Issue, number int, sha string) {
	t.Helper()
	seedRecord(t, rig.pool, func(tx pgx.Tx) error {
		ctx, _ := intake.WithCommitHooks(context.Background())
		return rig.engine.openCheck(ctx, tx, issue, record.PullRequest{Issue: issue.Key, Repo: "sjawhar/legion", Number: number, MergeSHA: sha}, false)
	})
}

// checksOfIssue is every check of issue, newest opened first, as recorded.
func checksOfIssue(t *testing.T, rig checkRig, issue string) []record.ProductionCheck {
	t.Helper()
	var checks []record.ProductionCheck
	seedRecord(t, rig.pool, func(tx pgx.Tx) error {
		var err error
		checks, err = record.NewStore().ProductionChecksOf(t.Context(), tx, issue)
		return err
	})
	return checks
}

// Each merge of one run opens a check of its own, of the issue's run, and leaves the run's other
// checks open beside it; a merge seen again — whichever of the run's checks is its — opens nothing.
// The merge of a later run closes each check of an earlier run still open as not run, with its
// look closed: nothing would look at it again, and an open check left so would keep a tree's
// linger waiting for ever (closeCheck re-arms the linger's close only when the tree's last open
// check closes).
func TestEachMergeOfARunOpensItsOwnCheckAndALaterRunsClosesTheEarlier(t *testing.T) {
	rig := newCheckRig(t)
	issue := record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
		Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1}
	seedIssue(t, rig.pool, issue)
	openMerge(t, rig, issue, 1, "merge-1")
	rig.clock.now = checkOpened.Add(time.Hour)
	openMerge(t, rig, issue, 2, "merge-2")
	rig.clock.now = checkOpened.Add(2 * time.Hour)
	openMerge(t, rig, issue, 1, "merge-1")

	checks := checksOfIssue(t, rig, issue.Key)
	if len(checks) != 2 || checks[0].Number != 2 || checks[1].Number != 1 {
		t.Fatalf("the checks of %s after two merges of its run and the first seen again = %+v, want #2's and #1's alone", issue.Key, checks)
	}
	for _, check := range checks {
		if !check.Open() || check.Generation != 7 {
			t.Fatalf("the check of #%d after two merges of one run = %+v, want it open, of run 7", check.Number, check)
		}
	}

	later := issue
	later.Generation = 8
	if _, err := rig.pool.Exec(context.Background(), "update issues set generation = 8 where key = $1", issue.Key); err != nil {
		t.Fatal(err)
	}
	rig.clock.now = checkOpened.Add(3 * time.Hour)
	openMerge(t, rig, later, 3, "merge-3")
	checks = checksOfIssue(t, rig, issue.Key)
	if len(checks) != 3 || checks[0].Number != 3 || !checks[0].Open() || checks[0].Generation != 8 {
		t.Fatalf("the checks of %s after a merge of run 8 = %+v, want #3's open first, of run 8", issue.Key, checks)
	}
	for _, check := range checks[1:] {
		if check.Open() || check.Outcome != record.CheckNotRun || check.LookOpen {
			t.Fatalf("the check of #%d of run 7 after a merge of run 8 = %+v, want it closed not_run with its look closed", check.Number, check)
		}
	}
}

// A check of the run that holds the issue looks while the issue runs: its agent's wake starts its
// next look, and its bound starts its last look, as for an issue that left the workflow. A check
// of an earlier run waits instead, as every check of an issue back in the workflow did before: its
// wake is queued again a look bound later, and its bound closes it as not run.
func TestACheckOfTheRunThatHoldsTheIssueLooksWhileItRuns(t *testing.T) {
	for _, tc := range []struct {
		name       string
		generation uint64
		looks      bool
	}{
		{name: "a check of the run", generation: 7, looks: true},
		{name: "a check of an earlier run", generation: 6, looks: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newCheckRig(t)
			issue := record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
				Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1}
			seedIssue(t, rig.pool, issue)
			wake := checkOpened.Add(3 * time.Hour)
			check := record.ProductionCheck{Issue: issue.Key, Project: issue.Project, Tree: issue.Tree, Repo: "sjawhar/legion", Number: 1, MergeSHA: "merge-1",
				Role: claim.RoleTester, OpenedAt: checkOpened, BoundAt: checkOpened.Add(checkBound), Looks: 1, LookWakeAt: &checkOpened,
				LastWakeKind: wakeMerge, LastWakeDetail: "merge-1", LastWakeAt: &checkOpened, WakeAt: &wake, Generation: tc.generation, Task: "a"}
			seedRecord(t, rig.pool, func(tx pgx.Tx) error { return record.NewStore().PutProductionCheck(t.Context(), tx, check) })

			rig.clock.now = wake
			rig.apply("wake", intake.CheckWakeDue{Issue: issue.Key, Look: 1, OpenedAt: checkOpened, Reason: "the deploy has not carried the merge"})
			got := checkOpenedAt(t, rig, issue.Key, checkOpened)
			starts := superviseRowsOf(t, rig.pool, issue.Key, "start")
			if tc.looks {
				if got.Looks != 2 || !got.LookOpen || len(starts) != 1 || starts[0].CheckLook != 2 || !starts[0].CheckOpenedAt.Equal(checkOpened) {
					t.Fatalf("the check at its wake = %+v with starts %+v, want look 2 started for it while its run goes on", got, starts)
				}
			} else if got.Looks != 1 || got.LookOpen || len(starts) != 0 || got.WakeAt == nil || !got.WakeAt.Equal(wake.Add(CheckLookBound)) {
				t.Fatalf("the check at its wake = %+v with starts %+v, want look 1 still ended and the wake a look bound later", got, starts)
			}

			if _, err := rig.pool.Exec(context.Background(), "update production_checks set looks = 1, look_open = false where issue = $1", issue.Key); err != nil {
				t.Fatal(err)
			}
			rig.clock.now = check.BoundAt
			rig.apply("bound", intake.CheckBound{Issue: issue.Key})
			got = checkOpenedAt(t, rig, issue.Key, checkOpened)
			if tc.looks && (!got.Open() || !got.Last || !got.LookOpen) {
				t.Fatalf("the check at its bound = %+v, want its last look started while its run goes on", got)
			}
			if !tc.looks && (got.Open() || got.Outcome != record.CheckNotRun) {
				t.Fatalf("the check at its bound = %+v, want it closed as not run", got)
			}
		})
	}
}

// lookStarts is each start row of key that starts a look of a production check, oldest first.
func lookStarts(t *testing.T, pool *pgxpool.Pool, key string) []record.SuperviseRequest {
	t.Helper()
	var looks []record.SuperviseRequest
	for _, start := range superviseRowsOf(t, pool, key, "start") {
		if start.CheckLook > 0 {
			looks = append(looks, start)
		}
	}
	return looks
}

// Each merge of a task opens its own production check, and its looks run while the issue's other
// tasks go on. Of a (merges) at its merge and b (no needs) at its first step, under
// `production_check: tester`, a's pull request merges: one check is open, of the issue's run, of
// a's merge commit and of task a, with its first look started. That look's pending names a wake and
// suspends nobody, since the issue is in the workflow, and the wake starts look 2 while b still
// implements. b stands where it stood all along: no task waits for a check. Then b reaches its
// merge and its pull request merges, which ends the issue: a second check opens beside the first,
// and both are open.
//
// The fault it catches is a merge that is not the issue's last opening no check, so its work goes
// unchecked in production until the issue's last merge (its control: closeMerged with no openCheck
// at such a merge, under which a's merge opens none).
func TestEachMergeOpensACheckAndItsLooksRunWhileTheIssueRuns(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	clock := &checkClock{now: stepClock}
	engine.cfg.Clock, engine.cfg.ProductionCheckBound = clock.Now, checkBound
	root := stepRoot(t, pool, phase.Implementing)
	seedAAndB(t, pool, engine, root.Key, root.Generation, atMerge, atStep(1), 45)
	b := tasksOf(t, pool, engine, root.Key)["b"]
	apply := applyFacts(t, pool, engine)

	mustSpend(t, "the merge of a's pull request", apply("merged-a", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge-a"}))
	checks := checksOfIssue(t, checkRig{pool: pool}, root.Key)
	if len(checks) != 1 || !checks[0].Open() || checks[0].Generation != root.Generation || checks[0].MergeSHA != "merge-a" || checks[0].Task != "a" ||
		checks[0].Looks != 1 || !checks[0].LookOpen {
		t.Fatalf("the checks of %s after a's merge = %+v, want one open, of run %d, of a's merge-a and task a, at look 1 open", root.Key, checks, root.Generation)
	}
	check := checks[0]
	if starts := lookStarts(t, pool, root.Key); len(starts) != 1 || starts[0].CheckLook != 1 || !starts[0].CheckOpenedAt.Equal(check.OpenedAt) {
		t.Fatalf("the look starts of %s after a's merge = %+v, want one, of look 1 of the check opened at %s", root.Key, starts, check.OpenedAt)
	}

	wake := clock.now.Add(3 * time.Hour)
	if result := apply("pending-1", intake.CheckReport{Issue: root.Key, Look: 1, Verdict: "pending", Summary: "the deploy has not carried merge-a",
		WakeAt: &wake, OpenedAt: check.OpenedAt}); result.Refusal != nil {
		t.Fatalf("look 1's pending = %+v, want it recorded", result.Refusal)
	}
	if rows := issueSuspensions(t, pool, root.Key); len(rows) != 0 {
		t.Fatalf("the issue_suspend rows of %s after look 1's pending = %+v, want none: the issue is in the workflow", root.Key, rows)
	}
	if rows := superviseRowsOf(t, pool, root.Key, "suspend"); len(rows) != 0 {
		t.Fatalf("the suspend rows of %s after look 1's pending = %+v, want none: the issue is in the workflow", root.Key, rows)
	}
	row := onlyWake(t, pool, root.Key)
	if !row.nextAt.Equal(wake) {
		t.Fatalf("the check_wake row of %s is due at %s, want the wake look 1 named, %s", root.Key, row.nextAt, wake)
	}
	due := decodeWake(t, row)
	clock.now = wake
	apply("wake", intake.CheckWakeDue{Issue: root.Key, Look: due.Look, OpenedAt: due.OpenedAt, Reason: due.Reason})
	if starts := lookStarts(t, pool, root.Key); len(starts) != 2 || starts[1].CheckLook != 2 || !starts[1].CheckOpenedAt.Equal(check.OpenedAt) {
		t.Fatalf("the look starts of %s at the wake = %+v, want look 2 of the check opened at %s started, not deferred", root.Key, starts, check.OpenedAt)
	}
	if got := tasksOf(t, pool, engine, root.Key)["b"]; !reflect.DeepEqual(got, b) {
		t.Fatalf("task b after two looks of a's check = %+v, want it as it was, %+v: no task waits for a check", got, b)
	}

	taskMoves(t, pool, engine, root.Key, "b", TriggerImplementationReady, "")
	taskMoves(t, pool, engine, root.Key, "b", TriggerRetroCompleted, "")
	assertTask(t, tasksOf(t, pool, engine, root.Key), "b", record.TaskRunning, atMerge)
	mustSpend(t, "the merge of b's pull request", apply("merged-b", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 45, MergeSHA: "merge-b"}))
	if got := issueAt(t, pool, root.Key); got != phase.Done {
		t.Fatalf("%s after b's merge stands in %s, want done: b's merge was its last", root.Key, got)
	}
	checks = checksOfIssue(t, checkRig{pool: pool}, root.Key)
	if len(checks) != 2 || checks[0].MergeSHA != "merge-b" || checks[1].MergeSHA != "merge-a" {
		t.Fatalf("the checks of %s after b's merge = %+v, want b's then a's", root.Key, checks)
	}
	for _, got := range checks {
		if !got.Open() || got.Generation != root.Generation {
			t.Fatalf("the check of %s after b's merge = %+v, want it open, of run %d", got.MergeSHA, got, root.Generation)
		}
	}
}

// A merge of a lingering tree's member that is not the member's last opens the check of that merge.
// Of LEGION-209 under the lingering LEGION-208, a and c each merging by itself, a at its merge and c
// at its first step, a's pull request merges: a is done, the member stays in the workflow, and one
// check is open, of a's merge, whose first look is a wake of the checking role's claim, which the
// linger suspended.
//
// The fault it catches is a lingering member's merge left unchecked until the member's last merge
// (its control: the lingering branch of closeMerged returning with no openCheck where the merge is
// not the last, under which no check opens).
func TestALingeringMembersMergeThatIsNotItsLastOpensItsCheck(t *testing.T) {
	pool := migratedPool(t)
	engine, logged := loggedEngine()
	until := stepClock.Add(time.Hour)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
		Project: "LEGION", Title: "root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "U", LingerUntil: &until, LastDispatchSeq: 1})
	child := stepChild(t, pool, heldParentChild, phase.Implementing, nil)
	seedTasksAt(t, pool, child.Key, child.Generation,
		taskAt{id: "a", state: record.TaskRunning, place: atMerge, number: 43, merges: true},
		taskAt{id: "c", state: record.TaskRunning, place: atStep(1), number: 45, merges: true})

	mustSpend(t, "the merge of a's pull request", applyFacts(t, pool, engine)("merged-a", mergedFact(43, "a")))
	assertTask(t, tasksOf(t, pool, engine, child.Key), "a", record.TaskDone, atMerge)
	if member := issueOf(t, pool, engine, child.Key); !record.InWorkflow(member) {
		t.Fatalf("%s after a's merge stands in %s with status %q, want it in the workflow: c goes on", child.Key, member.Phase, member.Status)
	}
	checksAre(t, pool, child.Key, "merge-a")
	if check := productionCheck(t, pool, child.Key); check == nil || !check.Open() || check.Looks != 1 || !check.LookOpen {
		t.Fatalf("the check of %s after a's merge = %+v, want it open at look 1", child.Key, check)
	}
	opened := false
	for _, line := range strings.Split(logged.String(), "\n") {
		if strings.Contains(line, "production check opened") && strings.Contains(line, "issue="+child.Key) {
			opened = strings.Contains(line, "firstLook=wake")
		}
	}
	if !opened {
		t.Fatalf("the log of a's merge = %q, want the check of %s opened with its first look a wake", logged.String(), child.Key)
	}
}

// A re-entered child's next run opens no check of a merge of its earlier run. LEGION-209's run of
// generation 3 merged task a's pull request #1, on the issue's own branch, and no check of that
// merge exists. A person sets the child todo, which re-enters it at generation 4, whose plan is one
// task a that has not started, so it holds the issue's own branch (record.StoredPlan.TasksOfBranch).
// A person then sets it done before a opens a pull request: it holds no check. What keeps #1 out of
// that leave is that a new generation deletes the issue's merged and closed pull requests
// (record.Store.ClearGeneration); openCheck's compare with the issue's earlier checks has no check
// of #1 to find here.
//
// The fault it catches is a merge of an earlier run read as a merge of the run that leaves, so its
// check opens in a run that never made it (its control: a ClearGeneration that keeps merged rows,
// under which the leave reads #1 as a's merge and opens a check of it).
func TestAReenteredChildsNextRunOpensNoCheckOfTheEarlierRunsMerge(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Implementing)
	seedTreeLifecycle(t, pool, root.Key)
	parent := root.Key
	child := record.Issue{Key: heldParentChild, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: root.Key, Project: "LEGION",
		Title: heldParentChild, Parent: &parent, Phase: phase.Done, Generation: 3, Status: "done", DispatchStatus: "done", Rank: "V", LastDispatchSeq: 1}
	seedIssue(t, pool, child)
	seedTasksAt(t, pool, child.Key, child.Generation, taskAt{id: "a", state: record.TaskDone, place: atMerge, merges: true, number: 1, merged: true})
	if got := productionChecksOf(t, pool, child.Key); len(got) != 0 {
		t.Fatalf("premise: the checks of %s before its re-entry = %v, want none", child.Key, got)
	}
	apply := applyFacts(t, pool, engine)

	apply("child-todo", statusMove(child.Key, root.Key, "todo", 2))
	reentered := issueOf(t, pool, engine, child.Key)
	if reentered.Generation != 4 || reentered.Phase != phase.Admitted {
		t.Fatalf("premise: %s after the person's todo stands in %s at generation %d, want admitted at 4", child.Key, reentered.Phase, reentered.Generation)
	}
	seedTasks(t, pool, child.Key, reentered.Generation, planTask{id: "a", steps: []string{"implementer"}, agents: []claim.Agent{"implementer"}, state: record.TaskWaiting})
	apply("child-done", statusMove(child.Key, root.Key, "done", 3))
	if got := issueOf(t, pool, engine, child.Key); got.Phase != phase.Done {
		t.Fatalf("premise: %s after the person's done stands in %s, want done", child.Key, got.Phase)
	}
	checksAre(t, pool, child.Key)
}

// A carried orphan's leave opens no second check of a merge whose check closed before the
// hand-over. LEGION-208 stands as an orphan's re-admission that carries its run leaves it: a root of
// its own at generation 4, its plan of a (merges) and b carried, a done at its merge with its pull
// request #42 merged and carried to generation 4, b at its first step; the check of a's merge,
// opened at generation 3, closed as recorded before the hand-over. A person sets it done: it holds
// that one check of a's merge and no other.
//
// The fault it catches is a merge checked twice because the run that carried it reads the check of
// its earlier generation as none of its own (its control: openCheck comparing only with the checks
// of the issue's generation, under which a second check of a's merge opens).
func TestACarriedOrphansLeaveOpensNoSecondCheckOfItsMerge(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	orphan := record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
		Project: "LEGION", Title: "orphan", Phase: phase.Implementing, Generation: 4, Status: "in_progress", Rank: "U", LastDispatchSeq: 1, HandedOver: true}
	seedIssue(t, pool, orphan)
	seedTasksAt(t, pool, orphan.Key, orphan.Generation,
		taskAt{id: "a", state: record.TaskDone, place: atMerge, merges: true, number: 42, merged: true},
		taskAt{id: "b", state: record.TaskRunning, place: atStep(1), number: 45})
	if _, err := pool.Exec(context.Background(), "update pull_requests set generation = $2 where issue = $1", orphan.Key, orphan.Generation); err != nil {
		t.Fatal(err)
	}
	opened, closed := stepClock.Add(-48*time.Hour), stepClock.Add(-24*time.Hour)
	recorded := record.ProductionCheck{Issue: orphan.Key, Project: orphan.Project, Tree: orphan.Tree, Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge-a",
		Role: claim.RoleTester, OpenedAt: opened, BoundAt: opened.Add(checkBound), Looks: 1, LookWakeAt: &opened, LastWakeKind: wakeMerge,
		LastWakeDetail: "merge-a", LastWakeAt: &opened, ClosedAt: &closed, Outcome: record.CheckRecorded, Generation: 3, Task: "a"}
	seedRecord(t, pool, func(tx pgx.Tx) error { return record.NewStore().PutProductionCheck(t.Context(), tx, recorded) })

	mustSpend(t, "the person's done", applyFacts(t, pool, engine)("done", statusMove(orphan.Key, "", "done", 2)))
	if got := issueOf(t, pool, engine, orphan.Key); got.Phase != phase.Done {
		t.Fatalf("premise: %s after the person's done stands in %s, want done", orphan.Key, got.Phase)
	}
	checks := checksOfIssue(t, checkRig{pool: pool}, orphan.Key)
	if len(checks) != 1 || !checks[0].OpenedAt.Equal(opened) || checks[0].Open() || checks[0].Generation != 3 {
		t.Fatalf("the checks of %s after its done = %+v, want the one of a's merge, closed at generation 3, and no other", orphan.Key, checks)
	}
}

// claimRows is each supervise row of key of op that names token as its claim, oldest first.
func claimRows(t *testing.T, pool *pgxpool.Pool, key, op string, token claim.Token) []record.SuperviseRequest {
	t.Helper()
	var rows []record.SuperviseRequest
	for _, row := range superviseRowsOf(t, pool, key, op) {
		if row.Claim == token {
			rows = append(rows, row)
		}
	}
	return rows
}

// seedAWithATesterAndB is LEGION-209, a child of the running LEGION-208, whose plan is a
// (`implementer tester`, its agents named implementer and tester) and b (`implementer`, its agent
// implementer-b), each merging by itself: a at its merge with the child's pull request #43 open on
// the issue's own branch, and b at bAt with its pull request #45 open on its own branch.
func seedAWithATesterAndB(t *testing.T, pool *pgxpool.Pool, engine *Engine, bAt record.TaskPlace) record.Issue {
	t.Helper()
	stepRoot(t, pool, phase.Implementing)
	child := stepChild(t, pool, heldParentChild, phase.Implementing, nil)
	seedChildPR(t, pool, record.PullRequestOpen)
	seedTasks(t, pool, child.Key, child.Generation,
		planTask{id: "a", steps: []string{"implementer", "tester"}, agents: []claim.Agent{"implementer", "tester"}, state: record.TaskRunning, place: atMerge},
		planTask{id: "b", steps: []string{"implementer"}, agents: []claim.Agent{"implementer-b"}, state: record.TaskRunning, place: bAt})
	markMerges(t, pool, child.Key, "a")
	markMerges(t, pool, child.Key, "b")
	seedTaskBranch(t, pool, child.Key, "a", claim.IssueBranch(child.Key))
	seedTaskBranch(t, pool, child.Key, "b", claim.IssueBranch(child.Key)+".b")
	seedTaskPR(t, pool, engine, child.Key, "b", 45)
	return child
}

// Each row of a check that names a claim names the claim of the check's agent. Of LEGION-209's a
// (`implementer tester`) and b (`implementer`), a merges: its check's agent is a's tester, and its
// first look's start row names the tester's claim. b reaches its merge and merges, which ends the
// issue: its check's agent is the fresh check-b, since b has no tester step, and its first look's
// start row names check-b's claim; that check's pending report queues the suspend of check-b's
// claim, and its pass queues the issue_close of check-b's claim. a's check's pass queues the
// issue_close of the tester's claim.
//
// The fault it catches is a row of a check reaching another agent of the checking role (its
// control: startLook with no claim on its row, which the runner then reads as the claim of the
// worker on the tester's step, so check-b's start row names no claim of check-b's).
func TestEachRowOfACheckNamesItsAgentsClaim(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	clock := &checkClock{now: stepClock}
	engine.cfg.Clock, engine.cfg.ProductionCheckBound = clock.Now, checkBound
	child := seedAWithATesterAndB(t, pool, engine, atStep(1))
	apply := applyFacts(t, pool, engine)
	tester, err := claim.AgentToken("legion", child.Key, "tester")
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := claim.AgentToken("legion", child.Key, "check-b")
	if err != nil {
		t.Fatal(err)
	}

	mustSpend(t, "the merge of a's pull request", apply("merged-a", mergedFact(43, "a")))
	first := productionCheck(t, pool, child.Key)
	if first == nil || first.Task != "a" {
		t.Fatalf("the check of %s after a's merge = %+v, want one, of task a", child.Key, first)
	}
	if starts := lookStarts(t, pool, child.Key); len(starts) != 1 || starts[0].Claim != tester {
		t.Fatalf("the look starts of %s after a's merge = %+v, want one naming the tester's claim %s", child.Key, starts, tester)
	}

	taskMoves(t, pool, engine, child.Key, "b", TriggerImplementationReady, "")
	taskMoves(t, pool, engine, child.Key, "b", TriggerRetroCompleted, "")
	mustSpend(t, "the merge of b's pull request", apply("merged-b", mergedFact(45, "b")))
	if got := issueAt(t, pool, child.Key); got != phase.Done {
		t.Fatalf("%s after b's merge stands in %s, want done: b's merge was its last", child.Key, got)
	}
	second := productionCheck(t, pool, child.Key)
	if second == nil || second.Task != "b" || second.OpenedAt.Equal(first.OpenedAt) {
		t.Fatalf("the newest check of %s after b's merge = %+v, want one of task b", child.Key, second)
	}
	starts := lookStarts(t, pool, child.Key)
	if len(starts) != 2 || !starts[1].CheckOpenedAt.Equal(second.OpenedAt) || starts[1].Claim != fresh {
		t.Fatalf("the look starts of %s after b's merge = %+v, want b's check's naming check-b's claim %s", child.Key, starts, fresh)
	}

	if result := apply("pending-b", intake.CheckReport{Issue: child.Key, Look: 1, Verdict: "pending", Summary: "the deploy has not carried merge-b",
		OpenedAt: second.OpenedAt}); result.Refusal != nil {
		t.Fatalf("b's check's pending = %+v, want it recorded", result.Refusal)
	}
	if rows := claimRows(t, pool, child.Key, "suspend", fresh); len(rows) != 1 {
		t.Fatalf("the suspend rows of %s naming check-b's claim after its pending = %+v, want one", child.Key, rows)
	}
	if rows := claimRows(t, pool, child.Key, "suspend", tester); len(rows) != 0 {
		t.Fatalf("the suspend rows of %s naming the tester's claim after b's check's pending = %+v, want none", child.Key, rows)
	}
	if result := apply("pass-b", intake.CheckReport{Issue: child.Key, Look: 1, Verdict: "pass", Summary: "driven in production",
		OpenedAt: second.OpenedAt}); result.Refusal != nil {
		t.Fatalf("b's check's pass = %+v, want it recorded", result.Refusal)
	}
	if rows := claimRows(t, pool, child.Key, "issue_close", fresh); len(rows) != 1 {
		t.Fatalf("the issue_close rows of %s naming check-b's claim after its pass = %+v, want one", child.Key, rows)
	}

	if result := apply("pass-a", intake.CheckReport{Issue: child.Key, Look: 1, Verdict: "pass", Summary: "driven in production",
		OpenedAt: first.OpenedAt}); result.Refusal != nil {
		t.Fatalf("a's check's pass = %+v, want it recorded", result.Refusal)
	}
	if rows := claimRows(t, pool, child.Key, "issue_close", tester); len(rows) != 1 {
		t.Fatalf("the issue_close rows of %s naming the tester's claim after a's check's pass = %+v, want one", child.Key, rows)
	}
}

// A look of a merge's check and a step of another task go to one claim where one agent works both.
// Of LEGION-209's a and b, each `implementer tester:k`, so one agent, tester, works the tester step
// of each: a merges, and its check's first look starts on the tester's claim (its task's last agent
// of the checking role); b passes its implementer step, and its tester step starts on that same
// claim. One claim runs one turn at a time (supervise's TestALookAndAStepOfOneAgentAreSentOneTurnAtATime),
// so the look and the step take that agent in turn, and no task of the workflow waits for the look.
func TestALookAndAnotherTasksStepOfOneAgentGoToOneClaim(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	stepRoot(t, pool, phase.Implementing)
	child := stepChild(t, pool, heldParentChild, phase.Implementing, nil)
	seedChildPR(t, pool, record.PullRequestOpen)
	seedTasks(t, pool, child.Key, child.Generation,
		planTask{id: "a", steps: []string{"implementer", "tester:k"}, agents: []claim.Agent{"implementer", "tester"}, state: record.TaskRunning, place: atMerge},
		planTask{id: "b", steps: []string{"implementer", "tester:k"}, agents: []claim.Agent{"implementer-b", "tester"}, state: record.TaskRunning, place: atStep(1)})
	markMerges(t, pool, child.Key, "a")
	markMerges(t, pool, child.Key, "b")
	seedTaskBranch(t, pool, child.Key, "a", claim.IssueBranch(child.Key))
	seedTaskBranch(t, pool, child.Key, "b", claim.IssueBranch(child.Key)+".b")
	seedTaskPR(t, pool, engine, child.Key, "b", 45)
	tester, err := claim.AgentToken("legion", child.Key, "tester")
	if err != nil {
		t.Fatal(err)
	}

	mustSpend(t, "the merge of a's pull request", applyFacts(t, pool, engine)("merged-a", mergedFact(43, "a")))
	if starts := lookStarts(t, pool, child.Key); len(starts) != 1 || starts[0].Claim != tester {
		t.Fatalf("the look starts of %s after a's merge = %+v, want one naming the tester's claim %s", child.Key, starts, tester)
	}
	taskMoves(t, pool, engine, child.Key, "b", TriggerImplementationReady, "")
	assertTask(t, tasksOf(t, pool, engine, child.Key), "b", record.TaskRunning, atStep(2))
	var steps []record.SuperviseRequest
	for _, start := range superviseRowsOf(t, pool, child.Key, "start") {
		if start.CheckLook == 0 && start.Role == claim.RoleTester {
			steps = append(steps, start)
		}
	}
	if len(steps) != 1 || steps[0].Claim != tester {
		t.Fatalf("the tester's step starts of %s after b's implementer step = %+v, want one naming the tester's claim %s, the look's", child.Key, steps, tester)
	}
}

// storeClaim stores token as a claim of role on key, idle, in key's tree and at its generation, as
// the supervisor stores the claim of each agent it ran there: what the close's walk over every
// claim of an issue reads (record.Store.IssueClaims).
func storeClaim(t *testing.T, pool *pgxpool.Pool, key string, token claim.Token, role claim.Role) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		select $1, 'legion', tree, key, $3, generation, '', '', 'idle', 0, 0, 0, 0 from issues where key = $2
		on conflict (token) do nothing`, string(token), key, string(role)); err != nil {
		t.Fatalf("store the claim %s of %s: %v", token, key, err)
	}
}

// closeRowsNaming is how many issue_close rows of key name token as their claim.
func closeRowsNaming(t *testing.T, pool *pgxpool.Pool, key string, token claim.Token) int {
	t.Helper()
	return len(claimRows(t, pool, key, "issue_close", token))
}

// The close of an issue keeps the claim of each production check that stays open past it: each
// check of the issue already open, and each check the close opens itself. LEGION-209's a
// (`implementer tester`) and b (`implementer`) both merge: a merges first, and its check goes to a's
// tester; then b's merge ends the issue, and its check goes to the fresh check-b, since b has no
// tester step. A claim of each role of Legion is stored, and so is check-b's, as the supervisor
// stores each claim it runs. The close queues one issue_close row for each stored claim of the
// issue but the tester's and check-b's, the claims of the two open checks, and no IssueSuspend: the
// checks still look in the issue's pod, and each check's own close suspends it.
//
// The faults it catches are the close retiring the agent of an open check, so its next look starts
// in a lost session (its control: the leave keeping the claim of the check role's worker alone,
// under which check-b's claim gets an issue_close row), and the close retiring the agent of a check
// it opens itself (its control: the leave keeping the claims of the checks open before it alone,
// under which check-b's claim, the check this close opens, gets one too).
func TestTheCloseOfTheIssueKeepsTheClaimOfEachOpenCheck(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	child := seedAWithATesterAndB(t, pool, engine, atStep(1))
	seedRoleClaims(t, pool, child.Key)
	fresh, err := claim.AgentToken("legion", child.Key, "check-b")
	if err != nil {
		t.Fatal(err)
	}
	storeClaim(t, pool, child.Key, fresh, claim.RoleTester)
	apply := applyFacts(t, pool, engine)

	mustSpend(t, "the merge of a's pull request", apply("merged-a", mergedFact(43, "a")))
	taskMoves(t, pool, engine, child.Key, "b", TriggerImplementationReady, "")
	taskMoves(t, pool, engine, child.Key, "b", TriggerRetroCompleted, "")
	mustSpend(t, "the merge of b's pull request", apply("merged-b", mergedFact(45, "b")))
	if got := issueAt(t, pool, child.Key); got != phase.Done {
		t.Fatalf("%s after b's merge stands in %s, want done: b's merge was its last", child.Key, got)
	}
	checks := checksOfIssue(t, checkRig{pool: pool}, child.Key)
	if len(checks) != 2 || !checks[0].Open() || checks[0].Task != "b" || !checks[1].Open() || checks[1].Task != "a" {
		t.Fatalf("the checks of %s after its close = %+v, want b's and a's, both open", child.Key, checks)
	}

	for _, role := range claim.Roles {
		token, err := claim.NewToken("legion", child.Key, role)
		if err != nil {
			t.Fatal(err)
		}
		want := 1
		if role == claim.RoleTester {
			want = 0
		}
		if got := closeRowsNaming(t, pool, child.Key, token); got != want {
			t.Errorf("the issue_close rows of %s naming the %s's claim = %d, want %d", child.Key, role, got, want)
		}
	}
	if got := closeRowsNaming(t, pool, child.Key, fresh); got != 0 {
		t.Errorf("the issue_close rows of %s naming check-b's claim = %d, want none: b's check is open", child.Key, got)
	}
	if rows := issueSuspensions(t, pool, child.Key); len(rows) != 0 {
		t.Errorf("the issue_suspend rows of %s after its close = %+v, want none: its checks still look in its pod", child.Key, rows)
	}
}

// The close of a child of one merge keeps the claim of the check that close opens, and the child's
// Sandbox. LEGION-209's one task a (`implementer tester`) merges and ends the issue: its check opens
// in that very close, after the close's walk over every claim, and its agent is a's tester. No
// issue_close row names the tester's claim, no IssueSuspend is queued, and the check is open with
// its first look's start row naming the tester's claim.
//
// The fault it catches is the close of a child retiring the agent of the check it opens, and
// deleting the pod that check's look runs in (its control: the leave keeping the claims of the
// checks open before it alone, under which the tester's claim gets an issue_close row and one
// IssueSuspend is queued).
func TestTheCloseOfAChildOfOneMergeKeepsItsCheckersClaimAndItsSandbox(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	stepRoot(t, pool, phase.Implementing)
	child := stepChild(t, pool, heldParentChild, phase.Merging, nil)
	seedChildPR(t, pool, record.PullRequestOpen)
	seedTasks(t, pool, child.Key, child.Generation,
		planTask{id: "a", steps: []string{"implementer", "tester"}, agents: []claim.Agent{"implementer", "tester"}, state: record.TaskRunning, place: atMerge})
	seedTaskBranch(t, pool, child.Key, "a", claim.IssueBranch(child.Key))
	seedRoleClaims(t, pool, child.Key)
	tester, err := claim.NewToken("legion", child.Key, claim.RoleTester)
	if err != nil {
		t.Fatal(err)
	}

	mustSpend(t, "the merge of a's pull request", applyFacts(t, pool, engine)("merged-a", mergedFact(43, "a")))
	if got := issueAt(t, pool, child.Key); got != phase.Done {
		t.Fatalf("%s after a's merge stands in %s, want done: a's merge was its last", child.Key, got)
	}
	if got := closeRowsNaming(t, pool, child.Key, tester); got != 0 {
		t.Errorf("the issue_close rows of %s naming the tester's claim = %d, want none: the close opened the tester's check", child.Key, got)
	}
	if rows := issueSuspensions(t, pool, child.Key); len(rows) != 0 {
		t.Errorf("the issue_suspend rows of %s after its close = %+v, want none: its check looks in its pod", child.Key, rows)
	}
	check := productionCheck(t, pool, child.Key)
	starts := lookStarts(t, pool, child.Key)
	if check == nil || !check.Open() || len(starts) != 1 || starts[0].Claim != tester || !starts[0].CheckOpenedAt.Equal(check.OpenedAt) {
		t.Errorf("the check of %s after its close = %+v with look starts %+v, want it open, its first look naming the tester's claim %s", child.Key, check, starts, tester)
	}
}
