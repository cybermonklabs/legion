import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, cleanup, type RenderResult, render, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter, type NavigateFunction, useLocation } from "react-router-dom";

import { api } from "../../../api/client";
import type { Comment, IssueDetails } from "../../../api/types";
import { parseIssuePath } from "../../refs/routes";
import {
  artifact,
  issueDetails,
  KEY,
  SPEC_DOCUMENT,
  SPEC_SLUG,
  specRef,
} from "../decisions-filter/decisions-fixtures";
import { presetPath } from "../issue-address";
import { TWO_THREADS, waitInAct } from "../outline-fixtures";
import { STATE_LINES } from "../page-words";
import { blockNodeId } from "../tree-drafts";
import { treeNode, treeTuple } from "../tree-read/tree-fixtures";
import { outlineQuery, type TreeAnswerNode } from "../tree-read/tree-read";
import { check, VERIFICATION_SLUG } from "../verification-filter/verification-fixtures";
import { FAILED_READ_RETRY } from "./failed-read-line";
import { ProvidersSlot } from "./providers";
import { StageSlot } from "./stage";
import {
  AppProviders,
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
} from "./stage-fixtures";

/**
 * Where an address lands on the stage under the Plan and Verification views, and in the
 * Conversation's outline as the browser changes the fragment under one key: on a row the view
 * draws, or on a row or a line of the outline, which under a view the stage draws in its place.
 */

let stubs: { mockRestore(): void }[] = [];
beforeEach(() => {
  stubs = stubReads();
});
afterEach(() => {
  cleanup();
  for (const stub of stubs) stub.mockRestore();
});

/** The plan and the Verification document of the issue the cases draw (`verifiedIssue`), beside
 *  its spec and its decisions document. */
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

/** The tree read for `verifiedIssue`: the spec's statement under the issue, the plan's task and
 *  the Verification document's check each under the statement it serves (a `serves` tuple), each
 *  in its document, then `checks` more of the Verification document, and the issue's two threads
 *  where `threads` says so. */
function verifiedTree(checks: readonly TreeAnswerNode[] = [], threads = true): TreeAnswerNode[] {
  return [
    treeNode(STATEMENT, "requirement", KEY, {
      ...inDocument(SPEC_DOCUMENT),
      text: "The page draws the plan.",
    }),
    treeNode(PLAN_TASK, "plan_task", STATEMENT, inDocument(PLAN_DOCUMENT)),
    treeNode(CHECK, "check", STATEMENT, inDocument(VERIFICATION_DOCUMENT)),
    ...checks,
    ...(threads ? commentNodes(TWO_THREADS) : []),
  ];
}

/** The tuples of `verifiedTree`: the task and the check each serve the statement. */
const SERVES = [treeTuple("serves", PLAN_TASK, STATEMENT), treeTuple("serves", CHECK, STATEMENT)];

/** Answers the tree read with `verifiedTree`'s nodes and tuples (`treeOf`), and each view read
 *  with the view's own nodes of them, as its query took them, and the statement they serve as the
 *  Verification view's context, as the route answers a view. */
function answerVerifiedTree(checks: readonly TreeAnswerNode[] = [], threads = true): void {
  const nodes = verifiedTree(checks, threads);
  const own: Readonly<Record<string, string>> = {
    plan: "plan_task",
    spec: "requirement",
    verification: "check",
  };
  spyOn(api, "getIssueTree").mockImplementation(async (_key, query = {}) => {
    const { view } = query;
    if (view === undefined) return treeOf(nodes, SERVES);
    return treeOf(
      nodes.flatMap((node): TreeAnswerNode[] => {
        if (node.supertag === own[view]) return [{ ...node, role: "query" }];
        if (view === "verification" && node.supertag === "requirement") {
          return [{ ...node, role: "context" }];
        }
        return [];
      }),
      SERVES
    );
  });
}

/** Answers the reads the tree and the Verification view are drawn from for `verifiedIssue`: the
 *  coverage read with the statement, the tree read with each document's blocks and the issue's
 *  two threads (`answerVerifiedTree`), the results read with the check, and the comments read.
 *  Each spy is the one `stubReads` set, which `spyOn` hands back, so the `afterEach` that
 *  restores those restores it. */
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
  answerVerifiedTree();
  spyOn(api, "getIssueResults").mockResolvedValue({
    checks: [check({ block_id: CHECK })],
    issue: KEY,
    verification_artifact_id: VERIFICATION_DOCUMENT.id,
  });
  spyOn(api, "listComments").mockResolvedValue([...TWO_THREADS]);
}

