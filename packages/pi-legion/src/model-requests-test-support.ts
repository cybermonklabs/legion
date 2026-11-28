/**
 * What the two test files of the report of model requests share: `./model-requests.test.ts`, the
 * hand-written rows, and `./model-requests-property.test.ts`, the seeded runs. The claims and the
 * sessions a request is reported under, the moment every clock starts at, an answered request as
 * the host carries it, and the fake clock both drive the reporter on. It holds no test, so
 * `bun test` runs none of it.
 */
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops";
import type { AgentEndMessage } from "@legion/pi-shared/pi-types";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import type { ModelRequestCredential } from "./model-requests";

/** The contract this build states on every request (`Legion-Plugin-Contract`). */
export const CONTRACT = 60;

export const ROLE_SESSION = "ses_implementer_208";
export const SUBAGENT_SESSION = "ses_subagent_of_208";
export const CONTROLLER_SESSION = "ses_controller";

export const implementer: ClaimCapability = {
  sessionID: ROLE_SESSION,
  tree: "LEGION-208",
  issue: "LEGION-208",
  role: "implementer",
  secret: fixtureSecret("claim"),
  claimToken: "legion-legion-legion-208-implementer",
  generation: 3,
};

export const controller: ModelRequestCredential = {
  claimToken: "legion-legion-controller",
  sessionID: CONTROLLER_SESSION,
  secret: fixtureSecret("control"),
  generation: 5,
};

/** The claim each top-level session holds: the role's and the controller's, and none for any
 * other session (the reporter's `credential`). */
export function credentialOf(sessionID: string): ModelRequestCredential | undefined {
  return sessionID === ROLE_SESSION
    ? implementer
    : sessionID === CONTROLLER_SESSION
      ? controller
      : undefined;
}

export const role = { session: ROLE_SESSION, subagent: false } as const;

/** 2026-11-25T21:53:17.412Z, the moment every case's clock starts at. */
export const START = Date.parse("2026-11-25T21:53:17.412Z");

/** An answered request as Oh My Pi's `message_end` carries it, ended at `at`. */
export function answered(
  at: number,
  usage: AgentEndMessage["usage"] = { input: 11, output: 3, cacheRead: 100, cacheWrite: 7 },
  model = "standin-model"
): AgentEndMessage {
  return {
    role: "assistant",
    model,
    responseId: `msg_${at}`,
    stopReason: "stop",
    usage,
    completedAt: at,
  };
}

/** What fires on the fake clock: a timer the reporter armed (`schedule`), or the end of an answer
 * the daemon holds (`hold`). */
export type ClockFiring = "a timer" | "a daemon answer";

export interface FakeClock {
  readonly now: () => number;
  /** The reporter's `schedule`: runs `callback` once `advance` has moved the clock `delayMs` on, and
   * answers what cancels it. */
  readonly schedule: (callback: () => void, delayMs: number) => () => void;
  /** Holds a daemon's answer `ms` on the clock: resolves once `advance` has moved the clock `ms` on,
   * or, where `signal` aborts first, runs `onAbort` and rejects with the abort's reason, as `fetch`
   * does. */
  readonly hold: (
    ms: number,
    signal: AbortSignal | null | undefined,
    onAbort?: () => void
  ) => Promise<void>;
  /** How many answers `hold` holds now: neither released by `advance` nor aborted. */
  readonly holding: () => number;
  /** Moves the clock on by `ms`, firing each timer and held answer that falls due, in the order they
   * fall due, and in the order they were set where two fall due at once: the clock stands at each
   * one's moment while `each` runs it (`fire`) and waits for what it set off. */
  readonly advance: (
    ms: number,
    each: (fire: () => void, by: ClockFiring) => Promise<void>
  ) => Promise<void>;
}

/** A clock that starts at `START` and moves only by `advance`: nothing waits on the wall clock. */
export function fakeClock(): FakeClock {
  let clock = START;
  let timers: { due: number; callback: () => void; cancelled: boolean; by: ClockFiring }[] = [];
  let holding = 0;
  const set = (by: ClockFiring, delayMs: number, callback: () => void) => {
    const timer = { due: clock + delayMs, callback, cancelled: false, by };
    timers.push(timer);
    return timer;
  };
  return {
    now: () => clock,
    schedule: (callback, delayMs) => {
      const timer = set("a timer", delayMs, callback);
      return () => {
        timer.cancelled = true;
      };
    },
    hold: (ms, signal, onAbort) => {
      const held = Promise.withResolvers<void>();
      holding += 1;
      const timer = set("a daemon answer", ms, () => {
        holding -= 1;
        held.resolve();
      });
      signal?.addEventListener(
        "abort",
        () => {
          // An answer `advance` released already is no longer held.
          if (timer.cancelled) return;
          timer.cancelled = true;
          holding -= 1;
          onAbort?.();
          held.reject(signal.reason);
        },
        { once: true }
      );
      return held.promise;
    },
    holding: () => holding,
    advance: async (ms, each) => {
      const until = clock + ms;
      for (;;) {
        let next: (typeof timers)[number] | undefined;
        for (const timer of timers) {
          if (timer.cancelled || timer.due > until) continue;
          if (next === undefined || timer.due < next.due) next = timer;
        }
        if (next === undefined) break;
        clock = next.due;
        next.cancelled = true;
        await each(next.callback, next.by);
      }
      clock = until;
      timers = timers.filter((timer) => !timer.cancelled);
    },
  };
}
