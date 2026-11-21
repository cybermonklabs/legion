package supervise

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// report is one turn's outcome as its producer reports it, at the claim's current generation.
func (h *harness) report(sequence uint64, refused bool, message string) {
	h.t.Helper()
	h.must(RequestTurnOutcome{
		Claim: h.token, Generation: h.generation(), Sequence: sequence, Refused: refused, Message: message,
	})
}

func (h *harness) wantRefusedRun(want int) {
	h.t.Helper()
	if got := h.claim().RefusedRun(); got != want {
		h.t.Fatalf("refused run = %d, want %d; the claim keeps %+v", got, want, h.claim().TurnOutcomes)
	}
}

// The run is derived from the reports in (generation, sequence) order, never counted as they
// arrive, so the order they arrive in cannot change it: in order, reversed, and one arriving late
// after the turns that followed it had already reported, all leave the same run.
func TestTheRefusedRunIsTheSameWhateverOrderTheReportsArriveIn(t *testing.T) {
	for name, arrivals := range map[string][]uint64{
		"in order":                  {1, 2},
		"reversed":                  {2, 1},
		"the first arriving last":   {2, 1},
		"a repeat of the same turn": {1, 2, 2},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateIdle)

			for _, sequence := range arrivals {
				h.report(sequence, true, "the provider refused this turn")
			}

			h.wantRefusedRun(2)
			h.wantState(StateIdle)
			if got := h.claim().LastRefusal(); got != "the provider refused this turn" {
				t.Errorf("last refusal %q, want the provider's words", got)
			}
		})
	}
}

// A report that arrives after the next turn has already reported lands in its own place: the run
// counts the turns, not the calls, so a late refusal of turn 2 beside refusals of 1 and 3 is the
// third refusal in a row and fails the claim, exactly as the three in order would.
func TestALateReportLandsInItsOwnPlaceAndFailsTheClaimAsAnInOrderOneWould(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	h.report(1, true, "first")
	h.report(3, true, "third")
	h.wantRefusedRun(2)
	h.wantState(StateIdle)

	h.report(2, true, "second, reported late")

	h.wantState(StateFailed)
	if want := "the provider refused 3 turns in a row: third"; h.claim().Failure != want {
		t.Fatalf("failure %q, want %q: the newest refused report names the message, whichever arrived last", h.claim().Failure, want)
	}
}

// Three refused turns in a row fail the claim, with the cause naming the provider's refusal and
// what it said about the newest of them. The process is let go, as every failure lets it go.
func TestARunOfRefusedTurnsAtTheLimitFailsTheClaimNamingTheProvidersRefusal(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	limit := testLimits().LaunchFailures

	for turn := 1; turn < limit; turn++ {
		h.report(uint64(turn), true, "stopReason: error")
		h.wantState(StateIdle)
	}
	h.report(uint64(limit), true, "the provider ended the turn with an error at 226k tokens of context")

	h.wantState(StateFailed)
	want := "the provider refused 3 turns in a row: the provider ended the turn with an error at 226k tokens of context"
	if h.claim().Failure != want {
		t.Fatalf("failure %q, want %q", h.claim().Failure, want)
	}
	if stored := h.store.load(testToken); stored.State != StateFailed || stored.Failure != want {
		t.Errorf("stored claim = %s with failure %q, want the failure persisted", stored.State, stored.Failure)
	}
	if h.claim().Locator != nil {
		t.Errorf("the failed claim still records %+v, want its process let go", h.claim().Locator)
	}
}

// A refusal with no message still fails the claim at the limit, and says so rather than naming an
// empty sentence.
func TestARunOfSilentRefusalsFailsTheClaimSayingTheProviderSaidNothing(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)

	for turn := 1; turn <= testLimits().LaunchFailures; turn++ {
		h.report(uint64(turn), true, "")
	}

	h.wantState(StateFailed)
	if want := "the provider refused 3 turns in a row, and said nothing about why"; h.claim().Failure != want {
		t.Fatalf("failure %q, want %q", h.claim().Failure, want)
	}
}

// A turn the provider took ends the run: the count is of refusals in a row, so an agent that is
// refused, works, and is refused again is making progress and is never failed for it.
func TestACleanTurnBetweenRefusalsStartsTheRunAgain(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)

	h.report(1, true, "refused")
	h.report(2, true, "refused")
	h.wantRefusedRun(2)

	h.report(3, false, "")

	h.wantRefusedRun(0)
	h.report(4, true, "refused again")
	h.report(5, true, "refused again")
	h.wantState(StateIdle)
	h.wantRefusedRun(2)
}

