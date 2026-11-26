import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, waitFor, within } from "@testing-library/react";
import { createElement, Fragment, type ReactElement, type ReactNode, useEffect } from "react";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { api } from "../../api/client";
import { whoAmIQuery } from "../../api/queries";
import type { Actor } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { PublishedReferenceNames, ReferenceNamesProvider } from "../refs/reference-names";
import { producingMessages } from "./contents-entries";
import { KEY } from "./decisions-filter/decisions-fixtures";
import { FocusedNodeProvider, useSetFocusedNode } from "./focused-node";
import { type ContentsEntry, ContentsList, contentsOf } from "./IssueContents";
import type { TreeNode } from "./issue-tree";
import { NoTooltips } from "./NoTooltips";
import { type LineNames, useIssueNames } from "./one-line";
import { CONTROL_LABELS, type NodeType } from "./page-words";
import { presetView } from "./presets";
import { Publisher, publishedNames } from "./projection-fixture";
import { RowText } from "./RowText";
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

/** What a page that published `lines`, the line a chip names each block by, and no other name
 *  names an entry's references by (`useIssueNames`): a reference with no line reads as its
 *  author wrote it. */
function lineNamesOf(lines: ReadonlyMap<string, string> = new Map()): LineNames {
  return {
    ambiguous: new Set(),
    author: () => "",
    deleted: new Map(),
    edited: new Set(),
    issue: "CORE-1",
    issues: [],
    lines,
    names: new Map(),
  };
}

/** A page that has published no name. */
const NO_NAMES = lineNamesOf();

/** Each entry as its id, with the entries under it after a colon. */
function shape(entries: readonly ContentsEntry[]): string[] {
  return entries.map((entry) =>
    entry.children.length === 0
      ? entry.node.id
      : `${entry.node.id}: ${shape(entry.children).join(", ")}`
  );
}

/** The entries of `root` for `viewer`, its producing messages read off `root` itself, as the
 *  slot reads them with no words filter. */
function entriesOf(root: TreeNode, viewer: string | undefined): ContentsEntry[] {
  return contentsOf(root, viewer, producingMessages(root));
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
  expect(shape(entriesOf(nested, "alice"))).toEqual(["ask-1: ask-2, comment-mine", "message-mine"]);
});

test("a reader the page has not named yet begins no thread of the list", () => {
  expect(shape(entriesOf(mine, undefined))).toEqual([]);
});

// The reader is told from the login as the server holds it, whatever its case
// (`refs/actor.ts`'s `isViewer`).
test("a thread the reader began is theirs whatever the case of their login", () => {
  expect(shape(entriesOf(mine, "Alice"))).toEqual(["comment-mine"]);
});

