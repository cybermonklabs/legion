/** The planted shapes of e2e/__tests__/no-browser-rows.test.ts that turn on WHICH MEMBER OF A
 *  `test` DECLARES A ROW. @playwright/test 1.64.1 (`types/test.d.ts`) gives `test` itself,
 *  `only`, `skip`, `fixme`, `fail` and `fail.only` a `(title, body)` form and a
 *  `(title, details, body)` form, and no other member of a `TestType` carries one; the modifier
 *  forms of `skip`, `fixme`, `fail` and `slow`, every chain of `describe` and of `step`, `use`
 *  and `extend` declare no row. Which spellings of a `test` those members are read on is the
 *  neighbouring row-callee-spellings.fixture.ts.
 *
 * Six rows here, and the file is as full of the calls that are not rows as of the calls that
 * are. Five are declared by the members a reading of two-part names alone read as modifiers or
 * missed; the sixth is the one inside a `test.describe.only` block, which is what shows the
 * block's own call declares no row of its own while the rows under it are read as ever. Around
 * them stand a `test.skip(<condition>, <reason>)` whose reason is a `const` of this file — the
 * shape the four real calls of this suite have, with the imported `plainHttpSkipReason` of
 * e2e/plain-http-origin.ts — a `test.slow()`, and a `test.step.skip(…)`, none of which may be
 * read as a row.
 *
 * Of those five, a reading that took a row from a literal title or a callback the file itself
 * declares read one: the `test.fail` written with both. The other four it declared no row for —
 * `test.fail.only`, whose callee it did not spell, and the `skip`, `fixme` and three-argument
 * `fail` below, whose title is a name and whose body is a `let` that no such reading resolves.
 * The callee is all there is to read those three from.
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

/** The titles of the three rows below, each computed and each passed by name, so that no reader
 *  of the call alone has the title and the row is read from its callee. */
const skipTitle = ["a planted row declared by", "test.skip"].join(" ");
const fixmeTitle = ["a planted row declared by", "test.fixme"].join(" ");
const failTitle = ["a planted row declared by", "test.fail with details before its body"].join(" ");

/** The body of those same three rows. A `let` can hold another function by the time Playwright
 *  runs the row, so the scan reads none and reports each row with no fixtures: a title it
 *  cannot read and a body it cannot read leave the callee as all there is to read the row
 *  from, which is the whole of what this fixture is for. */
let unreadableBody: () => Promise<void> = async (): Promise<void> => {
  expect(await Promise.resolve("the layer that owns this claim")).toContain("claim");
};
unreadableBody = offTheBrowserInAnotherFile;

test.fail("a planted row declared by test.fail", offTheBrowserInAnotherFile);

test.skip(skipTitle, unreadableBody);

test.fixme(fixmeTitle, unreadableBody);

test.fail(failTitle, { tag: "@planted" }, unreadableBody);

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
