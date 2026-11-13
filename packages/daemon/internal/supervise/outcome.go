package supervise

import (
	"errors"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// Outcome is one event that may charge a claim's budget: a prompt the agent refused or never
// turned, a process that died, a launch that failed. It is minted where the daemon learns the
// fact — for an answer of the agent's, by the package that read the frame (internal/stream) — and
// no member is derived from the words of a sentence here. Whether an outcome charges a budget is
// Charges's alone: each charge function asks it (chargePrompt, chargeDeath, relaunchAfterFailure
// and chargeLaunch), and each site that can charge names its outcome and decides nothing.
//
// The set is closed, and Outcomes lists it. Its zero value is OutcomeUnrecognised, which is
// charged, as is every value the set does not hold: a policy that waived what it cannot name would
// make a crash-looping process immortal.
//
// An Outcome is never stored. What ended a failed claim is a FailureCause, a different type whose
// doc says how the two differ; failureCause is the one place a cause is derived from an outcome.
type Outcome string

const (
	// OutcomeUnrecognised is the zero value: an event no site named, or a value the set does not
	// hold. Nothing mints it on purpose. A prompt refusal whose error carries no minted member is
	// this (sendOutcome).
	OutcomeUnrecognised Outcome = ""
	// OutcomeAgentBusy is the agent answering that it is in a turn of its own: Oh My Pi's busy
	// refusal or the Claude Code bridge's, read from the frame by internal/stream
	// (stream.PromptRefusalOutcome), before the prompt's acknowledgement, after it, or as a
	// result that ran no turn. A late refusal is this also when the claim is in a turn the stream
	// saw start, or the agent says it is streaming when asked (lateRefused).
	OutcomeAgentBusy Outcome = "agent_busy"
	// OutcomePromptRefused is the agent refusing a prompt in any other words — a provider that
	// rejected before any turn began, an agent that will not take the work. internal/stream mints
	// it from the same frame.
	OutcomePromptRefused Outcome = "prompt_refused"
	// OutcomeAdoptionRefused is the shim refusing to adopt the working copy
	// (runtime.ErrAdoptRefused), which internal/stream mints on its refusal of that command.
	OutcomeAdoptionRefused Outcome = "adoption_refused"
	// OutcomeIdentityUnavailable is the daemon failing to mint the role's GitHub App identity
	// before any frame left (runtime.ErrIdentityUnavailable, Machine.adopt).
	OutcomeIdentityUnavailable Outcome = "identity_unavailable"
	// OutcomeTransportLost is a send that failed with no answer from the agent: a closed
	// connection, no answer within the RPC timeout, a frame lost on the way (sendOutcome).
	OutcomeTransportLost Outcome = "transport_lost"
	// OutcomeNoTurnStarted is the wait for an acknowledged prompt's turn running out with no turn
	// started (noTurn).
	OutcomeNoTurnStarted Outcome = "no_turn_started"
	// OutcomeNoTurnRun is a prompt result that ran no turn and names no turn of the agent's own,
	// which internal/stream mints from the result's frame (stream.PromptResult).
	OutcomeNoTurnRun Outcome = "no_turn_run"
	// OutcomeProviderError is a turn that ran and ended in the provider's error
	// (providerErrored), read from the result's AgentInvoked and its error object.
	OutcomeProviderError Outcome = "provider_error"
	// OutcomeProcessDied is the claim's process found gone while it was live (died).
	OutcomeProcessDied Outcome = "process_died"
	// OutcomeLaunchFailed is a launch the runtime refused (launchNow).
	OutcomeLaunchFailed Outcome = "launch_failed"
	// OutcomeNeverRegistered is a process whose agent did not register before its deadline, or
	// registered and never said it was ready (registrationDeadline). It is a member of its own,
	// beside OutcomeLaunchFailed, because the cause a claim fails under tells the two apart
	// (CauseNeverRegistered, CauseLaunchRefused).
	OutcomeNeverRegistered Outcome = "never_registered"
	// OutcomeStalePlugin is a registration on a plugin contract other than the daemon's own from
	// a process a relaunch onto that contract already started (staleRegistration). A
	// registration no relaunch was made for is no failure and names no outcome: it is the
	// stale-plugin lane's uncharged relaunch (relaunchStale).
	OutcomeStalePlugin Outcome = "stale_plugin"
)

// Outcomes is every member of Outcome, in the order they are declared. A member added to the type
// is added here and given a row in Charges. A fresh slice is returned so that no caller can edit
// the one the next reads.
func Outcomes() []Outcome {
	return []Outcome{OutcomeUnrecognised, OutcomeAgentBusy, OutcomePromptRefused, OutcomeAdoptionRefused,
		OutcomeIdentityUnavailable, OutcomeTransportLost, OutcomeNoTurnStarted, OutcomeNoTurnRun,
		OutcomeProviderError, OutcomeProcessDied, OutcomeLaunchFailed, OutcomeNeverRegistered, OutcomeStalePlugin}
}

// Charges is the one policy that decides whether an outcome charges the budget its site counts:
// the prompt budget (chargePrompt), the death budget (chargeDeath) or the launch budget
// (relaunchAfterFailure, chargeLaunch). It is one switch over the closed set, one row per member,
// and its default charges: an outcome the policy does not recognise — the zero value, or a value
// no member holds — is charged, because a policy that waived what it cannot name would make a
// crash-looping process immortal. TestChargesDecidesEveryOutcome holds each row, and
// TestAnOutcomeThePolicyDoesNotKnowIsCharged the default.
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
func Charges(o Outcome) bool {
	switch o {
	case OutcomeAgentBusy:
		return false
	case OutcomePromptRefused:
		return true
	case OutcomeAdoptionRefused:
		return true
	case OutcomeIdentityUnavailable:
		return false
	case OutcomeTransportLost:
		return false
	case OutcomeNoTurnStarted:
		return true
	case OutcomeNoTurnRun:
		return true
	case OutcomeProviderError:
		return false
	case OutcomeProcessDied:
		return true
	case OutcomeLaunchFailed:
		return true
	case OutcomeNeverRegistered:
		return true
	case OutcomeStalePlugin:
		return true
	case OutcomeUnrecognised:
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
func failureCause(o Outcome) FailureCause {
	switch o {
	case OutcomeProcessDied:
		return CauseDied
	case OutcomeLaunchFailed:
		return CauseLaunchRefused
	case OutcomeNeverRegistered:
		return CauseNeverRegistered
	case OutcomeStalePlugin:
		return CauseStalePlugin
	case OutcomeProviderError:
		return CauseProviderError
	}
	return CauseUnrecorded
}

// sendOutcome is the outcome of a send that failed (refused): the member the error carries, where
// the package that read the far side's answer minted one (internal/stream's refusal of a prompt or
// of an adoption); else the runtime's own sentinel for the daemon's identity mint and the shim's
// adoption; else, for an error that is no answer about the prompt at all — the transport, or a
// refusal of the negotiation ahead of it, which mints nothing — the transport. A prompt refusal
// that carries no member is OutcomeUnrecognised, which is charged: nothing here reads the
// refusal's words.
func sendOutcome(err error) Outcome {
	var minted interface{ RefusalOutcome() Outcome }
	if errors.As(err, &minted) && minted.RefusalOutcome() != OutcomeUnrecognised {
		return minted.RefusalOutcome()
	}
	switch {
	case errors.Is(err, runtime.ErrPromptRefused):
		return OutcomeUnrecognised
	case errors.Is(err, runtime.ErrAdoptRefused):
		return OutcomeAdoptionRefused
	case errors.Is(err, runtime.ErrIdentityUnavailable):
		return OutcomeIdentityUnavailable
	}
	return OutcomeTransportLost
}
