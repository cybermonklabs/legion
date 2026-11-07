import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
// The import above is the whole of this file's merge-base check: on the merge base there is no
// `./stall-watchdog`, so every test here fails to resolve rather than passing vacuously.
import { DEFAULT_STALL_LIMITS, defaultCallBoundMs, type StallLimits } from "./stall-limits";
import {
  callOverdue,
  createStallWatchdog,
  effectiveDeadlineMs,
  STALL_EXIT_CAP,
  STALL_EXIT_CODE,
  type StallCall,
  type StallEvidence,
  type StallProcessTree,
  type StallStepRecord,
  type StallWatchdog,
  type StallWatchdogDeps,
} from "./stall-watchdog";

const SECOND = 1_000;
const MINUTE = 60 * SECOND;
const HOUR = 60 * MINUTE;

/** A wall-clock moment to count from, so every number a failure prints is a difference. */
const START = 1_793_000_000_000;

/** The limits a role runs on with nothing set in its environment. */
function limitsFor(role: string): StallLimits {
  return { ...DEFAULT_STALL_LIMITS, callBoundMs: defaultCallBoundMs(role) };
}

/** The agent's process tree the fake reader answers with, which the evidence carries. */
const TREE: StallProcessTree = {
  read: true,
  processes: [
    {
      pid: 4321,
      command: "sh -c sleep 100000",
      startedAt: START,
      state: "S",
      cpuMs: 0,
      toolCallId: "toolu_1",
    },
  ],
};

interface Rig {
  readonly watchdog: StallWatchdog;
  /** Moves the clock on by `ms`. */
  readonly tick: (ms: number) => void;
  readonly now: () => number;
  readonly told: StallEvidence[];
  readonly steps: StallStepRecord[];
  readonly stopped: { call: StallCall; reason: string }[];
  /** The calls `endCall` was given, by id. */
  readonly endedCalls: string[];
  readonly aborts: number[];
  readonly messages: string[];
  readonly exitCodes: number[];
  /** The counter file's content, and what was written to it. */
  readonly exitsWritten: number[];
  idle: boolean;
  /** The tree the reader answers; a string makes it throw that message. */
  treeAnswer: StallProcessTree | string;
  /** How many processes `endCall` answers; a string makes it throw that message. */
  endCallAnswer: number | string;
  /** What the counter file reads; undefined is a file that could not be read. */
  exitsRead: number | undefined;
  /** Whether the counter file can be written. */
  exitsWritable: boolean;
  /** What the teller answers for every call it is told about. */
  tellAnswer: { evidenceWritten: boolean; ownerTold: boolean };
  /** A message the aborter throws instead of ending the turn. */
  abortThrows: string | undefined;
  /** Whether `abortTurn` answers with a Promise that never settles of itself: the host whose
   * `abort()` waits on the very call that is stuck, which is what bounds the second step. */
  abortHangs: boolean;
  /** Settles the Promise a hung `abortTurn` answered with, late and from outside. */
  readonly settleAbort: () => void;
  /** A message the session refuses the self-message with. */
  sendThrows: string | undefined;
}

