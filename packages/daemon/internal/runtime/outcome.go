package runtime

// Outcome is one event that may charge a claim's budget: a take whose turn never started, a refused
// adoption, a process that died, a launch that failed. It is minted where the daemon learns the
// fact, and never from the words of a far side's answer, and the supervisor decides on the member
// alone. Whether an outcome charges a budget is supervise.Charges's alone.
//
// The set is declared here, below the supervisor that charges it, so the transport and the runtime
// depend on no package above them. The set is closed, and Outcomes lists it. Its zero value is
// OutcomeUnrecognised, which is charged, as is every value the set does not hold: a policy that
// waived what it cannot name would make a crash-looping process immortal.
//
// An Outcome is never stored. What ended a failed claim is a supervise.FailureCause, a different
// type whose doc says how the two differ.
type Outcome string

const (
	// OutcomeUnrecognised is the zero value: an event no site named, or a value the set does not
	// hold. No live site names it; the policy charges it (supervise.Charges's default).
	OutcomeUnrecognised Outcome = ""
	// OutcomeAdoptionRefused is the shim refusing to adopt the working copy for a take
	// (ErrAdoptRefused, supervise's refusedAdoption).
	OutcomeAdoptionRefused Outcome = "adoption_refused"
	// OutcomeIdentityUnavailable is the daemon failing to mint the role's GitHub App identity for
	// a take's adoption, before any frame left (ErrIdentityUnavailable, supervise's Machine.adopt
	// and refusedAdoption).
	OutcomeIdentityUnavailable Outcome = "identity_unavailable"
	// OutcomeTransportLost is a take's adoption that failed with no answer from the shim: a closed
	// connection, no answer within the request's reply deadline, a frame lost on the way
	// (supervise's refusedAdoption).
	OutcomeTransportLost Outcome = "transport_lost"
	// OutcomeNoTurnStarted is one of R5's two waits running out with no turn started: a value the
	// worker reported seeing and asked no take of within the first bound, or an accepted take whose
	// turn did not start within the second (supervise's takeBoundOver and turnStartBoundOver).
	OutcomeNoTurnStarted Outcome = "no_turn_started"
	// OutcomeProviderError is a turn that ran and ended in the provider's error, read from its
	// producer's turn-outcome report, which is that turn's one record: the turn of a take, the
	// report naming the take's number (supervise's providerErrored), a take Oh My Pi dropped at its
	// cost gate before any turn among them, and a turn that ran none of the daemon's tasks
	// (supervise's unpromptedTurnEnded). Both are one kind of end, which the claim counts in one run
	// of its own (dispatch://INFRA-4508).
	OutcomeProviderError Outcome = "provider_error"
	// OutcomeProcessDied is the claim's process found gone while it was live (supervise's died).
	OutcomeProcessDied Outcome = "process_died"
	// OutcomeLaunchFailed is a launch the runtime refused (supervise's launchTries).
	OutcomeLaunchFailed Outcome = "launch_failed"
	// OutcomeNeverRegistered is a process whose agent did not register before its deadline, or
	// registered and never said it was ready (supervise's registrationDeadline). It is a member of
	// its own, beside OutcomeLaunchFailed, because the cause a claim fails under tells the two
	// apart (CauseNeverRegistered, CauseLaunchRefused).
	OutcomeNeverRegistered Outcome = "never_registered"
	// OutcomeNeverReconnected is a live process whose agent had said it was ready and whose shim
	// then said no hello within the reconnect deadline: after a daemon start restored its claim, or
	// after its connection closed with a take's task held or given to it since (supervise's
	// reconnectDeadline). It is a member of its own, beside OutcomeNeverRegistered, because the
	// cause a claim fails under tells the two apart (CauseNeverReconnected, CauseNeverRegistered).
	OutcomeNeverReconnected Outcome = "never_reconnected"
	// OutcomeStalePlugin is a registration on a plugin contract other than the daemon's own from
	// a process a relaunch onto that contract already started (supervise's staleRegistration). A
	// registration no relaunch was made for is no failure and names no outcome: it is the
	// stale-plugin lane's uncharged relaunch (relaunchStale).
	OutcomeStalePlugin Outcome = "stale_plugin"
	// OutcomeEarlierReleaseProcessEnded is the end of a process an earlier release started for a
	// claim that records a session and no file, whose registration the fence refused: the
	// process died, or its registration deadline passed (the roll edge, supervise's endOutcome).
	// It is a member of its own, beside OutcomeProcessDied and OutcomeNeverRegistered, which name
	// the same ends of any other process, because which process ended is what the policy decides
	// its charge on.
	OutcomeEarlierReleaseProcessEnded Outcome = "earlier_release_process_ended"
	// OutcomeResumeByAgentUnregistered is the end of a start that had Oh My Pi resume by its id
	// the session a claim records with no file (SpawnSpec.ResumeByAgent), whose agent did not
	// register as that session: the process died, or its registration deadline passed
	// (supervise's endOutcome). It is a member of its own for the reason
	// OutcomeEarlierReleaseProcessEnded is.
	OutcomeResumeByAgentUnregistered Outcome = "resume_by_agent_unregistered"
)

// Outcomes is every member of Outcome, in the order they are declared. A member added to the type
// is added here and given a row in supervise.Charges. A fresh slice is returned so that no caller
// can edit the one the next reads.
func Outcomes() []Outcome {
	return []Outcome{OutcomeUnrecognised, OutcomeAdoptionRefused, OutcomeIdentityUnavailable,
		OutcomeTransportLost, OutcomeNoTurnStarted, OutcomeProviderError, OutcomeProcessDied,
		OutcomeLaunchFailed, OutcomeNeverRegistered, OutcomeNeverReconnected, OutcomeStalePlugin,
		OutcomeEarlierReleaseProcessEnded, OutcomeResumeByAgentUnregistered}
}
