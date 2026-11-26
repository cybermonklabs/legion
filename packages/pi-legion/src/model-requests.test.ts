import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import { createLegionDaemonClient } from "@legion/pi-shared/daemon-client";
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops";
import type { AgentEndEvent, AgentEndMessage } from "@legion/pi-shared/pi-types";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import {
  createModelRequestsReporter,
  type HostEvents,
  MODEL_REQUEST_REPORT_BOUND_MS,
  type ModelRequestCredential,
  type ModelRequestsReporter,
  PINNED_HOST_EVENTS,
  refusalPatterns,
  reportsRetriedRefusals,
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

/** The shared table of refusal texts and the class each host gives them
 * (`packages/contracts/fixtures/refusal-classes.json`), which the Claude Code bridge's test reads
 * too. */
const REFUSAL_CLASSES: {
  readonly patterns: { readonly own: string; readonly provider: string };
  readonly cases: readonly {
    readonly name: string;
    readonly text: string;
    readonly refusal: "own" | "provider" | "unknown";
  }[];
} = JSON.parse(
  readFileSync(
    path.resolve(import.meta.dir, "../../contracts/fixtures/refusal-classes.json"),
    "utf8"
  )
);

const PATTERN_ENV = {
  LEGION_SIGNAL_OWN_REFUSAL: REFUSAL_CLASSES.patterns.own,
  LEGION_SIGNAL_PROVIDER_REFUSAL: REFUSAL_CLASSES.patterns.provider,
};

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
   * start. */
  readonly advance: (ms: number) => Promise<void>;
  readonly now: () => number;
}

/**
 * The reporter over a stub daemon. `answer` answers each report of model requests, by the order it
 * arrived in, with the ids the daemon keeps, and defaults to every id of the batch; it may hold its
 * answer back by returning a promise. The clock and the timers are the rig's: nothing waits on the
 * wall clock. `retriedRefusalsReported` is what the detection read of the host, and defaults to
 * its reading of the pinned host, as the plugin builds the reporter.
 */
function rig(
  options: {
    readonly env?: Readonly<Record<string, string | undefined>>;
    readonly answer?: (
      body: Record<string, unknown>,
      attempt: number
    ) => Promise<Response> | Response;
    readonly credential?: (sessionID: string) => ModelRequestCredential | undefined;
    readonly retriedRefusalsReported?: boolean;
  } = {}
): Rig {
  const posted: Posted[] = [];
  const warned: string[] = [];
  let clock = START;
  let attempts = 0;
  let nextId = 0;
  const timers: { due: number; callback: () => void; cancelled: boolean }[] = [];
  const stub = (async (input, init) => {
    const route = new URL(input.toString()).pathname;
    const body = JSON.parse(String(init?.body)) as Record<string, unknown>;
    posted.push({ route, body });
    if (route === "/legion/v1/claims/turn-outcome") {
      return Response.json({ refusedRun: 1, failed: false });
    }
    if (route !== "/legion/v1/claims/model-requests") throw new Error(`unexpected route ${route}`);
    attempts += 1;
    if (options.answer !== undefined) return options.answer(body, attempts);
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
    patterns: refusalPatterns(options.env ?? PATTERN_ENV, {
      debug: () => undefined,
      warn: (message) => warned.push(message),
    }),
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
    retriedRefusalsReported:
      options.retriedRefusalsReported ?? reportsRetriedRefusals(PINNED_HOST_EVENTS),
  });
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
      await reporter.settled();
    }
    clock = until;
    await reporter.settled();
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

/** A request the provider refused with HTTP 429, as Oh My Pi's `message_end` carries it: measured
 * on the pinned host, no usage, the provider's text and its status (the capture in
 * `./model-requests.ts`). */
function refused(at: number, text: string): AgentEndMessage {
  return {
    role: "assistant",
    model: "standin-model",
    stopReason: "error",
    errorStatus: 429,
    errorMessage: text,
    usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
    completedAt: at,
  };
}

const role = { session: ROLE_SESSION, subagent: false } as const;

// A turn runs on for minutes; a request that ended is reported within the bound of its end while
// the turn goes on, with the moment it ended and its fresh input plus its cache writes, in a batch
// that says the pinned host does not report the 429s its transport retries.
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
      retriedRefusalsReported: false,
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
  expect(batches()).toHaveLength(1);
  expect(
    (batches()[0]?.requests as { endedAt: string }[]).map((request) => request.endedAt)
  ).toEqual(["2026-11-25T21:53:17.412Z", "2026-11-25T21:53:27.412Z"]);

  await advance(MODEL_REQUEST_REPORT_BOUND_MS);
  expect(batches()).toHaveLength(1);
});

