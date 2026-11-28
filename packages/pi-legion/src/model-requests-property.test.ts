import { expect, test } from "bun:test";
import { LEGION_MAX_MODEL_REQUESTS } from "@legion/contracts/legion-api";
import { createLegionDaemonClient } from "@legion/pi-shared/daemon-client";
import {
  createModelRequestsReporter,
  MODEL_REQUEST_DUE_AFTER_MS,
  MODEL_REQUEST_REPORT_BOUND_MS,
  MODEL_REQUEST_REPORT_TIMEOUT_MS,
} from "./model-requests";
import {
  answered,
  type ClockFiring,
  CONTRACT,
  CONTROLLER_SESSION,
  controller,
  credentialOf,
  fakeClock,
  implementer,
  ROLE_SESSION,
  role,
  START,
  SUBAGENT_SESSION,
} from "./model-requests-test-support";

/** How long the reporter keeps a request for a retry, an hour (`MODEL_REQUEST_KEEP_MS` in
 * `./model-requests`). */
const KEEP_MS = 60 * 60_000;

/** How many seeded runs the case draws. */
const SEEDS = 200;

const GOES_AGAIN = "legion: a report of model requests did not reach the daemon; it goes again";
const UNREPORTED = "legion: the daemon refused a report of model requests; they go unreported";
const EXPIRED = "legion: dropped a model request past the daemon's keep window";
const DROPPED_INVALID = "legion: dropped a model request the contract refuses";

/** What makes a tick of a random run: a hook call, at a run's end or at a request's end, or a
 * firing of the run's clock, the reporter's timer or the end of an answer the daemon held. */
type TickCause = "a run's end" | "a request's end" | ClockFiring;

/** Each way a call of a random run can end, and how long the daemon or a proxy takes to answer it
 * on the run's clock (`ms`), always within the 10 s limit. */
type Fate =
  | { readonly kind: "client throws" }
  | { readonly kind: "client refuses" }
  | { readonly kind: "connection drops"; readonly ms: number }
  | { readonly kind: "daemon refuses"; readonly ms: number; readonly status: number }
  | { readonly kind: "proxy outage"; readonly ms: number; readonly status: number }
  | {
      readonly kind: "server error";
      readonly ms: number;
      readonly status: number;
      readonly shaped: boolean;
    }
  | { readonly kind: "no answer" }
  | { readonly kind: "answer"; readonly ms: number; readonly keepsAll: boolean };

/** Whether a call that ends so failed, so that the requests it put back wait a bound: every way
 * but the daemon's own refusal (a 4xx in its refusal shape) and its answer. */
function failedCall(fate: Fate): boolean {
  return fate.kind !== "daemon refuses" && fate.kind !== "answer";
}

/** A seeded PRNG (mulberry32): the same seed draws the same numbers, each in [0, 1). */
function seeded(seed: number): () => number {
  let state = seed >>> 0;
  return () => {
    state = (state + 0x6d2b79f5) >>> 0;
    let mixed = Math.imul(state ^ (state >>> 15), state | 1);
    mixed ^= mixed + Math.imul(mixed ^ (mixed >>> 7), mixed | 61);
    return ((mixed ^ (mixed >>> 14)) >>> 0) / 4_294_967_296;
  };
}

/** A request of a random run: its owner's claim token, when it ended and when the host read it,
 * its current deadline on the run's clock, when a call last put it back (if any), and how the run
 * is done with it — the daemon kept it, refused its report, or the reporter dropped it past the
 * keep window — when, and in which tick. */
interface RunRequest {
  readonly id: string;
  readonly owner: string;
  readonly ended: number;
  readonly read: number;
  due: number;
  lastPutBack?: number;
  gone?: {
    readonly how: "kept" | "refused" | "expired";
    readonly at: number;
    readonly tick: number;
  };
}

/** A tick of a random run, one hook call or one timer firing: what made it, when, the requests
 * pending as it began (none done with, none an open call carries) and whether one of them was due
 * then, and what it threw, if it did. */
interface RunTick {
  readonly by: TickCause;
  readonly at: number;
  readonly pending: readonly string[];
  readonly due: boolean;
  threw?: string;
}

