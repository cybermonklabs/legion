import { beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, type RenderResult, render, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useRef, useState } from "react";
import { MemoryRouter, useLocation, useNavigationType } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { installIntersectionObserver } from "../../__tests__/intersection-observer";
import { api } from "../../api/client";
import type { DisplayTree, IssueChild } from "../../api/types";
import { pageKindBullet, pageKindTag } from "../../theme/classes";
import type { ReplyTarget } from "../conversation/composer-model";
import { heldSends } from "../conversation/held-sends";
import type { SentRequest } from "../conversation/send-request";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { measureNearWords } from "./clamp-fixtures";
import { issueDetails, KEY } from "./decisions-filter/decisions-fixtures";
import type { NodeChip, TreeNode } from "./issue-tree";
import { KeepOpen, NodeOptions, NodeRow, useRowOnScreen } from "./NodeRow";
import { NoTooltips } from "./NoTooltips";
import * as nodeRowChip from "./node-row-chip";
import { type OutlineWriter, OutlineWriterProvider, useOutlineWriter } from "./outline-writer";
import {
  type AuthorKind,
  authorKind,
  BLOCK_TYPE_NAMES,
  CONTROL_LABELS,
  type GapKind,
  NODE_TYPE_NAMES,
  type NodeType,
} from "./page-words";
import { RowText } from "./RowText";
import { ReferenceNamesProvider, usePublishReferenceNames } from "./reference-names";
import { ProvidersSlot } from "./slots/providers";
import { addressesAsShortForms } from "./tree-drafts";
import { overflowing, wordsScrollRules, wordsScrollSheet } from "./words-scroll-fixtures";

/**
 * What one row of the tree draws, read as a reader reads it (R1, R6, R9,
 * R34, and the architect's ruling 1 on R1).
 *
 * The row is rendered on its own rather than through `IssueTree`, because what these rows are
 * about is the words and the shapes one node draws: the tree's fold, its keys and its placements
 * are read in `IssueTree.test.tsx` and `issue-tree.test.ts`. Where a row is cut is read from the
 * browser's own layout, which no DOM of a unit test lays out, so the cut is read by the browser
 * suite's c-row.
 */

function node(partial: Partial<TreeNode> & { id: string; type: NodeType }): TreeNode {
  return {
    alsoUnder: [],
    children: [],
    chips: [],
    facts: [],
    gaps: [],
    held: 0,
    question: false,
    replaced: false,
    text: partial.id,
    transcluded: false,
    waiting: false,
    ...partial,
  };
}

/** Who made a node, as the tree names them (`AgentText.author`). */
type Author = { kind: AuthorKind; name: string; reader: boolean };

/** Who made a node the model holds no author for, as the tree names one (`AgentText.author`). */
const NOBODY_RECORDED: Author = {
  kind: authorKind(undefined),
  name: CONTROL_LABELS.unknown_author,
  reader: false,
};

/** One row, drawn with its body open, as the focused row draws it, unless `body` is false;
 *  `author` is who made it, as the tree names them (`AgentText.author`); `answerer` the person
 *  whose answer an ask carries; `expanded` and `folded` its fold, for a node that holds any;
 *  `focused` whether it holds the tree's focus. It is drawn inside `NoTooltips`, as the page's
 *  slots draw every row, so an author is named as the page names one. `redraw` draws the same
 *  row of another node, as a later read of the tree does. */
function renderRow(
  drawn: TreeNode,
  {
    answerer,
    author = NOBODY_RECORDED,
    body = true,
    editable,
    expanded,
    focused = false,
    folded,
  }: {
    answerer?: string;
    author?: Author;
    body?: boolean;
    editable?: boolean;
    expanded?: boolean;
    focused?: boolean;
    folded?: number;
  } = {}
) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const row = (shown: TreeNode) => (
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <NoTooltips>
          <NodeRow
            {...(answerer === undefined ? {} : { answerer })}
            author={author}
            body={body}
            context={false}
            {...(editable === undefined ? {} : { editable })}
            {...(expanded === undefined ? {} : { expanded })}
            {...(folded === undefined ? {} : { folded })}
            focused={focused}
            isClosed={false}
            level={1}
            node={shown}
            onFocus={() => {}}
            onPin={() => {}}
            onToggle={() => {}}
            ownAnswer={false}
            pinned={false}
            rowKey="row"
            tabStop={false}
          />
        </NoTooltips>
      </MemoryRouter>
    </QueryClientProvider>
  );
  const view = render(row(drawn));
  return Object.assign(view, { redraw: (next: TreeNode) => view.rerender(row(next)) });
}

beforeAll(async () => {
  await warmMarkdownRenderer();
});

// R9: a node whose author the model holds says who made it by a name and a shape, never by
// colour alone and never by an id, and two authors of different kinds never share a shape.
// Ruling 5: a node the model holds no author for draws none on its row, and its opened body
// says so. Ruling 6: an agent is named by its role's word, and its opened body gives its whole
// title.
test("an author is drawn by a name and a shape of its own kind", () => {
  // A comment, which a person and an agent both write: a decision's body draws Keep and
  // Change, which are writes, and this row is about the words and the shape alone.
  const drawn = (author: Author, body = true) => {
    const view = renderRow(node({ id: "comment-1", type: "comment" }), { author, body });
    try {
      const shown = view.container.querySelector("[data-node-author]");
      const shape = shown?.querySelector("svg[data-glyph]");
      return {
        body: shown === null ? null : shown.closest("[data-node-body]") !== null,
        chrome: shown === null ? null : shown.querySelector("[data-page-chrome]") !== null,
        margin: shown === null ? null : shown.closest("[data-node-margin]") !== null,
        name: shape?.getAttribute("aria-label") ?? null,
        role: shape?.getAttribute("role") ?? null,
        text: shown?.textContent ?? null,
        title: view.container.querySelector("[data-node-author-title]")?.textContent ?? null,
        token: shape?.getAttribute("data-glyph") ?? null,
      };
    } finally {
      view.unmount();
    }
  };
  // The word is the author's name, and the shape is drawn beside it rather than in place of it,
  // exactly as the type's shape and the state's shape are, at the right of the row (R34). The
  // shape carries its own accessible name: a reader who cannot see it still hears what kind of
  // author it stands for.
  expect(drawn({ kind: "person", name: "alice", reader: false })).toEqual({
    body: false,
    chrome: false,
    margin: true,
    name: "Written by a person",
    role: "img",
    text: "alice",
    title: null,
    token: "author-person",
  });
  expect(drawn({ kind: "agent", name: "Planner", reader: false })).toEqual({
    body: false,
    chrome: false,
    margin: true,
    name: "Written by an agent",
    role: "img",
    text: "Planner",
    title: null,
    token: "author-agent",
  });
  // An agent whose session's title says more than its role is named by the role's word on the
  // row, and the opened body gives the whole title; a row that is not open gives none.
  const architect: Author = {
    kind: "agent",
    name: "Legion architect · CORE-1",
    reader: false,
  };
  expect(drawn(architect)).toMatchObject({
    margin: true,
    text: "architect",
    title: "Legion architect · CORE-1",
  });
  expect(drawn(architect, false)).toMatchObject({ margin: true, text: "architect", title: null });
  // Where the model holds no author the row draws none, and its opened body says so in the
  // page's own words, which are marked as the page's rather than as somebody's name, with a
  // shape neither other kind draws.
  expect(drawn(NOBODY_RECORDED, false)).toMatchObject({ margin: null, text: null });
  expect(drawn(NOBODY_RECORDED)).toEqual({
    body: true,
    chrome: true,
    margin: false,
    name: "Written by an author nobody recorded",
    role: "img",
    text: "Author unknown",
    title: null,
    token: "author-unknown",
  });
});

// A view that edits rows in place passes `editable`, and the row element itself carries
// `data-editable` for that view's style. Every other view passes nothing, and its rows carry none.
test("a row carries data-editable where its view passes editable, and none by default", () => {
  const marked = (editable?: boolean) => {
    const view = renderRow(node({ id: "comment-1", type: "comment" }), { editable });
    try {
      const row = view.container.querySelector("[data-node-row]");
      if (row === null) throw new Error("the row draws no [data-node-row]");
      return {
        anywhere: view.container.querySelectorAll("[data-editable]").length,
        row: row.getAttribute("data-editable"),
      };
    } finally {
      view.unmount();
    }
  };
  expect(marked(true), "editable left the row unmarked").toEqual({ anywhere: 1, row: "" });
  // The negative controls: a row its view passes nothing, and one passed false, carry none.
  expect(marked(), "a row with no editable is marked").toEqual({ anywhere: 0, row: null });
  expect(marked(false), "a row passed editable false is marked").toEqual({
    anywhere: 0,
    row: null,
  });
});