// The turn's end sends what is pending at once and the timer it had armed sends nothing more.
test("turn end flush", async () => {
  const { reporter, batches, advance, now } = rig();

  reporter.messageEnded(role, answered(now()));
  await advance(2_000);
  reporter.turnEnded();
  await reporter.settled();
  expect(batches()).toHaveLength(1);
  expect((batches()[0]?.requests as unknown[]).length).toBe(1);

  await advance(MODEL_REQUEST_REPORT_BOUND_MS * 2);
  expect(batches()).toHaveLength(1);

  // A turn that ends with nothing pending sends nothing.
  reporter.turnEnded();
  await reporter.settled();
  expect(batches()).toHaveLength(1);
});

// A request that ends while a report is on its way waits for a report of its own, and a request
// the daemon did not say it holds goes again under its own id; no id ever names two requests.
test("retry and next batch", async () => {
  const held = Promise.withResolvers<void>();
  const arrived = Promise.withResolvers<void>();
  const { reporter, batches, advance, now } = rig({
    answer: async (body, attempt) => {
      const ids = (body.requests as { id: string }[]).map((request) => request.id);
      if (attempt === 1) {
        arrived.resolve();
        await held.promise;
        // The daemon holds the first request of the batch and not the second.
        return Response.json({ kept: ids.slice(0, 1) });
      }
      return Response.json({ kept: ids });
    },
  });

  reporter.messageEnded(role, answered(now()));
  reporter.messageEnded(role, answered(now() + 1));
  reporter.turnEnded();
  // The first report is on its way and unanswered when a third request ends.
  await arrived.promise;
  expect(batches()).toHaveLength(1);
  reporter.messageEnded(role, answered(now() + 2));
  held.resolve();
  await reporter.settled();

  // The third request's own batch goes at its end plus the bound, and the request the daemon did
  // not hold goes with it.
  await advance(MODEL_REQUEST_REPORT_BOUND_MS + 2);
  const sent = batches().map((batch) => (batch.requests as { id: string }[]).map((r) => r.id));
  expect(sent[0]).toEqual(["req-1", "req-2"]);
  expect(sent.slice(1).flat().sort()).toEqual(["req-2", "req-3"]);
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
  expect(batches()).toHaveLength(sent.length);
});

