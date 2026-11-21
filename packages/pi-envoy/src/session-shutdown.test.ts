import { expect, test } from "bun:test";
import { SHUTDOWN_SETTLE_MS } from "@legion/envoy-client/envoy-settings";
import {
  deregisterAndDrain,
  HOST_SHUTDOWN_HANDLER_MS,
  HOST_SHUTDOWN_MARGIN_MS,
  SHUTDOWN_DEADLINE_MS,
  SHUTDOWN_DELETE_MS,
  type ShutdownWarn,
} from "./session-shutdown";

/** A warn sink that keeps each line, and the call that runs the race with it. */
function shutdownRace(deregistration: Promise<void>, draining: Promise<void> = Promise.resolve()) {
  const warned: {
    readonly message: string;
    readonly sessionID: string;
    readonly detail: string;
  }[] = [];
  const warn: ShutdownWarn = (message, context) => warned.push({ message, ...context });
  const deadline = Promise.withResolvers<void>();
  const ran = deregisterAndDrain({
    sessionID: "ses_shutdown",
    deregistration,
    draining,
    deadline: deadline.promise,
    warn,
  });
  return { warned, deadline, ran };
}

test("the shutdown deadline is the host's limit less its margin, and holds the settle and the DELETE's own budget", () => {
  expect(SHUTDOWN_DEADLINE_MS).toBe(HOST_SHUTDOWN_HANDLER_MS - HOST_SHUTDOWN_MARGIN_MS);
  expect(SHUTDOWN_DEADLINE_MS).toBeLessThan(HOST_SHUTDOWN_HANDLER_MS);
  expect(SHUTDOWN_DEADLINE_MS).toBeGreaterThanOrEqual(SHUTDOWN_SETTLE_MS + SHUTDOWN_DELETE_MS);
});

test("a DELETE still pending at the deadline is given up and logged at warn with the session id", async () => {
  // The listener never answers: only the deadline ends the race.
  const deletion = Promise.withResolvers<void>();
  const race = shutdownRace(deletion.promise);
  race.deadline.resolve();
  await race.ran;
  expect(race.warned).toEqual([
    {
      message: "envoy: this session's deregistration was given up at the shutdown deadline",
      sessionID: "ses_shutdown",
      detail: `the DELETE was still pending ${SHUTDOWN_DEADLINE_MS} ms into the shutdown, so the session reads live until the listener's live window passes`,
    },
  ]);
  // The failure it meets once given up is not logged a second time.
  deletion.reject(new Error("aborted at its own deadline"));
  await deletion.promise.catch(() => undefined);
  await Promise.resolve();
  expect(race.warned).toHaveLength(1);
});

test("a DELETE that fails is logged at warn with the session id and its error", async () => {
  const race = shutdownRace(Promise.reject(new Error("listener cleanup failed")));
  await race.ran;
  expect(race.warned).toEqual([
    {
      message: "envoy: this session could not be deregistered at shutdown",
      sessionID: "ses_shutdown",
      detail: "listener cleanup failed",
    },
  ]);
});

test("a DELETE that lands logs nothing, and the race waits for the drain beside it", async () => {
  const drain = Promise.withResolvers<void>();
  const race = shutdownRace(Promise.resolve(), drain.promise);
  let ended = false;
  void race.ran.then(() => {
    ended = true;
  });
  // The drain still runs: a later action's effect (its own end) is what ends the race.
  await Promise.resolve();
  expect(ended).toBe(false);
  drain.resolve();
  await race.ran;
  expect(race.warned).toEqual([]);
});