// R34: a typed node draws its kind as one tag, its word and its shape, in the margin at the
// right of its row and never before its words; a turn of the conversation draws no tag. R9: the
// time it was made is drawn after its author in the page's short form.
test("a typed node draws its kind as one tag at the right, and a turn draws none", () => {
  // Noon of a day of the year the test runs in, so its day is the 5th from UTC-11 to UTC+11 and
  // the page's short form draws no year.
  const at = `${new Date().getFullYear()}-11-05T12:00:00Z`;
  const drawn = (type: NodeType, more: Partial<TreeNode> = {}) => {
    const view = renderRow(
      node({ at, id: `${type}-1`, text: "Words of the node.", type, ...more })
    );
    try {
      const tag = view.container.querySelector("[data-node-tag]");
      return {
        afterWords: tag === null || tag.closest("[data-node-margin]") !== null,
        tag: tag?.textContent ?? null,
        token: tag?.querySelector("svg")?.getAttribute("data-glyph") ?? null,
        time: view.container.querySelector("[data-node-time]")?.textContent ?? null,
      };
    } finally {
      view.unmount();
    }
  };
  expect(drawn("requirement", { statement: true })).toEqual({
    afterWords: true,
    tag: NODE_TYPE_NAMES.requirement,
    time: "5 Nov",
    token: "requirement",
  });
  expect(drawn("check")).toMatchObject({ tag: NODE_TYPE_NAMES.check, token: "check" });
  expect(drawn("comment")).toMatchObject({ tag: null, time: "5 Nov" });
  expect(drawn("message")).toMatchObject({ tag: null });
});

// Decision 246: a `requirement` block is a statement only in the issue's spec (`isStatement`).
// The same block of the plan, or of any other document, is a block and nothing more, so its row
// draws its type's own word and none of a statement's marks - not its word, its shape or its
// colour - as a `check` block outside the Verification document draws no state word.
test("a plan's requirement row reads requirement with no statement mark, and a spec's reads as a statement", () => {
  const drawn = (id: string, more: Partial<TreeNode>) => {
    const view = renderRow(
      node({ id, text: "The page draws no raw source.", type: "requirement", ...more })
    );
    try {
      const tag = view.container.querySelector("[data-node-margin] [data-node-tag]");
      const dot = view.container.querySelector("[data-node-bullet] i");
      return {
        bullet: dot?.classList.contains(pageKindBullet.statement) ?? null,
        colour: tag?.classList.contains(pageKindTag.statement) ?? null,
        tag: tag?.textContent ?? null,
        token: tag?.querySelector("svg")?.getAttribute("data-glyph") ?? null,
      };
    } finally {
      view.unmount();
    }
  };
  expect(drawn("plan-md#b-r1", {}), "a plan's requirement row drew a statement's mark").toEqual({
    bullet: false,
    colour: false,
    tag: BLOCK_TYPE_NAMES.requirement,
    token: null,
  });
  expect(
    drawn("spec-md#b-r1", { statement: true }),
    "a spec's requirement row did not draw a statement"
  ).toEqual({
    bullet: true,
    colour: true,
    tag: NODE_TYPE_NAMES.requirement,
    token: "requirement",
  });
});

// R1, ruling 1: a folded branch is the one place a row draws one line of its node: the first
// line, cut at its first sentence end, and how many nodes it holds. The focus draws its words
// whole, and keeps the count of what the fold still hides after them.
test("a folded branch draws its first line and its count, and its focus its words", () => {
  const text = "The page draws the whole issue in one reading. Nothing is lost on the way.";
  const folded = node({ held: 3, id: "comment-1", text, type: "comment" });
  const shut = renderRow(folded, { body: false, expanded: false, folded: 3 });
  try {
    // A line that is a whole sentence ends on the sentence's own mark, with no ellipsis after it.
    expect(shut.container.querySelector("[data-node-first-line]")?.textContent).toBe(
      "The page draws the whole issue in one reading."
    );
    expect(shut.container.querySelector("[data-node-count]")?.textContent).toBe(
      `3 ${CONTROL_LABELS.folded_rows}`
    );
    expect(shut.container.querySelector("[data-node-words]")).toBeNull();
    // The bullet is the branch's fold control, named by what pressing it does.
    expect(shut.getByRole("button", { name: CONTROL_LABELS.unfold })).toBeDefined();
  } finally {
    shut.unmount();
  }
  const focused = renderRow(folded, { body: true, expanded: false, folded: 3 });
  try {
    expect(focused.container.querySelector("[data-node-first-line]")).toBeNull();
    expect(
      focused.container.querySelector("[data-node-words] [data-author-text]")?.textContent
    ).toBe(text);
    expect(
      focused.container.querySelector("[data-node-words] [data-node-count]")?.textContent,
      "a focused folded branch no longer says how many rows it hides"
    ).toBe(`3 ${CONTROL_LABELS.folded_rows}`);
  } finally {
    focused.unmount();
  }
});

// Decision 95: a folded branch's one line writes a bare address in its words as its target's
// short form, as every one line of a node does (`lineWords`); its body keeps the address as its
// author wrote it.
test("a folded branch's line writes a bare address as its short form", () => {
  const address = "dispatch://CORE-1/spec#b-r1";
  const text = `Read ${address} before the rows.`;
  const folded = node({ held: 1, id: "comment-1", text, tree: displayTree(text), type: "comment" });
  const shut = renderRow(folded, { body: false, expanded: false, folded: 1 });
  try {
    const line = shut.container.querySelector("[data-node-first-line]")?.textContent ?? "";
    expect(line, "the folded line draws the address as written").not.toContain("dispatch://");
    expect(line).toBe(`Read ${addressesAsShortForms(address)} before the rows.`);
  } finally {
    shut.unmount();
  }
});

// R1, R9, ruling 1: an answered ask carries the person's answer in its own row,
// and no row of its own: who answered by a name and a shape, what they chose among the ask's own
// options, and what they wrote as the store holds it, character for character and whole.
test("an answered ask draws the answer in its own row, word for word", () => {
  const text = "The preset row keeps its labels above that width.\n\nBelow it, one control.";
  const asked = node({
    answer: {
      at: "2026-11-06T00:00:00Z",
      by: { id: "alice", kind: "user" },
      selected: ["1280 px"],
      text,
    },
    id: "ask-1",
    options: [{ description: "Where the rail fits.", label: "1280 px" }, { label: "1024 px" }],
    state: "answered",
    text: "At which width does the identity block condense?",
    type: "ask",
  });
  const planner: Author = { kind: "agent", name: "Planner", reader: false };
  const view = renderRow(asked, { answerer: "alice", author: planner });
  try {
    const rows = view.container.querySelectorAll("[data-node-row]");
    expect(rows.length).toBe(1);
    const line = view.container.querySelector("[data-node-answer]");
    expect(line?.querySelector("[data-page-chrome]")?.textContent).toBe("Answer");
    expect(line?.querySelector("[data-answer-author]")?.textContent).toBe("alice");
    expect(line?.querySelector("[data-answer-author] svg")?.getAttribute("data-glyph")).toBe(
      "author-person"
    );
    expect(line?.querySelector("[data-answer-text]")?.textContent).toBe(text);
    // The option the person chose is marked in the ask's own list, and the body adds what an
    // option says beside its name.
    expect(
      [...view.container.querySelectorAll("[data-node-option]")].map((one) => [
        one.getAttribute("data-node-option"),
        one.textContent,
      ])
    ).toEqual([
      ["chosen", "●1280 px — Where the rail fits."],
      ["", "○1024 px"],
    ]);
    // The row's own author is still the asker's, and the answerer's name is no row author.
    expect(view.container.querySelector("[data-node-author]")?.textContent).toBe("Planner");
  } finally {
    view.unmount();
  }
});

// R9: a decision the model marks unsure says so in one word on its own row, and a decision it
// does not mark says nothing of the kind.
test("a decision the model marks unsure says so in one word", () => {
  const unsure = (marked: boolean) => {
    const view = renderRow(
      node({ id: "decisions-md#b-d3", type: "decision", ...(marked ? { unsure: true } : {}) })
    );
    try {
      return view.container.querySelector("[data-node-unsure]")?.textContent ?? null;
    } finally {
      view.unmount();
    }
  };
  expect(unsure(true)).toBe("Unsure");
  expect(unsure(false)).toBeNull();
});

