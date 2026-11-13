package supervise

import (
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// busyWords is Oh My Pi's busy answer as it words it. The words decide nothing here: the
// supervisor reads the outcome the stream minted from them (stream.PromptRefusalOutcome), and the
// words ride along for the log, as the stream's own events carry them.
const busyWords = "Agent is already processing. Use steer() or followUp() to queue messages, or wait for completion."

// mintedRefusal is a prompt refusal carrying the outcome the stream minted for it, as
// stream.RefusedError carries it: runtime.ErrPromptRefused with its words (runtime.RefusalReason)
// and its member (sendOutcome). runtime/fake's refusal carries words and no member, which
// sendOutcome reads as OutcomeUnrecognised.
type mintedRefusal struct {
	outcome Outcome
	reason  string
}

func (r mintedRefusal) Error() string           { return r.reason + ": " + runtime.ErrPromptRefused.Error() }
func (r mintedRefusal) Is(target error) bool    { return target == runtime.ErrPromptRefused }
func (r mintedRefusal) RefusalReason() string   { return r.reason }
func (r mintedRefusal) RefusalOutcome() Outcome { return r.outcome }
func busyRefusal() mintedRefusal                { return mintedRefusal{outcome: OutcomeAgentBusy, reason: busyWords} }
func outcomeRefusal(o Outcome) mintedRefusal {
	return mintedRefusal{outcome: o, reason: "refused as " + string(o)}
}

// unknownOutcome is a value of the type the set does not hold.
const unknownOutcome Outcome = "a word no release of the daemon has minted"

// The rows of the charge policy: whether each member of the set charges the budget its site counts.
// Every member listed by Outcomes must have a row here, so a member added without a decision
// fails this test by name.
func TestChargesDecidesEveryOutcome(t *testing.T) {
	want := map[Outcome]bool{
		OutcomeUnrecognised:        true,
		OutcomeAgentBusy:           false,
		OutcomePromptRefused:       true,
		OutcomeAdoptionRefused:     true,
		OutcomeIdentityUnavailable: false,
		OutcomeTransportLost:       false,
		OutcomeNoTurnStarted:       true,
		OutcomeNoTurnRun:           true,
		OutcomeProviderError:       false,
		OutcomeProcessDied:         true,
		OutcomeLaunchFailed:        true,
		OutcomeNeverRegistered:     true,
		OutcomeStalePlugin:         true,
	}
	for _, o := range Outcomes() {
		charges, decided := want[o]
		if !decided {
			t.Errorf("outcome %q has no row in this test: decide whether it charges", o)
			continue
		}
		if got := Charges(o); got != charges {
			t.Errorf("Charges(%q) = %t, want %t", o, got, charges)
		}
	}
	if len(want) != len(Outcomes()) {
		t.Errorf("this test decides %d outcomes and Outcomes lists %d", len(want), len(Outcomes()))
	}
}

// An outcome the policy does not recognise is charged: a policy that waived what it cannot name
// would make a crash-looping process immortal. The zero value is one, and so is a word no member
// holds.
func TestAnOutcomeThePolicyDoesNotKnowIsCharged(t *testing.T) {
	if slices.Contains(Outcomes(), unknownOutcome) {
		t.Fatalf("%q is a member of Outcomes, want a value outside the set", unknownOutcome)
	}
	for _, o := range []Outcome{"", unknownOutcome} {
		if !Charges(o) {
			t.Errorf("Charges(%q) = false, want an unknown outcome charged", o)
		}
	}

	// And a send refused under it is charged by the prompt budget, as a refusal is.
	h := newHarness(t)
	h.reach(StateReady)
	h.conn.FailPrompt(outcomeRefusal(unknownOutcome))
	h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
	h.wantBudgets(Budgets{PromptFailures: 1})
}

// The prompt budget asks the policy: a send refused under each outcome charges one prompt
// failure exactly where Charges says it does, and nothing where it says not. The outcome is the
// one the error carries (sendOutcome), so the site decides nothing.
func TestThePromptBudgetChargesWhatTheOutcomePolicySays(t *testing.T) {
	for _, o := range append(Outcomes(), unknownOutcome) {
		t.Run(string(o), func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateReady)
			h.conn.FailPrompt(outcomeRefusal(o))
			h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
			want := Budgets{}
			if Charges(o) {
				want.PromptFailures = 1
			}
			h.wantBudgets(want)
			if p := h.pending(); !p.DeliveredAt.IsZero() || !p.ConfirmedAt.IsZero() {
				t.Fatalf("after a refused send the task is %+v, want it waiting unread", p)
			}
		})
	}
}

