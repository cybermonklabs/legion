package workflow

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The tests of the production check in verifying (D6): it opens at the first fresh read after the
// entry, its looks run while the issue waits, the pod stays, its verdicts feed the close, the look
// after not_run, and the daily notice. Each drives the waitRig of close_wait_test.go with the
// tester as the project's production check.

// newBoundWaitRig is newWaitRig with a production check bound of bound.
func newBoundWaitRig(t *testing.T, bound time.Duration) *waitRig {
	t.Helper()
	rig := &waitRig{t: t, pool: migratedPool(t), now: waitMergedAt.Add(time.Minute)}
	rig.engine = New(record.NewStore(), Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour,
		ProductionCheckBound: bound, ProductionCheckRole: claim.RoleTester, Clock: func() time.Time { return rig.now }}, nil)
	rig.apply = applyFacts(t, rig.pool, rig.engine)
	return rig
}

// mergedRoot is a rig whose root merged and waits in verifying, its first read of the checks not
// yet applied.
func mergedRoot(t *testing.T, rig *waitRig) {
	t.Helper()
	rig.seedRoot(phase.Merging, "retro")
	rig.mergeRoot()
}

// lookStarts is issue's start rows of a production check's look, oldest first.
func (r *waitRig) lookStarts(issue string) []record.SuperviseRequest {
	r.t.Helper()
	var looks []record.SuperviseRequest
	for _, start := range superviseRowsOf(r.t, r.pool, issue, "start") {
		if start.CheckLook > 0 {
			looks = append(looks, start)
		}
	}
	return looks
}

// check is issue's production check, failing the test where it has none.
func (r *waitRig) check(issue string) record.ProductionCheck {
	r.t.Helper()
	check := productionCheck(r.t, r.pool, issue)
	if check == nil {
		r.t.Fatalf("%s has no production check", issue)
	}
	return *check
}

// report is the tester's report of look on issue's check, as handoff_complete applies it with the
// checkOpenedAt the look's own task named: the test reads that moment from the look's start row
// and holds it to the check's, so the report names the check its task named.
func (r *waitRig) report(issue string, look int, verdict, summary string, wake *time.Time) intake.Result {
	r.t.Helper()
	starts := r.lookStarts(issue)
	if len(starts) == 0 {
		r.t.Fatalf("%s has no look started for its report of look %d", issue, look)
	}
	named := starts[len(starts)-1]
	if named.CheckLook != look || !named.CheckOpenedAt.Equal(r.check(issue).OpenedAt) {
		r.t.Fatalf("the look's task names look %d of the check opened %s, want look %d of the check opened %s", named.CheckLook, named.CheckOpenedAt,
			look, r.check(issue).OpenedAt)
	}
	return r.fact(intake.CheckReport{Issue: issue, Look: look, Verdict: verdict, Summary: summary, WakeAt: wake})
}

// lastLook comes to the check's bound and starts its last look. A look still open first ends at
// its own bound, nobody having reported it, so the check's bound starts the last look.
func (r *waitRig) lastLook(issue string) {
	r.t.Helper()
	if check := r.check(issue); check.LookOpen {
		r.now = r.now.Add(CheckLookBound)
		r.fact(intake.CheckLookEnded{Issue: issue, Look: check.Looks, Cause: "look bound", OpenedAt: check.OpenedAt})
	}
	r.now = r.check(issue).BoundAt.Add(time.Second)
	r.fact(intake.CheckBound{Issue: issue})
	if check := r.check(issue); !check.Last || !check.LookOpen {
		r.t.Fatalf("the check at its bound = %+v, want its last look open", check)
	}
}

// The first read after the entry opens the check of the issue's generation and newest merge, and
// starts its look 1. The look's task names each check of the read that runs after the merge and
// names no ci_check, with the commit its result is recorded at. The start's fence is the one the
// outbox's checkLookHolds reads, which lets it run for an issue in verifying (TestAProductionCheckFollowsAPlan
// in internal/daemon runs that row).
func TestAProductionCheckOpensAtTheFirstReadAndItsLookStarts(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, claim.RoleTester)
	mergedRoot(t, rig)
	if check := productionCheck(t, rig.pool, waitRoot); check != nil {
		t.Fatalf("the production check at the merge = %+v, want none before the first read", check)
	}
	ci := noResult("c-ci", waitRootMerge)
	ci.CICheck = "pr-checks"
	live := noResult("c-live", waitRootMerge)
	live.Subject = dispatch.CheckSubjectProduction
	rig.read(waitRoot, answerOf(waitRoot, nil, noResult("c-after", waitRootMerge), ci, live))
	check := rig.check(waitRoot)
	if !check.Open() || check.Generation != waitGeneration || check.MergeSHA != waitRootMerge || check.Looks != 1 || !check.LookOpen {
		t.Fatalf("the check after the first read = %+v, want generation %d's on %s, at look 1 open", check, waitGeneration, waitRootMerge)
	}
	starts := rig.lookStarts(waitRoot)
	if len(starts) != 1 || starts[0].CheckLook != 1 || !starts[0].CheckOpenedAt.Equal(check.OpenedAt) {
		t.Fatalf("the look starts = %+v, want look 1 of the check opened %s", starts, check.OpenedAt)
	}
	for _, want := range []string{
		"Check c-after, when after_merge: record its result at a commit of the merge's branch that holds merge commit " + waitRootMerge + ".",
		"Check c-live, when after_merge: record its result at the commit of the build the live system runs, which holds merge commit " + waitRootMerge + ".",
	} {
		if !strings.Contains(starts[0].Task, want) {
			t.Fatalf("the look's task = %q, want it to say %q", starts[0].Task, want)
		}
	}
	if strings.Contains(starts[0].Task, "c-ci") {
		t.Fatalf("the look's task names the check CI runs: %q", starts[0].Task)
	}
	rig.read(waitRoot, answerOf(waitRoot, nil, noResult("c-after", waitRootMerge)))
	if got := len(rig.lookStarts(waitRoot)); got != 1 {
		t.Fatalf("look starts after a second read = %d, want the one: one check for each generation and merge", got)
	}
}

