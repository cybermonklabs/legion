package supervise

import (
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// unknownOutcome is a value of the type the set does not hold.
const unknownOutcome runtime.Outcome = "a word no release of the daemon has minted"

// The rows of the charge policy: whether each member of the set charges the budget its site counts.
// Every member listed by Outcomes must have a row here, so a member added without a decision
// fails this test by name.
func TestChargesDecidesEveryOutcome(t *testing.T) {
	want := map[runtime.Outcome]bool{
		runtime.OutcomeUnrecognised:               true,
		runtime.OutcomeAdoptionRefused:            true,
		runtime.OutcomeIdentityUnavailable:        false,
		runtime.OutcomeTransportLost:              false,
		runtime.OutcomeNoTurnStarted:              true,
		runtime.OutcomeProviderError:              false,
		runtime.OutcomeProcessDied:                true,
		runtime.OutcomeLaunchFailed:               true,
		runtime.OutcomeNeverRegistered:            true,
		runtime.OutcomeNeverReconnected:           true,
		runtime.OutcomeStalePlugin:                true,
		runtime.OutcomeEarlierReleaseProcessEnded: false,
		runtime.OutcomeResumeByAgentUnregistered:  false,
	}
	for _, o := range runtime.Outcomes() {
		charges, decided := want[o]
		if !decided {
			t.Errorf("outcome %q has no row in this test: decide whether it charges", o)
			continue
		}
		if got := Charges(o); got != charges {
			t.Errorf("Charges(%q) = %t, want %t", o, got, charges)
		}
	}
	if len(want) != len(runtime.Outcomes()) {
		t.Errorf("this test decides %d outcomes and Outcomes lists %d", len(want), len(runtime.Outcomes()))
	}
}

// An outcome the policy does not recognise is charged: a policy that waived what it cannot name
// would make a crash-looping process immortal. The zero value is one, and so is a word no member
// holds.
func TestAnOutcomeThePolicyDoesNotKnowIsCharged(t *testing.T) {
	if slices.Contains(runtime.Outcomes(), unknownOutcome) {
		t.Fatalf("%q is a member of Outcomes, want a value outside the set", unknownOutcome)
	}
	for _, o := range []runtime.Outcome{"", unknownOutcome} {
		if !Charges(o) {
			t.Errorf("Charges(%q) = false, want an unknown outcome charged", o)
		}
	}

	// And the prompt budget charges a failure named under it, as it charges a refusal.
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
	h.m.mu.Lock()
	err := h.m.chargePrompt(h.ctx, unknownOutcome, WaitTake, "a failure named by no member")
	h.m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	h.wantBudgets(Budgets{PromptFailures: 1})
}

// The prompt budget asks the policy: a failure named under each outcome charges one prompt failure
// exactly where Charges says it does, and nothing where it says not. The site names the outcome
// (refusedAdoption, the two waits of R5, the report's provider error) and decides nothing; the
// state is held open here — a ready claim holding a task, short of the limit — so what varies is
// the outcome alone.
func TestThePromptBudgetChargesWhatTheOutcomePolicySays(t *testing.T) {
	for _, o := range append(runtime.Outcomes(), unknownOutcome) {
		t.Run(string(o), func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateReady)
			h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
			h.m.mu.Lock()
			err := h.m.chargePrompt(h.ctx, o, WaitTurnStart, "named "+string(o))
			h.m.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			want := Budgets{}
			if Charges(o) {
				want.PromptFailures = 1
			}
			h.wantBudgets(want)
			h.wantState(StateReady)
		})
	}
}

// The death budget asks the policy: a death with work outstanding counts one death exactly where
// Charges says the outcome charges. The state guard is the function's own and is held open here —
// a ready claim holding a task — so what varies is the outcome alone.
func TestTheDeathBudgetChargesWhatTheOutcomePolicySays(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
	for _, o := range append(runtime.Outcomes(), unknownOutcome) {
		h.m.mu.Lock()
		before := h.m.claim.Budgets.Deaths
		h.m.claim.Budgets.Deaths = 0
		spent := h.m.chargeDeath(o)
		charged := h.m.claim.Budgets.Deaths
		h.m.claim.Budgets.Deaths = before
		h.m.mu.Unlock()
		want := 0
		if Charges(o) {
			want = 1
		}
		if charged != want || spent {
			t.Errorf("chargeDeath(%q) counted %d deaths (spent %t), want %d and not spent", o, charged, spent, want)
		}
	}
}

// The launch budget asks the policy: a launch failure counts exactly where Charges says the
// outcome charges, for both sites that count it (relaunchAfterFailure and launchTries, through
// chargeLaunch).
func TestTheLaunchBudgetChargesWhatTheOutcomePolicySays(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	for _, o := range append(runtime.Outcomes(), unknownOutcome) {
		h.m.mu.Lock()
		h.m.claim.Budgets.LaunchFailures = 0
		spent := h.m.chargeLaunch(o)
		charged := h.m.claim.Budgets.LaunchFailures
		h.m.claim.Budgets.LaunchFailures = 0
		h.m.mu.Unlock()
		want := 0
		if Charges(o) {
			want = 1
		}
		if charged != want || spent {
			t.Errorf("chargeLaunch(%q) counted %d launch failures (spent %t), want %d and not spent", o, charged, spent, want)
		}
	}
}
