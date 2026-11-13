package supervise

// The stale-plugin refresh gate as a machine meets it: one credit for one relaunch, taken before
// the relaunch was decided and given back at the end of the decision that holds it unless a
// refresh wake of this claim's is waiting (RefreshGate, admitRefresh, endRefreshLane,
// dispatch://INFRA-2712 R1). The gate itself is counted in gate_test.go; what these tests hold
// is the machine's half of it — that every path out of refreshPlugin which raises no relaunch
// returns its credit at once, that a relaunch the pacing holds keeps its credit for exactly as
// long as it waits, that a claim whose turn ends with a relaunch held asks the gate for one of
// its own, and that each door a relaunch can leave by with no wake behind it returns the credit
// there (the two take-backs a store can refuse, at the end of this file).
//
// Beside the credit runs the re-read the admitted wake makes when its turn comes (R2): a refresh
// can wait tens of minutes in a wave, and a claim that has registered on this daemon's contract
// meanwhile, or started a turn, is relaunched by that wake not at all (refreshOverturned,
// runWake). Those two tests are here rather than in plugin_refresh_wake_test.go because what the
// aborted wake owes is the credit, and the proof that it is given back is the same assertion.
//
// The gate here is a fake that records what was asked of it (fakeAdmitter), so a test reads the
// asks themselves rather than inferring them from a rate; the daemon's own pass over a fleet, and
// the bound a real gate holds it to, are internal/daemon's (wake_wave_test.go).

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// refreshAsks is everything one gate was asked for, as the fake counted it: how many admissions
// were asked of it, how many it granted, and how many credits came back. A test states all three
// at once, because the question a credit path raises is always the pair — was one taken, and did
// it come back.
type refreshAsks struct {
	asked    int
	granted  int
	released int
}

// fakeAdmitter is the gate a test hands a machine: it answers admissions with what the test set
// and counts every call. It is the surface Deps.RefreshGate names and nothing more
// (RefreshAdmitter), which is why a fake can stand where the daemon's one real gate stands.
//
// Every method takes the lock, as the real gate's do: a machine's decisions run on a goroutine of
// its own, and a test reads the counts from its own.
type fakeAdmitter struct {
	mu sync.Mutex
	// refuse is what the gate answers from now on: false grants, true refuses, as a real gate
	// does once K credits are out.
	refuse      bool
	asks        refreshAsks
	corrections []int
}

func newFakeAdmitter() *fakeAdmitter { return &fakeAdmitter{} }

var _ RefreshAdmitter = (*fakeAdmitter)(nil)

func (g *fakeAdmitter) Admit() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asks.asked++
	if g.refuse {
		return false
	}
	g.asks.granted++
	return true
}

func (g *fakeAdmitter) Release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asks.released++
}

func (g *fakeAdmitter) Correct(waiting int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.corrections = append(g.corrections, waiting)
}

// refusing sets what the gate answers the next admission with, which is how a test puts the gate
// at its K without counting to it.
func (g *fakeAdmitter) refusing(refuse bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refuse = refuse
}

func (g *fakeAdmitter) counted() refreshAsks {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.asks
}

// wantAsks is the whole of what the gate was asked for at this point of the test, why naming what
// the machine had just been asked to do. No machine corrects a gate — the correction is the
// sweep's own call, made from the wakes the machines publish (internal/daemon's
// refreshStalePlugins) — so a correction counted here is a machine reaching for a method that is
// not its to call, and every one of these reads says so.
func (g *fakeAdmitter) wantAsks(t *testing.T, why string, want refreshAsks) {
	t.Helper()
	if got := g.counted(); got != want {
		t.Fatalf("%s: the gate was asked %+v, want %+v", why, got, want)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.corrections) != 0 {
		t.Fatalf("%s: a machine corrected the gate with %v, want no correction: that is the sweep's own call", why, g.corrections)
	}
}

// gatedHarness is a paced harness whose machine holds a gate of its own, as every machine of a
// daemon holds the one gate that daemon built (Deps.RefreshGate, openSupervision in
// internal/daemon).
func gatedHarness(t *testing.T, rate int) (*harness, *fakeAdmitter) {
	t.Helper()
	gate := newFakeAdmitter()
	return pacedHarness(t, rate, func(d *Deps) { d.RefreshGate = gate }), gate
}

// refreshPass is the daemon's own stale-plugin pass over this one claim: a credit is asked of the
// gate first, and the event is posted only where one was granted, carrying it (refreshStalePlugins
// in internal/daemon, StalePlugin.Admitted). It answers whether the claim was posted at all, since
// a refused claim is left to the next sweep and nothing reaches the machine.
//
// It is the pass over a claim the snapshot shows idle, which is the only claim that pass takes
// a credit for: the controller's own and one the snapshot shows in a turn are posted with no
// credit at all. A case here that drives a claim in a turn through this helper is the window
// where the snapshot and the machine disagree — the claim was idle when the pass read it — and
// says so where it does it.
func (h *harness) refreshPass(gate *fakeAdmitter, trigger string) bool {
	h.t.Helper()
	if !gate.Admit() {
		return false
	}
	h.must(StalePlugin{Claim: h.token, Trigger: trigger, Admitted: true})
	return true
}

