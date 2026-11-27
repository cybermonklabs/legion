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
import type { Comment, DecisionKeep, IssueAskRow, MessageRead } from "../../../api/types";
import { type EventFreshness, useIsHuman } from "../../../hooks/useIsHuman";
import { answeredAsk } from "../../inbox/ask-card-fixture";
import { DESKTOP_WIDTH, stubViewport } from "../../margin/margin-fixture";
import { EVENT_FRESH } from "../../refs/reference-target";
import { buildDispatchReference } from "../../refs/routes";
import { useMarkThreadRead } from "../../threads/useMarkThreadRead";
import { plainWrite, writesOf } from "../../writes/write-store";
import { rowActionSelector } from "../DecisionActions";
import { presetPath } from "../issue-address";
import { framesByHand, readComment, TWO_THREADS, waitInAct } from "../outline-fixtures";
import { type ThreadMark, ThreadReadMark } from "../outline-thread-read";
import { PROPERTY_LABELS } from "../page-structure";
import { EMPTY_STATE_LINES, FILTER_SIGNAL_WORDS, STATE_LINES } from "../page-words";
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
  rowTakesFocus,
  stubReads,
  treeOf,
  watchDrawn,
} from "../slots/stage-fixtures";
import { blockNodeId } from "../tree-drafts";
import { commentNode, pagedAnswer, treeAnswer, treeNode } from "../tree-read/tree-fixtures";
import {
  outlineQuery,
  type TreeAnswer,
  type TreeAnswerNode,
  viewKey,
} from "../tree-read/tree-read";
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
  cleanup();
});

test("words of spaces alone draw the empty issue's line, as no words do", async () => {
  expect(await emptyLineOf(drawStage("?q=%20%20"))).toBe(EMPTY_STATE_LINES.decisions);
  cleanup();
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
    cleanup();
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
  view.unmount();
});

// A comment the tree holds, named beside the Decisions view, is the view's to land on: it draws the
// comment as a row before its parts, with the focus on it, and the stage keeps the view once every
// read is held.
test("a comment the tree holds, named beside the Decisions view, keeps the view, with the focus on the comment's row", async () => {
  const comments = spyOn(api, "listComments");
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
    expect(comments, "the page read the issue's comments").not.toHaveBeenCalled();
    expect(focusedRow(), "the focus left the comment's row").toBe("reply-b");
  } finally {
    cleanup();
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

/** The decisions view read's answer holding `decisions`, each a node its query took
 *  (`role: "query"`), as the server's view answers each decision of the issue. */
function decisionsView(decisions: readonly TreeAnswerNode[]): TreeAnswer {
  return treeOf(decisions.map((node): TreeAnswerNode => ({ ...node, role: "query" })));
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
    cleanup();
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
      ? decisionsView([tied("first"), tied("later")])
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
  view.unmount();
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
  await act(async () => viewOut.resolve(decisionsView([tied("first"), tied("later")])));
  await waitFor(() =>
    expect(
      focusedRow(),
      `the later decision's address left the focus elsewhere; the stage drew ${drawnSlot(view)}`
    ).toBe(later)
  );
  await waitInAct(20);
  expect(drawnSlot(view), "the stage drew the outline in place of the view").toBe("the view");
  expect(focusedRow(), "the focus left the later decision's row").toBe(later);
  view.unmount();
});

// R4: a refused Keep says so on the row. A Keep, however it ends, reads the tree again and drops
// the views the page holds (`DecisionActions.tsx`), so the decisions view read is read again;
// until its next answer lands the page keeps drawing the answer the drop took
// (`useDecisionsView`), as the Spec view's column does its own. A decision only that read holds,
// past Conversation's first page, keeps its row while the read is out again, the row keeps the
// focus, and the row keeps saying the Keep was refused (`409 DECISION_CHANGED`).
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
      ? Promise.resolve(decisionsView([tied("first"), tied("later")]))
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
    await user.click(await keepOf(later));
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
    cleanup();
    keep.mockRestore();
  }
});

/** What the view row's Decisions control draws - its count and its flag of what waits for
 *  review - and the header's count of what waits, where the page draws each. */
function waitingCounts(view: RenderResult) {
  return {
    count: view.container.querySelector('[data-preset-count="decisions"]')?.textContent,
    flag: view.container.querySelector('[data-preset-flag="to_review"]')?.textContent,
    header: view.container.querySelector('[data-testid="issue-whose-turn"]')?.textContent,
  };
}

/** Answers the coverage read with statement `s1` of the spec (`S1`), which makes a waiting
 *  decision tied to it one of the review list (`coverageStatements`): the Decisions control's
 *  flag and the header's count count it. */
function coverS1(): void {
  spyOn(api, "getIssueCoverage").mockResolvedValue({
    gaps: [],
    has_plan: false,
    issue: KEY,
    requirements: [
      { block_id: "s1", checks: 1, gaps: [], plan_tasks: 0, ref: specRef("s1"), text: "S1." },
    ],
    spec_artifact_id: SPEC_DOCUMENT.id,
  });
}

/** The Keep of the later decision as Dispatch takes it. */
const KEPT_LATER = {
  artifact_id: SPEC_DOCUMENT.id,
  block_id: "later",
  changed_version: 1,
  kept_at: "2026-11-17T00:00:00Z",
  kept_by: READER,
} satisfies DecisionKeep;

/** The Keep control of the row of decision `id` the slot `within` draws, the Decisions view's
 *  where none is given, once it draws it. */
async function keepOf(id: string, within = "[data-decisions-body]"): Promise<HTMLElement> {
  return await waitFor(() => {
    const button = document
      .querySelector(`${within} [data-node-row][data-node-id="${id}"]`)
      ?.querySelector<HTMLElement>(rowActionSelector("keep"));
    if (button == null) throw new Error(`the row of ${id} draws no Keep`);
    return button;
  });
}

/** Decision `later` as Conversation's read answers it once the reader kept it: waiting for no
 *  review. */
