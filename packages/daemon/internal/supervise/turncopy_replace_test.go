package supervise

// What would let a live process go while its held stop waits for the copy its turn's end started
// (R1 of dispatch://INFRA-1117): a stale-plugin relaunch, a stale address, the turn of a wake the
// pacing parked over that process, the agent's ready said again, the stall backstop over a turn
// the agent started since, and a registration on another plugin contract. Each would run or end
// the held stop while the copy still asks the process's shim, so each waits for that copy as the
// stop does.

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// copyWaiting is the copying harness's claim idle on its process, holding a daemon's stop that
// waits for the copy its turn's end started. It answers the copies and the process the copy asks.
func copyWaiting(t *testing.T) (*harness, *turnEndCopies, runtime.Locator) {
	t.Helper()
	h, copies := copyingHarness(t)
	h.reach(StateWorking)
	holdStop(t, h, "LEGION-208 left implementing")
	h.must(StreamTurnEnd{Claim: h.token})
	h.wantState(StateIdle)
	return h, copies, h.locator()
}

// copyWaitingStale is copyWaiting's claim speaking the contract of the daemon before this one,
// which can relaunch onto its plugin.
func copyWaitingStale(t *testing.T) (*harness, *turnEndCopies, runtime.Locator) {
	t.Helper()
	h, copies, loc := copyWaiting(t)
	h.deps.PluginContract, h.m.deps.PluginContract = refreshContract, refreshContract
	h.m.deps.PluginRelaunchable = true
	return h, copies, loc
}

// waitsOnItsProcess fails unless the claim is still idle on loc, holding its stop, with no process
// stopped: what came in was left to the copy's end.
func waitsOnItsProcess(t *testing.T, h *harness, loc runtime.Locator) {
	t.Helper()
	h.wantState(StateIdle)
	h.wantCalls("Suspend", 0)
	if c := h.claim(); !c.SuspensionHeld || c.Locator == nil || *c.Locator != loc {
		t.Fatalf("the claim is %+v, want it idle on its process %+v, holding the stop that waits for the copy", c, loc)
	}
}

// stopsAfterTheCopy hands back the end of the copy the turn's end started, and fails unless the
// held stop then stops loc, once.
func stopsAfterTheCopy(t *testing.T, h *harness, copies *turnEndCopies, loc runtime.Locator) {
	t.Helper()
	h.must(copies.copied(t, 0))
	h.wantState(StateSuspended)
	if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != loc {
		t.Fatalf("suspended %+v, want the process whose turn ended, %+v", suspend.Locator, loc)
	}
}

// The stale-plugin sweep posts every claim behind the daemon's contract, which at a release that
// moves the contract is every claim. A claim whose held stop waits for its turn's copy is idle, so
// the relaunch was not held back for a turn, and it let the process go through the held stop while
// the copy still asked its shim: the copy failed and the turn's end was in no copy once the role
// was stopped. The relaunch is held while the stop waits, as it is held for a turn, and the copy's
// end runs the stop, which takes the process and the relaunch with it.
//
// The control is the relaunch held back for a turn alone: the claim is then suspended at the
// sweep's post, while the copy is still being taken, and the test fails there.
func TestAStalePluginRelaunchWaitsForTheTurnsCopyItsHeldStopWaitsFor(t *testing.T) {
	h, copies, loc := copyWaitingStale(t)

	h.must(StalePlugin{Claim: h.token, Trigger: "sweep", Admitted: true})

	waitsOnItsProcess(t, h, loc)
	stopsAfterTheCopy(t, h, copies, loc)
}

// A process found at a stale address is replaced, and that replacement lets the process go
// through a held stop. While the stop waits for its turn's copy the observation is left alone:
// the runtime observes the process again at every tick, and the copy's end runs the stop, which
// lets the process go itself.
//
// The control is the observation replacing the process as it comes: the claim is then suspended
// while the copy is still being taken, and the test fails there.
func TestAStaleAddressWaitsForTheTurnsCopyItsHeldStopWaitsFor(t *testing.T) {
	h, copies, loc := copyWaitingStale(t)

	h.observe(runtime.StaleAddress)

	waitsOnItsProcess(t, h, loc)
	stopsAfterTheCopy(t, h, copies, loc)
}

