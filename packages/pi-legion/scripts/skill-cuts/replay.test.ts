import { expect, test } from "bun:test";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { type Arm, buildArm, type InstalledCopy, snapshotTag } from "./arms";
import { type Case, pathHash } from "./cases";
import type { CheckoutCopy } from "./checkout";
import {
  bodyIdentity,
  type ContentBlock,
  createGateway,
  freshIdentity,
  type Gateway,
  isResponsesBody,
  type MessagesBody,
  type Reply,
  type RequestBody,
  type ResponsesBody,
  toolCalls,
} from "./gateway";
import { judgeReply, stepText } from "./judge";
import { messageSse, sseResponse } from "./recorder";
import {
  MAX_BREAKPOINTS,
  MAX_CHECKOUT_ROUNDS,
  MAX_SKILL_ROUNDS,
  READS_PAST_LAST_ROUND,
  ReadsPastLastRound,
  type Replayed,
  replayInterleaved,
  replayOnce,
  sendOrder,
  UNANSWERED_READ,
  UnansweredRead,
  withCacheBreakpoint,
} from "./replay";
import { pinNatives } from "./test-natives";
import { detachedCommit, f9Completed, fixtureReply, writeTree } from "./testdata/fixtures";

// The replay of an arm, on F5 and F9 (testdata/): the model gateway's replies to request bodies
// made from F2 and F8, captured as testdata/CAPTURE.md says ("F5", and "F8 and F9") with
// `bun send.ts <body.json> > F5-<shape>.sse` and `bun send-openai.ts <body.json> >
// F9-<shape>.sse`, scratch scripts that post the body to the gateway's `/v1/messages` and
// `/v1/responses`. F5-skill-read's only tool call reads skill://alpha/references/x.md;
// F5-skill-read-and-other holds a read of skill://beta, a `bash` call and a read of
// skill://alpha/references/x.md; F5-text holds text only. F9's are the Responses API's
// (openai/gpt-6.1-sol's) replies: F9-skill-read's only tool call reads
// skill://alpha/references/x.md; F9-skill-read-and-other holds a read of skill://beta and a
// `bash` call; F9-text a reasoning item and a message. The gateway's fetch is a fake that answers
// with them: no test reaches a network.

const TESTDATA = path.join(import.meta.dir, "testdata");
const scratch = temporaryDirectory("skill-cuts-replay", { real: true });
/** The `test` of the rows whose reads need the pinned Oh My Pi's native addon (a window that leaves
 *  lines of a file out, for its block context, or a code file the pin may summarize): skipped, or
 *  under GITHUB_ACTIONS failing, where the runner finds none (test-natives.ts). */
const natives = pinNatives("replay.test.ts");

/** The body the arm's capture recorded: F2, a replayed history Oh My Pi 18.18.6 sent. */
const BODY = JSON.parse(
  readFileSync(path.join(TESTDATA, "F2-request.json"), "utf8")
) as MessagesBody;

/** The Responses body of an arm on openai/gpt-6.1-sol: F8, the F2-like history Oh My Pi 18.18.6
 *  sent for that model. */
const RESPONSES_BODY = JSON.parse(
  readFileSync(path.join(TESTDATA, "F8-request.json"), "utf8")
) as ResponsesBody;

const SKILL_MD =
  "---\nname: alpha\ndescription: Use when a task asks for the alpha checklist of the made-up fixture project; it lists the three alpha steps.\n---\n\n# Alpha\n\n1. Read `references/x.md` before the first step.\n2. Write each step's result on one line.\n3. Stop after the third step.\n";
const X_MD = "# X\n\nEach alpha step's result is one short line.\n";

const PROFILE_SKILLS = ["alpha", "beta"].map((name) => ({
  repo: "acme/skills",
  commit: "0123456789abcdef0123456789abcdef01234567",
  path: `skills/${name}`,
  name,
}));

/** The skill's directory as the model sees it, the same in every arm. */
const INSTALLED = "/home/agent/.omp/agent/skills/alpha";

/** An arm of skill alpha whose directory holds `files`. */
function armWith(name: "old" | "new" | "minus" | "ours", files: Record<string, string>): Arm {
  const skillDir = path.join(scratch, `${name}-${Object.keys(files).length}-${Math.random()}`);
  for (const [file, text] of Object.entries(files)) {
    mkdirSync(path.dirname(path.join(skillDir, file)), { recursive: true });
    writeFileSync(path.join(skillDir, file), text);
  }
  return {
    name,
    skill: "alpha",
    skillDir,
    installedDir: INSTALLED,
    messages: [],
    profileSkills: PROFILE_SKILLS,
  };
}

/** F5's reply `shape`, a Messages API stream. */
function f5(shape: string): Response {
  return fixtureReply(`F5-${shape}`);
}

/** F9's reply `shape`, a Responses API stream. */
function f9(shape: string): Response {
  return fixtureReply(`F9-${shape}`);
}

/** A gateway whose fake fetch answers the n-th call (from 1) with `answer`, and the bodies and
 *  headers sent. */
function world(answer: (n: number) => Response) {
  const bodies: MessagesBody[] = [];
  const headers: Headers[] = [];
  const keyFile = path.join(scratch, `key-${Math.random()}`);
  writeFileSync(keyFile, fixtureSecret("gw"), { mode: 0o600 });
  const gateway = createGateway({
    baseUrl: "http://gateway.test",
    keyFile,
    concurrency: 2,
    timeoutMs: 5_000,
    spendRecorder: null,
    fetch: async (_url, init) => {
      bodies.push(JSON.parse(String(init.body)));
      headers.push(new Headers(init.headers));
      return answer(bodies.length);
    },
    sleep: async () => {},
  });
  return { gateway, bodies, headers };
}

/** The `anthropic-*` headers Oh My Pi 18.18.6 sent beside a body at `--thinking low` (F2), as a
 *  capture keeps them. */
const CAPTURED_HEADERS: Record<string, string> = Object.fromEntries(
  Object.entries(
    JSON.parse(readFileSync(path.join(TESTDATA, "F2-request-thinking-low.headers.json"), "utf8"))
      .headers
  ).filter((entry): entry is [string, string] => entry[0].startsWith("anthropic-"))
);

/** F5-skill-read's one tool call. */
const SKILL_READ_CALL = {
  type: "tool_use",
  id: "toolu_01WDxLVjvqwiht3W6X8n4wAn",
  name: "read",
  input: { path: "skill://alpha/references/x.md", i: "Reading alpha reference" },
  caller: { type: "direct" },
};

/** Where `value` holds cache breakpoints: `tools.10`, `system.0`, `messages.2.content.0`. */
function breakpointPaths(value: unknown, at = ""): string[] {
  const under = (key: string | number) => (at === "" ? `${key}` : `${at}.${key}`);
  if (Array.isArray(value))
    return value.flatMap((item, index) => breakpointPaths(item, under(index)));
  if (typeof value !== "object" || value === null) return [];
  return [
    ...("cache_control" in value ? [at] : []),
    ...Object.entries(value).flatMap(([key, field]) =>
      key === "cache_control" ? [] : breakpointPaths(field, under(key))
    ),
  ];
}

/** A copy of `body` to change in a test. */
function copyOf(body: MessagesBody) {
  return JSON.parse(JSON.stringify(body));
}

/** `body` with each breakpoint of F2's (5 minutes) made an hour long, as the pilot's bodies, which
 *  Oh My Pi built for claude-opus-5-5, hold them. */
function hourLong(body: MessagesBody): MessagesBody {
  return JSON.parse(
    JSON.stringify(body).replaceAll(
      '"cache_control":{"type":"ephemeral"}',
      '"cache_control":{"type":"ephemeral","ttl":"1h"}'
    )
  );
}

/** The last block of `body`'s last message. */
function lastBlock(body: MessagesBody): Record<string, unknown> {
  const content = Object(body.messages.at(-1)).content;
  const block = Array.isArray(content) ? content.at(-1) : undefined;
  if (typeof block !== "object" || block === null) throw new Error("the last message has no block");
  return block;
}

/** `body` with the session id `id`: what a run sends of it, its id its own. */
function withId<T extends RequestBody>(body: T, id: string | null): T {
  return isResponsesBody(body)
    ? { ...body, prompt_cache_key: id }
    : { ...body, metadata: { ...Object(body.metadata), user_id: id } };
}

test("answers skill reads and stops at the first other call", async () => {
  const arm = armWith("old", { "SKILL.md": SKILL_MD, "references/x.md": X_MD });
  const { gateway, bodies } = world((n) => f5(n === 1 ? "skill-read" : "text"));
  const replayed = await replayOnce(BODY, arm, gateway);

  expect(bodies).toHaveLength(2);
  // Each send is the body with a cache breakpoint on its last block (withCacheBreakpoint) and a
  // session id of the run's own, in the captured id's shape; both rounds go with that one id.
  const cached = withCacheBreakpoint(BODY);
  const id = bodyIdentity(bodies[0] ?? BODY);
  expect(id).not.toBe(bodyIdentity(BODY));
  expect(Object.keys(JSON.parse(id ?? "{}"))).toEqual(
    Object.keys(JSON.parse(bodyIdentity(BODY) ?? ""))
  );
  expect(JSON.parse(id ?? "{}").session_id).toMatch(
    /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/
  );
  expect(replayed.userId).toBe(id);
  expect(bodies[0]).toEqual(withId(cached, id));
  expect(bodyIdentity(bodies[1] ?? BODY)).toBe(id);
  // The second send is the same body with the reply and the arm's answer to its read added.
  const { messages: sentMessages, ...sentFields } = bodies[1] ?? BODY;
  const { messages: bodyMessages, ...bodyFields } = withId(cached, id);
  expect(sentFields).toEqual(bodyFields);
  expect(sentMessages).toEqual([
    ...bodyMessages,
    { role: "assistant", content: [SKILL_READ_CALL] },
    {
      role: "user",
      content: [
        {
          type: "tool_result",
          tool_use_id: SKILL_READ_CALL.id,
          content: [
            {
              type: "text",
              text: `[Skill file: ${INSTALLED}/references/x.md]\n${X_MD.replace(/\n$/, "")}`,
            },
          ],
          is_error: false,
        },
      ],
    },
  ]);
  // The answer shows the installed path, the same in every arm: the arm's own directory is in no
  // body sent.
  expect(JSON.stringify(bodies)).not.toContain(arm.skillDir ?? "no directory");

  expect(replayed.rounds).toBe(1);
  expect(replayed.reads).toEqual([
    [{ skill: "alpha", file: "references/x.md", by: "skill", from: "arm", isError: false }],
  ]);
  // The judged step is the answered round's read call, without its answer, then the reply that
  // holds no other call.
  expect(replayed.step.message.stop_reason).toBe("end_turn");
  expect(replayed.step.message.content.map((block) => block.type)).toEqual(["tool_use", "text"]);
  expect(replayed.step.message.content[0]).toEqual(SKILL_READ_CALL);
  expect(replayed.usage.map((spent) => [spent.model, spent.output])).toEqual([
    ["claude-sonnet-5-5", 84],
    ["claude-sonnet-5-5", 96],
  ]);
});

test("a reply with a skill read and another tool call is the judged step at once", async () => {
  const arm = armWith("old", { "SKILL.md": SKILL_MD, "references/x.md": X_MD });
  const { gateway, bodies } = world(() => f5("skill-read-and-other"));
  const replayed = await replayOnce(BODY, arm, gateway);
  expect(bodies).toHaveLength(1);
  expect(replayed.rounds).toBe(0);
  expect(replayed.step.message.content.map((block) => block.name ?? block.type)).toEqual([
    "text",
    "read",
    "bash",
    "read",
  ]);
  expect(replayed.usage).toHaveLength(1);
});

