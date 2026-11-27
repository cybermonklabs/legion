<<<<<<< 7fa22de66d
import { afterEach, expect, test } from "bun:test";
=======
import { expect, test } from "bun:test";
import { EnvelopeSchema } from "@legion/contracts";
>>>>>>> 67bcd7c78393eb8d85c10bbae0ceb998d1b01253
import { LEGION_PHASES, type LegionOwed } from "@legion/contracts/legion-api";
import {
  createEnvoyClient,
  EnvoyApiError,
  type PublishInput,
  type PublishResult,
} from "@legion/envoy-client/transport";
import { UPDATE_ASK_KEY, type WaitStandIn, waitStandIn } from "../test/wait-stand-in";
import {
  createLegionDaemonClient,
  type LegionDaemonClient,
  type OwedInput,
  type WaitNewsInput,
} from "./daemon-client";
import { type ClaimCapability, executePhaseOp, PHASE_OPS } from "./legion-phase-ops";
import {
  createPhaseReminder,
  type EnvoyPublish,
  type PhaseReminder,
  type PhaseReminderDeps,
} from "./phase-reminder";
import {
  owedReminder,
  type PhaseStallEntry,
  phaseStallEntryFrom,
  reminderText,
  UNREAD_REMINDER,
} from "./phase-stall";
import { DEFAULT_STALL_LIMITS } from "./stall-limits";

/** A contract the test's client states on every request. */
const CONTRACT = 60;

const implementer = {
  sessionID: "ses_implementer",
  tree: "LEGION-208",
  issue: "LEGION-208",
  role: "implementer",
  secret: "secret",
  claimToken: "legion-legion-legion-208-implementer",
  generation: 1,
} as const;

/** What the daemon calls the work it answers is owed (`LegionOwed.work`). */
const WORK = "the implementing phase of LEGION-208";
const OWES: LegionOwed = { owes: true, work: WORK };
const OWES_NOTHING: LegionOwed = { owes: false, work: "" };

/** A turn the worker ended by saying `text`, as `turnEndOf` classifies it on Oh My Pi. */
const said = (text: string) => ({ kind: "settle", lastAssistantText: text }) as const;

/** The phase names and operation names a sentence carries, as whole words. A sentence the
 * reminder prints without the daemon's answer carries none but `declare_wait`, which a worker
 * that waits needs whatever the record says: it read neither the phase nor what ends it. */
function phasesAndOperations(text: string): string[] {
  const words = new Set(text.split(/[^A-Za-z_]+/));
  return [...LEGION_PHASES, ...PHASE_OPS, "phase"].filter((word) => words.has(word));
}

/** Every report of news a case that reports none was asked to send. The reminder catches a
 * report's failure and only logs it, so a throw here would fail nothing: the hook below holds
 * the list empty at the end of each case. */
const unexpectedReports: WaitNewsInput[] = [];

/** The news report of a case that reports none. */
const NO_NEWS: PhaseReminderDeps["reportNews"] = async (input) => {
  unexpectedReports.push(input);
  throw new Error("this case reports no news");
};

afterEach(() => {
  expect(unexpectedReports.splice(0), "the reports of news a case without news sent").toEqual([]);
});

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
<<<<<<< 7fa22de66d
    reportNews: NO_NEWS,
    publisher: () => undefined,
=======
    publisher: async () => undefined,
>>>>>>> 67bcd7c78393eb8d85c10bbae0ceb998d1b01253
    project: "LEGION",
    limits: DEFAULT_STALL_LIMITS,
    log: { debug: (message) => debugged.push(message), warn: () => {} },
    owedBoundMs: 20,
  });

  const continuation = await reminder.settle(said("Pushed."));

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
  expect((handed[0]?.reason as Error | undefined)?.message).toBe("no answer within 20ms");
  expect(debugged).toEqual(["legion: the daemon did not say what this claim owes"]);
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
    pluginContract: CONTRACT,
    fetch: recording,
  });
  const reminder = createPhaseReminder({
    sendReminder: () => {},
    armTimer: () => {},
    saveEntry: () => {},
    claim: () => implementer,
    readOwed: (input, signal) => daemon.owed(input, { signal }),
<<<<<<< 7fa22de66d
    reportNews: (input, signal) => daemon.waitNews(input, { signal }),
    publisher: () => undefined,
=======
    publisher: async () => undefined,
>>>>>>> 67bcd7c78393eb8d85c10bbae0ceb998d1b01253
    project: "LEGION",
    limits: DEFAULT_STALL_LIMITS,
    log: { debug: (message) => debugged.push(message), warn: () => {} },
  });
  return { reminder, routes, debugged };
}

