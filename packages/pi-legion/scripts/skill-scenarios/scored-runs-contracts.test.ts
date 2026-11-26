import { expect, test } from "bun:test";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import {
  CellLine,
  DispatchProxyLine,
  ForwardReadings,
  Judgment,
  LaunchAnswer,
  LaunchRequest,
  OwnerWalkWorld,
  PlanAnswer,
  PlanRequest,
  PlansAnswerSource,
  PodCallLine,
  parseManifest,
  RigDaemonReady,
  RigDaemonRecord,
  RigDaemonRequest,
  RigDaemonStatus,
  RunRow,
  Score,
  type ScoreClause,
  StandinIssues,
  StandinLogLine,
  WorldAnswer,
  WorldRequest,
} from "./scored-runs-contracts";

interface Schema {
  safeParse(value: unknown): { success: boolean };
}

/** The names of the cases whose schema parses their value, in the cases' order. */
function parsing(cases: Record<string, readonly [Schema, unknown]>): string[] {
  return Object.entries(cases)
    .filter(([, [schema, value]]) => schema.safeParse(value).success)
    .map(([name]) => name);
}

/** `value` without its field `key`. */
function without(value: object, key: string): Record<string, unknown> {
  return Object.fromEntries(Object.entries(value).filter(([name]) => name !== key));
}

/** The message of the error `run` throws. */
function refusal(run: () => unknown): string {
  try {
    run();
  } catch (error) {
    return error instanceof Error ? error.message : String(error);
  }
  throw new Error("it threw nothing");
}

const AT = "2026-11-24T10:00:00Z";
const COMMIT = "0123456789abcdef0123456789abcdef01234567";
const SHA256 = "0123456789abcdef".repeat(4);
const GH_TOKEN = fixtureSecret("gh");

const OWNER_WALK_A = {
  scenario: "owner-walk",
  stage: "guidance",
  kind: "root",
  form: "A",
  family: "claude",
  issue: "issue.md",
  plansProject: "DEMO",
  plansKeys: ["DEMO-3", "DEMO-4"],
  guidance: {
    file: "guidance-first.md",
    implementerFamily: "claude",
    reviewerFamily: "gpt",
    implementerModel: "claude-model-1",
    reviewerModel: "gpt-model-1",
  },
  questions: ["question-marker-1", "question-marker-2"],
};
const OWNER_WALK_B = {
  ...OWNER_WALK_A,
  form: "B",
  guidance: null,
  questions: ["question-marker-1"],
};

test("an owner-walk world parses, and one with an unknown kind does not", () => {
  const worlds = {
    A: [OwnerWalkWorld, OWNER_WALK_A],
    B: [OwnerWalkWorld, OWNER_WALK_B],
  } as const;
  expect(parsing(worlds)).toEqual(Object.keys(worlds));
  expect(
    parsing({
      "kind sub": [OwnerWalkWorld, { ...OWNER_WALK_A, kind: "sub" }],
      "form F": [OwnerWalkWorld, { ...OWNER_WALK_A, form: "F" }],
      "family gemini": [OwnerWalkWorld, { ...OWNER_WALK_A, family: "gemini" }],
      "guidance family gemini": [
        OwnerWalkWorld,
        { ...OWNER_WALK_A, guidance: { ...OWNER_WALK_A.guidance, reviewerFamily: "gemini" } },
      ],
      "scenario separate-pod": [OwnerWalkWorld, { ...OWNER_WALK_A, scenario: "separate-pod" }],
    })
  ).toEqual([]);
});

test("an owner-walk world of form B or E has no guidance, and one of form A, C or D has one", () => {
  expect(
    parsing({
      A: [OwnerWalkWorld, OWNER_WALK_A],
      C: [OwnerWalkWorld, { ...OWNER_WALK_A, form: "C" }],
      D: [OwnerWalkWorld, { ...OWNER_WALK_A, form: "D" }],
      B: [OwnerWalkWorld, OWNER_WALK_B],
      E: [OwnerWalkWorld, { ...OWNER_WALK_B, form: "E" }],
    })
  ).toEqual(["A", "C", "D", "B", "E"]);
  expect(
    parsing({
      "A without guidance": [OwnerWalkWorld, { ...OWNER_WALK_A, guidance: null }],
      "C without guidance": [OwnerWalkWorld, { ...OWNER_WALK_A, form: "C", guidance: null }],
      "D without guidance": [OwnerWalkWorld, { ...OWNER_WALK_A, form: "D", guidance: null }],
      "B with guidance": [OwnerWalkWorld, { ...OWNER_WALK_B, guidance: OWNER_WALK_A.guidance }],
      "E with guidance": [
        OwnerWalkWorld,
        { ...OWNER_WALK_B, form: "E", guidance: OWNER_WALK_A.guidance },
      ],
    })
  ).toEqual([]);
});

