import { beforeAll, describe, expect, test } from "bun:test";
import { readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import type { IssueSteps, StepRecord } from "@legion/contracts";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { bearerCredential } from "../../../packages/envoy-client/src/bearer";
import { executeDispatchTool } from "../../../packages/envoy-client/src/dispatch-execute";
import { fakeDispatchFetch } from "../../../packages/envoy-client/test/fake-dispatch";
import { fakeDispatchRead } from "./fake-dispatch-command";
import { heldModels, transcriptResponses } from "./step-models-check";

// step-models-check.sh against the fake `dispatch` of lib/fake-dispatch-command.ts, first on PATH.
// The fake answers `dispatch steps --issue <KEY>` with that issue's records, lines of
// testdata/step-models/printed-steps-16.txt, printed-steps-16-b.txt and printed-steps-16-c.txt, or
// of the whole-chain test's printed-steps-16-captured-A.txt and -B.txt. The daemon's logs, the
// transcripts and the agents files are the other files of testdata/step-models. Its README names
// each record (A0 to A8, C1 to C3, C1b, C3b, D1 to D10, T1 and S1), and which fixtures are made.
const script = join(import.meta.dir, "..", "step-models-check.sh");
const data = join(import.meta.dir, "testdata", "step-models");
const dir = temporaryDirectory("step-models-check-test");

const lines = (name: string) => readFileSync(join(data, name), "utf8").trimEnd().split("\n");
const sixteen = lines("printed-steps-16.txt");
const sixteenB = lines("printed-steps-16-b.txt");
const sixteenC = lines("printed-steps-16-c.txt");
const A1 = sixteen[1];
const A2 = sixteen[2];
const A3 = sixteen[3];
const A4 = sixteen[4];
const A5 = sixteen[5];
const A6 = sixteen[6];
const A7 = sixteen[7];
const A8 = sixteen[8];
const C1 = sixteen[9];
const C2 = sixteen[10];
const C3 = sixteen[11];
const A0 = sixteen[12];
const C1b = sixteenB[1];
const C3b = sixteenB[2];
const [, D1, D2, D3, D4, D5, D6, D7, D8, D9, D10, T1, S1] = sixteenC;

const A = "LEGION-3550";
const B = "LEGION-3551";
const D = "LEGION-3552";
const implementer = "legion-legion-legion-3550-implementer";
const tester = "legion-legion-legion-3550-tester";
const omp = "gateway-openai/openai/gpt-6.1-sol";
const fallback = "gateway/openai/gpt-6.1-sol";
const sonnet = "anthropic/claude-sonnet-4-5";
// The lines that A1 and A6 pass with, in each row that takes them. A taken step's line names its
// harness.
const A1Passes = `PASS: c-real: harness omp: ${A} default 2 step attempt 1: ${omp}=4,${fallback}=2`;
const A6Passes = `PASS: live-at-both-reads: harness omp: ${A} default 2 step attempt 6: ${omp}=2`;

/** What `dispatch steps --issue <issue>` prints of these run lines: a header, then each line. */
function answer(issue: string, runs: string[]): string {
  return `${[`${issue}: ${runs.length} steps in 1 tasks`, ...runs].join("\n")}\n`;
}

// Agents files of one claim each, written beside the run. Each names its transcript by its full
// path, where agents.json names its own by a path relative to its directory.
const agentsFile = (name: string) => join(dir, `agents-${name}.json`);
const agentsJson = join(data, "agents.json");
const daemonLog = join(data, "daemon-log-reads.jsonl");
// The read lines of LEGION-3552's records, D1 to D10, T1 and S1, and the start read of the step
// after D10.
const completionLog = join(data, "daemon-log-reads-completion.jsonl");
// The same log with line 6, A2's end read, cut to its first 200 bytes, as a copy made while the
// daemon writes may be. The cut line still holds the read line's message.
const cutLog = join(dir, "daemon-log-cut.jsonl");
beforeAll(() => {
  const one = (claim: string, harness: string, transcript: string, setModel: string) =>
    JSON.stringify({ [claim]: { harness, transcript: join(data, transcript), setModel } });
  writeFileSync(
    agentsFile("implementer"),
    one(implementer, "omp", "omp-session-fallback.jsonl", omp)
  );
  writeFileSync(
    agentsFile("implementer-set-fallback"),
    one(implementer, "omp", "omp-session-fallback.jsonl", fallback)
  );
  writeFileSync(
    agentsFile("tester-background"),
    one(tester, "claude", "claude-stream-background-turn.jsonl", sonnet)
  );
  writeFileSync(
    agentsFile("tester-refusal"),
    one(tester, "claude", "claude-stream-refusal-turn.jsonl", sonnet)
  );
  writeFileSync(
    agentsFile("d-implementer"),
    one("legion-legion-legion-3552-implementer", "omp", "omp-session-completion.jsonl", omp)
  );
  writeFileSync(
    agentsFile("d-tester"),
    one("legion-legion-legion-3552-tester", "claude", "claude-transcript-completion.jsonl", sonnet)
  );
  writeFileSync(
    agentsFile("d-architect"),
    one("legion-legion-legion-3552-architect", "omp", "omp-session-sign-off.jsonl", omp)
  );
  const log = readFileSync(daemonLog, "utf8").split("\n");
  log[5] = log[5].slice(0, 200);
  writeFileSync(cutLog, log.join("\n"));
});

const readOf = fakeDispatchRead(script, dir, []);
// The run needs bun on its PATH beside the fake, for lib/step-models-check.ts.
const path = `${dir}:${dirname(process.execPath)}:/usr/bin:/bin`;

/** One run on each issue's records, in the order given, with the log and the agents file. */
function check(
  records: Readonly<Record<string, string[]>>,
  agents: string,
  more: { log?: string; args?: string[]; exit?: number; err?: string } = {}
) {
  const issues = Object.keys(records);
  const run = readOf(
    Object.fromEntries(
      issues.map((issue) => [`steps_${issue.replaceAll("-", "_")}`, answer(issue, records[issue])])
    ),
    [
      ...issues.flatMap((issue) => ["--issue", issue]),
      "--daemon-log",
      more.log ?? daemonLog,
      "--agents",
      agents,
      ...(more.args ?? []),
    ],
    more.exit ?? 0,
    path,
    more.err
  );
  return { ...run, verdicts: run.stdout.trimEnd().split("\n").slice(-2) };
}

// One session of Claude Code captured against a stand-in endpoint, its subagent's transcript, and
// the endpoint's own list of each response it served. Claude Code asked for claude-model, and the
// endpoint served claude-served-model each time. The README says which run captured them.
const captured = "claude-session-capture-differ.jsonl";
const capturedSubagent = "claude-session-capture-differ-subagent.jsonl";
const served: Array<{ id: string; subagent: boolean; asked: string; model: string }> = JSON.parse(
  readFileSync(join(data, "capture-differ-served.json"), "utf8")
);
const servedMain = served.filter((response) => !response.subagent);

describe("step-models-check.sh", () => {
  test("an Oh My Pi step that its agent ended equals its transcript", () => {
    const run = check({ [A]: [A1, A0] }, agentsFile("implementer"));
    expect(run.stdout).toContain(A1Passes);
    expect(run.stdout).toContain("live-at-both-reads: UNKNOWN: took no step of harness omp");
    expect(run.stdout).not.toContain("FAIL");
    expect(run.verdicts).toEqual(["c-real: PASS", "live-at-both-reads: UNKNOWN"]);
    expect(run.code).toBe(0);
    expect(run.calls).toBe(`steps --issue ${A}\n`);
  });

  test("a Claude Code step counts each response once by its id", () => {
    const run = check({ [A]: [C1] }, agentsFile("tester-background"));
    expect(run.stdout).toContain(
      `PASS: c-real: harness claude: ${A} default 3 step attempt 1: ${sonnet}=3`
    );
    expect(run.verdicts).toEqual(["c-real: PASS", "live-at-both-reads: UNKNOWN"]);
    expect(run.code).toBe(0);
  });

  test("a synthetic response counts for no model", () => {
    const run = check({ [A]: [C2] }, agentsFile("tester-refusal"));
    expect(run.stdout).toContain(
      `PASS: c-real: harness claude: ${A} default 3 step attempt 2: ${sonnet}=1`
    );
    expect(run.verdicts).toEqual(["c-real: PASS", "live-at-both-reads: UNKNOWN"]);
    expect(run.code).toBe(0);
  });

  test("both comparisons on both harnesses", () => {
    const run = check({ [A]: [A1, A6, C1, C3, A0] }, agentsJson);
    expect(run.stdout).toContain(A1Passes);
    expect(run.stdout).toContain(
      `PASS: c-real: harness claude: ${A} default 3 step attempt 1: ${sonnet}=3`
    );
    expect(run.stdout).toContain(A6Passes);
    expect(run.stdout).toContain(
      `PASS: live-at-both-reads: harness claude: ${A} default 3 step attempt 3: ${sonnet}=1`
    );
    expect(run.stdout).not.toContain("FAIL");
    expect(run.verdicts).toEqual(["c-real: PASS", "live-at-both-reads: PASS"]);
    expect(run.code).toBe(0);
  });

  test("a record whose set model is not the settings' fails", () => {
    const run = check({ [A]: [A1] }, agentsFile("implementer-set-fallback"));
    expect(run.stdout).toContain(
      `FAIL: c-real: harness omp: ${A} default 2 step attempt 1: its set model is ${omp}, and the settings give its role ${fallback}`
    );
    expect(run.verdicts).toEqual(["c-real: FAIL", "live-at-both-reads: UNKNOWN"]);
    expect(run.code).toBe(1);
  });

  test("a record that counts no call fails", () => {
    const run = check({ [A]: [A2] }, agentsFile("implementer"));
    expect(run.stdout).toContain(
      `FAIL: c-real: harness omp: ${A} default 2 step attempt 2: its counts total 0`
    );
    // Its counts equal the transcript's none, so only the clause of at least 1 call fails it.
    expect(run.stdout).not.toContain("between the two reads");
    expect(run.verdicts).toEqual(["c-real: FAIL", "live-at-both-reads: UNKNOWN"]);
    expect(run.code).toBe(1);
  });

  test("a step with a response 1.236 s from its end read is not taken", () => {
    const run = check({ [A]: [A3] }, agentsFile("implementer"));
    expect(run.stdout).toContain(
      `GAP: ${A} default 2 step attempt 3 (omp): start read 2026-11-20T00:00:58.000338412Z: the last response ended 5.518 s before it, the next 2.365 s after it. end read 2026-11-20T00:07:02.500129008Z: the last response ended 1.236 s before it, the next 23.941 s after it. No response holds the completion call, so the end read leaves out none. Not taken: a response ended within 2 s of its end read.`
    );
    expect(run.stdout).toContain("c-real: UNKNOWN: took no step of harness omp");
    expect(run.verdicts).toEqual(["c-real: UNKNOWN", "live-at-both-reads: UNKNOWN"]);
    expect(run.code).toBe(2);
  });

  test("a step whose end read is final totals is not taken", () => {
    const run = check({ [A]: [A5] }, agentsFile("implementer"));
    expect(run.stdout).toContain(
      `GAP: ${A} default 2 step attempt 5 (omp): start read 2026-11-20T00:00:29.000187006Z: no response ended before it, the next 4.045 s after it. end read 2026-11-20T00:07:31.000462817Z: final totals. Not taken: its end read is final totals.`
    );
    expect(run.stdout).toContain("c-real: UNKNOWN: took no step of harness omp");
    expect(run.verdicts).toEqual(["c-real: UNKNOWN", "live-at-both-reads: UNKNOWN"]);
    expect(run.code).toBe(2);
  });

  test("an answering model that only a skipped record names fails", () => {
    const run = check({ [A]: [A1, A4] }, agentsFile("implementer"));
    expect(run.stdout).toContain(A1Passes);
    expect(run.stdout).toContain(
      `FAIL: c-real: ${A} default 2 step attempt 4: its answering model anthropic/claude-opus-5-5 is held by no entry of the transcript of ${implementer}`
    );
    expect(run.verdicts).toEqual(["c-real: FAIL", "live-at-both-reads: UNKNOWN"]);
    expect(run.code).toBe(1);
  });

  test("a set model the transcript does not hold fails", () => {
    const run = check({ [A]: [A1, A7] }, agentsFile("implementer"));
    expect(run.stdout).toContain(
      `FAIL: c-real: ${A} default 2 step attempt 7: its set model anthropic/claude-opus-5-5 is held by no entry of the transcript of ${implementer}`
    );
    expect(run.verdicts).toEqual(["c-real: FAIL", "live-at-both-reads: UNKNOWN"]);
    expect(run.code).toBe(1);
  });

  test("a last model the transcript does not hold fails", () => {
    const run = check({ [A]: [A1, A8] }, agentsFile("implementer"));
    expect(run.stdout).toContain(
      `FAIL: c-real: ${A} default 2 step attempt 8: its last model anthropic/claude-opus-4-8 is held by no entry of the transcript of ${implementer}`
    );
    expect(run.verdicts).toEqual(["c-real: FAIL", "live-at-both-reads: UNKNOWN"]);
    expect(run.code).toBe(1);
  });

  test("a log with no read line exits 2", () => {
    const log = join(data, "daemon-log-envelope.jsonl");
    const run = check({ [A]: [A1] }, agentsFile("implementer"), { log });
    expect(run.stdout).toContain(
      `UNKNOWN: the daemon log ${log} holds no line "workflow: a step used a read of its agent's calls"`
    );
    expect(run.stdout).not.toContain("PASS");
    expect(run.verdicts).toEqual(["c-real: UNKNOWN", "live-at-both-reads: UNKNOWN"]);
    expect(run.code).toBe(2);
  });

  test("a read line cut short exits 2, and takes no other step in its place", () => {
    const run = check({ [A]: [A2, A1] }, agentsFile("implementer"), { log: cutLog });
    expect(run.stdout).toContain(
      `UNKNOWN: line 6 of the daemon log ${cutLog} holds "workflow: a step used a read of its agent's calls" and is no JSON object`
    );
    expect(run.stdout).not.toContain("PASS");
    expect(run.verdicts).toEqual(["c-real: UNKNOWN", "live-at-both-reads: UNKNOWN"]);
    expect(run.code).toBe(2);
  });

  test("a record of an agent with no transcript exits 2", () => {
    const run = check({ [A]: [A1, C1] }, agentsFile("implementer"));
    expect(run.stdout).toContain(`UNKNOWN: no transcript of ${tester}`);
    expect(run.stdout).not.toContain("PASS");
    expect(run.verdicts).toEqual(["c-real: UNKNOWN", "live-at-both-reads: UNKNOWN"]);
    expect(run.code).toBe(2);
  });

  test("an answer of the release before exits 2", () => {
    const main = lines("printed-steps-main.txt");
    const run = check({ [A]: main.slice(1) }, agentsJson);
    expect(run.stdout).toContain(
      `UNKNOWN: a line of ${A} has 12 words, not the 16 that name a step's models: ${main[1]}`
    );
    expect(run.stdout).not.toContain("PASS");
    expect(run.verdicts).toEqual(["c-real: UNKNOWN", "live-at-both-reads: UNKNOWN"]);
    expect(run.code).toBe(2);
  });

  test("a dispatch steps that fails exits 2, with both verdicts", () => {
    const err = "dispatch: the Dispatch server answered no route\n";
    const run = check({ [A]: [A1] }, agentsFile("implementer"), { exit: 1, err });
    expect(run.stdout).toContain(
      `UNKNOWN: dispatch steps --issue ${A} exited 1, so nothing was read: ${err.trim()}`
    );
    expect(run.stdout).not.toContain("PASS");
    expect(run.verdicts).toEqual(["c-real: UNKNOWN", "live-at-both-reads: UNKNOWN"]);
    expect(run.code).toBe(2);
  });

  test("c-real takes no step that another fact ended", () => {
    const run = check({ [A]: [A6] }, agentsFile("implementer"));
    expect(run.stdout).toContain("GAP: harness omp: saw no step that its agent ended");
    expect(run.stdout).toContain("c-real: UNKNOWN: took no step of harness omp");
    expect(run.stdout).toContain(A6Passes);
    expect(run.stdout).not.toContain("PASS: c-real");
    expect(run.verdicts).toEqual(["c-real: UNKNOWN", "live-at-both-reads: PASS"]);
    expect(run.code).toBe(2);
  });

  test("--lower-one fails the steps that pass", () => {
    const run = check({ [A]: [A1, A6, C1, C3, A0] }, agentsJson, { args: ["--lower-one"] });
    expect(run.stdout).toContain(
      `FAIL: c-real: harness omp: ${A} default 2 step attempt 1: its counts are ${omp}=3,${fallback}=2, and its transcript holds ${omp}=4,${fallback}=2 between the two reads`
    );
    expect(run.stdout).toContain(
      `FAIL: c-real: harness claude: ${A} default 3 step attempt 1: its counts are ${sonnet}=2, and its transcript holds ${sonnet}=3 between the two reads`
    );
    expect(run.stdout).toContain(
      `FAIL: live-at-both-reads: harness omp: ${A} default 2 step attempt 6: its counts are ${omp}=1, and its transcript holds ${omp}=2 between the two reads`
    );
    expect(run.stdout).toContain(
      `FAIL: live-at-both-reads: harness claude: ${A} default 3 step attempt 3: its counts are ${sonnet}=0, and its transcript holds ${sonnet}=1 between the two reads`
    );
    expect(run.stdout).not.toContain("PASS");
    expect(run.verdicts).toEqual(["c-real: FAIL", "live-at-both-reads: FAIL"]);
    expect(run.code).toBe(1);
  });

  test("the printer renders printed-steps-16.txt from its records", async () => {
    /** One run of a step, as Dispatch holds it: the fields its printed line shows. */
    const run = (
      issue: string,
      fields: Pick<
        StepRecord,
        | "step"
        | "kind"
        | "role"
        | "type"
        | "attempt"
        | "agent"
        | "model"
        | "started_at"
        | "ended_at"
        | "result"
        | "set_model"
        | "answers"
      >
    ): StepRecord => ({
      id: `${issue}-${fields.kind}-${fields.step}-${fields.attempt}`,
      issue,
      task: "default",
      session: "",
      generation: 1,
      returned_to: null,
      report: null,
      rating: null,
      rating_reason: null,
      ...fields,
    });
    const implementation = (
      attempt: number,
      model: string,
      started_at: string,
      ended_at: string,
      result: StepRecord["result"],
      set_model: string,
      answers: StepRecord["answers"]
    ) =>
      run(A, {
        step: 2,
        kind: "step",
        role: "implementer",
        type: "implementation",
        attempt,
        agent: implementer,
        model,
        started_at,
        ended_at,
        result,
        set_model,
        answers,
      });
    const testing = (
      issue: string,
      attempt: number,
      started_at: string,
      ended_at: string,
      result: StepRecord["result"],
      calls: number
    ) =>
      run(issue, {
        step: 3,
        kind: "step",
        role: "tester",
        type: "testing",
        attempt,
        agent: `legion-legion-legion-${issue.split("-")[1]}-tester`,
        model: sonnet,
        started_at,
        ended_at,
        result,
        set_model: sonnet,
        answers: [{ model: sonnet, calls }],
      });
    const day = "2026-11-20T00:";
    const records: StepRecord[] = [
      implementation(1, fallback, `${day}00:30.083117Z`, `${day}07:30.104223871Z`, "passed", omp, [
        { model: omp, calls: 4 },
        { model: fallback, calls: 2 },
      ]),
      implementation(2, omp, `${day}01:25.081226Z`, `${day}06:50.087410333Z`, "passed", omp, []),
      implementation(3, fallback, `${day}00:58.074402Z`, `${day}07:02.581066718Z`, "passed", omp, [
        { model: omp, calls: 2 },
        { model: fallback, calls: 1 },
      ]),
      implementation(4, fallback, `${day}00:57.069812Z`, `${day}07:02.466190251Z`, "passed", omp, [
        { model: "anthropic/claude-opus-5-5", calls: 3 },
      ]),
      implementation(5, fallback, `${day}00:29.071183Z`, `${day}07:31.207554105Z`, "passed", omp, [
        { model: omp, calls: 4 },
        { model: fallback, calls: 2 },
      ]),
      implementation(6, omp, `${day}00:40.071183Z`, `${day}01:10.095552013Z`, "stopped", omp, [
        { model: omp, calls: 2 },
      ]),
      implementation(
        7,
        fallback,
        `${day}07:40.011111Z`,
        `${day}07:50.022222222Z`,
        "passed",
        "anthropic/claude-opus-5-5",
        null
      ),
      implementation(
        8,
        "anthropic/claude-opus-4-8",
        `${day}07:55.033333Z`,
        `${day}07:59.044444444Z`,
        "passed",
        "",
        null
      ),
      testing(A, 1, "2026-10-30T09:01:20.079934Z", "2026-10-30T09:02:00.093118446Z", "passed", 3),
      testing(A, 2, "2026-11-04T17:04:30.081115Z", "2026-11-04T17:04:45.09557019Z", "passed", 1),
      testing(A, 3, "2026-10-30T09:01:40.081234Z", "2026-10-30T09:02:05.097531246Z", "stopped", 1),
      run(A, {
        step: 0,
        kind: "merge",
        role: "merger",
        type: "",
        attempt: 1,
        agent: "legion-legion-legion-3550-merger",
        model: "",
        started_at: `${day}08:00.012345Z`,
        ended_at: `${day}09:10.078660636Z`,
        result: "passed",
        set_model: "",
        answers: null,
      }),
    ];
    // The second issue's file is made to the same shape, so the printer holds it too.
    const recordsB: StepRecord[] = [
      testing(B, 1, "2026-10-30T09:01:20.079934Z", "2026-10-30T09:01:45.093118446Z", "passed", 2),
      testing(B, 2, "2026-10-30T09:01:46.081234Z", "2026-10-30T09:02:05.097531246Z", "stopped", 1),
    ];
    const rendered = async (read: IssueSteps) => {
      const result = await executeDispatchTool({
        tool: "dispatch_steps",
        args: { issue: read.issue },
        cwd: dir,
        host: "omp",
        sessionId: "session-1",
        config: { url: "http://dispatch.test", credential: bearerCredential("secret") },
        env: {},
        exec: async (file, argv) => {
          throw new Error(`unexpected command: ${file} ${argv.join(" ")}`);
        },
        fetchImpl: fakeDispatchFetch({ routes: { [`/api/v1/issues/${read.issue}/steps`]: read } })
          .fetchImpl,
      });
      return `${result.text}\n`;
    };
    expect(await rendered({ issue: A, tasks: [{ task: "default", steps: records }] })).toBe(
      readFileSync(join(data, "printed-steps-16.txt"), "utf8")
    );
    expect(await rendered({ issue: B, tasks: [{ task: "default", steps: recordsB }] })).toBe(
      readFileSync(join(data, "printed-steps-16-b.txt"), "utf8")
    );
  });

  test("two issues each answer their own records", () => {
    const run = check({ [A]: [A1, A6, A0], [B]: [C1b, C3b] }, agentsJson);
    expect(run.stdout).toContain(A1Passes);
    expect(run.stdout).toContain(
      `PASS: c-real: harness claude: ${B} default 3 step attempt 1: ${sonnet}=2`
    );
    expect(run.stdout).toContain(A6Passes);
    expect(run.stdout).toContain(
      `PASS: live-at-both-reads: harness claude: ${B} default 3 step attempt 2: ${sonnet}=1`
    );
    expect(run.stdout).not.toContain("FAIL");
    expect(run.verdicts).toEqual(["c-real: PASS", "live-at-both-reads: PASS"]);
    expect(run.code).toBe(0);
    // Each issue is read once, with `dispatch steps --issue <KEY>`, in the order given.
    expect(run.calls).toBe(`steps --issue ${A}\nsteps --issue ${B}\n`);
  });

  test("a captured Claude Code session counts each main response once and no subagent response", () => {
    // The input holds what the row needs. One response has two lines, and the main ids are the
    // list's. The subagent's file holds the one line of the list's subagent response.
    const [mainLines, subagentLines] = [captured, capturedSubagent].map((name) =>
      lines(name)
        .map((line) => JSON.parse(line))
        .filter((entry) => entry.type === "assistant")
    );
    const ids = mainLines.map((entry) => entry.message.id);
    expect(ids.length).toBeGreaterThan(new Set(ids).size);
    expect([...new Set(ids)].sort()).toEqual(servedMain.map((response) => response.id).sort());
    expect(subagentLines.map((entry) => [entry.message.id, entry.isSidechain])).toEqual(
      served.filter((response) => response.subagent).map((response) => [response.id, true])
    );
    const counted = transcriptResponses(join(data, captured), "claude");
    expect(counted.map(({ model, counts }) => ({ model, counts }))).toEqual(
      servedMain.map((response) => ({ model: `anthropic/${response.model}`, counts: true }))
    );
    expect(transcriptResponses(join(data, capturedSubagent), "claude")).toEqual([]);
  });

  test("a captured session holds its set model", () => {
    // The input holds what the row needs: Claude Code asked for claude-model, which never answered.
    expect(servedMain.length).toBeGreaterThan(0);
    for (const response of servedMain) {
      expect(response).toMatchObject({ asked: "claude-model", model: "claude-served-model" });
    }
    const transcript = join(data, captured);
    const answered = transcriptResponses(transcript, "claude").map((response) => response.model);
    expect(answered).not.toContain("anthropic/claude-model");
    expect([...heldModels(transcript, "claude")]).toContain("anthropic/claude-model");
  });

  test("the chain's captured records of both issues pass both comparisons", () => {
    // Each issue's records are what `dispatch steps` printed in the whole-chain test, header first.
    const [headerA, ...recordsA] = lines("printed-steps-16-captured-A.txt");
    const [headerB, ...recordsB] = lines("printed-steps-16-captured-B.txt");
    const [issueA, issueB] = [headerA, headerB].map((header) => header.split(":")[0]);
    // The chain's own run judged these records before a step's completion call counted. Its two
    // live-at-both-reads PASS lines stand, and its c-real answer was UNKNOWN.
    const compared = lines("compare-captured.txt");
    const livePasses = compared.filter((line) => line.startsWith("PASS: live-at-both-reads: "));
    expect(livePasses.length).toBe(2);
    expect(compared.slice(-2)).toEqual(["c-real: UNKNOWN", "live-at-both-reads: PASS"]);
    const run = check(
      { [issueA]: recordsA, [issueB]: recordsB },
      join(data, "agents-captured.json"),
      { log: join(data, "daemon-log-captured.jsonl") }
    );
    expect(run.stdout).toContain(
      `PASS: c-real: harness omp: ${issueA} a 1 step attempt 2: smimplementera/smimplementera-model=3`
    );
    expect(run.stdout).toContain(
      `PASS: c-real: harness claude: ${issueA} a 2 step attempt 2: anthropic/claude-model=3`
    );
    for (const line of livePasses) {
      expect(run.stdout).toContain(line);
    }
    expect(run.stdout).not.toContain("FAIL");
    expect(run.verdicts).toEqual(["c-real: PASS", "live-at-both-reads: PASS"]);
    expect(run.code).toBe(0);
    expect(run.calls).toBe(`steps --issue ${issueA}\nsteps --issue ${issueB}\n`);
  });

  // The rows below read LEGION-3552's records. Each step that its agent ended calls the `legion`
  // tool with a completion op in its last response before the end read, as a real agent does.
  test("an Oh My Pi step counts its completion's response, 0.050 s before its end read", () => {
    const run = check({ [D]: [D1] }, agentsFile("d-implementer"), { log: completionLog });
    expect(run.stdout).toContain(
      `GAP: ${D} default 2 step attempt 1 (omp): start read 2026-11-21T10:00:00.000318273Z: no response ended before it, the next 3.000 s after it. end read 2026-11-21T10:00:10.000216347Z: the last response ended 4.000 s before it, the next 3.000 s after it. The end read leaves out the response of 2026-11-21T10:00:09.950Z, which holds the completion call and ended 0.050 s before it.`
    );
    expect(run.stdout).toContain(
      `PASS: c-real: harness omp: ${D} default 2 step attempt 1: ${omp}=3`
    );
    expect(run.verdicts).toEqual(["c-real: PASS", "live-at-both-reads: UNKNOWN"]);
    expect(run.code).toBe(0);
  });

  test("a Claude Code step counts its completion's response once by its id", () => {
    const run = check({ [D]: [T1] }, agentsFile("d-tester"), { log: completionLog });
    expect(run.stdout).toContain(
      `GAP: ${D} default 3 step attempt 1 (claude): start read 2026-11-21T10:10:00.000318273Z: no response ended before it, the next 3.000 s after it. end read 2026-11-21T10:10:10.000216347Z: the last response ended 4.000 s before it, the next 3.000 s after it. The end read leaves out the response of 2026-11-21T10:10:09.950Z, which holds the completion call and ended 0.050 s before it.`
    );
    expect(run.stdout).toContain(
      `PASS: c-real: harness claude: ${D} default 3 step attempt 1: ${sonnet}=3`
    );
    expect(run.verdicts).toEqual(["c-real: PASS", "live-at-both-reads: UNKNOWN"]);
    expect(run.code).toBe(0);
  });

  test("another response 1.500 s before the end read refuses the step", () => {
    const run = check({ [D]: [D2] }, agentsFile("d-implementer"), { log: completionLog });
    // That response calls the `legion` tool too, with the op `read_record`.
    expect(run.stdout).toContain(
      `GAP: ${D} default 2 step attempt 2 (omp): start read 2026-11-21T10:01:00.000318273Z: the last response ended 47.000 s before it, the next 3.000 s after it. end read 2026-11-21T10:01:10.000216347Z: the last response ended 1.500 s before it, the next 3.000 s after it. The end read leaves out the response of 2026-11-21T10:01:09.950Z, which holds the completion call and ended 0.050 s before it. Not taken: a response ended within 2 s of its end read.`
    );
    expect(run.stdout).toContain("c-real: UNKNOWN: took no step of harness omp");
    expect(run.verdicts).toEqual(["c-real: UNKNOWN", "live-at-both-reads: UNKNOWN"]);
    expect(run.code).toBe(2);
  });

  test("a response within 2 s after the end read or of the start read refuses the step", () => {
    const run = check({ [D]: [D3, D4, D5] }, agentsFile("d-implementer"), { log: completionLog });
    expect(run.stdout).toContain(
      `GAP: ${D} default 2 step attempt 3 (omp): start read 2026-11-21T10:02:00.000318273Z: the last response ended 47.000 s before it, the next 3.000 s after it. end read 2026-11-21T10:02:10.000216347Z: the last response ended 4.000 s before it, the next 1.000 s after it. The end read leaves out the response of 2026-11-21T10:02:09.950Z, which holds the completion call and ended 0.050 s before it. Not taken: a response ended within 2 s of its end read.`
    );
    expect(run.stdout).toContain(
      `GAP: ${D} default 2 step attempt 4 (omp): start read 2026-11-21T10:03:00.000318273Z: the last response ended 49.000 s before it, the next 1.000 s after it. end read 2026-11-21T10:03:10.000216347Z: the last response ended 4.000 s before it, the next 3.000 s after it. The end read leaves out the response of 2026-11-21T10:03:09.950Z, which holds the completion call and ended 0.050 s before it. Not taken: a response ended within 2 s of its start read.`
    );
    // D5's only response is the completion's, 1 s after its start read. The start read leaves out
    // no response.
    expect(run.stdout).toContain(
      `GAP: ${D} default 2 step attempt 5 (omp): start read 2026-11-21T10:04:00.000318273Z: the last response ended 47.000 s before it, the next 1.000 s after it. end read 2026-11-21T10:04:01.050216347Z: the last response ended 48.050 s before it, the next 3.000 s after it. The end read leaves out the response of 2026-11-21T10:04:01.000Z, which holds the completion call and ended 0.050 s before it. Not taken: a response ended within 2 s of its start read.`
    );
    expect(run.stdout).toContain("c-real: UNKNOWN: took no step of harness omp");
    expect(run.verdicts).toEqual(["c-real: UNKNOWN", "live-at-both-reads: UNKNOWN"]);
    expect(run.code).toBe(2);
  });

  test("the completion's response is found by its call where it ends after the end read", () => {
    const run = check({ [D]: [D6] }, agentsFile("d-implementer"), { log: completionLog });
    expect(run.stdout).toContain(
      `GAP: ${D} default 2 step attempt 6 (omp): start read 2026-11-21T10:05:00.000318273Z: the last response ended 55.950 s before it, the next 3.000 s after it. end read 2026-11-21T10:05:10.000216347Z: the last response ended 4.000 s before it, the next 3.080 s after it. The end read leaves out the response of 2026-11-21T10:05:10.080Z, which holds the completion call and ended 0.080 s after it.`
    );
    // The count holds that response too, so it equals the record's 3.
    expect(run.stdout).toContain(
      `PASS: c-real: harness omp: ${D} default 2 step attempt 6: ${omp}=3`
    );
    expect(run.verdicts).toEqual(["c-real: PASS", "live-at-both-reads: UNKNOWN"]);
    expect(run.code).toBe(0);
  });

  test("a step that another fact ended gets no exception at its end read", () => {
    // D7's last response before its end read calls handoff_complete 0.050 s before the stop.
    const run = check({ [D]: [D7] }, agentsFile("d-implementer"), { log: completionLog });
    expect(run.stdout).toContain("GAP: harness omp: saw no step that its agent ended");
    expect(run.stdout).toContain("live-at-both-reads: UNKNOWN: took no step of harness omp");
    expect(run.stdout).not.toContain("PASS");
    expect(run.verdicts).toEqual(["c-real: UNKNOWN", "live-at-both-reads: UNKNOWN"]);
    expect(run.code).toBe(2);
  });

  test("a step whose transcript holds no completion call keeps the plain 2 s rule", () => {
    // The real cut session holds no `content`, and A1's reads are more than 2 s from any response.
    const run = check({ [A]: [A1] }, agentsFile("implementer"));
    expect(run.stdout).toContain(
      `GAP: ${A} default 2 step attempt 1 (omp): start read 2026-11-20T00:00:30.000913551Z: no response ended before it, the next 3.044 s after it. end read 2026-11-20T00:07:30.003562114Z: the last response ended 3.563 s before it, the next 3.346 s after it. No response holds the completion call, so the end read leaves out none.`
    );
    expect(run.stdout).toContain(A1Passes);
    expect(run.verdicts).toEqual(["c-real: PASS", "live-at-both-reads: UNKNOWN"]);
    expect(run.code).toBe(0);
  });

  test("a legion call of another op is no completion call", () => {
    // D8's last response before its end read calls the `legion` tool with `ready_for_update`.
    const run = check({ [D]: [D8] }, agentsFile("d-implementer"), { log: completionLog });
    expect(run.stdout).toContain(
      `GAP: ${D} default 2 step attempt 8 (omp): start read 2026-11-21T10:07:00.000318273Z: the last response ended 47.000 s before it, the next 3.000 s after it. end read 2026-11-21T10:07:10.000216347Z: the last response ended 0.050 s before it, the next 3.000 s after it. No response holds the completion call, so the end read leaves out none. Not taken: a response ended within 2 s of its end read.`
    );
    expect(run.stdout).toContain("c-real: UNKNOWN: took no step of harness omp");
    expect(run.verdicts).toEqual(["c-real: UNKNOWN", "live-at-both-reads: UNKNOWN"]);
    expect(run.code).toBe(2);
  });

  test("the step's completion call is the last one between its reads", () => {
    // D9's agent called handoff_complete 5.000 s before the end read, and again 0.050 s before it.
    const run = check({ [D]: [D9] }, agentsFile("d-implementer"), { log: completionLog });
    expect(run.stdout).toContain(
      `GAP: ${D} default 2 step attempt 9 (omp): start read 2026-11-21T10:08:00.000318273Z: the last response ended 47.000 s before it, the next 3.000 s after it. end read 2026-11-21T10:08:10.000216347Z: the last response ended 3.000 s before it, the next 3.000 s after it. The end read leaves out the response of 2026-11-21T10:08:09.950Z, which holds the completion call and ended 0.050 s before it.`
    );
    expect(run.stdout).toContain(
      `PASS: c-real: harness omp: ${D} default 2 step attempt 9: ${omp}=4`
    );
    expect(run.verdicts).toEqual(["c-real: PASS", "live-at-both-reads: UNKNOWN"]);
    expect(run.code).toBe(0);
  });

  test("a call after the end read and after the claim's next read is no completion call", () => {
    // D10's only call of the `legion` tool ends 0.080 s after its end read, as D6's does. A read
    // of the same claim for its next step comes 0.040 s after the end read, before that call.
    const run = check({ [D]: [D10] }, agentsFile("d-implementer"), { log: completionLog });
    expect(run.stdout).toContain(
      `GAP: ${D} default 2 step attempt 10 (omp): start read 2026-11-21T10:09:00.000318273Z: the last response ended 47.000 s before it, the next 3.000 s after it. end read 2026-11-21T10:09:10.000216347Z: the last response ended 4.000 s before it, the next 0.080 s after it. No response holds the completion call, so the end read leaves out none. Not taken: a response ended within 2 s of its end read.`
    );
    expect(run.stdout).toContain("c-real: UNKNOWN: took no step of harness omp");
    expect(run.verdicts).toEqual(["c-real: UNKNOWN", "live-at-both-reads: UNKNOWN"]);
    expect(run.code).toBe(2);
  });

  test("an acceptance step that its architect ended with sign_off counts that response", () => {
    const run = check({ [D]: [S1] }, agentsFile("d-architect"), { log: completionLog });
    expect(run.stdout).toContain(
      `GAP: ${D} default 5 step attempt 1 (omp): start read 2026-11-21T10:20:00.000318273Z: no response ended before it, the next 3.000 s after it. end read 2026-11-21T10:20:10.000216347Z: the last response ended 4.000 s before it, the next 3.000 s after it. The end read leaves out the response of 2026-11-21T10:20:09.950Z, which holds the completion call and ended 0.050 s before it.`
    );
    expect(run.stdout).toContain(
      `PASS: c-real: harness omp: ${D} default 5 step attempt 1: ${omp}=3`
    );
    expect(run.verdicts).toEqual(["c-real: PASS", "live-at-both-reads: UNKNOWN"]);
    expect(run.code).toBe(0);
  });
});
