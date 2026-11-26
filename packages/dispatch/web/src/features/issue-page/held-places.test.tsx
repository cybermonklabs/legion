import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import { DESKTOP_WIDTH, PHONE_WIDTH, stubViewport } from "../margin/margin-fixture";
import { KeymapProvider } from "../shell/KeymapProvider";
import {
  decisionsRead,
  issueDetails,
  KEY,
  SPEC_DOCUMENT,
  SPEC_SLUG,
} from "./decisions-filter/decisions-fixtures";
import { presetPath } from "./issue-address";
import { TWO_THREADS } from "./outline-fixtures";
import { PRESET_NAMES, PRESET_ORDER, type Preset } from "./page-words";
import { ProvidersSlot } from "./slots/providers";
import { ViewRowSlot } from "./slots/view-row";
import { commentNode, treeAnswer, treeNode } from "./tree-read/tree-fixtures";
import type { TreeAnswer } from "./tree-read/tree-read";

/**
 * The places the page holds before their reads answer (R3, "The places held" of the plan), row
 * 1: each view control's count. The view row draws its controls at once and each count only once
 * the reads it is counted from have answered (`slots/view-row.tsx`), so a count drawn into no
 * place held for it widens its control and moves every control after it: on INFRA-1196 a press
 * on Decisions during the load opened Plan (decision 500 (1)). Each control holds a slot for its
 * count from its first drawing, of the width the count will need, and draws the count into that
 * slot.
 *
 * A unit test's DOM lays nothing out, so here a control's box is what decides it: the control
 * itself, its classes and its style, and those of each count slot it holds. The boxes the browser
 * lays out are read by row 1 of `c-load-1440` and `c-load-390` (`e2e/issue-page-load.e2e.ts`).
 */

/** The view row's controls whose boxes R3 holds, as the load rows record them (`BOX_GROUPS` of
 *  `e2e/load-instruments.ts`): each toggle, each count's label and, below 1280 px, the one
 *  control. */
const CONTROLS = [
  '[data-testid="issue-presets"] button',
  '[data-testid="issue-presets"] label',
  '[data-filter-row] button[aria-haspopup="menu"]',
].join(", ");

/** The mark of the slot a control holds for its count, by the view the count is drawn for. */
const COUNT_SLOT = "data-count-slot";

/** The tree read's answer for the fixtures' issue: the issue, the spec's two statements, and the
 *  two threads the reader began, so that every view but Graph has a count to draw. */
const TREE: TreeAnswer = treeAnswer([
  treeNode(KEY, "issue", null),
  ...["r1", "r2"].map((id, position) =>
    treeNode(id, "requirement", KEY, {
      document: { artifact_id: SPEC_DOCUMENT.id, position, slug: SPEC_SLUG },
    })
  ),
  ...TWO_THREADS.map((comment) => commentNode(comment)),
]);

/** Answers every read the row reaches, but the tree read, which it holds until `answer` is
 *  called: the read every count is counted from (`tree` is its stub). */
function holdTheTreeRead() {
  const held = Promise.withResolvers<TreeAnswer>();
  const tree = spyOn(api, "getIssueTree").mockReturnValue(held.promise);
  const spies = [
    tree,
    spyOn(api, "listIssueAskThreads").mockResolvedValue([] as never),
    spyOn(api, "listComments").mockResolvedValue(TWO_THREADS as never),
    spyOn(api, "getIssueCoverage").mockResolvedValue({ requirements: [] } as never),
    spyOn(api, "getIssueDecisions").mockResolvedValue(decisionsRead([])),
    spyOn(api, "listIssueMessages").mockResolvedValue([] as never),
    spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never),
    spyOn(api, "getIssueResults").mockResolvedValue({ checks: [] } as never),
    spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "alice" } as never),
  ];
  return {
    answer: () => held.resolve(TREE),
    restore: () => {
      for (const spy of spies) spy.mockRestore();
    },
    tree,
  };
}

/** The view row of the fixtures' issue at `preset`'s own address, under the page's providers,
 *  with the route the page reads from that address. */