// The check's look starts the tester's claim, the role the project names, and no implementer's.
func TestTheProductionCheckStartsTheTestersClaim(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, claim.RoleTester)
	mergedRoot(t, rig)
	rig.read(waitRoot, answerOf(waitRoot, nil, noResult("c-after", waitRootMerge)))
	starts := superviseRowsOf(t, rig.pool, waitRoot, "start")
	if len(starts) != 1 || starts[0].Role != claim.RoleTester || starts[0].CheckLook != 1 {
		t.Fatalf("the starts after the check opened = %+v, want the tester's look 1 alone", starts)
	}
}

// A fail of a look in verifying records a reason of the next change, of kind production, with the
// report's summary, and starts the next change (the seam the next change's opener replaces).
func TestAProductionCheckFailStartsTheNextChange(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, claim.RoleTester)
	var started []string
	rig.engine.nextChange = func(_ context.Context, _ pgx.Tx, issue record.Issue, reason record.Defect) error {
		started = append(started, issue.Key+"/"+string(reason.Kind))
		return nil
	}
	mergedRoot(t, rig)
	rig.read(waitRoot, answerOf(waitRoot, nil, noResult("c-after", waitRootMerge)))
	rig.report(waitRoot, 1, "fail", "the widget cache serves a stale page in production", nil)
	var kind, text string
	var generation int64
	if err := rig.pool.QueryRow(t.Context(), "select kind, text, generation from defects where issue = $1", waitRoot).Scan(&kind, &text, &generation); err != nil {
		t.Fatalf("read the reason the fail recorded: %v", err)
	}
	if kind != string(record.DefectKindProduction) || text != "the widget cache serves a stale page in production" || generation != waitGeneration {
		t.Fatalf("the reason = %s %q at generation %d, want a production reason with the report's summary at %d", kind, text, generation, waitGeneration)
	}
	if !slices.Equal(started, []string{waitRoot + "/production"}) {
		t.Fatalf("next changes started = %v, want one for the production reason", started)
	}
	if check := rig.check(waitRoot); check.Open() || check.Outcome != record.CheckDefect {
		t.Fatalf("the check after the fail = %+v, want it closed as a defect", check)
	}
	rig.stands(waitRoot, phase.Verifying, "testing")
}

// The last look's pending closes the check not_run: the issue stays in verifying, and the owning
// architect is told once, close-wait-no-result, with the check that has no pass.
func TestAProductionCheckNotRunHoldsTheCloseAndTellsTheOwner(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, claim.RoleTester)
	mergedRoot(t, rig)
	rig.read(waitRoot, answerOf(waitRoot, nil, noResult("c-after", waitRootMerge)))
	wake := rig.now.Add(time.Hour)
	rig.report(waitRoot, 1, "pending", "production runs yesterday's build", &wake)
	rig.lastLook(waitRoot)
	rig.report(waitRoot, 2, "pending", "the requirement is still unobserved", nil)
	if check := rig.check(waitRoot); check.Open() || check.Outcome != record.CheckNotRun {
		t.Fatalf("the check after the last look's pending = %+v, want it closed not_run", check)
	}
	rig.stands(waitRoot, phase.Verifying, "testing")
	notices := noticesOfKind(t, rig.pool, waitRoot, "close-wait-no-result")
	if len(notices) != 1 || !strings.Contains(notices[0].Reason, "not_run") || !strings.Contains(notices[0].Reason, "c-after") {
		t.Fatalf("close-wait-no-result notices = %+v, want one naming not_run and c-after", notices)
	}
}

