import { expect, test } from "bun:test";

import type { TreeNode } from "./issue-tree";
import type { NodeType } from "./page-words";
import { specSummary } from "./spec-summary";
import { treeAnswer } from "./tree-read/tree-fixtures";
import type { TreeAnswer } from "./tree-read/tree-read";

/**
 * The summary found in the Spec view's own tree is found once for each answer of the Spec view's
 * read: the outline's tree is made anew at each read's answer, with a new getter of the Spec
 * view's tree that builds it again where it is read (`useIssueTree.ts`' `trees`), and a summary
 * read again from it each time built the whole Spec view over again.
 */

function node(id: string, type: NodeType, children: readonly TreeNode[] = []): TreeNode {
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
  };
}

/** An outline's tree that holds no memo, whose Spec view's tree is built from `answer` when it
 *  is read, each build counted in `builds`. */
function outline(answer: TreeAnswer, builds: { count: number }): TreeNode {
  const trees = {};
  Object.defineProperty(trees, "spec", {
    enumerable: false,
    get: () => {
      builds.count += 1;
      return node("CORE-1", "issue", [node("spec#b-memo", "callout")]);
    },
  });
  return { ...node("CORE-1", "issue"), views: { spec: answer, trees } };
}

test("the Spec view's tree is built once for the summary of each answer of its read", () => {
  const answer = treeAnswer([]);
  const builds = { count: 0 };
  expect(specSummary(outline(answer, builds), "spec")?.id).toBe("spec#b-memo");
  expect(specSummary(outline(answer, builds), "spec")?.id).toBe("spec#b-memo");
  expect(builds.count, "the summary built the Spec view's tree again for the same answer").toBe(1);
  expect(specSummary(outline(treeAnswer([]), builds), "spec")?.id).toBe("spec#b-memo");
  expect(builds.count, "a new answer of the Spec view's read built no tree").toBe(2);
});
