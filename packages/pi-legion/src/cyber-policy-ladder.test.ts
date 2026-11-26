import { describe, expect, test } from "bun:test";
import type {
  CompactionPreparation,
  HostModel,
  SessionBeforeCompactEvent,
  SessionContext,
} from "@legion/pi-shared/pi-types";
import {
  CYBER_POLICY_CONTINUE,
  CYBER_POLICY_RETRY_ENTRY,
  type CyberPolicyLadderSettings,
  createCyberPolicyLadder,
  cyberPolicyLadderSettings,
  DEFAULT_CYBER_POLICY_MESSAGE,
  LONGEST_RUNG_WAIT_MS,
  summarizeThrough,
} from "./cyber-policy-ladder";

/**
 * The ladder against fake deps: no Oh My Pi. The host's classifiers are the two predicates the
 * fixture writes, read from the refused message's own text as the host reads it: a refusal names
 * `cyber_policy`, and a call the host's hold held starts with the hold's own words
 * (`cyberPolicyHoldMessage`, `session/cyber-policy-closure.ts:13-19` at the pinned tag).
 */

const HOLD_PREFIX = "OpenAI refused content of this session (cyber_policy); no request goes to ";

const refused = (errorMessage = "cyber_policy: flagged for possible cybersecurity risk") => ({
  role: "assistant",
  stopReason: "error",
  errorMessage,
  content: [],
});
const held = () => refused(`${HOLD_PREFIX}standin/gpt-5.5 or another OpenAI route.`);
const otherError = () => refused("529 Overloaded");

const SETTINGS: CyberPolicyLadderSettings = {
  waitsMs: { retry: 15_000, message: 25_000, compact: 25_000, "weaker-model": 25_000 },
  message: DEFAULT_CYBER_POLICY_MESSAGE,
};

const WEAKER: HostModel = { provider: "openai", id: "gpt-5.4-mini", identity: { class: "openai" } };

type Ctx = Pick<SessionContext, "compact">;

/** An error of the name Oh My Pi's own rejection of a compaction carries. */
function named(name: string, message: string): Error {
  const error = new Error(message);
  error.name = name;
  return error;
}

/** What a case does inside the ladder's collaborators, set after the rig is made. */
interface Hooks {
  /** Runs inside the host's classifier of a refusal, before it answers. */
  onRefusalCheck?: () => void;
  /** Runs inside `wait`, before it answers. */
  onWait?: () => void;
  /** Runs inside `stepDownTo` once it has begun the switch, before it answers. */
  onStep?: () => void;
  /** What `stepDownTo` answers; true by default. */
  stepAnswer?: boolean;
}

