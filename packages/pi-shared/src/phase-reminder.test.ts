import { expect, test } from "bun:test";
import { EnvelopeSchema } from "@legion/contracts";
import type { LegionIssue } from "@legion/contracts/legion-api";
import {
  createEnvoyClient,
  EnvoyApiError,
  type PublishInput,
  type PublishResult,
} from "@legion/envoy-client/transport";
import { createLegionDaemonClient } from "./daemon-client";
import { createPhaseReminder, type EnvoyPublish, type PhaseReminder } from "./phase-reminder";
import { type PhaseStallEntry, REMINDER } from "./phase-stall";
import { DEFAULT_STALL_LIMITS } from "./stall-limits";

const implementer = {
  sessionID: "ses_implementer",
  tree: "LEGION-208",
  issue: "LEGION-208",
  role: "implementer",
  secret: "secret",
  claimToken: "legion-legion-legion-208-implementer",
  generation: 1,
} as const;

/** A turn the worker ended by saying `text`, as `turnEndOf` classifies it on Oh My Pi. */
const said = (text: string) => ({ kind: "settle", lastAssistantText: text }) as const;

/** This role's own issue record with its claim failed, which is the daemon's word that the
 * session is finished. Only the two fields the record check reads, cast because the record's own
 * schema would demand the whole issue document for them; `rig` below answers no record at all. */
const claimFailed = {
  phase: "implementing",
  workers: { implementer: { claim: { state: "failed" } } },
} as unknown as LegionIssue;

// A daemon that never answers the record check: the check fails open once its bound passes, and
// the read it started is aborted then rather than left open, one request per reminder.
test("the record check aborts a daemon read that outlives its bound, and reminds all the same", async () => {
  const handed: { readonly issue: string; readonly signal: AbortSignal }[] = [];
  const debugged: string[] = [];
  const reminder = createPhaseReminder({
    sendReminder: () => {},
    armTimer: () => {},
    saveEntry: () => {},
    claim: () => implementer,
    readRecord: (issue, signal) => {
      handed.push({ issue, signal });
      return new Promise<LegionIssue | undefined>(() => {});
    },
    publisher: async () => undefined,
    project: "LEGION",
    limits: DEFAULT_STALL_LIMITS,
    log: { debug: (message) => debugged.push(message), warn: () => {} },
    recordBoundMs: 20,
  });
  reminder.apply({ kind: "assignment" });

  const continuation = await reminder.settle(said("Pushed."));

  expect(continuation).toBeString();
  expect(handed).toHaveLength(1);
  expect(handed[0]?.issue).toBe("LEGION-208");
  expect(handed[0]?.signal.aborted).toBe(true);
  expect((handed[0]?.signal.reason as Error).message).toBe("no answer within 20ms");
  expect(debugged).toEqual(["legion: the daemon did not answer the phase reminder's record check"]);
});

/** A reminder over a real daemon client whose fetch is `answer`, recording every route the
 * check asks for and every line it logs at debug. */
function overDaemon(answer: typeof fetch) {
  const routes: string[] = [];
  const debugged: string[] = [];
  const recording = (async (input, init) => {
    routes.push(new URL(input.toString()).pathname);
    return answer(input, init);
  }) as typeof fetch;
  const daemon = createLegionDaemonClient("http://daemon.test", {
    pluginContract: 60,
    fetch: recording,
  });
  const reminder = createPhaseReminder({
    sendReminder: () => {},
    armTimer: () => {},
    saveEntry: () => {},
    claim: () => implementer,
    readRecord: (issue, signal) => daemon.record(issue, { signal }),
    publisher: async () => undefined,
    project: "LEGION",
    limits: DEFAULT_STALL_LIMITS,
    log: { debug: (message) => debugged.push(message), warn: () => {} },
  });
  reminder.apply({ kind: "assignment" });
  return { reminder, routes, debugged };
}

