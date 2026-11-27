package supervise

// The stale-plugin refresh gate's bound: no more than K = wakeRate plugin-refresh wakes wait in
// the daemon's wake pacing at once, across every road that puts such a wake into the pacing
// (RefreshGate, WakePluginRefresh, dispatch://INFRA-4110). A machine admitted to the lane keeps
// its credit for as long as a launch of the lane is owed on it — through a retry after a failed
// write, an unanswered session hold or a refused lifecycle check, through a project pause's gate,
// and through a wait at its issue pod's start gate — and gives it back as soon as that launch
// ends or is dropped.
//
// The four paths of dispatch://INFRA-4110/spec, Problem, are named here by their number. Path 1
// is a launch whose StateLaunching write the store did not take (writeTry), then its retry on
// TimerRelaunch. Path 2 is a start whose session hold the store did not answer
// (errHoldUnanswered), then its retry. Path 3 is the retry a refused lifecycle check arms
// (relaunchRefused). Path 4 is a let-out of the project pause's gate (letOutHeldLaunch).
//
// This file uses only identifiers that exist in internal/supervise on main at 8868205a18f3, so
// that the negative control compiles and runs against main as well as against the change.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// boundRig is a paced daemon of stale idle claims behind one RefreshGate, sharing one store,
// clock, runtime, pacer and optional pause flag or issue-pod start gate. h.m is walked to idle to
// spend the pacer's initial burst so that every admitted refresh parks, and is left on the current
// plugin contract; the stale claims live in machines, each on an issue of its own so that a
// Deps.PodStarts gate holds no claim behind another claim's pod unless the test adds a second role
// to that claim's issue. Every claim's token is distinct from waveToken(0..n-1), which queueAhead
// takes.
type boundRig struct {
	t        *testing.T
	h        *harness
	gate     *RefreshGate
	pause    *pauseFlag
	machines []*Machine
	spawned  int
}

// boundRigOptions configures optional Deps shared by every machine of a boundRig.
type boundRigOptions struct {
	withPause bool
	withPods  bool
}

// newBoundRig builds a boundRig with n stale idle claims (issues LEGION-301 .. LEGION-(300+n)).
// Each stale claim is created through h.machineFor so its machine starts with no cached session
// holds, which lets a test fail HoldSessions at its first relaunch try (path 2) as a restarted
// daemon's claim does.
func newBoundRig(t *testing.T, n int, opts boundRigOptions) *boundRig {
	t.Helper()
	gate := NewRefreshGate(wakeRate)
	var flag *pauseFlag

	var tunes []func(*Deps)
	tunes = append(tunes, func(d *Deps) {
		d.RefreshGate = gate
	})
	if opts.withPause {
		flag = &pauseFlag{}
		tunes = append(tunes, func(d *Deps) {
			stops := &stopTransaction{store: d.Store.(*memStore)}
			d.Paused, d.PauseStopped = flag.Paused, stops.PauseStopped
		})
	}
	if opts.withPods {
		tunes = append(tunes, func(d *Deps) {
			clock := d.Clock.(*fakeClock)
			pods := NewPodStarts(clock)
			pods.post = clock.whenStill
			d.PodStarts = pods
		})
	}

	h := pacedHarness(t, wakeRate, tunes...)

	// Walk h.m to idle on the harness's initial plugin contract to spend the limiter's burst so
	// that every admitted refresh parks. Moving h.deps.PluginContract to refreshContract after
	// reach leaves h.m's own copy on the initial contract, so h.m is never part of the stale set.
	h.reach(StateIdle)
	h.deps.PluginContract = refreshContract
	r := &boundRig{
		t:        t,
		h:        h,
		gate:     gate,
		pause:    flag,
		machines: make([]*Machine, 0, n),
	}
	for i := range n {
		r.machines = append(r.machines, h.machineFor(boundStaleClaim(i+1)))
	}
	return r
}

// boundStaleClaim is a stale idle worker claim on issue LEGION-(300+index) with a token and
// session of its own.
func boundStaleClaim(index int) Claim {
	issue := fmt.Sprintf("LEGION-%d", 300+index)
	token := claim.Token(fmt.Sprintf("legion-%s-implementer", issue))
	c := stalePluginClaim(wakeClaim(token, StateIdle), staleContract)
	c.Issue = issue
	c.State = StateIdle
	return c
}

// addPodRole builds a second role claim in the same issue pod as m, suspended over a session of
// its own so an operator's resume starts it at once and holds that issue's pod start gate. It is
// not added to r.machines because it is not one of the stale claims the sweep posts.
func (r *boundRig) addPodRole(m *Machine, role claim.Role) *Machine {
	r.t.Helper()
	issue := m.View().Issue
	token := claim.Token(fmt.Sprintf("legion-%s-%s", issue, role))
	c := wakeClaim(token, StateSuspended)
	c.Issue = issue
	c.Role = role
	return r.h.machineFor(c)
}

// waitingRefreshes is the number of stale-claim machines whose published view names a waiting
// WakePluginRefresh wake.
func (r *boundRig) waitingRefreshes() int {
	n := 0
	for _, m := range r.machines {
		if w := m.View().WakeWaiting; w != nil && w.Kind == WakePluginRefresh {
			n++
		}
	}
	return n
}

// creditsOut is the number of credits the refresh gate has granted and not had back.
func (r *boundRig) creditsOut() int {
	r.gate.mu.Lock()
	defer r.gate.mu.Unlock()
	return len(r.gate.out)
}

// everyWaitingRefreshHoldsACredit reports whether every machine whose published view shows a
// waiting WakePluginRefresh wake holds a non-nil m.refreshCredit that is still a key of
// r.gate.out. When false, it returns the first offending claim's token and a reason.
func (r *boundRig) everyWaitingRefreshHoldsACredit() (bool, claim.Token, string) {
	for _, m := range r.machines {
		w := m.View().WakeWaiting
		if w == nil || w.Kind != WakePluginRefresh {
			continue
		}
		m.mu.Lock()
		credit := m.refreshCredit
		m.mu.Unlock()
		if credit == nil {
			return false, m.View().Token, "holds no refresh credit"
		}
		r.gate.mu.Lock()
		_, out := r.gate.out[credit]
		r.gate.mu.Unlock()
		if !out {
			return false, m.View().Token, "holds a credit that is no longer in gate.out"
		}
	}
	return true, "", ""
}

// sweep does what refreshStalePlugins in internal/daemon does, in its order: it calls
// gate.Correct(waitingRefreshes()), then admits each stale idle machine that holds no wake (read
// from m.wake under m.mu) and is not in a turn, stopping at the first nil credit, posting
// StalePlugin{Credit: c}, and waiting on every machine.
func (r *boundRig) sweep() int {
	r.t.Helper()
	r.gate.Correct(r.waitingRefreshes())
	return r.admitIdle()
}

// admitIdle is the admission half of sweep, split out so a test that runs gate.Correct at one
// moment (for example inside a held Resume) can admit the gate's remaining share right after.
func (r *boundRig) admitIdle() int {
	r.t.Helper()
	admitted := 0
	for _, m := range r.machines {
		if !r.eligibleForSweep(m) {
			continue
		}
		c := r.gate.Admit()
		if c == nil {
			break
		}
		r.h.mustPostTo(m, StalePlugin{Claim: m.View().Token, Trigger: "sweep", Credit: c})
		admitted++
	}
	r.waitAll()
	return admitted
}

// eligibleForSweep reports whether m is a stale idle claim that holds no wake and is not in a
// turn. It reads m.wake under m.mu rather than the published view so that a wake held by the
// pause's gate (which the view hides) is still seen as held.
func (r *boundRig) eligibleForSweep(m *Machine) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.claim.State == StateIdle &&
		m.claim.PluginContract != m.deps.PluginContract &&
		m.wake == nil
}

// postAdmitted takes one credit from the gate (failing if the gate refuses) and posts a sweep
// StalePlugin carrying it to m.
func (r *boundRig) postAdmitted(m *Machine) {
	r.t.Helper()
	c := r.gate.Admit()
	if c == nil {
		r.t.Fatalf("setup: the gate refused an admission for %s (%d credits out, %d waiting refreshes)",
			m.View().Token, r.creditsOut(), r.waitingRefreshes())
	}
	r.h.mustPostTo(m, StalePlugin{Claim: m.View().Token, Trigger: "sweep", Credit: c})
}

// waitAll waits for every machine in the rig (and h.m) to settle.
func (r *boundRig) waitAll() {
	r.t.Helper()
	r.h.m.Wait()
	for _, m := range r.machines {
		m.Wait()
	}
}

// advance moves the shared clock by d, waits for any posted pod-gate grants to return, and waits
// on every machine in the rig.
func (r *boundRig) advance(d time.Duration) {
	r.t.Helper()
	r.h.clock.Advance(d)
	r.h.postsReturned()
	r.waitAll()
}

// advanceToTurn moves the shared clock step by step until m's currently waiting paced wake runs,
// then waits for any posted pod-gate grants and every machine in the rig.
func (r *boundRig) advanceToTurn(m *Machine) {
	r.t.Helper()
	r.h.advanceToTurn(m)
	r.h.postsReturned()
	r.waitAll()
}

// wantWithinRate checks the lane's bound after step: no more than wakeRate plugin-refresh wakes
// wait in the pacing. Its failure names the reading and the bound.
func (r *boundRig) wantWithinRate(step string) {
	r.t.Helper()
	if got := r.waitingRefreshes(); got > wakeRate {
		r.t.Fatalf("%s: %d plugin-refresh wakes wait, want at most %d", step, got, wakeRate)
	}
}

// wakeSnapshot is the fields of m.wake read under m.mu that exist on main at 8868205a18f3 and
// after the change.
type wakeSnapshot struct {
	held   bool
	kind   WakeKind
	pause  int64
	parked bool
}

func readWake(m *Machine) wakeSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.wake == nil {
		return wakeSnapshot{}
	}
	return wakeSnapshot{
		held:   true,
		kind:   m.wake.kind,
		pause:  m.wake.pause,
		parked: m.wake.park != nil,
	}
}

