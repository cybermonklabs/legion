import { expect, test } from "bun:test";
import { LEGION_MAX_MODEL_REQUESTS } from "@legion/contracts/legion-api";
import type { LegionDaemonClient } from "@legion/pi-shared/daemon-client";
import {
  MODEL_REQUEST_DUE_AFTER_MS,
  MODEL_REQUEST_LEFT_DUE_AFTER_MS,
  MODEL_REQUEST_REPORT_BOUND_MS,
  MODEL_REQUEST_REPORT_TIMEOUT_MS,
} from "./model-requests";
import {
  answered,
  type ClockFiring,
  CONTROLLER_SESSION,
  controller,
  daemonOver,
  fakeClock,
  implementer,
  reporterOn,
  role,
  START,
  SUBAGENT_SESSION,
  turn,
} from "./model-requests-test-support";

/** How long the reporter keeps a request for a retry, an hour (`MODEL_REQUEST_KEEP_MS` in
 * `./model-requests`). */
const KEEP_MS = 60 * 60_000;

/** How many seeded runs the case draws. */
const SEEDS = 200;

const GOES_AGAIN = "legion: a report of model requests did not reach the daemon; it goes again";
/** The warning of a report refused with `status`, a 4xx other than 408 or 429. */
const unreported = (status: number): string =>
  `legion: a report of model requests was refused with ${status}; they go unreported`;
const EXPIRED = "legion: dropped a model request past the daemon's keep window";
const DROPPED_INVALID = "legion: dropped a model request the contract refuses";

/** The statuses a random run's call refused for good is answered with: a 4xx other than 408 or
 * 429. */
const REFUSED_STATUSES = [400, 401, 403, 404, 409, 422] as const;

/** What makes a tick of a random run: a hook call, at a run's end or at a request's end, or a
 * firing of the run's clock, the reporter's timer or the end of an answer the daemon held. */
type TickCause = "a run's end" | "a request's end" | ClockFiring;

/** Each way a call of a random run can end, and how long the daemon or a proxy takes to answer it
 * on the run's clock (`ms`), always within the 10 s limit; an answer with a status is in the
 * daemon's refusal shape or a proxy's page (`shaped`). */
type Fate =
  | { readonly kind: "client throws" }
  | { readonly kind: "client refuses" }
  | { readonly kind: "connection drops"; readonly ms: number }
  | {
      readonly kind: "4xx refused" | "408 or 429" | "server error";
      readonly ms: number;
      readonly status: number;
      readonly shaped: boolean;
    }
  | { readonly kind: "no answer" }
  | { readonly kind: "answer"; readonly ms: number; readonly keepsAll: boolean };

/** Whether a call that ends so failed, so that the requests it put back wait a bound: every way
 * but a 4xx other than 408 or 429 and an answer ("When it is sent" in `./model-requests.ts`). */
