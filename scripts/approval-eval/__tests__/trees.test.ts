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
  readTrees,
  TREES_FILE,
  type TreeChecks,
  type TreeGroup,
  TreeLog,
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
    expect(() => treeGroupingOf([caseRow(ASK_ONE, "INFRA-77", PICKED)], trees, NONE)).toThrow(
      CorpusError
    );
    const grouping = treeGroupingOf([caseRow(ASK_ONE, PICKED, PICKED)], trees, NONE);
    expect(grouping.outcomes.get(`tree:${PICKED}`)).toBe("eligible");
  });
});
