package supervise

// The wake kind the stale-plugin refresh lane parks under (WakePluginRefresh), and what every
// decision that reads a waiting wake makes of it. The lane's relaunch is a launch in everything
// it does — it waits at the same gate, it replaces the claim's process at its turn, it marks the
// claim where it starts the replacement — and the kind exists for one reader, outside the
// daemon: an operator reading `GET /legion/v1/state` through a plugin wave tells the lane's
// wakes from the launches that start a tree's work by it, which no reading of the wave of
// 2026-11-10 could do while the two were one kind (dispatch://INFRA-2712 R4).
//
// So these tests come in two halves. The first two say which lane parks which kind: the refresh
// pass's relaunch parks the lane's own, and the relaunch a stale registration raises keeps the
// launch kind, because that is one claim's own launch rather than the lane the gate bounds. The
// rest hold every decision that reads a waiting wake to answering of a parked refresh exactly
// what the test beside it holds that decision to for a parked launch (StartsProcess,
// launchWaits): the suspension its turn ends (wakeNow, endHeld), the compaction refused
// (TestACompactionAskedOverALaunchWaitingItsTurnIsNotTaken), the observation and the probe that
// decide nothing (TestASecondReportOfTheOneDeathOverAWaitingRelaunchChargesNothing,
// TestTheProcessAWaitingLaunchReplacesIsNotProbedInsideTheWait) and the parked launch an
// operator's own task runs where it arrives
// (TestAnOperatorsDeliveryOverAParkedLaunchRunsThatLaunchNow). The sixth reader, the pending
// delivery a waiting launch holds back, is already driven by this lane and names the kind there
// (TestATaskDeliveredWhileALaunchWaitsItsTurnWaitsForTheProcessThatLaunchStarts, sendPending).
//
// The negative control for all of the second half is the predicate narrowed to the launch kind
// (StartsProcess): every one of those decisions then reads a parked refresh as no launch at all
// — the suspension is not ended at its turn, the compaction is taken, the observation and the
// probe are acted on, and an operator's task waits out a paced turn — which is what each test
// below reads.

import (
	"errors"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// staleContract is the plugin contract a claim of the release before this daemon's records, and
// refreshContract the one this daemon speaks: the comparison the lane acts on (refreshPlugin).
const (
	staleContract   = PodUpdateContract
	refreshContract = PodUpdateContract + 1
)

// refreshParked is the window a plugin wave leaves most of a fleet in: a live claim whose plugin
// this daemon does not speak, with the lane's relaunch reserved and waiting its paced turn over
// the process the claim still runs (refreshPlugin, relaunchStale, parkWake). It answers the
// harness and the wake that waits.
//
// The walk to ready spends the limiter's burst, so the relaunch raised after it is one the
// pacing holds rather than one that runs where it is raised, which is the whole of what these
// tests are about. The claim keeps the process it records and the state it is in, because the
// park writes neither (parkWake). PluginRelaunchable is left as the harness leaves it, false:
// the lane's own pass never reads it — the daemon's pass and the registration path do
// (refreshStalePlugins, staleRegistration) — so a test that walks a relaunched claim back to
// ready has its registration accepted rather than refused for a contract this harness gives it
// no way to speak.
//
// rate is the pace it is parked at, which decides when the turn comes: one interval of that
// rate and the jitter's half-interval after the burst is spent (pacedDeps). Every test here
// takes the shipped rate but one, which needs its turn to fall inside another bound of the
// claim's (refreshWakeRate).
func refreshParked(t *testing.T) (*harness, WakeWaiting) {
	t.Helper()
	return refreshParkedAt(t, wakeRate)
}

// refreshWakeRate is twice the shipped rate, which puts the turn of a second wake 7.5s out
// rather than 11.25s: inside the stop deadline a held suspension is bounded by (testStop,
// holdSuspension), so a test of what the turn does to that hold reads the turn rather than the
// deadline that would otherwise have run first.
const refreshWakeRate = 2 * wakeRate

func refreshParkedAt(t *testing.T, rate int) (*harness, WakeWaiting) {
	t.Helper()
	h := pacedHarness(t, rate)
	h.reach(StateReady)
	h.m.deps.PluginContract = refreshContract

	h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})

	waiting := h.wantWaitingWake(WakePluginRefresh)
	if got := h.claim(); got.State != StateReady || got.Locator == nil {
		t.Fatalf("the claim whose refresh waits is %s recording %+v, want %s recording the process it still runs",
			got.State, got.Locator, StateReady)
	}
	return h, waiting
}