// A pending that names its next look ten days ahead tells nobody before the check closes: the
// stored wait shows that look as the production check's next look.
func TestAPendingCheckIsShownWithTheNextLookAndNotToldLate(t *testing.T) {
	t.Parallel()
	rig := newBoundWaitRig(t, 21*24*time.Hour)
	mergedRoot(t, rig)
	rig.read(waitRoot, answerOf(waitRoot, nil, noResult("c-after", waitRootMerge)))
	wake := rig.now.Add(10 * 24 * time.Hour).Truncate(time.Microsecond)
	rig.report(waitRoot, 1, "pending", "the check is due in ten days", &wake)
	stored := rig.stored(waitRoot)
	if stored.Production == nil || stored.Production.NextLookAt == nil || !stored.Production.NextLookAt.Equal(wake) {
		t.Fatalf("the stored wait's production check = %+v, want its next look at %s", stored.Production, wake)
	}
	if got := noticesOfKind(t, rig.pool, waitRoot, "close-wait-no-result"); len(got) != 0 {
		t.Fatalf("close-wait-no-result notices before the check closed = %+v, want none", got)
	}
}

// A look that ends with no report names the check's next look, and the stored wait shows that look
// at once, before any tick: at the look's bound, at its checking role's claim failing in it, and at
// that claim failing after the look ended, which moves the next look later.
func TestALookThatEndsWithNoReportShowsItsNextLook(t *testing.T) {
	t.Parallel()
	shows := func(t *testing.T, rig *waitRig) {
		t.Helper()
		check := rig.check(waitRoot)
		stored := rig.stored(waitRoot)
		if check.WakeAt == nil || stored.Production == nil || stored.Production.NextLookAt == nil || !stored.Production.NextLookAt.Equal(*check.WakeAt) {
			t.Fatalf("the stored wait's production check = %+v, want its next look at the check's wake %v", stored.Production, check.WakeAt)
		}
	}
	t.Run("the look's bound", func(t *testing.T) {
		rig := newWaitRig(t, claim.RoleTester)
		mergedRoot(t, rig)
		rig.read(waitRoot, answerOf(waitRoot, nil, noResult("c-after", waitRootMerge)))
		check := rig.check(waitRoot)
		rig.now = rig.now.Add(CheckLookBound)
		rig.fact(intake.CheckLookEnded{Issue: waitRoot, Look: check.Looks, Cause: "look bound", OpenedAt: check.OpenedAt})
		shows(t, rig)
	})
	t.Run("its claim fails in the look", func(t *testing.T) {
		rig := newWaitRig(t, claim.RoleTester)
		mergedRoot(t, rig)
		rig.read(waitRoot, answerOf(waitRoot, nil, noResult("c-after", waitRootMerge)))
		rig.fact(intake.ClaimFailed{Issue: waitRoot, Role: claim.RoleTester, Reason: "the pod was evicted"})
		if check := rig.check(waitRoot); check.LookOpen || check.FailedLooks != 1 {
			t.Fatalf("the check after its claim failed in the look = %+v, want the look ended as a failed look", check)
		}
		shows(t, rig)
	})
	t.Run("its claim fails after the look ended", func(t *testing.T) {
		rig := newWaitRig(t, claim.RoleTester)
		mergedRoot(t, rig)
		rig.read(waitRoot, answerOf(waitRoot, nil, noResult("c-after", waitRootMerge)))
		wake := rig.now.Add(time.Hour).Truncate(time.Microsecond)
		rig.report(waitRoot, 1, "pending", "production runs yesterday's build", &wake)
		rig.fact(intake.ClaimFailed{Issue: waitRoot, Role: claim.RoleTester, Reason: "the pod was evicted"})
		if check := rig.check(waitRoot); check.WakeAt == nil || !check.WakeAt.After(wake) {
			t.Fatalf("the check after its claim failed between looks = %+v, want its next look moved past %s", check, wake)
		}
		shows(t, rig)
	})
}

