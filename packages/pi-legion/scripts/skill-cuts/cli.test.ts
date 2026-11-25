import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import {
  appendFileSync,
  copyFileSync,
  cpSync,
  existsSync,
  mkdirSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { postedSpan, startCollector } from "../skill-scenarios/collector-standin";
import { createSpendRecorder, FLUSH_MS, POST_TIMEOUT_MS } from "../skill-scenarios/spend-record";
import { SessionEntry, type SessionMessage, stripSkillHeader } from "../skill-scenarios/transcript";
import { type ArmName, buildArm, snapshotTag } from "./arms";
import { ListingMeasure } from "./build";
import { capturePaths } from "./capture";
import { type Case, pathHash } from "./cases";
import {
  armCaptureDir,
  CaseResult,
  type CliIo,
  caseResultFile,
  falseLossChance,
  lossChance,
  main,
  noVerdict,
  ompBinary,
  runAfterCase,
  runnerDigest,
  SENDS,
  textDigest,
} from "./cli";
import { loadConfig } from "./config";
import { cacheKey, skillFrontmatter } from "./context";
import { countSql } from "./countsql";
import { GATEWAY_TRIES } from "./gateway";
import { NOTHING_REPLY } from "./judge";
import { ATTACK_PLANTED_LINE, loadPrompt, READER_TRIES } from "./readers";
import {
  messageSse,
  type RecordedRequest,
  type Recorder,
  responseSse,
  sseResponse,
  startRecorder,
} from "./recorder";
import { READS_PAST_LAST_ROUND, sendOrder } from "./replay";
import {
  FRESH_RUNS,
  LISTING_MEASURE_ITEM,
  listingStanding,
  MAX_EXTRA_RUNS,
  MIN_INFORMATIVE_CASES,
  type RunRecord,
  SCREEN_RUNS,
  STAGE_RUNS,
  type Stage,
  scoreSkill,
} from "./score";
import { truncatedSkillsTree, writeGhStandIn } from "./testdata/fixtures";

// The runner's command line, end to end: each command runs as its own `bun cli.ts` process, with
// every GH_* and GITHUB_* variable taken out of its environment, against a stand-in gateway on
// 127.0.0.1 built on recorder.ts, which answers in the stream shape of the F5 replies
// (testdata/CAPTURE.md), and with its usage spans posted to a stand-in collector on 127.0.0.1
// (skill-scenarios/collector-standin.ts's `startCollector`), every other OTEL_* variable taken
// out. The behaviour commands read their cases from a temporary directory, made from F1 (an Oh My
// Pi 18.18.6 session
// of a scratch profile with the made-up skills alpha and beta: 0 the user's prompt, 1 a read of
// skill://alpha, 2 its result, 3 a read of skill://alpha/references/x.md, 4 its result, 5 a read
// of the missing skill://gamma, 6 its result, 7 a `task` call), and replay bodies recorded from F2
// (the request body Oh My Pi sends for a replayed history, `omp -p --mode json --no-session --hook
// <replay hook>` against the recorder),
// each with its arm's read result put in, saved where the capture step reads a capture back: no
// Oh My Pi runs here. No test here reaches a network. Every skill, repository and store is made up.

const CLI = path.join(import.meta.dir, "cli.ts");
const scratch = temporaryDirectory("skill-cuts-cli", { real: true });

/** The stand-in spend collector each command's usage spans go to. */
const collector = startCollector();
afterAll(() => {
  collector.close();
});

/** The environment of each command: this one's, less every GH_*, GITHUB_* and OTEL_* variable,
 *  with the stand-in collector as its spend endpoint. */
const ENV: Readonly<Record<string, string | undefined>> = {
  ...Object.fromEntries(
    Object.entries(process.env).filter(([name]) => !/^(GH|GITHUB|OTEL)_/.test(name))
  ),
  OTEL_EXPORTER_OTLP_ENDPOINT: collector.url,
  OTEL_RESOURCE_ATTRIBUTES: "theorem.project=skill-cuts-test",
};

/** What one command did. */
interface Ran {
  readonly code: number;
  readonly stdout: string;
  readonly stderr: string;
}

/** Runs `bun cli.ts <args>` in `env`. */
async function cli(
  args: readonly string[],
  env: Readonly<Record<string, string | undefined>> = ENV
): Promise<Ran> {
  const child = Bun.spawn([process.execPath, CLI, ...args], {
    env,
    stdin: "ignore",
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  return { code, stdout, stderr };
}

/** The token counts of each stand-in reply. */
let usage = { input: 10, output: 3 };

/** The stand-in gateway: answers each POST to /v1/messages with what `answer` gives for its body:
 *  a response as it is, or a text as a streamed reply of the body's own model with `usage`. */
let answer: (body: { model: string; [field: string]: unknown }) => string | Response = () => "{}";
let recorder: Recorder;
beforeAll(() => {
  recorder = startRecorder({
    route: (request: RecordedRequest) => {
      const responses = request.path === "/v1/responses";
      if (request.method !== "POST" || (request.path !== "/v1/messages" && !responses)) {
        return undefined;
      }
      const body = JSON.parse(request.body) as { model: string };
      const answered = answer(body);
      if (answered instanceof Response) return answered;
      // A Responses body (an openai/<id> case's replay) gets the answer in its API's stream.
      return sseResponse(
        responses
          ? responseSse({
              id: `resp_standin_${request.n}`,
              model: body.model,
              output: [{ type: "message", text: answered }],
              inputTokens: usage.input,
              outputTokens: usage.output,
            })
          : messageSse({
              id: `msg_standin_${request.n}`,
              model: body.model,
              content: [{ type: "text", text: answered }],
              stopReason: "end_turn",
              inputTokens: usage.input,
              outputTokens: usage.output,
            }),
        `req_standin_${request.n}`
      );
    },
  });
});
afterAll(async () => {
  await recorder.close();
});

/** The requests the stand-in took since `from`. */
function requestsSince(from: number): readonly RecordedRequest[] {
  return recorder.requests.slice(from);
}

/** A configuration whose gateway is the stand-in, and a key file. */
function setup(): { config: string; keyFile: string; key: string } {
  const config = path.join(scratch, "skill-cuts.json");
  const fixture = JSON.parse(
    readFileSync(path.join(import.meta.dir, "testdata", "F7-skill-cuts.json"), "utf8")
  );
  writeFileSync(config, JSON.stringify({ ...fixture, gatewayBaseUrl: recorder.url }));
  const key = fixtureSecret("gw");
  const keyFile = path.join(scratch, "key");
  writeFileSync(keyFile, key, { mode: 0o600 });
  return { config, keyFile, key };
}

const SKILL_MD = [
  "---",
  "name: alpha",
  "description: Use when a task asks for the alpha checklist of the made-up fixture project.",
  "---",
  "",
  "# Alpha",
  "",
  "1. Read `references/x.md` before the first step.",
  "2. Write each step's result on one line.",
  "",
].join("\n");

describe("read", () => {
  test("runs the reader through the gateway, writes its answer, and logs each request's settings and usage, never the key or a message's text", async () => {
    const { config, keyFile, key } = setup();
    const input = path.join(scratch, "attack-in.json");
    writeFileSync(
      input,
      JSON.stringify({ skill: "alpha", files: [{ path: "SKILL.md", text: SKILL_MD }] })
    );
    const finding = {
      kind: "vague",
      file: "SKILL.md",
      line: 9,
      quote: "Write each step's result on one line.",
      why: "It does not say what a result is.",
    };
    answer = () => JSON.stringify({ findings: [finding] });
    const out = path.join(scratch, "attack-out.json");
    const from = recorder.requests.length;
    const ran = await cli([
      "read",
      "--config",
      config,
      "--key-file",
      keyFile,
      "--prompt",
      "attack",
      "--in",
      input,
      "--out",
      out,
    ]);
    expect(ran.stderr).toBe("");
    expect(ran.code).toBe(0);
    const written = JSON.parse(readFileSync(out, "utf8"));
    expect(written.prompt).toBe("attack");
    expect(written.answer).toEqual({ findings: [finding] });
    expect(written.refusals).toEqual([]);
    expect(written.settings).toEqual(loadPrompt("attack").settings);
    expect(written.spent).toHaveLength(1);

    const requests = requestsSince(from);
    expect(requests).toHaveLength(1);
    expect(JSON.parse(requests[0]?.body ?? "{}").messages[0].content[0].text).toContain(
      "Write each step's result on one line."
    );

    const log = readFileSync(`${out}.requests.jsonl`, "utf8");
    const lines = log
      .trim()
      .split("\n")
      .map((line) => JSON.parse(line));
    const settings = loadPrompt("attack").settings;
    expect(lines).toHaveLength(1);
    expect(lines[0]).toMatchObject({
      kind: "reader:attack",
      model: settings.model,
      thinking: "enabled",
      thinking_budget: settings.thinking_budget,
      max_tokens: settings.max_tokens,
      temperature: settings.temperature,
      effort: settings.effort,
      stream: true,
      answeredBy: settings.model,
      requestId: `req_standin_${requests[0]?.n}`,
      stopReason: "end_turn",
      usage: { input_tokens: 10, output_tokens: 3 },
    });
    expect(log).not.toContain(key);
    expect(log).not.toContain("alpha checklist");
    expect(log).not.toContain("one line");
  }, 30_000);

  test("records one usage span of its model call, sent before it exits, whose request log line gives the same span", async () => {
    const { config, keyFile } = setup();
    const input = path.join(scratch, "spans-in.json");
    writeFileSync(
      input,
      JSON.stringify({ skill: "alpha", files: [{ path: "SKILL.md", text: SKILL_MD }] })
    );
    answer = () => JSON.stringify({ findings: [] });
    const out = path.join(scratch, "spans-out.json");
    const from = recorder.requests.length;
    const posted = collector.posts.length;
    const ran = await cli([
      "read",
      "--config",
      config,
      "--key-file",
      keyFile,
      "--prompt",
      "attack",
      "--in",
      input,
      "--out",
      out,
    ]);
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const requests = requestsSince(from);
    expect(requests).toHaveLength(1);
    // The command flushed its usage spans before it exited: its one span is here already.
    const spans = collector.posts.slice(posted);
    expect(spans.map((post) => post.path)).toEqual(["/v1/traces"]);
    const [post] = spans;
    if (post === undefined) throw new Error("no span");
    const live = postedSpan(post);
    const { model } = loadPrompt("attack").settings;
    expect(live.attributes).toMatchObject({
      "gen_ai.provider.name": "anthropic",
      "gen_ai.request.model": model,
      "gen_ai.response.model": model,
      "gen_ai.response.id": `msg_standin_${requests[0]?.n}`,
      "gen_ai.usage.input_tokens": "10",
      "gen_ai.usage.output_tokens": "3",
      "omp.gen_ai.auth.kind": "api_key",
    });
    expect(JSON.parse(post.body).resourceSpans[0].resource.attributes).toEqual([
      { key: "theorem.project", value: { stringValue: "skill-cuts-test" } },
      { key: "service.name", value: { stringValue: "spend-record" } },
      { key: "theorem.harness", value: { stringValue: "script" } },
    ]);

    // The request log's line of that call, sent through `recordLogged` as a backfill sends it,
    // gives the same attributes, and a start no later than the live span's.
    const [line] = readFileSync(`${out}.requests.jsonl`, "utf8").trim().split("\n");
    const backfill = createSpendRecorder({ env: { OTEL_EXPORTER_OTLP_ENDPOINT: collector.url } });
    backfill.recordLogged(JSON.parse(line ?? "{}"));
    await backfill.flush();
    const [again] = collector.posts.slice(posted + 1);
    if (again === undefined) throw new Error("no span of the logged line");
    const logged = postedSpan(again);
    expect(logged.attributes).toEqual(live.attributes);
    const apart =
      BigInt(String(live.span.startTimeUnixNano)) - BigInt(String(logged.span.startTimeUnixNano));
    expect(apart >= 0n && apart < 900_000_000_000n).toBe(true);
  }, 30_000);

  test("refuses an unknown prompt and an input its reader's schema refuses, before any call", async () => {
    const { config, keyFile } = setup();
    const input = path.join(scratch, "bad-in.json");
    writeFileSync(input, JSON.stringify({ skill: "alpha", files: [] }));
    const out = path.join(scratch, "bad-out.json");
    const from = recorder.requests.length;
    const unknown = await cli([
      "read",
      "--config",
      config,
      "--key-file",
      keyFile,
      "--prompt",
      "poem",
      "--in",
      input,
      "--out",
      out,
    ]);
    expect(unknown.code).toBe(2);
    expect(unknown.stderr).toContain("--prompt poem is no reader");
    const refused = await cli([
      "read",
      "--config",
      config,
      "--key-file",
      keyFile,
      "--prompt",
      "attack",
      "--in",
      input,
      "--out",
      out,
    ]);
    expect(refused.code).toBe(2);
    expect(refused.stderr).toContain("the attack reader's input");
    const missing = await cli([
      "read",
      "--config",
      config,
      "--prompt",
      "attack",
      "--in",
      input,
      "--out",
      out,
    ]);
    expect(missing.code).toBe(2);
    expect(missing.stderr).toContain("--key-file required");
    expect(requestsSince(from)).toHaveLength(0);
  }, 30_000);

  test("writes a null answer with each refusal when every reply is refused, and exits 1", async () => {
    const { config, keyFile } = setup();
    const input = path.join(scratch, "attack-in.json");
    writeFileSync(
      input,
      JSON.stringify({ skill: "alpha", files: [{ path: "SKILL.md", text: SKILL_MD }] })
    );
    answer = () => "not JSON at all";
    const out = path.join(scratch, "refused-out.json");
    const from = recorder.requests.length;
    const ran = await cli([
      "read",
      "--config",
      config,
      "--key-file",
      keyFile,
      "--prompt",
      "attack",
      "--in",
      input,
      "--out",
      out,
    ]);
    expect(ran.code).toBe(1);
    const written = JSON.parse(readFileSync(out, "utf8"));
    expect(written.answer).toBeNull();
    expect(written.refusals).toHaveLength(READER_TRIES);
    expect(requestsSince(from)).toHaveLength(READER_TRIES);
  }, 30_000);
});

describe("control", () => {
  test("attack plants its line, runs the reader once, and holds when the answer lists that line", async () => {
    const { config, keyFile } = setup();
    const input = path.join(scratch, "attack-in.json");
    writeFileSync(
      input,
      JSON.stringify({ skill: "alpha", files: [{ path: "SKILL.md", text: SKILL_MD }] })
    );
    // The stand-in finds the planted line where the reader's numbered text shows it.
    answer = (body) => {
      const line = new RegExp(`(\\d+): ${ATTACK_PLANTED_LINE.replace(".", "\\.")}`).exec(
        JSON.stringify(body)
      )?.[1];
      return JSON.stringify({
        findings:
          line === undefined
            ? []
            : [
                {
                  kind: "vague",
                  file: "SKILL.md",
                  line: Number(line),
                  quote: ATTACK_PLANTED_LINE,
                  why: "Where is that?",
                },
              ],
      });
    };
    const out = path.join(scratch, "control-attack.json");
    const held = await cli([
      "control",
      "attack",
      "--config",
      config,
      "--key-file",
      keyFile,
      "--in",
      input,
      "--out",
      out,
    ]);
    expect(held.stderr).toBe("");
    expect(held.code).toBe(0);
    expect(JSON.parse(readFileSync(out, "utf8"))).toMatchObject({ control: "attack", held: true });

    answer = () => JSON.stringify({ findings: [] });
    const missed = await cli([
      "control",
      "attack",
      "--config",
      config,
      "--key-file",
      keyFile,
      "--in",
      input,
      "--out",
      out,
    ]);
    expect(missed.code).toBe(1);
    expect(JSON.parse(readFileSync(out, "utf8"))).toMatchObject({ control: "attack", held: false });
  }, 30_000);

  test("refuses a control it cannot plant, before any call", async () => {
    const { config, keyFile } = setup();
    const input = path.join(scratch, "map-in.json");
    writeFileSync(
      input,
      JSON.stringify({
        skill: "alpha",
        base: [{ path: "SKILL.md", text: SKILL_MD }],
        cut: [{ path: "SKILL.md", text: SKILL_MD }],
        audit: null,
      })
    );
    const out = path.join(scratch, "control-map.json");
    const from = recorder.requests.length;
    const noLine = await cli([
      "control",
      "map",
      "--config",
      config,
      "--key-file",
      keyFile,
      "--in",
      input,
      "--out",
      out,
    ]);
    expect(noLine.code).toBe(2);
    expect(noLine.stderr).toContain("--delete-line required");
    const absent = await cli([
      "control",
      "map",
      "--config",
      config,
      "--key-file",
      keyFile,
      "--in",
      input,
      "--out",
      out,
      "--delete-line",
      "No such line.",
    ]);
    expect(absent.code).toBe(2);
    expect(absent.stderr).toContain("no line of the cut text");
    const kind = await cli(["control", "poem", "--config", config, "--key-file", keyFile]);
    expect(kind.code).toBe(2);
    expect(requestsSince(from)).toHaveLength(0);
  }, 30_000);
});

describe("count-sql", () => {
  test("prints count.sql filled for the store and day, as countsql.ts does", async () => {
    const config = path.join(import.meta.dir, "testdata", "F7-skill-cuts.json");
    const ran = await cli([
      "count-sql",
      "--config",
      config,
      "--store",
      "store_a",
      "--day",
      "2026-11-20",
    ]);
    expect(ran.stderr).toBe("");
    expect(ran.code).toBe(0);
    expect(ran.stdout).toBe(countSql(loadConfig(config), "store_a", "2026-11-20"));
    const refused = await cli([
      "count-sql",
      "--config",
      config,
      "--store",
      "store_c",
      "--day",
      "2026-11-20",
    ]);
    expect(refused.code).toBe(2);
  }, 30_000);
});

// The behaviour test.

const TESTDATA = path.join(import.meta.dir, "testdata");
const F1 = path.join(
  TESTDATA,
  "F1",
  "2026-11-20T01-59-41-780Z_01a1f6ae-51d4-715f-a2a6-59f275bdff80.jsonl"
);
const F2 = path.join(TESTDATA, "F2-request.json");
/** The Responses API body Oh My Pi sent for an F2-like history on openai/gpt-6.1-sol. */
const F8 = path.join(TESTDATA, "F8-request.json");

/** F1's messages, in order. */
const messages: SessionMessage[] = readFileSync(F1, "utf8")
  .split("\n")
  .filter((line) => line.trim() !== "")
  .map((line) => SessionEntry.parse(JSON.parse(line)))
  .flatMap((entry) => (entry.type === "message" && entry.message ? [entry.message] : []));

/** The file F1's read result at `index` read, as it was on disk. */
function f1File(index: number): string {
  return `${stripSkillHeader(messages[index]?.content?.[0]?.text ?? "")}\n`;
}

/** The line of alpha's SKILL.md (line 9 of 10) each case exercises, and another line of it. */
const LINE = "2. Write each step's result on one line.";
const OTHER_LINE = "3. Stop after the third step.";
/** The steps the stand-in replays with: the one the line asks for, and one it does not. */
const PASS_STEP = "I write each step's result on one line.";
const FAIL_STEP = "I put the results wherever they fit.";
/** The anthropic-* headers Oh My Pi sent with F2's body. */
const F2_HEADERS: Record<string, string> = Object.fromEntries(
  Object.entries(
    JSON.parse(readFileSync(path.join(TESTDATA, "F2-request.headers.json"), "utf8"))
      .headers as Record<string, string>
  ).filter(([name]) => name.startsWith("anthropic-"))
);

/** How many replays of a case marked FLAKY the stand-in still refuses. */
let flakyLeft = 0;

/** Whether `body` is a judge's request: its system prompt is prompts/judge.md's. */
function isJudgement(body: { [field: string]: unknown }): boolean {
  const system = body.system as { text?: string }[] | undefined;
  return system?.[0]?.text === loadPrompt("judge").text;
}

/** The stand-in of the behaviour test. A judge passes a step that holds PASS_STEP and fails any
 *  other. A replay passes or fails by the arm text it receives: it answers PASS_STEP when its
 *  messages hold the case's line, and FAIL_STEP when they do not; it refuses (400) each body of a
 *  case marked BROKEN, and the first `flakyLeft` bodies of a case marked FLAKY. */
function behaviourStandIn(body: { [field: string]: unknown }): string | Response {
  if (isJudgement(body)) {
    const user = body.messages as { content: { text: string }[] }[];
    const step = user[0]?.content[1]?.text ?? "";
    return JSON.stringify({
      verdict: step.includes(PASS_STEP) ? "pass" : "fail",
      reason: "The stand-in judge read the step.",
    });
  }
  const text = JSON.stringify(body.messages ?? body.input);
  let refuse = text.includes("BROKEN");
  if (text.includes("FLAKY") && flakyLeft > 0) {
    flakyLeft -= 1;
    refuse = true;
  }
  if (refuse) {
    return Response.json(
      {
        type: "error",
        error: { type: "invalid_request_error", message: "the stand-in refuses this body" },
      },
      { status: 400 }
    );
  }
  return text.includes(LINE) ? PASS_STEP : FAIL_STEP;
}

/** A case of F1's step after alpha's reads, which LINE governs, whose real step is `realStep` and
 *  whose violating step is `violatingStep`. */
function makeCase(id: string, realStep = PASS_STEP, violatingStep = FAIL_STEP): Case {
  return {
    id,
    cut: "acme-skills-1",
    skill: "alpha",
    baseFile: "SKILL.md",
    line: { text: LINE, lineNumber: 9, lineCount: 10 },
    criterion: {
      pass: "The step writes each step's result on one line.",
      fail: "The step spreads a step's result over more than one line, or writes none.",
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
    realStep: { ...messages[7], content: [{ type: "text", text: realStep }] },
    violatingStep: { role: "assistant", content: [{ type: "text", text: violatingStep }] },
    source: { store: "store_a", pathHash: pathHash(`/sessions/${id}.jsonl`) },
    sessionStart: "2026-11-20T01:59:41.780Z",
    secretsRemoved: 0,
    sessionDigest: "46874d907e557998",
  };
}

/** One skill's texts, cases and output directory, under the scratch directory. */
interface Scenario {
  readonly dir: string;
  readonly old: string;
  readonly new: string;
  readonly casesFile: string;
  readonly out: string;
  readonly cases: readonly Case[];
  /** The word each case's recorded bodies carry: FLAKY, BROKEN or none. */
  readonly markers: ReadonlyMap<string, string>;
}

/** Writes a skill directory holding `files` (path inside it to text). */
function writeSkill(dir: string, files: Record<string, string>): void {
  for (const [file, text] of Object.entries(files)) {
    mkdirSync(path.dirname(path.join(dir, file)), { recursive: true });
    writeFileSync(path.join(dir, file), text);
  }
}

/** A scenario: alpha's old text as F1 read it, the cut's SKILL.md `newSkill`, and the cases. */
function scenario(
  name: string,
  newSkill: string,
  cases: readonly {
    id: string;
    realStep?: string;
    violatingStep?: string;
    marker?: string;
    model?: string;
  }[]
): Scenario {
  const dir = path.join(scratch, name);
  const old = path.join(dir, "old");
  const fresh = path.join(dir, "new");
  writeSkill(old, { "SKILL.md": f1File(2), "references/x.md": f1File(4) });
  writeSkill(fresh, { "SKILL.md": newSkill, "references/x.md": f1File(4) });
  const made = cases.map(({ id, realStep, violatingStep, model }) => ({
    ...makeCase(id, realStep, violatingStep),
    ...(model === undefined ? {} : { model }),
  }));
  const casesFile = path.join(dir, "cases.jsonl");
  writeFileSync(casesFile, `${made.map((c) => JSON.stringify(c)).join("\n")}\n`);
  return {
    dir,
    old,
    new: fresh,
    casesFile,
    out: path.join(dir, "out"),
    cases: made,
    markers: new Map(cases.map(({ id, marker }) => [id, marker ?? ""])),
  };
}

/** Where a capture of makeCase's profile installs beta from: its repository directory, under its
 *  cache key (context.ts's ContextRecord). */
const BETA_FROM = {
  repo: "acme/skills",
  commit: "0123456789abcdef",
  path: "skills/beta",
  key: cacheKey("acme/skills", "0123456789abcdef", "skills/beta"),
};

/** Saves a body recorded from F2 for each case of `s` and each arm of `arms` (with `plantedLine`
 *  for minus), where the capture step under `out` reads it back: F2 with the case's id and marker
 *  in its user text, and the arm's read result of SKILL.md in place of F2's, or, for none, no read.
 *  An `openai/<id>` case's body is F8 changed the same way, and goes with no header. Each record
 *  names the skills its context installed: alpha from the arm (none installs no alpha), and beta
 *  from BETA_FROM, whose cache entry it writes in `<dir>/cache`, the cache `behaviour` gives the
 *  commands, as such a capture leaves it. */
function recordCaptures(
  s: Scenario,
  arms: readonly ArmName[],
  plantedLine?: string,
  out = s.out
): void {
  const f2 = readFileSync(F2, "utf8");
  const f8 = readFileSync(F8, "utf8");
  const dirs = { old: s.old, new: s.new, ...(plantedLine === undefined ? {} : { plantedLine }) };
  const texts = { old: textDigest(s.old), new: textDigest(s.new) };
  for (const c of s.cases) {
    const responses = c.model.startsWith("openai/");
    for (const name of arms) {
      const arm = buildArm(c, name, dirs, path.join(s.dir, "arms", c.id));
      const said = `Case ${c.id} ${s.markers.get(c.id)}: write the checklist.`;
      const read = arm.messages[2]?.content?.[0]?.text;
      const body = JSON.parse(responses ? f8 : f2);
      if (responses) {
        body.input[0].content[1].text = said;
        if (name === "none") body.input = [body.input[0]];
        else body.input[2].output = read;
      } else {
        body.messages[0].content[1].text = said;
        if (name === "none") body.messages = [body.messages[0]];
        else body.messages[2].content[0].content[0].text = read;
      }
      const files = capturePaths(armCaptureDir(out, arm, texts), c.id, name);
      mkdirSync(path.dirname(files.body), { recursive: true });
      writeFileSync(files.body, JSON.stringify(body));
      writeFileSync(
        files.record,
        JSON.stringify({
          case: c.id,
          arm: name,
          requestHeaders: responses ? {} : F2_HEADERS,
          captureDigest: "0123456789abcdef",
          sessionDigest: c.sessionDigest,
          // A saved capture holds the harness-string rule's record, or it is refused.
          harness: { head: null, replaced: [] },
          context: {
            skills: [
              ...(name === "none" ? [] : [{ name: "alpha", from: "arm" }]),
              { name: "beta", from: BETA_FROM },
            ],
          },
        })
      );
    }
  }
  writeSkill(path.join(s.dir, "cache", "skills", BETA_FROM.key), {
    "SKILL.md": "---\nname: beta\ndescription: Use for the beta greeting.\n---\n",
  });
}

/** The arguments of `behaviour <command>` for `s`, with `<dir>/cache` as its cache, and `extra`. */
function behaviour(command: string, s: Scenario, ...extra: string[]): string[] {
  const { config, keyFile } = setup();
  return [
    "behaviour",
    command,
    "--config",
    config,
    "--key-file",
    keyFile,
    "--out",
    s.out,
    "--old",
    s.old,
    "--new",
    s.new,
    "--cases",
    s.casesFile,
    "--scratch",
    path.join(s.dir, "scratch"),
    "--cache-dir",
    path.join(s.dir, "cache"),
    ...extra,
  ];
}

/** The arguments of `behaviour report` for `s`: no key file and no new text. */
function report(s: Scenario): string[] {
  const { config } = setup();
  return [
    "behaviour",
    "report",
    "--config",
    config,
    "--out",
    s.out,
    "--old",
    s.old,
    "--cases",
    s.casesFile,
  ];
}

/** Case `id`'s results under `out`. */
function result(out: string, id: string): CaseResult {
  return CaseResult.parse(JSON.parse(readFileSync(caseResultFile(out, id), "utf8")));
}

/** What a capture of case `caseId` of `s` reads before any Oh My Pi run, made up: an Oh My Pi in
 *  `ompDir` that answers --version (the context is built before it runs any capture), a
 *  models.yml, and the session's head saved already, so no store is read. */
function standInContext(
  s: Scenario,
  caseId: string,
  ompDir: string
): { omp: string; models: string } {
  const omp = path.join(ompDir, "omp");
  writeFileSync(omp, "#!/bin/sh\necho 'omp 0.0.0 (stand-in)'\n", { mode: 0o755 });
  const models = path.join(s.dir, "models.yml");
  writeFileSync(models, "providers: {}\n");
  const hash = pathHash(`/sessions/${caseId}.jsonl`);
  mkdirSync(path.join(s.out, "sessions"), { recursive: true });
  writeFileSync(
    path.join(s.out, "sessions", `store_a-${hash}.json`),
    JSON.stringify({
      store: "store_a",
      pathHash: hash,
      held: false,
      cwd: null,
      start: null,
      firstUser: null,
      reminders: [],
      bytes: 0,
      badLines: 0,
      readAt: "2026-11-22T00:00:00.000Z",
    })
  );
  return { omp, models };
}

/** The passes of each arm in a stage of `r`. */
function passes(r: CaseResult, stage: Stage): Record<string, number> {
  const counts: Record<string, number> = {};
  for (const run of r.stages[stage]?.runs ?? []) {
    counts[run.arm] = (counts[run.arm] ?? 0) + (run.verdict === "pass" ? 1 : 0);
  }
  return counts;
}

/** The cut's SKILL.md with the case's line rewritten. */
const LOSS_SKILL = () => f1File(2).replace(LINE, "2. Put each step's result where it fits.");

describe("behaviour", () => {
  test("screen, confirm, control and report give each case's passes and the skill's verdict", async () => {
    const s = scenario("loss", LOSS_SKILL(), [
      { id: "c00", violatingStep: PASS_STEP },
      { id: "c01", realStep: FAIL_STEP },
      { id: "c02", marker: "FLAKY" },
      { id: "c03", marker: "BROKEN" },
      { id: "c04" },
      { id: "c05" },
      { id: "c06" },
      { id: "c07" },
      { id: "c08" },
      { id: "c09" },
    ]);
    recordCaptures(s, ["old", "new", "none", "minus", "twin"]);
    answer = behaviourStandIn;
    usage = { input: 10, output: 3 };
    flakyLeft = 3;
    const { key } = setup();
    const afterLog = path.join(s.dir, "after-case.log");

    let from = recorder.requests.length;
    const screen = await cli(behaviour("screen", s, "--after-case", `echo >> ${afterLog}`));
    expect(screen.stderr).toBe("");
    expect(screen.code).toBe(0);
    expect(screen.stdout).toContain(
      "screening stops before case c09: 6 informative and 6 screened cases"
    );
    // Three judge controls a case; then 30 replays and 30 judgements, c02's three refused replays
    // made again, and every one of c03's 15 replays of each arm refused.
    const screenRequests = requestsSince(from);
    expect(screenRequests).toHaveLength(3 + 3 + (3 + 33 + 30) + (3 + 45) + 5 * 63);
    // Each replay sends the anthropic-* headers its capture recorded.
    const replays = screenRequests.filter((request) => !isJudgement(JSON.parse(request.body)));
    expect(replays).toHaveLength(33 + 45 + 5 * 30);
    for (const request of replays) {
      expect(request.headers["anthropic-beta"]).toBe(F2_HEADERS["anthropic-beta"] ?? "");
    }
    // Every replay and every judgement went with a session id of its own (no stand-in reply
    // reads a skill file, so each run is one request), none the id its capture holds.
    const sessionIds = screenRequests.map(
      (request) => JSON.parse(request.body).metadata?.user_id as string | undefined
    );
    expect(sessionIds.every((id) => typeof id === "string")).toBe(true);
    expect(new Set(sessionIds).size).toBe(screenRequests.length);
    const captured = JSON.parse(readFileSync(F2, "utf8")).metadata.user_id;
    expect(sessionIds).not.toContain(captured);

    // c00's violating step is judged pass, and c01's real step fail: each is dropped, its drop
    // naming the control it failed.
    const c00 = result(s.out, "c00");
    expect(c00.dropped).toBe(
      "the judge controls do not hold: the violating step is judged pass (fail wanted)"
    );
    expect(c00.controls).toMatchObject({
      realStep: { verdict: "pass" },
      violating: { verdict: "pass" },
      nothing: { verdict: "fail" },
      failed: ["violating"],
      hold: false,
    });
    const c01 = result(s.out, "c01");
    expect(c01.dropped).toBe(
      "the judge controls do not hold: the real step is judged fail (pass wanted)"
    );
    expect(c01.controls).toMatchObject({
      realStep: { verdict: "fail" },
      violating: { verdict: "fail" },
      nothing: { verdict: "fail" },
      failed: ["realStep"],
      hold: false,
    });
    expect(c00.stages.screen).toBeUndefined();
    expect(c01.stages.screen).toBeUndefined();
    const c02 = result(s.out, "c02");
    expect(c02.stages.screen?.state).toBe("counted");
    expect(passes(c02, "screen")).toEqual({ old: SCREEN_RUNS, new: 0, none: 0 });
    expect(c02.stages.screen?.runs.filter((run) => run.verdict === null)).toHaveLength(3);
    expect(c02.stages.screen?.seeds).toHaveLength(2);
    for (const run of c02.stages.screen?.runs ?? []) {
      if (run.verdict !== null) expect(run.id).toMatch(/^[0-9a-f]{12}$/);
    }
    // Each run records its session id, the one its request went with, and its place in its
    // round's send order, which is the seed's: the three arms' runs interleaved, never one arm's
    // after another's.
    const c04 = result(s.out, "c04");
    const screened = c04.stages.screen;
    expect(screened?.sends).toBe(
      "each run its own session id; a round's arms interleaved in one seeded order"
    );
    const firstRound = (screened?.runs ?? []).filter((run) => run.round === 1);
    expect(firstRound).toHaveLength(30);
    const bySend = firstRound.toSorted((a, b) => a.sendPlace - b.sendPlace);
    expect(bySend.map((run) => run.sendPlace)).toEqual(Array.from({ length: 30 }, (_, n) => n));
    expect(bySend.map((run) => run.arm)).toEqual(
      sendOrder(
        (["old", "new", "none"] as const).map((key) => ({ key, runs: SCREEN_RUNS })),
        screened?.seeds[0] ?? ""
      ).map((each) => each.key)
    );
    expect(new Set(bySend.slice(0, 10).map((run) => run.arm)).size).toBeGreaterThan(1);
    const runIds = firstRound.map((run) => run.userId);
    expect(new Set(runIds).size).toBe(30);
    for (const id of runIds) expect(sessionIds).toContain(id ?? "no id");
    expect(result(s.out, "c03").stages.screen?.state).toBe("unscreened");
    expect(existsSync(caseResultFile(s.out, "c09"))).toBe(false);
    expect(readFileSync(afterLog, "utf8").trim().split("\n")).toEqual(
      ["c00", "c01", "c02", "c03", "c04", "c05", "c06", "c07", "c08"].map((id) =>
        caseResultFile(s.out, id)
      )
    );

    from = recorder.requests.length;
    const confirm = await cli(behaviour("confirm", s));
    expect(confirm.stderr).toBe("");
    expect(confirm.code).toBe(0);
    expect(confirm.stdout).toContain("6 flagged cases: c02, c04, c05, c06, c07, c08");
    expect(requestsSince(from)).toHaveLength(6 * 160);
    expect(passes(result(s.out, "c04"), "confirm")).toEqual({ old: 40, new: 0 });

    from = recorder.requests.length;
    const control = await cli(behaviour("control", s));
    expect(control.stderr).toBe("");
    expect(control.code).toBe(0);
    expect(control.stdout).toContain("the control case is c02; minus leaves out the case's line");
    // old, minus and twin, the false-loss control's second copy of old, 40 runs each.
    expect(requestsSince(from)).toHaveLength(240);
    expect(passes(result(s.out, "c02"), "control")).toEqual({ old: 40, minus: 0, twin: 40 });

    const reported = await cli(report(s));
    expect(reported.stderr).toBe("");
    expect(reported.code).toBe(0);
    const lines = reported.stdout.split("\n");
    expect(lines).toContain(
      "case c00: dropped at screen: the judge controls do not hold: the violating step is judged pass (fail wanted); screen: not run"
    );
    expect(lines).toContain(
      "case c01: dropped at screen: the judge controls do not hold: the real step is judged fail (pass wanted); screen: not run"
    );
    expect(lines).toContain(
      "case c02: screen: old 10/10, new 0/10, none 0/10: informative, flagged (3 runs with no verdict, run again); confirm: old 40/40, new 0/40: a loss; control: old 40/40, minus 0/40, twin 40/40: the planted-loss control (old against minus) is a loss, the false-loss control (old against twin) is no loss"
    );
    expect(lines).toContain(
      "case c03: screen: unscreened: an arm lacks its 10 judged runs after 5 extra runs (45 runs with no verdict, run again)"
    );
    expect(lines).toContain(
      "case c04: screen: old 10/10, new 0/10, none 0/10: informative, flagged; confirm: old 40/40, new 0/40: a loss"
    );
    expect(lines).toContain("case c09: not run");
    expect(lines).toContain(
      `cases: 6 screened, 6 informative, 6 flagged, 6 confirmed losses; 2 dropped (2 by the judge controls, 0 model-not-served, 0 unreadable-directory, ${NO_LEGION_TOOL}, 0 listing-misses, 0 listing-adds, ${NO_INJECTION}, ${NO_PATH_FORM}, ${NO_READS_PAST}, ${NO_REFUSED}), 1 unscreened, 1 not run`
    );
    expect(lines).toContain(
      `judge controls failed, by control (a case can fail more than one): the real step not judged pass 1, the violating step not judged fail 1, the reply "${NOTHING_REPLY}" not judged fail 0`
    );
    expect(lines).toContain("control case: c02");
    expect(lines).toContain(
      `verdict: loss: in c02, c04, c05, c06, c07, c08 the cut's text passes at least 10 fewer of 40 fresh runs than the old text, and the planted-loss control (case c02) is a loss and its false-loss control is no loss${unmeasured(6, 6)}`
    );
    // Each informative case passed old 10, new 0 and none 0 of its screen runs.
    const each = falseLossChance({ old: 10, new: 0, none: 0 });
    const percent = (x: number) => `${(100 * x).toFixed(2)}%`;
    expect(each).toBeGreaterThan(0);
    expect(reported.stdout).toContain(
      `false-loss rate at the cases run (an estimate): ${percent(1 - (1 - each) ** 6)}, `
    );
    expect(reported.stdout).toContain(
      `(c02 ${percent(each)}, c04 ${percent(each)}, c05 ${percent(each)}, c06 ${percent(each)}, c07 ${percent(each)}, c08 ${percent(each)})`
    );
    expect(lines).toContain(
      `  old 0.9: ${[0.8, 0.7, 0.6, 0.5].map((cut) => `new ${cut} ${percent(lossChance({ old: 0.9, cut, none: 0.1 }))}`).join(", ")}`
    );
    expect(lines).toContain(
      "digests: 23 of 23 captures' system-prompt digests differ from their session's"
    );

    // The request log: one line per request with its settings and usage, never the key and never
    // a message's text.
    const log = readFileSync(path.join(s.out, "requests.jsonl"), "utf8");
    expect(log).not.toContain(key);
    expect(log).not.toContain(LINE);
    expect(log).not.toContain(PASS_STEP);
    const logged = log
      .trim()
      .split("\n")
      .map((line) => JSON.parse(line));
    expect(logged).toHaveLength(435 + 6 * 160 + 240);
    // Each line says when its call was sent, how many were in flight then, the session id it went
    // with, and the reply's model, ids, gateway headers and cache tokens.
    const replayLine = logged.find((line) => line.kind === "replay" && line.usage);
    expect(replayLine.answeredBy).toBe("claude-sonnet-5-5");
    expect(replayLine.messageId).toMatch(/^msg_standin_\d+$/);
    expect(replayLine.requestId).toMatch(/^req_standin_\d+$/);
    expect(replayLine.providerRequestId).toBe(replayLine.requestId);
    expect(replayLine.gateway).toEqual({ "request-id": replayLine.requestId });
    expect([replayLine.cacheRead, replayLine.cacheWrite]).toEqual([0, 0]);
    expect(replayLine.userId).toMatch(/^\{"session_id":"[0-9a-f-]{36}"\}$/);
    expect(Date.parse(replayLine.sentAt)).toBeGreaterThanOrEqual(Date.parse(replayLine.at));
    for (const line of logged) {
      expect(line.inFlight === null || (line.inFlight >= 1 && line.inFlight <= 8)).toBe(true);
      expect(typeof line.userId).toBe("string");
    }
    expect(logged.filter((line) => line.inFlight > 1).length).toBeGreaterThan(0);
    // Every replay's and judgement's line names an id no other run's line names.
    const replayIds = logged
      .filter((line) => line.kind === "replay" || line.kind === "judge")
      .map((line) => line.userId);
    expect(new Set(replayIds).size).toBe(replayIds.length);
    expect(logged.filter((line) => line.status === 400)).toHaveLength(48);
    const judge = loadPrompt("judge").settings;
    expect(logged.find((line) => line.kind === "judge")).toMatchObject({
      model: judge.model,
      thinking: "enabled",
      thinking_budget: judge.thinking_budget,
      max_tokens: judge.max_tokens,
      temperature: judge.temperature,
      effort: judge.effort,
      usage: { input_tokens: 10, output_tokens: 3 },
    });
    expect(logged.find((line) => line.kind === "replay" && line.usage)).toMatchObject({
      model: "claude-sonnet-5-5",
      thinking: "between_tools",
      max_tokens: 128000,
    });
  }, 300_000);

  test("a run started again skips each complete case and sends no request for it", async () => {
    const s = scenario("resume", LOSS_SKILL(), [{ id: "c04" }, { id: "c05" }]);
    recordCaptures(s, ["old", "new", "none"]);
    answer = behaviourStandIn;
    usage = { input: 10, output: 3 };
    const early = await cli(behaviour("confirm", s));
    expect(early.code).toBe(2);
    expect(early.stderr).toContain("the screen is not finished: case c04 is not screened yet");

    let from = recorder.requests.length;
    const screen = await cli(behaviour("screen", s));
    expect(screen.code).toBe(0);
    expect(requestsSince(from)).toHaveLength(2 * 63);
    // Each case's line ends with the seconds of each of its phases; its captures are read back,
    // and no GitHub read is made.
    const sec = "\\d+\\.\\d";
    const escaped = (text: string) => text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    for (const id of ["c04", "c05"]) {
      expect(screen.stdout).toMatch(
        new RegExp(
          `^behaviour screen: case ${id}: screen: [^\\n]*; peak memory \\d+ MiB; seconds: judge controls ${sec}, captures ${sec} \\(old ${sec}, new ${sec}, none ${sec}; context 0\\.0; 0 GitHub reads, 0\\.0\\), replays ${sec}, judge ${sec}, write ${sec}, other ${sec}, total ${sec}; written to ${escaped(caseResultFile(s.out, id))}$`,
          "m"
        )
      );
    }
    const c04 = readFileSync(caseResultFile(s.out, "c04"), "utf8");

    from = recorder.requests.length;
    const again = await cli(behaviour("screen", s));
    expect(again.code).toBe(0);
    expect(again.stdout).toContain("case c04: screened before, skipped");
    expect(again.stdout).toContain("case c05: screened before, skipped");
    expect(requestsSince(from)).toHaveLength(0);

    rmSync(caseResultFile(s.out, "c05"));
    from = recorder.requests.length;
    const rerun = await cli(behaviour("screen", s));
    expect(rerun.code).toBe(0);
    expect(requestsSince(from)).toHaveLength(63);
    expect(readFileSync(caseResultFile(s.out, "c04"), "utf8")).toBe(c04);
    expect(result(s.out, "c05").stages.screen?.state).toBe("counted");

    // A result whose stage was run before each run had its own session id and the arms were
    // interleaved (it records no `sends`) is refused, so no old run is counted with new ones.
    const before = JSON.parse(readFileSync(caseResultFile(s.out, "c05"), "utf8"));
    delete before.stages.screen.sends;
    writeFileSync(caseResultFile(s.out, "c05"), JSON.stringify(before));
    from = recorder.requests.length;
    const stale = await cli(behaviour("screen", s));
    expect(stale.code).toBe(2);
    expect(stale.stderr).toContain(
      "holds screen runs sent before each run had its own session id and a round's arms were interleaved, so they are no runs of this procedure: use a new --out"
    );
    expect(requestsSince(from)).toHaveLength(0);
  }, 120_000);

  test("the budget stops new calls, and a run started again goes on from the runs saved", async () => {
    const s = scenario("budget", LOSS_SKILL(), [{ id: "c04" }]);
    recordCaptures(s, ["old", "new", "none"]);
    answer = behaviourStandIn;
    // 100,000 prompt tokens a reply: $0.40 a judgement at claude-opus-5-5's list price, $0.20 a
    // replay at claude-sonnet-5-5's.
    usage = { input: 100_000, output: 3 };
    let from = recorder.requests.length;
    const stopped = await cli(behaviour("screen", s, "--budget-usd", "1", "--concurrency", "1"));
    expect(stopped.code).toBe(3);
    expect(stopped.stderr).toContain("the budget is spent");
    // The three judge controls (to $1.20), then no new call starts: no replay is made.
    expect(requestsSince(from)).toHaveLength(3);
    const logged = readFileSync(path.join(s.out, "requests.jsonl"), "utf8")
      .trim()
      .split("\n")
      .map((line) => JSON.parse(line));
    expect(logged.filter((line) => line.usage !== undefined)).toHaveLength(3);
    expect(
      logged
        .filter((line) => line.usage === undefined)
        .every((line) => line.error === "BudgetSpent")
    ).toBe(true);
    const saved = result(s.out, "c04");
    expect(saved.controls?.hold).toBe(true);
    expect(saved.stages.screen).toMatchObject({ state: "running", runs: [] });

    usage = { input: 10, output: 3 };
    from = recorder.requests.length;
    const resumed = await cli(behaviour("screen", s));
    expect(resumed.stderr).toBe("");
    expect(resumed.code).toBe(0);
    // The controls are not judged again: 30 replays and their 30 judgements.
    expect(requestsSince(from)).toHaveLength(60);
    expect(result(s.out, "c04").stages.screen?.state).toBe("counted");
  }, 120_000);

  test("a cut that keeps the line keeps it, and a control that leaves out a line the cases do not need gives no verdict", async () => {
    const s = scenario(
      "keeps",
      f1File(2),
      ["c04", "c05", "c06", "c07", "c08", "c09"].map((id) => ({ id }))
    );
    recordCaptures(s, ["old", "new", "none", "minus", "twin"]);
    answer = behaviourStandIn;
    usage = { input: 10, output: 3 };
    expect((await cli(behaviour("screen", s))).code).toBe(0);
    let from = recorder.requests.length;
    const confirm = await cli(behaviour("confirm", s));
    expect(confirm.code).toBe(0);
    expect(confirm.stdout).toContain("0 flagged cases");
    expect(requestsSince(from)).toHaveLength(0);

    const planted = { ...s, out: path.join(s.dir, "out-planted") };
    cpSync(s.out, planted.out, { recursive: true });
    recordCaptures(s, ["minus"], OTHER_LINE, planted.out);
    // A false-loss control that is a loss: the twin's capture made to fail every run (its body is
    // minus's, which lacks the line), as a procedure that tells the two copies of old apart would.
    const falseLoss = { ...s, out: path.join(s.dir, "out-false-loss") };
    cpSync(s.out, falseLoss.out, { recursive: true });
    const c04 = s.cases.find((each) => each.id === "c04") ?? makeCase("c04");
    const dirs = { old: s.old, new: s.new };
    const texts = { old: textDigest(s.old), new: textDigest(s.new) };
    const captureOf = (name: ArmName) =>
      capturePaths(
        armCaptureDir(
          falseLoss.out,
          buildArm(c04, name, dirs, path.join(s.dir, "arms", "c04")),
          texts
        ),
        "c04",
        name
      ).body;
    copyFileSync(captureOf("minus"), captureOf("twin"));

    from = recorder.requests.length;
    const held = await cli(behaviour("control", s));
    expect(held.code).toBe(0);
    // old, minus and twin: 40 replays and 40 judgements each.
    expect(requestsSince(from)).toHaveLength(240);
    expect(passes(result(s.out, "c04"), "control")).toEqual({ old: 40, minus: 0, twin: 40 });
    const kept = (await cli(report(s))).stdout.split("\n");
    expect(kept).toContain(
      "case c04: screen: old 10/10, new 10/10, none 0/10: informative, not flagged; control: old 40/40, minus 0/40, twin 40/40: the planted-loss control (old against minus) is a loss, the false-loss control (old against twin) is no loss"
    );
    expect(kept).toContain(
      "controls, scored by the confirm rule (a loss when the other arm passes at least 10 fewer of 40 fresh runs than old): old 40/40, minus 0/40, twin 40/40; the planted-loss control (old less minus, 40) is a loss, as it must be; the false-loss control (old less twin, the same text twice, 0) is no loss, as it must be"
    );
    expect(kept).toContain(
      `verdict: no loss detected by the registered rule: no confirmed case is a loss (0 flagged of 6 informative), and the planted-loss control (case c04) is a loss and its false-loss control is no loss${unmeasured(6, 6)}`
    );

    from = recorder.requests.length;
    const control = await cli(behaviour("control", planted, "--planted-line", OTHER_LINE));
    expect(control.code).toBe(0);
    expect(control.stdout).toContain(`minus leaves out the planted line "${OTHER_LINE}"`);
    expect(requestsSince(from)).toHaveLength(240);
    expect(passes(result(planted.out, "c04"), "control")).toEqual({
      old: 40,
      minus: 40,
      twin: 40,
    });
    expect((await cli(report(planted))).stdout.split("\n")).toContain(
      `verdict: none: control not a loss${unmeasured(6, 6)}`
    );
    // A control made with one planted line is not taken for one made with another.
    const other = await cli(behaviour("control", planted));
    expect(other.code).toBe(2);
    expect(other.stderr).toContain("were made with other skill texts");

    // The false-loss control is a loss: the skill gets no verdict, and the report says why.
    // Another case's result is a pipe whose writer, once the command opens it, waits 1.5 s before
    // it writes, so reading the screen's results takes that long: the control case's seconds
    // start after it.
    // Catches: the control case's "other" and total counting the read of every case's result.
    const slowResult = caseResultFile(falseLoss.out, "c05");
    const resultText = `${slowResult}.text`;
    writeFileSync(resultText, readFileSync(slowResult));
    rmSync(slowResult);
    expect(Bun.spawnSync(["mkfifo", slowResult]).exitCode).toBe(0);
    const writer = Bun.spawn([
      "sh",
      "-c",
      'exec 3>"$1"; sleep 1.5; cat "$0" >&3',
      resultText,
      slowResult,
    ]);
    const falseControl = await cli(behaviour("control", falseLoss));
    await writer.exited;
    rmSync(slowResult);
    copyFileSync(resultText, slowResult);
    expect(falseControl.code).toBe(0);
    const controlLine = falseControl.stdout.split("\n").find((line) => line.includes("case c04: "));
    // No default: with no such line, or another format, Number(undefined) is NaN and fails.
    const [, otherSeconds] = /, other (\d+\.\d), total/.exec(controlLine ?? "") ?? [];
    expect(Number(otherSeconds)).toBeLessThan(1);
    expect(passes(result(falseLoss.out, "c04"), "control")).toEqual({
      old: 40,
      minus: 0,
      twin: 0,
    });
    const refused = (await cli(report(falseLoss))).stdout.split("\n");
    expect(refused).toContain(`verdict: none: false-loss control a loss${unmeasured(6, 6)}`);
    expect(refused).toContain(
      "controls, scored by the confirm rule (a loss when the other arm passes at least 10 fewer of 40 fresh runs than old): old 40/40, minus 0/40, twin 0/40; the planted-loss control (old less minus, 40) is a loss, as it must be; the false-loss control (old less twin, the same text twice, 40) is a loss, so the skill gets no verdict"
    );
  }, 300_000);

  test("stops above 1 GiB of peak resident memory, once the case is saved", async () => {
    const s = scenario("memory", LOSS_SKILL(), [{ id: "c04" }, { id: "c05" }]);
    recordCaptures(s, ["old", "new", "none"]);
    answer = behaviourStandIn;
    usage = { input: 10, output: 3 };
    const err: string[] = [];
    const io: CliIo = {
      out: () => {},
      err: (line) => err.push(line),
      peakRssBytes: () => 1024 ** 3 + 1,
    };
    expect(await main(behaviour("screen", s), io)).toBe(3);
    expect(err.join("\n")).toContain("is above 1 GiB, so the run stops after case c04");
    expect(result(s.out, "c04").peakRssBytes).toBe(1024 ** 3 + 1);
    expect(existsSync(caseResultFile(s.out, "c05"))).toBe(false);
  }, 120_000);

  /** One screen of three cases, made once for the rows below: on openai/gpt-6.1-sol, on an
   *  Anthropic model behind another gateway prefix, and on a model the gateway does not serve
   *  (F7-skill-cuts.json's servedModels: anthropic/claude-sonnet-5-5, openai/gpt-6.1-sol). */
  let servedRun:
    | Promise<{ s: Scenario; ran: Ran; requests: readonly RecordedRequest[] }>
    | undefined;
  const served = () => {
    servedRun ??= (async () => {
      const s = scenario("served", LOSS_SKILL(), [
        { id: "c-openai", model: "openai/gpt-6.1-sol" },
        { id: "c-prefixed", model: "gateway/claude-sonnet-5-5" },
        { id: "c-unserved", model: "anthropic/claude-opus-5-5" },
      ]);
      recordCaptures(s, ["old", "new", "none"]);
      answer = behaviourStandIn;
      usage = { input: 10, output: 3 };
      const from = recorder.requests.length;
      const ran = await cli(behaviour("screen", s));
      return { s, ran, requests: requestsSince(from) };
    })();
    return servedRun;
  };

  test("a case on openai/gpt-6.1-sol runs: its replays go to the Responses route on its own model, and its judge stays the pinned one", async () => {
    const { s, ran, requests } = await served();
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    // The two served cases, 63 requests each: three controls, 30 replays, 30 judgements.
    expect(requests).toHaveLength(2 * 63);
    const c = result(s.out, "c-openai");
    expect(c.dropped).toBeNull();
    expect(c.stages.screen?.state).toBe("counted");
    expect(passes(c, "screen")).toEqual({ old: SCREEN_RUNS, new: 0, none: 0 });
    const responses = requests.filter((request) => request.path === "/v1/responses");
    expect(responses).toHaveLength(30);
    for (const request of responses) {
      const body = JSON.parse(request.body);
      expect(body.model).toBe("gpt-6.1-sol");
      expect(body.input[0].content[1].text).toContain("Case c-openai");
      expect(request.body).not.toContain("cache_control");
      expect(Object.keys(request.headers).filter((name) => name.startsWith("anthropic-"))).toEqual(
        []
      );
    }
    const judge = loadPrompt("judge").settings;
    const judged = requests.filter((request) => isJudgement(JSON.parse(request.body)));
    expect(judged).toHaveLength(2 * 33);
    expect(judged.every((request) => request.path === "/v1/messages")).toBe(true);
    expect(judged.every((request) => JSON.parse(request.body).model === judge.model)).toBe(true);
    // Each run keeps its reply's output items and what the replay used, its reasoning counted.
    const run = c.stages.screen?.runs[0];
    expect(run?.reply?.map((block) => block.type)).toEqual(["message"]);
    expect(run?.replaySpent[0]).toMatchObject({ model: "gpt-6.1-sol", reasoning: 0 });
    // The request log records a Responses body's max_output_tokens and reasoning effort.
    const logged = readFileSync(path.join(s.out, "requests.jsonl"), "utf8")
      .trim()
      .split("\n")
      .map((line) => JSON.parse(line));
    expect(
      logged.find((line) => line.kind === "replay" && line.model === "gpt-6.1-sol")
    ).toMatchObject({ max_tokens: 128000, effort: "low", stream: true, stopReason: "completed" });
  }, 300_000);

  test("a case on an Anthropic model behind another gateway prefix runs, as the served model of its id, on the Messages route", async () => {
    const { s, requests } = await served();
    const c = result(s.out, "c-prefixed");
    expect(c.dropped).toBeNull();
    expect(passes(c, "screen")).toEqual({ old: SCREEN_RUNS, new: 0, none: 0 });
    const replays = requests.filter(
      (request) => request.path === "/v1/messages" && !isJudgement(JSON.parse(request.body))
    );
    expect(replays.filter((request) => request.body.includes("Case c-prefixed"))).toHaveLength(30);
    for (const request of replays) {
      expect(JSON.parse(request.body).model).toBe("claude-sonnet-5-5");
      expect(request.headers["anthropic-beta"]).toBe(F2_HEADERS["anthropic-beta"] ?? "");
    }
  }, 300_000);

  test("a case on a model the gateway does not serve is dropped as model-not-served, before any call, and the report counts it", async () => {
    const { s, ran, requests } = await served();
    const c = result(s.out, "c-unserved");
    expect(c.dropped).toBe(
      "model-not-served: its model anthropic/claude-opus-5-5 is none the gateway serves (the configuration's servedModels: anthropic/claude-sonnet-5-5, openai/gpt-6.1-sol)"
    );
    expect(c.controls).toBeNull();
    expect(c.droppedAt).toBe("screen");
    expect(c.stages.screen).toBeUndefined();
    expect(requests.filter((request) => request.body.includes("Case c-unserved"))).toEqual([]);
    expect(ran.code).toBe(0);
    const reported = await cli(report(s));
    expect(reported.code).toBe(0);
    expect(reported.stdout).toContain(
      "case c-unserved: dropped at screen: model-not-served: its model anthropic/claude-opus-5-5 is none the gateway serves"
    );
    expect(reported.stdout).toContain(
      `; 1 dropped (0 by the judge controls, 1 model-not-served, 0 unreadable-directory, ${NO_LEGION_TOOL}, 0 listing-misses, 0 listing-adds, ${NO_INJECTION}, ${NO_PATH_FORM}, ${NO_READS_PAST}, ${NO_REFUSED}), `
    );
  }, 300_000);

  // Catches: a case whose context holds a directory no read can give ending the whole screen,
  // read again by every run started again, or read from the whole repository's tarball.
  test("a case whose context needs a directory GitHub cuts short even read alone is dropped as unreadable-directory, with no tarball read; a run started again reads nothing for it, and report counts it", async () => {
    const s = scenario("unreadable", LOSS_SKILL(), [{ id: "c04" }]);
    answer = behaviourStandIn;
    const commit = s.cases[0]?.profile.skills[1]?.commit ?? "";
    // acme/skills at the case's commit, as GitHub lists a repository of too many entries: its
    // recursive tree cut short, then skills/beta's, and skills/beta's one level even read alone.
    const bin = path.join(s.dir, "bin");
    const log = path.join(s.dir, "gh-calls.jsonl");
    mkdirSync(bin, { recursive: true });
    writeGhStandIn(path.join(bin, "gh"), log, truncatedSkillsTree(commit));
    const { omp, models } = standInContext(s, "c04", bin);
    const env = { ...ENV, PATH: `${bin}:${ENV.PATH ?? ""}` };
    const args = behaviour("screen", s, "--models", models, "--omp", omp);
    const routes = () => readFileSync(log, "utf8").split("\n").filter(Boolean);

    const first = await cli(args, env);
    expect({ code: first.code, stderr: first.stderr }).toEqual({ code: 0, stderr: "" });
    const dropped = `unreadable-directory: acme/skills at ${commit}: skills/beta cannot be read: GitHub cuts the tree of skills/beta short even one level at a time`;
    const c = result(s.out, "c04");
    expect(c.dropped).toBe(dropped);
    expect(c.controls?.hold).toBe(true);
    expect(c.stages).toEqual({});
    expect(c.droppedAt).toBe("screen");
    expect(first.stdout).toContain(`case c04: dropped: ${dropped};`);
    expect(routes().filter((route) => route.includes("/tarball/"))).toEqual([]);
    expect(routes()).toHaveLength(5);

    const before = recorder.requests.length;
    const again = await cli(args, env);
    expect({ code: again.code, stderr: again.stderr }).toEqual({ code: 0, stderr: "" });
    expect(again.stdout).toContain(`case c04: dropped before, skipped: ${dropped}`);
    expect(routes()).toHaveLength(5);
    expect(requestsSince(before)).toEqual([]);

    const reported = await cli(report(s));
    expect(reported.code).toBe(0);
    expect(reported.stdout).toContain(`case c04: dropped at screen: ${dropped}; screen: not run\n`);
    expect(reported.stdout).toContain(
      `; 1 dropped (0 by the judge controls, 0 model-not-served, 1 unreadable-directory, ${NO_LEGION_TOOL}, 0 listing-misses, 0 listing-adds, ${NO_INJECTION}, ${NO_PATH_FORM}, ${NO_READS_PAST}, ${NO_REFUSED}), `
    );
  }, 120_000);

  // Catches: a Legion pane's capture whose pi-legion defined no legion tool in either attempt
  // ending the whole screen in place of its case, or a drop of it that report does not count.
  test("a case whose Legion pane's capture defines no legion tool in either of its two attempts is dropped as legion-tool-undefined, naming each attempt's stderr lines; a run started again skips it, and report counts it", async () => {
    const s = scenario("legion-tool", LOSS_SKILL(), [{ id: "c04" }]);
    answer = behaviourStandIn;
    const commit = "0123456789abcdef";
    // c04 in a Legion implementer pane whose profile installs pi-legion as an extension: a module
    // that does nothing, since the stand-in Oh My Pi below loads no extension.
    const [c04] = s.cases;
    const legionCase = {
      ...c04,
      context: "legion:acme:implementer",
      profile: {
        ...c04?.profile,
        plugins: [{ repo: "acme/legion", commit, path: "packages/pi-legion", name: "pi-legion" }],
        appendSystemPrompt: "You are a made-up role of a made-up project.",
      },
    };
    writeFileSync(s.casesFile, `${JSON.stringify(legionCase)}\n`);
    const repos = path.join(s.dir, "repos");
    const beta = path.join(repos, "acme", "skills", commit, "skills/beta");
    writeSkill(beta, {
      "SKILL.md": "---\nname: beta\ndescription: Use for the beta greeting.\n---\n",
    });
    writeSkill(path.join(repos, "acme", "legion", commit, "packages/pi-legion"), {
      "package.json": JSON.stringify({
        name: "@acme/pi-legion",
        files: ["dist"],
        omp: { extensions: ["dist/legion.js"] },
      }),
      "dist/legion.js": "export default function legion() {}\n",
    });
    // Oh My Pi's stand-in, in place of standInContext's: it answers --version, gives the natives
    // cache (omp-natives.ts) the one native its comment says it embeds, and as the capture run
    // sends the turn's request with two tools and no legion tool, and writes on stderr a line of
    // the plugin's, one of Oh My Pi's on a session start that threw, and one of neither.
    const bin = path.join(s.dir, "bin");
    mkdirSync(bin, { recursive: true });
    const { models } = standInContext(s, "c04", bin);
    writeFileSync(
      models,
      "providers:\n  anthropic:\n    baseUrl: http://127.0.0.1:9\n    api: anthropic-messages\n    models:\n      - {id: claude-sonnet-5-5}\n"
    );
    const said = [
      "[legion] stand-in: the daemon refused the registration",
      "Extension error (dist/legion.js): Error: the stand-in's session start failed",
    ];
    const turn = path.join(bin, "turn.ts");
    writeFileSync(
      turn,
      `await (await fetch(process.env.LEGION_DAEMON_URL + "/v1/messages", {
  method: "POST",
  headers: { "content-type": "application/json" },
  body: JSON.stringify({
    model: "claude-sonnet-5-5",
    system: [{ type: "text", text: "A stand-in system prompt." }],
    tools: [{ name: "read" }, { name: "bash" }],
    messages: [{ role: "user", content: "A stand-in turn." }],
  }),
})).text();
console.error(${JSON.stringify(said[0])});
console.error("a stand-in line of neither");
console.error(${JSON.stringify(said[1])});
`
    );
    const omp = path.join(bin, "omp");
    writeFileSync(
      omp,
      `#!/bin/sh
# filename: "pi_natives.linux-x64-modern.node", size: 5,
case "$1" in
--version) echo 'omp 0.0.0 (stand-in)' ;;
config) mkdir -p "$HOME/.omp/natives/0.0.0" && printf stand >"$HOME/.omp/natives/0.0.0/pi_natives.linux-x64-modern.node" ;;
*) exec ${JSON.stringify(process.execPath)} ${JSON.stringify(turn)} ;;
esac
`,
      { mode: 0o755 }
    );
    // Each listed skill's hint in the cache, as a capture before would have left it, so the
    // capture makes no hint run.
    for (const [name, dir] of [
      ["alpha", s.old],
      ["beta", beta],
    ] as const) {
      const { description } = skillFrontmatter(dir);
      const key = cacheKey("omp 0.0.0 (stand-in)", name, description);
      writeSkill(path.join(s.dir, "cache", "hints"), {
        [`${key}.json`]: JSON.stringify({
          omp: "omp 0.0.0 (stand-in)",
          name,
          description,
          ompKey: cacheKey("made-up Oh My Pi key", name),
          hint: `Use for the ${name} work.`,
          model: "anthropic/claude-sonnet-5-5",
          writtenAt: "2026-11-22T00:00:00.000Z",
          kept: true,
        }),
      });
    }
    // The natives cache under the row's directory, on the scratch's filesystem.
    const env = { ...ENV, XDG_CACHE_HOME: path.join(s.dir, "xdg-cache") };
    const args = behaviour("screen", s, "--models", models, "--omp", omp, "--repos", repos);

    const first = await cli(args, env);
    expect({ code: first.code, stderr: first.stderr }).toEqual({ code: 0, stderr: "" });
    const lines = said.map((line) => JSON.stringify(line)).join(" ");
    const attempt = (n: number) =>
      `attempt ${n}: its body defines 2 tools, legion not among them, and its stderr's [legion] and Extension error lines are ${lines}`;
    const dropped = `legion-tool-undefined: case c04 arm old: the captured body is refused: a Legion implementer pane whose context installs pi-legion as an extension must send pi-legion's legion tool, and the body of none of its 2 attempts defines it: ${attempt(1)}; ${attempt(2)}`;
    const c = result(s.out, "c04");
    expect([c.dropped, c.droppedAt, c.controls?.hold, c.stages]).toEqual([
      dropped,
      "screen",
      true,
      {},
    ]);
    expect(first.stdout).toContain(`case c04: dropped: ${dropped};`);

    const again = await cli(args, env);
    expect({ code: again.code, stderr: again.stderr }).toEqual({ code: 0, stderr: "" });
    expect(again.stdout).toContain(`case c04: dropped before, skipped: ${dropped}`);

    const reported = await cli(report(s));
    expect(reported.code).toBe(0);
    expect(reported.stdout).toContain(`case c04: dropped at screen: ${dropped}; screen: not run\n`);
    expect(reported.stdout).toContain(
      `; 1 dropped (0 by the judge controls, 0 model-not-served, 0 unreadable-directory, 1 legion-tool-undefined, 0 listing-misses, 0 listing-adds, ${NO_INJECTION}, ${NO_PATH_FORM}, ${NO_READS_PAST}, ${NO_REFUSED}), `
    );
  }, 120_000);

  /** Writes `count` as the reading sessions by model of `s` (attribute's
   *  `<cut id>/models/<skill>.json`) to `name` in its directory, and returns that file. */
  const readingModels = (s: Scenario, name: string, count: object) => {
    const file = path.join(s.dir, name);
    writeFileSync(file, JSON.stringify(count));
    return file;
  };

  test("with --reading-models, screen refuses a skill too few of whose reading sessions are on served models, before any call, and report gives it no verdict", async () => {
    const s = scenario("reading-few", LOSS_SKILL(), [{ id: "c00" }]);
    const file = readingModels(s, "reading-models.json", {
      sessions: 8,
      byModel: [
        { model: "anthropic/claude-sonnet-5-5", anthropic: true, served: true, sessions: 5 },
        { model: "acme-gateway/gpt-6.1", anthropic: false, served: false, sessions: 3 },
      ],
      noModel: 0,
      notQueried: 0,
      notAnthropic: 3,
      served: 5,
      withModel: 8,
    });
    const reason =
      "too few sessions on served models: 5 of its 8 reading sessions are on a model the gateway serves, fewer than 6";
    const from = recorder.requests.length;
    const screen = await cli(behaviour("screen", s, "--reading-models", file));
    expect(screen.code).toBe(2);
    expect(screen.stderr).toContain(
      `the skill alpha gets no verdict, and no case of it is screened: ${reason} (--reading-models ${file}); the walk of the cut's ranking goes on to its next skill`
    );
    expect(existsSync(caseResultFile(s.out, "c00"))).toBe(false);
    const reported = await cli([...report(s), "--reading-models", file]);
    expect(reported.code).toBe(0);
    expect(reported.stdout.split("\n")).toContain(`verdict: none: ${reason}`);
    // A file that is no skill's reading sessions by model is refused, before any call too.
    const bad = readingModels(s, "not-reading-models.json", { sessions: 8 });
    const refused = await cli(behaviour("screen", s, "--reading-models", bad));
    expect(refused.code).toBe(2);
    expect(refused.stderr).toContain(
      `--reading-models ${bad} is not a skill's reading sessions by model`
    );
    expect(requestsSince(from)).toHaveLength(0);
  }, 60_000);

  test("report --reading-models prints the reading sessions by model and the screened cases' models, and the verdict names the reading sessions it speaks for", async () => {
    const s = scenario("reading-models", f1File(2), [
      { id: "c04" },
      { id: "c05" },
      { id: "c06" },
      { id: "c07" },
      { id: "c08", model: "gateway/claude-sonnet-5-5" },
      { id: "c09", model: "openai/gpt-6.1-sol" },
    ]);
    recordCaptures(s, ["old", "new", "none", "minus", "twin"]);
    answer = behaviourStandIn;
    usage = { input: 10, output: 3 };
    const sonnet = {
      model: "anthropic/claude-sonnet-5-5",
      anthropic: true,
      served: true,
      sessions: 6,
    };
    const others = [
      { model: "acme-gateway/gpt-6.1", anthropic: false, served: false, sessions: 2 },
      { model: "openai/gpt-6.1-sol", anthropic: false, served: true, sessions: 2 },
      { model: "gateway/claude-sonnet-5-5", anthropic: true, served: true, sessions: 1 },
    ];
    const file = readingModels(s, "reading-models.json", {
      sessions: 13,
      byModel: [sonnet, ...others],
      noModel: 1,
      notQueried: 1,
      notAnthropic: 4,
      served: 9,
      withModel: 11,
    });
    // 9 of the 13 reading sessions are on served models: the screen runs.
    const screen = await cli(behaviour("screen", s, "--reading-models", file));
    expect({ code: screen.code, stderr: screen.stderr }).toEqual({ code: 0, stderr: "" });
    expect((await cli(behaviour("control", s))).code).toBe(0);
    const keeps =
      "verdict: no loss detected by the registered rule: no confirmed case is a loss (0 flagged of 6 informative), and the planted-loss control (case c04) is a loss and its false-loss control is no loss";

    // Without --reading-models the report says so, and its verdict names no models.
    const plain = (await cli(report(s))).stdout.split("\n");
    expect(plain).toContain(
      "reading sessions by model: not given (no --reading-models), so the verdict names no models"
    );
    expect(plain).toContain(`${keeps}${unmeasured(6, 6)}`);

    // c08's model counts under the served model of its id, and so do its reading sessions.
    const reported = await cli([...report(s), "--reading-models", file]);
    expect({ code: reported.code, stderr: reported.stderr }).toEqual({ code: 0, stderr: "" });
    const lines = reported.stdout.split("\n");
    for (const line of [
      `reading sessions by model (--reading-models ${file}): 13 sessions`,
      "  anthropic/claude-sonnet-5-5: 6 sessions, served by the gateway",
      "  acme-gateway/gpt-6.1: 2 sessions, not served by the gateway",
      "  openai/gpt-6.1-sol: 2 sessions, served by the gateway",
      "  gateway/claude-sonnet-5-5: 1 session, served by the gateway",
      "  with no model (its file's head names none): 1; not queried (in no models query's rows): 1",
      "screened cases by the served model they ran on: anthropic/claude-sonnet-5-5 5 cases, openai/gpt-6.1-sol 1 case",
      `${keeps}; it speaks for the reading sessions on anthropic/claude-sonnet-5-5 (7 sessions), openai/gpt-6.1-sol (2 sessions); not for the 2 sessions on models the gateway does not serve${unmeasured(6, 6)}`,
    ]) {
      expect(lines).toContain(line);
    }

    // Under a configuration that serves a third model, which no screened case ran on, the
    // reading sessions on it are named apart.
    const { config } = setup();
    const fixture = JSON.parse(readFileSync(config, "utf8"));
    const opusConfig = path.join(s.dir, "skill-cuts-opus.json");
    writeFileSync(
      opusConfig,
      JSON.stringify({
        ...fixture,
        servedModels: [...fixture.servedModels, "anthropic/claude-opus-5-5"],
      })
    );
    const opus = readingModels(s, "reading-models-opus.json", {
      sessions: 16,
      byModel: [
        sonnet,
        { model: "anthropic/claude-opus-5-5", anthropic: true, served: true, sessions: 3 },
        ...others,
      ],
      noModel: 1,
      notQueried: 1,
      notAnthropic: 4,
      served: 12,
      withModel: 14,
    });
    const withOpus = await cli([
      "behaviour",
      "report",
      "--config",
      opusConfig,
      "--out",
      s.out,
      "--old",
      s.old,
      "--cases",
      s.casesFile,
      "--reading-models",
      opus,
    ]);
    expect(withOpus.stdout.split("\n")).toContain(
      `${keeps}; it speaks for the reading sessions on anthropic/claude-sonnet-5-5 (7 sessions), openai/gpt-6.1-sol (2 sessions); not for the 2 sessions on models the gateway does not serve; nor for the 3 sessions on served models no screened case ran on${unmeasured(6, 6)}`
    );
  }, 300_000);

  test("Oh My Pi is --omp, LEGION_OMP_PATH or the omp on PATH, as an absolute path, and behaviour and load given --models refuse by name when none names one", async () => {
    const bin = path.join(scratch, "omp-bin");
    mkdirSync(bin, { recursive: true });
    const fake = path.join(bin, "omp");
    writeFileSync(fake, "#!/bin/sh\nexit 0\n", { mode: 0o755 });
    expect(ompBinary(undefined, { PATH: `/nowhere:${bin}` })).toBe(fake);
    expect(ompBinary(undefined, { PATH: "/nowhere", LEGION_OMP_PATH: fake })).toBe(fake);
    expect(ompBinary(path.relative(process.cwd(), fake), { PATH: "/nowhere" })).toBe(fake);
    expect(ompBinary(undefined, { PATH: "/nowhere" })).toBeUndefined();
    expect(ompBinary(path.join(bin, "missing"), { PATH: bin })).toBeUndefined();

    const none = { ...ENV, PATH: "/nowhere", LEGION_OMP_PATH: "" };
    const s = scenario("omp-missing", LOSS_SKILL(), [{ id: "c00" }]);
    const models = path.join(s.dir, "models.yml");
    writeFileSync(models, "providers: {}\n");
    const screen = await cli(behaviour("screen", s, "--models", models), none);
    expect(screen.code).toBe(2);
    expect(screen.stderr).toContain(
      "--models makes captures, which run Oh My Pi, and none is found: no --omp is given, LEGION_OMP_PATH is unset, and no omp is on PATH"
    );
    const { config, keyFile } = setup();
    const load = await cli(
      [
        "load",
        "--config",
        config,
        "--key-file",
        keyFile,
        "--skill",
        s.old,
        "--tasks",
        path.join(s.dir, "tasks.jsonl"),
        "--host",
        "omp",
        "--profile",
        path.join(s.dir, "profile.json"),
        "--out",
        path.join(s.dir, "load"),
        "--models",
        models,
        "--omp",
        path.join(bin, "missing"),
      ],
      none
    );
    expect(load.code).toBe(2);
    expect(load.stderr).toContain(`--omp ${path.join(bin, "missing")} names no file`);
  }, 60_000);

  test("the false-loss chance is the procedure's own, exact, and above 0 for an old text at 10 of 10", () => {
    /** The chance of each count of `runs` runs that each pass with chance `p`. */
    const pmf = (runs: number, p: number) => {
      const choose = (k: number) => {
        let ways = 1;
        for (let i = 1; i <= k; i += 1) ways = (ways * (runs - k + i)) / i;
        return ways;
      };
      return Array.from({ length: runs + 1 }, (_, k) => choose(k) * p ** k * (1 - p) ** (runs - k));
    };
    /** The rules table's figures, every outcome counted: of the screen's 10 runs a case is
     *  informative at old − none ≥ 2 and flagged at old − new ≥ 2 too, and of the confirm's 40
     *  fresh runs a loss at old − new ≥ 10. */
    const bruteForce = (old: number, cut: number, none: number) => {
      const [o, n, z] = [pmf(10, old), pmf(10, cut), pmf(10, none)];
      let informative = 0;
      let flagged = 0;
      for (let a = 0; a <= 10; a += 1) {
        for (let b = 0; b <= 10; b += 1) {
          for (let c = 0; c <= 10; c += 1) {
            const chance = (o[a] ?? 0) * (n[b] ?? 0) * (z[c] ?? 0);
            if (a - c < 2) continue;
            informative += chance;
            if (a - b >= 2) flagged += chance;
          }
        }
      }
      const [O, N] = [pmf(40, old), pmf(40, cut)];
      let loss = 0;
      for (let a = 0; a <= 40; a += 1) {
        for (let b = 0; b <= a - 10; b += 1) loss += (O[a] ?? 0) * (N[b] ?? 0);
      }
      return (flagged / informative) * loss;
    };
    /** Whether `exact` is `counted` but for rounding: a relative difference below 1e-9. */
    const agrees = (exact: number, counted: number) =>
      counted > 0 && Math.abs(exact - counted) / counted < 1e-9;
    // Old and cut pooled with a Laplace term, none with its own: (o + n + 1)/22 and (z + 1)/12.
    for (const passes of [
      { old: 7, new: 6, none: 2 },
      { old: 9, new: 4, none: 0 },
    ]) {
      const p = (passes.old + passes.new + 1) / 22;
      const counted = bruteForce(p, p, (passes.none + 1) / 12);
      expect({ passes, agrees: agrees(falseLossChance(passes), counted) }).toEqual({
        passes,
        agrees: true,
      });
    }
    // An old text that passed 10 of 10 still has a chance above 0: the pooled rate is 21/22, not
    // the screen's own rate of 1, at which no loss could happen.
    const full = falseLossChance({ old: 10, new: 10, none: 0 });
    expect(full).toBeGreaterThan(0);
    expect(agrees(full, bruteForce(21 / 22, 21 / 22, 1 / 12))).toBe(true);
    // A sensitivity point: the old text at 0.9, the cut's at 0.7, no skill at 0.1 (about 20%).
    const point = lossChance({ old: 0.9, cut: 0.7, none: 0.1 });
    expect(agrees(point, bruteForce(0.9, 0.7, 0.1))).toBe(true);
    expect(point).toBeGreaterThan(0.2);
    expect(point).toBeLessThan(0.21);
    expect(() => lossChance({ old: 0, cut: 0, none: 0.1 })).toThrow("no case is informative");
  });
});

/** A made-up drop of a case whose context needs a directory no read gives. */
const UNREADABLE =
  "unreadable-directory: acme/skills at 0123456789abcdef: skills/beta cannot be read: GitHub cuts the tree of skills/beta short even one level at a time";
/** The report's count of `legion-tool-undefined` drops when there is none. */
const NO_LEGION_TOOL = "0 legion-tool-undefined";
/** The report's count of `injection-unrebuildable` drops, by kind, when there is none. */
const NO_INJECTION =
  "0 injection-unrebuildable (user-invoked 0, other-shape 0, plugin-block-misplaced 0, autoload-no-block 0)";
/** The report's count of `path-form` drops, by arm, when there is none. */
const NO_PATH_FORM = "0 path-form (old 0, new 0)";
/** The report's count of `reads-past-last-round` drops when there is none. */
const NO_READS_PAST = `0 ${READS_PAST_LAST_ROUND}`;
/** The report's count of `provider-refused` drops when there is none. */
const NO_REFUSED = "0 provider-refused";

/** Run `n` (from 0) of `arm` in case `caseId`'s `stage`, as the runner records one, with `verdict`
 *  and `reason`. */
function plantedRun(
  caseId: string,
  arm: string,
  stage: Stage,
  n: number,
  verdict: "pass" | "fail" | null,
  reason: string
): Record<string, unknown> {
  return {
    caseId,
    arm,
    stage,
    verdict,
    reason,
    run: n + 1,
    round: 1,
    sendPlace: n,
    userId: null,
    id: null,
    skillRounds: null,
    reply: null,
    replaySpent: [],
    judgeSpent: [],
  };
}

/** This runner (cli.ts's `runnerDigest`), which each stage it starts and each run it makes
 *  records. */
const RUNNER = runnerDigest();

/** A finished stage of case `caseId`, made with the texts of `s` and by `runner` (this runner by
 *  default, as a stage command writes it), each arm of `passes` passing that many of the stage's
 *  judged runs. Its runs name no runner, as a run made before runs recorded theirs, so each takes
 *  its stage's. */
function plantedStage(
  s: Scenario,
  caseId: string,
  stage: Stage,
  passes: Record<string, number>,
  runner = RUNNER
): Record<string, unknown> {
  const runs = Object.entries(passes).flatMap(([arm, passed]) =>
    Array.from({ length: STAGE_RUNS[stage] }, (_, n) =>
      plantedRun(caseId, arm, stage, n, n < passed ? "pass" : "fail", "planted")
    )
  );
  return {
    state: "counted",
    sends: SENDS,
    texts: { old: textDigest(s.old), new: textDigest(s.new) },
    plantedLine: null,
    captures: {},
    seeds: ["planted"],
    runs,
    usd: 0,
    runner,
  };
}

/** Writes case `caseId`'s result under `s.out`, checked against cli.ts's CaseResult: `fields` over
 *  a result with no drop, no judge controls and no stage. */
function plantResult(s: Scenario, caseId: string, fields: Record<string, unknown>): void {
  const file = caseResultFile(s.out, caseId);
  mkdirSync(path.dirname(file), { recursive: true });
  const result = CaseResult.parse({
    caseId,
    skill: "alpha",
    cut: "acme-skills-1",
    dropped: null,
    controls: null,
    stages: {},
    peakRssBytes: null,
    ...fields,
  });
  writeFileSync(file, JSON.stringify(result));
}

/** The screen of a case that is informative and not flagged: old 10, new 10, none 0 of 10. */
const KEPT = { old: SCREEN_RUNS, new: SCREEN_RUNS, none: 0 };
/** The screen of a case that is informative and flagged: old 10, new 0, none 0 of 10. */
const FLAGGED = { old: SCREEN_RUNS, new: 0, none: 0 };

/** Plants under `s.out` the screens of `ids`, each informative and not flagged (KEPT), and the
 *  control runs of `control` among them, whose two controls hold; each stage made by `runner`
 *  (this runner by default). */
function plantSix(s: Scenario, ids: readonly string[], control: string, runner = RUNNER): void {
  for (const id of ids) {
    plantResult(s, id, {
      stages: {
        screen: plantedStage(s, id, "screen", KEPT, runner),
        ...(id === control
          ? {
              control: plantedStage(
                s,
                id,
                "control",
                { old: FRESH_RUNS, minus: 0, twin: FRESH_RUNS },
                runner
              ),
            }
          : {}),
      },
    });
  }
}

/** A case's evidence holding one listing measure as build.ts writes it (ListingMeasure): of
 *  `kind`, its recorded listing holding `recorded` names, of which the rebuilt one misses
 *  `missing`, and to which it adds `added`. */
function listingEvidence(
  kind: ListingMeasure["kind"],
  recorded: number | null,
  missing: string[],
  added: string[] = []
): { item: string; source: string }[] {
  const measure = ListingMeasure.parse({
    kind,
    recorded,
    listed: 12,
    missing,
    added,
    missingShare: recorded === null || recorded === 0 ? null : missing.length / recorded,
    sessionDigest: "46874d907e557998",
    rebuiltDigest: null,
    subagent: null,
    why: "the made-up session records no system prompt, only its digest",
  });
  return [{ item: LISTING_MEASURE_ITEM, source: JSON.stringify(measure) }];
}

/** The cases of `s` again in its cases file, each id of `evidence` with that evidence. */
function withEvidence(
  s: Scenario,
  evidence: Record<string, { item: string; source: string }[]>
): void {
  const cases = s.cases.map((c) => {
    const given = evidence[c.id];
    return given === undefined ? c : { ...c, evidence: given };
  });
  writeFileSync(s.casesFile, cases.map((c) => `${JSON.stringify(c)}\n`).join(""));
}

/** The end of a verdict's line where `count` of `screened` cases' listings were not measured,
 *  `taken` of them installed names their recorded listing gave them (fromRecorded), and
 *  `assumed` of them rest on the assumption that their session had Dispatch. */
const unmeasured = (count: number, screened: number, assumed = 0, taken = 0) =>
  `; the listing of ${count} of its ${screened} screened cases could not be measured, ${taken} of the ${screened} installed names their recorded listing gave them (fromRecorded), and ${assumed} of the ${screened} rest on the assumption that their session had Dispatch`;

// The architect's rules for dropped cases, the listing measure and pooled confirms
// (2026-11-22): the results are planted as the runner writes them, so no row calls a model but
// the one that runs a third confirm, against the stand-in.
describe("behaviour: dropped cases, the listing rule and pooled confirms", () => {
  const ids = (count: number) =>
    Array.from({ length: count }, (_, n) => ({ id: `c${String(n + 1).padStart(2, "0")}` }));
  const SIX = ["c01", "c02", "c03", "c04", "c05", "c06"];

  test("a flagged case dropped at confirm is listed with its screen passes, and its skill gets no verdict that names it", async () => {
    const s = scenario("dropped-confirm", LOSS_SKILL(), ids(7));
    plantSix(s, SIX, "c01");
    // c07's screen flagged it, and its confirm's capture needed a directory no read gives. A
    // result written before the runner recorded the stage of a drop names none: one whose screen
    // flagged it and that has no confirm runs was dropped at confirm.
    plantResult(s, "c07", {
      dropped: UNREADABLE,
      stages: { screen: plantedStage(s, "c07", "screen", FLAGGED) },
    });
    const ran = await cli(report(s));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const lines = ran.stdout.split("\n");
    expect(lines).toContain(
      `case c07: dropped at confirm: ${UNREADABLE}; screen: old 10/10, new 0/10, none 0/10: informative, flagged`
    );
    expect(lines).toContain(
      `cases: 7 screened, 7 informative, 1 flagged, 0 confirmed losses; 1 dropped (0 by the judge controls, 0 model-not-served, 1 unreadable-directory, ${NO_LEGION_TOOL}, 0 listing-misses, 0 listing-adds, ${NO_INJECTION}, ${NO_PATH_FORM}, ${NO_READS_PAST}, ${NO_REFUSED}), 0 unscreened, 0 not run`
    );
    expect(lines).toContain(
      `verdict: none: flagged case c07 could not be confirmed (${UNREADABLE})${unmeasured(7, 7)}`
    );
  }, 60_000);

  test("a flagged case dropped at confirm leaves another case's confirmed loss standing as the skill's loss, and is listed", async () => {
    const s = scenario("dropped-confirm-loss", LOSS_SKILL(), ids(8));
    plantSix(s, SIX, "c01");
    plantResult(s, "c07", {
      dropped: UNREADABLE,
      droppedAt: "confirm",
      stages: { screen: plantedStage(s, "c07", "screen", FLAGGED) },
    });
    plantResult(s, "c08", {
      stages: {
        screen: plantedStage(s, "c08", "screen", FLAGGED),
        confirm: plantedStage(s, "c08", "confirm", { old: FRESH_RUNS, new: 0 }),
      },
    });
    const ran = await cli(report(s));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const lines = ran.stdout.split("\n");
    expect(lines).toContain(
      `case c07: dropped at confirm: ${UNREADABLE}; screen: old 10/10, new 0/10, none 0/10: informative, flagged`
    );
    expect(lines).toContain(
      `verdict: loss: in c08 the cut's text passes at least 10 fewer of 40 fresh runs than the old text, and the planted-loss control (case c01) is a loss and its false-loss control is no loss${unmeasured(8, 8)}`
    );
  }, 60_000);

  test("a dropped control case gives its place to the next informative case by the control rule, and report and control name both", async () => {
    const s = scenario("dropped-control", LOSS_SKILL(), ids(6));
    plantSix(s, SIX.slice(1), "c02");
    // c01 comes first in the control rule's order (every gap is 10, and it has the lowest id);
    // a capture of its control runs needed a directory no read gives. Written before the runner
    // recorded the stage of a drop: a screened case its screen did not flag was dropped at
    // control.
    plantResult(s, "c01", {
      dropped: UNREADABLE,
      stages: { screen: plantedStage(s, "c01", "screen", KEPT) },
    });
    const ran = await cli(report(s));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const lines = ran.stdout.split("\n");
    expect(lines).toContain(
      `case c01: dropped at control: ${UNREADABLE}; screen: old 10/10, new 10/10, none 0/10: informative, not flagged`
    );
    expect(lines).toContain(
      "control case: c02, in place of c01 (dropped at control), which the control rule chose before it"
    );
    expect(lines).toContain(
      `verdict: no loss detected by the registered rule: no confirmed case is a loss (0 flagged of 6 informative), and the planted-loss control (case c02) is a loss and its false-loss control is no loss${unmeasured(6, 6)}`
    );
    const from = recorder.requests.length;
    const control = await cli(behaviour("control", s));
    expect({ code: control.code, stderr: control.stderr }).toEqual({ code: 0, stderr: "" });
    expect(control.stdout).toContain(
      "the control case is c02, in place of c01 (dropped at control), which the control rule chose before it;"
    );
    expect(control.stdout).toContain("case c02: control runs made before, skipped");
    expect(requestsSince(from)).toHaveLength(0);
  }, 60_000);

  test("a planted record of a case dropped at confirm that its screen did not flag is refused, naming the case (exit 1)", async () => {
    const s = scenario("dropped-unflagged", LOSS_SKILL(), ids(6));
    plantSix(
      s,
      SIX.filter((id) => id !== "c03"),
      "c01"
    );
    plantResult(s, "c03", {
      dropped: UNREADABLE,
      droppedAt: "confirm",
      stages: { screen: plantedStage(s, "c03", "screen", KEPT) },
    });
    const refusal = `case c03 is recorded as dropped at confirm, but its screen did not flag it, and only flagged cases are confirmed: its records are refused, not scored (its drop: ${UNREADABLE})`;
    const ran = await cli(report(s));
    expect(ran.code).toBe(1);
    expect(ran.stderr).toContain(refusal);
    expect(ran.stdout).toBe("");
    const from = recorder.requests.length;
    const confirm = await cli(behaviour("confirm", s));
    expect(confirm.code).toBe(1);
    expect(confirm.stderr).toContain(refusal);
    expect(requestsSince(from)).toHaveLength(0);
  }, 60_000);

  test("a case whose rebuilt listing misses more than 10% of its session's recorded names is dropped: report lists and counts it, screen drops it before any call, and the verdict line counts the listings not measured and those that took names from their recorded listing", async () => {
    const s = scenario("listing", LOSS_SKILL(), ids(7));
    // c02's profile took a name from its recorded listing (build.ts's fromRecorded mark).
    const taken = listingEvidence("session listing", 10, ["beta"]).map((each) => ({
      ...each,
      source: JSON.stringify({ ...JSON.parse(each.source), fromRecorded: ["cw-one"] }),
    }));
    withEvidence(s, {
      ...Object.fromEntries(
        SIX.slice(0, 5).map((id) => [id, listingEvidence("session listing", 10, ["beta"])])
      ),
      c02: taken,
      c06: listingEvidence("not measurable", null, []),
      c07: listingEvidence("subagent listing", 20, ["beta", "gamma", "delta"]),
    });
    plantSix(s, [...SIX, "c07"], "c01");
    const ran = await cli(report(s));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const lines = ran.stdout.split("\n");
    const misses =
      "listing-misses: its rebuilt listing misses 3 of the 20 names of the recorded subagent listing (15.0%), more than 10%: beta, gamma, delta";
    expect(lines).toContain(
      `case c07: dropped at screen: ${misses}; screen: old 10/10, new 10/10, none 0/10: informative, not flagged`
    );
    expect(lines).toContain(
      `cases: 6 screened, 6 informative, 0 flagged, 0 confirmed losses; 1 dropped (0 by the judge controls, 0 model-not-served, 0 unreadable-directory, ${NO_LEGION_TOOL}, 1 listing-misses, 0 listing-adds, ${NO_INJECTION}, ${NO_PATH_FORM}, ${NO_READS_PAST}, ${NO_REFUSED}), 0 unscreened, 0 not run`
    );
    expect(lines).toContain(
      `verdict: no loss detected by the registered rule: no confirmed case is a loss (0 flagged of 6 informative), and the planted-loss control (case c01) is a loss and its false-loss control is no loss${unmeasured(1, 6, 0, 1)}`
    );

    // A case not screened yet is dropped by screen before any call.
    const fresh = scenario("listing-screen", LOSS_SKILL(), [{ id: "c-misses" }]);
    withEvidence(fresh, {
      "c-misses": listingEvidence("session listing", 9, ["beta"]),
    });
    answer = behaviourStandIn;
    const from = recorder.requests.length;
    const screen = await cli(behaviour("screen", fresh));
    expect({ code: screen.code, stderr: screen.stderr }).toEqual({ code: 0, stderr: "" });
    expect(requestsSince(from)).toHaveLength(0);
    const dropped = result(fresh.out, "c-misses");
    expect([dropped.dropped, dropped.droppedAt, dropped.stages]).toEqual([
      "listing-misses: its rebuilt listing misses 1 of the 9 names of the recorded session listing (11.1%), more than 10%: beta",
      "screen",
      {},
    ]);
  }, 60_000);

  // The architect, 2026-11-24T23:52Z: the listing rule drops on added names too, for a case whose
  // capture builds its listing from the rebuilt profile (a measure of kind `subagent listing`; one
  // whose capture sends its session's recorded prompt whole is not dropped for them, below).
  // Report applies it to saved results, so a case a runner of the earlier code screened, under
  // the rule that dropped only on missing names, is dropped at screen with no model call.
  test("a case whose rebuilt profile-built listing adds more names than 10% of its session's recorded names is dropped as listing-adds: report drops and counts a saved case an earlier runner screened, and screen drops a new one before any call", async () => {
    const s = scenario("listing-adds", LOSS_SKILL(), ids(7));
    withEvidence(s, {
      ...Object.fromEntries(
        SIX.map((id) => [id, listingEvidence("subagent listing", 10, [], ["beta"])])
      ),
      c07: listingEvidence("subagent listing", 20, [], ["cw-one", "cw-two", "cw-three"]),
    });
    // A runner digest no runner of this tree has: the code before the listing-adds rule.
    const earlier = "0123456789abcdef";
    plantSix(s, [...SIX, "c07"], "c01", earlier);
    const ran = await cli(report(s));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const lines = ran.stdout.split("\n");
    expect(lines[1]).toContain(`screen: ${earlier} (7 case stages, `);
    const adds =
      "listing-adds: its rebuilt listing adds 3 to the 20 names of the recorded subagent listing (15.0%), more than 10%: cw-one, cw-two, cw-three";
    expect(lines).toContain(
      `case c07: dropped at screen: ${adds}; screen: old 10/10, new 10/10, none 0/10: informative, not flagged`
    );
    expect(lines).toContain(
      `cases: 6 screened, 6 informative, 0 flagged, 0 confirmed losses; 1 dropped (0 by the judge controls, 0 model-not-served, 0 unreadable-directory, ${NO_LEGION_TOOL}, 0 listing-misses, 1 listing-adds, ${NO_INJECTION}, ${NO_PATH_FORM}, ${NO_READS_PAST}, ${NO_REFUSED}), 0 unscreened, 0 not run`
    );

    // A case not screened yet is dropped by screen before any call.
    const fresh = scenario("listing-adds-screen", LOSS_SKILL(), [{ id: "c-adds" }]);
    withEvidence(fresh, {
      "c-adds": listingEvidence("subagent listing", 9, [], ["cw-one"]),
    });
    answer = behaviourStandIn;
    const from = recorder.requests.length;
    const screen = await cli(behaviour("screen", fresh));
    expect({ code: screen.code, stderr: screen.stderr }).toEqual({ code: 0, stderr: "" });
    expect(requestsSince(from)).toHaveLength(0);
    const dropped = result(fresh.out, "c-adds");
    expect([dropped.dropped, dropped.droppedAt, dropped.stages]).toEqual([
      "listing-adds: its rebuilt listing adds 1 to the 9 names of the recorded subagent listing (11.1%), more than 10%: cw-one",
      "screen",
      {},
    ]);
  }, 60_000);

  test("the listing rule reads the listing measure build.ts writes", () => {
    expect(listingStanding(listingEvidence("subagent listing", 30, ["beta", "gamma"]))).toEqual({
      state: "measured",
      recorded: 30,
      missing: 2,
      added: 0,
    });
  });

  // The architect, 2026-11-25T00:52:35Z: a case whose capture sends its session's recorded system
  // prompt whole (a measure of kind `session listing`) shows the model the recorded listing
  // whatever the rebuilt profile installs, so names its profile adds drop nothing; report counts
  // the runs of such cases that read a skill outside that listing, by skill and answer kind.
  test("behaviour report over saved results of a recorded-prompt case whose measure adds over 10% keeps the case and prints the count of runs that read a skill outside the recorded listing, by skill and answer kind", async () => {
    const s = scenario("listing-recorded-prompt", LOSS_SKILL(), ids(6));
    const recordedPrompt = [
      "You are a made-up subagent.\n<skills>\n- alpha: Use for the alpha steps.\n- beta: Use for the beta greeting.\n</skills>\n",
    ];
    // c06's session recorded its system prompt, which its capture sends whole; its profile adds 20
    // names to the 2 it lists. c01 to c05's captures build their listings from the profile.
    withEvidence(s, {
      ...Object.fromEntries(
        SIX.slice(0, 5).map((id) => [id, listingEvidence("subagent listing", 10, [], ["beta"])])
      ),
      c06: listingEvidence(
        "session listing",
        2,
        [],
        Array.from({ length: 20 }, (_, n) => `extra-${n + 1}`)
      ),
    });
    const cases = readFileSync(s.casesFile, "utf8")
      .split("\n")
      .filter((line) => line !== "")
      .map((line): Case => JSON.parse(line));
    writeFileSync(
      s.casesFile,
      cases
        .map((c) =>
          c.id !== "c06"
            ? c
            : {
                ...c,
                context: "subagent:task",
                profile: { ...c.profile, systemPrompt: recordedPrompt, tools: ["read", "bash"] },
              }
        )
        .map((c) => `${JSON.stringify(c)}\n`)
        .join("")
    );
    /** One answered read of a `skill://` file, as a run records it (replay.ts's AnsweredRead). */
    interface Read {
      skill: string;
      file: string;
      by: "skill";
      from: "copy" | "absent";
      isError: boolean;
    }
    const read = (skill: string, from: Read["from"], file = "SKILL.md"): Read => ({
      skill,
      file,
      by: "skill",
      from,
      isError: from === "absent",
    });
    /** `stage`'s runs, the `n`th of them having read `rounds[n]`, each round's reads in a list. */
    const readingRuns = (stage: Record<string, unknown>, rounds: Read[][][]) => ({
      ...stage,
      runs: (stage.runs as Record<string, unknown>[]).map((run, n) => ({
        ...run,
        skillRounds: rounds[n]?.length ?? 0,
        skillReads: rounds[n] ?? [],
      })),
    });
    plantSix(s, SIX.slice(0, 4), "c01");
    // c05's capture builds its listing from its profile: its run's read of zeta is no count of the
    // rule's.
    plantResult(s, "c05", {
      stages: {
        screen: readingRuns(plantedStage(s, "c05", "screen", KEPT), [[[read("zeta", "absent")]]]),
      },
    });
    // c06's runs: one reads beta, which its recorded listing holds; one reads gamma in two rounds;
    // one reads gamma and delta in one round; one reads delta; the others read no skill.
    plantResult(s, "c06", {
      stages: {
        screen: readingRuns(plantedStage(s, "c06", "screen", KEPT), [
          [[read("beta", "copy")]],
          [[read("gamma", "copy")], [read("gamma", "copy", "references/a.md")]],
          [[read("gamma", "copy"), read("delta", "absent")]],
          [[read("delta", "absent")]],
        ]),
      },
    });
    const ran = await cli(report(s));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const lines = ran.stdout.split("\n");
    expect(lines).toContain(
      "case c06: screen: old 10/10, new 10/10, none 0/10: informative, not flagged"
    );
    expect(lines).toContain(
      `cases: 6 screened, 6 informative, 0 flagged, 0 confirmed losses; 0 dropped (0 by the judge controls, 0 model-not-served, 0 unreadable-directory, ${NO_LEGION_TOOL}, 0 listing-misses, 0 listing-adds, ${NO_INJECTION}, ${NO_PATH_FORM}, ${NO_READS_PAST}, ${NO_REFUSED}), 0 unscreened, 0 not run`
    );
    expect(lines).toContain(
      "outside recorded listing: 3 of 30 runs of 1 recorded-prompt case (the capture sends the session's recorded system prompt whole) read a skill the recorded listing lacks; by skill and answer (copy: an installed copy; absent: the unknown-skill reply): delta (0 copy, 2 absent), gamma (2 copy, 0 absent)"
    );
    // Six informative cases and no loss: the reads change no verdict.
    expect(lines.find((line) => line.startsWith("verdict: "))).toContain(
      "no loss detected by the registered rule"
    );
  }, 60_000);

  test("report names a case whose three confirms on one runner were pooled, with each result and the pooled passes; a fourth confirm is refused, naming the case", async () => {
    const s = scenario("pooled", LOSS_SKILL(), ids(7));
    plantSix(s, SIX, "c01");
    const runner = runnerDigest();
    const confirm = (cut: number) =>
      plantedStage(s, "c07", "confirm", { old: FRESH_RUNS, new: cut }, runner);
    plantResult(s, "c07", {
      stages: { screen: plantedStage(s, "c07", "screen", FLAGGED), confirm: confirm(29) },
      earlierConfirms: [confirm(30), confirm(31)],
    });
    const ran = await cli(report(s));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const lines = ran.stdout.split("\n");
    expect(lines).toContain(
      "case c07: screen: old 10/10, new 0/10, none 0/10: informative, flagged; confirm 1: old 40/40, new 30/40: a loss; confirm 2: old 40/40, new 31/40: no loss; confirm 3: old 40/40, new 29/40: a loss; pooled: old 120/120, new 90/120, 30 fewer: a loss"
    );
    expect(lines).toContain(
      "pooled confirms (two confirms at one case's texts on one runner disagreed, so a third ran, and the case is a loss when the cut's text passes at least 30 fewer of the 120 pooled runs than the old text): c07 (confirms of old 40, 40, 40 and new 30, 31, 29 of 40 each; pooled old 120/120, new 90/120, 30 fewer: a loss)"
    );
    expect(lines).toContain(
      `verdict: loss: in c07, over 3 pooled confirms, at least 30 fewer of 120, and the planted-loss control (case c01) is a loss and its false-loss control is no loss${unmeasured(7, 7)}`
    );
    const from = recorder.requests.length;
    const fourth = await cli(behaviour("confirm", s, "--again", "c07"));
    expect(fourth.code).toBe(2);
    expect(fourth.stderr).toContain(
      "--again c07: case c07 has 3 confirms at these texts on this runner, which the rule pools, and no fourth confirm runs"
    );
    expect(requestsSince(from)).toHaveLength(0);
  }, 60_000);

  test("confirm runs a third confirm of a case whose two confirms on one runner disagree, and report pools the three", async () => {
    const s = scenario("third", LOSS_SKILL(), ids(7));
    plantSix(s, SIX, "c01");
    recordCaptures(s, ["old", "new"]);
    answer = behaviourStandIn;
    const runner = runnerDigest();
    const confirm = (cut: number) =>
      plantedStage(s, "c07", "confirm", { old: FRESH_RUNS, new: cut }, runner);
    plantResult(s, "c07", {
      stages: { screen: plantedStage(s, "c07", "screen", FLAGGED), confirm: confirm(31) },
      earlierConfirms: [confirm(30)],
    });
    const before = await cli(report(s));
    expect(before.stdout.split("\n")).toContain(
      "verdict: not yet: two confirms on one runner disagree in c07, so a third confirm of each is due (behaviour confirm)"
    );
    const ran = await cli(behaviour("confirm", s, "--concurrency", "32"));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    expect(ran.stdout).toContain(
      "case c07: its last two confirms at these texts on one runner disagree, one a loss and one not, so a third confirm runs, and the three are pooled"
    );
    const c07 = result(s.out, "c07");
    const earlier = (c07.earlierConfirms ?? []).map((each) =>
      passes({ ...c07, stages: { confirm: each } }, "confirm")
    );
    expect(earlier).toEqual([
      { old: FRESH_RUNS, new: 30 },
      { old: FRESH_RUNS, new: 31 },
    ]);
    expect([c07.stages.confirm?.state, c07.stages.confirm?.runner]).toEqual(["counted", runner]);
    expect(passes(c07, "confirm")).toEqual({ old: FRESH_RUNS, new: 0 });
    const after = await cli(report(s));
    expect(after.stdout).toContain(
      "c07 (confirms of old 40, 40, 40 and new 30, 31, 0 of 40 each; pooled old 120/120, new 61/120, 59 fewer: a loss)"
    );
  }, 60_000);
});

// INFRA-4676's architect (2026-11-25T18:54:22Z and 19:02:42Z): cases come from Postgres sessions
// alone, so a session only the transcript store holds is counted but is never a case source. Each
// behaviour report says how many of its skill's counted sessions are such, and a skill with too
// few informative cases for a verdict says so in its reason.
describe("behaviour report --store-only", () => {
  /** Writes `content` (JSON of a value, or a text as it is) to `name` in the directory of `s`, as
   *  attribute's `<cut id>/store-only/<skill>.json` of its skill, and returns that file. */
  const storeOnlyFile = (s: Scenario, name: string, content: unknown) => {
    const file = path.join(s.dir, name);
    writeFileSync(file, typeof content === "string" ? content : JSON.stringify(content));
    return file;
  };
  const SOME = {
    skill: "alpha",
    counted: 40,
    storeOnly: 3,
    hosts: [
      { host: "host-a", sessions: 2 },
      { host: "host-b", sessions: 1 },
    ],
  };
  const NONE = { skill: "alpha", counted: 12, storeOnly: 0, hosts: [] };
  const COUNTED_SOME =
    "counted sessions: 40, of which store-only (held by the transcript store alone, so not case sources): 3 (host-a: 2, host-b: 1)";
  const COUNTED_NONE =
    "counted sessions: 12, of which store-only (held by the transcript store alone, so not case sources): 0";
  const NEVER_SOURCES =
    "; 3 of the skill's 40 counted sessions are store-only, so they were never case sources (host-a: 2, host-b: 1)";
  /** The report of `s` with `extra`, as its lines, refused nothing. */
  const reportLines = async (s: Scenario, ...extra: string[]) => {
    const ran = await cli([...report(s), ...extra]);
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    return ran.stdout.split("\n");
  };
  const verdictOf = (lines: readonly string[]) =>
    lines.find((line) => line.startsWith("verdict: "));

  test("prints the counted sessions line, next to the reading sessions, for a skill that gets a verdict, 0 where none is store-only, and nothing of the store without the option", async () => {
    const six = ["c01", "c02", "c03", "c04", "c05", "c06"];
    const s = scenario(
      "store-only-verdict",
      f1File(2),
      six.map((id) => ({ id }))
    );
    plantSix(s, six, "c01");
    const plain = await reportLines(s);
    expect(plain.filter((line) => line.startsWith("counted sessions"))).toEqual([]);
    expect(plain.join("\n")).not.toContain("transcript store");
    const keeps = verdictOf(plain);
    expect(keeps).toStartWith("verdict: no loss detected by the registered rule");

    const some = await reportLines(s, "--store-only", storeOnlyFile(s, "some.json", SOME));
    const at = some.indexOf(COUNTED_SOME);
    expect(some[at - 1]).toStartWith("reading sessions by model: ");
    expect(some[at + 1]).toStartWith("verdict: ");
    // A verdict's reason is no fewer-than-6 one: it names no store-only session.
    expect(verdictOf(some)).toBe(keeps);

    const none = await reportLines(s, "--store-only", storeOnlyFile(s, "none.json", NONE));
    expect(none).toContain(COUNTED_NONE);
    expect(verdictOf(none)).toBe(keeps);
  }, 60_000);

  test("a skill with fewer than 6 informative cases ends its reason with how many of its counted sessions are store-only, and with none says nothing of them", async () => {
    const s = scenario("store-only-few", LOSS_SKILL(), [{ id: "c01" }]);
    plantResult(s, "c01", { stages: { screen: plantedStage(s, "c01", "screen", FLAGGED) } });
    const tooFew = `verdict: none: fewer than ${MIN_INFORMATIVE_CASES} informative cases: 1`;
    expect(verdictOf(await reportLines(s))).toBe(`${tooFew}${unmeasured(1, 1)}`);

    const some = await reportLines(s, "--store-only", storeOnlyFile(s, "some.json", SOME));
    expect(some).toContain(COUNTED_SOME);
    expect(verdictOf(some)).toBe(`${tooFew}${NEVER_SOURCES}${unmeasured(1, 1)}`);

    const none = await reportLines(s, "--store-only", storeOnlyFile(s, "none.json", NONE));
    expect(none).toContain(COUNTED_NONE);
    expect(verdictOf(none)).toBe(`${tooFew}${unmeasured(1, 1)}`);
  }, 60_000);

  test("a store-only file that is not JSON or not of its shape is refused, naming it, and so is the option on a stage command; a cases file with no case is still refused", async () => {
    const s = scenario("store-only-refused", LOSS_SKILL(), [{ id: "c01" }]);
    const notJson = storeOnlyFile(s, "not-json.json", "{ counted: 40");
    const notJsonRan = await cli([...report(s), "--store-only", notJson]);
    expect(notJsonRan.code).toBe(2);
    expect(notJsonRan.stderr).toContain(`--store-only ${notJson} is not JSON: `);
    const shape = storeOnlyFile(s, "shape.json", { skill: "alpha", counted: 40 });
    const shapeRan = await cli([...report(s), "--store-only", shape]);
    expect(shapeRan.code).toBe(2);
    expect(shapeRan.stderr).toContain(
      `--store-only ${shape} is not a skill's store-only sessions (attribute's <cut id>/store-only/<skill>.json): storeOnly: `
    );

    const from = recorder.requests.length;
    const screen = await cli(
      behaviour("screen", s, "--store-only", storeOnlyFile(s, "some.json", SOME))
    );
    expect(screen.code).toBe(2);
    expect(screen.stderr).toContain("--store-only goes with behaviour report only");
    expect(requestsSince(from)).toHaveLength(0);

    // build-cases' line says what a skill with no case leaves out (build.test.ts); the report
    // still reads no file with no case.
    writeFileSync(s.casesFile, "");
    const empty = await cli([...report(s), "--store-only", storeOnlyFile(s, "some.json", SOME)]);
    expect(empty.code).toBe(2);
    expect(empty.stderr).toContain(`${s.casesFile} holds no case`);
  }, 60_000);
});

// The architect's ruling on `confirm --confirm-flagged` (2026-11-22T21:46Z): it confirms each
// flagged case that is not dropped whatever the skill's informative count or other reasons for no
// verdict, so a case run again alone can be confirmed; it confirms no case its screen did not
// flag; and the report of a run it confirmed gives case results only, never a skill verdict.
describe("behaviour: confirm --confirm-flagged", () => {
  const SIX = ["c01", "c02", "c03", "c04", "c05", "c06"];
  /** The report's verdict line of a run with a confirm made by `--confirm-flagged`. */
  const CASE_RESULT_ONLY = "verdict: case result only; no skill verdict stands (--confirm-flagged)";

  // Catches: a case run again alone that can never be confirmed, since its skill has too few
  // informative cases for a verdict; and a confirm without the option that no longer waits for one.
  test("confirms the flagged case of a one-case run, 40 runs of old and of new, where confirm without it makes none, and report gives its case result and no skill verdict", async () => {
    const s = scenario("confirm-flagged-one", LOSS_SKILL(), [{ id: "c01" }]);
    recordCaptures(s, ["old", "new"]);
    answer = behaviourStandIn;
    plantResult(s, "c01", { stages: { screen: plantedStage(s, "c01", "screen", FLAGGED) } });
    const tooFew = `fewer than ${MIN_INFORMATIVE_CASES} informative cases: 1`;

    let from = recorder.requests.length;
    const without = await cli(behaviour("confirm", s));
    expect({ code: without.code, stderr: without.stderr }).toEqual({ code: 0, stderr: "" });
    expect(without.stdout).toContain(`no confirm runs: the skill gets no verdict: ${tooFew}`);
    expect(requestsSince(from)).toHaveLength(0);
    expect(result(s.out, "c01").stages.confirm).toBeUndefined();

    from = recorder.requests.length;
    const ran = await cli(behaviour("confirm", s, "--confirm-flagged", "--concurrency", "32"));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    expect(ran.stdout).toContain(
      `--confirm-flagged: each flagged case that is not dropped is confirmed, though the skill gets no verdict (${tooFew}); report gives the case results only, and no skill verdict`
    );
    expect(ran.stdout).toContain("1 flagged cases: c01");
    // FRESH_RUNS replays of each of old and new, and a judgement of each.
    expect(requestsSince(from)).toHaveLength(2 * FRESH_RUNS * 2);
    const c01 = result(s.out, "c01");
    expect([c01.stages.confirm?.state, c01.stages.confirm?.confirmFlagged]).toEqual([
      "counted",
      true,
    ]);
    expect(passes(c01, "confirm")).toEqual({ old: FRESH_RUNS, new: 0 });

    const reported = await cli(report(s));
    expect({ code: reported.code, stderr: reported.stderr }).toEqual({ code: 0, stderr: "" });
    const lines = reported.stdout.split("\n");
    expect(lines).toContain(
      "case c01: screen: old 10/10, new 0/10, none 0/10: informative, flagged; confirm: old 40/40, new 0/40: a loss"
    );
    expect(lines.filter((line) => line.startsWith("verdict: "))).toEqual([CASE_RESULT_ONLY]);
  }, 60_000);

  // Catches: an option that confirms every screened case, or every case whose cut passes fewer
  // runs than the old text, in place of the cases the screen's rule flagged.
  test("confirms no case its screen did not flag", async () => {
    const s = scenario("confirm-flagged-unflagged", LOSS_SKILL(), [
      { id: "c01" },
      { id: "c02" },
      { id: "c03" },
    ]);
    recordCaptures(s, ["old", "new"]);
    answer = behaviourStandIn;
    // c01 is flagged and confirmed before; c02 is informative and not flagged; c03's cut passes
    // 10 fewer runs than the old text, but no skill passes as many as the old text, so it is not
    // informative, and not flagged.
    plantResult(s, "c01", {
      stages: {
        screen: plantedStage(s, "c01", "screen", FLAGGED),
        confirm: plantedStage(s, "c01", "confirm", { old: FRESH_RUNS, new: 0 }, runnerDigest()),
      },
    });
    plantResult(s, "c02", { stages: { screen: plantedStage(s, "c02", "screen", KEPT) } });
    plantResult(s, "c03", {
      stages: {
        screen: plantedStage(s, "c03", "screen", { old: SCREEN_RUNS, new: 0, none: SCREEN_RUNS }),
      },
    });
    const from = recorder.requests.length;
    const ran = await cli(behaviour("confirm", s, "--confirm-flagged"));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    expect(ran.stdout).toContain("1 flagged cases: c01");
    expect(ran.stdout).toContain("case c01: confirmed before, skipped");
    expect(requestsSince(from)).toHaveLength(0);
    expect(result(s.out, "c02").stages.confirm).toBeUndefined();
    expect(result(s.out, "c03").stages.confirm).toBeUndefined();
  }, 60_000);

  // Catches: a skill verdict over a confirm the option made, which ran whatever the skill's
  // informative count and controls; and a report that needs the option given again to know.
  test("report of a run with a confirm made by --confirm-flagged gives the case results and no skill verdict, where the same runs without it give one", async () => {
    const s = scenario(
      "confirm-flagged-report",
      LOSS_SKILL(),
      [...SIX, "c07"].map((id) => ({ id }))
    );
    plantSix(s, SIX, "c01");
    const screen = plantedStage(s, "c07", "screen", FLAGGED);
    const confirm = plantedStage(s, "c07", "confirm", { old: FRESH_RUNS, new: 0 });
    plantResult(s, "c07", { stages: { screen, confirm } });
    const before = await cli(report(s));
    expect({ code: before.code, stderr: before.stderr }).toEqual({ code: 0, stderr: "" });
    expect(before.stdout.split("\n")).toContain(
      `verdict: loss: in c07 the cut's text passes at least 10 fewer of 40 fresh runs than the old text, and the planted-loss control (case c01) is a loss and its false-loss control is no loss${unmeasured(7, 7)}`
    );

    plantResult(s, "c07", { stages: { screen, confirm: { ...confirm, confirmFlagged: true } } });
    const ran = await cli(report(s));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const lines = ran.stdout.split("\n");
    expect(lines).toContain(
      "case c07: screen: old 10/10, new 0/10, none 0/10: informative, flagged; confirm: old 40/40, new 0/40: a loss"
    );
    expect(lines).toContain(
      `cases: 7 screened, 7 informative, 1 flagged, 1 confirmed losses; 0 dropped (0 by the judge controls, 0 model-not-served, 0 unreadable-directory, ${NO_LEGION_TOOL}, 0 listing-misses, 0 listing-adds, ${NO_INJECTION}, ${NO_PATH_FORM}, ${NO_READS_PAST}, ${NO_REFUSED}), 0 unscreened, 0 not run`
    );
    expect(lines.filter((line) => line.startsWith("verdict: "))).toEqual([CASE_RESULT_ONLY]);

    // report reads the option from the results, and is refused it.
    const given = await cli([...report(s), "--confirm-flagged"]);
    expect(given.code).toBe(2);
    expect(given.stderr).toContain(
      "--confirm-flagged goes with behaviour confirm only: report reads from the case results whether their confirms were made with it."
    );
  }, 60_000);
});

describe("refusals before any model call", () => {
  test("a --config that is not a configuration is refused (exit 2) by read, control, behaviour and load", async () => {
    const { keyFile } = setup();
    const bad = path.join(scratch, "bad-skill-cuts.json");
    writeFileSync(bad, "{ not json");
    const input = path.join(scratch, "bad-config-in.json");
    writeFileSync(
      input,
      JSON.stringify({ skill: "alpha", files: [{ path: "SKILL.md", text: SKILL_MD }] })
    );
    const s = scenario("bad-config", LOSS_SKILL(), [{ id: "c04" }]);
    const tasks = path.join(s.dir, "tasks.jsonl");
    writeFileSync(tasks, "");
    const from = recorder.requests.length;
    for (const args of [
      ["read", "--prompt", "attack", "--in", input, "--out", path.join(s.dir, "read.json")],
      ["control", "attack", "--in", input, "--out", path.join(s.dir, "control.json")],
      [
        "behaviour",
        "screen",
        ...["--out", s.out, "--old", s.old, "--new", s.new, "--cases", s.casesFile],
      ],
      ["load", "--skill", s.old, "--tasks", tasks, "--host", "omp", "--out", s.out],
    ]) {
      const ran = await cli([...args, "--config", bad, "--key-file", keyFile]);
      expect({ command: args[0], code: ran.code }).toEqual({ command: args[0], code: 2 });
      expect(ran.stderr).toContain(`skill-cuts: the configuration ${bad} is not JSON`);
    }
    expect(requestsSince(from)).toHaveLength(0);
  }, 60_000);

  test("behaviour and load refuse an --out inside a git or jj checkout (exit 2), since they write session text there", async () => {
    const { config, keyFile } = setup();
    const s = scenario("in-checkout", LOSS_SKILL(), [{ id: "c04" }]);
    recordCaptures(s, ["old", "new", "none"]);
    answer = behaviourStandIn;
    const tasks = path.join(s.dir, "tasks.jsonl");
    writeFileSync(tasks, "");
    const from = recorder.requests.length;
    for (const marker of [".git", ".jj"]) {
      const checkout = path.join(s.dir, `checkout${marker}`);
      mkdirSync(path.join(checkout, marker), { recursive: true });
      const out = path.join(checkout, "results", "out");
      for (const args of [
        ["behaviour", "screen", "--old", s.old, "--new", s.new, "--cases", s.casesFile],
        ["load", "--skill", s.old, "--tasks", tasks, "--host", "omp"],
      ]) {
        const ran = await cli([...args, "--config", config, "--key-file", keyFile, "--out", out]);
        expect({ command: args[0], code: ran.code }).toEqual({ command: args[0], code: 2 });
        expect(ran.stderr).toContain(`is inside the checkout ${checkout}`);
        expect(existsSync(out)).toBe(false);
      }
    }
    expect(requestsSince(from)).toHaveLength(0);
    // `report` writes nothing, so it reads an --out inside a checkout.
    const inCheckout = path.join(s.dir, "checkout.git", "results", "out");
    const reported = await cli(report(s).map((arg) => (arg === s.out ? inCheckout : arg)));
    expect({ code: reported.code, stderr: reported.stderr }).toEqual({ code: 0, stderr: "" });
  }, 60_000);

  // Catches: build-cases and build-tasks through cli.ts waiting for the span queue twice, in
  // build.ts's `main` and again in cli.ts's, so that a collector that hangs held the exit twice
  // FLUSH_MS where README.md ("Usage records") says 2 s.
  test("build-cases and build-tasks wait for the usage spans queued once: a collector that hangs holds the exit FLUSH_MS at most", async () => {
    const hanging = startCollector();
    hanging.delayMs = POST_TIMEOUT_MS + 1_000;
    try {
      for (const command of ["build-cases", "build-tasks"]) {
        const spendRecorder = createSpendRecorder({
          env: { OTEL_EXPORTER_OTLP_ENDPOINT: hanging.url },
          warn: () => {},
        });
        // Three spans, each post held until POST_TIMEOUT_MS: the queue outlasts any one wait.
        for (let n = 1; n <= 3; n++) {
          spendRecorder.record({
            id: `msg_queued_${n}`,
            model: "claude-sonnet-5-5",
            usage: { input_tokens: 1, output_tokens: 1 },
          });
        }
        const err: string[] = [];
        const started = performance.now();
        const code = await main([command], {
          out: () => {},
          err: (line) => err.push(line),
          peakRssBytes: () => 0,
          spendRecorder,
        });
        const took = performance.now() - started;
        expect({ command, code }).toEqual({ command, code: 2 });
        expect(err.join("\n")).toContain(`${command}: `);
        expect(took).toBeGreaterThanOrEqual(FLUSH_MS - 50);
        expect(took).toBeLessThan(FLUSH_MS + 500);
      }
    } finally {
      hanging.close();
    }
  }, 30_000);
});

// The architect, 2026-11-22T07:48:46Z: "Count, per report, the cases resting on the assumption,
// beside the listing measure".
describe("behaviour: the cases resting on the assumption that their session had Dispatch", () => {
  test("report counts them beside the listing on the verdict line, and gives each source of the answer in its dispatch line", async () => {
    const s = scenario("dispatch-count", LOSS_SKILL(), [
      { id: "c01" },
      { id: "c02" },
      { id: "c03" },
      { id: "c04" },
      { id: "c05" },
      { id: "c06" },
    ]);
    plantSix(s, ["c01", "c06"], "c01");
    // Four cases whose captures carry the Envoy plugin's block, each with where its answer comes
    // from, as a stage records it.
    const answers = { c02: "legion-role", c03: "assumed", c04: "assumed", c05: "transcript" };
    for (const [id, dispatch] of Object.entries(answers)) {
      const note = (arm: string) => ({
        bodyFile: `captures/${arm}-planted/${id}.body.json`,
        captureDigest: null,
        sessionDigest: null,
        dispatch,
      });
      const screen = plantedStage(s, id, "screen", KEPT);
      plantResult(s, id, {
        stages: { screen: { ...screen, captures: { old: note("old"), new: note("new") } } },
      });
    }
    const ran = await cli(report(s));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const lines = ran.stdout.split("\n");
    expect(lines).toContain(
      `verdict: no loss detected by the registered rule: no confirmed case is a loss (0 flagged of 6 informative), and the planted-loss control (case c01) is a loss and its false-loss control is no loss${unmeasured(6, 6, 2)}`
    );
    expect(lines).toContain(
      "dispatch: 4 cases carry the Envoy plugin's block of the skill: 2 rest on the assumption that their session had Dispatch, 1 take it from a Legion role, 1 from their transcript (arms.ts's dispatchOf)"
    );
  }, 60_000);

  test("a capture that carries the Envoy plugin's block records where the answer that its session had Dispatch comes from, and report counts it", async () => {
    const s = scenario("dispatch-capture", LOSS_SKILL(), [{ id: "c04" }]);
    recordCaptures(s, ["old", "new", "none"]);
    // Each saved capture's record as a capture of the plugin's block writes it.
    const texts = { old: textDigest(s.old), new: textDigest(s.new) };
    for (const name of ["old", "new", "none"] as const) {
      const c = s.cases[0] as Case;
      const arm = buildArm(c, name, { old: s.old, new: s.new }, path.join(s.dir, "arms", c.id));
      const file = capturePaths(armCaptureDir(s.out, arm, texts), c.id, name).record;
      writeFileSync(
        file,
        JSON.stringify({
          ...JSON.parse(readFileSync(file, "utf8")),
          injections:
            name === "none"
              ? []
              : [{ by: "plugin", plugin: "pi-envoy", name: "dispatch-first", at: 0 }],
          dispatch: { from: "assumed", why: "its context is devbox, and the runner assumes it" },
        })
      );
    }
    answer = behaviourStandIn;
    const ran = await cli(behaviour("screen", s));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const captures = result(s.out, "c04").stages.screen?.captures ?? {};
    expect(Object.entries(captures).map(([arm, note]) => [arm, note.dispatch])).toEqual([
      ["old", "assumed"],
      ["new", "assumed"],
      ["none", undefined],
    ]);
    const reported = await cli(report(s));
    expect(reported.stdout.split("\n")).toContain(
      "dispatch: 1 cases carry the Envoy plugin's block of the skill: 1 rest on the assumption that their session had Dispatch, 0 take it from a Legion role, 0 from their transcript (arms.ts's dispatchOf)"
    );
  }, 60_000);
});

// The architect's rule (2026-11-22T07:48:46Z): a case whose block of its skill the arms cannot
// each rebuild is dropped and counted under its kind, and the rest of the cases file runs; before,
// the whole file was refused.
describe("behaviour: a case whose block of its skill the arms cannot each rebuild", () => {
  // Catches: a case run with the recorded text of its skill in every arm because its messages
  // hold a block of it the arms do not rebuild, and the whole file refused for one such case.
  test("is dropped at screen before any model call, listed and counted by kind, and the rest of the file runs", async () => {
    const s = scenario("invoked-block", LOSS_SKILL(), [{ id: "c-invoked" }, { id: "c-plain" }]);
    const [invoked, plain] = s.cases;
    recordCaptures({ ...s, cases: s.cases.filter((c) => c.id === "c-plain") }, [
      "old",
      "new",
      "none",
    ]);
    // A `/skill:alpha` prompt the user typed: a skill-prompt message in the user's form.
    const typed: SessionMessage = {
      role: "custom",
      customType: "skill-prompt",
      content: [{ type: "text", text: "# Alpha\n\nThe user's prompt." }],
      display: true,
      details: { name: "alpha", path: "/skills/alpha/SKILL.md" },
      attribution: "user",
      timestamp: 1795139983600,
    };
    writeFileSync(
      s.casesFile,
      [
        {
          ...invoked,
          messages: [typed, ...(invoked?.messages ?? [])],
          skillReads: (invoked?.skillReads ?? []).map((index) => index + 1),
        },
        plain,
      ]
        .map((c) => `${JSON.stringify(c)}\n`)
        .join("")
    );
    answer = behaviourStandIn;
    const from = recorder.requests.length;
    const ran = await cli(behaviour("screen", s));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const fault =
      "injection-unrebuildable: user-invoked: its message 0 is a skill-prompt message of alpha, one the user invoked, not in the form of Oh My Pi's autoloaded block";
    expect(ran.stdout).toContain(`case c-invoked: dropped: ${fault}`);
    const requests = requestsSince(from);
    expect(requests.filter((request) => request.body.includes("Case c-invoked"))).toEqual([]);
    expect(requests.length).toBeGreaterThan(0);
    const dropped = result(s.out, "c-invoked");
    expect([dropped.droppedAt, dropped.injectionKinds, dropped.controls, dropped.stages]).toEqual([
      "screen",
      ["user-invoked"],
      null,
      {},
    ]);
    expect(dropped.dropped).toStartWith(fault);
    expect(result(s.out, "c-plain").stages.screen?.state).toBe("counted");

    const reported = await cli(report(s));
    expect({ code: reported.code, stderr: reported.stderr }).toEqual({ code: 0, stderr: "" });
    const lines = reported.stdout.split("\n");
    expect(lines.find((line) => line.startsWith("case c-invoked: "))).toStartWith(
      `case c-invoked: dropped at screen: ${fault}`
    );
    expect(reported.stdout).toContain(
      `; 1 dropped (0 by the judge controls, 0 model-not-served, 0 unreadable-directory, ${NO_LEGION_TOOL}, 0 listing-misses, 0 listing-adds, 1 injection-unrebuildable (user-invoked 1, other-shape 0, plugin-block-misplaced 0, autoload-no-block 0), ${NO_PATH_FORM}, ${NO_READS_PAST}, ${NO_REFUSED}), `
    );
    // The other stages read the file too, and do not refuse it.
    const confirm = await cli(behaviour("confirm", s));
    expect({ code: confirm.code, stderr: confirm.stderr }).toEqual({ code: 0, stderr: "" });
  }, 60_000);

  test("a case whose capture finds its subagent's agent autoloads the skill and its messages hold no block of it is dropped as autoload-no-block, and the rest of the file runs", async () => {
    const s = scenario("autoload-block", LOSS_SKILL(), [{ id: "c-autoload" }, { id: "c-plain" }]);
    const [autoload, plain] = s.cases;
    recordCaptures({ ...s, cases: s.cases.filter((c) => c.id === "c-plain") }, [
      "old",
      "new",
      "none",
    ]);
    const commit = "0123456789abcdef";
    const agents = {
      repo: "acme/legion",
      commit,
      path: "packages/agents-pack",
      name: "agents-pack",
    };
    writeFileSync(
      s.casesFile,
      [
        {
          ...autoload,
          context: "subagent:alpha-reviewer",
          profile: { ...autoload?.profile, plugins: [agents] },
        },
        plain,
      ]
        .map((c) => `${JSON.stringify(c)}\n`)
        .join("")
    );
    // The repositories the context reads: the made-up skills, and a plugin whose agent
    // alpha-reviewer autoloads alpha.
    const repos = path.join(s.dir, "repos");
    writeSkill(path.join(repos, "acme", "skills", commit), {
      "skills/alpha/SKILL.md": f1File(2),
      "skills/beta/SKILL.md": "---\nname: beta\ndescription: Use for the beta greeting.\n---\n",
    });
    writeSkill(path.join(repos, "acme", "legion", commit), {
      "packages/agents-pack/agents/reviewer.md":
        "---\nname: alpha-reviewer\ndescription: Reviews with alpha.\nautoloadSkills: [alpha]\n---\n\nReview.\n",
    });
    const { omp, models } = standInContext(s, "c-autoload", s.dir);
    answer = behaviourStandIn;
    const ran = await cli(
      behaviour("screen", s, ...["--models", models, "--omp", omp, "--repos", repos])
    );
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const fault = `injection-unrebuildable: autoload-no-block: case c-autoload: its agent alpha-reviewer (packages/agents-pack/agents/reviewer.md of acme/legion at ${commit}) autoloads alpha, and its messages hold no autoloaded block of it`;
    const dropped = result(s.out, "c-autoload");
    expect(dropped.dropped).toStartWith(fault);
    expect([dropped.droppedAt, dropped.injectionKinds, dropped.controls?.hold]).toEqual([
      "screen",
      ["autoload-no-block"],
      true,
    ]);
    expect(result(s.out, "c-plain").stages.screen?.state).toBe("counted");
    const reported = await cli(report(s));
    expect({ code: reported.code, stderr: reported.stderr }).toEqual({ code: 0, stderr: "" });
    expect(reported.stdout).toContain(`case c-autoload: dropped at screen: ${fault}`);
    expect(reported.stdout).toContain(
      `; 1 dropped (0 by the judge controls, 0 model-not-served, 0 unreadable-directory, ${NO_LEGION_TOOL}, 0 listing-misses, 0 listing-adds, 1 injection-unrebuildable (user-invoked 0, other-shape 0, plugin-block-misplaced 0, autoload-no-block 1), ${NO_PATH_FORM}, ${NO_READS_PAST}, ${NO_REFUSED}), `
    );
  }, 60_000);
});

// build-cases may run before the cut has a text (no --new), so a case whose file read by path the
// new text holds in a form the arms do not rebuild reaches the screen, which drops it before any
// arm is built.
describe("behaviour: a case whose new text holds a file it reads by path in a form the arms do not rebuild", () => {
  // Catches: a screen that fails the command on such a case, or that spends calls on it.
  test("is dropped at screen as path-form before any model call, listed and counted by arm, and the rest of the file runs", async () => {
    const s = scenario("path-form-screen", LOSS_SKILL(), [{ id: "c-wide" }, { id: "c-plain" }]);
    const [wide, plain] = s.cases;
    // The new text holds a file of 301 lines, more than a read by path shows whole; the captures
    // are kept under the texts' digests, so it is written first.
    writeSkill(s.new, {
      "references/wide.md": Array.from({ length: 301 }, (_, n) => `Line ${n + 1}.`).join("\n"),
    });
    recordCaptures({ ...s, cases: s.cases.filter((c) => c.id === "c-plain") }, [
      "old",
      "new",
      "none",
    ]);
    // c-wide's message 5 reads that file whole by its path in its copy, which the old text lacks.
    const dir = "/home/dev/skills/alpha";
    const target = `${dir}/references/wide.md`;
    const messages = structuredClone(wide?.messages ?? []);
    for (const part of messages[5]?.content ?? []) {
      if (part.type === "toolCall") part.arguments = { path: target };
    }
    const read = messages[6];
    if (read !== undefined) {
      read.content = [{ type: "text", text: `[${target}#${snapshotTag("Line 1.")}]\n1:Line 1.` }];
      read.details = { totalLines: 1, rowPrefixes: true };
      read.isError = false;
    }
    writeFileSync(
      s.casesFile,
      [{ ...wide, messages, skillReads: [2, 4, 6], pathCopy: { dir } }, plain]
        .map((c) => `${JSON.stringify(c)}\n`)
        .join("")
    );
    answer = behaviourStandIn;
    const from = recorder.requests.length;
    const ran = await cli(behaviour("screen", s));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const fault =
      "path-form: new: references/wide.md has 301 lines, and a read by path shows 300, then the end of each block past them";
    expect(ran.stdout).toContain(`case c-wide: dropped: ${fault}`);
    const requests = requestsSince(from);
    expect(requests.filter((request) => request.body.includes("Case c-wide"))).toEqual([]);
    expect(requests.length).toBeGreaterThan(0);
    const dropped = result(s.out, "c-wide");
    expect([dropped.dropped, dropped.droppedAt, dropped.controls, dropped.stages]).toEqual([
      fault,
      "screen",
      null,
      {},
    ]);
    expect(result(s.out, "c-plain").stages.screen?.state).toBe("counted");

    const reported = await cli(report(s));
    expect({ code: reported.code, stderr: reported.stderr }).toEqual({ code: 0, stderr: "" });
    expect(reported.stdout).toContain(`case c-wide: dropped at screen: ${fault}`);
    expect(reported.stdout).toContain(
      `; 1 dropped (0 by the judge controls, 0 model-not-served, 0 unreadable-directory, ${NO_LEGION_TOOL}, 0 listing-misses, 0 listing-adds, ${NO_INJECTION}, 1 path-form (old 0, new 1), ${NO_READS_PAST}, ${NO_REFUSED}), `
    );
  }, 60_000);
});

/** The run records of `informative` cases screened as informative (old and new pass each run,
 *  none no run), and of the first one's control runs, each with no verdict and no try left: its
 *  control is unscreened. */
function unscreenedControl(informative: number): RunRecord[] {
  const ids = Array.from({ length: informative }, (_, n) => `c${n}`);
  const screen = ids.flatMap((caseId) =>
    (["old", "new", "none"] as const).flatMap((arm) =>
      Array.from({ length: SCREEN_RUNS }, (): RunRecord => {
        const verdict = arm === "none" ? "fail" : "pass";
        return { caseId, arm, stage: "screen", verdict, reason: "" };
      })
    )
  );
  const control = Array.from(
    { length: FRESH_RUNS + MAX_EXTRA_RUNS },
    (): RunRecord => ({ caseId: "c0", arm: "old", stage: "control", verdict: null, reason: "" })
  );
  return [...screen, ...control];
}

// Catches: ": its runs are unscreened" added to a reason the control's runs are no part of.
test("a skill's no-verdict reason says its control's runs are unscreened only where the control is the reason", () => {
  expect(noVerdict(scoreSkill(unscreenedControl(MIN_INFORMATIVE_CASES - 1)))).toBe(
    `fewer than ${MIN_INFORMATIVE_CASES} informative cases: ${MIN_INFORMATIVE_CASES - 1}`
  );
  expect(noVerdict(scoreSkill(unscreenedControl(MIN_INFORMATIVE_CASES)))).toBe(
    "control not a loss: its runs are unscreened"
  );
});

// The architect's rules for a replay's reads of skill files (2026-11-22): a read of another
// installed skill is answered from the copy its capture installed, and each run records each
// answered round's reads; a run whose reply after the last answered round still only reads skill
// files gets no verdict, is run again and is counted by arm; and an arm that cannot reach its runs
// within its extra runs for such runs drops its case.
describe("behaviour: a replay's reads of skill files", () => {
  /** Beta's SKILL.md, as the cache holds the copy a capture installed from BETA_FROM. */
  const BETA_TEXT =
    "---\nname: beta\ndescription: Use before any alpha step.\n---\n\n# Beta\n\nLoad alpha before the first alpha step.\n";

  /** A replay's reply that reads `target`, as a Messages API stream, with a call id of its own. */
  let calls = 0;
  const readReply = (target: string): Response => {
    calls += 1;
    return sseResponse(
      messageSse({
        id: `msg_standin_read_${calls}`,
        model: "claude-sonnet-5-5",
        content: [
          { type: "tool_use", id: `toolu_standin_${calls}`, name: "read", input: { path: target } },
        ],
        stopReason: "tool_use",
      })
    );
  };

  /** A stand-in whose replays answer as behaviourStandIn but for each run `reads` picks at its
   *  first send, given that send's body: every reply of such a run reads alpha's reference. */
  const readingStandIn = (reads: (body: { [field: string]: unknown }) => boolean) => {
    const seen = new Set<string>();
    const readers = new Set<string>();
    return (body: { model: string; [field: string]: unknown }): string | Response => {
      if (isJudgement(body)) return behaviourStandIn(body);
      // Every round of a run goes with the run's own session id.
      const id = String(Object(body.metadata).user_id);
      if (!seen.has(id)) {
        seen.add(id);
        if (reads(body)) readers.add(id);
      }
      return readers.has(id) ? readReply("skill://alpha/references/x.md") : behaviourStandIn(body);
    };
  };

  /** The runs of each arm of `runs` that read skill files past the last answered round. */
  const pastByArm = (runs: readonly RunRecord[]) =>
    (["old", "new", "none"] as const)
      .map(
        (arm) =>
          `${arm} ${runs.filter((run) => run.arm === arm && run.reason.startsWith(READS_PAST_LAST_ROUND)).length}`
      )
      .join(", ");

  // Catches: a reply that only reads another installed skill judged as the step, so every arm
  // fails on that read; or a run record that does not say which files its rounds read.
  test("a reply that reads another installed skill is answered from the copy its capture installed, and each run records each answered round's reads and is judged on the act after them", async () => {
    const s = scenario("other-read", LOSS_SKILL(), [{ id: "c04" }]);
    recordCaptures(s, ["old", "new", "none"]);
    writeSkill(path.join(s.dir, "cache", "skills", BETA_FROM.key), { "SKILL.md": BETA_TEXT });
    // Every run reads beta first, and acts once beta's text is in its body.
    answer = (body) =>
      isJudgement(body) || JSON.stringify(body.messages).includes("Load alpha before")
        ? behaviourStandIn(body)
        : readReply("skill://beta");
    const ran = await cli(behaviour("screen", s));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const c = result(s.out, "c04");
    expect(passes(c, "screen")).toEqual({ old: SCREEN_RUNS, new: 0, none: 0 });
    const runs = c.stages.screen?.runs ?? [];
    expect(runs).toHaveLength(3 * SCREEN_RUNS);
    for (const run of runs) {
      expect(run.skillRounds).toBe(1);
      expect(run.skillReads).toEqual([
        [{ skill: "beta", file: "SKILL.md", by: "skill", from: "copy", isError: false }],
      ]);
      expect(run.reply?.[0]).toMatchObject({
        type: "tool_use",
        name: "read",
        input: { path: "skill://beta" },
      });
    }
  }, 120_000);

  // Catches: a saved capture replayed on a machine whose cache lacks the copy its record names
  // (the cache is not kept with --out), so every run that reads that skill fails with no verdict
  // (`the installed copy of beta, <dir>, is not a directory`) and its replay calls are paid.
  test("a saved capture whose cache lacks the installed copy its record names has it made again from its repository before the replays, and a read of it is answered from that copy", async () => {
    const s = scenario("other-read-restored", LOSS_SKILL(), [{ id: "c04" }]);
    recordCaptures(s, ["old", "new", "none"]);
    // The cache of a new machine holds no copy of beta; beta's repository holds it.
    const copy = path.join(s.dir, "cache", "skills", BETA_FROM.key);
    rmSync(copy, { recursive: true });
    const repos = path.join(s.dir, "repos");
    writeSkill(path.join(repos, BETA_FROM.repo, BETA_FROM.commit, BETA_FROM.path), {
      "SKILL.md": BETA_TEXT,
    });
    answer = (body) =>
      isJudgement(body) || JSON.stringify(body.messages).includes("Load alpha before")
        ? behaviourStandIn(body)
        : readReply("skill://beta");
    const ran = await cli(behaviour("screen", s, "--repos", repos));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    expect(readFileSync(path.join(copy, "SKILL.md"), "utf8")).toBe(BETA_TEXT);
    const c = result(s.out, "c04");
    expect(passes(c, "screen")).toEqual({ old: SCREEN_RUNS, new: 0, none: 0 });
    // Each run read beta in its one answered round and was judged: no replay failed, to be run
    // again.
    const runs = c.stages.screen?.runs ?? [];
    expect(runs).toHaveLength(3 * SCREEN_RUNS);
    for (const run of runs) {
      expect(run.skillReads).toEqual([
        [{ skill: "beta", file: "SKILL.md", by: "skill", from: "copy", isError: false }],
      ]);
    }
  }, 120_000);

  // Catches: a reply that still only reads skill files after the last answered round judged as
  // the step, which fails in every arm for a read alone; and a report that hides an arm that keeps
  // the agent reading skill files.
  test("a run whose replies only read skill files through every answered round gets no verdict and is run again, and its case's line and report count such runs by arm", async () => {
    const s = scenario("reads-past", LOSS_SKILL(), [{ id: "c04" }]);
    recordCaptures(s, ["old", "new", "none"]);
    let left = 2;
    answer = readingStandIn(() => left-- > 0);
    const ran = await cli(behaviour("screen", s));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const c = result(s.out, "c04");
    expect(c.stages.screen?.state).toBe("counted");
    expect(passes(c, "screen")).toEqual({ old: SCREEN_RUNS, new: 0, none: 0 });
    const runs = c.stages.screen?.runs ?? [];
    const past = runs.filter((run) => run.verdict === null);
    expect(past).toHaveLength(2);
    for (const run of past) {
      expect(run.reason).toStartWith(
        `${READS_PAST_LAST_ROUND}: after 3 answered rounds of skill reads, the reply still only reads skill files (alpha references/x.md)`
      );
      expect({ rounds: run.skillRounds, reads: run.skillReads?.length }).toEqual({
        rounds: 3,
        reads: 3,
      });
      expect({ reply: run.reply?.length, spent: run.replaySpent.length }).toEqual({
        reply: 4,
        spent: 4,
      });
    }
    // Each arm still has its judged runs: the two were run again.
    expect(runs).toHaveLength(3 * SCREEN_RUNS + 2);
    expect(ran.stdout).toContain(
      `(2 runs with no verdict, run again; ${READS_PAST_LAST_ROUND}: ${pastByArm(runs)})`
    );

    // The report gives them per stage and arm.
    const reported = await cli(report(s));
    expect({ code: reported.code, stderr: reported.stderr }).toEqual({ code: 0, stderr: "" });
    expect(reported.stdout.split("\n")).toContain(
      `${READS_PAST_LAST_ROUND}: the runs whose reply after the last of 3 answered rounds of skill reads still only read skill files, which get no verdict and are run again, by stage and arm: screen ${pastByArm(runs)}; confirm old 0, new 0; control old 0, minus 0, twin 0`
    );
    expect(
      pastByArm(runs)
        .match(/\d+/g)
        ?.reduce((sum, n) => sum + Number(n), 0)
    ).toBe(2);
  }, 120_000);

  // Catches: a case whose arm keeps the agent reading past the last answered round left
  // unscreened with no reason, or run as if the arm measured anything.
  test("an arm whose runs read skill files past the last answered round so that it cannot reach its runs within its extra runs drops the case as reads-past-last-round, listed with its counts", async () => {
    const s = scenario("reads-past-drop", LOSS_SKILL(), [{ id: "c04" }]);
    recordCaptures(s, ["old", "new", "none"]);
    // Every run of the none arm, whose captured body holds the user's message alone, reads.
    answer = readingStandIn((body) => (body.messages as unknown[]).length === 1);
    const ran = await cli(behaviour("screen", s));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const all = SCREEN_RUNS + MAX_EXTRA_RUNS;
    const reason = `${READS_PAST_LAST_ROUND}: the none arm has 0 of its ${SCREEN_RUNS} judged runs after ${all} runs, ${all} of which still only read skill files after the last answered round (runs that read past the last round: old 0, new 0, none ${all})`;
    const c = result(s.out, "c04");
    expect({ dropped: c.dropped, at: c.droppedAt, state: c.stages.screen?.state }).toEqual({
      dropped: reason,
      at: "screen",
      state: "unscreened",
    });
    expect(ran.stdout).toContain(`case c04: dropped: ${reason}; `);
    const reported = await cli(report(s));
    expect(reported.stdout).toContain(
      `case c04: dropped at screen: ${reason}; screen: unscreened: an arm lacks its ${SCREEN_RUNS} judged runs after ${MAX_EXTRA_RUNS} extra runs (${all} runs with no verdict, run again; ${READS_PAST_LAST_ROUND}: old 0, new 0, none ${all})\n`
    );
    expect(reported.stdout).toContain(
      `, 1 ${READS_PAST_LAST_ROUND}, ${NO_REFUSED}), 0 unscreened, 0 not run`
    );
  }, 120_000);
});

// The provider's content policy (the architect, 2026-11-24): a case whose every replay the provider
// refuses with a stream error of a `*_policy` type can never be judged, so its stage drops it as
// provider-refused, where it was left unscreened as if a later run could judge it.
describe("behaviour: a case the provider refuses on every replay", () => {
  /** What the stand-in provider says of the content it refuses. */
  const POLICY_TEXT = "This content was flagged for possible cybersecurity risk.";
  /** The reason of a refused replay as a run saved before the gateway stopped trying a policy
   *  refusal again holds it: GATEWAY_TRIES tries, named at its end. */
  const REFUSED_RUN = `the replay failed: the stream ended in an error: cyber_policy: ${POLICY_TEXT}, on each of ${GATEWAY_TRIES} tries`;
  /** The reason of another failure that names a policy outside a stream error's type. */
  const OTHER_FAILURE =
    "the replay failed: the model answered 400 invalid_request_error: the body names cyber_policy, which is no stream error";
  /** A screen's runs per arm: its runs and its extra runs. */
  const SCREEN_ALL = SCREEN_RUNS + MAX_EXTRA_RUNS;
  /** The drop of a case whose screen the provider refused on every replay. */
  const SCREEN_REFUSED = `provider-refused: provider refused every replay (cyber_policy): ${3 * SCREEN_ALL} refusals (old ${SCREEN_ALL}, new ${SCREEN_ALL}, none ${SCREEN_ALL})`;

  /** Case `caseId`'s `stage` as a run saved it unscreened with the texts of `s`, and no drop: each
   *  arm of `byArm` holding one run per reason, a judged pass for "pass", else a run with no
   *  verdict and that reason. */
  const unscreenedStage = (
    s: Scenario,
    caseId: string,
    stage: Stage,
    byArm: Record<string, readonly string[]>
  ): Record<string, unknown> => ({
    ...plantedStage(s, caseId, stage, {}),
    state: "unscreened",
    runs: Object.entries(byArm).flatMap(([arm, reasons]) =>
      reasons.map((reason, n) =>
        reason === "pass"
          ? plantedRun(caseId, arm, stage, n, "pass", "planted")
          : plantedRun(caseId, arm, stage, n, null, reason)
      )
    ),
  });

  /** Each arm's runs when every one is a refusal: the stage's runs and its extra runs. */
  const allRefused = (stage: Stage) => Array(STAGE_RUNS[stage] + MAX_EXTRA_RUNS).fill(REFUSED_RUN);

  // Catches: a case whose every replay the provider refuses left unscreened, as if a later run
  // could judge it, and counted as unscreened by the report; and a refused call logged with no
  // error name of its own and no status, where a call that fails in postMessages logs both.
  test("a case whose every replay the provider refuses on a content policy is dropped at screen as provider-refused with its refusals by arm, each refused call is logged as a MessagesApiError of status 200, the report counts it, and the screen goes on to the next case", async () => {
    const s = scenario("provider-refused", LOSS_SKILL(), [
      { id: "c04", marker: "REFUSED" },
      { id: "c05" },
    ]);
    recordCaptures(s, ["old", "new", "none"]);
    // The provider refuses each replay of c04 with a Messages stream of one error event of type
    // cyber_policy, which gateway.ts throws at once as `the stream ended in an error: <type>:
    // <message>`, and does not try again.
    answer = (body) =>
      !isJudgement(body) && JSON.stringify(body.messages).includes("REFUSED")
        ? sseResponse(
            `event: error\ndata: ${JSON.stringify({
              type: "error",
              error: { type: "cyber_policy", message: POLICY_TEXT },
            })}\n\n`
          )
        : behaviourStandIn(body);
    const ran = await cli(behaviour("screen", s));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const c = result(s.out, "c04");
    expect({ dropped: c.dropped, at: c.droppedAt, state: c.stages.screen?.state }).toEqual({
      dropped: SCREEN_REFUSED,
      at: "screen",
      state: "unscreened",
    });
    const runs = c.stages.screen?.runs ?? [];
    expect(runs).toHaveLength(3 * SCREEN_ALL);
    for (const run of runs) {
      expect({ verdict: run.verdict, reason: run.reason }).toEqual({
        verdict: null,
        reason: `the replay failed: the stream ended in an error: cyber_policy: ${POLICY_TEXT}`,
      });
    }
    // The request log names each refused call's error and the status of its answer.
    const failed = readFileSync(path.join(s.out, "requests.jsonl"), "utf8")
      .trim()
      .split("\n")
      .map((line) => JSON.parse(line))
      .filter((line) => line.kind === "replay" && line.error !== undefined);
    expect(failed.map((line) => `${line.error} ${line.status}`)).toEqual(
      Array(3 * SCREEN_ALL).fill("MessagesApiError 200")
    );
    expect(ran.stdout).toContain(`case c04: dropped: ${SCREEN_REFUSED}; `);
    // The screen goes on to the next case.
    expect(result(s.out, "c05").stages.screen?.state).toBe("counted");
    const reported = await cli(report(s));
    expect({ code: reported.code, stderr: reported.stderr }).toEqual({ code: 0, stderr: "" });
    expect(reported.stdout).toContain(
      `case c04: dropped at screen: ${SCREEN_REFUSED}; screen: unscreened: an arm lacks its ${SCREEN_RUNS} judged runs after ${MAX_EXTRA_RUNS} extra runs (${3 * SCREEN_ALL} runs with no verdict, run again)\n`
    );
    expect(reported.stdout).toContain(
      `; 1 dropped (0 by the judge controls, 0 model-not-served, 0 unreadable-directory, ${NO_LEGION_TOOL}, 0 listing-misses, 0 listing-adds, ${NO_INJECTION}, ${NO_PATH_FORM}, ${NO_READS_PAST}, 1 provider-refused), 0 unscreened, 0 not run`
    );
  }, 120_000);

  // Catches: a screen saved unscreened before the rule, every replay refused, skipped as
  // "screened before" and left unscreened for good; or dropped by calling the model again.
  test("a screen saved unscreened with every replay refused and no drop is dropped as provider-refused by a later screen, from its saved runs, with no model call", async () => {
    const s = scenario("provider-refused-saved", LOSS_SKILL(), [{ id: "c04" }]);
    plantResult(s, "c04", {
      stages: {
        screen: unscreenedStage(s, "c04", "screen", {
          old: allRefused("screen"),
          new: allRefused("screen"),
          none: allRefused("screen"),
        }),
      },
    });
    const from = recorder.requests.length;
    const ran = await cli(behaviour("screen", s));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    expect(requestsSince(from)).toHaveLength(0);
    expect(ran.stdout).not.toContain("case c04: screened before, skipped");
    expect(ran.stdout).toContain(`case c04: dropped: ${SCREEN_REFUSED}; `);
    const c = result(s.out, "c04");
    expect({ dropped: c.dropped, at: c.droppedAt, runs: c.stages.screen?.runs.length }).toEqual({
      dropped: SCREEN_REFUSED,
      at: "screen",
      runs: 3 * SCREEN_ALL,
    });
    // A screen started again skips the dropped case.
    const again = await cli(behaviour("screen", s));
    expect(again.stdout).toContain(`case c04: dropped before, skipped: ${SCREEN_REFUSED}`);
    const reported = await cli(report(s));
    expect(reported.stdout).toContain(
      `; 1 dropped (0 by the judge controls, 0 model-not-served, 0 unreadable-directory, ${NO_LEGION_TOOL}, 0 listing-misses, 0 listing-adds, ${NO_INJECTION}, ${NO_PATH_FORM}, ${NO_READS_PAST}, 1 provider-refused), 0 unscreened, 0 not run`
    );
  }, 60_000);

  // Catches: a control case whose saved control runs the provider refused every time kept as the
  // control case for good, so the skill could never get its controls.
  test("a control stage saved unscreened with every replay refused is dropped at control as provider-refused by a later control, with no model call, and the next case of the control rule's order takes its place", async () => {
    const SIX = ["c01", "c02", "c03", "c04", "c05", "c06"];
    const s = scenario(
      "provider-refused-control",
      LOSS_SKILL(),
      SIX.map((id) => ({ id }))
    );
    // Six informative cases, the control rule's first of them (c01) with its control saved so.
    for (const id of SIX.slice(1)) {
      plantResult(s, id, { stages: { screen: plantedStage(s, id, "screen", KEPT) } });
    }
    plantResult(s, "c01", {
      stages: {
        screen: plantedStage(s, "c01", "screen", KEPT),
        control: unscreenedStage(s, "c01", "control", {
          old: allRefused("control"),
          minus: allRefused("control"),
          twin: allRefused("control"),
        }),
      },
    });
    const all = STAGE_RUNS.control + MAX_EXTRA_RUNS;
    const reason = `provider-refused: provider refused every replay (cyber_policy): ${3 * all} refusals (old ${all}, minus ${all}, twin ${all})`;
    const from = recorder.requests.length;
    const ran = await cli(behaviour("control", s));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    expect(requestsSince(from)).toHaveLength(0);
    expect(ran.stdout).not.toContain("case c01: control runs made before, skipped");
    expect(ran.stdout).toContain(`case c01: dropped: ${reason}; `);
    const c01 = result(s.out, "c01");
    expect({ dropped: c01.dropped, at: c01.droppedAt }).toEqual({ dropped: reason, at: "control" });
    const reported = await cli(report(s));
    expect(reported.stdout.split("\n")).toContain(
      "control case: c02, in place of c01 (dropped at control), which the control rule chose before it"
    );
    expect(reported.stdout).toContain(`, ${NO_READS_PAST}, 1 provider-refused), `);
  }, 60_000);

  // Catches: a screen whose old and new arms the provider refused on every replay, its none arm
  // judged, left unscreened as if a later run could judge old and new (dotfiles-93-46c17afdabdd),
  // since the drop asked every run of the stage to be a refusal.
  test("a screen whose old and new arms the provider refuses on every replay, with its none arm judged, is dropped at screen as provider-refused naming old and new, with the refusals of each arm", async () => {
    const s = scenario("provider-refused-arms", LOSS_SKILL(), [{ id: "c04", marker: "REFUSED" }]);
    recordCaptures(s, ["old", "new", "none"]);
    // The provider refuses each replay of c04's old and new arms, whose bodies hold the skill's
    // reads; the none arm's body holds the user's message alone, and is answered.
    answer = (body) =>
      !isJudgement(body) &&
      JSON.stringify(body.messages).includes("REFUSED") &&
      (body.messages as unknown[]).length > 1
        ? sseResponse(
            `event: error\ndata: ${JSON.stringify({
              type: "error",
              error: { type: "cyber_policy", message: POLICY_TEXT },
            })}\n\n`
          )
        : behaviourStandIn(body);
    const ran = await cli(behaviour("screen", s));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const reason = `provider-refused: provider refused every replay of old and new (cyber_policy): ${2 * SCREEN_ALL} refusals (old ${SCREEN_ALL}, new ${SCREEN_ALL}, none 0)`;
    const c = result(s.out, "c04");
    expect({ dropped: c.dropped, at: c.droppedAt, state: c.stages.screen?.state }).toEqual({
      dropped: reason,
      at: "screen",
      state: "unscreened",
    });
    expect(passes(c, "screen")).toEqual({ old: 0, new: 0, none: 0 });
    expect(
      c.stages.screen?.runs.filter((run) => run.arm === "none" && run.verdict !== null)
    ).toHaveLength(SCREEN_RUNS);
    expect(ran.stdout).toContain(`case c04: dropped: ${reason}; `);
    const reported = await cli(report(s));
    expect(reported.stdout).toContain(`case c04: dropped at screen: ${reason}; `);
    expect(reported.stdout).toContain(`, ${NO_READS_PAST}, 1 provider-refused), 0 unscreened, `);
  }, 120_000);

  // Catches: a stage left unscreened whose one arm short of its runs the provider refused, kept
  // unscreened because its other arms were judged; or a stage dropped as provider-refused though an
  // arm short of its runs has a run that failed for another reason (a policy named outside a
  // stream error's type among them), which a later run could get past.
  test("a saved unscreened stage whose one arm short of its runs the provider refused is dropped naming that arm, though its other arms were judged; one whose arm short of its runs has a run that failed for another reason stays unscreened; and the report counts each", async () => {
    const s = scenario("provider-refused-mixed", LOSS_SKILL(), [
      { id: "c01" },
      { id: "c02" },
      { id: "c03" },
    ]);
    const passed = Array(SCREEN_RUNS).fill("pass");
    // The none arm refused, the old and new arms judged.
    plantResult(s, "c01", {
      stages: {
        screen: unscreenedStage(s, "c01", "screen", {
          old: passed,
          new: passed,
          none: allRefused("screen"),
        }),
      },
    });
    // Each arm refused, but one of the none arm's runs failed for another reason.
    plantResult(s, "c02", {
      stages: {
        screen: unscreenedStage(s, "c02", "screen", {
          old: allRefused("screen"),
          new: allRefused("screen"),
          none: [...allRefused("screen").slice(1), OTHER_FAILURE],
        }),
      },
    });
    // The old arm refused, the new arm failed for another reason, the none arm judged.
    plantResult(s, "c03", {
      stages: {
        screen: unscreenedStage(s, "c03", "screen", {
          old: allRefused("screen"),
          new: Array(SCREEN_ALL).fill(OTHER_FAILURE),
          none: passed,
        }),
      },
    });
    const from = recorder.requests.length;
    const ran = await cli(behaviour("screen", s));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    expect(requestsSince(from)).toHaveLength(0);
    const noneRefused = `provider-refused: provider refused every replay of none (cyber_policy): ${SCREEN_ALL} refusals (old 0, new 0, none ${SCREEN_ALL})`;
    expect(ran.stdout).toContain(`case c01: dropped: ${noneRefused}; `);
    const c01 = result(s.out, "c01");
    expect({ dropped: c01.dropped, at: c01.droppedAt }).toEqual({
      dropped: noneRefused,
      at: "screen",
    });
    for (const id of ["c02", "c03"]) {
      expect(ran.stdout).toContain(`case ${id}: screened before, skipped`);
      const c = result(s.out, id);
      expect({ id, dropped: c.dropped, state: c.stages.screen?.state }).toEqual({
        id,
        dropped: null,
        state: "unscreened",
      });
    }
    const reported = await cli(report(s));
    expect(reported.stdout).toContain(
      `; 1 dropped (0 by the judge controls, 0 model-not-served, 0 unreadable-directory, ${NO_LEGION_TOOL}, 0 listing-misses, 0 listing-adds, ${NO_INJECTION}, ${NO_PATH_FORM}, ${NO_READS_PAST}, 1 provider-refused), 2 unscreened, 0 not run`
    );
  }, 60_000);
});

// The runner of each run (a tree whose runner is built on this one found a screen that skipped
// screens another runner made as its own, and a report that pooled them with no word): each run
// names the runner that made it; a stage command takes no saved stage of another runner as done;
// and report refuses results of more than one runner unless told to report them together.
describe("behaviour: the runner of each run", () => {
  /** Digests no runner of this tree has: two other runners'. */
  const OTHER = "0123456789abcdef";
  const ANOTHER = "fedcba9876543210";

  // Catches: a run that does not name the runner that made it, and a report that does not say
  // which runners made the results it counts.
  test("each run records this runner, and report of one runner's results names it with its case stages and runs", async () => {
    const s = scenario("runner-one", LOSS_SKILL(), [{ id: "c04" }, { id: "c05" }]);
    recordCaptures(s, ["old", "new", "none"]);
    answer = behaviourStandIn;
    usage = { input: 10, output: 3 };
    const screen = await cli(behaviour("screen", s));
    expect({ code: screen.code, stderr: screen.stderr }).toEqual({ code: 0, stderr: "" });
    for (const id of ["c04", "c05"]) {
      const runs = result(s.out, id).stages.screen?.runs ?? [];
      expect(runs).toHaveLength(3 * SCREEN_RUNS);
      expect(new Set(runs.map((run) => run.runner))).toEqual(new Set([RUNNER]));
    }
    const reported = await cli(report(s));
    expect({ code: reported.code, stderr: reported.stderr }).toEqual({ code: 0, stderr: "" });
    expect(reported.stdout.split("\n")[1]).toBe(
      `runners: screen: ${RUNNER} (2 case stages, 60 runs); confirm: none; control: none`
    );
  }, 120_000);

  // Catches: a report that pools the stages of two runners with no word, and one that counts a
  // stage whose runs two runners made as one runner's.
  test("report refuses results two runners made, naming each with its case stages and runs, and with --allow-runners naming both reports them and says so", async () => {
    const s = scenario("runner-two", LOSS_SKILL(), [{ id: "c01" }, { id: "c02" }]);
    plantResult(s, "c01", { stages: { screen: plantedStage(s, "c01", "screen", KEPT, OTHER) } });
    plantResult(s, "c02", {
      stages: { screen: plantedStage(s, "c02", "screen", FLAGGED, ANOTHER) },
    });
    const byStage = `screen: ${OTHER} (1 case stage, 30 runs), ${ANOTHER} (1 case stage, 30 runs); confirm: none; control: none`;

    const refused = await cli(report(s));
    expect({ code: refused.code, stdout: refused.stdout }).toEqual({ code: 2, stdout: "" });
    expect(refused.stderr).toContain(
      `the case results in ${path.join(s.out, "cases")} were made by 2 runners, so report counts none of them: ${byStage}; no --allow-runners is given. Give --allow-runners ${OTHER},${ANOTHER} to report them together`
    );
    const one = await cli([...report(s), "--allow-runners", OTHER]);
    expect({ code: one.code, stdout: one.stdout }).toEqual({ code: 2, stdout: "" });
    expect(one.stderr).toContain(`; --allow-runners does not name ${ANOTHER}. `);

    const allowed = await cli([...report(s), "--allow-runners", `${ANOTHER},${OTHER}`]);
    expect({ code: allowed.code, stderr: allowed.stderr }).toEqual({ code: 0, stderr: "" });
    const lines = allowed.stdout.split("\n");
    const both =
      "2 runners, each allowed by --allow-runners, so their results are reported together";
    expect(lines[1]).toBe(`runners: ${byStage}; ${both}`);
    expect(lines).toContain(
      "case c02: screen: old 10/10, new 0/10, none 0/10: informative, flagged"
    );

    // OTHER started c02's screen, and ANOTHER went on with it: its first 15 runs name no runner,
    // so they are its stage's, OTHER's, and the other 15 name ANOTHER. The stage counts under each.
    const started = plantedStage(s, "c02", "screen", FLAGGED, OTHER);
    const runs = (started.runs as Record<string, unknown>[]).map((run, n) =>
      n < 15 ? run : { ...run, runner: ANOTHER }
    );
    plantResult(s, "c02", { stages: { screen: { ...started, runs } } });
    const byRun = `screen: ${OTHER} (2 case stages, 45 runs), ${ANOTHER} (1 case stage, 15 runs); confirm: none; control: none`;
    const mixed = await cli(report(s));
    expect(mixed.code).toBe(2);
    expect(mixed.stderr).toContain(`report counts none of them: ${byRun}; `);
    const named = await cli([...report(s), "--allow-runners", `${OTHER},${ANOTHER}`]);
    expect({ code: named.code, stderr: named.stderr }).toEqual({ code: 0, stderr: "" });
    expect(named.stdout.split("\n")[1]).toBe(`runners: ${byRun}; ${both}`);
  }, 60_000);

  // Catches: a screen that skips a screen another runner made as screened before, or goes on with
  // one it started, with no word.
  test("screen refuses saved screens another runner made, naming each case, its runner and this runner, before any model call; with --allow-runners naming it, skips the finished one as done and goes on with the running one, whose new runs name this runner", async () => {
    const s = scenario("runner-screen", LOSS_SKILL(), [{ id: "c04" }, { id: "c05" }]);
    recordCaptures(s, ["old", "new", "none"]);
    answer = behaviourStandIn;
    usage = { input: 10, output: 3 };
    plantResult(s, "c04", { stages: { screen: plantedStage(s, "c04", "screen", FLAGGED, OTHER) } });
    // c05's screen, which OTHER started, holds its old arm's runs only.
    const running = plantedStage(s, "c05", "screen", { old: SCREEN_RUNS }, OTHER);
    plantResult(s, "c05", { stages: { screen: { ...running, state: "running" } } });

    let from = recorder.requests.length;
    const refused = await cli(behaviour("screen", s));
    expect(refused.code).toBe(2);
    expect(refused.stderr).toContain(
      `case c04's screen in ${caseResultFile(s.out, "c04")} was made by runner ${OTHER}; case c05's screen in ${caseResultFile(s.out, "c05")} was made by runner ${OTHER}, and this runner is ${RUNNER}: `
    );
    expect(refused.stderr).toContain(
      `Give --allow-runners ${OTHER} to take those stages as they are, or use another --out`
    );
    expect(refused.stdout).not.toContain("screened before, skipped");
    expect(requestsSince(from)).toHaveLength(0);

    from = recorder.requests.length;
    const allowed = await cli(behaviour("screen", s, "--allow-runners", OTHER));
    expect({ code: allowed.code, stderr: allowed.stderr }).toEqual({ code: 0, stderr: "" });
    expect(allowed.stdout).toContain("case c04: screened before, skipped");
    // c05's judge controls, then 10 replays of new and of none, and their judgements.
    expect(requestsSince(from)).toHaveLength(3 + 2 * 2 * SCREEN_RUNS);
    const c05 = result(s.out, "c05").stages.screen;
    expect(c05?.state).toBe("counted");
    expect(c05?.runs).toHaveLength(3 * SCREEN_RUNS);
    expect(new Set(c05?.runs.map((run) => `${run.arm} ${run.runner ?? "its stage's"}`))).toEqual(
      new Set(["old its stage's", `new ${RUNNER}`, `none ${RUNNER}`])
    );

    // Its runs are now two runners', so report reports them only with both named.
    expect((await cli(report(s))).code).toBe(2);
    const reported = await cli([...report(s), "--allow-runners", `${OTHER},${RUNNER}`]);
    expect({ code: reported.code, stderr: reported.stderr }).toEqual({ code: 0, stderr: "" });
    expect(reported.stdout.split("\n")[1]).toBe(
      `runners: screen: ${OTHER} (2 case stages, 40 runs), ${RUNNER} (1 case stage, 20 runs); confirm: none; control: none; 2 runners, each allowed by --allow-runners, so their results are reported together`
    );
  }, 120_000);

  // Catches: a refusal whose advice named only the runners not yet allowed, so the run that took
  // it, its --allow-runners replaced, refused the stages the first value allowed.
  test("screen's refusal of a saved screen another runner made gives, as its --allow-runners advice, the runners already allowed and the others", async () => {
    const s = scenario("runner-union", LOSS_SKILL(), [{ id: "c04" }, { id: "c05" }]);
    recordCaptures(s, ["old", "new", "none"]);
    answer = behaviourStandIn;
    usage = { input: 10, output: 3 };
    plantResult(s, "c04", { stages: { screen: plantedStage(s, "c04", "screen", FLAGGED, OTHER) } });
    plantResult(s, "c05", {
      stages: { screen: plantedStage(s, "c05", "screen", FLAGGED, ANOTHER) },
    });
    const from = recorder.requests.length;
    const refused = await cli(behaviour("screen", s, "--allow-runners", OTHER));
    expect(refused.code).toBe(2);
    expect(refused.stderr).toContain(
      `case c05's screen in ${caseResultFile(s.out, "c05")} was made by runner ${ANOTHER}, and this runner is ${RUNNER}: `
    );
    expect(refused.stderr).toContain(
      `Give --allow-runners ${OTHER},${ANOTHER} to take those stages as they are, or use another --out`
    );
    expect(requestsSince(from)).toHaveLength(0);
  }, 60_000);
});

// The report's spend (the same tree found a report whose spend matched neither its
// screen command's line nor its request log): the report says what its sum is made of, part by
// part, and sets it against the request log.
describe("behaviour: the report's spend", () => {
  /** The dollars of each call `spent` records. */
  const usdSum = (spent: readonly { usd: number }[]) =>
    spent.reduce((sum, each) => sum + each.usd, 0);
  /** The calls no run records, as the report names them. */
  const UNRECORDED =
    "its captures' hint calls, the rounds a replay that then failed had answered, replies whose judgement the budget refused";

  // Catches: a spend that does not say what it sums, and one that leaves out the calls of a stage
  // no run records (a failed replay's answered round, a capture's hint calls).
  test("report's spend is the sum of the parts its case results record, each named: the judge controls, the replays and judgements of the runs (a run with no verdict, run again, among them), and the calls no run records (a failed replay's answered round, a capture's hint call)", async () => {
    const s = scenario("spend-parts", LOSS_SKILL(), [{ id: "c04" }]);
    recordCaptures(s, ["old", "new", "none"]);
    // 100,000 prompt tokens and no output a reply: $0.40 a judgement at claude-opus-5-5's list
    // price, $0.20 a replay call at claude-sonnet-5-5's.
    usage = { input: 100_000, output: 0 };
    // The first run of the old arm sent (its body holds the case's line) reads alpha's reference
    // first, which the replay answers; its next call is refused (400), so the run gets no verdict
    // and is run again, and its first call was paid.
    let failing: string | undefined;
    let failingCalls = 0;
    answer = (body) => {
      if (isJudgement(body)) return behaviourStandIn(body);
      const id = String(Object(body.metadata).user_id);
      if (failing === undefined && JSON.stringify(body.messages).includes(LINE)) failing = id;
      if (id !== failing) return behaviourStandIn(body);
      failingCalls += 1;
      return failingCalls === 1
        ? sseResponse(
            messageSse({
              id: "msg_standin_spend_read",
              model: "claude-sonnet-5-5",
              content: [
                {
                  type: "tool_use",
                  id: "toolu_standin_spend_read",
                  name: "read",
                  input: { path: "skill://alpha/references/x.md" },
                },
              ],
              stopReason: "tool_use",
              inputTokens: usage.input,
              outputTokens: usage.output,
            })
          )
        : Response.json(
            {
              type: "error",
              error: { type: "invalid_request_error", message: "the stand-in refuses this body" },
            },
            { status: 400 }
          );
    };
    const ran = await cli(behaviour("screen", s));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    expect(failingCalls).toBe(2);

    // A capture made in the stage adds its hint run's calls to the stage's dollars, and a `hint`
    // line each to the request log (capture.ts's gatewayHints, through the log); no Oh My Pi runs
    // here, so one hint call of $0.0500 is added to the saved stage and to the log as such a
    // capture adds it.
    const HINT = 0.05;
    const log = path.join(s.out, "requests.jsonl");
    const saved = result(s.out, "c04");
    const screened = saved.stages.screen;
    if (screened === undefined) throw new Error("c04 has no screen");
    writeFileSync(
      caseResultFile(s.out, "c04"),
      JSON.stringify({
        ...saved,
        stages: { ...saved.stages, screen: { ...screened, usd: screened.usd + HINT } },
      })
    );
    appendFileSync(
      log,
      `${JSON.stringify({ kind: "hint", model: "claude-sonnet-5-5", usd: HINT })}\n`
    );

    // The parts, from the records: the run with no verdict records no replay call, and the log
    // holds its one paid call under its session id.
    const c = result(s.out, "c04");
    const runs = c.stages.screen?.runs ?? [];
    const failed = runs.filter((run) => run.verdict === null);
    expect(runs).toHaveLength(3 * SCREEN_RUNS + 1);
    expect(failed).toHaveLength(1);
    expect(failed[0]?.reason).toStartWith("the replay failed: ");
    expect(failed[0]?.replaySpent).toEqual([]);
    const logged = readFileSync(log, "utf8")
      .trim()
      .split("\n")
      .map((line) => JSON.parse(line));
    const answered = logged.filter(
      (line) => line.kind === "replay" && line.userId === failed[0]?.userId && line.usd > 0
    );
    expect(answered).toHaveLength(1);
    const controls = c.controls?.usd ?? 0;
    const replays = runs.reduce((sum, run) => sum + usdSum(run.replaySpent), 0);
    const judgements = runs.reduce((sum, run) => sum + usdSum(run.judgeSpent), 0);
    const unrecorded = (c.stages.screen?.usd ?? 0) - replays - judgements;
    expect(unrecorded).toBeCloseTo(answered[0].usd + HINT, 10);
    // Three judge controls; 30 judged runs' replay calls and judgements; the failed run's call and
    // the hint call.
    expect([controls, replays, judgements, unrecorded].map((usd) => usd.toFixed(4))).toEqual([
      "1.2000",
      "6.0000",
      "12.0000",
      "0.2500",
    ]);
    const total = controls + replays + judgements + unrecorded;

    const reported = await cli(report(s));
    expect({ code: reported.code, stderr: reported.stderr }).toEqual({ code: 0, stderr: "" });
    const lines = reported.stdout.split("\n");
    const at = lines.findIndex((line) => line.startsWith("spend at list price: "));
    expect(lines.slice(at, at + 5)).toEqual([
      `spend at list price: $${total.toFixed(4)}, the sum of what the case results in ${path.join(s.out, "cases")} record their calls cost (judge controls $${controls.toFixed(4)}, screen $${(replays + judgements + unrecorded).toFixed(4)}, confirm $0.0000, control $0.0000):`,
      `  judge controls: $${controls.toFixed(4)}, the judge controls of 1 case`,
      `  screen: $${(replays + judgements + unrecorded).toFixed(4)} over 1 case stage: $${replays.toFixed(4)} for the replays and $${judgements.toFixed(4)} for the judgements of their 31 runs (1 with no verdict), and $${unrecorded.toFixed(4)} for the calls no run records (${UNRECORDED})`,
      `  confirm: $0.0000 over 0 case stages: $0.0000 for the replays and $0.0000 for the judgements of their 0 runs (0 with no verdict), and $0.0000 for the calls no run records (${UNRECORDED})`,
      `  control: $0.0000 over 0 case stages: $0.0000 for the replays and $0.0000 for the judgements of their 0 runs (0 with no verdict), and $0.0000 for the calls no run records (${UNRECORDED})`,
    ]);
    expect(total.toFixed(4)).toBe("19.4500");
    // The log holds each of those calls once: its total is the report's.
    const judgeCalls = logged.filter((line) => line.kind === "judge");
    const replayCalls = logged.filter((line) => line.kind === "replay");
    expect(lines[at + 5]).toBe(
      `request log ${log}: $${total.toFixed(4)} over ${logged.length} calls (judge ${judgeCalls.length} calls $${(controls + judgements).toFixed(4)}, replay ${replayCalls.length} calls $${(replays + answered[0].usd).toFixed(4)}, hint 1 call $${HINT.toFixed(4)}), the same as the spend the case results record; by kind of call:`
    );
  }, 120_000);

  // Catches: a report whose spend differs from its request log with no word of why, as a run whose
  // results were taken out by hand does.
  test("report sets its spend against the request log: the log's total, its difference, and each part of the difference with its cause, where a case's result was taken out of the directory; and against a log that holds none of its calls, and none", async () => {
    const s = scenario("spend-log", LOSS_SKILL(), [{ id: "c04" }, { id: "c05" }]);
    recordCaptures(s, ["old", "new", "none"]);
    usage = { input: 100_000, output: 0 };
    answer = behaviourStandIn;
    const ran = await cli(behaviour("screen", s));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const log = path.join(s.out, "requests.jsonl");
    const cases = path.join(s.out, "cases");
    // c05's result is taken out by hand; its calls stay in the log. And the log ends in the start
    // of a line, as one a process was stopped in the middle of writing.
    const gone = result(s.out, "c05");
    rmSync(caseResultFile(s.out, "c05"));
    appendFileSync(log, '{"at":"2026-11-24T00:00:00.000Z","kind":"rep');
    const kept = result(s.out, "c04");
    const runsOf = (r: CaseResult) => r.stages.screen?.runs ?? [];
    const judged = (r: CaseResult) =>
      (r.controls?.usd ?? 0) + runsOf(r).reduce((sum, run) => sum + usdSum(run.judgeSpent), 0);
    const replayed = (r: CaseResult) =>
      runsOf(r).reduce((sum, run) => sum + usdSum(run.replaySpent), 0);
    const total = (r: CaseResult) => (r.controls?.usd ?? 0) + (r.stages.screen?.usd ?? 0);
    expect([total(kept), judged(gone), replayed(gone)].map((usd) => usd.toFixed(4))).toEqual([
      "19.2000",
      "13.2000",
      "6.0000",
    ]);

    const reported = await cli(report(s));
    expect({ code: reported.code, stderr: reported.stderr }).toEqual({ code: 0, stderr: "" });
    const lines = reported.stdout.split("\n");
    const at = lines.findIndex((line) => line.startsWith(`request log ${log}: `));
    expect(lines.slice(at, at + 3)).toEqual([
      `request log ${log}: $${(total(kept) + total(gone)).toFixed(4)} over 126 calls (judge 66 calls $${(judged(kept) + judged(gone)).toFixed(4)}, replay 60 calls $${(replayed(kept) + replayed(gone)).toFixed(4)}), 1 line that is no JSON left out, $${total(gone).toFixed(4)} more than the spend the case results record; by kind of call:`,
      `  judge (the results' judge controls and judgements): $${(judged(kept) + judged(gone)).toFixed(4)} in the log, $${judged(kept).toFixed(4)} in the results, $${judged(gone).toFixed(4)} more in the log: judge calls no saved result records: the judge controls or judgements of a case stopped before they were saved, or of a result no longer in ${cases}`,
      `  replay and hint (the results' replays and the calls no run records): $${(replayed(kept) + replayed(gone)).toFixed(4)} in the log, $${replayed(kept).toFixed(4)} in the results, $${replayed(gone).toFixed(4)} more in the log: calls no saved stage records: a round stopped before it was saved, a result no longer in ${cases}, or the hint calls of a capture that failed; 30 replay calls of the log, $${replayed(gone).toFixed(4)}, went with a session id no run of the results holds: runs of a round stopped before it was saved, of a result no longer in ${cases}, or replies whose judgement the budget refused`,
    ]);

    // A log that holds none of the results' calls: the results record them, and their runs' session
    // ids are in no line of it.
    const other = path.join(s.dir, "other-requests.jsonl");
    writeFileSync(other, "");
    const against = await cli([...report(s), "--request-log", other]);
    expect({ code: against.code, stderr: against.stderr }).toEqual({ code: 0, stderr: "" });
    const elsewhere = "results made with another --request-log, or brought from another --out";
    expect(against.stdout).toContain(
      `request log ${other}: $0.0000 over 0 calls (none), $${total(kept).toFixed(4)} less than the spend the case results record; by kind of call:\n  judge (the results' judge controls and judgements): $0.0000 in the log, $${judged(kept).toFixed(4)} in the results, $${judged(kept).toFixed(4)} less in the log: the results record calls this log does not hold: ${elsewhere}\n  replay and hint (the results' replays and the calls no run records): $0.0000 in the log, $${replayed(kept).toFixed(4)} in the results, $${replayed(kept).toFixed(4)} less in the log: the results record calls this log does not hold: ${elsewhere}; 30 runs of the results, $${(replayed(kept) + judged(kept) - (kept.controls?.usd ?? 0)).toFixed(4)}, went with a session id no replay call of the log holds: ${elsewhere}\n`
    );
    const none = path.join(s.dir, "no-requests.jsonl");
    const absent = await cli([...report(s), "--request-log", none]);
    expect({ code: absent.code, stderr: absent.stderr }).toEqual({ code: 0, stderr: "" });
    expect(absent.stdout.split("\n")).toContain(
      `request log: none at ${none}, so the spend is set against none`
    );
  }, 120_000);
});

// Catches: the --after-case command run with no limit, so one that never exits held the
// behaviour run for ever after a case.
test("an --after-case command that never exits is ended at its limit, and counts as failed, naming the file, the command and the limit", async () => {
  const root = path.join(scratch, "after-case-hangs");
  mkdirSync(root, { recursive: true });
  const command = path.join(root, "upload.sh");
  writeFileSync(command, "#!/bin/sh\nsleep 5\n", { mode: 0o755 });
  const file = path.join(root, "c1.json");
  const started = performance.now();
  expect(await runAfterCase(command, file, 200)).toBe(
    `the --after-case command for ${file}: \`${command}\` ran past its limit of 200 ms, so it was ended`
  );
  expect(performance.now() - started).toBeLessThan(3_000);
  writeFileSync(command, "#!/bin/sh\nexit 0\n", { mode: 0o755 });
  expect(await runAfterCase(command, file, 2_000)).toBeUndefined();
});
