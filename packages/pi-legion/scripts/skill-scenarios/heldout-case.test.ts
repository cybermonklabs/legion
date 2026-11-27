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
