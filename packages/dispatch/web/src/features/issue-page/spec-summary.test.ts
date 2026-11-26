import { expect, test } from "bun:test";

import type { TreeNode } from "./issue-tree";
import type { NodeType } from "./page-words";
import { specSummary } from "./spec-summary";

/**
 * The issue's summary, the first callout of its spec: read from the outline's tree, and from the
 * Spec view's own tree where the outline's holds none, as on a Spec view loaded cold below
 * 1024 px, which sends no outline read.
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

/** The Spec view's own tree: the issue, its memo and a statement, as its answer builds it. */
const SPEC_VIEW = node("CORE-1", "issue", [
  node("spec#b-memo", "callout"),
  node("spec#b-r1", "requirement", [], { statement: true }),
]);

test("the summary is the first callout of the spec in the outline's tree", () => {
  const root = node("CORE-1", "issue", [
    node("notes#b-aside", "callout"),
    node("spec#b-summary", "callout"),
    node("spec#b-later", "callout"),
  ]);
  expect(specSummary(root, "spec")?.id).toBe("spec#b-summary");
});

test("where the outline's tree holds no memo, the summary is the Spec view's own tree's", () => {
  const root = node("CORE-1", "issue", [], { views: { trees: { spec: SPEC_VIEW } } });
  expect(specSummary(root, "spec")?.id).toBe("spec#b-memo");
});

test("the outline's own memo is the summary where the Spec view's tree holds one too", () => {
  const root = node("CORE-1", "issue", [node("spec#b-summary", "callout")], {
    views: { trees: { spec: SPEC_VIEW } },
  });
  expect(specSummary(root, "spec")?.id).toBe("spec#b-summary");
});

test("an issue with no spec has no summary, whatever its trees hold", () => {
  const root = node("CORE-1", "issue", [node("spec#b-summary", "callout")], {
    views: { trees: { spec: SPEC_VIEW } },
  });
  expect(specSummary(root, "")).toBeUndefined();
});
