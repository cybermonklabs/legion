import { expect, test } from "@playwright/test";

import { resetDatabase } from "./seed";

/**
 * The reader nobody signed in. It is no stubbed row: that layer's page is signed in, because
 * its recording answers `/auth/whoami` with the person the seeding signed in as. The signed-in
 * reader's empty Inbox is `smoke.stub.e2e.ts`.
 */

test.beforeEach(async () => {
  await resetDatabase();
});

test("an anonymous user is sent to Google sign-in", async ({ browser }) => {
  const context = await browser.newContext();
  const page = await context.newPage();

  await page.goto("/");

  await expect(page.getByRole("link", { name: "Sign in with Google" })).toHaveAttribute(
    "href",
    "/auth/start"
  );

  await context.close();
});