function renderRowAt(client: QueryClient, preset: Preset) {
  const issue = issueDetails();
  const route =
    preset === "conversation"
      ? ({ key: issue.key, kind: "issue" } as const)
      : ({ key: issue.key, kind: preset } as const);
  return render(
    <MemoryRouter initialEntries={[presetPath(KEY, preset)]}>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <ProvidersSlot issue={issue} route={route}>
            <ViewRowSlot issue={issue} route={route} />
          </ProvidersSlot>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** What decides one control's box where nothing is laid out. */
interface HeldBox {
  readonly control: HTMLElement;
  readonly drawn: {
    readonly className: string;
    readonly slots: readonly { readonly className: string; readonly style: string | null }[];
    readonly style: string | null;
  };
  readonly slots: readonly HTMLElement[];
}

/** Each control of the view row in `container`, with its classes, its style, and the classes and
 *  style of each count slot it holds. */
function heldBoxes(container: HTMLElement): HeldBox[] {
  return [...container.querySelectorAll<HTMLElement>(CONTROLS)].map((control) => {
    const slots = [...control.querySelectorAll<HTMLElement>(`[${COUNT_SLOT}]`)];
    return {
      control,
      drawn: {
        className: control.className,
        slots: slots.map((slot) => ({
          className: slot.className,
          style: slot.getAttribute("style"),
        })),
        style: control.getAttribute("style"),
      },
      slots,
    };
  });
}

/** The width in `ch` a count slot holds, from its own style; NaN where it holds none. */
function heldWidth(slot: HTMLElement): number {
  return Number(/(\d+(?:\.\d+)?)ch\b/u.exec(slot.getAttribute("style") ?? "")?.[1]);
}

/** The widths the row is drawn at, each by the shape it draws there: the row of five from 1280
 *  px; the one control, as wide as what it draws, from 761 to 1279 px; and the one control filling
 *  the row at 760 px and below. */
const WIDTHS = [
  { name: "the row of five at 1440 px", width: DESKTOP_WIDTH },
  { name: "the one control at 1024 px", width: 1024 },
  { name: "the one control at 390 px", width: PHONE_WIDTH },
] as const;

/** The views whose one control draws a count when the row is drawn alone: every view but Graph,
 *  which has none (R10), and Spec, whose outline read the row does not send - the Contents pane
 *  beside the view or the Outline sheet sends it (`tree-read.ts`'s `OutlineChoice.sends`) - so
 *  Spec's count is held by the row of five here. */
const COUNTED = PRESET_ORDER.filter((preset) => preset !== "graph" && preset !== "spec");

test("each view control's box is the same before and after its count's read answers", async () => {
  for (const { name, width } of WIDTHS) {
    const restoreMatchMedia = stubViewport(width);
    try {
      // The row of five draws every view's control on any view's address, and the one control
      // the view that is on, so the one control is drawn on each view that draws a count.
      for (const preset of width === DESKTOP_WIDTH ? (["conversation"] as const) : COUNTED) {
        const where = `${name} with ${PRESET_NAMES[preset]} on`;
        // As the page's tests take one: no read tried again, and none read again by its age.
        const client = new QueryClient({
          defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
        });
        const reads = holdTheTreeRead();
        const view = renderRowAt(client, preset);
        try {
          await waitFor(() => {
            expect(reads.tree, `the tree was never read ${where}`).toHaveBeenCalled();
            expect(client.isFetching(), `a read other than the tree's is still out ${where}`).toBe(
              1
            );
          });
          expect(
            view.container.querySelector("[data-preset-count]"),
            `the row drew a count with the tree read out ${where}`
          ).toBeNull();
          const before = heldBoxes(view.container);
          expect(before.length, `the row drew no control ${where}`).toBeGreaterThan(0);

          await act(async () => reads.answer());
          const counts = await waitFor(() => {
            const drawn = [...view.container.querySelectorAll<HTMLElement>("[data-preset-count]")];
            // Every control but Graph's in the row of five; the one control's own.
            expect(drawn.length, `the row drew no count once the tree read answered ${where}`).toBe(
              width === DESKTOP_WIDTH ? PRESET_ORDER.length - 1 : 1
            );
            return drawn;
          });

          const after = heldBoxes(view.container);
          expect(
            after.map((box) => box.drawn),
            `a control's classes or style changed when its count's read answered ${where}`
          ).toEqual(before.map((box) => box.drawn));
          expect(
            after.every((box, at) => box.control === before[at]?.control),
            `a control was drawn again as another element when its count's read answered ${where}`
          ).toBe(true);
          const held = new Set(before.flatMap((box) => box.slots));
          for (const count of counts) {
            const of = count.dataset.presetCount ?? "";
            const slot = count.closest<HTMLElement>(`[${COUNT_SLOT}]`);
            expect(
              slot !== null && held.has(slot) && slot.getAttribute(COUNT_SLOT) === of,
              `${of}'s count is drawn in no slot its control held for it before its read answered ${where}`
            ).toBe(true);
            if (slot === null) continue;
            expect(
              (slot.textContent ?? "").length,
              `${of}'s slot holds less room than the count drawn in it (${heldWidth(slot)}ch) ${where}`
            ).toBeLessThanOrEqual(heldWidth(slot));
          }
        } finally {
          view.unmount();
          client.clear();
          reads.restore();
        }
      }
    } finally {
      restoreMatchMedia();
    }
  }
});