/** The few words a row draws for `gap`, and the sentence its body draws under them. */
function drawnGap(drawn: TreeNode): { chip: string | undefined; line: string | undefined } {
  const view = renderRow(drawn);
  try {
    return {
      chip: view.container.querySelector("[data-gap]")?.textContent ?? undefined,
      line: [...view.container.querySelectorAll("[data-node-body] p")]
        .map((paragraph) => paragraph.textContent ?? "")
        .find((text) => text.endsWith(".")),
    };
  } finally {
    view.unmount();
  }
}

// R8: a citation that must name a requirement and names something else is a gap, drawn on the
// node that holds the citation and never as an error.
test("a check whose citation names no statement says so, in words", () => {
  const gaps: GapKind[] = ["serves_not_requirement"];
  expect(drawnGap(node({ gaps, id: "verification-md#b-c1", type: "check" }))).toEqual({
    chip: "Not a statement",
    line: "What this says it serves is not a statement.",
  });
});

// R8: a plan task that names no requirement it serves and no declaration it attempts is work
// no reason stands behind, and the row says that rather than leaving the reader to notice it,
// in words that pass no judgement on the work.
test("a plan task nobody asked for says so, in words", () => {
  const gaps: GapKind[] = ["nobody_asked"];
  expect(drawnGap(node({ gaps, id: "plan-md#b-t9", type: "plan_task" }))).toEqual({
    chip: "No reason given",
    line: "This task names no statement it serves and nothing a person asked for.",
  });
});

/** One row the way the tree draws it: unfocused until a focus lands inside it, and then the
 *  focused row, so a test reads what that focus does to the page. */
function FocusableRow({ drawn }: { drawn: TreeNode }): ReactNode {
  const [focused, setFocused] = useState(false);
  return (
    <NodeRow
      author={NOBODY_RECORDED}
      body
      context={false}
      focused={focused}
      isClosed={false}
      level={1}
      node={drawn}
      onFocus={() => setFocused(true)}
      onPin={() => {}}
      onToggle={() => {}}
      ownAnswer={false}
      pinned={false}
      rowKey="row"
      tabStop={false}
    />
  );
}

// R9 brings the focused row on screen, but a press inside an unfocused row focuses the row
// before the press ends: a scroll then moves what the pointer went down on, and the press ends
// on something else and does nothing. Measured at 1440 px on the Decisions preset, with the
// row's top 24 px under the identity block, the page scrolled 32 px between mousedown and
// mouseup and a reader's Keep was never sent. A key's focus still brings the row on screen.
test("a press inside a row does not scroll the page under it, and a key's focus still does", async () => {
  const user = userEvent.setup();
  const scrollBy = spyOn(window, "scrollBy").mockImplementation(() => {});
  // The row's top sits above the line the identity block leaves clear, so R9 would scroll. On
  // Element, which owns the method: a spy on HTMLElement leaves an own property behind once
  // restored, which hides every later spy on Element (Margin.landing.test.tsx's `stubRects`).
  const box = spyOn(Element.prototype, "getBoundingClientRect").mockReturnValue(
    new DOMRect(0, -24, 400, 300)
  );
  const drawn = node({ id: "comment-1", type: "comment" });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const draw = () =>
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <FocusableRow drawn={drawn} />
        </MemoryRouter>
      </QueryClientProvider>
    );
  const pressed = draw();
  try {
    // The row's words, which a press focuses the row through: Keep open leaves the focus where
    // it is (decision 141).
    const words = pressed.container.querySelector("[data-node-words]");
    if (words === null) throw new Error("the row draws no words");
    await user.pointer({ keys: "[MouseLeft>]", target: words });
    expect(
      pressed.container.querySelector("[data-node-row]")?.contains(document.activeElement)
    ).toBe(true);
    expect(scrollBy, "the press scrolled the page under the pointer").not.toHaveBeenCalled();
    await user.pointer({ keys: "[/MouseLeft]" });
  } finally {
    pressed.unmount();
  }
  const keyed = draw();
  try {
    const row = keyed.container.querySelector<HTMLElement>("[data-node-row]");
    act(() => row?.focus());
    expect(scrollBy, "a key's focus no longer brings the row on screen").toHaveBeenCalledTimes(1);
  } finally {
    keyed.unmount();
    box.mockRestore();
    scrollBy.mockRestore();
  }
});

/** A stored text holding a heading, bold, a list item and a code span: every one of them a
 *  symbol a reader must never see drawn as written. */
const MARKED = "## Store the notices\n\nA **decided** thing.\n\n- one `code` item";

/** What a reader reads of one drawn row: whether its words hold any symbol the author typed,
 *  and what its words format. */
function drawnWords(view: RenderResult) {
  const words = view.container.querySelector("[data-node-words]");
  return {
    bold: words?.querySelector("strong")?.textContent,
    code: words?.querySelector("code")?.textContent,
    item: words?.querySelector("li")?.textContent,
    symbols: /\*\*|##|`/u.test(view.container.querySelector("[data-node-row]")?.textContent ?? ""),
  };
}

/** What `drawnWords` reads of a row drawn from the reading of `MARKED`. */
const MARKED_DRAWN = {
  bold: "decided",
  code: "code",
  item: "one code item",
  symbols: false,
};

// R20, c-plain-words: a decision's text is stored markdown, and the decisions read sends the
// server's reading of it (`Decision.text_tree`). The row draws that reading, so the reader sees
// a heading, bold words and a list, never the `##`, `**` and `-` the author typed, and the
// browser parses nothing to get there. Folded, its one line holds none of those symbols either,
// and is cut from its first block's words alone (`lineWords`): the heading never runs into the
// paragraph after it.
test("a decision whose text holds bold draws no literal asterisks", () => {
  const decision = node({
    id: "decisions-md#b-d1",
    text: MARKED,
    tree: displayTree(MARKED),
    type: "decision",
  });
  const view = renderRow(decision);
  try {
    expect(drawnWords(view)).toEqual(MARKED_DRAWN);
  } finally {
    view.unmount();
  }
  const folded = renderRow({ ...decision, held: 1 }, { body: false, expanded: false, folded: 1 });
  try {
    expect(folded.container.querySelector("[data-node-first-line]")?.textContent).toBe(
      "Store the notices"
    );
  } finally {
    folded.unmount();
  }
});

// The same for every other kind whose read sends the server's reading of its text: an ask's
// question (`question_tree`), a comment's and a message's body (`body_tree`).
test("an ask, a comment and a message draw the server's reading of their text", () => {
  for (const [type, id] of [
    ["ask", "ask-1"],
    ["comment", "comment-1"],
    ["message", "message-1"],
  ] as const) {
    const view = renderRow(node({ id, text: MARKED, tree: displayTree(MARKED), type }));
    try {
      expect({ type, ...drawnWords(view) }).toEqual({ type, ...MARKED_DRAWN });
    } finally {
      view.unmount();
    }
  }
});

// R20: a node whose read sends no reading of its text - a requirement, a check, a result, and a
// block of a document named by the batched reference read - draws the text as stored. The page
// parses no stored markdown of its own to draw it otherwise.
test("a node whose read sends no reading of its text draws the text as stored", () => {
  const view = renderRow(node({ id: "spec#b-r1", text: MARKED, type: "requirement" }));
  try {
    const words = view.container.querySelector("[data-node-words]");
    expect(words?.textContent).toContain("A **decided** thing.");
    expect(words?.querySelector("strong")).toBeNull();
  } finally {
    view.unmount();
  }
});

// Decision 282: at 390 px a note whose body holds a table of sixteen columns, drawn in its row,
// made the whole page 794 px wide. A wide block in a node's words - a table, a code block -
// scrolls sideways inside itself, as a table does in a document's reading, so neither the row
// nor the page does. The row's own blocks are the ones a rule of `styles.css` gives that scroll,
// drawn as a block held to the width of what holds it; the widths are the browser suite's to
// read, since no DOM of a unit test lays any out.
test("a table and a code block in a row's words each scroll sideways on their own", () => {
  const text = "| One | Two |\n| --- | --- |\n| 1 | 2 |\n\n```\nconst wide = true;\n```";
  const view = renderRow(
    node({ id: "notes-md#b-n1", text, tree: displayTree(text), type: "callout" })
  );
  try {
    const rules = wordsScrollRules().filter(({ declared }) =>
      ["display: block", "max-width: 100%"].every((one) => declared.has(one))
    );
    for (const block of ["table", "pre"]) {
      const drawn = view.container.querySelector(`[data-node-words] ${block}`);
      if (drawn === null) throw new Error(`the row's words draw no ${block}`);
      expect(
        rules.some(({ selectors }) => [...document.querySelectorAll(selectors)].includes(drawn)),
        `no rule of styles.css scrolls the ${block} in a row's words sideways on its own`
      ).toBe(true);
    }
  } finally {
    view.unmount();
  }
});

