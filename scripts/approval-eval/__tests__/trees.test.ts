import { afterEach, describe, expect, test } from "bun:test";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { CaseRow } from "../build-corpus";
import { CorpusError } from "../corpus-io";
import type { DispatchEvent } from "../dispatch-read";
import {
  changesAfterAskOf,
  idsInFiles,
  jaccardOf,
  nearDuplicatesOf,
  readTrees,
  shinglesOf,
  TREES_FILE,
  type TreeChecks,
  type TreeGroup,
  TreeLog,
  type TreeOutcome,
  treeGroupingOf,
  treeOutcomeOf,
} from "../trees";

/**
 * The trees the claim's draw groups by, and item 6 of the reserve's version 2 over them
 * (dispatch://INFRA/artifact/infra-2897-final-test-reserve-md). Nothing here reads Dispatch.
 */

/** A root key the registered rule picks (its digit is 2) and one it does not (its digit is c). */
const PICKED = "INFRA-3189";
const NOT_PICKED = "INFRA-2523";

/** Two asks' ids, as Dispatch writes them, and a session of the claim's round. */
const ASK_ONE = "0b1d5a9e-3f4c-4d2a-9b8e-1c2d3e4f5a6b";
const ASK_TWO = "7c8d9e0f-1a2b-4c3d-8e9f-0a1b2c3d4e5f";
const ASK_THREE = "2e3f4a5b-6c7d-4e8f-9a0b-1c2d3e4f5a6b";
const ROUND_SESSION = "01a1e000-0000-7000-8000-000000000001";

/** One event of the log; it is made at second `id` of the cut's minute unless `at` is given. */
function event(
  id: number,
  type: string,
  payload: Record<string, unknown>,
  actor: string | null = null,
  at = `2026-11-16T00:00:0${id}Z`
): DispatchEvent {
  return {
    id,
    issue_key: null,
    project: "INFRA",
    seq: id,
    type,
    actor: actor === null ? null : { kind: "session", id: actor },
    created_at: at,
    payload,
  };
}

function logOf(events: readonly DispatchEvent[]): TreeLog {
  const log = new TreeLog();
  for (const one of events) log.note(one);
  return log;
}

/** A case of `askId` on `issueKey`, held at its opening under `root`, answered after the cut. */
function caseRow(askId: string, issueKey: string, root: string | null): CaseRow {
  return {
    caseId: `${askId}@2026-11-16T02:00:00Z`,
    eventId: 1,
    askId,
    issueKey,
    project: "INFRA",
    kind: "approval",
    urgency: null,
    multiple: false,
    question: "Approve spec.md (version 2)?",
    options: [],
    selected: ["Approve"],
    words: null,
    hasWords: false,
    askCreatedAt: "2026-11-16T01:00:00Z",
    askTreeRoot: root,
    issueTreeRoot: root,
    answeredAt: "2026-11-16T02:00:00Z",
    eventCreatedAt: "2026-11-16T02:00:00Z",
    approval: null,
    verdict: "approve",
    confounds: {
      version: null,
      answersThatDayBefore: 0,
      hourUtc: 2,
      hourPacific: 18,
      changesAlreadyRequested: false,
      priorCasesOnDocument: 0,
      caseKind: "approval",
    },
  };
}

const NONE: TreeChecks = {
  holds: () => true,
  readAskIds: new Set(),
  roundSessions: new Set(),
};

/** One tree under `PICKED` with one ask, after the cut, by a session outside the round. */
function treeOf(asks: TreeGroup["asks"]): TreeGroup {
  return { key: PICKED, issues: [PICKED], asks, parentChanges: [] };
}

const AFTER = {
  askId: ASK_ONE,
  issueKey: PICKED,
  eventId: 10,
  openedAt: "2026-11-16T01:00:00Z",
  actors: ["s-0"],
};