// The check keeps its two branches apart: the route's own 404 for an issue the daemon records
// nothing of is the absent record, and everything else is a read that failed. It asks for its
// own issue's record and never for the whole project.
test("the record check reads one record: the route's 404 is the absent record, a transport failure the read that failed", async () => {
  const absent = overDaemon((async (_input, _init) =>
    Response.json(
      { code: "RECORD_NOT_FOUND", error: "The daemon has no record for LEGION-208" },
      { status: 404 }
    )) as typeof fetch);

  expect(await absent.reminder.settle(said("Pushed."))).toBeString();
  expect(absent.routes).toEqual(["/legion/v1/issues/LEGION-208/record"]);
  expect(absent.debugged).toEqual([
    "legion: the daemon holds no record for this phase reminder's issue",
  ]);

  const failed = overDaemon((async (_input, _init): Promise<Response> => {
    throw new TypeError("fetch failed");
  }) as typeof fetch);

  expect(await failed.reminder.settle(said("Pushed."))).toBeString();
  expect(failed.routes).toEqual(["/legion/v1/issues/LEGION-208/record"]);
  expect(failed.debugged).toEqual([
    "legion: the daemon did not answer the phase reminder's record check",
  ]);
});

interface ArmedTimer {
  readonly callback: () => void | Promise<void>;
  readonly delayMs: number;
}

interface Rig {
  readonly reminder: PhaseReminder;
  /** Every timer the runner armed, in order; a case fires one by calling its callback. */
  readonly timers: ArmedTimer[];
  /** Every reminder handed to `sendReminder`, delivered or not. */
  readonly sent: string[];
  readonly entries: PhaseStallEntry[];
  readonly published: PublishInput[];
  readonly warned: string[];
}

/** The runner over an open phase, with the architect told at the second delivered reminder. The
 * daemon holds no record for the issue unless a case names one, so every record check reminds. */
function rig(options: {
  readonly sendReminder?: (reminder: string) => void | Promise<void>;
  readonly publish?: EnvoyPublish;
  readonly record?: LegionIssue;
  /** The publisher's build, which settles before the publisher is handed over, as a first build
   *  that runs Envoy's token command does; the publisher is at hand at once otherwise. */
  readonly buildPublisher?: () => Promise<void>;
  /** The session's claim: the implementer's throughout unless a case names one. */
  readonly claim?: () => typeof implementer | undefined;
}): Rig {
  const timers: ArmedTimer[] = [];
  const sent: string[] = [];
  const entries: PhaseStallEntry[] = [];
  const published: PublishInput[] = [];
  const warned: string[] = [];
  const reminder = createPhaseReminder({
    sendReminder: (text) => {
      sent.push(text);
      return options.sendReminder?.(text);
    },
    armTimer: (callback, delayMs) => {
      timers.push({ callback, delayMs });
    },
    saveEntry: (entry) => entries.push(entry),
    claim: options.claim ?? (() => implementer),
    readRecord: async () => options.record,
    publisher: async () => {
      await options.buildPublisher?.();
      return async (input) => {
        published.push(input);
        return options.publish === undefined ? {} : options.publish(input);
      };
    },
    project: "legion",
    limits: { ...DEFAULT_STALL_LIMITS, architectAfter: 2 },
    log: { debug: () => {}, warn: (message) => warned.push(message) },
  });
  reminder.apply({ kind: "assignment" });
  return { reminder, timers, sent, entries, published, warned };
}

/** Fires the last timer the runner armed. */
function fireLast(timers: readonly ArmedTimer[]): void | Promise<void> {
  const timer = timers.at(-1);
  if (timer === undefined) throw new Error("no timer is armed");
  return timer.callback();
}

