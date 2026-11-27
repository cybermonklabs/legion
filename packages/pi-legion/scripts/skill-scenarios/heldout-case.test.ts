import { beforeAll, describe, expect, test } from "bun:test";
import { readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { batchGaps } from "./case-world";
import {
  HELDOUT_ROLES,
  type HeldoutCase,
  heldoutGaps,
  legionAgents,
  legionPartition,
  POPULATION_KINDS,
  parseHeldoutFile,
  parseOutcome,
  parseSkillUseFile,
  type SkillUseCase,
  skillUseGaps,
} from "./heldout-case";

const TESTDATA = path.join(import.meta.dir, "testdata");
const HELDOUT_EXAMPLE = path.join(TESTDATA, "heldout-case-example.json");
const SKILLUSE_EXAMPLE = path.join(TESTDATA, "skilluse-case-example.json");
const heldoutText = readFileSync(HELDOUT_EXAMPLE, "utf8");
const skillUseText = readFileSync(SKILLUSE_EXAMPLE, "utf8");

/** The example held-out file as JSON, to take a part out of before parsing it again. */
const heldoutJson = () => JSON.parse(heldoutText);

/** Every agent the Legion plugin ships, and its skill partition, as the CLI reads them. The
 * partition is staged through the prepack script in a hook, as prepack.test.ts stages it. */
let agents: string[] = [];
let partition: string[] = [];
beforeAll(() => {
  agents = legionAgents();
  partition = legionPartition();
});

/** A copy of `from` under `id` with `fields` over it; a kind other than S or B keeps no planted
 * omission, which the parser refuses on such a case. */
function cloneCase(from: HeldoutCase, id: string, fields: Partial<HeldoutCase>): HeldoutCase {
  const copy: HeldoutCase = { ...structuredClone(from), id, ...fields };
  if (copy.kind !== "S" && copy.kind !== "B") delete copy.plantedOmission;
  if (copy.kind !== "A") delete copy.agent;
  return copy;
}

/** A held-out population with nothing missing, cloned from the two example cases: every role's
 * S, B and E case, every agent's A case, and Q1..Q4 and D1, each S and B case with its known-bad
 * trace and its planted omission. */
function fullHeldout(): HeldoutCase[] {
  const { cases } = parseHeldoutFile(heldoutText);
  const success = cases.find((each) => each.kind === "S");
  const blocked = cases.find((each) => each.kind === "B");
  if (success === undefined || blocked === undefined) throw new Error("the example lost a case");
  const table: HeldoutCase[] = [];
  for (const role of HELDOUT_ROLES) {
    table.push(cloneCase(success, `S-${role}`, { role }));
    table.push(cloneCase(blocked, `B-${role}`, { role }));
    table.push(cloneCase(success, `E-${role}`, { role, kind: "E" }));
  }
  for (const agent of agents) table.push(cloneCase(success, `A-${agent}`, { kind: "A", agent }));
  for (const kind of POPULATION_KINDS) table.push(cloneCase(success, kind, { kind }));
  return table;
}

/** A skill-use population with nothing missing, cloned from the example's need case: a need and
 * a near-miss case in a Legion pane for every partition skill and dispatch-first, and
 * dispatch-first's two once more in a plain Envoy session. */
function fullSkillUse(): SkillUseCase[] {
  const [need] = parseSkillUseFile(skillUseText).cases;
  if (need === undefined) throw new Error("the example lost its case");
  const clone = (id: string, fields: Partial<SkillUseCase>): SkillUseCase => {
    const copy: SkillUseCase = { ...structuredClone(need), id, ...fields };
    if (copy.kind !== "need") delete copy.withheldControl;
    if (copy.session !== "legion") delete copy.role;
    return copy;
  };
  const table: SkillUseCase[] = [];
  for (const skill of [...partition, "dispatch-first"]) {
    table.push(clone(`need-${skill}`, { skill }));
    table.push(clone(`near-miss-${skill}`, { skill, kind: "near-miss" }));
  }
  table.push(
    clone("plain-need-dispatch-first", { skill: "dispatch-first", session: "plain-envoy" })
  );
  table.push(
    clone("plain-near-miss-dispatch-first", {
      skill: "dispatch-first",
      session: "plain-envoy",
      kind: "near-miss",
    })
  );
  return table;
}

function without<T extends { id: string }>(table: T[], id: string): T[] {
  const kept = table.filter((each) => each.id !== id);
  if (kept.length !== table.length - 1) throw new Error(`the full table holds no case ${id}`);
  return kept;
}

function edited(
  table: HeldoutCase[],
  id: string,
  edit: (each: HeldoutCase) => void
): HeldoutCase[] {
  const copy = structuredClone(table);
  const target = copy.find((each) => each.id === id);
  if (target === undefined) throw new Error(`the full table holds no case ${id}`);
  edit(target);
  return copy;
}

describe("the inventory", () => {
  test("a full held-out population has no gap", () => {
    expect(heldoutGaps(HELDOUT_ROLES, agents, fullHeldout())).toEqual([]);
  });

  test("a full skill-use population has no gap", () => {
    expect(skillUseGaps(partition, fullSkillUse())).toEqual([]);
  });

  const heldoutRows: [string, (table: HeldoutCase[]) => HeldoutCase[], string][] = [
    ["a role's S case", (table) => without(table, "S-reviewer"), "role reviewer: no S case"],
    ["a role's B case", (table) => without(table, "B-merger"), "role merger: no B case"],
    [
      "a known-bad trace",
      (table) =>
        edited(table, "S-planner", (each) => {
          delete each.traces.bad;
        }),
      "case S-planner: no traces.bad",
    ],
    [
      "a planted omission",
      (table) =>
        edited(table, "B-tester", (each) => {
          delete each.plantedOmission;
        }),
      "case B-tester: no plantedOmission",
    ],
    ["an E case", (table) => without(table, "E-retro"), "role retro: no E case"],
    ["an agent's A case", (table) => without(table, "A-oracle"), "agent oracle: no A case"],
    ["Q3", (table) => without(table, "Q3"), "population: no Q3 case"],
    ["D1", (table) => without(table, "D1"), "population: no D1 case"],
  ];
  for (const [member, takeOut, gap] of heldoutRows) {
    test(`taking out ${member} leaves exactly its one gap`, () => {
      expect(heldoutGaps(HELDOUT_ROLES, agents, takeOut(fullHeldout()))).toEqual([gap]);
    });
  }

  const skillUseRows: [string, string, string][] = [
    ["a skill's near-miss case", "near-miss-legion-retro", "skill legion-retro: no near-miss case"],
    [
      "dispatch-first's need case in a plain Envoy session",
      "plain-need-dispatch-first",
      "skill dispatch-first: no plain-envoy need case",
    ],
  ];
  for (const [member, id, gap] of skillUseRows) {
    test(`taking out ${member} leaves exactly its one gap`, () => {
      expect(skillUseGaps(partition, without(fullSkillUse(), id))).toEqual([gap]);
    });
  }
});

describe("heldout-case.ts gaps", () => {
  const dir = temporaryDirectory("heldout-case-gaps");

  // Catches: a count that holds a development file to the held-out population, where a case batch
  // of that file (case-world.ts batchGaps) holds it to none, so the two disagree on whether the
  // file may run. The command names the gaps a batch of each file refuses to start on.
  test("names the gaps a batch of the file names: a held-out file's, and none of a development file", () => {
    const heldout = path.join(dir, "heldout.json");
    const devcase = path.join(dir, "devcase.json");
    writeFileSync(heldout, heldoutText);
    writeFileSync(devcase, JSON.stringify({ ...heldoutJson(), set: "devcase" }));
    expect(batchGaps(heldout, "heldout", "P05")).not.toEqual([]);
    expect(batchGaps(devcase, "devcase", "P05")).toEqual([]);
    for (const [file, set] of [
      [heldout, "heldout"],
      [devcase, "devcase"],
    ] as const) {
      const counted = batchGaps(file, set, "P05");
      const run = Bun.spawnSync([
        process.execPath,
        path.join(import.meta.dir, "heldout-case.ts"),
        "gaps",
        file,
      ]);
      expect({
        code: run.exitCode,
        stdout: run.stdout.toString(),
        stderr: run.stderr.toString(),
      }).toEqual({
        code: counted.length === 0 ? 0 : 1,
        stdout: `${file}: ${counted.length} gaps\n`,
        stderr: counted.map((gap) => `${file}: ${gap}\n`).join(""),
      });
    }
  });
});

describe("parseHeldoutFile", () => {
  test("the example parses: P05 an implementer S case, P10 a production-check B case", () => {
    const file = parseHeldoutFile(heldoutText);
    expect(file.set).toBe("heldout");
    expect(file.cases.map((each) => [each.id, each.role, each.kind])).toEqual([
      ["P05", "implementer", "S"],
      ["P10", "production-check", "B"],
    ]);
  });

  for (const part of ["world", "task", "actions", "rubric", "source"]) {
    test(`a case missing ${part} is refused, naming the case and ${part}`, () => {
      const file = heldoutJson();
      delete file.cases[0][part];
      expect(() => parseHeldoutFile(JSON.stringify(file))).toThrow(`case P05: ${part}:`);
    });
  }

  test("a key the schema does not name is refused, naming the case and the key", () => {
    const file = heldoutJson();
    file.cases[1].world.daemon.note = "an aside";
    expect(() => parseHeldoutFile(JSON.stringify(file))).toThrow(
      /case P10: world\.daemon: Unrecognized key: "note"/
    );
  });

  test("a time that is no RFC 3339 time is refused at its part, the windows over it included", () => {
    const file = heldoutJson();
    file.cases[1].world.clock = "yesterday";
    file.cases[1].actions[1].effect.wakeAt.before = "soon";
    const refusal = () => parseHeldoutFile(JSON.stringify(file));
    expect(refusal).toThrow("case P10: world.clock: an RFC 3339 time");
    expect(refusal).toThrow("case P10: actions[1].effect.wakeAt.before: an RFC 3339 time");
  });

  test("a case without its known-bad trace or planted omission parses; the inventory names both", () => {
    const file = heldoutJson();
    delete file.cases[0].traces.bad;
    delete file.cases[0].plantedOmission;
    const parsed = parseHeldoutFile(JSON.stringify(file));
    expect(heldoutGaps([], [], parsed.cases)).toEqual([
      "case P05: no traces.bad",
      "case P05: no plantedOmission",
      ...POPULATION_KINDS.map((kind) => `population: no ${kind} case`),
    ]);
  });

  test("an agent is named on an A case and on no other", () => {
    const missing = heldoutJson();
    missing.cases[0].kind = "A";
    delete missing.cases[0].plantedOmission;
    expect(() => parseHeldoutFile(JSON.stringify(missing))).toThrow("case P05: agent:");
    const extra = heldoutJson();
    extra.cases[0].agent = "oracle";
    expect(() => parseHeldoutFile(JSON.stringify(extra))).toThrow("case P05: agent:");
  });

  test("an A case may name no action and run under no daemon, and each trace carries its report", () => {
    const agentCase = () => {
      const file = heldoutJson();
      Object.assign(file.cases[0], {
        kind: "A",
        agent: "oracle",
        actions: [],
        allowedRefusals: [],
      });
      Object.assign(file.cases[0], { forbidden: [], waitRequired: null });
      file.cases[0].world.daemon = null;
      delete file.cases[0].plantedOmission;
      file.cases[0].traces.good.agentReport = "The trade-off, with its evidence.";
      file.cases[0].traces.bad.agentReport = "An answer with no evidence.";
      return file;
    };
    expect(parseHeldoutFile(JSON.stringify(agentCase())).cases[0].kind).toBe("A");
    const noReport = agentCase();
    delete noReport.cases[0].traces.bad.agentReport;
    expect(() => parseHeldoutFile(JSON.stringify(noReport))).toThrow(
      "case P05: traces.bad.agentReport: an A case's trace carries its report"
    );
    const roleCase = heldoutJson();
    roleCase.cases[0].actions = [];
    roleCase.cases[0].allowedRefusals = [];
    roleCase.cases[0].forbidden = [];
    expect(() => parseHeldoutFile(JSON.stringify(roleCase))).toThrow("case P05: actions:");
    const noDaemon = heldoutJson();
    noDaemon.cases[0].world.daemon = null;
    expect(() => parseHeldoutFile(JSON.stringify(noDaemon))).toThrow("case P05: world.daemon:");
  });
});

describe("parseSkillUseFile", () => {
  test("the example parses: one need case for legion-retro in a Legion pane", () => {
    const file = parseSkillUseFile(skillUseText);
    expect(file.set).toBe("skilluse");
    expect(file.cases.map((each) => [each.skill, each.kind, each.session, each.role])).toEqual([
      ["legion-retro", "need", "legion", "retro"],
    ]);
  });

  test("a need case without its withheld control is refused, naming it", () => {
    const file = JSON.parse(skillUseText);
    const id = file.cases[0].id;
    delete file.cases[0].withheldControl;
    expect(() => parseSkillUseFile(JSON.stringify(file))).toThrow(`case ${id}: withheldControl:`);
  });
});

describe("parseOutcome", () => {
  const trace = () => heldoutJson().cases[0].traces.good;

  test("a known-good trace parses as an outcome", () => {
    expect(parseOutcome(JSON.stringify(trace())).actions.length).toBeGreaterThan(1);
  });

  test("a seq that does not rise is refused, naming the action", () => {
    const repeated = trace();
    repeated.actions[1].seq = repeated.actions[0].seq;
    expect(() => parseOutcome(JSON.stringify(repeated))).toThrow("actions[1].seq:");
    const falling = trace();
    falling.actions[1].seq = falling.actions[0].seq - 1;
    expect(() => parseOutcome(JSON.stringify(falling))).toThrow("actions[1].seq:");
  });

  test("an arm label is no part of an outcome", () => {
    const labelled = { ...trace(), arm: "cut" };
    expect(() => parseOutcome(JSON.stringify(labelled))).toThrow(/Unrecognized key: "arm"/);
  });
});

describe("the carried kinds, the workspace and staging's job and source", () => {
  const good = (file: ReturnType<typeof heldoutJson>) => file.cases[0].traces.good;
  const envoy = (seq: number) => ({
    kind: "envoy",
    seq,
    at: "2030-01-07T15:05:00Z",
    tool: "publish",
    target: "notifications.role.acme.acme-12-tester",
    fields: { message: "ACME-12 is pushed.", expects_reply: false },
  });
  const read = (seq: number) => ({
    kind: "dispatch-read",
    seq,
    at: "2030-01-07T15:05:10Z",
    command: "handoff-read",
    target: "ACME-12",
    fields: { phase: "implement" },
  });
  const agent = (seq: number) => ({
    kind: "agent",
    seq,
    at: "2030-01-07T15:05:20Z",
    agent: "task",
    name: "ReviewDiff",
    fields: { task: "Read the diff." },
    endedAt: "2030-01-07T15:05:40Z",
  });

  test("an action of each kind parses on the example's trace, a record without them still does, and each keeps its seq among the others", () => {
    const file = heldoutJson();
    good(file).actions.push(envoy(6), read(7), agent(8));
    const parsed = parseHeldoutFile(JSON.stringify(file));
    expect(parsed.cases[0]?.traces.good.actions.map((action) => action.kind)).toEqual([
      "push",
      "github",
      "dispatch",
      "dispatch",
      "daemon",
      "envoy",
      "dispatch-read",
      "agent",
    ]);
    // The agent's end is optional, as a task agent that wrote no transcript has none.
    delete good(file).actions[7].endedAt;
    expect(() => parseHeldoutFile(JSON.stringify(file))).not.toThrow();
    // seq rises across every kind.
    good(file).actions[6].seq = 5;
    expect(() => parseHeldoutFile(JSON.stringify(file))).toThrow(
      "case P05: traces.good.actions[6].seq: seq 5 does not rise above seq 6"
    );
  });

  const refused: [string, (action: Record<string, unknown>) => void, string][] = [
    ["an envoy action of another tool", (a) => (a.tool = "broadcast"), "tool"],
    ["an envoy action with no target", (a) => delete a.target, "target"],
    ["an envoy action with a blank target", (a) => (a.target = " "), "target"],
    ["a field the kind does not name", (a) => (a.topic = "x"), 'Unrecognized key: "topic"'],
  ];
  for (const [what, edit, message] of refused)
    test(`${what} is refused at its part`, () => {
      const file = heldoutJson();
      good(file).actions.push(envoy(6));
      edit(good(file).actions[5]);
      expect(() => parseHeldoutFile(JSON.stringify(file))).toThrow(message);
    });

  test("a dispatch-read and an agent keep the fields the contract names, and refuse a missing name or command", () => {
    const file = heldoutJson();
    good(file).actions.push(read(6), agent(7));
    delete good(file).actions[6].name;
    expect(() => parseHeldoutFile(JSON.stringify(file))).toThrow("case P05: traces.good");
    const noCommand = heldoutJson();
    good(noCommand).actions.push(read(6));
    delete good(noCommand).actions[5].command;
    expect(() => parseHeldoutFile(JSON.stringify(noCommand))).toThrow("case P05: traces.good");
  });

  test("a route names each carried kind and its fields, and refuses a field the kind lacks; a route of a T0 kind takes no carried action", () => {
    const file = heldoutJson();
    const routes = file.cases[0].actions[0].routes;
    routes.push(
      { kind: "envoy", match: { tool: "publish", fields: { expects_reply: false } } },
      { kind: "dispatch-read", match: { command: "handoff-read", target: "ACME-12" } },
      {
        kind: "agent",
        match: { agent: "task" },
        between: [
          { field: "endedAt", after: "2030-01-07T15:00:00Z", before: "2030-01-07T16:00:00Z" },
        ],
      }
    );
    expect(() => parseHeldoutFile(JSON.stringify(file))).not.toThrow();
    routes.push({ kind: "envoy", match: { command: "x" } });
    expect(() => parseHeldoutFile(JSON.stringify(file))).toThrow(
      "a envoy action has no field command; its fields are tool, target, fields"
    );
    const t0 = heldoutJson();
    t0.cases[0].actions[0].routes.push({ kind: "dispatch", match: { tool: "publish" } });
    expect(() => parseHeldoutFile(JSON.stringify(t0))).toThrow(
      "a dispatch action has no field tool"
    );
  });

  test("a staging action's job and source are optional, and each a text", () => {
    const staging = (extra: Record<string, unknown>) => ({
      kind: "staging",
      seq: 6,
      at: "2030-01-07T15:05:00Z",
      tree: "a".repeat(40),
      files: ["app.ts"],
      ...extra,
    });
    for (const extra of [{}, { job: "build" }, { job: "build", source: "$TMPDIR/stage-a" }]) {
      const file = heldoutJson();
      good(file).actions.push(staging(extra));
      expect(() => parseHeldoutFile(JSON.stringify(file))).not.toThrow();
    }
    const blank = heldoutJson();
    good(blank).actions.push(staging({ source: " " }));
    expect(() => parseHeldoutFile(JSON.stringify(blank))).toThrow("source");
  });

  test("the workspace is an optional list of operations in time order, each with its end, description and commits", () => {
    const sha = (n: number) => String(n).repeat(40);
    const file = heldoutJson();
    good(file).workspace = [
      { at: "2030-01-07T15:00:01Z", description: "add workspace 'default'", standingOn: [sha(1)] },
      { at: "2030-01-07T15:02:00Z", description: "commit 3797d8d1", standingOn: [sha(2)] },
    ];
    expect(parseHeldoutFile(JSON.stringify(file)).cases[0]?.traces.good.workspace).toHaveLength(2);
    good(file).workspace.reverse();
    expect(() => parseHeldoutFile(JSON.stringify(file))).toThrow(
      "case P05: traces.good.workspace[1].at: 2030-01-07T15:00:01Z is before"
    );
    good(file).workspace = [{ at: "2030-01-07T15:00:01Z", description: "x", standingOn: ["abc"] }];
    expect(() => parseHeldoutFile(JSON.stringify(file))).toThrow("a commit: 40 lower-case");
    good(file).workspace = [];
    expect(() => parseHeldoutFile(JSON.stringify(file))).not.toThrow();
  });

  test("the carried-kinds file parses and keeps the example's worlds and rubrics", () => {
    const file = parseHeldoutFile(
      readFileSync(
        path.join(
          import.meta.dir,
          "../../../daemon/internal/daemon/testdata/teardown-effects/carried-kinds-cases.json"
        ),
        "utf8"
      )
    );
    const example = parseHeldoutFile(heldoutText);
    expect(file.cases.map((each) => each.world)).toEqual(example.cases.map((each) => each.world));
    expect(file.cases.map((each) => each.rubric)).toEqual(example.cases.map((each) => each.rubric));
    const kinds = new Set<string>(
      file.cases.flatMap((each) => each.traces.good.actions.map((a) => a.kind))
    );
    expect([...kinds].sort()).toEqual(
      ["agent", "daemon", "dispatch", "dispatch-read", "envoy", "github", "push"].sort()
    );
    // The same daemon actions as the example's, moved in seq and not in time or answer.
    const daemon = (cases: typeof file.cases) =>
      cases.flatMap((each) =>
        each.traces.good.actions.flatMap((a) =>
          a.kind === "daemon" ? [[a.at, a.op, a.answer]] : []
        )
      );
    expect(daemon(file.cases)).toEqual(daemon(example.cases));
  });
});

// biome-ignore lint/suspicious/noExplicitAny: a world as parsed JSON, which each row edits at will.
type WorldJson = Record<string, any>;

describe("the typed world parts", () => {
  const sha = "7c2e9a4b6d8f0a1c3e5b7d9f2a4c6e8b0d1f3a5c";
  /** The example's P05 world with `edit` made to it. */
  const parsedWorld = (edit: (world: WorldJson) => void) => {
    const file = heldoutJson();
    edit(file.cases[0].world);
    return parseHeldoutFile(JSON.stringify(file)).cases[0]?.world;
  };
  const run = (extra: Record<string, unknown>) => ({
    task: "default",
    step: 1,
    kind: "step",
    role: "planner",
    result: "passed",
    startedAt: "2030-01-07T14:00:00Z",
    endedAt: "2030-01-07T14:30:00Z",
    ...extra,
  });

  test("a world with every part parses, and the example's world means what it meant", () => {
    const world = parsedWorld((w) => {
      w.daemon.claim = { issue: "ACME-12", tree: "ACME-10" };
      w.daemon.issue = {
        tree: "ACME-10",
        parent: "ACME-10",
        generation: 2,
        lingerUntil: "2030-01-08T00:00:00Z",
      };
      w.daemon.entry = { phase: 1, turn: 2, owedTask: null, movedBack: true };
      w.daemon.architect = { state: "suspended" };
      w.daemon.records = [
        {
          key: "ACME-13",
          tree: "ACME-10",
          parent: "ACME-10",
          phase: "planning",
          status: "in_progress",
          generation: 1,
          architect: { state: "live" },
        },
      ];
      w.daemon.admission = { cap: 3, active: ["ACME-12"], waiting: ["ACME-13"], blocked: [] };
      w.daemon.gate = [
        {
          step: "design",
          document: "design.md",
          current: 2,
          approved: 1,
          artifactId: "art-design",
        },
        { step: "plan", document: "plan.md", current: 1, approved: null },
      ];
      w.dispatch.issue.labels = ["legion"];
      w.dispatch.issues = [
        {
          key: "ACME-13",
          status: "todo",
          parent: null,
          rank: 1,
          labels: [],
          blockedBy: ["ACME-14"],
          claimedBy: null,
        },
        {
          key: "ACME-14",
          title: "Another issue",
          status: "done",
          parent: "ACME-10",
          rank: null,
          labels: ["legion"],
          blockedBy: [],
          claimedBy: "ses-acme-14",
        },
      ];
      w.dispatch.steps = [
        run({}),
        run({ step: 0, kind: "retro", role: "implementer", planRating: 4, planReason: "Clear." }),
        run({ step: 0, kind: "merge", role: null, result: null, endedAt: null }),
      ];
      w.github.settings = { default_branch: "main", allow_squash_merge: true };
      w.github.branchRules = [{ branch: "main", rules: [{ type: "pull_request" }] }];
      w.github.pullRequests.push({
        number: 3,
        state: "open",
        draft: false,
        head: { ref: "legion/ACME-13", sha },
        base: "main",
        title: "A pull request",
        body: "",
        mergeCommit: null,
        reviews: [],
        reviewThreads: [],
        checks: [],
        mergeStateStatus: "CLEAN",
        autoMergeRequest: null,
        nodeId: "PR_kwDOA",
        diff: "diff --git a/x b/x\n",
        mergeResult: { merged: true },
      });
    });
    expect(world?.daemon?.entry).toEqual({ phase: 1, turn: 2, owedTask: null, movedBack: true });
    expect(world?.dispatch.issues?.[0]?.title).toBeUndefined();
    expect(world?.dispatch.steps?.map((each) => [each.kind, each.step])).toEqual([
      ["step", 1],
      ["retro", 0],
      ["merge", 0],
    ]);
    const plain = parsedWorld(() => {});
    for (const part of ["claim", "issue", "entry", "architect", "records", "admission", "gate"])
      expect(plain?.daemon).not.toHaveProperty(part);
    expect(plain?.dispatch).not.toHaveProperty("steps");
    expect(plain?.github).not.toHaveProperty("settings");
  });

  const refusals: [string, (w: WorldJson) => void, string][] = [
    [
      "a step run at place 0",
      (w) => (w.dispatch.steps = [run({ step: 0 })]),
      "a step run's place is 1 or more, and a retro's or a merge's is 0",
    ],
    [
      "a retro at place 1",
      (w) => (w.dispatch.steps = [run({ kind: "retro", step: 1 })]),
      "dispatch.steps[0].step",
    ],
    [
      "a run with a result and no end",
      (w) => (w.dispatch.steps = [run({ endedAt: null })]),
      "a run that has ended has a result and an end",
    ],
    [
      "a rating on a step run",
      (w) => (w.dispatch.steps = [run({ planRating: 3 })]),
      "only a retro that ended rates its plan",
    ],
    [
      "a rating of 6",
      (w) => (w.dispatch.steps = [run({ kind: "retro", step: 0, planRating: 6 })]),
      "dispatch.steps[0].planRating",
    ],
    [
      "the case's own issue among the other issues",
      (w) =>
        (w.dispatch.issues = [
          {
            key: "ACME-12",
            status: "todo",
            parent: null,
            rank: null,
            labels: [],
            blockedBy: [],
            claimedBy: null,
          },
        ]),
      "the case's own issue is no other issue",
    ],
    [
      "a rank of 0",
      (w) =>
        (w.dispatch.issues = [
          {
            key: "ACME-13",
            status: "todo",
            parent: null,
            rank: 0,
            labels: [],
            blockedBy: [],
            claimedBy: null,
          },
        ]),
      "dispatch.issues[0].rank",
    ],
    [
      "the case's own issue among the daemon's records",
      (w) =>
        (w.daemon.records = [
          {
            key: "ACME-12",
            tree: "ACME-10",
            parent: null,
            phase: "planning",
            status: "todo",
            generation: 1,
            architect: null,
          },
        ]),
      "the case's own issue is daemon.issue",
    ],
    [
      "an entry of phase 0",
      (w) => (w.daemon.entry = { phase: 0, turn: 1, owedTask: null }),
      "daemon.entry.phase",
    ],
    [
      "two gate steps of one name",
      (w) =>
        (w.daemon.gate = [
          { step: "design", document: "design.md", current: 1, approved: null },
          { step: "design", document: "design.md", current: 2, approved: null },
        ]),
      "daemon.gate[1].step",
    ],
    ["a claim with no tree", (w) => (w.daemon.claim = { issue: "ACME-12" }), "daemon.claim.tree"],
    [
      "a negative admission cap",
      (w) => (w.daemon.admission = { cap: -1, active: [], waiting: [], blocked: [] }),
      "daemon.admission.cap",
    ],
    [
      "a branch named twice among the branch rules",
      (w) =>
        (w.github.branchRules = [
          { branch: "main", rules: [] },
          { branch: "main", rules: [] },
        ]),
      "github.branchRules[1].branch",
    ],
    [
      "a part the schema does not name",
      (w) => (w.daemon.records2 = []),
      'Unrecognized key: "records2"',
    ],
  ];
  for (const [what, edit, message] of refusals)
    test(`${what} is refused, naming the case and the part`, () => {
      expect(() => parsedWorld(edit)).toThrow(message);
    });

  test("each part is one a refused call can be held to leave unchanged", () => {
    const file = heldoutJson();
    file.cases[0].allowedRefusals = [
      {
        code: "HANDOFF_NO_PULL_REQUEST",
        action: file.cases[0].actions[4].name,
        unchanged: ["daemon.records", "daemon.admission", "dispatch.steps", "github.settings"],
      },
    ];
    expect(() => parseHeldoutFile(JSON.stringify(file))).not.toThrow();
    file.cases[0].allowedRefusals[0].unchanged = ["daemon.nothing"];
    expect(() => parseHeldoutFile(JSON.stringify(file))).toThrow("a part of the world");
  });
});
