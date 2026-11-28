/**
 * The rows of the report of model requests (`./model-requests.ts`) against a daemon that takes
 * each report's connection and never answers, or holds an answer past the call's limit, as a
 * stalled or restarting one does. The hand-written rows of a daemon that answers are
 * `./model-requests.test.ts`.
 */
import { expect, test } from "bun:test";
import { LEGION_MAX_MODEL_REQUESTS } from "@legion/contracts/legion-api";
import {
  MODEL_REQUEST_DUE_AFTER_MS,
  MODEL_REQUEST_REPORT_BOUND_MS,
  MODEL_REQUEST_REPORT_TIMEOUT_MS,
} from "./model-requests";
import {
  answered,
  CONTROLLER_SESSION,
  type FakeClock,
  fakeClock,
  reporterOn,
  rig,
  role,
  START,
  turn,
} from "./model-requests-test-support";

/** Moves `clock` on by `ms`, letting what each firing sets off run (`turn`), but waiting on no
 * answer: a row's daemon may hold one off the clock. */
function moveOn(clock: FakeClock, ms: number): Promise<void> {
  return clock.advance(ms, async (fire) => {
    fire();
    await turn();
  });
}

// S1 against a daemon that takes each report's connection and never answers, as a stalled or
// restarting one does: each report's call is aborted `MODEL_REQUEST_REPORT_TIMEOUT_MS` after it
// left, a failed call, while requests no call carried keep their own deadlines. A turn runs on for
// two minutes, a request ending every 5 s. The first request after a deadline's send sets the next
// deadline, a period after the one before: the bound less the margin rounded up to the 5 s between
// requests, 15 s, so every third request. Each call aborts 5 s before the next deadline, and in
// this row the requests it put back go with that deadline's send, in its one report of put-back
// requests beside the report of the requests no call carried ("When it is sent" in
// `./model-requests.ts`): so from the second deadline on two calls are open at once, until a run's
// end fires halfway through the last deadline's calls, where three are. Once no new requests
// arrive, the put-back requests wait a full bound after their abort before going again: a stalled
// daemon is never called in a loop, and open calls do not grow with the stall.
test("a report the daemon never answers is aborted at its timeout and goes again after the hold", async () => {
  /** Each call: the ids it carries, when it left and was aborted on the rig's clock, and how many
   * calls were open once it left. */
  const calls: { ids: string[]; sent: number; aborted?: number; open: number }[] = [];
  let open = 0;
  const { reporter, advance, now, warned } = rig({
    answer: async (body, _attempt, hold) => {
      open += 1;
      const call: (typeof calls)[number] = {
        ids: (body.requests as { id: string }[]).map(({ id }) => id),
        sent: now(),
        open,
      };
      calls.push(call);
      // The daemon never answers: only the request's abort ends the hold, and rejects it.
      await hold(Number.POSITIVE_INFINITY).catch((reason: unknown) => {
        open -= 1;
        call.aborted = now();
        throw reason;
      });
      throw new Error("a daemon that never answers answered");
    },
  });

  // The request that sets each deadline (at 0, 15 s, ..., 105 s) falls due a period after the one
  // before; a run ends halfway through the last deadline's call, after a 25th request at 120 s;
  // then with no more requests, the put-back batch goes again a full bound after that call aborts.
  const requestEvery = 5_000;
  const period = Math.ceil(MODEL_REQUEST_DUE_AFTER_MS / requestEvery) * requestEvery;
  const deadlines = Array.from(
    { length: 8 },
    (_, index) => START + MODEL_REQUEST_DUE_AFTER_MS + index * period
  );
  const lastDeadline = deadlines.at(-1) ?? Number.NaN;
  const whileOpen = lastDeadline + MODEL_REQUEST_REPORT_TIMEOUT_MS / 2;
  const retryAfterAbort =
    lastDeadline + MODEL_REQUEST_REPORT_TIMEOUT_MS + MODEL_REQUEST_REPORT_BOUND_MS;
  const events = [
    ...Array.from({ length: 25 }, (_, index) => ({
      at: START + index * requestEvery,
      happen: (at: number) => reporter.messageEnded(role, answered(at)),
    })),
    { at: whileOpen, happen: () => reporter.turnEnded() },
  ].sort((a, b) => a.at - b.at);
  for (const { at, happen } of events) {
    await advance(at - now());
    happen(at);
  }
  // Wait through the abort of the open calls, the put-back hold, and the retry's abort.
  await advance(retryAfterAbort + MODEL_REQUEST_REPORT_TIMEOUT_MS - now());

  // Each call left as the schedule says, two at each deadline after the first, one at the run's end
  // while the last deadline's calls were open, and one again only once the put-back hold ended.
  expect(calls.map(({ sent }) => sent)).toEqual([
    deadlines[0],
    ...deadlines.slice(1).flatMap((deadline) => [deadline, deadline]),
    whileOpen,
    retryAfterAbort,
  ]);
  expect(calls.map(({ open }) => open)).toEqual([
    1, 1, 2, 1, 2, 1, 2, 1, 2, 1, 2, 1, 2, 1, 2, 3, 1,
  ]);
  // Each call was aborted at its limit, a failed call whose requests go again.
  expect(calls.map(({ sent, aborted }) => (aborted ?? Number.NaN) - sent)).toEqual(
    calls.map(() => MODEL_REQUEST_REPORT_TIMEOUT_MS)
  );
  expect(warned).toEqual(
    calls.map(() => "legion: a report of model requests did not reach the daemon; it goes again")
  );
  // At each deadline, the first report carries the requests that ended since the deadline before,
  // which no call carried, and from the second deadline on the other carries every id the deadline
  // before carried; the run-end call carries only the request that ended while the last deadline's
  // calls were open; the retry carries all 25.
  const perPeriod = period / requestEvery;
  const byDeadline = deadlines.map((deadline) => calls.filter(({ sent }) => sent === deadline));
  for (const [index, [fresh, putBack]] of byDeadline.entries()) {
    expect(fresh?.ids).toEqual(
      Array.from({ length: perPeriod }, (_, next) => `req-${index * perPeriod + next + 1}`)
    );
    const before = byDeadline[index - 1]?.flatMap(({ ids }) => ids) ?? [];
    expect(putBack?.ids.sort() ?? []).toEqual(before.sort());
  }
  expect(calls.at(-2)?.ids).toEqual(["req-25"]);
  expect(calls.at(-1)?.ids).toHaveLength(25);
});