// mustRefreshPass is refreshPass over a claim the gate must admit.
func (h *harness) mustRefreshPass(gate *fakeAdmitter, trigger string) {
	h.t.Helper()
	if !h.refreshPass(gate, trigger) {
		h.t.Fatalf("the %s pass was refused its credit, want it granted", trigger)
	}
}

// A claim admitted while it is idle is relaunched at the wake that admission paid for, and the
// credit comes back there and not before: what the gate counts is a refresh waiting in the
// pacing, and this one waits from the post until its turn. The relaunch itself is the lane's
// ordinary one — the process the claim still runs is replaced at that turn and the claim is
// marked for this daemon's contract.
//
// The negative control is the credit given back at the decision that raises the relaunch (the
// wake check dropped from endRefreshLane, so every decision releases): the credit is then back
// while the wake still waits, the lane admits K more refreshes than it holds, and this test
// reads it at the asks taken while the wake waits.
func TestARefreshAdmittedWhileIdleIsRelaunchedAtItsWakeAndGivesItsCreditBackThere(t *testing.T) {
	h, gate := gatedHarness(t, wakeRate)
	h.reach(StateIdle)
	h.m.deps.PluginContract = refreshContract
	replaced := h.locator()

	h.mustRefreshPass(gate, "boot")

	waiting := h.wantWaitingWake(WakePluginRefresh)
	gate.wantAsks(t, "the refresh waiting its turn", refreshAsks{asked: 1, granted: 1})
	if got := h.claim(); got.State != StateIdle || got.Locator == nil || *got.Locator != replaced {
		t.Fatalf("the claim whose admitted refresh waits is %s recording %+v, want %s recording the process it still runs",
			got.State, got.Locator, StateIdle)
	}

	h.advance(waiting.At.Sub(h.clock.Now()))

	h.wantState(StateLaunching)
	h.wantStarts(h.token, 2)
	if resume := h.wantCalls("Resume", 1)[0]; resume.Previous == nil || *resume.Previous != replaced {
		t.Fatalf("the refresh's turn replaced %+v, want the process the gate left running, %+v", resume.Previous, replaced)
	}
	if got := h.claim(); got.PluginRelaunchFor != refreshContract {
		t.Fatalf("the claim after the refresh that ran is marked for %d, want %d", got.PluginRelaunchFor, refreshContract)
	}
	gate.wantAsks(t, "the refresh that ran its turn", refreshAsks{asked: 1, granted: 1, released: 1})
}

// A claim admitted while it was idle and in a turn when that wake runs is relaunched not at all.
// The wake is spent, the claim keeps the turn and the task that turn is working, the credit goes
// back, and the next sweep admits the claim again once it is idle — which is the whole of what
// the wait for a turn protects, carried into a wake that can be tens of minutes late
// (refreshOverturned, runWake, dispatch://INFRA-2712 R2).
//
// The negative control is the re-read taken out of runWake: the turn is then cut by a relaunch
// raised while the claim was idle, and this test reads it at the state, at the starts and at the
// task the claim no longer holds.
func TestARefreshWhoseClaimIsInATurnAtItsTurnRelaunchesNothingAndIsAdmittedAgainWhenIdle(t *testing.T) {
	const next = "review the pull request"
	h, gate := gatedHarness(t, wakeRate)
	h.reach(StateIdle)
	h.m.deps.PluginContract = refreshContract
	kept := h.locator()

	h.mustRefreshPass(gate, "sweep")
	waiting := h.wantWaitingWake(WakePluginRefresh)

	// The claim takes a task and starts a turn inside the wait. The task waits for the process
	// the relaunch would start, as every task delivered over a waiting launch does (sendPending),
	// so the turn is one of the worker's own.
	h.must(RequestDeliver{Claim: h.token, Task: next, ID: "outbox:42", Row: 42, Generation: 7})
	h.must(StreamTurnStart{Claim: h.token})
	h.wantState(StateWorking)

	h.advance(waiting.At.Sub(h.clock.Now()))

	h.wantState(StateWorking)
	h.wantStarts(h.token, 1)
	h.wantCalls("Resume", 0)
	if got := h.claim(); got.Locator == nil || *got.Locator != kept {
		t.Fatalf("the claim whose refresh met its turn records %+v, want the process it is working on, %+v", got.Locator, kept)
	}
	if p := h.pending(); p.Task != next {
		t.Fatalf("the claim holds %+v, want the task %q it still has to work: the aborted wake takes nothing", p, next)
	}
	gate.wantAsks(t, "the refresh aborted at its turn", refreshAsks{asked: 1, granted: 1, released: 1})
	if lines := h.logs.lines("supervise: the fact this claim's plugin refresh was raised for is gone"); len(lines) != 1 {
		t.Fatalf("the aborted refresh logged %v, want one line saying why it relaunched nothing", lines)
	}

	// The turn ends, and the next sweep finds the claim idle and still stale: it is admitted
	// again, and this relaunch waits its own turn.
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	h.wantState(StateIdle)

	h.mustRefreshPass(gate, "sweep")

	h.wantWaitingWake(WakePluginRefresh)
	gate.wantAsks(t, "the sweep after the turn ended", refreshAsks{asked: 2, granted: 2, released: 1})
}

