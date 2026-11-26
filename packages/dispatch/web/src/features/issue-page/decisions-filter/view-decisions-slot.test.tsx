import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, type RenderResult, render, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import type { NavigateFunction } from "react-router-dom";

import { ApiError, api } from "../../../api/client";
import {
  issueAskThreadsQuery,
  issueCoverageQuery,
  issueResultsQuery,
  myRepliesOwnerQuery,
  referenceTargetsQuery,
  whoAmIQuery,
} from "../../../api/queries";
import type { Comment, IssueAskRow } from "../../../api/types";
import { type EventFreshness, useIsHuman } from "../../../hooks/useIsHuman";
import { answeredAsk } from "../../inbox/ask-card-fixture";
import { EVENT_FRESH } from "../../refs/reference-target";
import { buildDispatchReference } from "../../refs/routes";
import { useMarkThreadRead } from "../../threads/useMarkThreadRead";
import { rowActionSelector } from "../DecisionActions";
import { presetPath } from "../issue-address";
import { framesByHand, TWO_THREADS, waitInAct } from "../outline-fixtures";
import { type ThreadMark, ThreadReadMark } from "../outline-thread-read";
import { EMPTY_STATE_LINES, STATE_LINES } from "../page-words";
import { FAILED_READ_RETRY } from "../slots/failed-read-line";
import {
  answerTree,
  commentGone,
  commentNodes,
  drawAtBrowserEntry,
  drawnSlot,
  drawStage,
  focusedRow,
  GONE_COMMENT,
  goneLineTakesFocus,
  inDocument,
  REFUSED,
  retryTakesFocus,
  rowTakesFocus,
  stubReads,
  treeOf,
} from "../slots/stage-fixtures";
import { blockNodeId } from "../tree-drafts";
import { treeNode } from "../tree-read/tree-fixtures";
import { outlineQuery, type TreeAnswer, type TreeAnswerNode } from "../tree-read/tree-read";
import {
  decision,
  decisionNode,
  issueDetails,
  KEY,
  READER,
  SPEC_DOCUMENT,
  shaped,
  specRef,
} from "./decisions-fixtures";

let stubs: { mockRestore(): void }[] = [];
beforeEach(() => {
  stubs = stubReads();
});
afterEach(() => {
  cleanup();
  for (const stub of stubs) stub.mockRestore();
});

/** The line the body draws once the reads that say which entries there are have answered. */
async function emptyLineOf(view: RenderResult): Promise<string | null | undefined> {
  await waitFor(() =>
    expect(view.container.querySelector("[data-decisions-body] p")).not.toBeNull()
  );
  return view.container.querySelector("[data-decisions-body] p")?.textContent;
}

// The line the Decisions view's slot hands its body for an issue that holds no question and no
// decision it draws: the empty issue's own line where no words are typed, and the line that says
// nothing matched where they are. Words of spaces alone filter nothing (the view trims them), so
// they draw the empty issue's line too.
test("no words draw the empty issue's line, and words that match nothing the no-match line", async () => {
  expect(await emptyLineOf(drawStage(""))).toBe(EMPTY_STATE_LINES.decisions);
  cleanup();
  expect(await emptyLineOf(drawStage("?q=zzz"))).toBe(EMPTY_STATE_LINES.no_match);
});

test("words of spaces alone draw the empty issue's line, as no words do", async () => {
  expect(await emptyLineOf(drawStage("?q=%20%20"))).toBe(EMPTY_STATE_LINES.decisions);
});

// An address beside the Decisions view names a node of the outline. Where the view's tree holds
// none, the view lands on nothing, and the stage draws the outline's fallback in its place, as it
// did before the view was filled: for an id no comment holds, the comment's own read finds none,
// and the page says so where the outline would be, with the focus on that line.
test("a comment the issue holds none of, named beside the Decisions view, draws the outline's line that says so, with the focus on it", async () => {
  const read = commentGone();
  try {
    const view = drawStage(`?comment=${GONE_COMMENT}`);
    await goneLineTakesFocus(view, "the address of a comment the issue holds none of");
    expect(read).toHaveBeenCalledWith(GONE_COMMENT);
    expect(
      view.container.querySelector("[data-decisions-body]"),
      "the view was drawn beside the line"
    ).toBeNull();
  } finally {
    read.mockRestore();
  }
});

