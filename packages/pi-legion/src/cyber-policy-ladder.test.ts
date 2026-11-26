import { describe, expect, test } from "bun:test";
import type { CompactionPreparation, HostModel } from "@legion/pi-shared/pi-types";
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

interface Ctx {
  compact?: (options: { readonly origin?: "outside" | "inside" }) => Promise<void>;
}

function rig(options: { weaker?: HostModel; compact?: "ok" | "fails" | "absent" } = {}) {
  const calls: string[] = [];
  const waits: number[] = [];
  const warnings: string[] = [];
  let failCompaction: ((error: Error) => void) | undefined;
  let finishCompaction: (() => void) | undefined;
  const ctx: Ctx =
    options.compact === "absent"
      ? {}
      : {
          compact: (compactOptions) => {
            calls.push(`compact ${compactOptions.origin}`);
            const { promise, resolve, reject } = Promise.withResolvers<void>();
            finishCompaction = resolve;
            failCompaction = reject;
            return promise;
          },
        };
  const ladder = createCyberPolicyLadder<Ctx>({
    settings: SETTINGS,
    cyberPolicyRefusal: async (message) =>
      typeof message === "object" &&
      message !== null &&
      "errorMessage" in message &&
      String(message.errorMessage).includes("cyber_policy"),
    recordableRefusal: async (message) =>
      typeof message === "object" &&
      message !== null &&
      "errorMessage" in message &&
      String(message.errorMessage).includes("cyber_policy") &&
      !String(message.errorMessage).startsWith(HOLD_PREFIX),
    sendOutside: (triggerTurn) => void calls.push(`outside ${triggerTurn}`),
    sendInside: () => void calls.push("inside"),
    sameFamilyFallback: async () => options.weaker,
    stepDownTo: async (_ctx, model) => {
      calls.push(`step ${model.provider}/${model.id}`);
      return true;
    },
    wait: async (ms, signal) => {
      waits.push(ms);
      return signal?.aborted !== true;
    },
    paneContext: () => ctx,
    log: { debug: () => undefined, warn: (message) => void warnings.push(message) },
  });
  const signal = new AbortController().signal;
  return {
    ladder,
    ctx,
    calls,
    waits,
    warnings,
    stop: (message: unknown) => ladder.settle({ last_assistant_message: message, signal }, ctx),
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
      "compact outside",
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

  test("an aborted reply, as the compaction's own abort leaves, does not end the pass", async () => {
    const pane = rig();
    await pane.stop(refused());
    pane.ladder.turnEnded({ role: "assistant", stopReason: "aborted" });
    expect(await pane.stop(refused())).toMatchObject({
      additionalContext: DEFAULT_CYBER_POLICY_MESSAGE,
    });
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

  test("a host with no compaction for extensions passes over the compacted rung", async () => {
    const pane = rig({ weaker: WEAKER, compact: "absent" });
    await pane.stop(refused());
    await pane.stop(refused());
    expect(await pane.stop(refused())).toMatchObject({ additionalContext: CYBER_POLICY_CONTINUE });
    expect(pane.calls).toContain("step openai/gpt-5.4-mini");
  });
});

describe("a compaction that fails", () => {
  test("counts as refused: the weaker model of the family is tried from outside the session", async () => {
    const pane = rig({ weaker: WEAKER });
    for (let rung = 0; rung < 3; rung += 1) await pane.stop(refused());
    await pane.ladder.request([], pane.ctx);
    pane.failCompaction(new Error("cyber_policy: the summary was refused"));
    await settle();
    await settle();

    expect(pane.waits.at(-1)).toBe(25_000);
    expect(pane.calls.slice(-2)).toEqual(["step openai/gpt-5.4-mini", "outside true"]);
    // That rung's own refusal spends the pass.
    expect(await pane.stop(refused())).toBeUndefined();
  });

  test("with no rung left starts a turn the hold refuses, so the refusal reaches the daemon", async () => {
    const pane = rig();
    for (let rung = 0; rung < 3; rung += 1) await pane.stop(refused());
    await pane.ladder.request([], pane.ctx);
    pane.failCompaction(new Error("cyber_policy: the summary was refused"));
    await settle();

    expect(pane.calls.at(-1)).toBe("inside");
    // The held call of that turn settles: the pass is spent.
    expect(await pane.stop(held())).toBeUndefined();
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
    expect(await pane.ladder.request([user, withCall, otherError()], pane.ctx)).toBeUndefined();
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

  test("widens only a compaction the ladder started", async () => {
    const pane = rig();
    const untouched = preparation();
    pane.ladder.beforeCompact({ preparation: untouched, branchEntries: [{ id: "leaf" }] });
    expect(untouched).toEqual(preparation());

    for (let rung = 0; rung < 3; rung += 1) await pane.stop(refused());
    await pane.ladder.request([], pane.ctx);
    const widened = preparation();
    pane.ladder.beforeCompact({ preparation: widened, branchEntries: [{ id: "leaf" }] });
    expect(widened.firstKeptEntryId).toBe("leaf");
    expect(widened.recentMessages).toEqual(["refused"]);
  });
});
