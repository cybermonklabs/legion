import { afterEach, beforeAll, expect, spyOn, test } from "bun:test";
import type { LensRead, NodeLenses } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createElement, type ReactNode, useMemo } from "react";
import {
  MemoryRouter,
  Route,
  Routes,
  useLocation,
  useNavigate,
  useSearchParams,
} from "react-router-dom";

import { api, type WriteRequest } from "../../../api/client";
import { artifactBlocksQuery, issueCoverageQuery, issueResultsQuery } from "../../../api/queries";
import type { CheckBlock, IssueCheckResults, IssueDetails } from "../../../api/types";
import { pageFocusedRow } from "../../../theme/classes";
import { MarginProvider } from "../../margin/margin-context";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import { parseIssuePath } from "../../refs/routes";
import { KeymapProvider } from "../../shell/KeymapProvider";
import { useKeymapScope } from "../../shell/keymap";
import { contentsEntries } from "../contents-entries";
import { DecisionsFilterBody } from "../decisions-filter/DecisionsFilterBody";
import {
  answeredAsk,
  artifact,
  decision,
  decisionNode,
  treeAnswer as decisionsAnswer,
  issueDetails as decisionsIssue,
  openAsk,
  READER,
  shaped,
  issueTree as specIssueTree,
  specRef,
  statement as specStatement,
} from "../decisions-filter/decisions-fixtures";
import { FocusedNodeProvider } from "../focused-node";
import { ContentsList, contentsOf } from "../IssueContents";
import { IssuePresets } from "../IssuePresets";
import { IssueTree, type OutlineRequest } from "../IssueTree";
import { overtakenLineOf } from "../issue-address";
import { artifactsBySlug } from "../issue-artifacts";
import {
  buildIssueTree,
  heldBy,
  type NodeChip,
  type ParentLine,
  type TreeNode,
} from "../issue-tree";
import { LensChain } from "../lenses/LensChain";
import type { LensChip, LensFacts } from "../lenses/lens-drafts";
import { NodeLink } from "../lenses/NodeLink";
import { NoTooltips } from "../NoTooltips";
import { OutlineRequestProvider } from "../outline-request";
import { KEY_REPORTS } from "../page-structure";
import {
  BLOCK_ATTRIBUTE_LABELS,
  CONTROL_LABELS,
  EMPTY_STATE_LINES,
  GLYPH_TOKENS,
  NODE_STATE_WORDS,
  type NodeType,
  PLAN_WORDS,
  type Preset,
  ROW_MARK_WORDS,
  VERIFICATION_WORDS,
} from "../page-words";
import { PlanFilterBody } from "../plan-filter/PlanFilterBody";
import { planOrder } from "../plan-filter/plan-order";
import { type PresetSignal, presetView } from "../presets";
import { ReferenceNamesProvider } from "../reference-names";
import { ShortcutHelpOpener } from "../shortcut-help-opener";
import { SpecFilterBody } from "../spec-filter/SpecFilterBody";
import {
  answerNode,
  coverageRead,
  issueDetails,
  decision as specDecision,
  decisionNode as specDecisionNode,
  shaped as specShaped,
  statementRef as specStatementRef,
  specViewAnswer,
  statement,
} from "../spec-filter/spec-fixtures";
import { pathTo } from "../tree-fold";
import { statesOfRead } from "../tree-read/tree-nodes";
import {
  issueTreeKey,
  type TreeAnswer,
  type TreeAnswerNode,
  viewKey,
} from "../tree-read/tree-read";
import { useIssueKeymap } from "../useIssueKeymap";
import { VerificationFilterBody } from "../verification-filter/VerificationFilterBody";
import {
  check,
  checkNode,
  result,
  statementNode,
} from "../verification-filter/verification-fixtures";
import { NodeMarks, NodeStatesProvider } from "./NodeMarks";
import {
  linesIn,
  marksAt,
  type NodeStates,
  stampStates,
  statesByReference,
  withLine,
} from "./node-states";
import { OvertakenLine } from "./OvertakenLine";
import { useOvertakenTarget } from "./settle-write";

/**
 * A node's life and who may read it, put on a tree built by hand: the states arrive by the read's
 * own id, which each node built here carries as its id (`TreeNode.readId`), and every place that
 * draws a node, and every count over the tree, reads them from the one tree. The lines a path
 * opens, and the marks and the line as a row draws them, are read here too, and so are the tree's
 * keys and its opening on a line, over `IssueTree` drawn under the page's keymap (`renderTree`);
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
  return { answered: true, gone: false, name: ref, ref };
}

/** `root` with `states` put on it by the read's own id, as the page puts the tree read's on it.
 *  A node built by another file's helper carries no read id, so each node is read as the node of
 *  the tree read whose id is its own where it names none. */
function stamped(root: TreeNode, states: Readonly<Record<string, NodeStates>>): TreeNode {
  return stampStates(readAsIds(root), new Map(Object.entries(states)), heldBy);
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
  const line = (id: string): ParentLine => ({ id, line: `${id}.`, readId: id, type: "ask" });
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
  expect(stampStates(root, new Map(), heldBy)).toBe(root);
});

// A parent's overtaken children fold to one line, so its folded count counts that line as
// one row and nothing under it, while each overtaken node still counts what it holds itself.
test("a parent's overtaken children are one row of its held count", () => {
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

  expect(tree.held).toBe(3);
  expect(pathTo(tree, "spec#b-r2")?.at(-1)?.held).toBe(2);
  expect(stamped(root, { "spec#b-r3": { life: "overtaken" } }).held).toBe(6);
});

// An address, a chip, a Contents entry, `q` or `w` that names an overtaken node, or a node
// under one, opens the line that holds it - each line, where an overtaken node stands under
// another - and a path through no overtaken node leaves the fold as it was.
test("a path opens the line of each overtaken node it runs through, and none for a path through none", () => {
  const deep = node("comment-2", "comment", [], { closed: true });
  const inner = node("verification-md#b-c1", "check", [deep], { closed: true, life: "overtaken" });
  const outer = node("spec#b-r8", "requirement", [inner], { closed: true, life: "overtaken" });
  const root = node(KEY, "issue", [
    node("spec", "document", [node("spec#b-r7", "requirement"), outer]),
  ]);
  const opened: ReadonlySet<string> = new Set(["elsewhere"]);

  expect(overtakenLineOf(pathTo(root, "comment-2"))).toBe("spec");
  expect([...(withLine(opened, pathTo(root, "comment-2")) ?? [])]).toEqual([
    "elsewhere",
    "spec",
    "spec#b-r8",
  ]);
  expect([...(withLine(undefined, pathTo(root, "spec#b-r8")) ?? [])]).toEqual(["spec"]);
  // The set the reader's own lines are held in is a new one, never the one they had.
  expect(opened).toEqual(new Set(["elsewhere"]));

  expect(overtakenLineOf(pathTo(root, "spec#b-r7"))).toBeUndefined();
  expect(withLine(opened, pathTo(root, "spec#b-r7"))).toBe(opened);
  expect(withLine(opened, undefined)).toBe(opened);
});

