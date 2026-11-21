import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { existsSync, mkdirSync, readdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import path from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { z } from "zod";
import { skillHeaderPath } from "../skill-scenarios/transcript";
import {
  armPromptBlocks,
  type CaptureRecord,
  renamedListingBlocks,
  renderReminder,
  rewriteBody,
  sessionFacts,
  skillListing,
} from "./capture";
import { pathHash } from "./cases";
import { type CliIo, main } from "./cli";
import { Stopped } from "./common";
import { localSource } from "./context";
import {
  ContentBlock,
  createGateway,
  ReplyMessage,
  responseMessage,
  streamedMessage,
  streamedResponse,
} from "./gateway";
import {
  claudeHarnessRules,
  claudeListing,
  firstMessageLoad,
  type Host,
  LOAD_MODEL,
  LoadRun,
  loadCapturePaths,
  loadRunFile,
  renamedSkill,
  runLoad,
  skillName,
  skillOutcome,
  type TestedSkill,
  taskOutcome,
} from "./load";
import {
  messageSse,
  type RecordedRequest,
  type Recorder,
  type ReplyBlock,
  type ResponseItem,
  responseSse,
  sseResponse,
  startRecorder,
} from "./recorder";
import { sendOrder } from "./replay";
import type { TaskLabel } from "./score";

// The load test (load.ts). Its detection rows read the request bodies of the pinned
// hosts and the gateway's replies to them (testdata/CAPTURE.md): F2, the body Oh My Pi 18.18.6
// sends for a replayed history whose assistant message reads skill://alpha (`omp -p --mode json
// --no-session --hook <replay hook>` against the recorder); F5, the gateway's replies to bodies
// made from F2 (`bun send.ts <body.json>`); and F3, the bodies Claude Code 2.1.311 sent for a task
// that calls `Skill` on acme-tools:alpha and `Read` on beta's SKILL.md
// (`CLAUDE_CONFIG_DIR=<scratch>
// ANTHROPIC_BASE_URL=<recording proxy> claude -p --output-format stream-json --verbose
// "<prompt>"`). The subagent row's profile carries the system prompt and tools F1's `task`
// subagent recorded in its `session_init` line (F1, `HOME=<scratch> omp -p --session-dir
// <scratch>/sessions --model anthropic/claude-sonnet-5-5 "<prompt>"`). The command rows run
// `skill-cuts load` in this process against a stand-in gateway on 127.0.0.1 built on recorder.ts,
// with captures saved where the command reads them back, so no host runs and no row reaches a
// network. The rows that run a real host are skipped without LEGION_TEST_OMP (refused on GitHub
// Actions without it, where the pi-legion job installs the pin, as capture.test.ts does), and
// without LEGION_TEST_CLAUDE naming the Claude Code binary (which that job does not install).
// Every skill, repository and store here is made up.

const TESTDATA = path.join(import.meta.dir, "testdata");
const omp = process.env.LEGION_TEST_OMP;
const claude = process.env.LEGION_TEST_CLAUDE;
if (omp === undefined && process.env.GITHUB_ACTIONS === "true") {
  throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
}
/** A row's own timeout when it runs a real host: a few of its runs, about ten seconds each in a
 *  slow pod. */
const HOST_ROW_TIMEOUT_MS = 600_000;

/** A request body, as far as these rows read one. */
const Body = z.looseObject({
  messages: z.array(
    z.looseObject({ role: z.string(), content: z.union([z.string(), z.array(ContentBlock)]) })
  ),
});
type Body = z.infer<typeof Body>;

/** The request body in testdata file `name`. */
function fixture(name: string): Body {
  return Body.parse(JSON.parse(readFileSync(path.join(TESTDATA, name), "utf8")));
}
/** The content blocks of message `index` of `body`. */
function blocksOf(body: Body, index: number): ContentBlock[] {
  const content = body.messages[index]?.content;
  if (!Array.isArray(content)) throw new Error(`message ${index} holds no content blocks`);
  return content;
}
/** The content of the F5 reply `name`, read whole as gateway.ts reads a streamed reply. */
function f5(name: string): ContentBlock[] {
  const sse = readFileSync(path.join(TESTDATA, `F5-${name}.sse`), "utf8");
  return ReplyMessage.parse(streamedMessage(sse)).content;
}

/** The content of the F9 reply `name`, a Responses API reply of openai/gpt-6.1-sol, read whole as
 *  gateway.ts reads one: its output items. */
function f9(name: string): ContentBlock[] {
  const sse = readFileSync(path.join(TESTDATA, `F9-${name}.sse`), "utf8");
  return responseMessage(streamedResponse(sse)).content;
}

/** A Responses API `function_call` item of tool `name` with `input`, as F9's are written. */
function functionCall(name: string, input: Record<string, unknown>): ContentBlock {
  return {
    type: "function_call",
    call_id: `call_${name}_1`,
    name,
    arguments: JSON.stringify(input),
  };
}

const F2 = fixture("F2-request.json");
const F3 = fixture("F3-request-last.json");
/** F2's assistant message: one `read` of skill://alpha. */
const F2_READ = blocksOf(F2, 1);
/** The path of alpha's SKILL.md, as F2's read result names it in its `[Skill file: …]` line. */
const ALPHA_PATH =
  skillHeaderPath(
    z
      .looseObject({ content: z.array(z.looseObject({ text: z.string() })) })
      .parse(blocksOf(F2, 2)[0]).content[0]?.text ?? ""
  ) ?? "";
const F5_TOOLS = f5("skill-read-and-other");
/** F5's `bash` call, made in a message beside reads of skill://beta and of alpha's reference. */
const BASH = F5_TOOLS.filter((block) => block.name === "bash");
/** F3's assistant messages: a `Skill` call on acme-tools:alpha, and a `Read` of beta's SKILL.md. */
const F3_SKILL = blocksOf(F3, 2);
const F3_READ = blocksOf(F3, 5);
const F3_READ_PATH = z.object({ file_path: z.string() }).parse(F3_READ[0]?.input).file_path;
const F3_CALL = F3_SKILL[0] ?? { type: "tool_use" };
const CLAUDE_BASH: ContentBlock[] = [
  { type: "tool_use", id: "toolu_bash_1", name: "Bash", input: { command: "ls" } },
];

const alpha: TestedSkill = { name: "alpha" };
const beta: TestedSkill = { name: "beta" };
const pluginAlpha: TestedSkill = { name: "alpha", plugin: "acme-tools" };
const pluginBeta: TestedSkill = { name: "beta", plugin: "acme-tools" };
const none = (toolCalls: string[]) => ({ loaded: false, loadedBy: null, toolCalls });

describe("a run loads the skill when its first assistant message reads it", () => {
  const rows: [string, Host, readonly ContentBlock[], TestedSkill, unknown][] = [
    [
      "Oh My Pi: a read of skill://alpha loads alpha (F2)",
      "omp",
      F2_READ,
      alpha,
      { loaded: true, loadedBy: "read skill://alpha", toolCalls: ["read"] },
    ],
    [
      "Oh My Pi: a read of alpha's SKILL.md by its path loads alpha (the path of F2's read result)",
      "omp",
      [{ type: "tool_use", id: "toolu_path_1", name: "read", input: { path: ALPHA_PATH } }],
      alpha,
      { loaded: true, loadedBy: `read ${ALPHA_PATH}`, toolCalls: ["read"] },
    ],
    [
      "Oh My Pi: a read of skill://alpha with a read selector loads alpha",
      "omp",
      [
        {
          type: "tool_use",
          id: "toolu_sel_1",
          name: "read",
          input: { path: "skill://alpha:1-40" },
        },
      ],
      alpha,
      { loaded: true, loadedBy: "read skill://alpha:1-40", toolCalls: ["read"] },
    ],
    [
      "Oh My Pi: a read of another skill does not load beta (F2)",
      "omp",
      F2_READ,
      beta,
      none(["read"]),
    ],
    [
      "Oh My Pi: a read of alpha's reference does not load alpha (F5 skill-read)",
      "omp",
      f5("skill-read"),
      alpha,
      none(["read"]),
    ],
    [
      "Oh My Pi: reads of another skill and of alpha's reference beside a bash call do not load alpha (F5)",
      "omp",
      F5_TOOLS,
      alpha,
      none(["read", "bash", "read"]),
    ],
    [
      "Oh My Pi: the same message loads beta, by its read of skill://beta (F5)",
      "omp",
      F5_TOOLS,
      beta,
      { loaded: true, loadedBy: "read skill://beta", toolCalls: ["read", "bash", "read"] },
    ],
    [
      "Oh My Pi: another tool first, and no read, does not load alpha (F5's bash call)",
      "omp",
      BASH,
      alpha,
      none(["bash"]),
    ],
    [
      "Oh My Pi: another tool first, then a read of skill://alpha in the same message, loads alpha",
      "omp",
      [...BASH, ...F2_READ],
      alpha,
      { loaded: true, loadedBy: "read skill://alpha", toolCalls: ["bash", "read"] },
    ],
    ["Oh My Pi: text only does not load alpha (F5 text)", "omp", f5("text"), alpha, none([])],
    [
      "Claude Code: a Skill call naming acme-tools:alpha loads alpha of acme-tools (F3)",
      "claude",
      F3_SKILL,
      pluginAlpha,
      { loaded: true, loadedBy: "Skill acme-tools:alpha", toolCalls: ["Skill"] },
    ],
    [
      "Claude Code: a Skill call naming plain alpha loads alpha",
      "claude",
      [{ ...F3_CALL, input: { skill: "alpha" } }],
      pluginAlpha,
      { loaded: true, loadedBy: "Skill alpha", toolCalls: ["Skill"] },
    ],
    [
      "Claude Code: a Skill call naming another plugin's alpha does not load alpha of acme-tools",
      "claude",
      [{ ...F3_CALL, input: { skill: "other-tools:alpha" } }],
      pluginAlpha,
      none(["Skill"]),
    ],
    [
      "Claude Code: a Skill call on alpha does not load beta (F3)",
      "claude",
      F3_SKILL,
      pluginBeta,
      none(["Skill"]),
    ],
    [
      "Claude Code: a Read of beta's SKILL.md loads beta (F3)",
      "claude",
      F3_READ,
      pluginBeta,
      { loaded: true, loadedBy: `Read ${F3_READ_PATH}`, toolCalls: ["Read"] },
    ],
    [
      "Claude Code: a Read of beta's SKILL.md does not load alpha (F3)",
      "claude",
      F3_READ,
      pluginAlpha,
      none(["Read"]),
    ],
    [
      "Claude Code: another tool first, and no Skill call or Read of it, does not load alpha",
      "claude",
      CLAUDE_BASH,
      pluginAlpha,
      none(["Bash"]),
    ],
    [
      "Claude Code: another tool first, then a Skill call on alpha in the same message, loads alpha",
      "claude",
      [...CLAUDE_BASH, ...F3_SKILL],
      pluginAlpha,
      { loaded: true, loadedBy: "Skill acme-tools:alpha", toolCalls: ["Bash", "Skill"] },
    ],
    [
      "Claude Code: Oh My Pi's read of skill://alpha is no Claude Code load (F2)",
      "claude",
      F2_READ,
      pluginAlpha,
      none(["read"]),
    ],
    [
      "Oh My Pi on a Responses reply: a function call reading skill://alpha loads alpha",
      "omp",
      [functionCall("read", { i: "Reading the alpha skill", path: "skill://alpha" })],
      alpha,
      { loaded: true, loadedBy: "read skill://alpha", toolCalls: ["read"] },
    ],
    [
      "Oh My Pi on a Responses reply: a function call reading alpha's SKILL.md by its path loads alpha",
      "omp",
      [functionCall("read", { path: ALPHA_PATH })],
      alpha,
      { loaded: true, loadedBy: `read ${ALPHA_PATH}`, toolCalls: ["read"] },
    ],
    [
      "Oh My Pi on a Responses reply: a read of alpha's reference does not load alpha (F9 skill-read)",
      "omp",
      f9("skill-read"),
      alpha,
      none(["read"]),
    ],
    [
      "Oh My Pi on a Responses reply: a read of skill://beta beside a bash call loads beta and not alpha (F9)",
      "omp",
      f9("skill-read-and-other"),
      beta,
      { loaded: true, loadedBy: "read skill://beta", toolCalls: ["read", "bash"] },
    ],
    [
      "Oh My Pi on a Responses reply: the same message does not load alpha (F9)",
      "omp",
      f9("skill-read-and-other"),
      alpha,
      none(["read", "bash"]),
    ],
    [
      "Oh My Pi on a Responses reply: reasoning and text only do not load alpha (F9 text)",
      "omp",
      f9("text"),
      alpha,
      none([]),
    ],
  ];
  test.each(rows)("%s", (_name, host, content, skill, want) => {
    expect(ALPHA_PATH).toEndWith("/alpha/SKILL.md");
    expect(firstMessageLoad(host, content, skill)).toEqual(want as never);
  });

  test("Claude Code's listing gives each plugin skill as <plugin>:<name> with its description (F3)", () => {
    const listing = claudeListing(
      JSON.parse(readFileSync(path.join(TESTDATA, "F3-request-first.json"), "utf8"))
    );
    expect(listing.get("acme-tools:alpha")).toBe(
      "Use when a task asks for the alpha checklist of the made-up fixture project; it lists the three alpha steps."
    );
    expect(listing.get("acme-tools:beta")).toStartWith(
      "Use when a task asks for the beta greeting"
    );
    expect(listing.has("alpha")).toBe(false);
  });
});

/** The runs of one task, each loading the skill or not, as its run file keeps them. */
function runsOf(label: TaskLabel, loads: readonly boolean[]): LoadRun[] {
  return loads.map((loaded, index) =>
    LoadRun.parse({
      task: "t",
      run: index + 1,
      label,
      source: "written",
      host: "omp",
      name: "alpha",
      loaded,
      loadedBy: loaded ? "read skill://alpha" : null,
      toolCalls: loaded ? ["read"] : [],
      stopReason: loaded ? "tool_use" : "end_turn",
      requestId: null,
      usage: {
        model: "claude-sonnet-5-5",
        input: 1,
        output: 1,
        cacheRead: 0,
        cacheWrite: 0,
        usd: 0,
      },
      listed: "Use for the alpha checklist.",
      profile: "/profiles/alpha.json",
      profileDigest: "0".repeat(64),
      userId: JSON.stringify({ session_id: `00000000-0000-4000-8000-00000000000${index}` }),
      sendSeed: "made-up-seed",
      sendPlace: index,
    })
  );
}

describe("each task's loads feed score.ts's load rule", () => {
  const rows: [string, TaskLabel, boolean[], boolean | null][] = [
    ["a need task that loads in 1 of 3 runs is a fault", "need", [false, true, false], true],
    ["a need task that loads in 2 of 3 runs is no fault", "need", [true, false, true], false],
    [
      "a near-miss task that loads in 2 of 3 runs is a fault",
      "near-miss",
      [true, true, false],
      true,
    ],
    [
      "a near-miss task that loads in 1 of 3 runs is no fault",
      "near-miss",
      [false, false, true],
      false,
    ],
    ["a task with 2 of its 3 runs made has no verdict yet", "need", [true, true], null],
  ];
  test.each(rows)("%s", (_name, label, loads, fault) => {
    const outcome = taskOutcome(label, runsOf(label, loads), 3);
    expect(outcome.fault).toBe(fault);
    expect(outcome.loads).toBe(loads.filter(Boolean).length);
    expect(outcome.complete).toBe(loads.length === 3);
  });

  test("the skill's verdict is a description fault when one task is a fault, none when no task is, and waits for every task's runs", () => {
    const task = (id: string, label: TaskLabel, loads: boolean[], wanted = 3) => ({
      id,
      label,
      outcome: taskOutcome(label, runsOf(label, loads), wanted),
    });
    const good = [
      task("n1", "need", [true, true, false]),
      task("m1", "near-miss", [true, false, false]),
    ];
    expect(skillOutcome(good, 3)).toEqual({
      descriptionFault: false,
      reason: "no description fault in 2 tasks",
    });
    expect(skillOutcome([...good, task("n2", "need", [false, false, true])], 3)).toEqual({
      descriptionFault: true,
      reason: "a description fault: tasks n2",
    });
    expect(skillOutcome([...good, task("n3", "need", [true])], 3)).toEqual({
      descriptionFault: null,
      reason: "no verdict yet: tasks n3 lack runs",
    });
    expect(skillOutcome([task("n1", "need", [true], 1)], 1)).toEqual({
      descriptionFault: null,
      reason: "no verdict: the load test's rule takes 3 runs a task, and this test runs 1",
    });
  });
});

const scratch = temporaryDirectory("skill-cuts-load", { real: true });

/** Writes `files` (path to text) under `root`. */
function writeTree(root: string, files: Record<string, string>): void {
  for (const [file, text] of Object.entries(files)) {
    mkdirSync(path.dirname(path.join(root, file)), { recursive: true });
    writeFileSync(path.join(root, file), text);
  }
}

/** Each file under `dir`, by its path inside it, with its text. */
function treeOf(dir: string): Record<string, string> {
  return Object.fromEntries(
    readdirSync(dir, { recursive: true, encoding: "utf8" })
      .filter((file) => statSync(path.join(dir, file)).isFile())
      .sort()
      .map((file) => [file, readFileSync(path.join(dir, file), "utf8")])
  );
}

const ALPHA_SKILL = [
  "---",
  "name: alpha",
  "description: Use when a task asks for the alpha checklist of the made-up project.",
  "---",
  "",
  "# Alpha",
  "",
  "Read skill://alpha/references/x.md before the first alpha step.",
  "",
].join("\n");
const ALPHA_X = "The alpha steps: one, two, three.\n";
const BETA_SKILL =
  "---\nname: beta\ndescription: Use when a task asks for the beta greeting of the made-up project.\n---\n\n# Beta\n\nGreet with one line.\n";
const GAMMA_SKILL =
  "---\nname: gamma\ndescription: Use when a task asks for the gamma report of the made-up project.\n---\n\n# Gamma\n\nReport in one line.\n";
/** The SHA-256 of `file`'s bytes, in hex. */
const sha256 = (file: string) => createHash("sha256").update(readFileSync(file)).digest("hex");

describe("--name tests the skill under another name", () => {
  test("the scratch copy changes only the frontmatter name and the directory's name", () => {
    const dir = path.join(scratch, "rename", "alpha");
    writeTree(dir, { "SKILL.md": ALPHA_SKILL, "references/x.md": ALPHA_X });
    const copy = renamedSkill(dir, "omega", path.join(scratch, "rename", "copies"));
    expect(path.basename(copy)).toBe("omega");
    expect(treeOf(copy)).toEqual({
      "SKILL.md": ALPHA_SKILL.replace("name: alpha", "name: omega"),
      "references/x.md": ALPHA_X,
    });
    // The skill's own text still names alpha: only the frontmatter's name line changed.
    expect(readFileSync(path.join(copy, "SKILL.md"), "utf8")).toContain(
      "skill://alpha/references/x.md"
    );
    expect(treeOf(dir)).toEqual({ "SKILL.md": ALPHA_SKILL, "references/x.md": ALPHA_X });
  });

  test("a frontmatter with no name gets one, and every other line stays", () => {
    const dir = path.join(scratch, "rename", "unnamed");
    const text = ALPHA_SKILL.replace("name: alpha\n", "");
    writeTree(dir, { "SKILL.md": text });
    const copy = renamedSkill(dir, "omega", path.join(scratch, "rename", "copies"));
    expect(readFileSync(path.join(copy, "SKILL.md"), "utf8")).toBe(
      text.replace("---\n", "---\nname: omega\n")
    );
  });

  test("a recorded prompt under another name changes only the name of the skill's listing line (F1's subagent)", () => {
    const renamed = renamedListingBlocks(SUBAGENT.systemPrompt, "beta", "beta-renamed");
    const line = "- beta: Use when asked for the beta greeting of the fixture project.";
    expect(SUBAGENT.systemPrompt.join("\n")).toContain(`\n${line}\n`);
    expect(renamed).toEqual(
      SUBAGENT.systemPrompt.map((block) =>
        block.replace(line, line.replace("beta", "beta-renamed"))
      )
    );
    expect(renamedListingBlocks(SUBAGENT.systemPrompt, "delta", "omega")).toEqual(
      SUBAGENT.systemPrompt
    );
  });
});

// The command, against the stand-in gateway.

/** What one command wrote and returned. */
interface Ran {
  readonly code: number;
  readonly stdout: string;
  readonly stderr: string;
}

/** Runs `skill-cuts <args>` in this process. */
async function cli(args: readonly string[]): Promise<Ran> {
  let stdout = "";
  let stderr = "";
  const io: CliIo = {
    out: (text) => {
      stdout += text;
    },
    err: (line) => {
      stderr += `${line}\n`;
    },
    peakRssBytes: () => 0,
  };
  const code = await main(args, io);
  return { code, stdout, stderr };
}

/** A request the stand-in takes, as far as it reads one. */
const Request = z.looseObject({
  model: z.string(),
  system: z.unknown().optional(),
  tools: z.unknown().optional(),
  messages: z.unknown(),
});
type Request = z.infer<typeof Request>;

/** How the stand-in answers a run's request. */
let answer: (body: Request) => ReplyBlock[] = () => [{ type: "text", text: "nothing" }];
let recorder: Recorder;
beforeAll(() => {
  recorder = startRecorder({
    route: (request: RecordedRequest) => {
      if (
        request.method !== "POST" ||
        (request.path !== "/v1/messages" && request.path !== "/v1/responses")
      ) {
        return undefined;
      }
      const body = Request.parse(JSON.parse(request.body));
      // Oh My Pi's hint request: no system prompt, no tools, `Skill: <name>` in its message.
      const hint = /\\nSkill: ([^\\]+)\\nDescription: /.exec(request.body)?.[1];
      const content: ReplyBlock[] =
        body.system === undefined && body.tools === undefined && hint !== undefined
          ? [{ type: "text", text: `Use for the ${hint} work, stand-in hint.` }]
          : answer(body);
      // A Responses body gets the same answer in its API's stream.
      if (request.path === "/v1/responses") {
        const output = content.flatMap((block): ResponseItem[] =>
          block.type === "text"
            ? [{ type: "message", text: block.text }]
            : block.type === "tool_use"
              ? [
                  {
                    type: "function_call",
                    callId: block.id,
                    name: block.name,
                    arguments: block.input,
                  },
                ]
              : []
        );
        return sseResponse(
          responseSse({ id: `resp_standin_${request.n}`, model: body.model, output }),
          `req_standin_${request.n}`
        );
      }
      return sseResponse(
        messageSse({
          id: `msg_standin_${request.n}`,
          model: body.model,
          content,
          stopReason: content.some((block) => block.type === "tool_use") ? "tool_use" : "end_turn",
        }),
        `req_standin_${request.n}`
      );
    },
  });
});
afterAll(async () => {
  await recorder.close();
});

/** A configuration whose gateway is the stand-in, and a key file, under `dir`. */
function setup(dir: string): { config: string; keyFile: string; key: string } {
  mkdirSync(dir, { recursive: true });
  const config = path.join(dir, "skill-cuts.json");
  const made = JSON.parse(readFileSync(path.join(TESTDATA, "F7-skill-cuts.json"), "utf8"));
  writeFileSync(config, JSON.stringify({ ...made, gatewayBaseUrl: recorder.url }));
  const key = fixtureSecret("gw");
  const keyFile = path.join(dir, "key");
  writeFileSync(keyFile, key, { mode: 0o600 });
  return { config, keyFile, key };
}

/** Every string inside `value`. */
function texts(value: unknown): string[] {
  if (typeof value === "string") return [value];
  if (Array.isArray(value)) return value.flatMap(texts);
  if (typeof value === "object" && value !== null) return Object.values(value).flatMap(texts);
  return [];
}

/** The runs of task `id`'s run file under `out`. */
function savedRuns(out: string, id: string): LoadRun[] {
  return readFileSync(loadRunFile(out, id), "utf8")
    .trim()
    .split("\n")
    .map((line) => LoadRun.parse(JSON.parse(line)));
}

const READ_ALPHA: ReplyBlock[] = [
  { type: "tool_use", id: "toolu_alpha", name: "read", input: { path: "skill://alpha" } },
];
const TEXT: ReplyBlock[] = [{ type: "text", text: "Here is the answer." }];
const COMMIT = "0123456789abcdef0123456789abcdef01234567";
const NEED = "Write the alpha checklist of the made-up project.";
const NEAR = "Write the beta greeting of the made-up project.";
const BETA_PROFILE = {
  skills: [{ repo: "acme/skills", commit: COMMIT, path: "skills/beta", name: "beta" }],
  plugins: [],
  contextFiles: [],
};
const GAMMA_PROFILE = {
  skills: [{ repo: "acme/skills", commit: COMMIT, path: "skills/gamma", name: "gamma" }],
  plugins: [],
  contextFiles: [],
};
/** The user-level context file of a devbox profile (build.ts: Claude Code's user memory of the
 *  repository that installs its dev box's units), in the made-up acme/skills. */
const USER_RULES = "# Made-up user rules\n\nEnd each answer with the word done.\n";
const USER_FILE = { repo: "acme/skills", commit: COMMIT, path: ".claude/CLAUDE.md" };
/** A `models.yml` of the gateway's Messages route, whose provider a capture points at its
 *  recorder. */
const ANTHROPIC_MODELS =
  "providers:\n  anthropic:\n    baseUrl: http://127.0.0.1:9\n    api: anthropic-messages\n    compat: {replayUnsignedThinking: false, supportsLongCacheRetention: true}\n    models:\n      - {id: claude-sonnet-5-5, name: Claude Sonnet 5.5, reasoning: true, input: [text, image], contextWindow: 1000000, maxTokens: 128000, cost: {input: 2, output: 10, cacheRead: 0.1, cacheWrite: 2.5}}\n";
/** The system prompt and tool names F1's `task` subagent recorded in its `session_init` line (its
 *  skill listing names alpha and beta). */
const SUBAGENT = z
  .looseObject({ systemPrompt: z.array(z.string()).min(1), tools: z.array(z.string()).min(1) })
  .parse(
    readFileSync(
      path.join(
        TESTDATA,
        "F1",
        "2026-11-20T01-59-41-780Z_01a1f6ae-51d4-715f-a2a6-59f275bdff80",
        "ProductiveChicken.jsonl"
      ),
      "utf8"
    )
      .split("\n")
      .filter((line) => line.trim() !== "")
      .map((line): unknown => JSON.parse(line))
      .find((entry) => z.looseObject({ type: z.literal("session_init") }).safeParse(entry).success)
  );

describe("skill-cuts load with saved captures", () => {
  const dir = path.join(scratch, "saved");
  const out = path.join(dir, "out");
  const skill = path.join(dir, "alpha");
  const tasks = path.join(dir, "tasks.jsonl");
  const profile = path.join(dir, "profile.json");
  /** need-1's own profile, which its line names relative to the tasks file's directory. */
  const profileA = path.join(dir, "profile-a.json");
  let args: string[] = [];
  let key = "";
  /** Of each task's text, the runs the stand-in answers with a read of skill://alpha. */
  const reads = new Map([
    [NEED, 2],
    [NEAR, 2],
  ]);
  const answered = new Map<string, number>();

  beforeAll(() => {
    const made = setup(dir);
    key = made.key;
    writeTree(skill, { "SKILL.md": ALPHA_SKILL, "references/x.md": ALPHA_X });
    const lines = [
      {
        id: "need-1",
        text: NEED,
        label: "need",
        source: "session",
        store: "store_a",
        pathHash: pathHash("/sessions/a.jsonl"),
        profile: "profile-a.json",
      },
      { id: "near-1", text: NEAR, label: "near-miss", source: "written" },
    ];
    writeFileSync(tasks, lines.map((line) => `${JSON.stringify(line)}\n`).join(""));
    writeFileSync(profile, JSON.stringify(BETA_PROFILE));
    writeFileSync(profileA, JSON.stringify(GAMMA_PROFILE));
    // Each task's capture, saved where the command reads it back: F2's body with the task's text
    // as its one message, and the fields of its record that the load test reads.
    for (const { id, text } of lines) {
      const files = loadCapturePaths(out, "omp", id);
      writeTree(path.dirname(files.body), {
        [path.basename(files.body)]: JSON.stringify({
          ...F2,
          messages: [{ role: "user", content: [{ type: "text", text }] }],
        }),
        [path.basename(files.record)]: JSON.stringify({
          listing: [
            { name: "alpha", hint: "Use for the alpha checklist." },
            { name: "beta", hint: "Use for the beta greeting." },
          ],
          // A header the gateway's client never adds itself, so a run carries it only when the
          // capture's headers go with it.
          requestHeaders: { "anthropic-version": "2023-06-01", "anthropic-beta": "made-up-beta-1" },
          hintCalls: [],
          // A saved capture holds the harness-string rule's record, or it is refused.
          harness: { head: null, replaced: [] },
        }),
      });
    }
    answer = (body) => {
      const text = texts(body.messages).find((each) => reads.has(each)) ?? "";
      const n = (answered.get(text) ?? 0) + 1;
      answered.set(text, n);
      return n <= (reads.get(text) ?? 0) ? READ_ALPHA : TEXT;
    };
    args = [
      "load",
      "--config",
      made.config,
      "--key-file",
      made.keyFile,
      "--skill",
      skill,
      "--tasks",
      tasks,
      "--host",
      "omp",
      "--profile",
      profile,
      "--out",
      out,
      "--omp",
      path.join(dir, "no-omp-runs-here"),
      "--scratch",
      path.join(dir, "scratch"),
      "--cache-dir",
      path.join(dir, "cache"),
    ];
  });

  test("sends each task's body 3 times, prints each task's loads and verdict and the skill's, and writes a run file per task and the summary", async () => {
    const before = recorder.requests.length;
    const ran = await cli(args);
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    expect(recorder.requests.length - before).toBe(6);
    expect(ran.stdout).toContain(
      'load: Oh My Pi lists alpha under the hint "Use for the alpha checklist."'
    );
    expect(ran.stdout).toMatch(
      /task need-1 \(need, from a session\): loaded in 2 of 3 runs \(.*\): no fault/
    );
    // Each task's line ends with the seconds of each phase; its capture is read back, and no
    // GitHub read is made.
    for (const id of ["need-1", "near-1"]) {
      expect(ran.stdout).toMatch(
        new RegExp(
          `^load: task ${id} \\([^)]*\\): loaded in [^\\n]*; seconds: capture \\d+\\.\\d \\(context 0\\.0; 0 GitHub reads, 0\\.0\\), runs \\d+\\.\\d, write \\d+\\.\\d, other \\d+\\.\\d, total \\d+\\.\\d$`,
          "m"
        )
      );
    }
    expect(ran.stdout).toMatch(
      /task near-1 \(near-miss, written\): loaded in 2 of 3 runs \(.*\): a fault: a near-miss task loads it in more than 1 of 3 runs/
    );
    expect(ran.stdout).toContain(
      "the skill alpha: a description fault: tasks near-1 (a load test has at least 5 need and 5 near-miss tasks; this one has 1 and 1)"
    );
    // Each task in its own profile's context: need-1 the one its line names, near-1 --profile's.
    expect(ran.stdout).toContain(
      `load: task need-1 run in the context devbox of the profile ${profileA} (SHA-256 ${sha256(profileA)})`
    );
    expect(ran.stdout).toContain(
      `load: task near-1 run in the context devbox of the profile ${profile} (SHA-256 ${sha256(profile)})`
    );
    for (const [id, file] of [
      ["need-1", profileA],
      ["near-1", profile],
    ] as const) {
      const runs = savedRuns(out, id);
      expect(runs.map((run) => run.run)).toEqual([1, 2, 3]);
      expect(runs.filter((run) => run.loaded).map((run) => run.loadedBy)).toEqual([
        "read skill://alpha",
        "read skill://alpha",
      ]);
      expect(runs.every((run) => run.listed === "Use for the alpha checklist.")).toBe(true);
      expect(runs.every((run) => run.usage.model === "claude-sonnet-5-5")).toBe(true);
      expect(runs.every((run) => run.profile === file && run.profileDigest === sha256(file))).toBe(
        true
      );
    }
    const summary = z
      .looseObject({
        verdict: z.unknown(),
        tested: z.unknown(),
        tasks: z.array(
          z.looseObject({
            id: z.string(),
            loads: z.number(),
            fault: z.unknown(),
            profile: z.string(),
            profileDigest: z.string(),
          })
        ),
      })
      .parse(JSON.parse(readFileSync(path.join(out, "summary.json"), "utf8")));
    expect(summary.verdict).toEqual({
      descriptionFault: true,
      reason: "a description fault: tasks near-1",
    });
    expect(
      summary.tasks.map((task) => [
        task.id,
        task.loads,
        task.fault,
        task.profile,
        task.profileDigest,
      ])
    ).toEqual([
      ["need-1", 2, false, profileA, sha256(profileA)],
      ["near-1", 2, true, profile, sha256(profile)],
    ]);
    expect(summary.tested).toEqual({ name: "alpha", renamedFrom: null, changed: "nothing" });
    // Each run went with the headers the host sent, and no output holds the key.
    const sent = recorder.requests.slice(before);
    expect(sent.map((request) => request.headers["anthropic-beta"])).toEqual(
      Array(6).fill("made-up-beta-1")
    );
    const log = readFileSync(path.join(out, "requests.jsonl"), "utf8");
    expect(
      log
        .trim()
        .split("\n")
        .map((line) => JSON.parse(line).kind)
    ).toEqual(Array(6).fill("load"));
    for (const text of [ran.stdout, log, JSON.stringify(summary)]) expect(text).not.toContain(key);
  });

  test("a command started again skips each task whose run file is complete, and makes only the runs a task lacks", async () => {
    let before = recorder.requests.length;
    const again = await cli(args);
    expect(again.code).toBe(0);
    expect(recorder.requests.length).toBe(before);
    expect(again.stdout).toContain(
      "task need-1 (need, from a session): its run file is complete, skipped"
    );
    const file = loadRunFile(out, "near-1");
    const lines = readFileSync(file, "utf8").trim().split("\n");
    writeFileSync(file, `${lines.slice(0, 2).join("\n")}\n`);
    before = recorder.requests.length;
    const resumed = await cli(args);
    expect(resumed.code).toBe(0);
    expect(recorder.requests.length - before).toBe(1);
    expect(savedRuns(out, "near-1").map((run) => run.run)).toEqual([1, 2, 3]);
  });

  test("an --out of another test's settings, a task id given twice, a profile that installs the tested name, a task with no profile and no --profile, and a task whose runs were made under a profile of other bytes are refused before any model call", async () => {
    const before = recorder.requests.length;
    const renamed = await cli([...args, "--name", "omega"]);
    expect(renamed.code).toBe(2);
    expect(renamed.stderr).toContain("holds a load test of other settings");
    const twice = path.join(dir, "twice.jsonl");
    writeFileSync(twice, `${readFileSync(tasks, "utf8")}${readFileSync(tasks, "utf8")}`);
    const dup = await cli(args.map((arg) => (arg === tasks ? twice : arg)));
    expect(dup.code).toBe(2);
    expect(dup.stderr).toContain("the task id need-1 is given twice");
    const own = path.join(dir, "own-profile.json");
    writeFileSync(
      own,
      JSON.stringify({
        ...BETA_PROFILE,
        skills: [{ repo: "acme/skills", commit: COMMIT, path: "skills/alpha", name: "alpha" }],
      })
    );
    const mine = await cli(args.map((arg) => (arg === profile ? own : arg)));
    expect(mine.code).toBe(2);
    expect(mine.stderr).toContain(
      "it installs a skill named alpha, the name the skill is tested under"
    );
    // near-1 names no profile, and no --profile is given.
    const at = args.indexOf("--profile");
    const bare = await cli([...args.slice(0, at), ...args.slice(at + 2)]);
    expect(bare.code).toBe(2);
    expect(bare.stderr).toContain(
      `task near-1 of ${tasks} names no profile, and no --profile is given for a task that names none`
    );
    // need-1's line names a profile of other bytes than the one its runs were made under.
    const moved = path.join(dir, "moved.jsonl");
    writeFileSync(
      moved,
      readFileSync(tasks, "utf8").replace('"profile-a.json"', '"profile-b.json"')
    );
    writeFileSync(path.join(dir, "profile-b.json"), JSON.stringify(GAMMA_PROFILE, null, 2));
    const other = await cli(args.map((arg) => (arg === tasks ? moved : arg)));
    expect(other.code).toBe(2);
    expect(other.stderr).toContain(
      `holds task need-1 made under a profile of other bytes (SHA-256 ${sha256(profileA)})`
    );
    expect(recorder.requests.length).toBe(before);
  });

  test("a task whose capture and runs are saved but which the summary no longer lists is refused under another text", async () => {
    const before = recorder.requests.length;
    // A run of need-1 alone: its summary then lists need-1 only.
    const alone = path.join(dir, "need-alone.jsonl");
    writeFileSync(alone, `${readFileSync(tasks, "utf8").split("\n")[0]}\n`);
    const only = await cli(args.map((arg) => (arg === tasks ? alone : arg)));
    expect({ code: only.code, stderr: only.stderr }).toEqual({ code: 0, stderr: "" });
    // near-1 under a new text: its saved capture and runs were made for the old one.
    const changed = path.join(dir, "near-changed.jsonl");
    writeFileSync(
      changed,
      `${JSON.stringify({ id: "near-1", text: `${NEAR} Twice.`, label: "near-miss", source: "written" })}\n`
    );
    const refused = await cli(args.map((arg) => (arg === tasks ? changed : arg)));
    expect(refused.code).toBe(2);
    expect(refused.stderr).toContain(
      `${out} holds a capture or runs of task near-1, which its summary does not list`
    );
    expect(recorder.requests.length).toBe(before);
  });
});

test("the skill's name is read as Oh My Pi reads its frontmatter: a value that holds ': ' does not stop the read", () => {
  const named = path.join(scratch, "colon", "folder");
  writeTree(named, {
    "SKILL.md": "---\nname: lookup\ndescription: Use when: you need a lookup\n---\n\n# Lookup\n",
  });
  expect(skillName(named)).toBe("lookup");
  const unnamed = path.join(scratch, "colon", "unnamed");
  writeTree(unnamed, { "SKILL.md": "---\ndescription: Use when: you need a lookup\n---\n" });
  expect(skillName(unnamed)).toBe("unnamed");
});

describe("skill-cuts load refuses or stops on its own rules", () => {
  test("an openai/<id> model on Oh My Pi with no --hint-model and no saved capture is refused before any model call", async () => {
    const dir = path.join(scratch, "openai-no-hint");
    const { common } = hostInputs(dir);
    const profile = path.join(dir, "profile.json");
    writeFileSync(profile, JSON.stringify(BETA_PROFILE));
    const before = recorder.requests.length;
    const ran = await cli([
      "load",
      ...common,
      ...["--host", "omp", "--profile", profile, "--model", "openai/gpt-6.1-sol"],
    ]);
    expect(ran.code).toBe(2);
    expect(ran.stderr).toContain(
      "--model openai/gpt-6.1-sol is given with no --hint-model, so the hint model is openai/gpt-6.1-sol"
    );
    expect(recorder.requests.length).toBe(before);
  });

  test("a task that ends above the memory limit stops the test, the message naming that limit", async () => {
    const dir = path.join(scratch, "memory");
    const made = setup(dir);
    const skill = path.join(dir, "alpha");
    writeTree(skill, { "SKILL.md": ALPHA_SKILL, "references/x.md": ALPHA_X });
    const tasks = path.join(dir, "tasks.jsonl");
    const task = { id: "need-1", text: NEED, label: "need", source: "written" };
    writeFileSync(tasks, `${JSON.stringify(task)}\n`);
    const profile = path.join(dir, "profile.json");
    writeFileSync(profile, JSON.stringify(BETA_PROFILE));
    const out = path.join(dir, "out");
    const files = loadCapturePaths(out, "omp", "need-1");
    writeTree(path.dirname(files.body), {
      [path.basename(files.body)]: JSON.stringify({
        ...F2,
        messages: [{ role: "user", content: [{ type: "text", text: NEED }] }],
      }),
      [path.basename(files.record)]: JSON.stringify({
        listing: [{ name: "alpha", hint: "Use for the alpha checklist." }],
        requestHeaders: { "anthropic-version": "2023-06-01" },
        hintCalls: [],
        harness: { head: null, replaced: [] },
      }),
    });
    answer = () => TEXT;
    const stopped = await runLoad({
      io: { out: () => {}, err: () => {}, peakRssBytes: () => 3 * 2 ** 20 },
      host: "omp",
      skillDir: skill,
      skillDigest: "a-made-up-digest",
      tasksFile: tasks,
      profileFile: profile,
      runs: 1,
      out,
      model: LOAD_MODEL,
      thinking: "off",
      gateway: createGateway({
        baseUrl: recorder.url,
        keyFile: made.keyFile,
        concurrency: 2,
        timeoutMs: 60_000,
      }),
      hints: async () => {
        throw new Error("no hint is written here");
      },
      source: localSource(dir),
      scratch: path.join(dir, "scratch"),
      cacheDir: path.join(dir, "cache"),
      configFiles: [],
      claude: "claude",
      requestLog: path.join(dir, "requests.jsonl"),
      memoryLimitBytes: 2 ** 20,
      psqlCommand: "/nowhere/read-store",
      stores: ["store_a"],
    }).catch((caught: unknown) => caught);
    expect(stopped).toBeInstanceOf(Stopped);
    expect(String(stopped)).toContain(
      "the runner's peak resident memory, 3 MiB, is above its limit of 1 MiB, so the test stops after task need-1"
    );
  });
});

describe("skill-cuts load sends a profile's tasks' runs in one order, each with its own id", () => {
  const dir = path.join(scratch, "interleaved");
  const out = path.join(dir, "out");
  const texts3 = [
    "Write the alpha checklist, one.",
    "Write the alpha checklist, two.",
    "Write the beta greeting, three.",
  ];
  const ids = ["t1", "t2", "t3"];
  let args: string[] = [];

  beforeAll(() => {
    const made = setup(dir);
    const skill = path.join(dir, "alpha");
    writeTree(skill, { "SKILL.md": ALPHA_SKILL, "references/x.md": ALPHA_X });
    const tasks = path.join(dir, "tasks.jsonl");
    writeFileSync(
      tasks,
      ids
        .map((id, index) =>
          JSON.stringify({
            id,
            text: texts3[index],
            label: index < 2 ? "need" : "near-miss",
            source: "written",
          })
        )
        .map((line) => `${line}\n`)
        .join("")
    );
    const profile = path.join(dir, "profile.json");
    writeFileSync(profile, JSON.stringify(BETA_PROFILE));
    for (const [index, id] of ids.entries()) {
      const files = loadCapturePaths(out, "omp", id);
      writeTree(path.dirname(files.body), {
        [path.basename(files.body)]: JSON.stringify({
          ...F2,
          messages: [{ role: "user", content: [{ type: "text", text: texts3[index] }] }],
        }),
        [path.basename(files.record)]: JSON.stringify({
          listing: [{ name: "alpha", hint: "Use for the alpha checklist." }],
          requestHeaders: { "anthropic-version": "2023-06-01" },
          hintCalls: [],
          harness: { head: null, replaced: [] },
        }),
      });
    }
    args = [
      "load",
      ...["--config", made.config, "--key-file", made.keyFile, "--skill", skill],
      ...["--tasks", tasks, "--host", "omp", "--profile", profile, "--out", out],
      ...["--scratch", path.join(dir, "scratch"), "--cache-dir", path.join(dir, "cache")],
    ];
  });

  // Catches: a task's runs sent as a batch of their own, or runs that share the captured id.
  test("the runs of every task of a profile go in the seeded order, each with a session id no other run carries", async () => {
    answer = () => TEXT;
    const before = recorder.requests.length;
    const ran = await cli(args);
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const sent = recorder.requests.slice(before).map((request) => JSON.parse(request.body));
    expect(sent).toHaveLength(9);
    const userIds = sent.map((body) => body.metadata.user_id as string);
    expect(new Set(userIds).size).toBe(9);
    expect(userIds).not.toContain(Object(F2.metadata).user_id);
    for (const id of userIds) {
      expect(JSON.parse(id).session_id).toMatch(
        /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/
      );
    }
    // Every run line names its id, the one seed of its profile's sends and its place in them,
    // and the places are the seed's order of the three tasks' runs.
    const runs = ids.flatMap((id) => savedRuns(out, id));
    const seeds = new Set(runs.map((run) => run.sendSeed));
    expect(seeds.size).toBe(1);
    const seed = [...seeds][0] ?? "";
    expect(ran.stdout).toContain(
      `the runs of tasks t1, t2, t3 in the context of ${path.join(dir, "profile.json")} went in one order (seed ${seed}), each with a session id of its own`
    );
    const order = sendOrder(
      ids.map((key) => ({ key, runs: 3 })),
      seed
    );
    for (const id of ids) {
      expect(
        runs
          .filter((run) => run.task === id)
          .map((run) => run.sendPlace)
          .toSorted((a, b) => a - b)
      ).toEqual(order.flatMap((each, place) => (each.key === id ? [place] : [])));
    }
    expect(new Set(runs.map((run) => run.userId))).toEqual(new Set(userIds));
    // The request log names each run's id, when it was sent and the calls in flight then.
    const logged = readFileSync(path.join(out, "requests.jsonl"), "utf8")
      .trim()
      .split("\n")
      .map((line) => JSON.parse(line));
    expect(new Set(logged.map((line) => line.userId))).toEqual(new Set(userIds));
    for (const line of logged) {
      expect(line.inFlight).toBeGreaterThanOrEqual(1);
      expect(Date.parse(line.sentAt)).toBeGreaterThanOrEqual(Date.parse(line.at));
      expect(line.providerRequestId).toMatch(/^req_standin_\d+$/);
    }
  });

  // Catches: runs sent before each had its own id taken up again, or a capture made before the
  // harness-string rule replayed.
  test("an --out whose runs were sent with the captured id, and a saved capture made before the harness-string rule, are refused", async () => {
    const summary = path.join(out, "summary.json");
    const saved = JSON.parse(readFileSync(summary, "utf8"));
    const { sends: _sends, ...before } = saved.settings;
    writeFileSync(summary, JSON.stringify({ ...saved, settings: before }));
    const stale = await cli(args);
    expect(stale.code).toBe(2);
    expect(stale.stderr).toContain("holds a load test of other settings");
    writeFileSync(summary, JSON.stringify(saved));
    // A capture whose record holds no harness record fails its task, saying why.
    const files = loadCapturePaths(out, "omp", "t3");
    const record = JSON.parse(readFileSync(files.record, "utf8"));
    delete record.harness;
    writeFileSync(files.record, JSON.stringify(record));
    writeFileSync(loadRunFile(out, "t3"), "");
    const refused = await cli(args);
    expect(refused.code).toBe(1);
    expect(refused.stderr).toContain("was made before the harness-string rule");
  });

  test("--context-session is <store>:<path hash> of a store the configuration names", async () => {
    const bad = await cli([...args, "--context-session", "store_a:not-a-hash"]);
    expect(bad.code).toBe(2);
    expect(bad.stderr).toContain("--context-session store_a:not-a-hash is not <store>:<path hash>");
    const unknown = await cli([
      ...args,
      "--context-session",
      `store_z:${pathHash("/sessions/z.jsonl")}`,
    ]);
    expect(unknown.code).toBe(2);
    expect(unknown.stderr).toContain("the configuration names no store store_z");
  });
});

// Catches: a Claude Code body that still names the capture's working directory, its scratch
// config directory (in the memory path too), its date or its sandbox's OS version.
test("Claude Code's harness rules replace the working directory, the memory path, the date line and the OS version, and nothing of the session's text", () => {
  const work = "/scratch/load-1/claude/profiles/abc/context/work";
  const config = "/scratch/load-1/claude/profiles/abc/config";
  const head = {
    file: "/made-up/sessions/h.json",
    head: {
      store: "store_a",
      pathHash: pathHash("/sessions/h.jsonl"),
      held: true,
      cwd: "/srv/acme/widgets",
      start: "2026-11-18T08:00:00.000Z",
      firstUser: { timestamp: Date.parse("2026-11-18T08:00:01.000Z"), attribution: "user" },
      reminders: [],
      bytes: 100,
      badLines: 0,
      readAt: "2026-11-21T00:00:00.000Z",
    },
  };
  const facts = sessionFacts(head, {
    turn: undefined,
    date: { value: "2026-11-18", from: "the UTC date of the session's first user turn" },
    devbox: true,
    devboxClaudeConfigDir: "/srv/acme/dotfiles/.claude",
  });
  const projects = `${config}/projects/${work.replace(/[^A-Za-z0-9]/g, "-")}/memory/`;
  const body = {
    model: "claude-sonnet-5-5",
    system: [
      {
        type: "text",
        text: `You have a persistent file-based memory at \`${projects}\`.\nContents of ${config}/CLAUDE.md (user's private global instructions for all projects):\nRules.`,
      },
    ],
    messages: [
      {
        role: "user",
        content: [
          {
            type: "text",
            text: `# Environment\n - Primary working directory: ${work}\n - Is a git repository: false\n - Platform: linux\n - OS Version: Linux 4.19.0-gvisor\n`,
          },
          {
            type: "text",
            text: "<system-reminder>\nToday's date is 2026-11-21.\n</system-reminder>",
          },
          { type: "text", text: "Write the alpha checklist; cost $5." },
        ],
      },
    ],
  };
  const rewritten = rewriteBody(body, claudeHarnessRules(work, config, facts), ["/scratch/load-1"]);
  expect(rewritten.faults).toEqual([]);
  const text = JSON.stringify(rewritten.body);
  expect(text).not.toContain("/scratch/load-1");
  expect(text).not.toContain("gvisor");
  expect(text).toContain("/srv/acme/dotfiles/.claude/projects/-srv-acme-widgets/memory/");
  expect(text).toContain("Contents of /srv/acme/dotfiles/.claude/CLAUDE.md (user's private");
  expect(text).toContain("Primary working directory: /srv/acme/widgets\\n");
  expect(text).toContain("OS Version: Linux\\n");
  expect(text).toContain("Today's date is 2026-11-18.");
  expect(text).toContain("Write the alpha checklist; cost $5.");
  expect(rewritten.replaced).toEqual([
    { what: "the memory directory's project path", count: 1 },
    { what: "the date line", count: 1 },
    { what: "the OS version line", count: 1 },
    { what: "the config directory", count: 1 },
    { what: "the working directory", count: 1 },
  ]);
  expect(renderReminder("2026-11-18", "/srv/acme/widgets")).toContain(
    "current working directory: '/srv/acme/widgets'"
  );
});

/** Made-up repositories (skills alpha, beta and gamma), the tested skill's `tested` files (alpha's
 *  unless given) and the tasks `lines` (one need task unless given) for a real host's run, under
 *  `dir`, with the stand-in's configuration. */
function hostInputs(
  dir: string,
  lines: readonly object[] = [{ id: "need-1", text: NEED, label: "need", source: "written" }],
  tested: Record<string, string> = { "SKILL.md": ALPHA_SKILL, "references/x.md": ALPHA_X }
) {
  const made = setup(dir);
  const repos = path.join(dir, "repos");
  writeTree(path.join(repos, "acme/skills", COMMIT), {
    "skills/alpha/SKILL.md": ALPHA_SKILL,
    "skills/beta/SKILL.md": BETA_SKILL,
    "skills/gamma/SKILL.md": GAMMA_SKILL,
    ".claude/CLAUDE.md": USER_RULES,
  });
  const skill = path.join(dir, "tested");
  writeTree(skill, tested);
  const tasks = path.join(dir, "tasks.jsonl");
  writeFileSync(tasks, lines.map((line) => `${JSON.stringify(line)}\n`).join(""));
  const out = path.join(dir, "out");
  const common = [
    "--config",
    made.config,
    "--key-file",
    made.keyFile,
    "--skill",
    skill,
    "--tasks",
    tasks,
    "--out",
    out,
    "--runs",
    "1",
    "--repos",
    repos,
    "--scratch",
    path.join(dir, "scratch"),
    "--cache-dir",
    path.join(dir, "cache"),
  ];
  return { out, common };
}

describe.skipIf(omp === undefined)("skill-cuts load on the pinned Oh My Pi", () => {
  test(
    "captures the first request of a one-message history, lists the renamed skill under its hint, and counts a read of skill://<name> as a load",
    async () => {
      const dir = path.join(scratch, "omp");
      const { out, common } = hostInputs(dir);
      const models = path.join(dir, "models.yml");
      writeFileSync(models, ANTHROPIC_MODELS);
      const profile = path.join(dir, "profile.json");
      writeFileSync(profile, JSON.stringify(BETA_PROFILE));
      answer = () => [
        { type: "tool_use", id: "toolu_omega", name: "read", input: { path: "skill://omega" } },
      ];
      const ran = await cli([
        "load",
        ...common,
        "--name",
        "omega",
        "--host",
        "omp",
        "--profile",
        profile,
        "--omp",
        String(omp),
        "--models",
        models,
      ]);
      expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
      expect(ran.stdout).toContain(
        "alpha is tested under the name omega: its frontmatter name and its directory's name are changed"
      );
      const body = JSON.parse(readFileSync(loadCapturePaths(out, "omp", "need-1").body, "utf8"));
      const listing = skillListing(body);
      expect([...listing.keys()].sort()).toEqual(["beta", "omega"]);
      expect(listing.get("omega")).toBe("Use for the omega work, stand-in hint.");
      expect(texts(body.messages).some((text) => text.includes(NEED))).toBe(true);
      expect(texts(body).some((text) => text.includes("placeholder prompt"))).toBe(false);
      expect(savedRuns(out, "need-1")).toMatchObject([
        {
          loaded: true,
          loadedBy: "read skill://omega",
          name: "omega",
          listed: "Use for the omega work, stand-in hint.",
        },
      ]);
      expect(ran.stdout).toContain(
        "no verdict: the load test's rule takes 3 runs a task, and this test runs 1"
      );
    },
    HOST_ROW_TIMEOUT_MS
  );

  test(
    "a hint call the budget refuses stops the test at once (exit 3): the capture ends with the gateway's BudgetSpent, not a wait for the hint",
    async () => {
      const dir = path.join(scratch, "omp-budget");
      const { out, common } = hostInputs(dir);
      const models = path.join(dir, "models.yml");
      writeFileSync(models, ANTHROPIC_MODELS);
      const profile = path.join(dir, "profile.json");
      writeFileSync(profile, JSON.stringify(BETA_PROFILE));
      const started = performance.now();
      const ran = await cli([
        "load",
        ...common,
        ...["--host", "omp", "--profile", profile, "--omp", String(omp), "--models", models],
        ...["--budget-usd", "0"],
      ]);
      expect({ code: ran.code, stderr: ran.stderr }).toEqual({
        code: 3,
        stderr:
          "skill-cuts: stopped: the budget is spent, so no new model call starts, and task need-1 has no capture; " +
          `the runs so far are saved under ${out}\n`,
      });
      // Well within the 180 s the hint run's turn waits for a hint it is owed.
      expect(performance.now() - started).toBeLessThan(120_000);
      expect(existsSync(loadCapturePaths(out, "omp", "need-1").body)).toBe(false);
    },
    HOST_ROW_TIMEOUT_MS
  );

  test(
    "captures each of two tasks in its own profile's context: each body lists its own profile's skills, and each run names its profile",
    async () => {
      const dir = path.join(scratch, "omp-profiles");
      const { out, common } = hostInputs(dir, [
        { id: "need-1", text: NEED, label: "need", source: "written", profile: "gamma.json" },
        { id: "near-1", text: NEAR, label: "near-miss", source: "written" },
      ]);
      const models = path.join(dir, "models.yml");
      writeFileSync(models, ANTHROPIC_MODELS);
      const profile = path.join(dir, "profile.json");
      writeFileSync(profile, JSON.stringify(BETA_PROFILE));
      const gamma = path.join(dir, "gamma.json");
      writeFileSync(gamma, JSON.stringify(GAMMA_PROFILE));
      answer = () => READ_ALPHA;
      const ran = await cli([
        "load",
        ...common,
        "--host",
        "omp",
        "--profile",
        profile,
        "--omp",
        String(omp),
        "--models",
        models,
      ]);
      expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
      for (const [id, file, other] of [
        ["need-1", gamma, "gamma"],
        ["near-1", profile, "beta"],
      ] as const) {
        const body = JSON.parse(readFileSync(loadCapturePaths(out, "omp", id).body, "utf8"));
        expect([...skillListing(body).keys()].sort()).toEqual(["alpha", other]);
        expect(savedRuns(out, id)).toMatchObject([
          { loaded: true, profile: file, profileDigest: sha256(file) },
        ]);
      }
    },
    HOST_ROW_TIMEOUT_MS
  );

  test(
    "captures a subagent-context task as a subagent case: its recorded prompt with only the skill's listing line changed, its recorded tools less one nothing defines, which is recorded",
    async () => {
      const dir = path.join(scratch, "omp-subagent");
      const { out, common } = hostInputs(dir, [
        { id: "need-1", text: NEED, label: "need", source: "written", profile: "subagent.json" },
      ]);
      const models = path.join(dir, "models.yml");
      writeFileSync(models, ANTHROPIC_MODELS);
      const subagent = path.join(dir, "subagent.json");
      writeFileSync(
        subagent,
        JSON.stringify({
          ...BETA_PROFILE,
          context: "subagent:task",
          systemPrompt: SUBAGENT.systemPrompt,
          tools: [...SUBAGENT.tools, "acme_absent_tool"],
        })
      );
      answer = () => READ_ALPHA;
      const ran = await cli([
        "load",
        ...common,
        "--host",
        "omp",
        "--omp",
        String(omp),
        "--models",
        models,
      ]);
      expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
      expect(ran.stdout).toContain(
        `load: task need-1 run in the context subagent:task of the profile ${subagent}`
      );
      const files = loadCapturePaths(out, "omp", "need-1");
      const body = JSON.parse(readFileSync(files.body, "utf8"));
      const record = JSON.parse(readFileSync(files.record, "utf8")) as CaptureRecord;
      const hint = "Use for the alpha work, stand-in hint.";
      // The recorded blocks byte for byte, but alpha's listing line, which lists the tested text.
      const sent = body.system.map((block: { text: string }) => block.text);
      expect(sent).toEqual(armPromptBlocks(SUBAGENT.systemPrompt, "alpha", hint).blocks);
      expect(skillListing(body).get("alpha")).toBe(hint);
      expect(record.recordedPrompt?.line).toBe("replaced");
      expect(record.tools?.sent).toEqual(SUBAGENT.tools);
      expect(record.tools?.leftOut.map((tool) => tool.name)).toEqual(["acme_absent_tool"]);
      expect(texts(body.messages).some((text) => text.includes(NEED))).toBe(true);
      expect(savedRuns(out, "need-1")).toMatchObject([
        {
          loaded: true,
          listed: hint,
          profile: subagent,
          profileDigest: sha256(subagent),
        },
      ]);
    },
    HOST_ROW_TIMEOUT_MS
  );

  test(
    "captures a subagent-context task under --name beta-renamed with the recorded listing line naming beta-renamed, and counts its read as a load",
    async () => {
      const dir = path.join(scratch, "omp-subagent-renamed");
      const { out, common } = hostInputs(
        dir,
        [{ id: "need-1", text: NEAR, label: "need", source: "written", profile: "subagent.json" }],
        { "SKILL.md": BETA_SKILL }
      );
      const models = path.join(dir, "models.yml");
      writeFileSync(models, ANTHROPIC_MODELS);
      const subagent = path.join(dir, "subagent.json");
      writeFileSync(
        subagent,
        JSON.stringify({
          skills: [{ repo: "acme/skills", commit: COMMIT, path: "skills/alpha", name: "alpha" }],
          plugins: [],
          contextFiles: [],
          context: "subagent:task",
          systemPrompt: SUBAGENT.systemPrompt,
          tools: SUBAGENT.tools,
        })
      );
      answer = () => [
        {
          type: "tool_use",
          id: "toolu_renamed",
          name: "read",
          input: { path: "skill://beta-renamed" },
        },
      ];
      const ran = await cli([
        "load",
        ...common,
        "--name",
        "beta-renamed",
        "--host",
        "omp",
        "--omp",
        String(omp),
        "--models",
        models,
      ]);
      expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
      expect(ran.stdout).toContain("beta is tested under the name beta-renamed");
      const files = loadCapturePaths(out, "omp", "need-1");
      const body = JSON.parse(readFileSync(files.body, "utf8"));
      const record = JSON.parse(readFileSync(files.record, "utf8")) as CaptureRecord;
      const hint = "Use for the beta-renamed work, stand-in hint.";
      // The recorded blocks byte for byte, but beta's listing line, which names beta-renamed and
      // lists it under its hint; alpha's line stays as recorded.
      const line = "- beta: Use when asked for the beta greeting of the fixture project.";
      const sent = body.system.map((block: { text: string }) => block.text);
      expect(sent).toEqual(
        SUBAGENT.systemPrompt.map((block) => block.replace(line, `- beta-renamed: ${hint}`))
      );
      const listing = skillListing(body);
      expect([...listing.keys()].sort()).toEqual(["alpha", "beta-renamed"]);
      expect(listing.get("beta-renamed")).toBe(hint);
      expect(record.recordedPrompt?.line).toBe("replaced");
      expect(savedRuns(out, "need-1")).toMatchObject([
        {
          loaded: true,
          loadedBy: "read skill://beta-renamed",
          name: "beta-renamed",
          listed: hint,
          profile: subagent,
        },
      ]);
    },
    HOST_ROW_TIMEOUT_MS
  );

  test(
    "on openai/gpt-6.1-sol: captures the Responses body Oh My Pi sends, its hints from the Anthropic hint model, and counts a function call reading skill://<name> as a load",
    async () => {
      const dir = path.join(scratch, "omp-openai");
      const { out, common } = hostInputs(dir);
      const models = path.join(dir, "models.yml");
      // The pod's models.yml shape: the gateway's Messages route, and its OpenAI route as Oh My
      // Pi's built-in openai provider at the gateway's /v1, with no api and no models list.
      writeFileSync(
        models,
        "providers:\n  anthropic:\n    baseUrl: http://127.0.0.1:9\n    api: anthropic-messages\n    compat: {replayUnsignedThinking: false, supportsLongCacheRetention: true}\n    models:\n      - {id: claude-sonnet-5-5, name: Claude Sonnet 5.5, reasoning: true, input: [text, image], contextWindow: 1000000, maxTokens: 128000, cost: {input: 2, output: 10, cacheRead: 0.1, cacheWrite: 2.5}}\n  openai:\n    baseUrl: http://127.0.0.1:9/v1\n    compat: {statefulResponses: false}\n    modelOverrides:\n      gpt-6.1-sol: {contextWindow: 922000}\n"
      );
      const profile = path.join(dir, "profile.json");
      writeFileSync(profile, JSON.stringify(BETA_PROFILE));
      answer = () => [
        { type: "tool_use", id: "call_alpha_1", name: "read", input: { path: "skill://alpha" } },
      ];
      const before = recorder.requests.length;
      const ran = await cli([
        "load",
        ...common,
        "--host",
        "omp",
        "--profile",
        profile,
        "--omp",
        String(omp),
        "--models",
        models,
        "--model",
        "openai/gpt-6.1-sol",
        "--hint-model",
        "anthropic/claude-sonnet-5-5",
      ]);
      expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
      expect(ran.stdout).toContain("model openai/gpt-6.1-sol");
      const files = loadCapturePaths(out, "omp", "need-1");
      const body = JSON.parse(readFileSync(files.body, "utf8"));
      // F8's shape: the prompt as `instructions`, the history as `input` items, no messages.
      expect(body.model).toBe("gpt-6.1-sol");
      expect(body.messages).toBeUndefined();
      expect(typeof body.instructions).toBe("string");
      expect(JSON.stringify(body)).not.toContain("cache_control");
      expect(texts(body.input).some((text) => text.includes(NEED))).toBe(true);
      const listing = skillListing(body);
      expect([...listing.keys()].sort()).toEqual(["alpha", "beta"]);
      expect(listing.get("alpha")).toBe("Use for the alpha work, stand-in hint.");
      const record = JSON.parse(readFileSync(files.record, "utf8"));
      expect(record.requestHeaders).toEqual({});
      expect(record.connections.http).toContain("POST /v1/responses");
      expect(record.hintCalls.map((call: { model: string }) => call.model)).toEqual([
        "claude-sonnet-5-5",
        "claude-sonnet-5-5",
      ]);
      // The run went to the Responses route; the hints to the Messages route.
      const sent = recorder.requests.slice(before);
      expect(sent.filter((request) => request.path === "/v1/responses")).toHaveLength(1);
      expect(sent.filter((request) => request.path === "/v1/messages")).toHaveLength(2);
      expect(savedRuns(out, "need-1")).toMatchObject([
        {
          loaded: true,
          loadedBy: "read skill://alpha",
          toolCalls: ["read"],
          stopReason: "completed",
          usage: { model: "gpt-6.1-sol", reasoning: 0 },
        },
      ]);
    },
    HOST_ROW_TIMEOUT_MS
  );
});

describe.skipIf(claude === undefined)("skill-cuts load on Claude Code", () => {
  test(
    "installs each task's profile and the tested skill as local plugins, gives a devbox profile's context file as user memory, captures each task's first request with its own profile's listing and its headers, and counts a Skill call as a load",
    async () => {
      const dir = path.join(scratch, "claude");
      const { out, common } = hostInputs(dir, [
        { id: "need-1", text: NEED, label: "need", source: "written" },
        { id: "near-1", text: NEAR, label: "near-miss", source: "written", profile: "other.json" },
      ]);
      const profile = path.join(dir, "profile.json");
      writeFileSync(
        profile,
        JSON.stringify({
          ...BETA_PROFILE,
          contextFiles: [USER_FILE],
          claude: {
            marketplace: "acme-market",
            plugins: [{ name: "acme-tools", skills: ["beta"] }],
            skillPlugin: "acme-tools",
          },
        })
      );
      const other = path.join(dir, "other.json");
      writeFileSync(
        other,
        JSON.stringify({
          ...GAMMA_PROFILE,
          claude: {
            marketplace: "other-market",
            plugins: [{ name: "other-tools", skills: ["gamma"] }],
            skillPlugin: "other-tools",
          },
        })
      );
      // Each request gets a Skill call on alpha under the plugin its own listing names.
      answer = (body) => [
        {
          type: "tool_use",
          id: "toolu_skill",
          name: "Skill",
          input: {
            skill: JSON.stringify(body).includes("- other-tools:alpha: ")
              ? "other-tools:alpha"
              : "acme-tools:alpha",
          },
        },
      ];
      const ran = await cli([
        "load",
        ...common,
        "--host",
        "claude",
        "--profile",
        profile,
        "--thinking",
        "off",
        "--claude",
        String(claude),
      ]);
      expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
      const description = "Use when a task asks for the alpha checklist of the made-up project.";
      for (const [id, file, plugin, sibling, text, memory] of [
        ["need-1", profile, "acme-tools", "beta", NEED, USER_FILE],
        ["near-1", other, "other-tools", "gamma", NEAR, null],
      ] as const) {
        const files = loadCapturePaths(out, "claude", id);
        const body = JSON.parse(readFileSync(files.body, "utf8"));
        const listing = claudeListing(body);
        expect(listing.get(`${plugin}:alpha`)).toBe(description);
        // Only the plugins of this task's own profile, each with its own skills.
        const plugged = [...listing.keys()].filter((name) => name.includes(":")).sort();
        expect(plugged).toEqual([`${plugin}:alpha`, `${plugin}:${sibling}`].sort());
        expect(texts(body.messages).some((each) => each.includes(text))).toBe(true);
        // The devbox profile's context file is Claude Code's user memory, and no project's.
        const sent = texts(body.messages).join("\n");
        const rule = "End each answer with the word done.";
        expect(sent.includes(rule)).toBe(memory !== null);
        if (memory !== null) {
          const said = sent.slice(0, sent.indexOf(rule));
          expect(said).toContain("(user's private global instructions for all projects)");
          expect(said).not.toContain("(project instructions");
        }
        // Thinking off: the body asks for none.
        expect(body.thinking).toBeUndefined();
        const record = z
          .looseObject({
            requestHeaders: z.record(z.string(), z.string()),
            profile: z.string(),
            profileDigest: z.string(),
            userMemory: z.unknown(),
          })
          .parse(JSON.parse(readFileSync(files.record, "utf8")));
        expect(record.userMemory).toEqual(memory);
        expect(record.requestHeaders["anthropic-beta"]).toBeString();
        expect(
          Object.keys(record.requestHeaders).every((name) => name.startsWith("anthropic-"))
        ).toBe(true);
        expect([record.profile, record.profileDigest]).toEqual([file, sha256(file)]);
        expect(savedRuns(out, id)).toMatchObject([
          {
            loaded: true,
            loadedBy: `Skill ${plugin}:alpha`,
            toolCalls: ["Skill"],
            profile: file,
            profileDigest: sha256(file),
          },
        ]);
        expect(ran.stdout).toContain(
          `Claude Code lists ${plugin}:alpha as "${description}" in the context of ${file}`
        );
      }
    },
    HOST_ROW_TIMEOUT_MS
  );
});
