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
// The walk to the state asked for spends the limiter's burst, so the relaunch raised after it
// is one the pacing holds rather than one that runs where it is raised, which is the whole of
// what these tests are about. The claim keeps the process it records and the state it is in,
// because the park writes neither (parkWake). PluginRelaunchable is left as the harness leaves
// it, false: the lane's own pass never reads it — the daemon's pass and the registration path
// do (refreshStalePlugins, staleRegistration) — so a test that walks a relaunched claim back to
// ready has its registration accepted rather than refused for a contract this harness gives it
// no way to speak.
//
// rate is the pace it is parked at, which decides when the turn comes: one interval of that
// rate and the jitter's half-interval after the burst is spent (pacedDeps). Every test here
// takes the shipped rate but one, which needs its turn to fall inside another bound of the
// claim's (refreshWakeRate).
func refreshParked(t *testing.T) (*harness, WakeWaiting) {
	t.Helper()
	return refreshParkedAt(t, wakeRate, StateReady)
}

// refreshParkedIdle is that same window over a claim whose turn has ended, which is the state
// the lane's own pass finds most of a fleet in: a claim in a turn is held back for its end
// (holdStaleRelaunch), so what a sweep posts and the gate admits is mostly idle claims.
func refreshParkedIdle(t *testing.T) (*harness, WakeWaiting) {
	t.Helper()
	return refreshParkedAt(t, wakeRate, StateIdle)
}

// refreshWakeRate is twice the shipped rate, which puts the turn of a second wake 7.5s out
// rather than 11.25s: inside the stop deadline a held suspension is bounded by (testStop,
// holdSuspension), so a test of what the turn does to that hold reads the turn rather than the
// deadline that would otherwise have run first.
const refreshWakeRate = 2 * wakeRate

func refreshParkedAt(t *testing.T, rate int, state ClaimState) (*harness, WakeWaiting) {
	t.Helper()
	h := pacedHarness(t, rate)
	h.reach(state)
	h.m.deps.PluginContract = refreshContract

	h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})

	waiting := h.wantWaitingWake(WakePluginRefresh)
	if got := h.claim(); got.State != state || got.Locator == nil {
		t.Fatalf("the claim whose refresh waits is %s recording %+v, want %s recording the process it still runs",
			got.State, got.Locator, state)
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
	h, waiting := refreshParkedAt(t, refreshWakeRate, StateReady)
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
	if p := h.pending(); p.Task != task || p.taken() {
		t.Fatalf("the task is %+v, want %q untaken: it goes to the process this relaunch starts", p, task)
	}
	h.wantWakeLine(h.token, "kind=plugin-refresh", "waited=true", "waitedFor="+wakePart.String(), "paced=false")

	h.relaunched()

	if sent := h.wantPrompts(1); sent[0].Message != task {
		t.Fatalf("the prompts are %+v, want the operator's task at the process the relaunch started", sent)
	}
}