// Under the words filter each line of the view holds a match, so each is drawn open: the
// lines are the nodes that hold an overtaken child, at any depth, and no other.
test("each node that holds an overtaken child holds a line, in reading order", () => {
  const root = node(KEY, "issue", [
    node("spec", "document", [
      node("spec#b-r8", "requirement", [node("verification-md#b-c1", "check")], {
        life: "overtaken",
      }),
      node("spec#b-r9", "requirement", [
        node("verification-md#b-c2", "check", [], { life: "overtaken" }),
      ]),
    ]),
    node("plan-md", "document", [node("plan-md#b-t1", "plan_task", [], { life: "proposed" })]),
  ]);

  expect(linesIn(root)).toEqual(["spec", "spec#b-r9"]);
  expect(linesIn(node(KEY, "issue"))).toEqual([]);
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

// A parent's overtaken children fold to one line after its other children, which says how
// many it holds in one text node, takes the focus with `aria-expanded`, and opens and closes in
// place: in the outline a press opens it, drawing each child as a row, and the next press closes
// it again. It is no line of a deeper branch and holds no Unfold, so nothing that opens every
// branch opens it.
test("a parent's overtaken children fold to one line that a press opens and closes", async () => {
  readWide();
  const user = userEvent.setup();
  const view = renderTree(
    stamped(
      node(KEY, "issue", [
        node("comment-1", "comment"),
        node("spec#b-r8", "requirement"),
        node("spec#b-r9", "requirement"),
      ]),
      { "spec#b-r8": { life: "overtaken" }, "spec#b-r9": { life: "overtaken" } }
    )
  );
  try {
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    const line = theLine(view.container);
    expect(line).toHaveAttribute("role", "treeitem");
    expect(line).toHaveAttribute("aria-expanded", "false");
    // At the level of the children it stands after.
    expect(line.getAttribute("aria-level")).toBe(
      rowOf(view.container, "comment-1")?.getAttribute("aria-level") ?? null
    );
    expect(line).toHaveAttribute("tabindex", "-1");
    const texts: string[] = [];
    const walker = document.createTreeWalker(line, NodeFilter.SHOW_TEXT);
    for (let text = walker.nextNode(); text !== null; text = walker.nextNode()) {
      texts.push(text.textContent ?? "");
    }
    expect(texts).toEqual([`2 ${CONTROL_LABELS.overtaken_count}`]);
    expect(view.container.querySelector("[data-branch-more]")).toBeNull();
    expect(line.querySelector("button")).toBeNull();
    expect(rowOf(view.container, "spec#b-r8")).toBeNull();
    expect(rowOf(view.container, "spec#b-r9")).toBeNull();

    await user.click(line);
    await waitFor(() => expect(theLine(view.container)).toHaveAttribute("aria-expanded", "true"));
    expect(rowOf(view.container, "spec#b-r8")).not.toBeNull();
    expect(rowOf(view.container, "spec#b-r9")).not.toBeNull();

    await user.click(theLine(view.container));
    await waitFor(() => expect(theLine(view.container)).toHaveAttribute("aria-expanded", "false"));
    expect(rowOf(view.container, "spec#b-r8")).toBeNull();
    expect(rowOf(view.container, "spec#b-r9")).toBeNull();
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
 *  one 1440 px wide, where the tree draws its rows whole rather than one node at a time
 *  (`IssueTree.tsx`'s `ZOOMED_TREE_QUERY`), as `IssueTree.test.tsx`'s `setViewport` does. */
function readWide(): void {
  const width = 1440;
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

/** `root` drawn by the page's tree, under the page's keymap and the providers it reads, as
 *  `IssueTree.test.tsx`'s `renderTree` draws it: no address, no filter, no document; with
 *  `beside` drawn after the tree, under the same providers. */
function renderTree(root: TreeNode, ...beside: ReactNode[]) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
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
              createElement(IssueTree, {
                artifacts: new Map(),
                context: new Set<string>(),
                emptyLine: "Nothing here.",
                filtering: false,
                focusReveal: false,
                holdsEscape: false,
                isClosed: false,
                onEditing: () => {},
                reveal: undefined,
                root,
                rootRow: true,
              }),
              ...beside
            )
          )
        )
      )
    )
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

/** The one line of overtaken nodes the tree draws. */
function theLine(container: HTMLElement): HTMLElement {
  const line = container.querySelector<HTMLElement>("[data-overtaken-line]");
  if (line === null) throw new Error("the tree draws no line of overtaken nodes");
  return line;
}

/** An issue whose first row is a comment, and whose second comment a later one took the place
 *  of, so the tree draws the comment and then the line its overtaken sibling folds to. */
function commentAndLine(): TreeNode {
  return stamped(node(KEY, "issue", [node("comment-1", "comment"), node("comment-2", "comment")]), {
    "comment-2": { life: "overtaken" },
  });
}

// The line takes the focus as a row does, so it is the tree's one tab stop once the
// reader is on it: Shift+Tab comes back to it, and the row they left takes no Tab of its own.
test("the overtaken line is the tree's tab stop once the reader is on it", async () => {
  readWide();
  const user = userEvent.setup();
  const view = renderTree(commentAndLine());
  try {
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    expect(rowOf(view.container, "comment-1")).toHaveAttribute("tabindex", "0");
    const line = theLine(view.container);
    expect(line).toHaveAttribute("tabindex", "-1");

    await user.keyboard("j");
    await waitFor(() => expect(document.activeElement).toBe(line));
    await waitFor(() => expect(line).toHaveAttribute("tabindex", "0"));
    expect(rowOf(view.container, "comment-1")).toHaveAttribute("tabindex", "-1");
  } finally {
    view.unmount();
  }
});

/** A proposed requirement the tree draws first, and the line its overtaken sibling folds to. */
function proposedAndLine(R7: string): TreeNode {
  return stamped(
    node(KEY, "issue", [
      node(R7, "requirement", [], { reference: `dispatch://${KEY}/${R7}` }),
      node("comment-2", "comment"),
    ]),
    { [R7]: { life: "proposed" }, "comment-2": { life: "overtaken" } }
  );
}

// The focus going from a row to the line lets go of the row it left, as the focus leaving the
// tree does: its focused look and its acts, Settle among them for a proposed row, which no key
// reaches from the line. The line looks focused instead.
test("the row the reader leaves for the overtaken line no longer looks focused", async () => {
  readWide();
  const user = userEvent.setup();
  const R7 = "spec#b-r7";
  const view = renderTree(proposedAndLine(R7));
  const left = () => rowOf(view.container, R7);
  try {
    await waitFor(() => expect(focusedRow(view.container)).toBe(R7));
    expect(left()?.querySelector("[data-node-card]")?.className).toContain(pageFocusedRow);
    expect(left()?.querySelector('[data-row-act="settle"]')).not.toBeNull();
    const line = theLine(view.container);
    expect(line.className).not.toContain(pageFocusedRow);

    await user.keyboard("j");
    await waitFor(() => expect(document.activeElement).toBe(line));
    await waitFor(() =>
      expect(
        left()?.querySelector("[data-node-card]")?.className,
        "the row left for the line still looks focused"
      ).not.toContain(pageFocusedRow)
    );
    expect(left()?.querySelector("[data-row-acts]"), "the row left kept its acts").toBeNull();
    expect(line.className, "the line holds the focus with no focused look").toContain(
      pageFocusedRow
    );
  } finally {
    view.unmount();
  }
});

// A press focuses the line as it goes down, and the row above it letting go of its look and its
// acts then would shrink the rows under the pointer, so the press could end off the line and open
// nothing. The row the press left is let go once the press is over, as a pressed row takes the
// focus then (`press-focus.ts`), and the line holds the stop from then on.
test("a press on the overtaken line lets go of the row the reader left once it ends", async () => {
  readWide();
  const user = userEvent.setup();
  const R7 = "spec#b-r7";
  const view = renderTree(proposedAndLine(R7));
  const left = () => rowOf(view.container, R7);
  try {
    await waitFor(() => expect(focusedRow(view.container)).toBe(R7));
    await waitFor(() => expect(left()?.querySelector('[data-row-act="settle"]')).not.toBeNull());
    const line = theLine(view.container);

    await user.pointer({ keys: "[MouseLeft>]", target: line });
    expect(document.activeElement, "the press did not focus the line").toBe(line);
    expect(
      left()?.querySelector("[data-node-card]")?.className,
      "the held press let go of the row's focused look"
    ).toContain(pageFocusedRow);
    expect(
      left()?.querySelector('[data-row-act="settle"]'),
      "the held press let go of the row's acts"
    ).not.toBeNull();
    expect(line, "the held press gave the line the stop").toHaveAttribute("tabindex", "-1");

    await user.pointer({ keys: "[/MouseLeft]", target: line });
    await waitFor(() =>
      expect(left()?.querySelector("[data-node-card]")?.className).not.toContain(pageFocusedRow)
    );
    expect(left()?.querySelector("[data-row-acts]"), "the row left kept its acts").toBeNull();
    expect(line).toHaveAttribute("tabindex", "0");
    expect(line, "the press did not open the line").toHaveAttribute("aria-expanded", "true");
  } finally {
    view.unmount();
  }
});

// The line hands its own element to the tree as its focus lands (`onStop`), so a press that
// focused it is checked against the line itself once it ends, whatever holds the focus when the
// tree reads it.
test("the overtaken line hands itself to the tree as it takes the focus", () => {
  const stops: HTMLElement[] = [];
  const view = render(
    createElement(OvertakenLine, {
      onStop: (line: HTMLElement) => stops.push(line),
      row: { held: 1, level: 1, of: node(KEY, "issue"), open: false, trail: [] },
      tabStop: true,
    })
  );
  try {
    const line = theLine(view.container);
    act(() => line.focus());
    expect(stops).toHaveLength(1);
    expect(stops[0], "the line handed the tree no element of its own").toBe(line);
  } finally {
    view.unmount();
  }
});

/** The page's chords of the view row, under the page's keymap (`useIssueKeymap.ts`). */
function IssueChords(): ReactNode {
  useIssueKeymap(KEY);
  return null;
}

/** What each control of the view row draws after its name: nothing is counted here. */
const NO_SIGNALS: Readonly<Record<Preset, PresetSignal>> = {
  conversation: { count: 0 },
  decisions: { count: 0 },
  plan: { count: 0 },
  spec: { count: 0 },
  verification: { count: 0 },
};

/** The outline of a comment and the line its overtaken sibling folds to, drawn with the view
 *  row, whose Keys hands `opened` the element that holds the focus as the list of keys opens,
 *  and with the page's chords; and that line, once the reader is on it, holding the tree's one
 *  tab stop, with the focus back on the page itself. */
async function lineHoldingTheStop(opened: (Element | null)[]) {
  const user = userEvent.setup();
  const view = renderTree(
    commentAndLine(),
    createElement(IssueChords),
    createElement(
      ShortcutHelpOpener.Provider,
      { value: () => opened.push(document.activeElement) },
      createElement(IssuePresets, { active: "conversation", issueKey: KEY, signals: NO_SIGNALS })
    )
  );
  await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
  const line = theLine(view.container);
  await user.keyboard("j");
  await waitFor(() => expect(document.activeElement).toBe(line));
  act(() => line.blur());
  expect(document.activeElement).toBe(document.body);
  expect(line).toHaveAttribute("tabindex", "0");
  return { line, user, view };
}

// While the reader's last place in the outline is an overtaken line, the line holds the tree's
// one tab stop and no row does, so the chord that brings the reader to the outline puts the
// focus on the line.
test("the chord to the outline focuses an overtaken line that holds the tab stop", async () => {
  readWide();
  const { line, user, view } = await lineHoldingTheStop([]);
  try {
    await user.keyboard("tc");
    await waitFor(() => expect(document.activeElement).toBe(line));
  } finally {
    view.unmount();
  }
});

// Keys puts the focus on the outline's tab stop before the list of keys opens, so the list
// offers the outline's keys: that stop is an overtaken line where the line holds it.
test("Keys focuses an overtaken line that holds the tab stop before the keys list opens", async () => {
  readWide();
  const opened: (Element | null)[] = [];
  const { line, user, view } = await lineHoldingTheStop(opened);
  try {
    await user.click(view.getByRole("button", { name: CONTROL_LABELS.keys }));
    // By identity: a failed deep comparison of two elements walks the whole page.
    expect(opened).toHaveLength(1);
    expect(opened[0] === line).toBe(true);
  } finally {
    view.unmount();
  }
});

// A key pressed on the line that answers in words - `w` there has no decision behind
// it, since the line stands for no node - is drawn at the line, and the next move lets it go.
test("a key's report on the overtaken line is drawn at the line", async () => {
  readWide();
  const user = userEvent.setup();
  const view = renderTree(commentAndLine());
  try {
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    const line = theLine(view.container);
    await user.keyboard("j");
    await waitFor(() => expect(document.activeElement).toBe(line));

    await user.keyboard("w");
    await waitFor(() =>
      expect(line.querySelector("[data-row-report]")?.textContent).toBe(KEY_REPORTS.no_decision)
    );
    expect(view.container.querySelectorAll("[data-row-report]")).toHaveLength(1);

    await user.keyboard("k");
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    expect(view.container.querySelector("[data-row-report]")).toBeNull();
  } finally {
    view.unmount();
  }
});

// A key's report on one line of overtaken nodes is that line's: a press on another line
// takes the reader there, and the report does not follow them to it.
test("a key's report on one overtaken line does not move to another line pressed after it", async () => {
  readWide();
  const user = userEvent.setup();
  const view = renderTree(
    stamped(
      node(KEY, "issue", [
        node("comment-1", "comment", [
          node("comment-1a", "comment"),
          node("comment-1b", "comment"),
        ]),
        node("comment-2", "comment"),
      ]),
      { "comment-1b": { life: "overtaken" }, "comment-2": { life: "overtaken" } }
    )
  );
  try {
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    // The cycle's last state, which draws every branch, so both lines are drawn.
    await user.keyboard("z");
    await user.keyboard("z");
    const lines = () => [...view.container.querySelectorAll<HTMLElement>("[data-overtaken-line]")];
    await waitFor(() => expect(lines()).toHaveLength(2));

    await user.click(lines()[0] as HTMLElement);
    await waitFor(() => expect(document.activeElement).toBe(lines()[0]));
    await user.keyboard("w");
    await waitFor(() =>
      expect(lines()[0]?.querySelector("[data-row-report]")?.textContent).toBe(
        KEY_REPORTS.no_decision
      )
    );

    await user.click(lines()[1] as HTMLElement);
    await waitFor(() => expect(document.activeElement).toBe(lines()[1]));
    expect(lines()[1]?.querySelector("[data-row-report]")).toBeNull();
    expect(view.container.querySelectorAll("[data-row-report]")).toHaveLength(0);
  } finally {
    view.unmount();
  }
});

// The cycle key's last state draws every branch, and a line the reader opens there opens:
// the lines are no branch, so that state keeps the ones the reader opened.
test("in the cycle's last state, Enter on the overtaken line opens it", async () => {
  readWide();
  const user = userEvent.setup();
  const view = renderTree(commentAndLine());
  try {
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    await user.keyboard("z");
    await user.keyboard("z");
    const line = theLine(view.container);
    await user.keyboard("j");
    await waitFor(() => expect(document.activeElement).toBe(line));
    expect(line).toHaveAttribute("aria-expanded", "false");
    expect(rowOf(view.container, "comment-2")).toBeNull();

    await user.keyboard("{Enter}");
    await waitFor(() => expect(line).toHaveAttribute("aria-expanded", "true"));
    expect(rowOf(view.container, "comment-2")).not.toBeNull();
  } finally {
    view.unmount();
  }
});

// The cycle key draws bodies above what holds the focus and moves no focus, so the outline puts
// what holds it back on screen once the cycle's tree is drawn: on the outline's line of overtaken
// nodes that is the line, which the bodies drawn above it would otherwise push off the screen.
test("the cycle key brings the overtaken line that holds the focus on screen", async () => {
  readWide();
  const user = userEvent.setup();
  const view = renderTree(commentAndLine());
  // happy-dom lays nothing out, so the line's box is given: below the window's foot. Every other
  // box is empty.
  const below = { bottom: 2028, top: 2000 };
  const boxes = spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement
  ) {
    return this.matches("[data-overtaken-line]")
      ? new DOMRect(0, below.top, 800, below.bottom - below.top)
      : new DOMRect();
  });
  const scrolled = spyOn(window, "scrollBy").mockImplementation(() => {});
  try {
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    const line = theLine(view.container);
    await user.keyboard("j");
    await waitFor(() => expect(document.activeElement).toBe(line));
    scrolled.mockClear();

    await user.keyboard("z");
    expect(document.activeElement).toBe(line);
    expect(scrolled, "the cycle left the line where it stood").toHaveBeenCalled();
    // Scrolled by that much, the line stands whole inside the window.
    const by = (scrolled.mock.calls[0]?.[0] as ScrollToOptions | undefined)?.top ?? 0;
    expect(below.top - by).toBeGreaterThanOrEqual(0);
    expect(below.bottom - by).toBeLessThanOrEqual(window.innerHeight);
  } finally {
    boxes.mockRestore();
    scrolled.mockRestore();
    view.unmount();
  }
});

// An overtaken question that waited on the reader waits no more, so the page opens on
// the outline's first row, with the line that holds the question closed.
test("the page does not open on an overtaken question", async () => {
  readWide();
  const view = renderTree(
    stamped(
      node(KEY, "issue", [
        node("comment-1", "comment"),
        node("ask-1", "ask", [], { question: true, state: "open", waiting: true }),
      ]),
      { "ask-1": { life: "overtaken" } }
    )
  );
  try {
    await waitFor(() => expect(focusedRow(view.container)).toBe("comment-1"));
    expect(theLine(view.container)).toHaveAttribute("aria-expanded", "false");
    expect(rowOf(view.container, "ask-1")).toBeNull();
    expect(focusedRow(view.container)).not.toBe("ask-1");
  } finally {
    view.unmount();
  }
});

