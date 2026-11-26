import { beforeAll, expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { warmMarkdownRenderer } from "../refs/markdown-engine";
import {
  NOTHING_PUBLISHED,
  PublishedReferenceNames,
  ReferenceNamesProvider,
} from "../refs/reference-names";
import type { TreeNode } from "./issue-tree";
import { NodeRow } from "./NodeRow";
import { NoTooltips } from "./NoTooltips";
import { CONTROL_LABELS } from "./page-words";
import {
  accessibleNameOf,
  rowControls,
  tabStopsOf,
  withMarginAfterWords,
} from "./row-name-fixtures";

/**
 * What a row of the outline is named by and where the keyboard stops on it. The row holds its
 * margin - the kind's tag, the author, the time - ahead of its words in the DOM, so that on a
 * phone the margin floats beside the words' first line (`NodeRow.tsx`'s `NODE_ROW_LAYOUT`), and it
 * is read in its reading order all the same: its words first. happy-dom lays nothing out, so
 * these read the DOM and the names it gives; where the margin stands on the page is the browser
 * suite's to read (c-row).
 */

/** An agent's decision of the spec, which its row names by a word and a link to it in Decisions
 *  (`tagFilter`), with who made it and when. */
const DECISION: TreeNode = {
  alsoUnder: [],
  at: "2026-11-05T13:00:00Z",
  author: { id: "019a1c0a-0000-7000-8000-000000000001", kind: "agent" },
  children: [],
  chips: [],
  facts: [],
  gaps: [],
  held: 0,
  id: "spec#b-d1",
  question: false,
  reference: "dispatch://CORE-1/spec#b-d1",
  replaced: false,
  text: "Keep the rail at the right of the page.",
  transcluded: false,
  type: "decision",
  waiting: false,
};

/** The decision's row as the outline draws it: unfolded, holding nodes under it, and focused or
 *  not, with the tree's one tab stop where it is focused. */
function renderDecision(focused: boolean) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <ReferenceNamesProvider>
          <PublishedReferenceNames published={NOTHING_PUBLISHED}>
            <NoTooltips>
              <NodeRow
                author={{ kind: "agent", name: "Planner", reader: false }}
                context={false}
                expanded
                focused={focused}
                isClosed={false}
                level={1}
                node={DECISION}
                onFocus={() => {}}
                onToggle={() => {}}
                ownAnswer={false}
                rowKey="CORE-1>spec#b-d1"
                tabStop={focused}
              />
            </NoTooltips>
          </PublishedReferenceNames>
        </ReferenceNamesProvider>
      </MemoryRouter>
    </QueryClientProvider>
  );
}

beforeAll(async () => {
  await warmMarkdownRenderer();
});

// A row is named by its words and what it draws under them, then its margin - its kind, its
// author and its time - though the margin stands first in the document: the row names itself by
// those two parts in that order (`aria-labelledby`), and the name is word for word the one its
// text gives a copy of the row with its margin after its words. Named by its text in the
// document's order, it would read the kind first.
test("a row's accessible name reads its words before its kind, author and time", () => {
  for (const focused of [false, true]) {
    const view = renderDecision(focused);
    const row = view.getByRole("treeitem");
    const ordered = withMarginAfterWords(row);
    try {
      const where = focused ? "focused" : "unfocused";
      const name = accessibleNameOf(row);
      expect(name.startsWith(DECISION.text), `${where}: ${name}`).toBe(true);
      const words = name.indexOf(DECISION.text);
      expect(name.indexOf("Planner", words), `${where}: ${name}`).toBeGreaterThan(words);
      expect(name.lastIndexOf("5 Nov"), `${where}: ${name}`).toBeGreaterThan(words);
      expect(name, where).toBe(accessibleNameOf(ordered));
    } finally {
      ordered.remove();
      view.unmount();
    }
  }
});

// A row's controls, in the order it reads in - its fold control, the controls under its words,
// then its margin's - are, focused or not, its fold control, Copy reference, which the row draws
// whatever the focus, and its kind's link; and none is a tab stop: the tree is one tab stop, so an
// unfocused row holds none and the focused row is its own. This catches a control that gains a
// tab stop, one lost or added, and one drawn in another part of the row. Every part of the
// content but the words - the face, the acts - clears the margin's float on a phone, so none of
// them is squeezed beside it (`NODE_ROW_LAYOUT.content`), and the browser suite reads where each
// stands.
test("a row's controls stand in its reading order and none of them is a tab stop", () => {
  const drawn = (focused: boolean) => {
    const view = renderDecision(focused);
    try {
      const row = view.getByRole("treeitem");
      expect(
        row
          .querySelector("[data-node-content]")
          ?.classList.contains(
            "[&>:not([data-node-words]):not(:has(>[data-node-first-line]))]:clear-right"
          )
      ).toBe(true);
      return { controls: rowControls(row), stops: tabStopsOf(row) };
    } finally {
      view.unmount();
    }
  };
  const controls = ["fold", CONTROL_LABELS.copy_reference, "the kind's link"];
  expect(drawn(false)).toEqual({ controls, stops: [] });
  expect(drawn(true)).toEqual({ controls, stops: [true] });
});
