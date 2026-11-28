package supervise

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/wakepace"
)

// A relaunch the pause's gate held joins, at the pause's release, the queue its claim's class
// names (letOutHeldLaunch, WakeQueue): a charged death's the pacing's first queue, and that of an
// end from outside the pod, which charges no death though its decision writes Claim.DeathCharged,
// the second, as a displaced relaunch does (dispatch://INFRA-5682 R2). Each row stands the claim
// in a turn of its task, pauses the project and reports the claim's process gone, so the gate
// holds the relaunch that report raises; then the resume's event lets it out. A place another
// claim joined in the first queue just before takes the turn ahead of it, so the relaunch's own
// place waits, and its queue is read off that place and, at its turn, off the line the wake
// writes.
//
// The negative control is WakeQueue reading Claim.DeathCharged in place of Claim.chargedDeath:
// the relaunch of the end from outside the pod then joins the first queue.
func TestAPausesReleaseJoinsAnOutsideEndsRelaunchToTheSecondQueueAndADeathsToTheFirst(t *testing.T) {
	for _, row := range []struct {
		name    string
		reading runtime.PodEndReading
		queue   wakepace.Queue
		line    string
	}{
		{name: "a charged death", queue: wakepace.First, line: "queue=message"},
		{name: "an end from outside the pod", reading: runtime.PodEndNotOurs, queue: wakepace.Second, line: "queue=other"},
	} {
		t.Run(row.name, func(t *testing.T) {
			h, flag, stops := pauseHarness(t, queuedClaim(), pacedDeps(wakeRate))
			h.reach(StateWorking)
			flag.set(7)

			h.must(RuntimeObservation{Observation: runtime.Observation{
				Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now(), EndReading: row.reading,
			}})

			c := h.claim()
			if outside := row.reading == runtime.PodEndNotOurs; c.decidedFromOutside() != outside || c.chargedDeath() == outside {
				t.Fatalf("the claim reads decided from outside the pod %v and a charged death %v, want %v and %v",
					c.decidedFromOutside(), c.chargedDeath(), outside, !outside)
			}
			if waiting := h.m.View().WakeWaiting; waiting != nil || !h.armed(TimerPause) {
				t.Fatalf("the claim publishes wake %+v with the gate's stop armed %v, want the relaunch held by the pause's gate, in no pacing",
					waiting, h.armed(TimerPause))
			}
			firstAhead(h.deps.WakePace.Pacer, 1)
			flag.set(0)
			ran := len(h.logs.lines(WakeRanMessage, "claim="+string(h.token)))

			h.must(ResumePause{Claim: h.token, Pause: 7})

			place := h.m.view.Load().place
			if place == nil {
				t.Fatalf("the claim holds no place in the pacing after the release, want its relaunch waiting at one (wake %+v)", h.m.View().WakeWaiting)
			}
			if got := place.Queue(); got != row.queue {
				t.Fatalf("the released relaunch waits in the %s queue, want the %s queue", got, row.queue)
			}
			h.advanceToTurn(h.m)
			h.wantStarts(h.token, 2)
			if lines := h.logs.lines(WakeRanMessage, "claim="+string(h.token))[ran:]; len(lines) != 1 || !strings.Contains(lines[0], row.line) {
				t.Fatalf("the released relaunch wrote the wake lines %q, want one saying %s", lines, row.line)
			}
			if row.reading == runtime.PodEndNotOurs {
				h.wantNothingCharged("the released relaunch")
			}
			stops.wantStops(t)
		})
	}
}
