import { afterAll, beforeAll, expect, type Mock, spyOn, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, type RenderResult, render, waitFor } from "@testing-library/react";
import { type ReactNode, useState } from "react";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { api } from "../../api/client";
import { issueCoverageQuery, issueResultsQuery } from "../../api/queries";
import type { DisplayTree, IssueChild, IssueDetails, RequirementCoverage } from "../../api/types";
import { issueDecisionsQuery } from "../doc/spec-marks";
import { MarginProvider } from "../margin/margin-context";
import { DESKTOP_WIDTH, stubViewport } from "../margin/margin-fixture";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { POST_MARK_WORDS } from "../refs/post-marks";
import {
  NOTHING_PUBLISHED,
  PublishedReferenceNames,
  ReferenceNamesProvider,
} from "../refs/reference-names";
import { KeymapProvider } from "../shell/KeymapProvider";
import { useKeymapScope } from "../shell/keymap";
import { DecisionsFilterBody } from "./decisions-filter/DecisionsFilterBody";
import {
  answeredAsk,
  decision,
  decisionNode,
  decisionsRead,
  issueDetails,
  issueTree,
  KEY,
  node,
  openAsk,
  READER,
  shaped,
  specRef,
  statement,
} from "./decisions-filter/decisions-fixtures";
import { FocusedNodeProvider } from "./focused-node";
import { artifactsBySlug } from "./issue-artifacts";
import type { TreeNode } from "./issue-tree";
import { NodeRow } from "./NodeRow";
import { NoTooltips } from "./NoTooltips";
import { TWO_THREADS } from "./outline-fixtures";
import { NO_FAILED_READS } from "./outline-reads";
import { OutlineRequestProvider } from "./outline-request";
import {
  BLOCK_ATTRIBUTE_LABELS,
  EMPTY_STATE_LINES,
  NODE_TYPE_NAMES,
  type NodeType,
  PLAN_WORDS,
  VERIFICATION_WORDS,
} from "./page-words";
import { PlanFilterBody } from "./plan-filter/PlanFilterBody";
import { presetView } from "./presets";
import { Publisher, publishedTree } from "./projection-fixture";
import { RowText } from "./RowText";
import { ViewStage } from "./slots/stage";
import { commentNodes, drawStage, stubReads, treeOf } from "./slots/stage-fixtures";
import { SpecFilterBody } from "./spec-filter/SpecFilterBody";
import {
  coverageRead,
  decisionsRead as specDecisionsRead,
  issueDetails as specIssueDetails,
  statement as specStatement,
} from "./spec-filter/spec-fixtures";
import { blockNodeId } from "./tree-drafts";
import { treeAnswer, treeNode } from "./tree-read/tree-fixtures";
import { outlineQuery } from "./tree-read/tree-read";
import { issueAskThreadsQuery } from "./useIssueTree";
import { VerificationFilterBody } from "./verification-filter/VerificationFilterBody";
import {
  check,
  checkNode,
  ISSUE_KEY,
  result,
  statementNode,
} from "./verification-filter/verification-fixtures";

/**
 * Element Timing's attribute on each row the load rows read (Verification v4, "Element Timing").
 *
 * The load rows read when a row is first painted from Chromium's Element Timing entries
 * (`e2e/load-instruments.ts`), and Chromium makes an entry only for an element that carries
 * `elementtiming` and paints text nodes itself: Paint Timing gives a painted text node to "the
 * Element which determines the containing block of text", the nearest element around it that
 * lays out a box of its own rather than flowing inline. A row's own element holds its text in
 * the blocks inside it, so the attribute stands on the block that holds the start of the row's
 * first line, and on nothing else of the row: the init script ties each entry to its row by
 * `entry.element.closest(...)` with the rows' selector (`e2e/issue-page-load.ts`'s
 * `LOAD_ROW_SELECTOR`, copied below as `ROW_SELECTOR`).
 *
 * A row's first line starts with the first words it draws at its left edge, which each case
 * names: a node's words, after a bullet that draws none (the margin at the right end of that line
 * stands before them in the document, and starts no line); the line a row draws over its node's
 * words where it draws one, a Verification statement's state and the kind's word over a node an
 * address named; a child's key in the children's section; a step's own line and a cycle's word in
 * the Plan chart; and the count the Verification issue's row opens on.
 *
 * A unit test's DOM lays nothing out, so which element is that block is read here as the page's
 * styles decide it: the element's own `display` (the defaults of its tag, and the rules of
 * `styles.css` that set a display for a row's words or for a drawn text, which the case appends,
 * `wordsDisplaySheet`), the Tailwind classes that set a display or take an element out of the
 * inline flow, and a flex or grid container making each of its children a block
 * (`paintedBlockOf`). The page itself chooses the element with no read of style
 * (`useRenderedMarkdown.tsx`'s `markFirstLine`), and the second case holds it to that. Each row is
 * drawn as its view's own unit tests draw it.
 */

/** The identifier the page gives every row's first-line element, which the load rows read. */
const ROW_TIMING = "issue-page-row";

/** Every row the load rows read (`e2e/issue-page-load.ts`'s `LOAD_ROW_SELECTOR`): an entry
 *  belongs to the nearest of these around its element. */
const ROW_SELECTOR = [
  "[data-node-row]",
  "[data-spec-statement]",
  "[data-spec-other]",
  "[data-verification-card]",
  "[data-verification-check]",
  "[data-verification-issue]",
  "[data-plan-bar-row]",
  "[data-plan-cycle-row]",
  "[data-decisions-body] [data-node-row]",
  "[data-child-entry]",
].join(", ");