const PLAN = { task: "rig-marker-task", steps: ["implementer", "reviewer"], text: "text-marker" };
const WORLD_REQUEST = {
  project: "DEMO",
  repository: "example/widgets",
  defaultBranch: "main",
  workspace: "/work/run-1/workspace",
  issues: [
    {
      key: "DEMO-1",
      title: "The root",
      parent: null,
      tree: "DEMO-1",
      status: "in_progress",
      phase: "implementing",
    },
    {
      key: "DEMO-2",
      title: "The child",
      parent: "DEMO-1",
      tree: "DEMO-1",
      status: "backlog",
      phase: null,
    },
  ],
  gate: {
    issue: "DEMO-1",
    step: "spec",
    version: 2,
    approved: true,
    artifactId: "spec-demo-1",
  },
  subject: "DEMO-2",
  plan: PLAN,
  earlier: [
    {
      role: "implementer",
      handoffRecord: "implement",
      handoffVersion: 3,
      commit: COMMIT,
      verdict: null,
    },
  ],
  pullRequest: { number: 7, head: COMMIT },
  moment: "step",
  role: "reviewer",
};
const NOTICE = { issue: "DEMO-1", kind: "catch-up", row: 12, payload: { kind: "catch-up" } };
const WORLD_ANSWER = {
  starts: [{ role: "reviewer", issue: "DEMO-2", task: "start-marker" }],
  notices: [NOTICE],
};
const LAUNCH_REQUEST = {
  project: "DEMO",
  stateDir: "/work/run-1/state",
  workspace: "/work/run-1/workspace",
  daemonUrl: "http://127.0.0.1:9000",
  envoyUrl: "http://127.0.0.1:9001",
  envoyTokenFile: "/work/run-1/envoy-token",
  natsUrls: ["nats://127.0.0.1:4222"],
  bootTokenFile: "/work/run-1/boot-token",
  ghToken: GH_TOKEN,
  ghPath: "/work/run-1/bin/gh",
  path: "/work/run-1/bin:/usr/bin",
  claim: { tree: "DEMO-1", issue: "DEMO-1", role: "architect" },
  repository: "example/widgets",
  defaultBranch: "main",
  instructionsFile: "/work/run-1/instructions.md",
  notices: [NOTICE],
};
const LAUNCH_ANSWER = {
  systemPromptFile: "/work/run-1/texts/system-prompt.md",
  env: { LEGION_DAEMON_URL: "http://127.0.0.1:9000", EMPTY_VALUE: "" },
  published: [{ topic: "notifications.role.marker", message: "catch-up DEMO-1", key: "outbox-12" }],
};
const PLAN_REQUEST = {
  artifactId: "plan-artifact",
  version: 2,
  latestVersion: 2,
  blocks: [
    { id: "b-1", type: "plan_task", attributes: { id: "rig-marker-task" }, text: "text-marker" },
  ],
};
const PLAN_ANSWER = { status: 409, body: '{"code":"PLAN_TASKS"}' };