describe("TreeLog", () => {
  test("keeps an issue in its group after it moves to another tree", () => {
    const log = logOf([
      event(1, "issue.created", { key: "INFRA-1", parent: null }),
      event(2, "issue.created", { key: "INFRA-2", parent: "INFRA-1" }),
      event(3, "issue.created", { key: "INFRA-3", parent: null }),
      event(4, "issue.updated", { key: "INFRA-2", parent: "INFRA-3" }, "s-mover"),
      event(5, "issue.created", { key: "INFRA-9", parent: null }),
    ]);
    const groups = log.groupsOf(["INFRA-1"]);
    expect(groups.map((group) => [group.key, group.issues])).toEqual([
      ["INFRA-1", ["INFRA-1", "INFRA-2", "INFRA-3"]],
    ]);
    expect(groups[0]?.parentChanges).toEqual([
      {
        eventId: 4,
        issueKey: "INFRA-2",
        from: "INFRA-1",
        to: "INFRA-3",
        at: "2026-11-16T00:00:04Z",
        actor: "s-mover",
      },
    ]);
  });

  test("names an issue that appears only as another issue's parent", () => {
    const log = logOf([event(1, "issue.created", { key: "INFRA-5", parent: "INFRA-4" })]);
    expect(log.groupsOf(["INFRA-5"]).map((group) => [group.key, group.issues])).toEqual([
      ["INFRA-4", ["INFRA-4", "INFRA-5"]],
    ]);
  });

  test("records each ask's first opening, and every actor that opened it", () => {
    const log = logOf([
      event(1, "issue.created", { key: "INFRA-1", parent: null }),
      event(
        2,
        "ask.opened",
        { id: ASK_ONE, issue_key: "INFRA-1", created_at: "2026-11-15T23:00:00Z" },
        "s-1"
      ),
      event(3, "ask.opened", { id: ASK_ONE, issue_key: "INFRA-1" }, "s-2"),
      event(4, "ask.opened", { id: ASK_TWO }, "s-3"),
    ]);
    expect(log.groupsOf(["INFRA-1"])[0]?.asks).toEqual([
      {
        askId: ASK_ONE,
        issueKey: "INFRA-1",
        eventId: 2,
        openedAt: "2026-11-15T23:00:00Z",
        actors: ["s-1", "s-2"],
      },
    ]);
  });

  test("lists the parent changes the log recorded after the tree's first ask opened", () => {
    // The opening and the late change share a time: the log's order, not the clock, decides.
    const log = logOf([
      event(1, "issue.created", { key: "INFRA-1", parent: null }),
      event(2, "issue.created", { key: "INFRA-2", parent: null }),
      event(3, "issue.updated", { key: "INFRA-2", parent: "INFRA-1" }, "s-early"),
      event(4, "ask.opened", { id: ASK_ONE, issue_key: "INFRA-1" }, "s-1", "2026-11-16T00:00:04Z"),
      event(5, "issue.updated", { key: "INFRA-2", parent: null }, "s-late", "2026-11-16T00:00:04Z"),
    ]);
    const [group] = log.groupsOf(["INFRA-2"]);
    expect(
      group === undefined ? [] : changesAfterAskOf(group).map((change) => change.actor)
    ).toEqual(["s-late"]);
  });

  test("version 3's list is every issue of each tree with an ask, less the trees version 2 held whole", () => {
    const opened = (id: number, askId: string, key: string, at: string) =>
      event(id, "ask.opened", { id: askId, issue_key: key, created_at: at });
    const log = logOf([
      // An ask before version 2's cut puts its whole tree in the list.
      event(1, "issue.created", { key: "INFRA-1", parent: null }),
      event(2, "issue.created", { key: "INFRA-2", parent: "INFRA-1" }),
      opened(3, ASK_ONE, "INFRA-2", "2026-11-15T23:00:00Z"),
      // Every ask after the cut, on a root version 2 picks: held whole, so not in the list.
      event(4, "issue.created", { key: PICKED, parent: null }),
      opened(5, ASK_TWO, PICKED, "2026-11-16T01:00:00Z"),
      // After the cut, on a root version 2 does not pick: a scorer may read it, so in the list.
      event(6, "issue.created", { key: NOT_PICKED, parent: null }),
      opened(7, ASK_THREE, NOT_PICKED, "2026-11-16T01:00:00Z"),
      // A tree with no ask is in no list.
      event(8, "issue.created", { key: "INFRA-9", parent: null }),
    ]);
    const roots = new Map([
      [ASK_ONE, "INFRA-1"],
      [ASK_TWO, PICKED],
      [ASK_THREE, NOT_PICKED],
    ]);
    expect(log.reserveListOf(roots)).toEqual(["INFRA-1", "INFRA-2", NOT_PICKED]);
    // An ask that opened under a root version 2 does not pick, before its issue moved into the
    // picked tree, puts that tree in the list too.
    expect(log.reserveListOf(new Map([...roots, [ASK_TWO, NOT_PICKED]]))).toEqual([
      "INFRA-1",
      "INFRA-2",
      NOT_PICKED,
      PICKED,
    ]);
  });
});