function failedCall(fate: Fate): boolean {
  return fate.kind !== "4xx refused" && fate.kind !== "answer";
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
 * when a call last put it back (if any), and how the run is done with it — the daemon kept it, its
 * report was refused with a 4xx, or the reporter dropped it past the keep window — when, and in
 * which tick. */
interface RunRequest {
  readonly id: string;
  readonly owner: string;
  readonly ended: number;
  readonly read: number;
  lastPutBack?: number;
  gone?: {
    readonly how: "kept" | "refused" | "expired";
    readonly at: number;
    readonly tick: number;
  };
}

/** A tick of a random run, one hook call or one timer firing: what made it, when, the requests
 * pending as it began (none done with, none an open call carries), those of them a call had put
 * back, and those of these that wait for a report of their owner's put-back requests open then;
 * what it threw, if it did; and, where it made a send, each put-back request it left pending behind
 * its report. */
interface RunTick {
  readonly by: TickCause;
  readonly at: number;
  readonly pending: readonly string[];
  readonly putBack: readonly string[];
  readonly waiting: readonly string[];
  threw?: string;
  left?: readonly string[];
}

/** A call of a random run: the tick that made it and what made that tick, when it started, how it
 * ends and whether that was drawn at random (and not the answer that ends a loop or the run), its
 * place among its tick's calls (1 for the first) and the most calls the requests pending at the
 * tick's first call allow, the ids it carries, its owner's claim token, whether it is a report of
 * put-back requests and, if so, another report of its owner's put-back requests open when it was
 * made, whether its body reached the daemon and what was open then, when and in which tick it
 * ended, and the ids its answer kept. */
interface RunCall {
  readonly tick: number;
  readonly by: TickCause;
  readonly start: number;
  readonly fate: Fate;
  readonly drawn: boolean;
  readonly place: number;
  readonly bound: number;
  ids?: readonly string[];
  owner?: string;
  retry?: boolean;
  retryOpen?: RunCall;
  reached: boolean;
  reach?: RunReach;
  end?: number;
  endTick?: number;
  kept?: ReadonlySet<string>;
}

/** The moment a call's body reached the daemon: when, the calls open then, itself among them, the
 * sends of the last `MODEL_REQUEST_REPORT_TIMEOUT_MS` (`sends`), and the most calls open at once
 * they and the rule allow (`bound`): the reports of the requests no call carried those sends made
 * room for, and one report of put-back requests for each owner with one open. */
interface RunReach {
  readonly at: number;
  readonly open: readonly RunCall[];
  readonly sends: number;
  readonly bound: number;
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
  /** Each tick's send so far: when, its calls, the most calls the requests pending at its first
   * call allow, and how many of them for the requests no call carried. */
  const sends = new Map<
    number,
    { readonly at: number; calls: number; readonly bound: number; readonly fresh: number }
  >();
  let callsEnded = 0;
  let callsHandled = 0;
  let invalid = 0;

  const finish = (call: RunCall): void => {
    call.end = clock.now();
    call.endTick = tick;
    callsEnded += 1;
  };
  const putBack = (ids: readonly string[]): void => {
    for (const id of ids) {
      const request = requests.get(id);
      if (request !== undefined && request.gone === undefined) request.lastPutBack = clock.now();
    }
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
  /** The owners with a report of their put-back requests open. */
  const retrying = (): Set<string | undefined> =>
    new Set(open().flatMap((call) => (call.retry === true ? [call.owner] : [])));
  /** The most calls a send of `pending` makes (`bound`): per owner, the reports of the requests no
   * call carried (`fresh`, together), and one report of the requests calls put back, if any and
   * none of the owner's is open. */
  const callBoundFor = (pending: readonly RunRequest[]): { bound: number; fresh: number } => {
    const fresh = new Map<string, number>();
    const retried = new Set<string>();
    const waiting = retrying();
    for (const request of pending) {
      if (request.lastPutBack === undefined) {
        fresh.set(request.owner, (fresh.get(request.owner) ?? 0) + 1);
      } else if (!waiting.has(request.owner)) {
        retried.add(request.owner);
      }
    }
    let reports = 0;
    for (const count of fresh.values()) {
      reports += Math.ceil(count / LEGION_MAX_MODEL_REQUESTS);
    }
    return { bound: reports + retried.size, fresh: reports };
  };
  /** Records the ids `call` carries, its owner, and whether it is a report of put-back requests,
   * with another report of its owner's put-back requests open then, if any. */
  const carries = (call: RunCall, ids: readonly string[]): void => {
    const putBack = new Set(current?.putBack);
    call.ids = ids;
    call.owner = requests.get(ids[0] ?? "")?.owner;
    call.retry = ids.length > 0 && ids.every((id) => putBack.has(id));
    if (call.retry) {
      call.retryOpen = open().find(
        (other) => other !== call && other.retry === true && other.owner === call.owner
      );
    }
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
        return {
          kind: "4xx refused",
          ms,
          status: pick(REFUSED_STATUSES),
          shaped: random() < 0.5,
        };
      case 4:
        return { kind: "408 or 429", ms, status: pick([408, 429]), shaped: random() < 0.5 };
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
      carries(call, ids);
      call.reached = true;
      const now = clock.now();
      const since = now - MODEL_REQUEST_REPORT_TIMEOUT_MS;
      // Every open call began in the last `MODEL_REQUEST_REPORT_TIMEOUT_MS`, at a send then, which
      // made at most the reports of the requests no call carried that its pending requests allow
      // (`callBoundFor`), and at most one report of put-back requests is open per owner: a bound
      // that counts no backlog of put-back requests.
      const recent = [...sends.values()].filter((send) => send.at >= since);
      call.reach = {
        at: now,
        open: open(),
        sends: recent.length,
        bound: recent.reduce((sum, send) => sum + send.fresh, 0) + retrying().size,
      };
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
        case "4xx refused":
        case "408 or 429":
        case "server error":
          // A 4xx other than 408 or 429 drops the report's requests, and any other status puts
          // them back, in the daemon's refusal shape or a proxy's page alike.
          if (fate.kind === "4xx refused") done(ids, "refused");
          else putBack(ids);
          return fate.shaped
            ? Response.json(
                { error: "the daemon did not take this report" },
                { status: fate.status }
              )
            : new Response(`<html><body><h1>${fate.status}</h1></body></html>`, {
                status: fate.status,
                headers: { "Content-Type": "text/html" },
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
  /** The client each call of the reporter goes through: it records the call and draws how it
   * ends, but answers every call of a send past its bound and every call of the drain. */
  const daemon = (): LegionDaemonClient => {
    if (current === undefined) {
      throw new Error(`seed ${seed}: the reporter called the daemon outside a tick`);
    }
    let send = sends.get(tick);
    if (send === undefined) {
      send = { at: clock.now(), calls: 0, ...callBoundFor(pendingNow()) };
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
    const client = daemonOver(fetchFor(call));
    if (looping) {
      // Answer asynchronously keeping every id without parsing the body, so a reporter that loops
      // on a synchronous refusal cannot keep putting the same batch back.
      return {
        ...client,
        modelRequests: async (input) => {
          const kept = input.requests.map(({ id }) => id);
          carries(call, kept);
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
          carries(call, ids);
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
          carries(call, ids);
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
  };
  const { reporter, ids: given } = reporterOn(
    clock,
    { daemon },
    {
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
    }
  );
  /** Waits until the reporter has handled the end of each call that ended, one log line each. */
  const quiet = async (): Promise<void> => {
    for (let turns = 0; turns < 1_000; turns += 1) {
      await turn();
      if (callsHandled === callsEnded) return;
    }
    throw new Error(
      `seed ${seed}: the reporter handled ${callsHandled} of the ${callsEnded} calls that ended`
    );
  };
  /** Runs one hook call or timer firing as a tick of its own, records it and what it threw, and
   * waits for what it set off. It records the put-back requests that wait as it begins for a report
   * of their owner's put-back requests open then, and, where it made a send, each other put-back
   * request the send left pending, which `deadlines` reads. */
  const hook = async (by: TickCause, run: () => void): Promise<void> => {
    tick += 1;
    const pending = pendingNow();
    const owners = retrying();
    const waiting = pending.filter(
      ({ owner, lastPutBack }) => lastPutBack !== undefined && owners.has(owner)
    );
    const record: RunTick = {
      by,
      at: clock.now(),
      pending: pending.map(({ id }) => id),
      putBack: pending.flatMap(({ id, lastPutBack }) => (lastPutBack === undefined ? [] : [id])),
      waiting: waiting.map(({ id }) => id),
    };
    ticks.set(tick, record);
    current = record;
    const before = calls.length;
    try {
      run();
    } catch (error) {
      record.threw = String(error);
    }
    await quiet();
    const made = calls.slice(before);
    if (made.length === 0) return;
    const carried = new Set(made.flatMap((call) => call.ids ?? []));
    const waits = new Set(record.waiting);
    record.left = record.putBack.filter(
      (id) => !carried.has(id) && !waits.has(id) && requests.get(id)?.gone === undefined
    );
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
      const before = given();
      reporter.messageEnded(where, answered(ended));
      if (given() === before) return;
      if (where.session === "") {
        invalid += 1;
        return;
      }
      const id = `req-${given()}`;
      const owner = where === controllerPane ? controller : implementer;
      requests.set(id, { id, owner: owner.claimToken, ended, read: clock.now() });
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
  // ends, each put-back's hold ends, each request a send left falls due and each request pending
  // falls due.
  draining = true;
  await advance(
    2 * (MODEL_REQUEST_REPORT_TIMEOUT_MS + MODEL_REQUEST_REPORT_BOUND_MS) +
      MODEL_REQUEST_LEFT_DUE_AFTER_MS
  );
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
  refused: "its report was refused with a 4xx other than 408 or 429",
  expired: "the reporter dropped it past the keep window",
} as const;

/** A request's deadline at a tick of `run`, from the record alone ("When it is sent" in
 * `./model-requests.ts`): its end plus `MODEL_REQUEST_DUE_AFTER_MS`, or its read where that came
 * later; once a call that carried it was made before the tick, a bound after the last such call's
 * end; and `MODEL_REQUEST_LEFT_DUE_AFTER_MS` after each send of an earlier tick that left it pending
 * since, where later. Checks 1 and 3 both judge a tick by it. */
function deadlines(run: RunRecord): (id: string, tick: number) => number {
  const byRequest = callsByRequest(run.calls);
  /** Each send that left a request pending, by the request's id, in the order they were made. */
  const leftBy = new Map<string, { readonly tick: number; readonly at: number }[]>();
  for (const [tick, { at: time, left = [] }] of run.ticks) {
    for (const id of left) {
      const sends = leftBy.get(id) ?? [];
      sends.push({ tick, at: time });
      leftBy.set(id, sends);
    }
  }
  return (id, tick) => {
    const request = run.requests.get(id);
    if (request === undefined) return Number.NaN;
    let due = Math.max(request.ended + MODEL_REQUEST_DUE_AFTER_MS, request.read);
    let lastCall = Number.NEGATIVE_INFINITY;
    for (const call of byRequest.get(id) ?? []) {
      if (call.tick >= tick) break;
      due = (call.end ?? Number.POSITIVE_INFINITY) + MODEL_REQUEST_REPORT_BOUND_MS;
      lastCall = call.tick;
    }
    for (const left of leftBy.get(id) ?? []) {
      if (left.tick >= tick) break;
      if (left.tick > lastCall) due = Math.max(due, left.at + MODEL_REQUEST_LEFT_DUE_AFTER_MS);
    }
    return due;
  };
}

/** Check 1. Each hook call and timer firing returns, makes per owner at most the reports of the
 * pending requests no call carried (`ceil(count / LEGION_MAX_MODEL_REQUESTS)`) and one report of
 * those calls put back, and that one only while no other report of the owner's put-back requests
 * is open; and only a run's end or a timer with a request due (`deadlines`), but none that waits
 * for its owner's report, makes a send: a failing daemon is never called in a loop. */
function sendsReturnAndNeverLoop(run: RunRecord): string[] {
  const problems: string[] = [];
  const deadline = deadlines(run);
  /** Whether a request pending as `tick` began, but none that waited, was due then. */
  const dueAt = (tick: number): boolean => {
    const record = run.ticks.get(tick);
    if (record === undefined) return false;
    const waiting = new Set(record.waiting);
    return record.pending.some((id) => !waiting.has(id) && deadline(id, tick) <= record.at);
  };
  for (const { by, at: time, threw } of run.ticks.values()) {
    if (threw !== undefined) problems.push(`${by} at ${at(time)} threw: ${threw}`);
  }
  for (const call of run.calls) {
    if (
      call.place === 1 &&
      call.by !== "a run's end" &&
      (call.by !== "a timer" || !dueAt(call.tick))
    ) {
      problems.push(
        `${call.by} at ${at(call.start)} made a call when neither a run's end nor a due request triggered it`
      );
    }
    if (call.place === call.bound + 1) {
      problems.push(
        `${call.by} at ${at(call.start)} made more calls than the ${call.bound} its pending requests allow, per owner the reports of those no call carried and one of those calls put back: it took again a batch a call put back before it left`
      );
    }
    if (call.retryOpen !== undefined) {
      problems.push(
        `${call.by} at ${at(call.start)} made a report of put-back requests of ${call.owner} while the one ${call.retryOpen.by} made at ${at(call.retryOpen.start)} was open`
      );
    }
  }
  return problems;
}

/** Check 2. Each open call ends by `MODEL_REQUEST_REPORT_TIMEOUT_MS`, at most one report of an
 * owner's put-back requests is open at once, and the calls open at once never outnumber the
 * reports of the requests no call carried that the sends of the last
 * `MODEL_REQUEST_REPORT_TIMEOUT_MS` made room for, and that one report per owner. The bound counts
 * no backlog: however many requests calls put back, at most one call per owner carries them, so the
 * open calls stay bounded whatever the backlog. */
function openCallsEndAndStayBounded(run: RunRecord): string[] {
  const problems: string[] = [];
  for (const { reach } of run.calls) {
    if (reach === undefined) continue;
    const retries = new Map<string | undefined, number>();
    for (const active of reach.open) {
      if (reach.at - active.start > MODEL_REQUEST_REPORT_TIMEOUT_MS) {
        problems.push(
          `the call ${active.by} made at ${at(active.start)} was still open at ${at(reach.at)}, past the timeout`
        );
      }
      if (active.retry === true) {
        retries.set(active.owner, (retries.get(active.owner) ?? 0) + 1);
      }
    }
    for (const [owner, count] of retries) {
      if (count > 1) {
        problems.push(
          `${count} reports of put-back requests of ${owner} were open at ${at(reach.at)}, more than one`
        );
      }
    }
    if (reach.open.length > reach.bound) {
      problems.push(
        `${reach.open.length} calls were open at ${at(reach.at)}, above the bound of ${reach.bound} the ${reach.sends} sends since ${at(reach.at - MODEL_REQUEST_REPORT_TIMEOUT_MS)} allow`
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
 * call ended, or `MODEL_REQUEST_LEFT_DUE_AFTER_MS` after the last send that left it pending, if
 * later (`deadlines`); a send leaves a put-back request pending behind its report only where it
 * carries a full report of its owner's put-back requests, none of them ended after it; and a run's
 * end sends at once every request pending, but those it so leaves and those that wait for a report
 * of their owner's put-back requests, whatever failed before. A put-back request that waits keeps
 * its due, which in these runs comes after that report's end: it is a bound after a call that ended
 * after that report left, or after the send that left it behind that report or one before it. A
 * request goes no more once its report was refused with a 4xx other than 408 or 429, or once it was
 * dropped past the keep window, which happens only past it. Each failed call and each refusal warns
 * once, a refusal naming its status, and the run's end finds each request kept, refused or
 * dropped. */
function requestsGoInTime(run: RunRecord): string[] {
  const problems: string[] = [];
  const byTick = callsByTick(run.calls);
  const deadline = deadlines(run);
  const endOf = (id: string): number => run.requests.get(id)?.ended ?? Number.NaN;
  const ownerOf = (id: string): string | undefined => run.requests.get(id)?.owner;
  for (const [tick, { by, at: time, pending, putBack, waiting, left = [] }] of run.ticks) {
    const sent = new Set((byTick.get(tick) ?? []).flatMap((call) => call.ids ?? []));
    for (const owner of new Set(left.map(ownerOf))) {
      const carried = putBack.filter((id) => sent.has(id) && ownerOf(id) === owner);
      const leftOf = left.filter((id) => ownerOf(id) === owner);
      if (carried.length < LEGION_MAX_MODEL_REQUESTS) {
        problems.push(
          `${by} at ${at(time)} left ${leftOf.length} put-back requests of ${owner} pending, though it carried only ${carried.length} of them`
        );
      }
      const newest = Math.max(...carried.map(endOf));
      const oldest = Math.min(...leftOf.map(endOf));
      if (newest > oldest) {
        problems.push(
          `${by} at ${at(time)} left a put-back request of ${owner} that ended at ${at(oldest)} pending, though it carried one that ended later, at ${at(newest)}`
        );
      }
    }
    if (by !== "a run's end") continue;
    const stays = new Set([...left, ...waiting]);
    for (const id of pending) {
      const request = run.requests.get(id);
      // A request that send dropped past the keep window leaves not at all.
      const expiredNow = request?.gone?.how === "expired" && request.gone.tick === tick;
      if (!expiredNow && !sent.has(id) && !stays.has(id)) {
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
    // No failed call of other requests excuses a missed deadline.
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
      const due = deadline(id, call.tick);
      if (call.start > due) {
        problems.push(`${id} went at ${at(call.start)}, past its deadline at ${at(due)}`);
      }
      if (call.start - request.ended > KEEP_MS) {
        problems.push(`${id} went at ${at(call.start)}, past the keep window of its end`);
      }
    }
    if (gone === undefined) {
      problems.push(
        `${id}, which ended at ${at(request.ended)}, was neither kept, refused nor dropped by the run's end`
      );
    } else if (gone.how === "expired") {
      const due = deadline(id, gone.tick);
      if (gone.at > due) {
        problems.push(`${id} was dropped at ${at(gone.at)}, past its deadline at ${at(due)}`);
      }
    }
  }
  const warned = (message: string): number =>
    run.warnings.filter((warning) => warning === message).length;
  const failed = run.calls.filter((call) => failedCall(call.fate)).length;
  if (warned(GOES_AGAIN) !== failed) {
    problems.push(
      `the reporter warned ${warned(GOES_AGAIN)} times that a report goes again, for ${failed} failed calls`
    );
  }
  for (const status of REFUSED_STATUSES) {
    const refused = run.calls.filter(
      (call) => call.fate.kind === "4xx refused" && call.fate.status === status
    ).length;
    if (warned(unreported(status)) !== refused) {
      problems.push(
        `the reporter warned ${warned(unreported(status))} times that a report refused with ${status} goes unreported, for ${refused} calls refused with ${status}`
      );
    }
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
 * requests no call carried past `LEGION_MAX_MODEL_REQUESTS`, a send that left put-back requests
 * pending past its one report of them, a send whose put-back requests waited for a report of their
 * owner's on its way, a request the contract refuses, and each way a call can end, counted over the
 * calls whose way was drawn. */
interface RunCoverage {
  sentDuringHold: number;
  turnEndsDuringHold: number;
  splitBatches: number;
  leftBehind: number;
  waited: number;
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
  for (const [tick, { by, at: time, pending, putBack, waiting, left = [] }] of run.ticks) {
    const tickCalls = byTick.get(tick);
    if (waiting.length > 0 && (by === "a run's end" || tickCalls !== undefined)) {
      coverage.waited += 1;
    }
    if (tickCalls === undefined) continue;
    const retried = new Set(putBack);
    const owners = new Map<string, number>();
    for (const id of pending) {
      if (retried.has(id)) continue;
      const owner = run.requests.get(id)?.owner ?? "";
      owners.set(owner, (owners.get(owner) ?? 0) + 1);
    }
    for (const count of owners.values()) {
      if (count > LEGION_MAX_MODEL_REQUESTS) coverage.splitBatches += 1;
    }
    if (left.length > 0) coverage.leftBehind += 1;
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
// or refuses the body, before anything leaves; the connection drops; a 4xx other than 408 or 429
// refuses it; a 408 or a 429 fails it; a 5xx fails it, each in the daemon's refusal shape or a
// proxy's page; the 10 s limit aborts it; or an answer within the limit keeps every id or some. A
// run ends with a daemon that answers at once and keeps every id, until each request is done
// with. Every run holds each check of `CHECKS`, whose function states its rule, and across the
// seeds each case `RunCoverage` names occurs.
test("seeded random runs: each call returns, the open calls stay bounded, each request goes in time and again only on a retry", async () => {
  const problems: string[] = [];
  const coverage: RunCoverage = {
    sentDuringHold: 0,
    turnEndsDuringHold: 0,
    splitBatches: 0,
    leftBehind: 0,
    waited: 0,
    invalidDropped: 0,
    fates: {
      "client throws": 0,
      "client refuses": 0,
      "connection drops": 0,
      "4xx refused": 0,
      "408 or 429": 0,
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
  expect(coverage.leftBehind).toBeGreaterThan(0);
  expect(coverage.waited).toBeGreaterThan(0);
  expect(coverage.invalidDropped).toBeGreaterThan(0);
  for (const [kind, count] of Object.entries(coverage.fates)) {
    expect(count, `the calls drawn to end so: ${kind}`).toBeGreaterThan(0);
  }
});
