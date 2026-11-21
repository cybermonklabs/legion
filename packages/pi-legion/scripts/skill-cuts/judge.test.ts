import { beforeAll, expect, test } from "bun:test";
import { mkdirSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { z } from "zod";
import {
  SessionEntry,
  type SessionMessage,
  skillHeaderPath,
  stripSkillHeader,
} from "../skill-scenarios/transcript";
import { type Arm, type ArmName, buildArm } from "./arms";
import { type Case, pathHash } from "./cases";
import {
  type ContentBlock,
  createGateway,
  type Gateway,
  type MessagesBody,
  ReplyMessage,
  responseMessage,
  streamedMessage,
  streamedResponse,
} from "./gateway";
import {
  type ArmReply,
  BEFORE_MESSAGES,
  beforeText,
  blindOrder,
  JUDGE_TRIES,
  judgeBlind,
  judgeBody,
  judgeControls,
  judgeReply,
  MESSAGE_CHARS,
  NOTHING_REPLY,
  newSeed,
  stepText,
} from "./judge";
import { PROMPTS_DIR, parsePrompt } from "./readers";

// The blind judge, on F1, F2, F5 and F9 (testdata/, captured as testdata/CAPTURE.md says). F1 is
// an Oh My Pi 18.18.6 session of a scratch profile with the made-up skills alpha and beta
// (`HOME=<scratch> omp -p --session-dir <scratch>/sessions --model anthropic/claude-sonnet-5-5
// "<prompt>"`): 0 the user's prompt, 1 a read of skill://alpha, 2 its result, 3 a read of
// skill://alpha/references/x.md, 4 its result, 5 a read of the missing skill://gamma, 6 its
// result, 7 a `task` call. F5 holds the gateway's replies to bodies made from F2's replayed
// history (`bun $TOOLS/send.ts <body.json> <F2 headers.json>`); F5-judge is its reply to the
// judge's own body (`judgeBody`) for a case made from that history and F5-text's reply (`bun
// $TOOLS/send.ts <judge body.json>`). F9 holds the gateway's Responses API replies
// (openai/gpt-6.1-sol's) to bodies made from F8, the F2-like history Oh My Pi sent for that model
// (`bun $O/send-openai.ts <body.json>`). The gateway's fetch is a fake that answers with them, or
// with a synthetic judgement: no test here reaches a network.

const TESTDATA = path.join(import.meta.dir, "testdata");
const F1 = path.join(
  TESTDATA,
  "F1",
  "2026-11-20T01-59-41-780Z_01a1f6ae-51d4-715f-a2a6-59f275bdff80.jsonl"
);
const scratch = temporaryDirectory("skill-cuts-judge", { real: true });

/** The one user message of a judge body, its blocks' text. */
const UserMessage = z.tuple([
  z.object({
    role: z.literal("user"),
    content: z.array(z.object({ type: z.literal("text"), text: z.string() })),
  }),
]);

/** F1's messages, in order. */
const messages: SessionMessage[] = readFileSync(F1, "utf8")
  .split("\n")
  .filter((line) => line.trim() !== "")
  .map((line) => SessionEntry.parse(JSON.parse(line)))
  .flatMap((entry) => (entry.type === "message" && entry.message ? [entry.message] : []));

/** The one text of a message. */
function textOf(message: SessionMessage | undefined): string {
  return message?.content?.[0]?.text ?? "";
}

/** The file F1's read result at `index` read, as it was on disk. */
function f1File(index: number): string {
  return `${stripSkillHeader(textOf(messages[index]))}\n`;
}

const LINE = "2. Write each step's result on one line.";
/** Where F1's profile installed alpha. */
const INSTALLED = path.dirname(skillHeaderPath(textOf(messages[2])) ?? "");

/** The case of F1's `task` step, which line 9 of alpha's SKILL.md governs. */
const f1Case: Case = {
  id: "c1",
  cut: "acme-skills-1",
  skill: "alpha",
  baseFile: "SKILL.md",
  line: { text: LINE, lineNumber: 9, lineCount: 10 },
  criterion: {
    pass: "The reply hands the beta greeting to a task agent.",
    fail: "The reply reads skill://beta itself, or starts no task.",
  },
  model: "anthropic/claude-sonnet-5-5",
  thinking: "off",
  context: "devbox",
  profile: {
    skills: [
      { repo: "acme/skills", commit: "0123456789abcdef", path: "skills/alpha", name: "alpha" },
      { repo: "acme/skills", commit: "0123456789abcdef", path: "skills/beta", name: "beta" },
    ],
    plugins: [],
    contextFiles: [],
  },
  messages: messages.slice(0, 7),
  skillReads: [2, 4],
  realStep: messages[7] ?? {},
  // Reads skill://beta itself where the criterion asks for a task agent.
  violatingStep: {
    role: "assistant",
    content: [
      { type: "text", text: "Step 4." },
      { type: "toolCall", id: "violating_1", name: "read", arguments: { path: "skill://beta" } },
    ],
  },
  source: { store: "store_a", pathHash: pathHash("/sessions/alpha.jsonl") },
  sessionStart: "2026-11-20T01:59:41.780Z",
  secretsRemoved: 0,
  sessionDigest: null,
};

/** A skill directory under the scratch directory holding `files` (path inside it to text). */
function skillDir(name: string, files: Record<string, string>): string {
  const dir = path.join(scratch, name);
  for (const [file, text] of Object.entries(files)) {
    mkdirSync(path.dirname(path.join(dir, file)), { recursive: true });
    writeFileSync(path.join(dir, file), text);
  }
  return dir;
}

/** The old text is alpha's files as F1 read them; the new text rewrites both files. */
const oldDir = path.join(scratch, "old");
const newDir = path.join(scratch, "new");
beforeAll(() => {
  skillDir("old", { "SKILL.md": f1File(2), "references/x.md": f1File(4) });
  skillDir("new", {
    "SKILL.md": f1File(2).replace(
      "3. Stop after the third step.",
      "3. Halt once step three is done."
    ),
    "references/x.md": f1File(4).replace("counted from one", "numbered from one"),
  });
});

/** Each file's text under `dir`, by its path inside it. */
function filesUnder(dir: string): string[] {
  return readdirSync(dir, { recursive: true, withFileTypes: true })
    .filter((entry) => entry.isFile())
    .map((entry) => readFileSync(path.join(entry.parentPath, entry.name), "utf8"));
}

/** F5's reply `shape` as the gateway sent it. */
function f5(shape: string): Response {
  const kept = JSON.parse(readFileSync(path.join(TESTDATA, `F5-${shape}.headers.json`), "utf8"));
  return new Response(readFileSync(path.join(TESTDATA, `F5-${shape}.sse`), "utf8"), {
    status: kept.status,
    headers: kept.headers,
  });
}

/** The message F5's reply `shape` holds. */
function f5Message(shape: string): ReplyMessage {
  return ReplyMessage.parse(
    streamedMessage(readFileSync(path.join(TESTDATA, `F5-${shape}.sse`), "utf8"))
  );
}

/** The content of F9's Responses reply `shape`, as the gateway reads it: its output items. */
function f9Content(shape: string): ContentBlock[] {
  return responseMessage(
    streamedResponse(readFileSync(path.join(TESTDATA, `F9-${shape}.sse`), "utf8"))
  ).content;
}

/** The Messages API reply that matches a Responses reply's `content`: each message part a text
 *  block, each function call a `tool_use` block of its name and parsed arguments, each reasoning
 *  item a thinking block. */
function asMessagesReply(content: readonly ContentBlock[]): ContentBlock[] {
  return content.flatMap((item): ContentBlock[] => {
    if (item.type === "message" && Array.isArray(item.content)) {
      return item.content.map((part: { text: string }) => ({ type: "text", text: part.text }));
    }
    if (item.type === "function_call") {
      return [
        {
          type: "tool_use",
          id: item.call_id,
          name: item.name,
          input: JSON.parse(String(item.arguments)),
        },
      ];
    }
    return [{ type: "thinking", thinking: "made-up thinking", signature: "made-up-signature" }];
  });
}

/** A streamed reply whose one text block is `text`, in the shape the gateway streams (F5). */
function sse(text: string): Response {
  const events = [
    {
      type: "message_start",
      message: {
        model: "claude-opus-5-5",
        id: "msg_test",
        type: "message",
        role: "assistant",
        content: [],
        stop_reason: null,
        usage: { input_tokens: 100, output_tokens: 1 },
      },
    },
    { type: "content_block_start", index: 0, content_block: { type: "text", text: "" } },
    { type: "content_block_delta", index: 0, delta: { type: "text_delta", text } },
    { type: "content_block_stop", index: 0 },
    { type: "message_delta", delta: { stop_reason: "end_turn" }, usage: { output_tokens: 20 } },
    { type: "message_stop" },
  ];
  return new Response(
    events.map((event) => `event: ${event.type}\ndata: ${JSON.stringify(event)}\n\n`).join(""),
    {
      status: 200,
      headers: { "content-type": "text/event-stream" },
    }
  );
}

/** A gateway whose fake fetch answers each body with `answer(body, n)` (n from 1), and the bodies
 *  sent, in order. One call is in flight at a time, so they go in the order started. */
function world(answer: (body: MessagesBody, n: number) => Response, budgetUsd?: number) {
  const bodies: MessagesBody[] = [];
  const keyFile = path.join(scratch, `key-${Math.random()}`);
  writeFileSync(keyFile, fixtureSecret("gw"), { mode: 0o600 });
  const gateway = createGateway({
    baseUrl: "http://gateway.test",
    keyFile,
    concurrency: 1,
    timeoutMs: 5_000,
    ...(budgetUsd === undefined ? {} : { budgetUsd }),
    fetch: async (_url, init) => {
      const body = JSON.parse(String(init.body)) as MessagesBody;
      bodies.push(body);
      return answer(body, bodies.length);
    },
    sleep: async () => {},
  });
  return { gateway, bodies };
}

const PASS = JSON.stringify({ verdict: "pass", reason: "It hands the work to a task agent." });
const FAIL = JSON.stringify({ verdict: "fail", reason: "It does nothing yet." });

/** The user message's blocks of a judge body: the case's part, then the step. */
function blocksOf(body: MessagesBody | undefined): string[] {
  const [message] = UserMessage.parse(body?.messages);
  return message.content.map((block) => block.text);
}

/** The step a judge body holds, less its tags. */
function stepOf(body: MessagesBody | undefined): string {
  return (blocksOf(body)[1] ?? "").replace(/^<step>\n/, "").replace(/\n<\/step>$/, "");
}

/** A judge body less its `metadata`, the session id each call draws for itself. */
function withoutId(body: MessagesBody | undefined): Record<string, unknown> {
  const { metadata: _metadata, ...rest } = body ?? { model: "", messages: [] };
  return rest;
}

/** The session id of each body, as `metadata.user_id` names it. */
function sessionIds(bodies: readonly MessagesBody[]): unknown[] {
  return bodies.map((body) => JSON.parse(String(Object(body.metadata).user_id)).session_id);
}

/** The replies of each arm: F5's text, thinking, and skill read with another call. */
function armReplies(arms: readonly ArmName[]): ArmReply[] {
  const shapes = ["text", "thinking", "skill-read-and-other"];
  return arms.flatMap((arm) =>
    shapes.map((shape, run) => ({ arm, run, content: f5Message(shape).content }))
  );
}

test("the request names no arm and no path, and holds no text of an arm's skill files but the case's line", async () => {
  const dirs = { old: oldDir, new: newDir };
  const arms: Arm[] = (["old", "new", "none", "minus"] as const).map((name) =>
    buildArm(f1Case, name, dirs, path.join(scratch, `arm-${name}`))
  );
  const replies = armReplies(arms.map((arm) => arm.name));
  const { gateway, bodies } = world(() => sse(PASS));
  const seed = "seed-paths";
  const { judged } = await judgeBlind(f1Case, replies, gateway, seed);

  expect(bodies).toHaveLength(replies.length);
  expect(judged.map((each) => each.verdict)).toEqual(replies.map(() => "pass"));
  // Every request holds the same case part and the same system prompt; only the step differs, and
  // the step is the judged reply as stepText renders it, in the shuffled order.
  const [first] = bodies;
  const shared = blocksOf(first)[0] ?? "";
  for (const [place, body] of bodies.entries()) {
    expect(Object.keys(body).sort()).toEqual(
      [
        "max_tokens",
        "messages",
        "metadata",
        "model",
        "output_config",
        "stream",
        "system",
        "temperature",
        "thinking",
      ].sort()
    );
    expect(body.system).toEqual(first?.system);
    expect(blocksOf(body)).toHaveLength(2);
    expect(blocksOf(body)[0]).toBe(shared);
    const reply = replies.find(
      (each) => each.arm === judged[place]?.arm && each.run === judged[place]?.run
    );
    expect(stepOf(body)).toBe(stepText(reply?.content ?? []));
  }
  // Each judgement went with a session id of its own, a UUID in Oh My Pi's shape, and its
  // metadata holds that id alone.
  const ids = sessionIds(bodies);
  expect(new Set(ids).size).toBe(bodies.length);
  for (const [index, id] of ids.entries()) {
    expect(String(id)).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/);
    expect(bodies[index]?.metadata).toEqual({ user_id: JSON.stringify({ session_id: id }) });
  }

  // What the runner puts in a request (all of it but the step) names no arm, no path of an arm's
  // files, and holds none of their lines but the case's line, once.
  const ours = `${JSON.stringify(first?.system)}\n${shared}`;
  expect(shared).not.toMatch(/\b(old|new|none|minus)\b/i);
  const paths = [scratch, oldDir, newDir, INSTALLED, ...arms.flatMap((arm) => arm.skillDir ?? [])];
  for (const each of new Set(paths)) expect(JSON.stringify(bodies)).not.toContain(each);
  expect(JSON.stringify(bodies)).not.toContain("[Skill file:");
  const armLines = arms
    .flatMap((arm) => (arm.skillDir === undefined ? [] : filesUnder(arm.skillDir)))
    .flatMap((text) => text.split("\n"))
    .map((line) => line.trim())
    .filter((line) => line.length >= 4 && line !== LINE);
  expect(armLines.length).toBeGreaterThan(10);
  for (const line of new Set(armLines)) expect(ours).not.toContain(line);
  expect(shared.split(LINE)).toHaveLength(2);
  // No opaque id, and no thinking of a reply, reaches the judge.
  for (const { id } of judged) expect(JSON.stringify(bodies)).not.toContain(id);
  const thinking = f5Message("thinking").content.find((block) => block.type === "thinking");
  expect(typeof thinking?.thinking).toBe("string");
  expect(JSON.stringify(bodies)).not.toContain(String(thinking?.thinking).slice(0, 60));
});

