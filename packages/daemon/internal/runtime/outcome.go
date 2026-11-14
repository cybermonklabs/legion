package runtime

import "errors"

// Outcome is one event that may charge a claim's budget: a prompt the agent refused or never
// turned, a process that died, a launch that failed. It is minted where the daemon learns the
// fact — for an answer of the agent's, by the package that read the frame (internal/stream),
// from the frame's machine-readable code and never from its words — and the supervisor decides on
// the member alone. Whether an outcome charges a budget is supervise.Charges's alone.
//
// The set is declared here, below both the stream that mints its members and the supervisor that
// charges them, so the transport depends on no package above it. The set is closed, and Outcomes
// lists it. Its zero value is OutcomeUnrecognised, which is charged, as is every value the set does
// not hold: a policy that waived what it cannot name would make a crash-looping process immortal.
//
// An Outcome is never stored. What ended a failed claim is a supervise.FailureCause, a different
// type whose doc says how the two differ.
type Outcome string

const (
	// OutcomeUnrecognised is the zero value: an event no site named, or a value the set does not
	// hold. A prompt refusal that carries no code is this, as internal/stream mints it
	// (stream.PromptRefusalOutcome) — Oh My Pi's busy sentence with no code among them, since its
	// words are not read — and so is a prompt refusal whose error carries no minted member
	// (supervise's sendOutcome).
	OutcomeUnrecognised Outcome = ""
	// OutcomeAgentBusy is the agent answering that it is in a turn of its own, which it says by the
	// code its answer carries (shimwire.CodeSessionBusy): Oh My Pi's refusal, and the Claude Code
	// bridge's, read by internal/stream before the prompt's acknowledgement, after it, or as a
	// result that ran no turn. A refusal that says it in words alone is not this. A late refusal is
	// this also when the claim is in a turn the stream saw start, or the agent says it is streaming
	// when asked (supervise's lateRefused).
	OutcomeAgentBusy Outcome = "agent_busy"
	// OutcomePromptRefused is the agent refusing a prompt with a code other than the busy one — a
	// provider that rejected before any turn began, an agent that will not take the work.
	// internal/stream mints it from the same frame.
	OutcomePromptRefused Outcome = "prompt_refused"
	// OutcomeAdoptionRefused is the shim refusing to adopt the working copy (ErrAdoptRefused),
	// which internal/stream mints on its refusal of that command.
	OutcomeAdoptionRefused Outcome = "adoption_refused"
	// OutcomeIdentityUnavailable is the daemon failing to mint the role's GitHub App identity
	// before any frame left (ErrIdentityUnavailable, supervise's Machine.adopt).
	OutcomeIdentityUnavailable Outcome = "identity_unavailable"
	// OutcomeTransportLost is a send that failed with no answer from the agent: a closed
	// connection, no answer within the RPC timeout, a frame lost on the way (sendOutcome).
	OutcomeTransportLost Outcome = "transport_lost"
	// OutcomeNoTurnStarted is the wait for an acknowledged prompt's turn running out with no turn
	// started (supervise's noTurn).
	OutcomeNoTurnStarted Outcome = "no_turn_started"
	// OutcomeNoTurnRun is a prompt result that ran no turn and whose code names no turn of the
	// agent's own, which internal/stream mints from the result's frame (stream.PromptResult).
	OutcomeNoTurnRun Outcome = "no_turn_run"
	// OutcomeProviderError is a turn that ran and ended in the provider's error (supervise's
	// providerErrored), read from the result's AgentInvoked and its error object.
	OutcomeProviderError Outcome = "provider_error"
	// OutcomeProcessDied is the claim's process found gone while it was live (supervise's died).
	OutcomeProcessDied Outcome = "process_died"
	// OutcomeLaunchFailed is a launch the runtime refused (supervise's launchNow).
	OutcomeLaunchFailed Outcome = "launch_failed"
	// OutcomeNeverRegistered is a process whose agent did not register before its deadline, or
	// registered and never said it was ready (supervise's registrationDeadline). It is a member of
	// its own, beside OutcomeLaunchFailed, because the cause a claim fails under tells the two
	// apart (CauseNeverRegistered, CauseLaunchRefused).
	OutcomeNeverRegistered Outcome = "never_registered"
	// OutcomeStalePlugin is a registration on a plugin contract other than the daemon's own from
	// a process a relaunch onto that contract already started (supervise's staleRegistration). A
	// registration no relaunch was made for is no failure and names no outcome: it is the
	// stale-plugin lane's uncharged relaunch (relaunchStale).
	OutcomeStalePlugin Outcome = "stale_plugin"
)

// Outcomes is every member of Outcome, in the order they are declared. A member added to the type
// is added here and given a row in supervise.Charges. A fresh slice is returned so that no caller
// can edit the one the next reads.
func Outcomes() []Outcome {
	return []Outcome{OutcomeUnrecognised, OutcomeAgentBusy, OutcomePromptRefused, OutcomeAdoptionRefused,
		OutcomeIdentityUnavailable, OutcomeTransportLost, OutcomeNoTurnStarted, OutcomeNoTurnRun,
		OutcomeProviderError, OutcomeProcessDied, OutcomeLaunchFailed, OutcomeNeverRegistered, OutcomeStalePlugin}
}

// RefusalOutcome is the member the error in err's chain that minted one carries — the stream's
// refusal of a prompt or of an adoption (stream.RefusedError) — or OutcomeUnrecognised when none
// does, as runtime/fake's refusal does not. It reads the member and never the refusal's words
// (RefusalReason).
func RefusalOutcome(err error) Outcome {
	var minted interface{ RefusalOutcome() Outcome }
	if errors.As(err, &minted) {
		return minted.RefusalOutcome()
	}
	return OutcomeUnrecognised
}
