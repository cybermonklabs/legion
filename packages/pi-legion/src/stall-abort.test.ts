import { expect, test } from "bun:test";
import type { LegionTake } from "@legion/contracts/legion-api";
import type { PiApi, SessionContext } from "@legion/pi-shared/pi-types";
import { DEFAULT_STALL_LIMITS, defaultCallBoundMs } from "@legion/pi-shared/stall-limits";
import {
  createStallWatchdog,
  type StallCall,
  type StallStepRecord,
  type StallWatchdog,
} from "@legion/pi-shared/stall-watchdog";
import { createTaskTake } from "@legion/pi-shared/task-take";
// The import below is this file's merge-base check: on the merge base there is no `./stall-abort`,
// so every test here fails to resolve rather than passing vacuously.
import { createStallAborter, STALL_RESTART_MESSAGE } from "./stall-abort";

const SECOND = 1_000;
const MINUTE = 60 * SECOND;
const START = 1_793_000_000_000;

type SentMessage = {
  readonly message: Parameters<PiApi["sendMessage"]>[0];
  readonly options: Parameters<PiApi["sendMessage"]>[1];
};

/** What every aborter here is built with unless its case is about a pending take: no take is
 * pending, so nothing is held for one, and the log line a held message writes goes nowhere. */
const NOTHING_PENDING = {
  deferUntilTakeSettles: () => undefined,
  log: { info: () => {} },
} as const;

/**
 * The handler context as the host hands it over: `abort` answers `typeof` but is not an own
 * enumerable property — `Object.keys(ctx)` lists `ui` alone (measured 2026-11-07, `omp/18.16.0`
 * in `--mode rpc`) — so it is put on the prototype here, which is the shape a key list misses.
 */
function hostContext(abort?: (this: SessionContext) => void | Promise<void>): SessionContext {
  const prototype = abort === undefined ? {} : { abort };
  return Object.assign(Object.create(prototype), {
    cwd: "/workspace",
    ui: { notify: () => {} },
  }) as SessionContext;
}

test("the turn is ended with the host's own abort(), called on the context it came from", async () => {
  let abortedOn: unknown;
  // The host's `abort()` returns a Promise (measured), so the step is over only when that
  // Promise is: this one is held open until the case lets it settle.
  const host = Promise.withResolvers<void>();
  const context = hostContext(function abort(this: SessionContext) {
    abortedOn = this;
    return host.promise;
  });
  // The member the module reads is not one a key list holds, which is why it is asked for by
  // `typeof`.
  expect(Object.keys(context)).not.toContain("abort");

  const aborter = createStallAborter({
    ...NOTHING_PENDING,
    context: () => context,
    sendMessage: () => {},
  });
  // `abortTurn` may answer either way, and the detector awaits whatever it answers.
  const aborting = Promise.resolve(aborter.abortTurn());
  expect(abortedOn).toBe(context);
  expect(await Promise.race([aborting.then(() => "ended"), Promise.resolve("still ending")])).toBe(
    "still ending"
  );

  host.resolve();
  await aborting;
});

test("a host with no abort(), and a session with no context, throw so the ladder goes on", async () => {
  const hostless = createStallAborter({
    ...NOTHING_PENDING,
    context: () => hostContext(),
    sendMessage: () => {},
  });
  expect(hostless.abortTurn()).rejects.toThrow("this host has no ctx.abort()");

  const contextless = createStallAborter({
    ...NOTHING_PENDING,
    context: () => undefined,
    sendMessage: () => {},
  });
  expect(contextless.abortTurn()).rejects.toThrow("this session has no handler context");
});

test("the message the worker sends itself is steered, starts a turn and is no user message", () => {
  const sent: SentMessage[] = [];
  const aborter = createStallAborter({
    ...NOTHING_PENDING,
    context: () => hostContext(),
    sendMessage: (message, options) => {
      sent.push({ message, options });
    },
  });

  aborter.sendMessage("Your turn was ended because its `bash` call (toolu_stuck) had no result.");

  expect(sent).toEqual([
    {
      message: {
        // Its own custom type: a `role: "user"` message is what the phase check reads as a new
        // assignment, so a worker restarting itself would reset that check's own count.
        customType: STALL_RESTART_MESSAGE,
        content: "Your turn was ended because its `bash` call (toolu_stuck) had no result.",
        display: true,
      },
      // Steered, and triggering a turn: the session is idle once the abort has ended the turn,
      // and the daemon sends nothing after a turn it counts as served.
      options: { deliverAs: "steer", triggerTurn: true },
    },
  ]);
});