function rig(options: { role?: string; limits?: Partial<StallLimits> } = {}): Rig {
  let clock = START;
  /** The resolver of the Promise a hung `abortTurn` last answered with. */
  let hung: (() => void) | undefined;
  const state: Omit<Rig, "watchdog" | "tick" | "now" | "settleAbort"> = {
    told: [],
    steps: [],
    stopped: [],
    endedCalls: [],
    aborts: [],
    messages: [],
    exitCodes: [],
    exitsWritten: [],
    idle: false,
    treeAnswer: TREE,
    endCallAnswer: 1,
    exitsRead: 0,
    exitsWritable: true,
    tellAnswer: { evidenceWritten: true, ownerTold: true },
    abortThrows: undefined,
    abortHangs: false,
    sendThrows: undefined,
  };
  const deps: StallWatchdogDeps = {
    now: () => clock,
    limits: { ...limitsFor(options.role ?? "tester"), ...options.limits },
    isIdle: () => state.idle,
    processes: {
      tree: () => {
        if (typeof state.treeAnswer === "string") throw new Error(state.treeAnswer);
        return state.treeAnswer;
      },
      endCall: (call) => {
        if (typeof state.endCallAnswer === "string") throw new Error(state.endCallAnswer);
        state.endedCalls.push(call.toolCallId);
        return state.endCallAnswer;
      },
    },
    teller: {
      tellOverdue: (evidence) => {
        state.told.push(evidence);
        return state.tellAnswer;
      },
      recordStep: (record) => {
        state.steps.push(record);
      },
      tellStopped: (call, reason) => {
        state.stopped.push({ call, reason });
      },
    },
    aborter: {
      abortTurn: () => {
        if (state.abortThrows !== undefined) throw new Error(state.abortThrows);
        state.aborts.push(clock);
        if (!state.abortHangs) return;
        const never = Promise.withResolvers<void>();
        hung = never.resolve;
        return never.promise;
      },
      sendMessage: (text) => {
        if (state.sendThrows !== undefined) throw new Error(state.sendThrows);
        state.messages.push(text);
      },
    },
    exits: {
      read: () => state.exitsRead,
      write: (count) => {
        if (!state.exitsWritable) return false;
        state.exitsWritten.push(count);
        state.exitsRead = count;
        return true;
      },
    },
    exitProcess: (code) => state.exitCodes.push(code),
    log: { debug: () => {}, warn: () => {} },
  };
  // The same object the deps read, never a copy of it: a test moves `idle` or `exitsRead` on the
  // rig it holds and the next sweep reads what it set.
  return Object.assign(state, {
    watchdog: createStallWatchdog(deps),
    tick: (ms: number) => {
      clock += ms;
    },
    now: () => clock,
    settleAbort: () => hung?.(),
  });
}

/** One call made and handed to its tool, the ordinary path every test but the queue one takes. */
function running(
  watchdog: StallWatchdog,
  toolName: string,
  input: Record<string, unknown> = {},
  toolCallId = "toolu_1"
): void {
  watchdog.callRequested({ toolName, toolCallId, input });
  watchdog.executionStarted({ toolName, toolCallId, input });
}

// ---------------------------------------------------------------------------------------------
// R2: what is recorded, when its clock starts, and what closes it.
// ---------------------------------------------------------------------------------------------

test("the arguments are taken at tool_call and the clock starts at tool_execution_start", () => {
  const r = rig();
  // The arguments the model made the call with, which declare a two-minute deadline.
  r.watchdog.callRequested({
    toolName: "bash",
    toolCallId: "toolu_1",
    input: { command: "sleep 100000", timeout: 120 },
  });
  const recorded = r.watchdog.openCalls()[0];
  expect(recorded?.input).toEqual({ command: "sleep 100000", timeout: 120 });
  expect(recorded?.deadlineMs).toBe(120 * SECOND);
  expect(recorded?.startedAt).toBeUndefined();

  // Half an hour in a queue and at the approval gate: nothing judges a call that has not run.
  r.tick(30 * MINUTE);
  expect(callOverdue(r.watchdog.openCalls()[0] as StallCall, r.now(), limitsFor("tester"))).toBe(
    undefined
  );

  // The host hands the call to its tool. `tool_execution_start` carries the arguments too, and
  // the record keeps the ones `tool_call` took.
  r.watchdog.executionStarted({ toolName: "bash", toolCallId: "toolu_1", input: {} });
  const started = r.watchdog.openCalls()[0] as StallCall;
  expect(started.startedAt).toBe(r.now());
  expect(started.input).toEqual({ command: "sleep 100000", timeout: 120 });
  expect(started.deadlineMs).toBe(120 * SECOND);

  // Judged from its execution: seven minutes after the call was made but one minute after it
  // started, it is inside its own deadline and the grace.
  r.tick(MINUTE);
  expect(callOverdue(r.watchdog.openCalls()[0] as StallCall, r.now(), limitsFor("tester"))).toBe(
    undefined
  );
  // Past its own 120 seconds plus the 5-minute grace, measured from the execution's start.
  r.tick(6 * MINUTE + SECOND);
  const overdue = callOverdue(r.watchdog.openCalls()[0] as StallCall, r.now(), limitsFor("tester"));
  expect(overdue?.rule).toEqual({
    kind: "deadline",
    deadlineMs: 120 * SECOND,
    graceMs: 5 * MINUTE,
  });
  expect(overdue?.openMs).toBe(7 * MINUTE + SECOND);
});