test("the entry's request and answer files parse, and a request with an unknown field does not", () => {
  const files = {
    "world request": [WorldRequest, WORLD_REQUEST],
    "world answer": [WorldAnswer, WORLD_ANSWER],
    "launch request": [LaunchRequest, LAUNCH_REQUEST],
    "launch answer": [LaunchAnswer, LAUNCH_ANSWER],
    "plan request": [PlanRequest, PLAN_REQUEST],
    "plan answer": [PlanAnswer, PLAN_ANSWER],
  } as const;
  expect(parsing(files)).toEqual(Object.keys(files));
  expect(
    parsing({
      "world request, a field of its own": [WorldRequest, { ...WORLD_REQUEST, deployment: {} }],
      "world request, a field of an issue": [
        WorldRequest,
        { ...WORLD_REQUEST, issues: [{ ...WORLD_REQUEST.issues[0], generation: 1 }] },
      ],
      "world request, a field of the plan": [
        WorldRequest,
        { ...WORLD_REQUEST, plan: { ...PLAN, agentKeys: ["a"] } },
      ],
      "launch request, a field of its own": [LaunchRequest, { ...LAUNCH_REQUEST, roles: [] }],
      "launch request, a field of the claim": [
        LaunchRequest,
        { ...LAUNCH_REQUEST, claim: { ...LAUNCH_REQUEST.claim, generation: 1 } },
      ],
      "plan request, a field of its own": [PlanRequest, { ...PLAN_REQUEST, project: "DEMO" }],
      "plan request, a field of a block": [
        PlanRequest,
        { ...PLAN_REQUEST, blocks: [{ ...PLAN_REQUEST.blocks[0], version: 2 }] },
      ],
    })
  ).toEqual([]);
});

test("a launch request with an absolute planGuidanceFile parses, and one with a relative path does not", () => {
  expect(
    parsing({
      "no guidance file": [LaunchRequest, LAUNCH_REQUEST],
      "an absolute guidance file": [
        LaunchRequest,
        { ...LAUNCH_REQUEST, planGuidanceFile: "/work/run-1/guidance-first.md" },
      ],
    })
  ).toEqual(["no guidance file", "an absolute guidance file"]);
  expect(
    parsing({
      "a relative guidance file": [
        LaunchRequest,
        { ...LAUNCH_REQUEST, planGuidanceFile: "guidance-first.md" },
      ],
      "an empty guidance file": [LaunchRequest, { ...LAUNCH_REQUEST, planGuidanceFile: "" }],
    })
  ).toEqual([]);
});

test("a world request with a plan and no task id does not parse", () => {
  expect(
    parsing({
      "no plan": [WorldRequest, { ...WORLD_REQUEST, plan: null }],
      "a plan with its task id": [WorldRequest, WORLD_REQUEST],
    })
  ).toEqual(["no plan", "a plan with its task id"]);
  expect(
    parsing({
      "a plan with no task id": [WorldRequest, { ...WORLD_REQUEST, plan: without(PLAN, "task") }],
      "a plan with an empty task id": [
        WorldRequest,
        { ...WORLD_REQUEST, plan: { ...PLAN, task: "" } },
      ],
    })
  ).toEqual([]);
});

test("a world request whose gate has no artifactId does not parse", () => {
  const gate = WORLD_REQUEST.gate as Record<string, unknown>;
  expect(
    parsing({
      "a gate with its artifactId": [WorldRequest, WORLD_REQUEST],
      "no gate": [WorldRequest, { ...WORLD_REQUEST, gate: null }],
    })
  ).toEqual(["a gate with its artifactId", "no gate"]);
  expect(
    parsing({
      "a gate with no artifactId": [
        WorldRequest,
        { ...WORLD_REQUEST, gate: without(gate, "artifactId") },
      ],
      "a gate with an empty artifactId": [
        WorldRequest,
        { ...WORLD_REQUEST, gate: { ...gate, artifactId: "" } },
      ],
    })
  ).toEqual([]);
});

test("parseManifest reads each of the six verbs, and refuses another verb, naming its line", () => {
  const lines = [
    "# a comment, then a blank line",
    "",
    "file    abc1234^ packages/a.md",
    "reverse abc1234 packages/b.md",
    "patch   controls/patches/one.patch",
    "unpatch controls/patches/two.patch",
    "drop    packages/c.md controls/drops/c.txt",
    "replace packages/d.md controls/drops/d-old.txt controls/drops/d-new.txt",
  ];
  expect(parseManifest(`${lines.join("\n")}\n`)).toEqual([
    { kind: "file", commit: "abc1234^", path: "packages/a.md", line: 3, text: lines[2] ?? "" },
    { kind: "reverse", commit: "abc1234", path: "packages/b.md", line: 4, text: lines[3] ?? "" },
    { kind: "patch", patchFile: "controls/patches/one.patch", line: 5, text: lines[4] ?? "" },
    { kind: "unpatch", patchFile: "controls/patches/two.patch", line: 6, text: lines[5] ?? "" },
    {
      kind: "drop",
      path: "packages/c.md",
      textFile: "controls/drops/c.txt",
      line: 7,
      text: lines[6] ?? "",
    },
    {
      kind: "replace",
      path: "packages/d.md",
      oldFile: "controls/drops/d-old.txt",
      newFile: "controls/drops/d-new.txt",
      line: 8,
      text: lines[7] ?? "",
    },
  ]);
  const message = refusal(() =>
    parseManifest("file abc1234 packages/a.md\nmove packages/a.md packages/b.md\n")
  );
  expect(message).toContain("line 2");
  expect(message).toContain('"move packages/a.md packages/b.md"');
});

