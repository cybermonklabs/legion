import { expect, test } from "bun:test";
import { LEGION_PHASES, type LegionOwed } from "@legion/contracts/legion-api";
import {
  createEnvoyClient,
  EnvoyApiError,
  type PublishInput,
  type PublishResult,
} from "@legion/envoy-client/transport";
import { createLegionDaemonClient, type OwedInput } from "./daemon-client";
import type { ClaimCapability } from "./legion-phase-ops";
import { PHASE_OPS } from "./legion-phase-ops";
import { createPhaseReminder, NUDGE_WHEN_OWED_UNREAD, type PhaseReminder } from "./phase-reminder";
import {
  owedReminder,
  type PhaseStallEntry,
  phaseStallEntryFrom,
  reminderText,
  UNREAD_REMINDER,
} from "./phase-stall";
import { DEFAULT_STALL_LIMITS } from "./stall-limits";

const implementer = {
  sessionID: "ses_implementer",
  claimToken: "legion-legion-legion-208-implementer",
  generation: 5,
  tree: "LEGION-208",
  issue: "LEGION-208",
  role: "implementer",
  secret: "secret",
} as const;

/** What the daemon calls the work it answers is owed (`LegionOwed.work`). */
const WORK = "the implementing phase of LEGION-208";
const OWES: LegionOwed = { owes: true, work: WORK };
const OWES_NOTHING: LegionOwed = { owes: false, work: "" };

/** The phase names and operation names a sentence carries, as whole words. A sentence the
 * reminder prints without the daemon's answer must carry none: it read neither. */
function phasesAndOperations(text: string): string[] {
  const words = new Set(text.split(/[^A-Za-z_]+/));
  return [...LEGION_PHASES, ...PHASE_OPS, "phase"].filter((word) => words.has(word));
}

