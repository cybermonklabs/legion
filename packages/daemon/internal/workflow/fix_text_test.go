package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// fixWorkspace is the directory the runtime of these tests gives LEGION-208's working copy.
const fixWorkspace = "/work/sjawhar/legion/LEGION-208"

// withWorkspace is engine with the runtime's directory of every issue's working copy.
func withWorkspace(engine *Engine) *Engine {
	engine.cfg.WorkspaceDir = func(record.Issue) string { return fixWorkspace }
	return engine
}

// lastStart is the task of the latest start of role the outbox holds for LEGION-208, "" for none.
func (r *fixRig) lastStart(role claim.Role) string {
	r.t.Helper()
	starts := roleStartsOf(r.t, r.pool, "LEGION-208", role)
	if len(starts) == 0 {
		return ""
	}
	return starts[len(starts)-1].Task
}

// assertHolds fails unless task holds each of facts, what naming the task.
func assertHolds(t *testing.T, what, task string, facts ...string) {
	t.Helper()
	for _, fact := range facts {
		if !strings.Contains(task, fact) {
			t.Fatalf("%s = %q, want it to name %q", what, task, fact)
		}
	}
}

// assertLacks fails where task holds any of facts, what naming the task.
func assertLacks(t *testing.T, what, task string, facts ...string) {
	t.Helper()
	for _, fact := range facts {
		if strings.Contains(task, fact) {
			t.Fatalf("%s = %q, want it to name no %q", what, task, fact)
		}
	}
}

// A fix's task carries the facts the daemon fills in: the directory of the fix's own working copy,
// beside the task's, the check the fix fixes, the head it failed at, its CI job and the job's
// page, and each other check whose failure the fix holds. The control: the implementer's first
// task, of the same step with no fix, names neither the fix's directory nor a check.
func TestTheFixTextNamesItsCopyAndItsFailures(t *testing.T) {
	r := newFixRig(t, withWorkspace(returnEngine()), []string{"implementer", "tester", "reviewer"})
	assertLacks(t, "the implementer's first task", r.lastStart(claim.RoleImplementer), fixWorkspace+"-fix", "k-ci")
	move(t, r.pool, r.engine, "LEGION-208", TriggerImplementationReady, "", record.PhaseRow{})
	move(t, r.pool, r.engine, "LEGION-208", TriggerTesterPassed, "", record.PhaseRow{})
	r.fails(fixHead, "k-ci", "k-ci2")
	r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})

	starts := r.fixStarts()
	if len(starts) != 1 {
		t.Fatalf("the starts of a fix = %+v, want one", starts)
	}
	assertHolds(t, "the fix's task", starts[0].Task, fixWorkspace+"-fix", "k-ci", fixHead, "unit", fixPage, "k-ci2")
}

// An implementation step that starts while checks of a CI job fail at the head names them and the
// head, and its start gives each to the implementer (ci_failures). The control: the same start
// with no failure at the head names no check.
func TestAnImplementationStepsTaskTextNamesTheFailedChecks(t *testing.T) {
	t.Run("the failures held at its start", func(t *testing.T) {
		r := newFixRig(t, returnEngine(), []string{"planner", "implementer", "reviewer"})
		r.fails(fixHead, "k-ci", "k-ci2")
		assertStrings(t, "the failures while the planner runs", r.failures(), nil)
		move(t, r.pool, r.engine, "LEGION-208", TriggerPlannerCompleted, "", record.PhaseRow{})
		assertHolds(t, "the implementer's task", r.lastStart(claim.RoleImplementer), fixHead, "k-ci", "k-ci2")
		assertStrings(t, "the failures after its start", r.failures(), []string{failureOf("k-ci", "implementer", 0), failureOf("k-ci2", "implementer", 0)})
	})

	t.Run("the control: no failure at the head names none", func(t *testing.T) {
		r := newFixRig(t, returnEngine(), []string{"planner", "implementer", "reviewer"})
		move(t, r.pool, r.engine, "LEGION-208", TriggerPlannerCompleted, "", record.PhaseRow{})
		assertLacks(t, "the implementer's task", r.lastStart(claim.RoleImplementer), "k-ci", fixHead)
	})
}

