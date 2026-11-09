package supervise

import (
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// compactInstructions stands for what the API sends with every compaction: Oh My Pi writes a
// text summary only for a compaction that carries instructions, and Machine.Compact refuses one
// that carries none.
const testCompactInstructions = "Write the summary as text."

// ladderStuck is the claim the provider-errors notice names, idle between the ladder's sends:
// six turns of it in a row ended in a provider error, so the project's controller has been told
// once, the task it owes is unconfirmed on the claim, and the ladder's next step is ten minutes
// away. ladderSends is how many prompts the connection has seen by then. set, when it is given,
// changes the dependencies before the machine is built, as workingClaim's own does.
const ladderSends = providerErrorRunTold

func ladderStuck(t *testing.T, notices *int, set ...func(*Deps)) *harness {
	t.Helper()
	h := workingClaim(t, append([]func(*Deps){func(d *Deps) {
		d.ProviderErrorRunReached = func(Claim, string) { *notices++ }
	}}, set...)...)
	for turn := range providerErrorRunTold {
		h.errorTurn(StreamPromptResult{Error: "overloaded_error", HTTPStatus: 529})
		if turn+1 < providerErrorRunTold {
			h.climbs(turn+1, promptRetryDelay(turn+1))
		}
	}
	if got := h.claim().ProviderErrorRun; got != providerErrorRunTold {
		t.Fatalf("the run of provider errors is %d, want the %d the controller is told at", got, providerErrorRunTold)
	}
	if *notices != 1 {
		t.Fatalf("the controller was told %d times, want once", *notices)
	}
	if got := h.state(); got != StateIdle {
		t.Fatalf("the claim is %s, want it idle between the ladder's sends", got)
	}
	h.wantPrompts(ladderSends)
	return h
}

// A compaction that ran starts the claim's run of provider-error turns again, so the project's
// controller is told a second time after six more refused turns. Without the reset the
// controller would be told once and never again until a turn of that claim ended with no error
// at all, and the compaction it asked for would buy it no second look.
//
// The owed task goes again at once rather than at the end of the ladder step the compaction
// interrupted: the next step of that ladder is ten minutes away, and putting the task in front
// of the shortened session is the whole point of the operation.
func TestACompactionStartsTheRunOfProviderErrorsAgainSoTheControllerIsToldASecondTime(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	sent := ladderSends

	if err := h.m.Compact(h.ctx, testCompactInstructions); err != nil {
		t.Fatalf("compact: %v", err)
	}
	h.m.Wait()

	if got := h.claim().ProviderErrorRun; got != 0 {
		t.Fatalf("the run of provider errors is %d after the compaction, want 0", got)
	}
	if got := h.store.load(h.token).ProviderErrorRun; got != 0 {
		t.Fatalf("the stored run of provider errors is %d after the compaction, want 0", got)
	}
	if got := h.claim().Session; got != session {
		t.Fatalf("the compacted claim records session %q, want the session it had, %q", got, session)
	}
	if got := h.state(); got != StateIdle {
		t.Fatalf("the compacted claim is %s, want idle again", got)
	}
	h.wantPrompts(sent + 1)
	h.must(StreamTurnStart{Claim: h.token})

	for turn := range providerErrorRunTold {
		h.errorTurn(StreamPromptResult{Error: "overloaded_error", HTTPStatus: 529})
		if turn+1 < providerErrorRunTold {
			h.climbs(sent+1+turn, promptRetryMax)
		}
	}

	if notices != 2 {
		t.Fatalf("the controller was told %d times, want a second notice after six more refused turns", notices)
	}
}

// A compaction Oh My Pi refused or skipped resets nothing: the session is as it was, so the run
// of refused turns it carried still stands and the controller is not told again for it. The
// owed task goes again all the same.
func TestACompactionThatWasRefusedKeepsTheRunOfProviderErrors(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	sent := ladderSends
	h.conn.RefuseCompact("Nothing to compact (session too small)")

	err := h.m.Compact(h.ctx, testCompactInstructions)

	if reason := runtimeRefusalReason(err); reason != "Nothing to compact (session too small)" {
		t.Fatalf("compact answered %v, want Oh My Pi's own refusal", err)
	}
	h.m.Wait()
	if got := h.claim().ProviderErrorRun; got != providerErrorRunTold {
		t.Errorf("the run of provider errors is %d, want the %d it carried: nothing was compacted", got, providerErrorRunTold)
	}
	if got := h.state(); got != StateIdle {
		t.Errorf("the claim is %s, want idle again", got)
	}
	h.wantPrompts(sent + 1)
}

// runtimeRefusalReason is the words the far side refused with, as runtime.RefusalReason reads
// them; it is spelled here so this file needs no import for one call.
func runtimeRefusalReason(err error) string {
	var refusal interface{ RefusalReason() string }
	if errors.As(err, &refusal) {
		return refusal.RefusalReason()
	}
	return ""
}

// A claim being compacted is busy exactly as a claim in a turn is: it is working, a newer task
// waits rather than being sent into the compaction, a second compaction is refused, and the end
// of the compaction is what releases the task.
func TestAClaimBeingCompactedIsBusyUntilTheCompactionEnds(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	sent := ladderSends
	release := h.conn.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	waitForCompacting(t, h)

	h.must(RequestDeliver{Claim: h.token, Task: "A newer task.", ID: "newer"})
	second := h.m.Handle(h.ctx, RequestCompact{Claim: h.token})

	var refused *RefusedError
	if !errors.As(second, &refused) {
		t.Fatalf("a second compaction answered %v, want a refusal", second)
	}
	h.wantPrompts(sent)
	release()
	if err := <-compacted; err != nil {
		t.Fatalf("compact: %v", err)
	}
	h.m.Wait()
	if got := h.state(); got != StateIdle {
		t.Fatalf("the claim is %s after the compaction, want idle", got)
	}
	prompts := h.wantPrompts(sent + 1)
	if prompts[sent].DeliveryID != "newer" {
		t.Fatalf("the send after the compaction carried %q, want the newer task", prompts[sent].DeliveryID)
	}
}

// waitForCompacting waits until the machine reports the claim being compacted.
func waitForCompacting(t *testing.T, h *harness) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c := h.m.View().Claim; c.Compacting && c.State == StateWorking {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the claim never reported itself being compacted")
}

// A turn that ends while the session is being compacted leaves the claim exactly where it is: a
// compaction aborts whatever turn its session was running, so a background turn's end can
// arrive under one, and taking it for the compaction's end would hand the claim its owed task
// while Oh My Pi is still writing the summary.
func TestATurnThatEndsDuringACompactionLeavesTheClaimWorking(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	sent := ladderSends
	release := h.conn.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	waitForCompacting(t, h)

	h.mustQuiet(StreamTurnEnd{Claim: h.token})

	if got := h.state(); got != StateWorking {
		t.Fatalf("the claim is %s after a turn ended under the compaction, want it still working", got)
	}
	h.wantPrompts(sent)
	release()
	if err := <-compacted; err != nil {
		t.Fatalf("compact: %v", err)
	}
	h.m.Wait()
	h.wantPrompts(sent + 1)
}

// A compaction asked for with no instructions is refused before anything is sent: Oh My Pi
// archives the history onto snapcompact's images for a compaction that carries none, and a
// worker resumed on images needs a vision model to read its own past.
func TestACompactionWithNoInstructionsIsRefusedBeforeAnythingIsSent(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)

	err := h.m.Compact(h.ctx, "  ")

	if !errors.Is(err, ErrCompactInstructions) {
		t.Fatalf("compact with no instructions answered %v, want ErrCompactInstructions", err)
	}
	if got := len(h.conn.Compactions()); got != 0 {
		t.Fatalf("Oh My Pi was asked for %d compactions, want none", got)
	}
	if got := h.state(); got != StateIdle {
		t.Fatalf("the claim is %s, want it left idle", got)
	}
}

