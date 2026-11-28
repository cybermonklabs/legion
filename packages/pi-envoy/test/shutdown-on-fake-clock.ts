// The one way the suite times a `session_shutdown` handler: `extensions/envoy.test.ts` times the
// Envoy entry's through it, and `extensions/askgate/askgate.test.ts` the gate's.
import { jest } from "bun:test";
import { turnsUntil } from "@legion/test-fixtures/fake-clock";
import { timeLeft } from "@legion/test-fixtures/wait";
import { HOST_SHUTDOWN_HANDLER_MS } from "../src/session-shutdown";

/**
 * Runs `shutdown` on Bun's fake clock, moved a millisecond a turn, until it settles, and answers
 * when it began and how far the clock had moved when it settled. The handler's waits are its own
 * timers, so the reading is theirs to the millisecond however loaded the machine, while the
 * stand-ins answer on turns of the real event loop. The clock stops at the host's limit
 * (`HOST_SHUTDOWN_HANDLER_MS`, past which Oh My Pi abandons the handler), and a handler still
 * running there reads as that limit. The wait's one bound is the row's deadline, read before the
 * clock is held.
 */
export async function shutdownOnFakeClock(
  shutdown: () => Promise<unknown> | undefined
): Promise<{ readonly began: number; readonly tookMs: number }> {
  const boundMs = Math.floor(timeLeft());
  jest.useFakeTimers();
  try {
    const began = Date.now();
    let done = false;
    void shutdown()?.then(() => {
      done = true;
    });
    await turnsUntil(
      () => done || Date.now() - began >= HOST_SHUTDOWN_HANDLER_MS,
      "the session_shutdown handler to settle, or the clock to reach the host's limit",
      { boundMs, each: () => jest.advanceTimersByTime(1) }
    );
    return { began, tookMs: done ? Date.now() - began : HOST_SHUTDOWN_HANDLER_MS };
  } finally {
    jest.useRealTimers();
  }
}
