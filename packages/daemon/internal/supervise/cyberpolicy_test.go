package supervise

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The cases of this file hold the daemon to one rule: after OpenAI refuses a session's content
// for cyber policy, the daemon gives that session nothing again until a person or the issue's
// architect decides it. Each task the worker takes starts a turn from input outside the session,
// which ends Oh My Pi's own hold, so the refused content would go to OpenAI again and add one more
// refusal to the organization's record.

// cyberRefusal is OpenAI's cyber-policy refusal as a current pi-legion names it beside a refused
// turn: the provider's failure, with whatever status a gateway relayed it under.
func cyberRefusal(status int) *ProviderError {
	return &ProviderError{Status: status, Provider: "openai", Model: "gpt-5.5", Message: "cyber_policy: synthetic refusal text"}
}

// cyberPluginTurn is a turn that was not the take's — a phase reminder, an Envoy message, a
// background job's result — that OpenAI refused for cyber policy, as a current pi-legion reports
// it: naming no task, with the provider's failure and the class the host gave the refusal.
func (h *harness) cyberPluginTurn(status int) {
	h.t.Helper()
	h.worker.sequence++
	h.must(RequestTurnOutcome{Claim: h.token, Generation: h.generation(), Sequence: h.worker.sequence, Refused: true,
		Message: "cyber_policy: synthetic refusal text", ProviderError: cyberRefusal(status), Code: codeCyberPolicy})
}

// cyberTakeTurn ends the turn of the take of the pending task and reports it refused for cyber
// policy, as a current pi-legion does: the end frame, and then the report naming the task's
// number with the provider's failure and the class.
func (h *harness) cyberTakeTurn(status int) error {
	h.t.Helper()
	number := h.pending().Number
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	h.worker.sequence++
	err := h.m.Handle(h.ctx, RequestTurnOutcome{Claim: h.token, Generation: h.generation(), Sequence: h.worker.sequence,
		Refused: true, Message: "cyber_policy: synthetic refusal text", Task: number, ProviderError: cyberRefusal(status),
		Code: codeCyberPolicy})
	h.m.Wait()
	h.workerAsks()
	return err
}

// wantHeldForCyberPolicy fails the case unless the claim failed under CauseCyberPolicy, with a
// sentence that names the class and none of the provider's own words.
func (h *harness) wantHeldForCyberPolicy() {
	h.t.Helper()
	h.wantState(StateFailed)
	c := h.claim()
	if c.FailureCause != CauseCyberPolicy {
		h.t.Fatalf("the failure's cause is %q, want %q", c.FailureCause, CauseCyberPolicy)
	}
	for _, want := range []string{"cannot clear", "cyber_policy"} {
		if !strings.Contains(c.Failure, want) {
			h.t.Errorf("the failure is %q, want it to name %q", c.Failure, want)
		}
	}
	if strings.Contains(c.Failure, "synthetic refusal text") {
		h.t.Errorf("the failure is %q, want none of the provider's own words", c.Failure)
	}
	if stored := h.store.load(h.token); stored.FailureCause != CauseCyberPolicy {
		h.t.Errorf("the stored claim carries cause %q, want %q persisted", stored.FailureCause, CauseCyberPolicy)
	}
}

