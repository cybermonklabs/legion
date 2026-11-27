import { beforeAll, describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
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

  /** P05's world given the runs of its task's steps as `dispatch steps` lists them: the
   * planner's and the implementer's ended, the retro's ended with the rating it gave the plan, and
   * the merge still running. */
  const steppedFile = () => {
    const file = heldoutJson();
    file.cases[0].world.dispatch.steps = [
      {
        task: "t1",
        step: 1,
        kind: "step",
        role: "planner",
        result: "passed",
        startedAt: "2030-01-07T12:40:00Z",
        endedAt: "2030-01-07T13:10:30Z",
      },
      {
        task: "t1",
        step: 2,
        kind: "step",
        role: "implementer",
        result: "passed",
        startedAt: "2030-01-07T13:10:30Z",
        endedAt: "2030-01-07T13:50:30Z",
      },
      {
        task: "t1",
        step: 0,
        kind: "retro",
        role: "implementer",
        result: "passed",
        startedAt: "2030-01-07T13:50:30Z",
        endedAt: "2030-01-07T14:00:00Z",
        planRating: 4,
        planReason: "The plan fit the work.",
      },
      {
        task: "t1",
        step: 0,
        kind: "merge",
        role: "merger",
        result: null,
        startedAt: "2030-01-07T14:00:00Z",
        endedAt: null,
      },
    ];
    return file;
  };
  /** The parse of `steppedFile()` after `edit` changed its step runs. */
  const steppedRefusal = (edit: (steps: Record<string, unknown>[]) => void) => () => {
    const file = steppedFile();
    edit(file.cases[0].world.dispatch.steps);
    return parseHeldoutFile(JSON.stringify(file));
  };

  test("a world's step runs parse, a run with no role among them, and a step at step 0, a retro or a merge at another step, and a run that ended before it started are refused", () => {
    expect(parseHeldoutFile(JSON.stringify(steppedFile())).cases[0].world.dispatch.steps).toEqual(
      steppedFile().cases[0].world.dispatch.steps
    );
    expect(
      steppedRefusal((steps) => {
        Object.assign(steps[3] ?? {}, { role: null });
      })
    ).not.toThrow();
    expect(
      steppedRefusal((steps) => {
        Object.assign(steps[0] ?? {}, { step: 0 });
      })
    ).toThrow("case P05: world.dispatch.steps[0].step: a step is at step 1 or more, not 0");
    expect(
      steppedRefusal((steps) => {
        Object.assign(steps[2] ?? {}, { step: 1 });
      })
    ).toThrow("case P05: world.dispatch.steps[2].step: a retro is at step 0, not 1");
    expect(
      steppedRefusal((steps) => {
        Object.assign(steps[3] ?? {}, { step: 5 });
      })
    ).toThrow("case P05: world.dispatch.steps[3].step: a merge is at step 0, not 5");
    expect(
      steppedRefusal((steps) => {
        Object.assign(steps[0] ?? {}, { endedAt: "2030-01-07T12:30:00Z" });
      })
    ).toThrow(
      "case P05: world.dispatch.steps[0].endedAt: 2030-01-07T12:30:00Z is not after startedAt 2030-01-07T12:40:00Z"
    );
  });

  test("a run's result and endedAt are both null or both set", () => {
    expect(
      steppedRefusal((steps) => {
        Object.assign(steps[3] ?? {}, { result: "passed" });
      })
    ).toThrow("case P05: world.dispatch.steps[3].endedAt: a run that holds its result has ended");
    expect(
      steppedRefusal((steps) => {
        Object.assign(steps[0] ?? {}, { result: null });
      })
    ).toThrow("case P05: world.dispatch.steps[0].result: a run that ended holds its result");
  });

  test("a plan's rating and its reason are held by a retro that ended alone, and a rating is 1 to 5", () => {
    expect(
      steppedRefusal((steps) => {
        Object.assign(steps[1] ?? {}, { planRating: 3 });
      })
    ).toThrow(
      "case P05: world.dispatch.steps[1].planRating: a step holds no planRating: only a retro rates the plan"
    );
    expect(
      steppedRefusal((steps) => {
        Object.assign(steps[3] ?? {}, { planReason: "The merge waited." });
      })
    ).toThrow(
      "case P05: world.dispatch.steps[3].planReason: a merge holds no planReason: only a retro rates the plan"
    );
    expect(
      steppedRefusal((steps) => {
        Object.assign(steps[2] ?? {}, { result: null, endedAt: null });
      })
    ).toThrow(
      "case P05: world.dispatch.steps[2].planRating: a running retro holds no planRating: a retro rates the plan as it ends"
    );
    expect(
      steppedRefusal((steps) => {
        Object.assign(steps[2] ?? {}, { planRating: 6 });
      })
    ).toThrow("case P05: world.dispatch.steps[2].planRating:");
  });

  // Catches: a world that still names a run by a case id, or a run's session or report, which
  // `dispatch steps` lists none of, parsing as one of the shape's runs.
  test("a step run that names an id, a session or a report is refused", () => {
    for (const [part, value] of [
      ["id", "stp-acme-12-plan"],
      ["session", "ses-acme-12-planner"],
      ["report", "hnd-acme-12-plan-1"],
    ] as const)
      expect(
        steppedRefusal((steps) => {
          Object.assign(steps[0] ?? {}, { [part]: value });
        })
      ).toThrow(`case P05: world.dispatch.steps[0]: Unrecognized key: "${part}"`);
  });

  test("a comment's anchor names a document of the world and words of its body, and a reply names an earlier comment and holds no anchor", () => {
    /** P05's world with the implementer's comment on words of its Verification document and the
     * architect's reply in that thread, after `edit` changed the two comments. */
    const threaded = (edit: (comments: Record<string, unknown>[]) => void = () => {}) => {
      const file = heldoutJson();
      file.cases[0].world.dispatch.comments = [
        {
          id: "cmt-acme-12-departure",
          author: "ses-acme-12-implementer",
          at: "2030-01-07T13:35:00Z",
          body: "For the architect: the reader keeps no variant. Please confirm it in this thread.",
          anchor: { document: "verification.md", quote: "carry `experiment` and `variant`" },
        },
        {
          id: "cmt-acme-12-decision",
          author: "ses-acme-12-architect",
          at: "2030-01-07T13:37:00Z",
          body: "Take it.",
          replyTo: "cmt-acme-12-departure",
        },
      ];
      edit(file.cases[0].world.dispatch.comments);
      return file;
    };
    const refusal = (edit: (comments: Record<string, unknown>[]) => void) => () =>
      parseHeldoutFile(JSON.stringify(threaded(edit)));
    expect(parseHeldoutFile(JSON.stringify(threaded())).cases[0].world.dispatch.comments).toEqual(
      threaded().cases[0].world.dispatch.comments
    );
    expect(
      refusal((comments) => {
        Object.assign(comments[0] ?? {}, { anchor: { document: "plan.md", quote: "carry" } });
      })
    ).toThrow(
      "case P05: world.dispatch.comments[0].anchor.document: plan.md is no document of the world"
    );
    expect(
      refusal((comments) => {
        Object.assign(comments[0] ?? {}, {
          anchor: { document: "verification.md", quote: "The flag's default is 3." },
        });
      })
    ).toThrow(
      `case P05: world.dispatch.comments[0].anchor.quote: "The flag's default is 3." is not in the body of verification.md`
    );
    expect(
      refusal((comments) => {
        Object.assign(comments[0] ?? {}, { at: "2030-01-07T13:40:00Z" });
      })
    ).toThrow(
      "case P05: world.dispatch.comments[1].replyTo: cmt-acme-12-departure is no earlier comment of the world"
    );
    expect(
      refusal((comments) => {
        Object.assign(comments[1] ?? {}, { replyTo: "cmt-acme-12-decision" });
      })
    ).toThrow(
      "case P05: world.dispatch.comments[1].replyTo: cmt-acme-12-decision is no earlier comment of the world"
    );
    expect(
      refusal((comments) => {
        Object.assign(comments[1] ?? {}, {
          anchor: { document: "verification.md", quote: "carry" },
        });
      })
    ).toThrow("case P05: world.dispatch.comments[1].anchor: a reply holds no anchor");
  });

  test("a world with no steps and no anchors parses as before", () => {
    const file = parseHeldoutFile(heldoutText);
    expect(file).toEqual(heldoutJson());
    for (const each of file.cases) {
      expect(each.world.dispatch.steps).toBeUndefined();
      for (const comment of each.world.dispatch.comments) {
        expect(comment.anchor).toBeUndefined();
        expect(comment.replyTo).toBeUndefined();
      }
    }
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