// The retry a refused refresh leaves waiting is still the lane's. A wake whose turn the
// lifecycle recheck cannot be answered for starts nothing and leaves the launch owing, retried
// at the probe interval from the top (runWake, relaunchRefused, relaunchLater), and that retry
// re-enters the gate: the intent the wake carried goes back to the machine for it, so the wake
// it parks waits under the lane's own kind rather than under the launch kind every other launch
// takes (retryHolding, reserveWake). Counted by its kind — which is the whole reason the kind
// exists — a lane that lost its refreshes to `launch` there would under-count exactly the
// claims whose refresh is in trouble, while the count of what the lane has left still named
// them, and the two numbers an operator reads a wave by would disagree in the one case they are
// read for (dispatch://INFRA-2712 R4, api.State.ClaimsToRefresh). The mark follows the intent
// and is written by the launch that starts the process replacing the stale one, and by nothing
// before it: the refusal marks nothing, and the retry's own launch marks the claim as it starts
// (launchNow, commitPluginRelaunch).
//
// The refusal leaves a retry only over a claim that records no process and is live
// (relaunchRefused's first branch, which hands the error back for every other claim), and the
// refresh pass itself parks no refresh that way: the lane relaunches a claim that holds a
// capability, so it records its process, and every path that lets a process go drops the wake
// with it (letGo, refreshPlugin). So the claim is put in that window here rather than driven
// into it. The road that does drive a refresh wake over a claim recording no process is the
// retry of a launch whose own write failed, which the case below drives from end to end
// (TestTheRetryOfARefreshWhoseLaunchWriteFailedWaitsUnderTheLanesOwnKind); what this case
// holds is the same rule met at the turn's own refusal — a retry of this lane's launch is this
// lane's.
//
// The negative control is the wrapping reverted (runWake handed waiting.again itself): the
// retry then parks under `launch`, which this test reads at the kind of the wake waiting.
func TestTheRetryOfARefusedRefreshWaitsUnderTheLanesOwnKind(t *testing.T) {
	h, waiting := refreshParked(t)
	store := &unansweredCheckLaunchStore{memStore: h.store, failing: true}
	h.m.deps.Store = store
	// The window the refusal's retry answers: the claim live with no process of its own, which
	// is where a park that launched nothing leaves one (parkWake).
	h.m.claim.State, h.m.claim.Locator = StateLaunching, nil

	h.advance(waiting.At.Sub(h.clock.Now()))

	h.wantStarts(h.token, 1)
	if held := h.m.View().WakeWaiting; held != nil {
		t.Fatalf("the refused refresh names wake %+v, want none: its wake is spent and its retry waits on the probe interval", held)
	}
	if got := h.claim().PluginRelaunchFor; got != 0 {
		t.Fatalf("the claim after the refused refresh is marked for %d, want no mark: the refusal started no process", got)
	}

	// The store answers again and the retry runs, with a wave of a daemon's own claims in the
	// queue ahead of it: the retry is a launch from the top and reserves afresh (launchAfter),
	// so it waits a turn of its own, which is the wake this case reads the kind of. Without the
	// queue the limiter has caught up over the probe interval and the retry runs where it is
	// raised, which is no park at all.
	store.failing = false
	queueAhead(h, 4)
	h.advance(testProbe)

	retry := h.wantWaitingWake(WakePluginRefresh)
	if got := h.claim().PluginRelaunchFor; got != 0 {
		t.Fatalf("the claim whose retry waits its turn is marked for %d, want no mark until that turn starts the process", got)
	}

	h.advance(retry.At.Sub(h.clock.Now()))

	h.wantStarts(h.token, 2)
	if got := h.claim().PluginRelaunchFor; got != refreshContract {
		t.Fatalf("the claim after the retry that ran is marked for %d, want %d: the retry is the relaunch this lane owed", got, refreshContract)
	}
	h.wantWakeLine(h.token, "kind=plugin-refresh", "waited=true")
}

// wantRetryRunsUnderTheLanesKind is the tail both roads to a retry of this lane end in, from the
// store answering again to the mark the retry that ran wrote. A wave of a daemon's own claims is
// queued ahead of the retry, so it waits a turn of its own, which is the wake the kind is read
// off: without the queue the limiter has caught up over the probe interval and the retry runs
// where it is raised, which is no park at all. That turn starts the process the relaunch owed
// and marks the claim for this daemon's contract, and the lines this claim's wakes wrote name
// the lane for both of its turns, the one launch-kind wake being the first launch of the walk to
// idle.
//
// which names the retry, for the lines a failure of either road writes.
func (h *harness) wantRetryRunsUnderTheLanesKind(which string) {
	h.t.Helper()
	queueAhead(h, 4)
	h.advance(testProbe)

	retry := h.wantWaitingWake(WakePluginRefresh)
	h.wantStarts(h.token, 1)

	h.advance(retry.At.Sub(h.clock.Now()))

	h.wantStarts(h.token, 2)
	if got := h.store.load(h.token).PluginRelaunchFor; got != refreshContract {
		h.t.Fatalf("the claim stored after %s is marked for %d, want %d: the retry is the relaunch this lane owed",
			which, got, refreshContract)
	}
	if lines := h.logs.lines(WakeRanMessage, "claim="+string(h.token), "kind=plugin-refresh"); len(lines) != 2 {
		h.t.Fatalf("logged %v, want the lane's own line from the turn whose write failed and from the turn of %s", lines, which)
	}
	if lines := h.logs.lines(WakeRanMessage, "claim="+string(h.token), "kind=launch"); len(lines) != 1 {
		h.t.Fatalf("logged %v, want the one launch-kind wake of this claim, its own first launch in the walk to idle: every wake of it since is this lane's", lines)
	}
}