function rig(options: { weaker?: HostModel; compact?: "ok" | "fails" | "absent" } = {}) {
  const calls: string[] = [];
  const waits: number[] = [];
  const warnings: string[] = [];
  const hooks: Hooks = {};
  /** The handlers the ladder registered on `session_before_compact`. */
  const beforeCompact: Array<(event: SessionBeforeCompactEvent) => void> = [];
  /** The settle budgets the ladder asked for, each a controller the case can abort. */
  const budgets: Array<{ readonly ms: number; readonly controller: AbortController }> = [];
  let failCompaction: ((error: Error) => void) | undefined;
  let finishCompaction: (() => void) | undefined;
  const ctx: Ctx =
    options.compact === "absent"
      ? {}
      : {
          compact: (compactOptions) => {
            calls.push(`compact ${compactOptions.origin} ${compactOptions.mode}`);
            const { promise, resolve, reject } = Promise.withResolvers<void>();
            finishCompaction = resolve;
            failCompaction = reject;
            return promise;
          },
        };
  const ladder = createCyberPolicyLadder<Ctx>({
    settings: SETTINGS,
    cyberPolicyRefusal: async (message) => {
      hooks.onRefusalCheck?.();
      return (
        typeof message === "object" &&
        message !== null &&
        "errorMessage" in message &&
        String(message.errorMessage).includes("cyber_policy")
      );
    },
    recordableRefusal: async (message) =>
      typeof message === "object" &&
      message !== null &&
      "errorMessage" in message &&
      String(message.errorMessage).includes("cyber_policy") &&
      !String(message.errorMessage).startsWith(HOLD_PREFIX),
    sendMessage: (message, options) => {
      expect(message).toMatchObject({ customType: CYBER_POLICY_RETRY_ENTRY, display: false });
      calls.push(`${options?.origin ?? "inside"} ${options?.triggerTurn}`);
    },
    sameFamilyFallback: async () => options.weaker,
    stepDownTo: async (_ctx, model, signal) => {
      // As `ModelFallback.stepDownTo` does: an aborted signal moves nothing.
      if (signal.aborted) return false;
      calls.push(`step ${model.provider}/${model.id}`);
      hooks.onStep?.();
      return hooks.stepAnswer ?? true;
    },
    wait: async (ms, signal) => {
      waits.push(ms);
      hooks.onWait?.();
      return signal?.aborted !== true;
    },
    timeout: (ms) => {
      const controller = new AbortController();
      budgets.push({ ms, controller });
      return controller.signal;
    },
    registerBeforeCompact: (handler) => void beforeCompact.push(handler),
    log: { debug: () => undefined, warn: (message) => void warnings.push(message) },
  });
  const signal = new AbortController().signal;
  return {
    ladder,
    ctx,
    calls,
    waits,
    warnings,
    hooks,
    budgets,
    beforeCompact,
    stop: (message: unknown, stopSignal: AbortSignal = signal) =>
      ladder.settle({ last_assistant_message: message, signal: stopSignal }, ctx),
    failCompaction: (error: Error) => failCompaction?.(error),
    finishCompaction: () => finishCompaction?.(),
  };
}

/** Lets the promise callbacks a settled compaction queued run. */
const settle = () => new Promise<void>((resolve) => setImmediate(resolve));

describe("the ladder's settings", () => {
  test("default to 15 s, then 25 s for each later rung, and the default message", () => {
    expect(cyberPolicyLadderSettings({}, { warn: () => undefined })).toEqual(SETTINGS);
  });

  test("are read from the worker's environment", () => {
    const warnings: string[] = [];
    expect(
      cyberPolicyLadderSettings(
        {
          LEGION_CYBER_POLICY_RETRY_WAIT_MS: "0",
          LEGION_CYBER_POLICY_MESSAGE_WAIT_MS: "100",
          LEGION_CYBER_POLICY_COMPACT_WAIT_MS: "200",
          LEGION_CYBER_POLICY_FALLBACK_WAIT_MS: "300",
          LEGION_CYBER_POLICY_RETRY_MESSAGE: "Describe it instead.",
        },
        { warn: (message) => void warnings.push(message) }
      )
    ).toEqual({
      waitsMs: { retry: 0, message: 100, compact: 200, "weaker-model": 300 },
      message: "Describe it instead.",
    });
    expect(warnings).toEqual([]);
  });

  test("cut a wait above the handler's room to the longest, and refuse what is no wait", () => {
    const warnings: string[] = [];
    const settings = cyberPolicyLadderSettings(
      {
        LEGION_CYBER_POLICY_RETRY_WAIT_MS: "60000",
        LEGION_CYBER_POLICY_MESSAGE_WAIT_MS: "soon",
        LEGION_CYBER_POLICY_RETRY_MESSAGE: "  ",
      },
      { warn: (message) => void warnings.push(message) }
    );
    expect(settings.waitsMs.retry).toBe(LONGEST_RUNG_WAIT_MS);
    expect(settings.waitsMs.message).toBe(25_000);
    expect(settings.message).toBe(DEFAULT_CYBER_POLICY_MESSAGE);
    expect(warnings).toEqual([
      "legion: LEGION_CYBER_POLICY_RETRY_WAIT_MS is above 25000, the longest wait Oh My Pi's 30 s bound on a session_stop handler leaves room for; using 25000",
      "legion: LEGION_CYBER_POLICY_MESSAGE_WAIT_MS is not an integer of 0 or more; using 25000",
      "legion: LEGION_CYBER_POLICY_RETRY_MESSAGE is empty; using the default message",
    ]);
  });
});