/** The task's step in the Plan view's chart, and the outline's rows of the task, the statement,
 *  the check and the second thread's head. */
const TASK_STEP = `[data-plan-body] [data-node-row][data-node-id="${blockNodeId(PLAN_DOCUMENT.slug, PLAN_TASK)}"]`;
const TASK_ROW = `[data-issue-body] [data-node-row][data-node-id="${blockNodeId(PLAN_DOCUMENT.slug, PLAN_TASK)}"]`;
const STATEMENT_ROW = `[data-issue-body] [data-node-row][data-node-id="${blockNodeId(SPEC_SLUG, STATEMENT)}"]`;
const CHECK_ROW = `[data-issue-body] [data-node-row][data-node-id="${blockNodeId(VERIFICATION_SLUG, CHECK)}"]`;
const COMMENT_ROW = '[data-issue-body] [data-node-row][data-node-id="head-b"]';

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
  // Only the outline draws the task's row, once the view has said it draws none, and the stage
  // takes nothing more the view says in this following, so nothing is waited for here.
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
    // The messages read, which answers last: with it every read is held, and the view has told
    // the stage whether it draws the node.
    const messagesOut = Promise.withResolvers<[]>();
    spyOn(api, "listIssueMessages").mockReturnValue(messagesOut.promise);
    const view = drawStage(after, { issue: verifiedIssue(), preset: "verification" });
    await waitFor(() =>
      expect(
        view.container.querySelector(`[data-verification-body] [data-node-id="${row}"]`),
        `the Verification view drew no row for ${after}; the stage drew ${drawnSlot(view)}`
      ).not.toBeNull()
    );
    await act(async () => messagesOut.resolve([]));
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
  const read = commentGone();
  try {
    const view = drawStage(`?comment=${GONE_COMMENT}`, { issue: verifiedIssue(), preset: "plan" });
    await goneLineTakesFocus(view, "the address of a comment the issue holds none of, under Plan");
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
  await rowTakesFocus(view, COMMENT_ROW, "the comment's address");
  expect(
    view.container.querySelector("[data-plan-body]"),
    "the Plan view was drawn beside the outline"
  ).toBeNull();
});

// A key pressed on the Plan view's step is an act of the following too, but the step it leaves the
// focus on leaves the page with the view where the view then draws no node the address names.
// The outline drawn in the view's place lands as it does where the reader pressed nothing: with
// the comments read out the view draws the task's step with the focus, `j` is pressed there, and
// once the comments answer, the outline lands on the comment's row.
test("`j` on the Plan view's step, before it draws the outline in its place for a comment of the issue named on its address, leaves the focus on the comment's row", async () => {
  answerVerificationReads();
  const commentsOut = Promise.withResolvers<Comment[]>();
  spyOn(api, "listComments").mockReturnValue(commentsOut.promise);
  const view = drawStage("?comment=head-b", { issue: verifiedIssue(), preset: "plan" });
  await rowTakesFocus(view, TASK_STEP, "the Plan view with the comments out");
  act(() => {
    document.activeElement?.dispatchEvent(
      new KeyboardEvent("keydown", { bubbles: true, key: "j" })
    );
  });
  await act(async () => commentsOut.resolve([...TWO_THREADS]));
  await rowTakesFocus(view, COMMENT_ROW, "the comments answering after `j`");
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
// with a watch of its own, so the outline lands on the new node as it landed on the first: the
// check, which Conversation draws under the statement it serves. A plan task is no row of
// Conversation (R9); the case below reads where its fragment goes.
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
  view.follow(`${conversation}#b-${CHECK}`);
  await rowTakesFocus(view, CHECK_ROW, "the check's fragment under the same key, after a key");
});

// R9: an address that names a node Conversation draws no row of opens the filter of its kind at
// that node: a plan task's fragment on Conversation's own address opens Plan, whose address it
// replaces, and the Plan view lands on the task's step.
test("a plan task named on Conversation's address opens Plan, with the focus on the task's step", async () => {
  answerVerificationReads();
  const view = drawStage(`#b-${PLAN_TASK}`, { issue: verifiedIssue(), preset: "conversation" });
  await rowTakesFocus(view, TASK_STEP, "the task's fragment on Conversation's address");
  expect(view.container.querySelector("[data-issue-body]")).toBeNull();
});