// The check keeps its two branches apart: the daemon's answer names the work the reminder gives,
// and everything else is a read that failed, which nudges and names none. It asks the owed route
// alone and never reads the whole project.
test("the owed check asks one route: the answer names the work, a transport failure is the read that failed", async () => {
  const answered = overDaemon((async (_input, _init) => Response.json(OWES)) as typeof fetch);

  expect(await answered.reminder.settle(said("Pushed."))).toBe(reminderText("Pushed.", WORK));
  expect(answered.routes).toEqual(["/legion/v1/claims/owed"]);
  expect(answered.debugged).toEqual([]);

  const failed = overDaemon((async (_input, _init): Promise<Response> => {
    throw new TypeError("fetch failed");
  }) as typeof fetch);

  expect(await failed.reminder.settle(said("Pushed."))).toBe(UNREAD_REMINDER);
  expect(failed.routes).toEqual(["/legion/v1/claims/owed"]);
  expect(failed.debugged).toEqual(["legion: the daemon did not say what this claim owes"]);
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
 * owes `WORK`, naming no architect as its owner, so a notice goes to the tree's architect. */
function rig(options: {
  readonly sendReminder?: (reminder: string) => void | Promise<void>;
  readonly publish?: EnvoyPublish;
  readonly owed?: (input: OwedInput, signal: AbortSignal) => Promise<LegionOwed>;
  readonly restore?: PhaseStallEntry;
  /** The bound on the owed check, when a case's daemon hangs past it. */
  readonly owedBoundMs?: number;
  /** The publisher's build, which settles before the publisher is handed over, as a first build
   *  that runs Envoy's token command does; the publisher is at hand at once otherwise. */
  readonly buildPublisher?: () => Promise<void>;
  /** The session's claim: the implementer of LEGION-208, a tree root, throughout unless a case
   *  names one. */
  readonly claim?: () => ClaimCapability | undefined;
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
    claim: options.claim ?? (() => implementer),
    readOwed: (input, signal) => {
      asked += 1;
      return options.owed === undefined ? Promise.resolve(OWES) : options.owed(input, signal);
    },
<<<<<<< 7fa22de66d
    reportNews: NO_NEWS,
    publisher: () => async (input) => {
      published.push(input);
      return options.publish === undefined ? {} : options.publish(input);
=======
    publisher: async () => {
      await options.buildPublisher?.();
      return async (input) => {
        published.push(input);
        return options.publish === undefined ? {} : options.publish(input);
      };
>>>>>>> 67bcd7c78393eb8d85c10bbae0ceb998d1b01253
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

// The daemon's answer decides, each way, at the continuation and at every timer after it.
test("a claim the daemon says owes work is reminded of the work the daemon named", async () => {
  const { reminder, timers, sent } = rig({});

  const continuation = await reminder.settle(said("Pushed."));
  expect(continuation).toBe(reminderText("Pushed.", WORK));
  expect(continuation).toContain(`Your turn ended with ${WORK} still owed`);

  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  await fireLast(timers);
  expect(sent).toEqual([reminderText("Still here.", WORK)]);
});

test("a claim the daemon says owes nothing is reminded of nothing, at the continuation or a timer", async () => {
  const { reminder, timers, sent, asked } = rig({ owed: async () => OWES_NOTHING });

  expect(await reminder.settle(said("Pushed."))).toBeUndefined();
  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  // The quiet wait the worker's own background work buys is still armed — the rhythm is the
  // reminder's own — and the daemon is asked again when it is due, and answers the same. Every
  // turn's end asks the daemon before it arms anything.
  expect(
    await reminder.settle({
      kind: "settle",
      lastAssistantText: "Started the build.",
      backgroundWork: true,
    })
  ).toBeUndefined();
  expect(timers.map((timer) => timer.delayMs)).toEqual([DEFAULT_STALL_LIMITS.waitingMs]);
  await fireLast(timers);

  expect(sent).toEqual([]);
  expect(asked()).toBe(4);
});

// Where the daemon's answer does not arrive, the worker is still nudged, and the nudge says
// it could not read the record: it names no phase and no operation that ends one, because it read
// neither, and the one operation it names is `declare_wait`. Each failure is the real client's,
// over a daemon that fails that way.
test("a daemon that does not answer still nudges, and the nudge names no phase and no operation but declare_wait", async () => {
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
    const client = createLegionDaemonClient("http://daemon.test", {
      pluginContract: CONTRACT,
      fetch: fetchFn,
    });
    // The bound is the reminder's own, cut short so the hang is over in 20ms.
    const { reminder, timers, sent } = rig({
      owed: (input, signal) => client.owed(input, { signal }),
      owedBoundMs: 20,
    });

    const continuation = await reminder.settle(said("Pushed."));
    expect(continuation, failure).toBe(UNREAD_REMINDER);
    expect(continuation, failure).toContain("could not read the Legion daemon's record");

    // The timer's reminder, led by the text-call warning, says the same and names nothing more.
    const textCall = '<invoke name="legion"><parameter name="op">handoff_complete</parameter>';
    expect(await reminder.settle(said(textCall)), failure).toBeUndefined();
    await fireLast(timers);
    expect(sent, failure).toEqual([reminderText(textCall, undefined)]);
    for (const text of [UNREAD_REMINDER, reminderText(textCall, undefined)]) {
      expect(phasesAndOperations(text), failure).toEqual(["declare_wait"]);
    }
  }
});

// The other side of the rows above, over the same real client: only the daemon's success that the
// claim owes nothing silences the reminder. A failure, the 500 of a record the daemon could not
// read among them, is no such answer and nudges.
test("a daemon that answers 200 that the claim owes nothing is reminded of nothing", async () => {
  const client = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: (async () => Response.json(OWES_NOTHING)) as unknown as typeof fetch,
  });
  const { reminder, timers, sent, asked } = rig({
    owed: (input, signal) => client.owed(input, { signal }),
  });

  expect(await reminder.settle(said("Pushed."))).toBeUndefined();
  // The quiet wait the worker's own background work buys is armed, and its timer asks the daemon
  // again.
  expect(
    await reminder.settle({
      kind: "settle",
      lastAssistantText: "Started the build.",
      backgroundWork: true,
    })
  ).toBeUndefined();
  await fireLast(timers);
  expect(sent).toEqual([]);
  expect(asked()).toBe(3);
});

// A daemon that answers the owed read later than the bound still has the worker nudged
// (`UNREAD_CHECK`), and the nudge, and the architect notice that follows it, name no phase, no
// operation that ends one and no owed work: the answer that comes after the bound, naming the
// work, is never rendered.
test("a read that times out produces a nudge that names no phase and produces no claim about what is owed", async () => {
  // A real daemon over HTTP that answers, saying the claim owes WORK, but only once the test
  // releases it, which it does after both reminders were decided: every answer arrives later
  // than the bound. The reads are counted where the reminder makes them, since a read the bound
  // aborts on a loaded machine can end before the server sees its request.
  const release = Promise.withResolvers<void>();
  // Each request the server took, answered once the test releases it.
  const taken: Promise<void>[] = [];
  let asked = 0;
  let answered = 0;
  const daemon = Bun.serve({
    port: 0,
    fetch: (request) => {
      expect(new URL(request.url).pathname).toBe("/legion/v1/claims/owed");
      const answer = release.promise.then(() => {
        answered += 1;
        return Response.json(OWES);
      });
      taken.push(answer.then(() => undefined));
      return answer;
    },
  });
  try {
    const client = createLegionDaemonClient(daemon.url.href, { pluginContract: CONTRACT });
    const { reminder, timers, sent, published } = rig({
      owed: (input, signal) => {
        asked += 1;
        return client.owed(input, { signal });
      },
      owedBoundMs: 30,
    });

    // The continuation goes, and it is the unread nudge.
    const continuation = await reminder.settle(said("Pushed."));
    expect(continuation).toBe(UNREAD_REMINDER);
    // The timer's reminder goes too, the second of the run, and the architect is told. The turn's
    // end that armed it asked the daemon too, before it armed anything.
    expect(await reminder.settle(said("Still pushing the change."))).toBeUndefined();
    await fireLast(timers);
    expect(sent).toEqual([UNREAD_REMINDER]);
    expect(published).toHaveLength(1);
    expect(asked).toBe(3);
    expect(answered).toBe(0);

    // The daemon answers now, past the bound: an answer naming the work changes nothing. A read
    // the bound aborted before its request reached the server is never answered, so this waits
    // for the requests the server took.
    release.resolve();
    await Promise.all(taken);
    expect(sent).toEqual([UNREAD_REMINDER]);
    expect(published).toHaveLength(1);

    const notice = published[0];
    const notified = String(notice?.message).replace(/What it last said was: .*$/, "");
    for (const text of [String(continuation), ...sent, notified]) {
      // The nudge names `declare_wait` alone, and the notice names no operation at all.
      expect(phasesAndOperations(text), text).toEqual(text === notified ? [] : ["declare_wait"]);
      expect(text).not.toContain(WORK);
      expect(text).not.toContain("still owed");
      expect(text).not.toContain(owedReminder(WORK));
    }
    expect(notified).toContain("the daemon's record of what it owes cannot be read");
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

/** The implementer of LEGION-209, a child of the tree LEGION-208. */
const child = {
  ...implementer,
  claimToken: "legion-legion-legion-209-implementer",
  issue: "LEGION-209",
};

/** The child's own architect claim, and the tree's, as the daemon names them. */
const CHILD_ARCHITECT = "legion-legion-legion-209-architect";
const TREE_ARCHITECT = "legion-legion-legion-208-architect";

/** What the daemon answers the child's implementer owes, naming its owner when one is given. */
const childOwes = (architect?: string): LegionOwed => ({
  owes: true,
  work: "the implementing phase of LEGION-209",
  ...(architect === undefined ? {} : { architect }),
});

// The notice goes to the architect the daemon names as the claim's owner in its owed answer: a
// child whose own architect claim is live owns its workers, so the daemon names that architect,
// and the worker tells it. Nothing else of the daemon is read for it.
test("a child whose own architect claim is live has its worker's notice go to that architect", async () => {
  const { reminder, timers, published, asked } = rig({
    claim: () => child,
    owed: async () => childOwes(CHILD_ARCHITECT),
  });
  expect(await reminder.settle(said("Pushed."))).toBeString();
  expect(await reminder.settle(said("Still pushing the change."))).toBeUndefined();
  await fireLast(timers);

  expect(published.map((input) => input.topic)).toEqual([`notifications.role.${CHILD_ARCHITECT}`]);
  expect(String(published[0]?.message)).toContain("The implementer of LEGION-209");
  // One owed read per turn's end and per reminder, and no other daemon read.
  expect(asked()).toBe(3);
});

// A child whose own architect is retired or failed is owned by the architect above it, which the
// daemon names in its stead: the notice goes to the architect named. An answer that names no
// architect, and a read that fails at every reminder, name nobody, and the notice goes to the
// tree's architect, named from the claim's tree and LEGION_PROJECT. An answer that named the owner
// is kept, so a notice whose own reminder could not read the daemon still reaches it.
test("a child whose own architect is not live has its worker's notice go to the architect the daemon names, else the tree's", async () => {
  const unreachable = async (): Promise<LegionOwed> => {
    throw new Error("connect ECONNREFUSED");
  };
  let reads = 0;
  const cases: readonly {
    readonly label: string;
    readonly owed: () => Promise<LegionOwed>;
    readonly topic: string;
  }[] = [
    {
      label: "the daemon names the tree's architect, the child's own retired",
      owed: async () => childOwes(TREE_ARCHITECT),
      topic: TREE_ARCHITECT,
    },
    {
      label: "an answer that names no architect",
      owed: async () => childOwes(),
      topic: TREE_ARCHITECT,
    },
    { label: "a read that fails at every reminder", owed: unreachable, topic: TREE_ARCHITECT },
    {
      label: "the continuation's answer named the child's architect, the notice's read failed",
      owed: () => {
        reads += 1;
        return reads === 1 ? Promise.resolve(childOwes(CHILD_ARCHITECT)) : unreachable();
      },
      topic: CHILD_ARCHITECT,
    },
  ];
  for (const { label, owed, topic } of cases) {
    const { reminder, timers, published } = rig({ claim: () => child, owed });
    expect(await reminder.settle(said("Pushed.")), label).toBeString();
    expect(await reminder.settle(said("Still pushing the change.")), label).toBeUndefined();
    await fireLast(timers);

    expect(
      published.map((input) => input.topic),
      label
    ).toEqual([`notifications.role.${topic}`]);
  }
});

// The architect notice renders the daemon's answer: the work the daemon named when it answered,
// and that the record could not be read when it did not.
test("the architect notice names the work the daemon gave, and no work when it gave none", async () => {
  const named = rig({});
  expect(await named.reminder.settle(said("Pushed."))).toBeString();
  expect(await named.reminder.settle(said("Still pushing the change."))).toBeUndefined();
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
  expect(await unread.reminder.settle(said("Pushed."))).toBeString();
  expect(await unread.reminder.settle(said("Still pushing the change."))).toBeUndefined();
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

// A restarted plugin reminds no role the daemon says owes nothing (dispatch://INFRA-2746). A
// machine made afresh — with no record, or with a record an earlier release wrote, closed or
// open — holds nothing a restart could reset: while the daemon answers that the claim owes
// nothing, it reports nothing, from its resume timer or from any settle.
test("a reminder machine made afresh reports nothing while the daemon answers that the claim owes nothing", async () => {
  // Each record as a host hands it over: through `phaseStallEntryFrom`, which every host's
  // restore reads its record with.
  const records = [
    undefined,
    { state: "active", count: 0, reported: false },
    // The states an earlier release wrote are read as the rhythm's `active`.
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

    await reminder.armResumeReminder();
    await fireLast(timers);
    for (const text of ["Done; handed off.", "Nothing left.", "Idle."]) {
      expect(await reminder.settle(said(text)), label).toBeUndefined();
      if (timers.length > 1) await fireLast(timers);
    }

    expect(sent, label).toEqual([]);
    expect(published, label).toEqual([]);

    // And the same fresh machine, on the same record, is reminded the moment the daemon answers
    // that work is owed: a restart changes nothing about whether a role is reminded, in either
    // direction, and what decides is the answer.
    const owing = rig(entry === undefined ? {} : { restore: entry });
    await owing.reminder.armResumeReminder();
    await fireLast(owing.timers);
    expect(owing.sent, label).toEqual([reminderText("", WORK)]);
  }
});

// The daemon's own update message arrives over Envoy, which the reminder reads as news. It moves
// the rhythm's count and nothing else, so a role whose phase has closed is reminded of nothing
// after it; with no wait standing, it reports nothing either. There is no guard for this: with no
// model of the phase, nothing news moves can decide that a reminder goes out.
test("an update message sent to a role whose phase is closed produces no reminder", async () => {
  const { reminder, timers, sent } = rig({ owed: async () => OWES_NOTHING });

  // The role finished its phase: its last tool call was the completion, and its turn settled.
  reminder.apply({ kind: "tool-call" });
  expect(await reminder.settle(said("Handed off."))).toBeUndefined();

  // The daemon's update message, and the turn it starts.
  reminder.turnStarted();
  await reminder.news(UPDATE_ASK_KEY);
  expect(await reminder.settle(said("Nothing of mine to push."))).toBeUndefined();
  expect(await reminder.settle(said("Still nothing."))).toBeUndefined();
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
    state: "active",
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
// worker that "has let 0 reminders go unanswered". The next run is told as any run is.
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
  expect(published[0]?.message).toContain("has let 2 reminders go unanswered");
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
  expect(sent).toEqual([reminderText("", WORK)]);
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
// (dispatch://INFRA-4508). The timer the refused turn armed still fires, and the daemon answers
// its owed check that the failed claim owes nothing, so nothing is written into a session nobody
// can act on, and the reminding ends. The reminder holds no model of the claim's failure: the
// daemon's answer is the whole of it.
test("a refused turn whose claim the daemon failed reminds nothing", async () => {
  const { reminder, timers, sent, asked } = rig({ owed: async () => OWES_NOTHING });

  expect(
    await reminder.settle({ kind: "refused", message: "Refusal (reasoning_extraction)" })
  ).toBeUndefined();
  // The refused settle asks nothing: it earns no reminder of its own, only the timer.
  expect(asked()).toBe(0);

  expect(timers).toHaveLength(1);
  await fireLast(timers);

  expect(asked()).toBe(1);
  expect(sent).toEqual([]);
});

// The wait a `ready_for_update` answer creates is `awaiting-update`, and nothing is armed in it:
// the daemon is what wakes the worker, with the task it held behind the round, once the round
// has replaced the pod. A timer fired in that wait would steer a real turn into the worker
// instead: its work would go with the pod, and before the round's bound a running turn holds the
// replacement back, so a reminder every `waitingMs` would keep the round from ending early. The
// quiet wait the worker's own background work buys is untouched: it is the reminder's to time
// out.
test("the wait an update answer creates arms no reminder, where background work still arms one", async () => {
  const { reminder, timers, sent, entries, asked } = rig({});

  reminder.apply({ kind: "update-answered" });
  expect(await reminder.settle(said("Pushed and answered."))).toBeUndefined();

  expect(timers).toEqual([]);
  expect(sent).toEqual([]);
  // Nothing is asked of the daemon in this wait either: no reminder is due.
  expect(asked()).toBe(0);
  // The record holds the wait as the plain `waiting` it is a kind of: nothing restores a record
  // without a relaunch, and that relaunch is this round over (`recordedPhaseStall`).
  expect(entries.at(-1)).toMatchObject({ state: "waiting", count: 0 });

  // A second turn in the same wait — a question answered while the pod waits to be replaced —
  // arms none either.
  expect(await reminder.settle(said("Still waiting for the pod."))).toBeUndefined();
  expect(timers).toEqual([]);

  // The daemon wakes the worker with the task it held behind the round: the rhythm is active
  // again, and background work buys the quiet wait the reminder itself ends.
  reminder.apply({ kind: "prompt" });
  expect(
    await reminder.settle({
      kind: "settle",
      lastAssistantText: "Started the build.",
      backgroundWork: true,
    })
  ).toBeUndefined();

  expect(timers).toHaveLength(1);
  expect(timers[0]?.delayMs).toBe(DEFAULT_STALL_LIMITS.waitingMs);
});

// dispatch://INFRA-2040: a WAITING line buys nothing. A worker that waits on something outside
// its own work declares the wait (`declare_wait`); a turn that only says so is reminded as any
// other turn is, at the continuation and from the ordinary timer after it.
test("a turn that ends with a WAITING line and nothing else gets the same reminder as a turn without it", async () => {
  const waiting = rig({});
  const plain = rig({});
  const line = "WAITING: CI on the pull request";

  const continuation = await waiting.reminder.settle(said(line));
  expect(continuation).toBeString();
  expect(continuation).toBe(await plain.reminder.settle(said("Pushed.")));

  expect(await waiting.reminder.settle(said(line))).toBeUndefined();
  expect(await plain.reminder.settle(said("Pushed."))).toBeUndefined();
  expect(waiting.timers.map((timer) => timer.delayMs)).toEqual([DEFAULT_STALL_LIMITS.reminderMs]);
  expect(waiting.timers.map((timer) => timer.delayMs)).toEqual(
    plain.timers.map((timer) => timer.delayMs)
  );
  await fireLast(waiting.timers);
  await fireLast(plain.timers);
  expect(waiting.sent).toEqual(plain.sent);
  expect(waiting.sent).toEqual([owedReminder(WORK)]);
});

// ─────────────────────────────────────────────────────────────────────────────
// A declared wait and the design gate's hold (dispatch://INFRA-2040), over a stand-in daemon and
// the real daemon client: the owed answer gives the wait while it stands and the hold while the
// gate is closed, the declaration goes through the `legion` tool's own executor, and the news
// route ends the wait a report names as the daemon does.
// ─────────────────────────────────────────────────────────────────────────────

/** Another delivery's key, which is news the daemon ends the wait for. */
const DELIVERY_KEY = "publish.6f2a9c1e4b7d40a8b3e5c9d1f0a2b4c6";

/** The merger of LEGION-208, a tree root, and what the daemon calls its work. */
const MERGER = {
  ...implementer,
  sessionID: "ses_merger",
  role: "merger",
  claimToken: "legion-legion-legion-208-merger",
} as const;
const MERGER_WORK = "the merging phase of LEGION-208";

/** The closed gate the owed answer gives a held merger. */
const HOLD = {
  step: "spec",
  artifactId: "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093",
  version: 7,
} as const;

/** A tool call written as text, in the shape the Stage 3 model emitted. */
const TEXT_CALL = '<invoke name="legion"><parameter name="op">handoff_complete</parameter>';

/** The build a host hands the `legion` tool's executor. */
const BUILD = { name: "@sjawhar/pi-legion", version: "18.14.1", pluginContract: CONTRACT } as const;

/** A stand-in daemon (`waitStandIn`) behind the real client, which fails the next
 * `state.failReports` reports of news with a 503 and 404s every route it does not serve. */
interface StandIn extends WaitStandIn {
  readonly client: LegionDaemonClient;
}

function standInDaemon(work: string = WORK): StandIn {
  const standIn = waitStandIn(work);
  const fetchFn = (async (input, init) => {
    const route = new URL(input.toString()).pathname;
    const body: unknown = typeof init?.body === "string" ? JSON.parse(init.body) : undefined;
    if (route === "/legion/v1/claims/owed") return standIn.owed();
    return (
      standIn.answer(route, body) ?? Response.json({ error: `no route ${route}` }, { status: 404 })
    );
  }) as typeof fetch;
  const client = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: fetchFn,
  });
  return { ...standIn, client };
}

/** The body of a report of news the claim sends for `waitId`, with `deliveryKey` when given. */
const reportOf = (claim: ClaimCapability, waitId: string, deliveryKey?: string) => ({
  claimToken: claim.claimToken,
  sessionId: claim.sessionID,
  secret: claim.secret,
  waitId,
  ...(deliveryKey === undefined ? {} : { deliveryKey }),
});

/** A worker's reminder over `standIn`, with the production limits (the architect told at the
 * third delivered reminder), as a process the daemon has just started makes it, restored from
 * `restore` when given. `declareWait` runs the `legion` tool's `declare_wait` through its own
 * executor, which tells this reminder the wait. */
function waitRig(
  standIn: StandIn,
  options: {
    readonly claim?: ClaimCapability;
    readonly restore?: PhaseStallEntry | undefined;
  } = {}
) {
  const claim = options.claim ?? implementer;
  const timers: ArmedTimer[] = [];
  const sent: string[] = [];
  const entries: PhaseStallEntry[] = [];
  const published: PublishInput[] = [];
  const reminder = createPhaseReminder({
    sendReminder: (text) => {
      sent.push(text);
    },
    armTimer: (callback, delayMs) => {
      timers.push({ callback, delayMs });
    },
    saveEntry: (entry) => entries.push(entry),
    claim: () => claim,
    readOwed: (input, signal) => standIn.client.owed(input, { signal }),
    reportNews: (input, signal) => standIn.client.waitNews(input, { signal }),
    publisher: () => async (input) => {
      published.push(input);
      return {};
    },
    project: "legion",
    limits: DEFAULT_STALL_LIMITS,
    log: { debug: () => {}, warn: () => {} },
  });
  if (options.restore !== undefined) reminder.restore(options.restore);
  const declareWait = () =>
    executePhaseOp(
      {
        daemon: standIn.client,
        claim,
        build: BUILD,
        handoffReport: async () => {
          throw new Error("no handoff report expected");
        },
        branchHead: async () => {
          throw new Error("no branch-head lookup expected");
        },
        onWaitDeclared: reminder.waitDeclared,
      },
      { op: "declare_wait", waiting_on: "CI on the pull request", wake_at: "2026-11-21T10:00:00Z" }
    );
  return { reminder, timers, sent, entries, published, declareWait };
}

/** A turn that ends with the worker's own background work still running. */
const backgroundWork = {
  kind: "settle",
  lastAssistantText: "Started the build.",
  backgroundWork: true,
} as const;

// R1 and R2: a declared wait that stands holds every reminder back. A turn's end, the quiet wait of
// background work among them, continues nothing, arms nothing and reports no reminder due, and the
// resume reminder of a process relaunched while the wait stands — from its record, or with none —
// arms nothing either. A relaunch with no record learns the wait from the daemon's answer, and
// news that reaches it then is reported against that wait.
test("a standing wait sends no reminder, arms no timer, reports no reminder due, and the resume reminder arms nothing", async () => {
  const standIn = standInDaemon();
  const worker = waitRig(standIn);
  worker.reminder.turnStarted();
  await expect(worker.declareWait()).resolves.toMatchObject({ wait: { id: "wait-1" } });

  for (const turn of [said("Declared the wait on CI."), said("Still waiting."), backgroundWork]) {
    expect(await worker.reminder.settle(turn)).toBeUndefined();
  }
  expect(worker.timers).toEqual([]);
  expect(worker.sent).toEqual([]);
  expect(worker.reminder.phaseWake()).toEqual({ nextReminderAt: null });
  const record = worker.entries.at(-1);
  expect(record).toMatchObject({ waitId: "wait-1" });

  const fromRecord = waitRig(standIn, { restore: record });
  await fromRecord.reminder.armResumeReminder();
  expect(fromRecord.timers).toEqual([]);
  expect(fromRecord.reminder.phaseWake()).toEqual({ nextReminderAt: null });

  const fresh = waitRig(standIn);
  await fresh.reminder.armResumeReminder();
  expect(fresh.timers).toEqual([]);
  expect(fresh.reminder.phaseWake()).toEqual({ nextReminderAt: null });
  expect(fresh.entries.at(-1)).toMatchObject({ waitId: "wait-1" });
  expect(standIn.reports).toEqual([]);

  fresh.reminder.turnStarted();
  await fresh.reminder.news(DELIVERY_KEY);
  expect(standIn.reports).toEqual([reportOf(implementer, "wait-1", DELIVERY_KEY)]);
  expect(await fresh.reminder.settle(said("Read the delivery."))).toBe(
    reminderText("Read the delivery.", WORK)
  );
});

// R1 and R9: a merger the closed design gate holds is reminded of nothing, and its resume reminder
// arms nothing. Once the gate opens the daemon's answer gives no hold, and the next turn's end
// with no completion gets the reminder.
test("a standing hold sends no reminder, arms no timer, reports no reminder due, and the resume reminder arms nothing; after the gate opens the reminder comes", async () => {
  const standIn = standInDaemon(MERGER_WORK);
  standIn.state.hold = HOLD;
  const merger = waitRig(standIn, { claim: MERGER });

  for (const turn of [said("The gate is closed."), said("Still held."), backgroundWork]) {
    expect(await merger.reminder.settle(turn)).toBeUndefined();
  }
  expect(merger.timers).toEqual([]);
  expect(merger.sent).toEqual([]);
  expect(merger.reminder.phaseWake()).toEqual({ nextReminderAt: null });

  const relaunched = waitRig(standIn, { claim: MERGER });
  await relaunched.reminder.armResumeReminder();
  expect(relaunched.timers).toEqual([]);
  expect(relaunched.reminder.phaseWake()).toEqual({ nextReminderAt: null });

  // The gate opens, and its opening message reaches the merger on its role topic: news, which
  // starts a turn and ends the quiet wait its background work bought.
  standIn.state.hold = undefined;
  merger.reminder.turnStarted();
  await merger.reminder.news(DELIVERY_KEY);
  expect(await merger.reminder.settle(said("The gate opened."))).toBe(
    reminderText("The gate opened.", MERGER_WORK)
  );
  await relaunched.reminder.armResumeReminder();
  expect(relaunched.timers.map((timer) => timer.delayMs)).toEqual([DEFAULT_STALL_LIMITS.resumeMs]);
});

// R3 and D8: news after the declaration is reported to the daemon naming the wait, with the
// delivery's own key verbatim and none for a person's message, and the daemon's answer is the word
// on the wait. The update ask's key is news the daemon answers `ended: false`, which keeps the
// wait standing; another delivery ends it, and that turn's end gets the reminder. A message that
// reached the session before the declaration reports nothing.
test("news after the declaration is reported with the wait and the key, and the daemon's ended:false keeps the wait", async () => {
  const standIn = standInDaemon();
  const worker = waitRig(standIn);
  worker.reminder.turnStarted();
  await worker.reminder.news(DELIVERY_KEY);
  expect(standIn.reports).toEqual([]);
  await worker.declareWait();

  await worker.reminder.news(UPDATE_ASK_KEY);
  expect(standIn.reports).toEqual([reportOf(implementer, "wait-1", UPDATE_ASK_KEY)]);
  expect(await worker.reminder.settle(said("Answered the update ask."))).toBeUndefined();
  expect(worker.timers).toEqual([]);
  expect(worker.sent).toEqual([]);

  worker.reminder.turnStarted();
  await worker.reminder.news(DELIVERY_KEY);
  expect(standIn.reports.at(-1)).toEqual(reportOf(implementer, "wait-1", DELIVERY_KEY));
  expect(standIn.state.wait).toBeUndefined();
  expect(await worker.reminder.settle(said("Read the review."))).toBe(
    reminderText("Read the review.", WORK)
  );

  worker.reminder.turnStarted();
  await worker.declareWait();
  await worker.reminder.news();
  expect(standIn.reports.at(-1)).toEqual(reportOf(implementer, "wait-2"));
  expect(standIn.state.wait).toBeUndefined();
});

// R3: a daemon prompt after the declaration — the update task, the resume task, the wait's own
// task — is no news: it reports nothing and leaves the wait standing.
test("a daemon prompt after the declaration reports nothing and leaves the wait standing", async () => {
  const standIn = standInDaemon();
  const worker = waitRig(standIn);
  await worker.declareWait();
  expect(await worker.reminder.settle(said("Declared the wait."))).toBeUndefined();

  worker.reminder.turnStarted();
  worker.reminder.apply({ kind: "prompt" });
  expect(await worker.reminder.settle(said("Took the task up; CI still runs."))).toBeUndefined();
  expect(standIn.reports).toEqual([]);
  expect(standIn.state.wait).toMatchObject({ id: "wait-1" });
  expect(worker.timers).toEqual([]);
  expect(worker.sent).toEqual([]);
});

// D8: a report that fails ends the wait in the plugin and is kept, with its key. The turn's end
// sends it again first; when that fails too, the turn's end gets the reminder although the
// daemon's answer still gives the wait, and the next turn's end sends it again, which the daemon
// takes and ends the wait with.
test("a failed report gets the reminder and is sent again", async () => {
  const standIn = standInDaemon();
  const worker = waitRig(standIn);
  await worker.declareWait();
  standIn.state.failReports = 2;

  worker.reminder.turnStarted();
  await worker.reminder.news(DELIVERY_KEY);
  expect(worker.entries.at(-1)).toMatchObject({
    waitId: "wait-1",
    unsentNews: [{ waitId: "wait-1", deliveryKey: DELIVERY_KEY }],
  });
  expect(await worker.reminder.settle(said("Read the delivery."))).toBe(
    reminderText("Read the delivery.", WORK)
  );
  expect(standIn.state.wait).toMatchObject({ id: "wait-1" });

  expect(await worker.reminder.settle(said("Still reading."))).toBeUndefined();
  const report = reportOf(implementer, "wait-1", DELIVERY_KEY);
  expect(standIn.reports).toEqual([report, report, report]);
  expect(standIn.state.wait).toBeUndefined();
  expect(worker.entries.at(-1)).not.toHaveProperty("unsentNews");
  await fireLast(worker.timers);
  expect(worker.sent).toEqual([reminderText("Still reading.", WORK)]);
});

// D8: a report not yet sent when the process ends is in the record, and the relaunched process
// sends it at boot, with its key verbatim, before it reads the owed answer: the update ask's key
// is still the update ask's, which keeps the wait standing, and another delivery's ends the wait,
// so the resume reminder is armed and reminds.
test("a report the process could not send is sent again by the relaunched process", async () => {
  for (const key of [UPDATE_ASK_KEY, DELIVERY_KEY]) {
    const standIn = standInDaemon();
    const worker = waitRig(standIn);
    await worker.declareWait();
    standIn.state.failReports = 1;
    worker.reminder.turnStarted();
    await worker.reminder.news(key);
    const record = worker.entries.at(-1);
    expect(record, key).toMatchObject({ unsentNews: [{ waitId: "wait-1", deliveryKey: key }] });

    const relaunched = waitRig(standIn, { restore: record });
    await relaunched.reminder.armResumeReminder();
    expect(standIn.reports, key).toEqual([
      reportOf(implementer, "wait-1", key),
      reportOf(implementer, "wait-1", key),
    ]);
    expect(relaunched.entries.at(-1), key).not.toHaveProperty("unsentNews");
    if (key === UPDATE_ASK_KEY) {
      expect(standIn.state.wait, key).toMatchObject({ id: "wait-1" });
      expect(relaunched.timers, key).toEqual([]);
    } else {
      expect(standIn.state.wait, key).toBeUndefined();
      expect(relaunched.timers.map((timer) => timer.delayMs)).toEqual([
        DEFAULT_STALL_LIMITS.resumeMs,
      ]);
      await fireLast(relaunched.timers);
      expect(relaunched.sent).toEqual([reminderText("", WORK)]);
    }
  }
});

// News that reaches the session while a pass of reports is out goes in the one pass queued behind
// it, however much of it arrives. Over a daemon that answers no report, each pass stops at the
// oldest kept report once the bound passes, so a burst of news costs two bounds, and a turn's end
// then sends one pass of its own before it reads the owed answer. The bound is the reminder's own
// timer, so the case runs on the clock with a short one, as the owed check's case above does.
test("news that arrives while a report is out is sent in one pass queued behind it", async () => {
  const reported: WaitNewsInput[] = [];
  const firstOut = Promise.withResolvers<void>();
  const entries: PhaseStallEntry[] = [];
  const reminder = createPhaseReminder({
    sendReminder: () => {},
    armTimer: () => {},
    saveEntry: (entry) => entries.push(entry),
    claim: () => implementer,
    readOwed: async () => OWES,
    reportNews: (input) => {
      reported.push(input);
      firstOut.resolve();
      return new Promise(() => {});
    },
    publisher: () => undefined,
    project: "LEGION",
    limits: DEFAULT_STALL_LIMITS,
    log: { debug: () => {}, warn: () => {} },
    owedBoundMs: 50,
  });
  reminder.waitDeclared("wait-1");
  reminder.turnStarted();

  const first = reminder.news("publish.first");
  await firstOut.promise;
  const burst = ["publish.second", "publish.third", "publish.fourth"].map((key) =>
    reminder.news(key)
  );
  await Promise.all([first, ...burst]);

  expect(reported.map((report) => report.deliveryKey)).toEqual(["publish.first", "publish.first"]);
  expect(entries.at(-1)?.unsentNews).toEqual(
    ["publish.first", "publish.second", "publish.third", "publish.fourth"].map((deliveryKey) => ({
      waitId: "wait-1",
      deliveryKey,
    }))
  );

  await reminder.settle(said("Read the news."));
  expect(reported.map((report) => report.deliveryKey)).toEqual([
    "publish.first",
    "publish.first",
    "publish.first",
  ]);
});

/** A second delivery's key: news the daemon ends a wait for, as `DELIVERY_KEY` is. */
const SECOND_KEY = "publish.0b8e4d2f6a1c49e7b3d5f8a2c4e6b0d1";

// The daemon applies the news of one wait under one event id (`(*server).waitNews` in
// `packages/daemon/internal/api/wait.go`). Once it has answered a report of the wait that is not
// its update ask, it answers each later report of that wait `ended: false`, as a duplicate. So
// the plugin drops every other kept report of that wait and reports no later news against it. An
// answer of `ended: false` still leaves the wait standing in the plugin (D8).
test("once the daemon has answered a report of a wait that is not its update ask, no other report of that wait goes out", async () => {
  // The kept report ends the wait, and the report kept behind it is dropped.
  const standIn = standInDaemon();
  const worker = waitRig(standIn);
  await worker.declareWait();
  standIn.state.failReports = 1;
  worker.reminder.turnStarted();
  await worker.reminder.news(DELIVERY_KEY);
  await worker.reminder.news(SECOND_KEY);
  const delivery = reportOf(implementer, "wait-1", DELIVERY_KEY);
  expect(standIn.reports).toEqual([delivery, delivery]);
  expect(standIn.state.wait).toBeUndefined();
  expect(worker.entries.at(-1)).not.toHaveProperty("unsentNews");

  // The daemon ended the wait itself (its time came), so it answers the first report
  // `ended: false`. The wait stands in the plugin, and later news reports nothing.
  const timedOut = standInDaemon();
  const waiting = waitRig(timedOut);
  await waiting.declareWait();
  timedOut.state.wait = undefined;
  waiting.reminder.turnStarted();
  await waiting.reminder.news(DELIVERY_KEY);
  await waiting.reminder.news(SECOND_KEY);
  await waiting.reminder.news();
  expect(timedOut.reports).toEqual([delivery]);
  expect(waiting.entries.at(-1)).toMatchObject({ waitId: "wait-1" });
  expect(waiting.entries.at(-1)).not.toHaveProperty("unsentNews");
});

// R1 and R5: owed work with no wait and no hold, and no completion, is reminded at every turn's
// end, and the reminder names `declare_wait` and no WAITING line. The architect is told after 3
// reminders that change nothing, naming the work.
test("owed work with no wait and no hold is reminded, the text names declare_wait, and the architect is told after 3 reminders", async () => {
  const standIn = standInDaemon();
  const worker = waitRig(standIn);

  const continuation = await worker.reminder.settle(said("Pushed."));
  expect(continuation).toBe(reminderText("Pushed.", WORK));
  expect(continuation).toContain("op `declare_wait`");
  expect(continuation).toContain("The update round's message does not end the wait.");
  expect(continuation).toContain("A message whose wake is lost waits until the wait's time.");
  expect(continuation).not.toContain("WAITING");
  for (const reminders of [2, 3]) {
    expect(await worker.reminder.settle(said("Still here."))).toBeUndefined();
    expect(worker.published).toEqual([]);
    await fireLast(worker.timers);
    expect(worker.sent).toHaveLength(reminders - 1);
  }
  expect(worker.published.map((input) => input.message)).toEqual([
    `The implementer of LEGION-208 has let 3 reminders go unanswered: its turns keep ending with ${WORK} still owed, and nothing it does clears the reminder. What it last said was: Still here.`,
  ]);
});

// R1, R9 and D15: the one reminder a wait or a hold lets through is the one for a tool call written
// as text. It says that the call did not run and asks for a real tool call, and asks for no
// `handoff_complete`; during a hold it also names the closed gate, its step and its version, and
// tells the merger to merge nothing and to take a queued pull request back out of the merge queue.
// It keeps every reminder's rhythm, and the architect is told after 3 of them that its tool calls
// written as text did not run.
test("a tool call written as text is reminded during a wait and during a hold, and counts toward the notice", async () => {
  const closedNote =
    "The design gate of LEGION-208 closed: version 7 of its spec document " +
    "(d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093) is with its reviewer. Merge nothing, enqueue " +
    "nothing, and take a pull request you queued back out of the merge queue.";
  for (const held of ["wait", "hold"] as const) {
    const standIn = standInDaemon(held === "hold" ? MERGER_WORK : WORK);
    const claim = held === "hold" ? MERGER : implementer;
    const worker = waitRig(standIn, { claim });
    if (held === "wait") await worker.declareWait();
    else standIn.state.hold = HOLD;

    const continuation = String(await worker.reminder.settle(said(TEXT_CALL)));
    expect(continuation, held).toContain(
      "Your last message holds a tool call written as text, so it did not run: make it a real tool call."
    );
    expect(continuation, held).not.toContain("handoff_complete");
    if (held === "hold") expect(continuation, held).toContain(closedNote);
    else expect(continuation, held).not.toContain("design gate");

    for (const reminders of [2, 3]) {
      expect(await worker.reminder.settle(said(TEXT_CALL)), held).toBeUndefined();
      expect(worker.timers.at(-1)?.delayMs, held).toBe(DEFAULT_STALL_LIMITS.reminderMs);
      await fireLast(worker.timers);
      expect(worker.sent, held).toHaveLength(reminders - 1);
    }
    expect(worker.sent, held).toEqual([continuation, continuation]);
    expect(
      worker.published.map((input) => input.message),
      held
    ).toEqual([
      `The ${claim.role} of LEGION-208 has let 3 reminders go unanswered: its tool calls written as text did not run, and nothing it does clears the reminder. What it last said was: ${TEXT_CALL}`,
    ]);
  }
});
