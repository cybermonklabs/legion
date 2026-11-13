import { expect, spyOn, test } from "bun:test";
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
 *  fence and all. `cut` leaves the closing fence off, which is how a summary longer than the
 *  server's excerpt arrives: cut at `BlockExcerptRunes` wherever that falls. */
function summaryNode(text: string, cut = false): TreeNode {
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
    text: `:::callout{#summary title="What this issue is"}\n${text}${cut ? "" : "\n:::"}`,
    transcluded: false,
    type: "callout",
    waiting: false,
  };
}

/** The bullets the head draws, in order. `reading` is whether the reads that carry the summary
 *  are still out. */
function bullets(
  summary: TreeNode | undefined,
  reading = false
): { lines: string[]; unmount: () => void } {
  const view = render(
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
      <IssueThreeThings
        checks={[]}
        issueKey="CORE-1"
        plan={undefined}
        planTasks={0}
        reading={reading}
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

// The node's text is the server's excerpt, cut at 480 runes, so a longer summary arrives ending
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

// A summary cut inside its first sentence holds words and no whole sentence of them. That is a
// spec with a summary, so the head says why it draws none rather than that there is none; and a
// whole summary whose words end in no sentence end says that instead, since it is not too long.
test("a summary with no whole sentence says why it draws none, not that there is none", () => {
  for (const [summary, line] of [
    [
      summaryNode("The reasoning behind each requirement is reached from the node it", true),
      EMPTY_STATE_LINES.spec_summary_cut,
    ],
    [summaryNode("The page is one outline"), EMPTY_STATE_LINES.spec_summary_unended],
  ] as const) {
    const { lines, unmount } = bullets(summary);
    try {
      expect(lines).toEqual([]);
      const head = screen.getByTestId("issue-three-things").textContent;
      expect(head).not.toContain("This spec has no summary.");
      expect(head).toContain(line);
    } finally {
      unmount();
    }
  }
  expect(EMPTY_STATE_LINES.spec_summary_cut).toBe("The summary is too long to show here.");
});

// A summary may say one sentence twice. Each is a bullet of its own, and React is told the two
// apart by their place: keyed by its words alone, the two bullets shared one key.
test("a summary that says one sentence twice draws both, each under a key of its own", () => {
  const errors = spyOn(console, "error").mockImplementation(() => {});
  try {
    const { lines, unmount } = bullets(
      summaryNode("The page is one outline. The page is one outline. It fits a phone.")
    );
    try {
      expect(lines).toEqual([
        "The page is one outline.",
        "The page is one outline.",
        "It fits a phone.",
      ]);
      expect(errors.mock.calls.filter((call) => String(call[0]).includes("same key"))).toEqual([]);
    } finally {
      unmount();
    }
  } finally {
    errors.mockRestore();
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

// The page's first load draws the tree before the spec's blocks and their words arrive, so the
// summary node is missing, or there with no words yet. That is no spec without a summary, and
// the head says nothing about one until the reads that carry it have answered.
test("while the reads that carry the summary are out, the head says nothing about it", () => {
  for (const summary of [undefined, { ...summaryNode(""), text: "" }]) {
    const { lines, unmount } = bullets(summary, true);
    try {
      expect(lines).toEqual([]);
      expect(screen.getByTestId("issue-three-things").textContent).not.toContain(
        EMPTY_STATE_LINES.spec_summary
      );
    } finally {
      unmount();
    }
  }
});

// A summary written as bullets is read as the sentences its items hold, each item one bullet
// of the head whether or not its author ended it with a full stop: the item's own end is where
// it stops, and the marker in front of it is the list's syntax, not the author's words.
test("a summary written as bullets draws each item as a sentence", () => {
  const { lines, unmount } = bullets(
    summaryNode(
      "- What: the page is one outline\n- Why: a reader finds the work\n- Done: it fits a phone"
    )
  );
  try {
    expect(lines).toEqual([
      "What: the page is one outline",
      "Why: a reader finds the work",
      "Done: it fits a phone",
    ]);
  } finally {
    unmount();
  }
});

// A sentence in bold still ends at its full stop: the asterisks after it are the emphasis, and
// they are neither the sentence's end nor words of it.
test("a summary in bold, or ending in a bold sentence, draws that sentence", () => {
  const whole = bullets(summaryNode("**The page is one outline.**"));
  try {
    expect(whole.lines).toEqual(["The page is one outline."]);
  } finally {
    whole.unmount();
  }
  const last = bullets(
    summaryNode("The page is one outline. Nothing is lost. **It fits a phone.**")
  );
  try {
    expect(last.lines).toEqual([
      "The page is one outline.",
      "Nothing is lost.",
      "It fits a phone.",
    ]);
  } finally {
    last.unmount();
  }
});

// The excerpt is cut wherever `BlockExcerptRunes` falls, which can be inside the last item of
// a list as well as inside a sentence; the words after the cut are no item anybody wrote.
test("a bulleted summary cut inside its last item draws the whole items and not the fragment", () => {
  const { lines, unmount } = bullets(
    summaryNode(
      "- What: the page is one outline\n- Why: a reader finds the work\n- Done: it fi",
      true
    )
  );
  try {
    expect(lines).toEqual(["What: the page is one outline", "Why: a reader finds the work"]);
  } finally {
    unmount();
  }
});
