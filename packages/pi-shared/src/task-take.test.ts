import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import {
  type LegionTake,
  LegionTakeResponse,
  LegionTaskResponse,
} from "@legion/contracts/legion-api";
import type { RelayTaskValue } from "@legion/envoy-client/consumer-delivery";
import { LegionDaemonApiError } from "./daemon-client";
import {
  createTaskTake,
  TASK_ANSWER_TIMEOUT_MS,
  TASK_RESEND_FIRST_DELAY_MS,
  TASK_RESEND_MAX_DELAY_MS,
  type TaskTakeCaller,
} from "./task-take";

/** A response the daemon's own golden test wrote (`packages/daemon/internal/api`). */
function daemonFixture(name: string): unknown {
  return JSON.parse(
    readFileSync(path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api", name), "utf8")
  );
}

// The daemon's answers, as its golden tests wrote them and the client reads them.
const ACCEPTED = LegionTakeResponse.parse(daemonFixture("take-accepted.json"));
const REFUSED = LegionTakeResponse.parse(daemonFixture("take-refused.json"));
const DROPPED = LegionTakeResponse.parse(daemonFixture("take-dropped.json"));
const NOTHING_HELD = LegionTaskResponse.parse(daemonFixture("task.json"));
const TEXT = ACCEPTED.outcome === "accepted" ? ACCEPTED.text : "";

const caller: TaskTakeCaller = {
  claimToken: "legion-legion-legion-209-implementer",
  sessionId: "ses_implementer_209",
  secret: "U2VjcmV0",
  generation: 3,
};
const EPOCH = "2026-11-13T16:21:44.123456789Z";

/** A value the daemon read or relayed: task `number` at `revision` of the bucket's epoch. */
const value = (number: number, revision: number, epoch = EPOCH) => ({ number, epoch, revision });

/** A task frame the listener relayed for this claim. */
const put = (number: number, revision: number, claim = caller.claimToken): RelayTaskValue => ({
  claim,
  epoch: EPOCH,
  revision,
  op: "put",
  task: number,
});

/** What one attempt of a request meets: the daemon's answer, a failure, or no answer at all, in
 * which case the attempt waits until its bound aborts it. */
type Attempt<T> = { readonly answer: T } | { readonly fail: Error } | { readonly silent: true };

interface Call {
  readonly route: "task-seen" | "take";
  readonly body: Record<string, unknown>;
}

/** Lets every promise queued so far settle. */
async function flush(): Promise<void> {
  for (let turn = 0; turn < 50; turn += 1) await Promise.resolve();
}

/**
 * The take over a fake daemon client and a fake clock. The daemon answers each take with the next
 * attempt `takes` holds (a refusal, by default, as a hold would answer it), and each seen report
 * with the next `seens` holds (204, by default). `advance` moves the clock, firing every wait it
 * passes; `waits` is every wait between two attempts of one request, in the order they were made.
 */
function rig() {
  const calls: Call[] = [];
  const takes: Attempt<LegionTake>[] = [];
  const seens: Attempt<undefined>[] = [];
  const ran: string[] = [];
  const marks: boolean[] = [];
  const warned: string[] = [];
  const waits: number[] = [];
  let now = 0;
  const timers: { due: number; readonly resolve: (passed: boolean) => void }[] = [];

  const attempt = <T>(next: Attempt<T> | undefined, fallback: T, signal?: AbortSignal) => {
    if (next === undefined) return Promise.resolve(fallback);
    if ("answer" in next) return Promise.resolve(next.answer);
    if ("fail" in next) return Promise.reject(next.fail);
    const { promise, reject } = Promise.withResolvers<T>();
    signal?.addEventListener("abort", () => reject(signal.reason), { once: true });
    return promise;
  };

  const take = createTaskTake({
    caller,
    daemon: {
      taskSeen: (input, options) => {
        calls.push({ route: "task-seen", body: { ...input } });
        return attempt(seens.shift(), undefined, options?.signal);
      },
      take: (input, options) => {
        calls.push({ route: "take", body: { ...input } });
        return attempt(takes.shift(), REFUSED, options?.signal);
      },
    },
    run: (text) => ran.push(text),
    setPending: (pending) => marks.push(pending),
    log: { info: () => undefined, warn: (message) => warned.push(message) },
    wait: (ms, signal) => {
      const { promise, resolve } = Promise.withResolvers<boolean>();
      if (signal.aborted) {
        resolve(false);
        return promise;
      }
      if (ms !== TASK_ANSWER_TIMEOUT_MS) waits.push(ms);
      const timer = { due: now + ms, resolve };
      timers.push(timer);
      signal.addEventListener(
        "abort",
        () => {
          timers.splice(timers.indexOf(timer), 1);
          resolve(false);
        },
        { once: true }
      );
      return promise;
    },
  });

  const advance = async (ms: number): Promise<void> => {
    const until = now + ms;
    for (;;) {
      await flush();
      timers.sort((a, b) => a.due - b.due);
      const next = timers[0];
      if (next === undefined || next.due > until) break;
      timers.shift();
      now = next.due;
      next.resolve(true);
    }
    now = until;
    await flush();
  };

  const routed = (route: Call["route"]) =>
    calls.filter((call) => call.route === route).map((call) => call.body);

  return { take, calls, takes, seens, ran, marks, warned, waits, advance, routed };
}

const body = (number: number, revision: number) => ({
  ...caller,
  number,
  epoch: EPOCH,
  revision,
});

test("reports a value seen while the host is busy, and asks only once the run ends", async () => {
  const { take, routed, advance, takes } = rig();
  take.ready();
  take.runStarted();
  take.read({ task: value(4, 12), finished: 3 });
  await advance(0);

  expect(routed("task-seen")).toEqual([body(4, 12)]);
  expect(routed("take")).toEqual([]);

  takes.push({ answer: ACCEPTED });
  expect(take.runEnded()).toBeUndefined();
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12)]);
});

