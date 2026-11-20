package daemon

// The acts of a pod update round that a project's pause changes, each made over the pause's holds
// of the pod as they stand when it is made (roundClockNow): a replacement, before the pod's launch
// turn and under it (replacePodForRound, replacementDue), an opening (reconcileIssuePodUpdate),
// and a late round's notice (tellLateRound). The sweep reads the holds once at its top
// (podPauseHolds), and then its walk can wait before it reaches an issue, on a write it makes for
// an issue before it (sweepWaitingInAWrite), and each replacement after the walk waits for its
// pod's launch turn (the fake's HoldReplacePod). Meanwhile a pause can take the pod, a hold can
// end, and the wake pacing can move the let-out of a launch the pause held later
// (store.SetResumeClock), which moves the round's bound later (heldUntil). The limit rig is
// podupdate_limits_test.go's, with no limit set.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// heldPod is one pause's hold of an issue's pod that a role's ready ended, with the open resume
// of the pod's implementer that the pause held (pod_resumes), whose stuck clock is the let-out of
// that launch: at the hold's end until the wake pacing moves it (store.SetResumeClock). The round's
// bound counts the hold on to the later of its end and that let-out (heldUntil).
type heldPod struct {
	issue   string
	pause   int64
	heldAt  time.Time
	endedAt time.Time
}

// holdAndEnd holds issue's pod under a pause of the rig's own project at the clock as it stands,
// moves the clock by held, and ends the hold there as a role's ready ends it, with the held
// launch of the pod's implementer let out at that end.
func (r *fleetRig) holdAndEnd(issue string, held time.Duration) heldPod {
	r.t.Helper()
	project := r.s.cfg.Project
	h := heldPod{issue: issue, heldAt: r.clock.Now()}
	h.pause = openProjectPause(r.t, r.pool, project, h.heldAt)
	holdProjectPod(r.t, r.sup.store, h.pause, project, issue, h.heldAt)
	r.clock.advance(held)
	h.endedAt = r.clock.Now()
	if done, err := r.sup.store.EndPausePodHold(r.ctx, project, issue, store.PausePodHoldReady, h.endedAt); err != nil || !done {
		r.t.Fatalf("end the hold of %s's pod at %s = %t, %v; want it ended", issue, h.endedAt.Format(time.RFC3339), done, err)
	}
	if _, err := r.pool.Exec(r.ctx, `insert into pod_resumes (release_id, project, issue, role, kind, at, seen_release, clock_at, pause_id)
		values (0, $1, $2, 'implementer', 'role-message', $3, 0, $3, $4)`, strings.ToLower(project), issue, h.endedAt, h.pause); err != nil {
		r.t.Fatalf("write the held wake of %s's implementer: %v", issue, err)
	}
	return h
}

// letOutLater moves the let-out of the held launch to letOut, as the wake pacing's reservation of
// that launch does (store.SetResumeClock).
func (r *fleetRig) letOutLater(h heldPod, letOut time.Time) {
	r.t.Helper()
	if moved, err := r.sup.store.SetResumeClock(r.ctx, strings.ToLower(r.s.cfg.Project), h.issue, claim.RoleImplementer, letOut); err != nil || !moved {
		r.t.Errorf("let the held launch of %s's implementer out at %s = %t, %v; want it moved", h.issue, letOut.Format(time.RFC3339), moved, err)
	}
}

// openedRound is the round issue's architect records and when it opened.
func (r *fleetRig) openedRound(issue string) (string, time.Time) {
	r.t.Helper()
	c := r.claimOf(issue, claim.RoleArchitect)
	if c.UpdateOpenedAt == nil {
		r.t.Fatalf("%s's claim records round %q with no opening", issue, c.UpdateRound)
	}
	return c.UpdateRound, *c.UpdateOpenedAt
}

