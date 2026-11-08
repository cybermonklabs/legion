/** The planted shape of e2e/__tests__/no-browser-rows.test.ts with NOTHING written in the call:
 *  a row whose title and whose callback are both passed by name. Playwright runs it like any
 *  other row, and it binds no browser fixture, so `rowsOfFiles` must report it and
 *  `unexplainedRows` must name it.
 *
 * `unread-row-shapes.fixture.ts` holds the two shapes with one unreadable half each — a computed
 * title with an inline callback, and a literal title with a callback passed by name. Here both
 * halves are names, which is the shape a reading that wanted a literal title or an inline
 * function had nothing of at all: it reported no row, and a row that drives no browser passed
 * the gate in silence.
 *
 * What the scan reports it from is its callee — `test` takes a title first and nothing else
 * (@playwright/test 1.64.1, `types/test.d.ts`) — and its callback is the function the second
 * name is bound to, read where that name is bound. The title stays what the source says, the
 * expression's own text, since no reader of the source can evaluate it.
 *
 * Its name ends `.fixture.ts` and not `.e2e.ts` because e2e/playwright.config.ts says
 * `testDir: "."` and `testMatch: /.*\.e2e\.ts/`: Playwright walks the `e2e` directory
 * recursively, so a `.e2e.ts` file here, under `__tests__/fixtures/`, would join the real suite
 * and run. `bun test` collects only `*.test.ts` and its kin, so this file runs nowhere; the scan
 * parses it and the test reads the rows.
 */

import { expect, test } from "@playwright/test";

/** The row's title, computed so that the source alone cannot say what it reads. */
const computedTitle = [
  "a planted row whose title and whose callback",
  "are both passed by name",
].join(" ");

/** The row's callback, declared here rather than written in the call. It takes no fixture. */
async function alsoOffTheBrowser(): Promise<void> {
  expect(await Promise.resolve("the layer that owns this claim")).toContain("claim");
}

test(computedTitle, alsoOffTheBrowser);