// `q` and `w` take the reader to an overtaken question and an overtaken decision, which are
// no rows while their line is closed: each key opens the line and focuses the node's row.
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
    const line = theLine(view.container);
    expect(line).toHaveAttribute("aria-expanded", "false");
    expect(rowOf(view.container, "ask-1")).toBeNull();

    await user.keyboard("q");
    await waitFor(() => expect(focusedRow(view.container)).toBe("ask-1"));
    expect(line).toHaveAttribute("aria-expanded", "true");

    // The line closed again, from the line itself, so `w` finds its decision no row.
    await user.keyboard("k");
    await waitFor(() => expect(document.activeElement).toBe(line));
    await user.keyboard("{Enter}");
    await waitFor(() => expect(line).toHaveAttribute("aria-expanded", "false"));
    expect(rowOf(view.container, "decisions-md#b-d1")).toBeNull();
    await user.keyboard("k");
    await waitFor(() => expect(focusedRow(view.container)).toBe("spec#b-r1"));

    await user.keyboard("w");
    await waitFor(() => expect(focusedRow(view.container)).toBe("decisions-md#b-d1"));
    expect(line).toHaveAttribute("aria-expanded", "true");
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

/** The Verification view over `root`, with `checks` as the issue's results read, drawn as its
 *  slot draws it (`VerificationFilterBody.drawn.test.tsx`'s `bodyOf`): under the words `query`,
 *  where a case types any, with the view under no query as `whole`, which is that view itself
 *  where no query is typed, on the page's `address`, which carries the words as the page's own
 *  does (`?q=`). */
function drawVerification(
  root: TreeNode,
  checks: readonly CheckBlock[],
  query = "",
  address = query === "" ? "/" : `/?q=${encodeURIComponent(query)}`
) {
  const preset = presetView(root, "verification", query, new Set());
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  return render(
    createElement(
      QueryClientProvider,
      { client },
      createElement(
        MemoryRouter,
        { initialEntries: [address] },
        createElement(
          NoTooltips,
          null,
          createElement(VerificationFilterBody, {
            checks,
            context: preset.context,
            reads: { state: "answered" },
            view: preset.root,
            whole:
              query === "" ? preset.root : presetView(root, "verification", "", new Set()).root,
          })
        )
      )
    )
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

// Under Verification a statement's overtaken check is no row until the reader opens the one
// line it folds to, the last row of the statement's group of checks, and an overtaken statement
// folds to one line after every other row. Each line is a row the tree's keys walk, in the order
// it is drawn, and the tree's one tab stop once the walk is on it. A press, Enter and the arrows
// open and close it in place: the check is then a row under it marked Overtaken, which the walk
// reaches after the line. No count of the summary reads the check or the statement.
test("the Verification view folds an overtaken check into one line that a press opens", async () => {
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
  const replacedId = checkNode(replaced).id;
  const view = drawVerification(
    stamped(node(KEY, "issue", [first, second]), {
      [replacedId]: { life: "overtaken" },
      [second.id]: { life: "overtaken" },
    }),
    [passed, replaced, underReplaced]
  );
  try {
    expect(rowOf(view.container, replacedId)).toBeNull();
    expect(rowOf(view.container, second.id)).toBeNull();
    const [line, issueLine] = [
      ...view.container.querySelectorAll<HTMLElement>("[data-overtaken-line]"),
    ];
    if (line === undefined || issueLine === undefined) throw new Error("two lines are drawn");
    expect(line).toHaveAttribute("role", "treeitem");
    expect(line).toHaveAttribute("aria-expanded", "false");
    expect(line.textContent).toBe(`1 ${CONTROL_LABELS.overtaken_count}`);
    expect(rowOf(view.container, first.id)?.querySelector('[role="group"]')?.lastElementChild).toBe(
      line
    );
    expect(issueLine.textContent).toBe(`1 ${CONTROL_LABELS.overtaken_count}`);
    expect(textOf(view.container, "[data-verification-covered]")).toBe(
      `1 ${VERIFICATION_WORDS.of} 1 ${VERIFICATION_WORDS.covered}`
    );
    expect(rowOf(view.container, first.id)).toHaveAttribute("data-statement-state", "met");
    expect(textOf(view.container, '[data-check-legend-state="failed"]')).toBe(
      `0${NODE_STATE_WORDS.check.failed}`
    );
    // One tab stop in the tree, the issue's row, before the walk.
    const stops = () => view.container.querySelectorAll('[role="tree"] [tabindex="0"]');
    expect(line).toHaveAttribute("tabindex", "-1");

    // The walk passes through each line, which takes the tab stop as a row does.
    act(() => rowOf(view.container, first.id)?.focus());
    await user.keyboard("j");
    expect(document.activeElement).toBe(rowOf(view.container, checkNode(passed).id));
    await user.keyboard("j");
    expect(document.activeElement).toBe(line);
    expect(line).toHaveAttribute("tabindex", "0");
    expect(
      [...stops()].filter((stop) => stop.matches("[data-node-row], [data-overtaken-line]"))
    ).toEqual([line]);
    await user.keyboard("j");
    expect(document.activeElement).toBe(issueLine);
    await user.keyboard("k");
    expect(document.activeElement).toBe(line);

    // Enter opens the line in place, and the walk goes on into the check it holds.
    await user.keyboard("{Enter}");
    expect(line).toHaveAttribute("aria-expanded", "true");
    const opened = rowOf(view.container, replacedId);
    expect(opened !== null && line.contains(opened)).toBe(true);
    expect(opened?.querySelector('[data-node-mark="overtaken"]')?.textContent).toContain(
      ROW_MARK_WORDS.overtaken
    );
    await user.keyboard("j");
    expect(document.activeElement).toBe(opened);
    await user.keyboard("k");
    expect(document.activeElement).toBe(line);

    // The left arrow closes the open line, and on the closed line goes to its statement.
    await user.keyboard("{ArrowLeft}");
    expect(line).toHaveAttribute("aria-expanded", "false");
    expect(rowOf(view.container, replacedId)).toBeNull();
    await user.keyboard("{ArrowLeft}");
    expect(document.activeElement).toBe(rowOf(view.container, first.id));

    // The issue's line, after every other row, opens by a press, and End and Home reach it and
    // leave it.
    await user.keyboard("{End}");
    expect(document.activeElement).toBe(issueLine);
    await user.click(issueLine);
    expect(issueLine).toHaveAttribute("aria-expanded", "true");
    expect(
      rowOf(view.container, second.id)?.querySelector('[data-node-mark="overtaken"]')
    ).not.toBeNull();
    await user.keyboard("{Home}");
    expect(document.activeElement).toBe(view.container.querySelector("[data-verification-issue]"));
  } finally {
    view.unmount();
  }
});

// Under Verification a press that closes a line takes the rows it holds off the tree, so
// where one of them held the tree's one tab stop, the line takes it: a statement's line, whose
// check would hand it back to the issue's row, and the issue's line, whose statements would keep
// it on a row no longer drawn.
test("a press that closes a Verification line over the tab stop hands the stop to the line", async () => {
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
    const [line, issueLine] = [
      ...view.container.querySelectorAll<HTMLElement>("[data-overtaken-line]"),
    ];
    if (line === undefined || issueLine === undefined) throw new Error("two lines are drawn");
    const issueRow = view.container.querySelector("[data-verification-issue]");
    const rowStops = () =>
      view.container.querySelectorAll('[role="tree"] [data-node-row][tabindex="0"]');

    // A statement's line: the stop on the check it holds, then a press closes it.
    await user.click(line);
    expect(line).toHaveAttribute("aria-expanded", "true");
    act(() => rowOf(view.container, first.id)?.focus());
    await user.keyboard("jjj");
    expect(document.activeElement).toBe(rowOf(view.container, replacedId));
    expect(rowOf(view.container, replacedId)).toHaveAttribute("tabindex", "0");
    await user.click(line);
    expect(line).toHaveAttribute("aria-expanded", "false");
    expect(rowOf(view.container, replacedId)).toBeNull();
    expect(line).toHaveAttribute("tabindex", "0");
    expect(issueRow).toHaveAttribute("tabindex", "-1");
    expect(rowStops()).toHaveLength(0);

    // The issue's line: the stop on a check of the statement it holds, then a press closes it.
    await user.click(issueLine);
    expect(issueLine).toHaveAttribute("aria-expanded", "true");
    await user.keyboard("{End}");
    expect(document.activeElement).toBe(rowOf(view.container, underId));
    expect(rowOf(view.container, underId)).toHaveAttribute("tabindex", "0");
    await user.click(issueLine);
    expect(issueLine).toHaveAttribute("aria-expanded", "false");
    expect(rowOf(view.container, second.id)).toBeNull();
    expect(issueLine).toHaveAttribute("tabindex", "0");
    expect(line).toHaveAttribute("tabindex", "-1");
    expect(rowStops()).toHaveLength(0);
  } finally {
    view.unmount();
  }
});

// Under Verification the words filter draws a match that is an overtaken check under its
// open line, and a line the reader closes under the words stays closed.
test("the Verification view draws an overtaken check the words match under an open line", async () => {
  const user = userEvent.setup();
  const passed = checkThat("c-one", true);
  const replaced = { ...checkThat("c-two", false), text: "The zebrafinch words are drawn." };
  const replacedId = checkNode(replaced).id;
  const view = drawVerification(
    stamped(
      node(KEY, "issue", [
        statementNode("r1", "The page draws the checks.", [checkNode(passed), checkNode(replaced)]),
      ]),
      { [replacedId]: { life: "overtaken" } }
    ),
    [passed, replaced],
    "zebrafinch"
  );
  try {
    const line = theLine(view.container);
    expect(line).toHaveAttribute("aria-expanded", "true");
    const match = rowOf(view.container, replacedId);
    expect(match !== null && line.contains(match)).toBe(true);
    expect(rowOf(view.container, checkNode(passed).id)).toBeNull();

    await user.click(line);
    expect(line).toHaveAttribute("aria-expanded", "false");
    expect(rowOf(view.container, replacedId)).toBeNull();
  } finally {
    view.unmount();
  }
});

// Under Verification a words match on a result of an overtaken check is drawn under the check's
// open line, with the check: the view lifts the result under its statement, with the check as
// its parent line, and the body draws the check that holds it under the statement's line. With
// the check live the result is drawn with it, and no line is drawn.
test("the Verification view draws a result of an overtaken check the words match under an open line", () => {
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
  const issue = node(KEY, "issue", [
    statementNode("r1", "The page draws the checks.", [checkNode(passed), checkNode(seen)]),
  ]);
  const overtaken = drawVerification(
    stamped(issue, { [seenId]: { life: "overtaken" } }),
    [passed, seen],
    "zebrafinch"
  );
  try {
    expect(overtaken.container.textContent).toContain("zebrafinch");
    const line = theLine(overtaken.container);
    expect(line).toHaveAttribute("aria-expanded", "true");
    const check = rowOf(overtaken.container, seenId);
    expect(check !== null && line.contains(check)).toBe(true);
    expect(check?.textContent).toContain("zebrafinch seen");
  } finally {
    overtaken.unmount();
  }

  const live = drawVerification(issue, [passed, seen], "zebrafinch");
  try {
    expect(live.container.textContent).toContain("zebrafinch seen");
    expect(live.container.querySelector("[data-overtaken-line]")).toBeNull();
  } finally {
    live.unmount();
  }
});

// Under Verification a words match on a result of an overtaken check that serves no statement is
// drawn with that check under the issue's line, as the check is with no words, and the words draw
// that line open: the view lifts the result to its top, with the check as its parent line, and the
// body draws the check that holds it in its place. Where the result the words kept is not the
// check's latest, the check draws it under its latest, so the result is on screen. With the check
// live the result is drawn with it too, as a row of the table's own, and no line is drawn.
test("the Verification view draws a result of an overtaken check that serves no statement under the issue's line", () => {
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
    const line = theLine(overtaken.container);
    expect(line).toHaveAttribute("aria-expanded", "true");
    const check = rowOf(overtaken.container, seenId);
    expect(check !== null && line.contains(check)).toBe(true);
    expect(check?.textContent).toContain("zebrafinch seen");
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
    expect(check !== null && theLine(kept.container).contains(check)).toBe(true);
    expect(check?.textContent).toContain("zebrafinch seen");
  } finally {
    kept.unmount();
  }

  const live = drawVerification(issueWith(seen), [passed, seen], "zebrafinch");
  try {
    expect(live.container.querySelector("[data-verification-other]")).toBeNull();
    const check = rowOf(live.container, seenId);
    expect(check?.textContent).toContain("zebrafinch seen");
    expect(live.container.querySelector("[data-overtaken-line]")).toBeNull();
  } finally {
    live.unmount();
  }
});

