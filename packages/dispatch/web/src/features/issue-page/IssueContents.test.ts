import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, within } from "@testing-library/react";
import { createElement, useEffect } from "react";

import { api } from "../../api/client";
import { whoAmIQuery } from "../../api/queries";
import type { Actor } from "../../api/types";
import { FocusedNodeProvider, useSetFocusedNode } from "./focused-node";
import { type ContentsEntry, ContentsList, contentsOf } from "./IssueContents";
import type { TreeNode } from "./issue-tree";
import { CONTROL_LABELS, type NodeType } from "./page-words";
import { SENTENCE_TABLE } from "./sentence-table";

/**
 * The Contents list's entries, nested under the nearest entry above them, over a tree built
 * by hand. The issue itself is no entry: the header is the issue.
 */

/** One node of a hand-built tree. A requirement here is one of the issue's own spec, which the
 *  page's model marks a statement (`isStatement`), so the list counts it as the filter row
 *  does. */
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
    ...(type === "requirement" ? { statement: true as const } : {}),
    ...extra,
  };
}

const alice: Actor = { id: "alice", kind: "user" };
const bob: Actor = { id: "bob", kind: "user" };
const agent: Actor = { id: "session-1", kind: "agent" };

/** Each entry as its id, with the entries under it after a colon. */
function shape(entries: readonly ContentsEntry[]): string[] {
  return entries.map((entry) =>
    entry.children.length === 0
      ? entry.node.id
      : `${entry.node.id}: ${shape(entry.children).join(", ")}`
  );
}

/** A question holding a nested question and the reader's comment thread. Beside it are other
 *  people's turns, the reader's messages with and without a statement, and a transcluded ask. */
const nested = node("CORE-1", "issue", [
  node("ask-1", "ask", [
    node("requirement-1", "requirement", [node("ask-2", "ask")]),
    node("comment-mine", "comment", [node("reply-mine", "comment", [], { author: alice })], {
      author: alice,
    }),
  ]),
  node("comment-bob", "comment", [], { author: bob }),
  node("message-agent", "message", [], { author: agent }),
  node("message-mine", "message", [node("requirement-2", "requirement")], { author: alice }),
  node("message-alone", "message", [], { author: alice }),
  node("ask-1-again", "ask", [], { transcluded: true }),
]);

/** One comment of the reader's under the issue. */
const mine = node("CORE-1", "issue", [node("comment-mine", "comment", [], { author: alice })]);

/** A branch drawn under two statements: at its own place, and as a transcluded copy. */
function branch(transcluded: boolean): TreeNode {
  return node(
    "plan#b-t1",
    "plan_task",
    [node("ask-under", "ask"), node("decisions#b-d1", "decision", [], { waiting: true })],
    { transcluded }
  );
}
const copied = node("CORE-1", "issue", [
  node("ask-1", "ask", [
    node("spec#b-r1", "requirement", [branch(false)]),
    node("spec#b-r2", "requirement", [branch(true)]),
  ]),
]);

/** Statements, a child issue and decisions under the issue, under a question and under a
 *  question nested in it, one statement with a failed check. */
const counted = node("CORE-1", "issue", [
  node("spec#b-r1", "requirement"),
  node("CORE-2", "child_issue"),
  node("ask-1", "ask", [
    node("spec#b-r2", "requirement", [node("verification#b-c2", "check", [], { state: "failed" })]),
    node("decisions#b-d1", "decision", [], { waiting: true }),
    node("decisions#b-d2", "decision"),
    node("ask-2", "ask", [node("spec#b-r3", "requirement")]),
  ]),
]);

test("the list holds each question and each thread the reader began, nested, and no issue", () => {
  expect(shape(contentsOf(nested, "alice"))).toEqual([
    "ask-1: ask-2, comment-mine",
    "message-mine",
  ]);
});

test("a reader the page has not named yet begins no thread of the list", () => {
  expect(shape(contentsOf(mine, undefined))).toEqual([]);
});

// The reader is told from the login as the server holds it, whatever its case
// (`refs/actor.ts`'s `isViewer`).
test("a thread the reader began is theirs whatever the case of their login", () => {
  expect(shape(contentsOf(mine, "Alice"))).toEqual(["comment-mine"]);
});

// A node drawn under a second parent is the same node: its copy, and the branch under the copy,
// are neither listed nor counted again.
test("a transcluded copy and the branch under it are listed and counted once", () => {
  const [entry] = contentsOf(copied, "alice");
  expect(shape(entry === undefined ? [] : [entry])).toEqual(["ask-1: ask-under"]);
  expect(entry?.marks.for_review).toBe(1);
});

