import { afterEach, expect, test } from "bun:test";
import {
  type Cleanup,
  isSelfCheck,
  isSummarization,
  messageStream,
  ompNatsGuard,
  ownMessages,
  type Request,
  runLegionPane,
  type StandinProvider,
} from "@legion/pi-shared/test/omp-harness";
import { DEFAULT_CYBER_POLICY_MESSAGE } from "../src/cyber-policy-ladder";

// The cyber_policy ladder on the real Oh My Pi (src/cyber-policy-ladder.ts): only the real binary
// shows that each rung's request reaches the gateway past the host's hold, what each request
// carries, that the daemon's prompt stays open until the ladder's last rung, and what the daemon
// stand-in is told. LEGION_TEST_OMP names the binary (the repository's .omp-pin) and
// LEGION_TEST_NATS_URL the NATS server each pane runs against (`ompNatsGuard`).
const { skip, setup } = ompNatsGuard();

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

/** Two OpenAI-family models, as the host's catalog reads them from their ids: the session's own,
 * which every model role names, and the weaker one below it in the chain the fourth rung uses. */
const PROVIDERS: readonly StandinProvider[] = [
  { provider: "standin", models: ["gpt-5.5", "gpt-5.4-mini"] },
];

/** Oh My Pi's own cyber_policy refusal as a gateway relays it: a request error naming the code
 * (`test/rpc-prompt-result.test.ts:205-212` at v18.18.6-sami.20261125-164517). Synthetic text. */
function refuses(): Response {
  return Response.json(
    {
      type: "error",
      error: {
        type: "invalid_request_error",
        message: "cyber_policy: This content was flagged for possible cybersecurity risk.",
      },
    },
    { status: 400 }
  );
}

function replies(text: string, id: string): Response {
  return new Response(messageStream([{ type: "text", text }], id), {
    headers: { "content-type": "text/event-stream" },
  });
}

/** One request the stand-in gateway served: its model, whether it was a compaction's summary and
 * whether it carried an image, and its own messages as role and text, cache markers and the date
 * reminder left out. */
interface Served {
  readonly model: string;
  readonly summary: boolean;
  readonly images: boolean;
  readonly conversation: readonly string[];
}

function conversation(request: Request): string[] {
  return ownMessages(request.body).map((message) => {
    if (typeof message !== "object" || message === null) return "";
    const role = "role" in message ? String(message.role) : "";
    const content = "content" in message ? message.content : undefined;
    const text =
      typeof content === "string"
        ? content
        : Array.isArray(content)
          ? content
              .map((block: unknown) =>
                typeof block === "object" && block !== null && "text" in block
                  ? String(block.text)
                  : ""
              )
              .join("")
          : "";
    return `${role}: ${text.replace(/^<system-reminder>[\s\S]*?<\/system-reminder>\n/, "")}`;
  });
}

/** What a ladder pane left: every gateway request, the prompt results omp wrote, and every
 * turn-outcome report the daemon stand-in took. */
interface LadderPane {
  readonly served: readonly Served[];
  /** The conversation's own turns, every summary request left out. */
  readonly turns: () => readonly Served[];
  readonly promptResults: readonly Record<string, unknown>[];
  readonly turnOutcomes: readonly Record<string, unknown>[];
}

/** The task whose turns the gateway refuses. */
const TASK = "Do the cyber task.";

/**
 * Runs a root architect's pane (`runLegionPane`, a Legion claim, so the ladder runs) whose
 * gateway answers each request with `answer`. Every wait of the ladder is 0 here; their lengths
 * are the unit tests' (`src/cyber-policy-ladder.test.ts`). `warmUps` are answered prompts sent
 * first, so a compaction has history to summarize; the keep budget is small for the same reason,
 * unless `shipped` leaves every compaction setting at what a deployment runs.
 */