/** The rules of `styles.css` that set a `display` for an element of a row's words or of a drawn
 *  text (`.dispatch-markdown`), each as a rule of that declaration alone: among them the one that
 *  lays a row's words and a drawn text's last paragraph out inline before the row's trail, which
 *  moves the start of the words' first line into the words' own box, and the ones that lay a
 *  typed block's summary out as flex. Throws where `styles.css` holds no such rule, so no case
 *  passes on rules it never read. */
function wordsDisplaySheet(): HTMLStyleElement {
  const css = readFileSync(join(import.meta.dir, "..", "..", "styles.css"), "utf8").replace(
    /\/\*[\s\S]*?\*\//g,
    ""
  );
  const rules = [...css.matchAll(/([^{};]+)\{([^{}]*)\}/g)].flatMap(
    ([, selectors = "", body = ""]) => {
      const display = body
        .split(";")
        .map((one) => one.replace(/\s+/g, " ").trim())
        .find((one) => one.startsWith("display:"));
      return (selectors.includes("[data-node-words]") ||
        selectors.includes(".dispatch-markdown")) &&
        display !== undefined
        ? [`${selectors.trim()} { ${display}; }`]
        : [];
    }
  );
  if (!rules.some((rule) => rule.includes("[data-node-trail]"))) {
    throw new Error("no rule of styles.css lays a row's words out inline before its trail");
  }
  if (!rules.some((rule) => rule.includes("[data-proof-block-summary]"))) {
    throw new Error("no rule of styles.css lays a typed block's summary out");
  }
  const sheet = document.createElement("style");
  sheet.textContent = rules.join("\n");
  return sheet;
}

/** What a Tailwind class lays an element out as: each `display` utility, and the classes that
 *  take an element out of the inline flow whatever its display (a float, a box positioned out of
 *  the flow), which lay it out as a block. */
const LAYOUT_CLASSES: Readonly<Record<string, string>> = {
  absolute: "block",
  block: "block",
  contents: "contents",
  fixed: "block",
  flex: "flex",
  "float-end": "block",
  "float-left": "block",
  "float-right": "block",
  "float-start": "block",
  "flow-root": "flow-root",
  grid: "grid",
  hidden: "none",
  inline: "inline",
  "inline-block": "inline-block",
  "inline-flex": "inline-flex",
  "inline-grid": "inline-grid",
  "list-item": "list-item",
  table: "table",
};

/** An element as a failure names it: its tag, its `data-` attributes and its first words. */
function describe(element: Element): string {
  const marks = [...element.attributes]
    .filter(({ name }) => name.startsWith("data-"))
    .map(({ name }) => ` ${name}`)
    .join("");
  const words = (element.textContent ?? "").replace(/\s+/g, " ").trim().slice(0, 40);
  return `<${element.localName}${marks}> "${words}"`;
}

/** How `element` is laid out: by its Tailwind class where it has one (`LAYOUT_CLASSES`), else by
 *  its computed `display`, which is its tag's own or the one an appended rule sets, an inline
 *  element's read as "inline". Throws on a class that lays it out so at some widths only, which
 *  this test cannot tell from the others. */
function displayOf(element: Element): string {
  let display: string | undefined;
  for (const name of element.classList) {
    const utility = name.slice(name.lastIndexOf(":") + 1);
    if (!Object.hasOwn(LAYOUT_CLASSES, utility)) continue;
    if (utility !== name) {
      throw new Error(`${describe(element)} is laid out by ${name} at some widths only`);
    }
    display = LAYOUT_CLASSES[utility];
  }
  return display ?? (getComputedStyle(element).display || "inline");
}

/** The element Paint Timing gives `text` to, the one whose `elementtiming` an entry for it reads:
 *  the nearest element around it that lays out a box of its own. An inline element is one where
 *  its parent lays its children out as flex or grid items, which makes each a block; an element
 *  laid out as `contents` draws no box, and its children are laid out by its own parent. */
function paintedBlockOf(text: Text): Element {
  for (let element = text.parentElement; element !== null; element = element.parentElement) {
    const display = displayOf(element);
    if (display === "none") throw new Error(`${describe(element)} is not drawn`);
    if (display === "contents") continue;
    if (display !== "inline") return element;
    let parent = element.parentElement;
    while (parent !== null && displayOf(parent) === "contents") parent = parent.parentElement;
    if (parent !== null && /^(inline-)?(flex|grid)$/u.test(displayOf(parent))) return element;
  }
  throw new Error("the text stands in no block");
}

/** The first text node of `row` that is drawn and begins with `first`, which is where the row's
 *  first line starts. A text inside a hidden element of the row, or inside a shape, is not drawn. */
function textStarting(row: Element, first: string): Text {
  const walker = document.createTreeWalker(row, NodeFilter.SHOW_TEXT);
  for (let found = walker.nextNode(); found !== null; found = walker.nextNode()) {
    const unseen = found.parentElement?.closest("[hidden], svg");
    if (unseen != null && row.contains(unseen)) continue;
    if (found instanceof Text && found.data.trimStart().startsWith(first)) return found;
  }
  throw new Error(`the row draws no text that begins "${first}": ${describe(row)}`);
}