// refreshRetryArmed is the opening every road to a retry of this lane takes: the lane's relaunch
// parked over a stale idle claim, its turn met by a store that takes no claim write, and the
// retry that failed write arms waiting on the probe interval over a claim recording no process
// (refreshParkedIdle, launchNow, relaunchLater). That retry holds the intent the launch had
// already committed, which is what each road below reads at the wake its own retry parks
// (retryHolding). The write lands again before this returns, so what the retry meets when it
// runs is each road's own to set up.
//
// It answers the harness and the stale process that launch let go, for the road that reads what
// was stopped.
func refreshRetryArmed(t *testing.T) (*harness, runtime.Locator) {
	t.Helper()
	h, waiting := refreshParkedIdle(t)
	stale := h.locator()
	// From here the store takes no claim write, so the launch the turn runs records nothing.
	h.store.fail("PutClaim", errBoom)

	h.advance(waiting.At.Sub(h.clock.Now()))

	if c := h.claim(); c.State != StateLaunching || c.Locator != nil {
		t.Fatalf("the launch whose write failed left the claim %s recording %+v, want %s recording no process",
			c.State, c.Locator, StateLaunching)
	}
	if _, ok := h.m.timers[TimerRelaunch]; !ok {
		t.Fatalf("the launch whose write failed armed %v, want its retry on the probe interval", h.m.timers)
	}
	h.store.fail("PutClaim", nil)
	return h, stale
}

// The retry a launch's own failed write leaves waiting is the lane's too, and this is the road
// the machine drives to it. The refresh's turn passes its lifecycle recheck, lets the stale
// process go, and the claim write that records StateLaunching fails: the launch has started
// nothing, it keeps in memory the mark it had already committed, and it tries the same launch
// again at the probe interval (launchNow, relaunchLater). That retry is a launch from the top
// and reserves afresh, and the intent the failed launch took off the machine goes back on it
// where the retry runs, so the wake it parks is the lane's rather than a plain launch
// (retryHolding, reserveWake). Counted by its kind, a refresh that lost its wake to `launch`
// here is a claim the lane is still working through that an operator reading the wave cannot
// see it working through, which is the one disagreement the kind exists to prevent
// (dispatch://INFRA-2712 R4, api.State.ClaimsToRefresh).
//
// The wake that retry parks waits over a claim that records no process, which is the window
// the case above can only put a claim in: this one reaches it from a stale idle claim and the
// lane's own pass.
//
// The negative control is the wrapping reverted (launchNow handing relaunchLater the bare
// closure): the retry then parks under `launch`, which this case reads at the kind of the wake
// waiting and again at the lines its turns write.
func TestTheRetryOfARefreshWhoseLaunchWriteFailedWaitsUnderTheLanesOwnKind(t *testing.T) {
	h, stale := refreshRetryArmed(t)

	h.wantWakeLine(h.token, "kind=plugin-refresh", "waited=true")
	h.wantStarts(h.token, 1)
	if stops := h.calls("Suspend"); len(stops) == 0 || stops[len(stops)-1].Locator != stale {
		t.Fatalf("stopped %+v, want the stale process this launch let go, %+v", stops, stale)
	}

	// The retry that failure armed runs as the lane's own (wantRetryRunsUnderTheLanesKind).
	h.wantRetryRunsUnderTheLanesKind("the retry that ran")
}

