import { afterEach, expect, setSystemTime, test } from "bun:test";
import { createPhaseReminder } from "./phase-reminder";
import {
  ASYNC_JOB_KIND,
  asyncJobKinds,
  type RunningAsyncJobs,
  type SessionActivityPhase,
  SUBAGENT_KIND,
  sameSessionActivity,
  sessionActivity,
  sessionActivityRequest,
} from "./session-activity";
import { DEFAULT_STALL_LIMITS } from "./stall-limits";

// A row that pins the clock with `setSystemTime` and is ended by the command's guard never
// reaches its own `finally`, so the real clock comes back here too, before the next row.
afterEach(() => {
  setSystemTime();
});

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

/** The reminder made afresh, over a daemon that answers the claim owes work and holds no record,
 * with no timer of its own fired. */
function reminderRig() {
  const timers: { readonly delayMs: number }[] = [];
  const reminder = createPhaseReminder({
    sendReminder: () => {},
    armTimer: (_callback, delayMs) => {
      timers.push({ delayMs });
    },
    saveEntry: () => {},
    claim: () => implementer,
    readOwed: async () => ({ owes: true, work: "the implementing phase of LEGION-208" }),
    publisher: async () => undefined,
    project: "legion",
    limits: DEFAULT_STALL_LIMITS,
    log: { debug: () => {}, warn: () => {} },
  });
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
  // Each wake is stamped on `Date.now()`, held here at one moment so each is read exactly.
  const settledAt = Date.now();
  setSystemTime(new Date(settledAt));
  let ordinary: SessionActivityPhase;
  let waiting: SessionActivityPhase;
  try {
    await reminder.settle({ kind: "settle", lastAssistantText: "Still pushing." });
    ordinary = reminder.phaseWake();
    await reminder.settle({
      kind: "settle",
      lastAssistantText: "WAITING on CI to finish the run.",
    });
    waiting = reminder.phaseWake();
  } finally {
    setSystemTime();
  }

  expect(timers.map((timer) => timer.delayMs)).toEqual([
    DEFAULT_STALL_LIMITS.reminderMs,
    DEFAULT_STALL_LIMITS.waitingMs,
  ]);
  expect(ordinary, "the ordinary settle armed a reminder at the ordinary limit").toEqual({
    nextReminderAt: new Date(settledAt + DEFAULT_STALL_LIMITS.reminderMs).toISOString(),
  });
  expect(waiting, "the wait buys the waiting limit instead of the ordinary one").toEqual({
    nextReminderAt: new Date(settledAt + DEFAULT_STALL_LIMITS.waitingMs).toISOString(),
  });

  // A turn that starts makes every armed timer stale, so the report says no wake at all, whatever
  // timer is still armed on the host.
  reminder.turnStarted();
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

// A claim's token names its agent, and its role is the role of the step it runs now: a claim of
// the agent implementer that runs a tester's step reports for its own token. The fault: the token
// spelled from the role, `…-tester`, a claim the session does not hold, which the daemon refuses.
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