// The death budget asks the policy: a death with work outstanding counts one death exactly where
// Charges says the outcome charges. The state guard is the function's own and is held open here —
// a ready claim holding a task — so what varies is the outcome alone.
func TestTheDeathBudgetChargesWhatTheOutcomePolicySays(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.conn.FailPrompt(errBoom)
	h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
	for _, o := range append(Outcomes(), unknownOutcome) {
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
// outcome charges, for both sites that count it (relaunchAfterFailure and launchNow, through
// chargeLaunch).
func TestTheLaunchBudgetChargesWhatTheOutcomePolicySays(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	for _, o := range append(Outcomes(), unknownOutcome) {
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

// The agent's answer that it is in a turn of its own is charged nothing on each of the four
// paths a prompt can fail on, and the task waits unread for that turn's end: refused before the
// acknowledgement (refused), refused after it (lateRefused), a result that ran no turn
// (promptResulted), and the wait for the turn running out after the agent said so (noTurn). Each
// path is driven past the prompt failure limit, so a path that charged it would retire the
// process; the claim keeps it.
func TestABusyAnswerOnEachPromptPathKeepsTheClaimAndTheTaskWaitsUnread(t *testing.T) {
	for _, path := range []struct {
		name string
		// answer has the agent answer the pending prompt that it is in a turn of its own, by
		// this path.
		answer func(h *harness)
	}{
		{name: "refused", answer: func(h *harness) {
			h.conn.FailPrompt(busyRefusal())
			h.observe(runtime.Alive) // the sweep sends it, and the agent refuses it busy
			h.conn.FailPrompt(nil)
		}},
		{name: "lateRefused", answer: func(h *harness) {
			h.observe(runtime.Alive) // the sweep sends it, and the agent acknowledges it
			h.must(StreamLateRefusal{Claim: testToken, DeliveryID: h.pending().ID, Error: busyWords,
				Outcome: OutcomeAgentBusy, ConnSequence: h.conn.Sequence()})
		}},
		{name: "promptResulted", answer: func(h *harness) {
			h.observe(runtime.Alive)
			h.must(StreamPromptResult{Claim: testToken, DeliveryID: h.pending().ID, AgentInvoked: false,
				Error: busyWords, Outcome: OutcomeAgentBusy})
		}},
		{name: "noTurn", answer: func(h *harness) {
			h.observe(runtime.Alive)
			// The shim replays what Oh My Pi answered an earlier send of this same task: it is
			// in a turn of its own. The wait for this send's turn is still armed, and runs out.
			h.must(StreamLateRefusal{Claim: testToken, DeliveryID: h.pending().ID, Error: busyWords,
				Outcome: OutcomeAgentBusy, Replayed: true, ConnSequence: h.conn.Sequence()})
			h.advance(2 * testRPC)
		}},
	} {
		t.Run(path.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateReady)
			process := h.locator()
			// The first send is lost, so each answer below is to a send the sweep makes.
			h.conn.FailPrompt(errBoom)
			h.must(RequestDeliver{Claim: testToken, Task: "the task", Generation: 7})
			h.conn.FailPrompt(nil)
			for answered := 1; answered <= testLimits().PromptFailures+1; answered++ {
				path.answer(h)
				h.wantBudgets(Budgets{})
				h.wantCalls("Suspend", 0)
				if p := h.pending(); !p.DeliveredAt.IsZero() || !p.ConfirmedAt.IsZero() {
					t.Fatalf("after busy answer %d the task is %+v, want it waiting unread and unconfirmed", answered, p)
				}
				if loc := h.locator(); loc != process {
					t.Fatalf("after busy answer %d the claim runs %+v, want the process it had", answered, loc)
				}
				if run := h.claim().ServingRun(); run != 0 {
					t.Fatalf("after busy answer %d the claim serves run %d, want none: the agent never read the task", answered, run)
				}
			}
		})
	}
}