// The lane's relaunch waits under the lane's own kind, from the moment it is parked to the line
// its turn writes, and the turn it waits for replaces the process exactly as a launch's does.
// Both halves matter: the kind is what a reader of the daemon's state counts the lane by, and
// the line is what the operator counts a minute's wakes from (WakeWaiting, wakeRan).
func TestTheRefreshLanesRelaunchWaitsUnderTheLanesOwnWakeKind(t *testing.T) {
	h, waiting := refreshParked(t)
	replaced := h.locator()

	h.advance(waiting.At.Sub(h.clock.Now()))

	h.wantState(StateLaunching)
	h.wantStarts(h.token, 2)
	if resume := h.wantCalls("Resume", 1)[0]; resume.Previous == nil || *resume.Previous != replaced {
		t.Fatalf("the refresh's turn replaced %+v, want the process the gate left running, %+v", resume.Previous, replaced)
	}
	if got := h.claim(); got.PluginRelaunchFor != refreshContract {
		t.Fatalf("the claim after the refresh that ran is marked for %d, want %d", got.PluginRelaunchFor, refreshContract)
	}
	h.wantWakeLine(h.token, "kind=plugin-refresh", "waited=true", "waitedFor="+(wakeInterval+wakePart).String())
}

// The other caller of relaunchStale keeps the launch kind. A registration whose contract still
// differs from the daemon's is this claim's own launch, raised to answer the registration in
// front of it rather than to work through a fleet, and it is outside the lane the gate bounds
// (staleRegistration, RefreshGate): a reader counting what the refresh has left to do must not
// count it. Everything else about it is the lane's relaunch — it waits its paced turn over the
// process it replaces and marks the claim where it starts the replacement.
func TestAStaleRegistrationsRelaunchWaitsUnderTheLaunchKind(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	h.reach(StateReady)
	h.m.deps.PluginContract = refreshContract
	h.m.deps.PluginRelaunchable = true
	replaced := h.locator()

	err := h.handle(RequestRegister{Claim: h.token, Generation: h.generation(), Session: session,
		SessionFile: sessionFile, CapabilityHash: []byte("capability"), PluginContract: staleContract})

	var refusal claim.Refusal
	if !errors.As(err, &refusal) || refusal != claim.StalePluginContract(staleContract, refreshContract) {
		t.Fatalf("the stale registration = %v, want the refusal naming %d and %d", err, staleContract, refreshContract)
	}
	waiting := h.wantWaitingWake(WakeLaunch)

	h.advance(waiting.At.Sub(h.clock.Now()))

	h.wantStarts(h.token, 2)
	if resume := h.wantCalls("Resume", 1)[0]; resume.Previous == nil || *resume.Previous != replaced {
		t.Fatalf("the registration's relaunch replaced %+v, want the process it registered from, %+v", resume.Previous, replaced)
	}
	if lines := h.logs.lines("supervise: wake ran", "kind=plugin-refresh"); len(lines) != 0 {
		t.Fatalf("logged %v, want no refresh wake: this relaunch is the claim's own launch", lines)
	}
	h.wantWakeLine(h.token, "kind=launch", "waited=true", "waitedFor="+(wakeInterval+wakePart).String())
}

// A suspension held over a claim whose refresh waits is ended at that wake's turn, as one held
// over a waiting launch is (wakeNow, endHeld): the process the refresh would have replaced is
// stopped by the suspension instead, and nothing is launched in its place. The claim's next
// resume brings it up on this daemon's plugin by itself, which is the whole of what the refresh
// was for, so nothing of the lane is owed afterwards.
//
// The pace is twice the shipped one so that the turn comes before the hold's own stop deadline
// (refreshWakeRate, testStop): that deadline would suspend the claim by itself a few seconds
// later, and a test that let it fire would read the same claim whatever the turn did.
func TestASuspensionHeldOverTheRefreshsRelaunchEndsAtItsTurn(t *testing.T) {
	h, waiting := refreshParkedAt(t, refreshWakeRate)
	stale := h.locator()
	// A turn of the agent's own is what makes the suspension one the machine holds rather than
	// one it runs where it arrives (holdSuspension).
	h.must(StreamTurnStart{Claim: h.token})
	if err := h.handle(RequestSuspend{Claim: h.token, Reason: "the operator is draining the daemon", By: SuspendedByOperator}); !suspensionHeld(err) {
		t.Fatalf("the suspend of a claim in a turn answered %v, want the hold for the turn's end", err)
	}
	if wait := waiting.At.Sub(h.clock.Now()); wait >= testStop {
		t.Fatalf("the refresh's turn is %s away and the hold's own deadline %s, want the turn first: this test would read the deadline", wait, testStop)
	}

	h.advance(waiting.At.Sub(h.clock.Now()))

	got := h.claim()
	if got.State != StateSuspended || got.SuspendedBy != SuspendedByOperator || got.Locator != nil {
		t.Fatalf("after the refresh's turn the claim is %s by %q recording %+v, want it suspended by the operator with its process let go",
			got.State, got.SuspendedBy, got.Locator)
	}
	if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != stale {
		t.Fatalf("the turn suspended %+v, want the process the refresh was to replace, %+v", suspend.Locator, stale)
	}
	h.wantStarts(h.token, 1)
	if held := h.m.View().WakeWaiting; held != nil {
		t.Fatalf("the claim names wake %+v, want none: its turn has run", held)
	}
	if got.PluginRelaunchFor != 0 {
		t.Fatalf("the suspended claim is marked for %d, want 0: the refresh replaced no process", got.PluginRelaunchFor)
	}
}

