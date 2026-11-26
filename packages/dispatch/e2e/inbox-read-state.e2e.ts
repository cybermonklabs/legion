import { setLiveSessions } from "./agents";
import { createIssue, createProject, listMyReplies } from "./api";
import { selectedRow, selectRow } from "./inbox-pane";
import { replyRowId, rowThreadSession, seedCommentThread, unreadThreadRows } from "./inbox-threads";
import { hasReadingPane } from "./reading-pane";
import { expect, rowKey, rowPerson, rowSessionId, test } from "./test";
import { asUser } from "./users";

/** The session that answers the seeded threads, under the running row's own id
 *  (`rowThreadSession`, e2e/inbox-threads.ts): Dispatch keeps what is sent to it and its replies
 *  by its id, for every person at once. */
const threadSession = rowThreadSession(rowSessionId);

/**
 * Reading a thread in the Inbox marks it read (INFRA-450, A7): "a thread is marked when its
 * newest message has been on screen, in the pane or on a card ... three quarters of the message
 * visible for one second, or, for a message taller than the screen, the message filling three
 * quarters of the screen for one second". The row stays where it is, drawn as read: a thread
 * leaves the Inbox only when the reader dismisses it (`e2e/inbox-dismiss.e2e.ts`).
 *
 * The reading pane is an inner scroller about 200 px shorter than the window, so a reply taller
 * than the pane can never cover three quarters of the *window*: at the suite's own viewport
 * (`Desktop Chrome`, 1280x720) the pane is 519 px tall and the rule asks for 540 px. A reader who
 * scrolls such a reply through the pane for as long as they like never clears its row, which is
 * the complaint this issue exists to answer.
 *
 * Everything here is made up: this repository is public, so nothing names a real person, host or
 * project.
 */

/** A reply far taller than the reading pane at any window height the suite runs at. */
const LONG_REPLY = Array.from(
  { length: 40 },
  (_, index) => `Line ${index + 1} of the replay answer.`
).join("\n\n");

// @pr: The read mark's own row: reading a reply taller than the pane marks it read and leaves its row in the list.
test("a reply taller than the reading pane is marked read by reading it, and its row stays @pr", async ({
  browser,
}) => {
  const core = rowKey("CORE");
  // The reader is a person of the row's own (`rowPerson`, e2e/test.ts), whose threads are this
  // row's two and no other row's.
  const me = rowPerson("alice");
  await setLiveSessions([threadSession]);
  await createProject({ key: core, name: "Core" }, { login: me });
  const issue = await createIssue({ project: core, title: "Replay budget" }, { login: me });
  const thread = await seedCommentThread(
    issue.key,
    "Why does it give up?",
    LONG_REPLY,
    threadSession,
    me
  );
  // A second thread, so the selection has somewhere to move once the long one has been read.
  const other = await seedCommentThread(
    issue.key,
    "And how far back does the replay look?",
    "One hour, then it gives up.",
    threadSession,
    me
  );
  expect(await unreadThreadRows(me)).toBe(2);
  // Whether the long thread itself is still unread. The other row is read as soon as the column
  // draws it, so the count of every unread thread would not say which of the two this is about.
  const longUnread = async (): Promise<boolean> =>
    ((await listMyReplies({ state: "unread" }, { login: me })).rows ?? []).some(
      (row) => row.thread_id === thread
    );
  expect(await longUnread()).toBe(true);

  const context = await asUser(browser, me);
  try {
    const page = await context.newPage();
    await page.goto("/");
    const row = page.locator(`[data-inbox-row="${replyRowId(thread)}"]`);
    await expect(row).toBeVisible();
    test.skip(!hasReadingPane(page), "the reading pane is this row's subject");
    await row.locator("time").first().click();
    await expect(row).toHaveAttribute("data-inbox-selected", "true");
    const reply = page.locator("[data-inbox-thread-turn]").last();
    await expect(reply).toBeVisible();

    // The reader scrolls the reply through the pane, a part of a screen at a time, pausing on
    // each - the dwell the read mark asks for, given ten times over. The poll is both halves of
    // that: each attempt moves the wheel once, and the 1,200 ms between attempts is the pause
    // the mark asks a reader for, so the row ends on the mark rather than on a count of turns.
    const pane = await reply.boundingBox();
    expect(pane).not.toBeNull();
    await expect
      .poll(
        async () => {
          await page.mouse.move((pane?.x ?? 0) + 10, 400);
          await page.mouse.wheel(0, 300);
          return await longUnread();
        },
        { intervals: [1200], timeout: 12_000 }
      )
      .toBe(false);
    await expect(row.locator("[aria-label='Unread']")).toHaveCount(0);

    // The row stays where it stood, drawn as read: only a dismiss takes a thread out of the
    // Inbox. The selection moves to the other row, and a reload reads both rows back.
    await selectRow(page, replyRowId(other));
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", replyRowId(other));
    await expect(row).toBeVisible();
    await expect(row.locator("[aria-label='Unread']")).toHaveCount(0);
    await page.reload();
    await expect(page.locator("[data-inbox-row]")).toHaveCount(2);
    await expect(row).toBeVisible();
    await expect(row.locator("[aria-label='Unread']")).toHaveCount(0);
  } finally {
    await context.close();
  }
});
