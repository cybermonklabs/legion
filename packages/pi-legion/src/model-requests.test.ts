import { expect, test } from "bun:test";
import { createLegionDaemonClient } from "@legion/pi-shared/daemon-client";
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops";
import type { AgentEndEvent, AgentEndMessage } from "@legion/pi-shared/pi-types";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import {
  createModelRequestsReporter,
  MODEL_REQUEST_DUE_AFTER_MS,
  MODEL_REQUEST_REPORT_BOUND_MS,
  MODEL_REQUEST_REPORT_TIMEOUT_MS,
  MODEL_REQUEST_SEND_MARGIN_MS,
  type ModelRequestCredential,
  type ModelRequestsReporter,
} from "./model-requests";
import { createTurnOutcomeReporter } from "./turn-outcome";

/** The contract this build states on every request (`Legion-Plugin-Contract`). */
const CONTRACT = 60;

const ROLE_SESSION = "ses_implementer_208";
const SUBAGENT_SESSION = "ses_subagent_of_208";
const CONTROLLER_SESSION = "ses_controller";
const OTHER_SESSION = "ses_person_pane";

const implementer: ClaimCapability = {
  sessionID: ROLE_SESSION,
  tree: "LEGION-208",
  issue: "LEGION-208",
  role: "implementer",
  secret: fixtureSecret("claim"),
  claimToken: "legion-legion-legion-208-implementer",
  generation: 3,
};

const controller: ModelRequestCredential = {
  claimToken: "legion-legion-controller",
  sessionID: CONTROLLER_SESSION,
  secret: fixtureSecret("control"),
  generation: 5,
};

/** 2026-11-25T21:53:17.412Z, the moment every case's clock starts at. */
const START = Date.parse("2026-11-25T21:53:17.412Z");

/** More warnings than any case writes (the rig's `log`). */
const WARNINGS_OF_A_LOOP = 1_000;

interface Posted {
  readonly route: string;
  readonly body: Record<string, unknown>;
}

interface Rig {
  readonly reporter: ModelRequestsReporter;
  /** Every body posted to the daemon, in the order it received them, by route. */
  readonly posted: Posted[];
  readonly warned: string[];
  /** The batches posted to `claims/model-requests`. */
  readonly batches: () => Record<string, unknown>[];
  /** Moves the clock on by `ms`, firing each timer that falls due, and waits for the sends they
   * start to be answered and handled; while the daemon holds an answer on the rig's clock
   * (`hold`), which only this moves, it lets every callback already queued run instead. */
  readonly advance: (ms: number) => Promise<void>;
  readonly now: () => number;
}

/**
 * The reporter over a stub daemon. `answer` answers each report of model requests, by the order it
 * arrived in, with the ids the daemon keeps, and defaults to every id of the batch; it may hold its
 * answer back by returning a promise, and `hold(ms)` resolves once `advance` has moved the clock
 * `ms` on, which holds the answer that long on the rig's clock, or rejects with the abort's reason
 * once the report's request is aborted, as `fetch` does. The clock and the timers are the rig's:
 * nothing waits on the wall clock.
 */
function rig(
  options: {
    readonly answer?: (
      body: Record<string, unknown>,
      attempt: number,
      hold: (ms: number) => Promise<void>
    ) => Promise<Response> | Response;
    readonly credential?: (sessionID: string) => ModelRequestCredential | undefined;
  } = {}
): Rig {
  const posted: Posted[] = [];
  const warned: string[] = [];
  let clock = START;
  let attempts = 0;
  let nextId = 0;
  const timers: { due: number; callback: () => void; cancelled: boolean }[] = [];
  /** How many answers the daemon holds on the rig's clock (`hold`). */
  let holding = 0;
  const hold = (ms: number, signal: AbortSignal | undefined): Promise<void> => {
    const held = Promise.withResolvers<void>();
    holding += 1;
    const timer = {
      due: clock + ms,
      callback: () => {
        holding -= 1;
        held.resolve();
      },
      cancelled: false,
    };
    timers.push(timer);
    signal?.addEventListener(
      "abort",
      () => {
        // An answer `advance` released already is no longer held.
        if (timer.cancelled) return;
        timer.cancelled = true;
        holding -= 1;
        held.reject(signal.reason);
      },
      { once: true }
    );
    return held.promise;
  };
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
      return options.answer(body, attempts, (ms) => hold(ms, init?.signal ?? undefined));
    }
    const requests = body.requests as { id: string }[];
    return Response.json({ kept: requests.map((request) => request.id) });
  }) as typeof fetch;
  const client = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: stub,
  });
  const reporter = createModelRequestsReporter({
    daemon: () => client,
    credential:
      options.credential ??
      ((sessionID) => (sessionID === implementer.sessionID ? implementer : undefined)),
    paneSession: () => ROLE_SESSION,
    log: {
      debug: () => undefined,
      warn: (message) => {
        warned.push(message);
        // No case warns this often: a reporter that does is calling in a loop, and the throw ends
        // the loop, so the case fails rather than hang.
        if (warned.length > WARNINGS_OF_A_LOOP) {
          throw new Error(`the reporter warned ${warned.length} times: it is calling in a loop`);
        }
      },
    },
    now: () => clock,
    schedule: (callback, delayMs) => {
      const timer = { due: clock + delayMs, callback, cancelled: false };
      timers.push(timer);
      return () => {
        timer.cancelled = true;
      };
    },
    newId: () => `req-${++nextId}`,
  });
  /** Waits until every report on its way is answered and handled, but while the daemon holds an
   * answer on the rig's clock: that answer waits for `advance`, so this lets every callback
   * already queued run instead. */
  const handled = async (): Promise<void> => {
    if (holding === 0) return reporter.settled();
    const turn = Promise.withResolvers<void>();
    setImmediate(turn.resolve);
    await turn.promise;
  };
  const advance = async (ms: number) => {
    const until = clock + ms;
    for (;;) {
      const due = timers
        .filter((timer) => !timer.cancelled && timer.due <= until)
        .sort((a, b) => a.due - b.due)[0];
      if (due === undefined) break;
      clock = due.due;
      due.cancelled = true;
      due.callback();
      await handled();
    }
    clock = until;
    await handled();
  };
  return {
    reporter,
    posted,
    warned,
    batches: () =>
      posted
        .filter((post) => post.route === "/legion/v1/claims/model-requests")
        .map((post) => post.body),
    advance,
    now: () => clock,
  };
}

