/** The planted shapes of e2e/__tests__/no-browser-rows.test.ts that turn on WHICH CALLEE
 *  DECLARES A ROW. @playwright/test 1.64.1 (`types/test.d.ts`) gives `test`, `test.only`,
 *  `test.skip`, `test.fixme`, `test.fail` and `test.fail.only` a `(title, body)` form and a
 *  `(title, details, body)` form, and those six are every way a row is declared; the modifier
 *  forms of `skip`, `fixme`, `fail` and `slow`, every chain of `test.describe` and of
 *  `test.step`, and `test.use` declare none.
 *
 * Six rows here, and the file is as full of the calls that are not rows as of the calls that
 * are. Five rows are declared by the callees a reading of two-part names alone missed or read as
 * modifiers; the sixth is the one inside a `test.describe.only` block, which is what shows the
 * block's own call declares no row of its own while the rows under it are read as ever. Around
 * them stand a `test.skip(<condition>, <reason>)` whose reason is a `const` of this file — the
 * shape the four real calls of this suite have, with the imported `plainHttpSkipReason` of
 * e2e/plain-http-origin.ts — a `test.slow()`, and a `test.step.skip(…)`, none of which may be
 * read as a row.
 *
 * Every row but the one in the block drives no browser, so the check must name the five and
 * leave that one.
 *
 * Its name ends `.fixture.ts` and not `.e2e.ts` because e2e/playwright.config.ts says
 * `testDir: "."` and `testMatch: /.*\.e2e\.ts/`: Playwright walks the `e2e` directory
 * recursively, so a `.e2e.ts` file here, under `__tests__/fixtures/`, would join the real suite
 * and run - and `test.fail.only` would then be the only row of the whole run. `bun test`
 * collects only `*.test.ts` and its kin, so this file runs nowhere; the scan parses it and the
 * test reads the rows.
 */

import { expect, test } from "@playwright/test";

import { offTheBrowserInAnotherFile } from "./cross-file-row-callback";

/** The reason of the modifier call below, a `const` of this file so that the scan reads what the
 *  name is bound to rather than the literal it would see in the call. */
const skipReason = "a reason this row is skipped, which is no body";

test.fail("a planted row declared by test.fail", offTheBrowserInAnotherFile);

test.skip("a planted row declared by test.skip", offTheBrowserInAnotherFile);

test.fixme("a planted row declared by test.fixme", offTheBrowserInAnotherFile);

test.fail(
  "a planted row declared by test.fail with details before its body",
  { tag: "@planted" },
  offTheBrowserInAnotherFile
);

test.fail.only("a planted row declared by test.fail.only", async () => {
  expect(await Promise.resolve("the layer that owns this claim")).toContain("claim");
});

test.describe
  .only("a planted describe block, which declares no row of its own", () => {
    test("a planted row inside a describe block", async ({ page }) => {
      test.slow();
      test.skip(page.url() === "about:blank", skipReason);
      await test.step
        .skip("a step of the row, which declares no row either", async () => {
          await page.goto("/");
        });
    });
  });
