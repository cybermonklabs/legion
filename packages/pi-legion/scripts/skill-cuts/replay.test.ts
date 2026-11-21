import { expect, test } from "bun:test";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import type { Arm } from "./arms";
import {
  bodyIdentity,
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
import {
  MAX_BREAKPOINTS,
  MAX_SKILL_ROUNDS,
  replayInterleaved,
  replayOnce,
  sendOrder,
  withCacheBreakpoint,
} from "./replay";

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

/** The fixture reply `stem` as the gateway sent it. */
function fixtureReply(stem: string): Response {
  const kept = JSON.parse(readFileSync(path.join(TESTDATA, `${stem}.headers.json`), "utf8"));
  return new Response(readFileSync(path.join(TESTDATA, `${stem}.sse`), "utf8"), {
    status: kept.status,
    headers: kept.headers,
  });
}

/** F5's reply `shape`, a Messages API stream. */
function f5(shape: string): Response {
  return fixtureReply(`F5-${shape}`);
}

/** F9's reply `shape`, a Responses API stream. */
function f9(shape: string): Response {
  return fixtureReply(`F9-${shape}`);
}

/** The output items of F9's reply `shape`: those its `response.completed` event carries. */
function f9Output(shape: string): Record<string, unknown>[] {
  const events = readFileSync(path.join(TESTDATA, `F9-${shape}.sse`), "utf8")
    .split("\n")
    .filter((line) => line.startsWith("data: {"))
    .map((line) => JSON.parse(line.slice("data: ".length)));
  return events.find((event) => event.type === "response.completed").response.output;
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
  expect(replayed.step.message.stop_reason).toBe("end_turn");
  expect(replayed.step.message.content.map((block) => block.type)).toEqual(["text"]);
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

test("answers at most three rounds of skill reads; the reply after the last is the step", async () => {
  const arm = armWith("old", { "SKILL.md": SKILL_MD, "references/x.md": X_MD });
  const { gateway, bodies } = world(() => f5("skill-read"));
  const replayed = await replayOnce(BODY, arm, gateway);
  expect(MAX_SKILL_ROUNDS).toBe(3);
  expect(bodies).toHaveLength(4);
  expect(bodies.map((body) => body.messages.length)).toEqual([3, 5, 7, 9]);
  // Every round keeps the one breakpoint on the body's own last block: the prefix all runs share.
  for (const sent of bodies) {
    expect(breakpointPaths(sent).sort()).toEqual(["messages.2.content.0", "system.0", "tools.10"]);
  }
  expect(replayed.rounds).toBe(3);
  expect(replayed.step.message.stop_reason).toBe("tool_use");
  expect(replayed.usage).toHaveLength(4);

  const once = world(() => f5("skill-read"));
  expect((await replayOnce(BODY, arm, once.gateway, { maxSkillRounds: 1 })).rounds).toBe(1);
  expect(once.bodies).toHaveLength(2);
});

test("sends the captured anthropic-* headers with the body on every round", async () => {
  const arm = armWith("old", { "SKILL.md": SKILL_MD, "references/x.md": X_MD });
  expect(CAPTURED_HEADERS["anthropic-beta"]).toContain("context-management-2025-06-27");
  const { gateway, headers } = world(() => f5("skill-read"));
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
// runner draws a fresh id for every run, so it never sends one id in two runs.
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
  for (const name of ["old", "new", "minus"] as const) {
    const passes = runs.filter(
      (run) =>
        run.arm === name &&
        run.outcome.status === "fulfilled" &&
        readsReference(run.outcome.value.step)
    ).length;
    // No arm's count is its fixed id's 30 or 0: the id is no factor of the arm's batch.
    expect(passes).toBeGreaterThan(0);
    expect(passes).toBeLessThan(30);
  }
});

// Catches: a body made again for each run, or a run whose bytes differ from replayOnce's in more
// than its own session id.
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
  const [read] = f9Output("skill-read");
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
  expect(replayed.step.message.content.map((block) => block.type)).toEqual([
    "reasoning",
    "message",
  ]);
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
  const { gateway, bodies } = world(() => f9("skill-read"));
  const replayed = await replayOnce(RESPONSES_BODY, arm, gateway);
  const sent = bodies as readonly unknown[] as readonly ResponsesBody[];
  expect(sent.map((body) => body.input.length)).toEqual([3, 5, 7, 9]);
  expect(replayed.rounds).toBe(MAX_SKILL_ROUNDS);
  expect(sent[1]?.input.at(-1)).toEqual({
    type: "function_call_output",
    call_id: f9Output("skill-read")[0]?.call_id,
    output: `File not found: ${INSTALLED}/references/x.md`,
  });
});