describe("treeOutcomeOf, item 6", () => {
  test("(a) an ask of anyone before the cut takes the tree out of every slice", () => {
    const before = { ...AFTER, askId: ASK_TWO, openedAt: "2026-11-15T23:59:59Z" };
    expect(treeOutcomeOf(treeOf([before, AFTER]), [caseRow(ASK_ONE, PICKED, PICKED)], NONE)).toBe(
      "ask-before-cut"
    );
  });

  test("(b) a case whose root at its ask's opening the rule does not pick sends the tree to development", () => {
    // The issue joined the picked tree after its ask opened: the reserve holds it at the build.
    expect(treeOutcomeOf(treeOf([AFTER]), [caseRow(ASK_ONE, PICKED, NOT_PICKED)], NONE)).toBe(
      "not-held-at-opening"
    );
    const notHeld = { ...NONE, holds: () => false };
    expect(treeOutcomeOf(treeOf([AFTER]), [caseRow(ASK_ONE, PICKED, PICKED)], notHeld)).toBe(
      "not-held-at-opening"
    );
  });

  test("(b) from version 3's cut, a case needs a root at its opening that version 3's list leaves out", () => {
    const late = { ...AFTER, openedAt: "2026-11-18T01:00:00Z" };
    const lateCase = (askRootListed: boolean | null): CaseRow => ({
      ...caseRow(ASK_ONE, NOT_PICKED, NOT_PICKED),
      askCreatedAt: "2026-11-18T01:00:00Z",
      answeredAt: "2026-11-18T02:00:00Z",
      askRootListed,
    });
    // Version 2's hash does not pick this root; from version 3's cut only the list decides.
    expect(treeOutcomeOf(treeOf([late]), [lateCase(false)], NONE)).toBe("eligible");
    expect(treeOutcomeOf(treeOf([late]), [lateCase(true)], NONE)).toBe("not-held-at-opening");
    expect(treeOutcomeOf(treeOf([late]), [lateCase(null)], NONE)).toBe("not-held-at-opening");
  });

  test("(c) an ask a scorer's record names sends the tree to development", () => {
    const checks = { ...NONE, readAskIds: new Set([ASK_TWO]) };
    const other = { ...AFTER, askId: ASK_TWO };
    expect(treeOutcomeOf(treeOf([AFTER, other]), [caseRow(ASK_ONE, PICKED, PICKED)], checks)).toBe(
      "read-by-scorer"
    );
  });

  test("(d) an ask a session of the claim's round opened, even only reopened, sends the tree to development", () => {
    const log = logOf([
      event(1, "issue.created", { key: PICKED, parent: null }),
      event(2, "ask.opened", { id: ASK_ONE, issue_key: PICKED }, "s-0"),
      event(3, "ask.opened", { id: ASK_ONE, issue_key: PICKED }, ROUND_SESSION.toUpperCase()),
    ]);
    const [tree] = log.groupsOf([PICKED]);
    const checks = { ...NONE, roundSessions: new Set([ROUND_SESSION]) };
    expect(
      tree === undefined ? null : treeOutcomeOf(tree, [caseRow(ASK_ONE, PICKED, PICKED)], checks)
    ).toBe("asked-by-round");
  });

  test("a tree that passes every check is eligible", () => {
    expect(treeOutcomeOf(treeOf([AFTER]), [caseRow(ASK_ONE, PICKED, PICKED)], NONE)).toBe(
      "eligible"
    );
  });
});

