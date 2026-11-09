import { expect, test } from "@playwright/test";

import { resetDatabase } from "./seed";
import {
  DECISION_BLOCK,
  expand,
  FOLD_ORDERS,
  foldParts,
  REPLIES,
  ROOT,
  readAskThrough,
  seedThreads,
  setThreadOrder,
  verticalOrder,
} from "./threads-order";
import { asUser } from "./users";

/**
 * One thread order everywhere (INFRA-450), in the two surfaces that sit on the live document: a
 * document block's thread in the spec, and the margin thread beside the passage it is about,
 * with the Conversation card next to it. The Inbox's own surfaces - its reading pane, its card
 * and an ask's own thread - are in `threads-order.stub.e2e.ts`, and the three threads both
 * files are read over are in `threads-order.ts`.
 *
 * Everything here is made up: this repository is public, so nothing names a real person, host or
 * project.
 */

test.beforeEach(async () => {
  await resetDatabase();
});

test("a document block's thread folds and places its box by the chosen order", async ({
  browser,
}) => {
  // Two live documents, each opened with its own room and editor.
  test.slow();
  const seeded = await seedThreads();
  // The block's card marks the thread read as the reader looks at it, so it folds by the
  // reader's unread set rather than by count alone: read through reply 4, leaving 5 and 6.
  await readAskThrough(seeded.blockIssue, seeded.blockAsk, 4);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    for (const [order, expected] of FOLD_ORDERS) {
      await page.goto("/");
      await setThreadOrder(page, order);
      await page.goto(`/issues/${seeded.blockIssue}/spec`);
      // The block holds the thread's section and, beside it, the control that reveals the
      // reply box: a collapsed thread puts the box after its own Write a reply (`AskCard`'s
      // `thread="collapsed"`), so the block is what the order places both in.
      const block = page.locator(`[data-dispatch-ask-block="${DECISION_BLOCK}"]`);
      const parts = foldParts(block, 4);
      await expect(block.getByTestId(`thread-${seeded.blockAsk}`), order).toBeVisible();
      await expect(parts.fold, order).toBeVisible();
      const writeReply = block.getByRole("button", { name: "Write a reply" });
      await expect(writeReply, order).toBeVisible();
      await writeReply.click();
      await expect(parts.box, order).toBeVisible();
      expect(await verticalOrder(parts), order).toEqual(expected);
    }
  } finally {
    await alice.close();
  }
});

test("a margin thread and the Conversation show every reply in the chosen order", async ({
  browser,
}) => {
  test.slow();
  const seeded = await seedThreads();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await setThreadOrder(page, "oldest");

    // A thread card is collapsed until the reader opens it, on the tab and in the margin alike,
    // and an opened one shows every reply: neither surface folds.
    await page.goto(`/issues/${seeded.commentIssue}/conversation`);
    const card = page.locator(`[data-turn="comment:${seeded.comment}"]`);
    await expect(card).toBeVisible();
    await expand(card);
    const replies = card.getByRole("list", { name: "Replies" }).getByRole("listitem");
    await expect(replies).toHaveCount(6);
    expect(await replies.allTextContents()).toEqual(
      REPLIES.map((reply) => expect.stringContaining(reply))
    );

    // The issue's margin draws the same thread, the same way.
    await page.goto(`/issues/${seeded.commentIssue}/spec?comment=${seeded.comment}`);
    const margin = page.locator(`[data-margin-item="${seeded.comment}"]`);
    await expect(margin).toBeVisible();
    await expand(margin);
    const marginReplies = margin.getByRole("list", { name: "Replies" }).getByRole("listitem");
    await expect(marginReplies).toHaveCount(6);
    expect(await marginReplies.allTextContents()).toEqual(
      REPLIES.map((reply) => expect.stringContaining(reply))
    );
    const marginBox = margin.getByRole("textbox", { name: "Reply" });
    expect(
      await verticalOrder({
        box: marginBox,
        last: marginReplies.last(),
        root: margin.getByText(ROOT).first(),
      })
    ).toEqual(["root", "last", "box"]);

    // Newest first turns both round and puts the box under the root.
    await setThreadOrder(page, "newest");
    await page.goto(`/issues/${seeded.commentIssue}/conversation`);
    await expand(card);
    await expect(replies).toHaveCount(6);
    expect(await replies.allTextContents()).toEqual(
      [...REPLIES].reverse().map((reply) => expect.stringContaining(reply))
    );

    await page.goto(`/issues/${seeded.commentIssue}/spec?comment=${seeded.comment}`);
    await expect(margin).toBeVisible();
    await expand(margin);
    await expect(marginReplies).toHaveCount(6);
    expect(await marginReplies.allTextContents()).toEqual(
      [...REPLIES].reverse().map((reply) => expect.stringContaining(reply))
    );
    expect(
      await verticalOrder({
        box: margin.getByRole("textbox", { name: "Reply" }),
        first: marginReplies.first(),
        root: margin.getByText(ROOT).first(),
      })
    ).toEqual(["root", "box", "first"]);
  } finally {
    await alice.close();
  }
});
