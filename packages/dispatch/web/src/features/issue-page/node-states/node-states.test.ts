import { afterEach, beforeAll, expect, spyOn, test } from "bun:test";
import type { LensRead, NodeLenses } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createElement, type ReactNode, useMemo } from "react";
import { MemoryRouter, Route, Routes, useNavigate, useSearchParams } from "react-router-dom";

import { displayTree } from "../../../__tests__/display-trees";
import { renderWithQueries } from "../../../__tests__/query-provider";
import { api, type WriteRequest } from "../../../api/client";
import {
  artifactBlocksQuery,
  conversationTreeKey,
  issueCoverageQuery,
  issueResultsQuery,
} from "../../../api/queries";
import type {
  CheckBlock,
  IssueCheckResults,
  IssueDetails,
  IssueTreeParent,
  RequirementCoverage,
} from "../../../api/types";
import { pageStateText } from "../../../theme/classes";
import { MarginProvider } from "../../margin/margin-context";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import { type PublishedReferences, ReferenceNamesProvider } from "../../refs/reference-names";
import { KeymapProvider } from "../../shell/KeymapProvider";
import { useKeymapScope } from "../../shell/keymap";
import { contentsEntries } from "../contents-entries";
import { rowActionSelector } from "../DecisionActions";
import { DecisionsFilterBody } from "../decisions-filter/DecisionsFilterBody";
import {
  answeredAsk,
  artifact,
  decision,
  decisionNode,
  treeAnswer as decisionsAnswer,
  issueDetails as decisionsIssue,
  openAsk,
  personDecision,
  READER,
  SPEC_DOCUMENT,
  shaped,
  shapedTuples,
  issueTree as specIssueTree,
  specRef,
  statement as specStatement,
} from "../decisions-filter/decisions-fixtures";
import { FocusedNodeProvider } from "../focused-node";
import { ContentsList, contentsOf } from "../IssueContents";
import { IssuePresets } from "../IssuePresets";
import { IssueTree, type OutlineRequest } from "../IssueTree";
import { presetPath } from "../issue-address";
import { artifactsBySlug } from "../issue-artifacts";
import {
  buildIssueTree,
  heldBy,
  type NodeChip,
  type ParentLine,
  type TreeNode,
  targetNaming,
} from "../issue-tree";
import { LensChain } from "../lenses/LensChain";
import type { LensChip, LensFacts } from "../lenses/lens-drafts";
import { NodeLink } from "../lenses/NodeLink";
import { NoTooltips } from "../NoTooltips";
import { NO_FAILED_READS } from "../outline-reads";
import { OutlineRequestProvider } from "../outline-request";
import {
  BLOCK_ATTRIBUTE_LABELS,
  CONTROL_LABELS,
  EMPTY_STATE_LINES,
  GLYPH_TOKENS,
  NODE_STATE_WORDS,
  type NodeType,
  PLAN_WORDS,
  ROW_MARK_WORDS,
  VERIFICATION_WORDS,
} from "../page-words";
import { PlanFilterBody } from "../plan-filter/PlanFilterBody";
import { planOrder } from "../plan-filter/plan-order";
import { filterSignals, nodeReading, presetView } from "../presets";
import { blockReads, Publisher, publishedChips, publishedTree } from "../projection-fixture";
import { drawAtBrowserEntry, drawnSlot } from "../slots/stage-fixtures";
import { SpecFilterBody } from "../spec-filter/SpecFilterBody";
import {
  answerNode,
  askRead,
  coverageRead,
  issueDetails,
  decision as specDecision,
  decisionNode as specDecisionNode,
  shaped as specShaped,
  statementRef as specStatementRef,
  specViewAnswer,
  statement,
} from "../spec-filter/spec-fixtures";
import { outOfCounts, pathTo } from "../tree-fold";
import { nodesInOrder } from "../tree-keys";
import { type TreeNodeExtra, treeAnswer, treeNode } from "../tree-read/tree-fixtures";
import { statesOfRead } from "../tree-read/tree-nodes";
import { type TreeAnswer, type TreeAnswerNode, viewKey } from "../tree-read/tree-read";
import { queryTyped, VerificationFilterBody } from "../verification-filter/VerificationFilterBody";
import {
  check,
  checkNode,
  result,
  statementNode,
} from "../verification-filter/verification-fixtures";
import { NodeMarks, NodeStatesProvider } from "./NodeMarks";
import { marksAt, type NodeStates, stampStates, stateIdOf, statesByReference } from "./node-states";

/**
 * A node's life and who may read it, put on a tree built by hand: the states arrive by the read's
 * own id, which each node built here carries as its id (`TreeNode.readId`), and every place that
 * draws a node, and every count over the tree, reads them from the one tree. The marks as a row
 * draws them are read here too, and so are an overtaken node's row, the tree's keys on it and an
 * address to it, over `IssueTree` drawn under the page's keymap (`renderTree`);
 * the rows of a whole page are the browser's. The states the tree read's answers give, with no
 * view and with one, are read over trees `buildIssueTree` and `presetView` build.
 */

const KEY = "CORE-1";

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
    held: heldBy(children),
    id,
    question: false,
    readId: id,
    replaced: false,
    text: id,
    transcluded: false,
    type,
    waiting: false,
    ...extra,
  };
}

/** A chip naming `ref`, which the batched reference read answered. */
function chip(ref: string): NodeChip {
  return { answered: true, gone: false, reading: { words: ref }, ref };
}

/** `root` with `states` put on it by the read's own id, as the page puts the tree read's on it.
 *  A node built by another file's helper carries no read id, so each node is read as the node of
 *  the tree read whose id is its own where it names none. */
function stamped(root: TreeNode, states: Readonly<Record<string, NodeStates>>): TreeNode {
  return stampStates(readAsIds(root), new Map(Object.entries(states)));
}

/** `root` with every node that carries no read id given its own id as one. */
function readAsIds(root: TreeNode): TreeNode {
  return { readId: root.id, ...root, children: root.children.map(readAsIds) };
}

// The negative control's assertion. An overtaken node and every node under it, at any
// depth, draw no gap, though each drew one before; a live sibling keeps its own.
test("no node at or under an overtaken one draws a gap", () => {
  const root = node(KEY, "issue", [
    node(
      "spec#b-r1",
      "requirement",
      [
        node(
          "verification-md#b-c1",
          "check",
          [node("result-1", "result", [], { gaps: ["no_check"] })],
          { gaps: ["serves_not_requirement"] }
        ),
        node("plan-md#b-t1", "plan_task", [], { gaps: ["nobody_asked"] }),
      ],
      { gaps: ["no_check"] }
    ),
    node("spec#b-r2", "requirement", [], { gaps: ["no_check"] }),
  ]);
  const tree = stamped(root, { "spec#b-r1": { life: "overtaken" } });

  expect(pathTo(root, "spec#b-r1")?.at(-1)?.gaps).toEqual(["no_check"]);
  expect(pathTo(root, "plan-md#b-t1")?.at(-1)?.gaps).toEqual(["nobody_asked"]);
  expect(pathTo(tree, "spec#b-r1")?.at(-1)?.gaps).toEqual([]);
  expect(pathTo(tree, "verification-md#b-c1")?.at(-1)?.gaps).toEqual([]);
  expect(pathTo(tree, "result-1")?.at(-1)?.gaps).toEqual([]);
  expect(pathTo(tree, "plan-md#b-t1")?.at(-1)?.gaps).toEqual([]);
  for (const id of ["spec#b-r1", "verification-md#b-c1", "result-1", "plan-md#b-t1"]) {
    expect(pathTo(tree, id)?.at(-1)?.closed, id).toBe(true);
  }
  // Only the overtaken node is itself overtaken; what sits under it is closed by it.
  expect(pathTo(tree, "spec#b-r1")?.at(-1)?.life).toBe("overtaken");
  expect("life" in (pathTo(tree, "plan-md#b-t1")?.at(-1) ?? {})).toBe(false);
  expect(pathTo(tree, "spec#b-r2")?.at(-1)).toStrictEqual(pathTo(root, "spec#b-r2")?.at(-1));
});

// A node at or under an overtaken one waits on nobody, since a later node took its
// place: the page opens on none of them and draws none in the waiting tone. An open question
// among them is still a question, which `q` reaches; a live one keeps waiting.
test("no node at or under an overtaken one waits on the reader", () => {
  const root = node(KEY, "issue", [
    node("ask-1", "ask", [], { question: true, waiting: true }),
    node("spec#b-r1", "requirement", [node("ask-2", "ask", [], { question: true, waiting: true })]),
    node("ask-3", "ask", [], { question: true, waiting: true }),
  ]);
  const tree = stamped(root, {
    "ask-1": { life: "overtaken" },
    "spec#b-r1": { life: "overtaken" },
  });

  for (const id of ["ask-1", "ask-2"]) {
    expect(pathTo(tree, id)?.at(-1)?.waiting, id).toBe(false);
    expect(pathTo(tree, id)?.at(-1)?.question, id).toBe(true);
  }
  expect(pathTo(tree, "ask-3")?.at(-1)).toStrictEqual(pathTo(root, "ask-3")?.at(-1));
});

// A node's marks are its entry's and no other. A settled node's entry holds none, and a
// live node visible to everyone has no entry at all: neither carries a mark or is closed, and
// each is the node it was before, key for key.
test("a live or settled node has no mark", () => {
  const root = node(KEY, "issue", [
    node("spec#b-r1", "requirement", [node("verification-md#b-c1", "check")], {
      gaps: ["no_check"],
    }),
    node("spec#b-r2", "requirement", [], { gaps: ["no_check"] }),
    node("spec#b-r3", "requirement"),
  ]);
  const tree = stamped(root, { "spec#b-r1": {}, "spec#b-r3": { life: "proposed" } });

  for (const id of ["spec#b-r1", "verification-md#b-c1", "spec#b-r2"]) {
    const found = pathTo(tree, id)?.at(-1);
    expect(
      ["closed", "life", "onlyYou"].filter((key) => key in (found ?? {})),
      id
    ).toEqual([]);
    expect(found, id).toStrictEqual(pathTo(root, id)?.at(-1));
  }
});

// A proposed node draws no gap, the page's own included: a plan task that serves nothing
// draws no "No reason given", a check citing no statement none either, and a statement two
// sources disagree on none. A proposed node closes nothing under it, and a live one keeps its gap.
test("a proposed node draws no gap the page made", () => {
  const root = node(KEY, "issue", [
    node("plan-md#b-t1", "plan_task", [], { gaps: ["nobody_asked"] }),
    node("verification-md#b-c1", "check", [], { gaps: ["serves_not_requirement"] }),
    node("spec#b-r1", "requirement", [], { gaps: ["shaped_disagreement"] }),
    node("plan-md#b-t2", "plan_task", [], { gaps: ["nobody_asked"] }),
  ]);
  const proposed = ["plan-md#b-t1", "verification-md#b-c1", "spec#b-r1"];
  const tree = stamped(
    root,
    Object.fromEntries(proposed.map((id) => [id, { life: "proposed" } as const]))
  );

  for (const id of proposed) {
    expect(pathTo(tree, id)?.at(-1)?.life, id).toBe("proposed");
    expect(pathTo(tree, id)?.at(-1)?.gaps, id).toEqual([]);
    expect("closed" in (pathTo(tree, id)?.at(-1) ?? {}), id).toBe(false);
  }
  expect(pathTo(tree, "plan-md#b-t2")?.at(-1)?.gaps).toEqual(["nobody_asked"]);
});

// A chip names a node by its reference, whatever version it pins, and a parent line by
// its id, and each carries that node's marks, in `chips` and in `alsoUnder` alike; one naming an
// unmarked node carries none. A parent line's node need not be in the tree at all: a view draws
// it as no row.
test("a chip and a parent line carry the marks of the node they name", () => {
  const marked = `dispatch://${KEY}/spec#b-r7`;
  const pinned = `${marked}@v3`;
  const plain = `dispatch://${KEY}/spec#b-r8`;
  const line = (id: string): ParentLine => ({ id, readId: id, text: `${id}.`, type: "ask" });
  const root = node(KEY, "issue", [
    node("spec#b-r7", "requirement", [], { reference: marked }),
    node("spec#b-r8", "requirement", [], { reference: plain }),
    node("decisions-md#b-d1", "decision", [], {
      alsoUnder: [chip(marked)],
      chips: [chip(marked), chip(pinned), chip(plain)],
    }),
    node("verification-md#b-c1", "check", [], { parentLine: line("ask-1") }),
    node("verification-md#b-c2", "check", [], { parentLine: line("ask-2") }),
  ]);
  const tree = stamped(root, {
    "ask-1": { life: "proposed" },
    "spec#b-r7": { life: "proposed", onlyYou: true },
  });

  const decision = pathTo(tree, "decisions-md#b-d1")?.at(-1);
  expect(decision?.chips).toStrictEqual([
    { ...chip(marked), life: "proposed", onlyYou: true },
    { ...chip(pinned), life: "proposed", onlyYou: true },
    chip(plain),
  ]);
  expect(decision?.alsoUnder).toStrictEqual([{ ...chip(marked), life: "proposed", onlyYou: true }]);
  expect(pathTo(tree, "verification-md#b-c1")?.at(-1)?.parentLine).toStrictEqual({
    ...line("ask-1"),
    life: "proposed",
  });
  expect(pathTo(tree, "verification-md#b-c2")?.at(-1)?.parentLine).toStrictEqual(line("ask-2"));
  const states = statesByReference(tree);
  expect([...states]).toStrictEqual([[marked, { life: "proposed", onlyYou: true }]]);
  expect(marksAt(states, pinned)).toStrictEqual({ life: "proposed", onlyYou: true });
  expect(marksAt(states, marked)).toStrictEqual({ life: "proposed", onlyYou: true });
  expect(marksAt(states, `${plain}@v2`)).toBeUndefined();
});

test("a tree given no states is answered as it is", () => {
  const root = node(KEY, "issue", [node("spec#b-r1", "requirement", [], { gaps: ["no_check"] })]);
  expect(stampStates(root, new Map())).toBe(root);
});

// A folded branch's count counts an overtaken node and the nodes under it as it counts every
// node, so the states put on the tree leave every count as it was.
test("an overtaken node counts in its parent's held count as any node", () => {
  const root = node(KEY, "issue", [
    node("spec#b-r1", "requirement", [node("verification-md#b-c1", "check")]),
    node("spec#b-r2", "requirement", [
      node("verification-md#b-c2", "check"),
      node("plan-md#b-t2", "plan_task"),
    ]),
    node("spec#b-r3", "requirement", [node("verification-md#b-c3", "check")]),
  ]);
  expect(root.held).toBe(7);
  const tree = stamped(root, {
    "spec#b-r2": { life: "overtaken" },
    "spec#b-r3": { life: "overtaken" },
  });

  expect(tree.held).toBe(7);
  expect(pathTo(tree, "spec#b-r2")?.at(-1)?.held).toBe(2);
  expect(stamped(root, { "spec#b-r3": { life: "overtaken" } }).held).toBe(7);
});

// A node's marks are its words and its shapes, its life first, in every place that
// draws it; a node that has none draws nothing at all, not even an empty box.
test("a proposed node is marked Proposed with its shape, and a node with no mark draws none", () => {
  const proposed = render(createElement(NodeMarks, { marks: { life: "proposed" } }));
  try {
    const marks = proposed.container.querySelectorAll('[data-node-mark="proposed"]');
    expect(marks).toHaveLength(1);
    expect(marks[0]?.textContent).toBe("Proposed");
    expect(marks[0]?.querySelector("svg")).toHaveAttribute("data-glyph", "mark-proposed");
    expect(proposed.container.querySelectorAll("[data-node-mark]")).toHaveLength(1);
  } finally {
    proposed.unmount();
  }

  const both = render(createElement(NodeMarks, { marks: { life: "overtaken", onlyYou: true } }));
  try {
    const marks = [...both.container.querySelectorAll<HTMLElement>("[data-node-mark]")];
    expect(marks.map((mark) => mark.dataset.nodeMark)).toEqual(["overtaken", "only_you"]);
    expect(marks.map((mark) => mark.textContent)).toEqual(["Overtaken", "Only you"]);
  } finally {
    both.unmount();
  }

  const none = render(createElement(NodeMarks, { compact: true, marks: {} }));
  try {
    expect(none.container.innerHTML).toBe("");
  } finally {
    none.unmount();
  }
});

/** An issue whose second row is a requirement a later one took the place of, holding a comment,
 *  between two live comments. */
function overtakenBetween(): TreeNode {
  return stamped(
    node(KEY, "issue", [
      node("comment-1", "comment"),
      node("spec#b-r8", "requirement", [node("comment-8", "comment")], {
        reference: `dispatch://${KEY}/spec#b-r8`,
      }),
      node("comment-3", "comment"),
    ]),
    { "spec#b-r8": { life: "overtaken" } }
  );
}

/** The ids of the nodes whose rows the tree draws under the issue, in the order it draws them. */
function rowIds(container: HTMLElement): (string | undefined)[] {
  return [...container.querySelectorAll<HTMLElement>("[data-node-row]")]
    .map((row) => row.dataset.nodeId)
    .filter((id) => id !== KEY);
}