/** Each way the host closes a call's record, with the event the plugin hears it through. A call
 * blocked before it ran has no event of its own — measured, a block and a failed `tool_call`
 * handler each make the host fail the call closed, and the call still gets its
 * `tool_execution_start` and then an error `tool_execution_end` one millisecond later — so it is
 * closed by `tool_execution_end`, named here as its own path. */
const CLOSERS: readonly { readonly name: string; readonly close: (w: StallWatchdog) => void }[] = [
  { name: "tool_execution_end", close: (w) => w.executionEnded({ toolCallId: "toolu_1" }) },
  {
    name: "tool_result",
    close: (w) =>
      w.resultDelivered({
        toolName: "bash",
        toolCallId: "toolu_1",
        input: {},
        details: {},
        isError: false,
      }),
  },
  {
    name: "an assistant message that ends aborted",
    close: (w) => w.assistantMessage({ role: "assistant", stopReason: "aborted" }),
  },
  {
    name: "a call blocked before it ran, through the host's error tool_execution_end",
    close: (w) => w.executionEnded({ toolCallId: "toolu_1" }),
  },
  { name: "the turn's end", close: (w) => w.turnEnded() },
  { name: "a session switch", close: (w) => w.sessionChanged() },
];

test("each of the six paths closes the record, and a closed call is never overdue", async () => {
  for (const closer of CLOSERS) {
    const r = rig();
    running(r.watchdog, "bash", { command: "sleep 100000" });
    expect(r.watchdog.openCalls()).toHaveLength(1);

    closer.close(r.watchdog);
    expect(r.watchdog.openCalls(), closer.name).toHaveLength(0);

    // However long the clock runs on, nothing is told and no step runs for a closed call.
    r.tick(10 * HOUR);
    await r.watchdog.sweep();
    expect(r.told, closer.name).toEqual([]);
    expect(r.steps, closer.name).toEqual([]);
    expect(r.exitCodes, closer.name).toEqual([]);
  }
});

test("an assistant message that did not end aborted closes nothing", () => {
  const r = rig();
  running(r.watchdog, "bash", { command: "sleep 100000" });
  r.watchdog.assistantMessage({ role: "assistant", stopReason: "stop" });
  r.watchdog.assistantMessage({ role: "user" });
  r.watchdog.assistantMessage(undefined);
  expect(r.watchdog.openCalls()).toHaveLength(1);
});

// ---------------------------------------------------------------------------------------------
// R6: which rule judges which call.
// ---------------------------------------------------------------------------------------------

test("the shell tool's deadline is its own number, and 300 seconds where it declares none", () => {
  expect(effectiveDeadlineMs("bash", { command: "ls" })).toBe(300 * SECOND);
  expect(effectiveDeadlineMs("bash", { command: "ls", timeout: 120 })).toBe(120 * SECOND);
  // The host clamps a declared deadline to 1–3600 seconds, and 0 disables it altogether.
  expect(effectiveDeadlineMs("bash", { command: "ls", timeout: 9_000 })).toBe(HOUR);
  expect(effectiveDeadlineMs("bash", { command: "ls", timeout: 0 })).toBeUndefined();
  // A tool whose deadline the host may pause, and one that takes none, promise nothing.
  expect(effectiveDeadlineMs("eval", { code: "1", timeout: 600 })).toBeUndefined();
  expect(effectiveDeadlineMs("task", { prompt: "go" })).toBeUndefined();
});

test("a shell call is overdue past 300 seconds and the grace when it declares none", () => {
  const limits = limitsFor("tester");
  const r = rig();
  running(r.watchdog, "bash", { command: "sleep 100000", pty: true });

  // 300 seconds of deadline and 300 of grace: at the boundary it is still working.
  r.tick(10 * MINUTE);
  expect(callOverdue(r.watchdog.openCalls()[0] as StallCall, r.now(), limits)).toBeUndefined();
  r.tick(SECOND);
  const overdue = callOverdue(r.watchdog.openCalls()[0] as StallCall, r.now(), limits);
  expect(overdue?.rule).toEqual({
    kind: "deadline",
    deadlineMs: 300 * SECOND,
    graceMs: 5 * MINUTE,
  });
  expect(overdue?.pastMs).toBe(SECOND);
});