// Both producers report a turn a provider failed as refused, with the failure beside it
// (pi-legion's turn-outcome, the Claude bridge's turnEnded). Such a turn is the provider-error
// ladder's, which has no attempt limit (dispatch://INFRA-956 R2): a run of them longer than the
// launch failure limit never fails the claim, the run of refused turns that limit reads stays at
// zero, and every one of them takes the task back and writes it again after the ladder's wait,
// until the run reaches the turn the project's controller is told at (providerErrorRunTold). A
// refused turn that carries no provider error is still the launch budget's: the same claim,
// refused that way up to the limit, fails.
func TestTurnOutcomeAProviderErrorTurnIsNoLaunchFailure(t *testing.T) {
	h := workingClaim(t)
	limit := testLimits().LaunchFailures
	if providerErrorRunTold <= limit {
		t.Fatalf("the controller is told at a run of %d, which the launch failure limit of %d reaches first: this case proves nothing",
			providerErrorRunTold, limit)
	}
	sent := 1
	for turn := 1; turn <= providerErrorRunTold; turn++ {
		pe := ProviderError{Message: "overloaded_error", Status: 529}
		number := h.pending().Number
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.worker.sequence++
		if err := h.m.Handle(h.ctx, RequestTurnOutcome{Claim: h.token, Generation: h.generation(), Sequence: h.worker.sequence,
			Task: number, Refused: true, Message: pe.Message, ProviderError: &pe}); err != nil {
			t.Fatalf("turn %d: report the turn refused with its provider error: %v", turn, err)
		}
		h.m.Wait()
		h.workerAsks()
		if state := h.state(); state == StateFailed {
			t.Fatalf("turn %d: the claim failed with %q, want it on the ladder: a provider error is no launch failure", turn, h.claim().Failure)
		}
		h.wantRefusedRun(0)
		if got := h.claim().ProviderErrorRun; got != turn {
			t.Fatalf("turn %d: the run of provider errors is %d, want %d", turn, got, turn)
		}
		if message := h.climbs(sent, promptRetryDelay(turn)); !strings.HasSuffix(message, providerErrorTaskText) {
			t.Fatalf("turn %d: the ladder sent %q, want the task again", turn, message)
		}
		sent++
	}

	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	for turn := 1; turn <= limit; turn++ {
		h.report(h.worker.sequence+uint64(turn), true, "the provider ended the turn with an error")
		if turn < limit {
			h.wantRefusedRun(turn)
		}
	}
	h.wantState(StateFailed)
	if want := "the provider refused 3 turns in a row: the provider ended the turn with an error"; h.claim().Failure != want {
		t.Fatalf("failure %q, want %q: a refused turn with no provider error is still the launch budget's", h.claim().Failure, want)
	}
}

// A refused turn that carries a provider error and reaches no provider-error ladder — its report
// names no task, or a number no take of the claim holds, as a reminder's, a card's or a background
// result's turn does — is counted by nothing else, so it counts toward the refused run, and the
// launch failure limit fails the claim out of it (dispatch://INFRA-714). Both producers name a
// provider error beside every refused turn, so this is the only way such a run is counted at all;
// a provider error on the turn of the task the claim's take holds stays the ladder's
// (TestTurnOutcomeAProviderErrorTurnIsNoLaunchFailure).
//
// The negative control is every provider error left uncounted (turnOutcome's refused before this
// change): the run stays at zero and the claim never fails.
func TestTurnOutcomeAProviderErrorRefusalThatReachesNoLadderCountsTowardTheRefusedRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		task func(h *harness) uint64
	}{
		{"naming no task", func(*harness) uint64 { return 0 }},
		{"naming a task no take holds", func(h *harness) uint64 {
			finished := h.claim().TaskFinished
			if finished == 0 {
				h.t.Fatal("the claim has finished no task, so this row would name none, as the row before it does")
			}
			return finished
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateIdle)
			limit := testLimits().LaunchFailures
			pe := ProviderError{Message: "overloaded_error", Status: 529}

			for turn := 1; turn <= limit; turn++ {
				h.worker.sequence++
				if err := h.m.Handle(h.ctx, RequestTurnOutcome{Claim: h.token, Generation: h.generation(), Sequence: h.worker.sequence,
					Task: tc.task(h), Refused: true, Message: "a card's turn the provider refused", ProviderError: &pe}); err != nil {
					t.Fatalf("turn %d: report the refused turn: %v", turn, err)
				}
				h.m.Wait()
				if turn < limit {
					h.wantRefusedRun(turn)
					h.wantState(StateIdle)
				}
			}

			h.wantState(StateFailed)
			if want := "the provider refused 3 turns in a row: a card's turn the provider refused"; h.claim().Failure != want {
				t.Fatalf("failure %q, want %q", h.claim().Failure, want)
			}
		})
	}
}

