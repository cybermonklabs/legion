import { describe, expect, test } from "bun:test";
import type { CaseKind, CaseRow } from "../build-corpus";
import { type Draw, type DrawCase, DrawError, drawSlices, TARGETS } from "../draw";
import { SLICES } from "../draw-io";
import type { NeverDecideAnswer } from "../never-decide";

/**
 * The draw over a hand-written corpus: nothing here reads a corpus from disk, renders a document
 * or reads Dispatch. What it holds the draw to is the plan's own rules - a group goes into one
 * slice whole, a seed reproduces a draw, and a held or undrawable case is left out of every
 * slice.
 */

const DECIDED: NeverDecideAnswer = {
  held: false,
  kind: null,
  reason: "the document asks for no act the agent never decides",
  answer: "none\nthe document asks for no act the agent never decides",
  source: "model",
  fault: null,
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