test("a read of a directory of the skill is the judged step: the arm answers files only", async () => {
  const arm = armWith("old", { "SKILL.md": SKILL_MD, "references/x.md": X_MD });
  // A body that does not stream gets one JSON message.
  const reply = {
    id: "msg_made_up",
    type: "message",
    role: "assistant",
    model: "claude-sonnet-5-5",
    content: [
      {
        type: "tool_use",
        id: "toolu_dir",
        name: "read",
        input: { path: "skill://alpha/references" },
      },
    ],
    stop_reason: "tool_use",
    usage: { input_tokens: 10, output_tokens: 5 },
  };
  const { gateway, bodies } = world(() => Response.json(reply));
  const replayed = await replayOnce({ ...BODY, stream: false }, arm, gateway);
  expect(bodies).toHaveLength(1);
  expect(replayed.rounds).toBe(0);
  expect(replayed.step.message.content).toEqual(reply.content);
});

// Catches: a reply after the last answered round that still only reads skill files taken for the
// judged step, which fails in every arm for a read alone.
test("answers at most three rounds of skill reads; a reply after the last that still only reads skill files gets no verdict", async () => {
  const arm = armWith("old", { "SKILL.md": SKILL_MD, "references/x.md": X_MD });
  const { gateway, bodies } = world(() => f5("skill-read"));
  const failed = await replayOnce(BODY, arm, gateway).then(
    () => "judged",
    (error: unknown) => error
  );
  expect(MAX_SKILL_ROUNDS).toBe(3);
  expect(bodies).toHaveLength(4);
  expect(bodies.map((body) => body.messages.length)).toEqual([3, 5, 7, 9]);
  // Every round keeps the one breakpoint on the body's own last block: the prefix all runs share.
  for (const sent of bodies) {
    expect(breakpointPaths(sent).sort()).toEqual(["messages.2.content.0", "system.0", "tools.10"]);
  }
  if (!(failed instanceof ReadsPastLastRound)) throw new Error(`not refused: ${String(failed)}`);
  expect(failed.message).toBe(
    `${READS_PAST_LAST_ROUND}: after 3 answered rounds of skill reads, the reply still only reads skill files (alpha references/x.md)`
  );
  const read = {
    skill: "alpha",
    file: "references/x.md",
    by: "skill",
    from: "arm",
    isError: false,
  } as const;
  expect(failed.replayed.rounds).toBe(3);
  expect(failed.replayed.reads).toEqual([[read], [read], [read]]);
  expect(failed.replayed.usage).toHaveLength(4);
  expect(failed.replayed.step.message.content).toEqual(Array(4).fill(SKILL_READ_CALL));

  const once = world(() => f5("skill-read"));
  await expect(replayOnce(BODY, arm, once.gateway, { maxSkillRounds: 1 })).rejects.toThrow(
    `${READS_PAST_LAST_ROUND}: after 1 answered rounds`
  );
  expect(once.bodies).toHaveLength(2);

  // In an interleaved round such a run's outcome is that refusal; another arm's run keeps its
  // step. The old arm's every reply reads x.md, the new arm's holds text.
  const reads = await world(() => f5("skill-read")).gateway.send(BODY);
  const text = await world(() => f5("text")).gateway.send(BODY);
  const byArm: Gateway = {
    send: async (body) => (body.x_arm === "old" ? reads : text),
    spend: () => ({ models: {}, usd: 0 }),
  };
  const settled = await replayInterleaved(
    (["old", "new"] as const).map((name) => ({
      arm: armWith(name, { "SKILL.md": SKILL_MD, "references/x.md": X_MD }),
      body: { ...BODY, x_arm: name },
      runs: 1,
    })),
    byArm,
    "past-seed"
  );
  const outcomes = new Map(settled.map((run) => [run.arm, run.outcome]));
  const refused = outcomes.get("old");
  expect(refused?.status === "rejected" && refused.reason instanceof ReadsPastLastRound).toBe(true);
  expect(outcomes.get("new")).toMatchObject({ status: "fulfilled", value: { rounds: 0 } });
});

test("sends the captured anthropic-* headers with the body on every round", async () => {
  const arm = armWith("old", { "SKILL.md": SKILL_MD, "references/x.md": X_MD });
  expect(CAPTURED_HEADERS["anthropic-beta"]).toContain("context-management-2025-06-27");
  const { gateway, headers } = world((n) => f5(n <= 3 ? "skill-read" : "text"));
  const replayed = await replayOnce(BODY, arm, gateway, { headers: CAPTURED_HEADERS });
  expect(replayed.rounds).toBe(3);
  expect(headers).toHaveLength(4);
  for (const sent of headers) {
    for (const [name, value] of Object.entries(CAPTURED_HEADERS)) {
      expect(sent.get(name)).toBe(value);
    }
  }

  // An interleaved round gives each of its runs the same headers.
  const runs = world(() => f5("text"));
  const settled = await replayInterleaved(
    [{ arm, body: BODY, headers: CAPTURED_HEADERS, runs: 2 }],
    runs.gateway,
    "headers-seed"
  );
  expect(settled.map((run) => run.outcome.status)).toEqual(["fulfilled", "fulfilled"]);
  expect(runs.headers.map((sent) => sent.get("anthropic-beta"))).toEqual([
    CAPTURED_HEADERS["anthropic-beta"],
    CAPTURED_HEADERS["anthropic-beta"],
  ]);
});

test("a read of a file the arm lacks is answered with the not-found result", async () => {
  const lacking = armWith("new", { "SKILL.md": SKILL_MD });
  const { gateway, bodies } = world((n) => f5(n === 1 ? "skill-read" : "text"));
  await replayOnce(BODY, lacking, gateway);
  expect(bodies[1]?.messages.at(-1)).toEqual({
    role: "user",
    content: [
      {
        type: "tool_result",
        tool_use_id: SKILL_READ_CALL.id,
        content: [
          {
            type: "text",
            text: `File not found: ${INSTALLED}/references/x.md`,
          },
        ],
        is_error: true,
      },
    ],
  });

  // The `none` arm has no directory of the skill: Oh My Pi's answer to a skill it lacks.
  const none: Arm = {
    name: "none",
    skill: "alpha",
    skillDir: undefined,
    installedDir: INSTALLED,
    messages: [],
    profileSkills: PROFILE_SKILLS.filter((skill) => skill.name !== "alpha"),
  };
  const noSkill = world((n) => f5(n === 1 ? "skill-read" : "text"));
  await replayOnce(BODY, none, noSkill.gateway);
  expect(noSkill.bodies[1]?.messages.at(-1)).toMatchObject({
    content: [
      {
        tool_use_id: SKILL_READ_CALL.id,
        content: [{ type: "text", text: "Unknown skill: alpha\nAvailable: beta" }],
        is_error: true,
      },
    ],
  });
});

/** The made-up copy of alpha the case's session read by path, and the directory Oh My Pi's
 *  hashline line shows its files under. */
const COPY = "/home/agent/src/acme-skills/skills/alpha";
const SHOWN = "~/src/acme-skills/skills/alpha";

/** A reply, not streamed, holding `content`. */
function jsonReply(content: Record<string, unknown>[], stop: string): Response {
  return Response.json({
    id: "msg_made_up_path",
    type: "message",
    role: "assistant",
    model: "claude-sonnet-5-5",
    content,
    stop_reason: stop,
    usage: { input_tokens: 10, output_tokens: 5 },
  });
}

// Catches: a reply's read of the skill by its path on disk taken for the judged step, so an arm
// is judged while it still loads the skill, or answered with one text in every arm; or a range
// past the file's end judged as the step where the pin answers it with its notice.
natives.test(
  "answers a reply's whole read by path of the cut's own copy by arm, and a read of it with a line range with the lines of its range, or the pin's notice for a range past its end; another selector is the judged step",
  async () => {
    const pathCopy = { dir: COPY, shownDir: SHOWN, rows: "hashline" as const };
    const old: Arm = {
      ...armWith("old", { "SKILL.md": SKILL_MD, "references/x.md": X_MD }),
      pathCopy,
    };
    const lacking: Arm = { ...armWith("new", { "SKILL.md": SKILL_MD }), pathCopy };
    const none: Arm = { ...noneArm(), pathCopy };
    const call = { type: "tool_use", id: "toolu_path", name: "read" };
    const answers: [Arm, string, boolean][] = [
      // 0098 is Oh My Pi's tag of X_MD.
      [
        old,
        `[${SHOWN}/references/x.md#0098]\n1:# X\n2:\n3:Each alpha step's result is one short line.`,
        false,
      ],
      [lacking, `Path '${COPY}/references/x.md' not found`, true],
      [none, `Path '${COPY}/references/x.md' not found`, true],
    ];
    for (const [arm, text, isError] of answers) {
      const read = { ...call, input: { path: `${COPY}/references/x.md` } };
      const { gateway, bodies } = world((n) =>
        n === 1
          ? jsonReply([read], "tool_use")
          : jsonReply([{ type: "text", text: "Step 4." }], "end_turn")
      );
      const replayed = await replayOnce({ ...BODY, stream: false }, arm, gateway);
      expect({ arm: arm.name, rounds: replayed.rounds }).toEqual({ arm: arm.name, rounds: 1 });
      expect(bodies[1]?.messages.at(-1)).toEqual({
        role: "user",
        content: [
          {
            type: "tool_result",
            tool_use_id: "toolu_path",
            content: [{ type: "text", text }],
            is_error: isError,
          },
        ],
      });
      expect(JSON.stringify(bodies)).not.toContain(arm.skillDir ?? "no directory");
    }

    // A line range of the copy is answered with its lines in the range form; :raw is not. Its rows
    // stop inside SKILL_MD's list (lines 8 to 10), so the pin adds the list's last line after a `…`.
    const ranged = { ...call, input: { path: `${COPY}/SKILL.md:1-5` } };
    const lines = SKILL_MD.replace(/\n$/, "").split("\n");
    const rows = [...lines.slice(0, 8).map((line, index) => `${index + 1}:${line}`), "…"]
      .concat(`10:${lines[9]}`)
      .join("\n");
    const { gateway, bodies } = scripted([[ranged], [ACT]]);
    const replayed = await replayOnce({ ...BODY, stream: false }, old, gateway);
    expect(replayed.rounds).toBe(1);
    expect(bodies[1]?.messages.at(-1)).toEqual({
      role: "user",
      content: [
        toolResult(
          "toolu_path",
          `[${SHOWN}/SKILL.md#${snapshotTag(SKILL_MD)}]\n${rows}\n\n[Showing lines 1-8 of ${lines.length}. Use :9 to continue]`,
          false
        ),
      ],
    });
    expect(replayed.step.message.content).toEqual([ranged, ACT]);
    // A range past the copy's end is answered with the pin's notice.
    const past = {
      ...call,
      input: { path: `${COPY}/SKILL.md:${lines.length + 5}-${lines.length + 9}` },
    };
    const beyond = scripted([[past], [ACT]]);
    const answeredPast = await replayOnce({ ...BODY, stream: false }, old, beyond.gateway);
    expect(answeredPast.rounds).toBe(1);
    expect(beyond.bodies[1]?.messages.at(-1)).toEqual({
      role: "user",
      content: [
        toolResult(
          "toolu_path",
          `Line ${lines.length + 5} is beyond end of file (${lines.length} lines total). Use :1 to read from the start, or :${lines.length} to read the last line.`,
          false
        ),
      ],
    });
    const raw = { ...call, input: { path: `${COPY}/SKILL.md:raw` } };
    const judged = await replayOnce({ ...BODY, stream: false }, old, scripted([[raw]]).gateway);
    expect({ rounds: judged.rounds, step: judged.step.message.content }).toEqual({
      rounds: 0,
      step: [raw],
    });
  }
);

/** Beta's SKILL.md: a made-up skill of the profile, which sends the agent to alpha. */
const BETA_MD =
  "---\nname: beta\ndescription: Use before any step of the made-up fixture project; it says which skill to load.\n---\n\n# Beta\n\nLoad alpha before the first alpha step.\n";

