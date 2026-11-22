package supervise

import (
	"context"
	"errors"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The closed set of events that may charge a claim's budget is runtime.Outcome, declared below
// both the stream that mints its members and this package, which decides on them. No member is
// derived from the words of a sentence here. Whether an outcome charges a budget is Charges's
// alone: each charge function asks it (chargePrompt, chargeDeath, chargeLaunch, and
// relaunchAfterFailure, which counts through chargeLaunch), and each site that can charge names
// its outcome and decides nothing. Two rules name an outcome from more than the event the site
// saw: the agent's busy answer, from the daemon's own record or the agent's get_state, never from
// its words (agentRefusalOutcome); and the end of a process that never registered as the session
// its claim records with no file, from which process that was (endOutcome). An outcome is never
// stored: what ended a failed claim is a FailureCause, a different type whose doc says how the
// two differ, and failureCause is the one place a cause is derived from an outcome.

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
// budget that would fail every claim at once in an outage. The end of a process that never
// registered as the session its claim records with no file says nothing against the agent either
// where that process is one an earlier release started (runtime.OutcomeEarlierReleaseProcessEnded)
// or a resume Oh My Pi made by the session's id (runtime.OutcomeResumeByAgentUnregistered,
// dispatch://INFRA-4108): the first is that release's agent and not the claim's, and the second is
// a resume whose session the pod's launcher cannot look up, so its end cannot tell a session that
// is gone from an agent that failed, and the next try forgets the session (resumeByID).
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
	case runtime.OutcomeEarlierReleaseProcessEnded:
		return false
	case runtime.OutcomeResumeByAgentUnregistered:
		return false
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
// the members below, and relaunchAfterFailure asks it of no outcome the policy charges nothing.
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
// prompt refusal that carries no member is OutcomeUnrecognised: nothing here reads the refusal's
// words, and whether the agent was busy is the next rule's (agentRefusalOutcome).
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

// agentRefusalOutcome is the outcome of the agent's answer to a prompt of the task that ran no
// turn, where o is what the stream minted from the answer's frame. It is the one rule for the
// three places the agent gives that answer — the send refused outright (refused), the
// acknowledged prompt refused (lateRefused), and its result saying it ran no turn
// (promptResulted) — and each charges the outcome it returns through the one policy (Charges).
// The same answers naming a prompt whose wait for its turn already ran out read it too
// (waitedOutAnswer, from lateRefused's refusedElsewhere and from promptResulted): they charge
// nothing, and a busy outcome records the turn there as it does on the pending prompt. So does a
// replayed refusal of the pending prompt while the wait for its turn is armed (refusedElsewhere),
// whose busy outcome is kept for that wait's end, which then charges nothing (answeredBusy,
// noTurn).
//
// The answer is the agent's refusal when o is a refusal with no code (OutcomeUnrecognised), one
// with a code other than the busy one (OutcomePromptRefused), or a result that ran no turn
// (OutcomeNoTurnRun). Such a refusal is the agent's busy answer (OutcomeAgentBusy) when the agent is
// in a turn, whatever its code says, and two facts tell the daemon so beside the code. The first is
// its own record: the claim is working, in a turn the stream saw start or a busy answer recorded.
// The second, asked only where the record does not hold the claim working, is the agent's own
// state (get_state's isStreaming): the pinned Oh My Pi says it is streaming from a prompt's
// admission through its turn. That is one ask per refusal, and refusals are rare. A turn running is
// the collision: the agent is working, not refusing to work, and charging it would spend the
// budget of the turn doing the work. Neither fact is the agent's words, and the code is read by
// the stream alone, which mints OutcomeAgentBusy from code session_busy; so a busy outcome comes
// back as it is, unasked, and so does every outcome that is no answer of the agent's (a lost
// transport, a refused adoption, an identity the daemon could not mint). A refusal that neither
// fact nor its code makes busy comes back as the stream minted it, and is charged.
//
// The facts are what keep a pod on the earlier worker image from being charged for being busy:
// its Oh My Pi gives its busy refusal no code, and a pod moves onto this image only at its update
// round, never when the daemon moves. The record is read first because it costs nothing, and
// because the ask can time out, where charging a working agent is the worse mistake; an ask that
// fails or times out says the agent is not streaming (streaming). So an agent of the earlier
// image that refuses busy with no code, while the daemon's record does not hold the claim working,
// and that does not answer get_state within the RPC bound, is still charged as unrecognised (as a
// prompt that ran no turn, for a result) until its pod's update round; so is one whose turn ended
// between its refusal and the ask.
func (m *Machine) agentRefusalOutcome(ctx context.Context, o runtime.Outcome) runtime.Outcome {
	switch o {
	case runtime.OutcomeUnrecognised, runtime.OutcomePromptRefused, runtime.OutcomeNoTurnRun:
	default:
		return o
	}
	if m.claim.State == StateWorking {
		return runtime.OutcomeAgentBusy
	}
	if conn, connected := m.deps.Conns.Conn(m.claim.Token); connected {
		if streaming, _ := m.streaming(ctx, conn); streaming {
			return runtime.OutcomeAgentBusy
		}
	}
	return o
}
