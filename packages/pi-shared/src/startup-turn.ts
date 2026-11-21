import { envoyPluginInterface, readEnvoyPluginInterface } from "./interface";

// The startup-turn mark (`EnvoyPluginInterface.startupTurn`) names a session whose first run has
// not started while its start is due or on the way: the start message the operator-launched
// controller's `/legion-controller-start` sends after Oh My Pi's startup (due from that session's
// `session_start`, `markStartupTurnPending`), the start message the Legion entry sends to an idle
// session, or the start prompt the daemon delivers once its controller reports ready. The Legion
// entry marks it sent right before that start (`markStartupTurnSent`) and clears it
// (`clearStartupTurn`) on a fact, never on a clock: from that session's `agent_start` and
// `session_shutdown`, an unsent one when a controller claim fails before the send or hands over to
// a new session, and a sent one when its send fails. pi-envoy holds the session's startup cards for
// as long as an unsent mark stands, and for a sent one until it goes or 30 s after its send,
// whichever comes first (`packages/pi-envoy/src/card-wake.ts`), so every unsent mark set has these
// ways to go. A start message queued as a follow-up behind a turn the operator already started is
// never marked sent: that run, the session's first, has already fired its one `agent_start`, which
// cleared an unsent mark, and the follow-up runs inside it.

/** Marks `sessionID` as the session whose start is due and not sent yet. */
export function markStartupTurnPending(sessionID: string): void {
  envoyPluginInterface().startupTurn = { session: sessionID, sentAt: undefined };
}

/** Marks `sessionID` as the session whose start was sent now. */
export function markStartupTurnSent(sessionID: string): void {
  envoyPluginInterface().startupTurn = { session: sessionID, sentAt: Date.now() };
}

/**
 * Clears the mark when it names `sessionID`. A `task` subagent's run has its own session id and
 * clears nothing; nor does any subagent's shutdown. Read only at this build's interface version: a
 * session that is not Legion's may run beside an Envoy entry speaking another.
 */
export function clearStartupTurn(sessionID: string): void {
  const reading = readEnvoyPluginInterface();
  if (reading.kind !== "present") return;
  if (reading.iface.startupTurn?.session !== sessionID) return;
  reading.iface.startupTurn = undefined;
}