async function ladderPane(options: {
  readonly answer: (served: Served, nth: number) => Response;
  readonly chain?: boolean;
  readonly warmUps?: number;
  /** How many times each warm-up repeats its sentence; 60 by default. */
  readonly warmUpLength?: number;
  /** Refuse the compaction's summary requests too, so the compacted rung fails. */
  readonly refuseSummaries?: boolean;
  /** No `compaction:` settings: Oh My Pi's own `keepRecentTokens` and `methodOrder`, as a
   * deployed pane runs them. */
  readonly shipped?: boolean;
  /** The models take images, so snapcompact is a method the host could run. */
  readonly images?: boolean;
  /** How long the pane stays up after its last prompt settled, so a late send would be served. */
  readonly lingerMs?: number;
}): Promise<LadderPane> {
  const { binary, nats } = setup();
  const served: Served[] = [];
  const promptResults: Record<string, unknown>[] = [];
  const turnOutcomes: Record<string, unknown>[] = [];
  const warmUps = Array.from(
    { length: options.warmUps ?? 0 },
    (_, n) => `Warm-up ${n}: ${"Read the notes and say done. ".repeat(options.warmUpLength ?? 60)}`
  );
  await runLegionPane(
    binary,
    [],
    {
      name: "ladder",
      role: "architect",
      tree: "LADDER-1",
      issue: "LADDER-1",
      nats,
      prompt: [...warmUps, TASK],
      providers: PROVIDERS.map((provider) => ({ ...provider, images: options.images === true })),
      config: [
        "retry:",
        "  modelFallback: true",
        "  fallbackRevertPolicy: never",
        ...(options.chain === true
          ? ["  fallbackChains:", "    standin/gpt-5.5: [standin/gpt-5.4-mini]"]
          : []),
        ...(options.shipped === true
          ? []
          : ["compaction:", "  keepRecentTokens: 100", "  methodOrder: [soft]"]),
      ],
      env: {
        LEGION_CYBER_POLICY_RETRY_WAIT_MS: "0",
        LEGION_CYBER_POLICY_MESSAGE_WAIT_MS: "0",
        LEGION_CYBER_POLICY_COMPACT_WAIT_MS: "0",
        LEGION_CYBER_POLICY_FALLBACK_WAIT_MS: "0",
      },
      onFrame: (frame) => {
        if ("type" in frame && frame.type === "prompt_result") {
          promptResults.push(frame as Record<string, unknown>);
        }
      },
      // A send that should not happen has no event to await, so the pane stays up a real while
      // after its last prompt, long enough for a ladder whose waits are 0 to make one.
      ...(options.lingerMs === undefined
        ? {}
        : {
            whileUp: () => {
              const { promise, resolve } = Promise.withResolvers<void>();
              setTimeout(resolve, options.lingerMs);
              return promise;
            },
          }),
      answer: (url, body) => {
        if (url.pathname === "/legion/v1/claims/turn-outcome") {
          turnOutcomes.push(body);
          return Response.json({ refusedRun: 0, failed: false });
        }
        if (url.pathname !== "/anthropic/v1/messages") return undefined;
        const request: Request = { path: url.pathname, body };
        // The run-end self-check is the runner's to answer.
        if (isSelfCheck(request)) return undefined;
        const turn: Served = {
          model: String(body.model ?? ""),
          summary: isSummarization(request),
          images: JSON.stringify(body.messages ?? null).includes('"type":"image"'),
          conversation: conversation(request),
        };
        served.push(turn);
        if (turn.summary) {
          return options.refuseSummaries === true
            ? refuses()
            : replies("SUMMARY: the notes were read.", `sum_${served.length}`);
        }
        return options.answer(turn, served.filter((each) => !each.summary).length);
      },
    },
    cleanup
  );
  return {
    served,
    turns: () => served.filter((turn) => !turn.summary),
    promptResults,
    turnOutcomes,
  };
}

/** Whether the turn is one of the task's, not a warm-up's. */
const onTask = (turn: Served): boolean =>
  turn.conversation.some((line) => line.includes(TASK)) ||
  turn.conversation.some((line) => line.includes("SUMMARY"));

/** The prompt result of the task, the last prompt each pane sends. */
const taskResult = (pane: LadderPane) => pane.promptResults.at(-1);

test.skipIf(skip)(
  "a refusal the first retry gets past: the same request goes again and the task's prompt completes",
  async () => {
    const pane = await ladderPane({
      answer: (_turn, nth) => (nth === 1 ? refuses() : replies("Done.", `msg_${nth}`)),
    });

    const turns = pane.turns();
    expect(turns).toHaveLength(2);
    // The plain retry: the request that was refused, sent again as it was, past the host's hold.
    expect(turns[1]?.conversation).toEqual(turns[0]?.conversation);
    expect(taskResult(pane)).toMatchObject({ status: "completed" });
    expect(pane.turnOutcomes.filter((report) => report.code === "cyber_policy")).toEqual([]);
  },
  120_000
);

