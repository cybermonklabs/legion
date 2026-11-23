package supervise

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// report is one turn's outcome as its producer reports it, at the claim's current generation, for
// a turn the daemon's prompt ran: the report is not marked unprompted, so that prompt's result,
// and never the report, judges the turn (turnOutcome).
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

// refusedTurn ends the turn the claim is working as an Oh My Pi worker's refused turn ends: its
// producer reports the turn refused (turnOutcome), and the result of the prompt the turn ran names
// the status the provider answered, which alone decides what the error does (providerErrored).
func (h *harness) refusedTurn(sequence uint64, status int, message string) {
	h.t.Helper()
	h.report(sequence, true, message)
	h.errorTurn(StreamPromptResult{Error: message, HTTPStatus: status})
}

// refusedUntilA400 is a working claim whose provider refused launch_failure_limit turns in a row:
// each but the last with a 529, which can clear, so the ladder sends the task again after it, and
// the last with a 400, which cannot clear and fails the claim. It answers the claim's harness and
// that limit.
func refusedUntilA400(t *testing.T) (*harness, int) {
	t.Helper()
	h := workingClaim(t)
	limit := testLimits().LaunchFailures
	for turn := 1; turn < limit; turn++ {
		h.refusedTurn(uint64(turn), 529, "overloaded_error: Overloaded")
		h.climbs(turn, promptRetryDelay(turn))
	}
	h.refusedTurn(uint64(limit), 400, "invalid_request_error: messages: unexpected role")
	return h, limit
}

// A run of refused turns fails no claim, however long it is (dispatch://INFRA-4508). Each turn
// here ends as an Oh My Pi worker's turn ends when its provider is overloaded: the producer
// reports it refused, and the prompt's result names a 529, an error that can clear. Twenty-five
// such turns in a row, far past the launch failure limit at which the run used to fail the claim,
// leave the claim working in the session it had: the task goes to that session again after every
// turn, on the ladder's waits and with no attempt limit, and the project's controller is told
// once, at the sixth turn (providerErrorRunTold).
func TestARunOfRefusedTurnsThatCanClearHasNoAttemptLimit(t *testing.T) {
	notices := 0
	h := workingClaim(t, func(d *Deps) {
		d.ProviderErrorRunReached = func(Claim, string) { notices++ }
	})
	spawns, resumes := len(h.calls("Spawn")), len(h.calls("Resume"))
	const turns = 25

	for turn := 1; turn <= turns; turn++ {
		h.refusedTurn(uint64(turn), 529, "overloaded_error: Overloaded")
		if c := h.claim(); c.State == StateFailed {
			t.Fatalf("refused turn %d failed the claim: %q", turn, c.Failure)
		}
		told := 0
		if turn >= providerErrorRunTold {
			told = 1
		}
		if notices != told {
			t.Fatalf("after refused turn %d the controller was told %d times, want %d: once, at the sixth", turn, notices, told)
		}
		if sent := h.climbs(turn, promptRetryDelay(turn)); !strings.HasSuffix(sent, providerErrorTaskText) {
			t.Fatalf("after refused turn %d the ladder sent %q, want the task again", turn, sent)
		}
	}

	h.wantState(StateWorking)
	if c := h.claim(); c.Session != session || c.Failure != "" {
		t.Fatalf("after %d refused turns the claim records session %q and failure %q, want the session it had and no failure",
			turns, c.Session, c.Failure)
	}
	h.wantCalls("Spawn", spawns)
	h.wantCalls("Resume", resumes)
	// The run is counted up to the launch failure limit, which is what a retry reads.
	h.wantRefusedRun(testLimits().LaunchFailures)
	if got := h.claim().ProviderErrorRun; got != turns {
		t.Errorf("the run of provider errors is %d, want %d: the notice bounds nothing", got, turns)
	}
	if stored := h.store.load(testToken); stored.State == StateFailed || stored.Failure != "" {
		t.Errorf("the stored claim is %s with failure %q, want it live", stored.State, stored.Failure)
	}
	h.wantBudgets(Budgets{})
}

