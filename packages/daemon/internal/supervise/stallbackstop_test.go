package supervise

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// backstopBound is the bound the daemon's sweep would hand this claim's role: four hours for an
// implementer, the role the harness drives (config.StallCallBoundFor), plus the grace and the
// two step gaps. Nothing here reads the configuration — the sweep resolves the bound and the
// request carries it — so the number is written out.
const backstopBound = 4*time.Hour + 5*time.Minute + 2*2*time.Minute

// backstop is the request the daemon's sweep makes for this claim: the role's bound, the
// deadline it computed from the record it read, which is already past when it posts, and that
// record itself, which is the store's own as the sweep read it there.
func (h *harness) backstop(last time.Time) StallBackstop {
	return h.backstopOf(last, h.store.step(h.token))
}

// backstopOf is backstop over a record of the caller's, for a case about the record the request
// carries rather than the one the store happens to hold.
func (h *harness) backstopOf(last time.Time, step StepRecord) StallBackstop {
	return StallBackstop{Claim: h.token, Bound: backstopBound, Deadline: last.Add(backstopBound), Step: step}
}

// backstopReports is a harness whose machine's reports of the turns it ended are collected
// rather than told (Deps.StallBackstopped), over the claim the walks drive: what the daemon
// writes the owner's notice from.
func backstopReports(t *testing.T) (*harness, *[]StallBackstopStop) {
	t.Helper()
	h := newBareHarness(t)
	stops := &[]StallBackstopStop{}
	h.deps.StallBackstopped = func(_ Claim, stop StallBackstopStop) { *stops = append(*stops, stop) }
	c := queuedClaim()
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	return h, stops
}

// workingOnATask walks the claim to a turn of a delivered task and reports the id of the prompt
// that turn is running, which is the task the claim is serving.
func (h *harness) workingOnATask(task string) string {
	h.t.Helper()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: h.token, Task: task})
	h.must(StreamTurnStart{Claim: h.token})
	h.wantState(StateWorking)
	return h.wantPrompts(1)[0].DeliveryID
}

// A claim in a turn whose recorded last step is older than its role's bound has that turn ended
// by the daemon: the process is replaced, the session is resumed on the new one, and the task it
// was serving is still owed to it — taken back unconfirmed and marked interrupted, kept in the
// store, and sent again under a new id once the replacement is ready. Nothing is charged: no
// launch failure, and no death with work outstanding, which is what would fail the claim at the
// launch budget if a stall repeated.
func TestTheBackstopEndsAStalledTurnAndLeavesTheTaskOwed(t *testing.T) {
	h := newHarness(t)
	sent := h.workingOnATask("implement the plan")
	h.advance(time.Minute)
	h.stepped("bash", "toolu_turn1_0", toolArgs, false)
	last := h.clock.Now()
	h.advance(backstopBound + time.Minute)

	h.must(h.backstop(last))

	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{})
	if p := h.pending(); !p.Interrupted || p.ID == sent || !p.ConfirmedAt.IsZero() {
		t.Fatalf("pending after the backstop = %+v, want the task taken back, unconfirmed and interrupted", p)
	}
	stored := h.store.load(h.token)
	if stored.Pending == nil || stored.ServingGeneration != 0 {
		t.Fatalf("stored claim = %+v, want the task kept and no run served", stored)
	}
	if resumes := h.wantCalls("Resume", 1); resumes[0].Spec.ResumeSessionFile != sessionFile {
		t.Fatalf("resumed %q, want the claim's own session %q", resumes[0].Spec.ResumeSessionFile, sessionFile)
	}

	h.relaunched()
	if resent := h.wantPrompts(2)[1]; resent.DeliveryID == sent {
		t.Fatalf("re-sent %+v under the id the stalled turn ran under", resent)
	}
	if h.store.load(h.token).Session != session {
		t.Fatalf("session after the backstop = %q, want the same session brought back", h.store.load(h.token).Session)
	}
}