// Decision 317: the outline is one tab stop, and only the focused row's scrollers keep a tab stop
// of their own, each named, so a reader on the keyboard can scroll a wide code block or table
// sideways; every other row's stay out of the tab order. The row hands its focus to its words.
test("out of the tab order, a focused row's code block and table keep their tab stop, named, and an unfocused row's do not", () => {
  const text = "| One | Two |\n| --- | --- |\n| 1 | 2 |\n\n```\nconst wide = true;\n```";
  // The rules that make each block scroll, which the hook reads through the computed style, and
  // each block's content laid out past its box, as a wide one's is.
  const sheet = wordsScrollSheet();
  document.head.append(sheet);
  const restore = overflowing("[data-node-words] pre, [data-node-words] table");
  const drawn = (focused: boolean) => {
    const view = renderRow(
      node({ id: "notes-md#b-n1", text, tree: displayTree(text), type: "callout" }),
      { body: false, focused }
    );
    try {
      return ["pre", "table"].map((block) => {
        const scroller = view.container.querySelector(`[data-node-words] ${block}`);
        if (scroller === null) throw new Error(`the row's words draw no ${block}`);
        return {
          label: scroller.getAttribute("aria-label"),
          role: scroller.getAttribute("role"),
          tabIndex: scroller.getAttribute("tabindex"),
        };
      });
    } finally {
      view.unmount();
    }
  };
  try {
    expect(drawn(true)).toEqual([
      { label: CONTROL_LABELS.scrolls_code, role: "region", tabIndex: "0" },
      { label: CONTROL_LABELS.scrolls_table, role: null, tabIndex: "0" },
    ]);
    expect(drawn(false)).toEqual([
      { label: null, role: null, tabIndex: "-1" },
      { label: null, role: null, tabIndex: "-1" },
    ]);
  } finally {
    restore();
    sheet.remove();
  }
});

// Part G's R4, the tester's F20: a decision's text holds the source of a typed block its section
// quotes, and the row draws the block's words but none of its source lines, whether it draws the
// text as stored or from its reading.
test("a decision whose text quotes a typed block draws none of its source lines", async () => {
  const text = "It names the statement.\n\n:::requirement{#r1}\nThe page draws no raw source.\n:::";
  for (const tree of [undefined, displayTree(text)]) {
    const view = renderRow(
      node({
        id: "decisions-md#b-d1",
        text,
        type: "decision",
        ...(tree === undefined ? {} : { tree }),
      })
    );
    try {
      await waitFor(() => {
        const words = view.container.querySelector("[data-node-words]")?.textContent ?? "";
        expect(words).toContain("The page draws no raw source.");
        expect(words).not.toContain(":::");
      });
    } finally {
      view.unmount();
    }
  }
});

// R9: the tree is one tab stop and nothing inside a row takes one; a key reaches a row's links
// (`useTreeKeymap.ts`'s `rowStops`). Words drawn from a reading hold real links, and each is
// out of the tab order too.
test("the links a row's words draw are out of the tab order", async () => {
  const text =
    "Read [the notes](https://example.com/notes) and [the plan](https://example.com/plan).";
  const view = renderRow(node({ id: "comment-1", text, tree: displayTree(text), type: "comment" }));
  try {
    await waitFor(() => {
      const links = [...view.container.querySelectorAll<HTMLElement>("[data-node-words] a[href]")];
      expect(links.length).toBe(2);
      expect(links.map((link) => link.tabIndex)).toEqual([-1, -1]);
    });
  } finally {
    view.unmount();
  }
});

// Ruling 5: a tie is a chip at the end of the node's words, named by its target's first words
// cut at a word within 32 characters, and never by an address, a slug or a document's name
// (R22). A target the reference read has not named yet draws nothing.
test("a chip names its target by its first words and never by an address", () => {
  const view = renderRow(
    node({
      chips: [
        {
          answered: true,
          gone: false,
          name: "The page draws the whole issue in one reading and nothing is lost.",
          ref: "dispatch://CORE-1/spec#b-r2",
        },
        { answered: false, gone: false, name: "", ref: "dispatch://CORE-1/artifact/plan-md#b-t4" },
        { answered: true, gone: true, name: "", ref: "dispatch://CORE-1/spec#b-r9" },
        {
          answered: true,
          gone: false,
          label: "Needs",
          name: "Add the routes.",
          ref: "dispatch://CORE-1/artifact/plan-md#b-t1",
        },
      ],
      id: "decisions-md#b-d1",
      text: "Keep one reading.",
      type: "decision",
    }),
    { body: false }
  );
  try {
    const chips = [...view.container.querySelectorAll("[data-node-chip]")];
    expect(chips.map((chip) => chip.textContent)).toEqual([
      "The page draws the whole issue…",
      CONTROL_LABELS.target_gone,
      "Needs Add the routes.",
    ]);
    for (const chip of chips) {
      const name = chip.querySelector("[data-chip-name]")?.textContent ?? "";
      expect(name.length, `"${name}" is longer than a chip's 32 characters`).toBeLessThanOrEqual(
        32
      );
    }
    const row = view.container.querySelector("[data-node-row]")?.textContent ?? "";
    expect(row).not.toContain("dispatch://");
    expect(row).not.toMatch(/CORE-1 (spec|plan-md)/u);
    // The chips stand inside the words' own box, after the words, and on no line of their own.
    const trail = view.container.querySelector("[data-node-trail]");
    expect(trail?.parentElement?.hasAttribute("data-node-words")).toBe(true);
    expect(trail?.previousElementSibling?.textContent).toBe("Keep one reading.");
    // The named chip is the link a reader follows to its target.
    expect(chips[0]?.querySelector("a")?.getAttribute("href")).toBe("/issues/CORE-1/spec#b-r2");
  } finally {
    view.unmount();
  }
});

// Decision 66: a target's name is plain words already (`tree-drafts.ts`'s `treeContext` names a
// block by the words of the server's reading of it), so a chip draws it as it stands, with no
// markdown pass, which would read a numbered step as a list and drop its number.
test("a chip named as a numbered step draws its number", () => {
  const view = renderRow(
    node({
      chips: [
        {
          answered: true,
          gone: false,
          label: "Needs",
          name: "1. Stand the harness up.",
          ref: "dispatch://CORE-1/artifact/plan-md#b-t1",
        },
        {
          answered: true,
          gone: false,
          name: "2. Stand the harness up and read the rows",
          ref: "dispatch://CORE-1/artifact/plan-md#b-t3",
        },
      ],
      id: "plan-md#b-t2",
      text: "Draw the rows.",
      type: "plan_task",
    }),
    { body: false }
  );
  try {
    const chips = [...view.container.querySelectorAll("[data-node-chip]")];
    expect(chips.map((chip) => chip.textContent)).toEqual([
      "Needs 1. Stand the harness up.",
      "2. Stand the harness up and…",
    ]);
  } finally {
    view.unmount();
  }
});

// Decisions 62 and 89: a short reference that two or more targets answer to names no one target,
// and the read answers it gone and ambiguous. Its chip draws the reference as its author wrote
// it, as words with no link, and never as a target deleted.
test("a chip of an ambiguous short reference draws the reference as written, not as deleted", () => {
  const ambiguous: NodeChip = {
    ambiguous: true,
    answered: true,
    gone: true,
    name: "",
    ref: "dispatch://CORE-1/comment/c1",
  };
  const view = renderRow(
    node({
      chips: [ambiguous],
      id: "decisions-md#b-d1",
      text: "Keep one reading.",
      type: "decision",
    }),
    { body: false }
  );
  try {
    const chip = view.container.querySelector("[data-node-chip]");
    expect(chip?.textContent).toBe("dispatch://CORE-1/comment/c1");
    expect(chip?.querySelector("a"), "an ambiguous reference is a link to one target").toBeNull();
  } finally {
    view.unmount();
  }
});

/** The names the page's one batched read brought, and the references it read as ambiguous,
 *  published as the page publishes them. */
function Named({
  ambiguous,
  children,
  names,
}: {
  ambiguous?: ReadonlySet<string>;
  children: ReactNode;
  names: ReadonlyMap<string, string>;
}): ReactNode {
  usePublishReferenceNames(names, ambiguous);
  return children;
}