// An overtaken node is a row at its place among its parent's other children, at their level, and
// carries the word Overtaken and its shape; drawn whole, as the page opens with every node, its
// child is a row under it, one level down, as under any row.
test("an overtaken node is a row at its place with its mark and its child row under it", async () => {
  readWide();
  const view = renderTree(overtakenBetween());
  const levelOf = (id: string) => Number(rowOf(view.container, id)?.getAttribute("aria-level"));
  try {
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    expect(rowIds(view.container)).toEqual(["comment-1", "spec#b-r8", "comment-8", "comment-3"]);
    expect(levelOf("spec#b-r8")).toBe(levelOf("comment-1"));
    expect(levelOf("comment-8")).toBe(levelOf("spec#b-r8") + 1);
    const row = rowOf(view.container, "spec#b-r8");
    expect(marksIn(row)).toEqual(["overtaken"]);
    const mark = row?.querySelector("[data-node-mark]");
    expect(mark?.textContent).toBe(ROW_MARK_WORDS.overtaken);
    expect(mark?.querySelector("svg")).toHaveAttribute("data-glyph", GLYPH_TOKENS.marks.overtaken);
  } finally {
    view.unmount();
  }
});

/** The page's own `issue` scope, which every one of the tree's keys is registered in. */
function IssueScope(): ReactNode {
  useKeymapScope("issue");
  return null;
}

const originalMatchMedia = window.matchMedia;
afterEach(() => {
  window.matchMedia = originalMatchMedia;
});

/** happy-dom answers every media query against its own 1024 px window: this answers them for
 *  one `width` px wide, as `IssueTree.test.tsx`'s `setViewport` does: from 1024 px up the tree
 *  draws its rows whole, and below it one node at a time (`IssueTree.tsx`'s `ZOOMED_TREE_QUERY`). */
function readAt(width: number): void {
  window.matchMedia = ((query: string) => {
    const max = /max-width:\s*([\d.]+)px/.exec(query)?.[1];
    const min = /min-width:\s*([\d.]+)rem/.exec(query)?.[1];
    const matched =
      max !== undefined ? width <= Number(max) : min !== undefined && width >= Number(min) * 16;
    return {
      addEventListener: () => {},
      addListener: () => {},
      dispatchEvent: () => true,
      matches: query.startsWith("not all and ") ? !matched : matched,
      media: query,
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    } as MediaQueryList;
  }) as typeof window.matchMedia;
}

/** `readAt` a window 1440 px wide, where the tree draws its rows whole. */
function readWide(): void {
  readAt(1440);
}

/** `root` drawn by the page's tree, under the page's keymap and the providers it reads, as
 *  `IssueTree.test.tsx`'s `renderTree` draws it: no address, no filter, no document, under the
 *  names store with what the page publishes for the tree's chips (`publishedTree`); with
 *  `beside` drawn after the tree, under the same providers. */
function renderTree(root: TreeNode, ...beside: ReactNode[]) {
  return renderTreeAt(undefined, root, ...beside);
}

/** `renderTree`'s tree at an address: `reveal` is the chain down to the node the address names,
 *  which the tree is then to put the focus on (`slots/body.tsx`), as `IssueTree.test.tsx`'s
 *  `renderTree` gives it; undefined for no address. */
function renderTreeAt(
  reveal: readonly TreeNode[] | undefined,
  root: TreeNode,
  ...beside: ReactNode[]
) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const published = publishedTree(root);
  return render(
    createElement(
      MemoryRouter,
      null,
      createElement(
        QueryClientProvider,
        { client },
        createElement(
          KeymapProvider,
          null,
          createElement(
            MarginProvider,
            null,
            createElement(
              FocusedNodeProvider,
              null,
              createElement(IssueScope),
              createElement(Publisher, { published }),
              createElement(IssueTree, {
                artifacts: new Map(),
                context: new Set<string>(),
                emptyLine: "Nothing here.",
                focusReveal: reveal !== undefined,
                holdsEscape: false,
                isClosed: false,
                onEditing: () => {},
                published,
                reveal,
                root,
                rootRow: true,
              }),
              ...beside
            )
          )
        )
      )
    ),
    // Under the names store, as the shell draws every page (`app.tsx`).
    { wrapper: ReferenceNamesProvider }
  );
}

/** The row the tree draws for the node `id`, or null where it draws none. */
function rowOf(container: HTMLElement, id: string): HTMLElement | null {
  return container.querySelector<HTMLElement>(`[data-node-row][data-node-id="${id}"]`);
}

/** The id of the node whose row holds the focus, or undefined where none does. */
function focusedRow(container: HTMLElement): string | undefined {
  const active = document.activeElement;
  return [...container.querySelectorAll<HTMLElement>("[data-node-row]")].find((row) =>
    row.contains(active)
  )?.dataset.nodeId;
}

// `j` and `k` step onto an overtaken row and off it as they do any row: down from the row above
// it, on to the row after it, its child, drawn open as every row is, and back up, the row the
// reader is on holding the tree's tab stop.
test("`j` and `k` reach and leave an overtaken row as any row", async () => {
  readWide();
  const user = userEvent.setup();
  const view = renderTree(overtakenBetween());
  try {
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    const steps = [
      ["j", "spec#b-r8"],
      ["j", "comment-8"],
      ["j", "comment-3"],
      ["k", "comment-8"],
      ["k", "spec#b-r8"],
      ["k", "comment-1"],
    ] as const;
    for (const [key, to] of steps) {
      await user.keyboard(key);
      await waitFor(() => expect(focusedRow(view.container), `${key} to ${to}`).toBe(to));
      expect(rowOf(view.container, to)).toHaveAttribute("tabindex", "0");
    }
  } finally {
    view.unmount();
  }
});

// An overtaken row is drawn open, and its fold control folds it and opens it again as any row's
// does: its child is a row under it while the branch is open, and none once it is folded.
test("an overtaken row's fold control folds it and opens it again", async () => {
  readWide();
  const user = userEvent.setup();
  const view = renderTree(overtakenBetween());
  const control = () =>
    rowOf(view.container, "spec#b-r8")?.querySelector<HTMLElement>("button[data-node-bullet]");
  try {
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    expect(rowOf(view.container, "comment-8")).not.toBeNull();
    expect(rowOf(view.container, "spec#b-r8")).toHaveAttribute("aria-expanded", "true");
    const presses = [
      [CONTROL_LABELS.fold, false],
      [CONTROL_LABELS.unfold, true],
    ] as const;
    for (const [label, open] of presses) {
      expect(control()).toHaveAttribute("aria-label", label);
      await user.click(control() as HTMLElement);
      await waitFor(() => expect(rowOf(view.container, "comment-8") !== null, label).toBe(open));
      expect(rowOf(view.container, "spec#b-r8")).toHaveAttribute("aria-expanded", String(open));
    }
  } finally {
    view.unmount();
  }
});

// An address that names an overtaken node, or a node under one, focuses that node's row, which
// stands at its place among its parent's children, as an address to any node does.
test("an address to an overtaken node focuses its row", async () => {
  readWide();
  const tree = overtakenBetween();
  const cases = [
    ["spec#b-r8", "comment-1"],
    ["comment-8", "spec#b-r8"],
  ] as const;
  for (const [id, above] of cases) {
    const view = renderTreeAt(pathTo(tree, id), tree);
    try {
      await waitFor(() => expect(focusedRow(view.container), id).toBe(id));
      const ids = rowIds(view.container);
      expect(ids.indexOf(id), `${id} after ${above}`).toBe(ids.indexOf(above) + 1);
    } finally {
      view.unmount();
    }
  }
});

// An overtaken open question that waited on the reader waits no more, so the page opens on the
// outline's first row, though the question is a row of its own with its mark. An overtaken node's
// branch arrives open as every branch does, with the question under it drawn in it, and the page
// does not open on that question either.
test("the page does not open on an overtaken open question", async () => {
  readWide();
  const asking = { question: true, state: "open", waiting: true } as const;
  const view = renderTree(
    stamped(
      node(KEY, "issue", [
        node("comment-1", "comment"),
        node("ask-1", "ask", [], asking),
        node("spec#b-r8", "requirement", [node("ask-2", "ask", [], asking)]),
      ]),
      { "ask-1": { life: "overtaken" }, "spec#b-r8": { life: "overtaken" } }
    )
  );
  try {
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    expect(rowIds(view.container)).toEqual(["comment-1", "ask-1", "spec#b-r8", "ask-2"]);
    expect(
      rowOf(view.container, "ask-1")?.querySelector('[data-node-mark="overtaken"]')
    ).not.toBeNull();
    expect(rowOf(view.container, "spec#b-r8")).toHaveAttribute("aria-expanded", "true");
    await act(async () => {});
    expect(focusedRow(view.container)).toBe("comment-1");
  } finally {
    view.unmount();
  }
});

// `q` and `w` take the reader to an overtaken question and an overtaken decision as to any
// other: each is a row at its place, which the key focuses.
test("`q` and `w` reach an overtaken question and decision", async () => {
  readWide();
  const user = userEvent.setup();
  const requirement = `dispatch://${KEY}/spec#b-r1`;
  const view = renderTree(
    stamped(
      node(KEY, "issue", [
        node("comment-1", "comment"),
        node("spec#b-r1", "requirement", [], { reference: requirement }),
        node("ask-1", "ask", [], { question: true, state: "open" }),
        node("decisions-md#b-d1", "decision", [], { chips: [chip(requirement)] }),
      ]),
      { "ask-1": { life: "overtaken" }, "decisions-md#b-d1": { life: "overtaken" } }
    )
  );
  try {
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    expect(rowIds(view.container)).toEqual([
      "comment-1",
      "spec#b-r1",
      "ask-1",
      "decisions-md#b-d1",
    ]);

    await user.keyboard("q");
    await waitFor(() => expect(focusedRow(view.container)).toBe("ask-1"));
    await user.keyboard("k");
    await waitFor(() => expect(focusedRow(view.container)).toBe("spec#b-r1"));
    await user.keyboard("w");
    await waitFor(() => expect(focusedRow(view.container)).toBe("decisions-md#b-d1"));
  } finally {
    view.unmount();
  }
});

// A row's words are drawn from their reading, which the page's own engine reads.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** The text of the first element under `container` that `selector` matches, its white space
 *  read as one space. */
function textOf(container: HTMLElement, selector: string): string | undefined {
  return container.querySelector(selector)?.textContent?.replace(/\s+/gu, " ").trim();
}

/** A check of the Verification document whose one result passed, or failed, at the commit the
 *  page names (`verification-fixtures.ts`). */
function checkThat(blockId: string, passed: boolean): CheckBlock {
  return check({
    block_id: blockId,
    pass_stands: { answer: passed ? "yes" : "no" },
    results: [
      result({
        check_block_id: blockId,
        failed: passed ? 0 : 1,
        id: `result-${blockId}`,
        verdict: passed ? "pass" : "fail",
      }),
    ],
  });
}

/** The coverage read's rows over `root`, as the server builds them (`coverage_tuples.go`'s
 *  `coverTreeRequirements`): one row for each statement, with its own state, its coverage
 *  `closed` where it, or a node above it in its walk, is overtaken by a state of its own
 *  (`closed`), `proposed` where it is proposed, and the checks that serve it - its count of
 *  checks where it carries one (`TreeNode.counts`), else the checks under it but a proposed or an
 *  overtaken one, which serves nothing (`readServingChecks`). A question's row drawn from a block
 *  takes the state of its ask's node (`stateIdOf`), which stands in no statement's walk, so the
 *  row of a statement under it is not closed by it, though the page closes that statement. No
 *  result is read for a proof here, so a row a check serves is `unproven`. */
function coverageRows(root: TreeNode): RequirementCoverage[] {
  const rows = new Map<string, RequirementCoverage>();
  // `above` is whether a node above `node` in its walk is overtaken by a state of its own.
  const walk = (node: TreeNode, above: boolean): void => {
    const closed = above || (node.life === "overtaken" && stateIdOf(node) === node.readId);
    const ref = node.reference;
    if (node.statement === true && ref !== undefined && !rows.has(ref)) {
      const checks =
        node.counts?.checks ??
        node.children.filter(
          (child) =>
            child.type === "check" && child.life !== "proposed" && child.life !== "overtaken"
        ).length;
      rows.set(ref, {
        block_id: ref.slice(ref.lastIndexOf("#b-") + "#b-".length),
        checks,
        coverage: closed
          ? "closed"
          : node.life === "proposed"
            ? "proposed"
            : checks === 0
              ? "gap"
              : "unproven",
        gaps: [],
        plan_tasks: 0,
        ref,
        state: node.life ?? "live",
        text: node.text,
      });
    }
    for (const child of node.children) walk(child, closed);
  };
  walk(root, false);
  return [...rows.values()];
}

/** The Verification view over `root`, with `checks` as the issue's results read and the coverage
 *  read's rows over it (`coverageRows`), drawn as its slot draws it
 *  (`VerificationFilterBody.drawn.test.tsx`'s `bodyOf`): under the words `query`,
 *  where a case types any, with the view under no query as `whole`, which is that view itself
 *  where no query is typed, on the page's `address`, which carries the words as the page's own
 *  does (`?q=`); with `addressed`, the nodes the address names and what the body tells the slot
 *  of them, as the slot hands them on where it may tell (`slots/view-verification.tsx`); under
 *  the names store with what the page publishes for the tree's chips (`publishedTree`), and no
 *  names line. */
function drawVerification(
  root: TreeNode,
  checks: readonly CheckBlock[],
  query = "",
  address = query === "" ? "/" : `/?q=${encodeURIComponent(query)}`,
  addressed?: { readonly named: ReadonlySet<string>; readonly tell: (drawn: boolean) => void }
) {
  const preset = presetView(root, "verification", query, new Set());
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const published = publishedTree(root);
  return render(
    createElement(
      QueryClientProvider,
      { client },
      createElement(
        ReferenceNamesProvider,
        null,
        createElement(
          MemoryRouter,
          { initialEntries: [address] },
          createElement(
            NoTooltips,
            null,
            createElement(Publisher, { published }),
            createElement(VerificationFilterBody, {
              ...(addressed === undefined ? {} : { addressed }),
              checks,
              context: preset.context,
              coverage: {
                gaps: [],
                has_plan: false,
                issue: KEY,
                requirements: coverageRows(root),
                spec_artifact_id: "artifact-spec",
              },
              namesLine: NO_FAILED_READS,
              published,
              reads: { state: "answered" },
              view: preset.root,
              whole: queryTyped(query)
                ? presetView(root, "verification", "", new Set()).root
                : undefined,
            })
          )
        )
      )
    )
  );
}

/** The id of each row the Verification view's tree draws, in the order drawn, as each row's
 *  element carries it (`data-verification-row`): the issue's, each statement's, and each check's
 *  under a statement as the statement's id and the check's. */
function verificationRows(container: HTMLElement): (string | null)[] {
  return [...container.querySelectorAll("[data-verification-row]")].map((row) =>
    row.getAttribute("data-verification-row")
  );
}

// Under Verification a proposed check carries its mark in its row's state line, and no
// count of the summary reads it: not the covered count, not the "of M" that count is out of (a
// proposed statement is out of both), and not the legend's count of how the checks stand. The
// table still draws every statement and every check.
test("the Verification view marks a proposed check and leaves it out of its summary", () => {
  const passed = checkThat("c-one", true);
  const failedProposed = checkThat("c-two", false);
  const passedProposed = checkThat("c-three", true);
  const underProposed = checkThat("c-four", true);
  const first = statementNode("r1", "The page draws the checks.", [
    checkNode(passed),
    checkNode(failedProposed),
  ]);
  const second = statementNode("r2", "Only a proposed check serves this one.", [
    checkNode(passedProposed),
  ]);
  const third = statementNode("r3", "This one is itself proposed.", [checkNode(underProposed)]);
  const view = drawVerification(
    stamped(node(KEY, "issue", [first, second, third]), {
      [checkNode(failedProposed).id]: { life: "proposed" },
      [checkNode(passedProposed).id]: { life: "proposed" },
      [third.id]: { life: "proposed" },
    }),
    [passed, failedProposed, passedProposed, underProposed]
  );
  try {
    const markOn = (id: string) =>
      rowOf(view.container, id)?.querySelector('[data-node-mark="proposed"]');
    expect(markOn(checkNode(failedProposed).id)?.textContent).toContain(ROW_MARK_WORDS.proposed);
    expect(markOn(checkNode(passedProposed).id)?.textContent).toContain(ROW_MARK_WORDS.proposed);
    expect(markOn(third.id)?.textContent).toContain(ROW_MARK_WORDS.proposed);
    expect(
      rowOf(view.container, checkNode(passed).id)?.querySelector("[data-node-mark]")
    ).toBeNull();
    expect(view.container.querySelectorAll("[data-verification-card]")).toHaveLength(3);
    expect(view.container.querySelectorAll("[data-verification-check]")).toHaveLength(4);

    // The first statement is covered by its live check alone, and Met by it; the second, whose
    // only check is proposed, is not covered and is Unknown; the third is in no count.
    expect(textOf(view.container, "[data-verification-covered]")).toBe(
      `1 ${VERIFICATION_WORDS.of} 2 ${VERIFICATION_WORDS.covered}`
    );
    expect(
      [...view.container.querySelectorAll("[data-verification-legend] > span")].map(
        (state) => state.textContent
      )
    ).toEqual([
      `1${VERIFICATION_WORDS.states.met}`,
      `0${VERIFICATION_WORDS.states.not_met}`,
      `1${VERIFICATION_WORDS.states.unknown}`,
    ]);
    expect(textOf(view.container, '[data-check-legend-state="passed"]')).toBe(
      `1${NODE_STATE_WORDS.check.passed}`
    );
    expect(textOf(view.container, '[data-check-legend-state="failed"]')).toBe(
      `0${NODE_STATE_WORDS.check.failed}`
    );
  } finally {
    view.unmount();
  }
});