func timerArmed(m *Machine, kind TimerKind) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.timers[kind]
	return ok
}

// wantRetrySetup checks that a launch failure armed TimerRelaunch on m over a launching claim
// with no process and no waiting wake.
func wantRetrySetup(t *testing.T, m *Machine, why string) {
	t.Helper()
	c := m.View().Claim
	if c.State != StateLaunching || c.Locator != nil {
		t.Fatalf("setup (%s): %s is %s recording %+v, want %s recording no process",
			why, c.Token, c.State, c.Locator, StateLaunching)
	}
	if !timerArmed(m, TimerRelaunch) {
		t.Fatalf("setup (%s): %s did not arm TimerRelaunch", why, c.Token)
	}
	if w := m.View().WakeWaiting; w != nil {
		t.Fatalf("setup (%s): %s still shows waiting wake %+v, want none while its retry waits", why, c.Token, w)
	}
}

// wantWaitingKind checks that m's published view names a waiting wake of kind.
func wantWaitingKind(t *testing.T, m *Machine, kind WakeKind, why string) WakeWaiting {
	t.Helper()
	w := m.View().WakeWaiting
	if w == nil || w.Kind != kind {
		t.Fatalf("setup (%s): %s names wake %+v, want a waiting %s wake", why, m.View().Token, w, kind)
	}
	return *w
}

// wantOthersWaiting checks that every machine in others still has a waiting WakePluginRefresh
// wake in its published view, so that a test proves the path's wake entered the pacing while the
// other K-1 lane wakes still waited.
func wantOthersWaiting(t *testing.T, others []*Machine, why string) {
	t.Helper()
	for _, m := range others {
		wantWaitingKind(t, m, WakePluginRefresh, why)
	}
}

// Every road that can park a plugin-refresh wake in the pacing keeps the lane within K = wakeRate
// waiting wakes, even when K+1 stale idle claims compete for the gate and a sweep runs right
// after the step that used to drop the credit. Each subtest drives one road on
// the claim under test (failed StateLaunching write, unanswered HoldSessions, refused lifecycle
// recheck on a retry, resume before the pause's stop on a wake held by pauseGate, and an
// unanswered HoldSessions after a park at the issue pod's start gate) while the other K-1 lane
// wakes stay waiting behind a queue of spawns.
//
// The negative control is main at 8868205a18f3: each subtest gives the claim's credit back when
// its launch leaves the pacing, the sweep admits the (K+1)th claim onto that credit, and the
// path's wake then parks with no credit, reading 9 waiting plugin-refresh wakes against the
// bound of 8.
func TestRefreshBoundEachPathKeepsTheLaneWithinTheRate(t *testing.T) {
	const queueDepth = 12

	t.Run("path_1_write_failed", func(t *testing.T) {
		r := newBoundRig(t, wakeRate+1, boundRigOptions{})
		u := r.machines[0]
		others := r.machines[1:wakeRate]

		// Admit the claim under test at the head of the pacing, put a queue of spawns behind it
		// so the other K-1 lane wakes stay waiting while u's retry waits out testProbe, then
		// finish the first sweep to fill the gate with K lane wakes.
		r.postAdmitted(u)
		wantWaitingKind(t, u, WakePluginRefresh, "after first admission")
		queueAhead(r.h, queueDepth)
		if got := r.sweep(); got != wakeRate-1 {
			t.Fatalf("setup: first sweep admitted %d claims, want %d", got, wakeRate-1)
		}
		wantOthersWaiting(t, others, "after first sweep")
		r.wantWithinRate("after first sweep")

		// Path 1: PutClaim fails at u's turn, arming TimerRelaunch.
		r.h.store.fail("PutClaim", errBoom)
		r.advanceToTurn(u)
		r.h.store.fail("PutClaim", nil)
		wantRetrySetup(t, u, "path 1 failed StateLaunching write")
		r.wantWithinRate("after path 1 armed retry")

		// Sweep after the step where main at 8868205a18f3 returns u's credit.
		r.sweep()
		r.wantWithinRate("after sweep while path 1 retry waits")

		// u's retry fires and enters the pacing while the other K-1 lane wakes still wait.
		r.advance(testProbe)
		wantWaitingKind(t, u, WakePluginRefresh, "after path 1 retry fired")
		wantOthersWaiting(t, others, "when path 1 retry entered the pacing")
		r.wantWithinRate("after path 1 retry entered the pacing")
	})

	t.Run("path_2_hold_unanswered", func(t *testing.T) {
		r := newBoundRig(t, wakeRate+1, boundRigOptions{})
		u := r.machines[0]
		others := r.machines[1:wakeRate]

		r.postAdmitted(u)
		wantWaitingKind(t, u, WakePluginRefresh, "after first admission")
		queueAhead(r.h, queueDepth)
		if got := r.sweep(); got != wakeRate-1 {
			t.Fatalf("setup: first sweep admitted %d claims, want %d", got, wakeRate-1)
		}
		wantOthersWaiting(t, others, "after first sweep")
		r.wantWithinRate("after first sweep")

		// Path 2: HoldSessions fails at u's turn (errHoldUnanswered), arming TimerRelaunch.
		r.h.store.fail("HoldSessions", errBoom)
		r.advanceToTurn(u)
		r.h.store.fail("HoldSessions", nil)
		wantRetrySetup(t, u, "path 2 unanswered HoldSessions")
		r.wantWithinRate("after path 2 armed retry")

		// Sweep after the step where main at 8868205a18f3 returns u's credit.
		r.sweep()
		r.wantWithinRate("after sweep while path 2 retry waits")

		// u's retry fires and enters the pacing while the other K-1 lane wakes still wait.
		r.advance(testProbe)
		wantWaitingKind(t, u, WakePluginRefresh, "after path 2 retry fired")
		wantOthersWaiting(t, others, "when path 2 retry entered the pacing")
		r.wantWithinRate("after path 2 retry entered the pacing")
	})

	t.Run("path_3_refused_retry", func(t *testing.T) {
		r := newBoundRig(t, wakeRate+1, boundRigOptions{})
		u := r.machines[0]
		others := r.machines[1:wakeRate]

		r.postAdmitted(u)
		wantWaitingKind(t, u, WakePluginRefresh, "after first admission")
		queueAhead(r.h, queueDepth)
		if got := r.sweep(); got != wakeRate-1 {
			t.Fatalf("setup: first sweep admitted %d claims, want %d", got, wakeRate-1)
		}
		wantOthersWaiting(t, others, "after first sweep")
		r.wantWithinRate("after first sweep")

		// Arm the first retry via path 1 so u is live with no process.
		r.h.store.fail("PutClaim", errBoom)
		r.advanceToTurn(u)
		r.h.store.fail("PutClaim", nil)
		wantRetrySetup(t, u, "path 3 opening failed write")
		r.wantWithinRate("after opening retry armed")

		// Sweep after the opening failure (where main at 8868205a18f3 returns u's credit).
		r.sweep()
		r.wantWithinRate("after sweep before refused retry")

		// Path 3 (row A5 of dispatch://INFRA-4110/artifact/plan-md): when the first retry's timer
		// fires, u's store cannot answer CheckLaunch, so launchAfter refuses it and
		// relaunchRefused arms a second retry.
		checkStore := &unansweredCheckLaunchStore{memStore: r.h.store, failing: true}
		u.deps.Store = checkStore
		r.advance(testProbe)
		wantRetrySetup(t, u, "path 3 second retry after refused CheckLaunch")
		r.wantWithinRate("after path 3 armed second retry")

		// The store answers again; sweep again before the second retry's wake enters the pacing.
		checkStore.failing = false
		r.sweep()
		r.wantWithinRate("after sweep while path 3 second retry waits")

		// The second retry fires and enters the pacing while the other K-1 lane wakes still wait.
		r.advance(testProbe)
		wantWaitingKind(t, u, WakePluginRefresh, "after path 3 second retry fired")
		wantOthersWaiting(t, others, "when path 3 second retry entered the pacing")
		r.wantWithinRate("after path 3 second retry entered the pacing")
	})

	t.Run("path_4_resume_before_the_pause_stop", func(t *testing.T) {
		r := newBoundRig(t, wakeRate+1, boundRigOptions{withPause: true})
		// Reserve the Kth credit up front so the first sweep (with no pause) admits K-1 other
		// claims and stops, leaving machines[wakeRate-1] un-admitted and u = machines[wakeRate]
		// ready to receive the Kth credit once the project is paused.
		others := r.machines[:wakeRate-1]
		u := r.machines[wakeRate]

		heldCredit := admit(t, r.gate)
		if got := r.sweep(); got != wakeRate-1 {
			t.Fatalf("setup: first sweep admitted %d claims, want %d", got, wakeRate-1)
		}
		wantOthersWaiting(t, others, "after first sweep")
		r.wantWithinRate("after first sweep")

		// Pause the project and post the Kth credit to u so reserveWake gives its wake to
		// pauseGate (TimerPause armed at 0, wake hidden from the published view).
		r.pause.set(7)
		r.h.mustPostTo(u, StalePlugin{Claim: u.View().Token, Trigger: "sweep", Credit: heldCredit})
		ws := readWake(u)
		if !ws.held || ws.kind != WakePluginRefresh || ws.pause != 7 {
			t.Fatalf("setup: u's wake = %+v, want a WakePluginRefresh held for pause 7", ws)
		}
		if !timerArmed(u, TimerPause) {
			t.Fatal("setup: pauseGate did not arm TimerPause on u")
		}
		if w := u.View().WakeWaiting; w != nil {
			t.Fatalf("setup: u's view shows %+v while paused, want none", w)
		}
		r.wantWithinRate("after pauseGate held u's refresh")

		// Clear the pause flag and sweep before sending ResumePause (no clock advance, so
		// TimerPause has not fired). On main at 8868205a18f3, u's credit went back at pauseGate's
		// decision, so this sweep admits machines[wakeRate-1].
		r.pause.set(0)
		r.sweep()
		r.wantWithinRate("after sweep while u's refresh is held by pauseGate")

		// ResumePause lets u's held launch out into the pacing behind the other K-1 lane wakes.
		r.h.mustPostTo(u, ResumePause{Claim: u.View().Token, Pause: 7})
		wantWaitingKind(t, u, WakePluginRefresh, "after ResumePause let u's wake out")
		wantOthersWaiting(t, others, "when path 4 let-out entered the pacing")
		r.wantWithinRate("after path 4 let-out entered the pacing")
	})

	t.Run("pod_gate_then_path_2", func(t *testing.T) {
		r := newBoundRig(t, wakeRate+1, boundRigOptions{withPods: true})
		u := r.machines[0]
		others := r.machines[1:wakeRate]
		peer := r.addPodRole(u, claim.RoleArchitect)

		r.postAdmitted(u)
		wantWaitingKind(t, u, WakePluginRefresh, "after first admission")
		queueAhead(r.h, queueDepth)
		if got := r.sweep(); got != wakeRate-1 {
			t.Fatalf("setup: first sweep admitted %d claims, want %d", got, wakeRate-1)
		}
		wantOthersWaiting(t, others, "after first sweep")
		r.wantWithinRate("after first sweep")

		// Start the second role in u's issue pod first (operator resume is exempt from wake
		// pacing and starts immediately), so it holds u's issue pod while loading.
		peerToken := peer.View().Token
		r.h.mustPostTo(peer, RequestResume{Claim: peerToken, By: SuspendedByOperator})
		r.h.wantStarts(peerToken, 1)

		// u's lane wake runs and parks its try at its issue pod's start gate (askPod).
		r.advanceToTurn(u)
		ws := readWake(u)
		if !ws.held || !ws.parked {
			t.Fatalf("setup: after u's turn its wake = %+v, want a try parked at the pod gate", ws)
		}
		if w := u.View().WakeWaiting; w != nil {
			t.Fatalf("setup: u's view shows %+v while parked at the pod gate, want none", w)
		}
		r.h.wantStarts(u.View().Token, 0)
		r.wantWithinRate("after u parked at the pod start gate")

		// Sweep after the pod park (where main at 8868205a18f3 has already returned u's credit).
		r.sweep()
		r.wantWithinRate("after sweep while u waits at the pod start gate")

		// End the peer's session load (register, then settle) while HoldSessions fails, so u's
		// granted start hits errHoldUnanswered and arms path 2's retry.
		r.registerPeer(peer)
		r.h.store.fail("HoldSessions", errBoom)
		r.advance(podLoadSettle)
		r.h.store.fail("HoldSessions", nil)

		wantRetrySetup(t, u, "path 2 after pod grant")
		r.wantWithinRate("after pod grant armed path 2 retry")

		// Sweep again before the retry enters the pacing.
		r.sweep()
		r.wantWithinRate("after sweep while post-pod retry waits")

		// The retry fires and enters the pacing while the other K-1 lane wakes still wait.
		r.advance(testProbe)
		wantWaitingKind(t, u, WakePluginRefresh, "after post-pod path 2 retry fired")
		wantOthersWaiting(t, others, "when post-pod path 2 retry entered the pacing")
		r.wantWithinRate("after post-pod path 2 retry entered the pacing")
	})
}