// A block the tree holds no node of, named by a `#b-` fragment on the Decisions view's address:
// the stage draws the outline's line that the outline has no row for it, with the focus on it.
test("a block no node carries, named on the Decisions view's address, draws the outline's line that says so, with the focus on it", async () => {
  const view = drawStage("#b-no-such-block");
  const heading = await waitFor(
    () => view.getByRole("heading", { name: "No row for this block" }),
    {
      onTimeout: () =>
        new Error(
          `the address of a block no node carries drew no line saying so; the stage drew ${drawnSlot(view)}, with the focus on ${document.activeElement?.tagName}`
        ),
    }
  );
  await waitFor(() =>
    expect(document.activeElement, "the line that names no block does not hold the focus").toBe(
      heading
    )
  );
  expect(heading.parentElement?.textContent).toContain(
    `That link names no row of ${KEY}'s outline.`
  );
  expect(
    view.container.querySelector("[data-decisions-body]"),
    "the view was drawn beside the line"
  ).toBeNull();
});

// A comment the tree holds, named beside the Decisions view, is the view's to land on: it draws the
// comment as a row before its parts, with the focus on it, and the stage keeps the view once every
// read is held.
test("a comment the tree holds, named beside the Decisions view, keeps the view, with the focus on the comment's row", async () => {
  const comments = spyOn(api, "listComments").mockResolvedValue([...TWO_THREADS]);
  answerTree(commentNodes(TWO_THREADS));
  try {
    const view = drawStage("?comment=reply-b");
    await waitFor(() =>
      expect(focusedRow(), `the stage drew ${drawnSlot(view)}, with the focus on no row`).toBe(
        "reply-b"
      )
    );
    // Every read has answered, and the view has said whether its tree draws the comment.
    await waitInAct(20);
    expect(drawnSlot(view), "the stage drew the outline in place of the view").toBe("the view");
    expect(focusedRow(), "the focus left the comment's row").toBe("reply-b");
  } finally {
    comments.mockRestore();
  }
});

/** The issue's asks as the asks read answers them, with no reply under either: a question still
 *  open, and one the reader answered. */
const ASKED: readonly IssueAskRow[] = [
  answeredAsk({ id: "ask-open", answer: null, question: "Which flag?", state: "open" }),
  answeredAsk({ id: "ask-answered" }),
].map((asked) => ({ ...asked, last_reply: null }));

/** Statement `s1` of the spec as Conversation's read answers it, which the decisions view read
 *  leaves to that read. */
const S1 = treeNode("s1", "requirement", KEY, inDocument(SPEC_DOCUMENT, 0));

/** The decision block `block` of the spec, waiting for review and tied to `S1`. */
function tied(block: string): TreeAnswerNode {
  return decision(block, { decision: { links: [shaped(specRef("s1"))] } });
}

/** The block of the spec that holds the reader's write-up of their answer to `ask-answered`: a
 *  `reading` block. */
const WRITE_UP_BLOCK = "writeup";

/** The write-up's node, which the view draws no row for once the asks read tells it. */
const WRITE_UP_NODE = blockNodeId(SPEC_DOCUMENT.slug, WRITE_UP_BLOCK);

/** The tree read for the write-up cases: each asked question under the issue, `before` ahead of
 *  them, and the write-up, a `reading` block of the spec, under the question the reader
 *  answered. */
function answerWriteUpTree(before: readonly TreeAnswerNode[] = []): void {
  answerTree([
    ...before,
    ...ASKED.map((asked) =>
      treeNode(asked.id, "ask", KEY, { made_at: asked.created_at, text: asked.question })
    ),
    treeNode(WRITE_UP_BLOCK, "reading", "ask-answered", {
      ...inDocument(SPEC_DOCUMENT, 1),
      text: "The reader keeps it as it is.",
    }),
  ]);
}

// A reading of a question of this issue the reader answered is a write-up of that answer, which
// the view draws no row for: the question's own row is the decision. An address naming the
// write-up keeps the view and lands on the row of the question it answers, though a question
// still open stands first in the list and the stage would give its row the focus.
test("a person's write-up of an answered question, named on the Decisions view's address, keeps the view, with the focus on the question's row", async () => {
  const asks = spyOn(api, "listIssueAskThreads").mockResolvedValue([...ASKED] as never);
  answerWriteUpTree();
  // The messages read, which answers last: with it every read is held, and the view has told the
  // stage whether its tree draws the write-up.
  const messagesOut = Promise.withResolvers<[]>();
  spyOn(api, "listIssueMessages").mockReturnValue(messagesOut.promise);
  try {
    const view = drawStage("#b-writeup");
    await waitFor(() =>
      expect(
        focusedRow(),
        `the write-up's address left the focus elsewhere; the stage drew ${drawnSlot(view)}`
      ).toBe("ask-answered")
    );
    await act(async () => messagesOut.resolve([]));
    expect(drawnSlot(view), "the stage drew the outline in place of the view").toBe("the view");
    expect(focusedRow(), "the focus left the question's row").toBe("ask-answered");
    expect(
      view.container.querySelector(`[data-node-id="${WRITE_UP_NODE}"]`),
      "the view drew the write-up as a row"
    ).toBeNull();
  } finally {
    asks.mockRestore();
  }
});

