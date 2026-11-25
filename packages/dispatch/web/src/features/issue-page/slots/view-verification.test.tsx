import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { act, cleanup } from "@testing-library/react";

import { nextFrame } from "../../../__tests__/frames";
import { api } from "../../../api/client";
import type { ArtifactBlock, IssueDetails } from "../../../api/types";
import {
  artifact,
  issueDetails,
  KEY,
  SPEC_SLUG,
  specRef,
} from "../decisions-filter/decisions-fixtures";
import { blockNodeId } from "../tree-drafts";
import { nodeRowSelectorOf } from "../tree-focus";
import { check, VERIFICATION_SLUG } from "../verification-filter/verification-fixtures";
import { drawnSlot, drawStage, focusedRow, rowTakesFocus, stubReads } from "./stage-fixtures";

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

// `VERIFICATION_DOCUMENT`, `verifiedIssue`, `typedBlock`, `BLOCKS` and `answerVerificationReads`
// are cut-down copies of the private fixtures of the same names in `view-address.test.tsx`: a
// test file imports no other test file, and `stage-fixtures.tsx`, where a shared copy would go,
// may take no more changed lines in this change. They go once those fixtures move into it.

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

/** The block ids of the spec's one statement and of the check of the Verification document that
 *  serves it. */
const STATEMENT = "r1";
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

/** Each document's blocks, by its id: the spec's statement, and the Verification document's
 *  check, whose `serves` places it under that statement (`tree-drafts.ts`). */
const BLOCKS: Readonly<Record<string, readonly ArtifactBlock[]>> = {
  "artifact-spec": [typedBlock(STATEMENT, "requirement")],
  [VERIFICATION_DOCUMENT.id]: [typedBlock(CHECK, "check", specRef(STATEMENT))],
};

/** Answers the reads the tree and the Verification view are drawn from for `verifiedIssue`: the
 *  coverage read with the statement, each document's blocks, and the results read with the
 *  check. Each spy is the one `stubReads` set, which `spyOn` hands back, so the `afterEach` that
 *  restores those restores it. */
function answerVerificationReads(): void {
  spyOn(api, "getIssueCoverage").mockResolvedValue({
    gaps: [],
    has_plan: false,
    issue: KEY,
    requirements: [
      {
        block_id: STATEMENT,
        checks: 1,
        gaps: [],
        plan_tasks: 0,
        ref: specRef(STATEMENT),
        text: "The view lands on the statement an address names.",
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