// A compaction asked for while the refresh's relaunch waits is a take that did not happen, as
// one asked over a waiting launch is (compactStart): that relaunch lets the process go at its
// turn, so there is nothing of its session worth ten minutes of a model. The claim is left
// exactly where the request found it, with its process and its turn in the pacing.
func TestACompactionAskedOverTheRefreshsWaitingRelaunchIsNotTaken(t *testing.T) {
	h, waiting := refreshParked(t)
	stale := h.locator()

	err := h.m.Compact(h.ctx, testCompactInstructions)

	if !errors.Is(err, ErrCompactNotTaken) {
		t.Fatalf("a compaction asked over the refresh's waiting relaunch answered %v, want ErrCompactNotTaken", err)
	}
	h.m.Wait()
	if asked := h.conn.Compactions(); len(asked) != 0 {
		t.Fatalf("Oh My Pi was asked %+v, want nothing: the claim was never taken", asked)
	}
	h.wantState(StateReady)
	if h.m.View().Claim.Compacting {
		t.Fatal("the claim reports itself being compacted, want no compaction: the take did not happen")
	}
	if got := h.claim().Locator; got == nil || *got != stale {
		t.Fatalf("the claim records %+v, want the process the refresh replaces, %+v", got, stale)
	}
	h.wantWakeDueAt(waiting)
}

// An observation of the process the refresh's relaunch replaces decides nothing, as one of the
// process a waiting launch replaces decides nothing (judge): what becomes of that process is
// already settled, and the refresh's turn replaces it. Acted on, the death here would charge a
// launch failure and send this claim to the back of the queue with a fresh token, which in a
// wave is the claim failing before its launch budget is spent and waiting another paced turn to
// do it.
func TestAnObservationOfTheProcessTheRefreshsRelaunchReplacesDecidesNothing(t *testing.T) {
	h, waiting := refreshParked(t)
	stale := h.locator()

	h.observe(runtime.Gone)

	h.wantBudgets(Budgets{})
	h.wantState(StateReady)
	h.wantStarts(h.token, 1)
	if got := h.claim().Locator; got == nil || *got != stale {
		t.Fatalf("the claim records %+v, want the process the refresh replaces, %+v", got, stale)
	}
	h.wantWakeDueAt(waiting)
}

// The process the refresh's relaunch replaces is not probed either, as the process a waiting
// launch replaces is not (probe): the closed connection asks the runtime nothing, and the probe
// interval the park disarmed is not armed again inside a wait that is tens of minutes long in a
// wave. Every answer the runtime could give is one the refresh's turn already answers.
func TestTheProcessTheRefreshsRelaunchReplacesIsNotProbed(t *testing.T) {
	h, waiting := refreshParked(t)
	// The runtime would answer that it cannot say what became of the pod, which is the answer
	// that arms the probe interval.
	h.rt.ScriptProbe(fake.ProbeResult{Err: errors.New("the node the pod ran on is unreachable")})

	h.must(StreamClosed{Claim: h.token})

	h.wantCalls("Probe", 0)
	if _, armed := h.m.timers[TimerProbe]; armed {
		t.Fatal("the closed connection armed the probe interval inside the wait, want nothing of the process the refresh replaces")
	}
	h.wantBudgets(Budgets{})
	h.wantWakeDueAt(waiting)
}

// An operator's own task over the refresh's parked relaunch runs that relaunch where the task
// arrives, as it runs a parked launch (runParkedLaunchNow): a person is waiting on the task, and
// in a wave the refresh's turn is tens of minutes away. The task is queued for the process the
// relaunch starts, which is where every task held back by a waiting launch goes, and the line
// the relaunch writes still names the lane it belongs to and says the pacing did not decide when
// it ran.
func TestAnOperatorsDeliveryOverTheRefreshsParkedRelaunchRunsItNow(t *testing.T) {
	const task = "look at the production check"
	h, _ := refreshParked(t)
	replaced := h.locator()

	// The task arrives part of the way into the wait, as a person's does.
	h.advance(wakePart)
	h.must(RequestDeliver{Claim: h.token, Task: task})

	h.wantPrompts(0)
	h.wantState(StateLaunching)
	h.wantStarts(h.token, 2)
	if resume := h.wantCalls("Resume", 1)[0]; resume.Previous == nil || *resume.Previous != replaced {
		t.Fatalf("the relaunch the operator's task ran replaced %+v, want the process the gate left running, %+v",
			resume.Previous, replaced)
	}
	if held := h.m.View().WakeWaiting; held != nil {
		t.Fatalf("the claim names wake %+v, want none: an operator's task waits for no turn", held)
	}
	if p := h.pending(); p.Task != task || !p.DeliveredAt.IsZero() {
		t.Fatalf("the task is %+v, want %q queued unread: it goes to the process this relaunch starts", p, task)
	}
	h.wantWakeLine(h.token, "kind=plugin-refresh", "waited=true", "waitedFor="+wakePart.String(), "paced=false")

	h.relaunched()

	if sent := h.wantPrompts(1); sent[0].Message != task {
		t.Fatalf("the prompts are %+v, want the operator's task at the process the relaunch started", sent)
	}
}