test("a shell call that declares its own number is judged by that number", () => {
  const limits = limitsFor("tester");
  const r = rig();
  running(r.watchdog, "bash", { command: "sleep 100000", pty: true, timeout: 30 });
  // 30 seconds of its own, plus the 5-minute grace.
  r.tick(5 * MINUTE + 30 * SECOND);
  expect(callOverdue(r.watchdog.openCalls()[0] as StallCall, r.now(), limits)).toBeUndefined();
  r.tick(SECOND);
  expect(callOverdue(r.watchdog.openCalls()[0] as StallCall, r.now(), limits)?.rule).toEqual({
    kind: "deadline",
    deadlineMs: 30 * SECOND,
    graceMs: 5 * MINUTE,
  });
});

test("an eval call is judged by the role's bound alone, never by a deadline", () => {
  const limits = limitsFor("tester");
  const r = rig();
  running(r.watchdog, "eval", { code: "while True: pass" });

  // 40 seconds is past `eval`'s own 30-second default, and the host pauses that deadline across
  // agent and completion waits, so no deadline judges this call at all.
  r.tick(40 * SECOND);
  expect(callOverdue(r.watchdog.openCalls()[0] as StallCall, r.now(), limits)).toBeUndefined();
  // Still nothing at 55 minutes, which is inside this role's hour.
  r.tick(55 * MINUTE - 40 * SECOND);
  expect(callOverdue(r.watchdog.openCalls()[0] as StallCall, r.now(), limits)).toBeUndefined();
  // Past the hour it is overdue, by the bound and not by a deadline.
  r.tick(5 * MINUTE + SECOND);
  expect(callOverdue(r.watchdog.openCalls()[0] as StallCall, r.now(), limits)?.rule).toEqual({
    kind: "bound",
    boundMs: HOUR,
  });
});

test("a shell call that declares 0 is judged by the role's bound", () => {
  const limits = limitsFor("tester");
  const r = rig();
  running(r.watchdog, "bash", { command: "sleep 100000", pty: true, timeout: 0 });
  // Five minutes in, a call the host armed no deadline for is nowhere near its role's bound.
  r.tick(5 * MINUTE);
  expect(callOverdue(r.watchdog.openCalls()[0] as StallCall, r.now(), limits)).toBeUndefined();
  r.tick(HOUR);
  expect(callOverdue(r.watchdog.openCalls()[0] as StallCall, r.now(), limits)?.rule).toEqual({
    kind: "bound",
    boundMs: HOUR,
  });
});

test("a shell call that declares an hour is working ten minutes in", () => {
  const r = rig();
  running(r.watchdog, "bash", { command: "sleep 100000", pty: true, timeout: 3_600 });
  r.tick(10 * MINUTE);
  expect(
    callOverdue(r.watchdog.openCalls()[0] as StallCall, r.now(), limitsFor("tester"))
  ).toBeUndefined();
});

test("an implementer's subagent call open three hours is inside that role's bound", async () => {
  // Three hours is past the longest silence this deployment has measured — 2 h 24 min, for an
  // implementer running a subagent — and inside the 4-hour bound its role gets.
  const r = rig({ role: "implementer" });
  running(r.watchdog, "task", { prompt: "review the change" });
  r.tick(3 * HOUR);
  expect(
    callOverdue(r.watchdog.openCalls()[0] as StallCall, r.now(), limitsFor("implementer"))
  ).toBeUndefined();
  await r.watchdog.sweep();
  expect(r.told).toEqual([]);
  expect(r.steps).toEqual([]);

  // Past four hours the same call is overdue, by the bound.
  r.tick(HOUR + SECOND);
  expect(
    callOverdue(r.watchdog.openCalls()[0] as StallCall, r.now(), limitsFor("implementer"))?.rule
  ).toEqual({ kind: "bound", boundMs: 4 * HOUR });
});

