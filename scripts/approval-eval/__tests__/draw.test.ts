import { afterEach, describe, expect, spyOn, test } from "bun:test";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { CaseKind, CaseRow } from "../build-corpus";
import { CorpusError, UsageError } from "../corpus-io";
import {
  claimCutRefusal,
  type Draw,
  type DrawCase,
  DrawError,
  drawSlices,
  type FinalTestSource,
  main,
  pinnedBacklogRoots,
  renderingCheckOf,
  TARGETS,
} from "../draw";
import { drawPath, readDraw, rootFactsOf, SLICES } from "../draw-io";
import { type AskModel, type NeverDecideAnswer, neverDecide } from "../never-decide";
import type { TreeOutcome } from "../trees";

/**
 * The draw over a hand-written corpus: nothing here renders a document or reads Dispatch, and
 * the one file read is the fixture a test writes for the roots, since the recorded corpus's own
 * draw holds no malformed root to drive either reader of them with. What it holds the draw to
 * is the plan's own rules - a group goes into one slice whole, a seed reproduces a draw, and a
 * held or undrawable case is left out of every slice - and the roots it is given to the one
 * policy both readers of that shape read under.
 */

const DECIDED: NeverDecideAnswer = {
  held: false,
  kind: null,
  reason: "the document asks for no act the agent never decides",
  answer: "none\nthe document asks for no act the agent never decides",
  source: "model",
  fault: null,
  servedModel: "anthropic/claude-opus-5-5",
  inputTokens: 900,
  outputTokens: 30,
};

/** One case, with only the fields the draw reads named. `issue` is the group it belongs to. */
function caseRow(
  caseId: string,
  kind: CaseKind,
  issue: string,
  options = 2,
  selected: string[] = ["Option 0"]
): CaseRow {
  return {
    caseId,
    eventId: 1,
    askId: `ask-${caseId}`,
    issueKey: issue,
    project: "PROJ",
    kind,
    urgency: "med",
    multiple: false,
    question: `Question of ${caseId}?`,
    options: Array.from({ length: options }, (_, index) => ({
      id: `b${index}`,
      label: `Option ${index}`,
      description: null,
    })),
    selected,
    words: null,
    hasWords: false,
    askCreatedAt: "2026-11-06T09:00:00Z",
    answeredAt: "2026-11-06T12:00:00Z",
    eventCreatedAt: "2026-11-06T12:00:00Z",
    approval:
      kind === "approval"
        ? {
            documents: [
              { artifactId: `document-${issue}`, name: "spec.md", version: 3, requestedVersion: 3 },
            ],
            artifactSlug: "spec",
            artifactPrimary: true,
          }
        : null,
    verdict: kind === "approval" ? "approve" : null,
    confounds: {
      version: 3,
      answersThatDayBefore: 0,
      hourUtc: 12,
      hourPacific: 4,
      changesAlreadyRequested: false,
      priorCasesOnDocument: 0,
      caseKind: kind,
    },
  };
}

/** One case as the draw is given it. */
function drawCase(row: CaseRow, hold: NeverDecideAnswer = DECIDED): DrawCase {
  return {
    row,
    hold,
    documentId: row.kind === "approval" ? (row.approval?.documents[0]?.artifactId ?? null) : null,
    requestedVersion: row.kind === "approval" ? 3 : null,
    sha256: `sha-of-${row.caseId}`,
  };
}

/**
 * A corpus of 420 approval cases over 140 issues and 120 question cases over 40 further issues,
 * each issue holding three cases, which is enough to fill every slice and leave a reserve.
 */
function corpus(): DrawCase[] {
  const cases: DrawCase[] = [];
  for (let group = 0; group < 140; group += 1) {
    for (let at = 0; at < 3; at += 1) {
      cases.push(drawCase(caseRow(`a-${group}-${at}`, "approval", `ISSUE-a-${group}`)));
    }
  }
  for (let group = 0; group < 40; group += 1) {
    for (let at = 0; at < 3; at += 1) {
      cases.push(drawCase(caseRow(`q-${group}-${at}`, "question", `ISSUE-q-${group}`)));
    }
  }
  return cases;
}

/** Which slice each case landed in. */
function placement(draw: Draw): Map<string, string> {
  const where = new Map<string, string>();
  for (const slice of SLICES) {
    for (const caseId of draw.slices[slice]) where.set(caseId, slice);
  }
  return where;
}