/** An issue of one statement whose second check a later one took the place of, and the checks
 *  the results read answers for it: the live one, then the overtaken one. */
function statementWithOvertakenCheck() {
  const passed = checkThat("c-one", true);
  const replaced = checkThat("c-two", false);
  const replacedId = checkNode(replaced).id;
  const root = stamped(
    node(KEY, "issue", [
      statementNode("r1", "The page draws the checks.", [checkNode(passed), checkNode(replaced)]),
    ]),
    { [replacedId]: { life: "overtaken" } }
  );
  return { checks: [passed, replaced], replacedId, root };
}

// Words of spaces alone narrow nothing: the view trims them, and so does what the body reads the
// address's words by, so under `?q=%20` the line of overtaken checks is drawn closed, as it is
// with no words.
test("the Verification view reads words of spaces as no words and draws its line closed", () => {
  const { checks, replacedId, root } = statementWithOvertakenCheck();
  const view = drawVerification(root, checks, " ");
  try {
    expect(theLine(view.container)).toHaveAttribute("aria-expanded", "false");
    expect(rowOf(view.container, replacedId)).toBeNull();
  } finally {
    view.unmount();
  }
});

// An address that names an overtaken check opens the line that holds it, so the check is drawn
// as a row under its open line, as the address drew it before the check folded; the reader's
// press closes that line again, and it stays closed.
test("the Verification view opens the line of an overtaken check an address names", async () => {
  const user = userEvent.setup();
  const { checks, replacedId, root } = statementWithOvertakenCheck();
  const view = drawVerification(root, checks, "", `/issues/${KEY}/verification#b-c-two`);
  try {
    const line = theLine(view.container);
    await waitFor(() => expect(line).toHaveAttribute("aria-expanded", "true"));
    const addressed = rowOf(view.container, replacedId);
    expect(addressed !== null && line.contains(addressed)).toBe(true);

    await user.click(line);
    await waitFor(() => expect(line).toHaveAttribute("aria-expanded", "false"));
    expect(rowOf(view.container, replacedId)).toBeNull();
  } finally {
    view.unmount();
  }
});

