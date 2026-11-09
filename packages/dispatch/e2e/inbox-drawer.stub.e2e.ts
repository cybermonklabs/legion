/** The Inbox drawer over a page that is not the Inbox: where it opens from, what it covers,
 *  what Escape and a click outside do, and the key that toggles it.
 *
 *  Each reads what the page draws over one fixed answer, so each runs against a recording
 *  rather than against a harness (packages/dispatch/AGENTS.md, "The stubbed layer"). The row
 *  that answers an ask in the drawer over a live conversation stayed in `inbox-drawer.e2e.ts`.
 */
import { expect } from "@playwright/test";

import { attachShot, drawer, peekTrigger, seedAsk } from "./inbox-drawer";
import { stubbed } from "./stub/stub-test";

/** One question, in the drawer over the issue it was asked on. */
const test = stubbed(() => seedAsk("Which label for the drawer?"));

/** The same, for the row that reads the shortcut rather than the panel. */
const shortcutTest = stubbed(async () => ({
  ...(await seedAsk("Is the shortcut listed?")),
  rows: 1,
}));

test("over an issue page the drawer opens from the header, keeps its page, and Escape returns focus to the button @pr", async ({
  page,
  scene,
}, testInfo) => {
  await page.goto(`/issues/${scene.issueKey}`);
  await expect(
    page.getByTestId("issue-header").getByRole("heading", { level: 1, name: "Drawer issue" })
  ).toBeVisible();
  // One name at both widths, so the trigger is read by width rather than by a second name
  // (`peekTrigger`): this row runs above the compact width, where the rail carries it.
  const trigger = peekTrigger(page);
  await trigger.click();
  await expect(drawer(page).getByTestId(`ask-${scene.askId}`)).toBeVisible();
  await expect(page).toHaveURL(new RegExp(`/issues/${scene.issueKey}$`));
  await attachShot(page, testInfo, "inbox-drawer-over-issue");

  await page.keyboard.press("Escape");
  await expect(drawer(page)).toHaveCount(0);
  await expect(trigger).toBeFocused();
  await expect(
    page.getByTestId("issue-header").getByRole("heading", { level: 1, name: "Drawer issue" })
  ).toBeVisible();

  // A click outside the panel closes it too.
  await trigger.click();
  await expect(drawer(page)).toBeVisible();
  await page.mouse.click(10, 10);
  await expect(drawer(page)).toHaveCount(0);
});

// The drawer's phone half: below the compact width the top bar carries the same Peek, and the
// panel it opens takes the whole screen rather than the side panel the row above opens. What
// the drawer does once open is that row's, at any width. `stub-phone` is the one project that
// runs it, the `stub` project inverting the tag, so the layout it reads is the phone's.
test("on a phone the inbox drawer opens from the compact top bar and covers the screen @phone @pr", async ({
  page,
  scene,
}, testInfo) => {
  await page.goto(`/issues/${scene.issueKey}`);
  await peekTrigger(page).click();
  await expect(drawer(page).getByTestId(`ask-${scene.askId}`)).toBeVisible();
  const box = await drawer(page).boundingBox();
  const viewport = page.viewportSize();
  expect(box?.x).toBe(0);
  expect(box?.width).toBe(viewport?.width);
  await attachShot(page, testInfo, "inbox-drawer-over-issue-phone");
});

shortcutTest(
  "`i` toggles the drawer, is listed in ?, and is not offered on the Inbox page itself @pr",
  async ({ page, scene }, testInfo) => {
    await page.goto("/agents");
    await expect(page.getByRole("heading", { name: "Agents" })).toBeVisible();
    await page.locator("body").focus();
    await page.keyboard.press("i");
    await expect(drawer(page)).toBeVisible();
    await attachShot(page, testInfo, "inbox-drawer-over-agents-list");
    await page.getByRole("button", { name: "Close" }).click();
    await expect(drawer(page)).toHaveCount(0);

    await page.keyboard.press("?");
    const help = page.getByRole("dialog", { name: "Keyboard shortcuts" });
    const row = help.getByRole("listitem").filter({ hasText: "Open the inbox" });
    await expect(row.locator("kbd")).toHaveText(["i"]);
    await expect(row).toHaveAttribute("data-enabled", "true");
    await page.keyboard.press("Escape");

    await page.goto("/");
    await expect(page.locator("[data-inbox-row]")).toHaveCount(scene.rows);
    await page.locator("body").focus();
    await page.keyboard.press("i");
    await expect(drawer(page)).toHaveCount(0);
    await expect(
      page.getByRole("button", { name: "Peek at the inbox without leaving this page" })
    ).toHaveCount(0);
  }
);