/** k7's negative control, as it is written: "A worker whose bound is 0 records no call and ends
 * nothing, however long a call runs." The deadline-judged call is in it because 0 is the watch
 * off for the worker entirely and not its bound rule waived, which is the issue's own ruling. */
test("a bound of 0 records no call and ends nothing, deadline-judged calls included", async () => {
  const r = rig({ limits: { callBoundMs: 0 } });
  // One call the bound would judge, and one the deadline rule would: a `pty: true` shell call
  // with its own 30 seconds, this issue's measured shape.
  running(r.watchdog, "eval", { code: "while True: pass" }, "toolu_1");
  running(r.watchdog, "bash", { command: "sleep 100000", pty: true, timeout: 30 }, "toolu_2");
  expect(r.watchdog.openCalls()).toEqual([]);

  // Ten hours is past every bound and every deadline this watch knows, and nothing at all has
  // happened: no record to judge, so nobody is told, no step runs and no process is ended.
  r.tick(10 * HOUR);
  await r.watchdog.sweep();
  expect(r.told).toEqual([]);
  expect(r.steps).toEqual([]);
  expect(r.endedCalls).toEqual([]);
  expect(r.aborts).toEqual([]);
  expect(r.exitCodes).toEqual([]);

  // And the rule itself says so, for a reader who reaches it with a call of their own: the
  // deadline branch is behind the off switch, not in front of it.
  const deadlineCall: StallCall = {
    toolCallId: "toolu_3",
    toolName: "bash",
    input: { command: "sleep 100000", pty: true, timeout: 30 },
    startedAt: START,
    deadlineMs: 30 * SECOND,
    step: 0,
    steppedAt: undefined,
    told: undefined,
  };
  const off: StallLimits = { ...limitsFor("tester"), callBoundMs: 0 };
  expect(callOverdue(deadlineCall, START + 10 * HOUR, off)).toBeUndefined();
  expect(callOverdue({ ...deadlineCall, deadlineMs: undefined }, START + 10 * HOUR, off)).toBe(
    undefined
  );
});

// ---------------------------------------------------------------------------------------------
// R3 and R4: the evidence, the owner, and the ladder, driven with no host.
// ---------------------------------------------------------------------------------------------

/** A `pty: true` shell call that writes nothing after its start — this issue's own measured shape
 * — swept until it is overdue. Answers the rig. */
async function stuckShellCall(options: { role?: string } = {}): Promise<Rig> {
  const r = rig(options);
  running(r.watchdog, "bash", { command: "sleep 100000", pty: true });
  r.tick(10 * MINUTE + SECOND);
  await r.watchdog.sweep();
  return r;
}

test("the evidence is written and the owner told once, before any step", async () => {
  const r = await stuckShellCall();

  expect(r.told).toHaveLength(1);
  expect(r.told[0]?.call.toolName).toBe("bash");
  expect(r.told[0]?.call.input).toEqual({ command: "sleep 100000", pty: true });
  expect(r.told[0]?.overdue.openMs).toBe(10 * MINUTE + SECOND);
  expect(r.told[0]?.tree).toBe(TREE);
  // The first step runs in the same sweep, after the telling.
  expect(r.steps.map((step) => step.step)).toEqual(["end-processes"]);
  expect(r.endedCalls).toEqual(["toolu_1"]);

  // The next sweeps inside the step gap tell nobody a second time and run no further step.
  r.tick(SECOND);
  await r.watchdog.sweep();
  expect(r.told).toHaveLength(1);
  expect(r.steps).toHaveLength(1);
});

test("the ladder runs its three steps, each one step gap after the last", async () => {
  const r = await stuckShellCall();
  expect(r.steps.map((step) => step.step)).toEqual(["end-processes"]);

  // Step two only because step one did not end the call within `stepMs`.
  r.tick(2 * MINUTE);
  await r.watchdog.sweep();
  expect(r.steps.map((step) => step.step)).toEqual(["end-processes", "abort-turn"]);
  expect(r.aborts).toHaveLength(1);
  expect(r.messages).toHaveLength(1);
  expect(r.messages[0]).toContain("bash");
  expect(r.messages[0]).toContain("toolu_1");
  // The turn is not over, so nothing has exited yet.
  expect(r.exitCodes).toEqual([]);

  // Step three only because the abort did not end the turn.
  r.tick(2 * MINUTE);
  await r.watchdog.sweep();
  expect(r.steps.map((step) => step.step)).toEqual(["end-processes", "abort-turn", "exit-process"]);
  expect(r.exitsWritten).toEqual([1]);
  expect(r.exitCodes).toEqual([STALL_EXIT_CODE]);

  // The ladder is over: a fourth sweep runs nothing.
  r.tick(2 * MINUTE);
  await r.watchdog.sweep();
  expect(r.steps).toHaveLength(3);
  expect(r.exitCodes).toHaveLength(1);
});