/**
 * The assertion of every group: of the elements whose row is `row` (the row itself, and each
 * element inside it whose nearest row is this one), exactly one carries `elementtiming`, with the
 * page's identifier, and it is the element that paints the text `first` begins
 * (`paintedBlockOf`). `holder`, where a case gives it, is a selector the element must match,
 * which pins down what the case is about where the page's styles move it out of the paragraph.
 */
function expectTimed(row: Element, first: string, what: string, holder?: string): void {
  const expected = paintedBlockOf(textStarting(row, first));
  const timed = [row, ...row.querySelectorAll("[elementtiming]")].filter(
    (element) => element.hasAttribute("elementtiming") && element.closest(ROW_SELECTOR) === row
  );
  expect(timed.map(describe), `${what}: what carries elementtiming`).toEqual([describe(expected)]);
  expect(timed[0] === expected, `${what}: the element that paints its first line`).toBe(true);
  expect(expected.getAttribute("elementtiming"), `${what}: its identifier`).toBe(ROW_TIMING);
  if (holder !== undefined) {
    expect(expected.matches(holder), `${what}: ${describe(expected)} is ${holder}`).toBe(true);
  }
}

/** Waits for `row` to draw the text `first` begins, and asserts what `expectTimed` asserts. */
async function expectTimedOnceDrawn(
  row: Element,
  first: string,
  what: string,
  holder?: string
): Promise<void> {
  await waitFor(() => textStarting(row, first));
  expectTimed(row, first, what, holder);
}

/** The row `selector` finds in `view`, failing where it finds none. */
function rowIn(view: RenderResult, selector: string): Element {
  const found = view.container.querySelector(selector);
  if (found === null) throw new Error(`nothing is drawn at ${selector}`);
  return found;
}

/** `partial`, with the server's reading of its words as a tree read sends it (`displayTree`). */
function withReading(
  partial: Partial<TreeNode> & { id: string; text: string; type: NodeType }
): TreeNode {
  const tree = displayTree(partial.text);
  return node({ ...partial, ...(tree === undefined ? {} : { tree }) });
}

/** The page's own `issue` scope, which every key of a view's body is registered in. */
function IssueScope(): ReactNode {
  useKeymapScope("issue");
  return null;
}

/** A query client for one drawn body, whose answers never go stale. */
function testClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
}

let sheet: HTMLStyleElement | undefined;

beforeAll(async () => {
  await warmMarkdownRenderer();
  sheet = wordsDisplaySheet();
  document.head.append(sheet);
});

afterAll(() => {
  sheet?.remove();
});

// ---------------------------------------------------------------------------------------------
// The outline's rows (`NodeRow.tsx`), each kind of node and each way a row draws its first line.
// ---------------------------------------------------------------------------------------------

/** What the outline draws a row under (`NodeRow.drawn.test.tsx`'s `renderRow`): the names store,
 *  with nothing published, and `NoTooltips`, as the page's slots draw every row. */
function RowProviders({ children }: { children: ReactNode }): ReactNode {
  const [client] = useState(
    () => new QueryClient({ defaultOptions: { queries: { retry: false } } })
  );
  return (
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <ReferenceNamesProvider>
          <PublishedReferenceNames published={NOTHING_PUBLISHED}>
            <NoTooltips>{children}</NoTooltips>
          </PublishedReferenceNames>
        </ReferenceNamesProvider>
      </MemoryRouter>
    </QueryClientProvider>
  );
}

/** One row of the outline, drawn as the outline draws every row, with no focus. */
function drawRow(
  drawn: TreeNode,
  { answerer, expanded, folded }: { answerer?: string; expanded?: boolean; folded?: number } = {}
): RenderResult {
  return render(
    <RowProviders>
      <NodeRow
        {...(answerer === undefined ? {} : { answerer })}
        author={{ kind: "person", name: "alice", reader: false }}
        context={false}
        {...(expanded === undefined ? {} : { expanded })}
        {...(folded === undefined ? {} : { folded })}
        focused={false}
        isClosed={false}
        level={1}
        node={drawn}
        onFocus={() => {}}
        onToggle={() => {}}
        ownAnswer={false}
        rowKey="row"
        tabStop={false}
      />
    </RowProviders>
  );
}

/** One task of the page's thread, inside `act`, so what a paint across tasks does in it lands. */
async function nextTask(): Promise<void> {
  await act(async () => {
    const task = Promise.withResolvers<void>();
    setTimeout(() => task.resolve(), 0);
    await task.promise;
  });
}

/** A reading whose paragraphs are `lines`, as the server reads a decision's text that holds the
 *  source lines of a typed block (`RowText.tsx`'s `DIRECTIVE_LINE`): each line a paragraph. */
function paragraphs(lines: readonly string[]): DisplayTree {
  return {
    content: lines.map((line) => ({ content: [{ text: line, type: "text" }], type: "paragraph" })),
    type: "doc",
  };
}

const CHILD: IssueChild = {
  active_at: "2026-11-05T12:00:00Z",
  external_links: null,
  key: "CORE-2",
  needs: ["CORE-3"],
  status: "backlog",
  subtree_done: 0,
  subtree_total: 0,
  title: "Draw the children in the outline",
};

/** Each kind of row the outline draws, with the words its first line begins and, where the
 *  page's styles move that line's element out of the words' first paragraph, what it is. Built
 *  once the engine has loaded, which the server's reading of each text is read with
 *  (`withReading`). */
