import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, type RenderResult, render, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter, type NavigateFunction } from "react-router-dom";

import { ApiError, api } from "../../../api/client";
import {
  artifactBlocksQuery,
  issueCoverageQuery,
  issueResultsQuery,
  myRepliesOwnerQuery,
  referenceTargetsQuery,
  whoAmIQuery,
} from "../../../api/queries";
import type { ArtifactBlock, Comment, IssueAskRow, IssueDetails } from "../../../api/types";
import { type EventFreshness, useIsHuman } from "../../../hooks/useIsHuman";
import { issueDecisionsQuery } from "../../doc/spec-marks";
import { answeredAsk } from "../../inbox/ask-card-fixture";
import { MarginProvider } from "../../margin/margin-context";
import { EVENT_FRESH } from "../../refs/reference-target";
import { buildDispatchReference } from "../../refs/routes";
import { KeymapProvider } from "../../shell/KeymapProvider";
import { useMarkThreadRead } from "../../threads/useMarkThreadRead";
import { presetPath } from "../issue-address";
import { framesByHand, TWO_THREADS, waitInAct } from "../outline-fixtures";
import { type ThreadMark, ThreadReadMark } from "../outline-thread-read";
import { EMPTY_STATE_LINES, type Preset, STATE_LINES } from "../page-words";
import { FAILED_READ_RETRY } from "../slots/failed-read-line";
import { AddressedStage, drawAtBrowserEntry, drawnSlot } from "../slots/stage-fixtures";
import { blockNodeId } from "../tree-drafts";
import { issueAsksQuery } from "../useIssueTree";
import { check, VERIFICATION_SLUG } from "../verification-filter/verification-fixtures";
import {
  artifact,
  decision,
  decisionNode,
  decisionsRead,
  issueDetails,
  KEY,
  READER,
  SPEC_SLUG,
  specRef,
} from "./decisions-fixtures";

/** Every read the page's tree is built from, answered with nothing, and who is reading. */
function stubReads(): { mockRestore(): void }[] {
  return [
    spyOn(api, "getIssueCoverage").mockResolvedValue({
      gaps: [],
      has_plan: false,
      issue: KEY,
      requirements: [],
      spec_artifact_id: "",
    }),
    spyOn(api, "getIssueDecisions").mockResolvedValue({
      decisions: [],
      document: null,
      open_questions: [],
    }),
    spyOn(api, "getIssueResults").mockResolvedValue({
      checks: [],
      issue: KEY,
      verification_artifact_id: "",
    }),
    spyOn(api, "getIssueReferences").mockResolvedValue({ members: [], truncated: false }),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "getArtifactBlocks").mockResolvedValue([]),
    spyOn(api, "whoAmI").mockResolvedValue({ event_head: "0", kind: "user", login: "alice" }),
    spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] }),
  ];
}

let stubs: { mockRestore(): void }[] = [];
beforeEach(() => {
  stubs = stubReads();
});
afterEach(() => {
  cleanup();
  for (const stub of stubs) stub.mockRestore();
});

/** The page's stage at the issue's address of `preset`, Decisions where none is given, with
 *  `after` after it, a search or a fragment, under the page's own providers, as the page's root
 *  draws it (`IssueOutlinePage.tsx`): the stage's own views, each slot filled as `SLOT_FILLED`
 *  has it. The issue is `issueDetails()` and the client a fresh one where the case gives none,
 *  and `held.navigate` moves the router once the stage is drawn (`AddressedStage`). */
