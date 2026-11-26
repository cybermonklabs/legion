import { expect, test } from "bun:test";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import {
  compare,
  EXPORT_SCHEMA,
  type Expected,
  main,
  type PromptInputsRow,
  parseComposition,
  parseExport,
  parseRows,
  type RenderExport,
  rowsLaunchName,
  rowsPartPath,
  sizes,
} from "./teardown-inputs";

// The sample in testdata/teardown-inputs (teardown-inputs.command): one real row of the
// prompt-inputs proof and its composition.json, as the Go driver wrote them, and an export
// written by hand in the private render's shape at the same public commit, with the neutral
// projects ACME and BETA. No test here reads a real export of the private render: that pair is
// fixture F6.
const sample = path.join(import.meta.dir, "testdata", "teardown-inputs");
const exportFile = path.join(sample, "export.json");
const evidenceDir = path.join(sample, "evidence");
const HEAD = "ecd967b17a4e527009dab56f459e0f4e76c0bd25";
const OTHER = "d00ed2547ce52cb9c42fc333410b2ce7381630f0";
const CHECKOUT = "0123456789abcdef0123456789abcdef01234567";
const LAUNCH = "controller-headless/omp";
const expected: Expected = {
  head: HEAD,
  branch: "sample-branch",
  projects: ["ACME", "BETA"],
  launches: [LAUNCH],
};

const exportText = readFileSync(exportFile, "utf8");
const rowText = readFileSync(path.join(evidenceDir, "rows.jsonl"), "utf8");
const compositionText = readFileSync(path.join(evidenceDir, "composition.json"), "utf8");
const sampleExport = parseExport(exportText);
const rows = parseRows(rowText);
const evidence = { rows, subjectHead: parseComposition(compositionText) };
const scratch = temporaryDirectory("teardown-inputs", { real: true });

/** A copy of the sample export with `change` made to it. */
function exportWith(change: (exported: RenderExport) => void): RenderExport {
  const copy = structuredClone(sampleExport);
  change(copy);
  return copy;
}

/** The problems `compare` finds in an export against the sample's rows. */
function problemsOf(exported: RenderExport, against: Expected = expected): readonly string[] {
  return compare(exported, evidence, against).problems;
}

test("the sample's row and composition read as the Go driver wrote them, and its export reads", () => {
  expect(rows).toHaveLength(1);
  expect(rows[0]).toMatchObject({
    launch: LAUNCH,
    parts: [
      {
        path: "roles/controller-root.md",
        bytes: 3693,
        sha256: "c211980843f14c75d90bf17a1f82f54db66e0e6a2d878cddb4b78618e3fb3aac",
      },
      {
        path: "go/controller-headless.md",
        bytes: 2830,
        sha256: "24660f30c9d5fab861c48458fcdc31226f27541d27329b8a9740d578e10e2f5b",
      },
    ],
    required_reads: [],
    reads_observed: "not observed",
    total_bytes: 78862,
  });
  expect(evidence.subjectHead).toBe(HEAD);
  expect(sampleExport.schema).toBe(EXPORT_SCHEMA);
  expect(sampleExport.renders.map((render) => render.project)).toEqual(["ACME", "BETA"]);
  expect(sampleExport.renders[1]?.memory).toEqual({
    bytes: 57,
    sha256: "e88aae70ffdd80677d4e949aa7afb55862e982ee21841a33889ce022de6c8edf",
  });
});

test("each public part of the sample export agrees with the row in path, bytes and sha256", () => {
  expect(compare(sampleExport, evidence, expected)).toEqual({
    problems: [],
    projects: [
      { project: "ACME", launches: 1, parts: 2 },
      { project: "BETA", launches: 1, parts: 2 },
    ],
    checkout: { label: "main", commit: CHECKOUT },
  });
});