// The acceptance of one run's notice, landing after the worker's own turn reset the count and a
// new run began, must not mark the new run as told: that would silence its notice at every
// threshold until the count reset again.
test("a notice accepted after its run ended acknowledges nothing of the next run", async () => {
  const listener = Promise.withResolvers<Pick<PublishResult, "kept">>();
  const reached = Promise.withResolvers<void>();
  let attempt = 0;
  const { reminder, timers, entries, published } = rig({
    publish: () => {
      attempt += 1;
      if (attempt > 1) return Promise.resolve({});
      reached.resolve();
      return listener.promise;
    },
  });

  // The run's continuation, then the timer reminder that brings the count to the threshold, whose
  // publish the listener has not answered yet.
  expect(await reminder.settle(said("Pushed."))).toBeString();
  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  const delivered = fireLast(timers);
  await reached.promise;
  const firstKey = published[0]?.idempotencyKey;
  expect(firstKey).toMatch(/^[0-9a-f-]{36}$/);

  // The worker answers in the reminder's own turn, and its next settle opens a new run.
  reminder.apply({ kind: "tool-call" });
  expect(await reminder.settle(said("Working on it."))).toBeString();

  listener.resolve({});
  await delivered;
  expect(entries.at(-1)).toEqual({
    state: "open",
    count: 1,
    reported: false,
    lastMessage: "Working on it.",
  });

  // The new run reaches the threshold and tells the architect under a key of its own.
  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  await fireLast(timers);
  expect(published).toHaveLength(2);
  expect(published[1]?.idempotencyKey).toMatch(/^[0-9a-f-]{36}$/);
  expect(published[1]?.idempotencyKey).not.toBe(firstKey);
  expect(entries.at(-1)).toMatchObject({ count: 2, reported: true });
});

// The publisher's first build can run Envoy's token command, and the event loop goes on serving
// the worker meanwhile: a turn that resets the count lands inside that wait. The notice must then
// tell nothing, since its run has ended: read after the wait, it would tell the architect of a
// worker that "has let 0 phase reminders go unanswered". The next run is told as any run is.
test("a notice whose publisher was still being built when the run ended tells nothing, and the next run is told", async () => {
  const building = Promise.withResolvers<void>();
  const built = Promise.withResolvers<void>();
  let builds = 0;
  const { reminder, timers, entries, published } = rig({
    buildPublisher: () => {
      builds += 1;
      if (builds > 1) return Promise.resolve();
      building.resolve();
      return built.promise;
    },
  });

  // The run reaches the threshold; its notice waits for the publisher.
  expect(await reminder.settle(said("Pushed."))).toBeString();
  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  const delivered = fireLast(timers);
  await building.promise;

  // The worker acts meanwhile, which ends the run, and the publisher is built after.
  reminder.apply({ kind: "tool-call" });
  built.resolve();
  await delivered;
  expect(published).toEqual([]);
  expect(entries.at(-1)).toMatchObject({ count: 0, reported: false });

  // The next run reaches the threshold and tells the architect of its two reminders.
  expect(await reminder.settle(said("Working on it."))).toBeString();
  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  await fireLast(timers);
  expect(published).toHaveLength(1);
  expect(published[0]?.message).toContain("has let 2 phase reminders go unanswered");
  expect(entries.at(-1)).toMatchObject({ count: 2, reported: true });
});

// A run whose claim has ended sends no notice, so it builds no publisher for one either: the
// first build can run Envoy's token command, and a broker trade would buy a token for nothing.
test("a run whose claim has ended builds no publisher for its notice", async () => {
  let claimed = true;
  let builds = 0;
  const { reminder, timers, published } = rig({
    claim: () => (claimed ? implementer : undefined),
    buildPublisher: async () => {
      builds += 1;
    },
  });

  // The run reaches the threshold, and its claim ends before the reminder that would tell.
  expect(await reminder.settle(said("Pushed."))).toBeString();
  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  claimed = false;
  await fireLast(timers);
  expect(builds).toBe(0);
  expect(published).toEqual([]);
});

// A failure other than "no live session holds the role" is not dropped: the next delivered
// reminder of the run publishes the same notice again, under the same key, until one is accepted.
test("a notice the listener failed is published again by the run's next reminder, under the same key", async () => {
  let attempt = 0;
  const { reminder, timers, entries, published, warned } = rig({
    publish: async () => {
      attempt += 1;
      if (attempt > 1) return {};
      throw new EnvoyApiError({
        method: "POST",
        url: "http://listener.test/v1/messages/publish",
        status: 503,
        responseBody: JSON.stringify({ error: "the listener is restarting" }),
      });
    },
  });

  expect(await reminder.settle(said("Pushed."))).toBeString();
  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  await fireLast(timers);
  expect(published).toHaveLength(1);
  expect(warned).toEqual(["legion: the architect was not told its worker is stalled"]);
  const failed = entries.at(-1);
  expect(failed).toMatchObject({ count: 2, reported: false });
  expect(failed?.reportKey).toBe(published[0]?.idempotencyKey);

  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  await fireLast(timers);
  expect(published).toHaveLength(2);
  expect(published[1]?.idempotencyKey).toBe(failed?.reportKey);
  expect(entries.at(-1)).toMatchObject({ count: 3, reported: true, reportKey: failed?.reportKey });

  // Accepted once is told once.
  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  await fireLast(timers);
  expect(published).toHaveLength(2);
});