// A daemon that never answers the owed check: the check fails open once its bound passes, and the
// request it started is aborted then rather than left open, one request per reminder.
test("the owed check aborts a daemon request that outlives its bound, and reminds all the same", async () => {
  const handed: AbortSignal[] = [];
  const asked: OwedInput[] = [];
  const debugged: string[] = [];
  const reminder = createPhaseReminder({
    sendReminder: () => {},
    armTimer: () => {},
    saveEntry: () => {},
    claim: () => implementer,
    readOwed: (input, signal) => {
      asked.push(input);
      handed.push(signal);
      return new Promise<LegionOwed>(() => {});
    },
    publisher: () => undefined,
    project: "LEGION",
    limits: DEFAULT_STALL_LIMITS,
    log: { debug: (message) => debugged.push(message), warn: () => {} },
    owedBoundMs: 20,
  });

  const continuation = await reminder.settle("Pushed.");

  expect(continuation).toBe(UNREAD_REMINDER);
  // The claim asks about itself, authenticated as its other claim routes are.
  expect(asked).toEqual([
    {
      claimToken: "legion-legion-legion-208-implementer",
      sessionId: "ses_implementer",
      secret: "secret",
    },
  ]);
  expect(handed).toHaveLength(1);
  expect(handed[0]?.aborted).toBe(true);
  expect((handed[0]?.reason as Error).message).toBe("no answer within 20ms");
  expect(debugged).toEqual(["legion: the daemon did not say what this claim owes"]);
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
  /** How many times the daemon was asked what the claim owes. */
  readonly asked: () => number;
}

/** The runner made afresh, as a process the daemon has just started makes it, with the architect
 * told at the second delivered reminder. The daemon answers `owed` — by default, that the claim
 * owes `WORK` — and holds no record for the issue, so a notice goes to the tree's architect. */
function rig(options: {
  readonly sendReminder?: (reminder: string) => void | Promise<void>;
  readonly publish?: (input: PublishInput) => Promise<Pick<PublishResult, "kept">>;
  readonly owed?: (input: OwedInput, signal: AbortSignal) => Promise<LegionOwed>;
  readonly restore?: PhaseStallEntry;
  /** The bound on the owed check, when a case's daemon hangs past it. */
  readonly owedBoundMs?: number;
  /** The session's claim; the implementer of LEGION-208, a tree root, when it is left out. */
  readonly claim?: ClaimCapability;
}): Rig {
  const timers: ArmedTimer[] = [];
  const sent: string[] = [];
  const entries: PhaseStallEntry[] = [];
  const published: PublishInput[] = [];
  const warned: string[] = [];
  let asked = 0;
  const reminder = createPhaseReminder({
    sendReminder: (text) => {
      sent.push(text);
      return options.sendReminder?.(text);
    },
    armTimer: (callback, delayMs) => {
      timers.push({ callback, delayMs });
    },
    saveEntry: (entry) => entries.push(entry),
    claim: () => options.claim ?? implementer,
    readOwed: (input, signal) => {
      asked += 1;
      return options.owed === undefined ? Promise.resolve(OWES) : options.owed(input, signal);
    },
    publisher: () => async (input) => {
      published.push(input);
      return options.publish === undefined ? {} : options.publish(input);
    },
    project: "legion",
    limits: { ...DEFAULT_STALL_LIMITS, architectAfter: 2 },
    log: { debug: () => {}, warn: (message) => warned.push(message) },
    ...(options.owedBoundMs === undefined ? {} : { owedBoundMs: options.owedBoundMs }),
  });
  if (options.restore !== undefined) reminder.restore(options.restore);
  return { reminder, timers, sent, entries, published, warned, asked: () => asked };
}

/** Fires the last timer the runner armed. */
function fireLast(timers: readonly ArmedTimer[]): void | Promise<void> {
  const timer = timers.at(-1);
  if (timer === undefined) throw new Error("no timer is armed");
  return timer.callback();
}

// R5: the daemon's answer decides, each way, at the continuation and at every timer after it.
test("a claim the daemon says owes work is reminded of the work the daemon named", async () => {
  const { reminder, timers, sent } = rig({});

  const continuation = await reminder.settle("Pushed.");
  expect(continuation).toBe(reminderText("Pushed.", WORK));
  expect(continuation).toContain(`Your turn ended with ${WORK} still owed`);

  expect(await reminder.settle("Still here.")).toBeUndefined();
  await fireLast(timers);
  expect(sent).toEqual([reminderText("Still here.", WORK)]);
});

test("a claim the daemon says owes nothing is reminded of nothing, at the continuation or a timer", async () => {
  const { reminder, timers, sent, asked } = rig({ owed: async () => OWES_NOTHING });

  expect(await reminder.settle("Pushed.")).toBeUndefined();
  expect(await reminder.settle("Still here.")).toBeUndefined();
  // A quiet wait is still armed — the rhythm is the reminder's own — and the daemon is asked
  // again when it is due, and answers the same.
  expect(await reminder.settle("WAITING on CI.")).toBeUndefined();
  expect(timers.map((timer) => timer.delayMs)).toEqual([DEFAULT_STALL_LIMITS.waitingMs]);
  await fireLast(timers);

  expect(sent).toEqual([]);
  expect(asked()).toBe(3);
});

// R5: where the daemon's answer does not arrive, the worker is still nudged, and the nudge says
// it could not read the record: it names no phase and no operation, because it read neither.
// Each failure is the real client's, over a daemon that fails that way.
test("a daemon that does not answer still nudges, and the nudge names no phase and no operation", async () => {
  const failures: Record<string, typeof fetch> = {
    unreachable: (async () => {
      throw new TypeError("fetch failed");
    }) as unknown as typeof fetch,
    "a hang past the bound": (async (_input: unknown, init?: RequestInit) => {
      const aborted = Promise.withResolvers<Response>();
      init?.signal?.addEventListener("abort", () => aborted.reject(init.signal?.reason));
      return aborted.promise;
    }) as unknown as typeof fetch,
    "a refusal": (async () =>
      Response.json(
        { error: "Invalid session secret" },
        { status: 403 }
      )) as unknown as typeof fetch,
    "a failure to read the record": (async () =>
      Response.json(
        { error: "read the issue record: connection refused" },
        { status: 500 }
      )) as unknown as typeof fetch,
    "an answer the contract refuses": (async () =>
      Response.json({ owes: true, work: "" })) as unknown as typeof fetch,
  };
  for (const [failure, fetchFn] of Object.entries(failures)) {
    const client = createLegionDaemonClient("http://daemon.test", fetchFn);
    // The bound is the reminder's own, cut short so the hang is over in 20ms.
    const { reminder, timers, sent } = rig({
      owed: (input, signal) => client.owed(input, { signal }),
      owedBoundMs: 20,
    });

    const continuation = await reminder.settle("Pushed.");
    expect(continuation, failure).toBe(UNREAD_REMINDER);
    expect(continuation, failure).toContain("could not read the Legion daemon's record");

    // The timer's reminder, led by the text-call warning, says the same and names nothing more.
    const textCall = '<invoke name="legion"><parameter name="op">handoff_complete</parameter>';
    expect(await reminder.settle(textCall), failure).toBeUndefined();
    await fireLast(timers);
    expect(sent, failure).toEqual([reminderText(textCall, undefined)]);
    for (const text of [UNREAD_REMINDER, reminderText(textCall, undefined)]) {
      expect(phasesAndOperations(text), failure).toEqual([]);
    }
  }
});

// R5, the third cause of the reminder noise, proven: INFRA's daemon answered the reminder's old
// whole-state read later than its bound, and every timeout reminded. A daemon that answers the
// owed read later than the bound still has the worker nudged (`NUDGE_WHEN_OWED_UNREAD`), and the
// nudge, and the architect notice that follows it, name no phase, no operation and no owed work:
// the answer that comes after the bound, naming the work, is never rendered.
test("a read that times out produces a nudge that names no phase and produces no claim about what is owed", async () => {
  expect(NUDGE_WHEN_OWED_UNREAD).toBe(true);
  // A real daemon over HTTP that answers, saying the claim owes WORK, but only once the test
  // releases it, which it does after both reminders were decided: every answer arrives later
  // than the bound.
  const release = Promise.withResolvers<void>();
  const bothAnswered = Promise.withResolvers<void>();
  let asked = 0;
  let answered = 0;
  const daemon = Bun.serve({
    port: 0,
    fetch: async (request) => {
      expect(new URL(request.url).pathname).toBe("/legion/v1/claims/owed");
      asked += 1;
      await release.promise;
      answered += 1;
      if (answered === 2) bothAnswered.resolve();
      return Response.json(OWES);
    },
  });
  try {
    const client = createLegionDaemonClient(daemon.url.href);
    const { reminder, timers, sent, published } = rig({
      owed: (input, signal) => client.owed(input, { signal }),
      owedBoundMs: 30,
    });

    // The continuation goes, and it is the unread nudge.
    const continuation = await reminder.settle("Pushed.");
    expect(continuation).toBe(UNREAD_REMINDER);
    // The timer's reminder goes too, the second of the run, and the architect is told.
    expect(await reminder.settle("Still pushing the change.")).toBeUndefined();
    await fireLast(timers);
    expect(sent).toEqual([UNREAD_REMINDER]);
    expect(published).toHaveLength(1);
    expect(asked).toBe(2);
    expect(answered).toBe(0);

    // The daemon answers now, past the bound: the answers naming the work change nothing.
    release.resolve();
    await bothAnswered.promise;
    expect(sent).toEqual([UNREAD_REMINDER]);
    expect(published).toHaveLength(1);

    const notice = published[0];
    const said = String(notice?.message).replace(/What it last said was: .*$/, "");
    for (const text of [String(continuation), ...sent, said]) {
      expect(phasesAndOperations(text), text).toEqual([]);
      expect(text).not.toContain(WORK);
      expect(text).not.toContain("still owed");
      expect(text).not.toContain(owedReminder(WORK));
    }
    expect(said).toContain("the daemon's record of what it owes cannot be read");
    // The notice's facts carry no `work`: it makes no claim about what is owed.
    expect(JSON.parse(String(notice?.payload))).toEqual({
      issue: "LEGION-208",
      role: "implementer",
      reminders: 2,
      lastMessage: "Still pushing the change.",
    });
  } finally {
    await daemon.stop(true);
  }
});

// The notice's topic is the architect the daemon's `Legion addressing` names to the pane: the
// tree's, from the claim's tree and LEGION_PROJECT. A worker on a child issue tells its tree's
// architect, and nothing of the daemon is read for it but the owed answer.
test("the architect notice goes to the claim's tree architect, read from nothing but the claim", async () => {
  const child = {
    ...implementer,
    claimToken: "legion-legion-legion-209-implementer",
    issue: "LEGION-209",
  };
  const { reminder, timers, published, asked } = rig({ claim: child });
  expect(await reminder.settle("Pushed.")).toBeString();
  expect(await reminder.settle("Still pushing the change.")).toBeUndefined();
  await fireLast(timers);

  expect(published.map((input) => input.topic)).toEqual([
    "notifications.role.legion-legion-legion-208-architect",
  ]);
  expect(String(published[0]?.message)).toContain("The implementer of LEGION-209");
  // One owed read per reminder, and no other daemon read.
  expect(asked()).toBe(2);
});

// R5: the architect notice renders the daemon's answer where it rendered the phase it read: the
// work the daemon named when it answered, and that the record could not be read when it did not.
test("the architect notice names the work the daemon gave, and no work when it gave none", async () => {
  const named = rig({});
  expect(await named.reminder.settle("Pushed.")).toBeString();
  expect(await named.reminder.settle("Still pushing the change.")).toBeUndefined();
  await fireLast(named.timers);

  expect(named.published).toHaveLength(1);
  expect(named.published[0]?.topic).toBe("notifications.role.legion-legion-legion-208-architect");
  expect(named.published[0]?.message).toBe(
    `The implementer of LEGION-208 has let 2 reminders go unanswered: its turns keep ending with ${WORK} still owed, and nothing it does clears the reminder. What it last said was: Still pushing the change.`
  );
  expect(JSON.parse(String(named.published[0]?.payload))).toEqual({
    issue: "LEGION-208",
    role: "implementer",
    work: WORK,
    reminders: 2,
    lastMessage: "Still pushing the change.",
  });

  const unread = rig({
    owed: async () => {
      throw new Error("connect ECONNREFUSED");
    },
  });
  expect(await unread.reminder.settle("Pushed.")).toBeString();
  expect(await unread.reminder.settle("Still pushing the change.")).toBeUndefined();
  await fireLast(unread.timers);

  expect(unread.published).toHaveLength(1);
  const message = String(unread.published[0]?.message);
  expect(message).toBe(
    "The implementer of LEGION-208 has let 2 reminders go unanswered: its turns keep ending while the daemon's record of what it owes cannot be read, and nothing it does clears the reminder. What it last said was: Still pushing the change."
  );
  expect(phasesAndOperations(message.replace(/What it last said was: .*$/, ""))).toEqual([]);
  expect(JSON.parse(String(unread.published[0]?.payload))).toEqual({
    issue: "LEGION-208",
    role: "implementer",
    reminders: 2,
    lastMessage: "Still pushing the change.",
  });
});

// R5's restart row, the defect's own signature (dispatch://INFRA-2746): a restarted plugin
// reminded a closed-phase planner on four turns in a row, because the phase model it lost was its
// own. A machine made afresh — with no record, or with the record the release before this one
// left, closed or open — holds nothing a restart could reset: while the daemon answers that the
// claim owes nothing, it reports nothing, from its resume timer or from any settle.
test("a reminder machine made afresh reports nothing while the daemon answers that the claim owes nothing", async () => {
  // Each record as a host hands it over: through `phaseStallEntryFrom`, which every host's
  // restore reads its record with.
  const records = [
    undefined,
    { state: "active", count: 0, reported: false },
    // What the release before this one wrote: its states are read as the rhythm's `active`.
    { state: "closed", count: 0, reported: false },
    { state: "open", count: 3, reported: true },
  ].map((data) => ({
    label: JSON.stringify(data ?? "no record"),
    entry: phaseStallEntryFrom(data),
  }));
  // Every record but the absent one reads, so no row silently tests "no record" twice.
  expect(
    records.filter(({ entry }) => entry !== undefined).map(({ entry }) => entry?.state)
  ).toEqual(["active", "active", "active"]);
  for (const { label, entry } of records) {
    const { reminder, timers, sent, published } = rig({
      owed: async () => OWES_NOTHING,
      ...(entry === undefined ? {} : { restore: entry }),
    });

    reminder.armResumeReminder();
    await fireLast(timers);
    for (const text of ["Done; handed off.", "Nothing left.", "Idle."]) {
      expect(await reminder.settle(text), label).toBeUndefined();
      if (timers.length > 1) await fireLast(timers);
    }

    expect(sent, label).toEqual([]);
    expect(published, label).toEqual([]);

    // And the same fresh machine, on the same record, is reminded the moment the daemon answers
    // that work is owed: a restart changes nothing about whether a role is reminded, in either
    // direction, and what decides is the answer.
    const owing = rig(entry === undefined ? {} : { restore: entry });
    owing.reminder.armResumeReminder();
    await fireLast(owing.timers);
    expect(owing.sent, label).toEqual([reminderText("", WORK)]);
  }
});

// R5: the daemon's own update task arrives as a prompt, which the reminder reads as an inbound
// message. It moves the rhythm's count and nothing else, so a role whose phase has closed is
// reminded of nothing after it. There is no guard for this: with no model of the phase, nothing
// an inbound message moves can decide that a reminder goes out.
test("an update task sent to a role whose phase is closed produces no reminder", async () => {
  const { reminder, timers, sent } = rig({ owed: async () => OWES_NOTHING });

  // The role finished its phase: its last tool call was the completion, and its turn settled.
  reminder.apply({ kind: "tool-call" });
  expect(await reminder.settle("Handed off.")).toBeUndefined();

  // The daemon's update task, and the turn it starts.
  reminder.turnStarted();
  reminder.apply({ kind: "inbound" });
  expect(await reminder.settle("Nothing of mine to push.")).toBeUndefined();
  expect(await reminder.settle("Still nothing.")).toBeUndefined();
  for (const timer of timers) await timer.callback();

  expect(sent).toEqual([]);
});

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
  expect(await reminder.settle("Pushed.")).toBeString();
  expect(await reminder.settle("Still here.")).toBeUndefined();
  const delivered = fireLast(timers);
  await reached.promise;
  const firstKey = published[0]?.idempotencyKey;
  expect(firstKey).toMatch(/^[0-9a-f-]{36}$/);

  // The worker answers in the reminder's own turn, and its next settle opens a new run.
  reminder.apply({ kind: "tool-call" });
  expect(await reminder.settle("Working on it.")).toBeString();

  listener.resolve({});
  await delivered;
  expect(entries.at(-1)).toEqual({
    state: "active",
    count: 1,
    reported: false,
    lastMessage: "Working on it.",
  });

  // The new run reaches the threshold and tells the architect under a key of its own.
  expect(await reminder.settle("Still here.")).toBeUndefined();
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

  expect(await reminder.settle("Pushed.")).toBeString();
  expect(await reminder.settle("Still here.")).toBeUndefined();
  await fireLast(timers);
  expect(published).toHaveLength(1);
  expect(warned).toEqual(["legion: the architect was not told its worker is stalled"]);
  const failed = entries.at(-1);
  expect(failed).toMatchObject({ count: 2, reported: false });
  expect(failed?.reportKey).toBe(published[0]?.idempotencyKey);

  expect(await reminder.settle("Still here.")).toBeUndefined();
  await fireLast(timers);
  expect(published).toHaveLength(2);
  expect(published[1]?.idempotencyKey).toBe(failed?.reportKey);
  expect(entries.at(-1)).toMatchObject({ count: 3, reported: true, reportKey: failed?.reportKey });

  // Accepted once is told once.
  expect(await reminder.settle("Still here.")).toBeUndefined();
  await fireLast(timers);
  expect(published).toHaveLength(2);
});

