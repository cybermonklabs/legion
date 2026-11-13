import { beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { type RenderResult, render, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { api } from "../../api/client";
import type { Artifact } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import type { TreeNode } from "./issue-tree";
import { NodeRow, WIDE_ONE_LINE_MEASURE } from "./NodeRow";
import type { GapKind, NodeType } from "./page-words";

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

/** One row, drawn with its body open, as the focused row draws it. `artifact` is the document
 *  or image the node is, whose body draws the artifact itself. */
function renderRow(drawn: TreeNode, artifact?: Artifact) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <NodeRow
          artifact={artifact}
          body
          context={false}
          focused={false}
          indent={0}
          isClosed={false}
          level={1}
          node={drawn}
          oneLine={false}
          oneLineMeasure={WIDE_ONE_LINE_MEASURE}
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

// R7: a node whose author the model holds says who made it by a word and a shape, never by
// colour alone. One shape for an author, whoever they are: the node carries an author's id and
// nothing else, so a shape per kind of author would assert a kind the model does not hold.
test("the author is drawn by a word and a shape", () => {
  // A comment, which is one of the five kinds R7 names: a decision's body draws Keep and
  // Change, which are writes, and this row is about the words and the shape alone.
  const view = renderRow(
    node({ author: { id: "planner", kind: "session" }, id: "comment-1", type: "comment" })
  );
  try {
    const author = view.container.querySelector("[data-node-author]");
    expect(author).not.toBeNull();
    // The word is the author's own id, and the shape is drawn beside it rather than in place
    // of it, exactly as the type's shape and the state's shape are.
    expect(author?.textContent).toBe("planner");
    const shape = author?.querySelector("svg[data-glyph]");
    expect(shape?.getAttribute("data-glyph")).toBe("author");
    // The shape carries its own accessible name: a reader who cannot see it still hears it.
    expect(shape?.getAttribute("aria-label")).toBe("Written by");
    expect(shape?.getAttribute("role")).toBe("img");
  } finally {
    view.unmount();
  }
});

test("a node with no author says nothing about one, and draws no shape for it", () => {
  const view = renderRow(node({ id: "spec#b-r1", type: "requirement" }));
  try {
    expect(view.container.querySelector("[data-node-author]")).toBeNull();
    expect(view.container.querySelector('svg[data-glyph="author"]')).toBeNull();
  } finally {
    view.unmount();
  }
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
    line: "This says it serves a node that is not a requirement.",
  });
});

// R8: a plan task that names no requirement it serves and no declaration it attempts is work
// nobody asked for, and the row says that rather than leaving the reader to notice it.
test("a plan task nobody asked for says so, in words", () => {
  const gaps: GapKind[] = ["nobody_asked"];
  expect(drawnGap(node({ gaps, id: "plan-md#b-t9", type: "plan_task" }))).toEqual({
    chip: "Nobody asked for this",
    line: "This task names no requirement it serves and no declaration it attempts.",
  });
});

// R8: a result recording no negative control is drawn beside that result for the reader to
// weigh. The page states no verdict of its own on it: the line says what is absent, no more.
test("a result that records no negative control says so, in words", () => {
  const gaps: GapKind[] = ["no_negative_control"];
  expect(drawnGap(node({ gaps, id: "result-1", type: "result" }))).toEqual({
    chip: "No negative control",
    line: "This result records no negative control.",
  });
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

// R16, R20: a document's node opens the document itself, read when the body opens; that read
// sends the server's reading of the text (`ArtifactText.markdown_tree`), which the body draws.
test("a document's body draws the server's reading of its text", async () => {
  const read = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: MARKED,
    markdown_tree: displayTree(MARKED),
    token: "sha256:marked",
    version: 1,
  });
  const document: Artifact = {
    created_at: "2026-11-05T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    id: "artifact-notes",
    issue_key: "CORE-1",
    kind: "doc",
    name: "notes.md",
    primary: false,
    project: "CORE",
    ref_key: "artifact:artifact-notes",
    session_id: null,
    slug: "notes-md",
    versions: [],
  };
  const view = renderRow(node({ id: "notes-md", text: "notes.md", type: "document" }), document);
  try {
    await waitFor(() => expect(drawnWords(view).bold).toBe("decided"));
    const body = view.container.querySelector("[data-node-body]")?.textContent ?? "";
    expect(body).not.toContain("**");
    expect(body).not.toContain("##");
  } finally {
    view.unmount();
    read.mockRestore();
  }
});
