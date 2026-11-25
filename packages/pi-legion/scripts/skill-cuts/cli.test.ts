import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { createHash } from "node:crypto";
import {
  appendFileSync,
  copyFileSync,
  cpSync,
  existsSync,
  lstatSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  rmSync,
  symlinkSync,
  utimesSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { postedSpan, startCollector } from "../skill-scenarios/collector-standin";
import { createSpendRecorder, FLUSH_MS, POST_TIMEOUT_MS } from "../skill-scenarios/spend-record";
import {
  SessionEntry,
  type SessionMessage,
  skillHeaderPath,
  stripSkillHeader,
} from "../skill-scenarios/transcript";
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
  main,
  noVerdict,
  ompBinary,
  readCheckoutMarker,
  runAfterCase,
  runnerDigest,
  SENDS,
  textDigest,
} from "./cli";
import { loadConfig } from "./config";
import { cacheKey, skillFrontmatter } from "./context";
import { countSql } from "./countsql";
import { GATEWAY_TRIES } from "./gateway";
import { beforeText, judgeBody, NOTHING_REPLY } from "./judge";
import { ATTACK_PLANTED_LINE, loadPrompt, READER_TRIES } from "./readers";
import {
  messageSse,
  type RecordedRequest,
  type Recorder,
  responseSse,
  sseResponse,
  startRecorder,
} from "./recorder";
import { READS_PAST_LAST_ROUND, sendOrder, UNANSWERED_READ } from "./replay";
import {
  type Compared,
  FRESH_RUNS,
  LISTING_MEASURE_ITEM,
  listingStanding,
  lossChance,
  MAX_EXTRA_RUNS,
  MIN_INFORMATIVE_CASES,
  meanDifference,
  type RunRecord,
  SCREEN_RUNS,
  STAGE_RUNS,
  type Stage,
  scoreSkill,
  smallestDrop,
} from "./score";
import { pinBinary, pinNatives } from "./test-natives";
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
/** The `test` of the rows whose reads need the pinned Oh My Pi's native addon (a code file the pin
 *  may summarize): skipped, or under GITHUB_ACTIONS failing, where the runner finds none
 *  (test-natives.ts). */
const natives = pinNatives("cli.test.ts");
/** The `test` of the rows whose written cases answer `bash` calls, each of which the pinned Oh My
 *  Pi's run of the session's bash gate sees first (checkout.ts's `gateAnswer`): skipped, or under
 *  GITHUB_ACTIONS failing, where the runner finds none (test-natives.ts). */
const pinned = pinBinary("cli.test.ts");

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

/** The made-up world's dotfiles repository (the configuration's `dotfilesRepo`): acme/skills, at
 *  the commit makeCase's profile names its skills at, so a written case's `bash` calls are answered
 *  in the shell that commit's shims/omp gives (cli.ts's `caseShell`). */
const DOTFILES = { repo: "acme/skills", commit: "0123456789abcdef" } as const;

/** The dotfiles' shims/omp at DOTFILES' commit, as `--repos` gives it to the runner's reads: the
 *  export of PI_SHELL_PREFIX as the dotfiles write it from commit bcd580fa on. */
const SHIM = [
  "#!/usr/bin/env bash",
  "set -euo pipefail",
  "# omp puts PI_SHELL_PREFIX in front of each command inside its executor.",
  // biome-ignore lint/suspicious/noTemplateCurlyInString: the shim's own `${PI_SHELL_PREFIX:-…}`
  'export PI_SHELL_PREFIX="${PI_SHELL_PREFIX:-set +e +u +o pipefail;}"',
  'exec omp "$@"',
  "",
].join("\n");

/** The dotfiles' experiments extension of the made-up world (testdata/CAPTURE.md, "F16": the
 *  extension at 3c266c9f cut to its bash gate), at DOTFILES' commit. */
const EXPERIMENTS = path.join(import.meta.dir, "testdata", "F16-experiments");

/** The bash gate's arm the made-up written cases' context sessions record (cases.ts's
 *  `checkout.bashGate`): on, which tests a read rule against a command's leading segment alone. */
const BASH_GATE = { leading: true } as const;

/** What a written case's stage records of the shell its `bash` calls were answered in, where its
 *  case names DOTFILES' commit and BASH_GATE (cli.ts's StageShell): the history held no refusal
 *  of the gate, so its version is the profile's commit. */
const STAGE_SHELL = {
  prefix: "set +e +u +o pipefail;",
  gate: { commit: DOTFILES.commit, from: "profile" as const, leading: true },
};

