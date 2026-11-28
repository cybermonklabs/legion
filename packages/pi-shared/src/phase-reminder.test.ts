import { expect, test } from "bun:test";
import { EnvelopeSchema } from "@legion/contracts";
import type { LegionOwed } from "@legion/contracts/legion-api";
import {
  createEnvoyClient,
  EnvoyApiError,
  type PublishInput,
  type PublishResult,
} from "@legion/envoy-client/transport";
import { createLegionDaemonClient, type OwedInput } from "./daemon-client";
import type { ClaimCapability } from "./legion-phase-ops";
import { createPhaseReminder, type EnvoyPublish, type PhaseReminder } from "./phase-reminder";
import { type PhaseStallEntry, phaseStallEntryFrom, reminderText } from "./phase-stall";
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

/** The line a failed read writes at warn level, once per claim per hour. */
const UNREAD_LINE = "legion: the daemon did not say what this claim owes; no reminder is sent";

/** A daemon nothing reaches: every read of what the claim owes fails. */
const unreachable = async (): Promise<LegionOwed> => {
  throw new Error("connect ECONNREFUSED");
};

// A daemon that never answers the owed check: the check gives up once its bound passes, and the
// request it started is aborted then rather than left open, one request per reminder. A read that
// fails reminds no one, and the log says why.
test("the owed check aborts a daemon request that outlives its bound, and reminds no one", async () => {
  const handed: AbortSignal[] = [];
  const asked: OwedInput[] = [];
  const warned: string[] = [];
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
    publisher: async () => undefined,
    project: "LEGION",
    limits: DEFAULT_STALL_LIMITS,
    log: { debug: () => {}, warn: (message) => warned.push(message) },
    wakeChanged: () => {},
    owedBoundMs: 20,
  });

  const continuation = await reminder.settle(said("Pushed."));

  expect(continuation).toBeUndefined();
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
  expect(warned).toEqual([UNREAD_LINE]);
});

/** A reminder over a real daemon client whose fetch is `answer`, recording every route the
 * check asks for and every line it logs at warn. */
function overDaemon(answer: typeof fetch) {
  const routes: string[] = [];
  const warned: string[] = [];
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
    publisher: async () => undefined,
    project: "LEGION",
    limits: DEFAULT_STALL_LIMITS,
    log: { debug: () => {}, warn: (message) => warned.push(message) },
    wakeChanged: () => {},
  });
  return { reminder, routes, warned };
}

// The check keeps its two branches apart: the daemon's answer names the work the reminder gives,
// and everything else is a read that failed, which reminds no one and writes its warn line
// (dispatch://INFRA-2746 R7). It asks the owed route alone and never reads the whole project.
test("the owed check asks one route: the answer names the work, a transport failure is the read that failed", async () => {
  const answered = overDaemon((async (_input, _init) => Response.json(OWES)) as typeof fetch);

  expect(await answered.reminder.settle(said("Pushed."))).toBe(reminderText("Pushed.", WORK));
  expect(answered.routes).toEqual(["/legion/v1/claims/owed"]);
  expect(answered.warned).toEqual([]);

  const failed = overDaemon((async (_input, _init): Promise<Response> => {
    throw new TypeError("fetch failed");
  }) as typeof fetch);

  expect(await failed.reminder.settle(said("Pushed."))).toBeUndefined();
  expect(failed.routes).toEqual(["/legion/v1/claims/owed"]);
  expect(failed.warned).toEqual([UNREAD_LINE]);
});

interface ArmedTimer {
  readonly callback: () => void | Promise<void>;
  readonly delayMs: number;
}

/** One line the runner wrote at warn level, with the context it was written with. */
interface WarnLine {
  readonly message: string;
  readonly context: Record<string, unknown> | undefined;
}

