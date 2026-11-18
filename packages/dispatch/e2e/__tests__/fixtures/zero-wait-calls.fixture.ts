/** The planted controls of e2e/__tests__/zero-wait-calls.test.ts: a call on a probe's answer whose
 *  result decides a path, each written twice, once on the answer in place and once on a name the
 *  answer is bound to, and one answer handed to a function that decides inside it.
 *
 * Its name ends `.fixture.ts` for the reasons `zero-wait-probes.fixture.ts` gives: Playwright and
 * `bun test` collect neither, and `suiteFiles` leaves `__tests__` out.
 *
 * The lines are asserted in the test, so a probe added here goes at the END of the file.
 */

import type { Locator } from "@playwright/test";

/** Clicks `menu` where `visible` says it shows: the decision is made inside, on a parameter. */
async function clickIfShown(menu: Locator, visible: boolean): Promise<void> {
  if (visible) await menu.click();
}

/** A method of the answer, `Boolean` of it and an optional call on it, each deciding an `if`. */
export async function plantedCalls(menu: Locator): Promise<void> {
  const text = (await menu.textContent()) ?? "";
  if (text.includes("B")) await menu.click();
  if (((await menu.textContent()) ?? "").includes("B")) await menu.click();

  const shown = await menu.isVisible();
  if (Boolean(shown)) await menu.click();
  if (Boolean(await menu.isVisible())) await menu.click();

  const label = await menu.textContent();
  if (label?.trim()) await menu.click();
  if ((await menu.textContent())?.trim()) await menu.click();

  await clickIfShown(menu, await menu.isVisible());
}
