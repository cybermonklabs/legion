import { expect, test } from "bun:test";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { IssueThreeThings } from "./IssueThreeThings";
import type { TreeNode } from "./issue-tree";
import { EMPTY_STATE_LINES } from "./page-words";

/**
 * The head of the page draws the issue in plain language: at most three bullets, each one spoken
 * sentence of the spec's summary (dispatch://INFRA-1196 R5), out of the summary node's own text,
 * which the tree the page already holds carries - the head reads no document of its own (R20).
 *
 * Every row below renders the head with no `QueryClientProvider` around it, which is the whole
 * proof of R20 here: a component that read anything would throw for want of a client, so these
 * rows pass only while the head makes no read at all.
 */

/** The spec's summary node, as the tree holds it: a `callout` whose text is the block's source,
 *  fence and all. */
function summaryNode(text: string): TreeNode {
  return {
    alsoUnder: [],
    children: [],
    chips: [],
    facts: [],
    gaps: [],
    held: 0,
    id: "spec#b-summary",
    question: false,
    replaced: false,
    text: `:::callout{#summary title="What this issue is"}\n${text}\n:::`,
    transcluded: false,
    type: "callout",
    waiting: false,
  };
}

/** The bullets the head draws, in order. */
function bullets(summary: TreeNode | undefined): { lines: string[]; unmount: () => void } {
  const view = render(
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
      <IssueThreeThings
        checks={[]}
        issueKey="CORE-1"
        plan={undefined}
        planTasks={0}
        summary={summary}
      />
    </MemoryRouter>
  );
  const head = within(screen.getByTestId("issue-three-things"));
  return {
    lines: head.queryAllByRole("listitem").map((item) => item.textContent ?? ""),
    unmount: view.unmount,
  };
}

test("a summary of four sentences draws the first three of them", () => {
  const { lines, unmount } = bullets(
    summaryNode(
      "The issue page draws one ordered body. Every property is visible at every width. " +
        "The reasoning behind each requirement is one press away. Nothing the page drew is lost."
    )
  );
  try {
    expect(lines).toEqual([
      "The issue page draws one ordered body.",
      "Every property is visible at every width.",
      "The reasoning behind each requirement is one press away.",
    ]);
  } finally {
    unmount();
  }
});

// An author who wrote two sentences gets two: no third bullet, and nothing drawn in its place.
test("a summary of two sentences draws two bullets and says nothing about the third", () => {
  const { lines, unmount } = bullets(
    summaryNode("The page is one outline. It opens folded to the work in front of the reader.")
  );
  try {
    expect(lines).toEqual([
      "The page is one outline.",
      "It opens folded to the work in front of the reader.",
    ]);
    expect(screen.getByTestId("issue-three-things").textContent).not.toContain(
      EMPTY_STATE_LINES.spec_summary
    );
  } finally {
    unmount();
  }
});

// The node's text is the server's excerpt, cut at 240 runes, so a longer summary arrives ending
// mid-sentence. That tail is words nobody spoke as a sentence, so it is not drawn.
test("a summary cut mid-sentence draws its whole sentences and not the fragment", () => {
  const { lines, unmount } = bullets(
    summaryNode("The page is one outline. The reasoning behind each requirement is reach")
  );
  try {
    expect(lines).toEqual(["The page is one outline."]);
  } finally {
    unmount();
  }
});

// A spec that carries no summary node at all still draws its row in that same place, saying so.
test("a spec with no summary draws the page's own line and no bullet", () => {
  const { lines, unmount } = bullets(undefined);
  try {
    expect(lines).toEqual([]);
    expect(screen.getByTestId("issue-three-things").textContent).toContain(
      EMPTY_STATE_LINES.spec_summary
    );
  } finally {
    unmount();
  }
});
