package supervise

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The rows of this file are one boundary: a prompt of the claim's task and its turn around a
// compaction, which is Oh My Pi's handoff. The handoff aborts no turn, and the pinned Oh My Pi
// holds a prompt that reaches it while a handoff runs, admitting and acknowledging it only once the
// handoff ends. So whatever order the prompt's acknowledgement, its turn, its result and the
// compaction's end reach the machine in, the task is sent once and its own turn confirms it — and a
// send that ends before its prompt was written leaves the task unsent, whatever turn runs.

// unansweredPrompt is what the stream answers a prompt whose frame it wrote and whose answer did
// not come within its wait (stream's Conn.Prompt).
var unansweredPrompt = fmt.Errorf("worker-stream: prompt: %w: %w", runtime.ErrPromptUnanswered, runtime.ErrNoAnswer)

// The send of a phase task is still minting the role's identity when the compaction is taken, and
// the adoption of the working copy that follows runs out its wait: no prompt frame was written.
// The task is not sent, so it stays owed and unconfirmed (refused): an unrelated turn under the
// compaction is not taken for its own, since no prompt of it exists, and the idle release that
// follows keeps the task as it keeps any owed task. Kept as a prompt the handoff held, the task
// was confirmed by that unrelated turn and retired at the release, never having run.
func TestASendWhoseAdoptionRunsOutUnderTheHandoffStaysOwedThroughAnotherTurnAndAnIdleRelease(t *testing.T) {
	h := newHarness(t)
	minting, minted := make(chan struct{}, 1), make(chan struct{})
	h.deps.Identity = func(context.Context, claim.Role, string) (runtime.GitIdentity, error) {
		select {
		case minting <- struct{}{}:
		default:
		}
		<-minted
		return runtime.GitIdentity{Name: "legion-implementer[bot]", Email: "7+legion-implementer[bot]@users.noreply.github.com"}, nil
	}
	h.restart()
	h.reach(StateReady)
	h.rt.FailAdoptWorkingCopy(fmt.Errorf("worker-stream: adopt_working_copy: %w", runtime.ErrNoAnswer))
	if err := h.m.Handle(h.ctx, RequestDeliver{Claim: testToken, Task: "the task", Phase: phase.Implementing}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-minting:
	case <-time.After(5 * time.Second):
		t.Fatal("the send never began minting the role's identity")
	}
	release := h.conn.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	waitForCompacting(t, h)

	close(minted) // the adoption runs out its wait, before any prompt is written
	h.m.Wait()

	owed := h.pending()
	if !owed.ConfirmedAt.IsZero() {
		t.Fatalf("after the adoption ran out the claim holds %+v, want the task unconfirmed", owed)
	}
	h.wantPrompts(0)

	h.must(StreamTurnStart{Claim: testToken})
	h.must(StreamTurnEnd{Claim: testToken})
	if p := h.claim().Pending; p == nil || p.ID != owed.ID || !p.ConfirmedAt.IsZero() {
		t.Fatalf("after an unrelated turn the claim holds %+v, want %s owed and unconfirmed: no prompt of it was sent", p, owed.ID)
	}

	release()
	if err := <-compacted; err != nil {
		t.Fatalf("compact: %v", err)
	}
	h.m.Wait()
	h.must(RequestSuspend{Claim: testToken, By: SuspendedByReleased})
	h.wantState(StateSuspended)

	if p := h.claim().Pending; p == nil || p.Task != "the task" || !p.ConfirmedAt.IsZero() {
		t.Fatalf("after the idle release the claim holds %+v, want the task still owed and unconfirmed", p)
	}
	if stored := h.store.load(h.token).Pending; stored == nil || stored.Task != "the task" || !stored.ConfirmedAt.IsZero() {
		t.Fatalf("after the idle release the store holds %+v, want the task still owed and unconfirmed", stored)
	}
	h.wantPrompts(0)
}

