/** The fourth planted shape of e2e/__tests__/no-browser-rows.test.ts, and the one the check
 *  still does not see: a row whose title and whose callback are BOTH passed by name. Playwright
 *  runs it like any other row, and it binds no browser fixture, so `rowsOfSource` must report it
 *  and `unexplainedRows` must name it.
 *
 * `unread-row-shapes.fixture.ts` holds the two shapes with one unreadable half each — a computed
 * title with an inline callback, and a literal title with a callback passed by name — and the
 * check reads both. With both halves unreadable it reads nothing at all, so a row of this shape
 * that drives no browser passes the gate in silence, which is the one thing the rule is about and
 * what `packages/dispatch/AGENTS.md` promises the check does not allow: it says the check "reads
 * every `test(` call of `e2e/**​/*.e2e.ts`" and "refuses … a row whose callback it cannot read …
 * since fixtures it did not read are not fixtures it saw".
 *
 * Reading the second argument's declaration in the same file settles it without a type checker:
 * an identifier that names a function declaration or a function-valued `const` of this file is a
 * callback, and its parameters are the row's fixtures; one that names anything else — the
 * `plainHttpSkipReason` string of `plain-http-proxy.e2e.ts:18` and the three other
 * `test.skip(<condition>, <reason>)` calls of this suite written with no string literal — is not.
 *
 * Its name ends `.fixture.ts` and not `.e2e.ts` because e2e/playwright.config.ts says
 * `testDir: "."` and `testMatch: /.*\.e2e\.ts/`: Playwright walks the `e2e` directory
 * recursively, so a `.e2e.ts` file here, under `__tests__/fixtures/`, would join the real suite
 * and run. `bun test` collects only `*.test.ts` and its kin, so this file runs nowhere; the test
 * reads its text and parses it.
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
