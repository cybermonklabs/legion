package supervise

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// compactInstructions stands for what the API sends with every compaction: the instructions
// keep the reviewer's entry in Oh My Pi's handoff document, and Machine.Compact refuses a
// compaction that carries none.
const testCompactInstructions = "Write the summary as text."

// ladderStuck is the claim the provider-errors notice names, idle between the ladder's takes:
// six turns of it in a row ended in a provider error, so the notice at providerErrorRunTold has
// gone once, the task it owes is taken back on the claim, and the ladder's next step is ten
// minutes away. ladderSends is how many takes the worker has had accepted by then. set, when it
// is given, changes the dependencies before the machine is built, as workingClaim's own does.
const ladderSends = providerErrorRunTold

func ladderStuck(t *testing.T, notices *int, set ...func(*Deps)) *harness {
	t.Helper()
	h := workingClaim(t, append([]func(*Deps){func(d *Deps) {
		d.ProviderErrorRunReached = func(Claim, string) { *notices++ }
	}}, set...)...)
	for turn := range providerErrorRunTold {
		h.errorTurn(ProviderError{Message: "overloaded_error", Status: 529})
		if turn+1 < providerErrorRunTold {
			h.climbs(turn+1, promptRetryDelay(turn+1))
		}
	}
	if got := h.claim().ProviderErrorRun; got != providerErrorRunTold {
		t.Fatalf("the run of provider errors is %d, want the %d at which it is handed on", got, providerErrorRunTold)
	}
	if *notices != 1 {
		t.Fatalf("the run was handed on %d times, want once", *notices)
	}
	if got := h.state(); got != StateIdle {
		t.Fatalf("the claim is %s, want it idle between the ladder's sends", got)
	}
	h.wantPrompts(ladderSends)
	return h
}

// A compaction that ran starts the claim's run of provider-error turns again, so the notice at
// providerErrorRunTold goes a second time after six more refused turns. Without the reset it
// would go once and not again until another of the run's ends (Claim.ProviderErrorRun), and a
// compaction the controller asked for would buy it no second look.
//
// The owed task goes again at once rather than at the end of the ladder step the compaction
// interrupted: the next step of that ladder is ten minutes away, and putting the task in front
// of the shortened session is the whole point of the operation.
func TestACompactionStartsTheRunOfProviderErrorsAgainSoItIsHandedOnASecondTime(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	sent := ladderSends

	if err := h.m.Compact(h.ctx, testCompactInstructions); err != nil {
		t.Fatalf("compact: %v", err)
	}
	h.m.Wait()
	h.workerAsks()

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
		h.errorTurn(ProviderError{Message: "overloaded_error", Status: 529})
		if turn+1 < providerErrorRunTold {
			h.climbs(sent+1+turn, promptRetryMax)
		}
	}

	if notices != 2 {
		t.Fatalf("the runs were handed on %d times, want 2: once for each of the two runs that reached %d", notices, providerErrorRunTold)
	}
}

// A handoff Oh My Pi refused resets nothing: the session is as it was, so the run of refused
// turns it carried still stands and the notice does not go again for it. The owed task goes
// again all the same.
func TestACompactionThatWasRefusedKeepsTheRunOfProviderErrors(t *testing.T) {
	notices := 0
	h := ladderStuck(t, &notices)
	sent := ladderSends
	h.conn.RefuseCompact("Nothing to hand off (already compacted)")

	err := h.m.Compact(h.ctx, testCompactInstructions)

	if reason := runtimeRefusalReason(err); reason != "Nothing to hand off (already compacted)" {
		t.Fatalf("compact answered %v, want Oh My Pi's own refusal", err)
	}
	h.m.Wait()
	h.workerAsks()
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
	h.workerAsks()
	if got := h.state(); got != StateIdle {
		t.Fatalf("the claim is %s after the compaction, want idle", got)
	}
	prompts := h.wantPrompts(sent + 1)
	if prompts[sent].DeliveryID != "newer" {
		t.Fatalf("the send after the compaction carried %q, want the newer task", prompts[sent].DeliveryID)
	}
}

