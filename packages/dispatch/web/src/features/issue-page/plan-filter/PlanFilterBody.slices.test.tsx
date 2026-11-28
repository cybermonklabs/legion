import { afterEach, beforeAll, beforeEach, expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, type RenderResult, render, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { MarginProvider } from "../../margin/margin-context";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import { ReferenceNamesProvider } from "../../refs/reference-names";
import { KeymapProvider } from "../../shell/KeymapProvider";
import { KEY, node } from "../decisions-filter/decisions-fixtures";
import { FocusedNodeProvider } from "../focused-node";
import type { TreeNode } from "../issue-tree";
import { NO_FAILED_READS } from "../outline-reads";
import { EMPTY_STATE_LINES } from "../page-words";
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
import { ViewStage } from "../slots/stage";
import { focusedRow } from "../slots/stage-fixtures";
import { blockNodeId } from "../tree-drafts";
import { PlanFilterBody } from "./PlanFilterBody";

/**
 * The Plan view's chart drawn in slices (`drawn-in-slices.ts`), the plan's state table "The rows
 * drawn in slices" rows 4, 5, 6 and 10 for the chart's own keys, its address and the next page
 * its read answers. The chart binds its own `j` and `k` (`PlanFilterBody.tsx`'s `PlanChart`):
 * `j` on the last bar drawn and `k` on the first draw the next and the one before in the same
 * commit and move the focus there; an address that names a step not drawn draws every step up to
 * it, and the commit that draws it brings it under the header, the focus coming in the frame after
 * (`landOnRow`); and the steps a next page brings are drawn the first screen of them at once, the
 * rest in slices. The ordered steps are the outline's rows (`IssueTree.tsx`), which
 * `drawn-in-slices.test.tsx` holds.
 *
 * happy-dom lays nothing out, so the page is laid out by hand, each row 100 px tall in a window
 * 600 px tall under a header's band (`layoutByHand`), and the slices are tasks run by hand
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

/** The plan document's slug, which a task's node id and address name. */
const PLAN = "plan-md";

/** `count` plan tasks, `t00` on, each in its own place in the read's order and needing none. */
function tasksOf(count: number): TreeNode[] {
  return idsOf(count, "t").map((blockId, position) =>
    node({
      id: blockNodeId(PLAN, blockId),
      position,
      reference: `dispatch://${KEY}/artifact/${PLAN}#b-${blockId}`,
      text: `Task ${blockId}`,
      type: "plan_task",
    })
  );
}

/** The Plan view's chart over `plan`, every read answered, with `named` the task an address names;
 *  `redraw` draws it again over another plan or another address. */
function drawChart(
  plan: readonly TreeNode[],
  named?: string
): {
  readonly redraw: (next: readonly TreeNode[], address?: string) => void;
  readonly view: RenderResult;
} {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const draw = (shown: readonly TreeNode[], address: string | undefined) => {
    const root = node({ children: [...shown], held: shown.length, id: KEY, type: "issue" });
    const target = shown.find((one) => one.id === address);
    const published = publishedTree(root);
    return (
      <ReferenceNamesProvider>
        <MemoryRouter>
          <QueryClientProvider client={client}>
            <KeymapProvider>
              <MarginProvider>
                <FocusedNodeProvider>
                  <IssueScope />
                  <Publisher published={published} />
                  <ViewStage>
                    <PlanFilterBody
                      artifacts={new Map()}
                      context={new Set()}
                      emptyLine={EMPTY_STATE_LINES.plan}
                      focusReveal
                      holdsEscape={false}
                      isClosed={false}
                      namesLine={NO_FAILED_READS}
                      onEditing={() => {}}
                      plan={shown}
                      published={published}
                      reads={{ state: "answered" }}
                      reveal={target === undefined ? undefined : [root, target]}
                      root={root}
                      rootRow={false}
                    />
                  </ViewStage>
                </FocusedNodeProvider>
              </MarginProvider>
            </KeymapProvider>
          </QueryClientProvider>
        </MemoryRouter>
      </ReferenceNamesProvider>
    );
  };
  const view = render(draw(plan, named));
  return { redraw: (next, address) => view.rerender(draw(next, address)), view };
}

/** The tasks the chart draws a bar of in `container`, by their node ids, in order. */
function barsDrawn(container: HTMLElement): string[] {
  return [...container.querySelectorAll<HTMLElement>("[data-plan-bar-row]")].map(
    (row) => row.dataset.nodeId ?? ""
  );
}

// Row 4. `j` on the last bar drawn draws the next in the same commit and moves the focus there.
test("row 4: j on the last bar drawn draws the next and moves the focus there", async () => {
  const user = userEvent.setup();
  const plan = tasksOf(40);
  const ids = plan.map((one) => one.id);
  const { view } = drawChart(plan);
  try {
    await waitFor(() => expect(focusedRow(), "the chart took no focus").toBe(ids[0]));
    const first = barsDrawn(view.container);
    expect(first, "the first drawing drew every bar").not.toContain(ids[39]);
    for (let at = 1; at < first.length; at += 1) await user.keyboard("j");
    const last = first.at(-1) ?? "";
    expect(focusedRow(), "j did not reach the last bar drawn").toBe(last);
    const next = ids[ids.indexOf(last) + 1] ?? "";
    expect(barsDrawn(view.container), "the next bar was drawn already").not.toContain(next);
    await user.keyboard("j");
    expect(focusedRow(), "j on the last bar drawn did not move to the next").toBe(next);
    expect(tasks.pending(), "the bars were all drawn by then").toBe(1);
  } finally {
    view.unmount();
  }
});

// Row 5. `k` on the first bar drawn, with bars above it not drawn, draws the one above in the
// same commit and moves the focus there.
test("row 5: k on the first bar drawn, with bars above it not drawn, draws the one above", async () => {
  const user = userEvent.setup();
  const plan = tasksOf(40);
  const ids = plan.map((one) => one.id);
  const named = ids[30] ?? "";
  const { view } = drawChart(plan, named);
  try {
    await waitFor(() => expect(focusedRow(), "the address landed nowhere").toBe(named));
    const first = barsDrawn(view.container);
    expect(first, "the first drawing drew the first bar").not.toContain(ids[0]);
    for (let at = first.indexOf(named); at > 0; at -= 1) await user.keyboard("k");
    const top = first[0] ?? "";
    expect(focusedRow(), "k did not reach the first bar drawn").toBe(top);
    const above = ids[ids.indexOf(top) - 1] ?? "";
    await user.keyboard("k");
    expect(focusedRow(), "k on the first bar drawn did not move to the one above").toBe(above);
    expect(tasks.pending(), "the bars were all drawn by then").toBe(1);
  } finally {
    view.unmount();
  }
});

// Row 6. An address that names a step not drawn draws every bar up to it in one commit, whose
// layout effect brings it under the header; it takes the focus in the frame after.
test("row 6: an address that names a step not drawn draws every bar up to it and lands on it", async () => {
  const plan = tasksOf(40);
  const ids = plan.map((one) => one.id);
  const named = ids[30] ?? "";
  const chart = drawChart(plan);
  const { view } = chart;
  try {
    await waitFor(() => expect(focusedRow(), "the chart took no focus").toBe(ids[0]));
    expect(barsDrawn(view.container), "the named step was drawn at once").not.toContain(named);
    act(() => chart.redraw(plan, named));
    expect(
      barsDrawn(view.container).slice(0, 31),
      "the address drew the bars up to its own with one left out"
    ).toEqual(ids.slice(0, 31));
    const top = () =>
      layout.topOf(
        view.container.querySelector(`[data-plan-bar-row][data-node-id="${CSS.escape(named)}"]`)
      );
    const placed = top();
    expect(
      placed !== undefined && placed >= BAND_PX && placed < SCREEN_PX,
      `the commit that drew the named step left it at ${placed} px, not under the header`
    ).toBe(true);
    await waitFor(() => expect(focusedRow(), "the named step took no focus").toBe(named));
    expect(top(), "the focus moved the named step").toBe(placed);
    expect(tasks.pending(), "the bars were all drawn by then").toBe(1);
  } finally {
    view.unmount();
  }
});

// Row 10. The steps the view read's next page brings are drawn the first screen of them in the
// commit that brings them and the rest in slices; none is drawn before the page is read.
test("row 10: a next page's steps are drawn the first screen first, and none before it is read", async () => {
  const plan = tasksOf(60);
  const ids = plan.map((one) => one.id);
  const chart = drawChart(plan.slice(0, 10));
  const { view } = chart;
  try {
    await waitFor(() => expect(focusedRow(), "the chart took no focus").toBe(ids[0]));
    await tasks.runAll();
    expect(barsDrawn(view.container), "the first page was not drawn whole").toEqual(
      ids.slice(0, 10)
    );
    act(() => chart.redraw(plan));
    const paged = barsDrawn(view.container);
    expect(paged, "the next page drew no step at once").toContain(ids[10]);
    expect(paged, "the next page was drawn whole at once").not.toContain(ids[59]);
    expect(tasks.pending(), "the next page asked for no slice").toBe(1);
    await tasks.runAll();
    expect(barsDrawn(view.container), "the slices left steps not drawn").toEqual(ids);
  } finally {
    view.unmount();
  }
});
