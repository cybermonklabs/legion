import { expect, setSystemTime, test } from "bun:test";
import type { LegionState } from "@legion/contracts/legion-api";
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
    readState: async () => ({ issues: {} }) as unknown as LegionState,
    publisher: () => undefined,
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
  // Each wake is stamped on `Date.now()`, held here at one moment so each is read exactly.
  const settledAt = Date.now();
  setSystemTime(new Date(settledAt));
  let ordinaryAt: unknown;
  let waitingAt: unknown;
  try {
    await reminder.settle({ kind: "settle", lastAssistantText: "Still pushing." });
    ordinaryAt = reminder.phaseWake().nextReminderAt;
    await reminder.settle({
      kind: "settle",
      lastAssistantText: "WAITING on CI to finish the run.",
    });
    waitingAt = reminder.phaseWake().nextReminderAt;
  } finally {
    setSystemTime();
  }

  expect(timers.map((timer) => timer.delayMs)).toEqual([
    DEFAULT_STALL_LIMITS.reminderMs,
    DEFAULT_STALL_LIMITS.waitingMs,
  ]);
  expect(ordinaryAt, "the ordinary settle armed a reminder").not.toBeNull();
  expect(waitingAt, "the WAITING settle armed one too").not.toBeNull();
  expect(Date.parse(String(ordinaryAt))).toBe(settledAt + DEFAULT_STALL_LIMITS.reminderMs);
  expect(
    Date.parse(String(waitingAt)),
    "the wait buys the waiting limit instead of the ordinary one"
  ).toBe(settledAt + DEFAULT_STALL_LIMITS.waitingMs);

  // A closed phase reports no wake at all, whatever timer is still armed on the host.
  reminder.apply({ kind: "phase-closed" });
  expect(reminder.phaseWake()).toEqual({ nextReminderAt: null });
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
