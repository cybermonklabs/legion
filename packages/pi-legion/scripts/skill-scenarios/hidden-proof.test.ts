// hidden-proof.ts on staged runs: the rig's own reads of a run's results before and after its
// switch went on (rig.sh quiet_reads), and its transcripts. Each check is shown to fail on the fault
// it is for: the answer after left as the answer before (the measurement's planted fault), one more
// field gone, a state line after, a state line in a subagent's transcript; and to say UNKNOWN where
// it could not see: no reads, an answer before with no state, a run that read no results.
//
//   bun test hidden-proof.test.ts

import { expect, setDefaultTimeout, test } from "bun:test";
import { mkdirSync, symlinkSync, writeFileSync } from "node:fs";
import path from "node:path";
import { spawnWithin } from "@legion/test-fixtures/row-clock";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { quietCheck, scanTranscripts, stateLines } from "./hidden-proof";
import { fileRowLimit, SCRIPT_RUN } from "./test-limit";
import { parseSession } from "./transcript";

// The rows that run hidden-proof.ts as a process of its own take longer than bun's 5 s default on a
// loaded machine, so each test and hook gets the limit test-limit.ts computes from the bounds of
// what they wait for.
setDefaultTimeout(fileRowLimit({ ...SCRIPT_RUN, part: "hidden-proof.ts" }));

const scratch = temporaryDirectory("hidden-proof", { real: true });
const HEAD = "b".repeat(40);

/** A results answer of one check at one open head, with the state the server computes. */
function answer() {
  return {
    issue: "ACME-1",
    verification_artifact_id: "art-1",
    checks: [
      {
        block_id: "k1",
        when: "before_merge",
        text: "docs/usage.md names widgets --help.",
        compared_with: [{ kind: "pull_request", number: 702, commit: HEAD }],
        pass_stands: { state: "not_known", reason: "pass_and_fail", subjects: [{ commit: HEAD }] },
        ci: { line: "no result" },
        results: [
          {
            id: "r1",
            verdict: "pass",
            role: "tester",
            commit: HEAD,
            currency: "current",
            commit_stands: "head",
          },
          {
            id: "r2",
            verdict: "fail",
            role: "tester",
            commit: HEAD,
            currency: "current",
            commit_stands: "head",
          },
        ],
      },
    ],
  };
}

function hiddenAnswer() {
  const after = answer();
  for (const check of after.checks as Record<string, unknown>[]) {
    delete check.pass_stands;
    delete check.ci;
    for (const result of check.results as Record<string, unknown>[]) delete result.currency;
  }
  return after;
}

const TEXT_BEFORE = [
  "Checks of ACME-1:",
  "",
  "- k1 (before_merge). Compared with the heads of 1 open pull request. A pass stands: not known, as 0 of 1 heads read yes.",
  `  - #702 at ${HEAD}: not known, as pass r1 and fail r2 count at this commit.`,
  "    Pull request line: - Check k1 (before_merge): not known at bbbbbbbbbbbb",
  "  The check's words: docs/usage.md names widgets --help.",
  `  - fail by tester at ${HEAD}. That commit is the head of pull request #702. Result r2.`,
].join("\n");
const TEXT_AFTER = [
  "Checks of ACME-1:",
  "",
  "- k1 (before_merge). Compared with the heads of 1 open pull request.",
  "  The check's words: docs/usage.md names widgets --help.",
  `  - fail by tester at ${HEAD}. That commit is the head of pull request #702. Result r2.`,
].join("\n");

let stages = 0;
/** The six reads of one run under a quiet directory, each overridable. */
function reads(over: Partial<Record<string, string>> = {}): string {
  const dir = path.join(scratch, `quiet-${++stages}`);
  mkdirSync(dir, { recursive: true });
  const base = path.join(dir, "0123456789abcdef");
  const files: Record<string, string> = {
    "before-curl": JSON.stringify(answer()),
    "after-curl": JSON.stringify(hiddenAnswer()),
    "before-text": TEXT_BEFORE,
    "after-text": TEXT_AFTER,
    "before-json": JSON.stringify(answer(), null, 2),
    "after-json": JSON.stringify(hiddenAnswer(), null, 2),
    ...over,
  };
  for (const [which, body] of Object.entries(files)) writeFileSync(`${base}.${which}`, body ?? "");
  return base;
}

// Catches: a switch that hides nothing, or that hides more than the three fields.
test("the answer after, the answer before less pass_stands, ci and currency, is hidden", () => {
  expect(quietCheck(reads())).toEqual({
    verdict: "hidden",
    detail:
      "answer before less 1 pass_stands and 2 currency equals the answer after; 3 state lines before, 0 after",
  });
});