// The deep review's long stall (review round 8 of cybermonklabs/legion#5603): a daemon that takes
// each report's connection and never answers, a request every 2 s alternating the role's claim and
// the controller's, and a run's end every 30 s, for 90 minutes on the clock. Each call is aborted
// at its 10 s limit, so the requests calls put back pile up to hundreds of each owner, which only
// the hour's keep window caps. A send takes, per owner, the reports of the requests no call carried
// and at most one report of put-back requests, none while another is on its way, and the put-back
// requests it leaves wait `MODEL_REQUEST_LEFT_DUE_AFTER_MS` ("When it is sent" in
// `./model-requests.ts`): so the most calls open at once in the last 5 minutes is no more than in
// the first 5, while the calls of the last 5 minutes carry hundreds of requests that ended before
// them, and every request no call carried still goes by its own deadline. On a reporter whose send
// takes every put-back request, the open calls grow with the backlog.
test("the open calls of a long stall do not grow with the requests calls put back", async () => {
  const clock = fakeClock();
  /** Each call: when it left, the ids it carries, and how many calls were open once it left. */
  const calls: { readonly sent: number; readonly ids: readonly string[]; readonly open: number }[] =
    [];
  let open = 0;
  const { reporter, ids: given } = reporterOn(clock, {
    fetch: (async (_input, init): Promise<Response> => {
      open += 1;
      const ids = (JSON.parse(String(init?.body)).requests as { id: string }[]).map(({ id }) => id);
      calls.push({ sent: clock.now(), ids, open });
      // The daemon never answers: only the call's abort ends the hold, and rejects it.
      await clock.hold(Number.POSITIVE_INFINITY, init?.signal, () => {
        open -= 1;
      });
      throw new Error("a daemon that never answers answered");
    }) as typeof fetch,
  });
  const controllerPane = { session: CONTROLLER_SESSION, subagent: false } as const;
  const stall = 90 * 60_000;
  const window = 5 * 60_000;
  /** Each request's end, by its id. */
  const ended = new Map<string, number>();
  for (let at = START; at < START + stall; at += 2_000) {
    await moveOn(clock, at - clock.now());
    reporter.messageEnded((at - START) % 4_000 === 0 ? role : controllerPane, answered(at));
    ended.set(`req-${given()}`, at);
    if ((at - START) % 30_000 === 0) reporter.turnEnded();
    await turn();
  }
  // The last requests' deadlines.
  await moveOn(clock, MODEL_REQUEST_REPORT_BOUND_MS);

  const last = START + stall - window;
  const retried = new Set(
    calls
      .filter(({ sent }) => sent >= last)
      .flatMap(({ ids }) => ids.filter((id) => (ended.get(id) ?? Number.NaN) < last))
  );
  expect(retried.size).toBeGreaterThan(2 * LEGION_MAX_MODEL_REQUESTS);
  const most = (from: number, until: number): number =>
    Math.max(...calls.filter(({ sent }) => from <= sent && sent < until).map(({ open }) => open));
  expect(most(last, START + stall)).toBeLessThanOrEqual(most(START, START + window));
  const first = new Map<string, number>();
  for (const { sent, ids } of calls) {
    for (const id of ids) if (!first.has(id)) first.set(id, sent);
  }
  const late = [...ended].filter(
    ([id, end]) => (first.get(id) ?? Number.POSITIVE_INFINITY) > end + MODEL_REQUEST_DUE_AFTER_MS
  );
  expect(late).toEqual([]);
});