// And when that retry's own lifecycle recheck refuses, the machine is left holding no intent at
// all: the retry put it back for the launch it was raising, and the decision that met the
// refusal ended with it, which is the rule every decision of a machine ends on (endRefreshLane,
// Machine.unlock). So the next launch of this claim is counted as the launch it is, and not as
// one of the lane's. An intent left standing would mark that launch for a contract no relaunch
// of it was made for and count it into the lane an operator reads the wave by
// (commitPluginRelaunch, dispatch://INFRA-2712 R4), and the relaunch the lane still owes is not
// lost by that: the next sweep reads the claim stale and posts it again (refreshStalePlugins).
//
// The refusal here is the tree's cleanup reserved, which suspends the claim for the daemon
// (relaunchRefused), and the launch that follows is the daemon's own resume by rule once that
// cleanup has cleared. An operator's resume is the same launch and reads the same, but it is
// exempt from the pacing and so parks nothing (exemptWake): a paced one is taken here because
// it parks a wake, and this case reads the kind of that wake as well as the kind of the line
// its turn writes.
//
// The negative control is the intent left standing at the end of the decision (endRefreshLane's
// last line removed): the resume's wake then parks under `plugin-refresh` and its line says the
// lane ran a refresh it never raised.
func TestTheLaunchAfterARefreshRetryTheLifecycleRefusedIsCountedAsNoRefresh(t *testing.T) {
	h, _ := refreshRetryArmed(t)
	// The tree's cleanup is reserved by the time the retry rechecks the lifecycle, which is the
	// refusal that suspends the claim for the tree's close.
	store := &refusableCheckLaunchStore{memStore: h.store, refuse: true}
	h.m.deps.Store = store

	h.advance(testProbe)

	if c := h.claim(); c.State != StateSuspended || c.SuspendedBy != SuspendedByDaemon {
		t.Fatalf("the refused retry left the claim %s suspended by %q, want %s by %q: the tree's close retires it",
			c.State, c.SuspendedBy, StateSuspended, SuspendedByDaemon)
	}
	if intent := h.m.pluginRelaunch; intent != nil {
		t.Fatalf("the machine holds the relaunch intent %+v after the refused retry, want none: nothing of this lane outlives the decision that held it", *intent)
	}

	// The cleanup clears and the claim is launched again, by the rule that says it owns its
	// issue's phase, with a wave in the queue ahead of it so that this launch waits a turn.
	store.refuse = false
	queueAhead(h, 4)
	h.must(RequestRuleResume{Claim: h.token, Reason: "it owns its issue's phase"})

	resumed := h.wantWaitingWake(WakeLaunch)

	h.advance(resumed.At.Sub(h.clock.Now()))

	h.wantStarts(h.token, 2)
	h.wantWakeLine(h.token, "kind=launch", "waited=true")
	if lines := h.logs.lines(WakeRanMessage, "claim="+string(h.token), "kind=plugin-refresh"); len(lines) != 1 {
		t.Fatalf("logged %v, want the one line of the refresh turn whose write failed: the resume's launch is none of this lane's", lines)
	}
}

// A retry of this lane the store refuses a second time is the lane's still. The refusal here is
// the other half of the one above — a store that did not answer rather than the tree's reserved
// cleanup — and it ends nothing: the claim is live recording no process, so the refusal leaves
// a second retry waiting on the probe interval and the relaunch is still owed
// (relaunchRefused's last branch, relaunchLater). That second retry carries the intent the
// first one put back, inside a closure of its own, which is the only way an intent crosses a
// decision at all: the decision that met the refusal dropped whatever the machine held
// (retryHolding, endRefreshLane).
//
// The road is the one the case above drives: the refresh's turn runs, the claim write that
// records StateLaunching fails, and the retry that failure arms puts the intent back and raises
// the same launch anew from the top, where its own lifecycle check is refused (launchNow,
// launchAfter).
//
// A refresh that lost its wake to `launch` at this second refusal is lost to the operator
// twice over: the line that wake writes and the kind a reader counts the lane by both say
// `launch`, while the lane's own count still names the claim, and the correction the sweep
// makes of the gate is taken from the refresh wakes the machines publish, so a wake parked
// under `launch` is never counted against the gate either (dispatch://INFRA-2712 R4,
// api.State.ClaimsToRefresh, refreshStalePlugins in internal/daemon).
//
// The negative control is the wrapping reverted (relaunchRefused arming the bare relaunch it
// was handed): the second retry then parks under `launch`, which this case reads at the kind of
// the wake waiting and again at the lines its turns write.
func TestTheRetryOfARefreshRefusedASecondTimeWaitsUnderTheLanesOwnKind(t *testing.T) {
	h, _ := refreshRetryArmed(t)
	// The store cannot answer the lifecycle check the retry makes: the refusal that leaves the
	// claim as it found it and arms a second retry.
	store := &unansweredCheckLaunchStore{memStore: h.store, failing: true}
	h.m.deps.Store = store

	h.advance(testProbe)

	h.wantStarts(h.token, 1)
	if c := h.claim(); c.State != StateLaunching || c.Locator != nil {
		t.Fatalf("the refused retry left the claim %s recording %+v, want %s recording no process",
			c.State, c.Locator, StateLaunching)
	}
	if _, ok := h.m.timers[TimerRelaunch]; !ok {
		t.Fatalf("the refused retry armed %v, want the second retry on the probe interval", h.m.timers)
	}
	if held := h.m.View().WakeWaiting; held != nil {
		t.Fatalf("the refused retry names wake %+v, want none: a refused launch raises none", held)
	}

	// The store answers again, and the second retry runs as the lane's own
	// (wantRetryRunsUnderTheLanesKind).
	store.failing = false
	h.wantRetryRunsUnderTheLanesKind("the second retry that ran")
}