// The measurement's control: the after-file a copy of the before-file.
test("an answer after that is the answer before, unchanged, is a FAULT", () => {
  const before = JSON.stringify(answer());
  const quiet = quietCheck(
    reads({ "after-curl": before, "after-text": TEXT_BEFORE, "after-json": before })
  );
  expect(quiet.verdict).toBe("FAULT");
  expect(quiet.detail).toContain(
    "the answer after is not the answer before less pass_stands, ci and currency"
  );
  expect(quiet.detail).toContain("the text after holds 3 state lines");
  expect(quiet.detail).toContain("the --json read after holds");
});

// Catches: a switch that also drops a field the agent must still read (commit_stands).
test("an answer after that lacks one more field than the three is a FAULT", () => {
  const after = hiddenAnswer();
  for (const result of after.checks[0]?.results as Record<string, unknown>[])
    delete result.commit_stands;
  const quiet = quietCheck(reads({ "after-curl": JSON.stringify(after) }));
  expect(quiet).toEqual({
    verdict: "FAULT",
    detail: "the answer after is not the answer before less pass_stands, ci and currency",
  });
});

// Catches: a command that still prints the line of a head with pass_stands absent.
test("a text after that holds the line of a head is a FAULT", () => {
  const quiet = quietCheck(
    reads({
      "after-text": `${TEXT_AFTER}\n  - #702 at ${HEAD}: not known, as pass r1 and fail r2 count at this commit.`,
    })
  );
  expect(quiet.verdict).toBe("FAULT");
  expect(quiet.detail).toBe(
    `the text after holds 1 state lines: - #702 at ${HEAD}: not known, as pass r1 and fail r2 count at this commit.`
  );
});

test("no reads, an answer before with no state, or a text before with no state line is UNKNOWN", () => {
  expect(quietCheck(path.join(scratch, "nothing", "0123456789abcdef")).verdict).toBe("UNKNOWN");
  const switchedEarly = JSON.stringify(hiddenAnswer());
  expect(quietCheck(reads({ "before-curl": switchedEarly })).detail).toBe(
    "the answer before holds 0 pass_stands and 0 currency, so the comparison could not fail"
  );
  expect(quietCheck(reads({ "before-text": TEXT_AFTER })).verdict).toBe("UNKNOWN");
  expect(quietCheck(reads({ "after-curl": "curl: (7) Failed to connect" })).verdict).toBe(
    "UNKNOWN"
  );
});

test("a line of a skill's template, with its placeholders, is no state line", () => {
  expect(stateLines("Pull request line: - Check <id> (before_merge): <verdict> at <sha>")).toEqual(
    []
  );
  expect(
    stateLines("    Pull request line: - Check k1 (before_merge): pass at bbbbbbbbbbbb")
  ).toHaveLength(1);
});

let runs = 0;
/** A run directory whose transcripts are `sessions`: a path under sessions/ to the tool results of
 * that transcript. */
function run(sessions: Record<string, string[]>, calls: string[] = []): string {
  const dir = path.join(scratch, `run-${++runs}`);
  for (const [file, results] of Object.entries(sessions)) {
    const full = path.join(dir, "sessions", file);
    mkdirSync(path.dirname(full), { recursive: true });
    const lines = [
      { type: "session", id: `s-${runs}` },
      ...calls.map((command) => ({
        type: "message",
        message: {
          role: "assistant",
          content: [{ type: "toolCall", id: "c1", name: "bash", arguments: { command } }],
        },
      })),
      ...results.map((text) => ({
        type: "message",
        message: { role: "toolResult", toolName: "bash", content: [{ type: "text", text }] },
      })),
    ];
    writeFileSync(full, `${lines.map((line) => JSON.stringify(line)).join("\n")}\n`);
  }
  return dir;
}

test("the transcript decoder reads a file the hidden proof scans", () => {
  const dir = run({ "decoder.jsonl": [TEXT_AFTER] });
  const entries = parseSession(path.join(dir, "sessions", "decoder.jsonl"));
  expect(entries).toHaveLength(2);
  expect(entries[0]?.type).toBe("session");
  expect(entries[1]?.message?.role).toBe("toolResult");
});

test("a run that read its results and saw no state line is hidden; its curl of the route is counted", () => {
  const scan = scanTranscripts(
    run({ "a.jsonl": [TEXT_AFTER, JSON.stringify(hiddenAnswer())] }, [
      "curl -s http://127.0.0.1:4567/api/v1/issues/ACME-1/results",
    ])
  );
  expect(scan).toEqual({
    transcripts: 1,
    reads: 2,
    lines: 0,
    apiCalls: 1,
    first: "",
    verdict: "hidden",
  });
});

// Catches: a scan that reads the agent's own transcript alone, or one level of subagents.
test("a state line in a subagent's subagent's transcript is a LEAK", () => {
  const scan = scanTranscripts(
    run({
      "a.jsonl": [TEXT_AFTER],
      "a/task-1.jsonl": ["nothing here"],
      "a/task-1/deep.jsonl": [`  - #702 at ${HEAD}: yes, from implementer, tester.`],
    })
  );
  expect(scan.verdict).toBe("LEAK");
  expect(scan.transcripts).toBe(3);
  expect(scan.first).toBe(`- #702 at ${HEAD}: yes, from implementer, tester.`);
});

