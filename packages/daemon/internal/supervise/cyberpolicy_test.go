package supervise

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The cases of this file hold the daemon to one rule: after OpenAI refuses a session's content
// for cyber policy, the daemon sends that session nothing again until a person or the issue's
// architect decides it. Each prompt the daemon sends is input from outside the session, which
// ends Oh My Pi's own hold, so the refused content would go to OpenAI again and add one more
// refusal to the organization's record.

// cyberPluginTurn is a turn that ran no prompt of the daemon's — a phase reminder, an Envoy
// message, a background job's result — that OpenAI refused for cyber policy, as pi-legion
// reports it: unprompted, with the class the host gave the refusal and whatever status a
// gateway relayed it under.
func (h *harness) cyberPluginTurn(sequence uint64, status int) {
	h.t.Helper()
	h.must(RequestTurnOutcome{Claim: h.token, Generation: h.generation(), Sequence: sequence, Refused: true,
		Message: "cyber_policy: synthetic refusal text", HTTPStatus: status, Unprompted: true, Code: codeCyberPolicy})
}

// wantHeldForCyberPolicy fails the case unless the claim failed under CauseCyberPolicy, with a
// sentence that names the class and none of the provider's own words, and recorded its session as
// one whose content OpenAI refused (Claim.CyberPolicySessions), in memory and in the store.
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
	if want := []string{c.Session}; c.Session == "" || !slices.Equal(c.CyberPolicySessions, want) ||
		!slices.Equal(h.store.load(h.token).CyberPolicySessions, want) {
		h.t.Errorf("the held claim records the refused sessions %q and the store %q, want its session %q in both",
			c.CyberPolicySessions, h.store.load(h.token).CyberPolicySessions, c.Session)
	}
}

// A turn that ran no prompt of the daemon's and that OpenAI refused for cyber policy fails the
// claim under CauseCyberPolicy, whatever status the report carries, and nothing is sent to the
// session again, however long the claim waits: not the task the daemon queued while that turn
// ran, which the end of the turn would otherwise send (turnEnded, afterTurn, sendPending), and
// not a task delivered after it. The report is the daemon's one record of such a turn, since no
// prompt result comes for it. Without the class on the report, a statusless or 503 refusal fails
// nothing, and the queued task goes at the turn's end.
func TestAPluginTurnRefusedForCyberPolicyFailsTheClaimAndNothingIsSentAgain(t *testing.T) {
	for _, status := range []int{0, 400, 503} {
		t.Run(fmt.Sprintf("HTTP %d", status), func(t *testing.T) {
			h := idleClaim(t)
			sent := len(h.prompts())
			// The plugin's own turn starts (a phase reminder), and the daemon queues a task while it
			// runs.
			h.must(StreamTurnStart{Claim: h.token})
			h.must(RequestDeliver{Claim: h.token, Task: providerErrorTaskText, Phase: phase.Implementing, Generation: 7, Entry: 1})

			h.cyberPluginTurn(1, status)
			h.mustQuiet(StreamTurnEnd{Claim: h.token})

			h.wantHeldForCyberPolicy()
			if c := h.claim(); c.Session != session || c.Pending == nil || c.Pending.Task != providerErrorTaskText {
				t.Errorf("the failed claim records session %q and holds %+v, want its session and the queued task kept for its architect",
					c.Session, c.Pending)
			}
			_ = h.handle(RequestDeliver{Claim: h.token, Task: "a later task", Phase: phase.Implementing, Generation: 7, Entry: 2})
			h.advance(2 * promptRetryMax)
			h.wantPrompts(sent)
		})
	}
}

