/** One thread order everywhere, in the surface the Inbox draws itself over one fixed
 *  answer: an ask's own thread in the reading pane, with the fold and the reply box the chosen
 *  order places.
 *
 *  Every row reads what the page draws over one fixed answer, the order being the reader's own
 *  choice kept in their browser, so each runs against a recording rather than against a harness
 *  (packages/dispatch/README.md, "The stubbed layer"). The rows that read a document block's
 *  thread, a margin thread and the comment thread in the reading pane are in
 *  `threads-order.e2e.ts`: all three sit on the live document, since the comment thread is
 *  anchored to the spec and the pane draws it inside the spec's live room (the in-document pane,
 *  R3).
 */
import { expect } from "@playwright/test";

import { inboxPane } from "./inbox-pane";
import { holdReadMarks, replyRowId } from "./inbox-threads";
import { stubbed } from "./stub/stub-test";
import {
  FOLD_ORDERS,
  foldParts,
  REPLIES,
  readAskThrough,
  seedThreads,
  setThreadOrder,
  verticalOrder,
} from "./threads-order";

/** The ask's own thread, read through its fourth reply. */
const askTest = stubbed(async () => {
  const seeded = await seedThreads();
  // The Inbox's row carries the thread's read mark, and the card folds by it: read through
  // reply 4, so the fold holds those four and replies 5 and 6 stay on screen.
  await readAskThrough(seeded.askIssue, seeded.askId, 4);
  return seeded;
});

/** The same thread read through its second reply, leaving four unread. */
const unreadAskTest = stubbed(async () => {
  const seeded = await seedThreads();
  await readAskThrough(seeded.askIssue, seeded.askId, 2);
  return seeded;
});

askTest(
  "an ask's thread folds and places its box by the chosen order @pr",
  async ({ page, scene }) => {
    for (const [order, expected] of FOLD_ORDERS) {
      await page.goto("/");
      await setThreadOrder(page, order);
      // An answered ask's own thread, as the Inbox's reading pane draws its card.
      const row = page.locator(`[data-inbox-row="${replyRowId(scene.askId)}"]`);
      await expect(row).toBeVisible();
      await row.locator("time").first().click();
      const thread = inboxPane(page).getByTestId(`thread-${scene.askId}`);
      await expect(thread, order).toBeVisible();
      expect(await verticalOrder(foldParts(thread, 4)), order).toEqual(expected);
    }
  }
);

/**
 * An unread reply is never folded away, in an ask's thread as in every other one (spec v23,
 * A10). The Inbox's row carries the thread's read mark, and the pane passes the unread set it
 * names down to the ask's card, so with replies 1 and 2 read the four unread ones are all on
 * screen and the fold holds exactly those two. Folding by count alone put replies 3 and 4
 * behind **Show 4 earlier replies**, and the row's own mark then read them through reply 6 -
 * two writes the reader never saw, marked read.
 *
 * The read marks are held, so the row keeps saying what it has that the reader has not read
 * while the thread is drawn (`holdReadMarks` in `inbox-threads.ts`, which the Inbox's own pane
 * spec holds them with too).
 */
unreadAskTest(
  "an ask's thread leaves every unread reply on screen and folds only the read ones @pr",
  async ({ page, scene }) => {
    // Held before the load rather than after it: a route added to a page mid-run leaves the
    // reads it already made out of the recording, and this row's Inbox list is one of them.
    await holdReadMarks(page);
    await page.goto("/");
    await setThreadOrder(page, "oldest");
    const row = page.locator(`[data-inbox-row="${replyRowId(scene.askId)}"]`);
    await expect(row).toBeVisible();
    await row.locator("time").first().click();
    const thread = inboxPane(page).getByTestId(`thread-${scene.askId}`);
    await expect(thread).toBeVisible();
    // The fold holds the two read replies and nothing else.
    await expect(thread.getByRole("button", { name: "Show 2 earlier replies" })).toBeVisible();
    for (const place of [3, 4, 5, 6]) {
      await expect(thread.getByText(REPLIES[place - 1] ?? ""), `reply ${place}`).toBeVisible();
    }
    for (const place of [1, 2]) {
      await expect(thread.getByText(REPLIES[place - 1] ?? ""), `reply ${place}`).toHaveCount(0);
    }
  }
);