test("a message the host refuses throws, so the step records it", () => {
  const aborter = createStallAborter({
    ...NOTHING_PENDING,
    context: () => hostContext(),
    sendMessage: () => {
      throw new Error("this session is shutting down");
    },
  });
  expect(() => aborter.sendMessage("anything")).toThrow("this session is shutting down");
});

// ---------------------------------------------------------------------------------------------
// Driven by the detector itself: the second step of the ladder, against a faked host.
// ---------------------------------------------------------------------------------------------

interface LadderRig {
  readonly watchdog: StallWatchdog;
  readonly tick: (ms: number) => void;
  readonly steps: StallStepRecord[];
  readonly stopped: { readonly call: StallCall; readonly reason: string }[];
  readonly ended: string[];
  readonly aborts: number[];
  readonly sent: SentMessage[];
  readonly exitCodes: number[];
  readonly exitsWritten: number[];
  /** Every line the aborter logged, in order, with the fields it carried. */
  readonly logged: { readonly message: string; readonly fields?: Record<string, unknown> }[];
}

/**
 * The detector with the real aborter over a faked host. `abortsTurn` is what this host's
 * `abort()` does: the measured one ends the turn, which the plugin's entry sees as an assistant
 * message that ended aborted and a `turn_end`; a host that cannot abort has no `abort` at all.
 * `deferUntilTakeSettles` is the claim's take as the aborter reads it
 * (`TaskTake.deferUntilSettled`); nothing is pending unless a case says so.
 */
function ladder(options: {
  abortsTurn: boolean;
  exitsRead?: number;
  deferUntilTakeSettles?: (callback: () => void) => number | undefined;
}): LadderRig {
  let clock = START;
  const steps: StallStepRecord[] = [];
  const stopped: { call: StallCall; reason: string }[] = [];
  const ended: string[] = [];
  const aborts: number[] = [];
  const sent: SentMessage[] = [];
  const exitCodes: number[] = [];
  const exitsWritten: number[] = [];
  const logged: { message: string; fields?: Record<string, unknown> }[] = [];
  let exits = options.exitsRead ?? 0;
  let watchdog: StallWatchdog | undefined;

  const context = options.abortsTurn
    ? hostContext(async () => {
        aborts.push(clock);
        // What the host does with the turn, as it was measured: the call's result comes back in
        // the same millisecond, the turn ends, and the abortedness is on the last assistant
        // message (`agent_end` carries no `stopReason`).
        watchdog?.assistantMessage({
          role: "assistant",
          stopReason: "aborted",
          errorMessage: "Interrupted by user",
        });
        watchdog?.turnEnded();
      })
    : hostContext();

  watchdog = createStallWatchdog({
    now: () => clock,
    limits: { ...DEFAULT_STALL_LIMITS, callBoundMs: defaultCallBoundMs("tester") },
    isIdle: () => false,
    processes: {
      tree: () => ({ read: true, processes: [] }),
      endCall: (call) => {
        ended.push(call.toolCallId);
        return 0;
      },
    },
    teller: {
      tellOverdue: () => ({ evidenceWritten: true, ownerTold: true }),
      recordStep: (record) => {
        steps.push(record);
      },
      tellStopped: (call, reason) => {
        stopped.push({ call, reason });
      },
    },
    aborter: createStallAborter({
      context: () => context,
      sendMessage: (message, messageOptions) => {
        sent.push({ message, options: messageOptions });
      },
      deferUntilTakeSettles: options.deferUntilTakeSettles ?? (() => undefined),
      log: {
        info: (message, fields) => {
          logged.push({ message, fields });
        },
      },
    }),
    exits: {
      read: () => exits,
      write: (count) => {
        exitsWritten.push(count);
        exits = count;
        return true;
      },
    },
    exitProcess: (code) => {
      exitCodes.push(code);
    },
    log: { debug: () => {}, warn: () => {} },
  });

  return {
    watchdog,
    tick: (ms) => {
      clock += ms;
    },
    steps,
    stopped,
    ended,
    aborts,
    sent,
    exitCodes,
    exitsWritten,
    logged,
  };
}

/** One call made and handed to its tool: the shape this issue is about, a PTY shell call that
 * writes nothing after its `tool_execution_start` (measured twice). */
function running(watchdog: StallWatchdog, toolCallId = "toolu_stuck"): void {
  const input = { command: "sleep 100000", pty: true };
  watchdog.callRequested({ toolName: "bash", toolCallId, input });
  watchdog.executionStarted({ toolName: "bash", toolCallId, input });
}