test("an abort whose Promise never settles hands back at the step gap, and the ladder goes on", async () => {
  // The host's `abort()` resolves only once the call it aborts has ended, so on the hang this
  // ladder exists for it may never resolve. The bound on that wait is a real host timer and the
  // module takes no timer seam — it is a pure detector with one injected clock, and the clock it
  // judges by is deliberately not the one it waits on — so this one number is wall-clock: a
  // second, chosen short because the case pays it once. Every other number here is the fake
  // clock's.
  const gap = SECOND;
  const r = rig({ limits: { stepMs: gap } });
  r.abortHangs = true;
  running(r.watchdog, "bash", { command: "sleep 100000", pty: true });
  r.tick(10 * MINUTE + SECOND);
  await r.watchdog.sweep();
  expect(r.steps.map((step) => step.step)).toEqual(["end-processes"]);

  // Step two: the abort is made and answers neither way. The step waits the ladder's own gap and
  // hands the sweep back, rather than holding it for as long as the turn stays hung.
  r.tick(2 * MINUTE);
  const began = Date.now();
  await r.watchdog.sweep();
  const waited = Date.now() - began;
  expect(r.aborts).toHaveLength(1);
  // It waited, and it stopped waiting: the lower bound is half the gap rather than the gap
  // itself, because a host timer may fire a millisecond early, and the upper bound is what
  // fails on an unbounded wait — there the sweep never returns and this case times out.
  expect(waited).toBeGreaterThan(gap / 2);
  expect(waited).toBeLessThan(10 * gap);
  expect(r.steps[1]?.outcome).toContain(
    "the host's abort() had not returned within the 1-second step gap"
  );
  // The message that starts the work again still went out, so a turn the abort does end later
  // has something waiting to restart it.
  expect(r.messages).toHaveLength(1);

  // Step three at the next gap, exactly as the ladder says, because the abort did not end the
  // turn.
  r.tick(2 * MINUTE);
  await r.watchdog.sweep();
  expect(r.steps.map((step) => step.step)).toEqual(["end-processes", "abort-turn", "exit-process"]);
  expect(r.exitCodes).toEqual([STALL_EXIT_CODE]);

  // A late settle is not a second step: the Promise answering now records nothing and ends
  // nothing, because nothing is reading it any more.
  r.settleAbort();
  // The sweep below is itself an await, so the resolution above has run by the time it judges.
  r.tick(2 * MINUTE);
  await r.watchdog.sweep();
  expect(r.steps).toHaveLength(3);
  expect(r.exitCodes).toHaveLength(1);
});

test("a turn the abort ended never reaches the exit", async () => {
  const r = await stuckShellCall();
  r.tick(2 * MINUTE);
  await r.watchdog.sweep();
  expect(r.aborts).toHaveLength(1);

  // The host ends the turn: the aborted assistant message and `turn_end` close the record.
  r.watchdog.assistantMessage({ role: "assistant", stopReason: "aborted" });
  r.watchdog.turnEnded();

  r.tick(10 * HOUR);
  await r.watchdog.sweep();
  expect(r.exitCodes).toEqual([]);
  expect(r.steps).toHaveLength(2);
});

test("a call that ends after step one never reaches the abort", async () => {
  const r = await stuckShellCall();
  r.watchdog.executionEnded({ toolCallId: "toolu_1" });
  r.tick(2 * MINUTE);
  await r.watchdog.sweep();
  expect(r.aborts).toEqual([]);
  expect(r.steps.map((step) => step.step)).toEqual(["end-processes"]);
});