// A claim that registered on this daemon's own contract while its refresh waited is relaunched by
// that wake not at all: there is nothing left to replace, and replacing it would cost the claim
// its process for a fact that is already fixed. The credit goes back, and the claim is left
// watched exactly as the lifecycle refusal beside it leaves one (watchAgain). The task the pass's
// relaunch took back is still the claim's under its number, and the worker takes it again once its
// agent is ready.
//
// The negative control is the re-read taken out of runWake: the claim's process is then replaced
// for a contract it already speaks, which this test reads at the starts and at the mark the
// relaunch would have written.
func TestARefreshWhoseClaimRegisteredCurrentInsideTheWaitRelaunchesNothing(t *testing.T) {
	h, gate := gatedHarness(t, wakeRate)
	h.reach(StateReady)
	h.m.deps.PluginContract = refreshContract
	kept := h.locator()

	// The task is taken, and the relaunch the pass raises takes it back under its number before
	// it parks the wake (replaceProcess, interrupted).
	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan", Row: 7})
	number := h.wantPrompts(1)[0].Number

	h.mustRefreshPass(gate, "sweep")
	waiting := h.wantWaitingWake(WakePluginRefresh)
	if p := h.pending(); p.taken() || p.Number != number {
		t.Fatalf("the claim holds %+v after the pass, want task %d taken back", p, number)
	}

	// The shim reconnects inside the wait and the agent registers on this daemon's own contract.
	h.must(RequestRegister{Claim: h.token, Generation: h.generation(), Session: session,
		SessionFile: sessionFile, CapabilityHash: []byte("capability"), PluginContract: refreshContract})

	h.advance(waiting.At.Sub(h.clock.Now()))

	h.wantStarts(h.token, 1)
	h.wantCalls("Resume", 0)
	if got := h.claim(); got.Locator == nil || *got.Locator != kept || got.PluginRelaunchFor != 0 {
		t.Fatalf("the claim whose refresh found it current records %+v marked for %d, want the process it still runs, %+v, marked for 0",
			got.Locator, got.PluginRelaunchFor, kept)
	}
	if p := h.pending(); p.Number != number {
		t.Fatalf("the claim holds %+v, want task %d it was already given", p, number)
	}
	gate.wantAsks(t, "the refresh that found its claim current", refreshAsks{asked: 1, granted: 1, released: 1})

	h.ready()
	if again := h.wantPrompts(2)[1]; again.Number != number {
		t.Fatalf("the take at the ready was of task %d, want task %d again", again.Number, number)
	}
}

