package workflow

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// tickingClock makes engine's clock move on a second at each read, from an hour after stepClock,
// so each write the test makes through it is later than the one before.
func tickingClock(engine *Engine) {
	at := stepClock.Add(time.Hour)
	engine.cfg.Clock = func() time.Time {
		at = at.Add(time.Second)
		return at
	}
}

// moveToOf is the head the newest start of role the outbox holds for LEGION-208 moves the task's
// working copy to, a fix's starts left out; the test fails where the outbox holds none.
func (r *fixRig) moveToOf(role claim.Role) string {
	r.t.Helper()
	starts := r.startsOf(role)
	if len(starts) == 0 {
		r.t.Fatalf("the outbox holds no start of the %s", role)
	}
	return starts[len(starts)-1].MoveTo
}

// A fix that made commits leaves the task's working copy where the fix started, since the fix
// pushes from a copy of its own. So the first start of a step after it ended moves that copy to the
// head the fix ended at: here a stop ended the fix after it pushed headTwo, and the restart of the
// task starts the implementer moving its copy there. A later start finds the copy moved, and moves
// it no more: a move then could take it back from a commit pushed since. The controls, kept: a fix
// that made no commits, and a task that had no fix, move nothing at the restart.
func TestTheFirstStartAfterAFixWithCommitsMovesTheCopy(t *testing.T) {
	r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})
	tickingClock(r.engine)
	r.pushed(headTwo)
	r.stop()
	r.todo()

	if got := r.moveToOf(claim.RoleImplementer); got != headTwo {
		t.Fatalf("the restarted implementer moves the task's copy to %q, want %s, the head the fix ended at", got, headTwo)
	}
	r.move(TriggerImplementationReady, "", record.PhaseRow{})
	if got := r.moveToOf(claim.RoleTester); got != "" {
		t.Fatalf("the tester after the restart moves the task's copy to %q, want no move: the restart moved it", got)
	}

	t.Run("a fix that made no commits moves nothing", func(t *testing.T) {
		r := startedFix(t, intake.FixJobRead{Name: "unit", URL: fixPage})
		tickingClock(r.engine)
		r.stop()
		r.todo()
		if got := r.moveToOf(claim.RoleImplementer); got != "" {
			t.Fatalf("the restarted implementer moves the task's copy to %q, want no move", got)
		}
	})

	t.Run("a task that had no fix moves nothing", func(t *testing.T) {
		r := reviewing(t)
		tickingClock(r.engine)
		r.pushed(headTwo)
		r.stop()
		r.todo()
		if got := r.moveToOf(claim.RoleImplementer); got != "" {
			t.Fatalf("the restarted implementer moves the task's copy to %q, want no move", got)
		}
	})
}

// The retro after a fix at the task's end moves the task's copy to the head the fix ended at: on a
// task whose one step is the implementation, a check that fails at its end starts a fix there; the
// fix pushes headTwo and ends, no step comes after its own, and once CI passes at headTwo the
// retro starts with its copy moved there. The control, kept: the same fix ending at the head it
// started at moves nothing.
func TestTheRetroAfterAFixAtTheEndMovesTheCopy(t *testing.T) {
	retroAfterFix := func(t *testing.T, end string) record.SuperviseRequest {
		t.Helper()
		r := newFixRig(t, returnEngine(), []string{"implementer"})
		tickingClock(r.engine)
		r.at(fixHead, jobRunning("k-ci"))
		r.move(TriggerImplementationReady, "", record.PhaseRow{})
		r.assertEndHold(record.HoldForCI)
		r.fails(fixHead, "k-ci")
		r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
		r.assertEndHold(record.HoldForFix)
		r.pushed(end)
		if result := r.apply("fix-done", fixCompletion(end)); result.Refusal != nil {
			t.Fatalf("the fix's completion was refused: %+v", result.Refusal)
		}
		r.at(end, noCIJob("k-unit"), ciPassing("k-ci2"), ciPassing("k-ci"))
		return r.assertRetroStarted()
	}

	if got := retroAfterFix(t, headTwo).MoveTo; got != headTwo {
		t.Fatalf("the retro's start moves the task's copy to %q, want %s, the head the fix ended at", got, headTwo)
	}

	t.Run("a fix that made no commits moves nothing", func(t *testing.T) {
		if got := retroAfterFix(t, fixHead).MoveTo; got != "" {
			t.Fatalf("the retro's start moves the task's copy to %q, want no move", got)
		}
	})
}

// A head pushed over the fix's end holds the fix's commits, so the first start after the fix moves
// the task's copy to that head, not back to the fix's end. On a task whose one step is the
// implementation, a check fails at its end and a fix starts there; the fix's push of Y and its
// synchronize arrive and the fix ends at Y; a person then pushes P over Y, and once CI passes at P
// the retro starts with its copy moved to P. Its red is the fix's end taken whatever the record's
// moves say (copyMoveAfterFix): the retro then started at Y, without P.
func TestTheRetroAfterAPushOverTheFixsEndStartsAtThatPush(t *testing.T) {
	r := newFixRig(t, returnEngine(), []string{"implementer"})
	tickingClock(r.engine)
	r.at(fixHead, jobRunning("k-ci"))
	r.move(TriggerImplementationReady, "", record.PhaseRow{})
	r.assertEndHold(record.HoldForCI)
	r.fails(fixHead, "k-ci")
	r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
	r.assertEndHold(record.HoldForFix)
	r.fixPushes(headTwo)
	if result := r.apply("fix-done", fixCompletion(headTwo)); result.Refusal != nil {
		t.Fatalf("the fix's completion was refused: %+v", result.Refusal)
	}
	r.personPushes(headTwo, headThree)
	r.at(headThree, noCIJob("k-unit"), ciPassing("k-ci2"), ciPassing("k-ci"))
	if got := r.assertRetroStarted().MoveTo; got != headThree {
		t.Fatalf("the retro's start moves the task's copy to %q, want %s, the head pushed over the fix's end %s", got, headThree, headTwo)
	}
}