/** A call of a random run: the tick that made it and what made that tick, when it started, how it
 * ends and whether that was drawn at random (and not the answer that ends a loop or the run), its
 * place among its tick's calls (1 for the first) and the most calls the requests pending at the
 * tick's first call allow, the ids it carries, whether its body reached the daemon and what was
 * open then, when and in which tick it ended, and the ids its answer kept. */
interface RunCall {
  readonly tick: number;
  readonly by: TickCause;
  readonly start: number;
  readonly fate: Fate;
  readonly drawn: boolean;
  readonly place: number;
  readonly bound: number;
  ids?: readonly string[];
  reached: boolean;
  reach?: RunReach;
  end?: number;
  endTick?: number;
  kept?: ReadonlySet<string>;
}

/** The moment a call's body reached the daemon: when, the calls open then, itself among them, the
 * batches those calls carry of requests read and last put back before the last
 * `MODEL_REQUEST_REPORT_TIMEOUT_MS` (`base`), and the refills since (`refills`). */
interface RunReach {
  readonly at: number;
  readonly open: readonly RunCall[];
  readonly base: number;
  readonly refills: number;
}

/** What one seeded run did, which the checks read once it is over: its requests by id, its calls in
 * the order they were made, its ticks by number, each request the reporter dropped past the keep
 * window and when, every warning it logged, and how many requests it ended that the contract
 * refuses. */
interface RunRecord {
  readonly seed: number;
  readonly requests: ReadonlyMap<string, RunRequest>;
  readonly calls: readonly RunCall[];
  readonly ticks: ReadonlyMap<number, RunTick>;
  readonly expired: readonly { readonly id: string; readonly at: number }[];
  readonly warnings: readonly string[];
  readonly invalid: number;
}

/** One seeded run of the reporter over the real client, on a clock of its own, as the case below
 * draws it, and its record. */