describe("the files the claim reads", () => {
  const made: string[] = [];
  afterEach(async () => {
    for (const directory of made.splice(0)) await rm(directory, { recursive: true, force: true });
  });

  async function directory(): Promise<string> {
    const one = await mkdtemp(join(tmpdir(), "approval-eval-trees-"));
    made.push(one);
    return one;
  }

  test("a scorer's record counts every id it names, in any case and any shape", async () => {
    const at = await directory();
    const path = join(at, "rows.jsonl");
    await writeFile(
      path,
      `{"request":"dispatch://INFRA-1/ask/${ASK_ONE.toUpperCase()}","neighbour_asks":["${ASK_TWO}"]}\n`
    );
    expect(await idsInFiles([path])).toEqual(new Set([ASK_ONE, ASK_TWO]));
    await expect(idsInFiles([join(at, "missing.jsonl")])).rejects.toThrow(CorpusError);
  });

  test("a record that names no id stops the claim, beside a record that names one", async () => {
    const at = await directory();
    const named = join(at, "rows.jsonl");
    await writeFile(named, `{"request":"dispatch://INFRA-1/ask/${ASK_ONE}"}\n`);
    for (const [name, text] of [
      ["empty.jsonl", ""],
      ["truncated.jsonl", '{"request":"dispatch://INFRA-1/ask/0b1d5a9e-3f4c"}\n'],
      ["sessions.txt", "the sessions of the round\n"],
    ] as const) {
      const empty = join(at, name);
      await writeFile(empty, text);
      await expect(idsInFiles([empty])).rejects.toThrow(`${empty} names no id`);
      await expect(idsInFiles([named, empty])).rejects.toThrow(CorpusError);
    }
  });

  test("a corpus built before the build wrote its trees cannot be drawn by the claim", async () => {
    const at = await directory();
    await expect(readTrees(at)).rejects.toThrow(CorpusError);
    await writeFile(
      join(at, TREES_FILE),
      JSON.stringify({ version: 0, throughEventId: 1, groups: [] })
    );
    await expect(readTrees(at)).rejects.toThrow(CorpusError);
  });

  test("a case on an issue the trees do not name stops the claim", () => {
    const trees = { version: 1, throughEventId: 9, groups: [treeOf([AFTER])] };
    const row = caseRow(ASK_ONE, "INFRA-77", PICKED);
    expect(() => treeGroupingOf([row], trees, NONE, evidenceOf([row]))).toThrow(CorpusError);
    const named = caseRow(ASK_ONE, PICKED, PICKED);
    const grouping = treeGroupingOf([named], trees, NONE, evidenceOf([named]));
    expect(grouping.outcomes.get(`tree:${PICKED}`)).toBe("eligible");
  });
});

/** Each row's shingles, over `texts[i]` where given and the row's own question otherwise. */
function evidenceOf(
  rows: readonly CaseRow[],
  texts: readonly string[] = []
): Map<string, number[]> {
  return new Map(rows.map((row, at) => [row.caseId, shinglesOf(texts[at] ?? row.question ?? "")]));
}

/** `count` distinct words joined by spaces, so a text of n words has n − 4 distinct 5-grams. */
function wordsOf(count: number, from = 0, last: string | null = null): string {
  const words = Array.from({ length: count }, (_, at) => `w${from + at}`);
  if (last !== null) words[count - 1] = last;
  return words.join(" ");
}