test("the messages before the step: the last ones, the last user message, skill reads left out, long texts cut", () => {
  const user = (text: string): SessionMessage => ({
    role: "user",
    content: [{ type: "text", text }],
  });
  const call = (id: string, target: string): SessionMessage => ({
    role: "assistant",
    content: [{ type: "toolCall", id, name: "read", arguments: { path: target } }],
  });
  const result = (id: string, text: string): SessionMessage => ({
    role: "toolResult",
    toolCallId: id,
    toolName: "read",
    isError: false,
    content: [{ type: "text", text }],
  });
  const long = `${"a".repeat(MESSAGE_CHARS)}${"b".repeat(1000)}`;
  const session: SessionMessage[] = [
    user("the first prompt"),
    user("the task: build the alpha checklist"),
    call("r1", "skill://alpha"),
    result("r1", "[Skill file: /skills/alpha/SKILL.md]\nalpha's own text"),
  ];
  for (let n = 0; n < 6; n += 1) {
    session.push(
      call(`o${n}`, `/work/file-${n}.txt`),
      result(`o${n}`, n === 5 ? long : `file ${n}`)
    );
  }
  session.push(
    call("s1", "skill://alpha:1-3"),
    result("s1", "alpha's selected lines"),
    call("b1", "skill://beta"),
    result("b1", "[Skill file: /skills/beta/SKILL.md]\nbeta's own text")
  );
  expect(session.length).toBeGreaterThan(BEFORE_MESSAGES + 2);

  const before = beforeText({ messages: session, skill: "alpha", skillReads: [3] });
  const parts = before.split("\n\n");
  // The last user message comes first, after a note of the one message before it; then a note of
  // the messages between it and the last BEFORE_MESSAGES.
  expect(parts[0]).toBe("[… 1 message left out …]");
  expect(parts[1]).toBe("[user]\nthe task: build the alpha checklist");
  const between = session.length - BEFORE_MESSAGES - 2;
  expect(parts[2]).toBe(`[… ${between} messages left out …]`);
  expect(parts.length).toBe(3 + BEFORE_MESSAGES);
  // A read of the skill with a selector is left out though skillReads does not hold it; a read of
  // another skill keeps its text, less its header's path.
  expect(before).not.toContain("alpha's selected lines");
  expect(before).toContain("[tool result: read]\n[the text of a file of the skill, left out]");
  expect(before).toContain("[tool result: read]\nbeta's own text");
  expect(before).not.toContain("/skills/");
  // A long text keeps its start and its end, and says how much it left out.
  const cut = parts.find((part) => part.includes("characters left out"));
  expect(cut).toContain(
    `[… ${long.length + "[tool result: read]\n".length - MESSAGE_CHARS} characters left out …]`
  );
  expect(cut?.startsWith("[tool result: read]\naaa")).toBe(true);
  expect(cut?.endsWith("bbb")).toBe(true);
  // The read of skill://alpha before the window is gone with the messages left out.
  expect(before).not.toContain("alpha's own text");
});