// A wake parked without a credit can hide behind a credit whose turn is running, so a sweep's
// correction under-counts the lane and the admissions right after the turn ends lift the waiting
// plugin-refresh wakes above K = wakeRate (dispatch://INFRA-4110/spec, Problem). Three claims
// R1..R3 take path 1 and re-enter the pacing behind a fourth admitted claim T; while T's turn is
// held inside Resume, the views show only R1..R3 (3 wakes) and no wake of T, and gate.Correct(3)
// runs. When T's turn ends, admitting until nil and posting to stale idle claims must never leave
// more than K plugin-refresh wakes waiting.
//
// The negative control is main at 8868205a18f3: R1..R3 hold no credit, so Correct(3) sets
// gate.held to max(3, 1) = 3; T's turn ends and drops gate.held to 2; the gate admits 6 more
// claims, leaving 3 + 6 = 9 plugin-refresh wakes waiting against the bound of 8.
func TestRefreshBoundAHiddenWakeCannotLiftTheLaneOverTheRate(t *testing.T) {
	// 3 retries (R1..R3) + 1 running-turn claim (T) + 7 extra stale idle claims so the gate,
	// never the claim supply, bounds the post-turn admissions.
	r := newBoundRig(t, 11, boundRigOptions{})
	retries := r.machines[0:3]
	turnClaim := r.machines[3]

	// Step 1: admit R1..R3 and fail PutClaim at each of their turns so all three have retries
	// armed on TimerRelaunch (path 1).
	for _, m := range retries {
		r.postAdmitted(m)
		wantWaitingKind(t, m, WakePluginRefresh, "after admitting retry claim")
	}
	r.wantWithinRate("after admitting R1..R3")

	r.h.store.fail("PutClaim", errBoom)
	for _, m := range retries {
		r.advanceToTurn(m)
		wantRetrySetup(t, m, "path 1 retry armed on R1..R3")
	}
	r.h.store.fail("PutClaim", nil)
	r.wantWithinRate("after R1..R3 armed path 1 retries")

	// Step 2: queue spawns ahead of T so T's turn falls after all three retries' probe timers
	// fire, then admit T and park its lane wake behind that queue.
	queueAhead(r.h, 6)
	r.postAdmitted(turnClaim)
	wantWaitingKind(t, turnClaim, WakePluginRefresh, "after admitting T")
	r.wantWithinRate("after admitting T")

	// Step 3: advance by testProbe so all three retries' timers fire and their wakes park in the
	// pacing behind T's wake, while T still waits.
	r.advance(testProbe)
	for _, m := range retries {
		wantWaitingKind(t, m, WakePluginRefresh, "after R1..R3 retry fired")
	}
	wantWaitingKind(t, turnClaim, WakePluginRefresh, "while T still waits ahead of R1..R3")
	r.wantWithinRate("after R1..R3 parked behind T")

	// Step 4: hold T's relaunch inside the fake runtime's Resume. While it is held, the views
	// show R1..R3 (3 waiting wakes) and no wake of T; run gate.Correct(waitingRefreshes()) there.
	type holdReading struct {
		turnWakeShown bool
		waiting       int
	}
	during := make(chan holdReading, 1)
	r.h.rt.HoldResume(func() {
		r.h.rt.HoldResume(nil)
		reading := holdReading{
			turnWakeShown: turnClaim.View().WakeWaiting != nil,
			waiting:       r.waitingRefreshes(),
		}
		r.gate.Correct(reading.waiting)
		during <- reading
	})

	r.advanceToTurn(turnClaim)

	var reading holdReading
	select {
	case reading = <-during:
	default:
		t.Fatal("setup: T's turn did not reach Resume; HoldResume was never entered")
	}
	if reading.turnWakeShown {
		t.Fatal("setup: T's view still showed a waiting wake during its held Resume, want none")
	}
	if reading.waiting != 3 {
		t.Fatalf("setup: %d plugin-refresh wakes waited during T's held Resume, want 3 (R1..R3)", reading.waiting)
	}
	if w := turnClaim.View().WakeWaiting; w != nil {
		t.Fatalf("setup: T's view still shows %+v after its turn ended, want none", w)
	}
	for _, m := range retries {
		wantWaitingKind(t, m, WakePluginRefresh, "after T's turn ended")
	}
	r.wantWithinRate("after T's turn ended")

	// Step 5: admit until nil and post each credit to a stale idle claim, whose wake parks
	// behind R1..R3.
	r.admitIdle()
	r.wantWithinRate("after post-turn admissions")
}

