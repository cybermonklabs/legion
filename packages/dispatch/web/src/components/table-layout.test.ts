import { afterEach, expect, test } from "bun:test";
import type { SortingState } from "@tanstack/react-table";
import { act, renderHook } from "@testing-library/react";
import { type LayoutColumns, reorder, SORT_MARK_WIDTH, useTableLayout } from "./table-layout";

test("a dropped column lands immediately before the one it was dropped on", () => {
  const order = ["select", "key", "title", "status", "epic"];
  expect(reorder(order, "epic", "title")).toEqual(["select", "key", "epic", "title", "status"]);
  expect(reorder(order, "key", "epic")).toEqual(["select", "title", "status", "key", "epic"]);
  expect(order).toEqual(["select", "key", "title", "status", "epic"]);
});

test("a column dropped on itself, or on one the order does not hold, stays where it is", () => {
  const order = ["select", "key", "title"];
  expect(reorder(order, "title", "title")).toEqual(order);
  expect(reorder(order, "title", "claim")).toEqual(order);
  expect(reorder(order, "claim", "title")).toEqual(order);
});

/** The Issues tab's pins at the registry's widths: Select, Priority, Key and Title within 60% of
 *  a wide box, Title giving up width first; on a phone Select and Key, and Title after them.
 *  `keepDragged` is the caller's choice: the Issues tab's tables take it, the priority pages'
 *  do not (R10, R13). */
function issuesLayout(keepDragged: boolean, phonePins: readonly string[]): LayoutColumns {
  return {
    ids: ["select", "priority", "key", "title", "status"],
    rowAttribute: "data-layout-row",
    sizes: { select: 44, priority: 132, key: 124, title: 380, status: 150 },
    minimums: { title: 240 },
    pins: {
      wide: { columns: ["select", "priority", "key", "title"], share: 0.6, keepDragged },
      phone: { columns: phonePins, share: 0.99, widths: { key: 124 }, flexibleMin: 120 },
      flexible: "title",
    },
  };
}
const KEEPS_DRAGGED = issuesLayout(true, ["select", "key"]);
const NARROWS = issuesLayout(false, ["select", "key"]);
const PHONE_PINS_TITLE = issuesLayout(false, ["select", "key", "title"]);

const unmounts: (() => void)[] = [];
afterEach(() => {
  for (const unmount of unmounts.splice(0)) unmount();
  localStorage.clear();
});

/** The layout `layout` draws in a box `boxWidth` wide, with `sizing` the widths the reader
 *  stored and `sorting` the table's sort, under a key of its own. The unit DOM lays nothing out,
 *  so the box reports the width itself. */
function layoutIn(
  layout: LayoutColumns,
  boxWidth: number,
  sizing: Readonly<Record<string, number>> = {},
  sorting: SortingState = []
) {
  const key = `layout-test-${unmounts.length}`;
  localStorage.setItem(key, JSON.stringify({ sizing }));
  const hook = renderHook(() => useTableLayout(key, layout, sorting));
  unmounts.push(hook.unmount);
  const box = document.createElement("div");
  box.getBoundingClientRect = () => new DOMRect(0, 0, boxWidth, 400);
  act(() => hook.result.current.attachBox(box));
  expect(hook.result.current.boxWidth).toBe(boxWidth);
  return hook.result.current;
}

/** The widths and pins `layout` draws in a box `boxWidth` wide (`layoutIn`). */
function drawnIn(
  layout: LayoutColumns,
  boxWidth: number,
  sizing: Readonly<Record<string, number>> = {}
) {
  return layoutIn(layout, boxWidth, sizing).responsive;
}

// k12 (R10): a Title the reader widened by 300 px is drawn at its stored width. In a 1400 px box
// the pins would then cover 980 px, past their share of 840, so Title scrolls with the table.
test("with keepDragged a Title the reader widened keeps its width, and unpins past the share", () => {
  const widened = drawnIn(KEEPS_DRAGGED, 1400, { title: 380 + 300 });
  expect(widened.sizing.title).toBe(680);
  expect(widened.pinned).toEqual(["select", "priority", "key"]);

  // A width that leaves the pins within their share keeps Title pinned.
  const within = drawnIn(KEEPS_DRAGGED, 1400, { title: 480 });
  expect(within.sizing.title).toBe(480);
  expect(within.pinned).toEqual(["select", "priority", "key", "title"]);

  // Until the reader drags Title, it still gives way first: 600 px of a 1000 px box, less the
  // 300 the other pins take.
  const undragged = drawnIn(KEEPS_DRAGGED, 1000, { status: 200 });
  expect(undragged.sizing.title).toBeCloseTo(300);
  expect(undragged.pinned).toEqual(["select", "priority", "key", "title"]);
});

// R13: the priority pages' tables keep today's rule, by which the pins' share caps Title.
test("without keepDragged a Title the reader widened narrows to keep the pins within their share", () => {
  const capped = drawnIn(NARROWS, 1400, { title: 680 });
  expect(capped.sizing.title).toBeCloseTo(840 - 44 - 132 - 124);
  expect(capped.pinned).toEqual(["select", "priority", "key", "title"]);
});

// R10: on a phone Select and Key are pinned, and Title, which scrolls, takes all they leave of
// the box's share, whatever its own width.
test("on a phone a flexible column the phone does not pin fills the rest of the row", () => {
  for (const [boxWidth, sizing] of [
    [390, {}],
    [600, {}],
    [600, { title: 200 }],
  ] as const) {
    const phone = drawnIn(KEEPS_DRAGGED, boxWidth, sizing);
    expect(phone.pinned).toEqual(["select", "key"]);
    expect(phone.sizing.key).toBe(124);
    expect(phone.sizing.title).toBeCloseTo(boxWidth * 0.99 - 44 - 124);
  }
  // A flexible column the phone pins takes what the others leave only up to its own width.
  const pinnedTitle = drawnIn(PHONE_PINS_TITLE, 600);
  expect(pinnedTitle.pinned).toEqual(["select", "key", "title"]);
  expect(pinnedTitle.sizing.title).toBe(380);
});

// dispatch://INFRA-3221/comment/141bbad2-b75e-4bb4-b0a7-b5b1af541a3c: while the table is sorted
// by a column, the column's minimum holds its header's sort mark too. In an 800 px box the pins'
// share is 480 px, which leaves Title 180 of it, under its minimum: Title stops at 240 px, and
// at 240 px and the mark while the table is sorted by it. The minimums the layout hands its caller
// say the same, and a sort by a column the caller names a sorted minimum for widens nothing.
test("while the table is sorted by a column, its minimum holds the sort mark too", () => {
  const sorted = layoutIn(NARROWS, 800, {}, [{ id: "title", desc: false }]);
  expect(sorted.responsive.sizing.title).toBe(240 + SORT_MARK_WIDTH);
  expect(sorted.minimums).toEqual({ title: 240 + SORT_MARK_WIDTH });

  const unsorted = layoutIn(NARROWS, 800);
  expect(unsorted.responsive.sizing.title).toBe(240);
  const own = NARROWS.minimums;
  if (own === undefined) throw new Error("NARROWS names its minimums");
  expect(unsorted.minimums).toBe(own);

  const holds = { ...NARROWS, sortedMinimums: { title: 240 } };
  const named = layoutIn(holds, 800, {}, [{ id: "title", desc: true }]);
  expect(named.responsive.sizing.title).toBe(240);
  expect(named.minimums).toEqual({ title: 240 });
});
