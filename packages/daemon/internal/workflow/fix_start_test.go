package workflow

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// fixPage is the address of the page of the CI job a fix's read finds.
const fixPage = "https://ci.example/run/7"

// startedFix is LEGION-208 in reviewing on `implementer tester reviewer`, k-ci failed at fixHead,
// and the read of fix 1's job answered read (its key, run, task and fix filled in here).
func startedFix(t *testing.T, read intake.FixJobRead) *fixRig {
	t.Helper()
	r := reviewing(t)
	r.fails(fixHead, "k-ci")
	r.readJob(read)
	return r
}

// readJob applies the answer of the read of fix 1's job of task a of LEGION-208.
func (r *fixRig) readJob(read intake.FixJobRead) {
	r.t.Helper()
	read.Key, read.Generation, read.Task, read.Fix = "LEGION-208", 1, "a", 1
	applyRefusingNothing(r.t, r.pool, r.engine, read)
}

// fixStarts is each start of a fix the outbox holds for LEGION-208, oldest first.
func (r *fixRig) fixStarts() []record.SuperviseRequest {
	r.t.Helper()
	var starts []record.SuperviseRequest
	for _, start := range superviseRowsOf(r.t, r.pool, "LEGION-208", "start") {
		if start.Fix > 0 {
			starts = append(starts, start)
		}
	}
	return starts
}

// latestFix is fix of LEGION-208's task a as the store holds it now.
func (r *fixRig) latestFix() record.TaskFix {
	r.t.Helper()
	var fix *record.TaskFix
	applyInTx(r.t, r.pool, r.engine, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		fix, err = r.engine.store.LatestFix(ctx, tx, "LEGION-208", 1, "a")
		return err
	})
	if fix == nil {
		r.t.Fatal("LEGION-208's task a holds no fix")
	}
	return *fix
}

// The read of the fix's failed job starts the fix: the fix runs, at an entry raised on the
// implementer's row, and the issue stays in reviewing, its record naming the fix that runs. One
// start of the implementer is queued for implementing with the fix's number and the fix's head as
// the commit its working copy is made at, waiting for no slot, and its task names the check, the
// head, the job, how it ended and its page. The implementer's place starts a second run of its
// step, whose record names the fix's check.
func TestTheFixStartsInItsOwnCopyWithItsJob(t *testing.T) {
	r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})

	assertPhase(t, r.pool, phase.Reviewing)
	assertStrings(t, "the fixes", r.fixes(), []string{fixOf(1, "running", "k-ci")})
	row := roleRowOf(t, r.pool, r.engine, "LEGION-208", claim.RoleImplementer)
	fix := r.latestFix()
	if fix.Entry != row.Entry || fix.JobName != "unit" || fix.JobURL != fixPage || fix.ReadFailed != "" || fix.StartedAt == nil {
		t.Fatalf("the running fix = %+v, want its job unit at %s, started at the implementer's entry %d", fix, fixPage, row.Entry)
	}
	if issue := issueOf(t, r.pool, r.engine, "LEGION-208"); issue.RunningFix != 1 {
		t.Fatalf("the issue read names running fix %d, want 1", issue.RunningFix)
	}
	starts := r.fixStarts()
	if len(starts) != 1 {
		t.Fatalf("the starts of a fix = %+v, want one", starts)
	}
	start := starts[0]
	if start.Role != claim.RoleImplementer || start.Phase != phase.Implementing || start.Fix != 1 || start.FixFrom != fixHead ||
		start.FixBranch != besideFix1(t, claim.RoleImplementer) || start.NeedsSlot || start.Entry != row.Entry || start.Generation != 1 {
		t.Fatalf("the fix's start = %+v, want the implementer's in implementing for fix 1 from %s on its own branch at entry %d, waiting for no slot",
			start, fixHead, row.Entry)
	}
	assertHolds(t, "the fix's task", start.Task, "Phase: implementing.", "Step 1 of 3: implementer.", "k-ci", fixHead, "unit", "failure", fixPage)
	if row.EntryTask != start.Task || row.StepTask != "a" || row.StepPlace != 1 {
		t.Fatalf("the implementer's row holds task %q of step %s/%d, want the fix's task of step a/1", row.EntryTask, row.StepTask, row.StepPlace)
	}
	if got := stepStartCounts(t, r.pool, "LEGION-208"); !slices.Contains(got, "a/step/1=2") {
		t.Fatalf("the starts of LEGION-208 = %v, want a/step/1=2: the fix is the second run of the implementer's step", got)
	}
	var fixRun *record.StepRecord
	for _, write := range stepWrites(t, r.pool, "LEGION-208") {
		if isStart(write) && write.Step == 1 && write.ID == StepID("LEGION-208", 1, record.StepPlace{Task: "a", Kind: record.PlaceStep, Place: 1}, 2) {
			fixRun = &write
		}
	}
	if fixRun == nil || fixRun.FixCheck == nil || *fixRun.FixCheck != "k-ci" {
		t.Fatalf("the start record of the fix's run = %+v, want one naming its check k-ci", fixRun)
	}
}

