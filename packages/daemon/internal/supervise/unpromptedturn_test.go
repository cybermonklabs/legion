package supervise

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/texts"
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
	return func(d *Deps) { d.ProviderErrorRunReached = func(Claim, texts.ProviderErrorsRecord) { *told++ } }
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

// A clean report the plugin marked prompted ends the run too, though no prompt result of its own
// comes for it: the run after one that started in the window between Oh My Pi's acceptance of a
// prompt and its `input` (that run itself is reported unprompted), or a run after a prompt the host
// refused as busy, is read as the prompt's. The provider took that turn, so five refusals, that
// clean turn and one more refusal make a run of one, and the controller hears nothing; counted
// across the clean turn, the run would reach six and tell it.
//
// A clean report ends the run only when no later turn has reported before it: a clean report that
// reaches the daemon after a later turn's refused report is of a turn before that refusal, so the
// run the refusal began stays, whether the late report is marked prompted or not.
func TestACleanReportMarkedPromptedEndsTheRunWithNoPromptResultOfItsOwn(t *testing.T) {
	told := 0
	h := workingClaim(t, countNotices(&told))
	for turn := 1; turn < providerErrorRunTold; turn++ {
		h.refusedTurn(uint64(turn), 529, "529 overloaded_error: Overloaded")
		h.climbs(turn, promptRetryDelay(turn))
	}
	if got := h.claim().ProviderErrorRun; got != providerErrorRunTold-1 {
		t.Fatalf("the run of provider errors is %d, want %d", got, providerErrorRunTold-1)
	}

	h.report(providerErrorRunTold, false, "")
	h.refusedTurn(providerErrorRunTold+1, 529, "529 overloaded_error: Overloaded")

	if got := h.claim().ProviderErrorRun; got != 1 {
		t.Fatalf("the run of provider errors is %d after a clean prompted report and one refusal, want 1", got)
	}
	if told != 0 {
		t.Fatalf("the controller was told %d times, want never: the run never reached %d", told, providerErrorRunTold)
	}

	// The refused report of turn 9 arrives before the clean report of turn 8, prompted.
	h.pluginTurn(providerErrorRunTold+3, true, 529)
	h.report(providerErrorRunTold+2, false, "")
	if got := h.claim().ProviderErrorRun; got != 2 {
		t.Fatalf("the run of provider errors is %d after an earlier turn's clean prompted report arrived late, want the 2 it was", got)
	}
	// And the same with the late clean report unprompted: turn 11's refusal, then turn 10 clean.
	h.pluginTurn(providerErrorRunTold+5, true, 529)
	h.pluginTurn(providerErrorRunTold+4, false, 0)
	if got := h.claim().ProviderErrorRun; got != 3 {
		t.Fatalf("the run of provider errors is %d after an earlier turn's clean unprompted report arrived late, want the 3 it was", got)
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

// The controller's notice says what the daemon does about the turn that reached the count. A run
// of refused reminders on an idle claim owes no task, so nothing is sent again, and the notice
// says that the last turn ran no prompt of the daemon's and names no wait between sends; a run
// that a refused prompt reaches six at keeps that prompt's task and sends it again, and its notice
// names the longest wait between those sends (TestTheSixthProviderErrorTurnInARowTellsTheControllerOnce
// holds the model it names).
func TestTheNoticeOfARunEndedByAPluginTurnSendsNoTaskAgain(t *testing.T) {
	var notices []texts.ProviderErrorsRecord
	tell := func(d *Deps) {
		d.ProviderErrorRunReached = func(_ Claim, notice texts.ProviderErrorsRecord) { notices = append(notices, notice) }
	}

	h := idleClaim(t, tell)
	for turn := 1; turn <= providerErrorRunTold; turn++ {
		h.pluginTurn(uint64(turn), true, 529)
	}
	if len(notices) != 1 {
		t.Fatalf("the controller was told %d times, want once", len(notices))
	}
	if got := notices[0]; got.Turns != providerErrorRunTold || got.HTTPStatus != 529 || !got.LastTurnUnprompted {
		t.Errorf("the notice is %+v, want it to name %d turns in a row, HTTP 529, and a last turn that ran no prompt of the daemon's",
			got, providerErrorRunTold)
	}
	if got := notices[0].RetryEvery; got != "" {
		t.Errorf("the notice names a task kept and sent again every %s, want none: the claim owes no task", got)
	}

	notices = nil
	p := workingClaim(t, tell)
	for turn := 1; turn < providerErrorRunTold; turn++ {
		p.pluginTurn(uint64(turn), true, 529)
	}
	p.refusedTurn(providerErrorRunTold, 529, "529 overloaded_error: Overloaded")
	if len(notices) != 1 || notices[0].LastTurnUnprompted || notices[0].RetryEvery != promptRetryMax.String() {
		t.Fatalf("the notices are %+v, want one that says the prompt's task is kept and sent again every %s at most", notices, promptRetryMax)
	}
}

// The turn-outcome answer says whether this report failed the claim, read under the report's own
// decision (HandleTurnOutcome): true for the report of a plugin turn refused with a 400, and false
// for a clean report, a refused report of a turn a prompt ran, and one that can clear. The answer
// is this report's even when another event's decision fails the claim the moment this report's
// decision lets its lock go (Machine.turnOutcomeDecided): a prompt result's 400 there leaves the
// clean report answering that it failed nothing, where a read of the claim made after the
// decision returned would find it failed and answer true.
func TestTheTurnOutcomeAnswerSaysWhetherThisReportFailedTheClaim(t *testing.T) {
	h := workingClaim(t)
	for i, tc := range []struct {
		refused, unprompted bool
		status              int
	}{
		{false, true, 0},
		{true, false, 400},
		{true, true, 529},
	} {
		answer, err := h.m.HandleTurnOutcome(h.ctx, RequestTurnOutcome{
			Claim: h.token, Generation: h.generation(), Sequence: uint64(i + 1), Refused: tc.refused,
			HTTPStatus: tc.status, Unprompted: tc.unprompted,
		})
		if err != nil || answer.Failed {
			t.Fatalf("report %d (%+v) answered %+v, %v; want it not failed", i+1, tc, answer, err)
		}
	}
	h.wantState(StateWorking)

	answer, err := h.m.HandleTurnOutcome(h.ctx, RequestTurnOutcome{
		Claim: h.token, Generation: h.generation(), Sequence: 4, Refused: true, HTTPStatus: 400, Unprompted: true,
	})

	if want := (TurnOutcomeAnswer{RefusedRun: min(3, testLimits().LaunchFailures), Failed: true}); err != nil || answer != want {
		t.Fatalf("the 400 answered %+v, %v; want %+v: the run of the three refused reports, and the claim failed", answer, err, want)
	}
	h.wantState(StateFailed)

	// A clean report whose decision another event's follows at once: the prompt result's 400
	// fails the claim between this report's decision and the return of HandleTurnOutcome.
	p := workingClaim(t)
	p.m.turnOutcomeDecided = func() {
		p.m.turnOutcomeDecided = nil
		p.errorTurn(StreamPromptResult{Error: "invalid_request_error", HTTPStatus: 400})
	}

	answer, err = p.m.HandleTurnOutcome(p.ctx, RequestTurnOutcome{
		Claim: p.token, Generation: p.generation(), Sequence: 1,
	})

	p.wantState(StateFailed)
	if err != nil || answer != (TurnOutcomeAnswer{}) {
		t.Fatalf("the clean report answered %+v, %v; want no run and not failed: the prompt result's 400 failed the claim after this report's decision",
			answer, err)
	}
}

// A report the table ignores runs no judgment, so it failed nothing, and its answer says so. The
// report of a plugin turn refused with a 400 fails the claim and answers that it did; the claim
// is failed then (and a failed claim takes no suspension, table.go's onSuspend), so the next
// report reaches a state whose row ignores it (onTurnOutcome, failedClaim): it is refused and
// turnOutcome does not run for it. That report's answer must not be the earlier report's, since
// HandleTurnOutcome's caller reads Failed as "this report failed the claim".
func TestAnIgnoredTurnOutcomeAnswersNotFailed(t *testing.T) {
	h := workingClaim(t)
	answer, err := h.m.HandleTurnOutcome(h.ctx, RequestTurnOutcome{
		Claim: h.token, Generation: h.generation(), Sequence: 1, Refused: true, HTTPStatus: 400, Unprompted: true,
	})
	if err != nil || !answer.Failed {
		t.Fatalf("the 400 answered %+v, %v; want it failed", answer, err)
	}
	h.wantState(StateFailed)

	answer, err = h.m.HandleTurnOutcome(h.ctx, RequestTurnOutcome{
		Claim: h.token, Generation: h.generation(), Sequence: 2, Refused: true, HTTPStatus: 400, Unprompted: true,
	})

	h.wantState(StateFailed)
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("the report to the failed claim returned %v, want the table's refusal: the failed claim ignores it", err)
	}
	if answer.Failed {
		t.Errorf("the report the failed claim ignored answered %+v; want it not failed: it judged nothing, and the failure was the report before it", answer)
	}
}