function outlineCases(): readonly {
  readonly drawn: TreeNode;
  readonly first: string;
  readonly holder?: string;
  readonly options?: { answerer?: string; expanded?: boolean; folded?: number };
  readonly what: string;
}[] {
  return [
    {
      drawn: withReading({ id: "comment-1", text: "Plain words of a comment.", type: "comment" }),
      first: "Plain words",
      holder: "p",
      what: "a comment's paragraph",
    },
    {
      drawn: withReading({
        id: "comment-2",
        text: "**Bold** words open this comment.\n\nA second paragraph.",
        type: "comment",
      }),
      first: "Bold",
      holder: "p",
      what: "a comment whose first word is bold",
    },
    {
      drawn: withReading({
        id: "message-1",
        text: "## A heading of a message\n\nThe words under it.",
        type: "message",
      }),
      first: "A heading",
      holder: "h2",
      what: "a message that opens on a heading",
    },
    {
      drawn: withReading({
        id: "comment-3",
        text: "- The first item of a list\n- The second item of it",
        type: "comment",
      }),
      first: "The first item",
      holder: "li",
      what: "a comment that is a list",
    },
    {
      drawn: withReading({
        id: "comment-typed",
        text: ":::requirement{#r9}\nThe page draws a typed block.\n:::",
        type: "comment",
      }),
      first: "requirement",
      holder: "[data-proof-block-summary] > *",
      what: "a comment that is a typed block, whose summary line is laid out as flex",
    },
    {
      drawn: withReading({
        gaps: ["shaped_disagreement"],
        id: "spec#b-r1",
        reference: specRef("r1"),
        statement: true,
        text: "The page draws r1 with its gap after it.",
        type: "requirement",
      }),
      first: "The page draws r1",
      holder: "[data-node-words]",
      what: "a statement of one paragraph with its trail after it",
    },
    {
      drawn: withReading({
        gaps: ["shaped_disagreement"],
        id: "spec#b-r2",
        reference: specRef("r2"),
        statement: true,
        text: "The page draws r2.\n\nIt draws nothing else before its trail.",
        type: "requirement",
      }),
      first: "The page draws r2",
      holder: "p",
      what: "a statement of two paragraphs with its trail after the last",
    },
    {
      drawn: node({
        id: "spec#b-d1",
        reference: specRef("d1"),
        text: ":::decision{#d1}\n\nWe keep the rail at 56 px.\n\n:::",
        tree: paragraphs([":::decision{#d1}", "We keep the rail at 56 px.", ":::"]),
        type: "decision",
      }),
      first: "We keep the rail",
      holder: "p",
      what: "a decision whose text opens on a typed block's source line",
    },
    {
      drawn: openAsk("q-open", true, {
        options: [{ description: "Where the rail fits.", label: "1280 px" }, { label: "1024 px" }],
        text: "At which width does the header condense?",
      }),
      first: "At which width",
      holder: "p",
      what: "an open question with its options",
    },
    {
      drawn: answeredAsk(
        "a-answered",
        { at: "2026-11-14T09:00:00Z", selected: ["Works"], text: "It works for me." },
        {
          options: [{ label: "Works" }, { label: "Changes needed" }],
          text: "Does the built page work for you?",
        }
      ),
      first: "Does the built page",
      holder: "p",
      options: { answerer: "alice" },
      what: "an answered question with its answer under it",
    },
    {
      drawn: node({ child: CHILD, id: "CORE-2", text: CHILD.title, type: "child_issue" }),
      first: CHILD.title,
      holder: "p",
      what: "a child issue",
    },
    {
      drawn: withReading({
        children: [node({ id: "reply-1", type: "comment" })],
        id: "comment-4",
        text: "The words of a folded branch.",
        type: "comment",
      }),
      first: "The words of a folded",
      holder: "p",
      options: { expanded: false, folded: 1 },
      what: "a folded branch's one line",
    },
    {
      drawn: node({
        deletedAt: "2026-11-05T12:00:00Z",
        id: "comment-5",
        text: "Deleted",
        type: "comment",
      }),
      first: POST_MARK_WORDS.deleted,
      holder: "p",
      what: "a deleted comment's line",
    },
    {
      drawn: node({
        deletedAt: "2026-11-05T12:00:00Z",
        gaps: ["shaped_disagreement"],
        id: "comment-6",
        text: "Deleted",
        type: "comment",
      }),
      first: POST_MARK_WORDS.deleted,
      holder: "[data-node-words]",
      what: "a deleted comment's line with its trail after it",
    },
  ];
}

/** The outline's rows, each kind of node and each way a row draws its first line. */
async function outlineRows(): Promise<void> {
  for (const { drawn, first, holder, options, what } of outlineCases()) {
    const view = drawRow(drawn, options);
    try {
      await expectTimedOnceDrawn(
        rowIn(view, "[data-node-row]"),
        first,
        `the outline: ${what}`,
        holder
      );
    } finally {
      view.unmount();
    }
  }
}

/** How many items the long list holds: a body of more nodes than one task puts into the page
 *  (`useRenderedMarkdown.tsx`'s `ATTACH_BATCH`, 500), which is painted across tasks. */
const LONG_LIST = 300;

/** The words of a comment that is a list of `LONG_LIST` items. */
const LONG_LIST_TEXT = Array.from(
  { length: LONG_LIST },
  (_, at) => `- Item ${at + 1} of a long list`
).join("\n");

/** A row whose words are painted across tasks (`paintsAcrossTasks`) carries the attribute on its
 *  first slice: on the first look at it that finds part of its list drawn, the first item is the
 *  row's one element with `elementtiming`, before the last slice has landed. */