// The fix's start stands beside the start the issue owes its reviewer: it is written outside the
// line of starts that wait for a slot. The control: written as a start of that line (the seam
// fixStartInLine), the fix's start takes the reviewer's owed start out, which the subtest asserts.
func TestAFixStartAndTheReviewersStartBothStand(t *testing.T) {
	t.Run("both stand", func(t *testing.T) {
		r := reviewing(t)
		if reviewers := roleStartsOf(t, r.pool, "LEGION-208", claim.RoleReviewer); len(reviewers) != 1 {
			t.Fatalf("the reviewer's starts before the fix = %+v, want the one the move into reviewing queued", reviewers)
		}
		r.fails(fixHead, "k-ci")
		r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
		if reviewers := roleStartsOf(t, r.pool, "LEGION-208", claim.RoleReviewer); len(reviewers) != 1 {
			t.Fatalf("the reviewer's starts after the fix started = %+v, want the one still owed", reviewers)
		}
		if starts := r.fixStarts(); len(starts) != 1 {
			t.Fatalf("the starts of a fix = %+v, want one", starts)
		}
	})

	t.Run("the control: a fix's start in the line of starts takes the reviewer's out", func(t *testing.T) {
		r := reviewing(t)
		r.engine.fixStartInLine = true
		r.fails(fixHead, "k-ci")
		r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
		if reviewers := roleStartsOf(t, r.pool, "LEGION-208", claim.RoleReviewer); len(reviewers) != 0 {
			t.Fatalf("the reviewer's starts after a fix's start in the line = %+v, want none: the line keeps one owed start", reviewers)
		}
	})
}

// runs is each run of a step of LEGION-208 the store counts, as `<task>/<kind>/<place> <run>`.
func (r *fixRig) runs() []string {
	r.t.Helper()
	return outboxStrings(r.t, r.pool, `select task || '/' || kind || '/' || place || ' ' || run from step_starts
		where issue = $1 order by task, kind, place`, "LEGION-208")
}

// The fix's run of the implementer's step stays open beside the review's: a later fact of the
// issue settles its runs, and the issue stands at the review's place, yet the step the fix runs
// is not over while the fix runs. The control is the tester's run, which the move into reviewing
// ended.
func TestTheFixsRunStaysOpenBesideTheReview(t *testing.T) {
	r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})
	r.fails(fixHead, "k-ci2")
	assertStrings(t, "the runs", r.runs(), []string{"a/step/1 open", "a/step/2 ended", "a/step/3 open"})
}

// fixStartedRows is each fix-started row of LEGION-208, oldest first, as `<role> <reader> <branch>
// fix <n> <check>@<head>`, with `-` for no reader and no branch.
func (r *fixRig) fixStartedRows() []string {
	r.t.Helper()
	var lines []string
	for _, n := range r.newsOf() {
		if n.Purpose != record.CINewsFixStarted || n.Assigned == nil {
			continue
		}
		reader, branch := readerLine(n.Reader), n.Branch
		if reader == "" {
			reader = "-"
		}
		if branch == "" {
			branch = "-"
		}
		lines = append(lines, fmt.Sprintf("%s %s %s fix %d %s@%s", roleOf(r.t, n.Role), reader, branch, n.Assigned.Fix, n.Assigned.Check, n.Assigned.Head))
	}
	return lines
}

// toldOfFix1 is the fixStartedRows line of the row of fix 1 for k-ci at fixHead to role, for
// reader ("-" for none), naming role's branch beside fix 1 where branch is true.
func toldOfFix1(t *testing.T, role claim.Role, reader string, branch bool) string {
	t.Helper()
	named := "-"
	if branch {
		named = besideFix1(t, role)
	}
	return fmt.Sprintf("%s %s %s fix 1 k-ci@%s", role, reader, named, fixHead)
}

