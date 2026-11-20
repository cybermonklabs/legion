package supervise

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
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

// reportRefused is one refused turn as its producer reports it, with the HTTP status the provider
// answered it with (zero for a refusal that names none), at the claim's current generation.
func (h *harness) reportRefused(sequence uint64, message string, status int) {
	h.t.Helper()
	h.must(RequestTurnOutcome{
		Claim: h.token, Generation: h.generation(), Sequence: sequence, Refused: true, Message: message, HTTPStatus: status,
	})
}

// ownTurnRefused is a turn no daemon prompt ran — a phase reminder's, an Envoy message's, the
// agent's own continuation — that the provider refused: the agent starts it, its producer reports
// it refused, and it ends. No prompt result follows such a turn (stream's Conn drops a result that
// names no delivery, and the Claude bridge writes one only for the daemon's prompt), so the report
// is all the daemon learns of it.
func (h *harness) ownTurnRefused(sequence uint64, message string, status int) {
	h.t.Helper()
	h.must(StreamTurnStart{Claim: h.token})
	h.reportRefused(sequence, message, status)
	if h.state() != StateFailed {
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
	}
}

// idleClaimTold is an idle claim whose machine tells the project's controller of a run of
// provider-error turns into notices, each the reason the controller is given.
func idleClaimTold(t *testing.T, notices *[]string) *harness {
	t.Helper()
	h := newBareHarness(t)
	h.deps.ProviderErrorRunReached = func(_ Claim, reason string) { *notices = append(*notices, reason) }
	c := queuedClaim()
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	h.reach(StateIdle)
	return h
}

// A turn no daemon prompt ran gets no prompt result, so its report decides it. A refusal whose
// status cannot clear fails the claim at once from that report, by the rule the same status fails
// it by from a prompt result, and the workflow then holds the issue for its architect
// (internal/daemon's TestARefusal400HoldsTheIssueForItsArchitect). Before the report carried the
// status, such a claim was failed by nothing, and the phase reminder sent it a refused turn every
// five minutes with no end.
func TestATurnNoPromptRanThatIsRefusedWithAnErrorThatCannotClearFailsTheClaimAtOnce(t *testing.T) {
	for _, status := range []int{400, 404, 409, 413, 422} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var notices []string
			h := idleClaimTold(t, &notices)

			h.ownTurnRefused(1, "API Error: "+strconv.Itoa(status)+" invalid_request_error: prompt is too long", status)

			h.wantState(StateFailed)
			c := h.claim()
			if c.FailureCause != CauseProviderError {
				t.Errorf("the failure cause is %q, want %q", c.FailureCause, CauseProviderError)
			}
			if want := fmt.Sprintf("a provider error that cannot clear ended this claim's turn: a provider error (HTTP %d)", status); c.Failure != want {
				t.Errorf("the failure is %q, want %q", c.Failure, want)
			}
			if stored := h.store.load(testToken); stored.State != StateFailed || stored.FailureCause != CauseProviderError {
				t.Errorf("the stored claim is %s (%q), want it failed with %q", stored.State, stored.FailureCause, CauseProviderError)
			}
			if len(notices) != 0 {
				t.Errorf("the controller was told %v after one refused turn, want nothing", notices)
			}
		})
	}
}

// A refusal whose status can clear, and one whose report names no status, fail no claim however
// long the run, on turns no daemon prompt ran as on turns one did (R1's run with no attempt
// limit). Those turns count toward the run the project's controller is told of, once, at the
// sixth: the report's run is the one count that sees them, since no prompt result follows them.
func TestATurnNoPromptRanThatIsRefusedWithAnErrorThatCanClearFailsNothingAndIsToldAtTheSixth(t *testing.T) {
	for _, status := range []int{529, 503, 500, 429, 408, 401, 403, 0} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var notices []string
			h := idleClaimTold(t, &notices)

			for turn := 1; turn <= 25; turn++ {
				h.ownTurnRefused(uint64(turn), "the provider refused this turn", status)

				if state := h.state(); state == StateFailed {
					t.Fatalf("the claim failed after %d refused turns with status %d: %q", turn, status, h.claim().Failure)
				}
				if want := min(max(turn-5, 0), 1); len(notices) != want {
					t.Fatalf("the controller was told %d times after refused turn %d, want %d: %v", len(notices), turn, want, notices)
				}
			}

			h.wantState(StateIdle)
			h.wantRefusedRun(25)
			want := "6 turns in a row ended in " + providerErrorCause(status) + "; the claim holds no task, so the daemon has nothing to send again"
			if len(notices) != 1 || notices[0] != want {
				t.Fatalf("the controller was told %q, want one notice %q", notices, want)
			}
		})
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