/** An answered request as Oh My Pi's `message_end` carries it, ended now on the rig's clock. */
function answered(
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

/** The provider's text of a 429. */
const RATE_LIMITED_TEXT = "429 Too Many Requests: rate limit exceeded";

/** A request the provider answered with HTTP 429, as Oh My Pi's `message_end` carries it: measured
 * on the pinned host, no usage, the provider's text and its status (the capture in
 * `./model-requests.ts`). */
function rateLimited(at: number): AgentEndMessage {
  return {
    role: "assistant",
    model: "standin-model",
    stopReason: "error",
    errorStatus: 429,
    errorMessage: RATE_LIMITED_TEXT,
    usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
    completedAt: at,
  };
}

const role = { session: ROLE_SESSION, subagent: false } as const;

// A turn runs on for minutes; a request that ended is reported within the bound of its end while
// the turn goes on, with the moment it ended and its fresh input plus its cache writes.
test("partial while active", async () => {
  const { reporter, batches, advance, now } = rig();

  reporter.messageEnded(role, answered(now()));
  await advance(MODEL_REQUEST_DUE_AFTER_MS - 1);
  expect(batches()).toEqual([]);

  await advance(1);
  expect(batches()).toEqual([
    {
      claimToken: implementer.claimToken,
      sessionId: ROLE_SESSION,
      secret: implementer.secret,
      generation: 3,
      requests: [
        {
          id: "req-1",
          session: ROLE_SESSION,
          model: "standin-model",
          endedAt: "2026-11-25T21:53:17.412Z",
          uncachedInputTokens: 18,
        },
      ],
    },
  ]);
});

// A request a limit answered with HTTP 429 counted no token and carries no response id, so it is no
// request and is not reported, as the Claude Code bridge reports no `api_retry` line; the retry the
// provider answered in the same turn is reported alone, with its time and its uncached input
// tokens and no member more.
test("a 429 is not reported, and the answered retry of the same turn is", async () => {
  const { reporter, batches, now } = rig();

  reporter.messageEnded(role, rateLimited(now()));
  reporter.messageEnded(role, answered(now() + 1_000));
  reporter.turnEnded();
  await reporter.settled();
  expect(batches()).toEqual([
    {
      claimToken: implementer.claimToken,
      sessionId: ROLE_SESSION,
      secret: implementer.secret,
      generation: 3,
      requests: [
        {
          id: "req-1",
          session: ROLE_SESSION,
          model: "standin-model",
          endedAt: "2026-11-25T21:53:18.412Z",
          uncachedInputTokens: 18,
        },
      ],
    },
  ]);
});

// The first request of a batch sets its deadline, and one that joins it later moves nothing: the
// two cross a minute's boundary and each keeps the moment it ended.
test("batch across minutes", async () => {
  const { reporter, batches, advance, now } = rig();

  const first = now();
  reporter.messageEnded(role, answered(first));
  await advance(10_000);
  const second = now();
  reporter.messageEnded(role, answered(second));

  await advance(MODEL_REQUEST_DUE_AFTER_MS - 10_000 - 1);
  expect(batches()).toEqual([]);
  await advance(1);
  const [batch] = batches();
  expect(batches()).toHaveLength(1);
  expect(
    ((batch?.requests ?? []) as { endedAt: string }[]).map((request) => request.endedAt)
  ).toEqual(["2026-11-25T21:53:17.412Z", "2026-11-25T21:53:27.412Z"]);

  await advance(MODEL_REQUEST_REPORT_BOUND_MS);
  expect(batches()).toHaveLength(1);
});

// A request read late, its `message_end` 13 s after the end the host stamped, joins a batch whose
// timer is armed for a later deadline: the timer is armed again for the late request's deadline,
// the batch goes then, within the bound of that request's end, and the timer armed before sends
// nothing.
test("a request read late goes by its own deadline, before the armed timer", async () => {
  const { reporter, batches, advance, now } = rig();

  reporter.messageEnded(role, answered(now()));
  await advance(5_000);
  const late = now() - 13_000;
  reporter.messageEnded(role, answered(late));

  // The late request falls due 2 s less the margin from now; the first, 10 s less the margin from
  // now.
  await advance(2_000 - MODEL_REQUEST_SEND_MARGIN_MS - 1);
  expect(batches()).toEqual([]);
  await advance(1);
  expect(
    batches().map((batch) => (batch.requests as { id: string }[]).map(({ id }) => id).sort())
  ).toEqual([["req-1", "req-2"]]);
  expect(now() - late).toBe(MODEL_REQUEST_DUE_AFTER_MS);

  await advance(MODEL_REQUEST_REPORT_BOUND_MS * 2);
  expect(batches()).toHaveLength(1);
});

// The turn's end sends what is pending at once and the timer it had armed sends nothing more.
test("turn end flush", async () => {
  const { reporter, batches, advance, now } = rig();

  reporter.messageEnded(role, answered(now()));
  await advance(2_000);
  reporter.turnEnded();
  await reporter.settled();
  const [batch] = batches();
  expect(batches()).toHaveLength(1);
  expect(batch?.requests).toHaveLength(1);

  await advance(MODEL_REQUEST_REPORT_BOUND_MS * 2);
  expect(batches()).toHaveLength(1);

  // A turn that ends with nothing pending sends nothing.
  reporter.turnEnded();
  await reporter.settled();
  expect(batches()).toHaveLength(1);
});

// A request that ends while a report is on its way does not wait for that report's answer: the
// turn's end sends it at once, in a report of its own that carries it alone, and the daemon answers
// that report before the first. A request the daemon did not say it holds goes again under its
// own id, a bound after the answer; no id ever names two requests.
test("retry and next batch", async () => {
  const held = Promise.withResolvers<void>();
  const arrived = Promise.withResolvers<void>();
  const alone = Promise.withResolvers<void>();
  const { reporter, batches, advance, now } = rig({
    answer: async (body, attempt) => {
      const ids = (body.requests as { id: string }[]).map((request) => request.id);
      if (attempt === 1) {
        arrived.resolve();
        await held.promise;
        // The daemon holds the first request of the batch and not the second.
        return Response.json({ kept: ids.slice(0, 1) });
      }
      if (attempt === 2) alone.resolve();
      return Response.json({ kept: ids });
    },
  });
  const sent = () =>
    batches().map((batch) => (batch.requests as { id: string }[]).map((r) => r.id));

  reporter.messageEnded(role, answered(now()));
  reporter.messageEnded(role, answered(now() + 1));
  reporter.turnEnded();
  // The first report is on its way and unanswered when a third request ends, and the turn after it.
  await arrived.promise;
  expect(batches()).toHaveLength(1);
  reporter.messageEnded(role, answered(now() + 2));
  reporter.turnEnded();
  // The third request goes at once, alone, while the first report is still unanswered.
  await alone.promise;
  expect(sent()).toEqual([["req-1", "req-2"], ["req-3"]]);
  held.resolve();
  await reporter.settled();

  // The request the daemon did not hold goes again a bound after the answer, under its own id.
  await advance(MODEL_REQUEST_REPORT_BOUND_MS - 1);
  expect(sent()).toHaveLength(2);
  await advance(1);
  expect(sent()).toEqual([["req-1", "req-2"], ["req-3"], ["req-2"]]);
  // Each request has one id, and the ids of the three requests are three.
  const endedAt = new Map<string, string>();
  for (const batch of batches()) {
    for (const request of batch.requests as { id: string; endedAt: string }[]) {
      const before = endedAt.get(request.id);
      if (before !== undefined) expect(request.endedAt).toBe(before);
      endedAt.set(request.id, request.endedAt);
    }
  }
  expect(endedAt.size).toBe(3);

  await advance(MODEL_REQUEST_REPORT_BOUND_MS * 2);
  expect(batches()).toHaveLength(3);
});

// S1 against a daemon that takes 6 s of the rig's clock to answer each report: a report that falls
// due while another is on its way goes at once, in a call of its own that carries only what no
// report on its way carries, and waits for no answer. The second request's `message_end` is read
// while the first report is on its way, stamped 12 s before (a request read late, as the tester
// measured on the Claude Code bridge): it falls due while the daemon holds the first answer, and
// every request reaches the daemon before the bound of its end.
test("a report that falls due while another is on its way goes at once", async () => {
  const answerMs = 6_000;
  /** Each report: the ids it carries, and when the daemon took it and answered it, on the rig's
   * clock. */
  const reports: { ids: string[]; sent: number; answered?: number }[] = [];
  const { reporter, advance, now } = rig({
    answer: async (body, _attempt, hold) => {
      const report: (typeof reports)[number] = {
        ids: (body.requests as { id: string }[]).map(({ id }) => id),
        sent: now(),
      };
      reports.push(report);
      await hold(answerMs);
      report.answered = now();
      return Response.json({ kept: report.ids });
    },
  });

  const first = now();
  reporter.messageEnded(role, answered(first));
  await advance(MODEL_REQUEST_DUE_AFTER_MS);
  // The first report reached the daemon at its request's end plus the bound less the margin, and
  // is held.
  expect(reports).toEqual([{ ids: ["req-1"], sent: first + MODEL_REQUEST_DUE_AFTER_MS }]);

  // The second request ended 12 s ago and is read only now: it falls due 3 s less the margin
  // from now.
  const second = now() - 12_000;
  reporter.messageEnded(role, answered(second));
  await advance(3_000 - MODEL_REQUEST_SEND_MARGIN_MS);
  // Its report reached the daemon then, alone, while the first is still unanswered.
  expect(reports.map(({ ids }) => ids)).toEqual([["req-1"], ["req-2"]]);
  expect(reports.map(({ answered }) => answered)).toEqual([undefined, undefined]);

  // The daemon answers each 6 s after it took it, and nothing more goes.
  await advance(answerMs);
  expect(reports.map(({ sent, answered }) => (answered ?? Number.NaN) - sent)).toEqual([
    answerMs,
    answerMs,
  ]);
  await advance(MODEL_REQUEST_REPORT_BOUND_MS * 2);
  expect(reports).toHaveLength(2);
  // Every request reached the daemon before the bound of its end.
  const ended: Record<string, number> = { "req-1": first, "req-2": second };
  for (const { ids, sent } of reports) {
    for (const id of ids) {
      expect(sent - (ended[id] ?? Number.NaN)).toBeLessThan(MODEL_REQUEST_REPORT_BOUND_MS);
    }
  }
});

// S1 bounds the send, not the timer: a host sends a request's report within 15 s of the request's
// end. A timer never fires early, and the call that carries the report starts only once it has
// fired, so a timer armed for the end plus the bound sends every report after the bound. On the
// pinned Oh My Pi, against a stand-in daemon, the timer's reports reached the daemon 15.003 s and
// 15.005 s after their requests' ends. The Claude Code bridge leaves room for this
// (`requestSendMargin`, 250 ms, in packages/daemon/internal/claudebridge/modelrequests.go); this
// reporter's timer must leave the same room before the bound.
test("the timer sends a request's report with room before the bound, for the send itself", async () => {
  const { reporter, batches, advance, now } = rig();
  const ended = now();
  reporter.messageEnded(role, answered(ended));
  await advance(MODEL_REQUEST_REPORT_BOUND_MS - 250);
  expect(batches()).toHaveLength(1);
});

// The daemon fails both reports with its own 500. Each report is one call, with no retry inside
// it, so the failure comes back to the reporter, whose own wait is what this case reads.
test("a failed batch waits before retrying and the earliest deadline goes first", async () => {
  const { reporter, batches, advance, now } = rig({
    credential: (session) => (session === ROLE_SESSION ? implementer : controller),
    answer: (body, attempt) =>
      attempt <= 2
        ? Response.json({ error: "the store is unavailable" }, { status: 500 })
        : Response.json({ kept: (body.requests as { id: string }[]).map(({ id }) => id) }),
  });
  reporter.messageEnded(role, answered(now() - 1));
  reporter.messageEnded({ session: CONTROLLER_SESSION, subagent: false }, answered(now() - 2));
  reporter.turnEnded();
  await reporter.settled();
  const sessions = () => batches().map((batch) => batch.sessionId);
  // Each owner's report goes at once in a call of its own, the earliest deadline's first, and the
  // daemon fails both.
  expect(sessions()).toEqual([CONTROLLER_SESSION, ROLE_SESSION]);

  // A failed call holds every send for a bound after its answer, and then both go again.
  await advance(MODEL_REQUEST_REPORT_BOUND_MS - 1);
  expect(sessions()).toEqual([CONTROLLER_SESSION, ROLE_SESSION]);
  await advance(1);
  expect(sessions().slice(2).sort()).toEqual([CONTROLLER_SESSION, ROLE_SESSION]);
  await advance(MODEL_REQUEST_REPORT_BOUND_MS);
  expect(sessions()).toHaveLength(4);
});

// A request the contract refuses (a `session` of "", which its `nonEmptyString` refuses) is
// refused by the client before anything leaves: a failed call, whose batch goes back under its ids
// and whose end holds every send for a bound, as every failed call's does. The run's end that
// sends it returns, nothing reaches the daemon, the send warns once, and nothing goes again before
// the hold ends.
test("a report the client refuses before it leaves is a failed call, and the run's end returns", async () => {
  const { reporter, posted, warned, advance, now } = rig({ credential: () => implementer });
  reporter.messageEnded({ session: "", subagent: false }, answered(now()));
  reporter.turnEnded();
  await reporter.settled();
  expect(posted).toEqual([]);
  expect(warned).toEqual([
    "legion: a report of model requests did not reach the daemon; it goes again",
  ]);

  // Nothing goes again before the hold ends; then the request goes again, and is refused again.
  await advance(MODEL_REQUEST_REPORT_BOUND_MS - 1);
  expect(warned).toHaveLength(1);
  await advance(1);
  expect(posted).toEqual([]);
  expect(warned).toHaveLength(2);
});

// The 4xx rule, each branch. A proxy in front of the daemon that answers 429 with a page of its
// own is not the daemon's refusal (`daemonUnreachable` names it an outage): a failed call, whose
// request goes again after the hold. The daemon's own 409, in its refusal shape, will not pass on
// a retry: its request goes once and is dropped with a warning.
test("a proxy's 429 goes again after the hold, and the daemon's own 409 is dropped", async () => {
  const proxied = rig({
    answer: (body, attempt) =>
      attempt === 1
        ? new Response("<html><body><h1>429 Too Many Requests</h1></body></html>", {
            status: 429,
            headers: { "Content-Type": "text/html" },
          })
        : Response.json({ kept: (body.requests as { id: string }[]).map(({ id }) => id) }),
  });
  const proxiedIds = () =>
    proxied.batches().map((batch) => (batch.requests as { id: string }[]).map(({ id }) => id));
  proxied.reporter.messageEnded(role, answered(proxied.now()));
  proxied.reporter.turnEnded();
  await proxied.reporter.settled();
  expect(proxiedIds()).toEqual([["req-1"]]);
  expect(proxied.warned).toEqual([
    "legion: a report of model requests did not reach the daemon; it goes again",
  ]);
  await proxied.advance(MODEL_REQUEST_REPORT_BOUND_MS - 1);
  expect(proxiedIds()).toEqual([["req-1"]]);
  await proxied.advance(1);
  expect(proxiedIds()).toEqual([["req-1"], ["req-1"]]);
  expect(proxied.warned).toHaveLength(1);

  const refused = rig({
    answer: () =>
      Response.json(
        { error: "the claim's launch generation 3 is not its current one" },
        { status: 409 }
      ),
  });
  refused.reporter.messageEnded(role, answered(refused.now()));
  refused.reporter.turnEnded();
  await refused.reporter.settled();
  expect(refused.batches()).toHaveLength(1);
  expect(refused.warned).toEqual([
    "legion: the daemon refused a report of model requests; they go unreported",
  ]);
  await refused.advance(MODEL_REQUEST_REPORT_BOUND_MS * 2);
  expect(refused.batches()).toHaveLength(1);
  expect(refused.warned).toHaveLength(1);
});

// S1 against a daemon that takes each report's connection and never answers, as a stalled or
// restarting one does: each report's call is aborted `MODEL_REQUEST_REPORT_TIMEOUT_MS` after it
// left, a failed call, so its requests go again under their ids a bound after the abort, and the
// hold after a failed call holds every send: a stalled daemon is never called in a loop. A turn
// runs on for two minutes, a request ending every 5 s, so the timer falls due again and again.
// This schedule keeps each run end and each deadline out of an open call: each deadline falls in
// the hold after an aborted call, as each run end does but the last, so at most one call is open
// until then. The last run end falls halfway through a call, and a send a run's end makes goes at
// once, whatever report is on its way, so two calls are open at once there. What the row proves is
// that each call ends at its 10 s limit, so the open calls do not grow with the stall.
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

  // A call leaves when its first request falls due, and each after it when the hold after the one
  // before ends: its limit and then the bound after it.
  const cycle = MODEL_REQUEST_REPORT_TIMEOUT_MS + MODEL_REQUEST_REPORT_BOUND_MS;
  const leaves = [0, 1, 2, 3, 4].map((index) => START + MODEL_REQUEST_DUE_AFTER_MS + index * cycle);
  // A run ends halfway through each hold, and halfway through the last call.
  const inHolds = leaves
    .slice(0, -1)
    .map((left) => left + MODEL_REQUEST_REPORT_TIMEOUT_MS + MODEL_REQUEST_REPORT_BOUND_MS / 2);
  const whileOpen = (leaves.at(-1) ?? Number.NaN) + MODEL_REQUEST_REPORT_TIMEOUT_MS / 2;
  const events = [
    ...Array.from({ length: 24 }, (_, index) => ({
      at: START + index * 5_000,
      happen: (at: number) => reporter.messageEnded(role, answered(at)),
    })),
    ...[...inHolds, whileOpen].map((at) => ({ at, happen: () => reporter.turnEnded() })),
  ].sort((a, b) => a.at - b.at);
  for (const { at, happen } of events) {
    await advance(at - now());
    happen(at);
  }
  // The aborts of the last two calls.
  await advance(MODEL_REQUEST_REPORT_TIMEOUT_MS);

  // Each call left as the schedule says, alone but the last, which the last run end sent while
  // the one before it was open.
  expect(calls.map(({ sent }) => sent)).toEqual([...leaves, whileOpen]);
  expect(calls.map(({ open }) => open)).toEqual([1, 1, 1, 1, 1, 2]);
  // Each call was aborted at its limit, a failed call whose requests go again.
  expect(calls.map(({ sent, aborted }) => (aborted ?? Number.NaN) - sent)).toEqual(
    calls.map(() => MODEL_REQUEST_REPORT_TIMEOUT_MS)
  );
  expect(warned).toEqual(
    calls.map(() => "legion: a report of model requests did not reach the daemon; it goes again")
  );
  // Each call after a hold carries again every id the one before carried; the last carries only
  // the request that ended after the one before it left.
  for (const [index, call] of calls.slice(0, -1).entries()) {
    const before = calls[index - 1];
    if (before !== undefined) expect(call.ids).toEqual(expect.arrayContaining(before.ids));
  }
  expect(calls.at(-1)?.ids).toEqual(["req-24"]);
});