/** Beta's installed directory as the model sees it: beside alpha's (INSTALLED). */
const BETA_INSTALLED = "/home/agent/.omp/agent/skills/beta";

/** The installed copy of the skill `name` whose SKILL.md is `text`, a cache entry as context.ts's
 *  installedCopies names it. */
function installedCopy(name: string, text: string): InstalledCopy {
  const dir = path.join(scratch, `${name}-copy-${Math.random()}`);
  mkdirSync(dir, { recursive: true });
  writeFileSync(path.join(dir, "SKILL.md"), text);
  return { name, dir };
}

/** The `none` arm of alpha: no directory of it, and a profile of beta alone. */
function noneArm(): Arm {
  return {
    name: "none",
    skill: "alpha",
    skillDir: undefined,
    installedDir: INSTALLED,
    messages: [],
    profileSkills: PROFILE_SKILLS.filter((skill) => skill.name !== "alpha"),
  };
}

/** A `read` call of `target`, as a Messages reply holds it. */
function readCall(id: string, target: string): ContentBlock {
  return { type: "tool_use", id, name: "read", input: { path: target } };
}

/** The act a reply makes after its reads: text alone. */
const ACT: ContentBlock = { type: "text", text: "Step 1: done." };

/** A gateway whose n-th reply (from 1) holds `replies[n - 1]`, not streamed, and the bodies sent. */
function scripted(replies: readonly ContentBlock[][]) {
  return world((n) => {
    const content = replies[n - 1] ?? [{ type: "text", text: "no reply is scripted" }];
    const calls = content.some((block) => block.type === "tool_use");
    return jsonReply(content, calls ? "tool_use" : "end_turn");
  });
}

/** The tool result of the call `id` holding `text`, as a round's answers send it. */
function toolResult(id: string, text: string, isError: boolean): Record<string, unknown> {
  return {
    type: "tool_result",
    tool_use_id: id,
    content: [{ type: "text", text }],
    is_error: isError,
  };
}

/** Beta's SKILL.md under its `[Skill file: …]` line, at the installed directory `dir`. */
const betaAnswer = (dir: string) => `[Skill file: ${dir}/SKILL.md]\n${BETA_MD.replace(/\n$/, "")}`;

// Catches: a reply that only reads another installed skill taken for the judged step, so every arm
// fails on a read the case's line does not govern; or that read answered from one arm's text.
test("a reply that reads another installed skill is answered from its installed copy, the same in every arm, and the act after it is judged", async () => {
  const copies = [installedCopy("beta", BETA_MD)];
  const beta = readCall("toolu_beta", "skill://beta");
  const arms = [
    armWith("old", { "SKILL.md": SKILL_MD, "references/x.md": X_MD }),
    armWith("new", { "SKILL.md": SKILL_MD }),
    noneArm(),
  ];
  for (const arm of arms) {
    const { gateway, bodies } = scripted([[beta], [ACT]]);
    const replayed = await replayOnce({ ...BODY, stream: false }, arm, gateway, { copies });
    expect({ arm: arm.name, rounds: replayed.rounds }).toEqual({ arm: arm.name, rounds: 1 });
    expect(bodies[1]?.messages.at(-1)).toEqual({
      role: "user",
      content: [toolResult("toolu_beta", betaAnswer(BETA_INSTALLED), false)],
    });
    expect(replayed.reads).toEqual([
      [{ skill: "beta", file: "SKILL.md", by: "skill", from: "copy", isError: false }],
    ]);
    expect(replayed.step.message.content).toEqual([beta, ACT]);
    expect(JSON.stringify(bodies)).not.toContain(copies[0]?.dir ?? "no copy");
  }

  // Where the case's own reads show beta's installed directory, the answer names it; a file the
  // copy lacks gets Oh My Pi's not-found result at that directory.
  const shown: Arm = {
    ...armWith("old", { "SKILL.md": SKILL_MD }),
    skillDirs: { beta: "/opt/plugin/skills/beta" },
  };
  const gone = readCall("toolu_gone", "skill://beta/references/gone.md");
  const both = scripted([[beta, gone], [ACT]]);
  await replayOnce({ ...BODY, stream: false }, shown, both.gateway, { copies });
  expect(both.bodies[1]?.messages.at(-1)).toEqual({
    role: "user",
    content: [
      toolResult("toolu_beta", betaAnswer("/opt/plugin/skills/beta"), false),
      toolResult("toolu_gone", "File not found: /opt/plugin/skills/beta/references/gone.md", true),
    ],
  });

  // With no copy of beta the run fails, rather than answer it as not found; a reply that also
  // makes another call is judged as it is, and needs no copy.
  await expect(
    replayOnce({ ...BODY, stream: false }, shown, scripted([[beta], [ACT]]).gateway)
  ).rejects.toThrow("the old arm's profile installs beta, and the replay holds no installed copy");
  const bash = { type: "tool_use", id: "toolu_bash", name: "bash", input: { command: "ls" } };
  const judged = await replayOnce(
    { ...BODY, stream: false },
    shown,
    scripted([[beta, bash]]).gateway
  );
  expect({ rounds: judged.rounds, step: judged.step.message.content }).toEqual({
    rounds: 0,
    step: [beta, bash],
  });
});

// Catches: a reply whose only call reads a range of a skill file judged as the step (2 of 220 runs
// of a control screen, `skill://<skill>/references/reports.md:1-90`), while the agent still loads
// the skill, so every arm fails it alike.
natives.test(
  "a skill:// read with a line range, of another installed skill or the case's own, is answered with those lines in Oh My Pi's range form, and the run goes on to the act",
  async () => {
    const copies = [installedCopy("beta", BETA_MD)];
    const betaDir = copies[0]?.dir ?? "";
    const betaX = Array.from({ length: 10 }, (_, n) => `Beta line ${n + 1}.\n`).join("");
    mkdirSync(path.join(betaDir, "references"), { recursive: true });
    writeFileSync(path.join(betaDir, "references", "x.md"), betaX);
    const beta = readCall("toolu_beta", "skill://beta/references/x.md:2-3");
    const alpha = readCall("toolu_alpha", "skill://alpha:2-2");
    const arm = armWith("old", { "SKILL.md": SKILL_MD, "references/x.md": X_MD });
    const { gateway, bodies } = scripted([[beta, alpha], [ACT]]);
    const replayed = await replayOnce({ ...BODY, stream: false }, arm, gateway, { copies });
    const betaLines = betaX.replace(/\n$/, "").split("\n");
    const alphaLines = SKILL_MD.replace(/\n$/, "").split("\n");
    expect(bodies[1]?.messages.at(-1)).toEqual({
      role: "user",
      content: [
        // The skill protocol is unbounded: it keeps the in-memory builder's count-of-rest notice.
        // Its ten lines are one Markdown paragraph, whose last line the pin adds after a `…`.
        toolResult(
          "toolu_beta",
          `[Skill file: ${BETA_INSTALLED}/references/x.md]\n${betaLines.slice(0, 6).join("\n")}\n…\n${betaLines[9]}\n\n[4 more lines in file. Use :7 to continue]`,
          false
        ),
        toolResult(
          "toolu_alpha",
          `[Skill file: ${INSTALLED}/SKILL.md]\n${alphaLines.slice(0, 5).join("\n")}\n\n[${alphaLines.length - 5} more lines in file. Use :6 to continue]`,
          false
        ),
      ],
    });
    expect(replayed.reads).toEqual([
      [
        { skill: "beta", file: "references/x.md", by: "skill", from: "copy", isError: false },
        { skill: "alpha", file: "SKILL.md", by: "skill", from: "arm", isError: false },
      ],
    ]);
    expect(replayed.step.message.content).toEqual([beta, alpha, ACT]);
    // In the arm with no skill, the case's skill read with a range gets the unknown-skill text.
    const none = await replayOnce(
      { ...BODY, stream: false },
      noneArm(),
      scripted([[alpha], [ACT]]).gateway,
      { copies }
    );
    expect(none.reads).toEqual([
      [{ skill: "alpha", file: "SKILL.md", by: "skill", from: "absent", isError: true }],
    ]);
    // A range of a file whose blocks the pin finds with a grammar it downloads (Ruby's) is not
    // answered: the run is judged on the read.
    mkdirSync(path.join(betaDir, "scripts"), { recursive: true });
    writeFileSync(
      path.join(betaDir, "scripts", "tool.rb"),
      "def tool\n  if ready\n    run\n  end\nend\n"
    );
    const ruby = readCall("toolu_ruby", "skill://beta/scripts/tool.rb:3-3");
    const judged = await replayOnce({ ...BODY, stream: false }, arm, scripted([[ruby]]).gateway, {
      copies,
    });
    expect({ rounds: judged.rounds, step: judged.step.message.content }).toEqual({
      rounds: 0,
      step: [ruby],
    });
  }
);

/** acme/tools' alpha, a copy of another source that shares the case skill's name. */
const TOOLS_ALPHA_MD =
  "---\nname: alpha\ndescription: Use for the tools' own alpha list.\n---\n\n# Alpha of tools\n\nOne line.\n";

// Catches: a none run's read of the case skill's name answered with Oh My Pi's unknown-skill text
// where the arm lists another source's copy under that name, which Oh My Pi reads instead.
test("in none, a reply's skill:// read of the case's skill is answered from the copy of another source the profile lists under its name, and with the unknown-skill text where it lists none", async () => {
  const tools = installedCopy("alpha", TOOLS_ALPHA_MD);
  const copies = [installedCopy("beta", BETA_MD), tools];
  const none = noneArm();
  const relisted: Arm = {
    ...none,
    profileSkills: [
      ...none.profileSkills,
      { repo: "acme/tools", commit: "0123456789abcdef", path: "skills/alpha", name: "alpha" },
    ],
    relisted: ["tools/alpha"],
  };
  const read = readCall("toolu_alpha", "skill://alpha");
  const listed = scripted([[read], [ACT]]);
  const answered = await replayOnce({ ...BODY, stream: false }, relisted, listed.gateway, {
    copies,
  });
  expect(listed.bodies[1]?.messages.at(-1)).toEqual({
    role: "user",
    content: [
      toolResult(
        "toolu_alpha",
        `[Skill file: ${INSTALLED}/SKILL.md]\n${TOOLS_ALPHA_MD.replace(/\n$/, "")}`,
        false
      ),
    ],
  });
  expect(answered.reads).toEqual([
    [{ skill: "alpha", file: "SKILL.md", by: "skill", from: "copy", isError: false }],
  ]);
  expect(JSON.stringify(listed.bodies)).not.toContain(tools.dir);

  // A none that lists no copy under the name: Oh My Pi's answer to a skill it lacks.
  const unlisted = scripted([[read], [ACT]]);
  const absent = await replayOnce({ ...BODY, stream: false }, noneArm(), unlisted.gateway, {
    copies,
  });
  expect(unlisted.bodies[1]?.messages.at(-1)).toEqual({
    role: "user",
    content: [toolResult("toolu_alpha", "Unknown skill: alpha\nAvailable: beta", true)],
  });
  expect(absent.reads).toEqual([
    [{ skill: "alpha", file: "SKILL.md", by: "skill", from: "absent", isError: true }],
  ]);
});