// R14, the 1440 px picture: each entry's marks count what its own branch holds - its
// statements, its decisions waiting for the reader's review, its child issues and its statements
// a check failed - and a node counts once, on the nearest entry above it.
test("each entry counts what its own branch holds, and a nested entry's branch is its own", () => {
  const marks = (entries: readonly ContentsEntry[]): Record<string, unknown> =>
    Object.fromEntries(
      entries.flatMap((entry) => [
        [entry.node.id, entry.marks],
        ...Object.entries(marks(entry.children)),
      ])
    );
  expect(marks(contentsOf(counted, "alice"))).toEqual({
    // What sits under the issue and under no entry is counted on none: the issue has no entry.
    "ask-1": { children: 0, failing: 1, for_review: 1, statements: 1 },
    "ask-2": { children: 0, failing: 0, for_review: 0, statements: 1 },
  });
});

// R2 and R14: a requirement of a document other than the spec - a plan's, under a question - is
// a block and nothing more, so the list counts no statement for it, as the filter row counts
// none (`isStatement`).
test("a requirement of another document is no statement an entry counts", () => {
  const root = node("CORE-1", "issue", [
    node("ask-1", "ask", [
      node("spec#b-r1", "requirement"),
      // The model marks no requirement of the plan a statement.
      node("plan-md#b-r9", "requirement", [], { statement: undefined }),
    ]),
  ]);
  expect(contentsOf(root, "alice")[0]?.marks.statements).toBe(1);
});

/** Puts the page's focus on node `id`, as the outline does when the reader moves to it. */
function FocusOn({ id }: { id: string }): null {
  const setFocused = useSetFocusedNode();
  useEffect(() => setFocused({ id, title: id, type: "ask" }), [id, setFocused]);
  return null;
}

// R9 and R14: the list is one tab stop, and that stop is always a drawn row. The reader is in an
// entry under another, and folds the one above it: the entry they are in is drawn no more, so
// the tab stop goes to the nearest drawn entry above it rather than to no row at all, which Tab
// would skip the list for.
test("folding the entry above the one the reader is in leaves the tab stop on the folded entry", () => {
  const root = node("CORE-1", "issue", [node("ask-1", "ask", [node("ask-2", "ask")])]);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  const view = render(
    createElement(
      QueryClientProvider,
      { client },
      createElement(
        FocusedNodeProvider,
        null,
        createElement(FocusOn, { id: "ask-2" }),
        createElement(ContentsList, {
          focusBranch: false,
          nameOf: () => undefined,
          onFocusBranch: () => {},
          onGo: () => {},
          root,
        })
      )
    )
  );
  const stops = () =>
    [...view.container.querySelectorAll<HTMLElement>('[role="treeitem"][tabindex="0"]')].map(
      (row) => row.dataset.contentsEntry
    );
  try {
    expect(stops()).toEqual(["ask-2"]);
    const parent = view.container.querySelector<HTMLElement>('[data-contents-entry="ask-1"]');
    if (parent === null) throw new Error("the list draws no entry for ask-1");
    act(() => within(parent).getByRole("button", { name: CONTROL_LABELS.fold }).click());
    expect(view.container.querySelector('[data-contents-entry="ask-2"]')).toBeNull();
    expect(stops()).toEqual(["ask-1"]);
  } finally {
    view.unmount();
  }
});

// The list reads who is reading as the tree reads it, kept fresh by events and never read again
// by its age (`EVENT_FRESH`). Under the app's 30 s `staleTime` (`main.tsx`) a list drawn more than
// 30 s after whoami answered asked for it again, a request of the list's own.
test("a list drawn more than 30 s after whoami answered asks for it nothing", () => {
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
  });
  client.setQueryData(
    whoAmIQuery().queryKey,
    { event_head: "0", kind: "user", login: "alice" },
    { updatedAt: Date.now() - 31_000 }
  );
  const view = render(
    createElement(
      QueryClientProvider,
      { client },
      createElement(ContentsList, {
        focusBranch: false,
        nameOf: () => undefined,
        onFocusBranch: () => {},
        onGo: () => {},
        root: node("CORE-1", "issue", [node("ask-1", "ask")]),
      })
    )
  );
  try {
    expect(
      view.container.querySelector('[data-contents-entry="ask-1"]'),
      "the list drew no entry"
    ).not.toBeNull();
    expect(whoAmI, "the list asked for whoami again").not.toHaveBeenCalled();
  } finally {
    view.unmount();
    whoAmI.mockRestore();
  }
});