test("parseManifest refuses a line with a wrong count of arguments, naming the line", () => {
  const extra = refusal(() => parseManifest("patch controls/patches/one.patch packages/a.md"));
  expect(extra).toContain("line 1");
  expect(extra).toContain('"patch controls/patches/one.patch packages/a.md"');
  const short = refusal(() =>
    parseManifest("# a comment\n\nreplace packages/d.md controls/drops/d-old.txt\n")
  );
  expect(short).toContain("line 3");
  expect(short).toContain('"replace packages/d.md controls/drops/d-old.txt"');
});

const JUDGMENT = {
  run: "owner-walk-head-1",
  items: [
    { item: "choices", verdict: "pass", why: "why-marker-1" },
    { item: "yes", verdict: "fail", why: "why-marker-2" },
  ],
  by: "acceptance-worker",
  at: AT,
};

test("a judgment with a verdict other than pass or fail does not parse", () => {
  expect(parsing({ judgment: [Judgment, JUDGMENT] })).toEqual(["judgment"]);
  const verdict = (word: string) => ({
    ...JUDGMENT,
    items: [{ ...JUDGMENT.items[0], verdict: word }],
  });
  expect(
    parsing({
      pending: [Judgment, verdict("pending")],
      PASS: [Judgment, verdict("PASS")],
      unknown: [Judgment, verdict("unknown")],
      true: [Judgment, verdict("true")],
    })
  ).toEqual([]);
});

/** A clause of a run's score or row, by its id, with a planted sentence and evidence. */
function clause(id: string, pass: boolean | "pending") {
  return {
    id,
    clause: `sentence-marker ${id}`,
    pass,
    evidence: `evidence-marker ${id}`,
    counts: {},
  };
}

const REACH = [
  { session: "session-1", message: 0, tool: "bash", host: "metadata.google.internal" },
  { session: "session-1", message: 4, tool: "fetch", host: "169.254.169.254" },
  { session: "subagent-1", message: 2, tool: "bash", host: "storage.googleapis.com" },
];

const SCORED_ROW = {
  outcome: "scored",
  run: "owner-walk-head-1",
  scenario: "owner-walk",
  label: "head",
  case: "root-A-claude",
  pass: false,
  clauses: [{ ...clause("read", true), counts: { reads: 1 } }, clause("choices", false)],
  reach: REACH,
};

test("a row's reach is a list of {session, message, tool, host}, and a host other than the three does not parse", () => {
  expect(
    parsing({
      "one entry for each host": [RunRow, SCORED_ROW],
      "no entry": [RunRow, { ...SCORED_ROW, reach: [] }],
    })
  ).toEqual(["one entry for each host", "no entry"]);
  const host = (name: string) => ({ ...SCORED_ROW, reach: [{ ...REACH[0], host: name }] });
  expect(
    parsing({
      "the cluster's API": [RunRow, host("kubernetes.default.svc")],
      "a longer name": [RunRow, host("metadata.google.internal.example")],
      "the metadata server's name in capitals": [RunRow, host("METADATA.GOOGLE.INTERNAL")],
      "an empty host": [RunRow, host("")],
    })
  ).toEqual([]);
});

test("a scored row and a score with no reach do not parse, and a reach entry with an unknown field does not", () => {
  const score = {
    run: "owner-walk-head-1",
    clauses: [clause("read", true)],
    reported: [],
    reach: REACH,
  };
  expect(parsing({ row: [RunRow, SCORED_ROW], score: [Score, score] })).toEqual(["row", "score"]);
  const extra = [{ ...REACH[0], argument: "curl -s http://metadata.google.internal/" }];
  expect(
    parsing({
      "a row with no reach": [RunRow, without(SCORED_ROW, "reach")],
      "a score with no reach": [Score, without(score, "reach")],
      "a row's entry with an unknown field": [RunRow, { ...SCORED_ROW, reach: extra }],
      "a score's entry with an unknown field": [Score, { ...score, reach: extra }],
      "a negative message index": [
        RunRow,
        { ...SCORED_ROW, reach: [{ ...REACH[0], message: -1 }] },
      ],
    })
  ).toEqual([]);
});