// pastAnEndedHold is one issue's round, opened at a sweep of its own, whose pod a pause held from a
// minute after the opening for a minute, the hold ended as a role's ready ends it, and the clock
// 30 seconds past the bound that hold makes. Nobody has answered the round, so the sweep at this
// clock replaces the pod at the bound. It answers the rig, the pod, the hold and the bound.
func pastAnEndedHold(t *testing.T) (*fleetRig, string, heldPod, time.Time) {
	t.Helper()
	issue := "ACME-1"
	rig := newLimitRig(t, issue)
	rig.tick()
	rig.awaitRounds("the sweep that opened the round", issue)
	round, opened := rig.openedRound(issue)
	rig.clock.advance(time.Minute)
	h := rig.holdAndEnd(issue, time.Minute)
	bound := opened.Add(podUpdateGrace + h.endedAt.Sub(h.heldAt))
	rig.clock.advance(bound.Add(30 * time.Second).Sub(rig.clock.Now()))
	return rig, strings.TrimPrefix(round, supervise.PodRoundPrefix), h, bound
}

// A hold that ended before the replacement reads the holds, with its held launch let out at that
// end, keeps the pod to the bound that end makes and no longer: the replacement reads the ended
// hold and replaces the pod at the first sweep past that bound.
func TestPodUpdateAReplacementGoesOnceItsPodsHoldEndedAndTheBoundItMakesHasCome(t *testing.T) {
	rig, pod, _, bound := pastAnEndedHold(t)
	rig.tick()
	if calls := rig.replacementsOf(pod); len(calls) != 1 {
		t.Errorf("the sweep at %s replaced the pod %d times, want once: the pause's hold of it ended and its bound, %s, has come",
			rig.clock.Now().Format(time.RFC3339), len(calls), bound.Format(time.RFC3339))
	}
}

// The wake pacing let the held launch out 10 minutes later before the sweep's top, so the sweep
// reads that let-out, the bound is 40 minutes on, and nothing is replaced.
func TestPodUpdateAReplacementWaitsForALetOutTheSweepRead(t *testing.T) {
	rig, pod, h, _ := pastAnEndedHold(t)
	rig.letOutLater(h, rig.clock.Now().Add(10*time.Minute))
	rig.tick()
	if calls := rig.replacementsOf(pod); len(calls) != 0 {
		t.Errorf("the sweep at %s replaced the pod %d times, with the held implementer's launch let out 10 minutes later", rig.clock.Now().Format(time.RFC3339), len(calls))
	}
}

// The same let-out, set while the replacement waits for the pod's launch turn, after the sweep's
// top and the replacement's own read, and before the daemon's last word (replacementDue): the
// last word reads the holds again and refuses the replacement, since the bound is 40 minutes on.
func TestPodUpdateAReplacementReadsALetOutThePacingSetsWhileItWaitsItsTurn(t *testing.T) {
	rig, pod, h, bound := pastAnEndedHold(t)
	reserved := false
	rig.rt.HoldReplacePod(func() {
		if reserved {
			return
		}
		reserved = true
		rig.letOutLater(h, rig.clock.Now().Add(10*time.Minute))
	})
	rig.tick()
	if !reserved {
		t.Fatalf("the sweep at %s made no replacement call, so no launch turn was waited for", rig.clock.Now().Format(time.RFC3339))
	}
	if calls := rig.replacementsOf(pod); len(calls) != 0 {
		t.Errorf("the sweep at %s replaced the pod, with its disk, %d times: the pacing let the held implementer's launch out 10 minutes later while the replacement waited its turn, so the round's bound is 40 minutes on; the replacement read the bound the sweep's top read, %s",
			rig.clock.Now().Format(time.RFC3339), len(calls), bound.Format(time.RFC3339))
	}
}