// Under Verification an overtaken check is a row under the statement it serves, after the check
// before it, carrying Overtaken and its shape beside how it stands, with its result, as any check
// is; an overtaken statement is a row at its place, with its check under it. The tree's keys walk
// them as they walk any row. No count of the summary reads either: not the covered count, not the
// "of M" it is out of, and not the legend's counts of how the statements and the checks stand.
test("under Verification an overtaken check is a row under its statement, marked, with its result", async () => {
  const user = userEvent.setup();
  const passed = checkThat("c-one", true);
  const replaced = checkThat("c-two", false);
  const underReplaced = checkThat("c-three", true);
  const first = statementNode("r1", "The page draws the checks.", [
    checkNode(passed),
    checkNode(replaced),
  ]);
  const second = statementNode("r2", "A later statement took this one's place.", [
    checkNode(underReplaced),
  ]);
  const passedId = checkNode(passed).id;
  const replacedId = checkNode(replaced).id;
  const underId = checkNode(underReplaced).id;
  const view = drawVerification(
    stamped(node(KEY, "issue", [first, second]), {
      [replacedId]: { life: "overtaken" },
      [second.id]: { life: "overtaken" },
    }),
    [passed, replaced, underReplaced]
  );
  try {
    expect(verificationRows(view.container)).toEqual([
      KEY,
      first.id,
      `${first.id} ${passedId}`,
      `${first.id} ${replacedId}`,
      second.id,
      `${second.id} ${underId}`,
    ]);
    const check = rowOf(view.container, replacedId);
    expect(check !== null && rowOf(view.container, first.id)?.contains(check) === true).toBe(true);
    expect(marksIn(check)).toEqual(["overtaken"]);
    expect(check?.querySelector('[data-node-mark="overtaken"]')?.textContent).toContain(
      ROW_MARK_WORDS.overtaken
    );
    expect(check?.querySelector('[data-node-state="failed"]')).not.toBeNull();
    expect(check?.querySelector("[data-verification-result]")).not.toBeNull();
    const statementRow = view.container.querySelector(`[data-verification-row="${second.id}"]`);
    expect(statementRow?.querySelector('[data-node-mark="overtaken"]')).not.toBeNull();

    expect(textOf(view.container, "[data-verification-covered]")).toBe(
      `1 ${VERIFICATION_WORDS.of} 1 ${VERIFICATION_WORDS.covered}`
    );
    expect(rowOf(view.container, first.id)).toHaveAttribute("data-statement-state", "met");
    expect(textOf(view.container, '[data-check-legend-state="failed"]')).toBe(
      `0${NODE_STATE_WORDS.check.failed}`
    );
    expect(textOf(view.container, '[data-check-legend-state="passed"]')).toBe(
      `1${NODE_STATE_WORDS.check.passed}`
    );

    act(() => rowOf(view.container, first.id)?.focus());
    await user.keyboard("j");
    expect(document.activeElement).toBe(rowOf(view.container, passedId));
    await user.keyboard("j");
    expect(document.activeElement).toBe(check);
    await user.keyboard("j");
    expect(document.activeElement).toBe(statementRow);
    await user.keyboard("k");
    expect(document.activeElement).toBe(check);
  } finally {
    view.unmount();
  }
});

// The server closes a statement's coverage row only where the statement, or a node above it in
// its walk, is overtaken by a state of its own (`coverage_tuples.go`'s `closed`). A question
// written as an `:::ask` block is overtaken by its ask's node, which stands in no statement's
// walk, so the row of a statement under that question comes unclosed; the page closes that
// statement with its question all the same (`stateIdOf`). No count of the summary reads it: not
// the covered count, and not the "of M" that count is out of. The table still draws it.
test("the Verification summary leaves out a statement under an overtaken question whose coverage row is not closed", () => {
  const ask = "ask-q1";
  const passed = checkThat("c-one", true);
  const underQuestion = checkThat("c-two", true);
  const first = statementNode("r1", "The page draws the checks.", [checkNode(passed)]);
  const asked = statementNode("r2", "A later question took this one's place.", [
    checkNode(underQuestion),
  ]);
  const root = stamped(
    node(KEY, "issue", [
      first,
      node("spec#b-q1", "ask", [asked], { source: { id: ask, issue: KEY, kind: "ask" } }),
    ]),
    { [ask]: { life: "overtaken" } }
  );
  expect(coverageRows(root).map((row) => [row.ref, row.coverage])).toEqual([
    [first.reference, "unproven"],
    [asked.reference, "unproven"],
  ]);
  expect(nodesInOrder(root).find((one) => one.id === asked.id)?.closed).toBe(true);

  const view = drawVerification(root, [passed, underQuestion]);
  try {
    expect(view.container.querySelectorAll("[data-verification-card]")).toHaveLength(2);
    expect(textOf(view.container, "[data-verification-covered]")).toBe(
      `1 ${VERIFICATION_WORDS.of} 1 ${VERIFICATION_WORDS.covered}`
    );
  } finally {
    view.unmount();
  }
});

// Under Verification the words filter matches an overtaken check's words and draws it as it draws
// every match: a row under the statement it serves, marked Overtaken, with its result.
test("the Verification view's words filter draws an overtaken check it matches as a row, marked", () => {
  const passed = checkThat("c-one", true);
  const replaced = { ...checkThat("c-two", false), text: "The zebrafinch words are drawn." };
  const replacedId = checkNode(replaced).id;
  const first = statementNode("r1", "The page draws the checks.", [
    checkNode(passed),
    checkNode(replaced),
  ]);
  const view = drawVerification(
    stamped(node(KEY, "issue", [first]), { [replacedId]: { life: "overtaken" } }),
    [passed, replaced],
    "zebrafinch"
  );
  try {
    expect(verificationRows(view.container)).toEqual([KEY, first.id, `${first.id} ${replacedId}`]);
    const match = rowOf(view.container, replacedId);
    expect(marksIn(match)).toEqual(["overtaken"]);
    expect(match?.querySelector("[data-verification-result]")).not.toBeNull();
  } finally {
    view.unmount();
  }
});

// Under Verification a words match on a result of an overtaken check is drawn with its check, a
// row under the statement it serves marked Overtaken: the view lifts the result under its
// statement, with the check as its parent line, and the body draws it in the check that holds
// it, as it does for a live check, which carries no mark.
test("the Verification view draws a result of an overtaken check the words match with its check", () => {
  const passed = checkThat("c-one", true);
  const seen: CheckBlock = {
    ...checkThat("c-two", false),
    results: [
      result({
        check_block_id: "c-two",
        failed: 1,
        id: "result-c-two",
        observed: "zebrafinch seen",
        verdict: "fail",
      }),
    ],
  };
  const seenId = checkNode(seen).id;
  const first = statementNode("r1", "The page draws the checks.", [
    checkNode(passed),
    checkNode(seen),
  ]);
  const issue = node(KEY, "issue", [first]);
  const overtaken = drawVerification(
    stamped(issue, { [seenId]: { life: "overtaken" } }),
    [passed, seen],
    "zebrafinch"
  );
  try {
    expect(verificationRows(overtaken.container)).toEqual([KEY, first.id, `${first.id} ${seenId}`]);
    const check = rowOf(overtaken.container, seenId);
    expect(check?.textContent).toContain("zebrafinch seen");
    expect(marksIn(check)).toEqual(["overtaken"]);
  } finally {
    overtaken.unmount();
  }

  const live = drawVerification(issue, [passed, seen], "zebrafinch");
  try {
    const check = rowOf(live.container, seenId);
    expect(check?.textContent).toContain("zebrafinch seen");
    expect(marksIn(check)).toEqual([]);
  } finally {
    live.unmount();
  }
});

// Under Verification a words match on a result of an overtaken check that serves no statement is
// drawn with that check, a row of the table's own marked Overtaken, as the check is with no words:
// the view lifts the result to its top, with the check as its parent line, and the body draws the
// check that holds it in its place. Where the result the words kept is not the check's latest,
// the check draws it under its latest, so the result is on screen. With the check live the result
// is drawn with it too, and the check carries no mark.
test("the Verification view draws a result of an overtaken check that serves no statement with its check", () => {
  const passed = checkThat("c-one", true);
  // The check, with one failed result for each of `observed`, each recorded after the one before.
  const seenWith = (observed: readonly string[]): CheckBlock => ({
    ...checkThat("c-two", false),
    results: observed.map((words, place) =>
      result({
        check_block_id: "c-two",
        created_at: `2026-11-14T01:0${place}:00Z`,
        failed: 1,
        id: `result-c-two-${place}`,
        observed: words,
        verdict: "fail",
      })
    ),
  });
  const seen = seenWith(["zebrafinch seen"]);
  const seenId = checkNode(seen).id;
  const issueWith = (loose: CheckBlock): TreeNode =>
    node(KEY, "issue", [
      statementNode("r1", "The page draws the checks.", [checkNode(passed)]),
      checkNode(loose),
    ]);
  const overtaken = drawVerification(
    stamped(issueWith(seen), { [seenId]: { life: "overtaken" } }),
    [passed, seen],
    "zebrafinch"
  );
  try {
    expect(verificationRows(overtaken.container)).toEqual([KEY, seenId]);
    const check = rowOf(overtaken.container, seenId);
    expect(check).toHaveAttribute("data-verification-loose", "");
    expect(check?.textContent).toContain("zebrafinch seen");
    expect(marksIn(check)).toEqual(["overtaken"]);
    expect(overtaken.container.querySelector("[data-verification-other]")).toBeNull();
  } finally {
    overtaken.unmount();
  }

  const earlier = seenWith(["zebrafinch seen", "nothing seen"]);
  const kept = drawVerification(
    stamped(issueWith(earlier), { [seenId]: { life: "overtaken" } }),
    [passed, earlier],
    "zebrafinch"
  );
  try {
    const check = rowOf(kept.container, seenId);
    expect(check?.textContent).toContain("zebrafinch seen");
    expect(marksIn(check)).toEqual(["overtaken"]);
  } finally {
    kept.unmount();
  }

  const live = drawVerification(issueWith(seen), [passed, seen], "zebrafinch");
  try {
    expect(live.container.querySelector("[data-verification-other]")).toBeNull();
    const check = rowOf(live.container, seenId);
    expect(check?.textContent).toContain("zebrafinch seen");
    expect(marksIn(check)).toEqual([]);
  } finally {
    live.unmount();
  }
});

/** An issue of one statement whose second check a later one took the place of, the checks the
 *  results read answers for it - the live one, then the overtaken one - and the ids of the
 *  statement and of its two checks. */
function statementWithOvertakenCheck() {
  const passed = checkThat("c-one", true);
  const replaced = checkThat("c-two", false);
  const replacedId = checkNode(replaced).id;
  const served = statementNode("r1", "The page draws the checks.", [
    checkNode(passed),
    checkNode(replaced),
  ]);
  const root = stamped(node(KEY, "issue", [served]), { [replacedId]: { life: "overtaken" } });
  return {
    checks: [passed, replaced],
    passedId: checkNode(passed).id,
    replacedId,
    root,
    statementId: served.id,
  };
}

// An address that names an overtaken check draws its row under the statement it serves, at its
// place after the check before it, marked Overtaken, as an address naming any check draws its
// row; and the body tells the page it draws the node the address names, so the page keeps the
// view drawn.
test("the Verification view draws the row of an overtaken check an address names", () => {
  const { checks, passedId, replacedId, root, statementId } = statementWithOvertakenCheck();
  const told: boolean[] = [];
  const view = drawVerification(root, checks, "", `/issues/${KEY}/verification#b-c-two`, {
    named: new Set([replacedId]),
    tell: (drawn) => told.push(drawn),
  });
  try {
    expect(verificationRows(view.container)).toEqual([
      KEY,
      statementId,
      `${statementId} ${passedId}`,
      `${statementId} ${replacedId}`,
    ]);
    expect(marksIn(rowOf(view.container, replacedId))).toEqual(["overtaken"]);
    expect(told.at(-1)).toBe(true);
  } finally {
    view.unmount();
  }
});

// A state chip's count leaves a proposed statement out, and so does what the chip draws: the
// chip says one number and draws as many statements.
test("the Verification view's state chip draws no statement its count leaves out", async () => {
  const user = userEvent.setup();
  const passed = checkThat("c-one", true);
  const passedToo = checkThat("c-three", true);
  const live = statementNode("r1", "The page draws the checks.", [checkNode(passed)]);
  const proposed = statementNode("r3", "This one is proposed.", [checkNode(passedToo)]);
  const view = drawVerification(
    stamped(node(KEY, "issue", [live, proposed]), { [proposed.id]: { life: "proposed" } }),
    [passed, passedToo]
  );
  try {
    expect(view.container.querySelectorAll("[data-verification-card]")).toHaveLength(2);
    const met = view.container.querySelector<HTMLElement>('[data-verification-chip="met"]');
    expect(met?.textContent?.replace(/\s+/gu, " ").trim()).toBe(
      `${VERIFICATION_WORDS.states.met} 1`
    );

    await user.click(met as HTMLElement);
    expect(
      [...view.container.querySelectorAll("[data-verification-card]")].map((card) =>
        card.getAttribute("data-node-id")
      )
    ).toEqual([live.id]);

    await user.click(met as HTMLElement);
    expect(view.container.querySelectorAll("[data-verification-card]")).toHaveLength(2);
  } finally {
    view.unmount();
  }
});

/** The check `blockId`, passed, with one result for each of `observed`, each recorded after the
 *  one before, so the last is its latest. */
function passedWith(blockId: string, observed: readonly string[]): CheckBlock {
  return {
    ...checkThat(blockId, true),
    results: observed.map((words, place) =>
      result({
        check_block_id: blockId,
        created_at: `2026-11-14T01:0${place}:00Z`,
        id: `result-${blockId}-${place}`,
        observed: words,
      })
    ),
  };
}

/** The words of each result the row of the check `id` draws, in the order drawn. */
function resultsDrawn(container: HTMLElement, id: string): (string | null)[] {
  const row = rowOf(container, id);
  return [...(row?.querySelectorAll("[data-verification-result] [data-node-words]") ?? [])].map(
    (words) => words.textContent
  );
}

// With no words typed, a check draws its latest result alone while its line of earlier results
// is shut, where a chip narrows the view too: the body reads its rows from the view as the chip
// leaves it, and that reading keeps no result of a check, as the view with no words keeps none.
test("the Verification view with no words draws no earlier result where a chip narrows it", async () => {
  const user = userEvent.setup();
  const twice = passedWith("c-one", ["older pass", "newer pass"]);
  const root = node(KEY, "issue", [
    statementNode("r1", "The page draws the checks.", [checkNode(twice)]),
  ]);
  const view = drawVerification(root, [twice]);
  try {
    expect(resultsDrawn(view.container, checkNode(twice).id)).toEqual(["newer pass"]);
    const met = view.container.querySelector<HTMLElement>('[data-verification-chip="met"]');
    await user.click(met as HTMLElement);
    expect(resultsDrawn(view.container, checkNode(twice).id)).toEqual(["newer pass"]);
  } finally {
    view.unmount();
  }
});

// A chip pressed where its count leaves out every statement of the issue - here its one
// statement, proposed - draws none of them, and the table says that no statement stands in that
// state, as it says where the issue holds none in it, and not that nothing holds the words.
test("the Verification view's chip says no statement stands in its state where it leaves out every one", async () => {
  const user = userEvent.setup();
  const passed = checkThat("c-one", true);
  const proposed = statementNode("r1", "This one is proposed.", [checkNode(passed)]);
  const view = drawVerification(
    stamped(node(KEY, "issue", [proposed]), { [proposed.id]: { life: "proposed" } }),
    [passed]
  );
  try {
    const met = view.container.querySelector<HTMLElement>('[data-verification-chip="met"]');
    await user.click(met as HTMLElement);
    expect(view.container.querySelectorAll("[data-verification-card]")).toHaveLength(0);
    expect(view.container.querySelector("[data-verification-none]")?.textContent).toBe(
      EMPTY_STATE_LINES.verification_none
    );
  } finally {
    view.unmount();
  }
});

// Words that keep one earlier result of an overtaken check that serves no statement draw that
// check, a row of the table's own marked Overtaken, with its latest result and the one the words
// kept, and leave its other earlier results folded behind their line, as every check the words
// keep a result of does.
test("the Verification view draws only the results the words keep of an overtaken check that serves no statement", () => {
  const passed = checkThat("c-one", true);
  const seen = passedWith("c-two", ["zebrafinch seen", "nothing seen", "nothing seen again"]);
  const seenId = checkNode(seen).id;
  const root = stamped(
    node(KEY, "issue", [
      statementNode("r1", "The page draws the checks.", [checkNode(passed)]),
      checkNode(seen),
    ]),
    { [seenId]: { life: "overtaken" } }
  );
  const view = drawVerification(root, [passed, seen], "zebrafinch");
  try {
    expect(marksIn(rowOf(view.container, seenId))).toEqual(["overtaken"]);
    expect(resultsDrawn(view.container, seenId)).toEqual(["nothing seen again", "zebrafinch seen"]);
  } finally {
    view.unmount();
  }
});