test("a scored row's pass is its clauses' verdict: false for a failed clause, pending for a pending one, else true", () => {
  const row = (pass: boolean | "pending", ...clauses: ScoreClause[]) => ({
    ...SCORED_ROW,
    pass,
    clauses,
  });
  expect(
    parsing({
      "all pass": [RunRow, row(true, clause("read", true), clause("choices", true))],
      "one pending": [RunRow, row("pending", clause("read", true), clause("choices", "pending"))],
      "one failed, one pending": [
        RunRow,
        row(false, clause("read", false), clause("choices", "pending")),
      ],
    })
  ).toEqual(["all pass", "one pending", "one failed, one pending"]);
  expect(
    parsing({
      "one pending, said true": [RunRow, row(true, clause("read", true), clause("x", "pending"))],
      "one failed, said pending": [
        RunRow,
        row("pending", clause("read", false), clause("x", true)),
      ],
      "all pass, said false": [RunRow, row(false, clause("read", true), clause("x", true))],
      "no clause": [RunRow, row(true)],
    })
  ).toEqual([]);
});

const RIG_ERROR_ROW = {
  outcome: "rig-error",
  run: "owner-walk-head-2",
  scenario: "owner-walk",
  label: "head",
  case: "root-A-claude",
  error: "error-marker",
};

test("a rig error row parses with its error, and one that carries clauses or reach does not", () => {
  expect(parsing({ "rig error": [RunRow, RIG_ERROR_ROW] })).toEqual(["rig error"]);
  expect(
    parsing({
      "with clauses": [RunRow, { ...RIG_ERROR_ROW, clauses: [clause("read", true)] }],
      "with reach": [RunRow, { ...RIG_ERROR_ROW, reach: [] }],
      "with no error": [RunRow, without(RIG_ERROR_ROW, "error")],
    })
  ).toEqual([]);
});

test("a score's clause may be pending, and a clause whose pass is another word does not parse", () => {
  const score = {
    run: "owner-walk-head-1",
    clauses: [clause("read", true), clause("choices", "pending"), clause("yes", false)],
    reported: [
      { name: "cites-key", value: false, evidence: "evidence-marker cites-key" },
      { name: "reminders", value: 0, evidence: "evidence-marker reminders" },
    ],
    reach: [],
  };
  expect(parsing({ score: [Score, score] })).toEqual(["score"]);
  expect(
    parsing({
      "pass fail": [Score, { ...score, clauses: [{ ...clause("read", true), pass: "fail" }] }],
      "pass unknown": [
        Score,
        { ...score, clauses: [{ ...clause("read", true), pass: "unknown" }] },
      ],
      "a clause with an unknown field": [
        Score,
        { ...score, clauses: [{ ...clause("read", true), judged: true }] },
      ],
      "a clause with no id": [Score, { ...score, clauses: [without(clause("read", true), "id")] }],
      "two clauses of one id": [
        Score,
        { ...score, clauses: [clause("read", true), clause("read", false)] },
      ],
      "a row's two clauses of one id": [
        RunRow,
        { ...SCORED_ROW, clauses: [clause("read", false), clause("read", false)] },
      ],
    })
  ).toEqual([]);
});

const CELL = {
  scenario: "owner-walk",
  label: "head",
  cell: "root-A-claude",
  count: 3,
  scored: 3,
  passes: 3,
  pending: 0,
  verdict: "pass",
};