interface Rig {
  readonly reminder: PhaseReminder;
  /** Every timer the runner armed, in order; a case fires one by calling its callback. */
  readonly timers: ArmedTimer[];
  /** Every reminder handed to `sendReminder`, delivered or not. */
  readonly sent: string[];
  readonly entries: PhaseStallEntry[];
  readonly published: PublishInput[];
  /** Every warn line the runner wrote, with its context, in order. */
  readonly warnings: WarnLine[];
  /** How many times the daemon was asked what the claim owes. */
  readonly asked: () => number;
  /** How many times the runner asked the host to send the session's activity report now
   * (`wakeChanged`). */
  readonly wakesChanged: () => number;
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
  /** The runner's clock; the real one when it is left out. */
  readonly now?: () => number;
}): Rig {
  const timers: ArmedTimer[] = [];
  const sent: string[] = [];
  const entries: PhaseStallEntry[] = [];
  const published: PublishInput[] = [];
  const warnings: WarnLine[] = [];
  let asked = 0;
  let wakesChanged = 0;
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
    publisher: async () => {
      await options.buildPublisher?.();
      return async (input) => {
        published.push(input);
        return options.publish === undefined ? {} : options.publish(input);
      };
    },
    project: "legion",
    limits: { ...DEFAULT_STALL_LIMITS, architectAfter: 2 },
    log: {
      debug: () => {},
      warn: (message, context) => {
        warnings.push({ message, context });
      },
    },
    wakeChanged: () => {
      wakesChanged += 1;
    },
    ...(options.owedBoundMs === undefined ? {} : { owedBoundMs: options.owedBoundMs }),
    ...(options.now === undefined ? {} : { now: options.now }),
  });
  if (options.restore !== undefined) reminder.restore(options.restore);
  return {
    reminder,
    timers,
    sent,
    entries,
    published,
    warnings,
    asked: () => asked,
    wakesChanged: () => wakesChanged,
  };
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
  // A quiet wait is still armed — the rhythm is the reminder's own — and the daemon is asked
  // again when it is due, and answers the same.
  expect(await reminder.settle(said("WAITING on CI."))).toBeUndefined();
  expect(timers.map((timer) => timer.delayMs)).toEqual([DEFAULT_STALL_LIMITS.waitingMs]);
  await fireLast(timers);

  expect(sent).toEqual([]);
  expect(asked()).toBe(3);
});

// Where the daemon's answer does not arrive, the worker is reminded of nothing, at its settle or
// at a timer, and the log says why: one warn line per claim per hour, which names the claim, its
// issue and the cause. Each failure is the real client's, over a daemon that fails that way.
test("a daemon that does not answer sends no reminder, and the log names each cause", async () => {
  const failures: Record<string, { readonly fetch: typeof fetch; readonly cause: string }> = {
    unreachable: {
      fetch: (async () => {
        throw new TypeError("fetch failed");
      }) as unknown as typeof fetch,
      cause: "fetch failed",
    },
    "a hang past the bound": {
      fetch: (async (_input: unknown, init?: RequestInit) => {
        const aborted = Promise.withResolvers<Response>();
        init?.signal?.addEventListener("abort", () => aborted.reject(init.signal?.reason));
        return aborted.promise;
      }) as unknown as typeof fetch,
      cause: "no answer within 20ms",
    },
    "a refusal": {
      fetch: (async () =>
        Response.json(
          { error: "Invalid session secret" },
          { status: 403 }
        )) as unknown as typeof fetch,
      cause: "Invalid session secret",
    },
    "a failure to read the record": {
      fetch: (async () =>
        Response.json(
          { error: "read the issue record: connection refused" },
          { status: 500 }
        )) as unknown as typeof fetch,
      cause: "read the issue record: connection refused",
    },
    "an answer the contract refuses": {
      fetch: (async () => Response.json({ owes: true, work: "" })) as unknown as typeof fetch,
      cause: "work names the owed work exactly when owes is true",
    },
  };
  for (const [failure, { fetch: fetchFn, cause }] of Object.entries(failures)) {
    const client = createLegionDaemonClient("http://daemon.test", {
      pluginContract: CONTRACT,
      fetch: fetchFn,
    });
    // The bound is the reminder's own, cut short so the hang is over in 20ms.
    const { reminder, timers, sent, warnings } = rig({
      owed: (input, signal) => client.owed(input, { signal }),
      owedBoundMs: 20,
    });

    expect(await reminder.settle(said("Pushed.")), failure).toBeUndefined();
    // The resume timer asks again when it is due: it sends nothing, and arms nothing after it.
    reminder.armResumeReminder();
    await fireLast(timers);
    expect(sent, failure).toEqual([]);
    expect(timers, failure).toHaveLength(1);

    // Two failed reads of one claim inside the hour, and one line.
    expect(warnings, failure).toEqual([
      {
        message: UNREAD_LINE,
        context: {
          claimToken: implementer.claimToken,
          issue: implementer.issue,
          cause: expect.stringContaining(cause),
        },
      },
    ]);
  }
});

