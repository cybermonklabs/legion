/** The Inbox's reading column over one fixed list: the two shapes of card it draws, the keys
 *  and the address that drive the selection, and what the narrow widths and the drawer keep.
 *
 *  Each reads what the page draws, so each runs against a recording rather than against a
 *  harness (packages/dispatch/AGENTS.md, "The stubbed layer"). The rows that answer or approve,
 *  the two the server withdraws a question under, the refusal that reads the Inbox back, and the
 *  two that read a decision block out of the live document editor stayed in
 *  `inbox-reading-pane.e2e.ts`.
 */
import { expect } from "@playwright/test";

import { inboxList, inboxPane, selectedRow } from "./inbox-pane";
import {
  ADDED_WORD,
  ANCHOR_QUOTE,
  ANCHORED_QUESTION,
  CHANGED_BLOCKS,
  expectTwoColumns,
  INBOX,
  inboxAt,
  PLAIN_QUESTION,
  REMOVED_WORD,
  RETIRED_BLOCK,
  SCOPE_ANCHOR,
  seedInbox,
  WINDOW_ANCHOR,
} from "./inbox-reading-pane";
import { navigateInApp } from "./sends";
import { stubbed } from "./stub/stub-test";

const test = stubbed(seedInbox);

test("the reading pane shows an anchored question and a question in no document as cards @pr", async ({
  page,
  scene,
}) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  const pane = inboxPane(page);

  await page.goto(inboxAt(scene.anchoredAskId));
  await expectTwoColumns(page);
  const anchoredCard = pane.getByTestId(`ask-${scene.anchoredAskId}`);
  await expect(anchoredCard).toContainText(ANCHORED_QUESTION);
  await expect(anchoredCard).toContainText(ANCHOR_QUOTE);
  await expect(anchoredCard.locator("[data-ask-answer]")).toBeVisible();

  // In-app again: the row proves the two shapes of card, not a load of either address.
  await navigateInApp(page, inboxAt(scene.plainAskId));
  const plainCard = pane.getByTestId(`ask-${scene.plainAskId}`);
  await expect(plainCard).toContainText(PLAIN_QUESTION);
  await expect(plainCard.locator("[data-ask-answer]")).toBeVisible();
});

test("the arrow keys, j, k, Enter and ?item= drive the reading pane @pr", async ({
  page,
  scene,
}) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  const pane = inboxPane(page);

  // A bare address selects the first row and writes it into the address.
  await page.goto(INBOX);
  await expect(page).toHaveURL(inboxAt(scene.decisionAskId));

  // `j` and `k` move the selection whether or not the list holds the focus.
  await page.keyboard.press("j");
  await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", scene.approvalAskId);
  await page.keyboard.press("k");
  await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", scene.decisionAskId);

  // `?item=` carries the selection, and a reload keeps it.
  await page.goto(inboxAt(scene.plainAskId));
  await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", scene.plainAskId);
  await page.reload();
  await expect(page).toHaveURL(inboxAt(scene.plainAskId));
  await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", scene.plainAskId);

  // Enter puts the focus in the column's answer box; Escape hands it back to the row, where
  // the arrows move the selection.
  await page.keyboard.press("Enter");
  await expect(pane.locator("[data-ask-answer]")).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(page.locator(`[data-inbox-row="${scene.plainAskId}"]`)).toBeFocused();
  await page.keyboard.press("ArrowUp");
  await expect(selectedRow(page)).not.toHaveAttribute("data-inbox-row", scene.plainAskId);
  await page.keyboard.press("ArrowDown");
  await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", scene.plainAskId);

  // An `?item=` this list cannot answer leaves the first row selected and rewrites the address.
  await page.goto("/?assignee=everyone&item=00000000-0000-7000-8000-000000000000");
  await expect(page).toHaveURL(inboxAt(scene.decisionAskId));
  await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", scene.decisionAskId);
});

test("the Inbox keeps today's card below 1280 px and in the drawer @phone @pr", async ({
  page,
  scene,
}) => {
  // One pixel below the column's width the Inbox is the list of cards it has always been.
  await page.setViewportSize({ height: 900, width: 1279 });
  await page.goto(INBOX);
  await expect(inboxList(page)).toBeVisible();
  await expect(inboxPane(page)).toHaveCount(0);
  const decisionCard = page.getByTestId(`ask-${scene.decisionAskId}`);
  await expect(decisionCard.locator("[data-ask-answer]")).toBeVisible();
  await expect(decisionCard.getByRole("link", { name: "Open in document" })).toBeVisible();

  // And so it is on a phone.
  await page.setViewportSize({ height: 844, width: 390 });
  await page.goto(INBOX);
  await expect(inboxPane(page)).toHaveCount(0);
  await expect(decisionCard.locator("[data-ask-answer]")).toBeVisible();
  await expect(decisionCard.getByRole("link", { name: "Open in document" })).toBeVisible();

  // The approval card's own diff is the change view, with the same marks the column draws.
  const approvalCard = page.getByTestId(`ask-${scene.approvalAskId}`);
  await approvalCard.getByRole("button", { name: "Show diff" }).click();
  const diff = approvalCard.getByTestId("change-view");
  const changed = diff.locator('[data-change="changed"]');
  await expect(changed).toHaveCount(CHANGED_BLOCKS);
  for (let index = 0; index < CHANGED_BLOCKS; index += 1) {
    await expect(changed.nth(index).locator("ins, del")).not.toHaveCount(0);
  }
  const words = diff.locator('[data-change="changed"]', { hasText: "The retry budget is" });
  await expect(words.locator("del")).toHaveText([REMOVED_WORD]);
  await expect(words.locator("ins")).toHaveText([ADDED_WORD]);
  const unchanged = diff.locator('[data-change="unchanged"]', { hasText: SCOPE_ANCHOR });
  await expect(unchanged.locator("ins, del")).toHaveCount(0);
  // The removed block is struck through where it stood: between the same two blocks.
  const blocks = await diff.locator("[data-change]").allTextContents();
  const at = blocks.indexOf(RETIRED_BLOCK);
  expect(at).toBeGreaterThan(0);
  expect(blocks[at - 1]).toBe(SCOPE_ANCHOR);
  expect(blocks[at + 1]).toBe(WINDOW_ANCHOR);
  await expect(diff.locator('[data-change="removed"] del')).toHaveText([RETIRED_BLOCK]);

  // The drawer is a peek beside the page it covers, so it keeps the card at every width.
  await page.setViewportSize({ height: 900, width: 1440 });
  await page.goto(`/issues/${scene.decisionIssueKey}`);
  await page
    .getByRole("navigation", { name: "Navigation" })
    .getByRole("button", { name: "Peek at the inbox without leaving this page" })
    .click();
  const drawer = page.getByRole("dialog", { name: "Inbox" });
  await expect(drawer).toBeVisible();
  await expect(drawer.getByTestId("inbox-pane")).toHaveCount(0);
  const drawerCard = drawer.getByTestId(`ask-${scene.decisionAskId}`);
  await expect(drawerCard.locator("[data-ask-answer]")).toBeVisible();
  await expect(drawerCard.getByRole("link", { name: "Open in document" })).toBeVisible();
});