// takeCompactionWithSendInFlight starts a ready claim's send of a task, holds it at a gated
// connection, and takes the compaction with that send in flight, held at the connection too. It
// answers the gated connection, the delivery id the send carries, the release of the compaction,
// and the channel its answer comes on. fail, when it is given, is what the send answers once the
// test lets it go.
func takeCompactionWithSendInFlight(t *testing.T, h *harness, fail error) (*gatedConn, string, func(), chan error) {
	t.Helper()
	h.reach(StateReady)
	gated := newGatedConn()
	if fail != nil {
		gated.FailPrompt(fail)
	}
	h.conns.Register(testToken, gated)
	if err := h.m.Handle(h.ctx, RequestDeliver{Claim: testToken, Task: "the task", Generation: 7}); err != nil {
		t.Fatal(err)
	}
	sent := waitFor(t, "the send", gated.entered)
	release := gated.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	deadline := time.Now().Add(5 * time.Second)
	for c := h.m.View().Claim; !c.Compacting || gated.CompactsHeld() == 0; c = h.m.View().Claim {
		if time.Now().After(deadline) {
			t.Fatal("the claim never reported itself being compacted with the compaction held at its connection")
		}
		time.Sleep(time.Millisecond)
	}
	return gated, sent, release, compacted
}

// sendRunsOutUnderTheHandoff lets the held send go, answering no acknowledgement within its wait:
// what the send sees when Oh My Pi holds its prompt until the handoff ends. Every later send the
// connection is given goes at once, so a second send the machine makes is counted rather than held.
func sendRunsOutUnderTheHandoff(t *testing.T, h *harness, gated *gatedConn, sent string) {
	t.Helper()
	gated.release <- struct{}{}
	go func() {
		for range gated.entered {
			gated.release <- struct{}{}
		}
	}()
	h.m.Wait()
	if p := h.pending(); p.ID != sent || !p.ConfirmedAt.IsZero() {
		t.Fatalf("after the send ran out the claim holds %+v, want %s owed and unconfirmed", p, sent)
	}
}

// The send's wait runs out while Oh My Pi holds its prompt behind the handoff, and the turn that
// prompt starts reaches the machine before the compaction's end. The prompt is still the task's
// (keepHeldPrompt), so that turn's start confirms the task: the worker calls the daemon inside
// that turn, and the entry those calls serve is the confirmed task's. Read as lost, the send left
// nothing to take the turn for the task's, and the task stayed unconfirmed through its own turn.
func TestAPromptHeldPastItsWaitByTheHandoffIsConfirmedByATurnBeforeTheCompactionsEnd(t *testing.T) {
	h := newHarness(t)
	gated, sent, release, compacted := takeCompactionWithSendInFlight(t, h, unansweredPrompt)
	sendRunsOutUnderTheHandoff(t, h, gated, sent)

	h.must(StreamTurnStart{Claim: testToken})

	if p := h.claim().Pending; p == nil || p.ID != sent || p.ConfirmedAt.IsZero() {
		t.Fatalf("after its turn started the claim holds %+v, want %s confirmed", p, sent)
	}
	h.must(StreamTurnEnd{Claim: testToken})
	release()
	if err := <-compacted; err != nil {
		t.Fatalf("compact: %v", err)
	}
	h.m.Wait()
	h.must(StreamPromptResult{Claim: testToken, DeliveryID: sent, AgentInvoked: true})

	if c := h.claim(); c.Pending != nil || c.ServingGeneration != 7 {
		t.Fatalf("after the prompt's result: pending %+v serving %d, want the task retired and run 7 served", c.Pending, c.ServingGeneration)
	}
	if prompts := gated.Prompts(); len(prompts) != 1 {
		t.Fatalf("the task was prompted %d times, %+v, want once", len(prompts), prompts)
	}
}

