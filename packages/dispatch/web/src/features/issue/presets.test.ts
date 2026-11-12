import { expect, test } from "bun:test";

import type { TreeNode } from "./issue-tree";
import type { NodeType, Preset } from "./page-words";
import { openedWithMatches, presetView } from "./presets";
import { type DrawnRow, NOTHING_OPENED, treeRows } from "./tree-fold";

/**
 * A preset as a filter over the one tree (dispatch://INFRA-1196 R1, R3): what it keeps, and what
 * the page draws of what it kept. A filter exists to show what it matched, so its matches are
 * drawn while it is on - the fold is the unfiltered page's rule and may not hide them - and the
 * reader's own hand still outranks that.
 */

function node(
  id: string,
  type: NodeType,
  children: readonly TreeNode[] = [],
  extra: Partial<TreeNode> = {}
): TreeNode {
  return {
    alsoUnder: [],
    children,
    chips: [],
    facts: [],
    gaps: [],
    held: children.reduce((total, child) => total + child.held + 1, 0),
    id,
    question: false,
    replaced: false,
    text: id,
    transcluded: false,
    type,
    waiting: false,
    ...extra,
  };
}

/** The ids of every row drawn, in reading order; a branch's own row reads as `more:<id>`. */
function idsOf(rows: readonly DrawnRow[]): string[] {
  return rows.map((row) => (row.kind === "deeper" ? `more:${row.of.id}` : row.node.id));
}

/**
 * An issue whose plan sits under a document holding nothing open: no plan task is ever
 * unanswered (`tree-fold.ts`'s `unanswered`), so nothing of the frontier's runs through that
 * document and the unfiltered page draws it folded. Beside it is the one open question, which
 * is what the frontier does reach.
 */
function issue(): TreeNode {
  return node("CORE-1", "issue", [
    node("plan-md", "document", [
      node("plan-md#b-t1", "plan_task"),
      node("plan-md#b-t2", "plan_task"),
    ]),
    node("spec", "document", [node("spec#b-q1", "ask", [], { question: true, state: "open" })]),
  ]);
}

/** The rows the page draws for `preset` under `query`, with `opened` as the reader left it. */
function drawn(root: TreeNode, preset: Preset, query = "", opened = NOTHING_OPENED): string[] {
  const view = presetView(root, preset, query);
  return idsOf(treeRows(view.root, openedWithMatches(opened, view.opened)));
}

test("a filter draws its matches, and a branch the reader closed stays closed", () => {
  const root = issue();
  // The Plan preset keeps the two tasks and the document above them as context. Behind the
  // frontier's own rule that document arrives folded, so the page would draw the context row
  // and neither match.
  const view = presetView(root, "plan", "");
  expect(view.matches).toBe(2);
  expect([...view.context]).toEqual(["plan-md", "CORE-1"]);
  expect(idsOf(treeRows(view.root))).toEqual(["CORE-1", "plan-md"]);

  // The filter opens the path down to each match, so both are drawn.
  expect(drawn(root, "plan")).toEqual(["CORE-1", "plan-md", "plan-md#b-t1", "plan-md#b-t2"]);

  // The reader's own hand outranks it: the branch they folded shut stays shut, matches and all.
  const shut = {
    branches: new Set<string>(),
    closed: new Set(["plan-md"]),
    deeper: new Set<string>(),
  };
  expect(drawn(root, "plan", "", shut)).toEqual(["CORE-1", "plan-md"]);
});

test("the unfiltered view opens nothing of its own and folds to the frontier", () => {
  const root = issue();
  const view = presetView(root, "tree", "");
  expect([...view.opened]).toEqual([]);
  // Exactly the fold the page draws with no preset and no query: the path to the open question,
  // and the plan folded to one row with a count of what it holds.
  expect(drawn(root, "tree")).toEqual(idsOf(treeRows(root)));
  expect(drawn(root, "tree")).toEqual(["CORE-1", "plan-md", "spec", "spec#b-q1"]);
});

test("a query over the whole tree opens the path down to what it matched", () => {
  const root = issue();
  expect(drawn(root, "tree", "b-t2")).toEqual(["CORE-1", "plan-md", "plan-md#b-t2"]);
});

test("a match below the drawn levels is a row of its own rather than a count", () => {
  const deep = node("CORE-1", "issue", [
    node("plan-md", "document", [
      node("plan-md#b-s1", "callout", [
        node("plan-md#b-s2", "callout", [node("plan-md#b-t1", "plan_task")]),
      ]),
    ]),
  ]);
  expect(drawn(deep, "plan")).toEqual([
    "CORE-1",
    "plan-md",
    "plan-md#b-s1",
    "plan-md#b-s2",
    "plan-md#b-t1",
  ]);
});