async function rowPaintedAcrossTasks(): Promise<void> {
  const view = drawRow(withReading({ id: "comment-long", text: LONG_LIST_TEXT, type: "comment" }));
  try {
    const row = rowIn(view, "[data-node-row]");
    // What the first look that finds part of the list drawn found carrying `elementtiming`.
    const firstSlice: { items?: number; timed?: readonly string[] } = {};
    const look = (): number => {
      const list = row.querySelector("[data-node-words] ul");
      const items = list?.children.length ?? 0;
      if (firstSlice.items === undefined && items > 0 && items < LONG_LIST) {
        firstSlice.items = items;
        firstSlice.timed = [...row.querySelectorAll("[elementtiming]")].map(
          (element) =>
            `${element === list?.firstElementChild ? "the first item" : describe(element)}=${element.getAttribute("elementtiming")}`
        );
      }
      return items;
    };
    for (let tasks = 0; look() < LONG_LIST; tasks += 1) {
      if (tasks > 1000) throw new Error("the long list never landed whole");
      await nextTask();
    }
    expect(firstSlice.items ?? LONG_LIST, "the list was drawn in one task").toBeLessThan(LONG_LIST);
    expect(firstSlice.timed, "what carries elementtiming on the first slice").toEqual([
      `the first item=${ROW_TIMING}`,
    ]);
    expectTimed(row, "Item 1 of", "the outline: a list painted across tasks", "li");
  } finally {
    view.unmount();
  }
}

// ---------------------------------------------------------------------------------------------
// What choosing the first line's element reads: no computed style and no layout.
// ---------------------------------------------------------------------------------------------

/** A text a row's words draw, whether the row's trail follows it in the words' box, and when it
 *  has landed whole in what is drawn. */
interface CountedText {
  readonly landed: (drawn: HTMLElement) => boolean;
  readonly text: string;
  readonly trail: boolean;
  readonly tree: DisplayTree | undefined;
  readonly what: string;
}

/** The texts a row's words draw that the count is read over: one paragraph laid out inline before
 *  the row's trail (`styles.css`), a decision whose first paragraph is a typed block's source
 *  line, which the row hides after the paint (`RowText.tsx`), and a list painted across tasks. */
function countedTexts(): readonly CountedText[] {
  const decision = [":::decision{#d1}", "We keep the rail at 56 px.", ":::"];
  return [
    {
      landed: (drawn) => drawn.querySelector("[data-node-words] p") !== null,
      text: "The page draws r1 with its gap after it.",
      trail: true,
      tree: displayTree("The page draws r1 with its gap after it."),
      what: "one paragraph before the row's trail",
    },
    {
      landed: (drawn) => drawn.querySelector("[data-node-words] p[hidden]") !== null,
      text: decision.join("\n\n"),
      trail: false,
      tree: paragraphs(decision),
      what: "a decision that opens on a typed block's source line",
    },
    {
      landed: (drawn) => drawn.querySelector("[data-node-words] ul")?.children.length === LONG_LIST,
      text: LONG_LIST_TEXT,
      trail: false,
      tree: displayTree(LONG_LIST_TEXT),
      what: "a list painted across tasks",
    },
  ];
}

/** How many times the page reads computed style (`getComputedStyle`) and layout
 *  (`getBoundingClientRect`) from the render that mounts a row's words drawing `counted`, with
 *  `elementTiming` naming the element that paints its first line or with none, until the text has
 *  landed whole and one task more has run; and how many elements carry `elementtiming` then. The
 *  words keep their tab stops (`tabStops`, as the Verification view's do): the pass that takes
 *  them out of the tab order reads style each time its observer reports a change of the words
 *  (`useOutOfTabOrder`), which a paint across tasks lands in as many reports as the engine's
 *  timing makes, so what is counted is what the paint itself reads. */
async function readsOfPaint(
  counted: CountedText,
  elementTiming: string | undefined
): Promise<{ readonly layout: number; readonly style: number; readonly timed: number }> {
  const style = spyOn(globalThis, "getComputedStyle");
  const layout = spyOn(Element.prototype, "getBoundingClientRect");
  try {
    const view = render(
      <RowProviders>
        <div data-node-row="">
          <div data-node-words="">
            <RowText
              {...(elementTiming === undefined ? {} : { elementTiming })}
              markdown={counted.text}
              tabStops
              trailFollows={counted.trail}
              {...(counted.tree === undefined ? {} : { tree: counted.tree })}
            />
            {counted.trail ? <span data-node-trail="">CORE-2</span> : null}
          </div>
        </div>
      </RowProviders>
    );
    try {
      for (let tasks = 0; !counted.landed(view.container); tasks += 1) {
        if (tasks > 1000) throw new Error(`${counted.what} never landed whole`);
        await nextTask();
      }
      await nextTask();
      return {
        layout: layout.mock.calls.length,
        style: style.mock.calls.length,
        timed: view.container.querySelectorAll("[elementtiming]").length,
      };
    } finally {
      view.unmount();
    }
  } finally {
    style.mockRestore();
    layout.mockRestore();
  }
}

/** Choosing the element that paints a row's first line reads no computed style and no layout,
 *  each of which would make the engine recalculate style, or lay the page out, in the task that
 *  paints each row: the same words painted with the row's identifier and with none read style
 *  and layout the same number of times, and only the first marks an element. */