// The other side of the rows above, over the same real client: the daemon's success that the
// claim owes nothing reminds of nothing too, and its timer asks the daemon again.
test("a daemon that answers 200 that the claim owes nothing is reminded of nothing", async () => {
  const client = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: (async () => Response.json(OWES_NOTHING)) as unknown as typeof fetch,
  });
  const { reminder, timers, sent, asked } = rig({
    owed: (input, signal) => client.owed(input, { signal }),
  });

  expect(await reminder.settle(said("Pushed."))).toBeUndefined();
  // A quiet wait is armed, and its timer asks the daemon again.
  expect(await reminder.settle(said("WAITING on CI."))).toBeUndefined();
  await fireLast(timers);
  expect(sent).toEqual([]);
  expect(asked()).toBe(2);
});

/** The tester of LEGION-208, and the work the daemon answers is its own when its phase is open. */
const tester: ClaimCapability = {
  ...implementer,
  role: "tester",
  claimToken: "legion-legion-legion-208-tester",
};
const TESTING = "the testing phase of LEGION-208";

interface StandInDaemon {
  /** What the claim owes, read through the real client. */
  readonly owed: (input: OwedInput, signal: AbortSignal) => Promise<LegionOwed>;
  /** The daemon moves the issue out of the claim's phase. From then on the owed route answers
   * that the claim owes nothing, as `owedBy` (`packages/daemon/internal/api/owed.go`) answers once
   * the issue's phase is another role's. */
  readonly move: () => void;
  /** How many times anything asked the stand-in for the daemon's whole state. */
  readonly stateReads: () => number;
}

/** The real daemon client (`createLegionDaemonClient`) over a stand-in daemon whose owed route
 * answers `owed` until `move` is called, and whose `GET /legion/v1/state` never answers, as
 * INFRA's whole-state read ran past every bound. */
function standInDaemon(owed: LegionOwed): StandInDaemon {
  let answer = owed;
  let stateReads = 0;
  const client = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: (async (input: string | URL | Request) => {
      const path = new URL(input instanceof Request ? input.url : input).pathname;
      if (path === "/legion/v1/state") {
        stateReads += 1;
        return new Promise<Response>(() => {});
      }
      if (path === "/legion/v1/claims/owed") return Response.json(answer);
      return Response.json({ error: "no stand-in route" }, { status: 404 });
    }) as unknown as typeof fetch,
  });
  return {
    owed: (input, signal) => client.owed(input, { signal }),
    move: () => {
      answer = OWES_NOTHING;
    },
    stateReads: () => stateReads,
  };
}

// R1: the daemon answers that a claim owes nothing when the issue's phase is another role's, as
// for a tester whose issue the daemon moved back to implementing. Neither its settle nor a timer
// reminds it.
test("a record of another role phase sends no reminder", async () => {
  const daemon = standInDaemon(OWES_NOTHING);
  const { reminder, timers, sent, asked } = rig({ claim: () => tester, owed: daemon.owed });

  expect(await reminder.settle(said("Tested the change."))).toBeUndefined();
  // A WAITING reply buys a quiet wait, whose timer asks again and sends nothing; so does the
  // resume timer.
  expect(await reminder.settle(said("WAITING on CI."))).toBeUndefined();
  await fireLast(timers);
  reminder.armResumeReminder();
  await fireLast(timers);

  expect(sent).toEqual([]);
  expect(asked()).toBe(3);
  expect(daemon.stateReads()).toBe(0);
});