test.skipIf(skip)(
  "a refusal the retry with a message gets past: the message is appended and the task's prompt completes",
  async () => {
    const pane = await ladderPane({
      answer: (_turn, nth) => (nth <= 2 ? refuses() : replies("Done.", `msg_${nth}`)),
    });

    const turns = pane.turns();
    expect(turns).toHaveLength(3);
    // The second rung's request is the refused one with the ladder's message after it.
    expect(turns[2]?.conversation.slice(0, -1)).toEqual(turns[0]?.conversation);
    expect(turns[2]?.conversation.at(-1)).toContain(DEFAULT_CYBER_POLICY_MESSAGE);
    expect(taskResult(pane)).toMatchObject({ status: "completed" });
    expect(pane.turnOutcomes.filter((report) => report.code === "cyber_policy")).toEqual([]);
  },
  120_000
);

test.skipIf(skip)(
  "a refusal the compacted retry gets past: the request goes from a summary, without the refused task",
  async () => {
    let refusals = 0;
    const pane = await ladderPane({
      warmUps: 2,
      answer: (turn, nth) => {
        if (!onTask(turn) || refusals >= 3) return replies("Done.", `msg_${nth}`);
        refusals += 1;
        return refuses();
      },
    });

    const task = pane.served.filter(onTask);
    // The three refused requests, the compaction's summary requests, and the resumed turn.
    expect(task.slice(0, 3).every((turn) => !turn.summary)).toBe(true);
    expect(task.slice(3).some((turn) => turn.summary)).toBe(true);
    const resumed = task.at(-1);
    expect(resumed?.summary).toBe(false);
    // The compaction summarized the newest turn group too: the task's own words are gone from
    // the request, and the summary stands in for them.
    expect(resumed?.conversation.some((line) => line.includes("SUMMARY"))).toBe(true);
    expect(resumed?.conversation.some((line) => line.includes(TASK))).toBe(false);
    expect(taskResult(pane)).toMatchObject({ status: "completed" });
    expect(pane.turnOutcomes.filter((report) => report.code === "cyber_policy")).toEqual([]);
  },
  180_000
);

test.skipIf(skip)(
  "a refusal only the weaker model of the same family gets past: the request goes to it and completes",
  async () => {
    const pane = await ladderPane({
      warmUps: 2,
      chain: true,
      answer: (turn, nth) =>
        onTask(turn) && turn.model === "gpt-5.5" ? refuses() : replies("Done.", `msg_${nth}`),
    });

    const task = pane.turns().filter(onTask);
    // Retry, message and the compacted retry all went to the session's own model; the fourth
    // rung went to the weaker model the chain names, which answered.
    expect(task.map((turn) => turn.model)).toEqual([
      "gpt-5.5",
      "gpt-5.5",
      "gpt-5.5",
      "gpt-5.5",
      "gpt-5.4-mini",
    ]);
    expect(taskResult(pane)).toMatchObject({ status: "completed" });
    expect(pane.turnOutcomes.filter((report) => report.code === "cyber_policy")).toEqual([]);
  },
  180_000
);

test.skipIf(skip)(
  "a refusal every rung meets reaches the daemon with its cyber_policy code, after one pass",
  async () => {
    const pane = await ladderPane({
      warmUps: 2,
      chain: true,
      answer: (turn, nth) => (onTask(turn) ? refuses() : replies("Done.", `msg_${nth}`)),
    });

    // The task's request, then one try of each rung: retry, message, compacted retry, weaker
    // model. No second pass.
    const task = pane.turns().filter(onTask);
    expect(task.map((turn) => turn.model)).toEqual([
      "gpt-5.5",
      "gpt-5.5",
      "gpt-5.5",
      "gpt-5.5",
      "gpt-5.4-mini",
    ]);
    expect(taskResult(pane)).toMatchObject({
      status: "error",
      error: { code: "cyber_policy", retryable: false },
    });
    expect(pane.turnOutcomes.filter((report) => report.code === "cyber_policy")).toHaveLength(1);
  },
  180_000
);