test("asks no take before the claim's ready has answered, and once after it", async () => {
  const { take, routed, advance } = rig();
  take.read({ task: value(4, 12), finished: 3 });
  await advance(0);
  expect(routed("task-seen")).toEqual([body(4, 12)]);
  expect(routed("take")).toEqual([]);

  take.ready();
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12)]);
});

test("asks once per revision: the same value read and relayed again is neither reported nor asked again", async () => {
  const { take, routed, advance } = rig();
  take.ready();
  take.read({ task: value(4, 12), finished: 3 });
  take.relayed(put(4, 12));
  await advance(0);
  take.read({ task: value(4, 12), finished: 3 });
  take.relayed(put(4, 12));
  await advance(0);

  expect(routed("task-seen")).toEqual([body(4, 12)]);
  expect(routed("take")).toEqual([body(4, 12)]);
});

test("asks nothing again for the same value relayed after a refusal, and once at the hold-end's new revision, running the accepted text", async () => {
  const { take, routed, advance, takes, ran, marks } = rig();
  take.ready();
  take.relayed(put(4, 12));
  await advance(0);
  // The take was refused (a hold stands, `take-refused.json`), so the task's turn is not pending.
  expect(routed("take")).toEqual([body(4, 12)]);
  expect(marks).toEqual([true, false]);

  take.relayed(put(4, 12));
  take.read({ task: value(4, 12), finished: 3 });
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12)]);

  // The hold ended: the daemon wrote the value again, a new revision, which is the only signal.
  takes.push({ answer: ACCEPTED });
  take.relayed(put(4, 13));
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12), body(4, 13)]);
  expect(ran).toEqual([TEXT]);
  // The mark stays until the task's turn starts.
  expect(marks).toEqual([true, false, true]);
  take.runStarted();
  expect(marks).toEqual([true, false, true, false]);
  expect(take.runEnded()).toBe(4);
});

test("holds the task-turn mark from the registration's value, before the ready and the role claim, until the task's turn starts", async () => {
  const { take, marks, takes, advance } = rig();
  take.read({ task: value(4, 12), finished: 3 });
  expect(marks).toEqual([true]);

  takes.push({ answer: ACCEPTED });
  take.ready();
  await advance(0);
  expect(marks).toEqual([true]);
  take.runStarted();
  expect(marks).toEqual([true, false]);
});

test("takes during a compaction, which leaves the host idle, and the first run after it is the task's turn", async () => {
  const { take, routed, takes, ran, advance } = rig();
  take.ready();
  // A run ends, and the host compacts in its idle maintenance: no run is going, so the host is
  // idle for the take, and the text waits for the compaction (the host's own form, `run`).
  take.runStarted();
  take.runEnded();
  takes.push({ answer: ACCEPTED });
  take.relayed(put(4, 12));
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12)]);
  expect(ran).toEqual([TEXT]);

  take.runStarted();
  expect(take.runEnded()).toBe(4);
  // A run after the task's names no task.
  take.runStarted();
  expect(take.runEnded()).toBeUndefined();
});

