import {
  type EnvoyPluginInterface,
  type EnvoyTaskSignal,
  envoyPluginInterface,
  readEnvoyPluginInterface,
} from "./interface";

// A claim's task value reaches the Legion entry through the Envoy entry, which holds the session's
// relay subject and so the task frames the listener relays on it, and the two plugins share no
// import: the Envoy entry publishes each value and each new epoch of that relay into the
// interface (`publishTaskSignal`), and the Legion entry's claim takes them through the one taker
// it sets there (`takeTaskSignals`). The Legion entry's take, in turn, says through the interface
// when a task's turn is pending (`markTaskTurnPending`, `clearTaskTurnPending`), which the Envoy
// entry's card wake reads, so a card that finds the session idle does not start the turn the task
// goes into.

/**
 * Hands `signal` to the Legion entry's taker, if one is set, on `iface`, the interface the Envoy
 * entry published into (undefined where it published nothing, a build at another version having
 * published first). A taker that throws is the Legion entry's fault and not the relay's: it is
 * reported to `log` and the relay goes on.
 */
export function publishTaskSignal(
  iface: EnvoyPluginInterface | undefined,
  signal: EnvoyTaskSignal,
  log: (message: string, error: unknown) => void
): void {
  const taker = iface?.taskValues.taker;
  if (taker === undefined) return;
  try {
    taker(signal);
  } catch (error) {
    log("the Legion entry's task value taker threw", error);
  }
}

/**
 * Sets the one taker of every task signal: the last call wins, so only the claim boot that
 * establishes this process's Legion identity calls it, never an extension factory, whose `task`
 * subagent's instance would take the pane's values away from it (`EnvoyTaskValues.taker`).
 */
export function takeTaskSignals(taker: (signal: EnvoyTaskSignal) => void): void {
  envoyPluginInterface().taskValues.taker = taker;
}

/** Marks `sessionID` as the session whose task's turn is pending. */
export function markTaskTurnPending(sessionID: string): void {
  envoyPluginInterface().taskTurnPending = { session: sessionID };
}

/**
 * Clears the mark when it names `sessionID`. Read only at this build's interface version, as
 * `clearStartupTurn` is: a session that is not Legion's may run beside an Envoy entry speaking
 * another.
 */
export function clearTaskTurnPending(sessionID: string): void {
  const reading = readEnvoyPluginInterface();
  if (reading.kind !== "present") return;
  if (reading.iface.taskTurnPending?.session !== sessionID) return;
  reading.iface.taskTurnPending = undefined;
}