// A press focuses a Verification line as it goes down, and the check the reader left shutting
// the facts its focus drew open then would shrink it under the pointer: the line, after it, would
// move up, and the press could end off the line and open nothing. The check shuts them once the
// press is over.
test("a press on a Verification line keeps the facts of the check the reader left until it ends", async () => {
  const user = userEvent.setup();
  const { checks, root } = statementWithOvertakenCheck();
  const view = drawVerification(root, checks);
  try {
    const left = view.container.querySelector<HTMLElement>("[data-verification-check]");
    if (left === null) throw new Error("the view draws no check");
    const line = theLine(view.container);
    act(() => left.focus());
    expect(left, "the focus did not open the check's facts").toHaveAttribute(
      "aria-expanded",
      "true"
    );

    await user.pointer({ keys: "[MouseLeft>]", target: line });
    expect(document.activeElement, "the press did not focus the line").toBe(line);
    expect(left, "the held press shut the facts of the check left").toHaveAttribute(
      "aria-expanded",
      "true"
    );
    expect(left.querySelector("[data-node-body]")).not.toBeNull();

    await user.pointer({ keys: "[/MouseLeft]", target: line });
    await waitFor(() => expect(left).toHaveAttribute("aria-expanded", "false"));
    expect(left.querySelector("[data-node-body]")).toBeNull();
    expect(line, "the press did not open the line").toHaveAttribute("aria-expanded", "true");
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
// is shut, where the view's lines fold an overtaken check and where a chip narrows it alike: the
// body reads its rows from the view as the lines and the chip leave it, and that reading keeps
// no result of a check, as the view with no words keeps none.
test("the Verification view with no words draws no earlier result where its lines or a chip narrow it", async () => {
  const user = userEvent.setup();
  const twice = passedWith("c-one", ["older pass", "newer pass"]);
  const replaced = checkThat("c-two", false);
  const root = stamped(
    node(KEY, "issue", [
      statementNode("r1", "The page draws the checks.", [checkNode(twice), checkNode(replaced)]),
    ]),
    { [checkNode(replaced).id]: { life: "overtaken" } }
  );
  const view = drawVerification(root, [twice, replaced]);
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
// check under the issue's line with its latest result and the one the words kept, and leave its
// other earlier results folded behind their line, as every check the words keep a result of does.
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
    const check = rowOf(view.container, seenId);
    expect(check !== null && theLine(view.container).contains(check)).toBe(true);
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
 *  answered yet. `go` moves the address, and `unmount` puts the reads back as well. */
function drawSpec(
  root: TreeNode,
  {
    asksOut = false,
    query = "",
    request,
    reveal,
  }: { asksOut?: boolean; query?: string; request?: OutlineRequest; reveal?: string } = {}
): { container: HTMLElement; go: (to: string) => void; unmount: () => void } {
  const issue = issueDetails();
  const results: IssueCheckResults = { checks: [], issue: KEY, verification_artifact_id: "" };
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(issueResultsQuery(KEY).queryKey, results);
  client.setQueryData(issueCoverageQuery(KEY).queryKey, coverageRead([]));
  client.setQueryData(artifactBlocksQuery(issue.primary_artifact_id).queryKey, []);
  // The tree read and the Spec view's read, held answered with no node, as the page holds them
  // once read: the body takes its decisions from the view read and reads nothing on its own.
  client.setQueryData(issueTreeKey(KEY), readAnswer([]));
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
  const view = render(
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
                    root,
                  })
                )
              )
            )
          )
        )
      )
    )
  );
  return {
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

// The Contents list draws no entry for an overtaken question, nor for a
// question under one, and its count, the view row's, agrees with what it draws. A live question
// beside it keeps its entry.
test("the Contents list draws no entry for an overtaken question", () => {
  const root = stamped(
    node(KEY, "issue", [
      node("ask-1", "ask"),
      node("ask-2", "ask", [node("ask-3", "ask")]),
      node("ask-4", "ask"),
    ]),
    { "ask-2": { life: "overtaken" } }
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
    expect(entries).toEqual(["ask-1", "ask-4"]);
    expect(contentsEntries(root, undefined)).toBe(entries.length);
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

/** An issue of two statements, the second overtaken, whose one group under the issue draws the
 *  first statement and then the line the second folds to. */
function statementAndLine(): TreeNode {
  return stamped(node(KEY, "issue", [statement("r1"), statement("r2")]), {
    "spec#b-r2": { life: "overtaken" },
  });
}

// Under Spec the line a group's overtaken statements fold to takes the focus as a
// statement does, and with it the tree's one tab stop: the statement the reader left takes no
// Tab of its own while the line holds it, so Shift+Tab from the line leaves the tree.
test("the Spec view's overtaken line is its tab stop once the reader is on it", async () => {
  const user = userEvent.setup();
  const view = drawSpec(statementAndLine());
  try {
    const first = rowOf(view.container, "spec#b-r1");
    expect(first).toHaveAttribute("tabindex", "0");
    expect(rowOf(view.container, "spec#b-r2")).toBeNull();
    const line = theLine(view.container);
    expect(line).toHaveAttribute("tabindex", "-1");

    act(() => first?.focus());
    await user.keyboard("j");
    await waitFor(() => expect(document.activeElement).toBe(line));
    await waitFor(() => expect(line).toHaveAttribute("tabindex", "0"));
    expect(first).toHaveAttribute("tabindex", "-1");
    expect(view.container.querySelector('[data-node-row][tabindex="0"]')).toBeNull();

    await user.tab({ shift: true });
    const tree = view.container.querySelector("[data-spec-tree]");
    expect(tree?.contains(document.activeElement)).toBe(false);
  } finally {
    view.unmount();
  }
});

// A press focuses the Spec view's line as it goes down, and the statement the reader left letting
// go of its focus then - its words cut again, its "more" back - would shrink it under the pointer:
// the line, after it, would move up, and the press could end off the line and open nothing. The
// statement is let go once the press is over, as a pressed statement takes the focus then
// (`focusItem`), and the line holds the stop from then on.
test("a press on the Spec view's overtaken line lets go of the statement the reader left once it ends", async () => {
  const user = userEvent.setup();
  const view = drawSpec(statementAndLine());
  try {
    const first = rowOf(view.container, "spec#b-r1");
    if (first === null) throw new Error("the view draws no statement");
    const line = theLine(view.container);
    act(() => first.focus());
    await waitFor(() => expect(first.className).toContain(pageFocusedRow));

    await user.pointer({ keys: "[MouseLeft>]", target: line });
    expect(document.activeElement, "the press did not focus the line").toBe(line);
    expect(first.className, "the held press let go of the statement").toContain(pageFocusedRow);
    expect(line, "the held press gave the line the stop").toHaveAttribute("tabindex", "-1");

    await user.pointer({ keys: "[/MouseLeft]", target: line });
    await waitFor(() => expect(first.className).not.toContain(pageFocusedRow));
    expect(line).toHaveAttribute("tabindex", "0");
    expect(line, "the press did not open the line").toHaveAttribute("aria-expanded", "true");
  } finally {
    view.unmount();
  }
});

// A key pressed on the Spec view's line that answers in words - `q` where no question
// is open, since the line stands for no node - is drawn at the line, and the next move lets it
// go. The Spec view read is held on the issue's node, as the page holds it, with the two
// statements, the second overtaken: `q` says it goes nowhere only once every read the view draws
// from holds an answer.
test("a key's report on the Spec view's overtaken line is drawn at the line", async () => {
  const user = userEvent.setup();
  const spec = readAnswer([
    readBlock("r1", "requirement", "spec", 0, { mark: "query" }),
    readBlock("r2", "requirement", "spec", 1, { mark: "query", state: "overtaken" }),
  ]);
  const view = drawSpec({ ...statementAndLine(), views: { spec } });
  try {
    const first = rowOf(view.container, "spec#b-r1");
    const line = theLine(view.container);
    act(() => first?.focus());
    await user.keyboard("j");
    await waitFor(() => expect(document.activeElement).toBe(line));

    await user.keyboard("q");
    await waitFor(() =>
      expect(line.querySelector("[data-row-report]")?.textContent).toBe(
        KEY_REPORTS.no_next_question
      )
    );
    expect(view.container.querySelectorAll("[data-row-report]")).toHaveLength(1);

    await user.keyboard("k");
    await waitFor(() => expect(document.activeElement).toBe(first));
    expect(view.container.querySelector("[data-row-report]")).toBeNull();
  } finally {
    view.unmount();
  }
});

/** The lines of overtaken statements the Spec view draws, in the order drawn. */
function linesOf(container: HTMLElement): HTMLElement[] {
  return [...container.querySelectorAll<HTMLElement>("[data-overtaken-line]")];
}

// Two groups of statements can hang under one parent - the issue's statements before a
// question's and after it - and a line is drawn only after the group whose statements it folds,
// keyed by that group, so one press opens that line alone, and the keys walk the rows as drawn.
test("the Spec view draws a line only after the group whose statements it folds", async () => {
  const user = userEvent.setup();
  const one = drawSpec(
    stamped(
      node(KEY, "issue", [
        statement("r1"),
        statement("r2"),
        node("ask-1", "ask", [statement("r3")]),
        statement("r4"),
      ]),
      { "spec#b-r2": { life: "overtaken" } }
    )
  );
  try {
    const lines = linesOf(one.container);
    expect(lines.map((line) => line.textContent)).toEqual([`1 ${CONTROL_LABELS.overtaken_count}`]);
    const [line] = lines;
    const groups = [...one.container.querySelectorAll("[data-spec-group]")];
    expect(groups[0]?.contains(line ?? null)).toBe(true);

    // The keys walk the rows in the order they are drawn, the line among them, and stop at the
    // last row.
    const last = rowOf(one.container, "spec#b-r4");
    act(() => last?.focus());
    await user.keyboard("j");
    await waitFor(() => expect(document.activeElement).toBe(last));
    await user.keyboard("k");
    await waitFor(() => expect(document.activeElement).toBe(rowOf(one.container, "spec#b-r3")));
    await user.keyboard("k");
    await waitFor(() => expect(document.activeElement).toBe(line));
    await user.keyboard("k");
    await waitFor(() => expect(document.activeElement).toBe(rowOf(one.container, "spec#b-r1")));
  } finally {
    one.unmount();
  }

  const two = drawSpec(
    stamped(
      node(KEY, "issue", [
        statement("r1"),
        statement("r2"),
        node("ask-1", "ask", [statement("r3")]),
        statement("r4"),
        statement("r5"),
      ]),
      { "spec#b-r2": { life: "overtaken" }, "spec#b-r5": { life: "overtaken" } }
    )
  );
  try {
    const [first, second] = linesOf(two.container);
    if (first === undefined || second === undefined) throw new Error("two lines are drawn");
    await user.click(first);
    await waitFor(() => expect(first).toHaveAttribute("aria-expanded", "true"));
    expect(second).toHaveAttribute("aria-expanded", "false");
    expect(rowOf(two.container, "spec#b-r2")).not.toBeNull();
    expect(rowOf(two.container, "spec#b-r5")).toBeNull();
  } finally {
    two.unmount();
  }
});

// Under Spec a line counts the overtaken nodes its statements fold with, each once: the topmost
// one each statement stands at or under (`closedBy`), as the Verification view's line counts its
// rows. An overtaken statement that holds an overtaken one, which holds a live one, is one node of
// the count, and an overtaken statement beside it is another; opened, the line draws all four
// after the live statement beside them, in the order the spec holds them.
test("the Spec view's line counts each overtaken node its statements stand under once", async () => {
  const user = userEvent.setup();
  const view = drawSpec(
    stamped(
      node(KEY, "issue", [
        statement("r1", { children: [statement("r2", { children: [statement("r3")] })] }),
        statement("r4"),
        statement("r5"),
      ]),
      {
        "spec#b-r1": { life: "overtaken" },
        "spec#b-r2": { life: "overtaken" },
        "spec#b-r5": { life: "overtaken" },
      }
    )
  );
  try {
    const lines = linesOf(view.container);
    expect(lines.map((line) => line.textContent)).toEqual([`2 ${CONTROL_LABELS.overtaken_count}`]);
    const [line] = lines;
    if (line === undefined) throw new Error("the view draws no line");
    await user.click(line);
    await waitFor(() => expect(line).toHaveAttribute("aria-expanded", "true"));
    const drawn = [...view.container.querySelectorAll<HTMLElement>("[data-node-row]")].map(
      (row) => row.dataset.nodeId
    );
    expect(drawn).toEqual(["spec#b-r4", "spec#b-r1", "spec#b-r2", "spec#b-r3", "spec#b-r5"]);
  } finally {
    view.unmount();
  }
});

// An address that names an overtaken statement under Spec opens the line that holds it and
// puts the focus on it, and so does a request from the Contents list or a chip.
test("the Spec view opens the line of an overtaken statement an address or a request names", async () => {
  const addressed = drawSpec(statementAndLine(), { reveal: "spec#b-r2" });
  try {
    await waitFor(() =>
      expect(theLine(addressed.container)).toHaveAttribute("aria-expanded", "true")
    );
    await waitFor(() =>
      expect(document.activeElement).toBe(rowOf(addressed.container, "spec#b-r2"))
    );
  } finally {
    addressed.unmount();
  }

  const requested = drawSpec(statementAndLine(), {
    request: { focusBranch: false, id: "spec#b-r2", seq: 1 },
  });
  try {
    await waitFor(() =>
      expect(theLine(requested.container)).toHaveAttribute("aria-expanded", "true")
    );
    await waitFor(() =>
      expect(document.activeElement).toBe(rowOf(requested.container, "spec#b-r2"))
    );
  } finally {
    requested.unmount();
  }
});

// Under Spec the words filter draws a match that is an overtaken statement under its open
// line, and a line the reader closes under the words stays closed.
test("the Spec view draws an overtaken statement the words match under an open line", async () => {
  const user = userEvent.setup();
  const view = drawSpec(
    stamped(
      node(KEY, "issue", [
        statement("r1"),
        statement("r2", { text: "**R2.** The zebrafinch words are drawn." }),
      ]),
      { "spec#b-r2": { life: "overtaken" } }
    ),
    { query: "zebrafinch" }
  );
  try {
    const line = theLine(view.container);
    expect(line).toHaveAttribute("aria-expanded", "true");
    expect(rowOf(view.container, "spec#b-r2")).not.toBeNull();
    expect(rowOf(view.container, "spec#b-r1")).toBeNull();

    await user.click(line);
    await waitFor(() => expect(line).toHaveAttribute("aria-expanded", "false"));
    expect(rowOf(view.container, "spec#b-r2")).toBeNull();
  } finally {
    view.unmount();
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

// Under Spec a group's line of overtaken statements is a stop of the keys from a decision cell
// as it is from a statement: `j` from the cell beside the group's last run goes to the group's
// line, and, once the line is open, `k` from the cell beside the first run drawn under it goes
// back to the line.
test("the keys from a Spec decision cell stop on the overtaken line", async () => {
  const user = userEvent.setup();
  const view = drawSpec(statementAndLine());
  try {
    const line = theLine(view.container);
    const besideLive = cellBeside(view.container, "spec#b-r1");
    act(() => besideLive.focus());
    await user.keyboard("j");
    await waitFor(() => expect(document.activeElement).toBe(line));

    await user.click(line);
    await waitFor(() => expect(line).toHaveAttribute("aria-expanded", "true"));
    const besideFolded = cellBeside(view.container, "spec#b-r2");
    act(() => besideFolded.focus());
    await user.keyboard("k");
    await waitFor(() => expect(document.activeElement).toBe(line));
  } finally {
    view.unmount();
  }
});

// Under Spec a decision at or under an overtaken node shaped nothing that still stands: the run
// of the statement it shaped is drawn as a run no decision shaped, with no decision in its cell.
// With the decision live the cell draws it.
test("the Spec view draws no overtaken decision beside the statement it shaped", () => {
  const D1 = "decisions-md#b-d1";
  const rootWith = (states: Readonly<Record<string, NodeStates>>): TreeNode => ({
    ...stamped(specIssueTree([statement("r1"), specDecisionNode("d1")]), states),
    views: {
      spec: specViewAnswer([
        answerNode("spec#b-r1", "requirement", { mark: "query" }),
        specDecision("d1", { links: [specShaped(specStatementRef("r1"))] }),
      ]),
    },
  });
  const overtaken = drawSpec(rootWith({ [D1]: { life: "overtaken" } }));
  try {
    const cell = cellBeside(overtaken.container, "spec#b-r1");
    expect(cell).not.toHaveAttribute("data-decision-id");
    expect(cell.querySelector("[data-spec-said]")).toBeNull();
  } finally {
    overtaken.unmount();
  }

  const live = drawSpec(rootWith({}));
  try {
    expect(cellBeside(live.container, "spec#b-r1")).toHaveAttribute("data-decision-id", D1);
  } finally {
    live.unmount();
  }
});

// Under Spec, while the asks read is out, a person's decision that heads a statement's run waits
// on it, and keeps its place where the statement's group also holds an overtaken decision: the
// cell beside the statement draws no decision, and no other decision that informed the statement
// moves up in its place. The overtaken decision draws no cell either.
test("a Spec run headed by a person's decision that waits on the asks read keeps its head beside an overtaken decision", () => {
  const root: TreeNode = {
    ...stamped(
      specIssueTree([
        statement("r1"),
        statement("r2"),
        specDecisionNode("d1"),
        specDecisionNode("d2"),
        specDecisionNode("d3"),
      ]),
      { "decisions-md#b-d3": { life: "overtaken" } }
    ),
    views: {
      spec: specViewAnswer([
        answerNode("spec#b-r1", "requirement", { mark: "query" }),
        answerNode("spec#b-r2", "requirement", { mark: "query" }),
        specDecision("d1", { kind: "person", links: [specShaped(specStatementRef("r1"))] }),
        specDecision("d2", { links: [specShaped(specStatementRef("r1"))] }),
        specDecision("d3", { links: [specShaped(specStatementRef("r2"))] }),
      ]),
    },
  };
  const view = drawSpec(root, { asksOut: true });
  try {
    expect(cellBeside(view.container, "spec#b-r1")).not.toHaveAttribute("data-decision-id");
    expect(cellBeside(view.container, "spec#b-r2")).not.toHaveAttribute("data-decision-id");
  } finally {
    view.unmount();
  }
});

// Under Spec a decision at or under an overtaken node is left out before any run is headed, so
// the run of a statement whose ties are an overtaken decision, then a person's decision that waits
// on the asks read, then a live decision, is the person's: while the asks read is out its cell
// draws no decision, and the live decision does not move up to head it.
test("a Spec run waits on a person's decision that stands after an overtaken one", () => {
  const root: TreeNode = {
    ...stamped(
      specIssueTree([
        statement("r1"),
        specDecisionNode("d1"),
        specDecisionNode("d2"),
        specDecisionNode("d3"),
      ]),
      { "decisions-md#b-d1": { life: "overtaken" } }
    ),
    views: {
      spec: specViewAnswer([
        answerNode("spec#b-r1", "requirement", { mark: "query" }),
        specDecision("d1", { links: [specShaped(specStatementRef("r1"))] }),
        specDecision("d2", { kind: "person", links: [specShaped(specStatementRef("r1"))] }),
        specDecision("d3", { links: [specShaped(specStatementRef("r1"))] }),
      ]),
    },
  };
  const view = drawSpec(root, { asksOut: true });
  try {
    expect(cellBeside(view.container, "spec#b-r1")).not.toHaveAttribute("data-decision-id");
  } finally {
    view.unmount();
  }
});

// Under Spec a person's decision whose own node the tree does not hold is drawn by the question
// they answered, and stands where that question stands: with the question overtaken it heads no
// run, and the live decision that shaped the statement after it heads the statement's run.
test("a Spec run is not headed by a person's decision drawn by an overtaken question", () => {
  const root: TreeNode = {
    ...stamped(
      specIssueTree([
        statement("r1"),
        answeredAsk("ask-1", { at: "2026-11-14T09:00:00Z", text: "Draw it." }),
        specDecisionNode("d2"),
      ]),
      { "ask-1": { life: "overtaken" } }
    ),
    views: {
      spec: specViewAnswer([
        answerNode("spec#b-r1", "requirement", { mark: "query" }),
        specDecision("d1", {
          asks: ["ask-1"],
          kind: "person",
          links: [specShaped(specStatementRef("r1"))],
        }),
        specDecision("d2", { links: [specShaped(specStatementRef("r1"))] }),
      ]),
    },
  };
  const view = drawSpec(root);
  try {
    expect(cellBeside(view.container, "spec#b-r1")).toHaveAttribute(
      "data-decision-id",
      "decisions-md#b-d2"
    );
  } finally {
    view.unmount();
  }
});

// Under Spec an opened line draws the statements it holds run by run as their decisions shaped
// them, a run that waits on the asks read included: a folded statement whose run waits on a
// person's decision keeps a run of its own, which draws no decision while that read is out, though
// a live decision informed it too and another statement of the line was shaped by an overtaken
// decision.
test("an opened Spec line keeps a run that waits on the asks read", async () => {
  const user = userEvent.setup();
  const root: TreeNode = {
    ...stamped(
      specIssueTree([
        statement("r1"),
        statement("r2"),
        statement("r3"),
        statement("r4"),
        specDecisionNode("d1"),
        specDecisionNode("d2"),
        specDecisionNode("d3"),
      ]),
      {
        "decisions-md#b-d1": { life: "overtaken" },
        "spec#b-r2": { life: "overtaken" },
        "spec#b-r3": { life: "overtaken" },
        "spec#b-r4": { life: "overtaken" },
      }
    ),
    views: {
      spec: specViewAnswer([
        answerNode("spec#b-r1", "requirement", { mark: "query" }),
        // The view read's own state, which the view's rows carry.
        ...["r2", "r3", "r4"].map((block) =>
          answerNode(`spec#b-${block}`, "requirement", { mark: "query", state: "overtaken" })
        ),
        specDecision("d1", { links: [specShaped(specStatementRef("r4"))] }),
        specDecision("d2", { kind: "person", links: [specShaped(specStatementRef("r2"))] }),
        specDecision("d3", { links: [specShaped(specStatementRef("r2"))] }),
      ]),
    },
  };
  const view = drawSpec(root, { asksOut: true });
  try {
    const line = theLine(view.container);
    await user.click(line);
    await waitFor(() => expect(line).toHaveAttribute("aria-expanded", "true"));
    const waits = cellBeside(view.container, "spec#b-r2");
    expect(waits).not.toHaveAttribute("data-decision-id");
    expect(waits).toHaveAttribute("data-spec-statements", "spec#b-r2");
  } finally {
    view.unmount();
  }
});

// Under Spec a closed statement folds out of the runs before they are built: two statements one
// decision shaped, with an overtaken statement between them, are one run beside one cell of that
// decision, as they are with no statement between them.
test("a Spec run of one decision is one run across an overtaken statement", () => {
  const D1 = "decisions-md#b-d1";
  const root: TreeNode = {
    ...stamped(
      specIssueTree([statement("r1"), statement("r2"), statement("r3"), specDecisionNode("d1")]),
      { "spec#b-r2": { life: "overtaken" } }
    ),
    views: {
      spec: specViewAnswer([
        answerNode("spec#b-r1", "requirement", { mark: "query" }),
        answerNode("spec#b-r2", "requirement", { mark: "query", state: "overtaken" }),
        answerNode("spec#b-r3", "requirement", { mark: "query" }),
        specDecision("d1", {
          links: [specShaped(specStatementRef("r1")), specShaped(specStatementRef("r3"))],
        }),
      ]),
    },
  };
  const view = drawSpec(root);
  try {
    const cells = view.container.querySelectorAll(`[data-spec-cell][data-decision-id="${D1}"]`);
    expect(cells).toHaveLength(1);
    expect(cells[0]).toHaveAttribute("data-spec-statements", "spec#b-r1 spec#b-r3");
  } finally {
    view.unmount();
  }
});

// Under Spec a line the words drew open closes with no press once the reader clears the words.
// The statement under it that held the tree's one tab stop is then no row, so the stop goes to
// the line that now folds it, and not to the first statement drawn.
test("a Spec line the words drew open takes the tab stop of its statement when the words go", async () => {
  const view = drawSpec(
    stamped(
      node(KEY, "issue", [
        statement("r1"),
        statement("r2", { text: "**R2.** The zebrafinch words are drawn." }),
      ]),
      { "spec#b-r2": { life: "overtaken" } }
    ),
    { query: "zebrafinch" }
  );
  try {
    expect(theLine(view.container)).toHaveAttribute("aria-expanded", "true");
    act(() => rowOf(view.container, "spec#b-r2")?.focus());
    await waitFor(() =>
      expect(rowOf(view.container, "spec#b-r2")).toHaveAttribute("tabindex", "0")
    );

    view.go("/");
    await waitFor(() => expect(rowOf(view.container, "spec#b-r1")).not.toBeNull());
    expect(theLine(view.container)).toHaveAttribute("aria-expanded", "false");
    expect(rowOf(view.container, "spec#b-r2")).toBeNull();
    await waitFor(() => expect(theLine(view.container)).toHaveAttribute("tabindex", "0"));
    expect(rowOf(view.container, "spec#b-r1")).toHaveAttribute("tabindex", "-1");
  } finally {
    view.unmount();
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

/** `chip` drawn by `NodeLink` on this issue's route, so a chip with an id is a link. */
function drawChip(chip: LensChip) {
  return render(
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

/** One node of the tree read's answer as the contract gives it (`answerNode`): live and visible
 *  to every reader unless the case says otherwise, under the issue unless it names another
 *  parent, with words of its own. */
function readNode(
  id: string,
  supertag: string,
  extra: Partial<TreeAnswerNode> = {}
): TreeAnswerNode {
  return answerNode(id, supertag, { text: `Words of ${id}.`, ...extra });
}

/** A typed block of the issue's document `slug`, at `position` in it, which the tree draws by a
 *  tree id (`<slug>#b-<id>`) that is not its read id. */
function readBlock(
  id: string,
  supertag: string,
  slug: string,
  position: number,
  extra: Partial<TreeAnswerNode> = {}
): TreeAnswerNode {
  return readNode(id, supertag, {
    document: { artifact_id: `artifact-${slug}`, position, slug },
    ...extra,
  });
}

/** The issue's own node, which every node of a tree read hangs from. */
const ISSUE_NODE = readNode(KEY, "issue", { parent: null });

/** One answer of the tree read, or of one of its views, every page merged. */
function readAnswer(nodes: readonly TreeAnswerNode[]): TreeAnswer {
  return {
    converted: true,
    cursor: null,
    nodes: [...nodes],
    tree: "artifact-tree",
    tuples: [],
    view_of: null,
  };
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
 *  kind `kind` given the n-th values is read by the id `<kind>-<n>`. */
function eachKind(values: readonly Partial<TreeAnswerNode>[]): TreeAnswerNode[] {
  return READ_KINDS.flatMap(([kind, slug], k) =>
    values.map((extra, n) =>
      slug === undefined
        ? readNode(`${kind}-${n}`, kind, extra)
        : readBlock(`${kind}-${n}`, kind, slug, k * values.length + n, extra)
    )
  );
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
    const proposed = readNodeOf(root, `${kind}-0`);
    const overtaken = readNodeOf(root, `${kind}-1`);
    expect(proposed?.type, kind).toBe(kind);
    if (slug !== undefined) expect(proposed?.id, kind).toBe(`${slug}#b-${kind}-0`);
    expect(markKeys(proposed), kind).toEqual(["life"]);
    expect(proposed?.life, kind).toBe("proposed");
    expect(markKeys(overtaken), kind).toEqual(["closed", "life"]);
    expect(overtaken?.life, kind).toBe("overtaken");
    for (const id of [`${kind}-2`, `${kind}-3`]) {
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
    expect(markKeys(readNodeOf(root, `${kind}-0`)), kind).toEqual(["onlyYou"]);
    expect(readNodeOf(root, `${kind}-0`)?.onlyYou, kind).toBe(true);
    expect(markKeys(readNodeOf(root, `${kind}-1`)), kind).toEqual(["life", "onlyYou"]);
    expect(markKeys(readNodeOf(root, `${kind}-2`)), kind).toEqual([]);
  }
  const drawn = render(createElement(NodeMarks, { marks: readNodeOf(root, "decision-0") ?? {} }));
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
    readBlock("r1", "requirement", "spec", 0, { mark: "query", state: "proposed" }),
    readBlock("r2", "requirement", "spec", 1, { mark: "query" }),
    readBlock("r3", "requirement", "spec", 2, { mark: "query", visibility: "author" }),
  ]);
  expect(markKeys(readNodeOf(outline, "r1"))).toEqual([]);
  expect(readNodeOf(outline, "r2")?.life).toBe("proposed");

  const view = presetView({ ...outline, views: { spec } }, "spec", "").root;
  expect(view.children.map((row) => [row.readId, markKeys(row), row.life, row.onlyYou])).toEqual([
    ["r1", ["life"], "proposed", undefined],
    ["r2", [], undefined, undefined],
    ["r3", ["onlyYou"], undefined, true],
  ]);
});

// The Plan view answers a plan task's parent statement by its id, kind and first line
// alone (`parent_line`), and nothing of its state. The parent line names the statement by its
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
  const lineOf = (id: string) => ({ id, line: `Words of ${id}.`, supertag: "requirement" });
  const plan = readAnswer([
    readBlock("t7", "plan_task", "plan-md", 0, {
      mark: "query",
      parent: "r7",
      parent_line: lineOf("r7"),
    }),
    readBlock("t8", "plan_task", "plan-md", 1, {
      mark: "query",
      parent: "r8",
      parent_line: lineOf("r8"),
    }),
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
    ["t7", [], "r7", "proposed", true],
    ["t8", [], "r8", undefined, undefined],
  ]);
});

// A statement the Verification view's answer holds live, which the tree read with no
// view answered overtaken, is live in the view: its row carries no mark and is not closed, and
// neither is the check under it, so the view folds neither into a line nor leaves either out
// of a count. Only a node the view's answer does not hold takes the outline's marks.
test("a node the view holds live stays live whatever the outline says", () => {
  const outline = outlineOf([
    readBlock("r1", "requirement", "spec", 0, { state: "overtaken" }),
    readBlock("c1", "check", "verification-md", 0, { parent: "r1" }),
    readBlock("r2", "requirement", "spec", 1),
  ]);
  const verification = readAnswer([
    readBlock("r1", "requirement", "spec", 0, { mark: "context" }),
    readBlock("c1", "check", "verification-md", 0, { mark: "query", parent: "r1" }),
    readBlock("r2", "requirement", "spec", 1, { mark: "context" }),
  ]);
  expect(markKeys(readNodeOf(outline, "r1"))).toEqual(["closed", "life"]);
  expect(markKeys(readNodeOf(outline, "c1"))).toEqual(["closed"]);

  const view = presetView({ ...outline, views: { verification } }, "verification", "").root;
  for (const readId of ["r1", "c1", "r2"]) {
    expect(readNodeOf(view, readId), readId).toBeDefined();
    expect(markKeys(readNodeOf(view, readId)), readId).toEqual([]);
  }
  expect(linesIn(view)).toEqual([]);
});

// A statement whose parent in the tree read is an overtaken question, which the Spec view draws
// no row for, stands under that question in a view's held read as it does in the outline: it is
// closed in the Spec and the Verification views alike, draws no gap, "No check" included, and
// under Spec folds to its group's line, which opens on it. Under Verification it folds as an
// overtaken statement does, to the issue's line, which counts the question once and holds it
// once opened, by a press or by an address that names it. With nothing overtaken it is a row
// with the mark its check gives it, as the statement beside it is.
test("a statement under an overtaken question a view draws no row for draws no No check", async () => {
  const user = userEvent.setup();
  const R3 = "spec#b-r3";
  const rootWith = (asked: Partial<TreeAnswerNode>): TreeNode => {
    const outline = outlineOf([
      readBlock("r1", "requirement", "spec", 0),
      readBlock("c1", "check", "verification-md", 0, { parent: "r1" }),
      readNode("ask-1", "ask", asked),
      readBlock("r3", "requirement", "spec", 1, { parent: "ask-1" }),
      readBlock("c3", "check", "verification-md", 1, { parent: "r3" }),
    ]);
    const asks = { id: "ask-1", line: "Words of ask-1.", supertag: "ask" };
    const spec = readAnswer([
      readBlock("r1", "requirement", "spec", 0, { mark: "query" }),
      readBlock("r3", "requirement", "spec", 1, {
        mark: "query",
        parent: "ask-1",
        parent_line: asks,
      }),
    ]);
    const verification = readAnswer([
      readBlock("r1", "requirement", "spec", 0, { mark: "context" }),
      readBlock("c1", "check", "verification-md", 0, { mark: "query", parent: "r1" }),
      readBlock("r3", "requirement", "spec", 1, {
        mark: "context",
        parent: "ask-1",
        parent_line: asks,
      }),
      readBlock("c3", "check", "verification-md", 1, { mark: "query", parent: "r3" }),
    ]);
    return { ...outline, views: { spec, verification } };
  };
  const closedIn = (root: TreeNode, preset: "spec" | "verification") =>
    readNodeOf(presetView(root, preset, "").root, "r3")?.closed === true;

  const overtaken = rootWith({ state: "overtaken" });
  const detail = drawSpec(overtaken);
  try {
    expect(detail.container.querySelector('[data-spec-mark="no_check"]')).toBeNull();
    expect(rowOf(detail.container, "spec#b-r1")).toHaveAttribute("data-spec-mark", "unknown");
    expect(rowOf(detail.container, R3)).toBeNull();
    const line = theLine(detail.container);
    await user.click(line);
    await waitFor(() => expect(line).toHaveAttribute("aria-expanded", "true"));
    expect(rowOf(detail.container, R3)).not.toBeNull();
    expect(rowOf(detail.container, R3)).not.toHaveAttribute("data-spec-mark");
  } finally {
    detail.unmount();
  }
  expect([closedIn(overtaken, "spec"), closedIn(overtaken, "verification")]).toEqual([true, true]);

  const verification = drawVerification(overtaken, []);
  try {
    expect(rowOf(verification.container, "spec#b-r1")).not.toBeNull();
    expect(rowOf(verification.container, R3)).toBeNull();
    const line = theLine(verification.container);
    expect(line).toHaveAttribute("aria-expanded", "false");
    expect(line.textContent).toBe(`1 ${CONTROL_LABELS.overtaken_count}`);
    await user.click(line);
    expect(line).toHaveAttribute("aria-expanded", "true");
    const folded = rowOf(verification.container, R3);
    expect(folded !== null && line.contains(folded)).toBe(true);
    expect(folded).toHaveAttribute("data-statement-state", "unknown");
    expect(folded?.querySelector("[data-gap]")).toBeNull();
  } finally {
    verification.unmount();
  }
  const addressed = drawVerification(overtaken, [], "", `/issues/${KEY}/verification#b-r3`);
  try {
    const line = theLine(addressed.container);
    await waitFor(() => expect(line).toHaveAttribute("aria-expanded", "true"));
    const named = rowOf(addressed.container, R3);
    expect(named !== null && line.contains(named)).toBe(true);
  } finally {
    addressed.unmount();
  }

  const live = rootWith({});
  const control = drawSpec(live);
  try {
    expect(rowOf(control.container, "spec#b-r1")).toHaveAttribute("data-spec-mark", "unknown");
    expect(rowOf(control.container, R3)).toHaveAttribute("data-spec-mark", "unknown");
    expect(control.container.querySelector("[data-overtaken-line]")).toBeNull();
  } finally {
    control.unmount();
  }
  expect([closedIn(live, "spec"), closedIn(live, "verification")]).toEqual([false, false]);
});

// Under Spec the words filter lifts a match under the line of a parent the view's answer holds,
// where the outline holds that parent under an overtaken question the view draws nothing of. The
// match stands under that parent all the same: it is closed, as it is with no words and in the
// outline, so it draws no "No check" and folds to its group's line, which the words draw open.
test("a words match lifted under a parent the outline holds under an overtaken node is closed", async () => {
  const R3 = "spec#b-r3";
  const outline = outlineOf([
    readBlock("r1", "requirement", "spec", 0, { text: "The page draws r1." }),
    readNode("ask-1", "ask", { state: "overtaken" }),
    readBlock("r2", "requirement", "spec", 1, { parent: "ask-1", text: "The page draws r2." }),
    readBlock("r3", "requirement", "spec", 2, { parent: "r2", text: "A zebra crosses r3." }),
  ]);
  const spec = readAnswer([
    readBlock("r1", "requirement", "spec", 0, { mark: "query", text: "The page draws r1." }),
    readBlock("r2", "requirement", "spec", 1, {
      mark: "query",
      parent: "ask-1",
      parent_line: { id: "ask-1", line: "Words of ask-1.", supertag: "ask" },
      text: "The page draws r2.",
    }),
    readBlock("r3", "requirement", "spec", 2, {
      mark: "query",
      parent: "r2",
      text: "A zebra crosses r3.",
    }),
  ]);
  const root = { ...outline, views: { spec } };
  const r3Under = (query: string) => readNodeOf(presetView(root, "spec", query).root, "r3");
  expect(readNodeOf(outline, "r3")?.closed).toBe(true);
  expect(r3Under("")?.closed).toBe(true);
  expect(r3Under("zebra")?.parentLine?.id).toBe("spec#b-r2");
  expect(r3Under("zebra")?.closed).toBe(true);

  const view = drawSpec(root, { query: "zebra" });
  try {
    await waitFor(() => expect(rowOf(view.container, R3)).not.toBeNull());
    expect(theLine(view.container)).toHaveAttribute("aria-expanded", "true");
    expect(rowOf(view.container, R3)).not.toHaveAttribute("data-spec-mark");
    expect(view.container.querySelector('[data-spec-mark="no_check"]')).toBeNull();
  } finally {
    view.unmount();
  }
});

// Under Verification the issue's line counts each overtaken node its rows fold with once: the
// topmost one each stands at or under, as the tree holds it. Two statements under two live
// questions of one overtaken comment fold with that comment alone, though each carries its own
// question as its parent line, so the line says "1 overtaken".
test("the Verification issue's line counts the overtaken node its statements stand under once", () => {
  const asked = (id: string) => ({ id, line: `Words of ${id}.`, supertag: "ask" });
  const outline = outlineOf([
    readBlock("r1", "requirement", "spec", 0),
    readBlock("c1", "check", "verification-md", 0, { parent: "r1" }),
    readNode("com-1", "comment", { state: "overtaken" }),
    readNode("ask-1", "ask", { parent: "com-1" }),
    readNode("ask-2", "ask", { parent: "com-1" }),
    readBlock("r3", "requirement", "spec", 1, { parent: "ask-1" }),
    readBlock("c3", "check", "verification-md", 1, { parent: "r3" }),
    readBlock("r4", "requirement", "spec", 2, { parent: "ask-2" }),
    readBlock("c4", "check", "verification-md", 2, { parent: "r4" }),
  ]);
  const verification = readAnswer([
    readBlock("r1", "requirement", "spec", 0, { mark: "context" }),
    readBlock("c1", "check", "verification-md", 0, { mark: "query", parent: "r1" }),
    readBlock("r3", "requirement", "spec", 1, {
      mark: "context",
      parent: "ask-1",
      parent_line: asked("ask-1"),
    }),
    readBlock("c3", "check", "verification-md", 1, { mark: "query", parent: "r3" }),
    readBlock("r4", "requirement", "spec", 2, {
      mark: "context",
      parent: "ask-2",
      parent_line: asked("ask-2"),
    }),
    readBlock("c4", "check", "verification-md", 2, { mark: "query", parent: "r4" }),
  ]);
  const root = { ...outline, views: { verification } };
  const drawn = drawVerification(root, []);
  try {
    expect(linesOf(drawn.container).map((line) => line.textContent)).toEqual([
      `1 ${CONTROL_LABELS.overtaken_count}`,
    ]);
  } finally {
    drawn.unmount();
  }

  // Each statement folds with the comment, which the tree read gives as the topmost overtaken
  // node it stands under, in the outline and in the view alike.
  const comment = readNodeOf(outline, "com-1")?.id;
  expect(comment).toBeDefined();
  expect(readNodeOf(outline, "r3")?.closedBy).toBe(comment);
  const view = presetView(root, "verification", "").root;
  expect(["r3", "r4"].map((id) => readNodeOf(view, id)?.parentLine?.id)).toEqual([
    "ask-1",
    "ask-2",
  ]);
  expect(["r3", "r4"].map((id) => readNodeOf(view, id)?.closedBy)).toEqual([comment, comment]);
});

/** Where the page's address stands, drawn by a probe that runs `useOvertakenTarget` as the
 *  stage runs it, on the route the address names. */
function AddressProbe(): ReactNode {
  const { hash, pathname, search } = useLocation();
  useOvertakenTarget(ISSUE, parseIssuePath(pathname, search) ?? { key: KEY, kind: "issue" });
  return createElement("output", { "data-address": `${pathname}${search}${hash}` });
}

/** The page at `address`, with `tree` the tree read the page holds and no other read. */
function drawAddress(address: string, tree: TreeAnswer) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(issueTreeKey(KEY), tree);
  return render(
    createElement(
      QueryClientProvider,
      { client },
      createElement(MemoryRouter, { initialEntries: [address] }, createElement(AddressProbe))
    )
  );
}

/** The address the probe last drew. */
function addressOf(container: HTMLElement): string | null | undefined {
  return container.querySelector("[data-address]")?.getAttribute("data-address");
}

/** A tree read holding an overtaken decision with a question under it, and a live decision. */
function overtakenDecision(): TreeAnswer {
  return readAnswer([
    ISSUE_NODE,
    readBlock("d1", "decision", "decisions-md", 0, { state: "overtaken" }),
    readNode("a1", "ask", { parent: "d1" }),
    readBlock("d2", "decision", "decisions-md", 1),
  ]);
}

// The Decisions view draws an overtaken decision, and each node under one, nowhere, so an address
// naming one under that view is replaced by the same address under no filter, its search and its
// fragment kept, where the outline opens the line that holds it. Under another view, or naming a
// live decision, the address stays. The tree read is the one the page already holds: nothing is
// read for it.
test("an address naming an overtaken decision under the Decisions view is replaced by the same address under no filter", async () => {
  const fetched = spyOn(globalThis, "fetch").mockRejectedValue(new Error("no request"));
  const reads = [
    spyOn(api, "getIssue").mockRejectedValue(new Error("no request")),
    spyOn(api, "getIssueDecisions").mockRejectedValue(new Error("no request")),
    spyOn(api, "getIssueTree").mockRejectedValue(new Error("no request")),
  ];
  const cases: readonly (readonly [string, string])[] = [
    [`/issues/${KEY}/decisions#b-d1`, `/issues/${KEY}#b-d1`],
    [`/issues/${KEY}/decisions?q=words#b-d1`, `/issues/${KEY}?q=words#b-d1`],
    [`/issues/${KEY}/decisions?ask=a1`, `/issues/${KEY}?ask=a1`],
    [`/issues/${KEY}/decisions#b-d2`, `/issues/${KEY}/decisions#b-d2`],
    [`/issues/${KEY}/plan#b-d1`, `/issues/${KEY}/plan#b-d1`],
  ];
  try {
    for (const [address, landed] of cases) {
      const view = drawAddress(address, overtakenDecision());
      try {
        await waitFor(() => expect(addressOf(view.container), address).toBe(landed));
        await act(async () => {});
        expect(addressOf(view.container), address).toBe(landed);
      } finally {
        view.unmount();
      }
    }
    expect(fetched).not.toHaveBeenCalled();
    for (const read of reads) expect(read).not.toHaveBeenCalled();
  } finally {
    fetched.mockRestore();
    for (const read of reads) read.mockRestore();
  }
});

/** The marks drawn under `element`, by their tokens, in the order they are drawn. */
function marksIn(element: Element | null | undefined): (string | undefined)[] {
  return [...(element?.querySelectorAll<HTMLElement>("[data-node-mark]") ?? [])].map(
    (mark) => mark.dataset.nodeMark
  );
}

/** The Decisions view over `root`, with `made` the nodes of the decisions view the page holds on
 *  the issue's node (`TreeNode.views`), drawn as its slot draws it
 *  (`DecisionsFilterBody.drawn.test.tsx`'s `renderBody`): no words, and an address that names
 *  the nodes `named`, which the view keeps whatever it draws. */
function drawDecisions(
  root: TreeNode,
  made: readonly TreeAnswerNode[],
  named: readonly string[] = []
) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const held = decisionsAnswer(made);
  client.setQueryData(issueTreeKey(KEY), held);
  client.setQueryData(viewKey(KEY, "decisions"), held);
  client.setQueryData(["asks", KEY], []);
  client.setQueryData(["comments", KEY], []);
  const issue = decisionsIssue();
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
                reader: READER,
                root: { ...root, views: { decisions: held } },
                view: presetView(root, "decisions", "", new Set(named)).root,
              })
            )
          )
        )
      )
    )
  );
}

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
      links: [shaped(specRef("s7")), shaped(specRef("s1"))],
      waiting_review: false,
    }),
    decision("waiting", { links: [shaped(specRef("s1"))] }),
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

