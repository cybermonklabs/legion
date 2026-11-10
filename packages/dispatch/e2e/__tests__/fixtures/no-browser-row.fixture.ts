/** One of the two negative controls of e2e/__tests__/no-browser-rows.test.ts: a row that takes no
 *  fixture at all, which `unexplainedRows` must name. Without it, a check that finds nothing in
 *  the real suite would read the same whether it looked or not, and proving it looks must not
 *  need an edit of a real spec.
 *
 * Its name ends `.fixture.ts` and not `.e2e.ts` because e2e/playwright.config.ts says
 * `testDir: "."` and `testMatch: /.*\.e2e\.ts/`: Playwright walks the `e2e` directory
 * recursively, so a `.e2e.ts` file here, under `__tests__/fixtures/`, would join the real suite
 * and run. `bun test` collects only `*.test.ts` and its kin, so this file runs nowhere; the test
 * reads its text and parses it.
 */

import { expect, test } from "@playwright/test";

test("a planted row that takes no fixture drives no browser", async () => {
  expect(await Promise.resolve("the layer that owns this claim")).toContain("claim");
});