// The control: the daemon answers that the tester owes its own open phase, and the reminder goes,
// at the settle and at the timer after it, naming that work.
test("the role own open phase is reminded", async () => {
  const daemon = standInDaemon({ owes: true, work: TESTING });
  const { reminder, timers, sent } = rig({ claim: () => tester, owed: daemon.owed });

  const continuation = await reminder.settle(said("Tested the change."));
  expect(continuation).toBe(reminderText("Tested the change.", TESTING));
  expect(continuation).toContain(`Your turn ended with ${TESTING} still owed`);
  expect(await reminder.settle(said("Still testing."))).toBeUndefined();
  await fireLast(timers);

  expect(sent).toEqual([reminderText("Still testing.", TESTING)]);
  expect(daemon.stateReads()).toBe(0);
});

// R7: a read that runs past its bound reminds no one, at the settle or at a timer. The request it
// started is aborted at the bound, and nothing is armed in the reminder's place.
test("a read past its bound sends no reminder", async () => {
  const handed: AbortSignal[] = [];
  const { reminder, timers, sent } = rig({
    owed: (_input, signal) => {
      handed.push(signal);
      return new Promise<LegionOwed>(() => {});
    },
    owedBoundMs: 20,
  });

  expect(await reminder.settle(said("Pushed."))).toBeUndefined();
  expect(timers).toEqual([]);
  reminder.armResumeReminder();
  await fireLast(timers);

  expect(sent).toEqual([]);
  expect(handed.map((signal) => signal.aborted)).toEqual([true, true]);
  // The resume timer is the last one armed: its check armed nothing after it.
  expect(timers).toHaveLength(1);
  expect(reminder.phaseWake()).toEqual({ nextReminderAt: null });
});

// R7: a failed read writes one warn line per claim per hour, which names the cause, and the hour
// counts from the line written. The runner's clock is stubbed here.
test("a failed read logs its cause once per claim per hour", async () => {
  const start = Date.parse("2026-11-19T12:00:00Z");
  let clock = start;
  let claim: ClaimCapability | undefined;
  const { reminder, sent, warnings } = rig({
    claim: () => claim,
    owed: unreachable,
    now: () => clock,
  });

  // Before the session holds a claim, the check has nothing to ask about, and says so once.
  expect(await reminder.settle(said("Booting."))).toBeUndefined();
  expect(await reminder.settle(said("Still booting."))).toBeUndefined();
  expect(warnings).toEqual([{ message: UNREAD_LINE, context: { cause: "no claim yet" } }]);

  // The claim registers, and three of its reads fail inside one hour: one line, for that claim.
  claim = implementer;
  const line: WarnLine = {
    message: UNREAD_LINE,
    context: {
      claimToken: implementer.claimToken,
      issue: implementer.issue,
      cause: "connect ECONNREFUSED",
    },
  };
  for (const minutes of [0, 30, 59]) {
    clock = start + minutes * 60_000;
    expect(await reminder.settle(said("Pushed."))).toBeUndefined();
  }
  expect(warnings.slice(1)).toEqual([line]);

  // An hour after that line, the next failed read writes a second.
  clock = start + 60 * 60_000;
  expect(await reminder.settle(said("Pushed."))).toBeUndefined();
  expect(warnings.slice(1)).toEqual([line, line]);
  expect(sent).toEqual([]);
});

// R1's last sentence: a timer whose check decides not to remind — the daemon answers that the
// claim owes nothing, or the read fails — sends nothing and arms nothing, and it clears the wake
// and asks the host to report that, since no turn starts whose settle would. A wake left live
// past its due time holds the issue's pod from its idle release.
test("a check that decides not to remind clears the wake and asks for a report", async () => {
  const decisions: readonly (readonly [string, () => Promise<LegionOwed>])[] = [
    ["an answer that the claim owes nothing", async () => OWES_NOTHING],
    ["a failed read", unreachable],
  ];
  for (const [label, owed] of decisions) {
    const { reminder, timers, sent, wakesChanged } = rig({ owed });
    reminder.armResumeReminder();
    expect(reminder.phaseWake().nextReminderAt, label).not.toBeNull();

    await fireLast(timers);

    expect(reminder.phaseWake(), label).toEqual({ nextReminderAt: null });
    expect(wakesChanged(), label).toBe(1);
    expect(sent, label).toEqual([]);
    expect(timers, label).toHaveLength(1);
  }

  // The control: a timer whose check reminds starts a turn, and that turn's own settle reports.
  const owing = rig({});
  owing.reminder.armResumeReminder();
  await fireLast(owing.timers);
  expect(owing.sent).toEqual([reminderText("", WORK)]);
  expect(owing.wakesChanged()).toBe(0);
});

