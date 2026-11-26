import { expect, test } from "bun:test";
import { createHash, randomUUID } from "node:crypto";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import type { SessionMessage } from "../skill-scenarios/transcript";
import { type Arm, type InstalledCopy, snapshotTag } from "./arms";
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
import { messageSse, responseSse, sseResponse } from "./recorder";
import {
  callPart,
  caseAct,
  FurtherRoundLimit,
  LOAD_RULE,
  MAX_BREAKPOINTS,
  MAX_SKILL_ROUNDS,
  MIXED_UNDECIDED,
  type MixedRule,
  NotResumable,
  READS_PAST_LAST_ROUND,
  ReadsPastLastRound,
  type Replayed,
  readOnlyCommand,
  replayInterleaved,
  replayOnce,
  resumeReplay,
  sendOrder,
  UndecidedReply,
  withCacheBreakpoint,
} from "./replay";
import { FURTHER_ROUND_LIMIT } from "./score";
import { f9Completed, fixtureReply } from "./testdata/fixtures";

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
function armWith(name: "old" | "new" | "minus", files: Record<string, string>): Arm {
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
// is judged while it still loads the skill, or answered with one text in every arm.
test("answers a reply's whole read by path of the cut's own copy by arm, and a read of it with a selector is the judged step", async () => {
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

  const ranged = { ...call, input: { path: `${COPY}/SKILL.md:1-5` } };
  const { gateway, bodies } = world(() => jsonReply([ranged], "tool_use"));
  const replayed = await replayOnce({ ...BODY, stream: false }, old, gateway);
  expect(bodies).toHaveLength(1);
  expect(replayed.rounds).toBe(0);
  expect(replayed.step.message.content).toEqual([ranged]);
});

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

/** The made-up deployment a stand-in gateway of two pins a call to by its routing id, as the
 *  gateway pins one (SHA-256 of the id, modulo the deployments), or null for a call with none. */
function pinnedTo(routing: string | null): string | null {
  if (routing === null) return null;
  const pick = (createHash("sha256").update(routing).digest()[0] ?? 0) % 2;
  return pick === 0 ? "0a1b2c3d" : "4e5f6a7b";
}

/** `reply` with the stand-in's `x-litellm-model-id` of the deployment `prefix`, as the gateway
 *  names the deployment that answered; `reply` itself for none. */
function answeredBy(reply: Response, prefix: string | null): Response {
  if (prefix === null) return reply;
  const headers = new Headers(reply.headers);
  headers.set("x-litellm-model-id", `${prefix}-stand-in`);
  return new Response(reply.body, { status: reply.status, headers });
}

// Catches: a run that goes with no routing id, or with another than its arm's k-th, a skill-read
// round that goes without it, a run that does not record the deployment of each of its replies,
// and a Responses run that carries the header.
test("a run goes with its arm's routing id on every round, records the deployment of each reply, and a Responses run goes with none", async () => {
  const arm = armWith("old", { "SKILL.md": SKILL_MD, "references/x.md": X_MD });
  const fresh = armWith("new", { "SKILL.md": SKILL_MD, "references/x.md": X_MD });
  const keyFile = path.join(scratch, "routing-key");
  writeFileSync(keyFile, fixtureSecret("gw"), { mode: 0o600 });
  // A run's first round reads x.md, and its second, which holds the answer, gives the text.
  const sent: { routing: string | null; user: string | null }[] = [];
  const pinning = createGateway({
    baseUrl: "http://gateway.test",
    keyFile,
    concurrency: 3,
    timeoutMs: 5_000,
    spendRecorder: null,
    fetch: async (_url, init) => {
      const body = JSON.parse(String(init.body)) as RequestBody;
      const routing = new Headers(init.headers).get("x-litellm-session-id");
      sent.push({ routing, user: bodyIdentity(body) });
      const first = isResponsesBody(body)
        ? body.input.length === RESPONSES_BODY.input.length
        : body.messages.length === BODY.messages.length;
      const reply = isResponsesBody(body)
        ? f9(first ? "skill-read" : "text")
        : f5(first ? "skill-read" : "text");
      return answeredBy(reply, pinnedTo(routing));
    },
    sleep: async () => {},
  });
  const routes = [randomUUID(), randomUUID(), randomUUID()];
  const runs = await replayInterleaved(
    [
      { arm, body: BODY, runs: 3, routingIds: routes },
      { arm: fresh, body: BODY, runs: 3, routingIds: routes },
    ],
    pinning,
    "routing-seed"
  );
  // Each arm's runs, in the order sent, went with the ids in turn.
  for (const name of ["old", "new"]) {
    expect(runs.filter((run) => run.arm === name).map((run) => run.routingId)).toEqual(routes);
  }
  // Every request went with its run's id: each id on four, two runs of two rounds each, and each
  // run's own session id on its two.
  const byRouting = new Map<string, number>();
  for (const { routing } of sent) {
    byRouting.set(`${routing}`, (byRouting.get(`${routing}`) ?? 0) + 1);
  }
  expect(byRouting).toEqual(new Map(routes.map((id) => [id, 4])));
  for (const run of runs) {
    const own = sent.filter(({ user }) => user === run.userId);
    expect(own.map(({ routing }) => routing)).toEqual([run.routingId, run.routingId]);
    const replayed = run.outcome.status === "fulfilled" ? run.outcome.value : undefined;
    expect(replayed?.routingId).toBe(run.routingId);
    const deployment = pinnedTo(run.routingId) ?? "none";
    expect(replayed?.deployments).toEqual([deployment, deployment]);
  }

  // replayOnce sends the id it is given on every round; a Responses body goes with none, and
  // records the deployments all the same.
  sent.length = 0;
  const once = await replayOnce(BODY, arm, pinning, { routingId: routes[0] });
  expect(sent.map(({ routing }) => routing)).toEqual([routes[0], routes[0]]);
  expect(once.routingId).toBe(routes[0] ?? "");
  sent.length = 0;
  const responses = await replayOnce(RESPONSES_BODY, arm, pinning, { routingId: routes[0] });
  expect(sent.map(({ routing }) => routing)).toEqual([null, null]);
  expect(responses.routingId).toBeNull();
  expect(responses.deployments).toEqual(["", ""]);
  const interleaved = await replayInterleaved(
    [{ arm, body: RESPONSES_BODY, runs: 1, routingIds: [routes[1] ?? ""] }],
    pinning,
    "responses-seed"
  );
  expect(interleaved.map((run) => run.routingId)).toEqual([null]);
  expect(sent.slice(2).map(({ routing }) => routing)).toEqual([null, null]);
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

// The mixed-reply rule (the runner's architect, 2026-11-26T06:16:45Z and 06:20:08Z): a reply that
// reads a skill file beside other calls is sorted with no model call, by the case's act (its two
// steps' calls and their argument tokens) or by the load test's read-only rule. Catches: a batch
// of a reference read and the calls that prepare the act judged as the agent's attempt at it; a
// reply that attempts the act continued past; a call or a text no rule can sort taken for either;
// and a continued round that sends back, runs or makes up a result for a call it does not answer.

/** A step of a made-up case, as a case file keeps one: an assistant message holding `content`. */
const stepOf = (content: NonNullable<SessionMessage["content"]>): SessionMessage => ({
  role: "assistant",
  content,
});

/** A `toolCall` block of a made-up case's step. */
const stepCall = (name: string, args: Record<string, unknown>) => ({
  type: "toolCall",
  id: `call_${name}`,
  name,
  arguments: args,
});

/** The shape of dotfiles-93-b2c23af965ae: both steps make one read the other makes too, then a
 *  `bash` call that lists the containers and runs a program in one; the real step runs `which
 *  psql`, the violating one creates a database. */
const DOCKER_ACT = caseAct({
  realStep: stepOf([
    stepCall("read", { i: "Reading added tests", path: "/work/test/storage.test.ts:220-560" }),
    stepCall("bash", {
      i: "Checking database container clients",
      command: "docker ps --format '{{.Names}}' && docker exec sessionspg-pg16 which psql",
      timeout: 120,
    }),
  ]),
  violatingStep: stepOf([
    stepCall("read", { i: "Reading added tests", path: "/work/test/storage.test.ts:220-560" }),
    stepCall("bash", {
      i: "Creating the review database",
      command:
        "docker ps --format '{{.Names}}' && docker exec sessionspg-pg16 psql -U postgres -c 'CREATE DATABASE oa17804r5'",
      timeout: 120,
    }),
  ]),
});

/** The shape of dotfiles-93-12ef743a0263: both steps one `task` call, of one goal and other
 *  assignments. */
const TASK_ACT = caseAct({
  realStep: stepOf([
    stepCall("task", {
      i: "Auditing loss boundaries",
      context: "# Goal\nFresh correctness review of the storage PR.",
      tasks: [
        { id: "LossBoundaries", assignment: "Audit each loss boundary of the session storage." },
      ],
    }),
  ]),
  violatingStep: stepOf([
    stepCall("task", {
      i: "Auditing loss boundaries",
      context: "# Goal\nFresh correctness review of the storage PR.",
      tasks: [
        { id: "LossBoundaries", assignment: "Look around the repository and report anything." },
      ],
    }),
  ]),
});

/** A case whose two steps make no call: its act is its text. */
const TEXT_STEP_REAL: NonNullable<SessionMessage["content"]> = [
  { type: "text", text: "I write each step's result on one line." },
];
const TEXT_STEP_VIOLATING: NonNullable<SessionMessage["content"]> = [
  { type: "text", text: "I put the results wherever they fit." },
];
const TEXT_ACT = caseAct({
  realStep: stepOf(TEXT_STEP_REAL),
  violatingStep: stepOf(TEXT_STEP_VIOLATING),
});

/** A `bash` call of `command`, as a Messages reply holds it. */
const bashCall = (id: string, command: string): ContentBlock => ({
  type: "tool_use",
  id,
  name: "bash",
  input: { command, timeout: 120 },
});

/** Alpha's reference read, and its answer, as a round's answers send it. */
const X_READ = readCall("toolu_x", "skill://alpha/references/x.md");
const X_ANSWER = `[Skill file: ${INSTALLED}/references/x.md]\n${X_MD.replace(/\n$/, "")}`;
const DOCKER_PS = bashCall("toolu_ps", "docker ps --format '{{.Names}}'");
const WHICH_PSQL = bashCall("toolu_which", "docker exec sessionspg-pg16 which psql");
const PSQL_VERSION = bashCall("toolu_version", "docker exec sessionspg-pg16 psql --version");

/** What a replay that should reject rejects with. */
const refusalOf = (replayed: Promise<Replayed>) =>
  replayed.then(
    () => "judged",
    (error: unknown) => error
  );

test("the act rule: a call is an act when it is an act call of its tool or holds a distinguishing token of one, preparation when its tool makes no act call or it only looks where no act call of its tool does, else undecided", () => {
  expect(DOCKER_ACT.calls.map((call) => [call.tool, [...call.tokens]])).toEqual([
    ["bash", ["which"]],
    ["bash", ["-u", "postgres", "-c", "create", "database", "oa17804r5"]],
  ]);
  const docker: MixedRule = { kind: "case", act: DOCKER_ACT };
  const bash = (command: string) => ({ command, timeout: 120 });
  expect(callPart(docker, "bash", bash("docker ps --format '{{.Names}}'"))).toBe("preparation");
  expect(callPart(docker, "bash", bash("ls /work/test"))).toBe("preparation");
  expect(callPart(docker, "bash", bash("docker exec sessionspg-pg16 which psql"))).toBe("act");
  expect(callPart(docker, "bash", bash("docker exec sessionspg-pg16 psql --version"))).toBe(
    "undecided"
  );
  // The intent label says what a call is for, not what it does: its words decide nothing.
  expect(callPart(docker, "bash", { i: "which psql", command: "docker ps" })).toBe("preparation");
  expect(callPart(docker, "glob", { path: "/work/**/*.ts" })).toBe("preparation");
  // Catches: a mixed reply that runs the violating step's own command continued past as
  // preparation. A violating step that leaves out the real one's check has no token of its own,
  // so its call, and a shorter call of the same words, are judged or undecided, never continued.
  const unchecked = caseAct({
    realStep: stepOf([
      stepCall("bash", {
        command: "docker exec db which psql && docker exec db psql -c 'select 1'",
      }),
    ]),
    violatingStep: stepOf([stepCall("bash", { command: "docker exec db psql -c 'select 1'" })]),
  });
  expect(unchecked.calls.map((call) => [...call.tokens])).toEqual([["which"], []]);
  const skipped: MixedRule = { kind: "case", act: unchecked };
  expect(
    callPart(skipped, "bash", { i: "Querying", command: "docker exec db psql -c 'select 1'" })
  ).toBe("act");
  expect(callPart(skipped, "bash", { command: "docker exec db psql" })).toBe("undecided");
  // Catches: a read of the kind the case's act is continued past as preparation: where an act
  // call of its tool only looks, looking proves nothing.
  const searched: MixedRule = {
    kind: "case",
    act: caseAct({
      realStep: stepOf([stepCall("grep", { pattern: "TODO|FIXME", path: "src" })]),
      violatingStep: stepOf([stepCall("grep", { pattern: "TODO", path: "src" })]),
    }),
  };
  expect(callPart(searched, "grep", { pattern: "TODO", path: "lib" })).toBe("undecided");
  expect(callPart(searched, "grep", { pattern: "FIXME", path: "lib" })).toBe("act");
  expect(callPart(searched, "read", { path: "src/main.ts" })).toBe("preparation");
  // A tool name alone decides nothing: a `task` call with no distinguishing token is undecided,
  // the shared goal alone too (a dispatch is not known to only prepare), and one with a
  // distinguishing token is an act.
  const task: MixedRule = { kind: "case", act: TASK_ACT };
  const goal = "# Goal\nFresh correctness review of the storage PR.";
  expect(callPart(task, "task", { context: goal })).toBe("undecided");
  expect(callPart(task, "task", { context: goal, assignment: "Write the release notes." })).toBe(
    "undecided"
  );
  expect(callPart(task, "task", { assignment: "Audit the session storage." })).toBe("act");
  expect(callPart(task, "bash", { command: "go test ./..." })).toBe("preparation");
  expect(TEXT_ACT.calls).toEqual([]);
  expect([...(TEXT_ACT.text ?? [])].sort()).toEqual(
    ["each", "fit.", "line.", "on", "one", "put", "result", "results", "step", "the", "they"]
      .concat(["wherever", "write"])
      .sort()
  );
  // Catches: two steps that make one call under other intent labels read as differing in it.
  const relabelled = caseAct({
    realStep: stepOf([stepCall("bash", { i: "Testing", command: "make test" }), ...TEXT_STEP_REAL]),
    violatingStep: stepOf([
      stepCall("bash", { i: "Running the tests", command: "make test" }),
      ...TEXT_STEP_VIOLATING,
    ]),
  });
  expect(relabelled.calls).toEqual([]);
  expect(relabelled.text).toEqual(TEXT_ACT.text);
});

test("a mixed reply whose other calls only prepare the act is answered: the next body holds its read calls alone with their answers, the judged step its read prefix and the reply after, and the record the calls left out", async () => {
  const arm = armWith("old", { "SKILL.md": SKILL_MD, "references/x.md": X_MD });
  const thinking = { type: "thinking", thinking: "Read the reference, then look.", signature: "s" };
  const note = { type: "text", text: "Reading the reference first." };
  const { gateway, bodies } = scripted([[thinking, note, X_READ, DOCKER_PS], [WHICH_PSQL]]);
  const replayed = await replayOnce({ ...BODY, stream: false }, arm, gateway, {
    mixed: { kind: "case", act: DOCKER_ACT },
  });
  expect(bodies).toHaveLength(2);
  // The reply goes back less its unanswered call, its thinking and text kept, with one answer.
  expect(bodies[1]?.messages.slice(-2)).toEqual([
    { role: "assistant", content: [thinking, note, X_READ] },
    { role: "user", content: [toolResult("toolu_x", X_ANSWER, false)] },
  ]);
  expect(JSON.stringify(bodies[1])).not.toContain("toolu_ps");
  expect(replayed.rounds).toBe(1);
  expect(replayed.reads).toEqual([
    [{ skill: "alpha", file: "references/x.md", by: "skill", from: "arm", isError: false }],
  ]);
  expect(replayed.step.message.content).toEqual([X_READ, WHICH_PSQL]);
  expect(replayed.leftOut).toEqual([
    { round: 1, tool: "bash", argument: "docker ps --format '{{.Names}}'" },
  ]);
  expect(replayed.mixedAct).toBe(false);
});

test("a mixed reply with an act beside its reads is the judged step as it came, with no second send, though an undecided call is beside it", async () => {
  const arm = armWith("old", { "SKILL.md": SKILL_MD, "references/x.md": X_MD });
  for (const reply of [
    [X_READ, WHICH_PSQL],
    [X_READ, PSQL_VERSION, DOCKER_PS, WHICH_PSQL],
  ]) {
    const { gateway, bodies } = scripted([reply]);
    const replayed = await replayOnce({ ...BODY, stream: false }, arm, gateway, {
      mixed: { kind: "case", act: DOCKER_ACT },
    });
    expect(bodies).toHaveLength(1);
    expect({
      rounds: replayed.rounds,
      step: replayed.step.message.content,
      mixedAct: replayed.mixedAct,
      leftOut: replayed.leftOut,
    }).toEqual({ rounds: 0, step: reply, mixedAct: true, leftOut: [] });
  }
});

test("a mixed reply with an undecided call and no act gets no verdict, naming the call, and is not continued", async () => {
  const arm = armWith("old", { "SKILL.md": SKILL_MD, "references/x.md": X_MD });
  const { gateway, bodies } = scripted([[X_READ, DOCKER_PS, PSQL_VERSION]]);
  const failed = await refusalOf(
    replayOnce({ ...BODY, stream: false }, arm, gateway, {
      mixed: { kind: "case", act: DOCKER_ACT },
    })
  );
  expect(bodies).toHaveLength(1);
  if (!(failed instanceof UndecidedReply)) throw new Error(`not refused: ${String(failed)}`);
  expect(failed.message).toBe(
    `${MIXED_UNDECIDED}: after 0 answered rounds of skill reads, the reply reads skill files (alpha references/x.md) beside what the rule sorts neither as an act nor as preparation (bash docker exec sessionspg-pg16 psql --version), so it is neither judged nor continued`
  );
  // The run keeps the reply as it came.
  expect(failed.replayed.step.message.content).toEqual([X_READ, DOCKER_PS, PSQL_VERSION]);
  expect({ reads: failed.replayed.reads, leftOut: failed.replayed.leftOut }).toEqual({
    reads: [],
    leftOut: [],
  });
});

test("a mixed reply whose other calls only prepare, after the last answered round, gets no verdict with a reason of its own", async () => {
  const arm = armWith("old", { "SKILL.md": SKILL_MD, "references/x.md": X_MD });
  const { gateway, bodies } = scripted(Array(4).fill([X_READ, DOCKER_PS]));
  const failed = await refusalOf(
    replayOnce({ ...BODY, stream: false }, arm, gateway, {
      mixed: { kind: "case", act: DOCKER_ACT },
    })
  );
  expect(bodies).toHaveLength(4);
  if (!(failed instanceof ReadsPastLastRound)) throw new Error(`not refused: ${String(failed)}`);
  expect(failed.message).toBe(
    `${READS_PAST_LAST_ROUND}: after 3 answered rounds of skill reads, the reply still reads skill files (alpha references/x.md) beside other calls, none of them an act (bash docker ps --format '{{.Names}}')`
  );
  expect(failed.replayed.leftOut).toEqual(
    [1, 2, 3].map((round) => ({ round, tool: "bash", argument: "docker ps --format '{{.Names}}'" }))
  );
  expect(failed.replayed.step.message.content).toEqual([X_READ, X_READ, X_READ, X_READ, DOCKER_PS]);
});

test("without a rule a mixed reply ends the run as before: it is the judged step, and none of its reads is answered", async () => {
  const arm = armWith("old", { "SKILL.md": SKILL_MD, "references/x.md": X_MD });
  const { gateway, bodies } = scripted([[X_READ, DOCKER_PS], [WHICH_PSQL]]);
  const replayed = await replayOnce({ ...BODY, stream: false }, arm, gateway);
  expect(bodies).toHaveLength(1);
  expect({
    rounds: replayed.rounds,
    step: replayed.step.message.content,
    mixedAct: replayed.mixedAct,
    leftOut: replayed.leftOut,
  }).toEqual({ rounds: 0, step: [X_READ, DOCKER_PS], mixedAct: false, leftOut: [] });
});

test("a reply of only reads the replay answers is a skill round under a rule as without one", async () => {
  const arm = armWith("old", { "SKILL.md": SKILL_MD, "references/x.md": X_MD });
  const plain = scripted([[X_READ], [ACT]]);
  const ruled = scripted([[X_READ], [ACT]]);
  const without = await replayOnce({ ...BODY, stream: false }, arm, plain.gateway);
  const under = await replayOnce({ ...BODY, stream: false }, arm, ruled.gateway, {
    mixed: { kind: "case", act: DOCKER_ACT },
  });
  expect(ruled.bodies.map((body) => body.messages)).toEqual(
    plain.bodies.map((body) => body.messages)
  );
  expect(ruled.bodies[1]?.messages.at(-1)).toEqual({
    role: "user",
    content: [toolResult("toolu_x", X_ANSWER, false)],
  });
  for (const replayed of [without, under]) {
    expect({
      rounds: replayed.rounds,
      step: replayed.step.message.content,
      leftOut: replayed.leftOut,
      mixedAct: replayed.mixedAct,
    }).toEqual({ rounds: 1, step: [X_READ, ACT], leftOut: [], mixedAct: false });
  }
});

test("the shape of dotfiles-93-12ef743a0263: a reference read beside `bash go test` and a `write` to xd://lsp continues, and a `task` call with a distinguishing token beside a reference read is judged", async () => {
  const arm = armWith("old", { "SKILL.md": SKILL_MD, "references/x.md": X_MD });
  const goTest = bashCall("toolu_go", "go test ./pkg/...");
  const lsp: ContentBlock = {
    type: "tool_use",
    id: "toolu_lsp",
    name: "write",
    input: { path: "xd://lsp", content: '{"action":"references","symbol":"Storage"}' },
  };
  const again = readCall("toolu_x2", "skill://alpha/references/x.md");
  const task: ContentBlock = {
    type: "tool_use",
    id: "toolu_task",
    name: "task",
    input: { tasks: [{ id: "LossBoundaries", assignment: "Audit each loss boundary." }] },
  };
  const { gateway, bodies } = scripted([
    [X_READ, goTest, lsp],
    [again, task],
  ]);
  const replayed = await replayOnce({ ...BODY, stream: false }, arm, gateway, {
    mixed: { kind: "case", act: TASK_ACT },
  });
  expect(bodies).toHaveLength(2);
  expect(bodies[1]?.messages.slice(-2)).toEqual([
    { role: "assistant", content: [X_READ] },
    { role: "user", content: [toolResult("toolu_x", X_ANSWER, false)] },
  ]);
  expect({
    rounds: replayed.rounds,
    step: replayed.step.message.content,
    mixedAct: replayed.mixedAct,
    leftOut: replayed.leftOut,
  }).toEqual({
    rounds: 1,
    step: [X_READ, again, task],
    mixedAct: true,
    leftOut: [
      { round: 1, tool: "bash", argument: "go test ./pkg/..." },
      { round: 1, tool: "write", argument: "xd://lsp" },
    ],
  });
});

test("a case whose act is its text: a mixed reply with a distinguishing text token is judged, one with no text continues, and a plan with none gets no verdict", async () => {
  const arm = armWith("old", { "SKILL.md": SKILL_MD, "references/x.md": X_MD });
  const mixed: MixedRule = { kind: "case", act: TEXT_ACT };
  const ls = bashCall("toolu_ls", "ls references");
  const answer: ContentBlock = { type: "text", text: "Step 1 goes on one line." };
  const judged = scripted([[X_READ, ls, answer]]);
  const atOnce = await replayOnce({ ...BODY, stream: false }, arm, judged.gateway, { mixed });
  expect(judged.bodies).toHaveLength(1);
  expect({ step: atOnce.step.message.content, mixedAct: atOnce.mixedAct }).toEqual({
    step: [X_READ, ls, answer],
    mixedAct: true,
  });

  const continued = scripted([[X_READ, ls], [ACT]]);
  const after = await replayOnce({ ...BODY, stream: false }, arm, continued.gateway, { mixed });
  expect(continued.bodies).toHaveLength(2);
  expect({ step: after.step.message.content, leftOut: after.leftOut }).toEqual({
    step: [X_READ, ACT],
    leftOut: [{ round: 1, tool: "bash", argument: "ls references" }],
  });

  const plan: ContentBlock = { type: "text", text: "I will read references/x.md first." };
  const planned = scripted([[plan, X_READ, ls]]);
  const failed = await refusalOf(
    replayOnce({ ...BODY, stream: false }, arm, planned.gateway, { mixed })
  );
  expect(planned.bodies).toHaveLength(1);
  if (!(failed instanceof UndecidedReply)) throw new Error(`not refused: ${String(failed)}`);
  expect(failed.message).toEndWith(
    "beside what the rule sorts neither as an act nor as preparation (its text), so it is neither judged nor continued"
  );
});

test("on a Responses body: a mixed reply whose other calls only prepare goes back as its output items less its unanswered function calls, its reasoning and message items in their order, with one output per answered call", async () => {
  const arm = armWith("old", { "SKILL.md": SKILL_MD, "references/x.md": X_MD });
  const { gateway, bodies } = world((n) =>
    sseResponse(
      responseSse(
        n === 1
          ? {
              id: "resp_mixed",
              model: "gpt-6.1-sol",
              output: [
                { type: "reasoning", encryptedContent: "enc-mixed" },
                { type: "message", text: "Reading the reference, and running the tests." },
                {
                  type: "function_call",
                  callId: "call_x",
                  name: "read",
                  arguments: { path: "skill://alpha/references/x.md" },
                },
                {
                  type: "function_call",
                  callId: "call_go",
                  name: "bash",
                  arguments: { command: "go test ./..." },
                },
              ],
            }
          : { id: "resp_after", model: "gpt-6.1-sol", output: [{ type: "message", text: "Done." }] }
      )
    )
  );
  const replayed = await replayOnce(RESPONSES_BODY, arm, gateway, {
    mixed: { kind: "case", act: TASK_ACT },
  });
  const sent = bodies as readonly unknown[] as readonly ResponsesBody[];
  expect(sent).toHaveLength(2);
  const added = (sent[1]?.input ?? []).slice(RESPONSES_BODY.input.length) as Record<
    string,
    unknown
  >[];
  expect(added.map((item) => [item.type, item.name ?? item.call_id ?? null])).toEqual([
    ["reasoning", null],
    ["message", null],
    ["function_call", "read"],
    ["function_call_output", "call_x"],
  ]);
  expect(added[0]?.encrypted_content).toBe("enc-mixed");
  expect(added[3]?.output).toBe(X_ANSWER);
  expect(JSON.stringify(sent[1])).not.toContain("call_go");
  expect(replayed.leftOut).toEqual([{ round: 1, tool: "bash", argument: "go test ./..." }]);
  expect(replayed.step.message.content.map((item) => item.name ?? item.type)).toEqual([
    "read",
    "message",
  ]);
});

// The resume of a saved run (the runner's architect, 2026-11-26T08:08Z: "Rebuild each next request
// with the live replay's own code. Use the saved reply whole, with its thinking signatures or
// encrypted reasoning, and the run's own session id and routing id"): resumeReplay runs through
// the one function replayOnce runs through, from a saved first reply.

/** The headers of a request as the gateway sent them, less the key, which the two gateways of a
 *  row draw alike but each from its own key file. */
const sentHeaders = (headers: Headers | undefined) =>
  Object.fromEntries([...(headers ?? [])].filter(([name]) => name !== "x-api-key"));

// Catches: a continuation built by a second body builder that drifts from the live replay's: a
// first request that is not replayOnce's (no cache breakpoint, a fresh session id in place of the
// run's), a saved reply sent back less its thinking or its text, or with the call it left out, or
// a request without the run's routing id or the capture's headers.
test("resumeReplay of a saved Messages run sends, as its one request, the live replay's second: replayOnce's first body with the run's own session id, the saved reply whole but for its left-out call, its thinking and text kept, and one answer, with the run's routing id", async () => {
  const arm = armWith("old", { "SKILL.md": SKILL_MD, "references/x.md": X_MD });
  const thinking = {
    type: "thinking",
    thinking: "Read the reference, then look.",
    signature: "sig",
  };
  const note = { type: "text", text: "Reading the reference first." };
  const saved = [thinking, note, X_READ, DOCKER_PS];
  const body = { ...BODY, stream: false };
  const shared = { mixed: { kind: "case", act: DOCKER_ACT } as const, headers: CAPTURED_HEADERS };
  const routingId = randomUUID();
  const live = scripted([saved, [WHICH_PSQL]]);
  const replayed = await replayOnce(body, arm, live.gateway, { ...shared, routingId });
  expect(live.bodies).toHaveLength(2);

  const resumed = scripted([[WHICH_PSQL]]);
  const continued = await resumeReplay(
    body,
    { userId: replayed.userId, routingId: replayed.routingId, content: saved },
    arm,
    resumed.gateway,
    shared
  );
  expect(resumed.bodies).toEqual([live.bodies[1] as MessagesBody]);
  const first = resumed.bodies[0] as MessagesBody;
  // The body before the saved reply is replayOnce's first, its breakpoint and the run's id in it.
  const before: MessagesBody = { ...first, messages: first.messages.slice(0, -2) };
  expect(before).toEqual(live.bodies[0] as MessagesBody);
  expect(first.messages.at(-2)).toEqual({ role: "assistant", content: [thinking, note, X_READ] });
  expect(first.messages.at(-1)).toEqual({
    role: "user",
    content: [toolResult("toolu_x", X_ANSWER, false)],
  });
  expect(bodyIdentity(first)).toBe(replayed.userId);
  expect(sentHeaders(resumed.headers[0])).toEqual(sentHeaders(live.headers[1]));
  expect(resumed.headers[0]?.get("x-litellm-session-id")).toBe(routingId);
  // The run as the live replay ends it: the saved round answered, its call left out, the act.
  expect({
    rounds: continued.rounds,
    reads: continued.reads,
    leftOut: continued.leftOut,
    step: continued.step.message.content,
    userId: continued.userId,
    routingId: continued.routingId,
    sends: continued.usage.length,
  }).toEqual({
    rounds: replayed.rounds,
    reads: replayed.reads,
    leftOut: replayed.leftOut,
    step: replayed.step.message.content,
    userId: replayed.userId,
    routingId: replayed.routingId,
    sends: 1,
  });
});

// Catches: a Responses continuation that drops the saved reply's encrypted reasoning or its
// message item, reorders them, sends the call it left out, draws a fresh prompt_cache_key, or
// sends a routing header the Responses route refuses.
test("resumeReplay of a saved Responses run sends the live replay's second request: the saved output items in order, the reasoning's encrypted content kept, less the left-out call, with one output, and no routing header", async () => {
  const arm = armWith("old", { "SKILL.md": SKILL_MD, "references/x.md": X_MD });
  const mixedReply = () =>
    sseResponse(
      responseSse({
        id: "resp_mixed",
        model: "gpt-6.1-sol",
        output: [
          { type: "reasoning", encryptedContent: "enc-mixed" },
          { type: "message", text: "Reading the reference, and running the tests." },
          {
            type: "function_call",
            callId: "call_x",
            name: "read",
            arguments: { path: "skill://alpha/references/x.md" },
          },
          {
            type: "function_call",
            callId: "call_go",
            name: "bash",
            arguments: { command: "go test ./..." },
          },
        ],
      })
    );
  const done = () =>
    sseResponse(
      responseSse({
        id: "resp_after",
        model: "gpt-6.1-sol",
        output: [{ type: "message", text: "Done." }],
      })
    );
  const mixed: MixedRule = { kind: "case", act: TASK_ACT };
  // The reply as a run saves it: the output items the gateway read, whole.
  const saved = (await world(mixedReply).gateway.send(RESPONSES_BODY)).message.content;
  const live = world((n) => (n === 1 ? mixedReply() : done()));
  const replayed = await replayOnce(RESPONSES_BODY, arm, live.gateway, {
    mixed,
    routingId: randomUUID(),
  });
  const resumed = world(done);
  const continued = await resumeReplay(
    RESPONSES_BODY,
    { userId: replayed.userId, routingId: replayed.routingId, content: saved },
    arm,
    resumed.gateway,
    { mixed }
  );
  const sent = resumed.bodies as readonly unknown[] as readonly ResponsesBody[];
  expect(sent).toEqual([live.bodies[1] as unknown as ResponsesBody]);
  const first = sent[0] as ResponsesBody;
  const before: ResponsesBody = {
    ...first,
    input: first.input.slice(0, RESPONSES_BODY.input.length),
  };
  expect(before).toEqual(live.bodies[0] as unknown as ResponsesBody);
  const added = first.input.slice(RESPONSES_BODY.input.length) as Record<string, unknown>[];
  expect(added.map((item) => [item.type, item.name ?? item.call_id ?? null])).toEqual([
    ["reasoning", null],
    ["message", null],
    ["function_call", "read"],
    ["function_call_output", "call_x"],
  ]);
  expect(added[0]?.encrypted_content).toBe("enc-mixed");
  expect(bodyIdentity(first)).toBe(replayed.userId);
  expect(replayed.routingId).toBeNull();
  expect(resumed.headers[0]?.get("x-litellm-session-id")).toBeNull();
  expect(continued.leftOut).toEqual(replayed.leftOut);
  expect(continued.step.message.content).toEqual(replayed.step.message.content);
});

// Catches: a resume that sends a request for a saved reply the live replay would never have sent
// back (one that acts, one with no read it answers), so a continuation is paid that no run made.
test("resumeReplay refuses, before any send, a saved reply no replay sends back", async () => {
  const arm = armWith("old", { "SKILL.md": SKILL_MD, "references/x.md": X_MD });
  const mixed: MixedRule = { kind: "case", act: DOCKER_ACT };
  for (const content of [[X_READ, WHICH_PSQL], [ACT], [X_READ, PSQL_VERSION]]) {
    const { gateway, bodies } = scripted([[WHICH_PSQL]]);
    const refused = await refusalOf(
      resumeReplay(BODY, { userId: null, routingId: null, content }, arm, gateway, { mixed })
    );
    expect(refused).toBeInstanceOf(NotResumable);
    expect(bodies).toHaveLength(0);
  }
});

// Catches: a continuation that runs every further round the live rule allows when one is asked
// for, so the next round is paid before it is priced; or one stopped at the limit whose record
// lacks what it read and spent, or that is stopped though its reply acts.
test("resumeReplay with furtherRounds 1 stops a run whose reply after that round would be continued again, with no verdict, its reads and spend kept, and ends one that acts as the live rules say", async () => {
  const arm = armWith("old", { "SKILL.md": SKILL_MD, "references/x.md": X_MD });
  const mixed: MixedRule = { kind: "case", act: DOCKER_ACT };
  const saved = { userId: null, routingId: null, content: [X_READ, DOCKER_PS] };
  const again = scripted([[X_READ, DOCKER_PS], [WHICH_PSQL]]);
  const stopped = await refusalOf(
    resumeReplay({ ...BODY, stream: false }, saved, arm, again.gateway, {
      mixed,
      furtherRounds: 1,
    })
  );
  expect(again.bodies).toHaveLength(1);
  expect(stopped).toBeInstanceOf(FurtherRoundLimit);
  const limit = stopped as FurtherRoundLimit;
  expect(limit.message.startsWith(`${FURTHER_ROUND_LIMIT}: after 1 further rounds`)).toBe(true);
  expect({
    rounds: limit.replayed.rounds,
    reads: limit.replayed.reads.length,
    sends: limit.replayed.usage.length,
    step: limit.replayed.step.message.content,
  }).toEqual({ rounds: 1, reads: 1, sends: 1, step: [X_READ, X_READ, DOCKER_PS] });
  const acts = scripted([[WHICH_PSQL]]);
  const judged = await resumeReplay({ ...BODY, stream: false }, saved, arm, acts.gateway, {
    mixed,
    furtherRounds: 1,
  });
  expect(judged.step.message.content).toEqual([X_READ, WHICH_PSQL]);
  // Without a limit the live rule continues it past that reply.
  const live = scripted([[X_READ, DOCKER_PS], [WHICH_PSQL]]);
  const ran = await resumeReplay({ ...BODY, stream: false }, saved, arm, live.gateway, { mixed });
  expect({ sends: live.bodies.length, rounds: ran.rounds }).toEqual({ sends: 2, rounds: 2 });
});

test.each([
  ["ls -la references", true],
  ["cat SKILL.md | head -20", true],
  ["git status && git log --oneline -5", true],
  ["git -C /work/repo diff HEAD~1 -- src", true],
  ["git branch -a", true],
  ["git branch feature", false],
  ["docker ps --format '{{.Names}}' && docker images", true],
  ["kubectl -n builds get pods", true],
  ["rg -n 'foo|bar' src 2>/dev/null", true],
  ["grep -rn x . >/dev/null 2>&1", true],
  ["sed -n '1,40p' README.md", true],
  ["sed -i 's/a/b/' README.md", false],
  ["find . -name '*.md'", true],
  ["find . -name '*.tmp' -delete", false],
  ["cd /work/repo && FOO=1 ls", true],
  ["echo 'a; rm -rf b'", true],
  ["ls > files.txt", false],
  ["cat $(ls)", false],
  ["ls; rm -rf build", false],
  ["ls & make", false],
  ["go test ./...", false],
  ["jj squash --from @ --into @- -m 'Squash the stack'", false],
  ["docker exec sessionspg-pg16 which psql", false],
  // The additions of the runner's architect's ruling of 2026-11-26T10:10:22Z, each with the
  // observed commands that only read, and each flag or form that takes a command off the list.
  // Catches: a `gh api` GET, which only reads GitHub, left undecided.
  ["gh api repos/acme/widgets/issues/comments/6979847986 --jq .body", true],
  // Catches: gh api's flags with no value, or a quoted endpoint, read as a POST.
  [
    "gh api --paginate --slurp 'repos/acme/widgets/actions/workflows/ci.yml/runs?per_page=100' --jq '[.[]]'",
    true,
  ],
  // Catches: an explicit GET in lower case read as another method.
  ["gh api -X get repos/acme/widgets/pulls --jq '.[].number'", true],
  // Catches: a `gh api` POST taken for a read.
  ["gh api -X POST repos/acme/widgets/issues", false],
  // Catches: a `--method` with its value in the next word taken for a read.
  ["gh api --method PATCH repos/acme/widgets/issues/1", false],
  // Catches: a method joined to `-X` missed.
  ["gh api -XDELETE repos/acme/widgets/git/refs/heads/x", false],
  // Catches: a method joined to `--method` by `=` missed.
  ["gh api --method=PUT repos/acme/widgets/pulls/1/merge", false],
  // Catches: a `-f` field, which makes gh api a POST, missed.
  ["gh api repos/acme/widgets/issues -f title=x", false],
  // Catches: a `-F` field, which makes gh api a POST, missed.
  ["gh api repos/acme/widgets/issues -F title=x", false],
  // Catches: a `--field` field, which makes gh api a POST, missed.
  ["gh api repos/acme/widgets/issues --field title=x", false],
  // Catches: a `--raw-field` field, which makes gh api a POST, missed.
  ["gh api repos/acme/widgets/issues --raw-field body=x", false],
  // Catches: an `--input` body, which makes gh api a POST, missed.
  ["gh api repos/acme/widgets/issues --input body.json", false],
  // Catches: a field joined to `-f` missed.
  ["gh api repos/acme/widgets/issues -ftitle=x", false],
  // Catches: a field joined to `-F` missed.
  ["gh api repos/acme/widgets/issues -Ftitle=x", false],
  // Catches: a field joined to `--field` by `=` missed.
  ["gh api repos/acme/widgets/issues --field=title=x", false],
  // Catches: a field joined to `--raw-field` by `=` missed.
  ["gh api repos/acme/widgets/issues --raw-field=body=x", false],
  // Catches: a body file joined to `--input` by `=` missed.
  ["gh api repos/acme/widgets/issues --input=body.json", false],
  // Catches: a `-X POST` inside a cluster of short flags missed.
  ["gh api -iXPOST repos/acme/widgets/issues", false],
  // Catches: a GraphQL call, which gh always sends as a POST, taken for a read.
  ["gh api graphql --jq .data", false],
  // Catches: a leading slash on graphql slipping past the endpoint check.
  ["gh api /graphql --jq .data", false],
  // Catches: a field beside an explicit GET taken for a read: the ruling refuses the flag.
  ["gh api -X GET -f q=x search/issues", false],
  // Catches: `gh pr view`, which prints a pull request, left undecided.
  ["gh pr view 1804 -R acme/widgets --json headRefName,headRefOid,state", true],
  // Catches: `gh pr list`, which prints a list of pull requests, left undecided.
  [
    "gh pr list --repo acme/widgets --head legion/ACME-876 --state all --json number,state,url",
    true,
  ],
  // Catches: `gh run view`, which prints a run, left undecided.
  ["gh run view 41692557611 --repo acme/widgets --json headSha,conclusion,jobs", true],
  // Catches: `gh run view` of a job's log left undecided.
  ["gh run view 41692557715 --repo acme/widgets --job 126114596559 --log", true],
  // Catches: `gh stack view`, which prints a stack, left undecided.
  ["gh stack view 2>&1 | cat", true],
  // Catches: `gh pr merge`, which merges a pull request, taken for a read.
  ["gh pr merge 1804 -R acme/widgets --squash", false],
  // Catches: `gh run rerun`, which starts a run, taken for a read.
  ["gh run rerun 41692557611 --repo acme/widgets", false],
  // Catches: `gh stack push`, which pushes branches, taken for a read.
  ["gh stack push", false],
  // Catches: `--help` of a subcommand, whose program is otherwise on the list, left undecided.
  ["git status --short --branch && git log --oneline -5 && gh stack --help 2>&1 | head -40", true],
  // Catches: `--help` of a program with no subcommand left undecided.
  ["jj --help", true],
  // Catches: `--help` of a dispatch subcommand left undecided.
  ["dispatch doc-read --help", true],
  // Catches: `--help` of a program that is not otherwise on the list left undecided.
  ["make --help", true],
  // Catches: a last word `--help` behind a flag and its text, which bash runs, taken for a read.
  ["bash -c 'rm -rf x' --help", false],
  // Catches: `-h` taken for `--help`.
  ["rm -rf x -h", false],
  // Catches: `git worktree list`, which lists the worktrees, left undecided.
  ["git status -sb && git branch --show-current && git worktree list", true],
  // Catches: `git worktree add`, which makes a worktree, taken for a read.
  ["git worktree add ../widgets-876 legion/ACME-876", false],
  // Catches: `git fetch`, which writes the remote-tracking refs, taken for a read.
  ["git fetch origin", false],
  // Catches: `dispatch doc-read` of an issue's document left undecided.
  ["dispatch doc-read --issue ACME-147", true],
  // Catches: `dispatch doc-read` of a project's document left undecided.
  ["dispatch doc-read --project ACME --artifact goals-md", true],
  // Catches: `dispatch read`, which prints an issue or a comment, left undecided.
  ["dispatch read --ref dispatch://ACME-30/comment/7c3676be-23bf-4602-b956-907e96e14486", true],
  // Catches: `dispatch comment`, which writes a comment, taken for a read.
  ["dispatch comment --issue ACME-147 --body 'Looks right.'", false],
  // Catches: `env` with no argument, which prints the environment, left undecided.
  ["env | grep -E '^(LEGION_|DISPATCH_)'", true],
  // Catches: `env` with an assignment, which runs the command after it, taken for a read.
  ["env NAME=value make", false],
  // Catches: `env -i`, which runs the command after it, taken for a read.
  ["env -i bash", false],
  // Catches: `ps`, which lists processes, left undecided.
  ["ps -o pid,ppid,stat,args -p $$", true],
  // Catches: `jj diff` behind jj's valued `-R` left undecided.
  ["jj -R . diff -r @ --stat", true],
  // Catches: `jj status` left undecided.
  ["jj -R . status", true],
  // Catches: `jj log` behind a global flag with no value left undecided.
  ["jj --ignore-working-copy log -r @ --no-graph -T 'commit_id.short()'", true],
  // Catches: `jj diff --tool`, which runs an external program, taken for a read.
  ["jj diff --tool meld", false],
  // Catches: a tool joined to `--tool` by `=` missed.
  ["jj diff --tool=meld", false],
  // Catches: `jj log --tool`, which runs an external program for each diff, taken for a read.
  ["jj log -p --tool meld", false],
  // Catches: `jj new`, which makes a change, taken for a read.
  ["jj new", false],
  // Catches: the alias `jj st` taken for `jj status`.
  ["jj st", false],
])("the load rule: the bash command %p only reads or looks: %p", (command, want) => {
  expect(readOnlyCommand(command)).toBe(want);
});

test("the load rule: a call that only reads or looks is preparation, one that changes state or starts the task is an action, and a bash command off the list, or a command or path not known, is undecided", () => {
  const parts = [
    ["read", { path: "/work/src/storage.ts" }],
    ["grep", { pattern: "logger", path: "src" }],
    ["glob", { path: "src/**/*.ts" }],
    ["write", { path: "xd://lsp", content: "{}" }],
    ["bash", { command: "git status" }],
    ["write", { path: "notes.md", content: "# Notes" }],
    ["edit", { path: "src/storage.ts" }],
    ["task", { tasks: [] }],
    ["write", { path: "xd://envoy_send", content: "{}" }],
    ["bash", { command: "go test ./..." }],
    ["bash", {}],
    ["write", {}],
  ].map(([tool, input]) => callPart(LOAD_RULE, String(tool), input));
  expect(parts).toEqual([
    ...Array(5).fill("preparation"),
    ...Array(4).fill("act"),
    ...Array(3).fill("undecided"),
  ]);
});

// The ruling of 2026-11-26T10:10:22Z widens the read-only list through `onlyLooks`, so a
// behaviour case's rule and a load test's take it alike.
test("the read-only list's additions: a bash call of one is preparation by the load rule and by a case's rule whose act makes no bash call that only looks, and the same call with an off-list flag is undecided", () => {
  // Catches: a load test's `gh api` GET left undecided, or the same call as a PATCH continued
  // past as preparation.
  const get = "gh api repos/acme/widgets/pulls/1804 --jq .head.sha";
  expect(callPart(LOAD_RULE, "bash", { command: get })).toBe("preparation");
  const patch = "gh api -X PATCH repos/acme/widgets/pulls/1804 --jq .head.sha";
  expect(callPart(LOAD_RULE, "bash", { command: patch })).toBe("undecided");
  // Catches: a case's `jj diff` left undecided, or the same call that runs an external tool
  // continued past as preparation.
  const docker: MixedRule = { kind: "case", act: DOCKER_ACT };
  expect(DOCKER_ACT.calls.some((call) => call.tool === "bash" && call.looks)).toBe(false);
  expect(callPart(docker, "bash", { command: "jj -R . diff -r @ --stat" })).toBe("preparation");
  expect(callPart(docker, "bash", { command: "jj -R . diff -r @ --tool meld" })).toBe("undecided");
});