// A person's decision that restates an answer ties the answered question to the statements it
// shaped: the question's row draws them, each with the marks of the statement itself, since the
// question's own chips are none of the decision's ties.
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
    decision("restate", { asks: ["a-put"], kind: "person", links: [shaped(specRef("s7"))] }),
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

// An overtaken decision is drawn nowhere under Decisions, and that holds for one that restates
// an answer too: the answered question draws none of the statements it shaped.
test("under Decisions an answered question draws no tie of an overtaken decision that restates it", () => {
  const restate = decisionNode("restate");
  const root = stamped(
    node(KEY, "issue", [
      specStatement("s1"),
      answeredAsk("a-put", { at: "2026-11-14T09:00:00Z", text: "Keep the rail." }),
      restate,
    ]),
    { [restate.id]: { life: "overtaken" } }
  );
  const view = drawDecisions(root, [
    decision("restate", { asks: ["a-put"], kind: "person", links: [shaped(specRef("s1"))] }),
  ]);
  try {
    const asked = rowOf(view.container, "a-put");
    expect(asked).not.toBeNull();
    expect(
      asked?.querySelector(`[data-decision-statement="${specStatement("s1").id}"]`)
    ).toBeNull();
  } finally {
    view.unmount();
  }
});

// The Decisions view draws no overtaken question or decision, nor any node under one, and the
// tree its keys walk holds none either: `q` goes past an overtaken open question to the live one
// after it, and `w` on a statement goes past an overtaken decision that names it to a live one
// that names it too, rather than to a node the view draws nowhere.
test("under Decisions `q` and `w` go past an overtaken question and decision", async () => {
  const user = userEvent.setup();
  const statementId = specStatement("s1").id;
  const past = decisionNode("d-past", { chips: [chip(specRef("s1"))] });
  const live = decisionNode("d-live", { chips: [chip(specRef("s1"))] });
  const root = stamped(
    node(KEY, "issue", [specStatement("s1"), openAsk("q-past"), openAsk("q-live"), past, live]),
    { "q-past": { life: "overtaken" }, [past.id]: { life: "overtaken" } }
  );
  const view = drawDecisions(
    root,
    [
      decision("d-past", { links: [shaped(specRef("s1"))] }),
      decision("d-live", { links: [shaped(specRef("s1"))] }),
    ],
    [statementId]
  );
  try {
    expect(rowOf(view.container, "q-past")).toBeNull();
    expect(rowOf(view.container, past.id)).toBeNull();
    expect(rowOf(view.container, "q-live")).not.toBeNull();
    expect(rowOf(view.container, live.id)).not.toBeNull();

    await user.keyboard("q");
    await waitFor(() => expect(focusedRow(view.container)).toBe("q-live"));

    act(() => rowOf(view.container, statementId)?.focus());
    expect(focusedRow(view.container)).toBe(statementId);
    await user.keyboard("w");
    await waitFor(() => expect(focusedRow(view.container)).toBe(live.id));
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
 *  words filter left of them, the chain down to the node the address names, and the address's
 *  search, which carries the words (`?q=`); with `take`, the page's `navigate` handed to it, so a
 *  case moves the address as the reader does. */
interface PlanPlace {
  readonly plan?: readonly TreeNode[];
  readonly reveal?: readonly TreeNode[];
  readonly search?: string;
  readonly take?: (navigate: (to: string) => void) => void;
}

/** The Plan view over `root`, the view's tasks, drawn as its slot draws it
 *  (`PlanFilterBody.test.tsx`'s `renderBody`), with no document read, so the plan's own order is
 *  the order the view hands the tasks over in. */
function planBody(
  root: TreeNode,
  client: QueryClient,
  { plan = root.children, reveal, search = "", take }: PlanPlace
): ReactNode {
  return createElement(
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
            createElement(PlanFilterBody, {
              artifacts: new Map(),
              context: new Set<string>(),
              emptyLine: EMPTY_STATE_LINES.plan,
              filtering: true,
              focusReveal: true,
              holdsEscape: search !== "",
              isClosed: false,
              onEditing: () => {},
              plan,
              planBlocks: undefined,
              reads: { state: "answered" },
              reveal,
              root,
              rootRow: false,
            })
          )
        )
      )
    )
  );
}