// besideMarks is the fix and the branch beside it that the latest start of step place of
// LEGION-208's task a names (record.StepStart's BesideFix and PushBranch), as `<fix> <branch>`.
func (r *fixRig) besideMarks(place int) string {
	r.t.Helper()
	var start record.StepStart
	applyInTx(r.t, r.pool, r.engine, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		start, err = r.engine.store.StepStarts(ctx, tx, "LEGION-208", record.StepPlace{Task: "a", Kind: record.PlaceStep, Place: place})
		return err
	})
	return fmt.Sprintf("%d %s", start.BesideFix, start.PushBranch)
}

// testerCompletesBeforeItsStates is the tester's completion at fixHead whose test record names the
// state event after every one applied so far, so the testing move waits for that event.
func (r *fixRig) testerCompletesBeforeItsStates() {
	r.t.Helper()
	if result := r.apply("test-done", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleTester, Claim: "tester-claim",
		Summary: "tested", Commit: fixHead, HandoffRecord: "test-1", HandoffVersion: 1, StatesSerial: r.serial + 1}); result.Refusal != nil {
		r.t.Fatalf("the tester's completion was refused: %+v", result.Refusal)
	}
}

// At the fix's start each other step of the task that runs or was handed work in this run is told
// of the fix, by one fix-started row naming the fix's check, head and number: the reviewer, which
// creates no commit, with no branch, and the tester, which passed, with its own branch beside the
// fix. A tester that runs gets the row for its current work, and its open start is bound to that
// branch. The owner in accepting gets the row with its own branch, for its acceptance, both at the
// fix's start and at a move into accepting while the fix runs. The implementer's own fix is its
// task, and no step of the task's line that has not started is told.
//
// A step whose start named the fix is told by that start alone: a reviewer that starts while fix 1
// reads its job, and an owner that moves into accepting then, get no row at the fix's start, and
// their start records keep BesideFix 1. A step that started before the fix opened, in the same
// fact that opened it, keeps zero and gets its one row at the fix's start, under the engine's fixed
// clock, where the start and the fix's opening share one microsecond.
//
// The controls, kept: with the seam toldByClock, which takes a start no earlier than the fix's
// opening as told, the step started in the fact that opened the fix gets no row; and with the seam
// tellEveryStepOfFix, which reads no start, the reviewer and the owner told by their starts get a
// second notice, a row at the fix's start.
func TestTheReviewerIsToldOfTheFix(t *testing.T) {
	t.Run("the steps that ran, at the fix's start", func(t *testing.T) {
		r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})
		assertStrings(t, "the fix-started rows", r.fixStartedRows(), []string{
			toldOfFix1(t, claim.RoleTester, "-", true),
			toldOfFix1(t, claim.RoleReviewer, r.stepReader(claim.RoleReviewer, phase.Reviewing), false),
		})
		assertStrings(t, "the implementer's messages", r.told(claim.RoleImplementer), nil)
		assertStrings(t, "the reviewer's messages", r.told(claim.RoleReviewer), nil)
	})

	t.Run("a tester that runs", func(t *testing.T) {
		r := newFixRig(t, returnEngine(), []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady)
		r.fails(fixHead, "k-ci")
		r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
		assertStrings(t, "the fix-started rows", r.fixStartedRows(), []string{toldOfFix1(t, claim.RoleTester, r.stepReader(claim.RoleTester, phase.Testing), true)})
		if got, want := r.besideMarks(2), "0 "+besideFix1(t, claim.RoleTester); got != want {
			t.Fatalf("the tester's start names %q, want %q: started before the fix, bound at the fix's start", got, want)
		}
	})

	t.Run("the owner in accepting, at the fix's start", func(t *testing.T) {
		r := newFixRig(t, returnEngine(), []string{"implementer", "reviewer", "acceptance:owner"}, TriggerImplementationReady)
		r.approves()
		r.fails(fixHead, "k-ci")
		r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
		assertStrings(t, "the fix-started rows", r.fixStartedRows(), []string{
			toldOfFix1(t, claim.RoleReviewer, "-", false),
			toldOfFix1(t, claim.RoleArchitect, "accepting@0", true),
		})
		if got, want := r.besideMarks(3), "0 "+besideFix1(t, claim.RoleArchitect); got != want {
			t.Fatalf("the owner's acceptance start names %q, want %q", got, want)
		}
	})

	t.Run("the owner at a move into accepting while the fix runs", func(t *testing.T) {
		r := newFixRig(t, returnEngine(), []string{"implementer", "reviewer", "acceptance:owner"}, TriggerImplementationReady)
		r.fails(fixHead, "k-ci")
		r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
		r.delivered()
		r.approves()
		assertStrings(t, "the fix-started rows of the move", r.fixStartedRows(), []string{toldOfFix1(t, claim.RoleArchitect, "accepting@0", true)})
		if got, want := r.besideMarks(3), "1 "+besideFix1(t, claim.RoleArchitect); got != want {
			t.Fatalf("the owner's acceptance start names %q, want %q", got, want)
		}
	})

	// readingStarts is LEGION-208 whose reviewer starts while fix 1 reads its job, on
	// `implementer tester reviewer`, and whose owner moves into accepting then, on `implementer
	// reviewer acceptance:owner`; the fix then starts. It answers the two rigs.
	readingStarts := func(t *testing.T, seam bool) (reviewer, owner *fixRig) {
		reviewer = newFixRig(t, returnEngine(), []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady)
		reviewer.engine.tellEveryStepOfFix = seam
		reviewer.fails(fixHead, "k-ci")
		reviewer.move(TriggerTesterPassed, "", record.PhaseRow{})
		if facts := reviewer.startFactsIn(phase.Reviewing); facts.Fix != 1 || facts.Branch != "" {
			t.Fatalf("the reviewer's start facts = %+v, want fix 1 and no branch", facts)
		}
		reviewer.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
		owner = newFixRig(t, returnEngine(), []string{"implementer", "reviewer", "acceptance:owner"}, TriggerImplementationReady)
		owner.engine.tellEveryStepOfFix = seam
		owner.fails(fixHead, "k-ci")
		owner.approves()
		owner.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
		return reviewer, owner
	}

	t.Run("a reviewer and an owner that start while the fix reads its job", func(t *testing.T) {
		reviewer, owner := readingStarts(t, false)
		assertStrings(t, "the fix-started rows beside the reviewer", reviewer.fixStartedRows(), []string{toldOfFix1(t, claim.RoleTester, "-", true)})
		if got := reviewer.besideMarks(3); got != "1 " {
			t.Fatalf("the reviewer's start names %q, want fix 1 and no branch", got)
		}
		assertStrings(t, "the fix-started rows beside the owner", owner.fixStartedRows(), []string{
			toldOfFix1(t, claim.RoleArchitect, "accepting@0", true),
			toldOfFix1(t, claim.RoleReviewer, "-", false),
		})
		if got, want := owner.besideMarks(3), "1 "+besideFix1(t, claim.RoleArchitect); got != want {
			t.Fatalf("the owner's acceptance start names %q, want %q", got, want)
		}
	})

	t.Run("the control: tellEveryStepOfFix tells them again at the fix's start", func(t *testing.T) {
		reviewer, owner := readingStarts(t, true)
		if rows := reviewer.fixStartedRows(); !slices.Contains(rows, toldOfFix1(t, claim.RoleReviewer, reviewer.stepReader(claim.RoleReviewer, phase.Reviewing), false)) {
			t.Fatalf("the fix-started rows beside the reviewer with every step told = %q, want a second notice to the reviewer", rows)
		}
		if rows := owner.fixStartedRows(); len(rows) != 3 || rows[2] != toldOfFix1(t, claim.RoleArchitect, "accepting@0", true) {
			t.Fatalf("the fix-started rows beside the owner with every step told = %q, want a second row to the owner at the fix's start", rows)
		}
	})

	// sameFact is LEGION-208 on `implementer tester reviewer` whose one state event both moves
	// its testing step on, starting its reviewer, and opens fix 1 at its tail; the fix then starts.
	sameFact := func(t *testing.T, seam bool) *fixRig {
		r := newFixRig(t, returnEngine(), []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady)
		r.engine.toldByClock = seam
		r.at(fixHead, stateOf("k-unit", stateYes, "", "tester"), ciPassing("k-ci2"))
		r.testerCompletesBeforeItsStates()
		assertPhase(t, r.pool, phase.Testing)
		r.fails(fixHead, "k-ci")
		assertPhase(t, r.pool, phase.Reviewing)
		assertStrings(t, "the fixes", r.fixes(), []string{fixOf(1, "reading", "k-ci")})
		if got := r.besideMarks(3); got != "0 " {
			t.Fatalf("the reviewer's start names %q, want no fix: it started before the fix opened", got)
		}
		r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
		return r
	}

	t.Run("a step started in the fact that opened the fix", func(t *testing.T) {
		r := sameFact(t, false)
		if rows := r.fixStartedRows(); !slices.Contains(rows, toldOfFix1(t, claim.RoleReviewer, r.stepReader(claim.RoleReviewer, phase.Reviewing), false)) {
			t.Fatalf("the fix-started rows = %q, want the reviewer's one", rows)
		}
	})

	t.Run("the control: toldByClock takes that step as told", func(t *testing.T) {
		r := sameFact(t, true)
		if rows := r.fixStartedRows(); slices.ContainsFunc(rows, func(row string) bool { return strings.HasPrefix(row, string(claim.RoleReviewer)+" ") }) {
			t.Fatalf("the fix-started rows with the clock read = %q, want none to the reviewer: one shared microsecond confuses its start with the fix's", rows)
		}
	})
}

