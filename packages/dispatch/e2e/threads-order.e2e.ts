import { openAgents } from "./agents";
import { meInboxThreads } from "./api";
import { drawer } from "./inbox-drawer";
import { inboxPane } from "./inbox-pane";
import { replyRowId } from "./inbox-threads";
import { expect, test } from "./test";
import {
  collapsedParts,
  DECISION_BLOCK,
  expand,
  FOLD_ORDERS,
  foldParts,
  openInPane,
  REPLIES,
  ROOT,
  readAskThrough,
  readThrough,
  seedThreads,
  setThreadOrder,
  verticalOrder,
} from "./threads-order";
import { asUser } from "./users";

/**
 * One thread order everywhere, in the surfaces that sit on the live document: a
 * document block's thread in the spec, the margin thread beside the passage it is about, with
 * the Conversation card next to it, and the Inbox's reading pane over the comment thread, with
 * its reading view below the pane's width and the drawer's card. That thread is anchored to the
 * spec, so the pane draws it inside the spec's live room, under its block (the in-document pane,
 * R3). An ask's own thread in the Inbox is in `threads-order.stub.e2e.ts`, and the three threads
 * both files are read over are in `threads-order.ts`.
 *
 * Everything here is made up: this repository is public, so nothing names a real person, host or
 * project.
 */

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

test("the Inbox's pane, its reading view and the drawer's card read a thread in the chosen order, and keep it over a reload", async ({
  browser,
}) => {
  // The comment thread read through its fourth reply, so the fold holds four.
  const scene = await seedThreads();
  await readThrough(scene.commentIssue, scene.comment, 4);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
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
    // The fifth reply, the first unread, is on screen. The pane draws this thread inside the
    // spec and opens at the thread's section, not at its first unread reply; the spec is one
    // heading and one sentence, so the whole thread is in view.
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

    // Below the pane's width the row opens in place of the list, in the same pane, and the drawer's
    // row is its own card; both read the same way.
    await page.setViewportSize({ height: 900, width: 1024 });
    await page.goto("/");
    body = await openInPane(page, id);
    await expect(page.getByTestId("inbox-reading-view")).toBeVisible();
    expect(
      await verticalOrder(collapsedParts(body, 4, body.getByRole("textbox", { name: "Reply" })))
    ).toEqual(["root", "box", "reply-6", "reply-5", "fold"]);
    // The drawer's key is bound once sign-in answers, which `openAgents` waits for.
    await openAgents(page);
    await page.keyboard.press("i");
    const card = drawer(page).locator(`[data-inbox-row="${id}"] [data-inbox-thread-body]`);
    await expect(card).toBeVisible();
    expect(
      await verticalOrder(collapsedParts(card, 4, card.getByRole("textbox", { name: "Reply" })))
    ).toEqual(["root", "box", "reply-6", "reply-5", "fold"]);
  } finally {
    await alice.close();
  }
});

test("an unread reply is never folded: with 3 to 6 unread the fold holds only 1 and 2", async ({
  browser,
}) => {
  // The comment thread read through its second reply, which the server counts as four unread.
  const scene = await seedThreads();
  await readThrough(scene.commentIssue, scene.comment, 2);
  expect(
    (await meInboxThreads({ state: "unread" })).rows.find((row) => row.thread_id === scene.comment)
      ?.unread_count
  ).toBe(4);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await setThreadOrder(page, "oldest");
    const body = await openInPane(page, replyRowId(scene.comment));
    await expect(body.getByRole("button", { name: "Show 2 earlier replies" })).toBeVisible();
    await expect(body.locator("[data-inbox-thread-turn]")).toHaveCount(5);
    for (const reply of REPLIES.slice(2)) {
      await expect(body.getByText(reply)).toBeVisible();
    }
    await expect(body.getByText(REPLIES[1] ?? "")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});