// The plugin's report and the worker stream's turn end come from one agent_end on two channels,
// in no fixed order. Where the turn's end reaches the daemon first and a task waits, that task is
// sent before the report arrives: one prompt, which ends Oh My Pi's hold. That send is the bound.
// The prompt's own result names cyber_policy, which fails the claim under CauseCyberPolicy, and
// the report that arrives after it changes nothing; nothing is sent a second time.
func TestARaceBetweenTheTurnsEndAndItsReportCostsOneSendAtMost(t *testing.T) {
	h := idleClaim(t)
	sent := len(h.prompts())
	h.must(StreamTurnStart{Claim: h.token})
	h.must(RequestDeliver{Claim: h.token, Task: providerErrorTaskText, Phase: phase.Implementing, Generation: 7, Entry: 1})

	// The stream's end of the refused plugin turn wins the race: the queued task goes.
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	h.wantPrompts(sent + 1)
	h.must(StreamTurnStart{Claim: h.token})
	// The racing prompt ended Oh My Pi's hold, and OpenAI refused it again.
	h.errorTurn(StreamPromptResult{Error: "cyber_policy: synthetic refusal text", Provider: "openai", Model: "gpt-5.5", Code: codeCyberPolicy})
	h.wantHeldForCyberPolicy()

	// The plugin's report of the first turn arrives late.
	_ = h.handle(RequestTurnOutcome{Claim: h.token, Generation: h.generation(), Sequence: 1, Refused: true,
		Message: "cyber_policy: synthetic refusal text", Unprompted: true, Code: codeCyberPolicy})
	h.advance(2 * promptRetryMax)
	h.wantPrompts(sent + 1)
	h.wantHeldForCyberPolicy()
}

// failedForCyberPolicy is a phase worker failed for OpenAI's cyber-policy refusal of a turn that
// ran no prompt of the daemon's.
func failedForCyberPolicy(t *testing.T) *harness {
	t.Helper()
	h := idleClaim(t)
	h.cyberPluginTurn(1, 0)
	h.wantHeldForCyberPolicy()
	return h
}

// wantStartedInANewSession fails the case unless retry, admitted for a claim held for a
// cyber-policy refusal, launched the claim's agent again in a new session: one spawn and no
// resume, the recorded session abandoned under the failure's sentence, the run of provider-error
// turns started over, and the one line of a retry in a new session, saying why. A resume would
// hand the refused session the retry's first task, which ends Oh My Pi's hold and sends that
// content to OpenAI again. The claim is held no more, but the session it left behind stays
// recorded as refused, in memory and in the store, so its conversation is still answered by no
// model (Claim.SessionHeldForCyberPolicy).
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
	if want := []string{session}; c.HeldForCyberPolicy() || !c.SessionHeldForCyberPolicy(session) || !slices.Equal(c.CyberPolicySessions, want) ||
		!slices.Equal(h.store.load(h.token).CyberPolicySessions, want) {
		h.t.Errorf("the retried claim is held %t, its refused sessions %q and the store's %q; want it held no more and %q still refused in both",
			c.HeldForCyberPolicy(), c.CyberPolicySessions, h.store.load(h.token).CyberPolicySessions, session)
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
// a held root each start (RequestRetry.Decided), and the retry the operator or the controller asks
// (RequestAskedRetry). Each of those starts the agent in a new session, asked for or not: the
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
		h.must(RequestStop{Claim: h.token, By: SuspendedByOperator})
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
		{"the operator's retry", failedForCyberPolicy, RequestAskedRetry{Claim: testToken, By: SuspendedByOperator}},
		{"the operator's retry in a new session", failedForCyberPolicy, RequestAskedRetry{Claim: testToken, FreshSession: true, By: SuspendedByOperator}},
		{"the controller's retry", failedForCyberPolicy, RequestAskedRetry{Claim: testToken, By: SuspendedByController}},
		{"the operator's retry of a claim it stopped after it failed", stopped, RequestAskedRetry{Claim: testToken, By: SuspendedByOperator}},
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
		if c := h.claim(); c.Session != session || c.Abandoned.Session != "" || len(c.CyberPolicySessions) != 0 {
			t.Fatalf("the retried claim records session %q, abandoned %+v and refused sessions %q, want %q resumed and none refused",
				c.Session, c.Abandoned, c.CyberPolicySessions, session)
		}
	})
}