// waitForCompacting waits until the machine reports the claim being compacted and the compaction
// has reached the connection held: the machine reports the claim compacting as it takes it,
// before it asks Oh My Pi (Machine.Compact), so a test that moves the claim at that report alone
// can move it before the ask and read the answer of a compaction that never began.
func waitForCompacting(t *testing.T, h *harness) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c := h.m.View().Claim; c.Compacting && c.State == StateWorking && h.conn.CompactsHeld() > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the claim never reported itself being compacted with the compaction held at its connection")
}

// A turn that ends while the session is being compacted leaves the claim exactly where it is: a
// turn can start and end under the compaction, a background turn among them, and taking its end
// for the compaction's end would hand the claim its owed task while Oh My Pi is still writing the
// handoff document.
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
	h.workerAsks()
	h.wantPrompts(sent + 1)
}

// A compaction asked for with no instructions is refused before anything is sent: the
// instructions are what keep the reviewer's entry in Oh My Pi's handoff document.
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

// handoffRefusedInATurn is what the pinned Oh My Pi answers a handoff asked for while a turn
// runs (`case "handoff"` in packages/coding-agent/src/modes/rpc/rpc-mode.ts of
// cybermonklabs/oh-my-pi at the pinned tag).
const handoffRefusedInATurn = "Cannot hand off while a response is in progress"

// takenBeforeACompaction is ladderStuck, on the current plugin, with the ladder's next step come:
// the owed task's value written again and the worker's take of it accepted, its turn still to
// come. It answers that take. This is what a compaction finds when it is asked for after the take
// and before its turn (departure 10 of dispatch://INFRA-2811's plan).
func takenBeforeACompaction(t *testing.T) (*harness, taken) {
	t.Helper()
	notices := 0
	h := ladderStuck(t, &notices, currentPlugin)
	h.advance(promptRetryMax)
	sent := h.wantPrompts(ladderSends + 1)[ladderSends]
	if p := h.pending(); p.Number != sent.Number || !p.awaitsStart() {
		t.Fatalf("after the ladder's step the claim holds %+v, want task %d's take awaiting its turn", p, sent.Number)
	}
	return h, sent
}

// compactHeld starts a compaction whose answer the claim's connection holds, and answers the call
// that lets the answer through and the channel Machine.Compact answers on.
func compactHeld(t *testing.T, h *harness) (release func(), compacted chan error) {
	t.Helper()
	release = h.conn.HoldCompact()
	compacted = make(chan error, 1)
	go func() { compacted <- h.m.Compact(h.ctx, testCompactInstructions) }()
	waitForCompacting(t, h)
	return release, compacted
}

// wantTakeKept holds that the accepted take of sent stands as accepted left it, in memory and in
// the store: the same delivery and number, accepted at the same moment, its text kept, its turn
// still to come, and no note of a cut turn.
func (h *harness) wantTakeKept(sent taken, accepted Delivery, when string) {
	h.t.Helper()
	p := h.pending()
	if p.ID != sent.DeliveryID || p.Number != sent.Number || !p.awaitsStart() || !p.ConfirmedAt.Equal(accepted.ConfirmedAt) ||
		p.TakenText != accepted.TakenText || p.Interrupted {
		h.t.Fatalf("%s the claim holds %+v, want task %d's take kept as it was accepted, %+v", when, p, sent.Number, accepted)
	}
	stored, ok := h.store.delivery(h.token)
	if !ok || !stored.awaitsStart() || !stored.ConfirmedAt.Equal(accepted.ConfirmedAt) || stored.TakenText != accepted.TakenText {
		h.t.Fatalf("%s the store holds %+v, want task %d's take kept as it was accepted", when, stored, sent.Number)
	}
}

