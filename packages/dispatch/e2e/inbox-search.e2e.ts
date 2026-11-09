import { expect, test } from "@playwright/test";
import { searchInbox } from "./inbox-filter";
import { askControls, inboxRow } from "./inbox-pane";
import { openInbox, rows, seedInbox } from "./inbox-search";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * The Inbox's search box (INFRA-963), and the one row of it that needs the real stack: a write
 * the row holds open and then lets through. Every other row of this file reads only what the
 * page draws from one answer and is a stubbed row, in `inbox-search.stub.e2e.ts`.
 */

test.beforeEach(async () => {
  await resetDatabase();
});

test.describe("inbox search", () => {
  // One row outranks the typed text: the row whose "Assign to me" write is still in flight. Under
  // any narrowing the optimistic assignment would drop that row, and unmounting it would take
  // `Assigning…`, a refusal and its Retry off the screen with it.
  //
  // The width is this row's own, 1024 px: the control that registers the write with the list is
  // the card row's (`InboxCardRow`), and above 1280 px the page draws short rows beside the
  // reading column instead, whose head keeps its own `Assigning…` and registers nothing.
  //
  // It is no stubbed row: it holds a PATCH open and then lets it through to a server, and the
  // exemption it is about ends only when that write settles.
  test("a row whose Assign to me write is in flight stays listed with its control", async ({
    browser,
  }) => {
    const seed = await seedInbox();
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await page.setViewportSize({ height: 900, width: 1024 });

      const held = Promise.withResolvers<void>();
      const requested = Promise.withResolvers<void>();
      await page.route("**/api/v1/issues/MIRROR-2", async (route) => {
        if (route.request().method() !== "PATCH") return route.continue();
        requested.resolve();
        await held.promise;
        return route.continue();
      });

      await openInbox(page, seed.listed);
      const assign = (await askControls(page, seed.leaseAskId)).getByRole("button", {
        name: "Assign MIRROR-2 to me",
      });
      await assign.click();
      await requested.promise;
      await expect(assign).toHaveText("Assigning…");

      // Words this row does not hold leave it exactly where it was, with its control.
      await searchInbox(page, "clock drift");
      await expect(rows(page)).toHaveCount(2);
      await expect(inboxRow(page, seed.leaseAskId)).toBeVisible();
      await expect(assign).toHaveText("Assigning…");
      await expect(inboxRow(page, seed.clockAskId)).toBeVisible();

      // Once the write settles the exemption ends, and the text has the row.
      held.resolve();
      await expect(rows(page)).toHaveCount(1);
      await expect(inboxRow(page, seed.leaseAskId)).toHaveCount(0);
      await expect(inboxRow(page, seed.clockAskId)).toBeVisible();
    } finally {
      await alice.close();
    }
  });
});
