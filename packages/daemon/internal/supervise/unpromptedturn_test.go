package supervise

import (
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The cases of this file are dispatch://INFRA-4508's R4: a turn that ran no prompt of the
// daemon's — the phase reminder's, which the plugin starts itself with pi.sendMessage — has no
// prompt result, so the daemon judges it from its producer's report, which is marked unprompted
// and carries the status the provider refused it with where the provider library gave one
// (turnOutcome, unpromptedTurnEnded).

// pluginTurn is one such turn as its producer reports it, at the claim's current generation:
// refused with status, 0 where the library gave none, or clean. No prompt result follows it.
func (h *harness) pluginTurn(sequence uint64, refused bool, status int) {
	h.t.Helper()
	r := RequestTurnOutcome{
		Claim: h.token, Generation: h.generation(), Sequence: sequence, Refused: refused, HTTPStatus: status, Unprompted: true,
	}
	if refused {
		r.Message = "the provider refused the reminder's turn"
		if status != 0 {
			r.Message = strconv.Itoa(status) + " " + r.Message
		}
	}
	h.must(r)
}

// idleClaim is an implementer that has finished its task's turn and owes nothing, which is where a
// phase worker is when its plugin's reminder fires. set changes the dependencies before the
// machine is built, as workingClaim's does, which is how a case hears the controller's notice.
func idleClaim(t *testing.T, set ...func(*Deps)) *harness {
	t.Helper()
	h := newBareHarness(t)
	c := queuedClaim()
	h.token = c.Token
	for _, change := range set {
		change(&h.deps)
	}
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	h.reach(StateIdle)
	return h
}

// countNotices is a dependency change that counts the controller's notices of a run of
// provider-error turns into told.
func countNotices(told *int) func(*Deps) {
	return func(d *Deps) { d.ProviderErrorRunReached = func(Claim, string) { *told++ } }
}

// wantNotices fails the case unless the controller has been told want times after turn.
func wantNotices(t *testing.T, turn, told, want int) {
	t.Helper()
	if told != want {
		t.Fatalf("after turn %d the controller was told %d times, want %d", turn, told, want)
	}
}

// A plugin turn the provider refused with a 400, a client error that is none of 401, 403, 408
// and 429, cannot clear: it fails the claim at once with cause provider_error, as a refused
// prompt's 400 does, which holds the issue for its architect and ends the plugin's reminders.
// Without R4 the report only records the turn, and the claim stays idle.
func TestAPluginTurnRefusedWithA400FailsTheClaimAtOnce(t *testing.T) {
	h := idleClaim(t)

	h.pluginTurn(1, true, 400)

	h.wantState(StateFailed)
	c := h.claim()
	if c.FailureCause != CauseProviderError {
		t.Errorf("the failure's cause is %q, want %q", c.FailureCause, CauseProviderError)
	}
	for _, want := range []string{"cannot clear", "no prompt of the daemon's", "HTTP 400"} {
		if !strings.Contains(c.Failure, want) {
			t.Errorf("the failure is %q, want it to say %q", c.Failure, want)
		}
	}
	if strings.Contains(c.Failure, "refused the reminder's turn") {
		t.Errorf("the failure is %q, want none of the provider's own words in it", c.Failure)
	}
	if c.Session != session {
		t.Errorf("the failed claim records session %q, want %q kept for its architect's retry", c.Session, session)
	}
	if stored := h.store.load(testToken); stored.State != StateFailed || stored.Failure != c.Failure {
		t.Errorf("the stored claim is %s with failure %q, want the failure persisted", stored.State, stored.Failure)
	}
}

// Plugin turns refused with a status that can clear — a 529, the provider overloaded, or a 429, a
// rate limit — fail nothing, however many: twenty-five in a row leave the claim idle in the
// session it had, and the project's controller is told once, at the sixth, because each counts in
// the claim's run of provider-error turns (ProviderErrorRun). Without R4 the controller is told
// nothing.
func TestPluginTurnsRefusedWithAStatusThatCanClearFailNothingAndTellTheControllerOnce(t *testing.T) {
	for _, status := range []int{529, 429} {
		t.Run("HTTP "+strconv.Itoa(status), func(t *testing.T) {
			told := 0
			h := idleClaim(t, countNotices(&told))
			spawns, resumes := len(h.calls("Spawn")), len(h.calls("Resume"))
			const turns = 25

			for turn := 1; turn <= turns; turn++ {
				h.pluginTurn(uint64(turn), true, status)
				if c := h.claim(); c.State != StateIdle {
					t.Fatalf("refused plugin turn %d left the claim %s (%q), want it idle", turn, c.State, c.Failure)
				}
				wantNotices(t, turn, told, min(turn/providerErrorRunTold, 1))
			}

			if c := h.claim(); c.Session != session || c.Failure != "" || c.ProviderErrorRun != turns {
				t.Fatalf("after %d refused plugin turns the claim records session %q, failure %q and a run of %d, want the session it had, no failure and a run of %d",
					turns, c.Session, c.Failure, c.ProviderErrorRun, turns)
			}
			h.wantCalls("Spawn", spawns)
			h.wantCalls("Resume", resumes)
			if stored := h.store.load(testToken); stored.ProviderErrorRun != turns {
				t.Errorf("the stored run of provider errors is %d, want %d", stored.ProviderErrorRun, turns)
			}
		})
	}
}

// A plugin turn whose report carries no status — a content filter's refusal, or a provider
// library that named none — is one whose status cannot be read: it fails nothing, and it counts in
// the run, so the controller is told once at the sixth such turn. Without R4 the controller is
// told nothing.
func TestPluginTurnsRefusedWithNoStatusFailNothingAndTellTheControllerOnce(t *testing.T) {
	told := 0
	h := idleClaim(t, countNotices(&told))

	for turn := 1; turn <= 2*providerErrorRunTold; turn++ {
		h.pluginTurn(uint64(turn), true, 0)
		h.wantState(StateIdle)
		wantNotices(t, turn, told, min(turn/providerErrorRunTold, 1))
	}

	if got := h.claim().ProviderErrorRun; got != 2*providerErrorRunTold {
		t.Errorf("the run of provider errors is %d, want %d: every refused plugin turn counts in it", got, 2*providerErrorRunTold)
	}
}

// Refused prompts and refused plugin turns make one run: a worker whose task's prompts are refused
// with a 529 and whose reminders in between are refused too has its controller told exactly once,
// at the sixth turn of the run, whichever kind that turn is. A clean plugin turn ends the run, as a
// clean prompt does, so six more refusals after it tell the controller a second time. The prompts'
// own reports are not marked unprompted, so only their results count them. Without R4 only the
// prompts count, and the run reaches six at the eleventh turn.
func TestARunOfRefusedPromptsAndPluginTurnsTellsTheControllerOnceAtItsSixth(t *testing.T) {
	told := 0
	h := workingClaim(t, countNotices(&told))
	var sequence uint64
	next := func() uint64 { sequence++; return sequence }

	for turn := 1; turn <= 8; turn += 2 {
		h.refusedTurn(next(), 529, "529 overloaded_error: Overloaded")
		wantNotices(t, turn, told, min(turn/providerErrorRunTold, 1))
		h.pluginTurn(next(), true, 529)
		wantNotices(t, turn+1, told, min((turn+1)/providerErrorRunTold, 1))
		attempt := (turn + 1) / 2
		h.climbs(attempt, promptRetryDelay(attempt))
	}
	if c := h.claim(); c.ProviderErrorRun != 8 || c.State == StateFailed {
		t.Fatalf("after eight refused turns the claim is %s with a run of %d, want it live with a run of 8", c.State, c.ProviderErrorRun)
	}

	h.pluginTurn(next(), false, 0)

	if got := h.claim().ProviderErrorRun; got != 0 {
		t.Fatalf("the run of provider errors is %d after a clean plugin turn, want 0", got)
	}
	if got := h.store.load(testToken).ProviderErrorRun; got != 0 {
		t.Errorf("the stored run of provider errors is %d after a clean plugin turn, want 0", got)
	}
	for turn := 1; turn <= providerErrorRunTold; turn++ {
		h.pluginTurn(next(), true, 529)
	}
	if told != 2 {
		t.Fatalf("the controller was told %d times, want twice: once for each run that reached %d", told, providerErrorRunTold)
	}
}

// A clean plugin turn ends a run of refused prompts: the worker's provider answered it, so the
// run of turns that ended in a provider error is over, whichever kind of turn ended it. Without R4
// the run stands at two.
func TestACleanPluginTurnEndsTheRunOfRefusedPrompts(t *testing.T) {
	h := workingClaim(t)
	h.refusedTurn(1, 529, "529 overloaded_error: Overloaded")
	h.climbs(1, promptRetryDelay(1))
	h.refusedTurn(2, 529, "529 overloaded_error: Overloaded")
	if got := h.claim().ProviderErrorRun; got != 2 {
		t.Fatalf("the run of provider errors is %d, want 2", got)
	}

	h.pluginTurn(3, false, 0)

	if got := h.claim().ProviderErrorRun; got != 0 {
		t.Errorf("the run of provider errors is %d after a clean plugin turn, want 0", got)
	}
}

// The report of a turn the daemon's prompt ran is never judged, whether it reaches the daemon
// before that prompt's result or after it: the two travel on two channels, and the result alone
// judges the turn, so the turn counts once in the run either way. This case passes without R4 as
// well, since then no report is judged at all: it guards R4's own count.
func TestAPromptedTurnsReportCountsOnceWhicheverSideOfItsResultItArrives(t *testing.T) {
	for name, reportFirst := range map[string]bool{
		"its report before its result": true,
		"its report after its result":  false,
	} {
		t.Run(name, func(t *testing.T) {
			h := workingClaim(t)
			report := RequestTurnOutcome{
				Claim: h.token, Generation: h.generation(), Sequence: 1, Refused: true,
				Message: "529 overloaded_error: Overloaded", HTTPStatus: 529,
			}

			if reportFirst {
				h.must(report)
				h.errorTurn(StreamPromptResult{Error: "overloaded_error", HTTPStatus: 529})
			} else {
				h.errorTurn(StreamPromptResult{Error: "overloaded_error", HTTPStatus: 529})
				h.must(report)
			}

			if got := h.claim().ProviderErrorRun; got != 1 {
				t.Fatalf("the run of provider errors is %d after one refused turn, want 1", got)
			}
			h.wantRefusedRun(1)
		})
	}
}

// A report the plugin sends again — it does, when the daemon restarted under its first attempt —
// is the same turn, and counts once.
func TestAPluginTurnReportSentTwiceCountsOnce(t *testing.T) {
	h := idleClaim(t)

	h.pluginTurn(1, true, 529)
	h.pluginTurn(1, true, 529)

	if got := h.claim().ProviderErrorRun; got != 1 {
		t.Fatalf("the run of provider errors is %d after one refused turn reported twice, want 1", got)
	}
}

// A plugin turn's report that names a generation the claim has left is a turn of a process that
// is gone: it is recorded in its place in the run of refused turns, and it fails nothing and
// counts in no run of provider errors, even with a status that cannot clear.
func TestAPluginTurnReportOfAGenerationTheClaimLeftIsNotJudged(t *testing.T) {
	h := idleClaim(t)
	h.observe(runtime.Gone)
	h.relaunched()
	if h.generation() != 2 {
		t.Fatalf("generation %d, want the relaunch's 2", h.generation())
	}

	h.must(RequestTurnOutcome{
		Claim: h.token, Generation: 1, Sequence: 9, Refused: true, Message: "400 from the process that is gone",
		HTTPStatus: 400, Unprompted: true,
	})

	if c := h.claim(); c.State == StateFailed || c.ProviderErrorRun != 0 {
		t.Fatalf("the claim is %s with a run of %d after a report of the generation it left, want it live with none", c.State, c.ProviderErrorRun)
	}
	h.wantRefusedRun(1)
}