function keptLater(): TreeAnswerNode {
  return decision("later", { decision: { links: [shaped(specRef("s1"))], waiting_review: false } });
}

/** The header's count of what waits for `count` things, and the Decisions control's flag of
 *  `count` decisions to review. */
function waitingFor(count: number) {
  return {
    flag: ` · ${count} ${FILTER_SIGNAL_WORDS.to_review}`,
    header: `${PROPERTY_LABELS.waiting_on_you} (${count})`,
  };
}

/** The header's own reads: the issue, the reader's state of it and the projects, whose gates
 *  only a root issue reads. */
function answerHeaderReads(): { mockRestore(): void }[] {
  return [
    spyOn(api, "getIssue").mockResolvedValue(issueDetails()),
    spyOn(api, "getMyState").mockResolvedValue({}),
    spyOn(api, "listProjects").mockResolvedValue([]),
  ];
}

// The reset each load of the issue makes of the views (`ViewReadProvider`): no load draws a
// decisions view older than its own tree. An earlier visit of the issue read the decisions view
// while it held a decision since removed, and the cache keeps that answer as the reader comes
// back to the issue at the address of a later decision, with this load's read of the view held
// out. No reader takes the earlier visit's answer (`useDecisionsView`), on the load's first
// render either: until this load's read answers, the Decisions control and its flag count
// Conversation's read, and the view says nothing yet of the later decision, which no answer held
// so far holds, so the stage keeps the view; once it answers, the control counts it and the
// address lands on the later decision's row in the view.
test("coming back to the issue, the Decisions control, its flag and an address read no decisions view answer of the earlier visit, and this load's once it lands", async () => {
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  coverS1();
  const headerReads = answerHeaderReads();
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const thisLoad = Promise.withResolvers<TreeAnswer>();
  let earlierVisit = true;
  spyOn(api, "getIssueTree").mockImplementation((_key, query) => {
    if (query?.view !== "decisions") return Promise.resolve(treeOf([S1, tied("first")]));
    return earlierVisit
      ? Promise.resolve(decisionsView([tied("first"), tied("removed")]))
      : thisLoad.promise;
  });
  try {
    const earlier = drawStage("", { client, page: true });
    await waitFor(() =>
      expect(
        waitingCounts(earlier),
        "the earlier visit's count of its decisions view read"
      ).toMatchObject({ count: "2", flag: waitingFor(2).flag })
    );
    earlier.unmount();
    earlierVisit = false;

    const later = decisionNode("later").id;
    const view = drawStage("#b-later", { client, page: true });
    await waitFor(() =>
      expect(waitingCounts(view), "the count over Conversation's read").toMatchObject({
        count: "1",
        flag: waitingFor(1).flag,
      })
    );
    await waitInAct(20);
    expect(
      waitingCounts(view),
      "the Decisions control counted the earlier visit's decisions view read"
    ).toMatchObject({ count: "1", flag: waitingFor(1).flag });
    expect(
      drawnSlot(view),
      "the stage drew the outline in place of the view before this load's decisions view read answered"
    ).toBe("the view");

    await act(async () => thisLoad.resolve(decisionsView([tied("first"), tied("later")])));
    await waitFor(() =>
      expect(
        focusedRow(),
        `the later decision's address left the focus elsewhere; the stage drew ${drawnSlot(view)}`
      ).toBe(later)
    );
    expect(
      waitingCounts(view),
      "the count once this load's decisions view read answered"
    ).toMatchObject({ count: "2", flag: waitingFor(2).flag });
    await waitInAct(20);
    expect(drawnSlot(view), "the stage drew the outline in place of the view").toBe("the view");
  } finally {
    cleanup();
    for (const read of headerReads) read.mockRestore();
    restoreMatchMedia();
  }
});

// The control after the Decisions view's last row reads the answer the page holds of the view
// (`useNextViewPage`, `useDecisionsView`), which is this load's alone. An earlier visit of the
// issue read the decisions view while its answer named a next page, and the cache keeps that
// answer as the reader comes back to the Decisions view, with this load's read of the view held
// out. The view's first render, before the load's reset drops the view, draws no next-page
// control of that answer, and none is drawn while this load's read is out.
test("on a return to the issue, the Decisions view's first render draws no next-page control of the earlier visit's answer", async () => {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  let earlierVisit = true;
  spyOn(api, "getIssueTree").mockImplementation((_key, query) => {
    if (query?.view !== "decisions") return Promise.resolve(treeOf([S1, tied("first")]));
    return earlierVisit
      ? Promise.resolve(pagedAnswer(decisionsView([tied("first"), tied("later")]), "page-2"))
      : Promise.withResolvers<TreeAnswer>().promise;
  });
  const control = "[data-tree-more]";
  const earlier = drawStage("", { client });
  await waitFor(() =>
    expect(
      earlier.container.querySelector(control),
      "the earlier visit drew no next-page control of its decisions view answer"
    ).not.toBeNull()
  );
  earlier.unmount();
  earlierVisit = false;

  // Every element the return's renders put on the page or took off it, its first render's among
  // them, which the load's reset redraws before `render` hands the page back.
  const drewControl = watchDrawn(control);
  const view = drawStage("", { client });
  expect(
    drewControl(),
    "the Decisions view's first render on the return drew the earlier visit's next-page control"
  ).toBe(false);
  await waitInAct(20);
  expect(
    view.container.querySelector(control),
    "the Decisions view drew the earlier visit's next-page control while this load's read is out"
  ).toBeNull();
  view.unmount();
});

