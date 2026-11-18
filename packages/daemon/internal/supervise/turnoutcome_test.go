package supervise

import (
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

// A report that arrives after the next turn has already reported lands in its own place. Until it
// lands its turn is a gap, which the run ends at, so refusals of turns 1 and 3 are a run of one;
// the late refusal of turn 2 fills the gap and is the third refusal in a row, and fails the claim
// exactly as the three in order would.
func TestALateReportLandsInItsOwnPlaceAndFailsTheClaimAsAnInOrderOneWould(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	h.report(1, true, "first")
	h.report(3, true, "third")
	h.wantRefusedRun(1)
	h.wantState(StateIdle)

	h.report(2, true, "second, reported late")

	h.wantState(StateFailed)
	if want := "the provider refused 3 turns in a row: third"; h.claim().Failure != want {
		t.Fatalf("failure %q, want %q: the newest refused report names the message, whichever arrived last", h.claim().Failure, want)
	}
}

// A report still on its way leaves a gap in the sequence, and a run of refused turns ends at the
// gap: during an outage each turn's report waits to be sent again on a backoff of its own, so a
// clean turn's report and the refused turn's after it can land in either order. A refusal that
// lands first, read across the gap, would be the third in a row after the two before the outage
// and fail a claim whose provider took the turn between them. At the gap it is a run of one, and
// the clean report, once it lands, ends the run before it. A relaunched generation's own first
// turn is the same gap: its second refusal lands before its first turn's clean report.
func TestARefusalThatLandsBeforeAnEarlierTurnsReportCompletesNoRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		// relaunch is the claim relaunched after the two refusals, so the two turns below are the
		// new generation's first and second.
		relaunch bool
		// clean is the turn whose report is still on its way, refused the turn after it.
		clean, refused uint64
	}{
		{name: "within one generation", clean: 3, refused: 4},
		{name: "the relaunched generation's first turn", relaunch: true, clean: 1, refused: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateIdle)
			h.report(1, true, "refused before the outage")
			h.report(2, true, "refused before the outage")
			if tc.relaunch {
				h.observe(runtime.Gone)
				h.relaunched()
			}
			live := h.claim().State

			h.report(tc.refused, true, "refused after the outage, landing first")

			h.wantState(live)
			h.wantRefusedRun(1)
			h.report(tc.clean, false, "")
			h.wantState(live)
			h.wantRefusedRun(1)
		})
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

// A turn whose report never lands — the daemon refused it, or it was given up on after the hour a
// report waits for a daemon that is not serving — is a gap the run ends at, whatever the turn was:
// the refusals on either side of it are runs of one, not one run of two, and the claim goes on.
func TestATurnWithNoReportEndsTheRun(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)

	h.report(1, true, "refused")
	// Turn 2's report never landed.
	h.report(3, true, "refused")

	h.wantState(StateIdle)
	h.wantRefusedRun(1)
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
