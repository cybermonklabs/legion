/** The callback of `cross-file-row-shape.fixture.ts`'s planted row, declared in a module of its
 *  own so that no reading of that file's own source can say what it binds.
 *
 * It is a neighbour rather than a second function of the fixture because the hole the fixture
 * pins is exactly the one same-file resolution cannot close: `namedCallbacksOfFile` reads the
 * functions one file declares, and an imported name is not one of them.
 *
 * Its name ends neither `.e2e.ts` nor `.test.ts`, so neither Playwright's `testMatch`
 * (e2e/playwright.config.ts) nor `bun test` collects it; it is imported, and its text is read.
 */

import { expect } from "@playwright/test";

/** Takes no fixture, so the row it is the callback of drives no browser. */
export async function offTheBrowserInAnotherFile(): Promise<void> {
  expect(await Promise.resolve("the layer that owns this claim")).toContain("claim");
}