// A report of put-back requests does not leave while another report of its owner's put-back
// requests is on its way, and the put-back requests that wait for it arm no timer meanwhile; once
// that report's call ends, a put-back report that fell due meanwhile goes at once ("When it is
// sent" in `./model-requests.ts`). A call ends within its 10 s limit as a rule, and a put-back
// request is then not due yet, so here the daemon answers the first put-back report 19 s after it
// left, past a limit whose abort this stand-in does not honor, as a transport slow to end the call
// would not: the put-back request that fell due at 16 s waits for that answer, and no longer.
test("a put-back report due while another of its owner's is on its way waits for that one's end and goes at once then", async () => {
  const clock = fakeClock();
  /** Each report the daemon took: the ids it carries, and when it left. */
  const sent: { readonly ids: string[]; readonly at: number }[] = [];
  const seen = new Set<string>();
  const released = Promise.withResolvers<void>();
  let fired = 0;
  const { reporter } = reporterOn(
    clock,
    {
      fetch: (async (_input, init): Promise<Response> => {
        const ids = (JSON.parse(String(init?.body)).requests as { id: string }[]).map(
          ({ id }) => id
        );
        const again = ids.some((id) => seen.has(id));
        for (const id of ids) seen.add(id);
        sent.push({ ids, at: clock.now() });
        // A request's first report fails; req-1's put-back report is answered once the row lets
        // it.
        if (!again) return Response.json({ error: "unavailable" }, { status: 503 });
        if (ids.includes("req-1")) await released.promise;
        return Response.json({ kept: ids });
      }) as typeof fetch,
    },
    {
      schedule: (callback, delayMs) =>
        clock.schedule(() => {
          fired += 1;
          callback();
        }, delayMs),
    }
  );

  // req-1's report fails at 0 s, and req-1 is put back, due at 15 s. At 1 s a run's end sends
  // req-1 again, in a report of put-back requests the daemon holds, and req-2 in a report of its
  // own, which fails: req-2 is put back, due at 16 s, and waits for req-1's report.
  reporter.messageEnded(role, answered(START));
  reporter.turnEnded();
  await turn();
  await moveOn(clock, 1_000);
  reporter.messageEnded(role, answered(clock.now()));
  reporter.turnEnded();
  await turn();
  expect(sent).toEqual([
    { ids: ["req-1"], at: START },
    { ids: ["req-1"], at: START + 1_000 },
    { ids: ["req-2"], at: START + 1_000 },
  ]);

  // Through 19 s nothing goes, and the one timer that fires is the put-back report's own abort at
  // its limit: none fires for req-2, due at 16 s.
  const firedBefore = fired;
  await moveOn(clock, 19_000);
  expect(sent).toHaveLength(3);
  expect(fired - firedBefore).toBe(1);

  // The daemon answers req-1's report at 20 s, and req-2's put-back report goes then.
  released.resolve();
  await turn();
  await moveOn(clock, 0);
  expect(sent.slice(3)).toEqual([{ ids: ["req-2"], at: START + 20_000 }]);
});

