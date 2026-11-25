package supervise

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
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
// ran no prompt of the daemon's, and how many launches the runtime had been asked for by then.
func failedForCyberPolicy(t *testing.T) (*harness, int) {
	t.Helper()
	h := idleClaim(t)
	h.cyberPluginTurn(1, 0)
	h.wantHeldForCyberPolicy()
	return h, len(h.calls("Resume")) + len(h.calls("Spawn"))
}

// No retry the daemon makes by itself relaunches a claim held for a cyber-policy refusal: the
// next start of its role, a re-admission of its tree, a production check's look, the controller
// keeper (RequestRetry with no decision behind it). Each is refused with ErrHeldForCyberPolicy,
// and the claim stays failed with its record, its session and its task. A retry someone decided
// relaunches it: the held phase's retry, which the architect's retry_or_escalate, the operator's
// `legion claims retry` of a phase worker and a person's todo on a held root each start
// (RequestRetry.Decided), and the operator's own retry (RequestOperatorRetry). A claim an
// operator stopped after it failed keeps the record, so a retired one is held the same way.
func TestOnlyADecidedRetryRelaunchesAClaimHeldForCyberPolicy(t *testing.T) {
	refused := func(t *testing.T, h *harness, from string) {
		t.Helper()
		before, calls, sent := h.claim(), len(h.rt.Calls()), len(h.prompts())
		err := h.handle(RequestRetry{Claim: h.token})
		var refusal *RefusedError
		if !errors.Is(err, ErrHeldForCyberPolicy) || !errors.As(err, &refusal) || refusal.State != before.State {
			t.Fatalf("the daemon's own retry of a claim %s returned %v, want ErrHeldForCyberPolicy", from, err)
		}
		if !strings.Contains(err.Error(), "legion claims retry") {
			t.Errorf("the refusal %q names no way back, want the operator's `legion claims retry`", err)
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

	t.Run("failed", func(t *testing.T) {
		h, _ := failedForCyberPolicy(t)
		refused(t, h, "failed for cyber policy")
	})
	t.Run("retired after it failed", func(t *testing.T) {
		h, _ := failedForCyberPolicy(t)
		h.must(RequestStop{Claim: h.token})
		h.wantState(StateRetired)
		refused(t, h, "an operator stopped after it failed for cyber policy")
	})
	for _, tc := range []struct {
		name  string
		retry Event
	}{
		{"the held phase's retry", RequestRetry{Claim: testToken, Decided: true}},
		{"the operator's retry", RequestOperatorRetry{Claim: testToken}},
		{"the operator's retry in a new session", RequestOperatorRetry{Claim: testToken, FreshSession: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, launches := failedForCyberPolicy(t)
			h.must(tc.retry)
			h.wantState(StateLaunching)
			h.wantNoFailure()
			if got := len(h.calls("Resume")) + len(h.calls("Spawn")); got != launches+1 {
				t.Fatalf("the decided retry made %d launches, want one", got-launches)
			}
		})
	}
	// A claim held for any other cause is retried by the daemon as before.
	t.Run("a provider error that cannot clear", func(t *testing.T) {
		h := idleClaim(t)
		h.pluginTurn(1, true, 400)
		h.wantState(StateFailed)
		h.must(RequestRetry{Claim: h.token})
		h.wantState(StateLaunching)
	})
}

// The daemon's controller is retried by its keeper alone, so the operator's retry of it is refused
// (TestTheOperatorsRetryOfTheControllerIsRefused). A controller held for a cyber-policy refusal is
// the exception: its keeper retries it no more, so the operator's retry is the one way back, and
// it is admitted.
func TestTheOperatorsRetryRelaunchesAControllerHeldForCyberPolicy(t *testing.T) {
	h := newHarnessOf(t, controllerClaim())
	h.reach(StateIdle)
	h.cyberPluginTurn(1, 0)
	h.wantHeldForCyberPolicy()

	if err := h.handle(RequestRetry{Claim: h.token}); !errors.Is(err, ErrHeldForCyberPolicy) {
		t.Fatalf("the keeper's retry of the held controller returned %v, want ErrHeldForCyberPolicy", err)
	}
	h.must(RequestOperatorRetry{Claim: h.token, FreshSession: true})
	h.wantState(StateLaunching)
	if c := h.claim(); c.Role != claim.RoleController || c.FailureCause != "" {
		t.Fatalf("the retried controller is %+v, want it launching with no failure record", c)
	}
}