async function randomRun(seed: number): Promise<RunRecord> {
  const random = seeded(seed);
  const clock = fakeClock();
  /** The hook call or timer firing that runs now: its number, and its record. */
  let tick = 0;
  let current: RunTick | undefined;
  let draining = false;
  const requests = new Map<string, RunRequest>();
  const calls: RunCall[] = [];
  const ticks = new Map<number, RunTick>();
  const expired: { readonly id: string; readonly at: number }[] = [];
  const warnings: string[] = [];
  /** Each request read, partial answer put-back, or failed call put-back: what can add a batch to
   * `pending` inside a `MODEL_REQUEST_REPORT_TIMEOUT_MS` window (check 2). */
  const refills: number[] = [];
  /** Each tick's calls so far, and the most calls the requests pending at its first call allow. */
  const sends = new Map<number, { calls: number; bound: number }>();
  let callsEnded = 0;
  let callsHandled = 0;
  let invalid = 0;
  let nextId = 0;

  const finish = (call: RunCall): void => {
    call.end = clock.now();
    call.endTick = tick;
    callsEnded += 1;
  };
  const putBack = (ids: readonly string[]): void => {
    const now = clock.now();
    for (const id of ids) {
      const request = requests.get(id);
      if (request !== undefined && request.gone === undefined) {
        request.due = now + MODEL_REQUEST_REPORT_BOUND_MS;
        request.lastPutBack = now;
      }
    }
    if (ids.length > 0) refills.push(now);
  };
  const done = (ids: readonly string[], how: "kept" | "refused" | "expired"): void => {
    for (const id of ids) {
      const request = requests.get(id);
      if (request !== undefined && request.gone === undefined) {
        request.gone = { how, at: clock.now(), tick };
      }
    }
  };
  const open = (): RunCall[] => calls.filter((call) => call.reached && call.end === undefined);
  const pendingNow = (): RunRequest[] => {
    const carried = new Set(open().flatMap((call) => call.ids ?? []));
    return [...requests.values()].filter(
      (request) => request.gone === undefined && !carried.has(request.id)
    );
  };
  const callBoundFor = (pending: readonly RunRequest[]): number => {
    const byOwner = new Map<string, number>();
    for (const request of pending) {
      byOwner.set(request.owner, (byOwner.get(request.owner) ?? 0) + 1);
    }
    let bound = 0;
    for (const count of byOwner.values()) {
      bound += Math.ceil(count / LEGION_MAX_MODEL_REQUESTS);
    }
    return bound;
  };
  const pick = (choices: readonly number[]): number =>
    choices[Math.floor(random() * choices.length)] ?? Number.NaN;
  const drawFate = (): Fate => {
    const ms = Math.floor(random() * MODEL_REQUEST_REPORT_TIMEOUT_MS);
    switch (Math.floor(random() * 10)) {
      case 0:
        return { kind: "client throws" };
      case 1:
        return { kind: "client refuses" };
      case 2:
        return { kind: "connection drops", ms };
      case 3:
        return { kind: "daemon refuses", ms, status: pick([400, 401, 403, 404, 409, 422]) };
      case 4:
        return { kind: "proxy outage", ms, status: pick([408, 429]) };
      case 5:
        return {
          kind: "server error",
          ms,
          status: pick([500, 502, 503, 504]),
          shaped: random() < 0.5,
        };
      case 6:
        return { kind: "no answer" };
      default:
        return { kind: "answer", ms, keepsAll: random() < 0.6 };
    }
  };
  const fetchFor = (call: RunCall): typeof fetch =>
    (async (_input, init) => {
      const ids = (JSON.parse(String(init?.body)).requests as { id: string }[]).map(({ id }) => id);
      call.ids = ids;
      call.reached = true;
      const now = clock.now();
      const since = now - MODEL_REQUEST_REPORT_TIMEOUT_MS;
      const openCalls = open();
      // Every open call began in [since, clock], carrying requests that were either already
      // pending before `since` (bounded by ceil(count / LEGION_MAX_MODEL_REQUESTS) per owner) or
      // added/put back in [since, clock] (at most 1 batch per refill).
      const oldByOwner = new Map<string, number>();
      for (const active of openCalls) {
        for (const id of active.ids ?? []) {
          const request = requests.get(id);
          if (
            request !== undefined &&
            request.read < since &&
            (request.lastPutBack === undefined || request.lastPutBack < since)
          ) {
            oldByOwner.set(request.owner, (oldByOwner.get(request.owner) ?? 0) + 1);
          }
        }
      }
      let base = 0;
      for (const count of oldByOwner.values()) {
        base += Math.ceil(count / LEGION_MAX_MODEL_REQUESTS);
      }
      const recentRefills = refills.filter((refill) => refill >= since).length;
      call.reach = { at: now, open: openCalls, base, refills: recentRefills };
      const { fate } = call;
      // The daemon holds its answer `ms`, or, giving none, until the call's abort ends the call, a
      // failed call whose requests go back.
      await clock.hold("ms" in fate ? fate.ms : Number.POSITIVE_INFINITY, init?.signal, () => {
        finish(call);
        putBack(ids);
      });
      finish(call);
      switch (fate.kind) {
        case "connection drops":
          putBack(ids);
          throw new TypeError("fetch failed");
        case "daemon refuses":
          done(ids, "refused");
          return Response.json(
            { error: "the daemon refuses this report" },
            { status: fate.status }
          );
        case "proxy outage":
          putBack(ids);
          return new Response(`<html><body><h1>${fate.status}</h1></body></html>`, {
            status: fate.status,
            headers: { "Content-Type": "text/html" },
          });
        case "server error":
          putBack(ids);
          return fate.shaped
            ? Response.json(
                { error: "report of model requests failed: the daemon could not keep it" },
                { status: fate.status }
              )
            : new Response("<html><body><h1>Bad Gateway</h1></body></html>", {
                status: fate.status,
              });
        case "answer": {
          const kept = fate.keepsAll ? ids : ids.filter(() => random() < 0.5);
          call.kept = new Set(kept);
          done(kept, "kept");
          const unheld = ids.filter((id) => !call.kept?.has(id));
          putBack(unheld);
          return Response.json({ kept });
        }
        default:
          throw new Error(`a call whose fate is "${fate.kind}" reached the daemon`);
      }
    }) as typeof fetch;
  const reporter = createModelRequestsReporter({
    daemon: () => {
      if (current === undefined) {
        throw new Error(`seed ${seed}: the reporter called the daemon outside a tick`);
      }
      let send = sends.get(tick);
      if (send === undefined) {
        send = { calls: 0, bound: callBoundFor(pendingNow()) };
        sends.set(tick, send);
      }
      send.calls += 1;
      // Past the bound, the send took again a batch a call put back before it left (check 1): from
      // then on the tick's calls are answered, ending any loop.
      const looping = send.calls > send.bound;
      const drawn = !draining && !looping;
      const fate: Fate = drawn ? drawFate() : { kind: "answer", ms: 0, keepsAll: true };
      const call: RunCall = {
        tick,
        by: current.by,
        start: clock.now(),
        fate,
        drawn,
        place: send.calls,
        bound: send.bound,
        reached: false,
      };
      calls.push(call);
      const client = createLegionDaemonClient("http://daemon.test", {
        pluginContract: CONTRACT,
        fetch: fetchFor(call),
      });
      if (looping) {
        // Answer asynchronously keeping every id without parsing the body, so a reporter that
        // loops on a synchronous refusal cannot keep putting the same batch back.
        return {
          ...client,
          modelRequests: async (input) => {
            const kept = input.requests.map(({ id }) => id);
            call.ids = kept;
            call.kept = new Set(kept);
            done(kept, "kept");
            finish(call);
            return { kept };
          },
        };
      }
      if (fate.kind === "client throws") {
        return {
          ...client,
          // A throw before anything leaves, which the reporter meets inside the call's `try` before
          // its first `await`, as it meets one from `deps.daemon()`.
          modelRequests: (input) => {
            const ids = input.requests.map(({ id }) => id);
            call.ids = ids;
            finish(call);
            putBack(ids);
            throw new Error("the daemon client threw");
          },
        };
      }
      if (fate.kind === "client refuses") {
        return {
          ...client,
          // A body the contract refuses: its requests name no session.
          modelRequests: (input, options) => {
            const ids = input.requests.map(({ id }) => id);
            call.ids = ids;
            finish(call);
            putBack(ids);
            const requests = input.requests.map((request) => ({ ...request, session: "" }));
            return client.modelRequests({ ...input, requests }, options);
          },
        };
      }
      return {
        ...client,
        modelRequests: (input, options) => {
          try {
            return client.modelRequests(input, options).catch((error: unknown) => {
              if (call.end === undefined) finish(call);
              throw error;
            });
          } catch (error) {
            if (call.end === undefined) finish(call);
            throw error;
          }
        },
      };
    },
    credential: credentialOf,
    paneSession: () => ROLE_SESSION,
    log: {
      debug: () => {
        callsHandled += 1;
      },
      warn: (message, context) => {
        warnings.push(message);
        if (message === DROPPED_INVALID) return;
        if (message !== EXPIRED) {
          callsHandled += 1;
          return;
        }
        const id = String(context?.id);
        expired.push({ id, at: clock.now() });
        done([id], "expired");
      },
    },
    now: clock.now,
    schedule: clock.schedule,
    newId: () => `req-${++nextId}`,
  });
  /** Waits until the reporter has handled the end of each call that ended, one log line each. */
  const quiet = async (): Promise<void> => {
    for (let turn = 0; turn < 1_000; turn += 1) {
      const turned = Promise.withResolvers<void>();
      setImmediate(turned.resolve);
      await turned.promise;
      if (callsHandled === callsEnded) return;
    }
    throw new Error(
      `seed ${seed}: the reporter handled ${callsHandled} of the ${callsEnded} calls that ended`
    );
  };
  /** Runs one hook call or timer firing as a tick of its own, records it and what it threw, and
   * waits for what it set off. */
  const hook = async (by: TickCause, run: () => void): Promise<void> => {
    tick += 1;
    const now = clock.now();
    const pending = pendingNow();
    const record: RunTick = {
      by,
      at: now,
      pending: pending.map(({ id }) => id),
      due: pending.some((request) => request.due <= now),
    };
    ticks.set(tick, record);
    current = record;
    try {
      run();
    } catch (error) {
      record.threw = String(error);
    }
    await quiet();
  };
  /** Moves the clock on by `ms`, each timer that falls due firing as a tick of its own. */
  const advance = (ms: number): Promise<void> => clock.advance(ms, (fire, by) => hook(by, fire));

  const subagent = { session: SUBAGENT_SESSION, subagent: true } as const;
  const controllerPane = { session: CONTROLLER_SESSION, subagent: false } as const;
  const recordMessage = async (
    where: { readonly session: string; readonly subagent: boolean },
    ended: number
  ): Promise<void> => {
    await hook("a request's end", () => {
      const before = nextId;
      reporter.messageEnded(where, answered(ended));
      if (nextId === before) return;
      if (where.session === "") {
        invalid += 1;
        return;
      }
      const id = `req-${nextId}`;
      const owner = where === controllerPane ? controller : implementer;
      requests.set(id, {
        id,
        owner: owner.claimToken,
        ended,
        read: clock.now(),
        due: ended + MODEL_REQUEST_DUE_AFTER_MS,
      });
      refills.push(clock.now());
    });
  };

  const steps = 5 + Math.floor(random() * 60);
  // Every 20th seed injects a burst past `LEGION_MAX_MODEL_REQUESTS` (200) of one owner so batch
  // splitting is exercised alongside normal runs.
  const burstStep = seed % 20 === 0 ? Math.floor(random() * steps) : -1;
  for (let step = 0; step < steps; step += 1) {
    if (step === burstStep) {
      const burst = LEGION_MAX_MODEL_REQUESTS + 5 + Math.floor(random() * 40);
      for (let i = 0; i < burst; i += 1) {
        await recordMessage(i % 2 === 0 ? role : subagent, clock.now());
      }
    }
    if (random() < 0.6) {
      const who = random();
      const where =
        who < 0.03
          ? ({ session: "", subagent: true } as const)
          : who < 0.45
            ? role
            : who < 0.7
              ? subagent
              : controllerPane;
      const lateness = random();
      const ended =
        lateness < 0.7
          ? clock.now()
          : lateness < 0.97
            ? clock.now() - Math.floor(random() * 13_001)
            : clock.now() - KEEP_MS + Math.floor(random() * 20_000);
      await recordMessage(where, ended);
    }
    if (random() < 0.3) await hook("a run's end", () => reporter.turnEnded());
    const pace = random();
    await advance(1 + Math.floor(random() * (pace < 0.4 ? 500 : pace < 0.9 ? 5_000 : 20_000)));
  }
  // The daemon answers each call at once and keeps every id, for long enough that each open call
  // ends, each put-back's hold ends and each request pending falls due.
  draining = true;
  await advance(2 * (MODEL_REQUEST_REPORT_TIMEOUT_MS + MODEL_REQUEST_REPORT_BOUND_MS));
  return { seed, requests, calls, ticks, expired, warnings, invalid };
}