test("an unparsed judgement is judged again twice and then gives null, never fail", async () => {
  const input = {
    criterion: f1Case.criterion,
    line: LINE,
    before: beforeText(f1Case),
    reply: NOTHING_REPLY,
  };

  const never = world(() => sse("I would fail this step."));
  const unparsed = await judgeReply(input, never.gateway);
  expect(JUDGE_TRIES).toBe(3);
  expect(never.bodies).toHaveLength(JUDGE_TRIES);
  // Each try is the same request but for a session id of its own.
  expect(withoutId(never.bodies[1])).toEqual(withoutId(never.bodies[0]));
  expect(withoutId(never.bodies[2])).toEqual(withoutId(never.bodies[0]));
  expect(new Set(sessionIds(never.bodies)).size).toBe(JUDGE_TRIES);
  expect(unparsed.verdict).toBeNull();
  expect(unparsed.reason).toContain("the judgement did not parse on 3 tries");
  expect(unparsed.spent).toHaveLength(3);

  // A verdict outside pass and fail does not parse either; a fenced answer does.
  const answers = [
    "not JSON",
    JSON.stringify({ verdict: "maybe", reason: "unsure" }),
    `\`\`\`json\n${FAIL}\n\`\`\``,
  ];
  const late = world((_body, n) => sse(answers[n - 1] ?? PASS));
  const third = await judgeReply(input, late.gateway);
  expect(late.bodies).toHaveLength(3);
  expect(third).toMatchObject({ verdict: "fail", reason: "It does nothing yet." });

  const once = world(() => sse(PASS));
  expect((await judgeReply(input, once.gateway)).verdict).toBe("pass");
  expect(once.bodies).toHaveLength(1);

  // Judged blind, an unparsed judgement is a null verdict too, and a call the budget refuses is
  // marked so: neither is a fail.
  const replies = armReplies(["old", "none"]);
  const blind = await judgeBlind(f1Case, replies, never.gateway, "seed-null");
  expect(blind.judged.map((each) => [each.verdict, each.budgetRefused])).toEqual(
    replies.map(() => [null, false])
  );
  const spent = world(() => sse(PASS), 0);
  const refused = await judgeBlind(f1Case, replies, spent.gateway, "seed-budget");
  expect(spent.bodies).toHaveLength(0);
  expect(refused.judged.map((each) => [each.verdict, each.budgetRefused])).toEqual(
    replies.map(() => [null, true])
  );
  expect(refused.judged[0]?.reason).toContain("the budget of $0 is spent");
});

