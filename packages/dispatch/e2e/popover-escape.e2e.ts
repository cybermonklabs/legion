import { expect, test } from "@playwright/test";
import { createAsk, createIssue, createProject } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

// Escape belongs to the layer the reader is in. Every popover in Dispatch hangs off one
// `AnchoredPanel`, which listens for Escape at the document's capture phase: without a check on
// where the press came from, a popover left open behind the command bar swallows the bar's own
// Escape, and the second press lands focus on nothing.
test("the command bar over an open popover takes the first Escape, the popover the second", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await createIssue({ project: "CORE", title: "Escape ownership row" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/projects/CORE/issues");
    const trigger = page
      .getByRole("columnheader", { name: /Status/ })
      .getByRole("button", { name: "Filter Status" });
    await trigger.click();
    const options = page.getByRole("listbox", { name: "Status" });
    await expect(options).toBeVisible();
    await page.keyboard.press("Control+k");
    const bar = page.getByRole("dialog", { name: "Search" });
    await expect(bar).toBeVisible();

    // The bar is the layer in front, so it is the one Escape closes; the popover behind it is
    // untouched, which is what the bar's own focus return depends on.
    await page.keyboard.press("Escape");
    await expect(bar).toHaveCount(0);
    await expect(options).toBeVisible();
    expect(await page.evaluate(() => document.activeElement?.tagName)).not.toBe("BODY");

    // Now the popover is the layer in front, and the next Escape is its own.
    await page.keyboard.press("Escape");
    await expect(options).toHaveCount(0);
    await expect(trigger).toBeFocused();
  } finally {
    await context.close();
  }
});

// A panel inside a dialog closes itself and leaves the dialog open: the inbox peek is a dialog
// over the page, and its filters are the same popover.
test("Escape in a filter inside the inbox peek closes the filter, not the peek", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Peek dialog row" });
  // The peek draws its filters only once it has a row to filter.
  await createAsk(
    issue.key,
    { options: [{ label: "Yes" }, { label: "No" }], question: "Ship the peek?" },
    { actor: { id: "e2e-popover-escape", kind: "session" }, as: "agent" }
  );
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto(`/issues/${issue.key}`);
    await page
      .getByRole("navigation", { name: "Navigation" })
      .getByRole("button", { name: "Peek at the inbox without leaving this page" })
      .click();
    const peek = page.getByRole("dialog", { name: "Inbox" });
    await expect(peek).toBeVisible();
    // The bar keeps its filter row folded until the reader asks for it.
    await peek.locator("[data-inbox-filter]").click();
    const trigger = peek.getByRole("button", { name: "Filter by assignee" });
    await trigger.click();
    const options = page.getByRole("listbox", { name: "Assignee" });
    await expect(options).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(options).toHaveCount(0);
    await expect(peek).toBeVisible();
    await expect(trigger).toBeFocused();
  } finally {
    await context.close();
  }
});
