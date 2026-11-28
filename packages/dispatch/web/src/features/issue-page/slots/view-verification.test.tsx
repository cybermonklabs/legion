import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { act, cleanup, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { NavigateFunction } from "react-router-dom";

import { nextFrame } from "../../../__tests__/frames";
import { api } from "../../../api/client";
import type { TreeFilter } from "../../../api/queries";
import type { IssueDetails } from "../../../api/types";
import {
  artifact,
  issueDetails,
  KEY,
  SPEC_SLUG,
  specRef,
} from "../decisions-filter/decisions-fixtures";
import { presetPath } from "../issue-address";
import { blockNodeId } from "../tree-drafts";
import { nodeRowSelectorOf } from "../tree-focus";
import { treeNode, treeTuple } from "../tree-read/tree-fixtures";
import type { TreeAnswerNode } from "../tree-read/tree-read";
import { check, VERIFICATION_SLUG } from "../verification-filter/verification-fixtures";
import {
  drawnSlot,
  drawStage,
  focusedRow,
  inDocument,
  rowTakesFocus,
  stubReads,
  treeOf,
} from "./stage-fixtures";

/**
 * Where an address lands under the Verification view: on the row its body draws for the node the
 * address names, once every read the page's tree is built from has settled, and not where the
 * reader acted since they followed the address.
 */

let stubs: { mockRestore(): void }[] = [];
beforeEach(() => {
  stubs = stubReads();
});
afterEach(() => {
  cleanup();
  for (const stub of stubs) stub.mockRestore();
});

// `VERIFICATION_DOCUMENT`, `verifiedIssue`, `verifiedTree`, `SERVES` and `answerVerificationReads`
// are cut-down copies of the private fixtures of the same names in `view-address.test.tsx`, over
// the statements a case names: a test file imports no other test file, and `stage-fixtures.tsx`,
// where a shared copy would go, may take no more changed lines in this change. They go once
// those fixtures move into it.

/** The Verification document of the issue the case draws (`verifiedIssue`), beside its spec and
 *  its decisions document. */
const VERIFICATION_DOCUMENT = artifact("artifact-verification", VERIFICATION_SLUG);

/** The issue with its Verification document too, in its role. */
function verifiedIssue(): IssueDetails {
  const issue = issueDetails();
  return {
    ...issue,
    artifacts: [...(issue.artifacts ?? []), VERIFICATION_DOCUMENT],
    verification_artifact_id: VERIFICATION_DOCUMENT.id,
  };
}

/** The block ids of the spec's first statement, which the check serves, and of that check of the
 *  Verification document. */
const STATEMENT = "r1";
const CHECK = "c1";

/** The tree read's nodes for `verifiedIssue`: the spec's `statements` under the issue, and the
 *  Verification document's check under `STATEMENT`, which it serves (`SERVES`), each in its
 *  document. */
function verifiedTree(statements: readonly string[]): TreeAnswerNode[] {
  return [
    ...statements.map((id, position) =>
      treeNode(id, "requirement", KEY, {
        document: { artifact_id: "artifact-spec", position, slug: SPEC_SLUG },
        text: `The view lands on the statement an address names (${id}).`,
      })
    ),
    treeNode(CHECK, "check", STATEMENT, inDocument(VERIFICATION_DOCUMENT)),
  ];
}

/** The tuple of `verifiedTree`: the check serves the first statement. */
const SERVES = [treeTuple("serves", CHECK, STATEMENT)];

/** Answers the reads the tree and the Verification view are drawn from for `verifiedIssue`: the
 *  coverage read with `statements`, the tree read with each document's blocks (`verifiedTree`)
 *  and each view read with its view's own nodes of them, and the results read with the check,
 *  each once `until` answers, where a case holds them. Each spy is the one `stubReads` set, which
 *  `spyOn` hands back, so the `afterEach` that restores those restores it. */
function answerVerificationReads(
  statements: readonly string[] = [STATEMENT],
  until?: Promise<void>
): void {
  const answer = async <T,>(value: T): Promise<T> => {
    await until;
    return value;
  };
  spyOn(api, "getIssueCoverage").mockImplementation(() =>
    answer({
      gaps: [],
      has_plan: false,
      issue: KEY,
      requirements: statements.map((id) => ({
        block_id: id,
        checks: id === STATEMENT ? 1 : 0,
        gaps: [],
        plan_tasks: 0,
        ref: specRef(id),
        text: `The view lands on the statement an address names (${id}).`,
      })),
      spec_artifact_id: "artifact-spec",
    })
  );
  const nodes = verifiedTree(statements);
  const own: Readonly<Record<string, string>> = {
    plan: "plan_task",
    spec: "requirement",
    verification: "check",
  } satisfies Record<TreeFilter, string>;
  spyOn(api, "getIssueTree").mockImplementation((_key, query = {}) => {
    const { view } = query;
    if (view === undefined) return answer(treeOf(nodes, SERVES));
    return answer(
      treeOf(
        nodes.flatMap((node): TreeAnswerNode[] => {
          if (node.supertag === own[view]) return [{ ...node, role: "query" }];
          if (view === "verification" && node.supertag === "requirement") {
            return [{ ...node, role: "context" }];
          }
          return [];
        }),
        SERVES
      )
    );
  });
  spyOn(api, "getIssueResults").mockImplementation(() =>
    answer({
      checks: [check({ block_id: CHECK })],
      issue: KEY,
      verification_artifact_id: VERIFICATION_DOCUMENT.id,
    })
  );
}

/** The Verification view's row of the issue, its tab stop until the reads have settled, and its
 *  row of the spec's statement. */
const ISSUE_ROW = "[data-verification-body] [data-verification-issue]";
const STATEMENT_ROW = `[data-verification-body] ${nodeRowSelectorOf(blockNodeId(SPEC_SLUG, STATEMENT))}`;

// The Verification view lands on the row an address names once every read the tree's rows come
// from has settled, as the Spec view does. With the messages read out, the view draws its rows
// from its own reads, and the stage puts the focus on the issue's row, the view's tab stop; the
// statement's row does not take it. Once the messages answer every read has settled, and the
// statement's row takes the focus. Drawn again, the reader presses a control outside the stage
// while the messages are out, and once they answer the control keeps the focus: the landing
// yields to a reader who acted since they followed the address.
test("an address naming a statement on the Verification view lands the focus on that statement's row once the reads have settled", async () => {
  answerVerificationReads();
  const address = `#b-${STATEMENT}`;
  const messagesOut = Promise.withResolvers<[]>();
  spyOn(api, "listIssueMessages").mockReturnValue(messagesOut.promise);
  const view = drawStage(address, { issue: verifiedIssue(), preset: "verification" });
  await rowTakesFocus(view, ISSUE_ROW, "the Verification view with the messages out");
  // The frame a landing would have asked for as the rows were drawn.
  await act(() => nextFrame());
  expect(
    view.container.querySelector(STATEMENT_ROW),
    "the statement's row took the focus before the reads settled"
  ).not.toHaveFocus();
  await act(async () => messagesOut.resolve([]));
  await rowTakesFocus(view, STATEMENT_ROW, "the statement's address once the reads settled");
  expect(drawnSlot(view), "the stage drew the outline in place of the view").toBe("the view");
  cleanup();

  const actedOut = Promise.withResolvers<[]>();
  spyOn(api, "listIssueMessages").mockReturnValue(actedOut.promise);
  const control = document.body.appendChild(document.createElement("button"));
  try {
    const acted = drawStage(address, { issue: verifiedIssue(), preset: "verification" });
    await rowTakesFocus(
      acted,
      ISSUE_ROW,
      "the Verification view drawn again with the messages out"
    );
    act(() => {
      control.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true }));
      control.focus();
    });
    await act(async () => actedOut.resolve([]));
    // The frame a landing asked for as the reads settled.
    await act(() => nextFrame());
    expect(
      acted.container.querySelector(STATEMENT_ROW),
      `the Verification view drew no row for the statement; the stage drew ${drawnSlot(acted)}`
    ).toBeInTheDocument();
    expect(
      control,
      `the reads settling after the press moved the focus to ${focusedRow() ?? document.activeElement?.tagName}`
    ).toHaveFocus();
  } finally {
    control.remove();
  }
});