async function markingReadsNoStyle(): Promise<void> {
  for (const counted of countedTexts()) {
    const unmarked = await readsOfPaint(counted, undefined);
    const marked = await readsOfPaint(counted, ROW_TIMING);
    expect(unmarked.timed, `${counted.what}: elements marked with no identifier`).toBe(0);
    expect(marked.timed, `${counted.what}: elements marked with the row's identifier`).toBe(1);
    expect(
      { layout: marked.layout, style: marked.style },
      `${counted.what}: the style and layout read with the row's identifier`
    ).toEqual({ layout: unmarked.layout, style: unmarked.style });
  }
}

// ---------------------------------------------------------------------------------------------
// Conversation drawn by the page's stage: the children's section's rows and the outline's.
// ---------------------------------------------------------------------------------------------

/** Two children of the issue, in the issue read's order. */
const CHILDREN: readonly IssueChild[] = ["CORE-2", "CORE-3"].map((key) => ({
  active_at: "2026-11-05T12:00:00Z",
  external_links: null,
  key,
  status: "todo",
  subtree_done: 0,
  subtree_total: 1,
  title: `The child ${key}`,
}));

/** Conversation as the page's stage draws it (`children-section.test.tsx`): the issue's two
 *  threads and its two children in the outline, and the children's section above it. */
async function conversationRows(): Promise<void> {
  const restoreViewport = stubViewport(DESKTOP_WIDTH);
  const restore = stubReads();
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async () =>
    treeOf([
      ...commentNodes(TWO_THREADS),
      ...CHILDREN.map((child) => treeNode(child.key, "issue", KEY, { text: child.title })),
    ])
  );
  const issue: IssueDetails = { ...issueDetails(), children: [...CHILDREN] };
  const view = drawStage("", { issue, preset: "conversation" });
  try {
    await waitFor(() => {
      expect(view.container.querySelectorAll("[data-child-entry]")).toHaveLength(CHILDREN.length);
    });
    for (const child of CHILDREN) {
      const entry = rowIn(view, `[data-child-entry="${child.key}"]`);
      await expectTimedOnceDrawn(entry, child.key, `the children's section: ${child.key}`);
    }
    const firsts = new Map<string, string>([
      ...TWO_THREADS.map((comment): [string, string] => [comment.id, comment.body]),
      ...CHILDREN.map((child): [string, string] => [child.key, child.title]),
    ]);
    await waitFor(() => {
      expect(
        view.container.querySelectorAll("[data-issue-body] [data-node-row]").length
      ).toBeGreaterThanOrEqual(firsts.size);
    });
    for (const row of view.container.querySelectorAll("[data-issue-body] [data-node-row]")) {
      const id = row.getAttribute("data-node-id") ?? "";
      const first = firsts.get(id);
      if (first === undefined)
        throw new Error(`Conversation drew a row this case names no words of: ${id}`);
      await expectTimedOnceDrawn(row, first, `Conversation: the row of ${id}`);
    }
  } finally {
    view.unmount();
    for (const spy of restore) spy.mockRestore();
    restoreViewport();
  }
}

// ---------------------------------------------------------------------------------------------
// The Spec view's rows (`spec-filter/SpecFilterBody.drawn.test.tsx`'s `drawBody`).
// ---------------------------------------------------------------------------------------------

/** The reads the page's tree fills, each by an observer as the tree's own (`useIssueTree.ts`),
 *  which the Spec body only watches. */
function SpecTreeReads({ issue }: { issue: IssueDetails }): ReactNode {
  useQuery(issueCoverageQuery(issue.key));
  useQuery(issueDecisionsQuery(issue.key, true));
  useQuery(issueResultsQuery(issue.key));
  useQuery(issueAskThreadsQuery(issue.key));
  useQuery(outlineQuery(issue.key, "conversation"));
  return null;
}

/** The Spec view's rows: a statement of one paragraph with its trail after it, one with none, and
 *  a comment an address named, which the view draws by its kind above its words. */
async function specRows(): Promise<void> {
  const tree = issueTree([
    specStatement("r1", { gaps: ["shaped_disagreement"] }),
    specStatement("r2"),
    node({ id: "comment-1", text: "A remark the address names.", type: "comment" }),
  ]);
  const issue = specIssueDetails();
  const client = testClient();
  const results = { checks: [], issue: KEY, verification_artifact_id: "" };
  const spies = [
    spyOn(api, "getIssueDecisions").mockResolvedValue(specDecisionsRead([])),
    spyOn(api, "getIssueResults").mockResolvedValue(results),
    spyOn(api, "getIssueCoverage").mockResolvedValue(coverageRead([])),
    spyOn(api, "listIssueAskThreads").mockResolvedValue([]),
    spyOn(api, "getIssueTree").mockResolvedValue(treeAnswer([])),
  ];
  client.setQueryData(["decisions", KEY], specDecisionsRead([]));
  client.setQueryData(issueResultsQuery(KEY).queryKey, results);
  client.setQueryData(issueCoverageQuery(KEY).queryKey, coverageRead([]));
  client.setQueryData(issueAskThreadsQuery(KEY).queryKey, []);
  client.setQueryData(outlineQuery(KEY, "conversation").queryKey, treeAnswer([]));
  const view = render(
    <QueryClientProvider client={client}>
      <ReferenceNamesProvider>
        <Publisher published={publishedTree(tree)} />
        <MemoryRouter>
          <KeymapProvider>
            <MarginProvider>
              <OutlineRequestProvider>
                <FocusedNodeProvider>
                  <IssueScope />
                  <SpecTreeReads issue={issue} />
                  <SpecFilterBody
                    context={new Set()}
                    emptyLine={EMPTY_STATE_LINES.spec}
                    holdsEscape={false}
                    issue={issue}
                    reader={READER}
                    root={tree}
                    view={presetView(tree, "spec", "", new Set(["comment-1"])).root}
                  />
                </FocusedNodeProvider>
              </OutlineRequestProvider>
            </MarginProvider>
          </KeymapProvider>
        </MemoryRouter>
      </ReferenceNamesProvider>
    </QueryClientProvider>
  );
  try {
    await expectTimedOnceDrawn(
      rowIn(view, '[data-spec-statement][data-node-id="spec#b-r1"]'),
      "R1.",
      "Spec: a statement with its trail after its words",
      "[data-node-words]"
    );
    await expectTimedOnceDrawn(
      rowIn(view, '[data-spec-statement][data-node-id="spec#b-r2"]'),
      "R2.",
      "Spec: a statement with no trail",
      "p"
    );
    await expectTimedOnceDrawn(
      rowIn(view, "[data-spec-other]"),
      NODE_TYPE_NAMES.comment,
      "Spec: a row the address named that is no statement"
    );
  } finally {
    view.unmount();
    for (const spy of spies) spy.mockRestore();
  }
}