test("the pinned settings reach every request", async () => {
  const header = parsePrompt(
    "judge",
    readFileSync(path.join(PROMPTS_DIR, "judge.md"), "utf8")
  ).settings;
  expect(header).toEqual({
    model: "claude-opus-5-5",
    thinking_budget: 4000,
    max_tokens: 8000,
    temperature: 1,
    effort: "high",
  });
  // Each request of a judgement asked again, of the controls and of a blind judging.
  const { gateway, bodies } = world((_body, n) => sse(n === 1 ? "unparsed" : PASS));
  await judgeReply({ criterion: f1Case.criterion, line: LINE, before: "", reply: "r" }, gateway);
  await judgeControls(f1Case, gateway);
  await judgeBlind(f1Case, armReplies(["old", "new"]), gateway, newSeed());
  expect(bodies).toHaveLength(2 + 3 + 6);
  for (const body of bodies) {
    expect(body.model).toBe(header.model);
    expect(body.max_tokens).toBe(header.max_tokens);
    expect(body.temperature).toBe(header.temperature);
    expect(body.thinking).toEqual({ type: "enabled", budget_tokens: header.thinking_budget });
    expect(body.output_config).toEqual({ effort: header.effort });
    expect(body.stream).toBe(true);
  }
});

test("the shuffle is stable under a recorded seed and ids are opaque", async () => {
  const items = (["old", "new", "none"] as const).flatMap((arm) =>
    Array.from({ length: 10 }, (_, run) => ({ arm, run }))
  );
  const once = blindOrder(items, "seed-1");
  expect(blindOrder(items, "seed-1")).toEqual(once);
  // A shuffle: every item once, not in the order given, and another seed gives another order.
  expect(
    once
      .map(({ item }) => item)
      .sort((a, b) => `${a.arm}${a.run}`.localeCompare(`${b.arm}${b.run}`))
  ).toEqual([...items].sort((a, b) => `${a.arm}${a.run}`.localeCompare(`${b.arm}${b.run}`)));
  expect(once.map(({ item }) => item)).not.toEqual(items);
  const other = blindOrder(items, "seed-2");
  expect(other.map(({ item }) => item)).not.toEqual(once.map(({ item }) => item));
  // An id is 12 hex digits that depend on the seed and the place alone: the same items' ids under
  // another seed differ, and other items under the same seed get the same ids.
  const ids = once.map(({ id }) => id);
  for (const id of ids) expect(id).toMatch(/^[0-9a-f]{12}$/);
  expect(new Set(ids).size).toBe(items.length);
  expect(blindOrder(["x", "y", "z"], "seed-1").map(({ id }) => id)).toEqual(ids.slice(0, 3));
  expect(other.map(({ id }) => id).filter((id) => ids.includes(id))).toEqual([]);

  // judgeBlind judges in that order under the recorded seed, and names each reply by its id.
  const replies = armReplies(["old", "new", "none"]);
  const { gateway, bodies } = world(() => sse(PASS));
  const blind = await judgeBlind(f1Case, replies, gateway, "seed-1");
  expect(blind.seed).toBe("seed-1");
  const order = blindOrder(replies, "seed-1");
  expect(blind.judged.map(({ id, arm, run }) => ({ id, arm, run }))).toEqual(
    order.map(({ id, item }) => ({ id, arm: item.arm, run: item.run }))
  );
  expect(bodies.map(stepOf)).toEqual(order.map(({ item }) => stepText(item.content)));
  expect(newSeed()).toMatch(/^[0-9a-f]{32}$/);
  expect(newSeed()).not.toBe(newSeed());
});