// A turn that was not the take's and that OpenAI refused for cyber policy fails the claim under
// CauseCyberPolicy, whatever status the report carries, and nothing is given to the session again,
// however long the claim waits: not the task the daemon queued while that turn ran, which the
// worker would otherwise take at the turn's end (offerPending), and not a task delivered after it.
// The report is the daemon's one record of such a turn. Without the class on the report, a
// statusless or 503 refusal fails nothing, and the queued task is taken at the turn's end.
func TestAPluginTurnRefusedForCyberPolicyFailsTheClaimAndNothingIsTakenAgain(t *testing.T) {
	for _, status := range []int{0, 400, 503} {
		t.Run(fmt.Sprintf("HTTP %d", status), func(t *testing.T) {
			h := idleClaim(t)
			taken := len(h.prompts())
			// The plugin's own turn starts (a phase reminder), and the daemon queues a task while it
			// runs.
			h.must(StreamTurnStart{Claim: h.token})
			h.must(RequestDeliver{Claim: h.token, Task: providerErrorTaskText, Phase: phase.Implementing, Generation: 7, Entry: 1})

			h.cyberPluginTurn(status)
			h.mustQuiet(StreamTurnEnd{Claim: h.token})

			h.wantHeldForCyberPolicy()
			if c := h.claim(); c.Session != session || c.Pending == nil || c.Pending.Task != providerErrorTaskText {
				t.Errorf("the failed claim records session %q and holds %+v, want its session and the queued task kept for its architect",
					c.Session, c.Pending)
			}
			_ = h.handle(RequestDeliver{Claim: h.token, Task: "a later task", Phase: phase.Implementing, Generation: 7, Entry: 2})
			h.advance(2 * promptRetryMax)
			h.wantPrompts(taken)
		})
	}
}

// A take's report refused for cyber policy that the store took while it refused every write after
// it — the take-back and the claim's failure written nowhere — is decided again by a daemon
// restarted over what the store holds, from the class the report's entry in the window keeps
// (TurnOutcome.ProviderErrorCode, Machine.ladderOwed, takeToLadder): the take goes back and the
// claim fails under CauseCyberPolicy, once, and nothing is taken again. The control is the same
// report with no class, whose 503 the restart takes to the ladder.
func TestACyberPolicyReportWhoseTakeBackTheStoreRefusedFailsTheClaimAfterADaemonRestart(t *testing.T) {
	for _, coded := range []bool{true, false} {
		t.Run(map[bool]string{true: "with the class", false: "with no class, the control"}[coded], func(t *testing.T) {
			cut := &cutOffStore{takes: -1}
			h := workingClaim(t, currentPlugin, func(d *Deps) { cut.Store, d.Store = d.Store, cut })
			h.mustQuiet(StreamTurnEnd{Claim: h.token})
			number := h.pending().Number
			h.worker.sequence++
			report := RequestTurnOutcome{Claim: h.token, Generation: h.generation(), Sequence: h.worker.sequence, Refused: true,
				Message: "cyber_policy: synthetic refusal text", Task: number, ProviderError: cyberRefusal(503)}
			if coded {
				report.Code = codeCyberPolicy
			}
			cut.set(1)
			err := h.m.Handle(h.ctx, report)
			h.m.Wait()
			cut.set(-1)
			if !errors.Is(err, errBoom) {
				t.Fatalf("the report whose take-back the store refused = %v, want %v", err, errBoom)
			}
			stored := h.store.load(h.token)
			if kept := stored.TurnOutcomes; len(kept) != 1 || kept[0].Unapplied || kept[0].Task != number ||
				kept[0].ProviderErrorStatus != 503 || kept[0].ProviderErrorCode != report.Code {
				t.Fatalf("the store keeps %+v, want the report of task %d applied with its status and class %q", kept, number, report.Code)
			}
			if p := stored.Pending; p == nil || !p.taken() || stored.State == StateFailed {
				t.Fatalf("the store holds %s and %+v, want the take standing and the claim unfailed", stored.State, p)
			}

			h.restart()
			h.connect()
			h.wantLadderOwedLine(true)
			p := h.claim().Pending
			if !coded {
				if p == nil || p.taken() || p.ProviderErrorRetries != 1 || h.state() == StateFailed {
					t.Fatalf("the restarted claim is %s holding %+v, want the task on the ladder's first step", h.state(), p)
				}
				return
			}
			h.wantHeldForCyberPolicy()
			if p == nil || p.Number != number || p.taken() || p.ProviderErrorRetries != 0 {
				t.Fatalf("the restarted claim holds %+v, want task %d taken back with no ladder step", p, number)
			}
			h.advance(2 * promptRetryMax)
			h.wantPrompts(1)
		})
	}
}

