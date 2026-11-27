import type { Locator, Page } from "@playwright/test";

import type { MeInboxResponse } from "../web/src/api/types";
import { isDismissedPage, pageIds } from "./dismissed-seed";
import { inboxList } from "./inbox-pane";

/**
 * What R7's two spec files read of the Dismissed view: `e2e/inbox-dismissed-timing.e2e.ts`, whose
 * row times the view over thousands dismissed in `chromium-alone`, and
 * `e2e/inbox-dismissed-window.e2e.ts`, whose row holds the window to the focus rules in
 * `chromium`. The reads are here so that neither spec file imports the other.
 */

/** The size the view is measured at: wide enough for the reading column, beside which the list
 *  column scrolls in itself. */
export const WIDE = { height: 900, width: 1440 };

/** The Dismissed view's own address. */
const DISMISSED = "/?show=dismissed";

/** What one opening of the Dismissed view read. */
export interface Opening {
  /** The size in bytes of the first page's answer. */
  readonly bytes: number;
  /** The cursor the first page answered for the next one. */
  readonly cursor: string | undefined;
  /** The row ids the first page answered, in its order. */
  readonly ids: readonly string[];
  /** The time from the navigation to the first drawn row, in milliseconds. */
  readonly ms: number;
}

/** Opens the Dismissed view afresh and reads the time from the navigation to its first drawn row
 *  and the first page's answer. It asserts nothing, so a build that fails later still prints
 *  these readings first. The page leaves what it showed first, so no read the old page still has
 *  out, as a live update makes one, answers for the new page's or runs beside it. The time is
 *  what `noteFirstRow` (`e2e/inbox-dismissed-timing.e2e.ts`) noted on the page, and `NaN` on a
 *  page it did not prepare. */
export async function openDismissed(page: Page): Promise<Opening> {
  await page.goto("about:blank");
  const answered = page.waitForResponse((response) => isDismissedPage(response));
  await page.goto(DISMISSED);
  const response = await answered;
  const body = await response.body();
  await inboxList(page).locator("[data-inbox-row]").first().waitFor();
  const ms = await page.evaluate(() =>
    Number(document.documentElement.dataset.firstInboxRowAt ?? Number.NaN)
  );
  const answer = JSON.parse(body.toString("utf8")) as MeInboxResponse;
  return {
    bytes: body.byteLength,
    cursor: answer.next_cursor ?? undefined,
    ids: pageIds(answer),
    ms,
  };
}

/** One row of `within`'s list, by the id its `data-inbox-row` names. */
export function row(within: Page | Locator, id: string): Locator {
  return inboxList(within).locator(`[data-inbox-row="${id}"]`);
}

/** What the list draws now in `box`, the box its rows scroll in: the ids of the rows it draws, in
 *  order; how many of them the box shows, any part of a row in sight counting; and how many rows
 *  the view has loaded, which each drawn row says (`aria-setsize`). */
export function readWindow(
  box: Locator
): Promise<{ drawn: string[]; loaded: number; shown: number }> {
  return box.evaluate((scroller) => {
    const sight = scroller.getBoundingClientRect();
    const top = Math.max(sight.top, 0);
    const bottom = Math.min(sight.bottom, window.innerHeight);
    const rows = [...scroller.querySelectorAll<HTMLElement>("[data-inbox-row]")];
    const shown = rows.filter((each) => {
      const drawn = each.getBoundingClientRect();
      return drawn.bottom > top && drawn.top < bottom;
    });
    return {
      drawn: rows.map((each) => each.dataset.inboxRow ?? ""),
      loaded: Number(rows[0]?.getAttribute("aria-setsize") ?? 0),
      shown: shown.length,
    };
  });
}