// The notice sends no `keep` field, so a listener keeps it for an architect role that no live
// session holds, and answers 202 kept. That is not the architect being told: the copy waits for
// the next session that holds the role, and is dropped if none takes it in its hour. So the run's
// next delivered reminder publishes the notice again. A listener that names `kept_until` answers
// a repeat of the key with the copy it still holds, so the key stays. A listener of an earlier
// release names no time and answers a repeat as the first copy for 72 hours, even after it
// dropped that copy, so the next attempt mints a new key (dispatch://INFRA-2846, R6).
test("the owner notice is kept", async () => {
  const requests: Record<string, unknown>[] = [];
  // The listener's answers in turn: kept with its time, kept with no time (an earlier release),
  // and kept with its time again.
  const answers: Record<string, unknown>[] = [
    { kept_until: 1_793_003_600_000 },
    {},
    { kept_until: 1_793_003_900_000 },
  ];
  const client = createEnvoyClient({
    baseUrl: "http://listener.test",
    fetch: async (_input, init) => {
      const body = JSON.parse(String(init?.body)) as { readonly topic: string };
      requests.push(body);
      return new Response(
        JSON.stringify({
          event_id: "event-kept",
          source: "agent",
          source_event_id: "agent.ses_implementer.event-kept",
          source_session: "ses_implementer",
          topic: body.topic,
          dedupe_key: "agent.architect.event-kept",
          issued_at: 1,
          payload_summary: "stalled",
          trace_id: "trace-kept",
          kept: true,
          kept_reason: "unclaimed",
          ...answers.shift(),
        }),
        { status: 202, headers: { "Content-Type": "application/json" } }
      );
    },
  });
  const { reminder, timers, entries, published, warned } = rig({
    publish: (input) => client.publish(input),
  });

  expect(await reminder.settle(said("Pushed."))).toBeString();
  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  await fireLast(timers);

  expect(published).toHaveLength(1);
  // The notice asks for no `keep`, and none reaches the listener.
  expect(published[0]).not.toHaveProperty("keep");
  expect(requests[0]).not.toHaveProperty("keep");
  // Nobody was told, and that is not this worker's doing: no warning, and the notice stands.
  expect(warned).toEqual([]);
  const kept = entries.at(-1);
  expect(kept).toMatchObject({ count: 2, reported: false });
  expect(kept?.reportKey).toBe(published[0]?.idempotencyKey);

  // The next reminder publishes again, under the same key. This answer names no time.
  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  await fireLast(timers);
  expect(published).toHaveLength(2);
  expect(published[1]?.idempotencyKey).toBe(kept?.reportKey);
  expect(requests[1]).not.toHaveProperty("keep");
  const timeless = entries.at(-1);
  expect(timeless).toMatchObject({ count: 3, reported: false });
  expect(timeless).not.toHaveProperty("reportKey");

  // So the reminder after it publishes under a new key.
  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  await fireLast(timers);
  expect(published).toHaveLength(3);
  expect(published[2]?.idempotencyKey).toMatch(/^[0-9a-f-]{36}$/);
  expect(published[2]?.idempotencyKey).not.toBe(kept?.reportKey);
  expect(entries.at(-1)).toMatchObject({
    count: 4,
    reported: false,
    reportKey: published[2]?.idempotencyKey,
  });
  expect(warned).toEqual([]);
});