// The same held send, and the compaction's end reaches the machine before the prompt's turn. The
// claim given back does not prompt the task a second time: the held prompt is still the task's,
// and the wait for its turn starts at the compaction's end (compactEnded, sendPending). The turn
// that then starts confirms the task, and its result settles it. Read as lost, the send was made
// again at the compaction's end.
func TestAPromptHeldPastItsWaitByTheHandoffIsSentOnceWhenTheCompactionEndsFirst(t *testing.T) {
	h := newHarness(t)
	gated, sent, release, compacted := takeCompactionWithSendInFlight(t, h, unansweredPrompt)
	sendRunsOutUnderTheHandoff(t, h, gated, sent)

	release()
	if err := <-compacted; err != nil {
		t.Fatalf("compact: %v", err)
	}
	h.m.Wait()

	if prompts := gated.Prompts(); len(prompts) != 1 {
		t.Fatalf("after the compaction's end the task was prompted %d times, %+v, want once", len(prompts), prompts)
	}
	if got := h.state(); got != StateReady {
		t.Fatalf("the claim is %s after the compaction, want the ready it was", got)
	}
	h.must(StreamTurnStart{Claim: testToken})
	if p := h.claim().Pending; p == nil || p.ID != sent || p.ConfirmedAt.IsZero() {
		t.Fatalf("after its turn started the claim holds %+v, want %s confirmed", p, sent)
	}
	h.must(StreamTurnEnd{Claim: testToken})
	h.must(StreamPromptResult{Claim: testToken, DeliveryID: sent, AgentInvoked: true})

	if c := h.claim(); c.Pending != nil || c.ServingGeneration != 7 {
		t.Fatalf("after the prompt's result: pending %+v serving %d, want the task retired and run 7 served", c.Pending, c.ServingGeneration)
	}
	if prompts := gated.Prompts(); len(prompts) != 1 {
		t.Fatalf("the task was prompted %d times, %+v, want once", len(prompts), prompts)
	}
}

// The same held send, and no turn starts within the bound of the compaction's end. Nothing says Oh
// My Pi ever took the prompt, so the wait running out charges it nothing, and the send goes again
// under the same id (noTurn): the shim answers a prompt it has a record of from that record,
// without a second turn.
func TestAPromptHeldPastItsWaitWhoseTurnNeverStartsGoesAgainUncharged(t *testing.T) {
	h := newHarness(t)
	gated, sent, release, compacted := takeCompactionWithSendInFlight(t, h, unansweredPrompt)
	sendRunsOutUnderTheHandoff(t, h, gated, sent)
	release()
	if err := <-compacted; err != nil {
		t.Fatalf("compact: %v", err)
	}
	h.m.Wait()
	before := h.claim().Budgets

	h.advance(2 * testRPC)
	h.m.Wait()

	if got := h.claim().Budgets; got != before {
		t.Fatalf("the claim's budgets are %+v after the held prompt's wait ran out, want %+v: it is charged nothing", got, before)
	}
	prompts := gated.Prompts()
	if len(prompts) != 2 || prompts[1].DeliveryID != sent {
		t.Fatalf("the prompts are %+v, want the task and then the task again under %s", prompts, sent)
	}
}

// The ladder has just sent the owed task and Oh My Pi has accepted the prompt — acknowledged,
// with the wait for its turn armed — when the compaction is taken. The compaction is a handoff,
// which aborts no turn (dispatch://INFRA-5798 R1): the turn of a prompt Oh My Pi has admitted is
// the task's whatever the handoff does, so its start confirms the task. Its result settles the task, and
// nothing sends it again: taken back and sent again, as the daemon did around Oh My Pi's
// `compact`, which aborted that turn, the task would reach the agent twice.
func TestACompactionTakenWhileTheOwedPromptIsInFlightLeavesTheTaskToThatPromptsTurn(t *testing.T) {
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
	if owed := h.claim().Pending; owed == nil || owed.ID != sent {
		t.Fatalf("the compaction holds the claim with %+v, want the task left under the id its accepted prompt carries, %q", owed, sent)
	}

	h.must(StreamTurnStart{Claim: h.token})
	// The worker calls the daemon inside this turn, so its start confirms the task: the entry
	// those calls serve is the confirmed task's (Claim.ServingEntry).
	if owed := h.claim().Pending; owed == nil || owed.ID != sent || owed.ConfirmedAt.IsZero() {
		t.Fatalf("the claim holds %+v after its accepted prompt's turn started under the compaction, want %s confirmed", owed, sent)
	}
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	h.must(StreamPromptResult{Claim: h.token, DeliveryID: sent, AgentInvoked: true})

	release()
	if err := <-compacted; err != nil {
		t.Fatalf("compact: %v", err)
	}
	h.m.Wait()
	if owed := h.claim().Pending; owed != nil {
		t.Fatalf("the claim still owes %+v after the turn of the task's accepted prompt settled it", owed)
	}
	if got := h.state(); got != StateIdle {
		t.Fatalf("the claim is %s, want it idle once the compaction and the turn are over", got)
	}
	h.wantPrompts(ladderSends + 1)
}