// F18 and decision 66 in a node's words: a chip in place is named from the batched read, which
// the page publishes as plain words (`useIssueTree.ts`), so it draws them with no markdown pass,
// as the chip at the end of the words does, a numbered step's number kept. The words are drawn
// under the page's own providers (`ProvidersSlot`), as the page draws them, so a second reading
// of the names mounted there would fail on the number.
test("a chip in a node's words draws its target's plain words, a numbered step's number kept", async () => {
  const target = "dispatch://CORE-1/artifact/plan-md#b-t1";
  const text = `It follows ${target}.`;
  const names = new Map([[target, "1. Stand the harness up and read the rows"]]);
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  // The reads the page's writer makes (`outline-writer.tsx`), answered before it mounts.
  for (const key of [
    ["asks", KEY],
    ["comments", KEY],
    ["issue-messages", KEY],
  ]) {
    queryClient.setQueryData(key, []);
  }
  const view = render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <ReferenceNamesProvider>
          <Named names={names}>
            <ProvidersSlot issue={issueDetails()} route={{ key: KEY, kind: "issue" }}>
              <RowText markdown={text} tree={displayTree(text)} />
            </ProvidersSlot>
          </Named>
        </ReferenceNamesProvider>
      </MemoryRouter>
    </QueryClientProvider>
  );
  try {
    await waitFor(() => {
      const chip = view.container.querySelector(`a[data-row-reference="${target}"]`);
      expect(chip?.textContent).toBe("1. Stand the harness up and…");
    });
  } finally {
    view.unmount();
  }
});

// Decision 102: the batched read's names are named as chips once for each answer of the read,
// not once for each row. A block's name is its whole reading, so naming every name again for
// every row cost the page seconds on an issue of 650 rows.
test("the read's names are named as chips once per answer, not once per row", async () => {
  const names = new Map(
    Array.from({ length: 30 }, (_unused, at) => [
      `dispatch://CORE-1/spec#b-r${at}`,
      `The words of statement ${at}, which run on well past the room a chip has.`,
    ])
  );
  const target = "dispatch://CORE-1/spec#b-r0";
  const text = `It follows ${target}.`;
  const named = spyOn(nodeRowChip, "chipWords");
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <ReferenceNamesProvider>
          <Named names={names}>
            {Array.from({ length: 20 }, (_unused, at) => (
              // biome-ignore lint/suspicious/noArrayIndexKey: twenty rows of one text.
              <RowText key={at} markdown={text} tree={displayTree(text)} />
            ))}
          </Named>
        </ReferenceNamesProvider>
      </MemoryRouter>
    </QueryClientProvider>
  );
  try {
    await waitFor(() => {
      const chips = view.container.querySelectorAll(`a[data-row-reference="${target}"]`);
      expect(chips).toHaveLength(20);
      expect(chips[19]?.textContent).toBe("The words of statement 0, which…");
    });
    // A row names a reference the read has not answered by its address alone (`""`), which
    // costs nothing; each of the read's names is named once.
    const ofNames = named.mock.calls.filter(([name]) => name !== "");
    expect(ofNames.length, "every row named the read's names again").toBeLessThanOrEqual(
      names.size
    );
  } finally {
    view.unmount();
    named.mockRestore();
  }
});

// Decisions 62 and 89: a short reference two or more targets answer to names none of them, so
// the page publishes it as ambiguous, apart from its names, and a node's words draw it as its
// author wrote it, with no link, never as a target that is gone. A reference the read named
// nothing for and did not read as ambiguous keeps its fallback: a link, by its short form. Both
// the words drawn from their reading and the words drawn as stored are read.
test("an ambiguous short reference in a node's words is drawn as written with no link, and a gone one keeps its link", async () => {
  const written = "dispatch://CORE-1/spec#b-1a2b3c4d";
  const gone = "dispatch://CORE-1/spec#b-9f8e7d6c";
  const text = `It follows ${written} and ${gone}.`;
  const ambiguous: ReadonlySet<string> = new Set([written]);
  const names: ReadonlyMap<string, string> = new Map();
  for (const tree of [displayTree(text), undefined]) {
    const drawn = tree === undefined ? "as stored" : "from their reading";
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const view = render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <ReferenceNamesProvider>
            <Named ambiguous={ambiguous} names={names}>
              <RowText markdown={text} {...(tree === undefined ? {} : { tree })} />
            </Named>
          </ReferenceNamesProvider>
        </MemoryRouter>
      </QueryClientProvider>
    );
    try {
      await waitFor(() =>
        expect(
          view.container.querySelector(`a[data-row-reference="${gone}"]`),
          `the gone reference drawn ${drawn} lost its link`
        ).not.toBeNull()
      );
      expect(
        view.container.querySelector(`a[data-row-reference="${written}"]`),
        `the ambiguous reference drawn ${drawn} is a link`
      ).toBeNull();
      expect(view.container.textContent).toContain(`It follows ${written} and `);
    } finally {
      view.unmount();
    }
  }
});

/** Where the router stands and how it got there, drawn so a test reads them. */
function Where(): ReactNode {
  const at = useLocation();
  const how = useNavigationType();
  return <output data-how={how} data-where="">{`${at.pathname}${at.hash}`}</output>;
}

/** A node's words drawn at the address `at` under the read's `names`, with the router's place
 *  beside them. `click` dispatches a click on `link` and answers whether the browser was left to
 *  follow it, and then stops the browser: the unit suite's document loads no page. */
function drawWordsAt(
  at: string,
  text: string,
  tree: DisplayTree | undefined,
  names: ReadonlyMap<string, string>
) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[at]}>
        <ReferenceNamesProvider>
          <Named names={names}>
            <RowText markdown={text} {...(tree === undefined ? {} : { tree })} />
            <Where />
          </Named>
        </ReferenceNamesProvider>
      </MemoryRouter>
    </QueryClientProvider>
  );
  const place = () => view.container.querySelector("[data-where]");
  return {
    click(link: Element | null, init: MouseEventInit = {}): boolean {
      let left = false;
      const stop = (event: Event) => {
        left = !event.defaultPrevented;
        event.preventDefault();
      };
      document.addEventListener("click", stop);
      try {
        act(() => {
          link?.dispatchEvent(
            new MouseEvent("click", { bubbles: true, cancelable: true, ...init })
          );
        });
      } finally {
        document.removeEventListener("click", stop);
      }
      return left;
    },
    how: () => place()?.getAttribute("data-how"),
    view,
    where: () => place()?.textContent,
  };
}

// A reference in a row's words is a link of this application: a plain click follows it in place,
// as a link the router draws does, and never loads the whole page again. A click with a key
// held is the browser's, which opens it elsewhere. Both the words drawn as stored and the words
// drawn from their reading, whose links the row's own pass makes, are read.
test("a reference in a row's words is followed in place on a plain click", async () => {
  const target = "dispatch://CORE-1/spec#b-r1";
  const text = `It follows ${target}.`;
  const names = new Map([[target, "Draw the rows."]]);
  for (const tree of [undefined, displayTree(text)]) {
    const drawn = drawWordsAt("/issues/CORE-1", text, tree, names);
    try {
      const link = await waitFor(() => {
        const found = drawn.view.container.querySelector(`a[data-row-reference="${target}"]`);
        expect(found?.textContent).toBe("Draw the rows.");
        return found;
      });
      expect(drawn.click(link, { ctrlKey: true }), "a click with a key held was stopped").toBe(
        true
      );
      expect(drawn.where(), "a click with a key held was followed in place").toBe("/issues/CORE-1");
      expect(drawn.click(link), "a plain click was left to load the page").toBe(false);
      expect(drawn.where()).toBe("/issues/CORE-1/spec#b-r1");
      expect(drawn.how()).toBe("PUSH");
    } finally {
      drawn.view.unmount();
    }
  }
});

// The origin also serves paths the server answers itself, which the page has no route for: a
// link to its API is the browser's to load, never the router's, which would draw "not found".
test("a link in a row's words to a path the server answers is left to the browser", async () => {
  const text = "Read [the issue](/api/v1/issues/CORE-1) as JSON.";
  const drawn = drawWordsAt("/issues/CORE-1", text, displayTree(text), new Map());
  try {
    const link = await waitFor(() => {
      const found = drawn.view.container.querySelector('a[href="/api/v1/issues/CORE-1"]');
      expect(found).not.toBeNull();
      return found;
    });
    expect(drawn.click(link), "the router followed a path the server answers").toBe(true);
    expect(drawn.where()).toBe("/issues/CORE-1");
  } finally {
    drawn.view.unmount();
  }
});