// A claim whose agent has sent no tool frame at all since its turn began is judged from the
// turn's own start, which is the only thing the record holds for it. A claim whose record holds
// neither — no step and no turn start, as a claim whose machine has read nothing has — is judged
// from nothing and left alone: this path guesses at no time of its own.
func TestTheBackstopJudgesAClaimThatHasNotSteppedFromItsTurnsStart(t *testing.T) {
	h := newHarness(t)
	h.workingOnATask("implement the plan")
	started := h.clock.Now()
	if step := h.store.step(h.token); !step.TurnStartedAt.Equal(started) || !step.At.IsZero() {
		t.Fatalf("the record of a claim that has stepped none = %+v, want the turn's start alone", step)
	}
	h.advance(backstopBound + time.Minute)

	h.must(h.backstop(started))

	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{})

	// The same claim with nothing recorded at all: its machine knows of no step and its claim
	// carries none, so the backstop reads no moment to measure and the turn stands.
	bare := newHarness(t)
	bare.workingOnATask("implement the plan")
	bare.m.steppedAt = time.Time{}
	bare.m.claim.Step = StepRecord{}
	bare.advance(backstopBound + time.Minute)

	bare.must(bare.backstop(bare.clock.Now().Add(-backstopBound - time.Minute)))

	bare.wantState(StateWorking)
	bare.wantCalls("Resume", 0)
}

// The negative control: an agent that is sending tool frames is never touched, however long its
// turn runs. The sweep reads the claims before the machine decides, so the fence is the
// machine's own: a frame that arrived after that read is in the machine's inbox ahead of the
// backstop's event, and the turn it belongs to is left exactly as it stands.
func TestTheBackstopLeavesAClaimWhoseRecordMovedInsideItsBound(t *testing.T) {
	h := newHarness(t)
	h.workingOnATask("implement the plan")
	last := h.clock.Now()
	for range 4 {
		h.advance(backstopBound - time.Minute)
		h.stepped("bash", "toolu_turn1_0", toolArgs, false)
	}
	h.advance(time.Minute)

	// The sweep's own read was stale by one frame: the agent stepped again between that read and
	// this decision.
	h.must(h.backstop(last))

	h.wantState(StateWorking)
	h.wantCalls("Resume", 0)
	h.wantCalls("Suspend", 0)
	h.wantBudgets(Budgets{})
	if p := h.pending(); p.Interrupted {
		t.Fatalf("pending after a record that moved = %+v, want the turn untouched", p)
	}
}

// The backstop acts on its own deadline rather than waiting for the turn to end, which is what
// every other mid-turn stop of the daemon's does: a suspension that arrives mid-turn is held for
// that turn (holdSuspension, whose deadline is what bounds the wait), and the turn this exists
// for is one that never ends. The same claim, in the same state, answers the two differently.
func TestTheBackstopDoesNotWaitForATurnThatNeverEnds(t *testing.T) {
	held := newHarness(t)
	held.workingOnATask("implement the plan")
	held.advance(backstopBound + time.Minute)
	if err := held.handle(RequestSuspend{Claim: held.token, By: SuspendedByDaemon, Reason: "the issue closed"}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("a suspension mid-turn = %v, want it held for the turn", err)
	}
	held.wantState(StateWorking)

	h := newHarness(t)
	started := h.clock.Now()
	h.workingOnATask("implement the plan")
	h.advance(backstopBound + time.Minute)
	request := h.backstop(started)
	if !request.Deadline.Before(h.clock.Now()) {
		t.Fatalf("the request's deadline is %s, want a moment already past at %s", request.Deadline, h.clock.Now())
	}

	h.must(request)

	// No turn ended, and no timer of this machine's was waited on: the stop was made where the
	// request arrived.
	h.wantState(StateLaunching)
	h.wantCalls("Resume", 1)
}