// Catches: a whole read by path of another installed skill's SKILL.md taken for the judged step.
test("a whole read by path of another installed skill's SKILL.md is answered from its copy in Oh My Pi's form of a read by path; a file its copy lacks is not answered", async () => {
  const copies = [installedCopy("beta", BETA_MD)];
  const target = "/home/agent/src/acme-skills/skills/beta/SKILL.md";
  const lines = BETA_MD.replace(/\n$/, "").split("\n");
  const hashline = (shown: string) =>
    `[${shown}#${snapshotTag(BETA_MD)}]\n${lines.map((line, index) => `${index + 1}:${line}`).join("\n")}`;
  const answers: [Arm, string][] = [
    [armWith("old", { "SKILL.md": SKILL_MD }), hashline(target)],
    [noneArm(), hashline(target)],
    // A session whose working directory holds the file shows its path relative to it.
    [
      {
        ...armWith("new", { "SKILL.md": SKILL_MD }),
        pathCopy: {
          dir: COPY,
          cwd: "/home/agent/src/acme-skills",
          shownDir: "skills/alpha",
          rows: "hashline",
        },
      },
      hashline("skills/beta/SKILL.md"),
    ],
    // A session that shows the lines of a read by path alone gets them alone.
    [{ ...armWith("minus", { "SKILL.md": SKILL_MD }), pathRows: "plain" }, lines.join("\n")],
  ];
  for (const [arm, text] of answers) {
    const read = readCall("toolu_path", target);
    const { gateway, bodies } = scripted([[read], [ACT]]);
    const replayed = await replayOnce({ ...BODY, stream: false }, arm, gateway, { copies });
    expect({ arm: arm.name, rounds: replayed.rounds }).toEqual({ arm: arm.name, rounds: 1 });
    expect(bodies[1]?.messages.at(-1)).toEqual({
      role: "user",
      content: [toolResult("toolu_path", text, false)],
    });
    expect(replayed.reads).toEqual([
      [{ skill: "beta", file: "SKILL.md", by: "path", from: "copy", isError: false }],
    ]);
    expect(replayed.step.message.content).toEqual([read, ACT]);
  }
  // A reference the copy lacks may be on the session's disk: the reply is judged as it is.
  const gone = readCall("toolu_gone", "/home/agent/src/acme-skills/skills/beta/references/gone.md");
  const judged = await replayOnce(
    { ...BODY, stream: false },
    armWith("old", { "SKILL.md": SKILL_MD }),
    scripted([[gone]]).gateway,
    { copies }
  );
  expect(judged.rounds).toBe(0);
});

// Catches: a read of a skill the arm does not install taken for the judged step, which fails that
// arm by construction.
test("a read of a skill the profile does not list gets Oh My Pi's unknown-skill result, and the agent runs on", async () => {
  const gamma = readCall("toolu_gamma", "skill://gamma");
  const answers: [Arm, string][] = [
    [armWith("old", { "SKILL.md": SKILL_MD }), "alpha, beta"],
    [noneArm(), "beta"],
  ];
  for (const [arm, available] of answers) {
    const { gateway, bodies } = scripted([[gamma], [ACT]]);
    const replayed = await replayOnce({ ...BODY, stream: false }, arm, gateway, {
      copies: [installedCopy("beta", BETA_MD)],
    });
    expect(bodies[1]?.messages.at(-1)).toEqual({
      role: "user",
      content: [toolResult("toolu_gamma", `Unknown skill: gamma\nAvailable: ${available}`, true)],
    });
    expect(replayed.reads).toEqual([
      [{ skill: "gamma", file: "SKILL.md", by: "skill", from: "absent", isError: true }],
    ]);
    expect(replayed.step.message.content).toEqual([gamma, ACT]);
  }
});

// Catches: the case's skill's read after another skill's read answered other than a read of it in
// a round of its own is, or the none arm's read of the case's skill judged, which fails that arm by
// construction.
test("a read of another skill, then of the case's skill, then the act: two rounds, the case's skill answered as in a round of its own, and in none with the unknown-skill result", async () => {
  const copies = [installedCopy("beta", BETA_MD)];
  const beta = readCall("toolu_beta", "skill://beta");
  const alpha = readCall("toolu_alpha", "skill://alpha/references/x.md");
  const answers: [Arm, string, boolean, "arm" | "absent"][] = [
    [
      armWith("old", { "SKILL.md": SKILL_MD, "references/x.md": X_MD }),
      `[Skill file: ${INSTALLED}/references/x.md]\n${X_MD.replace(/\n$/, "")}`,
      false,
      "arm",
    ],
    [
      armWith("new", { "SKILL.md": SKILL_MD }),
      `File not found: ${INSTALLED}/references/x.md`,
      true,
      "arm",
    ],
    [noneArm(), "Unknown skill: alpha\nAvailable: beta", true, "absent"],
  ];
  for (const [arm, text, isError, from] of answers) {
    const { gateway, bodies } = scripted([[beta], [alpha], [ACT]]);
    const replayed = await replayOnce({ ...BODY, stream: false }, arm, gateway, { copies });
    expect({ arm: arm.name, rounds: replayed.rounds }).toEqual({ arm: arm.name, rounds: 2 });
    expect(bodies[2]?.messages.at(-1)).toEqual({
      role: "user",
      content: [toolResult("toolu_alpha", text, isError)],
    });
    expect(replayed.reads).toEqual([
      [{ skill: "beta", file: "SKILL.md", by: "skill", from: "copy", isError: false }],
      [{ skill: "alpha", file: "references/x.md", by: "skill", from, isError }],
    ]);
    expect(replayed.step.message.content).toEqual([beta, alpha, ACT]);
  }
});

// Catches: a case whose line governs a read of a file of the skill judged on the act after that
// read alone, so that it fails in every arm, the arm whose text keeps the line too.
test("a navigation case passes on its read alone, and fails when the read is of another file: the judge reads each answered round's reads, then the act", async () => {
  // A stand-in judge of the line "Read `references/x.md` before the first step": it passes a step
  // that reads x.md before anything else, and fails any other.
  const steps: string[] = [];
  const keyFile = path.join(scratch, "judge-key");
  writeFileSync(keyFile, fixtureSecret("gw"), { mode: 0o600 });
  const judge = createGateway({
    baseUrl: "http://gateway.test",
    keyFile,
    concurrency: 1,
    timeoutMs: 5_000,
    spendRecorder: null,
    fetch: async (_url, init) => {
      const step = JSON.parse(String(init.body)).messages[0].content[1].text as string;
      steps.push(step);
      const read = '<step>\n[tool call: read] {"path":"skill://alpha/references/x.md"}\n';
      const verdict = step.startsWith(read) ? "pass" : "fail";
      const judgement = JSON.stringify({ verdict, reason: "The stand-in judge read the step." });
      // The judge's body streams (prompts/judge.md), so its reply is a Messages API stream.
      return sseResponse(
        messageSse({
          id: "msg_made_up_judge",
          model: "claude-opus-5-5",
          content: [{ type: "text", text: judgement }],
          stopReason: "end_turn",
        })
      );
    },
    sleep: async () => {},
  });
  const verdictOf = async (target: string) => {
    const arm = armWith("old", { "SKILL.md": SKILL_MD, "references/x.md": X_MD });
    const { gateway } = scripted([[readCall("toolu_nav", target)], [ACT]]);
    const replayed = await replayOnce({ ...BODY, stream: false }, arm, gateway);
    const judged = await judgeReply(
      {
        criterion: {
          pass: "The step reads references/x.md before anything else.",
          fail: "The step does anything else first.",
        },
        line: "1. Read `references/x.md` before the first step.",
        before: "",
        reply: stepText(replayed.step.message.content),
      },
      judge
    );
    return judged.verdict;
  };
  expect(await verdictOf("skill://alpha/references/x.md")).toBe("pass");
  expect(await verdictOf("skill://alpha/SKILL.md")).toBe("fail");
  expect(steps[0]).toBe(
    `<step>\n[tool call: read] {"path":"skill://alpha/references/x.md"}\n${ACT.text}\n</step>`
  );
});

// Catches: a round that sends one arm's runs as a batch before another's, an order that is not
// the seed's, an arm's first run that does not go alone among its arm's, or a failed run that
// takes the round's other outcomes with it.
test("replayInterleaved sends every arm's runs in the seeded order, each arm's first run alone, and keeps each one's outcome", async () => {
  const names = ["old", "new", "none"] as const;
  const counts: Record<string, number> = { old: 3, new: 3, none: 2 };
  const text = await world(() => f5("text")).gateway.send(BODY);
  const events: string[] = [];
  const sent: { arm: unknown; id: string | null }[] = [];
  // A gateway whose fifth send fails: the other runs keep their steps.
  const gateway: Gateway = {
    async send(body): Promise<Reply> {
      sent.push({ arm: body.x_arm, id: bodyIdentity(body) });
      const n = sent.length;
      events.push(`start ${n}`);
      await Promise.resolve();
      events.push(`end ${n}`);
      if (n === 5) throw new Error("the model call failed: made up");
      return text;
    },
    spend: () => ({ models: {}, usd: 0 }),
  };
  const arms = names.map((name) => ({
    arm: armWith(name === "none" ? "minus" : name, { "SKILL.md": SKILL_MD }),
    name,
  }));
  const runs = await replayInterleaved(
    arms.map(({ arm, name }) => ({
      arm: { ...arm, name },
      body: { ...BODY, x_arm: name },
      runs: counts[name] ?? 0,
    })),
    gateway,
    "order-seed"
  );
  const order = sendOrder(
    names.map((name) => ({ key: name, runs: counts[name] ?? 0 })),
    "order-seed"
  );
  // The sends went in the seeded order, which interleaves the arms, and the runs come back in it.
  expect(sent.map((each) => each.arm)).toEqual(order.map((each) => each.key));
  expect(new Set(order.slice(0, 4).map((each) => each.key)).size).toBeGreaterThan(1);
  expect(runs.map(({ arm, run, place }) => [arm, run, place])).toEqual(
    order.map(({ key, run }, place) => [key, run, place])
  );
  // Each arm's first send in the order ended before the next send in the order started.
  for (const name of names) {
    const first = order.findIndex((each) => each.key === name) + 1;
    if (first === order.length) continue;
    expect(events.indexOf(`end ${first}`)).toBeLessThan(events.indexOf(`start ${first + 1}`));
  }
  expect(events).toHaveLength(2 * order.length);
  // Each run went with an id of its own, and its outcome is kept: the fifth failed alone.
  expect(new Set(runs.map((run) => run.userId)).size).toBe(runs.length);
  expect(runs.map((run) => run.outcome.status)).toEqual(
    order.map((_, place) => (place === 4 ? "rejected" : "fulfilled"))
  );
  const failed = runs[4]?.outcome;
  expect(failed?.status === "rejected" && String(failed.reason)).toBe(
    "Error: the model call failed: made up"
  );
  expect(runs[0]?.outcome).toMatchObject({ status: "fulfilled", value: { rounds: 0 } });
  // A seed gives one order, and another seed another.
  expect(
    sendOrder(
      [
        { key: "old", runs: 5 },
        { key: "new", runs: 5 },
      ],
      "a"
    )
  ).toEqual(
    sendOrder(
      [
        { key: "old", runs: 5 },
        { key: "new", runs: 5 },
      ],
      "a"
    )
  );
  expect(
    sendOrder(
      [
        { key: "old", runs: 5 },
        { key: "new", runs: 5 },
      ],
      "a"
    )
  ).not.toEqual(
    sendOrder(
      [
        { key: "old", runs: 5 },
        { key: "new", runs: 5 },
      ],
      "b"
    )
  );
  const old = arms[0]?.arm ?? armWith("old", {});
  await expect(
    replayInterleaved([{ arm: old, body: BODY, runs: -1 }], gateway, "s")
  ).rejects.toThrow("0 or more, not -1");
  await expect(
    replayInterleaved(
      [
        { arm: old, body: BODY, runs: 1 },
        { arm: old, body: BODY, runs: 1 },
      ],
      gateway,
      "s"
    )
  ).rejects.toThrow("the arm old is given twice");
});