// One refused turn whose provider error cannot clear fails the claim at once. The turns before it
// here are refused with a 529, which can clear, so the 400's turn is the launch_failure_limit-th
// refusal in a row: the claim's failure names the status the prompt's result gave, not a run of
// refused turns, the claim keeps the task it owes for its architect, and nothing is sent again.
func TestARefusedTurnWhoseErrorCannotClearFailsTheClaimAtOnce(t *testing.T) {
	h, limit := refusedUntilA400(t)

	h.wantState(StateFailed)
	c := h.claim()
	if !strings.Contains(c.Failure, "cannot clear") || !strings.Contains(c.Failure, "HTTP 400") || strings.Contains(c.Failure, "in a row") {
		t.Fatalf("the failure is %q, want the provider error that cannot clear named by its status, not a run of refused turns", c.Failure)
	}
	if c.FailureCause != CauseProviderError {
		t.Errorf("the failure's cause is %q, want %q", c.FailureCause, CauseProviderError)
	}
	if c.Pending == nil {
		t.Error("the failed claim holds no task; the work is still owed and the architect decides it")
	}
	if stored := h.store.load(testToken); stored.State != StateFailed || stored.Failure != c.Failure {
		t.Errorf("the stored claim is %s with failure %q, want the failure persisted", stored.State, stored.Failure)
	}
	h.wantRefusedRun(limit)
	// The task and the 529s' sends of it, one each; the 400 sends nothing again, however long the
	// claim waits.
	h.advance(promptRetryMax)
	h.wantPrompts(limit)
}

// A report that arrives after the next turn has already reported lands in its own place: the run
// counts the turns, not the calls, so a late refusal of turn 2 beside refusals of 1 and 3 makes a
// run of three, exactly as the three in order would, and the newest refusal is turn 3's whichever
// arrived last.
func TestALateReportLandsInItsOwnPlaceInTheRun(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	h.report(1, true, "first")
	h.report(3, true, "third")
	h.wantRefusedRun(2)

	h.report(2, true, "second, reported late")

	h.wantRefusedRun(3)
	h.wantState(StateIdle)
	if got := h.claim().LastRefusal(); got != "third" {
		t.Fatalf("the newest refusal is %q, want turn 3's: the order is the turns', not the arrivals'", got)
	}
}

// The reports alone fail nothing either: a run of refused reports past the launch failure limit
// is recorded and persisted, counted up to that limit, and leaves the claim where it was, with
// its process and its session. These are reports of turns the daemon's prompts ran, which those
// prompts' results judge, so the reports decide neither (turnOutcome).
func TestARunOfRefusedReportsPastTheLimitLeavesTheClaimWhereItWas(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	limit := testLimits().LaunchFailures

	for turn := 1; turn <= 3*limit; turn++ {
		h.report(uint64(turn), true, "the provider ended the turn with an error at 226k tokens of context")
		h.wantState(StateIdle)
	}

	h.wantRefusedRun(limit)
	if c := h.claim(); c.Failure != "" || c.Session != session || c.Locator == nil {
		t.Fatalf("the claim records failure %q, session %q and process %+v, want no failure, its session and its process",
			c.Failure, c.Session, c.Locator)
	}
	if stored := h.store.load(testToken); stored.State != StateIdle || stored.RefusedRun() != limit {
		t.Errorf("the stored claim is %s with a run of %d, want idle with the run of %d persisted", stored.State, stored.RefusedRun(), limit)
	}
}

// A turn the provider took ends the run: the count is of refusals in a row, so an agent that is
// refused, works, and is refused again starts its run again from that turn.
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

	h.wantRefusedRun(3)
	if c := h.claim(); c.State == StateFailed || c.LastRefusal() != "refused in the resumed session too" {
		t.Fatalf("the claim is %s with newest refusal %q, want it live and the resumed generation's own refusal newest", c.State, c.LastRefusal())
	}
}

// The retry of a claim whose provider refused the recorded session's last launch_failure_limit
// turns in a row starts a new session rather than resuming the recorded one: the session every
// recent turn of which was refused would be refused again. The run fails nothing by itself; the
// claim here fails at its last refused turn, whose provider error cannot clear. The claim spawns
// instead of resuming, and the new session starts with a fresh run.
func TestTheRetryOfAClaimItsProviderRefusedStartsANewSession(t *testing.T) {
	h, limit := refusedUntilA400(t)
	h.wantState(StateFailed)
	h.wantRefusedRun(limit)
	spawns, resumes := len(h.calls("Spawn")), len(h.calls("Resume"))

	h.must(RequestRetry{Claim: h.token})

	h.wantState(StateLaunching)
	h.wantCalls("Spawn", spawns+1)
	h.wantCalls("Resume", resumes)
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
// that restarts between the second refusal and the third counts a run of three at the third, as
// the daemon that saw all three would, which is what its retry reads.
func TestTheReportsSurviveARestartAndTheRunGoesOnFromThem(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	h.report(1, true, "refused")
	h.report(2, true, "refused")

	h.restart()

	h.wantRefusedRun(2)
	h.report(3, true, "refused after the restart")
	h.wantRefusedRun(3)
	h.wantState(StateIdle)
}