// The router's own `Link` replaces the reader's place when it points at that place, so Back
// leaves the page; a link in a row's words does the same, as a tie's chip to that address does.
test("a link in a row's words to the address the reader is at replaces it", async () => {
  const target = "dispatch://CORE-1/spec#b-r1";
  const text = `It follows ${target}.`;
  const drawn = drawWordsAt(
    "/issues/CORE-1/spec#b-r1",
    text,
    undefined,
    new Map([[target, "Draw the rows."]])
  );
  try {
    const link = await waitFor(() => {
      const found = drawn.view.container.querySelector(`a[data-row-reference="${target}"]`);
      expect(found).not.toBeNull();
      return found;
    });
    expect(drawn.click(link)).toBe(false);
    expect(drawn.where()).toBe("/issues/CORE-1/spec#b-r1");
    expect(drawn.how(), "a link to the reader's own place pushed a second entry").toBe("REPLACE");
  } finally {
    drawn.view.unmount();
  }
});

/** Lays every element out `height` px tall for the rows drawn until the answer is called: no
 *  DOM of a unit test lays anything out, and the cut reads the words' own boxes
 *  (`clamp-measurer.ts`'s `measureClamp`). An element's words run `height` px, or what `words`
 *  gives for it. */
function layOut(
  height: number,
  words: (element: HTMLElement) => number = () => height
): () => void {
  const own = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "scrollHeight");
  Object.defineProperty(HTMLElement.prototype, "scrollHeight", {
    configurable: true,
    get(this: HTMLElement) {
      return words(this);
    },
  });
  // On Element, which owns the method, for the reason the press case above gives.
  const box = spyOn(Element.prototype, "getBoundingClientRect").mockReturnValue(
    new DOMRect(0, 0, 600, height)
  );
  return () => {
    if (own === undefined) Reflect.deleteProperty(HTMLElement.prototype, "scrollHeight");
    else Object.defineProperty(HTMLElement.prototype, "scrollHeight", own);
    box.mockRestore();
  };
}

// Ruling 1, and the reviewer's reading of it: "more · N words" opens the rest of a cut text, so
// it is drawn only while the words are cut. A row whose body is open draws its words whole, and
// no control that would open what is already open. The count is written with its thousands
// separator. The page measures only the words of rows near the viewport, so each
// row's words are brought near and measured before the row is read.
test("more is drawn on a cut row and never beside words a body draws whole", async () => {
  const text = Array.from({ length: 3000 }, (_unused, at) => `word${at + 1}`).join(" ");
  const long = node({ id: "message-1", text, type: "message" });
  const planner: Author = { kind: "agent", name: "Planner", reader: false };
  // 3,000 words over 400 lines of 24 px: six lines are drawn and 394 of 400 parts are held back.
  const restore = layOut(9600);
  const near = installIntersectionObserver();
  try {
    const cut = renderRow(long, { author: planner, body: false });
    try {
      await measureNearWords(near);
      expect(cut.container.querySelector("[data-node-more]")?.textContent).toBe(
        `${CONTROL_LABELS.read_more} · 2,955 ${CONTROL_LABELS.read_more_words}`
      );
      expect(cut.container.querySelector<HTMLElement>("[data-node-words]")?.style.maxHeight).toBe(
        "144px"
      );
    } finally {
      cut.unmount();
    }
    const open = renderRow(long, { author: planner, body: true });
    try {
      await measureNearWords(near);
      expect(
        open.container.querySelector("[data-node-more]"),
        "a row whose body is open still draws more"
      ).toBeNull();
      expect(open.container.querySelector<HTMLElement>("[data-node-words]")?.style.maxHeight).toBe(
        ""
      );
    } finally {
      open.unmount();
    }
    // Ruling 6: a statement is an agent's words as much as its comment is, and is cut the same;
    // a person's words never are.
    const statement = renderRow(node({ id: "spec#b-r1", text, type: "requirement" }), {
      body: false,
    });
    try {
      await measureNearWords(near);
      expect(statement.container.querySelector("[data-node-more]")).not.toBeNull();
    } finally {
      statement.unmount();
    }
    const person = renderRow(node({ id: "comment-1", text, type: "comment" }), {
      author: { kind: "person", name: "bob", reader: false },
      body: false,
    });
    try {
      await measureNearWords(near);
      expect(person.container.querySelector("[data-node-more]")).toBeNull();
    } finally {
      person.unmount();
    }
  } finally {
    near.restore();
    restore();
  }
});

/** 3,000 words, which run 400 lines of 24 px where they are drawn whole. */
const LONG_WORDS = Array.from({ length: 3000 }, (_unused, at) => `word${at + 1}`).join(" ");

/**
 * What a row in view draws in the commit that draws it from `next`, before the page is painted:
 * the row is drawn from `first` and measured as its coming near measures it, then, if `relaidOut`
 * is given, laid out again by it and measured again as it stands in view, and then drawn from
 * `next` with every animation frame held, so none runs. `words` is how tall the browser lays a
 * row's words out (`layOut`). `cutBefore` is whether the row was cut before `next` was drawn.
 */
async function redrawnInView({
  first,
  next,
  relaidOut,
  words,
}: {
  readonly first: TreeNode;
  readonly next: TreeNode;
  readonly relaidOut?: () => void;
  readonly words: (element: HTMLElement) => number;
}): Promise<{ cutBefore: boolean; maxHeight: string | undefined; more: string | undefined }> {
  const restore = layOut(9600, words);
  const near = installIntersectionObserver();
  try {
    const row = renderRow(first, { body: false });
    try {
      await measureNearWords(near);
      if (relaidOut !== undefined) {
        relaidOut();
        await measureNearWords(near);
      }
      const cutBefore = row.container.querySelector("[data-node-more]") !== null;
      const frames: FrameRequestCallback[] = [];
      const frame = spyOn(window, "requestAnimationFrame").mockImplementation((callback) => {
        frames.push(callback);
        return frames.length;
      });
      try {
        await act(async () => {
          row.redraw(next);
        });
        return {
          cutBefore,
          maxHeight: row.container.querySelector<HTMLElement>("[data-node-words]")?.style.maxHeight,
          more: row.container.querySelector("[data-node-more]")?.textContent ?? undefined,
        };
      } finally {
        frame.mockRestore();
      }
    } finally {
      row.unmount();
    }
  } finally {
    near.restore();
    restore();
  }
}

// Decision 263: a row in view is cut before the page paints it, also where its words arrive
// after the row. A statement is drawn from its document's blocks before the coverage read
// brings its words, so its row mounts in view with none: the mount measures it whole, and the
// batch its coming near starts holds it in view at that length. The commit that brings its words
// measures it again as one that mounts, and it is drawn cut with no animation frame run. CI run
// 44547033390 caught it otherwise at 390 px: the words were drawn whole, 472 px tall, and stayed
// whole for the 59 frames recorded after.
test("a row in view whose words arrive after it is drawn cut before the page is painted", async () => {
  const statement = { id: "spec#b-r1", type: "requirement" } as const;
  const drawn = await redrawnInView({
    first: node({ ...statement, text: "" }),
    next: node({ ...statement, text: LONG_WORDS, tree: displayTree(LONG_WORDS) }),
    // Words not drawn yet run no height; drawn, they run 400 lines of 24 px.
    words: (element) => ((element.textContent ?? "").trim() === "" ? 0 : 9600),
  });
  expect(drawn.cutBefore, "a row with no words was cut").toBe(false);
  expect(drawn.more, "the words that arrived were drawn whole in view").toBe(
    `${CONTROL_LABELS.read_more} · 2,955 ${CONTROL_LABELS.read_more_words}`
  );
  expect(drawn.maxHeight).toBe("144px");
});

// Decision 261: a row in view keeps its length unless it draws other words. A new comment on the
// issue adds an address to the names read, which then reads under a key of its own, so every
// block row drawn from that read takes a new reading of the same words (`tree-drafts.ts`'s
// `treeOf`). A row measured whole, whose words then run past the cut, as a narrower width runs
// them, is held whole in view by the batch that measures it again; a new reading of the same
// words draws nothing new, so the row stays whole, with no animation frame run.
test("a row held whole in view stays whole when a later read draws the same words anew", async () => {
  const statement = { id: "spec#b-r1", text: LONG_WORDS, type: "requirement" } as const;
  // The server reads one text into one tree (`pmdoc.DisplayJSON`), where the page's own parse
  // gives each block a new id, so the later read's answer is a copy of the first.
  const reading = displayTree(LONG_WORDS);
  // Five lines of 24 px where the row is first measured, which six lines hold whole.
  let run = 120;
  const drawn = await redrawnInView({
    first: node({ ...statement, tree: reading }),
    next: node({ ...statement, tree: structuredClone(reading) }),
    // The words now run 400 lines, and the batch holds the row in view at its length.
    relaidOut: () => {
      run = 9600;
    },
    words: () => run,
  });
  expect(drawn.cutBefore, "the batch cut a row in view").toBe(false);
  expect(drawn.more, "a new reading of the same words cut the row held whole in view").toBe(
    undefined
  );
  expect(drawn.maxHeight).toBe("");
});