test("the controls: the real step must pass, and the violating step and the reply that does nothing must fail", async () => {
  const real = stepText(f1Case.realStep.content ?? []);
  const violating = stepText(f1Case.violatingStep.content ?? []);
  /** A judge that gives each step the verdict `verdicts` names for it, and fails any other. */
  const judging = (verdicts: Record<string, "pass" | "fail">) => (body: MessagesBody) =>
    sse(verdicts[stepOf(body)] === "pass" ? PASS : FAIL);
  const { gateway, bodies } = world(judging({ [real]: "pass" }));
  const controls = await judgeControls(f1Case, gateway);
  expect(controls.hold).toBe(true);
  expect(controls.failed).toEqual([]);
  expect(
    [controls.realStep, controls.violating, controls.nothing].map((each) => each.verdict)
  ).toEqual(["pass", "fail", "fail"]);
  // The real step is F1's own `task` call, and the violating step the case's own read of
  // skill://beta, each rendered as a replayed reply's would be; every control reads the same
  // case part.
  expect(bodies.map(stepOf)).toEqual([real, violating, NOTHING_REPLY]);
  expect(stepOf(bodies[0])).toContain("[tool call: task] ");
  expect(stepOf(bodies[1])).toBe('Step 4.\n[tool call: read] {"path":"skill://beta"}');
  expect(new Set(bodies.map((body) => blocksOf(body)[0])).size).toBe(1);

  // Seen failing, each control by itself and all at once: a judge that passes the violating step,
  // one that fails the real step, one that passes every reply, and one whose judgement never
  // parses, which is no verdict and holds no control.
  const failedBy = async (answer: (body: MessagesBody) => Response) => {
    const judged = await judgeControls(f1Case, world(answer).gateway);
    return { hold: judged.hold, failed: judged.failed };
  };
  expect(await failedBy(judging({ [real]: "pass", [violating]: "pass" }))).toEqual({
    hold: false,
    failed: ["violating"],
  });
  expect(await failedBy(judging({}))).toEqual({ hold: false, failed: ["realStep"] });
  expect(await failedBy(() => sse(PASS))).toEqual({
    hold: false,
    failed: ["violating", "nothing"],
  });
  expect(await failedBy(() => sse("no answer"))).toEqual({
    hold: false,
    failed: ["realStep", "violating", "nothing"],
  });
});

