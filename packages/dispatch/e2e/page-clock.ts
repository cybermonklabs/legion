import type { Page } from "@playwright/test";

/** Holds the page's installed clock still (`page.clock.install()` first): `install()` alone
 *  leaves it running at real speed, and `pauseAt` is what stops it, so from here only a
 *  `runFor`, a `fastForward` or a `resume` spends page time. `pauseAt` takes a time on the page's
 *  own clock, which a jump the row made before can have carried past the Node process's own
 *  `Date.now()`, and a clock refuses to move backwards, so the lead of a second is taken from the
 *  page's reading of its clock. It runs every page timer that falls due inside that second, so a
 *  row calls it only where no timer it must not fire is armed. */
export async function pauseClock(page: Page): Promise<void> {
  await page.clock.pauseAt((await page.evaluate(() => Date.now())) + 1000);
}