// The ladder has just sent the owed task and the prompt is in flight — acknowledged, with the
// wait for its turn armed — when the compaction is taken. Oh My Pi runs one thing at a time, so
// the compaction aborts the turn that prompt started, and the result of an aborted turn carries
// no error of its own: read as a turn that worked, it would confirm the task and the end of the
// compaction would retire it unsent. The task is taken back instead, and the end of the
// compaction sends it again.
func TestACompactionTakenWhileTheOwedPromptIsInFlightSendsTheTaskAgainAfterIt(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	h.advance(promptRetryMax) // the ladder's next step sends the owed task
	sent := h.wantPrompts(ladderSends + 1)[ladderSends].DeliveryID
	if got := h.state(); got != StateIdle {
		t.Fatalf("after the ladder's send the claim is %s, want it idle waiting for the turn", got)
	}
	release := h.conn.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	waitForCompacting(t, h)

	h.must(StreamTurnStart{Claim: h.token})
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	h.must(StreamPromptResult{Claim: h.token, DeliveryID: sent, AgentInvoked: true})

	release()
	if err := <-compacted; err != nil {
		t.Fatalf("compact: %v", err)
	}
	h.m.Wait()
	owed := h.claim().Pending
	if owed == nil {
		t.Fatal("the compaction retired the task whose turn it aborted: the claim owes nothing, and nothing sends it again")
	}
	if owed.ID == sent {
		t.Fatalf("the task kept the id %q its aborted prompt carried; a task taken back goes under a new one, so that prompt's result names nothing", sent)
	}
	prompts := h.wantPrompts(ladderSends + 2)
	if prompts[ladderSends+1].DeliveryID != owed.ID {
		t.Fatalf("the send after the compaction carried %q, want the task the claim still owes, %q",
			prompts[ladderSends+1].DeliveryID, owed.ID)
	}
}