test("zero rows fail, and so does a row without a part, a repeated row or a row of no expected launch", () => {
  expect(parseRows("")).toEqual([]);
  expect(compare(sampleExport, { ...evidence, rows: [] }, expected)).toEqual({
    problems: ["the rows hold no launch", `the rows hold no row of ${LAUNCH}`],
    projects: [
      { project: "ACME", launches: 0, parts: 0 },
      { project: "BETA", launches: 0, parts: 0 },
    ],
    checkout: { label: "main", commit: CHECKOUT },
  });
  const row = rows[0] as PromptInputsRow;
  const against = (some: PromptInputsRow[]) =>
    compare(sampleExport, { ...evidence, rows: some }, expected).problems;
  expect(against([{ ...row, parts: [] }])).toEqual([`the row of ${LAUNCH} has no part`]);
  expect(against([row, row])).toEqual([`the rows hold ${LAUNCH} 2 times`]);
  expect(against([row, { ...row, launch: "merger/omp" }])).toEqual([
    "the rows hold merger/omp, which is not an expected launch",
  ]);
});

test("a missing, unknown or repeated project fails", () => {
  expect(problemsOf(sampleExport, { ...expected, projects: ["ACME", "BETA", "GAMMA"] })).toEqual([
    "the export has no gate render of GAMMA",
  ]);
  const noBeta = exportWith((exported) => {
    exported.renders = exported.renders.filter((render) => render.project !== "BETA");
  });
  expect(problemsOf(noBeta)).toEqual(["the export has no gate render of BETA"]);
  expect(problemsOf(sampleExport, { ...expected, projects: ["ACME"] })).toEqual([
    "the export renders BETA, which is not an expected project",
  ]);
  const twice = exportWith((exported) => {
    const [acme] = exported.renders;
    if (acme !== undefined) exported.renders.push(structuredClone(acme));
  });
  expect(problemsOf(twice)).toEqual(["the export renders ACME 2 times"]);
  const noGate = exportWith((exported) => {
    for (const render of exported.renders) render.gate = false;
  });
  expect(compare(noGate, evidence, expected)).toMatchObject({
    problems: [
      "the export holds no gate render",
      "the export has no gate render of ACME",
      "the export has no gate render of BETA",
    ],
    checkout: undefined,
  });
});

test("a missing, unknown or repeated launch fails", () => {
  expect(problemsOf(sampleExport, { ...expected, launches: [LAUNCH, "merger/claude"] })).toEqual([
    "ACME: the export has no launch merger/claude",
    "BETA: the export has no launch merger/claude",
    "the rows hold no row of merger/claude",
  ]);
  const none = exportWith((exported) => {
    const [acme] = exported.renders;
    if (acme !== undefined) acme.launches = [];
  });
  expect(problemsOf(none)).toEqual([
    "ACME's render holds no launch",
    `ACME: the export has no launch ${LAUNCH}`,
  ]);
  const unknown = exportWith((exported) => {
    const launch = exported.renders[0]?.launches[0];
    if (launch !== undefined) launch.launch = "controller-sidecar";
  });
  expect(problemsOf(unknown)).toEqual([
    "ACME: the launch controller-sidecar (controller on omp) is no launch this reader knows",
    `ACME: the export has no launch ${LAUNCH}`,
  ]);
  const wrongRole = exportWith((exported) => {
    const launch = exported.renders[1]?.launches[0];
    if (launch !== undefined) launch.role = "planner";
  });
  expect(problemsOf(wrongRole)).toEqual([
    "BETA: the launch controller-daemon (planner on omp) is no launch this reader knows",
    `BETA: the export has no launch ${LAUNCH}`,
  ]);
  const twice = exportWith((exported) => {
    const [acme] = exported.renders;
    if (acme !== undefined) acme.launches = [...acme.launches, ...structuredClone(acme.launches)];
  });
  expect(compare(twice, evidence, expected)).toMatchObject({
    problems: [`ACME: the export holds the launch ${LAUNCH} 2 times`],
    projects: [
      { project: "ACME", launches: 0, parts: 0 },
      { project: "BETA", launches: 1, parts: 2 },
    ],
  });
});