// R4, and the line a refused read draws over the list (`decisions-entries.ts`' `unreadable`): a
// Keep, however it ends, drops the views the page holds and reads the decisions view again
// (`DecisionActions.tsx`), and the page draws the answer the drop took until the next one lands
// (`useDecisionsView`). Where that read is refused, the view says so over the rows it keeps: the
// decisions' failed read's line stands over the list with its Retry, and the later decision,
// which of the reads only that answer holds, keeps its row. The Retry reads the view again, and
// its answer takes the line away.
test("a refused read of the decisions view after a Keep draws the failed read's line and its Retry over the rows the page keeps", async () => {
  const keep = spyOn(api, "keepDecision").mockResolvedValue(KEPT_LATER);
  let viewReads = 0;
  let refusing = true;
  spyOn(api, "getIssueTree").mockImplementation((_key, query) => {
    if (query?.view !== "decisions") return Promise.resolve(treeOf([S1, tied("first")]));
    viewReads += 1;
    return viewReads > 1 && refusing
      ? Promise.reject(REFUSED)
      : Promise.resolve(decisionsView([tied("first"), tied("later")]));
  });
  const later = decisionNode("later").id;
  const user = userEvent.setup();
  try {
    const view = drawStage("");
    const row = () =>
      view.container.querySelector(
        `[data-decisions-body] [data-node-row][data-node-id="${later}"]`
      );
    const line = () =>
      view.queryByText(STATE_LINES.issue_decisions_unreadable)?.closest("[data-failed-read]") ??
      null;
    await user.click(await keepOf(later));
    await waitFor(() => expect(keep, "the Keep was never sent").toHaveBeenCalledTimes(1));
    await waitFor(() =>
      expect(
        line(),
        "the decisions view's read after the Keep is refused, and the view draws no line that says so"
      ).not.toBeNull()
    );
    const retry = line()?.querySelector<HTMLElement>(FAILED_READ_RETRY);
    expect(retry, "the refused read's line draws no Retry").not.toBeNull();
    expect(
      row(),
      "the later decision's row left the view under the refused read's line"
    ).not.toBeNull();
    expect(drawnSlot(view), "the stage drew the outline in place of the view").toBe("the view");

    refusing = false;
    if (retry != null) await user.click(retry);
    await waitFor(() =>
      expect(line(), "the line stayed once the Retry's read answered").toBeNull()
    );
    expect(row(), "the later decision's row left the view once the read answered").not.toBeNull();
  } finally {
    cleanup();
    keep.mockRestore();
  }
});

// R13, R35: the header's count of what waits and the Decisions control count the one decisions
// view answer the page holds (`useDecisionsView`), so the two cannot disagree. Conversation's
// first page holds the first decision and names a next page; the decisions view read holds the
// later one too, both waiting for the reader's review. A Keep of the later decision drops the
// view and reads it again, and while that read is out the header's count and the control both
// count the answer the drop took, two decisions with no "+", and neither counts Conversation's
// first page.
test("while a Keep's read of the decisions view is out, the header's count of what waits counts what the Decisions control counts", async () => {
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  coverS1();
  const headerReads = answerHeaderReads();
  const keep = spyOn(api, "keepDecision").mockResolvedValue(KEPT_LATER);
  const again = Promise.withResolvers<TreeAnswer>();
  let viewReads = 0;
  spyOn(api, "getIssueTree").mockImplementation((_key, query) => {
    if (query?.view !== "decisions") {
      return Promise.resolve({ ...treeOf([S1, tied("first")]), cursor: "page-2" });
    }
    viewReads += 1;
    return viewReads === 1
      ? Promise.resolve(decisionsView([tied("first"), tied("later")]))
      : again.promise;
  });
  const counted = { count: "2", ...waitingFor(2) };
  const user = userEvent.setup();
  try {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const view = drawStage("", { client, page: true });
    await waitFor(() =>
      expect(waitingCounts(view), "the counts once the decisions view read answered").toEqual(
        counted
      )
    );
    await user.click(await keepOf(decisionNode("later").id));
    await waitFor(() => expect(keep, "the Keep was never sent").toHaveBeenCalledTimes(1));
    await waitFor(() =>
      expect(viewReads, "the Keep did not read the decisions view again").toBeGreaterThan(1)
    );
    await waitInAct(20);
    expect(
      waitingCounts(view),
      "while the Keep's read of the decisions view is out, the header's count and the Decisions control count different answers"
    ).toEqual(counted);
  } finally {
    cleanup();
    keep.mockRestore();
    for (const read of headerReads) read.mockRestore();
    restoreMatchMedia();
  }
});

// The control after the Decisions view's last row reads the answer the page holds of the view
// (`useNextViewPage`, `useDecisionsView`), as the list does. The decisions view read's answer
// names a next page; a Keep of the later decision drops the view and reads it again, and while
// that read is out the view draws the later decision's row from the answer the drop took and,
// after the list, that answer's next-page control.
test("while a Keep's read of the decisions view is out, the view draws the next-page control of the kept answer, which names a next page", async () => {
  const keep = spyOn(api, "keepDecision").mockResolvedValue(KEPT_LATER);
  let viewReads = 0;
  spyOn(api, "getIssueTree").mockImplementation((_key, query) => {
    if (query?.view !== "decisions") return Promise.resolve(treeOf([S1, tied("first")]));
    viewReads += 1;
    return viewReads === 1
      ? Promise.resolve(pagedAnswer(decisionsView([tied("first"), tied("later")]), "page-2"))
      : Promise.withResolvers<TreeAnswer>().promise;
  });
  const later = decisionNode("later").id;
  const user = userEvent.setup();
  try {
    const view = drawStage("");
    const control = () => view.container.querySelector("[data-tree-more]");
    await waitFor(() =>
      expect(control(), "the decisions view read's answer drew no next-page control").not.toBeNull()
    );
    await user.click(await keepOf(later));
    await waitFor(() => expect(keep, "the Keep was never sent").toHaveBeenCalledTimes(1));
    await waitFor(() =>
      expect(viewReads, "the Keep did not read the decisions view again").toBeGreaterThan(1)
    );
    await waitInAct(20);
    expect(
      view.container.querySelector(
        `[data-decisions-body] [data-node-row][data-node-id="${later}"]`
      ),
      "the later decision's row left the view while the Keep's read is out"
    ).not.toBeNull();
    expect(
      control(),
      "while the Keep's read is out, the view draws no next-page control of the answer the Keep's drop took"
    ).not.toBeNull();
  } finally {
    cleanup();
    keep.mockRestore();
  }
});