// Every branch of refreshPlugin that raises no gated relaunch gives its credit straight back. The
// lane gives up nothing by any of them — the next sweep decides the claim again as it stands then
// — so a credit held through one would be a refresh the gate counts that nothing is waiting for,
// and K of them would stop the lane for good until the sweep's own correction caught up.
//
// A branch that leaves a wake of this claim's waiting is run on to that wake's turn (settle),
// because the credit that wake holds is half of what such a row says: every row ends with the
// gate back at the count of wakes this claim has waiting, which is the whole of what a credit
// may ever be held for (endRefreshLane, dispatch://INFRA-2712 R1).
//
// The negative control is the credit check removed from the end of a decision (endRefreshLane):
// each case then ends with a credit out and nothing holding it, which this test reads at the
// asks. The lagged second admission names a control of its own, at the row.
func TestEveryRefreshThatRaisesNoRelaunchGivesItsCreditBack(t *testing.T) {
	for _, tc := range []struct {
		name string
		// prepare leaves the claim where the branch under test reads it, and says whether the
		// claim's plugin is stale at all.
		prepare func(h *harness, gate *fakeAdmitter)
		// settle runs the claim on from the branch under test to where every credit of the row
		// is accounted for, which for a branch that leaves a wake waiting is that wake's turn.
		// It is nil for a branch that leaves none.
		settle func(h *harness)
		want   refreshAsks
	}{
		{"the contract is already this daemon's", func(h *harness, _ *fakeAdmitter) {
			// The pass reads the claims before it posts, so a registration that landed in
			// between leaves the machine a post for a fact already fixed.
			h.reach(StateIdle)
		}, nil, refreshAsks{asked: 1, granted: 1, released: 1}},
		{"the lagged second admission, over a wake of this claim's that already waits", func(h *harness, gate *fakeAdmitter) {
			// The pass reads the wakes the machines last published, so a wake parked a moment
			// before it counted is one its reading misses and posts a credit for anyway
			// (parkedWakes in internal/daemon, refreshPlugin): the post that lags reaches a
			// machine whose refresh is already waiting.
			h.reach(StateIdle)
			h.m.deps.PluginContract = refreshContract
			h.mustRefreshPass(gate, "boot")
			h.wantWaitingWake(WakePluginRefresh)
		}, func(h *harness) {
			// One flag holds one credit, so the lagged post's own goes back to the gate where
			// it arrives and the one the waiting wake holds goes back at that wake's turn,
			// which is run here: a row that stopped at the post would read the arrival release
			// alone and say nothing of the credit the gate is bounded by.
			//
			// The negative control of this row is the arrival release reverted (the post's
			// credit taken onto the flag the machine already holds): the two admissions are
			// then held by one flag, the wake's turn gives back one of them, and the lane is a
			// credit short for as long as it takes the sweep's own correction to find it.
			waiting := h.wantWaitingWake(WakePluginRefresh)
			h.advance(waiting.At.Sub(h.clock.Now()))
		}, refreshAsks{asked: 2, granted: 2, released: 2}},
		{"the claim is in a turn, and the relaunch waits for its end", func(h *harness, _ *fakeAdmitter) {
			// The daemon's pass takes no credit for a claim its snapshot shows in a turn
			// (refreshStalePlugins in internal/daemon), so the post that carries one here is
			// the window where that snapshot and the machine disagree: the claim the pass read
			// idle has started a turn by the time its post is decided. The credit is owed back
			// at once, which is what this case reads.
			h.reach(StateWorking)
			h.m.deps.PluginContract = refreshContract
		}, nil, refreshAsks{asked: 1, granted: 1, released: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, gate := gatedHarness(t, wakeRate)
			tc.prepare(h, gate)
			starts := h.starts(h.token)

			h.mustRefreshPass(gate, "sweep")

			if got := h.starts(h.token); got != starts {
				t.Fatalf("the runtime was asked to start the claim %d times, want the %d of before: this branch raises no relaunch", got, starts)
			}
			if tc.settle != nil {
				tc.settle(h)
			}
			waiting := 0
			if h.m.View().WakeWaiting != nil {
				waiting = 1
			}
			// The invariant is read before the row's own numbers are, and off the gate rather
			// than off tc.want: after the assertion below the two are equal, so a check made
			// there could fail for nothing the assertion has not already failed for.
			asks := gate.counted()
			if out := asks.granted - asks.released; out != waiting {
				t.Fatalf("the gate holds %d credits out over %d wake of this claim's waiting, want one credit for each: a credit is held for a waiting wake and nothing else",
					out, waiting)
			}
			gate.wantAsks(t, "the branch that raised no relaunch", tc.want)
		})
	}
}

// The other half of that window is the post this one mirrors: the pass read the claim in a turn,
// so it posted with no credit at all (refreshStalePlugins in internal/daemon), and the claim's
// turn ended between that read and this decision. The relaunch is reached with no credit, asks
// the gate itself, and relaunches nothing where the gate is full — the claim is left idle and
// stale for the next sweep, which reads it idle and admits it like any other (plugin.go's
// refreshPlugin, dispatch://INFRA-2712 R1).
//
// The disagreement is between the State in the pass's snapshot and the State this machine holds,
// and both are values this test sets: the post carries no credit, which is what the pass writes
// for a claim its snapshot showed working, and the machine is idle, which is where the turn's
// end left it. Nothing of it is timed, which is why it is here rather than in the daemon's wave
// rig, where the same window is manufactured by racing a wall-clock sweep against turn ends.
//
// The negative control is the guard removed (the `!controller && !m.refreshCredit` branch of
// refreshPlugin): eight such claims at once where a wave's turns end together park eight wakes
// the gate never granted, which is twice the rate in the pacing. This test reads it at the wake
// the claim then holds and at the asks the gate was never made.
func TestAnUncreditedRelaunchAsksTheGateAndRelaunchesNothingWhenItIsFull(t *testing.T) {
	h, gate := gatedHarness(t, wakeRate)
	h.reach(StateIdle)
	h.m.deps.PluginContract = refreshContract
	starts := h.starts(h.token)

	// Every credit is out over other claims of the same wave by the time this post is decided.
	gate.refusing(true)
	// Admitted is omitted, as the daemon's pass omits it for a claim its snapshot read in a turn
	// (refreshStalePlugins, StalePlugin.Admitted).
	h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})

	h.wantState(StateIdle)
	if got := h.starts(h.token); got != starts {
		t.Fatalf("the runtime was asked to start the claim %d times, want the %d of before: the gate refused this relaunch", got, starts)
	}
	if held := h.m.View().WakeWaiting; held != nil {
		t.Fatalf("the claim whose uncredited relaunch the gate refused names wake %+v, want none", held)
	}
	gate.wantAsks(t, "the uncredited relaunch the gate refused", refreshAsks{asked: 1})
	if lines := h.logs.lines("supervise: this claim's stale-plugin relaunch reached the refresh gate with no credit of its own and the gate is full"); len(lines) != 1 {
		t.Fatalf("the refused relaunch logged %v, want one line saying it waits for the next sweep", lines)
	}
}