// The same accepted prompt, and Oh My Pi refuses the handoff at once, as the pinned build refuses
// one asked for while a turn runs, a turn counting as running from the prompt's admission. The
// compaction's end gives the claim back with the prompt's turn still to start, and sends nothing:
// the task was sent, and the wait for its turn holds the send (compactEnded, sendPending). The
// turn that follows is the task's, and its result settles it.
func TestACompactionRefusedWhileTheOwedPromptIsAcceptedSendsTheTaskNoSecondTime(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	h.advance(promptRetryMax) // the ladder's next step sends the owed task
	sent := h.wantPrompts(ladderSends + 1)[ladderSends].DeliveryID
	const inATurn = "Cannot hand off while a response is in progress"
	h.conn.RefuseCompact(inATurn)

	err := h.m.Compact(h.ctx, testCompactInstructions)

	if reason := runtimeRefusalReason(err); reason != inATurn {
		t.Fatalf("compact answered %v, want Oh My Pi's own refusal", err)
	}
	h.m.Wait()
	if got := h.state(); got != StateIdle {
		t.Fatalf("the claim is %s after the refused compaction, want it idle waiting for the turn", got)
	}
	h.wantPrompts(ladderSends + 1)
	if owed := h.claim().Pending; owed == nil || owed.ID != sent {
		t.Fatalf("the claim holds %+v after the refused compaction, want the task under the id its accepted prompt carries, %q", owed, sent)
	}

	h.must(StreamTurnStart{Claim: h.token})
	h.must(StreamTurnEnd{Claim: h.token})

	if owed := h.claim().Pending; owed != nil {
		t.Fatalf("the claim still owes %+v after the turn of the task's accepted prompt", owed)
	}
	h.wantPrompts(ladderSends + 1)
}

// The wait for the accepted prompt's turn runs out while Oh My Pi writes the handoff, and a
// working claim ignores it. The handoff's end arms it again, from the moment the turn can start:
// Oh My Pi starts no turn while it writes the handoff document, so the turn has not been late, and the
// claim given back does not prompt the task a second time (compactEnded).
func TestTheWaitForTheAcceptedPromptsTurnStartsAgainWhenTheCompactionEnds(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	h.advance(promptRetryMax) // the ladder's next step sends the owed task
	sent := h.wantPrompts(ladderSends + 1)[ladderSends].DeliveryID
	release := h.conn.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	waitForCompacting(t, h)

	h.advance(2 * testRPC) // the wait armed at the acknowledgement runs out under the compaction

	release()
	if err := <-compacted; err != nil {
		t.Fatalf("compact: %v", err)
	}
	h.m.Wait()
	h.wantPrompts(ladderSends + 1)
	if owed := h.claim().Pending; owed == nil || owed.ID != sent || !owed.ConfirmedAt.IsZero() {
		t.Fatalf("the claim holds %+v after the compaction, want the task unconfirmed under the id its accepted prompt carries, %q", owed, sent)
	}
	h.advance(testRPC / 2)
	h.wantPrompts(ladderSends + 1)

	h.must(StreamTurnStart{Claim: h.token})
	h.must(StreamTurnEnd{Claim: h.token})

	if owed := h.claim().Pending; owed != nil {
		t.Fatalf("the claim still owes %+v after the turn of the task's accepted prompt", owed)
	}
	h.wantPrompts(ladderSends + 1)
}