// A turn no report arrived for — the process died before it could send one — counts neither way:
// the run is of the reports the daemon holds, so the turns around a missing one are a run of two,
// not three, and the claim goes on.
func TestATurnWithNoReportCountsNothing(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)

	h.report(1, true, "refused")
	// Turn 2 reported nothing.
	h.report(3, true, "refused")

	h.wantState(StateIdle)
	h.wantRefusedRun(2)
}

// The run carries across generations, in generation order: a session the provider refused is
// refused again when the daemon relaunches and resumes it, so the refusals of the relaunched
// generation go on counting from the ones before it. The relaunch here registers the same session
// the claim recorded, which is what a resume does.
func TestTheRefusedRunCarriesAcrossAGenerationThatResumesTheSameSession(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	h.report(1, true, "refused")
	h.report(2, true, "refused")

	h.observe(runtime.Gone)
	h.relaunched()
	if h.generation() != 2 {
		t.Fatalf("generation %d, want the relaunch's 2", h.generation())
	}
	h.wantRefusedRun(2)

	h.report(1, true, "refused in the resumed session too")

	h.wantState(StateFailed)
	if !strings.Contains(h.claim().Failure, "refused in the resumed session too") {
		t.Fatalf("failure %q, want the resumed generation's own refusal named", h.claim().Failure)
	}
}

// The retry of a claim the provider refused out of its budget starts a new session rather than
// resuming the recorded one: the session every turn of which was refused would be refused again.
// The claim spawns instead of resuming, and the new session starts with a fresh run.
func TestTheRetryOfAClaimItsProviderRefusedStartsANewSession(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	for turn := 1; turn <= testLimits().LaunchFailures; turn++ {
		h.report(uint64(turn), true, "the provider refused the turn")
	}
	h.wantState(StateFailed)
	spawns := len(h.calls("Spawn"))

	h.must(RequestRetry{Claim: h.token})

	h.wantState(StateLaunching)
	h.wantCalls("Spawn", spawns+1)
	h.wantCalls("Resume", 0)
	if c := h.claim(); c.Session != "" || c.SessionFile != "" {
		t.Errorf("the retried claim still records session %q (%s), want it started afresh", c.Session, c.SessionFile)
	}
	h.wantRefusedRun(0)
	if kept := h.claim().TurnOutcomes; len(kept) != 0 {
		t.Errorf("the retried claim keeps %+v, want the refused session's turns gone with it", kept)
	}
}

// A retry that asks for a new session starts one whatever failed the claim, and a retry that asks
// for nothing resumes the recorded session as every retry did before: the operator's
// `legion claims retry --fresh-session` is the ask, and the architect's and the workflow's retries
// make none. The claim here failed on deaths rather than refusals, so the ask is the only thing
// deciding it.
func TestARetryStartsANewSessionWhenItAsksAndResumesTheRecordedOneWhenItDoesNot(t *testing.T) {
	for _, tc := range []struct {
		name  string
		claim Claim
		retry Event
	}{
		{"the workflow's retry asking for a new session", queuedClaim(), RequestRetry{Claim: testToken, FreshSession: true}},
		{"the operator's retry asking for a new session", rootClaim(), RequestOperatorRetry{Claim: rootToken, FreshSession: true}},
		{"the workflow's retry asking for nothing", queuedClaim(), RequestRetry{Claim: testToken}},
		{"the operator's retry asking for nothing", rootClaim(), RequestOperatorRetry{Claim: rootToken}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarnessOf(t, tc.claim)
			h.reach(StateReady)
			h.report(1, true, "refused once, which is not the budget")
			for range testLimits().LaunchFailures {
				h.observe(runtime.Gone)
			}
			h.wantState(StateFailed)
			resumes, spawns := len(h.calls("Resume")), len(h.calls("Spawn"))

			h.must(tc.retry)

			h.wantState(StateLaunching)
			asked, _ := freshSession(tc.retry, Claim{}, 1)
			if asked {
				h.wantCalls("Spawn", spawns+1)
				h.wantCalls("Resume", resumes)
				if c := h.claim(); c.Session != "" || c.SessionFile != "" {
					t.Errorf("the claim still records session %q (%s), want a new one started", c.Session, c.SessionFile)
				}
				h.wantRefusedRun(0)
				return
			}
			h.wantCalls("Resume", resumes+1)
			h.wantCalls("Spawn", spawns)
			if c := h.claim(); c.Session != session {
				t.Errorf("the claim records session %q, want the recorded %q resumed", c.Session, session)
			}
			h.wantRefusedRun(1)
		})
	}
}