describe('near-duplicate grouping (dispatch://INFRA-3189 v8, "Grouping")', () => {
  /** A second tree, under a root the rule does not pick, whose one ask opened before the cut. */
  const EARLIER: TreeGroup = {
    key: NOT_PICKED,
    issues: [NOT_PICKED],
    asks: [
      {
        ...AFTER,
        askId: ASK_TWO,
        issueKey: NOT_PICKED,
        eventId: 5,
        openedAt: "2026-11-15T12:00:00Z",
      },
    ],
    parentChanges: [],
  };
  const trees = { version: 1, throughEventId: 9, groups: [treeOf([AFTER]), EARLIER] };
  const rows = [caseRow(ASK_ONE, PICKED, PICKED), caseRow(ASK_TWO, NOT_PICKED, NOT_PICKED)];
  const [own, earlier] = rows as [CaseRow, CaseRow];

  test("shingles are word 5-grams that neither case nor punctuation changes", () => {
    expect(shinglesOf("Approve spec.md, version 2? Yes: approve it.")).toEqual(
      shinglesOf("approve SPEC md version 2 yes approve it")
    );
    expect(shinglesOf(wordsOf(12))).toHaveLength(8);
    expect(shinglesOf("Approve it?")).toHaveLength(1);
    expect(shinglesOf("Approve it?")).toEqual(shinglesOf("approve, it"));
    expect(shinglesOf("— ?")).toEqual([]);
  });

  test("two cases join at a Jaccard similarity of 0.9 and not below it", () => {
    // 9 shingles inside 10, then 9 and 9 sharing 8.
    expect(jaccardOf(shinglesOf(wordsOf(13)), shinglesOf(wordsOf(14)))).toBe(0.9);
    expect(jaccardOf(shinglesOf(wordsOf(13)), shinglesOf(wordsOf(13, 0, "x")))).toBe(0.8);
    const at = treeGroupingOf(rows, trees, NONE, evidenceOf(rows, [wordsOf(13), wordsOf(14)]));
    expect(at.joins).toHaveLength(1);
    const below = treeGroupingOf(
      rows,
      trees,
      NONE,
      evidenceOf(rows, [wordsOf(13), wordsOf(13, 0, "x")])
    );
    expect(below.joins).toEqual([]);
  });

  test("the size bound drops no pair at its own threshold, and `apart` drops what the caller skips", () => {
    // 7 inside 25 is 0.28 exactly, where 0.28 * 25 rounds above 7.
    const sets = new Map([
      ["small", [1, 2, 3, 4, 5, 6, 7]],
      ["large", Array.from({ length: 25 }, (_, at) => at + 1)],
    ]);
    expect(nearDuplicatesOf(sets, undefined, 0.28).map((pair) => pair.cases)).toEqual([
      ["large", "small"],
    ]);
    expect(nearDuplicatesOf(sets, () => false, 0.28)).toEqual([]);
  });

  test("a near-duplicate of a case in a tree with an ask before the cut takes its own tree out of every slice", () => {
    const joined = treeGroupingOf(
      rows,
      trees,
      NONE,
      evidenceOf(rows, [wordsOf(100), wordsOf(100, 0, "x")])
    );
    expect(joined.groupOf(own)).toBe(joined.groupOf(earlier));
    expect(joined.outcomes.get(joined.groupOf(own))).toBe("ask-before-cut");
    expect(joined.groups.get(joined.groupOf(own))?.asks.map((ask) => ask.askId)).toEqual([
      ASK_TWO,
      ASK_ONE,
    ]);
    // Each join names its cases, the lesser id first, and the group each was in before it.
    expect(joined.joins.map((join) => [join.cases, join.groups])).toEqual([
      [
        [own.caseId, earlier.caseId],
        [`tree:${PICKED}`, `tree:${NOT_PICKED}`],
      ],
    ]);
    expect(joined.joins[0]?.jaccard).toBeGreaterThanOrEqual(0.9);
    const apart = treeGroupingOf(
      rows,
      trees,
      NONE,
      evidenceOf(rows, [wordsOf(100), wordsOf(100, 1000)])
    );
    expect(apart.joins).toEqual([]);
    expect(apart.outcomes.get(apart.groupOf(own))).toBe("eligible");
    expect(apart.outcomes.get(apart.groupOf(earlier))).toBe("ask-before-cut");
  });

  test("a case on no issue is under item 6 (a) by its own ask's opening", () => {
    const before: CaseRow = {
      ...caseRow(ASK_TWO, PICKED, null),
      issueKey: null,
      askCreatedAt: "2026-11-15T23:59:59Z",
    };
    const cases: Array<[CaseRow, TreeOutcome]> = [
      [before, "ask-before-cut"],
      [{ ...before, askCreatedAt: null }, "ask-before-cut"],
      [{ ...before, askCreatedAt: "2026-11-16T00:00:00Z" }, "not-held-at-opening"],
    ];
    for (const [row, outcome] of cases) {
      const grouping = treeGroupingOf([row], trees, NONE, evidenceOf([row]));
      expect(grouping.outcomes.get(`ask:${ASK_TWO}`)).toBe(outcome);
    }
  });

  test("a case with no shingles stops the claim", () => {
    expect(() => treeGroupingOf([own], trees, NONE, new Map())).toThrow(CorpusError);
  });
});