// The send of a task is still on its way to Oh My Pi when the compaction is taken, and Oh My Pi's
// acknowledgement lands while the handoff runs. No turn came first for it, so it is the prompt
// taken as any other is: the task is marked read and waits for its turn (ackedInTurn, acked).
// The compaction's end gives the claim back with that turn still to start and sends the task no
// second time; the turn that follows is the task's, and its result settles it.
func TestAnAcknowledgementThatLandsUnderTheCompactionIsTheTaskSent(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	gated := newGatedConn()
	h.conns.Register(testToken, gated)
	if err := h.m.Handle(h.ctx, RequestDeliver{Claim: testToken, Task: "the task"}); err != nil {
		t.Fatal(err)
	}
	sent := waitFor(t, "the send", gated.entered)
	release := gated.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	deadline := time.Now().Add(5 * time.Second)
	for c := h.m.View().Claim; !c.Compacting || gated.CompactsHeld() == 0; c = h.m.View().Claim {
		if time.Now().After(deadline) {
			t.Fatal("the claim never reported itself being compacted with the compaction held at its connection")
		}
		time.Sleep(time.Millisecond)
	}

	gated.release <- struct{}{}
	h.m.Wait()

	if p := h.pending(); p.ID != sent || p.DeliveredAt.IsZero() || !p.ConfirmedAt.IsZero() {
		t.Fatalf("the claim holds %+v after the acknowledgement under the compaction, want %s read and unconfirmed", p, sent)
	}
	release()
	if err := <-compacted; err != nil {
		t.Fatalf("compact: %v", err)
	}
	h.m.Wait()
	if got := h.state(); got != StateReady {
		t.Fatalf("the claim is %s after the compaction, want the ready it was", got)
	}
	if prompts := gated.Prompts(); len(prompts) != 1 {
		t.Fatalf("the task was prompted %d times, %+v, want once", len(prompts), prompts)
	}

	h.must(StreamTurnStart{Claim: testToken})
	h.must(StreamTurnEnd{Claim: testToken})

	if owed := h.claim().Pending; owed != nil {
		t.Fatalf("the claim still owes %+v after the turn of the task's accepted prompt", owed)
	}
	if prompts := gated.Prompts(); len(prompts) != 1 {
		t.Fatalf("the task was prompted %d times, %+v, want once", len(prompts), prompts)
	}
}

// The accepted prompt's turn starts and ends while Oh My Pi still writes the handoff, and its
// result comes only after the compaction's end. The start confirmed the task, so the compaction's
// end arms no wait for a turn that has already run, and nothing is charged for a turn that did
// not start: the result, when it comes, settles the task.
func TestATurnThatRanUnderTheCompactionIsChargedNothingWhileItsResultIsLate(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	h.advance(promptRetryMax) // the ladder's next step sends the owed task
	sent := h.wantPrompts(ladderSends + 1)[ladderSends].DeliveryID
	before := h.claim().Budgets
	release := h.conn.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	waitForCompacting(t, h)

	h.must(StreamTurnStart{Claim: h.token})
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	release()
	if err := <-compacted; err != nil {
		t.Fatalf("compact: %v", err)
	}
	h.m.Wait()
	h.advance(2 * testRPC) // past the wait a turn that never started would be charged at

	if got := h.claim().Budgets; got != before {
		t.Fatalf("the claim's budgets are %+v after a turn that ran under the compaction, want %+v: nothing was charged", got, before)
	}
	if owed := h.claim().Pending; owed == nil || owed.ID != sent || owed.ConfirmedAt.IsZero() {
		t.Fatalf("the claim holds %+v, want %s confirmed and waiting for its prompt's result", owed, sent)
	}
	h.wantPrompts(ladderSends + 1)

	h.must(StreamPromptResult{Claim: h.token, DeliveryID: sent, AgentInvoked: true})

	if owed := h.claim().Pending; owed != nil {
		t.Fatalf("the claim still owes %+v after its prompt's result", owed)
	}
	h.wantPrompts(ladderSends + 1)
}

