import { afterEach, beforeAll, beforeEach, expect, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, type RenderResult, render, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { issueAskThreadsQuery, issueCoverageQuery, issueResultsQuery } from "../../../api/queries";
import type { IssueDetails } from "../../../api/types";
import { issueDecisionsQuery } from "../../doc/spec-marks";
import { MarginProvider } from "../../margin/margin-context";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import { ReferenceNamesProvider } from "../../refs/reference-names";
import { KeymapProvider } from "../../shell/KeymapProvider";
import { issueTree, KEY, READER } from "../decisions-filter/decisions-fixtures";
import { DocumentStages } from "../document-stage";
import { FocusedNodeProvider } from "../focused-node";
import type { TreeNode } from "../issue-tree";
import { NO_FAILED_READS } from "../outline-reads";
import { OutlineRequestProvider } from "../outline-request";
import { EMPTY_STATE_LINES } from "../page-words";
import { presetView } from "../presets";
import { Publisher, publishedTree } from "../projection-fixture";
import {
  BAND_PX,
  IssueScope,
  idsOf,
  type LayoutByHand,
  layoutByHand,
  SCREEN_PX,
  type TasksByHand,
  tasksByHand,
} from "../slices-fixtures";
import { focusedRow } from "../slots/stage-fixtures";
import { treeAnswer } from "../tree-read/tree-fixtures";
import { outlineQuery } from "../tree-read/tree-read";
import { SpecFilterBody } from "./SpecFilterBody";
import { coverageRead, decisionsRead, issueDetails, statement } from "./spec-fixtures";

/**
 * The Spec view's statements drawn in slices (`drawn-in-slices.ts`), the plan's state table "The
 * rows drawn in slices" rows 4, 5, 6 and 10 for the view's own keys, its address and the next
 * page its read answers: `j` on the last statement drawn and `k` on the first draw the next and
 * the one before in the same commit and move the focus there; an address that names a statement
 * not drawn draws every statement up to it and lands on it, in that commit's layout effect, under
 * the header; and the statements a next page brings are drawn the first screen of them at once,
 * the rest in slices.
 *
 * happy-dom lays nothing out, so the page is laid out by hand, each statement 100 px tall in a
 * window 600 px tall under a header's band (`layoutByHand`), and the slices are tasks run by hand
 * (`tasksByHand`).
 */

beforeAll(async () => {
  await warmMarkdownRenderer();
});

let layout: LayoutByHand;
let tasks: TasksByHand;
beforeEach(() => {
  layout = layoutByHand({ band: true });
  tasks = tasksByHand();
});
afterEach(() => {
  tasks.restore();
  layout.restore();
});

/** The statement blocks `r00` on, `count` of them. */
function blocksOf(count: number): string[] {
  return idsOf(count, "r");
}

/** The node id of statement `block`. */
const statementId = (block: string) => `spec#b-${block}`;

/** The reads the page's tree fills, each by an observer as the tree's own (`useIssueTree.ts`),
 *  which the Spec body only watches. */
function SpecTreeReads({ issue }: { issue: IssueDetails }): ReactNode {
  useQuery(issueCoverageQuery(issue.key));
  useQuery(issueDecisionsQuery(issue.key, true));
  useQuery(issueResultsQuery(issue.key));
  useQuery(issueAskThreadsQuery(issue.key));
  useQuery(outlineQuery(issue.key, "conversation"));
  return null;
}

/** The Spec view of the issue holding `blocks`, as its statements, with every read it waits on
 *  answered; `reveal` is the statement an address names. `redraw` draws it again over other
 *  statements or another address. */
function drawSpec(
  blocks: readonly string[],
  reveal?: string
): {
  readonly redraw: (next: readonly string[], named?: string) => void;
  readonly view: RenderResult;
} {
  const issue = issueDetails();
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const results = { checks: [], issue: KEY, verification_artifact_id: "" };
  client.setQueryData(["decisions", KEY], decisionsRead([]));
  client.setQueryData(issueResultsQuery(KEY).queryKey, results);
  client.setQueryData(issueCoverageQuery(KEY).queryKey, coverageRead([...blocks]));
  client.setQueryData(issueAskThreadsQuery(KEY).queryKey, []);
  client.setQueryData(outlineQuery(KEY, "conversation").queryKey, treeAnswer([]));
  const draw = (drawn: readonly string[], named: string | undefined) => {
    const tree: TreeNode = issueTree(drawn.map((block) => statement(block)));
    const view = presetView(tree, "spec", "", new Set(named === undefined ? [] : [named])).root;
    const published = publishedTree(tree);
    return (
      <QueryClientProvider client={client}>
        <ReferenceNamesProvider>
          <Publisher published={published} />
          <MemoryRouter>
            <KeymapProvider>
              <MarginProvider>
                <OutlineRequestProvider>
                  <FocusedNodeProvider>
                    <IssueScope />
                    <SpecTreeReads issue={issue} />
                    <DocumentStages>
                      <SpecFilterBody
                        context={new Set()}
                        counts={undefined}
                        emptyLine={EMPTY_STATE_LINES.spec}
                        holdsEscape={false}
                        issue={issue}
                        namesLine={NO_FAILED_READS}
                        onEditing={() => {}}
                        published={published}
                        reader={READER}
                        readsSettled
                        {...(named === undefined ? {} : { reveal: named })}
                        root={tree}
                        view={view}
                      />
                    </DocumentStages>
                  </FocusedNodeProvider>
                </OutlineRequestProvider>
              </MarginProvider>
            </KeymapProvider>
          </MemoryRouter>
        </ReferenceNamesProvider>
      </QueryClientProvider>
    );
  };
  const view = render(draw(blocks, reveal === undefined ? undefined : statementId(reveal)));
  return {
    redraw: (next, named) =>
      view.rerender(draw(next, named === undefined ? undefined : statementId(named))),
    view,
  };
}

/** The statement blocks the view draws in `container`, in order. */
function statementsDrawn(container: HTMLElement): string[] {
  return [...container.querySelectorAll<HTMLElement>("[data-spec-statement]")].map((row) =>
    (row.dataset.nodeId ?? "").replace("spec#b-", "")
  );
}

// Row 4. `j` on the last statement drawn draws the next in the same commit and moves the focus
// there.
test("row 4: j on the last statement drawn draws the next and moves the focus there", async () => {
  const user = userEvent.setup();
  const blocks = blocksOf(40);
  const { view } = drawSpec(blocks);
  try {
    await waitFor(() => expect(statementsDrawn(view.container)).toContain("r00"));
    const first = statementsDrawn(view.container);
    expect(first, "the first drawing drew every statement").not.toContain("r39");
    await user.keyboard("j");
    expect(focusedRow(), "j from the page did not go to the first statement").toBe(
      statementId("r00")
    );
    for (let at = 1; at < first.length; at += 1) await user.keyboard("j");
    const last = first.at(-1) ?? "";
    expect(focusedRow(), "j did not reach the last statement drawn").toBe(statementId(last));
    const next = blocks[blocks.indexOf(last) + 1] ?? "";
    expect(statementsDrawn(view.container), "the next statement was drawn already").not.toContain(
      next
    );
    await user.keyboard("j");
    expect(focusedRow(), "j on the last statement drawn did not move to the next").toBe(
      statementId(next)
    );
    expect(tasks.pending(), "the statements were all drawn by then").toBe(1);
  } finally {
    view.unmount();
  }
});

// Row 5. `k` on the first statement drawn, with statements above it not drawn, draws the one
// above in the same commit and moves the focus there.
test("row 5: k on the first statement drawn, with statements above it not drawn, draws the one above", async () => {
  const user = userEvent.setup();
  const blocks = blocksOf(40);
  const { view } = drawSpec(blocks, "r30");
  try {
    await waitFor(() =>
      expect(focusedRow(), "the address landed nowhere").toBe(statementId("r30"))
    );
    const first = statementsDrawn(view.container);
    expect(first, "the first drawing drew the first statement").not.toContain("r00");
    for (let at = first.indexOf("r30"); at > 0; at -= 1) await user.keyboard("k");
    const top = first[0] ?? "";
    expect(focusedRow(), "k did not reach the first statement drawn").toBe(statementId(top));
    const above = blocks[blocks.indexOf(top) - 1] ?? "";
    await user.keyboard("k");
    expect(focusedRow(), "k on the first statement drawn did not move to the one above").toBe(
      statementId(above)
    );
    expect(tasks.pending(), "the statements were all drawn by then").toBe(1);
  } finally {
    view.unmount();
  }
});

// Row 6. An address that names a statement not drawn draws every statement up to it in one
// commit, whose layout effect brings it under the header; it takes the focus in the frame after,
// as an address's row does (`IssueTree.tsx`'s `landOnRow`).
test("row 6: an address that names a statement not drawn draws every statement up to it and lands on it", async () => {
  const blocks = blocksOf(40);
  const spec = drawSpec(blocks);
  const { view } = spec;
  try {
    await waitFor(() => expect(statementsDrawn(view.container)).toContain("r00"));
    expect(statementsDrawn(view.container), "the named statement was drawn at once").not.toContain(
      "r30"
    );
    act(() => spec.redraw(blocks, "r30"));
    expect(
      statementsDrawn(view.container).slice(0, 31),
      "the address drew the statements up to its own with one left out"
    ).toEqual(blocks.slice(0, 31));
    const top = () =>
      layout.topOf(view.container.querySelector(`[data-node-id="${statementId("r30")}"]`));
    const placed = top();
    expect(
      placed !== undefined && placed >= BAND_PX && placed < SCREEN_PX,
      `the commit that drew the named statement left it at ${placed} px, not under the header`
    ).toBe(true);
    await waitFor(() =>
      expect(focusedRow(), "the named statement took no focus").toBe(statementId("r30"))
    );
    expect(top(), "the focus moved the named statement").toBe(placed);
    expect(tasks.pending(), "the statements were all drawn by then").toBe(1);
  } finally {
    view.unmount();
  }
});

// Row 10. The statements the read's next page brings are drawn the first screen of them in the
// commit that brings them and the rest in slices; none is drawn before the page is read.
test("row 10: a next page's statements are drawn the first screen first, and none before it is read", async () => {
  const blocks = blocksOf(60);
  const spec = drawSpec(blocks.slice(0, 10));
  const { view } = spec;
  try {
    await waitFor(() => expect(statementsDrawn(view.container)).toContain("r00"));
    await tasks.runAll();
    expect(statementsDrawn(view.container), "the first page was not drawn whole").toEqual(
      blocks.slice(0, 10)
    );
    act(() => spec.redraw(blocks));
    const paged = statementsDrawn(view.container);
    expect(paged, "the next page drew no statement at once").toContain("r10");
    expect(paged, "the next page was drawn whole at once").not.toContain("r59");
    expect(tasks.pending(), "the next page asked for no slice").toBe(1);
    await tasks.runAll();
    expect(statementsDrawn(view.container), "the slices left statements not drawn").toEqual(blocks);
  } finally {
    view.unmount();
  }
});