/** The Spec view of `root` as its slot draws it (`slots/view-spec.tsx`): under the words the
 *  page's address carries (`?q=`), so a case that moves the address draws the view again. */
function SpecAtAddress({
  context,
  issue,
  request,
  reveal,
  root,
}: {
  context: ReadonlySet<string>;
  issue: IssueDetails;
  request?: OutlineRequest;
  reveal?: string;
  root: TreeNode;
}): ReactNode {
  const [search] = useSearchParams();
  const query = search.get("q") ?? "";
  const view = useMemo(() => presetView(root, "spec", query, new Set()).root, [query, root]);
  return createElement(SpecFilterBody, {
    context,
    emptyLine: query === "" ? EMPTY_STATE_LINES.spec : EMPTY_STATE_LINES.no_match,
    holdsEscape: false,
    issue,
    reader: "alice",
    ...(request === undefined ? {} : { request }),
    ...(reveal === undefined ? {} : { reveal }),
    root,
    view,
  });
}

/** Hands the page's own `navigate` to `take`, so a case moves the address as the reader does. */
function NavigateProbe({ take }: { take: (navigate: (to: string) => void) => void }): ReactNode {
  const navigate = useNavigate();
  take(navigate);
  return null;
}

/** The Spec view over `root`, its reads answered with no decision, no question, no result and no
 *  block, drawn under the providers the page draws it under (`SpecFilterBody.drawn.test.tsx`'s
 *  `drawBody`), on an address that carries the words `query`; with `asksOut`, the asks read not
 *  answered yet. `go` moves the address, `again` draws the view over another tree, as the page
 *  does once a read it holds on the tree answers, and `unmount` puts the reads back as well. */
function drawSpec(
  root: TreeNode,
  {
    asksOut = false,
    query = "",
    request,
    reveal,
  }: { asksOut?: boolean; query?: string; request?: OutlineRequest; reveal?: string } = {}
): {
  again: (root: TreeNode) => void;
  container: HTMLElement;
  go: (to: string) => void;
  unmount: () => void;
} {
  const issue = issueDetails();
  const results: IssueCheckResults = { checks: [], issue: KEY, verification_artifact_id: "" };
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(issueResultsQuery(KEY).queryKey, results);
  client.setQueryData(issueCoverageQuery(KEY).queryKey, coverageRead([]));
  client.setQueryData(artifactBlocksQuery(issue.primary_artifact_id).queryKey, []);
  // Conversation's read of the tree and the Spec view's read, held answered with no node, as the
  // page holds them once read: the body takes its decisions from the view read and reads nothing
  // on its own.
  client.setQueryData(conversationTreeKey(KEY), readAnswer([]));
  client.setQueryData(viewKey(KEY, "spec"), readAnswer([]));
  // The asks read, held answered with no question unless the case keeps it out: the body says
  // where `q` and `w` go nowhere only once every read it draws from holds an answer
  // (`readsHeld`).
  if (!asksOut) client.setQueryData(["asks", KEY], []);
  const spies = [
    spyOn(api, "getArtifactBlocks").mockResolvedValue([]),
    spyOn(api, "getIssueCoverage").mockResolvedValue(coverageRead([])),
    spyOn(api, "getIssueResults").mockResolvedValue(results),
  ];
  // The page's `navigate`, once the address is drawn.
  let moveTo: (to: string) => void = () => {};
  // The view over `over`, under the providers the page draws it under.
  const page = (over: TreeNode): ReactNode =>
    createElement(
      QueryClientProvider,
      { client },
      createElement(
        ReferenceNamesProvider,
        null,
        createElement(
          MemoryRouter,
          { initialEntries: [query === "" ? "/" : `/?q=${query}`] },
          createElement(NavigateProbe, {
            take: (navigate) => {
              moveTo = navigate;
            },
          }),
          createElement(
            KeymapProvider,
            null,
            createElement(
              MarginProvider,
              null,
              createElement(
                OutlineRequestProvider,
                null,
                createElement(
                  FocusedNodeProvider,
                  null,
                  createElement(IssueScope),
                  createElement(SpecAtAddress, {
                    context: new Set<string>(),
                    issue,
                    ...(request === undefined ? {} : { request }),
                    ...(reveal === undefined ? {} : { reveal }),
                    root: over,
                  })
                )
              )
            )
          )
        )
      )
    );
  const view = render(page(root));
  return {
    again: (over) => view.rerender(page(over)),
    container: view.container,
    go: (to) => act(() => moveTo(to)),
    unmount: () => {
      view.unmount();
      for (const spy of spies) spy.mockRestore();
    },
  };
}

// A proposed statement draws no gap, so under Spec it carries no mark of No check though no
// check serves it, and it carries its own mark, Proposed, instead. A live statement no check
// serves still carries No check, and no mark of a life.
test("the Spec view draws no No check on a proposed statement and marks it", () => {
  const view = drawSpec(
    stamped(
      node(KEY, "issue", [
        statement("r1", { counts: { checks: 0, planTasks: 0 }, gaps: ["no_check"] }),
        statement("r2", { counts: { checks: 0, planTasks: 0 }, gaps: ["no_check"] }),
      ]),
      { "spec#b-r1": { life: "proposed" } }
    )
  );
  try {
    const noCheck = `[data-glyph="${GLYPH_TOKENS.no_check}"]`;
    const proposed = rowOf(view.container, "spec#b-r1");
    expect(proposed).not.toBeNull();
    expect(proposed).not.toHaveAttribute("data-spec-mark");
    expect(proposed?.querySelector(noCheck)).toBeNull();
    expect(proposed?.querySelector('[data-node-mark="proposed"]')?.textContent).toContain(
      ROW_MARK_WORDS.proposed
    );
    const live = rowOf(view.container, "spec#b-r2");
    expect(live).toHaveAttribute("data-spec-mark", "no_check");
    expect(live?.querySelector(noCheck)).not.toBeNull();
    expect(live?.querySelector("[data-node-mark]")).toBeNull();
  } finally {
    view.unmount();
  }
});

// Contents lists an overtaken question in tree order, but neither it nor the entry above it
// counts work under an overtaken node.
test("an overtaken open question is a Contents entry with no branch counts", () => {
  const countedStatement = (block: string) => specStatement(block, { statement: true });
  const root = stamped(
    node(KEY, "issue", [
      node("ask-1", "ask", [
        countedStatement("live"),
        openAsk("ask-2", true, { children: [countedStatement("old")] }),
        node("old-thread", "comment", [countedStatement("under-thread")]),
        node("ask-3", "ask"),
      ]),
      node("ask-4", "ask"),
    ]),
    { "ask-2": { life: "overtaken" }, "old-thread": { life: "overtaken" } }
  );
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const view = render(
    createElement(
      QueryClientProvider,
      { client },
      createElement(ContentsList, {
        focusBranch: false,
        nameOf: () => undefined,
        onFocusBranch: () => {},
        onGo: () => {},
        root,
      })
    )
  );
  try {
    const entries = [...view.container.querySelectorAll("[data-contents-entry]")].map((entry) =>
      entry.getAttribute("data-contents-entry")
    );
    expect(entries).toEqual(["ask-1", "ask-2", "ask-3", "ask-4"]);
    expect(contentsEntries(root, undefined)).toBe(entries.length);
    expect(
      view.container.querySelector('[data-contents-entry="ask-2"] [data-contents-mark]')
    ).toBeNull();
    expect(
      textOf(
        view.container,
        '[data-contents-entry="ask-1"] [data-contents-mark="statements"] [data-author-text]'
      )
    ).toBe("1");
  } finally {
    view.unmount();
  }
});

// A Contents entry's failing mark counts a statement for a failed check that a count reads: a
// failed check that is overtaken, or proposed, marks nothing, as the Verification view and the
// failing flag leave it out, while a live failed check beside them still marks its statement.
test("a Contents entry's failing mark leaves out a failed check no count reads", () => {
  const served = (block: string) =>
    node(
      `spec#b-r${block}`,
      "requirement",
      [node(`verification-md#b-c${block}`, "check", [], { state: "failed" })],
      { statement: true }
    );
  const root = stamped(node(KEY, "issue", [node("ask-1", "ask", ["1", "2", "3"].map(served))]), {
    "verification-md#b-c1": { life: "overtaken" },
    "verification-md#b-c2": { life: "proposed" },
  });
  expect(contentsOf(root, undefined).map((entry) => entry.marks)).toEqual([
    { children: 0, failing: 1, for_review: 0, statements: 3 },
  ]);
});

// A proposed statement is no failing one of a Contents entry, though a live check under it
// failed, as the failing flag and the Verification summary leave that check out; it is still one
// of the entry's statements. A live statement whose check failed still marks the entry failing.
test("a Contents entry's failing mark leaves out a proposed statement whose check failed", () => {
  const served = (block: string) =>
    node(
      `spec#b-r${block}`,
      "requirement",
      [node(`verification-md#b-c${block}`, "check", [], { state: "failed" })],
      { statement: true }
    );
  const root = stamped(node(KEY, "issue", [node("ask-1", "ask", ["1", "2"].map(served))]), {
    "spec#b-r1": { life: "proposed" },
  });
  expect(contentsOf(root, undefined).map((entry) => entry.marks)).toEqual([
    { children: 0, failing: 1, for_review: 0, statements: 2 },
  ]);
});

// Under Spec an overtaken statement is a row in its group at its place, between the statements
// beside it, carrying Overtaken and its shape, and no gap: no mark of how it stands, "No check"
// included, though no check a count reads serves it. The Spec control counts it as it counts
// every statement, and the failing flag does not count the failed check under it.
test("under Spec an overtaken statement is a row in its group, marked, with no gap", () => {
  const unserved = { counts: { checks: 0, planTasks: 0 }, gaps: ["no_check" as const] };
  const root = stamped(
    node(KEY, "issue", [
      statement("r1", unserved),
      statement("r2", {
        ...unserved,
        children: [node("verification-md#b-c2", "check", [], { state: "failed" })],
      }),
      statement("r3", unserved),
    ]),
    { "spec#b-r2": { life: "overtaken" } }
  );
  const view = drawSpec(root);
  try {
    expect(rowIds(view.container)).toEqual(["spec#b-r1", "spec#b-r2", "spec#b-r3"]);
    expect(view.container.querySelectorAll("[data-spec-group]")).toHaveLength(1);
    const overtaken = rowOf(view.container, "spec#b-r2");
    expect(marksIn(overtaken)).toEqual(["overtaken"]);
    expect(overtaken?.querySelector('[data-node-mark="overtaken"]')?.textContent).toContain(
      ROW_MARK_WORDS.overtaken
    );
    expect(
      overtaken?.querySelector(`[data-glyph="${GLYPH_TOKENS.marks.overtaken}"]`)
    ).not.toBeNull();
    expect(overtaken).not.toHaveAttribute("data-spec-mark");
    expect(overtaken?.querySelector(`[data-glyph="${GLYPH_TOKENS.no_check}"]`)).toBeNull();
    expect(overtaken?.querySelector("[data-gap]")).toBeNull();
    expect(rowOf(view.container, "spec#b-r1")).toHaveAttribute("data-spec-mark", "no_check");
  } finally {
    view.unmount();
  }
  expect(filterSignals(root, "").spec).toEqual({ count: 3 });
  expect(filterSignals(root, "").verification.flag).toBeUndefined();
});

// Under Spec the words filter matches an overtaken statement's words and draws it as it draws
// every match: a row in its group at its place, after the live match, marked Overtaken.
test("the Spec view's words filter draws an overtaken statement it matches as a row, marked", async () => {
  const view = drawSpec(
    stamped(
      node(KEY, "issue", [
        statement("r1", { text: "**R1.** The zebrafinch is drawn first." }),
        statement("r2", { text: "**R2.** The zebrafinch is drawn second." }),
        statement("r3"),
      ]),
      { "spec#b-r2": { life: "overtaken" } }
    ),
    { query: "zebrafinch" }
  );
  try {
    await waitFor(() => expect(rowIds(view.container)).toEqual(["spec#b-r1", "spec#b-r2"]));
    expect(view.container.querySelectorAll("[data-spec-group]")).toHaveLength(1);
    expect(marksIn(rowOf(view.container, "spec#b-r2"))).toEqual(["overtaken"]);
  } finally {
    view.unmount();
  }
});

// An address that names an overtaken statement under Spec focuses its row, at its place between
// the statements beside it, and so does a request from the Contents list or a chip.
test("an address or a request naming an overtaken statement under Spec focuses its row", async () => {
  const places = [
    { reveal: "spec#b-r2" },
    { request: { focusBranch: false, id: "spec#b-r2", seq: 1 } },
  ] as const;
  for (const place of places) {
    const view = drawSpec(
      stamped(node(KEY, "issue", [statement("r1"), statement("r2"), statement("r3")]), {
        "spec#b-r2": { life: "overtaken" },
      }),
      place
    );
    try {
      await waitFor(() => expect(document.activeElement).toBe(rowOf(view.container, "spec#b-r2")));
      expect(rowIds(view.container)).toEqual(["spec#b-r1", "spec#b-r2", "spec#b-r3"]);
    } finally {
      view.unmount();
    }
  }
});

/** The decision cell the Spec view draws beside the run that holds the statement `id`. */
function cellBeside(container: HTMLElement, id: string): HTMLElement {
  const cell = container.querySelector<HTMLElement>(
    `[data-spec-cell][data-spec-statements~="${CSS.escape(id)}"]`
  );
  if (cell === null) throw new Error(`no cell is drawn beside ${id}`);
  return cell;
}

// Under Spec a decision a later one overtook still heads the run of the statement it shaped: its
// cell draws it, carrying Overtaken in its compact form after how it stands, and so does the line
// under the statement that names it. With the decision live neither carries a mark.
test("a Spec decision cell naming an overtaken decision draws it marked", () => {
  const D1 = "decisions-md#b-d1";
  const rootWith = (states: Readonly<Record<string, NodeStates>>): TreeNode => ({
    ...stamped(specIssueTree([statement("r1"), specDecisionNode("d1")]), states),
    views: {
      spec: specViewAnswer([
        answerNode("spec#b-r1", "requirement", { role: "query" }),
        specDecision("d1", { decision: { links: [specShaped(specStatementRef("r1"))] } }),
      ]),
    },
  });
  const overtaken = drawSpec(rootWith({ [D1]: { life: "overtaken" } }));
  try {
    const cell = cellBeside(overtaken.container, "spec#b-r1");
    expect(cell).toHaveAttribute("data-decision-id", D1);
    expect(marksIn(cell)).toEqual(["overtaken"]);
    expect(cell.querySelector('[data-node-mark="overtaken"]')?.textContent).toContain(
      ROW_MARK_WORDS.overtaken
    );
    expect(
      marksIn(rowOf(overtaken.container, "spec#b-r1")?.querySelector("[data-spec-because]"))
    ).toEqual(["overtaken"]);
  } finally {
    overtaken.unmount();
  }

  const live = drawSpec(rootWith({}));
  try {
    const cell = cellBeside(live.container, "spec#b-r1");
    expect(cell).toHaveAttribute("data-decision-id", D1);
    expect(marksIn(cell)).toEqual([]);
    expect(
      marksIn(rowOf(live.container, "spec#b-r1")?.querySelector("[data-spec-because]"))
    ).toEqual([]);
  } finally {
    live.unmount();
  }
});

/** Three statements under the issue, before and after the Spec view read is held on it
 *  (`TreeNode.views`): `tree` holds no view read, so the view is read off the tree, no decision
 *  is held and the three stand in one run; `decided` holds the read, which takes the three and
 *  an agent's decision that shaped the second, which then stands in a run of its own between the
 *  other two. Each node is read by its own id, as the read names it. */
function secondShaped(): { decided: TreeNode; tree: TreeNode } {
  const blocks = ["r1", "r2", "r3"];
  const statements = blocks.map((block) => statement(block));
  const tree = readAsIds(specIssueTree([...statements, specDecisionNode("d1")]));
  return {
    decided: {
      ...tree,
      views: {
        spec: specViewAnswer([
          ...blocks.map((block) => answerNode(`spec#b-${block}`, "requirement", { role: "query" })),
          specDecision("d1", { decision: { links: [specShaped(specStatementRef("r2"))] } }),
        ]),
      },
    },
    tree,
  };
}

/** The ids of the statements of each run the Spec view draws, run by run. */
function runsIn(container: HTMLElement): (string | undefined)[][] {
  return [...container.querySelectorAll<HTMLElement>("[data-spec-run]")].map((run) => rowIds(run));
}

// An address naming a statement puts the focus on its row while the Spec view read is not held
// yet, and every statement stands in one run. The decisions arriving split that run, and the
// statement a decision shaped is drawn anew in a run of its own: the focus the address put on it
// stays on its row there, since the reader did not move it.
test("the focus an address put on a statement stays on its row when a decision moves it to a run of its own", async () => {
  const { decided, tree } = secondShaped();
  const view = drawSpec(tree, { reveal: "spec#b-r2" });
  try {
    await waitFor(() => expect(document.activeElement).toBe(rowOf(view.container, "spec#b-r2")));
    expect(runsIn(view.container)).toEqual([["spec#b-r1", "spec#b-r2", "spec#b-r3"]]);
    const landed = rowOf(view.container, "spec#b-r2");
    view.again(decided);
    expect(runsIn(view.container)).toEqual([["spec#b-r1"], ["spec#b-r2"], ["spec#b-r3"]]);
    expect(landed?.isConnected, "the statement's row was not drawn anew").toBe(false);
    expect(document.activeElement).toBe(rowOf(view.container, "spec#b-r2"));
  } finally {
    view.unmount();
  }
});