// A press of that control while the Keep's read is out (`readNextTreePage`): the cache holds no
// answer of the view, which the drop reset, so the press waits for the read out, sends no first
// page of its own, and once that read answers reads the page after the answer it brings, as one
// press. The page holds a third decision, whose row the view then draws.
test("a press of the kept answer's next-page control while a Keep's read of the decisions view is out reads the next page once that read answers", async () => {
  const keep = spyOn(api, "keepDecision").mockResolvedValue(KEPT_LATER);
  const keepRead = Promise.withResolvers<TreeAnswer>();
  let viewReads = 0;
  const pages: string[] = [];
  spyOn(api, "getIssueTree").mockImplementation((_key, query) => {
    if (query?.view !== "decisions") return Promise.resolve(treeOf([S1, tied("first")]));
    if (query.cursor !== undefined) {
      pages.push(query.cursor);
      return Promise.resolve({
        ...decisionsView([]),
        cursor: null,
        nodes: [{ ...tied("third"), role: "query" }],
      });
    }
    viewReads += 1;
    return viewReads === 1
      ? Promise.resolve(pagedAnswer(decisionsView([tied("first"), tied("later")]), "page-2"))
      : keepRead.promise;
  });
  const third = () =>
    document.querySelector(
      `[data-decisions-body] [data-node-row][data-node-id="${decisionNode("third").id}"]`
    );
  const user = userEvent.setup();
  try {
    const view = drawStage("");
    const control = () => view.container.querySelector<HTMLElement>("[data-tree-more] button");
    await waitFor(() =>
      expect(control(), "the decisions view read's answer drew no next-page control").not.toBeNull()
    );
    await user.click(await keepOf(decisionNode("later").id));
    await waitFor(() => expect(keep, "the Keep was never sent").toHaveBeenCalledTimes(1));
    await waitFor(() =>
      expect(viewReads, "the Keep did not read the decisions view again").toBeGreaterThan(1)
    );
    await waitInAct(20);
    const readsBefore = viewReads;
    const press = control();
    expect(press, "the kept answer drew no next-page control").not.toBeNull();
    if (press != null) await user.click(press);
    await waitInAct(20);
    expect(pages, "the press read a page while the Keep's read is out").toEqual([]);
    expect(viewReads, "the press sent a first page of the view of its own").toBe(readsBefore);

    await act(async () =>
      keepRead.resolve(pagedAnswer(decisionsView([tied("first"), tied("later")]), "page-2"))
    );
    await waitFor(() =>
      expect(pages, "the press read no next page once the Keep's read answered").toEqual(["page-2"])
    );
    await waitFor(() =>
      expect(third(), "the view drew no row of the next page's decision").not.toBeNull()
    );
    await waitInAct(20);
    expect(pages, "the press read the next page more than once").toEqual(["page-2"]);
  } finally {
    cleanup();
    keep.mockRestore();
  }
});

// The landing of that press (`NextViewPage`): a Keep moves the kept decision's row from the
// review list to the reader's own part, each part its own element, so the Keep's read, which the
// press waits for and which answers before the next page does, draws that row anew. The press
// lands on the first row of a node not drawn as it was made: the next page's third decision,
// never the kept one, which was drawn then.
test("a press of the kept answer's next-page control while a Keep's read is out puts the focus on the next page's first row, not on the kept decision's", async () => {
  const keep = spyOn(api, "keepDecision").mockResolvedValue(KEPT_LATER);
  const keepRead = Promise.withResolvers<TreeAnswer>();
  const nextPage = Promise.withResolvers<TreeAnswer>();
  let viewReads = 0;
  spyOn(api, "getIssueTree").mockImplementation((_key, query) => {
    if (query?.view !== "decisions") return Promise.resolve(treeOf([S1, tied("first")]));
    if (query.cursor !== undefined) return nextPage.promise;
    viewReads += 1;
    return viewReads === 1
      ? Promise.resolve(pagedAnswer(decisionsView([tied("first"), tied("later")]), "page-2"))
      : keepRead.promise;
  });
  const later = decisionNode("later").id;
  const third = decisionNode("third").id;
  const rowOf = (id: string) =>
    document.querySelector(`[data-decisions-body] [data-node-row][data-node-id="${id}"]`);
  const user = userEvent.setup();
  try {
    const view = drawStage("");
    const control = () => view.container.querySelector<HTMLElement>("[data-tree-more] button");
    await waitFor(() =>
      expect(control(), "the decisions view read's answer drew no next-page control").not.toBeNull()
    );
    await user.click(await keepOf(later));
    await waitFor(() => expect(keep, "the Keep was never sent").toHaveBeenCalledTimes(1));
    await waitFor(() =>
      expect(viewReads, "the Keep did not read the decisions view again").toBeGreaterThan(1)
    );
    await waitInAct(20);
    const press = control();
    expect(press, "the kept answer drew no next-page control").not.toBeNull();
    if (press != null) await user.click(press);
    await waitInAct(20);

    // The Keep's read answers with the later decision kept, as the server answers it, and is
    // drawn before the next page lands.
    await act(async () =>
      keepRead.resolve(pagedAnswer(decisionsView([tied("first"), keptLater()]), "page-2"))
    );
    await waitFor(() =>
      expect(
        rowOf(later)?.closest("[data-decisions-part]")?.getAttribute("data-decisions-part"),
        "the Keep's read did not draw the later decision out of the review list"
      ).not.toBe("review")
    );
    await act(async () =>
      nextPage.resolve({
        ...decisionsView([]),
        cursor: null,
        nodes: [{ ...tied("third"), role: "query" }],
      })
    );
    await waitFor(() =>
      expect(rowOf(third), "the view drew no row of the next page's decision").not.toBeNull()
    );
    await waitInAct(20);
    expect(
      focusedRow(),
      "the press put the focus on another row than the next page's first, the kept decision's"
    ).toBe(third);
  } finally {
    cleanup();
    keep.mockRestore();
  }
});

