/**
 * The budget of the extension's `session_shutdown` handler, and the race its deregistration and
 * NATS drain run in: a session whose DELETE never reaches the listener reads live for the live
 * window and unresponsive for the gone horizon after it, with nothing heartbeating it, so a
 * DELETE that fails or is given up says so at warn, naming the session.
 */
import { messageFor } from "@legion/envoy-client/errors";

/** How long Oh My Pi gives a `session_shutdown` handler before it abandons it and seals the
 *  session (`SESSION_SHUTDOWN_HANDLER_TIMEOUT_MS` in the host's extension runner). */
export const HOST_SHUTDOWN_HANDLER_MS = 2_000;

/** What the handler leaves of the host's limit for the synchronous work ahead of its race and for
 *  the host itself. */
export const HOST_SHUTDOWN_MARGIN_MS = 1_000;

/**
 * What the DELETE needs of the shutdown deadline once the settle is spent, to reach the wire: one
 * attempt, whose credential waits on half of it and whose request has the other half
 * (`credentialDeadline`, `@legion/envoy-client/bearer`), so a broker credential still waiting on a
 * trade leaves the request 375 ms to go without it.
 */
export const SHUTDOWN_DELETE_MS = 750;

/**
 * The whole of what the `session_shutdown` handler gives its deregistration and its drain. The
 * shared `SHUTDOWN_SETTLE_MS` (`@legion/envoy-client/envoy-settings`) is what that path spends
 * waiting on a registration still on the wire, and the rest of this deadline, at least
 * `SHUTDOWN_DELETE_MS`, is the DELETE's to reach the wire (`session-shutdown.test.ts` holds the
 * sum): `REGISTRATION_SETTLE_MS` is five times this whole deadline, so a registration slower than
 * that would mean no DELETE was sent at all and the record outlived this process by the gone
 * horizon, where a DELETE that races a slow registration only may lose.
 */
export const SHUTDOWN_DEADLINE_MS = HOST_SHUTDOWN_HANDLER_MS - HOST_SHUTDOWN_MARGIN_MS;

/** A warn line of the host's logger: its message and what it names. */
export type ShutdownWarn = (
  message: string,
  context: { readonly sessionID: string; readonly detail: string }
) => void;

/**
 * Waits for the session's DELETE and the NATS drain together, or for `deadline`, whichever comes
 * first. A DELETE that fails is logged at warn with the session id and its error; one still
 * pending when `deadline` comes is given up, and logged at warn with the session id and that it
 * was still pending at the deadline (a failure it meets later is not logged again). The drain's
 * own failure is the connection's, and the drain is given up at the deadline silently: the
 * connection closes with the process.
 */
export async function deregisterAndDrain(input: {
  readonly sessionID: string;
  readonly deregistration: Promise<void>;
  readonly draining: Promise<void>;
  readonly deadline: Promise<void>;
  readonly warn: ShutdownWarn;
}): Promise<void> {
  const { sessionID, warn } = input;
  let state: "pending" | "settled" | "given up" = "pending";
  const deregistered = input.deregistration.then(
    () => {
      state = "settled";
    },
    (error: unknown) => {
      if (state === "given up") return;
      state = "settled";
      warn("envoy: this session could not be deregistered at shutdown", {
        sessionID,
        detail: messageFor(error),
      });
    }
  );
  await Promise.race([
    Promise.allSettled([deregistered, input.draining.catch(() => undefined)]),
    input.deadline,
  ]);
  if (state !== "pending") return;
  state = "given up";
  warn("envoy: this session's deregistration was given up at the shutdown deadline", {
    sessionID,
    detail: `the DELETE was still pending ${SHUTDOWN_DEADLINE_MS} ms into the shutdown, so the session reads live until the listener's live window passes`,
  });
}