// Catches: runs of an arm that share the captured id, or one run whose rounds go with two ids.
test("a run's requests carry an id no other run carries, the same in every round of the run", async () => {
  const arm = armWith("old", { "SKILL.md": SKILL_MD, "references/x.md": X_MD });
  const fresh = armWith("new", { "SKILL.md": SKILL_MD, "references/x.md": X_MD });
  // A run's first round reads x.md, and its second, which holds the answer, gives the text.
  const bodies: MessagesBody[] = [];
  const keyFile = path.join(scratch, "rounds-key");
  writeFileSync(keyFile, fixtureSecret("gw"), { mode: 0o600 });
  const byRounds = createGateway({
    baseUrl: "http://gateway.test",
    keyFile,
    concurrency: 3,
    timeoutMs: 5_000,
    spendRecorder: null,
    fetch: async (_url, init) => {
      const body = JSON.parse(String(init.body)) as MessagesBody;
      bodies.push(body);
      return f5(body.messages.length === BODY.messages.length ? "skill-read" : "text");
    },
    sleep: async () => {},
  });
  const runs = await replayInterleaved(
    [
      { arm, body: BODY, runs: 4 },
      { arm: fresh, body: BODY, runs: 4 },
    ],
    byRounds,
    "id-seed"
  );
  expect(runs.map((run) => run.outcome.status)).toEqual(Array(8).fill("fulfilled"));
  const requests = new Map<string, number>();
  for (const body of bodies) {
    const id = bodyIdentity(body) ?? "none";
    requests.set(id, (requests.get(id) ?? 0) + 1);
  }
  // Eight runs, eight ids, two requests each (its skill round and its step), none the captured id.
  expect(bodies).toHaveLength(16);
  expect([...requests.values()]).toEqual(Array(8).fill(2));
  expect(requests.has(bodyIdentity(BODY) ?? "")).toBe(false);
  expect(new Set(runs.map((run) => run.userId))).toEqual(new Set(requests.keys()));
  for (const run of runs) {
    expect(run.outcome.status === "fulfilled" && run.outcome.value.rounds).toBe(1);
    expect(run.outcome.status === "fulfilled" && run.outcome.value.userId).toBe(run.userId);
  }
});

// The batch probe of 2026-11-21 as a test: one made-up body, a stand-in gateway that answers by
// the body's session id, and three variants interleaved in one seeded order. With a fixed id a
// variant's every run gets the one answer that id gets, so the id is a factor of its batch; the
// runner draws a fresh id for every run, so it never sends one id in two runs. Its 180 streamed
// replies took past bun's 5-second default in a loaded pod, so the row has 30 seconds.
test("the batch probe: a fixed session id sets its batch's count, and the runner never sends one id in two runs", async () => {
  const FIXED_A = JSON.stringify({ session_id: "00000000-0000-4000-8000-000000000002" });
  const FIXED_B = JSON.stringify({ session_id: "00000000-0000-4000-8000-000000000001" });
  // The stand-in reads the skill's reference when the id's last hex digit is even.
  const reads = (id: string | null) =>
    Number.parseInt((id ?? "1").replace(/[^0-9a-f]/g, "").slice(-1), 16) % 2 === 0;
  const ids: (string | null)[] = [];
  const keyFile = path.join(scratch, "probe-key");
  writeFileSync(keyFile, fixtureSecret("gw"), { mode: 0o600 });
  const gateway = createGateway({
    baseUrl: "http://gateway.test",
    keyFile,
    concurrency: 8,
    timeoutMs: 5_000,
    spendRecorder: null,
    fetch: async (_url, init) => {
      const body = JSON.parse(String(init.body)) as MessagesBody;
      const id = bodyIdentity(body);
      ids.push(id);
      return f5(reads(id) ? "skill-read" : "text");
    },
    sleep: async () => {},
  });
  // A run passes when its reply's tool calls read the reference, as the probe counted one.
  const readsReference = (reply: Reply) =>
    toolCalls(reply.message.content).some((call) =>
      JSON.stringify(call.input).includes("references/x.md")
    );
  // The probe: 30 sends of each variant, interleaved in one seeded order.
  const variants = ["fixed-a", "fixed-b", "fresh"] as const;
  const probe = sendOrder(
    variants.map((key) => ({ key, runs: 30 })),
    "probe-seed"
  );
  const probed = await Promise.all(
    probe.map(({ key }) =>
      gateway.send(
        key === "fresh"
          ? freshIdentity(BODY).body
          : withId(BODY, key === "fixed-a" ? FIXED_A : FIXED_B)
      )
    )
  );
  const count = (key: string) =>
    probe.filter((each, place) => each.key === key && readsReference(probed[place] as Reply))
      .length;
  expect([count("fixed-a"), count("fixed-b")]).toEqual([30, 0]);
  expect(count("fresh")).toBeGreaterThan(0);
  expect(count("fresh")).toBeLessThan(30);
  // The runner: three arms whose captured bodies hold the fixed ids, interleaved in one order.
  ids.length = 0;
  const arm = (name: "old" | "new" | "minus") => armWith(name, { "SKILL.md": SKILL_MD });
  const runs = await replayInterleaved(
    [
      { arm: arm("old"), body: withId(BODY, FIXED_A), runs: 30 },
      { arm: arm("new"), body: withId(BODY, FIXED_B), runs: 30 },
      { arm: arm("minus"), body: withId(BODY, FIXED_A), runs: 30 },
    ],
    gateway,
    "probe-seed",
    { maxSkillRounds: 0 }
  );
  expect(ids).toHaveLength(90);
  expect(new Set(ids).size).toBe(90);
  expect(ids).not.toContain(FIXED_A);
  expect(ids).not.toContain(FIXED_B);
  // A run whose reply only reads the reference gets no verdict (ReadsPastLastRound, no round
  // being answered here); its refusal holds that reply.
  const stepOf = (outcome: PromiseSettledResult<Replayed>) =>
    outcome.status === "fulfilled"
      ? outcome.value.step
      : outcome.reason instanceof ReadsPastLastRound
        ? outcome.reason.replayed.step
        : undefined;
  for (const name of ["old", "new", "minus"] as const) {
    const passes = runs.filter((run) => {
      const step = stepOf(run.outcome);
      return run.arm === name && step !== undefined && readsReference(step);
    }).length;
    // No arm's count is its fixed id's 30 or 0: the id is no factor of the arm's batch.
    expect(passes).toBeGreaterThan(0);
    expect(passes).toBeLessThan(30);
  }
}, 30_000);

// Catches: a run whose bytes differ from replayOnce's in more than its own session id.
test("each run sends its arm's body as replayOnce sends it, but for a session id of its own", async () => {
  const arm = armWith("old", { "SKILL.md": SKILL_MD, "references/x.md": X_MD });
  /** A gateway whose fake fetch answers `answer` and keeps each body's text as sent. */
  const recording = (answer: () => Response) => {
    const texts: string[] = [];
    const keyFile = path.join(scratch, `key-${Math.random()}`);
    writeFileSync(keyFile, fixtureSecret("gw"), { mode: 0o600 });
    const gateway = createGateway({
      baseUrl: "http://gateway.test",
      keyFile,
      concurrency: 2,
      timeoutMs: 5_000,
      spendRecorder: null,
      fetch: async (_url, init) => {
        texts.push(String(init.body));
        return answer();
      },
      sleep: async () => {},
    });
    return { gateway, texts };
  };
  for (const [body, answer] of [
    [BODY, () => f5("text")],
    [RESPONSES_BODY, () => f9("text")],
  ] as const) {
    const one = recording(answer);
    await replayOnce(body, arm, one.gateway);
    expect(one.texts).toHaveLength(1);
    const sentOnce = JSON.parse(one.texts[0] ?? "{}") as RequestBody;
    const runs = recording(answer);
    const settled = await replayInterleaved([{ arm, body, runs: 6 }], runs.gateway, "bytes");
    expect(settled.map((run) => run.outcome.status)).toEqual(Array(6).fill("fulfilled"));
    const ids = runs.texts.map((text) => bodyIdentity(JSON.parse(text)));
    expect(new Set([...ids, bodyIdentity(sentOnce)]).size).toBe(7);
    expect(
      runs.texts.map((text) =>
        JSON.stringify(withId(JSON.parse(text) as RequestBody, bodyIdentity(sentOnce)))
      )
    ).toEqual(Array(6).fill(one.texts[0]));
  }
  // The bytes are the body with its cache breakpoint and the run's id, as each run made them.
  const one = recording(() => f5("text"));
  await replayInterleaved([{ arm, body: BODY, runs: 2 }], one.gateway, "bytes");
  for (const text of one.texts) {
    const sent = JSON.parse(text) as MessagesBody;
    expect(text).toBe(JSON.stringify(withId(withCacheBreakpoint(BODY), bodyIdentity(sent))));
  }
});

test("withCacheBreakpoint puts one breakpoint on the last block and changes nothing else", () => {
  const before = JSON.stringify(BODY);
  // F2 holds two breakpoints, on its tools and its system prompt, and none in its messages.
  expect(breakpointPaths(BODY).sort()).toEqual(["system.0", "tools.10"]);
  const long = hourLong(BODY);
  const cached = withCacheBreakpoint(long);
  expect(breakpointPaths(cached).sort()).toEqual(["messages.2.content.0", "system.0", "tools.10"]);
  expect(lastBlock(cached)).toMatchObject({ type: "tool_result" });
  expect(lastBlock(cached).cache_control).toEqual({ type: "ephemeral", ttl: "1h" });
  // Byte for byte, the body less the breakpoint added is the body given, which is left as it was.
  const back = copyOf(cached);
  delete lastBlock(back).cache_control;
  expect(JSON.stringify(back)).toBe(JSON.stringify(long));
  expect(JSON.stringify(BODY)).toBe(before);
  // After 5-minute breakpoints, as F2's own are, the one added is 5-minute too: a request may not
  // hold an hour-long breakpoint after a shorter one.
  expect(lastBlock(withCacheBreakpoint(BODY)).cache_control).toEqual({ type: "ephemeral" });
});

test("withCacheBreakpoint keeps at most four, dropping the earliest message ones first", () => {
  const crowded = copyOf(hourLong(BODY));
  crowded.messages[0].content[0].cache_control = { type: "ephemeral", ttl: "1h" };
  crowded.messages[0].content[1].cache_control = { type: "ephemeral", ttl: "1h" };
  crowded.messages[1].content[0].cache_control = { type: "ephemeral", ttl: "1h" };
  const cached = withCacheBreakpoint(crowded);
  expect(MAX_BREAKPOINTS).toBe(4);
  expect(breakpointPaths(cached).sort()).toEqual([
    "messages.1.content.0",
    "messages.2.content.0",
    "system.0",
    "tools.10",
  ]);
  expect(breakpointPaths(crowded)).toHaveLength(5);

  // Tools and a system prompt that hold four already leave no room: the body comes back as it is.
  const full = copyOf(BODY);
  for (const tool of full.tools.slice(0, 3)) tool.cache_control = { type: "ephemeral" };
  expect(breakpointPaths(full)).toHaveLength(5);
  expect(withCacheBreakpoint(full)).toBe(full);
});

test("withCacheBreakpoint does not add a second breakpoint to a last block that holds one", () => {
  const marked = copyOf(BODY);
  lastBlock(marked).cache_control = { type: "ephemeral" };
  expect(withCacheBreakpoint(marked)).toBe(marked);
  expect(breakpointPaths(withCacheBreakpoint(marked))).toHaveLength(3);

  // With more than four, the earliest message breakpoint goes and the last block's own stays.
  marked.messages[0].content[0].cache_control = { type: "ephemeral" };
  marked.messages[0].content[1].cache_control = { type: "ephemeral" };
  const cached = withCacheBreakpoint(marked);
  expect(breakpointPaths(cached).sort()).toEqual([
    "messages.0.content.1",
    "messages.2.content.0",
    "system.0",
    "tools.10",
  ]);
  expect(lastBlock(cached).cache_control).toEqual({ type: "ephemeral" });
});