// R16: an open question's options are the choices of its answer. A press on one leaves the
// focus where it is - a row that took the focus on mousedown would open its body, whose
// options' own words push every option below one that has them down, and the press would end on
// another option - and only a question that waits on the reader draws them in the waiting
// colour.
test("an option's press keeps the focus, and only a waiting question's are in the waiting colour", async () => {
  const user = userEvent.setup();
  const picked: string[] = [];
  const writer = {
    answer: undefined,
    pick: (_node: TreeNode, label: string) => picked.push(label),
  } as unknown as OutlineWriter;
  const asked = (waiting: boolean) =>
    node({
      id: "ask-1",
      options: [{ description: "Where the rail fits.", label: "1280 px" }, { label: "1024 px" }],
      reference: "dispatch://CORE-1/ask/ask-1",
      state: "open",
      text: "At which width does the identity block condense?",
      type: "ask",
      waiting,
    });
  const view = render(<NodeOptions body={false} node={asked(true)} writer={writer} />);
  try {
    const option = view.container.querySelector<HTMLElement>('[data-pick-option="1024 px"]');
    expect(option).not.toBeNull();
    if (option === null) return;
    // The press is held: a mousedown that moved the focus would have put it on the control, and
    // through it on the row.
    await user.pointer({ keys: "[MouseLeft>]", target: option });
    expect(
      view.container.contains(document.activeElement),
      "the press moved the focus into the row"
    ).toBe(false);
    await user.pointer({ keys: "[/MouseLeft]" });
    expect(picked).toEqual(["1024 px"]);
    expect(option.className).toContain("text-page-waiting-text");
  } finally {
    view.unmount();
  }
  const other = render(<NodeOptions body={false} node={asked(false)} writer={writer} />);
  try {
    const option = other.container.querySelector<HTMLElement>('[data-pick-option="1024 px"]');
    expect(option?.className).toContain("text-page-quiet");
    expect(option?.className).not.toContain("text-page-waiting-text");
  } finally {
    other.unmount();
  }
});

/** One row the way the conversation's rows draw it: closed until it takes the focus, open from
 *  then on, under the page's writer. */
function RowOpenedByFocus({ drawn }: { drawn: TreeNode }): ReactNode {
  const [focused, setFocused] = useState(false);
  return (
    <NodeRow
      author={{ kind: "agent", name: "Planner", reader: false }}
      body={focused}
      context={false}
      focused={focused}
      isClosed={false}
      level={1}
      node={drawn}
      onFocus={() => setFocused(true)}
      onPin={() => {}}
      onToggle={() => {}}
      ownAnswer={false}
      pinned={false}
      rowKey="row"
      tabStop
      tabStops
    />
  );
}

// Decision 120: a control a reader can see acts on its first press. An option drawn on a row
// that is not open yet is picked by that press, and the same press opens the row: the reader
// never presses twice.
test("a press on an option of a closed row picks it and opens the row in one", async () => {
  const user = userEvent.setup();
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  for (const key of [
    ["asks", "CORE-1"],
    ["comments", "CORE-1"],
    ["issue-messages", "CORE-1"],
    ["agents"],
  ]) {
    queryClient.setQueryData(key, []);
  }
  const asked = node({
    id: "ask-1",
    options: [{ description: "Where the rail fits.", label: "1280 px" }, { label: "1024 px" }],
    reference: "dispatch://CORE-1/ask/ask-1",
    state: "open",
    text: "At which width does the identity block condense?",
    type: "ask",
    waiting: true,
  });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <NoTooltips>
          <OutlineWriterProvider issueKey="CORE-1">
            <RowOpenedByFocus drawn={asked} />
          </OutlineWriterProvider>
        </NoTooltips>
      </MemoryRouter>
    </QueryClientProvider>
  );
  try {
    expect(view.container.querySelector("[data-node-body]")).toBeNull();
    const option = view.container.querySelector<HTMLElement>('[data-pick-option="1024 px"]');
    if (option === null) throw new Error("the closed row draws no option to press");
    await user.click(option);
    await waitFor(() => {
      expect(
        view.container.querySelector('[data-pick-option="1024 px"]')?.getAttribute("aria-pressed")
      ).toBe("true");
      expect(
        view.container.querySelector("[data-node-body]"),
        "the press picked the option and left the row closed"
      ).not.toBeNull();
    });
  } finally {
    view.unmount();
  }
});

// Ruling 5: a child issue's row is one line - its title, then how it stands and what blocks it -
// and its key is the row's tag, a link to the child's own page; it names no author and no time.
test("a child issue's row draws its title, its facts and its key as the tag", () => {
  const child: IssueChild = {
    active_at: "2026-11-05T12:00:00Z",
    external_links: null,
    key: "CORE-2",
    needs: ["CORE-3"],
    status: "backlog",
    subtree_done: 0,
    subtree_total: 0,
    title: "Draw the children in the outline",
  };
  const view = renderRow(node({ child, id: "CORE-2", text: child.title, type: "child_issue" }), {
    body: false,
  });
  try {
    expect(view.container.querySelector("[data-node-words] [data-author-text]")?.textContent).toBe(
      child.title
    );
    expect(view.container.querySelector("[data-child-facts]")?.textContent).toContain("CORE-3");
    const tag = view.container.querySelector("[data-node-margin] [data-node-tag]");
    expect(tag?.textContent).toBe("CORE-2");
    expect(tag?.querySelector("a")?.getAttribute("href")).toBe("/issues/CORE-2");
    expect(view.container.querySelector("[data-node-author]")).toBeNull();
    expect(view.container.querySelector("[data-node-time]")).toBeNull();
  } finally {
    view.unmount();
  }
});

/** The writer's acts the focused row draws for `drawn` (`[data-row-act]`), with the page's writer
 *  above it or with none, and whether it draws Copy reference (`[data-row-copy]`); `ownAnswer`
 *  says the answer an ask carries is the reader's own. The writer's reads are answered before it
 *  mounts, under the keys the tree reads them by, so it makes no request. */
function focusedControls(
  drawn: TreeNode,
  withWriter: boolean,
  ownAnswer = false
): { acts: string[]; copy: boolean } {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  for (const key of [
    ["asks", "CORE-1"],
    ["comments", "CORE-1"],
    ["issue-messages", "CORE-1"],
    ["agents"],
  ]) {
    queryClient.setQueryData(key, []);
  }
  const row = (
    <NodeRow
      author={{ kind: "person", name: "bob", reader: false }}
      body={false}
      context={false}
      focused
      isClosed={false}
      level={1}
      node={drawn}
      onFocus={() => {}}
      onPin={() => {}}
      onToggle={() => {}}
      ownAnswer={ownAnswer}
      pinned={false}
      rowKey="row"
      tabStop
    />
  );
  const view = render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <NoTooltips>
          {withWriter ? (
            <OutlineWriterProvider issueKey="CORE-1">{row}</OutlineWriterProvider>
          ) : (
            row
          )}
        </NoTooltips>
      </MemoryRouter>
    </QueryClientProvider>
  );
  try {
    return {
      acts: [...view.container.querySelectorAll("[data-row-act]")].map(
        (act) => act.getAttribute("data-row-act") ?? ""
      ),
      copy: view.container.querySelector("[data-row-copy]") !== null,
    };
  } finally {
    view.unmount();
  }
}

/** The writer's acts the focused row draws for `drawn` (`focusedControls`). */
function focusedActs(drawn: TreeNode, withWriter: boolean, ownAnswer = false): string[] {
  return focusedControls(drawn, withWriter, ownAnswer).acts;
}