test("a failed batch waits before retrying and the earliest deadline goes next", async () => {
  const { reporter, batches, advance, now } = rig({
    credential: (session) => (session === ROLE_SESSION ? implementer : controller),
    answer: (body, attempt) =>
      attempt <= 2
        ? new Response("unavailable", { status: 500 })
        : Response.json({ kept: (body.requests as { id: string }[]).map(({ id }) => id) }),
  });
  reporter.messageEnded(role, answered(now() - 1));
  reporter.messageEnded({ session: CONTROLLER_SESSION, subagent: false }, answered(now() - 2));
  reporter.turnEnded();
  await reporter.settled();
  const sessions = () => batches().map((batch) => batch.sessionId);
  expect(sessions()).toEqual([CONTROLLER_SESSION]);

  await advance(MODEL_REQUEST_REPORT_BOUND_MS - 1);
  expect(sessions()).toEqual([CONTROLLER_SESSION]);
  await advance(1);
  expect(sessions()).toEqual([CONTROLLER_SESSION, ROLE_SESSION]);
  await advance(MODEL_REQUEST_REPORT_BOUND_MS);
  expect(sessions()).toEqual([CONTROLLER_SESSION, ROLE_SESSION, CONTROLLER_SESSION, ROLE_SESSION]);
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

// Each kind of request the pinned host ends: an answered one, one refused by each limit or by one
// its text does not name, one whose text both patterns match, one that read the cache, and one on
// a model the project's limit does not count, which is still reported (the daemon selects).
test("request variants", async () => {
  const { reporter, batches, now } = rig();
  const variants: [string, AgentEndMessage, Record<string, unknown>][] = [
    ["clean", answered(now()), { uncachedInputTokens: 18 }],
    ...REFUSAL_CLASSES.cases.map((row): [string, AgentEndMessage, Record<string, unknown>] => [
      row.name,
      refused(now(), row.text),
      { uncachedInputTokens: 0, refusal: row.refusal },
    ]),
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
    if (want.refusal === undefined) expect(request.refusal).toBeUndefined();
  });

  // A message the host wrote itself, which no provider answered — a gate's stop, a request that
  // never went out — is no request, and nor is a failure with another status and no tokens.
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
// controller's. Each is reported once, under its own session; a session that holds no claim of
// this process — a person's own pane — reports nothing.
test("session inventory", async () => {
  const { reporter, batches, now } = rig({
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
  }[] = [
    { name: "role", where: role, credential: implementer },
    {
      name: "subagent",
      where: { session: SUBAGENT_SESSION, subagent: true },
      credential: implementer,
    },
    {
      name: "controller",
      where: { session: CONTROLLER_SESSION, subagent: false },
      credential: controller,
    },
  ];
  for (const row of inventory) reporter.messageEnded(row.where, answered(now()));
  reporter.messageEnded({ session: OTHER_SESSION, subagent: false }, answered(now()));
  reporter.turnEnded();
  await reporter.settled();
  reporter.turnEnded();
  await reporter.settled();

  const reported = batches().flatMap((batch) =>
    (batch.requests as { session: string }[]).map((request) => ({
      session: request.session,
      claimToken: batch.claimToken,
      sessionId: batch.sessionId,
      generation: batch.generation,
    }))
  );
  expect(reported.sort((a, b) => a.session.localeCompare(b.session))).toEqual(
    inventory
      .map((row) => ({
        session: row.where.session,
        claimToken: row.credential.claimToken,
        sessionId: row.credential.sessionID,
        generation: row.credential.generation,
      }))
      .sort((a, b) => a.session.localeCompare(b.session))
  );
});

// Each batch says whether its host reports the 429s its HTTP transport retries by itself, which
// the plugin reads from the host's events and never from a version text: true only where the host
// offers an event for each such attempt. The pinned host offers none, and no event is named until
// the fork's change decides one, so even a host that offers every event is read as without; a
// stand-in host that offers the event the detection is given is read as with it.
test("retried refusals", async () => {
  const STANDIN_EVENT = "standin_retried_attempt";
  const offering = (offered: readonly string[]): HostEvents => ({
    offers: (event) => offered.includes(event),
  });
  const everything: HostEvents = { offers: () => true };
  expect(reportsRetriedRefusals(PINNED_HOST_EVENTS)).toBe(false);
  expect(reportsRetriedRefusals(everything)).toBe(false);
  expect(reportsRetriedRefusals(offering([STANDIN_EVENT]), [STANDIN_EVENT])).toBe(true);
  expect(reportsRetriedRefusals(offering(["message_end"]), [STANDIN_EVENT])).toBe(false);
  expect(reportsRetriedRefusals(PINNED_HOST_EVENTS, [STANDIN_EVENT])).toBe(false);

  const hosts: readonly (readonly [string, boolean, boolean])[] = [
    ["the pinned host", reportsRetriedRefusals(PINNED_HOST_EVENTS), false],
    [
      "a host that offers the event",
      reportsRetriedRefusals(offering([STANDIN_EVENT]), [STANDIN_EVENT]),
      true,
    ],
  ];
  for (const [host, said, want] of hosts) {
    const { reporter, batches, now } = rig({ retriedRefusalsReported: said });
    reporter.messageEnded(role, answered(now()));
    reporter.messageEnded(role, refused(now(), "rate limited"));
    reporter.turnEnded();
    await reporter.settled();
    expect(
      `${host}: ${JSON.stringify(batches().map((batch) => batch.retriedRefusalsReported))}`
    ).toBe(`${host}: ${JSON.stringify([want])}`);
  }
});

// The two patterns come from the launch's environment; a pattern that is unset or empty classes
// nothing as its limit's, so every refusal is unknown, and the own pattern alone classes only own.
test("configured patterns", async () => {
  const configurations: [
    string,
    Readonly<Record<string, string | undefined>>,
    (row: (typeof REFUSAL_CLASSES.cases)[number]) => string,
  ][] = [
    ["both set", PATTERN_ENV, (row) => row.refusal],
    ["unset", {}, () => "unknown"],
    [
      "empty",
      { LEGION_SIGNAL_OWN_REFUSAL: "", LEGION_SIGNAL_PROVIDER_REFUSAL: "" },
      () => "unknown",
    ],
    [
      "own alone",
      { LEGION_SIGNAL_OWN_REFUSAL: REFUSAL_CLASSES.patterns.own },
      (row) => (row.refusal === "own" ? "own" : "unknown"),
    ],
  ];
  for (const [name, env, want] of configurations) {
    const { reporter, batches, now } = rig({ env });
    for (const row of REFUSAL_CLASSES.cases) reporter.messageEnded(role, refused(now(), row.text));
    reporter.turnEnded();
    await reporter.settled();
    const classes = batches().flatMap((batch) =>
      (batch.requests as { refusal?: string }[]).map((request) => request.refusal)
    );
    expect(`${name}: ${classes.join(" ")}`).toBe(
      `${name}: ${REFUSAL_CLASSES.cases.map(want).join(" ")}`
    );
  }
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
  const text = REFUSAL_CLASSES.cases[0]?.text ?? "";
  const refusal = refused(now(), text);
  reporter.messageEnded(role, refusal);
  turnOutcomes.inputArrived(ROLE_SESSION, { text: "Continue the task.", source: "rpc" });
  turnOutcomes.runStarted(ROLE_SESSION);
  const end: AgentEndEvent = { messages: [{ role: "user" }, refusal] };
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
        message: text,
        httpStatus: 429,
      },
    },
  ]);
  expect(posted.filter((post) => post.route === "/legion/v1/claims/model-requests")).toHaveLength(
    1
  );
});
