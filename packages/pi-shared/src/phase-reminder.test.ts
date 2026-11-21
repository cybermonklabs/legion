import { expect, test } from "bun:test";
import type { LegionIssue, LegionTake } from "@legion/contracts/legion-api";
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
import { createTaskTake } from "./task-take";

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
    publisher: () => undefined,
    project: "LEGION",
    limits: DEFAULT_STALL_LIMITS,
    log: { debug: (message) => debugged.push(message), warn: () => {} },
    taskPending: () => false,
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
    publisher: () => undefined,
    project: "LEGION",
    limits: DEFAULT_STALL_LIMITS,
    log: { debug: (message) => debugged.push(message), warn: () => {} },
    taskPending: () => false,
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
  readonly taskPending?: () => boolean | Promise<boolean>;
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
    claim: () => implementer,
    readRecord: async () => options.record,
    publisher: () => async (input) => {
      published.push(input);
      return options.publish === undefined ? {} : options.publish(input);
    },
    project: "legion",
    limits: { ...DEFAULT_STALL_LIMITS, architectAfter: 2 },
    log: { debug: () => {}, warn: (message) => warned.push(message) },
    taskPending: options.taskPending ?? (() => false),
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
// continuation; but the daemon's task for the phase ended with that turn, and below
// `launch_failure_limit` nothing fails the claim and nothing hands the task out again, so one
// delayed reminder follows it rather than silence (the architect's decision, review round 2).
test("a refused turn below the limit reminds once, after the delay and not before", async () => {
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

// At `launch_failure_limit` the daemon fails the claim, which is its own word that this session
// is finished: the retry runs in a session of its own, in which the phase's task is given again.
// The timer the last refused turn armed still fires, and its record check drops the reminder, so
// nothing is written into a session nobody can act on.
test("a refused turn at the limit, whose claim the daemon failed, reminds nothing", async () => {
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
    publisher: () => undefined,
    project: "legion",
    limits: DEFAULT_STALL_LIMITS,
    log: { debug: (message) => debugged.push(message), warn: () => {} },
    taskPending: () => false,
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

// A worker the daemon relaunched into an open phase, its session continuing, that finished the
// last task written to it is given nothing at the relaunch (R4 of INFRA-2811): its own reminder is
// what wakes it. One reminder turn comes after `resumeMs`, and none before it; a task the claim
// still owes is taken once the ready has answered, and its turn makes the timer stale; a phase the
// record holds closed arms nothing.
test("a resumed session whose phase is open gets one reminder turn after resumeMs", async () => {
  const { reminder, timers, sent } = rig({});
  reminder.restore({ state: "open", count: 0, reported: false });

  reminder.armResumeReminder();

  expect(timers.map((timer) => timer.delayMs)).toEqual([DEFAULT_STALL_LIMITS.resumeMs]);
  expect(sent).toEqual([]);
  await fireLast(timers);
  expect(sent).toHaveLength(1);
  expect(sent[0]).toContain(REMINDER);

  // A turn that starts first — a task the claim still owed, taken once its ready answered —
  // leaves the resume timer stale, and it reminds nothing.
  const resumed = rig({});
  resumed.reminder.restore({ state: "open", count: 0, reported: false });
  resumed.reminder.armResumeReminder();
  resumed.reminder.turnStarted();
  await fireLast(resumed.timers);
  expect(resumed.sent).toEqual([]);

  const closed = rig({});
  closed.reminder.restore(undefined);
  closed.reminder.armResumeReminder();
  expect(closed.timers).toEqual([]);
});

const EPOCH = "2026-11-13T16:21:44.123456789Z";
const TASK_TEXT = "Task 4 of LEGION-208: implement the change.";

/** Lets every promise queued so far settle. */
async function flush(): Promise<void> {
  for (let turn = 0; turn < 50; turn += 1) await Promise.resolve();
}

/**
 * The implementer's take of its claim's task (`src/task-take.ts`), ready, over a daemon that
 * answers each take only when a case answers it (`answer`, the take asked last). No attempt's
 * bound passes, so a take nobody answered stays in flight.
 */
function claimTake() {
  const answers: ((answer: LegionTake) => void)[] = [];
  const ran: string[] = [];
  const take = createTaskTake({
    caller: {
      claimToken: implementer.claimToken,
      sessionId: implementer.sessionID,
      secret: implementer.secret,
      generation: implementer.generation,
    },
    daemon: {
      taskSeen: async () => undefined,
      take: () => {
        const { promise, resolve } = Promise.withResolvers<LegionTake>();
        answers.push(resolve);
        return promise;
      },
    },
    run: (text) => {
      ran.push(text);
    },
    setPending: () => {},
    log: { info: () => {}, warn: () => {} },
    wait: (_ms, signal) => {
      const { promise, resolve } = Promise.withResolvers<boolean>();
      if (signal.aborted) resolve(false);
      signal.addEventListener("abort", () => resolve(false), { once: true });
      return promise;
    },
  });
  take.ready();
  return {
    take,
    ran,
    /** How many takes the daemon was asked. */
    asked: () => answers.length,
    /** The daemon's write of task `number` at `revision`, relayed to the session. */
    relay: async (number: number, revision: number) => {
      take.relayed({
        claim: implementer.claimToken,
        epoch: EPOCH,
        revision,
        op: "put",
        task: number,
      });
      await flush();
    },
    /** The daemon's answer to the take asked last. */
    answer: async (outcome: LegionTake) => {
      answers.at(-1)?.(outcome);
      await flush();
    },
  };
}

// dispatch://INFRA-4130 R6: while the take holds a task value it will ask about, or a take is in
// flight or accepted and its turn has not started, the worker starts no reminder turn: the task is
// the next turn, and the reminder has nothing to add. A hold that refuses the take still lets the
// reminder come, as at the base.
test("a settle while the take holds a task value returns no continuation, and a timer reminder sends nothing while the take is pending", async () => {
  // S7: a value arrives during the run and is not asked about: the settle arms the timer at the
  // reminder's delay with the same reminder, and the count does not move.
  const unasked = claimTake();
  const quiet = rig({ taskPending: () => unasked.take.pending() });
  unasked.take.runStarted();
  await unasked.relay(4, 12);
  expect(unasked.asked()).toBe(0);
  expect(await quiet.reminder.settle(said("Pushed."))).toBeUndefined();
  expect(quiet.timers.map((timer) => timer.delayMs)).toEqual([DEFAULT_STALL_LIMITS.reminderMs]);
  expect(quiet.entries.at(-1)).toMatchObject({ state: "open", count: 0 });
  // The run ends, and the take is asked: the timer that fires while it is in flight sends nothing.
  unasked.take.runEnded(false);
  await flush();
  expect(unasked.asked()).toBe(1);
  await fireLast(quiet.timers);
  expect(quiet.sent).toEqual([]);
  expect(quiet.entries.at(-1)).toMatchObject({ state: "open", count: 0 });
  // The take's text is the next turn.
  await unasked.answer({ outcome: "accepted", text: TASK_TEXT });
  expect(unasked.ran).toEqual([TASK_TEXT]);

  // S8: a take asked while the host was idle is in flight when a run that started meanwhile
  // settles.
  const inFlight = claimTake();
  const flying = rig({ taskPending: () => inFlight.take.pending() });
  await inFlight.relay(4, 12);
  expect(inFlight.asked()).toBe(1);
  inFlight.take.runStarted();
  flying.reminder.turnStarted();
  expect(await flying.reminder.settle(said("Pushed."))).toBeUndefined();
  expect(flying.timers.map((timer) => timer.delayMs)).toEqual([DEFAULT_STALL_LIMITS.reminderMs]);

  // S9: the take was accepted during the run, and its text waits for the run's end.
  const accepted = claimTake();
  const holding = rig({ taskPending: () => accepted.take.pending() });
  await accepted.relay(4, 12);
  accepted.take.runStarted();
  await accepted.answer({ outcome: "accepted", text: TASK_TEXT });
  expect(accepted.ran).toEqual([]);
  expect(await holding.reminder.settle(said("Pushed."))).toBeUndefined();
  accepted.take.runEnded(false);
  expect(accepted.ran).toEqual([TASK_TEXT]);

  // S10: the take was refused at this revision by a hold other than `in-turn`: the daemon writes
  // the value again once that hold ends, so nothing is pending and the continuation comes.
  const held = claimTake();
  const reminded = rig({ taskPending: () => held.take.pending() });
  await held.relay(4, 12);
  await held.answer({ outcome: "refused", hold: "issue-in-turn" });
  held.take.runStarted();
  expect(await reminded.reminder.settle(said("Pushed."))).toBe(REMINDER);
  expect(reminded.timers).toEqual([]);
  expect(reminded.entries.at(-1)).toMatchObject({ state: "open", count: 1 });

  // S10a: the take asked while the host was idle was overtaken by this run and refused `in-turn`
  // during it: the daemon writes the value again once it hears this run's end, so the take is
  // pending past that end, until the daemon's write or the bound (`IN_TURN_REWRITE_BOUND_MS`).
  const inTurn = claimTake();
  const waiting = rig({ taskPending: () => inTurn.take.pending() });
  await inTurn.relay(4, 12);
  inTurn.take.runStarted();
  await inTurn.answer({ outcome: "refused", hold: "in-turn" });
  expect(await waiting.reminder.settle(said("Pushed."))).toBeUndefined();
  expect(waiting.entries.at(-1)).toMatchObject({ state: "open", count: 0 });
});

test("a reminder skipped for a pending take is armed again, and reminds once the take is refused by a hold", async () => {
  const claim = claimTake();
  // Claude Code's host answers over the bridge, so its answer is a promise.
  const { reminder, timers, sent, entries } = rig({
    taskPending: async () => claim.take.pending(),
  });
  // The run's continuation, then the settle of the turn it bought, which arms the timer.
  expect(await reminder.settle(said("Pushed."))).toBe(REMINDER);
  reminder.turnStarted();
  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  expect(timers.map((timer) => timer.delayMs)).toEqual([DEFAULT_STALL_LIMITS.reminderMs]);

  // The host is idle and a value arrives: the take is asked at once, and is in flight.
  await claim.relay(4, 12);
  expect(claim.asked()).toBe(1);

  // S11: the timer fires while the take is pending: nothing is sent, the count stands, and the
  // timer is armed again at the reminder's delay, as often as it fires.
  await fireLast(timers);
  expect(sent).toEqual([]);
  expect(entries.at(-1)).toMatchObject({ state: "open", count: 1 });
  expect(timers.map((timer) => timer.delayMs)).toEqual([
    DEFAULT_STALL_LIMITS.reminderMs,
    DEFAULT_STALL_LIMITS.reminderMs,
  ]);
  expect(reminder.phaseWake().nextReminderAt).not.toBeNull();
  await fireLast(timers);
  expect(sent).toEqual([]);
  expect(timers).toHaveLength(3);

  // S12: the daemon refuses the take for another role's turn: nothing is pending, and the timer
  // reminds as at the base, with the same reminder.
  await claim.answer({ outcome: "refused", hold: "issue-in-turn" });
  await fireLast(timers);
  expect(sent).toEqual([REMINDER]);
  expect(entries.at(-1)).toMatchObject({ state: "open", count: 2 });
  expect(timers).toHaveLength(3);

  // The resume reminder goes the same way: skipped while the take is pending and armed again at
  // the reminder's delay, then sent once the take is refused.
  const resumedClaim = claimTake();
  const resumed = rig({ taskPending: () => resumedClaim.take.pending() });
  resumed.reminder.restore({ state: "open", count: 0, reported: false });
  await resumedClaim.relay(4, 12);
  resumed.reminder.armResumeReminder();
  await fireLast(resumed.timers);
  expect(resumed.sent).toEqual([]);
  expect(resumed.timers.map((timer) => timer.delayMs)).toEqual([
    DEFAULT_STALL_LIMITS.resumeMs,
    DEFAULT_STALL_LIMITS.reminderMs,
  ]);
  await resumedClaim.answer({ outcome: "refused", hold: "stale-plugin" });
  await fireLast(resumed.timers);
  expect(resumed.sent).toEqual([REMINDER]);
});

// The running task's own number written again at a newer revision during its turn (see the
// task-take test "pending() is false for a newer revision of the task whose turn is running"): no
// take follows it, so the settle gives the run's continuation as at the base, rather than holding
// it for a take that never comes and reminding only after the reminder's whole delay.
test("a newer revision of the running task's own number withholds no continuation", async () => {
  const claim = claimTake();
  const { reminder, timers, sent } = rig({ taskPending: () => claim.take.pending() });
  await claim.relay(4, 12);
  await claim.answer({ outcome: "accepted", text: TASK_TEXT });
  claim.take.runStarted();
  reminder.turnStarted();
  await claim.relay(4, 13);
  expect(claim.take.noteFor()).toBeUndefined();

  const continuation = await reminder.settle(said("Pushed."));
  claim.take.runEnded(false);
  await flush();
  expect(claim.asked()).toBe(1);
  expect(claim.ran).toEqual([TASK_TEXT]);
  expect({ continuation, timers: timers.map((t) => t.delayMs), sent }).toEqual({
    continuation: REMINDER,
    timers: [],
    sent: [],
  });
});

// The running task's own number written again while the host set its text up, before its
// agent_start: the idle host asks it at once, and the task's turn starts before the daemon
// answers. That take brings no turn (see the task-take test "pending() is false for a take in
// flight at a newer revision of the task whose turn is running"), so the settle of the task's own
// run gives its continuation, as for a newer revision not asked about.
test("a take in flight at a newer revision of the running task's own number withholds no continuation", async () => {
  const claim = claimTake();
  const { reminder, timers, sent } = rig({ taskPending: () => claim.take.pending() });
  await claim.relay(4, 12);
  await claim.answer({ outcome: "accepted", text: TASK_TEXT });
  expect(claim.ran).toEqual([TASK_TEXT]);
  await claim.relay(4, 13);
  expect(claim.asked()).toBe(2);
  claim.take.runStarted();
  reminder.turnStarted();
  expect(claim.take.noteFor()).toBeUndefined();

  const continuation = await reminder.settle(said("Pushed."));
  expect({ continuation, timers: timers.map((t) => t.delayMs), sent }).toEqual({
    continuation: REMINDER,
    timers: [],
    sent: [],
  });
});
