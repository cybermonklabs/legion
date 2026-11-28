/** The Inbox's reading column over one fixed list: the two shapes of card it draws, the keys
 *  and the address that drive the selection, the row the narrow widths read in place of the
 *  list, and the card the drawer keeps.
 *
 *  Each reads what the page draws, so each runs against a recording rather than against a
 *  harness (packages/dispatch/README.md, "The stubbed layer"). The rows that answer or approve,
 *  the two the server withdraws a question under, the refusal that reads the Inbox back, and the
 *  two that read a decision block out of the live document editor stayed in
 *  `inbox-reading-pane.e2e.ts`.
 */
import { expect, type Page } from "@playwright/test";

import { peekTrigger } from "./inbox-drawer";
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

test("below 1280 px the Inbox reads the row it opens in place of the list, and the drawer keeps the card @phone @pr", async ({
  page,
  scene,
}) => {
  // One pixel below the column's width the Inbox lists the short rows it lists beside the
  // column, with no card in them and no pane beside them.
  await page.setViewportSize({ height: 900, width: 1279 });
  await page.goto(INBOX);
  await expect(inboxList(page)).toBeVisible();
  await expect(inboxPane(page)).toHaveCount(0);
  await expect(page.getByTestId(`ask-${scene.plainAskId}`)).toHaveCount(0);
  await expect(page).toHaveURL(INBOX);

  // The row the reader opens is read in place of the list, its card under the view's own Inbox
  // control, which takes the reader back to the list at that row.
  const row = page.locator(`[data-inbox-row="${scene.plainAskId}"]`);
  await row.locator("time").first().click();
  const view = page.getByTestId("inbox-reading-view");
  await expect(view).toBeVisible();
  await expect(inboxList(page)).toBeHidden();
  await expect(
    view.getByTestId(`ask-${scene.plainAskId}`).locator("[data-ask-answer]")
  ).toBeVisible();
  await expect(page).toHaveURL(inboxAt(scene.plainAskId));
  await view.getByRole("button", { exact: true, name: "Back to the Inbox list" }).click();
  await expect(view).toHaveCount(0);
  await expect(row).toBeFocused();
  await expect(page).toHaveURL(INBOX);

  // And so it is on a phone, where the browser's Back leaves the view as the control does.
  await page.setViewportSize({ height: 844, width: 390 });
  await page.goto(INBOX);
  await row.locator("time").first().click();
  await expect(
    view.getByTestId(`ask-${scene.plainAskId}`).locator("[data-ask-answer]")
  ).toBeVisible();
  const widths = await page.evaluate(() => ({
    client: document.documentElement.clientWidth,
    scroll: document.documentElement.scrollWidth,
  }));
  expect(widths.scroll).toBe(widths.client);
  await page.goBack();
  await expect(view).toHaveCount(0);
  await expect(row).toBeFocused();

  // The drawer is a peek over the page it covers, so it keeps the card at every width - on a
  // phone it opens from the compact top bar's Peek - and the approval card's own diff is the
  // change view, with the same marks the column draws.
  await page.goto(`/issues/${scene.decisionIssueKey}`);
  await peekTrigger(page).click();
  const drawer = page.getByRole("dialog", { name: "Inbox" });
  await expect(drawer).toBeVisible();
  await expect(drawer.getByTestId("inbox-pane")).toHaveCount(0);
  const drawerCard = drawer.getByTestId(`ask-${scene.decisionAskId}`);
  await expect(drawerCard.locator("[data-ask-answer]")).toBeVisible();
  await expect(drawerCard.getByRole("link", { name: "Open in document" })).toBeVisible();

  const approvalCard = drawer.getByTestId(`ask-${scene.approvalAskId}`);
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
});

/**
 * How far a press on the short row's checkbox reaches: from the box's centre, the pixels along
 * each axis where `elementFromPoint` finds the box's own label, and whether every point of a
 * grid over the rectangle they span finds it too. A finger lands where the browser hit-tests, so
 * this reads the press square the row draws rather than any box's size.
 */
async function markSquare(page: Page, askId: string) {
  return page
    .locator(`[data-inbox-row="${askId}"] input[type="checkbox"]`)
    .evaluate((box: HTMLInputElement) => {
      // `elementFromPoint` answers only inside the window, and on a phone the row can start
      // below it.
      box.scrollIntoView({ block: "center" });
      const label = box.closest("label");
      const rect = box.getBoundingClientRect();
      const x = Math.round(rect.left + rect.width / 2);
      const y = Math.round(rect.top + rect.height / 2);
      const takes = (atX: number, atY: number) => {
        const hit = document.elementFromPoint(atX, atY);
        return hit !== null && label?.contains(hit) === true;
      };
      if (!takes(x, y)) return { covered: false, height: 0, width: 0 };
      const reach = (dx: number, dy: number) => {
        let step = 0;
        while (step < 60 && takes(x + (step + 1) * dx, y + (step + 1) * dy)) step += 1;
        return step;
      };
      const left = reach(-1, 0);
      const right = reach(1, 0);
      const up = reach(0, -1);
      const down = reach(0, 1);
      let covered = true;
      for (let column = 0; column <= 4; column += 1) {
        for (let line = 0; line <= 4; line += 1) {
          const atX = x - left + ((left + right) * column) / 4;
          const atY = y - up + ((up + down) * line) / 4;
          covered &&= takes(Math.round(atX), Math.round(atY));
        }
      }
      return { covered, height: up + down + 1, width: left + right + 1 };
    });
}

test("a short row's checkbox takes a press anywhere in a 44 px square, beside the column and below it @phone @pr", async ({
  page,
  scene,
}) => {
  // The row opens on a press anywhere but its controls, so a finger aiming at the 16 px box has
  // the 44 px every control gives it, at the column's width and on a phone alike.
  for (const size of [
    { height: 900, width: 1440 },
    { height: 844, width: 390 },
  ]) {
    await page.setViewportSize(size);
    await page.goto(INBOX);
    await expect(page.locator(`[data-inbox-row="${scene.plainAskId}"]`)).toBeVisible();
    const square = await markSquare(page, scene.plainAskId);
    expect(square.width, `the square's width at ${size.width} px`).toBeGreaterThanOrEqual(44);
    expect(square.height, `the square's height at ${size.width} px`).toBeGreaterThanOrEqual(44);
    expect(square.covered, `every point of the square at ${size.width} px`).toBe(true);
  }
});