// A turn Oh My Pi starts as the compaction ends — a follow-up an Envoy delivery queued while the
// session was being compacted — leaves the claim working for that turn. Idling it there would
// send the owed task into a running agent, and that turn's own end would then arrive at an idle
// claim, where it means nothing.
func TestATurnThatStartsDuringACompactionKeepsTheClaimWorkingUntilItEnds(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	owed := h.pending().ID
	release := h.conn.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	waitForCompacting(t, h)

	h.must(StreamTurnStart{Claim: h.token})

	release()
	if err := <-compacted; err != nil {
		t.Fatalf("compact: %v", err)
	}
	h.m.Wait()
	if got := h.state(); got != StateWorking {
		t.Fatalf("the claim is %s while its agent runs a turn that started under the compaction, want it working", got)
	}
	h.wantPrompts(ladderSends)

	h.mustQuiet(StreamTurnEnd{Claim: h.token})

	if got := h.state(); got != StateIdle {
		t.Fatalf("the claim is %s after that turn ended, want idle", got)
	}
	prompts := h.wantPrompts(ladderSends + 1)
	if prompts[ladderSends].DeliveryID != owed {
		t.Fatalf("the send after the turn carried %q, want the owed task %q", prompts[ladderSends].DeliveryID, owed)
	}
}

// A claim that was ready is ready again when the compaction ends: no turn of it has begun or
// ended, so idling it would say one had. The rollback of a take the store would not write
// restores the same state.
func TestACompactionGivesBackAReadyClaimReady(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.store.fail("PutClaim", errors.New("the store is down"))

	refused := h.m.Compact(h.ctx, testCompactInstructions)

	if refused == nil {
		t.Fatal("a compaction the store would not write the take of answered no error")
	}
	if got := h.state(); got != StateReady {
		t.Fatalf("the claim is %s after the take was rolled back, want the ready it was", got)
	}
	if got := len(h.conn.Compactions()); got != 0 {
		t.Fatalf("Oh My Pi was asked for %d compactions, want none: the claim was never taken", got)
	}
	h.store.fail("PutClaim", nil)

	if err := h.m.Compact(h.ctx, testCompactInstructions); err != nil {
		t.Fatalf("compact: %v", err)
	}

	h.m.Wait()
	if got := h.state(); got != StateReady {
		t.Fatalf("the compacted claim is %s, want the ready it was: no turn of it has ended", got)
	}
	if got := h.store.load(h.token).State; got != StateReady {
		t.Fatalf("the stored claim is %s, want ready", got)
	}
}

// A compaction whose take the store would not write asked Oh My Pi nothing and sent the claim
// nothing, so it is told apart from a compaction that ran and was refused: its caller answers
// a failure rather than report a compaction that did not run, which carries the claim's task
// with it (api's compact).
func TestACompactionTheStoreWouldNotTakeSaysNothingWasAsked(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	h.store.fail("PutClaim", errors.New("the store is down"))

	err := h.m.Compact(h.ctx, testCompactInstructions)

	if !errors.Is(err, ErrCompactNotTaken) {
		t.Fatalf("a compaction the store would not take answered %v, want ErrCompactNotTaken", err)
	}
	if got := len(h.conn.Compactions()); got != 0 {
		t.Fatalf("Oh My Pi was asked for %d compactions, want none", got)
	}
	h.wantPrompts(ladderSends)
}

// A compaction of a claim whose shim is not connected asks nothing and takes nothing: the daemon
// has no connection to ask through, which is no refusal of the claim's state — the state takes a
// compaction — so it answers the take that did not happen, and its caller reports the one
// failure it is rather than reading the claim's state for a cause that state does not hold
// (api's compact).
func TestACompactionWhoseShimIsNotConnectedSaysNothingWasAsked(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	h.conns.Remove(h.token)

	err := h.m.Compact(h.ctx, testCompactInstructions)

	if !errors.Is(err, ErrCompactNotTaken) {
		t.Fatalf("a compaction with no shim connected answered %v, want ErrCompactNotTaken", err)
	}
	var refused *RefusedError
	if errors.As(err, &refused) {
		t.Fatalf("the answer is the refusal %+v; the state takes a compaction, so no refusal of it says what went wrong", refused)
	}
	if got := len(h.conn.Compactions()); got != 0 {
		t.Fatalf("Oh My Pi was asked for %d compactions, want none", got)
	}
	if got := h.state(); got != StateIdle {
		t.Fatalf("the claim is %s, want the idle it was", got)
	}
	h.wantPrompts(ladderSends)
}

// A compaction whose end the store would not write is told apart from one that was refused: Oh
// My Pi compacted the session, the write that gives the claim back failed, and the task the
// claim owes is unsent. Answered as a compaction that did not run, it would tell its caller the
// claim was back and its task had gone again (api's compact).
func TestACompactionWhoseEndTheStoreWouldNotWriteSaysTheClaimWasNotGivenBack(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	release := h.conn.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	waitForCompacting(t, h)

	h.store.fail("PutClaim", errors.New("the store is down"))
	release()

	err := <-compacted
	if !errors.Is(err, ErrCompactNotEnded) {
		t.Fatalf("a compaction whose end the store would not write answered %v, want ErrCompactNotEnded", err)
	}
	if got := len(h.conn.Compactions()); got != 1 {
		t.Fatalf("Oh My Pi was asked for %d compactions, want the one that ran", got)
	}
	h.m.Wait()
	h.wantPrompts(ladderSends)
	if got := h.store.load(h.token).State; got != StateWorking {
		t.Fatalf("the stored claim is %s, want the working the take wrote: the end was never written", got)
	}
}