test("a cell with fewer scored runs than its count is UNKNOWN, and one with a pending run is PENDING, never a pass", () => {
  const cells = {
    "three of three, passed": [CellLine, CELL],
    "three of three, failed": [CellLine, { ...CELL, passes: 2, verdict: "fail" }],
    "two of three": [CellLine, { ...CELL, scored: 2, passes: 2, verdict: "UNKNOWN" }],
    "two of three, one pending": [
      CellLine,
      { ...CELL, scored: 2, passes: 1, pending: 1, verdict: "UNKNOWN" },
    ],
    "three of three, one pending": [
      CellLine,
      { ...CELL, passes: 2, pending: 1, verdict: "PENDING" },
    ],
  } as const;
  expect(parsing(cells)).toEqual(Object.keys(cells));
  expect(
    parsing({
      "two of three, said pass": [CellLine, { ...CELL, scored: 2, passes: 2 }],
      "two of three, said fail": [CellLine, { ...CELL, scored: 2, passes: 1, verdict: "fail" }],
      "three of three, one pending, said pass": [CellLine, { ...CELL, passes: 2, pending: 1 }],
      "two of three, one pending, said PENDING": [
        CellLine,
        { ...CELL, scored: 2, passes: 1, pending: 1, verdict: "PENDING" },
      ],
      "three of three, said UNKNOWN": [CellLine, { ...CELL, verdict: "UNKNOWN" }],
      "three of three, said PENDING": [CellLine, { ...CELL, verdict: "PENDING" }],
      "four passes of three scored": [CellLine, { ...CELL, passes: 4 }],
    })
  ).toEqual([]);
});

test("each log line of a run parses, and one with an unknown field does not", () => {
  const proxy = {
    at: AT,
    method: "GET",
    path: "/api/v1/projects/DEMO/plans",
    query: "",
    status: 200,
  };
  const pod = { at: AT, tool: "kubectl", argv: ["run", "x", "--image=busybox"], starts: true };
  const standin = {
    at: AT,
    path: "/legion/v1/claims/activity",
    status: 200,
    sessionId: "session-1",
    body: { backgroundWork: { running: false, kinds: [] } },
  };
  const issue = { key: "DEMO-1", tree: "DEMO-1", phase: "implementing", status: "in_progress" };
  expect(
    parsing({
      proxy: [DispatchProxyLine, proxy],
      pod: [PodCallLine, pod],
      standin: [StandinLogLine, standin],
      "stand-in with no optional field": [
        StandinLogLine,
        { at: AT, path: "/healthz", status: 200 },
      ],
      issues: [StandinIssues, [issue, { ...issue, key: "DEMO-2", phase: null }]],
    })
  ).toEqual(["proxy", "pod", "standin", "stand-in with no optional field", "issues"]);
  expect(
    parsing({
      proxy: [DispatchProxyLine, { ...proxy, body: "" }],
      pod: [PodCallLine, { ...pod, exit: 1 }],
      "pod, another tool": [PodCallLine, { ...pod, tool: "curl" }],
      standin: [StandinLogLine, { ...standin, method: "POST" }],
      issues: [StandinIssues, [{ ...issue, title: "The root" }]],
    })
  ).toEqual([]);
});

test("a plans-answer source parses, and one with a short sha256 or an unknown field does not", () => {
  const source = { commit: COMMIT, sha256: SHA256, keyMap: { DEMO: "RUN", "DEMO-3": "RUN-7" } };
  expect(parsing({ source: [PlansAnswerSource, source] })).toEqual(["source"]);
  expect(
    parsing({
      "a short sha256": [PlansAnswerSource, { ...source, sha256: SHA256.slice(1) }],
      "an unknown field": [PlansAnswerSource, { ...source, project: "DEMO" }],
    })
  ).toEqual([]);
});

