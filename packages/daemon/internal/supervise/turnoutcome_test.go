package supervise

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// report is one turn's outcome as its producer reports it, at the claim's current generation,
// naming no task and marked as nothing: a report of a turn that was not the take's, which a
// refusal of is judged by its status alone and no status here gives (turnOutcome,
// unpromptedTurnEnded), so it fails nothing.
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

// refusedTurn ends the turn of the take the claim is working as a current Oh My Pi worker's
// refused turn ends: the end frame, and then its producer's end report, which names the take's
// task and carries the provider's failure with the status the provider answered, which alone
// decides what the error does (reportedTask, providerErrored). The report goes under sequence, so
// a case that interleaves it with reports of other turns keeps one order of the producer's turns.
func (h *harness) refusedTurn(sequence uint64, status int, message string) {
	h.t.Helper()
	number := h.pending().Number
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	if err := h.m.Handle(h.ctx, RequestTurnOutcome{Claim: h.token, Generation: h.generation(), Sequence: sequence,
		Task: number, Refused: true, Message: message, ProviderError: &ProviderError{Status: status, Message: message}}); err != nil {
		h.t.Fatalf("report the refused turn of task %d: %v", number, err)
	}
	h.m.Wait()
	h.workerAsks()
}

// refusedUntilA400 is a working claim whose provider refused launch_failure_limit turns in a row:
// each but the last with a 529, which can clear, so the ladder writes the task's value again after
// it for the worker to take, and the last with a 400, which cannot clear and fails the claim. It
// answers the claim's harness and that limit.
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
// here ends as a current Oh My Pi worker's take's turn ends when its provider is overloaded: the
// producer's end report names the task and carries a 529, an error that can clear. Twenty-five
// such turns in a row, far past the launch failure limit at which the run used to fail the claim,
// leave the claim working in the session it had: the task goes to that session again after every
// turn, on the ladder's waits and with no attempt limit, and the run is handed on once
// (Deps.ProviderErrorRunReached), at the sixth turn (providerErrorRunTold).
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
			t.Fatalf("after refused turn %d the run was handed on %d times, want %d: once, at the sixth", turn, notices, told)
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
	// The run is counted up to the launch failure limit, which is what each report is answered with.
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
// refusal in a row: the claim's failure names the status the take's end report gave, not a run
// of refused turns, the claim keeps the task it owes for its architect, and nothing is given again.
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
// its process and its session. These reports name no task and their refusals no status, so each
// is judged as a turn that was not the take's, by a status that can clear (unpromptedTurnEnded).
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

// A report whose write failed (TurnOutcome.Unapplied) stays in the window until it is sent again,
// held beside the window's bound rather than in it, so the run of refused turns that follows it is
// read whole: at the limit it reads the limit, as a run with no such report before it does. In the
// bound, that report would take the place of the run's oldest turn, and the run would stop one
// short of the limit for as long as the report stayed unsent.
//
// The negative control is the commit under this change, whose bound counted the report never
// applied: the third refused turn after it left a run of two.
func TestARefusedRunAfterAReportWhoseWriteFailedIsTakenWhole(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	limit := testLimits().LaunchFailures
	unapplied := h.worker.sequence + 1
	h.store.fail("PutClaim", errBoom)
	if err := h.m.Handle(h.ctx, RequestTurnOutcome{Claim: h.token, Generation: h.generation(), Sequence: unapplied}); !errors.Is(err, errBoom) {
		t.Fatalf("the report whose write failed = %v, want %v", err, errBoom)
	}
	h.m.Wait()
	h.store.fail("PutClaim", nil)
	for turn := 1; turn < limit; turn++ {
		h.report(unapplied+uint64(turn), true, "stopReason: error")
		h.wantState(StateIdle)
	}

	h.report(unapplied+uint64(limit), true, "the provider refused the turn")

	h.wantState(StateIdle)
	h.wantRefusedRun(limit)
	if kept := h.claim().TurnOutcomes; len(kept) != limit+1 || !kept[0].Unapplied {
		t.Fatalf("the window keeps %+v, want the report whose write failed beside the %d newest", kept, limit)
	}
}