// The check awaits the daemon. A turn that started and settled meanwhile armed a wake of its own,
// and the late answer leaves that wake live: it is the newer turn's, not the stale timer's.
test("a check answered after a newer timer was armed leaves that timer's wake live", async () => {
  const answer = Promise.withResolvers<LegionOwed>();
  const { reminder, timers, wakesChanged } = rig({ owed: () => answer.promise });
  reminder.armResumeReminder();
  const firing = fireLast(timers);

  reminder.turnStarted();
  expect(await reminder.settle(said("WAITING on CI."))).toBeUndefined();
  const wake = reminder.phaseWake();
  expect(wake.nextReminderAt).not.toBeNull();

  answer.resolve(OWES_NOTHING);
  await firing;

  expect(reminder.phaseWake()).toEqual(wake);
  expect(wakesChanged()).toBe(0);
});

// One of the largest groups of reminded roles: a message on the role's topic wakes a role whose
// completion the daemon accepted. The message is an inbound turn, which moves the rhythm's count
// and nothing else, and the turn settles while the daemon answers that the claim owes nothing.
test("a role woken by a role-topic message after its accepted completion is reminded of nothing", async () => {
  let owed: LegionOwed = OWES;
  const { reminder, timers, sent, published } = rig({ owed: async () => owed });

  // The role completes its phase: its last tool call is the completion, which the daemon takes.
  reminder.apply({ kind: "tool-call" });
  owed = OWES_NOTHING;
  expect(await reminder.settle(said("Handed off."))).toBeUndefined();

  // The role-topic message starts a turn, which the role answers and ends.
  reminder.turnStarted();
  reminder.apply({ kind: "inbound" });
  expect(await reminder.settle(said("Answered the architect's question."))).toBeUndefined();
  expect(await reminder.settle(said("WAITING on the architect's reply."))).toBeUndefined();
  await fireLast(timers);

  expect(sent).toEqual([]);
  expect(published).toEqual([]);
  expect(reminder.phaseWake()).toEqual({ nextReminderAt: null });
});

// A daemon that answers the owed read later than the bound has the worker reminded of nothing and
// its architect told nothing: the answer that comes after the bound, naming the work, is never
// read.
test("a read that times out sends no reminder and no notice", async () => {
  // A real daemon over HTTP that answers, saying the claim owes WORK, but only once the test
  // releases it, which it does after both checks were decided: every answer arrives later than
  // the bound. The reads are counted where the reminder makes them, since a read the bound
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

    // The settle's read times out, and no continuation goes.
    expect(await reminder.settle(said("Pushed."))).toBeUndefined();
    // The resume timer's read times out too: nothing is sent, and the architect is told nothing.
    reminder.armResumeReminder();
    await fireLast(timers);
    expect(sent).toEqual([]);
    expect(published).toEqual([]);
    expect(asked).toBe(2);
    expect(answered).toBe(0);

    // The daemon answers now, past the bound: an answer naming the work changes nothing. A read
    // the bound aborted before its request reached the server is never answered, so this waits
    // for the requests the server took.
    release.resolve();
    await Promise.all(taken);
    expect(sent).toEqual([]);
    expect(published).toEqual([]);
    expect(reminder.phaseWake()).toEqual({ nextReminderAt: null });
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
  // One owed read per reminder, and no other daemon read.
  expect(asked()).toBe(2);
});

