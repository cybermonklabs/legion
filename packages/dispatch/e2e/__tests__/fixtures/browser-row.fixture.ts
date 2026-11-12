/** The other negative control of e2e/__tests__/no-browser-rows.test.ts: a row that does take
 *  `{ page }`. It serves twice. `unexplainedRows` must leave it alone, which is what shows that
 *  check names the row it should and not every row it reads; and an exemption naming it is an
 *  exemption for a row that drives a browser, which `staleExemptions` must report stale.
 *
 * Its name ends `.fixture.ts` and not `.e2e.ts` because e2e/playwright.config.ts says
 * `testDir: "."` and `testMatch: /.*\.e2e\.ts/`: Playwright walks the `e2e` directory
 * recursively, so a `.e2e.ts` file here, under `__tests__/fixtures/`, would join the real suite
 * and run - and this row would then open a page against a harness that is not there. `bun test`
 * collects only `*.test.ts` and its kin, so this file runs nowhere; the test reads its text and
 * parses it.
 */

import { expect, test } from "@playwright/test";

test("a planted row that opens a page drives a browser", async ({ page }) => {
  await page.goto("/");
  await expect(page).toHaveTitle(/./);
});
