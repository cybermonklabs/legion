import { expect, test } from "bun:test";
import { createLegionDaemonClient } from "@legion/pi-shared/daemon-client";
import type { AgentEndEvent, AgentEndMessage } from "@legion/pi-shared/pi-types";
import {
  MODEL_REQUEST_DUE_AFTER_MS,
  MODEL_REQUEST_REPORT_BOUND_MS,
  MODEL_REQUEST_REPORT_TIMEOUT_MS,
  MODEL_REQUEST_SEND_MARGIN_MS,
  type ModelRequestCredential,
} from "./model-requests";
import {
  answered,
  CONTRACT,
  CONTROLLER_SESSION,
  controller,
  credentialOf,
  implementer,
  ROLE_SESSION,
  rig,
  role,
  START,
  SUBAGENT_SESSION,
} from "./model-requests-test-support";
import { createTurnOutcomeReporter } from "./turn-outcome";

const OTHER_SESSION = "ses_person_pane";

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

// The turn's end sends the fresh request pending at once, and the timer it had armed sends nothing
// more.
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

  // A failed call holds the requests it put back for a bound after its answer, and then both go
  // again.
  await advance(MODEL_REQUEST_REPORT_BOUND_MS - 1);
  expect(sessions()).toEqual([CONTROLLER_SESSION, ROLE_SESSION]);
  await advance(1);
  expect(sessions().slice(2).sort()).toEqual([CONTROLLER_SESSION, ROLE_SESSION]);
  await advance(MODEL_REQUEST_REPORT_BOUND_MS);
  expect(sessions()).toHaveLength(4);
});

// A failed call holds only the requests it put back: a request no call carried keeps its own
// deadline (`end + MODEL_REQUEST_DUE_AFTER_MS`), even while a put-back request is waiting out its
// bound. Both a request that ended while a stalled call was open and one that ended right after a
// failed call keep their own deadlines and their send margin; the put-back request goes with that
// send, in its one report of put-back requests, after the report of the request whose deadline it
// is.
test("a request no call carried goes by its own deadline even while a failed call's request waits", async () => {
  const sentAt: { ids: string[]; at: number }[] = [];
  const { reporter, advance, now } = rig({
    answer: async (body, attempt, hold) => {
      const ids = (body.requests as { id: string }[]).map(({ id }) => id);
      sentAt.push({ ids, at: now() });
      if (attempt === 1) {
        await hold(Number.POSITIVE_INFINITY);
      }
      if (attempt === 4) {
        return Response.json({ error: "unavailable" }, { status: 503 });
      }
      return Response.json({ kept: ids });
    },
  });

  // Call 1 carries req-1 at 0 s and stalls until its 10 s abort, putting req-1 back due at 25 s.
  // Req-2 ends at 1 s and no call carried it: it is due at 15.75 s and must not wait until 25 s.
  reporter.messageEnded(role, answered(now()));
  reporter.turnEnded();
  await advance(1_000);
  const req2End = now();
  reporter.messageEnded(role, answered(req2End));

  await advance(MODEL_REQUEST_REPORT_TIMEOUT_MS - 1_000);
  // Call 1 has aborted at 10 s; req-1 is held until 25 s, req-2 is due at 15.75 s.
  expect(sentAt).toEqual([{ ids: ["req-1"], at: START }]);
  await advance(req2End + MODEL_REQUEST_DUE_AFTER_MS - now() - 1);
  expect(sentAt).toHaveLength(1);
  await advance(1);
  expect(sentAt.slice(1)).toEqual([
    { ids: ["req-2"], at: req2End + MODEL_REQUEST_DUE_AFTER_MS },
    { ids: ["req-1"], at: req2End + MODEL_REQUEST_DUE_AFTER_MS },
  ]);

  // Req-3 fails immediately with 503 (put back due +15 s); req-4 ends 100 ms later (less than the
  // 250 ms margin) and still keeps its own deadline with full margin, not the put-back's +15 s.
  reporter.messageEnded(role, answered(now()));
  reporter.turnEnded();
  await reporter.settled();
  expect(sentAt).toHaveLength(4);
  await advance(100);
  const req4End = now();
  reporter.messageEnded(role, answered(req4End));
  await advance(MODEL_REQUEST_DUE_AFTER_MS - 1);
  expect(sentAt).toHaveLength(4);
  await advance(1);
  expect(sentAt.slice(4)).toEqual([
    { ids: ["req-4"], at: req4End + MODEL_REQUEST_DUE_AFTER_MS },
    { ids: ["req-3"], at: req4End + MODEL_REQUEST_DUE_AFTER_MS },
  ]);
});