// The daily notice names each term that holds the close, once each UTC day: two ticks in one day
// tell once, and a tick the next day tells again. A term that no longer stands is not named.
func TestTheDailyTurnNamesEachTermThatStands(t *testing.T) {
	t.Parallel()
	approval := &record.CheckApproval{Version: 1,
		Approved: []dispatch.DocumentBlock{{ID: "c-after", Type: "check", Text: "the page loads"}},
		Current:  []dispatch.DocumentBlock{{ID: "c-after", Type: "check", Text: "the page loads fast"}}}
	for _, tc := range []struct {
		name  string
		role  claim.Role
		setUp func(t *testing.T, rig *waitRig)
		says  string
	}{
		{"unread", "", func(t *testing.T, rig *waitRig) {}, "Its checks are unread since"},
		{"unapproved", "", func(t *testing.T, rig *waitRig) {
			answer := answerOf(waitRoot, nil, passing("c-after", waitRootMerge))
			answer.Approval = approval
			rig.read(waitRoot, answer)
		}, "changed without a person's approval: c-after"},
		{"beneath", "", func(t *testing.T, rig *waitRig) {
			rig.read(waitRoot, answerOf(waitRoot, []dispatch.IssueChild{{Key: waitChild, Status: "in_progress"}}, passing("c-after", waitRootMerge)))
		}, waitChild + " beneath it is in the workflow"},
		{"openChildren", "", func(t *testing.T, rig *waitRig) {
			rig.read(waitRoot, answerOf(waitRoot, []dispatch.IssueChild{{Key: "LEGION-300", Status: "backlog"}}, passing("c-after", waitRootMerge)))
		}, "Its child LEGION-300 is open in Dispatch"},
		{"a check without a pass", "", func(t *testing.T, rig *waitRig) {
			rig.read(waitRoot, answerOf(waitRoot, nil, noResult("c-after", waitRootMerge)))
		}, "Check c-after has no pass since"},
		{"the production check", claim.RoleTester, func(t *testing.T, rig *waitRig) {
			rig.read(waitRoot, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
		}, "Its production check of generation 3 at merge commit " + waitRootMerge},
		{"a reason", "", func(t *testing.T, rig *waitRig) {
			rig.seedDefect(waitRoot, "the retro found the cache key unchecked")
			rig.read(waitRoot, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
		}, "waits since " + waitMergedAt.Add(time.Minute).Format("2006-01-02")},
		{"a reason in a parked tree", "", func(t *testing.T, rig *waitRig) {
			rig.seedDefect(waitRoot, "the retro found the cache key unchecked")
			until := rig.now.Add(30 * 24 * time.Hour)
			root := rig.issue(waitRoot)
			root.LingerUntil = &until
			seedIssue(t, rig.pool, root)
			rig.read(waitRoot, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
		}, "does not run while its root lingers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newWaitRig(t, tc.role)
			rig.seedRoot(phase.Merging, "retro")
			if tc.name == "beneath" {
				rig.seedChild(waitChild, phase.Implementing, "in_progress")
			}
			rig.mergeRoot()
			tc.setUp(t, rig)
			rig.fact(intake.ControllerTick{})
			rig.fact(intake.ControllerTick{})
			told := noticesOfKind(t, rig.pool, waitRoot, "close-wait-standing")
			if len(told) != 1 || !strings.Contains(told[0].Reason, tc.says) {
				t.Fatalf("close-wait-standing notices after two ticks in one day = %+v, want one saying %q", told, tc.says)
			}
			rig.now = rig.now.Add(24 * time.Hour)
			rig.fact(intake.ControllerTick{})
			if told := noticesOfKind(t, rig.pool, waitRoot, "close-wait-standing"); len(told) != 2 {
				t.Fatalf("close-wait-standing notices after a tick the next day = %+v, want two", told)
			}
		})
	}

	t.Run("a term that stands no more is not named", func(t *testing.T) {
		rig := newWaitRig(t, "")
		mergedRoot(t, rig)
		rig.read(waitRoot, answerOf(waitRoot, nil, noResult("c-after", waitRootMerge)))
		rig.fact(intake.ControllerTick{})
		rig.now = rig.now.Add(24 * time.Hour)
		rig.read(waitRoot, answerOf(waitRoot, []dispatch.IssueChild{{Key: "LEGION-300", Status: "backlog"}}, passing("c-after", waitRootMerge)))
		rig.fact(intake.ControllerTick{})
		told := noticesOfKind(t, rig.pool, waitRoot, "close-wait-standing")
		if len(told) != 2 || strings.Contains(told[1].Reason, "c-after") || !strings.Contains(told[1].Reason, "LEGION-300") {
			t.Fatalf("the second day's notice = %+v, want it to name LEGION-300 and not c-after, which passes now", told)
		}
	})
}

// A check of the issue that waits in verifying is deferred while its next change runs: RunsAgain
// is true in implementing of the next generation and false in verifying, and a wake that comes due
// in implementing starts no look.
func TestALookIsDeferredWhileTheNextChangeRuns(t *testing.T) {
	t.Parallel()
	if record.RunsAgain(record.Issue{Phase: phase.Verifying, Status: "testing"}) {
		t.Fatalf("RunsAgain is true in verifying, want false: the looks run while the issue waits")
	}
	if !record.RunsAgain(record.Issue{Phase: phase.Implementing, Status: "in_progress", Generation: waitGeneration + 1}) {
		t.Fatalf("RunsAgain is false in implementing, want true: a next change runs")
	}
	rig := newWaitRig(t, claim.RoleTester)
	mergedRoot(t, rig)
	rig.read(waitRoot, answerOf(waitRoot, nil, noResult("c-after", waitRootMerge)))
	wake := rig.now.Add(time.Hour)
	rig.report(waitRoot, 1, "pending", "production runs yesterday's build", &wake)
	root := rig.issue(waitRoot)
	root.Phase, root.Status, root.Generation = phase.Implementing, "in_progress", waitGeneration+1
	seedIssue(t, rig.pool, root)
	rig.now = wake
	check := rig.check(waitRoot)
	rig.fact(intake.CheckWakeDue{Issue: waitRoot, Look: 1, OpenedAt: check.OpenedAt, WakeAt: wake, Reason: "production runs yesterday's build"})
	if got := len(rig.lookStarts(waitRoot)); got != 1 {
		t.Fatalf("look starts after a wake in the next change = %d, want the first alone: the look is deferred", got)
	}
}

// In verifying nothing of the issue's pod is deleted: a look's end suspends the tester's claim
// alone, and the check's close closes that claim alone. Neither queues an IssueSuspend.
func TestNothingOfThePodIsDeletedInVerifying(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, claim.RoleTester)
	mergedRoot(t, rig)
	rig.read(waitRoot, answerOf(waitRoot, nil, noResult("c-after", waitRootMerge)))
	wake := rig.now.Add(time.Hour)
	rig.report(waitRoot, 1, "pending", "production runs yesterday's build", &wake)
	if suspended := superviseRequests(t, rig.pool, "suspend"); suspended[waitRoot+"/tester"] != 1 || len(suspended) != 1 {
		t.Fatalf("suspend rows after the look's end = %v, want the tester's alone", suspended)
	}
	rig.lastLook(waitRoot)
	rig.report(waitRoot, 2, "pending", "the requirement is still unobserved", nil)
	if closed := superviseRequests(t, rig.pool, "issue_close"); closed[waitRoot+"/tester"] != 1 || len(closed) != 1 {
		t.Fatalf("issue_close rows after the check's close = %v, want the tester's alone", closed)
	}
	if got := issueSuspensions(t, rig.pool, waitRoot); len(got) != 0 {
		t.Fatalf("issue suspensions in verifying = %+v, want none", got)
	}
}

// No check opens where a reason of the next change already waits at the read: the next change's
// merge opens the check of its own merge.
func TestAProductionCheckDoesNotOpenWhereAReasonWaits(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, claim.RoleTester)
	mergedRoot(t, rig)
	rig.seedDefect(waitRoot, "the retro found the cache key unchecked")
	rig.read(waitRoot, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
	if check := productionCheck(t, rig.pool, waitRoot); check != nil {
		t.Fatalf("the check where a reason waits = %+v, want none", check)
	}
}

// No check opens at a read that does not count, nor where a check Dispatch computes as failed waits
// as a reason of the next change. A read whose basis names no merge commit of the newest merge is
// unread, and a check opened on it would count its bound from before Dispatch read the merge. A
// fail's next change opens the check of its own merge. The first read that counts, with no fail,
// opens the check.
func TestAProductionCheckDoesNotOpenAtAnUnreadReadOrAFail(t *testing.T) {
	t.Parallel()
	t.Run("a basis names no merge commit of the newest merge", func(t *testing.T) {
		rig := newWaitRig(t, claim.RoleTester)
		mergedRoot(t, rig)
		rig.read(waitRoot, answerOf(waitRoot, nil, noResult("c-after", "")))
		if stored := rig.stored(waitRoot); stored.Unread == nil {
			t.Fatalf("the stored wait after a read whose basis names no merge commit = %+v, want it unread", stored)
		}
		if check := productionCheck(t, rig.pool, waitRoot); check != nil {
			t.Fatalf("the check after an unread read = %+v, want none", check)
		}
		rig.read(waitRoot, answerOf(waitRoot, nil, noResult("c-after", waitRootMerge)))
		if check := rig.check(waitRoot); !check.Open() || check.MergeSHA != waitRootMerge {
			t.Fatalf("the check after the first read that counts = %+v, want it open on %s", check, waitRootMerge)
		}
	})
	t.Run("a check Dispatch computes as failed", func(t *testing.T) {
		rig := newWaitRig(t, claim.RoleTester)
		mergedRoot(t, rig)
		rig.read(waitRoot, answerOf(waitRoot, nil, check("c-after", waitRootMerge, dispatch.PassStandsNo, dispatch.PassStandsReasonFail)))
		if stored := rig.stored(waitRoot); stored.Unread != nil || len(stored.Checks) != 1 || stored.Checks[0].Reason != dispatch.PassStandsReasonFail {
			t.Fatalf("the stored wait after the fail = %+v, want the read counted and c-after failed", stored)
		}
		if check := productionCheck(t, rig.pool, waitRoot); check != nil {
			t.Fatalf("the check where a computed fail waits = %+v, want none", check)
		}
	})
}

// A merged root whose child still runs opens no check at the reads while the child runs. The
// child's leave queues a read, and that read opens the root's check: its OpenedAt is that read's
// time, and its bound's row is due the project's bound after it (M3).
func TestARootsProductionCheckOpensAfterItsLastChildLeft(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, claim.RoleTester)
	rig.seedRoot(phase.Merging, "retro")
	rig.seedChild(waitChild, phase.Implementing, "in_progress")
	rig.mergeRoot()
	rig.read(waitRoot, answerOf(waitRoot, []dispatch.IssueChild{{Key: waitChild, Status: "in_progress"}}, passing("c-after", waitRootMerge)))
	if check := productionCheck(t, rig.pool, waitRoot); check != nil {
		t.Fatalf("the root's check while its child runs = %+v, want none", check)
	}
	rig.now = rig.now.Add(3 * time.Hour)
	rig.fact(intake.DispatchIssue{Key: waitChild, ResolvedRepo: waitRepo, Seq: 2, Type: "issue.closed", Status: "done", Title: waitChild, Parent: waitRoot, Rank: "V"})
	rig.read(waitRoot, answerOf(waitRoot, []dispatch.IssueChild{{Key: waitChild, Status: "done"}}, passing("c-after", waitRootMerge)))
	check := rig.check(waitRoot)
	if !check.OpenedAt.Equal(rig.now.Truncate(time.Microsecond)) {
		t.Fatalf("the root's check opened %s, want the read's time %s", check.OpenedAt, rig.now)
	}
	bounds := issueOutboxOf(t, rig.pool, waitRoot, "check_bound")
	if len(bounds) != 1 || !bounds[0].nextAt.Equal(check.OpenedAt.Add(48*time.Hour)) {
		t.Fatalf("the root's bound rows = %+v, want one due %s", bounds, check.OpenedAt.Add(48*time.Hour))
	}
}

