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

// @pr: Sign-in and tokens: the first thing an anonymous person meets, and the row the whole lane rests on.
test("an anonymous user is sent to Google sign-in @pr", async ({ browser }) => {
  const context = await browser.newContext();
  const page = await context.newPage();

  await page.goto("/");

  await expect(page.getByRole("link", { name: "Sign in with Google" })).toHaveAttribute(
    "href",
    "/auth/start"
  );

  await context.close();
});
