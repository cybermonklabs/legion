import { dirname } from "node:path";
import { placeArming } from "@legion/test-fixtures/timer-record";

/** The root of nats.js's own code, as the frames of its stacks name it. */
export const NATS_ROOT = dirname(Bun.resolveSync("nats", import.meta.dir));

/** nats.js's own timers, held (`holdNatsTimers`). */
export interface HeldNatsTimers {
  /** Runs the oldest timer held. */
  runOldest(): void;
  /** Puts the process's `setTimeout` and `clearTimeout` back; a timer still held never runs. */
  release(): void;
}

/**
 * Holds nats.js's own timers until `release`: each timer nats.js's code arms (a handshake's, a
 * reconnect wait, a ping) runs only when the row runs it (`runOldest`), and is dropped when
 * nats.js clears it, so no real time decides what nats.js does: a handshake the row lets a server
 * answer completes before its timer, and a dial the row abandons is still on the address it was
 * on. Every other timer is the process's. A timer is nats.js's by the timer record's own rule
 * (`placeArming`), and a hold set before the record (`recordTimers`) leaves the record reading
 * each arming and each run.
 */
export function holdNatsTimers(): HeldNatsTimers {
  const real = { setTimeout: globalThis.setTimeout, clearTimeout: globalThis.clearTimeout };
  const held = new Map<object, () => void>();
  const ownFrame = (line: string) => line.includes(import.meta.path);
  globalThis.setTimeout = ((handler: TimerHandler, delay?: number, ...args: unknown[]) => {
    const stack = (new Error().stack ?? "").split("\n").filter((line) => !ownFrame(line));
    const place = placeArming(stack.join("\n"), [`${NATS_ROOT}/`]);
    if (typeof handler !== "function" || typeof place !== "object") {
      return real.setTimeout(handler, delay, ...args);
    }
    const handle = {};
    held.set(handle, () => {
      held.delete(handle);
      handler(...args);
    });
    return handle;
  }) as typeof setTimeout;
  globalThis.clearTimeout = ((handle: Parameters<typeof clearTimeout>[0]) => {
    if (typeof handle !== "object" || handle === null || !held.delete(handle)) {
      real.clearTimeout(handle);
    }
  }) as typeof clearTimeout;
  return {
    runOldest: () => {
      const [oldest] = held.values();
      if (oldest === undefined) throw new Error("nats.js holds no timer to run");
      oldest();
    },
    release: () => {
      globalThis.setTimeout = real.setTimeout;
      globalThis.clearTimeout = real.clearTimeout;
    },
  };
}