// A run's end (`agent_end`) makes a send at once and drops the timer, whatever failed before: a
// fresh request leaves, and a put-back request rides in its owner's one report of put-back requests
// ("When it is sent" in `./model-requests.ts`), each in a report of its own, the earliest
// deadline's first.
test("a run's end sends what is pending at once even while a failed call's request waits", async () => {
  const sentAt: { ids: string[]; at: number }[] = [];
  const { reporter, advance, now } = rig({
    answer: (body, attempt) => {
      const ids = (body.requests as { id: string }[]).map(({ id }) => id);
      sentAt.push({ ids, at: now() });
      if (attempt === 1 || attempt === 4) {
        return Response.json({ error: "unavailable" }, { status: 503 });
      }
      return Response.json({ kept: ids });
    },
  });

  // Call 1 fails with 503 at 0 s; req-1 is put back due at 15 s. Req-2 ends at 1 s and the run
  // ends at 2 s: both leave at 2 s, and the armed timer is dropped.
  reporter.messageEnded(role, answered(now()));
  reporter.turnEnded();
  await reporter.settled();
  expect(sentAt).toEqual([{ ids: ["req-1"], at: START }]);

  await advance(1_000);
  reporter.messageEnded(role, answered(now()));
  await advance(1_000);
  const firstTurnEnd = now();
  reporter.turnEnded();
  await reporter.settled();
  expect(sentAt.slice(1)).toEqual([
    { ids: ["req-1"], at: firstTurnEnd },
    { ids: ["req-2"], at: firstTurnEnd },
  ]);

  // Even with only a put-back request pending, a run's end sends it at once, in its owner's one
  // report of put-back requests, instead of leaving it on an unref'd timer.
  reporter.messageEnded(role, answered(now()));
  reporter.turnEnded();
  await reporter.settled();
  expect(sentAt).toHaveLength(4);
  await advance(2_000);
  const endAt = now();
  reporter.turnEnded();
  await reporter.settled();
  expect(sentAt).toHaveLength(5);
  expect(sentAt[4]).toEqual({ ids: ["req-3"], at: endAt });

  await advance(MODEL_REQUEST_REPORT_BOUND_MS * 2);
  expect(sentAt).toHaveLength(5);
});

// A call that fails before anything leaves (`deps.daemon()` throws) is a failed call whose batch
// goes back under its ids, held for a bound. The run's end that sends it returns without looping,
// nothing reaches the daemon, the send warns once, and nothing goes again before the hold ends.
test("a call that fails before it leaves is a failed call, and the run's end returns", async () => {
  const { reporter, posted, warned, advance, now } = rig({
    daemon: () => {
      throw new Error("LEGION_DAEMON_URL is not set");
    },
  });
  reporter.messageEnded(role, answered(now()));
  reporter.turnEnded();
  await reporter.settled();
  expect(posted).toEqual([]);
  expect(warned).toEqual([
    "legion: a report of model requests did not reach the daemon; it goes again",
  ]);

  // Nothing goes again before the hold ends; then the request goes again, and fails again.
  await advance(MODEL_REQUEST_REPORT_BOUND_MS - 1);
  expect(warned).toHaveLength(1);
  await advance(1);
  expect(posted).toEqual([]);
  expect(warned).toHaveLength(2);
});