// R6, and INFRA-1196's architect on R2: the Decisions view is drawn over the tree the decisions
// view read builds (`useDecisionsTree`), so the node an address beside it names is found among
// the nodes of that read. Conversation's read holds the first decision alone here, as a first
// page that ends before the second, and the decisions view read holds both: `#b-later` keeps the
// view, with the focus on the later decision's row.
test("a decision only the decisions view read holds, named on the Decisions view's address, keeps the view, with the focus on its row", async () => {
  spyOn(api, "getIssueTree").mockImplementation(async (_key, query) =>
    query?.view === "decisions"
      ? treeOf([tied("first"), tied("later")])
      : treeOf([S1, tied("first")])
  );
  // The messages read is the one held: it answers last, and with it every read the tree is built
  // from is held, once the decisions view read has answered.
  const messagesOut = Promise.withResolvers<[]>();
  spyOn(api, "listIssueMessages").mockReturnValue(messagesOut.promise);
  const later = decisionNode("later").id;
  const view = drawStage("#b-later");
  await waitFor(() =>
    expect(
      focusedRow(),
      `the later decision's address left the focus elsewhere; the stage drew ${drawnSlot(view)}`
    ).toBe(later)
  );
  await act(async () => messagesOut.resolve([]));
  await waitInAct(20);
  expect(drawnSlot(view), "the stage drew the outline in place of the view").toBe("the view");
  expect(focusedRow(), "the focus left the later decision's row").toBe(later);
});

// The same address with the reads in the opposite order: the outline's reads all answer, and
// the view is drawn over Conversation's first page, which holds the first decision alone, while
// the decisions view read is still out. The view says nothing of the later decision until that
// read answers (`decisions-entries.ts`' `viewSettled`): once it answers with the later decision,
// `#b-later` lands on its row in the Decisions view, and the stage never drew the outline in the
// view's place.
test("a decision only the decisions view read holds, its read answering after the outline's reads, keeps the view, with the focus on its row", async () => {
  const viewOut = Promise.withResolvers<TreeAnswer>();
  spyOn(api, "getIssueTree").mockImplementation((_key, query) =>
    query?.view === "decisions" ? viewOut.promise : Promise.resolve(treeOf([S1, tied("first")]))
  );
  const first = decisionNode("first").id;
  const later = decisionNode("later").id;
  const view = drawStage("#b-later");
  await waitFor(() =>
    expect(
      view.container.querySelector(
        `[data-decisions-body] [data-node-row][data-node-id="${first}"]`
      ),
      `the view drew no row of the first decision over the outline's reads; the stage drew ${drawnSlot(view)}`
    ).not.toBeNull()
  );
  // Every read the outline is built from has answered; the decisions view read is still out.
  await waitInAct(20);
  expect(
    drawnSlot(view),
    "the stage drew the outline in place of the view before the decisions view read answered"
  ).toBe("the view");
  await act(async () => viewOut.resolve(treeOf([tied("first"), tied("later")])));
  await waitFor(() =>
    expect(
      focusedRow(),
      `the later decision's address left the focus elsewhere; the stage drew ${drawnSlot(view)}`
    ).toBe(later)
  );
  await waitInAct(20);
  expect(drawnSlot(view), "the stage drew the outline in place of the view").toBe("the view");
  expect(focusedRow(), "the focus left the later decision's row").toBe(later);
});

// R4: a refused Keep says so on the row. A Keep, however it ends, reads the tree again and drops
// the views the page holds (`DecisionActions.tsx`), so the decisions view read is read again;
// until its next answer lands the view keeps drawing its last one (`useDecisionsTree`,
// `useDecisionReads`), as the Spec view's column does. A decision only that read holds, past
// Conversation's first page, keeps its row while the read is out again, the row keeps the focus,
// and the row keeps saying the Keep was refused (`409 DECISION_CHANGED`).
test("a refused Keep of a decision only the decisions view read holds keeps its row, its refusal and the focus while that read is read again", async () => {
  const refusal = "The decision changed since you read it.";
  const keep = spyOn(api, "keepDecision").mockRejectedValue(
    new ApiError(409, { code: "DECISION_CHANGED", error: refusal })
  );
  // The decisions view read answers once, and the read the refused Keep makes of it again is
  // held.
  const again = Promise.withResolvers<TreeAnswer>();
  let viewReads = 0;
  spyOn(api, "getIssueTree").mockImplementation((_key, query) => {
    if (query?.view !== "decisions") return Promise.resolve(treeOf([S1, tied("first")]));
    viewReads += 1;
    return viewReads === 1
      ? Promise.resolve(treeOf([tied("first"), tied("later")]))
      : again.promise;
  });
  const later = decisionNode("later").id;
  const row = () =>
    document.querySelector<HTMLElement>(
      `[data-decisions-body] [data-node-row][data-node-id="${later}"]`
    );
  const user = userEvent.setup();
  try {
    const view = drawStage("#b-later");
    await waitFor(() =>
      expect(
        focusedRow(),
        `the later decision's address left the focus elsewhere; the stage drew ${drawnSlot(view)}`
      ).toBe(later)
    );
    const press = await waitFor(() => {
      const button = row()?.querySelector<HTMLElement>(rowActionSelector("keep"));
      if (button == null) throw new Error("the later decision's row draws no Keep");
      return button;
    });
    await user.click(press);
    await waitFor(() => expect(keep, "the Keep was never sent").toHaveBeenCalledTimes(1));
    await waitFor(() =>
      expect(viewReads, "the refused Keep did not read the decisions view again").toBeGreaterThan(1)
    );
    await waitInAct(20);
    expect(
      row(),
      "the refused decision's row left the view while the decisions view read is read again"
    ).not.toBeNull();
    expect(
      row()?.querySelector('[role="alert"]')?.textContent,
      "the refused decision's row no longer says the Keep was refused"
    ).toBe(refusal);
    expect(focusedRow(), "the focus left the refused decision's row").toBe(later);
    expect(drawnSlot(view), "the stage drew the outline in place of the view").toBe("the view");
  } finally {
    keep.mockRestore();
  }
});