// A fix whose read found no page of its job, or found no job, names its head and how to find the
// run: a job with no address of its page, and a read that failed, with the read's error. Its
// row keeps what the read answered.
func TestAFixWhoseJobReadFailedNamesTheHeadAndTheRead(t *testing.T) {
	t.Run("a read that failed", func(t *testing.T) {
		r := startedFix(t, intake.FixJobRead{ReadFailed: "Dispatch answered 503"})
		if fix := r.latestFix(); fix.State != record.FixRunning || fix.ReadFailed != "Dispatch answered 503" || fix.JobName != "" {
			t.Fatalf("the fix = %+v, want it running with the read's error kept", fix)
		}
		starts := r.fixStarts()
		if len(starts) != 1 {
			t.Fatalf("the fix's starts = %+v, want one", starts)
		}
		assertHolds(t, "the fix's task", starts[0].Task, "k-ci", fixHead, "Dispatch answered 503", "LEGION-208")
		if rows := r.fixStartedRows(); len(rows) != 2 {
			t.Fatalf("the fix-started rows = %q, want the tester's and the reviewer's", rows)
		}
	})

	t.Run("a job with no page", func(t *testing.T) {
		r := startedFix(t, intake.FixJobRead{Name: "unit"})
		starts := r.fixStarts()
		if len(starts) != 1 {
			t.Fatalf("the fix's starts = %+v, want one", starts)
		}
		assertHolds(t, "the fix's task", starts[0].Task, "k-ci", fixHead, "unit")
	})
}