// A request the contract's per-request schema refuses as the daemon's `modelRequestsProblem` does,
// with a 400 for the whole report — a session of "", or a model of "", none or one only spaces — is
// dropped at `messageEnded` with one warning that names its id and whose reason names the refused
// field first, never silently, so it never shares a batch: its owner's valid request beside it goes
// on time, alone in its report.
test("a request the contract refuses is dropped at messageEnded with a warning, and its owner's valid request goes on time alone", async () => {
  const { model: _model, ...noModel } = answered(START);
  const refused: readonly {
    readonly name: string;
    readonly where: { readonly session: string; readonly subagent: boolean };
    readonly message: unknown;
    readonly field: string;
  }[] = [
    {
      name: 'session ""',
      where: { session: "", subagent: false },
      message: answered(START),
      field: "session",
    },
    { name: 'model ""', where: role, message: answered(START, undefined, ""), field: "model" },
    { name: "model absent", where: role, message: noModel, field: "model" },
    { name: 'model " "', where: role, message: answered(START, undefined, " "), field: "model" },
  ];
  for (const { name, where, message, field } of refused) {
    const { reporter, batches, warnings, advance, now } = rig({ credential: () => implementer });
    const validEnd = now();
    reporter.messageEnded(role, answered(validEnd));
    reporter.messageEnded(where, message);
    expect(
      warnings.map(
        ({ message: text, context }) =>
          `${text}; id ${String(context?.id)}; reason names ${field}: ${String(context?.reason).startsWith(`${field}: `)}`
      ),
      name
    ).toEqual([
      `legion: dropped a model request the contract refuses; id req-2; reason names ${field}: true`,
    ]);

    await advance(MODEL_REQUEST_DUE_AFTER_MS - 1);
    expect(batches(), name).toEqual([]);
    await advance(1);
    expect(
      batches().map((batch) => (batch.requests as { id: string }[]).map(({ id }) => id)),
      name
    ).toEqual([["req-1"]]);
    expect(now() - validEnd, name).toBe(MODEL_REQUEST_DUE_AFTER_MS);

    await advance(MODEL_REQUEST_REPORT_BOUND_MS * 2);
    expect(batches(), name).toHaveLength(1);
    expect(warnings, name).toHaveLength(1);
  }
});

// The 4xx rule ("When it is sent" in `./model-requests.ts`), each branch. A 408 or a 429 is a
// failed call whoever sent it and whatever its body, the daemon's refusal shape or a proxy's page:
// its request goes again after the hold. Any other 4xx will not pass on a retry, the daemon's own
// (a 409 in its refusal shape) or a proxy's (a 403 page): its request goes once and is dropped
// with a warning.
test("a 408 or a 429 goes again after the hold whoever sent it, and any other 4xx is dropped", async () => {
  const goesAgain = "legion: a report of model requests did not reach the daemon; it goes again";
  const unreported = (status: number) =>
    `legion: a report of model requests was refused with ${status}; they go unreported`;
  const daemonRefusal = (status: number) =>
    Response.json({ error: `the daemon answers ${status}` }, { status });
  const proxyPage = (status: number) =>
    new Response(`<html><body><h1>${status}</h1></body></html>`, {
      status,
      headers: { "Content-Type": "text/html" },
    });
  for (const { name, status, first, again } of [
    { name: "the daemon's 408", status: 408, first: daemonRefusal, again: true },
    { name: "the daemon's 429", status: 429, first: daemonRefusal, again: true },
    { name: "a proxy's 429", status: 429, first: proxyPage, again: true },
    { name: "the daemon's 409", status: 409, first: daemonRefusal, again: false },
    { name: "a proxy's 403", status: 403, first: proxyPage, again: false },
  ]) {
    const { reporter, batches, warned, advance, now } = rig({
      answer: (body, attempt) =>
        attempt === 1
          ? first(status)
          : Response.json({ kept: (body.requests as { id: string }[]).map(({ id }) => id) }),
    });
    reporter.messageEnded(role, answered(now()));
    reporter.turnEnded();
    await reporter.settled();
    expect(warned, name).toEqual([again ? goesAgain : unreported(status)]);
    await advance(MODEL_REQUEST_REPORT_BOUND_MS - 1);
    expect(batches(), name).toHaveLength(1);
    await advance(1);
    expect(batches(), name).toHaveLength(again ? 2 : 1);
    await advance(MODEL_REQUEST_REPORT_BOUND_MS * 2);
    expect(batches(), name).toHaveLength(again ? 2 : 1);
    expect(warned, name).toHaveLength(1);
  }
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
  const { reporter, batches, advance, now } = rig({ credential: credentialOf });
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