test("an unanswered request expires without blocking fresh requests", async () => {
  const { reporter, batches, advance, now, warned } = rig({
    answer: (body, attempt) =>
      attempt === 1
        ? Response.json({ kept: [] })
        : Response.json({ kept: (body.requests as { id: string }[]).map(({ id }) => id) }),
  });
  reporter.messageEnded(role, answered(now() - 60 * 60_000 + 1));
  reporter.turnEnded();
  await reporter.settled();
  reporter.messageEnded(role, answered(now()));
  await advance(MODEL_REQUEST_REPORT_BOUND_MS);
  await advance(MODEL_REQUEST_REPORT_BOUND_MS);
  expect(
    batches().map((batch) => (batch.requests as { id: string }[]).map(({ id }) => id))
  ).toEqual([["req-1"], ["req-2"]]);
  expect(warned).toEqual(["legion: dropped a model request past the daemon's keep window"]);
});

// Each kind of request the pinned host ends: an answered one, one that read the cache, and one on
// a model the project's limit does not count, which is still reported (the daemon selects). Each
// carries its id, session, model, time and uncached input tokens, and no member more.
test("request variants", async () => {
  const { reporter, batches, now } = rig();
  const variants: [string, AgentEndMessage, Record<string, unknown>][] = [
    ["clean", answered(now()), { uncachedInputTokens: 18 }],
    [
      "cached input",
      answered(now(), { input: 3, output: 9, cacheRead: 52_000, cacheWrite: 1_200 }),
      { uncachedInputTokens: 1_203 },
    ],
    [
      "nonmatching model",
      answered(now(), undefined, "another-model"),
      { model: "another-model", uncachedInputTokens: 18 },
    ],
  ];
  for (const [, message] of variants) reporter.messageEnded(role, message);
  reporter.turnEnded();
  await reporter.settled();

  const requests = batches().flatMap((batch) => batch.requests as Record<string, unknown>[]);
  expect(requests).toHaveLength(variants.length);
  variants.forEach(([name, , want], index) => {
    const request = requests[index] ?? {};
    for (const [key, value] of Object.entries(want)) {
      expect(`${name} ${key}: ${JSON.stringify(request[key])}`).toBe(
        `${name} ${key}: ${JSON.stringify(value)}`
      );
    }
    expect(`${name}: ${Object.keys(request).sort().join(" ")}`).toBe(
      `${name}: endedAt id model session uncachedInputTokens`
    );
  });

  // A message the host wrote itself, which no provider answered — a gate's stop, a request that
  // never went out — is no request, and nor is a failure of any status with no tokens.
  const before = requests.length;
  reporter.messageEnded(role, { role: "assistant", model: "standin-model", stopReason: "stop" });
  reporter.messageEnded(role, {
    role: "assistant",
    model: "standin-model",
    stopReason: "error",
    errorStatus: 529,
    errorMessage: "overloaded",
    usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
  });
  reporter.messageEnded(role, { role: "user" });
  reporter.turnEnded();
  await reporter.settled();
  expect(batches().flatMap((batch) => batch.requests as unknown[])).toHaveLength(before);
});