// The address's row takes the tree's one tab stop as the reads settle only where no key moved the
// stop in that following, as the Spec view keeps the row the reader moved to first. With the
// messages read out, the view draws its rows from its own reads and the stage puts the focus on
// the issue's row; ArrowDown moves the focus, and the stop with it, to the first statement. Once
// the messages answer, the landing yields to that key and leaves the focus there, and the stop
// stays with it: Tab out of the tree and Shift+Tab back lands where the reader was, not on the
// address's row.
test("a row the reader moved to by key before the reads settled keeps the tab stop", async () => {
  const user = userEvent.setup();
  const second = "r2";
  answerVerificationReads([STATEMENT, second]);
  const messagesOut = Promise.withResolvers<[]>();
  spyOn(api, "listIssueMessages").mockReturnValue(messagesOut.promise);
  const view = drawStage(`#b-${second}`, { issue: verifiedIssue(), preset: "verification" });
  await rowTakesFocus(view, ISSUE_ROW, "the Verification view with the messages out");
  await user.keyboard("{ArrowDown}");
  const first = view.container.querySelector<HTMLElement>(STATEMENT_ROW);
  const addressed = view.container.querySelector<HTMLElement>(
    `[data-verification-body] ${nodeRowSelectorOf(blockNodeId(SPEC_SLUG, second))}`
  );
  expect(first, "ArrowDown from the issue's row did not reach the first statement").toHaveFocus();
  expect(addressed, "the view drew no row for the address's statement").toBeInTheDocument();
  await act(async () => messagesOut.resolve([]));
  // The frame a landing would have asked for as the reads settled.
  await act(() => nextFrame());
  expect(drawnSlot(view), "the stage drew the outline in place of the view").toBe("the view");
  expect(first, `the reads settling moved the focus to ${focusedRow()}`).toHaveFocus();
  expect(
    [first?.tabIndex, addressed?.tabIndex],
    "the address's row took the tab stop from the row the reader moved to"
  ).toEqual([0, -1]);
});

