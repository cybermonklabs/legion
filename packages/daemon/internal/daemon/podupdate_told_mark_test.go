package daemon

// The mark the sweep makes on each claim of a round it has told the controller of, past the
// round's bound (markUpdateTold, markOwedTold): the sweep never waits on a claim's machine to make
// it, a mark it cannot make then — the machine is busy, or the store refuses the write — is made
// at a later sweep, and a round is told once all the same. The limit rig is
// podupdate_limits_test.go's, with no limit set.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// holdInARelaunch holds the one worker of issue inside a Resume, as a relaunch into a pod being
// built holds it: the claim's machine keeps its own lock until release is called, which the
// test's cleanup calls too. A relaunching claim records no pod (supervise's letGo), so every sweep
// from here on reads issue podless.
func (r *fleetRig) holdInARelaunch(issue string) (release func()) {
	r.t.Helper()
	// The locator is read before the machine is held: a read of the claim itself waits the
	// decision in flight out (Machine.Claim).
	architect := r.token(issue, claim.RoleArchitect)
	died := *r.claimOf(issue, claim.RoleArchitect).Locator
	r.rt.ScriptResume(fake.SpawnResult{Locator: *podUpdateLocator(architect, issue, r.pods[issue])})
	entered, held := make(chan struct{}), make(chan struct{})
	release = sync.OnceFunc(func() { close(held) })
	r.t.Cleanup(release)
	r.rt.HoldResume(func() {
		r.rt.HoldResume(nil)
		close(entered)
		<-held
	})
	go func() {
		m, _ := r.sup.Machine(architect)
		_ = m.Handle(r.ctx, supervise.RuntimeObservation{
			Observation: runtime.Observation{Locator: died, Kind: runtime.Gone, Detail: "the process ended"},
		})
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		r.t.Fatal("the architect's machine never reached the Resume this test holds it in")
	}
	r.podless(issue)
	return release
}

// sweepEnds is the rig's tick run on a goroutine of its own, and fails the test, naming what,
// when the sweep has not ended within testwait's bound: a sweep that waits on a held machine would
// otherwise hold the test with it. The claims are read here, on the test's goroutine, which is
// the one a failed read may stop (sweepClaims).
func (r *fleetRig) sweepEnds(what string) {
	r.t.Helper()
	claims := r.sweepClaims()
	swept := make(chan struct{})
	go func() {
		defer close(swept)
		r.s.reconcilePodUpdates(r.ctx, claims)
	}()
	testwait.Eventually(r.t, what, func() bool {
		select {
		case <-swept:
			return true
		default:
			return false
		}
	})
}

// The sweep never waits on a claim's machine to mark a late round told. A relaunch into a pod
// being built holds the claim's machine for minutes, and a sweep that waited on it would hold
// every other issue's round, and the orphan reaping behind them, for as long. The mark of a claim
// whose machine is busy is made at a later sweep instead, dated when the round was told, and the
// round is told once all the same: not again while the mark waits, and not again after.
func TestPodUpdateTheSweepMarksALateRoundToldWithoutWaitingOnARelaunch(t *testing.T) {
	relaunching, other := "ACME-1", "ACME-2"
	rig := newLimitRig(t, relaunching, other)

	// The other issue drifts only after the first sweep, so its round is one the held sweep opens.
	rig.drifts(other)
	rig.tick()
	rig.awaitRounds("the first sweep", relaunching)
	rig.drifts(other, limitDrift)

	// Past the bound of the first issue's round, with its one worker in a relaunch, the sweep
	// tells the controller of the round, and goes on to open the other issue's round.
	release := rig.holdInARelaunch(relaunching)
	rig.clock.advance(podUpdateGrace + time.Minute)
	told := rig.clock.Now()
	rig.sweepEnds("the sweep past the late round's bound to end while a relaunch holds its one claim")
	if notices := rig.controllerNotices(); len(notices) != 1 {
		t.Fatalf("the sweep past the bound told the controller %d times, want once: %+v", len(notices), notices)
	}
	rig.wantPodUpdateRecord("the sweep that told the late round", api.PodUpdateRounds{Swept: true, InRound: []string{other}})
	rig.awaitRound(other, claim.RoleArchitect)

	// A minute on, the relaunch still holds the claim, and its mark is still owed: the round is
	// not told again.
	rig.clock.advance(time.Minute)
	rig.sweepEnds("a second sweep to end while the relaunch holds the claim")
	if notices := rig.controllerNotices(); len(notices) != 1 {
		t.Fatalf("the controller has been told %d times while the mark waited for the relaunch, want once: %+v", len(notices), notices)
	}

	// The relaunch ends, and a later sweep makes the mark it owed, dated when the round was told.
	// Which sweep is no part of what this pins: the machine can be deciding what the relaunch left
	// it at the first one.
	release()
	rig.sent(relaunching, claim.RoleArchitect)
	testwait.Eventually(t, "a sweep after the relaunch to make the told mark it owed", func() bool {
		rig.clock.advance(time.Minute)
		rig.tick()
		return rig.claimOf(relaunching, claim.RoleArchitect).UpdateToldAt != nil
	})
	if got := rig.claimOf(relaunching, claim.RoleArchitect).UpdateToldAt; !got.Equal(told) {
		t.Errorf("the round was marked told at %s, want %s, when the sweep told it", got, told)
	}
	rig.clock.advance(time.Minute)
	rig.tick()
	if notices := rig.controllerNotices(); len(notices) != 1 {
		t.Fatalf("the controller has been told %d times of the round, want once: %+v", len(notices), notices)
	}
}