// A wake the pacing parked over a live process — a stale-plugin relaunch raised while the claim
// was idle — can come due after the agent's own turn has ended into a held stop that waits for
// that turn's copy. Its turn ends the hold (endHeld), which would let the process go while the
// copy still asks its shim, so the wake waits for that copy too and stays on the machine; the
// copy's end runs the stop, whose let-go takes the wake with the process and launches nothing.
//
// The control is the wake's turn ending the hold as it comes: the claim is then suspended at the
// wake's turn, while the copy is still being taken, and the test fails there.
func TestAParkedWakeWaitsForTheTurnsCopyItsHeldStopWaitsFor(t *testing.T) {
	copies := &turnEndCopies{}
	h := pacedHarness(t, wakeRate, func(d *Deps) { d.CopyTurnEnd = copies.start })
	h.reach(StateIdle)
	h.must(copies.copied(t, 0))
	h.deps.PluginContract, h.m.deps.PluginContract = refreshContract, refreshContract
	h.m.deps.PluginRelaunchable = true
	h.must(StalePlugin{Claim: h.token, Trigger: "sweep", Admitted: true})
	waiting := h.wantWaitingWake(WakePluginRefresh)
	loc := h.locator()
	h.must(StreamTurnStart{Claim: h.token})
	holdStop(t, h, "LEGION-208 left implementing")
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	h.wantState(StateIdle)
	starts := len(h.calls("Resume")) + len(h.calls("Spawn"))

	h.advance(waiting.At.Sub(h.clock.Now()))

	waitsOnItsProcess(t, h, loc)
	if h.m.View().WakeWaiting == nil {
		t.Fatal("the wake is off the machine, want it waiting for the copy with the stop")
	}
	h.must(copies.copied(t, 1))
	h.wantState(StateSuspended)
	if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != loc {
		t.Fatalf("suspended %+v, want the process whose turn ended, %+v", suspend.Locator, loc)
	}
	if got := len(h.calls("Resume")) + len(h.calls("Spawn")); got != starts {
		t.Fatalf("the runtime was asked %d launches after the turn's end, want none: the stop takes the wake with the process", got-starts)
	}
	if h.m.View().WakeWaiting != nil {
		t.Fatalf("the claim still holds the wake %+v, want it taken by the stop's let-go", h.m.View().WakeWaiting)
	}
}

// An agent that says ready again finds a held stop and runs it (reready), which a held stop that
// waits for its turn's copy must not do: the stop would take the process the copy still asks. The
// ready said again changes nothing then, and the copy's end runs the stop.
//
// The control is the ready running any held stop as it comes: the claim is then suspended at the
// ready, while the copy is still being taken, and the test fails there.
func TestAReadySaidAgainWaitsForTheTurnsCopyItsHeldStopWaitsFor(t *testing.T) {
	h, copies, loc := copyWaiting(t)

	h.must(RequestReady{Claim: h.token, Generation: h.generation(), Session: session})

	waitsOnItsProcess(t, h, loc)
	stopsAfterTheCopy(t, h, copies, loc)
}

// The agent can start a turn of its own while its held stop waits for the copy of the turn before
// it, and the copy's end then leaves the stop to this turn's end (endTurnCopyWait). The stall
// backstop over this turn replaces the process, which ends the hold (endHeld) while the copy still
// asks the process's shim. So the backstop leaves the turn alone while the stop waits for the
// copy, and the sweep's next post, once the copy has ended, ends the stalled turn as before. The
// bound the backstop carries is the daemon's to choose; a short one here reaches the wait.
//
// The control is the backstop acting whatever the stop waits for: the claim is then suspended at
// the first post, while the copy is still being taken, and the test fails there.
func TestTheStallBackstopWaitsForTheTurnsCopyItsHeldStopWaitsFor(t *testing.T) {
	h, copies, loc := copyWaiting(t)
	h.must(StreamTurnStart{Claim: h.token})
	h.stepped("bash", "toolu_own_turn_0", toolArgs, false)
	last := h.clock.Now()
	h.advance(2 * time.Minute)
	backstop := StallBackstop{Claim: h.token, Bound: time.Minute, Deadline: last.Add(time.Minute), Step: h.store.step(h.token)}

	h.must(backstop)

	h.wantState(StateWorking)
	h.wantCalls("Suspend", 0)
	if c := h.claim(); !c.SuspensionHeld || c.Locator == nil || *c.Locator != loc {
		t.Fatalf("the claim is %+v, want it working on its process %+v, holding the stop that waits for the copy", c, loc)
	}
	h.must(copies.copied(t, 0))
	h.wantState(StateWorking)
	h.wantCalls("Suspend", 0)

	h.must(backstop)

	h.wantState(StateSuspended)
	h.wantCalls("Suspend", 1)
}

// A registration on another plugin contract over a live claim relaunches its process onto this
// daemon's plugin (staleRegistration), which ends a held stop (endHeld) while the copy still asks
// the process's shim. While the stop waits for the copy the registration is refused as stale and
// nothing is relaunched: the copy's end runs the stop, which takes the stale process with it.
//
// The control is the relaunch made whatever the stop waits for: the claim is then suspended at the
// registration, while the copy is still being taken, and the test fails there.
func TestAStaleRegistrationWaitsForTheTurnsCopyItsHeldStopWaitsFor(t *testing.T) {
	h, copies, loc := copyWaiting(t)
	stale := h.claim().PluginContract
	h.deps.PluginContract, h.m.deps.PluginContract = refreshContract, refreshContract
	h.m.deps.PluginRelaunchable = true

	err := h.handle(RequestRegister{Claim: h.token, Generation: h.generation(), Session: session, SessionFile: sessionFile,
		CapabilityHash: []byte("capability"), PluginContract: stale})

	if err == nil {
		t.Fatal("the registration on another plugin contract was answered nil, want it refused as stale")
	}
	waitsOnItsProcess(t, h, loc)
	stopsAfterTheCopy(t, h, copies, loc)
}