test("a part whose bytes or sha256 differ from the row fails, and so does a part out of order or place", () => {
  const bytes = exportWith((exported) => {
    const part = exported.renders[0]?.launches[0]?.parts[1];
    if (part !== undefined) part.bytes = 2831;
  });
  expect(compare(bytes, evidence, expected)).toMatchObject({
    problems: [
      `ACME ${LAUNCH} go/controller-headless.md: 2831 bytes in the export, 2830 in the row`,
    ],
    projects: [
      { project: "ACME", launches: 0, parts: 0 },
      { project: "BETA", launches: 1, parts: 2 },
    ],
  });
  const digest = exportWith((exported) => {
    const part = exported.renders[1]?.launches[0]?.parts[0];
    if (part !== undefined) part.sha256 = "0".repeat(64);
  });
  expect(problemsOf(digest)).toEqual([
    `BETA ${LAUNCH} roles/controller-root.md: sha256 ${"0".repeat(64)} in the export, c211980843f14c75d90bf17a1f82f54db66e0e6a2d878cddb4b78618e3fb3aac in the row`,
  ]);
  const reversed = exportWith((exported) => {
    exported.renders[0]?.launches[0]?.parts.reverse();
  });
  expect(problemsOf(reversed)).toEqual([
    `ACME ${LAUNCH}: the export's parts are go/controller-headless.md, roles/controller-root.md, and the row's are roles/controller-root.md, go/controller-headless.md`,
  ]);
  const missing = exportWith((exported) => {
    exported.renders[0]?.launches[0]?.parts.pop();
  });
  expect(problemsOf(missing)).toEqual([
    `ACME ${LAUNCH}: the export's parts are roles/controller-root.md, and the row's are roles/controller-root.md, go/controller-headless.md`,
  ]);
  const outside = exportWith((exported) => {
    const part = exported.renders[0]?.launches[0]?.parts[0];
    if (part !== undefined) part.path = "roles/controller-root.md";
  });
  expect(problemsOf(outside)).toEqual([
    `ACME ${LAUNCH}: the part roles/controller-root.md is not under packages/daemon/internal/prompts/`,
  ]);
});

test("rows or an export at any commit but the caller's head fail, and so does a head its branch had moved from", () => {
  expect(compare(sampleExport, { ...evidence, subjectHead: OTHER }, expected).problems).toEqual([
    `the rows were measured at ${OTHER} (composition.json), not ${HEAD}`,
  ]);
  const elsewhere = exportWith((exported) => {
    for (const read of exported.public) {
      read.requested = OTHER;
      read.commit = OTHER;
      read.branchHead = OTHER;
    }
    for (const render of exported.renders) render.publicCommit = OTHER;
  });
  expect(problemsOf(elsewhere)).toEqual([
    `the export read the public commit ${OTHER}, not ${HEAD}`,
    `ACME is rendered at ${OTHER}, not ${HEAD}`,
    `BETA is rendered at ${OTHER}, not ${HEAD}`,
  ]);
  expect(problemsOf(sampleExport, { ...expected, branch: "main" })).toEqual([
    `the export compared ${HEAD} with sample-branch, not main`,
  ]);
  const moved = exportWith((exported) => {
    for (const read of exported.public) {
      read.branchHead = OTHER;
      read.isBranchHead = false;
    }
  });
  expect(problemsOf(moved)).toEqual([
    `${HEAD} was not the head of sample-branch when the export read it (the head was ${OTHER})`,
  ]);
  const unresolved = exportWith((exported) => {
    for (const read of exported.public) read.commit = "";
  });
  expect(problemsOf(unresolved)).toEqual([`the export resolved ${HEAD} to nothing`]);
  const unread = exportWith((exported) => {
    exported.public = [];
  });
  expect(problemsOf(unread)).toEqual(["the export read no public commit"]);
});