describe("one pass of the ladder", () => {
  test("climbs retry, message, compact and the weaker model, each after its wait, then settles", async () => {
    const pane = rig({ weaker: WEAKER });

    expect(await pane.stop(refused())).toEqual({
      continue: true,
      additionalContext: CYBER_POLICY_CONTINUE,
    });
    expect(await pane.stop(refused())).toEqual({
      continue: true,
      additionalContext: DEFAULT_CYBER_POLICY_MESSAGE,
    });
    expect(await pane.stop(refused())).toEqual({
      continue: true,
      additionalContext: CYBER_POLICY_CONTINUE,
    });
    // The compacted rung's request starts the compaction, from outside the session.
    await pane.ladder.request([], pane.ctx);
    pane.finishCompaction();
    expect(await pane.stop(refused())).toEqual({
      continue: true,
      additionalContext: CYBER_POLICY_CONTINUE,
    });
    // The last rung refused too: the turn settles and the refusal goes to the daemon.
    expect(await pane.stop(refused())).toBeUndefined();

    expect(pane.waits).toEqual([15_000, 25_000, 25_000, 25_000]);
    expect(pane.calls).toEqual([
      "outside false",
      "outside false",
      "outside false",
      "compact outside soft",
      "step openai/gpt-5.4-mini",
      "outside false",
    ]);
  });

  test("a call the hold held keeps the pass climbing, but never starts one", async () => {
    const pane = rig();
    expect(await pane.stop(held())).toBeUndefined();
    expect(pane.calls).toEqual([]);

    expect(await pane.stop(refused())).toMatchObject({ continue: true });
    // The retry's own send was held: the pass goes on to its next rung.
    expect(await pane.stop(held())).toMatchObject({
      additionalContext: DEFAULT_CYBER_POLICY_MESSAGE,
    });
  });

  test("a spent pass leaves every later refusal to settle until the provider answers", async () => {
    const pane = rig();
    for (let rung = 0; rung < 3; rung += 1) await pane.stop(refused());
    await pane.ladder.request([], pane.ctx);
    pane.finishCompaction();
    // No weaker model of the family: the pass is spent at its last rung.
    expect(await pane.stop(refused())).toBeUndefined();
    expect(await pane.stop(refused())).toBeUndefined();
    expect(await pane.stop(held())).toBeUndefined();

    pane.ladder.turnEnded({ role: "assistant", stopReason: "stop" });
    expect(await pane.stop(refused())).toMatchObject({ additionalContext: CYBER_POLICY_CONTINUE });
  });

  test("a reply the provider gave ends the pass, so the next refusal starts again at the retry", async () => {
    const pane = rig();
    await pane.stop(refused());
    await pane.stop(refused());
    pane.ladder.turnEnded({ role: "assistant", stopReason: "toolUse" });
    expect(await pane.stop(refused())).toMatchObject({ additionalContext: CYBER_POLICY_CONTINUE });
    expect(pane.waits).toEqual([15_000, 25_000, 15_000]);
  });

  test("the compaction aborts its turn with no turn_end, so the resumed turn's refusal climbs on", async () => {
    const pane = rig();
    for (let rung = 0; rung < 3; rung += 1) await pane.stop(refused());
    // Oh My Pi disconnects before its compaction aborts the turn: no `turn_end` reaches the plugin.
    await pane.ladder.request([], pane.ctx);
    pane.finishCompaction();
    await settle();
    // The resumed turn's refusal climbs the next rung, and with none left the pass is spent.
    expect(await pane.stop(refused())).toBeUndefined();
    expect(pane.waits).toEqual([15_000, 25_000, 25_000]);
  });

  test("an abort ends the pass, so the next refusal starts again at the retry", async () => {
    const pane = rig();
    await pane.stop(refused());
    pane.ladder.turnEnded({ role: "assistant", stopReason: "aborted" });
    expect(await pane.stop(refused())).toMatchObject({ additionalContext: CYBER_POLICY_CONTINUE });
    expect(pane.waits).toEqual([15_000, 15_000]);
  });

  test("an abort after the compacted rung's compaction ends the pass too", async () => {
    const pane = rig({ weaker: WEAKER });
    for (let rung = 0; rung < 3; rung += 1) await pane.stop(refused());
    await pane.ladder.request([], pane.ctx);
    pane.finishCompaction();
    await settle();
    // A person or the daemon interrupts the resumed turn.
    pane.ladder.turnEnded({ role: "assistant", stopReason: "aborted" });
    expect(await pane.stop(refused())).toMatchObject({ additionalContext: CYBER_POLICY_CONTINUE });
    expect(pane.waits.at(-1)).toBe(15_000);
    expect(pane.calls).not.toContain("step openai/gpt-5.4-mini");
  });

  test("another provider error and a clean end are no refusal and climb nothing", async () => {
    const pane = rig();
    expect(await pane.stop(otherError())).toBeUndefined();
    expect(await pane.stop({ role: "assistant", stopReason: "stop" })).toBeUndefined();
    expect(pane.calls).toEqual([]);
  });

  test("a settle pass aborted while it waits climbs nothing", async () => {
    const pane = rig();
    const aborted = new AbortController();
    aborted.abort();
    expect(
      await pane.ladder.settle(
        { last_assistant_message: refused(), signal: aborted.signal },
        pane.ctx
      )
    ).toBeUndefined();
    expect(pane.calls).toEqual([]);
  });

  test("a rung whose settle pass ends while its model switch runs ends no hold", async () => {
    const interrupt = new AbortController();
    const pane = rig({ weaker: WEAKER, compact: "absent" });
    pane.hooks.onStep = () => interrupt.abort();
    await pane.stop(refused(), interrupt.signal);
    await pane.stop(refused(), interrupt.signal);
    // The weaker model's rung: the switch had begun when the pass was aborted, and nothing is sent.
    expect(await pane.stop(refused(), interrupt.signal)).toBeUndefined();
    expect(pane.calls).toEqual(["outside false", "outside false", "step openai/gpt-5.4-mini"]);
  });

  test("a rung whose 28 s budget, counted from the handler's start, runs out moves no model and ends no hold", async () => {
    const pane = rig({ weaker: WEAKER, compact: "absent" });
    await pane.stop(refused());
    await pane.stop(refused());
    // The weaker model's rung: its budget runs out during the wait.
    pane.hooks.onWait = () => pane.budgets.at(-1)?.controller.abort();
    expect(await pane.stop(refused())).toBeUndefined();
    expect(pane.budgets.map((budget) => budget.ms)).toEqual([28_000, 28_000, 28_000]);
    expect(pane.calls).toEqual(["outside false", "outside false"]);
  });

  test("a session change during the model switch leaves the new session's ladder whole", async () => {
    const pane = rig({ weaker: WEAKER, compact: "absent" });
    await pane.stop(refused());
    await pane.stop(refused());
    // The switch fails, and the pane moved to another session meanwhile.
    pane.hooks.stepAnswer = false;
    pane.hooks.onStep = () => pane.ladder.reset();
    expect(await pane.stop(refused())).toBeUndefined();
    pane.hooks.onStep = undefined;
    // The new session's first refusal still gets a pass, from the retry.
    expect(await pane.stop(refused())).toMatchObject({ additionalContext: CYBER_POLICY_CONTINUE });
    expect(pane.waits.at(-1)).toBe(15_000);
  });

  test("a session change while a refusal is classed starts the new session at the retry", async () => {
    const pane = rig();
    await pane.stop(refused());
    pane.hooks.onRefusalCheck = () => pane.ladder.reset();
    // That refusal was the old session's: it climbs nothing.
    expect(await pane.stop(refused())).toBeUndefined();
    pane.hooks.onRefusalCheck = undefined;
    expect(await pane.stop(refused())).toMatchObject({ additionalContext: CYBER_POLICY_CONTINUE });
    expect(pane.waits).toEqual([15_000, 15_000]);
  });

  test("a host with no compaction for extensions passes over the compacted rung", async () => {
    const pane = rig({ weaker: WEAKER, compact: "absent" });
    await pane.stop(refused());
    await pane.stop(refused());
    expect(await pane.stop(refused())).toMatchObject({ additionalContext: CYBER_POLICY_CONTINUE });
    expect(pane.calls).toContain("step openai/gpt-5.4-mini");
  });
});