// Root R20, INFRA-1196's decision 513: a decision whose `from_ask` names a question the reader
// answered restates the answer, and the tree holds no node of it, so an address naming its block
// on the Decisions view's address keeps the view and lands on the row of the question it
// restates, by the rule the outline lands it by too (`pathToTarget`). The read answers a person's
// decision on the question, as it does on every ask a person answered.
test("a decision that restates a person's answer, named on the Decisions view's address, keeps the view, with the focus on the question's row", async () => {
  const asks = spyOn(api, "listIssueAskThreads").mockResolvedValue([...ASKED] as never);
  answerTree([
    ...ASKED.map((asked) =>
      treeNode(asked.id, "ask", KEY, {
        made_at: asked.created_at,
        text: asked.question,
        ...(asked.state === "answered" ? { decision: {} } : {}),
      })
    ),
    treeNode("restates", "decision", "ask-answered", {
      ...inDocument(SPEC_DOCUMENT, 1),
      decision: { asks: ["ask-answered"], waiting_review: true },
      text: "Keep the answer as it was given.",
    }),
  ]);
  // The messages read, which answers last: with it every read is held, and the view has told the
  // stage whether its tree draws the node the address names.
  const messagesOut = Promise.withResolvers<[]>();
  spyOn(api, "listIssueMessages").mockReturnValue(messagesOut.promise);
  try {
    const view = drawStage("#b-restates");
    await waitFor(() =>
      expect(
        focusedRow(),
        `the address of the decision left the focus elsewhere; the stage drew ${drawnSlot(view)}`
      ).toBe("ask-answered")
    );
    await act(async () => messagesOut.resolve([]));
    expect(drawnSlot(view), "the stage drew the outline in place of the view").toBe("the view");
    expect(focusedRow(), "the focus left the question's row").toBe("ask-answered");
    expect(
      view.container.querySelector(
        `[data-node-id="${blockNodeId(SPEC_DOCUMENT.slug, "restates")}"]`
      ),
      "the view drew the decision that restates the answer as a row"
    ).toBeNull();
  } finally {
    asks.mockRestore();
  }
});

// Until the asks read answers, the tree holds no answer on the ask the write-up stands under, so
// the list draws it as the node the address names. The address waits for the asks rather than
// land on that row, which leaves the list once they answer and would take the focus with it.
test("a write-up's address waits for the asks read, which answers after the tree read, and lands on the question's row", async () => {
  const asksOut = Promise.withResolvers<IssueAskRow[]>();
  const asks = spyOn(api, "listIssueAskThreads").mockReturnValue(asksOut.promise as never);
  answerWriteUpTree();
  try {
    const view = drawStage("#b-writeup");
    await waitFor(() =>
      expect(
        view.container.querySelector(`[data-node-id="${WRITE_UP_NODE}"]`),
        "the write-up was not drawn while the asks read is out"
      ).not.toBeNull()
    );
    await act(async () => asksOut.resolve([...ASKED]));
    await waitFor(() =>
      expect(
        focusedRow(),
        `the write-up's address left the focus elsewhere once the asks answered; the stage drew ${drawnSlot(view)}`
      ).toBe("ask-answered")
    );
  } finally {
    asks.mockRestore();
  }
});