// Row H1 of departure 10: the worker's take of the owed task is accepted, its turn still to come,
// when the compaction is asked for, and the compaction's end reaches the machine before that
// turn's start. The compaction is Oh My Pi's handoff, which aborts no turn: the pinned Oh My Pi
// parks the text the worker hands it while the handoff runs, and starts its turn once the handoff
// is over. So the take stands — its acceptance, its text and the handshake's confirmation — and
// nothing offers the task again, whether Oh My Pi wrote the handoff or refused it; the take's
// turn, when it starts, is the take's, and its report settles the task once. Taken back, as the
// daemon did around Oh My Pi's `compact`, the turn's report would name a task no take held, and
// the task would be offered and run a second time.
//
// Row H5 of departure 10 is the task no take holds: a take of it while the compaction runs is
// refused, the compaction's end writes its value once, and the worker takes it once. A claim that
// owes no task is offered nothing.
func TestTakeAcceptedBeforeACompactionIsKeptAndRunsOnce(t *testing.T) {
	for _, tc := range []struct{ name, refusal string }{
		{"the handoff shortens the session", ""},
		{"Oh My Pi refuses the handoff", handoffRefusedInATurn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, sent := takenBeforeACompaction(t)
			accepted, handshake, writes := h.pending(), h.claim().Handshake, len(h.values.written())
			var err error
			if tc.refusal == "" {
				release, compacted := compactHeld(t, h)
				h.wantTakeKept(sent, accepted, "under the compaction")
				release()
				err = <-compacted
			} else {
				h.conn.RefuseCompact(tc.refusal)
				err = h.m.Compact(h.ctx, testCompactInstructions)
			}
			if got := runtimeRefusalReason(err); got != tc.refusal || (tc.refusal == "" && err != nil) {
				t.Fatalf("compact answered %v, want %q", err, tc.refusal)
			}
			h.m.Wait()
			h.workerAsks()

			h.wantState(StateIdle)
			h.wantTakeKept(sent, accepted, "after the compaction's end")
			if got := h.claim().Handshake; got != handshake {
				t.Fatalf("the handshake is %+v after the compaction, want %+v: the take's confirmation stands", got, handshake)
			}
			if got := len(h.values.written()); got != writes {
				t.Fatalf("%d values were written over the compaction, want none: the take is accepted, so nothing is offered", got-writes)
			}
			h.wantPrompts(ladderSends + 1)

			h.must(StreamTurnStart{Claim: h.token})
			if p := h.pending(); p.Number != sent.Number || !p.startRecorded() {
				t.Fatalf("after the turn's start the claim holds %+v, want task %d's take given that turn", p, sent.Number)
			}
			h.must(StreamTurnEnd{Claim: h.token})

			if p := h.claim().Pending; p != nil {
				t.Fatalf("after the turn's report the claim holds %+v, want task %d finished", p, sent.Number)
			}
			if got := h.claim().TaskFinished; got != sent.Number {
				t.Fatalf("the claim finished task %d, want %d", got, sent.Number)
			}
			h.wantFinishedOnce(finishedByReport)
			h.wantPrompts(ladderSends + 1)
		})
	}
	t.Run("a task no take holds is written once and taken once at the compaction's end", func(t *testing.T) {
		notices := 0
		h := ladderStuck(t, &notices)
		owed := h.pending()
		h.manualTakes()
		release, compacted := compactHeld(t, h)
		h.wantRefused(HoldInTurn)
		writes := len(h.values.written())

		release()
		if err := <-compacted; err != nil {
			t.Fatalf("compact: %v", err)
		}
		h.m.Wait()

		if got := len(h.values.written()); got != writes+1 {
			t.Fatalf("%d values were written at the compaction's end, want one", got-writes)
		}
		h.mustTake()
		if p := h.pending(); p.Number != owed.Number || !p.awaitsStart() {
			t.Fatalf("after the take the claim holds %+v, want task %d's take awaiting its turn", p, owed.Number)
		}
		if got := len(h.values.written()); got != writes+1 {
			t.Fatalf("%d values were written by the take, want only the one the compaction's end wrote", got-writes)
		}
	})
	t.Run("a claim that owes no task is offered nothing at the compaction's end", func(t *testing.T) {
		h := waitsHarness(t, currentPlugin)
		h.reach(StateIdle)
		takes, writes := len(h.prompts()), len(h.values.written())

		if err := h.m.Compact(h.ctx, testCompactInstructions); err != nil {
			t.Fatalf("compact: %v", err)
		}
		h.m.Wait()
		h.workerAsks()

		if p := h.claim().Pending; p != nil {
			t.Fatalf("the claim holds %+v after the compaction, want no task", p)
		}
		if got := len(h.values.written()); got != writes {
			t.Fatalf("%d values were written at the compaction's end, want none", got-writes)
		}
		h.wantPrompts(takes)
	})
}

