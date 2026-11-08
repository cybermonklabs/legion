/** The callback of `cross-file-row-shape.fixture.ts`'s planted row, and of the four rows
 *  `row-callees.fixture.ts` declares with the title-first forms of `test.fail`, `test.skip` and
 *  `test.fixme`. It is declared in a module of its own so that reading what it binds takes
 *  following the import.
 *
 * It is a neighbour rather than a second function of those fixtures because a program is what
 * makes the two readings alike: the scan resolves the import to this declaration and reads its
 * parameters here, where a reading of the importing file alone had nothing to read.
 *
 * Its name ends neither `.e2e.ts` nor `.test.ts`, so neither Playwright's `testMatch`
 * (e2e/playwright.config.ts) nor `bun test` collects it; it is imported, and the scan reads it
 * because the fixtures that import it are in the program.
 */

import { expect } from "@playwright/test";

/** Takes no fixture, so the row it is the callback of drives no browser. */
export async function offTheBrowserInAnotherFile(): Promise<void> {
  expect(await Promise.resolve("the layer that owns this claim")).toContain("claim");
}