// Under words the answered question does not hold, the list draws neither the write-up, which is
// no row here, nor the question it answers, only the open question the words match, so the
// write-up's address has no row of the view to land on. The stage draws the outline in the view's
// place, which draws the write-up as a row under the question it answers and lands on it.
test("a write-up's address under words its question does not hold draws the outline, with the focus on the write-up's row", async () => {
  const asks = spyOn(api, "listIssueAskThreads").mockResolvedValue([...ASKED] as never);
  answerWriteUpTree();
  try {
    const view = drawStage("?q=flag#b-writeup");
    await waitFor(() =>
      expect(
        focusedRow(),
        `the write-up's address left the focus on ${document.activeElement?.tagName}; the stage drew ${drawnSlot(view)}`
      ).toBe(WRITE_UP_NODE)
    );
    // Only the outline draws the write-up's row, once the view has said it draws none, and the
    // stage takes nothing more the view says in this following, so nothing is waited for here.
    expect(drawnSlot(view), "the stage drew the view, which has no row for the write-up").toBe(
      "the body"
    );
    expect(focusedRow(), "the focus left the write-up's row").toBe(WRITE_UP_NODE);
  } finally {
    asks.mockRestore();
  }
});

/** The outline's row of the write-up, drawn in the Decisions view's place at the write-up's
 *  address under words its question does not hold: the asks read, which tells the write-up from
 *  a row the view draws, is held until the view is drawn, `press` is done then where it is given,
 *  and once the outline draws the row every frame asked for till then is run (`frames`). */
async function writeUpRowAfter(
  frames: { readonly run: () => Promise<void> },
  press?: () => void
): Promise<HTMLElement> {
  const asksOut = Promise.withResolvers<IssueAskRow[]>();
  spyOn(api, "listIssueAskThreads").mockReturnValue(asksOut.promise as never);
  answerWriteUpTree();
  const view = drawStage("?q=flag#b-writeup");
  await waitFor(() =>
    expect(drawnSlot(view), "the stage drew no view while the asks read is out").toBe("the view")
  );
  if (press !== undefined) act(press);
  await act(async () => asksOut.resolve([...ASKED]));
  const row = await waitFor(() => {
    const drawn = view.container.querySelector<HTMLElement>(
      `[data-issue-body] [data-node-row][data-node-id="${WRITE_UP_NODE}"]`
    );
    expect(
      drawn,
      `the outline drew no row of the write-up; the stage drew ${drawnSlot(view)}`
    ).not.toBeNull();
    return drawn as HTMLElement;
  });
  await frames.run();
  return row;
}

// The outline lands on the address's node until the reader's first act since they followed the
// address (`IssueTree`'s `addressActs`), which the stage watches from the following's drawing
// whichever slot it draws. With the asks read out the view stays drawn, and a press then on a
// control outside the stage is an act of that following: once the read answers, the outline
// drawn in the view's place leaves the focus on the control. With no act, the same address lands
// on the outline's row of the write-up.
test("a press while the view is drawn keeps the focus on the control pressed, where the outline drawn in the view's place lands on its row with no act", async () => {
  const frames = framesByHand();
  try {
    const landed = await writeUpRowAfter(frames);
    expect(landed, `with no act the outline left the focus on ${focusedRow()}`).toHaveFocus();
    cleanup();
    const control = render(<button type="button">A control outside the stage</button>).getByRole(
      "button"
    );
    await writeUpRowAfter(frames, () => {
      control.dispatchEvent(new window.PointerEvent("pointerdown", { bubbles: true }));
      control.focus();
    });
    expect(
      control,
      `a press while the view was drawn lost the focus to ${focusedRow() ?? document.activeElement?.tagName}`
    ).toHaveFocus();
  } finally {
    frames.restore();
  }
});

/** The Decisions view's row of the ask `id`. */
function askRow(id: string): string {
  return `[data-decisions-body] [data-node-row][data-node-id="${id}"]`;
}

// A key pressed on a row of the view is an act of the following too, but the row that holds the
// focus leaves the page with the view where the view then draws no node the address names. The
// outline drawn in the view's place lands as it does where the reader pressed nothing: with the
// comments read out the view draws the questions, `j` is pressed on the first one's row, and once
// the comments answer, the outline's line that the issue holds no such comment takes the focus.
test("`j` on a row of the Decisions view, before it draws the outline's line for a comment the issue holds none of in its place, leaves the focus on that line", async () => {
  spyOn(api, "listIssueAskThreads").mockResolvedValue([...ASKED] as never);
  answerWriteUpTree();
  const commentsOut = Promise.withResolvers<Comment[]>();
  spyOn(api, "listComments").mockReturnValue(commentsOut.promise);
  const read = commentGone();
  try {
    const view = drawStage(`?comment=${GONE_COMMENT}`);
    await rowTakesFocus(view, askRow("ask-open"), "the Decisions view with the comments out");
    act(() => {
      document.activeElement?.dispatchEvent(
        new KeyboardEvent("keydown", { bubbles: true, key: "j" })
      );
    });
    expect(
      view.container.querySelector(askRow("ask-open")),
      `the focus left the first question's row at \`j\` for ${focusedRow() ?? document.activeElement?.tagName}`
    ).toHaveFocus();
    await act(async () => commentsOut.resolve([]));
    await goneLineTakesFocus(view, "the comments answering after `j`");
  } finally {
    read.mockRestore();
  }
});