test("a provider error's end, the wait's end, and exactly one new take at the value the daemon writes again", async () => {
  const { take, routed, takes, ran, advance } = rig();
  take.ready();
  takes.push({ answer: ACCEPTED });
  take.read({ task: value(4, 12), finished: 3 });
  await advance(0);
  take.runStarted();
  // The turn ended in a provider error, which its report names beside the number: the daemon
  // keeps the task under its number and waits before it writes the value again.
  expect(take.runEnded()).toBe(4);

  // Nothing new while the wait runs: the same value read or relayed again asks nothing.
  take.relayed(put(4, 12));
  take.read({ task: value(4, 12), finished: 3 });
  await advance(30_000);
  expect(routed("take")).toEqual([body(4, 12)]);

  // The wait ended: the daemon wrote the value again, and the worker takes it once.
  takes.push({ answer: ACCEPTED });
  take.relayed(put(4, 13));
  take.read({ task: value(4, 13), finished: 3 });
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12), body(4, 13)]);
  expect(ran).toEqual([TEXT, TEXT]);
  take.runStarted();
  expect(take.runEnded()).toBe(4);
});

// Oh My Pi folds a follow-up sent during a run into that run (its agent loop reads the follow-up
// queue at the run's end and goes on with no `agent_end` between), so a text the daemon accepted
// while a run went on is held for that run's end, and starts the run that is the task's turn.
test("the text of a take accepted while a run is going is sent at that run's end, and the run it starts is the task's turn", async () => {
  const { take, takes, ran, advance, marks } = rig();
  take.ready();
  takes.push({ answer: ACCEPTED });
  take.relayed(put(4, 12));
  // A run starts before the take's answer comes back: the daemon had not yet heard of it.
  take.runStarted();
  await advance(0);
  expect(ran).toEqual([]);
  expect(marks).toEqual([true]);

  expect(take.runEnded()).toBeUndefined();
  expect(ran).toEqual([TEXT]);
  take.runStarted();
  expect(marks).toEqual([true, false]);
  expect(take.runEnded()).toBe(4);
});

test("a take sent again for a number already accepted runs the text once", async () => {
  const { take, takes, ran, advance } = rig();
  take.ready();
  takes.push({ answer: ACCEPTED });
  take.relayed(put(4, 12));
  await advance(0);
  // A newer revision of the same number before the turn started: the daemon answers the take of
  // an accepted number with the same text, which is already on its way.
  takes.push({ answer: ACCEPTED });
  take.relayed(put(4, 13));
  await advance(0);
  expect(ran).toEqual([TEXT]);
});

test("a dropped take clears the value and the mark, and a deleted value asks nothing", async () => {
  const { take, routed, takes, marks, advance } = rig();
  take.ready();
  takes.push({ answer: DROPPED });
  take.relayed(put(4, 12));
  await advance(0);
  expect(marks).toEqual([true, false]);

  // The delete the drop made, then a stale relay of the dropped value at its old revision.
  take.relayed({ claim: caller.claimToken, epoch: EPOCH, revision: 13, op: "delete" });
  take.relayed(put(4, 12));
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12)]);
});

test("a value naming a finished number, or another claim's, asks nothing", async () => {
  const { take, routed, advance, marks } = rig();
  take.ready();
  take.read({ task: value(3, 9), finished: 3 });
  take.relayed(put(5, 2, "legion-legion-legion-210-implementer"));
  take.read(NOTHING_HELD);
  await advance(0);

  expect(routed("take")).toEqual([]);
  expect(routed("task-seen")).toEqual([body(3, 9)]);
  expect(marks).toEqual([]);
});

test("a value of another epoch replaces the held one, whatever its revision", async () => {
  const { take, routed, advance } = rig();
  take.ready();
  take.relayed(put(4, 12));
  await advance(0);
  take.read({ task: value(4, 1, "2026-11-14T08:00:00.5Z"), finished: 3 });
  await advance(0);

  expect(routed("take")).toEqual([body(4, 12), { ...body(4, 1), epoch: "2026-11-14T08:00:00.5Z" }]);
});

/** A failure of the transport, as Bun's fetch raises it, carrying `code`. */
const transport = (code: string): Error =>
  Object.assign(new TypeError(`${code}: the request reached no daemon`), { code });

const uncoded = (status: number): Error =>
  new LegionDaemonApiError("POST", "/legion/v1/claims/take", status, undefined, "upstream failed");

