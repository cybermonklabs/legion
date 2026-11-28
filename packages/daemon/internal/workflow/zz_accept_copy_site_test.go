package workflow

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The tester's probe of the first start after a fix (copyMoveAfterFix) in the direction the three
// tests of the Medium leave unpinned there: a person pushes P while the fix runs, the fix's end E
// (a child of P) reaches the engine before E's synchronize, so the record's head is P, which lacks
// the fix's commits. On a task whose one step is the implementation, CI then passes at P and the
// retro starts: its copy must move to E, not to P. Its red is copyMoveAfterFix taking the record's
// head whatever its moves say.
func TestAcceptProbeTheRetroAfterAPushWhileTheFixRunsStartsAtTheFixsEnd(t *testing.T) {
	r := newFixRig(t, returnEngine(), []string{"implementer"})
	tickingClock(r.engine)
	r.at(fixHead, jobRunning("k-ci"))
	r.move(TriggerImplementationReady, "", record.PhaseRow{})
	r.assertEndHold(record.HoldForCI)
	r.fails(fixHead, "k-ci")
	r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
	r.assertEndHold(record.HoldForFix)
	r.personPushes(fixHead, headTwo)
	if result := r.apply("fix-done", fixCompletion(headThree)); result.Refusal != nil {
		t.Fatalf("the fix's completion was refused: %+v", result.Refusal)
	}
	if pr := recordedPR(t, r.pool, "LEGION-208"); pr.HeadSHA != headTwo {
		t.Fatalf("UNKNOWN: the record's head after the fix's completion is %s, want P %s: the probe needs E's synchronize not yet arrived", pr.HeadSHA, headTwo)
	}
	r.at(headTwo, noCIJob("k-unit"), ciPassing("k-ci2"), ciPassing("k-ci"))
	if got := r.assertRetroStarted().MoveTo; got != headThree {
		t.Fatalf("the retro's start moves the task's copy to %q, want %s, the fix's end, which holds P %s and the fix's commits", got, headThree, headTwo)
	}
}