// Enter on the Retry of a read the view draws its own line for is an act of the following too,
// and it comes before that read answers. Where the view then draws no node the address names,
// the Retry left the page with its line, and the outline drawn in the view's place lands as it
// does where the reader pressed nothing: its line that the issue holds no such comment takes the
// focus. The read refused here is the asks read, one the view draws its own line for.
test("Enter on the Retry of the Decisions view's refused asks read, before it draws the outline's line for a comment the issue holds none of in its place, leaves the focus on that line", async () => {
  const asksOut = Promise.withResolvers<IssueAskRow[]>();
  spyOn(api, "listIssueAskThreads")
    .mockRejectedValueOnce(REFUSED)
    .mockReturnValueOnce(asksOut.promise as never);
  const read = commentGone();
  try {
    const view = drawStage(`?comment=${GONE_COMMENT}`);
    const retry = await rowTakesFocus(
      view,
      `[data-decisions-body] ${FAILED_READ_RETRY}`,
      "the view's refused asks read"
    );
    // Enter on a button: its keydown, then the press it makes.
    act(() => {
      retry.dispatchEvent(new KeyboardEvent("keydown", { bubbles: true, key: "Enter" }));
      retry.click();
    });
    await waitFor(() => expect(retry).toHaveAttribute("aria-busy", "true"));
    await act(async () => asksOut.resolve([...ASKED]));
    await goneLineTakesFocus(view, "the asks read answering after Enter on its Retry");
  } finally {
    read.mockRestore();
  }
});

/** The reader's unread set on the issue as its read answers it: nothing unread. */
const NOTHING_UNREAD = { as_of: "2026-11-05T02:00:00Z", limit: 200, offset: 0, rows: [], total: 0 };

// The page's reads are refreshed by events, never by their age, and the stage draws the outline
// anew as the reader comes back to Conversation from a view. Under the app's `staleTime` a read is
// stale 30 s after it answered (`main.tsx`), each observer judging it by its own options: the
// outline's tree, its rows and the mark that reads a thread in the reader's Inbox
// (`outline-thread-read.tsx`) each read who is reading, and would ask for it again on a return more
// than 30 s after the page loaded unless they read it as the tree's own reads are kept fresh
// (`useIssueTree.ts`'s `EVENT_FRESH`). Here every read the page asks answered 31 s ago.
test("coming back to the outline from Decisions after the app's staleTime asks who is reading no second time", async () => {
  // The stub of who is reading that `stubReads` set: `spyOn` hands back that same spy, which
  // keeps its answer, and this counts its calls.
  const who = spyOn(api, "whoAmI");
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
  });
  const issue = issueDetails();
  const answered = { updatedAt: Date.now() - 31_000 };
  client.setQueryData(
    whoAmIQuery().queryKey,
    { event_head: "0", kind: "user", login: READER },
    answered
  );
  client.setQueryData(
    issueCoverageQuery(KEY).queryKey,
    { gaps: [], has_plan: false, issue: KEY, requirements: [], spec_artifact_id: "" },
    answered
  );
  client.setQueryData(
    issueResultsQuery(KEY).queryKey,
    { checks: [], issue: KEY, verification_artifact_id: "" },
    answered
  );
  client.setQueryData(issueAskThreadsQuery(KEY).queryKey, [], answered);
  // The issue holds two threads, so the outline draws rows.
  client.setQueryData(["comments", KEY], [...TWO_THREADS], answered);
  client.setQueryData(["issue-messages", KEY], [], answered);
  // What the reader holds unread on the issue, which a thread's read mark reads.
  client.setQueryData(myRepliesOwnerQuery({ issue: KEY }).queryKey, NOTHING_UNREAD, answered);
  // The tree read: the issue's two threads, each comment a node of it.
  client.setQueryData(
    outlineQuery(KEY, "conversation").queryKey,
    treeOf(commentNodes(TWO_THREADS)),
    answered
  );
  // The names of what the tree's rows name: the issue itself and each of its comments.
  const named = [
    buildDispatchReference({ key: KEY, kind: "issue" }),
    ...TWO_THREADS.map(({ id }) => buildDispatchReference({ id, key: KEY, kind: "comment" })),
  ];
  client.setQueryData(referenceTargetsQuery(named).queryKey, { targets: [] }, answered);
  const held: { navigate?: NavigateFunction } = {};
  const view = drawStage("", { client, held, issue });
  expect(await emptyLineOf(view)).toBe(EMPTY_STATE_LINES.decisions);
  expect(who, "the Decisions view asked who is reading").not.toHaveBeenCalled();
  // The reader leaves the view for the outline at the second thread's reply, as a link to it
  // does: the outline opens that thread, and the reply's row draws the thread's read mark.
  await act(async () => held.navigate?.(`${presetPath(KEY, "conversation")}?comment=reply-b`));
  await waitFor(() =>
    expect(
      view.container.querySelector('[data-issue-body] [data-thread-read="head-b"]'),
      `the stage drew ${drawnSlot(view)}, with no read mark of the second thread`
    ).not.toBeNull()
  );
  // Each read the outline's mount asks for is sent from its observer's effect: a flush of effects
  // and microtasks once the mark is drawn sends any not yet sent.
  await act(async () => {});
  expect(who, "the outline drawn again asked who is reading").not.toHaveBeenCalled();
});