test("an export that failed, a control it missed or a render with problems fails", () => {
  const failed = exportWith((exported) => {
    exported.outcome = "fail";
    exported.problems = ["control stale-revision: missed"];
    const control = exported.controls[2];
    if (control !== undefined) control.outcome = "missed";
  });
  expect(problemsOf(failed)).toEqual([
    'the export\'s outcome is "fail"',
    "the export reports 1 problems",
    "control stale-revision: missed",
  ]);
  const uncontrolled = exportWith((exported) => {
    exported.controls = [];
  });
  expect(problemsOf(uncontrolled)).toEqual(["the export carries no control"]);
  const missingReferenceControl = exportWith((exported) => {
    exported.controls = exported.controls.filter((control) => control.name !== "missing-reference");
  });
  expect(problemsOf(missingReferenceControl)).toEqual([
    "the export carries 0 missing-reference controls",
  ]);
  const troubled = exportWith((exported) => {
    const [acme] = exported.renders;
    if (acme !== undefined) acme.problems = ["a page the text names is gone"];
    const read = exported.public[0];
    if (read !== undefined) read.problems = ["listing a root failed"];
  });
  expect(problemsOf(troubled)).toEqual([
    `the export's read of ${HEAD} has 1 problems`,
    "ACME's render has 1 problems",
  ]);
});

test("only the schema prompt-render/2 reads; the keys it adds are left unread, and a null list is empty", () => {
  const raw = JSON.parse(exportText);
  expect(() => parseExport(JSON.stringify({ ...raw, schema: "prompt-render/1" }))).toThrow(
    'its schema is "prompt-render/1"; this reads prompt-render/2 alone'
  );
  expect(() => parseExport(JSON.stringify({ ...raw, schema: undefined }))).toThrow(
    "its schema is undefined"
  );
  const added = parseExport(JSON.stringify({ ...raw, experiments: [{ name: "memory" }] }));
  expect(compare(added, evidence, expected).problems).toEqual([]);
  const nulls = parseExport(JSON.stringify({ ...raw, renders: null, controls: null }));
  expect(problemsOf(nulls)).toEqual([
    "the export carries no control",
    "the export holds no gate render",
    "the export has no gate render of ACME",
    "the export has no gate render of BETA",
  ]);
  raw.renders[0].launches[0].parts = null;
  expect(problemsOf(parseExport(JSON.stringify(raw)))).toEqual([
    `ACME ${LAUNCH}: the export holds no part`,
  ]);
  expect(() => parseExport(JSON.stringify({ ...raw, renders: [{ project: "ACME" }] }))).toThrow();
});

test("a row holds every key of the Go row and no other", () => {
  expect(() => parseRows(rowText.replace('"total_bytes"', '"extra":1,"total_bytes"'))).toThrow(
    /^line 1: .*extra/
  );
  expect(() => parseRows(`${rowText}{"launch":"merger/omp"}\n`)).toThrow(/^line 2: /);
  expect(() => parseRows("not json\n")).toThrow(/^line 1: /);
});

test("each launch name of the render takes the rows' name, and a part loses the prompts directory", () => {
  const names = [
    ["controller-daemon", "controller", "omp", "controller-headless/omp"],
    ["controller-operator", "controller", "omp", "controller-interactive/omp"],
    ["architect-root", "architect", "omp", "architect-root/omp"],
    ["architect-child", "architect", "omp", "architect-child/omp"],
    ["planner-omp", "planner", "omp", "planner/omp"],
    ["planner-claude", "planner", "claude", "planner/claude"],
    ["implementer-omp", "implementer", "omp", "implementer/omp"],
    ["implementer-claude", "implementer", "claude", "implementer/claude"],
    ["tester-omp", "tester", "omp", "tester/omp"],
    ["tester-claude", "tester", "claude", "tester/claude"],
    ["reviewer-omp", "reviewer", "omp", "reviewer/omp"],
    ["reviewer-claude", "reviewer", "claude", "reviewer/claude"],
    ["merger-omp", "merger", "omp", "merger/omp"],
    ["merger-claude", "merger", "claude", "merger/claude"],
  ] as const;
  for (const [launch, role, backend, name] of names)
    expect(rowsLaunchName({ launch, role, backend })).toBe(name);
  expect(rowsLaunchName({ launch: "controller-daemon", role: "architect", backend: "omp" })).toBe(
    undefined
  );
  expect(rowsLaunchName({ launch: "planner-omp", role: "planner", backend: "claude" })).toBe(
    undefined
  );
  expect(rowsLaunchName({ launch: "planner", role: "planner", backend: "omp" })).toBe(undefined);
  expect(rowsLaunchName({ launch: "toString", role: "controller", backend: "omp" })).toBe(
    undefined
  );
  expect(rowsPartPath("packages/daemon/internal/prompts/roles/core/common.md")).toBe(
    "roles/core/common.md"
  );
  expect(rowsPartPath("packages/daemon/internal/prompts/go/claude-mechanics.md")).toBe(
    "go/claude-mechanics.md"
  );
  expect(rowsPartPath("roles/core/common.md")).toBe(undefined);
});