// toldMarkRefusal is a store that refuses the told mark of one claim while refusing is set, as a
// database that refuses the row does: a write of that claim that carries a told mark answers an
// error, and every other write goes to the store behind it. refused counts the marks it refused,
// so a test can say that the sweep reached the refusal it is of rather than found the machine
// busy. The two counters are shared by every supervisor wire builds, a restarted daemon's too.
type toldMarkRefusal struct {
	supervise.Store
	token    claim.Token
	refusing *atomic.Bool
	refused  *atomic.Int32
}

func (s *toldMarkRefusal) PutClaim(ctx context.Context, c supervise.Claim) error {
	if c.Token == s.token && c.UpdateToldAt != nil && s.refusing.Load() {
		s.refused.Add(1)
		return errors.New("the database refused the row")
	}
	return s.Store.PutClaim(ctx, c)
}

// refusedMarkRig is the limit rig over one issue, with its architect's told mark refused by the
// store while refusing is set. The round is open, and every sweep from here on reads the issue
// podless, so its round waits for a claim to run again and is told once its bound has come
// (tellPodlessRound).
func refusedMarkRig(t *testing.T, issue string) (rig *fleetRig, refusing *atomic.Bool, refused *atomic.Int32) {
	t.Helper()
	refusing, refused = &atomic.Bool{}, &atomic.Int32{}
	token := mustClaimToken(t, issue, claim.RoleArchitect)
	rig = newWiredFleetRig(t, func(sup *supervisor) {
		sup.deps.Store = &toldMarkRefusal{Store: sup.deps.Store, token: token, refusing: refusing, refused: refused}
	}, limitIssue(1, issue))
	rig.drifts(issue, limitDrift)
	rig.tick()
	rig.awaitRounds("the first sweep", issue)
	rig.podless(issue)
	return rig, refusing, refused
}

// sweepRefusingTheMark is one sweep with the store refusing the told mark of issue's architect.
// A sweep that found the architect's machine deciding something would owe the mark without
// asking the store, so the machine is left to finish what it was posted first, and a sweep at
// which the store refused no mark fails the test: it never reached the refusal it is of.
func (r *fleetRig) sweepRefusingTheMark(issue string, refusing *atomic.Bool, refused *atomic.Int32) {
	r.t.Helper()
	r.sent(issue, claim.RoleArchitect)
	refusing.Store(true)
	before := refused.Load()
	r.tick()
	if refused.Load() == before {
		r.t.Fatalf("the store was asked for no told mark of %s at this sweep, so this run never reached the refusal it is of", issue)
	}
}