// The fourth branch of refreshPlugin that raises no relaunch is the claim whose process holds no
// capability, and it gives its credit back like the three above. It is reached by one road only,
// which is why this test calls it where the others post an event: the table ignores a stale-plugin
// post over every state with no process at all (the onStalePlugin rows), so the daemon's pass can
// put no claim here, and the entry that can is the turn's end running a relaunch it held
// (afterTurn) over a claim the end of that turn left without one. The guard lives in
// refreshPlugin rather than in the pass alone for exactly that reason: a relaunch raised here
// would start a process nobody asked for.
//
// The negative control is the credit check removed from the end of a decision (endRefreshLane):
// the credit is then held by a machine that did nothing with it until the sweep's own correction
// takes it back, which this test reads at the asks.
func TestARefreshOverAClaimWithNoProcessToReplaceGivesItsCreditBack(t *testing.T) {
	h, gate := gatedHarness(t, wakeRate)
	h.reach(StateSuspended)
	h.m.deps.PluginContract = refreshContract

	if !gate.Admit() {
		t.Fatal("the gate refused the first admission of a fresh gate")
	}
	err := h.insideDecision(h.m, func() error { return h.m.refreshPlugin(h.ctx, "sweep", true) })
	if err != nil {
		t.Fatalf("the refresh of a suspended claim = %v, want nil", err)
	}

	h.wantState(StateSuspended)
	h.wantCalls("Resume", 0)
	gate.wantAsks(t, "the refresh of a claim with no process to replace", refreshAsks{asked: 1, granted: 1, released: 1})
}

// The relaunch a claim's turn held is the gate's second asker: it took no credit while the turn
// ran, so the relaunch its end runs asks for one at refreshPlugin's own guard and relaunches on
// it (afterTurn, holdStaleRelaunch, dispatch://INFRA-2712 R1). The pass here posts a credit with
// it, which is the window where the pass's snapshot and the machine disagree (refreshPass): the
// credit is given back where the relaunch is held, and the one the turn's end asks for is a
// second of its own, which is what this case counts.
//
// The negative control is the ask skipped for this road (afterTurn calling refreshPlugin as
// admitted, or the guard narrowed to the pass's own posts): the lane then relaunches one claim
// for every turn that ends, outside the bound the gate holds it to, which this test reads at
// the asks.
func TestTheRelaunchHeldForATurnAsksTheGateAtThatTurnsEnd(t *testing.T) {
	h, gate := gatedHarness(t, wakeRate)
	h.reach(StateWorking)
	h.m.deps.PluginContract = refreshContract
	replaced := h.locator()

	h.mustRefreshPass(gate, "sweep")

	if h.m.staleRelaunch == nil {
		t.Fatal("no relaunch waits for the turn: the claim in a turn was decided where it stood")
	}
	gate.wantAsks(t, "the relaunch held for the turn", refreshAsks{asked: 1, granted: 1, released: 1})

	h.must(StreamTurnEnd{Claim: h.token})

	waiting := h.wantWaitingWake(WakePluginRefresh)
	gate.wantAsks(t, "the turn's end asking for the held relaunch", refreshAsks{asked: 2, granted: 2, released: 1})

	h.advance(waiting.At.Sub(h.clock.Now()))

	h.wantStarts(h.token, 2)
	if resume := h.wantCalls("Resume", 1)[0]; resume.Previous == nil || *resume.Previous != replaced {
		t.Fatalf("the relaunch the turn's end raised replaced %+v, want the process the turn ran on, %+v", resume.Previous, replaced)
	}
	gate.wantAsks(t, "the relaunch of the ended turn, run", refreshAsks{asked: 2, granted: 2, released: 2})
}

