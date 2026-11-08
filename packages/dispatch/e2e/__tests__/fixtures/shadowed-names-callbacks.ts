/** The callbacks `shadowed-names.fixture.ts` imports: one that takes no fixture, one that opens a
 *  page, and an object a destructured `const` takes a third from.
 *
 * They live beside the fixture rather than in it because two of the shapes it plants are about a
 * name declared in another module: an import read for the fixtures it binds, and an import that a
 * nested function of the same name must not be read in place of.
 *
 * Its name ends neither `.e2e.ts` nor `.test.ts`, so neither Playwright's `testMatch`
 * (e2e/playwright.config.ts) nor `bun test` collects it; it is imported, and the scan reads it
 * because the fixture that imports it is in the program.
 */

import { expect, type Page } from "@playwright/test";

/** Takes no fixture, so a row whose callback is this one drives no browser. */
export async function readsNothing(): Promise<void> {
  expect(await Promise.resolve("the layer that owns this claim")).toContain("claim");
}

/** Binds `page` in another module, which is where the scan has to read it: a row that passes this
 *  name drives a browser and the check must leave it alone. */
export async function opensAPageInAnotherFile({ page }: { page: Page }): Promise<void> {
  await page.goto("/");
}

/** A callback reached through a property rather than a name of its own, for the two shapes no
 *  resolution of a name settles: `rowHelpers.body` written straight into the call, and a
 *  `const { body }` destructured out of this object. */
export const rowHelpers = {
  body: async (): Promise<void> => {
    expect(await Promise.resolve("the layer that owns this claim")).toContain("claim");
  },
};