// The bound of the answer a Keep's drop keeps (`useDecisionsView`): only the Decisions view
// draws it, while the read the drop started is out. A Keep of the later decision on the
// Decisions view drops the views and reads them again; before the decisions view's read answers,
// the reader moves to Conversation, whose read answers again with the later decision kept. There
// the header's count of what waits and the Decisions control's flag count that read, the first
// decision alone, as before the view was ever read, and never the answer the drop took, which
// has the later decision waiting still.
test("on Conversation after a Keep, the header's count and the Decisions control's flag count Conversation's read, not the answer the Keep's drop took", async () => {
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  coverS1();
  const headerReads = answerHeaderReads();
  let kept = false;
  const keep = spyOn(api, "keepDecision").mockImplementation(async () => {
    kept = true;
    return KEPT_LATER;
  });
  let viewReads = 0;
  spyOn(api, "getIssueTree").mockImplementation((_key, query) => {
    if (query?.view !== "decisions") {
      return Promise.resolve(treeOf([S1, tied("first"), kept ? keptLater() : tied("later")]));
    }
    viewReads += 1;
    return viewReads === 1
      ? Promise.resolve(decisionsView([tied("first"), tied("later")]))
      : Promise.withResolvers<TreeAnswer>().promise;
  });
  const held: { navigate?: NavigateFunction } = {};
  const user = userEvent.setup();
  try {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const view = drawStage("", { client, held, page: true });
    await waitFor(() =>
      expect(waitingCounts(view), "the counts once the decisions view read answered").toMatchObject(
        waitingFor(2)
      )
    );
    await user.click(await keepOf(decisionNode("later").id));
    await waitFor(() => expect(keep, "the Keep was never sent").toHaveBeenCalledTimes(1));
    await waitFor(() =>
      expect(viewReads, "the Keep did not read the decisions view again").toBeGreaterThan(1)
    );
    act(() => held.navigate?.(presetPath(KEY, "conversation")));
    await waitFor(() =>
      expect(
        view.container.querySelector("[data-issue-body]"),
        "the stage drew no outline on Conversation"
      ).not.toBeNull()
    );
    await waitInAct(20);
    expect(
      waitingCounts(view),
      "on Conversation, the header's count and the Decisions control's flag count the answer the Keep's drop took"
    ).toMatchObject(waitingFor(1));
  } finally {
    cleanup();
    keep.mockRestore();
    for (const read of headerReads) read.mockRestore();
    restoreMatchMedia();
  }
});

// The answer a drop keeps is the one the read it starts replaces (`dropViews`). On Conversation,
// with this load's decisions view answer held, a Keep of the later decision drops the views, and
// the decisions view, which no filter reads there, is read no more: the drop keeps nothing, and
// Conversation's read answers again with the later decision kept. The reader then opens the
// Decisions view, whose read is out: the view draws the later decision as Conversation's read
// has it, kept, and never as the answer before the Keep had it, waiting with Keep.
//
// The same story with another write of the issue out as the Keep settles - a comment the reader
// posts, which the write store tells the page's subscription of as it goes out
// (`subscribeTreeFreshness`), so it holds the issue's views (`heldByWrites`) and the Keep's drop
// owes the decisions view a read at that write's settle (`droppedWhileHeld`). On Conversation
// nothing reads that view, so the settle only marks it stale, and the drop keeps nothing
// (`dropViews`: only the Decisions filter's own read keeps). The other write settles,
// Conversation's read answers again with the later decision kept, and the reader opens the
// Decisions view, whose read is out: it draws the later decision as Conversation's read has it,
// and never as the answer before the Keep had it, waiting with Keep.
test.each([
  [
    "the Decisions view opened after a Keep on Conversation draws no decision as it stood before the Keep while its read is out",
    false,
  ],
  [
    "the Decisions view opened after a Keep on Conversation made while another write of the issue was out draws no decision as it stood before the Keep while its read is out",
    true,
  ],
] as const)("%s", async (_title, otherWrite) => {
  coverS1();
  const headerReads = answerHeaderReads();
  let kept = false;
  const keep = spyOn(api, "keepDecision").mockImplementation(async () => {
    kept = true;
    return KEPT_LATER;
  });
  let viewReads = 0;
  spyOn(api, "getIssueTree").mockImplementation((_key, query) => {
    if (query?.view !== "decisions") {
      return Promise.resolve(treeOf([S1, tied("first"), kept ? keptLater() : tied("later")]));
    }
    viewReads += 1;
    return Promise.withResolvers<TreeAnswer>().promise;
  });
  const later = decisionNode("later").id;
  const held: { navigate?: NavigateFunction } = {};
  const user = userEvent.setup();
  // What the failure messages add where the Keep was made while another write was out.
  const made = otherWrite ? " made while another write was out" : "";
  try {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const view = drawStage("", { client, held, page: true, preset: "conversation" });
    const keepOnConversation = await keepOf(later, "[data-issue-body]");
    // This load's answer of the decisions view, as the page holds it once the Decisions view was
    // on earlier in the load.
    await act(async () => {
      client.setQueryData(viewKey(KEY, "decisions"), decisionsView([tied("first"), tied("later")]));
    });
    // The other write of the issue, where the case sends one, sent through the page's write store
    // as the page sends a comment, and answered when the case says.
    const otherAnswer = Promise.withResolvers<void>();
    let otherSettled: Promise<unknown> = Promise.resolve();
    if (otherWrite) {
      act(() => {
        otherSettled = writesOf(client).run({
          detail: null,
          inline: true,
          issue: KEY,
          kind: plainWrite,
          label: "A comment",
          method: "POST",
          presentation: "indicator",
          write: () => otherAnswer.promise,
        });
      });
      expect(
        writesOf(client).writesOutOf(KEY),
        "the other write of the issue is not out"
      ).toHaveLength(1);
    }
    await user.click(keepOnConversation);
    await waitFor(() => expect(keep, "the Keep was never sent").toHaveBeenCalledTimes(1));
    if (otherWrite) {
      await waitInAct(20);
      await act(async () => {
        otherAnswer.resolve();
        await otherSettled;
      });
    }
    await waitFor(() =>
      expect(
        view.container.querySelector(
          `[data-issue-body] [data-node-row][data-node-id="${later}"] ${rowActionSelector("keep")}`
        ),
        "Conversation's read did not answer again with the later decision kept"
      ).toBeNull()
    );

    act(() => held.navigate?.(presetPath(KEY, "decisions")));
    const row = () =>
      view.container.querySelector<HTMLElement>(
        `[data-decisions-body] [data-node-row][data-node-id="${later}"]`
      );
    await waitFor(() =>
      expect(row(), "the Decisions view drew no row of the later decision").not.toBeNull()
    );
    await waitFor(() =>
      expect(viewReads, "opening the Decisions view did not read it").toBeGreaterThan(0)
    );
    await waitInAct(20);
    expect(
      row()?.getAttribute("data-node-state"),
      `while its read is out, the Decisions view draws the later decision waiting, as it stood before the Keep${made}`
    ).not.toBe("waiting_review");
    expect(
      row()?.querySelector(rowActionSelector("keep")),
      `while its read is out, the Decisions view draws Keep on the later decision, as before the Keep${made}`
    ).toBeNull();
  } finally {
    cleanup();
    keep.mockRestore();
    for (const read of headerReads) read.mockRestore();
  }
});