test("the second step ends the turn and sends the session a message naming the ended call", async () => {
  const rig = ladder({ abortsTurn: true });
  running(rig.watchdog);

  // The first step, which does not end the call.
  rig.tick(10 * MINUTE + SECOND);
  await rig.watchdog.sweep();
  expect(rig.ended).toEqual(["toolu_stuck"]);
  expect(rig.aborts).toEqual([]);

  // The second, a step gap later, because the first did not end it.
  rig.tick(2 * MINUTE);
  await rig.watchdog.sweep();
  expect(rig.aborts).toHaveLength(1);
  expect(rig.steps.map((step) => step.step)).toEqual(["end-processes", "abort-turn"]);
  expect(rig.steps[1]?.outcome).toContain("the turn was ended with the host's abort()");
  expect(rig.steps[1]?.outcome).toContain("the session was sent the message");

  // The message names the call that was ended, and the tool it was a call of.
  expect(rig.sent).toHaveLength(1);
  const message = rig.sent[0]?.message as { customType: string; content: string };
  expect(message.customType).toBe(STALL_RESTART_MESSAGE);
  expect(message.content).toContain("toolu_stuck");
  expect(message.content).toContain("`bash`");
  expect(rig.sent[0]?.options).toEqual({ deliverAs: "steer", triggerTurn: true });

  // A turn the abort ended never reaches the exit: the call's record went with the turn.
  expect(rig.watchdog.openCalls()).toEqual([]);
  rig.tick(2 * MINUTE);
  await rig.watchdog.sweep();
  expect(rig.exitCodes).toEqual([]);
  expect(rig.exitsWritten).toEqual([]);
  expect(rig.steps.map((step) => step.step)).toEqual(["end-processes", "abort-turn"]);
});

// dispatch://INFRA-4130 R6, row S13: the abort ends the turn, and that run's `agent_end` asks the
// take of the task the claim is owed, whose text is then the next run. A restart message sent as
// well would start a turn of its own ahead of the task's. With no take pending the step is as it
// was: the message is what starts the work again.
test("a stall abort while a take is pending sends no restart message", async () => {
  let pending = false;
  const withheld = ladder({
    abortsTurn: true,
    deferUntilTakeSettles: () => (pending ? 4 : undefined),
  });
  running(withheld.watchdog);
  withheld.tick(10 * MINUTE + SECOND);
  await withheld.watchdog.sweep();
  // The take is read when the message would go, after the abort: it turns pending here.
  pending = true;
  withheld.tick(2 * MINUTE);
  await withheld.watchdog.sweep();

  expect(withheld.aborts).toHaveLength(1);
  expect(withheld.steps.map((step) => step.step)).toEqual(["end-processes", "abort-turn"]);
  expect(withheld.sent).toEqual([]);
  expect(withheld.steps[1]?.outcome).toContain("the message naming the ended call was not sent");
  expect(withheld.steps[1]?.outcome).not.toContain("the session was sent the message");
  expect(withheld.logged).toEqual([
    {
      message: expect.stringContaining("a take of the claim's task is pending"),
      fields: { task: 4 },
    },
  ]);
  // The abort ended the turn, so the ladder takes no step after it.
  withheld.tick(2 * MINUTE);
  await withheld.watchdog.sweep();
  expect(withheld.exitCodes).toEqual([]);

  const sent = ladder({ abortsTurn: true, deferUntilTakeSettles: () => undefined });
  running(sent.watchdog);
  sent.tick(10 * MINUTE + SECOND);
  await sent.watchdog.sweep();
  sent.tick(2 * MINUTE);
  await sent.watchdog.sweep();

  expect(sent.aborts).toHaveLength(1);
  expect(sent.sent.map((message) => message.options)).toEqual([
    { deliverAs: "steer", triggerTurn: true },
  ]);
  expect(sent.logged).toEqual([]);
  expect(sent.steps[1]?.outcome).toContain("the session was sent the message");
});

/** Lets every promise queued so far settle. */
async function flush(): Promise<void> {
  for (let turn = 0; turn < 50; turn += 1) await Promise.resolve();
}

/**
 * The claim's take of its task (`@legion/pi-shared/task-take`), ready, over a daemon that answers
 * each take only when a case answers it (`answer`, the take asked last). No attempt's bound passes,
 * so a take nobody answered stays in flight.
 */