// The same let-out, set while the walk waits in a write for an issue before it, after the sweep's
// top: the walk finds the round past the bound the sweep read, and the replacement it leaves for
// after the walk reads the holds again before it takes the pod's launch turn, so it neither
// replaces the pod nor asks the runtime for the turn.
func TestPodUpdateAReplacementReadsALetOutThePacingSetsWhileTheWalkWaits(t *testing.T) {
	slow, late := "ACME-1", "ACME-2"
	rig := newLimitRig(t, slow, late)
	rig.drifts(slow)
	rig.tick()
	rig.awaitRounds("the sweep that opened the later issue's round", late)
	round, opened := rig.openedRound(late)
	pod := strings.TrimPrefix(round, supervise.PodRoundPrefix)
	rig.clock.advance(time.Minute)
	h := rig.holdAndEnd(late, time.Minute)
	bound := opened.Add(podUpdateGrace + h.endedAt.Sub(h.heldAt))

	// Before that bound the earlier issue's round opens; the walk of the sweep after it writes
	// that round's ask.
	rig.drifts(slow, limitDrift)
	rig.tick()
	rig.awaitRounds("the sweep that opened the earlier issue's round", slow, late)

	rig.clock.advance(bound.Add(30 * time.Second).Sub(rig.clock.Now()))
	rig.sent(late, claim.RoleArchitect)
	if !rig.sweepWaitingInAWrite(slow, 20*time.Second, func() {
		rig.letOutLater(h, rig.clock.Now().Add(10*time.Minute))
		rig.clock.advance(20 * time.Second)
	}) {
		t.Fatalf("the sweep made no write for %s", slow)
	}
	if calls := rig.replacementsOf(pod); len(calls) != 0 {
		t.Errorf("the sweep at %s replaced %s's pod, with its disk, %d times: the pacing let its held implementer's launch out 10 minutes later while the walk waited, so the round's bound is 40 minutes on; the sweep's top read %s",
			rig.clock.Now().Format(time.RFC3339), late, len(calls), bound.Format(time.RFC3339))
	}
	if calls := rig.rt.CallsOf("ReplacePodNotDue"); len(calls) != 0 {
		t.Errorf("the replacement of %s's pod took the pod's launch turn %d times and was refused there: it read the bound the sweep's top read, not the let-out the pacing set during the walk",
			late, len(calls))
	}
}

// The pause whose hold of the pod ended takes the pod again while the replacement waits for the
// pod's launch turn, after the sweep's top and the replacement's own read, and before the daemon's
// last word (replacementDue). The bound has come, and a hold that has just begun moves it by
// nothing yet, so only the hold itself can keep the pod: the last word reads the holds again,
// finds the hold standing, and refuses the replacement. A hold that ends in the same wait, 10
// seconds after it began and before the last word, keeps the pod no longer: it moves the bound by
// its 10 seconds, the clock has moved with it, and the pod is replaced once.
func TestPodUpdateAPauseThatTakesThePodInItsLaunchTurnHoldsTheReplacement(t *testing.T) {
	for _, row := range []struct {
		name  string
		ended bool
	}{
		{name: "the hold stands at the last word"},
		{name: "the hold ended before the last word", ended: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig, pod, h, bound := pastAnEndedHold(t)
			project := rig.s.cfg.Project
			var takenAt time.Time
			rig.rt.HoldReplacePod(func() {
				if !takenAt.IsZero() {
					return
				}
				takenAt = rig.clock.Now()
				holdProjectPod(t, rig.sup.store, h.pause, project, h.issue, takenAt)
				if !row.ended {
					return
				}
				rig.clock.advance(10 * time.Second)
				if done, err := rig.sup.store.EndPausePodHold(rig.ctx, project, h.issue, store.PausePodHoldReady, rig.clock.Now()); err != nil || !done {
					t.Fatalf("end the pause's hold of %s's pod at %s = %t, %v; want it ended", h.issue, rig.clock.Now().Format(time.RFC3339), done, err)
				}
			})
			rig.tick()
			if takenAt.IsZero() {
				t.Fatalf("the sweep at %s made no replacement call, so no launch turn was waited for", rig.clock.Now().Format(time.RFC3339))
			}
			replaced, refused := len(rig.replacementsOf(pod)), len(rig.rt.CallsOf("ReplacePodNotDue"))
			if row.ended {
				if replaced != 1 || refused != 0 {
					t.Errorf("the sweep at %s replaced the pod %d times and the last word refused it %d times, want one replacement: the pause's hold of it that began at %s ended 10 seconds later, before the last word, and the bound it moved, %s, has come",
						rig.clock.Now().Format(time.RFC3339), replaced, refused, takenAt.Format(time.RFC3339), bound.Add(10*time.Second).Format(time.RFC3339))
				}
				return
			}
			if replaced != 0 || refused != 1 {
				t.Errorf("the sweep at %s replaced the pod, with its disk, %d times and the last word refused it %d times, want one refusal and no replacement: a pause took the pod at %s while the replacement waited its turn, and its hold still stands; the bound, %s, does not let a replacement through a hold",
					rig.clock.Now().Format(time.RFC3339), replaced, refused, takenAt.Format(time.RFC3339), bound.Format(time.RFC3339))
			}
		})
	}
}

