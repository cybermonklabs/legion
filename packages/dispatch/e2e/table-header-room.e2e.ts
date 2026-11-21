/** The room a shared table's header cell gives its controls, read on the Delivery list
 *  (INFRA-2160's review round 1, thread PRRT_kwDORFy7ds66U3XJ). Each column's width there
 *  budgets its label, its sort mark, its filter and its padding, and no more. A column's Move
 *  control is drawn out of the header's flow (`TableHeaderCell` in `components/DataTable.tsx`),
 *  so it takes none of that room: it hangs from the cell's bottom edge, shows while the pointer
 *  is over the cell or the focus is on it, and is transparent otherwise. Drawn in the flow, it
 *  pushed Parent agent's filter under the Author header at 1100 px, where every button is 44 px
 *  wide, and took the whole of Rework's sort button, the column's name and its drag handle, at
 *  1440 px. */
import { seedDeliveryFixture, TIMELINE_FIXTURE_QUERY } from "./delivery-seed";
import { expect, type Locator, type Page, test } from "./test";

/** A box as Playwright measures one, in the page's px. */
interface Box {
  readonly height: number;
  readonly width: number;
  readonly x: number;
  readonly y: number;
}

/** The Delivery list of `seedDeliveryFixture`'s two pull requests in a window `width` px wide,
 *  read once both rows are drawn, which draws the header cells with them. */
async function openList(page: Page, width: number, height: number): Promise<Locator> {
  await page.setViewportSize({ height, width });
  await seedDeliveryFixture();
  await page.goto(`/delivery?${TIMELINE_FIXTURE_QUERY}&mode=list`);
  const table = page.getByRole("table", { name: "Merged pull requests" });
  await expect(table.locator("tbody tr[data-pr-row]")).toHaveCount(2);
  return table;
}

/** The header cell of the column named `column`, found by its grip. A `has` locator is read
 *  inside each header, so it starts from the page, not from the table. */
function headerOf(page: Page, table: Locator, column: string): Locator {
  return table.getByRole("columnheader").filter({
    has: page.getByRole("separator", { exact: true, name: `Resize ${column}` }),
  });
}

/** How wide `control` is drawn, to the px; 0 where it is not drawn. */
async function widthOf(control: Locator): Promise<number> {
  return Math.round((await control.boundingBox())?.width ?? 0);
}

/** Whether two boxes share no area, to the half px a layout rounds to. A box that is not drawn
 *  is no answer, so it reads as sharing. */
function apart(a: Box | null, b: Box | null): boolean {
  if (a === null || b === null) return false;
  return (
    a.x + a.width <= b.x + 0.5 ||
    b.x + b.width <= a.x + 0.5 ||
    a.y + a.height <= b.y + 0.5 ||
    b.y + b.height <= a.y + 0.5
  );
}

test("the Move control takes no header room: at 1100x800 a click on the Delivery list's Parent agent filter reaches the filter", async ({
  page,
}) => {
  const table = await openList(page, 1100, 800);
  const filter = table.getByRole("button", { exact: true, name: "Filter Parent agent" });
  // The room the thread counts: a 112 px column whose filter, as every button under 1280 px,
  // is 44 px wide (`styles.css`), which a Move control of 44 px beside it would overflow.
  await expect
    .poll(() => widthOf(headerOf(page, table, "Parent agent")), {
      message: "Parent agent is drawn at its 112 px",
    })
    .toBe(112);
  await expect
    .poll(() => widthOf(filter), { message: "the filter is 44 px wide below 1280 px" })
    .toBe(44);
  // The click waits until the press would land on the filter itself. With the Move control in
  // the header's flow the filter stood past the cell's right edge, under the Author header,
  // which took every press.
  await filter.click();
  await expect(
    page.getByRole("combobox", { exact: true, name: "Search Parent agent" }),
    "the press opens the filter"
  ).toBeVisible();
  await page.keyboard.press("Escape");
});

test("the Move control takes no header room: at 1440x900 the Delivery list's Rework sort keeps its width and its press, and the Move control shows on focus and on hover", async ({
  page,
}) => {
  const table = await openList(page, 1440, 900);
  const header = headerOf(page, table, "Rework");
  const sort = table.getByRole("button", { exact: true, name: "Rework" });
  const filter = table.getByRole("button", { exact: true, name: "Filter Rework" });
  const move = table.getByRole("button", { exact: true, name: "Move Rework" });
  // The room the thread counts: a 72 px column whose filter is 32 px wide from 1280 px up, and
  // whose sort button takes what the filter leaves.
  await expect.poll(() => widthOf(header), { message: "Rework is drawn at its 72 px" }).toBe(72);
  await expect
    .poll(() => widthOf(filter), { message: "the filter is 32 px wide from 1280 px up" })
    .toBe(32);
  await expect
    .poll(() => widthOf(sort), { message: "the Rework sort button keeps a width" })
    .toBeGreaterThan(0);

  // At rest the Move control is transparent; Tab from the sort reaches it, and it shows.
  await expect(move, "the Move control is transparent at rest").toHaveCSS("opacity", "0");
  await sort.focus();
  await page.keyboard.press("Tab");
  await expect(move, "Tab from the sort reaches the Move control").toBeFocused();
  await expect(move, "the focused Move control shows").toHaveCSS("opacity", "1");
  await page.keyboard.press("Shift+Tab");
  await expect(sort).toBeFocused();
  await expect(move, "the Move control is transparent once the focus leaves").toHaveCSS(
    "opacity",
    "0"
  );

  // The pointer over the header shows it too, clear of the sort and the filter.
  await header.hover();
  await expect(move, "the Move control shows under the pointer").toHaveCSS("opacity", "1");
  // The wait above read the Move control shown, and a hover moves no box, so the three boxes
  // are read at rest.
  const [moveBox, sortBox, filterBox] = await Promise.all(
    [move, sort, filter].map((control) => control.boundingBox())
  );
  expect(apart(moveBox, sortBox), "the shown Move control covers no part of the sort").toBe(true);
  expect(apart(moveBox, filterBox), "the shown Move control covers no part of the filter").toBe(
    true
  );
  await sort.click();
  await expect(header, "a press on the sort sorts the column").toHaveAttribute(
    "aria-sort",
    "ascending"
  );
});