/** Another write of issue `KEY`, sent through the page's write store as the page sends a
 *  comment, so the page's subscription holds the issue's views while it is out
 *  (`subscribeTreeFreshness`), and held out until `settle`, which answers it and waits for the
 *  store to settle it. */
function otherWriteOut(client: QueryClient): { readonly settle: () => Promise<void> } {
  const answer = Promise.withResolvers<void>();
  let settled: Promise<unknown> = Promise.resolve();
  act(() => {
    settled = writesOf(client).run({
      detail: null,
      inline: true,
      issue: KEY,
      kind: plainWrite,
      label: "A comment",
      method: "POST",
      presentation: "indicator",
      write: () => answer.promise,
    });
  });
  expect(writesOf(client).writesOutOf(KEY), "the other write of the issue is not out").toHaveLength(
    1
  );
  return {
    settle: () =>
      act(async () => {
        answer.resolve();
        await settled;
      }),
  };
}

// R4 with another write of the issue out as a Keep on the Decisions view settles: that write
// holds the issue's views (`heldByWrites`), so the Keep's drop sends the decisions view no read
// and owes it one at that write's settle (`droppedWhileHeld`). The Decisions filter's own read
// is mounted, so the drop keeps the answer it took (`dropViews`) and the view draws it while the
// read is owed and while it is out: the later decision, which of the reads only the decisions
// view holds, keeps its row. The other write settles, the owed read is sent, and its answer draws
// the later decision kept.
test("on the Decisions view, a Keep that settles while another write of the issue is out keeps the row of a decision only the decisions view read holds, until the read the write owes lands", async () => {
  const keep = spyOn(api, "keepDecision").mockResolvedValue(KEPT_LATER);
  // The decisions view read answers once, and the read the other write's settle owes it is held.
  const owed = Promise.withResolvers<TreeAnswer>();
  let viewReads = 0;
  spyOn(api, "getIssueTree").mockImplementation((_key, query) => {
    if (query?.view !== "decisions") return Promise.resolve(treeOf([S1, tied("first")]));
    viewReads += 1;
    return viewReads === 1
      ? Promise.resolve(decisionsView([tied("first"), tied("later")]))
      : owed.promise;
  });
  const later = decisionNode("later").id;
  const row = () =>
    document.querySelector<HTMLElement>(
      `[data-decisions-body] [data-node-row][data-node-id="${later}"]`
    );
  const user = userEvent.setup();
  try {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    drawStage("", { client });
    const keepLater = await keepOf(later);
    const other = otherWriteOut(client);
    await user.click(keepLater);
    await waitFor(() => expect(keep, "the Keep was never sent").toHaveBeenCalledTimes(1));
    await waitFor(() =>
      expect(
        client.getQueryData(viewKey(KEY, "decisions")),
        "the Keep's settle did not drop the decisions view"
      ).toBeUndefined()
    );
    await waitInAct(20);
    expect(viewReads, "the decisions view was read while the other write is out").toBe(1);
    expect(
      row(),
      "the later decision's row left the view while the other write is out"
    ).not.toBeNull();

    await other.settle();
    await waitFor(() =>
      expect(viewReads, "the other write's settle sent no decisions view read").toBeGreaterThan(1)
    );
    await waitInAct(20);
    expect(
      row(),
      "the later decision's row left the view while the read the other write owes is out"
    ).not.toBeNull();
    await act(async () => owed.resolve(decisionsView([tied("first"), keptLater()])));
    await waitFor(() =>
      expect(
        row()?.querySelector(rowActionSelector("keep")),
        "once the owed read landed, the view draws Keep on the later decision still"
      ).toBeNull()
    );
    expect(
      row(),
      "once the owed read landed, the view draws no row of the later decision"
    ).not.toBeNull();
    expect(
      row()?.getAttribute("data-node-state"),
      "once the owed read landed, the view draws the later decision waiting still"
    ).not.toBe("waiting_review");
  } finally {
    cleanup();
    keep.mockRestore();
  }
});