// A pause holds the later issue's pod from before the sweep, so the sweep reads the hold, and no
// round opens over the pod.
func TestPodUpdateNoRoundOpensOverAPodAPauseHeldBeforeTheSweep(t *testing.T) {
	slow, late := "ACME-1", "ACME-2"
	rig := newLimitRig(t, slow, late)
	rig.openSlowRound(slow, late)
	rig.clock.advance(time.Minute)
	rig.pauseHolds(late, rig.clock.Now())
	if !rig.sweepWaitingInAWrite(slow, 20*time.Second, func() { rig.clock.advance(20 * time.Second) }) {
		t.Fatalf("the sweep made no write for %s", slow)
	}
	if round, opened := rig.podUpdateOpening(late); opened {
		t.Errorf("the sweep opened round %s over %s's pod, which a pause held from before the sweep", round, late)
	}
}

// A pause takes the later issue's pod 5 seconds into the walk's 20-second write for the earlier
// issue, after the sweep's top read the holds, and the hold still stands when the walk reaches the
// later issue: the opening reads the holds again and opens no round over the pod.
func TestPodUpdateNoRoundOpensOverAPodAPauseTookWhileTheWalkWaited(t *testing.T) {
	slow, late := "ACME-1", "ACME-2"
	rig := newLimitRig(t, slow, late)
	rig.openSlowRound(slow, late)
	rig.clock.advance(time.Minute)
	if !rig.sweepWaitingInAWrite(slow, 20*time.Second, func() {
		rig.clock.advance(5 * time.Second)
		rig.pauseHolds(late, rig.clock.Now())
		rig.clock.advance(15 * time.Second)
	}) {
		t.Fatalf("the sweep made no write for %s", slow)
	}
	if round, opened := rig.podUpdateOpening(late); opened {
		t.Errorf("the sweep opened round %s over %s's pod, which a pause's hold took 5 seconds into the walk and still holds when the walk reached it", round, late)
	}
}

// A hold that ended before the sweep, whose held launch the pacing lets out 10 minutes on, stands
// on the pod no more: the opening reads that hold, ended, and opens the round. The round's bound
// counts the hold on to the let-out (heldUntil).
func TestPodUpdateARoundOpensOverAPodWhoseHoldEndedWithItsLaunchStillToBeLetOut(t *testing.T) {
	slow, late := "ACME-1", "ACME-2"
	rig := newLimitRig(t, slow, late)
	rig.openSlowRound(slow, late)
	rig.clock.advance(time.Minute)
	h := rig.holdAndEnd(late, time.Minute)
	rig.letOutLater(h, rig.clock.Now().Add(10*time.Minute))
	if !rig.sweepWaitingInAWrite(slow, 20*time.Second, func() { rig.clock.advance(20 * time.Second) }) {
		t.Fatalf("the sweep made no write for %s", slow)
	}
	if _, opened := rig.podUpdateOpening(late); !opened {
		t.Errorf("the sweep opened no round over %s's pod, whose pause's hold ended at %s, before the sweep, with its held launch let out 10 minutes later",
			late, h.endedAt.Format(time.RFC3339))
	}
}

// podUpdateOpening is the round this daemon has opened for issue and not yet seen its claims
// record (podUpdateOpenings), and whether there is one.
func (r *fleetRig) podUpdateOpening(issue string) (string, bool) {
	r.s.podUpdateMu.Lock()
	defer r.s.podUpdateMu.Unlock()
	round, opened := r.s.podUpdateOpened[issue]
	return round, opened
}