/** A moment of a run as a problem names it: how long after the run's start. */
function at(time: number): string {
  return `${time - START} ms`;
}

/** The calls of a run by the tick that made them. */
function callsByTick(calls: readonly RunCall[]): Map<number, RunCall[]> {
  const byTick = new Map<number, RunCall[]>();
  for (const call of calls) {
    const atTick = byTick.get(call.tick) ?? [];
    atTick.push(call);
    byTick.set(call.tick, atTick);
  }
  return byTick;
}

/** The calls of a run by the id of each request they carry, in the order they were made: a lookup
 * of its own, since burst seeds exceed 200 requests. */
function callsByRequest(calls: readonly RunCall[]): Map<string, RunCall[]> {
  const byRequest = new Map<string, RunCall[]>();
  for (const call of calls) {
    for (const id of call.ids ?? []) {
      const list = byRequest.get(id) ?? [];
      list.push(call);
      byRequest.set(id, list);
    }
  }
  return byRequest;
}

/** How a request is done with, as a problem says it. */
const WHY_GONE = {
  kept: "the daemon kept it",
  refused: "the daemon refused its report",
  expired: "the reporter dropped it past the keep window",
} as const;

/** Check 1. Each hook call and timer firing returns, makes at most
 * `ceil(pending / LEGION_MAX_MODEL_REQUESTS)` calls per owner with requests pending, and only a
 * run's end or a timer with a due request (a fresh deadline or a put-back's due) makes a send: a
 * failing daemon is never called in a loop. */