// A claim the release before this one failed under OpenAI's cyber-policy refusal, which that
// release records nowhere, and whose session that release's forget-session then left behind under
// the asker's sentence (forgottenBy), stays held with no session: nothing names the refused session
// but the claim's abandoned-session record. The decided retry that ends the hold records that
// session as refused (holdCyberPolicySession), in memory and in the store, so its conversation
// stays held once the hold has ended (Claim.SessionHeldForCyberPolicy). The control is a session
// the claim abandoned before it failed, which was not the one refused: the retry records none.
func TestADecidedRetryRecordsTheRefusedSessionAnEarlierForgetLeftBehind(t *testing.T) {
	failedAt := epoch.Add(-time.Hour)
	forgotten, err := forgottenBy(SuspendedByOperator)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		abandonedAt time.Time
		want        []string
	}{
		{"the session forgotten after the failure", failedAt.Add(time.Minute), []string{session}},
		{"a session abandoned before the failure", failedAt.Add(-time.Minute), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := queuedClaim()
			c.State, c.Generation, c.FailureCause, c.FailedAt = StateFailed, 3, CauseCyberPolicy, failedAt
			c.Failure = "a provider error that cannot clear ended this claim's turn: OpenAI's cyber_policy refusal of this session's content"
			c.Abandoned = AbandonedSession{Session: session, SessionFile: sessionFile, Why: forgotten, At: tc.abandonedAt}
			h := newHarnessOf(t, c)
			if held := h.claim(); !held.HeldForCyberPolicy() || held.Session != "" || len(held.CyberPolicySessions) != 0 {
				t.Fatalf("the claim is held %t on session %q with the refused sessions %q, want it held on none and none recorded",
					held.HeldForCyberPolicy(), held.Session, held.CyberPolicySessions)
			}

			h.must(RequestRetry{Claim: h.token, Decided: true})
			h.wantState(StateLaunching)
			retried := h.claim()
			if stored := h.store.load(h.token).CyberPolicySessions; retried.HeldForCyberPolicy() ||
				!slices.Equal(retried.CyberPolicySessions, tc.want) || !slices.Equal(stored, tc.want) {
				t.Fatalf("the retried claim is held %t with the refused sessions %q and the store's %q; want it held no more and %q in both",
					retried.HeldForCyberPolicy(), retried.CyberPolicySessions, stored, tc.want)
			}
			if held := retried.SessionHeldForCyberPolicy(session); held != (tc.want != nil) {
				t.Fatalf("the conversation of %s is held %t after the retry, want %t", session, held, tc.want != nil)
			}
		})
	}
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
			h.cyberPluginTurn(1, 0)
			h.wantHeldForCyberPolicy()
			if h.claim().KeeperRetries() {
				t.Fatal("the held controller reads as one its keeper retries")
			}

			if err := h.handle(RequestRetry{Claim: h.token}); !errors.Is(err, ErrHeldForCyberPolicy) {
				t.Fatalf("the keeper's retry of the held controller returned %v, want ErrHeldForCyberPolicy", err)
			}
			h.wantStartedInANewSession(RequestAskedRetry{Claim: h.token, FreshSession: fresh, By: SuspendedByOperator})
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
// agent in a new one, which is the control: the claim is one the retry does reach. The held claim
// records no refused session, as one a release that kept no such record failed does: the
// admitted retry records the session it leaves behind as refused (Claim.CyberPolicySessions), so
// the session's conversation stays held after the hold ends, and the refused admission records
// nothing, while the stopped claim, which no refusal holds, records none either way.
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
			func(token claim.Token) Event { return RequestAskedRetry{Claim: token, By: SuspendedByOperator} }},
		{"the operator's --fresh-session retry of a stopped claim", stopped,
			func(token claim.Token) Event {
				return RequestAskedRetry{Claim: token, FreshSession: true, By: SuspendedByOperator}
			}},
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
				after.ProviderErrorRun != before.ProviderErrorRun || after.FailureCause != before.FailureCause || after.State != before.State ||
				len(after.CyberPolicySessions) != 0 {
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
			var refused []string
			if before.HeldForCyberPolicy() {
				refused = []string{before.Session}
			}
			if got := h.m.Claim().CyberPolicySessions; !slices.Equal(got, refused) {
				t.Fatalf("the admitted retry left the refused sessions %q, want %q", got, refused)
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
// under provider_error, a cost cap among them, and every other error clears.
func TestOneClassificationNamesAProviderErrorAndDecidesItsFailure(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		cause  string
		fails  FailureCause
	}{
		{0, codeCyberPolicy, "OpenAI's cyber_policy refusal of this session's content", CauseCyberPolicy},
		{503, codeCyberPolicy, "OpenAI's cyber_policy refusal of this session's content (HTTP 503)", CauseCyberPolicy},
		{0, codeCostCap, "the session's cost cap (Oh My Pi's session.maxCost, cost_cap)", ""},
		{400, codeCostCap, "the session's cost cap (Oh My Pi's session.maxCost, cost_cap)", CauseProviderError},
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
