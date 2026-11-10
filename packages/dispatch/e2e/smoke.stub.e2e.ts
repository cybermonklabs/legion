/** The signed-in reader's empty Inbox. It reads only what the page draws, so it runs against a
 *  recording rather than against a harness (packages/dispatch/AGENTS.md, "The stubbed layer").
 *  The anonymous reader's row stayed in `smoke.e2e.ts`: this fixture's page is signed in,
 *  because the recording answers `/auth/whoami`. */
import { expect } from "@playwright/test";

import { stubbed } from "./stub/stub-test";

const test = stubbed(async () => ({ person: "alice" }));

test("an authenticated user sees their empty inbox @pr", async ({ page, scene }, testInfo) => {
  await page.goto("/");

  await expect(page.getByText(`Signed in as ${scene.person}`)).toBeVisible();
  await expect(page.getByText("Nothing needs you")).toBeVisible();
  await page.screenshot({ path: testInfo.outputPath("empty-inbox.png"), fullPage: true });
});
