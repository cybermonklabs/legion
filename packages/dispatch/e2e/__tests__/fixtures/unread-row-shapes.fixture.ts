/** A planted control of e2e/__tests__/no-browser-rows.test.ts: the two row shapes with one half
 *  written in the call and one half not, which a reading that wanted both a literal title and
 *  an inline callback dropped without a word. One row is titled from a variable; the other's
 *  callback is declared beside it and passed by name. Neither binds a browser fixture, so
 *  `rowsOfFiles` must report both and `unexplainedRows` must name both. A reading that skipped
 *  them would let past the one thing the rule is about.
 *
 * Its name ends `.fixture.ts` and not `.e2e.ts` because e2e/playwright.config.ts says
 * `testDir: "."` and `testMatch: /.*\.e2e\.ts/`: Playwright walks the `e2e` directory
 * recursively, so a `.e2e.ts` file here, under `__tests__/fixtures/`, would join the real suite
 * and run. `bun test` collects only `*.test.ts` and its kin, so this file runs nowhere; the test
 * reads its text and parses it.
 */

import { expect, test } from "@playwright/test";

/** The first row's title, computed so that the source alone cannot say what it reads. */
const computedTitle = ["a planted row whose title", "is computed"].join(" ");

test(computedTitle, async () => {
  expect(await Promise.resolve("the layer that owns this claim")).toContain("claim");
});

/** The second row's callback, declared here rather than written in the call, so that the row's
 *  fixtures are read from the declaration the name is bound to. It takes none, which is why the
 *  row drives no browser. */
async function offTheBrowser(): Promise<void> {
  expect(await Promise.resolve("the layer that owns this claim")).toContain("claim");
}

test("a planted row whose callback is declared beside it", offTheBrowser);