// A listener answers a notice whose key another sender's message already holds with that
// message's ids and times alone (`requestSender.answer` in the listener). Nothing reached the
// architect, so the notice is not reported, its key is cleared, and the next reminder publishes it
// under a new key. The control is the envelope schema every answer was read by before, which
// throws on that answer.
test("a notice whose key another sender's message holds is sent again under a new key", async () => {
  const taken = {
    event_id: "event-other",
    source: "",
    source_event_id: "",
    topic: "notifications.role.legion-legion-208-architect",
    dedupe_key: "publish.key",
    issued_at: 1,
    payload_summary: "",
    trace_id: "",
    duplicate: true,
  };
  expect(() => EnvelopeSchema.parse(taken)).toThrow();
  const client = createEnvoyClient({
    baseUrl: "http://listener.test",
    fetch: async () => Response.json(taken),
  });
  const { reminder, timers, entries, published, warned } = rig({
    publish: (input) => client.publish(input),
  });

  expect(await reminder.settle(said("Pushed."))).toBeString();
  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  await fireLast(timers);

  expect(published).toHaveLength(1);
  expect(warned).toEqual([
    "legion: another sender's message holds the architect notice's key; telling it again later",
  ]);
  const cleared = entries.at(-1);
  expect(cleared).toMatchObject({ count: 2, reported: false });
  expect(cleared).not.toHaveProperty("reportKey");

  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  await fireLast(timers);
  expect(published).toHaveLength(2);
  expect(published[1]?.idempotencyKey).not.toBe(published[0]?.idempotencyKey);
});

// A listener answers 404 with nothing kept when its keep failed, and a listener of an earlier
// release answers it for an architect role whose holder is not live. That is the answer this
// sender's retry exists for. Nobody was told, so the next delivered reminder publishes again
// under the same key, and the worker's transcript gets no warning for something that is not its
// doing.
test("a notice no session held the architect role for is published again by the run's next reminder, under the same key", async () => {
  let attempt = 0;
  const { reminder, timers, entries, published, warned } = rig({
    publish: async () => {
      attempt += 1;
      if (attempt > 1) return {};
      throw new EnvoyApiError({
        method: "POST",
        url: "http://listener.test/v1/messages/publish",
        status: 404,
        responseBody: JSON.stringify({
          error: "no holder for role architect",
          reason: "unclaimed",
        }),
      });
    },
  });

  expect(await reminder.settle(said("Pushed."))).toBeString();
  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  await fireLast(timers);
  expect(published).toHaveLength(1);
  expect(warned).toEqual([]);
  const refused = entries.at(-1);
  expect(refused).toMatchObject({ count: 2, reported: false });

  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  await fireLast(timers);
  expect(published).toHaveLength(2);
  expect(published[1]?.idempotencyKey).toBe(refused?.reportKey);
  expect(entries.at(-1)).toMatchObject({ count: 3, reported: true });
});

// No turn starts on a reminder that never reached the worker, so no settle would arm the next
// one: the runner uncounts it and arms the next itself, and the reminding goes on.
test("a reminder the host failed to deliver is not counted, and the next is armed", async () => {
  let deliveries = 0;
  const { reminder, timers, sent, entries, published, warned } = rig({
    sendReminder: async () => {
      deliveries += 1;
      if (deliveries === 1) throw new Error("the bridge closed the socket");
    },
  });

  expect(await reminder.settle(said("Pushed."))).toBeString();
  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  expect(timers).toHaveLength(1);
  await fireLast(timers);

  expect(warned).toEqual([
    "legion: a phase reminder did not reach this worker; it comes again later",
  ]);
  // Still at the continuation's count: the failed one counted for nothing, so it told no one.
  expect(entries.at(-1)).toMatchObject({ state: "open", count: 1 });
  expect(published).toEqual([]);
  expect(timers).toHaveLength(2);
  expect(timers[1]?.delayMs).toBe(DEFAULT_STALL_LIMITS.reminderMs);

  await fireLast(timers);
  expect(sent).toHaveLength(2);
  expect(sent[1]).toBe(sent[0]);
  expect(entries.at(-1)).toMatchObject({ state: "open", count: 2 });
  expect(published).toHaveLength(1);
});

// A turn that started while the failed write was in flight is a turn whose settle arms the next
// reminder, so the runner arms none of its own on top of it.
test("a failed delivery arms nothing once a turn has started meanwhile", async () => {
  const write = Promise.withResolvers<void>();
  const writing = Promise.withResolvers<void>();
  const { reminder, timers } = rig({
    sendReminder: () => {
      writing.resolve();
      return write.promise;
    },
  });

  expect(await reminder.settle(said("Pushed."))).toBeString();
  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  const delivered = fireLast(timers);
  await writing.promise;
  reminder.turnStarted();
  write.reject(new Error("the bridge closed the socket"));
  await delivered;

  expect(timers).toHaveLength(1);
});