test("on a Responses body (F8, F9): answers a skill read with a function_call_output item, adds no cache_control, and stops at the first other reply", async () => {
  const arm = armWith("old", { "SKILL.md": SKILL_MD, "references/x.md": X_MD });
  const { gateway, bodies, headers } = world((n) => f9(n === 1 ? "skill-read" : "text"));
  const replayed = await replayOnce(RESPONSES_BODY, arm, gateway);
  const sent = bodies as readonly unknown[] as readonly ResponsesBody[];
  expect(sent).toHaveLength(2);
  // The body goes as it was captured but for its cache key, a fresh one of the run's own: the
  // Responses route caches the prefix it shares by itself.
  const key = bodyIdentity(sent[0] ?? RESPONSES_BODY);
  expect(key).not.toBe(RESPONSES_BODY.prompt_cache_key);
  expect(key).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/);
  expect(replayed.userId).toBe(key);
  expect(sent[0]).toEqual(withId(RESPONSES_BODY, key));
  expect(JSON.stringify(sent)).not.toContain("cache_control");
  // The second send is the body with the reply's output items as they came and the arm's answer
  // to its read, the form Oh My Pi sends a tool result in (F8's `function_call_output`).
  const [read] = f9Completed("skill-read").output;
  const { input: sentInput, ...sentFields } = sent[1] ?? RESPONSES_BODY;
  const { input: bodyInput, ...bodyFields } = withId(RESPONSES_BODY, key);
  expect(sentFields).toEqual(bodyFields);
  expect(sentInput).toEqual([
    ...bodyInput,
    read,
    {
      type: "function_call_output",
      call_id: read?.call_id,
      output: `[Skill file: ${INSTALLED}/references/x.md]\n${X_MD.replace(/\n$/, "")}`,
    },
  ]);
  expect(JSON.stringify(sent)).not.toContain(arm.skillDir ?? "no directory");
  // No anthropic-* header goes beside a Responses body.
  for (const each of headers) {
    expect([...each.keys()].filter((name) => name.startsWith("anthropic-"))).toEqual([]);
  }

  expect(replayed.rounds).toBe(1);
  expect(replayed.step.message.stop_reason).toBe("completed");
  // The judged step is the answered round's function call, then the reply after it.
  expect(replayed.step.message.content.map((block) => block.type)).toEqual([
    "function_call",
    "reasoning",
    "message",
  ]);
  expect<unknown>(replayed.step.message.content[0]).toEqual(read);
  // Each send's cached tokens are recorded as its cache reads.
  expect(replayed.usage.map((spent) => [spent.model, spent.cacheRead, spent.reasoning])).toEqual([
    ["gpt-6.1-sol", 0, 0],
    ["gpt-6.1-sol", 8083, 43],
  ]);
});

test("on a Responses body: a reply with a read of another skill and another tool call is the judged step at once (F9)", async () => {
  const arm = armWith("old", { "SKILL.md": SKILL_MD, "references/x.md": X_MD });
  const { gateway, bodies } = world(() => f9("skill-read-and-other"));
  const replayed = await replayOnce(RESPONSES_BODY, arm, gateway);
  expect(bodies).toHaveLength(1);
  expect(replayed.rounds).toBe(0);
  expect(replayed.step.message.content.map((block) => block.name)).toEqual(["read", "bash"]);
});

test("on a Responses body: at most three rounds of skill reads, and a read of a file the arm lacks is answered with the not-found text", async () => {
  const arm = armWith("new", { "SKILL.md": SKILL_MD });
  const { gateway, bodies } = world((n) => f9(n <= 3 ? "skill-read" : "text"));
  const replayed = await replayOnce(RESPONSES_BODY, arm, gateway);
  const sent = bodies as readonly unknown[] as readonly ResponsesBody[];
  expect(sent.map((body) => body.input.length)).toEqual([3, 5, 7, 9]);
  expect(replayed.rounds).toBe(MAX_SKILL_ROUNDS);
  expect(sent[1]?.input.at(-1)).toEqual({
    type: "function_call_output",
    call_id: f9Completed("skill-read").output[0]?.call_id,
    output: `File not found: ${INSTALLED}/references/x.md`,
  });
});

// Catches: a reply in the arm of ours whose read of their skill's URL is judged as the step, or
// is answered from our files; or a read of our skill's URL that goes unanswered.
test("in ours, a reply's read of skill://<ours> is answered from our directory and a read of skill://<theirs> gets the Unknown skill text and is answered, not judged", async () => {
  const oursSkill = SKILL_MD.replace("name: alpha\n", "name: alpha-ours\n");
  const oursInstalled = path.join(path.dirname(INSTALLED), "alpha-ours");
  const ours: Arm = {
    ...armWith("ours", { "SKILL.md": oursSkill, "references/x.md": X_MD }),
    skill: "alpha-ours",
    installedDir: oursInstalled,
    profileSkills: PROFILE_SKILLS.map((skill) =>
      skill.name === "alpha" ? { ...skill, name: "alpha-ours" } : skill
    ),
    ours: { theirs: "alpha", replacedOwn: false, removedReads: [] },
  };
  /** A reply, not streamed, whose one block is `block`. */
  const reply = (n: number, block: Record<string, unknown>) =>
    Response.json({
      id: `msg_made_up_${n}`,
      type: "message",
      role: "assistant",
      model: "claude-sonnet-5-5",
      content: [block],
      stop_reason: block.type === "text" ? "end_turn" : "tool_use",
      usage: { input_tokens: 10, output_tokens: 5 },
    });
  const read = (id: string, target: string) => ({
    type: "tool_use",
    id,
    name: "read",
    input: { path: target },
  });
  const { gateway, bodies } = world((n) =>
    n === 1
      ? reply(n, read("toolu_theirs", "skill://alpha/references/x.md"))
      : n === 2
        ? reply(n, read("toolu_ours", "skill://alpha-ours/references/x.md"))
        : reply(n, { type: "text", text: "Step 1 done." })
  );
  const replayed = await replayOnce({ ...BODY, stream: false }, ours, gateway);
  // Both reads were answered, so the judged step is their two calls, then the third reply; the
  // read of theirs is recorded as one no copy answered.
  expect(bodies).toHaveLength(3);
  expect(replayed.rounds).toBe(2);
  expect(replayed.step.message.content).toEqual([
    read("toolu_theirs", "skill://alpha/references/x.md"),
    read("toolu_ours", "skill://alpha-ours/references/x.md"),
    { type: "text", text: "Step 1 done." },
  ]);
  expect(replayed.reads).toEqual([
    [{ skill: "alpha", file: "references/x.md", by: "skill", from: "absent", isError: true }],
    [{ skill: "alpha-ours", file: "references/x.md", by: "skill", from: "arm", isError: false }],
  ]);
  expect(bodies[1]?.messages.at(-1)).toEqual({
    role: "user",
    content: [
      {
        type: "tool_result",
        tool_use_id: "toolu_theirs",
        content: [{ type: "text", text: "Unknown skill: alpha\nAvailable: alpha-ours, beta" }],
        is_error: true,
      },
    ],
  });
  expect(bodies[2]?.messages.at(-1)).toEqual({
    role: "user",
    content: [
      {
        type: "tool_result",
        tool_use_id: "toolu_ours",
        content: [
          {
            type: "text",
            text: `[Skill file: ${oursInstalled}/references/x.md]\n${X_MD.replace(/\n$/, "")}`,
          },
        ],
        is_error: false,
      },
    ],
  });
  expect(JSON.stringify(bodies)).not.toContain(ours.skillDir);
});

// Catches: a reply in the arm of ours whose read of their skill's URL gets Oh My Pi's
// unknown-skill text where the arm lists another source's copy under their name, which Oh My Pi
// reads instead.
test("in ours, a reply's skill:// read of their skill is answered from the copy of another source the arm lists under their name, and a read of ours from our directory", async () => {
  const tools = installedCopy("alpha", TOOLS_ALPHA_MD);
  const copies = [installedCopy("beta", BETA_MD), tools];
  const oursSkill = SKILL_MD.replace("name: alpha\n", "name: alpha-ours\n");
  const oursInstalled = path.join(path.dirname(INSTALLED), "alpha-ours");
  const ours: Arm = {
    ...armWith("ours", { "SKILL.md": oursSkill }),
    skill: "alpha-ours",
    installedDir: oursInstalled,
    profileSkills: [
      ...PROFILE_SKILLS.map((skill) =>
        skill.name === "alpha" ? { ...skill, name: "alpha-ours" } : skill
      ),
      { repo: "acme/tools", commit: "0123456789abcdef", path: "skills/alpha", name: "alpha" },
    ],
    relisted: ["tools/alpha"],
    ours: { theirs: "alpha", replacedOwn: false, removedReads: [] },
  };
  const theirs = readCall("toolu_theirs", "skill://alpha");
  const own = readCall("toolu_ours", "skill://alpha-ours");
  const { gateway, bodies } = scripted([[theirs, own], [ACT]]);
  const replayed = await replayOnce({ ...BODY, stream: false }, ours, gateway, { copies });
  expect(bodies[1]?.messages.at(-1)).toEqual({
    role: "user",
    content: [
      toolResult(
        "toolu_theirs",
        `[Skill file: ${INSTALLED}/SKILL.md]\n${TOOLS_ALPHA_MD.replace(/\n$/, "")}`,
        false
      ),
      toolResult(
        "toolu_ours",
        `[Skill file: ${oursInstalled}/SKILL.md]\n${oursSkill.replace(/\n$/, "")}`,
        false
      ),
    ],
  });
  expect(replayed.reads).toEqual([
    [
      { skill: "alpha", file: "SKILL.md", by: "skill", from: "copy", isError: false },
      { skill: "alpha-ours", file: "SKILL.md", by: "skill", from: "arm", isError: false },
    ],
  ]);
  expect(replayed.step.message.content).toEqual([theirs, own, ACT]);
  expect(JSON.stringify(bodies)).not.toContain(tools.dir);
  expect(JSON.stringify(bodies)).not.toContain(ours.skillDir);
});

// Catches: a reply in the arm of ours whose whole read by path of the cut's own copy is judged as
// the step, or answered with their text where ours has the file.
test("in ours, a reply's whole read by path of the cut's own copy is answered from our file at that path, or as not found where ours lacks it", async () => {
  const pathCopy = { dir: COPY, shownDir: SHOWN, rows: "hashline" as const };
  const oursSkill = SKILL_MD.replace("name: alpha\n", "name: alpha-ours\n");
  const oursX = "# X\n\nOurs: each step's result is one line.\n";
  const ours = (files: Record<string, string>): Arm => ({
    ...armWith("ours", files),
    skill: "alpha-ours",
    installedDir: path.join(path.dirname(INSTALLED), "alpha-ours"),
    profileSkills: PROFILE_SKILLS.map((skill) =>
      skill.name === "alpha" ? { ...skill, name: "alpha-ours" } : skill
    ),
    ours: { theirs: "alpha", replacedOwn: false, removedReads: [] },
    pathCopy,
  });
  const answers: [Arm, string, boolean][] = [
    [
      ours({ "SKILL.md": oursSkill, "references/x.md": oursX }),
      `[${SHOWN}/references/x.md#${snapshotTag(oursX)}]\n1:# X\n2:\n3:Ours: each step's result is one line.`,
      false,
    ],
    [ours({ "SKILL.md": oursSkill }), `Path '${COPY}/references/x.md' not found`, true],
  ];
  for (const [arm, text, isError] of answers) {
    const read = {
      type: "tool_use",
      id: "toolu_path",
      name: "read",
      input: { path: `${COPY}/references/x.md` },
    };
    const { gateway, bodies } = world((n) =>
      n === 1
        ? jsonReply([read], "tool_use")
        : jsonReply([{ type: "text", text: "Step 4." }], "end_turn")
    );
    const replayed = await replayOnce({ ...BODY, stream: false }, arm, gateway);
    expect(replayed.rounds).toBe(1);
    expect(bodies[1]?.messages.at(-1)).toEqual({
      role: "user",
      content: [
        {
          type: "tool_result",
          tool_use_id: "toolu_path",
          content: [{ type: "text", text }],
          is_error: isError,
        },
      ],
    });
    expect(JSON.stringify(bodies)).not.toContain(arm.skillDir ?? "no directory");
  }
});