// The sessions whose requests count against the project's limit: the role's own, each subagent's
// it starts (reported through the role's claim, never a credential of its own), and the
// controller's. Each request is reported once, under its own session, with the moment it ended and
// its own uncached input tokens, within the bound of its end while the turn goes on, and at once
// at the turn's end; a session that holds no claim of this process — a person's own pane —
// reports nothing.
test("session inventory", async () => {
  const { reporter, batches, advance, now } = rig({
    credential: (sessionID) =>
      sessionID === ROLE_SESSION
        ? implementer
        : sessionID === CONTROLLER_SESSION
          ? controller
          : undefined,
  });
  const inventory: {
    readonly name: string;
    readonly where: { readonly session: string; readonly subagent: boolean };
    readonly credential: ModelRequestCredential;
    readonly usage: AgentEndMessage["usage"];
    readonly uncachedInputTokens: number;
  }[] = [
    {
      name: "role",
      where: role,
      credential: implementer,
      usage: { input: 11, output: 3, cacheRead: 100, cacheWrite: 7 },
      uncachedInputTokens: 18,
    },
    {
      name: "subagent",
      where: { session: SUBAGENT_SESSION, subagent: true },
      credential: implementer,
      usage: { input: 5, output: 1, cacheRead: 9_000, cacheWrite: 0 },
      uncachedInputTokens: 5,
    },
    {
      name: "controller",
      where: { session: CONTROLLER_SESSION, subagent: false },
      credential: controller,
      usage: { input: 2, output: 8, cacheRead: 3, cacheWrite: 40 },
      uncachedInputTokens: 42,
    },
  ];
  const reported = (from: number) =>
    batches()
      .slice(from)
      .flatMap((batch) =>
        (batch.requests as Record<string, unknown>[]).map((request) => ({
          session: String(request.session),
          endedAt: request.endedAt,
          uncachedInputTokens: request.uncachedInputTokens,
          claimToken: batch.claimToken,
          sessionId: batch.sessionId,
          generation: batch.generation,
        }))
      )
      .sort((a, b) => a.session.localeCompare(b.session));
  const want = (at: number) =>
    inventory
      .map((row, index) => ({
        session: row.where.session,
        endedAt: new Date(at + index).toISOString(),
        uncachedInputTokens: row.uncachedInputTokens,
        claimToken: row.credential.claimToken,
        sessionId: row.credential.sessionID,
        generation: row.credential.generation,
      }))
      .sort((a, b) => a.session.localeCompare(b.session));
  const endEach = (at: number) => {
    inventory.forEach((row, index) => {
      reporter.messageEnded(row.where, answered(at + index, row.usage));
    });
    reporter.messageEnded({ session: OTHER_SESSION, subagent: false }, answered(at));
  };

  // While the turn goes on: nothing before the first request's end plus the bound less the margin,
  // every request then.
  const during = now();
  endEach(during);
  await advance(MODEL_REQUEST_DUE_AFTER_MS - 1);
  expect(batches()).toEqual([]);
  await advance(1);
  expect(reported(0)).toEqual(want(during));

  // At the turn's end: every request at once.
  const sent = batches().length;
  const atEnd = now();
  endEach(atEnd);
  reporter.turnEnded();
  await reporter.settled();
  expect(reported(sent)).toEqual(want(atEnd));
});