// Graph draws Graph's read whole as one outline, through the outline's own rows, and an address
// that names a node lands on its row there as it does in Conversation, through the outline's own
// landing: a comment named beside the address, a node address (`?node=`, the node's id in the
// tree read), and the node Graph's zoom address roots Graph at.
test("an address naming a node lands on its row in Graph as in Conversation: a comment beside the address, a node address, and Graph's zoom", async () => {
  spyOn(api, "listComments").mockResolvedValue([...TWO_THREADS]);
  answerTree(commentNodes(TWO_THREADS));
  for (const [after, preset, row] of [
    ["?comment=reply-b", "conversation", "reply-b"],
    ["?comment=reply-b", "graph", "reply-b"],
    ["?node=reply-b", "conversation", "reply-b"],
    ["?node=reply-b", "graph", "reply-b"],
    ["/head-b", "graph", "head-b"],
  ] as const) {
    const view = drawStage(after, { preset });
    await rowTakesFocus(view, `[data-node-row][data-node-id="${row}"]`, `${after} in ${preset}`);
    cleanup();
  }
});

// Conversation's outline alone reads a node address, and Graph's (`?node=`), so one on
// Conversation's own address whose node Conversation draws no row of opens Graph at that node,
// whose address it replaces: a plan task Conversation's tree holds, which it draws no row of
// (R9), and one Conversation's read holds none of, which Graph's read, as the page holds it from
// the reader's earlier visit to Graph, holds. Graph lands on the task's row, and draws a second
// task, which Conversation draws no row of.
test("a node address whose node Conversation draws no row of opens Graph at that node, with the focus on its row", async () => {
  // The task's id in the tree read, which the tree draws by its block's node id.
  const readId = `${KEY}/${PLAN_DOCUMENT.slug}#${PLAN_TASK}`;
  const second = treeNode("t2", "plan_task", STATEMENT, inDocument(PLAN_DOCUMENT, 1));
  const secondRow = `[data-issue-body] [data-node-row][data-node-id="${blockNodeId(PLAN_DOCUMENT.slug, "t2")}"]`;
  for (const conversationHolds of [true, false]) {
    answerVerificationReads();
    answerVerifiedTree([second]);
    if (!conversationHolds) {
      const whole = treeOf(verifiedTree([second]), SERVES);
      const conversation = treeOf(
        verifiedTree([second]).filter((node) => node.id !== readId),
        SERVES
      );
      spyOn(api, "getIssueTree").mockImplementation(async (_key, query = {}) => {
        const asked = query as { readonly kind?: unknown; readonly view?: string };
        if (asked.view !== undefined) return Promise.withResolvers<never>().promise;
        return asked.kind === undefined ? whole : conversation;
      });
    }
    const what = `the task's node address, ${conversationHolds ? "" : "not "}held by Conversation`;
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    if (!conversationHolds) await client.prefetchQuery(outlineQuery(KEY, "graph"));
    const view = drawStage(`?node=${encodeURIComponent(readId)}`, {
      client,
      issue: verifiedIssue(),
      preset: "conversation",
    });
    await rowTakesFocus(view, TASK_ROW, what);
    // Conversation draws the task too where the address names it, but no other task: the
    // second task's row is Graph's.
    expect(view.container.querySelector(secondRow), `${what} drew no Graph`).not.toBeNull();
    cleanup();
  }
});

/** The address the router held as the stage was last drawn (`SeenStage`): its path, search and
 *  fragment. */
const seen: { address?: string } = {};

/** The page's stage at the address the router holds, as `drawStage` draws it, which marks that
 *  address in `seen`. */
function SeenStage({ issue }: { readonly issue: IssueDetails }): ReactNode {
  const { hash, pathname, search } = useLocation();
  seen.address = `${pathname}${search}${hash}`;
  const route = parseIssuePath(pathname, search);
  if (route === undefined) throw new Error(`${pathname} names no route`);
  return (
    <ProvidersSlot issue={issue} route={route}>
      <StageSlot issue={issue} route={route} />
    </ProvidersSlot>
  );
}