// A held relaunch the gate refuses at its turn's end relaunches nothing and is not held again:
// the mark waits for a turn, and that turn has ended. The claim is left idle and stale, which is
// what the next sweep posts for, and it is relaunched on the first credit that comes free.
//
// The negative control is the refusal ignored (the guard relaunching whatever the gate
// answered): the claim is then relaunched at its turn's end past a full gate, which this test
// reads at the starts taken while the gate refuses.
func TestAHeldRelaunchTheGateRefusesAtTheTurnsEndWaitsForTheNextSweep(t *testing.T) {
	h, gate := gatedHarness(t, wakeRate)
	h.reach(StateWorking)
	h.m.deps.PluginContract = refreshContract

	h.mustRefreshPass(gate, "sweep")
	if h.m.staleRelaunch == nil {
		t.Fatal("no relaunch waits for the turn: the claim in a turn was decided where it stood")
	}

	// The gate is at its K by the time the turn ends: every credit is out over other claims of
	// the same wave.
	gate.refusing(true)
	h.must(StreamTurnEnd{Claim: h.token})

	h.wantState(StateIdle)
	h.wantStarts(h.token, 1)
	h.wantCalls("Resume", 0)
	if held := h.m.View().WakeWaiting; held != nil {
		t.Fatalf("the claim whose relaunch the gate refused names wake %+v, want none", held)
	}
	if h.m.staleRelaunch != nil {
		t.Fatalf("the refused relaunch waits for a turn again (%q), want it left to the next sweep: this turn has ended", *h.m.staleRelaunch)
	}
	gate.wantAsks(t, "the turn's end the gate refused", refreshAsks{asked: 2, granted: 1, released: 1})
	if lines := h.logs.lines("supervise: this claim's stale-plugin relaunch reached the refresh gate with no credit of its own and the gate is full"); len(lines) != 1 {
		t.Fatalf("the refused relaunch logged %v, want one line saying it waits for the next sweep", lines)
	}

	// A credit comes free, and the next sweep posts the claim as it stands then: idle, and still
	// speaking the contract this daemon does not.
	gate.refusing(false)
	h.mustRefreshPass(gate, "sweep")

	waiting := h.wantWaitingWake(WakePluginRefresh)
	h.advance(waiting.At.Sub(h.clock.Now()))

	h.wantStarts(h.token, 2)
	gate.wantAsks(t, "the sweep after a credit came free", refreshAsks{asked: 3, granted: 2, released: 2})
}

// A refresh whose wake is let go with the claim's process gives its credit back there. Every
// suspension, failure and retirement of a claim ends with that one call (letGo), which drops the
// wake parked over the process together with the process itself: nothing of the refresh waits in
// the pacing after it, so the credit is owed back at once (dispatch://INFRA-2712 R1).
//
// A credit kept here would cost the lane more than a credit. The sweep's own correction takes it
// back a minute later, but the machine goes on reading it as held until then, so the next post
// it meets with no credit of its own skips the gate and parks a wake the gate never granted,
// which is the one thing the bound promises cannot happen (refreshPlugin, Machine.refreshCredit).
//
// The negative control is the credit check removed from the end of a decision (endRefreshLane):
// the suspended claim then holds a credit for a wake that no longer exists, which this test
// reads at the asks.
func TestARefreshWhoseWakeIsLetGoWithTheClaimsProcessGivesItsCreditBack(t *testing.T) {
	h, gate := gatedHarness(t, wakeRate)
	h.reach(StateIdle)
	h.m.deps.PluginContract = refreshContract

	h.mustRefreshPass(gate, "sweep")
	h.wantWaitingWake(WakePluginRefresh)

	// The operator suspends the claim while its refresh waits: the stop lets the process go, and
	// the wake parked over that process goes with it (the table's suspend row, suspendNow, letGo).
	h.must(RequestSuspend{Claim: h.token, Reason: "the operator drains the tree", By: SuspendedByOperator})

	h.wantState(StateSuspended)
	h.wantStarts(h.token, 1)
	h.wantCalls("Suspend", 1)
	if held := h.m.View().WakeWaiting; held != nil {
		t.Fatalf("the suspended claim names wake %+v, want none: the wake went with the process it was parked over", held)
	}
	gate.wantAsks(t, "the refresh whose wake was let go with the claim's process", refreshAsks{asked: 1, granted: 1, released: 1})
}

