import { describe, expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { OWNER_WALK_STAGES, ownerWalkWorld, questionsOf } from "./owner-walk";
import { cells, ownerWalk, reachOf } from "./scored-runs";
import {
  type OwnerWalkWorld,
  type PlansAnswerSource,
  RunRow,
  runPass,
  type ScoreClause,
} from "./scored-runs-contracts";

// Each row plants a run directory as rig.sh, seed.ts, dispatch-proxy.ts and Oh My Pi leave one,
// and scores it. No row reads a world text's words: a row plants a marker and finds it.

const fresh = temporaryDirectories("skill-scenarios-scored-runs");

/** The check's sentence of a clause of the guidance stage, by its id. */
function sentence(id: string): string {
  const clause = OWNER_WALK_STAGES.guidance?.clauses.find((each) => each.id === id);
  if (clause === undefined) throw new Error(`the guidance stage has no clause ${id}`);
  return clause.sentence;
}
/** A score's clause, by the id of its check sentence. */
function clauseOf(clauses: readonly ScoreClause[], id: string): ScoreClause {
  const found = clauses.find((clause) => clause.clause === sentence(id));
  if (found === undefined) throw new Error(`the score holds no clause ${id}`);
  return found;
}

const owner = "ses-owner";
const R = "LWEVAL-4";
const E1 = "LWEVAL-5";
const G = "LWEVAL-6";
const head = "4f1c0e9a6b2d8e7f3a5c1b9d0e2f4a6c8b1d3e5f";
const alice = { kind: "user", id: "alice@example.invalid" };
/** The recorded plans answer of the run: two finished issues of another project. */
const recorded = {
  project: "SRX1",
  issues: [
    {
      issue: "SRX1-2",
      title: "A second change rates its plan",
      ratings: [
        { rating: 4, reason: "The plan fit the work as written, with no task added later." },
      ],
    },
    {
      issue: "SRX1-1",
      title: "One issue walks to done",
      ratings: [{ rating: 3, reason: "The review sent it back once." }],
    },
  ],
};
const keyMap = { SRX1: "LWEVAL", "SRX1-1": "LWEVAL-1", "SRX1-2": "LWEVAL-2" };

interface Planted {
  form?: "A" | "B" | "C" | "D" | "E";
  kind?: "root" | "child";
  family?: "claude" | "gpt";
  label?: string;
  n?: number;
  /** The newest version of G's plan.md, or none. */
  plan?: string;
  /** dispatch-proxy.jsonl's requests, in log order. */
  proxy?: { method: string; path: string; status?: number }[];
  /** The assistant's answer to the questions. */
  answer?: string;
  /** What the run's `dispatch plans` call printed. */
  read?: string;
  /** Tool calls the transcript holds before the plan, each `[tool, arguments]`. */
  calls?: [string, object][];
  /** The models the world's guidance names. */
  models?: { claude: string; gpt: string };
  judgment?: { item: string; verdict: "pass" | "fail"; why: string }[];
  /** The run's own files for score.ts runs: dispatch.json, built-from. */
  forScore?: { database: string; model: string };
  /** The recorded plans answer, in place of `recorded`. */
  recorded?: typeof recorded;
}

const readCall: [string, object] = ["bash", { command: "dispatch plans --project LWEVAL" }];
const readFirst = [
  { method: "GET", path: "/api/v1/projects/LWEVAL/plans" },
  { method: "POST", path: `/api/v1/issues/${G}/artifacts`, status: 201 },
];
const planOf = (steps: string, text: string) =>
  `# Plan\n\n:::plan_task{#a steps="${steps}"}\n${text}\n:::\n`;

/** The world of a planted run, through the world builder the rig runs. */
function worldOf(planted: Planted): OwnerWalkWorld {
  const source: PlansAnswerSource = { commit: "a".repeat(40), sha256: "b".repeat(64), keyMap };
  return ownerWalkWorld({
    case: {
      kind: planted.kind ?? "root",
      form: planted.form ?? "B",
      family: planted.family ?? "claude",
    },
    stage: "guidance",
    source,
    models: planted.models ?? { claude: "anthropic/claude-opus-5-5", gpt: "openai/gpt-6.1-sol" },
  });
}

/** The run's name, as rig.sh names an owner-walk run's directory. */
function nameOf(planted: Planted): string {
  const kind = planted.kind ?? "root";
  return `owner-walk-${kind}-${planted.form ?? "B"}-${planted.family ?? "claude"}-${planted.label ?? "head"}-${planted.n ?? 1}`;
}

/** Writes one owner-walk run's directory under `runs`, and answers its directory and name. */
function plant(runs: string, planted: Planted): { dir: string; name: string } {
  const name = nameOf(planted);
  const dir = path.join(runs, name);
  mkdirSync(path.join(dir, "sessions"), { recursive: true });
  const write = (file: string, value: unknown) =>
    writeFileSync(
      path.join(dir, file),
      typeof value === "string" ? value : `${JSON.stringify(value)}\n`
    );
  const world = worldOf(planted);
  write("world.json", world);
  const answerText = `${JSON.stringify(planted.recorded ?? recorded)}\n`;
  write("plans-answer.json", answerText);
  write("plans-answer.source", {
    commit: "a".repeat(40),
    sha256: createHash("sha256").update(answerText).digest("hex"),
    keyMap,
  });
  const fixture = {
    scenario: "owner-walk",
    kind: world.kind,
    project: "LWEVAL",
    owner: R,
    planned: G,
    issues: [
      {
        key: R,
        part: "root",
        title: "Import bank statements",
        parent: null,
        status: "todo",
        spec: { artifact: "art-r", approvedVersion: 1 },
      },
      {
        key: E1,
        part: "earlier",
        title: "The CSV importer reads bank statements",
        parent: null,
        status: "done",
        spec: { artifact: "art-e", approvedVersion: null },
      },
      {
        key: G,
        part: "planned",
        title: "Finish the CSV importer",
        parent: R,
        status: "backlog",
        spec: { artifact: "art-g", approvedVersion: null },
      },
    ],
    earlier: {
      key: E1,
      record: { id: "h-e1", version: 1 },
      session: "rig-implementer-e1",
      pr: 41,
      head,
    },
    plans: [
      { recorded: "SRX1-1", key: "LWEVAL-1", title: "One issue walks to done" },
      { recorded: "SRX1-2", key: "LWEVAL-2", title: "A second change rates its plan" },
    ],
  };
  const spec = (id: string, markdown: string) => ({
    id,
    name: "spec.md",
    slug: "spec",
    primary: true,
    versions: [
      { version: 1, created_at: "2026-11-24T11:00:00.000001Z", authors: [alice], markdown },
    ],
  });
  write("captured.json", {
    fixture,
    issues: [
      {
        key: R,
        title: "Import bank statements",
        status: "todo",
        parent: null,
        artifacts: [spec("art-r", "## Goal\n\nThe root.")],
        handoffs: [],
      },
      {
        key: E1,
        title: "The CSV importer reads bank statements",
        status: "done",
        parent: null,
        artifacts: [spec("art-e", "## Goal\n\nThe earlier issue.")],
        handoffs: [
          {
            id: "h-e1",
            phase: "implement",
            version: 1,
            author: { kind: "session", id: "rig-implementer-e1" },
            created_at: "2026-11-24T11:00:01.000001Z",
            body: JSON.stringify({ schemaVersion: 1, phase: "implement", issue: E1, head }),
          },
        ],
      },
      {
        key: G,
        title: "Finish the CSV importer",
        status: "backlog",
        parent: R,
        artifacts: [
          spec("art-g", "## Goal\n\nThe planned issue."),
          ...(planted.plan === undefined
            ? []
            : [
                {
                  id: "art-plan",
                  name: "plan.md",
                  slug: "plan-md",
                  primary: false,
                  versions: [
                    {
                      version: 1,
                      created_at: "2026-11-24T12:00:05.000001Z",
                      authors: [{ kind: "session", id: owner }],
                      markdown: planted.plan,
                    },
                  ],
                },
              ]),
        ],
        handoffs: [],
      },
    ],
  });
  write(
    "dispatch-proxy.jsonl",
    (planted.proxy ?? [])
      .map(
        (line, index) =>
          `${JSON.stringify({
            at: new Date(Date.parse("2026-11-24T12:00:00.000Z") + index * 1000).toISOString(),
            method: line.method,
            path: line.path,
            query: "",
            status: line.status ?? 200,
          })}\n`
      )
      .join("")
  );
  write("out.txt", "exit=0\n");
  if (planted.judgment !== undefined)
    write("judgment.json", {
      run: name,
      items: planted.judgment,
      by: "ses-judge",
      at: "2026-11-24T13:00:00Z",
    });
  const model = planted.forScore?.model ?? "model-gateway/anthropic/claude-opus-5-5";
  if (planted.forScore !== undefined) {
    write(
      "dispatch.json",
      JSON.stringify({
        postgres: "pg-1",
        database: planted.forScore.database,
        port: 20000,
        url: "http://127.0.0.1:20000",
      })
    );
    write("built-from", `checkout /checkouts/head\nmodel ${planted.forScore.model}\n`);
  }
  let call = 0;
  const toolCall = (tool: string, args: object) => {
    call += 1;
    return [
      {
        type: "message",
        timestamp: "2026-11-24T12:00:02.000Z",
        message: {
          role: "assistant",
          content: [{ type: "toolCall", id: `call-${call}`, name: tool, arguments: args }],
        },
      },
      {
        type: "message",
        message: {
          role: "toolResult",
          toolCallId: `call-${call}`,
          content: [
            {
              type: "text",
              text: tool === "bash" && args === readCall[1] ? (planted.read ?? "") : "ok",
            },
          ],
        },
      },
    ];
  };
  const transcript = [
    { type: "session", id: owner, cwd: "/rig/cwd" },
    { type: "model_change", model },
    {
      type: "message",
      timestamp: "2026-11-24T11:59:00.000Z",
      message: { role: "user", content: [{ type: "text", text: "The start task." }] },
    },
    ...(planted.calls ?? [readCall]).flatMap(([tool, args]) => toolCall(tool, args)),
    {
      type: "message",
      timestamp: "2026-11-24T12:00:06.000Z",
      message: { role: "assistant", content: [{ type: "text", text: "I wrote the plan." }] },
    },
    {
      type: "message",
      timestamp: "2026-11-24T12:10:00.000Z",
      message: { role: "user", content: [{ type: "text", text: world.questions.join("\n\n") }] },
    },
    {
      type: "message",
      timestamp: "2026-11-24T12:10:30.000Z",
      message: {
        role: "assistant",
        content: [{ type: "text", text: planted.answer ?? "The reviewer step." }],
      },
    },
  ];
  write(
    path.join("sessions", "2026-11-24T11-58-00-000Z_ses-owner.jsonl"),
    transcript.map((entry) => `${JSON.stringify(entry)}\n`).join("")
  );
  return { dir, name };
}

describe("ownerWalk's program clauses", () => {
  // Catches: a read scored by its presence alone, or by the plan version's settle time rather
  // than by the order of the proxy's log.
  test("a read before the plan's first write passes, and one after it fails", () => {
    const runs = fresh();
    const plan = planOf("implementer reviewer", "Finish the importer.");
    const before = plant(runs, { plan, proxy: readFirst, n: 1 });
    const after = plant(runs, { plan, proxy: [...readFirst].reverse(), n: 2 });
    expect(clauseOf(ownerWalk(before.dir, before.name, "head").clauses, "read").pass).toBe(true);
    expect(clauseOf(ownerWalk(after.dir, after.name, "head").clauses, "read").pass).toBe(false);
  });

  // Catches: a family named only as a word the check gives, never inside a model id.
  test("a plan that names 'GPT' and 'Claude' in its review block passes families, and one that names only model ids passes too", () => {
    const runs = fresh();
    const words = plant(runs, {
      form: "A",
      n: 1,
      proxy: readFirst,
      plan: planOf(
        "implementer reviewer",
        "The implementer runs on Claude. The reviewer runs on GPT, another family."
      ),
    });
    const ids = plant(runs, {
      form: "A",
      n: 2,
      proxy: readFirst,
      plan: planOf(
        "implementer reviewer",
        "The implementer is anthropic/claude-opus-5-5 and the reviewer is openai/gpt-6.1-sol."
      ),
    });
    expect(clauseOf(ownerWalk(words.dir, words.name, "head").clauses, "families").pass).toBe(true);
    expect(clauseOf(ownerWalk(ids.dir, ids.name, "head").clauses, "families").pass).toBe(true);
  });

  // Catches: a families rule that passes on one family, or on a word inside another word.
  test("a review block that names one family fails families", () => {
    const runs = fresh();
    const one = plant(runs, {
      form: "A",
      proxy: readFirst,
      plan: planOf(
        "implementer reviewer",
        "Both steps run on Claude. The reviewer checks ChatGPTs."
      ),
    });
    expect(clauseOf(ownerWalk(one.dir, one.name, "head").clauses, "families").pass).toBe(false);
  });

  // Catches: a review step found by the word alone, wherever it stands in the steps.
  test("a reviewer step before the implementer fails the review step clause", () => {
    const runs = fresh();
    const backwards = plant(runs, {
      form: "C",
      proxy: readFirst,
      plan: planOf("reviewer implementer", "Review, then build."),
    });
    const clauses = ownerWalk(backwards.dir, backwards.name, "head").clauses;
    expect(clauseOf(clauses, "review-step").pass).toBe(false);
  });

  // Catches: a judged clause scored before its judgment, or a judgment item for another run.
  test("a judged clause is pending until judgment.json gives its verdict", () => {
    const runs = fresh();
    const plan = planOf("implementer reviewer", "Finish the importer.");
    const waiting = plant(runs, { plan, proxy: readFirst, n: 1 });
    const judged = plant(runs, {
      plan,
      proxy: readFirst,
      n: 2,
      judgment: [
        { item: "choices", verdict: "pass", why: "Each step names its agent and why." },
        { item: "earlier-and-fresh", verdict: "fail", why: "The second task names no agent." },
      ],
    });
    const pending = ownerWalk(waiting.dir, waiting.name, "head").clauses;
    expect(clauseOf(pending, "choices").pass).toBe("pending");
    expect(runPass(pending)).toBe("pending");
    const decided = ownerWalk(judged.dir, judged.name, "head").clauses;
    expect(clauseOf(decided, "choices").pass).toBe(true);
    expect(clauseOf(decided, "earlier-and-fresh").pass).toBe(false);
    expect(runPass(decided)).toBe(false);
  });

  // Catches: a control form scored on its head form's clauses, which a control does not count.
  test("a run of form D is scored on read alone, and one of form E on choices alone", () => {
    const runs = fresh();
    const plan = planOf("implementer reviewer", "Finish the importer.");
    const d = plant(runs, { form: "D", label: "ctl-D", plan, proxy: readFirst });
    const e = plant(runs, { form: "E", label: "ctl-E", plan, proxy: readFirst });
    expect(ownerWalk(d.dir, d.name, "ctl-D").clauses.map((clause) => clause.clause)).toEqual([
      sentence("read"),
    ]);
    expect(ownerWalk(e.dir, e.name, "ctl-E").clauses.map((clause) => clause.clause)).toEqual([
      sentence("choices"),
    ]);
  });
});

describe("what is reported beside the clauses", () => {
  // Catches: a scorer that maps a recorded reason's keys by another rule than the proxy's, so the
  // reason it holds the plan to is not the text the agent read: a key followed by `-` or `_`,
  // which the proxy serves as it was recorded.
  test("a plan that quotes a recorded reason as the proxy served it cites it, a key followed by - or _ included", () => {
    const reason = "Fix SRX1-1_b and SRX1-2-x first, then stop.";
    const answer = {
      ...recorded,
      issues: recorded.issues.map((issue) =>
        issue.issue === "SRX1-1" ? { ...issue, ratings: [{ rating: 2, reason }] } : issue
      ),
    };
    const run = plant(fresh(), {
      recorded: answer,
      proxy: readFirst,
      plan: planOf("implementer reviewer", `The last retro said: ${reason}`),
    });
    expect(
      ownerWalk(run.dir, run.name, "head").reported.find((each) => each.name === "cites-key")
    ).toEqual({
      name: "cites-key",
      value: true,
      evidence:
        'the plan holds "fix srx1 1 b and srx1 2 x", eight words in a row of a recorded reason',
    });
  });
});

describe("judge-input.md", () => {
  // Catches: a judge's input that leaks the label or the form, so the judge could tell a control
  // from the head; or one that leaves out a text an item needs.
  test("judge-input.md names no label and no form letter", () => {
    const runs = fresh();
    const label = "lbl-q7z";
    const run = plant(runs, {
      form: "A",
      label,
      proxy: readFirst,
      plan: planOf("implementer reviewer", "PLAN-MARKER-31 for both tasks."),
      read: "READ-MARKER-47 LWEVAL-2 rated 4",
      answer: "ANSWER-MARKER-58",
      models: { claude: "claude-marker-5", gpt: "gpt-marker-9" },
    });
    ownerWalk(run.dir, run.name, label);
    const input = readFileSync(path.join(run.dir, "judge-input.md"), "utf8");
    for (const marker of [
      "PLAN-MARKER-31",
      "READ-MARKER-47",
      "ANSWER-MARKER-58",
      "claude-marker-5",
      "gpt-marker-9",
    ])
      expect(input).toContain(marker);
    expect(input).not.toContain(label);
    expect(input).not.toContain(run.name);
    expect(input).not.toMatch(/\bform\s+[A-E]\b/i);
    expect(input).not.toMatch(/\([A-E]\)/);
  });

  // Catches: a judge asked for an item its form does not hold, or no input for a form that needs
  // no judgment.
  test("a run of form D writes no judge-input.md", () => {
    const runs = fresh();
    const d = plant(runs, {
      form: "D",
      label: "ctl-D",
      proxy: readFirst,
      plan: planOf("implementer reviewer", "Finish it."),
    });
    ownerWalk(d.dir, d.name, "ctl-D");
    expect(existsSync(path.join(d.dir, "judge-input.md"))).toBe(false);
  });
});

/** Writes a run directory holding only its row.json and world.json, as cells reads them. */
function plantRow(
  runs: string,
  planted: Planted & { clauses?: ScoreClause[]; error?: string }
): void {
  const name = nameOf(planted);
  const dir = path.join(runs, name);
  mkdirSync(dir, { recursive: true });
  writeFileSync(path.join(dir, "world.json"), `${JSON.stringify(worldOf(planted))}\n`);
  const common = {
    run: name,
    scenario: "owner-walk",
    label: planted.label ?? "head",
    case: `${planted.kind ?? "root"}-${planted.form ?? "B"}-${planted.family ?? "claude"}`,
  };
  const row =
    planted.error !== undefined
      ? { outcome: "rig-error", ...common, error: planted.error }
      : {
          outcome: "scored",
          ...common,
          pass: runPass(planted.clauses ?? []),
          clauses: planted.clauses ?? [],
          reach: [],
        };
  writeFileSync(path.join(dir, "row.json"), `${JSON.stringify(RunRow.parse(row))}\n`);
}
const clause = (id: string, pass: boolean | "pending"): ScoreClause => ({
  clause: sentence(id),
  pass,
  evidence: `${id} planted`,
  counts: {},
});
/** The twelve owners of a control form: three runs of each kind and family, each with `pass(i)`. */
function plantControl(
  runs: string,
  form: "D" | "E",
  label: string,
  passes: (owner: number) => boolean
): void {
  let owner = 0;
  for (const kind of ["root", "child"] as const)
    for (const family of ["claude", "gpt"] as const)
      for (let n = 1; n <= 3; n += 1) {
        plantRow(runs, {
          form,
          kind,
          family,
          label,
          n,
          clauses: [clause(form === "D" ? "read" : "choices", passes(owner))],
        });
        owner += 1;
      }
}

describe("cells", () => {
  // Catches: a control counted per kind and family, where the check counts all twelve owners of
  // the form together; and a control whose rule reads "pass" for the clause's majority.
  test("D's control is one count over its twelve owners: five of twelve passes the control, with one cell at two of three", () => {
    const runs = fresh();
    // root-claude: owners 0, 1 pass (two of three); root-gpt: owner 3; child-claude: owner 6;
    // child-gpt: owner 9. Five of twelve.
    plantControl(runs, "D", "ctl-D", (owner) => [0, 1, 3, 6, 9].includes(owner));
    // Six of twelve: the control does not pass.
    plantControl(runs, "D", "ctl-D-strict", (owner) => [0, 1, 3, 6, 9, 10].includes(owner));
    expect(cells(runs, "owner-walk")).toEqual([
      {
        scenario: "owner-walk",
        label: "ctl-D",
        cell: "guidance/D",
        count: 12,
        scored: 12,
        passes: 5,
        pending: 0,
        verdict: "pass",
      },
      {
        scenario: "owner-walk",
        label: "ctl-D-strict",
        cell: "guidance/D",
        count: 12,
        scored: 12,
        passes: 6,
        pending: 0,
        verdict: "fail",
      },
    ]);
  });

  // Catches: a control that a label which cannot boot passes, its few scored runs read as few
  // owners who passed.
  test("a cell with four scored runs is UNKNOWN, never a pass", () => {
    const runs = fresh();
    let owner = 0;
    for (const kind of ["root", "child"] as const)
      for (const family of ["claude", "gpt"] as const)
        for (let n = 1; n <= 3; n += 1) {
          const scored = owner < 4;
          plantRow(runs, {
            form: "D",
            kind,
            family,
            label: "ctl-D",
            n,
            ...(scored
              ? { clauses: [clause("read", false)] }
              : { error: "the agent got no model turn" }),
          });
          owner += 1;
        }
    expect(cells(runs, "owner-walk")).toEqual([
      {
        scenario: "owner-walk",
        label: "ctl-D",
        cell: "guidance/D",
        count: 12,
        scored: 4,
        passes: 0,
        pending: 0,
        verdict: "UNKNOWN",
      },
    ]);
  });

  // Catches: a cell decided while one of its runs waits for its judgment.
  test("a cell with a pending judgment is PENDING", () => {
    const runs = fresh();
    for (let n = 1; n <= 3; n += 1)
      plantRow(runs, {
        form: "A",
        n,
        clauses: [clause("read", true), clause("choices", n === 2 ? "pending" : true)],
      });
    expect(cells(runs, "owner-walk")).toEqual([
      {
        scenario: "owner-walk",
        label: "head",
        cell: "guidance/root-A-claude",
        count: 3,
        scored: 3,
        passes: 2,
        pending: 1,
        verdict: "PENDING",
      },
    ]);
  });
});

/** A run directory with the transcripts `sessions` names: each file's entries, by its path under
 * sessions/. */
function transcripts(sessions: Record<string, object[]>): string {
  const dir = fresh();
  for (const [file, entries] of Object.entries(sessions)) {
    const full = path.join(dir, "sessions", file);
    mkdirSync(path.dirname(full), { recursive: true });
    writeFileSync(full, entries.map((entry) => `${JSON.stringify(entry)}\n`).join(""));
  }
  return dir;
}
const said = (role: string, content: object[]) => ({ type: "message", message: { role, content } });
const called = (id: string, tool: string, args: object) =>
  said("assistant", [{ type: "toolCall", id, name: tool, arguments: args }]);
const answered = (id: string, text: string) => ({
  type: "message",
  message: { role: "toolResult", toolCallId: id, content: [{ type: "text", text }] },
});

describe("reachOf", () => {
  // Catches: a reach that misses a subagent's call, or names the wrong session or message.
  test("reach lists a tool argument naming metadata.google.internal, one naming 169.254.169.254 and one naming storage.googleapis.com, each with its session, message, tool and host", () => {
    const dir = transcripts({
      "2026-11-24T12-00-00-000Z_ses-main.jsonl": [
        { type: "session", id: "ses-main", cwd: "/rig/cwd" },
        { type: "model_change", model: "model-gateway/anthropic/claude-opus-5-5" },
        said("user", [{ type: "text", text: "The start task." }]),
        called("c1", "bash", {
          command:
            "curl -s -H 'Metadata-Flavor: Google' http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token",
        }),
        answered("c1", "a token"),
        called("c2", "web_fetch", { url: "https://storage.googleapis.com/a-bucket/runs/" }),
      ],
      "2026-11-24T12-00-00-000Z_ses-main/Probe.jsonl": [
        { type: "session", id: "ses-sub", cwd: "/rig/cwd" },
        said("user", [{ type: "text", text: "Probe it." }]),
        called("s1", "eval", { code: "await fetch('http://169.254.169.254/computeMetadata/v1/')" }),
      ],
    });
    expect(reachOf(dir, "owner-walk-root-B-claude-head-1")).toEqual([
      { session: "ses-main", message: 1, tool: "bash", host: "metadata.google.internal" },
      { session: "ses-main", message: 3, tool: "web_fetch", host: "storage.googleapis.com" },
      { session: "ses-sub", message: 1, tool: "eval", host: "169.254.169.254" },
    ]);
  });

  // Catches: a reach that is undefined, or a placeholder entry, for a run that named no host.
  test("a transcript with none of the three gives an empty reach", () => {
    const dir = transcripts({
      "2026-11-24T12-00-00-000Z_ses-main.jsonl": [
        { type: "session", id: "ses-main", cwd: "/rig/cwd" },
        said("user", [{ type: "text", text: "The start task." }]),
        called("c1", "bash", { command: "dispatch plans --project LWEVAL" }),
        answered("c1", "LWEVAL-1 One issue walks to done"),
        called("c2", "bash", { command: "curl -s https://metadata.example.invalid/storage" }),
      ],
    });
    expect(reachOf(dir, "owner-walk-root-B-claude-head-1")).toEqual([]);
  });

  // Catches: a reach read from every string of the transcript, not from tool arguments alone.
  test("a host named in an assistant's text, or in a tool's result, is no tool argument and gives no entry", () => {
    const dir = transcripts({
      "2026-11-24T12-00-00-000Z_ses-main.jsonl": [
        { type: "session", id: "ses-main", cwd: "/rig/cwd" },
        said("user", [{ type: "text", text: "Do not read 169.254.169.254." }]),
        said("assistant", [
          { type: "thinking", thinking: "The metadata server is at 169.254.169.254." },
          { type: "text", text: "I will not call metadata.google.internal." },
        ]),
        called("c1", "bash", { command: "echo hi" }),
        answered("c1", "see https://storage.googleapis.com/a-bucket"),
      ],
    });
    expect(reachOf(dir, "owner-walk-root-B-claude-head-1")).toEqual([]);
  });

  // Catches: a reach that changes a clause or makes the run a rig error, where it is only a record
  // for the reader of the scores.
  test("a run with a reach keeps its verdict, and is no rig error", () => {
    const work = fresh();
    const runs = path.join(work, "runs");
    mkdirSync(path.join(work, "profiles", "head"), { recursive: true });
    writeFileSync(path.join(work, "profiles", "head", "checkout"), "/checkouts/head\n");
    const plan = planOf("implementer reviewer", "Finish the importer.");
    const reaching = plant(runs, {
      n: 1,
      plan,
      proxy: readFirst,
      calls: [
        readCall,
        ["bash", { command: "curl -s http://metadata.google.internal/computeMetadata/v1/" }],
      ],
      forScore: { database: "run_reach_1", model: "model-gateway/anthropic/claude-opus-5-5" },
    });
    const quiet = plant(runs, {
      n: 2,
      plan,
      proxy: readFirst,
      forScore: { database: "run_reach_2", model: "model-gateway/anthropic/claude-opus-5-5" },
    });
    const rowOf = (name: string) => {
      const scored = Bun.spawnSync([
        "bun",
        path.join(import.meta.dir, "score.ts"),
        "runs",
        runs,
        "--run",
        name,
      ]);
      expect(scored.exitCode).toBe(0);
      return RunRow.parse(JSON.parse(readFileSync(path.join(runs, name, "row.json"), "utf8")));
    };
    const withReach = rowOf(reaching.name);
    const without = rowOf(quiet.name);
    if (withReach.outcome !== "scored" || without.outcome !== "scored")
      throw new Error(`want two scored rows, got ${JSON.stringify([withReach, without])}`);
    expect(withReach.reach).toEqual([
      { session: owner, message: 3, tool: "bash", host: "metadata.google.internal" },
    ]);
    expect(without.reach).toEqual([]);
    expect(withReach.clauses).toEqual(without.clauses);
    expect(withReach.pass).toBe(without.pass);
  });
});

describe("ownerWalkWorld", () => {
  // Catches: a world whose guidance or questions do not follow its form, which would give a
  // control the head's texts; or plans keys that are the recorded answer's, not the run's.
  test("a world's guidance and questions follow its form, and its plans keys are the run's", () => {
    const models = { claude: "claude-marker-5", gpt: "gpt-marker-9" };
    const world = (form: "A" | "B" | "C" | "D" | "E") =>
      worldOf({ form, models, kind: "child", family: "gpt" });
    expect(world("A").guidance).toEqual({
      file: "guidance-first.md",
      implementerFamily: "claude",
      reviewerFamily: "gpt",
      implementerModel: "claude-marker-5",
      reviewerModel: "gpt-marker-9",
    });
    expect(world("D").guidance).toEqual(world("A").guidance);
    expect(world("C").guidance).toEqual({
      file: "guidance-second.md",
      implementerFamily: "claude",
      reviewerFamily: "claude",
      implementerModel: "claude-marker-5",
      reviewerModel: "claude-marker-5",
    });
    expect(world("B").guidance).toBeNull();
    expect(world("E").guidance).toBeNull();
    expect(world("D").questions).toEqual(questionsOf("A"));
    expect(world("E").questions).toEqual(questionsOf("B"));
    expect(questionsOf("A").length).toBe(2);
    expect(world("A")).toMatchObject({
      scenario: "owner-walk",
      stage: "guidance",
      kind: "child",
      form: "A",
      family: "gpt",
      issue: "issue.md",
      plansProject: "LWEVAL",
      plansKeys: ["LWEVAL-1", "LWEVAL-2"],
    });
  });
});