// A claim that loses its process while its session is being compacted gets nothing from the end
// of that compaction: the end reaches a claim the relaunch has moved past, so it decides nothing
// and sends nothing. The task the claim owes goes at the relaunch, once its agent is ready
// again, and the answer says so rather than reporting a compaction that gave the claim back.
func TestACompactionWhoseClaimLosesItsProcessLeavesTheOwedTaskToTheRelaunch(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	owed := h.pending().ID
	release := h.conn.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	waitForCompacting(t, h)

	h.observe(runtime.Gone)
	release()

	if err := <-compacted; !errors.Is(err, ErrCompactProcessGone) {
		t.Fatalf("a compaction whose claim lost its process answered %v, want ErrCompactProcessGone", err)
	}
	h.m.Wait()
	h.wantPrompts(ladderSends)

	h.relaunched()

	h.m.Wait()
	prompts := h.wantPrompts(ladderSends + 1)
	if prompts[ladderSends].DeliveryID != owed {
		t.Fatalf("the relaunched claim was sent %q, want the task it owed, %q", prompts[ladderSends].DeliveryID, owed)
	}
}

// A death the wake pacing parks the relaunch of leaves the claim exactly as the park found it:
// working, over the dead process it still records, at the generation the compaction was taken
// at. The park writes nothing of the claim at all, so the compaction is dropped where the death
// is read (compactionLostProcess) and the end of the compaction finds none to give back
// (ownsCompaction). Taken for a compaction that ran, the end would give the claim back idle and
// prompt a process that is gone, and the answer would promise its caller both.
func TestACompactionWhoseRelaunchWaitsItsTurnLeavesTheOwedTaskToThatTurn(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices, pacedDeps(wakeRate))
	owed := h.pending().ID
	dead := h.locator()
	release := h.conn.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	waitForCompacting(t, h)

	// A claim of the wave takes the turn the limiter had saved, so the relaunch this death
	// charges waits for the next one rather than running where it is raised.
	queueAhead(h, 1)
	h.observe(runtime.Gone)
	waiting := h.wantWaitingWake(WakeLaunch)
	release()

	if err := <-compacted; !errors.Is(err, ErrCompactProcessGone) {
		t.Fatalf("a compaction whose claim's relaunch waits its turn answered %v, want ErrCompactProcessGone", err)
	}
	h.m.Wait()
	h.wantState(StateWorking)
	if got := h.claim().Locator; got == nil || *got != dead {
		t.Fatalf("the claim records %+v, want the dead process the park kept, %+v", got, dead)
	}
	h.wantWakeDueAt(waiting)
	h.wantPrompts(ladderSends)
	if got := h.pending(); got.ID != owed || !got.ConfirmedAt.IsZero() {
		t.Fatalf("the claim holds %+v, want the task %s still owed and unconfirmed", got, owed)
	}

	h.advance(waiting.At.Sub(h.clock.Now()))
	h.relaunched()

	h.m.Wait()
	prompts := h.wantPrompts(ladderSends + 1)
	if prompts[ladderSends].DeliveryID != owed {
		t.Fatalf("the claim the wake's turn relaunched was sent %q, want the task it owed, %q", prompts[ladderSends].DeliveryID, owed)
	}
}

// What that same claim records once the launch's turn has come and the turn's own lifecycle
// recheck refused it: the refusal keeps the process the claim records and the wake is spent,
// so nothing waits over the claim and nothing will end the turn it is not in (runWake,
// relaunchRefused). The next report of that process raises a relaunch again, and until one
// does the claim stands stored working over a process that is gone.
//
// What that claim records of its process is the verdict that raised the relaunch, kept where
// every verdict is judged and read by both recovery doors (Claim.ProcessGone,
// reportedProcess): state, the compaction in flight, the suspension held and the wake waiting
// are the rest of what either door reads of a working claim (api's refuseState, freshSession),
// and a claim genuinely in a turn reads the same in all four, so the fifth is the whole of
// what tells the two apart. Without it both doors answer such a claim "is in a turn", which it
// is not — the turn is over, its process gone, and what raises the next relaunch is the next
// report of that process.
//
// This case was TestAParkedRelaunchRefusedAtItsTurnRecordsNothingThatTellsItFromATurn, which
// held that nothing of the claim told the two apart: that is the gap this round closes, so the
// case is rewritten to read the fact that closes it rather than kept beside it.
func TestAParkedRelaunchRefusedAtItsTurnRecordsThatItsProcessIsGone(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices, pacedDeps(wakeRate))
	dead := h.locator()
	release := h.conn.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	waitForCompacting(t, h)
	queueAhead(h, 1)
	h.observe(runtime.Gone)
	waiting := h.wantWaitingWake(WakeLaunch)
	release()
	if err := <-compacted; !errors.Is(err, ErrCompactProcessGone) {
		t.Fatalf("a compaction whose claim's relaunch waits its turn answered %v, want ErrCompactProcessGone", err)
	}
	h.m.Wait()

	// The store cannot answer the lifecycle recheck when the launch's turn comes, which is the
	// refusal that keeps the process and clears by itself (unansweredCheckLaunchStore).
	h.m.deps.Store = &unansweredCheckLaunchStore{memStore: h.store, failing: true}
	h.advance(waiting.At.Sub(h.clock.Now()))

	h.wantState(StateWorking)
	view := h.m.View()
	if view.WakeWaiting != nil {
		t.Fatalf("the claim names wake %+v, want none: the turn ran and its launch was refused", view.WakeWaiting)
	}
	if view.Compacting || view.SuspensionHeld {
		t.Fatalf("the claim reports Compacting %v and a suspension held %v, want neither", view.Compacting, view.SuspensionHeld)
	}
	if !view.ProcessGone {
		t.Fatal("the claim reports nothing of its process, want the gone verdict that raised the refused relaunch")
	}
	if got := view.Locator; got == nil || *got != dead {
		t.Fatalf("the claim records %+v, want the dead process the refusal kept, %+v", got, dead)
	}
}