// A reader who moved the focus out of the Spec view keeps it where they put it when the
// decisions arriving draw the statement they left anew in another run.
test("a focus the reader moved out of the Spec view stays there when its runs regroup", async () => {
  const { decided, tree } = secondShaped();
  const elsewhere = document.createElement("button");
  document.body.append(elsewhere);
  const view = drawSpec(tree, { reveal: "spec#b-r2" });
  try {
    await waitFor(() => expect(document.activeElement).toBe(rowOf(view.container, "spec#b-r2")));
    act(() => elsewhere.focus());
    view.again(decided);
    expect(runsIn(view.container)).toEqual([["spec#b-r1"], ["spec#b-r2"], ["spec#b-r3"]]);
    expect(document.activeElement).toBe(elsewhere);
  } finally {
    view.unmount();
    elsewhere.remove();
  }
});

// The tree read's own `state` and `visibility` are a node's marks where they are a
// life or the author's alone, and nothing else is one: a live, settled or gone node and one the
// read gives no value for carry none.
test("statesOfRead marks a proposed or overtaken state and the author's visibility, and no other", () => {
  expect(statesOfRead({ state: "proposed" })).toStrictEqual({ life: "proposed" });
  expect(statesOfRead({ state: "overtaken" })).toStrictEqual({ life: "overtaken" });
  for (const state of ["settled", "live", "gone", null, undefined]) {
    expect(statesOfRead({ state }), String(state)).toStrictEqual({});
  }
  expect(statesOfRead({ visibility: "author" })).toStrictEqual({ onlyYou: true });
  for (const visibility of ["project", null, undefined]) {
    expect(statesOfRead({ visibility }), String(visibility)).toStrictEqual({});
  }
  expect(statesOfRead({ state: "proposed", visibility: "author" })).toStrictEqual({
    life: "proposed",
    onlyYou: true,
  });
  expect(statesOfRead({ state: "live", visibility: "project" })).toStrictEqual({});
  expect(statesOfRead({})).toStrictEqual({});
});

/** Four lenses of the issue's node store, by their addresses. */
const TRADE_OFF = "dispatch://lens/0c6f1d2e-1111-4a6b-9c1d-5e2f3a4b5c61";
const PREMORTEM = "dispatch://lens/0c6f1d2e-2222-4a6b-9c1d-5e2f3a4b5c62";
const ELSEWHERE = "dispatch://lens/0c6f1d2e-6666-4a6b-9c1d-5e2f3a4b5c66";
const BEYOND = "dispatch://lens/0c6f1d2e-7777-4a6b-9c1d-5e2f3a4b5c67";

/** `chip` drawn by `NodeLink` on this issue's route, so a chip with an id is a link. It is drawn
 *  under a query client whose failed read is not retried: a chip's words are drawn through the
 *  plain-text projection, which reads through react-query. */
function drawChip(chip: LensChip) {
  return renderWithQueries(
    createElement(
      MemoryRouter,
      { initialEntries: [`/issues/${KEY}`] },
      createElement(
        Routes,
        null,
        createElement(Route, {
          element: createElement(NodeLink, { chip, tabStops: false }),
          path: "/issues/:key/*",
        })
      )
    )
  );
}

/** What a chip draws inside `[data-node-chip]`, element by element: its words, then each mark. */
function chipDrawn(container: HTMLElement): (string | null)[] {
  const drawn = container.querySelector("[data-node-chip]");
  if (drawn === null) throw new Error("no chip is drawn");
  return [...drawn.children].map((child) =>
    child.hasAttribute("data-chip-name") ? "name" : child.getAttribute("data-node-mark")
  );
}

// A chip of a lens line carries the marks of the node it names, from the state and
// the visibility its tree read gives, after the chip's words - a link or the words alone - in
// the chip's own form; a live node every reader may see, or a chip given neither, carries none.
test("a lens chip carries the marks its tree read gives", () => {
  const statement = `dispatch://${KEY}/spec#b-r7`;
  const cases: readonly [LensChip, readonly (string | null)[]][] = [
    [
      {
        address: statement,
        id: "spec#b-r7",
        state: "proposed",
        visibility: "project",
        words: "R7",
      },
      ["name", "proposed"],
    ],
    [
      { address: TRADE_OFF, state: "live", visibility: "author", words: "Trade-off analysis" },
      ["name", "only_you"],
    ],
    [
      {
        address: statement,
        id: "spec#b-r7",
        state: "overtaken",
        visibility: "author",
        words: "R7",
      },
      ["name", "overtaken", "only_you"],
    ],
    [{ address: statement, state: "live", visibility: "project", words: "R7" }, ["name"]],
    [{ address: TRADE_OFF, words: "Trade-off analysis" }, ["name"]],
  ];
  for (const [chip, drawn] of cases) {
    const view = drawChip(chip);
    try {
      expect(chipDrawn(view.container), JSON.stringify(chip)).toEqual([...drawn]);
      const name = view.container.querySelector("[data-chip-name]");
      expect(name?.tagName, "a chip with an id is a link").toBe(
        chip.id === undefined ? "SPAN" : "A"
      );
      expect(name?.querySelector("[data-node-mark]") ?? null).toBeNull();
    } finally {
      view.unmount();
    }
  }

  const proposed = drawChip({ address: statement, state: "proposed", words: "R7" });
  try {
    const mark = proposed.container.querySelector('[data-node-mark="proposed"]');
    expect(mark?.textContent).toBe(ROW_MARK_WORDS.proposed);
    expect(mark?.querySelector("svg")).toHaveAttribute("data-glyph", GLYPH_TOKENS.marks.proposed);
  } finally {
    proposed.unmount();
  }
});

/** One lens as the lens read answers it, in the state and the visibility that read gives it. */
function lensRead(
  address: string,
  words: string,
  state: LensRead["state"],
  visibility: LensRead["visibility"]
): LensRead {
  return {
    address,
    author: { id: "alice", kind: "person" },
    created_at: "2026-11-16T09:00:00Z",
    findings: [],
    kind: { id: `kind-${address.slice(-2)}`, words },
    on: `dispatch://${KEY}/spec#b-r1`,
    risks: [],
    state,
    triggered: [],
    triggered_by: [],
    verdict: null,
    visibility,
  };
}

/** The lens read of the pre-mortem: its one chain runs from the trade-off analysis, at an
 *  address pinned to a version, through the pre-mortem to a lens the tree holds no node for.
 *  The read says the pre-mortem is proposed and the last lens overtaken, each reserved for its
 *  author, and the trade-off analysis live for every reader: none of it is what the tree says. */
function premortemChain(): NodeLenses {
  return {
    chains: [
      {
        lenses: [
          lensRead(`${TRADE_OFF}@v2`, "trade-off analysis", "live", "project"),
          lensRead(PREMORTEM, "pre-mortem", "proposed", "author"),
          lensRead(ELSEWHERE, "second opinion", "overtaken", "author"),
        ],
        stopped_at: null,
      },
    ],
    lens: lensRead(PREMORTEM, "pre-mortem", "proposed", "author"),
    lenses: [],
    on: { address: PREMORTEM, kind: "lens", state: "live", visibility: "project" },
  };
}

/** The pre-mortem's facts: triggered by the trade-off analysis, so its row draws the chain's
 *  control. */
const PREMORTEM_FACTS: LensFacts = {
  constrainedBy: [],
  kind: "pre-mortem",
  preventedBy: [],
  triggered: [],
  triggeredBy: [{ address: TRADE_OFF, words: "Trade-off analysis" }],
};

/** The issue's tree: the pre-mortem, live and the project's, first, and the trade-off analysis,
 *  which the tree read says is proposed and reserved for its author, the reader. */
function lensTree(): TreeNode {
  return stamped(
    node(KEY, "issue", [
      node("lens/2222", "lens", [], {
        lens: PREMORTEM_FACTS,
        reference: PREMORTEM,
        text: "Ship it behind a flag",
      }),
      node("lens/1111", "lens", [], { reference: TRADE_OFF, text: "Weigh the flag" }),
    ]),
    { "lens/1111": { life: "proposed", onlyYou: true } }
  );
}

/** The pre-mortem's chain drawn alone, under the tree's marks by reference where `states` is
 *  given and under no provider where it is not. */
function drawChain(states: ReadonlyMap<string, NodeStates> | undefined) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const lens = pathTo(lensTree(), "lens/2222")?.at(-1);
  if (lens === undefined) throw new Error("the tree holds no pre-mortem");
  const chain = createElement(LensChain, { node: lens, tabStops: false });
  return render(
    createElement(
      QueryClientProvider,
      { client },
      createElement(
        MemoryRouter,
        null,
        states === undefined ? chain : createElement(NodeStatesProvider, { value: states }, chain)
      )
    )
  );
}

/** Each entry of the drawn chain by its lens's address, element by element: its kind's words,
 *  then each mark, then the line it stands under. */
function chainDrawn(container: HTMLElement): Record<string, (string | null)[]> {
  return Object.fromEntries(
    [...container.querySelectorAll("[data-lens-chain-entry]")].map((entry) => [
      entry.getAttribute("data-lens-address") ?? "",
      [...entry.children].map((child) =>
        child.hasAttribute("data-lens-chain-kind")
          ? "kind"
          : child.hasAttribute("data-lens-chain-line")
            ? "line"
            : child.getAttribute("data-node-mark")
      ),
    ])
  );
}

/** Presses the chain's control and waits for its three entries. */
async function showChain(container: HTMLElement): Promise<void> {
  const user = userEvent.setup();
  const control = await waitFor(() => {
    const found = container.querySelector<HTMLButtonElement>("[data-lens-show-chain]");
    if (found === null) throw new Error("the pre-mortem draws no control for its chain");
    return found;
  });
  await user.click(control);
  await waitFor(() =>
    expect(container.querySelectorAll("[data-lens-chain-entry]")).toHaveLength(3)
  );
}

// An entry of a lens's chain carries the marks of the tree's node at its lens's
// address, whatever version the address pins, after the lens's name; an entry the tree holds no
// node for carries none, and so does every entry outside the outline's rows. The lens read's
// own state and visibility mark nothing. The outline hands its tree's marks to its rows itself.
test("a chain entry carries the marks of the tree's node at its address", async () => {
  const read = spyOn(api, "getLenses").mockResolvedValue(premortemChain());
  try {
    const marked = {
      [`${TRADE_OFF}@v2`]: ["kind", "proposed", "only_you"],
      [PREMORTEM]: ["kind"],
      [ELSEWHERE]: ["kind"],
    };
    const provided = drawChain(statesByReference(lensTree()));
    try {
      await showChain(provided.container);
      expect(chainDrawn(provided.container)).toEqual(marked);
    } finally {
      provided.unmount();
    }

    const alone = drawChain(undefined);
    try {
      await showChain(alone.container);
      expect(chainDrawn(alone.container)).toEqual({
        [`${TRADE_OFF}@v2`]: ["kind"],
        [PREMORTEM]: ["kind"],
        [ELSEWHERE]: ["kind"],
      });
    } finally {
      alone.unmount();
    }

    readWide();
    const outline = renderTree(lensTree());
    try {
      await waitFor(() => expect(focusedRow(outline.container)).toBe("lens/2222"));
      await showChain(outline.container);
      expect(chainDrawn(outline.container)).toEqual(marked);
    } finally {
      outline.unmount();
    }
  } finally {
    read.mockRestore();
  }
});

/** The pre-mortem's chain as `premortemChain` reads it, cut short at the lens `BEYOND`, which
 *  the read names by its address alone. */
function stoppedChain(): NodeLenses {
  const read = premortemChain();
  return {
    ...read,
    chains: (read.chains ?? []).map((chain) => ({ ...chain, stopped_at: BEYOND })),
  };
}

/** The marks of the tree's nodes by reference, from a tree that holds the lens `BEYOND` with
 *  `marks`, as the tree read gives them, beside the pre-mortem. */
function statesWithBeyond(marks: NodeStates): ReadonlyMap<string, NodeStates> {
  const beyond = node("lens/7777", "lens", [], { reference: BEYOND, text: "Look further" });
  return statesByReference(
    stamped(node(KEY, "issue", [...lensTree().children, beyond]), { "lens/7777": marks })
  );
}

/** What the chip of the lens the chain stopped at draws, element by element. */
function stoppedDrawn(container: HTMLElement): (string | null)[] {
  const more = container.querySelector<HTMLElement>("[data-lens-chain-more]");
  if (more === null) throw new Error("the chain draws no lens it stopped at");
  return chipDrawn(more);
}

// The chip of the lens a cut-short chain stopped at names that lens by its address
// alone, with no state of its own, so it carries the marks of the tree's node at that address,
// after its words; outside the outline's rows it carries none.
test("the lens a cut-short chain stopped at carries the tree's marks", async () => {
  const read = spyOn(api, "getLenses").mockResolvedValue(stoppedChain());
  try {
    const cases: readonly [NodeStates | undefined, readonly (string | null)[]][] = [
      [{ life: "proposed" }, ["name", "proposed"]],
      [{ onlyYou: true }, ["name", "only_you"]],
      [undefined, ["name"]],
    ];
    for (const [marks, drawn] of cases) {
      const view = drawChain(marks === undefined ? undefined : statesWithBeyond(marks));
      try {
        await showChain(view.container);
        expect(stoppedDrawn(view.container), JSON.stringify(marks)).toEqual([...drawn]);
      } finally {
        view.unmount();
      }
    }
  } finally {
    read.mockRestore();
  }
});

/** What a case gives a node of the tree read beside its name and supertag: any member of the
 *  generated node `treeNode` takes, and its parent, named as `treeNode` names one - under the
 *  issue unless it names another. */
type ReadExtra = TreeNodeExtra & { readonly parent?: string | IssueTreeParent | null };

/** One node of the tree read's answer as the route sends it (`treeNode`): live and visible
 *  to every reader unless the case says otherwise, under the issue unless it names another
 *  parent, with words of its own. */
function readNode(
  id: string,
  supertag: string,
  { parent = KEY, ...extra }: ReadExtra = {}
): TreeAnswerNode {
  return treeNode(id, supertag, parent, { text: `Words of ${id}.`, ...extra });
}

/** The ask `id` as a view's answer names it as a node's parent, with `readNode`'s words. */
function askLine(id: string): IssueTreeParent {
  return { first_line: `Words of ${id}.`, id, kind: "ask", supertag: "ask" };
}

/** The read's own id of the block `id` of the issue's document `slug`, as the route names a
 *  typed block (`<KEY>/<slug>#<block id>`). */
function blockReadId(slug: string, id: string): string {
  return `${KEY}/${slug}#${id}`;
}

/** A typed block of the issue's document `slug`, at `position` in it, which the tree draws by a
 *  tree id (`<slug>#b-<id>`) that is not its read id (`blockReadId`). */
function readBlock(
  id: string,
  supertag: string,
  slug: string,
  position: number,
  extra: ReadExtra = {}
): TreeAnswerNode {
  return readNode(id, supertag, {
    document: { artifact_id: `artifact-${slug}`, position, slug },
    ...extra,
  });
}

/** The issue's own node, which every node of a tree read hangs from. */
const ISSUE_NODE = readNode(KEY, "issue", { parent: null });

/** One answer of the tree read, or of one of its views, every page merged, each parent a node
 *  names by name named as the route names it (`treeAnswer`). */
function readAnswer(nodes: readonly TreeAnswerNode[]): TreeAnswer {
  return treeAnswer(nodes, [], { converted: true, tree: "artifact-tree" });
}

/** The issue the tree read is of, as `issueQuery` loads it, with the Verification document the
 *  `check` blocks here are blocks of, so each is a check (`isCheck`). */
const ISSUE = {
  ...issueDetails(),
  artifacts: [...issueDetails().artifacts, artifact("artifact-verification-md", "verification-md")],
  verification_artifact_id: "artifact-verification-md",
};

/** The outline `buildIssueTree` builds from the tree read's answer of `nodes`, under the issue's
 *  own node, and from no other read. */
function outlineOf(nodes: readonly TreeAnswerNode[]): TreeNode {
  return buildIssueTree({ issue: ISSUE, tree: readAnswer([ISSUE_NODE, ...nodes]) });
}

/** The first placement under `root` of the node the tree read gives `readId`. */
function readNodeOf(root: TreeNode, readId: string): TreeNode | undefined {
  if (root.readId === readId) return root;
  for (const child of root.children) {
    const found = readNodeOf(child, readId);
    if (found !== undefined) return found;
  }
  return undefined;
}

/** Which of the marks and of `closed` `found` carries a key for. */
function markKeys(found: TreeNode | undefined): string[] {
  return ["closed", "life", "onlyYou"].filter((key) => key in (found ?? {}));
}

/** Each kind the outline draws a row of from the tree read, with the document it is a typed
 *  block of where it is one. */