// A report that arrives after the next turn has already reported lands in its own place. It records
// the provider's final refusal without failing the claim.
func TestALateReportLandsInItsOwnPlaceWithoutFailingTheClaim(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	h.report(1, true, "first")
	h.report(3, true, "third")
	h.wantRefusedRun(2)
	h.wantState(StateIdle)

	h.report(2, true, "second, reported late")

	h.wantState(StateIdle)
	h.wantRefusedRun(3)
	if got := h.claim().LastRefusal(); got != "third" {
		t.Fatalf("last refusal %q, want the newest turn's message", got)
	}
}

// A run of refused turns whose reports name no status is kept as a record, and it does not fail the
// claim or release its process, however long it is: only a status that cannot clear fails a claim.
func TestARunOfRefusedTurnsAtTheLimitKeepsTheClaimWorking(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	limit := testLimits().LaunchFailures

	for turn := 1; turn <= limit; turn++ {
		h.report(uint64(turn), true, "the provider refused this turn")
	}

	h.wantState(StateIdle)
	h.wantRefusedRun(limit)
	if stored := h.store.load(testToken); stored.State != StateIdle || stored.Failure != "" {
		t.Errorf("stored claim = %s with failure %q, want an idle claim with no failure", stored.State, stored.Failure)
	}
	if h.claim().Locator == nil {
		t.Error("the claim lost its process after refused turns, want it kept")
	}
}

// A refusal with no message is also only a record. Its lack of text does not turn it into a claim
// failure.
func TestARunOfSilentRefusalsKeepsTheClaimWorking(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)

	for turn := 1; turn <= testLimits().LaunchFailures; turn++ {
		h.report(uint64(turn), true, "")
	}

	h.wantState(StateIdle)
	h.wantRefusedRun(testLimits().LaunchFailures)
	if h.claim().Failure != "" {
		t.Fatalf("failure %q, want no failure", h.claim().Failure)
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

// The run carries across generations, in generation order. A relaunch resumes the recorded
// session, and another refusal remains only a report.
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

	h.wantState(StateReady)
	h.wantRefusedRun(3)
}

// A retry that asks for a new session starts one whatever failed the claim, and a retry that asks
// for nothing resumes the recorded session as every retry did before: the operator's
// `legion claims retry --fresh-session` is the ask, and the architect's and the workflow's retries
// make none. The claim here carries a run of refused turns as long as the launch failure limit —
// the run that chose a new session before refusals stopped failing claims, and the run a claim on
// a 529 ladder now carries as a matter of course — and failed on deaths, so the ask is the only
// thing that decides it: the run chooses nothing.
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
			limit := testLimits().LaunchFailures
			for turn := 1; turn <= limit; turn++ {
				h.report(uint64(turn), true, "the provider is overloaded")
			}
			h.wantRefusedRun(limit)
			for range testLimits().LaunchFailures {
				h.observe(runtime.Gone)
			}
			h.wantState(StateFailed)
			resumes, spawns := len(h.calls("Resume")), len(h.calls("Spawn"))

			h.must(tc.retry)

			h.wantState(StateLaunching)
			asked, _ := freshSession(tc.retry)
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
			h.wantRefusedRun(limit)
		})
	}
}

// The window keeps only the newest launch_failure_limit reports. A claim whose agent works for a
// long time carries that handful of rows and no more, and the run it derives is unchanged by what
// fell out of it.
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