// startLinesOf counts the start lines of turns given task number (startTurn).
func (h *harness) startLinesOf(number uint64) int {
	n := 0
	for _, r := range h.records.of(turnStartedLine) {
		if task, ok := r.attr("task"); ok && task.String() == strconv.FormatUint(number, 10) {
			n++
		}
	}
	return n
}

// Row H2 of departure 10: the take's turn starts while the handoff still runs — Oh My Pi parked
// the take's text and started it as the handoff ended, before the handoff's answer reached the
// machine — so the start is the take's, decided and written as any start of a ready or idle
// claim is (turnStarted): its line names the task, the delivery records the turn started, and the
// claim stays working for that turn past the compaction's end. Its end and report may come before
// that end, or the end before it and the report after; the report settles the task once either way,
// and a finished task does not come back at the compaction's end. A foreign turn the start was
// given to, whose report says the take's text was held through it, gives the take its turn back,
// and the take's own turn settles it.
func TestTakeWhoseTurnStartsBeforeTheCompactionEndsIsGivenThatTurn(t *testing.T) {
	started := func(t *testing.T, h *harness, sent taken) {
		t.Helper()
		before := h.startLinesOf(sent.Number)
		h.must(StreamTurnStart{Claim: h.token})
		if p := h.pending(); p.Number != sent.Number || !p.startRecorded() {
			t.Fatalf("after the start under the compaction the claim holds %+v, want task %d's take given that turn", p, sent.Number)
		}
		if stored, ok := h.store.delivery(h.token); !ok || !stored.startRecorded() {
			t.Fatalf("after the start under the compaction the store holds %+v, want the take's turn written started", stored)
		}
		if got := h.startLinesOf(sent.Number) - before; got != 1 {
			t.Fatalf("the start under the compaction wrote %d start lines naming task %d, want one", got, sent.Number)
		}
		h.wantState(StateWorking)
	}
	ended := func(t *testing.T, h *harness, release func(), compacted chan error) {
		t.Helper()
		release()
		if err := <-compacted; err != nil {
			t.Fatalf("compact: %v", err)
		}
		h.m.Wait()
		h.workerAsks()
	}
	settledOnce := func(t *testing.T, h *harness, sent taken) {
		t.Helper()
		if p := h.claim().Pending; p != nil {
			t.Fatalf("the claim holds %+v, want task %d finished", p, sent.Number)
		}
		h.wantFinishedOnce(finishedByReport)
		h.wantPrompts(ladderSends + 1)
		if value, held := h.values.value(h.token); held && value.Number == sent.Number {
			t.Fatalf("the bucket holds %+v, want task %d's value deleted", value, sent.Number)
		}
	}
	t.Run("its turn runs on past the compaction's end", func(t *testing.T) {
		h, sent := takenBeforeACompaction(t)
		release, compacted := compactHeld(t, h)
		started(t, h, sent)
		ended(t, h, release, compacted)
		h.wantState(StateWorking)
		h.must(StreamTurnEnd{Claim: h.token})
		h.wantState(StateIdle)
		settledOnce(t, h, sent)
	})
	t.Run("its end and its report come before the compaction's end", func(t *testing.T) {
		h, sent := takenBeforeACompaction(t)
		release, compacted := compactHeld(t, h)
		started(t, h, sent)
		h.must(StreamTurnEnd{Claim: h.token})
		if p := h.claim().Pending; p != nil {
			t.Fatalf("after the report under the compaction the claim holds %+v, want task %d finished", p, sent.Number)
		}
		ended(t, h, release, compacted)
		h.wantState(StateIdle)
		settledOnce(t, h, sent)
	})
	t.Run("its end comes before the compaction's end and its report after", func(t *testing.T) {
		h, sent := takenBeforeACompaction(t)
		release, compacted := compactHeld(t, h)
		started(t, h, sent)
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		ended(t, h, release, compacted)
		h.wantState(StateIdle)
		if p := h.pending(); p.Number != sent.Number || !p.startRecorded() {
			t.Fatalf("after the compaction's end the claim holds %+v, want task %d's turn run and its report to come", p, sent.Number)
		}
		h.mustReport(nil)
		settledOnce(t, h, sent)
	})
	t.Run("a foreign turn whose report says the take's text was held through it", func(t *testing.T) {
		h, sent := takenBeforeACompaction(t)
		release, compacted := compactHeld(t, h)
		started(t, h, sent)
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.worker.sequence++
		h.must(RequestTurnOutcome{Claim: h.token, Generation: h.generation(), Sequence: h.worker.sequence, Held: sent.Number})
		if p := h.pending(); p.Number != sent.Number || !p.awaitsStart() {
			t.Fatalf("after the foreign turn's report the claim holds %+v, want task %d's take awaiting its turn again", p, sent.Number)
		}
		ended(t, h, release, compacted)
		h.must(StreamTurnStart{Claim: h.token})
		h.must(StreamTurnEnd{Claim: h.token})
		settledOnce(t, h, sent)
	})
}