// The whole of dispatch://INFRA-264: every turn of that implementer was refused by the provider,
// and every reminder it was given at once bought another refused turn. So a refused turn earns no
// continuation; but the daemon's task for the phase ended with that turn, and a refusal that can
// clear fails no claim (dispatch://INFRA-4508), so one delayed reminder follows it rather than
// silence (the architect's decision, review round 2).
test("a refused turn reminds once, after the delay and not before", async () => {
  const { reminder, timers, sent, entries } = rig({});
  const recorded = entries.length;

  expect(
    await reminder.settle({ kind: "refused", message: "Overloaded: the model is busy" })
  ).toBeUndefined();

  expect(sent).toEqual([]);
  // Nothing of the state moved, the words the worker last said included: the provider's are not
  // the worker's, and the architect's notice quotes the worker.
  expect(entries).toHaveLength(recorded);
  expect(timers.map((timer) => timer.delayMs)).toEqual([DEFAULT_STALL_LIMITS.reminderMs]);

  await fireLast(timers);

  // Once: the delivered reminder arms no second timer, and the next turn settles its own.
  expect(sent).toEqual([REMINDER]);
  expect(timers).toHaveLength(1);

  // The next turn the worker does answer is reminded as any later settle of a run is: the first
  // reminder of this run has gone out, so this one comes from a timer too.
  expect(await reminder.settle(said("Pushed."))).toBeUndefined();
  expect(timers).toHaveLength(2);
});

// A run of refusals that can clear — a provider overloaded, a team limit — fails no claim, a
// reminder's own turn included: the daemon judges that turn from its turn-outcome report, counts
// it in the claim's run of provider-error turns, and at six in a row sends the `provider-errors`
// notice once (dispatch://INFRA-4508; `providerErrorRunReached` in the daemon's `workflow.go` says
// to whom). So each refused turn arms the next reminder at the same pace, for as long as the run
// lasts, and the worker is reminded again the moment its provider takes a turn. Only the daemon
// failing the claim stops them (the case below).
test("a run of refused turns that can clear keeps arming its reminder", async () => {
  const { reminder, timers, sent } = rig({});

  for (let turn = 1; turn <= 6; turn += 1) {
    reminder.turnStarted();
    expect(
      await reminder.settle({ kind: "refused", message: "529 overloaded_error: Overloaded" })
    ).toBeUndefined();
    expect(timers).toHaveLength(turn);
    expect(timers.at(-1)?.delayMs).toBe(DEFAULT_STALL_LIMITS.reminderMs);
    await fireLast(timers);
    expect(sent).toHaveLength(turn);
  }
});

// A refusal that cannot clear fails the claim, which is the daemon's own word that this session's
// work is the architect's to decide: the daemon fails it at once, whether the refused turn ran its
// prompt or was a reminder's own, which it judges from the turn-outcome report
// (dispatch://INFRA-4508). The timer the refused turn armed still fires, and its record check
// drops the reminder, so nothing is written into a session nobody can act on, and the reminding
// ends.
test("a refused turn whose claim the daemon failed reminds nothing", async () => {
  const { reminder, timers, sent } = rig({ record: claimFailed });

  expect(
    await reminder.settle({ kind: "refused", message: "Refusal (reasoning_extraction)" })
  ).toBeUndefined();

  expect(timers).toHaveLength(1);
  await fireLast(timers);

  expect(sent).toEqual([]);
});