for (const [name, attempt] of [
  ["a refused connection", { fail: transport("ECONNREFUSED") }],
  ["a reset connection", { fail: transport("ECONNRESET") }],
  ["a closed connection", { fail: transport("ConnectionClosed") }],
  ["a DNS failure", { fail: transport("ENOTFOUND") }],
  ["no answer within 5 minutes", { silent: true }],
  ["a 502 with no code", { fail: uncoded(502) }],
  ["a 503 with no code", { fail: uncoded(503) }],
  ["a 504 with no code", { fail: uncoded(504) }],
] as const) {
  test(`a take that meets ${name} is sent again, the same request, with growing waits and no limit, until it is answered`, async () => {
    const { take, routed, takes, ran, waits, advance, marks } = rig();
    take.ready();
    const failures = 12;
    for (let n = 0; n < failures; n += 1) takes.push(attempt);
    takes.push({ answer: ACCEPTED });
    take.relayed(put(4, 12));
    // Long enough for every attempt's bound and every wait between them.
    for (let n = 0; n <= failures; n += 1) await advance(TASK_ANSWER_TIMEOUT_MS);

    expect(routed("take")).toEqual(Array(failures + 1).fill(body(4, 12)));
    expect(waits).toEqual(
      Array.from({ length: failures }, (_, n) =>
        Math.min(TASK_RESEND_FIRST_DELAY_MS * 2 ** n, TASK_RESEND_MAX_DELAY_MS)
      )
    );
    expect(ran).toEqual([TEXT]);
    // Sending again is the same ask: the mark held throughout, until the turn starts.
    expect(marks).toEqual([true]);
  });

  test(`a seen report that meets ${name} is sent again, the same request, until it is answered`, async () => {
    const { take, routed, seens, advance } = rig();
    seens.push(attempt, attempt, attempt);
    take.relayed(put(4, 12));
    for (let n = 0; n <= 3; n += 1) await advance(TASK_ANSWER_TIMEOUT_MS);

    expect(routed("task-seen")).toEqual(Array(4).fill(body(4, 12)));
  });
}

for (const [name, refusal] of [
  ["a 409 with a code", new LegionDaemonApiError("POST", "/p", 409, "TASK_NOT_OWED", "refused")],
  ["a 503 with a code", new LegionDaemonApiError("POST", "/p", 503, "DAEMON_DRAINING", "draining")],
  ["a 400", new LegionDaemonApiError("POST", "/p", 400, undefined, "malformed")],
] as const) {
  test(`a take or a seen report the daemon answers with ${name} is not sent again`, async () => {
    const { take, routed, takes, seens, advance, marks } = rig();
    take.ready();
    takes.push({ fail: refusal });
    seens.push({ fail: refusal });
    take.relayed(put(4, 12));
    await advance(10 * TASK_ANSWER_TIMEOUT_MS);

    expect(routed("take")).toEqual([body(4, 12)]);
    expect(routed("task-seen")).toEqual([body(4, 12)]);
    expect(marks).toEqual([true, false]);
  });
}

test("a newer revision ends the re-sending of the older take and seen report, and is asked at once", async () => {
  const { take, routed, takes, seens, ran, advance } = rig();
  take.ready();
  takes.push({ fail: transport("ECONNREFUSED") }, { fail: transport("ECONNREFUSED") });
  seens.push({ fail: transport("ECONNREFUSED") }, { fail: transport("ECONNREFUSED") });
  take.relayed(put(4, 12));
  await advance(0);
  expect(routed("take")).toEqual([body(4, 12)]);

  takes.splice(0);
  takes.push({ answer: ACCEPTED });
  seens.splice(0);
  take.relayed(put(4, 13));
  await advance(10 * TASK_RESEND_MAX_DELAY_MS);

  expect(routed("take")).toEqual([body(4, 12), body(4, 13)]);
  expect(routed("task-seen")).toEqual([body(4, 12), body(4, 13)]);
  expect(ran).toEqual([TEXT]);
});

test("a deleted value ends the re-sending of its take", async () => {
  const { take, routed, takes, advance, marks } = rig();
  take.ready();
  for (let n = 0; n < 5; n += 1) takes.push({ fail: transport("ECONNRESET") });
  take.relayed(put(4, 12));
  await advance(0);
  take.relayed({ claim: caller.claimToken, epoch: EPOCH, revision: 13, op: "delete" });
  await advance(10 * TASK_RESEND_MAX_DELAY_MS);

  expect(routed("take")).toEqual([body(4, 12)]);
  expect(marks).toEqual([true, false]);
});