// A written case's run: a reply whose calls all read is answered from a copy of its context
// session's checkout at its commit (replay.ts's checkout rounds), made here as a small fixture
// repository; a session case's run gets no such answers.

/** The context session's checkout root and working directory, as a written case names them. */
const SESSION_ROOT = "/work/acme/proj";
const APP_PY = "import os\n\ndef main():\n    return os.getcwd()\n";
const GUIDE_MD = "# Guide\n\nRun the app.\n";
/** A code file of 120 one-line statements, whose parse the pinned Oh My Pi finds no body to elide
 *  in, so it shows it whole. */
const BIG_PY = Array.from({ length: 120 }, (_, n) => `x${n + 1} = ${n + 1}\n`).join("");
/** A code file of 120 lines, a table whose 118 entries the pinned Oh My Pi elides in its summary. */
const TABLE_PY = `${[
  "TABLE = {",
  ...Array.from({ length: 118 }, (_, n) => `    "k${n + 1}": ${n + 1},`),
  "}",
].join("\n")}\n`;

/** A copy of the made-up checkout of acme/proj: src/app.py and docs/guide.md, and `more`, in a
 *  directory of its own, and the session's root and working directory it stands for. */
function checkoutCopy(more: Record<string, string> = {}): {
  dir: string;
  root: string;
  cwd: string;
  git: boolean;
} {
  const dir = path.join(scratch, `checkout-${Math.random()}`);
  writeTree(dir, { "src/app.py": APP_PY, "docs/guide.md": GUIDE_MD, ...more });
  return { dir, root: SESSION_ROOT, cwd: SESSION_ROOT, git: false };
}

/** A tool call `name` with `input`, as a Messages reply holds it. */
function toolCall(id: string, name: string, input: Record<string, unknown>): ContentBlock {
  return { type: "tool_use", id, name, input };
}

/** The act after the inspection: a new test file, which the line governs. */
const WRITE_ACT = toolCall("toolu_write", "write", {
  path: "src/test_app.py",
  content: "from app import main\n",
});

/** The inspection reply: a read, a grep, a find and a cat through bash, as a session's agent
 *  looks before it acts. */
const INSPECTION = [
  toolCall("toolu_read", "read", { path: "docs/guide.md", i: "Reading the guide" }),
  toolCall("toolu_grep", "grep", { pattern: "def ", path: "src" }),
  toolCall("toolu_find", "bash", { command: `find ${SESSION_ROOT}/src -name '*.py'` }),
  toolCall("toolu_cat", "bash", { command: "cat src/app.py | head -2" }),
];

// Catches: a written case judged on its agent's first look at the work (the defect: every judged
// step of test-driven-development's written cases only inspected), an answer that shows the copy's
// own path, or an answer in another form than the pinned Oh My Pi's.
test("a written case's reply whose calls all read is answered from a copy of its checkout in Oh My Pi's forms at the session's paths, and the act after it is judged with those calls", async () => {
  const copy = checkoutCopy();
  const arm = armWith("old", { "SKILL.md": SKILL_MD });
  const { gateway, bodies } = scripted([INSPECTION, [WRITE_ACT]]);
  const replayed = await replayOnce({ ...BODY, stream: false }, arm, gateway, { checkout: copy });
  expect(bodies).toHaveLength(2);
  expect(bodies[1]?.messages.at(-1)).toEqual({
    role: "user",
    content: [
      toolResult(
        "toolu_read",
        `[docs/guide.md#${snapshotTag(GUIDE_MD)}]\n1:# Guide\n2:\n3:Run the app.`,
        false
      ),
      toolResult(
        "toolu_grep",
        `# src/\n## app.py#${snapshotTag(APP_PY)}\n 2:\n*3:def main():\n 4:    return os.getcwd()`,
        false
      ),
      toolResult("toolu_find", `${SESSION_ROOT}/src/app.py\n`, false),
      toolResult("toolu_cat", "import os\n\n", false),
    ],
  });
  expect(JSON.stringify(bodies)).not.toContain(copy.dir);
  expect({ rounds: replayed.rounds, checkoutRounds: replayed.checkoutRounds }).toEqual({
    rounds: 0,
    checkoutRounds: 1,
  });
  expect(replayed.reads).toEqual([
    INSPECTION.map((call) => ({
      from: "checkout",
      tool: call.name as string,
      input: call.input,
      isError: false,
    })),
  ]);
  expect(replayed.step.message.content).toEqual([...INSPECTION, WRITE_ACT]);
});

/** checkoutCopy's copy made a git checkout of one commit with a detached HEAD, as cli.ts's shared
 *  checkout is. */
function gitCheckoutCopy(): CheckoutCopy {
  const copy = checkoutCopy();
  detachedCommit(copy.dir, "Add the app");
  return { ...copy, git: true };
}

// Catches: a written case of the dev box judged on its agent's first look at the work (the defect:
// every judged step of test-driven-development's written cases was such a bash call, which starts
// with `unset`, prints `pwd`, and asks jj or git where it is, in a subshell), or an answer that
// shows the copy's own path in place of the session's.
test("a written case's reply that unsets variables, prints pwd and asks jj or git where it is, in a subshell, is answered from the copy at the session's paths, and the act after it is judged", async () => {
  const copy = gitCheckoutCopy();
  const look = toolCall("toolu_where", "bash", {
    command: `cd ${SESSION_ROOT} && unset GIT_CONFIG_GLOBAL GIT_CONFIG_NOSYSTEM ACME_TOOLS_PIN && pwd && (jj st 2>/dev/null || git status --short) && git worktree list`,
  });
  const top = toolCall("toolu_top", "bash", { command: "git rev-parse --show-toplevel" });
  const { gateway, bodies } = scripted([[look, top], [WRITE_ACT]]);
  const replayed = await replayOnce(
    { ...BODY, stream: false },
    armWith("old", { "SKILL.md": SKILL_MD }),
    gateway,
    { checkout: copy }
  );
  const last = bodies[1]?.messages.at(-1) as { content: { content: { text: string }[] }[] };
  const texts = last.content.map((result) =>
    (result.content[0]?.text ?? "").replace(/\n\nWall time: \d+\.\d\d seconds/, "")
  );
  expect(texts[0]).toMatch(
    new RegExp(`^${SESSION_ROOT}\\n${SESSION_ROOT} +[0-9a-f]{7,} \\(detached HEAD\\)\\n$`)
  );
  expect(texts[1]).toBe(`${SESSION_ROOT}\n`);
  expect(JSON.stringify(bodies)).not.toContain(copy.dir);
  expect(replayed.checkoutRounds).toBe(1);
  expect(replayed.step.message.content).toEqual([look, top, WRITE_ACT]);
});

// Catches: a written case's read of a code file the pin summarizes answered with the file's whole
// text (the defect: every such read showed the arms a text the session's agent never saw), a code
// file the pin shows whole, or a ranged read, left unanswered (such a run was judged on that
// read), or a checkout read recorded as answered whole.
natives.test(
  "a written case's read of a code file the pin summarizes is answered with the pin's summary, one whose parse elides nothing with its whole text, and a ranged read with the lines of its range, each recorded as the checkout answered it",
  async () => {
    const copy = checkoutCopy({ "src/big.py": BIG_PY, "src/table.py": TABLE_PY });
    const arm = armWith("old", { "SKILL.md": SKILL_MD });
    const summarized = toolCall("toolu_summary", "read", { path: "src/table.py" });
    const whole = toolCall("toolu_whole", "read", { path: "src/big.py" });
    const ranged = toolCall("toolu_ranged", "read", { path: "src/big.py:10-12" });
    const { gateway, bodies } = scripted([[summarized, whole, ranged], [WRITE_ACT]]);
    const replayed = await replayOnce({ ...BODY, stream: false }, arm, gateway, { checkout: copy });
    const lines = BIG_PY.replace(/\n$/, "").split("\n");
    const rows = (from: number, to: number) =>
      lines
        .slice(from - 1, to)
        .map((line, index) => `${from + index}:${line}`)
        .join("\n");
    const header = `[src/big.py#${snapshotTag(BIG_PY)}]`;
    expect(bodies[1]?.messages.at(-1)).toEqual({
      role: "user",
      content: [
        // The table's brace pair on one row, and the footer naming its one elided range.
        toolResult(
          "toolu_summary",
          `[src/table.py#${snapshotTag(TABLE_PY)}]\n1-120:TABLE = { … }\n\n[…118ln elided; re-read needed ranges with src/table.py:1-120]`,
          false
        ),
        toolResult("toolu_whole", `${header}\n${rows(1, 120)}`, false),
        // Lines 10 to 12 with the pin's context, a line before and three after.
        toolResult(
          "toolu_ranged",
          `${header}\n${rows(9, 15)}\n\n[Showing lines 9-15 of 120. Use :16 to continue]`,
          false
        ),
      ],
    });
    expect(replayed.reads).toEqual([
      [summarized, whole, ranged].map((call) => ({
        from: "checkout",
        tool: "read",
        input: call.input,
        isError: false,
      })),
    ]);
    expect(replayed.step.message.content).toEqual([summarized, whole, ranged, WRITE_ACT]);
  }
);

// Catches: a written case judged on a reply one of whose read-only calls the copy answers in no
// form, a step every arm takes alike whatever the skill says.
test("a written case's reply one of whose read-only calls the copy answers in no form gets no verdict, as unanswered-read, naming the call and why", async () => {
  const copy = checkoutCopy();
  const log = toolCall("toolu_log", "bash", { command: "git log --oneline" });
  const failed = await replayOnce(
    { ...BODY, stream: false },
    armWith("old", { "SKILL.md": SKILL_MD }),
    scripted([[INSPECTION[0] as ContentBlock, log]]).gateway,
    { checkout: copy }
  ).then(
    () => "judged",
    (error: unknown) => error
  );
  if (!(failed instanceof UnansweredRead)) throw new Error(`not refused: ${String(failed)}`);
  expect(failed.message).toStartWith(`${UNANSWERED_READ}: bash {"command":"git log --oneline"}: `);
  expect(failed.message).toContain("GitHub's tarball of the commit, which holds no history");
  expect({
    checkoutRounds: failed.replayed.checkoutRounds,
    step: failed.replayed.step.message.content,
  }).toEqual({ checkoutRounds: 0, step: [INSPECTION[0], log] });
});

// Catches: a written case with no checkout (one built before build-cases passed over a context
// session whose working directory is in no checkout it knows) judged on a reply whose calls all
// read, which the runner answers in no form, a step every arm takes alike whatever the skill says
// (the defect: such cases floored at 0 of 10 in both arms, each run judged fail on its reads of
// files no checkout holds); or its reads of the skill, or its act, no longer answered and judged.
test("a written case's reply whose calls all read, where the case has no checkout, gets no verdict, as unanswered-read, naming the call and that the case has no checkout; its skill reads are answered and its act is judged", async () => {
  const none = { why: "the case has no checkout" };
  const arm = armWith("old", { "SKILL.md": SKILL_MD, "references/x.md": X_MD });
  const skill = readCall("toolu_skill", "skill://alpha/references/x.md");
  const brief = toolCall("toolu_brief", "read", { path: "/mnt/briefs/plan.md" });
  const grep = INSPECTION[1] as ContentBlock;
  const { gateway, bodies } = scripted([[skill], [brief, grep]]);
  const failed = await replayOnce({ ...BODY, stream: false }, arm, gateway, {
    checkout: none,
  }).then(
    () => "judged",
    (error: unknown) => error
  );
  if (!(failed instanceof UnansweredRead)) throw new Error(`not refused: ${String(failed)}`);
  expect(failed.message).toBe(
    `${UNANSWERED_READ}: read {"path":"/mnt/briefs/plan.md"}: the case has no checkout`
  );
  expect(bodies).toHaveLength(2);
  expect({
    rounds: failed.replayed.rounds,
    checkoutRounds: failed.replayed.checkoutRounds,
    step: failed.replayed.step.message.content,
  }).toEqual({ rounds: 1, checkoutRounds: 0, step: [skill, brief, grep] });
  // A reply that acts is the judged step, as any written case's.
  const judged = await replayOnce(
    { ...BODY, stream: false },
    arm,
    scripted([[brief, WRITE_ACT]]).gateway,
    { checkout: none }
  );
  expect(judged.step.message.content).toEqual([brief, WRITE_ACT]);
});

