package workflow

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// steppingBackClock makes engine's clock step back a second at each read, from an hour after
// stepClock, so each write the test makes through it is earlier than the one before, and later
// than every write made before it.
func steppingBackClock(engine *Engine) {
	at := stepClock.Add(time.Hour)
	engine.cfg.Clock = func() time.Time {
		at = at.Add(-time.Second)
		return at
	}
}

// engineClocks is each clock these tests run the engine on: one that does not advance (stepClock,
// the clock of every fix rig), one that steps back, and one that advances.
var engineClocks = []struct {
	name string
	set  func(*Engine)
}{
	{"a clock that does not advance", func(*Engine) {}},
	{"a clock that steps back at each read", steppingBackClock},
	{"a clock that advances", tickingClock},
}

// nextStart applies trigger and answers the start of role it makes: the latest start of role the
// outbox holds for LEGION-208, in at, of the entry after the one role's row held before. The test
// fails where there is none.
func (r *fixRig) nextStart(trigger TriggerKind, role claim.Role, at phase.Phase) record.SuperviseRequest {
	r.t.Helper()
	entry := roleRowOf(r.t, r.pool, r.engine, "LEGION-208", role).Entry
	r.move(trigger, "", record.PhaseRow{})
	starts := r.startsOf(role)
	if len(starts) == 0 || starts[len(starts)-1].Phase != at || starts[len(starts)-1].Entry != entry+1 {
		r.t.Fatalf("the %s's starts after %s = %+v, want the latest in %s of entry %d", role, trigger, starts, at, entry+1)
	}
	return starts[len(starts)-1]
}

// The start a way after a fix makes moves the task's working copy to the head the fix ended at, Y,
// and no later start moves it for that fix: the step the way started may commit over Y in that copy,
// and a move to Y again would take the copy off those commits. A fix ends at Y with commits of its
// own; way 2 starts the tester with its copy moved to Y, and way 1 the implementer; that step passes,
// and the next step's start moves nothing, whatever the engine's clock reads at the fix's end and
// at the way's start. Its red is the clock deciding it: a start after the way's start moved the copy
// to Y again wherever the way's start was not later than the fix's end by the clock, as with a clock
// that does not advance, or one that steps back.
func TestNoStartAfterTheWaysStartMovesTheCopyAgain(t *testing.T) {
	for _, clock := range engineClocks {
		t.Run(clock.name, func(t *testing.T) {
			t.Run("way 2", func(t *testing.T) {
				r := fixAtTheEnd(t, returnEngine())
				clock.set(r.engine)
				r.fixEnds(headTwo)
				r.assertWay(wayAfterFix, phase.Testing, claim.RoleTester, headTwo, 1, retroTesting)
				if got := r.nextStart(TriggerTesterPassed, claim.RoleReviewer, phase.Reviewing).MoveTo; got != "" {
					t.Fatalf("the reviewer's start after the tester the way started passed moves the task's copy to %q, want no move", got)
				}
			})
			t.Run("way 1", func(t *testing.T) {
				r := newFixRig(t, returnEngine(), []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady, TriggerTesterPassed)
				r.fails(fixHead, "k-ci")
				r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
				clock.set(r.engine)
				r.fixEnds(headTwo)
				r.reviewedX()
				r.move(TriggerReviewRejected, "", reviewReport("the review's report"))
				r.assertWay(wayReturn, phase.Implementing, claim.RoleImplementer, headTwo, 1, reviewingImplementing)
				if got := r.nextStart(TriggerImplementationReady, claim.RoleTester, phase.Testing).MoveTo; got != "" {
					t.Fatalf("the tester's start after the implementer the way started passed moves the task's copy to %q, want no move", got)
				}
			})
		})
	}
}

// The start a way 2 runs again of a step that pushed to its own branch beside the fix names that
// branch and the head the daemon holds of the push, so the step takes its commits from it
// (keptSentence), whatever the engine's clock reads. Fix 1 runs beside the running tester, which is
// bound to B1, and ends with commits of its own at Y while the tester runs, so its way waits; the
// tester pushes to B1, at P, and passes, which takes way 2. The way's start of the tester is written
// after its task is composed (start, stepStanding), so the task reads the tester's start beside the
// fix. Its red is the clock deciding it: a clock that advances puts that start before the fix's end,
// and the task then named neither B1 nor P.
func TestTheStartAWay2RunsAgainNamesItsKeptBranchWhateverTheClock(t *testing.T) {
	const pushed = "4444444444444444444444444444444444444444"
	for _, clock := range engineClocks {
		t.Run(clock.name, func(t *testing.T) {
			engine := returnEngine()
			clock.set(engine)
			r := newFixRig(t, engine, []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady)
			r.fails(fixHead, "k-ci")
			r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
			r.fixEnds(headTwo)
			if fix := r.latestFix(); fix.State != record.FixEnded || fix.Way != 0 {
				t.Fatalf("fix 1 after its end beside the running tester = %+v, want ended with its way pending", fix)
			}
			r.recordsPush(claim.RoleTester, pushed)
			start := r.nextStart(TriggerTesterPassed, claim.RoleTester, phase.Testing)
			r.assertWayTaken(wayAfterFix)
			branch := besideFix1(t, claim.RoleTester)
			if !strings.Contains(start.Task, branch) || !strings.Contains(start.Task, pushed) {
				t.Fatalf("the start of the tester the way runs again names its kept branch %s: %t, the pushed head %s: %t; want both",
					branch, strings.Contains(start.Task, branch), pushed, strings.Contains(start.Task, pushed))
			}
		})
	}
}