// A tree that lingers past its deadline waits for each open production check of its members
// (lingerExpired). A member that waits in verifying has its check's close re-arm the tree's close,
// a LingerClose due now, as any member's check close does, and the close deletes nothing of the
// member's pod there: its close on its checks does that.
func TestACheckThatClosesInVerifyingReArmsALingeringTreesClose(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, claim.RoleTester)
	until := rig.now.Add(time.Hour)
	seedIssue(t, rig.pool, record.Issue{Key: waitRoot, Repo: waitRepo, DefaultBranch: "main", GitHubRepo: waitRepo, Tree: waitRoot, Project: "LEGION",
		Title: "root", Phase: phase.Done, Generation: waitGeneration, Status: "done", DispatchStatus: "done", Rank: "U", LingerUntil: &until, LastDispatchSeq: 1})
	rig.seedChild(waitChild, phase.Merging, "retro")
	rig.mergeChild()
	rig.read(waitChild, answerOf(waitChild, nil, noResult("c-after", waitChildMerge)))
	if check := rig.check(waitChild); !check.Open() {
		t.Fatalf("the member's check after its first read = %+v, want it open", check)
	}
	rig.now = until.Add(time.Minute)
	rig.report(waitChild, 1, "pass", "the check passed in production", nil)
	rig.stands(waitChild, phase.Verifying, "testing")
	if closes := issueOutboxOf(t, rig.pool, waitRoot, "linger_close"); len(closes) != 1 || !closes[0].nextAt.Equal(rig.now) {
		t.Fatalf("the root's linger_close rows after its member's check closed = %+v, want one due now, %s", closes, rig.now)
	}
	if got := issueSuspensions(t, rig.pool, waitChild); len(got) != 0 {
		t.Fatalf("the member's suspensions at its check's close in verifying = %+v, want none", got)
	}
}