// A press of the next-page control that reads nothing (`readNextTreePage`) arms no landing for a
// row drawn later. A Keep of the later decision, refused, settles while another write of the
// issue is out: its drop resets the decisions view and owes it a read at that write's settle, and
// the Decisions view draws the answer the drop took, which names a next page, with its control.
// A press of it then reads nothing - the cache holds no answer of the view, and no read of it is
// out - and the focus stays on the refused decision's row. The other write settles, and the owed
// read answers a decision no answer held before: its row is drawn, and the focus stays put.
test("a press of the next-page control that reads nothing leaves no landing armed", async () => {
  const keep = spyOn(api, "keepDecision").mockRejectedValue(
    new ApiError(409, {
      code: "DECISION_CHANGED",
      error: "The decision changed since you read it.",
    })
  );
  const pages: string[] = [];
  let viewReads = 0;
  spyOn(api, "getIssueTree").mockImplementation((_key, query) => {
    if (query?.view !== "decisions") return Promise.resolve(treeOf([S1, tied("first")]));
    if (query.cursor !== undefined) {
      pages.push(query.cursor);
      return Promise.withResolvers<TreeAnswer>().promise;
    }
    viewReads += 1;
    const added = viewReads === 1 ? [] : [tied("fourth")];
    return Promise.resolve(
      pagedAnswer(decisionsView([tied("first"), tied("later"), ...added]), "page-2")
    );
  });
  const later = decisionNode("later").id;
  const fourth = `[data-decisions-body] [data-node-row][data-node-id="${decisionNode("fourth").id}"]`;
  const user = userEvent.setup();
  try {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const view = drawStage("", { client });
    const keepLater = await keepOf(later);
    const other = otherWriteOut(client);
    await user.click(keepLater);
    await waitFor(() => expect(keep, "the Keep was never sent").toHaveBeenCalledTimes(1));
    await waitFor(() =>
      expect(
        client.getQueryData(viewKey(KEY, "decisions")),
        "the Keep's settle did not drop the decisions view"
      ).toBeUndefined()
    );
    await waitInAct(20);
    expect(focusedRow(), "the focus left the refused decision's row").toBe(later);
    const press = view.container.querySelector<HTMLElement>("[data-tree-more] button");
    expect(press, "the answer the Keep's drop took drew no next-page control").not.toBeNull();
    if (press != null) await user.click(press);
    await waitInAct(20);
    expect(pages, "the press read a page").toEqual([]);
    expect(viewReads, "the press read the decisions view").toBe(1);

    await other.settle();
    await waitFor(() =>
      expect(
        document.querySelector(fourth),
        "the read the other write owes drew no row of the decision it adds"
      ).not.toBeNull()
    );
    await waitInAct(20);
    expect(focusedRow(), "a press that read nothing landed on a row drawn after it").toBe(later);
  } finally {
    cleanup();
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
    cleanup();
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
    cleanup();
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
    cleanup();
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
    cleanup();
  }
});

/** The Decisions view's row of the ask `id`. */
function askRow(id: string): string {
  return `[data-decisions-body] [data-node-row][data-node-id="${id}"]`;
}