// The other road to that second refusal is a refused turn's own retry, and this case drives it
// from end to end. The retry the failed write armed parks a wake of this lane over the claim
// that write left recording no process, the store cannot answer the lifecycle check that wake's
// turn makes, and the retry that refusal arms is refused in its turn at the launch it raises
// (runWake, relaunchRefused, launchAfter). The second retry carries the intent in a closure of
// its own, as the case above states (retryHolding, endRefreshLane).
//
// What both cases hold is that a retry of this lane refused again is this lane's still,
// whichever refusal armed the retry that met the second one — and a refresh that lost its wake
// to `launch` here is lost to the operator the same way the case above states.
//
// The negative control is the wrapping reverted (relaunchRefused arming the bare relaunch it
// was handed): the second retry then parks under `launch`, which this case reads at the kind of
// the wake waiting and again at the lines its turns write.
func TestTheRetryOfARefusedRefreshTurnRefusedAgainWaitsUnderTheLanesOwnKind(t *testing.T) {
	h, _ := refreshRetryArmed(t)
	// A wave of a daemon's own claims is queued ahead of the first retry, so the launch it
	// raises waits a turn of its own: the refresh wake over a claim recording no process that
	// this road is driven through (launchAfter, parkWake).
	queueAhead(h, 4)

	h.advance(testProbe)

	parked := h.wantWaitingWake(WakePluginRefresh)

	// The store cannot answer the lifecycle check that wake's turn makes: the turn starts
	// nothing and the retry it arms holds the intent the wake carried (runWake, retryHolding).
	store := &unansweredCheckLaunchStore{memStore: h.store, failing: true}
	h.m.deps.Store = store

	h.advance(parked.At.Sub(h.clock.Now()))

	h.wantStarts(h.token, 1)
	if held := h.m.View().WakeWaiting; held != nil {
		t.Fatalf("the refused turn names wake %+v, want none: its wake is spent and its retry waits on the probe interval", held)
	}
	if _, ok := h.m.timers[TimerRelaunch]; !ok {
		t.Fatalf("the refused turn armed %v, want its retry on the probe interval", h.m.timers)
	}

	// That retry puts the intent back and raises the same launch anew from the top, whose own
	// lifecycle check the store refuses too: the second refusal, which arms the retry this case
	// reads (launchAfter, relaunchRefused).
	h.advance(testProbe)

	h.wantStarts(h.token, 1)
	if c := h.claim(); c.State != StateLaunching || c.Locator != nil {
		t.Fatalf("the retry refused again left the claim %s recording %+v, want %s recording no process",
			c.State, c.Locator, StateLaunching)
	}
	if _, ok := h.m.timers[TimerRelaunch]; !ok {
		t.Fatalf("the retry refused again armed %v, want the second retry on the probe interval", h.m.timers)
	}
	if intent := h.m.pluginRelaunch; intent != nil {
		t.Fatalf("the machine holds the relaunch intent %+v after the second refusal, want none: the retry carries it in a closure of its own", *intent)
	}

	// The store answers again, and the second retry runs as the lane's own
	// (wantRetryRunsUnderTheLanesKind).
	store.failing = false
	h.wantRetryRunsUnderTheLanesKind("the second retry that ran")
}