// A retry of a processless claim (paths 1, 2, 3, and path 2 after a pod grant) enters the pacing
// at its probe-interval timer, and a let-out of path 4 enters the pacing at ResumePause, even
// while the refresh gate is full (dispatch://INFRA-4110/spec#b-r2). Neither waits for a free
// credit or a later sweep.
//
// This test passes on main at 8868205a18f3 as well as after the change, and fails for the
// refused design that makes a retry or let-out ask the gate and wait for a free credit.
func TestRefreshBoundARetryEntersThePacingAtItsTimer(t *testing.T) {
	const queueDepth = 12

	// fillRest takes the other K-1 credits from r.gate so the gate has no free credit while the
	// claim under test holds the Kth.
	fillRest := func(t *testing.T, r *boundRig) {
		t.Helper()
		for range wakeRate - 1 {
			admit(t, r.gate)
		}
		if free := freeCredits(r.gate); free != 0 {
			t.Fatalf("setup: gate still has %d free credits, want 0 (full)", free)
		}
	}

	// wantEnteredAtTimer checks that after advancing testProbe, m shows a waiting
	// WakePluginRefresh wake and TimerRelaunch is no longer armed.
	wantEnteredAtTimer := func(t *testing.T, r *boundRig, m *Machine, road string) {
		t.Helper()
		r.advance(testProbe)
		wantWaitingKind(t, m, WakePluginRefresh, road+" at TimerRelaunch")
		if timerArmed(m, TimerRelaunch) {
			t.Fatalf("%s: TimerRelaunch is still armed after advancing testProbe, want it fired", road)
		}
	}

	t.Run("path_1_write_failed", func(t *testing.T) {
		r := newBoundRig(t, 1, boundRigOptions{})
		u := r.machines[0]
		r.postAdmitted(u)
		wantWaitingKind(t, u, WakePluginRefresh, "after admission")
		fillRest(t, r)
		queueAhead(r.h, queueDepth)

		r.h.store.fail("PutClaim", errBoom)
		r.advanceToTurn(u)
		r.h.store.fail("PutClaim", nil)
		wantRetrySetup(t, u, "path 1")

		wantEnteredAtTimer(t, r, u, "path 1")
	})

	t.Run("path_2_hold_unanswered", func(t *testing.T) {
		r := newBoundRig(t, 1, boundRigOptions{})
		u := r.machines[0]
		r.postAdmitted(u)
		wantWaitingKind(t, u, WakePluginRefresh, "after admission")
		fillRest(t, r)
		queueAhead(r.h, queueDepth)

		r.h.store.fail("HoldSessions", errBoom)
		r.advanceToTurn(u)
		r.h.store.fail("HoldSessions", nil)
		wantRetrySetup(t, u, "path 2")

		wantEnteredAtTimer(t, r, u, "path 2")
	})

	t.Run("path_3_refused_retry", func(t *testing.T) {
		r := newBoundRig(t, 1, boundRigOptions{})
		u := r.machines[0]
		r.postAdmitted(u)
		wantWaitingKind(t, u, WakePluginRefresh, "after admission")
		fillRest(t, r)
		queueAhead(r.h, queueDepth)

		r.h.store.fail("PutClaim", errBoom)
		r.advanceToTurn(u)
		r.h.store.fail("PutClaim", nil)
		wantRetrySetup(t, u, "path 3 opening write failure")

		checkStore := &unansweredCheckLaunchStore{memStore: r.h.store, failing: true}
		u.deps.Store = checkStore
		r.advance(testProbe)
		wantRetrySetup(t, u, "path 3 refused retry")
		checkStore.failing = false

		wantEnteredAtTimer(t, r, u, "path 3")
	})

	t.Run("path_2_after_pod_grant", func(t *testing.T) {
		r := newBoundRig(t, 1, boundRigOptions{withPods: true})
		u := r.machines[0]
		peer := r.addPodRole(u, claim.RoleArchitect)

		r.postAdmitted(u)
		wantWaitingKind(t, u, WakePluginRefresh, "after admission")
		fillRest(t, r)
		queueAhead(r.h, queueDepth)

		peerToken := peer.View().Token
		r.h.mustPostTo(peer, RequestResume{Claim: peerToken, By: SuspendedByOperator})
		r.h.wantStarts(peerToken, 1)

		r.advanceToTurn(u)
		ws := readWake(u)
		if !ws.held || !ws.parked {
			t.Fatalf("setup: after u's turn its wake = %+v, want a try parked at the pod gate", ws)
		}

		r.registerPeer(peer)
		r.h.store.fail("HoldSessions", errBoom)
		r.advance(podLoadSettle)
		r.h.store.fail("HoldSessions", nil)
		wantRetrySetup(t, u, "path 2 after pod grant")

		wantEnteredAtTimer(t, r, u, "path 2 after pod grant")
	})

	t.Run("path_4_resume_before_the_pause_stop", func(t *testing.T) {
		r := newBoundRig(t, 1, boundRigOptions{withPause: true})
		u := r.machines[0]
		queueAhead(r.h, queueDepth)

		heldCredit := admit(t, r.gate)
		fillRest(t, r)

		r.pause.set(7)
		r.h.mustPostTo(u, StalePlugin{Claim: u.View().Token, Trigger: "sweep", Credit: heldCredit})
		ws := readWake(u)
		if !ws.held || ws.kind != WakePluginRefresh || ws.pause != 7 {
			t.Fatalf("setup: u's wake = %+v, want a WakePluginRefresh held for pause 7", ws)
		}

		r.pause.set(0)
		r.h.mustPostTo(u, ResumePause{Claim: u.View().Token, Pause: 7})
		wantWaitingKind(t, u, WakePluginRefresh, "path 4 right after ResumePause")
	})
}

// queueSpawns queues n fresh spawns on distinct tokens and issues (LEGION-9001..) so that a
// subtest needing a fresh pacing queue more than once never collides with waveToken(0..n-1) or
// with any stale claim's issue pod.
func (r *boundRig) queueSpawns(n int) {
	r.t.Helper()
	for range n {
		r.spawned++
		issue := fmt.Sprintf("LEGION-%d", 9000+r.spawned)
		token := claim.Token(fmt.Sprintf("bound-queue-%d", r.spawned))
		c := wakeClaim(token, StateQueued)
		c.Issue = issue
		r.h.mustPostTo(r.h.machineFor(c), RequestSpawn{Claim: token})
	}
}

// registerPeer registers peer on its current generation and the daemon's plugin contract, so its
// issue-pod load settles after podLoadSettle.
func (r *boundRig) registerPeer(peer *Machine) {
	r.t.Helper()
	peerToken := peer.View().Token
	r.h.mustPostTo(peer, RequestRegister{
		Claim:          peerToken,
		Generation:     peer.View().Generation,
		Session:        "ses_" + string(peerToken),
		SessionFile:    "/state/sessions/" + string(peerToken) + ".jsonl",
		CapabilityHash: []byte("capability"),
		PluginContract: r.h.deps.PluginContract,
	})
}

// wantEveryWaitingRefreshCredited checks that every machine whose published view shows a waiting
// WakePluginRefresh wake holds a live credit in r.gate.out, failing with the claim, its credit
// status and the bound.
func (r *boundRig) wantEveryWaitingRefreshCredited(step string) {
	r.t.Helper()
	if ok, tok, why := r.everyWaitingRefreshHoldsACredit(); !ok {
		r.t.Fatalf("%s: claim %s %s for its waiting plugin-refresh wake (%d credits out, bound %d)",
			step, tok, why, r.creditsOut(), wakeRate)
	}
}

// wantAllCreditsReturned checks that every credit of the gate is free, no stale machine holds a
// refreshCredit, and no creditTakenBackMessage was logged (so the credit came back on its own
// rather than through the correction's backstop).
func (r *boundRig) wantAllCreditsReturned(step string) {
	r.t.Helper()
	if got := freeCredits(r.gate); got != wakeRate {
		r.t.Fatalf("%s: freeCredits(gate) = %d, want %d (bound %d)", step, got, wakeRate, wakeRate)
	}
	for _, m := range r.machines {
		m.mu.Lock()
		credit := m.refreshCredit
		m.mu.Unlock()
		if credit != nil {
			r.t.Fatalf("%s: claim %s still holds a non-nil refreshCredit, want nil", step, m.View().Token)
		}
	}
	if got := len(r.h.records.of(creditTakenBackMessage)); got != 0 {
		r.t.Fatalf("%s: %d creditTakenBackMessage records written, want 0", step, got)
	}
}

// wantCreditHeld checks that m holds a non-nil m.refreshCredit that is still a key of r.gate.out,
// that freeCredits(r.gate) == wakeRate-1 and r.creditsOut() == 1, and that no
// creditTakenBackMessage was logged.
func (r *boundRig) wantCreditHeld(m *Machine, step string) {
	r.t.Helper()
	if got := freeCredits(r.gate); got != wakeRate-1 {
		r.t.Fatalf("%s: freeCredits(gate) = %d, want %d (bound %d)", step, got, wakeRate-1, wakeRate)
	}
	if got := r.creditsOut(); got != 1 {
		r.t.Fatalf("%s: %d refresh credits are out, want 1 (bound %d)", step, got, wakeRate)
	}
	m.mu.Lock()
	credit := m.refreshCredit
	m.mu.Unlock()
	if credit == nil {
		r.t.Fatalf("%s: claim %s holds nil refreshCredit, want its held credit (bound %d)",
			step, m.View().Token, wakeRate)
	}
	r.gate.mu.Lock()
	_, out := r.gate.out[credit]
	r.gate.mu.Unlock()
	if !out {
		r.t.Fatalf("%s: claim %s holds a refreshCredit no longer in gate.out (bound %d)",
			step, m.View().Token, wakeRate)
	}
	if got := len(r.h.records.of(creditTakenBackMessage)); got != 0 {
		r.t.Fatalf("%s: %d creditTakenBackMessage records written, want 0", step, got)
	}
}

// pauseOnLaunchWriteStore wraps a *memStore and raises a project pause once right as token's
// StateLaunching claim write lands, placing the pause between the wake gate and the try's own
// pause check (pauseHeldTry).
type pauseOnLaunchWriteStore struct {
	*memStore
	token   claim.Token
	pause   *pauseFlag
	pauseID int64
	armed   bool
}

func (s *pauseOnLaunchWriteStore) PutClaim(ctx context.Context, c Claim) error {
	if err := s.memStore.PutClaim(ctx, c); err != nil {
		return err
	}
	if s.armed && c.Token == s.token && c.State == StateLaunching {
		s.armed = false
		s.pause.set(s.pauseID)
	}
	return nil
}