// A refresh whose turn finds a suspension held over the claim relaunches nothing: the hold ends
// the process the relaunch would have replaced, and the claim is suspended rather than launched
// (wakeNow, endHeld). The wake is spent and nothing of this refresh waits in the pacing, so the
// credit goes back with the process the suspension lets go (suspendedFor, letGo).
//
// It is the one of wakeNow's two turns that do not reach runWake which the machine drives: the
// other is a wake whose kind starts no process, and every kind a machine parks today starts one
// (StartsProcess, reserveWake).
//
// The negative control is the credit check removed from the end of a decision (endRefreshLane):
// the suspended claim then holds a credit for a relaunch that never ran, which this test reads
// at the asks.
func TestARefreshWhoseTurnEndsAHeldSuspensionGivesItsCreditBack(t *testing.T) {
	h, gate := gatedHarness(t, wakeRate)
	h.reach(StateIdle)
	h.m.deps.PluginContract = refreshContract

	h.mustRefreshPass(gate, "sweep")
	waiting := h.wantWaitingWake(WakePluginRefresh)

	// A task starts a turn inside the wait — the task itself waits for the process the relaunch
	// would start (sendPending) — and the operator's suspend is held for that turn's end
	// (holdSuspension). The hold is still standing when the wake's turn comes.
	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan", ID: "outbox:42", Row: 42, Generation: 7})
	h.must(StreamTurnStart{Claim: h.token})
	h.wantState(StateWorking)
	if err := h.handle(RequestSuspend{Claim: h.token, Reason: "the operator drains the tree", By: SuspendedByOperator}); !suspensionHeld(err) {
		t.Fatalf("the suspend of a claim in a turn = %v, want the hold", err)
	}

	h.advance(waiting.At.Sub(h.clock.Now()))

	h.wantState(StateSuspended)
	h.wantStarts(h.token, 1)
	h.wantCalls("Resume", 0)
	h.wantCalls("Suspend", 1)
	if held := h.m.View().WakeWaiting; held != nil {
		t.Fatalf("the suspended claim names wake %+v, want none: its turn ended the suspension instead of launching", held)
	}
	gate.wantAsks(t, "the refresh whose turn ended the suspension held over its claim", refreshAsks{asked: 1, granted: 1, released: 1})
}

// cleanupReservedAfterFirstCheckStore answers one lifecycle recheck and refuses every one after
// it with a reserved tree cleanup, which is the launch path's second check refusing where the
// relaunch's own first check passed: an operator's close reserves the tree's cleanup between
// the two (relaunchStale's checkLaunch, launchAfter's, relaunchRefused).
type cleanupReservedAfterFirstCheckStore struct {
	*memStore
	answered int
}

func (s *cleanupReservedAfterFirstCheckStore) CheckLaunch(ctx context.Context, c Claim) error {
	s.answered++
	if s.answered > 1 {
		return treelifecycle.ErrCleanupReserved
	}
	return s.memStore.CheckLaunch(ctx, c)
}

// A refused launch gives the credit back wherever the refusal came from, because the one release
// sits in the refusal of every launch there is (relaunchRefused, dispatch://INFRA-2712 R1). The
// refusal driven here is the one that reaches that refusal's first branch over a claim which
// still records its process: the error goes back to whoever asked for it and no retry is armed,
// so the credit would otherwise be held by a machine with no wake behind it until the sweep's
// own correction took it back — and, until then, read as held, which makes the machine skip the
// gate at its next uncredited post and park a wake the gate never granted (Machine.refreshCredit,
// refreshPlugin).
//
// It is driven rather than placed: the relaunch's own lifecycle check passes, as it does for
// every relaunch the lane raises, and the launch path's second check meets the cleanup an
// operator's close reserved in between (cleanupReservedAfterFirstCheckStore). Nothing of the
// claim is written by it — the process it runs, its state and the mark of whatever relaunch
// last ran are exactly as the refusal found them — which is what the rest of this test reads
// beside the asks.
//
// The negative control is the credit check removed from the end of a decision (endRefreshLane):
// the idle claim then holds a credit for a relaunch that never ran, which this test reads at the
// asks.
func TestARefreshTheLaunchPathRefusesGivesItsCreditBack(t *testing.T) {
	h, gate := gatedHarness(t, wakeRate)
	h.reach(StateIdle)
	h.m.deps.PluginContract = refreshContract
	kept := h.locator()
	h.m.deps.Store = &cleanupReservedAfterFirstCheckStore{memStore: h.store}

	// The pass's own admission, as refreshPass makes it: the event is posted carrying the credit
	// the gate granted for it (refreshStalePlugins in internal/daemon, StalePlugin.Admitted).
	// The post is handled here rather than through that helper because the refusal hands its
	// error back to the caller.
	if !gate.Admit() {
		t.Fatal("the sweep pass was refused its credit, want it granted")
	}
	err := h.handle(StalePlugin{Claim: h.token, Trigger: "sweep", Admitted: true})
	if !errors.Is(err, treelifecycle.ErrCleanupReserved) {
		t.Fatalf("the relaunch the launch path refused = %v, want the reservation's wait", err)
	}

	if got := h.claim(); got.State != StateIdle || got.Locator == nil || *got.Locator != kept {
		t.Fatalf("the claim whose relaunch the launch path refused is %s recording %+v, want %s recording the process it still runs",
			got.State, got.Locator, StateIdle)
	}
	h.wantStarts(h.token, 1)
	if got := h.claim(); got.PluginRelaunchFor != 0 {
		t.Fatalf("PluginRelaunchFor after the refusal = %d, want 0: only a launch that starts a process writes the mark", got.PluginRelaunchFor)
	}
	if held := h.m.View().WakeWaiting; held != nil {
		t.Fatalf("the claim whose relaunch the launch path refused names wake %+v, want none", held)
	}
	gate.wantAsks(t, "the refresh the launch path refused", refreshAsks{asked: 1, granted: 1, released: 1})
}