/** A configuration whose gateway is the stand-in, and a key file. */
function setup(): { config: string; keyFile: string; key: string } {
  const config = path.join(scratch, "skill-cuts.json");
  const fixture = JSON.parse(
    readFileSync(path.join(import.meta.dir, "testdata", "F7-skill-cuts.json"), "utf8")
  );
  writeFileSync(
    config,
    JSON.stringify({ ...fixture, dotfilesRepo: DOTFILES.repo, gatewayBaseUrl: recorder.url })
  );
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
  /** The skill of ours (`--ours`): its directory and its name, for the scenarios that have one. */
  readonly ours?: { readonly dir: string; readonly name: string };
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

/** The arms' directories of `s` (with `plantedLine` for minus) and the digests of their texts, as
 *  buildArm and armCaptureDir take them. */
function armInputs(s: Scenario, plantedLine?: string) {
  return {
    dirs: {
      old: s.old,
      new: s.new,
      ...(s.ours === undefined ? {} : { ours: s.ours }),
      ...(plantedLine === undefined ? {} : { plantedLine }),
    },
    texts: {
      old: textDigest(s.old),
      new: textDigest(s.new),
      ...(s.ours === undefined ? {} : { ours: textDigest(s.ours.dir) }),
    },
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
  const { dirs, texts } = armInputs(s, plantedLine);
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

/** A chance as the report prints it: a percentage with two decimals. */
const percent = (x: number) => `${(100 * x).toFixed(2)}%`;

/** Writes `count` as a skill's reading sessions by model (attribute's
 *  `<cut or set id>/models/<skill>.json`) to `name` in `dir`, and returns that file. */
function readingModels(dir: string, name: string, count: object): string {
  const file = path.join(dir, name);
  writeFileSync(file, JSON.stringify(count));
  return file;
}

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
    const each = falseLossChance({ old: 10, compared: 0, none: 0 });
    expect(each).toBeGreaterThan(0);
    expect(reported.stdout).toContain(
      `false-loss rate at the cases run (an estimate): ${percent(1 - (1 - each) ** 6)}, `
    );
    expect(reported.stdout).toContain(
      `(c02 ${percent(each)}, c04 ${percent(each)}, c05 ${percent(each)}, c06 ${percent(each)}, c07 ${percent(each)}, c08 ${percent(each)})`
    );
    expect(lines).toContain(
      `  old 0.9: ${[0.8, 0.7, 0.6, 0.5].map((cut) => `new ${cut} ${percent(lossChance({ old: 0.9, compared: cut, none: 0.1 }))}`).join(", ")}`
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
      `verdict: none: deleting the planted line ${JSON.stringify(OTHER_LINE)} showed no detectable change (planted-loss control not a loss)${unmeasured(6, 6)}`
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
      `attempt ${n}: its body defines 2 tools, legion not among them, and its stderr's [legion], Extension error and Failed to load extension lines are ${lines}`;
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

  test("with --reading-models, screen refuses a skill too few of whose reading sessions are on served models, before any call, and report gives it no verdict", async () => {
    const s = scenario("reading-few", LOSS_SKILL(), [{ id: "c00" }]);
    const file = readingModels(s.dir, "reading-models.json", {
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
    const bad = readingModels(s.dir, "not-reading-models.json", { sessions: 8 });
    const refused = await cli(behaviour("screen", s, "--reading-models", bad));
    expect(refused.code).toBe(2);
    expect(refused.stderr).toContain(
      `--reading-models ${bad} is not a skill's reading sessions by model`
    );
    expect(requestsSince(from)).toHaveLength(0);
  }, 60_000);

  // Catches: a session case whose screen began before the served-sessions refusal applied, which
  // the screen then passes over as it passes over every session case, taken by confirm, control
  // and report as not screened yet for ever.
  test("with --reading-models refusing session cases, a session case whose screen began is passed over, as the screen passes it over", async () => {
    const s = withWritten(scenario("reading-begun", f1File(2), [{ id: "c00" }]), ["w00", "w01"]);
    for (const id of ["w00", "w01"]) {
      writeMadeUpResult(s.out, id, { screen: { old: 10, none: 0 } }, { compared: "none" });
    }
    // c00's screen began with no refusal: 3 runs of each arm, then the run stopped.
    writeMadeUpResult(s.out, "c00", { screen: { old: 3, none: 0 } }, { compared: "none" });
    const begun = JSON.parse(readFileSync(caseResultFile(s.out, "c00"), "utf8"));
    begun.stages.screen.state = "running";
    begun.stages.screen.runs = begun.stages.screen.runs.filter(
      (run: { run: number }) => run.run <= 3
    );
    writeFileSync(caseResultFile(s.out, "c00"), JSON.stringify(CaseResult.parse(begun)));
    const file = readingModels(s.dir, "reading-models.json", {
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
    const tooFew = `fewer than ${MIN_INFORMATIVE_CASES} informative cases: 2`;
    const reported = await cli([...report(s), "--reading-models", file]);
    expect({ code: reported.code, stderr: reported.stderr }).toEqual({ code: 0, stderr: "" });
    expect(reported.stdout.split("\n")).toContain(`verdict: none: ${tooFew}${unmeasured(2, 2)}`);
    const from = recorder.requests.length;
    const confirm = await cli(comparedArgs("confirm", s, "none", "--reading-models", file));
    expect({ code: confirm.code, stderr: confirm.stderr }).toEqual({ code: 0, stderr: "" });
    expect(confirm.stdout).toContain(`no confirm runs: the skill gets no verdict: ${tooFew}`);
    expect(requestsSince(from)).toHaveLength(0);
    // With no refusal, the screen goes on with c00, so confirm waits for it.
    const waits = await cli(comparedArgs("confirm", s, "none"));
    expect(waits.code).toBe(2);
    expect(waits.stderr).toContain("the screen is not finished: case c00 is not screened yet");
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
    const file = readingModels(s.dir, "reading-models.json", {
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
      "screened session cases by the served model they ran on: anthropic/claude-sonnet-5-5 5 cases, openai/gpt-6.1-sol 1 case",
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
    const opus = readingModels(s.dir, "reading-models-opus.json", {
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
      `${keeps}; it speaks for the reading sessions on anthropic/claude-sonnet-5-5 (7 sessions), openai/gpt-6.1-sol (2 sessions); not for the 2 sessions on models the gateway does not serve; nor for the 3 sessions on served models no screened session case ran on${unmeasured(6, 6)}`
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
      { old: 7, compared: 6, none: 2 },
      { old: 9, compared: 4, none: 0 },
    ]) {
      const p = (passes.old + passes.compared + 1) / 22;
      const counted = bruteForce(p, p, (passes.none + 1) / 12);
      expect({ passes, agrees: agrees(falseLossChance(passes), counted) }).toEqual({
        passes,
        agrees: true,
      });
    }
    // An old text that passed 10 of 10 still has a chance above 0: the pooled rate is 21/22, not
    // the screen's own rate of 1, at which no loss could happen.
    const full = falseLossChance({ old: 10, compared: 10, none: 0 });
    expect(full).toBeGreaterThan(0);
    expect(agrees(full, bruteForce(21 / 22, 21 / 22, 1 / 12))).toBe(true);
    // A sensitivity point: the old text at 0.9, the cut's at 0.7, no skill at 0.1 (about 20%).
    const point = lossChance({ old: 0.9, compared: 0.7, none: 0.1 });
    expect(agrees(point, bruteForce(0.9, 0.7, 0.1))).toBe(true);
    expect(point).toBeGreaterThan(0.2);
    expect(point).toBeLessThan(0.21);
    expect(() => lossChance({ old: 0, compared: 0, none: 0.1 })).toThrow("no case is informative");
  });
});

// The arm the old text is compared with: `--new` (the cut's text), `--ours` (a skill of ours,
// whose name may differ from the case's skill) or neither (no skill).

/** Alpha's SKILL.md as F1 read it, under the name alpha-ours. */
const OURS_SKILL = () => f1File(2).replace("name: alpha\n", "name: alpha-ours\n");

/** `s` with a skill of ours: the directory `dirName` under its own, holding `skillMd` and F1's
 *  references/x.md, whose frontmatter names it alpha-ours. */
function withOurs(s: Scenario, dirName: string, skillMd: string): Scenario {
  const dir = path.join(s.dir, dirName);
  writeSkill(dir, { "SKILL.md": skillMd, "references/x.md": f1File(4) });
  return { ...s, ours: { dir, name: "alpha-ours" } };
}

/** The arguments of `behaviour <command>` for `s` with the compared arm `compared`: `--ours`, or
 *  neither `--new` nor `--ours`. */
function comparedArgs(
  command: string,
  s: Scenario,
  compared: Exclude<Compared, "new">,
  ...extra: string[]
) {
  const args = behaviour(command, s, ...extra);
  const at = args.indexOf("--new");
  return compared === "none"
    ? [...args.slice(0, at), ...args.slice(at + 2)]
    : [...args.slice(0, at), "--ours", s.ours?.dir ?? "", ...args.slice(at + 2)];
}

/** The report's verdict with no skill compared on six cases, each flagged and none a confirmed
 *  loss, the first of them the control case. */
const NO_SKILL_KEEPS =
  "verdict: no loss of no skill detected by the registered rule: no confirmed case is a loss (6 flagged of 6 informative), and the planted-loss control (case c04) is a loss and its false-loss control is no loss";

/** Writes case `id`'s results under `out` in the runner's shape, made up: each stage of `stages`
 *  with each arm's passes of its runs (10 in the screen, 40 in confirm and control), its other
 *  runs fails. Each stage records `compared` (absent: a stage the cut's runner made before the
 *  compared arm was recorded), and the screen and confirm stages record the ours arm's `ours`.
 *  `step`, where given, gives each run's judged step and its answered rounds' reads (by its arm
 *  and its number from 0); else every run's step is empty. */
function writeMadeUpResult(
  out: string,
  id: string,
  stages: Partial<Record<Stage, Record<string, number>>>,
  recorded: {
    compared?: Compared;
    ours?: { name: string; replacedOwn: boolean; removedReads: string[] };
    step?: (arm: string, n: number) => { reply: unknown[]; skillReads?: unknown[][] };
  } = {}
): void {
  const judged = (verdict: "pass" | "fail") => ({ verdict, reason: "made up" });
  const texts =
    recorded.compared === "none"
      ? { old: "0123456789abcdef" }
      : recorded.compared === "ours"
        ? { old: "0123456789abcdef", ours: "fedcba9876543210" }
        : { old: "0123456789abcdef", new: "00112233445566ff" };
  const result = {
    caseId: id,
    skill: "alpha",
    cut: "acme-skills-1",
    dropped: null,
    controls: {
      realStep: judged("pass"),
      violating: judged("fail"),
      nothing: judged("fail"),
      failed: [],
      hold: true,
      usd: 0,
    },
    stages: Object.fromEntries(
      Object.entries(stages).map(([stage, byArm]) => {
        const total = stage === "screen" ? SCREEN_RUNS : FRESH_RUNS;
        const runs = Object.entries(byArm).flatMap(([arm, passed]) =>
          Array.from({ length: total }, (_, n) => ({
            caseId: id,
            arm,
            stage,
            ...judged(n < passed ? "pass" : "fail"),
            run: n + 1,
            round: 1,
            sendPlace: n,
            userId: null,
            id: null,
            skillRounds: 0,
            ...(recorded.step === undefined
              ? { reply: [] }
              : {
                  reply: recorded.step(arm, n).reply,
                  skillReads: recorded.step(arm, n).skillReads ?? [],
                }),
            replaySpent: [],
            judgeSpent: [],
          }))
        );
        const state = {
          state: "counted",
          sends: SENDS,
          texts,
          plantedLine: null,
          captures: {},
          seeds: ["made-up-seed"],
          runs,
          usd: 0,
          ...(recorded.compared === undefined ? {} : { compared: recorded.compared }),
          ...(recorded.ours === undefined || stage === "control" ? {} : { ours: recorded.ours }),
        };
        return [stage, state];
      })
    ),
    peakRssBytes: null,
  };
  mkdirSync(path.dirname(caseResultFile(out, id)), { recursive: true });
  writeFileSync(caseResultFile(out, id), JSON.stringify(CaseResult.parse(result)));
}

describe("behaviour: the compared arm", () => {
  /** A screen run into its own --out: that --out, the run, and the requests it sent. */
  interface Screened {
    readonly out: string;
    readonly ran: Ran;
    readonly sent: readonly RecordedRequest[];
  }
  /** One case of alpha with a skill of ours, and its screens with --new and with no skill
   *  compared, each run once for the two tests that read them. */
  let oneCase: Scenario;
  let screens: Record<"new" | "none", Screened>;
  beforeAll(async () => {
    oneCase = withOurs(scenario("compared", LOSS_SKILL(), [{ id: "c04" }]), "ours", OURS_SKILL());
    answer = behaviourStandIn;
    usage = { input: 10, output: 3 };
    const screen = async (name: "new" | "none"): Promise<Screened> => {
      const s = { ...oneCase, out: path.join(oneCase.dir, `out-${name}`) };
      recordCaptures(s, name === "new" ? ["old", "new", "none"] : ["old", "none"]);
      const from = recorder.requests.length;
      const ran = await cli(
        name === "new" ? behaviour("screen", s) : comparedArgs("screen", s, "none")
      );
      return { out: s.out, ran, sent: requestsSince(from) };
    };
    screens = { new: await screen("new"), none: await screen("none") };
  }, 120_000);

  test("the ours arm's captures go under their own directory", () => {
    const s = withOurs(
      scenario("ours-captures", LOSS_SKILL(), [{ id: "c04" }]),
      "ours",
      OURS_SKILL()
    );
    const c = s.cases[0] ?? makeCase("c04");
    const { dirs, texts } = armInputs(s);
    const at = (name: ArmName) =>
      armCaptureDir(s.out, buildArm(c, name, dirs, path.join(s.dir, "arms")), texts);
    expect(at("ours")).toBe(path.join(s.out, "captures", `ours-${texts.ours}`));
    // A renamed copy of old, whose files differ from old's in the name alone, has its own too.
    expect(texts.ours).not.toBe(texts.old);
    const names: ArmName[] = ["old", "new", "ours", "none", "minus", "twin"];
    expect(new Set(names.map(at)).size).toBe(names.length);
    expect(at("twin")).toBe(path.join(s.out, "captures", `twin-${texts.old}`));
  });

  test("a stage records its compared arm", async () => {
    const s = oneCase;
    recordCaptures(s, ["old", "new", "ours", "none"]);
    answer = behaviourStandIn;
    usage = { input: 10, output: 3 };
    let from = recorder.requests.length;
    const both = await cli(behaviour("screen", s, "--ours", s.ours?.dir ?? ""));
    expect(both.code).toBe(2);
    expect(both.stderr).toContain("--new and --ours exclude each other");
    expect(requestsSince(from)).toHaveLength(0);

    // --ours: the screen runs old, ours and none, records ours, and the ours arm's note.
    from = recorder.requests.length;
    const ours = await cli(comparedArgs("screen", s, "ours"));
    expect({ code: ours.code, stderr: ours.stderr }).toEqual({ code: 0, stderr: "" });
    expect(requestsSince(from)).toHaveLength(3 + 30 + 30);
    const screened = result(s.out, "c04").stages.screen;
    expect(screened?.compared).toBe("ours");
    expect(screened?.ours).toEqual({ name: "alpha-ours", replacedOwn: false, removedReads: [] });
    expect(passes(result(s.out, "c04"), "screen")).toEqual({ old: 10, ours: 10, none: 0 });
    const oursDir = path.join(s.out, "captures", `ours-${textDigest(s.ours?.dir ?? "")}`);
    expect(screened?.captures.ours?.bodyFile).toBe(
      path.relative(s.out, capturePaths(oursDir, "c04", "ours").body)
    );
    // Every judgement, the controls' among them, reads the skill's names as the-skill.
    const judged = requestsSince(from).filter((request) => isJudgement(JSON.parse(request.body)));
    expect(judged).toHaveLength(33);
    for (const request of judged) {
      const user = JSON.parse(request.body).messages[0].content as { text: string }[];
      const text = user.map((block) => block.text).join("\n");
      expect(text).not.toMatch(/(?<![\w-])alpha(-ours)?(?![\w-])/);
      expect(text).toContain("skill://the-skill");
    }

    // Neither --new nor --ours: the screen runs old and none, and records none.
    const { none, new: cut } = screens;
    expect({ code: none.ran.code, stderr: none.ran.stderr }).toEqual({ code: 0, stderr: "" });
    expect(none.sent).toHaveLength(3 + 20 + 20);
    expect(result(none.out, "c04").stages.screen?.compared).toBe("none");
    expect(result(none.out, "c04").stages.screen?.ours).toBeUndefined();
    expect(passes(result(none.out, "c04"), "screen")).toEqual({ old: 10, none: 0 });

    // --new: the screen records new.
    expect(cut.ran.code).toBe(0);
    expect(result(cut.out, "c04").stages.screen?.compared).toBe("new");
  }, 120_000);

  // Catches: a run of the cut's test (the old and new arms) or of their skill against no skill
  // whose judge reads `the-skill` where the case's text names the skill.
  test("a run with --new or with no skill compared sends the judge the case's text unchanged", () => {
    const c = oneCase.cases[0] ?? makeCase("c04");
    /** Each judge request of `requests` less its own session id, beside the body the judge builds
     *  for its step from the case's text with no name blinded. */
    const judged = (requests: readonly RecordedRequest[]) =>
      requests.flatMap((request) => {
        const { metadata: _metadata, ...sent } = JSON.parse(request.body);
        if (!isJudgement(sent)) return [];
        const step = String(sent.messages[0].content[1].text)
          .replace(/^<step>\n/, "")
          .replace(/\n<\/step>$/, "");
        const before = beforeText(c);
        const body = judgeBody({ criterion: c.criterion, line: c.line.text, before, reply: step });
        return [{ sent, unchanged: JSON.parse(JSON.stringify(body)) }];
      });
    for (const [name, judgements] of [
      ["new", 33],
      ["none", 23],
    ] as const) {
      const { ran, sent: requests } = screens[name];
      expect({ name, code: ran.code, stderr: ran.stderr }).toEqual({ name, code: 0, stderr: "" });
      const bodies = judged(requests);
      expect(bodies).toHaveLength(judgements);
      for (const { sent, unchanged } of bodies) expect(sent).toEqual(unchanged);
      // The case's text names the skill, and the judge reads it so.
      expect(JSON.stringify(bodies[0]?.sent)).toContain("skill://alpha");
      expect(JSON.stringify(bodies.map((each) => each.sent))).not.toContain("the-skill");
    }
  });

  test("a results file of a cut's test, with old and new, still reports", async () => {
    const ids = ["c04", "c05", "c06", "c07", "c08", "c09"];
    const s = scenario(
      "cut-results",
      LOSS_SKILL(),
      ids.map((id) => ({ id }))
    );
    for (const id of ids) {
      // Made before the compared arm was recorded: no stage records one.
      writeMadeUpResult(s.out, id, {
        screen: { old: 10, new: 0, none: 0 },
        confirm: { old: 40, new: 0 },
        ...(id === "c04" ? { control: { old: 40, minus: 0, twin: 40 } } : {}),
      });
    }
    const reported = await cli(report(s));
    expect({ code: reported.code, stderr: reported.stderr }).toEqual({ code: 0, stderr: "" });
    const lines = reported.stdout.split("\n");
    expect(lines).toContain("compared arm: new (the cut's text)");
    expect(lines).toContain(
      "case c05: screen: old 10/10, new 0/10, none 0/10: informative, flagged; confirm: old 40/40, new 0/40: a loss"
    );
    expect(lines).toContain(
      `verdict: loss: in c04, c05, c06, c07, c08, c09 the cut's text passes at least 10 fewer of 40 fresh runs than the old text, and the planted-loss control (case c04) is a loss and its false-loss control is no loss${unmeasured(6, 6)}`
    );
    expect(lines).toContain(
      `  old 0.9: ${[0.8, 0.7, 0.6, 0.5].map((cut) => `new ${cut} ${percent(lossChance({ old: 0.9, compared: cut, none: 0.1 }))}`).join(", ")}`
    );
  }, 60_000);

  test("a resume with another compared arm is refused", async () => {
    const s = withOurs(
      scenario("resume-compared", LOSS_SKILL(), [{ id: "c04" }]),
      "ours",
      OURS_SKILL()
    );
    // A stage the cut's runner made: it records no compared arm, so it was made with --new.
    writeMadeUpResult(s.out, "c04", { screen: { old: 10, new: 0, none: 0 } });
    const ours = { ...s, out: path.join(s.dir, "out-ours") };
    writeMadeUpResult(
      ours.out,
      "c04",
      { screen: { old: 10, ours: 0, none: 0 } },
      { compared: "ours", ours: { name: "alpha-ours", replacedOwn: false, removedReads: [] } }
    );
    const from = recorder.requests.length;
    for (const [args, made, given] of [
      [comparedArgs("screen", s, "ours"), "new", "ours"],
      [comparedArgs("confirm", s, "none"), "new", "none"],
      [behaviour("control", ours), "ours", "new"],
      [comparedArgs("screen", ours, "none"), "ours", "none"],
      [[...report(ours), "--new", s.new], "ours", "new"],
    ] as const) {
      const ran = await cli(args);
      expect({ code: ran.code, made, given }).toEqual({ code: 2, made, given });
      expect(ran.stderr).toContain(`holds runs made with the compared arm ${made}, not ${given}`);
    }
    expect(requestsSince(from)).toHaveLength(0);
    // A report given neither reads the compared arm the results record.
    const reported = await cli(report(ours));
    expect(reported.code).toBe(0);
    expect(reported.stdout).toContain("compared arm: ours (alpha-ours)");
  }, 60_000);

  test("confirm --case runs 40 fresh runs of old and the compared arm on the named case with no flag gate and no informative gate", async () => {
    const s = scenario("confirm-case", LOSS_SKILL(), [{ id: "c04" }, { id: "c05" }]);
    // A planted equivalent: a renamed copy of old without the case's line.
    const planted = withOurs(s, "planted", OURS_SKILL().replace(`${LINE}\n`, ""));
    recordCaptures(planted, ["old", "ours"]);
    answer = behaviourStandIn;
    usage = { input: 10, output: 3 };
    let from = recorder.requests.length;
    const lost = await cli(comparedArgs("confirm", planted, "ours", "--case", "c04"));
    expect({ code: lost.code, stderr: lost.stderr }).toEqual({ code: 0, stderr: "" });
    // No judge controls and no screen: 40 replays of each arm and their 80 judgements.
    expect(requestsSince(from)).toHaveLength(160);
    const c04 = result(planted.out, "c04");
    expect(c04.stages.screen).toBeUndefined();
    expect(c04.stages.confirm?.compared).toBe("ours");
    expect(passes(c04, "confirm")).toEqual({ old: 40, ours: 0 });
    expect(lost.stdout).toContain("case c04: confirm: old 40/40, ours 0/40: a loss");
    expect(existsSync(caseResultFile(planted.out, "c05"))).toBe(false);

    // A renamed copy of old unchanged, into its own --out: no loss.
    const same = { ...withOurs(s, "same", OURS_SKILL()), out: path.join(s.dir, "out-same") };
    recordCaptures(same, ["old", "ours"]);
    from = recorder.requests.length;
    const kept = await cli(comparedArgs("confirm", same, "ours", "--case", "c04"));
    expect({ code: kept.code, stderr: kept.stderr }).toEqual({ code: 0, stderr: "" });
    expect(requestsSince(from)).toHaveLength(160);
    expect(passes(result(same.out, "c04"), "confirm")).toEqual({ old: 40, ours: 40 });
    expect(kept.stdout).toContain("case c04: confirm: old 40/40, ours 40/40: no loss");
  }, 120_000);

  test("confirm --case refuses an id the case file does not hold, and --again beside it", async () => {
    const s = withOurs(
      scenario("confirm-case-missing", LOSS_SKILL(), [{ id: "c04" }]),
      "ours",
      OURS_SKILL()
    );
    const from = recorder.requests.length;
    const missing = await cli(comparedArgs("confirm", s, "ours", "--case", "c99"));
    expect(missing.code).toBe(2);
    expect(missing.stderr).toContain(`--case c99: ${s.casesFile} holds no case c99`);
    const screen = await cli(comparedArgs("screen", s, "ours", "--case", "c04"));
    expect(screen.code).toBe(2);
    expect(screen.stderr).toContain("--case is for behaviour confirm only");
    const again = await cli(comparedArgs("confirm", s, "ours", "--case", "c04", "--again", "c04"));
    expect(again.code).toBe(2);
    expect(again.stderr).toContain("--again and --case exclude each other");
    expect(requestsSince(from)).toHaveLength(0);
    expect(existsSync(s.out)).toBe(false);
  }, 60_000);

  test("report prints session and written cases, informative of each kind, passes and runs per arm and stage, the smallest drop with its chance, and the models", async () => {
    const s = scenario(
      "report-none",
      f1File(2),
      ["c04", "c05", "c06", "c07", "c08", "c09"].map((id) => ({
        id,
        ...(id === "c09" ? { model: "openai/gpt-6.1-sol" } : {}),
      }))
    );
    const write = (out: string, confirmNone: number) => {
      for (const { id } of s.cases) {
        writeMadeUpResult(
          out,
          id,
          {
            screen: id === "c09" ? { old: 9, none: 1 } : { old: 10, none: 0 },
            confirm: { old: 40, none: confirmNone },
            ...(id === "c04" ? { control: { old: 40, minus: 0, twin: 40 } } : {}),
          },
          { compared: "none" }
        );
      }
    };
    write(s.out, 0);
    const limits = path.join(s.dir, "limits.md");
    writeFileSync(limits, "Limits:\n- The profile is the session's reads.\n- No bootstrap.\n");
    const reported = await cli([...report(s), "--limits", limits]);
    expect({ code: reported.code, stderr: reported.stderr }).toEqual({ code: 0, stderr: "" });
    const lines = reported.stdout.split("\n");
    for (const line of [
      "compared arm: none (no skill): the arm with no skill informs and clears nothing",
      "cases by kind: 6 session (6 informative), 0 written (0 informative)",
      "case c04: screen: old 10/10, none 0/10: informative, flagged; confirm: old 40/40, none 0/40: a loss; control: old 40/40, minus 0/40, twin 40/40: the planted-loss control (old against minus) is a loss, the false-loss control (old against twin) is no loss",
      "case c09: screen: old 9/10, none 1/10: informative, flagged; confirm: old 40/40, none 0/40: a loss",
      "smallest drop the rule catches with a chance of at least 80%, by screened case (5-point steps below the case's screened pass rate of old; the rule's chance with the compared arm none):",
      `  c04 old 1.0: 35 points (${percent(smallestDrop({ old: 10, none: 0 }, "none").chance)})`,
      "  c09 old 0.9: 40 points (87.52%)",
      "median smallest drop: 35 points over the 6 screened cases; 35 points over the 6 informative cases",
      "models of the cases: anthropic/claude-sonnet-5-5 5 cases, openai/gpt-6.1-sol 1 case",
      `verdict: loss: no skill loses against their skill on 6 cases (c04, c05, c06, c07, c08, c09): in each, no skill passes at least 10 fewer of 40 fresh runs than their skill, and the planted-loss control (case c04) is a loss and its false-loss control is no loss${unmeasured(6, 6)}`,
    ]) {
      expect(lines).toContain(line);
    }
    expect(reported.stdout).toContain(
      `limits (--limits ${limits}):\nLimits:\n- The profile is the session's reads.\n- No bootstrap.\n`
    );

    // No confirmed loss: the verdict's words for no skill.
    const kept = { ...s, out: path.join(s.dir, "out-keeps") };
    write(kept.out, 35);
    const keeps = (await cli(report(kept))).stdout.split("\n");
    expect(keeps).toContain(`${NO_SKILL_KEEPS}${unmeasured(6, 6)}`);
    expect(keeps).toContain("limits: not given (no --limits)");
  }, 60_000);

  test("report calls the ours arm a bundle when it replaced a profile skill of our name", async () => {
    const s = scenario("bundle", f1File(2), [{ id: "c04" }]);
    const note = (replacedOwn: boolean) => ({ name: "alpha-ours", replacedOwn, removedReads: [] });
    writeMadeUpResult(
      s.out,
      "c04",
      { screen: { old: 10, ours: 10, none: 0 } },
      {
        compared: "ours",
        ours: note(true),
      }
    );
    const bundled = (await cli(report(s))).stdout.split("\n");
    expect(bundled).toContain(
      "compared arm: ours (alpha-ours), a bundle: their skill hidden, ours at the given text (it replaced the profile's own alpha-ours)"
    );
    const plain = { ...s, out: path.join(s.dir, "out-plain") };
    writeMadeUpResult(
      plain.out,
      "c04",
      { screen: { old: 10, ours: 10, none: 0 } },
      {
        compared: "ours",
        ours: note(false),
      }
    );
    const kept = await cli(report(plain));
    expect(kept.stdout.split("\n")).toContain(
      "compared arm: ours (alpha-ours): their skill hidden, ours in its place under its own name"
    );
    expect(kept.stdout).not.toContain("a bundle");
  }, 60_000);

  test("report gives ours against old over all cases and over the cases with no removed read", async () => {
    const s = scenario("ours-removed", f1File(2), [{ id: "c04" }, { id: "c05" }, { id: "c06" }]);
    const recorded = (removedReads: string[]) => ({
      compared: "ours" as const,
      ours: { name: "alpha-ours", replacedOwn: false, removedReads },
    });
    writeMadeUpResult(
      s.out,
      "c04",
      { screen: { old: 10, ours: 0, none: 0 }, confirm: { old: 40, ours: 0 } },
      recorded(["references/x.md"])
    );
    writeMadeUpResult(s.out, "c05", { screen: { old: 10, ours: 9, none: 0 } }, recorded([]));
    writeMadeUpResult(
      s.out,
      "c06",
      { screen: { old: 9, ours: 5, none: 1 }, confirm: { old: 36, ours: 30 } },
      recorded([])
    );
    const reported = await cli(report(s));
    expect({ code: reported.code, stderr: reported.stderr }).toEqual({ code: 0, stderr: "" });
    const lines = reported.stdout.split("\n");
    for (const line of [
      "case c04: screen: old 10/10, ours 0/10, none 0/10: informative, flagged; confirm: old 40/40, ours 0/40: a loss; ours left out the history's reads of references/x.md (our skill has no file at that path)",
      "case c05: screen: old 10/10, ours 9/10, none 0/10: informative, not flagged",
      "ours against old (their skill), over all 3 screened cases: screen old 29/30, ours 14/30, none 1/30; confirm on 2 cases: old 76/80, ours 30/80, 1 a loss, 1 no loss",
      "ours against old (their skill), over the 2 screened cases with no removed read (c05, c06): screen old 19/20, ours 14/20, none 1/20; confirm on 1 case: old 36/40, ours 30/40, 0 a loss, 1 no loss",
    ]) {
      expect(lines).toContain(line);
    }
  }, 60_000);
});

// Written cases (`build-cases --written`) beside session cases, and the report's descriptions
// beside the registered verdict.

/** A written case of alpha, as `build-cases --written` writes one: its task as the user's message,
 *  a read of skill://alpha and its result as F1 holds it, and a real step that passes. */
function makeWrittenCase(id: string): Case {
  const c = makeCase(id);
  return {
    ...c,
    messages: [
      {
        role: "user",
        content: [{ type: "text", text: "Write the alpha checklist for the release." }],
      },
      {
        role: "assistant",
        content: [
          {
            type: "toolCall",
            id: "written_read",
            name: "read",
            arguments: { path: "skill://alpha" },
          },
        ],
      },
      { ...messages[2], toolCallId: "written_read" },
    ],
    skillReads: [2],
    realStep: { role: "assistant", content: [{ type: "text", text: PASS_STEP }] },
    source: { ...c.source, kind: "written" },
  };
}

/** `s` with the written cases `ids` after its own cases, in its case file. */
function withWritten(s: Scenario, ids: readonly string[]): Scenario {
  const cases = [...s.cases, ...ids.map(makeWrittenCase)];
  writeFileSync(s.casesFile, `${cases.map((c) => JSON.stringify(c)).join("\n")}\n`);
  return { ...s, cases, markers: new Map([...s.markers, ...ids.map((id) => [id, ""] as const)]) };
}

/** The report's words of the mean of `differences` (score.ts's `meanDifference`). */
function meanWords(differences: number[]): string {
  const mean = meanDifference(differences);
  if (mean === undefined) return "none";
  const head = `${mean.cases} case${mean.cases === 1 ? "" : "s"} ${mean.mean.toFixed(2)}`;
  return mean.interval === undefined
    ? `${head} (one case, so no interval)`
    : `${head} (95% interval ${mean.interval.low.toFixed(2)} to ${mean.interval.high.toFixed(2)}, t over the cases)`;
}

describe("behaviour: written cases and the report's descriptions", () => {
  test("the served-sessions refusal stops session cases only, and a skill with written cases never reports it as its reason for no verdict", async () => {
    const s = withWritten(scenario("reading-written", f1File(2), [{ id: "c00" }, { id: "c01" }]), [
      "w00",
      "w01",
    ]);
    recordCaptures(s, ["old", "none"]);
    answer = behaviourStandIn;
    usage = { input: 10, output: 3 };
    const file = readingModels(s.dir, "reading-models.json", {
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
    const screen = await cli(comparedArgs("screen", s, "none", "--reading-models", file));
    expect({ code: screen.code, stderr: screen.stderr }).toEqual({ code: 0, stderr: "" });
    expect(screen.stdout).toContain(
      `session cases are not screened: ${reason} (--reading-models ${file}); the written cases are`
    );
    // The session cases make no run; each written case its controls, and 10 runs of old and none,
    // each judged.
    for (const id of ["c00", "c01"]) expect(existsSync(caseResultFile(s.out, id))).toBe(false);
    for (const id of ["w00", "w01"]) {
      expect(passes(result(s.out, id), "screen")).toEqual({ old: SCREEN_RUNS, none: 0 });
    }
    expect(requestsSince(from)).toHaveLength(2 * (3 + 2 * SCREEN_RUNS + 2 * SCREEN_RUNS));
    const reported = await cli([...report(s), "--reading-models", file]);
    expect({ code: reported.code, stderr: reported.stderr }).toEqual({ code: 0, stderr: "" });
    const lines = reported.stdout.split("\n");
    expect(lines).toContain(
      `verdict: none: fewer than ${MIN_INFORMATIVE_CASES} informative cases: 2${unmeasured(2, 2)}`
    );
    expect(reported.stdout).not.toContain(reason);
    expect(lines).toContain("cases by kind: 2 session (0 informative), 2 written (2 informative)");
    expect(lines).toContain("case c00: not run");
    // Confirm and control take the screen as finished: the session cases were never to be run.
    const confirm = await cli(comparedArgs("confirm", s, "none", "--reading-models", file));
    expect(confirm.stdout).toContain(
      `no confirm runs: the skill gets no verdict: fewer than ${MIN_INFORMATIVE_CASES} informative cases: 2`
    );
  }, 300_000);

  test("report gives the mean difference over screened cases with its interval, per kind", async () => {
    const s = withWritten(
      scenario("report-mean", f1File(2), [{ id: "c04" }, { id: "c05" }, { id: "c06" }]),
      ["w00", "w01", "w02"]
    );
    const screens: Record<string, { old: number; none: number }> = {
      c04: { old: 10, none: 0 },
      c05: { old: 8, none: 4 },
      c06: { old: 5, none: 5 },
      w00: { old: 8, none: 2 },
      w01: { old: 3, none: 5 },
    };
    for (const [id, screen] of Object.entries(screens)) {
      writeMadeUpResult(s.out, id, { screen }, { compared: "none" });
    }
    const reported = await cli(report(s));
    expect({ code: reported.code, stderr: reported.stderr }).toEqual({ code: 0, stderr: "" });
    const lines = reported.stdout.split("\n");
    const difference = (id: string) => ((screens[id]?.old ?? 0) - (screens[id]?.none ?? 0)) / 10;
    const session = ["c04", "c05", "c06"].map(difference);
    const written = ["w00", "w01"].map(difference);
    for (const line of [
      "screened session cases: 3 (c04, c05, c06), 2 informative; screen passes old 23/30, none 9/30",
      "screened written cases: 2 (w00, w01), 1 informative; screen passes old 11/20, none 7/20",
      `mean of old less none in screen pass rate, each screened case one observation (a description beside the registered rule): all ${meanWords([...session, ...written])}; session ${meanWords(session)}; written ${meanWords(written)}`,
    ]) {
      expect(lines).toContain(line);
    }
    // One case of a kind has no interval, and a kind with no screened case has no mean.
    const one = withWritten(scenario("report-mean-one", f1File(2), [{ id: "c04" }]), ["w00"]);
    writeMadeUpResult(one.out, "c04", { screen: { old: 10, none: 0 } }, { compared: "none" });
    const oneLines = (await cli(report(one))).stdout.split("\n");
    expect(oneLines).toContain(
      "mean of old less none in screen pass rate, each screened case one observation (a description beside the registered rule): all 1 case 1.00 (one case, so no interval); session 1 case 1.00 (one case, so no interval); written none"
    );
    expect(oneLines).toContain("screened written cases: none");
  }, 60_000);

  test("report counts ties, floors, ceilings and reverse gaps", async () => {
    const s = withWritten(
      scenario("report-shapes", f1File(2), [{ id: "c04" }, { id: "c05" }, { id: "c06" }]),
      ["w00", "w01"]
    );
    const screens: Record<string, { old: number; none: number }> = {
      c04: { old: 0, none: 0 },
      c05: { old: 10, none: 10 },
      c06: { old: 5, none: 5 },
      w00: { old: 3, none: 5 },
      w01: { old: 8, none: 2 },
    };
    for (const [id, screen] of Object.entries(screens)) {
      writeMadeUpResult(s.out, id, { screen }, { compared: "none" });
    }
    const reported = await cli(report(s));
    expect(reported.stdout.split("\n")).toContain(
      "screened cases, old against none in the screen: 3 ties, 1 floor (0 of 10 in both), 1 ceiling (10 of 10 in both), 1 reverse gap (none above old); a floor or a ceiling is a tie too"
    );
  }, 60_000);

  // Catches: the measured defect hidden in the mean (a case on which every arm fails on a first
  // look at the work counts as a tie), a floor counted from an answered round's reads rather than
  // the judged reply's own calls, or a floor case dropped.
  test("report counts the inspection floors by kind with their ids, and gives the mean over all screened cases and over those that are not floors", async () => {
    const s = withWritten(scenario("report-floors", f1File(2), [{ id: "c04" }, { id: "c05" }]), [
      "w00",
      "w01",
    ]);
    const look = { type: "tool_use", id: "toolu_look", name: "read", input: { path: "app.py" } };
    const grep = {
      type: "tool_use",
      id: "toolu_grep",
      name: "bash",
      input: { command: "grep -n main app.py | head" },
    };
    const act = {
      type: "tool_use",
      id: "toolu_act",
      name: "write",
      input: { path: "test_app.py", content: "def test_main(): pass\n" },
    };
    const answered = { from: "checkout", tool: "read", input: look.input, isError: false };
    const screens: Record<string, { old: number; none: number }> = {
      c04: { old: 0, none: 0 },
      c05: { old: 8, none: 2 },
      w00: { old: 1, none: 0 },
      w01: { old: 0, none: 0 },
    };
    const steps: Record<
      string,
      (arm: string, n: number) => { reply: unknown[]; skillReads?: unknown[][] }
    > = {
      // Every run of each arm is judged on a look: a floor.
      c04: () => ({ reply: [look, grep] }),
      // Every run acts: no floor.
      c05: () => ({ reply: [act] }),
      // Old passes one run and fails nine on a look; none fails all ten on one: a floor.
      w00: () => ({ reply: [look] }),
      // Seven of old's ten fail on a look; the other three on an act after an answered round of
      // reads, which is no look of the judged reply: no floor.
      w01: (arm, n) =>
        arm === "old" && n >= 7
          ? { reply: [look, act], skillReads: [[answered]] }
          : { reply: [look] },
    };
    for (const [id, screen] of Object.entries(screens)) {
      writeMadeUpResult(s.out, id, { screen }, { compared: "none", step: steps[id] });
    }
    const reported = await cli(report(s));
    expect({ code: reported.code, stderr: reported.stderr }).toEqual({ code: 0, stderr: "" });
    const lines = reported.stdout.split("\n");
    const difference = (id: string) => ((screens[id]?.old ?? 0) - (screens[id]?.none ?? 0)) / 10;
    expect(lines).toContain(
      "inspection floors (screened cases on which at least 8 of the 10 screen runs of each screen arm are judged fail on a step whose own tool calls are all read-only calls; a description beside the registered rule, which drops no case): 2 of 4 screened (session 1: c04; written 1: w00)"
    );
    expect(lines).toContain(
      `mean of old less none in screen pass rate, each screened case one observation (a description beside the registered rule): over all 4 screened cases ${meanWords(["c04", "c05", "w00", "w01"].map(difference))}; over the 2 screened cases that are not inspection floors (c05, w01) ${meanWords(["c05", "w01"].map(difference))}`
    );
    // Each case is still screened and counted.
    expect(lines).toContain("cases by kind: 2 session (1 informative), 2 written (0 informative)");
  }, 60_000);

  // Catches: a case the arm with no skill fails by construction, since its real step reads a file
  // of the skill that arm does not have, counted beside the verdict with no word that says so.
  test("report names the screened cases whose real step reads a file of the skill other than SKILL.md, and counts the others", async () => {
    const s = scenario("report-step-reads", f1File(2), [
      { id: "c04" },
      { id: "c05" },
      { id: "c06" },
    ]);
    // Where F1's profile installed alpha, as its first `[Skill file: …]` line shows it.
    const installed = path.dirname(skillHeaderPath(messages[2]?.content?.[0]?.text ?? "") ?? "");
    /** The cases of `s` again in its cases file, each id of `targets` with a real step that reads
     *  that path or URL. */
    const reading = (targets: Record<string, string>) => {
      const cases = s.cases.map((c) => {
        const target = targets[c.id];
        if (target === undefined) return c;
        const content = [
          ...(c.realStep.content ?? []),
          { type: "toolCall", id: `read_${c.id}`, name: "read", arguments: { path: target } },
        ];
        return { ...c, realStep: { ...c.realStep, content } };
      });
      writeFileSync(s.casesFile, `${cases.map((c) => JSON.stringify(c)).join("\n")}\n`);
    };
    reading({
      c04: "skill://alpha/references/x.md",
      c05: `${installed}/references/x.md`,
    });
    /** Writes the results of the three cases under `out`, compared with `compared`. */
    const write = (out: string, compared: "none" | "new") => {
      // The compared arm's screen passes, and no skill's: none of 10 where the cut is compared.
      const other = (passes: number) => ({
        [compared]: passes,
        none: compared === "new" ? 0 : passes,
      });
      writeMadeUpResult(
        out,
        "c04",
        { screen: { old: 10, ...other(0) }, confirm: { old: 40, [compared]: 0 } },
        { compared }
      );
      writeMadeUpResult(out, "c05", { screen: { old: 10, ...other(0) } }, { compared });
      writeMadeUpResult(
        out,
        "c06",
        { screen: { old: 8, ...other(4) }, confirm: { old: 40, [compared]: 20 } },
        { compared }
      );
    };
    write(s.out, "none");
    const reported = await cli(report(s));
    expect({ code: reported.code, stderr: reported.stderr }).toEqual({ code: 0, stderr: "" });
    const lines = reported.stdout.split("\n");
    const line =
      "cases whose real step reads a file of the skill other than SKILL.md, which the arm with no skill does not have, so it fails them by construction: 2 of 3 screened (c04 reads references/x.md; c05 reads references/x.md); the 1 screened case with no such read: 1 informative, 1 flagged, 1 confirmed losses (a description beside the registered rule, which it does not change)";
    expect(lines).toContain(line);
    // It comes after the lines of the cases by kind.
    expect(lines.indexOf(line)).toBeGreaterThan(lines.indexOf("screened written cases: none"));
    // The cases line still counts every screened case: the line changes no count of the rule.
    expect(reported.stdout).toContain(
      "cases: 3 screened, 3 informative, 3 flagged, 2 confirmed losses;"
    );

    // With the cut compared, no skill is in the screen too, and the line is the same.
    const cut = { ...s, out: path.join(s.dir, "out-new") };
    write(cut.out, "new");
    const withCut = await cli(report(cut));
    expect({ code: withCut.code, stderr: withCut.stderr }).toEqual({ code: 0, stderr: "" });
    expect(withCut.stdout.split("\n")).toContain(line);

    // A read of SKILL.md, and of a path inside the skill that is no file of --old, is no such read.
    reading({
      c04: "skill://alpha",
      c05: `${installed}/references/missing.md`,
    });
    const none = await cli(report(s));
    expect(none.stdout.split("\n")).toContain(
      "cases whose real step reads a file of the skill other than SKILL.md, which the arm with no skill does not have, so it fails them by construction: none of 3 screened; the 3 screened cases with no such read: 3 informative, 3 flagged, 2 confirmed losses (a description beside the registered rule, which it does not change)"
    );

    // An --old whose reference is a symbolic link to a file outside it, as the arms take one: the
    // line counts it as a file of the skill, as build-cases --drop-navigation lists it (one
    // listing, readers.ts's skillFilePaths; build.test.ts, "lists the files of an --old whose
    // files are symbolic links").
    reading({
      c04: "skill://alpha/references/x.md",
      c05: `${installed}/references/x.md`,
    });
    const outside = path.join(s.dir, "x-outside.md");
    writeFileSync(outside, f1File(4));
    rmSync(path.join(s.old, "references", "x.md"));
    symlinkSync(outside, path.join(s.old, "references", "x.md"));
    const linked = await cli(report(s));
    expect({ code: linked.code, stderr: linked.stderr }).toEqual({ code: 0, stderr: "" });
    expect(linked.stdout.split("\n")).toContain(line);
  }, 60_000);

  // Catches: a report that stops at one case whose arms buildArm refuses, or counts that case
  // among the cases that read a file of the skill or among the others.
  test("report names a screened case whose arms buildArm refuses as one whose reads cannot be told, and still exits 0", async () => {
    const s = scenario("report-step-untold", f1File(2), [
      { id: "c04" },
      { id: "c05" },
      { id: "c07" },
    ]);
    // c07's messages open with a `/skill:alpha` prompt the user typed, a block the arms cannot
    // rebuild, so buildArm refuses its arms; its result records a drop at confirm, after its
    // screen, so it is a screened case.
    const typed: SessionMessage = {
      role: "custom",
      customType: "skill-prompt",
      content: [{ type: "text", text: "# Alpha\n\nThe user's prompt." }],
      display: true,
      details: { name: "alpha", path: "/skills/alpha/SKILL.md" },
      attribution: "user",
      timestamp: 1795139983600,
    };
    const read = { type: "toolCall", id: "read_c04", name: "read" };
    const cases = s.cases.map((c) =>
      c.id === "c04"
        ? {
            ...c,
            realStep: {
              ...c.realStep,
              content: [
                ...(c.realStep.content ?? []),
                { ...read, arguments: { path: "skill://alpha/references/x.md" } },
              ],
            },
          }
        : c.id === "c07"
          ? {
              ...c,
              messages: [typed, ...c.messages],
              skillReads: c.skillReads.map((index) => index + 1),
            }
          : c
    );
    writeFileSync(s.casesFile, `${cases.map((c) => JSON.stringify(c)).join("\n")}\n`);
    const screens: Record<string, { old: number; none: number }> = {
      c04: { old: 10, none: 0 },
      c05: { old: 8, none: 4 },
      c07: { old: 10, none: 0 },
    };
    for (const [id, screen] of Object.entries(screens)) {
      writeMadeUpResult(s.out, id, { screen }, { compared: "none" });
    }
    const file = caseResultFile(s.out, "c07");
    const dropped = { dropped: UNREADABLE, droppedAt: "confirm" };
    const made = JSON.parse(readFileSync(file, "utf8"));
    writeFileSync(file, JSON.stringify(CaseResult.parse({ ...made, ...dropped })));
    const reported = await cli(report(s));
    expect({ code: reported.code, stderr: reported.stderr }).toEqual({ code: 0, stderr: "" });
    expect(reported.stdout.split("\n")).toContain(
      `cases whose real step reads a file of the skill other than SKILL.md, which the arm with no skill does not have, so it fails them by construction: 1 of 3 screened (c04 reads references/x.md); c07: its reads cannot be told (case c07: its message 0 is a skill-prompt message of alpha, one the user invoked, not in the form of Oh My Pi's autoloaded block (one text part that ends "Skill: <its details.path>")); the 1 screened case with no such read: 1 informative, 1 flagged, 0 confirmed losses (a description beside the registered rule, which it does not change)`
    );
  }, 60_000);

  /** alpha's line 8, which only sends the agent to references/x.md. */
  const NAVIGATION = "1. Read `references/x.md` before the first step.";

  // Catches: a navigation line of the skill that a build dropped steps for and no case tests left
  // out of the report, a tested one named, one named twice, and a file that is no build-cases
  // summary read as one.
  test("report --build-summary names each navigation line of the build summaries that no case has as its line, once, or none, and refuses a file that is no build-cases summary", async () => {
    const s = scenario("report-navigation", f1File(2), [{ id: "c04" }, { id: "c05" }]);
    writeMadeUpResult(s.out, "c04", { screen: { old: 10, none: 0 } }, { compared: "none" });
    writeMadeUpResult(s.out, "c05", { screen: { old: 8, none: 4 } }, { compared: "none" });
    /** Writes the summary file `name` of a build-cases form `command` that dropped one step for
     *  each line of `lines` (file and number of SKILL.md, and text), and gives its path. */
    const buildSummary = (name: string, command: string, lines: [number, string][]) => {
      const file = path.join(s.dir, name);
      const navigation = {
        lines: lines.map(([number, text]) => ({ file: "SKILL.md", number, text, dropped: 1 })),
        dropped: lines.length,
        withReplacement: 0,
        withoutReplacement: lines.length,
      };
      writeFileSync(file, JSON.stringify({ command, navigation }));
      return file;
    };
    // The session build names line 8 and line 9 (LINE, which the cases test); the written build
    // names line 8 again.
    const sessionBuild = buildSummary("session.summary.json", "build-cases", [
      [8, NAVIGATION],
      [9, LINE],
    ]);
    const writtenBuild = buildSummary("written.summary.json", "build-cases --written", [
      [8, NAVIGATION],
    ]);
    const reported = await cli([
      ...report(s),
      "--build-summary",
      sessionBuild,
      "--build-summary",
      writtenBuild,
    ]);
    expect({ code: reported.code, stderr: reported.stderr }).toEqual({ code: 0, stderr: "" });
    const lines = reported.stdout.split("\n");
    const line = `navigation lines of the skill that no case tests (from the build summaries): SKILL.md:8 ${JSON.stringify(NAVIGATION)}`;
    expect(lines).toContain(line);
    // It comes beside the verdict, and the verdict is the rule's.
    const verdict = lines.findIndex((each) => each.startsWith("verdict: "));
    expect(lines.indexOf(line)).toBe(verdict + 1);
    const plain = await cli(report(s));
    expect(plain.stdout.split("\n")[verdict]).toBe(lines[verdict]);
    // Without --build-summary the report has no such line.
    expect(plain.stdout).not.toContain("navigation lines of the skill");

    // Every navigation line tested by a case: none.
    const tested = buildSummary("tested.summary.json", "build-cases", [[9, LINE]]);
    const none = await cli([...report(s), "--build-summary", tested]);
    expect(none.stdout.split("\n")).toContain(
      "navigation lines of the skill that no case tests (from the build summaries): none"
    );

    // A file that is no build-cases summary is refused, and so is the flag on a stage command.
    const tasks = path.join(s.dir, "tasks.summary.json");
    writeFileSync(tasks, JSON.stringify({ command: "build-tasks", queries: [] }));
    const refused = await cli([...report(s), "--build-summary", tasks]);
    expect({ code: refused.code, stdout: refused.stdout }).toEqual({ code: 2, stdout: "" });
    expect(refused.stderr).toContain(`--build-summary ${tasks} is no build-cases summary`);
    const screen = await cli(behaviour("screen", s, "--build-summary", tested));
    expect(screen.code).toBe(2);
    expect(screen.stderr).toContain("--build-summary is for behaviour report only");
  }, 60_000);

  // Catches: a replacement case a judge control dropped counted as one that stands, or left
  // unnamed; one with no result yet left out of the count; one two summaries name counted twice.
  test("report --build-summary counts the replacement cases the build summaries' session records name and those the judge controls leave, naming each the controls dropped and each not screened yet", async () => {
    const s = scenario("report-replacements", f1File(2), [{ id: "c04" }, { id: "c05" }]);
    writeMadeUpResult(s.out, "c04", { screen: { old: 10, none: 0 } }, { compared: "none" });
    writeMadeUpResult(s.out, "c05", {}, { compared: "none" });
    // c05's judge controls do not hold: its violating step is judged pass.
    const resultFile = caseResultFile(s.out, "c05");
    const made = JSON.parse(readFileSync(resultFile, "utf8"));
    const judged = (verdict: "pass" | "fail") => ({ verdict, reason: "made up" });
    const controls = {
      realStep: judged("pass"),
      violating: judged("pass"),
      nothing: judged("fail"),
      failed: ["violating"],
      hold: false,
      usd: 0,
    };
    const dropped =
      "the judge controls do not hold: the violating step is judged pass (fail wanted)";
    writeFileSync(
      resultFile,
      JSON.stringify(CaseResult.parse({ ...made, dropped, droppedAt: "screen", controls }))
    );
    /** Writes the summary file `name` of a session build with a session that dropped a step under
     *  NAVIGATION for each of `replacements` (the id of the case that replaced it, or null) and one
     *  session that dropped none, and gives its path. */
    const buildSummary = (name: string, replacements: (string | null)[]) => {
      const file = path.join(s.dir, name);
      const withReplacement = replacements.filter((id) => id !== null).length;
      const navigation = {
        lines: [{ file: "SKILL.md", number: 8, text: NAVIGATION, dropped: replacements.length }],
        dropped: replacements.length,
        withReplacement,
        withoutReplacement: replacements.length - withReplacement,
      };
      const passedOver = { messageIndex: 3, file: "SKILL.md", number: 8, text: NAVIGATION };
      const sessions = [
        ...replacements.map((replacement, n) => ({
          store: "store_a",
          hash: String(n).padStart(16, "0"),
          readerUsd: 0,
          outcome: replacement === null ? "dropped" : `case ${replacement}`,
          navigation: [{ ...passedOver, reason: "made up", replacement }],
        })),
        { store: "store_a", hash: "f".repeat(16), readerUsd: 0, outcome: "case c06" },
      ];
      writeFileSync(file, JSON.stringify({ command: "build-cases", navigation, sessions }));
      return file;
    };
    const two = buildSummary("two.summary.json", ["c04", "c05", null]);
    const reported = await cli([...report(s), "--build-summary", two]);
    expect({ code: reported.code, stderr: reported.stderr }).toEqual({ code: 0, stderr: "" });
    const lines = reported.stdout.split("\n");
    const line =
      "navigation drops with a replacement case: 2 by the build summaries, 1 after the judge controls (c05 dropped by the judge controls)";
    expect(lines).toContain(line);
    // It comes beside the navigation lines no case tests.
    const untested = lines.findIndex((each) => each.startsWith("navigation lines of the skill"));
    expect(lines.indexOf(line)).toBe(untested + 1);

    // A case two summaries name counts once; one with no result yet counts, and is named.
    const more = buildSummary("more.summary.json", ["c05", "c09"]);
    const both = await cli([...report(s), "--build-summary", two, "--build-summary", more]);
    expect(both.stdout.split("\n")).toContain(
      "navigation drops with a replacement case: 3 by the build summaries, 2 after the judge controls (c05 dropped by the judge controls; c09 not screened yet)"
    );
    // Without --build-summary the report has no such line.
    const plain = await cli(report(s));
    expect(plain.stdout).not.toContain("navigation drops with a replacement case");
  }, 60_000);

  test("the judge prompt's sha256 and settings are in the report and in each newly judged stage", async () => {
    const s = scenario("judge-identity", LOSS_SKILL(), [{ id: "c04" }]);
    recordCaptures(s, ["old", "none"]);
    answer = behaviourStandIn;
    usage = { input: 10, output: 3 };
    const screen = await cli(comparedArgs("screen", s, "none"));
    expect({ code: screen.code, stderr: screen.stderr }).toEqual({ code: 0, stderr: "" });
    const prompt = readFileSync(path.join(import.meta.dir, "prompts", "judge.md"));
    const settings = loadPrompt("judge").settings;
    expect(result(s.out, "c04").stages.screen?.judge).toEqual({ sha256: sha256(prompt), settings });
    const reported = await cli(report(s));
    expect(reported.stdout.split("\n")).toContain(
      `judge: prompts/judge.md sha256 ${sha256(prompt)}; model ${settings.model}, thinking_budget ${settings.thinking_budget}, max_tokens ${settings.max_tokens}, temperature ${settings.temperature}, effort ${settings.effort}; recorded by 1 of the 1 stages of the results, another judge by 0, none by 0`
    );
  }, 120_000);

  // Catches: a written case's model counted among the reading sessions a verdict speaks for, which
  // would claim a session on that model was tested.
  test("a written case's model is counted under written tasks, never among the reading sessions the verdict speaks for", async () => {
    const models = readingModels(scratch, "written-reading-models.json", {
      sessions: 11,
      byModel: [
        { model: "anthropic/claude-sonnet-5-5", anthropic: true, served: true, sessions: 7 },
        { model: "openai/gpt-6.1-sol", anthropic: false, served: true, sessions: 2 },
        { model: "openai-codex/gpt-6-astra", anthropic: false, served: false, sessions: 2 },
      ],
      noModel: 0,
      notQueried: 0,
      notAnthropic: 4,
      served: 9,
      withModel: 11,
    });
    /** A scenario of `session` session cases and the written cases `written` (id to model), each
     *  screened at old 10, none 0, confirmed at old 40, none 35, and c04 or w04 the control case. */
    const scenarioOf = (name: string, session: string[], written: Record<string, string>) => {
      const base = scenario(
        name,
        f1File(2),
        session.map((id) => ({ id }))
      );
      const cases = [
        ...base.cases,
        ...Object.entries(written).map(([id, model]) => ({ ...makeWrittenCase(id), model })),
      ];
      writeFileSync(base.casesFile, `${cases.map((c) => JSON.stringify(c)).join("\n")}\n`);
      const s = { ...base, cases };
      for (const c of cases) {
        writeMadeUpResult(
          s.out,
          c.id,
          {
            screen: { old: 10, none: 0 },
            confirm: { old: 40, none: 35 },
            ...(c.id === cases[0]?.id ? { control: { old: 40, minus: 0, twin: 40 } } : {}),
          },
          { compared: "none" }
        );
      }
      return s;
    };
    // Session cases on anthropic/claude-sonnet-5-5, written ones on openai/gpt-6.1-sol: the verdict
    // speaks for the sonnet sessions alone, and the openai ones are sessions no screened session
    // case ran on.
    const mixed = scenarioOf("written-models", ["c04", "c05", "c06"], {
      w04: "openai/gpt-6.1-sol",
      w05: "openai/gpt-6.1-sol",
      w06: "openai/gpt-6.1-sol",
    });
    const lines = (await cli([...report(mixed), "--reading-models", models])).stdout.split("\n");
    for (const line of [
      "screened session cases by the served model they ran on: anthropic/claude-sonnet-5-5 3 cases",
      "written tasks: 3 on openai/gpt-6.1-sol (screened written cases: each a written task in a recorded session's context, never a reading session)",
      `${NO_SKILL_KEEPS}; it speaks for the reading sessions on anthropic/claude-sonnet-5-5 (7 sessions); not for the 2 sessions on models the gateway does not serve; nor for the 2 sessions on served models no screened session case ran on${unmeasured(6, 6)}`,
    ]) {
      expect(lines).toContain(line);
    }
    // Every screened case written: the verdict speaks for no reading sessions, and says so.
    const written = scenarioOf("written-only-models", [], {
      w04: "anthropic/claude-sonnet-5-5",
      w05: "anthropic/claude-sonnet-5-5",
      w06: "anthropic/claude-sonnet-5-5",
      w07: "anthropic/claude-sonnet-5-5",
      w08: "openai/gpt-6.1-sol",
      w09: "openai/gpt-6.1-sol",
    });
    const only = (await cli([...report(written), "--reading-models", models])).stdout.split("\n");
    for (const line of [
      "screened session cases by the served model they ran on: none",
      "written tasks: 4 on anthropic/claude-sonnet-5-5, 2 on openai/gpt-6.1-sol (screened written cases: each a written task in a recorded session's context, never a reading session)",
      `${NO_SKILL_KEEPS.replace("case c04", "case w04")}; it speaks for no reading sessions: its screened cases are all written tasks, never reading sessions${unmeasured(6, 6)}`,
    ]) {
      expect(only).toContain(line);
    }
  }, 60_000);

  test("a results file with no judge identity still loads", async () => {
    const s = scenario("judge-none", f1File(2), [{ id: "c04" }, { id: "c05" }]);
    for (const id of ["c04", "c05"]) {
      writeMadeUpResult(s.out, id, { screen: { old: 10, none: 0 } }, { compared: "none" });
    }
    const reported = await cli(report(s));
    expect({ code: reported.code, stderr: reported.stderr }).toEqual({ code: 0, stderr: "" });
    expect(reported.stdout).toContain(
      "; recorded by 0 of the 2 stages of the results, another judge by 0, none by 2 (judged before the judge's identity was recorded)"
    );
  }, 60_000);
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

// The task's architect (2026-11-25T18:54:22Z and 19:02:42Z): cases come from Postgres sessions
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
    // The written tasks line, which names no reading session, stands between it and the verdict.
    expect(some[at + 1]).toStartWith("written tasks: ");
    expect(some[at + 2]).toStartWith("verdict: ");
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

  // Catches: a screen with ours compared that checks only the old and new texts, so buildArm's
  // throw on our file fails the whole command at the capture.
  test("with ours compared, a case whose file read by path our text holds in such a form is dropped at screen as path-form: ours, and counted under ours", async () => {
    const s = withOurs(
      scenario("path-form-ours", LOSS_SKILL(), [{ id: "c-wide" }, { id: "c-plain" }]),
      "ours",
      OURS_SKILL()
    );
    const [wide, plain] = s.cases;
    writeSkill(s.ours?.dir ?? "", {
      "references/wide.md": Array.from({ length: 301 }, (_, n) => `Line ${n + 1}.`).join("\n"),
    });
    recordCaptures({ ...s, cases: s.cases.filter((c) => c.id === "c-plain") }, [
      "old",
      "ours",
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
    const ran = await cli(comparedArgs("screen", s, "ours"));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const fault =
      "path-form: ours: references/wide.md has 301 lines, and a read by path shows 300, then the end of each block past them";
    expect(ran.stdout).toContain(`case c-wide: dropped: ${fault}`);
    expect(requestsSince(from).filter((request) => request.body.includes("Case c-wide"))).toEqual(
      []
    );
    expect(result(s.out, "c-plain").stages.screen?.state).toBe("counted");

    const reported = await cli(report(s));
    expect({ code: reported.code, stderr: reported.stderr }).toEqual({ code: 0, stderr: "" });
    expect(reported.stdout).toContain(`case c-wide: dropped at screen: ${fault}`);
    expect(reported.stdout).toContain("1 path-form (old 0, new 0, ours 1)");
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
    "planted-loss control not a loss: its runs are unscreened"
  );
});

// The readers of our skills: `craft`, `nearest` and `control craft`, each against the stand-in,
// which answers each request by its prompt (its system text) with a canned answer, made up. The
// skills are made up (testdata/control-skills.json, CAPTURE.md "The control skills"):
// tidy-settled-change, whose `## Step 4: Verify` runs from line 41 to 46 after Step 3 (29 to 39),
// with Step 5's heading at 47 and its summary at 49; tracker-first, whose three list items at
// lines 37 to 39 sit under its heading at 35; and check-before-claim, a nearest skill of theirs.
// The style text is testdata/style-check.md (S1 to S20; S19 is "No paragraph holds more than one
// rule"), and the craft list is F12 (reader properties C1 and C3; C2 is checked by a behaviour
// test).

const STYLE = path.join(TESTDATA, "style-check.md");
const STYLE_SOURCE = "the style notes, version 1";
const CRAFT_LIST = path.join(TESTDATA, "F12-craft-list.json");
const LIST_SOURCE = "craft-list.json, version 2";
const CONTROL_SKILLS: Readonly<Record<string, string>> = JSON.parse(
  readFileSync(path.join(TESTDATA, "control-skills.json"), "utf8")
);
const sha256 = (bytes: string | Buffer) => createHash("sha256").update(bytes).digest("hex");

/** Writes the control skill `name` under `dir`, and gives its directory. */
function controlSkill(dir: string, name: string): string {
  const skill = path.join(dir, name);
  writeSkill(
    skill,
    Object.fromEntries(
      Object.entries(CONTROL_SKILLS).flatMap(([file, text]) =>
        file.startsWith(`${name}/`) ? [[file.slice(name.length + 1), text]] : []
      )
    )
  );
  return skill;
}

/** A gap of a craft answer, in SKILL.md. */
interface Gap {
  readonly file: string;
  readonly line: number;
  readonly rule: string;
  readonly kind: "present" | "missing";
  readonly why: string;
}
const gap = (line: number, rule: string, why: string, kind: Gap["kind"] = "present"): Gap => ({
  file: "SKILL.md",
  line,
  rule,
  kind,
  why,
});

/** What a craft answer judges: the style text's rules and the craft list's reader properties. */
const JUDGED = [...Array.from({ length: 20 }, (_, n) => `S${n + 1}`), "C1", "C3"];

/** A craft answer holding `gaps`: each rule of JUDGED met, but those the gaps name, judged gap. */
function craftReply(gaps: readonly Gap[]): string {
  const gapped = new Set(gaps.map((each) => each.rule));
  return JSON.stringify({
    judged: JUDGED.map((rule) => ({ rule, verdict: gapped.has(rule) ? "gap" : "met" })),
    gaps,
  });
}

/** The system text and the user text of a request body. */
function bodyTexts(body: { [field: string]: unknown }): { system: string; user: string } {
  const system = (body.system as { text: string }[] | undefined)?.[0]?.text ?? "";
  const messages = body.messages as { content: { text: string }[] }[] | undefined;
  return { system, user: messages?.[0]?.content[0]?.text ?? "" };
}

/** Each request since `from`, as the name of its prompt and its user text. */
function readerRequests(from: number): { prompt: string; user: string }[] {
  return requestsSince(from).map((request) => {
    const { system, user } = bodyTexts(JSON.parse(request.body));
    const prompt = ["craft", "nearest", "claim"].find((name) => loadPrompt(name).text === system);
    return { prompt: prompt ?? "another prompt", user };
  });
}

/** `craft`'s arguments for the skill `skill` and the entry `out`, and `extra`. */
function craftArgs(skill: string, out: string, ...extra: string[]): string[] {
  const { config, keyFile } = setup();
  return [
    ...["craft", "--config", config, "--key-file", keyFile, "--skill", skill],
    ...["--style", STYLE, "--style-source", STYLE_SOURCE],
    ...["--craft", CRAFT_LIST, "--craft-source", LIST_SOURCE, "--out", out, ...extra],
  ];
}

describe("craft", () => {
  // Catches: an entry that cannot say which style text or craft list, of which version, its gaps
  // were read against, or a property a behaviour test checks sent to the reader.
  test("craft's entry records the style text's and the craft list's source and sha256, and the request sends the list's reader properties", async () => {
    const dir = path.join(scratch, "craft-style");
    const skill = controlSkill(dir, "tidy-settled-change");
    const gaps = [gap(39, "S8", "The bold label holds two rules.")];
    answer = () => craftReply(gaps);
    const out = path.join(dir, "entry.json");
    const from = recorder.requests.length;
    const ran = await cli(craftArgs(skill, out));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const entry = JSON.parse(readFileSync(out, "utf8"));
    expect(entry.entry).toBe("craft");
    expect(entry.style).toEqual({
      file: STYLE,
      source: STYLE_SOURCE,
      sha256: sha256(readFileSync(STYLE)),
      rules: 20,
    });
    expect(entry.craft).toEqual({
      file: CRAFT_LIST,
      source: LIST_SOURCE,
      sha256: sha256(readFileSync(CRAFT_LIST)),
      readerProperties: ["C1", "C3"],
    });
    expect(entry.answer).toEqual(JSON.parse(craftReply(gaps)));
    expect(entry.reader).toEqual({ prompt: "craft", settings: loadPrompt("craft").settings });
    expect(entry.refusals).toEqual([]);
    expect(entry.spent).toHaveLength(1);
    expect(typeof entry.usd).toBe("number");
    const requests = readerRequests(from);
    expect(requests.map((request) => request.prompt)).toEqual(["craft"]);
    expect(requests[0]?.user).toContain(
      '<rule id="S19" line="33" part="the check before you commit">\n- No paragraph holds more than one rule.\n</rule>'
    );
    expect(requests[0]?.user).toContain(
      '<property id="C1">\n<states>The description says when to use the skill, and never sums up its steps.</states>'
    );
    expect(requests[0]?.user).toContain('<property id="C3">');
    expect(requests[0]?.user).not.toContain("C2");
    expect(requests[0]?.user).not.toContain("an arm that runs with no skill");
    expect(ran.stdout).toContain("craft: tidy-settled-change: 1 gap (S8)");
  }, 30_000);

  // Catches: a nearest skill named in the entry but not sent, or sent without its references.
  test("craft sends each --nearest skill's files", async () => {
    const dir = path.join(scratch, "craft-nearest");
    const skill = controlSkill(path.join(dir, "ours"), "tidy-settled-change");
    const check = controlSkill(path.join(dir, "theirs"), "check-before-claim");
    const beta = path.join(dir, "theirs", "beta-skill-one");
    const betaFiles = {
      "SKILL.md":
        "---\nname: beta-skill-one\ndescription: Use for committing a finished change.\n---\n\n1. Stage only the change.\n",
      "references/r.md": "# R\n\nRead the diff before you stage it.\n",
    };
    writeSkill(beta, betaFiles);
    const gaps = [
      gap(43, "nearest:check-before-claim:SKILL.md:8", "No step names the check before the claim."),
    ];
    answer = () => craftReply(gaps);
    const out = path.join(dir, "entry.json");
    const from = recorder.requests.length;
    const ran = await cli(craftArgs(skill, out, "--nearest", check, "--nearest", beta));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const entry = JSON.parse(readFileSync(out, "utf8"));
    expect(entry.nearest).toEqual([
      {
        name: "check-before-claim",
        dir: check,
        files: [
          { path: "SKILL.md", sha256: sha256(CONTROL_SKILLS["check-before-claim/SKILL.md"] ?? "") },
        ],
      },
      {
        name: "beta-skill-one",
        dir: beta,
        files: [
          { path: "SKILL.md", sha256: sha256(betaFiles["SKILL.md"]) },
          { path: "references/r.md", sha256: sha256(betaFiles["references/r.md"]) },
        ],
      },
    ]);
    expect(entry.answer.gaps).toEqual(gaps);
    const [request] = readerRequests(from);
    expect(request?.user).toContain(
      '<nearest>\n<skill name="check-before-claim">\n<file path="SKILL.md">\n1: ---\n2: name: check-before-claim'
    );
    expect(request?.user).toContain(
      "8: 1. Name the command that proves the claim: the typecheck, the lint, the build or the tests."
    );
    expect(request?.user).toContain(
      '<skill name="beta-skill-one">\n<file path="SKILL.md">\n1: ---\n2: name: beta-skill-one'
    );
    expect(request?.user).toContain(
      '<file path="references/r.md">\n1: # R\n2: \n3: Read the diff before you stage it.\n</file>\n</skill>\n</nearest>'
    );
  }, 30_000);

  // Catches: a read of a file the skill does not hold as `.md`, of another skill of ours, or a
  // second request; and an input file that is not the message sent.
  test("craft reads every *.md of --skill and no other directory of ours", async () => {
    const dir = path.join(scratch, "craft-reads");
    const skill = controlSkill(path.join(dir, "ours"), "tidy-settled-change");
    controlSkill(path.join(dir, "ours"), "tracker-first");
    answer = () => craftReply([]);
    const out = path.join(dir, "entry.json");
    const from = recorder.requests.length;
    const ran = await cli(craftArgs(skill, out, "--skill-source", "acme/skills at 1a2b3c4d"));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const entry = JSON.parse(readFileSync(out, "utf8"));
    expect(entry.skill).toEqual({
      name: "tidy-settled-change",
      dir: skill,
      source: "acme/skills at 1a2b3c4d",
      files: [
        {
          path: "SKILL.md",
          sha256: sha256(CONTROL_SKILLS["tidy-settled-change/SKILL.md"] ?? ""),
        },
        {
          path: "references/reuse-reader.md",
          sha256: sha256(CONTROL_SKILLS["tidy-settled-change/references/reuse-reader.md"] ?? ""),
        },
      ],
    });
    expect(entry.nearest).toEqual([]);
    const requests = readerRequests(from);
    expect(requests.map((request) => request.prompt)).toEqual(["craft"]);
    const user = requests[0]?.user ?? "";
    expect(user).toStartWith('<skill name="tidy-settled-change">\n<file path="SKILL.md">\n1: ---');
    expect(user).toContain("49: Say what was already sound and what improved.");
    expect(user).toContain('<file path="references/reuse-reader.md">\n1: # Reuse reader');
    expect(user).not.toContain("Made-up licence text");
    expect(user).not.toContain("tracker-first");
    expect(user).toEndWith("<nearest>\nNone: no nearest skill of theirs was given.\n</nearest>");
    expect(entry.input).toBe(path.join(dir, "entry.input.md"));
    expect(readFileSync(path.join(dir, "entry.input.md"), "utf8")).toBe(user);
  }, 30_000);

  test("craft writes each refusal and a null answer when every reply is refused, and exits 1", async () => {
    const dir = path.join(scratch, "craft-refused");
    const skill = controlSkill(dir, "tidy-settled-change");
    answer = () => craftReply([gap(60, "S8", "Past the end of the file.")]);
    const out = path.join(dir, "entry.json");
    const from = recorder.requests.length;
    const ran = await cli(craftArgs(skill, out));
    expect(ran.code).toBe(1);
    const entry = JSON.parse(readFileSync(out, "utf8"));
    expect(entry.answer).toBeNull();
    expect(entry.refusals).toHaveLength(READER_TRIES);
    expect(entry.refusals[0]).toContain("gaps.0.line: is past the last line, 49, of SKILL.md");
    expect(requestsSince(from)).toHaveLength(READER_TRIES);
    expect(ran.stderr).toContain("craft: the craft reader's answer was refused");
  }, 30_000);

  test("craft refuses a style text with no rule, a craft list the shape refuses, a --skill with no SKILL.md and a missing source, before any call", async () => {
    const dir = path.join(scratch, "craft-refusals");
    const skill = controlSkill(dir, "tidy-settled-change");
    const noRules = path.join(dir, "no-rules.md");
    writeFileSync(noRules, "# Notes\n\nNo table and no rules here.\n");
    const twice = path.join(dir, "twice.json");
    const list = JSON.parse(readFileSync(CRAFT_LIST, "utf8"));
    list.properties[2].id = "C1";
    writeFileSync(twice, JSON.stringify(list));
    const notSkill = path.join(dir, "not-a-skill");
    mkdirSync(notSkill, { recursive: true });
    const out = path.join(dir, "entry.json");
    const from = recorder.requests.length;
    const swap = (args: string[], flag: string, value: string) =>
      args.map((arg, at) => (args[at - 1] === flag ? value : arg));
    for (const [args, why] of [
      [swap(craftArgs(skill, out), "--style", noRules), "the style text"],
      [swap(craftArgs(skill, out), "--craft", twice), "gives the id C1 twice"],
      [craftArgs(notSkill, out), "has no SKILL.md"],
      [craftArgs(skill, out, "--nearest", notSkill), "has no SKILL.md"],
      [
        craftArgs(skill, out).filter(
          (arg, at, all) => arg !== "--style-source" && all[at - 1] !== "--style-source"
        ),
        "--style-source required",
      ],
    ] as const) {
      const ran = await cli(args);
      expect({ why, code: ran.code, says: ran.stderr.includes(why) }).toEqual({
        why,
        code: 2,
        says: true,
      });
    }
    expect(requestsSince(from)).toHaveLength(0);
    expect(existsSync(out)).toBe(false);
  }, 60_000);
});

describe("nearest", () => {
  /** Their skill, whose folded description spans two lines and holds colons, and two roots of
   *  ours: each holds a skill named ours-one, and the second's description is two lines kept. */
  function nearestScenario(name: string) {
    const dir = path.join(scratch, name);
    const skill = path.join(dir, "theirs", "verify-first");
    writeSkill(skill, {
      "SKILL.md":
        "---\nname: verify-first\ndescription: >-\n  Use when about to say that work is done: run the checks first,\n  and report: what ran, and what it showed.\n---\n\n# Verify first\n\n1. Run the checks.\n",
    });
    const rootA = path.join(dir, "ours-a");
    const rootB = path.join(dir, "ours-b");
    writeSkill(rootA, {
      "ours-one/SKILL.md":
        '---\nname: ours-one\ndescription: "Use when a task asks for the alpha checklist: every step."\n---\n',
      "tidy/SKILL.md":
        "---\nname: tidy\ndescription: Use when a change is done and before review.\n---\n",
      "notes/README.md": "No skill here.\n",
    });
    writeSkill(rootB, {
      "ours-one/SKILL.md":
        "---\nname: ours-one\ndescription: |-\n  Use when a session ends.\n  Check the work first.\n---\n",
      "ours-two/SKILL.md": "---\nname: ours-two\ndescription: Use to greet.\n---\n",
    });
    const { config, keyFile } = setup();
    const out = path.join(dir, "row.json");
    const args = [
      ...["nearest", "--config", config, "--key-file", keyFile, "--skill", skill],
      ...["--skill-source", "acme/alpha-set at 2ce8aa15"],
      ...["--candidates", `acme/skills at 1a2b3c4d=${rootA}`],
      ...["--candidates", `acme/dotfiles at 5e6f7a8b=${rootB}`, "--out", out],
    ];
    return { dir, skill, rootA, rootB, out, args };
  }
  const THEIR_DESCRIPTION =
    "Use when about to say that work is done: run the checks first, and report: what ran, and what it showed.";

  // Catches: a description cut at its first line or at a colon, in the request or the row, or a
  // named skill of ours given another root's description.
  test("nearest-theirs rows quote both descriptions whole", async () => {
    const s = nearestScenario("nearest-whole");
    const reply = {
      verdict: "partial",
      nearest: [
        {
          name: "ours-one",
          source: "acme/dotfiles at 5e6f7a8b",
          verdict: "partial",
          missing: "the report of what ran",
          reason: "It checks the work before a session ends.",
        },
      ],
      reason: "One skill of ours checks the work, and none reports it.",
    };
    answer = () => JSON.stringify(reply);
    const from = recorder.requests.length;
    const ran = await cli(s.args);
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const row = JSON.parse(readFileSync(s.out, "utf8"));
    expect(row.skill).toEqual({
      name: "verify-first",
      description: THEIR_DESCRIPTION,
      source: "acme/alpha-set at 2ce8aa15",
      dir: s.skill,
    });
    expect(row.answer).toEqual(reply);
    expect(row.nearest).toEqual([
      { ...reply.nearest[0], description: "Use when a session ends.\nCheck the work first." },
    ]);
    expect(row.candidates).toEqual([
      { source: "acme/skills at 1a2b3c4d", root: s.rootA, skills: 2 },
      { source: "acme/dotfiles at 5e6f7a8b", root: s.rootB, skills: 2 },
    ]);
    const [request] = readerRequests(from);
    expect(request?.prompt).toBe("nearest");
    expect(request?.user).toStartWith(
      `<skill name="verify-first">\n<description>\n${THEIR_DESCRIPTION}\n</description>\n<body>\n# Verify first\n\n1. Run the checks.\n</body>\n</skill>`
    );
    expect(request?.user).toContain(
      '<candidate name="ours-one" source="acme/skills at 1a2b3c4d">\nUse when a task asks for the alpha checklist: every step.\n</candidate>'
    );
    expect(request?.user).toContain(
      '<candidate name="ours-one" source="acme/dotfiles at 5e6f7a8b">\nUse when a session ends.\nCheck the work first.\n</candidate>'
    );
    expect(request?.user).not.toContain("No skill here.");
    expect(ran.stdout).toContain(
      "nearest: verify-first: partial: ours-one (acme/dotfiles at 5e6f7a8b)"
    );
  }, 30_000);

  test("a none answer's row names no skill of ours, and its reason", async () => {
    const s = nearestScenario("nearest-none");
    const reply = { verdict: "none", nearest: [], reason: "No skill of ours checks work first." };
    answer = () => JSON.stringify(reply);
    const ran = await cli(s.args);
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const row = JSON.parse(readFileSync(s.out, "utf8"));
    expect(row.answer).toEqual(reply);
    expect(row.nearest).toEqual([]);
    expect(row.skill.description).toBe(THEIR_DESCRIPTION);
    expect(ran.stdout).toContain("nearest: verify-first: none");
  }, 30_000);

  // Catches: a body that keeps the frontmatter of a SKILL.md that starts with an HTML comment, or
  // a name read untrimmed where load.ts trims it.
  test("the body is SKILL.md less the frontmatter readFrontmatter finds after a leading comment, and the name is trimmed", async () => {
    const s = nearestScenario("nearest-comment");
    writeSkill(s.skill, {
      "SKILL.md":
        "<!-- made up -->\n---\nname: '  verify-first  '\ndescription: Use when about to say that work is done.\n---\n\n# Verify first\n\n1. Run the checks.\n",
    });
    answer = () => JSON.stringify({ verdict: "none", nearest: [], reason: "None checks first." });
    const from = recorder.requests.length;
    const ran = await cli(s.args);
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const [request] = readerRequests(from);
    expect(request?.user).toStartWith(
      '<skill name="verify-first">\n<description>\nUse when about to say that work is done.\n</description>\n<body>\n# Verify first\n\n1. Run the checks.\n</body>\n</skill>'
    );
    expect(JSON.parse(readFileSync(s.out, "utf8")).skill.name).toBe("verify-first");
  }, 30_000);

  test("a --candidates that is not <source>=<root>, or a root with no skill, is refused before any call", async () => {
    const s = nearestScenario("nearest-refused");
    const from = recorder.requests.length;
    const bare = await cli(s.args.map((arg) => (arg.endsWith(s.rootA) ? s.rootA : arg)));
    expect(bare.code).toBe(2);
    expect(bare.stderr).toContain(`--candidates ${s.rootA} is not <source>=<root>`);
    const empty = path.join(s.dir, "empty-root");
    mkdirSync(empty, { recursive: true });
    const none = await cli(s.args.map((arg) => (arg.endsWith(s.rootA) ? `acme/x=${empty}` : arg)));
    expect(none.code).toBe(2);
    expect(none.stderr).toContain(`holds no <name>/SKILL.md`);
    expect(requestsSince(from)).toHaveLength(0);
  }, 30_000);
});

// The `proposals` command's own rows are in proposals.test.ts; this one runs it as the command line
// does, on F11 (testdata/CAPTURE.md "F11") and F15.
describe("proposals", () => {
  test("proposals is a command: --dry-run prints the items, and a --pr-diff that is not <id prefix>=<file> is refused before any call", async () => {
    const body = path.join(TESTDATA, "F11-body.md");
    const diff = path.join(TESTDATA, "F15-pr.diff");
    const lists = [
      "--list",
      "Lines this pull request can carry",
      "--list",
      "Lines whose target is gone",
    ];
    const section = ["--section", "Open proposals of #41 and #75 (R5)"];
    const dry = await cli(["proposals", "--dry-run", "--body", body, ...section, ...lists]);
    expect({ code: dry.code, stderr: dry.stderr }).toEqual({ code: 0, stderr: "" });
    expect(dry.stdout).toStartWith(
      `proposals: ${body}: 7 items: 4 in "Lines this pull request can carry", 3 in "Lines whose target is gone"\n41-1.1 (open): line 9\n`
    );
    const { config, keyFile } = setup();
    const out = path.join(scratch, "proposals-refused");
    const from = recorder.requests.length;
    const refused = await cli([
      ...["proposals", "--config", config, "--key-file", keyFile, "--body", body, ...section],
      ...lists,
      ...["--body-source", "acme/dotfiles#105 at ac1b11e8", "--pr-diff", diff],
      ...[
        "--skills-root",
        `acme/skills at 1a2b3c4d=${scratch}`,
        "--repo-root",
        `acme/dotfiles=${scratch}`,
      ],
      ...["--style", STYLE, "--style-source", STYLE_SOURCE, "--craft", CRAFT_LIST],
      ...[
        "--craft-source",
        LIST_SOURCE,
        "--findings",
        path.join(scratch, "none.jsonl"),
        "--out",
        out,
      ],
    ]);
    expect(refused.code).toBe(2);
    expect(refused.stderr).toContain(`--pr-diff ${diff} is not <id prefix>=<file>`);
    expect(requestsSince(from)).toHaveLength(0);
    expect(existsSync(out)).toBe(false);
  }, 30_000);
});

describe("control craft", () => {
  const CLAIM_CHECK =
    "the skill has no check that runs the typecheck, the lint, the build or the tests before the summary of Step 5";
  const CLAIM_38 = "line 38 holds more than one rule";

  /** The stand-in of a control: a craft request on the unchanged skill (its text holds
   *  `originalMarker`) gets `original`'s gaps, one on the copy `copy`'s; a claim request says the
   *  gap states the claim when `states` holds for its words. */
  function controlStandIn(given: {
    copy: readonly Gap[];
    original: readonly Gap[];
    originalMarker: string;
    states?: (why: string) => boolean;
  }) {
    const claimPrompt = loadPrompt("claim").text;
    return (body: { [field: string]: unknown }) => {
      const { system, user } = bodyTexts(body);
      if (system === claimPrompt) {
        const why = /<gap [^>]*>\n([\s\S]*?)\n<\/gap>/.exec(user)?.[1] ?? "";
        const states = given.states?.(why) ?? false;
        return JSON.stringify({ states, reason: `${states ? "It says" : "It does not say"} so.` });
      }
      return craftReply(user.includes(given.originalMarker) ? given.original : given.copy);
    };
  }

  /** `control craft`'s arguments on the control skill `name`, written under a fresh directory
   *  `dir`, with `change` (the change and each side's criteria). */
  function controlArgs(dir: string, name: string, ...change: string[]) {
    const { config, keyFile } = setup();
    const skill = controlSkill(path.join(dir, "ours"), name);
    const nearest = controlSkill(path.join(dir, "theirs"), "check-before-claim");
    const out = path.join(dir, "out");
    return {
      skill,
      out,
      args: [
        ...["control", "craft", ...change, "--config", config, "--key-file", keyFile],
        ...["--skill", skill, "--style", STYLE, "--style-source", STYLE_SOURCE],
        ...["--craft", CRAFT_LIST, "--craft-source", LIST_SOURCE, "--nearest", nearest],
        ...["--out", out],
      ],
    };
  }

  const STEP_4 = "## Step 4: Verify";
  /** Controls (1) and (2): tidy-settled-change less its Step 4; the copy's lines 31 to 43 and any
   *  line of the unchanged skill, each with the claim that the check is missing. */
  const REMOVE = [
    ...["--remove-section", STEP_4, "--copy-line", "31-43", "--copy-claim", CLAIM_CHECK],
    ...["--original-line", "all", "--original-claim", CLAIM_CHECK],
  ];
  /** Controls (3) and (4): tracker-first with its lines 37 to 39 joined; a present gap at the
   *  copy's line 37 against the paragraph rule (S19), and the unchanged skill's line 38 with the
   *  claim that it holds two rules. */
  const JOIN = [
    ...["--join-lines", "37-39", "--copy-line", "37", "--copy-kind", "present"],
    ...["--copy-rule", "S19", "--original-line", "38", "--original-claim", CLAIM_38],
  ];
  const LINE_38 = "38: - A result a person asked for is the same";
  /** The copy's gap at line 43, its Step 5's summary: it reports check outcomes no step makes. */
  const OUTCOMES = gap(43, "S9", "Step 5 reports check outcomes that no step produces.");

  /** Runs a control, and gives its exit status, its output, its --out, its control.json and the
   *  prompts of its requests. */
  async function runControl(c: { out: string; args: string[] }) {
    const from = recorder.requests.length;
    const ran = await cli(c.args);
    const control = existsSync(path.join(c.out, "control.json"))
      ? JSON.parse(readFileSync(path.join(c.out, "control.json"), "utf8"))
      : undefined;
    return {
      ...ran,
      out: c.out,
      control,
      prompts: readerRequests(from).map((request) => request.prompt),
    };
  }

  /** Controls (1) and (2), each run once for the tests that read it: `held`, with the copy's gap
   *  at line 43 stating the claim and no gap of the unchanged skill; `none`, with no gap of
   *  either. */
  let held: Awaited<ReturnType<typeof runControl>>;
  let none: Awaited<ReturnType<typeof runControl>>;
  beforeAll(async () => {
    answer = controlStandIn({
      copy: [OUTCOMES],
      original: [],
      originalMarker: STEP_4,
      states: (why) => why === OUTCOMES.why,
    });
    held = await runControl(
      controlArgs(path.join(scratch, "control-holds"), "tidy-settled-change", ...REMOVE)
    );
    answer = controlStandIn({ copy: [], original: [], originalMarker: STEP_4 });
    none = await runControl(
      controlArgs(path.join(scratch, "control-none"), "tidy-settled-change", ...REMOVE)
    );
  }, 60_000);

  // Catches: a control that holds on a copy with no candidate that states its claim, or on an
  // unchanged skill with one.
  test("control craft holds only when the copy has a candidate that states its claim and the unchanged skill has none", async () => {
    expect({ code: held.code, stderr: held.stderr }).toEqual({ code: 0, stderr: "" });
    expect(held.control.held).toBe(true);
    expect(held.prompts.toSorted()).toEqual(["claim", "craft", "craft"]);
    expect(held.stdout).toContain("control craft: held");
    expect(held.control.copy.candidates).toEqual([
      {
        gap: OUTCOMES,
        claim: {
          states: true,
          reason: "It says so.",
          file: path.join(held.out, "claims", "copy-1.json"),
        },
      },
    ]);
    expect(held.control.original.candidates).toEqual([]);

    expect(none.code).toBe(1);
    expect(none.control.held).toBe(false);
    expect(none.stderr).toContain("control craft: not held: the copy has no candidate");

    const states = (why: string) => why === OUTCOMES.why;
    const both = controlArgs(path.join(scratch, "control-both"), "tidy-settled-change", ...REMOVE);
    answer = controlStandIn({
      copy: [OUTCOMES],
      original: [OUTCOMES],
      originalMarker: STEP_4,
      states,
    });
    const failed = await runControl(both);
    expect(failed.code).toBe(1);
    expect(failed.control.copy.holds).toBe(true);
    expect(failed.control.original.holds).toBe(false);
  }, 60_000);

  test("(1) a copy nearest: gap within 31-43 that the claim reader says states the claim holds", async () => {
    const nearestGap = gap(
      40,
      "nearest:check-before-claim:SKILL.md:8",
      "No step runs the check that the nearest skill's line 8 names before the summary.",
      "missing"
    );
    const c = controlArgs(
      path.join(scratch, "control-1-nearest"),
      "tidy-settled-change",
      ...REMOVE
    );
    answer = controlStandIn({
      copy: [nearestGap],
      original: [],
      originalMarker: STEP_4,
      states: (why) => why === nearestGap.why,
    });
    const ran = await runControl(c);
    expect({ code: ran.code, held: ran.control.held }).toEqual({ code: 0, held: true });
    const claim = JSON.parse(readFileSync(path.join(c.out, "claims", "copy-1.json"), "utf8"));
    expect(claim.input).toMatchObject({ claim: CLAIM_CHECK, gap: nearestGap });
    expect(claim.answer).toEqual({ states: true, reason: "It says so." });
    expect(readFileSync(path.join(c.out, "claims", "copy-1.input.md"), "utf8")).toContain(
      `<claim>\n${CLAIM_CHECK}\n</claim>`
    );
  }, 30_000);

  test("(1) a copy present gap at line 43 (Step 5 reports check outcomes no step produces) that states the claim holds", () => {
    expect({ code: held.code, held: held.control.held }).toEqual({ code: 0, held: true });
    expect(held.stdout).toContain(
      "  SKILL.md:43 present S9: Step 5 reports check outcomes that no step produces. (states the claim: It says so.)"
    );
  });

  test("(1) a copy gap at the top (a missing Done when line) is no candidate", async () => {
    const c = controlArgs(path.join(scratch, "control-1-top"), "tidy-settled-change", ...REMOVE);
    answer = controlStandIn({
      copy: [gap(7, "S2", "No Done when line follows the goal.", "missing")],
      original: [],
      originalMarker: STEP_4,
      states: () => true,
    });
    const ran = await runControl(c);
    expect({ code: ran.code, held: ran.control.held }).toEqual({ code: 1, held: false });
    expect(ran.control.copy.candidates).toEqual([]);
    expect(ran.prompts).toEqual(["craft", "craft"]);
  }, 30_000);

  test("(1) a copy candidate the claim reader says does not state the claim does not hold", async () => {
    const c = controlArgs(path.join(scratch, "control-1-not"), "tidy-settled-change", ...REMOVE);
    answer = controlStandIn({
      copy: [gap(35, "S8", "The item holds a rule and its exception.")],
      original: [],
      originalMarker: STEP_4,
      states: () => false,
    });
    const ran = await runControl(c);
    expect({ code: ran.code, held: ran.control.held }).toEqual({ code: 1, held: false });
    expect(ran.control.copy.why).toBe("no candidate of the copy states the claim (1 candidate)");
    expect(ran.prompts.toSorted()).toEqual(["claim", "craft", "craft"]);
  }, 30_000);

  test("(2) an unchanged-skill gap at line 43 about which tests Step 4 runs, which the claim reader says does not state the claim, holds", async () => {
    const which = gap(43, "C1", "Step 4 does not say which tests cover the changed code.");
    const doneWhen = gap(7, "S2", "No Done when line follows the goal.", "missing");
    const c = controlArgs(path.join(scratch, "control-2-which"), "tidy-settled-change", ...REMOVE);
    answer = controlStandIn({
      copy: [OUTCOMES],
      original: [which, doneWhen],
      originalMarker: STEP_4,
      states: (why) => why === OUTCOMES.why,
    });
    const ran = await runControl(c);
    expect({ code: ran.code, held: ran.control.held }).toEqual({ code: 0, held: true });
    expect(ran.control.original.candidates.map((each: { gap: Gap }) => each.gap)).toEqual([
      which,
      doneWhen,
    ]);
    expect(ran.control.original.why).toBe(
      "no candidate of the unchanged skill states the claim (2 candidates)"
    );
    expect(ran.prompts.toSorted()).toEqual(["claim", "claim", "claim", "craft", "craft"]);
  }, 30_000);

  test("(2) an unchanged-skill gap anywhere that states the claim fails", async () => {
    const missing = gap(12, "S8", "The skill has no check before its summary.");
    const c = controlArgs(path.join(scratch, "control-2-states"), "tidy-settled-change", ...REMOVE);
    answer = controlStandIn({
      copy: [OUTCOMES],
      original: [missing],
      originalMarker: STEP_4,
      states: (why) => why === OUTCOMES.why || why === missing.why,
    });
    const ran = await runControl(c);
    expect({ code: ran.code, held: ran.control.held }).toEqual({ code: 1, held: false });
    expect(ran.control.original.why).toBe(
      "a candidate of the unchanged skill states the claim: SKILL.md:12"
    );
  }, 30_000);

  test("(3) a copy gap at line 37 against the paragraph rule holds with no claim reader call", async () => {
    const c = controlArgs(path.join(scratch, "control-3"), "tracker-first", ...JOIN);
    answer = controlStandIn({
      copy: [gap(37, "S19", "The paragraph holds three rules.")],
      original: [
        gap(37, "S8", "The item holds two rules."),
        gap(39, "S8", "The item holds two rules."),
      ],
      originalMarker: LINE_38,
    });
    const ran = await runControl(c);
    expect({ code: ran.code, held: ran.control.held }).toEqual({ code: 0, held: true });
    expect(ran.prompts).toEqual(["craft", "craft"]);
    expect(ran.control.copy.why).toBe("the copy has a candidate: SKILL.md:37");
    expect(ran.control.original.why).toBe("the unchanged skill has no candidate");
  }, 30_000);

  test("(4) a line-38 gap about a missing bold label, which does not state the claim, holds", async () => {
    const label = gap(38, "S8", "The item does not start with a bold label.");
    const c = controlArgs(path.join(scratch, "control-4-label"), "tracker-first", ...JOIN);
    answer = controlStandIn({
      copy: [gap(37, "S19", "The paragraph holds three rules.")],
      original: [label],
      originalMarker: LINE_38,
      states: () => false,
    });
    const ran = await runControl(c);
    expect({ code: ran.code, held: ran.control.held }).toEqual({ code: 0, held: true });
    expect(ran.prompts.toSorted()).toEqual(["claim", "craft", "craft"]);
  }, 30_000);

  test("(4) a line-38 gap that states line 38 holds two rules fails, whatever its rule", async () => {
    for (const rule of ["S9", "C1", "nearest:check-before-claim:SKILL.md:9"]) {
      const two = gap(38, rule, "Line 38 holds two rules: end with an ask, and ask the next step.");
      const c = controlArgs(
        path.join(scratch, `control-4-two-${rule.replaceAll(":", "_")}`),
        "tracker-first",
        ...JOIN
      );
      answer = controlStandIn({
        copy: [gap(37, "S19", "The paragraph holds three rules.")],
        original: [two],
        originalMarker: LINE_38,
        states: (why) => why === two.why,
      });
      const ran = await runControl(c);
      expect({ rule, code: ran.code, held: ran.control.held }).toEqual({
        rule,
        code: 1,
        held: false,
      });
    }
  }, 60_000);

  // Catches: a copy that removes too much or too little, or a joined paragraph that keeps the
  // list markers.
  test("--remove-section and --join-lines write a copy of the skill less the section, or with the list items joined into one paragraph less their markers", async () => {
    // Control (2)'s run with no gap: its copy of tidy-settled-change less Step 4.
    const original = (CONTROL_SKILLS["tidy-settled-change/SKILL.md"] ?? "").split("\n");
    const copyDir = path.join(none.out, "copy", "tidy-settled-change");
    expect(readFileSync(path.join(copyDir, "SKILL.md"), "utf8")).toBe(
      [...original.slice(0, 40), ...original.slice(46)].join("\n")
    );
    expect(readFileSync(path.join(copyDir, "references", "reuse-reader.md"), "utf8")).toBe(
      CONTROL_SKILLS["tidy-settled-change/references/reuse-reader.md"]
    );
    for (const file of ["copy.json", "copy.input.md", "original.json", "original.input.md"]) {
      expect({ file, written: existsSync(path.join(none.out, file)) }).toEqual({
        file,
        written: true,
      });
    }

    const joined = controlArgs(path.join(scratch, "control-copy-join"), "tracker-first", ...JOIN);
    answer = controlStandIn({ copy: [], original: [], originalMarker: LINE_38 });
    await runControl(joined);
    const lines = (CONTROL_SKILLS["tracker-first/SKILL.md"] ?? "").split("\n");
    const copy = readFileSync(path.join(joined.out, "copy", "tracker-first", "SKILL.md"), "utf8");
    expect(copy).toBe(
      [
        ...lines.slice(0, 36),
        lines
          .slice(36, 39)
          .map((line) => line.replace(/^- /, ""))
          .join(" "),
        ...lines.slice(39),
      ].join("\n")
    );
    // The joined line starts with the first item's text, its list marker gone.
    expect(copy.split("\n")[36]).toStartWith("The moment a person blocks your work");

    // A heading the skill does not hold, or a range that is not list items, is refused before
    // any call.
    const from = recorder.requests.length;
    const noHeading = controlArgs(
      path.join(scratch, "control-copy-refused-1"),
      "tidy-settled-change",
      ...REMOVE.map((arg) => (arg === STEP_4 ? "## Step 9: Ship" : arg))
    );
    const refusedHeading = await cli(noHeading.args);
    expect(refusedHeading.code).toBe(2);
    expect(refusedHeading.stderr).toContain("no line of SKILL.md is the heading ## Step 9: Ship");
    const notItems = controlArgs(
      path.join(scratch, "control-copy-refused-2"),
      "tracker-first",
      ...JOIN.map((arg) => (arg === "37-39" ? "35-37" : arg))
    );
    const refusedItems = await cli(notItems.args);
    expect(refusedItems.code).toBe(2);
    expect(refusedItems.stderr).toContain("line 35 of SKILL.md is no list item");
    const both = controlArgs(
      path.join(scratch, "control-copy-refused-3"),
      "tracker-first",
      ...JOIN,
      "--remove-section",
      "## Which skill to read"
    );
    const refusedBoth = await cli(both.args);
    expect(refusedBoth.code).toBe(2);
    expect(refusedBoth.stderr).toContain("give one of --remove-section and --join-lines");
    expect(requestsSince(from)).toHaveLength(0);
  }, 60_000);

  // Catches: a --skill that is a link to a directory copied as the link, so that the change is
  // written through it into the skill itself.
  test("control craft copies the directory a linked --skill names, and leaves that skill's SKILL.md as it was", async () => {
    const c = controlArgs(path.join(scratch, "control-link"), "tidy-settled-change", ...REMOVE);
    const link = path.join(scratch, "control-link", "linked", "tidy-settled-change");
    mkdirSync(path.dirname(link), { recursive: true });
    symlinkSync(c.skill, link, "dir");
    answer = controlStandIn({ copy: [], original: [], originalMarker: STEP_4 });
    await runControl({ out: c.out, args: c.args.map((arg) => (arg === c.skill ? link : arg)) });
    const text = CONTROL_SKILLS["tidy-settled-change/SKILL.md"] ?? "";
    expect(readFileSync(path.join(c.skill, "SKILL.md"), "utf8")).toBe(text);
    const copyDir = path.join(c.out, "copy", "tidy-settled-change");
    expect(lstatSync(copyDir).isSymbolicLink()).toBe(false);
    const lines = text.split("\n");
    expect(readFileSync(path.join(copyDir, "SKILL.md"), "utf8")).toBe(
      [...lines.slice(0, 40), ...lines.slice(46)].join("\n")
    );
  }, 30_000);

  // Catches: a line of a fenced block taken for the heading: the section refused as given twice,
  // or, where only the block holds the heading's text, the lines after it removed.
  test("--remove-section takes no line of a fenced block for the heading", async () => {
    /** Control (1)'s arguments with `heading` removed, on tidy-settled-change with a code example
     *  after Step 5 that shows `heading`, and that SKILL.md's text. */
    const fenced = (dir: string, heading: string) => {
      const c = controlArgs(
        path.join(scratch, dir),
        "tidy-settled-change",
        ...REMOVE.map((arg) => (arg === STEP_4 ? heading : arg))
      );
      const text = `${CONTROL_SKILLS["tidy-settled-change/SKILL.md"]}\n\`\`\`markdown\n${heading}\n\`\`\`\n`;
      writeFileSync(path.join(c.skill, "SKILL.md"), text);
      return { ...c, text };
    };
    const both = fenced("control-fence-both", STEP_4);
    answer = controlStandIn({ copy: [], original: [], originalMarker: STEP_4 });
    await runControl(both);
    const lines = both.text.split("\n");
    expect(
      readFileSync(path.join(both.out, "copy", "tidy-settled-change", "SKILL.md"), "utf8")
    ).toBe([...lines.slice(0, 40), ...lines.slice(46)].join("\n"));
    // A heading only a code example shows is no heading of the skill: refused before any call.
    const from = recorder.requests.length;
    const only = fenced("control-fence-only", "## Step 9: Ship");
    const refused = await cli(only.args);
    expect(refused.code).toBe(2);
    expect(refused.stderr).toContain("no line of SKILL.md is the heading ## Step 9: Ship");
    expect(requestsSince(from)).toHaveLength(0);
  }, 60_000);
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

  /** Every replay reads beta first, then acts once beta's text is in the request. */
  const readsBetaFirst: typeof answer = (body) =>
    isJudgement(body) || JSON.stringify(body.messages).includes("Load alpha before")
      ? behaviourStandIn(body)
      : readReply("skill://beta");

  const expectBetaCopyRead = (run: NonNullable<CaseResult["stages"]["screen"]>["runs"][number]) => {
    expect(run.skillReads).toEqual([
      [{ skill: "beta", file: "SKILL.md", by: "skill", from: "copy", isError: false }],
    ]);
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
    answer = readsBetaFirst;
    const ran = await cli(behaviour("screen", s));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const c = result(s.out, "c04");
    expect(passes(c, "screen")).toEqual({ old: SCREEN_RUNS, new: 0, none: 0 });
    const runs = c.stages.screen?.runs ?? [];
    expect(runs).toHaveLength(3 * SCREEN_RUNS);
    for (const run of runs) {
      expect(run.skillRounds).toBe(1);
      expectBetaCopyRead(run);
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
    answer = readsBetaFirst;
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
      expectBetaCopyRead(run);
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
      `${READS_PAST_LAST_ROUND}: the runs whose reply after the last of 3 answered rounds of skill reads, or of 5 rounds of a written case's read-only calls answered from its checkout, still only read, which get no verdict and are run again, by stage and arm: screen ${pastByArm(runs)}; confirm old 0, new 0; control old 0, minus 0, twin 0`
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
    const reason = `${READS_PAST_LAST_ROUND}: the none arm has 0 of its ${SCREEN_RUNS} judged runs after ${all} runs, ${all} of which still only read after the last answered round (skill files, or read-only calls a written case's checkout answers) (runs that read past the last round: old 0, new 0, none ${all})`;
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

  /** A made-up repository acme/proj holding `files` and the symbolic links `links` (each path to its
   *  target) in its last commit, after a commit of `earlier` where given (each path to its text), as
   *  `--repos <dir>/acme/proj.git` gives it to a written case's fetch, whose server, as GitHub's,
   *  takes a filter of the objects it sends; beside it the dotfiles at DOTFILES' commit (SHIM, the
   *  experiments extension and `dotfiles`, each path to its text), as `--repos` gives them to the
   *  runner's reads; that `--repos` directory and the last commit. */
  const gitRepository = (
    dir: string,
    files: Record<string, string>,
    {
      links = {},
      earlier,
      dotfiles = {},
    }: {
      links?: Record<string, string>;
      earlier?: Record<string, string>;
      dotfiles?: Record<string, string>;
    } = {}
  ): { repos: string; commit: string } => {
    const repos = path.join(dir, "repos");
    const work = path.join(dir, "proj-work");
    const git = (cwd: string, ...args: string[]) => {
      const ran = Bun.spawnSync(["git", ...args], {
        cwd,
        env: {
          PATH: process.env.PATH ?? "/usr/bin:/bin",
          HOME: work,
          GIT_AUTHOR_NAME: "Acme",
          GIT_AUTHOR_EMAIL: "dev@acme.test",
          GIT_COMMITTER_NAME: "Acme",
          GIT_COMMITTER_EMAIL: "dev@acme.test",
          GIT_AUTHOR_DATE: "2026-11-20T00:00:00Z",
          GIT_COMMITTER_DATE: "2026-11-20T00:00:00Z",
        },
      });
      expect({ args, code: ran.exitCode, err: ran.stderr.toString() }).toMatchObject({ code: 0 });
      return ran.stdout.toString().trim();
    };
    mkdirSync(work, { recursive: true });
    git(work, "init", "--quiet");
    if (earlier !== undefined) {
      writeSkill(work, earlier);
      git(work, "add", ".");
      git(work, "commit", "--quiet", "-m", "Write the first guide");
    }
    writeSkill(work, files);
    for (const [link, target] of Object.entries(links)) symlinkSync(target, path.join(work, link));
    git(work, "add", ".");
    git(work, "commit", "--quiet", "-m", "Add the guide");
    const commit = git(work, "rev-parse", "HEAD");
    const bare = path.join(repos, "acme", "proj.git");
    git(dir, "clone", "--quiet", "--bare", work, bare);
    git(bare, "config", "uploadpack.allowFilter", "true");
    writeSkill(path.join(repos, DOTFILES.repo, DOTFILES.commit), {
      "shims/omp": SHIM,
      ...dotfiles,
    });
    cpSync(
      EXPERIMENTS,
      path.join(repos, DOTFILES.repo, DOTFILES.commit, "omp", "extensions", "experiments"),
      { recursive: true }
    );
    return { repos, commit };
  };

  // Catches: a written case run with no copy of its checkout, so its agent is judged on its first
  // look at the work; a git command answered from no history; a session case answered from a
  // checkout it does not have; a checkout made again for each case or stage of one commit (a large
  // repository's checkout takes minutes and gigabytes); a checkout fetched with no blobs, whose
  // git command of an older commit then fetches one from the remote as it answers; or a checkout
  // whose `git remote -v` shows the runner's `--repos` directory (the second security review).
  pinned.test(
    "written cases of one commit answer a reply that only reads from one shared checkout of their repository at that commit, made once and kept, git history among it with the blobs of its commits, its origin the repository's GitHub URL, and record the round; a session case's runs are judged on that reply",
    async () => {
      const base = scenario("checkout-rounds", LOSS_SKILL(), [{ id: "c04" }]);
      // The made-up repository acme/proj, two commits, as `--repos` gives it to the fetch.
      const guide = "# Guide\n\nRun the app.\n";
      const first = "# Guide\n\nRun the old app.\n";
      const { repos, commit } = gitRepository(
        base.dir,
        { "docs/guide.md": guide },
        { earlier: { "docs/guide.md": first } }
      );
      const checkout = {
        repo: "acme/proj",
        commit,
        root: "/work/acme/proj",
        cwd: "/work/acme/proj",
        bashGate: BASH_GATE,
      };
      const written = ["w00", "w01"].map((id): Case => ({ ...makeWrittenCase(id), checkout }));
      const cases = [...base.cases, ...written];
      writeFileSync(base.casesFile, `${cases.map((c) => JSON.stringify(c)).join("\n")}\n`);
      const markers = new Map([...base.markers, ["w00", ""] as const, ["w01", ""] as const]);
      const s = { ...base, cases, markers };
      recordCaptures(s, ["old", "new", "none"]);
      // Each run reads docs/guide.md and the log first, and acts once their answers are in its body.
      const answered: string[] = [];
      let calls = 0;
      answer = (body) => {
        if (isJudgement(body)) return behaviourStandIn(body);
        const last = (body.messages as { content: unknown }[]).at(-1)?.content;
        const results = Array.isArray(last)
          ? last.filter((block) => String(block?.tool_use_id).startsWith("toolu_look_"))
          : [];
        if (results.length > 0) {
          answered.push(results.map((block) => block.content[0].text).join("\n---\n"));
          return behaviourStandIn(body);
        }
        calls += 1;
        return sseResponse(
          messageSse({
            id: `msg_look_${calls}`,
            model: "claude-sonnet-5-5",
            content: [
              {
                type: "tool_use",
                id: `toolu_look_${calls}_read`,
                name: "read",
                input: { path: "docs/guide.md" },
              },
              {
                type: "tool_use",
                id: `toolu_look_${calls}_log`,
                name: "bash",
                input: { command: "git log --format=%s && git status --short" },
              },
            ],
            stopReason: "tool_use",
          })
        );
      };
      const ran = await cli(behaviour("screen", s, "--repos", repos));
      expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
      const runsOf = (id: string) => result(s.out, id).stages.screen?.runs ?? [];
      for (const id of ["w00", "w01"]) {
        expect(runsOf(id)).toHaveLength(3 * SCREEN_RUNS);
        expect(result(s.out, id).stages.screen?.checkout).toEqual({
          git: true,
          depth: 50,
          removedLinks: [],
          shell: STAGE_SHELL,
        });
        for (const run of runsOf(id)) {
          expect({ rounds: run.checkoutRounds, unanswered: run.unanswered }).toEqual({
            rounds: 1,
            unanswered: undefined,
          });
          expect(
            run.skillReads?.[0]?.map((read) => [read.from, "tool" in read ? read.tool : ""])
          ).toEqual([
            ["checkout", "read"],
            ["checkout", "bash"],
          ]);
        }
      }
      // A run of a second or more says its wall time, as Oh My Pi's bash does.
      const expected = `[docs/guide.md#${snapshotTag(guide)}]\n1:# Guide\n2:\n3:Run the app.\n---\nAdd the guide\nWrite the first guide\n`;
      expect(answered.length).toBeGreaterThan(0);
      for (const text of answered) {
        expect(text.replace(/\n\nWall time: \d+\.\d\d seconds/g, "")).toBe(expected);
      }
      for (const run of runsOf("c04")) {
        expect(run.checkoutRounds).toBe(0);
        expect(run.reply).toMatchObject([
          { type: "tool_use", name: "read", input: { path: "docs/guide.md" } },
          { type: "tool_use", name: "bash" },
        ]);
      }
      // One checkout of the commit, made once, said once, and kept in the cache with its marker.
      const made = ran.stdout
        .split("\n")
        .filter((line) => line.includes(`the checkout of acme/proj at ${commit} is made in `));
      expect(made).toHaveLength(1);
      expect(made[0]).toContain("(git, its history 50 commits deep; no symbolic link leaves it)");
      const checkouts = path.join(s.dir, "cache", "checkouts", "acme", "proj");
      expect(readdirSync(checkouts).sort()).toEqual([commit, `${commit}.ready.json`]);
      expect(readFileSync(path.join(checkouts, commit, "docs", "guide.md"), "utf8")).toBe(guide);
      // It holds the blobs of its commits, the earlier one's among them, with no fetch to come.
      expect(
        JSON.parse(readFileSync(path.join(checkouts, `${commit}.ready.json`), "utf8"))
      ).toMatchObject({ git: true, depth: 50, blobs: true });
      const shown = Bun.spawnSync(["git", "cat-file", "-p", "HEAD~1:docs/guide.md"], {
        cwd: path.join(checkouts, commit),
        env: { PATH: process.env.PATH ?? "/usr/bin:/bin", HOME: base.dir, GIT_NO_LAZY_FETCH: "1" },
      });
      expect({ code: shown.exitCode, text: shown.stdout.toString() }).toEqual({
        code: 0,
        text: first,
      });
      // Its origin is the repository's GitHub URL, as a session's checkout names it, not the
      // `--repos` directory on the runner that its fetch read.
      const remote = Bun.spawnSync(["git", "remote", "-v"], {
        cwd: path.join(checkouts, commit),
        env: { PATH: process.env.PATH ?? "/usr/bin:/bin", HOME: base.dir },
      });
      expect({ code: remote.exitCode, text: remote.stdout.toString() }).toEqual({
        code: 0,
        text: "origin\thttps://github.com/acme/proj.git (fetch)\norigin\thttps://github.com/acme/proj.git (push)\n",
      });
    },
    120_000
  );

  // Catches: a jj command of a written case whose context session ran jj answered with an error
  // that session never saw (the copy no jj repository; the defect: the screen looked for a jj call
  // in the case's own history, which holds only the task and the skill's read), one of a session
  // that ran none answered as in a jj workspace, a checkout made again to add jj, an answer that
  // names the cache's copy in place of the session's paths, or a jj answer that takes the working
  // copy's lock, which answers at once race on.
  pinned.test(
    "the shared checkout is made a colocated jj repository once a written case whose checkout records that its context session ran jj needs it, recorded in its marker and that case's stage; jj, git and pwd answer at the session's paths, and no answer names the copy",
    async () => {
      const base = scenario("checkout-jj", LOSS_SKILL(), [{ id: "c04" }]);
      const guide = "# Guide\n\nRun the app.\n";
      const { repos, commit } = gitRepository(base.dir, { "docs/guide.md": guide });
      const root = "/work/acme/proj";
      const checkout = { repo: "acme/proj", commit, root, cwd: root, bashGate: BASH_GATE };
      // w00's context session ran no jj; w01's did, as the written build records it on the case.
      const written = [
        { ...makeWrittenCase("w00"), checkout: { ...checkout, jj: false } },
        { ...makeWrittenCase("w01"), checkout: { ...checkout, jj: true } },
      ];
      const cases = [...base.cases, ...written];
      writeFileSync(base.casesFile, `${cases.map((c) => JSON.stringify(c)).join("\n")}\n`);
      const markers = new Map([...base.markers, ["w00", ""] as const, ["w01", ""] as const]);
      const s = { ...base, cases, markers };
      recordCaptures(s, ["old", "new", "none"]);
      // Each run looks where it is as the dev box's sessions do, then acts.
      const where = `cd ${root} && unset GIT_CONFIG_GLOBAL GIT_CONFIG_NOSYSTEM ACME_TOOLS_PIN && pwd && (jj root 2>/dev/null || git rev-parse --show-toplevel) && git worktree list`;
      const answered: { id: string; texts: string[]; errors: boolean[] }[] = [];
      let calls = 0;
      answer = (body) => {
        if (isJudgement(body)) return behaviourStandIn(body);
        const last = (body.messages as { content: unknown }[]).at(-1)?.content;
        const results = Array.isArray(last)
          ? last.filter((block) => String(block?.tool_use_id).startsWith("toolu_where_"))
          : [];
        if (results.length > 0) {
          answered.push({
            id: JSON.stringify(body).match(/Case (w0\d)/)?.[1] ?? "",
            texts: results.map((block) =>
              block.content[0].text.replace(/\n\nWall time: \d+\.\d\d seconds/g, "")
            ),
            errors: results.map((block) => block.is_error === true),
          });
          return behaviourStandIn(body);
        }
        calls += 1;
        const call = (n: number, command: string) => ({
          type: "tool_use" as const,
          id: `toolu_where_${calls}_${n}`,
          name: "bash",
          input: { command },
        });
        return sseResponse(
          messageSse({
            id: `msg_where_${calls}`,
            model: "claude-sonnet-5-5",
            content: [call(1, where), call(2, "jj st")],
            stopReason: "tool_use",
          })
        );
      };
      const cache = path.join(s.dir, "cache");
      const ran = await cli(behaviour("screen", s, "--repos", repos));
      expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
      const checkouts = path.join(cache, "checkouts", "acme", "proj");
      // Made once by w00, with no jj; made a colocated jj repository once, for w01.
      const said = ran.stdout
        .split("\n")
        .filter((line) => line.includes("the checkout of acme/proj"));
      expect(said).toEqual([
        expect.stringContaining("(git, its history 50 commits deep; no symbolic link leaves it)"),
        expect.stringContaining(
          `the checkout of acme/proj at ${commit} is made a colocated jj repository in `
        ),
      ]);
      expect(readdirSync(checkouts).sort()).toEqual([commit, `${commit}.ready.json`]);
      expect(
        JSON.parse(readFileSync(path.join(checkouts, `${commit}.ready.json`), "utf8"))
      ).toMatchObject({ git: true, jj: true });
      const plain = { git: true, depth: 50, removedLinks: [], shell: STAGE_SHELL };
      expect(result(s.out, "w00").stages.screen?.checkout).toEqual(plain);
      expect(result(s.out, "w01").stages.screen?.checkout).toEqual({ ...plain, jj: true });
      const shape = new RegExp(
        `^${root}\\n${root}\\n${root} +[0-9a-f]{7,} \\(detached HEAD\\)\\n$`
      );
      for (const id of ["w00", "w01"]) {
        const mine = answered.filter((each) => each.id === id);
        expect(mine.length).toBeGreaterThan(0);
        for (const { texts, errors } of mine) {
          expect(texts[0]).toMatch(shape);
          if (id === "w00") {
            expect({ text: texts[1]?.split("\n")[0], error: errors[1] }).toEqual({
              text: 'Error: There is no jj repo in "."',
              error: true,
            });
          } else {
            expect(texts[1]).toContain("The working copy has no changes.");
            expect(texts[1]).toContain("Add the guide");
          }
        }
      }
      expect(JSON.stringify(answered)).not.toContain(cache);
      for (const id of ["w00", "w01"]) {
        for (const run of result(s.out, id).stages.screen?.runs ?? []) {
          expect({ rounds: run.checkoutRounds, unanswered: run.unanswered }).toEqual({
            rounds: 1,
            unanswered: undefined,
          });
        }
      }
    },
    180_000
  );

  // Catches: a model-written call of a written case's run that reads a file outside its checkout
  // copy (a key file of the pod) through a symbolic link the repository holds, so its text reaches
  // the model and the run records posted on Dispatch; or a half-made checkout, left by a run that
  // was killed, used as it is.
  pinned.test(
    "a written case's copy holds no symbolic link that resolves outside it: cat and grep -R after a leading ls, which the bash gate's arm on lets run, and read of the repository's link to a secret outside never show it, and the stage records the link removed; a half-made checkout of a dead run is made again",
    async () => {
      const base = scenario("checkout-links", LOSS_SKILL(), [{ id: "c04" }]);
      const secret = fixtureSecret("podkey");
      const outside = path.join(base.dir, "outside-key.txt");
      writeFileSync(outside, `${secret}\n`);
      const { repos, commit } = gitRepository(
        base.dir,
        { "docs/guide.md": "# Guide\n\nRun the app.\n" },
        { links: { "leak.txt": outside } }
      );
      const written: Case = {
        ...makeWrittenCase("w00"),
        checkout: {
          repo: "acme/proj",
          commit,
          root: "/work/acme/proj",
          cwd: "/work/acme/proj",
          bashGate: BASH_GATE,
        },
      };
      const cases = [...base.cases, written];
      writeFileSync(base.casesFile, `${cases.map((c) => JSON.stringify(c)).join("\n")}\n`);
      const s = { ...base, cases, markers: new Map([...base.markers, ["w00", ""] as const]) };
      recordCaptures(s, ["old", "new", "none"]);
      // A run that died making the checkout: its lock names a process that runs no more, and its
      // half-made checkout holds a file the commit does not.
      const checkouts = path.join(s.dir, "cache", "checkouts", "acme", "proj");
      mkdirSync(path.join(checkouts, commit), { recursive: true });
      writeFileSync(path.join(checkouts, commit, "half.txt"), "half-made\n");
      const dead = Bun.spawnSync(["true"]).pid;
      writeFileSync(path.join(checkouts, `${commit}.lock`), `${dead}\n`);
      const answered: string[] = [];
      let calls = 0;
      answer = (body) => {
        if (isJudgement(body)) return behaviourStandIn(body);
        const last = (body.messages as { content: unknown }[]).at(-1)?.content;
        const results = Array.isArray(last)
          ? last.filter((block) => String(block?.tool_use_id).startsWith("toolu_leak_"))
          : [];
        if (results.length > 0) {
          answered.push(...results.map((block) => block.content[0].text));
          return behaviourStandIn(body);
        }
        calls += 1;
        const call = (n: number, name: string, input: Record<string, unknown>) => ({
          type: "tool_use" as const,
          id: `toolu_leak_${calls}_${n}`,
          name,
          input,
        });
        return sseResponse(
          messageSse({
            id: `msg_leak_${calls}`,
            model: "claude-sonnet-5-5",
            content: [
              call(1, "read", { path: "leak.txt" }),
              call(2, "bash", { command: "ls; cat leak.txt" }),
              call(3, "bash", { command: `ls; grep -R ${secret.slice(0, 12)} .` }),
            ],
            stopReason: "tool_use",
          })
        );
      };
      const ran = await cli(behaviour("screen", s, "--repos", repos));
      expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
      expect(ran.stdout).toContain(
        "(git, its history 50 commits deep; 1 symbolic links that leave it removed: leak.txt)"
      );
      expect(ran.stdout).toContain(
        `the checkout ${path.join(checkouts, commit)} is half-made by process ${dead}, which holds it no more: it is made again`
      );
      expect(readdirSync(checkouts).sort()).toEqual([commit, `${commit}.ready.json`]);
      expect(existsSync(path.join(checkouts, commit, "half.txt"))).toBe(false);
      const stage = result(s.out, "w00").stages.screen;
      expect(stage?.checkout).toEqual({
        git: true,
        depth: 50,
        removedLinks: ["leak.txt"],
        shell: STAGE_SHELL,
      });
      for (const run of stage?.runs ?? []) {
        expect({ rounds: run.checkoutRounds, unanswered: run.unanswered }).toEqual({
          rounds: 1,
          unanswered: undefined,
        });
      }
      expect(answered.length).toBeGreaterThan(0);
      expect(
        new Set(answered.map((text) => text.replace(/\n\nWall time: \d+\.\d\d seconds/g, "")))
      ).toEqual(
        new Set([
          "Path 'leak.txt' not found",
          "docs\ncat: leak.txt: No such file or directory\n\n\nCommand exited with code 1",
          "docs\n\n\nCommand exited with code 1",
        ])
      );
      expect(answered.join("\n")).not.toContain(secret);
      expect(readFileSync(caseResultFile(s.out, "w00"), "utf8")).not.toContain(secret);
    },
    120_000
  );

  // Catches: a written case whose profile names no commit of the dotfiles repository, so the
  // runner knows no shell prefix its context session ran with, answered in a shell of the runner's
  // guess (the pin's default, no prefix); or such a refusal that does not say why, or a stage that
  // does not record it.
  test("a written case whose profile names no commit of the configuration's dotfiles repository answers none of its bash calls: its runs get no verdict as unanswered-read, saying so, and its stage records why", async () => {
    const base = scenario("checkout-no-dotfiles", LOSS_SKILL(), [{ id: "c04" }]);
    const { repos, commit } = gitRepository(base.dir, { "docs/guide.md": "# Guide\n" });
    const written: Case = {
      ...makeWrittenCase("w00"),
      checkout: { repo: "acme/proj", commit, root: "/work/acme/proj", cwd: "/work/acme/proj" },
    };
    const cases = [...base.cases, written];
    writeFileSync(base.casesFile, `${cases.map((c) => JSON.stringify(c)).join("\n")}\n`);
    const s = { ...base, cases, markers: new Map([...base.markers, ["w00", ""] as const]) };
    recordCaptures(s, ["old", "new", "none"]);
    // w00's first two runs list the checkout; each later one acts.
    let looks = 0;
    answer = (body) => {
      if (isJudgement(body) || !JSON.stringify(body.messages).includes("Case w00")) {
        return behaviourStandIn(body);
      }
      const last = (body.messages as { content: unknown }[]).at(-1)?.content;
      if (Array.isArray(last) && last.some((block) => block?.type === "tool_result")) {
        return behaviourStandIn(body);
      }
      looks += 1;
      if (looks > 2) return behaviourStandIn(body);
      return sseResponse(
        messageSse({
          id: `msg_ls_${looks}`,
          model: "claude-sonnet-5-5",
          content: [
            { type: "tool_use", id: `toolu_ls_${looks}`, name: "bash", input: { command: "ls" } },
          ],
          stopReason: "tool_use",
        })
      );
    };
    // The configuration names a dotfiles repository the case's profile names no skill of.
    const args = behaviour("screen", s, "--repos", repos);
    const config = path.join(s.dir, "skill-cuts-other-dotfiles.json");
    const given = JSON.parse(readFileSync(args[args.indexOf("--config") + 1] ?? "", "utf8"));
    writeFileSync(config, JSON.stringify({ ...given, dotfilesRepo: "acme/dotfiles" }));
    args[args.indexOf("--config") + 1] = config;
    const ran = await cli(args);
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const why =
      "the case's profile names no commit of acme/dotfiles (no skill of it), whose shims/omp gives the PI_SHELL_PREFIX the session's Oh My Pi ran its bash calls with";
    const stage = result(s.out, "w00").stages.screen;
    expect(stage?.checkout?.shell).toEqual({ why });
    const runs = stage?.runs ?? [];
    expect(runs).toHaveLength(3 * SCREEN_RUNS + 2);
    expect(runs.filter((run) => run.verdict === null).map((run) => run.reason)).toEqual([
      `${UNANSWERED_READ}: bash {"command":"ls"}: ${why}`,
      `${UNANSWERED_READ}: bash {"command":"ls"}: ${why}`,
    ]);
  }, 120_000);

  // Catches: a written case whose checkout's repository knives trusts answered without the blocks
  // knives would add to the first result that touches it, where the runner has none of them (its
  // context session never touched it); or one refused where that session touched it and knives
  // gave it none, so a run gets none either.
  test("a written case whose checkout's repository the dotfiles' knives registry trusts: where its context session touched the repository and got no block, a run's read of it is answered with none; where that session never touched it, the read gets no verdict as unanswered-read, saying why", async () => {
    const base = scenario("checkout-knives", LOSS_SKILL(), [{ id: "c04" }]);
    const { repos, commit } = gitRepository(
      base.dir,
      { "docs/guide.md": "# Guide\n" },
      { dotfiles: { "knives/repos.toml": '[trust]\nowners = ["acme"]\n' } }
    );
    const checkout = { repo: "acme/proj", commit, root: "/work/acme/proj", cwd: "/work/acme/proj" };
    const written = (id: string, touched: boolean): Case => ({
      ...makeWrittenCase(id),
      checkout: { ...checkout, knives: { blocks: null, touched } },
    });
    const cases = [...base.cases, written("w00", true), written("w01", false)];
    writeFileSync(base.casesFile, `${cases.map((c) => JSON.stringify(c)).join("\n")}\n`);
    const s = {
      ...base,
      cases,
      markers: new Map([...base.markers, ["w00", ""] as const, ["w01", ""] as const]),
    };
    recordCaptures(s, ["old", "new", "none"]);
    // Each written case's first two runs read the guide; each later one acts.
    const looks = new Map<string, number>();
    answer = (body) => {
      const sent = JSON.stringify(body.messages);
      const id = ["w00", "w01"].find((each) => sent.includes(`Case ${each}`));
      if (isJudgement(body) || id === undefined) return behaviourStandIn(body);
      const last = (body.messages as { content: unknown }[]).at(-1)?.content;
      if (Array.isArray(last) && last.some((block) => block?.type === "tool_result")) {
        return behaviourStandIn(body);
      }
      const n = (looks.get(id) ?? 0) + 1;
      looks.set(id, n);
      if (n > 2) return behaviourStandIn(body);
      return sseResponse(
        messageSse({
          id: `msg_guide_${id}_${n}`,
          model: "claude-sonnet-5-5",
          content: [
            {
              type: "tool_use",
              id: `toolu_guide_${n}`,
              name: "read",
              input: { path: "docs/guide.md" },
            },
          ],
          stopReason: "tool_use",
        })
      );
    };
    const ran = await cli(behaviour("screen", s, "--repos", repos));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const touched = result(s.out, "w00").stages.screen?.runs ?? [];
    expect(touched.filter((run) => run.verdict === null)).toEqual([]);
    expect(touched.filter((run) => run.checkoutRounds === 1)).toHaveLength(2);
    const why = `the call touches acme/proj: knives trusts it (knives/repos.toml at ${DOTFILES.repo}@${DOTFILES.commit}, its [trust]), and the case's context session never touched it, so the runner has none of the blocks knives adds to the first result of a session that touches it`;
    const untouched = result(s.out, "w01").stages.screen?.runs ?? [];
    expect(untouched).toHaveLength(3 * SCREEN_RUNS + 2);
    expect(untouched.filter((run) => run.verdict === null).map((run) => run.reason)).toEqual([
      `${UNANSWERED_READ}: read {"path":"docs/guide.md"}: ${why}`,
      `${UNANSWERED_READ}: read {"path":"docs/guide.md"}: ${why}`,
    ]);
  }, 120_000);

  // Catches: a written case judged on a reply one of whose read-only calls its checkout answers in
  // no form (a step every arm takes alike, whatever the skill says), a run so made again with no
  // word of why, and a report that does not count those runs, or the reads a run saved before the
  // runner rebuilt the pin's summaries marks `wholeText`, by kind of case; or a run that marks one.
  natives.test(
    "a written case's run whose reply makes a read-only call the checkout answers in no form gets no verdict and runs again; the report counts those runs and the saved whole-text answers by kind of case",
    async () => {
      const base = scenario("checkout-unanswered", LOSS_SKILL(), [{ id: "c04" }]);
      const big = Array.from({ length: 120 }, (_, n) => `x${n + 1} = ${n + 1}\n`).join("");
      const { repos, commit } = gitRepository(base.dir, { "src/big.py": big });
      const written: Case = {
        ...makeWrittenCase("w00"),
        checkout: { repo: "acme/proj", commit, root: "/work/acme/proj", cwd: "/work/acme/proj" },
      };
      const cases = [...base.cases, written];
      writeFileSync(base.casesFile, `${cases.map((c) => JSON.stringify(c)).join("\n")}\n`);
      const s = { ...base, cases, markers: new Map([...base.markers, ["w00", ""] as const]) };
      recordCaptures(s, ["old", "new", "none"]);
      // w00's first two runs read /etc/hostname, outside the checkout, which the copy answers in no
      // form; each later one reads src/big.py, a code file of 120 lines whose parse elides nothing,
      // answered whole with no mark; and acts once that answer is in its body.
      let looks = 0;
      answer = (body) => {
        if (isJudgement(body) || !JSON.stringify(body.messages).includes("Case w00")) {
          return behaviourStandIn(body);
        }
        const last = (body.messages as { content: unknown }[]).at(-1)?.content;
        const answered = (block: { tool_use_id?: unknown }) =>
          String(block?.tool_use_id).startsWith("toolu_odd_");
        if (Array.isArray(last) && last.some(answered)) {
          return behaviourStandIn(body);
        }
        looks += 1;
        return sseResponse(
          messageSse({
            id: `msg_odd_${looks}`,
            model: "claude-sonnet-5-5",
            content: [
              {
                type: "tool_use",
                id: `toolu_odd_${looks}`,
                name: "read",
                input: { path: looks <= 2 ? "/etc/hostname" : "src/big.py" },
              },
            ],
            stopReason: "tool_use",
          })
        );
      };
      const ran = await cli(behaviour("screen", s, "--repos", repos));
      expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
      const runs = result(s.out, "w00").stages.screen?.runs ?? [];
      expect(runs).toHaveLength(3 * SCREEN_RUNS + 2);
      const unjudged = runs.filter((run) => run.verdict === null);
      expect(unjudged.map((run) => run.reason)).toEqual([
        expect.stringMatching(/^unanswered-read: read \{"path":"\/etc\/hostname"\}: /),
        expect.stringMatching(/^unanswered-read: read \{"path":"\/etc\/hostname"\}: /),
      ]);
      expect(unjudged.map((run) => run.reply)).toEqual([
        [expect.objectContaining({ name: "read", input: { path: "/etc/hostname" } })],
        [expect.objectContaining({ name: "read", input: { path: "/etc/hostname" } })],
      ]);
      for (const run of runs.filter((each) => each.verdict !== null)) {
        expect(run.skillReads).toEqual([
          [{ from: "checkout", tool: "read", input: { path: "src/big.py" }, isError: false }],
        ]);
      }
      expect(ran.stdout).toContain("(2 runs with no verdict, run again; unanswered-read: ");
      const counted = (saved: number) =>
        `runs with no verdict, run again, and saved whole-text reads, by kind of case: session: 0 ${READS_PAST_LAST_ROUND}, 0 unanswered-read, 0 saved reads marked wholeText (answered whole before the runner rebuilt the pin's summaries); written: 0 ${READS_PAST_LAST_ROUND}, 2 unanswered-read, ${saved} saved reads marked wholeText (answered whole before the runner rebuilt the pin's summaries)`;
      const reported = await cli(report(s));
      expect({ code: reported.code, stderr: reported.stderr }).toEqual({ code: 0, stderr: "" });
      expect(reported.stdout.split("\n")).toContain(counted(0));
      // A run saved before the runner rebuilt the pin's summaries marks such a read `wholeText`.
      const file = caseResultFile(s.out, "w00");
      const saved = CaseResult.parse(JSON.parse(readFileSync(file, "utf8")));
      const marked = {
        ...saved,
        stages: {
          ...saved.stages,
          screen: saved.stages.screen && {
            ...saved.stages.screen,
            runs: saved.stages.screen.runs.map((run) => ({
              ...run,
              skillReads: run.skillReads?.map((round) =>
                round.map((read) =>
                  read.from === "checkout" ? { ...read, wholeText: true } : read
                )
              ),
            })),
          },
        },
      };
      writeFileSync(file, JSON.stringify(marked));
      const again = await cli(report(s));
      expect({ code: again.code, stderr: again.stderr }).toEqual({ code: 0, stderr: "" });
      expect(again.stdout.split("\n")).toContain(counted(3 * SCREEN_RUNS));
    },
    120_000
  );

  // Catches: a written case built before build-cases passed over a context session in no checkout
  // it knows, so it has no checkout, judged on a reply whose calls all read, which the runner
  // answers in no form (the defect: such cases floored at 0 of 10 in both arms, each run judged
  // fail on its reads of files no checkout holds); or a session case's reply of such calls, which no
  // checkout answers by design, no longer judged.
  test("a written case with no checkout whose reply reads a file gets no verdict as unanswered-read, saying the case has no checkout, and runs again; a session case's run is judged on that reply", async () => {
    const s = withWritten(scenario("no-checkout", LOSS_SKILL(), [{ id: "c04" }]), ["w00"]);
    recordCaptures(s, ["old", "new", "none"]);
    // w00's first two runs, and every run of c04, read docs/guide.md; w00's later runs act.
    const looks: Record<"c04" | "w00", number> = { c04: 0, w00: 0 };
    answer = (body) => {
      if (isJudgement(body)) return behaviourStandIn(body);
      const id = JSON.stringify(body.messages).includes("Case w00") ? "w00" : "c04";
      looks[id] += 1;
      return id === "w00" && looks.w00 > 2 ? behaviourStandIn(body) : readReply("docs/guide.md");
    };
    const ran = await cli(behaviour("screen", s));
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const written = result(s.out, "w00").stages.screen?.runs ?? [];
    expect(written).toHaveLength(3 * SCREEN_RUNS + 2);
    const unjudged = written.filter((run) => run.verdict === null);
    expect(unjudged).toHaveLength(2);
    for (const run of unjudged) {
      expect(run.reason).toStartWith(
        `${UNANSWERED_READ}: read {"path":"docs/guide.md"}: the case has no checkout`
      );
      expect({ rounds: run.checkoutRounds, reply: run.reply }).toEqual({
        rounds: 0,
        reply: [expect.objectContaining({ name: "read", input: { path: "docs/guide.md" } })],
      });
    }
    expect(ran.stdout).toContain(`(2 runs with no verdict, run again; ${UNANSWERED_READ}: `);
    const session = result(s.out, "c04").stages.screen?.runs ?? [];
    expect(session).toHaveLength(3 * SCREEN_RUNS);
    for (const run of session) {
      expect({ verdict: run.verdict, rounds: run.checkoutRounds }).toEqual({
        verdict: "fail",
        rounds: 0,
      });
      expect(run.reply).toEqual([
        expect.objectContaining({ name: "read", input: { path: "docs/guide.md" } }),
      ]);
    }
    const reported = await cli(report(s));
    expect({ code: reported.code, stderr: reported.stderr }).toEqual({ code: 0, stderr: "" });
    expect(reported.stdout.split("\n")).toContain(
      `runs with no verdict, run again, and saved whole-text reads, by kind of case: session: 0 ${READS_PAST_LAST_ROUND}, 0 ${UNANSWERED_READ}, 0 saved reads marked wholeText (answered whole before the runner rebuilt the pin's summaries); written: 0 ${READS_PAST_LAST_ROUND}, 2 ${UNANSWERED_READ}, 0 saved reads marked wholeText (answered whole before the runner rebuilt the pin's summaries)`
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

// Catches: the time a shared checkout's copy was made, which a listing's ages are measured from,
// taken from the time of the call (two runs of a case then differ), or a marker written before it
// recorded that time read with none.
test("readCheckoutMarker gives a marker's recorded madeAt, and for one written before it recorded one, its file's own time of change in whole milliseconds, not the time of the read; no marker is none", () => {
  const root = path.join(scratch, "checkout-markers");
  mkdirSync(root, { recursive: true });
  const marker = { git: true, depth: 1, removedLinks: [], seconds: 2, bytes: 10 };
  const recorded = path.join(root, "recorded.ready.json");
  writeFileSync(recorded, JSON.stringify({ ...marker, madeAt: 1_763_640_000_123 }));
  utimesSync(recorded, 1_000, 1_000);
  expect(readCheckoutMarker(recorded)).toEqual({ ...marker, madeAt: 1_763_640_000_123 });
  const old = path.join(root, "old.ready.json");
  writeFileSync(old, JSON.stringify(marker));
  utimesSync(old, 1_763_640_000.9876, 1_763_640_000.9876);
  expect(readCheckoutMarker(old)).toEqual({ ...marker, madeAt: 1_763_640_000_987 });
  expect(readCheckoutMarker(path.join(root, "none.ready.json"))).toBeUndefined();
});