// The plugin's report and the worker stream's turn end come from one agent_end on two channels,
// in no fixed order, and pi-legion's take hears the end before the report goes (its agent_end
// entry). Where the end reaches the daemon first and a task waits, the worker takes that task
// before the report arrives: one take, whose turn ends Oh My Pi's hold. That take is the bound.
// The report of its own turn names its number and cyber_policy, which takes the task back and
// fails the claim under CauseCyberPolicy, and the report of the first turn, arriving after it,
// changes nothing; nothing is taken a second time.
func TestARaceBetweenTheTurnsEndAndItsReportCostsOneTakeAtMost(t *testing.T) {
	h := idleClaim(t)
	taken := len(h.prompts())
	h.must(StreamTurnStart{Claim: h.token})
	h.must(RequestDeliver{Claim: h.token, Task: providerErrorTaskText, Phase: phase.Implementing, Generation: 7, Entry: 1})
	// The plugin's turn is the worker's next to report; its report is sent last, below.
	h.worker.sequence++
	late := h.worker.sequence

	// The stream's end of the refused plugin turn wins the race: the worker takes the queued task.
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	h.wantPrompts(taken + 1)
	h.must(StreamTurnStart{Claim: h.token})
	// The racing take ended Oh My Pi's hold, and OpenAI refused its turn again.
	if err := h.cyberTakeTurn(503); err != nil {
		t.Fatalf("report the take's turn: %v", err)
	}
	h.wantHeldForCyberPolicy()
	if p := h.claim().Pending; p == nil || p.taken() {
		t.Fatalf("the failed claim holds %+v, want the racing take's task kept with its take given back", p)
	}

	// The plugin's report of the first turn arrives late.
	_ = h.handle(RequestTurnOutcome{Claim: h.token, Generation: h.generation(), Sequence: late, Refused: true,
		Message: "cyber_policy: synthetic refusal text", ProviderError: cyberRefusal(0), Code: codeCyberPolicy})
	h.advance(2 * promptRetryMax)
	h.wantPrompts(taken + 1)
	h.wantHeldForCyberPolicy()
}

// failedForCyberPolicy is a phase worker failed for OpenAI's cyber-policy refusal of a turn that
// was not a take's.
func failedForCyberPolicy(t *testing.T) *harness {
	t.Helper()
	h := idleClaim(t)
	h.cyberPluginTurn(0)
	h.wantHeldForCyberPolicy()
	return h
}

// wantStartedInANewSession fails the case unless retry, admitted for a claim held for a
// cyber-policy refusal, launched the claim's agent again in a new session: one spawn and no
// resume, the recorded session abandoned under the failure's sentence, the run of provider-error
// turns started over, and the one line of a retry in a new session, saying why. A resume would
// hand the refused session the retry's first task, which ends Oh My Pi's hold and sends that
// content to OpenAI again.
func (h *harness) wantStartedInANewSession(retry Event) {
	h.t.Helper()
	resumes, spawns := len(h.calls("Resume")), len(h.calls("Spawn"))
	lines := len(h.records.of("supervise: retrying in a new session"))
	h.must(retry)
	h.wantState(StateLaunching)
	h.wantNoFailure()
	h.wantCalls("Spawn", spawns+1)
	h.wantCalls("Resume", resumes)
	c := h.claim()
	if c.Session != "" || c.SessionFile != "" || c.Abandoned.Session != session || !strings.Contains(c.Abandoned.Why, "cyber_policy") {
		h.t.Fatalf("the retried claim records session %q (%s) and abandoned %+v, want %q left behind under the failure's sentence",
			c.Session, c.SessionFile, c.Abandoned, session)
	}
	if c.ProviderErrorRun != 0 {
		h.t.Errorf("the retried claim keeps a run of %d provider-error turns, want it started over with the new session", c.ProviderErrorRun)
	}
	if got := h.records.of("supervise: retrying in a new session"); len(got) != lines+1 {
		h.t.Errorf("the retry logged %d lines of a retry in a new session, want one", len(got)-lines)
	}
}