const READ_KINDS: readonly (readonly [NodeType, string | undefined])[] = [
  ["requirement", "spec"],
  ["check", "verification-md"],
  ["plan_task", "plan-md"],
  ["decision", "decisions-md"],
  ["callout", "spec"],
  ["ask", undefined],
  ["comment", undefined],
  ["message", undefined],
];

/** One node of each kind of `READ_KINDS` for each of `values`, under the issue: the node of the
 *  kind `kind` given the n-th values is named `<kind>-<n>`, and read by the id `kindReadId`
 *  gives it. */
function eachKind(values: readonly Partial<TreeAnswerNode>[]): TreeAnswerNode[] {
  return READ_KINDS.flatMap(([kind, slug], k) =>
    values.map((extra, n) =>
      slug === undefined
        ? readNode(`${kind}-${n}`, kind, extra)
        : readBlock(`${kind}-${n}`, kind, slug, k * values.length + n, extra)
    )
  );
}

/** The read's own id of the node of kind `kind` that `eachKind` gives the n-th values: a typed
 *  block's id in its document (`blockReadId`), else its name. */
function kindReadId(kind: NodeType, n: number): string {
  const slug = READ_KINDS.find(([one]) => one === kind)?.[1];
  return slug === undefined ? `${kind}-${n}` : blockReadId(slug, `${kind}-${n}`);
}

// A node of every kind the outline draws takes its life from the `state` the tree read
// answers for it, by the read's own id, which a typed block's tree id is not: proposed or
// overtaken, an overtaken one closed and a proposed one not. A live node and a settled one
// carry no mark at all.
test("a node of any kind the tree read answers proposed or overtaken has that life", () => {
  const root = outlineOf(
    eachKind([
      { state: "proposed" },
      { state: "overtaken" },
      { state: "live" },
      { state: "settled" },
    ])
  );
  for (const [kind, slug] of READ_KINDS) {
    const proposed = readNodeOf(root, kindReadId(kind, 0));
    const overtaken = readNodeOf(root, kindReadId(kind, 1));
    expect(proposed?.type, kind).toBe(kind);
    if (slug !== undefined) expect(proposed?.id, kind).toBe(`${slug}#b-${kind}-0`);
    expect(markKeys(proposed), kind).toEqual(["life"]);
    expect(proposed?.life, kind).toBe("proposed");
    expect(markKeys(overtaken), kind).toEqual(["closed", "life"]);
    expect(overtaken?.life, kind).toBe("overtaken");
    for (const id of [kindReadId(kind, 2), kindReadId(kind, 3)]) {
      expect(readNodeOf(root, id)?.type, id).toBe(kind);
      expect(markKeys(readNodeOf(root, id)), id).toEqual([]);
    }
  }
});

// A node of every kind the outline draws whose `visibility` the tree read answers as
// `author` is reserved for its author, the reader, and is marked "Only you", beside "Proposed"
// where both hold; a node every reader may see carries no such mark.
test("a node of any kind whose visibility is author is marked Only you", () => {
  const root = outlineOf(
    eachKind([
      { visibility: "author" },
      { state: "proposed", visibility: "author" },
      { visibility: "project" },
    ])
  );
  for (const [kind] of READ_KINDS) {
    expect(markKeys(readNodeOf(root, kindReadId(kind, 0))), kind).toEqual(["onlyYou"]);
    expect(readNodeOf(root, kindReadId(kind, 0))?.onlyYou, kind).toBe(true);
    expect(markKeys(readNodeOf(root, kindReadId(kind, 1))), kind).toEqual(["life", "onlyYou"]);
    expect(markKeys(readNodeOf(root, kindReadId(kind, 2))), kind).toEqual([]);
  }
  const drawn = render(
    createElement(NodeMarks, { marks: readNodeOf(root, kindReadId("decision", 0)) ?? {} })
  );
  try {
    expect(
      [...drawn.container.querySelectorAll("[data-node-mark]")].map((mark) => mark.textContent)
    ).toEqual([ROW_MARK_WORDS.only_you]);
  } finally {
    drawn.unmount();
  }
});

// A view's rows carry the states the view's own answer gives them, where the tree read with
// no view answered otherwise: a statement the Spec view answers proposed is marked there though
// the outline's is live, one it answers live carries no mark though the outline's is proposed,
// and one it answers reserved for its author is marked "Only you".
test("a view's rows carry their own state", () => {
  const outline = outlineOf([
    readBlock("r1", "requirement", "spec", 0),
    readBlock("r2", "requirement", "spec", 1, { state: "proposed" }),
    readBlock("r3", "requirement", "spec", 2),
  ]);
  const spec = readAnswer([
    readBlock("r1", "requirement", "spec", 0, { role: "query", state: "proposed" }),
    readBlock("r2", "requirement", "spec", 1, { role: "query" }),
    readBlock("r3", "requirement", "spec", 2, { role: "query", visibility: "author" }),
  ]);
  expect(markKeys(readNodeOf(outline, blockReadId("spec", "r1")))).toEqual([]);
  expect(readNodeOf(outline, blockReadId("spec", "r2"))?.life).toBe("proposed");

  const view = presetView({ ...outline, views: { spec } }, "spec", "").root;
  expect(view.children.map((row) => [row.readId, markKeys(row), row.life, row.onlyYou])).toEqual([
    [blockReadId("spec", "r1"), ["life"], "proposed", undefined],
    [blockReadId("spec", "r2"), [], undefined, undefined],
    [blockReadId("spec", "r3"), ["onlyYou"], undefined, true],
  ]);
});

// The Plan view answers a plan task's parent statement by its id, kind and first line
// alone (`parent`), and nothing of its state. The parent line names the statement by its
// read id and takes the state the tree read with no view gives it, so statement 7 is marked on
// plan task 7's line, though the view holds no node of it; the plan task itself carries the
// view's own state, and a live statement's line carries nothing.
test("a parent line in a view takes its parent's state from the tree when the view holds only its id", () => {
  const outline = outlineOf([
    readBlock("r7", "requirement", "spec", 0, { state: "proposed", visibility: "author" }),
    readBlock("t7", "plan_task", "plan-md", 0, { parent: "r7" }),
    readBlock("r8", "requirement", "spec", 1),
    readBlock("t8", "plan_task", "plan-md", 1, { parent: "r8" }),
  ]);
  const lineOf = (id: string): IssueTreeParent => ({
    first_line: `Words of ${id}.`,
    id: blockReadId("spec", id),
    kind: "block",
    supertag: "requirement",
  });
  const plan = readAnswer([
    readBlock("t7", "plan_task", "plan-md", 0, { parent: lineOf("r7"), role: "query" }),
    readBlock("t8", "plan_task", "plan-md", 1, { parent: lineOf("r8"), role: "query" }),
  ]);

  const rows = presetView({ ...outline, views: { plan } }, "plan", "").root.children;
  expect(
    rows.map((row) => [
      row.readId,
      markKeys(row),
      row.parentLine?.readId,
      row.parentLine?.life,
      row.parentLine?.onlyYou,
    ])
  ).toEqual([
    [blockReadId("plan-md", "t7"), [], blockReadId("spec", "r7"), "proposed", true],
    [blockReadId("plan-md", "t8"), [], blockReadId("spec", "r8"), undefined, undefined],
  ]);
});

// A statement the Verification view's answer holds live, which the tree read with no
// view answered overtaken, is live in the view: its row carries no mark and is not closed, and
// neither is the check under it, so the view leaves neither out of a count. Only a node the
// view's answer does not hold takes the outline's marks.
test("a node the view holds live stays live whatever the outline says", () => {
  const outline = outlineOf([
    readBlock("r1", "requirement", "spec", 0, { state: "overtaken" }),
    readBlock("c1", "check", "verification-md", 0, { parent: "r1" }),
    readBlock("r2", "requirement", "spec", 1),
  ]);
  const verification = readAnswer([
    readBlock("r1", "requirement", "spec", 0, { role: "context" }),
    readBlock("c1", "check", "verification-md", 0, { parent: "r1", role: "query" }),
    readBlock("r2", "requirement", "spec", 1, { role: "context" }),
  ]);
  expect(markKeys(readNodeOf(outline, blockReadId("spec", "r1")))).toEqual(["closed", "life"]);
  expect(markKeys(readNodeOf(outline, blockReadId("verification-md", "c1")))).toEqual(["closed"]);

  const view = presetView({ ...outline, views: { verification } }, "verification", "").root;
  for (const readId of [
    blockReadId("spec", "r1"),
    blockReadId("verification-md", "c1"),
    blockReadId("spec", "r2"),
  ]) {
    expect(readNodeOf(view, readId), readId).toBeDefined();
    expect(markKeys(readNodeOf(view, readId)), readId).toEqual([]);
  }
});

// A statement whose parent in the tree read is an overtaken question, which the Spec view draws
// no row for, stands under that question in a view's held read as it does in the outline: it is
// closed in the Spec and the Verification views alike, and draws no gap, "No check" included. It
// is a row at its place in each view all the same, and under Spec its group's heading, the
// question, carries Overtaken. With nothing overtaken it is a row with the mark its check gives
// it, as the statement beside it is.
test("a statement under an overtaken question a view draws no row for draws no No check", () => {
  const R3 = "spec#b-r3";
  const rootWith = (asked: Partial<TreeAnswerNode>): TreeNode => {
    const outline = outlineOf([
      readBlock("r1", "requirement", "spec", 0),
      readBlock("c1", "check", "verification-md", 0, { parent: "r1" }),
      readNode("ask-1", "ask", asked),
      readBlock("r3", "requirement", "spec", 1, { parent: "ask-1" }),
      readBlock("c3", "check", "verification-md", 1, { parent: "r3" }),
    ]);
    const asks = askLine("ask-1");
    const spec = readAnswer([
      readBlock("r1", "requirement", "spec", 0, { role: "query" }),
      readBlock("r3", "requirement", "spec", 1, { parent: asks, role: "query" }),
    ]);
    const verification = readAnswer([
      readBlock("r1", "requirement", "spec", 0, { role: "context" }),
      readBlock("c1", "check", "verification-md", 0, { parent: "r1", role: "query" }),
      readBlock("r3", "requirement", "spec", 1, { parent: asks, role: "context" }),
      readBlock("c3", "check", "verification-md", 1, { parent: "r3", role: "query" }),
    ]);
    return { ...outline, views: { spec, verification } };
  };
  const closedIn = (root: TreeNode, preset: "spec" | "verification") =>
    readNodeOf(presetView(root, preset, "").root, blockReadId("spec", "r3"))?.closed === true;

  const overtaken = rootWith({ state: "overtaken" });
  const detail = drawSpec(overtaken);
  try {
    expect(detail.container.querySelector('[data-spec-mark="no_check"]')).toBeNull();
    expect(rowOf(detail.container, "spec#b-r1")).toHaveAttribute("data-spec-mark", "unknown");
    expect(rowOf(detail.container, R3)).not.toBeNull();
    expect(rowOf(detail.container, R3)).not.toHaveAttribute("data-spec-mark");
    expect(marksIn(detail.container.querySelector("[data-spec-group-marks]"))).toEqual([
      "overtaken",
    ]);
  } finally {
    detail.unmount();
  }
  expect([closedIn(overtaken, "spec"), closedIn(overtaken, "verification")]).toEqual([true, true]);

  const verification = drawVerification(overtaken, []);
  try {
    expect(rowOf(verification.container, "spec#b-r1")).not.toBeNull();
    const folded = rowOf(verification.container, R3);
    expect(folded).toHaveAttribute("data-statement-state", "unknown");
    expect(folded?.querySelector("[data-gap]")).toBeNull();
  } finally {
    verification.unmount();
  }

  const live = rootWith({});
  const control = drawSpec(live);
  try {
    expect(rowOf(control.container, "spec#b-r1")).toHaveAttribute("data-spec-mark", "unknown");
    expect(rowOf(control.container, R3)).toHaveAttribute("data-spec-mark", "unknown");
    expect(control.container.querySelector("[data-spec-group-marks]")).toBeNull();
  } finally {
    control.unmount();
  }
  expect([closedIn(live, "spec"), closedIn(live, "verification")]).toEqual([false, false]);
});

// Under Spec the words filter lifts a match under the line of a parent the view's answer holds,
// where the outline holds that parent under an overtaken question the view draws nothing of. The
// match stands under that parent all the same: it is closed, as it is with no words and in the
// outline, so it draws no "No check", and the words draw it as a row as they draw every match.
test("a words match lifted under a parent the outline holds under an overtaken node is closed", async () => {
  const R3 = "spec#b-r3";
  const outline = outlineOf([
    readBlock("r1", "requirement", "spec", 0, { text: "The page draws r1." }),
    readNode("ask-1", "ask", { state: "overtaken" }),
    readBlock("r2", "requirement", "spec", 1, { parent: "ask-1", text: "The page draws r2." }),
    readBlock("r3", "requirement", "spec", 2, { parent: "r2", text: "A zebra crosses r3." }),
  ]);
  const spec = readAnswer([
    readBlock("r1", "requirement", "spec", 0, { role: "query", text: "The page draws r1." }),
    readBlock("r2", "requirement", "spec", 1, {
      parent: askLine("ask-1"),
      role: "query",
      text: "The page draws r2.",
    }),
    readBlock("r3", "requirement", "spec", 2, {
      parent: "r2",
      role: "query",
      text: "A zebra crosses r3.",
    }),
  ]);
  const root = { ...outline, views: { spec } };
  const r3Under = (query: string) =>
    readNodeOf(presetView(root, "spec", query).root, blockReadId("spec", "r3"));
  expect(readNodeOf(outline, blockReadId("spec", "r3"))?.closed).toBe(true);
  expect(r3Under("")?.closed).toBe(true);
  expect(r3Under("zebra")?.parentLine?.id).toBe("spec#b-r2");
  expect(r3Under("zebra")?.closed).toBe(true);

  const view = drawSpec(root, { query: "zebra" });
  try {
    await waitFor(() => expect(rowOf(view.container, R3)).not.toBeNull());
    expect(rowOf(view.container, R3)).not.toHaveAttribute("data-spec-mark");
    expect(view.container.querySelector('[data-spec-mark="no_check"]')).toBeNull();
  } finally {
    view.unmount();
  }
});

// Under Spec the words filter lifts a match under the line of a parent the view's answer holds
// live, where the outline holds that parent overtaken itself and under no closed node. The
// parent's life in the view is the view's own, so the match stands under a live parent: it is
// not closed, with the words as with none, though the outline closes it under that parent. Only a
// parent the outline holds under a closed node closes the rows under its line.
test("a view's row under a parent the view holds live is not closed by that parent's overtaken life in the outline", () => {
  const outline = outlineOf([
    readBlock("r2", "requirement", "spec", 0, { state: "overtaken", text: "The page draws r2." }),
    readBlock("r3", "requirement", "spec", 1, { parent: "r2", text: "A zebra crosses r3." }),
  ]);
  const spec = readAnswer([
    readBlock("r2", "requirement", "spec", 0, { role: "query", text: "The page draws r2." }),
    readBlock("r3", "requirement", "spec", 1, {
      parent: "r2",
      role: "query",
      text: "A zebra crosses r3.",
    }),
  ]);
  const root = { ...outline, views: { spec } };
  const r3Under = (query: string) =>
    readNodeOf(presetView(root, "spec", query).root, blockReadId("spec", "r3"));
  expect(markKeys(readNodeOf(outline, blockReadId("spec", "r2")))).toEqual(["closed", "life"]);
  expect(markKeys(readNodeOf(outline, blockReadId("spec", "r3")))).toEqual(["closed"]);
  expect(r3Under("")?.id).toBe("spec#b-r3");
  expect(markKeys(r3Under(""))).toEqual([]);
  expect(r3Under("zebra")?.parentLine).toMatchObject({
    id: "spec#b-r2",
    readId: blockReadId("spec", "r2"),
  });
  expect(r3Under("zebra")?.parentLine?.life).toBeUndefined();
  expect(markKeys(r3Under("zebra"))).toEqual([]);
});

/** The ask of the question written as block `q1` of the spec. */
const BLOCK_ASK = "ask-1";

/** The question block `id` of the spec as a view's answer names it as a node's parent. */
function blockLine(id: string): IssueTreeParent {
  return {
    first_line: `Words of ${id}.`,
    id: blockReadId("spec", id),
    kind: "block",
    supertag: "ask",
  };
}

/**
 * A question written as an `:::ask` block of the spec: block `q1`, whose node the tree read
 * answers with `block`, and the open ask `BLOCK_ASK` indexed from it, whose node it answers with
 * `ask`, with statement `r1` under the ask. Answers the outline `buildIssueTree` draws of them, the
 * question's row in it, and `r1`'s row in the Verification view, whose answer holds `r1` alone and
 * names its parent by `line`.
 */
function blockQuestion(
  block: ReadExtra,
  ask: ReadExtra,
  line: IssueTreeParent = askLine(BLOCK_ASK)
) {
  const outline = buildIssueTree({
    asks: [
      {
        ...askRead(BLOCK_ASK, ""),
        answer: null,
        block_artifact: { id: "artifact-spec", primary: true, slug: "spec" },
        block_id: "q1",
        state: "open",
      },
    ],
    issue: ISSUE,
    tree: readAnswer([
      ISSUE_NODE,
      readBlock("q1", "ask", "spec", 0, block),
      readNode(BLOCK_ASK, "ask", ask),
      readBlock("r1", "requirement", "spec", 1, { parent: BLOCK_ASK }),
    ]),
  });
  const verification = readAnswer([
    readBlock("r1", "requirement", "spec", 1, { parent: line, role: "context" }),
  ]);
  const view = presetView({ ...outline, views: { verification } }, "verification", "").root;
  return {
    outline,
    question: readNodeOf(outline, blockReadId("spec", "q1")),
    row: readNodeOf(view, blockReadId("spec", "r1")),
  };
}

