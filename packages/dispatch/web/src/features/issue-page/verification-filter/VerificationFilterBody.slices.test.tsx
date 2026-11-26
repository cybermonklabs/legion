import { afterEach, beforeAll, beforeEach, expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, type RenderResult, render, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import type { IssueCoverage } from "../../../api/types";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import { ReferenceNamesProvider } from "../../refs/reference-names";
import type { TreeNode } from "../issue-tree";
import { NoTooltips } from "../NoTooltips";
import { NO_FAILED_READS } from "../outline-reads";
import { presetView } from "../presets";
import { Publisher, publishedTree } from "../projection-fixture";
import {
  BAND_PX,
  idsOf,
  type LayoutByHand,
  layoutByHand,
  SCREEN_PX,
  type TasksByHand,
  tasksByHand,
} from "../slices-fixtures";
import { focusedRow } from "../slots/stage-fixtures";
import { VerificationFilterBody } from "./VerificationFilterBody";
import { ISSUE_KEY, node, statementNode } from "./verification-fixtures";

/**
 * The Verification view's rows drawn in slices (`drawn-in-slices.ts`), the plan's state table
 * "The rows drawn in slices" rows 4, 5, 6 and 10 for the view's own keys, its address and the
 * next page its read answers. The view binds its own `j` and `k` (`VerificationFilterBody`'s
 * `keyDown`): `j` on the last statement drawn and `k` on the first draw the next and the one
 * before in the same commit and move the focus there; an address that names a statement not drawn
 * draws every statement up to it, and the commit that draws it brings it under the header, the
 * focus coming in the frame after (`useAddressLanding`); and the statements a next page brings are
 * drawn the first screen of them at once, the rest in slices. The issue's row, which holds the
 * summary and the chips, is drawn at once, above whatever the slices draw.
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

/** `count` statements, `r00` on, each served by no check. */
function statementsOf(count: number): TreeNode[] {
  return idsOf(count, "r").map((block) => statementNode(block, `The page draws ${block}.`));
}

/** The coverage read over `statements`, each served by no check. */
function coverageOf(statements: readonly TreeNode[]): IssueCoverage {
  return {
    gaps: [],
    has_plan: false,
    issue: ISSUE_KEY,
    requirements: statements.map((one) => ({
      block_id: (one.reference ?? "").slice((one.reference ?? "").lastIndexOf("#b-") + 3),
      checks: 0,
      gaps: [],
      plan_tasks: 0,
      ref: one.reference ?? "",
      text: one.text,
    })),
    spec_artifact_id: "",
  };
}

/** The Verification view over `statements`, every read answered, with `reveal` the statement an
 *  address names; `redraw` draws it again over other statements or another address. */
function drawVerification(
  statements: readonly TreeNode[],
  reveal?: string
): {
  readonly redraw: (next: readonly TreeNode[], named?: string) => void;
  readonly view: RenderResult;
} {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const draw = (under: readonly TreeNode[], named: string | undefined) => {
    const issue = node({ children: [...under], id: ISSUE_KEY, text: "An issue", type: "issue" });
    const shown = presetView(
      issue,
      "verification",
      "",
      new Set(named === undefined ? [] : [named])
    );
    const published = publishedTree(issue);
    return (
      <QueryClientProvider client={client}>
        <ReferenceNamesProvider>
          <MemoryRouter>
            <NoTooltips>
              <Publisher published={published} />
              <VerificationFilterBody
                checks={[]}
                context={shown.context}
                coverage={coverageOf(under)}
                namesLine={NO_FAILED_READS}
                published={published}
                reads={{ state: "answered" }}
                readsSettled
                {...(named === undefined ? {} : { reveal: named })}
                view={shown.root}
                whole={undefined}
              />
            </NoTooltips>
          </MemoryRouter>
        </ReferenceNamesProvider>
      </QueryClientProvider>
    );
  };
  const view = render(draw(statements, reveal));
  return { redraw: (next, named) => view.rerender(draw(next, named)), view };
}

/** The statements the view draws in `container`, by their node ids, in order. */
function statementsDrawn(container: HTMLElement): string[] {
  return [...container.querySelectorAll<HTMLElement>("[data-verification-card]")].map(
    (row) => row.dataset.nodeId ?? ""
  );
}

/** The row of node `id` in `container`. */
function rowOf(container: HTMLElement, id: string): HTMLElement | null {
  return container.querySelector<HTMLElement>(`[data-verification-row="${CSS.escape(id)}"]`);
}

// Row 4. `j` on the last statement drawn draws the next in the same commit and moves the focus
// there.
test("row 4: j on the last statement drawn draws the next and moves the focus there", async () => {
  const user = userEvent.setup();
  const statements = statementsOf(40);
  const ids = statements.map((one) => one.id);
  const { view } = drawVerification(statements);
  try {
    const first = statementsDrawn(view.container);
    expect(first, "the first drawing drew no statement").toContain(ids[0]);
    expect(first, "the first drawing drew every statement").not.toContain(ids[39]);
    act(() => rowOf(view.container, ISSUE_KEY)?.focus());
    for (let at = 0; at < first.length; at += 1) await user.keyboard("j");
    const last = first.at(-1) ?? "";
    expect(focusedRow(), "j did not reach the last statement drawn").toBe(last);
    const next = ids[ids.indexOf(last) + 1] ?? "";
    expect(statementsDrawn(view.container), "the next statement was drawn already").not.toContain(
      next
    );
    await user.keyboard("j");
    expect(focusedRow(), "j on the last statement drawn did not move to the next").toBe(next);
    expect(tasks.pending(), "the statements were all drawn by then").toBe(1);
  } finally {
    view.unmount();
  }
});

// Row 5. `k` on the first statement drawn, with statements above it not drawn, draws the one
// above in the same commit and moves the focus there, not to the issue's row above it.
test("row 5: k on the first statement drawn, with statements above it not drawn, draws the one above", async () => {
  const user = userEvent.setup();
  const statements = statementsOf(40);
  const ids = statements.map((one) => one.id);
  const named = ids[30] ?? "";
  const { view } = drawVerification(statements, named);
  try {
    await waitFor(() => expect(focusedRow(), "the address landed nowhere").toBe(named));
    const first = statementsDrawn(view.container);
    expect(first, "the first drawing drew the first statement").not.toContain(ids[0]);
    for (let at = first.indexOf(named); at > 0; at -= 1) await user.keyboard("k");
    const top = first[0] ?? "";
    expect(focusedRow(), "k did not reach the first statement drawn").toBe(top);
    const above = ids[ids.indexOf(top) - 1] ?? "";
    await user.keyboard("k");
    expect(focusedRow(), "k on the first statement drawn did not move to the one above").toBe(
      above
    );
    expect(tasks.pending(), "the statements were all drawn by then").toBe(1);
  } finally {
    view.unmount();
  }
});

// Row 6. An address that names a statement not drawn draws every statement up to it in one
// commit, whose layout effect brings it under the header; it takes the focus in the frame after.
test("row 6: an address that names a statement not drawn draws every statement up to it and lands on it", async () => {
  const statements = statementsOf(40);
  const ids = statements.map((one) => one.id);
  const named = ids[30] ?? "";
  const verification = drawVerification(statements);
  const { view } = verification;
  try {
    expect(statementsDrawn(view.container), "the named statement was drawn at once").not.toContain(
      named
    );
    act(() => verification.redraw(statements, named));
    expect(
      statementsDrawn(view.container).slice(0, 31),
      "the address drew the statements up to its own with one left out"
    ).toEqual(ids.slice(0, 31));
    const top = () => layout.topOf(rowOf(view.container, named));
    const placed = top();
    expect(
      placed !== undefined && placed >= BAND_PX && placed < SCREEN_PX,
      `the commit that drew the named statement left it at ${placed} px, not under the header`
    ).toBe(true);
    await waitFor(() => expect(focusedRow(), "the named statement took no focus").toBe(named));
    expect(top(), "the focus moved the named statement").toBe(placed);
    expect(tasks.pending(), "the statements were all drawn by then").toBe(1);
  } finally {
    view.unmount();
  }
});

// Row 10. The statements the view read's next page brings are drawn the first screen of them in
// the commit that brings them and the rest in slices; none is drawn before the page is read.
test("row 10: a next page's statements are drawn the first screen first, and none before it is read", async () => {
  const statements = statementsOf(60);
  const ids = statements.map((one) => one.id);
  const verification = drawVerification(statements.slice(0, 10));
  const { view } = verification;
  try {
    await tasks.runAll();
    expect(statementsDrawn(view.container), "the first page was not drawn whole").toEqual(
      ids.slice(0, 10)
    );
    act(() => verification.redraw(statements));
    const paged = statementsDrawn(view.container);
    expect(paged, "the next page drew no statement at once").toContain(ids[10]);
    expect(paged, "the next page was drawn whole at once").not.toContain(ids[59]);
    expect(tasks.pending(), "the next page asked for no slice").toBe(1);
    await tasks.runAll();
    expect(statementsDrawn(view.container), "the slices left statements not drawn").toEqual(ids);
  } finally {
    view.unmount();
  }
});