// A child whose own architect is retired or failed is owned by the architect above it, which the
// daemon names in its stead: the notice goes to the architect named. An answer that names no
// architect names nobody, and the notice goes to the tree's architect, named from the claim's tree
// and LEGION_PROJECT. An answer that named the owner is kept, so a notice whose own answer names
// none still reaches it. A read that fails at every reminder sends no reminder, so it earns no
// notice to anyone.
test("a child whose own architect is not live has its worker's notice go to the architect the daemon names, else the tree's", async () => {
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
    {
      label:
        "the continuation's answer named the child's architect, the notice's answer named none",
      owed: async () => {
        reads += 1;
        return reads === 1 ? childOwes(CHILD_ARCHITECT) : childOwes();
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

  const failing = rig({ claim: () => child, owed: unreachable });
  expect(await failing.reminder.settle(said("Pushed."))).toBeUndefined();
  expect(await failing.reminder.settle(said("Still pushing the change."))).toBeUndefined();
  failing.reminder.armResumeReminder();
  await fireLast(failing.timers);
  expect(failing.sent).toEqual([]);
  expect(failing.published).toEqual([]);
});

// The architect notice renders the daemon's answer: the work the daemon named, in its text and in
// its facts.
test("the architect notice names the work the daemon gave", async () => {
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
});

/** The settles of a run that, were every one of them reminded, reaches `architectAfter` (2 in the
 * rig) and passes it: the continuation, a timer, a quiet wait's timer and one more timer. Each
 * timer a settle arms is fired as it comes due. */
async function settlePastTheThreshold({ reminder, timers }: Rig): Promise<void> {
  for (const text of ["Pushed.", "Still here.", "WAITING on CI.", "Still here."]) {
    const armed = timers.length;
    await reminder.settle(said(text));
    if (timers.length > armed) await fireLast(timers);
  }
}

// R5: the notice follows the daemon's answer. A role whose claim does not work the issue's current
// phase is answered that it owes nothing, so it is reminded of nothing, and no run of reminders
// reaches its architect. The reminder reads that one answer and never the whole state, which the
// stand-in never answers.
test("tells the architect nothing for a role whose claim does not work the current phase", async () => {
  const daemon = standInDaemon(OWES_NOTHING);
  const run = rig({ claim: () => tester, owed: daemon.owed });

  await settlePastTheThreshold(run);

  expect(run.sent).toEqual([]);
  expect(run.published).toEqual([]);
  expect(run.asked()).toBe(4);
  expect(daemon.stateReads()).toBe(0);
});

// The control: a role that owes its phase is reminded past `architectAfter`, and its architect is
// told once, by a notice whose source is the worker's own session.
test("still tells the architect for a role that works the current phase", async () => {
  const daemon = standInDaemon({ owes: true, work: TESTING });
  const run = rig({ claim: () => tester, owed: daemon.owed });

  await settlePastTheThreshold(run);

  expect(run.sent).toHaveLength(3);
  expect(run.published).toHaveLength(1);
  expect(run.published[0]?.sourceSessionID).toBe(tester.sessionID);
  expect(JSON.parse(String(run.published[0]?.payload))).toMatchObject({
    role: "tester",
    work: TESTING,
  });
  expect(daemon.stateReads()).toBe(0);
});

// R8: the daemon moves an issue out of a role's phase while the role's turn runs. On 2026-11-17 a
// conflict moved INFRA-1117 and INFRA-3080 out of testing, and their testers were reminded at
// every turn end after it. From the move on the daemon answers that the tester owes nothing, so
// the turn ends with no continuation, no timer after it reminds, and no run of settles past
// `architectAfter` tells the architect. The tester made no completion call: the daemon's answer
// alone ends the reminding.
test("a tester the daemon moved out of its phase while in a turn ends that turn with no reminder, having made no handoff_complete call", async () => {
  const daemon = standInDaemon({ owes: true, work: TESTING });
  const run = rig({ claim: () => tester, owed: daemon.owed });

  // The daemon's task starts the turn while the testing phase is open, and the tester runs the
  // suite: tool calls, none of them handoff_complete.
  run.reminder.turnStarted();
  run.reminder.apply({ kind: "inbound" });
  run.reminder.apply({ kind: "tool-call" });
  // The daemon moves the issue back to implementing while the turn runs.
  daemon.move();

  expect(await run.reminder.settle(said("Ran the suite; two tests fail."))).toBeUndefined();
  await settlePastTheThreshold(run);

  expect(run.sent).toEqual([]);
  expect(run.published).toEqual([]);
  // The daemon was asked at each decision: at three settles, and at the two timers of the quiet
  // wait, which fired and sent nothing.
  expect(run.asked()).toBe(5);
  expect(run.timers.map((timer) => timer.delayMs)).toEqual([
    DEFAULT_STALL_LIMITS.waitingMs,
    DEFAULT_STALL_LIMITS.waitingMs,
  ]);
  expect(daemon.stateReads()).toBe(0);
});

/** The reviewer of LEGION-208, and the work the daemon answers is its own while the issue is in
 * review. */
const reviewer: ClaimCapability = {
  ...implementer,
  role: "reviewer",
  claimToken: "legion-legion-legion-208-reviewer",
};
const REVIEWING = "the reviewing phase of LEGION-208";

// R8: a role's own backward move is a move out of its phase too. The reviewer finds the change
// wrong and calls request_backward_move, which the daemon takes: the issue is back in
// implementing, and from then on the daemon answers that the reviewer owes nothing. The reviewer
// made no completion call, and nothing reminds it: not the turn's end, not a timer, not a run of
// settles past `architectAfter`.
test("a reviewer moved out of its phase by its own request_backward_move is reminded of nothing", async () => {
  const daemon = standInDaemon({ owes: true, work: REVIEWING });
  const run = rig({ claim: () => reviewer, owed: daemon.owed });

  // The daemon's task starts the turn while the issue is in review. The reviewer reads the change
  // and calls request_backward_move, and the daemon moves the issue as the call runs.
  run.reminder.turnStarted();
  run.reminder.apply({ kind: "inbound" });
  run.reminder.apply({ kind: "tool-call" });
  daemon.move();

  expect(
    await run.reminder.settle(said("Sent the change back to implementing: the retry never ends."))
  ).toBeUndefined();
  await settlePastTheThreshold(run);

  expect(run.sent).toEqual([]);
  expect(run.published).toEqual([]);
  expect(run.asked()).toBe(5);
  expect(run.timers).toHaveLength(2);
  expect(daemon.stateReads()).toBe(0);
});

// The control: the same turn of the tester, and no move. The daemon still answers that the
// tester owes its open phase, so the turn that ended with no completion is continued, each timer
// after it reminds, and the run tells the architect once it reaches `architectAfter`.
test("a tester not moved out of its phase is still reminded when its turn ends with no completion", async () => {
  const daemon = standInDaemon({ owes: true, work: TESTING });
  const run = rig({ claim: () => tester, owed: daemon.owed });

  run.reminder.turnStarted();
  run.reminder.apply({ kind: "inbound" });
  run.reminder.apply({ kind: "tool-call" });

  expect(await run.reminder.settle(said("Ran the suite; two tests fail."))).toBe(
    reminderText("Ran the suite; two tests fail.", TESTING)
  );
  await settlePastTheThreshold(run);

  expect(run.sent).toEqual(
    ["Pushed.", "Still here.", "WAITING on CI.", "Still here."].map((text) =>
      reminderText(text, TESTING)
    )
  );
  expect(run.published).toHaveLength(1);
  expect(JSON.parse(String(run.published[0]?.payload))).toMatchObject({
    role: "tester",
    work: TESTING,
  });
  expect(daemon.stateReads()).toBe(0);
});

// R8 and R1's last sentence: a timer armed while the daemon answers that the tester owes its
// phase fires after the daemon moved the issue out of that phase. Its check reads the answer of
// the moment it fires, which is that the tester owes nothing: it sends nothing, arms nothing, and
// clears the wake it was armed with, and it asks the host to report that. A wake left live past
// its due time holds the issue's pod from its idle release.
test("a reminder timer armed before the daemon moved the issue out of its phase fires after the move, sends no reminder and clears its wake", async () => {
  const daemon = standInDaemon({ owes: true, work: TESTING });
  const run = rig({ claim: () => tester, owed: daemon.owed });

  // The tester's turn ends while its phase is open, and the continuation goes. The turn the
  // continuation starts ends with no completion too, and its settle arms the timer of the next
  // reminder.
  expect(await run.reminder.settle(said("Ran the suite; two tests fail."))).toBe(
    reminderText("Ran the suite; two tests fail.", TESTING)
  );
  run.reminder.turnStarted();
  expect(await run.reminder.settle(said("Still running the suite."))).toBeUndefined();
  expect(run.timers.map((timer) => timer.delayMs)).toEqual([DEFAULT_STALL_LIMITS.reminderMs]);
  expect(run.reminder.phaseWake().nextReminderAt).not.toBeNull();

  // The daemon moves the issue back to implementing, and then the timer fires.
  daemon.move();
  await fireLast(run.timers);

  expect(run.sent).toEqual([]);
  expect(run.published).toEqual([]);
  expect(run.timers).toHaveLength(1);
  expect(run.reminder.phaseWake()).toEqual({ nextReminderAt: null });
  expect(run.wakesChanged()).toBe(1);
  expect(run.asked()).toBe(2);
});

// A turn whose read failed counts no reminder toward the notice: a worker one reminder short of it
// whose next three reads fail tells its architect nothing, and its count stands.
test("three failed reads send no report to the architect", async () => {
  const { reminder, timers, sent, published, entries, asked } = rig({
    owed: unreachable,
    restore: { state: "active", count: 1, reported: false },
  });

  // The restored count makes each settle's reminder a timer's, and each timer's read fails.
  for (const text of ["Still here.", "Still here.", "Still here."]) {
    expect(await reminder.settle(said(text))).toBeUndefined();
    await fireLast(timers);
  }

  expect(asked()).toBe(3);
  expect(sent).toEqual([]);
  expect(published).toEqual([]);
  expect(entries.at(-1)).toMatchObject({ count: 1, reported: false });
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

    reminder.armResumeReminder();
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
    owing.reminder.armResumeReminder();
    await fireLast(owing.timers);
    expect(owing.sent, label).toEqual([reminderText("", WORK)]);
  }
});

// The daemon's own update message arrives over Envoy, which the reminder reads as an inbound
// message. It moves the rhythm's count and nothing else, so a role whose phase has closed is
// reminded of nothing after it. There is no guard for this: with no model of the phase, nothing
// an inbound message moves can decide that a reminder goes out.
test("an update message sent to a role whose phase is closed produces no reminder", async () => {
  const { reminder, timers, sent } = rig({ owed: async () => OWES_NOTHING });

  // The role finished its phase: its last tool call was the completion, and its turn settled.
  reminder.apply({ kind: "tool-call" });
  expect(await reminder.settle(said("Handed off."))).toBeUndefined();

  // The daemon's update message, and the turn it starts.
  reminder.turnStarted();
  reminder.apply({ kind: "inbound" });
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
  const { reminder, timers, entries, published, warnings } = rig({
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
  expect(warnings).toMatchObject([
    { message: "legion: the architect was not told its worker is stalled" },
  ]);
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
  const { reminder, timers, entries, published, warnings } = rig({
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
  expect(warnings).toEqual([]);
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
  expect(warnings).toEqual([]);
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
  const { reminder, timers, entries, published, warnings } = rig({
    publish: (input) => client.publish(input),
  });

  expect(await reminder.settle(said("Pushed."))).toBeString();
  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  await fireLast(timers);

  expect(published).toHaveLength(1);
  expect(warnings.map((line) => line.message)).toEqual([
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
  const { reminder, timers, entries, published, warnings } = rig({
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
  expect(warnings).toEqual([]);
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
  const { reminder, timers, sent, entries, published, warnings } = rig({
    sendReminder: async () => {
      deliveries += 1;
      if (deliveries === 1) throw new Error("the bridge closed the socket");
    },
  });

  expect(await reminder.settle(said("Pushed."))).toBeString();
  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  expect(timers).toHaveLength(1);
  await fireLast(timers);

  expect(warnings).toMatchObject([
    { message: "legion: a phase reminder did not reach this worker; it comes again later" },
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
// WAITING reply's own wait is untouched: it is the reminder's to time out.
test("the wait an update answer creates arms no reminder, where a WAITING reply still arms one", async () => {
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
  // again, and a WAITING reply buys the quiet wait the reminder itself ends.
  reminder.apply({ kind: "inbound" });
  expect(await reminder.settle(said("WAITING on the reviewer."))).toBeUndefined();

  expect(timers).toHaveLength(1);
  expect(timers[0]?.delayMs).toBe(DEFAULT_STALL_LIMITS.waitingMs);
});