function sendsReturnAndNeverLoop(run: RunRecord): string[] {
  const problems: string[] = [];
  for (const { by, at: time, threw } of run.ticks.values()) {
    if (threw !== undefined) problems.push(`${by} at ${at(time)} threw: ${threw}`);
  }
  for (const call of run.calls) {
    const due = run.ticks.get(call.tick)?.due === true;
    if (call.place === 1 && call.by !== "a run's end" && (call.by !== "a timer" || !due)) {
      problems.push(
        `${call.by} at ${at(call.start)} made a call when neither a run's end nor a due request triggered it`
      );
    }
    if (call.place === call.bound + 1) {
      problems.push(
        `${call.by} at ${at(call.start)} made more calls than the ${call.bound} batches pending requests allow: it took again a batch a call put back before it left`
      );
    }
  }
  return problems;
}

/** Check 2. Each open call ends by `MODEL_REQUEST_REPORT_TIMEOUT_MS`, and open calls never
 * outnumber the batches carried from before the last 10 s window plus the refills (requests read,
 * partial answers, and failed-call put-backs) in that window. The bound reads only the last 10 s,
 * so open calls do not grow with the run. */
function openCallsEndAndStayBounded(run: RunRecord): string[] {
  const problems: string[] = [];
  for (const { reach } of run.calls) {
    if (reach === undefined) continue;
    for (const active of reach.open) {
      if (reach.at - active.start > MODEL_REQUEST_REPORT_TIMEOUT_MS) {
        problems.push(
          `the call ${active.by} made at ${at(active.start)} was still open at ${at(reach.at)}, past the timeout`
        );
      }
    }
    const bound = reach.base + reach.refills;
    if (reach.open.length > bound) {
      problems.push(
        `${reach.open.length} calls were open at ${at(reach.at)}, above the bound of ${bound}: ${reach.base} from earlier batches, and ${reach.refills} refills since ${at(reach.at - MODEL_REQUEST_REPORT_TIMEOUT_MS)}`
      );
    }
  }
  for (const call of run.calls) {
    if (call.reached && call.end === undefined) {
      problems.push(
        `the call ${call.by} made at ${at(call.start)} was still open at the run's end`
      );
    }
  }
  return problems;
}

