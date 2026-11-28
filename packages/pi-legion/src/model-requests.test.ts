import { expect, test } from "bun:test";
import { createLegionDaemonClient } from "@legion/pi-shared/daemon-client";
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops";
import type { AgentEndEvent, AgentEndMessage } from "@legion/pi-shared/pi-types";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import {
  createModelRequestsReporter,
  MODEL_REQUEST_REPORT_BOUND_MS,
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
 * `ms` on, which holds the answer that long on the rig's clock. The clock and the timers are the
 * rig's: nothing waits on the wall clock.
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
  const hold = (ms: number): Promise<void> => {
    const held = Promise.withResolvers<void>();
    holding += 1;
    timers.push({
      due: clock + ms,
      callback: () => {
        holding -= 1;
        held.resolve();
      },
      cancelled: false,
    });
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
    if (options.answer !== undefined) return options.answer(body, attempts, hold);
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
    log: { debug: () => undefined, warn: (message) => warned.push(message) },
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
  await advance(MODEL_REQUEST_REPORT_BOUND_MS - 1);
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

  await advance(MODEL_REQUEST_REPORT_BOUND_MS - 10_000 - 1);
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

  // The late request falls due 2 s from now; the first, 10 s from now.
  await advance(2_000 - 1);
  expect(batches()).toEqual([]);
  await advance(1);
  expect(
    batches().map((batch) => (batch.requests as { id: string }[]).map(({ id }) => id).sort())
  ).toEqual([["req-1", "req-2"]]);
  expect(now() - late).toBe(MODEL_REQUEST_REPORT_BOUND_MS);

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
// every request reaches the daemon within the bound of its end.
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
  await advance(MODEL_REQUEST_REPORT_BOUND_MS);
  // The first report reached the daemon at its request's end plus the bound, and is held.
  expect(reports).toEqual([{ ids: ["req-1"], sent: first + MODEL_REQUEST_REPORT_BOUND_MS }]);

  // The second request ended 12 s ago and is read only now: it falls due 3 s from now.
  const second = now() - 12_000;
  reporter.messageEnded(role, answered(second));
  await advance(3_000);
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
  // Every request reached the daemon within the bound of its end.
  const ended: Record<string, number> = { "req-1": first, "req-2": second };
  for (const { ids, sent } of reports) {
    for (const id of ids) {
      expect(sent - (ended[id] ?? Number.NaN)).toBeLessThanOrEqual(MODEL_REQUEST_REPORT_BOUND_MS);
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

// The failure is the daemon's own 500, in its refusal shape: the shared restart retry
// (`retryWhileDaemonRestarts`) asks again only an outage whose body is not the daemon's, such as a
// proxy's page (`daemonUnreachable`), so this one comes back to the reporter, whose own wait is
// what this case reads.
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

  // While the turn goes on: nothing before the bound of the first request's end, every request at
  // it.
  const during = now();
  endEach(during);
  await advance(MODEL_REQUEST_REPORT_BOUND_MS - 1);
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