// Row H3 of departure 10: the claim's view of the worker's turns is not whole when the handoff
// starts — a hello's ask still waits for its answer — or the connection cuts while the handoff
// runs. Nothing takes the take back, and nothing gives it a note. An answer that the agent is in
// a turn, under the compaction, is the take's turn starting, given by the rule of every start
// (turnStarted), once: a replayed start frame after it is the same turn. An answer that the agent
// is in no turn leaves the take awaiting its turn. A turn's end the daemon never saw begin, under
// the compaction, marks the take as one whose turn may have run unseen, as such an end does
// outside one (blindTurnEnded), and a later let-go of the process takes it back as a cut turn.
func TestTakeKeptByACompactionOverACutViewIsGivenTheTurnTheAnswerReports(t *testing.T) {
	cuts := []struct {
		name string
		cut  func(t *testing.T, h *harness) (func(), chan error)
	}{
		{"the view is cut when the handoff starts", func(t *testing.T, h *harness) (func(), chan error) {
			h.reconnectLater()
			h.helloOf(true, false)
			return compactHeld(t, h)
		}},
		{"the connection cuts while the handoff runs", func(t *testing.T, h *harness) (func(), chan error) {
			release, compacted := compactHeld(t, h)
			h.reconnectLater()
			h.helloOf(true, false)
			return release, compacted
		}},
	}
	for _, cut := range cuts {
		t.Run(cut.name, func(t *testing.T) {
			t.Run("an answer of a turn, then the turn's replayed start", func(t *testing.T) {
				h := boundsHarness(t)
				number := h.takeOf("implement the plan")
				release, compacted := cut.cut(t, h)

				h.answerAsk(true)
				if p := h.pending(); p.Number != number || !p.startRecorded() || p.Interrupted {
					t.Fatalf("after the answer the claim holds %+v, want task %d's take given the turn the answer reports", p, number)
				}
				h.must(StreamTurnStart{Claim: h.token})
				if got := h.startLinesOf(number); got != 1 {
					t.Fatalf("%d start lines name task %d, want one: the replayed start is the turn the answer admitted", got, number)
				}
				release()
				if err := <-compacted; err != nil && runtimeRefusalReason(err) != "" {
					t.Fatalf("compact: %v", err)
				}
				h.m.Wait()
				h.mustQuiet(StreamTurnEnd{Claim: h.token})
				h.reportOf(h.worker.sequence+1, number)
				h.wantRunOnce(number)
			})
			t.Run("an answer of no turn", func(t *testing.T) {
				h := boundsHarness(t)
				number := h.takeOf("implement the plan")
				release, compacted := cut.cut(t, h)

				h.answerAsk(false)
				h.wantAwaitingItsTurn(number, "after the answer")
				release()
				if err := <-compacted; err != nil && runtimeRefusalReason(err) != "" {
					t.Fatalf("compact: %v", err)
				}
				h.m.Wait()
				h.wantAwaitingItsTurn(number, "after the compaction's end")
				h.must(StreamTurnStart{Claim: h.token})
				h.mustQuiet(StreamTurnEnd{Claim: h.token})
				h.reportOf(h.worker.sequence+1, number)
				h.wantRunOnce(number)
			})
		})
	}
	t.Run("an end never seen begun, then a let-go", func(t *testing.T) {
		h := boundsHarness(t)
		h.takeRunsItsOwnTurn("the first task", 1, false)
		number := h.takeOf("implement the plan")
		release, compacted := compactHeld(t, h)
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		release()
		if err := <-compacted; err != nil {
			t.Fatalf("compact: %v", err)
		}
		h.m.Wait()
		h.wantAwaitingItsTurn(number, "after the compaction's end")

		h.observe(runtime.Gone)

		h.wantState(StateLaunching)
		if ended := h.claim().EndedWork; !ended.TaskTurn {
			t.Fatalf("the let-go recorded %+v, want the task's turn recorded as cut off", ended)
		}
		if p := h.pending(); p.Number != number || p.taken() || !p.Interrupted {
			t.Fatalf("the let-go left %+v, want task %d taken back with the note of a cut turn", p, number)
		}
		h.wantTakenOnceMore(number, relaunchedAndTaken)
	})
}

