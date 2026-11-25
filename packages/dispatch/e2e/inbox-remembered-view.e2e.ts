/** The Inbox keeps the view a reader sets - its filters, its sort and its grouping - for their
 *  browser until they clear it (`features/inbox/remembered-view.ts`): a visit from the
 *  sidebar, a reload and a new tab open on it, and **Clear filters** forgets the filters.
 */
import type { Page } from "@playwright/test";

import { pickAssignee } from "./inbox-filter";
import { expect, test } from "./test";
import { asUser } from "./users";

/** The sidebar's Inbox link, whose count rides in its name. */
function sidebarInbox(page: Page) {
  return page.getByRole("navigation", { name: "Navigation" }).getByRole("link", { name: /^Inbox/ });
}

async function groupBy(page: Page, label: string): Promise<void> {
  await page.locator("[data-inbox-view]").click();
  await page
    .getByRole("group", { name: "Group by" })
    .getByRole("radio", { exact: true, name: label })
    .check();
  await page.keyboard.press("Escape");
}

test("the Inbox opens on the view the reader set until they clear its filters", async ({
  browser,
}) => {
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto("/");
    await pickAssignee(page, "Unassigned");
    await page.keyboard.press("Escape");
    await groupBy(page, "Project");
    await expect(page).toHaveURL(/[?&]assignee=/);
    await expect(page).toHaveURL(/[?&]group=project/);
    const view = new URL(page.url()).search;

    // Leaving and coming back from the sidebar, whose link names no view.
    await page.goto("/agents");
    await sidebarInbox(page).click();
    await expect.poll(() => new URL(page.url()).search).toBe(view);
    await expect(page.locator("[data-inbox-filter-tokens]")).toHaveCount(1);

    // A reload of the plain address, and a new tab of the same browser.
    await page.goto("/");
    await expect.poll(() => new URL(page.url()).search).toBe(view);
    const tab = await context.newPage();
    await tab.goto("/");
    await expect.poll(() => new URL(tab.url()).search).toBe(view);
    await tab.close();

    // Clear filters forgets the filters; the grouping is a View choice and stays.
    await page.locator("[data-inbox-clear-filters]").click();
    await expect(page).not.toHaveURL(/[?&]assignee=/);
    await page.goto("/");
    await expect(page).toHaveURL(/[?&]group=project/);
    await expect(page).not.toHaveURL(/[?&]assignee=/);
    await expect(page.locator("[data-inbox-filter-tokens]")).toHaveCount(0);
  } finally {
    await context.close();
  }
});