// After the check closed not_run, a result recorded for a check without a pass opens one more
// look, the last. The same event delivered twice opens one look. That look's pass closes the check
// recorded, and the read after it closes the issue.
func TestALateResultAfterNotRunOpensOneLookAndCloses(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, claim.RoleTester)
	mergedRoot(t, rig)
	rig.read(waitRoot, answerOf(waitRoot, nil, noResult("c-after", waitRootMerge)))
	rig.lastLook(waitRoot)
	rig.report(waitRoot, 2, "pending", "the check is due after the bound", nil)
	late := intake.DispatchResultChanged{Key: waitRoot, Seq: 50, ResultID: "result-late", Check: "c-after", Verdict: "pass"}
	rig.fact(late)
	rig.fact(late)
	check := rig.check(waitRoot)
	if !check.Open() || !check.Last || check.Looks != 3 || !check.LookOpen || check.RelookSeq != 50 {
		t.Fatalf("the check after the late result, twice = %+v, want it open at its last look 3, opened by seq 50", check)
	}
	if got := len(rig.lookStarts(waitRoot)); got != 3 {
		t.Fatalf("look starts = %d, want three: the first, the bound's, and one after the late result", got)
	}
	rig.ranOut(waitRoot, "close_read")
	rig.report(waitRoot, 3, "pass", "the check passed in production", nil)
	if check := rig.check(waitRoot); check.Open() || check.Outcome != record.CheckRecorded {
		t.Fatalf("the check after the pass = %+v, want it closed recorded", check)
	}
	if got := rig.rows(waitRoot, "close_read"); got != 1 {
		t.Fatalf("close_read rows after the check's close = %d, want one", got)
	}
	rig.read(waitRoot, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
	closedOnChecks(t, rig, waitRoot)
}

