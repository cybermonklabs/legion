import { expect, test } from "bun:test";
import type { LegionOwed } from "@legion/contracts/legion-api";
import { createPhaseReminder } from "./phase-reminder";
import {
  ASYNC_JOB_KIND,
  asyncJobKinds,
  type RunningAsyncJobs,
  SUBAGENT_KIND,
  sameSessionActivity,
  sessionActivity,
  sessionActivityRequest,
} from "./session-activity";
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

const AT = new Date("2026-11-01T12:00:00.000Z");

/** A snapshot of the jobs a host says are running, in the shape `getAsyncJobSnapshot` answers. */
function snapshot(...types: readonly string[]): RunningAsyncJobs {
  return { running: types.map((type) => ({ type })) };
}

// The report the daemon reads: a backgrounded shell command and a subagent are two kinds of work
// that hold the pod; once both have ended the same session reports none, and the daemon may
// release it.
test("the report names the background work running, and none once it has ended", () => {
  const noWake = { nextReminderAt: null };

  const running = sessionActivity(asyncJobKinds(snapshot("bash", "task", "bash")), noWake, AT);
  const ended = sessionActivity(asyncJobKinds(snapshot()), noWake, AT);

  expect(running).toEqual({
    at: "2026-11-01T12:00:00.000Z",
    // Sorted and de-duplicated: two backgrounded commands are one kind.
    backgroundWork: { running: true, kinds: [ASYNC_JOB_KIND, SUBAGENT_KIND] },
    phase: noWake,
  });
  expect(ended).toEqual({
    at: "2026-11-01T12:00:00.000Z",
    backgroundWork: { running: false, kinds: [] },
    phase: noWake,
  });
  expect(sameSessionActivity(running, ended)).toBeFalse();
  // A host with no such member at all, and one that answers null, both report no background work.
  expect(asyncJobKinds(undefined)).toEqual([]);
  expect(asyncJobKinds(null)).toEqual([]);
});

// `at` is when the session read the report, not part of what it says: a host that sends only on a
// change must not send one reading a second apart from the last.
test("two readings of the same work are the same report, whenever they were read", () => {
  const phase = { nextReminderAt: "2026-11-01T12:05:00.000Z" };
  const one = sessionActivity([ASYNC_JOB_KIND], phase, AT);
  const later = sessionActivity([ASYNC_JOB_KIND], phase, new Date(AT.getTime() + 60_000));

  expect(one.at).not.toBe(later.at);
  expect(sameSessionActivity(one, later)).toBeTrue();
  expect(
    sameSessionActivity(one, sessionActivity([SUBAGENT_KIND], phase, AT)),
    "another kind of work is another report"
  ).toBeFalse();
  expect(
    sameSessionActivity(one, sessionActivity([ASYNC_JOB_KIND], { nextReminderAt: null }, AT)),
    "a phase with no armed reminder is another report"
  ).toBeFalse();
});

/** What the daemon answers the implementer owes: its phase, with no wait and no hold. */
const OWES: LegionOwed = { owes: true, work: "the implementing phase of LEGION-208" };

/** The reminder made afresh, over a daemon that answers `owed` (the claim owes work by default)
 * and holds no record, with no timer of its own fired. No case here reports news: `reported` is
 * every report the reminder sent, which each case holds empty. */
function reminderRig(owed: LegionOwed = OWES) {
  const timers: { readonly delayMs: number }[] = [];
  const reported: unknown[] = [];
  const reminder = createPhaseReminder({
    sendReminder: () => {},
    armTimer: (_callback, delayMs) => {
      timers.push({ delayMs });
    },
    saveEntry: () => {},
    claim: () => implementer,
<<<<<<< 7fa22de66d
    readOwed: async () => owed,
    reportNews: async (input) => {
      reported.push(input);
      throw new Error("this case reports no news");
    },
    publisher: () => undefined,
=======
    readOwed: async () => ({ owes: true, work: "the implementing phase of LEGION-208" }),
    publisher: async () => undefined,
>>>>>>> 67bcd7c78393eb8d85c10bbae0ceb998d1b01253
    project: "legion",
    limits: DEFAULT_STALL_LIMITS,
    log: { debug: () => {}, warn: () => {} },
  });
  return { reminder, timers, reported };
}

