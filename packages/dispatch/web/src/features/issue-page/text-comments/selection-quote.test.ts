import { afterEach, beforeAll, describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { type RenderResult, render, waitFor } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../../__tests__/display-trees";
import type { DisplayTree } from "../../../api/types";
import { quoteAnchor } from "../../doc/quote-anchor";
import { loadedEngine, warmMarkdownRenderer } from "../../refs/markdown-engine";
import { KEY, node } from "../decisions-filter/decisions-fixtures";
import { buildIssueTree, type TreeNode } from "../issue-tree";
import { NoTooltips } from "../NoTooltips";
import { RowText } from "../RowText";
import { ReferenceNamesProvider, usePublishReferenceNames } from "../reference-names";
import { issueDetails } from "../spec-filter/spec-fixtures";
import { treeAnswer, treeNode } from "../tree-read/tree-fixtures";
import type { TreeAnswer, TreeAnswerNode } from "../tree-read/tree-read";
import {
  answerSeededReads,
  clearSelection,
  drawPage,
  drawTree,
  located,
  newClient,
  PLAN_TASK_WORDS,
  STATEMENT_WORDS,
  setSelection,
} from "./margin-fixtures";
import { textRuns } from "./row-text-runs";
import { itemBlocksOf, type QuotedNode, quoteOf, rangeOf } from "./selection-quote";

/**
 * R5: what a selection in a row quotes. Each case draws the real row (`drawTree`, `drawPage`,
 * `RowText` as `RowText.test.tsx` draws it), makes a real DOM selection and asks `quoteOf`, so the
 * words are those the page draws and the quote is what the server's `FindInBlock` matches
 * (`fixtures/quotes.json` holds that for each inline kind a row draws, recorded from the real
 * server at B0). happy-dom lays nothing out, so no case reads a rectangle.
 */

beforeAll(async () => {
  await warmMarkdownRenderer();
});

let stubs: { mockRestore(): void }[] = [];
afterEach(() => {
  clearSelection();
  for (const stub of stubs) stub.mockRestore();
  stubs = [];
});

/** A selection that the page's `Selection` holds. */
function selected(): Selection {
  const selection = window.getSelection();
  if (selection === null) throw new Error("the window holds no selection");
  return selection;
}

/** The text node under `root` that holds `needle`, counting `nth` from zero over the nodes that
 *  hold it, in order, and where in it the needle starts. */
function locate(root: Element, needle: string, nth = 0): { node: Text; at: number } {
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  let seen = 0;
  for (let text = walker.nextNode(); text !== null; text = walker.nextNode()) {
    const at = (text as Text).data.indexOf(needle);
    if (at < 0) continue;
    if (seen === nth) return { at, node: text as Text };
    seen += 1;
  }
  throw new Error(`the row draws no text node ${nth} that holds "${needle}"`);
}

/** Selects from the start of `first` to the end of `last` under `root`, as a drag does. */
function selectBetween(
  root: Element,
  first: string,
  last: string = first,
  nth: { first?: number; last?: number } = {}
): void {
  const from = locate(root, first, nth.first ?? 0);
  // A selection of one needle ends in the node it starts in.
  const to = locate(root, last, nth.last ?? (last === first ? (nth.first ?? 0) : 0));
  const range = document.createRange();
  range.setStart(from.node, from.at);
  range.setEnd(to.node, to.at + last.length);
  setSelection(range);
}

/** Selects the characters `[start, end)` of the plain text that `words` draws, by the DOM point
 *  `textRuns` gives each end. */
function selectOffsets(words: HTMLElement, start: number, end: number): void {
  const runs = textRuns(words);
  const from = runs.pointOf(start);
  const last = runs.pointOf(end - 1);
  if (from === null || last === null) throw new Error(`no run holds ${start}..${end}`);
  const range = document.createRange();
  range.setStart(from.node, from.offset);
  range.setEnd(last.node, last.offset + 1);
  setSelection(range);
}

/** The reading of `markdown` the server would send. */
function readingOf(markdown: string): DisplayTree {
  const tree = displayTree(markdown);
  if (tree === undefined)
    throw new Error(`the engine reads no tree of ${JSON.stringify(markdown)}`);
  return tree;
}

/** A node of a comment's kind drawn from `markdown`'s reading. */
function comment(id: string, markdown: string): TreeNode {
  return node({ id, text: markdown, tree: readingOf(markdown), type: "comment" });
}

/** The node a row holds as the tree read answers it: its text and its reading. */
function quotedFrom(markdown: string): QuotedNode {
  return { text: markdown, text_tree: readingOf(markdown) };
}

/** How many matches of `quote` the text holds, overlapping ones included. */
function matchCount(text: string, quote: string): number {
  let count = 0;
  for (let at = text.indexOf(quote); at >= 0; at = text.indexOf(quote, at + 1)) count += 1;
  return count;
}

/** The names and marks a page publishes, stable across renders as `usePublishReferenceNames`
 *  wants. */
interface Published {
  readonly edited: ReadonlySet<string>;
  readonly names: ReadonlyMap<string, string>;
}

const NOTHING_PUBLISHED: Published = { edited: new Set(), names: new Map() };

function Publishing({
  children,
  published,
}: {
  children?: ReactNode;
  published: Published;
}): ReactNode {
  usePublishReferenceNames(published.names, undefined, published.edited);
  return children;
}

/** `RowText` of `markdown` as a row draws it, in a row element that carries a node id and a words
 *  element, with the names the page published. */
function drawWords(
  markdown: string,
  published: Published = NOTHING_PUBLISHED
): { readonly row: HTMLElement; readonly unmount: () => void; readonly words: HTMLElement } {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const view = render(
    createElement(
      QueryClientProvider,
      { client },
      createElement(
        MemoryRouter,
        { initialEntries: ["/issues/CORE-1"] },
        createElement(
          NoTooltips,
          null,
          createElement(
            ReferenceNamesProvider,
            null,
            createElement(
              Publishing,
              { published },
              createElement(
                "div",
                { "data-node-id": "words-row" },
                createElement(
                  "div",
                  { "data-node-words": "" },
                  createElement(RowText, { markdown, tree: readingOf(markdown) })
                )
              )
            )
          )
        )
      )
    )
  );
  const { row, words } = located(view, "words-row");
  return { row, unmount: view.unmount, words };
}

describe("occurrence", () => {
  test("occurrence counts overlapping matches before the selection", () => {
    const view = drawWords("aaaa");
    try {
      const text = quotedFrom("aaaa");
      for (const [start, end, expected] of [
        [0, 3, { occurrence: 0, quote: "aaa" }],
        [1, 4, { occurrence: 1, quote: "aaa" }],
        [2, 4, { occurrence: 2, quote: "aa" }],
        [3, 4, { occurrence: 3, quote: "a" }],
      ] as const) {
        selectOffsets(view.words, start, end);
        expect(quoteOf(selected(), view.row, text), `[${start}, ${end})`).toEqual({
          block: null,
          ...expected,
        });
      }
    } finally {
      view.unmount();
    }
  });

  test("a heading marker at the start of the quote is left out, as quoteAnchor leaves it", () => {
    const tree: DisplayTree = {
      content: [
        { content: [{ text: "Plan", type: "text" }], type: "paragraph" },
        { content: [{ text: "## Plan", type: "text" }], type: "paragraph" },
      ],
      type: "doc",
    };
    const text = "Plan\n\n\\#\\# Plan";
    const view = drawTree([node({ id: "marked", text, tree, type: "comment" })]);
    try {
      const { row, words } = located(view, "marked");
      selectBetween(words, "## Plan");
      expect(quoteOf(selected(), row, { text, text_tree: tree })).toEqual({
        block: null,
        occurrence: 1,
        quote: "Plan",
      });
    } finally {
      view.unmount();
    }
  });
});

/** One selection made from the start of `first` to the end of `last`, each found as `locate`
 *  finds it. */
interface Pick {
  readonly first: string;
  readonly last?: string;
  readonly nth?: { first?: number; last?: number };
}

/** `quoteAnchor`'s reading of each selection of `picks` in the document `markdown` parses to,
 *  beside the page's, for the same characters. */
function readBoth(
  markdown: string,
  picks: readonly Pick[]
): { page: unknown; reader: unknown; pick: Pick }[] {
  const doc = loadedEngine()?.parse(markdown);
  if (doc === undefined) throw new Error("the engine parses no document");
  const positions: { pos: number; text: string }[] = [];
  doc.descendants((child, pos) => {
    if (child.isText) positions.push({ pos, text: child.text ?? "" });
    return true;
  });
  const view = drawWords(markdown);
  try {
    const runs = textRuns(view.words).runs;
    expect(
      runs.map((run) => run.node.data),
      "the page draws the text nodes the document holds"
    ).toEqual(positions.map((one) => one.text));
    const text = quotedFrom(markdown);
    return picks.map((pick) => {
      const last = pick.last ?? pick.first;
      const from = locate(view.words, pick.first, pick.nth?.first ?? 0);
      const to = locate(
        view.words,
        last,
        pick.nth?.last ?? (last === pick.first ? (pick.nth?.first ?? 0) : 0)
      );
      const fromRun = runs.findIndex((run) => run.node === from.node);
      const toRun = runs.findIndex((run) => run.node === to.node);
      const start = (positions[fromRun]?.pos ?? 0) + from.at;
      const end = (positions[toRun]?.pos ?? 0) + to.at + last.length;
      selectBetween(view.words, pick.first, last, pick.nth);
      const asked = quoteOf(selected(), view.row, text);
      return {
        page: asked === null ? null : { occurrence: asked.occurrence, quote: asked.quote },
        pick,
        reader: quoteAnchor(doc, start, end),
      };
    });
  } finally {
    view.unmount();
  }
}

describe("a quote reads as quoteAnchor reads it", () => {
  test("a quote across a link and plain text reads as quoteAnchor reads it", () => {
    const answers = readBoth("Follow the [reading guide](https://example.com/guide) first.", [
      { first: "the", last: "first" },
      { first: "reading", last: "guide" },
      { first: "guide", last: " first." },
      { first: "Follow", last: "reading" },
    ]);
    for (const { page, pick, reader } of answers)
      expect(page, JSON.stringify(pick)).toEqual(reader);
    expect(answers[0]?.page).toEqual({ occurrence: 0, quote: "the reading guide first" });
  });

  test("a quote across list items, a hard break and a table cell reads as quoteAnchor reads it", () => {
    const lists = readBoth("- First item\n- Second item\n- First item", [
      { first: "Second", last: "First", nth: { last: 1 } },
      { first: "First item", nth: { first: 1 } },
      { first: "First item" },
    ]);
    const broken = readBoth("line one  \nline two", [{ first: "one", last: "line two" }]);
    const emphasis = readBoth("Keep the *first* and `first` lines", [
      { first: "first" },
      { first: "first", nth: { first: 1 } },
      { first: "the ", last: "first", nth: { last: 1 } },
    ]);
    const table = readBoth("| a | b |\n| --- | --- |\n| c | a |", [
      { first: "a" },
      { first: "a", nth: { first: 1 } },
      { first: "b", last: "c" },
    ]);
    for (const { page, pick, reader } of [...lists, ...broken, ...emphasis, ...table]) {
      expect(page, JSON.stringify(pick)).toEqual(reader);
    }
    // The selection ends after the word "First" of the third item, so no second "item" is in it.
    expect(lists[0]?.page).toEqual({ occurrence: 0, quote: "Second item First" });
    expect(lists[1]?.page).toEqual({ occurrence: 1, quote: "First item" });
    expect(broken[0]?.page).toEqual({ occurrence: 0, quote: "one line two" });
    expect(emphasis[1]?.page).toEqual({ occurrence: 1, quote: "first" });
    expect(table[1]?.page).toEqual({ occurrence: 1, quote: "a" });
  });
});

/** The recorded answers of the real server at B0. */
function recorded<Shape>(name: string): Shape {
  return JSON.parse(readFileSync(new URL(`./fixtures/${name}`, import.meta.url), "utf8")) as Shape;
}

const CONVERSATION = recorded<{ read: TreeAnswer }>("tree-b0-conversation.json").read;

/** A comment of the recorded answer, with its text, its reading and its blocks, drawn as a row. */
function recordedItem(id: string): {
  readonly blocks: TreeAnswerNode[];
  readonly quoted: QuotedNode<TreeAnswerNode>;
  readonly row: TreeNode;
} {
  const found = (CONVERSATION.nodes ?? []).find((one) => one.id === id);
  if (found === undefined || found.text_tree === undefined) {
    throw new Error(`the recorded answer holds no ${id} with a reading`);
  }
  const blocks = itemBlocksOf(CONVERSATION, id);
  return {
    blocks,
    quoted: { blocks, text: found.text, text_tree: found.text_tree },
    row: node({ id, text: found.text, tree: found.text_tree, type: "comment" }),
  };
}

describe("the block of an item", () => {
  test("the block of an item is the block that holds the selection's offset", () => {
    const { blocks, quoted, row } = recordedItem("post");
    expect(
      blocks.map((block) => block.block_id),
      "the answer hangs three blocks under the post, in order"
    ).toEqual(["b1", "b2", "b3"]);
    const view = drawTree([row]);
    try {
      const { row: drawn, words } = located(view, "post");
      for (const [pick, index, quote] of [
        ["sample has room", 0, "sample has room"],
        ["order", 1, "order"],
        ["Second item", 2, "Second item"],
      ] as const) {
        selectBetween(words, pick);
        const asked = quoteOf(selected(), drawn, quoted);
        expect(asked?.quote, pick).toBe(quote);
        expect(asked?.block, `the block of "${pick}"`).toBe(blocks[index]);
        expect(asked?.occurrence, `the match of "${pick}" in its block`).toBe(0);
      }
    } finally {
      view.unmount();
    }
  });

  test("the same words in two blocks name the block of the selection", () => {
    const { blocks, quoted, row } = recordedItem("threadsS1[0]");
    expect(blocks.map((block) => block.block_id)).toEqual(["b1", "b2", "b3"]);
    expect(blocks[1]?.text, "the second and third blocks hold the same words").toBe(
      blocks[2]?.text
    );
    const view = drawTree([row]);
    try {
      const { row: drawn, words } = located(view, "threadsS1[0]");
      // The second paragraph's words, then the third's: the same characters, two blocks.
      selectBetween(words, "Keep these lines", "Keep these lines", { first: 1, last: 1 });
      const second = quoteOf(selected(), drawn, quoted);
      selectBetween(words, "Keep these lines", "Keep these lines", { first: 2, last: 2 });
      const third = quoteOf(selected(), drawn, quoted);
      expect(second?.block).toBe(blocks[1]);
      expect(third?.block).toBe(blocks[2]);
      expect(second?.quote).toBe("Keep these lines");
      expect(third?.quote).toBe(second?.quote);
      // Each counts the matches inside its own block, so neither sees the other.
      expect(second?.occurrence).toBe(0);
      expect(third?.occurrence).toBe(0);
    } finally {
      view.unmount();
    }
  });

  test("a selection across two blocks or two nodes gives no quote", () => {
    const { quoted, row } = recordedItem("post");
    const other = comment("other", "Another comment with its own words.");
    const view = drawTree([row, other]);
    try {
      const first = located(view, "post");
      const second = located(view, "other");
      // Inside one block the selection takes the control, so a null below is the rule's.
      selectBetween(first.words, "sample has room");
      expect(quoteOf(selected(), first.row, quoted), "one block").not.toBeNull();
      selectBetween(first.words, "sample", "order");
      expect(quoteOf(selected(), first.row, quoted), "two blocks of one item").toBeNull();
      selectBetween(first.words, "order", "Second item");
      expect(quoteOf(selected(), first.row, quoted), "the second and third blocks").toBeNull();

      const range = document.createRange();
      const from = locate(first.words, "sample");
      const to = locate(second.words, "own words");
      range.setStart(from.node, from.at);
      range.setEnd(to.node, to.at + 3);
      setSelection(range);
      expect(quoteOf(selected(), first.row, quoted), "from the post into another row").toBeNull();
      expect(
        quoteOf(selected(), second.row, quotedFrom("Another comment with its own words.")),
        "seen from the row it ends in"
      ).toBeNull();
    } finally {
      view.unmount();
    }
  });
});

/** The ids of the lens rows the kinds below draw. */
const LENS = "c3000000-0000-4000-8000-000000000003";
const FINDING = "f6000000-0000-4000-8000-000000000006";
const RISK = "d4000000-0000-4000-8000-000000000004";
const STANDARD = "e5000000-0000-4000-8000-000000000005";
const MEASURED = "e5000000-0000-4000-8000-000000000007";

const VERDICT = "Read dispatch://CORE-1/spec#b-r1  before   the plan.";
const RESULT = "The run took 4 s.";

/** A lens with a verdict naming an address, a finding under it, a risk with its early warning and
 *  its prevention, a constraint with a standard and one with a measurement, and a result, each
 *  drawn as the outline draws it (`buildIssueTree` and `drawTree`). */
function drawKinds(): RenderResult {
  const root = buildIssueTree({
    issue: issueDetails(),
    tree: treeAnswer(
      [
        treeNode(KEY, "issue", null, { text: "The issue page" }),
        treeNode(LENS, "lens", KEY, { attributes: { verdict: VERDICT }, text: VERDICT }),
        treeNode(FINDING, "block", LENS, { text: "A finding of the lens." }),
        treeNode(RISK, "risk", LENS, {
          attributes: {
            early_warning: "Watch the queue grow.",
            heading: "Replies break.",
            prevention: "Keep one reply open.",
          },
          text: "Replies break.",
        }),
        treeNode(STANDARD, "constraint", KEY, {
          attributes: { standard: "RFC 9110" },
          text: "RFC 9110",
        }),
        treeNode(MEASURED, "constraint", KEY, {
          attributes: { measurement: "Latency", value: "200 ms" },
          text: "Latency",
        }),
      ],
      [],
      { converted: true, tree: "artifact-tree" }
    ),
  });
  return drawTree([...root.children, node({ id: "result", text: RESULT, type: "result" })]);
}

describe("the words of each kind", () => {
  test("a lens's verdict, a finding's text, a risk's heading, a constraint's standard and a result's observed words take the control", () => {
    const view = drawKinds();
    try {
      const cases: readonly [string, string, QuotedNode, string, string][] = [
        [LENS, "before", { text: VERDICT }, "before", "before"],
        [FINDING, "finding of", { text: "A finding of the lens." }, "finding of", "finding of"],
        [RISK, "Replies", { text: "Replies break." }, "Replies break.", "Replies break."],
        [STANDARD, "RFC 9110", { text: "RFC 9110" }, "RFC 9110", "RFC 9110"],
        [MEASURED, "Latency", { text: "Latency" }, "Latency", "Latency"],
        ["result", "took 4", { text: RESULT }, "took 4", "took 4"],
      ];
      for (const [id, from, held, to, quote] of cases) {
        const { row, words } = located(view, id);
        selectBetween(words, from, to);
        const asked = quoteOf(selected(), row, held);
        expect(asked, `the row of ${id}`).toEqual({ block: null, occurrence: 0, quote });
      }
    } finally {
      view.unmount();
    }
  });

  test("a constraint's words that no standard or measurement holds take none", () => {
    const view = drawKinds();
    try {
      const { row, words } = located(view, MEASURED);
      // The measurement is the node's text; the value drawn after it is another attribute's.
      selectBetween(words, "Latency");
      expect(quoteOf(selected(), row, { text: "Latency" })?.quote).toBe("Latency");
      selectBetween(words, "200 ms");
      expect(quoteOf(selected(), row, { text: "Latency" }), "the value").toBeNull();
      selectBetween(words, "Latency", "200 ms");
      expect(quoteOf(selected(), row, { text: "Latency" }), "across the measurement").toBeNull();
    } finally {
      view.unmount();
    }
  });

  test("a risk's early warning takes none", () => {
    const view = drawKinds();
    try {
      const { row, words } = located(view, RISK);
      const held: QuotedNode = { text: "Replies break." };
      selectBetween(words, "Replies break.");
      expect(quoteOf(selected(), row, held), "the heading").not.toBeNull();
      for (const [line, words] of [
        ["early_warning", "Watch the queue grow."],
        ["prevention", "Keep one reply open."],
      ] as const) {
        const drawn = row.querySelector(`[data-lens-line="${line}"]`);
        if (drawn === null) throw new Error(`the risk draws no ${line} line`);
        selectBetween(drawn, words);
        expect(quoteOf(selected(), row, held), `the ${line} line`).toBeNull();
      }
      // From the heading into the early warning is no selection of the node's text either.
      const heading = locate(words, "Replies");
      const warning = locate(row, "Watch the queue grow.");
      const range = document.createRange();
      range.setStart(heading.node, heading.at);
      range.setEnd(warning.node, warning.at + 5);
      setSelection(range);
      expect(quoteOf(selected(), row, held), "from the heading into the early warning").toBeNull();
    } finally {
      view.unmount();
    }
  });

  test("a shortened address is quoted from the node's text", () => {
    const view = drawKinds();
    try {
      const { row, words } = located(view, LENS);
      const held: QuotedNode = { text: VERDICT };
      expect(words.textContent, "the row draws the address short and one space for each run").toBe(
        "Read CORE-1 spec block before the plan."
      );
      // The words after the address, with the node's own white space between them.
      selectBetween(words, "before the plan.");
      expect(quoteOf(selected(), row, held)).toEqual({
        block: null,
        occurrence: 0,
        quote: "before   the plan.",
      });
      // Part of the short form quotes the whole address, as the node's text writes it.
      selectBetween(words, "spec block", "before");
      expect(quoteOf(selected(), row, held)?.quote).toBe("dispatch://CORE-1/spec#b-r1  before");
      selectBetween(words, "Read", "CORE-1");
      expect(quoteOf(selected(), row, held)?.quote).toBe("Read dispatch://CORE-1/spec#b-r1");
    } finally {
      view.unmount();
    }
  });
});

describe("what a row draws that is no part of the node's text", () => {
  const SHORT = "dispatch://CORE-1/spec#b-r1";

  test("a reference's drawn name takes none, and the words beside it take the control", () => {
    const text = "See dispatch://CORE-2 and more words.";
    const names = new Map([["dispatch://CORE-2", "Draw the children"]]);
    const view = drawWords(text, { edited: new Set(), names });
    try {
      const held = quotedFrom(text);
      expect(textRuns(view.words).text, "the address the text writes").toBe(text);
      selectBetween(view.words, "and more words");
      expect(quoteOf(selected(), view.row, held)).toEqual({
        block: null,
        occurrence: 0,
        quote: "and more words",
      });
      selectBetween(view.words, "Draw the children");
      expect(quoteOf(selected(), view.row, held), "the name").toBeNull();
      selectBetween(view.words, "See", "more");
      expect(quoteOf(selected(), view.row, held), "across the link").toBeNull();
      // The key the link draws stands for the address.
      selectBetween(view.words, "CORE-2");
      expect(quoteOf(selected(), view.row, held)?.quote).toBe("dispatch://CORE-2");
    } finally {
      view.unmount();
    }
  });

  test("a chip's words and an edited mark take none", () => {
    const text = `See ${SHORT} first.`;
    const names = new Map([[SHORT, "The page draws the plan."]]);
    const view = drawWords(text, { edited: new Set([SHORT]), names });
    try {
      const held = quotedFrom(text);
      selectBetween(view.words, "See");
      expect(quoteOf(selected(), view.row, held)?.quote, "the words before").toBe("See");
      selectBetween(view.words, "first.");
      expect(quoteOf(selected(), view.row, held)?.quote, "the words after").toBe("first.");
      selectBetween(view.words, "page draws");
      expect(quoteOf(selected(), view.row, held), "the chip's words").toBeNull();
      selectBetween(view.words, "edited");
      expect(quoteOf(selected(), view.row, held), "the edited mark").toBeNull();
    } finally {
      view.unmount();
    }
  });

  test("a reference not yet named is its short form, which stands for the whole address", () => {
    const text = `See ${SHORT} first.`;
    const view = drawWords(text);
    try {
      const held = quotedFrom(text);
      const runs = textRuns(view.words);
      expect(runs.text).toBe(text);
      selectBetween(view.words, "spec block");
      expect(quoteOf(selected(), view.row, held)?.quote).toBe(SHORT);
      selectBetween(view.words, "See", "first.");
      expect(quoteOf(selected(), view.row, held)?.quote).toBe(text);
    } finally {
      view.unmount();
    }
  });

  test("a typed block's head takes none", () => {
    const { text, text_tree: tree } = recorded<{
      cases: { text: string; text_tree: DisplayTree }[];
    }>("quotes.json").cases[0] ?? { text: "", text_tree: undefined };
    if (tree === undefined) throw new Error("quotes.json holds no case");
    const view = drawTree([node({ id: "typed", text, tree, type: "decision" })]);
    try {
      const { row, words } = located(view, "typed");
      const head = words.querySelector("[data-proof-block-name]");
      if (head === null) throw new Error("the row draws no typed block head");
      selectBetween(head, "decision");
      expect(quoteOf(selected(), row, { text, text_tree: tree }), "the head").toBeNull();
      selectBetween(words, "blue line");
      expect(quoteOf(selected(), row, { text, text_tree: tree })?.quote, "the words").toBe(
        "blue line"
      );
    } finally {
      view.unmount();
    }
  });

  test("a deleted line and a node's ties take none", () => {
    const text = "Words that were deleted.";
    const chip = {
      answered: true,
      editedAt: "2026-11-05T00:00:00Z",
      gone: false,
      name: "Another block",
      ref: "dispatch://CORE-1/spec#b-r2",
    };
    const view = drawTree([
      node({
        deletedAt: "2026-11-05T00:00:00Z",
        id: "deleted",
        text,
        tree: readingOf(text),
        type: "comment",
      }),
      node({ chips: [chip], id: "tied", text: "Words with a tie.", type: "comment" }),
    ]);
    try {
      const deleted = located(view, "deleted");
      const line = deleted.words.querySelector("[data-post-deleted]");
      if (line === null) throw new Error("the row draws no deleted line");
      expect(textRuns(deleted.words).runs, "the deleted line draws no run").toEqual([]);
      selectBetween(line, "Deleted");
      expect(quoteOf(selected(), deleted.row, quotedFrom(text)), "the deleted line").toBeNull();

      const tied = located(view, "tied");
      const trail = tied.words.querySelector("[data-node-trail]");
      if (trail === null) throw new Error("the row draws no trail of ties");
      const held: QuotedNode = { text: "Words with a tie." };
      selectBetween(tied.words, "with a tie");
      expect(quoteOf(selected(), tied.row, held)?.quote, "the words").toBe("with a tie");
      selectBetween(trail, "Another block");
      expect(quoteOf(selected(), tied.row, held), "the tie's name").toBeNull();
      expect(textRuns(tied.words).text, "the tie is no part of the text").toBe("Words with a tie.");
    } finally {
      view.unmount();
    }
  });
});

describe("rangeOf", () => {
  test("rangeOf answers the DOM range of a stored quote at its occurrence", () => {
    const text = "Pick the blue line or the blue square.";
    const view = drawWords(text);
    try {
      const first = rangeOf(view.row, "blue", 0);
      const second = rangeOf(view.row, "blue", 1);
      expect(first?.toString()).toBe("blue");
      expect(second?.toString()).toBe("blue");
      expect(first?.startOffset).toBe(9);
      expect(second?.startOffset).toBe(26);
      expect(rangeOf(view.row, "blue", 2), "a third match the words do not hold").toBeNull();
      expect(rangeOf(view.row, "green", 0), "words that are gone").toBeNull();
    } finally {
      view.unmount();
    }
  });

  test("a range runs across a link and the text beside it", () => {
    const text = "Follow the [reading guide](https://example.com/guide) first.";
    const view = drawWords(text);
    try {
      expect(rangeOf(view.row, "the reading guide first", 0)?.toString()).toBe(
        "the reading guide first"
      );
    } finally {
      view.unmount();
    }
  });

  test("a range of overlapping matches starts at each", () => {
    const view = drawWords("aaaa");
    try {
      for (const occurrence of [0, 1]) {
        const range = rangeOf(view.row, "aaa", occurrence);
        expect(range?.toString()).toBe("aaa");
        expect(range?.startOffset).toBe(occurrence);
      }
      expect(rangeOf(view.row, "aaa", 2)).toBeNull();
    } finally {
      view.unmount();
    }
  });

  test("a range in a block of an item stands in that block", () => {
    const { blocks, quoted, row } = recordedItem("threadsS1[0]");
    const view = drawTree([row]);
    try {
      const { row: drawn, words } = located(view, "threadsS1[0]");
      const paragraphs = [...words.querySelectorAll("p")];
      for (const [index, block] of blocks.entries()) {
        // The words stand in each block, and the range stands in the block asked for.
        const range = rangeOf(drawn, "Keep these lines", 0, { block, node: quoted });
        expect(range?.toString(), `block ${index}`).toBe("Keep these lines");
        expect(paragraphs[index]?.contains(range?.startContainer ?? null), `block ${index}`).toBe(
          true
        );
      }
      // Words that one block holds and another does not stand in no range of the other.
      const second = blocks[1];
      if (second === undefined) throw new Error("the item holds no second block");
      expect(rangeOf(drawn, "On thread", 0, { block: second, node: quoted })).toBeNull();
    } finally {
      view.unmount();
    }
  });

  test("a range reaches words a row draws read to plain words, from the node's text", () => {
    const view = drawKinds();
    try {
      const { row } = located(view, LENS);
      const held: QuotedNode = { text: VERDICT };
      const range = rangeOf(row, "before   the plan.", 0, { node: held });
      expect(range?.toString()).toBe("before the plan.");
      expect(
        rangeOf(row, `${"dispatch://CORE-1/spec#b-r1"}  before`, 0, { node: held })?.toString()
      ).toBe("CORE-1 spec block before");
    } finally {
      view.unmount();
    }
  });
});

/** The recorded server answers for each inline kind a row draws (gate B0). */
interface Fixture {
  readonly item: string;
  readonly plainText: string;
  readonly text: string;
  readonly text_tree: DisplayTree;
  readonly writes: readonly {
    readonly answer: {
      readonly anchor?: { readonly block_id: string; readonly quote: string };
      readonly code?: string;
      readonly status: number;
    };
    readonly selection: {
      readonly occurrence: number;
      readonly offsets: { readonly end: number; readonly start: number } | null;
      readonly quote: string;
    };
  }[];
}

const FIXTURES = recorded<{ cases: Fixture[] }>("quotes.json").cases;

describe("the server takes each fixture's quote at its occurrence", () => {
  for (const fixture of FIXTURES) {
    test(fixture.item, () => {
      const view = drawTree([
        node({ id: fixture.item, text: fixture.text, tree: fixture.text_tree, type: "decision" }),
      ]);
      try {
        const { row, words } = located(view, fixture.item);
        const runs = textRuns(words);
        expect(runs.text, "the page's plain text is the server's").toBe(fixture.plainText);
        const held: QuotedNode = { text: fixture.text, text_tree: fixture.text_tree };
        for (const { answer, selection } of fixture.writes) {
          if (answer.status === 201) {
            if (selection.offsets === null)
              throw new Error("the server took a quote it never held");
            selectOffsets(words, selection.offsets.start, selection.offsets.end);
            const asked = quoteOf(selected(), row, held);
            expect(asked?.quote, `occurrence ${selection.occurrence}'s quote`).toBe(
              answer.anchor?.quote
            );
            expect(asked?.occurrence, `the occurrence of ${selection.quote}`).toBe(
              selection.occurrence
            );
            continue;
          }
          // The server finds no such match (400 INVALID_ANCHOR or 404 TARGET_NOT_FOUND): the page
          // counts as many matches as the server does, so the occurrence is past all of them.
          expect(["INVALID_ANCHOR", "TARGET_NOT_FOUND"]).toContain(answer.code ?? "");
          expect(selection.offsets).toBeNull();
          expect(matchCount(runs.text, selection.quote)).toBeLessThanOrEqual(selection.occurrence);
        }
      } finally {
        view.unmount();
      }
    });
  }
});

describe("a row site", () => {
  test("a selection in a row site the list does not name takes no control", async () => {
    stubs = answerSeededReads({ threads: [] });
    const client = newClient();
    const view = drawPage(client, "verification");
    try {
      // The statement's row names its words element, so a selection in it takes the control.
      const statement = await waitFor(
        () => {
          const found = view.container.querySelector<HTMLElement>("[data-verification-card]");
          expect(found, "the Verification view draws no statement").not.toBeNull();
          return found as HTMLElement;
        },
        { timeout: 4000 }
      );
      selectBetween(statement, "draws the plan");
      expect(quoteOf(selected(), statement, { text: STATEMENT_WORDS })?.quote).toBe(
        "draws the plan"
      );
      // The issue's own row draws counts of its statements and no node's words: it is no site.
      const issue = view.container.querySelector<HTMLElement>("[data-verification-issue]");
      if (issue === null) throw new Error("the Verification view draws no issue row");
      const drawn = issue.querySelector("[data-author-text]");
      if (drawn === null) throw new Error("the issue row draws no words");
      const range = document.createRange();
      range.selectNodeContents(drawn);
      setSelection(range);
      expect(selected().isCollapsed, "the selection holds words").toBe(false);
      expect(quoteOf(selected(), issue, { text: drawn.textContent ?? "" })).toBeNull();
    } finally {
      view.finish();
      client.clear();
    }
  });

  test("a Plan step's own words, drawn with no RowText, take the control", async () => {
    // The Plan view's rows draw the task's own words with no `RowText`: the words element is the
    // author-text element itself.
    stubs = answerSeededReads({ threads: [] });
    const client = newClient();
    const view = drawPage(client, "plan");
    try {
      const bar = await waitFor(
        () => {
          const found = view.container.querySelector<HTMLElement>("[data-plan-bar-row]");
          expect(found, "the Plan view draws no step").not.toBeNull();
          return found as HTMLElement;
        },
        { timeout: 4000 }
      );
      selectBetween(bar, "beside the spec");
      expect(quoteOf(selected(), bar, { text: PLAN_TASK_WORDS })).toEqual({
        block: null,
        occurrence: 0,
        quote: "beside the spec",
      });
    } finally {
      view.finish();
      client.clear();
    }
  });
});