// A read of the job of a fix that no longer reads it changes nothing: the fix ended before the
// answer came, so no start is queued, the fix stays as it ended, and nobody is told.
func TestALateJobReadOfAnEndedFixChangesNothing(t *testing.T) {
	r := reviewing(t)
	r.fails(fixHead, "k-ci")
	onStored(t, r.pool, r.engine, "LEGION-208", func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
		fix, err := r.engine.store.LatestFix(ctx, tx, issue.Key, issue.Generation, "a")
		if err != nil {
			return err
		}
		at := stepClock
		fix.State, fix.Result, fix.Way, fix.EndedAt = record.FixEnded, "stopped", 4, &at
		return r.engine.store.UpdateFix(ctx, tx, *fix)
	})

	r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})

	assertStrings(t, "the fixes", r.fixes(), []string{fixOf(1, "ended", "k-ci")})
	if starts := r.fixStarts(); len(starts) != 0 {
		t.Fatalf("the starts of a fix after a late read = %+v, want none", starts)
	}
	if fix := r.latestFix(); fix.JobName != "" || fix.Entry != 0 {
		t.Fatalf("the ended fix after a late read = %+v, want it as it ended", fix)
	}
	assertStrings(t, "the fix-started rows", r.fixStartedRows(), nil)
}

// The line of starts that wait for a slot takes no fix's start: a start of a fix is written
// beside the phase's own start, never in its place.
func TestTheStartLineRefusesAFixsStart(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Reviewing)
	err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		return StartOrOwe(t.Context(), tx, engine.store, root.Key, record.SuperviseRequest{Op: "start", Tree: root.Tree, Role: claim.RoleImplementer,
			Task: "Fix k-ci.", Generation: 1, Phase: phase.Implementing, Entry: 2, Fix: 1, FixFrom: fixHead}, stepClock)
	})
	if err == nil || !strings.Contains(err.Error(), "is written beside the line of starts that wait for a slot, never through it") {
		t.Fatalf("StartOrOwe of a fix's start = %v, want it refused as a start that is written beside the line", err)
	}
}