// A turn Oh My Pi starts as the compaction ends — a follow-up an Envoy delivery queued while the
// session was being compacted — leaves the claim working for that turn. Idling it there would
// offer the owed task while a turn runs, and that turn's own end would then arrive at an idle
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

// A compaction the store would not take puts back the end it dated as well as the state: its take
// closes a turn the claim's times still show open (compactStart), and a write that fails leaves
// the claim exactly as it found it, in memory and in the store. A claim left with the end dated
// and no compaction running would read as in no turn, and the controller keeper would never end
// the turn that is still open (dispatch://INFRA-2045 R2).
func TestACompactionTheStoreWouldNotTakeLeavesTheCutOffTurnOpen(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(StreamTurnStart{Claim: h.token})
	// A displacement cuts the turn off without dating its end, and the relaunched agent stands
	// ready with that turn still open.
	h.observeDisplaced(runtime.Gone)
	h.relaunched()
	before := h.claim()
	if !before.LastTurnStart.After(before.LastTurnEnd) {
		t.Fatalf("the relaunched claim's turn started %s and the last ended %s, want the cut-off turn still open", before.LastTurnStart, before.LastTurnEnd)
	}
	h.advance(time.Minute)
	h.store.fail("PutClaim", errors.New("the store is down"))

	if err := h.m.Compact(h.ctx, testCompactInstructions); !errors.Is(err, ErrCompactNotTaken) {
		t.Fatalf("a compaction the store would not take answered %v, want ErrCompactNotTaken", err)
	}

	h.store.fail("PutClaim", nil)
	for where, got := range map[string]Claim{"the machine": h.claim(), "the store": h.store.load(h.token)} {
		if got.State != before.State || !got.LastTurnEnd.Equal(before.LastTurnEnd) {
			t.Errorf("%s reads the claim %s with its last turn ended %s, want %s and %s as the compaction found them",
				where, got.State, got.LastTurnEnd, before.State, before.LastTurnEnd)
		}
	}
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
// claim owes is not offered. Answered as a compaction that did not run, it would tell its caller
// the claim was back and its task offered again (api's compact).
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
// every verdict is judged (Claim.ProcessGone, reportedProcess): state, the compaction in
// flight, the suspension held and the wake waiting all read as a claim genuinely in a turn
// reads, so this record is the whole of what the machine's own side has to tell the two apart,
// and a suspension that reaches such a claim lands where it arrives on it (holdSuspension).
// It is memory, so it is no answer for a recovery door: a restart forgets it, and both doors
// read the process itself at the moment of the request instead (ReadProcess, api's
// refuseWorkingClaim). What ends this window is a launch, raised by the next report of that
// process or by the fresh session that stops the claim and starts it again.
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
// waits is the owed task, which goes at the turn of the launch parked over the process a take
// would reach (claimHold's launchWaits), not into a process that turn is about to replace.
//
// Read as a lost process — which a parked launch alone says nothing about — the caller would be
// told its compaction failed for a compaction that ran, the claim would be left working with
// its run of provider errors still at providerErrorRunTold, and the notice, which goes only as the
// run reaches that count, could never go a second time.
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
// a compaction either: Oh My Pi answers the `handoff` request, and that answer goes to the
// connection of the daemon that asked (shimwire.Handoff). So the restored claim is the shape
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
	if got := h.pending(); got.ID != owed || got.taken() {
		t.Fatalf("the claim holds %+v, want the task %s still owed and untaken", got, owed)
	}

	// Oh My Pi is done and the turn the agent reported ends.
	h.mustQuiet(StreamTurnEnd{Claim: h.token})

	h.wantState(StateIdle)
	wantOwedSentNext(t, h, owed)
}