// A stall that repeats leaves the claim relaunchable rather than failed. The worker's own ladder
// ends its process at most twice (the plugin's stall recovery), and each of those is a death
// with work outstanding, which is charged; the daemon's backstop is the third stop, and it
// charges nothing, so the claim comes back instead of spending the last of a launch budget whose
// default is three.
func TestTheBackstopChargesNothingAfterTheWorkersOwnTwoExits(t *testing.T) {
	h := newHarness(t)
	h.workingOnATask("implement the plan")

	// The worker's two exits, as the daemon reads them: a process gone with the task still
	// outstanding, each charged a death and relaunched.
	for exit := range 2 {
		h.observe(runtime.Gone)
		h.wantState(StateLaunching)
		if got := h.budgets().Deaths; got != exit+1 {
			t.Fatalf("deaths after exit %d = %d, want %d", exit+1, got, exit+1)
		}
		h.relaunched()
		h.must(StreamTurnStart{Claim: h.token})
	}
	before := h.budgets()
	last := h.clock.Now()
	h.advance(backstopBound + time.Minute)

	h.must(h.backstop(last))

	h.wantState(StateLaunching)
	if got := h.budgets(); got != before {
		t.Fatalf("budgets after the backstop = %+v, want them unchanged at %+v: the stop charges nothing", got, before)
	}
	if got := h.budgets().Deaths; got != 2 {
		t.Fatalf("deaths after the backstop = %d, want the worker's own two", got)
	}
	h.relaunched()
	h.wantState(StateReady)
}