// A prompt's whole turn and result can reach the machine before the send's own acknowledgement
// while a compaction keeps the claim working. promptResulted marks the send answered, so
// fenceOutcome drops that late acknowledgement before acked and takeAck read it. The compaction's
// end then retires the task: no task stays owed, the claim is idle, and no second prompt goes out.
func TestAnAcknowledgementAfterItsPromptsResultUnderTheCompactionIsDropped(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	gated := newGatedConn()
	h.conns.Register(testToken, gated)
	if err := h.m.Handle(h.ctx, RequestDeliver{Claim: testToken, Task: "the task"}); err != nil {
		t.Fatal(err)
	}
	sent := waitFor(t, "the send", gated.entered)
	release := gated.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	deadline := time.Now().Add(5 * time.Second)
	for c := h.m.View().Claim; !c.Compacting || gated.CompactsHeld() == 0; c = h.m.View().Claim {
		if time.Now().After(deadline) {
			t.Fatal("the claim never reported itself being compacted with the compaction held at its connection")
		}
		time.Sleep(time.Millisecond)
	}
	for _, ev := range []Event{
		StreamTurnStart{Claim: testToken},
		StreamTurnEnd{Claim: testToken},
		StreamPromptResult{Claim: testToken, DeliveryID: sent, AgentInvoked: true},
	} {
		if err := h.m.Handle(h.ctx, ev); err != nil {
			t.Fatalf("handle %T: %v", ev, err)
		}
	}

	gated.release <- struct{}{}
	h.m.Wait()
	release()
	if err := <-compacted; err != nil {
		t.Fatalf("compact: %v", err)
	}
	h.m.Wait()

	if owed := h.claim().Pending; owed != nil {
		t.Fatalf("the claim still owes %+v after its prompt's turn and result, and the compaction's end", owed)
	}
	if got := h.state(); got != StateIdle {
		t.Fatalf("the claim is %s, want it idle once the compaction and the turn are over", got)
	}
	if prompts := gated.Prompts(); len(prompts) != 1 {
		t.Fatalf("the task was prompted %d times, %+v, want once", len(prompts), prompts)
	}
}

// The send of a task is still adopting the working copy when the compaction is taken, and an
// Envoy turn starts and ends while Oh My Pi writes the handoff. That turn is taken for the task's,
// since the task's send is in flight (turnAnswers), so it confirms the task; but only the send's
// outcome says whether the prompt went out at all, so neither the turn's end nor the
// compaction's end retires it (settle). The send then fails, and its refusal takes the
// confirmation back: the task is owed again rather than retired unsent.
func TestAForeignTurnUnderTheCompactionRetiresNothingWhileTheSendIsInFlight(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	gated := newGatedConn()
	gated.FailPrompt(errBoom)
	h.conns.Register(testToken, gated)
	if err := h.m.Handle(h.ctx, RequestDeliver{Claim: testToken, Task: "the task"}); err != nil {
		t.Fatal(err)
	}
	sent := waitFor(t, "the send", gated.entered)
	release := gated.HoldCompact()
	compacted := make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	deadline := time.Now().Add(5 * time.Second)
	for c := h.m.View().Claim; !c.Compacting || gated.CompactsHeld() == 0; c = h.m.View().Claim {
		if time.Now().After(deadline) {
			t.Fatal("the claim never reported itself being compacted with the compaction held at its connection")
		}
		time.Sleep(time.Millisecond)
	}
	for _, ev := range []Event{StreamTurnStart{Claim: testToken}, StreamTurnEnd{Claim: testToken}} {
		if err := h.m.Handle(h.ctx, ev); err != nil {
			t.Fatalf("handle %T: %v", ev, err)
		}
	}
	release()
	if err := <-compacted; err != nil {
		t.Fatalf("compact: %v", err)
	}
	if p := h.claim().Pending; p == nil || p.ID != sent {
		t.Fatalf("the claim holds %+v after the foreign turn and the compaction ended, want %s kept while its send is in flight", p, sent)
	}

	gated.release <- struct{}{}
	h.m.Wait()

	if p := h.pending(); p.ID != sent || !p.ConfirmedAt.IsZero() {
		t.Fatalf("pending %+v after the send failed, want %s unconfirmed and owed", p, sent)
	}
}