// A session's shutdown during a stall (`shutdown`, "When it is sent" in `./model-requests.ts`): no
// later send comes, so it sends every request pending at once, put-back requests included, in
// reports of at most 200, each a call of its own, and waits for no report on its way. The daemon
// takes each report's connection and never answers. 450 requests of the role's claim go at a
// run's end in three reports, which their limit aborts at 10 s, so all 450 are put back; a run's
// end at 11 s sends the oldest 200 of them in the one report of put-back requests a send may make,
// and leaves 250, and a fresh request ends at 11.5 s. A shutdown at 12 s, while that report is on
// its way, sends the 250 and the fresh one at once. On a reporter whose shutdown makes a run's
// end's send, the 250 wait for the report on its way, and are lost when the process exits.
test("a shutdown during a stall sends every request pending, put-back requests included", async () => {
  const clock = fakeClock();
  /** Each call: when it left, and the ids it carries. */
  const calls: { readonly sent: number; readonly ids: readonly string[] }[] = [];
  const { reporter } = reporterOn(clock, {
    fetch: (async (_input, init): Promise<Response> => {
      const ids = (JSON.parse(String(init?.body)).requests as { id: string }[]).map(({ id }) => id);
      calls.push({ sent: clock.now(), ids });
      // The daemon never answers: only the call's abort ends the hold, and rejects it.
      await clock.hold(Number.POSITIVE_INFINITY, init?.signal);
      throw new Error("a daemon that never answers answered");
    }) as typeof fetch,
  });
  /** The ids `req-${from}` to `req-${to}`. */
  const idsOf = (from: number, to: number): string[] =>
    Array.from({ length: to - from + 1 }, (_, index) => `req-${from + index}`);

  // req-1 to req-450 end a millisecond apart, the oldest first, and all fail at their limit.
  const backlog = 2 * LEGION_MAX_MODEL_REQUESTS + 50;
  for (let n = 1; n <= backlog; n += 1) {
    reporter.messageEnded(role, answered(START - backlog + n));
  }
  reporter.turnEnded();
  await turn();
  expect(calls.map(({ ids }) => ids.length)).toEqual([200, 200, 50]);
  await moveOn(clock, MODEL_REQUEST_REPORT_TIMEOUT_MS);
  expect(clock.holding()).toBe(0);

  // The run's end at 11 s: one report of put-back requests, the oldest 200, which stays on its way.
  await moveOn(clock, 1_000);
  reporter.turnEnded();
  await turn();
  expect(calls.slice(3)).toEqual([{ sent: START + 11_000, ids: idsOf(1, 200) }]);
  await moveOn(clock, 500);
  reporter.messageEnded(role, answered(clock.now()));
  await moveOn(clock, 500);
  expect(clock.holding()).toBe(1);

  // The shutdown at 12 s: the 250 put-back requests and the fresh one, at once, in reports of at
  // most 200, while the report of put-back requests made at 11 s is on its way.
  reporter.shutdown();
  await turn();
  const atShutdown = calls.slice(4);
  expect(atShutdown.map(({ sent }) => sent - START)).toEqual([12_000, 12_000, 12_000]);
  expect(atShutdown.map(({ ids }) => ids.length).sort((a, b) => a - b)).toEqual([1, 50, 200]);
  expect(atShutdown.flatMap(({ ids }) => ids).sort()).toEqual(idsOf(201, backlog + 1).sort());
  expect(clock.holding()).toBe(4);
});