// Part G's R7: the focused row draws Reply on a comment, a message and an ask in any state, and
// Answer on an open question; a statement, a check and a decision draw none, since today's model
// holds no reply to a block; and with no writer above it the row draws no act at all.
test("Reply is drawn on each node that takes one, Answer on an open question, and no action without a writer", () => {
  const by = { id: "bob", kind: "user" };
  const turn = (type: "ask" | "comment" | "message", id: string, state?: TreeNode["state"]) =>
    node({
      author: by,
      id,
      reference: `dispatch://CORE-1/${type}/${id}`,
      ...(state === undefined ? {} : { state }),
      type,
    });
  expect(focusedActs(turn("comment", "comment-1"), true)).toEqual(["reply"]);
  expect(focusedActs(turn("message", "message-1"), true)).toEqual(["reply"]);
  expect(focusedActs(turn("ask", "ask-1", "answered"), true)).toEqual(["reply"]);
  expect(focusedActs(turn("ask", "ask-2", "resolved"), true)).toEqual(["reply"]);
  expect(focusedActs(turn("ask", "ask-3", "open"), true)).toEqual(["answer", "reply"]);
  const block = (type: NodeType, id: string) =>
    node({ author: by, id, reference: `dispatch://CORE-1/spec#b-${id}`, type });
  expect(focusedActs(block("requirement", "r1"), true)).toEqual([]);
  expect(focusedActs(block("check", "c1"), true)).toEqual([]);
  expect(focusedActs(block("decision", "d1"), true)).toEqual([]);
  expect(focusedActs(turn("ask", "ask-4", "open"), false)).toEqual([]);
  expect(focusedActs(turn("comment", "comment-2"), false)).toEqual([]);
});

// Part G's R7: Change answer is drawn on an answered question whose answer is the reader's own,
// beside its Reply, and on no other answered question.
test("Change answer is drawn on an answered question the reader answered, and on no other", () => {
  const answered = node({
    author: { id: "bob", kind: "user" },
    id: "ask-1",
    reference: "dispatch://CORE-1/ask/ask-1",
    state: "answered",
    type: "ask",
  });
  expect(focusedActs(answered, true, true)).toEqual(["change-answer", "reply"]);
  expect(focusedActs(answered, true, false)).toEqual(["reply"]);
  expect(focusedActs(answered, false, true)).toEqual([]);
});

// Part G's R6: the focused row offers Copy reference on a node that has an address, with a
// writer above it or with none, since it writes nothing; a node with no address draws none.
test("Copy reference is drawn on a focused node with an address, and on none without", () => {
  const comment = (reference?: string) =>
    node({
      author: { id: "bob", kind: "user" },
      id: "comment-1",
      ...(reference === undefined ? {} : { reference }),
      type: "comment",
    });
  expect(focusedControls(comment("dispatch://CORE-1/comment/comment-1"), true).copy).toBe(true);
  expect(focusedControls(comment("dispatch://CORE-1/comment/comment-1"), false).copy).toBe(true);
  expect(focusedControls(comment(), true).copy).toBe(false);
});

// Part G's R16: while a send of the line's own is out, a press that would set the line to
// something else is refused, as a Reply is, so the line keeps the reply that send answers.
test("while the line's send is out, Answer, an option, Edit and the issue's Reply keep its reply", () => {
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  for (const key of [
    ["asks", "CORE-1"],
    ["comments", "CORE-1"],
    ["issue-messages", "CORE-1"],
    ["agents"],
  ]) {
    queryClient.setQueryData(key, []);
  }
  const reply: ReplyTarget = {
    actor: { id: "bob", kind: "user" },
    excerpt: "Earlier comment",
    id: "comment-1",
    parentKind: "comment",
  };
  const request: SentRequest = {
    anchor: undefined,
    ask: { multiple: false, options: [], urgency: "med" },
    draft: { body: "A reply on its way", mentions: [], replacement: "" },
    edit: undefined,
    kind: "comment",
    owner: { issueKey: "CORE-1", kind: "issue" },
    plainDelivery: "steer",
    replyTo: reply,
    sendKey: "send-1",
  };
  const asked = node({
    id: "ask-1",
    options: [{ label: "1024 px" }],
    reference: "dispatch://CORE-1/ask/ask-1",
    state: "open",
    type: "ask",
  });
  // The send stays out: Dispatch never answers it.
  const createComment = spyOn(api, "createComment").mockImplementation(
    () => new Promise<never>(() => {})
  );
  const seen: { writer?: OutlineWriter } = {};
  function Probe(): ReactNode {
    seen.writer = useOutlineWriter();
    return null;
  }
  try {
    act(() => {
      heldSends(queryClient).send(["issue-composer", "CORE-1"], request);
    });
    const view = render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <NoTooltips>
            <OutlineWriterProvider issueKey="CORE-1">
              <Probe />
            </OutlineWriterProvider>
          </NoTooltips>
        </MemoryRouter>
      </QueryClientProvider>
    );
    try {
      expect(seen.writer?.sendKey).toEqual(["issue-composer", "CORE-1"]);
      expect(seen.writer?.reply).toEqual(reply);
      act(() => seen.writer?.pick(asked, "1024 px"));
      act(() => seen.writer?.answerNode(asked));
      act(() => seen.writer?.edit("comment-2", "My own words"));
      act(() => seen.writer?.writeOnIssue());
      act(() => seen.writer?.clear());
      expect(seen.writer?.reply).toEqual(reply);
      expect(seen.writer?.answer).toBeUndefined();
      expect(seen.writer?.editing).toBeUndefined();
    } finally {
      view.unmount();
    }
  } finally {
    createComment.mockRestore();
  }
});

/** A focused row whose box the browser lays out at `top`, 24 px tall, placed by the hook every
 *  row of the outline places itself with, by its default step or by `below`. */
function FocusedRowAt({ below, top }: { below?: number; top: number }): ReactNode {
  const row = useRef<HTMLDivElement | null>(null);
  useRowOnScreen(row, true, below);
  return (
    <div
      ref={(element) => {
        row.current = element;
        if (element !== null) element.getBoundingClientRect = () => new DOMRect(0, top, 320, 24);
      }}
    />
  );
}

// Decision 113: a focused row stands below everything that stays at the top of the screen. At
// 760 px and below the view row stays there, and the page's `scroll-padding-top` is its height
// (`styles.css`, `slots/view-row.tsx`), while a header that publishes no band of its own leaves
// `bringOnScreen` nothing to clear: so a focused row 10 px from the top lay under the view row.
// It is scrolled to 8 px below it, and a row already below it is left where it is.
test("a focused row under the view row that stays is placed below it, and one below it is left", () => {
  document.documentElement.style.setProperty("scroll-padding-top", "44px");
  const scrollBy = spyOn(window, "scrollBy").mockImplementation(() => {});
  try {
    const under = render(<FocusedRowAt top={10} />);
    expect(
      scrollBy,
      "the focused row was left under the view row that stays"
    ).toHaveBeenCalledTimes(1);
    expect(scrollBy).toHaveBeenCalledWith({ top: 10 - 44 - 8 });
    under.unmount();
    scrollBy.mockClear();
    render(<FocusedRowAt top={60} />).unmount();
    expect(scrollBy, "a row already below the view row was scrolled").not.toHaveBeenCalled();
  } finally {
    scrollBy.mockRestore();
    document.documentElement.style.removeProperty("scroll-padding-top");
  }
});

// A view that places its rows by a step of its own passes it, and a focused row
// under what stays is placed that far below it rather than 8 px.
test("a focused row under the view row that stays is placed the step it is given below it", () => {
  document.documentElement.style.setProperty("scroll-padding-top", "44px");
  const scrollBy = spyOn(window, "scrollBy").mockImplementation(() => {});
  try {
    render(<FocusedRowAt below={20} top={10} />).unmount();
    expect(scrollBy).toHaveBeenCalledTimes(1);
    expect(scrollBy).toHaveBeenCalledWith({ top: 10 - 44 - 20 });
  } finally {
    scrollBy.mockRestore();
    document.documentElement.style.removeProperty("scroll-padding-top");
  }
});

// Decision 141: a press on Keep open or Let close leaves the focus where it is, as the row's
// other press controls do. Under part I's press deferral, Let close on a pinned row that does
// not hold the focus took it, closed the body under the pointer and dropped the focus to the page.
test("a press on Keep open or Let close leaves the focus where it is", async () => {
  const user = userEvent.setup();
  for (const pinned of [true, false]) {
    const pressed: boolean[] = [];
    const view = render(
      <>
        <button type="button">Held</button>
        <KeepOpen onPin={() => pressed.push(true)} pinned={pinned} tabStops={false} />
      </>
    );
    try {
      const held = view.getByRole("button", { name: "Held" });
      act(() => held.focus());
      const control = view.getByRole("button", {
        name: pinned ? CONTROL_LABELS.let_close : CONTROL_LABELS.keep_open,
      });
      await user.pointer({ keys: "[MouseLeft>]", target: control });
      expect(document.activeElement, `a press on ${control.textContent} took the focus`).toBe(held);
      await user.pointer({ keys: "[/MouseLeft]", target: control });
      expect(pressed, `the press on ${control.textContent} did nothing`).toEqual([true]);
      expect(document.activeElement).toBe(held);
    } finally {
      view.unmount();
    }
  }
});