// Every road by which a WakePluginRefresh wake enters the daemon's wake pacing (rows A1 to A14
// of dispatch://INFRA-4110/artifact/plan-md, the inventory for dispatch://INFRA-4110/spec#b-r1)
// holds a live credit in the refresh gate while that wake waits. Each row drives its exact road
// on one stale claim, verifies the setup at every step, and reads everyWaitingRefreshHoldsACredit
// once the road's wake parks in the pacing.
//
// The negative control is main at 8868205a18f3: every row reaches its road's parked wake and
// fails on the credit check because main returned the credit before the road parked the wake.
func TestRefreshBoundEveryRoadIntoThePacingHoldsACredit(t *testing.T) {
	const queueDepth = 12

	t.Run("A01_path_1_at_turn_of_lane_wake", func(t *testing.T) {
		r := newBoundRig(t, 1, boundRigOptions{})
		u := r.machines[0]

		r.postAdmitted(u)
		wantWaitingKind(t, u, WakePluginRefresh, "A01 initial park")
		r.wantEveryWaitingRefreshCredited("road A1 after admission")
		queueAhead(r.h, queueDepth)

		r.h.store.fail("PutClaim", errBoom)
		r.advanceToTurn(u)
		r.h.store.fail("PutClaim", nil)
		wantRetrySetup(t, u, "A01 path 1 failed write at turn")
		r.wantEveryWaitingRefreshCredited("road A1 after retry armed")

		r.advance(testProbe)
		wantWaitingKind(t, u, WakePluginRefresh, "A01 retry entered pacing")
		r.wantEveryWaitingRefreshCredited("road A1")
	})

	t.Run("A02_path_1_at_immediate_let_out", func(t *testing.T) {
		r := newBoundRig(t, 1, boundRigOptions{})
		u := r.machines[0]

		// Refill the pacer's token so StalePlugin is let out at once without parking first.
		r.advance(wakeInterval + wakePart)
		r.h.store.fail("PutClaim", errBoom)
		c := admit(t, r.gate)
		if err := r.h.postTo(u, StalePlugin{Claim: u.View().Token, Trigger: "sweep", Credit: c}); err == nil {
			t.Fatal("setup: immediate StalePlugin with failing PutClaim succeeded, want error")
		}
		r.h.store.fail("PutClaim", nil)
		r.h.wantWakeLine(u.View().Token, "kind=plugin-refresh", "waited=false", "paced=true")
		wantRetrySetup(t, u, "A02 failed write at immediate let-out")
		r.wantEveryWaitingRefreshCredited("road A2 after retry armed")

		queueAhead(r.h, queueDepth)
		r.advance(testProbe)
		wantWaitingKind(t, u, WakePluginRefresh, "A02 retry entered pacing")
		r.wantEveryWaitingRefreshCredited("road A2")
	})

	t.Run("A03_path_2_at_turn_of_lane_wake", func(t *testing.T) {
		r := newBoundRig(t, 1, boundRigOptions{})
		u := r.machines[0]

		r.postAdmitted(u)
		wantWaitingKind(t, u, WakePluginRefresh, "A03 initial park")
		r.wantEveryWaitingRefreshCredited("road A3 after admission")
		queueAhead(r.h, queueDepth)

		r.h.store.fail("HoldSessions", errBoom)
		r.advanceToTurn(u)
		r.h.store.fail("HoldSessions", nil)
		wantRetrySetup(t, u, "A03 path 2 unanswered HoldSessions")
		r.wantEveryWaitingRefreshCredited("road A3 after retry armed")

		r.advance(testProbe)
		wantWaitingKind(t, u, WakePluginRefresh, "A03 retry entered pacing")
		r.wantEveryWaitingRefreshCredited("road A3")
	})

	t.Run("A04_path_3_at_turn_of_retrys_wake", func(t *testing.T) {
		r := newBoundRig(t, 1, boundRigOptions{})
		u := r.machines[0]

		r.postAdmitted(u)
		wantWaitingKind(t, u, WakePluginRefresh, "A04 initial park")
		r.wantEveryWaitingRefreshCredited("road A4 after admission")

		// Path 1 arms the opening retry so the claim becomes processless.
		r.h.store.fail("PutClaim", errBoom)
		r.advanceToTurn(u)
		r.h.store.fail("PutClaim", nil)
		wantRetrySetup(t, u, "A04 opening failed write")

		// Park the opening retry's wake over the processless claim (credit is checked after
		// A04's own refused turn parks the second retry, so main at 8868205a18f3 exercises
		// runWake too).
		r.queueSpawns(queueDepth)
		r.advance(testProbe)
		wantWaitingKind(t, u, WakePluginRefresh, "A04 opening retry parked over no process")
		if loc := u.View().Locator; loc != nil {
			t.Fatalf("setup: A04 opening retry parked over process %+v, want none", loc)
		}

		// At the opening retry wake's turn, CheckLaunch fails inside runWake, arming a second retry.
		checkStore := &unansweredCheckLaunchStore{memStore: r.h.store, failing: true}
		u.deps.Store = checkStore
		r.advanceToTurn(u)
		wantRetrySetup(t, u, "A04 refused CheckLaunch at turn of retry's wake")
		r.h.wantStarts(u.View().Token, 0)

		checkStore.failing = false
		r.queueSpawns(queueDepth)
		r.advance(testProbe)
		wantWaitingKind(t, u, WakePluginRefresh, "A04 second retry entered pacing")
		r.wantEveryWaitingRefreshCredited("road A4")
	})

	t.Run("A05_path_3_at_retrys_own_lifecycle_check", func(t *testing.T) {
		r := newBoundRig(t, 1, boundRigOptions{})
		u := r.machines[0]

		r.postAdmitted(u)
		wantWaitingKind(t, u, WakePluginRefresh, "A05 initial park")
		r.wantEveryWaitingRefreshCredited("road A5 after admission")
		queueAhead(r.h, queueDepth)

		r.h.store.fail("PutClaim", errBoom)
		r.advanceToTurn(u)
		r.h.store.fail("PutClaim", nil)
		wantRetrySetup(t, u, "A05 opening failed write")

		// At the first retry's timer, launchAfter's own CheckLaunch fails and arms a second retry.
		checkStore := &unansweredCheckLaunchStore{memStore: r.h.store, failing: true}
		u.deps.Store = checkStore
		r.advance(testProbe)
		wantRetrySetup(t, u, "A05 refused CheckLaunch at retry's launchAfter")
		r.h.wantStarts(u.View().Token, 0)

		checkStore.failing = false
		r.advance(testProbe)
		wantWaitingKind(t, u, WakePluginRefresh, "A05 second retry entered pacing")
		r.wantEveryWaitingRefreshCredited("road A5")
	})

	t.Run("A06_path_2_after_pod_start_gate_grant", func(t *testing.T) {
		r := newBoundRig(t, 1, boundRigOptions{withPods: true})
		u := r.machines[0]
		peer := r.addPodRole(u, claim.RoleArchitect)

		r.postAdmitted(u)
		wantWaitingKind(t, u, WakePluginRefresh, "A06 initial park")
		r.wantEveryWaitingRefreshCredited("road A6 after admission")
		queueAhead(r.h, queueDepth)

		peerToken := peer.View().Token
		r.h.mustPostTo(peer, RequestResume{Claim: peerToken, By: SuspendedByOperator})
		r.h.wantStarts(peerToken, 1)

		r.advanceToTurn(u)
		ws := readWake(u)
		if !ws.held || !ws.parked {
			t.Fatalf("setup: A06 wake = %+v, want a try parked at the pod start gate", ws)
		}
		r.h.wantStarts(u.View().Token, 0)
		r.wantEveryWaitingRefreshCredited("road A6 while parked at pod gate")

		r.registerPeer(peer)
		r.h.store.fail("HoldSessions", errBoom)
		r.advance(podLoadSettle)
		r.h.store.fail("HoldSessions", nil)
		wantRetrySetup(t, u, "A06 path 2 after pod grant")

		r.advance(testProbe)
		wantWaitingKind(t, u, WakePluginRefresh, "A06 retry entered pacing")
		r.wantEveryWaitingRefreshCredited("road A6")
	})

	t.Run("A07_path_3_after_pod_grant_meeting_pause", func(t *testing.T) {
		r := newBoundRig(t, 1, boundRigOptions{withPods: true, withPause: true})
		u := r.machines[0]
		peer := r.addPodRole(u, claim.RoleArchitect)

		r.postAdmitted(u)
		wantWaitingKind(t, u, WakePluginRefresh, "A07 initial park")
		r.wantEveryWaitingRefreshCredited("road A7 after admission")

		peerToken := peer.View().Token
		r.h.mustPostTo(peer, RequestResume{Claim: peerToken, By: SuspendedByOperator})
		r.h.wantStarts(peerToken, 1)

		r.advanceToTurn(u)
		ws := readWake(u)
		if !ws.held || !ws.parked {
			t.Fatalf("setup: A07 wake = %+v, want a try parked at the pod start gate", ws)
		}
		r.h.wantStarts(u.View().Token, 0)

		// Register peer, open pause before the pod grant settles, and advance podLoadSettle so
		// podStartDue hands the try to pauseGate (TimerPause armed at 0, not fired yet because
		// Pods.post runs after Advance returns).
		r.registerPeer(peer)
		r.pause.set(7)
		r.advance(podLoadSettle)
		ws = readWake(u)
		if !ws.held || ws.pause != 7 || ws.parked {
			t.Fatalf("setup: A07 wake after pod grant in pause = %+v, want held by pauseGate for pause 7", ws)
		}
		if !timerArmed(u, TimerPause) {
			t.Fatal("setup: A07 podStartDue did not arm TimerPause")
		}
		r.h.wantStarts(u.View().Token, 0)

		// Pause ends and ResumePause runs the paid let-out; CheckLaunch refuses it, arming retry.
		r.pause.set(0)
		checkStore := &unansweredCheckLaunchStore{memStore: r.h.store, failing: true}
		u.deps.Store = checkStore
		if err := r.h.postTo(u, ResumePause{Claim: u.View().Token, Pause: 7}); err == nil {
			t.Fatal("setup: A07 ResumePause with failing CheckLaunch succeeded, want error")
		}
		wantRetrySetup(t, u, "A07 refused CheckLaunch at let-out after pod grant met pause")
		r.h.wantStarts(u.View().Token, 0)

		checkStore.failing = false
		r.queueSpawns(queueDepth)
		r.advance(testProbe)
		wantWaitingKind(t, u, WakePluginRefresh, "A07 retry entered pacing")
		r.wantEveryWaitingRefreshCredited("road A7")
	})

	t.Run("A08_path_3_after_pause_held_try", func(t *testing.T) {
		r := newBoundRig(t, 1, boundRigOptions{withPause: true})
		u := r.machines[0]

		// Refill the pacer's token so StalePlugin passes the wake gate at once in a direct Handle
		// call (leaving TimerPause armed at 0 unfired once pauseHeldTry hands the try to pauseGate).
		r.advance(wakeInterval + wakePart)
		hook := &pauseOnLaunchWriteStore{
			memStore: r.h.store,
			token:    u.View().Token,
			pause:    r.pause,
			pauseID:  7,
			armed:    true,
		}
		u.deps.Store = hook

		r.postAdmitted(u)
		if hook.armed {
			t.Fatal("setup: A08 PutClaim hook did not fire on u's StateLaunching write")
		}
		ws := readWake(u)
		if !ws.held || ws.pause != 7 || ws.parked {
			t.Fatalf("setup: A08 wake after pauseHeldTry = %+v, want held by pauseGate for pause 7", ws)
		}
		if !timerArmed(u, TimerPause) {
			t.Fatal("setup: A08 pauseHeldTry did not arm TimerPause")
		}
		r.h.wantStarts(u.View().Token, 0)

		// End the pause and refuse the let-out's lifecycle recheck so path 3 arms a retry.
		r.pause.set(0)
		checkStore := &unansweredCheckLaunchStore{memStore: r.h.store, failing: true}
		u.deps.Store = checkStore
		if err := r.h.postTo(u, ResumePause{Claim: u.View().Token, Pause: 7}); err == nil {
			t.Fatal("setup: A08 ResumePause with failing CheckLaunch succeeded, want error")
		}
		wantRetrySetup(t, u, "A08 refused CheckLaunch after pauseHeldTry")
		r.h.wantStarts(u.View().Token, 0)

		checkStore.failing = false
		r.queueSpawns(queueDepth)
		r.advance(testProbe)
		wantWaitingKind(t, u, WakePluginRefresh, "A08 retry entered pacing")
		r.wantEveryWaitingRefreshCredited("road A8")
	})

	t.Run("A09_path_1_after_wake_now_pause_road", func(t *testing.T) {
		r := newBoundRig(t, 1, boundRigOptions{withPause: true})
		u := r.machines[0]

		r.postAdmitted(u)
		waiting := wantWaitingKind(t, u, WakePluginRefresh, "A09 initial park")
		r.wantEveryWaitingRefreshCredited("road A9 after admission")

		// Pause before u's turn and fail Suspend so that the immediate TimerPause fired at the
		// same moment leaves the wake held in pauseGate until ResumePause runs it.
		r.pause.set(7)
		r.h.rt.FailSuspend(errBoom)
		r.advance(waiting.At.Sub(r.h.clock.Now()))
		r.h.rt.FailSuspend(nil)

		ws := readWake(u)
		if !ws.held || ws.kind != WakePluginRefresh || ws.pause != 7 {
			t.Fatalf("setup: A09 wake after wakeNow met pause = %+v, want WakePluginRefresh held for pause 7", ws)
		}
		if w := u.View().WakeWaiting; w != nil {
			t.Fatalf("setup: A09 view shows %+v while held by pauseGate, want none", w)
		}
		r.h.wantStarts(u.View().Token, 0)

		// End the pause, fail PutClaim, and let the paid wake out via ResumePause so path 1 arms a retry.
		r.pause.set(0)
		r.h.store.fail("PutClaim", errBoom)
		if err := r.h.postTo(u, ResumePause{Claim: u.View().Token, Pause: 7}); err == nil {
			t.Fatal("setup: A09 ResumePause with failing PutClaim succeeded, want error")
		}
		r.h.store.fail("PutClaim", nil)
		wantRetrySetup(t, u, "A09 failed write at paid let-out after wakeNow pause road")

		r.queueSpawns(queueDepth)
		r.advance(testProbe)
		wantWaitingKind(t, u, WakePluginRefresh, "A09 retry entered pacing")
		r.wantEveryWaitingRefreshCredited("road A9")
	})

	t.Run("A10_path_4_resume_before_pause_stop", func(t *testing.T) {
		r := newBoundRig(t, 1, boundRigOptions{withPause: true})
		u := r.machines[0]

		queueAhead(r.h, queueDepth)
		r.pause.set(7)
		r.postAdmitted(u)
		ws := readWake(u)
		if !ws.held || ws.kind != WakePluginRefresh || ws.pause != 7 {
			t.Fatalf("setup: A10 wake = %+v, want WakePluginRefresh held for pause 7", ws)
		}
		if !timerArmed(u, TimerPause) {
			t.Fatal("setup: A10 pauseGate did not arm TimerPause")
		}
		r.wantEveryWaitingRefreshCredited("road A10 while held by pauseGate")

		r.pause.set(0)
		r.h.mustPostTo(u, ResumePause{Claim: u.View().Token, Pause: 7})
		wantWaitingKind(t, u, WakePluginRefresh, "A10 ResumePause let wake into pacing")
		r.wantEveryWaitingRefreshCredited("road A10")
	})

	t.Run("A11_path_4_after_failed_pause_stop", func(t *testing.T) {
		r := newBoundRig(t, 1, boundRigOptions{withPause: true})
		u := r.machines[0]

		queueAhead(r.h, queueDepth)
		r.pause.set(7)
		r.postAdmitted(u)
		ws := readWake(u)
		if !ws.held || ws.kind != WakePluginRefresh || ws.pause != 7 {
			t.Fatalf("setup: A11 wake = %+v, want WakePluginRefresh held for pause 7", ws)
		}

		// Refuse Suspend at the immediate pauseStopDue so the wake stays held and TimerPause is
		// armed again at testProbe.
		r.h.rt.FailSuspend(errBoom)
		r.advance(0)
		ws = readWake(u)
		if !ws.held || ws.kind != WakePluginRefresh || ws.pause != 7 || !timerArmed(u, TimerPause) {
			t.Fatalf("setup: A11 after failed pause stop wake = %+v (TimerPause=%v), want held for pause 7 with TimerPause re-armed",
				ws, timerArmed(u, TimerPause))
		}
		if u.View().State != StateIdle {
			t.Fatalf("setup: A11 claim state = %s after failed pause stop, want %s", u.View().State, StateIdle)
		}

		// Pause ends and runtime accepts stops again; pauseStopDue finds no pause and lets the wake out.
		r.h.rt.FailSuspend(nil)
		r.pause.set(0)
		r.advance(testProbe)
		if lines := r.h.logs.lines("the pause the gate held this claim's launch for has ended; the launch goes to the wake pacing",
			"claim="+string(u.View().Token)); len(lines) != 1 {
			t.Fatalf("setup: A11 pauseStopDue let-out lines = %v, want 1", lines)
		}
		wantWaitingKind(t, u, WakePluginRefresh, "A11 pauseStopDue let wake into pacing")
		r.wantEveryWaitingRefreshCredited("road A11")
	})

	t.Run("A12_path_4_after_repark_wake", func(t *testing.T) {
		r := newBoundRig(t, 1, boundRigOptions{withPause: true})
		u := r.machines[0]

		r.postAdmitted(u)
		wantWaitingKind(t, u, WakePluginRefresh, "A12 initial park")
		r.wantEveryWaitingRefreshCredited("road A12 after admission")

		// Drive u into a processless path 1 retry so that when the retry fires while paused,
		// pauseGate holds a WakePluginRefresh wake over a launching claim with Locator == nil
		// (the condition dropWakeFor requires) and paid == false (so ResumePause joins pacing).
		r.h.store.fail("PutClaim", errBoom)
		r.advanceToTurn(u)
		r.h.store.fail("PutClaim", nil)
		wantRetrySetup(t, u, "A12 path 1 retry armed")

		// Fire TimerRelaunch directly while paused so pauseGate arms TimerPause at 0 without the
		// fake clock immediately firing TimerPause inside Advance.
		u.mu.Lock()
		relaunchSeq := u.timers[TimerRelaunch].seq
		u.mu.Unlock()
		r.pause.set(7)
		r.h.mustPostTo(u, Timer{Claim: u.View().Token, Kind: TimerRelaunch, Generation: u.View().Generation, Seq: relaunchSeq})

		ws := readWake(u)
		if !ws.held || ws.kind != WakePluginRefresh || ws.pause != 7 || ws.parked {
			t.Fatalf("setup: A12 wake after retry met pause = %+v, want WakePluginRefresh held by pauseGate for pause 7", ws)
		}
		if loc := u.View().Locator; loc != nil {
			t.Fatalf("setup: A12 claim records process %+v, want none so dropWakeFor acts", loc)
		}

		// RequestForgetSession drops the processless held wake (dropWakeFor), is ignored on
		// StateLaunching, and reparkWake puts the wake back in pauseGate.
		const reparkLine = "supervise: the request launched nothing; the wake the pacing holds keeps its place"
		if err := r.h.postTo(u, RequestForgetSession{Claim: u.View().Token, Session: u.View().Session, By: SuspendedByOperator}); err == nil {
			t.Fatal("setup: A12 RequestForgetSession over StateLaunching succeeded, want refusal")
		}
		if got := len(r.h.records.of(reparkLine)); got != 1 {
			t.Fatalf("setup: A12 reparkWake logged %d times, want 1", got)
		}
		ws = readWake(u)
		if !ws.held || ws.kind != WakePluginRefresh || ws.pause != 7 {
			t.Fatalf("setup: A12 wake after reparkWake = %+v, want WakePluginRefresh held for pause 7", ws)
		}

		// End the pause, queue spawns so the let-out waits, and send ResumePause.
		r.pause.set(0)
		r.queueSpawns(queueDepth)
		r.h.mustPostTo(u, ResumePause{Claim: u.View().Token, Pause: 7})
		wantWaitingKind(t, u, WakePluginRefresh, "A12 let-out after reparkWake entered pacing")
		r.wantEveryWaitingRefreshCredited("road A12")
	})

	t.Run("A13_path_1_at_run_parked_launch_now", func(t *testing.T) {
		r := newBoundRig(t, 1, boundRigOptions{})
		u := r.machines[0]

		r.postAdmitted(u)
		wantWaitingKind(t, u, WakePluginRefresh, "A13 initial park")
		r.wantEveryWaitingRefreshCredited("road A13 after admission")
		queueAhead(r.h, queueDepth)

		// An operator's delivery (empty ID) runs the parked refresh launch immediately via
		// runParkedLaunchNow, and its StateLaunching write fails, arming path 1's retry.
		r.h.store.fail("PutClaim", errBoom)
		if err := r.h.postTo(u, RequestDeliver{Claim: u.View().Token, Task: "operator task"}); err == nil {
			t.Fatal("setup: A13 operator RequestDeliver with failing PutClaim succeeded, want error")
		}
		r.h.store.fail("PutClaim", nil)
		if lines := r.h.logs.lines("an operator's task reached a claim whose paced launch waits over the process it replaces",
			"claim="+string(u.View().Token)); len(lines) != 1 {
			t.Fatalf("setup: A13 runParkedLaunchNow lines = %v, want 1", lines)
		}
		wantRetrySetup(t, u, "A13 path 1 retry after runParkedLaunchNow")

		r.advance(testProbe)
		wantWaitingKind(t, u, WakePluginRefresh, "A13 retry entered pacing")
		r.wantEveryWaitingRefreshCredited("road A13")
	})

	t.Run("A14_path_3_at_run_dropped_launch_for_retry", func(t *testing.T) {
		r := newBoundRig(t, 1, boundRigOptions{})
		u := r.machines[0]

		r.postAdmitted(u)
		wantWaitingKind(t, u, WakePluginRefresh, "A14 initial park")
		r.wantEveryWaitingRefreshCredited("road A14 after admission")

		// Arm path 1's retry and let it park a WakePluginRefresh wake over a processless claim.
		r.h.store.fail("PutClaim", errBoom)
		r.advanceToTurn(u)
		r.h.store.fail("PutClaim", nil)
		wantRetrySetup(t, u, "A14 opening failed write")

		r.queueSpawns(queueDepth)
		r.advance(testProbe)
		wantWaitingKind(t, u, WakePluginRefresh, "A14 opening retry parked over no process")
		if loc := u.View().Locator; loc != nil {
			t.Fatalf("setup: A14 opening retry parked over process %+v, want none", loc)
		}

		// An operator's retry drops the processless wake and runs it now (runDroppedLaunchForRetry
		// -> runWake), where CheckLaunch fails and arms a second retry.
		checkStore := &unansweredCheckLaunchStore{memStore: r.h.store, failing: true}
		u.deps.Store = checkStore
		if err := r.h.postTo(u, RequestAskedRetry{Claim: u.View().Token, By: SuspendedByOperator}); err == nil {
			t.Fatal("setup: A14 operator RequestAskedRetry with failing CheckLaunch succeeded, want error")
		}
		const droppedRetryLine = "supervise: an operator's retry reached a claim whose paced launch waits over no process; that launch runs now"
		if got := len(r.h.records.of(droppedRetryLine)); got != 1 {
			t.Fatalf("setup: A14 runDroppedLaunchForRetry logged %d times, want 1", got)
		}
		wantRetrySetup(t, u, "A14 refused CheckLaunch at runDroppedLaunchForRetry")

		checkStore.failing = false
		r.queueSpawns(queueDepth)
		r.advance(testProbe)
		wantWaitingKind(t, u, WakePluginRefresh, "A14 second retry entered pacing")
		r.wantEveryWaitingRefreshCredited("road A14")
	})
}