function claimTake() {
  const answers: ((answer: LegionTake) => void)[] = [];
  const take = createTaskTake({
    caller: {
      claimToken: "legion-legion-legion-209-tester",
      sessionId: "ses_tester_209",
      secret: "U2VjcmV0",
      generation: 3,
    },
    daemon: {
      taskSeen: async () => undefined,
      take: () => {
        const { promise, resolve } = Promise.withResolvers<LegionTake>();
        answers.push(resolve);
        return promise;
      },
    },
    run: () => {},
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
    /** How many takes the daemon was asked. */
    asked: () => answers.length,
    /** The daemon's write of task `number` at `revision`, relayed to the session. */
    relay: async (number: number, revision: number) => {
      take.relayed({
        claim: "legion-legion-legion-209-tester",
        epoch: "2026-11-13T16:21:44.123456789Z",
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

// dispatch://INFRA-4130, risk (b): the restart a pending take holds back is not lost. The run the
// abort ended asks the take at its end; a take the daemon refuses starts no run, and the aborted
// run's `session_stop` arms nothing, so the held message is what starts the work again: it goes
// once the daemon has refused that take, the same message the step sends where nothing is pending.
test("a stall abort's restart held for a pending take is sent once that take is refused", async () => {
  const claim = claimTake();
  // The run that stalls; a value arrives during it, so the take is pending and not asked yet.
  claim.take.runStarted();
  await claim.relay(4, 12);
  const held = ladder({
    abortsTurn: true,
    deferUntilTakeSettles: (callback) => claim.take.deferUntilSettled(callback),
  });
  running(held.watchdog);
  held.tick(10 * MINUTE + SECOND);
  await held.watchdog.sweep();
  held.tick(2 * MINUTE);
  await held.watchdog.sweep();

  expect(held.aborts).toHaveLength(1);
  expect(held.sent).toEqual([]);
  // The line names the task the take waits for: task 4, whose value came during the run.
  expect(held.logged).toEqual([
    { message: expect.stringContaining("waits for the take"), fields: { task: 4 } },
  ]);
  expect(held.steps[1]?.outcome).toContain(
    "is sent if that take is refused by another hold or dropped"
  );

  // The aborted run ends and asks the take: nothing goes while the daemon has not answered it.
  expect(claim.take.runEnded(false)).toBeUndefined();
  await flush();
  expect(claim.asked()).toBe(1);
  expect(held.sent).toEqual([]);

  // The daemon refuses the take for another claim's turn: the message goes, the one the step
  // sends at once where no take is pending.
  await claim.answer({ outcome: "refused", hold: "issue-in-turn" });
  const atOnce = ladder({ abortsTurn: true });
  running(atOnce.watchdog);
  atOnce.tick(10 * MINUTE + SECOND);
  await atOnce.watchdog.sweep();
  atOnce.tick(2 * MINUTE);
  await atOnce.watchdog.sweep();
  expect(atOnce.sent).toHaveLength(1);
  expect(held.sent).toEqual(atOnce.sent);

  // Once: a later take refused again sends nothing more.
  await claim.relay(4, 13);
  await claim.answer({ outcome: "refused", hold: "stale-plugin" });
  expect(claim.asked()).toBe(2);
  expect(held.sent).toHaveLength(1);
});

test("a call that ends after the first step never reaches the abort", async () => {
  const rig = ladder({ abortsTurn: true });
  running(rig.watchdog);

  rig.tick(10 * MINUTE + SECOND);
  await rig.watchdog.sweep();
  expect(rig.ended).toEqual(["toolu_stuck"]);

  // The first step ended the call's own process, so the host closes the call.
  rig.watchdog.executionEnded({ toolCallId: "toolu_stuck" });

  rig.tick(2 * MINUTE);
  await rig.watchdog.sweep();
  rig.tick(2 * MINUTE);
  await rig.watchdog.sweep();
  expect(rig.aborts).toEqual([]);
  expect(rig.sent).toEqual([]);
  expect(rig.exitCodes).toEqual([]);
  expect(rig.steps.map((step) => step.step)).toEqual(["end-processes"]);
});

test("an abort the host cannot make is recorded, and the ladder goes on to the last step", async () => {
  const rig = ladder({ abortsTurn: false });
  running(rig.watchdog);

  rig.tick(10 * MINUTE + SECOND);
  await rig.watchdog.sweep();
  rig.tick(2 * MINUTE);
  await rig.watchdog.sweep();

  // The abort failed, and the message that would have restarted the work still went out, so the
  // turn is not left ended with nothing to restart it.
  expect(rig.steps[1]?.outcome).toContain(
    "the host's abort() failed: this host has no ctx.abort()"
  );
  expect(rig.sent).toHaveLength(1);
  expect(rig.aborts).toEqual([]);

  // And the ladder has a step left: the call is still open, so the next gap reaches the exit.
  rig.tick(2 * MINUTE);
  await rig.watchdog.sweep();
  expect(rig.steps.map((step) => step.step)).toEqual([
    "end-processes",
    "abort-turn",
    "exit-process",
  ]);
  expect(rig.exitsWritten).toEqual([1]);
  expect(rig.exitCodes).toEqual([1]);
  expect(rig.stopped).toEqual([]);
});