describe("drawSlices", () => {
  test("puts every case of a group into one slice", () => {
    const draw = drawSlices(corpus(), 7);
    const where = placement(draw);
    const byGroup = new Map<string, Set<string>>();
    for (const [caseId, slice] of where) {
      const group = caseId.slice(0, caseId.lastIndexOf("-"));
      const slices = byGroup.get(group) ?? new Set<string>();
      slices.add(slice);
      byGroup.set(group, slices);
    }
    expect(byGroup.size).toBe(180);
    for (const [group, slices] of byGroup) {
      expect([group, [...slices].length]).toEqual([group, 1]);
    }
  });

  test("draws every case into exactly one slice", () => {
    const cases = corpus();
    const draw = drawSlices(cases, 7);
    const drawn = SLICES.flatMap((slice) => draw.slices[slice]);
    expect(new Set(drawn).size).toBe(drawn.length);
    expect(drawn.length).toBe(cases.length);
  });

  test("reaches each target to within one group", () => {
    const draw = drawSlices(corpus(), 7);
    for (const kind of ["approval", "question"] as const) {
      for (const slice of ["fit", "report", "anchor"] as const) {
        const reached = draw.counts[slice][kind];
        expect(reached).toBeLessThanOrEqual(TARGETS[kind][slice]);
        expect(reached).toBeGreaterThan(TARGETS[kind][slice] - 3);
      }
    }
  });

  test("the same seed draws the same slices and another seed does not", () => {
    const again = drawSlices(corpus(), 12345);
    expect(drawSlices(corpus(), 12345).slices).toEqual(again.slices);
    expect(drawSlices(corpus(), 12346).slices).not.toEqual(again.slices);
  });

  test("the draw turns on the seed and not on the order the cases are held in", () => {
    const ordered = corpus();
    const draw = drawSlices(ordered, 99);
    expect(drawSlices([...ordered].reverse(), 99).slices).toEqual(draw.slices);
  });

  test("leaves a held case out of every slice and says which kind held it", () => {
    const cases = corpus();
    const held = cases[0];
    if (held === undefined) throw new Error("the corpus is empty");
    cases[0] = drawCase(held.row, {
      ...DECIDED,
      held: true,
      kind: "destructive-act",
      reason: "The change list asks for the spool directory to be removed.",
      answer: "destructive-act\nThe change list asks for the spool directory to be removed.",
    });
    const draw = drawSlices(cases, 7);
    expect(placement(draw).has(held.row.caseId)).toBe(false);
    expect(draw.held.map((one) => one.caseId)).toEqual([held.row.caseId]);
    expect(draw.heldByKind).toEqual({ "destructive-act": 1 });
    expect(draw.undrawable).toHaveLength(0);
  });

  test("leaves a question offering fewer than two options out of every slice", () => {
    const cases = corpus();
    cases.push(drawCase(caseRow("q-none", "question", "ISSUE-none", 0)));
    cases.push(drawCase(caseRow("q-one", "question", "ISSUE-one", 1)));
    const draw = drawSlices(cases, 7);
    const where = placement(draw);
    expect(where.has("q-none")).toBe(false);
    expect(where.has("q-one")).toBe(false);
    expect(draw.undrawable.map((one) => one.caseId).sort()).toEqual(["q-none", "q-one"]);
    expect(draw.undrawable[0]?.reason).toMatch(/cannot be judged as a choice/);
    expect(draw.undrawableByKind).toEqual({
      "too-few-options": 2,
      "no-option-chosen": 0,
      "rule-unreadable": 0,
    });
  });

  test("leaves a question the person answered with no option out of every slice", () => {
    const cases = corpus();
    cases.push(drawCase(caseRow("q-empty", "question", "ISSUE-empty", 2, [])));
    const draw = drawSlices(cases, 7);
    expect(placement(draw).has("q-empty")).toBe(false);
    expect(draw.undrawable.map((one) => one.caseId)).toEqual(["q-empty"]);
    expect(draw.undrawable[0]?.undrawableKind).toBe("no-option-chosen");
    expect(draw.undrawable[0]?.reason).toMatch(/cannot be scored by set equality/);
    expect(draw.undrawableByKind).toEqual({
      "too-few-options": 0,
      "no-option-chosen": 1,
      "rule-unreadable": 0,
    });
  });

  test("refuses the whole draw over an approval case answered with no option", () => {
    const cases = corpus();
    cases.push(drawCase(caseRow("a-empty", "approval", "ISSUE-empty", 2, [])));
    expect(() => drawSlices(cases, 7)).toThrow(DrawError);
    expect(() => drawSlices(cases, 7)).toThrow(/a-empty/);
  });

  test("refuses a case that carries no answer of the rule, which a draw outside a read applies", () => {
    const cases = corpus();
    const first = cases[0];
    if (first === undefined) throw new Error("the corpus is empty");
    cases[0] = { ...first, hold: null };
    expect(() => drawSlices(cases, 7)).toThrow(DrawError);
    expect(() => drawSlices(cases, 7)).toThrow(
      `the case ${first.row.caseId} carries no never-decide`
    );
  });

  test("writes an anchor descriptor for every anchor case", () => {
    const draw = drawSlices(corpus(), 7);
    expect(draw.anchors.map((one) => one.caseId).sort()).toEqual([...draw.slices.anchor].sort());
    for (const anchor of draw.anchors) {
      expect(anchor.sha256).toBe(`sha-of-${anchor.caseId}`);
      expect(anchor.askId).toBe(`ask-${anchor.caseId}`);
      if (anchor.kind === "approval") {
        expect(anchor.requestedVersion).toBe(3);
        expect(anchor.documentId).toMatch(/^document-/);
      }
    }
  });

  test("two documents of one issue are one group, so they land in one slice", () => {
    const other = caseRow("a-2", "approval", "ISSUE-7");
    const cases = [
      drawCase(caseRow("a-1", "approval", "ISSUE-7")),
      drawCase({
        ...other,
        approval: {
          documents: [
            { artifactId: "document-other", name: "other.md", version: 1, requestedVersion: 1 },
          ],
          artifactSlug: "other",
          artifactPrimary: false,
        },
      }),
    ];
    const draw = drawSlices(cases, 3);
    expect(draw.groupKey).toBe("issue");
    expect(draw.groupCount).toBe(1);
    expect(draw.groups).toEqual({ approval: 1, question: 0 });
    const where = placement(draw);
    expect(where.get("a-1")).toBe(where.get("a-2"));
  });

  test("an approval case and a question case of one issue are one group", () => {
    const cases = [
      drawCase(caseRow("a-1", "approval", "ISSUE-7")),
      drawCase(caseRow("q-1", "question", "ISSUE-7")),
    ];
    const draw = drawSlices(cases, 3);
    expect(draw.groupCount).toBe(1);
    expect(draw.groups).toEqual({ approval: 1, question: 1 });
    const where = placement(draw);
    expect(where.get("a-1")).toBe(where.get("q-1"));
  });

  test("a case naming no issue is its own group", () => {
    const cases = [
      drawCase({ ...caseRow("a-1", "approval", "ISSUE-7"), issueKey: null }),
      drawCase({ ...caseRow("a-2", "approval", "ISSUE-7"), issueKey: null }),
    ];
    const draw = drawSlices(cases, 3);
    expect(draw.groupCount).toBe(2);
  });
});