/** Check 3. Each request no call carried goes by its own deadline — its end plus
 * `MODEL_REQUEST_DUE_AFTER_MS`, within 15 s of its end, or at once where it was read later —
 * whatever calls failed; a request a call put back goes again no later than a bound after that
 * call ended; and a run's end sends everything pending at once, whatever failed before. A request
 * goes no more once the daemon refused its report or once it was dropped past the keep window,
 * which happens only past it. Each failed call and each refusal warns once, and the run's end
 * finds each request kept, refused or dropped. */
function requestsGoInTime(run: RunRecord): string[] {
  const problems: string[] = [];
  const byTick = callsByTick(run.calls);
  for (const [tick, { by, at: time, pending }] of run.ticks) {
    if (by !== "a run's end") continue;
    const sentInTick = new Set((byTick.get(tick) ?? []).flatMap((call) => call.ids ?? []));
    for (const id of pending) {
      const request = run.requests.get(id);
      // A request that send dropped past the keep window leaves not at all.
      const expiredNow = request?.gone?.how === "expired" && request.gone.tick === tick;
      if (!expiredNow && !sentInTick.has(id)) {
        problems.push(`${id} was pending at a run's end at ${at(time)}, and did not leave at once`);
      }
    }
  }
  for (const { id, at: dropped } of run.expired) {
    const request = run.requests.get(id);
    if (request !== undefined && dropped - request.ended <= KEEP_MS) {
      problems.push(
        `${id}, which ended at ${at(request.ended)}, was dropped at ${at(dropped)}, inside the keep window`
      );
    }
  }
  const byRequest = callsByRequest(run.calls);
  for (const request of run.requests.values()) {
    const { id, gone } = request;
    // Its deadline: its end plus the due delay, or its read where that came later; once a call
    // that carried it ended, a bound after that end. No failed call of other requests excuses a
    // missed deadline.
    let due = Math.max(request.ended + MODEL_REQUEST_DUE_AFTER_MS, request.read);
    for (const call of byRequest.get(id) ?? []) {
      if (gone !== undefined && call.tick > gone.tick) {
        // Going again once the daemon kept it is check 4's.
        if (gone.how !== "kept") {
          problems.push(
            `${id} went again at ${at(call.start)}, though ${WHY_GONE[gone.how]} at ${at(gone.at)}`
          );
        }
        break;
      }
      if (call.start > due) {
        problems.push(`${id} went at ${at(call.start)}, past its deadline at ${at(due)}`);
      }
      if (call.start - request.ended > KEEP_MS) {
        problems.push(`${id} went at ${at(call.start)}, past the keep window of its end`);
      }
      due = (call.end ?? Number.POSITIVE_INFINITY) + MODEL_REQUEST_REPORT_BOUND_MS;
    }
    if (gone === undefined) {
      problems.push(
        `${id}, which ended at ${at(request.ended)}, was neither kept, refused nor dropped by the run's end`
      );
    } else if (gone.how === "expired" && gone.at > due) {
      problems.push(`${id} was dropped at ${at(gone.at)}, past its deadline at ${at(due)}`);
    }
  }
  const warned = (message: string): number =>
    run.warnings.filter((warning) => warning === message).length;
  const failed = run.calls.filter((call) => failedCall(call.fate)).length;
  const refused = run.calls.filter((call) => call.fate.kind === "daemon refuses").length;
  if (warned(GOES_AGAIN) !== failed) {
    problems.push(
      `the reporter warned ${warned(GOES_AGAIN)} times that a report goes again, for ${failed} failed calls`
    );
  }
  if (warned(UNREPORTED) !== refused) {
    problems.push(
      `the reporter warned ${warned(UNREPORTED)} times that a report goes unreported, for ${refused} refused calls`
    );
  }
  if (warned(DROPPED_INVALID) !== run.invalid) {
    problems.push(
      `the reporter warned ${warned(DROPPED_INVALID)} times that a request was refused by the contract, for ${run.invalid} invalid requests`
    );
  }
  return problems;
}

