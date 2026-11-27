import { expect, type Locator, type Page } from "@playwright/test";

import { hasReadingPane } from "./reading-pane";
import { settledState } from "./settled";

/**
 * The Inbox's reading column (INFRA-575) and, below its width, its reading view (INFRA-5200),
 * for the specs that drive the Inbox at whatever width their project gives them.
 *
 * The Inbox page draws short rows in `inbox-list` at every width. A short row keeps every
 * `data-inbox-*` attribute it had, its checkbox, its owner link, a kind badge, the question's
 * first line and its age. The row the reader picks shows its own surface - its document, or its
 * card - in `inbox-pane`, whose head carries that question's Snooze, its priority and its
 * "Assign to me". From `READING_PANE_WIDTH` (`e2e/reading-pane.ts`) that pane is a column beside
 * the list, a row is always selected, and the selected row carries `data-inbox-selected="true"`.
 * Below it a row the reader opens is read in `inbox-reading-view` in place of the list and its
 * header, which stay hidden until the view's own Inbox control (or the browser's Back) brings them
 * back at that
 * row. The drawer, at any width, is the list of cards it has always been: each row holds its own
 * card and its own controls.
 *
 * Each helper reads which of the two this page is drawing off the viewport, so one spec row runs
 * under `chromium` (1280 px, the column on) and `iphone` (390 px, the reading view).
 */

/** The list column, which is the whole Inbox where no row is open below the column's width. */
export function inboxList(page: Page): Locator {
  return page.getByTestId("inbox-list");
}

/** The open row's surface: the reading column, or below its width the reading view's body.
 *  Absent while no row is open, and in the drawer. */
export function inboxPane(page: Page): Locator {
  return page.getByTestId("inbox-pane");
}

/** Below the column's width, the open row's reading view: its Inbox control over `inboxPane`. */
export function readingView(page: Page): Locator {
  return page.getByTestId("inbox-reading-view");
}

/** The pane's one scroller, inside it: every flow draws what it shows in this box, and it is the
 *  focus stop the keyboard reaches - `l` hands it the focus, and Escape gives it back to the
 *  row. It is there wherever the pane is, and nowhere else. */
export function paneScroller(page: Page): Locator {
  return inboxPane(page).getByTestId("inbox-pane-scroller");
}

/** One question's row, at either width. */
export function inboxRow(page: Page, askId: string): Locator {
  return page.locator(`[data-inbox-row="${askId}"]`);
}

/** The row the reading column is showing. */
export function selectedRow(page: Page): Locator {
  return inboxList(page).locator('[data-inbox-selected="true"]');
}

/**
 * Picks a row the way the pointer does. The click lands on the row's age, the one part of a
 * short row that is neither the owner link nor a control, so it reaches the row itself whatever
 * the question's length leaves on the line. From the column's width it selects the row and
 * leaves the focus on it; below it, it opens the row's reading view in place of the list, back
 * to the list first if another row's view is open.
 */
export async function selectRow(page: Page, askId: string): Promise<void> {
  const row = inboxRow(page, askId);
  if (hasReadingPane(page)) {
    await row.locator("time").first().click();
    await expect(row).toHaveAttribute("data-inbox-selected", "true");
    return;
  }
  const open = readingView(page).and(page.locator(`[data-inbox-reading-row="${askId}"]`));
  const state = await settledState(`the Inbox before ${askId} is opened`, [
    ["open", open],
    ["another open", readingView(page)],
    ["list", row],
  ]);
  if (state === "open") return;
  if (state === "another open") await backToList(page);
  await row.locator("time").first().click();
  await expect(open).toBeVisible();
}

/** Below the column's width, presses the open reading view's own Inbox control, which brings the
 *  list back at the row it showed; nothing to do where the column stands beside the list. The
 *  view must be open: a row that finds the list instead has not reached the state it claims. */
export async function backToList(page: Page): Promise<void> {
  if (hasReadingPane(page)) return;
  await expect(readingView(page)).toBeVisible();
  await readingView(page)
    .getByRole("button", { exact: true, name: "Back to the Inbox list" })
    .click();
  await expect(readingView(page)).toHaveCount(0);
  await expect(inboxList(page)).toBeVisible();
}

/** The question's own card, in the pane of the row picked for it. */
export async function askCard(page: Page, askId: string): Promise<Locator> {
  await selectRow(page, askId);
  return inboxPane(page).getByTestId(`ask-${askId}`);
}

/** The box holding a question's own controls - its priority, "Assign to me", its Snooze and the
 *  owner link beside them: the pane's head, at either width. */
export async function askControls(page: Page, askId: string): Promise<Locator> {
  await selectRow(page, askId);
  return inboxPane(page).locator("header").first();
}

/** The box the Inbox's rows scroll in: the list column, which scrolls in itself beside the
 *  reading column, and the window where the list is the whole page. */
export function listScrollTop(page: Page): Promise<number> {
  return page.evaluate(() => {
    const list = document.querySelector<HTMLElement>('[data-testid="inbox-list"]');
    return list !== null && list.scrollHeight > list.clientHeight ? list.scrollTop : window.scrollY;
  });
}

/** Scrolls whichever box that is so the row's centre sits at the centre of what is visible. */
export async function centreRow(row: Locator): Promise<void> {
  await row.evaluate((node) => {
    const list = node.closest<HTMLElement>('[data-testid="inbox-list"]');
    const rect = node.getBoundingClientRect();
    if (list !== null && list.scrollHeight > list.clientHeight) {
      const box = list.getBoundingClientRect();
      list.scrollTop += rect.top + rect.height / 2 - (box.top + box.height / 2);
      return;
    }
    window.scrollBy(0, rect.top + rect.height / 2 - window.innerHeight / 2);
  });
}