/** `planBody` drawn, with `again` to draw it once more at another place, as the page does when
 *  its address moves. */
function drawPlan(root: TreeNode, place: PlanPlace = {}) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const view = render(planBody(root, client, place));
  return { ...view, again: (next: PlanPlace) => view.rerender(planBody(root, client, next)) };
}

/** Each row the view draws, by its node's id, and each line of overtaken tasks, as `line`, in
 *  the order drawn. */
function placesDrawn(container: HTMLElement): string[] {
  return [...container.querySelectorAll<HTMLElement>("[data-node-row], [data-overtaken-line]")].map(
    (place) => place.dataset.nodeId ?? "line"
  );
}

// In the Plan view an overtaken task has no position: the plan's order leaves it
// out, so no step stands at its place. The steps draw it after every step, as the one line its
// issue's overtaken tasks fold to, which a press opens: the task is then a row under it, marked
// Overtaken.
test("the Plan view gives an overtaken step no position and folds it after the steps", async () => {
  readWide();
  const root = planWithOvertaken();
  expect(
    planOrder(root.children).map((entry) => (entry.kind === "step" ? entry.task.id : "cycle"))
  ).toEqual([T1, T3]);
  const user = userEvent.setup();
  const view = drawPlan(root);
  try {
    await user.click(view.getByRole("button", { name: PLAN_WORDS.steps }));
    expect(view.container.querySelectorAll("[data-plan-chart]")).toHaveLength(0);
    expect(view.container.querySelectorAll("[data-overtaken-line]")).toHaveLength(1);
    expect(placesDrawn(view.container)).toEqual([T1, T3, "line"]);
    expect(theLine(view.container).textContent).toBe(`1 ${CONTROL_LABELS.overtaken_count}`);
    expect(theLine(view.container)).toHaveAttribute("aria-expanded", "false");

    await user.click(theLine(view.container));
    expect(theLine(view.container)).toHaveAttribute("aria-expanded", "true");
    expect(placesDrawn(view.container)).toEqual([T1, T3, "line", T2]);
    expect(
      rowOf(view.container, T2)?.querySelector('[data-node-mark="overtaken"]')?.textContent
    ).toContain(ROW_MARK_WORDS.overtaken);
  } finally {
    view.unmount();
  }
});