/** How many times `read`, the stub of one read, is asked for as `node` is drawn, under the app's
 *  `staleTime` with who is reading and the reader's unread set on the issue each answered 31 s
 *  ago. */
async function requestsOf(
  read: { readonly mock: { readonly calls: readonly unknown[] }; mockClear(): unknown },
  node: ReactNode
): Promise<number> {
  read.mockClear();
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
  });
  const answered = { updatedAt: Date.now() - 31_000 };
  client.setQueryData(
    whoAmIQuery().queryKey,
    { event_head: "0", kind: "user", login: READER },
    answered
  );
  client.setQueryData(myRepliesOwnerQuery({ issue: KEY }).queryKey, NOTHING_UNREAD, answered);
  const view = render(<QueryClientProvider client={client}>{node}</QueryClientProvider>);
  // A read the mount asks for is sent from its observer's effect, which `render` runs: a flush of
  // effects and microtasks after it sends any not yet sent.
  await act(async () => {});
  view.unmount();
  return read.mock.calls.length;
}

/** Who is reading as a surface that calls `useIsHuman` with `options` reads it: the hook alone. */
function ReadsWhoIsReading({ options }: { options?: EventFreshness }): ReactNode {
  useIsHuman(options);
  return null;
}

// A surface that calls `useIsHuman` with no options reads who is reading under the app's
// defaults, and asks for it again once it mounts past the app's `staleTime`, more than 30 s after
// the answer. A surface that gives the tree's freshness (`EVENT_FRESH`) asks it no second time.
test("who is reading, read with no options, is asked again after the app's staleTime, and read with the tree's freshness is not", async () => {
  // The stub `stubReads` set, which `spyOn` hands back: an answer that never comes, so no read
  // updates the hook after `requestsOf` has counted the requests.
  const who = spyOn(api, "whoAmI").mockReturnValue(Promise.withResolvers<never>().promise);
  expect(await requestsOf(who, <ReadsWhoIsReading />), "a surface that gives no options").toBe(1);
  expect(
    await requestsOf(who, <ReadsWhoIsReading options={EVENT_FRESH} />),
    "a surface that gives the tree's freshness"
  ).toBe(0);
});

/** A thread's read mark as a surface that gives the hook no freshness draws it: the hook alone,
 *  with no element to watch. */
function MarkGivingNoFreshness({ mark }: { mark: ThreadMark }): ReactNode {
  useMarkThreadRead({
    enabled: true,
    newestShownAt: mark.at,
    owner: { issue: mark.issueKey },
    threadId: mark.threadId,
  });
  return null;
}

// A thread's read mark reads the reader's unread set on the issue as well as who is reading
// (`useMarkThreadRead`), and the stage draws it again at each press of a view. The feed's rules
// refresh that set whenever an event changes it (`api/event-refresh.ts`), so the mark reads it as
// the tree's own reads are kept fresh, and asks it no second time past the app's `staleTime`. A
// surface that gives the hook no freshness reads it under the app's defaults, and asks it again.
test("a thread's read mark drawn after the app's staleTime asks the unread set no second time, where a surface that gives no freshness asks it again", async () => {
  // Answers that never come, so no read updates what is drawn after `requestsOf` has counted the
  // requests. Who is reading is the stub `stubReads` set, which `spyOn` hands back.
  spyOn(api, "whoAmI").mockReturnValue(Promise.withResolvers<never>().promise);
  const replies = spyOn(api, "listMyReplies").mockReturnValue(
    Promise.withResolvers<never>().promise
  );
  const mark: ThreadMark = { at: "2026-11-05T01:04:00Z", issueKey: KEY, threadId: "head-b" };
  try {
    expect(
      await requestsOf(replies, <ThreadReadMark mark={mark} />),
      "the thread's read mark"
    ).toBe(0);
    expect(
      await requestsOf(replies, <MarkGivingNoFreshness mark={mark} />),
      "a surface that gives no freshness"
    ).toBe(1);
  } finally {
    replies.mockRestore();
  }
});