test("the ladder's record is cleared when the call closes, so a later call starts at step one", async () => {
  const r = await stuckShellCall();
  r.tick(2 * MINUTE);
  await r.watchdog.sweep();
  expect(r.steps.map((step) => step.step)).toEqual(["end-processes", "abort-turn"]);

  // The sweep finds the call closed, which clears its ladder.
  r.watchdog.executionEnded({ toolCallId: "toolu_1" });
  await r.watchdog.sweep();
  expect(r.watchdog.openCalls()).toEqual([]);

  // A later stuck call starts again at the first step, and its owner is told again.
  running(r.watchdog, "bash", { command: "sleep 100000", pty: true }, "toolu_2");
  r.tick(10 * MINUTE + SECOND);
  await r.watchdog.sweep();
  expect(r.told).toHaveLength(2);
  expect(r.steps.map((step) => step.step)).toEqual([
    "end-processes",
    "abort-turn",
    "end-processes",
  ]);
  expect(r.steps[2]?.call.toolCallId).toBe("toolu_2");
});

test("no step runs while the host reports the session idle, or for a turn that is over", async () => {
  const r = await (async () => {
    const idle = rig();
    idle.idle = true;
    running(idle.watchdog, "bash", { command: "sleep 100000", pty: true });
    idle.tick(10 * MINUTE + SECOND);
    await idle.watchdog.sweep();
    return idle;
  })();
  expect(r.told).toEqual([]);
  expect(r.steps).toEqual([]);

  // The same call, once the host says a turn runs again.
  r.idle = false;
  await r.watchdog.sweep();
  expect(r.steps.map((step) => step.step)).toEqual(["end-processes"]);
});

test("a step that fails is recorded and the ladder goes on", async () => {
  const r = await (async () => {
    const broken = rig();
    broken.endCallAnswer = "no /proc on this machine";
    broken.abortThrows = "the host refused the abort";
    broken.sendThrows = "the host refused the message";
    running(broken.watchdog, "bash", { command: "sleep 100000", pty: true });
    broken.tick(10 * MINUTE + SECOND);
    await broken.watchdog.sweep();
    return broken;
  })();
  expect(r.steps[0]?.outcome).toContain("no /proc on this machine");

  r.tick(2 * MINUTE);
  await r.watchdog.sweep();
  expect(r.steps[1]?.outcome).toContain("the host refused the abort");
  expect(r.steps[1]?.outcome).toContain("the host refused the message");

  // Neither failure left the ladder with nothing to run: the exit still comes.
  r.tick(2 * MINUTE);
  await r.watchdog.sweep();
  expect(r.exitCodes).toEqual([STALL_EXIT_CODE]);
});

test("a process tree that cannot be read is told as unread, and the ladder goes on", async () => {
  for (const reader of ["throws", "answers unread"] as const) {
    const r = rig();
    r.treeAnswer =
      reader === "throws"
        ? "/proc is not mounted"
        : { read: false, reason: "/proc is not mounted" };
    running(r.watchdog, "bash", { command: "sleep 100000", pty: true });
    r.tick(10 * MINUTE + SECOND);
    await r.watchdog.sweep();

    expect(r.told[0]?.tree, reader).toEqual({ read: false, reason: "/proc is not mounted" });
    expect(
      r.steps.map((step) => step.step),
      reader
    ).toEqual(["end-processes"]);
  }
});

test("an owner the publish never reached does not stop the ladder", async () => {
  const r = rig();
  r.tellAnswer = { evidenceWritten: true, ownerTold: false };
  running(r.watchdog, "bash", { command: "sleep 100000", pty: true });
  r.tick(10 * MINUTE + SECOND);
  await r.watchdog.sweep();
  r.tick(2 * MINUTE);
  await r.watchdog.sweep();
  r.tick(2 * MINUTE);
  await r.watchdog.sweep();
  expect(r.exitCodes).toEqual([STALL_EXIT_CODE]);
});