// ---------------------------------------------------------------------------------------------
// The Verification view's rows (`verification-filter/VerificationFilterBody.drawn.test.tsx`).
// ---------------------------------------------------------------------------------------------

/** The coverage read's rows for `statements`, one for each with the checks drawn under it. */
function coverageOf(statements: readonly TreeNode[]): RequirementCoverage[] {
  return statements.flatMap((one) =>
    one.statement === true && one.reference !== undefined
      ? [
          {
            block_id: one.reference.slice(one.reference.lastIndexOf("#b-") + "#b-".length),
            checks: one.children.filter((child) => child.type === "check").length,
            gaps: [],
            plan_tasks: 0,
            ref: one.reference,
            text: one.text,
          },
        ]
      : []
  );
}

/** The Verification view's rows: the issue's row, a statement with a check under it and one with
 *  none, a check that serves no statement, and a comment an address named. */
async function verificationRows(): Promise<void> {
  const met = check({
    block_id: "c-one",
    pass_stands: { answer: "yes" },
    results: [result({ check_block_id: "c-one", id: "result-c-one" })],
  });
  const loose = check({
    block_id: "c-loose",
    pass_stands: { answer: "yes" },
    results: [result({ check_block_id: "c-loose", id: "result-c-loose" })],
  });
  const named = node({ id: "comment-1", text: "A comment an address named.", type: "comment" });
  const under = [
    statementNode("r1", "The page draws the checks.", [checkNode(met)]),
    statementNode("r2", "Nothing serves this one.", []),
    checkNode(loose),
    named,
  ];
  const issue = node({ children: under, id: ISSUE_KEY, text: "An issue", type: "issue" });
  const shown = presetView(issue, "verification", "", new Set([named.id]));
  const published = publishedTree(issue);
  const view = render(
    <QueryClientProvider client={testClient()}>
      <ReferenceNamesProvider>
        <MemoryRouter>
          <NoTooltips>
            <Publisher published={published} />
            <VerificationFilterBody
              checks={[met, loose]}
              context={shown.context}
              namesLine={NO_FAILED_READS}
              published={published}
              reads={{ state: "answered" }}
              requirements={coverageOf(under)}
              view={shown.root}
              whole={undefined}
            />
          </NoTooltips>
        </MemoryRouter>
      </ReferenceNamesProvider>
    </QueryClientProvider>
  );
  try {
    await expectTimedOnceDrawn(
      rowIn(view, "[data-verification-issue]"),
      "1",
      "Verification: the issue's row"
    );
    for (const id of ["r1", "r2"]) {
      const card = rowIn(
        view,
        `[data-verification-card][data-node-id="${blockNodeId("spec", id)}"]`
      );
      const state = card.getAttribute(
        "data-statement-state"
      ) as keyof typeof VERIFICATION_WORDS.states;
      await expectTimedOnceDrawn(
        card,
        VERIFICATION_WORDS.states[state],
        `Verification: the statement ${id}`
      );
    }
    await expectTimedOnceDrawn(
      rowIn(view, "[data-verification-check]:not([data-verification-loose])"),
      "The page draws c-one",
      "Verification: a check under its statement",
      "p"
    );
    await expectTimedOnceDrawn(
      rowIn(view, "[data-verification-check][data-verification-loose]"),
      "The page draws c-loose",
      "Verification: a check that serves no statement",
      "p"
    );
    await expectTimedOnceDrawn(
      rowIn(view, "[data-verification-other]"),
      NODE_TYPE_NAMES.comment,
      "Verification: a row the address named that is neither a statement nor a check"
    );
  } finally {
    view.unmount();
  }
}

// ---------------------------------------------------------------------------------------------
// The Plan view's rows (`plan-filter/PlanFilterBody.test.tsx`'s `renderBody`).
// ---------------------------------------------------------------------------------------------

const PLAN = "plan-md";