const RIG_DAEMON_REQUEST = {
  moment: "retro",
  role: "implementer",
  issue: "ACME-1",
  steps: ["implementer", "reviewer", "acceptance:owner"],
  world: "/work/worlds/forward-only/retro-defect",
  dispatchUrl: "http://127.0.0.1:4100",
  dispatchTokenFile: "/work/run/dispatch-token",
  envoyUrl: "http://127.0.0.1:4200",
  natsUrl: "nats://127.0.0.1:4222",
  postgresUrl: "postgres://ci@127.0.0.1:5432/rig",
  ghLog: "/work/run/gh.jsonl",
  omp: "/work/bin/omp",
  home: "/work/run/home",
  profile: "/work/run/profile",
  paneLogs: "/work/run/panes",
  paneDaemon: "label",
};
const RIG_DAEMON_READY = {
  daemonUrl: "http://127.0.0.1:4300",
  claim: "claim-1",
  session: "session-2",
  replaced: "session-1",
  generation: 1,
  head: COMMIT,
  systemPrompt: "/work/run/prompt.md",
  sessionDir: "/work/run/home/.omp/agent/sessions",
};
const RIG_DAEMON_STATUS = {
  at: AT,
  paneAlive: true,
  turn: "idle",
  activity: { backgroundWork: { running: false }, phase: { nextReminderAt: null } },
};
const RIG_DAEMON_RECORD = {
  issue: "ACME-1",
  phase: "merging",
  generation: 1,
  calls: [
    {
      at: AT,
      method: "POST",
      path: "/legion/v1/defects/report",
      status: 200,
      body: { issue: "ACME-1" },
      answer: { defect: "d-1", state: "recorded", nextChange: 1 },
    },
  ],
  defects: [
    {
      id: "d-1",
      text: "src/config.ts loadConfig returns DEFAULTS on a parse error",
      role: "implementer",
      head: COMMIT,
      generation: 1,
      carriedBy: null,
      withdrawn: null,
    },
  ],
  steps: [
    { task: "t1", step: 1, kind: "implementer", role: "implementer", attempt: 1, result: "passed" },
  ],
};
const FORWARD_READINGS = {
  messages: [{ index: 3, item: 1, verdict: "no", reader: "reader-1", model: "claude-opus-5-5" }],
};

test("each file of the label's daemon in the rig parses, and one with an extra key does not", () => {
  expect(
    parsing({
      request: [RigDaemonRequest, RIG_DAEMON_REQUEST],
      "request, the intercept control": [
        RigDaemonRequest,
        { ...RIG_DAEMON_REQUEST, paneDaemon: "intercept-report" },
      ],
      ready: [RigDaemonReady, RIG_DAEMON_READY],
      "ready at a first launch": [RigDaemonReady, { ...RIG_DAEMON_READY, replaced: null }],
      status: [RigDaemonStatus, RIG_DAEMON_STATUS],
      "status before a turn ended": [RigDaemonStatus, { ...RIG_DAEMON_STATUS, activity: null }],
      record: [RigDaemonRecord, RIG_DAEMON_RECORD],
      readings: [ForwardReadings, FORWARD_READINGS],
    })
  ).toEqual([
    "request",
    "request, the intercept control",
    "ready",
    "ready at a first launch",
    "status",
    "status before a turn ended",
    "record",
    "readings",
  ]);
  expect(
    parsing({
      request: [RigDaemonRequest, { ...RIG_DAEMON_REQUEST, model: "claude-opus-5-5" }],
      "request, another daemon": [RigDaemonRequest, { ...RIG_DAEMON_REQUEST, paneDaemon: "standin" }],
      "request without its moment": [RigDaemonRequest, without(RIG_DAEMON_REQUEST, "moment")],
      ready: [RigDaemonReady, { ...RIG_DAEMON_READY, pid: 7 }],
      "ready, a short head": [RigDaemonReady, { ...RIG_DAEMON_READY, head: COMMIT.slice(1) }],
      status: [RigDaemonStatus, { ...RIG_DAEMON_STATUS, phase: "retro" }],
      record: [RigDaemonRecord, { ...RIG_DAEMON_RECORD, closed: false }],
      "record, a call with an extra key": [
        RigDaemonRecord,
        { ...RIG_DAEMON_RECORD, calls: [{ ...RIG_DAEMON_RECORD.calls[0], session: "s" }] },
      ],
      "record, a defect with an extra key": [
        RigDaemonRecord,
        { ...RIG_DAEMON_RECORD, defects: [{ ...RIG_DAEMON_RECORD.defects[0], kind: "defect" }] },
      ],
      "record, a step with an extra key": [
        RigDaemonRecord,
        { ...RIG_DAEMON_RECORD, steps: [{ ...RIG_DAEMON_RECORD.steps[0], model: "m" }] },
      ],
      readings: [ForwardReadings, { ...FORWARD_READINGS, run: "r1" }],
      "readings, item 3": [
        ForwardReadings,
        { messages: [{ ...FORWARD_READINGS.messages[0], item: 3 }] },
      ],
      "readings, a message with an extra key": [
        ForwardReadings,
        { messages: [{ ...FORWARD_READINGS.messages[0], why: "it asks" }] },
      ],
    })
  ).toEqual([]);
});
