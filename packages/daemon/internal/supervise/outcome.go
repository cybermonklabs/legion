package supervise

import (
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The closed set of events that may charge a claim's budget is runtime.Outcome, declared below
// this package, which decides on them. No member is derived from the words of a sentence here.
// Whether an outcome charges a budget is Charges's alone: each charge function asks it
// (chargePrompt, chargeDeath, chargeLaunch, and relaunchAfterFailure, which counts through
// chargeLaunch), and each site that can charge names its outcome: R5's two waits
// (OutcomeNoTurnStarted), a take's adoption (refusedAdoption), a turn-outcome report's provider
// error (providerErrorTurnEnded), a death (died), a launch the runtime refused (launchTries), the
// registration and reconnect deadlines (retireNeverReady) and the stale-plugin relaunch
// (staleRegistration). One rule names an outcome from more than the event the site saw: the end
// of a process that never registered as the session its claim records with no file, from which
// process that was (endOutcome). An outcome is never stored: what ended a failed claim is a
// FailureCause, a different type whose doc says how the two differ, and failureCause is the one
// place a cause is derived from an outcome.
// The provider classifier reads the refusal code separately to identify a policy hold
// (classifyProviderError). That does not change the turn's outcome or its budget charge.

// Charges is the one policy that decides whether an outcome charges the budget its site counts:
// the prompt budget (chargePrompt), the death budget (chargeDeath) or the launch budget
// (relaunchAfterFailure, chargeLaunch). It is one switch over the closed set, one row per member,
// and its default is the row of runtime.OutcomeUnrecognised: an outcome the policy does not
// recognise — the zero value, or a value no member holds — is charged, because a policy that
// waived what it cannot name would make a crash-looping process immortal.
// TestChargesDecidesEveryOutcome holds each row, and TestAnOutcomeThePolicyDoesNotKnowIsCharged
// the default.
//
// What charges nothing is an event that says nothing against the agent: an adoption the daemon
// never got an answer to (the transport, or an identity the daemon could not mint) was no answer of
// the shim's; and a provider error is the provider's, which a claim counts in a run of its own
// (Claim.ProviderErrorRun) rather than in a budget that would fail every claim at once in an
// outage. The end of a process that never registered as the session its claim records with no
// file says nothing against the agent either where that process is one an earlier release started
// (runtime.OutcomeEarlierReleaseProcessEnded) or a resume Oh My Pi made by the session's id
// (runtime.OutcomeResumeByAgentUnregistered, dispatch://INFRA-4108): the first is that release's
// agent and not the claim's, and the second is a resume whose session the pod's launcher cannot
// look up, so its end cannot tell a session that is gone from an agent that failed, and the next
// try forgets the session (resumeByID).
//
// The guards that are not outcome questions stay at their sites: the lifecycle
// check before a relaunch and before a retirement (checkLaunch), an operator's suspension held
// over a retirement (suspendHeld), the last retirement's fail, and the death budget's state guard
// (chargeDeath).
func Charges(o runtime.Outcome) bool {
	switch o {
	case runtime.OutcomeAdoptionRefused:
		return true
	case runtime.OutcomeIdentityUnavailable:
		return false
	case runtime.OutcomeTransportLost:
		return false
	case runtime.OutcomeNoTurnStarted:
		return true
	case runtime.OutcomeProviderError:
		return false
	case runtime.OutcomeProcessDied:
		return true
	case runtime.OutcomeLaunchFailed:
		return true
	case runtime.OutcomeNeverRegistered:
		return true
	case runtime.OutcomeNeverReconnected:
		return true
	case runtime.OutcomeStalePlugin:
		return true
	case runtime.OutcomeEarlierReleaseProcessEnded:
		return false
	case runtime.OutcomeResumeByAgentUnregistered:
		return false
	default:
		return true
	}
}

// failureCause is the word a claim fails under when the launch or the death budget runs out on
// o, or when o ends the claim by itself (a provider error that cannot clear, whether it ended a
// take's turn or a turn that ran none of the daemon's tasks, providerErrorTurnEnded; a run of
// turns the provider refused fails no claim, turnOutcome): the one place an outcome becomes the
// cause that is stored, served and shown (FailureCause). The prompt budget fails a claim under its
// own word whatever event spent it (CausePromptRetiresExhausted, chargePrompt), since what ran out
// there is the retirements and not the event. An outcome with no cause of its own is
// CauseUnrecorded, which no live path reaches: every caller names one of the members below, and
// relaunchAfterFailure asks it of no outcome the policy charges nothing.
func failureCause(o runtime.Outcome) FailureCause {
	switch o {
	case runtime.OutcomeProcessDied:
		return CauseDied
	case runtime.OutcomeLaunchFailed:
		return CauseLaunchRefused
	case runtime.OutcomeNeverRegistered:
		return CauseNeverRegistered
	case runtime.OutcomeNeverReconnected:
		return CauseNeverReconnected
	case runtime.OutcomeStalePlugin:
		return CauseStalePlugin
	case runtime.OutcomeProviderError:
		return CauseProviderError
	}
	return CauseUnrecorded
}