describe("a compaction that fails", () => {
  test("counts as refused: a turn from inside the session is held, and its settle climbs the weaker model", async () => {
    const pane = rig({ weaker: WEAKER });
    for (let rung = 0; rung < 3; rung += 1) await pane.stop(refused());
    await pane.ladder.request([], pane.ctx);
    pane.failCompaction(new Error("cyber_policy: the summary was refused"));
    await settle();

    expect(pane.calls.at(-1)).toBe("inside true");
    // The hold holds that turn's call, and its settle climbs the last rung from outside the session.
    expect(await pane.stop(held())).toEqual({
      continue: true,
      additionalContext: CYBER_POLICY_CONTINUE,
    });
    expect(pane.waits.at(-1)).toBe(25_000);
    expect(pane.calls.slice(-2)).toEqual(["step openai/gpt-5.4-mini", "outside false"]);
    // That rung's own refusal spends the pass.
    expect(await pane.stop(refused())).toBeUndefined();
  });

  test("with no rung left starts a turn the hold refuses, so the refusal reaches the daemon", async () => {
    const pane = rig();
    for (let rung = 0; rung < 3; rung += 1) await pane.stop(refused());
    await pane.ladder.request([], pane.ctx);
    pane.failCompaction(new Error("cyber_policy: the summary was refused"));
    await settle();

    expect(pane.calls.at(-1)).toBe("inside true");
    // The held call of that turn settles: the pass is spent.
    expect(await pane.stop(held())).toBeUndefined();
  });

  test("after the pane moved to another session answers nothing of the new one's", async () => {
    const pane = rig({ weaker: WEAKER });
    for (let rung = 0; rung < 3; rung += 1) await pane.stop(refused());
    await pane.ladder.request([], pane.ctx);
    pane.ladder.reset();
    pane.failCompaction(new Error("cyber_policy: the summary was refused"));
    await settle();

    // No turn is started for the old session's pass, and the new session's first refusal starts
    // its own pass at the retry.
    expect(pane.calls.at(-1)).toBe("compact outside soft");
    expect(await pane.stop(refused())).toMatchObject({ additionalContext: CYBER_POLICY_CONTINUE });
    expect(pane.waits.at(-1)).toBe(15_000);
  });

  test("with nothing to compact starts no turn: the turn Oh My Pi resumes is the rung's request", async () => {
    const pane = rig({ weaker: WEAKER });
    for (let rung = 0; rung < 3; rung += 1) await pane.stop(refused());
    await pane.ladder.request([], pane.ctx);
    pane.failCompaction(
      named("ManualCompactionNoOpError", "Nothing to compact (session too small)")
    );
    await settle();

    expect(pane.calls.at(-1)).toBe("compact outside soft");
    // The resumed turn is refused: the pass goes on to its last rung, and only once.
    expect(await pane.stop(refused())).toMatchObject({ additionalContext: CYBER_POLICY_CONTINUE });
    expect(pane.calls.slice(-2)).toEqual(["step openai/gpt-5.4-mini", "outside false"]);
    expect(await pane.stop(refused())).toBeUndefined();
    expect(pane.calls.filter((call) => call.startsWith("inside"))).toEqual([]);
  });

  test("that someone stopped ends the pass and starts no turn", async () => {
    const pane = rig({ weaker: WEAKER });
    for (let rung = 0; rung < 3; rung += 1) await pane.stop(refused());
    await pane.ladder.request([], pane.ctx);
    pane.failCompaction(named("CompactionCancelledError", "Compaction cancelled"));
    await settle();

    expect(pane.calls.at(-1)).toBe("compact outside soft");
    // The pass is over: the next refusal starts a new one at the retry.
    expect(await pane.stop(refused())).toMatchObject({ additionalContext: CYBER_POLICY_CONTINUE });
    expect(pane.waits.at(-1)).toBe(15_000);
  });
});

