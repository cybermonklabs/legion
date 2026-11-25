import { beforeAll, expect, setDefaultTimeout, test } from "bun:test";
import {
  cpSync,
  existsSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  renameSync,
  statSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { answerTurn, judgeRun } from "./judge";
import { FAULT_ROWS } from "./judge-prompts";
import { sharedRuns, workerReadings } from "./score";

// The rows run the rig's programs as real processes, which a loaded pod slows past bun's 5 s
// default, so each gets a minute, as rig.test.ts's do.
setDefaultTimeout(60_000);

// judge.ts gives each prompt a fresh directory under <work>/judging/<run>/ that holds only that
// prompt's inputs and prompt.txt, starts the judge there through an injected runner (rig.sh
// judge-omp in production), and copies the judge's validated answer into the run directory. The
// fake runner below records what each judge saw and writes a fixed answer. Every text is invented.

// Every run here is built from /checkouts/head, as its built-from records (rig.sh's cmd_profile),
// which is where judge.ts reads the checkouts a run's agent can reach (score.ts's `checkouts`).
const work = temporaryDirectory("skill-scenarios-judge");
/** A work directory of its own for the row whose runs directory holds an entry that is no run, so
 * no other row's runs sit beside it. */
const strayWork = temporaryDirectory("skill-scenarios-judge-stray");

const sessionId = "01a16640-8259-71ed-bb68-1e4927b77a01";
const spec = (problem: string) =>
  [
    "## Summary",
    "",
    "The widgets CLI greets a person by name.",
    "",
    "## Problem",
    "",
    problem,
    "",
    "## Goals and non-goals",
    "",
    "Goals: a greeting.",
    "",
    "## Requirements",
    "",
    "- **R1.** The CLI prints a greeting. From: your answer on the greeting.",
    "- **R2.** The greeting ends with a full stop. From: your message on the format.",
    "",
    "## Verification",
    "",
    "The checks are in this issue's verification.md.",
    "",
    "## Design",
    "",
    "The CLI prints one line.",
    "",
    "## Decisions",
    "",
    "### Plain text",
    "",
    "Plain text, because scripts read it.",
    "",
    "## Acceptance",
    "",
    "- **A1.** `greet Ada` prints one line. (R1, R2)",
    "",
  ].join("\n");
// Two passages of the person's input that the run's texts copy: one in the spec, one in a document.
const asked = "Please make the widgets command line print a friendly greeting for each person.";
const format = "Each greeting must end with a full stop so that our scripts can split the lines.";
const messages = [
  "<!-- message 2026-10-20T10:00:00Z -->",
  asked,
  "",
  "<!-- comment 2026-10-20T11:00:00Z anchored -->",
  "> The old spec said nothing about the format.",
  "",
  format,
  "",
].join("\n");
const v1 = spec("The CLI prints nothing.");
const v3 = spec(`The CLI prints nothing. ${asked}`);
const sourceAsks = [
  {
    id: "src-1",
    kind: "question",
    question: "Which output format?",
    options: [{ label: "Plain text" }, { label: "JSON" }],
    multiple: false,
    state: "answered",
    created_at: "2026-10-20T12:00:00Z",
    answer: {
      selected: ["Plain text"],
      text: "Plain text.",
      user: "alice",
      at: "2026-10-20T12:05:00Z",
    },
  },
];
const ask = (id: string, question: string, createdAt: string) => ({
  id,
  kind: "question",
  question,
  options: [{ label: "Ada" }, { label: "Grace", description: "The second name." }],
  multiple: false,
  state: "open",
  answer: null,
  created_at: createdAt,
});
/** An assistant turn at `at` whose one tool call is `tool` with `args`. */
const call = (at: string, id: string, tool: string, args: object) => ({
  type: "message",
  timestamp: at,
  message: { role: "assistant", content: [{ type: "toolCall", id, name: tool, arguments: args }] },
});
const answered = (at: string, id: string, tool: string, text: string, details: object = {}) => ({
  type: "message",
  timestamp: at,
  message: {
    role: "toolResult",
    toolName: tool,
    toolCallId: id,
    isError: false,
    details,
    content: [{ type: "text", text }],
  },
});
/** A doc-edit of the run's spec, answered as Dispatch answers one that made a version. */
const docEdit = (at: string, id: string, version: number) => [
  call(at, id, "bash", {
    command: "dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file ops.json",
  }),
  answered(at, id, "bash", `Applied 1 ops (version ${version})\nDocument token: sha256:abc`, {
    exitCode: 0,
  }),
];
const transcript = [
  { type: "session", id: sessionId, cwd: "/rig/cwd" },
  { type: "message", message: { role: "user", content: [{ type: "text", text: "Design it." }] } },
  // A check that names a path under the checkout: not a spec-only check.
  call("2026-10-24T10:00:00.000Z", "c0", "task", {
    context: `${v1}\nRead /checkouts/head/packages/cli/greet.ts too.`,
    tasks: [{ name: "CodeCheck", task: "Check it against the code." }],
  }),
  answered("2026-10-24T10:00:30.000Z", "c0", "task", "<task-result>The code agrees.</task-result>"),
  // A spec-only check that answers in its own result, as a blocking task does.
  call("2026-10-24T10:01:00.000Z", "c1", "task", {
    context: `Read this spec alone.\n\n${v1}`,
    tasks: [{ name: "SpecCheck1", task: "List each gap." }],
  }),
  // Its report quotes a closing `task-result` tag, which the agent being scored can ask its
  // checker for, since it writes the checker's prompt and Oh My Pi escapes no such tag. The
  // report runs to the envelope's own closing tag, so the finding after the quote is in it.
  answered(
    "2026-10-24T10:01:30.000Z",
    "c1",
    "task",
    '<task-result id="SpecCheck1" agent="task" status="completed" duration="30s">\n<output>\nR2 names no source.\nThe spec ends a line with `</task-result>`, which I quote here.\nR3 names no check.\n</output>\n</task-result>\n\nSpecCheck1 is now idle — message it via `write agent://SpecCheck1` to follow up; transcript at history://SpecCheck1'
  ),
  ...docEdit("2026-10-24T10:02:00.000Z", "w1", 2),
  // A spec-only check that runs in the background: its report arrives as a message of its own,
  // with the job's structured record as Oh My Pi writes it (a task job carries no resultText there).
  call("2026-10-24T10:04:00.000Z", "c2", "task", {
    context: `Read this spec alone.\n\n${v3}`,
    tasks: [{ name: "SpecCheck2", task: "List each gap." }],
  }),
  answered(
    "2026-10-24T10:04:01.000Z",
    "c2",
    "task",
    "Spawned agent `SpecCheck2` (job `SpecCheck2`).\n\nResults auto-deliver."
  ),
  {
    type: "custom_message",
    customType: "async-result",
    timestamp: "2026-10-24T10:05:00.000Z",
    details: {
      jobs: [
        {
          jobId: "SpecCheck2",
          type: "task",
          status: "completed",
          agentId: "SpecCheck2",
          label: "SpecCheck2",
          durationMs: 60000,
        },
      ],
    },
    content:
      '<system-notice>\nBackground job SpecCheck2 has completed.\n<task-result id="SpecCheck2" agent="task" status="completed">\nA1 tests no requirement alone.\n</task-result>\n</system-notice>',
  },
  ...docEdit("2026-10-24T10:06:00.000Z", "w2", 3),
];

/** A spec-writing run directory as rig.sh and seed.ts write one; `seeded-1` is a seeded ask that
 * capture-spec failed to leave out, so judge.ts must leave it out itself. */
function runDir(name: string, extra: Record<string, string> = {}, entries = transcript): string {
  const dir = path.join(work, "runs", name);
  for (const sub of ["sessions", "source", "documents", "spec-versions"])
    mkdirSync(path.join(dir, sub), { recursive: true });
  const files: Record<string, string> = {
    "built-from": "checkout /checkouts/head\n",
    "out.txt": "exit=0\n",
    "source.txt": "acme-1\n",
    "fixture.json": JSON.stringify({ issue: "LWEVAL-7", spec_artifact: "art-spec" }),
    "seeded.json": JSON.stringify(["seeded-1"]),
    "turns.txt": "1 first exit=0\n2 answered exit=0\n3 approved exit=0\n",
    "spec.md": v3,
    "spec-versions.json": JSON.stringify([
      {
        version: 1,
        created_at: "2026-10-24T10:02:01Z",
        authors: [{ kind: "session", id: sessionId }],
      },
      { version: 2, created_at: "2026-10-24T10:03:00Z", authors: [{ kind: "user", id: "alice" }] },
      {
        version: 3,
        created_at: "2026-10-24T10:06:01Z",
        authors: [{ kind: "session", id: sessionId }],
      },
    ]),
    "spec-versions/1.md": v1,
    "spec-versions/2.md": v1,
    "spec-versions/3.md": v3,
    "agent-asks.json": JSON.stringify([
      ask("ask-2", "Which second name? Recommendation: Grace.", "2026-10-24T10:07:00Z"),
      ask("seeded-1", "Which format did you want?", "2026-10-24T09:00:00Z"),
      ask("ask-1", "Which name? Recommendation: Ada.", "2026-10-24T10:03:00Z"),
      { id: "appr-1", kind: "approval", question: "Approve spec.md v3?", options: [] },
    ]),
    "agent-texts.json": JSON.stringify([
      { id: "msg-1", kind: "message", body: "The spec is ready.", created_at: "" },
      {
        id: "seeded-1",
        kind: "ask",
        body: "Which format did you want?\nPlain text",
        created_at: "",
      },
    ]),
    "state-1.json": JSON.stringify({
      turn: 1,
      open_asks: [
        {
          id: "ask-1",
          question: "Which name?",
          options: [{ label: "Ada" }, { label: "Grace" }],
          multiple: false,
        },
        {
          id: "ask-3",
          question: "Which city?",
          options: [{ label: "Oslo" }, { label: "Rome" }],
          multiple: false,
        },
      ],
      approval: null,
    }),
    "state-2.json": JSON.stringify({
      turn: 2,
      open_asks: [],
      approval: { ask_id: "appr-1", version: 3 },
    }),
    "state-3.json": JSON.stringify({ turn: 3, open_asks: [], approval: null }),
    "source/title.txt": "Greet a person\n",
    "source/messages.md": messages,
    "source/asks.json": JSON.stringify(sourceAsks),
    "documents/brief.md": `# Brief\n\n${format}\n`,
    ...extra,
  };
  for (const [file, text] of Object.entries(files)) writeFileSync(path.join(dir, file), text);
  writeFileSync(
    path.join(dir, "sessions", "2026-10-24T10-00-00-000Z_01a16640.jsonl"),
    entries.map((entry) => `${JSON.stringify(entry)}\n`).join("")
  );
  return dir;
}

/** Every file under `dir`, relative and sorted. */
function filesUnder(dir: string): string[] {
  return readdirSync(dir, { recursive: true, encoding: "utf8" })
    .filter((file) => statSync(path.join(dir, file)).isFile())
    .sort();
}
/** Every file's text under `dir`, joined. */
const textUnder = (dir: string) =>
  filesUnder(dir)
    .map((file) => readFileSync(path.join(dir, file), "utf8"))
    .join("\n");

interface Seen {
  profile: string;
  dir: string;
  output: string;
  files: string[];
  prompt: string;
  text: string;
}
/** A judge's fixed answer, by the base name of its directory. */
function answerFor(dir: string): [string, unknown] {
  const name = path.basename(dir).replace(/(-\d+)+$/, "");
  const inDir = (file: string) => readFileSync(path.join(dir, file), "utf8");
  const faults = Object.fromEntries(FAULT_ROWS.map((row) => [row, 0]));
  const answers: Record<string, () => [string, unknown]> = {
    faults: () => ["faults.json", { faults, ste_breaks: 1, items: [] }],
    decisions: () => ["decisions.json", { rows: [] }],
    findings: () => [
      "findings.json",
      {
        rows: [
          { check: 1, finding: "R2 names no source.", verdict: "fixed" },
          { check: 2, finding: "A1 tests no requirement alone.", verdict: "neither" },
        ],
      },
    ],
    checkable: () => [
      "checkable.json",
      {
        rows: [
          { requirement: "R1", check: "Run greet.", uncheckable: false },
          { requirement: "R2", check: "Read the last character.", uncheckable: false },
        ],
      },
    ],
    questions: () => [
      "questions.json",
      {
        rows: ["ask-1", "ask-2"].map((id) => ({
          id,
          dominated: false,
          settled: false,
          kind: "taste",
          reason: "A name is taste.",
        })),
      },
    ],
    numbers: () => ["numbers.json", { rows: [{ id: "msg-1", names_number: false, quote: null }] }],
    preamble: () => ["preamble.json", { rows: [] }],
    proposal: () => ["proposal.json", { rows: [] }],
    "shared-runs": () => [
      "shared-run.json",
      { run: inDir("run.txt").trim(), verdict: "copies", reason: "It copies the message." },
    ],
    answer: () => [
      "answer.json",
      {
        selected: [inDir("ask.json").includes("Oslo") ? "Oslo" : "Grace"],
        text: "The input says so.",
      },
    ],
  };
  const found = answers[name];
  if (found === undefined) throw new Error(`no answer for ${dir}`);
  return found();
}
function fakeRunner(seen: Seen[], answer: (dir: string) => [string, unknown] = answerFor) {
  return async (profile: "ste" | "bare", dir: string, output: string) => {
    const prompt = readFileSync(path.join(dir, "prompt.txt"), "utf8");
    seen.push({ profile, dir, output, files: filesUnder(dir), prompt, text: textUnder(dir) });
    const [file, body] = answer(dir);
    writeFileSync(path.join(dir, file), JSON.stringify(body));
    return 0;
  };
}

/** The run the judges below read, staged and judged in a hook: the module body writes nothing,
 *  so a run that selects no row of this file leaves nothing behind. */
let judged = "";
const seen: Seen[] = [];
beforeAll(async () => {
  judged = runDir("spec-writing-head-1");
  await judgeRun(judged, { runner: fakeRunner(seen) });
});
// The judges of one run start in parallel, so their order is their directories' order.
const byPrompt = (name: string) =>
  seen
    .filter((s) => path.basename(s.dir).replace(/(-\d+)+$/, "") === name)
    .sort((a, b) => a.dir.localeCompare(b.dir, "en", { numeric: true }));
const only = (name: string): Seen => {
  const [first, ...rest] = byPrompt(name);
  if (first === undefined || rest.length > 0)
    throw new Error(`${name} ran ${byPrompt(name).length} times`);
  return first;
};

test("each judge's working directory holds only its prompt's inputs and prompt.txt", () => {
  const expected: Record<string, string[]> = {
    faults: ["faults.md", "prompt.txt", "spec.md"],
    decisions: ["prompt.txt", "spec.md"],
    findings: [
      "check-1/assignment.md",
      "check-1/report.md",
      "check-1/spec-after.md",
      "check-2/assignment.md",
      "check-2/report.md",
      "check-2/spec-after.md",
      "prompt.txt",
    ],
    checkable: ["prompt.txt", "spec.md"],
    questions: ["agent-asks.json", "asks.json", "messages.md", "prompt.txt"],
    numbers: ["agent-texts.json", "prompt.txt"],
    preamble: ["agent-texts.json", "documents/brief.md", "messages.md", "prompt.txt", "spec.md"],
    proposal: ["agent-asks.json", "prompt.txt", "spec.md"],
  };
  for (const [name, files] of Object.entries(expected)) {
    const judge = only(name);
    expect(judge.dir).toBe(path.join(work, "judging", "spec-writing-head-1", name));
    expect(judge.files).toEqual(files);
    expect(judge.profile).toBe("ste");
  }
  // The faults judge gets the five rows of the fault table.
  for (const row of FAULT_ROWS) expect(only("faults").text).toContain(row);
});

test("each prompt names the one file the judge writes its answer to", () => {
  for (const judge of seen) {
    const [file] = answerFor(judge.dir);
    expect(judge.prompt).toContain(path.join(judge.dir, file));
  }
});

// rig.sh judge-omp mounts only the output file and sessions/ writable, so the runner gets the file.
test("the runner is told the one file the judge may write", async () => {
  for (const judge of seen) expect(judge.output).toBe(answerFor(judge.dir)[0]);
  const asked: Seen[] = [];
  await answerTurn(runDir("spec-writing-head-6"), 1, { runner: fakeRunner(asked) });
  expect(asked.map((judge) => judge.output)).toEqual(["answer.json", "answer.json"]);
});

// Catches: an archived run judged as if its archive were the work directory. rig.sh moves every
// run that has ended to <work>/archive/runs (its archive_run), and the judges' directories are
// the work directory's own (score.ts's `workOfRuns`).
test("a run in the archive is judged in the work directory's own judging directory", async () => {
  const ran = runDir("spec-writing-head-30");
  const archived = path.join(work, "archive", "runs", "spec-writing-head-30");
  mkdirSync(path.dirname(archived), { recursive: true });
  renameSync(ran, archived);
  const asked: Seen[] = [];
  await answerTurn(archived, 1, { runner: fakeRunner(asked) });
  expect(asked.length).toBeGreaterThan(0);
  for (const judge of asked)
    expect(path.dirname(judge.dir)).toBe(path.join(work, "judging", "spec-writing-head-30"));
});

// Dispatch writes `created_at` as Go does, with no trailing zeros: `…:00.5Z` is earlier than
// `…:00.51Z`, though it sorts after it as text.
test("questions gets the asks in time order at full precision", async () => {
  const dir = runDir("spec-writing-head-7", {
    "agent-asks.json": JSON.stringify([
      ask("ask-late", "Which second name?", "2026-10-24T10:07:00.51Z"),
      ask("ask-early", "Which name?", "2026-10-24T10:07:00.5Z"),
    ]),
  });
  const seenHere: Seen[] = [];
  await judgeRun(dir, {
    runner: fakeRunner(seenHere, (judge) =>
      path.basename(judge) === "questions"
        ? [
            "questions.json",
            {
              rows: ["ask-early", "ask-late"].map((id) => ({
                id,
                dominated: false,
                settled: false,
                kind: "taste",
                reason: "Taste.",
              })),
            },
          ]
        : answerFor(judge)
    ),
  });
  const questions = seenHere.find((judge) => path.basename(judge.dir) === "questions");
  const asks = JSON.parse(readFileSync(path.join(questions?.dir ?? "", "agent-asks.json"), "utf8"));
  expect(asks.map((a: { id: string }) => a.id)).toEqual(["ask-early", "ask-late"]);
});

test("the inputs of questions, numbers and preamble hold no seeded ask", () => {
  for (const name of ["questions", "numbers", "preamble", "proposal"])
    expect(only(name).text).not.toContain("seeded-1");
  // The rest of the run's asks reach the questions judge, oldest first, the approval request not.
  const asks = JSON.parse(
    readFileSync(path.join(only("questions").dir, "agent-asks.json"), "utf8")
  );
  expect(asks.map((a: { id: string }) => a.id)).toEqual(["ask-1", "ask-2"]);
});

test("findings reads each spec-only check's assignment and report and the version written after it", () => {
  const dir = only("findings").dir;
  const read = (file: string) => readFileSync(path.join(dir, file), "utf8");
  expect(read("check-1/assignment.md")).toContain(v1);
  expect(read("check-1/report.md")).toContain("R2 names no source.");
  // The report runs to the envelope's closing tag, not to a tag the report itself quotes, and it
  // holds none of the line Oh My Pi writes after that tag.
  expect(read("check-1/report.md")).toContain("R3 names no check.");
  expect(read("check-1/report.md")).not.toContain("is now idle");
  expect(read("check-1/spec-after.md")).toBe(v1);
  expect(read("check-2/report.md")).toContain("A1 tests no requirement alone.");
  // Version 2 is the person's: the agent's write after the second check made version 3.
  expect(read("check-2/spec-after.md")).toBe(v3);
  expect(only("findings").text).not.toContain("/checkouts/head/packages");
});

test("checkable reads the approved version of the spec alone", () => {
  expect(readFileSync(path.join(only("checkable").dir, "spec.md"), "utf8")).toBe(v3);
  const out = JSON.parse(readFileSync(path.join(judged, "checkable.json"), "utf8"));
  expect(out.version).toBe(3);
});

test("the shared-runs judge runs once for each shared run, with no skills, on the run, its context and the rule", () => {
  const runs = sharedRuns(judged);
  expect(runs.length).toBe(2);
  const judges = byPrompt("shared-runs");
  expect(judges.length).toBe(runs.length);
  for (const [index, judge] of judges.entries()) {
    expect(judge.profile).toBe("bare");
    expect(judge.dir).toBe(
      path.join(work, "judging", "spec-writing-head-1", `shared-runs-${index + 1}`)
    );
    expect(judge.files).toEqual(["context.md", "prompt.txt", "rule.md", "run.txt"]);
    expect(readFileSync(path.join(judge.dir, "run.txt"), "utf8").trim()).toBe(
      runs[index]?.run ?? ""
    );
    expect(judge.text).not.toContain("seeded-1");
  }
  const out = JSON.parse(readFileSync(path.join(judged, "shared-runs.json"), "utf8"));
  expect(out.rows).toEqual(
    runs.map((run) => ({
      source: run.source,
      run: run.run,
      verdict: "copies",
      reason: "It copies the message.",
    }))
  );
  expect(runs.map((run) => run.source)).toEqual(["spec.md", "documents/brief.md"]);
});

test("every judge output reaches the run directory", () => {
  for (const file of [
    "faults.json",
    "decisions.json",
    "findings.json",
    "checkable.json",
    "questions.json",
    "numbers.json",
    "preamble.json",
    "proposal.json",
    "shared-runs.json",
  ])
    expect(existsSync(path.join(judged, file))).toBe(true);
  expect(seen.length).toBe(8 + 2);
});

test("a judge that writes no answer, or one that does not parse, fails the run naming the file", async () => {
  const silent = fakeRunner([], (judge) =>
    path.basename(judge) === "decisions" ? ["other.json", {}] : answerFor(judge)
  );
  await expect(judgeRun(runDir("spec-writing-head-2"), { runner: silent })).rejects.toThrow(
    path.join(work, "judging", "spec-writing-head-2", "decisions", "decisions.json")
  );
  // A run of its own: the judging above left its judge directories, which a judging refuses.
  const wrong = fakeRunner([], (judge) =>
    path.basename(judge) === "numbers"
      ? ["numbers.json", { rows: [{ id: "msg-1" }] }]
      : answerFor(judge)
  );
  await expect(judgeRun(runDir("spec-writing-head-20"), { runner: wrong })).rejects.toThrow(
    `${path.join(work, "judging", "spec-writing-head-20", "numbers", "numbers.json")} does not parse`
  );
});

// Each judge's answer must cover exactly what judge.ts gave it: a judge that leaves out, repeats or
// invents an id fails the run, naming its file, as a judge that writes no answer does.
test("a judge whose answer does not cover its input fails the run naming the file", async () => {
  // Each faulty judge is given a run of its own, since a judging refuses the judge directories an
  // earlier one left.
  const faulty: Record<string, [string, unknown]> = {
    // The approved spec, v3, holds R1 and R2.
    checkable: [
      "checkable.json",
      { rows: [{ requirement: "R1", check: "Run greet.", uncheckable: false }] },
    ],
    questions: [
      "questions.json",
      {
        rows: ["ask-1", "ask-1", "ask-2", "ask-9"].map((id) => ({
          id,
          dominated: false,
          settled: false,
          kind: "taste",
          reason: "Taste.",
        })),
      },
    ],
    numbers: ["numbers.json", { rows: [{ id: "seeded-1", names_number: false, quote: null }] }],
    proposal: ["proposal.json", { rows: [{ item: "a name", verdict: "asked", ask: "seeded-1" }] }],
    preamble: [
      "preamble.json",
      { rows: [{ source: "documents/other.md", passage: "You said.", kind: "retold" }] },
    ],
    findings: ["findings.json", { rows: [{ check: 3, finding: "No owner.", verdict: "fixed" }] }],
  };
  const expected: Record<string, string[]> = {
    checkable: ["no row for requirement R2"],
    questions: ["two or more rows for ask ask-1", "rows for ask it was not given: ask-9"],
    numbers: ["no row for text msg-1", "rows for text it was not given: seeded-1"],
    proposal: ["rows for ask it was not given: seeded-1"],
    preamble: ["rows for source it was not given: documents/other.md"],
    findings: ["rows for check it was not given: 3"],
  };
  for (const [index, [name, answer]] of Object.entries(faulty).entries()) {
    const run = `spec-writing-head-8${index}`;
    const runner = fakeRunner([], (judge) =>
      path.basename(judge) === name ? answer : answerFor(judge)
    );
    const file = path.join(work, "judging", run, name, answer[0]);
    const failed = await judgeRun(runDir(run), { runner }).then(
      () => "",
      (error: Error) => error.message
    );
    expect(failed).toContain(`${file} does not cover what judge.ts gave its judge: `);
    for (const problem of expected[name] ?? []) expect(failed).toContain(problem);
  }
});

// A run whose transcript holds no spec-only check gives the findings judge nothing to read: no
// findings judge runs, and findings.json has no row.
test("a run with no spec-only check runs no findings judge and writes findings.json with no row", async () => {
  const unchecked = transcript.filter(
    (entry) =>
      !JSON.stringify(entry).includes('"c1"') &&
      !JSON.stringify(entry).includes('"c2"') &&
      entry.type !== "custom_message"
  );
  const dir = runDir("spec-writing-head-9", {}, unchecked);
  const ran: Seen[] = [];
  await judgeRun(dir, { runner: fakeRunner(ran) });
  expect(ran.map((judge) => path.basename(judge.dir))).not.toContain("findings");
  expect(JSON.parse(readFileSync(path.join(dir, "findings.json"), "utf8"))).toEqual({
    rows: [],
    clean: [],
  });
});

// A check the agent started from an eval cell has no `task` call in the transcript, only a transcript
// of its own beside the session's. The findings judge reads it as it reads a `task` call's check.
test("a check started from an eval cell reaches the findings judge, from its own transcript", async () => {
  const unchecked = transcript.filter(
    (entry) =>
      !JSON.stringify(entry).includes('"c1"') &&
      !JSON.stringify(entry).includes('"c2"') &&
      entry.type !== "custom_message"
  );
  const name = "spec-writing-head-10";
  const subagents = path.join(work, "runs", name, "sessions", "2026-10-24T10-00-00-000Z_01a16640");
  mkdirSync(subagents, { recursive: true });
  // It started after the first write (10:02:00) and reported before the second (10:06:00).
  const message = (at: string, role: string, text: string) => ({
    type: "message",
    timestamp: at,
    message: { role, content: [{ type: "text", text }] },
  });
  writeFileSync(
    path.join(subagents, "EvalCheck.jsonl"),
    [
      { type: "session", id: "01a16641", cwd: "/rig/cwd" },
      message("2026-10-24T10:04:00.000Z", "user", `Read this spec alone.\n\n${v3}`),
      message("2026-10-24T10:05:00.000Z", "assistant", "R2 names no owner."),
    ]
      .map((entry) => `${JSON.stringify(entry)}\n`)
      .join("")
  );
  const dir = runDir(name, {}, unchecked);
  const ran: Seen[] = [];
  // One check, so the findings answer names check 1 alone.
  const answer = (judgeDir: string): [string, unknown] =>
    path.basename(judgeDir) === "findings"
      ? ["findings.json", { rows: [{ check: 1, finding: "R2 names no owner.", verdict: "fixed" }] }]
      : answerFor(judgeDir);
  await judgeRun(dir, { runner: fakeRunner(ran, answer) });
  const findings = ran.find((judge) => path.basename(judge.dir) === "findings");
  expect(findings?.files).toEqual([
    "check-1/assignment.md",
    "check-1/report.md",
    "check-1/spec-after.md",
    "prompt.txt",
  ]);
  const read = (file: string) => readFileSync(path.join(findings?.dir ?? "", file), "utf8");
  expect(read("check-1/assignment.md")).toContain(v3);
  expect(read("check-1/report.md")).toBe("R2 names no owner.\n");
  expect(read("check-1/spec-after.md")).toBe(v3);
});

test("answer runs the stand-in once for each open ask and writes its answers", async () => {
  const dir = runDir("spec-writing-head-3");
  const asked: Seen[] = [];
  await answerTurn(dir, 1, { runner: fakeRunner(asked) });
  expect(asked.map((judge) => path.basename(judge.dir)).sort()).toEqual([
    "answer-1-1",
    "answer-1-2",
  ]);
  for (const judge of asked) {
    expect(judge.profile).toBe("ste");
    expect(judge.files).toEqual(["ask.json", "asks.json", "messages.md", "prompt.txt"]);
  }
  expect(readFileSync(path.join(dir, "answers-1.jsonl"), "utf8")).toBe(
    [
      JSON.stringify({ ask: "ask-1", selected: ["Grace"], text: "The input says so." }),
      JSON.stringify({ ask: "ask-3", selected: ["Oslo"], text: "The input says so." }),
      "",
    ].join("\n")
  );
});

test("answer refuses a label the ask does not offer, and writes an empty file when no ask is open", async () => {
  const dir = runDir("spec-writing-head-4");
  const invented = fakeRunner([], () => ["answer.json", { selected: ["Paris"], text: "No." }]);
  await expect(answerTurn(dir, 1, { runner: invented })).rejects.toThrow(
    `${path.join(work, "judging", "spec-writing-head-4", "answer-1-1", "answer.json")} does not parse`
  );
  const none: Seen[] = [];
  await answerTurn(dir, 3, { runner: fakeRunner(none) });
  expect(none).toEqual([]);
  expect(readFileSync(path.join(dir, "answers-3.jsonl"), "utf8")).toBe("");
});

// fetch-spec-sources.ts writes `>` lines for an anchored comment's quote alone, at the start of the
// comment's text, and marks that comment's header `anchored`. Any other `>` line, such as a block
// quote in a message or one an unanchored comment starts with, is the person's words.
test("each prompt that reads messages.md says only an anchored comment's leading `>` lines quote the spec", async () => {
  const asked: Seen[] = [];
  await answerTurn(runDir("spec-writing-head-5"), 1, { runner: fakeRunner(asked) });
  const prompts = [...asked, only("questions"), only("preamble")].map((judge) => judge.prompt);
  expect(prompts).toHaveLength(4);
  for (const prompt of prompts) {
    expect(prompt).toContain(
      "The `>` lines directly after a `<!-- comment <time> anchored -->` line quote the spec that the comment is about. They are not the person's words. Every other line is the person's words."
    );
    expect(prompt).not.toContain("A line that starts with `>` quotes the spec");
    expect(prompt).not.toContain("directly after a `<!-- comment <time> -->` line");
  }
});

// judge.ts removes and replaces nothing an earlier judging left: a second judging of a run, or of
// a turn, is refused before any judge starts, naming what is there, and each of it is kept as it
// was. `rig.sh judge` run twice on one run had its first judging's directories removed and its
// answers written over.
test("a second judging of a run is refused before any judge starts, and the first judging is kept", async () => {
  const dir = runDir("spec-writing-head-41");
  await judgeRun(dir, { runner: fakeRunner([]) });
  const faults = path.join(dir, "faults.json");
  const judge = path.join(work, "judging", "spec-writing-head-41", "faults");
  const [kept, keptJudge] = [readFileSync(faults, "utf8"), readdirSync(judge).sort()];
  writeFileSync(path.join(judge, "notes.md"), "read before judging again\n");
  const again: Seen[] = [];
  const refused = await judgeRun(dir, { runner: fakeRunner(again) }).then(
    () => "",
    (error: Error) => error.message
  );
  expect(refused).toStartWith(`judge.ts: ${faults}, `);
  expect(refused).toContain(`, ${judge}, `);
  expect(refused).toEndWith(
    " from an earlier judging; judge.ts removes and replaces none of it: move it aside to judge again"
  );
  expect(again).toEqual([]);
  expect(readFileSync(faults, "utf8")).toBe(kept);
  expect(readdirSync(judge).sort()).toEqual([...keptJudge, "notes.md"].sort());
});

test("a second answer of a turn is refused before any judge starts, and the first answers are kept", async () => {
  const dir = runDir("spec-writing-head-42");
  await answerTurn(dir, 1, { runner: fakeRunner([]) });
  const answers = path.join(dir, "answers-1.jsonl");
  const kept = readFileSync(answers, "utf8");
  const again: Seen[] = [];
  await expect(answerTurn(dir, 1, { runner: fakeRunner(again) })).rejects.toThrow(
    `judge.ts: ${answers}, ${path.join(work, "judging", "spec-writing-head-42", "answer-1-1")}, `
  );
  expect(again).toEqual([]);
  expect(readFileSync(answers, "utf8")).toBe(kept);
});

// An entry beside a run that is neither a run nor its record (score.ts's `runsOf`) is no state the
// rig writes, and judging past it would read the reach of a smaller archive than the one given:
// `judge.ts run` names it on stderr and exits 2 before any judge starts, as `rig.sh score` does.
// Catches: judge.ts letting the refusal through as an uncaught error (exit 1, a stack trace).
test("a run beside an entry that is no run is not judged, and judge.ts exits 2 naming the entry", () => {
  const other = path.join(strayWork, "runs");
  const run = path.join(other, "spec-writing-head-46");
  cpSync(runDir("spec-writing-head-46"), run, { recursive: true });
  const stray = path.join(other, "stray");
  symlinkSync(run, stray);
  const judged = Bun.spawnSync(["bun", path.join(import.meta.dir, "judge.ts"), "run", run], {
    env: { ...process.env, SKILL_SCENARIOS_WORK: path.dirname(other) },
  });
  expect({
    code: judged.exitCode,
    stdout: judged.stdout.toString(),
    stderr: judged.stderr.toString(),
  }).toEqual({
    code: 2,
    stdout: "",
    stderr: `judge: ${stray} is a symlink, neither a run directory nor a run's record (<id>.json)\n`,
  });
  expect(existsSync(path.join(path.dirname(other), "judging"))).toBe(false);
});

// A judge directory that appears after the check, while an earlier judge of the judging runs, is
// neither entered nor replaced: the judging fails on it, and it keeps what it held.
test("a judge directory that appears while the judging runs is kept, and the judging fails", async () => {
  const dir = runDir("spec-writing-head-43");
  const later = path.join(work, "judging", "spec-writing-head-43", "answer-1-2");
  const runner = fakeRunner([], (judge) => {
    if (path.basename(judge) === "answer-1-1") {
      mkdirSync(later);
      writeFileSync(path.join(later, "notes.md"), "not this judging's\n");
    }
    return answerFor(judge);
  });
  await expect(answerTurn(dir, 1, { runner, parallel: 1 })).rejects.toThrow(later);
  expect(readdirSync(later)).toEqual(["notes.md"]);
  expect(existsSync(path.join(dir, "answers-1.jsonl"))).toBe(false);
});

// An answer file that appears in the run directory while the judges run is not written over.
test("an answer file that appears while the judges run is kept, and the judging fails naming it", async () => {
  const turn = runDir("spec-writing-head-44");
  const answers = path.join(turn, "answers-1.jsonl");
  const planting = (file: string) =>
    fakeRunner([], (judge) => {
      if (!existsSync(file)) writeFileSync(file, "not this judging's\n");
      return answerFor(judge);
    });
  await expect(answerTurn(turn, 1, { runner: planting(answers) })).rejects.toThrow(answers);
  expect(readFileSync(answers, "utf8")).toBe("not this judging's\n");
  const run = runDir("spec-writing-head-45");
  const shared = path.join(run, "shared-runs.json");
  await expect(judgeRun(run, { runner: planting(shared) })).rejects.toThrow(shared);
  expect(readFileSync(shared, "utf8")).toBe("not this judging's\n");
});

/** The commit a worker run's world stands at: the reviewer's own commit, which threads-closed's
 * implementer reads as its last chance. */
const reviewerCommit = "099b05dc19f7f7e5058e791d16ae05f0b123a2cc";
/** A run of a worker scenario, as rig.sh's worker runs leave one once its results are captured:
 * its world, its threads and their routes, the gh stand-in's calls, the messages Envoy accepted on
 * its role topics, the results of its check, and its agent's transcript, by the session
 * `sessionId`. `files` is written over these. */
function workerRun(name: string, files: Record<string, string> = {}): string {
  const dir = path.join(work, "runs", name);
  mkdirSync(path.join(dir, "sessions"), { recursive: true });
  const published = (...lines: object[]) =>
    [
      { at: "2026-09-30T17:20:00.000Z", recording: "legion-skills1-lweval-1-" },
      ...lines,
      { at: "2026-09-30T17:50:00.000Z", closed: "stopped" },
    ]
      .map((line) => `${JSON.stringify(line)}\n`)
      .join("");
  const written: Record<string, string> = {
    "world.json": JSON.stringify({
      key: "LWEVAL-1",
      repo: "fernbrook/widgets",
      pr: 1001,
      branch: "legion/LWEVAL-1",
      base: "main",
      head: reviewerCommit,
    }),
    "fixtures.json": JSON.stringify({ routes: [] }),
    "threads.json": "[]",
    "calls.jsonl": `${JSON.stringify({
      at: "2026-09-30T17:31:00.000Z",
      as: "gh",
      by: "implementer",
      argv: ["pr", "comment", "1001", "--body", `Read ${reviewerCommit.slice(0, 12)}.`],
      exit: 0,
    })}\n`,
    "published.jsonl": published({
      at: "2026-09-30T17:32:00.000Z",
      topic: "notifications.role.legion-skills1-lweval-1-architect",
      message: `I accept ${reviewerCommit.slice(0, 12)}: check k1 still holds.`,
      sender: sessionId,
    }),
    "results.json": JSON.stringify([
      {
        check: "k1",
        commit: "1".repeat(40),
        session: "01a1b402-1f7d-7c55-8a3e-2b6d9f04c812",
        role: "tester",
        verdict: "pass",
        observed: "prints Hello, Ada! and exits 0",
        at: "2026-09-30T17:10:00.000001Z",
      },
    ]),
    [`sessions/2026-09-30T14-51-17-651Z_${sessionId}.jsonl`]: `${JSON.stringify({
      type: "session",
      id: sessionId,
      cwd: "/rig/cwd",
    })}\n`,
    ...files,
  };
  for (const [file, text] of Object.entries(written)) writeFileSync(path.join(dir, file), text);
  return dir;
}
/** The answer of a worker judge that reads its own inputs: a row for each text, and for each
 * thing where it was given things; the architect message accepts the commit, and the testing judge
 * reads it as sending the issue back to testing. */
function workerAnswer(dir: string): [string, unknown] {
  const texts = JSON.parse(readFileSync(path.join(dir, "texts.json"), "utf8")) as { id: string }[];
  if (path.basename(dir) === "accepts")
    return [
      "accepts.json",
      { rows: texts.map(({ id }) => ({ id, accepts: id === "message-1", quote: null })) },
    ];
  if (path.basename(dir) === "testing")
    return [
      "testing.json",
      {
        rows: texts.map(({ id }) => ({
          id,
          testing: id === "message-1",
          quote: id === "message-1" ? "check k1 still holds" : null,
        })),
      },
    ];
  const givens = JSON.parse(readFileSync(path.join(dir, "given.json"), "utf8")) as { id: string }[];
  return [
    "names.json",
    {
      rows: texts.flatMap(({ id }) =>
        givens.map((given) => ({ id, given: given.id, names: false, quote: null }))
      ),
    },
  ];
}

// Catches: a worker run judged as a spec run, or not at all; a judge given a text another session
// wrote, given no comment of the run's, or given more than its prompt's inputs; an earlier result
// read as the run's own text rather than as a thing a text may name; the testing reading k3's
// crossing (b) asks never asked, or asked of no text of the run's; and an answer that is not the
// judge's own copied into the run directory, where the scorer reads it by the text's id.
test("a threads-closed run's three judges read its agents' texts, against the reviewer's commit and the things they may name", async () => {
  const dir = workerRun("threads-closed-accept-head-1");
  const ran: Seen[] = [];
  await judgeRun(dir, { runner: fakeRunner(ran, workerAnswer) });
  const judge = (name: string) => ran.find((one) => path.basename(one.dir) === name);
  expect(
    ran.map(({ profile, dir: at, output, files }) => [profile, path.basename(at), output, files])
  ).toEqual([
    ["bare", "accepts", "accepts.json", ["commit.txt", "prompt.txt", "texts.json"]],
    ["bare", "names", "names.json", ["given.json", "prompt.txt", "texts.json"]],
    ["bare", "testing", "testing.json", ["prompt.txt", "texts.json"]],
  ]);
  expect(judge("testing")?.text).toContain(
    "whether the text says that the issue goes back to testing"
  );
  expect(readFileSync(path.join(judge("testing")?.dir ?? "", "texts.json"), "utf8")).toBe(
    readFileSync(path.join(judge("names")?.dir ?? "", "texts.json"), "utf8")
  );
  const texts = JSON.parse(
    readFileSync(path.join(judge("names")?.dir ?? "", "texts.json"), "utf8")
  );
  expect(texts).toEqual([
    {
      id: "message-1",
      kind: "message",
      body: `I accept ${reviewerCommit.slice(0, 12)}: check k1 still holds.`,
    },
    { id: "comment-1", kind: "comment", body: `Read ${reviewerCommit.slice(0, 12)}.` },
  ]);
  expect(readFileSync(path.join(judge("accepts")?.dir ?? "", "commit.txt"), "utf8")).toBe(
    `${reviewerCommit}\n`
  );
  const givens = JSON.parse(
    readFileSync(path.join(judge("names")?.dir ?? "", "given.json"), "utf8")
  );
  expect(givens.map((given: { id: string; kind: string }) => `${given.kind} ${given.id}`)).toEqual([
    "check check",
    "file file:docs/cli.md",
    "file file:.github/workflows/greet.yml",
    "result earlier-1",
  ]);
  for (const name of ["accepts", "names", "testing"])
    expect(readFileSync(path.join(dir, `${name}.json`), "utf8")).toBe(
      readFileSync(path.join(judge(name)?.dir ?? "", `${name}.json`), "utf8")
    );
  // The scorer reads the testing judge's answer by the text's id, as it reads the accepts judge's.
  const readings = workerReadings(dir, "threads-closed");
  expect({
    testing: readings.testing.get("message-1"),
    comment: readings.testing.get("comment-1")?.testing,
  }).toEqual({
    testing: { id: "message-1", testing: true, quote: "check k1 still holds" },
    comment: false,
  });
});

// Catches: a judge started where it has nothing to read, which spends a model session on it, and
// an answer file left unwritten for it, which the score reads as a rig error.
test("a reviewer-text-findings run runs no accepts or testing judge, and a run whose agents wrote nothing runs none", async () => {
  const reviewer = workerRun("reviewer-text-findings-text-only-head-1");
  const ran: Seen[] = [];
  await judgeRun(reviewer, { runner: fakeRunner(ran, workerAnswer) });
  expect(ran.map((one) => path.basename(one.dir))).toEqual(["names"]);
  for (const name of ["accepts", "testing"])
    expect(JSON.parse(readFileSync(path.join(reviewer, `${name}.json`), "utf8"))).toEqual({
      rows: [],
    });
  const quiet = workerRun("threads-closed-accept-head-2", {
    "calls.jsonl": "",
    "published.jsonl": "",
  });
  const none: Seen[] = [];
  await judgeRun(quiet, { runner: fakeRunner(none, workerAnswer) });
  expect({
    ran: none.length,
    accepts: JSON.parse(readFileSync(path.join(quiet, "accepts.json"), "utf8")),
    names: JSON.parse(readFileSync(path.join(quiet, "names.json"), "utf8")),
    testing: JSON.parse(readFileSync(path.join(quiet, "testing.json"), "utf8")),
  }).toEqual({ ran: 0, accepts: { rows: [] }, names: { rows: [] }, testing: { rows: [] } });
});