// The address's row takes the tab stop only where the reader has not acted since they followed
// the address, in whatever render the view first draws it. Every read the view's rows wait on -
// the coverage, the results and every document's blocks, which the settling waits on as well - is
// held, so the view draws its tree with no row in it and the stage finds no row to give the focus,
// which stays on the page's body. The reader turns the wheel there. The reads then answer
// together, and the rows are first drawn in the render the reads settle in: the landing yields to
// the wheel, and the stage gives the focus it finds on nothing to the tab-stop row, which is the
// issue's, at the view's top, and not the address's row, which a page taller than the screen
// draws below it.
test("an address whose rows are drawn only once the reads settle gives the stop to the issue's row where the reader acted first", async () => {
  const answered = Promise.withResolvers<void>();
  answerVerificationReads([STATEMENT], answered.promise);
  const view = drawStage(`#b-${STATEMENT}`, { issue: verifiedIssue(), preset: "verification" });
  await waitFor(() =>
    expect(
      view.container.querySelector('[data-verification-body] [role="tree"]'),
      `the stage drew ${drawnSlot(view)} and no Verification view`
    ).toBeInTheDocument()
  );
  expect(
    view.container.querySelector(ISSUE_ROW),
    "the view drew its rows before the reads answered"
  ).toBeNull();
  await act(async () => window.dispatchEvent(new Event("wheel")));
  await act(async () => answered.resolve());
  const issueRow = await rowTakesFocus(
    view,
    ISSUE_ROW,
    "the Verification view whose reads answered after a turn of the wheel"
  );
  // The frame a landing would have asked for as the reads settled.
  await act(() => nextFrame());
  const addressed = view.container.querySelector<HTMLElement>(STATEMENT_ROW);
  expect(drawnSlot(view), "the stage drew the outline in place of the view").toBe("the view");
  expect(addressed, "the view drew no row for the address's statement").toBeInTheDocument();
  expect(issueRow, `the reads settling moved the focus to ${focusedRow()}`).toHaveFocus();
  expect(
    [issueRow.tabIndex, addressed?.tabIndex],
    "the address's row took the tab stop after the reader acted"
  ).toEqual([0, -1]);
});

// The stage's controller of the reader's acts is a following's own only from that following's
// first commit, so the render that draws a view at a new address still reads the last
// following's, which the press that followed the new address ended. The Spec view draws the
// statement with every read answered, and the reader presses, as on the statement's count of
// checks, which opens the Verification view at the statement's address: the view draws its rows
// in its first render, from the reads the page holds, and its row of the statement takes the tab
// stop and the focus, since the reader has not acted since they followed that address.
test("the Verification view opened at a statement after a press on the Spec view gives that statement's row the stop and the focus", async () => {
  answerVerificationReads();
  const held: { navigate?: NavigateFunction } = {};
  const view = drawStage("", { held, issue: verifiedIssue(), preset: "spec" });
  await rowTakesFocus(
    view,
    `[data-spec-body] ${nodeRowSelectorOf(blockNodeId(SPEC_SLUG, STATEMENT))}`,
    "the Spec view"
  );
  await act(async () => window.dispatchEvent(new Event("pointerdown")));
  await act(async () => held.navigate?.(`${presetPath(KEY, "verification")}#b-${STATEMENT}`));
  const addressed = await rowTakesFocus(
    view,
    STATEMENT_ROW,
    "the Verification view opened at the statement's address"
  );
  // The frame the landing asked for as the view was drawn.
  await act(() => nextFrame());
  expect(addressed, `the landing moved the focus to ${focusedRow()}`).toHaveFocus();
  expect(
    [view.container.querySelector<HTMLElement>(ISSUE_ROW)?.tabIndex, addressed.tabIndex],
    "the statement's row took no tab stop"
  ).toEqual([-1, 0]);
});