test("sizes prints the before and after bytes of each project and launch, and counts a before render's problems", () => {
  const before = exportWith((exported) => {
    for (const render of exported.renders) {
      render.private.label = "base";
      render.gate = false;
      render.publicCommit = OTHER;
    }
    const [acme, beta] = exported.renders;
    if (acme === undefined || beta === undefined) return;
    acme.deployment.bytes = 150;
    const launch = acme.launches[0];
    if (launch !== undefined) launch.assembled.bytes = 6674;
    beta.problems = ["a page the old text names is gone"];
    beta.memory = undefined;
  });
  expect(sizes(before, "base", sampleExport, expected)).toEqual({
    problems: [],
    table: [
      `Before: the private base at ${CHECKOUT}. After: the private main at ${CHECKOUT}, the public ${HEAD} on sample-branch.`,
      "",
      "| Project | Public before | Deployment bytes before | after | Memory bytes before | after | Required-read bytes before | after | Problems before |",
      "| --- | --- | --- | --- | --- | --- | --- | --- | --- |",
      "| ACME | d00ed2547ce5 | 150 | 97 | - | - | 0 | 0 | 0 |",
      "| BETA | d00ed2547ce5 | 97 | 97 | - | 57 | 0 | 0 | 1 |",
      "",
      "| Project | Launch | Role-part bytes before | after | Assembled bytes before, addressing left out | after |",
      "| --- | --- | --- | --- | --- | --- |",
      `| ACME | ${LAUNCH} | 6522 | 6522 | 6674 | 6621 |`,
      `| BETA | ${LAUNCH} | 6522 | 6522 | 6621 | 6621 |`,
      "",
    ].join("\n"),
  });
});

test("sizes fails where a side lacks its renders, a launch was not composed, or the after export is off the head", () => {
  expect(sizes(sampleExport, "base", sampleExport, expected).problems).toEqual([
    "before: the export holds no base-labelled render",
    "before: the export has no base-labelled render of ACME",
    "before: the export has no base-labelled render of BETA",
  ]);
  const uncomposed = exportWith((exported) => {
    const launch = exported.renders[0]?.launches[0];
    if (launch === undefined) return;
    launch.parts = [];
    launch.assembled = { bytes: 0, sha256: "" };
  });
  expect(sizes(uncomposed, "main", sampleExport, expected).problems).toEqual([
    `before: ACME ${LAUNCH}: the launch was not composed`,
  ]);
  const offHead = { ...expected, head: OTHER };
  expect(sizes(sampleExport, "main", sampleExport, offHead).problems).toEqual([
    `after: the export read the public commit ${HEAD}, not ${OTHER}`,
    `after: ACME is rendered at ${HEAD}, not ${OTHER}`,
    `after: BETA is rendered at ${HEAD}, not ${OTHER}`,
  ]);
});

// The command line, on files.

function run(argv: readonly string[]) {
  const out: string[] = [];
  const err: string[] = [];
  const status = main(argv, { out: (line) => out.push(line), err: (line) => err.push(line) });
  return { status, out, err };
}