// A report never applied of a generation earlier than the claim's is dropped by the window's trim
// as an applied one is: no process sends it again, and it reaches no take (reportOfTake), so a
// producer that died before its report was sent again leaves no entry stored for good. The newer
// generation's reports fill the window to its bound, and the report the dead process never got
// applied goes. The negative control is the commit under this change, whose trim kept every report
// never applied, whatever its generation.
func TestAReportNeverAppliedOfAnEarlierGenerationIsTakenOutOfTheWindow(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	lost := h.generation()
	h.store.fail("PutClaim", errBoom)
	if err := h.m.Handle(h.ctx, RequestTurnOutcome{Claim: h.token, Generation: lost, Sequence: h.worker.sequence + 1}); !errors.Is(err, errBoom) {
		t.Fatalf("the report whose write failed = %v, want %v", err, errBoom)
	}
	h.m.Wait()
	h.store.fail("PutClaim", nil)
	h.observe(runtime.Gone)
	h.relaunched()
	if h.generation() == lost {
		t.Fatalf("the relaunch left the claim at generation %d, want a later one", lost)
	}
	limit := testLimits().LaunchFailures

	for sequence := 1; sequence <= limit; sequence++ {
		h.report(uint64(sequence), false, "")
	}

	kept := h.claim().TurnOutcomes
	if len(kept) != limit || slices.ContainsFunc(kept, func(o TurnOutcome) bool { return o.Generation == lost }) {
		t.Fatalf("the window keeps %+v, want the %d reports of the generation after %d alone", kept, limit, lost)
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

// Both producers report a turn a provider failed as refused, with the failure beside it
// (pi-legion's turn-outcome, the Claude bridge's turnEnded). Such a turn of the take's task is the
// provider-error ladder's, which has no attempt limit (dispatch://INFRA-956 R2): a run of them
// longer than the launch failure limit never fails the claim, the window's run of refused turns
// counts each up to that limit and decides nothing, and every one of them takes the task back and
// writes it again after the ladder's wait, until the run reaches the turn the project's controller
// is told at (providerErrorRunTold). A refused turn that names no task and carries no status fails
// nothing either (dispatch://INFRA-4508): it is judged as a turn that was not the take's, by a
// status that can clear, and counts in the same run of provider-error turns.
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
		h.wantRefusedRun(min(turn, limit))
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
		h.wantState(StateIdle)
	}
	h.wantRefusedRun(limit)
	if c := h.claim(); c.Failure != "" || c.ProviderErrorRun != providerErrorRunTold+limit {
		t.Fatalf("the claim records failure %q and a run of %d provider errors, want no failure and a run of %d: a refused turn of no task fails nothing and counts in the one run",
			c.Failure, c.ProviderErrorRun, providerErrorRunTold+limit)
	}
}

// A refused turn that carries a provider error and reaches no provider-error ladder — its report
// names no task, or a number no take of the claim holds, as a reminder's, a card's or a background
// result's turn does — is judged from its report alone (unpromptedTurnEnded): it counts toward the
// refused run and in the claim's run of provider-error turns, and a status that can clear fails
// nothing, however long the run (dispatch://INFRA-4508). A provider error on the turn of the task
// the claim's take holds stays the ladder's (TestTurnOutcomeAProviderErrorTurnIsNoLaunchFailure).
//
// The negative control is every provider error left uncounted (turnOutcome's refused before this
// change): the run stays at zero.
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
				}
				h.wantState(StateIdle)
			}

			h.wantRefusedRun(limit)
			if c := h.claim(); c.Failure != "" || c.ProviderErrorRun != limit {
				t.Fatalf("the claim records failure %q and a run of %d provider errors, want no failure and a run of %d", c.Failure, c.ProviderErrorRun, limit)
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

	h.wantRefusedRun(3)
	if c := h.claim(); c.State == StateFailed || c.LastRefusal() != "refused in the resumed session too" {
		t.Fatalf("the claim is %s with newest refusal %q, want it live and the resumed generation's own refusal newest", c.State, c.LastRefusal())
	}
}