// The notice asks the listener to keep nothing, because this sender keeps its own retry: the
// reminding never stops while the work is owed. A listener that keeps it all the same answers
// 202 with a kept body, and that is not the architect being told — the kept copy is dropped once
// its window passes, and nothing would publish the notice again. So the run's next delivered
// reminder publishes it again, under the same key.
test("a kept answer is not the architect being told, so the run's next reminder publishes again", async () => {
  const requests: unknown[] = [];
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
        }),
        { status: 202, headers: { "Content-Type": "application/json" } }
      );
    },
  });
  const { reminder, timers, entries, published, warned } = rig({
    publish: (input) => client.publish(input),
  });

  expect(await reminder.settle("Pushed.")).toBeString();
  expect(await reminder.settle("Still here.")).toBeUndefined();
  await fireLast(timers);

  expect(published).toHaveLength(1);
  // The notice asks for `keep: false`, and the flag reaches the listener.
  expect(published[0]?.keep).toBe(false);
  expect(requests[0]).toMatchObject({ keep: false });
  // Nobody was told, and that is not this worker's doing: no warning, and the notice stands.
  expect(warned).toEqual([]);
  const kept = entries.at(-1);
  expect(kept).toMatchObject({ count: 2, reported: false });

  expect(await reminder.settle("Still here.")).toBeUndefined();
  await fireLast(timers);
  expect(published).toHaveLength(2);
  expect(published[1]?.idempotencyKey).toBe(kept?.reportKey);
  expect(entries.at(-1)).toMatchObject({ count: 3, reported: false });
});