// lateRound opens late's round at a sweep of its own, with every claim of late read as recording
// no pod from then on (podless), so that a sweep past its bound tells the controller of it
// (tellPodlessRound). held, when it is not zero, is how long a pause held late's pod, from a
// minute after the opening, before a role's ready ended the hold, and the answer carries that
// hold. Thirty seconds before the round's bound, asked's round opens, and the walk of the sweep
// after it writes that round's ask, the write a test holds the walk in. The clock is left a minute
// later, 30 seconds past late's bound, which is answered too.
func lateRound(t *testing.T, rig *fleetRig, asked, late string, held time.Duration) (heldPod, time.Time) {
	t.Helper()
	rig.drifts(asked)
	rig.tick()
	rig.awaitRounds("the sweep that opened the late round", late)
	rig.podless(late)
	_, opened := rig.openedRound(late)
	var h heldPod
	if held > 0 {
		rig.clock.advance(time.Minute)
		h = rig.holdAndEnd(late, held)
	}
	bound := opened.Add(podUpdateGrace + h.endedAt.Sub(h.heldAt))
	rig.clock.advance(bound.Add(-30 * time.Second).Sub(rig.clock.Now()))
	rig.drifts(asked, limitDrift)
	rig.tick()
	rig.awaitRounds("the sweep that opened the asked round", asked, late)
	if n := len(rig.controllerNotices()); n != 0 {
		t.Fatalf("the late round was told %d times before its bound, want none", n)
	}
	rig.clock.advance(time.Minute)
	rig.sent(late, claim.RoleArchitect)
	return h, bound
}

// A late round's notice tells the controller the round is past its bound, and the controller acts
// on it. The pacing lets the held launch of the late round's pod out 10 minutes later while the
// walk waits in its write for the issue before it, after the sweep's top read a bound that has
// passed: the notice reads the holds again and tells nobody, since the bound is 40 minutes on. The
// first sweep at that bound tells the round once.
func TestPodUpdateALateRoundIsToldNothingBeforeABoundALetOutMovedWhileTheWalkWaited(t *testing.T) {
	asked, late := "ACME-1", "ACME-2"
	rig := newLimitRig(t, asked, late)
	h, bound := lateRound(t, rig, asked, late, time.Minute)
	var letOut time.Time
	if !rig.sweepWaitingInAWrite(asked, 20*time.Second, func() {
		letOut = rig.clock.Now().Add(10 * time.Minute)
		rig.letOutLater(h, letOut)
		rig.clock.advance(20 * time.Second)
	}) {
		t.Fatalf("the sweep made no write for %s", asked)
	}
	if notices := rig.controllerNotices(); len(notices) != 0 {
		t.Fatalf("the sweep at %s told the controller %d times that the late round is past its bound: the pacing let its held launch out at %s while the walk waited, so the bound is 40 minutes on; the sweep's top read %s",
			rig.clock.Now().Format(time.RFC3339), len(notices), letOut.Format(time.RFC3339), bound.Format(time.RFC3339))
	}

	_, opened := rig.openedRound(late)
	moved := opened.Add(podUpdateGrace + letOut.Sub(h.heldAt))
	rig.clock.advance(moved.Sub(rig.clock.Now()))
	rig.tick()
	if notices := rig.controllerNotices(); len(notices) != 1 {
		t.Fatalf("the sweep at the moved bound, %s, told the controller %d times of the late round, want once", moved.Format(time.RFC3339), len(notices))
	}
}