// The run is a record, so it is counted whole: a run of 25 refused turns reads 25, past the
// launch_failure_limit reports the claim keeps whole, since it keeps a key for each turn of the run
// while the run goes on and the provider's words for the newest limit of them alone. It reads the
// same after a restart, which reads the reports back from the store. A clean turn ends it, the
// old run's keys go down to the bound, and the next refusal starts a run of one; a run broken
// before the clean turn is not carried into the run that follows.
func TestTheRefusedRunCountsTheWholeRunPastTheWindow(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	limit := testLimits().LaunchFailures

	for turn := 1; turn <= 25; turn++ {
		h.report(uint64(turn), true, "the provider is overloaded")
		h.wantRefusedRun(turn)
	}
	kept := h.claim().TurnOutcomes
	if len(kept) != 25 {
		t.Fatalf("the claim keeps %d reports of a run of 25, want a key for each turn: %+v", len(kept), kept)
	}
	for i, o := range kept {
		if worded := o.Message != ""; worded != (i >= 25-limit) {
			t.Fatalf("report %d of the run keeps the provider's words %q, want them on the newest %d alone: %+v", o.Sequence, o.Message, limit, kept)
		}
	}
	h.restart()
	h.wantRefusedRun(25)

	h.report(26, false, "")
	h.wantRefusedRun(0)
	if kept := h.claim().TurnOutcomes; len(kept) != limit {
		t.Fatalf("after the clean turn the claim keeps %d reports, want the launch failure limit of %d: %+v", len(kept), limit, kept)
	}
	for turn := 27; turn <= 27+limit; turn++ {
		h.report(uint64(turn), true, "the provider is overloaded again")
	}
	h.wantRefusedRun(limit + 1)
	h.wantState(StateIdle)
}

// A report older than the newest clean one the claim keeps is in no run a reader reads, so once
// the bound is reached it is dropped: there is nothing for it to say and no room to say it in.
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

// A producer retries a report until it lands (pi-legion's retryWhileDaemonRestarts), so a turn's
// report can arrive twice, the second time after more newer reports than the launch failure limit.
// The claim keeps every key of the run it counts, so the repeat finds its turn and counts nothing:
// five refused turns read five and tell the controller nothing, and the sixth distinct turn, not
// a repeat, is the one that tells it, once.
func TestARepeatOfATurnThatLeftTheBoundChangesNoCount(t *testing.T) {
	var notices []string
	h := idleClaimTold(t, &notices)
	if limit := testLimits().LaunchFailures; limit >= 5 {
		t.Fatalf("the case needs a launch failure limit under 5 so that turn 1 leaves the bound; it is %d", limit)
	}
	for turn := uint64(1); turn <= 5; turn++ {
		h.ownTurnRefused(turn, "overloaded_error", 529)
	}
	for _, repeat := range []uint64{1, 2, 5} {
		h.reportRefused(repeat, "overloaded_error", 529)
		h.wantRefusedRun(5)
	}
	if len(notices) != 0 {
		t.Fatalf("after 5 refused turns and three repeated reports the controller was told %q, want nothing before the sixth turn", notices)
	}

	h.ownTurnRefused(6, "overloaded_error", 529)
	h.reportRefused(3, "overloaded_error", 529)
	h.wantRefusedRun(6)
	if want := "6 turns in a row ended in a provider error (HTTP 529); the claim holds no task, so the daemon has nothing to send again"; len(notices) != 1 || notices[0] != want {
		t.Fatalf("after the sixth refused turn the controller was told %q, want once: %q", notices, want)
	}
}

// A report that arrives late lands in the place of its turn, however many newer reports came
// first: behind a clean turn it joins no run, inside the run a refused one joins it and a clean
// one ends it there, and a refused one older than the run's own clean start stays out of it.
func TestALateReportCountsByItsTurnWhereverTheRunHasGotTo(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	if limit := testLimits().LaunchFailures; limit > 3 {
		t.Fatalf("the case needs a launch failure limit of 3 or under so that turn 1's report comes after a bound of newer ones; it is %d", limit)
	}
	h.report(2, false, "")
	for turn := uint64(3); turn <= 5; turn++ {
		h.report(turn, true, "overloaded_error")
	}
	h.wantRefusedRun(3)
	h.report(1, true, "overloaded_error") // behind the clean turn 2
	h.wantRefusedRun(3)

	// Turns 7 to 12 refused, turn 9's report still on its way.
	h.report(6, false, "")
	for _, turn := range []uint64{7, 8, 10, 11, 12} {
		h.report(turn, true, "overloaded_error")
	}
	h.wantRefusedRun(5)
	h.report(9, true, "overloaded_error") // refused: it joins the run
	h.wantRefusedRun(6)

	// Turns 14 to 18 refused, turn 16's report still on its way; it was clean.
	h.report(13, false, "")
	for _, turn := range []uint64{14, 15, 17, 18} {
		h.report(turn, true, "overloaded_error")
	}
	h.wantRefusedRun(4)
	h.report(16, false, "") // clean: the run starts again after it
	h.wantRefusedRun(2)
	h.report(14, true, "overloaded_error") // a repeat from before the late clean turn
	h.wantRefusedRun(2)
}