/** Check 4. No request goes again while a call that carries it is open, nor once the daemon kept
 * it: it goes again only after a failed call or an answer that did not keep its id. */
function requestsGoAgainOnlyOnARetry(run: RunRecord): string[] {
  const problems: string[] = [];
  const byRequest = callsByRequest(run.calls);
  for (const { id, gone } of run.requests.values()) {
    let carrying: RunCall | undefined;
    for (const call of byRequest.get(id) ?? []) {
      if (gone !== undefined && call.tick > gone.tick) {
        // Going again once its report was refused or it was dropped is check 3's.
        if (gone.how === "kept") {
          problems.push(
            `${id} went again at ${at(call.start)}, though ${WHY_GONE.kept} at ${at(gone.at)}`
          );
        }
        break;
      }
      if (!call.reached) continue;
      if (carrying !== undefined && (carrying.endTick ?? Number.POSITIVE_INFINITY) >= call.tick) {
        problems.push(
          `${id} went at ${at(call.start)} while the call made at ${at(carrying.start)}, which carries it, was open`
        );
      }
      carrying = call;
    }
  }
  return problems;
}

/** The checks every seeded run holds, in the order their problems number them. */
const CHECKS: readonly ((run: RunRecord) => string[])[] = [
  sendsReturnAndNeverLoop,
  openCallsEndAndStayBounded,
  requestsGoInTime,
  requestsGoAgainOnlyOnARetry,
];

/** The first three problems the checks find in `run`, each naming its seed and its check. */
function problemsOf(run: RunRecord): string[] {
  return CHECKS.flatMap((check, index) =>
    check(run).map((text) => `seed ${run.seed}, check ${index + 1}: ${text}`)
  ).slice(0, 3);
}

/** What the seeds exercised, each of which the case asserts occurs: a request a timer sent first
 * during a failed call's hold, a run's end that sent during one, a send that split an owner's
 * requests past `LEGION_MAX_MODEL_REQUESTS`, a request the contract refuses, and each way a call
 * can end, counted over the calls whose way was drawn. */