// The window keeps only what the derivation can read: the newest launch_failure_limit reports. A
// claim whose agent works for a long time carries that handful of rows and no more, and the run it
// derives is unchanged by what fell out of it.
func TestTheWindowKeepsOnlyTheNewestReportsTheRunCanBeDerivedFrom(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	limit := testLimits().LaunchFailures

	for turn := 1; turn <= 4*limit; turn++ {
		h.report(uint64(turn), false, "")
	}

	kept := h.claim().TurnOutcomes
	if len(kept) != limit {
		t.Fatalf("the window keeps %d reports, want the launch failure limit of %d", len(kept), limit)
	}
	if kept[0].Sequence != uint64(4*limit-limit+1) || kept[len(kept)-1].Sequence != uint64(4*limit) {
		t.Fatalf("the window keeps turns %d..%d, want the newest %d", kept[0].Sequence, kept[len(kept)-1].Sequence, limit)
	}
	h.wantRefusedRun(0)
}

// A report older than everything a full window keeps is dropped: it cannot shorten a run derived
// over the newest reports, so there is nothing for it to say and no room to say it in.
func TestAReportOlderThanAFullWindowIsDropped(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	limit := testLimits().LaunchFailures
	h.report(11, false, "")
	for turn := 2; turn <= limit; turn++ {
		h.report(uint64(10+turn), true, "refused")
	}
	h.wantRefusedRun(limit - 1)
	full := h.claim().TurnOutcomes
	if len(full) != limit {
		t.Fatalf("the window keeps %+v, want it full at %d", full, limit)
	}

	h.report(1, false, "a turn from long before these")

	h.wantState(StateIdle)
	h.wantRefusedRun(limit - 1)
	if kept := h.claim().TurnOutcomes; len(kept) != limit || kept[0].Sequence != 11 {
		t.Fatalf("the window keeps %+v, want the same %d newest reports", kept, limit)
	}
}

// The reports survive a daemon restart: the run is derived from what the store holds, so a daemon
// that restarts between the second refusal and the third fails the claim on the third, as the
// daemon that saw all three would.
func TestTheReportsSurviveARestartAndTheRunGoesOnFromThem(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	h.report(1, true, "refused")
	h.report(2, true, "refused")

	h.restart()

	h.wantRefusedRun(2)
	h.report(3, true, "refused after the restart")
	h.wantState(StateFailed)
}

// repeatReport sends the turn-outcome report r again, as the worker's plugin sends a report whose
// first attempt had no answer, and fails the test on an error.
func (h *harness) repeatReport(r RequestTurnOutcome) {
	h.t.Helper()
	if err := h.m.Handle(h.ctx, r); err != nil {
		h.t.Fatalf("send the report of task %d again: %v", r.Task, err)
	}
	h.m.Wait()
}