// A question written as an `:::ask` block of the spec is two nodes in the tree read, the block and
// the ask indexed from it, and one row in the outline: the row carries the block's read id and
// names the ask as its `source`, and it takes the state the ask's node holds, the question itself.
// Proposed, the row carries Proposed and closes nothing; overtaken, it carries Overtaken and closes
// the statement under it. A view's row under the question takes the same, whether the view names
// its parent by the ask or by the block, and so does a place that names the question by its
// address.
test("a question's row takes the state its ask's node holds", () => {
  for (const life of ["proposed", "overtaken"] as const) {
    for (const line of [askLine(BLOCK_ASK), blockLine("q1")]) {
      const { outline, question, row } = blockQuestion({}, { state: life }, line);
      const closes = life === "overtaken";
      const at = `${life} under ${line.id}`;
      expect(question?.source, at).toEqual({ id: BLOCK_ASK, issue: KEY, kind: "ask" });
      expect(question?.life, at).toBe(life);
      expect(question?.closed === true, at).toBe(closes);
      expect(readNodeOf(outline, blockReadId("spec", "r1"))?.closed === true, at).toBe(closes);
      expect(question?.reference, at).toBe(`dispatch://${KEY}/ask/${BLOCK_ASK}`);
      expect(marksAt(statesByReference(outline), question?.reference ?? "")?.life, at).toBe(life);
      expect(row?.parentLine?.life, at).toBe(life);
      expect(row?.closed === true, at).toBe(closes);
    }
  }
});

// The block a question is written as is only its place in the spec, so a state its node holds
// draws nothing: with the block's node proposed or overtaken and the ask's node live, the
// question's row carries no mark and closes nothing, and neither does a view's row under it or a
// place that names the question by its address.
test("a question's row draws nothing of a state its block's node holds", () => {
  for (const life of ["proposed", "overtaken"] as const) {
    for (const line of [askLine(BLOCK_ASK), blockLine("q1")]) {
      const { outline, question, row } = blockQuestion({ state: life }, {}, line);
      const at = `${life} under ${line.id}`;
      expect(question?.source, at).toEqual({ id: BLOCK_ASK, issue: KEY, kind: "ask" });
      expect(markKeys(question), at).toEqual([]);
      expect(markKeys(readNodeOf(outline, blockReadId("spec", "r1"))), at).toEqual([]);
      expect(statesByReference(outline).size, at).toBe(0);
      expect(row?.parentLine, at).toBeDefined();
      expect(row?.parentLine?.life, at).toBeUndefined();
      expect(markKeys(row), at).toEqual([]);
    }
  }
});

// A view that draws nothing of a question written as an `:::ask` block names that parent by the
// ask's read id alone, so the statement under it takes its closing and its parent line's mark from
// the outline's row of the question, which takes the state of the ask's node. A live question
// closes and marks nothing.
test("a view's row under an overtaken question drawn from a block and its ask is closed", () => {
  const overtaken = blockQuestion({}, { state: "overtaken" });
  expect(overtaken.question?.source).toEqual({ id: BLOCK_ASK, issue: KEY, kind: "ask" });
  expect(readNodeOf(overtaken.outline, blockReadId("spec", "r1"))?.closed).toBe(true);
  expect(overtaken.row?.parentLine).toMatchObject({
    id: BLOCK_ASK,
    life: "overtaken",
    readId: BLOCK_ASK,
  });
  expect(overtaken.row?.closed).toBe(true);
  expect(overtaken.row?.gaps).toEqual([]);
  expect(overtaken.row === undefined ? false : outOfCounts(overtaken.row)).toBe(true);

  const live = blockQuestion({}, { state: "live" });
  expect(live.row?.parentLine).toMatchObject({ id: BLOCK_ASK, readId: BLOCK_ASK });
  expect(markKeys(live.row)).toEqual([]);
  expect(live.row?.parentLine?.life).toBeUndefined();
  expect(live.row === undefined ? true : outOfCounts(live.row)).toBe(false);
});

/** The marks drawn under `element`, by their tokens, in the order they are drawn. */
function marksIn(element: Element | null | undefined): (string | undefined)[] {
  return [...(element?.querySelectorAll<HTMLElement>("[data-node-mark]") ?? [])].map(
    (mark) => mark.dataset.nodeMark
  );
}

/** What the page publishes for the statements of `root`: each named by the page's own rule
 *  (`targetNaming`) over the reads it names them from (`blockReads`), and each one's address as
 *  one the page asks for, as `DecisionsFilterBody.drawn.test.tsx`'s `publishedStatements`
 *  publishes them. */
function publishedStatements(root: TreeNode): PublishedReferences {
  const statements = nodesInOrder(root).flatMap((one) => {
    if (one.type !== "requirement") return [];
    const tree = one.tree ?? displayTree(one.text);
    return [tree === undefined ? one : { ...one, tree }];
  });
  return {
    ...targetNaming(blockReads(decisionsIssue(), statements)),
    asked: new Set(
      statements.flatMap(({ reference }) => (reference === undefined ? [] : [reference]))
    ),
  };
}

/** The Decisions view over `root`, with `made` the nodes of the decisions view the page holds on
 *  the issue's node (`TreeNode.views`), with the `shaped` tuples their links are, as the route
 *  answers them (`shapedTuples`), drawn as its slot draws it
 *  (`DecisionsFilterBody.drawn.test.tsx`'s `renderBody`): no words, and an address that names
 *  the nodes `named`, which the view keeps whatever it draws; under the names store with what
 *  the page publishes for the statements (`publishedStatements`), and no names line. */
function drawDecisions(
  root: TreeNode,
  made: readonly TreeAnswerNode[],
  named: readonly string[] = []
) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const held = decisionsAnswer(made, shapedTuples(made));
  client.setQueryData(conversationTreeKey(KEY), held);
  client.setQueryData(viewKey(KEY, "decisions"), held);
  client.setQueryData(["asks", KEY], []);
  client.setQueryData(["comments", KEY], []);
  const issue = decisionsIssue();
  const published = publishedStatements(root);
  return render(
    createElement(
      MemoryRouter,
      null,
      createElement(
        QueryClientProvider,
        { client },
        createElement(
          ReferenceNamesProvider,
          null,
          createElement(
            KeymapProvider,
            null,
            createElement(
              MarginProvider,
              null,
              createElement(Publisher, { published }),
              createElement(IssueScope),
              createElement(
                NoTooltips,
                null,
                createElement(DecisionsFilterBody, {
                  artifacts: artifactsBySlug(issue),
                  emptyLine: EMPTY_STATE_LINES.decisions,
                  holdsEscape: false,
                  isClosed: false,
                  issue,
                  namesLine: NO_FAILED_READS,
                  published,
                  reader: READER,
                  root: { ...root, views: { decisions: held } },
                  view: presetView(root, "decisions", "", new Set(named)).root,
                })
              )
            )
          )
        )
      )
    )
  );
}

/** An overtaken decision and a live one, each waiting for review and tied to statement `s1`,
 *  stamped and drawn under Decisions (`drawDecisions`). */
function drawOvertakenReview() {
  const old = decisionNode("old", { waiting: true });
  const live = decisionNode("live", { waiting: true });
  const root = stamped(node(KEY, "issue", [specStatement("s1"), old, live]), {
    [old.id]: { life: "overtaken" },
  });
  const tied = { decision: { links: [shaped(specRef("s1"))] } };
  const view = drawDecisions(root, [decision("old", tied), decision("live", tied)]);
  return { live, old, root, view };
}

test("Decisions draws an overtaken review row without counting it in the to-review flag", () => {
  const { live, old, root, view } = drawOvertakenReview();
  try {
    const row = rowOf(view.container, old.id);
    expect(row?.closest("[data-decisions-part]")).toHaveAttribute("data-decisions-part", "review");
    const mark = row?.querySelector('[data-node-mark="overtaken"]');
    expect(mark).toHaveTextContent(ROW_MARK_WORDS.overtaken);
    expect(mark?.querySelector("svg")).toHaveAttribute("data-glyph", GLYPH_TOKENS.marks.overtaken);
    expect(rowOf(view.container, live.id)?.closest("[data-decisions-part]")).toHaveAttribute(
      "data-decisions-part",
      "review"
    );
  } finally {
    view.unmount();
  }

  readWide();
  const row = render(
    createElement(
      MemoryRouter,
      null,
      createElement(IssuePresets, {
        active: "decisions",
        issueKey: KEY,
        signals: { ...filterSignals(root, ""), conversation: { count: 0 }, graph: undefined },
      })
    )
  );
  try {
    expect(textOf(row.container, '[data-preset-count="decisions"]')).toBe("2");
    expect(textOf(row.container, '[data-preset-flag="to_review"]')).toBe("· 1 to review");
  } finally {
    row.unmount();
  }
});

// An overtaken decision that waited for review keeps its part, its words and its mark under
// Decisions, but nothing there waits on the reader: its row draws no Keep, no Change and no
// waiting colour, as its row in the outline draws none. The live decision beside it draws all
// three.
test("an overtaken decision in the review part draws no Keep, no Change and no waiting colour", () => {
  const { live, old, view } = drawOvertakenReview();
  // What the row of `id` draws: its part, its words and its marks, whether it offers Keep and
  // Change, and whether its state is drawn in the decisions' violet, which says it waits.
  const drawn = (id: string) => {
    const row = rowOf(view.container, id);
    if (row === null) throw new Error(`the view draws no row of ${id}`);
    const state = row.querySelector("[data-node-margin] [data-node-state]")?.parentElement;
    return {
      change: row.querySelector(rowActionSelector("change")) !== null,
      keep: row.querySelector(rowActionSelector("keep")) !== null,
      marks: marksIn(row),
      part: row.closest("[data-decisions-part]")?.getAttribute("data-decisions-part"),
      waiting: state?.classList.contains(pageStateText.decision),
      words: textOf(row, "[data-node-words]"),
    };
  };
  try {
    expect(drawn(old.id)).toEqual({
      change: false,
      keep: false,
      marks: ["overtaken"],
      part: "review",
      waiting: false,
      words: "The decision old.",
    });
    expect(drawn(live.id)).toEqual({
      change: true,
      keep: true,
      marks: [],
      part: "review",
      waiting: true,
      words: "The decision live.",
    });
  } finally {
    view.unmount();
  }
});

test("an address to an overtaken decision keeps Decisions on and focuses its row", async () => {
  readWide();
  const old = decision("old", {
    decision: { links: [shaped(specRef("s1"))] },
    state: "overtaken",
  });
  const tree = decisionsAnswer([
    treeNode("s1", "requirement", KEY, {
      document: { artifact_id: SPEC_DOCUMENT.id, position: 1, slug: SPEC_DOCUMENT.slug },
      text: "A statement the decision shaped.",
    }),
    old,
  ]);
  const reads = [
    spyOn(api, "getIssueCoverage").mockResolvedValue(coverageRead([])),
    spyOn(api, "getIssueResults").mockResolvedValue({
      checks: [],
      issue: KEY,
      verification_artifact_id: "",
    }),
    spyOn(api, "getIssueReferences").mockResolvedValue({ members: [], truncated: false }),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
    spyOn(api, "listIssueAskThreads").mockResolvedValue([]),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "getIssueTree").mockResolvedValue(tree),
    spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] }),
    spyOn(api, "whoAmI").mockResolvedValue({ event_head: "0", kind: "user", login: READER }),
  ];
  const view = drawAtBrowserEntry(`${presetPath(KEY, "decisions")}#b-old`, decisionsIssue());
  try {
    const id = decisionNode("old").id;
    await waitFor(() => expect(focusedRow(view.container)).toBe(id));
    expect(drawnSlot(view)).toBe("the view");
    expect(rowOf(view.container, id)?.closest("[data-decisions-body]")).not.toBeNull();
    expect(rowOf(view.container, id)).toHaveAttribute("tabindex", "0");
  } finally {
    view.unmount();
    for (const read of reads) read.mockRestore();
  }
});

// Under Decisions a proposed decision is marked in its margin, after the word for how it
// stands on that line, and on a line of its own where it stands in none. The line naming its
// parent carries the parent's marks after the parent's words, and each statement it informs
// carries that statement's marks after its name, both in the compact form. A live entry, and a
// live statement it informs, carry none.
test("the Decisions view marks a proposed decision and its parent line", () => {
  const waiting = decisionNode("waiting");
  const under = decisionNode("under", { chips: [chip(specRef("s7")), chip(specRef("s1"))] });
  const root = stamped(
    node(KEY, "issue", [
      specStatement("s1"),
      specStatement("s7"),
      openAsk("q-live", false),
      answeredAsk(
        "a-put",
        { at: "2026-11-14T09:00:00Z", text: "Keep the rail." },
        { children: [under] }
      ),
      waiting,
    ]),
    {
      "a-put": { life: "proposed", onlyYou: true },
      [specStatement("s7").id]: { life: "proposed" },
      [under.id]: { life: "proposed" },
      [waiting.id]: { life: "proposed" },
    }
  );
  const view = drawDecisions(root, [
    decision("under", {
      decision: {
        links: [shaped(specRef("s7")), shaped(specRef("s1"))],
        waiting_review: false,
      },
    }),
    decision("waiting", { decision: { links: [shaped(specRef("s1"))] } }),
  ]);
  try {
    // Waiting for the reader's review: the mark after the state's word, on the state's line.
    const state = rowOf(view.container, waiting.id)?.querySelector(
      "[data-node-margin] [data-node-state]"
    );
    expect(state?.getAttribute("data-node-state")).toBe("waiting_review");
    expect(marksIn(state?.parentElement?.parentElement)).toEqual(["proposed"]);
    expect(state?.parentElement?.nextElementSibling?.getAttribute("data-node-mark")).toBe(
      "proposed"
    );

    // Standing in none: no state, and the mark on a line of its own.
    const decided = rowOf(view.container, under.id);
    expect(decided?.querySelector("[data-node-margin] [data-node-state]")).toBeNull();
    expect(marksIn(decided?.querySelector("[data-node-margin]"))).toEqual(["proposed"]);

    // Its parent's line: the question it hangs under, proposed and reserved for the reader,
    // marked after its words.
    const parentLine = decided?.querySelector("[data-node-parent-line]");
    expect(parentLine?.getAttribute("data-parent-id")).toBe("a-put");
    expect(marksIn(parentLine)).toEqual(["proposed", "only_you"]);
    expect(textOf(decided as HTMLElement, "[data-node-parent-line]")).toMatch(
      new RegExp(`${ROW_MARK_WORDS.proposed} ${ROW_MARK_WORDS.only_you}$`, "u")
    );

    // The statements it informs: the proposed one marked after its name, the live one not.
    const informs = (block: string) =>
      decided?.querySelector(`[data-decision-statement="${specStatement(block).id}"]`);
    expect(informs("s7")).not.toBeNull();
    expect(marksIn(informs("s7"))).toEqual(["proposed"]);
    expect(informs("s7")?.lastElementChild?.getAttribute("data-node-mark")).toBe("proposed");
    expect(informs("s1")).not.toBeNull();
    expect(marksIn(informs("s1"))).toEqual([]);

    // The question itself, answered, carries its own marks on its state's line; a live
    // question carries none.
    expect(marksIn(rowOf(view.container, "a-put")?.querySelector("[data-node-margin]"))).toEqual([
      "proposed",
      "only_you",
    ]);
    expect(rowOf(view.container, "q-live")).not.toBeNull();
    expect(marksIn(rowOf(view.container, "q-live"))).toEqual([]);
  } finally {
    view.unmount();
  }
});

// A person's decision, which the read answers on the answered question itself, ties that question
// to the statements it shaped: the question's row draws them, each with the marks of the statement
// itself, since the question's own chips are none of the decision's ties.
test("under Decisions a statement an answered question informs carries its own marks", () => {
  const root = stamped(
    node(KEY, "issue", [
      specStatement("s1"),
      specStatement("s7"),
      answeredAsk("a-put", { at: "2026-11-14T09:00:00Z", text: "Keep the rail." }),
    ]),
    { [specStatement("s7").id]: { life: "proposed" } }
  );
  const view = drawDecisions(root, [
    personDecision("a-put", { decision: { links: [shaped(specRef("s7"))] } }),
  ]);
  try {
    const informs = rowOf(view.container, "a-put")?.querySelector(
      `[data-decision-statement="${specStatement("s7").id}"]`
    );
    expect(informs).not.toBeNull();
    expect(marksIn(informs)).toEqual(["proposed"]);
  } finally {
    view.unmount();
  }
});

