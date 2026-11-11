package supervise

// The stale-plugin refresh gate as a machine meets it: one credit for one relaunch, taken before
// the relaunch was decided and given back where that relaunch runs or is abandoned
// (RefreshGate, admitRefresh, releaseRefresh, dispatch://INFRA-2712 R1). The gate itself is
// counted in gate_test.go; what these tests hold is the machine's half of it — that every path
// out of refreshPlugin which raises no relaunch returns its credit at once, that a relaunch the
// pacing holds keeps its credit for exactly as long as it waits, and that a claim whose turn ends
// with a relaunch held asks the gate for one of its own.
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
	"sync"
	"testing"
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
// The negative control is the release moved to the decision that raises the relaunch (a
// releaseRefresh before relaunchStale returns): the credit is then back while the wake still
// waits, the lane admits K more refreshes than it holds, and this test reads it at the asks taken
// while the wake waits.
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
// its process and the task it holds for a fact that is already fixed. The credit goes back, and
// the claim is left watched exactly as the lifecycle refusal beside it leaves one (watchAgain):
// the bound the acknowledged prompt waits for its turn on, which the park disarmed, is armed
// again, so a prompt the agent read and never turned on still fails rather than waiting for ever.
//
// The negative control is the re-read taken out of runWake: the claim's process is then replaced
// for a contract it already speaks, which this test reads at the starts and at the mark the
// relaunch would have written.
func TestARefreshWhoseClaimRegisteredCurrentInsideTheWaitRelaunchesNothing(t *testing.T) {
	h, gate := gatedHarness(t, wakeRate)
	h.reach(StateReady)
	h.m.deps.PluginContract = refreshContract
	kept := h.locator()

	// The task is sent and the agent acknowledges it, which is what arms the bound the park
	// takes away (acked, parkWake).
	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan", Row: 7})
	acked := h.wantPrompts(1)[0].DeliveryID

	h.mustRefreshPass(gate, "sweep")
	waiting := h.wantWaitingWake(WakePluginRefresh)
	if _, awaiting := h.m.timers[TimerTurn]; awaiting {
		t.Fatal("the park left the turn bound of the process the refresh replaces armed, want it disarmed")
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
	if p := h.pending(); p.ID != acked {
		t.Fatalf("the claim holds %+v, want the task %s it was already working", p, acked)
	}
	if _, awaiting := h.m.timers[TimerTurn]; !awaiting {
		t.Fatal("the acknowledged prompt the aborted refresh kept waits for its turn on nothing, want the bound armed again")
	}
	gate.wantAsks(t, "the refresh that found its claim current", refreshAsks{asked: 1, granted: 1, released: 1})
}

// Every branch of refreshPlugin that raises no gated relaunch gives its credit straight back. The
// lane gives up nothing by any of them — the next sweep decides the claim again as it stands then
// — so a credit held through one would be a refresh the gate counts that nothing is waiting for,
// and K of them would stop the lane for good until the sweep's own correction caught up.
//
// The negative control is the releases removed from refreshPlugin: each case then ends with a
// credit out and nothing holding it, which this test reads at the asks.
func TestEveryRefreshThatRaisesNoRelaunchGivesItsCreditBack(t *testing.T) {
	for _, tc := range []struct {
		name string
		// prepare leaves the claim where the branch under test reads it, and says whether the
		// claim's plugin is stale at all.
		prepare func(h *harness, gate *fakeAdmitter)
		want    refreshAsks
	}{
		{"the contract is already this daemon's", func(h *harness, _ *fakeAdmitter) {
			// The pass reads the claims before it posts, so a registration that landed in
			// between leaves the machine a post for a fact already fixed.
			h.reach(StateIdle)
		}, refreshAsks{asked: 1, granted: 1, released: 1}},
		{"a wake of this claim's already waits", func(h *harness, gate *fakeAdmitter) {
			h.reach(StateIdle)
			h.m.deps.PluginContract = refreshContract
			h.mustRefreshPass(gate, "boot")
			h.wantWaitingWake(WakePluginRefresh)
		}, refreshAsks{asked: 2, granted: 2, released: 1}},
		{"the claim is in a turn, and the relaunch waits for its end", func(h *harness, _ *fakeAdmitter) {
			// The daemon's pass takes no credit for a claim its snapshot shows in a turn
			// (refreshStalePlugins in internal/daemon), so the post that carries one here is
			// the window where that snapshot and the machine disagree: the claim the pass read
			// idle has started a turn by the time its post is decided. The credit is owed back
			// at once, which is what this case reads.
			h.reach(StateWorking)
			h.m.deps.PluginContract = refreshContract
		}, refreshAsks{asked: 1, granted: 1, released: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, gate := gatedHarness(t, wakeRate)
			tc.prepare(h, gate)
			starts := h.starts(h.token)

			h.mustRefreshPass(gate, "sweep")

			if got := h.starts(h.token); got != starts {
				t.Fatalf("the runtime was asked to start the claim %d times, want the %d of before: this branch raises no relaunch", got, starts)
			}
			gate.wantAsks(t, "the branch that raised no relaunch", tc.want)
		})
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
// The negative control is the release removed from that branch: the credit is then held by a
// machine that did nothing with it until the sweep's own correction takes it back, which this
// test reads at the asks.
func TestARefreshOverAClaimWithNoProcessToReplaceGivesItsCreditBack(t *testing.T) {
	h, gate := gatedHarness(t, wakeRate)
	h.reach(StateSuspended)
	h.m.deps.PluginContract = refreshContract

	if !gate.Admit() {
		t.Fatal("the gate refused the first admission of a fresh gate")
	}
	if err := h.m.refreshPlugin(h.ctx, "sweep", true); err != nil {
		t.Fatalf("the refresh of a suspended claim = %v, want nil", err)
	}

	h.wantState(StateSuspended)
	h.wantCalls("Resume", 0)
	gate.wantAsks(t, "the refresh of a claim with no process to replace", refreshAsks{asked: 1, granted: 1, released: 1})
}

// The relaunch a claim's turn held is the gate's second asker: it took no credit while the turn
// ran, so its turn's end asks for one and relaunches on it (afterTurn, holdStaleRelaunch,
// dispatch://INFRA-2712 R1). The pass here posts a credit with it, which is the window where
// the pass's snapshot and the machine disagree (refreshPass): the credit is given back where
// the relaunch is held, and the one the turn's end asks for is a second of its own, which is
// what this case counts.
//
// The negative control is the ask removed from afterTurn (the held relaunch run through
// refreshPlugin with no credit): the lane then relaunches one claim for every turn that ends,
// outside the bound the gate holds it to, which this test reads at the asks.
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
// The negative control is the refusal ignored (afterTurn relaunching whatever the gate answered):
// the claim is then relaunched at its turn's end past a full gate, which this test reads at the
// starts taken while the gate refuses.
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
	if lines := h.logs.lines("supervise: the turn of this claim on another plugin contract has ended, and the refresh gate is full"); len(lines) != 1 {
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