/** The words of a question the list draws five levels down. */
const DEEP_QUESTION = "Configuration drift between hosts is the question.";

/** A question five levels down, under a question at each level above it, holding a statement,
 *  so its entry carries one mark. */
const deep = node("CORE-1", "issue", [
  [4, 3, 2, 1, 0].reduce(
    (under, level) => node(`ask-${level}`, "ask", [under]),
    node("ask-5", "ask", [node("spec#b-r1", "requirement")], { text: DEEP_QUESTION })
  ),
]);

/** How wide the stubbed canvas draws each character of a run of words, at every size. */
const CHARACTER = 8;

/**
 * The list drawn over `root` in a box `width` px wide. The two reads of the browser's layout
 * that the list makes (`useOneLineBox`) and happy-dom does not answer are answered as a browser
 * would: the list's own width, and its words measured on a canvas, `CHARACTER` px a character.
 */
function drawList(root: TreeNode, width: number): { container: HTMLElement; unmount: () => void } {
  const canvas = spyOn(HTMLCanvasElement.prototype, "getContext").mockImplementation((() => ({
    font: "",
    measureText: (text: string) => ({ width: text.length * CHARACTER }),
  })) as unknown as HTMLCanvasElement["getContext"]);
  // On `HTMLElement.prototype`, where `IssueTree.test.tsx` spies the same method: a spy there
  // left behind as the prototype's own property would hide one made on `Element.prototype`.
  const box = spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement
  ) {
    return new DOMRect(0, 0, this.getAttribute("role") === "tree" ? width : 0, 0);
  });
  const restore = () => {
    canvas.mockRestore();
    box.mockRestore();
  };
  try {
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
    });
    client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
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
    return {
      container: view.container,
      unmount: () => {
        view.unmount();
        restore();
      },
    };
  } catch (error) {
    restore();
    throw error;
  }
}

// R3 and review finding 4590606141: where an entry's row leaves its line less room than the
// line's first word, the line goes under the row's other parts, on a line of its own at the
// entry's indent, cut at a word to that line's width, and the row grows to hold it. An entry
// whose line fits beside its parts keeps its one row of 32 px.
test("an entry whose row leaves its line no word draws the line under the row's other parts", () => {
  // 275 px: the question five levels down starts 86 px in, and its twisty, bullet and mark with
  // their gaps take 62 px of the 171 px inside its indent, which leaves its line 108 px beside
  // them (the pixel for rounding kept), short of "Configuration…" (112 px). Under them it has the
  // 171 px less that pixel, where "Configuration drift…" (160 px) fits and "Configuration drift
  // between…" (224 px) does not.
  const narrow = drawList(deep, 275);
  try {
    const row = narrow.container.querySelector<HTMLElement>('[data-contents-entry="ask-5"]');
    const line = row?.querySelector<HTMLElement>("[data-contents-line]");
    expect(line?.textContent).toBe("Configuration drift…");
    expect(row).toHaveClass("min-h-8", "flex-wrap");
    expect(row).not.toHaveClass("h-8");
    expect(line).toHaveClass("order-last", "basis-full");
    expect(line).not.toHaveClass("flex-1");
    // The question at the top has room for its words beside its parts, in the same list.
    const top = narrow.container.querySelector<HTMLElement>('[data-contents-entry="ask-0"]');
    expect(top).toHaveClass("h-8");
    expect(top).not.toHaveClass("flex-wrap");
    expect(top?.querySelector("[data-contents-line]")).toHaveClass("flex-1");
  } finally {
    narrow.unmount();
  }
  // 600 px leaves the deep question's whole line room beside its parts: one row of 32 px.
  const wide = drawList(deep, 600);
  try {
    const row = wide.container.querySelector<HTMLElement>('[data-contents-entry="ask-5"]');
    const line = row?.querySelector<HTMLElement>("[data-contents-line]");
    expect(line?.textContent).toBe(DEEP_QUESTION);
    expect(row).toHaveClass("h-8");
    expect(row).not.toHaveClass("flex-wrap");
    expect(line).toHaveClass("flex-1");
    expect(line).not.toHaveClass("basis-full");
  } finally {
    wide.unmount();
  }
});

