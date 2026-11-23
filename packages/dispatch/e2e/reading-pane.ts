/** The width the Inbox page draws its reading column from, in a file that imports no runtime
 *  module: `e2e/layout-routes.ts`, which the harness's own unit tests read without a browser,
 *  takes it from here as `e2e/inbox-pane.ts` does. */
import type { Page } from "@playwright/test";

/** The width the Inbox draws its reading column from (`READING_PANE_QUERY`, 80rem). */
export const READING_PANE_WIDTH = 1280;

/** Whether this page is wide enough for the Inbox page's reading column. */
export function hasReadingPane(page: Page): boolean {
  return (page.viewportSize()?.width ?? 0) >= READING_PANE_WIDTH;
}
