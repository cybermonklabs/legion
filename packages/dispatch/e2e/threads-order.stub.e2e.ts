/** One thread order everywhere (INFRA-450), in the surfaces the Inbox draws itself: the reading
 *  pane, its card below the pane's width, and an ask's own thread, each with the fold and the
 *  reply box the chosen order places.
 *
 *  Every row reads what the page draws over one fixed answer, the order being the reader's own
 *  choice kept in their browser, so each runs against a recording rather than against a harness
 *  (packages/dispatch/AGENTS.md, "The stubbed layer"). The two rows that read a document block's
 *  thread and a margin thread stayed in `threads-order.e2e.ts`: both sit on the live document.
 */
import { expect } from "@playwright/test";

import { listMyReplies } from "./api";
import { inboxPane } from "./inbox-pane";
import { holdReadMarks, replyRowId } from "./inbox-threads";
import { stubbed } from "./stub/stub-test";
import {
  collapsedParts,
  FOLD_ORDERS,
  foldParts,
  openInPane,
  REPLIES,
  readAskThrough,
  readThrough,
  seedThreads,
  setThreadOrder,
  verticalOrder,
} from "./threads-order";

/** The comment thread read through its fourth reply, so the fold holds four. */
const test = stubbed(async () => {
  const seeded = await seedThreads();
  await readThrough(seeded.commentIssue, seeded.comment, 4);
  return seeded;
});

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

/** The comment thread read through its second reply, which the server counts as four unread. */
const unreadCommentTest = stubbed(async () => {
  // The count is the server's, so it is read where the seeding runs: in record mode alone.
  const seeded = await seedThreads();
  await readThrough(seeded.commentIssue, seeded.comment, 2);
  expect(
    ((await listMyReplies({ state: "unread" })).rows ?? []).find(
      (row) => row.thread_id === seeded.comment
    )?.unread_count
  ).toBe(4);
  return seeded;
});

test("the Inbox's pane and its card read a thread in the chosen order, and keep it over a reload @pr", async ({
  page,
  scene,
}) => {
  await page.goto("/");
  const id = replyRowId(scene.comment);

  // Oldest first: the root, the fold over the four read replies, 5, 6, then the reply box.
  await setThreadOrder(page, "oldest");
  let body = await openInPane(page, id);
  const box = body.getByRole("textbox", { name: "Reply" });
  expect(await verticalOrder(collapsedParts(body, 4, box))).toEqual([
    "root",
    "fold",
    "reply-5",
    "reply-6",
    "box",
  ]);
  // The pane opens where the reader left off: the first unread reply is on screen.
  expect(
    await inboxPane(page).evaluate((pane, text) => {
      let scroller = pane;
      for (const candidate of pane.querySelectorAll<HTMLElement>("*")) {
        const overflow = window.getComputedStyle(candidate).overflowY;
        if (overflow === "auto" || overflow === "scroll") {
          scroller = candidate;
          break;
        }
      }
      const fifth = [...pane.querySelectorAll<HTMLElement>("[data-inbox-thread-turn]")].find(
        (turn) => (turn.textContent ?? "").includes(text)
      );
      if (fifth === undefined) return false;
      const box = scroller.getBoundingClientRect();
      const rect = fifth.getBoundingClientRect();
      return rect.bottom > box.top && rect.top < box.bottom;
    }, REPLIES[4] ?? "")
  ).toBe(true);

  // Newest first: the root, the box, 6, 5, then the fold.
  await setThreadOrder(page, "newest");
  body = await openInPane(page, id);
  expect(
    await verticalOrder(collapsedParts(body, 4, body.getByRole("textbox", { name: "Reply" })))
  ).toEqual(["root", "box", "reply-6", "reply-5", "fold"]);

  // The choice is the reader's, kept for their browser across a reload.
  await page.reload();
  body = await openInPane(page, id);
  expect(
    await verticalOrder(collapsedParts(body, 4, body.getByRole("textbox", { name: "Reply" })))
  ).toEqual(["root", "box", "reply-6", "reply-5", "fold"]);

  // Below the pane's width the row is its own card, and it reads the same way.
  await page.setViewportSize({ height: 900, width: 1024 });
  await page.goto("/");
  const card = page.locator(`[data-inbox-row="${id}"] [data-inbox-thread-body]`);
  await expect(card).toBeVisible();
  expect(
    await verticalOrder(collapsedParts(card, 4, card.getByRole("textbox", { name: "Reply" })))
  ).toEqual(["root", "box", "reply-6", "reply-5", "fold"]);
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

unreadCommentTest(
  "an unread reply is never folded: with 3 to 6 unread the fold holds only 1 and 2 @pr",
  async ({ page, scene }) => {
    await page.goto("/");
    await setThreadOrder(page, "oldest");
    const body = await openInPane(page, replyRowId(scene.comment));
    await expect(body.getByRole("button", { name: "Show 2 earlier replies" })).toBeVisible();
    await expect(body.locator("[data-inbox-thread-turn]")).toHaveCount(5);
    for (const reply of REPLIES.slice(2)) {
      await expect(body.getByText(reply)).toBeVisible();
    }
    await expect(body.getByText(REPLIES[1] ?? "")).toHaveCount(0);
  }
);