// A suspension already held for the stalled turn ends in a suspension rather than a relaunch, as
// it does for every other replacement the daemon makes (replaceProcess, endHeld): the process the
// backstop would have replaced is stopped, nothing is launched in its place, and the suspension
// decides what follows.
func TestTheBackstopWithAHeldSuspensionSuspendsTheClaim(t *testing.T) {
	h := newHarness(t)
	h.workingOnATask("implement the plan")
	last := h.clock.Now()
	loc := h.locator()
	if err := h.handle(RequestSuspend{Claim: h.token, By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("suspend mid-turn = %v, want ErrSuspendHeld", err)
	}
	h.advance(backstopBound + time.Minute)

	h.must(h.backstop(last))

	h.wantState(StateSuspended)
	h.wantBudgets(Budgets{})
	if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != loc {
		t.Fatalf("suspended %+v, want the stalled process %+v", suspend.Locator, loc)
	}
	h.wantCalls("Resume", 0)
}

// A claim already queued for a wake is decided nothing at all, as refreshPlugin decides nothing
// over one (plugin.go's guard): the wake is this same claim's launch or task waiting its turn in
// the pacing, and a relaunch raised over it would take a second token and replace the wake with
// a launch that does what the parked one was already going to do. A relaunch parked that way
// leaves the claim working, so without the guard every sweep of a wave would raise another over
// it.
func TestTheBackstopLeavesAClaimAlreadyQueuedForAWake(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	h.deps.PluginContract = PodUpdateContract + 1
	stalled := stalePluginClaim(wakeClaim(secondToken, StateWorking), PodUpdateContract)
	stalled.State = StateWorking
	stepped := h.clock.Now().Add(-backstopBound - time.Minute)
	stalled.Step = StepRecord{At: stepped, TurnStartedAt: stepped, Tool: "bash", CallID: "toolu_turn1_0", StartedAt: stepped}
	stale := h.machineFor(stalled)

	// The harness's own claim takes the limiter's one token, so the stale claim's update task
	// parks rather than running where it is raised.
	h.must(RequestSpawn{Claim: h.token})
	h.mustPostTo(stale, StalePlugin{Claim: secondToken, Trigger: "sweep"})
	parked := stale.View().WakeWaiting
	if parked == nil || parked.Kind != WakePluginTask {
		t.Fatalf("the stale claim holds wake %+v, want a plugin task waiting its turn", parked)
	}

	h.mustPostTo(stale, StallBackstop{Claim: secondToken, Bound: backstopBound,
		Deadline: stepped.Add(backstopBound), Step: stalled.Step})

	if held := stale.View().WakeWaiting; held == nil || *held != *parked {
		t.Fatalf("the claim holds wake %+v, want the one it held, %+v: the backstop took no second token", held, parked)
	}
	if got := stale.Claim().State; got != StateWorking {
		t.Fatalf("the claim is %s, want it left %s: its turn is the parked wake's to end", got, StateWorking)
	}
	h.wantStarts(secondToken, 0)
	if lines := h.logs.lines("supervise: the stall backstop found this claim already queued for a wake"); len(lines) != 1 {
		t.Fatalf("the backstop wrote %d lines about the wake it left the claim to, want 1", len(lines))
	}
}

// The daemon is told what the machine did, never what the sweep asked for, because the owner's
// notice is written from it: a turn ended carries the record the request brought and the end it
// made, and a turn the fence left alone is told to nobody at all.
func TestTheBackstopReportsOnlyTheEndItMade(t *testing.T) {
	h, stops := backstopReports(t)
	h.workingOnATask("implement the plan")
	h.advance(time.Minute)
	h.stepped("bash", "toolu_turn1_0", toolArgs, false)
	last := h.clock.Now()
	swept := h.store.step(h.token)
	h.advance(backstopBound + time.Minute)

	h.must(h.backstop(last))

	h.wantState(StateLaunching)
	want := StallBackstopStop{Step: swept, LastStep: last, Bound: backstopBound, End: EndRelaunched}
	if len(*stops) != 1 || (*stops)[0] != want {
		t.Fatalf("the backstop reported %+v, want one end of the turn, %+v", *stops, want)
	}

	// The same claim, stepping inside its bound: the fence leaves the turn alone and reports
	// nothing, so nobody is told a turn was ended that was not.
	moved, movedStops := backstopReports(t)
	moved.workingOnATask("implement the plan")
	began := moved.clock.Now()
	moved.advance(backstopBound + time.Minute)
	moved.stepped("bash", "toolu_turn1_0", toolArgs, false)

	moved.must(moved.backstop(began))

	moved.wantState(StateWorking)
	if len(*movedStops) != 0 {
		t.Fatalf("the backstop reported %+v over a claim whose record moved, want nothing told", *movedStops)
	}
}

// A turn a held suspension ended is reported as that: the process was stopped and nothing
// launched in its place (endHeld), so an owner told its session had come back would go looking
// for a process that is not there.
func TestTheBackstopReportsASuspendedEndAsSuspended(t *testing.T) {
	h, stops := backstopReports(t)
	h.workingOnATask("implement the plan")
	if err := h.handle(RequestSuspend{Claim: h.token, By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("suspend mid-turn = %v, want ErrSuspendHeld", err)
	}
	loc := h.locator()

	// The agent stopped stepping before that suspension arrived. The record is pushed back
	// rather than the clock forward: a held suspension has a deadline of its own, far shorter
	// than any backstop bound (holdSuspension, TimerSuspend), so a clock advanced past the bound
	// would suspend this claim before the backstop was ever asked.
	last := h.clock.Now().Add(-backstopBound - time.Minute)
	stalled := StepRecord{At: last, TurnStartedAt: last, Tool: "bash", CallID: "toolu_turn1_0", StartedAt: last}
	h.m.steppedAt, h.m.claim.Step = last, stalled

	h.must(h.backstopOf(last, stalled))

	h.wantState(StateSuspended)
	h.wantCalls("Resume", 0)
	if suspend := h.wantCalls("Suspend", 1)[0]; suspend.Locator != loc {
		t.Fatalf("suspended %+v, want the stalled process %+v", suspend.Locator, loc)
	}
	want := StallBackstopStop{Step: stalled, LastStep: last, Bound: backstopBound, End: EndSuspended}
	if len(*stops) != 1 || (*stops)[0] != want {
		t.Fatalf("the backstop reported %+v, want one end the held suspension made, %+v", *stops, want)
	}
}

// A turn the daemon ended reaches its owner whatever the store then refused. The end the
// machine reached is what the notice says, and the write of that end is a separate thing: the
// process is gone either way, so a stall told to nobody is a worker that stopped and an
// architect that never hears of it. Here the launch the backstop made could not be written, so
// the stalled process was let go and stopped and its replacement waits on the probe interval
// (launchNow, relaunchLater).
func TestTheBackstopTellsTheOwnerOfAnEndWhoseLaunchWriteFailed(t *testing.T) {
	h, stops := backstopReports(t)
	h.workingOnATask("implement the plan")
	h.advance(time.Minute)
	h.stepped("bash", "toolu_turn1_0", toolArgs, false)
	last := h.clock.Now()
	swept := h.store.step(h.token)
	h.advance(backstopBound + time.Minute)
	h.store.fail("PutClaim", errBoom)

	if err := h.handle(h.backstop(last)); !errors.Is(err, errBoom) {
		t.Fatalf("the backstop whose launch the store would not take = %v, want the store's refusal", err)
	}

	if c := h.claim(); c.State != StateLaunching || c.Locator != nil {
		t.Fatalf("the claim is %s with locator %+v, want it launching with the stalled process let go", c.State, c.Locator)
	}
	want := StallBackstopStop{Step: swept, LastStep: last, Bound: backstopBound, End: EndRelaunchPending}
	if len(*stops) != 1 || (*stops)[0] != want {
		t.Fatalf("the backstop reported %+v, want the one end it made told to the owner, %+v", *stops, want)
	}
}

// The same rule where the replacement started and the write of it did not: the session is back
// on a new process, which is what the owner is told, and the claim write that failed is owed
// and made again at its next decision.
func TestTheBackstopTellsTheOwnerOfARelaunchWhoseWriteFailed(t *testing.T) {
	h, stops := backstopReports(t)
	h.workingOnATask("implement the plan")
	h.advance(time.Minute)
	h.stepped("bash", "toolu_turn1_0", toolArgs, false)
	last := h.clock.Now()
	swept := h.store.step(h.token)
	h.advance(backstopBound + time.Minute)
	h.m.deps.Runtime = &failingWriteAfterResume{Runtime: h.rt, store: h.store}

	if err := h.handle(h.backstop(last)); !errors.Is(err, errBoom) {
		t.Fatalf("the backstop whose started process could not be written = %v, want the store's refusal", err)
	}

	if c := h.claim(); c.State != StateLaunching || c.Locator == nil {
		t.Fatalf("the claim is %s with locator %+v, want it launching on the process the runtime started", c.State, c.Locator)
	}
	want := StallBackstopStop{Step: swept, LastStep: last, Bound: backstopBound, End: EndRelaunched}
	if len(*stops) != 1 || (*stops)[0] != want {
		t.Fatalf("the backstop reported %+v, want the one end it made told to the owner, %+v", *stops, want)
	}
}

// failingWriteAfterResume starts the replacement and then refuses every claim write, which is
// the window launchNow's own last line sits in: the process runs and the record of it does not
// reach the store.
type failingWriteAfterResume struct {
	*fake.Runtime
	store *memStore
}

func (r *failingWriteAfterResume) Resume(ctx context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	loc, err := r.Runtime.Resume(ctx, prev, spec)
	r.store.fail("PutClaim", errBoom)
	return loc, err
}

// A backstop whose replacement the runtime refuses until the claim's launch budget is gone ends
// in a failed claim, and the owner is told that end: nothing runs for the claim, and an owner
// told its session had come back would wait for a worker that is never coming.
func TestTheBackstopTellsTheOwnerOfAnEndThatFailedTheClaim(t *testing.T) {
	h, stops := backstopReports(t)
	h.workingOnATask("implement the plan")
	h.advance(time.Minute)
	h.stepped("bash", "toolu_turn1_0", toolArgs, false)
	last := h.clock.Now()
	swept := h.store.step(h.token)
	h.advance(backstopBound + time.Minute)
	refused := errors.New("the pod did not start")
	for range testLimits().LaunchFailures {
		h.rt.ScriptResume(fake.SpawnResult{Err: refused})
	}

	if err := h.handle(h.backstop(last)); !errors.Is(err, refused) {
		t.Fatalf("the backstop whose every relaunch the runtime refused = %v, want the runtime's refusal", err)
	}
	h.wantState(StateFailed)

	want := StallBackstopStop{Step: swept, LastStep: last, Bound: backstopBound, End: EndFailed}
	if len(*stops) != 1 || (*stops)[0] != want {
		t.Fatalf("the backstop reported %+v, want the one end it made told to the owner, %+v", *stops, want)
	}
}

// The other half of the same rule, and the easier one to lose: nothing that did not happen is
// ever told. A take-back the store refuses leaves the turn exactly as it stands — the process
// still runs it, holding its task — so there is no end to tell, and an architect sent here
// would go looking for a recovery nobody made.
func TestTheBackstopTellsNobodyOfAnEndTheStoreRefusedBeforeItWasMade(t *testing.T) {
	h, stops := backstopReports(t)
	h.workingOnATask("implement the plan")
	h.advance(time.Minute)
	h.stepped("bash", "toolu_turn1_0", toolArgs, false)
	last := h.clock.Now()
	loc := h.locator()
	h.advance(backstopBound + time.Minute)
	h.store.fail("PutDelivery", errBoom)

	if err := h.handle(h.backstop(last)); !errors.Is(err, errBoom) {
		t.Fatalf("the backstop whose take-back the store would not take = %v, want the store's refusal", err)
	}

	c := h.claim()
	if c.State != StateWorking || c.Locator == nil || *c.Locator != loc {
		t.Fatalf("the claim is %s with locator %+v, want its turn left on the process %+v", c.State, c.Locator, loc)
	}
	h.wantCalls("Resume", 0)
	if len(*stops) != 0 {
		t.Fatalf("the backstop reported %+v over a turn it did not end, want nobody told", *stops)
	}
}

// Nor is a replacement the pacing parked told as an end: the claim keeps the process and its
// turn, and the wake's own turn is what replaces it later (reserveWake, wakeNow). It is the
// same news as the turn left alone — nothing has happened to this worker yet — and the one a
// report written from the error alone would get wrong, since this launch returns nil.
func TestTheBackstopTellsNobodyOfAReplacementThePacingParked(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	var stops []StallBackstopStop
	h.deps.StallBackstopped = func(_ Claim, stop StallBackstopStop) { stops = append(stops, stop) }
	stepped := h.clock.Now().Add(-backstopBound - time.Minute)
	working := stalePluginClaim(wakeClaim(secondToken, StateWorking), h.deps.PluginContract)
	working.State = StateWorking
	working.Step = StepRecord{At: stepped, TurnStartedAt: stepped, Tool: "bash", CallID: "toolu_turn1_0", StartedAt: stepped}
	stalled := h.machineFor(working)

	// The harness's own claim takes the limiter's one token, so the backstop's own relaunch
	// parks rather than running where it is raised.
	h.must(RequestSpawn{Claim: h.token})

	h.mustPostTo(stalled, StallBackstop{Claim: secondToken, Bound: backstopBound,
		Deadline: stepped.Add(backstopBound), Step: working.Step})

	if parked := stalled.View().WakeWaiting; parked == nil || parked.Kind != WakeLaunch {
		t.Fatalf("the claim holds wake %+v, want the backstop's launch waiting its turn", parked)
	}
	if c := stalled.Claim(); c.State != StateWorking || c.Locator == nil {
		t.Fatalf("the claim is %s with locator %+v, want it left in its turn until the wake's turn comes", c.State, c.Locator)
	}
	h.wantStarts(secondToken, 0)
	if len(stops) != 0 {
		t.Fatalf("the backstop reported %+v over a replacement the pacing parked, want nobody told", stops)
	}
}
