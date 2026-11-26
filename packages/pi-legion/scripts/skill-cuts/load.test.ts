import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { createHash } from "node:crypto";
import {
  existsSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { readDispatchFirstContext } from "@legion/envoy-client/dispatch-first";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { z } from "zod";
import { skillHeaderPath } from "../skill-scenarios/transcript";
import { ListingMeasure } from "./build";
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
import { cacheKey, ghSource, localSource } from "./context";
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
  FailedRun,
  type Host,
  LOAD_MODEL,
  LoadRun,
  loadCapturePaths,
  loadFailedRunFile,
  loadProfile,
  loadRunFile,
  messageLoad,
  mustRun,
  namesSkill,
  type RecordedListing,
  recordedLines,
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
import { MIXED_UNDECIDED, sendOrder } from "./replay";
import { LISTING_MEASURE_ITEM, type TaskLabel } from "./score";
import { answeredBy, pinnedTo, truncatedSkillsTree, writeGhStandIn } from "./testdata/fixtures";

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
    [
      "Oh My Pi: a read of another source's alpha, by its namespaced path or by skill://tools/alpha, does not load the tested alpha",
      "omp",
      [
        functionCall("read", { path: "/work/namespaced/tools/skills/alpha/SKILL.md" }),
        functionCall("read", { path: "skill://tools/alpha" }),
      ],
      alpha,
      none(["read", "read"]),
    ],
  ];
  test.each(rows)("%s", (_name, host, content, skill, want) => {
    expect(ALPHA_PATH).toEndWith("/alpha/SKILL.md");
    expect(messageLoad(host, content, skill)).toEqual(want as never);
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

/** What a made-up run came to: `true` (it loaded the skill), `false` (it did not), `sent by
 *  <skill>`, or `reads past last round`. */
type RunKind = boolean | `sent by ${string}` | "reads past last round";

/** The outcome a made-up run of `kind` came to. */
function outcomeOf(kind: RunKind): LoadRun["outcome"] {
  if (kind === true) return "loaded";
  if (kind === false) return "not loaded";
  return kind.startsWith("sent by ") ? "sent by" : "reads past last round";
}

/** The runs of one task, each of `kinds`, as its run file keeps them. */
function runsOf(label: TaskLabel, kinds: readonly RunKind[]): LoadRun[] {
  return kinds.map((kind, index) => {
    const sentBy = typeof kind === "string" && kind.startsWith("sent by ") ? kind.slice(8) : null;
    return LoadRun.parse({
      task: "t",
      run: index + 1,
      label,
      source: "written",
      host: "omp",
      name: "alpha",
      outcome: outcomeOf(kind),
      loadedBy: kind === true ? "read skill://alpha" : null,
      sentBy,
      skillReads: "answered",
      replies: [
        {
          calls: kind === true ? [{ name: "read", target: "skill://alpha" }] : [],
          answers: null,
          usage: {
            model: "claude-sonnet-5-5",
            input: 1,
            output: 1,
            cacheRead: 0,
            cacheWrite: 0,
            usd: 0,
          },
        },
      ],
      stopReason: kind === true ? "tool_use" : "end_turn",
      requestId: null,
      listed: "Use for the alpha checklist.",
      profile: "/profiles/alpha.json",
      profileDigest: "0".repeat(64),
      userId: JSON.stringify({ session_id: `00000000-0000-4000-8000-00000000000${index}` }),
      sendSeed: "made-up-seed",
      sendPlace: index,
    });
  });
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

  // Catches: a run sent by another skill or still reading counted as a miss, a task run again
  // past its 5 extra runs, or a task short of its judged runs read as a fault or a pass.
  test("runs sent by another skill or reading past the last round are not judged: the task runs again, at most 5 extra runs, then gives no description evidence, saying why, and the skill's verdict leaves it out", () => {
    const sent = "sent by beta";
    const past = "reads past last round";
    const going = taskOutcome("need", runsOf("need", [true, sent, past, false]), 3);
    expect(going).toMatchObject({
      made: 4,
      judged: 2,
      loads: 1,
      sentBy: { beta: 1 },
      readsPastLastRound: 1,
      extraRuns: 1,
      toStart: 1,
      complete: false,
      noEvidence: null,
      fault: null,
    });
    const done = taskOutcome("need", runsOf("need", [true, sent, past, false, true]), 3);
    expect(done).toMatchObject({
      judged: 3,
      loads: 2,
      extraRuns: 2,
      toStart: 0,
      complete: true,
      fault: false,
    });
    const short = taskOutcome(
      "near-miss",
      runsOf("near-miss", [false, sent, sent, sent, "sent by gamma", past, sent, true]),
      3
    );
    expect(short).toMatchObject({
      made: 8,
      judged: 2,
      sentBy: { beta: 4, gamma: 1 },
      extraRuns: 5,
      toStart: 0,
      complete: false,
      fault: null,
      noEvidence:
        "2 of 3 judged runs after 5 extra runs: sent by beta in 4 of 8 runs; sent by gamma in 1 of 8 runs; reads past the last round in 1 of 8 runs",
    });
    const n1 = { id: "n1", label: "need" as const };
    const m1 = { id: "m1", label: "near-miss" as const, outcome: short };
    expect(skillOutcome([{ ...n1, outcome: going }, m1], 3)).toEqual({
      descriptionFault: null,
      reason: "no verdict yet: tasks n1 lack runs",
    });
    expect(skillOutcome([{ ...n1, outcome: done }, m1], 3)).toEqual({
      descriptionFault: false,
      reason:
        "no description fault in 1 tasks; tasks m1 give no description evidence: they lack their 3 judged runs after 5 extra runs",
    });
    expect(skillOutcome([m1], 3)).toEqual({
      descriptionFault: null,
      reason:
        "no verdict: no task gives description evidence (tasks m1 lack their 3 judged runs after 5 extra runs)",
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

  // Catches: a SKILL.md that starts with a byte order mark read as UTF-8 with the mark kept, so no
  // frontmatter was found and the copy was refused, where Oh My Pi reads the file (dropping the
  // mark) and its frontmatter.
  test("a SKILL.md that starts with a byte order mark is renamed, the mark kept where it is", () => {
    const dir = path.join(scratch, "rename", "marked");
    writeTree(dir, { "SKILL.md": `\uFEFF${ALPHA_SKILL}` });
    const copy = renamedSkill(dir, "omega", path.join(scratch, "rename", "copies"));
    expect(readFileSync(path.join(copy, "SKILL.md"), "utf8")).toBe(
      `\uFEFF${ALPHA_SKILL.replace("name: alpha", "name: omega")}`
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

describe("a load lists a skill under its session's recorded line only where the arm's name and text are the session's", () => {
  const ALPHA_TEXT = "Use when a task asks for the alpha checklist of the made-up project.";
  const listing: RecordedListing = {
    source: { kind: "session" },
    lines: [
      { name: "alpha", text: "Use for the alpha checklist, as the session listed it." },
      { name: "beta", text: "Use for the beta greeting, as the session listed it." },
    ],
    tested: {
      repo: "acme/skills",
      commit: "0123456789abcdef0123456789abcdef01234567",
      path: "skills/alpha",
      name: "alpha",
    },
  };
  const alphaLine = { name: "alpha", text: listing.lines[0]?.text, from: "session" };
  const betaLine = { name: "beta", text: listing.lines[1]?.text, from: "session" };
  const own = { name: "alpha", ownName: "alpha", description: ALPHA_TEXT };
  test.each([
    [
      "the tested skill under its own name and the session's text keeps its line, given with that text, and every other skill keeps its own",
      own,
      ALPHA_TEXT,
      [{ ...alphaLine, description: ALPHA_TEXT }, betaLine],
    ],
    [
      "a name test's proposed name gets a drawn hint though the session recorded a line: only the other skills keep theirs",
      { ...own, name: "omega" },
      ALPHA_TEXT,
      [betaLine],
    ],
    [
      "a text that differs from the session's copy's gets a drawn hint",
      { ...own, description: `${ALPHA_TEXT} It names the new text.` },
      ALPHA_TEXT,
      [betaLine],
    ],
    [
      "a session copy whose text is not known gives the tested skill no line",
      own,
      undefined,
      [betaLine],
    ],
  ])("%s", (_name, tested, sessionDescription, want) => {
    expect(recordedLines(listing, tested, sessionDescription)).toEqual(want);
  });

  test("a subagent's listing names its path hash as each line's source, and a profile that keeps no listing gives no line", () => {
    const hash = "a".repeat(64);
    const subagent: RecordedListing = {
      source: { kind: "subagent", pathHash: hash },
      lines: listing.lines,
    };
    expect(recordedLines(subagent, own, ALPHA_TEXT)).toEqual([
      { ...alphaLine, from: `subagent ${hash}`, description: ALPHA_TEXT },
      { ...betaLine, from: `subagent ${hash}` },
    ]);
    expect(recordedLines(undefined, own, ALPHA_TEXT)).toEqual([]);
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

/** How the stand-in answers a run's request: with the blocks of its reply, or a response it
 *  gives as it is (a refusal). */
let answer: (body: Request) => ReplyBlock[] | Response = () => [{ type: "text", text: "nothing" }];

/** Whether the stand-in names the deployment that answered each request that goes with a routing
 *  id (`x-litellm-model-id`, `pinnedTo`), as the gateway does; off but in the rows that set it. */
let pinDeployments = false;

/** The stand-in's answer to `request`: Oh My Pi's hint request gets a hint, any other model
 *  request what `answer` gives for its body; a request of no model route, none. */
function standInReply(request: RecordedRequest): Response | undefined {
  if (
    request.method !== "POST" ||
    (request.path !== "/v1/messages" && request.path !== "/v1/responses")
  ) {
    return undefined;
  }
  const body = Request.parse(JSON.parse(request.body));
  // Oh My Pi's hint request: no system prompt, no tools, `Skill: <name>` in its message.
  const hint = /\\nSkill: ([^\\]+)\\nDescription: /.exec(request.body)?.[1];
  const answered =
    body.system === undefined && body.tools === undefined && hint !== undefined
      ? [{ type: "text" as const, text: `Use for the ${hint} work, stand-in hint.` }]
      : answer(body);
  if (answered instanceof Response) return answered;
  const content: ReplyBlock[] = answered;
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
}

let recorder: Recorder;
beforeAll(() => {
  recorder = startRecorder({
    route: (request: RecordedRequest) => {
      const reply = standInReply(request);
      const routing = request.headers["x-litellm-session-id"];
      if (reply === undefined || !pinDeployments || routing === undefined) return reply;
      return answeredBy(reply, pinnedTo(routing));
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
/** BETA_PROFILE with a Claude Code layout: beta in the plugin acme-tools of acme-market, which
 *  takes the tested skill too. */
const BETA_CLAUDE_PROFILE = {
  ...BETA_PROFILE,
  claude: {
    marketplace: "acme-market",
    plugins: [{ name: "acme-tools", skills: ["beta"] }],
    skillPlugin: "acme-tools",
  },
};
const GAMMA_PROFILE = {
  skills: [{ repo: "acme/skills", commit: COMMIT, path: "skills/gamma", name: "gamma" }],
  plugins: [],
  contextFiles: [],
};
/** The made-up commit of acme/legion a Legion project's daemon image was built from; a Legion
 *  profile of beta whose plugin is of acme/legion at COMMIT; and the evidence item build-tasks
 *  writes for that project (context.ts's DAEMON_IMAGE_ITEM), naming DAEMON_COMMIT. */
const DAEMON_COMMIT = "fedcba9876543210fedcba9876543210fedcba98";
const LEGION_BETA_PROFILE = {
  ...BETA_PROFILE,
  plugins: [
    { repo: "acme/legion", commit: COMMIT, path: "plugins/skills-only", name: "skills-only" },
  ],
  context: "legion:acme:implementer",
};
const DAEMON_DIGEST = "8".repeat(64);
const DAEMON_ITEM: EvidenceItem = {
  item: "the daemon image's commit of acme/legion",
  source: `acme/infra:legion/projects/10-ACME.yaml at ${COMMIT} gives the project ACME the daemonImage registry.example/acme/daemon@sha256:${DAEMON_DIGEST}; gh api repos/acme/legion/attestations/sha256:${DAEMON_DIGEST} names git+https://github.com/acme/legion@refs/heads/main at ${DAEMON_COMMIT}, so the role prompt is composed from acme/legion at ${DAEMON_COMMIT}: the Legion daemon of a pod composes its role prompt as it was at the commit its daemon image was built from`,
};
/** The user-level context file of a devbox profile (build.ts: Claude Code's user memory of the
 *  repository that installs its dev box's units), in the made-up acme/skills. */
const USER_RULES = "# Made-up user rules\n\nEnd each answer with the word done.\n";
const USER_FILE = {
  repo: "acme/skills",
  commit: COMMIT,
  path: ".claude/CLAUDE.md",
  user: true as const,
};
/** The root CLAUDE.md of the made-up acme/proj: the project context file of a devbox profile
 *  whose session's working directory was a checkout of acme/proj (build.ts). */
const PROJECT_RULES = "# Made-up project rules\n\nStart each answer with the word ready.\n";
const PROJECT_FILE = { repo: "acme/proj", commit: COMMIT, path: "CLAUDE.md" };
/** Two project skills of acme/proj (build.ts lists both as the session repository's): `kappa` in
 *  `.claude/skills` below the checkout's root, with a reference, and `lambda` in `.omp/skills` at
 *  the root. A session in `agents/agentvm` of that checkout reads both levels. */
const KAPPA_FILES = {
  "SKILL.md":
    "---\nname: kappa\ndescription: Use when a task asks for the kappa summary of the made-up project.\n---\n\n# Kappa\n\nRead references/k.md first.\n",
  "references/k.md": "The kappa summary is one line.\n",
};
const LAMBDA_SKILL =
  "---\nname: lambda\ndescription: Use when a task asks for the lambda list of the made-up project.\n---\n\n# Lambda\n\nList in one line.\n";
const KAPPA = {
  repo: "acme/proj",
  commit: COMMIT,
  path: "agents/.claude/skills/kappa",
  name: "kappa",
};
const LAMBDA = { repo: "acme/proj", commit: COMMIT, path: ".omp/skills/lambda", name: "lambda" };
const PROJ_CHECKOUT = {
  sessionRepository: { repo: "acme/proj", commit: COMMIT },
  workingDirectory: "agents/agentvm",
};
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

/** A made-up dispatch-first, which the Envoy plugin reads as it loads and puts into each request
 *  of a session with Dispatch; its entry in a profile; and the Envoy plugin's entry. */
const DISPATCH_FIRST_SKILL =
  "---\nname: dispatch-first\ndescription: Use when a task of the made-up project needs its tracker searched first.\n---\n\n# Dispatch first\n\nSearch the made-up tracker before you act.\n";
const DISPATCH_FIRST_ENTRY = {
  repo: "acme/skills",
  commit: COMMIT,
  path: "skills/dispatch-first",
  name: "dispatch-first",
};
const ENVOY_ENTRY = {
  repo: "acme/legion",
  commit: COMMIT,
  path: "packages/pi-envoy",
  name: "pi-envoy",
};

/** A saved capture's context record, as far as these tests write it: the skills it installed
 *  (`profileContext`), and any other field of a ContextRecord a row reads. */
type SavedContext = { readonly skills: readonly unknown[] } & Readonly<Record<string, unknown>>;

/** The SKILL.md of each made-up repository skill a profile here installs, by its name. */
const COPY_TEXTS: Readonly<Record<string, string>> = { beta: BETA_SKILL, gamma: GAMMA_SKILL };

/** The context record of a capture in the context of `profile`: each of the profile's skills
 *  installed from its repository, in the profile's order, under the cache key context.ts gives it
 *  (`cacheKey` of its repository, commit and path), then `tested` from the arm. A saved capture
 *  whose record names other copies than its task's profile gives is refused (capture.ts's
 *  `restoreCopies`). Writes each copy under `cache` at that key, its SKILL.md the text `texts`
 *  gives its name, so the capture read back makes none of them again and reads no repository. */
function profileContext(
  profile: {
    readonly skills: readonly {
      readonly repo: string;
      readonly commit: string;
      readonly path: string;
      readonly name: string;
    }[];
  },
  cache: string,
  { texts = COPY_TEXTS, tested = "alpha" }: { texts?: Record<string, string>; tested?: string } = {}
): SavedContext {
  const copies = profile.skills.map(({ repo, commit, path: at, name }) => {
    const key = cacheKey(repo, commit, at);
    const text = texts[name];
    if (text === undefined) throw new Error(`no SKILL.md of the profile's skill ${name} is given`);
    writeTree(path.join(cache, "skills", key), { "SKILL.md": text });
    return { name, from: { repo, commit, path: at, key } };
  });
  return { skills: [...copies, { name: tested, from: "arm" }] };
}

/** Saves under `out` each task's Oh My Pi capture, where the command reads it back: F2's body
 *  with the task's text as its one message, and the fields of its record that the load test
 *  reads, `record`'s over them. Its context is `context`, which names the copies the task's
 *  profile gives (`profileContext`), or the capture is refused when it is read back. */
function saveCaptures(
  out: string,
  lines: readonly { id: string; text: string }[],
  context: SavedContext,
  record: Record<string, unknown> = {}
): void {
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
        context,
        ...record,
      }),
    });
  }
}

/** The record fields over saveCaptures' of a capture that lists alpha alone and sends no beta
 *  header. */
const ALPHA_LISTED = {
  listing: [{ name: "alpha", hint: "Use for the alpha checklist." }],
  requestHeaders: { "anthropic-version": "2023-06-01" },
};

/** A Claude Code capture's context record that installed no copy of a skill: a load reads no
 *  copy of a Claude Code capture, whose runs answer no read (context.ts's `installedCopies`
 *  reads an Oh My Pi capture's `skills`). */
const NO_COPIES = { skills: [] };

/** One item of a profile's evidence, as a build-tasks summary keeps it. */
interface EvidenceItem {
  readonly item: string;
  readonly source: string;
}

/** A listing measure as build.ts writes it into a task profile's evidence (ListingMeasure): its
 *  session's recorded listing holding `recorded` names, `missing` of them not in the rebuilt
 *  listing, which adds `added` to them; `kind` says whose listing it is (`session listing`: the
 *  session's own recorded system prompt, which the task's capture sends whole; `subagent
 *  listing`: a subagent's, the capture's listing built from the rebuilt profile); with `recorded`
 *  null, not measurable. */
function measureItem(
  recorded: number | null,
  missing: string[] = [],
  added: string[] = [],
  kind: "session listing" | "subagent listing" = "session listing"
): EvidenceItem {
  const measure = ListingMeasure.parse({
    kind: recorded === null ? "not measurable" : kind,
    recorded,
    listed: 2,
    missing,
    added,
    missingShare: recorded === null || recorded === 0 ? null : missing.length / recorded,
    sessionDigest: null,
    rebuiltDigest: null,
    subagent: null,
    why:
      recorded === null
        ? "the made-up session records no system prompt, and the store holds no subagent of it"
        : "the made-up session recorded its system prompt",
  });
  return { item: LISTING_MEASURE_ITEM, source: JSON.stringify(measure) };
}

/** Writes beside the tasks file `tasks` the summary build-tasks writes (`<tasks>.summary.json`):
 *  the evidence of each session task's profile (`sessions`, by task id) and of the context
 *  session's (`context`, then `more`), which each written task takes, each its one listing
 *  measure. */
function writeTasksSummary(
  tasks: string,
  sessions: Record<string, EvidenceItem>,
  context: EvidenceItem,
  more: readonly EvidenceItem[] = []
): void {
  writeFileSync(
    `${tasks}.summary.json`,
    JSON.stringify({
      command: "build-tasks",
      skill: "alpha",
      profiles: {
        dir: path.dirname(tasks),
        contextSession: {
          store: "store_a",
          hash: "made-up-context",
          pathHash: pathHash("/sessions/context.jsonl"),
          context: "devbox",
          secretsRemoved: 0,
          evidence: [context, ...more],
        },
        tasks: Object.entries(sessions).map(([task, measure]) => ({
          task,
          from: "session",
          evidence: [measure],
        })),
      },
    })
  );
}

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
    // The build-tasks summary beside it: both tasks' listings measured, none missing a name.
    writeTasksSummary(tasks, { "need-1": measureItem(10) }, measureItem(10));
    writeFileSync(profile, JSON.stringify(BETA_PROFILE));
    writeFileSync(profileA, JSON.stringify(GAMMA_PROFILE));
    // Each capture names the copies its own task's profile gives: need-1's gamma, near-1's beta.
    const cache = path.join(dir, "cache");
    saveCaptures(out, lines.slice(0, 1), profileContext(GAMMA_PROFILE, cache));
    saveCaptures(out, lines.slice(1), profileContext(BETA_PROFILE, cache));
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
      cache,
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
      /task need-1 \(need, from a session\): loaded in 2 of 3 judged runs \(.*\); runs sent by another skill: 0; runs that read past the last round: 0; extra runs: 0: no fault/
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
      /task near-1 \(near-miss, written\): loaded in 2 of 3 judged runs \(.*\); runs sent by another skill: 0; runs that read past the last round: 0; extra runs: 0: a fault: a near-miss task loads it in more than 1 of 3 runs/
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
      expect(runs.filter((run) => run.outcome === "loaded").map((run) => run.loadedBy)).toEqual([
        "read skill://alpha",
        "read skill://alpha",
      ]);
      expect(runs.every((run) => run.listed === "Use for the alpha checklist.")).toBe(true);
      expect(
        runs.every((run) => run.replies.every((reply) => reply.usage.model === "claude-sonnet-5-5"))
      ).toBe(true);
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

// Catches: a summary and a report that do not say where each task's listing line of the tested
// skill came from, so a verdict that rests on drawn hints reads as one that rests on the lines
// the sessions recorded.
test("the summary names, for each task, the line its capture lists the tested skill under and where that line came from, and counts them; the report gives how many tasks list it under a recorded line and how many under a draw, and on Claude Code under its description", async () => {
  const dir = path.join(scratch, "listed-under");
  const made = setup(dir);
  const skill = path.join(dir, "alpha");
  writeTree(skill, { "SKILL.md": ALPHA_SKILL, "references/x.md": ALPHA_X });
  const tasks = path.join(dir, "tasks.jsonl");
  const lines = [
    { id: "need-1", text: NEED, label: "need", source: "written" },
    { id: "near-1", text: NEAR, label: "near-miss", source: "written" },
  ];
  writeFileSync(tasks, lines.map((line) => `${JSON.stringify(line)}\n`).join(""));
  const profile = path.join(dir, "profile.json");
  writeFileSync(profile, JSON.stringify(BETA_CLAUDE_PROFILE));
  const cache = path.join(dir, "cache");
  const args = (host: Host, out: string) => [
    "load",
    ...["--config", made.config, "--key-file", made.keyFile, "--skill", skill, "--tasks", tasks],
    ...["--host", host, "--profile", profile, "--out", out, "--runs", "1"],
    ...["--scratch", path.join(dir, "scratch"), "--cache-dir", cache],
  ];
  const ALPHA_HINT = "Use for the alpha checklist.";
  /** A capture record's line of alpha and of beta, each from `source`. */
  const hints = (source: string) => ({
    hints: [
      { skill: "alpha", hint: ALPHA_HINT, key: "k1", ompKey: "o1", written: false, source },
      {
        skill: "beta",
        hint: "Use for the beta greeting.",
        key: "k2",
        ompKey: "o2",
        written: false,
        source,
      },
    ],
  });
  answer = () => TEXT;

  const ompOut = path.join(dir, "omp");
  const copies = profileContext(BETA_CLAUDE_PROFILE, cache);
  saveCaptures(ompOut, lines.slice(0, 1), copies, hints("recorded (session)"));
  saveCaptures(ompOut, lines.slice(1), copies, hints("drawn (draw 1)"));
  const omp = await cli(args("omp", ompOut));
  expect({ code: omp.code, stderr: omp.stderr }).toEqual({ code: 0, stderr: "" });
  expect(omp.stdout).toContain(
    "load: the listing line of alpha: 1 tasks list it under the line their session recorded, 1 under a hint of draw 1\n"
  );
  const summaryOf = (out: string) =>
    z
      .looseObject({
        tasks: z.array(z.looseObject({ id: z.string(), listedUnder: z.unknown() })),
        listedUnder: z.unknown(),
      })
      .parse(JSON.parse(readFileSync(path.join(out, "summary.json"), "utf8")));
  const ompSummary = summaryOf(ompOut);
  expect(ompSummary.tasks.map((task) => [task.id, task.listedUnder])).toEqual([
    ["need-1", { source: "recorded (session)", line: ALPHA_HINT }],
    ["near-1", { source: "drawn (draw 1)", line: ALPHA_HINT }],
  ]);
  expect(ompSummary.listedUnder).toEqual({ recorded: 1, drawn: 1, description: 0, none: 0 });

  // Claude Code lists the skill under its description: no hint is drawn or recorded there.
  const claudeOut = path.join(dir, "claude");
  const listed = "Use when a task asks for the alpha checklist of the made-up project.";
  for (const { id, text } of lines) {
    const files = loadCapturePaths(claudeOut, "claude", id);
    writeTree(path.dirname(files.body), {
      [path.basename(files.body)]: JSON.stringify({
        ...F2,
        messages: [{ role: "user", content: [{ type: "text", text }] }],
      }),
      [path.basename(files.record)]: JSON.stringify({
        requestHeaders: { "anthropic-version": "2023-06-01" },
        listed,
        listedSource: "description (Claude Code)",
        harness: { head: null, replaced: [] },
        context: NO_COPIES,
      }),
    });
  }
  const claude = await cli(args("claude", claudeOut));
  expect({ code: claude.code, stderr: claude.stderr }).toEqual({ code: 0, stderr: "" });
  expect(claude.stdout).toContain(
    "load: the listing line of alpha: 2 tasks list it under its description, since Claude Code lists no hint\n"
  );
  const claudeSummary = summaryOf(claudeOut);
  expect(claudeSummary.tasks.map((task) => task.listedUnder)).toEqual(
    Array(2).fill({ source: "description (Claude Code)", line: listed })
  );
  expect(claudeSummary.listedUnder).toEqual({ recorded: 0, drawn: 0, description: 2, none: 0 });
});

// The skill reads rule (the architect, 2026-11-22T18:58Z and 18:59:51Z): a load run answers a
// reply that only reads skill files as the behaviour replay does, a run sent by another skill or
// still reading after the last round is not judged and runs again, and the record shows what each
// read read. Catches: a run that stops at a first message that reads another skill, so that a
// skill the agent reads next counts as not loaded; a run counted as a load when another skill's
// text sent the agent to it; and a record that keeps only the names of the first message's calls.
describe("skill-cuts load answers skill reads as the behaviour replay does", () => {
  const dir = path.join(scratch, "skill-reads");
  const skill = path.join(dir, "alpha");
  let made = { config: "", keyFile: "" };

  beforeAll(() => {
    made = setup(dir);
    writeTree(skill, { "SKILL.md": ALPHA_SKILL, "references/x.md": ALPHA_X });
  });

  /** A load test of alpha under `<dir>/<id>` on `host`: the one need task `id`, written, in the
   *  context of `profile` (BETA_PROFILE unless given), with its capture saved, whose context
   *  installed the profile's beta from its entry in the test's cache (`cache`), which holds
   *  `beta` as beta's SKILL.md; its listing measured; and the arguments that run it. */
  function readsTest(
    id: string,
    beta: string,
    host: Host = "omp",
    profile: typeof BETA_PROFILE = BETA_PROFILE
  ): { out: string; cache: string; args: string[] } {
    const root = path.join(dir, id);
    const out = path.join(root, "out");
    const cache = path.join(root, "cache");
    const tasks = path.join(root, "tasks.jsonl");
    writeTree(root, {
      "tasks.jsonl": `${JSON.stringify({ id, text: NEED, label: "need", source: "written" })}\n`,
      "profile.json": JSON.stringify(profile),
    });
    writeTasksSummary(tasks, {}, measureItem(10));
    const context = profileContext(profile, cache, { texts: { beta } });
    if (host === "omp") saveCaptures(out, [{ id, text: NEED }], context);
    else {
      const files = loadCapturePaths(out, "claude", id);
      writeTree(path.dirname(files.body), {
        [path.basename(files.body)]: JSON.stringify({
          ...F2,
          messages: [{ role: "user", content: [{ type: "text", text: NEED }] }],
        }),
        [path.basename(files.record)]: JSON.stringify({
          requestHeaders: { "anthropic-version": "2023-06-01" },
          listed: "Use for the alpha checklist.",
          harness: { head: null, replaced: [] },
          context,
        }),
      });
    }
    const args = [
      "load",
      ...["--config", made.config, "--key-file", made.keyFile, "--skill", skill],
      ...["--tasks", tasks, "--host", host, "--profile", path.join(root, "profile.json")],
      ...["--out", out, "--scratch", path.join(root, "scratch"), "--cache-dir", cache],
    ];
    return { out, cache, args };
  }

  /** An Oh My Pi `read` of `target`, as the stand-in answers with it. */
  const readOf = (target: string, id = "toolu_read"): ReplyBlock => ({
    type: "tool_use",
    id,
    name: "read",
    input: { path: target },
  });
  /** How many rounds of reads a request's body answers: each adds a reply and its answers. */
  const answeredRounds = (body: Request): number =>
    Array.isArray(body.messages) ? (body.messages.length - 1) / 2 : 0;
  /** The messages of a request the stand-in took. */
  const messagesOf = (request: RecordedRequest): unknown[] =>
    z.object({ messages: z.array(z.unknown()) }).parse(JSON.parse(request.body)).messages;
  /** The answers a request's last message holds: each `tool_result`'s text and error flag. */
  const answersOf = (request: RecordedRequest): { text: string; isError: boolean }[] =>
    z
      .object({
        content: z.array(
          z.object({
            type: z.literal("tool_result"),
            content: z.array(z.object({ text: z.string() })),
            is_error: z.boolean(),
          })
        ),
      })
      .parse(messagesOf(request).at(-1))
      .content.map((result) => ({
        text: result.content.map((part) => part.text).join(""),
        isError: result.is_error,
      }));
  /** The task `id` of the summary under `out`, as far as these rows read it. */
  const summaryTask = (out: string, id: string) =>
    z
      .looseObject({ tasks: z.array(z.looseObject({ id: z.string() })) })
      .parse(JSON.parse(readFileSync(path.join(out, "summary.json"), "utf8")))
      .tasks.find((task) => task.id === id);

  test("a first message that reads another installed skill, whose text names no skill, then a read of the tested skill, is a load", async () => {
    const s = readsTest("plain", BETA_SKILL);
    answer = (body) => (answeredRounds(body) === 0 ? [readOf("skill://beta")] : READ_ALPHA);
    const before = recorder.requests.length;
    const ran = await cli(s.args);
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    // Each run sends its first request, then the one that answers beta's read, whose reply loads
    // alpha and ends the run.
    const sent = recorder.requests.slice(before);
    expect(sent.map((request) => messagesOf(request).length).toSorted()).toEqual([
      1, 1, 1, 3, 3, 3,
    ]);
    // Beta's SKILL.md as a `skill://` read of beta's installed copy shows it.
    const shown = `[Skill file: /skills/beta/SKILL.md]\n${BETA_SKILL.replace(/\n$/, "")}`;
    for (const request of sent.filter((each) => messagesOf(each).length === 3)) {
      expect(answersOf(request)).toEqual([{ text: shown, isError: false }]);
    }
    expect(
      savedRuns(s.out, "plain").map((run) => [run.outcome, run.loadedBy, run.replies.length])
    ).toEqual(Array(3).fill(["loaded", "read skill://alpha", 2]));
    expect(ran.stdout).toContain(
      "load: task plain (need, written): loaded in 3 of 3 judged runs (run 1 loads, run 2 loads, run 3 loads); runs sent by another skill: 0; runs that read past the last round: 0; extra runs: 0: no fault"
    );
  });

  // Catches: a saved capture replayed on a machine whose cache lacks beta's installed copy (the
  // cache is not kept with --out), so each run that reads beta fails.
  test("a saved capture whose cache lacks beta's installed copy has it made again from beta's repository, and a first message that reads beta is answered from it", async () => {
    const key = cacheKey("acme/skills", COMMIT, "skills/beta");
    const s = readsTest("restored", BETA_SKILL);
    // The cache of a new machine holds no copy of beta; beta's repository holds it.
    rmSync(path.join(s.cache, "skills", key), { recursive: true });
    const repos = path.join(dir, "restored", "repos");
    writeTree(path.join(repos, "acme/skills", COMMIT), { "skills/beta/SKILL.md": BETA_SKILL });
    answer = (body) => (answeredRounds(body) === 0 ? [readOf("skill://beta")] : READ_ALPHA);
    const before = recorder.requests.length;
    const ran = await cli([...s.args, "--repos", repos]);
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    expect(readFileSync(path.join(s.cache, "skills", key, "SKILL.md"), "utf8")).toBe(BETA_SKILL);
    const shown = `[Skill file: /skills/beta/SKILL.md]\n${BETA_SKILL.replace(/\n$/, "")}`;
    const answered = recorder.requests
      .slice(before)
      .filter((request) => messagesOf(request).length === 3);
    expect(answered.map((request) => answersOf(request))).toEqual(
      Array(3).fill([{ text: shown, isError: false }])
    );
    expect(savedRuns(s.out, "restored").map((run) => run.outcome)).toEqual(Array(3).fill("loaded"));
  });

  test('a run whose answered skill text names the tested skill as skill://<name> is "sent by" that skill: not counted, and the task runs again', async () => {
    const sends = `${BETA_SKILL}Then read skill://alpha for the checklist.\n`;
    const s = readsTest("sent", sends);
    // The first two runs read beta first; every later one reads alpha.
    let firsts = 0;
    answer = (body) => {
      if (answeredRounds(body) > 0) return TEXT;
      firsts += 1;
      return firsts <= 2 ? [readOf("skill://beta")] : READ_ALPHA;
    };
    const before = recorder.requests.length;
    const ran = await cli(s.args);
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    // A run sent by beta ends once beta's answer is made: no request carries it.
    const sent = recorder.requests.slice(before);
    expect(sent.map((request) => messagesOf(request).length)).toEqual([1, 1, 1, 1, 1]);
    const runs = savedRuns(s.out, "sent");
    expect(runs.map((run) => run.outcome).toSorted()).toEqual([
      "loaded",
      "loaded",
      "loaded",
      "sent by",
      "sent by",
    ]);
    for (const run of runs.filter((each) => each.outcome === "sent by")) {
      expect([run.sentBy, run.loadedBy, run.replies.at(-1)?.answers]).toEqual([
        "beta",
        null,
        [{ skill: "beta", file: "SKILL.md", by: "skill", from: "copy", isError: false }],
      ]);
    }
    expect(summaryTask(s.out, "sent")).toMatchObject({
      made: 5,
      judged: 3,
      loads: 3,
      sentBy: { beta: 2 },
      readsPastLastRound: 0,
      extraRuns: 2,
      complete: true,
      noEvidence: null,
      fault: false,
    });
    expect(ran.stdout).toMatch(
      /^load: the extra runs, for runs that are not judged, of task sent \(2\) in the context of [^\n]* went in one order \(seed [0-9a-f]+\), each with a session id of its own$/m
    );
    expect(ran.stdout).toContain(
      "; runs sent by another skill: 2 (beta 2); runs that read past the last round: 0; extra runs: 2: no fault"
    );
  });

  test("a bare-word mention of the tested skill's name in the other skill's text does not stop the load", async () => {
    const bare = `${BETA_SKILL}The alpha checklist comes after the greeting; greet as alpha does.\n`;
    const s = readsTest("bare", bare);
    answer = (body) => (answeredRounds(body) === 0 ? [readOf("skill://beta")] : READ_ALPHA);
    const ran = await cli(s.args);
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    expect(savedRuns(s.out, "bare").map((run) => [run.outcome, run.sentBy])).toEqual(
      Array(3).fill(["loaded", null])
    );
  });

  test.each([
    ["Then read skill://alpha for the checklist.", "alpha", true],
    ["Read skill://alpha/references/x.md first.", "alpha", true],
    ["Run /skill:alpha now.", "alpha", true],
    ["Use `alpha` here.", "alpha", true],
    ["Load the skill alpha next.", "alpha", true],
    ["Skill alpha.", "alpha", true],
    ["The alpha skill lists the steps.", "alpha", true],
    ["The alpha checklist comes after the greeting; greet as alpha does.", "alpha", false],
    ["The alphabet skill and skill alphabet.", "alpha", false],
    ["Use `alpha-two` and skill://alpha-two.", "alpha", false],
    ["The alpha skills.", "alpha", false],
    ["Analyze the logs, then analyze the plan.", "analyze", false],
    ["Write the sdd plan.", "sdd", false],
  ])("the text %p names %p: %p", (text, name, want) => {
    expect(namesSkill(text, name)).toBe(want);
  });

  test("a read of a skill the profile does not install gets Oh My Pi's unknown-skill answer, and the run goes on", async () => {
    const s = readsTest("unknown", BETA_SKILL);
    answer = (body) => (answeredRounds(body) === 0 ? [readOf("skill://zeta")] : READ_ALPHA);
    const before = recorder.requests.length;
    const ran = await cli(s.args);
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const answered = recorder.requests
      .slice(before)
      .filter((request) => messagesOf(request).length === 3);
    expect(answered.map(answersOf)).toEqual(
      Array(3).fill([{ text: "Unknown skill: zeta\nAvailable: beta, alpha", isError: true }])
    );
    expect(savedRuns(s.out, "unknown").map((run) => run.outcome)).toEqual(Array(3).fill("loaded"));
  });

  test("a run still reading after round 3 gets no verdict and runs again; a task still short after its 5 extra runs gives no description evidence, and the report names the reason", async () => {
    const s = readsTest("past", BETA_SKILL);
    answer = () => [readOf("skill://beta")];
    const before = recorder.requests.length;
    const ran = await cli(s.args);
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    // 3 runs and 5 extra runs, each its first request and 3 answered rounds.
    expect(recorder.requests.length - before).toBe(32);
    const runs = savedRuns(s.out, "past");
    expect(runs.map((run) => [run.outcome, run.replies.length])).toEqual(
      Array(8).fill(["reads past last round", 4])
    );
    const reason =
      "0 of 3 judged runs after 5 extra runs: reads past the last round in 8 of 8 runs";
    expect(summaryTask(s.out, "past")).toMatchObject({
      made: 8,
      judged: 0,
      readsPastLastRound: 8,
      extraRuns: 5,
      toStart: 0,
      complete: false,
      noEvidence: reason,
      fault: null,
    });
    expect(ran.stdout).toContain(
      `; runs sent by another skill: 0; runs that read past the last round: 8; extra runs: 5: no description evidence: ${reason}`
    );
    expect(ran.stdout).toContain(
      "load: the skill alpha: no verdict: no task gives description evidence (tasks past lack their 3 judged runs after 5 extra runs)"
    );
    // A command started again makes no run of it.
    const again = await cli(s.args);
    expect(again.code).toBe(0);
    expect(again.stdout).toContain("task past (need, written): its run file holds its extra runs");
    expect(recorder.requests.length - before).toBe(32);
  });

  // Catches: a run whose later request fails dropped whole, so that what its earlier replies used
  // is in no total, and a total that forgets a failed run once the command is started again.
  test("a run whose second round fails after the first succeeded counts the first round's spend, in its task's line and the summary's totals", async () => {
    const s = readsTest("failing", BETA_SKILL);
    let failing = true;
    answer = (body) => {
      if (answeredRounds(body) === 0) return [readOf("skill://beta")];
      return failing
        ? new Response(JSON.stringify({ error: { message: "made-up refusal" } }), { status: 400 })
        : READ_ALPHA;
    };
    const ran = await cli(s.args);
    expect(ran.code).toBe(1);
    expect(ran.stderr).toContain("load: task failing: a run failed: ");
    expect(readFileSync(loadRunFile(s.out, "failing"), "utf8")).toBe("");
    const failed = readFileSync(loadFailedRunFile(s.out, "failing"), "utf8")
      .trim()
      .split("\n")
      .map((line) => FailedRun.parse(JSON.parse(line)));
    expect(
      failed.map((run) => run.usage.map(({ model, input, output }) => [model, input, output]))
    ).toEqual(Array(3).fill([["claude-sonnet-5-5", 10, 3]]));
    const failedUsd = failed.reduce((sum, run) => sum + (run.usage[0]?.usd ?? 0), 0);
    expect(failedUsd).toBeGreaterThan(0);
    const Spend = z.object({
      runsUsd: z.number(),
      failedRunsUsd: z.number(),
      hintsUsd: z.number(),
      usd: z.number(),
    });
    const spendOf = () => {
      const summary = JSON.parse(readFileSync(path.join(s.out, "summary.json"), "utf8"));
      const task = z
        .object({ spend: Spend.extend({ failedRuns: z.number() }) })
        .parse(summaryTask(s.out, "failing"));
      return { total: Spend.parse(Reflect.get(summary, "spend")), task: task.spend };
    };
    const first = spendOf();
    expect(first.task).toEqual({
      runsUsd: 0,
      failedRuns: 3,
      failedRunsUsd: failedUsd,
      hintsUsd: 0,
      usd: failedUsd,
    });
    expect(first.total).toEqual({
      runsUsd: 0,
      failedRunsUsd: failedUsd,
      hintsUsd: 0,
      usd: failedUsd,
    });
    expect(ran.stdout).toContain(
      `; spend at list price: runs $0.0000, 3 failed runs $${failedUsd.toFixed(4)}, hints $0.0000, all $${failedUsd.toFixed(4)}; seconds: `
    );
    // A command started again counts them still, beside the runs it makes.
    failing = false;
    const again = await cli(s.args);
    expect({ code: again.code, stderr: again.stderr }).toEqual({ code: 0, stderr: "" });
    const runsUsd = savedRuns(s.out, "failing").reduce(
      (sum, run) => sum + run.replies.reduce((each, reply) => each + reply.usage.usd, 0),
      0
    );
    const second = spendOf();
    expect(second.total.failedRunsUsd).toBe(failedUsd);
    expect(second.total.runsUsd).toBeCloseTo(runsUsd, 12);
    expect(second.total.usd).toBeCloseTo(runsUsd + failedUsd, 12);
  });

  test("the record holds each reply's call targets and each answer's source", async () => {
    const s = readsTest("record", BETA_SKILL);
    const byPath = "/home/agent/skills/alpha/SKILL.md";
    answer = (body) =>
      answeredRounds(body) === 0
        ? [readOf("skill://beta", "toolu_beta"), readOf("skill://zeta", "toolu_zeta")]
        : [readOf(byPath)];
    const ran = await cli(s.args);
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    for (const run of savedRuns(s.out, "record")) {
      expect(run).toMatchObject({
        outcome: "loaded",
        loadedBy: `read ${byPath}`,
        sentBy: null,
        skillReads: "answered",
        stopReason: "tool_use",
        replies: [
          {
            calls: [
              { name: "read", target: "skill://beta" },
              { name: "read", target: "skill://zeta" },
            ],
            answers: [
              { skill: "beta", file: "SKILL.md", by: "skill", from: "copy", isError: false },
              { skill: "zeta", file: "SKILL.md", by: "skill", from: "absent", isError: true },
            ],
          },
          { calls: [{ name: "read", target: byPath }], answers: null },
        ],
      });
      expect(run.replies.map((reply) => reply.usage.model)).toEqual([
        "claude-sonnet-5-5",
        "claude-sonnet-5-5",
      ]);
    }
  });

  test("a Claude Code run ends at its first message, which its record says: a Read of another skill's SKILL.md is not answered", async () => {
    const s = readsTest("claude-first", BETA_SKILL, "claude", BETA_CLAUDE_PROFILE);
    const betaPath = "/config/plugins/cache/acme-market/acme-tools/0.0.0/skills/beta/SKILL.md";
    answer = () => [
      { type: "tool_use", id: "toolu_read_beta", name: "Read", input: { file_path: betaPath } },
    ];
    const before = recorder.requests.length;
    const ran = await cli(s.args);
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    expect(recorder.requests.length - before).toBe(3);
    expect(
      savedRuns(s.out, "claude-first").map((run) => [run.outcome, run.skillReads, run.replies])
    ).toEqual(
      Array(3).fill([
        "not loaded",
        "first message",
        [
          {
            calls: [{ name: "Read", target: betaPath }],
            answers: null,
            usage: expect.objectContaining({ model: "claude-sonnet-5-5" }),
          },
        ],
      ])
    );
  });

  // The load test's mixed-reply rule (the runner's architect, 2026-11-26T06:20:08Z): a reply that
  // reads another skill beside other calls is sorted by what those calls can do, with no model.
  // Catches: a batch of a skill read and a look counted as the task started (not loaded), a call
  // that starts the task continued past, and a call no rule can sort counted as either.
  /** An Oh My Pi `bash` call of `command`, as the stand-in answers with it. */
  const bashOf = (command: string, id = "toolu_bash"): ReplyBlock => ({
    type: "tool_use",
    id,
    name: "bash",
    input: { command },
  });
  /** Beta's SKILL.md as a `skill://` read of beta's installed copy shows it. */
  const BETA_SHOWN = `[Skill file: /skills/beta/SKILL.md]\n${BETA_SKILL.replace(/\n$/, "")}`;

  test("a read of another skill beside a bash command that only reads or looks continues: the read is answered, the command left out of every body and recorded, and the next reply's load counts", async () => {
    const s = readsTest("mixed-looks", BETA_SKILL);
    const looks = bashOf("ls -la && git status");
    answer = (body) => (answeredRounds(body) === 0 ? [readOf("skill://beta"), looks] : READ_ALPHA);
    const before = recorder.requests.length;
    const ran = await cli(s.args);
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const sent = recorder.requests.slice(before);
    expect(sent.map((request) => messagesOf(request).length).toSorted()).toEqual([
      1, 1, 1, 3, 3, 3,
    ]);
    for (const request of sent.filter((each) => messagesOf(each).length === 3)) {
      expect(messagesOf(request).at(-2)).toEqual({
        role: "assistant",
        content: [readOf("skill://beta")],
      });
      expect(answersOf(request)).toEqual([{ text: BETA_SHOWN, isError: false }]);
    }
    expect(sent.some((request) => request.body.includes("toolu_bash"))).toBe(false);
    const runs = savedRuns(s.out, "mixed-looks");
    expect(runs.map((run) => [run.outcome, run.loadedBy, run.leftOut])).toEqual(
      Array(3).fill([
        "loaded",
        "read skill://alpha",
        [{ round: 1, tool: "bash", argument: "ls -la && git status" }],
      ])
    );
    expect(runs[0]?.replies[0]?.calls).toEqual([
      { name: "read", target: "skill://beta" },
      { name: "bash", target: null, argument: "ls -la && git status" },
    ]);
  });

  test("a read of another skill beside a write of a file ends the run there, not loaded, as before", async () => {
    const s = readsTest("mixed-write", BETA_SKILL);
    const write: ReplyBlock = {
      type: "tool_use",
      id: "toolu_write",
      name: "write",
      input: { path: "checklist.md", content: "# Alpha" },
    };
    answer = () => [readOf("skill://beta"), write];
    const before = recorder.requests.length;
    const ran = await cli(s.args);
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    expect(recorder.requests.length - before).toBe(3);
    expect(
      savedRuns(s.out, "mixed-write").map((run) => [run.outcome, run.replies.length, run.leftOut])
    ).toEqual(Array(3).fill(["not loaded", 1, undefined]));
  });

  test("a read of another skill beside a bash command off the read-only list is undecided: no verdict, its reason naming the call, and the task runs again up to its extra runs", async () => {
    const s = readsTest("mixed-off-list", BETA_SKILL);
    answer = () => [readOf("skill://beta"), bashOf("make test")];
    const before = recorder.requests.length;
    const ran = await cli(s.args);
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    expect(recorder.requests.length - before).toBe(8);
    const runs = savedRuns(s.out, "mixed-off-list");
    expect(runs.map((run) => [run.outcome, run.reason, run.replies.length])).toEqual(
      Array(8).fill([
        "undecided",
        `${MIXED_UNDECIDED}: after 0 answered rounds of skill reads, the reply reads skill files (beta SKILL.md) beside what the rule sorts neither as an act nor as preparation (bash make test), so it is neither judged nor continued`,
        1,
      ])
    );
    const reason =
      "0 of 3 judged runs after 5 extra runs: undecided at a mixed reply in 8 of 8 runs";
    expect(summaryTask(s.out, "mixed-off-list")).toMatchObject({
      made: 8,
      judged: 0,
      undecided: 8,
      extraRuns: 5,
      complete: false,
      noEvidence: reason,
    });
    expect(ran.stdout).toContain(
      `; runs that read past the last round: 0; runs undecided at a mixed reply: 8; extra runs: 5: no description evidence: ${reason}`
    );
  });

  test("the squash-stack shape: a read of skill://sdd/references/review-and-merge.md beside a bash command off the list is undecided, not a run that does not load", async () => {
    const s = readsTest("squash-stack", BETA_SKILL);
    answer = () => [
      readOf("skill://sdd/references/review-and-merge.md"),
      bashOf("jj squash --from @ --into @- && jj git push --bookmark stack"),
    ];
    const ran = await cli(s.args);
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const runs = savedRuns(s.out, "squash-stack");
    expect(runs.map((run) => run.outcome)).toEqual(Array(8).fill("undecided"));
    expect(runs[0]?.reason).toContain(
      "reads skill files (sdd references/review-and-merge.md) beside what the rule sorts neither as an act nor as preparation (bash jj squash --from @ --into @- && jj git push --bookmark stack)"
    );
  });

  test("a Claude Code run still ends at its first message: a Read of another skill beside a Bash call is not answered, and the run does not load", async () => {
    const s = readsTest("claude-mixed", BETA_SKILL, "claude", BETA_CLAUDE_PROFILE);
    const betaPath = "/config/plugins/cache/acme-market/acme-tools/0.0.0/skills/beta/SKILL.md";
    answer = () => [
      { type: "tool_use", id: "toolu_read_beta", name: "Read", input: { file_path: betaPath } },
      { type: "tool_use", id: "toolu_bash", name: "Bash", input: { command: "ls" } },
    ];
    const before = recorder.requests.length;
    const ran = await cli(s.args);
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    expect(recorder.requests.length - before).toBe(3);
    expect(
      savedRuns(s.out, "claude-mixed").map((run) => [
        run.outcome,
        run.skillReads,
        run.replies.map((reply) => reply.calls),
        run.leftOut,
      ])
    ).toEqual(
      Array(3).fill([
        "not loaded",
        "first message",
        [
          [
            { name: "Read", target: betaPath },
            { name: "Bash", target: null },
          ],
        ],
        undefined,
      ])
    );
  });

  test("load audit-mixed sorts each saved run's last reply by the rule with no model call: one that would continue, one that ends at an action, one unresolved for a command its record lacks, one past the last round; a loaded run and a run with no skill read are left as they are", async () => {
    const s = readsTest("audit", BETA_SKILL);
    answer = () => TEXT;
    expect((await cli(s.args)).code).toBe(0);
    const [template] = savedRuns(s.out, "audit");
    if (template === undefined) throw new Error("no run was saved");
    const usage = template.replies[0]?.usage;
    if (usage === undefined) throw new Error("the saved run has no reply");
    type Reply = LoadRun["replies"][number];
    const beta = { name: "read", target: "skill://beta" };
    const betaRead = {
      skill: "beta",
      file: "SKILL.md",
      by: "skill" as const,
      from: "copy" as const,
      isError: false,
    };
    const planted = (n: number, fields: Partial<LoadRun>) =>
      LoadRun.parse({ ...template, run: n, userId: `user-${n}`, ...fields });
    const reply = (calls: Reply["calls"], answers: Reply["answers"] = null): Reply => ({
      calls,
      answers,
      usage,
    });
    const runs = [
      // Made before the record kept a bash call's command: the rule cannot tell what it does.
      planted(1, { replies: [reply([beta, { name: "bash", target: null }])] }),
      planted(2, {
        replies: [reply([beta, { name: "bash", target: null, argument: "git log -3" }])],
      }),
      planted(3, {
        replies: [reply([beta, { name: "write", target: null, argument: "checklist.md" }])],
      }),
      planted(4, {
        outcome: "loaded",
        loadedBy: "read skill://alpha",
        replies: [reply([{ name: "read", target: "skill://alpha" }])],
      }),
      planted(5, { replies: [reply([])] }),
      planted(6, {
        replies: [
          ...Array(3).fill(reply([beta], [betaRead])),
          reply([beta, { name: "glob", target: null }]),
        ],
      }),
    ];
    writeFileSync(
      loadRunFile(s.out, "audit"),
      runs.map((run) => `${JSON.stringify(run)}\n`).join("")
    );
    const before = recorder.requests.length;
    const audited = await cli([
      "load",
      "audit-mixed",
      "--out",
      s.out,
      "--skill",
      skill,
      "--cache-dir",
      s.cache,
    ]);
    expect({ code: audited.code, stderr: audited.stderr }).toEqual({ code: 0, stderr: "" });
    expect(recorder.requests.length).toBe(before);
    const named = (n: number, tools: string[]) => ({ run: n, userId: `user-${n}`, tools });
    const file = path.join(s.out, "audit-mixed.json");
    const written = JSON.parse(readFileSync(file, "utf8"));
    expect(written.tasks).toEqual([
      {
        task: "audit",
        runs: 6,
        judged: 6,
        untouched: 1,
        ends: 1,
        skillRound: 0,
        actions: [named(3, ["write"])],
        continues: [named(2, ["bash"])],
        sentBy: [],
        pastLastRound: [named(6, ["glob"])],
        unresolved: [named(1, ["bash"])],
      },
    ]);
    expect(audited.stdout).toContain(
      "load audit-mixed: task audit: 6 runs, 6 judged; ended before the rule 1; ended at an action 1 (run 3: write); continued now 1 (run 2: bash); sent by another skill's answer now 0; no verdict past the last round 1 (run 6: glob); unresolved 1 (run 1: bash); only skill reads 0; no mixed reply 1\n"
    );
    expect(audited.stdout).toContain(`; written to ${file}\n`);
    expect(readFileSync(file, "utf8")).not.toContain("git log");
  });
});

// The listing rule (the architect, 2026-11-22T03:04Z; on added names too, 2026-11-24T23:52Z):
// every load applies it, from the listing measure of each task's profile in the tasks file's
// build-tasks summary.
describe("skill-cuts load applies the listing rule", () => {
  const NEED_2 = "List the alpha steps of the made-up project.";
  /** A load test under `scratch/<name>`: alpha against `lines` (session tasks of store_a unless
   *  written), each in BETA_PROFILE's context with its capture saved, and the arguments that run
   *  it; the stand-in answers a run of each text with a read of skill://alpha in its first `reads`
   *  runs. */
  const listingTest = (
    name: string,
    lines: readonly { id: string; text: string; label: TaskLabel; written?: true }[],
    reads: ReadonlyMap<string, number>
  ) => {
    const dir = path.join(scratch, name);
    const made = setup(dir);
    const skill = path.join(dir, "alpha");
    writeTree(skill, { "SKILL.md": ALPHA_SKILL, "references/x.md": ALPHA_X });
    const tasks = path.join(dir, "tasks.jsonl");
    writeFileSync(
      tasks,
      lines
        .map(({ id, text, label, written }) =>
          JSON.stringify(
            written
              ? { id, text, label, source: "written" }
              : { id, text, label, source: "session", store: "store_a", pathHash: pathHash(id) }
          )
        )
        .map((line) => `${line}\n`)
        .join("")
    );
    const profile = path.join(dir, "profile.json");
    writeFileSync(profile, JSON.stringify(BETA_PROFILE));
    const out = path.join(dir, "out");
    const cache = path.join(dir, "cache");
    saveCaptures(out, lines, profileContext(BETA_PROFILE, cache));
    const answered = new Map<string, number>();
    answer = (body) => {
      const text = texts(body.messages).find((each) => reads.has(each)) ?? "";
      const n = (answered.get(text) ?? 0) + 1;
      answered.set(text, n);
      return n <= (reads.get(text) ?? 0) ? READ_ALPHA : TEXT;
    };
    const args = [
      ...["load", "--config", made.config, "--key-file", made.keyFile, "--skill", skill],
      ...["--tasks", tasks, "--host", "omp", "--profile", profile, "--out", out],
      ...["--omp", path.join(dir, "no-omp-runs-here"), "--scratch", path.join(dir, "scratch")],
      ...["--cache-dir", cache],
    ];
    return { tasks, out, args };
  };
  /** The summary `out` holds, as far as these rows read it. */
  const summaryOf = (out: string) =>
    z
      .looseObject({
        tasks: z.array(z.looseObject({ id: z.string(), made: z.number(), dropped: z.unknown() })),
        listing: z.unknown(),
        verdict: z.unknown(),
        verdicts: z.unknown(),
      })
      .parse(JSON.parse(readFileSync(path.join(out, "summary.json"), "utf8")));
  /** The requests the stand-in took since `from` whose body holds `text`. */
  const sentWith = (from: number, text: string) =>
    recorder.requests.slice(from).filter((request) => request.body.includes(text)).length;

  test("a task whose rebuilt listing misses more than 10% of its session's recorded names is dropped with that reason: in the summary's tasks, not run, counted, and out of the verdict; a task whose profile took names from its recorded listing is counted as such", async () => {
    const { tasks, out, args } = listingTest(
      "listing-drop",
      [
        { id: "need-1", text: NEED, label: "need" },
        { id: "near-1", text: NEAR, label: "near-miss", written: true },
        { id: "need-2", text: NEED_2, label: "need" },
      ],
      new Map([
        [NEED, 2],
        [NEAR, 2],
        [NEED_2, 0],
      ])
    );
    // need-1's profile took a name from its recorded listing (build.ts's fromRecorded mark).
    const taken = {
      ...measureItem(10),
      source: JSON.stringify({ ...JSON.parse(measureItem(10).source), fromRecorded: ["cw-one"] }),
    };
    writeTasksSummary(
      tasks,
      { "need-1": taken, "need-2": measureItem(10, ["beta", "gamma"]) },
      measureItem(10, ["beta"])
    );
    const from = recorder.requests.length;
    const ran = await cli(args);
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const misses =
      "listing-misses: its rebuilt listing misses 2 of the 10 names of the recorded session listing (20.0%), more than 10%: beta, gamma";
    expect(ran.stdout).toContain(`task need-2 (need, from a session): dropped, not run: ${misses}`);
    expect(ran.stdout).toContain(
      `the listing rule (${tasks}.summary.json): 2 tasks' listings measured, 0 not measurable, 1 tasks dropped (1 listing-misses, 0 listing-adds), each missing more than 10% of the names its session's recorded listing holds or, where its capture builds its listing from the rebuilt profile, adding more names than 10% of them, 1 installed names their recorded listing gave them (fromRecorded); `
    );
    expect(sentWith(from, NEED_2)).toBe(0);
    expect(sentWith(from, NEED)).toBe(3);
    const summary = summaryOf(out);
    expect(summary.tasks.map((task) => [task.id, task.made, task.dropped])).toEqual([
      ["need-1", 3, null],
      ["near-1", 3, null],
      ["need-2", 0, misses],
    ]);
    expect(summary.listing).toEqual({
      summary: `${tasks}.summary.json`,
      measured: 2,
      notMeasurable: 0,
      dropped: 1,
      droppedBy: { "listing-misses": 1, "listing-adds": 0 },
      fromRecorded: 1,
    });
    expect(summary.verdict).toEqual({
      descriptionFault: true,
      reason: "a description fault: tasks near-1",
    });
  });

  // The architect, 2026-11-24T23:52Z: the listing rule drops on added names too, for a task whose
  // capture builds its listing from the rebuilt profile (a measure of kind `subagent listing`).
  test("a task whose profile-built listing adds more names than 10% of its session's recorded names is dropped with the listing-adds reason: in the summary's tasks, not run, and counted by its reason; one that adds 10% runs", async () => {
    const { tasks, out, args } = listingTest(
      "listing-adds",
      [
        { id: "need-1", text: NEED, label: "need" },
        { id: "near-1", text: NEAR, label: "near-miss", written: true },
        { id: "need-2", text: NEED_2, label: "need" },
      ],
      new Map([
        [NEED, 2],
        [NEAR, 0],
        [NEED_2, 0],
      ])
    );
    writeTasksSummary(
      tasks,
      {
        "need-1": measureItem(10, [], ["cw-one"], "subagent listing"),
        "need-2": measureItem(10, [], ["cw-one", "cw-two"], "subagent listing"),
      },
      measureItem(10)
    );
    const from = recorder.requests.length;
    const ran = await cli(args);
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const adds =
      "listing-adds: its rebuilt listing adds 2 to the 10 names of the recorded subagent listing (20.0%), more than 10%: cw-one, cw-two";
    expect(ran.stdout).toContain(`task need-2 (need, from a session): dropped, not run: ${adds}`);
    expect(ran.stdout).toContain(
      `the listing rule (${tasks}.summary.json): 2 tasks' listings measured, 0 not measurable, 1 tasks dropped (0 listing-misses, 1 listing-adds), each missing more than 10% of the names its session's recorded listing holds or, where its capture builds its listing from the rebuilt profile, adding more names than 10% of them, 0 installed names their recorded listing gave them (fromRecorded); `
    );
    expect(sentWith(from, NEED_2)).toBe(0);
    expect(sentWith(from, NEED)).toBe(3);
    const summary = summaryOf(out);
    expect(summary.tasks.map((task) => [task.id, task.made, task.dropped])).toEqual([
      ["need-1", 3, null],
      ["near-1", 3, null],
      ["need-2", 0, adds],
    ]);
    expect(summary.listing).toEqual({
      summary: `${tasks}.summary.json`,
      measured: 2,
      notMeasurable: 0,
      dropped: 1,
      droppedBy: { "listing-misses": 0, "listing-adds": 1 },
      fromRecorded: 0,
    });
  });

  // The architect, 2026-11-25T00:52:35Z: a task whose capture sends its session's recorded system
  // prompt whole (a measure of kind `session listing`) shows the model the recorded listing
  // whatever its profile installs, so names its profile adds drop nothing; load counts the runs of
  // such tasks that read a skill outside that listing, by skill and answer kind.
  test("a recorded-prompt task whose rebuilt listing adds over 10% runs (is not dropped), and load records and prints the runs that read a skill outside the recorded listing, by skill and answer kind", async () => {
    const dir = path.join(scratch, "listing-recorded-prompt-adds");
    const made = setup(dir);
    const skill = path.join(dir, "alpha");
    writeTree(skill, { "SKILL.md": ALPHA_SKILL, "references/x.md": ALPHA_X });
    const cache = path.join(dir, "cache");
    // need-1's session recorded its system prompt (which lists alpha and beta); its profile adds
    // gamma and two more names to them. need-2 and near-1 hold no recorded prompt.
    const subagent = {
      ...BETA_PROFILE,
      skills: [
        ...BETA_PROFILE.skills,
        { repo: "acme/skills", commit: COMMIT, path: "skills/gamma", name: "gamma" },
      ],
      context: "subagent:task",
      systemPrompt: SUBAGENT.systemPrompt,
      tools: SUBAGENT.tools,
    };
    writeTree(dir, {
      "subagent.json": JSON.stringify(subagent),
      "plain.json": JSON.stringify(BETA_PROFILE),
    });
    const lines = [
      {
        id: "need-1",
        text: NEED,
        label: "need",
        source: "session",
        store: "store_a",
        pathHash: pathHash("need-1"),
        profile: "subagent.json",
      },
      {
        id: "need-2",
        text: NEED_2,
        label: "need",
        source: "session",
        store: "store_a",
        pathHash: pathHash("need-2"),
        profile: "plain.json",
      },
      { id: "near-1", text: NEAR, label: "near-miss", source: "written", profile: "plain.json" },
    ];
    const tasks = path.join(dir, "tasks.jsonl");
    writeFileSync(tasks, lines.map((line) => `${JSON.stringify(line)}\n`).join(""));
    writeTasksSummary(
      tasks,
      {
        "need-1": measureItem(9, [], ["gamma", "cw-one", "cw-two"]),
        "need-2": measureItem(10),
      },
      measureItem(10)
    );
    const out = path.join(dir, "out");
    const gamma = "---\nname: gamma\ndescription: Gamma skill.\n---\n# Gamma\nNo mention here.\n";
    saveCaptures(
      out,
      [lines[0] as { id: string; text: string }],
      profileContext(subagent, cache, { texts: { ...COPY_TEXTS, gamma } })
    );
    saveCaptures(
      out,
      lines.slice(1) as { id: string; text: string }[],
      profileContext(BETA_PROFILE, cache)
    );
    // need-1's first run reads gamma (installed), its second delta (not installed), its third
    // loads alpha at once; each run that read another skill then loads alpha. need-2 loads alpha;
    // near-1 does not.
    const readOf = (target: string, id: string): ReplyBlock[] => [
      { type: "tool_use", id, name: "read", input: { path: target } },
    ];
    let first = 0;
    answer = (body) => {
      const sent = JSON.stringify(body.messages);
      if (sent.includes(NEED_2)) return READ_ALPHA;
      if (!sent.includes(NEED)) return TEXT;
      if (sent.includes("# Gamma") || sent.includes("Unknown skill: delta")) return READ_ALPHA;
      first += 1;
      return first === 1
        ? readOf("skill://gamma", "toolu_gamma")
        : first === 2
          ? readOf("skill://delta", "toolu_delta")
          : READ_ALPHA;
    };
    const from = recorder.requests.length;
    const ran = await cli([
      ...["load", "--config", made.config, "--key-file", made.keyFile, "--skill", skill],
      ...["--tasks", tasks, "--host", "omp", "--out", out],
      ...["--omp", path.join(dir, "no-omp-runs-here"), "--scratch", path.join(dir, "scratch")],
      ...["--cache-dir", cache],
    ]);
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const summary = z
      .looseObject({
        tasks: z.array(z.looseObject({ id: z.string(), made: z.number(), dropped: z.unknown() })),
        outsideRecordedListing: z.unknown(),
      })
      .parse(JSON.parse(readFileSync(path.join(out, "summary.json"), "utf8")));
    expect(summary.tasks.map((task) => [task.id, task.made, task.dropped])).toEqual([
      ["need-1", 3, null],
      ["need-2", 3, null],
      ["near-1", 3, null],
    ]);
    expect(sentWith(from, NEED)).toBeGreaterThanOrEqual(3);
    expect(summary.outsideRecordedListing).toEqual({
      tasks: 1,
      runs: 3,
      examined: 3,
      outside: 2,
      bySkill: { delta: { copy: 0, absent: 1 }, gamma: { copy: 1, absent: 0 } },
    });
    expect(ran.stdout).toContain(
      "outside recorded listing: 2 of 3 runs of 1 recorded-prompt task (the capture sends the session's recorded system prompt whole) read a skill the recorded listing lacks; by skill and answer (copy: an installed copy; absent: the unknown-skill reply): delta (0 copy, 1 absent), gamma (1 copy, 0 absent)"
    );
  });

  // Claude Code's capture lists the skills its plugins hold and sends no recorded prompt, so a
  // task whose measure says `session listing` is still a task whose listing is built from its
  // profile: both drops apply.
  test("a Claude Code task whose measure says session listing is dropped for adding over 10%, as a profile-built listing is", async () => {
    const dir = path.join(scratch, "listing-claude-adds");
    const made = setup(dir);
    const skill = path.join(dir, "alpha");
    writeTree(skill, { "SKILL.md": ALPHA_SKILL, "references/x.md": ALPHA_X });
    const tasks = path.join(dir, "tasks.jsonl");
    writeFileSync(
      tasks,
      `${JSON.stringify({ id: "need-1", text: NEED, label: "need", source: "session", store: "store_a", pathHash: pathHash("need-1") })}\n`
    );
    writeTree(dir, { "profile.json": JSON.stringify(BETA_CLAUDE_PROFILE) });
    writeTasksSummary(
      tasks,
      { "need-1": measureItem(10, [], ["cw-one", "cw-two"]) },
      measureItem(10)
    );
    const out = path.join(dir, "out");
    const from = recorder.requests.length;
    const ran = await cli([
      ...["load", "--config", made.config, "--key-file", made.keyFile, "--skill", skill],
      ...["--tasks", tasks, "--host", "claude", "--profile", path.join(dir, "profile.json")],
      ...[
        "--out",
        out,
        "--scratch",
        path.join(dir, "scratch"),
        "--cache-dir",
        path.join(dir, "cache"),
      ],
    ]);
    expect(ran.code).toBe(3);
    expect(ran.stderr).toContain("the listing rule drops 1 of the 1 tasks");
    expect(recorder.requests.length).toBe(from);
    const [task] = summaryOf(out).tasks;
    expect(task?.dropped).toBe(
      "listing-adds: its rebuilt listing adds 2 to the 10 names of the recorded session listing (20.0%), more than 10%: cw-one, cw-two"
    );
  });

  test("the verdict is made with and without the tasks whose listing could not be measured; where the two agree it stands, with their count", async () => {
    const { tasks, out, args } = listingTest(
      "listing-agree",
      [
        { id: "need-1", text: NEED, label: "need" },
        { id: "near-1", text: NEAR, label: "near-miss", written: true },
      ],
      new Map([
        [NEED, 2],
        [NEAR, 0],
      ])
    );
    writeTasksSummary(tasks, { "need-1": measureItem(10) }, measureItem(null));
    const ran = await cli(args);
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const reason =
      "no description fault in 2 tasks; the listing of 1 of its 2 tasks could not be measured, and without them the verdict is the same";
    expect(ran.stdout).toContain(`the skill alpha: ${reason}`);
    const summary = summaryOf(out);
    expect(summary.verdict).toEqual({ descriptionFault: false, reason });
    expect(summary.verdicts).toEqual({
      withUnmeasured: { descriptionFault: false, reason: "no description fault in 2 tasks" },
      withoutUnmeasured: { descriptionFault: false, reason: "no description fault in 1 tasks" },
    });
  });

  test("where the verdicts with and without the tasks whose listing could not be measured differ, there is no verdict, and the summary holds both", async () => {
    const { tasks, out, args } = listingTest(
      "listing-differ",
      [
        { id: "need-1", text: NEED, label: "need" },
        { id: "near-1", text: NEAR, label: "near-miss", written: true },
      ],
      new Map([
        [NEED, 2],
        [NEAR, 2],
      ])
    );
    writeTasksSummary(tasks, { "need-1": measureItem(10) }, measureItem(null));
    const ran = await cli(args);
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const reason =
      "no verdict: depends on tasks whose listing could not be measured (1 of its 2 tasks): with them, a description fault: tasks near-1; without them, no description fault in 1 tasks";
    expect(ran.stdout).toContain(`the skill alpha: ${reason}`);
    const summary = summaryOf(out);
    expect(summary.verdict).toEqual({ descriptionFault: null, reason });
    expect(summary.verdicts).toEqual({
      withUnmeasured: { descriptionFault: true, reason: "a description fault: tasks near-1" },
      withoutUnmeasured: { descriptionFault: false, reason: "no description fault in 1 tasks" },
    });
  });

  test("where the listing rule drops most tasks of a load, the command stops before any run and says so with the numbers (exit 3)", async () => {
    const { tasks, out, args } = listingTest(
      "listing-most",
      [
        { id: "need-1", text: NEED, label: "need" },
        { id: "near-1", text: NEAR, label: "near-miss", written: true },
        { id: "need-2", text: NEED_2, label: "need" },
      ],
      new Map()
    );
    writeTasksSummary(
      tasks,
      { "need-1": measureItem(10, ["beta", "gamma", "delta"]), "need-2": measureItem(10) },
      measureItem(10, ["beta", "gamma"])
    );
    const from = recorder.requests.length;
    const ran = await cli(args);
    expect(ran.code).toBe(3);
    expect(ran.stderr).toContain(
      `skill-cuts: stopped: the listing rule drops 2 of the 3 tasks of ${tasks}, more than half (need-1, near-1): the rebuilt listing of each misses more than 10% of the names its session's recorded listing holds or, where its capture builds its listing from the rebuilt profile, adds more names than 10% of them, so the test stops before any run, and the architect decides such a load; 1 tasks' listings were measured and 0 could not be`
    );
    expect(recorder.requests.length).toBe(from);
    const tasksOf = summaryOf(out).tasks;
    expect(tasksOf.map((task) => [task.id, task.made, task.dropped !== null])).toEqual([
      ["need-1", 0, true],
      ["near-1", 0, true],
      ["need-2", 0, false],
    ]);
  });

  // The architect, 2026-11-22T07:48:46Z: count, per report, the cases resting on the assumption
  // that their session had Dispatch, beside the listing measure.
  test("the summary and the listing rule's line count, beside the listing, the tasks that carry the Envoy plugin's block and those that rest on the assumption that their session had Dispatch", async () => {
    const dir = path.join(scratch, "dispatch-count");
    const made = setup(dir);
    const skill = path.join(dir, "dispatch-first");
    writeTree(skill, { "SKILL.md": DISPATCH_FIRST_SKILL });
    // One task in a dev box's context, one in a Legion role's: each profile installs the plugin.
    const lines = [
      { id: "box-1", text: NEED, label: "need", source: "written", profile: "box.json" },
      { id: "pod-1", text: NEAR, label: "near-miss", source: "written", profile: "pod.json" },
    ];
    const tasks = path.join(dir, "tasks.jsonl");
    writeFileSync(tasks, lines.map((line) => `${JSON.stringify(line)}\n`).join(""));
    const withEnvoy = { ...BETA_PROFILE, plugins: [ENVOY_ENTRY] };
    writeFileSync(path.join(dir, "box.json"), JSON.stringify(withEnvoy));
    writeFileSync(
      path.join(dir, "pod.json"),
      JSON.stringify({ ...withEnvoy, context: "legion:acme:worker" })
    );
    const out = path.join(dir, "out");
    const cache = path.join(dir, "cache");
    saveCaptures(out, lines, profileContext(withEnvoy, cache, { tested: "dispatch-first" }), {
      listing: [{ name: "dispatch-first", hint: "Use to search the made-up tracker first." }],
      injections: [{ by: "plugin", plugin: "pi-envoy", name: "dispatch-first", at: 0 }],
    });
    answer = () => TEXT;
    const ran = await cli([
      ...["load", "--config", made.config, "--key-file", made.keyFile, "--skill", skill],
      ...["--tasks", tasks, "--host", "omp", "--out", out],
      ...["--omp", path.join(dir, "no-omp-runs-here"), "--scratch", path.join(dir, "scratch")],
      ...["--cache-dir", cache],
    ]);
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    expect(ran.stdout).toContain(
      "tasks dropped (0 listing-misses, 0 listing-adds), each missing more than 10% of the names its session's recorded listing holds or, where its capture builds its listing from the rebuilt profile, adding more names than 10% of them, 0 installed names their recorded listing gave them (fromRecorded); 2 tasks carry the Envoy plugin's block of the skill, 1 of them on the assumption that their session had Dispatch"
    );
    const summary = z
      .looseObject({ listing: z.unknown(), dispatch: z.unknown() })
      .parse(JSON.parse(readFileSync(path.join(out, "summary.json"), "utf8")));
    // No transcript count: a load test has no transcript messages for arms.ts's `dispatchOf`.
    expect(summary.dispatch).toEqual({ carryBlock: 2, assumed: 1, legionRole: 1 });
  });
});

// Catches: a Legion task's saved capture, whose role prompt was composed at the plugin's commit,
// read back as it is after the task's evidence came to name another commit its daemon image was
// built from, so its runs go under a role prompt the pod's daemon never sent.
test("a saved capture whose role prompt was composed at another commit than the task's evidence names as its daemon image's is refused (exit 2) before any run, naming both; one composed at that commit is read back and run", async () => {
  const dir = path.join(scratch, "saved-role-commit");
  const made = setup(dir);
  const skill = path.join(dir, "alpha");
  writeTree(skill, { "SKILL.md": ALPHA_SKILL, "references/x.md": ALPHA_X });
  const lines = [{ id: "need-1", text: NEED, label: "need", source: "written" }];
  const tasks = path.join(dir, "tasks.jsonl");
  writeFileSync(tasks, lines.map((line) => `${JSON.stringify(line)}\n`).join(""));
  const profile = path.join(dir, "profile.json");
  writeFileSync(profile, JSON.stringify(LEGION_BETA_PROFILE));
  writeTasksSummary(tasks, {}, measureItem(null), [DAEMON_ITEM]);
  const cache = path.join(dir, "cache");
  const copies = profileContext(LEGION_BETA_PROFILE, cache);
  /** A capture record's context whose role prompt Compose ran at `commit` of acme/legion. */
  const recordAt = (commit: string, commitOf?: "daemon image" | "plugin") => ({
    ...copies,
    rolePrompt: {
      repo: "acme/legion",
      commit,
      ...(commitOf === undefined ? {} : { commitOf }),
      role: "implementer",
      isRoot: false,
      promptsBlob: "made-up",
      key: "made-up",
      parts: ["packages/daemon/internal/prompts/roles/worker.md"],
    },
  });
  const out = path.join(dir, "out");
  const record = loadCapturePaths(out, "omp", "need-1").record;
  answer = () => TEXT;
  const args = [
    ...["load", "--config", made.config, "--key-file", made.keyFile, "--skill", skill],
    ...["--tasks", tasks, "--host", "omp", "--profile", profile, "--out", out, "--runs", "1"],
    ...["--omp", path.join(dir, "no-omp-runs-here"), "--scratch", path.join(dir, "scratch")],
    ...["--cache-dir", cache],
  ];
  const from = recorder.requests.length;
  // Composed at the plugin's commit: so recorded, and as a record made before `commitOf` was.
  for (const commitOf of ["plugin", undefined] as const) {
    saveCaptures(out, lines, recordAt(COMMIT, commitOf));
    const refused = await cli(args);
    expect({ code: refused.code, stderr: refused.stderr }).toEqual({
      code: 2,
      stderr: `skill-cuts: ${out} holds task need-1, whose capture's role prompt was composed at the plugin's commit ${COMMIT} of acme/legion (${record}), and its evidence in the tasks file's summary now names ${DAEMON_COMMIT} as the daemon image's commit of acme/legion: give the tasks file that summary again, or use another --out\n`,
    });
  }
  expect(recorder.requests.length).toBe(from);
  expect(existsSync(loadRunFile(out, "need-1"))).toBe(false);

  // Composed at the commit the evidence names: read back, and run.
  saveCaptures(out, lines, recordAt(DAEMON_COMMIT, "daemon image"));
  const ran = await cli(args);
  expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
  expect(savedRuns(out, "need-1")).toHaveLength(1);
}, 60_000);

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

// Catches: the skill's SKILL.md read as UTF-8 with a leading byte order mark kept, so no `---` was
// found at its start and the name fell back to the directory's, where Oh My Pi drops the mark
// (`Bun.file(…).text()`) and reads the frontmatter name.
test("the skill's name is read from a SKILL.md that starts with a byte order mark, as Oh My Pi reads it", () => {
  const marked = path.join(scratch, "bom", "folder");
  writeTree(marked, {
    "SKILL.md": "\uFEFF---\nname: lookup\ndescription: Use when a lookup is due.\n---\n",
  });
  expect(skillName(marked)).toBe("lookup");
});

// Catches: Claude Code's `--version` and `claude plugin` commands run with no limit, so one that
// never exits held the load test for ever.
test("a Claude Code setup command that never exits is ended at its limit, and fails naming the command and the limit", async () => {
  const root = path.join(scratch, "claude-hangs");
  mkdirSync(root, { recursive: true });
  const claude = path.join(root, "claude");
  writeFileSync(claude, "#!/bin/sh\nsleep 5\n", { mode: 0o755 });
  const started = performance.now();
  await expect(
    mustRun([claude, "plugin", "install", "a@b"], { PATH: "/usr/bin:/bin" }, 200)
  ).rejects.toThrow(
    `\`${claude} plugin install a@b\` ran past its limit of 200 ms, so it was ended`
  );
  expect(performance.now() - started).toBeLessThan(3_000);
});

test("a profile skill with a namespace is checked under <namespace>/<name>: it may share the tested skill's name and another skill's, and Claude Code, whose layout places skills by their bare name, refuses it naming it", () => {
  const dir = path.join(scratch, "namespaced-profile");
  mkdirSync(dir, { recursive: true });
  const file = path.join(dir, "profile.json");
  const skills = [
    { repo: "acme/skills", commit: COMMIT, path: "skills/beta", name: "beta" },
    // Other repositories' alpha and beta, which Oh My Pi lists as tools/alpha and tools/beta.
    { repo: "acme/tools", commit: COMMIT, path: "skills/alpha", name: "alpha", namespace: "tools" },
    { repo: "acme/tools", commit: COMMIT, path: "skills/beta", name: "beta", namespace: "tools" },
  ];
  writeFileSync(file, JSON.stringify({ ...BETA_PROFILE, skills }));
  expect(loadProfile(file, "omp", "alpha").profile.skills).toEqual(skills);
  writeFileSync(
    file,
    JSON.stringify({
      ...BETA_PROFILE,
      skills,
      claude: {
        marketplace: "acme-load",
        plugins: [{ name: "acme-tools", skills: ["beta"] }],
        skillPlugin: "acme-tools",
      },
    })
  );
  expect(() => loadProfile(file, "claude", "alpha")).toThrow(
    "it installs tools/alpha, a second copy of alpha that Oh My Pi lists under a namespace, and a Claude Code layout places skills by their bare name; it installs tools/beta, a second copy of beta"
  );
  // A copy listed under the tested name itself is still refused.
  writeFileSync(
    file,
    JSON.stringify({ ...BETA_PROFILE, skills: [{ ...skills[1], namespace: undefined }] })
  );
  expect(() => loadProfile(file, "omp", "alpha")).toThrow(
    "it installs a skill named alpha, the name the skill is tested under"
  );
});

describe("skill-cuts load refuses or stops on its own rules", () => {
  // Catches: a second draw asked of Claude Code, which lists each skill under its description and
  // so draws no hint: its runs would be a first draw's again under another name.
  test("--hint-draw above 1 on Claude Code is refused (exit 2), naming why, and a --hint-draw that is no whole number of 1 or more is refused on either host, before any model call", async () => {
    const dir = path.join(scratch, "hint-draw-refused");
    const { common } = hostInputs(dir);
    const profile = path.join(dir, "profile.json");
    writeFileSync(profile, JSON.stringify(BETA_CLAUDE_PROFILE));
    const before = recorder.requests.length;
    const claude = await cli([
      "load",
      ...common,
      ...["--host", "claude", "--profile", profile, "--hint-draw", "2"],
    ]);
    expect({ code: claude.code, stderr: claude.stderr }).toEqual({
      code: 2,
      stderr:
        "skill-cuts: --hint-draw 2 is refused on Claude Code: it lists each skill under its description, not under a hint a model writes, so it has no hint to draw again\n",
    });
    for (const [host, draw] of [
      ["omp", "0"],
      ["omp", "1.5"],
      ["claude", "two"],
    ]) {
      const ran = await cli([
        "load",
        ...common,
        ...["--host", String(host), "--profile", profile, "--hint-draw", String(draw)],
      ]);
      expect(ran.code).toBe(2);
      expect(ran.stderr).toContain(`--hint-draw ${draw} is not a whole number of 1 or more`);
    }
    expect(recorder.requests.length).toBe(before);
  });

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

  // Catches: a project skill installed as a plugin's, so Claude Code lists it as <plugin>:<name>;
  // and a skill of the session repository at no level of the working directory, of another
  // commit, or of another repository taken for a project skill.
  test("a Claude Code layout that puts a project skill of the profile's session repository in a plugin is refused, and so is one that leaves out a skill that is no project skill", async () => {
    const dir = path.join(scratch, "claude-project-layout");
    const { common } = hostInputs(dir);
    const profile = path.join(dir, "profile.json");
    writeFileSync(
      profile,
      JSON.stringify({
        ...BETA_PROFILE,
        ...PROJ_CHECKOUT,
        skills: [
          ...BETA_PROFILE.skills,
          KAPPA,
          LAMBDA,
          { repo: "acme/proj", commit: COMMIT, path: "tools/.claude/skills/nu", name: "nu" },
          {
            repo: "acme/proj",
            commit: "fedcba9876543210fedcba9876543210fedcba98",
            path: ".claude/skills/xi",
            name: "xi",
          },
          { repo: "acme/skills", commit: COMMIT, path: ".claude/skills/omicron", name: "omicron" },
        ],
        claude: {
          marketplace: "acme-market",
          plugins: [{ name: "acme-tools", skills: ["beta", "kappa", "lambda"] }],
          skillPlugin: "acme-tools",
        },
      })
    );
    const before = recorder.requests.length;
    const ran = await cli([
      "load",
      ...common,
      ...["--host", "claude", "--profile", profile, "--claude", "/nowhere/claude"],
    ]);
    expect(ran.code).toBe(2);
    for (const fault of [
      "its layout puts kappa in a plugin, and it is a project skill of acme/proj (agents/.claude/skills/kappa), which Claude Code reads from the checkout",
      "its layout puts lambda in a plugin, and it is a project skill of acme/proj (.omp/skills/lambda), which Claude Code does not read",
      "its layout puts nu in 0 plugins, not 1",
      "its layout puts xi in 0 plugins, not 1",
      "its layout puts omicron in 0 plugins, not 1",
    ]) {
      expect(ran.stderr).toContain(fault);
    }
    expect(recorder.requests.length).toBe(before);
  });

  // Catches: a load test of a skill a plugin or an agent definition puts into the session made
  // where the test cannot give each name the block the session had, or where the copy the plugin
  // would put in is not the tested text.
  test("a skill a plugin or an agent definition puts into the session is refused where it cannot be load tested, naming the profile, before any model call", async () => {
    const dir = path.join(scratch, "injected-refusals");
    const { common } = hostInputs(dir, undefined, { "SKILL.md": DISPATCH_FIRST_SKILL });
    const before = recorder.requests.length;
    // dispatch-first on Claude Code, whose Envoy plugin puts it in from a hook no load test runs.
    const claudeProfile = path.join(dir, "claude.json");
    writeFileSync(claudeProfile, JSON.stringify(BETA_CLAUDE_PROFILE));
    const onClaude = await cli([
      "load",
      ...common,
      ...["--host", "claude", "--profile", claudeProfile, "--claude", "/nowhere/claude"],
    ]);
    expect(onClaude.code).toBe(2);
    expect(onClaude.stderr).toContain(
      "dispatch-first cannot be load tested on Claude Code: Claude Code's Envoy plugin puts it into each session with Dispatch from its session-start hook"
    );
    // dispatch-first under another name, beside a copy of its own name the plugin would read.
    const both = path.join(dir, "both.json");
    writeFileSync(
      both,
      JSON.stringify({
        ...BETA_PROFILE,
        skills: [...BETA_PROFILE.skills, DISPATCH_FIRST_ENTRY],
        plugins: [ENVOY_ENTRY],
      })
    );
    const renamed = await cli([
      "load",
      ...common,
      ...["--host", "omp", "--profile", both, "--name", "omega"],
    ]);
    expect(renamed.code).toBe(2);
    expect(renamed.stderr).toContain(
      `${both}: it installs dispatch-first beside the skill tested under omega, and the pi-envoy plugin would put that copy into each request, not the tested text`
    );

    // alpha in a subagent profile whose agent definition autoloads it.
    const agentDir = path.join(scratch, "injected-agent");
    const agent = hostInputs(agentDir);
    writeTree(path.join(agentDir, "repos", "acme", "legion", COMMIT), {
      "packages/agents-pack/agents/alpha-reviewer.md":
        "---\nname: alpha-reviewer\ndescription: Reviews with alpha.\nautoloadSkills: [alpha]\n---\n\nReview.\n",
    });
    const subagent = path.join(agentDir, "subagent.json");
    writeFileSync(
      subagent,
      JSON.stringify({
        ...BETA_PROFILE,
        plugins: [
          {
            repo: "acme/legion",
            commit: COMMIT,
            path: "packages/agents-pack",
            name: "agents-pack",
          },
        ],
        context: "subagent:alpha-reviewer",
        systemPrompt: SUBAGENT.systemPrompt,
        tools: SUBAGENT.tools,
      })
    );
    const autoloaded = await cli([
      "load",
      ...agent.common,
      ...["--host", "omp", "--profile", subagent, "--name", "omega"],
    ]);
    expect(autoloaded.code).toBe(2);
    expect(autoloaded.stderr).toContain(
      `${subagent}: its agent alpha-reviewer (packages/agents-pack/agents/alpha-reviewer.md of acme/legion at ${COMMIT}) autoloads alpha, so the subagent gets its text before its task whatever its listing says, and under the name omega it would autoload none`
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
    const cache = path.join(dir, "cache");
    saveCaptures(
      out,
      [{ id: "need-1", text: NEED }],
      profileContext(BETA_PROFILE, cache),
      ALPHA_LISTED
    );
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
        spendRecorder: null,
      }),
      hints: async () => {
        throw new Error("no hint is written here");
      },
      source: localSource(dir),
      scratch: path.join(dir, "scratch"),
      cacheDir: cache,
      hintDraw: 1,
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

// Catches: a task's total that counts the captures of the tasks after it in its profile's group.
test("a task's seconds count its own capture, its group's runs and its writes, and no later task's capture", async () => {
  const dir = path.join(scratch, "timing");
  const made = setup(dir);
  const skill = path.join(dir, "alpha");
  writeTree(skill, { "SKILL.md": ALPHA_SKILL, "references/x.md": ALPHA_X });
  const tasks = path.join(dir, "tasks.jsonl");
  const ids = ["t1", "t2"];
  writeFileSync(
    tasks,
    ids
      .map((id) => `${JSON.stringify({ id, text: NEED, label: "need", source: "written" })}\n`)
      .join("")
  );
  const profile = path.join(dir, "profile.json");
  writeFileSync(profile, JSON.stringify(BETA_PROFILE));
  const out = path.join(dir, "out");
  const cache = path.join(dir, "cache");
  saveCaptures(
    out,
    ids.map((id) => ({ id, text: NEED })),
    profileContext(BETA_PROFILE, cache),
    ALPHA_LISTED
  );
  // t2's saved body is a pipe whose writer, once t2's capture opens it, waits 1.5 s before it
  // writes, so t2's capture, read back from it, takes that long, and t1's takes none of it.
  const slow = loadCapturePaths(out, "omp", "t2").body;
  const text = `${slow}.text`;
  writeFileSync(text, readFileSync(slow));
  rmSync(slow);
  expect(Bun.spawnSync(["mkfifo", slow]).exitCode).toBe(0);
  const writer = Bun.spawn(["sh", "-c", 'exec 3>"$1"; sleep 1.5; cat "$0" >&3', text, slow]);
  answer = () => TEXT;
  const lines: string[] = [];
  await runLoad({
    io: { out: (line) => lines.push(line), err: () => {}, peakRssBytes: () => 0 },
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
      spendRecorder: null,
    }),
    hints: async () => {
      throw new Error("no hint is written here");
    },
    source: localSource(dir),
    scratch: path.join(dir, "scratch"),
    cacheDir: cache,
    hintDraw: 1,
    configFiles: [],
    claude: "claude",
    requestLog: path.join(dir, "requests.jsonl"),
    memoryLimitBytes: 2 ** 30,
    psqlCommand: "/nowhere/read-store",
    stores: ["store_a"],
  });
  await writer.exited;
  /** Task `id`'s seconds as its line gives them. */
  const seconds = (id: string) => {
    const line =
      lines
        .join("")
        .split("\n")
        .find((each) => each.includes(`task ${id} (`)) ?? "";
    const [, capture = "", other = ""] =
      /seconds: capture (\d+\.\d) .*, other (\d+\.\d), total/.exec(line) ?? [];
    return { capture: Number(capture), other: Number(other) };
  };
  expect(seconds("t2").capture).toBeGreaterThanOrEqual(1);
  expect(seconds("t1").other).toBeLessThan(0.5);
}, 60_000);

// Catches: a task whose context holds a directory no read can give counted as a failure that the
// next command tries again (each time from the whole repository's tarball, before 2026-11-22).
test("a task whose context needs a directory GitHub cuts short even read alone is dropped with its reason in its line and the summary, with no tarball read, and a command started again reads nothing for it", async () => {
  const dir = path.join(scratch, "unreadable");
  const made = setup(dir);
  const skill = path.join(dir, "alpha");
  writeTree(skill, { "SKILL.md": ALPHA_SKILL, "references/x.md": ALPHA_X });
  const tasks = path.join(dir, "tasks.jsonl");
  writeFileSync(
    tasks,
    [
      { id: "need-1", text: NEED, label: "need", source: "written" },
      { id: "near-1", text: NEAR, label: "near-miss", source: "written" },
    ]
      .map((task) => `${JSON.stringify(task)}\n`)
      .join("")
  );
  const profile = path.join(dir, "profile.json");
  writeFileSync(profile, JSON.stringify(BETA_PROFILE));
  // acme/skills at COMMIT, as GitHub lists a repository of too many entries: its recursive tree
  // cut short, then skills/beta's, and skills/beta's one level even read alone.
  const gh = path.join(dir, "gh");
  const log = path.join(dir, "gh-calls.jsonl");
  writeGhStandIn(gh, log, truncatedSkillsTree(COMMIT));
  // An Oh My Pi that answers --version: the context is built before it runs any capture.
  const ompStandIn = path.join(dir, "omp");
  writeFileSync(ompStandIn, "#!/bin/sh\necho 'omp 0.0.0 (stand-in)'\n", { mode: 0o755 });
  const models = path.join(dir, "models.yml");
  writeFileSync(models, ANTHROPIC_MODELS);
  const out = path.join(dir, "out");
  const load = async () => {
    const lines: string[] = [];
    const result = await runLoad({
      io: {
        out: (line) => lines.push(line),
        err: (line) => lines.push(line),
        peakRssBytes: () => 0,
      },
      host: "omp",
      skillDir: skill,
      skillDigest: "a-made-up-digest",
      tasksFile: tasks,
      profileFile: profile,
      runs: 3,
      out,
      model: LOAD_MODEL,
      thinking: "off",
      gateway: createGateway({
        baseUrl: recorder.url,
        keyFile: made.keyFile,
        concurrency: 2,
        timeoutMs: 60_000,
        spendRecorder: null,
      }),
      hints: async () => {
        throw new Error("no hint is written here");
      },
      source: ghSource(gh, { sleep: async () => {}, err: () => {} }),
      scratch: path.join(dir, "scratch"),
      cacheDir: path.join(dir, "cache"),
      omp: ompStandIn,
      models,
      hintDraw: 1,
      configFiles: [],
      claude: "claude",
      requestLog: path.join(dir, "requests.jsonl"),
      memoryLimitBytes: 2 ** 30,
      psqlCommand: "/nowhere/read-store",
      stores: ["store_a"],
    });
    return { result, said: lines.join("") };
  };
  const routes = () => readFileSync(log, "utf8").split("\n").filter(Boolean);
  const summary = () =>
    z
      .object({
        tasks: z.array(
          z.looseObject({ id: z.string(), made: z.number(), dropped: z.string().nullable() })
        ),
        verdict: z.looseObject({ reason: z.string() }),
      })
      .parse(JSON.parse(readFileSync(path.join(out, "summary.json"), "utf8")));
  const dropped = `unreadable-directory: acme/skills at ${COMMIT}: skills/beta cannot be read: GitHub cuts the tree of skills/beta short even one level at a time`;

  const before = recorder.requests.length;
  const first = await load();
  expect(first.result.failures).toEqual([]);
  expect(first.said).toContain(`load: task need-1 (need, written): dropped: ${dropped}\n`);
  expect(first.said).toContain(`load: task near-1 (near-miss, written): dropped: ${dropped}\n`);
  expect(summary().tasks.map(({ id, made, dropped }) => ({ id, made, dropped }))).toEqual([
    { id: "need-1", made: 0, dropped },
    { id: "near-1", made: 0, dropped },
  ]);
  expect(summary().verdict.reason).toBe("no verdict yet: tasks need-1, near-1 lack runs");
  expect(routes().filter((route) => route.includes("/tarball/"))).toEqual([]);
  // The commit's tree once; for the first task the root, skills/ and skills/beta twice (its tree
  // and its one level); for the second only skills/beta's tree, the levels kept from the first.
  expect(routes()).toHaveLength(6);
  expect(recorder.requests.length).toBe(before);

  const again = await load();
  expect(again.result.failures).toEqual([]);
  expect(again.said).toContain(
    `load: task need-1 (need, written): dropped before, skipped: ${dropped}\n`
  );
  expect(again.said).toContain(
    `load: task near-1 (near-miss, written): dropped before, skipped: ${dropped}\n`
  );
  expect(routes()).toHaveLength(6);
  expect(summary().tasks.map((task) => task.dropped)).toEqual([dropped, dropped]);
  expect(recorder.requests.length).toBe(before);
}, 60_000);

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
    const cache = path.join(dir, "cache");
    saveCaptures(
      out,
      ids.map((id, index) => ({ id, text: texts3[index] })),
      profileContext(BETA_PROFILE, cache),
      ALPHA_LISTED
    );
    args = [
      "load",
      ...["--config", made.config, "--key-file", made.keyFile, "--skill", skill],
      ...["--tasks", tasks, "--host", "omp", "--profile", profile, "--out", out],
      ...["--scratch", path.join(dir, "scratch"), "--cache-dir", cache],
    ];
  });

  // Catches: a task's runs sent as a batch of their own, or runs that share the captured id.
  test("the runs of every task of a profile go in the seeded order, each with a session id no other run carries", async () => {
    answer = () => TEXT;
    const before = recorder.requests.length;
    // One call at a time, so the stand-in takes the runs in the order they were sent.
    const ran = await cli([...args, "--concurrency", "1"]);
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
    // The stand-in took the runs in the seed's order, the tasks' runs interleaved as it says. The
    // seed is drawn fresh each run, and 6 of the 1,680 orders of three tasks' three runs (0.36%)
    // send each task's runs one after another: under such a seed, code that sends each task's runs
    // as a batch of their own, in that order of the tasks, passes this check.
    const taskOf = new Map(runs.map((run) => [run.userId, run.task]));
    expect(userIds.map((id) => taskOf.get(id))).toEqual(order.map((each) => each.key));
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

// Catches: load runs sent with no routing id, so each task's body went to the one deployment its
// hash picked; a routing id drawn at random, which the two loads of a name or description test
// (one command each) cannot share; a run that does not record its routing id and the deployment
// that answered it; and a summary that does not count the runs by deployment.
test("run n of a task goes with the routing id derived from --route-seed, the task and n, the same in the loads of two texts; each run records it and the deployment that answered, and the summary and its line count the runs by deployment under the tested name", async () => {
  const dir = path.join(scratch, "routing");
  /** The routing id of run `run` of task `task` under `seed`, as the rule derives it: the first
   *  32 hex digits of the SHA-256 of `<seed>:<task>:<run>`, in a UUID's 8-4-4-4-12 groups. */
  const routingIdOf = (seed: string, task: string, run: number) => {
    const hex = createHash("sha256").update(`${seed}:${task}:${run}`).digest("hex");
    return [
      hex.slice(0, 8),
      hex.slice(8, 12),
      hex.slice(12, 16),
      hex.slice(16, 20),
      hex.slice(20, 32),
    ].join("-");
  };
  const made = setup(dir);
  const ids = ["t1", "t2"];
  const texts2 = ["Write the alpha checklist, one.", "Write the beta greeting, two."];
  const tasks = path.join(dir, "tasks.jsonl");
  writeFileSync(
    tasks,
    ids
      .map((id, index) =>
        JSON.stringify({
          id,
          text: texts2[index],
          label: index === 0 ? "need" : "near-miss",
          source: "written",
        })
      )
      .map((line) => `${line}\n`)
      .join("")
  );
  const profile = path.join(dir, "profile.json");
  writeFileSync(profile, JSON.stringify(BETA_PROFILE));
  const cache = path.join(dir, "cache");
  /** One load of alpha's text `text` under `dir/<name>`, with `extra` arguments. */
  const load = async (name: string, text: string, ...extra: string[]) => {
    const skill = path.join(dir, name, "alpha");
    writeTree(skill, { "SKILL.md": text, "references/x.md": ALPHA_X });
    const out = path.join(dir, name, "out");
    saveCaptures(
      out,
      ids.map((id, index) => ({ id, text: texts2[index] ?? "" })),
      profileContext(BETA_PROFILE, cache),
      ALPHA_LISTED
    );
    const before = recorder.requests.length;
    const ran = await cli([
      "load",
      ...["--config", made.config, "--key-file", made.keyFile, "--skill", skill],
      ...["--tasks", tasks, "--host", "omp", "--profile", profile, "--out", out],
      ...["--scratch", path.join(dir, name, "scratch"), "--cache-dir", cache],
      ...extra,
    ]);
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const runs = ids.flatMap((id) => savedRuns(out, id));
    const sent = recorder.requests.slice(before);
    return { out, ran, runs, sent };
  };
  /** Checks that each run of `loaded` went with the routing id of `seed`, its task and its run
   *  number on its request, and records it and the deployment that id pins. */
  const routedBy = (
    loaded: { readonly runs: readonly LoadRun[]; readonly sent: readonly RecordedRequest[] },
    seed: string
  ) => {
    expect(loaded.runs).toHaveLength(6);
    expect(loaded.sent).toHaveLength(6);
    for (const run of loaded.runs) {
      const id = routingIdOf(seed, run.task, run.run);
      expect(run.routingId).toBe(id);
      expect(run.routingId).toMatch(
        /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/
      );
      const request = loaded.sent.filter(
        (each) => JSON.parse(each.body).metadata.user_id === run.userId
      );
      expect(request.map((each) => each.headers["x-litellm-session-id"])).toEqual([id]);
      expect(run.deployments).toEqual([pinnedTo(id)]);
    }
  };
  // A load takes no pin: run n of a task goes with its derived id only.
  const pinned = await cli(["load", "--route-id", "old=5eed0000-0000-4000-8000-000000000002"]);
  expect({ code: pinned.code, refused: pinned.stderr.includes("--route-id") }).toEqual({
    code: 2,
    refused: true,
  });
  answer = () => TEXT;
  pinDeployments = true;
  try {
    const first = await load("first", ALPHA_SKILL);
    routedBy(first, "");
    const second = await load(
      "second",
      ALPHA_SKILL.replace("description: ", "description: A made-up other description. ")
    );
    routedBy(second, "");
    const seeded = await load("seeded", ALPHA_SKILL, "--route-seed", "made-up-seed");
    routedBy(seeded, "made-up-seed");
    // The two texts' loads sent run n of each task with one id; another seed, with others.
    const routes = (runs: readonly LoadRun[]) =>
      runs.map((run) => [run.task, run.run, run.routingId]);
    expect(routes(second.runs)).toEqual(routes(first.runs));
    expect(new Set(first.runs.map((run) => run.routingId)).size).toBe(6);
    const seededIds = new Set(seeded.runs.map((run) => run.routingId));
    expect(first.runs.filter((run) => seededIds.has(run.routingId))).toEqual([]);

    const counts = new Map<string, number>();
    for (const run of first.runs) {
      const prefix = pinnedTo(run.routingId ?? "");
      counts.set(prefix, (counts.get(prefix) ?? 0) + 1);
    }
    const sorted = [...counts].sort(([a], [b]) => (a < b ? -1 : 1));
    const summary = JSON.parse(readFileSync(path.join(first.out, "summary.json"), "utf8"));
    expect(summary.routeSeed).toBe("");
    expect(summary.deployments).toEqual({
      byName: { alpha: Object.fromEntries(sorted) },
      unrecorded: 0,
    });
    expect(seeded.ran.stdout).toContain("load: deployments by name: alpha ");
    expect(first.ran.stdout).toContain(
      `load: deployments by name: alpha ${sorted.map(([prefix, n]) => `${prefix} ${n}`).join(", ")}\n`
    );
    // A load's runs made before runs recorded their deployments are said so.
    for (const id of ids) {
      const saved = savedRuns(first.out, id).map(
        ({ routingId: _r, deployments: _d, ...run }) => run
      );
      writeFileSync(
        loadRunFile(first.out, id),
        saved.map((run) => `${JSON.stringify(run)}\n`).join("")
      );
    }
    const again = await load("first", ALPHA_SKILL);
    expect(again.sent).toEqual([]);
    expect(again.ran.stdout).toContain("load: deployments by name: alpha not recorded\n");
  } finally {
    pinDeployments = false;
  }
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

// Catches: a parent's CLAUDE.md, or another mention of the context's checkout, left naming the
// test's scratch when the working directory is below the checkout's root.
test("Claude Code's harness rules name the session's checkout for a mention of the context's checkout when the working directory is below its root", () => {
  const checkout = "/scratch/load-1/claude/profiles/abc/context/work";
  const work = `${checkout}/agents/vm`;
  const facts = sessionFacts(
    {
      file: "/made-up/sessions/h.json",
      head: {
        store: "store_a",
        pathHash: pathHash("/sessions/h.jsonl"),
        held: true,
        cwd: "/srv/acme/widgets/agents/vm",
        start: "2026-11-18T08:00:00.000Z",
        firstUser: null,
        reminders: [],
        bytes: 100,
        badLines: 0,
        readAt: "2026-11-21T00:00:00.000Z",
      },
    },
    {
      turn: undefined,
      date: { value: "2026-11-18", from: "the UTC date of the session's first user turn" },
      devbox: false,
      devboxClaudeConfigDir: undefined,
    }
  );
  const body = {
    model: "claude-sonnet-5-5",
    system: [
      {
        type: "text",
        text: `Contents of ${checkout}/CLAUDE.md (project instructions, checked into the codebase):\nRoot rules.\nContents of ${work}/CLAUDE.md (project instructions, checked into the codebase):\nVM rules.`,
      },
    ],
    messages: [
      {
        role: "user",
        content: [{ type: "text", text: ` - Primary working directory: ${work}\n` }],
      },
    ],
  };
  const rewritten = rewriteBody(
    body,
    claudeHarnessRules(work, "", facts, { dir: checkout, workingDirectory: "agents/vm" }),
    ["/scratch/load-1"]
  );
  expect(rewritten.faults).toEqual([]);
  const text = JSON.stringify(rewritten.body);
  expect(text).toContain("Contents of /srv/acme/widgets/CLAUDE.md (project instructions");
  expect(text).toContain("Contents of /srv/acme/widgets/agents/vm/CLAUDE.md (project instructions");
  expect(text).toContain("Primary working directory: /srv/acme/widgets/agents/vm\\n");
  expect(rewritten.replaced).toContainEqual({ what: "the checkout", count: 1 });
});

// Catches: a session checkout at the file system's root joined to a path as `//`.
test("Claude Code's harness rules name a session checkout at the file system's root with one slash", () => {
  const checkout = "/scratch/load-1/claude/profiles/abc/context/work";
  const work = `${checkout}/agents`;
  // No session is named: the working directory is the neutral one, one level below `/`.
  const facts = sessionFacts(undefined, {
    turn: undefined,
    date: { value: "2026-11-18", from: "the UTC date of the session's first user turn" },
    devbox: false,
    devboxClaudeConfigDir: undefined,
  });
  const body = {
    model: "claude-sonnet-5-5",
    system: [{ type: "text", text: `Contents of ${checkout}/CLAUDE.md (project instructions)` }],
    messages: [{ role: "user", content: [{ type: "text", text: `The root is ${checkout}.` }] }],
  };
  const rewritten = rewriteBody(
    body,
    claudeHarnessRules(work, "", facts, { dir: checkout, workingDirectory: "agents" }),
    ["/scratch/load-1"]
  );
  expect(rewritten.faults).toEqual([]);
  const text = JSON.stringify(rewritten.body);
  expect(text).toContain("Contents of /CLAUDE.md (project instructions)");
  expect(text).toContain("The root is /.");
});

/** A made-up acme/skills (the skills alpha, beta and gamma, and Claude Code's user memory at
 *  .claude/CLAUDE.md), the tested skill's `tested` files (alpha's unless given) and the tasks
 *  `lines` (one need task unless given) for a real host's run, under `dir`, with the stand-in's
 *  configuration. */
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

// Catches: an observed copy treated as setup, a labelled task treated as observed, or a load of
// another inventory copy under the task's setup label.
test("load refuses to use setup tasks without the matching flag or to add an observed copy twice", async () => {
  const dir = path.join(scratch, "test-setup-refusals");
  const task = { id: "need-1", text: NEED, label: "need", source: "written" };
  const copy = { repo: "acme/skills", commit: COMMIT, path: "skills/alpha", name: "alpha" };
  const { common } = hostInputs(dir, [task]);
  const tasks = path.join(dir, "tasks.jsonl");
  const profile = path.join(dir, "profile.json");
  writeFileSync(
    profile,
    JSON.stringify({ ...BETA_PROFILE, skills: [...BETA_PROFILE.skills, copy] })
  );
  const args = ["load", ...common, "--host", "omp", "--profile", profile];
  const before = recorder.requests.length;
  const spec = `${copy.repo}@${copy.commit}:${copy.path}`;
  const duplicate = await cli([...args, "--test-setup", spec]);
  expect(duplicate.code).toBe(2);
  expect(duplicate.stderr).toContain(`${spec}: ${profile} already installs that copy`);
  const missingSpec = `${copy.repo}@${copy.commit}:skills/missing`;
  const missing = await cli([...args, "--test-setup", missingSpec]);
  expect(missing.code).toBe(2);
  expect(missing.stderr).toContain(`${missingSpec}: cannot read the copy`);
  const label = "test setup, not observed use";
  writeFileSync(tasks, `${JSON.stringify({ ...task, testSetup: { label, copy } })}\n`);
  const without = await cli(args);
  expect(without.code).toBe(2);
  expect(without.stderr).toContain(
    `tasks need-1 record "${label}"; give the matching --test-setup`
  );
  const other = await cli([...args, "--test-setup", missingSpec]);
  expect(other.code).toBe(2);
  expect(other.stderr).toContain(`with ${spec}, not --test-setup ${missingSpec}`);
  expect(recorder.requests.length).toBe(before);
});

/** Builds the Envoy plugin of this checkout into the made-up acme/legion at COMMIT under `repos`,
 *  as the worker image ships it: its bundle in `dist/`, and a manifest that names the bundle and
 *  `dist/skills`, where it reads dispatch-first as it loads. */
function writeEnvoyPlugin(repos: string): void {
  const dest = path.join(repos, "acme/legion", COMMIT, "packages/pi-envoy");
  const externals = ["pi-ai", "pi-coding-agent", "pi-tui", "pi-utils"];
  const built = Bun.spawnSync(
    [
      ...["bun", "build", "extensions/envoy.ts", "--outdir", path.join(dest, "dist")],
      ...["--target", "bun", "--format", "esm"],
      ...externals.flatMap((name) => ["--external", `@oh-my-pi/${name}`]),
    ],
    { cwd: path.resolve(import.meta.dir, "../../../pi-envoy"), stderr: "pipe" }
  );
  expect({
    code: built.exitCode,
    stderr: built.exitCode === 0 ? "" : built.stderr.toString(),
  }).toEqual({ code: 0, stderr: "" });
  writeTree(dest, {
    "package.json": JSON.stringify({
      name: "@acme/pi-envoy",
      files: ["dist"],
      omp: { extensions: ["dist/envoy.js"], skills: ["dist/skills"] },
    }),
  });
}

/** The role prompt composer of a made-up Legion daemon, with the API context.ts runs
 *  (`New(stateDir)`, `Compose(role, isRoot)`, the embedded `roles/` parts written under
 *  prompts/shared): every role's one part is `roles/worker.md`. */
const DAEMON_PROMPTS_GO = `package prompts

import (
	"embed"
	"os"
	"path/filepath"

	"example.com/acme/daemon/internal/claim"
)

//go:embed roles
var roleParts embed.FS

type Parts struct{ RolePromptPaths []string }

type Composer struct{ sharedDir string }

func New(stateDir string) (*Composer, error) {
	shared := filepath.Join(stateDir, "prompts", "shared")
	body, err := roleParts.ReadFile("roles/worker.md")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(shared, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(shared, "worker.md"), body, 0o600); err != nil {
		return nil, err
	}
	return &Composer{sharedDir: shared}, nil
}

func (c *Composer) Compose(role claim.Role, isRoot bool) (Parts, error) {
	return Parts{RolePromptPaths: []string{filepath.Join(c.sharedDir, "worker.md")}}, nil
}
`;

/** The made-up daemon module of acme/legion, its one role part holding `part`. */
const daemonFiles = (part: string): Record<string, string> => ({
  "packages/daemon/go.mod": "module example.com/acme/daemon\n\ngo 1.22\n",
  "packages/daemon/internal/claim/claim.go": "package claim\n\ntype Role string\n",
  "packages/daemon/internal/prompts/prompts.go": DAEMON_PROMPTS_GO,
  "packages/daemon/internal/prompts/roles/worker.md": `${part}\n`,
});

describe.skipIf(omp === undefined)("skill-cuts load on the pinned Oh My Pi", () => {
  // Catches: a setup run reading --skill instead of the inventory commit, installing the staged
  // copy twice, changing a companion's listing, borrowing a recorded hint on either draw, or
  // printing a verdict that reads as observed use.
  test(
    "load of a staged or unstaged profile installs the same inventory copy and draws its tested name on both draws",
    async () => {
      const dir = path.join(scratch, "omp-test-setup");
      const task = { id: "need-1", text: NEED, label: "need", source: "written" };
      const copy = { repo: "acme/skills", commit: COMMIT, path: "skills/alpha", name: "alpha" };
      const setup = { label: "test setup, not observed use", copy };
      const { out, common } = hostInputs(dir, [task], {
        "SKILL.md": ALPHA_SKILL.replace("the made-up project.", "a different local project."),
      });
      writeTasksSummary(path.join(dir, "tasks.jsonl"), {}, measureItem(2));
      const spec = `${copy.repo}@${copy.commit}:${copy.path}`;
      const models = path.join(dir, "models.yml");
      writeFileSync(models, ANTHROPIC_MODELS);
      const betaLine = "Use for the recorded beta greeting.";
      const baseline = {
        ...BETA_PROFILE,
        recordedListing: {
          source: { kind: "session" },
          // Even a nonempty, matching description for a pre-existing line must not be reused.
          tested: copy,
          lines: [
            { name: "alpha", text: "Do not reuse this alpha line." },
            { name: "beta", text: betaLine },
          ],
        },
      };
      answer = () => READ_ALPHA;
      const cache = path.join(dir, "cache");
      const description = "Use when a task asks for the alpha checklist of the made-up project.";
      for (const draw of [1, 2]) {
        const before = recorder.requests.length;
        const hint = draw === 1 ? "Use for the alpha work, stand-in hint." : "Alpha draw two.";
        if (draw === 2) {
          // Seed a distinct draw-2 hint under the key the normal second draw uses.
          const version = Bun.spawnSync([String(omp), "--version"])
            .stdout.toString()
            .trim();
          const draw1 = cacheKey(version, copy.name, description);
          const draw2 = cacheKey(version, copy.name, description, "draw 2");
          const first = JSON.parse(
            readFileSync(path.join(cache, "hints", `${draw1}.json`), "utf8")
          );
          writeFileSync(
            path.join(cache, "hints", `${draw2}.json`),
            JSON.stringify({ ...first, hint, draw })
          );
        }
        for (const staged of [false, true]) {
          const at = `${out}-${draw}-${staged}`;
          const file = path.join(dir, `profile-${draw}-${staged}.json`);
          const profile = {
            ...baseline,
            ...(staged ? { skills: [...baseline.skills, copy] } : {}),
            ...(draw === 2
              ? {
                  context: "subagent:task",
                  systemPrompt: [
                    "A recorded project prompt.",
                    `<skills>\n- beta: ${betaLine}\n</skills>`,
                  ],
                  tools: ["read"],
                }
              : {}),
          };
          const raw = JSON.stringify(profile);
          writeFileSync(file, raw);
          writeFileSync(
            path.join(dir, "tasks.jsonl"),
            `${JSON.stringify(staged ? { ...task, testSetup: setup } : task)}\n`
          );
          const ran = await cli([
            "load",
            // hostInputs asks for one run. Use the registered count so the report gives a verdict.
            ...common.map((arg) => (arg === out ? at : arg === "1" ? "3" : arg)),
            ...["--host", "omp", "--profile", file, "--omp", String(omp), "--models", models],
            ...["--test-setup", spec, "--hint-draw", String(draw)],
          ]);
          expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
          const capture = loadCapturePaths(at, "omp", task.id);
          const body = JSON.parse(readFileSync(capture.body, "utf8"));
          expect(Object.fromEntries(skillListing(body))).toEqual({
            alpha: hint,
            beta: betaLine,
          });
          if (draw === 2) {
            expect(body.system.map((block: { text: string }) => block.text)).toEqual([
              "A recorded project prompt.",
              `<skills>\n- beta: ${betaLine}\n- alpha: ${hint}\n</skills>`,
            ]);
          }
          const record = JSON.parse(readFileSync(capture.record, "utf8"));
          expect(record.context.skills).toEqual(profileContext(BETA_PROFILE, cache).skills);
          expect(record.hints).toContainEqual(
            expect.objectContaining({
              skill: "alpha",
              hint,
              source: `drawn (draw ${draw})`,
            })
          );
          expect(savedRuns(at, task.id)).toMatchObject(
            Array.from({ length: 3 }, () => ({ testSetup: setup, outcome: "loaded" }))
          );
          const summary = JSON.parse(readFileSync(path.join(at, "summary.json"), "utf8"));
          expect(summary).toMatchObject({
            testSetup: setup,
            verdict: { descriptionFault: false, testSetup: setup },
            tasks: [
              { testSetup: setup, listedUnder: { source: `drawn (draw ${draw})`, line: hint } },
            ],
          });
          expect(ran.stdout).toMatch(
            /^load: the skill alpha: no description fault in 1 tasks \(test setup, not observed use\)/m
          );
          expect(readFileSync(file, "utf8")).toBe(raw);
        }
        const hints = recorder.requests
          .slice(before)
          .filter((request) => request.body.includes("\\nSkill: alpha\\nDescription: "));
        expect(hints).toHaveLength(draw === 1 ? 1 : 0);
        if (draw === 1) expect(hints[0]?.body).toContain(description);
      }
    },
    HOST_ROW_TIMEOUT_MS
  );

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
          outcome: "loaded",
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

  // Catches (the architect, 2026-11-23T07:11Z): a load that draws a hint for a skill whose line
  // its session recorded, or that lists the session's line of the tested skill under a proposed
  // name or for a text the session's copy did not have.
  test(
    "a profile's recorded listing gives each skill its recorded line, and the tested skill its own only under its name and the session's text: a proposed name and a changed text get drawn hints, and the summary and the report say which",
    async () => {
      const dir = path.join(scratch, "omp-recorded");
      const { out, common } = hostInputs(dir);
      const tested = path.join(dir, "tested");
      const models = path.join(dir, "models.yml");
      writeFileSync(models, ANTHROPIC_MODELS);
      const ALPHA_LINE = "Use for the alpha checklist, as the session listed it.";
      const BETA_LINE = "Use for the beta greeting, as the session listed it.";
      const profile = path.join(dir, "profile.json");
      writeFileSync(
        profile,
        JSON.stringify({
          ...BETA_PROFILE,
          recordedListing: {
            source: { kind: "session" },
            lines: [
              { name: "alpha", text: ALPHA_LINE },
              { name: "beta", text: BETA_LINE },
            ],
            // The session's copy of alpha: acme/skills holds ALPHA_SKILL there, the tested text.
            tested: { repo: "acme/skills", commit: COMMIT, path: "skills/alpha", name: "alpha" },
          },
        })
      );
      // A tested text whose description the session's copy did not have.
      const changed = path.join(dir, "changed");
      writeTree(changed, {
        "SKILL.md": ALPHA_SKILL.replace(
          "the made-up project.",
          "the made-up project and its steps."
        ),
        "references/x.md": ALPHA_X,
      });
      answer = () => TEXT;
      /** A load into `<dir>/<name>` of `skill`, with `extra`: what it printed, its capture's
       *  listing, and its summary's line of the tested skill. */
      const load = async (name: string, skill: string, extra: readonly string[] = []) => {
        const at = path.join(dir, name);
        const given = common.map((arg) => (arg === out ? at : arg === tested ? skill : arg));
        const ran = await cli([
          "load",
          ...given,
          ...["--host", "omp", "--profile", profile, "--omp", String(omp), "--models", models],
          ...extra,
        ]);
        expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
        const body = readFileSync(loadCapturePaths(at, "omp", "need-1").body, "utf8");
        const summary = z
          .looseObject({ tasks: z.array(z.looseObject({ listedUnder: z.unknown() })) })
          .parse(JSON.parse(readFileSync(path.join(at, "summary.json"), "utf8")));
        return {
          said: ran.stdout,
          listing: Object.fromEntries(skillListing(JSON.parse(body))),
          listedUnder: summary.tasks[0]?.listedUnder,
        };
      };
      const counts = (name: string, recorded: number, drawn: number) =>
        `load: the listing line of ${name}: ${recorded} tasks list it under the line their session recorded, ${drawn} under a hint of draw 1\n`;

      const own = await load("own", tested);
      expect(own.listing).toEqual({ alpha: ALPHA_LINE, beta: BETA_LINE });
      expect(own.listedUnder).toEqual({ source: "recorded (session)", line: ALPHA_LINE });
      expect(own.said).toContain(counts("alpha", 1, 0));

      const renamed = await load("renamed", tested, ["--name", "omega"]);
      const omegaHint = "Use for the omega work, stand-in hint.";
      expect(renamed.listing).toEqual({ omega: omegaHint, beta: BETA_LINE });
      expect(renamed.listedUnder).toEqual({ source: "drawn (draw 1)", line: omegaHint });
      expect(renamed.said).toContain(counts("omega", 0, 1));

      const text = await load("changed", changed);
      const alphaHint = "Use for the alpha work, stand-in hint.";
      expect(text.listing).toEqual({ alpha: alphaHint, beta: BETA_LINE });
      expect(text.listedUnder).toEqual({ source: "drawn (draw 1)", line: alphaHint });
      expect(text.said).toContain(counts("alpha", 0, 1));
    },
    HOST_ROW_TIMEOUT_MS
  );

  // Catches: a capture failed by the read of the session copy's description where no line of its
  // listing uses it: a recorded listing with no line of the tested skill, or a test under another
  // name, whose lines are the same whatever the description.
  test(
    "the session copy's description is read only where the recorded line of the tested skill can be listed under its name: a failing read fails a capture that uses it, and no other",
    async () => {
      const dir = path.join(scratch, "omp-description-read");
      const { out, common } = hostInputs(dir);
      // The session copy's SKILL.md is a directory, so its read fails, and not as a missing file.
      mkdirSync(path.join(dir, "repos", "acme/skills", COMMIT, "skills/broken/SKILL.md"), {
        recursive: true,
      });
      const models = path.join(dir, "models.yml");
      writeFileSync(models, ANTHROPIC_MODELS);
      const ALPHA_LINE = "Use for the alpha checklist, as the session listed it.";
      const BETA_LINE = "Use for the beta greeting, as the session listed it.";
      answer = () => TEXT;
      /** A load into `<dir>/<name>` under a profile whose recorded listing holds `lines`, its
       *  session's copy of alpha the broken one, with `extra`. */
      const load = async (
        name: string,
        lines: readonly { name: string; text: string }[],
        extra: readonly string[] = []
      ) => {
        const profile = path.join(dir, `${name}.json`);
        writeFileSync(
          profile,
          JSON.stringify({
            ...BETA_PROFILE,
            recordedListing: {
              source: { kind: "session" },
              lines,
              tested: { repo: "acme/skills", commit: COMMIT, path: "skills/broken", name: "alpha" },
            },
          })
        );
        const at = path.join(dir, name);
        return await cli([
          "load",
          ...common.map((arg) => (arg === out ? at : arg)),
          ...["--host", "omp", "--profile", profile, "--omp", String(omp), "--models", models],
          ...extra,
        ]);
      };
      const alphaLine = { name: "alpha", text: ALPHA_LINE };
      const betaLine = { name: "beta", text: BETA_LINE };
      // No line of alpha, or alpha tested under another name: the copy is not read.
      const noLine = await load("no-line", [betaLine]);
      expect({ code: noLine.code, stderr: noLine.stderr }).toEqual({ code: 0, stderr: "" });
      const renamed = await load("renamed", [alphaLine, betaLine], ["--name", "omega"]);
      expect({ code: renamed.code, stderr: renamed.stderr }).toEqual({ code: 0, stderr: "" });
      // A line of alpha under its own name: read, and its failure fails the capture, naming it.
      const used = await load("used", [alphaLine, betaLine]);
      expect(used.code).toBe(1);
      expect(used.stderr).toContain("load: task need-1 (need, written): ");
      expect(used.stderr).toContain("EISDIR");
      expect(existsSync(loadCapturePaths(path.join(dir, "used"), "omp", "need-1").body)).toBe(
        false
      );
    },
    HOST_ROW_TIMEOUT_MS
  );

  // Catches: a load test of dispatch-first under another name whose capture fails as the Envoy
  // plugin finds no file of that name to read, or that lists the skill under its own name, or
  // sends no block of it though the plugin puts one into each request.
  test(
    "a skill the Envoy plugin puts into each request is captured under another name with the plugin's block of the tested text, the plugin loading on a hidden stand-in of its file, and listed under that name only",
    async () => {
      const dir = path.join(scratch, "omp-dispatch-first");
      const { out, common } = hostInputs(dir, undefined, { "SKILL.md": DISPATCH_FIRST_SKILL });
      writeEnvoyPlugin(path.join(dir, "repos"));
      const models = path.join(dir, "models.yml");
      writeFileSync(models, ANTHROPIC_MODELS);
      const profile = path.join(dir, "profile.json");
      writeFileSync(profile, JSON.stringify({ ...BETA_PROFILE, plugins: [ENVOY_ENTRY] }));
      answer = () => TEXT;
      const ran = await cli([
        "load",
        ...common,
        ...["--name", "omega", "--host", "omp", "--profile", profile],
        ...["--omp", String(omp), "--models", models],
      ]);
      expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
      const files = loadCapturePaths(out, "omp", "need-1");
      const body = JSON.parse(readFileSync(files.body, "utf8"));
      const record = JSON.parse(readFileSync(files.record, "utf8")) as CaptureRecord;
      // The plugin loaded, reading the hidden stand-in, which the listing does not name.
      expect(record.extensionErrors).toEqual([]);
      expect(record.pluginTools.envoy).toContain("envoy_send");
      expect(record.context.skills).toContainEqual({ name: "dispatch-first", from: "stand-in" });
      expect([...skillListing(body).keys()].sort()).toEqual(["beta", "omega"]);
      // The block is the plugin's of the tested text, the same as under the skill's own name: its
      // frontmatter, which holds the name, is not in it.
      const block = readDispatchFirstContext(path.join(dir, "tested", "SKILL.md"));
      expect(body.messages[0]?.role).toBe("user");
      expect(texts(body.messages[0]?.content)).toContain(block);
      expect(texts(body.messages).some((text) => text.includes(NEED))).toBe(true);
      expect(record.injections).toEqual([
        { by: "plugin", plugin: "pi-envoy", name: "dispatch-first", at: 0 },
      ]);
      expect(ran.stdout).toContain(
        `load: the pi-envoy plugin puts dispatch-first into each request in the context of ${profile}: each capture carries its block, built from the tested text, before the task's text, the same under the name omega`
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
      // Well within the 60 s the hint run's turn waits with no hint request or answer.
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
          { outcome: "loaded", profile: file, profileDigest: sha256(file) },
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
          outcome: "loaded",
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
          outcome: "loaded",
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
          outcome: "loaded",
          loadedBy: "read skill://alpha",
          skillReads: "answered",
          replies: [
            {
              calls: [{ name: "read", target: "skill://alpha" }],
              usage: { model: "gpt-6.1-sol", reasoning: 0 },
            },
          ],
          stopReason: "completed",
        },
      ]);
    },
    HOST_ROW_TIMEOUT_MS
  );

  // Catches: a Legion task's role prompt composed at its plugin's commit (the worker image's)
  // where the tasks file's summary names another commit its daemon image was built from, so a
  // load test ran under a role prompt the pod's daemon never sent.
  test(
    "a Legion task's role prompt is composed at the commit its evidence in the tasks file's summary names as its daemon image's, not at its plugin's commit",
    async () => {
      const dir = path.join(scratch, "omp-daemon-commit");
      const { out, common } = hostInputs(dir);
      const repos = path.join(dir, "repos");
      writeTree(path.join(repos, "acme/legion", COMMIT), {
        ...daemonFiles("The role part at the plugin's commit."),
        "plugins/skills-only/skills/beta/SKILL.md": BETA_SKILL,
      });
      writeTree(
        path.join(repos, "acme/legion", DAEMON_COMMIT),
        daemonFiles("The role part at the daemon image's commit.")
      );
      const models = path.join(dir, "models.yml");
      writeFileSync(models, ANTHROPIC_MODELS);
      const profile = path.join(dir, "profile.json");
      writeFileSync(profile, JSON.stringify(LEGION_BETA_PROFILE));
      writeTasksSummary(path.join(dir, "tasks.jsonl"), {}, measureItem(null), [DAEMON_ITEM]);
      answer = () => TEXT;
      const ran = await cli([
        "load",
        ...common,
        ...["--host", "omp", "--profile", profile, "--omp", String(omp), "--models", models],
      ]);
      expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
      const files = loadCapturePaths(out, "omp", "need-1");
      const body = JSON.parse(readFileSync(files.body, "utf8"));
      const record = JSON.parse(readFileSync(files.record, "utf8")) as CaptureRecord;
      expect(
        texts(body.system).some((text) =>
          text.includes("The role part at the daemon image's commit.")
        )
      ).toBe(true);
      expect(
        texts(body).some((text) => text.includes("The role part at the plugin's commit."))
      ).toBe(false);
      expect(record.context.rolePrompt).toMatchObject({
        repo: "acme/legion",
        commit: DAEMON_COMMIT,
        commitOf: "daemon image",
        role: "implementer",
      });
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
      writeFileSync(profile, JSON.stringify({ ...BETA_CLAUDE_PROFILE, contextFiles: [USER_FILE] }));
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
            sessionRepository: z.unknown(),
            placements: z.unknown(),
            listedSource: z.unknown(),
          })
          .parse(JSON.parse(readFileSync(files.record, "utf8")));
        expect(record.userMemory).toEqual(memory);
        // The profile names no session repository, so every skill is a plugin's, as the record says.
        expect(record.sessionRepository).toEqual({
          none: "the profile names no session repository, so no skill is placed as a project skill",
        });
        expect(record.placements).toEqual([
          { skill: sibling, as: "plugin", plugin },
          { skill: "alpha", as: "plugin", plugin },
        ]);
        expect(record.requestHeaders["anthropic-beta"]).toBeString();
        expect(
          Object.keys(record.requestHeaders).every((name) => name.startsWith("anthropic-"))
        ).toBe(true);
        expect([record.profile, record.profileDigest]).toEqual([file, sha256(file)]);
        // Claude Code lists the skill under its description: no hint is drawn or recorded.
        expect(record.listedSource).toBe("description (Claude Code)");
        expect(savedRuns(out, id)).toMatchObject([
          {
            outcome: "loaded",
            loadedBy: `Skill ${plugin}:alpha`,
            skillReads: "first message",
            replies: [{ calls: [{ name: "Skill", target: `${plugin}:alpha` }] }],
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

  test(
    "gives a devbox profile's user-level file as user memory and its repository's root CLAUDE.md as the project's instructions, from the working directory",
    async () => {
      const dir = path.join(scratch, "claude-project");
      const { out, common } = hostInputs(dir);
      writeTree(path.join(dir, "repos", "acme/proj", COMMIT), { "CLAUDE.md": PROJECT_RULES });
      const profile = path.join(dir, "profile.json");
      writeFileSync(
        profile,
        JSON.stringify({ ...BETA_CLAUDE_PROFILE, contextFiles: [USER_FILE, PROJECT_FILE] })
      );
      answer = () => [
        {
          type: "tool_use",
          id: "toolu_skill",
          name: "Skill",
          input: { skill: "acme-tools:alpha" },
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
      const files = loadCapturePaths(out, "claude", "need-1");
      const sent = texts(JSON.parse(readFileSync(files.body, "utf8")).messages).join("\n");
      const user = "End each answer with the word done.";
      const project = "Start each answer with the word ready.";
      expect(sent).toContain(user);
      expect(sent).toContain(project);
      // Each under its own heading: the user's memory, then the project's instructions.
      const userHeading = sent.lastIndexOf(
        "(user's private global instructions for all projects)",
        sent.indexOf(user)
      );
      const projectHeading = sent.lastIndexOf("(project instructions", sent.indexOf(project));
      expect(userHeading).toBeGreaterThan(-1);
      expect(projectHeading).toBeGreaterThan(sent.indexOf(user));
      const record = z
        .looseObject({
          userMemory: z.unknown(),
          context: z.looseObject({ contextFiles: z.array(z.looseObject({ as: z.string() })) }),
        })
        .parse(JSON.parse(readFileSync(files.record, "utf8")));
      expect(record.userMemory).toEqual(USER_FILE);
      expect(record.context.contextFiles).toMatchObject([
        { ...PROJECT_FILE, as: "working directory" },
      ]);
    },
    HOST_ROW_TIMEOUT_MS
  );

  // Catches: a project skill of the session's checkout listed as <plugin>:<name>, where the real
  // session's Claude Code listed it under its bare name; and a .omp/skills one listed at all.
  test(
    "writes a .claude/skills project skill of the profile's session repository at its path in the checkout, where Claude Code lists it under its bare name, leaves a .omp/skills one out, keeps the unit skill and the tested skill in their plugin, and records each placement",
    async () => {
      const dir = path.join(scratch, "claude-project-skill");
      const { out, common } = hostInputs(dir);
      writeTree(path.join(dir, "repos", "acme/proj", COMMIT), {
        ...Object.fromEntries(
          Object.entries(KAPPA_FILES).map(([file, text]) => [`${KAPPA.path}/${file}`, text])
        ),
        [`${LAMBDA.path}/SKILL.md`]: LAMBDA_SKILL,
      });
      const profile = path.join(dir, "profile.json");
      writeFileSync(
        profile,
        JSON.stringify({
          ...BETA_PROFILE,
          ...PROJ_CHECKOUT,
          skills: [...BETA_PROFILE.skills, KAPPA, LAMBDA],
          claude: BETA_CLAUDE_PROFILE.claude,
        })
      );
      answer = () => [
        {
          type: "tool_use",
          id: "toolu_skill",
          name: "Skill",
          input: { skill: "acme-tools:alpha" },
        },
      ];
      const ran = await cli([
        "load",
        ...common,
        ...["--host", "claude", "--profile", profile, "--thinking", "off"],
        ...["--claude", String(claude)],
      ]);
      expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
      const files = loadCapturePaths(out, "claude", "need-1");
      const listing = claudeListing(JSON.parse(readFileSync(files.body, "utf8")));
      expect(listing.get("kappa")).toBe(
        "Use when a task asks for the kappa summary of the made-up project."
      );
      const names = [...listing.keys()];
      expect(names.filter((name) => name.endsWith(":kappa"))).toEqual([]);
      // Claude Code reads no .omp/skills.
      expect(names.filter((name) => name.includes("lambda"))).toEqual([]);
      // The unit skill and the tested skill list as before, under their plugin.
      expect(listing.get("acme-tools:beta")).toBe(
        "Use when a task asks for the beta greeting of the made-up project."
      );
      expect(listing.get("acme-tools:alpha")).toBe(
        "Use when a task asks for the alpha checklist of the made-up project."
      );
      const record = z
        .looseObject({ sessionRepository: z.unknown(), placements: z.unknown() })
        .parse(JSON.parse(readFileSync(files.record, "utf8")));
      expect(record.sessionRepository).toEqual(PROJ_CHECKOUT.sessionRepository);
      expect(record.placements).toEqual([
        { skill: "beta", as: "plugin", plugin: "acme-tools" },
        { skill: "kappa", as: "checkout", path: KAPPA.path },
        {
          skill: "lambda",
          as: "left out",
          reason:
            "it is a project skill of acme/proj (.omp/skills/lambda), which Claude Code does not read",
        },
        { skill: "alpha", as: "plugin", plugin: "acme-tools" },
      ]);
      // kappa's files are in the checkout as its directory holds them.
      const [load = ""] = readdirSync(path.join(dir, "scratch")).filter((name) =>
        name.startsWith("load-")
      );
      const profiles = path.join(dir, "scratch", load, "claude", "profiles");
      const [one = ""] = readdirSync(profiles);
      expect(treeOf(path.join(profiles, one, "context", "work", KAPPA.path))).toEqual(KAPPA_FILES);
      expect(savedRuns(out, "need-1")).toMatchObject([
        { outcome: "loaded", loadedBy: "Skill acme-tools:alpha" },
      ]);
    },
    HOST_ROW_TIMEOUT_MS
  );
});