// A late result's look that closes not_run again is not opened a second time by the same event
// redelivered (relook_seq): only a later result opens the look after it.
func TestARedeliveredLateResultOpensNoSecondLook(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, claim.RoleTester)
	mergedRoot(t, rig)
	rig.read(waitRoot, answerOf(waitRoot, nil, noResult("c-after", waitRootMerge)))
	rig.lastLook(waitRoot)
	rig.report(waitRoot, 2, "pending", "the check is due after the bound", nil)
	late := intake.DispatchResultChanged{Key: waitRoot, Seq: 50, ResultID: "result-late", Check: "c-after", Verdict: "fail"}
	rig.fact(late)
	rig.report(waitRoot, 3, "pending", "the requirement is still unobserved", nil)
	if check := rig.check(waitRoot); check.Open() || check.Outcome != record.CheckNotRun {
		t.Fatalf("the check after the late look's pending = %+v, want it closed not_run again", check)
	}
	rig.fact(late)
	if check := rig.check(waitRoot); check.Open() || check.Looks != 3 {
		t.Fatalf("the check after the same event redelivered = %+v, want it closed at look 3", check)
	}
	later := late
	later.Seq, later.ResultID = 51, "result-later"
	rig.fact(later)
	if check := rig.check(waitRoot); !check.Open() || check.Looks != 4 || check.RelookSeq != 51 {
		t.Fatalf("the check after a later result = %+v, want look 4 open, opened by seq 51", check)
	}
}

// No timer opens a look after not_run: a day passes and the controller ticks, and no look opens.
func TestNoTimerOpensALookAfterNotRun(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, claim.RoleTester)
	mergedRoot(t, rig)
	rig.read(waitRoot, answerOf(waitRoot, nil, noResult("c-after", waitRootMerge)))
	rig.lastLook(waitRoot)
	rig.report(waitRoot, 2, "pending", "the check is due after the bound", nil)
	rig.now = rig.now.Add(24 * time.Hour)
	rig.fact(intake.ControllerTick{})
	if check := rig.check(waitRoot); check.Open() || check.Looks != 2 {
		t.Fatalf("the check a day after not_run = %+v, want it closed at look 2", check)
	}
}