// The report of a turn's outcome the daemon takes today is sent as it always was, its body and
// its route untouched by the requests reported beside it.
test("the turn-outcome report is unchanged beside the model requests", async () => {
  const { reporter, posted, now } = rig();
  const turnOutcomes = createTurnOutcomeReporter({
    daemon: () =>
      createLegionDaemonClient("http://daemon.test", {
        pluginContract: CONTRACT,
        fetch: (async (input, init) => {
          posted.push({
            route: new URL(input.toString()).pathname,
            body: JSON.parse(String(init?.body)),
          });
          return Response.json({ refusedRun: 1, failed: false });
        }) as typeof fetch,
      }),
    claim: (sessionID) => (sessionID === ROLE_SESSION ? implementer : undefined),
    log: { debug: () => undefined, warn: () => undefined },
    cyberPolicyRefusal: async () => false,
  });
  const limited = rateLimited(now());
  reporter.messageEnded(role, answered(now() - 1_000));
  reporter.messageEnded(role, limited);
  turnOutcomes.inputArrived(ROLE_SESSION, { text: "Continue the task.", source: "rpc" });
  turnOutcomes.runStarted(ROLE_SESSION);
  const end: AgentEndEvent = { messages: [{ role: "user" }, limited] };
  reporter.turnEnded();
  await turnOutcomes.report(ROLE_SESSION, end);
  await reporter.settled();

  expect(posted.filter((post) => post.route === "/legion/v1/claims/turn-outcome")).toEqual([
    {
      route: "/legion/v1/claims/turn-outcome",
      body: {
        claimToken: implementer.claimToken,
        sessionId: ROLE_SESSION,
        secret: implementer.secret,
        generation: 3,
        sequence: 1,
        outcome: "refused",
        message: RATE_LIMITED_TEXT,
        httpStatus: 429,
      },
    },
  ]);
  expect(posted.filter((post) => post.route === "/legion/v1/claims/model-requests")).toHaveLength(
    1
  );
});