// No retry the daemon makes by itself launches again the agent of a claim held for a cyber-policy
// refusal: the next start of its role, a re-admission of its tree, a production check's look, the
// controller keeper (RequestRetry with no decision behind it). Each is refused with
// ErrHeldForCyberPolicy, and the claim stays failed with its record, its session and its task. A
// retry someone decided launches the agent again: the held phase's retry, which the architect's
// retry_or_escalate, the operator's `legion claims retry` of a phase worker and a person's todo on
// a held root each start (RequestRetry.Decided), and the operator's own retry
// (RequestOperatorRetry). Each of those starts the agent in a new session, asked for or not: the
// recorded session would send the refused content again. A claim an operator stopped after it
// failed keeps the record, so a retired one is held the same way.
func TestOnlyADecidedRetryLaunchesTheAgentOfAHeldClaimAgain(t *testing.T) {
	refused := func(t *testing.T, h *harness, from string) {
		t.Helper()
		before, calls, sent := h.claim(), len(h.rt.Calls()), len(h.prompts())
		err := h.handle(RequestRetry{Claim: h.token})
		var refusal *RefusedError
		if !errors.Is(err, ErrHeldForCyberPolicy) || !errors.As(err, &refusal) || refusal.State != before.State {
			t.Fatalf("the daemon's own retry of a claim %s returned %v, want ErrHeldForCyberPolicy", from, err)
		}
		if want := "`" + RetryCommand(h.token) + "`"; !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "--fresh-session") {
			t.Errorf("the refusal %q names no way back, want the operator's %s and no flag it does not need", err, want)
		}
		if after := h.claim(); after.State != before.State || after.FailureCause != CauseCyberPolicy ||
			after.Session != before.Session || after.Generation != before.Generation {
			t.Errorf("the refused retry moved the claim: %+v -> %+v", before, after)
		}
		if got := h.rt.Calls()[calls:]; len(got) != 0 {
			t.Errorf("the refused retry asked the runtime %+v", got)
		}
		h.advance(2 * promptRetryMax)
		h.wantPrompts(sent)
	}
	stopped := func(t *testing.T) *harness {
		t.Helper()
		h := failedForCyberPolicy(t)
		h.must(RequestStop{Claim: h.token})
		h.wantState(StateRetired)
		return h
	}

	t.Run("failed", func(t *testing.T) {
		refused(t, failedForCyberPolicy(t), "failed for cyber policy")
	})
	t.Run("retired after it failed", func(t *testing.T) {
		refused(t, stopped(t), "an operator stopped after it failed for cyber policy")
	})
	for _, tc := range []struct {
		name  string
		claim func(*testing.T) *harness
		retry Event
	}{
		{"the held phase's retry", failedForCyberPolicy, RequestRetry{Claim: testToken, Decided: true}},
		{"the held phase's retry asking for a new session", failedForCyberPolicy, RequestRetry{Claim: testToken, Decided: true, FreshSession: true}},
		{"the operator's retry", failedForCyberPolicy, RequestOperatorRetry{Claim: testToken}},
		{"the operator's retry in a new session", failedForCyberPolicy, RequestOperatorRetry{Claim: testToken, FreshSession: true}},
		{"the operator's retry of a claim it stopped after it failed", stopped, RequestOperatorRetry{Claim: testToken}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.claim(t).wantStartedInANewSession(tc.retry)
		})
	}
	// A claim held for any other cause is retried by the daemon as before, on its recorded
	// session: the control that the new session above is the hold's and no retry's own.
	t.Run("a provider error that cannot clear", func(t *testing.T) {
		h := idleClaim(t)
		h.pluginTurn(1, true, 400)
		h.wantState(StateFailed)
		resumes := len(h.calls("Resume"))
		h.must(RequestRetry{Claim: h.token})
		h.wantState(StateLaunching)
		h.wantCalls("Resume", resumes+1)
		if c := h.claim(); c.Session != session || c.Abandoned.Session != "" {
			t.Fatalf("the retried claim records session %q and abandoned %+v, want %q resumed", c.Session, c.Abandoned, session)
		}
	})
}