function drawStage(
  after: string,
  {
    client = new QueryClient({ defaultOptions: { queries: { retry: false } } }),
    held = {},
    issue = issueDetails(),
    preset = "decisions",
  }: {
    client?: QueryClient;
    held?: { navigate?: NavigateFunction };
    issue?: IssueDetails;
    preset?: Preset;
  } = {}
): RenderResult {
  return render(
    <MemoryRouter initialEntries={[`${presetPath(KEY, preset)}${after}`]}>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <MarginProvider>
            <AddressedStage held={held} issue={issue} />
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** The line the body draws once the reads that say which entries there are have answered. */
async function emptyLineOf(view: RenderResult): Promise<string | null | undefined> {
  await waitFor(() =>
    expect(view.container.querySelector("[data-decisions-body] p")).not.toBeNull()
  );
  return view.container.querySelector("[data-decisions-body] p")?.textContent;
}

/** The node id of the row that holds the focus, or undefined where no row does. */
function focusedRow(): string | undefined {
  return document.activeElement?.closest<HTMLElement>("[data-node-row]")?.dataset.nodeId;
}

/** Waits for the row `selector` finds in `view` to hold the focus, saying what `what` drew
 *  where it does not. */
async function rowTakesFocus(view: RenderResult, selector: string, what: string): Promise<void> {
  await waitFor(() => {
    const row = view.container.querySelector(selector);
    expect(row, `${what} drew no such row; the stage drew ${drawnSlot(view)}`).not.toBeNull();
    expect(row, `${what} left the focus on ${focusedRow()}`).toHaveFocus();
  });
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

/** An id that names no comment of the issue, which the comment's own read answers 404 for. */
const GONE_COMMENT = "5f0c2a9e-7d41-4b8a-9c3e-2e6d1b7a4c90";

// An address beside the Decisions view names a node of the outline. Where the view's tree holds
// none, the view lands on nothing, and the stage draws the outline's fallback in its place, as it
// did before the view was filled: for an id no comment holds, the comment's own read finds none,
// and the page says so where the outline would be, with the focus on that line.
test("a comment the issue holds none of, named beside the Decisions view, draws the outline's line that says so, with the focus on it", async () => {
  const read = spyOn(api, "getComment").mockRejectedValue(
    new ApiError(404, { code: "NOT_FOUND", error: "no such comment" })
  );
  try {
    const view = drawStage(`?comment=${GONE_COMMENT}`);
    const heading = await waitFor(() => view.getByRole("heading", { name: "Comment not found" }), {
      onTimeout: () =>
        new Error(
          `the address of a comment the issue holds none of drew no line saying so; the stage drew ${drawnSlot(view)}, with the focus on ${document.activeElement?.tagName}`
        ),
    });
    expect(read).toHaveBeenCalledWith(GONE_COMMENT);
    await waitFor(() =>
      expect(document.activeElement, "the line that names no comment does not hold the focus").toBe(
        heading
      )
    );
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

/** The decisions read: the reader's write-up of their answer to `ask-answered`, a section the
 *  server marks a person's because it names that ask. */
const WRITE_UP = decisionsRead([
  decision("writeup", { asks: ["ask-answered"], kind: "person", waiting_review: false }),
]);

/** The write-up's node, which the view draws no row for once the asks read tells it. */
const WRITE_UP_NODE = decisionNode("writeup").id;

// A person's decision whose section names a question of this issue they answered is their
// write-up of that answer, which the view draws no row for: the question's own row is the
// decision. An address naming the write-up keeps the view and lands on the row of the question
// it answers, though a question still open stands first in the list and the stage would give its
// row the focus.
test("a person's write-up of an answered question, named on the Decisions view's address, keeps the view, with the focus on the question's row", async () => {
  const asks = spyOn(api, "listIssueAsks").mockResolvedValue([...ASKED]);
  const decisions = spyOn(api, "getIssueDecisions").mockResolvedValue(WRITE_UP);
  try {
    const view = drawStage("#b-writeup");
    await waitFor(() =>
      expect(
        focusedRow(),
        `the write-up's address left the focus elsewhere; the stage drew ${drawnSlot(view)}`
      ).toBe("ask-answered")
    );
    await waitInAct(20);
    expect(drawnSlot(view), "the stage drew the outline in place of the view").toBe("the view");
    expect(focusedRow(), "the focus left the question's row").toBe("ask-answered");
    expect(
      view.container.querySelector(`[data-node-id="${WRITE_UP_NODE}"]`),
      "the view drew the write-up as a row"
    ).toBeNull();
  } finally {
    asks.mockRestore();
    decisions.mockRestore();
  }
});

// Until the asks read answers, nothing tells the write-up from a decision of the person's own, so
// the list draws it as one, under the reader's decisions. The address waits for the asks rather
// than land on that row, which leaves the list once they answer and would take the focus with it.
test("a write-up's address waits for the asks read, which answers after the decisions read, and lands on the question's row", async () => {
  const asksOut = Promise.withResolvers<IssueAskRow[]>();
  const asks = spyOn(api, "listIssueAsks").mockReturnValue(asksOut.promise);
  const decisions = spyOn(api, "getIssueDecisions").mockResolvedValue(WRITE_UP);
  try {
    const view = drawStage("#b-writeup");
    await waitFor(() =>
      expect(
        view.container.querySelector(`[data-node-id="${WRITE_UP_NODE}"]`),
        "the write-up was drawn as no decision of its own while the asks read is out"
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
    decisions.mockRestore();
  }
});

// Under words the answered question does not hold, the list draws neither the write-up, which is
// no row here, nor the question it answers, only the open question the words match, so the
// write-up's address has no row of the view to land on. The stage draws the outline in the view's
// place, which draws the write-up as a row under the question it answers and lands on it.
test("a write-up's address under words its question does not hold draws the outline, with the focus on the write-up's row", async () => {
  const asks = spyOn(api, "listIssueAsks").mockResolvedValue([...ASKED]);
  const decisions = spyOn(api, "getIssueDecisions").mockResolvedValue(WRITE_UP);
  try {
    const view = drawStage("?q=flag#b-writeup");
    await waitFor(() =>
      expect(
        focusedRow(),
        `the write-up's address left the focus on ${document.activeElement?.tagName}; the stage drew ${drawnSlot(view)}`
      ).toBe(WRITE_UP_NODE)
    );
    await waitInAct(20);
    expect(drawnSlot(view), "the stage drew the view, which has no row for the write-up").toBe(
      "the body"
    );
    expect(focusedRow(), "the focus left the write-up's row").toBe(WRITE_UP_NODE);
  } finally {
    asks.mockRestore();
    decisions.mockRestore();
  }
});

/** The outline's row of the write-up, drawn in the Decisions view's place at the write-up's
 *  address under words its question does not hold: the decisions read is held until the view is
 *  drawn, `press` is done then where it is given, and once the outline draws the row every frame
 *  asked for till then is run (`frames`). */
async function writeUpRowAfter(
  frames: { readonly run: () => Promise<void> },
  press?: () => void
): Promise<HTMLElement> {
  const decisionsOut = Promise.withResolvers<typeof WRITE_UP>();
  spyOn(api, "getIssueDecisions").mockReturnValue(decisionsOut.promise);
  const view = drawStage("?q=flag#b-writeup");
  await waitFor(() =>
    expect(drawnSlot(view), "the stage drew no view while the decisions read is out").toBe(
      "the view"
    )
  );
  if (press !== undefined) act(press);
  await act(async () => decisionsOut.resolve(WRITE_UP));
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
// whichever slot it draws. With the decisions read out the view stays drawn, and a press then on
// a control outside the stage is an act of that following: once the read answers, the outline
// drawn in the view's place leaves the focus on the control. With no act, the same address lands
// on the outline's row of the write-up.
test("a press while the view is drawn keeps the focus on the control pressed, where the outline drawn in the view's place lands on its row with no act", async () => {
  spyOn(api, "listIssueAsks").mockResolvedValue([...ASKED]);
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
  client.setQueryData(issueDecisionsQuery(KEY, true).queryKey, decisionsRead([]), answered);
  client.setQueryData(issueAsksQuery(KEY).queryKey, [], answered);
  // The issue holds two threads, so the outline draws rows.
  client.setQueryData(["comments", KEY], [...TWO_THREADS], answered);
  client.setQueryData(["issue-messages", KEY], [], answered);
  // What the reader holds unread on the issue, which a thread's read mark reads.
  client.setQueryData(myRepliesOwnerQuery({ issue: KEY }).queryKey, NOTHING_UNREAD, answered);
  // The blocks of each of the issue's documents, the spec and the decisions document.
  for (const { id } of issue.artifacts ?? []) {
    client.setQueryData(artifactBlocksQuery(id).queryKey, [], answered);
  }
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

/** The plan and the Verification document of the issue the Verification cases draw, beside its
 *  spec and its decisions document. */
const PLAN_DOCUMENT = artifact("artifact-plan", "plan-md");
const VERIFICATION_DOCUMENT = artifact("artifact-verification", VERIFICATION_SLUG);

/** The issue with its plan and its Verification document too, each in its role. */
function verifiedIssue(): IssueDetails {
  const issue = issueDetails();
  return {
    ...issue,
    artifacts: [...(issue.artifacts ?? []), PLAN_DOCUMENT, VERIFICATION_DOCUMENT],
    plan_artifact_id: PLAN_DOCUMENT.id,
    verification_artifact_id: VERIFICATION_DOCUMENT.id,
  };
}

/** The block ids of the spec's one statement, and of the plan task of the plan and the check of
 *  the Verification document that each serve it. */
const STATEMENT = "r1";
const PLAN_TASK = "t1";
const CHECK = "c1";

/** The task's step in the Plan view's chart, the task's row of the outline, and the statement's
 *  row of the outline. */
const TASK_STEP = `[data-plan-body] [data-node-row][data-node-id="${blockNodeId(PLAN_DOCUMENT.slug, PLAN_TASK)}"]`;
const TASK_ROW = `[data-issue-body] [data-node-row][data-node-id="${blockNodeId(PLAN_DOCUMENT.slug, PLAN_TASK)}"]`;
const STATEMENT_ROW = `[data-issue-body] [data-node-row][data-node-id="${blockNodeId(SPEC_SLUG, STATEMENT)}"]`;

/** One typed block of a document as the blocks read answers it, with the statement it serves
 *  (`serves`, a `block[]` pointer) where it is given one. */
function typedBlock(id: string, type: string, serves?: string): ArtifactBlock {
  return {
    attributes: serves === undefined ? [] : [{ kind: "block[]", name: "serves", value: serves }],
    from: 0,
    id,
    references: { asks: 0, comments: 0 },
    to: 1,
    token: id,
    type,
  };
}

/** Each document's blocks, by its id: the spec's statement, and the plan's task and the
 *  Verification document's check, whose `serves` places each under that statement
 *  (`tree-drafts.ts`). */
const BLOCKS: Readonly<Record<string, readonly ArtifactBlock[]>> = {
  "artifact-spec": [typedBlock(STATEMENT, "requirement")],
  [PLAN_DOCUMENT.id]: [typedBlock(PLAN_TASK, "plan_task", specRef(STATEMENT))],
  [VERIFICATION_DOCUMENT.id]: [typedBlock(CHECK, "check", specRef(STATEMENT))],
};

/** Answers the reads the tree and the Verification view are drawn from for `verifiedIssue`: the
 *  coverage read with the statement, each document's blocks, the results read with the check,
 *  and the issue's two threads. Each spy is the one `stubReads` set, which `spyOn` hands back,
 *  so the file's `afterEach` restores it. */
function answerVerificationReads(): void {
  spyOn(api, "getIssueCoverage").mockResolvedValue({
    gaps: [],
    has_plan: true,
    issue: KEY,
    requirements: [
      {
        block_id: STATEMENT,
        checks: 1,
        gaps: [],
        plan_tasks: 1,
        ref: specRef(STATEMENT),
        text: "The page draws the plan.",
      },
    ],
    spec_artifact_id: "artifact-spec",
  });
  spyOn(api, "getArtifactBlocks").mockImplementation(async (id: string) => [...(BLOCKS[id] ?? [])]);
  spyOn(api, "getIssueResults").mockResolvedValue({
    checks: [check({ block_id: CHECK })],
    issue: KEY,
    verification_artifact_id: VERIFICATION_DOCUMENT.id,
  });
  spyOn(api, "listComments").mockResolvedValue([...TWO_THREADS]);
}

// Under a statement the Verification view draws that statement's checks alone
// (`verificationReading`), so a plan task that serves the statement, which the tree places under
// it, is no row of the view, though the view keeps it as the address's node. The view tells the
// stage so, and the stage draws the outline in the view's place, which lands on the task's row.
test("a plan task under a statement, named on the Verification view's address, draws the outline in the view's place, with the focus on the task's row", async () => {
  answerVerificationReads();
  const task = blockNodeId(PLAN_DOCUMENT.slug, PLAN_TASK);
  const view = drawStage(`#b-${PLAN_TASK}`, { issue: verifiedIssue(), preset: "verification" });
  await waitFor(() =>
    expect(
      focusedRow(),
      `the plan task's address left the focus on ${focusedRow() ?? document.activeElement?.tagName}; the stage drew ${drawnSlot(view)}`
    ).toBe(task)
  );
  // Every read has answered, and the view has said whether it draws the task.
  await waitInAct(20);
  expect(drawnSlot(view), "the stage drew the view, which has no row for the plan task").toBe(
    "the body"
  );
  expect(
    view.container.querySelector("[data-verification-body]"),
    "the Verification view was drawn beside the outline"
  ).toBeNull();
  expect(focusedRow(), "the focus left the plan task's row").toBe(task);
});

// A node the Verification view draws a row of keeps the view: a comment of the issue, which is
// of no kind the view draws, as a row of its own after the statements, and a check as a row
// under the statement it serves.
test("a comment of the issue or a check, named on the Verification view's address, keeps the view, which draws it as a row", async () => {
  answerVerificationReads();
  for (const [after, row] of [
    ["?comment=head-b", "head-b"],
    [`#b-${CHECK}`, blockNodeId(VERIFICATION_SLUG, CHECK)],
  ] as const) {
    const view = drawStage(after, { issue: verifiedIssue(), preset: "verification" });
    await waitFor(() =>
      expect(
        view.container.querySelector(`[data-verification-body] [data-node-id="${row}"]`),
        `the Verification view drew no row for ${after}; the stage drew ${drawnSlot(view)}`
      ).not.toBeNull()
    );
    // Every read has answered, and the view has said whether it draws the node.
    await waitInAct(20);
    expect(drawnSlot(view), `the stage drew the outline in place of the view for ${after}`).toBe(
      "the view"
    );
    cleanup();
  }
});

// The Plan view draws a row for a task alone, so a comment named beside it has no row of the view
// to land on. Once every read is held the view tells the stage so, and the stage draws the outline
// in the view's place: for an id no comment holds, the comment's own read finds none, and the
// outline's line says so, with the focus on it, where the view had drawn its step.
test("a comment the issue holds none of, named on the Plan view's address, draws the outline's line that says so in the view's place, with the focus on it", async () => {
  answerVerificationReads();
  const read = spyOn(api, "getComment").mockRejectedValue(
    new ApiError(404, { code: "NOT_FOUND", error: "no such comment" })
  );
  try {
    const view = drawStage(`?comment=${GONE_COMMENT}`, { issue: verifiedIssue(), preset: "plan" });
    const heading = await waitFor(() => view.getByRole("heading", { name: "Comment not found" }), {
      onTimeout: () =>
        new Error(
          `the address of a comment the issue holds none of drew no line saying so under Plan; the stage drew ${drawnSlot(view)}, with the focus on ${focusedRow() ?? document.activeElement?.tagName}`
        ),
    });
    await waitFor(() => expect(heading).toHaveFocus());
    expect(
      view.container.querySelector("[data-plan-body]"),
      "the Plan view was drawn beside the line"
    ).toBeNull();
  } finally {
    read.mockRestore();
  }
});

// A comment the tree holds is no task either, so the Plan view draws no row for it, where the
// Verification view draws it as a row of its own: the stage draws the outline in the Plan view's
// place, which lands on the comment's row.
test("a comment of the issue, named on the Plan view's address, draws the outline in the view's place, with the focus on the comment's row", async () => {
  answerVerificationReads();
  const view = drawStage("?comment=head-b", { issue: verifiedIssue(), preset: "plan" });
  await rowTakesFocus(
    view,
    '[data-issue-body] [data-node-row][data-node-id="head-b"]',
    "the comment's address"
  );
  expect(
    view.container.querySelector("[data-plan-body]"),
    "the Plan view was drawn beside the outline"
  ).toBeNull();
});

// Every entry the browser makes has the key "default", so each fragment it changes under the Plan
// view's address is a following of its own though the key stays one. The task keeps the view;
// the statement draws the outline in the view's place, which lands on the statement's row; and
// the task again draws the view, which lands on the task's step.
test("fragments the browser changes under one key each land under the Plan view: the task's step, the statement's row of the outline, then the step again", async () => {
  answerVerificationReads();
  const commentsOut = Promise.withResolvers<Comment[]>();
  spyOn(api, "listComments").mockReturnValue(commentsOut.promise);
  const plan = presetPath(KEY, "plan");
  const view = drawAtBrowserEntry(`${plan}#b-${PLAN_TASK}`, verifiedIssue());
  await rowTakesFocus(view, TASK_STEP, "the task's fragment");
  // The comments read answers last, and with it every read is held and the view has told the
  // stage whether it draws the task.
  await act(async () => commentsOut.resolve([...TWO_THREADS]));
  expect(drawnSlot(view), "the task's fragment drew the outline in place of the view").toBe(
    "the view"
  );
  expect(view.container.querySelector(TASK_STEP), "the focus left the task's step").toHaveFocus();
  view.follow(`${plan}#b-${STATEMENT}`);
  await rowTakesFocus(view, STATEMENT_ROW, "the statement's fragment under the same key");
  expect(
    view.container.querySelector("[data-plan-body]"),
    "the statement's fragment drew the Plan view beside the outline"
  ).toBeNull();
  view.follow(`${plan}#b-${PLAN_TASK}`);
  await rowTakesFocus(view, TASK_STEP, "the task's fragment again under the same key");
  expect(
    view.container.querySelector("[data-issue-body]"),
    "the task's fragment again drew the outline beside the Plan view"
  ).toBeNull();
});

// The outline decides each following of an address once: one whose tree has held the address's
// node draws no line in the outline's place for the rest of it. A fragment the browser changes
// under the one key "default", from a block the tree holds to one no node carries, is a following
// of its own, so the outline says it has no row for the new block, with the focus on that line.
test("a fragment the browser changes under one key, from a block the tree holds to one no node carries, draws the outline's line that says so, with the focus on it", async () => {
  answerVerificationReads();
  const conversation = presetPath(KEY, "conversation");
  const view = drawAtBrowserEntry(`${conversation}#b-${STATEMENT}`, verifiedIssue());
  await rowTakesFocus(view, STATEMENT_ROW, "the statement's fragment");
  view.follow(`${conversation}#b-no-such-block`);
  const heading = await waitFor(
    () => view.getByRole("heading", { name: "No row for this block" }),
    {
      onTimeout: () =>
        new Error(
          `the fragment of a block no node carries, under the same key, drew no line saying so; the focus is on ${focusedRow() ?? document.activeElement?.tagName}`
        ),
    }
  );
  await waitFor(() => expect(heading).toHaveFocus());
});

// The outline lands on the address's node until the reader's first act since they followed the
// address, which a watch of the following's own stops (`ReaderActsWatch`). A fragment the browser
// changes under the one key "default", after the reader pressed a key, is a following of its own,
// with a watch of its own, so the outline lands on the new node as it landed on the first.
test("after the reader pressed a key, a fragment the browser changes under one key lands on its row of the outline", async () => {
  answerVerificationReads();
  const conversation = presetPath(KEY, "conversation");
  const view = drawAtBrowserEntry(`${conversation}#b-${STATEMENT}`, verifiedIssue());
  await rowTakesFocus(view, STATEMENT_ROW, "the statement's fragment");
  // Shift alone, which moves no focus, pressed with the focus on the statement's row: the
  // reader's act since the statement's fragment.
  act(() => {
    document.activeElement?.dispatchEvent(
      new KeyboardEvent("keydown", { bubbles: true, key: "Shift" })
    );
  });
  view.follow(`${conversation}#b-${PLAN_TASK}`);
  await rowTakesFocus(view, TASK_ROW, "the task's fragment under the same key, after a key");
});

// Every entry the browser makes has the key "default". Under the Verification view a check keeps
// the view. A plan task under the statement, changed to with the fragment alone under the same
// key, is no row of the view, which draws a statement's checks alone: the stage draws the outline
// in the view's place, which lands on the task's row.
test("under the Verification view, a fragment the browser changes under one key from a check to a plan task draws the outline, with the focus on the task's row", async () => {
  answerVerificationReads();
  const commentsOut = Promise.withResolvers<Comment[]>();
  spyOn(api, "listComments").mockReturnValue(commentsOut.promise);
  const verification = presetPath(KEY, "verification");
  const view = drawAtBrowserEntry(`${verification}#b-${CHECK}`, verifiedIssue());
  const checkRow = `[data-verification-body] [data-node-row][data-node-id="${blockNodeId(VERIFICATION_SLUG, CHECK)}"]`;
  await waitFor(() =>
    expect(
      view.container.querySelector(checkRow),
      `the check's fragment drew no row of it; the stage drew ${drawnSlot(view)}`
    ).not.toBeNull()
  );
  // The comments read answers last, and with it every read is held and the view has told the
  // stage whether it draws the check.
  await act(async () => commentsOut.resolve([...TWO_THREADS]));
  expect(drawnSlot(view), "the check's fragment drew the outline in place of the view").toBe(
    "the view"
  );
  view.follow(`${verification}#b-${PLAN_TASK}`);
  await rowTakesFocus(view, TASK_ROW, "the task's fragment under the same key");
  expect(
    view.container.querySelector("[data-verification-body]"),
    "the task's fragment drew the Verification view beside the outline"
  ).toBeNull();
});

// Every entry the browser makes has the key "default". Under the Decisions view and words, a
// decision its fragment names keeps the view, which draws it whatever the words. A person's
// write-up, changed to with the fragment alone under the same key and words, is no row of the
// list where the words match no question it answers: the stage draws the outline in the view's
// place, which lands on the write-up's row.
test("under the Decisions view and words, a fragment the browser changes under one key from a decision to a write-up the list draws no row for draws the outline, with the focus on the write-up's row", async () => {
  spyOn(api, "listIssueAsks").mockResolvedValue([...ASKED]);
  spyOn(api, "getIssueDecisions").mockResolvedValue(
    decisionsRead([decision("d1"), ...(WRITE_UP.decisions ?? [])])
  );
  // The decisions read the view's entries come from reads the comments, so the messages read is
  // the one held: it answers last, and with it every read the tree is built from is held.
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

/** A refusal of a read, as Dispatch answers one it cannot serve now. */
const REFUSED = new ApiError(503, { code: "UNAVAILABLE", error: "the read is refused" });

/** Waits for the outline, drawn in a view's place, to draw the failed read's line that says
 *  `message` with the focus on its Retry, saying what `what` drew where it does not. */
async function retryTakesFocus(view: RenderResult, message: string, what: string): Promise<void> {
  await waitFor(() => {
    const line = [...view.container.querySelectorAll("[data-issue-body] [data-failed-read]")].find(
      (each) => each.textContent?.includes(message)
    );
    expect(
      line,
      `${what} drew no line "${message}"; the stage drew ${drawnSlot(view)}`
    ).toBeDefined();
    expect(
      line?.querySelector(FAILED_READ_RETRY),
      `${what} left the focus on ${focusedRow() ?? document.activeElement?.tagName}`
    ).toHaveFocus();
  });
}

// A read the view draws no line of its own for, refused, is no row of the view. The Decisions
// view draws a line for the decisions read and the asks alone, so with the comments read refused
// a comment named beside it lands on nothing there: the stage draws the outline in the view's
// place, which says the comments could not be loaded, with the focus on its Retry, as it did
// before the view was filled.
test("the comments read refused, a comment named beside the Decisions view draws the outline in its place, with the comments' line and the focus on its Retry", async () => {
  spyOn(api, "listComments").mockRejectedValue(REFUSED);
  const view = drawStage("?comment=reply-b");
  await retryTakesFocus(view, STATE_LINES.comments_unreadable, "the comment's address");
  expect(
    view.container.querySelector("[data-decisions-body]"),
    "the Decisions view was drawn beside the outline"
  ).toBeNull();
});

// The Verification view draws a line for the coverage read, the results read and, where the
// results read holds a check, its own document's blocks alone, so with the plan's blocks refused
// a plan task named on its address lands on nothing there: the stage draws the outline in the
// view's place, which says the plan could not be loaded, with the focus on its Retry.
test("the plan's blocks refused, a plan task named on the Verification view's address draws the outline in its place, with the plan's line and the focus on its Retry", async () => {
  answerVerificationReads();
  spyOn(api, "getArtifactBlocks").mockImplementation(async (id: string) => {
    if (id === PLAN_DOCUMENT.id) throw REFUSED;
    return [...(BLOCKS[id] ?? [])];
  });
  const view = drawStage(`#b-${PLAN_TASK}`, { issue: verifiedIssue(), preset: "verification" });
  await retryTakesFocus(view, STATE_LINES.plan_unreadable, "the plan task's address");
  expect(
    view.container.querySelector("[data-verification-body]"),
    "the Verification view was drawn beside the outline"
  ).toBeNull();
});

// Where the results read holds no check the Verification view needs no blocks read, and draws no
// line for its document's blocks (`useVerificationReads`), so with those blocks refused a check
// named on its address lands on nothing there: the stage draws the outline in the view's place,
// which says the Verification document could not be loaded, with the focus on its Retry.
test("the results read holding no check and the Verification document's blocks refused, a check named on the Verification view's address draws the outline in its place, with the blocks' line and the focus on its Retry", async () => {
  answerVerificationReads();
  spyOn(api, "getIssueResults").mockResolvedValue({
    checks: [],
    issue: KEY,
    verification_artifact_id: VERIFICATION_DOCUMENT.id,
  });
  spyOn(api, "getArtifactBlocks").mockImplementation(async (id: string) => {
    if (id === VERIFICATION_DOCUMENT.id) throw REFUSED;
    return [...(BLOCKS[id] ?? [])];
  });
  const view = drawStage(`#b-${CHECK}`, { issue: verifiedIssue(), preset: "verification" });
  await retryTakesFocus(view, STATE_LINES.verification_unreadable, "the check's address");
  expect(
    view.container.querySelector("[data-verification-body]"),
    "the Verification view was drawn beside the outline"
  ).toBeNull();
});

// The Plan view draws a line for the plan's blocks and the spec's alone, so with the comments
// read refused a comment named on its address lands on nothing there: the stage draws the outline
// in the view's place, which says the comments could not be loaded, with the focus on its Retry.
test("the comments read refused, a comment named on the Plan view's address draws the outline in its place, with the comments' line and the focus on its Retry", async () => {
  answerVerificationReads();
  spyOn(api, "listComments").mockRejectedValue(REFUSED);
  const view = drawStage("?comment=head-b", { issue: verifiedIssue(), preset: "plan" });
  await retryTakesFocus(view, STATE_LINES.comments_unreadable, "the comment's address");
  expect(
    view.container.querySelector("[data-plan-body]"),
    "the Plan view was drawn beside the outline"
  ).toBeNull();
});

/** A check of the Verification document that serves no statement. */
const LOOSE_CHECK = "c2";

// A chip the reader pressed draws that state's statements alone, with their checks, and no check
// that serves none (`VerificationFilterBody`), and no following clears it. The statement reads
// Unknown, its check holding no result, so Met draws no statement and no check. Followed under the
// same view with Met pressed, an address naming the check that serves none, or the statement's
// check, names no row the view draws: the stage draws the outline in the view's place, which
// lands on the check's row.
test("with a chip pressed, a check the chip leaves out, named on a new address under the Verification view, draws the outline in its place, with the focus on the check's row", async () => {
  answerVerificationReads();
  spyOn(api, "getArtifactBlocks").mockImplementation(async (id: string) => [
    ...(BLOCKS[id] ?? []),
    ...(id === VERIFICATION_DOCUMENT.id ? [typedBlock(LOOSE_CHECK, "check")] : []),
  ]);
  spyOn(api, "getIssueResults").mockResolvedValue({
    checks: [check({ block_id: CHECK }), check({ block_id: LOOSE_CHECK })],
    issue: KEY,
    verification_artifact_id: VERIFICATION_DOCUMENT.id,
  });
  const verification = presetPath(KEY, "verification");
  for (const id of [LOOSE_CHECK, CHECK]) {
    const held: { navigate?: NavigateFunction } = {};
    const view = drawStage("", { held, issue: verifiedIssue(), preset: "verification" });
    const row = `[data-node-row][data-node-id="${blockNodeId(VERIFICATION_SLUG, id)}"]`;
    await waitFor(() =>
      expect(
        view.container.querySelector(`[data-verification-body] ${row}`),
        `the Verification view drew no row of ${id}; the stage drew ${drawnSlot(view)}`
      ).not.toBeNull()
    );
    const met = view.container.querySelector<HTMLElement>('[data-verification-chip="met"]');
    act(() => met?.click());
    expect(met, "the Met chip was not pressed").toHaveAttribute("aria-pressed", "true");
    await act(async () => held.navigate?.(`${verification}#b-${id}`));
    await rowTakesFocus(view, `[data-issue-body] ${row}`, `the address of ${id} with Met pressed`);
    expect(
      view.container.querySelector("[data-verification-body]"),
      `the address of ${id} with Met pressed drew the Verification view beside the outline`
    ).toBeNull();
    cleanup();
  }
});
