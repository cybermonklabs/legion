import { beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, type RenderResult, render, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import type { TreeNode } from "./issue-tree";
import { NodeRow } from "./NodeRow";
import type { OneLineBox } from "./one-line";
import {
  type AuthorKind,
  authorKind,
  CONTROL_LABELS,
  type GapKind,
  type NodeType,
} from "./page-words";

/**
 * What one row of the tree draws, read as a reader reads it (dispatch://INFRA-1196 R7, R8).
 *
 * The row is rendered on its own rather than through `IssueTree`, because what these rows are
 * about is the words and the shapes one node draws: the tree's fold, its keys and its placements
 * are read in `IssueTree.test.tsx` and `issue-tree.test.ts`.
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

/** Who made a node the model holds no author for, as the tree names one (`AgentText.author`). */
const NOBODY_RECORDED = { kind: authorKind(undefined), name: CONTROL_LABELS.unknown_author };

/** One row, drawn with its body open, as the focused row draws it. `oneLineBox` draws the row
 *  on one line, cut to that reading of the tree's width; `author` is who made it, as the tree
 *  names them (`AgentText.author`); `answerer` the person whose answer an ask carries. */
function renderRow(
  drawn: TreeNode,
  oneLineBox?: OneLineBox,
  author: { kind: AuthorKind; name: string } = NOBODY_RECORDED,
  answerer?: string
) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <NodeRow
          {...(answerer === undefined ? {} : { answerer })}
          author={author}
          body
          context={false}
          focused={false}
          indent={0}
          isClosed={false}
          level={1}
          node={drawn}
          oneLine={oneLineBox !== undefined}
          oneLineBox={oneLineBox}
          onFocus={() => {}}
          onPin={() => {}}
          onToggle={() => {}}
          pinned={false}
          rowKey="row"
          tabStop={false}
        />
      </MemoryRouter>
    </QueryClientProvider>
  );
}

beforeAll(async () => {
  await warmMarkdownRenderer();
});

// R9: a node whose author the model holds says who made it by a name and a shape, never by
// colour alone and never by an id, and two authors of different kinds never share a shape.
test("an author is drawn by a name and a shape of its own kind", () => {
  // A comment, which a person and an agent both write: a decision's body draws Keep and
  // Change, which are writes, and this row is about the words and the shape alone.
  const drawn = (author: { kind: AuthorKind; name: string }) => {
    const view = renderRow(node({ id: "comment-1", type: "comment" }), undefined, author);
    try {
      const shown = view.container.querySelector("[data-node-author]");
      const shape = shown?.querySelector("svg[data-glyph]");
      return {
        chrome: shown?.querySelector("[data-page-chrome]") !== null,
        name: shape?.getAttribute("aria-label"),
        role: shape?.getAttribute("role"),
        text: shown?.textContent,
        token: shape?.getAttribute("data-glyph"),
      };
    } finally {
      view.unmount();
    }
  };
  // The word is the author's name, and the shape is drawn beside it rather than in place of it,
  // exactly as the type's shape and the state's shape are. The shape carries its own accessible
  // name: a reader who cannot see it still hears what kind of author it stands for.
  expect(drawn({ kind: "person", name: "alice" })).toEqual({
    chrome: false,
    name: "Written by a person",
    role: "img",
    text: "alice",
    token: "author-person",
  });
  expect(drawn({ kind: "agent", name: "Planner" })).toEqual({
    chrome: false,
    name: "Written by an agent",
    role: "img",
    text: "Planner",
    token: "author-agent",
  });
  // Where the model holds no author the row says so in the page's own words, which are marked
  // as the page's rather than as somebody's name, with a shape neither other kind draws.
  expect(drawn(NOBODY_RECORDED)).toEqual({
    chrome: true,
    name: "Written by an author nobody recorded",
    role: "img",
    text: "Author unknown",
    token: "author-unknown",
  });
});