// Every path that ends or drops an owed launch of the stale-plugin lane (rows G1 to G9 of
// dispatch://INFRA-4110/artifact/plan-md) gives its held refresh credit back at the end of that
// decision, leaving freeCredits(gate) == wakeRate with zero creditTakenBackMessage records, so
// no path leans on the correction's backstop.
//
// This test passes on main at 8868205a18f3 as well as after the change; after the change it
// catches any leak where a new hold is kept after its wait is dropped.
func TestRefreshBoundEachHeldCreditComesBack(t *testing.T) {
	t.Run("G1_retry_starts_process", func(t *testing.T) {
		r := newBoundRig(t, 1, boundRigOptions{})
		u := r.machines[0]

		r.postAdmitted(u)
		wantWaitingKind(t, u, WakePluginRefresh, "G1 initial park")

		r.h.store.fail("PutClaim", errBoom)
		r.advanceToTurn(u)
		r.h.store.fail("PutClaim", nil)
		wantRetrySetup(t, u, "G1 path 1 retry armed")

		r.advance(testProbe)
		r.h.wantStarts(u.View().Token, 1)
		if loc := u.View().Locator; loc == nil {
			t.Fatal("setup: G1 retry did not record a started process")
		}
		if got := u.View().PluginRelaunchFor; got != refreshContract {
			t.Fatalf("setup: G1 claim marked PluginRelaunchFor = %d, want %d", got, refreshContract)
		}
		r.wantAllCreditsReturned("G1 after retry started process")
	})

	t.Run("G2_retry_timer_meets_held_suspension", func(t *testing.T) {
		r := newBoundRig(t, 1, boundRigOptions{})
		u := r.machines[0]

		r.postAdmitted(u)
		wantWaitingKind(t, u, WakePluginRefresh, "G2 initial park")

		r.h.store.fail("PutClaim", errBoom)
		r.advanceToTurn(u)
		r.h.store.fail("PutClaim", nil)
		wantRetrySetup(t, u, "G2 path 1 retry armed")

		// On a launching claim with no process and no waiting wake, RequestSuspend is held by
		// holdSuspendUntilReady until TimerRelaunch fires and calls suspendedFor.
		err := r.h.postTo(u, RequestSuspend{Claim: u.View().Token, Reason: "operator drain", By: SuspendedByOperator})
		if !suspensionHeld(err) {
			t.Fatalf("setup: G2 RequestSuspend while retry waits answered %v, want ErrSuspendHeld", err)
		}
		if u.View().State != StateLaunching || !timerArmed(u, TimerRelaunch) {
			t.Fatalf("setup: G2 claim = %s (TimerRelaunch=%v), want StateLaunching with TimerRelaunch armed",
				u.View().State, timerArmed(u, TimerRelaunch))
		}

		r.advance(testProbe)
		if c := u.View().Claim; c.State != StateSuspended || c.SuspendedBy != SuspendedByOperator {
			t.Fatalf("setup: G2 claim after TimerRelaunch = %s by %q, want %s by %q",
				c.State, c.SuspendedBy, StateSuspended, SuspendedByOperator)
		}
		if timerArmed(u, TimerRelaunch) {
			t.Fatal("setup: G2 TimerRelaunch is still armed after suspension")
		}
		r.h.wantStarts(u.View().Token, 0)
		r.wantAllCreditsReturned("G2 after retry timer met held suspension")
	})

	t.Run("G3_stop_clears_waiting_retry", func(t *testing.T) {
		r := newBoundRig(t, 1, boundRigOptions{})
		u := r.machines[0]

		r.postAdmitted(u)
		wantWaitingKind(t, u, WakePluginRefresh, "G3 initial park")

		r.h.store.fail("PutClaim", errBoom)
		r.advanceToTurn(u)
		r.h.store.fail("PutClaim", nil)
		wantRetrySetup(t, u, "G3 path 1 retry armed")

		r.h.mustPostTo(u, RequestStop{Claim: u.View().Token, By: SuspendedByOperator})
		if u.View().State != StateRetired {
			t.Fatalf("setup: G3 claim state after RequestStop = %s, want %s", u.View().State, StateRetired)
		}
		if timerArmed(u, TimerRelaunch) {
			t.Fatal("setup: G3 TimerRelaunch is still armed after RequestStop")
		}
		r.h.wantStarts(u.View().Token, 0)
		r.wantAllCreditsReturned("G3 after RequestStop cleared retry")
	})

	t.Run("G4_retry_lifecycle_check_cleanup_reserved", func(t *testing.T) {
		r := newBoundRig(t, 1, boundRigOptions{})
		u := r.machines[0]

		r.postAdmitted(u)
		wantWaitingKind(t, u, WakePluginRefresh, "G4 initial park")

		r.h.store.fail("PutClaim", errBoom)
		r.advanceToTurn(u)
		r.h.store.fail("PutClaim", nil)
		wantRetrySetup(t, u, "G4 path 1 retry armed")

		u.deps.Store = &refusableCheckLaunchStore{memStore: r.h.store, refuse: true}
		r.advance(testProbe)
		if c := u.View().Claim; c.State != StateSuspended || c.SuspendedBy != SuspendedByDaemon {
			t.Fatalf("setup: G4 claim after reserved cleanup = %s by %q, want %s by %q",
				c.State, c.SuspendedBy, StateSuspended, SuspendedByDaemon)
		}
		if timerArmed(u, TimerRelaunch) {
			t.Fatal("setup: G4 TimerRelaunch is still armed after suspension")
		}
		r.h.wantStarts(u.View().Token, 0)
		r.wantAllCreditsReturned("G4 after retry lifecycle check found cleanup reserved")
	})

	t.Run("G5_pause_stop_suspends_held_lane_wake", func(t *testing.T) {
		r := newBoundRig(t, 1, boundRigOptions{withPause: true})
		u := r.machines[0]

		r.pause.set(7)
		r.postAdmitted(u)
		ws := readWake(u)
		if !ws.held || ws.kind != WakePluginRefresh || ws.pause != 7 {
			t.Fatalf("setup: G5 wake = %+v, want WakePluginRefresh held for pause 7", ws)
		}
		if !timerArmed(u, TimerPause) {
			t.Fatal("setup: G5 pauseGate did not arm TimerPause")
		}

		r.advance(0)
		if c := u.View().Claim; c.State != StateSuspended || c.SuspendedBy != SuspendedByReleased {
			t.Fatalf("setup: G5 claim after pauseStopDue = %s by %q, want %s by %q",
				c.State, c.SuspendedBy, StateSuspended, SuspendedByReleased)
		}
		if ws := readWake(u); ws.held {
			t.Fatalf("setup: G5 wake = %+v still held after pause stop, want none", ws)
		}
		r.h.wantStarts(u.View().Token, 0)
		r.wantAllCreditsReturned("G5 after pauseStopDue suspended held lane wake")
	})

	t.Run("G6_suspension_cuts_pod_parked_lane_try", func(t *testing.T) {
		r := newBoundRig(t, 1, boundRigOptions{withPods: true})
		u := r.machines[0]
		peer := r.addPodRole(u, claim.RoleArchitect)

		r.postAdmitted(u)
		wantWaitingKind(t, u, WakePluginRefresh, "G6 initial park")

		peerToken := peer.View().Token
		r.h.mustPostTo(peer, RequestResume{Claim: peerToken, By: SuspendedByOperator})
		r.h.wantStarts(peerToken, 1)

		r.advanceToTurn(u)
		ws := readWake(u)
		if !ws.held || !ws.parked {
			t.Fatalf("setup: G6 wake = %+v, want try parked at pod start gate", ws)
		}
		r.h.wantStarts(u.View().Token, 0)

		r.h.mustPostTo(u, RequestSuspend{Claim: u.View().Token, Reason: "operator drain", By: SuspendedByOperator})
		if c := u.View().Claim; c.State != StateSuspended || c.SuspendedBy != SuspendedByOperator {
			t.Fatalf("setup: G6 claim after suspend = %s by %q, want %s by %q",
				c.State, c.SuspendedBy, StateSuspended, SuspendedByOperator)
		}
		if ws := readWake(u); ws.held {
			t.Fatalf("setup: G6 wake = %+v still held after suspend, want none", ws)
		}
		if got := len(r.h.records.of(podLaunchCutShortLine)); got != 1 {
			t.Fatalf("setup: G6 podLaunchCutShortLine logged %d times, want 1 (endPodPark)", got)
		}
		r.h.wantStarts(u.View().Token, 0)
		r.wantAllCreditsReturned("G6 after suspension cut pod-parked lane try")
	})

	t.Run("G7_session_store_or_pod_round_retry_is_launch_kind", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			err   error
			retry time.Duration
		}{
			{"ErrSessionStore", runtime.ErrSessionStore, sessionStoreRetry},
			{"ErrAwaitingPodRound", runtime.ErrAwaitingPodRound, podRoundRetry},
		} {
			t.Run(tc.name, func(t *testing.T) {
				r := newBoundRig(t, 1, boundRigOptions{})
				u := r.machines[0]

				r.postAdmitted(u)
				wantWaitingKind(t, u, WakePluginRefresh, "G7 "+tc.name+" initial park")

				r.h.rt.OnResume = func(context.Context) error {
					return fmt.Errorf("resume %s: %w", u.View().Token, tc.err)
				}
				r.advanceToTurn(u)
				r.h.rt.OnResume = nil

				wantRetrySetup(t, u, "G7 "+tc.name+" retry armed")
				// The credit must already be back at the end of this decision while TimerRelaunch waits.
				r.wantAllCreditsReturned("G7 " + tc.name + " while launch-kind retry waits")

				// Confirm the retry parks under WakeLaunch rather than WakePluginRefresh.
				r.advance(tc.retry - time.Second)
				r.queueSpawns(4)
				r.advance(time.Second)
				wantWaitingKind(t, u, WakeLaunch, "G7 "+tc.name+" retry waits under WakeLaunch")
				r.wantAllCreditsReturned("G7 " + tc.name + " after launch-kind retry parked")
			})
		}
	})

	t.Run("G8_launch_fails_for_good", func(t *testing.T) {
		t.Run("prompt_refused", func(t *testing.T) {
			r := newBoundRig(t, 1, boundRigOptions{})
			u := r.machines[0]

			r.postAdmitted(u)
			wantWaitingKind(t, u, WakePluginRefresh, "G8 prompt_refused initial park")

			r.h.rt.OnResume = func(context.Context) error {
				return fmt.Errorf("resume %s: %w", u.View().Token,
					&runtime.PromptTooLarge{Role: u.View().Role, Bytes: 1_100_000, Frame: 1_100_400})
			}
			r.advanceToTurn(u)
			r.h.rt.OnResume = nil

			if u.View().State != StateFailed {
				t.Fatalf("setup: G8 prompt_refused state = %s, want %s", u.View().State, StateFailed)
			}
			if timerArmed(u, TimerRelaunch) || readWake(u).held {
				t.Fatal("setup: G8 prompt_refused still holds a retry or wake")
			}
			r.wantAllCreditsReturned("G8 prompt_refused")
		})

		t.Run("budget_spent", func(t *testing.T) {
			r := newBoundRig(t, 1, boundRigOptions{})
			u := r.machines[0]

			r.postAdmitted(u)
			wantWaitingKind(t, u, WakePluginRefresh, "G8 budget_spent initial park")

			r.h.rt.OnResume = func(context.Context) error { return errBoom }
			r.advanceToTurn(u)
			r.h.rt.OnResume = nil

			if u.View().State != StateFailed {
				t.Fatalf("setup: G8 budget_spent state = %s, want %s", u.View().State, StateFailed)
			}
			if timerArmed(u, TimerRelaunch) || readWake(u).held {
				t.Fatal("setup: G8 budget_spent still holds a retry or wake")
			}
			r.wantAllCreditsReturned("G8 budget_spent")
		})
	})

	t.Run("G9_pause_held_try_let_out_starts_process", func(t *testing.T) {
		r := newBoundRig(t, 1, boundRigOptions{withPause: true})
		u := r.machines[0]

		r.advance(wakeInterval + wakePart)
		hook := &pauseOnLaunchWriteStore{
			memStore: r.h.store,
			token:    u.View().Token,
			pause:    r.pause,
			pauseID:  7,
			armed:    true,
		}
		u.deps.Store = hook

		r.postAdmitted(u)
		if hook.armed {
			t.Fatal("setup: G9 PutClaim hook did not fire on u's StateLaunching write")
		}
		ws := readWake(u)
		if !ws.held || ws.pause != 7 || ws.parked {
			t.Fatalf("setup: G9 wake after pauseHeldTry = %+v, want held by pauseGate for pause 7", ws)
		}
		r.h.wantStarts(u.View().Token, 0)

		r.pause.set(0)
		r.h.mustPostTo(u, ResumePause{Claim: u.View().Token, Pause: 7})
		r.h.wantStarts(u.View().Token, 1)
		if loc := u.View().Locator; loc == nil {
			t.Fatal("setup: G9 let-out did not record a started process")
		}
		if lines := r.h.logs.lines("the fact this claim's plugin refresh was raised for is gone"); len(lines) != 0 {
			t.Fatalf("G9 refreshOverturned fired on held lane try: logged %v, want none", lines)
		}
		r.wantAllCreditsReturned("G9 after pauseHeldTry let-out started process")
	})
}

