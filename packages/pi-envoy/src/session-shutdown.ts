/**
 * The budget of the extension's stop (`stopSession`), and the race its deregistration and NATS
 * drain run in: a session whose DELETE never reaches the listener reads live for the live window
 * and unresponsive for the gone horizon after it, with nothing heartbeating it, so a DELETE that
 * fails or is given up says so at warn, naming the session.
 */
import { messageFor } from "@legion/envoy-client/errors";

/** How long Oh My Pi gives a `session_shutdown` handler before it abandons it and seals the
 *  session (`SESSION_SHUTDOWN_HANDLER_TIMEOUT_MS` in the host's extension runner). A SIGTERM gets
 *  no more: in a Legion pane the shim kills Oh My Pi 5 s after its SIGTERM (`DefaultGrace`,
 *  `packages/daemon/internal/shim`), and the agent shares that grace. */
export const HOST_SHUTDOWN_HANDLER_MS = 2_000;

/** What the stop leaves of the host's limit for the synchronous work ahead of its race and for
 *  the host itself. */
export const HOST_SHUTDOWN_MARGIN_MS = 1_000;

/** How long the DELETE of a stop waits for the release of the session's role claims, which goes
 *  first: a quarter of a second, as the wait on a registration still on the wire. A listener
 *  that answers at all answers a release in tens of milliseconds. A release that has not
 *  answered by then is no longer waited for: the DELETE goes, and nothing sends the release
 *  again. That release can still land while the process lives; when it does not, the claim
 *  stands until its last renewal is older than the listener's live window. The DELETE goes all
 *  the same, because it is what lets a message to the stopped session wake it. */
export const SHUTDOWN_RELEASE_MS = 250;

/**
 * What the DELETE needs of the shutdown deadline once the release and the settle are spent, to
 * reach the wire: one attempt, whose credential waits on half of it and whose request has the
 * other half (`credentialDeadline`, `@legion/envoy-client/bearer`), so a broker credential still
 * waiting on a trade leaves the request 250 ms to go without it.
 */
export const SHUTDOWN_DELETE_MS = 500;

/**
 * The whole of what a stop gives its role release, its deregistration and its drain. The release
 * goes first, for at most `SHUTDOWN_RELEASE_MS`; the shared `SHUTDOWN_SETTLE_MS`
 * (`@legion/envoy-client/envoy-settings`) is what the stop then spends waiting on a registration
 * still on the wire, and the rest of this deadline, at least `SHUTDOWN_DELETE_MS`, is the
 * DELETE's to reach the wire (`session-shutdown.test.ts` holds the sum): `REGISTRATION_SETTLE_MS`
 * is five times this whole deadline, so a registration slower than that would mean no DELETE was
 * sent at all and the record outlived this process by the gone horizon, where a DELETE that races
 * a slow registration only may lose.
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