test("a run that read no results answer is UNKNOWN, and so is a run with no transcript", () => {
  expect(scanTranscripts(run({ "a.jsonl": ["ls\nREADME.md"] })).verdict).toBe("UNKNOWN");
  expect(scanTranscripts(path.join(scratch, "no-run")).verdict).toBe("UNKNOWN");
});

/** A map with the named runs: each run's directory, its link and, for a hidden run, its reads. */
function map(
  entries: { name: string; quiet?: Partial<Record<string, string>>; results: string[] }[]
) {
  const work = path.join(scratch, `work-${++runs}`);
  const maps = path.join(work, "maps");
  mkdirSync(path.join(maps, "runs"), { recursive: true });
  mkdirSync(path.join(maps, "quiet"), { recursive: true });
  entries.forEach((entry, index) => {
    const id = `${index}`.padStart(16, "0");
    const dir = path.join(work, "runs", id);
    mkdirSync(path.join(dir, "sessions"), { recursive: true });
    writeFileSync(
      path.join(dir, "sessions", "s.jsonl"),
      `${[{ type: "session", id }, ...entry.results.map((text) => ({ type: "message", message: { role: "toolResult", content: [{ type: "text", text }] } }))].map((line) => JSON.stringify(line)).join("\n")}\n`
    );
    symlinkSync(`../../runs/${id}`, path.join(maps, "runs", entry.name));
    if (entry.quiet !== undefined) {
      const base = reads(entry.quiet);
      for (const which of [
        "before-curl",
        "after-curl",
        "before-text",
        "after-text",
        "before-json",
        "after-json",
      ])
        symlinkSync(`${base}.${which}`, path.join(maps, "quiet", `${id}.${which}`));
    }
  });
  return maps;
}

function proof(maps: string, scenario?: string) {
  const result = spawnWithin(
    { ...SCRIPT_RUN, part: "hidden-proof.ts" },
    [
      "bun",
      path.join(import.meta.dir, "hidden-proof.ts"),
      maps,
      ...(scenario === undefined ? [] : [scenario]),
    ],
    { stdout: "pipe", stderr: "pipe" }
  );
  return { code: result.exitCode, out: result.stdout.toString(), err: result.stderr.toString() };
}

test("rig.sh hidden-proof passes a map whose hidden runs are each hidden, with a visible run as the control", () => {
  const maps = map([
    { name: "architect-pass-and-fail-head-1", results: [TEXT_BEFORE] },
    { name: "architect-pass-and-fail-head-hidden-1", quiet: {}, results: [TEXT_AFTER] },
    {
      name: "architect-pass-and-fail-ablate-architect-later-fail-hidden-1",
      quiet: {},
      results: [TEXT_AFTER],
    },
  ]);
  const { code, out } = proof(maps);
  expect(code).toBe(0);
  expect(out).toContain(
    "architect-pass-and-fail-head-hidden-1\thidden\thidden\t1\t1\t0\t0\thidden\t"
  );
  expect(out).toContain("architect-pass-and-fail-head-1\tvisible\t-\t1\t1\t3\t0\tcontrol\t");
  expect(out).toContain(
    "runs of a -hidden label: 2, hidden: 2; runs of another label: 1, with state lines in their transcripts: 1"
  );
});

test("rig.sh hidden-proof fails a map with a hidden run that leaked, or one whose switch hid nothing", () => {
  const leaked = map([
    { name: "reviewer-pass-and-fail-head-hidden-1", quiet: {}, results: [TEXT_BEFORE] },
  ]);
  expect(proof(leaked)).toMatchObject({ code: 1 });
  expect(proof(leaked).out).toContain("\tLEAK\t");
  const before = JSON.stringify(answer());
  const unswitched = map([
    {
      name: "reviewer-pass-and-fail-head-hidden-1",
      quiet: { "after-curl": before },
      results: [TEXT_AFTER],
    },
  ]);
  expect(proof(unswitched).code).toBe(1);
  expect(proof(unswitched).out).toContain("\tFAULT\t");
});

// Catches: a proof that reports a clean result when it enumerated no hidden run.
test("rig.sh hidden-proof over a map with no run of a -hidden label is UNKNOWN, never clean", () => {
  const maps = map([{ name: "architect-pass-and-fail-head-1", results: [TEXT_BEFORE] }]);
  const { code, out } = proof(maps);
  expect(code).toBe(1);
  expect(out).toContain("UNKNOWN: no run of a -hidden label");
  expect(proof(maps, "reviewer-own-head").out).toContain("for reviewer-own-head");
});
