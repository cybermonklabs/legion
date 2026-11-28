/**
 * What the test files of the report of model requests share: `./model-requests.test.ts`, the
 * hand-written rows, `./model-requests-stall.test.ts`, the rows of a daemon that never answers,
 * and `./model-requests-property.test.ts`, the seeded runs. The claims and the sessions a request
 * is reported under, the moment every clock starts at, an answered request as the host carries
 * it, the fake clock every reporter runs on, the one builder of the reporter (`reporterOn`), a turn
 * of the event loop (`turn`), and the rig of the hand-written rows (`rig`). It holds no test, so
 * `bun test` runs none of it.
 */
import { createLegionDaemonClient, type LegionDaemonClient } from "@legion/pi-shared/daemon-client";
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops";
import type { AgentEndMessage } from "@legion/pi-shared/pi-types";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import {
  createModelRequestsReporter,
  type ModelRequestCredential,
  type ModelRequestsLog,
  type ModelRequestsReporter,
} from "./model-requests";

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

/** Lets every callback already queued run: one turn of the event loop. */
export async function turn(): Promise<void> {
  const turned = Promise.withResolvers<void>();
  setImmediate(turned.resolve);
  await turned.promise;
}

/** The daemon's client a test's reporter calls, over `fetch`. */
export function daemonOver(fetch: typeof globalThis.fetch): LegionDaemonClient {
  return createLegionDaemonClient("http://daemon.test", { pluginContract: CONTRACT, fetch });
}

/** What a test's reporter may be given in place of the deps every test gives it alike. */
export interface ReporterOptions {
  /** The claim each top-level session holds: `credentialOf` where none is given. */
  readonly credential?: (sessionID: string) => ModelRequestCredential | undefined;
  /** Where it logs: nowhere where none is given. */
  readonly log?: ModelRequestsLog;
  /** Its timers: the clock's own (`FakeClock.schedule`) where none is given. */
  readonly schedule?: (callback: () => void, delayMs: number) => () => void;
}

/** A test's reporter, and how many ids it has given out: `req-1` to `req-${ids()}`. */
export interface TestReporter {
  readonly reporter: ModelRequestsReporter;
  readonly ids: () => number;
}

/** The reporter of every test: on `clock`, calling the daemon's client over `fetch` (`daemonOver`),
 * or the one `daemon` gives where a test holds its own, with the pane on the role's session and
 * the ids `req-1`, `req-2` and on. A new dep of the reporter is given here once. */
export function reporterOn(
  clock: FakeClock,
  calls: { readonly fetch: typeof fetch } | { readonly daemon: () => LegionDaemonClient },
  options: ReporterOptions = {}
): TestReporter {
  let ids = 0;
  let daemon: () => LegionDaemonClient;
  if ("daemon" in calls) {
    daemon = calls.daemon;
  } else {
    const client = daemonOver(calls.fetch);
    daemon = () => client;
  }
  const reporter = createModelRequestsReporter({
    daemon,
    credential: options.credential ?? credentialOf,
    paneSession: () => ROLE_SESSION,
    log: options.log ?? { debug: () => undefined, warn: () => undefined },
    now: clock.now,
    schedule: options.schedule ?? clock.schedule,
    newId: () => `req-${++ids}`,
  });
  return { reporter, ids: () => ids };
}

/** More warnings than any row writes (the rig's `log`). */
const WARNINGS_OF_A_LOOP = 1_000;

export interface Posted {
  readonly route: string;
  readonly body: Record<string, unknown>;
}

export interface LoggedWarning {
  readonly message: string;
  readonly context: Record<string, unknown> | undefined;
}

export interface Rig {
  readonly reporter: ModelRequestsReporter;
  /** Every body posted to the daemon, in the order it received them, by route. */
  readonly posted: Posted[];
  readonly warned: string[];
  readonly warnings: LoggedWarning[];
  /** The batches posted to `claims/model-requests`. */
  readonly batches: () => Record<string, unknown>[];
  /** Moves the clock on by `ms`, firing each timer that falls due, and waits for the sends they
   * start to be answered and handled; while the daemon holds an answer on the rig's clock
   * (`hold`), which only this moves, it lets every callback already queued run instead. */
  readonly advance: (ms: number) => Promise<void>;
  readonly now: () => number;
}

/**
 * The reporter (`reporterOn`) over a stub daemon. `answer` answers each report of model requests,
 * by the order it arrived in, with the ids the daemon keeps, and defaults to every id of the batch;
 * it may hold its answer back by returning a promise, and `hold(ms)` resolves once `advance` has
 * moved the clock `ms` on, which holds the answer that long on the rig's clock, or rejects with the
 * abort's reason once the report's request is aborted, as `fetch` does. The clock and the timers
 * are the rig's (`fakeClock`): nothing waits on the wall clock.
 */
export function rig(
  options: {
    readonly answer?: (
      body: Record<string, unknown>,
      attempt: number,
      hold: (ms: number) => Promise<void>
    ) => Promise<Response> | Response;
    readonly credential?: (sessionID: string) => ModelRequestCredential | undefined;
    readonly daemon?: () => LegionDaemonClient;
  } = {}
): Rig {
  const posted: Posted[] = [];
  const warned: string[] = [];
  const warnings: LoggedWarning[] = [];
  const clock = fakeClock();
  let attempts = 0;
  const stub = (async (input, init) => {
    const route = new URL(input.toString()).pathname;
    const body = JSON.parse(String(init?.body)) as Record<string, unknown>;
    posted.push({ route, body });
    if (route === "/legion/v1/claims/turn-outcome") {
      return Response.json({ refusedRun: 1, failed: false });
    }
    if (route !== "/legion/v1/claims/model-requests") throw new Error(`unexpected route ${route}`);
    attempts += 1;
    if (options.answer !== undefined) {
      return options.answer(body, attempts, (ms) => clock.hold(ms, init?.signal));
    }
    const requests = body.requests as { id: string }[];
    return Response.json({ kept: requests.map((request) => request.id) });
  }) as typeof fetch;
  const { reporter } = reporterOn(
    clock,
    options.daemon === undefined ? { fetch: stub } : { daemon: options.daemon },
    {
      credential: options.credential,
      log: {
        debug: () => undefined,
        warn: (message, context) => {
          warned.push(message);
          warnings.push({ message, context });
          // No row warns this often: a reporter that does is calling in a loop, and the throw ends
          // the loop, so the row fails rather than hang.
          if (warned.length > WARNINGS_OF_A_LOOP) {
            throw new Error(`the reporter warned ${warned.length} times: it is calling in a loop`);
          }
        },
      },
    }
  );
  /** Waits until every report on its way is answered and handled, but while the daemon holds an
   * answer on the rig's clock: that answer waits for `advance`, so this lets every callback
   * already queued run instead. */
  const handled = async (): Promise<void> => {
    if (clock.holding() === 0) return reporter.settled();
    await turn();
  };
  const advance = async (ms: number) => {
    await clock.advance(ms, async (fire) => {
      fire();
      await handled();
    });
    await handled();
  };
  return {
    reporter,
    posted,
    warned,
    warnings,
    batches: () =>
      posted
        .filter((post) => post.route === "/legion/v1/claims/model-requests")
        .map((post) => post.body),
    advance,
    now: clock.now,
  };
}
