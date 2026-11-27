package replay

import (
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// openChildSignOffWindow is a window from 10:00 to 12:00 recorded under legion-v19.57.3, its
// inputs reaching back to the daemon's boot at 08:00. ACME-1, a root promoted at 08:05, works to
// its review as rootThroughReview runs it and into accepting at 09:10, its slot kept, its
// architect in a turn from then. At 09:30 the architect files ACME-2 under it in triage, a
// Dispatch child Legion is not given, which stays open and out of the workflow to the window's
// end. At 10:30 the architect ends its turn and signs off, and the record moves ACME-1 to retro:
// the release at 9542c6fb held a sign-off for an issue beneath it in the workflow alone.
func openChildSignOffWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(10, 0), clock(12, 0))
	b.window.Parts[0].Rule = "legion-v19.57.3"
	b.reaches(2 * time.Hour)
	b.boot(clock(8, 0))
	b.file(clock(8, 5), "ACME-1", filing{Title: "Ship the ACME-1 change", Status: "todo", Rank: "a1", Labels: handedToLegion})
	b.rootThroughReview(clock(8, 5), "ACME-1")
	b.pastReviewKept(clock(9, 10), "ACME-1", phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved)
	b.filedBy(clock(9, 30), "ACME-2", filing{Title: "Name the ACME-1 change's follow-up", Parent: "ACME-1", Status: "triage", Rank: "a2"},
		b.session("ACME-1", claim.RoleArchitect))
	b.pastReviewKept(clock(10, 30), "ACME-1", phase.Accepting, phase.Retro, workflow.TriggerSignOff)
	return b.build()
}

// A sign-off takes effect where the release the rule names would have taken it: an open Dispatch
// child of the issue holds its acceptance since cybermonklabs/legion#4592
// (record.SlotRule.OpenChildHoldsAcceptance), and the rules of the recorded windows, whose
// releases came before it, hold a sign-off for an issue beneath it still in the workflow alone.
// Each sign-off the engine holds is a line of the run's log (signOffHeldLine), with the issue and
// the refusal's code. The test replays openChildSignOffWindow: under legion-v19.57.3 ACME-1's
// sign-off at 10:30 moves it to retro, as the record shows, with no sign-off held and no line of
// one; under main with no wait that stops the phase (mechanismNoChildrenWait), which holds it for
// ACME-2 while ACME-1 keeps accepting, the run counts the held sign-off and its log holds the
// line of it at 10:30.
func TestASignOffTakesEffectWhereTheRecordedReleaseTookIt(t *testing.T) {
	t.Parallel()
	window := openChildSignOffWindow(t)
	held := func(log []replayLine) []replayLine {
		return slices.DeleteFunc(slices.Clone(log), func(line replayLine) bool { return line.Msg != signOffHeldLine })
	}

	result, log := knobRun(t, window, replayedRule(t, "legion-v19.57.3"), "main")
	signedOff := linesAt(log, func(line replayLine) bool {
		return line.Msg == phaseChangedLine && line.Issue == "ACME-1" && line.Trigger == workflow.TriggerSignOff && line.To == string(phase.Retro)
	})
	if want := []time.Time{clock(10, 30)}; !slices.EqualFunc(signedOff, want, time.Time.Equal) {
		t.Errorf("under legion-v19.57.3 ACME-1 is signed off to retro at %s, want at %s", describeTimes(signedOff), describeTimes(want))
	}
	if lines := held(log); result.Divergence.HeldSignOffs != 0 || len(lines) != 0 || len(result.Divergence.Refused) != 0 {
		t.Errorf("under legion-v19.57.3 the run holds %d sign-offs, logs %+v and refuses %+v; want none of each",
			result.Divergence.HeldSignOffs, lines, result.Divergence.Refused)
	}

	result, log = knobRun(t, window, mechanismNoChildrenWait(t, "main"), "main")
	lines := held(log)
	if result.Divergence.HeldSignOffs != 1 || len(lines) != 1 || lines[0].Issue != "ACME-1" || lines[0].Code == "" ||
		!lines[0].Time.Equal(clock(10, 30)) {
		t.Errorf("under main with no wait that stops the phase the run holds %d sign-offs and logs %+v; want ACME-1's at 10:30, logged once with its code",
			result.Divergence.HeldSignOffs, lines)
	}
}
