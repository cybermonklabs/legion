/** The planted control of e2e/__tests__/zero-wait-cases.test.ts: a `switch` whose discriminant is
 *  no probe and whose `case` is, so the answer of one zero-wait read picks the case that runs, as
 *  it does when the probe is the discriminant.
 *
 * Its name ends `.fixture.ts` for the reasons `zero-wait-probes.fixture.ts` gives: Playwright and
 * `bun test` collect neither, and `suiteFiles` leaves `__tests__` out.
 *
 * The line is asserted in the test, so a probe added here goes at the END of the file.
 */

import type { Locator } from "@playwright/test";

/** A `switch (true)` that picks its case on one read of whether the menu shows. */
export async function plantedCase(menu: Locator): Promise<string> {
  switch (true) {
    case await menu.isVisible():
      return "shown";
    default:
      return "hidden";
  }
}
