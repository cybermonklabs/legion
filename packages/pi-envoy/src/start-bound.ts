/** What a pass needs beside the start's wait for its broker token: the dial, its subscribes and
 *  the registration (in a pass of 30 s, the floor of the pass deadline, `TRADE_DEADLINE_MS`
 *  leaves this). */
export const PASS_REST_MS = 10_000;

/**
 * How long a session's start waits for its broker token inside a pass of `passDeadlineMs`: the
 * pass less the 10 s the rest of it needs (`PASS_REST_MS`), or half the pass where the pass is
 * shorter than twice that rest. A short pass deadline (one a short `ENVOY_HEARTBEAT_MS` allows,
 * such as 7.5 s) then still gives the trade a share of it, rather than 0 ms and a start that fails
 * "within 0 s" at once. `ready` waits the smaller of this and its own trade deadline
 * (`ReadyOptions.boundMs`).
 */
export function startTradeBoundMs(passDeadlineMs: number): number {
  return Math.max(passDeadlineMs - PASS_REST_MS, passDeadlineMs / 2);
}

/** What a pass needs after a session switch's follow: the new session's subjects and its
 *  registration. In a pass of 30 s, the floor of the pass deadline, the follow's own most
 *  (`FOLLOW_DEADLINE_MS`, 28 s) leaves this. */
export const FOLLOW_REST_MS = 2_000;

/**
 * How long a session switch's follow (`followNatsCredential`) may take inside a pass of
 * `passDeadlineMs`: the pass less the 2 s the rest of it needs (`FOLLOW_REST_MS`), or half the
 * pass where the pass is shorter than twice that rest, so a pass deadline under 30 s bounds the
 * follow by it rather than leave the follow running past the pass. The follow takes the smaller
 * of this and its own most, so a pass of 30 s or more, the default 90 s among them, changes
 * nothing.
 */
export function followBoundMs(passDeadlineMs: number): number {
  return Math.max(passDeadlineMs - FOLLOW_REST_MS, passDeadlineMs / 2);
}