// A node drawn under a second parent is the same node: its copy, and the branch under the copy,
// are neither listed nor counted again.
test("a transcluded copy and the branch under it are listed and counted once", () => {
  const [entry] = entriesOf(copied, "alice");
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
  expect(marks(entriesOf(counted, "alice"))).toEqual({
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
  expect(entriesOf(root, "alice")[0]?.marks.statements).toBe(1);
});

// R2: a words filter changes what the list draws, never what a thread produced. The view the
// filter leaves keeps the reader's message, whose words hold the filter's word, and drops the
// statement under it, which holds none; the list still reads the message as having produced it.
test("under a words filter a message the reader wrote that produced a node is an entry where only the message holds the words", () => {
  const root = node("CORE-1", "issue", [
    node(
      "message-mine",
      "message",
      [node("spec#b-r1", "requirement", [], { text: "The page draws the list." })],
      { author: alice, text: "Please add a quokka statement." }
    ),
  ]);
  const view = presetView(root, "conversation", "quokka").root;
  expect(
    view.children[0]?.children,
    "the filter's view keeps the statement that holds none of its words"
  ).toEqual([]);
  expect(shape(contentsOf(view, "alice", producingMessages(root)))).toEqual(["message-mine"]);
});

/** Puts the page's focus on node `id`, as the outline does when the reader moves to it. */
function FocusOn({ id }: { id: string }): null {
  const setFocused = useSetFocusedNode();
  useEffect(() => setFocused({ id, text: id, type: "ask" }), [id, setFocused]);
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
          lineNames: NO_NAMES,
          onFocusBranch: () => {},
          onGo: () => {},
          producing: producingMessages(root),
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
        lineNames: NO_NAMES,
        onFocusBranch: () => {},
        onGo: () => {},
        producing: new Set<string>(),
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
 * `element` drawn under a query client that holds the reader, as the list is drawn in a box
 * `width` px wide. The two reads of the browser's layout that the list makes (`useOneLineBox`)
 * and happy-dom does not answer are answered as a browser would: the list's own width, and its
 * words measured on a canvas, `CHARACTER` px a character.
 */
function drawMeasured(
  width: number,
  element: ReactElement
): { container: HTMLElement; unmount: () => void } {
  const canvas = spyOn(HTMLCanvasElement.prototype, "getContext").mockImplementation((() => ({
    font: "",
    measureText: (text: string) => ({ width: text.length * CHARACTER }),
  })) as unknown as HTMLCanvasElement["getContext"]);
  // On `Element.prototype`, which owns the method (`docs/testing.md`): a spy restored on
  // `HTMLElement.prototype` leaves a copy there that hides a later file's spy on `Element`.
  const box = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
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
    const view = render(createElement(QueryClientProvider, { client }, element));
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

/** The list drawn over `root` in a box `width` px wide (`drawMeasured`), its entries' references
 *  named by `lineNames`, as the slot names them (`useIssueNames`). */
function drawList(
  root: TreeNode,
  width: number,
  lineNames: LineNames = NO_NAMES
): { container: HTMLElement; unmount: () => void } {
  return drawMeasured(
    width,
    createElement(ContentsList, {
      focusBranch: false,
      lineNames,
      onFocusBranch: () => {},
      onGo: () => {},
      producing: producingMessages(root),
      root,
    })
  );
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
// line from the first block of its words (`lineWordsRaw`, as the outline's row and the page's
// other one-line places read the first block). A thread the reader began with a short line of
// its own, then a paragraph, draws that line; the whole words join the two with one space into a
// line its author never wrote.
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

// R1, R14 and the tester's F20: an entry's line is cut with each reference it cites still
// unnamed and measured with each named. A statement's name opens with its label ("R1."), which
// ends no sentence of the line, and the row's width cuts the line at a word of the name, never
// inside one: each word of a name is a token of its own.
test("an entry that cites a labelled statement is cut before the name goes in, never inside a word of it", () => {
  const cited = "dispatch://CORE-1/spec#b-r1";
  const lineNames = lineNamesOf(new Map([[cited, "R1. Every node is whole"]]));
  const opened = node("CORE-1", "issue", [
    node("ask-1", "ask", [], { text: `Should ${cited} stay as it is? Then more.` }),
  ]);
  // 600 px leaves the whole first sentence room: 45 characters, 360 px. 258 px leaves the line
  // 200 px beside the row's other parts (24 px of margins, padding and inset, 33 of twisty,
  // bullet and gaps, the pixel for rounding): "Should R1. Every node is whole…" (248 px) does not
  // fit, and "Should R1. Every node is…" (200 px), cut at the space between two of the name's
  // words, does.
  for (const [width, drawn] of [
    [600, "Should R1. Every node is whole stay as it is?"],
    [258, "Should R1. Every node is…"],
  ] as const) {
    const view = drawList(opened, width, lineNames);
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

// R8 and R14: an entry names a reference the page published no name for - another issue's
// comment it has not answered for, or answered is gone - as the words its author wrote, as the
// node's row in the outline draws it, and never as its short form ("OTHER-1 comment").
test("an entry draws a reference the page published no name for as written, never as its short form", () => {
  const gone = "dispatch://OTHER-1/comment/0f8e2c3a-6b1d-4e5f-9a7c-2d3e4f5a6b7c";
  const opened = node("CORE-1", "issue", [
    node("ask-1", "ask", [], { text: `Does ${gone} still hold? Then more.` }),
  ]);
  // 1000 px leaves the first sentence, 80 characters and 640 px, room beside the entry's parts.
  const view = drawList(opened, 1000);
  try {
    const line = view.container.querySelector<HTMLElement>(
      '[data-contents-entry="ask-1"] [data-contents-line]'
    );
    expect(line?.textContent).toBe(`Does ${gone} still hold?`);
    expect(line?.textContent).not.toContain("OTHER-1 comment");
  } finally {
    view.unmount();
  }
});

/** The Contents list over `root` as the slot draws it, its entries' references named by what the
 *  page published (`useIssueNames`). */
function PublishedContents({ root }: { root: TreeNode }): ReactNode {
  const lineNames = useIssueNames(KEY);
  return createElement(ContentsList, {
    focusBranch: false,
    lineNames,
    onFocusBranch: () => {},
    onGo: () => {},
    producing: producingMessages(root),
    root,
  });
}

// R1, R14 and R16: a Contents entry is a plain line, and names each reference its words cite as
// the node's row in the outline names it, by the one rule (`rowReferenceNames`,
// `rowReferenceWords`) from the same names the page published: an issue by its key and its title.
// The entry read the key alone where the row read the key and the title.
test("a Contents entry and its outline row name a resolved issue alike, by key and title", async () => {
  await warmMarkdownRenderer();
  const cited = "dispatch://CORE-7";
  const title = "The header draws the approval of each step";
  const text = `Does ${cited} still hold?`;
  const tree = displayTree(text);
  if (tree === undefined) throw new Error("the engine read no tree of the question's words");
  const root = node("CORE-1", "issue", [node("ask-1", "ask", [], { text, tree })]);
  const published = publishedNames(new Map([[cited, title]]));
  // 1000 px leaves the entry's whole line room beside its parts, so the box cuts nothing.
  const view = drawMeasured(
    1000,
    createElement(
      MemoryRouter,
      null,
      createElement(
        NoTooltips,
        null,
        createElement(
          ReferenceNamesProvider,
          null,
          createElement(Publisher, { published }),
          createElement(PublishedReferenceNames, {
            // biome-ignore lint/correctness/noChildrenProp: the provider types `children` as a required prop, which createElement's positional children do not fill in a file that writes no JSX.
            children: createElement(
              Fragment,
              null,
              createElement(PublishedContents, { root }),
              createElement(
                "div",
                { "data-outline-row": "" },
                createElement(RowText, { markdown: text, tree })
              )
            ),
            published,
          })
        )
      )
    )
  );
  try {
    const drawn = `Does CORE-7 ${title} still hold?`;
    const entry = () =>
      view.container.querySelector<HTMLElement>(
        '[data-contents-entry="ask-1"] [data-contents-line]'
      );
    const row = () => view.container.querySelector<HTMLElement>("[data-outline-row]");
    await waitFor(() => expect(entry()?.textContent, "the Contents entry").toBe(drawn));
    await waitFor(() =>
      expect(
        row()?.querySelector(`a[data-row-reference="${cited}"]`)?.textContent,
        "the outline row's link"
      ).toBe(`CORE-7 ${title}`)
    );
    expect(row()?.textContent?.trim(), "the outline row").toBe(drawn);
    for (const [where, words] of [
      ["the Contents entry", entry()?.textContent],
      ["the outline row", row()?.textContent],
    ] as const) {
      expect(words, where).not.toContain("dispatch://");
    }
  } finally {
    view.unmount();
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
