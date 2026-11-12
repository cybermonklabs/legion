import { expect, test } from "bun:test";
import { render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

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

/** One row, drawn with its body open, as the focused row draws it. */
function renderRow(drawn: TreeNode) {
  return render(
    <MemoryRouter>
      <NodeRow
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
  );
}

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