/**
 * A draw under a registered read of the reserve: report, the final test, takes only the trees
 * item 6 leaves eligible (`trees.ts`). The outcomes are handed to the draw, so these cases need no
 * log: here each issue is its own tree, the reserve holds every case whose id starts `r-`, a tree
 * is eligible when it holds such cases alone, and the issues in `beforeCut` had an ask before the
 * cut.
 */
describe("drawSlices under a read of the reserve", () => {
  const holds = (row: CaseRow): boolean => row.caseId.startsWith("r-");

  /**
   * The source over `cases`: the issues in `beforeCut` had an ask before the cut, and the cases in
   * `leftOut` are the ones the command left out before rendering, as it does for such a tree.
   */
  function source(
    cases: readonly DrawCase[],
    beforeCut: readonly string[] = [],
    leftOut: readonly DrawCase[] = []
  ): FinalTestSource {
    const outcomes = new Map<string, TreeOutcome>();
    for (const one of [...cases, ...leftOut]) {
      const key = `tree:${one.row.issueKey}`;
      if (beforeCut.includes(one.row.issueKey ?? "")) outcomes.set(key, "ask-before-cut");
      else if (!holds(one.row)) outcomes.set(key, "not-held-at-opening");
      else if (!outcomes.has(key)) outcomes.set(key, "eligible");
    }
    return {
      reference: "claim:test",
      holds,
      grouping: {
        groupOf: (row) => `tree:${row.issueKey}`,
        outcomes,
        groups: new Map(),
        joins: [],
      },
      beforeCut: leftOut.map((one) => one.row),
    };
  }

  /** `count` issues the reserve holds whole, three approval cases each. */
  function reserved(count: number): DrawCase[] {
    const cases: DrawCase[] = [];
    for (let group = 0; group < count; group += 1) {
      for (let at = 0; at < 3; at += 1) {
        cases.push(drawCase(caseRow(`r-${group}-${at}`, "approval", `RES-${group}`)));
      }
    }
    return cases;
  }

  test("report takes every eligible tree, before fit, and no other tree", () => {
    const held = reserved(10);
    const cases = [...corpus(), ...held];
    const draw = drawSlices(cases, 7, source(cases));
    expect([...draw.slices.report].sort()).toEqual(held.map((one) => one.row.caseId).sort());
    expect(draw.groupKey).toBe("tree");
    expect(draw.finalTest).toEqual({
      reference: "claim:test",
      groups: 10,
      outcomes: {
        "ask-before-cut": 0,
        "not-held-at-opening": 180,
        "read-by-scorer": 0,
        "asked-by-round": 0,
        eligible: 10,
      },
      beforeCut: { total: 0, cases: [] },
      heldOutsideReport: { total: 0, cases: [] },
      parentChanges: [],
      nearDuplicates: { threshold: 0.9, joins: [] },
      trees: held
        .filter((one) => one.row.caseId.endsWith("-0"))
        .map((one) => ({
          group: `tree:${one.row.issueKey}`,
          slice: "report" as const,
          cases: [0, 1, 2].map((at) => one.row.caseId.replace(/-0$/, `-${at}`)),
        }))
        .sort((one, other) => (one.group < other.group ? -1 : 1)),
    });
    expect(draw.counts.fit.approval).toBeGreaterThan(TARGETS.approval.fit - 3);
  });

  test("a tree that is not eligible never reaches report, and its reserved cases are named", () => {
    const cases = [...corpus(), ...reserved(10)];
    cases.push(drawCase(caseRow("x-0", "approval", "RES-0")));
    const draw = drawSlices(cases, 7, source(cases));
    const where = placement(draw);
    expect(where.get("x-0")).not.toBe("report");
    expect(where.get("r-0-0")).toBe(where.get("x-0"));
    expect(draw.slices.report).toHaveLength(27);
    expect(draw.finalTest?.groups).toBe(9);
    expect(draw.finalTest?.heldOutsideReport.cases.sort()).toEqual(["r-0-0", "r-0-1", "r-0-2"]);
  });

  test("a case the draw leaves out still keeps its tree out of report", () => {
    const cases = [...corpus(), ...reserved(10)];
    cases.push(drawCase(caseRow("x-1", "question", "RES-1", 1)));
    const draw = drawSlices(cases, 7, source(cases));
    const where = placement(draw);
    expect(where.has("x-1")).toBe(false);
    expect(where.get("r-1-0")).not.toBe("report");
    expect(draw.finalTest?.groups).toBe(9);
    expect(draw.finalTest?.heldOutsideReport.total).toBe(3);
  });

  test("a tree with an ask before the cut gives no case to any slice", () => {
    const held = reserved(10);
    // RES-2's cases were left out before rendering; ISSUE-a-0's reach the draw and are left out
    // there.
    const leftOut = held.filter((one) => one.row.issueKey === "RES-2");
    const cases = [...corpus(), ...held.filter((one) => one.row.issueKey !== "RES-2")];
    const draw = drawSlices(cases, 7, source(cases, ["RES-2", "ISSUE-a-0"], leftOut));
    const where = placement(draw);
    for (const caseId of ["r-2-0", "r-2-1", "r-2-2", "a-0-0", "a-0-1", "a-0-2"]) {
      expect(where.has(caseId)).toBe(false);
    }
    expect(draw.finalTest?.beforeCut.cases.sort()).toEqual(
      ["a-0-0", "a-0-1", "a-0-2", "r-2-0", "r-2-1", "r-2-2"].sort()
    );
    expect(draw.finalTest?.outcomes["ask-before-cut"]).toBe(2);
    expect(draw.finalTest?.groups).toBe(9);
    expect(draw.finalTest?.heldOutsideReport.total).toBe(0);
  });

  test("report takes every eligible tree whole, past its targets", () => {
    const cases = reserved(90);
    const draw = drawSlices(cases, 7, source(cases));
    // 270 approval cases against report's 250: the final test is every eligible tree.
    expect(draw.counts.report.approval).toBe(270);
    expect(draw.finalTest?.groups).toBe(90);
    expect(draw.finalTest?.heldOutsideReport.total).toBe(0);
    const trees = draw.finalTest?.trees ?? [];
    expect(trees).toHaveLength(90);
    expect(trees.every((tree) => tree.slice === "report")).toBe(true);
  });

  test("an eligible tree holding a question still reaches report when its question places are full", () => {
    // Sixty trees, each one approval request and one question: report's targets hold 50
    // questions, and the ten trees past them are the final test's all the same.
    const cases = Array.from({ length: 60 }, (_, group) => [
      drawCase(caseRow(`r-${group}-a`, "approval", `RES-${group}`)),
      drawCase(caseRow(`r-${group}-q`, "question", `RES-${group}`)),
    ]).flat();
    const draw = drawSlices(cases, 7, source(cases));
    expect(draw.counts.report).toEqual({ approval: 60, question: 60 });
    expect(draw.finalTest?.trees.filter((tree) => tree.slice === "report")).toHaveLength(60);
  });

  test("a claim draw takes a cut at or before the reserve's, and refuses a later one", () => {
    expect(claimCutRefusal(Date.parse("2026-11-16T00:00:00Z"))).toBeNull();
    expect(claimCutRefusal(Date.parse("2026-11-01T00:00:00Z"))).toBeNull();
    expect(claimCutRefusal(Date.parse("2026-11-18T00:00:00Z"))).toContain(
      "takes a --cut at or before the reserve's cut, 2026-11-16T00:00:00Z"
    );
  });

  test("the command that reads the reserve stops before the corpus on each claim guard", async () => {
    // Each guard runs before any corpus is read, so a corpus path that does not exist shows the
    // guard, not a missing file, answered.
    const base = ["--corpus", "/nowhere/corpus", "--out", "/nowhere/out"];
    const claim = [
      "--seed",
      "7",
      "--scorer-records",
      "/nowhere/a",
      "--round-sessions",
      "/nowhere/b",
    ];
    const runs: Array<[string[], string]> = [
      [[...base, "--cut", "2026-11-16T00:00:00Z"], "takes --seed, the seed its registration names"],
      [
        [...base, "--cut", "2026-11-16T00:00:00Z", "--seed", "7"],
        "takes --scorer-records and --round-sessions",
      ],
      [
        [...base, "--cut", "2026-11-18T00:00:00Z", ...claim],
        "takes a --cut at or before the reserve's cut, 2026-11-16T00:00:00Z",
      ],
      [
        [...base, "--cut", "2026-11-16T00:00:00Z", ...claim, "--backlog-roots", "/nowhere/c"],
        "applies no never-decide rule, so it takes no --backlog-roots and no --cache",
      ],
      [
        [...base, "--cut", "2026-11-16T00:00:00Z", ...claim, "--cache", "/nowhere/d"],
        "applies no never-decide rule, so it takes no --backlog-roots and no --cache",
      ],
    ];
    const before = process.env.APPROVAL_EVAL_RESERVE;
    process.env.APPROVAL_EVAL_RESERVE = "read:claim:test";
    const lines: string[] = [];
    const error = spyOn(console, "error").mockImplementation((line?: unknown) => {
      lines.push(String(line));
    });
    try {
      for (const [argv, words] of runs) {
        lines.length = 0;
        expect(await main(argv)).toBe(2);
        expect(lines.join("\n")).toContain(words);
      }
    } finally {
      error.mockRestore();
      if (before === undefined) delete process.env.APPROVAL_EVAL_RESERVE;
      else process.env.APPROVAL_EVAL_RESERVE = before;
    }
  });

  test("reads no never-decide answer: a case the rule would hold, or could not read, reaches report", () => {
    const held: NeverDecideAnswer = {
      ...DECIDED,
      held: true,
      kind: "destructive-act",
      reason: "The change list asks for the spool directory to be removed.",
      answer: "destructive-act\nThe change list asks for the spool directory to be removed.",
    };
    const unreadable: NeverDecideAnswer = { ...DECIDED, fault: "the answer named no kind" };
    const cases = [...corpus(), ...reserved(3)].map((one) =>
      one.row.issueKey === "RES-1"
        ? drawCase(one.row, held)
        : one.row.issueKey === "RES-2"
          ? drawCase(one.row, unreadable)
          : one
    );
    const draw = drawSlices(cases, 7, source(cases));
    expect(draw.held).toEqual([]);
    expect(draw.undrawableByKind["rule-unreadable"]).toBe(0);
    expect(draw.finalTest?.groups).toBe(3);
    expect(draw.finalTest?.trees).toEqual([
      { group: "tree:RES-0", slice: "report", cases: ["r-0-0", "r-0-1", "r-0-2"] },
      { group: "tree:RES-1", slice: "report", cases: ["r-1-0", "r-1-1", "r-1-2"] },
      { group: "tree:RES-2", slice: "report", cases: ["r-2-0", "r-2-1", "r-2-2"] },
    ]);
    // The draw the command makes under a read hands the cases no answer at all.
    const unruled = cases.map((one) => ({ ...one, hold: null }));
    expect(drawSlices(unruled, 7, source(unruled)).slices).toEqual(draw.slices);
  });

  test("an eligible tree whose every case no arm can be scored on is recorded in no slice", () => {
    const unscorable = [0, 1].map((at) => drawCase(caseRow(`r-1-${at}`, "question", "RES-1", 1)));
    const cases = [...corpus(), ...reserved(3).filter((one) => one.row.issueKey !== "RES-1")];
    cases.push(...unscorable);
    const draw = drawSlices(cases, 7, source(cases));
    expect(draw.finalTest?.groups).toBe(2);
    expect(draw.finalTest?.trees).toEqual([
      { group: "tree:RES-0", slice: "report", cases: ["r-0-0", "r-0-1", "r-0-2"] },
      { group: "tree:RES-1", slice: null, cases: ["r-1-0", "r-1-1"] },
      { group: "tree:RES-2", slice: "report", cases: ["r-2-0", "r-2-1", "r-2-2"] },
    ]);
  });

  test("with no eligible tree, report stays empty", () => {
    const cases = corpus();
    const draw = drawSlices(cases, 7, source(cases));
    expect(draw.slices.report).toEqual([]);
    expect(draw.finalTest?.groups).toBe(0);
  });

  test("the registered seed draws the same final test whatever order the cases come in", () => {
    const cases = [...corpus(), ...reserved(90)];
    const draw = drawSlices(cases, 12345, source(cases));
    expect(drawSlices([...cases].reverse(), 12345, source(cases)).slices).toEqual(draw.slices);
    expect(drawSlices(cases, 12346, source(cases)).slices.report).not.toEqual(draw.slices.report);
  });
});