// Catches: a session case's reply answered from a checkout, which a session case has none of.
test("a session case's reply whose calls all read files of the project is the judged step: no checkout answers it", async () => {
  const { gateway, bodies } = scripted([INSPECTION, [WRITE_ACT]]);
  const replayed = await replayOnce(
    { ...BODY, stream: false },
    armWith("old", { "SKILL.md": SKILL_MD }),
    gateway
  );
  expect(bodies).toHaveLength(1);
  expect({ rounds: replayed.rounds, checkoutRounds: replayed.checkoutRounds }).toEqual({
    rounds: 0,
    checkoutRounds: 0,
  });
  expect(replayed.step.message.content).toEqual(INSPECTION);
  // A ranged read, and a git command, are the judged step too: no read of a session case's is
  // answered from a checkout, so none is left unanswered.
  const other = [
    toolCall("toolu_ranged", "read", { path: "src/app.py:1-2" }),
    toolCall("toolu_log", "bash", { command: "git log --oneline" }),
  ];
  const judged = await replayOnce(
    { ...BODY, stream: false },
    armWith("old", { "SKILL.md": SKILL_MD }),
    scripted([other]).gateway
  );
  expect(judged.step.message.content).toEqual(other);
});

// Catches: a written case's run that reads on and on judged on a read, or answered without end.
test("a written case's reply that still only reads after five answered rounds of the checkout gets no verdict, as reads-past-last-round", async () => {
  expect(MAX_CHECKOUT_ROUNDS).toBe(5);
  const read = INSPECTION[0] as ContentBlock;
  const { gateway, bodies } = scripted(Array(7).fill([read]));
  const failed = await replayOnce(
    { ...BODY, stream: false },
    armWith("old", { "SKILL.md": SKILL_MD }),
    gateway,
    { checkout: checkoutCopy() }
  ).then(
    () => "judged",
    (error: unknown) => error
  );
  expect(bodies).toHaveLength(6);
  if (!(failed instanceof ReadsPastLastRound)) throw new Error(`not refused: ${String(failed)}`);
  expect(failed.message).toBe(
    `${READS_PAST_LAST_ROUND}: after 5 answered rounds of read-only calls answered from the checkout and 0 of skill reads, the reply still only makes read-only calls the checkout answers (read docs/guide.md)`
  );
  expect(failed.replayed.checkoutRounds).toBe(5);
  expect(failed.replayed.step.message.content).toEqual(Array(6).fill(read));
});

// A case whose session read alpha by skill:// only names no copy of it (cases.ts's `pathCopy`):
// its arms take the directory its read's `[Skill file: …]` line shows alpha installed in for one
// (arms.ts's buildArm), so a reply's read by path of a file under it is a read of a skill file.

/** A case of alpha of `kind` whose history read skill://alpha alone, its result under INSTALLED's
 *  `[Skill file: …]` line; a written one's checkout is acme/proj's at SESSION_ROOT. */
function urlOnlyCase(kind: "session" | "written"): Case {
  const result = `[Skill file: ${INSTALLED}/SKILL.md]\n${SKILL_MD.replace(/\n$/, "")}`;
  return {
    id: `c-url-only-${kind}`,
    cut: "acme-skills-1",
    skill: "alpha",
    baseFile: "SKILL.md",
    line: { text: "2. Write each step's result on one line.", lineNumber: 9, lineCount: 10 },
    criterion: {
      pass: "The reply writes each step's result on one line.",
      fail: "The reply spreads a step's result over more than one line.",
    },
    model: "claude-sonnet-5-5",
    thinking: "off",
    context: "devbox",
    profile: { skills: PROFILE_SKILLS, plugins: [], contextFiles: [] },
    messages: [
      { role: "user", content: [{ type: "text", text: "Run the alpha checklist." }] },
      {
        role: "assistant",
        content: [
          {
            type: "toolCall",
            id: "call_alpha",
            name: "read",
            arguments: { path: "skill://alpha" },
          },
        ],
      },
      {
        role: "toolResult",
        toolCallId: "call_alpha",
        toolName: "read",
        content: [{ type: "text", text: result }],
        isError: false,
      },
    ],
    skillReads: [2],
    realStep: { role: "assistant", content: [{ type: "text", text: "Step 1: done." }] },
    violatingStep: { role: "assistant", content: [{ type: "text", text: "Step 1:\ndone." }] },
    source: { store: "store_a", pathHash: pathHash("/sessions/alpha.jsonl"), kind },
    ...(kind === "written"
      ? {
          checkout: {
            repo: "acme/proj",
            commit: "0123456789abcdef0123456789abcdef01234567",
            root: SESSION_ROOT,
            cwd: SESSION_ROOT,
          },
        }
      : {}),
    sessionStart: "2026-11-20T01:59:41.780Z",
    secretsRemoved: 0,
    sessionDigest: null,
  };
}

/** The arm `name` (old, holding alpha's SKILL.md and references/x.md, or none) of `c`. */
function urlOnlyArm(c: Case, name: "old" | "none"): Arm {
  const old = path.join(scratch, `url-only-${Math.random()}`);
  writeTree(old, { "SKILL.md": SKILL_MD, "references/x.md": X_MD });
  return buildArm(c, name, { old }, path.join(scratch, `url-only-scratch-${Math.random()}`));
}

/** A read by path of alpha's references/x.md under the installed directory. */
const INSTALLED_X = readCall("toolu_x", `${INSTALLED}/references/x.md`);

/** The answer to INSTALLED_X from the arm's files (0098 is Oh My Pi's tag of X_MD), or, in none,
 *  which installs no alpha, Oh My Pi's not-found result for the path read. */
const INSTALLED_X_ANSWERS = {
  old: toolResult(
    "toolu_x",
    `[${INSTALLED}/references/x.md#0098]\n1:# X\n2:\n3:Each alpha step's result is one short line.`,
    false
  ),
  none: toolResult("toolu_x", `Path '${INSTALLED}/references/x.md' not found`, true),
};

// Catches: a session case that read its skill by skill:// only judged on a reply that reads
// skill://alpha and a file under its installed directory by path together (the defect: 11 judged
// replies of ce-simplify-code's cases), or that read answered with one text in every arm.
test("in a session case that read its skill by skill:// only, a reply's read of skill://alpha and a read by path of a file under the installed directory its header shows are answered, from the arm's files in old and in none with Oh My Pi's not-found result for the path, and the run goes on to the act", async () => {
  const c = urlOnlyCase("session");
  const url = readCall("toolu_url", "skill://alpha");
  const skillAnswers = {
    old: toolResult(
      "toolu_url",
      `[Skill file: ${INSTALLED}/SKILL.md]\n${SKILL_MD.replace(/\n$/, "")}`,
      false
    ),
    none: toolResult("toolu_url", "Unknown skill: alpha\nAvailable: beta", true),
  };
  for (const name of ["old", "none"] as const) {
    const arm = urlOnlyArm(c, name);
    const { gateway, bodies } = scripted([[url, INSTALLED_X], [ACT]]);
    const replayed = await replayOnce({ ...BODY, stream: false }, arm, gateway);
    expect(bodies).toHaveLength(2);
    expect(bodies[1]?.messages.at(-1)).toEqual({
      role: "user",
      content: [skillAnswers[name], INSTALLED_X_ANSWERS[name]],
    });
    const from = name === "old" ? "arm" : "absent";
    expect(replayed.reads).toEqual([
      [
        { skill: "alpha", file: "SKILL.md", by: "skill", from, isError: name === "none" },
        { skill: "alpha", file: "references/x.md", by: "path", from, isError: name === "none" },
      ],
    ]);
    expect(replayed.rounds).toBe(1);
    expect(replayed.step.message.content).toEqual([url, INSTALLED_X, ACT]);
    expect(JSON.stringify(bodies)).not.toContain(arm.skillDir ?? "no directory");
  }
});

// Catches: a written case that read its skill by skill:// only whose reply reads a file under the
// installed directory by path beside a file of the checkout left with no verdict, as
// unanswered-read (the defect: 94 runs of using-subagents' written cases), or that read answered
// from the checkout.
test("in a written case that read its skill by skill:// only, a reply's read by path of a file under the installed directory and its read of a checkout file are answered in one round, each from its own source, and the act after it is judged", async () => {
  const c = urlOnlyCase("written");
  const guide = INSPECTION[0] as ContentBlock;
  const guideAnswer = toolResult(
    "toolu_read",
    `[docs/guide.md#${snapshotTag(GUIDE_MD)}]\n1:# Guide\n2:\n3:Run the app.`,
    false
  );
  for (const name of ["old", "none"] as const) {
    const arm = urlOnlyArm(c, name);
    const copy = checkoutCopy();
    const { gateway, bodies } = scripted([[INSTALLED_X, guide], [WRITE_ACT]]);
    const replayed = await replayOnce({ ...BODY, stream: false }, arm, gateway, {
      checkout: copy,
    });
    expect(bodies).toHaveLength(2);
    expect(bodies[1]?.messages.at(-1)).toEqual({
      role: "user",
      content: [INSTALLED_X_ANSWERS[name], guideAnswer],
    });
    expect({ rounds: replayed.rounds, checkoutRounds: replayed.checkoutRounds }).toEqual({
      rounds: 0,
      checkoutRounds: 1,
    });
    expect(replayed.reads).toEqual([
      [
        {
          skill: "alpha",
          file: "references/x.md",
          by: "path",
          from: name === "old" ? "arm" : "absent",
          isError: name === "none",
        },
        { from: "checkout", tool: "read", input: guide.input, isError: false },
      ],
    ]);
    expect(replayed.step.message.content).toEqual([INSTALLED_X, guide, WRITE_ACT]);
    expect(JSON.stringify(bodies)).not.toContain(copy.dir);
  }
});

// Catches: a read of a directory under the installed directory answered as a file of the skill.
test("in a case that read its skill by skill:// only, a read of a directory under the installed directory is no skill read: a session case's run is judged on it, and a written case's gets no verdict, as unanswered-read", async () => {
  const listing = readCall("toolu_dir", `${INSTALLED}/references`);
  const session = urlOnlyArm(urlOnlyCase("session"), "old");
  const { gateway, bodies } = scripted([[listing], [ACT]]);
  const judged = await replayOnce({ ...BODY, stream: false }, session, gateway);
  expect(bodies).toHaveLength(1);
  expect({ rounds: judged.rounds, step: judged.step.message.content }).toEqual({
    rounds: 0,
    step: [listing],
  });

  const written = urlOnlyArm(urlOnlyCase("written"), "old");
  const failed = await replayOnce(
    { ...BODY, stream: false },
    written,
    scripted([[listing, INSPECTION[0] as ContentBlock], [WRITE_ACT]]).gateway,
    { checkout: checkoutCopy() }
  ).then(
    () => "judged",
    (error: unknown) => error
  );
  if (!(failed instanceof UnansweredRead)) throw new Error(`not refused: ${String(failed)}`);
  expect(failed.message).toBe(
    `${UNANSWERED_READ}: read {"path":"${INSTALLED}/references"}: ${INSTALLED}/references is no file of the session's checkout ${SESSION_ROOT}`
  );
  expect(failed.replayed.checkoutRounds).toBe(0);
});
