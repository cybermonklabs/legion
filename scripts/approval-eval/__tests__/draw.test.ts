import { afterEach, describe, expect, test } from "bun:test";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { CaseKind, CaseRow } from "../build-corpus";
import { CorpusError, UsageError } from "../corpus-io";
import {
  type Draw,
  type DrawCase,
  DrawError,
  drawSlices,
  pinnedBacklogRoots,
  renderingCheckOf,
  TARGETS,
} from "../draw";
import { drawPath, readDraw, rootFactsOf, SLICES } from "../draw-io";
import { type AskModel, type NeverDecideAnswer, neverDecide } from "../never-decide";

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
 * themselves, which `renderChecked` writes, under the bound this draw holds its reading to.
 */
describe("what a draw records of its renderings", () => {
  test("writes the renderings' own sentence, and this draw's bound after it", () => {
    const check = "The text was handed to the rule as it was rendered.";
    const record = renderingCheckOf([check]);
    expect(record.startsWith(`${check} `)).toBe(true);
    const bound = record.slice(check.length + 1);
    expect(bound.length).toBeGreaterThan(0);
    // A draw that rendered nothing has no such sentence, and leaves the bound over nothing.
    expect(renderingCheckOf([])).not.toContain(bound);
  });
});
