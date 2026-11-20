package daemon

// The Spec entry pod-update-neither-rescues-nor-rechecks-push with a worker's machine busy while
// its two checks run. The sweep reads each claim through the deciding mask (roundGroup), so a sweep
// that meets a worker's machine inside a decision reads that worker as not having answered: it
// queues a row of the round's ask for an answered role, which reaches nobody because the row reads
// the role's own record (supervise's Machine.PublishUpdateAsk), or it leaves the replacement to the
// next sweep. The tests here run each check's own steps and assertions with a machine held that way
// at the very sweeps the check runs, so a check that counted the rows queued, or read the
// replacement at one sweep, fails here, as both did before they counted what reached each role
// and waited for the replacement.

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The first check (TestPodUpdateReplacesThePodOnlyOnceEveryBlockingClaimHasAnswered) with the
// implementer's machine inside a decision through the first sweep after the tester answers: that
// sweep reads the implementer unanswered and leaves the replacement to the next sweep, which the
// check waits for.
func TestPodUpdateReplacesThePodOnlyOnceEveryBlockingClaimHasAnsweredWhileAWorkersMachineDecides(t *testing.T) {
	replacesThePodOnceEveryBlockingClaimHasAnswered(t, func(rig *podUpdateRig) { rig.holdDeciding(claim.RoleImplementer, 1) })
}

// The second check (TestPodUpdateRoundOpenAtTheDeployKeepsItsAnswerAndAsksOnlyTheUnanswered) with
// the tester's machine inside a decision through the three sweeps after the implementer's ask: each
// reads the answered tester unanswered and may queue a row for it, and no ask reaches the tester.
func TestPodUpdateRoundOpenAtTheDeployAsksOnlyTheUnansweredWhileTheTestersMachineDecides(t *testing.T) {
	roundOpenAtTheDeployAsksOnlyTheUnanswered(t, func(rig *podUpdateRig) { rig.holdDeciding(claim.RoleTester, 3) })
}

// holdDeciding holds role's machine inside a decision that keeps its view ready and answered
// through the next ticks of the rig, then releases it, waits for the decision to end, and logs that
// the hold held. The decision is the claim's own answer given a second time, whose write of the
// claim waits on a lock the rig takes on the claim's row. It fails the test where the claim is not
// ready and answered, or where its machine is not deciding at the end of a held tick, since such a
// run shows nothing.
func (r *podUpdateRig) holdDeciding(role claim.Role, ticks int) {
	r.t.Helper()
	c := r.claimOf(role)
	m, ok := r.sup.Machine(r.tokens[role])
	if !ok {
		r.t.Fatalf("the %s is not supervised", role)
	}
	if c.UpdateSignalledAt == nil || !supervise.SaidReady(c.State) {
		r.t.Fatalf("the %s is %s with answer %v; a hold of its answer needs a ready claim that answered", role, c.State, c.UpdateSignalledAt)
	}
	bg := context.Background()
	tx, err := r.pool.Begin(bg)
	if err != nil {
		r.t.Fatalf("begin the transaction that locks the %s's claim row: %v", role, err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			_ = tx.Rollback(bg)
		}
	}
	r.t.Cleanup(func() { r.afterTick = nil; release() })
	if _, err := tx.Exec(bg, "select 1 from claims where token = $1 for update", string(c.Token)); err != nil {
		r.t.Fatalf("lock the %s's claim row: %v", role, err)
	}
	answered := make(chan error, 1)
	go func() {
		answered <- m.Handle(r.ctx, supervise.PodUpdateSignalled{
			Claim: c.Token, Generation: c.Generation, Session: c.Session, Round: c.UpdateRound, Head: c.UpdateHead,
		})
	}()
	testwait.Eventually(r.t, "the "+string(role)+"'s machine to be deciding its answer given again", func() bool { return m.Deciding() != nil })
	if v := m.View().Claim; v.UpdateSignalledAt == nil || !supervise.SaidReady(v.State) {
		r.t.Fatalf("the %s's view while it decides is %s, answered %v; want it ready and answered, so only the mask can read it unanswered", role, v.State, v.UpdateSignalledAt)
	}
	left := ticks
	r.afterTick = func() {
		if m.Deciding() == nil {
			r.t.Fatalf("the %s's machine ended its decision during a held tick, so this run shows nothing", role)
		}
		if left--; left > 0 {
			return
		}
		r.afterTick = nil
		release()
		select {
		case err := <-answered:
			if err != nil {
				r.t.Fatalf("the %s's answer given again: %v", role, err)
			}
		case <-time.After(2 * time.Minute):
			r.t.Fatalf("the %s's answer given again never ended once its claim row was released", role)
		}
		r.t.Logf("the %s's machine was deciding at the end of each of %d held ticks, and its decision has ended", role, ticks)
	}
}
