import { expect, test } from "@playwright/test";

import { seedReference } from "./hover-preview";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * The phone's answer to a `dispatch://` reference. Every other row of this family reads only
 * what the page draws and is a stubbed row, in `hover-preview.stub.e2e.ts`; this one stays
 * because the `iphone` project runs no stubbed row.
 */

test.beforeEach(async () => {
  await resetDatabase();
});

test("on a phone, tapping a reference follows it and never shows a card @phone", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "iphone", "touch geometry is covered by the iphone project");
  const { source, target } = await seedReference();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${source}/conversation`);
    const link = page
      .getByRole("list", { name: "Conversation turns" })
      .getByRole("link", { name: "Hover target" });
    await expect(link).toBeVisible();
    await link.tap();
    await expect(page).toHaveURL(new RegExp(`/issues/${target}(/|$)`));
    await expect(page.getByRole("tooltip")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});