// The daemon failing this role's claim is its word that the session is finished: the retry runs
// in a session of its own, and a reminder written into this one reaches nobody who can act on it.
// It is what stops the reminding on Claude Code, whose turns this host does not settle.
test("no reminder is sent once the daemon has failed this role's claim", async () => {
  const timers: ArmedTimer[] = [];
  const sent: string[] = [];
  const debugged: string[] = [];
  const reminder = createPhaseReminder({
    sendReminder: (text) => {
      sent.push(text);
    },
    armTimer: (callback, delayMs) => {
      timers.push({ callback, delayMs });
    },
    saveEntry: () => {},
    claim: () => implementer,
    readRecord: async () => claimFailed,
    publisher: async () => undefined,
    project: "legion",
    limits: DEFAULT_STALL_LIMITS,
    log: { debug: (message) => debugged.push(message), warn: () => {} },
  });
  // The continuation this first settle of a run would earn: the record check drops it.
  reminder.apply({ kind: "assignment" });
  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  expect(timers).toEqual([]);

  // And the timer's reminder, for a run the worker has already been reminded of once: that one
  // is armed without a check and dropped by the check the timer runs.
  reminder.restore({ state: "open", count: 1, reported: false });
  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  expect(timers).toHaveLength(1);
  await fireLast(timers);

  expect(sent).toEqual([]);
  expect(debugged).toEqual([
    "legion: the daemon has failed this claim; no reminder is sent",
    "legion: the daemon has failed this claim; no reminder is sent",
  ]);
});

// Contract 258: a role of the deployment works the phase `step`, which no role's phase names, so
// the record check reads the issue's `stepRole`: the checker is reminded while its own step runs,
// and not while the step of another role of the deployment runs.
test("a role of the deployment is reminded at step while the issue's stepRole is its own", async () => {
  const checker = {
    ...implementer,
    role: "checker",
    claimToken: "legion-legion-legion-208-checker",
  };
  for (const [stepRole, reminds] of [
    ["checker", true],
    ["linter", false],
  ] as const) {
    const debugged: string[] = [];
    // Only the fields the record check reads, cast as `claimFailed` above is.
    const record = {
      phase: "step",
      stepRole,
      workers: { [stepRole]: { claim: { state: "working" } } },
    } as unknown as LegionIssue;
    const reminder = createPhaseReminder({
      sendReminder: () => {},
      armTimer: () => {},
      saveEntry: () => {},
      claim: () => checker,
      readRecord: async () => record,
      publisher: async () => undefined,
      project: "legion",
      limits: DEFAULT_STALL_LIMITS,
      log: { debug: (message) => debugged.push(message), warn: () => {} },
    });
    reminder.apply({ kind: "assignment" });
    const continuation = await reminder.settle(said("Checked."));
    if (reminds) {
      expect(continuation, `stepRole ${stepRole}`).toBeString();
      expect(debugged).toEqual([]);
    } else {
      expect(continuation, `stepRole ${stepRole}`).toBeUndefined();
      expect(debugged).toEqual([
        "legion: this role no longer works this phase; no reminder is sent",
      ]);
    }
  }
});

// The wait a `ready_for_update` answer creates is `awaiting-update`, and nothing is armed in it:
// the daemon is what wakes the worker, with the task it held behind the round, once the round
// has replaced the pod. A timer fired in that wait would steer a real turn into the worker
// instead: its work would go with the pod, and before the round's bound a running turn holds the
// replacement back, so a reminder every `waitingMs` would keep the round from ending early. The
// WAITING reply's own wait is untouched: it is the reminder's to time out.
test("the wait an update answer creates arms no reminder, where a WAITING reply still arms one", async () => {
  const { reminder, timers, sent, entries } = rig({});

  reminder.apply({ kind: "update-answered" });
  expect(await reminder.settle(said("Pushed and answered."))).toBeUndefined();

  expect(timers).toEqual([]);
  expect(sent).toEqual([]);
  // The record holds the wait as the plain `waiting` it is a kind of: nothing restores a record
  // without a relaunch, and that relaunch is this round over (`recordedPhaseStall`).
  expect(entries.at(-1)).toMatchObject({ state: "waiting", count: 0 });

  // A second turn in the same wait — a question answered while the pod waits to be replaced —
  // arms none either.
  expect(await reminder.settle(said("Still waiting for the pod."))).toBeUndefined();
  expect(timers).toEqual([]);

  // The daemon wakes the worker with the task it held behind the round: the phase is open again,
  // and a WAITING reply buys the quiet wait the reminder itself ends.
  reminder.apply({ kind: "inbound-event" });
  expect(await reminder.settle(said("WAITING on the reviewer."))).toBeUndefined();

  expect(timers).toHaveLength(1);
  expect(timers[0]?.delayMs).toBe(DEFAULT_STALL_LIMITS.waitingMs);
});
