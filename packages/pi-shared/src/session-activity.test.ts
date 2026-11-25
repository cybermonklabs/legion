import { expect, test } from "bun:test";
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

/** The reminder over an open phase, with no daemon record and no timer of its own fired. */
function reminderRig() {
  const timers: { readonly delayMs: number }[] = [];
  const reminder = createPhaseReminder({
    sendReminder: () => {},
    armTimer: (_callback, delayMs) => {
      timers.push({ delayMs });
    },
    saveEntry: () => {},
    claim: () => implementer,
    readRecord: async () => undefined,
    publisher: async () => undefined,
    project: "legion",
    limits: DEFAULT_STALL_LIMITS,
    log: { debug: () => {}, warn: () => {} },
  });
  reminder.apply({ kind: "assignment" });
  return { reminder, timers };
}

// The one statement of when the next reminder is due is the reminder's own: a WAITING reply buys
// the waiting limit instead of the ordinary one, and the report says so.
test("a WAITING reply moves the reported wake out by the waiting limit", async () => {
  const { reminder, timers } = reminderRig();
  expect(reminder.phaseWake(), "no timer is armed before the first settle").toEqual({
    nextReminderAt: null,
  });

  // An ordinary settle: the continuation is the first reminder of the run, so nothing is armed.
  await reminder.settle({ kind: "settle", lastAssistantText: "Pushed the branch." });
  expect(reminder.phaseWake().nextReminderAt).toBeNull();

  // The next settle arms the ordinary reminder; a WAITING reply arms the waiting one instead.
  const beforeOrdinary = Date.now();
  await reminder.settle({ kind: "settle", lastAssistantText: "Still pushing." });
  const ordinary = reminder.phaseWake();
  const beforeWaiting = Date.now();
  await reminder.settle({ kind: "settle", lastAssistantText: "WAITING on CI to finish the run." });
  const waiting = reminder.phaseWake();

  expect(timers.map((timer) => timer.delayMs)).toEqual([
    DEFAULT_STALL_LIMITS.reminderMs,
    DEFAULT_STALL_LIMITS.waitingMs,
  ]);
  expect(ordinary.nextReminderAt, "the ordinary settle armed a reminder").not.toBeNull();
  expect(waiting.nextReminderAt, "the WAITING settle armed one too").not.toBeNull();
  const ordinaryAt = Date.parse(String(ordinary.nextReminderAt));
  const waitingAt = Date.parse(String(waiting.nextReminderAt));
  expect(ordinaryAt).toBeGreaterThanOrEqual(beforeOrdinary + DEFAULT_STALL_LIMITS.reminderMs);
  expect(waitingAt).toBeGreaterThanOrEqual(beforeWaiting + DEFAULT_STALL_LIMITS.waitingMs);
  expect(
    waitingAt - ordinaryAt,
    "the wait buys the difference between the two limits"
  ).toBeGreaterThanOrEqual(DEFAULT_STALL_LIMITS.waitingMs - DEFAULT_STALL_LIMITS.reminderMs);

  // A closed phase reports no wake at all, whatever timer is still armed on the host.
  reminder.apply({ kind: "phase-closed" });
  expect(reminder.phaseWake()).toEqual({ nextReminderAt: null });
});

// The request carries the claim's own token, secret and session, with the report as its activity.
test("the request is the claim's own", () => {
  const report = sessionActivity([], { nextReminderAt: null }, AT);

  expect(sessionActivityRequest({ claim: implementer, report })).toEqual({
    claimToken: "legion-legion-legion-208-implementer",
    sessionId: "ses_implementer",
    secret: "secret",
    activity: report,
  });
});

// A claim's token names its agent, and its role can be another: a keyed agent's claim holds the
// role of the step it runs now, so a claim of the agent implementer that runs a tester's step
// reports for its own token. The fault: the token spelled from the role, `…-tester`, a claim the
// session does not hold, which the daemon refuses.
test("a session reports its activity for the token it registered, whatever role it holds now", () => {
  const report = sessionActivity([], { nextReminderAt: null }, AT);
  const reroled = {
    ...implementer,
    claimToken: "legion-p-infra-1-implementer",
    tree: "INFRA-1",
    issue: "INFRA-1",
    role: "tester",
  } as const;

  expect(sessionActivityRequest({ claim: reroled, report }).claimToken).toBe(
    "legion-p-infra-1-implementer"
  );
});
