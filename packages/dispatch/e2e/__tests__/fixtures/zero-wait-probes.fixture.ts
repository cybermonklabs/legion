/** The planted controls of e2e/__tests__/zero-wait-probes.test.ts: one probe of every shape the
 *  rule has an answer for, so proving the check looks needs no edit of a real spec. Without them
 *  a check that refuses nothing in the suite would read the same whether it looked or not.
 *
 * Its name ends `.fixture.ts` and not `.e2e.ts` because e2e/playwright.config.ts says
 * `testDir: "."` and `testMatch: /.*\.e2e\.ts/`: Playwright walks the `e2e` directory
 * recursively, so a `.e2e.ts` file here would join the real suite and run. `bun test` collects
 * only `*.test.ts` and its kin, so this file runs nowhere; the test reads its text and parses it.
 * `suiteFiles` skips this whole directory for the same reason: half the probes below are written
 * to be refused, and the rule is about the suite.
 *
 * No comment stands directly above a planted probe but the two whose length is what they plant,
 * because a comment there is what the rule reads as a reason.
 *
 * The lines are asserted in the test, so a probe added here goes at the END of the file.
 */

import { expect, type Locator, type Page, test } from "@playwright/test";

/** A wrapper: its own `return` hands on a probe, so a call of it is a probe where it is called.
 *  The probe it returns decides nothing here. */
async function menuShows(menu: Locator): Promise<boolean> {
  return menu.isVisible();
}

/** One probe per shape the rule refuses, in this order: an `if` on `isVisible`, a `const` read
 *  later in a ternary, the condition of `test.skip`, a `for` loop's bound, a wrapper called in an
 *  `if`, and an `if` under a comment of seven words. */
export async function plantedRefusals(page: Page, menu: Locator, rows: Locator): Promise<string> {
  if (!(await menu.isVisible())) return "closed";

  const shown = await menu.textContent();
  const label = shown === "Menu" ? "menu" : "other";

  test.skip((await rows.count()) === 0, "a planted skip decided by one count");

  for (let index = 0; index < (await rows.count()); index += 1) {
    await rows.nth(index).click();
  }

  if (await menuShows(menu)) await menu.click();

  // One read of this instant is enough.
  if ((await rows.count()) > 1) return label;

  return page.url();
}

/** One probe per shape the rule passes, in this order: a ternary inside `expect.poll`, a ternary
 *  inside the subject of `expect(...).toPass`, a ternary inside `page.evaluate`, a `getAttribute`
 *  bound to a `const` whose `if` only throws, a `getAttribute` in an `if` that only throws, a
 *  probe handed to `expect(...)`, and an `if` under a reason of eight words. */
export async function plantedPasses(page: Page, menu: Locator, rows: Locator): Promise<void> {
  await expect.poll(async () => ((await rows.count()) > 0 ? "some" : "none")).toBe("some");

  await expect(async () => {
    expect((await menu.inputValue()) === "" ? "empty" : "filled").toBe("filled");
  }).toPass();

  await page.evaluate(() => (document.body.getAttribute("data-ready") === "yes" ? 1 : 0));

  const state = await menu.getAttribute("data-state");
  if (state === null) throw new Error("the planted menu names no state");

  if ((await menu.getAttribute("aria-expanded")) !== "true") {
    throw new Error("the planted menu is not expanded");
  }

  expect(await rows.count()).toBe(2);

  // One read of this instant is enough here.
  if (await menu.isChecked()) await menu.uncheck();
}

/** Two loops over `Promise.all`, which is no probe: it waits for the promises it is handed and
 *  reads no page, though its member is named like `locator.all()`. One walks it in place, and one
 *  walks a `const` bound to it, as e2e/keyboard-inbox.e2e.ts walks the requests it awaits. */
export async function plantedPromiseAll(page: Page, rows: Locator): Promise<void> {
  for (const box of await Promise.all([rows.first().boundingBox(), rows.last().boundingBox()])) {
    expect(box).not.toBeNull();
  }

  const writes = Promise.all([page.waitForRequest("**/issues"), page.waitForRequest("**/asks")]);
  await rows.first().click();
  for (const request of await writes) expect(request.method()).toBe("POST");
}