describe("every request", () => {
  const user = { role: "user", content: [{ type: "text", text: "Do the task." }] };
  const holdEnd = { role: "custom", customType: CYBER_POLICY_RETRY_ENTRY, content: "note" };
  const plain = {
    role: "custom",
    customType: "session-stop-continuation",
    content: CYBER_POLICY_CONTINUE,
  };
  const guidance = {
    role: "custom",
    customType: "session-stop-continuation",
    content: DEFAULT_CYBER_POLICY_MESSAGE,
  };

  test("leaves out the ladder's own messages and the refused replies", async () => {
    const pane = rig();
    expect(
      await pane.ladder.request(
        [user, refused(), holdEnd, plain, refused(), holdEnd, guidance],
        pane.ctx
      )
    ).toEqual([user, guidance]);
  });

  test("keeps a refused reply that carries a tool call, and another provider's error", async () => {
    const pane = rig();
    const withCall = { ...refused(), content: [{ type: "toolCall", id: "call-1" }] };
    // Claude Code's name for the same block.
    const withToolUse = { ...refused(), content: [{ type: "tool_use", id: "call-2" }] };
    expect(
      await pane.ladder.request([user, withCall, withToolUse, otherError()], pane.ctx)
    ).toBeUndefined();
  });
});

describe("the ladder's compaction", () => {
  const preparation = (): CompactionPreparation => ({
    firstKeptEntryId: "e3",
    messagesToSummarize: ["m1"],
    turnPrefixMessages: ["m2"],
    recentMessages: ["m3", "m4", "refused"],
    recentEntryIds: ["e3", "e4", "leaf"],
    isSplitTurn: true,
  });

  test("summarizes the newest turn group and keeps only the leaf", () => {
    const prepared = preparation();
    summarizeThrough(prepared, "leaf");
    expect(prepared).toEqual({
      firstKeptEntryId: "leaf",
      messagesToSummarize: ["m1", "m2", "m3", "m4"],
      turnPrefixMessages: [],
      recentMessages: ["refused"],
      recentEntryIds: ["leaf"],
      isSplitTurn: false,
    });
  });

  test("registers its session_before_compact handler at its first compaction, and once", async () => {
    const pane = rig();
    // Until the ladder compacts, no handler is registered: Oh My Pi's background compaction runs.
    for (let rung = 0; rung < 3; rung += 1) await pane.stop(refused());
    expect(pane.beforeCompact).toHaveLength(0);
    await pane.ladder.request([], pane.ctx);
    expect(pane.beforeCompact).toHaveLength(1);

    pane.failCompaction(named("CompactionCancelledError", "Compaction cancelled"));
    await settle();
    for (let rung = 0; rung < 3; rung += 1) await pane.stop(refused());
    await pane.ladder.request([], pane.ctx);
    expect(pane.beforeCompact).toHaveLength(1);
  });

  test("widens only a compaction the ladder started", async () => {
    const pane = rig();
    for (let rung = 0; rung < 3; rung += 1) await pane.stop(refused());
    await pane.ladder.request([], pane.ctx);
    const widen = (event: SessionBeforeCompactEvent) => {
      for (const handler of pane.beforeCompact) handler(event);
    };
    const widened = preparation();
    widen({ preparation: widened, branchEntries: [{ id: "leaf" }] });
    expect(widened.firstKeptEntryId).toBe("leaf");
    expect(widened.recentMessages).toEqual(["refused"]);

    // Once the ladder's compaction is over, the host's own compactions are left as prepared.
    pane.finishCompaction();
    await settle();
    const untouched = preparation();
    widen({ preparation: untouched, branchEntries: [{ id: "leaf" }] });
    expect(untouched).toEqual(preparation());
  });
});