test("a case's first judgement goes alone and the rest after it at once, in the blind order; the controls' real step goes first", async () => {
  const reply = await world(() => sse(PASS)).gateway.send(
    judgeBody({ criterion: f1Case.criterion, line: LINE, before: "", reply: "r" })
  );
  const events: string[] = [];
  const bodies: MessagesBody[] = [];
  // A gateway that notes when each call starts and ends, and answers each with a pass.
  const gateway: Gateway = {
    async send(body) {
      bodies.push(body as MessagesBody);
      const n = bodies.length;
      events.push(`start ${n}`);
      await Promise.resolve();
      events.push(`end ${n}`);
      return reply;
    },
    spend: () => ({ models: {}, usd: 0 }),
  };
  const replies = armReplies(["old", "new", "none"]);
  const blind = await judgeBlind(f1Case, replies, gateway, "seed-first");
  expect(blind.judged.map((each) => each.verdict)).toEqual(replies.map(() => "pass"));
  const rest = replies.length - 1;
  expect(events.slice(0, 2 + rest)).toEqual([
    "start 1",
    "end 1",
    ...Array.from({ length: rest }, (_, index) => `start ${index + 2}`),
  ]);
  expect(bodies.map(stepOf)).toEqual(
    blindOrder(replies, "seed-first").map(({ item }) => stepText(item.content))
  );
  expect(new Set(bodies.map((body) => blocksOf(body)[0])).size).toBe(1);

  events.length = 0;
  bodies.length = 0;
  await judgeControls(f1Case, gateway);
  expect(events.slice(0, 4)).toEqual(["start 1", "end 1", "start 2", "start 3"]);
  expect(bodies.map(stepOf)).toEqual([
    stepText(f1Case.realStep.content ?? []),
    stepText(f1Case.violatingStep.content ?? []),
    NOTHING_REPLY,
  ]);
});