/** How long the reporter keeps a request for a retry, an hour (`MODEL_REQUEST_KEEP_MS` in
 * `./model-requests`). */
const KEEP_MS = 60 * 60_000;

/** The owners a random run's requests are reported under: the role's claim, for its own session
 * and its subagent's, and the controller's. */
const OWNERS = 2;

/** How many seeded runs the random case draws. */
const SEEDS = 200;

const GOES_AGAIN = "legion: a report of model requests did not reach the daemon; it goes again";
const UNREPORTED = "legion: the daemon refused a report of model requests; they go unreported";
const EXPIRED = "legion: dropped a model request past the daemon's keep window";

/** Each way a call of a random run can end, and how long the daemon or a proxy takes to answer it
 * on the run's clock (`ms`), always within the 10 s limit. */
type Fate =
  | { readonly kind: "daemon throws" }
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

/** Whether a call that ends so failed, so that its requests go again after a hold: every way but
 * the daemon's own refusal (a 4xx in its refusal shape) and its answer. */
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
 * and how the run is done with it — the daemon kept it, refused its report, or the reporter
 * dropped it past the keep window — when, and in which tick. */
interface RunRequest {
  readonly id: string;
  readonly owner: string;
  readonly ended: number;
  readonly read: number;
  gone?: {
    readonly how: "kept" | "refused" | "expired";
    readonly at: number;
    readonly tick: number;
  };
}

/** A call of a random run: the tick that made it (one hook call or one timer firing) and what that
 * was, when it started, how it ends, the ids it carries (unknown where `deps.daemon()` threw before
 * the body was built), whether its body reached the daemon, when and in which tick it ended, and
 * the ids its answer kept. */
interface RunCall {
  readonly tick: number;
  readonly by: string;
  readonly start: number;
  readonly fate: Fate;
  ids?: readonly string[];
  reached: boolean;
  end?: number;
  endTick?: number;
  kept?: ReadonlySet<string>;
}

/** One seeded run of the reporter over the real client, on a clock of its own, and what it found
 * against the four checks of the case below, each naming its seed. */