// A displacement the pacing parks the relaunch of is the same loss: the process the compaction
// ran over is gone, however the daemon recovers the claim, so its end gives the claim back
// nothing and the owed task waits for the wake's turn. The drop is made where the runtime's
// verdict is read, before a death is told from a displacement (judge), so both judgements carry
// it; made in the death alone, a displaced claim would report itself compacted until that turn,
// and the compaction's end would prompt the process the replacement ended.
func TestACompactionWhoseDisplacedRelaunchWaitsItsTurnLeavesTheOwedTaskToThatTurn(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices, pacedDeps(wakeRate))
	owed := h.pending().ID
	release := h.conn.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	waitForCompacting(t, h)

	// A claim of the wave takes the turn the limiter had saved, so the relaunch this
	// displacement raises waits for the next one rather than running where it is raised.
	queueAhead(h, 1)
	h.observeDisplaced(runtime.Gone)
	waiting := h.wantWaitingWake(WakeLaunch)
	release()

	if err := <-compacted; !errors.Is(err, ErrCompactProcessGone) {
		t.Fatalf("a compaction whose displaced claim's relaunch waits its turn answered %v, want ErrCompactProcessGone", err)
	}
	h.m.Wait()
	if h.m.View().Claim.Compacting {
		t.Fatal("the claim still reports itself being compacted, want the compaction dropped with the process it ran over")
	}
	h.wantWakeDueAt(waiting)
	h.wantPrompts(ladderSends)
	if got := h.pending(); got.ID != owed || !got.ConfirmedAt.IsZero() {
		t.Fatalf("the claim holds %+v, want the task %s still owed and unconfirmed", got, owed)
	}

	h.advance(waiting.At.Sub(h.clock.Now()))
	h.relaunched()

	h.m.Wait()
	prompts := h.wantPrompts(ladderSends + 1)
	if prompts[ladderSends].DeliveryID != owed {
		t.Fatalf("the claim the wake's turn relaunched was sent %q, want the task it owed, %q", prompts[ladderSends].DeliveryID, owed)
	}
}

// A process found alive at a stale address is replaced rather than lost (repoint), and the wake
// pacing can park that replacement over a session being compacted. The compaction ran on a
// process that is still running, so it is the claim's own to the end: the claim is given back,
// its run of provider-error turns starts again, and the answer is the compaction's own. What
// waits is the owed task, which goes at the turn of the launch parked over the process a send
// would reach (sendPending), not into a process that turn is about to replace.
//
// Read as a lost process — which a parked launch alone says nothing about — the caller would be
// told its compaction failed for a compaction that ran, the claim would be left working with
// its run of provider errors still at the notice, and the controller, told only at that run,
// could never be told a second time.
func TestACompactionUnderAParkedRepointGivesTheClaimBackAndLeavesTheTaskToThatTurn(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices, pacedDeps(wakeRate))
	owed := h.pending().ID
	release := h.conn.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	waitForCompacting(t, h)

	// A claim of the wave takes the turn the limiter had saved, so the relaunch the stale
	// address raises waits for the next one rather than running where it is raised.
	queueAhead(h, 1)
	h.observe(runtime.StaleAddress)
	waiting := h.wantWaitingWake(WakeLaunch)
	release()

	if err := <-compacted; err != nil {
		t.Fatalf("a compaction under a parked repoint answered %v, want the claim given back", err)
	}
	h.m.Wait()
	h.wantState(StateIdle)
	if got := h.claim().ProviderErrorRun; got != 0 {
		t.Fatalf("the run of provider errors is %d after the compaction, want 0", got)
	}
	if got := h.store.load(h.token).ProviderErrorRun; got != 0 {
		t.Fatalf("the stored run of provider errors is %d after the compaction, want 0", got)
	}
	h.wantWakeDueAt(waiting)
	h.wantPrompts(ladderSends)
	if got := h.pending(); got.ID != owed || !got.ConfirmedAt.IsZero() {
		t.Fatalf("the claim holds %+v, want the task %s still owed and unconfirmed", got, owed)
	}

	h.advance(waiting.At.Sub(h.clock.Now()))
	h.relaunched()

	h.m.Wait()
	prompts := h.wantPrompts(ladderSends + 1)
	if prompts[ladderSends].DeliveryID != owed {
		t.Fatalf("the claim the wake's turn relaunched was sent %q, want the task it owed, %q", prompts[ladderSends].DeliveryID, owed)
	}
}