interface RunCoverage {
  sentDuringHold: number;
  turnEndsDuringHold: number;
  splitBatches: number;
  invalidDropped: number;
  readonly fates: Record<Fate["kind"], number>;
}

/** Adds what `run` exercised to `coverage`. */
function cover(run: RunRecord, coverage: RunCoverage): void {
  // Failed-call hold spans, used only to verify that the generator exercises fresh sends and run
  // ends during a hold (not to excuse delays).
  const holds = run.calls.flatMap((call) =>
    failedCall(call.fate) && call.end !== undefined && call.endTick !== undefined
      ? [{ from: call.end, until: call.end + MODEL_REQUEST_REPORT_BOUND_MS, tick: call.endTick }]
      : []
  );
  const duringHold = (time: number, callTick: number): boolean =>
    holds.some((span) => span.tick < callTick && span.from < time && time < span.until);
  const byTick = callsByTick(run.calls);
  for (const [tick, { by, at: time, pending }] of run.ticks) {
    const tickCalls = byTick.get(tick);
    if (tickCalls === undefined) continue;
    const owners = new Map<string, number>();
    for (const id of pending) {
      const owner = run.requests.get(id)?.owner ?? "";
      owners.set(owner, (owners.get(owner) ?? 0) + 1);
    }
    for (const count of owners.values()) {
      if (count > LEGION_MAX_MODEL_REQUESTS) coverage.splitBatches += 1;
    }
    if (
      by === "a run's end" &&
      pending.length > 0 &&
      duringHold(time, tick) &&
      tickCalls.some((call) => (call.ids?.length ?? 0) > 0)
    ) {
      coverage.turnEndsDuringHold += 1;
    }
  }
  const byRequest = callsByRequest(run.calls);
  for (const id of run.requests.keys()) {
    const first = byRequest.get(id)?.[0];
    if (first !== undefined && first.by === "a timer" && duringHold(first.start, first.tick)) {
      coverage.sentDuringHold += 1;
    }
  }
  coverage.invalidDropped += run.invalid;
  for (const call of run.calls) {
    if (call.drawn) coverage.fates[call.fate.kind] += 1;
  }
}

// Seeded random runs of the reporter over the real client on a clock of their own. Each seed draws
// a run of its own length: requests that end at random times, some read late, some a second
// owner's, a few read so late that a retry finds them past the keep window, and every 20th seed a
// burst past `LEGION_MAX_MODEL_REQUESTS` (200) of one owner; run ends at random times; and the
// timer, firing as the clock moves. Each call ends in one way drawn at random: the client throws,
// or refuses the body, before anything leaves; the connection drops; the daemon refuses with a 4xx
// in its refusal shape; a proxy answers 408 or 429; a 5xx; the 10 s limit aborts it; or an answer
// within the limit keeps every id or some. A run ends with a daemon that answers at once and keeps
// every id, until each request is done with. Every run holds each check of `CHECKS`, whose function
// states its rule, and across the seeds each case `RunCoverage` names occurs.
test("seeded random runs: each call returns, the open calls stay bounded, each request goes in time and again only on a retry", async () => {
  const problems: string[] = [];
  const coverage: RunCoverage = {
    sentDuringHold: 0,
    turnEndsDuringHold: 0,
    splitBatches: 0,
    invalidDropped: 0,
    fates: {
      "client throws": 0,
      "client refuses": 0,
      "connection drops": 0,
      "daemon refuses": 0,
      "proxy outage": 0,
      "server error": 0,
      "no answer": 0,
      answer: 0,
    },
  };
  for (let seed = 1; seed <= SEEDS; seed += 1) {
    const run = await randomRun(seed);
    problems.push(...problemsOf(run));
    cover(run, coverage);
  }
  expect(problems).toEqual([]);
  expect(coverage.sentDuringHold).toBeGreaterThan(0);
  expect(coverage.turnEndsDuringHold).toBeGreaterThan(0);
  expect(coverage.splitBatches).toBeGreaterThan(0);
  expect(coverage.invalidDropped).toBeGreaterThan(0);
  for (const [kind, count] of Object.entries(coverage.fates)) {
    expect(count, `the calls drawn to end so: ${kind}`).toBeGreaterThan(0);
  }
});