test("on F5: the gateway's judge reply gives its verdict and reason", async () => {
  // F2's replayed history (testdata/CAPTURE.md, "F2"): the user's request, the read of
  // skill://alpha, and F1's result of it; the step is F5-text's reply.
  const history: SessionMessage[] = [
    {
      role: "user",
      content: [
        {
          type: "text",
          text: "Write the alpha checklist of the made-up fixture project. Use the alpha skill.",
        },
      ],
    },
    {
      role: "assistant",
      content: [
        {
          type: "toolCall",
          id: "toolu_replay_alpha_1",
          name: "read",
          arguments: { path: "skill://alpha" },
        },
      ],
    },
    { ...messages[2], toolCallId: "toolu_replay_alpha_1" },
  ];
  const { gateway, bodies } = world(() => f5("judge"));
  const judged = await judgeReply(
    {
      criterion: {
        pass: "The step reads references/x.md (a read of skill://alpha/references/x.md) before it writes any alpha step or its result.",
        fail: "The step writes the alpha steps or their results without reading references/x.md first.",
      },
      line: "1. Read `references/x.md` before the first step.",
      before: beforeText({ messages: history, skill: "alpha", skillReads: [2] }),
      reply: stepText(f5Message("text").content),
    },
    gateway
  );
  expect(bodies).toHaveLength(1);
  expect(judged.verdict).toBe("fail");
  expect(judged.reason).toContain("references/x.md");
  expect(judged.spent.map((spent) => [spent.model, spent.cacheWrite])).toEqual([
    ["claude-opus-5-5", 710],
  ]);
  // The fixture's reply holds a thinking block before its answer: the answer is the text alone.
  expect(f5Message("judge").content.map((block) => block.type)).toEqual(["thinking", "text"]);
});