// R1, R9, INFRA-1150 R3: an answered ask carries the person's answer in its own row and its own
// body, and no row of its own: who answered by a name and a shape, and what they chose and wrote
// as the store holds it, character for character. The row draws the first line of it; a row
// drawn on one line has no width for it, and its body draws it.
test("an answered ask draws the answer in its own row and body, word for word", () => {
  const text = "The preset row keeps its labels above that width.\n\nBelow it, one control.";
  const asked = node({
    answer: { by: { id: "alice", kind: "user" }, selected: ["1280 px"], text },
    id: "ask-1",
    state: "answered",
    text: "At which width does the identity block condense?",
    type: "ask",
  });
  const planner = { kind: "agent" as const, name: "Planner" };
  const wraps = renderRow(asked, undefined, planner, "alice");
  try {
    const rows = wraps.container.querySelectorAll("[data-node-row]");
    expect(rows.length).toBe(1);
    const line = wraps.container.querySelector("[data-node-answer]");
    expect(line?.querySelector("[data-page-chrome]")?.textContent).toBe("Answer");
    expect(line?.querySelector("[data-answer-author]")?.textContent).toBe("alice");
    expect(line?.querySelector("[data-answer-author] svg")?.getAttribute("data-glyph")).toBe(
      "author-person"
    );
    // The label, the answerer, then the first line, cut at its first sentence as every first
    // line is, with the rest after it.
    expect(line?.textContent).toBe("AnsweraliceThe preset row keeps its labels above that width.…");
    const body = wraps.container.querySelector("[data-node-body] [data-node-answer-body]");
    expect(body?.querySelector("[data-answer-text]")?.textContent).toBe(text);
    expect(
      [...(body?.querySelectorAll("[data-answer-choice]") ?? [])].map((one) => one.textContent)
    ).toEqual(["1280 px"]);
    // The row's own author is still the asker's, and the answerer's name is no row author.
    expect(wraps.container.querySelector("[data-node-author]")?.textContent).toBe("Planner");
  } finally {
    wraps.unmount();
  }
  const oneLine = renderRow(asked, treeOf(400), planner, "alice");
  try {
    expect(oneLine.container.querySelector("[data-node-answer]")).toBeNull();
    expect(oneLine.container.querySelector("[data-answer-text]")?.textContent).toBe(text);
  } finally {
    oneLine.unmount();
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

/** A reading of the tree's width whose face draws every character at half its size, which is
 *  what makes each line below a sum a reader can check by hand: 7 px a character of the
 *  row's `text-sm`. */
function treeOf(width: number): OneLineBox {
  return { family: "test", rem: 16, width, widthOf: (text, size) => text.length * size * 8 };
}

/** The first line a one-line row draws of `drawn` in a tree `width` px wide. */
function oneLineOf(
  drawn: TreeNode,
  width: number,
  author: { kind: AuthorKind; name: string }
): string | null | undefined {
  const view = renderRow(drawn, treeOf(width), author);
  try {
    return view.container.querySelector("[data-node-first-line]")?.textContent;
  } finally {
    view.unmount();
  }
}

// R1, R9: a one-line row's first line is what its own box leaves it beside its other words,
// the author's name and shape among them. The row is 10 px less than the tree (`border-l-2`,
// `pr-2`), and a leaf's twisty, the comment's shape and word and the gaps between them take
// 101 px of it; the author's shape, a name of 20 characters and its gap take 168 px more, and
// the page's 14 characters for an author nobody recorded, 126 px.
test("a one-line row's first line is what its author leaves it", () => {
  const text = "The page draws the whole issue in one reading, with nothing lost.";
  const commented = node({ id: "comment-1", text, type: "comment" });
  const author = { kind: "agent" as const, name: "Planner of the issue" };
  // 400 px: 120 px of line beside the named author's words, or 162 px beside the page's own
  // words for nobody, each cut at the last word whose run and ellipsis fit.
  expect(oneLineOf(commented, 400, author)).toBe("The page draws…");
  expect(oneLineOf(commented, 400, NOBODY_RECORDED)).toBe("The page draws the…");
  // 300 px: 20 px with the named author's - less than "The…" takes - so that line goes under
  // them, where it has the row's whole 290 px.
  expect(oneLineOf(commented, 300, author)).toBe("The page draws the whole issue in one…");
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
test("a check whose citation names no requirement says so, in words", () => {
  const gaps: GapKind[] = ["serves_not_requirement"];
  expect(drawnGap(node({ gaps, id: "verification-md#b-c1", type: "check" }))).toEqual({
    chip: "Not a requirement",
    line: "What this says it serves is not a requirement.",
  });
});

// R8: a plan task that names no requirement it serves and no declaration it attempts is work
// no reason stands behind, and the row says that rather than leaving the reader to notice it,
// in words that pass no judgement on the work.
test("a plan task nobody asked for says so, in words", () => {
  const gaps: GapKind[] = ["nobody_asked"];
  expect(drawnGap(node({ gaps, id: "plan-md#b-t9", type: "plan_task" }))).toEqual({
    chip: "No reason given",
    line: "This task names no requirement it serves and nothing a person asked for.",
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
      indent={0}
      isClosed={false}
      level={1}
      node={drawn}
      oneLine={false}
      oneLineBox={undefined}
      onFocus={() => setFocused(true)}
      onPin={() => {}}
      onToggle={() => {}}
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
  // The row's top sits above the line the identity block leaves clear, so R9 would scroll.
  const box = spyOn(HTMLElement.prototype, "getBoundingClientRect").mockReturnValue(
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
    const control = pressed.getByRole("button", { name: "Keep open" });
    await user.pointer({ keys: "[MouseLeft>]", target: control });
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
 *  and what its body formats. */
function drawnWords(view: RenderResult) {
  const body = view.container.querySelector("[data-node-body]");
  return {
    bold: body?.querySelector("strong")?.textContent,
    code: body?.querySelector("code")?.textContent,
    firstLine: view.container.querySelector("[data-node-first-line]")?.textContent,
    item: body?.querySelector("li")?.textContent,
    symbols: /\*\*|##|`/u.test(view.container.querySelector("[data-node-row]")?.textContent ?? ""),
  };
}

/** What `drawnWords` reads of a row drawn from the reading of `MARKED`. */
const MARKED_DRAWN = {
  bold: "decided",
  code: "code",
  firstLine: "Store the notices A decided thing.…",
  item: "one code item",
  symbols: false,
};

// R20, c-plain-words: a decision's text is stored markdown, and the decisions read sends the
// server's reading of it (`Decision.text_tree`). The row draws that reading, so the reader sees
// a heading, bold words and a list, never the `##`, `**` and `-` the author typed, and the
// browser parses nothing to get there.
test("a decision whose text holds bold draws no literal asterisks", () => {
  const view = renderRow(
    node({ id: "decisions-md#b-d1", text: MARKED, tree: displayTree(MARKED), type: "decision" })
  );
  try {
    expect(drawnWords(view)).toEqual(MARKED_DRAWN);
  } finally {
    view.unmount();
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
    const body = view.container.querySelector("[data-node-body]");
    expect(body?.textContent).toContain("A **decided** thing.");
    expect(body?.querySelector("strong")).toBeNull();
  } finally {
    view.unmount();
  }
});

// R9: the tree is one tab stop and nothing inside a row takes one; a key reaches a row's links
// (`useTreeKeymap.ts`'s `rowLinks`). A body drawn from a reading holds real links, and each is
// out of the tab order too.
test("the links a body draws are out of the tab order", async () => {
  const text =
    "Read [the notes](https://example.com/notes) and [the plan](https://example.com/plan).";
  const view = renderRow(node({ id: "comment-1", text, tree: displayTree(text), type: "comment" }));
  try {
    await waitFor(() => {
      const links = [...view.container.querySelectorAll<HTMLElement>("[data-node-body] a[href]")];
      expect(links.length).toBe(2);
      expect(links.map((link) => link.tabIndex)).toEqual([-1, -1]);
    });
  } finally {
    view.unmount();
  }
});