// An issue whose every check passes, with nothing beneath it, waits only for its production check
// of its generation, closed not_run: the stored wait, close-wait.md and the daily notice name it
// with its generation, merge, outcome and way out. Where no check of the issue's generation
// stands, the turn's read opens one.
func TestProductionOnlyWaitIsVisibleAndReconciled(t *testing.T) {
	t.Parallel()
	t.Run("closed not_run", func(t *testing.T) {
		rig := newWaitRig(t, claim.RoleTester)
		mergedRoot(t, rig)
		rig.read(waitRoot, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
		rig.lastLook(waitRoot)
		rig.report(waitRoot, 2, "pending", "the requirement is still unobserved", nil)
		rig.read(waitRoot, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
		stored := rig.stored(waitRoot)
		if stored.Production == nil || stored.Production.Generation != waitGeneration || stored.Production.Merge != waitRootMerge ||
			stored.Production.Outcome != string(record.CheckNotRun) || len(stored.Checks) != 0 {
			t.Fatalf("the stored wait = %+v, want the production check alone, not_run, at generation %d and %s", stored, waitGeneration, waitRootMerge)
		}
		document, err := record.CloseWaitDocument(rig.issue(waitRoot))
		if err != nil || !strings.Contains(document, waitRootMerge) || !strings.Contains(document, "not_run") {
			t.Fatalf("close-wait.md = %q, %v; want it to name the production check's merge and not_run", document, err)
		}
		rig.fact(intake.ControllerTick{})
		told := noticesOfKind(t, rig.pool, waitRoot, "close-wait-standing")
		if len(told) != 1 || !strings.Contains(told[0].Reason, "closed as not_run") || !strings.Contains(told[0].Reason, "Its way out is") {
			t.Fatalf("the daily notice = %+v, want it to name the production check, not_run, and its way out", told)
		}
	})
	t.Run("no check of the issue's generation", func(t *testing.T) {
		rig := newWaitRig(t, claim.RoleTester)
		mergedRoot(t, rig)
		rig.seedCheck(waitRoot, waitGeneration-1, waitRootMerge, record.CheckRecorded)
		rig.ranOut(waitRoot, "close_read")
		rig.fact(intake.ControllerTick{})
		if got := rig.rows(waitRoot, "close_read"); got != 1 {
			t.Fatalf("close_read rows after the turn = %d, want one", got)
		}
		rig.read(waitRoot, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
		if check := rig.check(waitRoot); check.Generation != waitGeneration || !check.Open() {
			t.Fatalf("the check after the turn's read = %+v, want one of generation %d open", check, waitGeneration)
		}
	})
}

// Under `production_check: off` no role of the project runs a check after the merge (D5). So at
// the first read that counts after the entry to verifying the owning architect is told, once, each
// check that has no pass, and that an eligible agent must run it. A second read with the same
// checks tells nothing more. A read that counts only in part, some of its checks naming no merge
// commit of the newest merge, tells nothing, and the read that counts after it tells. A project
// that runs a production check tells this at the check's close instead.
func TestAnOffProjectIsToldAtTheEntryWhichChecksNeedAnAgent(t *testing.T) {
	t.Parallel()
	t.Run("production check off", func(t *testing.T) {
		rig := newWaitRig(t, "")
		mergedRoot(t, rig)
		rig.read(waitRoot, answerOf(waitRoot, nil, noResult("c-after", waitRootMerge), passing("c-done", waitRootMerge)))
		told := noticesOfKind(t, rig.pool, waitRoot, "close-wait-no-result")
		if len(told) != 1 || !strings.Contains(told[0].Reason, "- c-after") || strings.Contains(told[0].Reason, "c-done") ||
			!strings.Contains(told[0].Reason, "eligible agent") {
			t.Fatalf("close-wait-no-result notices = %+v, want one naming c-after alone and an eligible agent", told)
		}
		rig.read(waitRoot, answerOf(waitRoot, nil, noResult("c-after", waitRootMerge), passing("c-done", waitRootMerge)))
		if told := noticesOfKind(t, rig.pool, waitRoot, "close-wait-no-result"); len(told) != 1 {
			t.Fatalf("close-wait-no-result notices after a second read = %+v, want the one", told)
		}
	})
	t.Run("production check on", func(t *testing.T) {
		rig := newWaitRig(t, claim.RoleTester)
		mergedRoot(t, rig)
		rig.read(waitRoot, answerOf(waitRoot, nil, noResult("c-after", waitRootMerge)))
		if told := noticesOfKind(t, rig.pool, waitRoot, "close-wait-no-result"); len(told) != 0 {
			t.Fatalf("close-wait-no-result notices at the first read with a production check = %+v, want none before its close", told)
		}
	})
	t.Run("after a read that counts in part", func(t *testing.T) {
		rig := newWaitRig(t, "")
		mergedRoot(t, rig)
		rig.read(waitRoot, answerOf(waitRoot, nil, noResult("c-after", waitRootMerge), passing("c-done", "")))
		if stored := rig.stored(waitRoot); stored.Unread == nil || len(stored.Checks) != 1 {
			t.Fatalf("the stored wait after a read that counts in part = %+v, want it unread with c-after listed", stored)
		}
		if told := noticesOfKind(t, rig.pool, waitRoot, "close-wait-no-result"); len(told) != 0 {
			t.Fatalf("close-wait-no-result notices before a read counts = %+v, want none", told)
		}
		rig.read(waitRoot, answerOf(waitRoot, nil, noResult("c-after", waitRootMerge), passing("c-done", waitRootMerge)))
		told := noticesOfKind(t, rig.pool, waitRoot, "close-wait-no-result")
		if len(told) != 1 || !strings.Contains(told[0].Reason, "- c-after") || strings.Contains(told[0].Reason, "c-done") {
			t.Fatalf("close-wait-no-result notices at the first read that counts = %+v, want one naming c-after alone", told)
		}
	})
}

// openCheck opens one check for each generation and merge: a second open in one generation opens
// nothing, and a check of the same merge from an earlier generation, closed recorded, is replaced
// by one of the issue's generation.
func TestOpenCheckOpensOneCheckForEachGenerationAndMerge(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, claim.RoleTester)
	mergedRoot(t, rig)
	root := rig.issue(waitRoot)
	open := func() {
		t.Helper()
		factTx(t, rig.pool, rig.engine, func(ctx context.Context, tx pgx.Tx) error {
			return rig.engine.openCheck(ctx, tx, root, root.Merge, false)
		})
	}
	open()
	first := rig.check(waitRoot)
	rig.now = rig.now.Add(time.Hour)
	open()
	if again := rig.check(waitRoot); !again.OpenedAt.Equal(first.OpenedAt) || again.Looks != 1 {
		t.Fatalf("the check after a second open = %+v, want the first, %s, untouched", again, first.OpenedAt)
	}
	rig.seedCheck(waitRoot, waitGeneration-1, waitRootMerge, record.CheckRecorded)
	rig.now = rig.now.Add(time.Hour)
	open()
	if replaced := rig.check(waitRoot); replaced.Generation != waitGeneration || !replaced.Open() || replaced.MergeSHA != waitRootMerge {
		t.Fatalf("the check after an open over an earlier generation's = %+v, want one of generation %d open on %s", replaced, waitGeneration, waitRootMerge)
	}
}