// A report the worker's plugin sends again, because a daemon restart cut off the answer to its
// first attempt, is applied once. The first attempt took the task back after a provider error.
// The restart dropped the ladder's wait, so the restarted daemon gives the task again at the
// shim's hello, and the worker takes it. The repeat must not take that take back: the run of
// provider errors and the ladder stay at one, and the take's own turn finishes the task.
func TestTurnOutcomeRepeatedAfterARestartIsAppliedOnce(t *testing.T) {
	h := workingClaim(t)
	number := h.pending().Number
	pe := ProviderError{Message: "overloaded_error", Status: 529}
	h.errorTurn(pe)
	first := RequestTurnOutcome{Claim: h.token, Generation: h.generation(), Sequence: h.worker.sequence,
		Task: number, ProviderError: &pe}

	h.restart()
	h.connect()
	h.wantPrompts(2)
	if p := h.pending(); !p.awaitsTurn() || p.Number != number {
		t.Fatalf("after the restart the task is %+v, want task %d taken again and its turn not started", p, number)
	}

	h.repeatReport(first)
	if p := h.pending(); !p.awaitsTurn() || p.ProviderErrorRetries != 1 || h.claim().ProviderErrorRun != 1 {
		t.Fatalf("after the repeated report the task is %+v with a run of %d provider errors, want the take standing, one retry and a run of one",
			p, h.claim().ProviderErrorRun)
	}
	if lines := h.logs.lines("a turn's report repeats one already applied", "task="); len(lines) != 1 {
		t.Errorf("the lines that say the report was a repeat are %q, want one", lines)
	}

	h.must(StreamTurnStart{Claim: h.token})
	h.must(StreamTurnEnd{Claim: h.token})
	if p := h.claim().Pending; p != nil {
		t.Fatalf("pending %+v after the take's own turn was reported, want task %d finished", p, number)
	}
	if run := h.claim().ProviderErrorRun; run != 0 {
		t.Fatalf("the run of provider errors is %d after a turn with none, want 0", run)
	}
}

// A report the worker's plugin sends again after newer reports pushed it out of the window is a
// repeat all the same (the window's watermark, recordTurnOutcome). Report k took its task back
// for a provider error; reports k+1 to k+3, of turns no take started, leave the window holding
// them alone; the ladder's wait gives the task again and the worker takes it. Report k sent again
// then changes nothing: its provider error is not counted again, the take stands, and the window
// is as it was. So in the daemon that applied k, and in one restarted over what the store holds.
//
// The negative control is a repeat found by the window's entries alone (recordTurnOutcome with no
// watermark): k is no repeat once it has left the window, and its provider error takes the new
// take back and counts a second step of the ladder.
func TestTurnOutcomeSentAgainAfterItLeftTheWindowIsARepeat(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "in the same daemon", true: "after a restart"}[restart], func(t *testing.T) {
			h := workingClaim(t)
			number := h.pending().Number
			pe := ProviderError{Message: "overloaded_error", Status: 529}
			h.errorTurn(pe)
			first := RequestTurnOutcome{Claim: h.token, Generation: h.generation(), Sequence: h.worker.sequence,
				Task: number, ProviderError: &pe}
			for range testLimits().LaunchFailures {
				h.must(StreamTurnStart{Claim: h.token})
				h.mustQuiet(StreamTurnEnd{Claim: h.token})
				if err := h.reportTurn(0, nil); err != nil {
					t.Fatalf("report a turn no take started: %v", err)
				}
			}
			if kept := h.claim().TurnOutcomes; slices.ContainsFunc(kept, func(o TurnOutcome) bool { return o.Sequence == first.Sequence }) {
				t.Fatalf("the window keeps %+v, want report %d out of it", kept, first.Sequence)
			}
			h.advance(promptRetryDelay(1))
			if p := h.pending(); !p.awaitsTurn() || p.Number != number {
				t.Fatalf("after the ladder's wait the task is %+v, want task %d taken again and its turn not started", p, number)
			}
			if restart {
				h.restart()
				h.connect()
			}
			window := h.claim().TurnOutcomes

			h.repeatReport(first)

			if p := h.pending(); !p.awaitsTurn() || p.ProviderErrorRetries != 1 || h.claim().ProviderErrorRun != 1 {
				t.Fatalf("after report %d was sent again the task is %+v with a run of %d provider errors, want the take standing, one retry and a run of one",
					first.Sequence, p, h.claim().ProviderErrorRun)
			}
			if kept := h.claim().TurnOutcomes; !slices.Equal(kept, window) {
				t.Fatalf("after report %d was sent again the window keeps %+v, want it as it was, %+v", first.Sequence, kept, window)
			}
			if lines := h.logs.lines("a turn's report repeats one already applied", "task="); len(lines) != 1 {
				t.Errorf("the lines that say the report was a repeat are %q, want one", lines)
			}
		})
	}
}