async function randomRun(seed: number): Promise<string[]> {
  const random = seeded(seed);
  const problems: string[] = [];
  const problem = (check: number, text: string): void => {
    problems.push(`seed ${seed}, check ${check}: ${text}`);
  };
  const at = (time: number): string => `${time - START} ms`;
  let clock = START;
  /** The hook call or timer firing that runs now, and what it is. */
  let tick = 0;
  let by = "";
  let draining = false;
  let timers: { due: number; callback: () => void; cancelled: boolean }[] = [];
  const requests = new Map<string, RunRequest>();
  const calls: RunCall[] = [];
  const warnings: string[] = [];
  /** When each request was read, and when each answer kept only some of its call's ids: what can
   * give a send something to send while no hold holds sends (check 2). */
  const refills: number[] = [];
  /** Each tick's calls, and the owners with requests pending when it made its first. */
  const ticks = new Map<number, { calls: number; owners: number }>();
  let callsEnded = 0;
  let callsHandled = 0;
  let nextId = 0;

  const finish = (call: RunCall): void => {
    call.end = clock;
    call.endTick = tick;
    callsEnded += 1;
  };
  const done = (ids: readonly string[], how: "kept" | "refused" | "expired"): void => {
    for (const id of ids) {
      const request = requests.get(id);
      if (request !== undefined && request.gone === undefined) {
        request.gone = { how, at: clock, tick };
      }
    }
  };
  const open = (): RunCall[] => calls.filter((call) => call.reached && call.end === undefined);
  const pendingOwners = (): number => {
    const carried = new Set(open().flatMap((call) => call.ids ?? []));
    const pending = [...requests.values()].filter(
      (request) => request.gone === undefined && !carried.has(request.id)
    );
    return new Set(pending.map((request) => request.owner)).size;
  };
  /** Holds an answer `ms` on the run's clock, or until the call's abort, which rejects it as
   * `fetch` does. */
  const hold = (call: RunCall, ms: number, signal: AbortSignal | null | undefined) => {
    const held = Promise.withResolvers<void>();
    const timer = {
      due: clock + ms,
      callback: () => {
        finish(call);
        held.resolve();
      },
      cancelled: false,
    };
    timers.push(timer);
    signal?.addEventListener(
      "abort",
      () => {
        if (timer.cancelled) return;
        timer.cancelled = true;
        finish(call);
        held.reject(signal.reason);
      },
      { once: true }
    );
    return held.promise;
  };
  const pick = (choices: readonly number[]): number =>
    choices[Math.floor(random() * choices.length)] ?? Number.NaN;
  const drawFate = (): Fate => {
    const ms = Math.floor(random() * MODEL_REQUEST_REPORT_TIMEOUT_MS);
    switch (Math.floor(random() * 10)) {
      case 0:
        return { kind: "daemon throws" };
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
      const since = clock - MODEL_REQUEST_REPORT_TIMEOUT_MS;
      const bound = OWNERS + refills.filter((refill) => refill >= since).length;
      const opened = open().length;
      if (opened > bound) {
        problem(
          2,
          `${opened} calls were open at ${at(clock)}, above the bound of ${bound}: ${OWNERS} owners, and ${bound - OWNERS} requests read or put back by an answer since ${at(since)}`
        );
      }
      const { fate } = call;
      await hold(call, "ms" in fate ? fate.ms : Number.POSITIVE_INFINITY, init?.signal);
      switch (fate.kind) {
        case "connection drops":
          throw new TypeError("fetch failed");
        case "daemon refuses":
          done(ids, "refused");
          return Response.json(
            { error: "the daemon refuses this report" },
            { status: fate.status }
          );
        case "proxy outage":
          return new Response(`<html><body><h1>${fate.status}</h1></body></html>`, {
            status: fate.status,
            headers: { "Content-Type": "text/html" },
          });
        case "server error":
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
          if (kept.length < ids.length) refills.push(clock);
          done(kept, "kept");
          return Response.json({ kept });
        }
        default:
          throw new Error(`a call whose fate is "${fate.kind}" reached the daemon`);
      }
    }) as typeof fetch;
  const reporter = createModelRequestsReporter({
    daemon: () => {
      const made = ticks.get(tick) ?? { calls: 0, owners: pendingOwners() };
      ticks.set(tick, made);
      made.calls += 1;
      // Check 1. A send makes one call for each owner with requests pending: past that, it took
      // again a batch a call put back before it left. From then on the tick's calls are answered,
      // which ends the loop, so the case fails rather than hang.
      const looping = made.calls > made.owners;
      if (looping && made.calls === made.owners + 1) {
        problem(
          1,
          `${by} at ${at(clock)} made more calls than the ${made.owners} owners with requests pending: it took again a batch a call put back before it left, the loop that never returns where the refusal repeats`
        );
      }
      const fate: Fate =
        draining || looping ? { kind: "answer", ms: 0, keepsAll: true } : drawFate();
      const call: RunCall = { tick, by, start: clock, fate, reached: false };
      calls.push(call);
      if (fate.kind === "daemon throws") {
        finish(call);
        throw new Error("LEGION_DAEMON_URL is not set");
      }
      const client = createLegionDaemonClient("http://daemon.test", {
        pluginContract: CONTRACT,
        fetch: fetchFor(call),
      });
      if (fate.kind !== "client refuses") return client;
      return {
        ...client,
        // A body the contract refuses: its requests name no session.
        modelRequests: (input, options) => {
          call.ids = input.requests.map(({ id }) => id);
          finish(call);
          const requests = input.requests.map((request) => ({ ...request, session: "" }));
          return client.modelRequests({ ...input, requests }, options);
        },
      };
    },
    credential: (sessionID) =>
      sessionID === ROLE_SESSION
        ? implementer
        : sessionID === CONTROLLER_SESSION
          ? controller
          : undefined,
    paneSession: () => ROLE_SESSION,
    log: {
      debug: () => {
        callsHandled += 1;
      },
      warn: (message, context) => {
        warnings.push(message);
        if (message !== EXPIRED) {
          callsHandled += 1;
          return;
        }
        const id = String(context?.id);
        const request = requests.get(id);
        if (request !== undefined && clock - request.ended <= KEEP_MS) {
          problem(
            3,
            `${id}, which ended at ${at(request.ended)}, was dropped at ${at(clock)}, inside the keep window`
          );
        }
        done([id], "expired");
      },
    },
    now: () => clock,
    schedule: (callback, delayMs) => {
      const timer = { due: clock + delayMs, callback, cancelled: false };
      timers.push(timer);
      return () => {
        timer.cancelled = true;
      };
    },
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
  /** Runs one hook call or timer firing as a tick of its own, and waits for what it set off. */
  const hook = async (name: string, run: () => void): Promise<void> => {
    tick += 1;
    by = name;
    run();
    await quiet();
  };
  /** Moves the clock on by `ms`, firing each timer that falls due, in order. */
  const advance = async (ms: number): Promise<void> => {
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
      await hook("a timer", next.callback);
    }
    clock = until;
    timers = timers.filter((timer) => !timer.cancelled);
  };

  const subagent = { session: SUBAGENT_SESSION, subagent: true } as const;
  const controllerPane = { session: CONTROLLER_SESSION, subagent: false } as const;
  const steps = 5 + Math.floor(random() * 60);
  for (let step = 0; step < steps; step += 1) {
    if (random() < 0.6) {
      const who = random();
      const where = who < 0.45 ? role : who < 0.7 ? subagent : controllerPane;
      // Most requests are read as they end; some late, as the tester measured on the Claude Code
      // bridge; and a few so late that a retry finds them past the keep window.
      const lateness = random();
      const ended =
        lateness < 0.7
          ? clock
          : lateness < 0.97
            ? clock - Math.floor(random() * 13_001)
            : clock - KEEP_MS + Math.floor(random() * 20_000);
      await hook("a request's end", () => {
        const before = nextId;
        reporter.messageEnded(where, answered(ended));
        if (nextId === before) return;
        const id = `req-${nextId}`;
        const owner = where === controllerPane ? controller : implementer;
        requests.set(id, { id, owner: owner.claimToken, ended, read: clock });
        refills.push(clock);
      });
    }
    if (random() < 0.3) await hook("a run's end", () => reporter.turnEnded());
    const pace = random();
    await advance(1 + Math.floor(random() * (pace < 0.4 ? 500 : pace < 0.9 ? 5_000 : 20_000)));
  }
  // The daemon answers each call at once and keeps every id, for long enough that each open call
  // ends, each hold ends and each request pending falls due.
  draining = true;
  await advance(2 * (MODEL_REQUEST_REPORT_TIMEOUT_MS + MODEL_REQUEST_REPORT_BOUND_MS));

  for (const call of open()) {
    problem(2, `the call ${call.by} made at ${at(call.start)} was still open at the run's end`);
  }

  // Check 3. A failed call holds every send for a bound after it ended.
  const holds = calls.flatMap((call) =>
    failedCall(call.fate) && call.end !== undefined && call.endTick !== undefined
      ? [{ from: call.end, until: call.end + MODEL_REQUEST_REPORT_BOUND_MS, tick: call.endTick }]
      : []
  );
  for (const call of calls) {
    const holding = holds.find(
      (span) => span.tick < call.tick && span.from <= call.start && call.start < span.until
    );
    if (holding !== undefined) {
      problem(
        3,
        `${call.by} made a call at ${at(call.start)}, inside the hold after the call that failed at ${at(holding.from)}`
      );
    }
  }
  /** Whether a hold held sends at every moment from `from` until `to`. */
  const heldThrough = (from: number, to: number): boolean => {
    let reached = from;
    while (reached < to) {
      const moment = reached;
      const next = Math.max(
        moment,
        ...holds.filter((span) => span.from <= moment && moment < span.until).map((s) => s.until)
      );
      if (next === moment) return false;
      reached = next;
    }
    return true;
  };
  const why = {
    kept: "the daemon kept it",
    refused: "the daemon refused its report",
    expired: "the reporter dropped it past the keep window",
  } as const;
  for (const request of requests.values()) {
    const { id, gone } = request;
    // Its deadline: its end plus the due delay, or its read where that came later; once a call
    // that carried it ended, a bound after that end.
    let due = Math.max(request.ended + MODEL_REQUEST_DUE_AFTER_MS, request.read);
    let carrying: RunCall | undefined;
    for (const call of calls) {
      if (call.ids?.includes(id) !== true) continue;
      if (gone !== undefined && call.tick > gone.tick) {
        problem(
          gone.how === "kept" ? 4 : 3,
          `${id} went again at ${at(call.start)}, though ${why[gone.how]} at ${at(gone.at)}`
        );
        break;
      }
      if (call.start > due && !heldThrough(due, call.start)) {
        problem(
          3,
          `${id} went at ${at(call.start)}, past its deadline at ${at(due)}, and no hold held it`
        );
      }
      if (call.start - request.ended > KEEP_MS) {
        problem(3, `${id} went at ${at(call.start)}, past the keep window of its end`);
      }
      if (call.reached) {
        // Check 4: a request goes again only once the call that carried it ended.
        if (carrying !== undefined && (carrying.endTick ?? Number.POSITIVE_INFINITY) >= call.tick) {
          problem(
            4,
            `${id} went at ${at(call.start)} while the call made at ${at(carrying.start)}, which carries it, was open`
          );
        }
        carrying = call;
      }
      due = (call.end ?? Number.POSITIVE_INFINITY) + MODEL_REQUEST_REPORT_BOUND_MS;
    }
    if (gone === undefined) {
      problem(
        3,
        `${id}, which ended at ${at(request.ended)}, was neither kept, refused nor dropped by the run's end`
      );
    } else if (gone.how === "expired" && gone.at > due && !heldThrough(due, gone.at)) {
      problem(
        3,
        `${id} was dropped at ${at(gone.at)}, past its deadline at ${at(due)}, and no hold held it`
      );
    }
  }
  const warned = (message: string): number =>
    warnings.filter((warning) => warning === message).length;
  const failed = calls.filter((call) => failedCall(call.fate)).length;
  const refused = calls.filter((call) => call.fate.kind === "daemon refuses").length;
  if (warned(GOES_AGAIN) !== failed) {
    problem(
      3,
      `the reporter warned ${warned(GOES_AGAIN)} times that a report goes again, for ${failed} failed calls`
    );
  }
  if (warned(UNREPORTED) !== refused) {
    problem(
      3,
      `the reporter warned ${warned(UNREPORTED)} times that a report goes unreported, for ${refused} refused calls`
    );
  }
  return problems.slice(0, 3);
}