// Two gate corrections (gate.Correct(0), twice) made while an admitted stale-plugin launch waits
// outside the wake pacing — on TimerRelaunch, held by the pause's gate, or parked at its issue
// pod's start gate — keep that claim's credit out and write no creditTakenBackMessage. Once the
// wait ends, the credit returns on its own with zero creditTakenBackMessage records.
//
// The negative control is main at 8868205a18f3: main already gave the credit back when the launch
// left the pacing, so freeCredits(gate) reads wakeRate (8) instead of wakeRate-1 (7).
func TestRefreshBoundACorrectionKeepsTheCreditOfALaunchThatWaitsOutsideThePacing(t *testing.T) {
	checkTwoCorrections := func(t *testing.T, r *boundRig, u *Machine, wait string) {
		t.Helper()
		if w := u.View().WakeWaiting; w != nil {
			t.Fatalf("setup (%s): view shows waiting wake %+v, want none while waiting outside the pacing", wait, w)
		}
		for i := 1; i <= 2; i++ {
			r.gate.Correct(0)
			r.wantCreditHeld(u, fmt.Sprintf("%s after correction %d", wait, i))
		}
	}

	t.Run("retry_on_timer_relaunch", func(t *testing.T) {
		r := newBoundRig(t, 1, boundRigOptions{})
		u := r.machines[0]

		r.postAdmitted(u)
		wantWaitingKind(t, u, WakePluginRefresh, "retry_on_timer_relaunch initial park")

		r.h.store.fail("PutClaim", errBoom)
		r.advanceToTurn(u)
		r.h.store.fail("PutClaim", nil)
		wantRetrySetup(t, u, "retry_on_timer_relaunch")

		checkTwoCorrections(t, r, u, "retry_on_timer_relaunch")

		// End the wait: retry runs and starts the process.
		r.advance(testProbe)
		r.h.wantStarts(u.View().Token, 1)
		r.wantAllCreditsReturned("retry_on_timer_relaunch after retry started process")
	})

	t.Run("wake_held_by_pause_gate", func(t *testing.T) {
		r := newBoundRig(t, 1, boundRigOptions{withPause: true})
		u := r.machines[0]

		r.pause.set(7)
		r.postAdmitted(u)
		ws := readWake(u)
		if !ws.held || ws.kind != WakePluginRefresh || ws.pause != 7 {
			t.Fatalf("setup: wake_held_by_pause_gate wake = %+v, want WakePluginRefresh held for pause 7", ws)
		}

		checkTwoCorrections(t, r, u, "wake_held_by_pause_gate")

		// End the wait: pause ends, ResumePause lets the wake into pacing, and its turn starts the process.
		r.pause.set(0)
		r.h.mustPostTo(u, ResumePause{Claim: u.View().Token, Pause: 7})
		wantWaitingKind(t, u, WakePluginRefresh, "wake_held_by_pause_gate after ResumePause")
		r.advanceToTurn(u)
		r.h.wantStarts(u.View().Token, 1)
		r.wantAllCreditsReturned("wake_held_by_pause_gate after let-out started process")
	})

	t.Run("try_parked_at_pod_start_gate", func(t *testing.T) {
		r := newBoundRig(t, 1, boundRigOptions{withPods: true})
		u := r.machines[0]
		peer := r.addPodRole(u, claim.RoleArchitect)

		r.postAdmitted(u)
		wantWaitingKind(t, u, WakePluginRefresh, "try_parked_at_pod_start_gate initial park")

		peerToken := peer.View().Token
		r.h.mustPostTo(peer, RequestResume{Claim: peerToken, By: SuspendedByOperator})
		r.h.wantStarts(peerToken, 1)

		r.advanceToTurn(u)
		ws := readWake(u)
		if !ws.held || !ws.parked {
			t.Fatalf("setup: try_parked_at_pod_start_gate wake = %+v, want try parked at pod start gate", ws)
		}
		r.h.wantStarts(u.View().Token, 0)

		checkTwoCorrections(t, r, u, "try_parked_at_pod_start_gate")

		// End the wait: peer's load settles and the pod gate grants u's start.
		r.registerPeer(peer)
		r.advance(podLoadSettle)
		r.h.wantStarts(u.View().Token, 1)
		r.wantAllCreditsReturned("try_parked_at_pod_start_gate after pod grant started process")
	})
}