// The Plan chart draws an overtaken task as no bar, since it has no position: one line
// after every bar says how many there are, a treeitem with `aria-expanded` that the chart's keys
// walk to after the last bar, and the chart's one tab stop once the walk is on it. Enter or a
// press opens it: the task is then a row after it, marked Overtaken, which the walk and End reach
// next. A press closes it again.
test("the Plan chart folds an overtaken task into one line after the bars", async () => {
  readWide();
  const user = userEvent.setup();
  const view = drawPlan(planWithOvertaken());
  try {
    expect(
      [...view.container.querySelectorAll("[data-plan-bar-row]")].map((bar) =>
        bar.getAttribute("data-node-id")
      )
    ).toEqual([T1, T3]);
    expect(view.container.querySelector(`[data-plan-bar="${T2}"]`)).toBeNull();
    expect(placesDrawn(view.container)).toEqual([T1, T3, "line"]);
    const line = theLine(view.container);
    expect(line).toHaveAttribute("role", "treeitem");
    expect(line).toHaveAttribute("aria-expanded", "false");
    expect(line.textContent).toBe(`1 ${CONTROL_LABELS.overtaken_count}`);
    const stops = (): (Element | null)[] => [
      ...view.container.querySelectorAll('[role="tree"] [tabindex="0"]'),
    ];
    expect(stops()).toEqual([rowOf(view.container, T1)]);

    // The walk goes from the last bar to the line, which takes the one tab stop.
    act(() => rowOf(view.container, T1)?.focus());
    await user.keyboard("j");
    expect(document.activeElement).toBe(rowOf(view.container, T3));
    await user.keyboard("j");
    expect(document.activeElement).toBe(line);
    expect(stops()).toEqual([line]);

    // Enter opens it in place, and the walk goes on into the task it holds.
    await user.keyboard("{Enter}");
    expect(line).toHaveAttribute("aria-expanded", "true");
    expect(placesDrawn(view.container)).toEqual([T1, T3, "line", T2]);
    const opened = rowOf(view.container, T2);
    expect(opened?.querySelector('[data-node-mark="overtaken"]')?.textContent).toContain(
      ROW_MARK_WORDS.overtaken
    );
    expect(opened?.querySelector("[data-plan-bar]")).toBeNull();
    await user.keyboard("j");
    expect(document.activeElement).toBe(opened);
    expect(stops()).toEqual([opened]);
    await user.keyboard("k");
    expect(document.activeElement).toBe(line);
    await user.keyboard("{End}");
    expect(document.activeElement).toBe(opened);

    // A press closes it, and the line keeps the one tab stop; a press opens it again.
    await user.click(line);
    expect(line).toHaveAttribute("aria-expanded", "false");
    expect(rowOf(view.container, T2)).toBeNull();
    expect(stops()).toEqual([line]);
    await user.click(line);
    expect(line).toHaveAttribute("aria-expanded", "true");
    expect(rowOf(view.container, T2)).not.toBeNull();
  } finally {
    view.unmount();
  }
});

// An address naming an overtaken task opens the line that holds it and puts the focus on the
// task's row there, in the chart and, where the address moves while the reader reads the steps,
// in the steps, though the outline holds the task under a statement the view draws as no row.
// Under the words filter the line that holds a match is drawn open, in both.
test("the Plan view opens the line of an overtaken task an address or the words name", async () => {
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
    expect(theLine(chart.container)).toHaveAttribute("aria-expanded", "true");
  } finally {
    chart.unmount();
  }

  const steps = drawPlan(root);
  try {
    await user.click(steps.getByRole("button", { name: PLAN_WORDS.steps }));
    expect(rowOf(steps.container, T2)).toBeNull();
    steps.again({ reveal: addressed });
    await waitFor(() => expect(document.activeElement).toBe(rowOf(steps.container, T2)));
    expect(theLine(steps.container)).toHaveAttribute("aria-expanded", "true");
  } finally {
    steps.unmount();
  }

  // The words match task 2 alone, so the view holds it alone, under the whole plan's order.
  const words = drawPlan(node(KEY, "issue", [replaced]), {
    plan: root.children,
    search: "?q=t2",
  });
  try {
    expect(theLine(words.container)).toHaveAttribute("aria-expanded", "true");
    expect(rowOf(words.container, T2)).not.toBeNull();
    expect(theLine(words.container)).toHaveAttribute("tabindex", "0");
    await user.click(words.getByRole("button", { name: PLAN_WORDS.steps }));
    expect(theLine(words.container)).toHaveAttribute("aria-expanded", "true");
    expect(rowOf(words.container, T2)).not.toBeNull();
  } finally {
    words.unmount();
  }
});

// Under the Plan chart a line the words drew open closes with no press once the reader clears
// the words. The task's row under it that held the chart's one tab stop is then no row, so the
// stop goes to the line that now folds it, as the outline's and the Spec view's do, and not to
// the first bar.
test("a Plan line the words drew open takes the tab stop of its task when the words go", async () => {
  readWide();
  let moveTo: (to: string) => void = () => {};
  const view = drawPlan(planWithOvertaken(), {
    search: "?q=t2",
    take: (navigate) => {
      moveTo = navigate;
    },
  });
  try {
    expect(theLine(view.container)).toHaveAttribute("aria-expanded", "true");
    act(() => rowOf(view.container, T2)?.focus());
    await waitFor(() => expect(rowOf(view.container, T2)).toHaveAttribute("tabindex", "0"));

    act(() => moveTo("/"));
    await waitFor(() => expect(theLine(view.container)).toHaveAttribute("aria-expanded", "false"));
    expect(rowOf(view.container, T2)).toBeNull();
    expect(theLine(view.container)).toHaveAttribute("tabindex", "0");
    expect(rowOf(view.container, T1)).toHaveAttribute("tabindex", "-1");
  } finally {
    view.unmount();
  }
});

// Under Steps, an address naming a live task that the outline holds under an overtaken statement
// lands on the task's row and opens no line: the steps hold every task under their own root, so
// the overtaken statement in the outline's chain is no node of theirs, and the line of overtaken
// tasks stays closed until the reader opens it.
test("under Steps an address to a live task under an overtaken statement opens no line", async () => {
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
    expect(theLine(view.container)).toHaveAttribute("aria-expanded", "false");
    expect(rowOf(view.container, T2)).toBeNull();
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
    line: "The page draws r7.",
    readId: "spec#b-r7",
    reference: `dispatch://${KEY}/spec#b-r7`,
    type: "requirement",
  };
  const needs: NodeChip = {
    answered: true,
    gone: false,
    label: BLOCK_ATTRIBUTE_LABELS.needs,
    name: "Task t1 is carried out.",
    ref: planRef("t1"),
  };
  const view = drawPlan(
    stamped(
      node(KEY, "issue", [planTask("t1"), planTask("t2", { chips: [needs], parentLine: serves })]),
      { [T1]: { life: "proposed" }, "spec#b-r7": { life: "proposed", onlyYou: true } }
    )
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
    for (const id of [LENS, FINDING, RISK, CONSTRAINT]) {
      if (id === FINDING) {
        // A proposed node opens no path of the fold (R2), so the lens stays folded and its
        // finding is a row once the reader opens the lens.
        expect(rowOf(view.container, LENS)).toHaveAttribute("aria-expanded", "false");
        act(() => rowOf(view.container, LENS)?.focus());
        await user.keyboard("{ArrowRight}");
        await waitFor(() =>
          expect(rowOf(view.container, LENS)).toHaveAttribute("aria-expanded", "true")
        );
      }
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