// A retry resumes the recorded session with fresh budgets, whoever makes it, unless it asks for a
// new session (dispatch://INFRA-4508's R5, check c-retry-keeps-session): a run of refused turns,
// however long, is the claim's own count of refusals, mostly ones that can clear — a team limit's
// 429 refuses every session of the team alike — and no sign that its session is bad. Two runs here
// are of twenty refused turns, each a plugin turn's report, one with a 429 and one with no status;
// neither fails the claim, so it fails the way such a claim does, its process dying until its
// launch budget runs out. A claim the 400 of its take's end report failed resumes too, and so do
// claims that failed on deaths, retried by the workflow or by an operator. Only the retry that asks
// for a new session (`--fresh-session`, the controller's fresh_session) leaves the recorded session
// behind, whatever failed the claim. Each row states which it wants. With freshSession as at
// 9e30e970, which started a new session for a run that reached launch_failure_limit, the 429 and
// no-status rows that ask for nothing start a new session and fail.
func TestARetryResumesTheRecordedSessionUnlessItAsksForANewOne(t *testing.T) {
	const run = 20
	const newSessionLine = "supervise: retrying in a new session"
	refusedRun := func(status int) func(*testing.T) *harness {
		return func(t *testing.T) *harness {
			t.Helper()
			h := newHarness(t)
			h.reach(StateReady)
			for turn := 1; turn <= run; turn++ {
				h.pluginTurn(uint64(turn), true, status)
			}
			for range testLimits().LaunchFailures {
				h.observe(runtime.Gone)
			}
			h.wantState(StateFailed)
			h.wantRefusedRun(testLimits().LaunchFailures)
			return h
		}
	}
	failedByA400 := func(t *testing.T) *harness {
		t.Helper()
		h := workingClaim(t)
		h.refusedTurn(1, 400, "400 invalid_request_error: prompt is too long")
		h.wantState(StateFailed)
		return h
	}
	diedOut := func(c Claim) func(*testing.T) *harness {
		return func(t *testing.T) *harness {
			t.Helper()
			h := newHarnessOf(t, c)
			h.reach(StateReady)
			h.report(1, true, "refused once, which is not the budget")
			for range testLimits().LaunchFailures {
				h.observe(runtime.Gone)
			}
			h.wantState(StateFailed)
			return h
		}
	}
	byWorkflow := func(fresh bool) func(claim.Token) Event {
		return func(token claim.Token) Event { return RequestRetry{Claim: token, FreshSession: fresh} }
	}
	byOperator := func(fresh bool) func(claim.Token) Event {
		return func(token claim.Token) Event {
			return RequestAskedRetry{Claim: token, FreshSession: fresh, By: SuspendedByOperator}
		}
	}

	for _, tc := range []struct {
		name  string
		claim func(*testing.T) *harness
		retry func(claim.Token) Event
		fresh bool
	}{
		{"a run of 20 refused turns that ends in a 429 resumes its session", refusedRun(429), byWorkflow(false), false},
		{"a run of 20 refused turns that ends in a report with no status resumes its session", refusedRun(0), byWorkflow(false), false},
		{"a claim the 400 of its take's end report failed resumes its session", failedByA400, byWorkflow(false), false},
		{"a claim that died out, retried by the workflow, resumes its session", diedOut(queuedClaim()), byWorkflow(false), false},
		{"a claim that died out, retried by an operator, resumes its session", diedOut(rootClaim()), byOperator(false), false},
		{"a run of 20 refused turns that ends in a 429, retried with --fresh-session, starts a new session", refusedRun(429), byWorkflow(true), true},
		{"a claim that died out, retried by the workflow asking for a new session, starts one", diedOut(queuedClaim()), byWorkflow(true), true},
		{"a claim that died out, retried by an operator asking for a new session, starts one", diedOut(rootClaim()), byOperator(true), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.claim(t)
			resumes, spawns := len(h.calls("Resume")), len(h.calls("Spawn"))

			h.must(tc.retry(h.token))

			h.wantState(StateLaunching)
			lines := h.records.of(newSessionLine)
			if !tc.fresh {
				h.wantCalls("Resume", resumes+1)
				h.wantCalls("Spawn", spawns)
				if c := h.claim(); c.Session != session || c.SessionFile != sessionFile || c.Abandoned.Session != "" {
					t.Fatalf("the retried claim records session %q (%s) and abandoned %+v, want %q resumed and nothing abandoned",
						c.Session, c.SessionFile, c.Abandoned, session)
				}
				h.wantBudgets(Budgets{})
				if len(lines) != 0 {
					t.Fatalf("the retry logged %q %d times, want never", newSessionLine, len(lines))
				}
				return
			}
			h.wantCalls("Spawn", spawns+1)
			h.wantCalls("Resume", resumes)
			if c := h.claim(); c.Session != "" || c.SessionFile != "" || c.Abandoned.Session != session {
				t.Fatalf("the retried claim records session %q (%s) and abandoned %+v, want the recorded %q forgotten",
					c.Session, c.SessionFile, c.Abandoned, session)
			}
			h.wantRefusedRun(0)
			if len(lines) != 1 {
				t.Fatalf("the retry logged %q %d times, want once", newSessionLine, len(lines))
			}
			if why, ok := lines[0].attr("why"); !ok || why.String() != "the retry asked for a new session" {
				t.Fatalf("the line's why is %v, want the retry's own ask", why)
			}
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
// the daemon that saw all three would, which is what it answers the third report with.
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
	if p := h.pending(); !p.awaitsStart() || p.Number != number {
		t.Fatalf("after the restart the task is %+v, want task %d taken again and its turn not started", p, number)
	}

	h.repeatReport(first)
	if p := h.pending(); !p.awaitsStart() || p.ProviderErrorRetries != 1 || h.claim().ProviderErrorRun != 1 {
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
// for a provider error; reports k+1 to k+3, clean reports of turns no take started, leave the
// window holding them alone and end the run of provider errors, since a clean report ends it
// whoever ran the turn (providerErrorRunOver, dispatch://INFRA-4508); the ladder's wait gives the
// task again and the worker takes it. Report k sent again then changes nothing: its provider
// error is not counted again, the run stays ended, the take stands, and the window is as it was.
// So in the daemon that applied k, and in one restarted over what the store holds.
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
			if p := h.pending(); !p.awaitsStart() || p.Number != number {
				t.Fatalf("after the ladder's wait the task is %+v, want task %d taken again and its turn not started", p, number)
			}
			if restart {
				h.restart()
				h.connect()
			}
			window := h.claim().TurnOutcomes

			h.repeatReport(first)

			if p := h.pending(); !p.awaitsStart() || p.ProviderErrorRetries != 1 || h.claim().ProviderErrorRun != 0 {
				t.Fatalf("after report %d was sent again the task is %+v with a run of %d provider errors, want the take standing, one retry and the run the clean turns ended still at zero",
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
			if p := h.pending(); p.Number != number || !p.startRecorded() {
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
			if p := h.pending(); !p.startRecorded() || h.claim().ProviderErrorRun != 0 {
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