// The one statement of when the next reminder is due is the reminder's own: the worker's own
// background work buys the waiting limit instead of the ordinary one, and the report says so.
test("background work moves the reported wake out by the waiting limit", async () => {
  const { reminder, timers, reported } = reminderRig();
  expect(reminder.phaseWake(), "no timer is armed before the first settle").toEqual({
    nextReminderAt: null,
  });

  // An ordinary settle: the continuation is the first reminder of the run, so nothing is armed.
  await reminder.settle({ kind: "settle", lastAssistantText: "Pushed the branch." });
  expect(reminder.phaseWake().nextReminderAt).toBeNull();

  // The next settle arms the ordinary reminder; background work arms the waiting one instead.
  const beforeOrdinary = Date.now();
  await reminder.settle({ kind: "settle", lastAssistantText: "Still pushing." });
  const ordinary = reminder.phaseWake();
  const beforeWaiting = Date.now();
  await reminder.settle({
    kind: "settle",
    lastAssistantText: "Started the build.",
    backgroundWork: true,
  });
  const waiting = reminder.phaseWake();

  expect(timers.map((timer) => timer.delayMs)).toEqual([
    DEFAULT_STALL_LIMITS.reminderMs,
    DEFAULT_STALL_LIMITS.waitingMs,
  ]);
  expect(ordinary.nextReminderAt, "the ordinary settle armed a reminder").not.toBeNull();
  expect(waiting.nextReminderAt, "the background work's settle armed one too").not.toBeNull();
  const ordinaryAt = Date.parse(String(ordinary.nextReminderAt));
  const waitingAt = Date.parse(String(waiting.nextReminderAt));
  expect(ordinaryAt).toBeGreaterThanOrEqual(beforeOrdinary + DEFAULT_STALL_LIMITS.reminderMs);
  expect(waitingAt).toBeGreaterThanOrEqual(beforeWaiting + DEFAULT_STALL_LIMITS.waitingMs);
  expect(
    waitingAt - ordinaryAt,
    "the wait buys the difference between the two limits"
  ).toBeGreaterThanOrEqual(DEFAULT_STALL_LIMITS.waitingMs - DEFAULT_STALL_LIMITS.reminderMs);

  // A turn that starts makes every armed timer stale, so the report says no wake at all, whatever
  // timer is still armed on the host.
  reminder.turnStarted();
  expect(reminder.phaseWake()).toEqual({ nextReminderAt: null });
  expect(reported).toEqual([]);
});

// dispatch://INFRA-2040: while the daemon's answer gives a wait the worker declared, or the closed
// design gate's hold, a turn's end arms no reminder, so the report says that none is due and the
// daemon can release the pod. The same turns with neither standing report one.
test("a turn's end while a wait or a hold stands reports no reminder due", async () => {
  const standing: Record<string, LegionOwed> = {
    "a wait": { ...OWES, wait: { id: "wait-1" } },
    "a hold": {
      owes: true,
      work: "the merging phase of LEGION-208",
      hold: { step: "spec", artifactId: "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093", version: 7 },
    },
    neither: OWES,
  };
  for (const [held, owed] of Object.entries(standing)) {
    const { reminder, reported } = reminderRig(owed);
    await reminder.settle({ kind: "settle", lastAssistantText: "Pushed the branch." });
    await reminder.settle({ kind: "settle", lastAssistantText: "Still here." });
    const report = sessionActivity([], reminder.phaseWake(), AT);
    if (held === "neither") expect(report.phase.nextReminderAt, held).not.toBeNull();
    else expect(report.phase, held).toEqual({ nextReminderAt: null });
    expect(reported, held).toEqual([]);
  }
});

// The claim token the request carries is the one the daemon minted for this claim; a pane with no
// project cannot spell one, and sends nothing rather than throwing inside a host's hook.
test("the request is the claim's own, and is left unbuilt when no token can be spelled", () => {
  const report = sessionActivity([], { nextReminderAt: null }, AT);

  expect(sessionActivityRequest({ project: "legion", claim: implementer, report })).toEqual({
    claimToken: "legion-legion-legion-208-implementer",
    sessionId: "ses_implementer",
    secret: "secret",
    activity: report,
  });
  expect(
    sessionActivityRequest({ project: undefined, claim: implementer, report })
  ).toBeUndefined();
  expect(
    sessionActivityRequest({
      project: "legion",
      claim: { ...implementer, issue: "not an issue key" },
      report,
    })
  ).toBeUndefined();
});
