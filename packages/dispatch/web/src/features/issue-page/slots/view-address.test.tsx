import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { act, cleanup, waitFor } from "@testing-library/react";
import type { NavigateFunction } from "react-router-dom";

import { api } from "../../../api/client";
import type { ArtifactBlock, Comment, IssueDetails } from "../../../api/types";
import {
  artifact,
  issueDetails,
  KEY,
  SPEC_SLUG,
  specRef,
} from "../decisions-filter/decisions-fixtures";
import { presetPath } from "../issue-address";
import { TWO_THREADS } from "../outline-fixtures";
import { STATE_LINES } from "../page-words";
import { blockNodeId } from "../statement-rules";
import { check, VERIFICATION_SLUG } from "../verification-filter/verification-fixtures";
import {
  commentGone,
  drawAtBrowserEntry,
  drawnSlot,
  drawStage,
  focusedRow,
  GONE_COMMENT,
  goneLineTakesFocus,
  REFUSED,
  retryTakesFocus,
  rowTakesFocus,
  stubReads,
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
 *  so the `afterEach` that restores those restores it. */
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

/** The task's step in the Plan view's chart, and the outline's rows of the task, the statement
 *  and the second thread's head. */
const TASK_STEP = `[data-plan-body] [data-node-row][data-node-id="${blockNodeId(PLAN_DOCUMENT.slug, PLAN_TASK)}"]`;
const TASK_ROW = `[data-issue-body] [data-node-row][data-node-id="${blockNodeId(PLAN_DOCUMENT.slug, PLAN_TASK)}"]`;
const STATEMENT_ROW = `[data-issue-body] [data-node-row][data-node-id="${blockNodeId(SPEC_SLUG, STATEMENT)}"]`;
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
