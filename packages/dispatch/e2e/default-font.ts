/** A reader's own default font size, as the rows that read a page at a size other than the
 *  browser's own 16 px set it and hold the page to it. */
import { expect, type Page } from "@playwright/test";

/** A reader's own larger default font size, at which every `rem` is 20 px and every width a
 *  layout switches at moves with it. */
export const LARGE_FONT = 20;

/** Tailwind's `xl` in rem (`--breakpoint-xl` in its `theme.css`, `XL_BREAKPOINT` in
 *  `web/src/features/shell/useDialog.ts`): from it the shell draws its sidebar beside `<main>`,
 *  and under it the compact bar with its Menu, where every button takes the 44 px touch floor
 *  (`web/src/styles.css`). */
export const XL = 80;

/**
 * Sets `page`'s default font size to `px`, as the reader's own browser setting does, and waits
 * until the page it shows is drawn at that size (`expectRootFont`). Chromium's
 * `Page.setFontSizes`, so a row that calls it runs under `chromium` alone. The size holds for
 * every later load of the page, and a row holds each load to it with `expectRootFont`.
 */
export async function setDefaultFont(page: Page, px: number): Promise<void> {
  const cdp = await page.context().newCDPSession(page);
  await cdp.send("Page.setFontSizes", { fontSizes: { fixed: 13, standard: px } });
  await expectRootFont(page, px);
}

/** Holds the page `page` shows to a default font size of `px`: its root's computed font size,
 *  which is the px of every `rem`, since nothing in `styles.css` sizes the root. A row that reads
 *  at a size other than the browser's own calls it after each load, so a page drawn at 16 px
 *  cannot pass for one drawn at `px`. `at` names the case in the failure. */
export async function expectRootFont(page: Page, px: number, at?: string): Promise<void> {
  await expect
    .poll(() => page.evaluate(() => getComputedStyle(document.documentElement).fontSize), {
      message: at === undefined ? "the root's font size" : `${at}: the root's font size`,
    })
    .toBe(`${px}px`);
}