// R2 and R3: an entry draws its node's first line by R1's one-line rule, which cuts a node's
// line from the first block of its words (`lineWords`, which the outline's row and the page's
// other one-line places read). A thread the reader began with a short line of its own, then a
// paragraph, draws that line; the whole words join the two with one space into a line its
// author never wrote.
test("an entry draws its node's first block as its line, never its first two blocks run together", () => {
  const opened = node("CORE-1", "issue", [
    node("comment-mine", "comment", [], {
      author: { id: "alice", kind: "user" },
      text: "A short note\n\nThe identity block condenses at one width only.",
    }),
  ]);
  const view = drawList(opened, 600);
  try {
    const line = view.container.querySelector<HTMLElement>(
      '[data-contents-entry="comment-mine"] [data-contents-line]'
    );
    expect(line?.textContent, "the entry runs its first block into the next").not.toContain(
      "note The identity"
    );
    expect(line?.textContent).toBe("A short note");
  } finally {
    view.unmount();
  }
});

/** `drawList`'s list over `root` in a box `width` px wide, its entries' references named by
 *  `nameOf`, as the slot names them (`useReferenceNames`). */
function drawNamedList(
  root: TreeNode,
  width: number,
  nameOf: (reference: string) => string | undefined
): { container: HTMLElement; unmount: () => void } {
  const canvas = spyOn(HTMLCanvasElement.prototype, "getContext").mockImplementation((() => ({
    font: "",
    measureText: (text: string) => ({ width: text.length * CHARACTER }),
  })) as unknown as HTMLCanvasElement["getContext"]);
  const box = spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement
  ) {
    return new DOMRect(0, 0, this.getAttribute("role") === "tree" ? width : 0, 0);
  });
  const restore = () => {
    canvas.mockRestore();
    box.mockRestore();
  };
  try {
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
    });
    client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
    const view = render(
      createElement(
        QueryClientProvider,
        { client },
        createElement(ContentsList, {
          focusBranch: false,
          nameOf,
          onFocusBranch: () => {},
          onGo: () => {},
          root,
        })
      )
    );
    return {
      container: view.container,
      unmount: () => {
        view.unmount();
        restore();
      },
    };
  } catch (error) {
    restore();
    throw error;
  }
}

// R1, R14 and the tester's F20: an entry's line is cut with each reference it cites still
// unnamed and measured with each named. A statement's name opens with its label ("R1."), which
// ends no sentence of the line, and the row's width never cuts the line inside the name.
test("an entry that cites a labelled statement is cut before the name goes in, never inside it", () => {
  const cited = "dispatch://CORE-1/spec#b-r1";
  const nameOf = (reference: string) =>
    reference === cited ? "R1. Every node is whole" : undefined;
  const opened = node("CORE-1", "issue", [
    node("ask-1", "ask", [], { text: `Should ${cited} stay as it is? Then more.` }),
  ]);
  // 600 px leaves the whole first sentence room: 45 characters, 360 px. 258 px leaves the line
  // 200 px beside the row's other parts (24 px of margins, padding and inset, 33 of twisty,
  // bullet and gaps, the pixel for rounding): "Should R1. Every node is…" would fit, but its cut
  // is inside the name, and "Should R1. Every node is whole…" (248 px) does not, so the line is
  // the word before the name.
  for (const [width, drawn] of [
    [600, "Should R1. Every node is whole stay as it is?"],
    [258, "Should…"],
  ] as const) {
    const view = drawNamedList(opened, width, nameOf);
    try {
      const line = view.container.querySelector<HTMLElement>(
        '[data-contents-entry="ask-1"] [data-contents-line]'
      );
      expect(line?.textContent, `${width} px`).toBe(drawn);
    } finally {
      view.unmount();
    }
  }
});

// R1 and R14: an entry for a thread the reader began draws its comment's first line by the page's
// one sentence end, as the table of sentence ends (`SENTENCE_TABLE`) states it for a stored text:
// a comment that opens with a list draws its first item, whose marker markdown reads as no word.
test("an entry for a comment of each row of the sentence table draws the row's stored sentence", () => {
  const opened = node(
    "CORE-1",
    "issue",
    SENTENCE_TABLE.map((row) =>
      node(`comment-${row.id}`, "comment", [], { author: alice, text: row.text })
    )
  );
  // 1000 px leaves the longest first sentence of the table, 36 characters and 288 px, room
  // beside its entry's other parts, so the box cuts no line.
  const view = drawList(opened, 1000);
  try {
    for (const row of SENTENCE_TABLE) {
      const line = view.container.querySelector<HTMLElement>(
        `[data-contents-entry="comment-${row.id}"] [data-contents-line]`
      );
      expect(line?.textContent, row.id).toBe(row.stored);
    }
  } finally {
    view.unmount();
  }
});