/**
 * The roots a draw is given, over fixtures: the recorded corpus's own draw records 122 roots
 * and not one of them is malformed under any field, so what a reader does with a malformed one
 * is shown here and nowhere else.
 *
 * Both readers of the shape are driven over the same file: `draw.json`, which `draw-io.ts`
 * owns, and the file `--backlog-roots` names, which this command reads itself so that its own
 * refusals stay usage errors about the flag. One policy, refusal with the offending label
 * quoted, and a wording of its own in each of the two. The third wording is the live issue
 * list's `DrawError`, which reads Dispatch rather than a file and is driven nowhere here. What
 * those labels decide is the never-decide rule's hold over a parked `legion` root, which is
 * driven here over the roots each reader hands back.
 */
describe("the roots a draw is given", () => {
  /** A root of a parked Legion tree, as a draw records one. */
  const parked = {
    key: "INFRA-7",
    parent: null,
    labels: ["legion"],
    status: "backlog",
    primaryArtifactId: "document-INFRA-7",
  };

  /** The directories this file made, each removed by its own exact path. */
  const made: string[] = [];

  afterEach(async () => {
    for (const directory of made.splice(0)) await rm(directory, { recursive: true, force: true });
  });

  /** One `draw.json` carrying the roots named, which is the file both readers are given. */
  async function written(...issues: unknown[]): Promise<string> {
    const directory = await mkdtemp(join(tmpdir(), "draw-roots-"));
    made.push(directory);
    const path = drawPath(directory);
    const roots = { readAt: "2026-11-06T09:00:00Z", source: "dispatch", issues };
    await writeFile(path, `${JSON.stringify({ backlogLegionRoots: roots })}\n`, "utf8");
    return path;
  }

  test("refuses a label that is no string, as a usage error naming the flag", async () => {
    const path = await written({ ...parked, labels: ["legion", 7] });
    let refused: unknown = null;
    try {
      await pinnedBacklogRoots(path);
    } catch (error) {
      refused = error;
    }
    expect(refused).toBeInstanceOf(UsageError);
    const said = String((refused as Error).message);
    expect(said).toContain(
      `--backlog-roots ${path} root INFRA-7 names a label that is no string: 7`
    );
    expect(said).toContain("it takes the shape draw.json writes");
  });

  test("refuses that same label through the draw, as the file being no draw", async () => {
    const path = await written({ ...parked, labels: ["legion", 7] });
    await expect(readDraw(path)).rejects.toThrow(CorpusError);
    await expect(readDraw(path)).rejects.toThrow(
      `${path} root INFRA-7 names a label that is no string: 7`
    );
  });

  test("takes a well-formed root, keeping the reading time the file carries", async () => {
    const path = await written(parked);
    const read = await pinnedBacklogRoots(path);
    expect(read.issues).toEqual([parked]);
    expect(read.readAt).toBe("2026-11-06T09:00:00Z");
    expect(read.source).toBe(path);
    expect((await readDraw(path))?.roots).toEqual([parked]);
  });

  test("hands the rule a parked root it holds, read either way", async () => {
    const path = await written(parked);
    const row = caseRow("case-1", "approval", "INFRA-7");
    const text = "# An approval request\n";
    // The rule holds this root on the issue's facts alone, so a call is a test that failed.
    const never: AskModel = () => Promise.reject(new Error("the rule was put to the model"));
    const read = [
      rootFactsOf((await readDraw(path))?.roots),
      rootFactsOf((await pinnedBacklogRoots(path)).issues),
    ];
    for (const facts of read) {
      const answer = await neverDecide(
        { text, row, issue: facts.get("INFRA-7") ?? null },
        { ask: never, definition: "the rule" }
      );
      expect(answer.held).toBe(true);
      expect(answer.kind).toBe("waiting-legion-root");
      expect(answer.source).toBe("issue-facts");
    }
    // What the labels decide: with the `legion` label gone the facts hold nothing, and the rule
    // goes to the model. A root the issue list passed over for a key or a status it could not
    // read ended there too, since the rule is handed no facts for a root no reader kept: the
    // hold silently not made and nobody told, which is why every reader refuses instead.
    await expect(
      neverDecide(
        {
          text,
          row,
          issue: {
            parent: null,
            labels: [],
            status: "backlog",
            primaryArtifactId: "document-INFRA-7",
          },
        },
        { ask: never, definition: "the rule" }
      )
    ).rejects.toThrow("the rule was put to the model");
  });
});

/**
 * What `rule.renderingCheck` is built from: the sentences the draw's own renderings said of
 * themselves, which `renderChecked` writes, under the bound an unguarded draw holds its
 * reading to.
 */
describe("what a draw records of its renderings", () => {
  test("joins the renderings' own sentences, and puts this draw's bound after them", () => {
    const one = "The text was handed to the rule as it was rendered.";
    const other = "The text was rendered over no document of its request.";
    const said = `${one} ${other}`;
    const record = renderingCheckOf([one, other], "none");
    expect(record.startsWith(`${said} `)).toBe(true);
    expect(record).toContain(
      "needs the text held to `assertNoLeak` in full, both of its rules, first."
    );
    // A draw that held its renderings to that guard has met the bound already, so its
    // sentences stand without it.
    expect(renderingCheckOf([one, other], "assert-no-leak")).toBe(said);
    // A draw that rendered nothing has no such sentence, and says that rather than leaving the
    // bound standing over nothing.
    expect(renderingCheckOf([], "none")).toBe(
      "No case was rendered in this draw, so no text was checked."
    );
  });
});