// wantOwedSentNext is the check that the next take after the restart's ladder carries owed, the
// task the restored claim owes.
func wantOwedSentNext(t *testing.T, h *harness, owed string) {
	t.Helper()
	prompts := h.wantPrompts(ladderSends + 1)
	if prompts[ladderSends].DeliveryID != owed {
		t.Fatalf("the take after the restart carried %q, want the task the claim owed, %q", prompts[ladderSends].DeliveryID, owed)
	}
}

// The same restart, and the agent answers that no turn is running — the answer the question gets
// while Oh My Pi is still writing the handoff document, since a handoff is not a turn of the
// agent and the daemon has no second word to ask about one. The restarted daemon ends the turn it
// was asking about and the owed task goes, into a session that may still be handing off.
//
// That is the recovery compactStart states and the only one a restart can make: no store and no
// frame tells the new daemon a compaction is in flight (restartedMidCompaction), so the agent's
// own answer is the whole of what it has to go on, and a daemon that distrusted that answer
// would hold the claim working with nothing left to end it — the stuck worker this issue is
// about. What the recovery must not do is lose the task, and it does not: the task is taken
// under the id and the number it was owed as, and its turn is the take's whenever it starts.
func TestARestartWhileASessionIsBeingCompactedSendsTheOwedTaskWhenItsAgentSaysNoTurnIsRunning(t *testing.T) {
	h := restartedMidCompaction(t)
	owed := h.pending().ID
	h.conn.SetStreaming(false)

	h.connect()

	h.wantState(StateIdle)
	wantOwedSentNext(t, h, owed)
	if got := h.pending(); got.ID != owed || !got.awaitsStart() {
		t.Fatalf("the claim holds %+v, want the task %s taken and its turn still to come", got, owed)
	}
}

// The same restart, and the agent does not answer the question: its get_state fails, so the
// restored claim stays working with the question armed. Then a turn starts — an Envoy follow-up —
// and ends. The turn is no task's: no take of the owed task was accepted, so its start gives it
// nothing (startOfTurn). The task stays pending and untaken through the turn, and the turn's end
// offers it, and the worker takes it.
func TestARestartWhileASessionIsBeingCompactedKeepsTheOwedTaskThroughATurnThatRanNoTake(t *testing.T) {
	h := restartedMidCompaction(t)
	owed := h.pending().ID
	h.conn.FailGetState(errors.New("the shim did not answer get_state in time"))

	h.connect()
	h.wantState(StateWorking)
	h.wantPrompts(ladderSends)

	h.must(StreamTurnStart{Claim: h.token})
	if got := h.claim().Pending; got == nil || got.ID != owed || got.taken() {
		t.Fatalf("after a start of no take the claim holds %+v, want the task %s still owed and untaken", got, owed)
	}
	h.mustQuiet(StreamTurnEnd{Claim: h.token})

	if got := h.claim().Pending; got == nil || got.ID != owed {
		t.Fatalf("after that turn's end the claim holds %+v, want the task %s still owed", got, owed)
	}
	wantOwedSentNext(t, h, owed)
}

// The same restart with the question unanswered, and the worker takes the owed task: its take is
// its word that its host is idle, so the turn the store recorded is over, and the take is
// accepted as a ready claim's is (takeInTurn). The next turn's start is that take's turn, as the
// first start after an accepted take is in a ready claim (turnStarted, startTakesTurn).
func TestARestartWhileASessionIsBeingCompactedGivesTheOwedTaskTheTurnAfterItsTake(t *testing.T) {
	h := restartedMidCompaction(t)
	owed := h.pending().ID
	h.conn.FailGetState(errors.New("the shim did not answer get_state in time"))

	h.connect()
	h.wantState(StateWorking)

	h.mustTake()
	if got := h.claim().Pending; got == nil || got.ID != owed || !got.awaitsStart() {
		t.Fatalf("after the worker's take the claim holds %+v, want the task %s taken and its turn still to come", got, owed)
	}
	h.must(StreamTurnStart{Claim: h.token})
	if got := h.claim().Pending; got == nil || got.ID != owed || !got.startRecorded() {
		t.Fatalf("after the start the claim holds %+v, want the task %s in its take's turn", got, owed)
	}
	wantOwedSentNext(t, h, owed)
}