// Every entry the browser makes has the key "default". Under the Decisions view and words, a
// decision its fragment names keeps the view, which draws it whatever the words. A reader's
// write-up, changed to with the fragment alone under the same key and words, is no row of the
// list where the words match no question it answers: the stage draws the outline in the view's
// place, which lands on the write-up's row.
test("under the Decisions view and words, a fragment the browser changes under one key from a decision to a write-up the list draws no row for draws the outline, with the focus on the write-up's row", async () => {
  spyOn(api, "listIssueAskThreads").mockResolvedValue([...ASKED] as never);
  answerWriteUpTree([decision("d1")]);
  // The messages read is the one held: it answers last, and with it every read the tree is built
  // from is held.
  const messagesOut = Promise.withResolvers<[]>();
  spyOn(api, "listIssueMessages").mockReturnValue(messagesOut.promise);
  const decisions = `${presetPath(KEY, "decisions")}?q=flag`;
  const view = drawAtBrowserEntry(`${decisions}#b-d1`, issueDetails());
  await rowTakesFocus(
    view,
    `[data-decisions-body] [data-node-row][data-node-id="${decisionNode("d1").id}"]`,
    "the decision's fragment"
  );
  await act(async () => messagesOut.resolve([]));
  expect(drawnSlot(view), "the decision's fragment drew the outline in place of the view").toBe(
    "the view"
  );
  view.follow(`${decisions}#b-writeup`);
  await rowTakesFocus(
    view,
    `[data-issue-body] [data-node-row][data-node-id="${WRITE_UP_NODE}"]`,
    "the write-up's fragment under the same key and words"
  );
  expect(
    view.container.querySelector("[data-decisions-body]"),
    "the write-up's fragment drew the Decisions view beside the outline"
  ).toBeNull();
});

// A read the view draws no line of its own for, refused, is no row of the view. The Decisions
// view draws a line for the decisions' reads, the tree read and its decisions view, and for the
// asks alone, so with the comments read refused a comment named beside it lands on nothing there:
// the stage draws the outline in the view's place, which says the comments could not be loaded,
// with the focus on its Retry, as it did before the view was filled.
test("the comments read refused, a comment named beside the Decisions view draws the outline in its place, with the comments' line and the focus on its Retry", async () => {
  spyOn(api, "listComments").mockRejectedValue(REFUSED);
  const view = drawStage("?comment=reply-b");
  await retryTakesFocus(view, STATE_LINES.comments_unreadable, "the comment's address");
  expect(
    view.container.querySelector("[data-decisions-body]"),
    "the Decisions view was drawn beside the outline"
  ).toBeNull();
});

// The Decisions view drew its rows' references from the page's names and said nothing when their
// read failed: every reference in an entry's words stayed its raw address, with no word of why
// and no Retry. It draws the outline's names line over its list, after its own reads' lines, and
// its Retry reads the names again.
test("under the Decisions view, a failed names read draws the outline's names line over the list, and its Retry reads the names again", async () => {
  // A decision node of the tree read whose words cite another issue, which the names read names.
  const cited = buildDispatchReference({ key: "CORE-2", kind: "issue" });
  answerTree([decision("d1", { text: `The decision d1 follows ${cited}.` })]);
  // The stub `stubReads` set, which `spyOn` hands back: refused once, then its own answer.
  const names = spyOn(api, "getReferenceTargets").mockRejectedValueOnce(REFUSED);
  const view = drawStage("");
  const namesLine = () =>
    view.queryByText(STATE_LINES.names_unreadable)?.closest<HTMLElement>("[data-failed-read]") ??
    null;
  const line = await waitFor(() => {
    const drawn = namesLine();
    expect(drawn, "the Decisions view drew no line for the names read").not.toBeNull();
    return drawn as HTMLElement;
  });
  const list = view.container.querySelector("[data-decisions-body]");
  const tree = list?.querySelector('[role="tree"]');
  if (list == null || tree == null) throw new Error(`the stage drew ${drawnSlot(view)}`);
  expect(
    {
      inList: list.contains(line),
      inTree: tree.contains(line),
      overTree: (line.compareDocumentPosition(tree) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0,
    },
    "the names line is not drawn in the list, over its tree"
  ).toEqual({ inList: true, inTree: false, overTree: true });
  const retry = line.querySelector<HTMLElement>(FAILED_READ_RETRY);
  if (retry === null) throw new Error("the Decisions view's names line drew no Retry");
  await act(async () => retry.click());
  await waitFor(() =>
    expect(namesLine(), "the Decisions view's names line stayed once the names answered").toBeNull()
  );
  expect(names.mock.calls.length, "the Retry did not ask the names again").toBe(2);
});