// R9: a node address no read holds opens the outline's own address without the node, with the
// same other search and fragment and no line: Conversation's, where the page holds no Graph read,
// and Graph's on Graph's own address, still zoomed to the node it zooms to where the tree holds
// that node. Graph's address zoomed to a node no read holds opens as Graph's own address. The
// outline is drawn there.
test("a node address no read holds opens the outline's own address without it, with no line", async () => {
  spyOn(api, "listComments").mockResolvedValue([...TWO_THREADS]);
  answerTree(commentNodes(TWO_THREADS));
  const conversation = presetPath(KEY, "conversation");
  const graph = presetPath(KEY, "graph");
  for (const [from, to] of [
    [`${conversation}?node=nobody&from=mail#seen`, `${conversation}?from=mail#seen`],
    [`${graph}?node=nobody&from=mail#seen`, `${graph}?from=mail#seen`],
    [`${graph}/head-b?node=nobody`, `${graph}/head-b`],
    [`${graph}/nobody?node=nobody`, graph],
    [`${graph}/nobody?from=mail#seen`, `${graph}?from=mail#seen`],
  ] as const) {
    const view = render(
      <MemoryRouter initialEntries={[from]}>
        <AppProviders client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
          <SeenStage issue={issueDetails()} />
        </AppProviders>
      </MemoryRouter>
    );
    await waitFor(() => expect(seen.address, from).toBe(to));
    await waitFor(() =>
      expect(
        view.container.querySelector('[data-issue-body] [data-node-row][data-node-id="head-b"]'),
        `${from} drew no outline at ${to}`
      ).not.toBeNull()
    );
    cleanup();
  }
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

/** Waits for the Verification view to draw its line that the Verification document could not be
 *  loaded, with the focus on its Retry, saying what `what` drew where it does not; then, once
 *  every read has settled, checks that the stage still draws the view, with the focus on that
 *  Retry. */
async function viewKeepsItsLine(view: RenderResult, what: string): Promise<void> {
  await retryTakesFocus(
    view,
    STATE_LINES.verification_unreadable,
    what,
    "[data-verification-body]"
  );
  // Every read has settled, and the stage has taken whatever the view said of the address's node.
  await waitInAct(20);
  expect(drawnSlot(view), "the stage drew the outline in place of the view").toBe("the view");
  expect(
    view.container.querySelector(`[data-verification-body] ${FAILED_READ_RETRY}`),
    `the focus left the view's Retry for ${focusedRow() ?? document.activeElement?.tagName}`
  ).toHaveFocus();
}

// One read, the tree read, stands behind every document's blocks, the plan's among them, and the
// Verification view draws its own line for that read's refusal, which says the Verification
// document could not be loaded. A view keeps its place where each refused read is one it draws
// its own line for, so with the plan's blocks refused the view keeps its place: a plan task named
// on its address draws the view's line, with the focus on its Retry, and no outline in the view's
// place.
test("the plan's blocks refused, a plan task named on the Verification view's address keeps the view, with the view's line and the focus on its Retry", async () => {
  answerVerificationReads();
  // The plan's blocks are the tree read's, so it is the tree read that is refused.
  spyOn(api, "getIssueTree").mockRejectedValue(REFUSED);
  const view = drawStage(`#b-${PLAN_TASK}`, { issue: verifiedIssue(), preset: "verification" });
  await viewKeepsItsLine(view, "the plan task's address");
});

// The Verification view waits on the tree read whether or not the results read holds a check,
// since that read holds the statements as well as its document's blocks (`useVerificationReads`),
// and draws its own line for the read's refusal. A view keeps its place where each refused read
// is one it draws its own line for, so with the results read holding no check and those blocks
// refused the view keeps its place: a check named on its address draws the view's line that the
// Verification document could not be loaded, with the focus on its Retry, and no outline in the
// view's place.
test("the results read holding no check and the Verification document's blocks refused, a check named on the Verification view's address keeps the view, with the blocks' line and the focus on its Retry", async () => {
  answerVerificationReads();
  spyOn(api, "getIssueResults").mockResolvedValue({
    checks: [],
    issue: KEY,
    verification_artifact_id: VERIFICATION_DOCUMENT.id,
  });
  // The Verification document's blocks are the tree read's, so it is the tree read that is
  // refused.
  spyOn(api, "getIssueTree").mockRejectedValue(REFUSED);
  const view = drawStage(`#b-${CHECK}`, { issue: verifiedIssue(), preset: "verification" });
  await viewKeepsItsLine(view, "the check's address");
});

// The Plan view draws a line for the plan's blocks and the spec's alone, so with the comments
// read refused a comment named on its address lands on nothing there: the stage draws the outline
// in the view's place, which says the comments could not be loaded, with the focus on its Retry.
test("the comments read refused, a comment named on the Plan view's address draws the outline in its place, with the comments' line and the focus on its Retry", async () => {
  answerVerificationReads();
  spyOn(api, "listComments").mockRejectedValue(REFUSED);
  // The comments come with the tree read: with the comments read refused, the page holds none.
  answerVerifiedTree([], false);
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
  answerVerifiedTree([treeNode(LOOSE_CHECK, "check", KEY, inDocument(VERIFICATION_DOCUMENT, 1))]);
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
