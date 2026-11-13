package supervise

import (
	"errors"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The closed set of events that may charge a claim's budget is runtime.Outcome, declared below
// both the stream that mints its members and this package, which decides on them. No member is
// derived from the words of a sentence here. Whether an outcome charges a budget is Charges's
// alone: each charge function asks it (chargePrompt, chargeDeath, and chargeLaunch, through which
// relaunchAfterFailure counts), and each site that can charge names its outcome and decides
// nothing. An outcome is never stored: what ended a failed claim is a FailureCause, a different
// type whose doc says how the two differ, and failureCause is the one place a cause is derived
// from an outcome.

// Charges is the one policy that decides whether an outcome charges the budget its site counts:
// the prompt budget (chargePrompt), the death budget (chargeDeath) or the launch budget
// (relaunchAfterFailure, chargeLaunch). It is one switch over the closed set, one row per member,
// and its default is the row of runtime.OutcomeUnrecognised: an outcome the policy does not
// recognise — the zero value, or a value no member holds — is charged, because a policy that
// waived what it cannot name would make a crash-looping process immortal.
// TestChargesDecidesEveryOutcome holds each row, and TestAnOutcomeThePolicyDoesNotKnowIsCharged
// the default.
//
// What charges nothing is an event that says nothing against the agent: an agent in a turn of its
// own is working, not refusing the work; a prompt that never reached the agent (the transport, or
// an identity the daemon could not mint) was no answer of the agent's; and a provider error is the
// provider's, which a claim counts in a run of its own (Claim.ProviderErrorRun) rather than in a
// budget that would fail every claim at once in an outage.
//
// The guards that are not outcome questions stay at their sites: the lifecycle
// check before a relaunch and before a retirement (checkLaunch), an operator's suspension held
// over a retirement (suspendHeld), the last retirement's fail, and the death budget's state guard
// (chargeDeath).
func Charges(o runtime.Outcome) bool {
	switch o {
	case runtime.OutcomeAgentBusy:
		return false
	case runtime.OutcomePromptRefused:
		return true
	case runtime.OutcomeAdoptionRefused:
		return true
	case runtime.OutcomeIdentityUnavailable:
		return false
	case runtime.OutcomeTransportLost:
		return false
	case runtime.OutcomeNoTurnStarted:
		return true
	case runtime.OutcomeNoTurnRun:
		return true
	case runtime.OutcomeProviderError:
		return false
	case runtime.OutcomeProcessDied:
		return true
	case runtime.OutcomeLaunchFailed:
		return true
	case runtime.OutcomeNeverRegistered:
		return true
	case runtime.OutcomeStalePlugin:
		return true
	default:
		return true
	}
}

// failureCause is the word a claim fails under when the launch or the death budget runs out on
// o, or when o ends the claim by itself (a provider error that cannot clear, and a run of turns
// the provider refused that reached the launch failure limit, turnOutcome): the one place an
// outcome becomes the cause that is stored, served and shown (FailureCause). The prompt budget
// fails a claim under its own word whatever event spent it (CausePromptRetiresExhausted,
// chargePrompt), since what ran out there is the retirements and not the event. An outcome with
// no cause of its own is CauseUnrecorded, which no live path reaches: every caller names one of
// the members below.
func failureCause(o runtime.Outcome) FailureCause {
	switch o {
	case runtime.OutcomeProcessDied:
		return CauseDied
	case runtime.OutcomeLaunchFailed:
		return CauseLaunchRefused
	case runtime.OutcomeNeverRegistered:
		return CauseNeverRegistered
	case runtime.OutcomeStalePlugin:
		return CauseStalePlugin
	case runtime.OutcomeProviderError:
		return CauseProviderError
	}
	return CauseUnrecorded
}

// sendOutcome is the outcome of a send that failed (refused): the member the error carries, where
// the package that read the far side's answer minted one (runtime.RefusalOutcome: internal/stream's
// refusal of a prompt or of an adoption); else the runtime's own sentinel for the daemon's identity
// mint and the shim's adoption; else, for an error that is no answer about the prompt at all — the
// transport, or a refusal of the negotiation ahead of it, which mints nothing — the transport. A
// prompt refusal that carries no member is OutcomeUnrecognised, which is charged: nothing here
// reads the refusal's words.
func sendOutcome(err error) runtime.Outcome {
	if minted := runtime.RefusalOutcome(err); minted != runtime.OutcomeUnrecognised {
		return minted
	}
	switch {
	case errors.Is(err, runtime.ErrPromptRefused):
		return runtime.OutcomeUnrecognised
	case errors.Is(err, runtime.ErrAdoptRefused):
		return runtime.OutcomeAdoptionRefused
	case errors.Is(err, runtime.ErrIdentityUnavailable):
		return runtime.OutcomeIdentityUnavailable
	}
	return runtime.OutcomeTransportLost
}