/** One task of the plan, by its block id, with a `needs` chip for each of `needs`. */
function planTask(blockId: string, needs: readonly string[] = []): TreeNode {
  const ref = (id: string) => `dispatch://${KEY}/artifact/${PLAN}#b-${id}`;
  return node({
    chips: needs.map((needed) => ({
      answered: true,
      gone: false,
      label: BLOCK_ATTRIBUTE_LABELS.needs,
      ref: ref(needed),
    })),
    id: blockNodeId(PLAN, blockId),
    reference: ref(blockId),
    text: `Task ${blockId}`,
    type: "plan_task",
  });
}

/** The Plan view's chart: a step's bar row, and the row of a cycle of two tasks that need each
 *  other. */
async function planRows(): Promise<void> {
  const restoreViewport = stubViewport(DESKTOP_WIDTH);
  const tasks = [planTask("t1"), planTask("t2", ["t3"]), planTask("t3", ["t2"])];
  const root = node({ children: tasks, held: tasks.length, id: KEY, text: KEY, type: "issue" });
  const published = publishedTree(root);
  const view = render(
    <ReferenceNamesProvider>
      <MemoryRouter>
        <QueryClientProvider client={testClient()}>
          <KeymapProvider>
            <MarginProvider>
              <FocusedNodeProvider>
                <IssueScope />
                <Publisher published={published} />
                <ViewStage>
                  <PlanFilterBody
                    artifacts={new Map()}
                    context={new Set()}
                    emptyLine={EMPTY_STATE_LINES.plan}
                    focusReveal
                    holdsEscape={false}
                    isClosed={false}
                    namesLine={NO_FAILED_READS}
                    onEditing={() => {}}
                    plan={tasks}
                    planBlocks={undefined}
                    published={published}
                    reads={{ state: "answered" }}
                    reveal={undefined}
                    root={root}
                    rootRow={false}
                  />
                </ViewStage>
              </FocusedNodeProvider>
            </MarginProvider>
          </KeymapProvider>
        </QueryClientProvider>
      </MemoryRouter>
    </ReferenceNamesProvider>
  );
  try {
    await expectTimedOnceDrawn(
      rowIn(view, `[data-plan-bar-row][data-node-id="${blockNodeId(PLAN, "t1")}"]`),
      "Task t1",
      "Plan: a step's row",
      "[data-node-words]"
    );
    await expectTimedOnceDrawn(
      rowIn(view, "[data-plan-cycle-row]"),
      PLAN_WORDS.cycle,
      "Plan: a cycle's row"
    );
  } finally {
    view.unmount();
    restoreViewport();
  }
}

// ---------------------------------------------------------------------------------------------
// The Decisions view's rows (`decisions-filter/DecisionsFilterBody.drawn.test.tsx`).
// ---------------------------------------------------------------------------------------------

/** The Decisions view's rows: an open question, and a decision a statement rests on, which waits
 *  for the reader's review. */
async function decisionsRows(): Promise<void> {
  const children = [
    statement("s1"),
    openAsk("q-open", true, {
      options: [{ label: "1280 px" }, { label: "1024 px" }],
      text: "At which width does the header condense?",
    }),
    decisionNode("first", { at: "2026-11-10T12:00:00Z" }),
  ];
  const made = [
    decision("first", { changed_at: "2026-11-10T12:00:00Z", links: [shaped(specRef("s1"))] }),
  ];
  const client = testClient();
  client.setQueryData(["decisions", KEY], decisionsRead(made));
  client.setQueryData(["asks", KEY, "thread"], []);
  client.setQueryData(["comments", KEY], []);
  const issue = issueDetails();
  const root = issueTree(children);
  const published = publishedTree(root);
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <MarginProvider>
            <ReferenceNamesProvider>
              <Publisher published={published} />
              <IssueScope />
              <NoTooltips>
                <DecisionsFilterBody
                  artifacts={artifactsBySlug(issue)}
                  emptyLine={EMPTY_STATE_LINES.decisions}
                  holdsEscape={false}
                  isClosed={false}
                  issue={issue}
                  namesLine={NO_FAILED_READS}
                  published={published}
                  reader={READER}
                  root={root}
                  view={presetView(root, "decisions", "", new Set()).root}
                />
              </NoTooltips>
            </ReferenceNamesProvider>
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await expectTimedOnceDrawn(
      rowIn(view, '[data-decisions-body] [data-node-row][data-node-id="q-open"]'),
      "At which width",
      "Decisions: an open question",
      "p"
    );
    await expectTimedOnceDrawn(
      rowIn(
        view,
        `[data-decisions-body] [data-node-row][data-node-id="${decisionNode("first").id}"]`
      ),
      "The decision first",
      "Decisions: a decision that waits for review",
      "p"
    );
  } finally {
    view.unmount();
  }
}

// Each row the load rows read, of each view and of the children's section, holds exactly one
// element with `elementtiming="issue-page-row"`, and it is the element that paints the start of
// the row's first line, so a load reads when that row was first painted (R1, Verification v4's
// "Element Timing"). One group for each kind of row.
test("each row of each view and of the children's section has one elementtiming element that owns its first line's text", async () => {
  await outlineRows();
  await rowPaintedAcrossTasks();
  await conversationRows();
  await specRows();
  await verificationRows();
  await planRows();
  await decisionsRows();
});

// The element that paints a row's first line is chosen from what the paint built and what the
// row's host says of its words, with no read of computed style or of layout: a read of either
// after a paint makes the engine recalculate style or lay the page out in that task, once for
// each row the page draws (INFRA-5939's profile of the load at the base).
test("choosing the element that paints a row's first line reads no computed style and no layout", async () => {
  await markingReadsNoStyle();
});