test("a run whose evidence was not written does not reach the exit", async () => {
  const r = rig();
  r.tellAnswer = { evidenceWritten: false, ownerTold: true };
  running(r.watchdog, "bash", { command: "sleep 100000", pty: true });
  r.tick(10 * MINUTE + SECOND);
  await r.watchdog.sweep();
  r.tick(2 * MINUTE);
  await r.watchdog.sweep();
  r.tick(2 * MINUTE);
  await r.watchdog.sweep();
  expect(r.exitCodes).toEqual([]);
  expect(r.exitsWritten).toEqual([]);
  expect(r.stopped[0]?.reason).toContain("the evidence for this call was not written");
});

test("a worker at its exit cap ends nothing further and tells its owner", async () => {
  const r = rig();
  r.exitsRead = STALL_EXIT_CAP;
  running(r.watchdog, "bash", { command: "sleep 100000", pty: true });
  r.tick(10 * MINUTE + SECOND);
  await r.watchdog.sweep();
  r.tick(2 * MINUTE);
  await r.watchdog.sweep();
  r.tick(2 * MINUTE);
  await r.watchdog.sweep();
  expect(r.exitCodes).toEqual([]);
  expect(r.exitsWritten).toEqual([]);
  expect(r.stopped).toHaveLength(1);
  expect(r.stopped[0]?.reason).toContain(`cap of ${STALL_EXIT_CAP}`);
});

test("a counter file that cannot be read or written ends no process and tells the owner", async () => {
  for (const broken of ["unreadable", "unwritable"] as const) {
    const r = rig();
    if (broken === "unreadable") r.exitsRead = undefined;
    else r.exitsWritable = false;
    running(r.watchdog, "bash", { command: "sleep 100000", pty: true });
    r.tick(10 * MINUTE + SECOND);
    await r.watchdog.sweep();
    r.tick(2 * MINUTE);
    await r.watchdog.sweep();
    r.tick(2 * MINUTE);
    await r.watchdog.sweep();
    expect(r.exitCodes, broken).toEqual([]);
    expect(r.stopped[0]?.reason, broken).toContain(
      broken === "unreadable" ? "could not be read" : "could not be written"
    );
  }
});

// ---------------------------------------------------------------------------------------------
// R7: the module's own documentation states its invariant.
// ---------------------------------------------------------------------------------------------

test("the module's documentation states what a missed beat is and is not", () => {
  const source = readFileSync(path.resolve(import.meta.dir, "stall-watchdog.ts"), "utf8");
  const header = source.slice(0, source.indexOf("\n */") + 4);

  expect(header).toContain("**The invariant.**");
  expect(header).toContain("A missed beat is one tool call that has outlived the deadline its own");
  expect(header).toContain("**What a missed beat is not.**");
  for (const notThis of ["a long turn", "a slow model", "pauses", "background job"]) {
    expect(header, notThis).toContain(notThis);
  }

  // The three measurements a reader would otherwise spend an hour on, each with its date and the
  // binary it was taken against.
  expect(header).toContain("2026-11-07");
  expect(header).toContain("/opt/omp/bin/omp");
  expect(header).toContain("omp/18.16.0");
  expect(header).toContain("--mode rpc");
  for (const measured of [
    "60.0 seconds",
    "pty: true",
    "/proc/self/environ",
    "tool_execution_start",
    "launch_failure_limit",
  ]) {
    expect(header, measured).toContain(measured);
  }
});

test("the module imports no Oh My Pi module and no node: module", () => {
  const source = readFileSync(path.resolve(import.meta.dir, "stall-watchdog.ts"), "utf8");
  const statements = [...source.matchAll(/^import[^"']*["']([^"']+)["']/gm)].map(
    ([statement, specifier]) => ({ specifier, typeOnly: statement.startsWith("import type ") })
  );
  // Three imports: two of this package's own leaf modules, type-only, and the error coercion
  // `@legion/envoy-client/errors`, which `src/phase-reminder.ts` imports the same way and which
  // itself imports nothing at all. Nothing of this module reaches a host module or a Node
  // builtin, so the bundle `packages/claude-envoy` builds with no node_modules keeps resolving
  // (`src/envoy-notice.ts`).
  expect(statements).toEqual([
    { specifier: "@legion/envoy-client/errors", typeOnly: false },
    { specifier: "./pi-types", typeOnly: true },
    { specifier: "./stall-limits", typeOnly: true },
  ]);
});
