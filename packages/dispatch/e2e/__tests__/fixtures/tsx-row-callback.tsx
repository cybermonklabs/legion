/** The callback of `compiler-options.fixture.ts`'s planted row, in a `.tsx` module because the
 *  extension is what the row is a control for: the scan's program resolves this import only
 *  while `jsx` is among its compiler options, and refuses it with TS6142 without one. The
 *  module writes JSX for the same reason a web module does, so the extension is the file's own
 *  and not a costume — `inbox-dismiss.e2e.ts` imports `GROUP_PAGE` out of
 *  `web/src/features/inbox/InboxGroups`, which is a `.tsx` of exactly this kind.
 *
 * Its name ends neither `.e2e.ts` nor `.test.ts`, so neither Playwright's `testMatch`
 * (e2e/playwright.config.ts) nor `bun test` collects it; it is imported, and the scan reads it
 * because the fixture that imports it is in the program.
 */

import type { Page } from "@playwright/test";
import type { ReactNode } from "react";

/** The path the planted row visits, read out of this module the way a spec reads a constant out
 *  of a web one. */
export const PLANTED_PATH = "/";

/** Why this module is a `.tsx`: it draws. */
export function PlantedLink(): ReactNode {
  return <a href={PLANTED_PATH}>the planted path</a>;
}

/** Binds `page`, so the row it is the callback of drives a browser — but only when the scan
 *  resolved this module and read these parameters. */
export async function opensAPageFromATsxModule({ page }: { page: Page }): Promise<void> {
  await page.goto(PLANTED_PATH);
}
