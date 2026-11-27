package supervise

// The stale-plugin refresh gate's bound: no more than K = wakeRate plugin-refresh wakes wait in
// the daemon's wake pacing at once, across every road that puts such a wake into the pacing
// (RefreshGate, WakePluginRefresh, dispatch://INFRA-4110). A machine admitted to the lane keeps
// its credit for as long as a launch of the lane is owed on it — through a retry after a failed
// write, an unanswered session hold or a refused lifecycle check, through a project pause's gate,
// and through a wait at its issue pod's start gate — and gives it back as soon as that launch
// ends or is dropped.
//
// This file uses only identifiers that exist in internal/supervise on main at 8868205a18f3, so
// that the negative control compiles and runs against main as well as against the change.

import (
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
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
	stops    *stopTransaction
	pods     *PodStarts
	machines []*Machine
}

// boundRigOptions configures optional Deps shared by every machine of a boundRig.
type boundRigOptions struct {
	withPause bool
	withPods  bool
	tune      func(*Deps)
}

// newBoundRig builds a boundRig with n stale idle claims (issues LEGION-301 .. LEGION-(300+n)).
// Each stale claim is created through h.machineFor so its machine starts with no cached session
// holds, which lets a test fail HoldSessions at its first relaunch try (path 2) as a restarted
// daemon's claim does.
func newBoundRig(t *testing.T, n int, opts boundRigOptions) *boundRig {
	t.Helper()
	gate := NewRefreshGate(wakeRate)
	var flag *pauseFlag
	var stops *stopTransaction
	var pods *PodStarts

	var tunes []func(*Deps)
	tunes = append(tunes, func(d *Deps) {
		d.RefreshGate = gate
	})
	if opts.withPause {
		flag = &pauseFlag{}
		tunes = append(tunes, func(d *Deps) {
			stops = &stopTransaction{store: d.Store.(*memStore)}
			d.Paused, d.PauseStopped = flag.Paused, stops.PauseStopped
		})
	}
	if opts.withPods {
		tunes = append(tunes, func(d *Deps) {
			clock := d.Clock.(*fakeClock)
			pods = NewPodStarts(clock)
			pods.post = clock.whenStill
			d.PodStarts = pods
		})
	}
	if opts.tune != nil {
		tunes = append(tunes, opts.tune)
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
		stops:    stops,
		pods:     pods,
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
// wait in the pacing, and no more than wakeRate credits are out. Each failure names the reading
// and the bound.
func (r *boundRig) wantWithinRate(step string) {
	r.t.Helper()
	if got := r.waitingRefreshes(); got > wakeRate {
		r.t.Fatalf("%s: %d plugin-refresh wakes wait, want at most %d", step, got, wakeRate)
	}
	if got := r.creditsOut(); got > wakeRate {
		r.t.Fatalf("%s: %d refresh credits are out, want at most %d", step, got, wakeRate)
	}
}

// wakeSnapshot is the fields of m.wake read under m.mu that are safe across T2.
type wakeSnapshot struct {
	held   bool
	kind   WakeKind
	pause  int64
	parked bool
	paced  bool
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
		paced:  m.wake.place != nil,
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
// waiting wakes and K credits out, even when K+1 stale idle claims compete for the gate and a
// sweep runs right after the step that used to drop the credit. Each subtest drives one road on
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

		// Sweep after the step where main returns u's credit.
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

		// Sweep after the step where main returns u's credit.
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

		// Sweep after the opening failure (where main returns u's credit).
		r.sweep()
		r.wantWithinRate("after sweep before refused retry")

		// Path 3 (row A5): when the first retry's timer fires, u's store cannot answer
		// CheckLaunch, so launchAfter refuses it and relaunchRefused arms a second retry.
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

		heldCredit := r.gate.Admit()
		if heldCredit == nil {
			t.Fatal("setup: the empty gate refused the Kth credit")
		}
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
		// TimerPause has not fired). At main, u's credit went back at pauseGate's decision, so
		// this sweep admits machines[wakeRate-1].
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

		// Sweep after the pod park (where main has already returned u's credit).
		r.sweep()
		r.wantWithinRate("after sweep while u waits at the pod start gate")

		// End the peer's session load (register, then settle) while HoldSessions fails, so u's
		// granted start hits errHoldUnanswered and arms path 2's retry.
		peerGen := peer.View().Generation
		r.h.mustPostTo(peer, RequestRegister{
			Claim:          peerToken,
			Generation:     peerGen,
			Session:        "ses_" + string(peerToken),
			SessionFile:    "/state/sessions/" + string(peerToken) + ".jsonl",
			CapabilityHash: []byte("capability"),
			PluginContract: r.h.deps.PluginContract,
		})
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
// plugin-refresh wakes above K = wakeRate (spec Problem). Three claims R1..R3 take path 1 and
// re-enter the pacing behind a fourth admitted claim T; while T's turn is held inside Resume,
// the views show only R1..R3 (3 wakes) and no wake of T, and gate.Correct(3) runs. When T's
// turn ends, admitting until nil and posting to stale idle claims must never leave more than K
// plugin-refresh wakes waiting.
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
// while the refresh gate is full (R2). Neither waits for a free credit or a later sweep.
//
// This test passes at main as well as after the change, and fails for the refused design that
// makes a retry or let-out ask the gate and wait for a free credit.
func TestRefreshBoundARetryEntersThePacingAtItsTimer(t *testing.T) {
	const queueDepth = 12

	// fillRest takes the other K-1 credits from r.gate so the gate has no free credit while the
	// claim under test holds the Kth.
	fillRest := func(t *testing.T, r *boundRig) {
		t.Helper()
		for range wakeRate - 1 {
			if c := r.gate.Admit(); c == nil {
				t.Fatalf("setup: gate had fewer than %d remaining credits to fill", wakeRate-1)
			}
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

		peerGen := peer.View().Generation
		r.h.mustPostTo(peer, RequestRegister{
			Claim:          peerToken,
			Generation:     peerGen,
			Session:        "ses_" + string(peerToken),
			SessionFile:    "/state/sessions/" + string(peerToken) + ".jsonl",
			CapabilityHash: []byte("capability"),
			PluginContract: r.h.deps.PluginContract,
		})
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

		heldCredit := r.gate.Admit()
		if heldCredit == nil {
			t.Fatal("setup: the empty gate refused u's credit")
		}
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