// A compaction asked for while a launch already waits its turn over the claim's process is a
// take that did not happen: that launch lets the process go at its turn, so nothing is asked of
// its session, nothing is written and nothing is sent. The claim is left exactly where the
// request found it — idle, holding the task it owes, its run of provider errors still at the
// notice — and its caller answers the one failure it is (api's compact, 409 COMPACTION_FAILED).
func TestACompactionAskedOverALaunchWaitingItsTurnIsNotTaken(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices, pacedDeps(wakeRate))
	owed := h.pending().ID
	stale := h.locator()
	queueAhead(h, 1)
	h.observe(runtime.StaleAddress)
	waiting := h.wantWaitingWake(WakeLaunch)

	err := h.m.Compact(h.ctx, testCompactInstructions)

	if !errors.Is(err, ErrCompactNotTaken) {
		t.Fatalf("a compaction asked over a launch waiting its turn answered %v, want ErrCompactNotTaken", err)
	}
	h.m.Wait()
	if asked := h.conn.Compactions(); len(asked) != 0 {
		t.Fatalf("Oh My Pi was asked %+v, want nothing: the claim was never taken", asked)
	}
	h.wantState(StateIdle)
	if got := h.store.load(h.token).State; got != StateIdle {
		t.Fatalf("the stored claim is %s, want the idle the request found", got)
	}
	if h.m.View().Claim.Compacting {
		t.Fatal("the claim reports itself being compacted, want no compaction: the take did not happen")
	}
	if got := h.claim().Locator; got == nil || *got != stale {
		t.Fatalf("the claim records %+v, want the process the parked launch replaces, %+v", got, stale)
	}
	h.wantWakeDueAt(waiting)
	h.wantPrompts(ladderSends)
	if got := h.pending(); got.ID != owed || !got.ConfirmedAt.IsZero() {
		t.Fatalf("the claim holds %+v, want the task %s still owed and unconfirmed", got, owed)
	}
	if got := h.claim().ProviderErrorRun; got != providerErrorRunTold {
		t.Fatalf("the run of provider errors is %d, want the %d it carried: nothing was compacted", got, providerErrorRunTold)
	}
}

// A death that spends the claim's last launch leaves it failed, and a failed claim relaunches of
// no accord of its own: an operator's retry is what starts it again. The end of the compaction
// finds a claim with no compaction recorded, which it must give nothing back — taken for a
// compaction that ran it would idle the failed claim and prompt the process the death ended.
func TestACompactionWhoseClaimItsDeathFailedGivesItBackNothing(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices, func(d *Deps) { d.Limits.LaunchFailures = 1 })
	owed := h.pending().ID
	release := h.conn.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	waitForCompacting(t, h)

	h.observe(runtime.Gone)
	h.wantState(StateFailed)
	release()

	if err := <-compacted; !errors.Is(err, ErrCompactProcessGone) {
		t.Fatalf("a compaction whose claim its death failed answered %v, want ErrCompactProcessGone", err)
	}
	h.m.Wait()
	h.wantState(StateFailed)
	if got := h.store.load(h.token).State; got != StateFailed {
		t.Fatalf("the stored claim is %s, want the failed its death left", got)
	}
	h.wantPrompts(ladderSends)
	if got := h.pending(); got.ID != owed || !got.ConfirmedAt.IsZero() {
		t.Fatalf("the claim holds %+v, want the task %s still owed and unconfirmed", got, owed)
	}
}

// An operator's suspension that arrives under the compaction is held for its end, and its stop
// timeout is what ends the wait when the compaction outlasts it: the process is stopped and the
// claim suspended, which only the operator's resume starts again. The end of the compaction
// finds a claim with no compaction recorded, which it must give nothing back — taken for a
// compaction that ran it would idle the suspended claim and prompt the process the stop ended.
func TestACompactionAHeldSuspensionStoppedGivesTheClaimBackNothing(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	owed := h.pending().ID
	release := h.conn.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	waitForCompacting(t, h)

	held := RequestSuspend{Claim: h.token, By: SuspendedByOperator, Reason: "the operator is draining the daemon"}
	if err := h.handle(held); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("the suspension answered %v, want it held for the compaction's end", err)
	}
	h.advance(testStop)
	h.wantState(StateSuspended)
	release()

	if err := <-compacted; !errors.Is(err, ErrCompactProcessGone) {
		t.Fatalf("a compaction a held suspension's deadline stopped answered %v, want ErrCompactProcessGone", err)
	}
	h.m.Wait()
	h.wantState(StateSuspended)
	if got := h.claim().SuspendedBy; got != SuspendedByOperator {
		t.Fatalf("the claim is suspended by %q, want the operator whose stop ended it", got)
	}
	h.wantPrompts(ladderSends)
	if got := h.pending(); got.ID != owed || !got.ConfirmedAt.IsZero() {
		t.Fatalf("the claim holds %+v, want the task %s still owed and unconfirmed", got, owed)
	}
}