// taskTheAgentTook hands the claim a task its worker takes, before anything of this lane is
// decided, and answers the task's number: the take is accepted and its turn has not started, so
// the claim is idle with the task in the worker's hands, which is what a stale-plugin relaunch
// takes back before it replaces the process (replaceProcess, interrupted).
func (h *harness) taskTheAgentTook() uint64 {
	h.t.Helper()
	h.must(RequestDeliver{Claim: h.token, Task: "review the pull request", ID: "outbox:42", Row: 42, Generation: 7})
	p := h.pending()
	if !p.awaitsTurn() {
		h.t.Fatalf("the task is %+v, want its take accepted and its turn still to come", p)
	}
	return p.Number
}

// wantTakeBackRefused is the tail of a refused take-back: one claim standing exactly as the
// refusal found it, and the credit of the relaunch that met it back at the gate (endRefreshLane,
// dispatch://INFRA-2712 R1). The claim is idle over the process it still runs (kept), nothing was
// started and nothing resumed, no wake waits — the relaunch raised none — and the task is still
// taken, because the take-back's write did not land: the task stands exactly as the store holds
// it, which is what keeps a restart from retiring a task no turn ever finished (interrupted).
//
// Then it reads that credit where losing it would be read. The claim is idle and still stale,
// so the next sweep decides it again, and that post carries no credit, as the pass's post of a
// claim its snapshot read in a turn does: the relaunch it raises asks the gate for one of its
// own, which a machine still holding the credit of a relaunch that is gone would skip
// (refreshPlugin's guard, Machine.refreshCredit).
//
// The negative control is the credit check removed from the end of a decision (endRefreshLane):
// the uncredited post that follows then raises its relaunch having asked the gate nothing, which
// this tail reads at the asks and at the wake the claim holds.
func (h *harness) wantTakeBackRefused(gate *fakeAdmitter, kept runtime.Locator, why string) {
	h.t.Helper()
	h.wantState(StateIdle)
	h.wantStarts(h.token, 1)
	h.wantCalls("Resume", 0)
	if got := h.claim(); got.Locator == nil || *got.Locator != kept {
		h.t.Fatalf("%s: the claim records %+v, want the process it still runs, %+v", why, got.Locator, kept)
	}
	if p := h.pending(); !p.taken() {
		h.t.Fatalf("%s: the claim holds %+v, want the task still taken: the take-back's write did not land", why, p)
	}
	if held := h.m.View().WakeWaiting; held != nil {
		h.t.Fatalf("%s: the claim names wake %+v, want none", why, held)
	}
	gate.wantAsks(h.t, why, refreshAsks{asked: 1, granted: 1, released: 1})

	h.store.fail("PutDelivery", nil)
	h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})

	h.wantWaitingWake(WakePluginRefresh)
	gate.wantAsks(h.t, "the uncredited post after "+why, refreshAsks{asked: 2, granted: 2, released: 1})
}

// A take-back the store refuses at the relaunch's own decision (replaceNow) gives the credit back:
// the relaunch replaces the claim's process where it is decided, and the take-back it makes first
// is refused, so it returns having raised no wake at all — with the credit it was admitted on,
// which the end of that decision gives back (endRefreshLane, dispatch://INFRA-2712 R1).
//
// Here the task is taken before the refresh is posted, which is what puts the take-back in the
// relaunch's own decision (taskTheAgentTook). The refusal writes nothing of the claim, the mark of
// whatever relaunch last ran included, which this case reads before the tail
// (PluginRelaunchFor, wantTakeBackRefused).
func TestATakeBackTheStoreRefusesAtTheRelaunchsOwnDecisionGivesTheCreditBack(t *testing.T) {
	h, gate := gatedHarness(t, wakeRate)
	h.reach(StateIdle)
	kept := h.locator()
	h.taskTheAgentTook()
	h.m.deps.PluginContract = refreshContract

	// The pass's own admission, as refreshPass makes it (refreshStalePlugins in internal/daemon,
	// StalePlugin.Admitted). The post is handled here rather than through that helper because the
	// refused take-back hands its error back to the caller.
	h.store.fail("PutDelivery", errBoom)
	if !gate.Admit() {
		t.Fatal("the sweep pass was refused its credit, want it granted")
	}
	if err := h.handle(StalePlugin{Claim: h.token, Trigger: "sweep", Admitted: true}); !errors.Is(err, errBoom) {
		t.Fatalf("the relaunch whose take-back the store refused = %v, want the store's refusal", err)
	}

	if got := h.claim(); got.PluginRelaunchFor != 0 {
		t.Fatalf("PluginRelaunchFor after the refused take-back = %d, want 0: only a launch that starts a process writes the mark", got.PluginRelaunchFor)
	}
	h.wantTakeBackRefused(gate, kept, "the take-back the store refused at the relaunch's own decision")
}