// A pause takes the late round's pod 5 seconds into the walk's 20-second write for the issue
// before it, after the sweep's top read no hold: the notice reads the holds again, and a pause's
// hold is told to nobody. Once a role's ready ends the hold, the next sweep tells the round once.
func TestPodUpdateALateRoundIsToldNothingOverAPodAPauseTookWhileTheWalkWaited(t *testing.T) {
	asked, late := "ACME-1", "ACME-2"
	rig := newLimitRig(t, asked, late)
	lateRound(t, rig, asked, late, 0)
	if !rig.sweepWaitingInAWrite(asked, 20*time.Second, func() {
		rig.clock.advance(5 * time.Second)
		rig.pauseHolds(late, rig.clock.Now())
		rig.clock.advance(15 * time.Second)
	}) {
		t.Fatalf("the sweep made no write for %s", asked)
	}
	if notices := rig.controllerNotices(); len(notices) != 0 {
		t.Fatalf("the sweep at %s told the controller %d times of the late round, whose pod a pause's hold took 5 seconds into the walk",
			rig.clock.Now().Format(time.RFC3339), len(notices))
	}

	if done, err := rig.sup.store.EndPausePodHold(rig.ctx, rig.s.cfg.Project, late, store.PausePodHoldReady, rig.clock.Now()); err != nil || !done {
		t.Fatalf("end the pause's hold of %s's pod = %t, %v; want it ended", late, done, err)
	}
	rig.clock.advance(time.Minute)
	rig.tick()
	if notices := rig.controllerNotices(); len(notices) != 1 {
		t.Fatalf("the sweep after the hold ended told the controller %d times of the late round, want once", len(notices))
	}
}

// An operator's suspension is a person's hold, and a pause's hold of the pod does not quiet its
// notice (readReplacement's order). The late round's implementer is suspended by its operator, its
// architect still runs in the pod, and a pause takes the pod 5 seconds into the walk's write for
// the issue before it, after the round's bound: the notice reads the holds again, finds the bound
// still past, and tells the operator's suspension once.
func TestPodUpdateALateRoundAnOperatorHoldsIsToldOverAPodAPauseTookWhileTheWalkWaited(t *testing.T) {
	asked, late := "ACME-1", "ACME-2"
	rig := newFleetRig(t, limitIssue(1, asked), fleetIssue{
		key: late, phase: phase.Implementing,
		pod:   fmt.Sprintf("%08d-7a92-4c1f-8d35-2f6b9c0ad417", 2),
		roles: []claim.Role{claim.RoleArchitect, claim.RoleImplementer},
	})
	rig.drifts(late, limitDrift)
	rig.tick()
	rig.awaitRounds("the sweep that opened the late round", late)
	_, opened := rig.openedRound(late)
	rig.handle(late, claim.RoleImplementer, supervise.RequestSuspend{Claim: rig.token(late, claim.RoleImplementer), By: supervise.SuspendedByOperator})
	testwait.Eventually(t, "the operator's suspension of "+late+"'s implementer to let its process go", func() bool {
		c := rig.claimOf(late, claim.RoleImplementer)
		return c.State == supervise.StateSuspended && c.Locator == nil
	})
	rig.sent(late, claim.RoleImplementer)

	bound := opened.Add(podUpdateGrace)
	rig.clock.advance(bound.Add(-30 * time.Second).Sub(rig.clock.Now()))
	rig.drifts(asked, limitDrift)
	rig.tick()
	rig.awaitRounds("the sweep that opened the asked round", asked, late)
	if n := len(rig.controllerNotices()); n != 0 {
		t.Fatalf("the late round was told %d times before its bound, want none", n)
	}

	rig.clock.advance(time.Minute)
	rig.sent(late, claim.RoleArchitect)
	if !rig.sweepWaitingInAWrite(asked, 20*time.Second, func() {
		rig.clock.advance(5 * time.Second)
		rig.pauseHolds(late, rig.clock.Now())
		rig.clock.advance(15 * time.Second)
	}) {
		t.Fatalf("the sweep made no write for %s", asked)
	}
	notices := rig.controllerNotices()
	if len(notices) != 1 {
		t.Fatalf("the sweep at %s told the controller %d times of the late round an operator's suspension holds, want once: a pause's hold quiets no operator's suspension",
			rig.clock.Now().Format(time.RFC3339), len(notices))
	}
	if !strings.Contains(notices[0].Reason, "An operator suspended") {
		t.Errorf("the late round's notice gives the reason %q, want the operator's suspension", notices[0].Reason)
	}
}