// From the statement above it `q` reaches the overtaken question, and `w` reaches the overtaken
// decision behind that statement, without leaving the view.
test("under Decisions `q` and `w` focus the overtaken question and decision", async () => {
  const user = userEvent.setup();
  const statementId = specStatement("s1").id;
  const past = decisionNode("d-past", { chips: [chip(specRef("s1"))] });
  const live = decisionNode("d-live", { chips: [chip(specRef("s1"))] });
  const root = stamped(
    node(KEY, "issue", [
      specStatement("s1"),
      openAsk("q-past"),
      openAsk("q-live", false),
      past,
      live,
    ]),
    { "q-past": { life: "overtaken" }, [past.id]: { life: "overtaken" } }
  );
  const view = drawDecisions(
    root,
    [
      decision("d-past", { decision: { links: [shaped(specRef("s1"))] } }),
      decision("d-live", { decision: { links: [shaped(specRef("s1"))] } }),
    ],
    [statementId]
  );
  try {
    const statement = rowOf(view.container, statementId);
    expect(statement).not.toBeNull();
    act(() => statement?.focus());
    expect(focusedRow(view.container)).toBe(statementId);

    await user.keyboard("q");
    await waitFor(() => expect(focusedRow(view.container)).toBe("q-past"));

    act(() => rowOf(view.container, statementId)?.focus());
    expect(focusedRow(view.container)).toBe(statementId);
    await user.keyboard("w");
    await waitFor(() => expect(focusedRow(view.container)).toBe(past.id));
  } finally {
    view.unmount();
  }
});

/** The plan document's address of the task `block`. */
function planRef(block: string): string {
  return `dispatch://${KEY}/artifact/plan-md#b-${block}`;
}

/** One task of the plan document, by its block id, as the Plan view hands it over. */
function planTask(block: string, extra: Partial<TreeNode> = {}): TreeNode {
  return node(`plan-md#b-${block}`, "plan_task", [], {
    reference: planRef(block),
    text: `Task ${block} is carried out.`,
    ...extra,
  });
}

const T1 = "plan-md#b-t1";
const T2 = "plan-md#b-t2";
const T3 = "plan-md#b-t3";

/** Three tasks of the plan, in its order, the second one a later task took the place of. */
function planWithOvertaken(): TreeNode {
  return stamped(node(KEY, "issue", [planTask("t1"), planTask("t2"), planTask("t3")]), {
    [T2]: { life: "overtaken" },
  });
}

/** What a case draws the Plan view under: the whole plan's tasks where the view is what the
 *  words filter left of them, the chain down to the node the address names, the address's
 *  search, which carries the words (`?q=`), and what the page publishes of its batched names
 *  read, by default what it publishes for the tree's chips (`publishedTree`); with `take`, the
 *  page's `navigate` handed to it, so a case moves the address as the reader does. */
interface PlanPlace {
  readonly plan?: readonly TreeNode[];
  readonly published?: PublishedReferences;
  readonly reveal?: readonly TreeNode[];
  readonly search?: string;
  readonly take?: (navigate: (to: string) => void) => void;
}

/** The Plan view over `root`, the view's tasks, drawn as its slot draws it
 *  (`PlanFilterBody.test.tsx`'s `renderBody`), with no document read, so the plan's own order is
 *  the order the view hands the tasks over in, under the names store with `published` published
 *  to it and handed to the body, and no names line. */
function planBody(
  root: TreeNode,
  client: QueryClient,
  {
    plan = root.children,
    published,
    reveal,
    search = "",
    take,
  }: PlanPlace & { readonly published: PublishedReferences }
): ReactNode {
  return createElement(
    ReferenceNamesProvider,
    null,
    createElement(
      MemoryRouter,
      { initialEntries: [`/${search}`] },
      take === undefined ? null : createElement(NavigateProbe, { take }),
      createElement(
        QueryClientProvider,
        { client },
        createElement(
          KeymapProvider,
          null,
          createElement(
            MarginProvider,
            null,
            createElement(
              FocusedNodeProvider,
              null,
              createElement(IssueScope),
              createElement(Publisher, { published }),
              createElement(PlanFilterBody, {
                artifacts: new Map(),
                context: new Set<string>(),
                emptyLine: EMPTY_STATE_LINES.plan,
                focusReveal: true,
                holdsEscape: search !== "",
                isClosed: false,
                namesLine: NO_FAILED_READS,
                onEditing: () => {},
                plan,
                planBlocks: undefined,
                published,
                reads: { state: "answered" },
                reveal,
                root,
                rootRow: false,
              })
            )
          )
        )
      )
    )
  );
}

/** `planBody` drawn, with `again` to draw it once more at another place, as the page does when
 *  its address moves. What the page publishes is one value for as long as the tree is the same,
 *  as the page's is: the case's, else what the page publishes for the tree's chips. */
function drawPlan(root: TreeNode, place: PlanPlace = {}) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const published = place.published ?? publishedTree(root);
  const view = render(planBody(root, client, { ...place, published }));
  return {
    ...view,
    again: (next: PlanPlace) => view.rerender(planBody(root, client, { ...next, published })),
  };
}

// In the Plan view an overtaken task has no position: the plan's order leaves it out, so no step
// stands at its place and the chart draws it no bar. It is a row after every step all the same,
// carrying Overtaken and its shape: in the chart a row of its own after the bars, at their level,
// which the chart's keys walk to after the last bar and End reaches, and which takes the one tab
// stop once the reader is on it; under Steps the outline's row after the steps.
test("under Plan an overtaken task is a row after the steps with no position, marked", async () => {
  readWide();
  const root = planWithOvertaken();
  expect(
    planOrder(root.children).map((entry) => (entry.kind === "step" ? entry.task.id : "cycle"))
  ).toEqual([T1, T3]);
  const user = userEvent.setup();
  const view = drawPlan(root);
  try {
    expect(
      [...view.container.querySelectorAll("[data-plan-bar-row]")].map((bar) =>
        bar.getAttribute("data-node-id")
      )
    ).toEqual([T1, T3]);
    expect(view.container.querySelector(`[data-plan-bar="${T2}"]`)).toBeNull();
    expect(rowIds(view.container)).toEqual([T1, T3, T2]);
    const replaced = rowOf(view.container, T2);
    expect(replaced).toHaveAttribute("role", "treeitem");
    expect(replaced).toHaveAttribute("aria-level", "1");
    expect(replaced?.querySelector("[data-plan-bar]")).toBeNull();
    expect(marksIn(replaced)).toEqual(["overtaken"]);
    expect(replaced?.querySelector('[data-node-mark="overtaken"]')?.textContent).toContain(
      ROW_MARK_WORDS.overtaken
    );
    const stops = (): (Element | null)[] => [
      ...view.container.querySelectorAll('[role="tree"] [tabindex="0"]'),
    ];
    expect(stops()).toEqual([rowOf(view.container, T1)]);

    act(() => rowOf(view.container, T1)?.focus());
    await user.keyboard("j");
    expect(document.activeElement).toBe(rowOf(view.container, T3));
    await user.keyboard("j");
    expect(document.activeElement).toBe(replaced);
    expect(stops()).toEqual([replaced]);
    await user.keyboard("k");
    expect(document.activeElement).toBe(rowOf(view.container, T3));
    await user.keyboard("{End}");
    expect(document.activeElement).toBe(replaced);

    await user.click(view.getByRole("button", { name: PLAN_WORDS.steps }));
    expect(view.container.querySelectorAll("[data-plan-chart]")).toHaveLength(0);
    expect(rowIds(view.container)).toEqual([T1, T3, T2]);
    expect(marksIn(rowOf(view.container, T2))).toEqual(["overtaken"]);
  } finally {
    view.unmount();
  }
});

// An address naming an overtaken task focuses its row after the bars, at their level, as an
// address naming any task focuses its row: in the chart, and, where the address moves while the
// reader reads the steps, in the steps, though the outline holds the task under a statement the
// view draws as no row.
test("an address naming an overtaken task focuses its row under Plan", async () => {
  readWide();
  const user = userEvent.setup();
  const root = planWithOvertaken();
  const replaced = pathTo(root, T2)?.at(-1);
  if (replaced === undefined) throw new Error("the plan holds task 2");
  // The chain the outline gives the address: the issue, the statement the task serves, the task.
  const addressed = [root, node("spec#b-r1", "requirement"), replaced];

  const chart = drawPlan(root, { reveal: addressed });
  try {
    await waitFor(() => expect(document.activeElement).toBe(rowOf(chart.container, T2)));
    expect(rowOf(chart.container, T2)).toHaveAttribute("aria-level", "1");
    expect(rowOf(chart.container, T2)).toHaveAttribute("tabindex", "0");
    expect(rowIds(chart.container)).toEqual([T1, T3, T2]);
  } finally {
    chart.unmount();
  }

  const steps = drawPlan(root);
  try {
    await user.click(steps.getByRole("button", { name: PLAN_WORDS.steps }));
    steps.again({ reveal: addressed });
    await waitFor(() => expect(document.activeElement).toBe(rowOf(steps.container, T2)));
    expect(rowIds(steps.container)).toEqual([T1, T3, T2]);
  } finally {
    steps.unmount();
  }
});

// The words filter matches an overtaken task's words and draws it as it draws every match: the
// view holds task 2 alone, under the whole plan's order, and the chart draws it as its one row, at
// a bar's level, holding the one tab stop, and the steps as theirs, each marked Overtaken.
test("the Plan view's words filter draws an overtaken task it matches as a row, marked", async () => {
  readWide();
  const user = userEvent.setup();
  const root = planWithOvertaken();
  const replaced = pathTo(root, T2)?.at(-1);
  if (replaced === undefined) throw new Error("the plan holds task 2");
  const words = drawPlan(node(KEY, "issue", [replaced]), {
    plan: root.children,
    search: "?q=t2",
  });
  try {
    expect(rowIds(words.container)).toEqual([T2]);
    expect(rowOf(words.container, T2)).toHaveAttribute("aria-level", "1");
    expect(rowOf(words.container, T2)).toHaveAttribute("tabindex", "0");
    expect(marksIn(rowOf(words.container, T2))).toEqual(["overtaken"]);
    await user.click(words.getByRole("button", { name: PLAN_WORDS.steps }));
    expect(rowIds(words.container)).toEqual([T2]);
    expect(marksIn(rowOf(words.container, T2))).toEqual(["overtaken"]);
  } finally {
    words.unmount();
  }
});

// Under Steps, an address naming a live task that the outline holds under an overtaken statement
// focuses the task's row, as an address naming any task does: the steps hold every task under
// their own root, and the overtaken statement in the outline's chain is no row of theirs.
test("under Steps an address to a live task under an overtaken statement focuses its row", async () => {
  readWide();
  const user = userEvent.setup();
  const root = planWithOvertaken();
  const live = pathTo(root, T3)?.at(-1);
  if (live === undefined) throw new Error("the plan holds task 3");
  // The chain the outline gives the address: the issue, the overtaken statement the task serves,
  // the task.
  const reveal = [root, node("spec#b-r8", "requirement", [], { life: "overtaken" }), live];
  const view = drawPlan(root);
  try {
    await user.click(view.getByRole("button", { name: PLAN_WORDS.steps }));
    view.again({ reveal });
    await waitFor(() => expect(document.activeElement).toBe(rowOf(view.container, T3)));
    expect(rowIds(view.container)).toEqual([T1, T3, T2]);
  } finally {
    view.unmount();
  }
});

// The Plan chart marks a proposed step after its facts, and names what a step needs and
// what it serves each with the marks of the node named, after its name, in the compact form. A
// step with no mark of its own draws none in its facts.
test("the Plan chart marks a proposed step, and what a step needs and serves", () => {
  readWide();
  const serves: ParentLine = {
    id: "spec#b-r7",
    readId: "spec#b-r7",
    reference: `dispatch://${KEY}/spec#b-r7`,
    text: "The page draws r7.",
    type: "requirement",
  };
  const needs: NodeChip = {
    answered: true,
    gone: false,
    label: BLOCK_ATTRIBUTE_LABELS.needs,
    reading: { words: "Task t1 is carried out." },
    ref: planRef("t1"),
  };
  // What the page's batched read names the step's need by, and the reading the view names the
  // statement it serves by (`presets.ts`' `nodeReading`, `PresetView.readings`).
  const published = publishedChips([
    needs,
    { answered: true, gone: false, reading: nodeReading(serves), ref: serves.reference ?? "" },
  ]);
  const view = drawPlan(
    stamped(
      node(KEY, "issue", [planTask("t1"), planTask("t2", { chips: [needs], parentLine: serves })]),
      { [T1]: { life: "proposed" }, "spec#b-r7": { life: "proposed", onlyYou: true } }
    ),
    { published }
  );
  try {
    const bar = (id: string) =>
      view.container.querySelector(`[data-plan-bar-row][data-node-id="${id}"]`);
    expect(marksIn(bar(T1)?.querySelector("[data-plan-facts]"))).toEqual(["proposed"]);
    expect(bar(T1)?.querySelector("[data-plan-facts]")?.lastElementChild).toHaveAttribute(
      "data-node-mark",
      "proposed"
    );
    expect(marksIn(bar(T2)?.querySelector(`[data-plan-need="${T1}"]`))).toEqual(["proposed"]);
    expect(bar(T2)?.querySelector("[data-plan-facts] > [data-node-mark]")).toBeNull();
    expect(marksIn(bar(T2)?.querySelector("[data-plan-serves]"))).toEqual(["proposed", "only_you"]);
  } finally {
    view.unmount();
  }
});

// The steps are the outline's rows, so a focused proposed step draws the row's acts (`RowActs`),
// Settle among them, and a focused live step draws its acts with no Settle.
test("a proposed step carries Settle", async () => {
  readWide();
  const user = userEvent.setup();
  const view = drawPlan(
    stamped(node(KEY, "issue", [planTask("t1"), planTask("t2")]), {
      [T1]: { life: "proposed" },
    })
  );
  try {
    await user.click(view.getByRole("button", { name: PLAN_WORDS.steps }));
    act(() => rowOf(view.container, T1)?.focus());
    await waitFor(() =>
      expect(rowOf(view.container, T1)?.querySelector('[data-row-act="settle"]')).not.toBeNull()
    );
    act(() => rowOf(view.container, T2)?.focus());
    await waitFor(() =>
      expect(rowOf(view.container, T2)?.querySelector("[data-row-acts]")).not.toBeNull()
    );
    expect(rowOf(view.container, T2)?.querySelector('[data-row-act="settle"]')).toBeNull();
    expect(view.container.querySelectorAll('[data-row-act="settle"]')).toHaveLength(0);
  } finally {
    view.unmount();
  }
});

// A lens, a finding, a risk or a constraint the tree read answers proposed is a proposed row like
// any other (R2, R4): the state route marks any node the store holds proposed, these four kinds
// among them, and the settle route takes such a node under the issue it stands under by its
// whole address (`dispatch://lens/<uuid>`, which names no issue of its own). So the focused row
// offers Settle, and a press sends it under the page's issue.
test("a proposed lens, finding, risk or constraint carries Settle, sent under the page's issue", async () => {
  readWide();
  const user = userEvent.setup();
  const LENS = "lens/0b8f6e2a-3c1d-4e5f-8a9b-0c1d2e3f4a5b";
  const RISK = "risk/1c9a7f3b-4d2e-4f60-9bac-1d2e3f4a5b6c";
  const CONSTRAINT = "constraint/2dab804c-5e3f-4071-acbd-2e3f4a5b6c7d";
  const FINDING = `${LENS}#b-f1`;
  const view = renderTree(
    stamped(
      node(KEY, "issue", [
        node(LENS, "lens", [node(FINDING, "finding", [], { reference: `dispatch://${FINDING}` })], {
          reference: `dispatch://${LENS}`,
        }),
        node(RISK, "risk", [], { reference: `dispatch://${RISK}` }),
        node(CONSTRAINT, "constraint", [], { reference: `dispatch://${CONSTRAINT}` }),
      ]),
      {
        [CONSTRAINT]: { life: "proposed" },
        [FINDING]: { life: "proposed" },
        [LENS]: { life: "proposed" },
        [RISK]: { life: "proposed" },
      }
    )
  );
  const sent: WriteRequest[] = [];
  const send = spyOn(api, "send").mockImplementation((async (request: WriteRequest) => {
    sent.push(request);
    return { node: {}, previous: "proposed" };
  }) as typeof api.send);
  try {
    // Every node is drawn open as the page opens, a proposed one too, so the finding is a row
    // under its open lens.
    await waitFor(() =>
      expect(rowOf(view.container, LENS)).toHaveAttribute("aria-expanded", "true")
    );
    for (const id of [LENS, FINDING, RISK, CONSTRAINT]) {
      await waitFor(() => expect(rowOf(view.container, id), `${id} is a row`).not.toBeNull());
      act(() => rowOf(view.container, id)?.focus());
      await waitFor(() =>
        expect(rowOf(view.container, id)?.querySelector("[data-row-acts]"), id).not.toBeNull()
      );
      expect(
        rowOf(view.container, id)?.querySelector('[data-row-act="settle"]'),
        `the focused proposed ${id} offers Settle`
      ).not.toBeNull();
    }
    act(() => rowOf(view.container, LENS)?.focus());
    const settle = await waitFor(() => {
      const found = rowOf(view.container, LENS)?.querySelector<HTMLElement>(
        '[data-row-act="settle"]'
      );
      expect(found, "the focused proposed lens offers Settle").toBeTruthy();
      return found as HTMLElement;
    });
    await user.click(settle);
    await waitFor(() => expect(sent).toHaveLength(1));
    expect(sent[0]?.path).toBe(
      `/api/v1/issues/${KEY}/nodes/${encodeURIComponent(`dispatch://${LENS}`)}/settle`
    );
  } finally {
    send.mockRestore();
    view.unmount();
  }
});