// ownTurnEndsInFrontOfATask is a turn no daemon prompt ran (a phase reminder's) during which a
// task is delivered: the daemon queues the task, and the turn's end sends it at once, so that
// turn's own report, an HTTP call that races the stream, can reach the daemon after the send,
// while the task's prompt is awaited.
func (h *harness) ownTurnEndsInFrontOfATask() {
	h.t.Helper()
	h.must(StreamTurnStart{Claim: h.token})
	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan", Phase: phase.Implementing, Generation: 7, Entry: 1})
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	if !h.m.promptAwaited() {
		h.t.Fatal("the turn's end sent no task, so no prompt is awaited and the case tests nothing")
	}
}

// A report decides the turn it ends, whenever it arrives. A phase reminder's turn is refused with
// a 400 and the task delivered during it is sent at its end, before the turn's report lands: the
// report is left to the task prompt's result, since the turn it ends may be that prompt's. The
// result shows it was not — the prompt's turn ended clean, or in an error of another status — so
// the 400 is decided on its own status then, and the claim fails with the issue held. Before, the
// clean result cleared the mark and nothing ever decided the 400.
func TestARefusalReportedAfterTheNextTasksSendIsDecidedByItsOwnStatus(t *testing.T) {
	t.Run("the task's turn ends clean", func(t *testing.T) {
		var notices []string
		h := idleClaimTold(t, &notices)
		h.ownTurnEndsInFrontOfATask()
		h.reportRefused(1, "API Error: 400 invalid_request_error: prompt is too long", 400)
		h.wantState(StateIdle)

		h.must(StreamTurnStart{Claim: h.token})
		h.must(StreamTurnEnd{Claim: h.token}) // the harness posts the prompt's clean result

		c := h.claim()
		if want := "a provider error that cannot clear ended this claim's turn: a provider error (HTTP 400)"; c.State != StateFailed || c.FailureCause != CauseProviderError || c.Failure != want {
			t.Fatalf("the claim is %s with cause %q and failure %q, want failed by the reminder turn's 400: %q", c.State, c.FailureCause, c.Failure, want)
		}
	})
	t.Run("the task's turn ends in an error of another status", func(t *testing.T) {
		var notices []string
		h := idleClaimTold(t, &notices)
		h.ownTurnEndsInFrontOfATask()
		h.reportRefused(1, "API Error: 400 invalid_request_error", 400)
		h.must(StreamTurnStart{Claim: h.token})
		h.errorTurn(StreamPromptResult{Error: "overloaded_error", HTTPStatus: 529})

		c := h.claim()
		if c.State != StateFailed || c.FailureCause != CauseProviderError {
			t.Fatalf("the claim is %s with cause %q after the reminder turn's 400 and the task turn's 529, want failed by the 400", c.State, c.FailureCause)
		}
		if p := c.Pending; p == nil || p.Task != "implement the plan" {
			t.Fatalf("the failed claim holds %+v, want the task the 529 took back", p)
		}
	})
}

// The prompt's turn is the newest turn, so of two refused reports that arrive while one prompt is
// awaited the older ended an earlier turn: it is decided on its own status as the newer one
// lands, and the newer is left to the prompt's result.
func TestANewerReportWhileAPromptIsAwaitedDecidesTheOlderRefusal(t *testing.T) {
	var notices []string
	h := idleClaimTold(t, &notices)
	h.ownTurnEndsInFrontOfATask()
	h.reportRefused(1, "API Error: 400 invalid_request_error", 400)
	h.wantState(StateIdle)
	h.must(StreamTurnStart{Claim: h.token})
	h.reportRefused(2, "overloaded_error", 529)

	if c := h.claim(); c.State != StateFailed || c.FailureCause != CauseProviderError {
		t.Fatalf("the claim is %s with cause %q once the prompt turn's report landed, want failed by the older 400", c.State, c.FailureCause)
	}
}

// The reports survive a daemon restart. A later refusal remains a report and does not fail the
// claim.
func TestTheReportsSurviveARestartAndTheRunGoesOnWithoutFailingTheClaim(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	h.report(1, true, "refused")
	h.report(2, true, "refused")

	h.restart()

	h.wantRefusedRun(2)
	h.report(3, true, "refused after the restart")
	h.wantState(StateIdle)
	h.wantRefusedRun(3)
}
