/** The planted shapes of e2e/__tests__/no-browser-rows.test.ts that turn on WHAT A NAME IS BOUND
 *  TO: a reading that matched the name in a call against the functions a file declares read every
 *  one of the first five as `opensAPage` below, reported `fixtures: ["page"]` for each, and let a
 *  row that drives nothing through the gate. The checker binds each name in its own scope
 *  instead.
 *
 * Eight rows. `opensAPage` is the trap under the first five: a for-of `const` of that name, a
 * `let` inside a `describe`, a `const` destructured out of an object, a helper's parameter, and
 * an import beside a nested function of that same name. None of the five is the function at the
 * top of this file, and none of their rows drives a browser, so the check must report all five
 * with no fixtures.
 *
 * Then the two a name settles nothing about — the value a call returns, and a property of an
 * imported object — which the check reports with no fixtures because it read no declaration, the
 * second of them a callback that does bind `page`: a body it could not read is no body it saw.
 *
 * The last two are the other direction: `opensAPage` passed where nothing shadows it, and an
 * import that binds `page` in the module it is declared in. The check must read both and leave
 * both rows alone, since a reading that named every row it saw would be no reading at all.
 *
 * Its name ends `.fixture.ts` and not `.e2e.ts` because e2e/playwright.config.ts says
 * `testDir: "."` and `testMatch: /.*\.e2e\.ts/`: Playwright walks the `e2e` directory
 * recursively, so a `.e2e.ts` file here, under `__tests__/fixtures/`, would join the real suite
 * and run. `bun test` collects only `*.test.ts` and its kin, so this file runs nowhere; the scan
 * parses it and the test reads the rows.
 */

import { expect, type Page, test } from "@playwright/test";

import { opensAPageInAnotherFile, readsNothing, rowHelpers } from "./shadowed-names-callbacks";

/** The trap: the only function this file declares that binds a browser fixture, and the one a
 *  reading by name alone resolved every row below to. */
async function opensAPage({ page }: { page: Page }): Promise<void> {
  await page.goto("/");
}

// The reviewer's repro: a for-of `const` of the trap's name, bound to a function that takes
// nothing, so the row drives no browser.
for (const opensAPage of [
  async (): Promise<void> => {
    expect(await Promise.resolve("the layer that owns this claim")).toContain("claim");
  },
]) {
  test("a planted row whose callback is a for-of const of a declared name", opensAPage);
}

test.describe("a describe whose row takes its callback from a let", () => {
  // A `let` can hold another function by the time Playwright runs the row — this one holds the
  // trap by then — so the scan reads no `let` and reports the row with no fixtures.
  let body: (args: { page: Page }) => Promise<void> = async (): Promise<void> => {
    expect(await Promise.resolve("the layer that owns this claim")).toContain("claim");
  };
  body = opensAPage;

  test("a planted row whose callback is a let inside a describe", body);
});

/** A name destructured out of an imported object: a binding element, and no function
 *  declaration. */
const { body: destructuredBody } = rowHelpers;

test("a planted row whose callback is a destructured const", destructuredBody);

/** Declares a row from its own parameter, so the name in the call is bound to whatever a caller
 *  passed. The row's title is that parameter's source text, which is all a reader of the source
 *  has. */
function declareRow(title: string, body: () => Promise<void>): void {
  test(title, body);
}

declareRow("a planted row declared by a helper from its parameter", readsNothing);

test(
  "a planted row whose callback is an import beside a nested function of that name",
  readsNothing
);

/** Holds a nested function of the imported name above, which is the shape a file-wide match of
 *  names read in place of the import. It is returned rather than called so that the row below
 *  passes a callback that binds `page` and no name at all. */
function nestedOfTheImportedName(): (args: { page: Page }) => Promise<void> {
  async function readsNothing({ page }: { page: Page }): Promise<void> {
    await page.goto("/");
  }
  return readsNothing;
}

test("a planted row whose callback is what a call returns", nestedOfTheImportedName());

test("a planted row whose callback is a property of an imported object", rowHelpers.body);

test("a planted row whose callback is this file's own function", opensAPage);

test("a planted row whose callback is imported and opens a page", opensAPageInAnotherFile);