// An operator's suspension that arrives under the compaction and lands at its end leaves the
// claim suspended with its process stopped and the task it owes unsent, so the compaction
// answers what became of the claim rather than nil: nil is the claim back and its task gone
// again, which its caller reports as a recovery that worked (ErrCompactSuspended, api's
// CompactResponse). Only the operator's resume starts such a claim again, and the task goes
// then.
func TestACompactionAHeldSuspensionEndsSaysTheClaimWasNotGivenBack(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	owed := h.pending().ID
	release := h.conn.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	waitForCompacting(t, h)

	held := RequestSuspend{Claim: h.token, By: SuspendedByOperator, Reason: "the operator is draining the daemon"}
	if err := h.handle(held); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("the suspension answered %v, want it held for the compaction's end", err)
	}
	release()

	if err := <-compacted; !errors.Is(err, ErrCompactSuspended) {
		t.Fatalf("a compaction whose end ran the held suspension answered %v, want ErrCompactSuspended", err)
	}
	h.m.Wait()
	h.wantState(StateSuspended)
	if got := h.claim().SuspendedBy; got != SuspendedByOperator {
		t.Fatalf("the claim is suspended by %q, want the operator whose hold ended the compaction", got)
	}
	h.wantPrompts(ladderSends)
	if got := h.pending(); got.ID != owed || !got.ConfirmedAt.IsZero() {
		t.Fatalf("the claim holds %+v, want the task %s still owed and unconfirmed", got, owed)
	}
}

// A turn Oh My Pi started under the compaction keeps the claim working at the compaction's end,
// and a suspension held meanwhile runs at that turn's end instead (turnEnded). The claim is
// back on neither its agent nor its task — the hold stops it when the turn is over — so the
// compaction says so as the end that runs the hold itself does: the two endings differ in when
// the stop lands and in nothing the answer may promise.
func TestACompactionThatEndsInATurnUnderAHeldSuspensionSaysTheClaimWasNotGivenBack(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	owed := h.pending().ID
	release := h.conn.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	waitForCompacting(t, h)

	h.must(StreamTurnStart{Claim: h.token})
	held := RequestSuspend{Claim: h.token, By: SuspendedByOperator, Reason: "the operator is draining the daemon"}
	if err := h.handle(held); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("the suspension answered %v, want it held for the turn's end", err)
	}
	release()

	if err := <-compacted; !errors.Is(err, ErrCompactSuspended) {
		t.Fatalf("a compaction that ended in a turn under a held suspension answered %v, want ErrCompactSuspended", err)
	}
	h.m.Wait()
	h.wantState(StateWorking)
	if got := h.claim(); !got.SuspensionHeld || got.Compacting {
		t.Fatalf("the claim holds %v and reports Compacting %v, want the hold kept for the turn's end and the compaction over",
			got.SuspensionHeld, got.Compacting)
	}
	h.wantPrompts(ladderSends)
	if got := h.pending(); got.ID != owed || !got.ConfirmedAt.IsZero() {
		t.Fatalf("the claim holds %+v, want the task %s still owed and unconfirmed", got, owed)
	}

	// The turn's end is where that hold lands, and the claim is suspended with the task still
	// owed: what the compaction answered is what became of it.
	h.mustQuiet(StreamTurnEnd{Claim: h.token})

	h.wantState(StateSuspended)
	h.wantPrompts(ladderSends)
}

// A compaction asked for over a claim a suspension is already held over is taken for nothing:
// the hold's end stops the process more often than not, so the ten minutes of a model reading
// the transcript would be spent on a session that is about to go, and the compaction's own end
// would hand the claim to that hold rather than back to its agent. The claim is left exactly
// where the request found it, which here is ready with the operator's hold intact — a
// ready-time stop the runtime refused, waiting on its retry at the probe interval.
//
// It is refused in that state, as every other action refuses a held claim, so the api door
// answers the hold once for both recoveries rather than writing a second sentence of its own
// (RefusedError, api's refuseState and refuseHeldSuspension).
func TestACompactionOfAClaimASuspensionIsHeldOverIsNotTaken(t *testing.T) {
	h := newHarness(t)
	suspendedBy(h, SuspendedByDaemon)
	h.must(RequestRuleResume{Claim: testToken, Reason: "it owns its issue's phase"})
	if err := h.handle(RequestSuspend{Claim: testToken, Reason: "the operator suspended it", By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("operator suspend during launch = %v, want ErrSuspendHeld", err)
	}
	h.connect()
	h.register()
	h.rt.FailSuspend(errors.New("the runtime refused the stop"))
	if err := h.handle(RequestReady{Claim: testToken, Generation: h.generation(), Session: session}); err == nil {
		t.Fatal("ready with a failed operator suspend succeeded, want the runtime failure")
	}
	if got := h.claim(); got.State != StateReady || !got.SuspensionHeld {
		t.Fatalf("after the failed ready-time stop %+v, want ready with the operator hold intact", got)
	}
	h.rt.FailSuspend(nil)

	err := h.m.Compact(h.ctx, testCompactInstructions)

	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("a compaction of a claim a suspension is held over answered %v, want a *RefusedError", err)
	}
	if refused.State != StateReady {
		t.Fatalf("the refusal names state %s, want %s: the state the hold found the claim in",
			refused.State, StateReady)
	}
	if errors.Is(err, ErrCompactNotTaken) {
		t.Fatalf("the refusal answers %v, want no ErrCompactNotTaken: a held claim is refused in its state", err)
	}
	h.m.Wait()
	if asked := h.conn.Compactions(); len(asked) != 0 {
		t.Fatalf("Oh My Pi was asked %+v, want nothing: the claim was never taken", asked)
	}
	h.wantState(StateReady)
	if got := h.claim(); !got.SuspensionHeld || got.SuspensionHeldBy != SuspendedByOperator || got.Compacting {
		t.Fatalf("the refused claim holds %v by %q and reports Compacting %v, want the operator's hold and no compaction",
			got.SuspensionHeld, got.SuspensionHeldBy, got.Compacting)
	}

	// The hold's own retry is what the claim was left waiting for, and it lands.
	h.advance(testProbe)

	h.wantState(StateSuspended)
}