// The daemon's controller is retried by its keeper alone, so the operator's retry of it is refused
// (TestTheOperatorsRetryOfTheControllerIsRefused). A controller held for a cyber-policy refusal is
// the exception: its keeper retries it no more (Claim.KeeperRetries), so the operator's retry is
// the one way back, and it is admitted, in a new session whether it asks for one or not.
func TestTheOperatorsRetryStartsAHeldControllerInANewSession(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		t.Run(fmt.Sprintf("--fresh-session %t", fresh), func(t *testing.T) {
			h := newHarnessOf(t, controllerClaim())
			h.reach(StateIdle)
			h.cyberPluginTurn(0)
			h.wantHeldForCyberPolicy()
			if h.claim().KeeperRetries() {
				t.Fatal("the held controller reads as one its keeper retries")
			}

			if err := h.handle(RequestRetry{Claim: h.token}); !errors.Is(err, ErrHeldForCyberPolicy) {
				t.Fatalf("the keeper's retry of the held controller returned %v, want ErrHeldForCyberPolicy", err)
			}
			h.wantStartedInANewSession(RequestOperatorRetry{Claim: h.token, FreshSession: fresh})
			if c := h.claim(); c.Role != claim.RoleController || !c.KeeperRetries() {
				t.Fatalf("the retried controller is %+v, want it the keeper's again", c)
			}
		})
	}
}

// A retry that leaves the recorded session behind does so only once the claim is admitted
// (retry, revive's admitted): an admission the tree lifecycle refuses — its cleanup is reserved —
// leaves the claim exactly as it was, its session, its file, its run of provider-error turns and
// its failure record kept on the live machine, which `legion claims list` reads, and nothing
// abandoned or logged as a retry in a new session. Every decided retry of a claim held for a
// cyber-policy refusal takes that road, and so does the operator's --fresh-session retry of any
// claim. Once the reservation is gone, the same retry leaves the session behind and launches the
// agent in a new one, which is the control: the claim is one the retry does reach.
func TestARefusedAdmissionLeavesTheSessionARetryInANewSessionWouldLeaveBehind(t *testing.T) {
	held := func() Claim {
		c := retiredOfEpoch(1)
		c.State, c.FailureCause, c.ProviderErrorRun = StateFailed, CauseCyberPolicy, 3
		c.Failure = "a provider error that cannot clear ended this claim's turn: OpenAI's cyber_policy refusal of this session's content"
		return c
	}
	stopped := retiredOfEpoch(1)
	stopped.ProviderErrorRun = 3
	for _, tc := range []struct {
		name  string
		claim Claim
		retry func(claim.Token) Event
	}{
		{"the held phase's retry of a claim held for cyber policy", held(),
			func(token claim.Token) Event { return RequestRetry{Claim: token, Decided: true} }},
		{"the operator's retry of a claim held for cyber policy", held(),
			func(token claim.Token) Event { return RequestOperatorRetry{Claim: token} }},
		{"the operator's --fresh-session retry of a stopped claim", stopped,
			func(token claim.Token) Event { return RequestOperatorRetry{Claim: token, FreshSession: true} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &epochStore{epoch: 1, reserved: true}
			h := lifecycleHarness(t, store, tc.claim)
			before := h.m.Claim()
			lines := len(h.records.of("supervise: retrying in a new session"))

			if err := h.handle(tc.retry(h.token)); !errors.Is(err, treelifecycle.ErrCleanupReserved) {
				t.Fatalf("the retry during a reserved cleanup = %v, want the reservation's wait", err)
			}
			after := h.m.Claim()
			if after.Session != before.Session || after.SessionFile != before.SessionFile || after.Abandoned.Session != "" ||
				after.ProviderErrorRun != before.ProviderErrorRun || after.FailureCause != before.FailureCause || after.State != before.State {
				t.Fatalf("the refused retry left the live claim %s with session %q (%s), abandoned %q, %d provider-error turns and cause %q; want it as it was: %s, %q (%s), nothing abandoned, %d, %q",
					after.State, after.Session, after.SessionFile, after.Abandoned.Session, after.ProviderErrorRun, after.FailureCause,
					before.State, before.Session, before.SessionFile, before.ProviderErrorRun, before.FailureCause)
			}
			if got := h.records.of("supervise: retrying in a new session"); len(got) != lines {
				t.Errorf("the refused retry logged %d lines of a retry in a new session, want none", len(got)-lines)
			}
			if calls := len(h.rt.CallsOf("Resume")) + len(h.rt.CallsOf("Spawn")); calls != 0 {
				t.Fatalf("the refused retry started %d launches, want none", calls)
			}

			store.reserved = false
			h.must(tc.retry(h.token))
			if c := h.m.Claim(); c.State != StateLaunching || c.Session != "" || c.Abandoned.Session != before.Session || c.ProviderErrorRun != 0 {
				t.Fatalf("the admitted retry left the claim %s with session %q, abandoned %q and %d provider-error turns; want it launching in a new session, %q abandoned, the run at 0",
					c.State, c.Session, c.Abandoned.Session, c.ProviderErrorRun, before.Session)
			}
			if spawns := len(h.rt.CallsOf("Spawn")); spawns != 1 || len(h.rt.CallsOf("Resume")) != 0 {
				t.Fatalf("the admitted retry made %d spawns and %d resumes, want one spawn", spawns, len(h.rt.CallsOf("Resume")))
			}
		})
	}
}