// The store refuses the told mark of a round the sweep has just told the controller of. The
// telling is in the store and the mark is the one thing of it left, so the sweep owes the mark as
// it owes a busy machine's: the round is not told again while the store refuses the mark, and a
// later sweep makes it, dated when the round was told.
func TestPodUpdateTheSweepMakesAToldMarkTheStoreRefusedAtALaterSweep(t *testing.T) {
	issue := "ACME-1"
	rig, refusing, refused := refusedMarkRig(t, issue)
	rig.clock.advance(podUpdateGrace + time.Minute)
	told := rig.clock.Now()
	rig.sweepRefusingTheMark(issue, refusing, refused)
	if notices := rig.controllerNotices(); len(notices) != 1 {
		t.Fatalf("the sweep past the bound told the controller %d times, want once: %+v", len(notices), notices)
	}

	// A minute on, the store still refuses the mark the sweep owes: it is owed still, and the
	// round is not told again.
	rig.clock.advance(time.Minute)
	rig.sweepRefusingTheMark(issue, refusing, refused)
	if notices := rig.controllerNotices(); len(notices) != 1 {
		t.Fatalf("the controller has been told %d times while the store refused the mark, want once: %+v", len(notices), notices)
	}

	// The store takes the mark again, and a later sweep makes it, dated when the round was told.
	refusing.Store(false)
	testwait.Eventually(t, "a sweep after the store takes writes again to make the told mark it owed", func() bool {
		rig.clock.advance(time.Minute)
		rig.tick()
		return rig.claimOf(issue, claim.RoleArchitect).UpdateToldAt != nil
	})
	if got := rig.claimOf(issue, claim.RoleArchitect).UpdateToldAt; !got.Equal(told) {
		t.Errorf("the round was marked told at %s, want %s, when the sweep told it", got, told)
	}
	rig.clock.advance(time.Minute)
	rig.tick()
	if notices := rig.controllerNotices(); len(notices) != 1 {
		t.Fatalf("the controller has been told %d times of the round, want once: %+v", len(notices), notices)
	}
}

// An owed told mark is kept in memory only, so a daemon that restarts before it makes the mark
// tells the round once more, and then marks it as the first daemon would have.
func TestPodUpdateADaemonThatRestartsBeforeAnOwedToldMarkTellsTheRoundOnceMore(t *testing.T) {
	issue := "ACME-1"
	rig, refusing, refused := refusedMarkRig(t, issue)
	rig.clock.advance(podUpdateGrace + time.Minute)
	rig.sweepRefusingTheMark(issue, refusing, refused)
	if notices := rig.controllerNotices(); len(notices) != 1 {
		t.Fatalf("the sweep past the bound told the controller %d times, want once: %+v", len(notices), notices)
	}

	next := rig.restart()
	next.clock.advance(time.Minute)
	retold := next.clock.Now()
	next.sweepRefusingTheMark(issue, refusing, refused)
	if notices := next.controllerNotices(); len(notices) != 2 {
		t.Fatalf("the restarted daemon's first sweep left %d notices of the round, want 2: the mark the first daemon owed went with it, so the round is told once more", len(notices))
	}

	refusing.Store(false)
	testwait.Eventually(t, "a sweep of the restarted daemon to make the told mark it owed", func() bool {
		next.clock.advance(time.Minute)
		next.tick()
		return next.claimOf(issue, claim.RoleArchitect).UpdateToldAt != nil
	})
	if got := next.claimOf(issue, claim.RoleArchitect).UpdateToldAt; !got.Equal(retold) {
		t.Errorf("the round was marked told at %s, want %s, when the restarted daemon told it", got, retold)
	}
	next.clock.advance(time.Minute)
	next.tick()
	if notices := next.controllerNotices(); len(notices) != 2 {
		t.Fatalf("the controller has been told %d times of the round, want twice: once by each daemon", len(notices))
	}
}