// With `keep: false` the listener keeps nothing for a role no live session holds: it answers the
// 404 it always did, and that is the answer this sender's retry exists for. Nobody was told, so
// the next delivered reminder publishes again under the same key, and the worker's transcript
// gets no warning for something that is not its doing.
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

  expect(await reminder.settle("Pushed.")).toBeString();
  expect(await reminder.settle("Still here.")).toBeUndefined();
  await fireLast(timers);
  expect(published).toHaveLength(1);
  expect(published[0]?.keep).toBe(false);
  expect(warned).toEqual([]);
  const refused = entries.at(-1);
  expect(refused).toMatchObject({ count: 2, reported: false });

  expect(await reminder.settle("Still here.")).toBeUndefined();
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

  expect(await reminder.settle("Pushed.")).toBeString();
  expect(await reminder.settle("Still here.")).toBeUndefined();
  expect(timers).toHaveLength(1);
  await fireLast(timers);

  expect(warned).toEqual([
    "legion: a phase reminder did not reach this worker; it comes again later",
  ]);
  // Still at the continuation's count: the failed one counted for nothing, so it told no one.
  expect(entries.at(-1)).toMatchObject({ state: "active", count: 1 });
  expect(published).toEqual([]);
  expect(timers).toHaveLength(2);
  expect(timers[1]?.delayMs).toBe(DEFAULT_STALL_LIMITS.reminderMs);

  await fireLast(timers);
  expect(sent).toHaveLength(2);
  expect(sent[1]).toBe(sent[0]);
  expect(entries.at(-1)).toMatchObject({ state: "active", count: 2 });
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

  expect(await reminder.settle("Pushed.")).toBeString();
  expect(await reminder.settle("Still here.")).toBeUndefined();
  const delivered = fireLast(timers);
  await writing.promise;
  reminder.turnStarted();
  write.reject(new Error("the bridge closed the socket"));
  await delivered;

  expect(timers).toHaveLength(1);
});