// A report never applied — its first attempt lost on the way, so no daemon saw it — that the
// worker's plugin sends again only after newer reports of the same generation pushed the window
// past its key is applied all the same: the window's watermark covers only keys below which every
// report of the generation was applied (recordTurnOutcome), and the window keeps each applied
// report above it. The take's turn k ends with its report lost; reports k+1 to k+4, of turns no
// take started, land; report k, naming the take's number, then finishes the task once, in the
// daemon that saw the others and in one restarted over what the store holds. The window is back
// at its bound once the gap is filled.
//
// The negative control is the watermark raised to every key the bound drops (recordTurnOutcome
// before this change): report k reads as a repeat of a report already applied, the task stays
// unfinished, and a relaunch would give it again to a worker that ran it whole.
func TestTurnOutcomeSentAgainAfterNewerOnesFilledTheWindowIsAppliedWhenItWasNever(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "in the same daemon", true: "after a restart"}[restart], func(t *testing.T) {
			h := newHarnessOf(t, queuedClaim(), currentPlugin)
			h.reach(StateReady)
			h.takesTurnEndsWithItsReportToCome()
			number := h.pending().Number
			h.worker.sequence++
			lost := RequestTurnOutcome{Claim: h.token, Generation: h.generation(), Sequence: h.worker.sequence, Task: number}
			for range testLimits().LaunchFailures + 1 {
				h.must(StreamTurnStart{Claim: h.token})
				h.mustQuiet(StreamTurnEnd{Claim: h.token})
				if err := h.reportTurn(0, nil); err != nil {
					t.Fatalf("report a turn no take started: %v", err)
				}
			}
			if p := h.pending(); p.Number != number || !p.turnStarted() {
				t.Fatalf("after the newer reports the claim holds %+v, want task %d's turn still waiting for its report", p, number)
			}
			if restart {
				h.restart()
				h.connect()
			}

			h.repeatReport(lost)

			if p := h.claim().Pending; p != nil {
				t.Fatalf("pending %+v after report %d came, want task %d finished", p, lost.Sequence, number)
			}
			h.wantFinishedOnce(finishedByReport)
			if kept := h.claim().TurnOutcomes; len(kept) != testLimits().LaunchFailures {
				t.Errorf("the window keeps %+v once the gap is filled, want %d reports", kept, testLimits().LaunchFailures)
			}
		})
	}
}

// A report whose first attempt the store did not write is applied in full when the worker's
// plugin sends it again: the failed write ended the decision before it reached the task, so the
// report sent again is its first application, not a repeat. That holds in the daemon whose write
// failed and in a daemon restarted after the write owed was made, which reads the mark back from
// the window.
func TestTurnOutcomeRepeatedAfterItsWriteFailedIsApplied(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "in the same daemon", true: "after a restart"}[restart], func(t *testing.T) {
			h := workingClaim(t)
			number := h.pending().Number
			h.mustQuiet(StreamTurnEnd{Claim: h.token})
			h.worker.sequence++
			report := RequestTurnOutcome{Claim: h.token, Generation: h.generation(), Sequence: h.worker.sequence,
				Task: number, ProviderError: &ProviderError{Message: "overloaded_error", Status: 529}}

			h.store.fail("PutClaim", errBoom)
			if err := h.m.Handle(h.ctx, report); !errors.Is(err, errBoom) {
				t.Fatalf("the report whose write failed = %v, want %v", err, errBoom)
			}
			h.m.Wait()
			h.store.fail("PutClaim", nil)
			if p := h.pending(); !p.turnStarted() || h.claim().ProviderErrorRun != 0 {
				t.Fatalf("after the failed write the task is %+v with a run of %d, want it still taken and no provider error counted",
					p, h.claim().ProviderErrorRun)
			}
			if restart {
				h.seen()
				if kept := h.store.load(h.token).TurnOutcomes; len(kept) != 1 || !kept[0].Unapplied {
					t.Fatalf("the store keeps %+v once the owed write was made, want the report marked unapplied", kept)
				}
				h.restart()
			}

			h.repeatReport(report)
			if p := h.pending(); p.taken() || p.ProviderErrorRetries != 1 || h.claim().ProviderErrorRun != 1 {
				t.Fatalf("after the report was sent again the task is %+v with a run of %d, want it taken back once and a run of one",
					p, h.claim().ProviderErrorRun)
			}
			if kept := h.store.load(h.token).TurnOutcomes; len(kept) != 1 || kept[0].Unapplied {
				t.Fatalf("the store keeps %+v, want the report applied", kept)
			}
		})
	}
}