// A sweep that cannot read the pods a project's pause holds decides nothing (reconcilePodUpdates),
// and the told marks it owes are no exception: it makes none, and a later sweep that reads the
// holds makes each, dated when the round was told. The read fails here because the column it
// reads is renamed in this test's own database for one sweep.
func TestPodUpdateASweepThatCannotReadThePauseHoldsMakesNoToldMarkItOwes(t *testing.T) {
	issue := "ACME-1"
	rig, refusing, refused := refusedMarkRig(t, issue)
	rig.clock.advance(podUpdateGrace + time.Minute)
	told := rig.clock.Now()
	rig.sweepRefusingTheMark(issue, refusing, refused)
	if notices := rig.controllerNotices(); len(notices) != 1 {
		t.Fatalf("the sweep past the bound told the controller %d times, want once: %+v", len(notices), notices)
	}

	// The store takes the mark again, and the machine has finished what it was posted, but this
	// sweep cannot read the pause holds.
	refusing.Store(false)
	rig.sent(issue, claim.RoleArchitect)
	rig.pauseHoldsRead(false)
	rig.clock.advance(time.Minute)
	rig.tick()
	if got := rig.claimOf(issue, claim.RoleArchitect).UpdateToldAt; got != nil {
		t.Fatalf("a sweep that could not read the pause holds marked the round told at %s, want no mark: such a sweep decides nothing", got)
	}

	rig.pauseHoldsRead(true)
	testwait.Eventually(t, "a sweep that reads the pause holds to make the told mark it owed", func() bool {
		rig.clock.advance(time.Minute)
		rig.tick()
		return rig.claimOf(issue, claim.RoleArchitect).UpdateToldAt != nil
	})
	if got := rig.claimOf(issue, claim.RoleArchitect).UpdateToldAt; !got.Equal(told) {
		t.Errorf("the round was marked told at %s, want %s, when the sweep told it", got, told)
	}
	if notices := rig.controllerNotices(); len(notices) != 1 {
		t.Fatalf("the controller has been told %d times of the round, want once: %+v", len(notices), notices)
	}
}

// pauseHoldsRead makes the sweep's read of the pods a project's pause holds
// (store.Store.PausePodHoldsSince) fail, readable false, or read again, readable true, by renaming
// the column it reads in this rig's own database (isolatedOutboxPool).
func (r *fleetRig) pauseHoldsRead(readable bool) {
	r.t.Helper()
	from, to := "held_at", "held_at_unread"
	if readable {
		from, to = to, from
	}
	if _, err := r.pool.Exec(r.ctx, "alter table project_pause_pod_holds rename column "+from+" to "+to); err != nil {
		r.t.Fatalf("rename the pause holds' column %s to %s: %v", from, to, err)
	}
}

// A late round's told mark carries the moment the walk told the round, not the top of its sweep.
// Here the walk first writes the ask of an earlier issue's open round, which takes 20 seconds
// (sweepHeldInAWrite), and only then reaches the late round and tells it, so the two moments are
// 20 seconds apart.
func TestPodUpdateALateRoundsToldMarkCarriesTheMomentTheWalkToldIt(t *testing.T) {
	asked, late := "ACME-1", "ACME-2"
	rig := newLimitRig(t, asked, late)
	rig.drifts(asked)
	rig.tick()
	rig.awaitRounds("the sweep that opened the late round", late)
	rig.podless(late)

	// 30 seconds before the late round's bound, the asked issue's round opens; the walk of the
	// sweep after it writes that round's ask.
	rig.clock.advance(podUpdateGrace - 30*time.Second)
	rig.drifts(asked, limitDrift)
	rig.tick()
	rig.awaitRounds("the sweep that opened the asked round", asked, late)
	if n := len(rig.controllerNotices()); n != 0 {
		t.Fatalf("the late round was told %d times before its bound, want none", n)
	}

	rig.clock.advance(time.Minute)
	top := rig.clock.Now()
	rig.sent(late, claim.RoleArchitect)
	rig.sweepHeldInAWrite(asked, 20*time.Second)
	if n := len(rig.controllerNotices()); n != 1 {
		t.Fatalf("the sweep past the late round's bound told the controller %d times, want once", n)
	}
	told := top.Add(20 * time.Second)
	testwait.Eventually(t, "the late round's told mark to be made", func() bool {
		if rig.claimOf(late, claim.RoleArchitect).UpdateToldAt != nil {
			return true
		}
		rig.clock.advance(time.Minute)
		rig.tick()
		return false
	})
	if got := rig.claimOf(late, claim.RoleArchitect).UpdateToldAt; !got.Equal(told) {
		t.Errorf("the late round was marked told at %s, want %s: the walk told it %s after the top of its sweep, %s",
			got.Format(time.RFC3339), told.Format(time.RFC3339), told.Sub(top), top.Format(time.RFC3339))
	}
}