test("an OpenAI reply renders as the matching Messages reply does, so the judge's request has one shape whichever model answered (F9)", async () => {
  const shapes = ["text", "skill-read", "skill-read-and-other"];
  for (const shape of shapes) {
    const content = f9Content(shape);
    expect(stepText(content)).toBe(stepText(asMessagesReply(content)));
  }
  // The text reply's reasoning is left out, as thinking is, and its message is its text.
  expect(stepText(f9Content("text"))).toBe(
    "1. Read `references/x.md`.\n2. Write each step’s result on one line.\n3. Stop after the third step."
  );
  expect(stepText(f9Content("skill-read"))).toBe(
    '[tool call: read] {"i":"Reading alpha checklist reference","path":"skill://alpha/references/x.md"}'
  );
  // A call with no arguments reads as a tool_use with no input; arguments that do not parse as
  // they were sent.
  expect(stepText([{ type: "function_call", name: "ls", arguments: "", call_id: "call_1" }])).toBe(
    stepText([{ type: "tool_use", name: "ls", id: "toolu_1", input: {} }])
  );
  expect(stepText([{ type: "function_call", name: "ls", arguments: "{not json" }])).toBe(
    "[tool call: ls] {not json"
  );

  // The judge's requests for an OpenAI reply and for its matching Messages reply are the same:
  // the judge's pinned model and prompt, naming no model, route or arm of the replay.
  const { gateway, bodies } = world(() => sse(PASS));
  const replies: ArmReply[] = shapes.flatMap((shape, run) => [
    { arm: "old", run, content: f9Content(shape) },
    { arm: "new", run, content: asMessagesReply(f9Content(shape)) },
  ]);
  const { judged } = await judgeBlind(f1Case, replies, gateway, "seed-routes");
  expect(judged.map((each) => each.verdict)).toEqual(replies.map(() => "pass"));
  for (const run of shapes.keys()) {
    const bodyOf = (arm: ArmName) =>
      bodies[judged.findIndex((each) => each.arm === arm && each.run === run)];
    expect(withoutId(bodyOf("old"))).toEqual(withoutId(bodyOf("new")));
  }
  const sent = JSON.stringify(bodies);
  expect(bodies.every((body) => body.model === "claude-opus-5-5")).toBe(true);
  for (const said of ["gpt-6.1-sol", "openai", "/v1/responses", "function_call", "reasoning"]) {
    expect(sent).not.toContain(said);
  }
  const reasoning = f9Content("text").find((item) => item.type === "reasoning");
  expect(sent).not.toContain(String(reasoning?.encrypted_content).slice(0, 40));
});