// A step that starts while a fix of its task reads its job or runs names the fix's check and
// head. The control: the same step's start with no fix names neither.
func TestAStepThatStartsBesideAFixTaskTextNamesTheFix(t *testing.T) {
	t.Run("a fix reading its job", func(t *testing.T) {
		r := newFixRig(t, returnEngine(), []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady)
		r.fails(fixHead, "k-ci")
		assertStrings(t, "the fixes", r.fixes(), []string{fixOf(1, "reading", "k-ci")})
		move(t, r.pool, r.engine, "LEGION-208", TriggerTesterPassed, "", record.PhaseRow{})
		assertHolds(t, "the reviewer's task", r.lastStart(claim.RoleReviewer), "k-ci", fixHead)
	})

	t.Run("the control: no fix names none", func(t *testing.T) {
		r := newFixRig(t, returnEngine(), []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady, TriggerTesterPassed)
		assertLacks(t, "the reviewer's task", r.lastStart(claim.RoleReviewer), "k-ci", fixHead)
	})
}

// recordsPush records that role pushed its branch beside fix 1 of LEGION-208's task a to head, as a
// push of that branch does (record.Store.RecordBesidePush).
func (r *fixRig) recordsPush(role claim.Role, head string) {
	r.t.Helper()
	onStored(r.t, r.pool, r.engine, "LEGION-208", func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
		return r.engine.store.RecordBesidePush(ctx, tx, issue.Key, 1, "a", 1, string(role), head)
	})
}

// Each start of a step of a task that takes a fix is told what its task says of the fix by one
// typed value (startFacts), which this test reads, never a sentence. The fix's own start holds its
// number, its check, the head, its job, the job's page and its own branch F. A tester that starts
// while the fix runs holds fix 1, its check and head, and its own branch beside the fix, which is
// no branch of the issue (claim.IsBesideBranch), and no pushed head until the daemon holds a push of
// that branch, then that head. The reviewer, which creates no commit, holds the fix and no branch.
// The control: the same starts with no fix hold none of it.
func TestEachStartBesideAFixHoldsItsFixAndItsBranch(t *testing.T) {
	const pushed = "2222222222222222222222222222222222222222"
	t.Run("beside fix 1", func(t *testing.T) {
		r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})
		own := r.startFactsIn(phase.Implementing)
		if !own.Own || own.Fix != 1 || own.Check != "k-ci" || own.Head != fixHead || own.Job != "unit" || own.Page != fixPage ||
			own.Branch != besideFix1(t, claim.RoleImplementer) {
			t.Fatalf("the fix's own start facts = %+v, want fix 1 of k-ci at %s, job unit at %s, on its own branch", own, fixHead, fixPage)
		}
		tester := r.startFactsIn(phase.Testing)
		if tester.Own || tester.Fix != 1 || tester.Check != "k-ci" || tester.Head != fixHead || !claim.IsBesideBranch(tester.Branch) ||
			tester.Branch != besideFix1(t, claim.RoleTester) || tester.Pushed != "" {
			t.Fatalf("the tester's start facts = %+v, want fix 1 of k-ci at %s on the tester's own branch beside it, with no push held", tester, fixHead)
		}
		if reviewer := r.startFactsIn(phase.Reviewing); reviewer.Fix != 1 || reviewer.Check != "k-ci" || reviewer.Branch != "" || reviewer.Pushed != "" {
			t.Fatalf("the reviewer's start facts = %+v, want fix 1 of k-ci and no branch", reviewer)
		}
		r.recordsPush(claim.RoleTester, pushed)
		if tester := r.startFactsIn(phase.Testing); tester.Branch != besideFix1(t, claim.RoleTester) || tester.Pushed != pushed {
			t.Fatalf("the tester's start facts after its push = %+v, want its branch at %s", tester, pushed)
		}
	})

	t.Run("the control: no fix", func(t *testing.T) {
		r := reviewing(t)
		for _, at := range []phase.Phase{phase.Implementing, phase.Testing, phase.Reviewing} {
			if facts := r.startFactsIn(at); facts.Fix != 0 || facts.Own || facts.Branch != "" || facts.Pushed != "" {
				t.Fatalf("the start facts in %s with no fix = %+v, want none of a fix", at, facts)
			}
		}
	})
}