// A key pressed on a row of the view is an act of the following too, but the row that holds the
// focus leaves the page with the view where the view then draws no node the address names. The
// outline drawn in the view's place lands as it does where the reader pressed nothing: with the
// messages read out the view draws the questions, `j` is pressed on the first one's row, and once
// the messages answer, the outline's line that the issue holds no such comment takes the focus.
test("`j` on a row of the Decisions view, before it draws the outline's line for a comment the issue holds none of in its place, leaves the focus on that line", async () => {
  spyOn(api, "listIssueAskThreads").mockResolvedValue([...ASKED] as never);
  answerWriteUpTree();
  const messagesOut = Promise.withResolvers<MessageRead[]>();
  spyOn(api, "listIssueMessages").mockReturnValue(messagesOut.promise);
  const read = commentGone();
  try {
    const view = drawStage(`?comment=${GONE_COMMENT}`);
    await rowTakesFocus(view, askRow("ask-open"), "the Decisions view with the messages out");
    act(() => {
      document.activeElement?.dispatchEvent(
        new KeyboardEvent("keydown", { bubbles: true, key: "j" })
      );
    });
    expect(
      view.container.querySelector(askRow("ask-open")),
      `the focus left the first question's row at \`j\` for ${focusedRow() ?? document.activeElement?.tagName}`
    ).toHaveFocus();
    await act(async () => messagesOut.resolve([]));
    await goneLineTakesFocus(view, "the messages answering after `j`");
  } finally {
    cleanup();
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
    cleanup();
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
  client.setQueryData(["issue-messages", KEY], [], answered);
  // What the reader holds unread on the issue, which a thread's read mark reads.
  client.setQueryData(myRepliesOwnerQuery({ issue: KEY }).queryKey, NOTHING_UNREAD, answered);
  // The tree read: the issue's two threads, each comment a node of it, so the outline draws rows.
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
  view.unmount();
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
  view.unmount();
});

// A read the view draws no line of its own for, refused, is no row of the view. A comment the
// tree read does not hold is read on its own where an address names it (`GET
// /api/v1/comments/{id}`, `useAddressFallback`): with that read refused, a comment named beside
// the Decisions view lands on nothing there, so the stage draws the outline in the view's place,
// which says the comment could not be loaded, with the focus on that line's heading.
test("the comment's own read refused, a comment named beside the Decisions view draws the outline in its place, with the comment's line and the focus on it", async () => {
  const read = spyOn(api, "getComment").mockRejectedValue(REFUSED);
  try {
    const view = drawStage("?comment=reply-b");
    await waitFor(() =>
      expect(view.getByRole("heading", { name: "Couldn't load this comment" })).toHaveFocus()
    );
    expect(read.mock.calls.map(([id]) => id)).toContain("reply-b");
    expect(
      view.container.querySelector("[data-decisions-body]"),
      "the Decisions view was drawn beside the outline"
    ).toBeNull();
  } finally {
    cleanup();
    read.mockRestore();
  }
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
  view.unmount();
});

/** The decisions of the older-Change cases (decision 563 (4)): each waits for review and is tied
 *  to the statement `s1`. The first last changed before the oldest comment Conversation's first
 *  page holds, so a Change of it may stand on the older page that answer names; the second
 *  changed after it, so none of its Changes can. */
const OLDER_DECISION = decision("older", {
  decision: { changed_at: "2026-11-01T00:00:00Z", links: [shaped(specRef("s1"))] },
  role: "query",
});
const NEWER_DECISION = decision("newer", {
  decision: { changed_at: "2026-11-06T00:00:00Z", links: [shaped(specRef("s1"))] },
  role: "query",
});

/** The reader's Change of the first decision, made after its words last changed and before
 *  every comment the first page holds. */
const OLDER_CHANGE: Comment = {
  ...readComment("change-older", 0),
  body: "Tie it to the plan instead.",
  created_at: "2026-11-03T00:00:00Z",
};

/** Conversation's first page for those cases: the statement, the two decisions and the two
 *  threads, and the position of its oldest comment, as older comments remain. */
const OLDER_FIRST: TreeAnswer = {
  ...treeOf([
    treeNode("s1", "requirement", KEY, { ...inDocument(SPEC_DOCUMENT), text: "The statement." }),
    OLDER_DECISION,
    NEWER_DECISION,
    ...commentNodes(TWO_THREADS),
  ]),
  comments_before: `${TWO_THREADS[0]?.created_at},${TWO_THREADS[0]?.id}`,
};

/** The older page: the Change, under the decision's node as the read hangs it, and no older
 *  comment left. */
const OLDER_PAGE: TreeAnswer = {
  ...treeAnswer([commentNode(OLDER_CHANGE, `${KEY}/${SPEC_DOCUMENT.slug}#older`)]),
  comments_before: null,
};

/** Holds the older page of one read out while the other cannot supply the Change. */
function answerOlderChange(page: Promise<TreeAnswer>, fromView: boolean): () => number {
  const tree = spyOn(api, "getIssueTree").mockImplementation(async (_key, query = {}) => {
    if (query.comments_before !== undefined) return page;
    if (query.view === "decisions") {
      return fromView
        ? { ...OLDER_FIRST, view: "decisions" }
        : Promise.withResolvers<never>().promise;
    }
    return fromView
      ? {
          ...OLDER_FIRST,
          comments_before: null,
          nodes: [...(OLDER_FIRST.nodes ?? []), ...(OLDER_PAGE.nodes ?? [])],
        }
      : OLDER_FIRST;
  });
  return () => {
    const requests = tree.mock.calls.flatMap(([, query]) =>
      query?.comments_before === undefined ? [] : [query]
    );
    for (const query of requests) {
      expect(query.view, "the older page came from the wrong read").toBe(
        fromView ? "decisions" : undefined
      );
    }
    return requests.length;
  };
}

// Decision 563 (4), as INFRA-1196's architect ruled it at 11:23Z (choice (b)): a decision whose
// Change may stand on a comment page its read has not loaded stays in the review list,
// whether that read is the decisions view or Conversation's fallback,
// drawn with the outline's mark ("+" after its state), and the list's count leaves it out until
// that page is read. The view reads it once the page has drawn, and the decision then carries the
// Change the page holds, which it waits on its agent for: it leaves the list then, and only then.
test.each([
  false,
  true,
])("a decision whose Change stands on an older comment page stays marked in the review list and out of its count until the page is read (decisions view: %s)", async (fromView) => {
  const page = Promise.withResolvers<TreeAnswer>();
  const pagesSent = answerOlderChange(page.promise, fromView);
  const view = drawStage("");
  const rowOf = (block: string) =>
    view.container.querySelector<HTMLElement>(
      `[data-decisions-body] [data-node-row][data-node-id="${decisionNode(block).id}"]`
    );
  const review = () => view.container.querySelector<HTMLElement>('[data-decisions-part="review"]');
  const reviewRows = () =>
    [...(review()?.querySelectorAll<HTMLElement>("[data-node-row]") ?? [])].map(
      (row) => row.dataset.nodeId
    );
  const reviewCount = () => review()?.querySelector("[data-decisions-count]")?.textContent;
  await waitFor(() =>
    expect(
      rowOf("older")?.querySelector("[data-change-unread]")?.textContent,
      `the decision is not drawn with the outline's mark; the stage drew ${drawnSlot(view)}`
    ).toBe("+")
  );
  expect(reviewRows(), "the marked decision left the review list before its page answered").toEqual(
    [decisionNode("newer").id, decisionNode("older").id]
  );
  expect(
    reviewCount(),
    "the review list's count counts the marked decision before its older page answers"
  ).toBe("1");
  await act(async () => page.resolve(OLDER_PAGE));
  await waitFor(() =>
    expect(rowOf("older")?.dataset.nodeState, "the decision does not carry its Change").toBe(
      "change_requested"
    )
  );
  await waitFor(() =>
    expect(rowOf("older")?.querySelector("[data-change-words]")?.textContent).toBe(
      OLDER_CHANGE.body
    )
  );
  expect(rowOf("older")?.querySelector("[data-change-unread]"), "the mark stayed").toBeNull();
  expect(reviewRows(), "the review list once the Change is held").toEqual([
    decisionNode("newer").id,
  ]);
  expect(reviewCount()).toBe("1");
  expect(pagesSent(), "the older page was read other than once").toBe(1);
});

// Decision 563 (4) under decision 538: the older comment pages are read only once the page has
// drawn, so a load reads none. With the reads that mark the decision held as the page mounts, no
// older page is asked for as it mounts, and one is in a frame after.
test("no older comment page is read before the page's first drawing, and one is read after it", async () => {
  const pagesSent = answerOlderChange(Promise.withResolvers<TreeAnswer>().promise, false);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(outlineQuery(KEY, "conversation").queryKey, OLDER_FIRST);
  drawStage("", { client });
  expect(pagesSent(), "an older comment page was read before the page's first drawing").toBe(0);
  await waitFor(() =>
    expect(pagesSent(), "no older comment page was read once the page had drawn").toBe(1)
  );
});
