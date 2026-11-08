/** The planted shapes of e2e/__tests__/no-browser-rows.test.ts that turn on HOW A `test` IS
 *  SPELLED. A reading that matched the callee's text against `test`, `test.only`, `test.skip`,
 *  `test.fixme`, `test.fail` and `test.fail.only` found no row at all in any of the six calls
 *  below: each declares one through a `TestType` under another spelling, and a row that drives
 *  no browser walked past a gate that never saw it.
 *
 * Six rows, one per spelling: a `test` extended by `test.extend`, which is Playwright's
 * documented way to add a fixture; a `const` bound to `test`; an import renamed to `it`; `test`
 * reached through a namespace import; `test["skip"]` by element access; and a parenthesized
 * `test`. None of the six binds a browser fixture, so the check must name all six.
 *
 * Around them stand calls of those same spellings that declare no row: the `apiTest.describe`
 * block, whose row is the one row of this file that does drive a browser, and the
 * `apiTest.skip(<condition>, <reason>)` and `alias.slow()` inside it. A member of a `TestType`
 * is read as the member it is whatever the `TestType` is called. That one row is this file's
 * control for a callback declaring a `this` parameter too: `this` binds no fixture, and a
 * reading that took it for the fixture parameter would name a row that opens a page.
 *
 * Its name ends `.fixture.ts` and not `.e2e.ts` because e2e/playwright.config.ts says
 * `testDir: "."` and `testMatch: /.*\.e2e\.ts/`: Playwright walks the `e2e` directory
 * recursively, so a `.e2e.ts` file here, under `__tests__/fixtures/`, would join the real suite
 * and run. `bun test` collects only `*.test.ts` and its kin, so this file runs nowhere; the scan
 * parses it and the test reads the rows.
 */

import * as pw from "@playwright/test";
import { expect, test as it, test } from "@playwright/test";

/** What `test.extend` returns is a `TestType` again, so its calls declare rows as `test`'s do.
 *  It takes no fixture of its own here: what this file plants is the spelling. */
const apiTest = test.extend({});

/** The same `TestType`, bound to another name. */
const alias = test;

apiTest("a planted row declared by an extended test", async ({ request }) => {
  expect(typeof request.get).toBe("function");
});

alias("a planted row declared by a const bound to test", async () => {
  expect(await Promise.resolve("the layer that owns this claim")).toContain("claim");
});

it("a planted row declared by an import renamed to it", async () => {
  expect(await Promise.resolve("the layer that owns this claim")).toContain("claim");
});

pw.test("a planted row declared through a namespace import", async () => {
  expect(await Promise.resolve("the layer that owns this claim")).toContain("claim");
});

// biome-ignore lint/complexity/useLiteralKeys: the element access is the spelling this row plants.
test["skip"]("a planted row declared by element access", async () => {
  expect(await Promise.resolve("the layer that owns this claim")).toContain("claim");
});

// biome-ignore format: the parentheses around `test` are the spelling this row plants.
(test)("a planted row declared by a parenthesized test", async () => {
  expect(await Promise.resolve("the layer that owns this claim")).toContain("claim");
});

apiTest.describe("a planted describe of an extended test, which declares no row", () => {
  // The one row here that drives a browser, and the one whose callback declares a `this`
  // parameter: `this` is a type annotation standing in the parameter list and no argument, so
  // the fixtures are the parameter after it. A reading that took the first parameter whatever
  // it was would report this row with no fixtures and name a row that opens a page.
  apiTest(
    "a planted row inside an extended describe",
    async function (this: void, { page }): Promise<void> {
      alias.slow();
      apiTest.skip(page.url() === "about:blank", "a reason this row is skipped, which is no body");
      await page.goto("/");
    }
  );
});