function compareArgs(changed: Readonly<Record<string, string>> = {}): string[] {
  const flags: Record<string, string> = {
    "--export": exportFile,
    "--evidence": evidenceDir,
    "--head": HEAD,
    "--branch": "sample-branch",
    "--projects": "ACME,BETA",
    "--launches": LAUNCH,
    ...changed,
  };
  return ["compare", ...Object.entries(flags).flat()];
}

/** A file under the scratch directory holding `text`. */
function written(name: string, text: string): string {
  const file = path.join(scratch, name);
  mkdirSync(path.dirname(file), { recursive: true });
  writeFileSync(file, text);
  return file;
}

test("compare passes on the sample and prints what agreed and at which commits", () => {
  expect(run(compareArgs())).toEqual({
    status: 0,
    out: [
      "ok ACME: 1 launches, 2 parts agree with the rows",
      "ok BETA: 1 launches, 2 parts agree with the rows",
      `teardown-inputs: pass: the private main at ${CHECKOUT}, the public ${HEAD} on sample-branch`,
    ],
    err: [],
  });
});

test("compare fails with exit 1 on a missing project, a differing byte count or zero rows", () => {
  expect(run(compareArgs({ "--projects": "ACME,BETA,GAMMA" }))).toEqual({
    status: 1,
    out: ["problem: the export has no gate render of GAMMA", "teardown-inputs: fail, 1 problems"],
    err: [],
  });
  const raw = JSON.parse(exportText);
  raw.renders[1].launches[0].parts[0].bytes = 3692;
  const changed = written("bytes/export.json", JSON.stringify(raw));
  expect(run(compareArgs({ "--export": changed }))).toEqual({
    status: 1,
    out: [
      `problem: BETA ${LAUNCH} roles/controller-root.md: 3692 bytes in the export, 3693 in the row`,
      "teardown-inputs: fail, 1 problems",
    ],
    err: [],
  });
  written("empty/rows.jsonl", "");
  written("empty/composition.json", compositionText);
  expect(run(compareArgs({ "--evidence": path.join(scratch, "empty") }))).toEqual({
    status: 1,
    out: [
      "problem: the rows hold no launch",
      `problem: the rows hold no row of ${LAUNCH}`,
      "teardown-inputs: fail, 2 problems",
    ],
    err: [],
  });
});

test("an input that does not read is exit 1, and refused arguments are exit 2", () => {
  const missing = run(compareArgs({ "--export": path.join(scratch, "absent.json") }));
  expect(missing.status).toBe(1);
  expect(missing.err.join("\n")).toContain("ENOENT");
  const old = written("v1/export.json", exportText.replace(EXPORT_SCHEMA, "prompt-render/1"));
  expect(run(compareArgs({ "--export": old })).err).toEqual([
    `teardown-inputs: ${old}: its schema is "prompt-render/1"; this reads prompt-render/2 alone`,
  ]);
  expect(run(["compare"]).status).toBe(2);
  expect(run(["check"]).status).toBe(2);
  expect(run(compareArgs({ "--head": "ecd967b17a4e" })).err).toEqual([
    "teardown-inputs: --head ecd967b17a4e is not a 40-hex lower-case commit",
  ]);
  expect(run(compareArgs({ "--projects": "ACME,ACME" })).err).toEqual([
    "teardown-inputs: --projects names ACME twice",
  ]);
  expect(run(compareArgs({ "--launches": `${LAUNCH},` })).status).toBe(2);
  expect(run([...compareArgs(), "--before", exportFile]).status).toBe(2);
});

test("sizes prints its table on the sample read as both sides", () => {
  const result = run([
    "sizes",
    "--before",
    exportFile,
    "--before-private",
    "main",
    "--after",
    exportFile,
    "--head",
    HEAD,
    "--branch",
    "sample-branch",
    "--projects",
    "ACME,BETA",
    "--launches",
    LAUNCH,
  ]);
  expect(result.status).toBe(0);
  expect(result.out.join("\n")).toContain(`| BETA | ${LAUNCH} | 6522 | 6522 | 6621 | 6621 |`);
});