// restartedMidCompaction is the daemon that stopped while one of its claims was being compacted,
// and the one its operator started again: the claim is taken, Oh My Pi is still writing the
// summary (the compaction is held and never let go), and a new machine is built over the store
// as a restart builds one.
//
// What the new machine has of the compaction is nothing. It is memory only (compacting), the
// claim records only the working the take wrote (compactStart), and the shim carries no event of
// a compaction either: Oh My Pi answers the `compact` request, and that answer goes to the
// connection of the daemon that asked (shimwire.Compact). So the restored claim is the shape
// every other restored working claim is, and its recovery is that shape's: the daemon asks the
// agent whether the turn an earlier daemon saw start is still running (askTurnOver).
//
// The machine the stop left is dropped where it stands, as a stopped daemon's machines are. Its
// Compact waits on the held connection until the harness's context ends with the test.
func restartedMidCompaction(t *testing.T) *harness {
	t.Helper()
	notices := 0
	h := ladderStuck(t, &notices)
	h.conn.HoldCompact()
	stopped := h.m
	go func() { _ = stopped.Compact(h.ctx, testCompactInstructions) }()
	waitForCompacting(t, h)

	h.restart()

	if got := h.store.load(h.token).State; got != StateWorking {
		t.Fatalf("the restarted daemon reads the claim as %s, want the working the take wrote", got)
	}
	if h.m.View().Claim.Compacting {
		t.Fatal("the restored claim reports itself being compacted, want nothing of the compaction: it was the stopped daemon's memory")
	}
	return h
}

// A daemon that restarts while one of its sessions is being compacted asks that session's agent
// whether the turn an earlier daemon saw start is still running, and the agent answers that one
// is. Nothing is sent into it: the claim stays working and the task it owes waits, exactly as it
// waits for any turn this daemon did not see begin, and the end of that turn gives the claim
// back and sends it.
func TestARestartWhileASessionIsBeingCompactedSendsNothingWhileItsAgentIsInATurn(t *testing.T) {
	h := restartedMidCompaction(t)
	owed := h.pending().ID
	h.conn.SetStreaming(true)

	h.connect()

	h.wantState(StateWorking)
	h.wantPrompts(ladderSends)
	if got := h.pending(); got.ID != owed || !got.ConfirmedAt.IsZero() {
		t.Fatalf("the claim holds %+v, want the task %s still owed and unconfirmed", got, owed)
	}

	// Oh My Pi is done and the turn the agent reported ends.
	h.mustQuiet(StreamTurnEnd{Claim: h.token})

	h.wantState(StateIdle)
	prompts := h.wantPrompts(ladderSends + 1)
	if prompts[ladderSends].DeliveryID != owed {
		t.Fatalf("the send after that turn carried %q, want the task the claim owed, %q", prompts[ladderSends].DeliveryID, owed)
	}
}

// The same restart, and the agent answers that no turn is running — the answer the question can
// get while Oh My Pi is still writing the summary, since a compaction is not a turn of the agent
// and the daemon has no second word to ask about one. The restarted daemon ends the turn it was
// asking about and the owed task goes, into a session that may still be being compacted.
//
// That is the recovery compactStart states and the only one a restart can make: no store and no
// frame tells the new daemon a compaction is in flight (restartedMidCompaction), so the agent's
// own answer is the whole of what it has to go on, and a daemon that distrusted that answer
// would hold the claim working with nothing left to end it — the stuck worker this issue is
// about. What the recovery must not do is lose the task, and it does not: the task stays
// pending and unconfirmed under the id it was sent as, so a prompt a compaction swallows starts
// no turn and the sweep that follows sends it again (observe).
func TestARestartWhileASessionIsBeingCompactedSendsTheOwedTaskWhenItsAgentSaysNoTurnIsRunning(t *testing.T) {
	h := restartedMidCompaction(t)
	owed := h.pending().ID
	h.conn.SetStreaming(false)

	h.connect()

	h.wantState(StateIdle)
	prompts := h.wantPrompts(ladderSends + 1)
	if prompts[ladderSends].DeliveryID != owed {
		t.Fatalf("the send after the restart's question carried %q, want the task the claim owed, %q", prompts[ladderSends].DeliveryID, owed)
	}
	if got := h.pending(); got.ID != owed || !got.ConfirmedAt.IsZero() {
		t.Fatalf("the claim holds %+v, want the task %s kept unconfirmed: a prompt the compaction swallows starts no turn, and the sweep sends it again", got, owed)
	}
}