// Seeded random runs of the reporter over the real client on a clock of their own. Each seed draws
// a run of its own length: requests that end at random times, some read late, some a second
// owner's, a few read so late that a retry finds them past the keep window; run ends at random
// times; and the timer, firing as the clock moves. Each call ends in one way drawn at random:
// `deps.daemon()` throws, or the client refuses the body, before anything leaves; the connection
// drops; the daemon refuses with a 4xx in its refusal shape; a proxy answers 408 or 429; a 5xx;
// the 10 s limit aborts it; or an answer within the limit keeps every id or some. A run ends with a
// daemon that answers at once and keeps every id, until each request is done with. Across the
// seeds:
// 1. Each hook call and timer firing returns, and makes at most one call per owner with requests
//    pending: one that takes again a batch a call put back before it left is in the loop that
//    never returns where the refusal repeats.
// 2. The open calls never outnumber the owners plus the requests read, and the answers that kept
//    only some of their ids, in the last `MODEL_REQUEST_REPORT_TIMEOUT_MS`. Each call ends by its
//    limit, so every call open began in that window. A send takes everything pending, in one call
//    per owner, so each send after the first in the window carries only what was read or put back
//    since the one before; and a failed call's put-back is never that, since its hold outlasts the
//    window. The bound reads only the last 10 s, so it does not grow with the run.
// 3. Each request goes by its deadline — its end plus `MODEL_REQUEST_DUE_AFTER_MS`, within 15 s of
//    its end, or at once where it was read later — and, once a call that carried it ended, by a
//    bound after that end, unless a failed call's hold held sends all the while. No call leaves
//    inside a hold. A request goes no more once the daemon refused its report or once it was
//    dropped past the keep window, which happens only past it. Each failed call and each refusal
//    warns once, and the run's end finds each request kept, refused or dropped.
// 4. No request goes again while a call that carries it is open, nor once the daemon kept it: it
//    goes again only after a failed call or an answer that did not keep its id.
test("seeded random runs: each call returns, the open calls stay bounded, each request goes in time and again only on a retry", async () => {
  const problems: string[] = [];
  for (let seed = 1; seed <= SEEDS; seed += 1) problems.push(...(await randomRun(seed)));
  expect(problems).toEqual([]);
});