test.skipIf(skip)(
  "a compaction refused too counts as refused: the weaker model is tried next, then the refusal reaches the daemon",
  async () => {
    const pane = await ladderPane({
      warmUps: 2,
      chain: true,
      refuseSummaries: true,
      answer: (turn, nth) => (onTask(turn) ? refuses() : replies("Done.", `msg_${nth}`)),
    });

    // The compacted rung's own request is aborted before it is sent, and its summary was refused,
    // so nothing resumes it: the ladder starts a turn from inside the session, the hold holds its
    // call, and that held call's settle climbs the last rung, on the weaker model.
    const task = pane.turns().filter(onTask);
    expect(task.map((turn) => turn.model)).toEqual([
      "gpt-5.5",
      "gpt-5.5",
      "gpt-5.5",
      "gpt-5.4-mini",
    ]);
    expect(pane.served.some((turn) => turn.summary)).toBe(true);
    expect(taskResult(pane)).toMatchObject({ status: "error", error: { code: "cyber_policy" } });
    expect(pane.turnOutcomes.filter((report) => report.code === "cyber_policy")).toHaveLength(1);
  },
  180_000
);

test.skipIf(skip)(
  "a compaction refused with no weaker model of the family left settles the prompt with cyber_policy",
  async () => {
    const pane = await ladderPane({
      warmUps: 2,
      refuseSummaries: true,
      answer: (turn, nth) => (onTask(turn) ? refuses() : replies("Done.", `msg_${nth}`)),
    });

    // No rung is left: the turn the ladder starts is held by Oh My Pi's hold, so nothing more
    // reaches the gateway, and the prompt the compaction left open settles with the code.
    const task = pane.turns().filter(onTask);
    expect(task.map((turn) => turn.model)).toEqual(["gpt-5.5", "gpt-5.5", "gpt-5.5"]);
    expect(taskResult(pane)).toMatchObject({ status: "error", error: { code: "cyber_policy" } });
    expect(pane.turnOutcomes.filter((report) => report.code === "cyber_policy")).toHaveLength(1);
  },
  180_000
);

test.skipIf(skip)(
  "under the shipped compaction settings a new session has nothing to compact: the resumed turn is the third rung, one pass runs, and nothing is sent after",
  async () => {
    const pane = await ladderPane({
      shipped: true,
      chain: true,
      lingerMs: 3_000,
      answer: () => refuses(),
    });

    // At the shipped keepRecentTokens a new session has nothing to compact: Oh My Pi resumes the
    // turn, and that request is the third rung's; the fourth goes to the weaker model. The pane
    // stayed up after the refusal settled, and the gateway took nothing more.
    const task = pane.turns().filter(onTask);
    expect(task.map((turn) => turn.model)).toEqual([
      "gpt-5.5",
      "gpt-5.5",
      "gpt-5.5",
      "gpt-5.5",
      "gpt-5.4-mini",
    ]);
    expect(pane.served.some((turn) => turn.summary)).toBe(false);
    expect(taskResult(pane)).toMatchObject({ status: "error", error: { code: "cyber_policy" } });
    expect(pane.turnOutcomes.filter((report) => report.code === "cyber_policy")).toHaveLength(1);
  },
  180_000
);

test.skipIf(skip)(
  "under the shipped compaction methods on a model that takes images, the compacted rung is a text summary without the refused task",
  async () => {
    let refusals = 0;
    const pane = await ladderPane({
      shipped: true,
      images: true,
      // More history than the shipped keepRecentTokens of 20,000 keeps, so there is something
      // to compact.
      warmUps: 3,
      warmUpLength: 1_500,
      answer: (turn, nth) => {
        if (!onTask(turn) || refusals >= 3) return replies("Done.", `msg_${nth}`);
        refusals += 1;
        return refuses();
      },
    });

    // The shipped methodOrder would pick snapcompact, which draws the history as images; the
    // ladder asks for a text summary instead.
    expect(pane.served.some((turn) => turn.summary)).toBe(true);
    expect(pane.served.filter((turn) => turn.images)).toEqual([]);
    const resumed = pane.turns().at(-1);
    expect(resumed?.conversation.some((line) => line.includes("SUMMARY"))).toBe(true);
    expect(resumed?.conversation.some((line) => line.includes(TASK))).toBe(false);
    expect(taskResult(pane)).toMatchObject({ status: "completed" });
  },
  240_000
);