// The one decoder of a stored cause and its held_for (DecodeCause), which the claim store and the
// record store's role-claim read both use: held_for counts only beside the `provider_error` it was
// written with, so a cause a release before migration 20261125223008_claim_held_for rewrote after
// a rollback reads as that release wrote it, and an empty held_for leaves every word as it is.
func TestAStoredCauseReadsAsCyberPolicyOnlyBesideItsProviderError(t *testing.T) {
	for _, tc := range []struct {
		column, heldFor string
		want            FailureCause
	}{
		{"provider_error", "cyber_policy", CauseCyberPolicy},
		{"provider_error", "", CauseProviderError},
		{"died", "cyber_policy", CauseDied},
		{"", "cyber_policy", ""},
		{"never_registered", "", CauseNeverRegistered},
	} {
		if got := DecodeCause(tc.column, tc.heldFor); got != tc.want {
			t.Errorf("DecodeCause(%q, %q) = %q, want %q", tc.column, tc.heldFor, got, tc.want)
		}
	}
}

// One classification decides both what a provider error is called and whether it fails the claim
// (classifyProviderError), so the two cannot drift for a class. A cyber-policy refusal fails under
// its own word whatever its status; a client error that is none of 401, 403, 408 and 429 fails
// under provider_error, and every other error clears. No other class is read: a turn-outcome
// report carries cyber_policy or nothing (the route takes any other word as none), so a word such
// as Oh My Pi's `cost_cap` is judged by its status alone.
func TestOneClassificationNamesAProviderErrorAndDecidesItsFailure(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		cause  string
		fails  FailureCause
	}{
		{0, codeCyberPolicy, "OpenAI's cyber_policy refusal of this session's content", CauseCyberPolicy},
		{503, codeCyberPolicy, "OpenAI's cyber_policy refusal of this session's content (HTTP 503)", CauseCyberPolicy},
		{0, "cost_cap", "a provider error", ""},
		{400, "cost_cap", "a provider error (HTTP 400)", CauseProviderError},
		{400, "", "a provider error (HTTP 400)", CauseProviderError},
		{429, "", "a provider error (HTTP 429)", ""},
		{401, "", "a provider error (HTTP 401)", ""},
		{529, "", "a provider error (HTTP 529)", ""},
		{0, "", "a provider error", ""},
	} {
		if got := classifyProviderError(tc.status, tc.code); got != (providerErrorClass{cause: tc.cause, fails: tc.fails}) {
			t.Errorf("classifyProviderError(%d, %q) = %+v, want cause %q failing under %q", tc.status, tc.code, got, tc.cause, tc.fails)
		}
	}
}