// The wait a `ready_for_update` answer creates is `awaiting-update`, and nothing is armed in it:
// the daemon is what wakes the worker, with the task it held behind the round, once the round
// has replaced the pod. A timer fired in that wait would steer a real turn into the worker
// instead, and a new turn is exactly what the daemon reads as the answer retracted
// (`takeBackSignal`), so the sweep would ask the worker to push again, the round would
// restart at every `waitingMs`, and the late-round notice — measured from the latest answer —
// would never name the `ReplacePod` error or the sibling turn that will not end. The WAITING
// reply's own wait is untouched: it is the reminder's to time out.
test("the wait an update answer creates arms no reminder, where a WAITING reply still arms one", async () => {
  const { reminder, timers, sent, entries, asked } = rig({});

  reminder.apply({ kind: "update-answered" });
  expect(await reminder.settle("Pushed and answered.")).toBeUndefined();

  expect(timers).toEqual([]);
  expect(sent).toEqual([]);
  // Nothing is asked of the daemon in this wait either: no reminder is due.
  expect(asked()).toBe(0);
  // The record holds the wait as the plain `waiting` it is a kind of: nothing restores a record
  // without a relaunch, and that relaunch is this round over (`recordedPhaseStall`).
  expect(entries.at(-1)).toMatchObject({ state: "waiting", count: 0 });

  // A second turn in the same wait — a question answered while the pod waits to be replaced —
  // arms none either.
  expect(await reminder.settle("Still waiting for the pod.")).toBeUndefined();
  expect(timers).toEqual([]);

  // The daemon wakes the worker with the task it held behind the round: the rhythm is active
  // again, and a WAITING reply buys the quiet wait the reminder itself ends.
  reminder.apply({ kind: "inbound" });
  expect(await reminder.settle("WAITING on the reviewer.")).toBeUndefined();

  expect(timers).toHaveLength(1);
  expect(timers[0]?.delayMs).toBe(DEFAULT_STALL_LIMITS.waitingMs);
});
